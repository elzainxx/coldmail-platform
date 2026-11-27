package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S6: an opted-out module 1 job cannot be confirmed until module 1's opt-out policy is known. The store refuses (ErrNotAllowed, jobs.ReasonOptOutPolicyUnavailable: internal/jobs and the harness
// prove when and that nothing is held); these tests prove what the CLIENT is told at the HTTP boundary and that the document says it.

func TestConfirm_AnOptedOutJobWhoseModuleOnePolicyIsUnknownIsA409WithItsOwnWord(t *testing.T) {
	r := newRig(t)
	h := http.Header{"Idempotency-Key": {"key-optout-1"}}
	r.store.fail["Confirm"] = fmt.Errorf("jobs.confirm: %w", &jobs.NotAllowedError{Reason: jobs.ReasonOptOutPolicyUnavailable})
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(``), h)
	e := r.wantError(resp, body, 409, "not_allowed")
	if e.Details["reason"] != "opt_out_policy_unavailable" {
		t.Fatalf("the reason is %v, want opt_out_policy_unavailable: %s", e.Details["reason"], body)
	}
	if e.Message != messages[codeNotAllowed] || strings.Contains(string(body), "jobs.confirm") || strings.Contains(string(body), "cold-email") {
		t.Fatalf("the answer names the Go error or a policy, which are the log's and the module's: %s", body)
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

func TestClassify_TheOptOutReasonIsAWordOfTheAPIAndTheDatabasesNamesForItAreNot(t *testing.T) {
	if got := notAllowedWord(jobs.ReasonOptOutPolicyUnavailable); got != "opt_out_policy_unavailable" {
		t.Fatalf("notAllowedWord(%q) = %q", jobs.ReasonOptOutPolicyUnavailable, got)
	}
	p := classify(fmt.Errorf("wrapped: %w", &jobs.NotAllowedError{Reason: jobs.ReasonOptOutPolicyUnavailable}))
	if p.status != http.StatusConflict || p.code != codeNotAllowed || len(p.details) != 2 || p.details[1] != "opt_out_policy_unavailable" || !p.expected {
		t.Errorf("classify = %+v, want a 409 not_allowed carrying the reason as an expected refusal", p)
	}
	// the control: the database's constraint names for the opt-out (the reserve walls of platform_0020) are never a word of the API's: they are "not_allowed"
	for _, constraint := range []string{"reserve_no_vendor_unit_for_an_opted_out_job", "jobs_vendor_opt_out_is_boolean", "jobs_vendor_opt_out_only_on_module_one"} {
		if got := notAllowedWord(constraint); got != "not_allowed" {
			t.Errorf("the constraint %q reached the client as %q", constraint, got)
		}
	}
}

func TestDocs_JobsAPIDocumentsTheOptOutPolicyUnavailableReason(t *testing.T) {
	doc := readJobsDoc(t)
	row := regexp.MustCompile("(?m)^\\| 409 \\| `not_allowed` \\|.*$").FindString(doc)
	if row == "" {
		t.Fatal("the not_allowed row of the error table is not in the document")
	}
	if !strings.Contains(row, "opt_out_policy_unavailable") {
		t.Fatalf("the not_allowed row does not list the reason opt_out_policy_unavailable: %s", row)
	}
	// the control: the check can fail
	if strings.Contains(strings.Replace(row, "opt_out_policy_unavailable", "x", 1), "opt_out_policy_unavailable") {
		t.Fatal("control failed: the reason is still found after it was taken out")
	}
}
