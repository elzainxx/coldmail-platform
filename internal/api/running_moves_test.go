package api

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D1 (platform_0016): the three moves on a RUNNING job, at the HTTP boundary, against the fake store. What the database decides (which module can pause, which move is legal) is in
// tests/harness/jobs_api_test.go over a real database; here the route calls ITS store method for the verified principal, answers with the job as the store left it (paused from running, running again,
// cancelling), and says every refusal of the lifecycle in the API's words, never a constraint's name.

// runningJob is a job of module 2 as the store leaves it after the move named by status.
func runningJob(status string) jobs.Job {
	j := fullJob()
	j.Status, j.PausedFrom, j.CancelRequestedAt, j.FinishedAt, j.ScheduledStart = status, "", nil, nil, nil
	switch status {
	case jobs.StatusPaused:
		j.PausedFrom = jobs.StatusRunning
	case jobs.StatusCancelling:
		at := j.CreatedAt
		j.CancelRequestedAt = &at
	}
	return j
}

func TestRoutes_PauseResumeAndCancelOfARunningJobAnswerWithTheJobAsTheStoreLeavesItAndCallOnlyTheirOwnMethod(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		route, method, status, from string
		cancelRequested             bool
	}{
		{"pause", "Pause", jobs.StatusPaused, "running", false},
		{"resume", "Resume", jobs.StatusRunning, "", false},
		{"cancel", "Cancel", jobs.StatusCancelling, "", true}, // a running job, or a job paused while running: the answer is the request, the engine ends the job
	} {
		r.store.reset()
		r.store.job = runningJob(c.status)
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/"+c.route, nil)
		m := r.wantStatus(resp, body, 200)
		job, _ := m["job"].(map[string]any)
		if len(m) != 1 || job == nil || job["status"] != c.status {
			t.Fatalf("%s: the answer is %s, want only the job, %s", c.route, body, c.status)
		}
		if c.from == "" && job["paused_from"] != nil || c.from != "" && job["paused_from"] != c.from {
			t.Errorf("%s: paused_from is %v, want %q", c.route, job["paused_from"], c.from)
		}
		if got := job["cancel_requested_at"] != nil; got != c.cancelRequested {
			t.Errorf("%s: cancel_requested_at present = %v, want %v", c.route, got, c.cancelRequested)
		}
		if calls := r.store.seen(); len(calls) != 1 || calls[0].Method != c.method || calls[0].User != userA || calls[0].Job != jobIDA {
			t.Errorf("%s: the store saw %+v, want exactly %s for %s on %s (the principal, never the body)", c.route, calls, c.method, userA, jobIDA)
		}
	}
	// the control: a route of the same family that is NOT one of the three never reaches these methods
	r.store.reset()
	r.store.job = runningJob(jobs.StatusRunning)
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/discard", nil)
	r.wantStatus(resp, body, 200)
	if calls := r.store.seen(); len(calls) != 1 || calls[0].Method != "DiscardDraft" {
		t.Errorf("discard reached %+v", calls)
	}
}

// Every refusal the database can give to these moves of a running job is a 409 not_allowed in the API's words (pause_not_supported for a module that cannot pause, cancel_not_supported for one that cannot
// cancel (reserved: no seed reaches it), submitted_job_ends_through_the_engine for the wall of a job the module holds, the plain not_allowed for the rest); another user's job is the 404 of a job that
// does not exist. No answer holds a constraint's name or an address, and a refusal is the same for the three routes whichever user asks.
func TestRoutes_TheRefusalsOfTheMovesOfARunningJobAreInTheAPIsWordsAndNeverAConstraintName(t *testing.T) {
	r := newRig(t)
	routes := map[string]string{"pause": "Pause", "resume": "Resume", "cancel": "Cancel"}
	for _, c := range []struct{ route, reason, word string }{
		{"pause", "job_module_cannot_pause", "pause_not_supported"},
		{"cancel", "job_module_cannot_cancel", "cancel_not_supported"},
		{"cancel", jobs.ReasonSubmittedJobCancelsThroughTheEngine, "submitted_job_ends_through_the_engine"},
		{"cancel", "job_unsubmitted_pause_never_cancelling", "not_allowed"},
		{"pause", "job_transition_illegal", "not_allowed"},
		{"resume", "not_paused", "not_paused"},
		{"resume", "job_resume_to_where_it_paused_from", "not_allowed"},
		{"resume", "start_deadline_passed", "start_deadline_passed"},
	} {
		method := routes[c.route]
		r.store.fail[method] = &jobs.NotAllowedError{Reason: c.reason}
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/"+c.route, nil)
		e := r.wantError(resp, body, 409, "not_allowed")
		if e.Details["reason"] != c.word {
			t.Errorf("%s: a refusal named %s is told as %v, want %s", c.route, c.reason, e.Details["reason"], c.word)
		}
		if s := string(body); strings.Contains(s, "job_module_") || strings.Contains(s, "job_submitted_") || strings.Contains(s, "job_unsubmitted_") || strings.Contains(s, "job_resume_") || strings.Contains(s, "job_transition_") {
			t.Errorf("%s: the answer holds a constraint's name: %s", c.route, s)
		}
		delete(r.store.fail, method)
	}
	// another user's job, and a job that is not there, are the same 404 for all three
	for route, method := range routes {
		r.store.fail[method] = jobs.ErrNotFound
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/"+route, nil)
		r.wantError(resp, body, 404, "not_found")
		delete(r.store.fail, method)
	}
	// the control: with nothing failing the same requests are 200s
	r.store.job = runningJob(jobs.StatusPaused)
	for route := range routes {
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/"+route, nil)
		r.wantStatus(resp, body, 200)
	}
}

// A request without a token, or with one for a user the platform will not act for, never reaches the store for these routes either (the same wall as every route), and the method is POST only.
func TestRoutes_TheMovesOfARunningJobNeedTheVerifiedPrincipalAndAreNotGettable(t *testing.T) {
	r := newRig(t)
	for _, route := range []string{"pause", "resume", "cancel"} {
		r.store.reset()
		resp, body := r.do(req{method: "POST", path: "/v1/jobs/" + jobIDA + "/" + route, ctype: "application/json"})
		r.wantError(resp, body, 401, "unauthenticated")
		if resp, body := r.get(loginA, "/v1/jobs/"+jobIDA+"/"+route); resp.StatusCode != 405 && resp.StatusCode != 404 {
			t.Errorf("GET %s answered %d: %s", route, resp.StatusCode, body)
		}
		if calls := r.store.seen(); len(calls) != 0 {
			t.Errorf("%s: the store was reached without a verified principal: %+v", route, calls)
		}
	}
}
