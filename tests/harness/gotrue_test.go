//go:build harness && gotrue

package harness

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 4, chunk B15 (Docs/phase-4-spec.md "Locked here" 29): the platform's token verification is proved against tokens issued by the REAL GoTrue (the digest-pinned supabase/gotrue of
// docker-compose.gotrue.yml, started by tests/harness/gotrue/stage.sh) on the real Supabase database image, not against tokens this repository wrote itself. internal/auth was written from the documented
// shape of a Supabase access token; these tests are where that shape meets the thing that makes them.
//
// What is proved, each beside a control that fails when the property breaks:
//   TestGoTrue_Preconditions                    GoTrue is up and configured as the other tests assume (sign-up open, mail confirmed at once, every other way to sign in off); the checker that says so can say no
//   TestGoTrue_SignupAndPasswordGrantTokensVerify
//                                               POST /signup and POST /token?grant_type=password each return an access token that auth.Verifier accepts under the configured secret, issuer and audience;
//                                               Subject is the id GoTrue returned and the id of the row GoTrue wrote to auth.users; a wrong password gets no token
//   TestGoTrue_TamperedAndForeignTokensAreRefused
//                                               a flipped signature character, a changed claim under the old signature, another secret, alg none, an asymmetric alg, a clock two hours on,
//                                               another issuer and another audience: each refused with ITS OWN sentinel, beside the same token accepted unaltered
//   TestGoTrue_ThePlatformIdentityChain         login -> verified Subject -> tenant.Resolver.ByAuthUser -> the platform user (id, login, active); a GoTrue login with no platform user is ErrUnknownLogin and
//                                               nothing is created for it
//   TestGoTrue_InactiveUserIsRefusedByTheResolverNotByTheToken
//                                               the revocation lever of the platform: public.users.is_active. The token verifies before and after; the resolver refuses after, and accepts again when the flag is restored
//   TestGoTrue_DeletedLoginIsRefusedByGoTrueButItsOldTokenStillVerifies
//                                               DELETE /admin/users/{id} with a service-role key minted from the same secret: GoTrue's own /user then refuses the old token while its signature still verifies
//                                               here (a signature check is not a revocation check)
//
// NEVER PRINTED: the secret, any token, any password. Tokens and the secret are config.Secret (every fmt verb prints [redacted]); a response body is never logged, only a scrubbed error code and
// message; stage.sh (gotrue_secret_leak) then scans the container's log and this run's own log for the secret, the database password and anything JWT-shaped.

const (
	gtrEnvURL    = "HARNESS_GOTRUE_URL"
	gtrEnvSecret = "HARNESS_GOTRUE_JWT_SECRET"
	gtrEnvIssuer = "HARNESS_GOTRUE_ISSUER"
	gtrAudience  = "authenticated"
	gtrExpiry    = time.Hour // GOTRUE_JWT_EXP=3600 in docker-compose.gotrue.yml
)

// gtrJWTShape is what a compact JWT looks like: used to scrub anything that is printed.
var gtrJWTShape = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// gtrSession is the part of GoTrue's answer to /signup and /token that the tests read. The two tokens are config.Secret: printing a session with any verb shows [redacted].
type gtrSession struct {
	AccessToken  config.Secret `json:"access_token"`
	TokenType    string        `json:"token_type"`
	ExpiresIn    int           `json:"expires_in"`
	RefreshToken config.Secret `json:"refresh_token"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Aud   string `json:"aud"`
		Role  string `json:"role"`
	} `json:"user"`
}

// gtrLogin is a user the test signed up: the email and password are generated per call, never printed.
type gtrLogin struct {
	Email    string
	password string
	Session  gtrSession
}

type gtrEnv struct {
	t      *testing.T
	ctx    context.Context
	url    string
	secret config.Secret // GOTRUE_JWT_SECRET, the raw bytes of the string are the HS256 key
	issuer string        // GOTRUE_JWT_ISSUER: the issuer the platform is configured with
	hc     *http.Client
	admin  *pgx.Conn // the image's superuser (read-write): reads auth.users, writes the platform's public.users
	v      *auth.Verifier
}

func newGtrEnv(t *testing.T) *gtrEnv {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // GoTrue is on the private network: nothing may route the request elsewhere
	e := &gtrEnv{
		t:      t,
		ctx:    testCtx(t),
		url:    strings.TrimRight(requireEnv(t, gtrEnvURL), "/"),
		secret: config.Secret(requireEnv(t, gtrEnvSecret)),
		issuer: requireEnv(t, gtrEnvIssuer),
		hc:     &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if n := len(e.secret.Reveal()); n < auth.MinHMACSecretBytes {
		t.Fatalf("%s is %d bytes, internal/auth needs at least %d: the stage mints 48 random bytes in hex (stage 2)", gtrEnvSecret, n, auth.MinHMACSecretBytes)
	}
	e.admin = dial(t, requireEnv(t, envAdminURL), nil)
	e.v = e.verifier(auth.Options{})
	return e
}

// verifier builds a Verifier from the harness's configuration; o overrides what it names (the zero Options is the platform's own configuration: this secret, this issuer, the audience `authenticated`).
func (e *gtrEnv) verifier(o auth.Options) *auth.Verifier {
	e.t.Helper()
	if len(o.HMACSecret) == 0 {
		o.HMACSecret = []byte(e.secret.Reveal())
	}
	if o.Issuer == "" {
		o.Issuer = e.issuer
	}
	if o.Audience == "" {
		o.Audience = gtrAudience
	}
	v, err := auth.New(o)
	if err != nil {
		e.t.Fatalf("auth.New refused the harness's configuration: %v", err)
	}
	return v
}

// call sends one request to GoTrue and returns the status and the raw body. The body is never logged here: it holds tokens.
func (e *gtrEnv) call(method, path string, bearer config.Secret, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("encode the body of %s %s: %v", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.url+path, rd)
	if err != nil {
		e.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer.Reveal())
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: the request failed: %v (is GoTrue reachable at %s from this container? tests/harness/gotrue/stage.sh waited for it)", method, path, err, e.url)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		e.t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return resp.StatusCode, b
}

// gtrWhy says why a call failed without echoing the body: the status and GoTrue's own error code and message, with anything JWT-shaped removed and the length cut.
func gtrWhy(status int, body []byte) string {
	var m struct {
		ErrorCode string `json:"error_code"`
		Msg       string `json:"msg"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &m)
	var parts []string
	for _, s := range []string{m.ErrorCode, m.Msg, m.Error, m.Desc} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	text := "(no error text in the answer)"
	if len(parts) > 0 {
		text = strings.Join(parts, " | ")
	}
	text = gtrJWTShape.ReplaceAllString(text, "[jwt]")
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return fmt.Sprintf("HTTP %d %s", status, text)
}

// gtrErrorCode is GoTrue's machine-readable error code of a refusal, or "" when the body has none.
func gtrErrorCode(body []byte) string {
	var m struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &m)
	return gtrJWTShape.ReplaceAllString(m.ErrorCode, "[jwt]")
}

func gtrShort(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func gtrRefusal(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// gtrParts splits a compact token into its decoded header and payload and its raw signature segment.
func gtrParts(tok config.Secret) (header, payload []byte, sig string, ok bool) {
	p := strings.Split(tok.Reveal(), ".")
	if len(p) != 3 {
		return nil, nil, "", false
	}
	h, err1 := base64.RawURLEncoding.DecodeString(p[0])
	pl, err2 := base64.RawURLEncoding.DecodeString(p[1])
	if err1 != nil || err2 != nil {
		return nil, nil, "", false
	}
	return h, pl, p[2], true
}

// gtrSignHS256 builds a compact token: the header and the payload as given, signed HS256 with the raw bytes of secret.
func gtrSignHS256(secret string, header, payload []byte) string {
	enc := base64.RawURLEncoding
	in := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(in))
	return in + "." + enc.EncodeToString(m.Sum(nil))
}

// gtrServiceRoleKey mints the credential GoTrue's admin API accepts: a token whose role is one of GOTRUE_JWT_ADMIN_ROLES (service_role), signed with the same secret. It is what a Supabase project's
// service_role key is, and it is NOT a user token: it has no sub.
func gtrServiceRoleKey(secret config.Secret, now time.Time) config.Secret {
	payload := fmt.Sprintf(`{"role":"service_role","iss":"supabase","iat":%d,"exp":%d}`, now.Unix(), now.Add(10*time.Minute).Unix())
	return config.Secret(gtrSignHS256(secret.Reveal(), []byte(`{"alg":"HS256","typ":"JWT"}`), []byte(payload)))
}

// gtrFlipSignatureChar changes one character in the MIDDLE of the signature segment: all six bits of it count. (The last character of a 43-character signature carries only four, and the decoder
// is strict about the unused two, so a flip there is a malformed token and not a wrong signature.)
func gtrFlipSignatureChar(tok string) string {
	i := strings.LastIndexByte(tok, '.')
	sig := []byte(tok[i+1:])
	k := len(sig) / 2
	if sig[k] == 'A' {
		sig[k] = 'B'
	} else {
		sig[k] = 'A'
	}
	return tok[:i+1] + string(sig)
}

// gtrTamperPayload changes the claims and keeps the header and the signature of the original.
func gtrTamperPayload(tok config.Secret, mutate func(map[string]any)) string {
	p := strings.Split(tok.Reveal(), ".")
	raw, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	mutate(m)
	out, _ := json.Marshal(m)
	return p[0] + "." + base64.RawURLEncoding.EncodeToString(out) + "." + p[2]
}

// gtrSettingsProblems is the check of GoTrue's /settings answer, as a pure function so that a control can feed it a doctored document: sign-up open, mail confirmed at once, the e-mail provider the
// only one on.
func gtrSettingsProblems(s map[string]any) []string {
	var p []string
	if v, ok := s["disable_signup"].(bool); !ok || v {
		p = append(p, "disable_signup is not false (GOTRUE_DISABLE_SIGNUP): nobody could sign up")
	}
	if v, ok := s["mailer_autoconfirm"].(bool); !ok || !v {
		p = append(p, "mailer_autoconfirm is not true (GOTRUE_MAILER_AUTOCONFIRM): a sign-up would wait for a mail that never arrives and return no session")
	}
	ext, ok := s["external"].(map[string]any)
	if !ok {
		return append(p, "the settings carry no `external` map of providers")
	}
	if v, _ := ext["email"].(bool); !v {
		p = append(p, "external.email is not true (GOTRUE_EXTERNAL_EMAIL_ENABLED)")
	}
	names := make([]string, 0, len(ext))
	for k := range ext {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if v, isBool := ext[k].(bool); isBool && v && k != "email" {
			p = append(p, "external."+k+" is enabled: every way to sign in but e-mail and password is off in this harness")
		}
	}
	return p
}

func gtrCleanSettings() map[string]any {
	return map[string]any{
		"disable_signup":     false,
		"mailer_autoconfirm": true,
		"external":           map[string]any{"email": true, "phone": false, "google": false, "github": false},
	}
}

// ---- GoTrue calls ----

func (e *gtrEnv) decodeSession(what string, status int, body []byte) gtrSession {
	e.t.Helper()
	if status != http.StatusOK {
		e.t.Fatalf("%s: %s", what, gtrWhy(status, body))
	}
	var s gtrSession
	if err := json.Unmarshal(body, &s); err != nil {
		e.t.Fatalf("%s: HTTP 200 but the answer is not the session JSON: %v", what, err)
	}
	if s.AccessToken == "" || s.User.ID == "" {
		e.t.Fatalf("%s: HTTP 200 but the answer holds no access_token or no user id: with GOTRUE_MAILER_AUTOCONFIRM off a sign-up returns the user and no session", what)
	}
	return s
}

// checkTokenShape reads the header and the payload of a token WITHOUT verifying it and fails, naming the difference, when GoTrue signs with something other than HS256 or puts another issuer in iss
// than the one it is configured with (GOTRUE_JWT_ISSUER == HARNESS_GOTRUE_ISSUER == the platform's issuer). A mismatch here is a configuration fact, not a verification failure.
func (e *gtrEnv) checkTokenShape(what string, tok config.Secret) {
	e.t.Helper()
	h, pl, _, ok := gtrParts(tok)
	if !ok {
		e.t.Fatalf("%s: the access token is not three base64url segments", what)
	}
	var hdr, claims map[string]any
	if json.Unmarshal(h, &hdr) != nil || json.Unmarshal(pl, &claims) != nil {
		e.t.Fatalf("%s: the token's header or payload is not a JSON object", what)
	}
	if alg, _ := hdr["alg"].(string); alg != "HS256" {
		e.t.Fatalf("%s: GoTrue signed this token with alg %q, not HS256: this harness configures GOTRUE_JWT_SECRET (a symmetric key) and the verifier is built from the same secret; if the image defaults to asymmetric signing keys, the secret does not apply", what, alg)
	}
	iss, present := claims["iss"].(string)
	if !present {
		e.t.Fatalf("%s: the token has no iss claim: GOTRUE_JWT_ISSUER should make GoTrue write %q", what, e.issuer)
	}
	if iss != e.issuer {
		e.t.Fatalf("%s: GoTrue's tokens carry iss=%q but the configured issuer (GOTRUE_JWT_ISSUER = HARNESS_GOTRUE_ISSUER = what PLATFORM_JWT_ISSUER will be set to) is %q: the two must be the same string", what, iss, e.issuer)
	}
}

// signup signs a new user up (POST /signup: with autoconfirm GoTrue answers with a session), checks the token's shape and returns the login. The email and the password are generated.
func (e *gtrEnv) signup(tag string) *gtrLogin {
	t := e.t
	t.Helper()
	l := &gtrLogin{Email: fmt.Sprintf("gotrue-%s-%s@example.test", tag, randomHex(t, 6)), password: randomHex(t, 16)}
	status, body := e.call(http.MethodPost, "/signup", "", map[string]string{"email": l.Email, "password": l.password})
	l.Session = e.decodeSession("POST /signup ("+tag+")", status, body)
	e.checkTokenShape("POST /signup ("+tag+")", l.Session.AccessToken)
	return l
}

// grant is POST /token?grant_type=password for the login.
func (e *gtrEnv) grant(l *gtrLogin, password string) (int, []byte) {
	e.t.Helper()
	return e.call(http.MethodPost, "/token?grant_type=password", "", map[string]string{"email": l.Email, "password": password})
}

type gtrAuthRow struct {
	Found                  bool
	Email, Aud, Role       string
	Confirmed, HasPassword bool
}

// authRow reads what GoTrue wrote to auth.users for the id.
func (e *gtrEnv) authRow(id string) gtrAuthRow {
	e.t.Helper()
	var r gtrAuthRow
	err := e.admin.QueryRow(e.ctx, `SELECT coalesce(email, ''), coalesce(aud, ''), coalesce(role, ''), email_confirmed_at IS NOT NULL, coalesce(encrypted_password, '') <> ''
		FROM auth.users WHERE id = $1::uuid`, id).Scan(&r.Email, &r.Aud, &r.Role, &r.Confirmed, &r.HasPassword)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return gtrAuthRow{}
	case err != nil:
		e.t.Fatalf("read auth.users for %s: %v", gtrShort(id), err)
	}
	r.Found = true
	return r
}

func (e *gtrEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%v\n  sql: %s", err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

// seedPlatformUser creates the platform user of a login the way an admin does (public.users, id = auth_user_id: users_auth_link). Users are never created by a login.
func (e *gtrEnv) seedPlatformUser(l *gtrLogin) {
	e.t.Helper()
	if _, err := e.admin.Exec(e.ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1::uuid, $1::uuid, $2)", l.Session.User.ID, l.Email); err != nil {
		e.t.Fatalf("seed public.users for %s: %v", gtrShort(l.Session.User.ID), err)
	}
}

// resolver is the platform's tenant.Resolver over real logins as platform_app (the identity the service runs as), proved not to be a superuser.
func (e *gtrEnv) resolver() *tenant.Resolver {
	t := e.t
	t.Helper()
	dsn := requireEnv(t, envAdminURL)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("cannot parse the connection string: %s", scrubbed(err, dsn))
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "platform_app", requireEnv(t, envAppPassword)
	delete(cfg.ConnConfig.RuntimeParams, "timezone")
	delete(cfg.ConnConfig.RuntimeParams, "options")
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open the platform_app pool: %s", scrubbed(err, dsn))
	}
	t.Cleanup(pool.Close)
	var who string
	var super bool
	if err := pool.QueryRow(e.ctx, "SELECT session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&who, &super); err != nil {
		t.Fatalf("the platform_app pool cannot run a query: %s", scrubbed(err, dsn))
	}
	if who != "platform_app" || super {
		t.Fatalf("the pool logged in as %q (superuser %v), want a real login as platform_app and no superuser", who, super)
	}
	res, err := tenant.NewResolver(pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	return res
}

// verifyOK verifies the login's access token with the platform's own configuration and fails the test when it is refused.
func (e *gtrEnv) verifyOK(what string, tok config.Secret) auth.Claims {
	e.t.Helper()
	c, err := e.v.Verify(e.ctx, tok.Reveal())
	if err != nil {
		e.t.Fatalf("%s: auth.Verifier refused a token GoTrue issued: %v", what, err)
	}
	return c
}

// ---- the tests ----

// TestGoTrue_Preconditions: GoTrue answers /health and /settings from inside the private network and is configured the way the other tests assume. The checker that reads /settings is itself
// exercised on documents that are wrong in each way it names (a checker that cannot say no is not a precondition).
func TestGoTrue_Preconditions(t *testing.T) {
	e := newGtrEnv(t)

	status, body := e.call(http.MethodGet, "/health", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /health: %s", gtrWhy(status, body))
	}
	var h struct{ Version, Name, Description string }
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("GET /health answered HTTP 200 with a body that is not JSON: %v", err)
	}
	t.Logf("NOTE health: HTTP 200 from %s, name=%q version=%q (the image is pinned by digest in docker-compose.gotrue.yml: the version is what it says it is, not a claim of this test)", e.url, h.Name, h.Version)

	status, body = e.call(http.MethodGet, "/settings", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /settings: %s", gtrWhy(status, body))
	}
	var s map[string]any
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("GET /settings answered HTTP 200 with a body that is not a JSON object: %v", err)
	}
	for _, p := range gtrSettingsProblems(s) {
		t.Errorf("GoTrue's settings: %s", p)
	}
	var on []string
	if ext, ok := s["external"].(map[string]any); ok {
		for k, v := range ext {
			if b, isBool := v.(bool); isBool && b {
				on = append(on, k)
			}
		}
	}
	sort.Strings(on)
	t.Logf("NOTE settings: disable_signup=%v mailer_autoconfirm=%v; sign-in methods enabled: %v", s["disable_signup"], s["mailer_autoconfirm"], on)

	// CONTROL: the checker accepts a good document and names each way a document can be wrong.
	if p := gtrSettingsProblems(gtrCleanSettings()); len(p) != 0 {
		t.Errorf("control: the settings checker flagged a clean document: %v", p)
	}
	for _, c := range []struct {
		what, want string
		mutate     func(map[string]any)
	}{
		{"a provider switched on", "external.google is enabled", func(m map[string]any) { m["external"].(map[string]any)["google"] = true }},
		{"sign-up closed", "disable_signup is not false", func(m map[string]any) { m["disable_signup"] = true }},
		{"mail confirmation required", "mailer_autoconfirm is not true", func(m map[string]any) { m["mailer_autoconfirm"] = false }},
		{"the e-mail provider off", "external.email is not true", func(m map[string]any) { m["external"].(map[string]any)["email"] = false }},
		{"no provider map", "no `external` map", func(m map[string]any) { delete(m, "external") }},
	} {
		doc := gtrCleanSettings()
		c.mutate(doc)
		if p := strings.Join(gtrSettingsProblems(doc), " ; "); !strings.Contains(p, c.want) {
			t.Errorf("control: for %s the settings checker said %q, want it to name %q", c.what, p, c.want)
		}
	}

	// the verifier the platform will build from its configuration says what it accepts
	sum := e.v.Summary()
	if !sum.HMAC || sum.JWKS || !sum.IssuerPinned || sum.Audience != gtrAudience {
		t.Errorf("the verifier built from the harness's configuration is %+v, want HS256 only, the issuer pinned and the audience %q", sum, gtrAudience)
	}
}

// TestGoTrue_SignupAndPasswordGrantTokensVerify: a user signs up (POST /signup) and signs in (POST /token?grant_type=password); each answer carries an access token; auth.Verifier accepts both under
// the configured secret, issuer and audience; Subject is the id GoTrue returned and the id of the row GoTrue wrote to auth.users. Controls: a wrong password gets no token; an id GoTrue never issued
// has no row; the verifier configured with another secret refuses the very token it just accepted.
func TestGoTrue_SignupAndPasswordGrantTokensVerify(t *testing.T) {
	e := newGtrEnv(t)
	l := e.signup("tokens")
	id := l.Session.User.ID

	// the sign-up token
	c := e.verifyOK("the sign-up token", l.Session.AccessToken)
	if c.Subject != id {
		t.Errorf("the sign-up token's verified Subject is %s but GoTrue returned the user id %s", gtrShort(c.Subject), gtrShort(id))
	}
	if c.Issuer != e.issuer {
		t.Errorf("the verified Issuer is %q, want %q", c.Issuer, e.issuer)
	}
	if len(c.Audience) != 1 || c.Audience[0] != gtrAudience {
		t.Errorf("the verified Audience is %v, want [%s]", c.Audience, gtrAudience)
	}
	if c.Role != "authenticated" || c.Email != l.Email {
		t.Errorf("the verified Role/Email are %q/%q, want authenticated/the sign-up email (carried for information, never trusted)", c.Role, c.Email)
	}
	if got := c.ExpiresAt.Sub(c.IssuedAt); got != gtrExpiry {
		t.Errorf("exp - iat is %s, want %s (GOTRUE_JWT_EXP=3600)", got, gtrExpiry)
	}
	if l.Session.ExpiresIn != int(gtrExpiry.Seconds()) {
		t.Errorf("expires_in is %d, want %d", l.Session.ExpiresIn, int(gtrExpiry.Seconds()))
	}

	// the row GoTrue wrote
	row := e.authRow(id)
	if !row.Found {
		t.Fatalf("GoTrue returned user %s but auth.users has no such row", gtrShort(id))
	}
	if row.Email != l.Email || row.Aud != gtrAudience || row.Role != "authenticated" || !row.Confirmed || !row.HasPassword {
		t.Errorf("the auth.users row of %s is email-matches=%v aud=%q role=%q confirmed=%v has-password=%v, want the sign-up email, authenticated/authenticated, confirmed at once (autoconfirm) and a password hash",
			gtrShort(id), row.Email == l.Email, row.Aud, row.Role, row.Confirmed, row.HasPassword)
	}
	if n := e.count("SELECT count(*) FROM auth.identities WHERE user_id = $1::uuid AND provider = 'email'", id); n != 1 {
		t.Errorf("auth.identities holds %d e-mail identities for the new user, want 1 (written by GoTrue itself)", n)
	}
	if c.SessionID != "" {
		if n := e.count("SELECT count(*) FROM auth.sessions WHERE id = $1::uuid AND user_id = $2::uuid", c.SessionID, id); n != 1 {
			t.Errorf("the token's session_id names %d rows of auth.sessions for this user, want 1", n)
		}
	}

	// the password grant
	status, body := e.grant(l, l.password)
	s2 := e.decodeSession("POST /token?grant_type=password", status, body)
	e.checkTokenShape("POST /token?grant_type=password", s2.AccessToken)
	c2 := e.verifyOK("the password-grant token", s2.AccessToken)
	if c2.Subject != id || s2.User.ID != id {
		t.Errorf("the password grant answered user %s and its token's Subject is %s, want the sign-up user %s", gtrShort(s2.User.ID), gtrShort(c2.Subject), gtrShort(id))
	}
	if s2.AccessToken == l.Session.AccessToken {
		t.Errorf("the password grant returned the sign-up token itself: it must be a new session's token")
	}

	// CONTROLS
	status, body = e.grant(l, l.password+"x")
	if status == http.StatusOK || bytes.Contains(body, []byte("access_token")) {
		t.Errorf("control: a wrong password got HTTP %d and a body with an access_token: GoTrue must refuse it", status)
	}
	if !gtrRefusal(status) && status != http.StatusBadRequest {
		t.Errorf("control: a wrong password got %s, want a 400 invalid_credentials", gtrWhy(status, body))
	}
	var unknown string
	if err := e.admin.QueryRow(e.ctx, "SELECT gen_random_uuid()::text").Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if e.authRow(unknown).Found {
		t.Errorf("control: auth.users has a row for an id GoTrue never issued")
	}
	other := e.verifier(auth.Options{HMACSecret: []byte(randomHex(t, 48))})
	if cl, err := other.Verify(e.ctx, l.Session.AccessToken.Reveal()); !errors.Is(err, auth.ErrSignature) || cl.Subject != "" {
		t.Errorf("control: a verifier configured with another secret answered %v for the token the real one accepted, want ErrSignature and no claims", err)
	}
	t.Logf("NOTE tokens: user %s signed up (HTTP 200, autoconfirm) and signed in (HTTP 200); both tokens verified with Subject = the user id = the auth.users row; iss=%s aud=%s role=%s exp-iat=%s; session_id present: %v; wrong password: %s",
		gtrShort(id), c.Issuer, gtrAudience, c.Role, c.ExpiresAt.Sub(c.IssuedAt), c.SessionID != "", gtrWhy(status, body))
}

// TestGoTrue_TamperedAndForeignTokensAreRefused: each way a token can be wrong is refused with ITS OWN sentinel, and the unaltered token is accepted beside every one (the control that says the
// refusal is about the alteration and not about the setup). The re-signing helper is itself proved: signing the real token's own header and payload with the real secret reproduces GoTrue's token
// byte for byte.
func TestGoTrue_TamperedAndForeignTokensAreRefused(t *testing.T) {
	e := newGtrEnv(t)
	l := e.signup("tamper")
	tok := l.Session.AccessToken
	good := tok.Reveal()
	hdr, pl, _, ok := gtrParts(tok)
	if !ok {
		t.Fatalf("cannot split the token GoTrue issued")
	}

	// CONTROL: the unaltered token verifies, and the signing helper reproduces GoTrue's signature exactly (so the forged tokens below differ from the real one in the alteration alone)
	if c, err := e.v.Verify(e.ctx, good); err != nil || c.Subject != l.Session.User.ID {
		t.Fatalf("control: the unaltered token is refused (%v) or names another subject: nothing below means anything", err)
	}
	if again := gtrSignHS256(e.secret.Reveal(), hdr, pl); again != good {
		t.Fatalf("control: signing the token's own header and payload with the configured secret does not reproduce GoTrue's token: GoTrue does not sign HS256 over the raw secret string (so the verifier and GoTrue disagree about the key)")
	}

	var otherUser string
	if err := e.admin.QueryRow(e.ctx, "SELECT gen_random_uuid()::text").Scan(&otherUser); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return time.Now().Add(2 * time.Hour) }
	cases := []struct {
		name  string
		token string
		v     *auth.Verifier
		want  error
	}{
		{"one flipped character in the signature", gtrFlipSignatureChar(good), e.v, auth.ErrSignature},
		{"another sub under GoTrue's signature", gtrTamperPayload(tok, func(m map[string]any) { m["sub"] = otherUser }), e.v, auth.ErrSignature},
		{"role service_role under GoTrue's signature", gtrTamperPayload(tok, func(m map[string]any) { m["role"] = "service_role" }), e.v, auth.ErrSignature},
		{"the header and payload signed with another secret", gtrSignHS256(randomHex(t, 48), hdr, pl), e.v, auth.ErrSignature},
		{"alg none and no signature", gtrUnsecured(pl), e.v, auth.ErrAlgorithm},
		{"alg RS256 where only a secret is configured", gtrTamperHeader(tok, "RS256"), e.v, auth.ErrAlgorithm},
		{"the genuine token two hours later", good, e.verifier(auth.Options{Now: later}), auth.ErrExpired},
		{"the genuine token under another issuer", good, e.verifier(auth.Options{Issuer: e.issuer + "/other"}), auth.ErrIssuer},
		{"the genuine token under another audience", good, e.verifier(auth.Options{Audience: "service"}), auth.ErrAudience},
		{"the genuine token under another secret", good, e.verifier(auth.Options{HMACSecret: []byte(randomHex(t, 48))}), auth.ErrSignature},
	}
	for _, c := range cases {
		if c.token == "" {
			t.Errorf("%s: the test could not build the token", c.name)
			continue
		}
		cl, err := c.v.Verify(e.ctx, c.token)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: auth.Verifier answered %v, want %v", c.name, err, c.want)
		}
		if cl.Subject != "" || cl.Role != "" || cl.Email != "" {
			t.Errorf("%s: a refused token still returned claims", c.name)
		}
	}
	t.Logf("NOTE tamper: the unaltered token verified, and the signing helper reproduced it byte for byte; %d alterations each refused with their own sentinel (signature x5 incl. a changed sub and a changed role, algorithm x2, expired, issuer, audience)", len(cases))
}

// gtrUnsecured is the payload under an "alg":"none" header with an empty signature segment: the shape of an unsecured JWT.
func gtrUnsecured(payload []byte) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + "."
}

// gtrTamperHeader keeps the payload and the signature and changes the header's alg.
func gtrTamperHeader(tok config.Secret, alg string) string {
	p := strings.Split(tok.Reveal(), ".")
	h, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(h, &m) != nil {
		return ""
	}
	m["alg"] = alg
	out, _ := json.Marshal(m)
	return base64.RawURLEncoding.EncodeToString(out) + "." + p[1] + "." + p[2]
}

// TestGoTrue_ThePlatformIdentityChain: the chain of Locked decision 29 on a real login. A user signs up; the verified Subject resolves through tenant.Resolver.ByAuthUser to the platform user an admin
// created for that login (id = the auth id, active). A second GoTrue login with NO platform user is tenant.ErrUnknownLogin, and nothing is created for it (a login never makes a user).
func TestGoTrue_ThePlatformIdentityChain(t *testing.T) {
	e := newGtrEnv(t)
	res := e.resolver()
	a := e.signup("chain-a")
	b := e.signup("chain-nobody")
	e.seedPlatformUser(a)

	ca := e.verifyOK("login A's token", a.Session.AccessToken)
	ten, err := res.ByAuthUser(e.ctx, ca.Subject)
	if err != nil {
		t.Fatalf("ByAuthUser(the verified Subject of login A) = %v, want the platform user", err)
	}
	if want := (tenant.Tenant{UserID: a.Session.User.ID, AuthUserID: a.Session.User.ID, Active: true}); ten != want {
		t.Errorf("ByAuthUser(A) = %+v, want %+v", ten, want)
	}
	if n := e.count("SELECT count(*) FROM public.users u JOIN auth.users a ON a.id = u.auth_user_id WHERE u.id = $1::uuid AND u.id = a.id AND u.email = a.email", a.Session.User.ID); n != 1 {
		t.Errorf("the platform user of A joins %d auth.users rows on id and email, want 1: the platform user IS GoTrue's login", n)
	}

	// the login that has a GoTrue account and no platform user
	cb := e.verifyOK("login B's token", b.Session.AccessToken)
	usersBefore := e.count("SELECT count(*) FROM public.users")
	got, err := res.ByAuthUser(e.ctx, cb.Subject)
	if !errors.Is(err, tenant.ErrUnknownLogin) || got != (tenant.Tenant{}) {
		t.Errorf("ByAuthUser(the verified Subject of a login with no platform user) = %+v, %v; want the zero Tenant and tenant.ErrUnknownLogin", got, err)
	}
	if n := e.count("SELECT count(*) FROM public.users"); n != usersBefore {
		t.Errorf("public.users holds %d rows after the unknown login was refused, %d before: a login must never create a user", n, usersBefore)
	}
	if n := e.count("SELECT count(*) FROM public.users WHERE id = $1::uuid OR auth_user_id = $1::uuid", b.Session.User.ID); n != 0 {
		t.Errorf("a platform user exists for login B (%d rows) although nobody created one", n)
	}

	// CONTROLS: the two logins are two people (A's token never resolves to B's id), and the chain does not depend on which one is asked first
	if ca.Subject == cb.Subject || ten.UserID == b.Session.User.ID {
		t.Errorf("control: logins A and B share an identity (%s, %s)", gtrShort(ca.Subject), gtrShort(cb.Subject))
	}
	if again, err := res.ByAuthUser(e.ctx, strings.ToUpper(ca.Subject)); err != nil || again != ten {
		t.Errorf("control: the same login asked in upper case resolved to %+v, %v, want %+v", again, err, ten)
	}
	t.Logf("NOTE chain: login %s -> verified Subject -> ByAuthUser -> platform user %s (id = login, active); login %s (a real GoTrue account, a verified token) -> ErrUnknownLogin and public.users stayed at %d rows",
		gtrShort(a.Session.User.ID), gtrShort(ten.UserID), gtrShort(b.Session.User.ID), usersBefore)
}

// TestGoTrue_InactiveUserIsRefusedByTheResolverNotByTheToken: the platform's revocation lever is public.users.is_active, and a token cannot carry it. The same token verifies before and after the
// switch; the resolver says ErrInactive after and accepts again when the flag is restored (so the flag, and nothing else, is the cause). GoTrue itself does not know the flag and still serves the user.
func TestGoTrue_InactiveUserIsRefusedByTheResolverNotByTheToken(t *testing.T) {
	e := newGtrEnv(t)
	res := e.resolver()
	l := e.signup("inactive")
	e.seedPlatformUser(l)
	id := l.Session.User.ID

	c := e.verifyOK("before the switch", l.Session.AccessToken)
	if ten, err := res.ByAuthUser(e.ctx, c.Subject); err != nil || ten.UserID != id || !ten.Active { // CONTROL: active, it resolves
		t.Fatalf("control: the active user does not resolve (%+v, %v)", ten, err)
	}

	if _, err := e.admin.Exec(e.ctx, "UPDATE public.users SET is_active = false WHERE id = $1::uuid", id); err != nil {
		t.Fatalf("switch the user off: %v", err)
	}
	c2 := e.verifyOK("after the switch", l.Session.AccessToken)
	if c2.Subject != id {
		t.Errorf("after the switch the same token verifies to Subject %s, want %s", gtrShort(c2.Subject), gtrShort(id))
	}
	got, err := res.ByAuthUser(e.ctx, c2.Subject)
	if !errors.Is(err, tenant.ErrInactive) || got != (tenant.Tenant{}) {
		t.Errorf("ByAuthUser after is_active=false = %+v, %v; want the zero Tenant and tenant.ErrInactive", got, err)
	}
	status, body := e.call(http.MethodGet, "/user", l.Session.AccessToken, nil)
	if status != http.StatusOK {
		t.Errorf("GoTrue's /user answered %s for a user the PLATFORM switched off: GoTrue does not know that flag and should still serve the user", gtrWhy(status, body))
	}

	if _, err := e.admin.Exec(e.ctx, "UPDATE public.users SET is_active = true WHERE id = $1::uuid", id); err != nil {
		t.Fatalf("switch the user on again: %v", err)
	}
	if ten, err := res.ByAuthUser(e.ctx, c2.Subject); err != nil || ten.UserID != id || !ten.Active { // CONTROL: the flag alone was the cause
		t.Errorf("control: after is_active=true again ByAuthUser = %+v, %v; want the user back", ten, err)
	}
	t.Logf("NOTE inactive: the same access token verified before and after is_active=false (a signature check is not a revocation check); the resolver said ErrInactive while the flag was false and resolved the user again when it was restored; GoTrue's own /user served the user throughout (HTTP %d)", status)
}

// TestGoTrue_DeletedLoginIsRefusedByGoTrueButItsOldTokenStillVerifies: GoTrue's admin API (a service-role key minted from the same secret) deletes a login. GoTrue's own /user then refuses the old
// token, while the token's SIGNATURE (and exp, iss, aud) still verifies in internal/auth. That is the documented limit of stateless verification, not a defect of the verifier; the platform's own
// check (the resolver, which reads public.users on every call) is what refuses the login: an unknown login. Controls: before the delete GoTrue serves the token; a user token is not an admin
// credential; a service-role key signed with another secret is not one either (and the user it tried to delete is still there).
func TestGoTrue_DeletedLoginIsRefusedByGoTrueButItsOldTokenStillVerifies(t *testing.T) {
	e := newGtrEnv(t)
	res := e.resolver()
	victim := e.signup("deleted")
	bystander := e.signup("bystander")
	vid, bid := victim.Session.User.ID, bystander.Session.User.ID
	svc := gtrServiceRoleKey(e.secret, time.Now())

	// CONTROLS before: GoTrue serves the token; the admin route wants an admin credential
	status, body := e.call(http.MethodGet, "/user", victim.Session.AccessToken, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(vid)) {
		t.Fatalf("control: GoTrue's /user did not serve the live user's token: %s", gtrWhy(status, body))
	}
	if status, body = e.call(http.MethodDelete, "/admin/users/"+bid, victim.Session.AccessToken, nil); !gtrRefusal(status) {
		t.Fatalf("control: a USER token on DELETE /admin/users/{id} got %s, want a refusal (401 or 403): the admin route is not guarded, and the service key below would prove nothing", gtrWhy(status, body))
	}
	forged := gtrServiceRoleKey(config.Secret(randomHex(t, 48)), time.Now())
	if status, body = e.call(http.MethodDelete, "/admin/users/"+bid, forged, nil); !gtrRefusal(status) {
		t.Fatalf("control: a service-role key signed with ANOTHER secret on DELETE /admin/users/{id} got %s, want a refusal", gtrWhy(status, body))
	}
	if !e.authRow(bid).Found {
		t.Fatalf("control: the bystander's auth.users row is gone after two refused deletes")
	}
	if status, body = e.call(http.MethodGet, "/admin/users/"+vid, svc, nil); status != http.StatusOK {
		t.Fatalf("control: GET /admin/users/{id} with the service-role key minted from the harness secret got %s, want 200: the minted key is not accepted, so the delete below cannot be tried", gtrWhy(status, body))
	}

	// the delete
	status, body = e.call(http.MethodDelete, "/admin/users/"+vid, svc, map[string]bool{"should_soft_delete": false})
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("DELETE /admin/users/{id} with the service-role key: %s", gtrWhy(status, body))
	}
	delStatus := status
	if e.authRow(vid).Found {
		t.Fatalf("GoTrue answered the delete with HTTP %d but auth.users still holds the row", status)
	}

	// GoTrue refuses the old token ...
	ustatus, ubody := e.call(http.MethodGet, "/user", victim.Session.AccessToken, nil)
	if !gtrRefusal(ustatus) {
		t.Errorf("after the delete GoTrue's /user answered %s for the deleted user's token, want a refusal (401 or 403)", gtrWhy(ustatus, ubody))
	}
	// ... the verifier still accepts its signature ...
	c, err := e.v.Verify(e.ctx, victim.Session.AccessToken.Reveal())
	if err != nil || c.Subject != vid {
		t.Errorf("after the delete auth.Verifier answered %v (Subject %s) for the old token, want it to verify: a signature check is not a revocation check", err, gtrShort(c.Subject))
	}
	// ... and the platform's own check refuses the login (no platform user was ever made for it, and none is made now)
	if _, err := res.ByAuthUser(e.ctx, vid); !errors.Is(err, tenant.ErrUnknownLogin) {
		t.Errorf("ByAuthUser(the deleted login) = %v, want tenant.ErrUnknownLogin", err)
	}
	// the bystander is untouched
	if !e.authRow(bid).Found {
		t.Errorf("the delete of one login removed another's auth.users row")
	}
	if status, body = e.call(http.MethodGet, "/user", bystander.Session.AccessToken, nil); status != http.StatusOK {
		t.Errorf("control: GoTrue's /user stopped serving the bystander's token after the other login was deleted: %s", gtrWhy(status, body))
	}
	t.Logf("NOTE revocation: DELETE /admin/users/{id} (service-role key minted from the run's secret) answered HTTP %d and removed the auth.users row; GoTrue's /user then answered HTTP %d (%s) for the old token, while auth.Verifier still verified it (Subject %s): "+
		"a signature check is not a revocation check, the documented limit of stateless verification; the platform's public.users.is_active (TestGoTrue_InactiveUserIsRefusedByTheResolverNotByTheToken) and the resolver, which reads the table on every call, are the revocation levers",
		delStatus, ustatus, gtrErrorCode(ubody), gtrShort(c.Subject))
}
