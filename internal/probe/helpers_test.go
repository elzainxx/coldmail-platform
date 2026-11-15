package probe_test

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/probe"
	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// healthyChecks is the number of checks a healthy run executes: platform 2, module 1 8, module 2 16,
// module 3 16. It is asserted exactly, so a check that quietly disappears turns a test red.
const (
	healthyChecks   = 42
	healthyPositive = 30
	healthyNegative = 12
)

// outcome is everything one run produced.
type outcome struct {
	code           int
	stdout, stderr string
	report         probe.Report
}

func getenvOf(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// run loads the configuration from env, lets tweak change it, and runs the default plan through
// probe.NewClient, exactly as Main does.
func run(t *testing.T, env map[string]string, tweak func(*probe.Config)) outcome {
	t.Helper()
	cfg, err := probe.LoadConfig(getenvOf(env))
	if err != nil {
		t.Fatalf("the test environment is invalid: %v", err)
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return runPlan(t, cfg, probe.NewClient(cfg.Timeout), probe.DefaultPlan(cfg))
}

func runPlan(t *testing.T, cfg probe.Config, d probe.Doer, plan []probe.Step) outcome {
	t.Helper()
	var out, errb bytes.Buffer
	code, rep := probe.Execute(context.Background(), cfg, d, plan, &out, &errb)
	return outcome{code: code, stdout: out.String(), stderr: errb.String(), report: rep}
}

// runMain runs the whole program (probe.Main) on env.
func runMain(env map[string]string) outcome {
	var out, errb bytes.Buffer
	code := probe.Main(getenvOf(env), &out, &errb)
	return outcome{code: code, stdout: out.String(), stderr: errb.String()}
}

func (o outcome) String() string {
	return fmt.Sprintf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s", o.code, o.stdout, o.stderr)
}

func (o outcome) stdoutLines() []string {
	return strings.Split(strings.TrimRight(o.stdout, "\n"), "\n")
}

// checkLine returns the CHECK line for id, or "".
func (o outcome) checkLine(id string) string {
	for _, l := range o.stdoutLines() {
		if strings.HasPrefix(l, "CHECK "+id+" ") {
			return l
		}
	}
	return ""
}

// failedIDs lists the ids of every FAIL line, sorted.
func (o outcome) failedIDs() []string {
	var ids []string
	for _, l := range o.stdoutLines() {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "CHECK" && f[2] == "FAIL" {
			ids = append(ids, f[1])
		}
	}
	sort.Strings(ids)
	return ids
}

func (o outcome) summary() string {
	lines := o.stdoutLines()
	return lines[len(lines)-1]
}

func keysOf(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// wantGreen asserts the exact healthy outcome.
func wantGreen(t *testing.T, o outcome) {
	t.Helper()
	want := fmt.Sprintf("PROBE SUMMARY checks=%d pass=%d fail=0 not_applicable=1", healthyChecks, healthyChecks)
	if o.code != 0 || o.summary() != want || len(o.failedIDs()) != 0 {
		t.Fatalf("expected a green run (%s)\n%s", want, o)
	}
}

// wantRed asserts exit 1, that EXACTLY the named checks failed (so the run is red for the reason under
// test and not for an unrelated one), that each failure line carries its expected text, and that the
// summary is arithmetically right: every check still ran, only some of them failed.
func wantRed(t *testing.T, o outcome, want map[string]string) {
	t.Helper()
	if o.code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", o.code, o)
	}
	if got, w := o.failedIDs(), keysOf(want); strings.Join(got, ",") != strings.Join(w, ",") {
		t.Fatalf("failed checks = %v, want exactly %v\n%s", got, w, o)
	}
	for id, text := range want {
		line := o.checkLine(id)
		if !strings.HasPrefix(line, "CHECK "+id+" FAIL ") || !strings.Contains(line, text) {
			t.Fatalf("line for %s = %q, want a FAIL containing %q\n%s", id, line, text, o)
		}
	}
	wantSummary := fmt.Sprintf("PROBE SUMMARY checks=%d pass=%d fail=%d not_applicable=1", healthyChecks, healthyChecks-len(want), len(want))
	if o.summary() != wantSummary {
		t.Fatalf("summary = %q, want %q\n%s", o.summary(), wantSummary, o)
	}
}

var (
	checkLineRE = regexp.MustCompile(`^(CHECK [a-z0-9_.]+ (PASS|FAIL .+)|NOT-APPLICABLE [a-z0-9_.]+ .+|PROBE SUMMARY checks=\d+ pass=\d+ fail=\d+ not_applicable=\d+)$`)
	requestRE   = regexp.MustCompile(`^REQUEST GET http://127\.0\.0\.1:\d+/\S* auth=(none|configured|wrong)$`)
)

// start runs a stack with the given options and fails the test if the probe cannot be configured.
func start(t *testing.T, o probetest.Options) *probetest.Stack {
	t.Helper()
	return probetest.Start(t, o)
}

func shortTimeout(d time.Duration) func(*probe.Config) {
	return func(c *probe.Config) { c.Timeout = d }
}
