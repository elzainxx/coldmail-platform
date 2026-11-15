package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const goodURL = "postgres://platform_app:pw@db.example:5432/postgres"

// envOf is an injected getenv: Load never sees the process environment in these tests.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// base is the smallest environment Load accepts.
func base() map[string]string { return map[string]string{VarDatabaseURL: goodURL} }

func with(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func TestVars_AreWellFormed(t *testing.T) {
	vars := Vars()
	if len(vars) == 0 {
		t.Fatal("Vars() is empty: every check below would pass over nothing")
	}
	seen := map[string]bool{}
	for _, v := range vars {
		if !regexp.MustCompile(`^PLATFORM_[A-Z0-9_]+$`).MatchString(v.Name) {
			t.Errorf("%q is not a PLATFORM_ upper-case name", v.Name)
		}
		if seen[v.Name] {
			t.Errorf("%s is declared twice", v.Name)
		}
		seen[v.Name] = true
		if strings.TrimSpace(v.Doc) == "" || strings.ContainsAny(v.Doc, "|\n\r") {
			t.Errorf("%s: Doc must be one non-empty line without '|' (it is a markdown table cell): %q", v.Name, v.Doc)
		}
		if v.Required && v.Default != "" {
			t.Errorf("%s is required and has a default %q: one of the two is a lie", v.Name, v.Default)
		}
		if !v.Required && v.Default == "" {
			t.Errorf("%s is optional and has no default: Load would have nothing to fall back on", v.Name)
		}
	}
	// The one secret the platform has today, and it must be both secret and required.
	var db *Var
	for i := range vars {
		if vars[i].Name == VarDatabaseURL {
			db = &vars[i]
		}
	}
	if db == nil || !db.Secret || !db.Required {
		t.Errorf("%s must be declared Required and Secret, got %+v", VarDatabaseURL, db)
	}

	// Vars returns a copy: mutating it must not change what the next caller sees.
	vars[0].Default = "mutated"
	if Vars()[0].Default == "mutated" {
		t.Fatal("Vars() returned its own backing array")
	}
}

func TestLoad_DefaultsAreTheDocumentedOnes(t *testing.T) {
	// The defaults the task specified, pinned independently of Vars() so that an accidental edit of a
	// default in code (and its table row) is a visible diff in a test, not just a quiet doc change.
	want := map[string]string{
		"PLATFORM_HTTP_ADDR":               ":8080",
		"PLATFORM_LOG_LEVEL":               "info",
		"PLATFORM_ENV":                     "development",
		"PLATFORM_SHUTDOWN_TIMEOUT_SEC":    "30",
		"PLATFORM_READYZ_TIMEOUT_MS":       "2000",
		"PLATFORM_READ_HEADER_TIMEOUT_SEC": "10",
	}
	got := map[string]string{}
	for _, v := range Vars() {
		if !v.Required {
			got[v.Name] = v.Default
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Vars() defaults = %v, want %v", got, want)
	}

	cfg, err := Load(envOf(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != "info" || cfg.Env != "development" ||
		cfg.ShutdownTimeout != 30*time.Second || cfg.ReadyzTimeout != 2*time.Second ||
		cfg.ReadHeaderTimeout != 10*time.Second || cfg.DatabaseURL.Reveal() != goodURL {
		t.Fatalf("defaults did not reach the Config: %v", cfg)
	}

	// Paired control: a blank value means "unset", so an environment that spells every default out loud
	// (and one that sets them to whitespace) loads to the same Config as one that sets nothing.
	explicit, blank := base(), base()
	for k, v := range got {
		explicit[k] = v
		blank[k] = "  "
	}
	for name, env := range map[string]map[string]string{"explicit defaults": explicit, "blank values": blank} {
		again, err := Load(envOf(env))
		if err != nil || again != cfg {
			t.Errorf("%s loaded to %v (err %v), want the same Config as the unset environment", name, again, err)
		}
	}
}

// ---- consumers: every dial is read by Load and changes the Config -----------------------------------------

// neverRead reports the declared variables load never asked getenv for, and the names it asked for that
// are not declared. A getenv that records is the instrument; load is injected so a test can aim it at a
// loader known to be broken.
func neverRead(vars []Var, load func(func(string) string)) (unread, undeclared []string) {
	asked := map[string]bool{}
	load(func(k string) string { asked[k] = true; return "" })
	declared := map[string]bool{}
	for _, v := range vars {
		declared[v.Name] = true
		if !asked[v.Name] {
			unread = append(unread, v.Name)
		}
	}
	for k := range asked {
		if !declared[k] {
			undeclared = append(undeclared, k)
		}
	}
	sort.Strings(unread)
	sort.Strings(undeclared)
	return unread, undeclared
}

func TestLoad_EveryVarIsReadAndNothingElseIs(t *testing.T) {
	if len(Vars()) == 0 {
		t.Fatal("no Vars: the check below would pass over nothing")
	}
	unread, undeclared := neverRead(Vars(), func(g func(string) string) { _, _ = Load(g) })
	if len(unread) != 0 {
		t.Errorf("Vars() declares %v but Load never reads them: a dial with no consumer", unread)
	}
	if len(undeclared) != 0 {
		t.Errorf("Load reads %v, which Vars() does not declare: the table is not the single source of truth", undeclared)
	}
}

func TestLoad_TheReadCheckCanFail(t *testing.T) {
	// A loader that forgets PLATFORM_ENV and one that reads a variable nobody declared.
	forgetful := func(g func(string) string) {
		for _, v := range Vars() {
			if v.Name != VarEnv {
				g(v.Name)
			}
		}
	}
	unread, _ := neverRead(Vars(), forgetful)
	if !reflect.DeepEqual(unread, []string{VarEnv}) {
		t.Errorf("a loader that skips %s was reported as unread=%v", VarEnv, unread)
	}
	sneaky := func(g func(string) string) {
		for _, v := range Vars() {
			g(v.Name)
		}
		g("PLATFORM_UNDECLARED_BACKDOOR")
	}
	if _, undeclared := neverRead(Vars(), sneaky); !reflect.DeepEqual(undeclared, []string{"PLATFORM_UNDECLARED_BACKDOOR"}) {
		t.Errorf("a loader that reads an undeclared variable was reported as undeclared=%v", undeclared)
	}
	// And the real loader is clean under the same instrument (the pairing).
	if unread, undeclared := neverRead(Vars(), func(g func(string) string) { _, _ = Load(g) }); len(unread)+len(undeclared) != 0 {
		t.Errorf("control failed: the real Load is flagged, unread=%v undeclared=%v", unread, undeclared)
	}
}

// inert returns the declared variables whose value, changed to alt[name], leaves the loaded Config
// untouched (a variable read and then dropped), plus the declared variables that have no alternative
// value in alt (so a newly added Var forces this table to grow).
func inert(vars []Var, alt map[string]string, load func(func(string) string) (Config, error)) (dropped, noAlt []string, err error) {
	baseline, err := load(envOf(base()))
	if err != nil {
		return nil, nil, fmt.Errorf("baseline load: %w", err)
	}
	for _, v := range vars {
		a, ok := alt[v.Name]
		if !ok {
			noAlt = append(noAlt, v.Name)
			continue
		}
		got, err := load(envOf(with(base(), v.Name, a)))
		if err != nil {
			return nil, nil, fmt.Errorf("%s=%q is not a valid alternative: %w", v.Name, a, err)
		}
		if got == baseline {
			dropped = append(dropped, v.Name)
		}
	}
	return dropped, noAlt, nil
}

var alternatives = map[string]string{
	VarHTTPAddr:             "127.0.0.1:9090",
	VarDatabaseURL:          "postgres://platform_app:other@elsewhere:5432/other",
	VarLogLevel:             "debug",
	VarEnv:                  "production",
	VarShutdownTimeoutSec:   "45",
	VarReadyzTimeoutMS:      "750",
	VarReadHeaderTimeoutSec: "7",
}

func TestLoad_EveryVarChangesTheConfig(t *testing.T) {
	dropped, noAlt, err := inert(Vars(), alternatives, Load)
	if err != nil {
		t.Fatal(err)
	}
	if len(noAlt) != 0 {
		t.Errorf("no alternative value for %v: add one to `alternatives` so the new dial is checked for a consumer", noAlt)
	}
	if len(dropped) != 0 {
		t.Errorf("%v are read by Load but changing them changes nothing in Config: a dial with no consumer", dropped)
	}
}

func TestLoad_TheConsumerCheckCanFail(t *testing.T) {
	// A loader that reads PLATFORM_ENV and then ignores it.
	ignoresEnv := func(g func(string) string) (Config, error) {
		return Load(func(k string) string {
			if k == VarEnv {
				return ""
			}
			return g(k)
		})
	}
	dropped, _, err := inert(Vars(), alternatives, ignoresEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dropped, []string{VarEnv}) {
		t.Fatalf("a loader that ignores %s was reported as dropped=%v", VarEnv, dropped)
	}
	// A Var added with no alternative value is reported, not skipped.
	_, noAlt, _ := inert(append(Vars(), Var{Name: "PLATFORM_NEW_DIAL", Default: "1", Doc: "x"}), alternatives, Load)
	if !reflect.DeepEqual(noAlt, []string{"PLATFORM_NEW_DIAL"}) {
		t.Fatalf("a new Var without an alternative was reported as %v", noAlt)
	}
}

func TestLoad_DoesNotReadTheProcessEnvironment(t *testing.T) {
	t.Setenv(VarEnv, "from-the-process")
	t.Setenv(VarDatabaseURL, "postgres://from-the-process")

	cfg, err := Load(envOf(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "development" || cfg.DatabaseURL.Reveal() != goodURL {
		t.Fatalf("Load picked up the process environment: %v", cfg)
	}
	// Paired control: the variables are really set, and a getenv that reads the process sees them.
	viaOS, err := Load(os.Getenv)
	if err != nil || viaOS.Env != "from-the-process" {
		t.Fatalf("control failed: Load(os.Getenv) = %v, %v", viaOS, err)
	}
}

// ---- validation ------------------------------------------------------------------------------------------

func TestLoad_InvalidValuesNameTheVariableAndNeverEchoTheValue(t *testing.T) {
	cases := []struct {
		name, value, reason string
	}{
		{VarHTTPAddr, "no-port-XYZ123", "host:port"},
		{VarHTTPAddr, ":99999", "host:port"},
		{VarHTTPAddr, ":http", "host:port"},
		{VarLogLevel, "loud-XYZ123", "one of"},
		{VarLogLevel, "warning", "one of"},
		{VarShutdownTimeoutSec, "soon-XYZ123", "integer"},
		{VarShutdownTimeoutSec, "99999999999999999999", "integer"},
		{VarShutdownTimeoutSec, "0", "between 1 and 3600"},
		{VarShutdownTimeoutSec, "-5", "between 1 and 3600"},
		{VarShutdownTimeoutSec, "3601", "between 1 and 3600"},
		{VarReadyzTimeoutMS, "fast-XYZ123", "integer"},
		{VarReadyzTimeoutMS, "0", "between 1 and 60000"},
		{VarReadyzTimeoutMS, "60001", "between 1 and 60000"},
		{VarReadHeaderTimeoutSec, "2.5", "integer"},
		{VarReadHeaderTimeoutSec, "0", "between 1 and 3600"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			_, err := Load(envOf(with(base(), tc.name, tc.value)))
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("Load = %v, want *Error", err)
			}
			if len(ce.Problems) != 1 || ce.Problems[0].Name != tc.name {
				t.Fatalf("problems = %+v, want exactly one, naming %s", ce.Problems, tc.name)
			}
			if !strings.Contains(ce.Problems[0].Reason, tc.reason) {
				t.Errorf("reason %q does not contain %q", ce.Problems[0].Reason, tc.reason)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("the error text does not name the variable: %v", err)
			}
			if len(tc.value) >= 5 && strings.Contains(err.Error(), tc.value) {
				t.Errorf("the error text echoes the rejected value: %v", err)
			}
		})
	}
}

func TestLoad_AcceptsTheEdgesOfEveryRange(t *testing.T) {
	// The pairing for the table above: the boundary values just inside each range load fine, so the
	// rejections are about the value and not about the variable being unusable.
	cfg, err := Load(envOf(map[string]string{
		VarDatabaseURL:          goodURL,
		VarHTTPAddr:             "127.0.0.1:0",
		VarLogLevel:             "WARN",
		VarShutdownTimeoutSec:   "3600",
		VarReadyzTimeoutMS:      "1",
		VarReadHeaderTimeoutSec: "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:0" || cfg.LogLevel != "warn" || cfg.ShutdownTimeout != time.Hour ||
		cfg.ReadyzTimeout != time.Millisecond || cfg.ReadHeaderTimeout != time.Second {
		t.Fatalf("edge values loaded wrongly: %v", cfg)
	}
}

func TestLoad_RequiredVariableMissingBlankOrWhitespace(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":      {},
		"empty":      {VarDatabaseURL: ""},
		"whitespace": {VarDatabaseURL: " \t "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(envOf(env))
			var ce *Error
			if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != VarDatabaseURL {
				t.Fatalf("Load = %v, want one problem naming %s", err, VarDatabaseURL)
			}
			if !strings.Contains(err.Error(), VarDatabaseURL) || !strings.Contains(err.Error(), "required") {
				t.Fatalf("message does not say which variable is required: %v", err)
			}
		})
	}
	if _, err := Load(envOf(base())); err != nil {
		t.Fatalf("control failed: the base environment is rejected: %v", err)
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("a nil getenv was accepted")
	}
}

func TestLoad_ReportsEveryProblemAtOnceInDeclarationOrder(t *testing.T) {
	_, err := Load(envOf(map[string]string{
		VarHTTPAddr:             "nope",
		VarLogLevel:             "nope",
		VarReadHeaderTimeoutSec: "nope",
	}))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("Load = %v", err)
	}
	var names []string
	for _, p := range ce.Problems {
		names = append(names, p.Name)
	}
	want := []string{VarHTTPAddr, VarDatabaseURL, VarLogLevel, VarReadHeaderTimeoutSec}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("problems named %v, want %v", names, want)
	}
}

func TestLoad_ErrorsNeverContainTheSecret(t *testing.T) {
	const canary = "CANARY-s3cr3t-7731"
	secretURL := "postgres://platform_app:" + canary + "@db.example:5432/postgres"

	// Every other variable invalid while the secret is set: the error is non-empty (so the absence of the
	// canary is not an artefact of Load returning nothing) and still has no canary.
	bad := map[string]string{
		VarDatabaseURL:          secretURL,
		VarHTTPAddr:             "nope",
		VarLogLevel:             "nope",
		VarShutdownTimeoutSec:   "nope",
		VarReadyzTimeoutMS:      "nope",
		VarReadHeaderTimeoutSec: "nope",
	}
	_, err := Load(envOf(bad))
	var ce *Error
	if !errors.As(err, &ce) || len(ce.Problems) != 5 {
		t.Fatalf("control failed: expected five problems, got %v", err)
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(fmt.Sprintf("%+v %#v %q", ce, ce, err), canary) {
		t.Fatalf("the secret leaked into a validation error: %v", err)
	}

	// And the same with the secret itself being the problem (blank next to a set value elsewhere).
	_, err = Load(envOf(map[string]string{VarLogLevel: canary}))
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("a bad non-secret value was echoed or accepted: %v", err)
	}
}

// ---- printing --------------------------------------------------------------------------------------------

func TestConfig_PrintingNeverRevealsTheDatabaseURL(t *testing.T) {
	const canary = "CANARY-pw-9f3a1c"
	url := "postgres://platform_app:" + canary + "@db.example:5432/postgres"
	cfg, err := Load(envOf(with(base(), VarDatabaseURL, url)))
	if err != nil {
		t.Fatal(err)
	}
	// Paired control 1: the value really is in the Config, behind Reveal.
	if cfg.DatabaseURL.Reveal() != url {
		t.Fatalf("control failed: Reveal() = %q", cfg.DatabaseURL.Reveal())
	}

	sinks := map[string]string{
		"String":      cfg.String(),
		"GoString":    cfg.GoString(),
		"Sprint":      fmt.Sprint(cfg),
		"Sprintln":    fmt.Sprintln(cfg, &cfg),
		"Secret.Text": cfg.DatabaseURL.String() + cfg.DatabaseURL.GoString(),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%c", "%e"} {
		sinks["Config "+verb] = fmt.Sprintf(verb, cfg)
		sinks["*Config "+verb] = fmt.Sprintf(verb, &cfg)
		sinks["Secret "+verb] = fmt.Sprintf(verb, cfg.DatabaseURL)
		sinks["struct{Secret} "+verb] = fmt.Sprintf(verb, struct{ U Secret }{cfg.DatabaseURL})
	}
	for _, v := range []any{cfg, &cfg, cfg.DatabaseURL, struct{ U Secret }{cfg.DatabaseURL}} {
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
		l := slog.New(handler)
		l.Info("value", "config", cfg)
		l.Info("pointer", "config", &cfg)
		l.Info("field", "database_url", cfg.DatabaseURL)
		l.Info("group", slog.Group("g", slog.Any("c", cfg)))
		sinks["slog "+h] = buf.String()
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		for _, needle := range []string{canary, "platform_app", "db.example"} {
			if strings.Contains(out, needle) {
				t.Errorf("%s reveals %q: %s", name, needle, out)
			}
		}
	}

	// Paired control 2: the same sinks DO leak a plain string field, so the assertions above can fail.
	type naive struct{ DatabaseURL string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{url})
	leaky := map[string]string{"slog": buf.String()}
	for _, verb := range []string{"%+v", "%d"} { // %d is the verb a Stringer does not intercept
		leaky["fmt "+verb] = fmt.Sprintf(verb, naive{url})
	}
	if b, err := json.Marshal(naive{url}); err == nil {
		leaky["json"] = string(b)
	}
	for name, out := range leaky {
		if !strings.Contains(out, canary) {
			t.Errorf("control failed: %s of a plain string did not contain the canary, so the checks above prove nothing: %s", name, out)
		}
	}
}

func TestConfig_LogValueShowsTheNonSecretsAndRedactsTheSecret(t *testing.T) {
	cfg, err := Load(envOf(with(base(), VarEnv, "staging")))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "config", cfg)

	var line struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	if line.Config["env"] != "staging" || line.Config["http_addr"] != ":8080" || line.Config["log_level"] != "info" {
		t.Errorf("the non-secret fields are missing from the log value: %v", line.Config)
	}
	if line.Config["database_url"] != "[redacted]" {
		t.Errorf("database_url = %v, want the redaction marker", line.Config["database_url"])
	}

	// Every Config field is rendered: a field added without a log attribute (or the reverse) is caught.
	fields := reflect.TypeFor[Config]().NumField()
	if fields == 0 || len(cfg.LogValue().Group()) != fields {
		t.Errorf("Config has %d fields but LogValue renders %d", fields, len(cfg.LogValue().Group()))
	}
	if !strings.Contains(cfg.String(), "env=staging") || !strings.Contains(cfg.String(), "database_url=[redacted]") {
		t.Errorf("String() = %s", cfg.String())
	}
}
