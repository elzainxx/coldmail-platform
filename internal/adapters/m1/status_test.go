package m1

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestStatus_ReadsTheRecordedStatus: GET /v1/jobs/{id} with the key, and the recorded finished and running bodies decode to status, complete and counts. Control: a body that says
// pending > 0 and complete false decodes as such (the decoder does not default to complete), and processing_paused is read as sent.
func TestStatus_ReadsTheRecordedStatus(t *testing.T) {
	body := fixture(t, "job-complete.json")
	f := newFakeM1(t, func(call) (int, string) { return 200, body })
	a := f.adapter(t)
	got, err := a.Status(testCtx(t), testJob)
	if err != nil {
		t.Fatal(err)
	}
	want := recordedStatus(t, "job-complete.json") // what the file says, read without the adapter
	if got != want || got.JobID != testJob || !got.Complete || got.Status != "complete" || got.Counts.Pending != 0 || got.Counts.Settled != got.Counts.Total {
		t.Fatalf("Status returned %+v, want %+v (a finished job: complete, everything settled)", got, want)
	}
	if r := f.seen()[0]; r.Method != http.MethodGet || r.Path != "/v1/jobs/"+testJob || r.Auth != "Bearer "+f.key {
		t.Fatalf("the request was %+v", r)
	}
	body = strings.Replace(fixture(t, "job-running.json"), `"processing_paused": false`, `"processing_paused": true`, 1)
	ctl, err := a.Status(testCtx(t), testJob)
	wantCtl := recordedStatus(t, "job-running.json")
	wantCtl.ProcessingPaused = true // the one value the test changed in the recording
	if err != nil || ctl != wantCtl || ctl.Complete || ctl.Counts.Pending == 0 || ctl.Counts.Pending != ctl.Counts.Total-ctl.Counts.Settled || ctl.Status != "running" || ctl == got {
		t.Fatalf("control: %+v, %v, want %+v (a running job: not complete, pending = total - settled, paused as sent, and not the finished one)", ctl, err, wantCtl)
	}
}

// TestStatus_TheStatusWordIsClosedAndTheBooleanDecides: each of module 1's four words is accepted (with a complete flag that fits it) and any other word, or complete:true beside a
// word that is not a finished one, is a Contract error naming status: never "still running". Control: each repaired body is accepted.
func TestStatus_TheStatusWordIsClosedAndTheBooleanDecides(t *testing.T) {
	read := func(body string) (JobStatus, error) {
		return serve(t, 200, body).adapter(t).Status(testCtx(t), testJob)
	}
	for _, ok := range []struct {
		word     string
		complete bool
	}{{"submitted", false}, {"running", false}, {"complete", true}, {"complete_with_unknowns", true}} {
		if js, err := read(statusBody(testJob, ok.word, ok.complete, 3, 1)); err != nil || js.Status != ok.word || js.Complete != ok.complete {
			t.Errorf("control: %q complete=%t was refused or misread: %+v %v", ok.word, ok.complete, js, err)
		}
	}
	for _, bad := range []struct {
		name     string
		word     string
		complete bool
	}{
		{"an unknown word", "queued", false},
		{"an unknown word beside complete true", "done", true},
		{"an empty word", "", false},
		{"a word in another case", "Running", false},
		{"complete true beside running", "running", true},
		{"complete true beside submitted", "submitted", true},
	} {
		js, err := read(statusBody(testJob, bad.word, bad.complete, 3, 1))
		if kind, key := kindAndKey(err); kind != modclient.Contract || key != "status" || js != (JobStatus{}) {
			t.Errorf("%s: got %+v kind %v key %q (%v), want no value and a Contract error naming status", bad.name, js, kind, key, err)
		}
	}
	// the boolean, not the word, says finished: a stored `complete` word beside complete:false is read as NOT finished (the handler only ever promotes running to complete)
	if js, err := read(statusBody(testJob, "complete", false, 3, 1)); err != nil || js.Complete {
		t.Errorf("complete word with complete:false: %+v %v, want a job that is not complete", js, err)
	}
}

// TestJobRoutes_ANonUUIDIDIsRefusedBeforeAnyRequest: Status and Results check the shape of the id locally (plain input validation: module 1 answers a non-uuid id with a 400 since the
// Phase 3 close-out, where it used to be a 500) and refuse it with an Invalid error before any request. Control: a uuid, in either letter case, goes out, and the request carries the id
// escaped as one path segment.
func TestJobRoutes_ANonUUIDIDIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/results") {
			return 200, page(testJob, "")
		}
		return 200, completeBody(3)
	})
	a := f.adapter(t)
	for _, id := range []string{"", "J1", "019a…", testJob + "0", testJob[:35], "g" + testJob[1:], "../" + testJob, testJob + "/results", "00000000_0000_7000_8000_00000000000a", "{" + testJob + "}"} {
		if validJobID(id) {
			t.Fatalf("setup: %q is a uuid, so the case proves nothing", id)
		}
		if _, err := a.Status(testCtx(t), id); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("Status(%q): got %v, want an Invalid error", id, err)
		}
		if _, _, err := a.Results(testCtx(t), id, "", 0); modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("Results(%q): got %v, want an Invalid error", id, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d request(s) for ids that are not uuids", f.count())
	}
	upper := strings.ToUpper(testJob)
	if _, err := a.Status(testCtx(t), upper); err != nil {
		t.Fatalf("control: a uuid in upper case was refused: %v", err)
	}
	if got := f.seen()[0].Path; got != "/v1/jobs/"+upper {
		t.Fatalf("the request went to %q, want the id as one path segment", got)
	}
}

// TestStatus_IsRetriedPerTheDialsAndTheModuleSaysWhatItIs: an idempotent read after a 503 is retried up to the dial (Retries 2: the third attempt answers); with no retries the
// first 503 is the error. A job module 1 does not know (another tenant's included: the same 404) is NotFound with its code and nothing else.
func TestStatus_IsRetriedPerTheDialsAndTheModuleSaysWhatItIs(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(fixture(t, "submit-error-unavailable.json")))
			return
		}
		_, _ = w.Write([]byte(completeBody(3)))
	}))
	defer srv.Close()
	a, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Retries: 2, Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if js, err := a.Status(testCtx(t), testJob); err != nil || !js.Complete || n.Load() != 3 {
		t.Fatalf("with 2 retries: %+v %v after %d request(s), want success on the third", js, err, n.Load())
	}
	n.Store(0)
	b, err := New(srv.URL, config.Secret(newToken(t)), nil, modclient.Options{Sleep: noWait})
	if err != nil {
		t.Fatal(err)
	}
	if js, err := b.Status(testCtx(t), testJob); modclient.KindOf(err) != modclient.Unavailable || js != (JobStatus{}) || n.Load() != 1 {
		t.Fatalf("with no retries: %+v %v after %d request(s), want the first 503 as the error", js, err, n.Load())
	}

	f := serve(t, 404, fixture(t, "status-error-not-found.json"))
	js, err := f.adapter(t).Status(testCtx(t), testJob)
	if e := asModclient(err); e == nil || e.Kind != modclient.NotFound || e.Code != "not_found" || js != (JobStatus{}) {
		t.Fatalf("an unknown job: got %+v %v, want a NotFound error with code not_found and no value", js, err)
	}
}

// TestJobRoutes_AMalformedIDTheModuleRefusesIsRefusedNotInternal: module 1 answers a job route called with an id that is not a uuid by 400 validation_failed with the detail
// {field id, issue malformed} (the Phase 3 close-out; it used to answer 500 internal_error). The adapter checks the id's shape itself, so the answer is staged here by a stand-in for a
// module that judges an id stricter than the adapter does: Status and Results return a Refused error (never Internal) that carries the module's code and the detail, are sent once (a
// refusal is not retried, whatever the dials), and no value. Paired control: the same stand-in answering 500 internal_error is Internal (the class the old answer for a bad id had), so
// the two are told apart by what the module said and not by the stand-in.
func TestJobRoutes_AMalformedIDTheModuleRefusesIsRefusedNotInternal(t *testing.T) {
	const malformed = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"id","issue":"malformed"}],"request_id":"00000000-0000-7000-8000-0000000000d1"}}`
	calls := map[string]func(a *Adapter) error{
		"Status":  func(a *Adapter) error { _, err := a.Status(testCtx(t), testJob); return err },
		"Results": func(a *Adapter) error { _, _, err := a.Results(testCtx(t), testJob, "", 0); return err },
	}
	for name, call := range calls {
		f := serve(t, 400, malformed)
		err := call(f.adapterWith(t, modclient.Options{Retries: 2, Sleep: noWait}))
		e := asModclient(err)
		if e == nil || e.Kind != modclient.Refused || e.Status != 400 || e.Code != "validation_failed" || !e.Enveloped || modclient.KindOf(err) == modclient.Internal {
			t.Errorf("%s: got %v, want a Refused 400 validation_failed from the module's envelope, never Internal", name, err)
			continue
		}
		if len(e.Details) != 1 || e.Details[0] != (modclient.Detail{Field: "id", Issue: "malformed"}) {
			t.Errorf("%s: the details are %+v, want the id/malformed detail the module sent", name, e.Details)
		}
		if f.count() != 1 {
			t.Errorf("%s: a refusal was sent %d time(s), want once", name, f.count())
		}
		g := serve(t, 500, fixtureErr("internal_error"))
		gerr := call(g.adapterWith(t, modclient.Options{Retries: 2, Sleep: noWait}))
		if modclient.KindOf(gerr) != modclient.Internal || g.count() != 1 {
			t.Errorf("%s control: a 500 gave %v after %d request(s), want Internal", name, gerr, g.count())
		}
	}
}

// TestStatus_ReadsTheTotalsAsTheModuleSaidThem: every number of `totals` lands in its own field (the values are all different, so a swapped pair is seen), the four statuses of by_status
// too, and the cross-checked identities hold for what is read. Control: the recorded running and finished jobs read the same way, against an independent parse of the files.
func TestStatus_ReadsTheTotalsAsTheModuleSaidThem(t *testing.T) {
	body := `{"job_id":"` + testJob + `","status":"running","complete":false,"caller_ref":"","counts":{"total":40,"settled":16,"pending":24},` +
		`"totals":{"submitted":40,"settled":16,"pending":24,"duplicates":5,"malformed":3,"by_status":{"valid":6,"invalid":7,"risky":2,"unknown":1}},"processing_paused":true}`
	js, err := serve(t, 200, body).adapter(t).Status(testCtx(t), testJob)
	want := JobTotals{Submitted: 40, Settled: 16, Pending: 24, Duplicates: 5, Malformed: 3, ByStatus: StatusCounts{Valid: 6, Invalid: 7, Risky: 2, Unknown: 1}}
	if err != nil || js.Totals != want || js.Counts != (JobCounts{Total: 40, Settled: 16, Pending: 24}) || !js.ProcessingPaused || js.Complete || js.Totals.ByStatus.Sum() != js.Totals.Settled {
		t.Fatalf("got %+v %v, want totals %+v", js, err, want)
	}
	for _, name := range []string{"job-running.json", "job-complete.json"} {
		got, err := serve(t, 200, fixture(t, name)).adapter(t).Status(testCtx(t), testJob)
		if err != nil || got != recordedStatus(t, name) || got.Totals.Submitted == 0 || got.Totals.ByStatus.Sum() != got.Totals.Settled {
			t.Errorf("control: %s read as %+v %v, want what the file says: %+v", name, got, err, recordedStatus(t, name))
		}
	}
}

// TestStatus_AnAnswerThatContradictsItselfIsAContractError: the counts, the totals and `complete` come from one snapshot, so numbers that break its identities are the module
// contradicting itself, and a key that is missing or retyped is the module changing its shape. Each case starts from a job read from the recording of a running job and changes one
// thing, and gets a Contract error that names the key and no value: totals.submitted that is not settled + pending, a by_status that does not sum to settled, counts.total,
// counts.settled or counts.pending that is not its totals twin, a negative count anywhere in totals, and a totals key removed or retyped.
//
// The recording is a job that has settled nothing (every row pending, every by_status count 0), so by itself it cannot say "one fewer than settled" without a negative count: the
// numeric cases start from a job BUILT from it inline (the recorded body with more rows submitted and some of the pending rows settled as `invalid`, every number that the change
// touches changed together), and every number a case writes is that job's own number plus or minus one, derived, never a literal of a recording. The cases about the shape of
// `totals` (a key removed, a value of the wrong type) do not depend on any number and are run on BOTH the recorded body and the built one. Paired controls: the recorded job
// unchanged is read (to what the file says), and so is the built job (to the numbers it was built with): the same numbers, changed together in every place that holds them, keep every
// identity, so what the cases are refused for is the one thing each changed; and each mutated body is checked to differ from the body it started from.
func TestStatus_AnAnswerThatContradictsItselfIsAContractError(t *testing.T) {
	recorded := fixture(t, "job-running.json")
	rec := recordedStatus(t, "job-running.json") // what the file says, read without the adapter

	// the built job: the recording with extra more rows submitted, settle of the pending rows settled as invalid
	const extra, settle = 10, 4
	built := rec
	built.Totals.Submitted += extra
	built.Totals.Settled += settle
	built.Totals.Pending += extra - settle
	built.Totals.ByStatus.Invalid += settle
	built.Counts = JobCounts{Total: built.Totals.Submitted, Settled: built.Totals.Settled, Pending: built.Totals.Pending}
	if built.Totals.Pending < 2 || built.Totals.Settled < 2 || built.Totals.ByStatus.Invalid < 1 || built == rec {
		t.Fatalf("setup: the built job %+v cannot say one fewer or one more of any of its numbers without a negative count, or it is the recording %+v", built, rec)
	}
	t0 := built.Totals
	grown := recorded
	for _, m := range []mutation{
		retype("totals.submitted", t0.Submitted), retype("totals.settled", t0.Settled), retype("totals.pending", t0.Pending), retype("totals.by_status.invalid", t0.ByStatus.Invalid),
		retype("counts.total", built.Counts.Total), retype("counts.settled", built.Counts.Settled), retype("counts.pending", built.Counts.Pending),
	} {
		grown = m.apply(t, grown)
	}

	// the cases about shape: no number of these is the recording's
	shape := func(b JobStatus) []mutation {
		return []mutation{
			remove("totals"),
			remove("totals.submitted"), remove("totals.settled"), remove("totals.pending"), remove("totals.duplicates"), remove("totals.malformed"),
			remove("totals.by_status"), remove("totals.by_status.valid"), remove("totals.by_status.invalid"), remove("totals.by_status.risky"), remove("totals.by_status.unknown"),
			retype("totals", []any{}), retype("totals.by_status", b.Totals.Settled), retype("totals.submitted", strconv.Itoa(b.Totals.Submitted)),
			retype("totals.settled", float64(b.Totals.Settled)+0.5), retype("totals.by_status.invalid", nil),
		}
	}
	// the cases about the identities, on the built job. A case that names another key than the one it changed is one where the changed number breaks the identity Status checks first
	// (`naming` says which): the totals are checked against themselves before the counts are checked against the totals.
	identities := []mutation{
		retype("totals.submitted", t0.Submitted+1),
		retype("totals.pending", t0.Pending-1).naming("totals.submitted"),
		retype("totals.settled", t0.Settled+1).naming("totals.submitted"),
		retype("totals.by_status.valid", t0.ByStatus.Valid+1).naming("totals.by_status"),
		retype("totals.by_status.invalid", t0.ByStatus.Invalid-1).naming("totals.by_status"),
		retype("counts.total", built.Counts.Total+1),
		retype("counts.settled", built.Counts.Settled+1),
		retype("counts.pending", built.Counts.Pending-1),
		retype("totals.duplicates", -1),
		retype("totals.malformed", -1),
		retype("totals.by_status.risky", -1),
		retype("totals.by_status.unknown", -3),
	}
	groups := []struct {
		label string
		body  string
		job   JobStatus
		cases []mutation
	}{
		{"the recorded job", recorded, rec, shape(rec)},
		{"the job built from the recording", grown, built, append(shape(built), identities...)},
	}
	checked := 0
	for _, g := range groups {
		unchanged := encodeDoc(t, decodeDoc(t, g.body)) // a mutated body is written back by the same encoder, so this is what "no change" looks like
		for _, m := range g.cases {
			mutated := m.apply(t, g.body)
			if mutated == unchanged {
				t.Errorf("%s, %s: the mutation changed nothing, so it proves nothing", g.label, m.name)
				continue
			}
			js, err := serve(t, 200, mutated).adapter(t).Status(testCtx(t), testJob)
			kind, key := kindAndKey(err)
			if kind != modclient.Contract || key != m.key || js != (JobStatus{}) {
				t.Errorf("%s, %s: got %+v kind %v key %q (%v), want no value and a Contract error naming %q", g.label, m.name, js, kind, key, err, m.key)
			}
			checked++
		}
		// control: the body each case started from is accepted, and is read as the numbers it holds
		if js, err := serve(t, 200, g.body).adapter(t).Status(testCtx(t), testJob); err != nil || js != g.job {
			t.Fatalf("control: %s gave %+v %v, want %+v", g.label, js, err, g.job)
		}
	}
	if checked < 40 {
		t.Fatalf("%d case(s) ran, want at least 40 (16 of shape on each of two jobs, 12 of identity on the built one)", checked)
	}
}
