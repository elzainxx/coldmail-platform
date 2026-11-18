package contractcheck

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func TestSessionPrintsTheNearestMissOfAnUnexercisedContractAndCountsItInTheLog(t *testing.T) {
	s := newSession(t, nil)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, pageWithExtraKey(t)))
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	errs := strings.Join(tb.errs, "\n")
	// The populated page is judged against the populated contract: its real difference fails the test with the exact kind and path ...
	for _, want := range []string{"case=done", "path=items[].extra", "kind=" + string(KeyAdded)} {
		if !strings.Contains(errs, want) {
			t.Fatalf("the failure must say %q:\n%s", want, errs)
		}
	}
	// ... the empty-page contract is the one left unexercised ...
	if !strings.Contains(errs, "case=empty") || !strings.Contains(errs, "never exercised") {
		t.Fatalf("the empty-page contract must be reported unexercised:\n%s", errs)
	}
	// ... and the log says how close the closest response came, and counts the nearest misses.
	logs := strings.Join(tb.logs, "\n")
	wantLine := "nearest miss: m2 GET /v1/jobs/{id} case=empty: 1 differences against the closest of 1 observed responses: items GapFixture"
	if !strings.Contains(logs, wantLine) || !strings.Contains(logs, "1 of 1 unexercised contracts have a nearest miss") {
		t.Fatalf("the log must hold the nearest miss and its count:\n%s", logs)
	}
	// The paired control: a run in which every contract was exercised prints neither.
	c := newSession(t, nil)
	observeAll(t, c)
	ctb := &fakeTB{}
	c.Finish(ctb)
	if strings.Contains(ctb.allText(), "nearest miss") {
		t.Fatalf("nothing is unexercised, so there is no nearest miss to print:\n%s", ctb.allText())
	}
}

func TestSessionCountsHowManyUnexercisedContractsHaveANearestMissIncludingNone(t *testing.T) {
	s := newSession(t, nil)
	s.Observer()(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	logs := strings.Join(tb.logs, "\n")
	if !strings.Contains(logs, "0 of 3 unexercised contracts have a nearest miss") || strings.Contains(logs, "nearest miss: ") {
		t.Fatalf("contracts that were never called have no nearest miss, and the count says so:\n%s", logs)
	}
}

func TestSessionAnAllowedUnexercisedContractStillGetsItsNearestMissAndDoesNotFail(t *testing.T) {
	s := newSession(t, map[string]string{"m2|GET /v1/jobs/{id}|empty": "the harness never gets an empty page from module 2"})
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, jobBody))
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("an allowed unexercised contract must not fail: %s", tb.allText())
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), "nearest miss: m2 GET /v1/jobs/{id} case=empty: 1 differences against the closest of 1 observed responses: items GapFixture") {
		t.Fatalf("the allowed contract must still show how it missed:\n%s", strings.Join(tb.logs, "\n"))
	}
}

func TestSessionRecordModeRecordsThePopulatedPageContractFromAPopulatedPageAndPrintsTheNearestMiss(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, pageWithExtraKey(t)))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("record mode must not fail: %s", tb.allText())
	}
	recs, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	if err != nil || len(recs) != 1 || recs[0].File != "job-done.json" || !strings.Contains(string(recs[0].Body), `"extra"`) {
		t.Fatalf("the populated page must be recorded as the populated contract (it used to be handed to the empty-page contract and lost): %v %v", recs, err)
	}
	logs := strings.Join(tb.logs, "\n")
	if !strings.Contains(logs, "nearest miss: m2 GET /v1/jobs/{id} case=empty:") || !strings.Contains(logs, "of 3 unexercised contracts have a nearest miss") {
		t.Fatalf("record mode must show the nearest misses too:\n%s", logs)
	}
	// The paired control: with the empty page observed as well, the empty-page contract is recorded and has no nearest miss.
	c := newSession(t, nil)
	c.SetRecordMode(true)
	c.Observer()(jobObs("/v1/jobs/a", 200, pageWithExtraKey(t)))
	c.Observer()(jobObs("/v1/jobs/b", 200, emptyJobBody(t)))
	ctb := &fakeTB{}
	c.Finish(ctb)
	crecs, cerr := ParseRecorded([]byte(strings.Join(ctb.logs, "\n")))
	if cerr != nil || len(crecs) != 2 || strings.Contains(strings.Join(ctb.logs, "\n"), "nearest miss: m2 GET /v1/jobs/{id} case=empty") {
		t.Fatalf("both page contracts must be recorded: %v %v", crecs, cerr)
	}
}
