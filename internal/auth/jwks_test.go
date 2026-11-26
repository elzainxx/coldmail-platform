package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jwksWorld is a verifier of one key set, a server for it and a clock, with the keys it has served. Every test below that is about the key set builds its own, so the fetch count of one
// is never another's.
type jwksWorld struct {
	clk *testClock
	srv *jwksServer
	v   *Verifier
	ec  map[string]ecSigner // by kid
}

func newJWKSWorld(t *testing.T, kids ...string) *jwksWorld {
	t.Helper()
	jw := &jwksWorld{clk: newClock(), ec: map[string]ecSigner{}}
	jw.add(t, kids...)
	jw.srv = newJWKSServer(t, jw.doc(t, kids...))
	jw.v = mustNew(t, Options{JWKSURL: jw.srv.url(), AllowInsecureHTTP: true, Issuer: testIssuer, Now: jw.clk.Now})
	return jw
}

func (jw *jwksWorld) add(t *testing.T, kids ...string) {
	for _, kid := range kids {
		jw.ec[kid] = ecSigner{t, ecKey(t)}
	}
}

// doc is the key set holding exactly these kids.
func (jw *jwksWorld) doc(t *testing.T, kids ...string) []byte {
	var keys []map[string]any
	for _, kid := range kids {
		keys = append(keys, ecJWK(&jw.ec[kid].key.PublicKey, kid))
	}
	return jwksDoc(t, keys...)
}

// token is a good token signed by the key of kid, valid at the clock's present.
func (jw *jwksWorld) token(t *testing.T, kid string) string {
	t.Helper()
	return mint(t, jw.ec[kid], kid, claimsAt(jw.clk.Now()))
}

func (jw *jwksWorld) check(t *testing.T, what, kid string, want error, wantHits int32) {
	t.Helper()
	_, err := verify(jw.v, jw.token(t, kid))
	wantErr(t, what, err, want)
	if got := jw.srv.hits.Load(); got != wantHits {
		t.Errorf("%s: the key set has been fetched %d times, want %d", what, got, wantHits)
	}
}

func TestJWKS_UnknownKidRefreshesOnceThenIsErrKey(t *testing.T) {
	jw := newJWKSWorld(t, "k1")
	jw.add(t, "stranger")

	if jw.srv.hits.Load() != 0 {
		t.Fatal("control failed: the key set was fetched before any token needed it")
	}
	jw.check(t, "the first token fetches the key set", "k1", nil, 1)
	jw.check(t, "a known kid needs no fetch", "k1", nil, 1)

	// An unknown kid shortly after: no refresh is allowed yet, and the answer is ErrKey.
	jw.clk.Advance(5 * time.Second)
	jw.check(t, "an unknown kid inside the interval", "stranger", ErrKey, 1)

	// Once the interval has passed an unknown kid makes exactly ONE refresh, and is ErrKey when the refreshed set does not hold it either.
	jw.clk.Advance(26 * time.Second)
	jw.check(t, "an unknown kid after the interval", "stranger", ErrKey, 2)
	jw.check(t, "the same unknown kid again", "stranger", ErrKey, 2)
	jw.check(t, "and again", "stranger", ErrKey, 2)
	jw.check(t, "the known kid still works without a fetch", "k1", nil, 2)

	// The twin: when the refreshed set DOES hold the kid, the one refresh finds it.
	jw.srv.answer(200, jw.doc(t, "k1", "stranger"))
	jw.clk.Advance(31 * time.Second)
	jw.check(t, "a kid that the refreshed set holds", "stranger", nil, 3)
	jw.check(t, "and it keeps working without a fetch", "stranger", nil, 3)
}

func TestJWKS_RefreshIsRateLimited(t *testing.T) {
	jw := newJWKSWorld(t, "k1")
	jw.add(t, "stranger")
	jw.check(t, "the first fetch", "k1", nil, 1)

	// The boundary: 29.999999999 s after the last attempt no fetch is made, at exactly 30 s one is.
	jw.clk.Advance(DefaultMinRefreshInterval - time.Nanosecond)
	jw.check(t, "a nanosecond short of the interval", "stranger", ErrKey, 1)
	jw.clk.Advance(time.Nanosecond)
	jw.check(t, "at the interval", "stranger", ErrKey, 2)
	// The new attempt restarts the interval.
	jw.clk.Advance(DefaultMinRefreshInterval - time.Nanosecond)
	jw.check(t, "a nanosecond short of the next interval", "stranger", ErrKey, 2)

	// A different interval is honoured, and the zero value is the default.
	custom := newJWKSWorld(t, "k1")
	custom.add(t, "stranger")
	custom.v = mustNew(t, Options{JWKSURL: custom.srv.url(), AllowInsecureHTTP: true, MinRefreshInterval: 2 * time.Minute, Now: custom.clk.Now})
	custom.check(t, "custom: the first fetch", "k1", nil, 1)
	custom.clk.Advance(119 * time.Second)
	custom.check(t, "custom: inside two minutes", "stranger", ErrKey, 1)
	custom.clk.Advance(time.Second)
	custom.check(t, "custom: at two minutes", "stranger", ErrKey, 2)
}

// goroutinesBlockedInRefresh counts the goroutines that are waiting for the fetch lock inside keySource.refresh: the proof that a concurrent test really has callers queued behind one
// fetch, and did not merely start them.
func goroutinesBlockedInRefresh() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*keySource).refresh") && (strings.Contains(g, "[sync.Mutex.Lock") || strings.Contains(g, "[semacquire")) {
			n++
		}
	}
	return n
}

func TestJWKS_ConcurrentCallersShareOneFetch(t *testing.T) {
	const callers = 24
	jw := newJWKSWorld(t, "k1")
	jw.add(t, "stranger")
	if n := goroutinesBlockedInRefresh(); n != 0 {
		t.Fatalf("control failed: %d goroutines are already blocked in refresh", n)
	}

	// The server holds its answer until the test lets it go, so the callers pile up behind the one fetch instead of racing past it.
	releaseAll := jw.srv.hold()
	defer releaseAll()

	known, unknown := jw.token(t, "k1"), jw.token(t, "stranger")
	results := make(chan error, 2*callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		tok := known
		if i%2 == 1 {
			tok = unknown
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := verify(jw.v, tok)
			results <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for jw.srv.hits.Load() < 1 || goroutinesBlockedInRefresh() < callers-1 {
		if time.Now().After(deadline) {
			t.Fatalf("the callers never queued behind the fetch: %d requests, %d goroutines blocked of %d", jw.srv.hits.Load(), goroutinesBlockedInRefresh(), callers)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := jw.srv.hits.Load(); got != 1 {
		t.Fatalf("with %d callers waiting, the key set was fetched %d times", callers-1, got)
	}
	releaseAll()
	wg.Wait()
	close(results)

	var ok, errKey int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrKey):
			errKey++
		default:
			t.Errorf("an unexpected error: %v", err)
		}
	}
	if ok != callers/2 || errKey != callers/2 {
		t.Errorf("%d accepted and %d ErrKey, want %d of each (the known kid verifies, the unknown one is ErrKey)", ok, errKey, callers/2)
	}
	if got := jw.srv.hits.Load(); got != 1 {
		t.Errorf("the key set was fetched %d times for %d concurrent callers, want 1", got, callers)
	}
}

func TestJWKS_ARotatedKeyIsPickedUp(t *testing.T) {
	jw := newJWKSWorld(t, "old")
	jw.add(t, "new")
	jw.check(t, "the old key", "old", nil, 1)

	// The issuer rotates: both keys are published for a while.
	jw.srv.answer(200, jw.doc(t, "old", "new"))
	jw.check(t, "the new key is not known yet, and no refresh is allowed yet", "new", ErrKey, 1)
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "the new key after the interval", "new", nil, 2)
	jw.check(t, "the old key still verifies", "old", nil, 2)

	// Then the old one is withdrawn: the next refresh (made at the next unknown kid, or when the cached set outlives its lifetime) drops it.
	jw.add(t, "newer")
	jw.srv.answer(200, jw.doc(t, "new", "newer"))
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "a newer key", "newer", nil, 3)
	jw.check(t, "the old key is gone from the set the refresh returned (and no second fetch is made)", "old", ErrKey, 3)
}

func TestJWKS_AWithdrawnKeyStopsWorkingAfterTheKeyLifetime(t *testing.T) {
	jw := newJWKSWorld(t, "withdrawn")
	jw.add(t, "kept")
	jw.check(t, "the key before it is withdrawn", "withdrawn", nil, 1)

	jw.srv.answer(200, jw.doc(t, "kept"))
	jw.clk.Advance(DefaultKeyTTL - time.Minute)
	jw.check(t, "inside the key lifetime the cached key still verifies, with no fetch", "withdrawn", nil, 1)

	// Past the lifetime a use of the key makes the refresh: the new set lacks it, and the answer is ErrKey (a revoked key is not served from the cache).
	jw.clk.Advance(2 * time.Minute)
	jw.check(t, "past the key lifetime, the refreshed set lacks the key", "withdrawn", ErrKey, 2)
	jw.check(t, "the key that was kept", "kept", nil, 2)

	// While the issuer cannot be reached, a stale key keeps serving: being unable to fetch is not a reason to sign everybody out.
	down := newJWKSWorld(t, "k1")
	down.check(t, "before the outage", "k1", nil, 1)
	down.srv.answer(503, []byte("down"))
	down.clk.Advance(DefaultKeyTTL + time.Minute)
	down.check(t, "past the lifetime with the issuer down: the cached key serves", "k1", nil, 2)
	down.check(t, "and the failed attempt is not repeated before the interval", "k1", nil, 2)
	down.clk.Advance(DefaultMinRefreshInterval)
	down.check(t, "after the interval it tries again", "k1", nil, 3)
	down.srv.answer(200, down.doc(t, "k1"))
	down.clk.Advance(DefaultMinRefreshInterval)
	down.check(t, "the issuer is back: the key is fresh again", "k1", nil, 4)
	down.srv.answer(503, []byte("down"))
	down.clk.Advance(DefaultKeyTTL - time.Minute)
	down.check(t, "and inside the new lifetime no fetch is made at all", "k1", nil, 4)

	// The lifetime is an option.
	short := newJWKSWorld(t, "k1")
	short.v = mustNew(t, Options{JWKSURL: short.srv.url(), AllowInsecureHTTP: true, KeyTTL: 5 * time.Minute, Now: short.clk.Now})
	short.check(t, "short lifetime: first use", "k1", nil, 1)
	short.clk.Advance(4*time.Minute + 59*time.Second)
	short.check(t, "short lifetime: just inside", "k1", nil, 1)
	short.clk.Advance(time.Second)
	short.check(t, "short lifetime: at the lifetime", "k1", nil, 2)
}

func TestJWKS_AFailingRefreshKeepsTheCachedKeys(t *testing.T) {
	jw := newJWKSWorld(t, "k1")
	jw.add(t, "k2")
	jw.check(t, "the first fetch", "k1", nil, 1)

	huge := append([]byte(nil), jw.doc(t, "k1", "k2")...)
	huge = append(huge, []byte(strings.Repeat(" ", MaxKeySetBytes))...)
	failures := []struct {
		name string
		code int
		body []byte
	}{
		{"a 500", 500, []byte("boom")},
		{"a 503", 503, nil},
		{"a 404", 404, []byte("not found")},
		{"a 204", 204, nil},
		{"a 301 that is not followed", 301, nil},
		{"not JSON", 200, []byte("<html>maintenance</html>")},
		{"an empty body", 200, nil},
		{"a JSON array", 200, []byte(`[1,2]`)},
		{"no keys member", 200, []byte(`{}`)},
		{"an empty key set", 200, []byte(`{"keys":[]}`)},
		{"only unusable keys", 200, []byte(`{"keys":[{"kty":"oct","k":"AAAA"}]}`)},
		{"a body over 1 MiB", 200, huge},
	}
	hits := int32(1)
	for _, f := range failures {
		jw.srv.answer(f.code, f.body)
		jw.clk.Advance(DefaultMinRefreshInterval)
		hits++
		// The refresh is made for the unknown kid and fails; the cached key keeps working, the unknown kid is ErrKeysUnavailable (the platform cannot tell whether the key exists).
		jw.check(t, f.name+": an unknown kid", "k2", ErrKeysUnavailable, hits)
		jw.check(t, f.name+": the cached key", "k1", nil, hits)
	}

	// The set recovers: the next unknown kid, after the interval, finds the kid the new set holds.
	jw.srv.answer(200, jw.doc(t, "k1", "k2"))
	jw.clk.Advance(DefaultMinRefreshInterval)
	jw.check(t, "the issuer recovers", "k2", nil, hits+1)

	// A server that is gone entirely (connection refused) is the same.
	jw.srv.Close()
	jw.add(t, "k3")
	jw.clk.Advance(DefaultMinRefreshInterval)
	_, err := verify(jw.v, jw.token(t, "k3"))
	wantErr(t, "an unknown kid with the server gone", err, ErrKeysUnavailable)
	_, err = verify(jw.v, jw.token(t, "k1"))
	wantErr(t, "the cached key with the server gone", err, nil)
}

func TestJWKS_TheFirstFetchFailingIsErrKeysUnavailable(t *testing.T) {
	jw := newJWKSWorld(t, "k1")
	jw.srv.answer(500, []byte("boom"))
	jw.check(t, "the very first fetch fails", "k1", ErrKeysUnavailable, 1)
	jw.check(t, "the next token, inside the interval, makes no fetch", "k1", ErrKeysUnavailable, 1)
	jw.clk.Advance(DefaultMinRefreshInterval - time.Second)
	jw.check(t, "still inside the interval", "k1", ErrKeysUnavailable, 1)
	jw.srv.answer(200, jw.doc(t, "k1"))
	jw.clk.Advance(time.Second)
	jw.check(t, "after the interval the fetch succeeds", "k1", nil, 2)

	// The server is not there at all.
	gone := newJWKSWorld(t, "k1")
	gone.srv.Close()
	_, err := verify(gone.v, gone.token(t, "k1"))
	wantErr(t, "no server", err, ErrKeysUnavailable)

	// A key set with nothing usable in it is the same as none.
	empty := newJWKSWorld(t, "k1")
	empty.srv.answer(200, []byte(`{"keys":[]}`))
	empty.check(t, "an empty set", "k1", ErrKeysUnavailable, 1)
}

func TestJWKS_KeySetParsing(t *testing.T) {
	ecA, ecB := ecKey(t), ecKey(t)
	rsaA, rsaWeak := rsaKey(t, 0), rsaKey(t, 2)
	good := ecJWK(&ecA.PublicKey, "ec-a")
	goodRSA := rsaJWK(&rsaA.PublicKey, "rsa-a")
	bigN := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 8200), big.NewInt(1))
	evenN := new(big.Int).Lsh(rsaA.PublicKey.N, 1)
	p384 := with(good, "crv", "P-384")
	offCurve := with(good, "y", enc(randBytes(t, 32)), "x", enc(randBytes(t, 32)))

	type keyCase struct {
		name string
		keys []map[string]any
		ok   int    // the number of usable keys; 0 means the set is refused
		why  string // for a refused set: a part of the reason
	}
	cases := []keyCase{
		{"an EC key", []map[string]any{good}, 1, ""},
		{"an RSA key", []map[string]any{goodRSA}, 1, ""},
		{"both", []map[string]any{good, goodRSA}, 2, ""},
		{"an EC key with no alg and no use", []map[string]any{with(good, "alg", nil, "use", nil)}, 1, ""},
		{"an RSA key with no alg and no use", []map[string]any{with(goodRSA, "alg", nil, "use", nil)}, 1, ""},
		{"key_ops holding verify", []map[string]any{with(good, "key_ops", []string{"verify"})}, 1, ""},
		{"key_ops without verify", []map[string]any{with(good, "key_ops", []string{"sign"})}, 0, "not for verification"},
		{"an encryption key", []map[string]any{with(good, "use", "enc")}, 0, "not for signatures"},
		{"an EC key whose alg is RS256", []map[string]any{with(good, "alg", "RS256")}, 0, "alg does not fit"},
		{"an EC key whose alg is ES384", []map[string]any{with(good, "alg", "ES384")}, 0, "alg does not fit"},
		{"an RSA key whose alg is ES256", []map[string]any{with(goodRSA, "alg", "ES256")}, 0, "alg does not fit"},
		{"an RSA key whose alg is RS384", []map[string]any{with(goodRSA, "alg", "RS384")}, 0, "alg does not fit"},
		{"an RSA key whose alg is HS256", []map[string]any{with(goodRSA, "alg", "HS256")}, 0, "alg does not fit"},
		{"a symmetric key (a secret in a key set)", []map[string]any{{"kty": "oct", "kid": "s", "alg": "HS256", "k": enc(randBytes(t, 32))}}, 0, "only EC and RSA"},
		{"an OKP key", []map[string]any{{"kty": "OKP", "crv": "Ed25519", "kid": "o", "x": enc(randBytes(t, 32))}}, 0, "only EC and RSA"},
		{"an EC key on P-384", []map[string]any{p384}, 0, "other than P-256"},
		{"an EC point that is not on the curve", []map[string]any{offCurve}, 0, "not on the curve"},
		{"EC coordinates of the wrong length", []map[string]any{with(good, "x", enc(randBytes(t, 31)))}, 0, "malformed coordinates"},
		{"EC coordinates that are not base64url", []map[string]any{with(good, "x", "!!!")}, 0, "malformed coordinates"},
		{"EC coordinates with padding", []map[string]any{with(good, "y", good["y"].(string)+"==")}, 0, "malformed coordinates"},
		{"an RSA key of 1024 bits", []map[string]any{rsaJWK(&rsaWeak.PublicKey, "weak")}, 0, "under 2048 bits"},
		{"an RSA key of 1024 bits beside a good one", []map[string]any{rsaJWK(&rsaWeak.PublicKey, "weak"), good}, 1, ""},
		{"an RSA modulus over 8192 bits", []map[string]any{with(goodRSA, "n", enc(bigN.Bytes()))}, 0, "over 8192 bits"},
		{"an even RSA modulus", []map[string]any{with(goodRSA, "n", enc(evenN.Bytes()))}, 0, "even RSA modulus"},
		{"an RSA exponent of 4", []map[string]any{with(goodRSA, "e", enc([]byte{4}))}, 0, "exponent"},
		{"an RSA exponent of 1", []map[string]any{with(goodRSA, "e", enc([]byte{1}))}, 0, "exponent"},
		{"an RSA exponent over 2^31-1", []map[string]any{with(goodRSA, "e", enc([]byte{0x80, 0, 0, 1}))}, 0, "exponent"},
		{"an RSA exponent of 3", []map[string]any{with(goodRSA, "e", enc([]byte{3}))}, 1, ""},
		{"an RSA key with a malformed modulus", []map[string]any{with(goodRSA, "n", "!!!")}, 0, "malformed modulus"},
		{"one kid-less key alone", []map[string]any{with(good, "kid", nil)}, 1, ""},
		{"two kid-less keys", []map[string]any{with(good, "kid", nil), with(goodRSA, "kid", nil)}, 0, "without a kid in a set of several"},
		{"a kid-less key beside a key with a kid", []map[string]any{with(good, "kid", nil), goodRSA}, 1, ""},
		{"a kid-less key beside an unusable key", []map[string]any{with(good, "kid", nil), p384}, 1, ""},
		{"two keys with one kid", []map[string]any{good, ecJWK(&ecB.PublicKey, "ec-a")}, 0, "share a kid"},
		{"two keys with two kids", []map[string]any{good, ecJWK(&ecB.PublicKey, "ec-b")}, 2, ""},
		{"a usable key beside unusable ones", []map[string]any{p384, good, offCurve, {"kty": "oct"}}, 1, ""},
	}
	for _, c := range cases {
		body := jwksDoc(t, c.keys...)
		snap, err := parseKeySet(body, testNow)
		switch {
		case c.ok == 0 && err == nil:
			t.Errorf("%s: accepted (%d keys), want refused", c.name, len(snap.keys))
		case c.ok == 0 && !strings.Contains(err.Error(), c.why):
			t.Errorf("%s: refused for %q, want the reason to say %q", c.name, err, c.why)
		case c.ok > 0 && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.ok > 0 && len(snap.keys) != c.ok:
			t.Errorf("%s: %d usable keys, want %d", c.name, len(snap.keys), c.ok)
		}
		if err != nil && strings.Contains(err.Error(), string(good["x"].(string))) {
			t.Errorf("%s: the refusal repeats key material: %v", c.name, err)
		}
	}
	for name, body := range map[string]string{
		"not JSON": "nope", "an array": `[]`, "a string": `"x"`, "keys is a string": `{"keys":"x"}`, "keys is an object": `{"keys":{}}`, "keys is empty": `{"keys":[]}`,
		"no keys": `{}`, "an entry that is a string": `{"keys":["x"]}`, "an entry that is an array": `{"keys":[[]]}`, "a kid that is a number": `{"keys":[{"kty":"EC","kid":7}]}`,
	} {
		if _, err := parseKeySet([]byte(body), testNow); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tooMany := make([]map[string]any, maxKeysInSet+1)
	for i := range tooMany {
		tooMany[i] = ecJWK(&ecA.PublicKey, fmt.Sprint("k", i))
	}
	if _, err := parseKeySet(jwksDoc(t, tooMany...), testNow); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("a set of %d keys: %v", len(tooMany), err)
	}
	if _, err := parseKeySet(jwksDoc(t, tooMany[:maxKeysInSet]...), testNow); err != nil {
		t.Errorf("a set of %d keys (the most): %v", maxKeysInSet, err)
	}

	// The same through Verify. An RSA key of 1024 bits is never a key: a token signed by it is ErrKey whatever it says, and the 2048-bit twin verifies.
	clk := newClock()
	srv := newJWKSServer(t, jwksDoc(t, rsaJWK(&rsaWeak.PublicKey, "weak")))
	v := mustNew(t, Options{JWKSURL: srv.url(), AllowInsecureHTTP: true, Now: clk.Now})
	_, err := verify(v, mint(t, rsaSigner{t, rsaWeak}, "weak", claimsAt(clk.Now())))
	if !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "under 2048 bits") {
		t.Errorf("a token signed by a 1024-bit RSA key: %v", err)
	}
	srv.answer(200, jwksDoc(t, rsaJWK(&rsaA.PublicKey, "weak")))
	clk.Advance(DefaultMinRefreshInterval)
	if _, err := verify(v, mint(t, rsaSigner{t, rsaA}, "weak", claimsAt(clk.Now()))); err != nil {
		t.Errorf("the 2048-bit twin under the same kid: %v", err)
	}

	// A key set with exactly one key and no kid serves a token with no kid; a token that names a kid is not served by a key that has none.
	clk2 := newClock()
	srv2 := newJWKSServer(t, jwksDoc(t, with(good, "kid", nil)))
	v2 := mustNew(t, Options{JWKSURL: srv2.url(), AllowInsecureHTTP: true, Now: clk2.Now})
	if _, err := verify(v2, mint(t, ecSigner{t, ecA}, "", claimsAt(clk2.Now()))); err != nil {
		t.Errorf("a token with no kid and a key set of one key with no kid: %v", err)
	}
	_, err = verify(v2, mint(t, ecSigner{t, ecA}, "somebody", claimsAt(clk2.Now())))
	wantErr(t, "a token that names a kid and a key that has none", err, ErrKey)
	// A set of one key WITH a kid also serves a token with no kid (there is nothing to choose between); a set of two does not.
	srv3 := newJWKSServer(t, jwksDoc(t, good))
	v3 := mustNew(t, Options{JWKSURL: srv3.url(), AllowInsecureHTTP: true, Now: clk2.Now})
	if _, err := verify(v3, mint(t, ecSigner{t, ecA}, "", claimsAt(clk2.Now()))); err != nil {
		t.Errorf("a token with no kid and a set of one key: %v", err)
	}
	srv4 := newJWKSServer(t, jwksDoc(t, good, ecJWK(&ecB.PublicKey, "ec-b")))
	v4 := mustNew(t, Options{JWKSURL: srv4.url(), AllowInsecureHTTP: true, Now: clk2.Now})
	_, err = verify(v4, mint(t, ecSigner{t, ecA}, "", claimsAt(clk2.Now())))
	wantErr(t, "a token with no kid and a set of two keys", err, ErrKey)
	clk2.Advance(time.Hour) // past the rate limit: a refresh could be made, and must not be, for a token that names no kid
	_, err = verify(v4, mint(t, ecSigner{t, ecA}, "", claimsAt(clk2.Now())))
	wantErr(t, "the same, long after", err, ErrKey)
	if got := srv4.hits.Load(); got != 1 {
		t.Errorf("a token with no kid, against a set of two keys, made %d fetches: want only the first, and none for the missing kid", got)
	}
}

func TestJWKS_TheFetchIsHardened(t *testing.T) {
	// The default client.
	c := hardenedClient(nil)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the default client must have its own transport: %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Errorf("the default client uses a proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Errorf("the default client's TLS configuration is not a verifying one: %+v", tr.TLSClientConfig)
	}
	if c.Timeout != 10*time.Second || c.Jar != nil {
		t.Errorf("the default client: timeout %v, jar %v", c.Timeout, c.Jar)
	}
	if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Errorf("the default client follows redirects")
	}
	// A client the caller gives is copied, not changed; redirects are refused all the same, and a zero timeout becomes 10 s.
	mine := &http.Client{Transport: tr}
	got := hardenedClient(mine)
	if mine.CheckRedirect != nil || mine.Timeout != 0 {
		t.Errorf("the caller's client was changed: %+v", mine)
	}
	if got == mine || got.Transport != mine.Transport || got.Timeout != 10*time.Second || got.CheckRedirect == nil || !errors.Is(got.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Errorf("the copy of the caller's client: %+v", got)
	}
	if kept := hardenedClient(&http.Client{Timeout: 3 * time.Second}); kept.Timeout != 3*time.Second {
		t.Errorf("a timeout the caller set was replaced by %v", kept.Timeout)
	}

	w := newWorld(t)
	good := jwksDoc(t, ecJWK(&w.ec.PublicKey, ecKID))
	tok := func(v *Verifier) error {
		_, err := verify(v, mint(t, ecSigner{t, w.ec}, ecKID, claimsAt(w.clk.Now())))
		return err
	}
	newV := func(url string, mod ...func(*Options)) *Verifier {
		o := Options{JWKSURL: url, AllowInsecureHTTP: true, Now: w.clk.Now}
		for _, m := range mod {
			m(&o)
		}
		return mustNew(t, o)
	}

	// The request: a GET that asks for JSON, and says nothing else of the caller.
	var seen atomic.Pointer[http.Request]
	probe := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		seen.Store(r.Clone(context.Background()))
		_, _ = rw.Write(good)
	}))
	defer probe.Close()
	if err := tok(newV(probe.URL + "/x")); err != nil {
		t.Errorf("the plain fetch: %v", err)
	}
	if r := seen.Load(); r == nil || r.Method != http.MethodGet || r.URL.Path != "/x" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Errorf("the request was not a plain GET for JSON: %+v", r)
	}

	// A redirect is not followed, and is a failed fetch; the twin is the same body served directly.
	target := newJWKSServer(t, good)
	redirector := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, target.url(), http.StatusFound) }))
	defer redirector.Close()
	err := tok(newV(redirector.URL + "/jwks"))
	if !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "redirects are not followed") || target.hits.Load() != 0 {
		t.Errorf("a redirect: %v (the target was asked %d times)", err, target.hits.Load())
	}
	if err := tok(newV(target.url())); err != nil {
		t.Errorf("control failed: the target served directly is refused: %v", err)
	}
	// ... even with a client that would follow them.
	err = tok(newV(redirector.URL+"/jwks", func(o *Options) { o.HTTPClient = &http.Client{} }))
	if !errors.Is(err, ErrKeysUnavailable) || target.hits.Load() != 1 {
		t.Errorf("a caller's client that follows redirects: %v (the target was asked %d times in all, want only the direct request)", err, target.hits.Load())
	}

	// The size: exactly one MiB is read, one byte more is refused.
	pad := func(n int) []byte {
		return append(append([]byte(nil), good...), []byte(strings.Repeat(" ", n-len(good)))...)
	}
	big1 := newJWKSServer(t, pad(MaxKeySetBytes))
	if err := tok(newV(big1.url())); err != nil {
		t.Errorf("a response of exactly 1 MiB: %v", err)
	}
	big2 := newJWKSServer(t, pad(MaxKeySetBytes+1))
	if err := tok(newV(big2.url())); !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Errorf("a response of 1 MiB and a byte: %v", err)
	}

	// The time: a server that does not answer in time is a failed fetch.
	slow := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	start := time.Now()
	err = tok(newV(slow.URL, func(o *Options) { o.HTTPClient = &http.Client{Timeout: 150 * time.Millisecond} }))
	if !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 4*time.Second {
		t.Errorf("a server that does not answer: %v after %v", err, time.Since(start))
	}

	// TLS: a certificate the system does not trust is refused by the default client; the twin trusts it explicitly.
	secure := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(good) }))
	defer secure.Close()
	plain := mustNew(t, Options{JWKSURL: secure.URL, Now: w.clk.Now})
	err = tok(plain)
	if !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an untrusted certificate: %v", err)
	}
	trusting := mustNew(t, Options{JWKSURL: secure.URL, HTTPClient: secure.Client(), Now: w.clk.Now})
	if err := tok(trusting); err != nil {
		t.Errorf("the twin that trusts the certificate: %v", err)
	}
}

// A key set is data from the network: members this package does not read may be of any shape (a huge number, a deeply nested array, a long string, the private half of the key) and cost
// neither a failure nor unbounded work, and nothing of them is kept.
func TestJWKS_UnusedMembersOfAnyShapeAreIgnored(t *testing.T) {
	w := newWorld(t)
	odd := rawJSON(`{"a":[[[[[[[[[[1e999999999,"` + strings.Repeat("x", 100000) + `"]]]]]]]]]]}`)
	key := with(ecJWK(&w.ec.PublicKey, ecKID), "extra", odd, "d", enc(randBytes(t, 32)), "x5c", []string{enc(randBytes(t, 40))})
	body, err := json.Marshal(map[string]any{"keys": []any{key}, "another": odd})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := parseKeySet(body, testNow)
	if err != nil || len(snap.keys) != 1 || snap.byKID[ecKID] == nil {
		t.Fatalf("a key with unused members of odd shapes: %v", err)
	}
	// The twin that must fail: the same key with a member the package DOES read in a shape it cannot read (a number where the coordinate string is).
	bad, _ := json.Marshal(map[string]any{"keys": []any{with(ecJWK(&w.ec.PublicKey, ecKID), "x", 5)}})
	if _, err := parseKeySet(bad, testNow); err == nil {
		t.Error("a key whose x is a number was accepted")
	}
}
