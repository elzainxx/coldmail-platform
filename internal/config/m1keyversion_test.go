package config_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The consumer of PLATFORM_M1_KEY_VERSION (Phase 3 close-out A4; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"). The dial is read by Load into
// Config.M1KeyVersion, and its consumer is internal/tenant: tenant.NewResolver(db, cfg.M1KeySecret, tenant.WithM1KeyVersion(cfg.M1KeyVersion)) presents, in Scope, the key an independent HMAC of
// "m1-caller|v<version>|<user>" gives.

func TestM1KeyVersionDial_DefaultsToOneAndIsRangeChecked(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil || cfg.M1KeyVersion != 1 {
		t.Fatalf("an environment without the dial: version %d, %v; want 1", cfg.M1KeyVersion, err)
	}
	for _, good := range []string{"1", "2", "999"} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarM1KeyVersion: good}))
		if n, _ := strconv.Atoi(good); err != nil || cfg.M1KeyVersion != n {
			t.Errorf("%q: version %d, %v; want %d", good, cfg.M1KeyVersion, err, n)
		}
	}
	// the control: each of these is exactly one problem naming the variable (a zero, a negative, one past the top, text, a decimal, a blank-with-text)
	for _, bad := range []string{"0", "-1", "1000", "two", "1.5", "0x2", "1e1"} {
		_, err := config.Load(lockEnv(map[string]string{config.VarM1KeyVersion: bad}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM1KeyVersion {
			t.Errorf("%q: Load = %v, want exactly one problem naming %s", bad, err, config.VarM1KeyVersion)
		}
	}
}

func TestM1KeyVersionDial_ReachesTheResolver(t *testing.T) {
	independent := func(secret, user string, version int) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("m1-caller|v" + strconv.Itoa(version) + "|" + user))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	secret := m1Secret("ver-", 40)
	scopeKey := func(version string) string {
		env := map[string]string{config.VarM1KeySecret: secret}
		if version != "" {
			env[config.VarM1KeyVersion] = version
		}
		cfg, err := config.Load(lockEnv(env))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		res, err := tenant.NewResolver(oneUserDB{m1UserA}, cfg.M1KeySecret, tenant.WithM1KeyVersion(cfg.M1KeyVersion))
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		sc, err := res.Scope(context.Background(), tenant.Tenant{UserID: m1UserA, AuthUserID: m1UserA, Active: true})
		if err != nil {
			t.Fatalf("Scope: %v", err)
		}
		return sc.M1Key.Reveal()
	}
	k1, k2, k3 := scopeKey(""), scopeKey("2"), scopeKey("3")
	if k1 != independent(secret, m1UserA, 1) {
		t.Errorf("the default dial does not present the independent version-1 key")
	}
	if k2 != independent(secret, m1UserA, 2) || k3 != independent(secret, m1UserA, 3) {
		t.Errorf("a version dial does not present the independent HMAC of that version")
	}
	if k1 == k2 || k2 == k3 || k1 == k3 {
		t.Errorf("two versions of the dial presented the same key: the dial changes nothing")
	}
}
