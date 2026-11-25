package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// The consumer of the five PLATFORM_JWT* / PLATFORM_JWKS_URL dials in the service itself (Phase 4 B14a): Run builds the verifier at start-up to validate them, says in one log line which
// sources are on, and refuses an invalid combination with exit status 2. internal/config's jwtdials_test.go proves that each dial changes what the verifier accepts.

const (
	jwksHTTPS = "https://project.example.test/auth/v1/.well-known/jwks.json"
	jwksHTTP  = "http://project.example.test/auth/v1/.well-known/jwks.json"
)

func randomSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b) // 48 characters, made for this run
}

func TestRun_JobAPIAuthenticationIsOptionalAndSaysSoWhenAbsent(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	r := startApp(t, base(closedPortDSN(t)))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	lines := r.logs.lines(t)
	if l := find(lines, "job API authentication is not configured"); l == nil {
		t.Errorf("the start-up log does not say the job API's authentication is not configured:\n%s", r.logs.String())
	}
	if find(lines, "job API authentication configured") != nil {
		t.Errorf("a service with no key source says it is configured:\n%s", r.logs.String())
	}
	// Other token dials alone, with no key source, change nothing: still started, still "not configured".
	r2 := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarJWTIssuer: "https://issuer.example.test", config.VarJWTLeewaySec: "5"}))
	if err := r2.stop(t); err != nil {
		t.Fatal(err)
	}
	if find(r2.logs.lines(t), "job API authentication is not configured") == nil {
		t.Errorf("with only the issuer and the leeway set the service did not say it is not configured:\n%s", r2.logs.String())
	}
}

func TestRun_JobAPIAuthenticationLogsWhichSourcesAreOnAndNeverAValue(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	secret := randomSecret(t)
	const issuer = "https://issuer.example.test/auth/v1"
	for name, c := range map[string]struct {
		env                 map[string]string
		hs256, jwks, pinned bool
		audience, leeway    string
	}{
		"the secret alone": {map[string]string{config.VarJWTSecret: secret}, true, false, false, "authenticated", "30s"},
		"the URL alone":    {map[string]string{config.VarJWKSURL: jwksHTTPS}, false, true, false, "authenticated", "30s"},
		"both, pinned, with the other dials": {map[string]string{
			config.VarJWTSecret: secret, config.VarJWKSURL: jwksHTTPS, config.VarJWTIssuer: issuer, config.VarJWTAudience: "platform", config.VarJWTLeewaySec: "0",
		}, true, true, true, "platform", "0s"},
	} {
		r := startApp(t, merge(base(closedPortDSN(t)), c.env))
		if err := r.stop(t); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		l := find(r.logs.lines(t), "job API authentication configured")
		if l == nil {
			t.Errorf("%s: no \"job API authentication configured\" line:\n%s", name, r.logs.String())
			continue
		}
		if l["hs256"] != c.hs256 || l["jwks"] != c.jwks || l["issuer_pinned"] != c.pinned || l["audience"] != c.audience || l["leeway"] != c.leeway {
			t.Errorf("%s: the line says %v, want hs256=%v jwks=%v issuer_pinned=%v audience=%s leeway=%s", name, l, c.hs256, c.jwks, c.pinned, c.audience, c.leeway)
		}
		if find(r.logs.lines(t), "job API authentication is not configured") != nil {
			t.Errorf("%s: the service says both configured and not configured", name)
		}
		// The line names sources, never values: not the secret, not the URL, not the issuer.
		for _, leak := range []string{secret, "project.example.test", issuer} {
			if strings.Contains(r.logs.String(), leak) {
				t.Errorf("%s: the log carries %q:\n%s", name, leak, r.logs.String())
			}
		}
	}
}

func TestRun_AnInvalidAuthenticationCombinationStopsTheProcessWithExitStatus2(t *testing.T) {
	secret := randomSecret(t)
	for name, c := range map[string]struct {
		env  map[string]string
		name string // the variable the message must name
	}{
		"an http JWKS URL (Load accepts a URL; the verifier refuses to fetch keys in the clear)": {map[string]string{config.VarJWKSURL: jwksHTTP}, config.VarJWKSURL},
		"an http JWKS URL beside a good secret":                                                  {map[string]string{config.VarJWKSURL: jwksHTTP, config.VarJWTSecret: secret}, config.VarJWKSURL},
		"a secret one byte under the floor":                                                      {map[string]string{config.VarJWTSecret: secret[:31]}, config.VarJWTSecret},
		"a leeway over five minutes":                                                             {map[string]string{config.VarJWTSecret: secret, config.VarJWTLeewaySec: "301"}, config.VarJWTLeewaySec},
		"an issuer with a space":                                                                 {map[string]string{config.VarJWTSecret: secret, config.VarJWTIssuer: "two words"}, config.VarJWTIssuer},
	} {
		logs := &syncBuffer{}
		// A bounded context: a service that wrongly accepts the configuration serves until it ends, and the test must then fail, not hang.
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		err := Run(ctx, envOf(merge(base(closedPortDSN(t)), c.env)), logs)
		cancel()
		var ce *config.Error
		if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
			t.Errorf("%s: Run = %v (exit %d), want a configuration error, exit status 2", name, err, ExitCode(err))
			continue
		}
		if !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: the message does not name %s: %v", name, c.name, err)
		}
		for _, leak := range []string{secret, secret[:31], "project.example.test", "two words"} {
			if strings.Contains(err.Error(), leak) || strings.Contains(logs.String(), leak) {
				t.Errorf("%s: the error or the log carries %q: %v / %s", name, leak, err, logs.String())
			}
		}
		if find(logs.lines(t), "platform starting") != nil || find(logs.lines(t), "http server listening") != nil {
			t.Errorf("%s: the service started although its configuration was refused:\n%s", name, logs.String())
		}
	}

	// The twins: the same settings made valid start the service.
	withSuspender(t, &fakeSuspender{}, nil)
	for name, env := range map[string]map[string]string{
		"the https JWKS URL":      {config.VarJWKSURL: jwksHTTPS},
		"the secret at the floor": {config.VarJWTSecret: secret[:32]},
		"the leeway at the top":   {config.VarJWTSecret: secret, config.VarJWTLeewaySec: "300"},
	} {
		r := startApp(t, merge(base(closedPortDSN(t)), env))
		if err := r.stop(t); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
