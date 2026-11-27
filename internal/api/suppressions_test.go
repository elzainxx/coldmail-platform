package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/norm"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// The suppression routes (suppressions.go) over a fake of the list that keeps one list PER USER, as the real store scopes its statements by user: a request that reaches the fake with the wrong user would
// read, write or remove somebody else's entry, and the tests below look at exactly which user the fake was given and what each user's list holds afterwards. The fake runs the REAL address rule and the
// REAL CSV import (suppress.Add, suppress.Remove and suppress.ImportCSV over a connection that accepts every statement), so a refusal is the store's own refusal, with the store's own error type. What only a
// database can show (the SQL itself, the history trigger) is in tests/harness/suppress_removal_test.go.

type fakeSuppress struct {
	mu      sync.Mutex
	calls   []call
	fail    map[string]error
	lists   map[string]map[string]suppress.Entry // user -> normalised address -> entry
	next    string
	listed  []suppress.Entry // when set, List answers this instead of the user's list
	imports int
}

func newFakeSuppress() *fakeSuppress {
	return &fakeSuppress{fail: map[string]error{}, lists: map[string]map[string]suppress.Entry{}}
}

func (f *fakeSuppress) rec(method, user string, arg any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Method: method, User: user, Arg: arg})
	return f.fail[method]
}

func (f *fakeSuppress) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeSuppress) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// put puts an entry on a user's list directly (the arrangement of a test).
func (f *fakeSuppress) put(user, email, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists[user] == nil {
		f.lists[user] = map[string]suppress.Entry{}
	}
	f.lists[user][email] = suppress.Entry{Email: email, Source: source, CreatedAt: t0}
}

func (f *fakeSuppress) has(user, email string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.lists[user][email]
	return ok
}

type listArg struct {
	Query, Cursor string
	Limit         int
}

func (f *fakeSuppress) List(_ context.Context, user, query, cursor string, limit int) ([]suppress.Entry, string, error) {
	if err := f.rec("List", user, listArg{query, cursor, limit}); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listed != nil {
		return f.listed, f.next, nil
	}
	var out []suppress.Entry
	for _, e := range f.lists[user] {
		if strings.Contains(e.Email, strings.ToLower(query)) {
			out = append(out, e)
		}
	}
	return out, f.next, nil
}

func (f *fakeSuppress) Add(ctx context.Context, user, email string) (suppress.Entry, bool, error) {
	if err := f.rec("Add", user, email); err != nil {
		return suppress.Entry{}, false, err
	}
	if _, err := suppress.Add(ctx, nil, user, email, suppress.SourceManual); err != nil && !strings.Contains(err.Error(), "no database connection") {
		return suppress.Entry{}, false, err // the store's own refusal of the address
	}
	normal, _ := norm.Validate(email)
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.lists[user][normal]; ok {
		return e, false, nil
	}
	if f.lists[user] == nil {
		f.lists[user] = map[string]suppress.Entry{}
	}
	e := suppress.Entry{Email: normal, Source: suppress.SourceManual, CreatedAt: t0}
	f.lists[user][normal] = e
	return e, true, nil
}

func (f *fakeSuppress) Remove(ctx context.Context, user, email string) (bool, error) {
	if err := f.rec("Remove", user, email); err != nil {
		return false, err
	}
	if _, err := suppress.Remove(ctx, nil, user, email); err != nil && !strings.Contains(err.Error(), "no database connection") {
		return false, err
	}
	normal, _ := norm.Validate(email)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.lists[user][normal]; !ok {
		return false, nil
	}
	delete(f.lists[user], normal)
	return true, nil
}

// acceptAllExec is a connection that accepts every batch of an import as new and remembers the addresses.
type acceptAllExec struct {
	user   string
	source string
	got    []string
}

func (e *acceptAllExec) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	e.user, _ = args[0].(string)
	e.source, _ = args[2].(string)
	list, _ := args[1].([]string)
	e.got = append(e.got, list...)
	return pgconn.NewCommandTag("INSERT 0 " + strconv.Itoa(len(list))), nil
}

func (f *fakeSuppress) Import(ctx context.Context, user string, body io.Reader, maxRows int) (suppress.ImportResult, error) {
	if err := f.rec("Import", user, maxRows); err != nil {
		return suppress.ImportResult{}, err
	}
	x := &acceptAllExec{}
	res, err := suppress.ImportCSV(ctx, x, user, body, maxRows)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports++
	if f.lists[user] == nil {
		f.lists[user] = map[string]suppress.Entry{}
	}
	for _, e := range x.got {
		f.lists[user][e] = suppress.Entry{Email: e, Source: x.source, CreatedAt: t0}
	}
	return res, err
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// serveOnce answers a request with no token against a mux and returns the status: 404 when the path is not served, 401 when it is (behind the authentication).
func serveOnce(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// ---- the tests -------------------------------------------------------------------------------------------------------------

func csvReq(r *rig, login, path, body string) (*http.Response, []byte) {
	return r.do(req{method: http.MethodPost, path: path, token: r.token(login), ctype: "text/csv", body: []byte(body)})
}

func TestSuppressions_ListIsTheCallersOwnAndNamesNoUser(t *testing.T) {
	r := newRig(t)
	r.supp.put(userA, "alice-list@example.com", "manual")
	r.supp.put(userB, "bob-list@example.com", "unsubscribe")
	for login, want := range map[string]struct{ user, mine, theirs string }{
		loginA: {userA, "alice-list@example.com", "bob-list@example.com"},
		loginB: {userB, "bob-list@example.com", "alice-list@example.com"},
	} {
		r.supp.reset()
		resp, body := r.get(login, "/v1/suppressions")
		m := r.wantStatus(resp, body, 200)
		entries, _ := m["entries"].([]any)
		if len(entries) != 1 || m["next_cursor"] != nil {
			t.Fatalf("%s: the list is %s", want.user, body)
		}
		e := entries[0].(map[string]any)
		if e["email"] != want.mine || e["source"] == "" || e["created_at"] != stamp(t0) || len(e) != 3 {
			t.Fatalf("%s: the entry is %v, want the caller's own with exactly email, source and created_at", want.user, e)
		}
		if strings.Contains(string(body), want.theirs) {
			t.Fatalf("%s: the answer holds the other user's address: %s", want.user, body)
		}
		if c := r.supp.seen(); len(c) != 1 || c[0].User != want.user {
			t.Fatalf("the store was asked as %v, want only %s (never the login id)", c, want.user)
		}
	}
	// a request that names a user is refused as an unknown parameter and never reaches the store (the control is the same request without it)
	r.supp.reset()
	resp, body := r.get(loginA, "/v1/suppressions?user_id="+userB)
	e := r.wantError(resp, body, 400, "bad_request")
	if !strings.Contains(e.Details["reason"].(string), "user_id") || len(r.supp.seen()) != 0 {
		t.Fatalf("a named user: %s after %d store call(s)", body, len(r.supp.seen()))
	}
	if resp, _ := r.get(loginA, "/v1/suppressions"); resp.StatusCode != 200 {
		t.Fatalf("control: %d", resp.StatusCode)
	}
}

func TestSuppressions_ListPassesTheSearchAndTheCursorAsTheyAreAndBoundsTheLimit(t *testing.T) {
	r := newRig(t)
	r.supp.listed = []suppress.Entry{{Email: "a@example.com", Source: "import", CreatedAt: t0}}
	r.supp.next = "opaque-next"
	// the search text is data, however hostile: it reaches the store as the one parameter
	for _, q := range []string{"%", "_", `\`, "a@ex", "'; DROP TABLE x; --", "%25%5F"} {
		r.supp.reset()
		resp, body := r.get(loginA, "/v1/suppressions?q="+urlEscape(q)+"&cursor=c1&limit=7")
		m := r.wantStatus(resp, body, 200)
		if m["next_cursor"] != "opaque-next" {
			t.Fatalf("%q: next_cursor %v", q, m["next_cursor"])
		}
		c := r.supp.seen()
		if len(c) != 1 || c[0].Arg != (listArg{q, "c1", 7}) {
			t.Fatalf("%q: the store was given %+v", q, c)
		}
	}
	// the limit: 1 to 100; anything else is a 400 that never reaches the store; the controls are the two ends
	for _, bad := range []string{"0", "101", "-1", "x", "1.5"} {
		r.supp.reset()
		resp, body := r.get(loginA, "/v1/suppressions?limit="+bad)
		r.wantError(resp, body, 400, "validation_failed")
		if len(r.supp.seen()) != 0 {
			t.Fatalf("limit %s reached the store", bad)
		}
	}
	for _, good := range []string{"1", "100"} {
		if resp, body := r.get(loginA, "/v1/suppressions?limit="+good); resp.StatusCode != 200 {
			t.Fatalf("control: limit %s: %d %s", good, resp.StatusCode, body)
		}
	}
	// a parameter the route does not know, and one given twice
	for _, bad := range []string{"?offset=5", "?q=a&q=b", "?cursor=a&cursor=b"} {
		resp, body := r.get(loginA, "/v1/suppressions"+bad)
		r.wantError(resp, body, 400, "bad_request")
	}
	// the store's own refusals of a search and a cursor are 400s with a sentence of the API's
	r.supp.fail["List"] = suppress.ErrBadCursor
	resp, body := r.get(loginA, "/v1/suppressions?cursor=forged")
	if e := r.wantError(resp, body, 400, "validation_failed"); e.Details["reason"] != "the cursor is not one this API gave" || strings.Contains(string(body), "forged") {
		t.Fatalf("a forged cursor: %s", body)
	}
	r.supp.fail["List"] = suppress.ErrBadQuery
	resp, body = r.get(loginA, "/v1/suppressions?q=x")
	r.wantError(resp, body, 400, "validation_failed")
	// an empty list is an empty array, never null
	r.supp.fail, r.supp.listed, r.supp.next = map[string]error{}, nil, ""
	resp, body = r.get(loginB, "/v1/suppressions")
	if m := r.wantStatus(resp, body, 200); string(mustJSON(t, m["entries"])) != "[]" {
		t.Fatalf("an empty list is %s", mustJSON(t, m["entries"]))
	}
}

func TestSuppressions_AddCreatesOnceAndSaysSoTheSecondTime(t *testing.T) {
	r := newRig(t)
	resp, body := r.post(loginA, "/v1/suppressions", map[string]any{"email": "  New.Person@EXAMPLE.com "})
	m := r.wantStatus(resp, body, 201)
	entry, _ := m["entry"].(map[string]any)
	if entry["email"] != "new.person@example.com" || entry["source"] != "manual" || entry["created_at"] != stamp(t0) || m["already"] != nil {
		t.Fatalf("created: %s", body)
	}
	resp, body = r.post(loginA, "/v1/suppressions", map[string]any{"email": "new.person@example.com"})
	m = r.wantStatus(resp, body, 200)
	if m["already"] != true || m["entry"].(map[string]any)["email"] != "new.person@example.com" {
		t.Fatalf("the second add: %s", body)
	}
	// another user adding the same address is their own entry: created, and the first user's list is untouched
	resp, body = r.post(loginB, "/v1/suppressions", map[string]any{"email": "new.person@example.com"})
	r.wantStatus(resp, body, 201)
	if !r.supp.has(userA, "new.person@example.com") || !r.supp.has(userB, "new.person@example.com") {
		t.Fatal("each user holds their own entry")
	}
	for _, c := range r.supp.seen() {
		if c.User != userA && c.User != userB {
			t.Fatalf("the store was given %q", c.User)
		}
	}
}

func TestSuppressions_AnInvalidAddressIsATextlessRefusalAndNothingIsWritten(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ email, reason string }{
		{"canary-no-at-zq9", "no_at"}, {"canaryzq9@@example.com", "multiple_at"}, {"@canaryzq9.example.com", "empty_local"}, {"canaryzq9@example", "domain_no_dot"}, {"   ", "empty"}, {"", "empty"},
	} {
		for _, route := range []string{"/v1/suppressions", "/v1/suppressions/remove"} {
			r.supp.reset()
			resp, body := r.post(loginA, route, map[string]any{"email": c.email})
			e := r.wantError(resp, body, 400, "validation_failed")
			if e.Details["reason"] != "the address is not valid" || e.Details["address_reason"] != c.reason || strings.Contains(string(body), "zq9") {
				t.Fatalf("%s %q: %s", route, c.email, body)
			}
			if strings.Contains(r.logs.String(), "zq9") {
				t.Fatalf("an address is in the log: %s", r.logs.String())
			}
		}
	}
	if r.supp.has(userA, "") || len(r.supp.lists[userA]) != 0 {
		t.Fatal("something was written for a refused address")
	}
	// the control: a valid address is accepted by the same routes
	if resp, body := r.post(loginA, "/v1/suppressions", map[string]any{"email": "fine@example.com"}); resp.StatusCode != 201 {
		t.Fatalf("control: %d %s", resp.StatusCode, body)
	}
}

func TestSuppressions_AddAndRemoveDecodeStrictlyLikeTheOtherRoutes(t *testing.T) {
	for _, route := range []string{"/v1/suppressions", "/v1/suppressions/remove"} {
		r := newRig(t)
		for name, c := range map[string]struct {
			body  any
			ctype string
			want  int
			code  string
		}{
			"a member naming a user":     {`{"email":"a@example.com","user_id":"` + userB + `"}`, "application/json", 400, "bad_request"},
			"a member the route lacks":   {`{"email":"a@example.com","source":"import"}`, "application/json", 400, "bad_request"},
			"no email":                   {`{}`, "application/json", 400, "validation_failed"},
			"a null email":               {`{"email":null}`, "application/json", 400, "validation_failed"},
			"an email of the wrong type": {`{"email":5}`, "application/json", 400, "bad_request"},
			"not JSON":                   {`email=a@example.com`, "application/json", 400, "bad_request"},
			"data after the object":      {`{"email":"a@example.com"} {}`, "application/json", 400, "bad_request"},
			"an array":                   {`["a@example.com"]`, "application/json", 400, "bad_request"},
			"an empty body":              {``, "application/json", 400, "bad_request"},
			"a form":                     {`email=a@example.com`, "application/x-www-form-urlencoded", 415, "unsupported_media_type"},
			"no content type":            {`{"email":"a@example.com"}`, "", 415, "unsupported_media_type"},
		} {
			r.supp.reset()
			resp, body := r.do(req{method: http.MethodPost, path: route, token: r.token(loginA), ctype: c.ctype, body: []byte(c.body.(string))})
			r.wantError(resp, body, c.want, c.code)
			if len(r.supp.seen()) != 0 {
				t.Errorf("%s %s: the store was reached", route, name)
			}
		}
		// a query parameter is refused too (the user above all), and the control is the plain request
		resp, body := r.post(loginA, route+"?user_id="+userB, map[string]any{"email": "a@example.com"})
		r.wantError(resp, body, 400, "bad_request")
		if resp, _ := r.post(loginA, route, map[string]any{"email": "a@example.com"}); resp.StatusCode != 200 && resp.StatusCode != 201 {
			t.Fatalf("%s control: %d", route, resp.StatusCode)
		}
	}
}

func TestSuppressions_RemoveSaysOnlyWhetherYourOwnEntryWentAndNeverTellsAnotherUsersFromAbsent(t *testing.T) {
	r := newRig(t)
	r.supp.put(userA, "shared@example.com", "unsubscribe")
	r.supp.put(userB, "only-b@example.com", "manual")
	// B removes an address that is only on A's list: false, A's entry stays, and the answer is the one for an address nobody suppressed
	resp1, body1 := r.post(loginB, "/v1/suppressions/remove", map[string]any{"email": "shared@example.com"})
	resp2, body2 := r.post(loginB, "/v1/suppressions/remove", map[string]any{"email": "nobody-has-this@example.com"})
	if m := r.wantStatus(resp1, body1, 200); m["removed"] != false {
		t.Fatalf("B removing A's address: %s", body1)
	}
	r.wantStatus(resp2, body2, 200)
	if sameBody(body1) != sameBody(body2) {
		t.Fatalf("another user's address and an absent one are answered differently:\n%s\n%s", body1, body2)
	}
	if !r.supp.has(userA, "shared@example.com") {
		t.Fatal("B's request removed A's entry")
	}
	// the control: the owner removes it (any spelling of it), and a second removal is false
	resp, body := r.post(loginA, "/v1/suppressions/remove", map[string]any{"email": " SHARED@example.COM "})
	if m := r.wantStatus(resp, body, 200); m["removed"] != true || r.supp.has(userA, "shared@example.com") {
		t.Fatalf("the owner's removal: %s", body)
	}
	resp, body = r.post(loginA, "/v1/suppressions/remove", map[string]any{"email": "shared@example.com"})
	if m := r.wantStatus(resp, body, 200); m["removed"] != false {
		t.Fatalf("a second removal: %s", body)
	}
	for _, c := range r.supp.seen() {
		if c.Method == "Remove" && c.User != userA && c.User != userB {
			t.Fatalf("a removal was made as %q", c.User)
		}
	}
}

func TestSuppressions_ImportCountsAndNeverEchoesAnAddress(t *testing.T) {
	r := newRig(t)
	r.store.maxRows = 100
	csvBody := "email,name\n" +
		"ada@example.com,Ada\n" +
		"canary-bad-line@@example.com,Bad\n" +
		"bob@example.com,Bob\n" +
		"not-an-address-zq7,Nope\n"
	resp, body := csvReq(r, loginA, "/v1/suppressions/import", csvBody)
	m := r.wantStatus(resp, body, 200)
	if m["added"] != float64(2) || m["already_there"] != float64(0) || m["invalid"] != float64(2) {
		t.Fatalf("the counts: %s", body)
	}
	sample, _ := m["invalid_sample"].([]any)
	if len(sample) != 2 || sample[0].(map[string]any)["line"] != float64(3) || sample[0].(map[string]any)["reason"] != "multiple_at" || sample[1].(map[string]any)["reason"] != "no_at" || len(sample[0].(map[string]any)) != 2 {
		t.Fatalf("the sample: %s", body)
	}
	if strings.Contains(string(body), "canary-bad-line") || strings.Contains(string(body), "zq7") || strings.Contains(r.logs.String(), "canary-bad-line") {
		t.Fatalf("an address is in the answer or the log: %s", body)
	}
	if !r.supp.has(userA, "ada@example.com") || !r.supp.has(userA, "bob@example.com") || r.supp.has(userB, "ada@example.com") {
		t.Fatal("the entries are on the importer's list, and only there")
	}
	if c := r.supp.seen(); len(c) != 1 || c[0].User != userA || c[0].Arg != 100 {
		t.Fatalf("the store was called %+v, want one import as A with the platform's row limit (100)", c)
	}
	// an empty sample is an empty array
	resp, body = csvReq(r, loginB, "/v1/suppressions/import", "email\nfine@example.com\n")
	if m := r.wantStatus(resp, body, 200); string(mustJSON(t, m["invalid_sample"])) != "[]" {
		t.Fatalf("no invalid line: %s", body)
	}
}

func TestSuppressions_ImportRefusesWhatIsNotACSVWithAnEmailColumnAndAFileOverTheLimit(t *testing.T) {
	r := newRig(t)
	r.store.maxRows = 3
	for name, c := range map[string]struct{ body, reason string }{
		"no header":         {"", "the CSV has no header row"},
		"no email column":   {"name,address\nAda,a@example.com\n", "the header has no email column"},
		"two email columns": {"email,EMAIL\na@example.com,b@example.com\n", "the header names the email column twice"},
	} {
		resp, body := csvReq(r, loginA, "/v1/suppressions/import", c.body)
		e := r.wantError(resp, body, 400, "validation_failed")
		if e.Details["reason"] != c.reason || strings.Contains(string(body), "a@example.com") {
			t.Errorf("%s: %s", name, body)
		}
	}
	resp, body := csvReq(r, loginA, "/v1/suppressions/import", "email\na@example.com\nb@example.com\nc@example.com\nd@example.com\n")
	if e := r.wantError(resp, body, 413, "too_many_rows"); e.Details["limit"] != float64(3) {
		t.Fatalf("four rows against three: %s", body)
	}
	if r.supp.has(userA, "a@example.com") {
		t.Fatal("a file over the limit wrote something")
	}
	// the media type: JSON is not a CSV; no content type either; a query parameter is refused
	for _, ctype := range []string{"application/json", "", "text/plain"} {
		resp, body := r.do(req{method: http.MethodPost, path: "/v1/suppressions/import", token: r.token(loginA), ctype: ctype, body: []byte("email\na@example.com\n")})
		r.wantError(resp, body, 415, "unsupported_media_type")
	}
	resp, body = r.do(req{method: http.MethodPost, path: "/v1/suppressions/import?user_id=" + userB, token: r.token(loginA), ctype: "text/csv", body: []byte("email\na@example.com\n")})
	r.wantError(resp, body, 400, "bad_request")
	// the control: the same file within the limit
	resp, body = csvReq(r, loginA, "/v1/suppressions/import", "email\na@example.com\nb@example.com\nc@example.com\n")
	if m := r.wantStatus(resp, body, 200); m["added"] != float64(3) {
		t.Fatalf("control: %s", body)
	}
}

func TestSuppressions_TheRoutesAreNotServedWithoutAListAndAServerFaultIsA500WithNoCause(t *testing.T) {
	r := newRig(t)
	r.supp.fail["Add"] = errBoom
	resp, body := r.post(loginA, "/v1/suppressions", map[string]any{"email": "a@example.com"})
	e := r.wantError(resp, body, 500, "internal_error")
	if e.Details != nil || strings.Contains(string(body), "boom") {
		t.Fatalf("the cause reached the client: %s", body)
	}
	// built with no list the four routes are the 404 of any unknown path, and the job routes are unaffected
	api, err := New(Deps{Service: newFakeStore(), Verifier: newRig(t).mint.Verifier(t), Resolver: &fakeResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Mount(mux)
	for _, route := range []string{"GET /v1/suppressions", "POST /v1/suppressions", "POST /v1/suppressions/remove", "POST /v1/suppressions/import"} {
		method, path, _ := strings.Cut(route, " ")
		rec := serveOnce(mux, method, path)
		if rec == http.StatusUnauthorized || rec == http.StatusOK {
			t.Errorf("%s is served by an API built with no list: %d", route, rec)
		}
	}
}

func TestSuppressions_ViewsHoldExactlyTheirMembers(t *testing.T) {
	v := mustJSON(t, viewEntry(suppress.Entry{Email: "a@example.com", Source: "import", CreatedAt: time.Date(2026, 11, 20, 10, 0, 0, 5000, time.UTC)}))
	if string(v) != `{"email":"a@example.com","source":"import","created_at":"2026-11-20T10:00:00.000005Z"}` {
		t.Fatalf("the entry view is %s", v)
	}
}
