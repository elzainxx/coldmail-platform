package probe_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// "Config, not code: every dial is seeded with a consumer." Each variable is changed on its own, and the
// run must change in the way only that variable can cause. The unchanged run is green first, on the same
// stack, so the change is what turns it red. A variable added to Vars() without an entry here fails the
// last assertion; an entry whose variable is read by nothing fails its own case.
func TestVars_EveryVariableHasAConsumer(t *testing.T) {
	type effect struct {
		opts       probetest.Options
		perturb    func(t *testing.T, env map[string]string)
		wantPrefix []string // every failing check id starts with one of these (none when no check must fail)
		wantFailed int
		wantStderr string // on stderr when the run is red
	}
	const wrong = "zzzz-sentinel-not-it-zzzz"
	closed := func(variable string) func(*testing.T, map[string]string) {
		return func(t *testing.T, env map[string]string) { env[variable] = probetest.ClosedURL(t) }
	}
	set := func(variable, value string) func(*testing.T, map[string]string) {
		return func(_ *testing.T, env map[string]string) { env[variable] = value }
	}
	cases := map[string]effect{
		probe.VarPlatformURL: {perturb: closed(probe.VarPlatformURL), wantPrefix: []string{"platform."}, wantFailed: 2},
		probe.VarM1URL:       {perturb: closed(probe.VarM1URL), wantPrefix: []string{"m1."}, wantFailed: healthyM1Checks},
		// The key is sent on the job lookup and on describe: the lookup's accepted check fails, and the
		// describe call (401 instead of 200) fails with its five field checks. The refusals still pass.
		probe.VarM1Key:   {perturb: set(probe.VarM1Key, wrong), wantPrefix: []string{"m1.jobs.right_key.", "m1.describe."}, wantFailed: 7},
		probe.VarM2URL:   {perturb: closed(probe.VarM2URL), wantPrefix: []string{"m2."}, wantFailed: 16},
		probe.VarM2Token: {perturb: set(probe.VarM2Token, wrong), wantPrefix: []string{"m2.describe."}, wantFailed: 10},
		probe.VarM3URL:   {perturb: closed(probe.VarM3URL), wantPrefix: []string{"m3."}, wantFailed: 16},
		probe.VarM3Token: {perturb: set(probe.VarM3Token, wrong), wantPrefix: []string{"m3.describe."}, wantFailed: 10},
		probe.VarTimeoutSec: {
			// Module 2 answers /healthz after 1.5 s: inside the default 10 s, past a 1 s timeout.
			opts:       probetest.Options{M2: probetest.Slow("/healthz", 1500*time.Millisecond)},
			perturb:    set(probe.VarTimeoutSec, "1"),
			wantPrefix: []string{"m2.healthz."}, wantFailed: 1,
		},
		probe.VarMinChecks: {
			perturb:    set(probe.VarMinChecks, strconv.Itoa(healthyChecks+1)),
			wantFailed: 0, wantStderr: "PROBE_MIN_CHECKS=" + strconv.Itoa(healthyChecks+1),
		},
	}

	// Every declared variable has a case and every case is a declared variable.
	declared := map[string]bool{}
	for _, v := range probe.Vars() {
		declared[v.Name] = true
		if _, ok := cases[v.Name]; !ok {
			t.Errorf("%s has no consumer case: nothing proves the code reads it", v.Name)
		}
	}
	for name := range cases {
		if !declared[name] {
			t.Errorf("a consumer case exists for %s, which is not in Vars()", name)
		}
	}
	if len(cases) == 0 || len(declared) == 0 {
		t.Fatal("found no variables or no cases: the instrument matched nothing")
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := start(t, c.opts)

			// The same stack, unchanged: green.
			wantGreen(t, runMain(st.Env()))

			env := st.Env()
			c.perturb(t, env)
			o := runMain(env)
			if o.code != 1 {
				t.Fatalf("changing %s did not turn the run red (exit %d)\n%s", name, o.code, o)
			}
			failed := o.failedIDs()
			if len(failed) != c.wantFailed {
				t.Fatalf("changing %s failed %d checks %v, want %d", name, len(failed), failed, c.wantFailed)
			}
			for _, id := range failed {
				inside := false
				for _, prefix := range c.wantPrefix {
					inside = inside || strings.HasPrefix(id, prefix)
				}
				if !inside {
					t.Errorf("changing %s failed %s, which is outside %q", name, id, c.wantPrefix)
				}
			}
			if !strings.Contains(o.stderr, c.wantStderr) {
				t.Fatalf("stderr does not contain %q\n%s", c.wantStderr, o)
			}
		})
	}
}

// Every Vars() entry is read by LoadConfig: a required variable is demanded, and a default is the value
// the code takes when the variable is absent. (The behavioural side of each variable is the test above.)
func TestVars_DeclaredFlagsMatchTheLoader(t *testing.T) {
	full := map[string]string{
		"PROBE_PLATFORM_URL": "http://p:1", "PROBE_M1_URL": "http://a:1", "PROBE_M1_KEY": "k1",
		"PROBE_M2_URL": "http://b:1", "PROBE_M2_TOKEN": "t2", "PROBE_M3_URL": "http://c:1", "PROBE_M3_TOKEN": "t3",
	}
	cfg, err := probe.LoadConfig(getenvOf(full))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range probe.Vars() {
		n++
		env := cloneEnv(full)
		delete(env, v.Name)
		_, err := probe.LoadConfig(getenvOf(env))
		switch {
		case v.Required && err == nil:
			t.Errorf("%s is declared required but LoadConfig accepts its absence", v.Name)
		case !v.Required && err != nil:
			t.Errorf("%s is declared optional but LoadConfig fails without it: %v", v.Name, err)
		}
	}
	if n != 9 {
		t.Fatalf("checked %d variables, want 9", n)
	}
	for _, v := range probe.Vars() {
		if v.Name == probe.VarTimeoutSec && v.Default != "10" || v.Name == probe.VarMinChecks && v.Default != "25" {
			t.Errorf("%s declares default %q", v.Name, v.Default)
		}
	}
	if cfg.Timeout != 10*time.Second || cfg.MinChecks != 25 {
		t.Fatalf("the defaults the code takes are %v and %d", cfg.Timeout, cfg.MinChecks)
	}
}
