package config_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/config"
)

// The consumer of the five token-verification dials (Phase 4 B14a; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"): PLATFORM_JWT_SECRET, PLATFORM_JWKS_URL,
// PLATFORM_JWT_ISSUER, PLATFORM_JWT_AUDIENCE and PLATFORM_JWT_LEEWAY_SEC are read by Load into Config and handed to internal/auth by auth.FromConfig, which internal/app calls at start-up. The
// tests below prove the whole path with tokens signed here by code that does not use internal/auth: each dial changes what the verifier built from the Config accepts, and an invalid value
// stops the process at start-up with exit status 2.

const jwtSub = "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90"

var jwtNow = time.Unix(1_000_000_000, 0).UTC()

func jwtClock() time.Time { return jwtNow }

// jwtSecretText is a secret made for this run: 48 hexadecimal characters.
func jwtSecretText(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func jwtSeg(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// jwtClaims is a good set of claims at jwtNow, with the overrides applied.
func jwtClaims(over map[string]any) map[string]any {
	c := map[string]any{"sub": jwtSub, "iss": "https://issuer.example.test/auth/v1", "aud": "authenticated", "exp": jwtNow.Unix() + 3600, "iat": jwtNow.Unix() - 5}
	for k, v := range over {
		c[k] = v
	}
	return c
}

// hs256 signs claims with an HMAC written out here, not taken from internal/auth.
func hs256(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	in := jwtSeg(t, map[string]any{"alg": "HS256", "typ": "JWT"}) + "." + jwtSeg(t, claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(in))
	return in + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// es256 signs claims with an ECDSA signature written out here (R then S, 32 bytes each).
func es256(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	in := jwtSeg(t, map[string]any{"alg": "ES256", "typ": "JWT", "kid": kid}) + "." + jwtSeg(t, claims)
	h := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
}

func jwksServing(t *testing.T, key *ecdsa.PrivateKey, kid string) (url string, hits *atomic.Int32) {
	t.Helper()
	pt, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(pt[1:33]), "y": base64.RawURLEncoding.EncodeToString(pt[33:]),
	}}})
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write(doc) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/auth/v1/.well-known/jwks.json", hits
}

func verifyWith(t *testing.T, env map[string]string, token string, opts ...auth.Option) error {
	t.Helper()
	cfg, err := config.Load(lockEnv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v, err := auth.FromConfig(cfg, append([]auth.Option{auth.WithNow(jwtClock)}, opts...)...)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if v == nil {
		t.Fatal("FromConfig returned no verifier")
	}
	_, err = v.Verify(context.Background(), token)
	return err
}

func TestJWTDials_DefaultsAndEdgesLoadAsDocumented(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWTSecret != "" || cfg.JWKSURL != "" || cfg.JWTIssuer != "" || cfg.JWTAudience != "authenticated" || cfg.JWTLeeway != 30*time.Second {
		t.Fatalf("an environment without the dials loaded as %v", cfg)
	}
	// With no key source the service starts, and FromConfig builds nothing (the job API is not served).
	if v, err := auth.FromConfig(cfg); v != nil || err != nil {
		t.Fatalf("FromConfig with no key source = %v, %v, want nil, nil", v, err)
	}
	secret := jwtSecretText(t)
	for _, c := range []struct {
		name string
		env  map[string]string
		ok   func(config.Config) bool
	}{
		{"a secret of exactly 32 bytes", map[string]string{config.VarJWTSecret: secret[:32]}, func(c config.Config) bool { return c.JWTSecret.Reveal() == secret[:32] }},
		{"a secret with space around it (trimmed)", map[string]string{config.VarJWTSecret: "  " + secret + "\n"}, func(c config.Config) bool { return c.JWTSecret.Reveal() == secret }},
		{"the unset marker", map[string]string{config.VarJWTSecret: "(unset)", config.VarJWKSURL: "(unset)", config.VarJWTIssuer: "(unset)"}, func(c config.Config) bool {
			return c.JWTSecret == "" && c.JWKSURL == "" && c.JWTIssuer == ""
		}},
		{"blank values", map[string]string{config.VarJWTSecret: " ", config.VarJWTAudience: " ", config.VarJWTLeewaySec: "  "}, func(c config.Config) bool {
			return c.JWTSecret == "" && c.JWTAudience == "authenticated" && c.JWTLeeway == 30*time.Second
		}},
		{"leeway 0 (none: not the default)", map[string]string{config.VarJWTLeewaySec: "0"}, func(c config.Config) bool { return c.JWTLeeway == 0 }},
		{"leeway 300", map[string]string{config.VarJWTLeewaySec: "300"}, func(c config.Config) bool { return c.JWTLeeway == 5*time.Minute }},
		{"an issuer of 512 bytes", map[string]string{config.VarJWTIssuer: strings.Repeat("i", 512)}, func(c config.Config) bool { return len(c.JWTIssuer) == 512 }},
		{"an audience of 512 bytes", map[string]string{config.VarJWTAudience: strings.Repeat("a", 512)}, func(c config.Config) bool { return len(c.JWTAudience) == 512 }},
		{"an http JWKS URL (a URL; the verifier decides about http)", map[string]string{config.VarJWKSURL: "http://gotrue:9999/.well-known/jwks.json"}, func(c config.Config) bool {
			return c.JWKSURL == "http://gotrue:9999/.well-known/jwks.json"
		}},
	} {
		got, err := config.Load(lockEnv(c.env))
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if !c.ok(got) {
			t.Errorf("%s: loaded as %v", c.name, got)
		}
	}
}

// TestJWTDials_EachReachesTheVerifier is the consumer proof: for each dial, the same token gets a different verdict from the verifier built from a Config that differs in that dial only.
func TestJWTDials_EachReachesTheVerifier(t *testing.T) {
	secret, other := jwtSecretText(t), jwtSecretText(t)
	good := hs256(t, secret, jwtClaims(nil))

	// PLATFORM_JWT_SECRET: the secret that is configured is the secret that is checked.
	if err := verifyWith(t, map[string]string{config.VarJWTSecret: secret}, good); err != nil {
		t.Errorf("a token under the configured secret was refused: %v", err)
	}
	if err := verifyWith(t, map[string]string{config.VarJWTSecret: other}, good); !errors.Is(err, auth.ErrSignature) {
		t.Errorf("a token under another secret: %v, want ErrSignature", err)
	}

	// PLATFORM_JWT_ISSUER: pinned, the issuer must match; unset, any issuer does (the control).
	for name, c := range map[string]struct {
		env  map[string]string
		want error
	}{
		"the issuer the token carries": {map[string]string{config.VarJWTIssuer: "https://issuer.example.test/auth/v1"}, nil},
		"another issuer":               {map[string]string{config.VarJWTIssuer: "https://other.example.test/auth/v1"}, auth.ErrIssuer},
		"no issuer configured":         {nil, nil},
	} {
		err := verifyWith(t, merge(map[string]string{config.VarJWTSecret: secret}, c.env), good)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("issuer dial, %s: %v, want %v", name, err, c.want)
		}
	}

	// PLATFORM_JWT_AUDIENCE: the default is the token's authenticated; another value is what the token must carry instead.
	platform := hs256(t, secret, jwtClaims(map[string]any{"aud": []string{"x", "platform"}}))
	for name, c := range map[string]struct {
		env   map[string]string
		token string
		want  error
	}{
		"the default audience, a token for it":       {nil, good, nil},
		"the default audience, a token for another":  {nil, platform, auth.ErrAudience},
		"audience platform, a token for it":          {map[string]string{config.VarJWTAudience: "platform"}, platform, nil},
		"audience platform, a token for the default": {map[string]string{config.VarJWTAudience: "platform"}, good, auth.ErrAudience},
	} {
		err := verifyWith(t, merge(map[string]string{config.VarJWTSecret: secret}, c.env), c.token)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("audience dial, %s: %v, want %v", name, err, c.want)
		}
	}

	// PLATFORM_JWT_LEEWAY_SEC: a token that expired ten seconds ago is valid with a leeway above ten seconds and expired at ten and below (expired when now >= exp + leeway).
	expired := hs256(t, secret, jwtClaims(map[string]any{"exp": jwtNow.Unix() - 10}))
	for leeway, want := range map[string]error{"": nil, "30": nil, "11": nil, "10": auth.ErrExpired, "5": auth.ErrExpired, "0": auth.ErrExpired} {
		env := map[string]string{config.VarJWTSecret: secret}
		if leeway != "" {
			env[config.VarJWTLeewaySec] = leeway
		}
		err := verifyWith(t, env, expired)
		if (want == nil) != (err == nil) || (want != nil && !errors.Is(err, want)) {
			t.Errorf("leeway dial %q: an exp ten seconds behind gave %v, want %v", leeway, err, want)
		}
	}

	// PLATFORM_JWKS_URL: the key set that is named is the key set that is read; the verifier is built without touching it, and a second URL with another key refuses the same token.
	keyA, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	urlA, hitsA := jwksServing(t, keyA, "key-1")
	urlB, hitsB := jwksServing(t, keyB, "key-1")
	token := es256(t, keyA, "key-1", jwtClaims(nil))
	if err := verifyWith(t, map[string]string{config.VarJWKSURL: urlA}, token, auth.WithAllowInsecureHTTP()); err != nil {
		t.Errorf("a token signed by the key at the configured URL was refused: %v", err)
	}
	if hitsA.Load() != 1 {
		t.Errorf("the configured URL was asked %d times, want 1", hitsA.Load())
	}
	if err := verifyWith(t, map[string]string{config.VarJWKSURL: urlB}, token, auth.WithAllowInsecureHTTP()); !errors.Is(err, auth.ErrSignature) || hitsB.Load() != 1 {
		t.Errorf("the same token against another URL's key: %v (%d requests), want ErrSignature after one", err, hitsB.Load())
	}
	// The control: without the harness's option an http URL is a configuration error naming the variable (the URL itself is a valid value for Load).
	cfg, err := config.Load(lockEnv(map[string]string{config.VarJWKSURL: urlA}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = auth.FromConfig(cfg)
	var ce *config.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarJWKSURL || strings.Contains(err.Error(), urlA) {
		t.Errorf("an http JWKS URL without the harness's option: %v, want a *config.Error naming %s and not echoing the URL", err, config.VarJWKSURL)
	}
	// And an HS256 token is not accepted when only a key set is configured (and the reverse): each source takes its own algorithm only.
	if err := verifyWith(t, map[string]string{config.VarJWKSURL: urlA}, good, auth.WithAllowInsecureHTTP()); !errors.Is(err, auth.ErrAlgorithm) {
		t.Errorf("an HS256 token at a verifier with only a key set: %v, want ErrAlgorithm", err)
	}
	if err := verifyWith(t, map[string]string{config.VarJWTSecret: secret}, token); !errors.Is(err, auth.ErrAlgorithm) {
		t.Errorf("an ES256 token at a verifier with only a secret: %v, want ErrAlgorithm", err)
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TestJWTDials_InvalidValuesStopTheProcessWithExitStatus2: the entry point's own check, not only Load's. The controls are the accepted values of the same variables.
func TestJWTDials_InvalidValuesStopTheProcessWithExitStatus2(t *testing.T) {
	secret := jwtSecretText(t)
	for _, c := range []struct{ name, value string }{
		{config.VarJWTSecret, secret[:31]},
		{config.VarJWTSecret, secret[:20] + " " + secret[20:]},
		{config.VarJWKSURL, "ftp://idp-XYZ123/jwks.json"},
		{config.VarJWKSURL, "https://user-XYZ123:pw-XYZ123@idp.example.test/jwks.json"},
		{config.VarJWTIssuer, "two words XYZ123"},
		{config.VarJWTAudience, "two words XYZ123"},
		{config.VarJWTLeewaySec, "301"},
		{config.VarJWTLeewaySec, "-1"},
		{config.VarJWTLeewaySec, "soon-XYZ123"},
	} {
		err := app.Run(context.Background(), lockEnv(map[string]string{c.name: c.value}), io.Discard)
		var ce *config.Error
		if !errors.As(err, &ce) {
			t.Errorf("%s=%q: Run = %v, want a *config.Error", c.name, c.value, err)
			continue
		}
		if got := app.ExitCode(err); got != 2 {
			t.Errorf("%s=%q: exit status %d, want 2", c.name, c.value, got)
		}
		if len(ce.Problems) != 1 || ce.Problems[0].Name != c.name {
			t.Errorf("%s=%q: problems %+v, want exactly one, naming the variable", c.name, c.value, ce.Problems)
		}
		if strings.Contains(err.Error(), c.value) {
			t.Errorf("%s=%q: the error echoes the value: %v", c.name, c.value, err)
		}
	}
	for name, env := range map[string]map[string]string{
		"the secret at the floor": {config.VarJWTSecret: secret[:32]},
		"the leeway at zero":      {config.VarJWTLeewaySec: "0"},
		"an https URL":            {config.VarJWKSURL: "https://idp.example.test/auth/v1/.well-known/jwks.json"},
	} {
		if _, err := config.Load(lockEnv(env)); err != nil {
			t.Errorf("control, %s: refused: %v", name, err)
		}
	}
}

// TestJWTSecretDial_IsNeverPrinted: no way the standard library prints a Config, a Secret or a log record shows the value, and an unset dial says so without a value. The paired control is a
// plain string field, which leaks through the same sinks.
func TestJWTSecretDial_IsNeverPrinted(t *testing.T) {
	canary := "CANARY-jwt-" + jwtSecretText(t)
	cfg, err := config.Load(lockEnv(map[string]string{config.VarJWTSecret: canary}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWTSecret.Reveal() != canary {
		t.Fatal("control failed: Reveal() does not return the loaded value")
	}
	sinks := map[string]string{
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
		"Sprintln": fmt.Sprintln(cfg, &cfg),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		sinks["Config "+verb] = fmt.Sprintf(verb, cfg)
		sinks["Secret "+verb] = fmt.Sprintf(verb, cfg.JWTSecret)
	}
	for _, v := range []any{cfg, &cfg, cfg.JWTSecret} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sinks[fmt.Sprintf("json %T", v)] = string(b)
	}
	for _, h := range []string{"json", "text"} {
		var buf bytes.Buffer
		var handler slog.Handler = slog.NewJSONHandler(&buf, nil)
		if h == "text" {
			handler = slog.NewTextHandler(&buf, nil)
		}
		slog.New(handler).Info("loaded", "config", cfg, "secret", cfg.JWTSecret)
		sinks["slog "+h] = buf.String()
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		if strings.Contains(out, canary) || strings.Contains(out, canary[len("CANARY-"):]) {
			t.Errorf("%s reveals the JWT secret: %s", name, out)
		}
	}
	if !strings.Contains(cfg.String(), "jwt_secret=[redacted]") {
		t.Errorf("a set secret is not shown as the redaction marker: %s", cfg.String())
	}
	if unset, err := config.Load(lockEnv(nil)); err != nil || !strings.Contains(unset.String(), "jwt_secret=(unset)") {
		t.Errorf("an unset secret is not shown as (unset): %v (%v)", unset.String(), err)
	}
	// The verifier built from it does not carry the secret into its summary or its refusals either.
	v, err := auth.FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if text := fmt.Sprintf("%v %+v", v.Summary(), v.Summary()); strings.Contains(text, canary) {
		t.Errorf("the verifier's summary carries the secret: %s", text)
	}
	if _, err := v.Verify(context.Background(), hs256(t, canary+"x", jwtClaims(nil))); err == nil || strings.Contains(err.Error(), canary) {
		t.Errorf("a refusal carries the secret or the verifier accepted a forged token: %v", err)
	}
	// The control: a plain string field of the same shape DOES leak through the same sinks.
	type naive struct{ JWTSecret string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{canary})
	if !strings.Contains(buf.String(), canary) || !strings.Contains(fmt.Sprintf("%+v", naive{canary}), canary) {
		t.Fatal("control failed: a plain string field did not reach the sinks, so the checks above prove nothing")
	}
}
