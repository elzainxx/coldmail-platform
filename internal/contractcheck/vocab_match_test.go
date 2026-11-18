package contractcheck

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Module 3's test button answers one body in three states: the three contracts' fixtures differ in nothing but the value of verification.state.

const testRoute = "POST /v1/send-byo/accounts/{id}/test"

var testShape = modclient.Object(
	modclient.Req("account_id", modclient.Str),
	modclient.Req("verification", modclient.Object(modclient.Req("state", modclient.Str), modclient.Req("detail", modclient.Str))),
)

func testBody(accountID, state, detail string) string {
	return fmt.Sprintf(`{"account_id":%q,"verification":{"state":%q,"detail":%q}}`, accountID, state, detail)
}

func testContracts(withVocab bool) []modclient.RouteContract {
	var out []modclient.RouteContract
	for _, st := range []string{"ok", "failed", "unavailable"} {
		rc := modclient.RouteContract{Module: modclient.M3, Route: testRoute, Case: st, Fixture: "test." + st + ".json", Status: 200, Shape: testShape}
		if withVocab {
			rc.Vocab = map[string][]string{"verification.state": {"ok", "failed", "unavailable", "pending"}}
		}
		out = append(out, rc)
	}
	return out
}

func testFiles() map[string]string {
	return map[string]string{
		"test.ok.json":          testBody("00000000-0000-4000-8000-000000000001", "ok", "checked"),
		"test.failed.json":      testBody("00000000-0000-4000-8000-000000000002", "failed", "checked"),
		"test.unavailable.json": testBody("00000000-0000-4000-8000-000000000003", "unavailable", "checked"),
	}
}

func testObs(state string, n int) Observation {
	id := fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", n)
	return Observation{Module: modclient.M3, Route: "m3.accounts.test", Method: "POST", Path: "/v1/send-byo/accounts/" + id + "/test", Status: 200, Body: []byte(testBody(id, state, "smtp "+state))}
}

func TestMatchVocabularyValuesTellApartContractsWhoseFixturesDifferOnlyByAVocabularyValue(t *testing.T) {
	res := Match(testContracts(true), fixtureFS(testFiles()), fxDir, []Observation{testObs("ok", 11), testObs("failed", 12), testObs("ok", 13)})
	if len(res.Matched) != 3 {
		t.Fatalf("matched %d", len(res.Matched))
	}
	for _, m := range res.Matched {
		want := "ok"
		if strings.Contains(string(m.Observation.Body), `"state":"failed"`) {
			want = "failed"
		}
		if m.Contract.Case != want || len(m.Tied) != 0 || !m.Report.Clean() {
			t.Fatalf("a %q response went to %q (tied %v, diffs:\n%s)", want, m.Contract.Case, m.Tied, detailOf(m.Report))
		}
	}
	// The contract no response of its value came for stays unexercised, with a nearest miss that names the vocabulary path and never a value.
	if len(res.Unexercised) != 1 || res.Unexercised[0].Case != "unavailable" || len(res.NearestMisses) != 1 {
		t.Fatalf("unexercised %+v nearest %+v", res.Unexercised, res.NearestMisses)
	}
	nm := res.NearestMisses[0]
	if nm.Observed != 3 || !reflect.DeepEqual(nm.Misses, []Miss{{Path: "verification.state", Kind: KindVocabValue}}) {
		t.Fatalf("nearest miss = %+v", nm)
	}
	want := "nearest miss: m3 " + testRoute + " case=unavailable: 1 differences against the closest of 3 observed responses: verification.state " + KindVocabValue
	if nm.String() != want {
		t.Fatalf("line:\n%s\nwant:\n%s", nm, want)
	}
	// The paired control: without the vocabulary the three contracts cannot be told apart (the behaviour that filed responses under the wrong case): the first response ties all three.
	bare := Match(testContracts(false), fixtureFS(testFiles()), fxDir, []Observation{testObs("ok", 11)})
	if len(bare.Matched) != 1 || len(bare.Matched[0].Tied) != 2 {
		t.Fatalf("without a Vocab the three contracts tie: %+v", bare.Matched)
	}
}

func TestMatchVocabularyValuesAreIndependentOfTheOrderOfContractsAndObservations(t *testing.T) {
	files := fixtureFS(testFiles())
	obs := []Observation{testObs("ok", 11), testObs("failed", 12), testObs("ok", 13), testObs("failed", 14), testObs("unavailable", 15)}
	rng := rand.New(rand.NewSource(21))
	for i := 0; i < 20; i++ {
		contracts := testContracts(true)
		rng.Shuffle(len(contracts), func(a, b int) { contracts[a], contracts[b] = contracts[b], contracts[a] })
		shuffled := append([]Observation(nil), obs...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		res := Match(contracts, files, fxDir, shuffled)
		if len(res.Matched) != 5 || len(res.Unexercised) != 0 {
			t.Fatalf("shuffle %d: matched %d unexercised %d", i, len(res.Matched), len(res.Unexercised))
		}
		for _, m := range res.Matched {
			state := "ok"
			for _, st := range []string{"failed", "unavailable"} {
				if strings.Contains(string(m.Observation.Body), `"state":"`+st+`"`) {
					state = st
				}
			}
			if m.Contract.Case != state {
				t.Fatalf("shuffle %d: a %q response went to %q", i, state, m.Contract.Case)
			}
		}
	}
}

func TestMatchAValueThatEqualsNoFixtureFallsBackToTheStructuralDistanceDeterministically(t *testing.T) {
	files := fixtureFS(testFiles())
	for _, state := range []string{"pending", "somethingelse"} { // in the closed set, and outside it
		rng := rand.New(rand.NewSource(5))
		for i := 0; i < 20; i++ {
			contracts := testContracts(true)
			rng.Shuffle(len(contracts), func(a, b int) { contracts[a], contracts[b] = contracts[b], contracts[a] })
			res := Match(contracts, files, fxDir, []Observation{testObs(state, 31)})
			if len(res.Matched) != 1 {
				t.Fatalf("matched %d", len(res.Matched))
			}
			m := res.Matched[0]
			// All three fixtures are equally far, so it goes to the first contract in contract order, with the vocabulary mismatch visible and the tie reported.
			if m.Contract.Case != contracts[0].Case || len(m.Tied) != 2 || !reflect.DeepEqual(m.Report.VocabMismatch, []string{"verification.state"}) {
				t.Fatalf("state %q: went to %q (tied %v, mismatch %v), want %q", state, m.Contract.Case, m.Tied, m.Report.VocabMismatch, contracts[0].Case)
			}
			if state == "somethingelse" && len(m.Report.Of(VocabNew)) != 1 {
				t.Fatalf("a value outside the closed set is still its own difference:\n%s", detailOf(m.Report))
			}
			if state == "pending" && !m.Report.Clean() {
				t.Fatalf("a value inside the closed set is not a difference:\n%s", detailOf(m.Report))
			}
		}
	}
}

func TestCompareReportsAVocabularyPathWhereTheFixtureAndTheLiveBodyCarryDifferentValuesWithoutMakingItADifference(t *testing.T) {
	rc := testContracts(true)[0]
	same := Compare(rc, []byte(testBody("a", "ok", "x")), []byte(testBody("b", "ok", "y")), 200)
	if !same.Clean() || len(same.VocabMismatch) != 0 {
		t.Fatalf("the same value is no mismatch: %+v", same)
	}
	diff := Compare(rc, []byte(testBody("a", "ok", "x")), []byte(testBody("b", "failed", "y")), 200)
	if !diff.Clean() || !reflect.DeepEqual(diff.VocabMismatch, []string{"verification.state"}) {
		t.Fatalf("a different value is a mismatch and not a difference: %+v", diff)
	}
	// A fixture that carries no string there (null) says nothing: no mismatch.
	nullFx := `{"account_id":"a","verification":{"state":null,"detail":"x"}}`
	rc.Shape = modclient.Shape{}
	if r := Compare(rc, []byte(nullFx), []byte(testBody("b", "failed", "y")), 200); len(r.VocabMismatch) != 0 {
		t.Fatalf("a null fixture value is no mismatch: %+v", r.VocabMismatch)
	}
	// A Vocab key that is not a path is the contract's problem, as before, and no mismatch.
	rc.Vocab = map[string][]string{"verification[.state": {"ok"}}
	r := Compare(rc, []byte(testBody("a", "ok", "x")), []byte(testBody("b", "failed", "y")), 200)
	if len(r.VocabMismatch) != 0 || len(r.Of(ContractInvalid)) != 1 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCompareVocabularyMismatchInAnArrayMeansNoValueInCommon(t *testing.T) {
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200,
		Vocab: map[string][]string{"items[].verdict": {"valid", "invalid", "risky"}}}
	row := func(vs ...string) string {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = fmt.Sprintf(`{"verdict":%q}`, v)
		}
		return `{"items":[` + strings.Join(parts, ",") + `]}`
	}
	fx := row("valid", "invalid")
	cases := []struct {
		name string
		live string
		want []string
	}{
		{"a subset of the fixture's values", row("valid"), nil},
		{"the same values", row("invalid", "valid"), nil},
		{"one value in common among others", row("risky", "valid"), nil},
		{"nothing in common", row("risky"), []string{"items[].verdict"}},
		{"an empty page", `{"items":[]}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Compare(rc, []byte(fx), []byte(c.live), 200).VocabMismatch; !reflect.DeepEqual(got, c.want) {
				t.Fatalf("mismatch = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRankCountsAVocabularyMismatchAsOneUnitAndMissesNameItByKind(t *testing.T) {
	rep := Report{VocabMismatch: []string{"verification.state"}}
	if got := newRank(rep); got != (rank{total: 1, oneSided: 0}) {
		t.Fatalf("rank = %+v", got)
	}
	both := Report{Diffs: []Diff{{}}, Gaps: []Gap{{Side: "live", Path: "p"}}, VocabMismatch: []string{"a", "b"}}
	if got := newRank(both); got != (rank{total: 4, oneSided: 1}) {
		t.Fatalf("rank = %+v: one difference, one gap and two vocabulary values are a distance of four", got)
	}
	if got := missesOf(rep); !reflect.DeepEqual(got, []Miss{{Path: "verification.state", Kind: "VocabValue"}}) {
		t.Fatalf("misses = %+v", got)
	}
	// A vocabulary mismatch does not outrank structure: a contract with it and no difference still beats one with two differences.
	if !newRank(rep).less(newRank(Report{Diffs: []Diff{{}, {}}})) {
		t.Fatal("one vocabulary mismatch is nearer than two differences")
	}
}

func TestSessionRecordModeFitNoteCountsTheVocabularyValuesThatDiffer(t *testing.T) {
	only := testContracts(true)[2] // unavailable
	s := NewSession([]modclient.RouteContract{only}, fixtureFS(testFiles()), fxDir, nil)
	s.SetRecordMode(true)
	s.Observer()(testObs("ok", 41))
	tb := &fakeTB{}
	s.Finish(tb)
	if !strings.Contains(strings.Join(tb.logs, "\n"), "case=unavailable file=test.unavailable.json: 0 differences, 0 gaps against the current fixture, 1 vocabulary values that differ") {
		t.Fatalf("a recording of ok filed under unavailable must say so:\n%s", strings.Join(tb.logs, "\n"))
	}
	// The paired control: filed under its own case, nothing differs.
	exact := NewSession([]modclient.RouteContract{testContracts(true)[0]}, fixtureFS(testFiles()), fxDir, nil)
	exact.SetRecordMode(true)
	exact.Observer()(testObs("ok", 42))
	etb := &fakeTB{}
	exact.Finish(etb)
	if !strings.Contains(strings.Join(etb.logs, "\n"), "case=ok file=test.ok.json: 0 differences, 0 gaps against the current fixture, 0 vocabulary values that differ") {
		t.Fatalf("an exact fit must say 0:\n%s", strings.Join(etb.logs, "\n"))
	}
}

func TestSessionAnAllowlistedContractNoResponseOfItsValueCameForIsNotStaleAndShowsItsNearestMiss(t *testing.T) {
	allow := map[string]string{"m3|" + testRoute + "|unavailable": "the module answers unavailable only while its mail host is down, which no harness test arranges"}
	s := NewSession(testContracts(true), fixtureFS(testFiles()), fxDir, allow)
	s.SetRecordMode(false)
	s.Observer()(testObs("ok", 51))
	s.Observer()(testObs("failed", 52))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("the allowed contract was not exercised, so the entry is not stale: %s", tb.allText())
	}
	logs := strings.Join(tb.logs, "\n")
	if !strings.Contains(logs, "nearest miss: m3 "+testRoute+" case=unavailable: 1 differences against the closest of 2 observed responses: verification.state "+KindVocabValue) {
		t.Fatalf("the allowed contract must still show its nearest miss:\n%s", logs)
	}
	// The paired control: a response of that value makes the entry stale, and every contract exercised.
	c := NewSession(testContracts(true), fixtureFS(testFiles()), fxDir, allow)
	c.SetRecordMode(false)
	co := c.Observer()
	co(testObs("ok", 51))
	co(testObs("failed", 52))
	co(testObs("unavailable", 53))
	ctb := &fakeTB{}
	c.Finish(ctb)
	if len(ctb.errs) != 1 || !strings.Contains(ctb.errs[0], "is stale") {
		t.Fatalf("errors = %v", ctb.errs)
	}
}

func errorBody(code, field, issue string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":"One or more fields are invalid.","details":[{"field":%q,"issue":%q}]}}`, code, field, issue)
}

func TestMatchErrorContractsOfOneCodeKeepWorkingAsBeforeAndAreToldApartWhenTheIssueIsInTheVocab(t *testing.T) {
	shape := modclient.Object(modclient.Req("error", modclient.Object(
		modclient.Req("code", modclient.Str), modclient.Req("message", modclient.Str),
		modclient.Req("details", modclient.ArrayOf(modclient.Object(modclient.Req("field", modclient.Str), modclient.Req("issue", modclient.Str)))))))
	mk := func(cas, fixture string, vocab map[string][]string) modclient.RouteContract {
		return modclient.RouteContract{Module: modclient.M3, Route: "GET /v1/send-byo/accounts", Case: cas, Fixture: fixture, Status: 400, Shape: shape, Vocab: vocab}
	}
	files := fixtureFS(map[string]string{
		"err.validation.json": errorBody("validation_failed", "name", "required"),
		"err.cursor.json":     errorBody("validation_failed", "cursor", "malformed"),
	})
	validation := Observation{Module: modclient.M3, Route: "m3.accounts.list", Method: "GET", Path: "/v1/send-byo/accounts", Status: 400, Body: []byte(errorBody("validation_failed", "name", "required"))}
	cursor := Observation{Module: modclient.M3, Route: "m3.accounts.list", Method: "GET", Path: "/v1/send-byo/accounts", Status: 400, Body: []byte(errorBody("validation_failed", "cursor", "malformed"))}

	// With only the code in the vocabulary the two fixtures differ in nothing the matcher reads: as before, they tie, each observation takes a contract no other observation has
	// taken, both contracts are exercised and nothing is a difference.
	codeOnly := map[string][]string{"error.code": {"validation_failed"}}
	contracts := []modclient.RouteContract{mk("validation_failed", "err.validation.json", codeOnly), mk("cursor_malformed", "err.cursor.json", codeOnly)}
	res := Match(contracts, files, fxDir, []Observation{validation, cursor})
	if len(res.Matched) != 2 || len(res.Unexercised) != 0 {
		t.Fatalf("matched %d unexercised %d", len(res.Matched), len(res.Unexercised))
	}
	for _, m := range res.Matched {
		if len(m.Report.VocabMismatch) != 0 {
			t.Fatalf("one code is no vocabulary mismatch: %v", m.Report.VocabMismatch)
		}
	}
	// With the issue in the vocabulary each observation goes to its own contract, in every order.
	withIssue := map[string][]string{"error.code": {"validation_failed"}, "error.details[].issue": {"required", "malformed"}}
	contracts = []modclient.RouteContract{mk("validation_failed", "err.validation.json", withIssue), mk("cursor_malformed", "err.cursor.json", withIssue)}
	rng := rand.New(rand.NewSource(13))
	for i := 0; i < 20; i++ {
		cs := append([]modclient.RouteContract(nil), contracts...)
		rng.Shuffle(len(cs), func(a, b int) { cs[a], cs[b] = cs[b], cs[a] })
		obs := []Observation{validation, cursor}
		rng.Shuffle(len(obs), func(a, b int) { obs[a], obs[b] = obs[b], obs[a] })
		res = Match(cs, files, fxDir, obs)
		for _, m := range res.Matched {
			want := "validation_failed"
			if strings.Contains(string(m.Observation.Body), "cursor") {
				want = "cursor_malformed"
			}
			if m.Contract.Case != want || len(m.Tied) != 0 {
				t.Fatalf("shuffle %d: a %q error went to %q (tied %v)", i, want, m.Contract.Case, m.Tied)
			}
		}
	}
}
