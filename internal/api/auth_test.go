package api

import (
	"context"
	"errors"
	"github.com/coldmail-platform/platform/internal/jobs"
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The authentication of every route (auth.go), against the REAL verifier, with tokens authtest mints from keys made at run time. Each attack is paired with its LEGAL TWIN: the same request
// with a token that is good, which is answered 200: so a refusal is never the rig refusing everything.

// attack is one way to be refused and the way to be let in that differs from it in exactly the thing under test.
type attack struct {
	name  string
	hurt  func(r *rig, m *authtest.Minter) req // the request that must be refused
	twin  func(r *rig, m *authtest.Minter) req // the request that differs only in that thing and must be served
	setup func(r *rig)                         // arranges the resolver, when the attack is about the login
	why   string                               // the word the log must carry
}

func list(token string, rawAuth *string, hdr http.Header) req {
	return req{method: http.MethodGet, path: "/v1/jobs", token: token, rawAuth: rawAuth, header: hdr}
}

func s(v string) *string { return &v }

func attacks() []attack {
	good := func(m *authtest.Minter, c authtest.Claims) string { return m.Mint(c) }
	legal := func(r *rig, m *authtest.Minter) req { return list(m.Token(loginA), nil, nil) }
	return []attack{
		{"no Authorization header", func(r *rig, m *authtest.Minter) req { return list("", nil, nil) }, legal, nil, whyNoHeader},
		{"two Authorization headers", func(r *rig, m *authtest.Minter) req {
			return req{method: http.MethodGet, path: "/v1/jobs", header: http.Header{"Authorization": {"Bearer " + m.Token(loginA), "Bearer " + m.Token(loginA)}}}
		}, legal, nil, whyManyHeaders},
		{"another scheme", func(r *rig, m *authtest.Minter) req { return list("", s("Basic "+m.Token(loginA)), nil) }, legal, nil, whyNotBearer},
		{"the scheme with no token", func(r *rig, m *authtest.Minter) req { return list("", s("Bearer "), nil) }, legal, nil, whyEmptyToken},
		{"a token that is not a token", func(r *rig, m *authtest.Minter) req { return list("not-a-jwt", nil, nil) }, legal, nil, whyMalformed},
		{"a token with two dots too many", func(r *rig, m *authtest.Minter) req { return list(m.Token(loginA)+".x.y", nil, nil) }, legal, nil, whyMalformed},
		{"an expired token", func(r *rig, m *authtest.Minter) req {
			return list(good(m, m.Good(loginA).With("exp", t0.Unix()-3600)), nil, nil)
		}, legal, nil, whyExpired},
		{"a token that is not valid yet", func(r *rig, m *authtest.Minter) req {
			return list(good(m, m.Good(loginA).With("nbf", t0.Unix()+3600)), nil, nil)
		}, legal, nil, whyNotYetValid},
		{"a forged signature", func(r *rig, m *authtest.Minter) req { return list(m.Forged(m.Good(loginA)), nil, nil) }, legal, nil, whySignature},
		{"a tampered payload", func(r *rig, m *authtest.Minter) req {
			return list(m.Tampered(m.Token(loginB), m.Good(loginA)), nil, nil)
		}, legal, nil, whySignature},
		{"the wrong audience", func(r *rig, m *authtest.Minter) req {
			return list(good(m, m.Good(loginA).With("aud", "someone-else")), nil, nil)
		}, legal, nil, whyAudience},
		{"the wrong issuer", func(r *rig, m *authtest.Minter) req {
			return list(good(m, m.Good(loginA).With("iss", "https://evil.example.test/auth/v1")), nil, nil)
		}, legal, nil, whyIssuer},
		{"alg none", func(r *rig, m *authtest.Minter) req { return list(m.Unsigned(m.Good(loginA)), nil, nil) }, legal, nil, whyAlgorithm},
		{"a subject that is not a uuid", func(r *rig, m *authtest.Minter) req {
			return list(good(m, m.Good(loginA).With("sub", "alice")), nil, nil)
		}, legal, nil, whySubject},
		{"a token for a login nobody made a user of", func(r *rig, m *authtest.Minter) req {
			return list(m.Token("c3c3c3c3-0000-4000-8000-0000000000c3"), nil, nil)
		}, legal, nil, whyUnknownLogin},
		{"an inactive user", func(r *rig, m *authtest.Minter) req { return list(m.Token(loginB), nil, nil) },
			func(r *rig, m *authtest.Minter) req { return list(m.Token(loginA), nil, nil) },
			func(r *rig) { r.res.errs[loginB] = tenant.ErrInactive }, whyInactive},
		{"the system user", func(r *rig, m *authtest.Minter) req { return list(m.Token(loginB), nil, nil) }, legal,
			func(r *rig) { r.res.errs[loginB] = tenant.ErrSystemUser }, whySystemUser},
		{"a resolver that answers a switched-off user without an error", func(r *rig, m *authtest.Minter) req { return list(m.Token(loginB), nil, nil) }, legal,
			func(r *rig) { r.res.users[loginB] = tenant.Tenant{UserID: userB, AuthUserID: loginB, Active: false} }, whyWrongTenantRow},
	}
}

func TestAuth_EveryRefusalIsTheSame401WithALegalTwinThatIsServed(t *testing.T) {
	for _, alg := range []struct {
		name string
		opts []rigOpt
	}{{"HS256", nil}, {"ES256", []rigOpt{withES256()}}} {
		t.Run(alg.name, func(t *testing.T) {
			var reference string
			for _, a := range attacks() {
				r := newRig(t, alg.opts...)
				if a.setup != nil {
					a.setup(r)
				}
				resp, body := r.do(a.hurt(r, r.mint))
				r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
				if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("%s: WWW-Authenticate = %q, want Bearer", a.name, got)
				}
				// the SAME body for every reason: not a word of it says why
				if reference == "" {
					reference = sameBody(body)
				} else if got := sameBody(body); got != reference {
					t.Errorf("%s: the 401 body differs from the others:\n  %s\n  %s", a.name, got, reference)
				}
				if n := len(r.store.seen()); n != 0 {
					t.Errorf("%s: the store was called (%d) for a refused login", a.name, n)
				}
				// its legal twin, on the same arrangement, is served
				resp, body = r.do(a.twin(r, r.mint))
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s: its legal twin was answered %d: %s", a.name, resp.StatusCode, body)
				}
			}
			if reference == "" {
				t.Fatal("no refusal was examined")
			}
		})
	}
}

func TestAuth_TheReasonIsInTheLogUnderTheRequestIDAndNeverTheToken(t *testing.T) {
	seenWhy := map[string]bool{}
	for _, a := range attacks() {
		r := newRig(t)
		if a.setup != nil {
			a.setup(r)
		}
		q := a.hurt(r, r.mint)
		resp, body := r.do(q)
		rid := resp.Header.Get("X-Request-Id")
		var line map[string]any
		for _, l := range r.logs.lines(t) {
			if l["msg"] == "authentication refused" {
				line = l
			}
		}
		if line == nil || line["request_id"] != rid || line["reason"] != a.why {
			t.Errorf("%s: the log line is %v, want reason %q under request id %s", a.name, line, a.why, rid)
		}
		seenWhy[a.why] = true
		// no token, and no part of one, in the log or in the answer
		tok := q.token
		if q.rawAuth != nil {
			tok = *q.rawAuth
		}
		if len(q.header["Authorization"]) > 0 {
			tok = strings.Join(q.header["Authorization"], " ")
		}
		for _, text := range []string{r.logs.String(), string(body)} {
			for _, seg := range strings.FieldsFunc(tok, func(c rune) bool { return c == '.' || c == ' ' }) {
				for i := 0; i+12 <= len(seg); i += 6 {
					if strings.Contains(text, seg[i:i+12]) {
						t.Errorf("%s: a piece of the token (%q) is in the log or the answer", a.name, seg[i:i+12])
						break
					}
				}
			}
		}
	}
	// the control: the reasons are told apart (a log that said "refused" for all of them would pass the loop above)
	if len(seenWhy) < 12 {
		t.Fatalf("only %d distinct reasons were told apart in the log", len(seenWhy))
	}
}

func TestAuth_TheSchemeIsCaseInsensitiveAndOnlyBearer(t *testing.T) {
	r := newRig(t)
	tok := r.token(loginA)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		resp, body := r.do(req{method: http.MethodGet, path: "/v1/jobs", rawAuth: s(scheme + " " + tok)})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("scheme %q: %d %s", scheme, resp.StatusCode, body)
		}
	}
	// several spaces between the scheme and the token are the grammar's 1*SP
	if resp, _ := r.do(req{method: http.MethodGet, path: "/v1/jobs", rawAuth: s("Bearer   " + tok)}); resp.StatusCode != http.StatusOK {
		t.Errorf("Bearer with three spaces: %d", resp.StatusCode)
	}
	// the controls: a scheme that merely starts like it, and a token of the right shape in a header of another name
	for _, bad := range []string{"Bearerx " + tok, "Token " + tok, tok, "Bearer\t" + tok, "Bearer " + tok + " extra"} {
		resp, body := r.do(req{method: http.MethodGet, path: "/v1/jobs", rawAuth: s(bad)})
		r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
	}
	resp, body := r.do(req{method: http.MethodGet, path: "/v1/jobs", header: http.Header{"X-Authorization": {"Bearer " + tok}, "Cookie": {"access_token=" + tok}}})
	r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = r.do(req{method: http.MethodGet, path: "/v1/jobs?access_token=" + tok})
	r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
}

func TestAuth_TheTokenIsCappedAt8KiB(t *testing.T) {
	// the parser: exactly the cap is read, one more is refused before the verifier is asked
	exactly := strings.Repeat("a", auth.MaxTokenBytes)
	hr := func(v string) *http.Request {
		q, _ := http.NewRequest(http.MethodGet, "/", nil)
		q.Header.Set("Authorization", v)
		return q
	}
	if tok, why := bearerToken(hr("Bearer " + exactly)); why != "" || tok != exactly {
		t.Fatalf("a token of exactly %d bytes: %d %q", auth.MaxTokenBytes, len(tok), why)
	}
	if _, why := bearerToken(hr("Bearer " + exactly + "a")); why != whyTokenTooLong {
		t.Fatalf("a token one byte over: %q, want %q", why, whyTokenTooLong)
	}
	if auth.MaxTokenBytes != 8<<10 {
		t.Fatalf("the verifier's cap is %d, the API documents 8 KiB", auth.MaxTokenBytes)
	}

	// end to end: a legal token as long as the verifier reads is served, one over the cap is the same 401 as every other refusal
	r := newRig(t)
	near, over := "", ""
	for pad := 5700; pad < 6200; pad++ {
		tok := r.mint.Mint(r.mint.Good(loginA).With("pad", strings.Repeat("p", pad)))
		switch {
		case len(tok) <= auth.MaxTokenBytes:
			near = tok
		case over == "":
			over = tok
		}
	}
	if len(near) < auth.MaxTokenBytes-4 || len(over) <= auth.MaxTokenBytes {
		t.Fatalf("could not build tokens either side of the cap: %d and %d bytes", len(near), len(over))
	}
	if resp, body := r.do(req{method: http.MethodGet, path: "/v1/jobs", token: near}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a legal token of %d bytes: %d %s", len(near), resp.StatusCode, body)
	}
	resp, body := r.do(req{method: http.MethodGet, path: "/v1/jobs", token: over})
	r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
}

func TestAuth_ADatabaseFailureWhileResolvingIsNotARefusalOfTheLogin(t *testing.T) {
	r := newRig(t)
	r.res.any = errors.New("tenant: read the platform user of login: connection refused")
	resp, body := r.get(loginA, "/v1/jobs")
	e := r.wantError(resp, body, http.StatusInternalServerError, "internal_error")
	if e.Details != nil || strings.Contains(string(body), "connection refused") || resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("the outage was reported to the client: %s", body)
	}
	var logged bool
	for _, l := range r.logs.lines(t) {
		if l["msg"] == "request failed" && l["request_id"] == resp.Header.Get("X-Request-Id") && strings.Contains(l["error"].(string), "connection refused") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("the cause was not logged under the request id: %s", r.logs.String())
	}
	if len(r.store.seen()) != 0 {
		t.Fatal("the store was called although the login could not be resolved")
	}
	// the control: a refusal the resolver names is the 401, not a 500
	r.res.any = tenant.ErrUnknownLogin
	resp, body = r.get(loginA, "/v1/jobs")
	r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
}

func TestAuth_ThePrincipalIsInTheContextAndIsThePlatformUserNotTheLogin(t *testing.T) {
	r := newRig(t)
	// the login's platform user is a DIFFERENT uuid: a handler that took the login id (the token's subject) for the store would be found by it
	if loginA == userA {
		t.Fatal("the rig's login and user must differ for this test to mean anything")
	}
	if resp, body := r.get(loginA, "/v1/jobs"); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	calls := r.store.seen()
	if len(calls) != 1 || calls[0].User != userA {
		t.Fatalf("the store was called %+v, want one call for user %s", calls, userA)
	}
	// a handler reached without the middleware has no principal and is not served
	if _, ok := principalFrom(context.Background()); ok {
		t.Fatal("an empty context has a principal")
	}
	if p, ok := principalFrom(context.WithValue(context.Background(), principalKey{}, principal{UserID: userA, AuthUserID: loginA})); !ok || p.UserID != userA || p.AuthUserID != loginA {
		t.Fatalf("principalFrom = %+v, %v", p, ok)
	}
}

// The user id of every store call is the principal's, whatever the request says: a login that is user B acts as user B and never as A, on every route, with hostile headers, cookies, query
// parameters and bodies. A request that NAMES a user in a body or a query is refused as an unknown member and reaches nothing.
func TestAuth_NothingInTheRequestNamesTheUserOfAStoreCall(t *testing.T) {
	r := newRig(t)
	r.store.results = []jobs.Result{{Ordinal: 1, Email: "a@example.test", Origin: "module", State: "valid", Decision: "free", Rule: "P-01"}}
	hostile := http.Header{"X-User-Id": {userA}, "X-Forwarded-User": {userA}, "X-Auth-User": {loginA}, "Cookie": {"user_id=" + userA}}
	good := map[string][]byte{
		"POST /v1/jobs":              []byte(`{"module":"m1","rows":[{"email":"a@example.test"}]}`),
		"POST /v1/jobs/{id}/confirm": []byte(`{"fingerprint":"` + strings.Repeat("a", 64) + `"}`),
	}
	for _, rt := range routes {
		key := rt.method + " " + rt.pattern
		q := req{method: rt.method, path: concretePath(rt.pattern), token: r.token(loginB), header: hostile.Clone()}
		if b, ok := good[key]; ok {
			q.body, q.ctype = b, "application/json"
		}
		if key == "POST /v1/jobs/{id}/confirm" {
			q.header.Set("Idempotency-Key", "key-1")
		}
		r.store.reset()
		resp, body := r.do(q)
		if resp.StatusCode >= 500 {
			t.Fatalf("%s: %d %s", key, resp.StatusCode, body)
		}
		calls := r.store.seen()
		if len(calls) == 0 {
			t.Errorf("%s: the store was never reached, so nothing was proved for this route", key)
		}
		for _, c := range calls {
			if c.User != userB {
				t.Errorf("%s: the store was called for user %s, want %s (the principal)", key, c.User, userB)
			}
		}
	}

	// naming a user in a body or in a query is refused, and reaches nothing; the same request without it is served (the control)
	for _, c := range []struct {
		name, method, path, body string
		hdr                      http.Header
	}{
		{"create, user_id in the body", "POST", "/v1/jobs", `{"module":"m1","user_id":"` + userA + `","rows":[{"email":"a@example.test"}]}`, nil},
		{"create, owner_user_id in a row", "POST", "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test","owner_user_id":"` + userA + `"}]}`, nil},
		{"confirm, user_id in the body", "POST", "/v1/jobs/" + jobIDA + "/confirm", `{"fingerprint":"` + strings.Repeat("a", 64) + `","user_id":"` + userA + `"}`, http.Header{"Idempotency-Key": {"key-2"}}},
		{"list, user_id in the query", "GET", "/v1/jobs?user_id=" + userA, "", nil},
		{"get, user_id in the query", "GET", "/v1/jobs/" + jobIDA + "?user_id=" + userA, "", nil},
		{"results, user in the query", "GET", "/v1/jobs/" + jobIDA + "/results?user=" + userA, "", nil},
		{"results.csv, user_id in the query", "GET", "/v1/jobs/" + jobIDA + "/results.csv?user_id=" + userA, "", nil},
		{"events, user_id in the query", "GET", "/v1/jobs/" + jobIDA + "/events?user_id=" + userA, "", nil},
	} {
		r.store.reset()
		q := req{method: c.method, path: c.path, token: r.token(loginB), header: c.hdr}
		if c.body != "" {
			q.body, q.ctype = []byte(c.body), "application/json"
		}
		resp, body := r.do(q)
		r.wantError(resp, body, http.StatusBadRequest, "bad_request")
		if n := len(r.store.seen()); n != 0 {
			t.Errorf("%s: a request naming a user reached the store (%d calls)", c.name, n)
		}
	}
}
