package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S2 at the HTTP boundary, against the fake store: what a preview answers with when the job has templates (the counts, the names, the samples), that the samples are in THAT answer and nowhere
// else, what a confirm says about the rows it left out, and what a syntax error of a template looks like to a client. What the store computes is proved against a real database by
// tests/harness/jobs_templates_test.go.

func templatesJob() jobs.Job {
	j := fullJob()
	j.Module = "m2"
	j.Status = jobs.StatusPreviewed
	j.Preview = &jobs.Preview{
		Rows: 10, Pending: 8, Malformed: 1, Duplicates: 1, Units: 5, Metered: true, RateUnit: "m2_accepted", UnitCost: 1, Credits: 5, Available: 100,
		MissingFields: 2, MissingFieldNames: []string{"first_name", "plan"}, MissingFieldNamesTruncated: false, Unrenderable: 1,
		Fingerprint: strings.Repeat("a", 64), ComputedAt: t0,
	}
	return j
}

func TestRoutes_PreviewShowsTheCountsTheNamesAndTheSamplesOnlyInItsOwnAnswer(t *testing.T) {
	r := newRig(t)
	r.store.job = templatesJob()
	r.store.preview = jobs.Preview{Samples: []jobs.Sample{
		{Ordinal: 1, Subject: "Hi Ann", HTML: "<p>Hi Ann</p>", Text: "Hi Ann"},
		{Ordinal: 4, Subject: "Hello Bob", HTML: "<p>Hello Bob</p>", Text: "Hello Bob"},
		{Ordinal: 9, Subject: "Hey Cy", HTML: "<p>Hey Cy</p>", Text: "Hey Cy"},
	}}
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/preview", nil)
	m := r.wantStatus(resp, body, 200)
	pv := m["job"].(map[string]any)["preview"].(map[string]any)
	if pv["missing_fields"] != float64(2) || pv["unrenderable"] != float64(1) || pv["missing_field_names_truncated"] != false {
		t.Errorf("the counts: %v", pv)
	}
	if names, _ := pv["missing_field_names"].([]any); len(names) != 2 || names[0] != "first_name" || names[1] != "plan" {
		t.Errorf("missing_field_names = %v", pv["missing_field_names"])
	}
	samples, _ := pv["samples"].([]any)
	if len(samples) != 3 {
		t.Fatalf("the preview answer carries %d samples, want 3: %s", len(samples), body)
	}
	first := samples[0].(map[string]any)
	if first["ordinal"] != float64(1) || first["subject"] != "Hi Ann" || first["html"] != "<p>Hi Ann</p>" || first["text"] != "Hi Ann" || len(first) != 4 {
		t.Errorf("a sample is %v, want exactly ordinal, subject, html and text", first)
	}
	// the control: the job as the store holds it, asked for afterwards, has no samples (they were never stored), while its counts are there
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA)
	m = r.wantStatus(resp, body, 200)
	got := m["job"].(map[string]any)["preview"].(map[string]any)
	if _, has := got["samples"]; has || strings.Contains(string(body), "Hello Bob") {
		t.Errorf("a GET of the job carries samples: %s", body)
	}
	if got["missing_fields"] != float64(2) {
		t.Errorf("the stored counts are not shown: %v", got)
	}
	// and a preview of a job that has none (a verify job, or a store with no templates) answers with no samples member and empty names, never null
	r.store.preview = jobs.Preview{}
	j := fullJob()
	j.Status = jobs.StatusPreviewed
	r.store.job = j
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/preview", nil)
	m = r.wantStatus(resp, body, 200)
	pv = m["job"].(map[string]any)["preview"].(map[string]any)
	if _, has := pv["samples"]; has {
		t.Errorf("a preview with no sample has a samples member: %v", pv)
	}
	if names, ok := pv["missing_field_names"].([]any); !ok || len(names) != 0 {
		t.Errorf("missing_field_names = %v, want []", pv["missing_field_names"])
	}
}

func TestRoutes_ConfirmSaysHowManyRowsTheTemplatesLeftOut(t *testing.T) {
	r := newRig(t)
	r.store.confirm = jobs.ConfirmResult{Job: templatesJob(), Reserved: 5, Pending: 5, Suppressed: 1, MissingFields: 2, Unrenderable: 1}
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", map[string]any{"fingerprint": strings.Repeat("a", 64)}, http.Header{"Idempotency-Key": []string{"k-1"}})
	m := r.wantStatus(resp, body, 200)
	if m["reserved"] != float64(5) || m["pending"] != float64(5) || m["suppressed"] != float64(1) || m["missing_fields"] != float64(2) || m["unrenderable"] != float64(1) {
		t.Errorf("the confirm answered %v", m)
	}
	// the control: a confirm that left nothing out says zero, not nothing
	r.store.confirm = jobs.ConfirmResult{Job: templatesJob(), Reserved: 8, Pending: 8}
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", map[string]any{"fingerprint": strings.Repeat("a", 64)}, http.Header{"Idempotency-Key": []string{"k-2"}})
	m = r.wantStatus(resp, body, 200)
	if v, ok := m["missing_fields"]; !ok || v != float64(0) {
		t.Errorf("missing_fields = %v (present %v), want 0", v, ok)
	}
	if v, ok := m["unrenderable"]; !ok || v != float64(0) {
		t.Errorf("unrenderable = %v (present %v), want 0", v, ok)
	}
}

// A syntax error of a template reaches the client with its part, line, column and message in the details, as the 400 of the create route: the whole path through the HTTP layer, not only classify.
func TestRoutes_CreateReportsATemplateSyntaxErrorWithItsPosition(t *testing.T) {
	r := newRig(t)
	r.store.fail["CreateDraft"] = &jobs.TemplateError{Part: "html", Line: 3, Column: 7, Message: `unexpected }: write \} for a literal brace`}
	resp, body := r.post(loginA, "/v1/jobs", map[string]any{
		"module": "m2", "rows": []map[string]any{{"email": "ann@example.test"}}, "content": map[string]any{"subject": "S", "html": "<style>p }</style>"},
	})
	e := r.wantError(resp, body, 400, "validation_failed")
	want := map[string]any{"part": "html", "line": float64(3), "column": float64(7), "message": `unexpected }: write \} for a literal brace`, "reason": "the html template has a syntax error"}
	for k, v := range want {
		if e.Details[k] != v {
			t.Errorf("details[%q] = %v, want %v (all: %v)", k, e.Details[k], v, e.Details)
		}
	}
	if strings.Contains(string(body), "<style>") || strings.Contains(string(body), "ann@example.test") {
		t.Errorf("the answer quotes what the user sent: %s", body)
	}
	// the control: a bad request that is not a template error has no position
	r.store.fail["CreateDraft"] = &jobs.BadRequestError{Reason: "content.subject must not be blank"}
	resp, body = r.post(loginA, "/v1/jobs", map[string]any{"module": "m2", "rows": []map[string]any{{"email": "ann@example.test"}}, "content": map[string]any{"subject": " ", "html": "x"}})
	e = r.wantError(resp, body, 400, "validation_failed")
	if _, has := e.Details["line"]; has || e.Details["reason"] != "content.subject must not be blank" {
		t.Errorf("a plain bad request carries a position: %v", e.Details)
	}
}

// detailOf is the value of one key of the details a problem carries (key and value pairs), nil when there is none.
func detailOf(p problem, key string) any {
	for i := 0; i+1 < len(p.details); i += 2 {
		if p.details[i] == key {
			return p.details[i+1]
		}
	}
	return nil
}

// The message of a template error is cut at 200 bytes without breaking a character, and the second wall holds for a part that is none of the three.
func TestClassify_TemplateErrorMessageIsBounded(t *testing.T) {
	long := strings.Repeat("é", 150) // 300 bytes
	p := classify(wrapped(&jobs.TemplateError{Part: "subject", Line: 1, Column: 1, Message: long}))
	msg, _ := detailOf(p, "message").(string)
	if len(msg) > 200 || len(msg) < 198 || !strings.HasPrefix(long, msg) || strings.ContainsRune(msg, '�') {
		t.Errorf("the message is %d bytes %q, want at most 200 and whole characters", len(msg), msg)
	}
	short := classify(wrapped(&jobs.TemplateError{Part: "subject", Line: 1, Column: 1, Message: "ok"}))
	if detailOf(short, "message") != "ok" {
		t.Errorf("a short message was changed: %v", short.details)
	}
	if b, _ := json.Marshal(p.details); strings.Contains(string(b), "\\ufffd") {
		t.Errorf("a broken character reached the details: %s", b)
	}
}
