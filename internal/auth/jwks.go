package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// jwk is one usable public key of a key set: an ES256 key (EC, P-256) or an RS256 key (RSA, at least MinRSABits). A key of any other kind never becomes one, so a published key can
// only verify a signature of its own algorithm and can never be mistaken for an HMAC secret.
type jwk struct {
	kid string
	alg string // "ES256" or "RS256"
	ec  *ecdsa.PublicKey
	rsa *rsa.PublicKey
}

// verify checks sig, the JWS form of the signature (ES256: R then S, 32 bytes each; RS256: PKCS #1 v1.5), over signingInput.
func (k *jwk) verify(signingInput string, sig []byte) error {
	h := sha256.Sum256([]byte(signingInput))
	switch {
	case k.ec != nil:
		if len(sig) != 64 {
			return fail(ErrSignature, "an ES256 signature is 64 bytes, R then S")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k.ec, h[:], r, s) {
			return fail(ErrSignature, "the signature does not match")
		}
		return nil
	case k.rsa != nil:
		if err := rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig); err != nil {
			return fail(ErrSignature, "the signature does not match")
		}
		return nil
	}
	return fail(ErrKey, "the key holds no key material")
}

// keySnapshot is one fetched key set. It is never changed after it is published: a refresh builds a new one and swaps the pointer.
type keySnapshot struct {
	keys      []*jwk
	byKID     map[string]*jwk // the keys that have a kid
	fetchedAt time.Time
}

// lookup finds the key a token names. A token that names a kid gets exactly that key; a token that names none gets the key of a set that holds exactly one. A nil snapshot has no key.
func (s *keySnapshot) lookup(kid string) *jwk {
	if s == nil {
		return nil
	}
	if kid != "" {
		return s.byKID[kid]
	}
	if len(s.keys) == 1 {
		return s.keys[0]
	}
	return nil
}

// keySource is the key set of one JWKS URL: the current snapshot, swapped atomically, and the rules for fetching it again.
type keySource struct {
	url        string
	client     *http.Client
	now        func() time.Time
	minRefresh time.Duration
	ttl        time.Duration

	snap atomic.Pointer[keySnapshot]

	mu          sync.Mutex // serialises fetches; guards the three fields below
	attempted   bool
	lastAttempt time.Time
	lastFailure string // why the last fetch failed, "" when it did not
}

func newKeySource(url string, client *http.Client, now func() time.Time, minRefresh, ttl time.Duration) *keySource {
	return &keySource{url: url, client: client, now: now, minRefresh: minRefresh, ttl: ttl}
}

// hardenedClient is the client the key set is fetched with: the caller's, or a default one with a 10 s timeout, no proxy and certificate verification on. A copy is made, so the
// caller's client is not changed; redirects are refused and cookies dropped in either case.
func hardenedClient(c *http.Client) *http.Client {
	var hc http.Client
	if c == nil {
		var tr *http.Transport
		if def, ok := http.DefaultTransport.(*http.Transport); ok {
			tr = def.Clone()
		} else {
			tr = &http.Transport{}
		}
		tr.Proxy = nil
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		hc = http.Client{Transport: tr}
	} else {
		hc = *c
	}
	if hc.Timeout == 0 {
		hc.Timeout = fetchTimeout
	}
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// keyFor returns the key a token with this kid is verified with. A key that is in the current snapshot and not older than the TTL is used as it is. A kid the snapshot lacks, a
// snapshot older than the TTL and a first use start ONE refresh, which MinRefreshInterval rate limits across all callers. If the refresh cannot be made or fails, a key that was
// already cached keeps serving (the issuer being unreachable does not sign everyone out); a kid that was never cached is ErrKey.
func (s *keySource) keyFor(ctx context.Context, kid string) (*jwk, error) {
	snap := s.snap.Load()
	k := snap.lookup(kid)
	if k != nil && s.now().Sub(snap.fetchedAt) < s.ttl {
		return k, nil
	}
	if k == nil && snap != nil && kid == "" {
		// A token with no kid and a set of several keys: which one is not for a refresh to settle.
		return nil, fail(ErrKey, "the token names no kid and the key set holds more than one key")
	}
	fresh, why := s.refresh(ctx, snap)
	if why == "" {
		if k2 := fresh.lookup(kid); k2 != nil {
			return k2, nil
		}
		return nil, fail(ErrKey, "the key set holds no key with the token's kid")
	}
	if k != nil {
		return k, nil // a stale key is better than none while the issuer cannot be reached
	}
	if snap == nil {
		return nil, fail(ErrKey, "the key set has not been fetched: "+why)
	}
	return nil, fail(ErrKey, "the key set holds no key with the token's kid and could not be refreshed: "+why)
}

// refresh makes the one fetch a caller asked for, unless another caller has just done it for them (seen is no longer the current snapshot) or the last attempt was less than
// minRefresh ago. It returns the current snapshot and "" when that snapshot is fresh, and the reason it could not be made fresh otherwise.
func (s *keySource) refresh(ctx context.Context, seen *keySnapshot) (*keySnapshot, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.snap.Load(); cur != seen {
		return cur, ""
	}
	now := s.now()
	if s.attempted && now.Sub(s.lastAttempt) < s.minRefresh {
		why := "the key set was fetched less than " + s.minRefresh.String() + " ago"
		if s.lastFailure != "" {
			why += " and that fetch failed: " + s.lastFailure
		}
		return s.snap.Load(), why
	}
	s.attempted, s.lastAttempt = true, now
	snap, err := s.fetch(ctx, now)
	if err != nil {
		s.lastFailure = err.Error()
		return s.snap.Load(), "the fetch failed: " + s.lastFailure
	}
	s.lastFailure = ""
	s.snap.Store(snap)
	return snap, ""
}

// keySetError is a failed fetch or an unusable key set. Its text is a fixed sentence about the response, never the response.
type keySetError string

func (e keySetError) Error() string { return string(e) }

// fetch gets and parses the key set. One caller's cancelled request must not fail the fetch every waiting caller shares, so the request outlives ctx's cancellation and is bounded by
// its own timeout.
func (s *keySource) fetch(ctx context.Context, now time.Time) (*keySnapshot, error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, keySetError("the request could not be built")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, keySetError(classifyFetchError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, keySetError(fmt.Sprintf("the server answered %d: redirects are not followed", resp.StatusCode))
		}
		return nil, keySetError(fmt.Sprintf("the server answered %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxKeySetBytes+1))
	if err != nil {
		return nil, keySetError("the response could not be read")
	}
	if len(body) > MaxKeySetBytes {
		return nil, keySetError("the response is larger than 1 MiB")
	}
	return parseKeySet(body, now)
}

// classifyFetchError names the kind of a transport failure without repeating the error, which carries the URL.
func classifyFetchError(err error) string {
	var certErr *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.As(err, &certErr):
		return "the server's certificate was not accepted"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "the request timed out"
	}
	return "the request failed"
}

// parseKeySet reads a JWKS document. A key it cannot use (another type, another curve, an RSA modulus under MinRSABits, an encryption key, a key whose alg does not fit) is skipped
// and counted; the set is refused when nothing usable is left, when two keys share a kid, or when it is not a JWKS at all. A key with no kid is kept only when it is the one usable key.
func parseKeySet(body []byte, fetchedAt time.Time) (*keySnapshot, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, keySetError("the key set is not a JSON object")
	}
	if len(doc.Keys) == 0 {
		return nil, keySetError("the key set holds no keys")
	}
	if len(doc.Keys) > maxKeysInSet {
		return nil, keySetError("the key set holds too many keys")
	}
	skipped := map[string]bool{}
	var usable []*jwk
	for _, raw := range doc.Keys {
		k, why := parseKey(raw)
		if k == nil {
			skipped[why] = true
			continue
		}
		usable = append(usable, k)
	}
	if len(usable) > 1 {
		kept := usable[:0:0]
		for _, k := range usable {
			if k.kid == "" {
				skipped["a key without a kid in a set of several"] = true
				continue
			}
			kept = append(kept, k)
		}
		usable = kept
	}
	if len(usable) == 0 {
		reasons := make([]string, 0, len(skipped))
		for r := range skipped {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		return nil, keySetError("the key set holds no usable key (" + strings.Join(reasons, "; ") + ")")
	}
	snap := &keySnapshot{keys: usable, byKID: map[string]*jwk{}, fetchedAt: fetchedAt}
	for _, k := range usable {
		if k.kid == "" {
			continue
		}
		if _, dup := snap.byKID[k.kid]; dup {
			return nil, keySetError("two keys in the key set share a kid")
		}
		snap.byKID[k.kid] = k
	}
	return snap, nil
}

// keyJSON is the part of a JWK this package reads.
type keyJSON struct {
	Kty    string   `json:"kty"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	Kid    string   `json:"kid"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	KeyOps []string `json:"key_ops"`
}

var rawB64 = base64.RawURLEncoding

// parseKey builds the key of one JWK, or says in a fixed sentence why it cannot be used.
func parseKey(raw json.RawMessage) (*jwk, string) {
	var j keyJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, "an entry is not a key object"
	}
	if j.Use != "" && j.Use != "sig" {
		return nil, "a key is not for signatures"
	}
	if len(j.KeyOps) > 0 && !slices.Contains(j.KeyOps, "verify") {
		return nil, "a key is not for verification"
	}
	switch j.Kty {
	case "EC":
		if j.Crv != "P-256" {
			return nil, "an EC key on a curve other than P-256"
		}
		if j.Alg != "" && j.Alg != "ES256" {
			return nil, "a key's alg does not fit its type"
		}
		x, errX := rawB64.DecodeString(j.X)
		y, errY := rawB64.DecodeString(j.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			return nil, "an EC key with malformed coordinates"
		}
		point := append(append([]byte{4}, x...), y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			return nil, "an EC point that is not on the curve"
		}
		return &jwk{kid: j.Kid, alg: "ES256", ec: pub}, ""
	case "RSA":
		if j.Alg != "" && j.Alg != "RS256" {
			return nil, "a key's alg does not fit its type"
		}
		nb, errN := rawB64.DecodeString(j.N)
		eb, errE := rawB64.DecodeString(j.E)
		if errN != nil || errE != nil {
			return nil, "an RSA key with a malformed modulus or exponent"
		}
		n, e := new(big.Int).SetBytes(nb), new(big.Int).SetBytes(eb)
		switch {
		case n.BitLen() < MinRSABits:
			return nil, "an RSA modulus under 2048 bits"
		case n.BitLen() > maxRSABits:
			return nil, "an RSA modulus over 8192 bits"
		case n.Bit(0) == 0:
			return nil, "an even RSA modulus"
		case !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0:
			return nil, "an RSA exponent outside the accepted range"
		}
		return &jwk{kid: j.Kid, alg: "RS256", rsa: &rsa.PublicKey{N: n, E: int(e.Int64())}}, ""
	}
	return nil, "a key of a type that is not used (only EC and RSA keys are)"
}
