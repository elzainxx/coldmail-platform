package api

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// The two owner rulings of the Phase 4 close-out at the HTTP boundary, against the fake store (what only a database can show is shown by TestJobsAPI_* in tests/harness):
//
//	OD-4  the `unknown` choice of a chained send is explicit and never defaulted: the preview takes it in an optional body, the confirm takes it in its body, the store's two refusals have their
//	      own answers (422 unknown_choice_required with the count and the two allowed values, 409 preview_stale with the reason unknown_choice_changed), and the job and its preview show
//	      `null` until the user has chosen;
//	OD-3  a verify job's suppressed address is verified and charged, and the results (JSON and CSV) say it in a `suppressed` member that the older members do not move for.

func choiceOf(r *rig) (string, bool) {
	r.t.Helper()
	for _, c := range r.store.seen() {
		if c.Method == "Preview" {
			s, ok := c.Arg.(string)
			return s, ok
		}
	}
	r.t.Fatal("the store was not asked for a preview")
	return "", false
}

func TestPreview_TheOptionalBodyCarriesTheUnknownChoiceToTheStoreAndNothingElseDoes(t *testing.T) {
	r := newRig(t)
	path := "/v1/jobs/" + jobIDA + "/preview"
	for _, c := range []struct {
		name, body string
		want       string
		given      bool
	}{
		{"no body", "", "", false},
		{"a body of white space (the old clients' empty one)", " \n ", "", false},
		{"an empty object", `{}`, "", false},
		{"a null choice", `{"unknown_choice":null}`, "", false},
		{"include", `{"unknown_choice":"include"}`, "include", true},
		{"exclude", `{"unknown_choice":"exclude"}`, "exclude", true},
		// a value the API does not judge: the store's vocabulary is the one rule, so it must SEE what was sent (it answers a bad request itself)
		{"an empty string is a choice that is none of the two, not no choice", `{"unknown_choice":""}`, "", true},
		{"a word that is neither", `{"unknown_choice":"maybe"}`, "maybe", true},
	} {
		r.store.reset()
		resp, body := r.post(loginA, path, c.body)
		r.wantStatus(resp, body, 200)
		got, given := choiceOf(r)
		if got != c.want || given != c.given {
			t.Errorf("%s: the store was asked with choice %q (given %v), want %q (given %v)", c.name, got, given, c.want, c.given)
		}
	}

	// what is not a body of this route is refused before the store is asked
	for _, c := range []struct {
		name, body, ctype string
		status            int
		code              string
	}{
		{"a member the API does not know", `{"unknown_choice":"include","force":true}`, "application/json", 400, "bad_request"},
		{"a choice that is not a string", `{"unknown_choice":1}`, "application/json", 400, "bad_request"},
		{"not JSON", `unknown_choice=include`, "application/json", 400, "bad_request"},
		{"data after the value", `{"unknown_choice":"include"} x`, "application/json", 400, "bad_request"},
		{"a body that is not declared as JSON", `{"unknown_choice":"include"}`, "text/plain", 415, "unsupported_media_type"},
	} {
		r.store.reset()
		resp, body := r.do(req{method: http.MethodPost, path: path, token: r.token(loginA), ctype: c.ctype, body: []byte(c.body)})
		r.wantError(resp, body, c.status, c.code)
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called", c.name)
		}
	}
}

func TestConfirm_TheUnknownChoiceTravelsInTheBodyAndIsAbsentWhenNotSent(t *testing.T) {
	r := newRig(t)
	h := http.Header{"Idempotency-Key": {"key-1"}}
	for _, c := range []struct{ name, extra, want string }{
		{"no choice", ``, ""},
		{"include", `,"unknown_choice":"include"`, "include"},
		{"exclude", `,"unknown_choice":"exclude"`, "exclude"},
		{"a null choice", `,"unknown_choice":null`, ""},
	} {
		r.store.reset()
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(c.extra), h)
		r.wantStatus(resp, body, 200)
		if got := r.store.confirmR.UnknownChoice; got != c.want {
			t.Errorf("%s: the store was asked to confirm with choice %q, want %q", c.name, got, c.want)
		}
	}
	// a choice that is not a string is the body's fault and reaches nothing
	r.store.reset()
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/confirm", confirmBodyJSON(`,"unknown_choice":true`), h)
	r.wantError(resp, body, 400, "bad_request")
	if len(r.store.seen()) != 0 {
		t.Error("the store was called for a confirm whose choice is not a string")
	}
}

func TestConfirm_TheTwoRefusalsOfTheChoiceAreTheClientsAnswers(t *testing.T) {
	r := newRig(t)
	h := http.Header{"Idempotency-Key": {"key-1"}}
	path := "/v1/jobs/" + jobIDA + "/confirm"

	// no choice while the source holds unknown results: 422 with the count and the two allowed values, in the order a client shows them
	r.store.fail["Confirm"] = &jobs.UnknownChoiceRequiredError{Unknown: 7}
	resp, body := r.post(loginA, path, confirmBodyJSON(``), h)
	e := r.wantError(resp, body, 422, "unknown_choice_required")
	if e.Details["unknown_rows"] != float64(7) || !reflect.DeepEqual(e.Details["allowed"], []any{"include", "exclude"}) || len(e.Details) != 2 {
		t.Errorf("the details are %v, want unknown_rows 7 and allowed [include exclude]", e.Details)
	}
	if e.Message != messages["unknown_choice_required"] || strings.Contains(string(body), "jobs.") {
		t.Errorf("the message is %q (%s)", e.Message, body)
	}

	// a choice that is not the one the job was previewed under: 409 preview_stale, now saying why
	r.store.fail["Confirm"] = &jobs.StalePreviewError{Reason: jobs.ReasonUnknownChoiceChanged}
	resp, body = r.post(loginA, path, confirmBodyJSON(`,"unknown_choice":"include"`), h)
	e = r.wantError(resp, body, 409, "preview_stale")
	if e.Details["reason"] != "unknown_choice_changed" || len(e.Details) != 1 {
		t.Errorf("the details are %v, want the reason unknown_choice_changed", e.Details)
	}
	// THE CONTROL: a stale preview that has no reason of its own is the same code with no details
	r.store.fail["Confirm"] = jobs.ErrStalePreview
	resp, body = r.post(loginA, path, confirmBodyJSON(``), h)
	if e = r.wantError(resp, body, 409, "preview_stale"); e.Details != nil {
		t.Errorf("a bare stale preview has details %v", e.Details)
	}
}

func TestViews_TheUnknownChoiceIsNullUntilTheUserHasChosenAndEveryNewMemberIsShown(t *testing.T) {
	r := newRig(t)
	j := fullJob()
	j.Settings = jobs.Settings{} // nothing chosen
	j.Preview.UnknownCount, j.Preview.UnknownChoiceRequired, j.Preview.SuppressedStillVerified = 3, true, true
	r.store.job = j
	resp, body := r.get(loginA, "/v1/jobs/"+jobIDA)
	m := r.wantStatus(resp, body, 200)["job"].(map[string]any)
	if v, has := m["settings"].(map[string]any)["unknown_choice"]; !has || v != nil {
		t.Errorf("settings.unknown_choice = %v (present %v), want null", v, has)
	}
	pv := m["preview"].(map[string]any)
	if v, has := pv["unknown_choice"]; !has || v != nil {
		t.Errorf("preview.unknown_choice = %v (present %v), want null", v, has)
	}
	if pv["unknown_count"] != float64(3) || pv["unknown_choice_required"] != true || pv["suppressed_still_verified"] != true {
		t.Errorf("the new members of the preview are %v", pv)
	}

	// THE CONTROL: once the user has chosen, both say it
	j.Settings.UnknownChoice, j.Preview.UnknownChoice = jobs.UnknownInclude, jobs.UnknownInclude
	r.store.job = j
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA)
	m = r.wantStatus(resp, body, 200)["job"].(map[string]any)
	if m["settings"].(map[string]any)["unknown_choice"] != "include" || m["preview"].(map[string]any)["unknown_choice"] != "include" {
		t.Errorf("after the choice: settings %v, preview %v", m["settings"], m["preview"].(map[string]any)["unknown_choice"])
	}
}

func TestResultsCSV_TheSuppressedColumnIsTheLastAndTheOldOnesDidNotMove(t *testing.T) {
	r := newRig(t)
	r.store.results = []jobs.Result{
		{Ordinal: 1, Email: "a@example.test", Origin: "module", State: "valid", Decision: "charge", Unit: "m1_own", Rule: "M1-03", Suppressed: true, RecordedAt: t0},
		{Ordinal: 2, Email: "b@example.test", Origin: "module", State: "valid", Decision: "charge", Unit: "m1_own", Rule: "M1-03", RecordedAt: t0},
		{Ordinal: 3, Email: "c@example.test", Origin: "platform", State: "suppressed", Decision: "free", Rule: "P-SUP", Suppressed: true, RecordedAt: t0},
	}
	resp, body := r.csvOf("/v1/jobs/" + jobIDA + "/results.csv")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	cr := csv.NewReader(bytes.NewReader(body))
	cr.FieldsPerRecord = 9
	recs, err := cr.ReadAll()
	if err != nil || len(recs) != 4 {
		t.Fatalf("the file has %d records (%v), want the header and 3", len(recs), err)
	}
	want := [][]string{
		{"ordinal", "email", "origin", "state", "reason", "decision", "unit", "rule", "suppressed"},
		{"1", "a@example.test", "module", "valid", "", "charge", "m1_own", "M1-03", "true"},
		{"2", "b@example.test", "module", "valid", "", "charge", "m1_own", "M1-03", "false"},
		{"3", "c@example.test", "platform", "suppressed", "", "free", "", "P-SUP", "true"},
	}
	if !reflect.DeepEqual(recs, want) {
		t.Fatalf("the CSV is\n  %v\nwant\n  %v", recs, want)
	}
}
