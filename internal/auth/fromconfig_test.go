package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// loadCfg is a Config as the process loads it: through config.Load, from a map.
func loadCfg(t *testing.T, extra map[string]string) config.Config {
	t.Helper()
	env := map[string]string{
		config.VarDatabaseURL:  "postgres://platform_app:pw@db.example:5432/postgres",
		config.VarM3URL:        "http://m3.example:8080",
		config.VarM3AdminToken: "operator-operator-ok",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestFromConfig_BuildsTheVerifierTheDialsDescribe(t *testing.T) {
	// No key source: no verifier and no error, whatever else is set.
	for name, extra := range map[string]map[string]string{
		"nothing set":                     nil,
		"issuer, audience and leeway set": {config.VarJWTIssuer: testIssuer, config.VarJWTAudience: "platform", config.VarJWTLeewaySec: "5"},
	} {
		v, err := FromConfig(loadCfg(t, extra))
		if v != nil || err != nil {
			t.Errorf("%s: FromConfig = %v, %v, want nil, nil", name, v, err)
		}
	}

	secret := randText(t, 48)
	// The secret alone: HS256 with the defaults.
	v, err := FromConfig(loadCfg(t, map[string]string{config.VarJWTSecret: secret}))
	if err != nil || v == nil {
		t.Fatalf("with the secret: %v, %v", v, err)
	}
	if s := v.Summary(); !s.HMAC || s.JWKS || s.IssuerPinned || s.Audience != "authenticated" || s.Leeway != 30*time.Second {
		t.Errorf("the secret alone: %+v", s)
	}
	// Each dial reaches the verifier: the issuer, the audience and the leeway change what it accepts (the paired controls are the defaults beside them).
	clk := newClock()
	tokenFor := func(claims map[string]any) string { return mint(t, hmacSigner{[]byte(secret)}, "", claims) }
	cfg := loadCfg(t, map[string]string{config.VarJWTSecret: secret, config.VarJWTIssuer: testIssuer, config.VarJWTAudience: "platform", config.VarJWTLeewaySec: "0"})
	v, err = FromConfig(cfg, WithNow(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.Summary(); !s.IssuerPinned || s.Audience != "platform" || s.Leeway != 0 {
		t.Errorf("the dials did not reach the verifier: %+v", s)
	}
	good := with(claimsAt(clk.Now()), "aud", "platform")
	for name, c := range map[string]struct {
		claims map[string]any
		want   error
	}{
		"the configured issuer and audience":    {good, nil},
		"another issuer":                        {with(good, "iss", "https://elsewhere.example.test"), ErrIssuer},
		"the default audience instead":          {with(good, "aud", "authenticated"), ErrAudience},
		"exp == now: no leeway at all (dial 0)": {with(good, "exp", clk.Now().Unix()), ErrExpired},
		"exp one second ahead":                  {with(good, "exp", clk.Now().Unix()+1), nil},
	} {
		_, err := verify(v, tokenFor(c.claims))
		wantErr(t, name, err, c.want)
	}
	// The default leeway is 30 s, the dial's other end 300 s.
	for leeway, behind := range map[string]int64{"30": 29, "300": 299, "1": 0} {
		v, err := FromConfig(loadCfg(t, map[string]string{config.VarJWTSecret: secret, config.VarJWTLeewaySec: leeway}), WithNow(clk.Now))
		if err != nil {
			t.Fatal(err)
		}
		_, err = verify(v, tokenFor(with(claimsAt(clk.Now()), "exp", clk.Now().Unix()-behind)))
		wantErr(t, fmt.Sprintf("leeway %s s: exp %d s behind", leeway, behind), err, nil)
	}
	// The secret that is configured is the secret that is checked.
	_, err = verify(v, mint(t, hmacSigner{[]byte(randText(t, 48))}, "", good))
	wantErr(t, "a token under another secret", err, ErrSignature)

	// The URL alone: ES256 and RS256 from the key set, and the verifier is built without touching the network.
	srv := newJWKSServer(t, nil)
	cfg = loadCfg(t, map[string]string{config.VarJWKSURL: srv.url()})
	v, err = FromConfig(cfg, WithAllowInsecureHTTP(), WithNow(clk.Now))
	if err != nil || v == nil || !v.Summary().JWKS || v.Summary().HMAC || srv.hits.Load() != 0 {
		t.Fatalf("with the URL: %v, %v (%d requests)", v, err, srv.hits.Load())
	}
	// Both.
	cfg = loadCfg(t, map[string]string{config.VarJWKSURL: srv.url(), config.VarJWTSecret: secret})
	if v, err = FromConfig(cfg, WithAllowInsecureHTTP()); err != nil || !v.Summary().JWKS || !v.Summary().HMAC {
		t.Fatalf("with both: %v, %v", v, err)
	}
	// An https URL needs no option.
	if v, err = FromConfig(loadCfg(t, map[string]string{config.VarJWKSURL: "https://project.example.test/auth/v1/.well-known/jwks.json"})); err != nil || v == nil {
		t.Errorf("an https URL: %v, %v", v, err)
	}

	// A combination New refuses is a *config.Error naming the variable, with a fixed reason and no value: an http URL, here, which Load accepts (it is a URL) and the verifier does not.
	httpURL := "http://project.example.test/auth/v1/.well-known/jwks.json"
	_, err = FromConfig(loadCfg(t, map[string]string{config.VarJWKSURL: httpURL}))
	var ce *config.Error
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarJWKSURL {
		t.Fatalf("an http JWKS URL: %v, want a *config.Error naming %s", err, config.VarJWKSURL)
	}
	if strings.Contains(err.Error(), httpURL) || strings.Contains(err.Error(), "project.example.test") {
		t.Errorf("the refusal repeats the URL: %v", err)
	}
	// Even for the harness: the flag opens http to a test host only.
	_, err = FromConfig(loadCfg(t, map[string]string{config.VarJWKSURL: httpURL}), WithAllowInsecureHTTP())
	if !errors.As(err, &ce) {
		t.Errorf("the flag opened http to a public name: %v", err)
	}
	// The control: the same URL as https.
	if _, err = FromConfig(loadCfg(t, map[string]string{config.VarJWKSURL: strings.Replace(httpURL, "http:", "https:", 1)})); err != nil {
		t.Errorf("the https twin was refused: %v", err)
	}
}

func TestFromConfig_TheLimitsAreTheConfigsLimits(t *testing.T) {
	// The two packages state the secret's floor once each; they must agree.
	if MinHMACSecretBytes != config.MinJWTSecretBytes {
		t.Errorf("auth.MinHMACSecretBytes = %d, config.MinJWTSecretBytes = %d", MinHMACSecretBytes, config.MinJWTSecretBytes)
	}
	if MaxLeeway != 300*time.Second {
		t.Errorf("auth.MaxLeeway = %v, the dial's top is 300 s", MaxLeeway)
	}
	if DefaultAudience != "authenticated" || DefaultLeeway != 30*time.Second {
		t.Errorf("the defaults are %q and %v", DefaultAudience, DefaultLeeway)
	}
	// The dials' defaults are the verifier's defaults: a Config loaded from nothing, given a secret, builds the verifier a bare New would.
	cfg := loadCfg(t, map[string]string{config.VarJWTSecret: randText(t, 40)})
	if cfg.JWTAudience != DefaultAudience || cfg.JWTLeeway != DefaultLeeway {
		t.Errorf("the dials default to %q and %v, the verifier to %q and %v", cfg.JWTAudience, cfg.JWTLeeway, DefaultAudience, DefaultLeeway)
	}
	// A Config built by hand and not by Load is judged the same way: no leeway is none, not the default.
	hand := config.Config{JWTSecret: config.Secret(randText(t, 40))}
	v, err := FromConfig(hand)
	if err != nil || v.Summary().Leeway != 0 || v.Summary().Audience != DefaultAudience {
		t.Errorf("a hand-built Config: %+v, %v", v, err)
	}
	// A hand-built Config with a weak secret is refused with the variable named, and Load never lets it get this far.
	weak := randText(t, 10)
	_, err = FromConfig(config.Config{JWTSecret: config.Secret(weak)})
	var ce *config.Error
	if !errors.As(err, &ce) || ce.Problems[0].Name != config.VarJWTSecret || strings.Contains(err.Error(), weak) {
		t.Errorf("a weak secret in a hand-built Config: %v", err)
	}
}

// TestAllowInsecureHTTP_NoProductionCodeSetsIt is the gate behind "the harness sets it and the production wiring never does": no non-test Go file under cmd/ or internal/, outside
// this package, may name the option or its FromConfig form. The scan prints what it covered, fails when it covered nothing, and has a control that finds a planted mention.
func TestAllowInsecureHTTP_NoProductionCodeSetsIt(t *testing.T) {
	root := filepath.Join("..", "..")
	own, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	needles := []string{"AllowInsecureHTTP", "WithAllowInsecureHTTP"}

	scan := func(dirs []string, skipDir string) (files int, hits []string) {
		for _, d := range dirs {
			_ = filepath.WalkDir(d, func(path string, e fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if e.IsDir() {
					if abs, _ := filepath.Abs(path); skipDir != "" && abs == skipDir {
						return filepath.SkipDir
					}
					return nil
				}
				if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				files++
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, n := range needles {
					if strings.Contains(string(b), n) {
						hits = append(hits, path+" names "+n)
					}
				}
				return nil
			})
		}
		return files, hits
	}

	files, hits := scan([]string{filepath.Join(root, "cmd"), filepath.Join(root, "internal")}, own)
	if files < 20 {
		t.Fatalf("the scan found %d non-test Go files under cmd/ and internal/: the instrument matched (almost) nothing", files)
	}
	if len(hits) != 0 {
		t.Errorf("production code names the insecure-HTTP option, which only tests and the harness may: %v", hits)
	}
	t.Logf("scanned %d non-test Go files under cmd/ and internal/ (internal/auth itself excluded); tests/ and every _test.go are not checked", files)

	// The control: a file that does name it is found, so the clean result above can fail.
	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, "wiring"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "wiring", "wiring.go"), []byte("package wiring\n\nvar _ = auth.WithAllowInsecureHTTP()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "wiring", "wiring_test.go"), []byte("package wiring\n\nvar _ = auth.WithAllowInsecureHTTP()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, ph := scan([]string{planted}, "")
	if pf != 1 || len(ph) != 2 {
		t.Errorf("control failed: the scan of a planted production file found %d files and %v (want 1 file, 2 needles); a _test.go file must not be counted", pf, ph)
	}
}
