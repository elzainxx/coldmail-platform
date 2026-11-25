// Package authtest mints Supabase-shaped access tokens for tests, with keys it generates at run time, and builds the real internal/auth Verifier that accepts them.
//
// IT IS TEST SUPPORT AND NOTHING ELSE. No production package imports it (authtest_test.go fails when `go list -deps ./cmd/platform` names it, and when any non-test file outside this
// directory imports it). It lives under internal/auth so that it can build a Verifier that fetches its keys from a loopback test server over plain http, an option that only tests and the
// harness may name (TestAllowInsecureHTTP_NoProductionCodeSetsIt skips this directory on purpose).
//
// WHAT IT GIVES A TEST. A Minter holds one key source: a random HS256 secret, or a freshly generated P-256 key whose public half it serves as a key set. Mint signs claims with it; Good
// makes the claims a signed-in user's token carries at the Minter's clock; Forged, Unsigned and Tampered make the tokens an attacker could make. Verifier returns the real verifier for the
// Minter's key source, reading the Minter's Now hook, so a test moves time without sleeping. Nothing here is a credential: every secret and key exists only for the life of the test.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/auth"
)

// The issuer and the key id of a Minter, and the audience a signed-in user's token carries.
const (
	Issuer   = "https://project.example.test/auth/v1"
	Audience = "authenticated"
	KeyID    = "kid-1"
)

// Minter mints tokens signed by one key source. Set Now to move its clock (nil means time.Now); the Verifier it builds reads the same hook on every call.
type Minter struct {
	// Now is the clock tokens are minted at and the Verifier judges them at. Tests assign it; a zero value means time.Now.
	Now func() time.Time
	// Issuer and Audience are what Good puts in the claims, and what the Verifier is configured to require (the issuer) or accept (the audience). They start as the package constants.
	Issuer   string
	Audience string

	alg    string // HS256 or ES256
	secret []byte
	key    *ecdsa.PrivateKey

	otherSecret []byte            // the secret of Forged for HS256
	other       *ecdsa.PrivateKey // the key of Forged for ES256
}

// NewHS256 returns a Minter whose key source is a random 48-character HMAC secret.
func NewHS256(t testing.TB) *Minter {
	t.Helper()
	return &Minter{alg: "HS256", secret: []byte(randText(t, 48)), otherSecret: []byte(randText(t, 48)), Issuer: Issuer, Audience: Audience}
}

// NewES256 returns a Minter whose key source is a freshly generated P-256 key, published (as a key set) by the test server its Verifier fetches from.
func NewES256(t testing.TB) *Minter {
	t.Helper()
	return &Minter{alg: "ES256", key: genKey(t), other: genKey(t), Issuer: Issuer, Audience: Audience}
}

// Alg is "HS256" or "ES256".
func (m *Minter) Alg() string { return m.alg }

func (m *Minter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Verifier builds the real *auth.Verifier for this Minter's key source: the issuer pinned, the audience as the Minter's, the default leeway, the Minter's clock. For ES256 it starts a loopback
// server that publishes the key set (closed when the test ends).
func (m *Minter) Verifier(t testing.TB) *auth.Verifier {
	t.Helper()
	o := auth.Options{Issuer: m.Issuer, Audience: m.Audience, Now: m.now}
	switch m.alg {
	case "HS256":
		o.HMACSecret = m.secret
	default:
		doc := m.JWKS(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(doc)
		}))
		t.Cleanup(srv.Close)
		o.JWKSURL, o.AllowInsecureHTTP = srv.URL+"/auth/v1/.well-known/jwks.json", true
	}
	v, err := auth.New(o)
	if err != nil {
		t.Fatalf("authtest: the verifier could not be built: %v", err)
	}
	return v
}

// JWKS is the key set of an ES256 Minter as the JSON document Supabase publishes (nil for HS256, which has no public key).
func (m *Minter) JWKS(t testing.TB) []byte {
	t.Helper()
	if m.key == nil {
		return nil
	}
	pt, err := m.key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("authtest: %v", err)
	}
	doc, err := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": KeyID,
		"x": b64(pt[1:33]), "y": b64(pt[33:]),
	}}})
	if err != nil {
		t.Fatalf("authtest: %v", err)
	}
	return doc
}

// Claims are the claims of a token: any JSON-able values (json.RawMessage for text that must be written exactly).
type Claims map[string]any

// Good is the claims of a signed-in user's token at the Minter's clock: valid for an hour, issued ten seconds ago, for the Minter's issuer and audience, role authenticated.
func (m *Minter) Good(sub string) Claims {
	now := m.now()
	return Claims{
		"sub": sub, "iss": m.Issuer, "aud": m.Audience,
		"exp": now.Unix() + 3600, "iat": now.Unix() - 10,
		"role": "authenticated", "email": "someone@example.test",
	}
}

// With returns a copy of c with the key/value pairs set; a nil value removes the key.
func (c Claims) With(kv ...any) Claims {
	out := make(Claims, len(c)+len(kv)/2)
	for k, v := range c {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if kv[i+1] == nil {
			delete(out, k)
			continue
		}
		out[k] = kv[i+1]
	}
	return out
}

// Token is Mint(Good(sub)): a legal token of the user.
func (m *Minter) Token(sub string) string { return m.Mint(m.Good(sub)) }

// Mint signs the claims with the Minter's key under its standard header (alg, typ, and kid for ES256).
func (m *Minter) Mint(c Claims) string { return m.mint(c, m.signer(false)) }

// Forged signs the claims as Mint does, but with a key the Minter's Verifier does not trust: another random secret, or another generated P-256 key under the same key id.
func (m *Minter) Forged(c Claims) string { return m.mint(c, m.signer(true)) }

// Unsigned is a token whose header says "alg":"none" and whose signature is empty.
func (m *Minter) Unsigned(c Claims) string {
	in := enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(c)
	return in + "."
}

// Tampered takes a legal token and replaces its payload with the claims, keeping the header and the signature: the signature no longer covers what the token says.
func (m *Minter) Tampered(legal string, c Claims) string {
	parts := strings.Split(legal, ".")
	if len(parts) != 3 {
		return legal
	}
	return parts[0] + "." + enc(c) + "." + parts[2]
}

// Header signs the claims under a header the caller writes (for a test of an odd header); the signature is the Minter's.
func (m *Minter) Header(header map[string]any, c Claims) string {
	in := enc(header) + "." + enc(c)
	return in + "." + b64(m.signer(false)(in))
}

func (m *Minter) mint(c Claims, sign func(string) []byte) string {
	h := map[string]any{"alg": m.alg, "typ": "JWT"}
	if m.alg == "ES256" {
		h["kid"] = KeyID
	}
	in := enc(h) + "." + enc(c)
	return in + "." + b64(sign(in))
}

// signer returns the function that signs a signing input: the Minter's own key, or (forged) one the verifier has never been given.
func (m *Minter) signer(forged bool) func(string) []byte {
	if m.alg == "HS256" {
		secret := m.secret
		if forged {
			secret = m.otherSecret
		}
		return func(in string) []byte {
			mac := hmac.New(sha256.New, secret)
			mac.Write([]byte(in))
			return mac.Sum(nil)
		}
	}
	key := m.key
	if forged {
		key = m.other
	}
	return func(in string) []byte {
		h := sha256.Sum256([]byte(in))
		r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
		if err != nil {
			panic("authtest: " + err.Error()) // crypto/rand does not fail here
		}
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}
}

func genKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("authtest: %v", err)
	}
	return k
}

func randText(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("authtest: %v", err)
	}
	return hex.EncodeToString(b)[:n]
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func enc(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("authtest: " + err.Error()) // only a test's own claims can fail to encode
	}
	return b64(b)
}

// Secret is the HMAC secret of an HS256 Minter, as text a test can hand to a service under test through its environment (PLATFORM_JWT_SECRET); "" for an ES256 Minter.
func (m *Minter) Secret() string { return string(m.secret) }
