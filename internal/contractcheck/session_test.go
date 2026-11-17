package contractcheck

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

func newSession(t *testing.T, allow map[string]string) *Session {
	t.Helper()
	s := NewSession(sessionContracts(), fixtureFS(sessionFiles(t)), fxDir, allow)
	s.SetRecordMode(false)
	return s
}

// observeAll feeds every observation of a full, matching run (all four contracts exercised) to the session.
func observeAll(t *testing.T, s *Session) {
	t.Helper()
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, jobBody))
	obs(jobObs("/v1/jobs/b", 200, emptyJobBody(t)))
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
}

func TestSessionPassesAndPrintsTheCoverageWhenEveryContractMatches(t *testing.T) {
	s := newSession(t, nil)
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("must pass: %s", tb.allText())
	}
	logs := strings.Join(tb.logs, "\n")
	for _, want := range []string{"4 of 4 contracts compared with a live response", "0 never exercised", BlindSpots} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the coverage must say %q:\n%s", want, logs)
		}
	}
}

func TestSessionFailsOnADifferenceNamingModuleRouteCasePathAndKind(t *testing.T) {
	s := newSession(t, nil)
	observeAll(t, s)
	s.Observer()(jobObs("/v1/jobs/z", 200, string(mutate(t, jobBody, func(m map[string]any) {
		delete(m["counts"].(map[string]any), "done")
		m["state"] = "paused"
	}))))
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	wants := []string{"m2 GET /v1/jobs/{id} case=done", "path=counts.done", "path=state"}
	for _, kind := range []Kind{KeyMissing, ShapeViolation, VocabNew} {
		wants = append(wants, "kind="+string(kind))
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("the failures must say %q:\n%s", want, text)
		}
	}
	// The paired control: the untouched run above passes (TestSessionPassesAndPrintsTheCoverageWhenEveryContractMatches), and an untouched extra observation adds nothing.
	s2 := newSession(t, nil)
	observeAll(t, s2)
	s2.Observer()(jobObs("/v1/jobs/z", 200, jobBody))
	tb2 := &fakeTB{}
	s2.Finish(tb2)
	if tb2.failed() {
		t.Fatalf("a second matching observation must pass: %s", tb2.allText())
	}
}

func TestSessionAnIdenticalDifferenceIsListedOnceWithHowManyObservationsShowedIt(t *testing.T) {
	s := newSession(t, nil)
	observeAll(t, s)
	for i := 0; i < 3; i++ {
		body := string(mutate(t, jobBody, func(m map[string]any) {
			m["added"] = 1
			m["job_id"] = fmt.Sprintf("00000000-0000-4000-8000-00000000009%d", i)
		}))
		s.Observer()(jobObs(fmt.Sprintf("/v1/jobs/n%d", i), 200, body))
	}
	tb := &fakeTB{}
	s.Finish(tb)
	n := 0
	for _, e := range tb.errs {
		if strings.Contains(e, "path=added") {
			n++
			if !strings.Contains(e, "shown by 3 observations") {
				t.Fatalf("the count must be shown: %s", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("the difference must be listed once, was listed %d times:\n%s", n, strings.Join(tb.errs, "\n"))
	}
}

func TestSessionFailsOnAnUndeclaredRouteWithoutPrintingItsBody(t *testing.T) {
	marker := "MARK" + token(t, 8)
	s := newSession(t, nil)
	observeAll(t, s)
	s.Observer()(ob(modclient.M2, "m2.new", "POST", "/v1/newroute/"+marker, 201, `{"note":"`+marker+`"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "undeclared dependency") || !strings.Contains(tb.errs[0], "POST /v1/{}/{}") || !strings.Contains(tb.errs[0], "answered 201") {
		t.Fatalf("errors = %v", tb.errs)
	}
	if strings.Contains(tb.allText(), marker) {
		t.Fatalf("the report carries a value of the observation:\n%s", tb.allText())
	}
}

func TestSessionFailsOnAnUnexercisedContractAndAnAllowlistReasonExcusesOnlyThatOne(t *testing.T) {
	s := newSession(t, nil)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, jobBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	if len(tb.errs) != 2 || !strings.Contains(text, "case=empty") || !strings.Contains(text, "case=not_found") || !strings.Contains(text, "never exercised") {
		t.Fatalf("errors = %v", tb.errs)
	}
	// With a reason for one of them it is excused, printed with its reason and counted; the other still fails.
	allow := map[string]string{"m2|GET /v1/jobs/{id}|empty": "no harness test can make module 2 return an empty page"}
	s2 := newSession(t, allow)
	obs = s2.Observer()
	obs(jobObs("/v1/jobs/a", 200, jobBody))
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
	tb2 := &fakeTB{}
	s2.Finish(tb2)
	if len(tb2.errs) != 1 || !strings.Contains(tb2.errs[0], "case=not_found") {
		t.Fatalf("only the contract without a reason may fail: %v", tb2.errs)
	}
	logs := strings.Join(tb2.logs, "\n")
	if !strings.Contains(logs, "ALLOWED UNEXERCISED: m2 GET /v1/jobs/{id} case=empty: no harness test can make module 2 return an empty page") || !strings.Contains(logs, "1 more contracts are allowed to stay unexercised") {
		t.Fatalf("the allowed contract and its reason must be printed:\n%s", logs)
	}
}

func TestSessionAnAllowlistEntryWithoutAReasonIsAnErrorAndDoesNotExcuseTheContract(t *testing.T) {
	for _, reason := range []string{"", "   ", "\t\n"} {
		s := newSession(t, map[string]string{"m2|GET /v1/jobs/{id}|empty": reason})
		obs := s.Observer()
		obs(jobObs("/v1/jobs/a", 200, jobBody))
		obs(jobObs("/v1/jobs/c", 404, notFoundBody))
		obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"ok"}`))
		tb := &fakeTB{}
		s.Finish(tb)
		text := strings.Join(tb.errs, "\n")
		if !strings.Contains(text, "has no reason") || !strings.Contains(text, "case=empty") || !strings.Contains(text, "never exercised") {
			t.Fatalf("reason %q: want the missing reason AND the unexercised contract, got:\n%s", reason, text)
		}
	}
}

func TestSessionAnAllowlistEntryForAContractThatWasExercisedIsStale(t *testing.T) {
	s := newSession(t, map[string]string{"m2|GET /healthz|": "the health route needs a stopped module"})
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "is stale") || !strings.Contains(tb.errs[0], "GET /healthz") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestSessionAnAllowlistEntryThatNamesNoContractOrIsMalformedIsAnError(t *testing.T) {
	s := newSession(t, map[string]string{
		"m2|GET /nothere|":       "a reason that is fine",
		"not three parts":        "a reason that is fine",
		"m9|GET /v1/jobs/{id}|x": "a reason that is fine",
	})
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	if len(tb.errs) != 3 || strings.Count(text, "names no contract") != 2 || !strings.Contains(text, "not of the form module|route|case") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestSessionWithNoContractsFailsBecauseTheSubjectIsEmpty(t *testing.T) {
	s := NewSession(nil, fixtureFS(nil), fxDir, nil)
	s.SetRecordMode(false)
	s.Observer()(jobObs("/v1/jobs/a", 200, jobBody))
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	if !strings.Contains(text, "found 0 contracts") || !strings.Contains(text, "undeclared dependency") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestSessionObserverIsGoroutineSafeAndCountsIdenticalResponsesOnceWithoutKeepingThem(t *testing.T) {
	s := newSession(t, nil)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			obs := s.Observer()
			for i := 0; i < 50; i++ {
				obs(jobObs("/v1/jobs/same", 200, jobBody))
				obs(jobObs(fmt.Sprintf("/v1/jobs/g%d", g%4), 200, jobBody))
			}
		}(g)
	}
	wg.Wait()
	snap, total, dropped := s.snapshot()
	if len(snap) != 5 || total != 32*100 || dropped != 0 {
		t.Fatalf("kept %d distinct (want 5), total %d (want %d), dropped %d", len(snap), total, 32*100, dropped)
	}
}

func TestSessionKeepsAtMostMaxObservationsAndSaysSoInsteadOfGrowing(t *testing.T) {
	s := newSession(t, nil)
	obs := s.Observer()
	over := 25
	for i := 0; i < MaxObservations+over; i++ {
		obs(ob(modclient.M2, "m2.x", "GET", "/v1/other", 200, fmt.Sprintf(`{"n":%d}`, i)))
	}
	snap, total, dropped := s.snapshot()
	if len(snap) != MaxObservations || dropped != over || total != MaxObservations+over {
		t.Fatalf("kept %d (cap %d), dropped %d (want %d), total %d", len(snap), MaxObservations, dropped, over, total)
	}
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	if !strings.Contains(text, fmt.Sprintf("%d distinct observations were dropped after the cap of %d", over, MaxObservations)) {
		t.Fatalf("a compare run past the cap must fail and say so:\n%s", text)
	}
	// The paired control: a run under the cap does not mention a cap.
	s2 := newSession(t, nil)
	observeAll(t, s2)
	tb2 := &fakeTB{}
	s2.Finish(tb2)
	if strings.Contains(tb2.allText(), "dropped") {
		t.Fatalf("no cap was hit: %s", tb2.allText())
	}
}

func TestSessionRecordModePrintsOneBlockPerMatchedContractAndPlaceholdersForUndeclaredCalls(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	// Differences and an undeclared route: none of it fails in record mode.
	obs(jobObs("/v1/jobs/a", 200, string(mutate(t, jobBody, func(m map[string]any) { m["extra"] = 1; m["state"] = "paused" }))))
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.new", "POST", "/v1/newroute", 202, `{"accepted":true}`))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("record mode must not fail on differences, undeclared calls or unexercised contracts: %s", tb.allText())
	}
	recs, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	if err != nil {
		t.Fatalf("what was logged must parse back: %v", err)
	}
	files := map[string]Recorded{}
	for _, r := range recs {
		files[r.File] = r
	}
	if len(recs) != 3 || files["job-done.json"].Route != "GET /v1/jobs/{id}" || files["job-done.json"].Case != "done" || files["job-not-found.json"].Status != 404 {
		t.Fatalf("blocks = %+v", recs)
	}
	u, ok := files["UNDECLARED-m2-1.json"]
	if !ok || u.Route != "POST /v1/{}" || u.Status != 202 {
		t.Fatalf("an undeclared call must be printed under a placeholder file: %+v", files)
	}
	logs := strings.Join(tb.logs, "\n")
	if !strings.Contains(logs, "2 contracts still unexercised") || !strings.Contains(logs, "still unexercised: m2 GET /healthz") {
		t.Fatalf("the unexercised contracts must be counted and named:\n%s", logs)
	}
	// The paired control: the same observations in compare mode fail.
	c := newSession(t, nil)
	co := c.Observer()
	co(jobObs("/v1/jobs/a", 200, string(mutate(t, jobBody, func(m map[string]any) { m["extra"] = 1 }))))
	co(ob(modclient.M2, "m2.new", "POST", "/v1/newroute", 202, `{"accepted":true}`))
	ctb := &fakeTB{}
	c.Finish(ctb)
	if !ctb.failed() {
		t.Fatal("compare mode must fail on the same observations")
	}
}

func TestSessionRecordModePrintsTheObservationThatSaysMostWhenSeveralFitOneContract(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/a", 200, jobBody)) // reason is null here
	obs(jobObs("/v1/jobs/b", 200, string(mutate(t, jobBody, func(m map[string]any) { m["reason"] = "bounced" }))))
	tb := &fakeTB{}
	s.Finish(tb)
	recs, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	if err != nil || len(recs) != 1 {
		t.Fatalf("one block per contract: %v %v", recs, err)
	}
	if !strings.Contains(string(recs[0].Body), "bounced") {
		t.Fatalf("the observation with fewer unexercised positions must be printed:\n%s", recs[0].Body)
	}
}

func TestSessionRecordModeRefusesASecretShapedBodyAndStillPrintsTheOthers(t *testing.T) {
	tok := token(t, 24)
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	obs(ob(modclient.M2, "m2.healthz", "GET", "/healthz", 200, `{"status":"Bearer `+tok+`"}`))
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	tb := &fakeTB{}
	s.Finish(tb)
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "cannot record") || strings.Contains(tb.allText(), tok) {
		t.Fatalf("errors = %v", tb.errs)
	}
	recs, err := ParseRecorded([]byte(strings.Join(tb.logs, "\n")))
	if err != nil || len(recs) != 1 || recs[0].File != "job-not-found.json" {
		t.Fatalf("the clean block must still be printed: %v %v", recs, err)
	}
}

func TestSessionRecordModeNotesABodyThatIsNotJSONWithoutFailing(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	obs := s.Observer()
	obs(jobObs("/v1/jobs/c", 404, notFoundBody))
	obs(ob(modclient.M2, "m2.x", "GET", "/v1/other", 502, `<html>bad gateway</html>`))
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() || !strings.Contains(strings.Join(tb.logs, "\n"), "not recorded") {
		t.Fatalf("a body that is not JSON is noted, not failed: %s", tb.allText())
	}
}

func TestSessionRecordModeWithNothingObservedFails(t *testing.T) {
	s := newSession(t, nil)
	s.SetRecordMode(true)
	tb := &fakeTB{}
	s.Finish(tb)
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "nothing to record") {
		t.Fatalf("errors = %v", tb.errs)
	}
}

func TestSessionTheEnvironmentVariableSelectsRecordMode(t *testing.T) {
	t.Setenv(EnvRecord, "1")
	if !NewSession(sessionContracts(), fixtureFS(nil), fxDir, nil).Recording() {
		t.Fatal("CONTRACT_RECORD=1 must select record mode")
	}
	t.Setenv(EnvRecord, "")
	if NewSession(sessionContracts(), fixtureFS(nil), fxDir, nil).Recording() {
		t.Fatal("record mode must be off by default")
	}
}

func TestSessionDoesNotKeepTheCallersAllowlistMap(t *testing.T) {
	allow := map[string]string{"m2|GET /healthz|": "a reason that is fine"}
	s := newSession(t, allow)
	delete(allow, "m2|GET /healthz|")
	observeAll(t, s)
	tb := &fakeTB{}
	s.Finish(tb)
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "is stale") {
		t.Fatalf("the session must work from its own copy of the allowlist: %v", tb.errs)
	}
}

// ---- through a real modclient.Client -----------------------------------------------------------------------------------

func realClient(t *testing.T, s *Session, handler http.HandlerFunc) *modclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	opts := modclient.Options{Timeout: 5 * time.Second, Observe: s.Observer()}
	c, err := modclient.New(modclient.M2, srv.URL, config.Secret("tok-"+token(t, 8)), srv.Client(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serve(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestSessionThroughARealClientMatchesTheContractOfTheRequestAsSent(t *testing.T) {
	s := newSession(t, map[string]string{
		"m2|GET /v1/jobs/{id}|empty":     "the harness never gets an empty page",
		"m2|GET /v1/jobs/{id}|not_found": "the harness never asks for an unknown job",
	})
	c := realClient(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			serve(w, 200, `{"status":"ok"}`)
			return
		}
		serve(w, 200, jobBody)
	})
	for _, req := range []modclient.Request{
		{Route: "m2.jobs", Method: "GET", Path: "/v1/jobs/0198f6a2-8c3e"},
		{Route: "m2.healthz", Method: "GET", Path: "/healthz", Anonymous: true},
	} {
		if _, err := c.Do(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("the real client's observations must match the contracts: %s", tb.allText())
	}
}

func TestSessionThroughARealClientSurfacesAModuleChangeAsTheExactDifference(t *testing.T) {
	s := newSession(t, map[string]string{
		"m2|GET /v1/jobs/{id}|empty":     "the harness never gets an empty page",
		"m2|GET /v1/jobs/{id}|not_found": "the harness never asks for an unknown job",
		"m2|GET /healthz|":               "not needed in this test",
	})
	changed := string(mutate(t, jobBody, func(m map[string]any) { delete(m, "created_at"); m["state"] = "paused" }))
	c := realClient(t, s, func(w http.ResponseWriter, r *http.Request) { serve(w, 200, changed) })
	if _, err := c.Do(t.Context(), modclient.Request{Route: "m2.jobs", Method: "GET", Path: "/v1/jobs/abc"}); err != nil {
		t.Fatal(err)
	}
	tb := &fakeTB{}
	s.Finish(tb)
	text := strings.Join(tb.errs, "\n")
	for _, want := range []string{"path=created_at", "kind=" + string(KeyMissing), "path=state", "kind=" + string(VocabNew)} {
		if !strings.Contains(text, want) {
			t.Fatalf("the failure must say %q:\n%s", want, text)
		}
	}
}

func TestSessionThroughARealClientSeesAnErrorResponseTheAdapterTurnedIntoAnError(t *testing.T) {
	s := newSession(t, map[string]string{
		"m2|GET /v1/jobs/{id}|done":  "this test only makes the error call",
		"m2|GET /v1/jobs/{id}|empty": "this test only makes the error call",
		"m2|GET /healthz|":           "this test only makes the error call",
	})
	c := realClient(t, s, func(w http.ResponseWriter, r *http.Request) { serve(w, 404, notFoundBody) })
	if _, err := c.Do(t.Context(), modclient.Request{Route: "m2.jobs", Method: "GET", Path: "/v1/jobs/abc"}); modclient.KindOf(err) != modclient.NotFound {
		t.Fatalf("the call must fail as not found: %v", err)
	}
	tb := &fakeTB{}
	s.Finish(tb)
	if tb.failed() {
		t.Fatalf("an error envelope is a declared response: %s", tb.allText())
	}
}
