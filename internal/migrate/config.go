// Package migrate is the platform's migration runner: the library behind cmd/platform-migrate.
//
// It keeps module 2's LOCKED convention. Every platform_NNNN_<desc>.up.sql SELF-RECORDS its version in
// platform.schema_migrations with its own INSERT and every .down.sql DELETEs it; the runner does not write the
// tracking row. It ORDERS and APPLIES pending files, and it VERIFIES, inside the file's own transaction, that the
// file did what the convention says (an up that did not record itself is rolled back, not committed).
//
// What the runner guarantees, each proven by a test (internal/migrate unit tests, tests/harness against a real Postgres):
//   - one session-level advisory lock for the whole of up and down, taken by a bounded pg_try_advisory_lock retry loop;
//   - the chain is checked for a gap BEFORE anything is applied;
//   - every file runs in ONE transaction with SET LOCAL lock_timeout and statement_timeout;
//   - down needs PLATFORM_MIGRATE_ALLOW_DOWN=true, refuses while a higher version is applied, and verifies the row is gone;
//   - no error that leaves the process carries the connection string or the app password.
//
// Every configuration value is an environment variable (see Vars and Docs/migrator-reference.md). The one fixed
// value in the package is the connect bound, which the connection string's own connect_timeout overrides.
package migrate

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The complete configuration surface of platform-migrate. Docs/migrator-reference.md lists exactly these, and a
// test fails when the document and Vars() disagree in either direction.
const (
	EnvDatabaseURL         = "PLATFORM_MIGRATE_DATABASE_URL"
	EnvMigrationsDir       = "PLATFORM_MIGRATIONS_DIR"
	EnvLockTimeoutSec      = "PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC"
	EnvStatementTimeoutSec = "PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC"
	EnvAllowDown           = "PLATFORM_MIGRATE_ALLOW_DOWN"
	EnvAppPassword         = "PLATFORM_APP_PASSWORD"
)

// Defaults and bounds. A timeout of 0 is rejected on purpose: to Postgres 0 means "no timeout".
const (
	DefaultMigrationsDir       = "db/migrations"
	DefaultLockTimeoutSec      = 10
	DefaultStatementTimeoutSec = 300
	minTimeoutSec              = 1
	maxLockTimeoutSec          = 3600
	maxStatementTimeoutSec     = 86400
)

// The commands.
const (
	CmdUp               = "up"
	CmdDown             = "down"
	CmdStatus           = "status"
	CmdSetAppPassword   = "set-app-password"
	CmdUpAndSetPassword = "up-and-set-password"
)

// The values of the Required column.
const (
	requiredYes          = "yes"
	requiredNo           = "no"
	requiredPasswordCmds = "password commands"
)

// secretMinLen: below four characters a "secret" is likelier to appear by accident than to be a credential, and
// replacing it would mangle every message (the same floor internal/dsnscrub uses).
const (
	secretMinLen        = 4
	redactedPlaceholder = "[redacted]"
)

// NeedsPassword reports whether a command sets the app password and therefore needs PLATFORM_APP_PASSWORD.
func NeedsPassword(command string) bool {
	return command == CmdSetAppPassword || command == CmdUpAndSetPassword
}

// Var describes one environment variable. Default "" means the variable has no default. Required is "yes", "no",
// or "password commands" (set-app-password and up-and-set-password only).
type Var struct {
	Name     string
	Default  string
	Required string
	Secret   bool
	Meaning  string
}

// Vars is the code's list of every variable the runner reads, in the order the reference document lists them.
func Vars() []Var {
	return []Var{
		{EnvDatabaseURL, "", requiredYes, true, "PostgreSQL connection string the migrator runs as: a role that may create roles and re-own objects. DIRECT (session-mode) connection only, never a transaction pooler."},
		{EnvMigrationsDir, DefaultMigrationsDir, requiredNo, false, "Directory holding the .up.sql and .down.sql migration files, named platform_NNNN_ then a description."},
		{EnvLockTimeoutSec, strconv.Itoa(DefaultLockTimeoutSec), requiredNo, false, "Seconds to wait for the migration advisory lock, and the SET LOCAL lock_timeout of every migration file. Integer, 1 to 3600."},
		{EnvStatementTimeoutSec, strconv.Itoa(DefaultStatementTimeoutSec), requiredNo, false, "The SET LOCAL statement_timeout of every migration file, in seconds. Integer, 1 to 86400."},
		{EnvAllowDown, "false", requiredNo, false, "true permits the down command; anything else refuses it. Forward-only unless set. Must be true or false."},
		{EnvAppPassword, "", requiredPasswordCmds, true, "The platform_app login password, set by set-app-password and up-and-set-password. Never logged, never in a migration file."},
	}
}

// Config is the parsed environment. Its String and GoString never print DatabaseURL or AppPassword.
type Config struct {
	DatabaseURL         string
	MigrationsDir       string
	LockTimeoutSec      int
	StatementTimeoutSec int
	AllowDown           bool
	AppPassword         string
}

func setOrUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return "set"
}

// String is the only text a Config produces, with both secrets reduced to set or unset, so no format verb can
// print them by accident.
func (c Config) String() string {
	return fmt.Sprintf("migrate.Config{dir=%q lock_timeout_sec=%d statement_timeout_sec=%d allow_down=%t database_url=%s app_password=%s}",
		c.MigrationsDir, c.LockTimeoutSec, c.StatementTimeoutSec, c.AllowDown, setOrUnset(c.DatabaseURL), setOrUnset(c.AppPassword))
}

// GoString makes %#v as safe as %v.
func (c Config) GoString() string { return c.String() }

// Secrets lists the strings that must never appear in an error or a log line, for the output backstop (Redact).
func (c Config) Secrets() []string {
	var out []string
	add := func(s string) {
		if len(s) >= secretMinLen {
			out = append(out, s)
		}
	}
	add(c.DatabaseURL)
	if u, err := url.Parse(c.DatabaseURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
	}
	add(c.AppPassword)
	return out
}

// Redact replaces every occurrence of each secret in s. It is a backstop at the output boundary: the primary
// defence is that no error is built from a secret in the first place.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= secretMinLen {
			s = strings.ReplaceAll(s, sec, redactedPlaceholder)
		}
	}
	return s
}

// ConfigError is a usage or configuration problem. The command exits 2 for it, never 1. Its message names the
// variable and the reason and never a value.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func configErr(format string, a ...any) *ConfigError {
	return &ConfigError{Msg: fmt.Sprintf(format, a...)}
}

// LoadConfig reads the environment through getenv. An unset or blank variable takes its default. command decides
// whether PLATFORM_APP_PASSWORD is required. All problems are reported together, none with a value in it.
func LoadConfig(getenv func(string) string, command string) (Config, error) {
	var problems []string
	trimmed := func(name string) string { return strings.TrimSpace(getenv(name)) }

	cfg := Config{
		DatabaseURL:         trimmed(EnvDatabaseURL),
		MigrationsDir:       trimmed(EnvMigrationsDir),
		LockTimeoutSec:      DefaultLockTimeoutSec,
		StatementTimeoutSec: DefaultStatementTimeoutSec,
	}
	if cfg.DatabaseURL == "" {
		problems = append(problems, EnvDatabaseURL+" is required: the direct (session-mode) connection string of the migrating role")
	}
	if cfg.MigrationsDir == "" {
		cfg.MigrationsDir = DefaultMigrationsDir
	}
	intVar := func(name string, def, max int) int {
		raw := trimmed(name)
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < minTimeoutSec || n > max {
			problems = append(problems, fmt.Sprintf("%s must be an integer number of seconds from %d to %d", name, minTimeoutSec, max))
			return def
		}
		return n
	}
	cfg.LockTimeoutSec = intVar(EnvLockTimeoutSec, DefaultLockTimeoutSec, maxLockTimeoutSec)
	cfg.StatementTimeoutSec = intVar(EnvStatementTimeoutSec, DefaultStatementTimeoutSec, maxStatementTimeoutSec)

	switch raw := strings.ToLower(trimmed(EnvAllowDown)); raw {
	case "", "false":
	case "true":
		cfg.AllowDown = true
	default:
		problems = append(problems, EnvAllowDown+" must be true or false")
	}

	// The password is deliberately not trimmed: leading or trailing spaces can be part of it. Only an all-blank
	// value counts as unset.
	if pw := getenv(EnvAppPassword); strings.TrimSpace(pw) != "" {
		cfg.AppPassword = pw
	} else if NeedsPassword(command) {
		problems = append(problems, EnvAppPassword+" is required by "+command+": the platform_app login password")
	}

	if len(problems) > 0 {
		return Config{}, &ConfigError{Msg: strings.Join(problems, "; ")}
	}
	return cfg, nil
}
