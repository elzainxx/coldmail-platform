package contractcheck

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Module 1's `signals` is a bag of whichever keys the pipeline layers that ran for a row produced: the Shape lists the two keys the platform reads and marks the object Open.

func signalsShape(open bool) modclient.Shape {
	s := modclient.Object(modclient.Req("syntax_valid", modclient.Bool), modclient.Opt("mx_provider", modclient.Str))
	if open {
		s = s.Open()
	}
	return s
}

func resultsContract(open bool) modclient.RouteContract {
	row := modclient.Object(modclient.Req("email", modclient.Str), modclient.Req("signals", signalsShape(open)))
	return modclient.RouteContract{Module: modclient.M1, Route: "GET /v1/jobs/{id}/results", Case: "page", Fixture: "results-page.json", Status: 200,
		Shape: modclient.Object(modclient.Req("results", modclient.ArrayOf(row)))}
}

// page is a results page with one row per signals bag given.
func page(bags ...string) string {
	rows := make([]string, len(bags))
	for i, b := range bags {
		rows[i] = fmt.Sprintf(`{"email":"user%d@example.com","signals":%s}`, i, b)
	}
	return `{"results":[` + strings.Join(rows, ",") + `]}`
}

const signalsPath = "results[].signals"

func TestCompareAKeyOfAnOpenObjectPresentOnlyLiveIsNoDifferenceAndTheObjectIsNoted(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true}`)
	live := page(`{"syntax_valid":true,"has_typo":false}`)
	rep := Compare(resultsContract(true), []byte(fx), []byte(live), 200)
	wantDiffs(t, rep)
	if !reflect.DeepEqual(rep.Open, []string{signalsPath}) {
		t.Fatalf("Open = %v", rep.Open)
	}
	// The paired control: the same two bodies under a Shape that is not open are a missing key and an added key.
	plain := Compare(resultsContract(false), []byte(fx), []byte(live), 200)
	wantDiffs(t, plain, string(KeyMissing)+":"+signalsPath+".mx_found", string(KeyAdded)+":"+signalsPath+".has_typo")
	if len(plain.Open) != 0 {
		t.Fatalf("a Shape that is not open notes nothing: %v", plain.Open)
	}
}

func TestCompareAKeyOfAnOpenObjectPresentOnlyInTheFixtureIsNoDifference(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true,"null_mx":false}`)
	live := page(`{"syntax_valid":false}`)
	wantDiffs(t, Compare(resultsContract(true), []byte(fx), []byte(live), 200))
	wantDiffs(t, Compare(resultsContract(false), []byte(fx), []byte(live), 200), string(KeyMissing)+":"+signalsPath+".mx_found", string(KeyMissing)+":"+signalsPath+".null_mx")
}

func TestCompareAListedRequiredKeyMissingFromAnOpenObjectIsStillAMissingKeyAndAShapeViolation(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true}`)
	live := page(`{"mx_found":true}`)
	wantDiffs(t, Compare(resultsContract(true), []byte(fx), []byte(live), 200),
		string(KeyMissing)+":"+signalsPath+".syntax_valid", string(ShapeViolation)+":"+signalsPath+".syntax_valid")
	// The key present only in the live body, with the listed one present: no difference (the control for the above).
	wantDiffs(t, Compare(resultsContract(true), []byte(fx), []byte(page(`{"syntax_valid":true,"extra":1}`)), 200))
}

func TestCompareAListedOptionalKeyAndAnUnlistedOneOfAnOpenObjectAreNotedSeparately(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_provider":"example"}`)
	live := page(`{"syntax_valid":true,"mx_found":true}`)
	rep := Compare(resultsContract(true), []byte(fx), []byte(live), 200)
	wantDiffs(t, rep)
	if !reflect.DeepEqual(rep.Optional, []string{signalsPath + ".mx_provider"}) || !reflect.DeepEqual(rep.Open, []string{signalsPath}) {
		t.Fatalf("Optional = %v, Open = %v", rep.Optional, rep.Open)
	}
	// Present on both sides, the listed optional key is still compared.
	bad := Compare(resultsContract(true), []byte(fx), []byte(page(`{"syntax_valid":true,"mx_provider":4}`)), 200)
	wantDiffs(t, bad, string(TypeChanged)+":"+signalsPath+".mx_provider", string(ShapeViolation)+":"+signalsPath+".mx_provider")
}

func TestCompareAnOpenObjectInsideEveryRowOfAPageIsNotedOnceWhateverTheRowsCarry(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true}`, `{"syntax_valid":true,"is_role":false}`)
	live := page(`{"syntax_valid":true}`, `{"syntax_valid":false,"has_typo":true,"engine_mx_hosts":["mx.example.com"]}`, `{"syntax_valid":true,"null_mx":true}`)
	rep := Compare(resultsContract(true), []byte(fx), []byte(live), 200)
	wantDiffs(t, rep)
	n := 0
	for _, p := range rep.Open {
		if p == signalsPath {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the object must be noted once, not once per row: %v", rep.Open)
	}
}

func TestCompareTheSameKeyOnBothSidesOfAnOpenObjectWithAnotherTypeIsStillATypeChange(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true}`)
	live := page(`{"syntax_valid":true,"mx_found":"yes"}`)
	rep := Compare(resultsContract(true), []byte(fx), []byte(live), 200)
	wantDiffs(t, rep, string(TypeChanged)+":"+signalsPath+".mx_found")
	if len(rep.Open) != 0 {
		t.Fatalf("a key on both sides is compared, not exempted: %v", rep.Open)
	}
	// The paired control: the same key with the same type is clean.
	wantDiffs(t, Compare(resultsContract(true), []byte(fx), []byte(page(`{"syntax_valid":false,"mx_found":false}`)), 200))
}

func TestCompareANullKeyOfAnOpenObjectOnOneSideIsNotADifferenceEither(t *testing.T) {
	fx := page(`{"syntax_valid":true}`)
	live := page(`{"syntax_valid":true,"mx_provider_guess":null}`)
	wantDiffs(t, Compare(resultsContract(true), []byte(fx), []byte(live), 200))
}

func TestCompareAnOpenObjectAtTheRootNamesTheBodyInItsNote(t *testing.T) {
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200, Shape: modclient.Object(modclient.Req("id", modclient.Str)).Open()}
	rep := Compare(rc, []byte(`{"id":"a","one":1}`), []byte(`{"id":"b","two":2}`), 200)
	wantDiffs(t, rep)
	if !reflect.DeepEqual(rep.Open, []string{"(body)"}) {
		t.Fatalf("Open = %v", rep.Open)
	}
}

func TestViewOfCarriesTheOpenMarkOfAnObjectAndOnlyOfAnObject(t *testing.T) {
	v, err := viewOf(modclient.Object(modclient.Req("signals", modclient.Object().Open()), modclient.Req("tags", modclient.Str.Open())))
	if err != nil {
		t.Fatal(err)
	}
	if !v.field("signals").view.isOpen() || v.field("tags").view.isOpen() || v.isOpen() {
		t.Fatal("only the signals object is open")
	}
}

func TestMatchTwoContractsThatDifferOnlyByTheKeysOfAnOpenObjectShareTheObservationsWithoutADifference(t *testing.T) {
	a, b := resultsContract(true), resultsContract(true)
	a.Case, a.Fixture = "a", "a.json"
	b.Case, b.Fixture = "b", "b.json"
	files := fixtureFS(map[string]string{"a.json": page(`{"syntax_valid":true,"mx_found":true}`), "b.json": page(`{"syntax_valid":true,"has_typo":false}`)})
	mk := func(n int, bag string) Observation {
		return Observation{Module: modclient.M1, Route: "m1.results", Method: "GET", Path: fmt.Sprintf("/v1/jobs/j%d/results", n), Status: 200, Body: []byte(page(bag))}
	}
	obs := []Observation{mk(1, `{"syntax_valid":true,"null_mx":true}`), mk(2, `{"syntax_valid":false,"is_role":true}`)}
	for _, order := range [][]modclient.RouteContract{{a, b}, {b, a}} {
		res := Match(order, files, fxDir, obs)
		if len(res.Matched) != 2 || len(res.Unexercised) != 0 {
			t.Fatalf("matched %d unexercised %d: both contracts must be exercised", len(res.Matched), len(res.Unexercised))
		}
		for _, m := range res.Matched {
			if !m.Report.Clean() {
				t.Fatalf("an open object's keys must not make a difference:\n%s", detailOf(m.Report))
			}
		}
	}
	// The distance of an open-object-only difference equals the distance of an exact fit; the control without Open does not.
	exact := newRank(Compare(resultsContract(true), []byte(page(`{"syntax_valid":true}`)), []byte(page(`{"syntax_valid":true}`)), 200))
	open := newRank(Compare(resultsContract(true), []byte(page(`{"syntax_valid":true,"a":1}`)), []byte(page(`{"syntax_valid":true,"b":1}`)), 200))
	plain := newRank(Compare(resultsContract(false), []byte(page(`{"syntax_valid":true,"a":1}`)), []byte(page(`{"syntax_valid":true,"b":1}`)), 200))
	if open != exact || plain.total != 2 {
		t.Fatalf("exact %+v, open %+v, plain %+v", exact, open, plain)
	}
}

func TestSessionManyRowsWithDifferentSignalsPassAndTheOpenObjectIsNotedOnce(t *testing.T) {
	bags := []string{`{"syntax_valid":true,"mx_found":true}`, `{"syntax_valid":true,"has_typo":false}`, `{"syntax_valid":false}`, `{"syntax_valid":true,"is_role":true,"engine_mx_hosts":["mx.example.com"]}`}
	var obs []Observation
	for i := 0; i < 43; i++ {
		obs = append(obs, Observation{Module: modclient.M1, Route: "m1.results", Method: "GET", Path: fmt.Sprintf("/v1/jobs/j%d/results", i), Status: 200,
			Body: []byte(page(bags[i%len(bags)], bags[(i+1)%len(bags)]))})
	}
	run := func(open bool) *fakeTB {
		s := NewSession([]modclient.RouteContract{resultsContract(open)}, fixtureFS(map[string]string{"results-page.json": page(`{"syntax_valid":true,"mx_found":true}`)}), fxDir, nil)
		s.SetRecordMode(false)
		for _, o := range obs {
			s.Observer()(o)
		}
		tb := &fakeTB{}
		s.Finish(tb)
		return tb
	}
	tb := run(true)
	if tb.failed() {
		t.Fatalf("rows with different signals must pass under an open object: %s", tb.allText())
	}
	want := "note: open object per the Shape, extra keys not compared: " + signalsPath + " (m1 GET /v1/jobs/{id}/results case=page)"
	if n := strings.Count(strings.Join(tb.logs, "\n"), want); n != 1 {
		t.Fatalf("the note must appear exactly once, appeared %d times:\n%s", n, strings.Join(tb.logs, "\n"))
	}
	// The paired control: the same observations against a Shape that is not open fail with the differences the harness saw.
	bad := run(false)
	text := strings.Join(bad.errs, "\n")
	if !strings.Contains(text, "path="+signalsPath+".mx_found") || !strings.Contains(text, "kind="+string(KeyMissing)) || !strings.Contains(text, "kind="+string(KeyAdded)) {
		t.Fatalf("without Open the rows differ:\n%s", text)
	}
}

func TestSessionRecordModeAndAssertLiveNoteAnOpenObjectAndCountNoDifference(t *testing.T) {
	fx := page(`{"syntax_valid":true,"mx_found":true}`)
	live := page(`{"syntax_valid":true,"has_typo":false}`)
	s := NewSession([]modclient.RouteContract{resultsContract(true)}, fixtureFS(map[string]string{"results-page.json": fx}), fxDir, nil)
	s.SetRecordMode(true)
	s.Observer()(Observation{Module: modclient.M1, Route: "m1.results", Method: "GET", Path: "/v1/jobs/j1/results", Status: 200, Body: []byte(live)})
	tb := &fakeTB{}
	s.Finish(tb)
	logs := strings.Join(tb.logs, "\n")
	if tb.failed() || !strings.Contains(logs, "case=page file=results-page.json: 0 differences") || !strings.Contains(logs, "open object per the Shape, extra keys not compared: "+signalsPath) {
		t.Fatalf("record mode: %s", tb.allText())
	}
	ab := &fakeTB{}
	AssertLive(ab, resultsContract(true), fixtureFS(map[string]string{"results-page.json": fx}), fxDir, 200, []byte(live), newRec(false))
	if ab.failed() || !strings.Contains(strings.Join(ab.logs, "\n"), "open object per the Shape, extra keys not compared: "+signalsPath) {
		t.Fatalf("AssertLive: %s", ab.allText())
	}
}
