package contractcheck

import (
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// jobShape declares provider_message_id (top level) and items[].note as Opt; created_at and every other key of jobBody are not in the Shape at all.

// withoutOptional is jobBody with the two Opt keys taken out of it (the fixture of a module state that omits them).
func withoutOptional(t *testing.T) string {
	t.Helper()
	return string(mutate(t, jobBody, func(m map[string]any) {
		delete(m, "provider_message_id")
		for _, it := range m["items"].([]any) {
			delete(it.(map[string]any), "note")
		}
	}))
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func TestCompareAnOptionalKeyPresentOnlyInTheLiveBodyIsNoDifferenceAndIsNoted(t *testing.T) {
	rep := Compare(jobContract(), []byte(withoutOptional(t)), []byte(jobBody), 200)
	wantDiffs(t, rep)
	if got := sorted(rep.Optional); !reflect.DeepEqual(got, []string{"items[].note", "provider_message_id"}) {
		t.Fatalf("Optional = %v", got)
	}
	// The paired control: a key the Shape does not list, present only live, is still an added key.
	fx := string(mutate(t, jobBody, func(m map[string]any) { delete(m, "created_at") }))
	wantDiffs(t, Compare(jobContract(), []byte(fx), []byte(jobBody), 200), string(KeyAdded)+":created_at")
}

func TestCompareAnOptionalKeyPresentOnlyInTheFixtureIsNoDifferenceAndIsNoted(t *testing.T) {
	rep := Compare(jobContract(), []byte(jobBody), []byte(withoutOptional(t)), 200)
	wantDiffs(t, rep)
	if got := sorted(rep.Optional); !reflect.DeepEqual(got, []string{"items[].note", "provider_message_id"}) {
		t.Fatalf("Optional = %v", got)
	}
	// The paired control: a key the Shape lists as required, present only in the fixture, is a missing key and a Shape violation as it always was.
	live := string(mutate(t, jobBody, func(m map[string]any) { delete(m, "state") }))
	wantDiffs(t, Compare(jobContract(), []byte(jobBody), []byte(live), 200), string(KeyMissing)+":state", string(ShapeViolation)+":state")
	// And a key the Shape does not list, present only in the fixture, is still a missing key.
	live = string(mutate(t, jobBody, func(m map[string]any) { delete(m, "created_at") }))
	wantDiffs(t, Compare(jobContract(), []byte(jobBody), []byte(live), 200), string(KeyMissing)+":created_at")
}

func TestCompareAnOptionalKeyPresentOnBothSidesIsStillCompared(t *testing.T) {
	// A changed type on both sides is a difference (and the adapter's own Shape refuses it) ...
	live := string(mutate(t, jobBody, func(m map[string]any) {
		m["provider_message_id"] = 5
		m["items"].([]any)[1].(map[string]any)["note"] = 7
	}))
	rep := Compare(jobContract(), []byte(jobBody), []byte(live), 200)
	// (the Shape check names the first key it refuses in the path and every one of them in its words)
	wantDiffs(t, rep, string(TypeChanged)+":provider_message_id", string(TypeChanged)+":items[].note", string(ShapeViolation)+":provider_message_id")
	if v := rep.Of(ShapeViolation); len(v) != 1 || !strings.Contains(v[0].Detail, "items[1].note") {
		t.Fatalf("the Shape violation must also name the note: %+v", v)
	}
	if len(rep.Optional) != 0 {
		t.Fatalf("a key present on both sides is not noted as optional: %v", rep.Optional)
	}
	// ... and the paired control: the same key, unchanged on both sides, is clean and unnoted.
	clean := Compare(jobContract(), []byte(jobBody), []byte(jobBody), 200)
	wantDiffs(t, clean)
	if len(clean.Optional) != 0 {
		t.Fatalf("Optional = %v", clean.Optional)
	}
}

func TestCompareNotesAnOptionalPathOnceHoweverManyElementsCarryIt(t *testing.T) {
	fx := string(mutate(t, jobBody, func(m map[string]any) {
		m["items"].([]any)[0].(map[string]any)["x"] = 1
		for _, it := range m["items"].([]any) {
			delete(it.(map[string]any), "note")
		}
		delete(m["items"].([]any)[0].(map[string]any), "x")
	}))
	live := string(mutate(t, jobBody, func(m map[string]any) {
		items := m["items"].([]any)
		items = append(items, map[string]any{"id": "00000000-0000-4000-8000-000000000009", "state": "ok", "note": "a"}, map[string]any{"id": "00000000-0000-4000-8000-00000000000a", "state": "ok", "note": "b"})
		m["items"] = items
	}))
	rep := Compare(jobContract(), []byte(fx), []byte(live), 200)
	wantDiffs(t, rep)
	n := 0
	for _, p := range rep.Optional {
		if p == "items[].note" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("items[].note noted %d times, want once: %v", n, rep.Optional)
	}
}

func TestCompareAnOptionalKeyInsideAMapElementIsAlsoNotCompared(t *testing.T) {
	rc := modclient.RouteContract{Module: modclient.M1, Route: "GET /x", Fixture: "x.json", Status: 200,
		Shape: modclient.Object(modclient.Req("checks", modclient.MapOf(modclient.Object(modclient.Req("ready", modclient.Bool), modclient.Opt("detail", modclient.Str)))))}
	fx := `{"checks":{"db":{"ready":true},"queue":{"ready":true}}}`
	live := `{"checks":{"db":{"ready":true,"detail":"slow"},"cache":{"ready":false}}}`
	rep := Compare(rc, []byte(fx), []byte(live), 200)
	wantDiffs(t, rep)
	if !reflect.DeepEqual(rep.Optional, []string{"checks{}.detail"}) {
		t.Fatalf("Optional = %v", rep.Optional)
	}
}

func TestCompareAnOptionalAndAnUnlistedKeyAddedTogetherGiveOneDifferenceAndOneNote(t *testing.T) {
	live := string(mutate(t, withoutOptional(t), func(m map[string]any) {
		m["provider_message_id"] = "abc"
		m["brand_new"] = true
	}))
	rep := Compare(jobContract(), []byte(withoutOptional(t)), []byte(live), 200)
	wantDiffs(t, rep, string(KeyAdded)+":brand_new")
	if !reflect.DeepEqual(rep.Optional, []string{"provider_message_id"}) {
		t.Fatalf("Optional = %v", rep.Optional)
	}
}

func TestMatchDistanceCountsNonOptionalKeysAndNotOptionalOnes(t *testing.T) {
	// An optional key on one side only adds nothing to the distance; a key the Shape does not list adds one.
	optOnly := newRank(Compare(jobContract(), []byte(withoutOptional(t)), []byte(jobBody), 200))
	exact := newRank(Compare(jobContract(), []byte(jobBody), []byte(jobBody), 200))
	if optOnly != exact {
		t.Fatalf("an optional key must not count: %+v vs %+v", optOnly, exact)
	}
	fx := string(mutate(t, jobBody, func(m map[string]any) { m["legacy"] = true }))
	nonOpt := newRank(Compare(jobContract(), []byte(fx), []byte(jobBody), 200))
	if !exact.less(nonOpt) || nonOpt.total != 1 {
		t.Fatalf("a non-optional difference must count one: %+v", nonOpt)
	}
	// Through Match: of two contracts, the one whose fixture differs by a non-optional key loses to the exact one, whatever the order, and the loser is not lost silently.
	a, b := jobContract(), jobContract()
	a.Case, a.Fixture = "a", "a.json"
	b.Case, b.Fixture = "b", "b.json"
	files := fixtureFS(map[string]string{"a.json": jobBody, "b.json": fx})
	for _, order := range [][]modclient.RouteContract{{a, b}, {b, a}} {
		res := Match(order, files, fxDir, []Observation{jobObs("/v1/jobs/1", 200, jobBody)})
		if len(res.Matched) != 1 || res.Matched[0].Contract.Case != "a" || len(res.NearestMisses) != 1 || len(res.NearestMisses[0].Misses) != 1 {
			t.Fatalf("the exact fixture must win and the other must keep its nearest miss: %+v", res)
		}
	}
}

func TestMatchTwoContractsThatDifferOnlyByAnOptionalKeyShareTheObservationsWithoutADifference(t *testing.T) {
	with, without := jobContract(), jobContract()
	with.Case, with.Fixture = "with", "with.json"
	without.Case, without.Fixture = "without", "without.json"
	files := fixtureFS(map[string]string{"with.json": jobBody, "without.json": withoutOptional(t)})
	liveWith := jobObs("/v1/jobs/1", 200, jobBody)
	liveWithout := jobObs("/v1/jobs/2", 200, withoutOptional(t))
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 40; i++ {
		contracts := []modclient.RouteContract{with, without}
		if i%2 == 1 {
			contracts = []modclient.RouteContract{without, with}
		}
		obs := []Observation{liveWith, liveWithout}
		rng.Shuffle(len(obs), func(a, b int) { obs[a], obs[b] = obs[b], obs[a] })
		res := Match(contracts, files, fxDir, obs)
		if len(res.Matched) != 2 || len(res.Unexercised) != 0 {
			t.Fatalf("matched %d unexercised %d: both contracts must be exercised", len(res.Matched), len(res.Unexercised))
		}
		for _, m := range res.Matched {
			if !m.Report.Clean() {
				t.Fatalf("an optional key must not make a difference:\n%s", detailOf(m.Report))
			}
		}
	}
	// One observation alone goes to the first contract nothing has been assigned to, in contract order, with no difference.
	for _, order := range [][]modclient.RouteContract{{with, without}, {without, with}} {
		res := Match(order, files, fxDir, []Observation{liveWith})
		if len(res.Matched) != 1 || res.Matched[0].Contract.Case != order[0].Case || !res.Matched[0].Report.Clean() || len(res.Matched[0].Tied) != 1 {
			t.Fatalf("a lone observation goes to the first of two equal fits (%s): %+v", order[0].Case, res.Matched)
		}
		if len(res.NearestMisses) != 1 || len(res.NearestMisses[0].Misses) != 0 {
			t.Fatalf("the contract left over differs only by an optional key, so its nearest miss is empty: %+v", res.NearestMisses)
		}
	}
}

func TestSessionAnOptionalKeyOnOneSideIsNotedOncePerPathAndFailsNothing(t *testing.T) {
	files := sessionFiles(t)
	files["job-done.json"] = withoutOptional(t)
	s := NewSession(sessionContracts(), fixtureFS(files), fxDir, nil)
	s.SetRecordMode(false)
	obs := s.Observer()
	for i, path := range []string{"/v1/jobs/a", "/v1/jobs/b", "/v1/jobs/c"} {
		body := string(mutate(t, jobBody, func(m map[string]any) {
			m["job_id"] = strings.Replace("00000000-0000-4000-8000-00000000000N", "N", string(rune('1'+i)), 1)
		}))
		obs(jobObs(path, 200, body))
	}
	obs(jobObs("/v1/jobs/e", 200, emptyJobBody(t)))
	obs(jobObs("/v1/jobs/n", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("an optional key present only live must not fail the gate: %s", tb.allText())
	}
	logs := strings.Join(tb.logs, "\n")
	for _, want := range []string{
		"note: optional per the Shape, presence not compared: provider_message_id (m2 GET /v1/jobs/{id} case=done)",
		"note: optional per the Shape, presence not compared: items[].note (m2 GET /v1/jobs/{id} case=done)",
	} {
		if strings.Count(logs, want) != 1 {
			t.Fatalf("%q must appear exactly once:\n%s", want, logs)
		}
	}
	// The paired control: an added key the Shape does not list still fails the gate.
	c := NewSession(sessionContracts(), fixtureFS(files), fxDir, nil)
	c.SetRecordMode(false)
	co := c.Observer()
	co(jobObs("/v1/jobs/a", 200, string(mutate(t, jobBody, func(m map[string]any) { m["brand_new"] = 1 }))))
	ctb := &fakeTB{}
	c.Finish(ctb)
	if !strings.Contains(strings.Join(ctb.errs, "\n"), "kind="+string(KeyAdded)) {
		t.Fatalf("a key the Shape does not list must still fail:\n%s", ctb.allText())
	}
}

func TestSessionRecordModeCountsNoDifferenceForAnOptionalKeyAndNotesIt(t *testing.T) {
	files := sessionFiles(t)
	files["job-done.json"] = withoutOptional(t)
	s := NewSession(sessionContracts(), fixtureFS(files), fxDir, nil)
	s.SetRecordMode(true)
	s.Observer()(jobObs("/v1/jobs/a", 200, jobBody))
	tb := &fakeTB{}
	s.Finish(tb)
	logs := strings.Join(tb.logs, "\n")
	if tb.failed() || !strings.Contains(logs, "case=done file=job-done.json: 0 differences, 0 gaps against the current fixture") || !strings.Contains(logs, "optional per the Shape, presence not compared: provider_message_id") {
		t.Fatalf("record mode: %s", tb.allText())
	}
}

func TestAssertLiveLogsAnOptionalKeyNoteAndDoesNotFail(t *testing.T) {
	tb := &fakeTB{}
	AssertLive(tb, jobContract(), fixtureFS(map[string]string{"job-done.json": withoutOptional(t)}), fxDir, 200, []byte(jobBody), newRec(false))
	if tb.failed() {
		t.Fatalf("must pass: %s", tb.allText())
	}
	if !strings.Contains(strings.Join(tb.logs, "\n"), "optional per the Shape, presence not compared: provider_message_id") {
		t.Fatalf("logs = %v", tb.logs)
	}
}
