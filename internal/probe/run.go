package probe

import (
	"context"
	"fmt"
	"io"
)

// Step is one service's checks. The plan is a list of steps so a test can run a smaller one.
type Step struct {
	Name string
	Run  func(ctx context.Context, d Doer, rec *Recorder)
}

// DefaultPlan is the real plan: the platform, module 1, module 2 and module 3, in that order.
func DefaultPlan(cfg Config) []Step {
	return []Step{
		{"platform", func(ctx context.Context, d Doer, rec *Recorder) {
			CheckPlatform(ctx, d, rec, Service{Name: "platform", BaseURL: cfg.PlatformURL})
		}},
		{"m1", func(ctx context.Context, d Doer, rec *Recorder) {
			CheckM1(ctx, d, rec, Service{Name: "m1", BaseURL: cfg.M1URL, Credential: cfg.M1Key})
		}},
		{"m2", func(ctx context.Context, d Doer, rec *Recorder) {
			CheckM2(ctx, d, rec, Service{Name: "m2", BaseURL: cfg.M2URL, Credential: cfg.M2Token})
		}},
		{"m3", func(ctx context.Context, d Doer, rec *Recorder) {
			CheckM3(ctx, d, rec, Service{Name: "m3", BaseURL: cfg.M3URL, Credential: cfg.M3Token})
		}},
	}
}

// Report is the outcome of a run.
type Report struct {
	Results       []Result
	Checks        int // executed: pass + fail; NOT-APPLICABLE lines are not checks
	Pass          int
	Fail          int
	NotApplicable int
	Floor         int // the minimum Checks for a green run
}

// Red reports whether the run is red: a check failed, or fewer than Floor checks ran.
func (r Report) Red() bool { return r.Fail > 0 || r.Checks < r.Floor }

// Summary is the last line the probe prints on stdout.
func (r Report) Summary() string {
	return fmt.Sprintf("PROBE SUMMARY checks=%d pass=%d fail=%d not_applicable=%d", r.Checks, r.Pass, r.Fail, r.NotApplicable)
}

// notChecked is printed on every run (the repository rule: a check that covers part of its domain says
// how much it did not cover).
const notChecked = "PROBE NOT CHECKED: module behaviour beyond health, readiness, describe and credential refusal " +
	"(no verify, send or bulk call is made); TLS; reachability from outside the private network; " +
	"the contents of describe beyond the fields listed in Docs/probe-reference.md"

// Execute runs the plan with cfg through d. Check lines and the summary go to stdout, request lines and
// diagnostics to stderr, each as it happens. The exit status is 0 only when no check failed and at least
// cfg.MinChecks ran (never fewer than one, whatever the config says).
func Execute(ctx context.Context, cfg Config, d Doer, plan []Step, stdout, stderr io.Writer) (int, Report) {
	rec := NewRecorder(stdout, stderr, cfg.secrets()...)
	for _, step := range plan {
		runStep(ctx, d, rec, step)
	}
	checks, pass, fail, na := rec.Counts()
	rep := Report{Results: rec.Results(), Checks: checks, Pass: pass, Fail: fail, NotApplicable: na, Floor: max(cfg.MinChecks, 1)}

	if rep.Fail > 0 {
		_, _ = fmt.Fprintf(stderr, "PROBE RED: %d of %d checks failed\n", rep.Fail, rep.Checks)
	}
	if rep.Checks < rep.Floor {
		_, _ = fmt.Fprintf(stderr, "PROBE RED: only %d checks were executed, fewer than the floor PROBE_MIN_CHECKS=%d (a check that did not run is a failure)\n", rep.Checks, rep.Floor)
	}
	_, _ = fmt.Fprintln(stderr, notChecked)
	_, _ = fmt.Fprintln(stdout, rep.Summary())
	if rep.Red() {
		return 1, rep
	}
	return 0, rep
}

// runStep runs one step. A step that panics did not run its checks, which is a failure, not a crash
// (a Go panic exits 2, and 2 means a configuration error here).
func runStep(ctx context.Context, d Doer, rec *Recorder, s Step) {
	defer func() {
		if r := recover(); r != nil {
			rec.Fail(s.Name+".completed", Positive, fmt.Sprintf("the step panicked (%T) before it finished its checks", r))
		}
	}()
	s.Run(ctx, d, rec)
}

// Main is the whole program: read the environment, run the default plan, return the exit status. 0 is
// green, 1 is red, 2 is a configuration error (printed to stderr, naming each variable, never a value).
func Main(getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := LoadConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "PROBE CONFIG ERROR: %v\n", err)
		return 2
	}
	code, _ := Execute(context.Background(), cfg, NewClient(cfg.Timeout), DefaultPlan(cfg), stdout, stderr)
	return code
}
