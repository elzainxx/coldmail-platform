package migrate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// env builds a getenv over a fixed map; a missing name reads as unset.
func env(kv map[string]string) func(string) string {
	return func(name string) string { return kv[name] }
}

const (
	testDSN = "postgres://migrator:Mig-secret-5521@db.example:5432/platform"
	testPW  = "App-secret-7788"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN}), CmdUp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != testDSN {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.MigrationsDir != "db/migrations" || cfg.LockTimeoutSec != 10 || cfg.StatementTimeoutSec != 300 || cfg.AllowDown || cfg.AppPassword != "" {
		t.Errorf("defaults wrong: %v", cfg)
	}
}

func TestLoadConfig_BlankTakesTheDefault(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		EnvDatabaseURL: testDSN, EnvMigrationsDir: "   ", EnvLockTimeoutSec: "", EnvStatementTimeoutSec: "\t", EnvAllowDown: " ",
	}), CmdUp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MigrationsDir != DefaultMigrationsDir || cfg.LockTimeoutSec != DefaultLockTimeoutSec || cfg.StatementTimeoutSec != DefaultStatementTimeoutSec || cfg.AllowDown {
		t.Errorf("blank values did not take their defaults: %v", cfg)
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		EnvDatabaseURL: " " + testDSN + "\n", EnvMigrationsDir: "/db/m", EnvLockTimeoutSec: "2", EnvStatementTimeoutSec: "45",
		EnvAllowDown: "TRUE", EnvAppPassword: " pw with spaces ",
	}), CmdUpAndSetPassword)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != testDSN {
		t.Errorf("a trailing newline on the connection string must be trimmed, got %q", cfg.DatabaseURL)
	}
	if cfg.MigrationsDir != "/db/m" || cfg.LockTimeoutSec != 2 || cfg.StatementTimeoutSec != 45 || !cfg.AllowDown {
		t.Errorf("overrides not applied: %v", cfg)
	}
	if cfg.AppPassword != " pw with spaces " {
		t.Errorf("the password must be kept byte for byte, got %q", cfg.AppPassword)
	}
}

func TestLoadConfig_DatabaseURLIsRequiredAndTheErrorNamesIt(t *testing.T) {
	for _, blank := range []string{"", "   ", "\n"} {
		_, err := LoadConfig(env(map[string]string{EnvDatabaseURL: blank}), CmdStatus)
		var ce *ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), EnvDatabaseURL) {
			t.Errorf("DSN %q: err = %v, want a *ConfigError naming %s", blank, err, EnvDatabaseURL)
		}
	}
}

// Every invalid value is rejected, the error names the VARIABLE, and it never echoes the value.
func TestLoadConfig_InvalidValuesNameTheVariableNotTheValue(t *testing.T) {
	bad := []struct{ name, value string }{
		{EnvLockTimeoutSec, "ten"}, {EnvLockTimeoutSec, "0"}, {EnvLockTimeoutSec, "-3"}, {EnvLockTimeoutSec, "3601"},
		{EnvLockTimeoutSec, "1.5"}, {EnvLockTimeoutSec, "10s"}, {EnvLockTimeoutSec, "99999999999999999999"},
		{EnvStatementTimeoutSec, "abc"}, {EnvStatementTimeoutSec, "0"}, {EnvStatementTimeoutSec, "86401"}, {EnvStatementTimeoutSec, "-1"},
		{EnvAllowDown, "yes"}, {EnvAllowDown, "1"}, {EnvAllowDown, "tru"},
	}
	for _, b := range bad {
		secretish := b.value + "-s3cr3t-zz91" // a value that would be recognisable if echoed
		for _, v := range []string{b.value, secretish} {
			_, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN, b.name: v}), CmdUp)
			if err == nil {
				if v == b.value {
					t.Errorf("%s=%q was accepted", b.name, v)
				}
				continue
			}
			if !strings.Contains(err.Error(), b.name) {
				t.Errorf("%s=%q: error %q does not name the variable", b.name, v, err)
			}
			if strings.Contains(err.Error(), "s3cr3t-zz91") {
				t.Errorf("%s=%q: error %q echoes the value", b.name, v, err)
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Errorf("%s=%q: error is %T, want *ConfigError (exit 2)", b.name, v, err)
			}
		}
	}
	// Control: the boundaries themselves are accepted.
	for name, ok := range map[string][]string{EnvLockTimeoutSec: {"1", "3600"}, EnvStatementTimeoutSec: {"1", "86400"}, EnvAllowDown: {"true", "false", "False"}} {
		for _, v := range ok {
			if _, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN, name: v}), CmdUp); err != nil {
				t.Errorf("%s=%q was rejected: %v", name, v, err)
			}
		}
	}
}

func TestLoadConfig_AllProblemsAreReportedTogether(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{EnvLockTimeoutSec: "x", EnvAllowDown: "maybe"}), CmdSetAppPassword)
	if err == nil {
		t.Fatal("no error")
	}
	for _, name := range []string{EnvDatabaseURL, EnvLockTimeoutSec, EnvAllowDown, EnvAppPassword} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q should name %s: an operator should not have to fix one variable per run", err, name)
		}
	}
}

func TestLoadConfig_PasswordIsRequiredOnlyByThePasswordCommands(t *testing.T) {
	noPassword := env(map[string]string{EnvDatabaseURL: testDSN})
	for _, cmd := range []string{CmdSetAppPassword, CmdUpAndSetPassword} {
		_, err := LoadConfig(noPassword, cmd)
		var ce *ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), EnvAppPassword) {
			t.Errorf("%s with no password: err = %v, want a *ConfigError naming %s", cmd, err, EnvAppPassword)
		}
		if _, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN, EnvAppPassword: "   "}), cmd); err == nil {
			t.Errorf("%s with an all-blank password was accepted", cmd)
		}
		if _, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN, EnvAppPassword: testPW}), cmd); err != nil {
			t.Errorf("%s with a password: %v", cmd, err)
		}
	}
	for _, cmd := range []string{CmdUp, CmdDown, CmdStatus} {
		if _, err := LoadConfig(noPassword, cmd); err != nil {
			t.Errorf("%s needs no password but LoadConfig said: %v", cmd, err)
		}
	}
	if !NeedsPassword(CmdSetAppPassword) || !NeedsPassword(CmdUpAndSetPassword) || NeedsPassword(CmdUp) || NeedsPassword(CmdDown) || NeedsPassword(CmdStatus) {
		t.Error("NeedsPassword disagrees with the documented set of password commands")
	}
}

// "Every dial has a consumer": for each variable Vars() lists, setting it to a non-default value changes the
// Config. A variable added to Vars() without a case here fails, so a new dial cannot ship unread.
func TestConfig_EveryVarIsConsumed(t *testing.T) {
	cases := map[string]struct {
		value string
		took  func(Config) bool
	}{
		EnvDatabaseURL:         {"postgres://u@h:5432/d", func(c Config) bool { return c.DatabaseURL == "postgres://u@h:5432/d" }},
		EnvMigrationsDir:       {"/other/dir", func(c Config) bool { return c.MigrationsDir == "/other/dir" }},
		EnvLockTimeoutSec:      {"7", func(c Config) bool { return c.LockTimeoutSec == 7 }},
		EnvStatementTimeoutSec: {"42", func(c Config) bool { return c.StatementTimeoutSec == 42 }},
		EnvAllowDown:           {"true", func(c Config) bool { return c.AllowDown }},
		EnvAppPassword:         {"pw-0000-xyz", func(c Config) bool { return c.AppPassword == "pw-0000-xyz" }},
	}
	seen := map[string]bool{}
	for _, v := range Vars() {
		seen[v.Name] = true
		c, ok := cases[v.Name]
		if !ok {
			t.Errorf("%s is in Vars() but has no consumer case: a variable nothing reads is a dial that does nothing", v.Name)
			continue
		}
		base := map[string]string{EnvDatabaseURL: testDSN, EnvAppPassword: testPW}
		before, err := LoadConfig(env(base), CmdUpAndSetPassword)
		if err != nil {
			t.Fatal(err)
		}
		if c.took(before) && v.Name != EnvDatabaseURL && v.Name != EnvAppPassword {
			t.Errorf("%s: the case's expectation already holds with the variable unset, so it proves nothing", v.Name)
		}
		base[v.Name] = c.value
		after, err := LoadConfig(env(base), CmdUpAndSetPassword)
		if err != nil {
			t.Errorf("%s=%q: %v", v.Name, c.value, err)
			continue
		}
		if !c.took(after) {
			t.Errorf("%s=%q did not reach the Config: %v", v.Name, c.value, after)
		}
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("%s has a consumer case but is not in Vars(): it would be read and never documented", name)
		}
	}
}

func TestVars_AreWellFormed(t *testing.T) {
	vars := Vars()
	if len(vars) == 0 {
		t.Fatal("Vars() is empty")
	}
	seen := map[string]bool{}
	for _, v := range vars {
		if !strings.HasPrefix(v.Name, "PLATFORM_") {
			t.Errorf("%s: the platform's variables all start with PLATFORM_", v.Name)
		}
		if seen[v.Name] {
			t.Errorf("%s is listed twice", v.Name)
		}
		seen[v.Name] = true
		if v.Meaning == "" || strings.Contains(v.Meaning, "|") {
			t.Errorf("%s: Meaning must be non-empty and contain no table separator", v.Name)
		}
		if v.Required != "yes" && v.Required != "no" && v.Required != "password commands" {
			t.Errorf("%s: Required = %q", v.Name, v.Required)
		}
		if v.Required == "yes" && v.Default != "" {
			t.Errorf("%s: a required variable has no default", v.Name)
		}
	}
	for _, name := range []string{EnvDatabaseURL, EnvAppPassword} {
		for _, v := range vars {
			if v.Name == name && !v.Secret {
				t.Errorf("%s must be marked secret", name)
			}
		}
	}
}

func TestConfig_NeverPrintsItsSecrets(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{EnvDatabaseURL: testDSN, EnvAppPassword: testPW}), CmdUpAndSetPassword)
	if err != nil {
		t.Fatal(err)
	}
	all := fmt.Sprintf("%v|%+v|%#v|%s|%q|%d", cfg, cfg, cfg, cfg, cfg.String(), cfg.LockTimeoutSec) + fmt.Sprint(&cfg)
	for _, secret := range []string{testPW, "Mig-secret-5521", testDSN} {
		if strings.Contains(all, secret) {
			t.Errorf("a formatted Config contains %q: %s", secret, all)
		}
	}
	if !strings.Contains(all, "database_url=set") || !strings.Contains(all, "app_password=set") {
		t.Errorf("a Config should say its secrets are set, got: %s", all)
	}
	// Control: the secrets list the backstop uses does contain them, so the redaction below is not vacuous.
	got := strings.Join(cfg.Secrets(), "|")
	for _, secret := range []string{testPW, "Mig-secret-5521", testDSN} {
		if !strings.Contains(got, secret) {
			t.Errorf("Secrets() is missing %q", secret)
		}
	}
}

func TestRedact(t *testing.T) {
	cfg := Config{DatabaseURL: testDSN, AppPassword: testPW}
	msg := fmt.Sprintf("dial %s failed; password %s rejected; also Mig-secret-5521 again", testDSN, testPW)
	got := Redact(msg, cfg.Secrets()...)
	for _, secret := range []string{testDSN, testPW, "Mig-secret-5521"} {
		if strings.Contains(got, secret) {
			t.Errorf("Redact left %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "dial") || !strings.Contains(got, "[redacted]") {
		t.Errorf("Redact should keep the message and mark what it removed, got %q", got)
	}
	// Control: a secret shorter than the floor is left alone (it would mangle every message), and a message without
	// a secret is returned byte for byte.
	if got := Redact("port 5432 open", "5"); got != "port 5432 open" {
		t.Errorf("a one-character secret must not be replaced, got %q", got)
	}
	if got := Redact("nothing to hide", cfg.Secrets()...); got != "nothing to hide" {
		t.Errorf("got %q", got)
	}
}
