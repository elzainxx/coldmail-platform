package config_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/money"
)

// The consumer of PLATFORM_MONEY_LOCK_TIMEOUT_MS (Phase 2 close-out D8; CLAUDE.md: "every dial is seeded with a consumer at the commit that seeds it").
//
// The service builds no money store yet: the first phase that serves a money request (Phase 3) is the first caller of money.New in internal/app. Until then the dial's consumer is
// the store's own option, and these tests are the proof that the whole path works: the environment variable is validated by the process's real entry point (app.Run, exit status 2),
// loaded into Config.MoneyLockTimeout, and handed to money.New through money.WithLockTimeout, where it becomes the wait of every money transaction. The wiring a later phase writes
// is exactly the line moneyStoreOf below.

const (
	lockURL   = "postgres://platform_app:pw@db.example:5432/postgres"
	lockM3URL = "http://m3.example:8080"
	lockM3Key = "operator-operator-ok"
)

func lockEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		config.VarDatabaseURL:  lockURL,
		config.VarM3URL:        lockM3URL,
		config.VarM3AdminToken: lockM3Key,
	}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// moneyStoreOf is what the service will do with its configuration once it builds a money store: the pool is nil here (nothing is run), the wait is the dial's.
func moneyStoreOf(cfg config.Config) *money.Store {
	return money.New(nil, money.WithLockTimeout(cfg.MoneyLockTimeout))
}

// The default of the dial and the default of the store are one number: a store built from an unset dial waits what a store built with no option waits, and both are bounded.
func TestMoneyLockDial_DefaultReachesTheStoreAndEqualsTheStoresOwnDefault(t *testing.T) {
	cfg, err := config.Load(lockEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MoneyLockTimeout != 10*time.Second {
		t.Fatalf("the dial's default is %s, want the documented 10s", cfg.MoneyLockTimeout)
	}
	if got := moneyStoreOf(cfg).LockTimeout(); got != money.DefaultLockTimeout {
		t.Fatalf("a store built from the unset dial waits %s, want money.DefaultLockTimeout (%s)", got, money.DefaultLockTimeout)
	}
	if got := money.New(nil).LockTimeout(); got != cfg.MoneyLockTimeout {
		t.Fatalf("a store built with no option waits %s, the dial's default is %s: the two defaults drifted apart", got, cfg.MoneyLockTimeout)
	}
}

// Changing the variable changes the wait of the store built from the Config: the dial has a consumer, and the paired control is the unchanged default beside it.
func TestMoneyLockDial_TheVariableIsTheStoresWait(t *testing.T) {
	for _, c := range []struct {
		value string
		want  time.Duration
	}{
		{"100", 100 * time.Millisecond},
		{"300", 300 * time.Millisecond},
		{"2500", 2500 * time.Millisecond},
		{"600000", 10 * time.Minute},
	} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarMoneyLockTimeoutMS: c.value}))
		if err != nil {
			t.Fatalf("%s=%s was refused: %v", config.VarMoneyLockTimeoutMS, c.value, err)
		}
		if got := moneyStoreOf(cfg).LockTimeout(); got != c.want {
			t.Errorf("%s=%s: the store waits %s, want %s", config.VarMoneyLockTimeoutMS, c.value, got, c.want)
		}
		if got := moneyStoreOf(cfg).LockTimeout(); got == money.DefaultLockTimeout {
			t.Errorf("%s=%s: the store waits the default %s: the dial changed nothing", config.VarMoneyLockTimeoutMS, c.value, got)
		}
	}
}

// An invalid value stops the process at start-up with exit status 2, naming the variable and the reason, never the value (the entry point's own check, not only Load's). The
// controls are the accepted edges of the same variable and a blank value (which means unset), which load without a problem.
func TestMoneyLockDial_InvalidValuesAreRefusedAtStartupWithExitStatus2(t *testing.T) {
	for _, bad := range []string{"0", "-1", "99", "600001", "forever-XYZ123", "2.5"} {
		err := app.Run(context.Background(), lockEnv(map[string]string{config.VarMoneyLockTimeoutMS: bad}), io.Discard)
		var ce *config.Error
		if !errors.As(err, &ce) {
			t.Errorf("%s=%q: Run = %v, want a *config.Error", config.VarMoneyLockTimeoutMS, bad, err)
			continue
		}
		if got := app.ExitCode(err); got != 2 {
			t.Errorf("%s=%q: exit status %d, want 2", config.VarMoneyLockTimeoutMS, bad, got)
		}
		if len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarMoneyLockTimeoutMS {
			t.Errorf("%s=%q: problems %+v, want exactly one, naming the variable", config.VarMoneyLockTimeoutMS, bad, ce.Problems)
		}
		if len(bad) >= 5 && strings.Contains(err.Error(), bad) {
			t.Errorf("%s=%q: the error echoes the value: %v", config.VarMoneyLockTimeoutMS, bad, err)
		}
	}

	// The controls: the edges and a blank value load without a problem about this variable.
	for _, ok := range []string{"100", "600000", "  ", "10000"} {
		cfg, err := config.Load(lockEnv(map[string]string{config.VarMoneyLockTimeoutMS: ok}))
		if err != nil {
			t.Errorf("%s=%q was refused: %v", config.VarMoneyLockTimeoutMS, ok, err)
			continue
		}
		if cfg.MoneyLockTimeout < 100*time.Millisecond || cfg.MoneyLockTimeout > 10*time.Minute {
			t.Errorf("%s=%q loaded as %s, outside the documented range", config.VarMoneyLockTimeoutMS, ok, cfg.MoneyLockTimeout)
		}
	}
}
