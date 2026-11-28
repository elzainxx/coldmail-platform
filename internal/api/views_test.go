package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

func fullJob() jobs.Job {
	at := time.Date(2030, 1, 2, 5, 30, 0, 123, time.FixedZone("EET", 2*3600)) // a zone that is not UTC: every time must come out in UTC
	return jobs.Job{
		ID: jobIDA, UserID: userA, Module: "m2", Status: jobs.StatusScheduled, SourceJobID: otherJob,
		ScheduledStart: &at, StartDeadline: &at, PausedFrom: "scheduled",
		Preview:            &jobs.Preview{Rows: 5, Pending: 4, Malformed: 1, MalformedReasons: nil, Units: 4, Metered: true, RateUnit: "m1_syntax", UnitCost: 1, Credits: 4, Available: 10, Fingerprint: strings.Repeat("a", 64), ComputedAt: at},
		PreviewFingerprint: strings.Repeat("a", 64), ConfirmKey: strings.Repeat("c", 24),
		PreviewedAt: &at, ConfirmedAt: &at, StartedAt: &at, FinishedAt: &at, CancelRequestedAt: &at, ContentClearedAt: &at,
		Settings: jobs.Settings{UnknownChoice: "include"}, Content: &jobs.Content{Subject: "s", HTML: "h", Text: "t", ReplyTo: "r@example.test"},
		CreatedAt: at, UpdatedAt: at,
	}
}

func keysOfJSON(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every field of the store's types is either shown on purpose or left out on purpose, and a new field is neither until somebody says which: the lists below are that decision, held against
// the types by reflection and against the JSON by marshalling.
func TestViews_EveryFieldOfTheStoresTypesIsShownOrLeftOutOnPurpose(t *testing.T) {
	j := fullJob()
	shown := map[string]string{ // Go field -> JSON member
		"ID": "id", "Module": "module", "Status": "status", "SourceJobID": "source_job_id", "ScheduledStart": "scheduled_start", "StartDeadline": "start_deadline", "PausedFrom": "paused_from",
		"Preview": "preview", "PreviewFingerprint": "preview_fingerprint", "PreviewedAt": "previewed_at", "ConfirmedAt": "confirmed_at", "StartedAt": "started_at", "FinishedAt": "finished_at",
		"CancelRequestedAt": "cancel_requested_at", "ContentClearedAt": "content_cleared_at", "MergeFields": "merge_fields", "Settings": "settings", "Content": "content", "CreatedAt": "created_at", "UpdatedAt": "updated_at",
	}
	omitted := map[string]string{"UserID": "the owner: a client knows who it is, and the id of a user is not the client's to learn", "ConfirmKey": "the client's own idempotency key"}
	checkAccounted(t, reflect.TypeOf(jobs.Job{}), shown, omitted)

	view := viewJob(j, true)
	var want []string
	for _, m := range shown {
		want = append(want, m)
	}
	sort.Strings(want)
	if got := keysOfJSON(t, view); !reflect.DeepEqual(got, want) {
		t.Fatalf("a job is shown with the members\n  %v\nwant\n  %v", got, want)
	}
	raw, _ := json.Marshal(view)
	for _, banned := range []string{"user_id", "confirm_key", userA, strings.Repeat("c", 24)} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("a job shows %q: %s", banned, raw)
		}
	}
	// a list leaves the campaign out, and a verify job has none to show
	if got := keysOfJSON(t, viewJob(j, false)); contains(got, "content") {
		t.Errorf("a job in a list carries its campaign: %v", got)
	}
	j.Content = nil
	if got := keysOfJSON(t, viewJob(j, true)); contains(got, "content") {
		t.Errorf("a job with no campaign shows one: %v", got)
	}

	// every member of a preview the store computes is shown (the view embeds the type), so a new one is shown at once: the pinned list is how a reviewer sees it arrive
	pv := keysOfJSON(t, viewJob(fullJob(), true).Preview)
	wantPV := []string{"available", "computed_at", "credits", "duplicates", "excluded", "fingerprint", "malformed", "malformed_reasons", "metered", "missing_field_names", "missing_field_names_truncated", "missing_fields", "pending", "rate_unit", "rows", "selected", "shortfall", "suppressed", "suppressed_still_verified", "unit_cost", "units", "unknown_choice", "unknown_choice_required", "unknown_count", "unrenderable", "vendor_opt_out", "vendor_reachable_known"}
	if !reflect.DeepEqual(pv, wantPV) {
		t.Fatalf("a preview is shown with the members\n  %v\nwant\n  %v", pv, wantPV)
	}

	ev := map[string]string{"ID": "id", "Kind": "kind", "FromStatus": "from_status", "ToStatus": "to_status", "ActorKind": "actor_kind", "Reason": "reason", "CreatedAt": "created_at"}
	checkAccounted(t, reflect.TypeOf(jobs.Event{}), ev, map[string]string{
		"JobID": "the job is the one asked for", "ActingUserID": "the caller", "ActingAdminID": "an administrator is not the customer's to identify", "Detail": "the engine's own notes: internals, not part of v1",
	})
	var wantEv []string
	for _, m := range ev {
		wantEv = append(wantEv, m)
	}
	sort.Strings(wantEv)
	if got := keysOfJSON(t, viewEvent(jobs.Event{ID: 1, Kind: "transition", ActorKind: "user", CreatedAt: t0})); !reflect.DeepEqual(got, wantEv) {
		t.Fatalf("an event is shown with %v, want %v", got, wantEv)
	}

	res := map[string]string{"Ordinal": "ordinal", "Email": "email", "Origin": "origin", "State": "state", "Reason": "reason", "Decision": "decision", "Unit": "unit", "Rule": "rule", "Suppressed": "suppressed", "RecordedAt": "recorded_at"}
	checkAccounted(t, reflect.TypeOf(jobs.Result{}), res, nil)
	var wantRes []string
	for _, m := range res {
		wantRes = append(wantRes, m)
	}
	sort.Strings(wantRes)
	if got := keysOfJSON(t, viewResult(jobs.Result{Ordinal: 1, RecordedAt: t0})); !reflect.DeepEqual(got, wantRes) {
		t.Fatalf("a result is shown with %v, want %v", got, wantRes)
	}
}

// checkAccounted fails for a field of the type that is in neither list, and for a name in a list that is not a field (a list that outlived its field).
func checkAccounted(t *testing.T, rt reflect.Type, shown, omitted map[string]string) {
	t.Helper()
	have := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		have[name] = true
		_, s := shown[name]
		_, o := omitted[name]
		switch {
		case !s && !o:
			t.Errorf("%s.%s is neither shown nor left out on purpose: decide, in views_test.go and views.go", rt.Name(), name)
		case s && o:
			t.Errorf("%s.%s is both shown and left out", rt.Name(), name)
		}
	}
	for _, list := range []map[string]string{shown, omitted} {
		for name := range list {
			if !have[name] {
				t.Errorf("%s has no field %s: the list is out of date", rt.Name(), name)
			}
		}
	}
	if rt.NumField() < 5 {
		t.Errorf("%s has %d fields: the instrument looked at nothing", rt.Name(), rt.NumField())
	}
}

func TestViews_TimesAreUTCStringsAndWhatDoesNotApplyIsNull(t *testing.T) {
	v := viewJob(fullJob(), true)
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"scheduled_start", "start_deadline", "previewed_at", "confirmed_at", "started_at", "finished_at", "cancel_requested_at", "content_cleared_at", "created_at", "updated_at"} {
		s, _ := m[k].(string)
		if !strings.HasSuffix(s, "Z") || strings.Contains(s, "+") {
			t.Errorf("%s = %q, want an RFC 3339 time in UTC", k, s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Errorf("%s = %q does not parse: %v", k, s, err)
		}
	}
	if s := m["preview"].(map[string]any)["computed_at"]; s != "2030-01-02T03:30:00.000000123Z" {
		t.Errorf("the preview's time = %v", s)
	}
	if mr, ok := m["preview"].(map[string]any)["malformed_reasons"].(map[string]any); !ok || len(mr) != 0 {
		t.Errorf("malformed_reasons = %v, want an empty object (never null)", m["preview"].(map[string]any)["malformed_reasons"])
	}
	// a draft: every time that has not come is null, and so is what is not there
	d := viewJob(jobs.Job{ID: jobIDA, Module: "m1", Status: jobs.StatusDraft, CreatedAt: t0, UpdatedAt: t0}, true)
	raw, _ = json.Marshal(d)
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"source_job_id", "scheduled_start", "start_deadline", "paused_from", "preview", "preview_fingerprint", "previewed_at", "confirmed_at", "started_at", "finished_at", "cancel_requested_at", "content_cleared_at"} {
		if v, has := m[k]; !has || v != nil {
			t.Errorf("a draft's %s = %v (present %v), want null", k, v, has)
		}
	}
}

// ---- what each route answers --------------------------------------------------------------------------------------

func TestRoutes_TheMovesCallTheirStoreMethodForThePrincipalAndAnswerWithTheJob(t *testing.T) {
	r := newRig(t)
	r.store.job.Status = jobs.StatusCancelled
	for path, method := range map[string]string{
		"discard": "DiscardDraft", "reopen": "Reopen", "cancel": "Cancel", "pause": "Pause", "resume": "Resume",
	} {
		r.store.reset()
		resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/"+path, nil)
		m := r.wantStatus(resp, body, 200)
		if m["job"].(map[string]any)["status"] != "cancelled" || len(m) != 1 {
			t.Errorf("%s: %v", path, m)
		}
		if c := r.store.seen(); len(c) != 1 || c[0].Method != method || c[0].User != userA || c[0].Job != jobIDA {
			t.Errorf("%s: the store saw %+v, want %s for %s on %s", path, c, method, userA, jobIDA)
		}
		// each move's refusal reaches the client in its words
		r.store.fail[method] = &jobs.NotAllowedError{Reason: "job_module_cannot_" + map[string]string{"pause": "pause", "cancel": "cancel"}[path]}
		resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/"+path, nil)
		wantReason := map[string]string{"pause": "pause_not_supported", "cancel": "cancel_not_supported"}[path]
		if wantReason == "" {
			wantReason = "not_allowed"
		}
		e := r.wantError(resp, body, 409, "not_allowed")
		if e.Details["reason"] != wantReason {
			t.Errorf("%s: reason %v, want %s", path, e.Details, wantReason)
		}
		delete(r.store.fail, method)
	}
}

func TestRoutes_PreviewIsThePreviewAndThenTheJobAsItIsNow(t *testing.T) {
	r := newRig(t)
	j := fullJob()
	j.Status = jobs.StatusPreviewed
	r.store.job = j
	resp, body := r.post(loginA, "/v1/jobs/"+jobIDA+"/preview", nil)
	m := r.wantStatus(resp, body, 200)
	if m["job"].(map[string]any)["status"] != "previewed" || m["job"].(map[string]any)["preview"] == nil {
		t.Fatalf("%s", body)
	}
	if c := r.store.seen(); len(c) != 2 || c[0].Method != "Preview" || c[1].Method != "Get" || c[0].User != userA || c[1].User != userA {
		t.Fatalf("the store saw %+v, want Preview then Get for the principal", c)
	}
	// a preview the store refuses is not followed by a read
	r.store.reset()
	r.store.fail["Preview"] = &jobs.NotAllowedError{Reason: "job_status_has_no_preview"}
	resp, body = r.post(loginA, "/v1/jobs/"+jobIDA+"/preview", nil)
	e := r.wantError(resp, body, 409, "not_allowed")
	if e.Details["reason"] != "job_status_has_no_preview" || len(r.store.seen()) != 1 {
		t.Fatalf("%v, %+v", e.Details, r.store.seen())
	}
}

func TestRoutes_ListGetAndEventsAnswerInTheDocumentedShapes(t *testing.T) {
	r := newRig(t)
	a, b := fullJob(), fullJob()
	b.ID = otherJob
	r.store.list, r.store.next = []jobs.Job{a, b}, "next-page-cursor"
	resp, body := r.get(loginA, "/v1/jobs?limit=2&cursor=abc")
	m := r.wantStatus(resp, body, 200)
	jobsOut := m["jobs"].([]any)
	if len(jobsOut) != 2 || m["next_cursor"] != "next-page-cursor" {
		t.Fatalf("%s", body)
	}
	if _, has := jobsOut[0].(map[string]any)["content"]; has {
		t.Fatalf("a job in the list carries its campaign: %s", body)
	}
	if c := r.store.seen(); len(c) != 1 || c[0].Arg.(jobs.ListOpts) != (jobs.ListOpts{After: "abc", Limit: 2}) {
		t.Fatalf("the list was asked for %+v", c)
	}
	r.store.list, r.store.next = nil, ""
	resp, body = r.get(loginA, "/v1/jobs")
	m = r.wantStatus(resp, body, 200)
	if jl, ok := m["jobs"].([]any); !ok || len(jl) != 0 || m["next_cursor"] != nil {
		t.Fatalf("an empty list is %v", m)
	}

	r.store.job = fullJob()
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA)
	m = r.wantStatus(resp, body, 200)
	if m["job"].(map[string]any)["content"] == nil {
		t.Fatalf("the job of GET /v1/jobs/{id} has no campaign: %s", body)
	}

	// events: a page as long as the limit points at the next one by its last id; a short page does not
	evs := make([]jobs.Event, 3)
	for i := range evs {
		evs[i] = jobs.Event{ID: int64(10 + i), JobID: jobIDA, Kind: "transition", FromStatus: "draft", ToStatus: "previewed", ActorKind: "user", ActingUserID: userA, Reason: "preview requested by the user", Detail: []byte(`{"secret":"x"}`), CreatedAt: t0}
	}
	r.store.events = evs
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA+"/events?after=9&limit=3")
	m = r.wantStatus(resp, body, 200)
	if m["next_after"] != float64(12) || len(m["events"].([]any)) != 3 {
		t.Fatalf("%s", body)
	}
	for _, banned := range []string{userA, "acting", "detail", "secret", "job_id"} {
		if strings.Contains(string(body), banned) {
			t.Errorf("an event shows %q: %s", banned, body)
		}
	}
	resp, body = r.get(loginA, "/v1/jobs/"+jobIDA+"/events?limit=4")
	m = r.wantStatus(resp, body, 200)
	if m["next_after"] != nil {
		t.Fatalf("a short page points at a next one: %s", body)
	}
	if c := r.store.seen(); c[len(c)-1].Arg.([2]int64) != [2]int64{0, 4} {
		t.Fatalf("events were asked for %+v", c[len(c)-1])
	}
}

func TestNew_RefusesAMiswiringAtStartUp(t *testing.T) {
	r := newRig(t)
	ok := Deps{Service: r.store, Verifier: r.mint.Verifier(t), Resolver: r.res}
	if _, err := New(ok); err != nil {
		t.Fatalf("a complete wiring was refused: %v", err)
	}
	for name, d := range map[string]Deps{
		"no service":  {Verifier: ok.Verifier, Resolver: ok.Resolver},
		"no verifier": {Service: ok.Service, Resolver: ok.Resolver},
		"no resolver": {Service: ok.Service, Verifier: ok.Verifier},
		"a nil store": {Service: (*jobs.Store)(nil), Verifier: ok.Verifier, Resolver: ok.Resolver},
	} {
		if _, err := New(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
