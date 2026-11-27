package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// GET /v1/jobs?status=active (handlers.go; Phase 6 A3): an additive filter. The statuses it keeps and the SQL are the store's (internal/jobs/list_status_test.go and, against a real database,
// tests/harness/jobs_api_account_test.go); what the API owns is that the word reaches the store, that another word is refused before the store is asked, and that the cursor and the limit still work.

func TestListStatus_ActiveReachesTheStoreAndEverythingElseIsA400(t *testing.T) {
	r := newRig(t)
	r.store.list, r.store.next = []jobs.Job{fullJob()}, "next-page-cursor"
	resp, body := r.get(loginA, "/v1/jobs?status=active&limit=2&cursor=abc")
	m := r.wantStatus(resp, body, 200)
	if m["next_cursor"] != "next-page-cursor" || len(m["jobs"].([]any)) != 1 {
		t.Fatalf("the filtered list is %s", body)
	}
	if c := r.store.seen(); len(c) != 1 || c[0].User != userA || c[0].Arg.(jobs.ListOpts) != (jobs.ListOpts{After: "abc", Limit: 2, Status: "active"}) {
		t.Fatalf("the store was asked %+v, want the principal's list with the cursor, the limit and the status", c)
	}
	// the twins: no parameter and an empty one are the unfiltered list, exactly as before
	for _, q := range []string{"", "?status=", "?limit=2&cursor=abc"} {
		r.store.reset()
		resp, body := r.get(loginA, "/v1/jobs"+q)
		r.wantStatus(resp, body, 200)
		if c := r.store.seen(); len(c) != 1 || c[0].Arg.(jobs.ListOpts).Status != "" {
			t.Errorf("GET /v1/jobs%s: the store was asked %+v, want no filter", q, c)
		}
	}
	for _, bad := range []string{"ACTIVE", "Active", " active", "active ", "running", "paused", "all", "finished", "done", "active,paused", "%27", "1"} {
		r.store.reset()
		resp, body := r.get(loginA, "/v1/jobs?status="+strings.ReplaceAll(bad, " ", "%20"))
		e := r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
		if reason, _ := e.Details["reason"].(string); reason == "" || strings.Contains(string(body), "finished") || strings.Contains(string(body), "paused") {
			t.Errorf("status=%q: the reason is %q: %s", bad, reason, body)
		}
		if n := len(r.store.seen()); n != 0 {
			t.Errorf("status=%q: the store was asked %d time(s) for a value the API refuses", bad, n)
		}
	}
	// a status given twice is refused like any repeated parameter
	resp, body = r.get(loginA, "/v1/jobs?status=active&status=active")
	r.wantError(resp, body, http.StatusBadRequest, "bad_request")
	// the filter does not turn a bad limit or a user into something else
	resp, body = r.get(loginA, "/v1/jobs?status=active&limit=0")
	r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
	resp, body = r.get(loginA, "/v1/jobs?status=active&user_id="+userB)
	r.wantError(resp, body, http.StatusBadRequest, "bad_request")
}

// A store that refuses the status itself (the second wall) is the same 400 validation_failed: the mapping is the one function.
func TestListStatus_TheStoresRefusalIsTheSame400(t *testing.T) {
	r := newRig(t)
	r.store.fail["List"] = &jobs.BadRequestError{Reason: "status must be \"active\" or left out"}
	resp, body := r.get(loginA, "/v1/jobs")
	e := r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
	if e.Details["reason"] != "status must be \"active\" or left out" {
		t.Errorf("details = %v", e.Details)
	}
}
