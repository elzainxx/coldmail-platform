package config_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The consumer of PLATFORM_M1_KEY_SECRET (Phase 3 B2; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it"). The dial is read by Load into Config.M1KeySecret, and the
// platform's one consumer of it is internal/tenant: tenant.NewResolver(db, cfg.M1KeySecret) derives each user's module 1 caller key from it. The service builds no resolver yet (nothing
// calls a module before the adapters), so these tests are the proof that the whole path works: the variable is validated by the process's real entry point (app.Run, exit status 2), loaded
// into the Config, handed to the resolver, and what the resolver derives from it is the HMAC an independent computation gives.

// Secrets here are built from a short repeated word, never typed as a long literal: they are stand-ins, and a long random-looking literal next to the word "secret" is what a secret scanner
// is meant to flag.
func m1Secret(word string, size int) string { return strings.Repeat(word, size/len(word)+1)[:size] }

const m1UserA = "a1a1a1a1-0000-4000-8000-00000000000a"

func TestM1KeySecretDial_NeedsAtLeast32BytesWhenSet(t *testing.T) {
	for _, bad := range []string{
		"short-XYZ123",               // 12 bytes
		m1Secret("b", 31),            // one byte under the floor
		"   " + m1Secret("c", 31),    // white space around the value is trimmed first and does not count
		m1Secret("d", 31) + "\t \n ", // the same on the other side
	} {
		_, err := config.Load(lockEnv(map[string]string{config.VarM1KeySecret: bad}))
		var ce *config.Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM1KeySecret {
			t.Errorf("a %d-byte value: Load = %v, want exactly one problem naming %s", len(bad), err, config.VarM1KeySecret)
			continue
		}
		if !strings.Contains(ce.Problems[0].Reason, "at least 32 bytes") {
			t.Errorf("a %d-byte value: the reason is %q, want it to say at least 32 bytes", len(bad), ce.Problems[0].Reason)
		}
		if strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Errorf("a %d-byte value: the error echoes the secret: %v", len(bad), err)
		}
	}

	// The twins: the same variable at the floor, far above it, with white space around it, blank, and the documented marker.
	for _, c := range []struct {
		name, value, want string
	}{
		{"exactly 32 bytes", m1Secret("e", 32), m1Secret("e", 32)},
		{"64 bytes", m1Secret("f", 64), m1Secret("f", 64)},
		{"trimmed", "  " + m1Secret("g", 32) + "\n", m1Secret("g", 32)},
		{"blank means unset", "   ", ""},
		{"the marker means unset", "(unset)", ""},
	} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarM1KeySecret: c.value}))
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if cfg.M1KeySecret.Reveal() != c.want {
			t.Errorf("%s: loaded as a secret of %d bytes, want %d", c.name, len(cfg.M1KeySecret.Reveal()), len(c.want))
		}
	}
	// And nothing at all: the service starts without it (the dial is optional).
	cfg, err := config.Load(lockEnv(nil))
	if err != nil || cfg.M1KeySecret != "" {
		t.Fatalf("an environment without %s did not load to an empty secret: %v (%v)", config.VarM1KeySecret, cfg, err)
	}
}

// TestM1KeySecretDial_ReachesTheResolver is the consumer proof. The environment variable becomes Config.M1KeySecret, the resolver built from it derives for a user exactly the key an
// independent HMAC-SHA256 computation gives (so the derivation did not silently use another input), another value of the variable gives another key (the dial changes something), and an unset
// variable gives no key at all (never a default one). The resolver's database is a stub that answers one active user: the path under test is the dial, not the query.
func TestM1KeySecretDial_ReachesTheResolver(t *testing.T) {
	independent := func(secret, user string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("m1-caller|v1|" + user))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	scopeKey := func(env map[string]string) (string, error) {
		cfg, err := config.Load(lockEnv(env))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		res, err := tenant.NewResolver(oneUserDB{m1UserA}, cfg.M1KeySecret)
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		sc, err := res.Scope(context.Background(), tenant.Tenant{UserID: m1UserA, AuthUserID: m1UserA, Active: true})
		return sc.M1Key.Reveal(), err
	}

	one, other := m1Secret("one-", 40), m1Secret("two-", 40)
	k1, err := scopeKey(map[string]string{config.VarM1KeySecret: one})
	if err != nil {
		t.Fatalf("Scope with the dial set: %v", err)
	}
	if want := independent(one, m1UserA); k1 != want {
		t.Fatalf("the key Scope derived from the dial is not the independent HMAC of the same secret (%d vs %d bytes)", len(k1), len(want))
	}
	k2, err := scopeKey(map[string]string{config.VarM1KeySecret: other})
	if err != nil || k2 == k1 || k2 != independent(other, m1UserA) {
		t.Fatalf("another value of the dial must give another key, the independent HMAC of that value (err %v, equal to the first: %v)", err, k2 == k1)
	}
	// the control: unset gives no key and the typed refusal, never a key derived from the marker or from nothing
	k0, err := scopeKey(nil)
	if !errors.Is(err, tenant.ErrNoM1KeySecret) || k0 != "" {
		t.Fatalf("with the dial unset Scope returned (%d bytes, %v), want no key and ErrNoM1KeySecret", len(k0), err)
	}
}

// TestM1KeySecretDial_IsNeverPrinted: no way the standard library prints a Config, a Secret or a log record shows the value, and an unset dial says so without a value. The paired control is a
// plain string field, which leaks through the same sinks.
func TestM1KeySecretDial_IsNeverPrinted(t *testing.T) {
	canary := "CANARY-m1key-" + m1Secret("z", 40)
	cfg, err := config.Load(lockEnv(map[string]string{config.VarM1KeySecret: canary}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.M1KeySecret.Reveal() != canary {
		t.Fatalf("control failed: Reveal() does not return the loaded value")
	}
	sinks := map[string]string{
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
		"Sprintln": fmt.Sprintln(cfg, &cfg),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		sinks["Config "+verb] = fmt.Sprintf(verb, cfg)
		sinks["Secret "+verb] = fmt.Sprintf(verb, cfg.M1KeySecret)
	}
	for _, v := range []any{cfg, &cfg, cfg.M1KeySecret} {
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
		slog.New(handler).Info("loaded", "config", cfg, "secret", cfg.M1KeySecret)
		sinks["slog "+h] = buf.String()
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		if strings.Contains(out, canary) || strings.Contains(out, canary[len("CANARY-"):]) {
			t.Errorf("%s reveals the module 1 key secret: %s", name, out)
		}
	}
	if !strings.Contains(cfg.String(), "m1_key_secret=[redacted]") {
		t.Errorf("a set secret is not shown as the redaction marker: %s", cfg.String())
	}
	unset, err := config.Load(lockEnv(nil))
	if err != nil || !strings.Contains(unset.String(), "m1_key_secret=(unset)") {
		t.Errorf("an unset secret is not shown as (unset): %v (%v)", unset.String(), err)
	}
	// the control: a plain string field of the same shape DOES leak through the same sinks
	type naive struct{ M1KeySecret string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{canary})
	if !strings.Contains(buf.String(), canary) || !strings.Contains(fmt.Sprintf("%+v", naive{canary}), canary) {
		t.Fatal("control failed: a plain string field did not reach the sinks, so the checks above prove nothing")
	}
}

// TestM1KeySecretDial_InvalidValueStopsTheProcessWithExitStatus2: the entry point's own check, not only Load's. The controls are the accepted values of the same variable.
func TestM1KeySecretDial_InvalidValueStopsTheProcessWithExitStatus2(t *testing.T) {
	bad := m1Secret("h", 31)
	err := app.Run(context.Background(), lockEnv(map[string]string{config.VarM1KeySecret: bad}), io.Discard)
	var ce *config.Error
	if !errors.As(err, &ce) {
		t.Fatalf("Run = %v, want a *config.Error", err)
	}
	if got := app.ExitCode(err); got != 2 {
		t.Errorf("exit status %d, want 2", got)
	}
	if len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM1KeySecret {
		t.Errorf("problems %+v, want exactly one, naming the variable", ce.Problems)
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("the error echoes the secret: %v", err)
	}
	for _, ok := range []string{m1Secret("i", 32), "(unset)", "  "} {
		if _, err := config.Load(lockEnv(map[string]string{config.VarM1KeySecret: ok})); err != nil {
			t.Errorf("a %d-byte value was refused: %v", len(ok), err)
		}
	}
}

// oneUserDB is a tenant.DB that knows exactly one active platform user, whose login is the user's own id.
type oneUserDB struct{ id string }

func (d oneUserDB) QueryRow(context.Context, string, ...any) pgx.Row { return oneUserRow(d) }

type oneUserRow struct{ id string }

func (r oneUserRow) Scan(dest ...any) error {
	if len(dest) != 3 {
		return fmt.Errorf("the stub expects three destinations (id, auth_user_id, is_active), got %d", len(dest))
	}
	*(dest[0].(*string)), *(dest[1].(*string)), *(dest[2].(*bool)) = r.id, r.id, true
	return nil
}
