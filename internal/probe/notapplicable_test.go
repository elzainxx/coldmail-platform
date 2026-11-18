package probe_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/probe"
)

// Nothing in the default plan is NOT-APPLICABLE any more (module 1 gained a describe), but the mechanism
// stays: a check that cannot apply to a service is recorded, printed, counted apart and never counted as a
// pass. These tests exercise it directly, so a later use does not find it untested. Each has its control.

func TestNotApplicable_RecorderPrintsAndCountsApart(t *testing.T) {
	record := func(withNA bool) (string, probe.Report) {
		var out, errb bytes.Buffer
		rec := probe.NewRecorder(&out, &errb)
		rec.Pass("a.one", probe.Positive)
		if withNA {
			rec.NotApplicable("a.two", "the service has no such route")
		}
		rec.Fail("a.three", probe.Negative, "expected x")
		checks, pass, fail, na := rec.Counts()
		return out.String(), probe.Report{Results: rec.Results(), Checks: checks, Pass: pass, Fail: fail, NotApplicable: na}
	}

	out, rep := record(true)
	if want := "CHECK a.one PASS\nNOT-APPLICABLE a.two the service has no such route\nCHECK a.three FAIL expected x\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if rep.Checks != 2 || rep.Pass != 1 || rep.Fail != 1 || rep.NotApplicable != 1 {
		t.Fatalf("counts = checks %d pass %d fail %d not-applicable %d, want 2/1/1/1: a NOT-APPLICABLE line is not a check", rep.Checks, rep.Pass, rep.Fail, rep.NotApplicable)
	}
	if got := rep.Results[1]; got.ID != "a.two" || got.Status != probe.StatusNotApplicable || got.Reason != "the service has no such route" {
		t.Fatalf("the recorded result = %+v", got)
	}
	if got, want := rep.Summary(), "PROBE SUMMARY checks=2 pass=1 fail=1 not_applicable=1"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	// The control: without the call there is no line and the counter stays at zero, with the same checks.
	out2, rep2 := record(false)
	if strings.Contains(out2, "NOT-APPLICABLE") || rep2.NotApplicable != 0 || rep2.Checks != rep.Checks || rep2.Pass != rep.Pass {
		t.Fatalf("control failed: without NotApplicable the output is %q and the counts %+v", out2, rep2)
	}
}

func TestNotApplicable_IsNotAPassAndDoesNotCountTowardsTheFloor(t *testing.T) {
	cfg, err := probe.LoadConfig(getenvOf(map[string]string{
		"PROBE_PLATFORM_URL": "http://p:1", "PROBE_M1_URL": "http://a:1", "PROBE_M1_KEY": "k1",
		"PROBE_M2_URL": "http://b:1", "PROBE_M2_TOKEN": "t2", "PROBE_M3_URL": "http://c:1", "PROBE_M3_TOKEN": "t3",
		"PROBE_MIN_CHECKS": "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	step := func(name string, record func(rec *probe.Recorder)) []probe.Step {
		return []probe.Step{{Name: name, Run: func(_ context.Context, _ probe.Doer, rec *probe.Recorder) { record(rec) }}}
	}

	// A step that records one pass and one NOT-APPLICABLE is green: one check, one pass, one counted apart.
	o := runPlan(t, cfg, probe.NewClient(cfg.Timeout), step("x", func(rec *probe.Recorder) {
		rec.Pass("x.ok", probe.Positive)
		rec.NotApplicable("x.describe", "no such endpoint")
	}))
	if o.code != 0 || o.summary() != "PROBE SUMMARY checks=1 pass=1 fail=0 not_applicable=1" {
		t.Fatalf("pass + not-applicable was not green with one check\n%s", o)
	}
	for _, l := range o.stdoutLines() {
		if !checkLineRE.MatchString(l) {
			t.Errorf("stdout line %q is not a CHECK / NOT-APPLICABLE / PROBE SUMMARY line", l)
		}
	}
	if !strings.Contains(o.stdout, "NOT-APPLICABLE x.describe no such endpoint\n") {
		t.Fatalf("the NOT-APPLICABLE line is missing\n%s", o)
	}

	// The paired control: the same plan with the pass removed. A NOT-APPLICABLE line alone is zero checks,
	// below the floor of one, so the run is red for that reason and no check failed.
	o = runPlan(t, cfg, probe.NewClient(cfg.Timeout), step("x", func(rec *probe.Recorder) {
		rec.NotApplicable("x.describe", "no such endpoint")
	}))
	if o.code != 1 || o.summary() != "PROBE SUMMARY checks=0 pass=0 fail=0 not_applicable=1" {
		t.Fatalf("a NOT-APPLICABLE line alone was not red\n%s", o)
	}
	if !strings.Contains(o.stderr, "only 0 checks were executed, fewer than the floor PROBE_MIN_CHECKS=1") {
		t.Fatalf("the message does not name the floor\n%s", o.stderr)
	}
}

func TestNotApplicable_IsNotInTheChecksTable(t *testing.T) {
	// The documented table lists the checks that run; a NOT-APPLICABLE line is not one, so the table does
	// not need a row for it and the instrument must not demand one.
	rows := [][]string{{"`a.ok`", "positive", "text"}}
	results := []probe.Result{
		{ID: "a.ok", Kind: probe.Positive, Status: probe.StatusPass},
		{ID: "a.na", Status: probe.StatusNotApplicable, Reason: "no such endpoint"},
	}
	if diffs := diffChecks(rows, results); len(diffs) != 0 {
		t.Fatalf("a NOT-APPLICABLE result was demanded in the table: %v", diffs)
	}
	if doc := goodChecksDoc(results); strings.Contains(doc, "a.na") {
		t.Fatalf("the generated table lists the NOT-APPLICABLE result:\n%s", doc)
	}

	// The control: the same result recorded as a pass is a check, and is reported as undocumented.
	results[1].Status = probe.StatusPass
	diffs := strings.Join(diffChecks(rows, results), "\n")
	if !strings.Contains(diffs, "a.na runs but is not documented") {
		t.Fatalf("control failed: a passing check missing from the table was not reported: %q", diffs)
	}
}
