//go:build harness

package harness

import (
	"bytes"
	"encoding/csv"
	"reflect"
	"testing"
)

// The two owner rulings of the Phase 4 close-out over HTTP, through the real stack in one process (newJapEnv: the real API, store, money store and tenant resolver on a migrated database). The
// store-level proofs are in jobs_suppression_scope_test.go and jobs_unknown_choice_test.go; these show what a CLIENT sees: the preview's new members, the flag in the JSON and the CSV, the two refusals
// of the choice with their details, and that a refused confirm writes nothing.

func (e *japEnv) confirmWith(user, id, fingerprint, key string, extra map[string]any) japResp {
	e.t.Helper()
	body := map[string]any{"fingerprint": fingerprint}
	for k, v := range extra {
		body[k] = v
	}
	return e.json(user, "POST", "/v1/jobs/"+id+"/confirm", body, "Idempotency-Key", key)
}

// resultsOf is the job's results over JSON, by ordinal.
func (e *japEnv) resultsOf(user, id string) map[int]map[string]any {
	e.t.Helper()
	out := map[int]map[string]any{}
	for _, x := range e.json(user, "GET", "/v1/jobs/"+id+"/results?limit=100", nil).obj(e.t, 200)["results"].([]any) {
		m := x.(map[string]any)
		out[int(m["ordinal"].(float64))] = m
	}
	return out
}

// csvFlags is the last column of the job's results.csv by ordinal, after checking the header names it.
func (e *japEnv) csvFlags(user, id string) map[int]string {
	e.t.Helper()
	resp := e.callAs(user, "GET", "/v1/jobs/"+id+"/results.csv", "", nil)
	recs, err := csv.NewReader(bytes.NewReader(resp.Body)).ReadAll()
	if err != nil || len(recs) < 1 {
		e.t.Fatalf("results.csv is not a CSV (%v): %s", err, resp.Body)
	}
	if got := recs[0][len(recs[0])-1]; got != "suppressed" || len(recs[0]) != 9 {
		e.t.Fatalf("the CSV header is %v, want nine columns ending in suppressed", recs[0])
	}
	out := map[int]string{}
	for _, rec := range recs[1:] {
		var ord int
		for _, c := range rec[0] {
			ord = ord*10 + int(c-'0')
		}
		out[ord] = rec[8]
	}
	return out
}

func TestJobsAPI_AVerifyJobsSuppressedAddressIsVerifiedChargedAndFlaggedAndItsSendTwinLeavesItOut(t *testing.T) {
	e := newJapEnv(t)
	A, B := mnyUserA, mnyUserB // A suppresses bob; B is the twin that suppresses nobody
	for _, u := range []string{A, B} {
		e.fund(u, "m1", 1000)
	}
	e.fund(A, "m2", 1000)
	e.suppressFor(A, "bob@example.test")
	list := []string{"alice@example.test", "bob@example.test", "carol@example.test"}

	// the preview: bob is counted, is said to be still verified, and the credits are the twin's three
	ida, idb := e.create(A, "m1", list...), e.create(B, "m1", list...)
	fpa, pa := e.previewJob(A, ida)
	fpb, pb := e.previewJob(B, idb)
	if pa["suppressed"] != float64(1) || pa["suppressed_still_verified"] != true || pa["units"] != float64(3) || pa["credits"] != float64(3) || pa["pending"] != float64(3) {
		t.Errorf("the verify preview is %v, want suppressed 1, still verified, 3 units, 3 credits", pa)
	}
	if pb["suppressed"] != float64(0) || pb["suppressed_still_verified"] != true || pb["credits"] != pa["credits"] {
		t.Errorf("the twin's preview is %v, want no suppressed address and the same credits", pb)
	}
	// the confirm holds the twin's credits and says one row carries the flag
	ca, cb := e.confirmJob(A, ida, fpa, e.key()).obj(t, 200), e.confirmJob(B, idb, fpb, e.key()).obj(t, 200)
	if ca["pending"] != float64(3) || ca["reserved"] != float64(3) || ca["suppressed"] != float64(1) || cb["pending"] != float64(3) || cb["reserved"] != float64(3) || cb["suppressed"] != float64(0) {
		t.Fatalf("the confirms answered %v and %v, want 3 pending and 3 reserved for both, the flag on one row of the first", ca, cb)
	}
	e.requireBal("the suppressed job holds three rows", A, "m1", 997, 3)
	e.requireBal("the twin holds three rows", B, "m1", 997, 3)

	// the results (the module's, written here as the engine will): JSON and CSV flag bob and nothing else; the twin's are all false
	e.recordFreeModuleResults(ida)
	e.recordFreeModuleResults(idb)
	ra, rb := e.resultsOf(A, ida), e.resultsOf(B, idb)
	if len(ra) != 3 || len(rb) != 3 {
		t.Fatalf("%d and %d results, want 3 each", len(ra), len(rb))
	}
	for ord, m := range ra {
		if want := ord == 2; m["suppressed"] != want || m["origin"] != "module" || m["state"] != "valid" {
			t.Errorf("JSON result %d of the suppressed job is %v, want a module result flagged only for ordinal 2", ord, m)
		}
	}
	for ord, m := range rb {
		if m["suppressed"] != false {
			t.Errorf("JSON result %d of the twin is flagged: %v", ord, m)
		}
	}
	if got, want := e.csvFlags(A, ida), map[int]string{1: "false", 2: "true", 3: "false"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the CSV flags of the suppressed job are %v, want %v", got, want)
	}
	if got, want := e.csvFlags(B, idb), map[int]string{1: "false", 2: "false", 3: "false"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the CSV flags of the twin are %v, want %v", got, want)
	}

	// THE SAME LIST AS A SEND JOB: bob is left out, free, not reserved, and his only result is the platform's, flagged
	ids := e.create(A, "m2", list...)
	fps, ps := e.previewJob(A, ids)
	if ps["suppressed"] != float64(1) || ps["suppressed_still_verified"] != false || ps["units"] != float64(2) || ps["credits"] != float64(2) {
		t.Errorf("the send preview is %v, want suppressed 1, NOT still verified, 2 units, 2 credits", ps)
	}
	cs := e.confirmJob(A, ids, fps, e.key()).obj(t, 200)
	if cs["pending"] != float64(2) || cs["reserved"] != float64(2) || cs["suppressed"] != float64(1) {
		t.Fatalf("the send confirm answered %v, want 2 pending, 2 reserved, 1 suppressed", cs)
	}
	rs := e.resultsOf(A, ids)
	if len(rs) != 1 || rs[2]["state"] != "suppressed" || rs[2]["origin"] != "platform" || rs[2]["rule"] != "P-SUP" || rs[2]["decision"] != "free" || rs[2]["suppressed"] != true || rs[2]["email"] != "bob@example.test" {
		t.Errorf("the send job's results are %v, want only bob's free platform result P-SUP, flagged", rs)
	}
	e.requireBooks("after the verify and send jobs")
}

func TestJobsAPI_TheUnknownChoiceIsExplicitRefusedWithItsDetailsAndNothingIsWrittenWithout(t *testing.T) {
	e := newJapEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m2", 1000)
	e.fund(B, "m2", 1000)
	srcA := e.verifiedSource(A, jbsUnknownVerdicts...)
	chain := func(user, src string) string {
		return jobIDOf(t, e.json(user, "POST", "/v1/jobs", map[string]any{"module": "m2", "source_job_id": src,
			"content": map[string]any{"subject": "Hello", "html": "<p>hi</p>", "text": "hi"}}), 201)
	}
	id := chain(A, srcA.ID)
	if got := e.json(A, "GET", "/v1/jobs/"+id, nil).obj(t, 200); dig(got, "job", "settings", "unknown_choice") != nil {
		t.Fatalf("a chain created with no choice shows settings %v, want unknown_choice null (there is no default)", dig(got, "job", "settings"))
	}

	// the preview with no body: the choice is null, 2 results are unknown and a choice is required
	fp0, p0 := e.previewJob(A, id)
	if p0["unknown_choice"] != nil || p0["unknown_count"] != float64(2) || p0["unknown_choice_required"] != true || p0["selected"] != float64(3) || p0["credits"] != float64(3) {
		t.Fatalf("the preview is %v, want unknown_choice null, unknown_count 2, required, 3 selected, 3 credits", p0)
	}

	// no choice: 422 with the count and the two values, and nothing is written
	plain := e.create(B, "m2", "x1@example.test", "x2@example.test") // a job that is not chained, made before the writes are measured
	before := e.writes()
	d := e.confirmWith(A, id, fp0, e.key(), nil).refused(t, 422, "unknown_choice_required")
	if d["unknown_rows"] != float64(2) || !reflect.DeepEqual(d["allowed"], []any{"include", "exclude"}) || len(d) != 2 {
		t.Errorf("the details are %v, want unknown_rows 2 and allowed [include exclude]", d)
	}
	e.requireWrites("a confirm with no unknown choice", before)
	if e.jobStatus(id) != "previewed" || e.reserveRows(id) != 0 {
		t.Errorf("the refusal left the job %s with %d reserve rows", e.jobStatus(id), e.reserveRows(id))
	}
	// a choice the job was not previewed under (nothing is recorded yet): 409 preview_stale with its reason
	d = e.confirmWith(A, id, fp0, e.key(), map[string]any{"unknown_choice": "include"}).refused(t, 409, "preview_stale")
	if d["reason"] != "unknown_choice_changed" || len(d) != 1 {
		t.Errorf("the details are %v, want the reason unknown_choice_changed", d)
	}
	e.requireWrites("a confirm under a choice the job was not previewed under", before)

	// bad requests: nothing is written, nothing is recorded
	e.json(A, "POST", "/v1/jobs/"+id+"/preview", map[string]any{"unknown_choice": "maybe"}).refused(t, 400, "validation_failed")
	e.json(A, "POST", "/v1/jobs/"+id+"/preview", map[string]any{"unknown_choice": ""}).refused(t, 400, "validation_failed")
	e.json(A, "POST", "/v1/jobs/"+id+"/preview", map[string]any{"colour": "red"}).refused(t, 400, "bad_request")
	e.confirmWith(A, id, fp0, e.key(), map[string]any{"unknown_choice": "maybe"}).refused(t, 400, "validation_failed")
	e.json(B, "POST", "/v1/jobs/"+plain+"/preview", map[string]any{"unknown_choice": "exclude"}).refused(t, 400, "validation_failed")
	e.requireWrites("the refused previews and confirms", before)

	// the preview under include records it and moves the unknown rows: the numbers and the fingerprint are the list under that choice
	body := e.json(A, "POST", "/v1/jobs/"+id+"/preview", map[string]any{"unknown_choice": "include"}).obj(t, 200)
	p1, _ := dig(body, "job", "preview").(map[string]any)
	fp1, _ := p1["fingerprint"].(string)
	if p1["unknown_choice"] != "include" || p1["pending"] != float64(5) || p1["selected"] != float64(5) || p1["credits"] != float64(5) || p1["unknown_choice_required"] != true || fp1 == "" || fp1 == fp0 {
		t.Fatalf("the preview under include is %v, want include recorded, 5 pending, 5 credits, another fingerprint", p1)
	}
	if dig(body, "job", "settings", "unknown_choice") != "include" {
		t.Errorf("the job's settings are %v, want include", dig(body, "job", "settings"))
	}
	// the confirm must say it again, and what it says must be what the job was previewed under
	e.confirmWith(A, id, fp1, e.key(), nil).refused(t, 422, "unknown_choice_required")
	e.confirmWith(A, id, fp1, e.key(), map[string]any{"unknown_choice": "exclude"}).refused(t, 409, "preview_stale")
	c := e.confirmWith(A, id, fp1, e.key(), map[string]any{"unknown_choice": "include"}).obj(t, 200)
	if c["pending"] != float64(5) || c["reserved"] != float64(5) || dig(c, "job", "settings", "unknown_choice") != "include" || dig(c, "job", "status") != "confirmed" {
		t.Fatalf("the confirm answered %v, want 5 pending, 5 reserved, include, confirmed", c)
	}
	e.requireBal("the five selected rows are held", A, "m2", 995, 5)

	// THE CONTROL: a source with no unknown result confirms with no choice at all
	srcB := e.verifiedSource(B, "valid", "risky", "invalid")
	idb := chain(B, srcB.ID)
	fpb, pb := e.previewJob(B, idb)
	if pb["unknown_count"] != float64(0) || pb["unknown_choice_required"] != false || pb["unknown_choice"] != nil {
		t.Fatalf("the preview with no unknown result is %v", pb)
	}
	if c := e.confirmJob(B, idb, fpb, e.key()).obj(t, 200); c["pending"] != float64(2) || c["reserved"] != float64(2) {
		t.Errorf("the confirm with no unknown result and no choice answered %v, want 2 pending and 2 reserved", c)
	}
	e.requireBooks("after the choice tests over HTTP")
}
