package contractcheck

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

func coverageContracts() []modclient.RouteContract {
	return []modclient.RouteContract{
		{Module: modclient.M1, Route: "GET /healthz", Fixture: "a.json"},
		{Module: modclient.M2, Route: "GET /v1/jobs/{id}", Case: "done", Fixture: "b.json"},
		{Module: modclient.M2, Route: "GET /v1/jobs/{id}", Case: "failed", Fixture: "c.json"},
	}
}

func exercisedAll() []Exercised {
	return []Exercised{
		{modclient.M1, "GET /healthz", ""},
		{modclient.M2, "GET /v1/jobs/{id}", "done"},
		{modclient.M2, "GET /v1/jobs/{id}", "failed"},
	}
}

func TestCoverageIsOKOnlyWhenEveryContractWasExercisedAndNothingElseWas(t *testing.T) {
	r := Coverage(coverageContracts(), exercisedAll())
	if !r.OK() || r.Contracts != 3 || r.Covered != 3 || r.Calls != 3 || len(r.Never) != 0 || len(r.Undeclared) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageListsAContractThatNoLiveCallExercised(t *testing.T) {
	r := Coverage(coverageContracts(), exercisedAll()[:2])
	if r.OK() || len(r.Never) != 1 || r.Never[0].Case != "failed" || r.Covered != 2 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageTellsTwoCasesOfOneRouteApart(t *testing.T) {
	// A call to the route with the other case does not cover the "failed" contract.
	ex := []Exercised{exercisedAll()[0], exercisedAll()[1], exercisedAll()[1]}
	r := Coverage(coverageContracts(), ex)
	if len(r.Never) != 1 || r.Never[0].Case != "failed" {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageListsALiveCallNoContractDeclares(t *testing.T) {
	ex := append(exercisedAll(), Exercised{modclient.M3, "POST /v1/secret-new-route", ""})
	r := Coverage(coverageContracts(), ex)
	if r.OK() || len(r.Undeclared) != 1 || r.Undeclared[0].Route != "POST /v1/secret-new-route" || len(r.Never) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageTellsTheSameRouteOfTwoModulesApart(t *testing.T) {
	ex := []Exercised{{modclient.M2, "GET /healthz", ""}, exercisedAll()[1], exercisedAll()[2]}
	r := Coverage(coverageContracts(), ex)
	if len(r.Never) != 1 || r.Never[0].Module != modclient.M1 || len(r.Undeclared) != 1 || r.Undeclared[0].Module != modclient.M2 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageCountsADistinctCallOnceHoweverOftenItWasMade(t *testing.T) {
	ex := append(exercisedAll(), exercisedAll()...)
	r := Coverage(coverageContracts(), ex)
	if r.Calls != 3 || !r.OK() {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoverageWithNoContractsIsNotOKBecauseTheSubjectIsEmpty(t *testing.T) {
	r := Coverage(nil, exercisedAll())
	if r.OK() || r.Contracts != 0 {
		t.Fatalf("report = %+v", r)
	}
	var out bytes.Buffer
	r.Print(&out)
	if !strings.Contains(out.String(), "found 0 contracts") {
		t.Fatalf("print = %q", out.String())
	}
}

func TestCoverageWithNoLiveCallsListsEveryContract(t *testing.T) {
	r := Coverage(coverageContracts(), nil)
	if r.OK() || len(r.Never) != 3 || r.Covered != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestCoveragePrintSaysTheCountsWhatIsNotCoveredAndWhatTheGateCannotSee(t *testing.T) {
	ex := append(exercisedAll()[:2], Exercised{modclient.M3, "GET /v1/extra", "x"})
	r := Coverage(coverageContracts(), ex)
	var out bytes.Buffer
	r.Print(&out)
	text := out.String()
	for _, want := range []string{
		"2 of 3 contracts compared with a live response",
		"1 never exercised",
		"1 live calls with no contract",
		"NOT COMPARED: m2 GET /v1/jobs/{id} case=failed",
		"UNDECLARED: m3 GET /v1/extra case=x",
		BlindSpots,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	// The paired control: a fully covered run prints no NOT COMPARED / UNDECLARED line but still prints the blind spots.
	out.Reset()
	Coverage(coverageContracts(), exercisedAll()).Print(&out)
	if strings.Contains(out.String(), "NOT COMPARED") || strings.Contains(out.String(), "UNDECLARED") || !strings.Contains(out.String(), BlindSpots) {
		t.Fatalf("print = %q", out.String())
	}
}

func TestBlindSpotsSentenceListsWhatTheDriftGateCannotSee(t *testing.T) {
	for _, part := range []string{"inputs the harness never sends", "states the harness never reaches", "only in production"} {
		if !strings.Contains(BlindSpots, part) {
			t.Fatalf("the fixed sentence must mention %q", part)
		}
	}
}

func TestCoverageCountsDuplicateContractsOnce(t *testing.T) {
	cs := append(coverageContracts(), coverageContracts()[0])
	r := Coverage(cs, exercisedAll())
	if r.Contracts != 3 || !r.OK() {
		t.Fatalf("report = %+v", r)
	}
}
