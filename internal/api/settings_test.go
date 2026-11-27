package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/usersettings"
)

// The settings routes (settings.go; Phase 5 S6): GET and PUT /v1/settings. What the API owns here is that the user is the authenticated principal's and nothing else, that the body is judged as strictly
// as the job routes judge theirs, and that what the store answers is what the client is told. What the store does with the value (the row, its audit trail, the scope by user) is proved against a real
// PostgreSQL by tests/harness/usersettings_test.go.

// settingsCall is one call the fake settings store received.
type settingsCall struct {
	Method string
	User   string
	Value  bool // the value of a Set
}

// fakeSettings is the user settings store as the API sees it: a value per user, every call recorded with the user it was given, and an error to answer when told.
type fakeSettings struct {
	mu    sync.Mutex
	vals  map[string]bool
	calls []settingsCall
	fail  error
}

func newFakeSettings() *fakeSettings { return &fakeSettings{vals: map[string]bool{}} }

func (f *fakeSettings) Get(_ context.Context, user string) (usersettings.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, settingsCall{Method: "Get", User: user})
	if f.fail != nil {
		return usersettings.Settings{}, f.fail
	}
	return usersettings.Settings{VendorFallbackOptOut: f.vals[user]}, nil
}

func (f *fakeSettings) Set(_ context.Context, user string, v bool) (usersettings.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, settingsCall{Method: "Set", User: user, Value: v})
	if f.fail != nil {
		return usersettings.Settings{}, f.fail
	}
	f.vals[user] = v
	return usersettings.Settings{VendorFallbackOptOut: v}, nil
}

func (f *fakeSettings) seen() []settingsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]settingsCall(nil), f.calls...)
}

func (r *rig) put(login, path string, body string) (*http.Response, []byte) {
	r.t.Helper()
	return r.do(req{method: http.MethodPut, path: path, token: r.token(login), ctype: "application/json", body: []byte(body)})
}

func TestSettings_TheDefaultIsOptedInAndAPutIsThePrincipalsAlone(t *testing.T) {
	r := newRig(t)
	resp, body := r.get(loginA, "/v1/settings")
	m := r.wantStatus(resp, body, 200)
	if len(m) != 1 || m["vendor_fallback_opt_out"] != false {
		t.Fatalf("a user who never stated a setting reads %s, want exactly {\"vendor_fallback_opt_out\": false}: opted IN by default", body)
	}
	if got := r.settings.seen(); len(got) != 1 || got[0].User != userA {
		t.Fatalf("the store was asked %+v, want ONE read for the PLATFORM user of the login (%s), never the login's own id", got, userA)
	}

	resp, body = r.put(loginA, "/v1/settings", `{"vendor_fallback_opt_out": true}`)
	m = r.wantStatus(resp, body, 200)
	if len(m) != 1 || m["vendor_fallback_opt_out"] != true {
		t.Fatalf("the PUT answered %s, want the settings now in force: true", body)
	}
	if got := r.settings.seen(); len(got) != 2 || got[1] != (settingsCall{Method: "Set", User: userA, Value: true}) {
		t.Fatalf("the store was called %+v, want a Set of true for user A", got)
	}
	// the new value is what the next GET says, and ANOTHER user's GET still says the default (the control: the value is the principal's, not the service's)
	if resp, body := r.get(loginA, "/v1/settings"); r.wantStatus(resp, body, 200)["vendor_fallback_opt_out"] != true {
		t.Errorf("a GET after the PUT says %s", body)
	}
	if resp, body := r.get(loginB, "/v1/settings"); r.wantStatus(resp, body, 200)["vendor_fallback_opt_out"] != false {
		t.Errorf("user B reads %s: another user's choice must never be seen", body)
	}
	// and it can be taken back; the same value again is an answer, not an error (the store makes it idempotent)
	for i, v := range []string{"false", "false"} {
		resp, body := r.put(loginA, "/v1/settings", `{"vendor_fallback_opt_out": `+v+`}`)
		if r.wantStatus(resp, body, 200)["vendor_fallback_opt_out"] != false {
			t.Errorf("PUT %d of false answered %s", i, body)
		}
	}
	for _, c := range r.settings.seen() {
		if c.User != userA && c.User != userB {
			t.Errorf("the store was given the user %q", c.User)
		}
	}
}

func TestSettings_EveryRefusalOfAPutIsA400OrBetterAndWritesNothing(t *testing.T) {
	r := newRig(t)
	long := `{"vendor_fallback_opt_out": true, "x": "` + strings.Repeat("a", int(maxSmallBody)) + `"}`
	for _, c := range []struct {
		name    string
		body    string
		ctype   string
		path    string
		status  int
		code    string
		reasonH string // a word the reason says
	}{
		{"an unknown member", `{"vendor_fallback_opt_out": true, "colour": "red"}`, "application/json", "/v1/settings", 400, "bad_request", "colour"},
		{"a user named in the body", `{"vendor_fallback_opt_out": true, "user_id": "` + userB + `"}`, "application/json", "/v1/settings", 400, "bad_request", "user_id"},
		{"an owner named in the body", `{"owner": "` + userB + `", "vendor_fallback_opt_out": true}`, "application/json", "/v1/settings", 400, "bad_request", "owner"},
		{"a string where a boolean goes", `{"vendor_fallback_opt_out": "true"}`, "application/json", "/v1/settings", 400, "bad_request", "wrong type"},
		{"a number where a boolean goes", `{"vendor_fallback_opt_out": 1}`, "application/json", "/v1/settings", 400, "bad_request", "wrong type"},
		{"an object where a boolean goes", `{"vendor_fallback_opt_out": {"a": true}}`, "application/json", "/v1/settings", 400, "bad_request", "wrong type"},
		{"null", `{"vendor_fallback_opt_out": null}`, "application/json", "/v1/settings", 400, "validation_failed", "required"},
		{"the member missing", `{}`, "application/json", "/v1/settings", 400, "validation_failed", "required"},
		{"a body that is an array", `[true]`, "application/json", "/v1/settings", 400, "bad_request", ""},
		{"a body that is a bare boolean", `true`, "application/json", "/v1/settings", 400, "bad_request", ""},
		{"data after the object", `{"vendor_fallback_opt_out": true} {"vendor_fallback_opt_out": false}`, "application/json", "/v1/settings", 400, "bad_request", "after the JSON value"},
		{"garbage after the object", `{"vendor_fallback_opt_out": true} x`, "application/json", "/v1/settings", 400, "bad_request", "after the JSON value"},
		{"a body that is not JSON", `vendor_fallback_opt_out=true`, "application/json", "/v1/settings", 400, "bad_request", "not valid JSON"},
		{"a body that ends too soon", `{"vendor_fallback_opt_out": tru`, "application/json", "/v1/settings", 400, "bad_request", ""},
		{"an empty body", ``, "application/json", "/v1/settings", 400, "bad_request", "empty"},
		{"another content type", `{"vendor_fallback_opt_out": true}`, "text/plain", "/v1/settings", 415, "unsupported_media_type", ""},
		{"a form", `vendor_fallback_opt_out=true`, "application/x-www-form-urlencoded", "/v1/settings", 415, "unsupported_media_type", ""},
		{"no content type", `{"vendor_fallback_opt_out": true}`, "", "/v1/settings", 415, "unsupported_media_type", ""},
		{"a body over 64 KiB", long, "application/json", "/v1/settings", 413, "payload_too_large", ""},
		{"a query parameter", `{"vendor_fallback_opt_out": true}`, "application/json", "/v1/settings?user_id=" + userB, 400, "bad_request", "user_id"},
		{"a query parameter that is the field", `{"vendor_fallback_opt_out": true}`, "application/json", "/v1/settings?vendor_fallback_opt_out=true", 400, "bad_request", "vendor_fallback_opt_out"},
	} {
		resp, b := r.do(req{method: http.MethodPut, path: c.path, token: r.token(loginA), ctype: c.ctype, body: []byte(c.body)})
		e := r.wantError(resp, b, c.status, c.code)
		if reason, _ := e.Details["reason"].(string); c.reasonH != "" && !strings.Contains(reason, c.reasonH) {
			t.Errorf("%s: the reason is %q, want it to say %q", c.name, reason, c.reasonH)
		}
		if strings.Contains(string(b), userB) {
			t.Errorf("%s: the answer echoes the user id the request named: %s", c.name, b)
		}
	}
	if got := r.settings.seen(); len(got) != 0 {
		t.Fatalf("a refused PUT reached the store: %+v", got)
	}
	// the control: the same route, a good body, the same login: accepted, and the store sees it once
	if resp, b := r.put(loginA, "/v1/settings", `{"vendor_fallback_opt_out": true}`); resp.StatusCode != 200 {
		t.Fatalf("the control PUT answered %d: %s", resp.StatusCode, b)
	}
	if got := r.settings.seen(); len(got) != 1 || got[0].Method != "Set" {
		t.Errorf("after the control the store holds %+v, want exactly one Set", got)
	}
}

func TestSettings_AGetTakesNoQueryAndEveryRouteNeedsAToken(t *testing.T) {
	r := newRig(t)
	for _, q := range []string{"?user_id=" + userB, "?x=1", "?vendor_fallback_opt_out=true"} {
		resp, b := r.get(loginA, "/v1/settings"+q)
		r.wantError(resp, b, 400, "bad_request")
	}
	// without a token the store is never reached, for either verb
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		resp, b := r.do(req{method: m, path: "/v1/settings", ctype: "application/json", body: []byte(`{"vendor_fallback_opt_out": true}`)})
		r.wantError(resp, b, 401, "unauthenticated")
	}
	// a token that is not a login of the platform is a 401 too (the same answer as everywhere)
	resp, b := r.do(req{method: http.MethodGet, path: "/v1/settings", token: "not-a-token"})
	r.wantError(resp, b, 401, "unauthenticated")
	if got := r.settings.seen(); len(got) != 0 {
		t.Fatalf("a refused request reached the store: %+v", got)
	}
	// the control: a legal token reads
	if resp, _ := r.get(loginA, "/v1/settings"); resp.StatusCode != 200 {
		t.Errorf("a legal GET answered %d", resp.StatusCode)
	}
}

func TestSettings_AFailureOfTheStoreIsA500WithNothingOfItsText(t *testing.T) {
	r := newRig(t)
	r.settings.fail = errBoom
	for _, c := range []struct {
		name string
		do   func() (*http.Response, []byte)
	}{
		{"GET", func() (*http.Response, []byte) { return r.get(loginA, "/v1/settings") }},
		{"PUT", func() (*http.Response, []byte) {
			return r.put(loginA, "/v1/settings", `{"vendor_fallback_opt_out": true}`)
		}},
	} {
		resp, b := c.do()
		e := r.wantError(resp, b, 500, "internal_error")
		if e.Message != messages[codeInternal] || e.Details != nil || strings.Contains(string(b), "boom") {
			t.Errorf("%s: the answer %s carries the store's text or a detail", c.name, b)
		}
	}
	// the control: the same routes answer once the store does
	r.settings.fail = nil
	if resp, _ := r.get(loginA, "/v1/settings"); resp.StatusCode != 200 {
		t.Errorf("after the store recovered a GET answered %d", resp.StatusCode)
	}
}

func TestSettings_ServiceBuiltWithNoStoreDoesNotServeThePathAndTheJobRoutesAreUnaffected(t *testing.T) {
	r := newRig(t, withoutSettings())
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		resp, b := r.do(req{method: m, path: "/v1/settings", token: r.token(loginA), ctype: "application/json", body: []byte(`{"vendor_fallback_opt_out": true}`)})
		r.wantError(resp, b, 404, "not_found")
	}
	// the control: the job routes are served as ever
	if resp, _ := r.get(loginA, "/v1/jobs"); resp.StatusCode != 200 {
		t.Errorf("GET /v1/jobs answered %d with no settings store", resp.StatusCode)
	}
	// a typed nil store is no store either (a wiring slip must not become a panic at the first request)
	var none *usersettings.Store
	a, err := New(Deps{Service: r.store, Verifier: r.mint.Verifier(t), Resolver: r.res, Settings: none})
	if err != nil || a.settings != nil {
		t.Errorf("a nil *usersettings.Store was taken for a store: %v, %v", a, err)
	}
}

func TestViewJob_TheJobShowsWhetherItWasCreatedOptedOutOnAVerifyJobAndNeverOnASendJob(t *testing.T) {
	for _, c := range []struct {
		name   string
		module string
		optOut bool
		want   string // the JSON of settings
	}{
		{"a verify job created opted out", jobs.ModuleVerify, true, `{"unknown_choice":null,"vendor_opt_out":true}`},
		{"a verify job created opted in", jobs.ModuleVerify, false, `{"unknown_choice":null,"vendor_opt_out":false}`},
		{"a managed send job", jobs.ModuleSendManaged, false, `{"unknown_choice":null}`},
		{"a BYO send job", jobs.ModuleSendBYO, false, `{"unknown_choice":null}`},
	} {
		j := jobs.Job{ID: jobIDA, UserID: userA, Module: c.module, Status: jobs.StatusDraft, Settings: jobs.Settings{VendorOptOut: c.optOut}, CreatedAt: t0, UpdatedAt: t0}
		v := viewJob(j, true)
		b := mustJSON(t, v.Settings)
		if b != c.want {
			t.Errorf("%s: settings is %s, want %s", c.name, b, c.want)
		}
	}
	// a send job that somehow carries the key in its frozen settings (the database refuses it) still shows none
	j := jobs.Job{ID: jobIDA, UserID: userA, Module: jobs.ModuleSendBYO, Settings: jobs.Settings{VendorOptOut: true}}
	if b := mustJSON(t, viewJob(j, false).Settings); strings.Contains(b, "vendor_opt_out") {
		t.Errorf("a send job shows the key: %s", b)
	}
}

func TestDocs_JobsAPIDocumentsTheSettingsRoutesAndTheOptOutMembers(t *testing.T) {
	doc := readJobsDoc(t)
	for _, need := range []string{"`GET /v1/settings`", "`PUT /v1/settings`", "vendor_fallback_opt_out", "settings.vendor_opt_out", "preview.vendor_opt_out", "opt_out_policy_unavailable", "user_settings_history"} {
		if !strings.Contains(doc, need) {
			t.Errorf("Docs/jobs-api.md does not mention %s", need)
		}
	}
	// the control: the check can fail
	if strings.Contains(strings.ReplaceAll(doc, "vendor_fallback_opt_out", "x"), "vendor_fallback_opt_out") {
		t.Error("control failed: the member is still found after it was taken out")
	}
}

// mustJSON is v as a compact JSON string.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// reset forgets the calls recorded so far.
func (f *fakeSettings) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func TestCreate_ARequestCannotSetTheVendorOptOutOfAJobInAnyWay(t *testing.T) {
	r := newRig(t)
	good := `{"module":"m1","rows":[{"email":"a@example.test"}]`
	// the member in the job's own settings, at the top of the body, in a row and in the CSV query: all unknown members of this API
	for name, body := range map[string]string{
		"in settings":       good + `,"settings":{"vendor_opt_out":true}}`,
		"in settings false": good + `,"settings":{"vendor_opt_out":false}}`,
		"at the top":        good + `,"vendor_opt_out":true}`,
		"the setting's own": good + `,"vendor_fallback_opt_out":true}`,
		"in a row":          `{"module":"m1","rows":[{"email":"a@example.test","vendor_opt_out":true}]}`,
	} {
		resp, b := r.post(loginA, "/v1/jobs", body)
		e := r.wantError(resp, b, 400, "bad_request")
		if reason, _ := e.Details["reason"].(string); !strings.Contains(reason, "does not know") {
			t.Errorf("%s: the reason is %q, want an unknown member", name, reason)
		}
	}
	resp, b := r.postCSV(loginA, "?module=m1&vendor_opt_out=true", "email\na@example.test\n")
	r.wantError(resp, b, 400, "bad_request")
	if n := len(r.store.seen()); n != 0 {
		t.Fatalf("a refused create reached the store (%d calls)", n)
	}
	// the control: the same body without the member is a create, and the store is handed settings that say nothing of the vendor (the store reads the user's own setting; a request never carries it)
	resp, b = r.post(loginA, "/v1/jobs", good+`}`)
	r.wantStatus(resp, b, 201)
	if got := r.created(); got.Settings.VendorOptOut {
		t.Errorf("the create request reached the store with VendorOptOut set: %+v", got.Settings)
	}
}
