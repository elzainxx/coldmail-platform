package probe_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

func TestConfig_MissingRequiredVariableExitsTwoAndNamesIt(t *testing.T) {
	st := start(t, probetest.Options{})

	// The control first: with every variable set, the same program is green (exit 0), so exit 2 below
	// is caused by the one variable removed.
	wantGreen(t, runMain(st.Env()))

	required := 0
	for _, v := range probe.Vars() {
		if !v.Required {
			continue
		}
		required++
		for how, set := range map[string]func(map[string]string){
			"unset": func(e map[string]string) { delete(e, v.Name) },
			"blank": func(e map[string]string) { e[v.Name] = "  \t " },
		} {
			t.Run(v.Name+" "+how, func(t *testing.T) {
				env := st.Env()
				set(env)
				o := runMain(env)
				if o.code != 2 {
					t.Fatalf("exit %d, want 2\n%s", o.code, o)
				}
				if !strings.Contains(o.stderr, "PROBE CONFIG ERROR: ") || !strings.Contains(o.stderr, v.Name+" is required but not set") {
					t.Fatalf("stderr does not name %s as required\n%s", v.Name, o)
				}
				if o.stdout != "" {
					t.Fatalf("a configuration error printed to stdout:\n%s", o.stdout)
				}
				// Only the missing variable is named; the others were fine.
				for _, other := range probe.Vars() {
					if other.Name != v.Name && strings.Contains(o.stderr, other.Name) {
						t.Fatalf("stderr names %s, which was set\n%s", other.Name, o)
					}
				}
				for _, s := range []string{probetest.SentinelM1, probetest.SentinelM2, probetest.SentinelM3} {
					if strings.Contains(o.stdout+o.stderr, s) {
						t.Fatalf("a configuration error printed a secret\n%s", o)
					}
				}
			})
		}
	}
	if required != 7 {
		t.Fatalf("found %d required variables, want 7: the instrument matched the wrong set", required)
	}
}

func TestConfig_AnEmptyEnvironmentNamesEveryRequiredVariable(t *testing.T) {
	o := runMain(map[string]string{})
	if o.code != 2 || o.stdout != "" {
		t.Fatalf("exit %d, stdout %q\n%s", o.code, o.stdout, o)
	}
	for _, name := range []string{
		"PROBE_PLATFORM_URL", "PROBE_M1_URL", "PROBE_M1_KEY", "PROBE_M2_URL", "PROBE_M2_TOKEN", "PROBE_M3_URL", "PROBE_M3_TOKEN",
	} {
		if !strings.Contains(o.stderr, name+" is required but not set") {
			t.Errorf("stderr does not name %s\n%s", name, o.stderr)
		}
	}
	for _, name := range []string{"PROBE_TIMEOUT_SEC", "PROBE_MIN_CHECKS"} {
		if strings.Contains(o.stderr, name) {
			t.Errorf("stderr names %s, which has a default\n%s", name, o.stderr)
		}
	}
}

func TestConfig_InvalidValuesExitTwoWithoutEchoingThem(t *testing.T) {
	st := start(t, probetest.Options{})
	for name, c := range map[string]struct {
		variable, value, reason string
		mustNotEcho             string // a distinctive part of the value that must not appear in the output
	}{
		"timeout not a number":    {"PROBE_TIMEOUT_SEC", "abc", "must be an integer from 1 to 600", "abc"},
		"timeout zero":            {"PROBE_TIMEOUT_SEC", "0", "must be an integer from 1 to 600", ""},
		"timeout too large":       {"PROBE_TIMEOUT_SEC", "601", "must be an integer from 1 to 600", "601"},
		"timeout fractional":      {"PROBE_TIMEOUT_SEC", "1.5", "must be an integer from 1 to 600", "1.5"},
		"floor zero":              {"PROBE_MIN_CHECKS", "0", "must be an integer from 1 to 1000", ""},
		"floor too large":         {"PROBE_MIN_CHECKS", "1001", "must be an integer from 1 to 1000", "1001"},
		"floor not a number":      {"PROBE_MIN_CHECKS", "many", "must be an integer from 1 to 1000", "many"},
		"url with another scheme": {"PROBE_M1_URL", "ftp://zzzz-host-zzzz:21", "must be an http or https URL", "zzzz-host-zzzz"},
		"url without a scheme":    {"PROBE_M2_URL", "zzzz-host-zzzz:8080", "must be an http or https URL", "zzzz-host-zzzz"},
		"url without a host":      {"PROBE_M3_URL", "http://", "has no host", ""},
		"url that does not parse": {"PROBE_PLATFORM_URL", "http://zzzz host zzzz/", "is not a valid URL", "zzzz host zzzz"},
		"url with credentials":    {"PROBE_M2_URL", "http://someone:zzzz-urlpass-zzzz@127.0.0.1:1", "must not contain credentials", "zzzz-urlpass-zzzz"},
		"url with a query":        {"PROBE_M3_URL", "http://127.0.0.1:1/?zzzz-query-zzzz", "must not contain a query or fragment", "zzzz-query-zzzz"},
		"url with a fragment":     {"PROBE_M3_URL", "http://127.0.0.1:1/#zzzz-fragment-zzzz", "must not contain a query or fragment", "zzzz-fragment-zzzz"},
		"token with a space":      {"PROBE_M2_TOKEN", "zzzz-part-one zzzz-part-two", "contains whitespace or a control character", "zzzz-part"},
		"key with a control byte": {"PROBE_M1_KEY", "zzzz-before\x01zzzz-after", "contains whitespace or a control character", "zzzz-before"},
		"token with an inner tab": {"PROBE_M3_TOKEN", "zzzz-a\tzzzz-b", "contains whitespace or a control character", "zzzz-a"},
	} {
		t.Run(name, func(t *testing.T) {
			env := st.Env()
			env[c.variable] = c.value
			o := runMain(env)
			if o.code != 2 {
				t.Fatalf("exit %d, want 2\n%s", o.code, o)
			}
			if want := c.variable + " " + c.reason; !strings.Contains(o.stderr, want) {
				t.Fatalf("stderr does not say %q\n%s", want, o)
			}
			if o.stdout != "" {
				t.Fatalf("a configuration error printed to stdout:\n%s", o.stdout)
			}
			if c.mustNotEcho != "" && strings.Contains(o.stdout+o.stderr, c.mustNotEcho) {
				t.Fatalf("the output echoes part of the bad value %q\n%s", c.mustNotEcho, o)
			}
		})
	}
}

func TestConfig_DefaultsAndNormalisation(t *testing.T) {
	base := map[string]string{
		"PROBE_PLATFORM_URL": "http://platform:8080/",
		"PROBE_M1_URL":       "https://m1.internal:8443/api/",
		"PROBE_M1_KEY":       "k1",
		"PROBE_M2_URL":       "  http://m2:8080  ",
		"PROBE_M2_TOKEN":     "t2",
		"PROBE_M3_URL":       "http://m3",
		"PROBE_M3_TOKEN":     "t3",
	}
	cfg, err := probe.LoadConfig(getenvOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 10*time.Second || cfg.MinChecks != 25 {
		t.Fatalf("defaults: timeout %v, floor %d; want 10s and 25", cfg.Timeout, cfg.MinChecks)
	}
	if cfg.PlatformURL != "http://platform:8080" || cfg.M1URL != "https://m1.internal:8443/api" || cfg.M2URL != "http://m2:8080" || cfg.M3URL != "http://m3" {
		t.Fatalf("URLs not normalised: %q %q %q %q", cfg.PlatformURL, cfg.M1URL, cfg.M2URL, cfg.M3URL)
	}

	// Explicit values are read (the paired control of the defaults above).
	base["PROBE_TIMEOUT_SEC"], base["PROBE_MIN_CHECKS"] = "3", "7"
	cfg, err = probe.LoadConfig(getenvOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 3*time.Second || cfg.MinChecks != 7 {
		t.Fatalf("explicit values: timeout %v, floor %d; want 3s and 7", cfg.Timeout, cfg.MinChecks)
	}
}
