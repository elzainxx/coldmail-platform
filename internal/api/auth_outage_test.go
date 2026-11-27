package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// A token that is bad is the one 401; a platform that cannot get the keys to judge a token is the 503 `auth_unavailable` and not a 401 that blames the user. These tests run the REAL
// verifier against a key-set server they break in every way a key set can fail (a closed port, a 500, a hang past the fetch timeout, garbage, an empty set), with the same token against a
// healthy key set as the control of each, and hold the 503 to its contract: the envelope, `Retry-After`, no cause, one warn line per stretch of unavailability.

// outageClock is the clock the verifier and the tokens share, which a test moves instead of sleeping.
type outageClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *outageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *outageClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// The modes of the key-set server.
const (
	jwksOK      = "ok"
	jwks500     = "500"
	jwksGarbage = "garbage"
	jwksEmpty   = "empty set"
	jwksHang    = "hang"
)

// outageJWKS is a key-set endpoint with a mode a test switches, and a count of what was asked of it.
type outageJWKS struct {
	*httptest.Server
	mu   sync.Mutex
	mode string
	doc  []byte
	hits atomic.Int32
}

func newOutageJWKS(t *testing.T, doc []byte, mode string) *outageJWKS {
	t.Helper()
	s := &outageJWKS{doc: doc, mode: mode}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		mode := s.mode
		s.mu.Unlock()
		switch mode {
		case jwks500:
			http.Error(w, "boom", http.StatusInternalServerError)
		case jwksGarbage:
			_, _ = w.Write([]byte("<html>down for maintenance</html>"))
		case jwksEmpty:
			_, _ = w.Write([]byte(`{"keys":[]}`))
		case jwksHang:
			select { // never answers: the verifier's fetch timeout ends the request, which ends this one
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(s.doc)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *outageJWKS) setMode(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = m
}

func (s *outageJWKS) url() string { return s.URL + "/auth/v1/.well-known/jwks.json" }

// closedPortURL is the key-set URL of a port nothing listens on.
func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr + "/auth/v1/.well-known/jwks.json"
}

// outageKeys are the keys of one test (made at run time) and their clock: an ES256 minter whose key set the server publishes, and an HS256 minter for the secret.
type outageKeys struct {
	clk *outageClock
	es  *authtest.Minter
	hs  *authtest.Minter
}

func newOutageKeys(t *testing.T) *outageKeys {
	t.Helper()
	k := &outageKeys{clk: &outageClock{t: t0}, es: authtest.NewES256(t), hs: authtest.NewHS256(t)}
	k.es.Now, k.hs.Now = k.clk.Now, k.clk.Now
	return k
}

// verifier is the real verifier: the HS256 secret when withSecret, the key set at jwksURL when it is not empty, the default 30 s refresh limit and a 200 ms fetch timeout.
func (k *outageKeys) verifier(t *testing.T, jwksURL string, withSecret bool) *auth.Verifier {
	t.Helper()
	o := auth.Options{Issuer: authtest.Issuer, Now: k.clk.Now, JWKSURL: jwksURL, AllowInsecureHTTP: true, HTTPClient: &http.Client{Timeout: 200 * time.Millisecond}}
	if withSecret {
		o.HMACSecret = []byte(k.hs.Secret())
	}
	v, err := auth.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// unknownKid is a token the ES256 key signed under a kid the key set does not hold.
func (k *outageKeys) unknownKid(c authtest.Claims) string {
	return k.es.Header(map[string]any{"alg": "ES256", "typ": "JWT", "kid": "rotated-away"}, c)
}

// noKid is a token the ES256 key signed with no kid in its header.
func (k *outageKeys) noKid(c authtest.Claims) string {
	return k.es.Header(map[string]any{"alg": "ES256", "typ": "JWT"}, c)
}

// outageRig is the job API over a verifier, the real handler, a store that records its calls and a log that keeps its lines.
type outageRig struct {
	t     *testing.T
	srv   *httptest.Server
	store *fakeStore
	logs  *syncBuf
}

func newOutageRig(t *testing.T, v Verifier) *outageRig {
	t.Helper()
	r := &outageRig{t: t, store: newFakeStore(), logs: &syncBuf{}}
	res := &fakeResolver{users: map[string]tenant.Tenant{loginA: {UserID: userA, AuthUserID: loginA, Active: true}}, errs: map[string]error{}}
	log, err := logging.New("debug", r.logs)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Deps{Service: r.store, Verifier: v, Resolver: res, Logger: log, Settings: newFakeSettings(), Account: newFakeAccount(), Suppressions: newFakeSuppress(), M3Accounts: newFakeM3()}) // the settings, account, suppression and module 3 routes are behind the same authentication: the test walks every route
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Logger: log, Pinger: pingOK{}, ReadyTimeout: time.Second, Now: func() time.Time { return t0 }, Mount: a.Mount}))
	t.Cleanup(r.srv.Close)
	return r
}

// send makes the request with the token as a bearer ("" sends none).
func (r *outageRig) send(method, path, token string) (*http.Response, []byte) {
	r.t.Helper()
	hr, err := http.NewRequest(method, r.srv.URL+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(hr)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp, b
}

// get is the list route with the token.
func (r *outageRig) get(token string) (*http.Response, []byte) {
	return r.send(http.MethodGet, "/v1/jobs", token)
}

// warnLines are the lines the stretch-of-unavailability warning wrote, parsed.
func (r *outageRig) warnLines() []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, l := range r.logs.lines(r.t) {
		if l["msg"] == msgKeysUnavailable {
			out = append(out, l)
		}
	}
	return out
}

// envelopeKeys decodes an error body and returns its error object.
func envelopeKeys(t *testing.T, what string, body []byte) map[string]any {
	t.Helper()
	var raw map[string]map[string]any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 1 || raw["error"] == nil {
		t.Fatalf("%s: the body is not an error envelope: %q", what, body)
	}
	return raw["error"]
}

// want503 holds an answer to the contract of the 503 `auth_unavailable`: the envelope with the fixed message and no details, `Retry-After` in whole seconds, no challenge, no cache,
// and nothing of the cause in the body: not the key set's address, not a key id, not the word http.
func want503(t *testing.T, what string, resp *http.Response, body []byte, forbid ...string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%s: status %d, want 503: %s", what, resp.StatusCode, body)
	}
	e := envelopeKeys(t, what, body)
	if len(e) != 3 || e["code"] != "auth_unavailable" || e["message"] != messages[codeAuthUnavailable] || e["request_id"] != resp.Header.Get("X-Request-Id") || e["request_id"] == "" {
		t.Errorf("%s: the envelope is %v, want exactly code, message and request_id with the fixed message", what, e)
	}
	if ra := resp.Header.Get("Retry-After"); ra != authRetryAfter {
		t.Errorf("%s: Retry-After %q, want %q", what, ra, authRetryAfter)
	}
	if n, err := strconv.Atoi(authRetryAfter); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After %q is not a small whole number of seconds", authRetryAfter)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("%s: WWW-Authenticate %q on a 503: nothing was refused, so there is nothing to challenge", what, got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control %q", what, got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("%s: Content-Type %q", what, ct)
	}
	lower := strings.ToLower(string(body))
	for _, bad := range append([]string{"http", "127.0.0.1", "localhost", ".well-known", "jwks", strings.ToLower(authtest.KeyID), "rotated-away", "timeout", "refused", "connection"}, forbid...) {
		if bad != "" && strings.Contains(lower, strings.ToLower(bad)) {
			t.Errorf("%s: the body names %q: %s", what, bad, body)
		}
	}
}

// want401 holds an answer to the one 401 and returns its comparable form: status, the headers that matter and the body with its request id blanked.
func want401(t *testing.T, what string, resp *http.Response, body []byte) string {
	t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%s: status %d, want 401: %s", what, resp.StatusCode, body)
	}
	e := envelopeKeys(t, what, body)
	if len(e) != 3 || e["code"] != "unauthenticated" || e["message"] != messages[codeUnauthenticated] {
		t.Errorf("%s: the envelope is %v", what, e)
	}
	return sameAnswer(resp, body)
}

// sameAnswer is what two answers that must be identical must share: the status, the headers a client acts on, and the body but for its request id.
func sameAnswer(resp *http.Response, body []byte) string {
	return fmt.Sprintf("%d|WWW-Authenticate=%q|Content-Type=%q|Cache-Control=%q|Retry-After=%q|Content-Length=%q|%s", resp.StatusCode, resp.Header.Get("WWW-Authenticate"),
		resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"), resp.Header.Get("Retry-After"), resp.Header.Get("Content-Length"), sameBody(body))
}

func TestAuthOutage_EveryKindOfOutageIs503AndTheSameTokenIsServedWhenTheKeysAreUp(t *testing.T) {
	cases := []struct {
		name  string
		mode  string // "" is a closed port
		class string
	}{
		{"a closed port", "", "unreachable"},
		{"a 500", jwks500, "bad_status"},
		{"a server that hangs past the fetch timeout", jwksHang, "timeout"},
		{"garbage instead of a key set", jwksGarbage, "bad_key_set"},
		{"an empty key set", jwksEmpty, "bad_key_set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newOutageKeys(t)
			doc := k.es.JWKS(t)
			up := newOutageJWKS(t, doc, jwksOK)
			downURL, forbid := closedPortURL(t), []string(nil)
			if c.mode != "" {
				down := newOutageJWKS(t, doc, c.mode)
				downURL = down.url()
			}
			forbid = append(forbid, strings.TrimPrefix(strings.TrimSuffix(downURL, "/auth/v1/.well-known/jwks.json"), "http://"))
			healthy := newOutageRig(t, k.verifier(t, up.url(), true))
			broken := newOutageRig(t, k.verifier(t, downURL, true))
			tok := k.es.Token(loginA)

			// the control: the very token is served by the verifier whose key set works
			resp, body := healthy.get(tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("control failed: the token against a healthy key set was answered %d: %s", resp.StatusCode, body)
			}
			// the outage: not a 401
			start := time.Now()
			resp, body = broken.get(tok)
			want503(t, c.name, resp, body, forbid...)
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("%s: the answer took %v: the fetch timeout did not bound it", c.name, d)
			}
			if n := len(broken.store.seen()); n != 0 {
				t.Errorf("%s: the store was called %d times for a token that could not be judged", c.name, n)
			}

			// every route is behind it: a 503 for the broken verifier, and for the healthy one anything but a 401 or a 503 (the control that the route itself is not what answers)
			for _, rt := range routes {
				path := concretePath(rt.pattern)
				resp, body := broken.send(rt.method, path, tok)
				want503(t, rt.method+" "+rt.pattern, resp, body, forbid...)
				if resp, body := healthy.send(rt.method, path, tok); resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusServiceUnavailable {
					t.Errorf("control failed: %s %s with a good token against a healthy key set: %d %s", rt.method, rt.pattern, resp.StatusCode, body)
				}
			}
			if n := len(broken.store.seen()); n != 0 {
				t.Errorf("%s: a route reached the store (%d calls) with the key set down", c.name, n)
			}

			// the log: one line for the whole stretch, whatever the number of requests, carrying the word and the class and nothing else about the cause
			lines := broken.warnLines()
			if len(lines) != 1 {
				t.Fatalf("%s: %d warn lines over %d requests, want 1", c.name, len(lines), 1+len(routes))
			}
			l := lines[0]
			raw, _ := json.Marshal(l)
			if l["level"] != "WARN" || l["reason"] != whyKeysUnavailable || l["class"] != c.class || l["request_id"] == nil || len(l) != 6 {
				t.Errorf("%s: the warn line is %s, want level WARN, reason %q, class %q, a request id and nothing else (time, level, msg)", c.name, raw, whyKeysUnavailable, c.class)
			}
			for _, bad := range append([]string{"http", "127.0.0.1", ".well-known", authtest.KeyID, "rotated-away"}, forbid...) {
				if strings.Contains(string(raw), bad) {
					t.Errorf("%s: the warn line names %q: %s", c.name, bad, raw)
				}
			}
		})
	}
	if len(cases) < 5 {
		t.Fatalf("control failed: %d outage kinds", len(cases))
	}
}

func TestAuthOutage_ABadTokenIsTheSame401WhetherTheKeysAreUpOrDownAndOnlyATokenThatNeedsAKeyIs503(t *testing.T) {
	k := newOutageKeys(t)
	doc := k.es.JWKS(t)
	up, down := newOutageJWKS(t, doc, jwksOK), newOutageJWKS(t, doc, jwks500)
	healthy := newOutageRig(t, k.verifier(t, up.url(), true))
	broken := newOutageRig(t, k.verifier(t, down.url(), true))

	good := func() authtest.Claims { return k.es.Good(loginA) }
	now := k.clk.Now().Unix()
	// bad without any key: no key could make them good, so the key set's state must not change their answer
	noKeyNeeded := []struct{ name, tok string }{
		{"not a token", "not-a-jwt"},
		{"two segments", "aaaa.bbbb"},
		{"alg none", k.es.Unsigned(good())},
		{"an algorithm outside the allow-list", k.es.Header(map[string]any{"alg": "HS384", "typ": "JWT", "kid": authtest.KeyID}, good())},
		{"expired", k.es.Mint(good().With("exp", now-3600))},
		{"not yet valid", k.es.Mint(good().With("nbf", now+3600))},
		{"the wrong audience", k.es.Mint(good().With("aud", "someone-else"))},
		{"the wrong issuer", k.es.Mint(good().With("iss", "https://evil.example.test/auth/v1"))},
		{"a subject that is not a uuid", k.es.Mint(good().With("sub", "alice"))},
		{"expired, under a kid nobody has", k.unknownKid(good().With("exp", now-3600))},
	}
	// bad, but only a key can say so
	keyNeeded := []struct{ name, tok string }{
		{"a forged signature", k.es.Forged(good())},
		{"a tampered payload", k.es.Tampered(k.es.Token(loginB), good())},
		{"a kid the key set does not hold", k.unknownKid(good())},
	}

	var reference string
	for _, c := range append(append([]struct{ name, tok string }(nil), noKeyNeeded...), keyNeeded...) {
		resp, body := healthy.get(c.tok)
		got := want401(t, "healthy, "+c.name, resp, body)
		if reference == "" {
			reference = got
		} else if got != reference {
			t.Errorf("healthy, %s: the 401 differs from the others:\n  %s\n  %s", c.name, got, reference)
		}
	}
	if !strings.Contains(reference, `WWW-Authenticate="Bearer"`) || !strings.Contains(reference, `Retry-After=""`) {
		t.Fatalf("the reference answer is not the 401: %s", reference)
	}

	// the key set down: the first group is the very same answer, byte for byte; the second is the platform's
	for _, c := range noKeyNeeded {
		resp, body := broken.get(c.tok)
		if got := want401(t, "down, "+c.name, resp, body); got != reference {
			t.Errorf("down, %s: not the identical 401:\n  %s\n  %s", c.name, got, reference)
		}
	}
	var unavailable string
	for _, c := range keyNeeded {
		resp, body := broken.get(c.tok)
		want503(t, "down, "+c.name, resp, body)
		unavailable = sameAnswer(resp, body)
	}
	// the controls: the comparison can tell a 503 from the 401, and the legal twin of every token above is served by the healthy verifier
	if unavailable == reference || unavailable == "" {
		t.Fatalf("control failed: the 503 compares equal to the 401: %s", unavailable)
	}
	if resp, body := healthy.get(k.es.Token(loginA)); resp.StatusCode != http.StatusOK {
		t.Fatalf("control failed: a legal token against the healthy key set: %d %s", resp.StatusCode, body)
	}
	if n := len(broken.store.seen()) + len(healthy.store.seen()); n != 1 {
		t.Errorf("the store was called %d times, want only the legal twin's one call", n)
	}
	if len(noKeyNeeded) < 10 || len(keyNeeded) < 3 {
		t.Fatalf("control failed: %d and %d cases", len(noKeyNeeded), len(keyNeeded))
	}
}

func TestAuthOutage_AnHS256TokenIsJudgedByItsSecretWhileTheKeySetIsDown(t *testing.T) {
	k := newOutageKeys(t)
	doc := k.es.JWKS(t)
	up, down := newOutageJWKS(t, doc, jwksOK), newOutageJWKS(t, doc, jwks500)
	healthy := newOutageRig(t, k.verifier(t, up.url(), true))
	broken := newOutageRig(t, k.verifier(t, down.url(), true))

	// a reference 401 from the healthy side
	resp, body := healthy.get(k.hs.Forged(k.hs.Good(loginA)))
	reference := want401(t, "healthy, a forged HS256 token", resp, body)

	// the secret decides, and the key set is never asked
	resp, body = broken.get(k.hs.Token(loginA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid HS256 token with the key set down: %d %s", resp.StatusCode, body)
	}
	resp, body = broken.get(k.hs.Forged(k.hs.Good(loginA)))
	if got := want401(t, "down, a forged HS256 token", resp, body); got != reference {
		t.Errorf("a forged HS256 token with the key set down is not the identical 401:\n  %s\n  %s", got, reference)
	}
	resp, body = broken.get(k.hs.Mint(k.hs.Good(loginA).With("exp", k.clk.Now().Unix()-3600)))
	if got := want401(t, "down, an expired HS256 token", resp, body); got != reference {
		t.Errorf("an expired HS256 token with the key set down is not the identical 401")
	}
	if n := down.hits.Load(); n != 0 {
		t.Errorf("the key set was asked %d times for HS256 tokens, want 0", n)
	}
	if len(broken.warnLines()) != 0 {
		t.Errorf("an outage was reported although no request needed the key set")
	}

	// an asymmetric token that names a key the platform cannot look up is the platform's to answer: 503, NOT 401
	for name, tok := range map[string]string{
		"an unknown kid": k.unknownKid(k.es.Good(loginA)),
		"no kid":         k.noKid(k.es.Good(loginA)),
		"the right kid":  k.es.Token(loginA),
	} {
		resp, body := broken.get(tok)
		want503(t, "down, "+name, resp, body)
	}
	// the controls: the same asymmetric tokens at the healthy key set are not an outage (the right kid is served, the unknown one is the token's fault)
	if resp, body := healthy.get(k.es.Token(loginA)); resp.StatusCode != http.StatusOK {
		t.Fatalf("control failed: the right kid, healthy: %d %s", resp.StatusCode, body)
	}
	resp, body = healthy.get(k.unknownKid(k.es.Good(loginA)))
	if got := want401(t, "healthy, an unknown kid", resp, body); got != reference {
		t.Errorf("an unknown kid against a healthy key set is not the identical 401")
	}
}

func TestAuthOutage_AnHS256OnlyDeploymentNeverAnswers503(t *testing.T) {
	k := newOutageKeys(t)
	rig := newOutageRig(t, k.verifier(t, "", true))
	forgedHS := k.hs.Forged(k.hs.Good(loginA))

	resp, body := rig.get(forgedHS)
	reference := want401(t, "a forged HS256 token", resp, body)
	if resp, body := rig.get(k.hs.Token(loginA)); resp.StatusCode != http.StatusOK {
		t.Fatalf("control failed: a valid HS256 token: %d %s", resp.StatusCode, body)
	}
	for name, tok := range map[string]string{
		"an ES256 token with a kid":      k.es.Token(loginA),
		"an ES256 token under no kid":    k.noKid(k.es.Good(loginA)),
		"an ES256 token under a new kid": k.unknownKid(k.es.Good(loginA)),
		"alg none":                       k.hs.Unsigned(k.hs.Good(loginA)),
		"not a token":                    "not-a-jwt",
		"a tampered HS256 token":         k.hs.Tampered(k.hs.Token(loginB), k.hs.Good(loginA)),
	} {
		resp, body := rig.get(tok)
		if got := want401(t, name, resp, body); got != reference {
			t.Errorf("%s: not the identical 401", name)
		}
	}
	if n := len(rig.warnLines()); n != 0 {
		t.Errorf("an HS256-only deployment reported %d outages", n)
	}

	// the control that the loop above can fail: a deployment whose only key source is a key set that cannot be reached answers the same ES256 token 503
	jwksOnly := newOutageRig(t, k.verifier(t, closedPortURL(t), false))
	resp, body = jwksOnly.get(k.es.Token(loginA))
	want503(t, "the control, a JWKS-only deployment whose key set is gone", resp, body)
}

func TestAuthOutage_TheWarningIsOneLinePerStretchAndAgainAfterARecovery(t *testing.T) {
	k := newOutageKeys(t)
	srv := newOutageJWKS(t, k.es.JWKS(t), jwks500)
	rig := newOutageRig(t, k.verifier(t, srv.url(), true))

	// The tokens of an outage that need the key set. Before the key set was ever fetched all three do (the right kid, an unknown one, none). Once it was loaded only the unknown kid does:
	// a key the loaded set holds still verifies while the issuer is down (a stale key beats none), which the loaded outages assert in passing, and a set of one key serves a token with no kid.
	needs := func(i int, loaded bool) string {
		if loaded {
			return k.unknownKid(k.es.Good(loginA))
		}
		switch i % 3 {
		case 0:
			return k.es.Token(loginA)
		case 1:
			return k.unknownKid(k.es.Good(loginA))
		}
		return k.noKid(k.es.Good(loginA))
	}
	outage := func(what string, requests int, loaded bool) (answered int) {
		t.Helper()
		before := srv.hits.Load()
		for i := 0; i < requests; i++ {
			if i%10 == 9 {
				k.clk.Advance(31 * time.Second) // past the refresh limit: another fetch, another failure, inside the same stretch
			}
			resp, body := rig.get(needs(i, loaded))
			want503(t, fmt.Sprintf("%s, request %d", what, i), resp, body)
			answered++
			if loaded && i%5 == 0 {
				if resp, body := rig.get(k.es.Token(loginA)); resp.StatusCode != http.StatusOK {
					t.Fatalf("%s: a key the loaded key set holds was refused while the issuer is down: %d %s", what, resp.StatusCode, body)
				}
			}
		}
		if fetches := srv.hits.Load() - before; requests >= 30 && fetches < 3 {
			t.Fatalf("control failed: %s made %d failed fetches, the test meant several inside one stretch", what, fetches)
		}
		return answered
	}
	recover := func(what string) {
		t.Helper()
		srv.setMode(jwksOK)
		k.clk.Advance(31 * time.Second)
		// a token that needs the key set: the fetch works, the set does not hold the kid, and it is the token's fault again (401) where a moment ago it was the platform's
		resp, body := rig.get(k.unknownKid(k.es.Good(loginA)))
		want401(t, what+": an unknown kid, the key set up", resp, body)
		if resp, body := rig.get(k.es.Token(loginA)); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: the right kid, the key set up: %d %s", what, resp.StatusCode, body)
		}
	}
	lines := func(want int, classes ...string) {
		t.Helper()
		got := rig.warnLines()
		if len(got) != want {
			t.Fatalf("%d warn lines, want %d: %v", len(got), want, got)
		}
		for i, c := range classes {
			if got[i]["class"] != c || got[i]["reason"] != whyKeysUnavailable || got[i]["level"] != "WARN" {
				t.Errorf("warn line %d is %v, want class %q", i+1, got[i], c)
			}
		}
	}

	// the first outage: 40 requests, several failed fetches, ONE line
	lines(0)
	served := outage("the first outage", 40, false)
	lines(1, "bad_status")

	// the key set comes back; bad tokens in a healthy period write nothing of this kind
	recover("the first recovery")
	for i := 0; i < 10; i++ {
		rig.get(k.es.Forged(k.es.Good(loginA)))
	}
	lines(1, "bad_status")

	// the second outage is a new stretch, whose first request writes the next line, and no other does
	srv.setMode(jwksGarbage)
	k.clk.Advance(31 * time.Second)
	served += outage("the second outage", 30, true)
	lines(2, "bad_status", "bad_key_set")

	// and a third, of another kind
	recover("the second recovery")
	srv.setMode(jwksHang)
	k.clk.Advance(31 * time.Second)
	served += outage("the third outage", 12, true)
	lines(3, "bad_status", "bad_key_set", "timeout")

	// the instrument: every line was written under a request id that is one of the answers' (not a made-up line), and the count of 503s is far past the count of lines
	if served < 80 {
		t.Fatalf("control failed: only %d requests were answered 503", served)
	}
	rids := map[string]bool{}
	for _, l := range rig.logs.lines(t) {
		if l["msg"] == "http_request" && l["status"] == float64(503) {
			rids[fmt.Sprint(l["request_id"])] = true
		}
	}
	for _, l := range rig.warnLines() {
		if !rids[fmt.Sprint(l["request_id"])] {
			t.Errorf("the warn line %v belongs to no 503 request", l)
		}
	}
	if len(rids) != served {
		t.Errorf("the access log holds %d 503 requests, the test sent %d", len(rids), served)
	}
}

// fakeVerifier answers every token with its error: the API tells the failures apart by their type, and not by anything the real verifier does.
type fakeVerifier struct{ err error }

func (f fakeVerifier) Verify(context.Context, string) (auth.Claims, error) {
	return auth.Claims{}, f.err
}

func TestAuthOutage_TheAPITellsTheSentinelsApartByTypeAndNotByText(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  int
		lines int // warn lines after three requests
	}{
		{"ErrKeysUnavailable bare", auth.ErrKeysUnavailable, 503, 3}, // no stretch: a line repeated is better than a line lost
		{"ErrKeysUnavailable wrapped", fmt.Errorf("verify: %w", auth.ErrKeysUnavailable), 503, 3},
		{"ErrKey", auth.ErrKey, 401, 0},
		{"ErrKey wrapped", fmt.Errorf("verify: %w", auth.ErrKey), 401, 0},
		{"ErrSignature", auth.ErrSignature, 401, 0},
		{"ErrExpired", auth.ErrExpired, 401, 0},
		{"an error nobody knows, whose text says keys are unavailable", errors.New("auth: the signing keys cannot be obtained"), 401, 0},
	}
	var reference string
	for _, c := range cases {
		rig := newOutageRig(t, fakeVerifier{c.err})
		var resp *http.Response
		var body []byte
		for i := 0; i < 3; i++ {
			resp, body = rig.get("any.token.value")
		}
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, resp.StatusCode, c.want, body)
			continue
		}
		if c.want == 503 {
			want503(t, c.name, resp, body)
		} else if got := want401(t, c.name, resp, body); reference == "" {
			reference = got
		} else if got != reference {
			t.Errorf("%s: not the identical 401", c.name)
		}
		lines := rig.warnLines()
		if len(lines) != c.lines {
			t.Errorf("%s: %d warn lines, want %d", c.name, len(lines), c.lines)
		}
		for _, l := range lines {
			if l["class"] != "unknown" {
				t.Errorf("%s: class %v, want %q (a verifier that names none)", c.name, l["class"], "unknown")
			}
		}
		if len(rig.store.seen()) != 0 {
			t.Errorf("%s: the store was called", c.name)
		}
	}
}

func TestAuthOutage_TheRetryAfterIsTheRefreshIntervalInWholeSeconds(t *testing.T) {
	if want := strconv.Itoa(int(auth.DefaultMinRefreshInterval / time.Second)); authRetryAfter != want || authRetryAfter != "30" {
		t.Errorf("Retry-After is %q, want %q (the key set is fetched at most every %v, and Docs/jobs-api.md says 30)", authRetryAfter, want, auth.DefaultMinRefreshInterval)
	}
	// the class words are the verifier's six, each mapped to a word of the API's own
	for _, c := range []string{auth.ClassUnreachable, auth.ClassTimeout, auth.ClassCertificate, auth.ClassStatus, auth.ClassUnreadable, auth.ClassBadKeySet} {
		if keysClassWords[c] == "" {
			t.Errorf("the class %q has no log word", c)
		}
	}
	if len(keysClassWords) != 6 {
		t.Errorf("%d class words, want the verifier's six", len(keysClassWords))
	}
}
