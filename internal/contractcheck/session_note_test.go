package contractcheck

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func recordSession(t *testing.T, contracts []modclient.RouteContract, files map[string]string) *Session {
	t.Helper()
	s := NewSession(contracts, fixtureFS(files), fxDir, nil)
	s.SetRecordMode(true)
	return s
}

func TestSessionRecordModePrintsAFitNoteRightBeforeEachBlockAndTheBlocksStillParse(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, pageWithExtraKey(t))) // one key more than the fixture
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))        // exactly the fixture
	obs(ob(modclient.M2, "m2.new", "POST", "/v1/newroute", 202, `{"accepted":true}`))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("a poor fit is shown, not refused: %s", tb.allText())
	}
	notes := map[string]string{} // file -> the note that stands right before its block
	for i, l := range tb.logs {
		if !strings.HasPrefix(l, "==== CONTRACT-FIXTURE BEGIN ") {
			continue
		}
		file := l[strings.Index(l, " file=")+len(" file="):]
		file = file[:strings.IndexByte(file, ' ')]
		if i == 0 || !strings.HasPrefix(tb.logs[i-1], "NOTE fixture fit: ") || !strings.Contains(tb.logs[i-1], "file="+file) {
			t.Fatalf("the block for %s has no fit note right before it:\n%s", file, strings.Join(tb.logs, "\n"))
		}
		notes[file] = tb.logs[i-1]
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %v", notes)
	}
	if !strings.Contains(notes["job-done.json"], ": 1 differences, 0 gaps against the current fixture") {
		t.Fatalf("a body with one key more must say one difference: %s", notes["job-done.json"])
	}
	if !strings.Contains(notes["job-not-found.json"], ": 0 differences, 0 gaps against the current fixture") {
		t.Fatalf("an exact fit must say 0 and 0: %s", notes["job-not-found.json"])
	}
	if !strings.Contains(notes["UNDECLARED-m2-1.json"], "no contract declares this call") {
		t.Fatalf("an undeclared call has no fixture to compare with: %s", notes["UNDECLARED-m2-1.json"])
	}
	// The notes sit outside the blocks: what ParseRecorded reads is the same with and without them.
	with, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	if err != nil || len(with) != 3 {
		t.Fatalf("blocks with notes: %v %v", with, err)
	}
	var bare []string
	for _, l := range tb.logs {
		if !strings.HasPrefix(l, "NOTE fixture fit: ") {
			bare = append(bare, l)
		}
	}
	without, err := ParseRecorded([]byte(strings.Join(bare, "\n")))
	if err != nil || len(without) != len(with) {
		t.Fatalf("blocks without notes: %v %v", without, err)
	}
	for i := range with {
		if with[i].File != without[i].File || string(with[i].Body) != string(without[i].Body) {
			t.Fatalf("a note changed what is parsed for %s", with[i].File)
		}
	}
}

func TestSessionRecordModeFitNoteCountsGapsAndSaysWhenThereIsNoFixture(t *testing.T) {
	// A populated page recorded against a lone empty-page contract: no differences, one gap on the fixture side.
	s := recordSession(t, []modclient.RouteContract{jobEmptyContract()}, map[string]string{"job-empty.json": emptyJobBody(t)})
	s.Observer()(jobObs("/v1/jobs/a", 200, jobBody))
	tb := &fakeTB{}
	s.Finish(tb)
	if !strings.Contains(strings.Join(tb.logs, "\n"), "NOTE fixture fit: m2 GET /v1/jobs/{id} case=empty file=job-empty.json: 0 differences, 1 gaps against the current fixture") {
		t.Fatalf("logs:\n%s", strings.Join(tb.logs, "\n"))
	}
	// A contract whose fixture does not exist yet is a new recording, not a number of differences.
	n := recordSession(t, []modclient.RouteContract{jobContract()}, nil)
	n.Observer()(jobObs("/v1/jobs/a", 200, jobBody))
	ntb := &fakeTB{}
	n.Finish(ntb)
	if text := strings.Join(ntb.logs, "\n"); !strings.Contains(text, "there is no readable current fixture, so this is a new recording") || strings.Contains(text, "differences, ") {
		t.Fatalf("logs:\n%s", text)
	}
}

func TestSessionRecordModeFitNoteIsNotPrintedInCompareMode(t *testing.T) {
	s := newSession(t, nil)
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	if strings.Contains(tb.allText(), "NOTE fixture fit") {
		t.Fatalf("the fit note belongs to record mode:\n%s", tb.allText())
	}
}

func TestParseRecordedIgnoresAFitNoteBetweenAndAroundBlocks(t *testing.T) {
	block := recordBlock(t, jobContract(), 200, `{"a":1}`)
	other := modclient.RouteContract{Module: modclient.M3, Route: "GET /healthz", Fixture: "health.json", Status: 200}
	log := "NOTE fixture fit: m2 GET /v1/jobs/{id} case=done file=job-done.json: 3 differences, 2 gaps against the current fixture\n" + block +
		"NOTE fixture fit: m3 GET /healthz file=health.json: 0 differences, 0 gaps\n" + recordBlock(t, other, 200, `{"status":"ok"}`)
	recs, err := ParseRecorded([]byte(log))
	if err != nil || len(recs) != 2 || recs[0].File != "job-done.json" || recs[1].File != "health.json" {
		t.Fatalf("blocks = %+v %v", recs, err)
	}
}
