package auth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A token that is bad and a platform that cannot get its keys are different failures (ErrKey and the other sentinels against ErrKeysUnavailable). These tests hold the line between
// them: every way the key set can fail is ErrKeysUnavailable, each paired with the same token accepted by a verifier whose key set is healthy; a token that is bad stays its own sentinel
// with the key set down; HS256 never notices the key set; and a stretch of unavailability is told apart from the next one.

// outageWorld is a verifier with both key sources whose key set is served by srv (the one the test breaks), and a twin whose key set is served by good (never broken), over one key and
// one clock.
type outageWorld struct {
	clk    *testClock
	secret []byte
	ec     *ecdsa.PrivateKey
	doc    []byte
	good   *jwksServer
	srv    *jwksServer
	down   *Verifier // HS256 and the key set of srv
	up     *Verifier // HS256 and the key set of good
}

func newOutageWorld(t *testing.T) *outageWorld {
	t.Helper()
	w := &outageWorld{clk: newClock(), secret: randBytes(t, 48), ec: ecKey(t)}
	w.doc = jwksDoc(t, ecJWK(&w.ec.PublicKey, ecKID))
	w.good, w.srv = newJWKSServer(t, w.doc), newJWKSServer(t, w.doc)
	mk := func(url string) *Verifier {
		return mustNew(t, Options{HMACSecret: w.secret, JWKSURL: url, AllowInsecureHTTP: true, Issuer: testIssuer, Now: w.clk.Now, HTTPClient: &http.Client{Timeout: 200 * time.Millisecond}})
	}
	w.down, w.up = mk(w.srv.url()), mk(w.good.url())
	return w
}

// es is an ES256 token under the world's key, with the kid and claims given.
func (w *outageWorld) es(t *testing.T, kid string, claims map[string]any) string {
	return mint(t, ecSigner{t, w.ec}, kid, claims)
}

// hs is an HS256 token under the world's secret.
func (w *outageWorld) hs(t *testing.T, claims map[string]any) string {
	return mint(t, hmacSigner{w.secret}, "", claims)
}

// keysUnavailable returns the typed error of err, failing the test when err is not one, and checks the two things every one of them must be: not an ErrKey, and free of what an outage
// must never name (the key set's address, the kid asked for, the word http).
func keysUnavailable(t *testing.T, what string, err error, w *outageWorld, kid string) *KeysUnavailableError {
	t.Helper()
	var ku *KeysUnavailableError
	if !errors.As(err, &ku) || !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("%s: got %v, want a *KeysUnavailableError", what, err)
	}
	if errors.Is(err, ErrKey) {
		t.Errorf("%s: the error is also an ErrKey: a token verdict and a platform verdict are one error", what)
	}
	host := strings.TrimPrefix(w.srv.URL, "http://")
	for _, text := range []string{err.Error(), fmt.Sprintf("%v|%+v|%#v|%q|%s", err, err, err, err, err)} {
		for _, name := range []string{host, w.srv.URL, "127.0.0.1", kid, "http", ".well-known"} {
			if name != "" && strings.Contains(text, name) {
				t.Errorf("%s: the error names %q: %s", what, name, text)
			}
		}
	}
	return ku
}

func TestOutage_EveryWayTheKeySetCannotBeHadIsErrKeysUnavailableAndTheSameTokenVerifiesWhenItCan(t *testing.T) {
	huge := func(w *outageWorld) []byte {
		return append(append([]byte(nil), w.doc...), []byte(strings.Repeat(" ", MaxKeySetBytes))...)
	}
	modes := []struct {
		name    string
		class   string
		arrange func(w *outageWorld) (restore func()) // breaks srv; restore mends it (nil when it cannot be mended: the port is gone)
	}{
		{"connection refused (a closed port)", ClassUnreachable, func(w *outageWorld) func() { w.srv.Close(); return nil }},
		{"a 500", ClassStatus, func(w *outageWorld) func() {
			w.srv.answer(500, []byte("boom"))
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a 503 with no body", ClassStatus, func(w *outageWorld) func() {
			w.srv.answer(503, nil)
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a 404", ClassStatus, func(w *outageWorld) func() {
			w.srv.answer(404, []byte("nope"))
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a redirect that is not followed", ClassStatus, func(w *outageWorld) func() {
			w.srv.answer(302, nil)
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a server that does not answer within the fetch timeout", ClassTimeout, func(w *outageWorld) func() { return w.srv.hold() }},
		{"garbage that is not JSON", ClassBadKeySet, func(w *outageWorld) func() {
			w.srv.answer(200, []byte("<html>maintenance</html>"))
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"an empty body", ClassBadKeySet, func(w *outageWorld) func() {
			w.srv.answer(200, nil)
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"an empty key set", ClassBadKeySet, func(w *outageWorld) func() {
			w.srv.answer(200, []byte(`{"keys":[]}`))
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a key set with no usable key", ClassBadKeySet, func(w *outageWorld) func() {
			w.srv.answer(200, []byte(`{"keys":[{"kty":"oct"}]}`))
			return func() { w.srv.answer(200, w.doc) }
		}},
		{"a key set over 1 MiB", ClassUnreadable, func(w *outageWorld) func() {
			w.srv.answer(200, huge(w))
			return func() { w.srv.answer(200, w.doc) }
		}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			w := newOutageWorld(t)
			tok := w.es(t, ecKID, claimsAt(w.clk.Now()))

			// The control: the very token is accepted when the key set can be had, so what follows is the outage's doing and not the token's.
			if _, err := verify(w.up, tok); err != nil {
				t.Fatalf("control failed: the token is refused by the verifier whose key set is healthy: %v", err)
			}
			restore := m.arrange(w)
			t.Cleanup(func() { // a held server waits for its gate: it must be opened even when the test stops early
				if restore != nil {
					restore()
				}
			})
			start := time.Now()
			_, err := verify(w.down, tok)
			wantErr(t, m.name, err, ErrKeysUnavailable)
			ku := keysUnavailable(t, m.name, err, w, ecKID)
			if ku.Class() != m.class {
				t.Errorf("%s: class %q, want %q", m.name, ku.Class(), m.class)
			}
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("%s: the answer took %v: the fetch timeout did not bound it", m.name, d)
			}

			// A second request inside the refresh interval is the same answer and makes no new fetch: an outage is not hammered, and a hanging endpoint stalls one request, not every one.
			hits := w.srv.hits.Load()
			start = time.Now()
			_, err = verify(w.down, tok)
			wantErr(t, m.name+": again, inside the interval", err, ErrKeysUnavailable)
			if got := w.srv.hits.Load(); got != hits || time.Since(start) > 150*time.Millisecond {
				t.Errorf("%s: the second request made %d fetches (took %v), want none", m.name, got-hits, time.Since(start))
			}

			// What needs no key is not touched by the outage: an HS256 token is judged by its secret alone, and a forged one is its own refusal.
			if _, err := verify(w.down, w.hs(t, claimsAt(w.clk.Now()))); err != nil {
				t.Errorf("%s: a valid HS256 token was refused: %v", m.name, err)
			}
			_, err = verify(w.down, mint(t, hmacSigner{randBytes(t, 48)}, "", claimsAt(w.clk.Now())))
			wantErr(t, m.name+": a forged HS256 token", err, ErrSignature)

			// The same token once the key set can be had again, after the interval.
			if restore == nil {
				return
			}
			restore()
			w.clk.Advance(DefaultMinRefreshInterval)
			if _, err := verify(w.down, tok); err != nil {
				t.Errorf("%s: the token is still refused after the key set came back: %v", m.name, err)
			}
		})
	}
	if len(modes) < 10 {
		t.Fatalf("control failed: only %d kinds of outage were examined", len(modes))
	}
}

func TestOutage_AnUntrustedCertificateIsAnOutageToo(t *testing.T) {
	w := newOutageWorld(t)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(w.doc) }))
	defer secure.Close()
	tok := w.es(t, ecKID, claimsAt(w.clk.Now()))
	plain := mustNew(t, Options{JWKSURL: secure.URL, Issuer: testIssuer, Now: w.clk.Now})
	_, err := verify(plain, tok)
	wantErr(t, "an untrusted certificate", err, ErrKeysUnavailable)
	if ku := keysUnavailable(t, "an untrusted certificate", err, w, ecKID); ku.Class() != ClassCertificate {
		t.Errorf("class %q, want %q", ku.Class(), ClassCertificate)
	}
	// the twin: the same server, trusted
	trusting := mustNew(t, Options{JWKSURL: secure.URL, HTTPClient: secure.Client(), Issuer: testIssuer, Now: w.clk.Now})
	if _, err := verify(trusting, tok); err != nil {
		t.Errorf("control failed: the same server, trusted: %v", err)
	}
}

// What the key set's state does to a kid, step by step on one verifier. The unknown kid is the token's fault (ErrKey) exactly while the platform KNOWS the key set (its last fetch worked
// and the rate limit holds the next one back), and the platform's (ErrKeysUnavailable) as soon as it does not; a key it holds verifies throughout.
func TestOutage_AnUnknownKidIsTheTokensFaultOnlyWhileTheKeySetIsKnown(t *testing.T) {
	jw := newJWKSWorld(t, "k1")
	jw.add(t, "stranger", "ghost")
	healthyTwin := newJWKSWorld(t, "k1")
	healthyTwin.add(t, "stranger")

	jw.check(t, "the first token loads the key set", "k1", nil, 1)
	healthyTwin.check(t, "the twin loads its key set", "k1", nil, 1)

	// 1. the key set is known and fresh; the rate limit refuses a refresh: ErrKey, the user retries.
	jw.clk.Advance(5 * time.Second)
	jw.check(t, "known set, an unknown kid, the refresh refused by the rate limit", "stranger", ErrKey, 1)

	// 2. the issuer goes down and the interval passes: the refresh the unknown kid asks for FAILS. The platform cannot tell whether the key exists.
	jw.srv.answer(500, []byte("down"))
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "an unknown kid while the refresh fails", "stranger", ErrKeysUnavailable, 2)
	// the twin, whose issuer is up, calls the very same token what it is: the token is the one it cannot verify
	healthyTwin.clk.Advance(5*time.Second + DefaultMinRefreshInterval)
	healthyTwin.check(t, "control: the same unknown kid against a healthy issuer", "stranger", ErrKey, 2)

	// 3. the failed fetch holds the next one back, and the answer stays the platform's, not the token's, for the whole interval.
	jw.clk.Advance(10 * time.Second)
	jw.check(t, "an unknown kid inside the interval after a failed fetch", "stranger", ErrKeysUnavailable, 2)

	// 4. a key the set holds keeps verifying, with no fetch, while the issuer is down.
	jw.check(t, "a key the loaded set holds", "k1", nil, 2)

	// 5. the set outlives its lifetime with the issuer still down: the key it holds is served from it (a stale key beats none), the unknown kid is still the platform's.
	jw.clk.Advance(DefaultKeyTTL + time.Minute)
	jw.check(t, "past the lifetime, a key the stale set holds", "k1", nil, 3)
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "past the lifetime, an unknown kid", "ghost", ErrKeysUnavailable, 4)

	// 6. the issuer is back, and publishes one more key: the kid that was unknown is a key now, and a kid that is still not in the set is the token's fault again.
	jw.srv.answer(200, jw.doc(t, "k1", "stranger"))
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "after recovery, the kid the new set holds", "stranger", nil, 5)
	jw.check(t, "after recovery, a kid it does not hold (the set is known again)", "ghost", ErrKey, 5)
}

// An HS256 token is judged by its secret and never waits for, or is changed by, the key set. A token that names an asymmetric algorithm and a kid the platform cannot look up is the
// platform's to refuse, never a 401.
func TestOutage_HS256IsJudgedBySecretWhateverTheKeySetDoes(t *testing.T) {
	w := newOutageWorld(t)
	w.srv.answer(500, []byte("down"))
	now := w.clk.Now()
	other := randBytes(t, 48)

	for name, c := range map[string]struct {
		tok  string
		want error
	}{
		"a valid HS256 token":                  {w.hs(t, claimsAt(now)), nil},
		"an HS256 token under another secret":  {mint(t, hmacSigner{other}, "", claimsAt(now)), ErrSignature},
		"an expired HS256 token":               {w.hs(t, with(claimsAt(now), "exp", now.Unix()-3600)), ErrExpired},
		"an HS256 token for another audience":  {w.hs(t, with(claimsAt(now), "aud", "other")), ErrAudience},
		"an HS256 token that also names a kid": {mint(t, hmacSigner{w.secret}, "unknown-kid", claimsAt(now)), nil},
	} {
		_, err := verify(w.down, c.tok)
		wantErr(t, name+" with the key set down", err, c.want)
		if errors.Is(err, ErrKeysUnavailable) {
			t.Errorf("%s: the key set decided the verdict of an HS256 token", name)
		}
	}
	if got := w.srv.hits.Load(); got != 0 {
		t.Errorf("the key set was fetched %d times for HS256 tokens: it must not be asked at all", got)
	}

	// The asymmetric twins of the same user, the same key set down: the platform cannot judge them, whatever they name.
	unknown := w.es(t, "no-such-kid", claimsAt(now))
	noKid := w.es(t, "", claimsAt(now))
	known := w.es(t, ecKID, claimsAt(now))
	for name, tok := range map[string]string{"an unknown kid": unknown, "no kid": noKid, "the right kid": known} {
		_, err := verify(w.down, tok)
		wantErr(t, name+" with the key set down", err, ErrKeysUnavailable)
	}
	// ... and the same three at the healthy twin are not an outage at all: the right kid verifies, the other two are the token's fault.
	_, err := verify(w.up, known)
	wantErr(t, "the right kid, healthy", err, nil)
	_, err = verify(w.up, unknown)
	wantErr(t, "an unknown kid, healthy", err, ErrKey)
	_, err = verify(w.up, noKid)
	wantErr(t, "no kid, healthy (a set of one key serves it)", err, nil)

	// A verifier with no JWKS URL has no path to the platform's verdict at all, whatever a token says.
	hmacOnly := mustNew(t, Options{HMACSecret: w.secret, Issuer: testIssuer, Now: w.clk.Now})
	for name, tok := range map[string]string{
		"ES256 with a kid":         unknown,
		"ES256 with no kid":        noKid,
		"RS256 with a kid":         mint(t, rsaSigner{t, rsaKey(t, 0)}, rsaKID, claimsAt(now)),
		"HS256 valid":              w.hs(t, claimsAt(now)),
		"HS256 forged":             mint(t, hmacSigner{other}, "", claimsAt(now)),
		"alg none":                 encJSON(t, map[string]any{"alg": "none"}) + "." + encJSON(t, claimsAt(now)) + ".",
		"not a token":              "not-a-token",
		"a token with a bad claim": w.hs(t, with(claimsAt(now), "sub", "x")),
	} {
		_, err := verify(hmacOnly, tok)
		if errors.Is(err, ErrKeysUnavailable) {
			t.Errorf("an HS256-only verifier answered %q for %s", err, name)
		}
	}
	// The control for the loop above: a verifier whose key set IS the thing that is down does answer the platform's verdict for the same ES256 token.
	_, err = verify(w.down, unknown)
	wantErr(t, "control: the verifier with a key set that is down", err, ErrKeysUnavailable)
}

// The order. A token that is bad without any key is its own refusal with the key set down (and, for the first group, never makes the verifier ask for the key set); a token only a key
// could judge is the platform's; and with the key set healthy every one of them is what the documented order says.
func TestOutage_ABadTokenIsNeverTurnedIntoAnOutage(t *testing.T) {
	type tc struct {
		name      string
		mk        func(t *testing.T, w *outageWorld) string
		down      error // with the key set down
		downFetch int32 // fetches the down verifier made
		healthy   error // with the key set healthy
	}
	now := testNow
	sec := now.Unix()
	cases := []tc{
		// no key could make these good, and none of them needs the key set
		{"an empty token", func(t *testing.T, w *outageWorld) string { return "" }, ErrMalformed, 0, ErrMalformed},
		{"not a token", func(t *testing.T, w *outageWorld) string { return "not-a-token" }, ErrMalformed, 0, ErrMalformed},
		{"two segments", func(t *testing.T, w *outageWorld) string { return "aaaa.bbbb" }, ErrMalformed, 0, ErrMalformed},
		{"a payload that is not an object", func(t *testing.T, w *outageWorld) string {
			return enc([]byte(`{"alg":"ES256"}`)) + "." + enc([]byte(`[1]`)) + "." + enc([]byte("sig"))
		}, ErrMalformed, 0, ErrMalformed},
		{"alg none", func(t *testing.T, w *outageWorld) string {
			return encJSON(t, map[string]any{"alg": "none", "typ": "JWT", "kid": ecKID}) + "." + encJSON(t, claimsAt(now)) + "."
		}, ErrAlgorithm, 0, ErrAlgorithm},
		{"alg None with a signature", func(t *testing.T, w *outageWorld) string {
			return mintWith(t, ecSigner{t, w.ec}, map[string]any{"alg": "None", "kid": ecKID}, claimsAt(now))
		}, ErrAlgorithm, 0, ErrAlgorithm},
		{"alg HS384", func(t *testing.T, w *outageWorld) string {
			return mintWith(t, hmacSigner{w.secret}, map[string]any{"alg": "HS384", "kid": ecKID}, claimsAt(now))
		}, ErrAlgorithm, 0, ErrAlgorithm},
		{"alg PS256", func(t *testing.T, w *outageWorld) string {
			return mintWith(t, ecSigner{t, w.ec}, map[string]any{"alg": "PS256", "kid": ecKID}, claimsAt(now))
		}, ErrAlgorithm, 0, ErrAlgorithm},
		{"no alg", func(t *testing.T, w *outageWorld) string {
			return mintWith(t, ecSigner{t, w.ec}, map[string]any{"kid": ecKID}, claimsAt(now))
		}, ErrAlgorithm, 0, ErrAlgorithm},
		{"a kid that is not a string", func(t *testing.T, w *outageWorld) string {
			return mintWith(t, ecSigner{t, w.ec}, map[string]any{"alg": "ES256", "kid": []string{ecKID}}, claimsAt(now))
		}, ErrMalformed, 0, ErrMalformed},
		// the claims already refuse these: no key could make them good, so they are refused for it (the fetch is made, and fails, first: the key set is asked before the claims are read)
		{"expired, the right kid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "exp", sec-3600))
		}, ErrExpired, 1, ErrExpired},
		{"expired, an unknown kid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, "no-such-kid", with(claimsAt(now), "exp", sec-3600))
		}, ErrExpired, 1, ErrKey},
		{"not yet valid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "nbf", sec+3600))
		}, ErrNotYetValid, 1, ErrNotYetValid},
		{"issued in the future", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "iat", sec+3600))
		}, ErrNotYetValid, 1, ErrNotYetValid},
		{"the wrong issuer", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "iss", "https://elsewhere.example.test"))
		}, ErrIssuer, 1, ErrIssuer},
		{"the wrong audience", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "aud", "someone-else"))
		}, ErrAudience, 1, ErrAudience},
		{"a subject that is not a uuid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "sub", "alice"))
		}, ErrSubject, 1, ErrSubject},
		{"no exp", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, with(claimsAt(now), "exp", nil))
		}, ErrMalformed, 1, ErrMalformed},
		// only a key can judge these: the platform's verdict with the key set down, the token's with it up
		{"a forged signature, good claims, the right kid", func(t *testing.T, w *outageWorld) string {
			return mint(t, ecSigner{t, ecKey(t)}, ecKID, claimsAt(now))
		}, ErrKeysUnavailable, 1, ErrSignature},
		{"good claims, an unknown kid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, "no-such-kid", claimsAt(now))
		}, ErrKeysUnavailable, 1, ErrKey},
		{"good claims, no kid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, "", claimsAt(now))
		}, ErrKeysUnavailable, 1, nil},
		{"the legal twin: a good token, the right kid", func(t *testing.T, w *outageWorld) string {
			return w.es(t, ecKID, claimsAt(now))
		}, ErrKeysUnavailable, 1, nil},
	}
	groups := map[error]int{}
	for _, c := range cases {
		w := newOutageWorld(t)
		w.srv.answer(500, []byte("down"))
		tok := c.mk(t, w)

		_, err := verify(w.down, tok)
		wantErr(t, c.name+": key set down", err, c.down)
		if got := w.srv.hits.Load(); got != c.downFetch {
			t.Errorf("%s: the down key set was fetched %d times, want %d", c.name, got, c.downFetch)
		}
		_, err = verify(w.up, tok)
		wantErr(t, c.name+": key set healthy", err, c.healthy)
		groups[c.down]++
	}
	// the instrument: the cases span the refusals that stay refusals and the one that is the platform's (a table that only held one kind would pass for the wrong reason)
	if groups[ErrKeysUnavailable] < 3 || groups[ErrMalformed] < 3 || groups[ErrAlgorithm] < 4 || groups[ErrExpired] < 2 || len(cases) < 20 {
		t.Fatalf("control failed: the cases cover %v over %d cases", groups, len(cases))
	}

	// A key-less refusal does not need the key set even when it never worked: a verifier with ONLY a JWKS URL, pointing nowhere, refuses HS256 for its algorithm, not for its keys.
	w := newOutageWorld(t)
	w.good.Close()
	jwksOnly := mustNew(t, Options{JWKSURL: w.good.url(), AllowInsecureHTTP: true, Issuer: testIssuer, Now: w.clk.Now, HTTPClient: &http.Client{Timeout: 200 * time.Millisecond}})
	_, err := verify(jwksOnly, w.hs(t, claimsAt(now)))
	wantErr(t, "HS256 at a JWKS-only verifier with its key set gone", err, ErrAlgorithm)
	_, err = verify(jwksOnly, w.es(t, ecKID, claimsAt(now)))
	wantErr(t, "the control: ES256 at the same verifier", err, ErrKeysUnavailable)
}

// A stretch of unavailability is one stretch however many requests and failed fetches it holds, and the next outage, after the key set was fetched again, is another.
func TestOutage_AStretchIsReportedOnceAndTheNextOutageIsAnother(t *testing.T) {
	w := newOutageWorld(t)
	now := w.clk.Now()
	tok := w.es(t, ecKID, claimsAt(now))
	unknown := func() string { return w.es(t, "no-such-kid", claimsAt(w.clk.Now())) }

	firsts := func(errs []error) (first int) {
		for _, err := range errs {
			var ku *KeysUnavailableError
			if !errors.As(err, &ku) {
				t.Fatalf("not a *KeysUnavailableError: %v", err)
			}
			if ku.FirstOfStretch() {
				first++
			}
		}
		return first
	}
	burst := func(n int, advanceEvery int) []error {
		var errs []error
		for i := 0; i < n; i++ {
			if advanceEvery > 0 && i%advanceEvery == advanceEvery-1 {
				w.clk.Advance(DefaultMinRefreshInterval) // a further failed fetch inside the same stretch
			}
			_, err := verify(w.down, unknown())
			wantErr(t, "during the outage", err, ErrKeysUnavailable)
			errs = append(errs, err)
		}
		return errs
	}

	// first outage: 40 requests and four failed fetches are one stretch
	w.srv.answer(500, []byte("down"))
	hits0 := w.srv.hits.Load()
	one := burst(40, 10)
	if got := firsts(one); got != 1 {
		t.Errorf("the first outage: %d first-of-stretch answers over %d errors, want exactly 1", got, len(one))
	}
	if fetches := w.srv.hits.Load() - hits0; fetches < 4 {
		t.Errorf("control failed: only %d failed fetches happened in the first outage, the test meant several", fetches)
	}
	// and an error of that stretch, asked again later, is never first again
	if firsts(one[:3]) != 0 {
		t.Errorf("an error of a stretch already reported was first again")
	}

	// The key set comes back. A stretch ends when the key set is FETCHED again (a fetch is the only thing that learns the issuer is there), so the recovery is a token that needs one: an
	// unknown kid, which the healthy set does not hold and which is therefore the token's fault again (ErrKey), where a moment ago it was the platform's.
	recover := func(what string) {
		t.Helper()
		w.srv.answer(200, w.doc)
		w.clk.Advance(DefaultMinRefreshInterval)
		before := w.srv.hits.Load()
		_, err := verify(w.down, unknown())
		wantErr(t, what+": the unknown kid, the key set healthy", err, ErrKey)
		if got := w.srv.hits.Load(); got != before+1 {
			t.Fatalf("%s: %d fetches were made for the recovery, want 1", what, got-before)
		}
		if _, err := verify(w.down, tok); err != nil {
			t.Fatalf("%s: the key set came back and the known kid is still refused: %v", what, err)
		}
	}
	recover("the first recovery")

	// second outage: a new stretch, whose first error is first
	w.srv.answer(500, []byte("down"))
	w.clk.Advance(DefaultMinRefreshInterval)
	two := burst(25, 8)
	if got := firsts(two); got != 1 {
		t.Errorf("the second outage: %d first-of-stretch answers over %d errors, want exactly 1", got, len(two))
	}

	// a third, after another recovery
	recover("the second recovery")
	w.srv.answer(502, nil)
	w.clk.Advance(DefaultMinRefreshInterval)
	if got := firsts(burst(5, 0)); got != 1 {
		t.Errorf("the third outage: %d first-of-stretch answers, want exactly 1", got)
	}

	// the same answer for goroutines racing for it: of 64 errors of one stretch exactly one is first
	recover("the third recovery")
	w.srv.answer(500, []byte("down"))
	w.clk.Advance(DefaultMinRefreshInterval)
	errs := burst(64, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, err := range errs {
		ku := err.(*KeysUnavailableError)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ku.FirstOfStretch() {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d goroutines were told they were first of the stretch, want exactly 1", won)
	}

	// an error that belongs to no stretch is always first (a line repeated is better than a line lost), and a nil one too: the control that FirstOfStretch can answer true more than once
	loose := &KeysUnavailableError{class: ClassTimeout, reason: "x"}
	if !loose.FirstOfStretch() || !loose.FirstOfStretch() {
		t.Error("an error that belongs to no stretch must always be first")
	}
	var none *KeysUnavailableError
	if !none.FirstOfStretch() {
		t.Error("a nil error must be first")
	}
}

// The types are what the API tests them as: errors.Is, errors.As through a wrap, a class that is one of the closed words.
func TestOutage_TheTypedErrorIsWhatTheAPITestsFor(t *testing.T) {
	ku := &KeysUnavailableError{class: ClassTimeout, reason: "the fetch failed: the request timed out"}
	wrapped := fmt.Errorf("verify: %w", ku)
	if !errors.Is(wrapped, ErrKeysUnavailable) || errors.Is(wrapped, ErrKey) || errors.Is(ErrKey, ErrKeysUnavailable) || errors.Is(ErrKeysUnavailable, ErrKey) {
		t.Error("ErrKeysUnavailable and ErrKey must be unrelated, and the typed error must match the first through a wrap")
	}
	var got *KeysUnavailableError
	if !errors.As(wrapped, &got) || got != ku || got.Class() != ClassTimeout {
		t.Errorf("errors.As: %v", got)
	}
	// every class a fetch can produce is one of the six words
	words := map[string]bool{ClassUnreachable: true, ClassTimeout: true, ClassCertificate: true, ClassStatus: true, ClassUnreadable: true, ClassBadKeySet: true}
	if len(words) != 6 {
		t.Fatalf("the class words are not six distinct ones: %v", words)
	}
	for _, w := range []string{ClassUnreachable, ClassTimeout, ClassCertificate, ClassStatus, ClassUnreadable, ClassBadKeySet} {
		if strings.ContainsAny(w, "/:.0123456789 ") || strings.Contains(w, "http") {
			t.Errorf("the class word %q looks like something other than a word", w)
		}
	}
}
