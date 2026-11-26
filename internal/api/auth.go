package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// principal is who the request acts for. UserID is public.users.id (what every store call takes and the only thing that does); AuthUserID is the login it was resolved from, kept for the log.
type principal struct {
	UserID     string
	AuthUserID string
}

type principalKey struct{}

// principalFrom is the principal the middleware put in the context. A handler is only ever reached through the middleware, so a missing one is a wiring bug and a 500, never a request acted
// on for nobody.
func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok && p.UserID != ""
}

// The log words of a refused login: one word each, chosen here from the TYPE of the failure. Never the error's text (the verifier's are fixed sentences, but a log line is the place an
// attacker's choice of text would land, so the API does not rely on that), never the token or a part of it.
const (
	whyNoHeader       = "no_authorization_header"
	whyManyHeaders    = "several_authorization_headers"
	whyNotBearer      = "scheme_is_not_bearer"
	whyEmptyToken     = "empty_token"
	whyTokenTooLong   = "token_too_long"
	whyMalformed      = "token_malformed"
	whyAlgorithm      = "algorithm_not_accepted"
	whySignature      = "signature_does_not_verify"
	whyNoKey          = "no_key_for_the_token"
	whyExpired        = "token_expired"
	whyNotYetValid    = "token_not_yet_valid"
	whyIssuer         = "issuer_not_accepted"
	whyAudience       = "audience_not_accepted"
	whySubject        = "subject_not_valid"
	whyVerifyOther    = "token_refused"
	whyUnknownLogin   = "no_platform_user_for_the_login"
	whyInactive       = "platform_user_inactive"
	whySystemUser     = "login_is_the_system_user"
	whyBadLoginID     = "login_id_not_a_uuid"
	whyResolveOther   = "login_refused"
	whyWrongTenantRow = "resolver_answered_another_user"

	// whyKeysUnavailable is the log word of the one failure that is not a refusal of the login: the verifier could not obtain the keys it needs (the 503 `auth_unavailable`).
	whyKeysUnavailable = "signing_keys_unavailable"

	// msgKeysUnavailable is the message of the one warn line a stretch of unavailability writes.
	msgKeysUnavailable = "authentication is unavailable: the signing keys cannot be obtained"
)

// authRetryAfter is the `Retry-After` of the 503 `auth_unavailable`, in whole seconds: the verifier fetches its key set at most this often (auth.DefaultMinRefreshInterval), so a retry
// sooner than this meets the same verdict for the tokens that need a fetch, and one after it is the first that can meet a recovered issuer.
var authRetryAfter = strconv.Itoa(int(auth.DefaultMinRefreshInterval / time.Second))

// keysClassWords are the words the log may carry for why the keys could not be had: this package's own list, chosen from the verifier's class (a closed set of constants), so that nothing
// but one of these ever reaches a line whatever a Verifier says.
var keysClassWords = map[string]string{
	auth.ClassUnreachable: "unreachable", auth.ClassTimeout: "timeout", auth.ClassCertificate: "certificate",
	auth.ClassStatus: "bad_status", auth.ClassUnreadable: "unreadable", auth.ClassBadKeySet: "bad_key_set",
}

// verifyWhy names a verifier failure by its sentinel.
func verifyWhy(err error) string {
	for _, c := range []struct {
		is  error
		why string
	}{
		{auth.ErrMalformed, whyMalformed}, {auth.ErrAlgorithm, whyAlgorithm}, {auth.ErrSignature, whySignature}, {auth.ErrKey, whyNoKey},
		{auth.ErrExpired, whyExpired}, {auth.ErrNotYetValid, whyNotYetValid}, {auth.ErrIssuer, whyIssuer}, {auth.ErrAudience, whyAudience}, {auth.ErrSubject, whySubject},
	} {
		if errors.Is(err, c.is) {
			return c.why
		}
	}
	return whyVerifyOther
}

// resolveWhy names a resolver refusal by its sentinel; ok is false for anything else (a failure of the database, which is not a refusal of the login).
func resolveWhy(err error) (why string, refusal bool) {
	for _, c := range []struct {
		is  error
		why string
	}{
		{tenant.ErrUnknownLogin, whyUnknownLogin}, {tenant.ErrInactive, whyInactive}, {tenant.ErrSystemUser, whySystemUser}, {tenant.ErrBadID, whyBadLoginID},
	} {
		if errors.Is(err, c.is) {
			return c.why, true
		}
	}
	return "", false
}

// bearerToken extracts the token of the request's one Authorization header: `Bearer <token>`, the scheme in any letter case, at most auth.MaxTokenBytes of token. The second result is the
// log word of the failure ("" when there is none). Whether the token is a good one is not decided here.
func bearerToken(r *http.Request) (string, string) {
	values := r.Header.Values("Authorization")
	switch len(values) {
	case 0:
		return "", whyNoHeader
	case 1:
	default:
		return "", whyManyHeaders
	}
	scheme, rest, _ := strings.Cut(values[0], " ") // the server trims a header value, so "Bearer " arrives as "Bearer": a scheme with no token
	if !strings.EqualFold(scheme, "Bearer") {
		return "", whyNotBearer
	}
	token := strings.TrimLeft(rest, " ")
	switch {
	case token == "":
		return "", whyEmptyToken
	case len(token) > auth.MaxTokenBytes:
		return "", whyTokenTooLong
	}
	return token, ""
}

// authenticated wraps a handler in the authentication of Docs/jobs-api.md: the header, the verifier, the resolver, and only then the handler, with the principal in the context.
// Every refusal of the LOGIN is the same 401, byte for byte but for the request id. A failure of the platform's own database while resolving a login is not a refusal of the login (the
// token verified): it is a 500 `internal_error`, so that a client does not sign a user out for an outage. The same goes for the verifier's keys: when it could not obtain them (the
// issuer's key set cannot be fetched) the token is neither accepted nor refused, and the answer is the 503 `auth_unavailable` with a `Retry-After`, never a 401 that tells a user their
// sign-in is wrong because the platform could not check it. The verifier decides which it is (auth.ErrKeysUnavailable, or any other error); a token that is bad without any key
// (malformed, "alg":"none", an algorithm it does not take, expired, for another audience) is the 401 whether or not the keys are up, and an HS256 token is judged by its secret alone.
func (a *API) authenticated(next func(w http.ResponseWriter, r *http.Request, p principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token, why := bearerToken(r)
		if why != "" {
			a.refuse(w, r, why)
			return
		}
		claims, err := a.verifier.Verify(r.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrKeysUnavailable) {
				a.keysUnavailable(w, r, err)
				return
			}
			a.refuse(w, r, verifyWhy(err))
			return
		}
		t, err := a.resolver.ByAuthUser(r.Context(), claims.Subject)
		if err != nil {
			if why, refusal := resolveWhy(err); refusal {
				a.refuse(w, r, why)
				return
			}
			a.fail(w, r, err) // the database could not be asked: not the login's fault
			return
		}
		if t.UserID == "" || t.UserID == tenant.SystemUserID || !t.Active {
			a.refuse(w, r, whyWrongTenantRow)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{UserID: t.UserID, AuthUserID: t.AuthUserID})), principal{UserID: t.UserID, AuthUserID: t.AuthUserID})
	})
}

// keysUnavailable answers a request whose token could not be judged because the verifier could not obtain its keys: 503 `auth_unavailable` with `Retry-After` (through the one mapping,
// classify, which is what also gives the code its fixed message and no detail), and NOT `WWW-Authenticate`: nothing was refused, so there is nothing to challenge.
//
// It logs, at warn, ONCE PER STRETCH of unavailability and not once per request: the first error of a stretch (auth.KeysUnavailableError.FirstOfStretch: a stretch opens with the first
// failed fetch of the key set after a good one and closes with the next good one) is the one request that writes the line, whichever goroutine gets there first, so a long outage under load
// is one line and a recovery followed by a second outage is a second. The line is the word and the class of the failure from this file's closed list, under the request id; never the
// key set's address, a key id, a status text or the error's text. A Verifier that is not the auth package's and returns the bare sentinel has no stretch: every such error is first, a
// line repeated being better than a line lost.
func (a *API) keysUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	var down *auth.KeysUnavailableError
	if !errors.As(err, &down) || down.FirstOfStretch() {
		class := "unknown"
		if down != nil {
			if word, ok := keysClassWords[down.Class()]; ok {
				class = word
			}
		}
		a.log.WarnContext(r.Context(), msgKeysUnavailable, "reason", whyKeysUnavailable, "class", class)
	}
	a.fail(w, r, err)
}

// refuse answers a refused login: 401, `unauthenticated`, the fixed message, `WWW-Authenticate: Bearer`, and one log line under the request id naming the reason as a word.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, why string) {
	a.log.InfoContext(r.Context(), "authentication refused", "reason", why, "method", r.Method, "path", r.URL.Path)
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpapi.WriteError(w, r, http.StatusUnauthorized, codeUnauthenticated, messages[codeUnauthenticated])
}
