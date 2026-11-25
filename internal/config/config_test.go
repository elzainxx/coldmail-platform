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

const (
	goodURL        = "postgres://platform_app:pw@db.example:5432/postgres"
	goodM3URL      = "http://m3.example:8080"
	goodM3Operator = "operator-operator-ok"
)

// envOf is an injected getenv: Load never sees the process environment in these tests.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// base is the smallest environment Load accepts: the three required variables.
func base() map[string]string {
	return map[string]string{VarDatabaseURL: goodURL, VarM3URL: goodM3URL, VarM3AdminToken: goodM3Operator}
}

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
	// The two secrets the platform has (the database login and module 3's operator key), each both secret
	// and required; module 3's URL is required and is NOT a secret (it is printed in the loaded state).
	byName := map[string]Var{}
	for _, v := range vars {
		byName[v.Name] = v
	}
	for _, name := range []string{VarDatabaseURL, VarM3AdminToken} {
		if v, ok := byName[name]; !ok || !v.Secret || !v.Required {
			t.Errorf("%s must be declared Required and Secret, got %+v", name, v)
		}
	}
	if v, ok := byName[VarM3URL]; !ok || v.Secret || !v.Required {
		t.Errorf("%s must be declared Required and not Secret, got %+v", VarM3URL, v)
	}
	// The module 1 key secret is the third secret, and the only one that is optional: unset means "not configured" (its default is the documented marker, never a usable value).
	if v, ok := byName[VarM1KeySecret]; !ok || !v.Secret || v.Required || v.Default != "(unset)" {
		t.Errorf("%s must be declared Secret, not Required, with the default (unset), got %+v", VarM1KeySecret, v)
	}
	// The vendor switch (Phase 4 B3): optional, not secret, and its default is the real value false (not the "(unset)" marker: it has no "not configured" state, off is a state).
	if v, ok := byName[VarM1VendorReachable]; !ok || v.Secret || v.Required || v.Default != "false" {
		t.Errorf("%s must be declared optional, not secret, with the default false, got %+v", VarM1VendorReachable, v)
	}
	// The four optional module dials: the two base URLs are not secrets, the two service tokens are, and none of the four is required (unset means "not configured", the documented marker).
	for _, c := range []struct {
		name   string
		secret bool
	}{{VarM1URL, false}, {VarM2URL, false}, {VarM2ServiceToken, true}, {VarM3ServiceToken, true}} {
		if v, ok := byName[c.name]; !ok || v.Secret != c.secret || v.Required || v.Default != "(unset)" {
			t.Errorf("%s must be declared optional, with the default (unset) and Secret=%v, got %+v", c.name, c.secret, v)
		}
	}
	// The token-verification dials (Phase 4 B14a): the HS256 secret is a secret, the JWKS URL and the issuer are not, and all three are optional with the (unset) marker; the audience and the
	// leeway are optional with a real default.
	for _, c := range []struct {
		name   string
		secret bool
	}{{VarJWTSecret, true}, {VarJWKSURL, false}, {VarJWTIssuer, false}} {
		if v, ok := byName[c.name]; !ok || v.Secret != c.secret || v.Required || v.Default != "(unset)" {
			t.Errorf("%s must be declared optional, with the default (unset) and Secret=%v, got %+v", c.name, c.secret, v)
		}
	}
	for name, def := range map[string]string{VarJWTAudience: "authenticated", VarJWTLeewaySec: "30"} {
		if v, ok := byName[name]; !ok || v.Secret || v.Required || v.Default != def {
			t.Errorf("%s must be declared optional, not secret, with the default %q, got %+v", name, def, v)
		}
	}
	// Every other variable is a non-secret dial: a new secret must be added to the list above on purpose.
	knownSecrets := map[string]bool{VarDatabaseURL: true, VarM3AdminToken: true, VarM1KeySecret: true, VarM2ServiceToken: true, VarM3ServiceToken: true, VarJWTSecret: true}
	for _, v := range vars {
		if v.Secret && !knownSecrets[v.Name] {
			t.Errorf("%s is Secret but is not one of the six known secrets: extend this test deliberately", v.Name)
		}
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

		"PLATFORM_ENTITLEMENT_EXPIRE_INTERVAL_SEC": "30",
		"PLATFORM_M3_RECONCILE_INTERVAL_SEC":       "5",
		"PLATFORM_M3_REVERIFY_INTERVAL_SEC":        "300",
		"PLATFORM_M3_REQUEST_TIMEOUT_SEC":          "10",

		"PLATFORM_MONEY_LOCK_TIMEOUT_MS": "10000",

		"PLATFORM_MODULE_TIMEOUT_MS":       "15000",
		"PLATFORM_MODULE_RETRIES":          "2",
		"PLATFORM_MODULE_RETRY_BACKOFF_MS": "250",

		"PLATFORM_M1_KEY_SECRET":  "(unset)",
		"PLATFORM_M1_KEY_VERSION": "1",

		"PLATFORM_M1_VENDOR_REACHABLE": "false",

		"PLATFORM_M1_URL":           "(unset)",
		"PLATFORM_M2_URL":           "(unset)",
		"PLATFORM_M2_SERVICE_TOKEN": "(unset)",
		"PLATFORM_M3_SERVICE_TOKEN": "(unset)",

		"PLATFORM_JWT_SECRET":     "(unset)",
		"PLATFORM_JWKS_URL":       "(unset)",
		"PLATFORM_JWT_ISSUER":     "(unset)",
		"PLATFORM_JWT_AUDIENCE":   "authenticated",
		"PLATFORM_JWT_LEEWAY_SEC": "30",

		"PLATFORM_INSTANCE_ID": "(hostname)",
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
	if cfg.M3URL != goodM3URL || cfg.M3AdminToken.Reveal() != goodM3Operator || cfg.EntitlementExpireInterval != 30*time.Second ||
		cfg.M3ReconcileInterval != 5*time.Second || cfg.M3ReverifyInterval != 300*time.Second || cfg.M3RequestTimeout != 10*time.Second {
		t.Fatalf("the module 3 defaults did not reach the Config: %v", cfg)
	}
	if cfg.MoneyLockTimeout != 10*time.Second {
		t.Fatalf("the money lock timeout default did not reach the Config: %v", cfg)
	}
	if cfg.ModuleTimeout != 15*time.Second || cfg.ModuleRetries != 2 || cfg.ModuleRetryBackoff != 250*time.Millisecond {
		t.Fatalf("the module client defaults did not reach the Config: %v", cfg)
	}
	// The optional dials' default is "not configured": an empty field, never the marker text itself.
	if cfg.M1KeyVersion != 1 {
		t.Fatalf("the module 1 key version default did not reach the Config: %d", cfg.M1KeyVersion)
	}
	if cfg.M1VendorReachable {
		t.Fatalf("the vendor switch is on in a Config built from nothing: the fallback ships disabled, so a module 1 job reserves 1 credit per row until the owner says otherwise")
	}
	if cfg.M1URL != "" || cfg.M2URL != "" || cfg.M2ServiceToken != "" || cfg.M3ServiceToken != "" || cfg.M1KeySecret != "" || cfg.InstanceID != "" {
		t.Fatalf("an optional dial's default did not load as empty: %v", cfg)
	}

	if cfg.JWTAudience != "authenticated" || cfg.JWTLeeway != 30*time.Second {
		t.Fatalf("the token-verification defaults did not reach the Config: %v", cfg)
	}
	if cfg.JWTSecret != "" || cfg.JWKSURL != "" || cfg.JWTIssuer != "" {
		t.Fatalf("an optional token-verification dial's default did not load as empty: %v", cfg)
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

	VarM3URL:                        "https://m3.internal:9443",
	VarM3AdminToken:                 "another-operator-key",
	VarEntitlementExpireIntervalSec: "77",
	VarM3ReconcileIntervalSec:       "9",
	VarM3ReverifyIntervalSec:        "600",
	VarM3RequestTimeoutSec:          "25",

	VarMoneyLockTimeoutMS: "2500",

	VarModuleTimeoutMS:      "4000",
	VarModuleRetries:        "5",
	VarModuleRetryBackoffMS: "600",

	VarM1KeySecret:  strings.Repeat("alt-", 9), // 36 bytes
	VarM1KeyVersion: "3",

	VarM1VendorReachable: "true",

	VarM1URL:          "http://m1.internal:8081",
	VarM2URL:          "https://m2.internal:9444/api",
	VarM2ServiceToken: "alt-m2-service-token",
	VarM3ServiceToken: "alt-m3-service-token",

	VarJWTSecret:    strings.Repeat("jwt-", 9), // 36 bytes
	VarJWKSURL:      "https://idp.internal/auth/v1/.well-known/jwks.json",
	VarJWTIssuer:    "https://idp.internal/auth/v1",
	VarJWTAudience:  "platform",
	VarJWTLeewaySec: "45",

	VarInstanceID: "platform-2",
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
	t.Setenv(VarM3URL, "http://from-the-process:8080")
	t.Setenv(VarM3AdminToken, "process-process-process")

	cfg, err := Load(envOf(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "development" || cfg.DatabaseURL.Reveal() != goodURL || cfg.M3URL != goodM3URL || cfg.M3AdminToken.Reveal() != goodM3Operator {
		t.Fatalf("Load picked up the process environment: %v", cfg)
	}
	// Paired control: the variables are really set, and a getenv that reads the process sees them.
	viaOS, err := Load(os.Getenv)
	if err != nil || viaOS.Env != "from-the-process" || viaOS.M3URL != "http://from-the-process:8080" || viaOS.M3AdminToken.Reveal() != "process-process-process" {
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
		{VarM3URL, "ftp://m3-XYZ123:8080", "http or https URL"},
		{VarM3URL, "m3-XYZ123:8080", "http or https URL"},
		{VarM3URL, "https://", "http or https URL"}, // a scheme and no host
		{VarM3URL, "http://user-XYZ123:pw-XYZ123@m3:8080", "no credentials"},
		{VarM3URL, "http://m3-XYZ123:8080?key=1", "http or https URL"},
		{VarM3URL, "http://m3-XYZ123:8080/#frag", "http or https URL"},
		{VarEntitlementExpireIntervalSec, "often-XYZ123", "integer"},
		{VarEntitlementExpireIntervalSec, "0", "between 1 and 3600"},
		{VarEntitlementExpireIntervalSec, "3601", "between 1 and 3600"},
		{VarM3ReconcileIntervalSec, "0", "between 1 and 3600"},
		{VarM3ReconcileIntervalSec, "3601", "between 1 and 3600"},
		{VarM3ReconcileIntervalSec, "1.5", "integer"},
		{VarM3ReverifyIntervalSec, "9", "between 10 and 86400"},
		{VarM3ReverifyIntervalSec, "86401", "between 10 and 86400"},
		{VarM3ReverifyIntervalSec, "never-XYZ123", "integer"},
		{VarM3RequestTimeoutSec, "0", "between 1 and 120"},
		{VarM3RequestTimeoutSec, "121", "between 1 and 120"},
		{VarM3RequestTimeoutSec, "-3", "between 1 and 120"},
		{VarMoneyLockTimeoutMS, "forever-XYZ123", "integer"},
		{VarMoneyLockTimeoutMS, "2.5", "integer"},
		{VarMoneyLockTimeoutMS, "0", "between 100 and 600000"}, // 0 would be "wait for ever" to PostgreSQL
		{VarMoneyLockTimeoutMS, "-1", "between 100 and 600000"},
		{VarMoneyLockTimeoutMS, "99", "between 100 and 600000"},
		{VarMoneyLockTimeoutMS, "600001", "between 100 and 600000"},
		{VarMoneyLockTimeoutMS, "99999999999999999999", "integer"},
		{VarModuleTimeoutMS, "soon-XYZ123", "integer"},
		{VarModuleTimeoutMS, "1.5", "integer"},
		{VarModuleTimeoutMS, "499", "between 500 and 300000"},
		{VarModuleTimeoutMS, "0", "between 500 and 300000"},
		{VarModuleTimeoutMS, "-1", "between 500 and 300000"},
		{VarModuleTimeoutMS, "300001", "between 500 and 300000"},
		{VarModuleRetries, "often-XYZ123", "integer"},
		{VarModuleRetries, "1.5", "integer"},
		{VarModuleRetries, "-1", "between 0 and 10"},
		{VarModuleRetries, "11", "between 0 and 10"},
		{VarModuleRetryBackoffMS, "slowly-XYZ123", "integer"},
		{VarModuleRetryBackoffMS, "0.5", "integer"},
		{VarModuleRetryBackoffMS, "-1", "between 0 and 30000"},
		{VarModuleRetryBackoffMS, "30001", "between 0 and 30000"},
		// The vendor switch (Phase 4 B3): exactly true or false; a switch that read "yes" or "1" as off would reserve a customer at the wrong rate without a word.
		{VarM1VendorReachable, "maybe-XYZ123", "must be true or false"},
		{VarM1VendorReachable, "TRUE", "must be true or false"},
		{VarM1VendorReachable, "True", "must be true or false"},
		{VarM1VendorReachable, "yes", "must be true or false"},
		{VarM1VendorReachable, "1", "must be true or false"},
		{VarM1VendorReachable, "on", "must be true or false"},
		{VarM3AdminToken, "two words-XYZ123", "printable ASCII"},
		{VarM3AdminToken, "café-key-XYZ123", "printable ASCII"},
		{VarM3AdminToken, "inner\ttab-XYZ123", "printable ASCII"},
		// The four optional module dials: the same URL rule as module 3's and the same bearer rule as its operator key, each naming its own variable.
		{VarM1URL, "ftp://m1-XYZ123:8080", "http or https URL"},
		{VarM1URL, "m1-XYZ123:8080", "http or https URL"},
		{VarM1URL, "http://user-XYZ123:pw-XYZ123@m1:8080", "no credentials"},
		{VarM1URL, "http://m1-XYZ123:8080?key=1", "http or https URL"},
		{VarM2URL, "https://", "http or https URL"}, // a scheme and no host
		{VarM2URL, "http://m2-XYZ123:8080/#frag", "http or https URL"},
		{VarM2URL, "http://user-XYZ123:pw-XYZ123@m2:8080", "no credentials"},
		{VarM2URL, "m2-XYZ123:8080", "http or https URL"},
		{VarM2ServiceToken, "two words-XYZ123", "printable ASCII"},
		{VarM2ServiceToken, "café-token-XYZ123", "printable ASCII"},
		{VarM2ServiceToken, "inner\ntoken-XYZ123", "printable ASCII"},
		{VarM3ServiceToken, "two words-XYZ123", "printable ASCII"},
		{VarM3ServiceToken, "café-token-XYZ123", "printable ASCII"},
		{VarM3ServiceToken, "inner\ttab-XYZ123", "printable ASCII"},
		// The token-verification dials (Phase 4 B14a): the secret's length and alphabet, the URL's shape, the issuer's and audience's alphabet and length, the leeway's range.
		{VarJWTSecret, "short-XYZ123", "at least 32 bytes"},
		{VarJWTSecret, strings.Repeat("a", 31), "at least 32 bytes"},
		{VarJWTSecret, strings.Repeat("a", 20) + " " + strings.Repeat("b", 20), "printable ASCII"},
		{VarJWTSecret, strings.Repeat("a", 20) + "\n" + strings.Repeat("b", 20), "printable ASCII"},
		{VarJWTSecret, strings.Repeat("é", 20), "printable ASCII"},
		{VarJWKSURL, "ftp://idp-XYZ123/jwks.json", "http or https URL"},
		{VarJWKSURL, "idp-XYZ123/jwks.json", "http or https URL"},
		{VarJWKSURL, "https://", "http or https URL"},
		{VarJWKSURL, "https://user-XYZ123:pw-XYZ123@idp/jwks.json", "no credentials"},
		{VarJWKSURL, "https://idp-XYZ123/jwks.json?apikey=1", "http or https URL"},
		{VarJWKSURL, "https://idp-XYZ123/jwks.json#f", "http or https URL"},
		{VarJWTIssuer, "two words-XYZ123", "printable ASCII"},
		{VarJWTIssuer, "café-issuer-XYZ123", "printable ASCII"},
		{VarJWTIssuer, strings.Repeat("i", 513), "at most 512"},
		{VarJWTAudience, "two words-XYZ123", "printable ASCII"},
		{VarJWTAudience, "café-XYZ123", "printable ASCII"},
		{VarJWTAudience, strings.Repeat("a", 513), "at most 512"},
		{VarJWTLeewaySec, "soon-XYZ123", "integer"},
		{VarJWTLeewaySec, "1.5", "integer"},
		{VarJWTLeewaySec, "-1", "between 0 and 300"},
		{VarJWTLeewaySec, "301", "between 0 and 300"},
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
		VarDatabaseURL:                  goodURL,
		VarHTTPAddr:                     "127.0.0.1:0",
		VarLogLevel:                     "WARN",
		VarShutdownTimeoutSec:           "3600",
		VarReadyzTimeoutMS:              "1",
		VarReadHeaderTimeoutSec:         "1",
		VarM3URL:                        "http://m3:8080",
		VarM3AdminToken:                 goodM3Operator,
		VarEntitlementExpireIntervalSec: "3600",
		VarM3ReconcileIntervalSec:       "1",
		VarM3ReverifyIntervalSec:        "10",
		VarM3RequestTimeoutSec:          "120",
		VarMoneyLockTimeoutMS:           "100",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MoneyLockTimeout != 100*time.Millisecond {
		t.Fatalf("the lower edge of the money lock timeout loaded wrongly: %v", cfg)
	}
	if top, err := Load(envOf(with(base(), VarMoneyLockTimeoutMS, "600000"))); err != nil || top.MoneyLockTimeout != 10*time.Minute {
		t.Fatalf("the upper edge of the money lock timeout was refused or loaded wrongly: %v (%v)", top, err)
	}
	// The module client dials: the lower edge of each (0 retries, an immediate retry) and the upper edge of each, with the twin just outside refused in the table above.
	if low, err := Load(envOf(with(with(with(base(), VarModuleTimeoutMS, "500"), VarModuleRetries, "0"), VarModuleRetryBackoffMS, "0"))); err != nil ||
		low.ModuleTimeout != 500*time.Millisecond || low.ModuleRetries != 0 || low.ModuleRetryBackoff != 0 {
		t.Fatalf("the lower edges of the module client dials were refused or loaded wrongly: %v (%v)", low, err)
	}
	if top, err := Load(envOf(with(with(with(base(), VarModuleTimeoutMS, "300000"), VarModuleRetries, "10"), VarModuleRetryBackoffMS, "30000"))); err != nil ||
		top.ModuleTimeout != 5*time.Minute || top.ModuleRetries != 10 || top.ModuleRetryBackoff != 30*time.Second {
		t.Fatalf("the upper edges of the module client dials were refused or loaded wrongly: %v (%v)", top, err)
	}
	if cfg.HTTPAddr != "127.0.0.1:0" || cfg.LogLevel != "warn" || cfg.ShutdownTimeout != time.Hour ||
		cfg.ReadyzTimeout != time.Millisecond || cfg.ReadHeaderTimeout != time.Second {
		t.Fatalf("edge values loaded wrongly: %v", cfg)
	}
	if cfg.M3URL != "http://m3:8080" || cfg.EntitlementExpireInterval != time.Hour || cfg.M3ReconcileInterval != time.Second ||
		cfg.M3ReverifyInterval != 10*time.Second || cfg.M3RequestTimeout != 2*time.Minute {
		t.Fatalf("module 3 edge values loaded wrongly: %v", cfg)
	}
	// The upper edge of the reverify dial, and an https URL with a port, an IPv6 host and a path prefix.
	upper, err := Load(envOf(with(with(base(), VarM3ReverifyIntervalSec, "86400"), VarM3URL, "https://[::1]:8443/m3")))
	if err != nil || upper.M3ReverifyInterval != 24*time.Hour || upper.M3URL != "https://[::1]:8443/m3" {
		t.Fatalf("the upper edge of the reverify dial, or an https/IPv6/path URL, was refused: %v (%v)", upper, err)
	}
}

// TestLoad_M3URLShapes pins which module 3 URLs load: any http or https URL with a host and no credentials,
// query or fragment, and nothing else. Each rejected shape has an accepted twin that differs in that one thing.
func TestLoad_M3URLShapes(t *testing.T) {
	accepted := []string{
		"http://m3:8080", "https://m3.internal", "http://10.0.0.5:8080/base", "HTTP://M3:8080", "http://[::1]:8080",
	}
	rejected := map[string]string{ // value -> the shape it breaks (its twin is in `accepted`)
		"ftp://m3:8080":         "scheme (twin http://m3:8080)",
		"//m3:8080":             "no scheme",
		"m3:8080":               "no scheme, a bare host:port",
		"http:///path":          "no host",
		"http://u:p@m3:8080":    "credentials",
		"http://u@m3:8080":      "a user name",
		"http://m3:8080?a=b":    "a query",
		"http://m3:8080?":       "an empty query",
		"http://m3:8080#x":      "a fragment",
		"http://m3 :8080":       "a space in the host",
		"mailto:ops@example.io": "an opaque URL",
	}
	for _, v := range accepted {
		cfg, err := Load(envOf(with(base(), VarM3URL, v)))
		if err != nil || cfg.M3URL != v {
			t.Errorf("%q was refused or altered: %v (%v)", v, cfg.M3URL, err)
		}
	}
	for v, why := range rejected {
		_, err := Load(envOf(with(base(), VarM3URL, v)))
		var ce *Error
		if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != VarM3URL {
			t.Errorf("%q (%s) loaded, or was refused for another variable: %v", v, why, err)
		}
	}
	if len(accepted) < 5 || len(rejected) < 11 {
		t.Fatal("the shape tables shrank: the check would pass over fewer cases than it was written for")
	}
}

// TestLoad_ModuleURLShapesOfTheOptionalDials runs the module 3 URL table against the two optional base URLs: one rule for the three variables. The control is the module 3 table itself (the
// accepted twins load for module 3 too), and a URL that is refused for one variable is refused for the others with that variable named, not another.
func TestLoad_ModuleURLShapesOfTheOptionalDials(t *testing.T) {
	accepted := []string{"http://m:8080", "https://m.internal", "http://10.0.0.5:8080/base", "HTTP://M:8080", "http://[::1]:8080", "  http://m:8080  "}
	rejected := map[string]string{ // value -> the shape it breaks (its twin is in `accepted`)
		"ftp://m:8080":          "scheme",
		"//m:8080":              "no scheme",
		"m:8080":                "no scheme, a bare host:port",
		"http:///path":          "no host",
		"http://u:p@m:8080":     "credentials",
		"http://u@m:8080":       "a user name",
		"http://m:8080?a=b":     "a query",
		"http://m:8080?":        "an empty query",
		"http://m:8080#x":       "a fragment",
		"http://m :8080":        "a space in the host",
		"mailto:ops@example.io": "an opaque URL",
	}
	for _, name := range []string{VarM1URL, VarM2URL, VarM3URL} {
		for _, v := range accepted {
			cfg, err := Load(envOf(with(base(), name, v)))
			if err != nil {
				t.Errorf("%s=%q was refused: %v", name, v, err)
				continue
			}
			got := map[string]string{VarM1URL: cfg.M1URL, VarM2URL: cfg.M2URL, VarM3URL: cfg.M3URL}[name]
			if got != strings.TrimSpace(v) {
				t.Errorf("%s=%q loaded as %q, want the trimmed value", name, v, got)
			}
		}
		for v, why := range rejected {
			_, err := Load(envOf(with(base(), name, v)))
			var ce *Error
			if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != name {
				t.Errorf("%s=%q (%s) loaded, or was refused for another variable: %v", name, v, why, err)
				continue
			}
			if !strings.Contains(ce.Problems[0].Reason, "http or https URL") {
				t.Errorf("%s=%q (%s): the reason is %q, want it to say what a URL here is", name, v, why, ce.Problems[0].Reason)
			}
		}
	}
	if len(accepted) < 6 || len(rejected) < 11 {
		t.Fatal("the shape tables shrank: the check would pass over fewer cases than it was written for")
	}
}

func TestLoad_RequiredVariableMissingBlankOrWhitespace(t *testing.T) {
	// Each of the three required variables, taken away from an otherwise complete environment in three
	// ways: exactly one problem, naming that variable, saying it is required.
	for _, required := range []string{VarDatabaseURL, VarM3URL, VarM3AdminToken} {
		for name, value := range map[string]*string{"unset": nil, "empty": ptr(""), "whitespace": ptr(" \t ")} {
			t.Run(required+"/"+name, func(t *testing.T) {
				env := base()
				delete(env, required)
				if value != nil {
					env[required] = *value
				}
				_, err := Load(envOf(env))
				var ce *Error
				if !errors.As(err, &ce) || len(ce.Problems) != 1 || ce.Problems[0].Name != required {
					t.Fatalf("Load = %v, want one problem naming %s", err, required)
				}
				if !strings.Contains(err.Error(), required) || !strings.Contains(err.Error(), "required") {
					t.Fatalf("message does not say which variable is required: %v", err)
				}
			})
		}
	}
	// Nothing set at all names all three, and only those.
	_, err := Load(envOf(map[string]string{}))
	var ce *Error
	if !errors.As(err, &ce) || len(ce.Problems) != 3 {
		t.Fatalf("an empty environment gave %v, want exactly the three required variables", err)
	}
	if _, err := Load(envOf(base())); err != nil {
		t.Fatalf("control failed: the base environment is rejected: %v", err)
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("a nil getenv was accepted")
	}
}

func ptr(s string) *string { return &s }

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
	want := []string{VarHTTPAddr, VarDatabaseURL, VarLogLevel, VarReadHeaderTimeoutSec, VarM3URL, VarM3AdminToken}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("problems named %v, want %v", names, want)
	}
}

func TestLoad_ErrorsNeverContainTheSecret(t *testing.T) {
	const canary = "CANARY-s3cr3t-7731"
	const m3Canary = "CANARY-m3key-4410"
	secretURL := "postgres://platform_app:" + canary + "@db.example:5432/postgres"

	// Every other variable invalid while the secrets are set: the error is non-empty (so the absence of the
	// canaries is not an artefact of Load returning nothing) and still has neither. The module 3 URL is
	// invalid too and carries credentials, which must not be echoed either.
	bad := map[string]string{
		VarDatabaseURL:                  secretURL,
		VarHTTPAddr:                     "nope",
		VarLogLevel:                     "nope",
		VarShutdownTimeoutSec:           "nope",
		VarReadyzTimeoutMS:              "nope",
		VarReadHeaderTimeoutSec:         "nope",
		VarM3URL:                        "http://operator:" + m3Canary + "@m3:8080",
		VarM3AdminToken:                 m3Canary,
		VarEntitlementExpireIntervalSec: "nope",
		VarM3ReconcileIntervalSec:       "nope",
		VarM3ReverifyIntervalSec:        "nope",
		VarM3RequestTimeoutSec:          "nope",
		VarMoneyLockTimeoutMS:           "nope",
	}
	_, err := Load(envOf(bad))
	var ce *Error
	if !errors.As(err, &ce) || len(ce.Problems) != 11 {
		t.Fatalf("control failed: expected eleven problems, got %v", err)
	}
	for _, c := range []string{canary, m3Canary} {
		if strings.Contains(err.Error(), c) || strings.Contains(fmt.Sprintf("%+v %#v %q", ce, ce, err), c) {
			t.Fatalf("the secret %q leaked into a validation error: %v", c, err)
		}
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

// TestConfig_PrintingNeverRevealsTheM3AdminToken is the module 3 half of the printing proof: the operator key
// is a Secret field like the database URL, so no sink the standard library offers renders it.
func TestConfig_PrintingNeverRevealsTheM3AdminToken(t *testing.T) {
	const canary = "CANARY-m3-operator-8841"
	cfg, err := Load(envOf(with(base(), VarM3AdminToken, canary)))
	if err != nil {
		t.Fatal(err)
	}
	// Paired control 1: the value really is in the Config, behind Reveal.
	if cfg.M3AdminToken.Reveal() != canary {
		t.Fatalf("control failed: Reveal() = %q", cfg.M3AdminToken.Reveal())
	}
	sinks := map[string]string{
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
		"Sprintln": fmt.Sprintln(cfg, &cfg),
		"token":    fmt.Sprintf("%v %+v %#v %s %q %x %d", cfg.M3AdminToken, cfg.M3AdminToken, cfg.M3AdminToken, cfg.M3AdminToken, cfg.M3AdminToken, cfg.M3AdminToken, cfg.M3AdminToken),
	}
	for _, h := range []string{"json", "text"} {
		var buf bytes.Buffer
		var handler slog.Handler = slog.NewJSONHandler(&buf, nil)
		if h == "text" {
			handler = slog.NewTextHandler(&buf, nil)
		}
		slog.New(handler).Info("loaded", "config", cfg, "token", cfg.M3AdminToken)
		sinks["slog "+h] = buf.String()
	}
	for _, v := range []any{cfg, &cfg, cfg.M3AdminToken} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sinks[fmt.Sprintf("json %T", v)] = string(b)
	}
	for name, out := range sinks {
		if out == "" {
			t.Errorf("%s produced no output: the check would pass over nothing", name)
		}
		if strings.Contains(out, canary) {
			t.Errorf("%s reveals the operator key: %s", name, out)
		}
	}
	// Paired control 2: a plain string field of the same shape DOES leak through the same sinks.
	type naive struct{ M3AdminToken string }
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("naive", "config", naive{canary})
	if !strings.Contains(buf.String(), canary) || !strings.Contains(fmt.Sprintf("%+v", naive{canary}), canary) {
		t.Fatal("control failed: a plain string field did not reach the sinks, so the checks above prove nothing")
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
	if line.Config["m3_admin_token"] != "[redacted]" || line.Config["m3_url"] != goodM3URL {
		t.Errorf("m3_admin_token = %v, m3_url = %v, want the redaction marker and the URL", line.Config["m3_admin_token"], line.Config["m3_url"])
	}

	// The optional module dials say "(unset)" when they are not configured (and never an empty string that could be mistaken for a value that was lost).
	for _, key := range []string{"m1_key_secret", "m1_url", "m2_url", "m2_service_token", "m3_service_token", "jwt_secret", "jwks_url", "jwt_issuer"} {
		if line.Config[key] != "(unset)" {
			t.Errorf("%s = %v, want (unset) for a dial that was not configured", key, line.Config[key])
		}
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
