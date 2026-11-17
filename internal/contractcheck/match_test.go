package contractcheck

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

var errorEnvelopeShape = modclient.Object(modclient.Req("error", modclient.Object(modclient.Req("code", modclient.Str), modclient.Req("message", modclient.Str), modclient.Opt("request_id", modclient.Str))))

const notFoundBody = `{"error":{"code":"not_found","message":"The requested resource was not found.","request_id":"req_00000000000000000000000000000000"}}`

func emptyJobBody(t *testing.T) string {
	t.Helper()
	return string(mutate(t, jobBody, func(m map[string]any) { m["items"] = []any{} }))
}

func jobEmptyContract() modclient.RouteContract {
	rc := jobContract()
	rc.Case, rc.Fixture = "empty", "job-empty.json"
	return rc
}

func jobNotFoundContract() modclient.RouteContract {
	return modclient.RouteContract{Module: modclient.M2, Route: "GET /v1/jobs/{id}", Case: "not_found", Fixture: "job-not-found.json", Status: 404, Shape: errorEnvelopeShape,
		Vocab: map[string][]string{"error.code": {"not_found", "validation_failed"}}}
}

func healthContract() modclient.RouteContract {
	return modclient.RouteContract{Module: modclient.M2, Route: "GET /healthz", Fixture: "health.json", Status: 200, Shape: modclient.Object(modclient.Req("status", modclient.Str))}
}

func sessionContracts() []modclient.RouteContract {
	return []modclient.RouteContract{jobContract(), jobEmptyContract(), jobNotFoundContract(), healthContract()}
}

func sessionFiles(t *testing.T) map[string]string {
	return map[string]string{"job-done.json": jobBody, "job-empty.json": emptyJobBody(t), "job-not-found.json": notFoundBody, "health.json": `{"status":"ok"}`}
}

func ob(module modclient.Module, label, method, path string, status int, body string) Observation {
	return Observation{Module: module, Route: label, Method: method, Path: path, Status: status, Body: []byte(body)}
}

func jobObs(path string, status int, body string) Observation {
	return ob(modclient.M2, "m2.jobs", "GET", path, status, body)
}

func TestMatchAssignsEachObservationToTheContractOfItsRouteAndStatus(t *testing.T) {
	obs := []Observation{
		jobObs("/v1/jobs/abc123", 200, jobBody),
		jobObs("/v1/jobs/abc123", 404, notFoundBody),
		ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`),
	}
	res := Match(sessionContracts(), fixtureFS(sessionFiles(t)), fxDir, obs)
	if len(res.Matched) != 3 || len(res.Undeclared) != 0 {
		t.Fatalf("matched %d, undeclared %d", len(res.Matched), len(res.Undeclared))
	}
	got := map[string]bool{}
	for _, m := range res.Matched {
		if !m.Report.Clean() {
			t.Fatalf("a response equal to its fixture differs:\n%s", detailOf(m.Report))
		}
		got[m.Contract.Case+"|"+m.Contract.Route] = true
	}
	for _, want := range []string{"done|GET /v1/jobs/{id}", "not_found|GET /v1/jobs/{id}", "|GET /healthz"} {
		if !got[want] {
			t.Fatalf("no observation was assigned to %s: %v", want, got)
		}
	}
	// "empty" had no observation: it is the one contract left over.
	if len(res.Unexercised) != 1 || res.Unexercised[0].Case != "empty" {
		t.Fatalf("unexercised = %+v", res.Unexercised)
	}
}

func TestMatchATemplateSegmentStandsForExactlyOneNonEmptySegmentAndTheMethodMustAgree(t *testing.T) {
	contracts := []modclient.RouteContract{jobContract()}
	files := map[string]string{"job-done.json": jobBody}
	fits := func(method, path string) bool {
		res := Match(contracts, fixtureFS(files), fxDir, []Observation{jobObs(path, 200, jobBody)})
		if method != "GET" {
			res = Match(contracts, fixtureFS(files), fxDir, []Observation{ob(modclient.M2, "m2.jobs", method, path, 200, jobBody)})
		}
		return len(res.Matched) == 1
	}
	if !fits("GET", "/v1/jobs/abc") || !fits("GET", "/v1/jobs/a%2Fb") {
		t.Fatal("a template segment must match one segment, escaped characters included")
	}
	for _, c := range []struct{ method, path string }{{"GET", "/v1/jobs/"}, {"GET", "/v1/jobs"}, {"GET", "/v1/jobs/a/b"}, {"POST", "/v1/jobs/abc"}, {"GET", "/v2/jobs/abc"}, {"GET", ""}} {
		if fits(c.method, c.path) {
			t.Fatalf("%s %q must not fit the template", c.method, c.path)
		}
	}
}

func TestMatchAMutatedLiveBodySurfacesAsTheExactKindOnTheExactPathThroughMatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   []string
	}{
		{"a required key removed", func(m map[string]any) { delete(m["counts"].(map[string]any), "done") },
			[]string{string(KeyMissing) + ":counts.done", string(ShapeViolation) + ":counts.done"}},
		{"a key added", func(m map[string]any) { m["extra"] = 1 }, []string{string(KeyAdded) + ":extra"}},
		{"a type changed", func(m map[string]any) { m["created_at"] = 5 }, []string{string(TypeChanged) + ":created_at"}},
		{"a vocabulary value added", func(m map[string]any) { m["state"] = "paused" }, []string{string(VocabNew) + ":state"}},
		{"nothing changed", func(m map[string]any) {}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live := mutate(t, jobBody, c.mutate)
			res := Match([]modclient.RouteContract{jobContract()}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{jobObs("/v1/jobs/abc", 200, string(live))})
			if len(res.Matched) != 1 {
				t.Fatalf("matched %d", len(res.Matched))
			}
			wantDiffs(t, res.Matched[0].Report, c.want...)
		})
	}
}

func TestMatchAChangedStatusIsAnUndeclaredStatusAndAnUnexercisedContract(t *testing.T) {
	res := Match([]modclient.RouteContract{jobContract()}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{jobObs("/v1/jobs/abc", 409, notFoundBody)})
	if len(res.Matched) != 0 || len(res.Undeclared) != 1 || len(res.Unexercised) != 1 {
		t.Fatalf("matched %d, undeclared %d, unexercised %d", len(res.Matched), len(res.Undeclared), len(res.Unexercised))
	}
	if u := res.Undeclared[0]; u.Status != 409 || !u.RouteDeclared || u.Module != modclient.M2 || u.Route != "m2.jobs" {
		t.Fatalf("undeclared = %+v", u)
	}
	// The paired control: the declared status is matched.
	res = Match([]modclient.RouteContract{jobContract()}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{jobObs("/v1/jobs/abc", 200, jobBody)})
	if len(res.Undeclared) != 0 || len(res.Unexercised) != 0 {
		t.Fatal("the declared status must be matched")
	}
}

func TestMatchPicksTheEmptyPageContractForAnEmptyPageAndThePageContractForAFullOne(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	contracts := []modclient.RouteContract{jobContract(), jobEmptyContract()}
	res := Match(contracts, files, fxDir, []Observation{jobObs("/v1/jobs/a", 200, emptyJobBody(t)), jobObs("/v1/jobs/b", 200, jobBody)})
	if len(res.Matched) != 2 || len(res.Unexercised) != 0 {
		t.Fatalf("matched %d, unexercised %d: both cases must be exercised", len(res.Matched), len(res.Unexercised))
	}
	for _, m := range res.Matched {
		empty := strings.Contains(string(m.Observation.Body), `"items":[]`)
		if empty != (m.Contract.Case == "empty") {
			t.Fatalf("an observation with empty=%v was assigned to case %q", empty, m.Contract.Case)
		}
		if len(m.Tied) != 0 || !m.Report.Clean() {
			t.Fatalf("a fitting contract must be unique and clean: tied %v, diffs:\n%s", m.Tied, detailOf(m.Report))
		}
	}
}

func TestMatchBreaksATieByContractOrderPrefersAContractNothingWasAssignedToAndReportsIt(t *testing.T) {
	a, b := jobContract(), jobContract()
	a.Case, a.Fixture = "a", "a.json"
	b.Case, b.Fixture = "b", "b.json"
	files := fixtureFS(map[string]string{"a.json": jobBody, "b.json": jobBody})
	first := jobObs("/v1/jobs/1", 200, jobBody)
	second := jobObs("/v1/jobs/2", 200, string(mutate(t, jobBody, func(m map[string]any) { m["state"] = "running" })))
	res := Match([]modclient.RouteContract{a, b}, files, fxDir, []Observation{first, second})
	if len(res.Matched) != 2 || len(res.Unexercised) != 0 {
		t.Fatalf("matched %d unexercised %d", len(res.Matched), len(res.Unexercised))
	}
	cases := []string{res.Matched[0].Contract.Case, res.Matched[1].Contract.Case}
	if !reflect.DeepEqual(cases, []string{"a", "b"}) {
		t.Fatalf("the two observations must go to a then b, got %v", cases)
	}
	if !reflect.DeepEqual(res.Matched[0].Tied, []string{"b"}) {
		t.Fatalf("the tie must be reported, got %v", res.Matched[0].Tied)
	}
	// The paired control: one observation alone goes to the first by order.
	res = Match([]modclient.RouteContract{a, b}, files, fxDir, []Observation{first})
	if res.Matched[0].Contract.Case != "a" || len(res.Unexercised) != 1 || res.Unexercised[0].Case != "b" {
		t.Fatalf("a lone observation goes to the first contract: %+v", res.Matched[0].Contract.Case)
	}
}

func TestMatchUndeclaredCallsCarryNoBodyAndNoRealId(t *testing.T) {
	marker := "MARK" + token(t, 8)
	body := `{"note":"` + marker + `"}`
	res := Match(sessionContracts(), fixtureFS(sessionFiles(t)), fxDir, []Observation{
		jobObs("/v1/jobs/"+marker+"/secretaction", 200, body),
		ob(modclient.M3, "m3.thing", "POST", "/v9/unknown", 201, body),
	})
	if len(res.Undeclared) != 2 {
		t.Fatalf("undeclared = %+v", res.Undeclared)
	}
	for _, u := range res.Undeclared {
		text := u.String() + " " + u.Path + " " + u.Route
		if strings.Contains(text, marker) {
			t.Fatalf("an undeclared call carries a value of the observation: %s", text)
		}
	}
	for i := 0; i < reflect.TypeOf(UndeclaredCall{}).NumField(); i++ {
		if reflect.TypeOf(UndeclaredCall{}).Field(i).Type.Kind() == reflect.Slice {
			t.Fatalf("UndeclaredCall.%s is a slice: it must not be able to hold a body", reflect.TypeOf(UndeclaredCall{}).Field(i).Name)
		}
	}
	if res.Undeclared[0].Path != "/v1/jobs/{}/{}" || res.Undeclared[0].RouteDeclared {
		t.Fatalf("the path must be redacted to what contracts name: %+v", res.Undeclared[0])
	}
	if got := res.Undeclared[1].Template(); got != "POST /{}/{}" {
		t.Fatalf("a module with no contract names no literal: %q", got)
	}
}

func TestMatchGroupsRepeatedUndeclaredCallsAndCountsThem(t *testing.T) {
	obs := []Observation{
		ob(modclient.M2, "m2.x", "GET", "/v1/other", 200, `{"a":1}`),
		ob(modclient.M2, "m2.x", "GET", "/v1/other", 200, `{"a":2}`),
		ob(modclient.M2, "m2.x", "GET", "/v1/other", 500, `{"a":3}`),
	}
	res := Match(sessionContracts(), fixtureFS(sessionFiles(t)), fxDir, obs)
	if len(res.Undeclared) != 2 || res.Undeclared[0].Count+res.Undeclared[1].Count != 3 {
		t.Fatalf("undeclared = %+v", res.Undeclared)
	}
}

func TestMatchUnexercisedListsEveryContractNothingWasAssignedToInContractOrder(t *testing.T) {
	res := Match(sessionContracts(), fixtureFS(sessionFiles(t)), fxDir, nil)
	var cases []string
	for _, rc := range res.Unexercised {
		cases = append(cases, rc.Case)
	}
	if !reflect.DeepEqual(cases, []string{"done", "empty", "not_found", ""}) {
		t.Fatalf("unexercised = %v", cases)
	}
	if res.Contracts != 4 || res.Observations != 0 {
		t.Fatalf("sizes = %d, %d", res.Contracts, res.Observations)
	}
}

func TestMatchWithNoContractsLeavesEveryObservationUndeclared(t *testing.T) {
	res := Match(nil, fixtureFS(nil), fxDir, []Observation{jobObs("/v1/jobs/a", 200, jobBody)})
	if len(res.Matched) != 0 || len(res.Undeclared) != 1 || len(res.Unexercised) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestMatchDoesNotDependOnTheOrderTheObservationsArriveIn(t *testing.T) {
	files := fixtureFS(sessionFiles(t))
	obs := []Observation{
		jobObs("/v1/jobs/a", 200, jobBody),
		jobObs("/v1/jobs/b", 200, emptyJobBody(t)),
		jobObs("/v1/jobs/c", 404, notFoundBody),
		jobObs("/v1/jobs/d", 200, string(mutate(t, jobBody, func(m map[string]any) { m["state"] = "paused"; m["extra"] = 1 }))),
		ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`),
		ob(modclient.M2, "m2.x", "GET", "/v1/other", 200, `{"a":1}`),
		ob(modclient.M2, "m2.x", "GET", "/v1/other", 200, `{"a":2}`),
		ob(modclient.M1, "m1.y", "POST", "/v1/y", 202, `{}`),
	}
	want := Match(sessionContracts(), files, fxDir, obs)
	if len(want.Matched) != 5 || len(want.Undeclared) != 2 {
		t.Fatalf("the reference result is not what the test assumes: %d matched, %d undeclared", len(want.Matched), len(want.Undeclared))
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		perm := append([]Observation(nil), obs...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		if got := Match(sessionContracts(), files, fxDir, perm); !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d changed the result:\n%+v\nvs\n%+v", i, got, want)
		}
	}
	// The paired control: a different set of observations is a different result.
	if reflect.DeepEqual(Match(sessionContracts(), files, fxDir, obs[:4]), want) {
		t.Fatal("the comparison cannot tell two results apart")
	}
}

func TestMatchAMissingFixtureIsADifferenceInWordsNotAnEmptyDocument(t *testing.T) {
	res := Match([]modclient.RouteContract{jobContract()}, fixtureFS(nil), fxDir, []Observation{jobObs("/v1/jobs/a", 200, jobBody)})
	if len(res.Matched) != 1 {
		t.Fatalf("matched %d", len(res.Matched))
	}
	invalid := res.Matched[0].Report.Of(ContractInvalid)
	if len(invalid) != 1 || !strings.Contains(invalid[0].Detail, EnvRecord+"=1") || !strings.Contains(invalid[0].Detail, "does not exist") {
		t.Fatalf("diffs:\n%s", detailOf(res.Matched[0].Report))
	}
}

func TestMatchAContractMayNameTheRouteLabelInsteadOfAMethodAndTemplate(t *testing.T) {
	rc := jobContract()
	rc.Route = "m2.jobs"
	res := Match([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{{Module: modclient.M2, Route: "m2.jobs", Status: 200, Body: []byte(jobBody)}})
	if len(res.Matched) != 1 {
		t.Fatalf("a contract that names the label must match an observation with that label: %+v", res)
	}
	other := Match([]modclient.RouteContract{rc}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{{Module: modclient.M2, Route: "m2.other", Status: 200, Body: []byte(jobBody)}})
	if len(other.Matched) != 0 {
		t.Fatal("another label must not match")
	}
}

func TestMatchTheSameRouteOfAnotherModuleIsNotTheSameContract(t *testing.T) {
	res := Match([]modclient.RouteContract{healthContract()}, fixtureFS(map[string]string{"health.json": `{"status":"ok"}`}), fxDir,
		[]Observation{ob(modclient.M1, "m1.healthz", "GET", "/healthz", 200, `{"status":"ok"}`)})
	if len(res.Matched) != 0 || len(res.Undeclared) != 1 || len(res.Unexercised) != 1 {
		t.Fatalf("matched %d undeclared %d unexercised %d", len(res.Matched), len(res.Undeclared), len(res.Unexercised))
	}
}

func TestMatchATruncatedBodyIsAnUnreadableResponse(t *testing.T) {
	o := jobObs("/v1/jobs/a", 200, jobBody[:40])
	o.Truncated = true
	res := Match([]modclient.RouteContract{jobContract()}, fixtureFS(map[string]string{"job-done.json": jobBody}), fxDir, []Observation{o})
	if len(res.Matched) != 1 {
		t.Fatalf("matched %d", len(res.Matched))
	}
	wantDiffs(t, res.Matched[0].Report, string(ShapeViolation)+":(body)")
}
