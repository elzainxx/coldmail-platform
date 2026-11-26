package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// The mapping of the store's errors to the envelope is ONE function (classify). This is its table: every sentinel and typed error of internal/jobs, as the store returns it (wrapped with the
// operation, "jobs.confirm: ..."), with the status, the code and the details a client must get, and nothing else in the details.

type mapping struct {
	name    string
	err     error
	status  int
	code    string
	details map[string]any // nil: the envelope has no details member
}

func wrapped(err error) error { return fmt.Errorf("jobs.confirm: %w", err) }

func mappingTable() []mapping {
	return []mapping{
		{"ErrNotFound", wrapped(jobs.ErrNotFound), 404, "not_found", nil},
		{"BadRequestError", wrapped(&jobs.BadRequestError{Reason: "the confirm key must be 1 to 128 characters"}), 400, "validation_failed", map[string]any{"reason": "the confirm key must be 1 to 128 characters"}},
		{"ErrBadRequest bare", wrapped(jobs.ErrBadRequest), 400, "validation_failed", map[string]any{"reason": "the request is not valid"}},
		{"TooManyRowsError", wrapped(&jobs.TooManyRowsError{Limit: 100000}), 413, "too_many_rows", map[string]any{"limit": float64(100000)}},
		{"ErrTooManyRows bare", wrapped(jobs.ErrTooManyRows), 413, "too_many_rows", nil},
		{"ErrNothingToDo", wrapped(jobs.ErrNothingToDo), 422, "nothing_to_do", nil},
		{"ErrStalePreview", wrapped(jobs.ErrStalePreview), 409, "preview_stale", nil},
		// owner ruling OD-4: the unknown choice of a chained send. A stale preview that knows its reason says it; the missing choice is a 422 that names how many results are unknown and the two values.
		{"StalePreviewError, unknown_choice_changed", wrapped(&jobs.StalePreviewError{Reason: jobs.ReasonUnknownChoiceChanged}), 409, "preview_stale", map[string]any{"reason": "unknown_choice_changed"}},
		{"StalePreviewError with no reason", wrapped(&jobs.StalePreviewError{}), 409, "preview_stale", nil},
		{"UnknownChoiceRequiredError", wrapped(&jobs.UnknownChoiceRequiredError{Unknown: 3}), 422, "unknown_choice_required", map[string]any{"unknown_rows": float64(3), "allowed": []any{"include", "exclude"}}},
		{"ErrUnknownChoiceRequired bare", wrapped(jobs.ErrUnknownChoiceRequired), 422, "unknown_choice_required", map[string]any{"allowed": []any{"include", "exclude"}}},
		{"ErrSlotTaken", wrapped(jobs.ErrSlotTaken), 409, "job_slot_taken", nil},
		{"ErrConfirmKeyReused", wrapped(jobs.ErrConfirmKeyReused), 409, "idempotency_key_reused", nil},
		{"ErrNotPreviewed", wrapped(jobs.ErrNotPreviewed), 409, "not_previewed", nil},
		{"ErrInsufficientCredits", wrapped(jobs.ErrInsufficientCredits{Available: 40, Needed: 100, Shortfall: 60}), 402, "insufficient_credits",
			map[string]any{"available": float64(40), "needed": float64(100), "shortfall": float64(60)}},
		{"ErrNoEntitlement", wrapped(jobs.ErrNoEntitlement), 403, "no_entitlement", nil},
		{"ErrSuspended", wrapped(jobs.ErrSuspended), 403, "suspended", nil},
		{"ErrInfeasible", wrapped(jobs.ErrInfeasible{Causes: []string{"no_active_accounts", "daily_cap_zero"}}), 409, "infeasible", map[string]any{"causes": []any{"no_active_accounts", "daily_cap_zero"}}},
		{"ErrInfeasible with no cause", wrapped(jobs.ErrInfeasible{}), 409, "infeasible", map[string]any{"causes": []any{}}},
		{"ErrModuleUnavailable", wrapped(jobs.ErrModuleUnavailable), 503, "module_unavailable", nil},
		{"ErrNotAllowed, the module cannot pause", wrapped(&jobs.NotAllowedError{Reason: "job_module_cannot_pause"}), 409, "not_allowed", map[string]any{"reason": "pause_not_supported"}},
		{"ErrNotAllowed, the module cannot cancel", wrapped(&jobs.NotAllowedError{Reason: "job_module_cannot_cancel"}), 409, "not_allowed", map[string]any{"reason": "cancel_not_supported"}},
		{"ErrNotAllowed, a constraint with no word", wrapped(&jobs.NotAllowedError{Reason: "job_transition_illegal"}), 409, "not_allowed", map[string]any{"reason": "not_allowed"}},
		{"ErrNotAllowed, a word of the store's", wrapped(&jobs.NotAllowedError{Reason: "not_a_draft"}), 409, "not_allowed", map[string]any{"reason": "not_a_draft"}},
		{"ErrNotAllowed bare", wrapped(jobs.ErrNotAllowed), 409, "not_allowed", map[string]any{"reason": "not_allowed"}},
		{"money.ErrLockTimeout", wrapped(&money.LockTimeoutError{}), 503, "busy", nil},
		{"auth.ErrKeysUnavailable", wrapped(auth.ErrKeysUnavailable), 503, "auth_unavailable", nil},
		// what no client route can cause: the engine's sentinels, and anything else, are the server's
		{"ErrIncomplete (engine only)", wrapped(&jobs.IncompleteError{Missing: 3}), 500, "internal_error", nil},
		{"ErrModuleRefTaken (engine only)", wrapped(jobs.ErrModuleRefTaken), 500, "internal_error", nil},
		{"ErrPagerStuck", wrapped(jobs.ErrPagerStuck), 500, "internal_error", nil},
		{"a plain error", errors.New("the database is gone"), 500, "internal_error", nil},
		{"a database error", wrapped(&pgconn.PgError{Code: "XX000", Message: "boom", ConstraintName: "jobs_secret_fkey", Detail: "Key (email)=(alice@example.test)"}), 500, "internal_error", nil},
	}
}

func TestClassify_EverySentinelIsMappedToItsStatusCodeAndDetails(t *testing.T) {
	table := mappingTable()
	r := newRig(t)
	for _, m := range table {
		r.store.fail["Get"] = m.err
		resp, body := r.get(loginA, "/v1/jobs/"+jobIDA)
		e := r.wantError(resp, body, m.status, m.code)
		if e.Message != messages[m.code] {
			t.Errorf("%s: message %q, want the fixed %q", m.name, e.Message, messages[m.code])
		}
		if fmt.Sprint(e.Details) != fmt.Sprint(m.details) && !(len(e.Details) == 0 && len(m.details) == 0) {
			t.Errorf("%s: details %v, want %v", m.name, e.Details, m.details)
		}
		if m.details == nil && e.Details != nil {
			t.Errorf("%s: the envelope carries details %v although the code has none to give", m.name, e.Details)
		}
		// nothing of the Go error, the constraint or the row ever reaches the client
		for _, leak := range []string{"jobs.confirm", "jobs_secret_fkey", "alice@example.test", "the database is gone", "pgconn", "Key (email)"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s: the answer carries %q: %s", m.name, leak, body)
			}
		}
	}
	// the control: a store that answers properly is not an error
	delete(r.store.fail, "Get")
	if resp, body := r.get(loginA, "/v1/jobs/"+jobIDA); resp.StatusCode != http.StatusOK {
		t.Fatalf("a store with no error was answered %d: %s", resp.StatusCode, body)
	}
}

// Every sentinel and every typed error declared in internal/jobs/errors.go has a row in the table above. A new one makes this fail until somebody decides what the client is told (or that it is
// the server's fault), which is the point: the mapping cannot silently fall back to a 500 for a refusal the client causes.
func TestClassify_TheTableNamesEverySentinelOfTheStore(t *testing.T) {
	src, err := os.ReadFile("../jobs/errors.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+(Err[A-Za-z]+)\s+=\s+errors\.New`).FindAllStringSubmatch(string(src), -1) {
		declared[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^type (Err[A-Za-z]+) `).FindAllStringSubmatch(string(src), -1) {
		declared[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^type ([A-Za-z]+Error) struct`).FindAllStringSubmatch(string(src), -1) {
		declared[m[1]] = true
	}
	if len(declared) < 17 {
		t.Fatalf("found %d sentinels and typed errors in internal/jobs/errors.go, want at least 17: the instrument matched (almost) nothing", len(declared))
	}
	named := ""
	for _, m := range mappingTable() {
		named += " " + m.name + " "
	}
	// the typed errors are named in the table by their Go type; the sentinels by theirs
	alias := map[string]string{"IncompleteError": "ErrIncomplete", "NotAllowedError": "ErrNotAllowed"} // a typed error is named in the table by the sentinel it matches
	var missing []string
	for name := range declared {
		want := name
		if a, ok := alias[name]; ok {
			want = a
		}
		if !strings.Contains(named, want) {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		t.Errorf("internal/jobs declares %v, which the mapping table of internal/api does not name: say what a client is told for each (errors_test.go, mappingTable)", missing)
	}
	// the control: the check can fail (a name the table does not have is reported)
	if strings.Contains(named, " ErrInventedForTheControl ") {
		t.Fatal("the control name is in the table")
	}
}

func TestClassify_ANotAllowedReasonIsAWordAndNeverAConstraintName(t *testing.T) {
	for reason, want := range map[string]string{
		"job_module_cannot_pause":               "pause_not_supported",
		"job_module_cannot_cancel":              "cancel_not_supported",
		"job_transition_illegal":                "not_allowed",
		"job_transition_actor_not_allowed":      "not_allowed",
		"job_transition_needs_an_actor":         "not_allowed",
		"jobs_terminal_is_final":                "not_allowed",
		"job_resume_to_where_it_paused_from":    "not_allowed",
		"restricted":                            "not_allowed",
		"a reason nobody has heard of":          "not_allowed",
		"":                                      "not_allowed",
		"not_a_draft":                           "not_a_draft",
		"already_a_draft":                       "already_a_draft",
		"not_confirmed":                         "not_confirmed",
		"not_paused":                            "not_paused",
		"start_deadline_passed":                 "start_deadline_passed",
		"submitted_job_ends_through_the_engine": "submitted_job_ends_through_the_engine",
		"job_status_has_no_preview":             "job_status_has_no_preview",
	} {
		got := notAllowedWord(reason)
		if got != want {
			t.Errorf("reason %q is told as %q, want %q", reason, got, want)
		}
		// a constraint's name (and a name nobody listed) is never what a client is told: only the two translations and "not_allowed"
		if (strings.HasPrefix(reason, "job_") || strings.HasPrefix(reason, "jobs_")) && reason != "job_status_has_no_preview" {
			if got != "pause_not_supported" && got != "cancel_not_supported" && got != "not_allowed" {
				t.Errorf("reason %q reached the client as %q, which is not a word of the API's", reason, got)
			}
		}
	}
}

func TestErrors_ReasonsAndCausesNeverCarryAnAddress(t *testing.T) {
	r := newRig(t)
	// the store promises not to quote an address; the API does not rely on it
	r.store.fail["Get"] = &jobs.BadRequestError{Reason: "row 3: alice@example.test is not valid"}
	resp, body := r.get(loginA, "/v1/jobs/"+jobIDA)
	e := r.wantError(resp, body, 400, "validation_failed")
	if strings.Contains(string(body), "alice@") || e.Details["reason"] != "the request is not valid" {
		t.Fatalf("a reason with an address reached the client: %s", body)
	}
	causes := []string{"no_active_accounts", "mailbox bob@example.test is full"}
	for i := 0; i < 30; i++ {
		causes = append(causes, fmt.Sprintf("cause_%d", i))
	}
	causes = append(causes, strings.Repeat("x", 500))
	r.store.fail["Get"] = jobs.ErrInfeasible{Causes: causes}
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA)
	e = r.wantError(resp, body, 409, "infeasible")
	got, _ := e.Details["causes"].([]any)
	if strings.Contains(string(body), "bob@") || len(got) != 20 || got[0] != "no_active_accounts" {
		t.Fatalf("the causes were not cleaned: %d of them, %s", len(got), body)
	}
	// the control: a clean reason and clean causes pass through as they are
	r.store.fail["Get"] = &jobs.BadRequestError{Reason: "row 3: merge fields hold 5000 bytes, the limit is 4096"}
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA)
	e = r.wantError(resp, body, 400, "validation_failed")
	if e.Details["reason"] != "row 3: merge fields hold 5000 bytes, the limit is 4096" {
		t.Fatalf("a clean reason was changed: %v", e.Details)
	}
}

func TestErrors_AnUnexpectedErrorIsA500WithNoDetailAndItsCauseIsInTheLogUnderTheRequestID(t *testing.T) {
	r := newRig(t)
	r.store.fail["Get"] = fmt.Errorf("jobs.get: %w", &pgconn.PgError{Code: "23514", Message: "new row violates check constraint \"job_rows_x\" for alice@example.test", ConstraintName: "job_rows_x"})
	resp, body := r.get(loginA, "/v1/jobs/"+jobIDA)
	e := r.wantError(resp, body, 500, "internal_error")
	if e.Details != nil || e.Message != "An unexpected error occurred." || strings.Contains(string(body), "job_rows_x") {
		t.Fatalf("the cause reached the client: %s", body)
	}
	var line map[string]any
	for _, l := range r.logs.lines(t) {
		if l["msg"] == "request failed" {
			line = l
		}
	}
	if line == nil || line["request_id"] != resp.Header.Get("X-Request-Id") || line["sqlstate"] != "23514" || line["constraint"] != "job_rows_x" || line["level"] != "ERROR" {
		t.Fatalf("the log line is %v", line)
	}
	if strings.Contains(r.logs.String(), "alice@") {
		t.Fatalf("an address is in the log: %s", r.logs.String())
	}
	// the control: a refusal the client caused is not logged as a fault
	r.logs.b.Reset()
	r.store.fail["Get"] = jobs.ErrNotFound
	r.get(loginA, "/v1/jobs/"+jobIDA)
	for _, l := range r.logs.lines(t) {
		if l["msg"] == "request failed" {
			t.Fatalf("a 404 was logged as a failure: %v", l)
		}
	}
}

func TestErrors_AJobThatCannotExistIs404WithoutAskingTheStoreAndAnotherUsersJobIsTheSameAnswer(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{"not-a-uuid", "0B000000-0000-4000-8000-000000000001", jobIDA + "0", "%20", "..%2f..%2fetc"} {
		resp, body := r.get(loginA, "/v1/jobs/"+id)
		r.wantError(resp, body, 404, "not_found")
	}
	if n := len(r.store.seen()); n != 0 {
		t.Fatalf("the store was asked %d time(s) about ids that cannot be jobs", n)
	}
	// a job the store does not find for this user (missing, or another user's: the store cannot tell, and neither can the answer)
	r.store.fail["Get"] = jobs.ErrNotFound
	resp1, body1 := r.get(loginA, "/v1/jobs/"+missingID)
	resp2, body2 := r.get(loginA, "/v1/jobs/"+otherJob)
	r.wantError(resp1, body1, 404, "not_found")
	r.wantError(resp2, body2, 404, "not_found")
	if sameBody(body1) != sameBody(body2) {
		t.Fatalf("a missing job and another user's job are answered differently:\n%s\n%s", body1, body2)
	}
}
