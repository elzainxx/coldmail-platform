package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/migrate"
)

// These tests drive the real command in-process (run is main with its inputs injected). None needs a database: the
// unreachable DSN below makes every path that connects fail, and the usage and configuration paths are proven to fail
// BEFORE they connect. The runner's behaviour against a real PostgreSQL is proven in tests/harness.

const (
	leakProbe  = "RECOGNISABLE-pw-Zq83-1177"
	appPW      = "App-pw-Lm49-2204"
	unreachDSN = "postgres://migrator:" + leakProbe + "@127.0.0.1:1/platform?connect_timeout=3"
)

type outcome struct {
	code   int
	stdout string
	stderr string
}

func invoke(t *testing.T, env map[string]string, args ...string) outcome {
	t.Helper()
	var so, se bytes.Buffer
	code := run(context.Background(), args, func(name string) string { return env[name] }, &so, &se)
	return outcome{code, so.String(), se.String()}
}

// migrationsDir is a directory with one valid migration pair, so that a command which reads the files before it
// connects gets as far as the connection.
func migrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"platform_0000_bootstrap.up.sql", "platform_0000_bootstrap.down.sql"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("-- test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRun_NoArgumentsIsAUsageError(t *testing.T) {
	got := invoke(t, nil)
	if got.code != 2 || !strings.Contains(got.stderr, "usage:") || got.stdout != "" {
		t.Fatalf("got %+v, want exit 2 with the usage on stderr and nothing on stdout", got)
	}
}

func TestRun_HelpExitsZeroAndListsEveryVariable(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		got := invoke(t, nil, arg)
		if got.code != 0 || got.stderr != "" {
			t.Fatalf("%s: got %+v, want exit 0 and an empty stderr", arg, got)
		}
		if len(migrate.Vars()) == 0 {
			t.Fatal("Vars() is empty")
		}
		for _, v := range migrate.Vars() {
			if !strings.Contains(got.stdout, v.Name) {
				t.Errorf("%s: the usage text does not list %s", arg, v.Name)
			}
		}
		if !strings.Contains(got.stdout, "DIRECT (session-mode)") {
			t.Errorf("%s: the usage text must say the connection has to be direct", arg)
		}
	}
	// Control: the usage printed for an error lists the same variables, so the two cannot drift apart.
	bad := invoke(t, nil, "no-such-command")
	for _, v := range migrate.Vars() {
		if !strings.Contains(bad.stderr, v.Name) {
			t.Errorf("the error usage does not list %s", v.Name)
		}
	}
}

// Every usage and configuration problem exits 2, names what is wrong, and does so BEFORE connecting: the DSN is
// unreachable, so an attempt to connect would show up as a "connect:" error and exit 1.
func TestRun_UsageAndConfigurationErrorsExitTwoBeforeConnecting(t *testing.T) {
	dir := migrationsDir(t)
	full := map[string]string{migrate.EnvDatabaseURL: unreachDSN, migrate.EnvMigrationsDir: dir}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"unknown command", full, []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"up takes no arguments", full, []string{"up", "extra"}, "up takes no arguments"},
		{"status takes no arguments", full, []string{"status", "0000"}, "status takes no arguments"},
		{"down needs a version", full, []string{"down"}, "down needs exactly one version"},
		{"down takes one version", full, []string{"down", "1", "2"}, "down needs exactly one version"},
		{"down version not digits", with(migrate.EnvAllowDown, "true"), []string{"down", "abc"}, "one to four digits"},
		{"down version too long", with(migrate.EnvAllowDown, "true"), []string{"down", "12345"}, "one to four digits"},
		{"database url missing", map[string]string{migrate.EnvMigrationsDir: dir}, []string{"up"}, migrate.EnvDatabaseURL},
		{"database url missing for status", map[string]string{migrate.EnvMigrationsDir: dir}, []string{"status"}, migrate.EnvDatabaseURL},
		{"lock timeout not a number", with(migrate.EnvLockTimeoutSec, "soon"), []string{"up"}, migrate.EnvLockTimeoutSec},
		{"statement timeout zero", with(migrate.EnvStatementTimeoutSec, "0"), []string{"up"}, migrate.EnvStatementTimeoutSec},
		{"allow down not a bool", with(migrate.EnvAllowDown, "yes"), []string{"down", "0000"}, migrate.EnvAllowDown},
		{"down while forbidden", full, []string{"down", "0000"}, migrate.EnvAllowDown + "=true"},
		{"set-app-password without a password", full, []string{"set-app-password"}, migrate.EnvAppPassword},
		{"up-and-set-password without a password", full, []string{"up-and-set-password"}, migrate.EnvAppPassword},
		{"unparseable connection string (URL)", with(migrate.EnvDatabaseURL, "postgres://migrator:"+leakProbe+"@db.example:notaport/platform"), []string{"status"}, migrate.EnvDatabaseURL},
		// the spaced keyword spelling is one pgx quotes back whole: the scrub (internal/migrate tests) and the output redaction (TestFail_Redacts...) each keep it out, and this proves the two together
		{"unparseable connection string (keyword, spaced password)", with(migrate.EnvDatabaseURL, "host=db.example port=notaport user=migrator password = "+leakProbe), []string{"status"}, migrate.EnvDatabaseURL},
	}
	for _, c := range cases {
		got := invoke(t, c.env, c.args...)
		if got.code != 2 {
			t.Errorf("%s: exit %d, want 2 (stdout %q stderr %q)", c.name, got.code, got.stdout, got.stderr)
			continue
		}
		if !strings.Contains(got.stderr, c.want) {
			t.Errorf("%s: stderr %q does not say %q", c.name, got.stderr, c.want)
		}
		if strings.Contains(got.stderr, "connect:") {
			t.Errorf("%s: it tried to connect before failing: %q", c.name, got.stderr)
		}
		if got.stdout != "" && !strings.HasPrefix(c.name, "unknown") {
			t.Errorf("%s: unexpected stdout %q: a refused command must not have done any work", c.name, got.stdout)
		}
		for _, secret := range []string{leakProbe, appPW} {
			if strings.Contains(got.stdout+got.stderr, secret) {
				t.Errorf("%s: output contains %q: %q", c.name, secret, got.stderr)
			}
		}
	}
	// Control: the same command line with the offence removed is not a usage error; it gets as far as the connection.
	got := invoke(t, full, "status")
	if got.code != 1 || !strings.Contains(got.stderr, "connect:") {
		t.Errorf("control: status with a valid configuration should fail at the connection with exit 1, got %+v", got)
	}
}

// The password check comes before any migration: up-and-set-password with no password applies nothing. Here that shows
// as "refused with exit 2 and no attempt to connect"; tests/harness shows it against a real database (nothing applied).
func TestRun_UpAndSetPasswordChecksThePasswordFirst(t *testing.T) {
	env := map[string]string{migrate.EnvDatabaseURL: unreachDSN, migrate.EnvMigrationsDir: migrationsDir(t)}
	got := invoke(t, env, "up-and-set-password")
	if got.code != 2 || strings.Contains(got.stderr, "connect:") {
		t.Fatalf("got %+v, want exit 2 without connecting", got)
	}
	env[migrate.EnvAppPassword] = appPW
	got = invoke(t, env, "up-and-set-password")
	if got.code != 1 || !strings.Contains(got.stderr, "connect:") {
		t.Fatalf("control: with a password it should proceed to the connection and fail there, got %+v", got)
	}
}

// create-first-admin is the one command with flags. Every bad command line exits 2, names the flag at fault and the rule, and does so
// BEFORE it connects (the DSN is unreachable, so an attempt would show up as "connect:" and exit 1). It needs no password and no
// migrations directory, and the usage text lists every command the runner has.
func TestRun_CreateFirstAdminIsValidatedBeforeConnecting(t *testing.T) {
	const id = "123e4567-e89b-12d3-a456-426614174000"
	env := map[string]string{migrate.EnvDatabaseURL: unreachDSN}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no flags", []string{"create-first-admin"}, "--auth-user-id is required"},
		{"no name", []string{"create-first-admin", "--auth-user-id", id}, "--display-name is required"},
		{"no id", []string{"create-first-admin", "--display-name", "Ada Boss"}, "--auth-user-id is required"},
		{"blank name", []string{"create-first-admin", "--auth-user-id", id, "--display-name", "   "}, "--display-name must not be blank"},
		{"all-zero id", []string{"create-first-admin", "--auth-user-id", "00000000-0000-0000-0000-000000000000", "--display-name", "Ada"}, "--auth-user-id must not be the all-zero uuid"},
		{"upper-case id", []string{"create-first-admin", "--auth-user-id", strings.ToUpper(id), "--display-name", "Ada"}, "--auth-user-id must be a lower-case hyphenated uuid"},
		{"malformed id", []string{"create-first-admin", "--auth-user-id", "not-a-uuid", "--display-name", "Ada"}, "--auth-user-id must be a lower-case hyphenated uuid"},
		{"control character in the name", []string{"create-first-admin", "--auth-user-id", id, "--display-name", "Bo\tss"}, "--display-name must not contain control characters"},
		{"name over 200 bytes", []string{"create-first-admin", "--auth-user-id", id, "--display-name", strings.Repeat("x", 201)}, "--display-name must be at most 200 bytes"},
		{"the old spelling --name", []string{"create-first-admin", "--auth-user-id", id, "--name", "Ada"}, "unknown flag --name"},
		{"a positional argument", []string{"create-first-admin", id}, "no other arguments"},
	}
	for _, c := range cases {
		got := invoke(t, env, c.args...)
		if got.code != 2 {
			t.Errorf("%s: exit %d, want 2 (stdout %q stderr %q)", c.name, got.code, got.stdout, got.stderr)
			continue
		}
		if !strings.Contains(got.stderr, "config error") || !strings.Contains(got.stderr, c.want) {
			t.Errorf("%s: stderr %q does not say %q as a config error", c.name, got.stderr, c.want)
		}
		if strings.Contains(got.stderr, "connect:") || got.stdout != "" {
			t.Errorf("%s: it connected or did work before refusing: stdout %q stderr %q", c.name, got.stdout, got.stderr)
		}
	}

	// Control: the same command with valid flags is not a usage error. It needs only the connection string (no password, no
	// migrations directory), gets as far as the connection, fails there with exit 1, and prints neither the DSN nor its password.
	got := invoke(t, env, "create-first-admin", "--auth-user-id", id, "--display-name", "Ada Boss")
	if got.code != 1 || !strings.Contains(got.stderr, "connect:") || got.stdout != "" {
		t.Fatalf("control: valid flags should fail at the connection with exit 1, got %+v", got)
	}
	for _, secret := range []string{unreachDSN, leakProbe} {
		if strings.Contains(got.stdout+got.stderr, secret) {
			t.Errorf("control: the output contains %q: %q", secret, got.stderr)
		}
	}
	// The connection string is still required, and still a configuration error (the flags were fine).
	got = invoke(t, nil, "create-first-admin", "--auth-user-id", id, "--display-name", "Ada Boss")
	if got.code != 2 || !strings.Contains(got.stderr, migrate.EnvDatabaseURL) {
		t.Errorf("no connection string: got %+v, want exit 2 naming %s", got, migrate.EnvDatabaseURL)
	}

	// The usage text lists every command, so the command is findable from help.
	help := invoke(t, nil, "help")
	if len(migrate.Commands()) == 0 {
		t.Fatal("Commands() is empty")
	}
	for _, name := range migrate.Commands() {
		if !strings.Contains(help.stdout, "platform-migrate "+name) {
			t.Errorf("the usage text does not list %q", name)
		}
	}
	if !strings.Contains(help.stdout, "--auth-user-id <uuid> --display-name <text>") {
		t.Errorf("the usage text does not show the flags of create-first-admin:\n%s", help.stdout)
	}
}

// The recognisable password and the whole DSN must appear nowhere in what any command prints, whichever way it fails.
func TestRun_NoCommandPrintsTheConnectionStringOrAPassword(t *testing.T) {
	dir := migrationsDir(t)
	env := map[string]string{
		migrate.EnvDatabaseURL: unreachDSN, migrate.EnvMigrationsDir: dir, migrate.EnvAppPassword: appPW, migrate.EnvAllowDown: "true",
	}
	for _, args := range [][]string{{"up"}, {"status"}, {"down", "0000"}, {"set-app-password"}, {"up-and-set-password"}} {
		got := invoke(t, env, args...)
		name := strings.Join(args, " ")
		if got.code != 1 {
			t.Errorf("%s: exit %d, want 1 (the host is unreachable): %+v", name, got.code, got)
			continue
		}
		if !strings.Contains(got.stderr, "connect:") {
			t.Errorf("%s: stderr %q does not report the failed connection, so the absence of a secret below proves nothing", name, got.stderr)
		}
		all := got.stdout + got.stderr
		for _, secret := range []string{unreachDSN, leakProbe, appPW} {
			if strings.Contains(all, secret) {
				t.Errorf("%s: the output contains %q: %s", name, secret, all)
			}
		}
	}
}

func TestFail_ExitStatusByKindOfError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a config error", &migrate.ConfigError{Msg: "x"}, 2},
		{"a wrapped config error", fmt.Errorf("outer: %w", &migrate.ConfigError{Msg: "x"}), 2},
		{"a usage error", &usageError{"x"}, 2},
		{"down forbidden", migrate.ErrDownForbidden, 2},
		{"down forbidden, wrapped", fmt.Errorf("outer: %w", migrate.ErrDownForbidden), 2},
		{"a plain error", errors.New("the database said no"), 1},
		{"a cancelled context", context.Canceled, 1},
		{"a deadline", context.DeadlineExceeded, 1},
	}
	for _, c := range cases {
		var se bytes.Buffer
		if got := fail(&se, c.err, nil); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
		if !strings.HasPrefix(se.String(), "platform-migrate: ") || !strings.HasSuffix(se.String(), "\n") {
			t.Errorf("%s: output %q should be one platform-migrate line", c.name, se.String())
		}
	}
}

// The output backstop: an error that somehow carries a secret is redacted at the boundary.
func TestFail_RedactsASecretThatGotIntoAnError(t *testing.T) {
	var se bytes.Buffer
	fail(&se, fmt.Errorf("the server echoed %s back", appPW), []string{appPW})
	if strings.Contains(se.String(), appPW) || !strings.Contains(se.String(), "[redacted]") {
		t.Fatalf("got %q", se.String())
	}
	// Control: with no secret listed the text passes through, so the redaction above is what removed it.
	se.Reset()
	fail(&se, fmt.Errorf("the server echoed %s back", appPW), nil)
	if !strings.Contains(se.String(), appPW) {
		t.Fatalf("control: got %q", se.String())
	}
}
