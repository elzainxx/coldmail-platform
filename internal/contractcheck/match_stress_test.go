package contractcheck

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The stress test of the ranking: module 2's GET /v1/report answers 200 with bodies of completely different structure (a job report, a page of a campaign's items), and the
// harness's five contracts for that one module, route and status are a sent job, a queued job with null optional keys, a populated page, a last page and an empty campaign.

const reportRoute = "GET /v1/report"

func jobReport(id, state, provider string, attempts int, sentAt string, extra string) string {
	return fmt.Sprintf(`{"job_id":%q,"state":%q,"provider_message_id":%s,"attempts":%d,"error_class":null,"reason":null,"queued_at":"2026-01-01T00:00:00Z","sent_at":%s%s}`,
		id, state, provider, attempts, sentAt, extra)
}

func campaignReport(id string, results []string, cursor string, complete bool, extra string) string {
	return fmt.Sprintf(`{"campaign_id":%q,"totals":{"total":%d,"sent":%d,"failed":0},"items":{"results":[%s],"next_cursor":%s,"complete":%v%s}}`,
		id, len(results), len(results), strings.Join(results, ","), cursor, complete, extra)
}

func resultItem(id string) string {
	return fmt.Sprintf(`{"job_item_id":%q,"state":"sent","reason":null}`, id)
}

type reportKit struct {
	contracts []modclient.RouteContract
	files     map[string]string
	// observations by structure, with the case each must be assigned to when every fixture fits its own structure exactly.
	obs      []Observation
	wantCase map[string]string // body -> case
	// loose names the observations that only have to land among the contracts of their own kind of structure (a job report or a campaign page), because their own fixture is
	// stale in what it says is exercised.
	loose map[string]bool
}

// staleEmpty makes the empty-campaign fixture a populated page, as a hand-written fixture of an empty page can be: the live empty campaign then has a gap against it, and
// against every other campaign fixture, but none against the job fixtures, which differ in a dozen keys instead.
func (k *reportKit) staleEmpty() {
	k.files["report.campaign.empty.json"] = campaignReport("00000000-0000-4000-8000-000000000005", []string{resultItem("a"), resultItem("b")}, `null`, true, "")
	// Its structure is now that of the last page: the two compete for the last-page observation and for the empty one, and either assignment is among the campaign contracts.
	k.loose[string(k.obs[3].Body)] = true
	k.loose[string(k.obs[4].Body)] = true
}

// newReportKit builds the five contracts, their fixtures and one observation of each structure (with other ids and values than the fixtures). stale puts one key that the live
// body does not have into every fixture.
func newReportKit(stale bool) reportKit {
	extraFx := func(key string) string {
		if !stale {
			return ""
		}
		return fmt.Sprintf(`,%q:true`, key)
	}
	k := reportKit{files: map[string]string{}, wantCase: map[string]string{}, loose: map[string]bool{}}
	fixtures := []struct{ cas, file, body string }{
		{"job sent", "report.job.sent.json", jobReport("00000000-0000-4000-8000-000000000001", "sent", `"pm-1"`, 1, `"2026-01-01T00:00:00Z"`, extraFx("stale_job_sent"))},
		{"job queued", "report.job.queued.json", jobReport("00000000-0000-4000-8000-000000000002", "queued", `null`, 0, `null`, extraFx("stale_job_queued"))},
		{"campaign page", "report.campaign.page.json", campaignReport("00000000-0000-4000-8000-000000000003", []string{resultItem("a"), resultItem("b")}, `"c1"`, false, extraFx("stale_page"))},
		{"campaign last page", "report.campaign.last.json", campaignReport("00000000-0000-4000-8000-000000000004", []string{resultItem("c")}, `null`, true, extraFx("stale_last"))},
		{"campaign empty", "report.campaign.empty.json", campaignReport("00000000-0000-4000-8000-000000000005", nil, `null`, true, extraFx("stale_empty"))},
	}
	for _, f := range fixtures {
		k.contracts = append(k.contracts, modclient.RouteContract{Module: modclient.M2, Route: reportRoute, Case: f.cas, Fixture: f.file, Status: 200})
		k.files[f.file] = f.body
	}
	live := []struct{ cas, body string }{
		{"job sent", jobReport("00000000-0000-4000-8000-0000000000a1", "sent", `"pm-9"`, 2, `"2026-02-02T00:00:00Z"`, "")},
		{"job queued", jobReport("00000000-0000-4000-8000-0000000000a2", "queued", `null`, 0, `null`, "")},
		{"campaign page", campaignReport("00000000-0000-4000-8000-0000000000a3", []string{resultItem("x"), resultItem("y"), resultItem("z")}, `"c9"`, false, "")},
		{"campaign last page", campaignReport("00000000-0000-4000-8000-0000000000a4", []string{resultItem("w")}, `null`, true, "")},
		{"campaign empty", campaignReport("00000000-0000-4000-8000-0000000000a5", nil, `null`, true, "")},
	}
	for _, l := range live {
		k.obs = append(k.obs, Observation{Module: modclient.M2, Route: "m2.report", Method: "GET", Path: "/v1/report", Status: 200, Body: []byte(l.body)})
		k.wantCase[l.body] = l.cas
	}
	return k
}

func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := append(append(append([]int(nil), p[:i]...), n-1), p[i:]...)
			out = append(out, q)
		}
	}
	return out
}

func family(cas string) string { return strings.Fields(cas)[0] }

func TestMatchEachObservationOfFiveStructuresGoesToTheContractOfItsOwnStructureWhateverTheOrders(t *testing.T) {
	for _, variant := range []string{"exact", "one stale key in every fixture", "an empty campaign whose fixture is a populated page"} {
		stale := variant == "one stale key in every fixture"
		k := newReportKit(stale)
		if strings.HasPrefix(variant, "an empty campaign") {
			k.staleEmpty()
		}
		fsys := fixtureFS(k.files)
		rng := rand.New(rand.NewSource(11))
		runs := 0
		for _, perm := range permutations(len(k.contracts)) {
			contracts := make([]modclient.RouteContract, len(perm))
			for i, j := range perm {
				contracts[i] = k.contracts[j]
			}
			for shuffle := 0; shuffle < 3; shuffle++ {
				obs := append([]Observation(nil), k.obs...)
				rng.Shuffle(len(obs), func(a, b int) { obs[a], obs[b] = obs[b], obs[a] })
				res := Match(contracts, fsys, fxDir, obs)
				runs++
				if len(res.Matched) != 5 || len(res.Undeclared) != 0 || len(res.Unexercised) != 0 {
					t.Fatalf("%s: matched %d undeclared %d unexercised %d", variant, len(res.Matched), len(res.Undeclared), len(res.Unexercised))
				}
				for _, m := range res.Matched {
					body := string(m.Observation.Body)
					want := k.wantCase[body]
					if k.loose[body] {
						if family(m.Contract.Case) != family(want) {
							t.Fatalf("%s: an observation of the %q structure was assigned to %q (contract order %v)", variant, want, m.Contract.Case, perm)
						}
						continue
					}
					if m.Contract.Case != want {
						t.Fatalf("%s: an observation of the %q structure was assigned to %q (contract order %v)", variant, want, m.Contract.Case, perm)
					}
					wantDiffs := 0
					if stale {
						wantDiffs = 1 // the one stale key of its own fixture
					}
					if len(m.Report.Diffs) != wantDiffs {
						t.Fatalf("%s: %q has %d differences, want %d:\n%s", variant, m.Contract.Case, len(m.Report.Diffs), wantDiffs, detailOf(m.Report))
					}
				}
			}
		}
		if runs != 5*4*3*2*3 {
			t.Fatalf("ran %d times, want %d: the instrument must try every contract order", runs, 5*4*3*2*3)
		}
	}
}

func TestMatchAStalePopulatedFixtureForAnEmptyCampaignDoesNotPullAJobOrAPageOverToIt(t *testing.T) {
	// The recorded fixture of the empty-campaign contract is a populated page (it was written by hand), and the live empty campaign must still land among the campaign contracts,
	// never on the job contracts, whatever order the contracts come in.
	k := newReportKit(false)
	k.files["report.campaign.empty.json"] = campaignReport("00000000-0000-4000-8000-000000000005", []string{resultItem("a"), resultItem("b")}, `"c1"`, false, "")
	emptyLive := string(k.obs[4].Body)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 60; i++ {
		contracts := append([]modclient.RouteContract(nil), k.contracts...)
		rng.Shuffle(len(contracts), func(a, b int) { contracts[a], contracts[b] = contracts[b], contracts[a] })
		res := Match(contracts, fixtureFS(k.files), fxDir, []Observation{k.obs[4]})
		if len(res.Matched) != 1 || family(res.Matched[0].Contract.Case) != "campaign" || string(res.Matched[0].Observation.Body) != emptyLive {
			t.Fatalf("an empty campaign was assigned to %q", res.Matched[0].Contract.Case)
		}
	}
	// The paired control: the structure that is NOT a campaign never takes a campaign page, and a job never takes a campaign contract.
	res := Match(k.contracts, fixtureFS(k.files), fxDir, []Observation{k.obs[0], k.obs[1]})
	for _, m := range res.Matched {
		if family(m.Contract.Case) != "job" {
			t.Fatalf("a job report was assigned to %q", m.Contract.Case)
		}
	}
}

func TestMatchTwoFixturesOfOneStructureShareTheObservationsOfThatStructure(t *testing.T) {
	k := newReportKit(false)
	twin := k.contracts[2]
	twin.Case, twin.Fixture = "campaign page twin", "report.campaign.page.twin.json"
	k.files[twin.Fixture] = k.files[k.contracts[2].Fixture]
	contracts := append(append([]modclient.RouteContract(nil), k.contracts...), twin)
	second := Observation{Module: modclient.M2, Route: "m2.report", Method: "GET", Path: "/v1/report", Status: 200,
		Body: []byte(campaignReport("00000000-0000-4000-8000-0000000000b3", []string{resultItem("p"), resultItem("q")}, `"c8"`, false, ""))}
	obs := append(append([]Observation(nil), k.obs...), second)
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 60; i++ {
		perm := append([]modclient.RouteContract(nil), contracts...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		shuffled := append([]Observation(nil), obs...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		res := Match(perm, fixtureFS(k.files), fxDir, shuffled)
		if len(res.Matched) != 6 || len(res.Unexercised) != 0 {
			t.Fatalf("matched %d unexercised %d", len(res.Matched), len(res.Unexercised))
		}
		pages := map[string]bool{}
		for _, m := range res.Matched {
			isPage := strings.Contains(string(m.Observation.Body), `"next_cursor":"c`)
			inPageFamily := strings.HasPrefix(m.Contract.Case, "campaign page")
			if isPage != inPageFamily {
				t.Fatalf("an observation with page=%v was assigned to %q", isPage, m.Contract.Case)
			}
			if isPage {
				pages[m.Contract.Case] = true
			}
		}
		if len(pages) != 2 {
			t.Fatalf("the two page observations must use the two page contracts, used %v", pages)
		}
	}
}

func TestMatchTheContractThatLosesKeepsANearestMissWhenTwoOfOneStructureCompete(t *testing.T) {
	k := newReportKit(false)
	twin := k.contracts[2]
	twin.Case, twin.Fixture = "campaign page twin", "report.campaign.page.twin.json"
	k.files[twin.Fixture] = k.files[k.contracts[2].Fixture]
	contracts := append(append([]modclient.RouteContract(nil), k.contracts...), twin)
	res := Match(contracts, fixtureFS(k.files), fxDir, k.obs) // one page observation for two page contracts
	if len(res.Unexercised) != 1 || res.Unexercised[0].Case != "campaign page twin" {
		t.Fatalf("unexercised = %+v", res.Unexercised)
	}
	if len(res.NearestMisses) != 1 || len(res.NearestMisses[0].Misses) != 0 || res.NearestMisses[0].Observed != 5 {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
	if line := res.NearestMisses[0].String(); !strings.Contains(line, "case=campaign page twin: 0 differences against the closest of 5 observed responses: none") {
		t.Fatalf("the losing twin must say that the page fitted it as well as the other: %s", line)
	}
	// And a contract of another structure that loses shows the keys of the closest response of its own kind of miss.
	k2 := newReportKit(false)
	res = Match(k2.contracts, fixtureFS(k2.files), fxDir, []Observation{k2.obs[0], k2.obs[2], k2.obs[3], k2.obs[4]}) // no queued job observed
	if len(res.NearestMisses) != 1 || res.NearestMisses[0].Contract.Case != "job queued" || res.NearestMisses[0].Observed != 4 {
		t.Fatalf("nearest misses = %+v", res.NearestMisses)
	}
	var paths []string
	for _, m := range res.NearestMisses[0].Misses {
		paths = append(paths, m.Path)
	}
	if !strings.Contains(strings.Join(paths, " "), "provider_message_id") || !strings.Contains(strings.Join(paths, " "), "sent_at") {
		t.Fatalf("the closest response to the queued-job fixture is the sent job, and the miss must name where they differ, got %v", paths)
	}
}

func TestMatchRankIsDistanceThenFewerGaps(t *testing.T) {
	// A pair at distance 1 by a gap equals a pair at distance 1 by a difference, and then the one with fewer gaps wins; a pair at distance 2 loses to either.
	byGap, byDiff, far := rank{total: 1, oneSided: 1}, rank{total: 1, oneSided: 0}, rank{total: 2, oneSided: 0}
	if !byDiff.less(byGap) || byGap.less(byDiff) || !byGap.less(far) || !byDiff.less(far) || far.less(byGap) {
		t.Fatal("rank is not ordered by distance, then by fewer gaps")
	}
	if got := newRank(Report{Diffs: []Diff{{}, {}}, Gaps: []Gap{{Side: "fixture", Path: "a"}, {Side: "fixture", Path: "b"}, {Side: "live", Path: "b"}}}); got != (rank{total: 3, oneSided: 1}) {
		t.Fatalf("rank = %+v: two differences and one one-sided gap are a distance of three (a position that is a gap on both sides is not a gap)", got)
	}
}
