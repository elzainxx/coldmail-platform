// Package auth verifies the access tokens Supabase issues to a signed-in user: the foundation of the platform's HTTP API (Docs/phase-4-spec.md, Locked decision 29).
//
// WHAT A VERIFIED TOKEN MEANS. Verify answers one question: was this token signed by a key this deployment trusts, and is it inside its validity window, addressed to this
// audience and (when one is configured) issued by this issuer. The answer is a Claims value whose Subject is the user's auth id, a canonical lower-case uuid. Role and Email are
// carried and NEVER trusted for authorisation here: the API resolves Subject to a platform user through tenant.Resolver.ByAuthUser, and nothing but the verified Subject names the user.
//
// THE ORDER OF THE CHECKS, each with its own sentinel error (errors.Is):
//
//  1. structure: three base64url segments, no padding, at most MaxTokenBytes in all; the header and the payload are JSON objects without a repeated key (ErrMalformed)
//  2. algorithm: an allow-list that follows the configured key sources, nothing else (ErrAlgorithm)
//  3. signature (ErrSignature); a kid the key set does not hold is ErrKey after at most one refresh of the set
//  4. time: exp required, nbf and iat when present, with a leeway (ErrExpired, ErrNotYetValid)
//  5. iss when an issuer is configured, aud always (ErrIssuer, ErrAudience)
//  6. sub, a canonical lower-case hyphenated uuid (ErrSubject)
//
// ALGORITHM CONFUSION. The allow-list is per key source. An HMAC secret allows HS256 and nothing else; a JWKS URL allows ES256 and RS256 and nothing else; "none", every other
// name and every case variant of a name ("None", "hs256") are refused. A key taken from a JWKS is a typed public key (ecdsa or rsa) that can only verify an asymmetric signature, so it
// can never be the key of an HMAC: a token whose header says HS256 and that was MACed with the bytes of a published public key is refused as ErrAlgorithm when no secret is configured
// and as ErrSignature when one is. A header's jku, x5u, x5c, jwk and crit are never read: no key is ever fetched from, or trusted because of, what the token says.
//
// ERROR TEXT. Every error carries a fixed sentence. It never holds the token or a part of it, a claim, a kid or a header value: an attacker chooses all of those, and a log line is
// the place they would land.
//
// NOT DONE HERE, on purpose: revocation (a signed-out session stays valid until exp; the platform's own inactive-user check is the control), the role and the email as authority, any
// algorithm beyond HS256, ES256 and RS256 (Supabase issues those), and the "crit" header's processing rule (the owner's brief says it is ignored, as are the other key-carrying headers).
package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The sentinels. Test them with errors.Is; the wrapped error says why in a fixed sentence.
var (
	ErrMalformed   = errors.New("auth: malformed token")
	ErrAlgorithm   = errors.New("auth: algorithm not accepted")
	ErrSignature   = errors.New("auth: signature does not verify")
	ErrKey         = errors.New("auth: no key to verify the token with")
	ErrExpired     = errors.New("auth: token expired")
	ErrNotYetValid = errors.New("auth: token not yet valid")
	ErrIssuer      = errors.New("auth: issuer not accepted")
	ErrAudience    = errors.New("auth: audience not accepted")
	ErrSubject     = errors.New("auth: subject not valid")
)

// The defaults and limits of the verifier.
const (
	// DefaultAudience is the audience Supabase puts in the access token of a signed-in user.
	DefaultAudience = "authenticated"
	// DefaultLeeway is the clock skew tolerated between the issuer and this process; MaxLeeway is the most Options.Leeway may ask for.
	DefaultLeeway = 30 * time.Second
	MaxLeeway     = 5 * time.Minute
	// NoLeeway, as Options.Leeway, means no leeway at all (the zero value means DefaultLeeway).
	NoLeeway time.Duration = -1
	// DefaultMinRefreshInterval is how soon after one fetch of the key set another may be started.
	DefaultMinRefreshInterval = 30 * time.Second
	// DefaultKeyTTL is how long a fetched key set is trusted without being fetched again: a key the issuer has withdrawn stops verifying after this, even when no token ever
	// names an unknown kid.
	DefaultKeyTTL = time.Hour
	// MinHMACSecretBytes is the shortest HMAC secret New accepts: the size of the SHA-256 output the signature is made of.
	MinHMACSecretBytes = 32
	// MaxTokenBytes is the longest token Verify reads; a longer one is ErrMalformed before anything is decoded.
	MaxTokenBytes = 8 << 10
	// MaxKeySetBytes is the most of a key-set response that is read.
	MaxKeySetBytes = 1 << 20
	// MinRSABits is the smallest RSA modulus a JWKS key may have.
	MinRSABits = 2048

	maxRSABits   = 8192
	maxKeysInSet = 100
	fetchTimeout = 10 * time.Second
	maxClaimText = 512 // the longest configured issuer or audience

	// maxNumericDate is 9999-12-31T23:59:59Z: a larger exp, nbf or iat is not a date anybody means.
	maxNumericDate = 253402300799
)

// Claims is what a verified token says. Subject is the user's auth id (a canonical lower-case uuid); everything else is carried for the caller's information and is NOT a basis for
// authorisation. The token's other claims are not kept.
type Claims struct {
	Subject   string
	Issuer    string
	Audience  []string
	ExpiresAt time.Time
	NotBefore time.Time // zero when the token has no nbf
	IssuedAt  time.Time // zero when the token has no iat
	Role      string
	Email     string
	SessionID string
}

// Options configures a Verifier. At least one of HMACSecret and JWKSURL is required.
type Options struct {
	// HMACSecret is the HS256 secret (Supabase's "legacy JWT secret"), used as the raw bytes of the string. At least MinHMACSecretBytes. New copies it.
	HMACSecret []byte
	// JWKSURL is where the issuer publishes its public keys (https://<project>.supabase.co/auth/v1/.well-known/jwks.json). It must be https unless AllowInsecureHTTP.
	JWKSURL string
	// HTTPClient fetches the key set. Nil means a client with a 10 s timeout, no proxy, certificate verification on and the system roots. Whatever is given, redirects are refused
	// (the answer of a 3xx is a failed fetch), the cookie jar is dropped, a zero Timeout becomes 10 s, and at most MaxKeySetBytes of a response are read.
	HTTPClient *http.Client
	// AllowInsecureHTTP lets JWKSURL be http, and only for a loopback address, a private address, localhost or a single-label host name (a compose service). The test harness sets it;
	// no production code does (TestAllowInsecureHTTP_NoProductionCodeSetsIt).
	AllowInsecureHTTP bool
	// Issuer, when set, must equal the token's iss exactly.
	Issuer string
	// Audience must be one of the token's aud values; empty means DefaultAudience.
	Audience string
	// Leeway is the clock skew tolerated on exp, nbf and iat: zero means DefaultLeeway, NoLeeway means none, and the most is MaxLeeway.
	Leeway time.Duration
	// MinRefreshInterval is the least time between two fetches of the key set (the first fetch is not held back); zero means DefaultMinRefreshInterval.
	MinRefreshInterval time.Duration
	// KeyTTL is how long a fetched key set is used before it is fetched again; zero means DefaultKeyTTL.
	KeyTTL time.Duration
	// Now is the clock; nil means time.Now. The verifier reads no other clock.
	Now func() time.Time
}

// OptionError is New's refusal of an option. Field names the Options field and Reason is a fixed sentence: neither holds a value, so the error is safe to print.
type OptionError struct {
	Field  string
	Reason string
}

func (e *OptionError) Error() string { return "auth: option " + e.Field + ": " + e.Reason }

func optionErr(field, reason string) error { return &OptionError{Field: field, Reason: reason} }

// verifyError is a sentinel plus a fixed reason.
type verifyError struct {
	kind   error
	reason string
}

func (e *verifyError) Error() string { return e.kind.Error() + ": " + e.reason }
func (e *verifyError) Unwrap() error { return e.kind }

// fail wraps a sentinel with a reason. The reason must be a fixed sentence: never build it from the token.
func fail(kind error, reason string) error { return &verifyError{kind: kind, reason: reason} }

// Verifier verifies access tokens. It is safe for concurrent use.
type Verifier struct {
	hmacSecret []byte
	keys       *keySource // nil when no JWKS URL is configured
	issuer     string
	audience   string
	leeway     time.Duration
	now        func() time.Time
}

// Summary says which key sources and rules a Verifier has, for one start-up log line. It carries no secret and no key.
type Summary struct {
	HMAC         bool // HS256 with a configured secret is accepted
	JWKS         bool // ES256 and RS256 with keys from the key set are accepted
	IssuerPinned bool
	Audience     string
	Leeway       time.Duration
}

// Summary describes the verifier's configuration.
func (v *Verifier) Summary() Summary {
	return Summary{HMAC: len(v.hmacSecret) > 0, JWKS: v.keys != nil, IssuerPinned: v.issuer != "", Audience: v.audience, Leeway: v.leeway}
}

// New builds a Verifier. It does no network I/O: the key set is fetched at the first token that needs it. Every refusal is an *OptionError.
func New(o Options) (*Verifier, error) {
	if len(o.HMACSecret) == 0 && o.JWKSURL == "" {
		return nil, optionErr("KeySource", "at least one key source is required, an HMAC secret or a JWKS URL")
	}
	v := &Verifier{issuer: o.Issuer, audience: o.Audience, now: o.Now}
	if len(o.HMACSecret) > 0 {
		if len(o.HMACSecret) < MinHMACSecretBytes {
			return nil, optionErr("HMACSecret", "the HMAC secret is shorter than the 32 bytes the SHA-256 signature needs")
		}
		v.hmacSecret = bytes.Clone(o.HMACSecret)
	}
	if v.issuer != "" && !validClaimText(v.issuer) {
		return nil, optionErr("Issuer", "the issuer must be printable ASCII with no spaces, at most 512 bytes")
	}
	if v.audience == "" {
		v.audience = DefaultAudience
	}
	if !validClaimText(v.audience) {
		return nil, optionErr("Audience", "the audience must be printable ASCII with no spaces, at most 512 bytes")
	}
	switch {
	case o.Leeway == 0:
		v.leeway = DefaultLeeway
	case o.Leeway == NoLeeway:
		v.leeway = 0
	case o.Leeway < 0 || o.Leeway > MaxLeeway:
		return nil, optionErr("Leeway", "the leeway must be between 0 and 5 minutes")
	default:
		v.leeway = o.Leeway
	}
	if o.MinRefreshInterval < 0 {
		return nil, optionErr("MinRefreshInterval", "the refresh interval cannot be negative")
	}
	if o.KeyTTL < 0 {
		return nil, optionErr("KeyTTL", "the key lifetime cannot be negative")
	}
	if v.now == nil {
		v.now = time.Now
	}
	if o.JWKSURL != "" {
		if err := checkJWKSURL(o.JWKSURL, o.AllowInsecureHTTP); err != nil {
			return nil, err
		}
		minRefresh, ttl := o.MinRefreshInterval, o.KeyTTL
		if minRefresh == 0 {
			minRefresh = DefaultMinRefreshInterval
		}
		if ttl == 0 {
			ttl = DefaultKeyTTL
		}
		v.keys = newKeySource(o.JWKSURL, hardenedClient(o.HTTPClient), v.now, minRefresh, ttl)
	}
	return v, nil
}

// checkJWKSURL accepts an absolute https URL with a host and no credentials, query or fragment; with allowInsecure also an http one, but only to a host a test can own.
func checkJWKSURL(raw string, allowInsecure bool) error {
	const shape = "the JWKS URL must be an absolute URL with a host and no credentials, query or fragment"
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Hostname() == "" {
		return optionErr("JWKSURL", shape)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return optionErr("JWKSURL", "the JWKS URL must be https: a key set fetched in the clear can be replaced on the way")
		}
		if !isTestHost(u.Hostname()) {
			return optionErr("JWKSURL", "an http JWKS URL is accepted only for a loopback or private address, localhost or a single-label host name")
		}
		return nil
	}
	return optionErr("JWKSURL", "the JWKS URL must be https")
}

// isTestHost is the hosts an insecure fetch may name: a loopback or private address, localhost, or a name with no dot (a compose service).
func isTestHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return !strings.Contains(host, ".")
}

// validClaimText accepts 1 to maxClaimText bytes of printable ASCII with no space: what an issuer URL or an audience name is.
func validClaimText(s string) bool {
	if len(s) < 1 || len(s) > maxClaimText {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// Verify checks the raw token (the HTTP layer strips "Bearer ") and returns its claims. On any failure the Claims are the zero value and the error is one of the sentinels above,
// wrapped with a fixed reason.
func (v *Verifier) Verify(ctx context.Context, bearer string) (Claims, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := v.now()
	tok, err := parseToken(bearer)
	if err != nil {
		return Claims{}, err
	}
	if err := v.verifySignature(ctx, tok); err != nil {
		return Claims{}, err
	}
	return v.checkClaims(now, tok.payload)
}

// verifySignature is checks 2 and 3: the algorithm allow-list, then the key and the signature.
func (v *Verifier) verifySignature(ctx context.Context, t *parsedToken) error {
	alg, ok := asString(t.header["alg"])
	if !ok {
		return fail(ErrAlgorithm, "the header names no algorithm")
	}
	switch alg {
	case "HS256":
		if len(v.hmacSecret) == 0 {
			return fail(ErrAlgorithm, "HS256 is not accepted: no HMAC secret is configured")
		}
		mac := hmac.New(sha256.New, v.hmacSecret)
		mac.Write([]byte(t.signingInput))
		if !hmac.Equal(mac.Sum(nil), t.sig) {
			return fail(ErrSignature, "the signature does not match")
		}
		return nil
	case "ES256", "RS256":
		if v.keys == nil {
			return fail(ErrAlgorithm, "an asymmetric algorithm is not accepted: no key set is configured")
		}
		kid := ""
		if raw, present := t.header["kid"]; present {
			if kid, ok = asString(raw); !ok {
				return fail(ErrMalformed, "the kid header is not a string")
			}
		}
		k, err := v.keys.keyFor(ctx, kid)
		if err != nil {
			return err
		}
		if k.alg != alg {
			return fail(ErrAlgorithm, "the key does not fit the token's algorithm")
		}
		return k.verify(t.signingInput, t.sig)
	}
	return fail(ErrAlgorithm, "the algorithm is not accepted")
}

// checkClaims is checks 4 to 6, over a payload whose signature has been verified.
func (v *Verifier) checkClaims(now time.Time, p map[string]json.RawMessage) (Claims, error) {
	var (
		c       Claims
		present bool
		err     error
	)

	// 4. time. exp is required; a token that never expires is refused.
	if c.ExpiresAt, present, err = timeClaim(p, "exp"); err != nil {
		return Claims{}, err
	}
	if !present {
		return Claims{}, fail(ErrMalformed, "the exp claim is missing")
	}
	if !now.Before(c.ExpiresAt.Add(v.leeway)) {
		return Claims{}, fail(ErrExpired, "the token has expired")
	}
	if c.NotBefore, present, err = timeClaim(p, "nbf"); err != nil {
		return Claims{}, err
	}
	if present && now.Add(v.leeway).Before(c.NotBefore) {
		return Claims{}, fail(ErrNotYetValid, "the token is not valid yet (nbf)")
	}
	if c.IssuedAt, present, err = timeClaim(p, "iat"); err != nil {
		return Claims{}, err
	}
	if present && now.Add(v.leeway).Before(c.IssuedAt) {
		return Claims{}, fail(ErrNotYetValid, "the token was issued in the future (iat)")
	}

	// 5. issuer and audience.
	if raw, has := p["iss"]; has {
		s, ok := asString(raw)
		if !ok {
			return Claims{}, fail(ErrIssuer, "the iss claim is not a string")
		}
		c.Issuer = s
	}
	if v.issuer != "" && c.Issuer != v.issuer {
		return Claims{}, fail(ErrIssuer, "the issuer is not the configured one")
	}
	aud, err := audienceOf(p["aud"])
	if err != nil {
		return Claims{}, err
	}
	if !contains(aud, v.audience) {
		return Claims{}, fail(ErrAudience, "the audience does not include the configured one")
	}
	c.Audience = aud

	// 6. subject.
	sub, ok := asString(p["sub"])
	if !ok {
		return Claims{}, fail(ErrSubject, "the sub claim is missing or not a string")
	}
	if !canonicalUUID(sub) {
		return Claims{}, fail(ErrSubject, "the sub claim is not a canonical lower-case uuid")
	}
	c.Subject = sub

	// Carried, never trusted: a claim of the wrong type is simply not carried.
	c.Role, _ = asString(p["role"])
	c.Email, _ = asString(p["email"])
	c.SessionID, _ = asString(p["session_id"])
	return c, nil
}

// audienceOf reads aud, a string or an array of strings.
func audienceOf(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fail(ErrAudience, "the aud claim is missing")
	}
	if s, ok := asString(raw); ok {
		return []string{s}, nil
	}
	var list []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &list) != nil {
		return nil, fail(ErrAudience, "the aud claim is neither a string nor an array of strings")
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := asString(item)
		if !ok {
			return nil, fail(ErrAudience, "the aud claim holds something that is not a string")
		}
		out = append(out, s)
	}
	return out, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// timeClaim reads a NumericDate claim: a JSON number of seconds since the epoch, with or without a fraction. present is false when the claim is absent; a claim that is present and
// is not a finite number in 0..maxNumericDate is ErrMalformed.
func timeClaim(p map[string]json.RawMessage, name string) (t time.Time, present bool, err error) {
	raw, has := p[name]
	if !has {
		return time.Time{}, false, nil
	}
	bad := fail(ErrMalformed, "the "+name+" claim is not a valid date")
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return time.Time{}, true, bad
	}
	f, perr := strconv.ParseFloat(string(raw), 64)
	if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > maxNumericDate {
		return time.Time{}, true, bad
	}
	whole, frac := math.Modf(f)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC(), true, nil
}

// asString decodes raw as a JSON string; anything else, null and an absent value included, is not ok.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// canonicalUUID reports whether s is a hyphenated uuid in lower case: 8-4-4-4-12 hexadecimal digits and nothing else.
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
