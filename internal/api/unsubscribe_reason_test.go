package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S3: a send job cannot be confirmed by a service that cannot make an unsubscribe link. The store refuses (ErrNotAllowed, jobs.ReasonUnsubscribeNotConfigured: internal/jobs and the harness
// prove when); these tests prove what the CLIENT is told at the HTTP boundary and that the document says it.

func TestConfirm_ASendJobRefusedForWantOfAnUnsubscribeLinkIsA409WithItsOwnWord(t *testing.T) {
	r := newRig(t)
	h := http.Header{"Idempotency-Key": {"key-unsub-1"}}
	r.store.fail["Confirm"] = fmt.Errorf("jobs.confirm: %w", &jobs.NotAllowedError{Reason: jobs.ReasonUnsubscribeNotConfigured})
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(``), h)
	e := r.wantError(resp, body, 409, "not_allowed")
	if e.Details["reason"] != "unsubscribe_not_configured" {
		t.Fatalf("the reason is %v, want unsubscribe_not_configured: %s", e.Details["reason"], body)
	}
	if e.Message != messages[codeNotAllowed] || strings.Contains(string(body), "PLATFORM_") || strings.Contains(string(body), "jobs.confirm") {
		t.Fatalf("the answer names a dial or the Go error, which are the operator's and the log's: %s", body)
	}
	// the control: another refusal of the same code does NOT carry the word, and a store that answers is a 200 (so the word above is the store's reason and not a property of every confirm)
	r.store.fail["Confirm"] = fmt.Errorf("jobs.confirm: %w", &jobs.NotAllowedError{Reason: "not_a_draft"})
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(``), h)
	if e := r.wantError(resp, body, 409, "not_allowed"); e.Details["reason"] != "not_a_draft" {
		t.Fatalf("another reason came out as %v", e.Details["reason"])
	}
	delete(r.store.fail, "Confirm")
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(``), h)
	r.wantStatus(resp, body, 200)
}

func TestDocs_JobsAPIDocumentsTheUnsubscribeNotConfiguredReason(t *testing.T) {
	doc := readJobsDoc(t)
	row := regexp.MustCompile("(?m)^\\| 409 \\| `not_allowed` \\|.*$").FindString(doc)
	if row == "" {
		t.Fatal("the not_allowed row of the error table is not in the document")
	}
	if !strings.Contains(row, "unsubscribe_not_configured") {
		t.Fatalf("the not_allowed row does not list the reason unsubscribe_not_configured: %s", row)
	}
	// the control: the check can fail
	if strings.Contains(strings.Replace(row, "unsubscribe_not_configured", "x", 1), "unsubscribe_not_configured") {
		t.Fatal("control failed: the reason is still found after it was taken out")
	}
	// the section that says what to do about it names both dials and the status
	for _, need := range []string{"PLATFORM_UNSUBSCRIBE_SECRET", "PLATFORM_PUBLIC_BASE_URL", "unsubscribe.md"} {
		if !strings.Contains(doc, need) {
			t.Errorf("Docs/jobs-api.md does not mention %s", need)
		}
	}
	// the word is in the mapping, so the document and the code name the same one
	if notAllowedWord(jobs.ReasonUnsubscribeNotConfigured) != "unsubscribe_not_configured" {
		t.Fatalf("notAllowedWord(%q) = %q", jobs.ReasonUnsubscribeNotConfigured, notAllowedWord(jobs.ReasonUnsubscribeNotConfigured))
	}
}
