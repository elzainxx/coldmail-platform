package contractcheck

import (
	"strings"
	"sync"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func jobFS() map[string]string { return map[string]string{"job-done.json": jobBody} }

func newRec(record bool) *Recorder {
	r := NewRecorder()
	r.SetRecordMode(record)
	return r
}

func TestAssertLivePassesAndMarksTheContractExercisedWhenTheResponseMatches(t *testing.T) {
	tb := &fakeTB{}
	rec := newRec(false)
	AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 200, []byte(jobBody), rec)
	if tb.failed() {
		t.Fatalf("must pass: %s", tb.allText())
	}
	if ex := rec.Exercised(); len(ex) != 1 || ex[0].Route != "GET /v1/jobs/{id}" || ex[0].Case != "done" {
		t.Fatalf("exercised = %+v", ex)
	}
}

func TestAssertLiveFailsOncePerDifferenceNamingRouteCasePathAndKind(t *testing.T) {
	tb := &fakeTB{}
	live := mutate(t, jobBody, func(m map[string]any) {
		delete(m["counts"].(map[string]any), "done")
		m["added"] = 1
		m["state"] = "paused"
	})
	AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 200, live, newRec(false))
	if len(tb.errs) != 4 {
		t.Fatalf("want one failure per difference (KeyMissing, ShapeViolation, KeyAdded, VocabNew), got %d:\n%s", len(tb.errs), tb.allText())
	}
	text := strings.Join(tb.errs, "\n")
	for _, want := range []string{"GET /v1/jobs/{id}", "case=done", "path=counts.done", "kind=KeyMissing", "kind=ShapeViolation", "path=added", "kind=KeyAdded", "path=state", "kind=VocabNew"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the failures must say %q:\n%s", want, text)
		}
	}
}

func TestAssertLiveMarksTheContractExercisedEvenWhenItFails(t *testing.T) {
	tb := &fakeTB{}
	rec := newRec(false)
	AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 500, []byte(`{}`), rec)
	if !tb.failed() || len(rec.Exercised()) != 1 {
		t.Fatalf("a failing call is still a live call: %v %+v", tb.errs, rec.Exercised())
	}
}

func TestAssertLiveFailsWhenTheFixtureIsMissingAndSaysHowToRecordIt(t *testing.T) {
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(nil), fxDir, 200, []byte(jobBody), newRec(false))
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], EnvRecord+"=1") || !strings.Contains(tb.errs[0], "job-done.json") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestAssertLiveFailsOnAContractThatNamesNoUsableFixtureFile(t *testing.T) {
	for _, name := range []string{"", "../x.json", "a/b.json"} {
		tb := &fakeTB{}
		rc := jobContract()
		rc.Fixture = name
		AssertLive(tb, rc, fixtureFS(jobFS()), fxDir, 200, []byte(jobBody), newRec(false))
		if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "no usable fixture") {
			t.Fatalf("%q: errors = %v", name, tb.errs)
		}
	}
}

func TestAssertLiveWithNoFileSystemIsAFatalMisuse(t *testing.T) {
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), nil, fxDir, 200, []byte(jobBody), newRec(false))
	if len(tb.fatals) != 1 || len(tb.errs) != 0 {
		t.Fatalf("fatals = %v errors = %v", tb.fatals, tb.errs)
	}
}

func TestAssertLiveLogsThePositionsItCouldNotJudgeWithoutFailing(t *testing.T) {
	tb := &fakeTB{}
	live := mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{} })
	AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 200, live, newRec(false))
	if tb.failed() {
		t.Fatalf("an empty page is not a failure: %s", tb.allText())
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), "not compared at items on the live side (empty array)") {
		t.Fatalf("logs = %v", tb.logs)
	}
}

func TestAssertLiveInRecordModeLogsOneLinePerLineOfTheBlockAndDoesNotCompare(t *testing.T) {
	tb := &fakeTB{}
	// A response that differs from any fixture, and there is no fixture at all: in record mode neither matters.
	AssertLive(tb, jobContract(), fixtureFS(nil), fxDir, 200, []byte(liveJob), newRec(true))
	if tb.failed() {
		t.Fatalf("record mode must not fail on differences: %s", tb.allText())
	}
	if len(tb.logs) < 5 || tb.logs[0] != beginJob || tb.logs[len(tb.logs)-1] != endMarker {
		t.Fatalf("the log must hold the block line by line, got %d lines:\n%s", len(tb.logs), strings.Join(tb.logs, "\n"))
	}
	// What was logged parses back to the normalised body of the response.
	recs, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	want, _ := Normalise([]byte(liveJob))
	if err != nil || len(recs) != 1 || string(recs[0].Body) != string(want) {
		t.Fatalf("round trip failed: %v %v", recs, err)
	}
}

func TestAssertLiveInRecordModeFailsOnlyWhenTheRecordIsRefused(t *testing.T) {
	tok := token(t, 24)
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(nil), fxDir, 200, []byte(`{"note":"Bearer `+tok+`"}`), newRec(true))
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "cannot be recorded") || strings.Contains(tb.allText(), tok) {
		t.Fatalf("errors = %v", tb.errs)
	}
	if len(tb.logs) != 0 {
		t.Fatalf("a refused record logs nothing: %v", tb.logs)
	}
}

func TestTheEnvironmentVariableSwitchesTheRecorderToRecordMode(t *testing.T) {
	t.Setenv(EnvRecord, "1")
	if !NewRecorder().Recording() {
		t.Fatal("CONTRACT_RECORD=1 must switch record mode on")
	}
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(nil), fxDir, 200, []byte(liveJob), nil) // no Recorder: the environment decides
	if tb.failed() || len(tb.logs) == 0 || tb.logs[0] != beginJob {
		t.Fatalf("with no Recorder the environment must still select record mode: %s", tb.allText())
	}
}

func TestWithoutTheEnvironmentVariableTheRecorderCompares(t *testing.T) {
	t.Setenv(EnvRecord, "")
	if NewRecorder().Recording() {
		t.Fatal("record mode must be off by default")
	}
	t.Setenv(EnvRecord, "0")
	if NewRecorder().Recording() {
		t.Fatal("only 1 switches record mode on")
	}
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(nil), fxDir, 200, []byte(jobBody), nil)
	if len(tb.errs) != 1 {
		t.Fatalf("compare mode with no fixture must fail: %v", tb.errs)
	}
}

func TestFinishFailsOnAContractNoCallExercisedAndOnACallNoContractDeclares(t *testing.T) {
	rc2 := modclient.RouteContract{Module: modclient.M2, Route: "GET /healthz", Fixture: "health.json", Status: 200, Shape: modclient.Object(modclient.Req("status", modclient.Str))}
	contracts := []modclient.RouteContract{jobContract(), rc2}
	rec := newRec(false)
	rec.Observe(Exercised{modclient.M2, "GET /v1/jobs/{id}", "done"})
	rec.Observe(Exercised{modclient.M3, "GET /v1/unlisted", ""})
	tb := &fakeTB{}
	rec.Finish(tb, contracts)
	text := strings.Join(tb.errs, "\n")
	if len(tb.errs) != 2 || !strings.Contains(text, "m2 GET /healthz was never exercised") || !strings.Contains(text, "m3 GET /v1/unlisted has no contract") {
		t.Fatalf("errors = %v", tb.errs)
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), BlindSpots) {
		t.Fatalf("the coverage must be printed, with what the gate cannot see: %v", tb.logs)
	}
}

func TestFinishIsGreenWhenEveryContractWasExercisedAndPrintsTheCoverage(t *testing.T) {
	rec := newRec(false)
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(jobFS()), fxDir, 200, []byte(jobBody), rec)
	rec.Finish(tb, []modclient.RouteContract{jobContract()})
	if tb.failed() {
		t.Fatalf("must be green: %s", tb.allText())
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), "1 of 1 contracts compared with a live response") {
		t.Fatalf("logs = %v", tb.logs)
	}
}

func TestFinishWithNoContractsFails(t *testing.T) {
	tb := &fakeTB{}
	newRec(false).Finish(tb, nil)
	if len(tb.errs) == 0 || !strings.Contains(tb.errs[0], "found 0 contracts") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	rec := newRec(false)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec.Observe(Exercised{modclient.M1, "GET /r", string(rune('a' + (i+j)%5))})
				_ = rec.Exercised()
				_ = rec.Recording()
			}
		}(i)
	}
	wg.Wait()
	if n := len(rec.Exercised()); n != 5 {
		t.Fatalf("distinct calls = %d, want 5", n)
	}
}
