package contractcheck

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Nearest misses, and an empty fixture never taking the observations of a populated one.

func pageWithExtraKey(t *testing.T) string {
	t.Helper()
	return string(mutate(t, jobBody, func(m map[string]any) {
		m["items"].([]any)[0].(map[string]any)["extra"] = true
	}))
}

func TestMatchAnEmptyFixtureNeverTakesAPopulatedPageEvenWhenItWouldHaveFewerDifferences(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	contracts := []modclient.RouteContract{jobContract(), jobEmptyContract()}
	res := Match(contracts, files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, pageWithExtraKey(t))})
	if len(res.Matched) != 1 || res.Matched[0].Contract.Case != "done" {
		t.Fatalf("the populated page must go to the populated contract, got %+v", res.Matched)
	}
	// Its real difference is visible, with the exact kind on the exact path ...
	wantDiffs(t, res.Matched[0].Report, string(KeyAdded)+":items[].extra")
	// ... and the empty-page contract is left unexercised, with a nearest miss that says why.
	if len(res.Unexercised) != 1 || res.Unexercised[0].Case != "empty" {
		t.Fatalf("unexercised = %+v", res.Unexercised)
	}
	if len(res.NearestMisses) != 1 || res.NearestMisses[0].Observed != 1 || !reflect.DeepEqual(res.NearestMisses[0].Misses, []Miss{{Path: "items", Kind: "GapFixture"}}) {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
	// The paired control: with no other contract the empty-page contract still takes what is left (it is the only one that can).
	only := Match([]modclient.RouteContract{jobEmptyContract()}, files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, pageWithExtraKey(t))})
	if len(only.Matched) != 1 || len(only.Unexercised) != 0 {
		t.Fatalf("a lone empty-page contract must not lose its only observation: %+v", only)
	}
}

func TestMatchTheEmptyPageContractIsMatchedOnlyByAnEmptyLivePage(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	contracts := []modclient.RouteContract{jobContract(), jobEmptyContract()}
	res := Match(contracts, files, fxDir, []Observation{
		jobObs("/v1/jobs/a", 200, pageWithExtraKey(t)),
		jobObs("/v1/jobs/b", 200, emptyJobBody(t)),
	})
	if len(res.Matched) != 2 || len(res.Unexercised) != 0 || len(res.NearestMisses) != 0 {
		t.Fatalf("matched %d unexercised %d nearest %d", len(res.Matched), len(res.Unexercised), len(res.NearestMisses))
	}
	for _, m := range res.Matched {
		empty := strings.Contains(string(m.Observation.Body), `"items":[]`)
		if empty != (m.Contract.Case == "empty") {
			t.Fatalf("an observation with empty=%v went to %q", empty, m.Contract.Case)
		}
	}
}

func TestMatchAnEmptyLivePageGoesToTheEmptyPageContractEvenWhenThatContractDiffersAndThePopulatedOneIsClean(t *testing.T) {
	// The mirror image: the real difference of an empty-page contract must not hide behind a populated contract that fits an empty page with no difference.
	files := fixtureFS(sessionFiles(t))
	contracts := []modclient.RouteContract{jobContract(), jobEmptyContract()}
	live := string(mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{}; m["added"] = 1 }))
	res := Match(contracts, files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, live)})
	if len(res.Matched) != 1 || res.Matched[0].Contract.Case != "empty" {
		t.Fatalf("an empty page belongs to the empty-page contract: %+v", res.Matched)
	}
	wantDiffs(t, res.Matched[0].Report, string(KeyAdded)+":added")
	if len(res.NearestMisses) != 1 || res.NearestMisses[0].Contract.Case != "done" || !reflect.DeepEqual(res.NearestMisses[0].Misses, []Miss{{Path: "added", Kind: string(KeyAdded)}, {Path: "items", Kind: "GapLive"}}) {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
}

func TestMatchAContractThatLostEveryObservationToABetterSiblingGetsANearestMissNamingTheKeys(t *testing.T) {
	a := jobContract()
	a.Case, a.Fixture = "a", "a.json"
	b := jobContract()
	b.Case, b.Fixture = "b", "b.json"
	files := fixtureFS(map[string]string{"a.json": jobBody, "b.json": pageWithExtraKey(t)})
	res := Match([]modclient.RouteContract{a, b}, files, fxDir, []Observation{
		jobObs("/v1/jobs/1", 200, pageWithExtraKey(t)),
		jobObs("/v1/jobs/2", 200, string(mutate(t, pageWithExtraKey(t), func(m map[string]any) { m["state"] = "running" }))),
	})
	for _, m := range res.Matched {
		if m.Contract.Case != "b" {
			t.Fatalf("both observations fit b exactly, got %q", m.Contract.Case)
		}
	}
	if len(res.Unexercised) != 1 || res.Unexercised[0].Case != "a" || len(res.NearestMisses) != 1 {
		t.Fatalf("unexercised %+v nearest %+v", res.Unexercised, res.NearestMisses)
	}
	nm := res.NearestMisses[0]
	if nm.Observed != 2 || !reflect.DeepEqual(nm.Misses, []Miss{{Path: "items[].extra", Kind: string(KeyAdded)}}) {
		t.Fatalf("nearest miss = %+v", nm)
	}
	want := "nearest miss: m2 GET /v1/jobs/{id} case=a: 1 differences against the closest of 2 observed responses: items[].extra " + string(KeyAdded)
	if nm.String() != want {
		t.Fatalf("line:\n%s\nwant:\n%s", nm, want)
	}
}

func TestMatchNearestMissNeedsObservedResponsesOfTheSameModuleRouteAndStatus(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	obs := []Observation{
		jobObs("/v1/jobs/a", 409, notFoundBody),                                   // the route, but not a status any contract declares
		ob(modclient.M1, "m1.healthz", "GET", "/healthz", 200, `{"status":"ok"}`), // the same route of another module
	}
	res := Match(sessionContracts(), files, fxDir, obs)
	if len(res.Unexercised) != 4 || len(res.NearestMisses) != 0 {
		t.Fatalf("unexercised %d, nearest misses %+v: a contract nothing of its module, route and status was observed for was never called, not nearly missed", len(res.Unexercised), res.NearestMisses)
	}
	// The paired control: a response of the same module, route and status makes the nearest miss appear.
	res = Match(sessionContracts(), files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, pageWithExtraKey(t))})
	if len(res.NearestMisses) != 1 || res.NearestMisses[0].Contract.Case != "empty" {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
}

func TestMatchNearestMissesDoNotDependOnTheOrderOfObservations(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	obs := []Observation{
		jobObs("/v1/jobs/a", 200, pageWithExtraKey(t)),
		jobObs("/v1/jobs/b", 200, string(mutate(t, jobBody, func(m map[string]any) { m["added"] = 1 }))),
		jobObs("/v1/jobs/c", 404, string(mutate(t, notFoundBody, func(m map[string]any) { m["error"].(map[string]any)["code"] = "gone" }))),
	}
	want := Match(sessionContracts(), files, fxDir, obs)
	if len(want.NearestMisses) != 1 || want.NearestMisses[0].Contract.Case != "empty" {
		t.Fatalf("the reference result is not what the test assumes: %+v", want.NearestMisses)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 30; i++ {
		perm := append([]Observation(nil), obs...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		if got := Match(sessionContracts(), files, fxDir, perm); !reflect.DeepEqual(got.NearestMisses, want.NearestMisses) || !reflect.DeepEqual(got.Unexercised, want.Unexercised) {
			t.Fatalf("permutation %d changed the nearest misses", i)
		}
	}
}

func TestMissTrackKeepsTheClosestCandidateTheFirstOfEqualOnesAndCountsAll(t *testing.T) {
	rep := func(n int) Report {
		r := Report{}
		for i := 0; i < n; i++ {
			r.Diffs = append(r.Diffs, Diff{Path: fmt.Sprintf("p%d", i), Kind: KeyAdded})
		}
		return r
	}
	var tr missTrack
	for _, n := range []int{3, 1, 1, 2} {
		r := rep(n)
		tr.offer(scored{rep: r, rk: newRank(r)})
	}
	if tr.seen != 4 || len(tr.rep.Diffs) != 1 || tr.rep.Diffs[0].Path != "p0" {
		t.Fatalf("track = %+v", tr)
	}
	// One-sided gaps count as misses too: a report with no differences but a one-sided gap is farther than a clean one.
	var tr2 missTrack
	gap := Report{Gaps: []Gap{{Side: "fixture", Path: "items"}}}
	tr2.offer(scored{rep: gap, rk: newRank(gap)})
	tr2.offer(scored{rep: Report{}, rk: newRank(Report{})})
	if len(missesOf(tr2.rep)) != 0 {
		t.Fatalf("the clean candidate is closer: %+v", missesOf(tr2.rep))
	}
}

func TestNearestMissLineCapsTheListAtEightAndSaysHowManyMore(t *testing.T) {
	mk := func(n int) NearestMiss {
		nm := NearestMiss{Contract: modclient.RouteContract{Module: modclient.M2, Route: "GET /v1/report", Case: "job queued"}, Observed: 12}
		for i := 0; i < n; i++ {
			nm.Misses = append(nm.Misses, Miss{Path: fmt.Sprintf("p%d", i), Kind: string(TypeChanged)})
		}
		return nm
	}
	line := mk(11).String()
	if !strings.HasPrefix(line, "nearest miss: m2 GET /v1/report case=job queued: 11 differences against the closest of 12 observed responses: p0 TypeChanged, p1 TypeChanged") ||
		!strings.Contains(line, "p7 TypeChanged, and 3 more") || strings.Contains(line, "p8") {
		t.Fatalf("line = %s", line)
	}
	if exact := mk(MaxMissesListed).String(); strings.Contains(exact, "more") || !strings.Contains(exact, "p7 TypeChanged") {
		t.Fatalf("exactly the cap lists everything: %s", exact)
	}
	if none := mk(0).String(); !strings.Contains(none, "0 differences against the closest of 12 observed responses: none") {
		t.Fatalf("line = %s", none)
	}
	noCase := NearestMiss{Contract: modclient.RouteContract{Module: modclient.M1, Route: "GET /healthz"}, Observed: 1, Misses: []Miss{{Path: "a", Kind: string(KeyMissing)}}}
	if strings.Contains(noCase.String(), "case=") {
		t.Fatalf("an empty case is not printed: %s", noCase)
	}
}

func TestMatchNearestMissCarriesNoValueOfAnObservation(t *testing.T) {
	marker := "MARK" + token(t, 8)
	files := fixtureFS(sessionFiles(t))
	live := string(mutate(t, jobBody, func(m map[string]any) {
		m["created_at"] = marker + "@corp.com"
		m["counts"].(map[string]any)["total"] = marker
		m["state"] = marker
		m["items"].([]any)[0].(map[string]any)["extra"] = marker
	}))
	res := Match(sessionContracts(), files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, live)})
	if len(res.NearestMisses) == 0 {
		t.Fatal("the test needs a nearest miss")
	}
	for _, nm := range res.NearestMisses {
		if strings.Contains(nm.String(), marker) {
			t.Fatalf("a nearest miss carries a value: %s", nm)
		}
	}
	// The paired control: the same observation has real differences, so the instrument has something it could leak.
	if m := res.Matched[0].Report; len(m.Diffs) == 0 || !strings.Contains(detailOf(m), "path") {
		t.Fatalf("the matched report must hold differences: %s", detailOf(m))
	}
}
