package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every key and secret in these tests is made at run time (crypto/rand): nothing key-shaped is committed.

const (
	testSub    = "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90"
	testSub2   = "7d4c2a90-3e51-4b8f-a6d2-91c0e5f3b7a4"
	testIssuer = "https://project.example.test/auth/v1"
)

// testNow is the injected clock's start: 2001-09-09. Every token below is valid at it and expired at the real time, so a verifier that read the real clock would refuse them all.
var testNow = time.Unix(1_000_000_000, 0).UTC()

// testClock is a clock the tests move.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: testNow} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func randBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// randText is n bytes of random printable text (hex), for a marker or a secret that has to be a string.
func randText(t testing.TB, n int) string { return hex.EncodeToString(randBytes(t, (n+1)/2))[:n] }

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func encJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return enc(b)
}

// ---- signers ------------------------------------------------------------------------------------------------

type signer interface {
	alg() string
	sign(input string) []byte
}

type hmacSigner struct{ secret []byte }

func (hmacSigner) alg() string { return "HS256" }
func (s hmacSigner) sign(in string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(in))
	return m.Sum(nil)
}

type ecSigner struct {
	t   testing.TB
	key *ecdsa.PrivateKey
}

func (ecSigner) alg() string { return "ES256" }
func (s ecSigner) sign(in string) []byte {
	h := sha256.Sum256([]byte(in))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		s.t.Fatal(err)
	}
	return append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
}

type rsaSigner struct {
	t   testing.TB
	key *rsa.PrivateKey
}

func (rsaSigner) alg() string { return "RS256" }
func (s rsaSigner) sign(in string) []byte {
	h := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, h[:])
	if err != nil {
		s.t.Fatal(err)
	}
	return sig
}

// The RSA keys are slow to make, so each is made once per test binary.
var (
	rsaOnce [3]sync.Once
	rsaKeys [3]*rsa.PrivateKey
)

// rsaKey returns RSA key number i (0 and 1 are 2048 bits, 2 is 1024 bits: under the floor).
func rsaKey(t testing.TB, i int) *rsa.PrivateKey {
	t.Helper()
	rsaOnce[i].Do(func() {
		bits := 2048
		if i == 2 {
			bits = 1024
		}
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		rsaKeys[i] = k
	})
	if rsaKeys[i] == nil {
		t.Fatal("an RSA test key could not be made")
	}
	return rsaKeys[i]
}

func ecKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// ---- tokens -------------------------------------------------------------------------------------------------

// mintWith signs header and claims (any JSON-able values; json.RawMessage for a value that must be written exactly).
func mintWith(t testing.TB, s signer, header, claims map[string]any) string {
	t.Helper()
	in := encJSON(t, header) + "." + encJSON(t, claims)
	return in + "." + enc(s.sign(in))
}

// mint signs claims with a standard header (alg of the signer, the kid when given).
func mint(t testing.TB, s signer, kid string, claims map[string]any) string {
	t.Helper()
	h := map[string]any{"alg": s.alg(), "typ": "JWT"}
	if kid != "" {
		h["kid"] = kid
	}
	return mintWith(t, s, h, claims)
}

// mintRaw signs a header and a payload given as exact JSON text (for duplicate keys, trailing text and the like).
func mintRaw(s signer, headerJSON, payloadJSON string) string {
	in := enc([]byte(headerJSON)) + "." + enc([]byte(payloadJSON))
	return in + "." + enc(s.sign(in))
}

// claimsAt is a good set of claims at the instant now: valid for an hour, issued ten seconds ago.
func claimsAt(now time.Time) map[string]any {
	return map[string]any{
		"sub": testSub, "iss": testIssuer, "aud": "authenticated",
		"exp": now.Unix() + 3600, "iat": now.Unix() - 10,
		"role": "authenticated", "email": "someone@example.test", "session_id": testSub2,
	}
}

// with returns a copy of m with the key/value pairs set; a nil value removes the key.
func with(m map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(m)+len(kv)/2)
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(out, kv[i].(string))
			continue
		}
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

// segments splits a token into its three segments.
func segments(tok string) (h, p, s string) {
	parts := strings.Split(tok, ".")
	return parts[0], parts[1], parts[2]
}

// ---- key sets -----------------------------------------------------------------------------------------------

func ecJWK(pub *ecdsa.PublicKey, kid string) map[string]any {
	b, err := pub.Bytes()
	if err != nil {
		panic(err)
	}
	m := map[string]any{"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "x": enc(b[1:33]), "y": enc(b[33:])}
	if kid != "" {
		m["kid"] = kid
	}
	return m
}

func rsaJWK(pub *rsa.PublicKey, kid string) map[string]any {
	m := map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "n": enc(pub.N.Bytes()), "e": enc(big.NewInt(int64(pub.E)).Bytes())}
	if kid != "" {
		m["kid"] = kid
	}
	return m
}

func jwksDoc(t testing.TB, keys ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jwksServer is a key-set endpoint that counts its requests and can be told what to answer.
type jwksServer struct {
	*httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	body []byte
	code int
	gate chan struct{} // when set, every answer waits for it to be closed
}

// hold makes every answer wait until the returned function is called.
func (s *jwksServer) hold() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func newJWKSServer(t testing.TB, body []byte) *jwksServer {
	t.Helper()
	s := &jwksServer{body: body, code: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		body, code, gate := s.body, s.code, s.gate
		s.mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) answer(code int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code, s.body = code, body
}

func (s *jwksServer) url() string { return s.URL + "/auth/v1/.well-known/jwks.json" }

// ---- a world: three signers, two verifiers, one clock ----------------------------------------------------------

const (
	ecKID  = "ec-key-1"
	rsaKID = "rsa-key-1"
)

type world struct {
	clk    *testClock
	secret []byte
	ec     *ecdsa.PrivateKey
	rsa    *rsa.PrivateKey
	srv    *jwksServer
	hmacV  *Verifier // HS256 only
	jwksV  *Verifier // ES256 and RS256 only
	bothV  *Verifier // both sources
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{clk: newClock(), secret: randBytes(t, 48), ec: ecKey(t), rsa: rsaKey(t, 0)}
	w.srv = newJWKSServer(t, jwksDoc(t, ecJWK(&w.ec.PublicKey, ecKID), rsaJWK(&w.rsa.PublicKey, rsaKID)))
	base := Options{Issuer: testIssuer, Now: w.clk.Now}
	h := base
	h.HMACSecret = w.secret
	j := base
	j.JWKSURL, j.AllowInsecureHTTP = w.srv.url(), true
	b := j
	b.HMACSecret = w.secret
	w.hmacV, w.jwksV, w.bothV = mustNew(t, h), mustNew(t, j), mustNew(t, b)
	return w
}

func mustNew(t testing.TB, o Options) *Verifier {
	t.Helper()
	v, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// fixture is one algorithm of the world: how to sign it, and the verifier that accepts it.
type fixture struct {
	name string
	s    signer
	kid  string
	v    *Verifier
}

func (w *world) fixtures(t *testing.T) []fixture {
	return []fixture{
		{"HS256", hmacSigner{w.secret}, "", w.hmacV},
		{"ES256", ecSigner{t, w.ec}, ecKID, w.jwksV},
		{"RS256", rsaSigner{t, w.rsa}, rsaKID, w.jwksV},
	}
}

func (f fixture) mint(t testing.TB, claims map[string]any) string { return mint(t, f.s, f.kid, claims) }

// ---- assertions -----------------------------------------------------------------------------------------------

// wantErr checks err is the sentinel (or nil, when want is nil) and says which case it was.
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Errorf("%s: refused, want accepted: %v", what, err)
	case want != nil && err == nil:
		t.Errorf("%s: accepted, want %v", what, want)
	case want != nil && !errors.Is(err, want):
		t.Errorf("%s: got %v, want %v", what, err, want)
	}
}

// leaks returns the parts of tokens that appear in text: any 8-byte window of any segment, and every extra marker. It is how the tests prove an error text holds nothing of the token.
func leaks(text string, tokens []string, markers ...string) []string {
	var out []string
	for _, m := range markers {
		if m != "" && strings.Contains(text, m) {
			out = append(out, m)
		}
	}
	for _, tok := range tokens {
		for _, seg := range strings.Split(tok, ".") {
			for i := 0; i+8 <= len(seg); i++ {
				if strings.Contains(text, seg[i:i+8]) {
					out = append(out, seg[i:i+8])
					break
				}
			}
		}
	}
	return out
}
