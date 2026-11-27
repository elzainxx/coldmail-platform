//go:build harness

package harness

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/suppress"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 6 A4: the four suppression routes over the whole stack in ONE process against a real database, exactly as jobs_api_test.go builds the job API (the real httpapi handler, the real verifier on tokens minted per
// run, the real tenant resolver, the real stores over a pool of logins as platform_app), with the suppression list the SERVICE gives it (suppress.NewStore). CROSS-USER and PAIRED: every route is driven as user A and
// as user B over the same addresses, and what each of them can read, write and remove is read back from the database as the superuser. The unit tests of internal/api prove the HTTP behaviour against a fake that
// keeps one list per user; what only this proves is that the SQL behind the routes keeps the two lists apart.
// Every name starts with `jas` so that it cannot clash with another file's.

func newJasEnv(t *testing.T) *japEnv {
	t.Helper()
	e := newJbsEnv(t)
	mint := authtest.NewHS256(t)
	res, err := tenant.NewResolver(e.pool, "")
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	st, err := suppress.NewStore(e.pool)
	if err != nil {
		t.Fatalf("suppress.NewStore: %v", err)
	}
	a, err := api.New(api.Deps{Service: e.jobs, Verifier: mint.Verifier(t), Resolver: res, Suppressions: st})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Pinger: e.pool, ReadyTimeout: 5 * time.Second, Mount: a.Mount}))
	t.Cleanup(srv.Close)
	return &japEnv{jbsEnv: e, srv: srv, mint: mint}
}

func jasEmails(t *testing.T, r japResp) []string {
	t.Helper()
	m := r.obj(t, 200)
	list, _ := m["entries"].([]any)
	var out []string
	for _, x := range list {
		out = append(out, x.(map[string]any)["email"].(string))
	}
	return supstSet(out)
}

func TestSuppressionAPI_EveryRouteIsTheCallersOwnAndAnotherUsersListIsNeverReadWrittenOrRemoved(t *testing.T) {
	e := newJasEnv(t)
	A, B := mnyUserA, mnyUserB

	// the same address for both users, one address each, all through the API
	e.json(A, "POST", "/v1/suppressions", map[string]any{"email": "Shared@Example.com"}).obj(t, 201)
	e.json(A, "POST", "/v1/suppressions", map[string]any{"email": "a-only@example.com"}).obj(t, 201)
	e.json(B, "POST", "/v1/suppressions", map[string]any{"email": "shared@example.com"}).obj(t, 201)
	e.json(B, "POST", "/v1/suppressions", map[string]any{"email": "b-only@example.com"}).obj(t, 201)
	if again := e.json(A, "POST", "/v1/suppressions", map[string]any{"email": "shared@example.com"}).obj(t, 200); again["already"] != true {
		t.Fatalf("a second add by the same user: %v", again)
	}

	// each user lists their own and only their own; a search can only narrow it
	if got := strings.Join(jasEmails(t, e.json(A, "GET", "/v1/suppressions", nil)), ","); got != "a-only@example.com,shared@example.com" {
		t.Fatalf("A's list is %s", got)
	}
	if got := strings.Join(jasEmails(t, e.json(B, "GET", "/v1/suppressions", nil)), ","); got != "b-only@example.com,shared@example.com" {
		t.Fatalf("B's list is %s", got)
	}
	if got := jasEmails(t, e.json(A, "GET", "/v1/suppressions?q=b-only", nil)); len(got) != 0 {
		t.Fatalf("A's search found %v of B's list", got)
	}
	if got := jasEmails(t, e.json(B, "GET", "/v1/suppressions?q=b-only", nil)); len(got) != 1 {
		t.Fatalf("control: B's own search found %v", got)
	}
	// the wildcards of a pattern are only characters: a search for % finds nothing here, and one for _ neither
	for _, q := range []string{"%25", "_", "%25only%25", "a_only"} {
		if got := jasEmails(t, e.json(A, "GET", "/v1/suppressions?q="+q, nil)); len(got) != 0 {
			t.Fatalf("the search %q found %v: a wildcard was read as a pattern", q, got)
		}
	}

	// B asks to remove an address that is only on A's list: the answer is the one for an address nobody suppressed, and A's entry stays
	other := e.json(B, "POST", "/v1/suppressions/remove", map[string]any{"email": "a-only@example.com"})
	absent := e.json(B, "POST", "/v1/suppressions/remove", map[string]any{"email": "nobody-has-this@example.com"})
	if m := other.obj(t, 200); m["removed"] != false {
		t.Fatalf("B removing A's address: %s", other.Body)
	}
	if other.normalised() != absent.normalised() {
		t.Fatalf("another user's address and an absent one are answered differently:\n%s\n%s", other.Body, absent.Body)
	}
	if got := jasEmails(t, e.json(A, "GET", "/v1/suppressions", nil)); len(got) != 2 {
		t.Fatalf("A's list after B's removal attempt: %v", got)
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals"); n != 0 {
		t.Fatalf("%d history row(s) for removals that removed nothing", n)
	}
	// the owner removes it: true, and the history is exactly that entry of exactly that user
	if m := e.json(A, "POST", "/v1/suppressions/remove", map[string]any{"email": " A-Only@example.com "}).obj(t, 200); m["removed"] != true {
		t.Fatalf("A removing their own entry: %v", m)
	}
	if rows := e.rows("SELECT user_id::text || '|' || email || '|' || source FROM platform.suppression_removals"); len(rows) != 1 || rows[0] != A+"|a-only@example.com|manual" {
		t.Fatalf("the history is %v, want exactly A's a-only@example.com", rows)
	}
	// B's shared entry is untouched by A's list changes, and removing it takes B's only
	if m := e.json(B, "POST", "/v1/suppressions/remove", map[string]any{"email": "shared@example.com"}).obj(t, 200); m["removed"] != true {
		t.Fatalf("B removing their own entry of the shared address: %v", m)
	}
	if got := strings.Join(jasEmails(t, e.json(A, "GET", "/v1/suppressions", nil)), ","); got != "shared@example.com" {
		t.Fatalf("A's list after B's removal of the same address: %s", got)
	}

	// a request that names a user is refused as an unknown member and writes nothing
	before := e.int("SELECT count(*) FROM platform.suppressions")
	e.json(B, "POST", "/v1/suppressions", map[string]any{"email": "named@example.com", "user_id": A}).refused(t, 400, "bad_request")
	e.json(B, "GET", "/v1/suppressions?user_id="+A, nil).refused(t, 400, "bad_request")
	if e.int("SELECT count(*) FROM platform.suppressions") != before {
		t.Fatal("a refused request wrote a row")
	}
	// an address the rule refuses is a refusal with a word and not the address, and writes nothing
	r := e.json(A, "POST", "/v1/suppressions", map[string]any{"email": "canary-no-at-sign-qz9"})
	if d := r.refused(t, 400, "validation_failed"); d["address_reason"] != "no_at" || strings.Contains(string(r.Body), "qz9") {
		t.Fatalf("an invalid address: %s", r.Body)
	}
	// nobody anonymous is served
	for _, route := range []struct{ method, path string }{{"GET", "/v1/suppressions"}, {"POST", "/v1/suppressions"}, {"POST", "/v1/suppressions/remove"}, {"POST", "/v1/suppressions/import"}} {
		e.json("", route.method, route.path, map[string]any{"email": "x@example.com"}).refused(t, 401, "unauthenticated")
	}
}

func TestSuppressionAPI_ImportIsTheImportersOwnAndCountsAgainstTheRealList(t *testing.T) {
	e := newJasEnv(t)
	A, B := mnyUserA, mnyUserB
	e.json(B, "POST", "/v1/suppressions", map[string]any{"email": "ada@example.com"}).obj(t, 201)
	e.json(A, "POST", "/v1/suppressions", map[string]any{"email": "known@example.com"}).obj(t, 201)
	csv := "email,name\nada@example.com,Ada\nknown@example.com,K\nnot-an-address-qz7,X\nbob@example.com,Bob\n"
	r := e.callAs(A, "POST", "/v1/suppressions/import", "text/csv", []byte(csv))
	m := r.obj(t, 200)
	if m["added"] != float64(2) || m["already_there"] != float64(1) || m["invalid"] != float64(1) || strings.Contains(string(r.Body), "qz7") {
		t.Fatalf("the counts: %s", r.Body)
	}
	if sample, _ := m["invalid_sample"].([]any); len(sample) != 1 || sample[0].(map[string]any)["line"] != float64(4) || sample[0].(map[string]any)["reason"] != "no_at" {
		t.Fatalf("the sample: %s", r.Body)
	}
	if got := strings.Join(jasEmails(t, e.json(A, "GET", "/v1/suppressions", nil)), ","); got != "ada@example.com,bob@example.com,known@example.com" {
		t.Fatalf("A's list after the import: %s", got)
	}
	if got := strings.Join(jasEmails(t, e.json(B, "GET", "/v1/suppressions", nil)), ","); got != "ada@example.com" {
		t.Fatalf("B's list after A's import: %s", got)
	}
	if rows := e.rows("SELECT source FROM platform.suppressions WHERE user_id = $1 AND email = 'known@example.com'", A); len(rows) != 1 || rows[0] != "manual" {
		t.Fatalf("the entry A already had changed its source: %v", rows)
	}
	// the refusals: not a CSV, no email column, over the row limit (the job store's, 100000 here is too many to send: the unit tests of the route hold the limit; this holds the media type and the header)
	e.callAs(A, "POST", "/v1/suppressions/import", "application/json", []byte(`{"email":"x@example.com"}`)).refused(t, 415, "unsupported_media_type")
	if d := e.callAs(A, "POST", "/v1/suppressions/import", "text/csv", []byte("name\nAda\n")).refused(t, 400, "validation_failed"); d["reason"] != "the header has no email column" {
		t.Fatalf("a header with no email column: %v", d)
	}
}
