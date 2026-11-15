package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/probe/probetest"
)

// These tests run the real main(), in a child process, because the exit status and the two output
// streams are what the harness consumes and neither can be observed from inside the process. The test
// binary re-executes itself: TestMain hands control to main() when the marker variable is set.
const runMainMarker = "PROBE_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainMarker) == "1" {
		main() // never returns: main exits with the probe's status
		return
	}
	os.Exit(m.Run())
}

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s", r.code, r.stdout, r.stderr)
}

// runBinary runs main() in a child with exactly the PROBE_* variables in env (any the test process
// itself has are dropped).
func runBinary(t *testing.T, env map[string]string) result {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PROBE_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, runMainMarker+"=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("cannot run the child: %v", err)
	}
	return result{code, out.String(), errb.String()}
}

var lineRE = regexp.MustCompile(`^(CHECK [a-z0-9_.]+ (PASS|FAIL .+)|NOT-APPLICABLE [a-z0-9_.]+ .+|PROBE SUMMARY checks=\d+ pass=\d+ fail=\d+ not_applicable=\d+)$`)

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestBinary_HealthyExitsZeroAndPrintsTheSummaryLast(t *testing.T) {
	st := probetest.Start(t, probetest.Options{})
	r := runBinary(t, st.Env())
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	if got, want := lastLine(r.stdout), "PROBE SUMMARY checks=42 pass=42 fail=0 not_applicable=1"; got != want {
		t.Fatalf("last stdout line = %q, want %q", got, want)
	}
	for _, l := range strings.Split(strings.TrimRight(r.stdout, "\n"), "\n") {
		if !lineRE.MatchString(l) {
			t.Errorf("stdout line %q is not CHECK / NOT-APPLICABLE / PROBE SUMMARY", l)
		}
	}
	if !strings.Contains(r.stderr, "REQUEST GET ") || !strings.Contains(r.stderr, "PROBE NOT CHECKED:") {
		t.Fatalf("stderr lacks the request lines or the not-checked line\n%s", r.stderr)
	}
	// The child really called all four services.
	for _, name := range []string{"platform", "m1", "m2", "m3"} {
		if len(st.Requests(name)) == 0 {
			t.Errorf("the child never called %s", name)
		}
	}
}

func TestBinary_ARedCheckExitsOne(t *testing.T) {
	st := probetest.Start(t, probetest.Options{})

	// Paired with the healthy test above: the same stack, one wrong token, and the status flips.
	env := st.Env()
	env["PROBE_M2_TOKEN"] = "zzzz-sentinel-not-the-token-zzzz"
	r := runBinary(t, env)
	if r.code != 1 {
		t.Fatalf("%s", r)
	}
	if got, want := lastLine(r.stdout), "PROBE SUMMARY checks=42 pass=32 fail=10 not_applicable=1"; got != want {
		t.Fatalf("last stdout line = %q, want %q", got, want)
	}
	if !strings.Contains(r.stdout, "CHECK m2.describe.ok FAIL expected status 200, got 401\n") {
		t.Fatalf("the failing check is not named\n%s", r)
	}
}

func TestBinary_FewerChecksThanTheFloorExitsOne(t *testing.T) {
	st := probetest.Start(t, probetest.Options{})
	env := st.Env()
	env["PROBE_MIN_CHECKS"] = "100"
	r := runBinary(t, env)
	if r.code != 1 || lastLine(r.stdout) != "PROBE SUMMARY checks=42 pass=42 fail=0 not_applicable=1" {
		t.Fatalf("%s", r)
	}
	if !strings.Contains(r.stderr, "fewer than the floor PROBE_MIN_CHECKS=100") {
		t.Fatalf("stderr does not name the floor\n%s", r.stderr)
	}
}

func TestBinary_ConfigurationErrorExitsTwoAndRunsNothing(t *testing.T) {
	st := probetest.Start(t, probetest.Options{})
	env := st.Env()
	delete(env, "PROBE_M3_TOKEN")
	r := runBinary(t, env)
	if r.code != 2 {
		t.Fatalf("%s", r)
	}
	if r.stdout != "" {
		t.Fatalf("a configuration error printed to stdout:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "PROBE_M3_TOKEN is required but not set") {
		t.Fatalf("stderr does not name the variable\n%s", r.stderr)
	}
	for _, name := range []string{"platform", "m1", "m2", "m3"} {
		if n := len(st.Requests(name)); n != 0 {
			t.Errorf("a configuration error still called %s %d times", name, n)
		}
	}

	// An empty environment names every required variable.
	r = runBinary(t, map[string]string{})
	if r.code != 2 || strings.Count(r.stderr, "is required but not set") != 7 {
		t.Fatalf("%s", r)
	}
}

func TestBinary_NeverPrintsASecret(t *testing.T) {
	for name, fault := range map[string]func(map[string]string){
		"a passing run":         func(map[string]string) {},
		"a failing run":         func(e map[string]string) { e["PROBE_M3_TOKEN"] = "zzzz-sentinel-not-the-token-zzzz" },
		"a configuration error": func(e map[string]string) { delete(e, "PROBE_PLATFORM_URL") },
	} {
		t.Run(name, func(t *testing.T) {
			st := probetest.Start(t, probetest.Options{})
			env := st.Env()
			fault(env)
			r := runBinary(t, env)
			for _, s := range []string{probetest.SentinelM1, probetest.SentinelM2, probetest.SentinelM3, "zzzz-sentinel-not-the-token-zzzz"} {
				if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
					t.Fatalf("the output contains the credential %q\n%s", s, r)
				}
			}
			if r.stdout == "" && r.stderr == "" {
				t.Fatal("the child printed nothing at all: the scan above proves nothing")
			}
			// Non-vacuity: the credentials were in the child's environment and, on a run that got that
			// far, on the wire.
			if r.code != 2 {
				if reqs := st.Requests("m2"); len(reqs) < 3 || reqs[2].Auth != "right" {
					t.Fatalf("the child did not send the configured token: %v", reqs)
				}
			}
		})
	}
}
