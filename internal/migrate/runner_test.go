package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests need no database. The behaviour that does (the lock, the transaction, the self-record check, the down
// guards) is proven against a real PostgreSQL in tests/harness.

func TestAdvisoryLockKeyIsInThePlatformBand(t *testing.T) {
	if advisoryLockKey/1_000_000_000 != 7 {
		t.Fatalf("advisoryLockKey = %d is outside the platform's 7_xxx_xxx_xxx band (module 3 holds 8_, module 2 holds 9_)", advisoryLockKey)
	}
	if advisoryLockKey != 7_100_000_001 {
		t.Fatalf("advisoryLockKey = %d; tests/harness asserts contention on 7100000001, change both together", advisoryLockKey)
	}
}

func TestTimeoutStatements(t *testing.T) {
	got := timeoutStatements(7, 42)
	want := []string{"SET LOCAL lock_timeout = '7s'", "SET LOCAL statement_timeout = '42s'"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("timeoutStatements(7, 42) = %q, want %q", got, want)
	}
}

// unreachableDSN points at a port nothing listens on, with a password that would be recognisable anywhere it leaked.
const (
	leakProbe      = "RECOGNISABLE-pw-Zq83-1177"
	unreachableDSN = "postgres://migrator:" + leakProbe + "@127.0.0.1:1/platform?connect_timeout=3"
)

func assertNoLeak(t *testing.T, where, text, dsn string, secrets ...string) {
	t.Helper()
	for _, s := range append([]string{dsn, leakProbe}, secrets...) {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s contains %q: %s", where, s, text)
		}
	}
}

func TestConnect_AnUnreachableHostIsARuntimeErrorThatCarriesNoSecret(t *testing.T) {
	r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, LockTimeoutSec: 1, StatementTimeoutSec: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.connect(ctx)
	if err == nil {
		t.Fatal("connected to 127.0.0.1:1")
	}
	var ce *ConfigError
	if errors.As(err, &ce) {
		t.Errorf("an unreachable host is a runtime error (exit 1), got a ConfigError: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "connect: ") {
		t.Errorf("error %q should say it was the connect that failed", err)
	}
	assertNoLeak(t, "the connect error", err.Error(), unreachableDSN)
}

// The shapes below are connection strings pgx cannot parse. For each, the control asks pgx directly whether ITS error
// quotes the password: where it does, the scrub is what keeps it out of ours. At least one must, or the test would
// pass whatever the scrub did.
func TestConnect_AConnectionStringTheDriverRejectsIsAConfigErrorWithoutTheSecret(t *testing.T) {
	shapes := []string{
		"postgres://migrator:" + leakProbe + "@db.example:notaport/platform",
		"postgres://migrator:" + leakProbe + "@db.example:5432/platform?sslmode=bogus-mode",
		"host=db.example port=notaport user=migrator password=" + leakProbe,
		"host=db.example port=notaport user=migrator password = " + leakProbe,
		"postgres://migrator@db.example:notaport/platform?password=" + leakProbe,
	}
	rawLeaks := 0
	for _, dsn := range shapes {
		if _, rawErr := pgx.ParseConfig(dsn); rawErr == nil {
			t.Errorf("control: pgx accepted %q, so it is not a rejected shape", dsn)
			continue
		} else if strings.Contains(rawErr.Error(), leakProbe) {
			rawLeaks++
		}
		r := &Runner{Cfg: Config{DatabaseURL: dsn}}
		_, err := r.connect(context.Background())
		var ce *ConfigError
		if !errors.As(err, &ce) {
			t.Errorf("%q: error is %T (%v), want *ConfigError so the command exits 2", dsn, err, err)
			continue
		}
		if !strings.Contains(err.Error(), EnvDatabaseURL) {
			t.Errorf("%q: error %q does not name %s", dsn, err, EnvDatabaseURL)
		}
		assertNoLeak(t, "the config error", err.Error(), dsn)
	}
	if rawLeaks == 0 {
		t.Error("no shape made pgx quote the password, so this test would pass with the scrub removed; add a leaking shape")
	}
}

func TestScrubDriverError(t *testing.T) {
	dsn := "postgres://u:" + leakProbe + "@h:5432/d"
	leaky := fmt.Errorf("driver says: cannot use %s", dsn)
	got := scrubDriverError(leaky, dsn)
	assertNoLeak(t, "the scrubbed error", got.Error(), dsn)
	if !strings.Contains(got.Error(), EnvDatabaseURL) {
		t.Errorf("the replacement should name %s (dsnscrub's own text names the service's variable), got %q", EnvDatabaseURL, got)
	}
	if strings.Contains(got.Error(), "PLATFORM_DATABASE_URL") {
		t.Errorf("the replacement names the service's variable, not the migrator's: %q", got)
	}
	if !errors.Is(got, leaky) {
		t.Error("errors.Is must still reach the original error")
	}
	// Control: an error that carries nothing from the DSN passes through untouched.
	plain := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	if got := scrubDriverError(plain, dsn); got != plain {
		t.Errorf("an innocuous error was rewritten: %v", got)
	}
	if scrubDriverError(nil, dsn) != nil {
		t.Error("nil must stay nil")
	}
}

func TestRedactErr(t *testing.T) {
	orig := fmt.Errorf("server said no near %s", testPW)
	got := redactErr(orig, testPW)
	if strings.Contains(got.Error(), testPW) {
		t.Errorf("the redacted error still carries the password: %v", got)
	}
	if !errors.Is(got, orig) {
		t.Error("errors.Is must still reach the original error")
	}
	if same := errors.New("nothing secret"); redactErr(same, testPW) != same {
		t.Error("an error without the secret must be returned as is")
	}
	if redactErr(nil, testPW) != nil {
		t.Error("nil must stay nil")
	}
}

// Down is a configuration refusal decided before anything is read or connected: the DSN here is unreachable and the
// directory empty, and the answer is still ErrDownForbidden. The control flips only the flag.
func TestDown_IsRefusedUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, MigrationsDir: dir, LockTimeoutSec: 1, StatementTimeoutSec: 1}}
	if err := r.Down(context.Background(), "0000"); !errors.Is(err, ErrDownForbidden) {
		t.Fatalf("Down without the flag: err = %v, want ErrDownForbidden", err)
	}
	if !strings.Contains(ErrDownForbidden.Error(), EnvAllowDown+"=true") {
		t.Errorf("the refusal %q must say how to permit it", ErrDownForbidden)
	}
	r.Cfg.AllowDown = true
	err := r.Down(context.Background(), "0000")
	if err == nil || errors.Is(err, ErrDownForbidden) {
		t.Fatalf("control: with the flag the refusal must be gone, err = %v", err)
	}
	if !strings.Contains(err.Error(), "found 0 migration files") {
		t.Errorf("control: with the flag Down should have moved on to reading the directory, err = %v", err)
	}
}

func TestDown_RejectsAVersionItCannotRead(t *testing.T) {
	r := &Runner{Cfg: Config{AllowDown: true, MigrationsDir: t.TempDir()}}
	err := r.Down(context.Background(), "latest")
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *ConfigError", err)
	}
}

// Every command reads the migration files before it connects, so a wrong directory is reported as that and is never
// mistaken for a database problem; and an empty directory is an error, not "already current".
func TestEveryCommandRefusesAnEmptyDirectoryBeforeConnecting(t *testing.T) {
	r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, MigrationsDir: t.TempDir(), LockTimeoutSec: 1, StatementTimeoutSec: 1, AllowDown: true}, Out: &bytes.Buffer{}}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"up":     func() error { return r.Up(ctx) },
		"status": func() error { return r.Status(ctx) },
		"down":   func() error { return r.Down(ctx, "0000") },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "found 0 migration files") {
			t.Errorf("%s on an empty directory: err = %v, want the empty-chain refusal", name, err)
			continue
		}
		if strings.Contains(err.Error(), "connect") {
			t.Errorf("%s tried to connect before reading the directory: %v", name, err)
		}
	}
	// Control: with a migration present the same call gets as far as the connection.
	dir := writeFiles(t, "platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql")
	r.Cfg.MigrationsDir = dir
	if err := r.Up(ctx); err == nil || !strings.HasPrefix(err.Error(), "connect: ") {
		t.Errorf("control: with a migration on disk Up should fail at the connection, err = %v", err)
	}
}

func TestSetAppPassword_AnEmptyPasswordIsRefusedBeforeConnecting(t *testing.T) {
	for _, pw := range []string{"", "   ", "\n"} {
		r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, AppPassword: pw}}
		err := r.SetAppPassword(context.Background())
		var ce *ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), EnvAppPassword) {
			t.Errorf("password %q: err = %v, want a *ConfigError naming %s", pw, err, EnvAppPassword)
		}
		if err != nil && strings.Contains(err.Error(), "connect") {
			t.Errorf("password %q: it connected before checking the password: %v", pw, err)
		}
	}
	// Control: a real password gets as far as the connection, and the connection error carries neither secret.
	r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, AppPassword: testPW}}
	err := r.SetAppPassword(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "connect: ") {
		t.Fatalf("control: err = %v, want a connect failure", err)
	}
	assertNoLeak(t, "the set-app-password error", err.Error(), unreachableDSN, testPW)
}
