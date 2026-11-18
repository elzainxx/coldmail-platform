package contractcheck

import (
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The adapters name their vocabularies ("test_verification_state") and say where each lives in RouteContract.VocabAt; a name with no VocabAt entry is read as a path itself, and a
// name that is no path of the body constrains nothing.

const (
	vocabName = "test_verification_state"
	vocabPos  = "verification.state"
)

func testContractsAt(withAt bool) []modclient.RouteContract {
	cs := testContracts(false)
	for i := range cs {
		cs[i].Vocab = map[string][]string{vocabName: {"ok", "failed", "unavailable", "pending"}}
		if withAt {
			cs[i].VocabAt = map[string]string{vocabName: vocabPos}
		}
	}
	return cs
}

func problemKinds(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p.Kind))
	}
	return out
}

func TestCompareResolvesAVocabularyNameThroughVocabAtAndReportsTheNewValueAtThePath(t *testing.T) {
	rc := testContractsAt(true)[0]
	fx := testBody("a", "ok", "x")
	rep := Compare(rc, []byte(fx), []byte(testBody("b", "weird", "y")), 200)
	wantDiffs(t, rep, string(VocabNew)+":"+vocabPos)
	if !strings.Contains(rep.Diffs[0].Detail, `"weird"`) || !strings.Contains(rep.Diffs[0].Detail, vocabName) {
		t.Fatalf("the detail must name the value and the vocabulary: %s", rep.Diffs[0].Detail)
	}
	wantDiffs(t, Compare(rc, []byte(fx), []byte(testBody("b", "failed", "y")), 200)) // a word of the set: no difference
	// The paired control: the same vocabulary with no path is inert, and says nothing about a value that is not in the set (the hole this closes).
	inert := testContractsAt(false)[0]
	wantDiffs(t, Compare(inert, []byte(fx), []byte(testBody("b", "weird", "y")), 200))
	if ps := CheckFixtures([]modclient.RouteContract{inert}, fixtureFS(map[string]string{"test.ok.json": fx}), fxDir); !reflect.DeepEqual(problemKinds(ps), []string{string(VocabInert)}) {
		t.Fatalf("the inert vocabulary must be a problem, got %v", ps)
	}
}

func TestMatchTellsTheTestButtonContractsApartThroughVocabularyNamesAndVocabAt(t *testing.T) {
	res := Match(testContractsAt(true), fixtureFS(testFiles()), fxDir, []Observation{testObs("ok", 11), testObs("failed", 12)})
	if len(res.Matched) != 2 || len(res.Unexercised) != 1 || res.Unexercised[0].Case != "unavailable" {
		t.Fatalf("matched %d unexercised %+v", len(res.Matched), res.Unexercised)
	}
	for _, m := range res.Matched {
		want := "ok"
		if strings.Contains(string(m.Observation.Body), `"state":"failed"`) {
			want = "failed"
		}
		if m.Contract.Case != want || len(m.Tied) != 0 {
			t.Fatalf("a %q response went to %q (tied %v)", want, m.Contract.Case, m.Tied)
		}
	}
	// The nearest miss names the resolved path, not the name the adapter chose.
	if len(res.NearestMisses) != 1 || !reflect.DeepEqual(res.NearestMisses[0].Misses, []Miss{{Path: vocabPos, Kind: KindVocabValue}}) {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
	// The paired control: the same names with no VocabAt tie (the vocabulary was inert), so the first response is filed under whichever contract comes first.
	bare := Match(testContractsAt(false), fixtureFS(testFiles()), fxDir, []Observation{testObs("ok", 11)})
	if len(bare.Matched) != 1 || len(bare.Matched[0].Tied) != 2 {
		t.Fatalf("without a path the three contracts tie: %+v", bare.Matched)
	}
}

func TestCheckFixturesAVocabularyWithNoPathIsInertAndWithAVocabAtPathItConstrains(t *testing.T) {
	one := func(vocab map[string][]string, at map[string]string) []Problem {
		rc := testContracts(false)[0]
		rc.Vocab, rc.VocabAt = vocab, at
		return CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"test.ok.json": testFiles()["test.ok.json"]}), fxDir)
	}
	words := map[string][]string{"job_status": {"ok", "failed"}}
	ps := one(words, nil)
	if len(ps) != 1 || ps[0].Kind != VocabInert {
		t.Fatalf("problems = %v", ps)
	}
	if want := "vocabulary job_status at job_status constrains nothing in test.ok.json: the path is wrong or the fixture has no value there"; ps[0].Detail != want {
		t.Fatalf("detail:\n%s\nwant:\n%s", ps[0].Detail, want)
	}
	if ps := one(words, map[string]string{"job_status": vocabPos}); len(ps) != 0 {
		t.Fatalf("with its path the vocabulary constrains something: %v", ps)
	}
	// A wrong path is inert, with the path in the message.
	ps = one(words, map[string]string{"job_status": "verification.statte"})
	if len(ps) != 1 || ps[0].Kind != VocabInert || !strings.Contains(ps[0].Detail, "at verification.statte constrains nothing") {
		t.Fatalf("problems = %v", ps)
	}
	// A path of the right place that holds no string (an object, a null, a number) constrains nothing either.
	for _, at := range []string{"verification", "account_id.x"} {
		if ps := one(words, map[string]string{"job_status": at}); len(ps) != 1 || ps[0].Kind != VocabInert {
			t.Fatalf("path %q: problems = %v", at, ps)
		}
	}
	// A path that is not of the grammar is a different problem, and a VocabAt entry for a vocabulary that does not exist is one too.
	if ps := one(words, map[string]string{"job_status": "verification[.state"}); len(ps) != 1 || ps[0].Kind != VocabKey {
		t.Fatalf("problems = %v", ps)
	}
	if ps := one(words, map[string]string{"job_status": vocabPos, "job_statuss": vocabPos}); len(ps) != 1 || ps[0].Kind != VocabKey || !strings.Contains(ps[0].Detail, `"job_statuss"`) {
		t.Fatalf("problems = %v", ps)
	}
}

func TestCheckFixturesAFixtureWordOutsideTheSetIsFoundThroughVocabAt(t *testing.T) {
	rc := testContracts(false)[0]
	rc.Vocab = map[string][]string{vocabName: {"failed", "unavailable"}} // ok is the fixture's word and is not in the set
	rc.VocabAt = map[string]string{vocabName: vocabPos}
	ps := CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"test.ok.json": testFiles()["test.ok.json"]}), fxDir)
	if len(ps) != 1 || ps[0].Kind != VocabOutside || !strings.Contains(ps[0].Detail, `"ok" at "`+vocabPos+`"`) {
		t.Fatalf("problems = %v", ps)
	}
	// The paired control: with the word in the set there is nothing.
	rc.Vocab = map[string][]string{vocabName: {"ok", "failed"}}
	if ps := CheckFixtures([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"test.ok.json": testFiles()["test.ok.json"]}), fxDir); len(ps) != 0 {
		t.Fatalf("problems = %v", ps)
	}
}

func TestInspectFixturesAnEmptyArrayOnTheWayToAVocabularyIsANoteAndNotAProblem(t *testing.T) {
	rc := jobEmptyContract()
	rc.Vocab = map[string][]string{"item_state": {"ok", "failed"}}
	rc.VocabAt = map[string]string{"item_state": "items[].state"}
	files := fixtureFS(map[string]string{"job-empty.json": emptyJobBody(t)})
	rep := InspectFixtures([]modclient.RouteContract{rc}, files, fxDir)
	if len(rep.Problems) != 0 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "vocabulary item_state at items[].state is not exercised by job-empty.json") {
		t.Fatalf("problems %v notes %v", rep.Problems, rep.Notes)
	}
	if ps := CheckFixtures([]modclient.RouteContract{rc}, files, fxDir); len(ps) != 0 {
		t.Fatalf("a note is not a problem: %v", ps)
	}
	// The paired controls: items that exist but carry no state are a problem, and so is a null leaf.
	populated := string(mutate(t, jobBody, func(m map[string]any) {
		for _, it := range m["items"].([]any) {
			delete(it.(map[string]any), "state")
		}
	}))
	pc := jobContract()
	pc.Vocab = rc.Vocab
	pc.VocabAt = rc.VocabAt
	pc.Shape = modclient.Shape{} // the fixture is not the contract's Shape's business here
	if ps := CheckFixtures([]modclient.RouteContract{pc}, fixtureFS(map[string]string{"job-done.json": populated}), fxDir); !reflect.DeepEqual(problemKinds(ps), []string{string(BadContract), string(VocabInert)}) {
		t.Fatalf("problems = %v", ps)
	}
	nullLeaf := string(mutate(t, jobBody, func(m map[string]any) { m["reason"] = nil }))
	nc := jobContract()
	nc.Vocab = map[string][]string{"why": {"a"}}
	nc.VocabAt = map[string]string{"why": "reason"}
	for _, p := range CheckFixtures([]modclient.RouteContract{nc}, fixtureFS(map[string]string{"job-done.json": nullLeaf}), fxDir) {
		if p.Kind == VocabInert {
			return
		}
	}
	t.Fatal("a null leaf holds no string: the vocabulary constrains nothing there")
}

func TestSessionFinishRefusesAnInertVocabularyAndNotesAnEmptyArrayOnTheWay(t *testing.T) {
	inert := testContractsAt(false)
	s := NewSession(inert, fixtureFS(testFiles()), fxDir, map[string]string{"m3|" + testRoute + "|unavailable": "no harness test makes the module answer unavailable"})
	s.SetRecordMode(false)
	s.Observer()(testObs("ok", 11))
	s.Observer()(testObs("failed", 12))
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	if strings.Count(text, "constrains nothing") != 3 || !strings.Contains(text, "vocabulary "+vocabName+" at "+vocabName) {
		t.Fatalf("every contract's inert vocabulary must fail the gate:\n%s", text)
	}
	// The paired control: with the path the same observations pass.
	good := NewSession(testContractsAt(true), fixtureFS(testFiles()), fxDir, map[string]string{"m3|" + testRoute + "|unavailable": "no harness test makes the module answer unavailable"})
	good.SetRecordMode(false)
	good.Observer()(testObs("ok", 11))
	good.Observer()(testObs("failed", 12))
	gtb := &fakeTB{}
	good.Finish(gtb)
	if gtb.failed() {
		t.Fatalf("with the vocabulary placed the gate passes: %s", gtb.allText())
	}
	// Record mode is about replacing the fixtures: it does not refuse on their vocabularies.
	rec := NewSession(inert, fixtureFS(testFiles()), fxDir, nil)
	rec.SetRecordMode(true)
	rec.Observer()(testObs("ok", 11))
	rtb := &fakeTB{}
	rec.Finish(rtb)
	if rtb.failed() {
		t.Fatalf("record mode must not fail on an inert vocabulary: %s", rtb.allText())
	}
}

func TestSessionFinishLogsTheEmptyArrayNoteOfAVocabulary(t *testing.T) {
	contracts := sessionContracts() // the empty-page contract carries the vocabulary of items[].state over an empty page
	s := NewSession(contracts, fixtureFS(sessionFiles(t)), fxDir, nil)
	s.SetRecordMode(false)
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("must pass: %s", tb.allText())
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), "note: vocabulary items[].state at items[].state is not exercised by job-empty.json") {
		t.Fatalf("the note must be logged:\n%s", strings.Join(tb.logs, "\n"))
	}
}

func TestCheckContractsFailsOncePerProblemLogsTheNotesAndRefusesNoContracts(t *testing.T) {
	good := fakeTB{}
	CheckContracts(&good, testContractsAt(true), fixtureFS(testFiles()), fxDir)
	if good.failed() {
		t.Fatalf("sound contracts must pass: %s", good.allText())
	}
	bad := fakeTB{}
	CheckContracts(&bad, testContractsAt(false), fixtureFS(testFiles()), fxDir)
	if len(bad.errs) != 3 || !strings.Contains(bad.errs[0], "contract fixtures: m3 "+testRoute) || !strings.Contains(bad.errs[0], "constrains nothing") {
		t.Fatalf("errors = %v", bad.errs)
	}
	none := fakeTB{}
	CheckContracts(&none, nil, fixtureFS(testFiles()), fxDir)
	if len(none.errs) != 1 || !strings.Contains(none.errs[0], "found 0 contracts") {
		t.Fatalf("errors = %v", none.errs)
	}
	noted := fakeTB{}
	rc := jobEmptyContract()
	rc.Vocab = map[string][]string{"item_state": {"ok"}}
	rc.VocabAt = map[string]string{"item_state": "items[].state"}
	CheckContracts(&noted, []modclient.RouteContract{rc}, fixtureFS(map[string]string{"job-empty.json": emptyJobBody(t)}), fxDir)
	if noted.failed() || len(noted.logs) != 1 || !strings.HasPrefix(noted.logs[0], "note: vocabulary item_state") {
		t.Fatalf("a note is logged and does not fail: %s", noted.allText())
	}
}

func TestAssertLiveReportsAVocabNewAtTheResolvedPath(t *testing.T) {
	rc := testContractsAt(true)[0]
	tb := &fakeTB{}
	AssertLive(tb, rc, fixtureFS(testFiles()), fxDir, 200, []byte(testBody("z", "weird", "y")), newRec(false))
	text := strings.Join(tb.errs, "\n")
	if !strings.Contains(text, "path="+vocabPos) || !strings.Contains(text, "kind="+string(VocabNew)) {
		t.Fatalf("errors:\n%s", text)
	}
}

func TestSessionRecordModeFitNoteCountsVocabularyValuesThroughVocabAt(t *testing.T) {
	only := testContractsAt(true)[2] // unavailable
	s := NewSession([]modclient.RouteContract{only}, fixtureFS(testFiles()), fxDir, nil)
	s.SetRecordMode(true)
	s.Observer()(testObs("ok", 41))
	tb := &fakeTB{}
	s.Finish(tb)
	if !strings.Contains(strings.Join(tb.logs, "\n"), "case=unavailable file=test.unavailable.json: 0 differences, 0 gaps against the current fixture, 1 vocabulary values that differ") {
		t.Fatalf("logs:\n%s", strings.Join(tb.logs, "\n"))
	}
}
