package m1

import (
	"net/http"
	"net/http/httptest"
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
	want := JobStatus{JobID: testJob, Status: "complete", Complete: true, Counts: JobCounts{Total: 3, Settled: 3, Pending: 0}}
	if got != want {
		t.Fatalf("Status returned %+v, want %+v", got, want)
	}
	if r := f.seen()[0]; r.Method != http.MethodGet || r.Path != "/v1/jobs/"+testJob || r.Auth != "Bearer "+f.key {
		t.Fatalf("the request was %+v", r)
	}
	body = strings.Replace(fixture(t, "job-running.json"), `"processing_paused": false`, `"processing_paused": true`, 1)
	ctl, err := a.Status(testCtx(t), testJob)
	if err != nil || ctl.Complete || ctl.Counts.Pending != 2 || !ctl.ProcessingPaused || ctl.Status != "running" {
		t.Fatalf("control: %+v, %v", ctl, err)
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

// TestJobRoutes_ANonUUIDIDIsRefusedBeforeAnyRequest: module 1 answers a job route with a non-uuid id by 500 (a store error), so Status and Results refuse the id locally with an
// Invalid error and send nothing (M1-F1). Control: a uuid, in either letter case, goes out, and the request carries the id escaped as one path segment.
func TestJobRoutes_ANonUUIDIDIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/results") {
			return 200, page(testJob, "")
		}
		return 200, completeBody(3)
	})
	a := f.adapter(t)
	for _, id := range []string{"", "J1", "019a…", testJob + "0", testJob[:35], strings.Replace(testJob, "7", "g", 1), "../" + testJob, testJob + "/results", "00000000_0000_7000_8000_00000000000a", "{" + testJob + "}"} {
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
