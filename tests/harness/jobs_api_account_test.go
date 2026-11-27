//go:build harness

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/app"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 6 A3 against a real database: GET /v1/me, GET /v1/balances, GET /v1/jobs?status=active, POST /v1/templates/check and the CSV create's email_column, over the whole stack in one process exactly as
// jobs_api_test.go builds it, with the one difference that the API is given the account service the SERVICE gives it (app.NewAccountService over the real resolver, money store and entitlement store) and
// the job store renders templates (jobs.WithPreparer, as in production). The unit tests of internal/api prove the HTTP behaviour against fakes; what only a database can prove is proved here: every number
// and every address is the principal's and nobody else's, the time of an entitlement is the database's, the filter of the list is the store's SQL, and the check and the save are one judgement.
//
// Every name starts with `jaa` so that it cannot clash with the other files of this package. The users are the three customers of the money fixtures (mnyUserA, mnyUserB, mnyUserC).

// newJaaEnv is newJapEnv with the account service and the template renderer of the production wiring.
func newJaaEnv(t *testing.T) *japEnv {
	t.Helper()
	e := newJbsEnv(t, jobs.WithPreparer(jobs.TemplateRenderer{}))
	mint := authtest.NewHS256(t)
	res, err := tenant.NewResolver(e.pool, "")
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	a, err := api.New(api.Deps{Service: e.jobs, Verifier: mint.Verifier(t), Resolver: res, Account: app.NewAccountService(res, e.store, entitlements.NewStore(e.pool))})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Pinger: e.pool, ReadyTimeout: 5 * time.Second, Mount: a.Mount}))
	t.Cleanup(srv.Close)
	return &japEnv{jbsEnv: e, srv: srv, mint: mint}
}

// jaaGetWith sends a GET with this exact token (a token the test minted by hand: a claim the minter's own Token does not carry).
func (e *japEnv) jaaGetWith(token, path string) japResp {
	e.t.Helper()
	q, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		q.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(q)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("GET %s: reading the answer: %v", path, err)
	}
	return japResp{resp.StatusCode, resp.Header, b}
}

func TestJobsAPIAccount_MeIsTheDatabasesAddressOfThePrincipalAndOnlyTheirs(t *testing.T) {
	e := newJaaEnv(t)
	A, B, C := mnyUserA, mnyUserB, mnyUserC

	meA := e.json(A, "GET", "/v1/me", nil).obj(t, 200)
	if len(meA) != 1 || meA["email"] != "money-a@example.test" {
		t.Fatalf("GET /v1/me as A = %v, want exactly {email: money-a@example.test}", meA)
	}
	respB := e.json(B, "GET", "/v1/me", nil)
	meB := respB.obj(t, 200)
	if meB["email"] != "money-b@example.test" || strings.Contains(string(respB.Body), "money-a@") {
		t.Fatalf("GET /v1/me as B = %s: another user's address must never be in it", respB.Body)
	}

	// the address is the DATABASE's, not a claim: a token that claims another one is still answered with the row's
	forged := e.mint.Mint(e.mint.Good(A).With("email", "forged@example.test", "user_metadata", map[string]any{"email": "forged@example.test"}))
	r := e.jaaGetWith(forged, "/v1/me")
	if got := r.obj(t, 200); got["email"] != "money-a@example.test" || strings.Contains(string(r.Body), "forged") {
		t.Fatalf("a token that claims an address changed the answer: %s", r.Body)
	}
	// ... and it is read at the request: a change of the row is the next answer (the twin of the claim: the row is what moves the answer)
	e.exec("UPDATE public.users SET email = 'renamed-a@example.test' WHERE id = $1", A)
	if got := e.json(A, "GET", "/v1/me", nil).obj(t, 200); got["email"] != "renamed-a@example.test" {
		t.Fatalf("after the row changed GET /v1/me = %v", got)
	}
	if got := e.json(B, "GET", "/v1/me", nil).obj(t, 200); got["email"] != "money-b@example.test" {
		t.Fatalf("a change of A's row changed B's answer: %v", got)
	}

	// refused as every route refuses: no token, a token of a login that has no platform user, and a user who was switched off (the platform's own check, made on every request)
	e.json("", "GET", "/v1/me", nil).refused(t, 401, "unauthenticated")
	e.json("e5e5e5e5-0000-4000-8000-0000000000e5", "GET", "/v1/me", nil).refused(t, 401, "unauthenticated")
	e.exec("UPDATE public.users SET is_active = false WHERE id = $1", C)
	e.json(C, "GET", "/v1/me", nil).refused(t, 401, "unauthenticated")
	e.exec("UPDATE public.users SET is_active = true WHERE id = $1", C)
	if got := e.json(C, "GET", "/v1/me", nil).obj(t, 200); got["email"] != "money-c@example.test" { // the twin: switched back on, answered
		t.Fatalf("a user switched back on reads %v", got)
	}
	// nothing names a user: a query that tries is refused and reaches nothing
	e.json(A, "GET", "/v1/me?user_id="+B, nil).refused(t, 400, "bad_request")
}

func TestJobsAPIAccount_BalancesAreEachUsersOwnAndTheEntitlementIsJudgedByTheDatabasesClock(t *testing.T) {
	e := newJaaEnv(t)
	A, B := mnyUserA, mnyUserB

	// A holds 997 credits on module 1 with 3 reserved by a confirmed job of 3 rows, and 70 on module 2; B holds 9 and 4000 with nothing reserved
	e.confirmed(A, "m1", 3)
	e.fund(A, "m2", 70)
	e.fund(B, "m1", 9)
	e.fund(B, "m2", 4000)
	e.requireBal("A on module 1", A, "m1", 997, 3)

	read := func(user string) japResp { return e.json(user, "GET", "/v1/balances", nil) }
	num := func(m map[string]any, path ...string) float64 {
		t.Helper()
		v, ok := dig(m, path...).(float64)
		if !ok {
			t.Fatalf("%v is not a number in %v", path, m)
		}
		return v
	}
	ra := read(A)
	a := ra.obj(t, 200)
	if len(a) != 2 || num(a, "modules", "m1", "available") != 997 || num(a, "modules", "m1", "reserved") != 3 || num(a, "modules", "m2", "available") != 70 || num(a, "modules", "m2", "reserved") != 0 {
		t.Fatalf("GET /v1/balances as A = %s, want m1 997/3 and m2 70/0", ra.Body)
	}
	rb := read(B)
	b := rb.obj(t, 200)
	if num(b, "modules", "m1", "available") != 9 || num(b, "modules", "m1", "reserved") != 0 || num(b, "modules", "m2", "available") != 4000 || num(b, "modules", "m2", "reserved") != 0 {
		t.Fatalf("GET /v1/balances as B = %s, want m1 9/0 and m2 4000/0", rb.Body)
	}
	for _, theirs := range []string{"997", "\"reserved\":3", "\"available\":70"} {
		if strings.Contains(string(rb.Body), theirs) {
			t.Errorf("B's answer holds %s, which is A's: %s", theirs, rb.Body)
		}
	}
	// a user who has never had credits reads zeros, and a user with no entitlement reads the empty module 3
	rc := e.json(mnyUserC, "GET", "/v1/balances", nil).obj(t, 200)
	if num(rc, "modules", "m1", "available") != 0 || num(rc, "modules", "m2", "reserved") != 0 || dig(rc, "m3", "enabled") != false || dig(rc, "m3", "until") != nil || dig(rc, "m3", "state") != "unknown" {
		t.Fatalf("a user with nothing reads %v", rc)
	}

	// MODULE 3. A has an entitlement for an hour: enabled, with its end, and nothing observed yet
	e.enableM3(A, false)
	m3 := func(user string) map[string]any {
		t.Helper()
		m, _ := dig(read(user).obj(t, 200), "m3").(map[string]any)
		return m
	}
	got := m3(A)
	if got["enabled"] != true || got["state"] != "unknown" || len(got) != 3 {
		t.Fatalf("an entitlement never observed: %v", got)
	}
	until, _ := got["until"].(string)
	if end, err := time.Parse(time.RFC3339Nano, until); err != nil || time.Until(end) < 30*time.Minute || time.Until(end) > 2*time.Hour {
		t.Fatalf("until = %q, want about an hour from now (%v)", until, err)
	}
	// what the platform last SAW module 3 hold the user in is the state, in the platform's own three words
	e.exec("UPDATE platform.m3_sync SET observed = 'active', observed_version = want_version WHERE user_id = $1", A)
	if got := m3(A); got["state"] != "active" || got["enabled"] != true {
		t.Fatalf("an entitlement seen active: %v", got)
	}
	e.exec("UPDATE platform.m3_sync SET observed = 'suspended', observed_version = want_version WHERE user_id = $1", A)
	if got := m3(A); got["state"] != "suspended" || got["enabled"] != true {
		t.Fatalf("an entitlement seen suspended: %v", got)
	}
	// B has none of this, whatever A's state is (the control of the user scope)
	if got := m3(B); got["enabled"] != false || got["until"] != nil || got["state"] != "unknown" {
		t.Fatalf("A's module 3 state reached B: %v", got)
	}
	// the date passes by the DATABASE's clock: the row still says enabled (the expiry loop has not run in this test), the answer says it is not, and still shows when it ended
	e.exec("UPDATE platform.m3_entitlements SET enabled_until = now() - interval '1 hour' WHERE user_id = $1", A)
	if e.str("SELECT enabled::text FROM platform.m3_entitlements WHERE user_id = $1", A) != "true" {
		t.Fatal("the fixture is wrong: the row must still say enabled")
	}
	got = m3(A)
	if got["enabled"] != false || got["state"] != "suspended" {
		t.Fatalf("an entitlement past its end: %v, want enabled false (and the state still the one seen)", got)
	}
	if end, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got["until"])); err != nil || !end.Before(time.Now()) {
		t.Fatalf("until = %v, want the date that passed", got["until"])
	}
	// the twin: the same row with the date ahead again is enabled
	e.exec("UPDATE platform.m3_entitlements SET enabled_until = now() + interval '1 hour' WHERE user_id = $1", A)
	if got := m3(A); got["enabled"] != true {
		t.Fatalf("an entitlement with its date ahead: %v", got)
	}

	e.json("", "GET", "/v1/balances", nil).refused(t, 401, "unauthenticated")
	e.json("e5e5e5e5-0000-4000-8000-0000000000e5", "GET", "/v1/balances", nil).refused(t, 401, "unauthenticated")
	e.json(A, "GET", "/v1/balances?user_id="+B, nil).refused(t, 400, "bad_request")
}

func jaaListIDs(t *testing.T, r japResp) (ids []string, statuses map[string]string, next any) {
	t.Helper()
	m := r.obj(t, 200)
	statuses = map[string]string{}
	list, _ := m["jobs"].([]any)
	for _, j := range list {
		jm, _ := j.(map[string]any)
		id, _ := jm["id"].(string)
		st, _ := jm["status"].(string)
		ids = append(ids, id)
		statuses[id] = st
	}
	return ids, statuses, m["next_cursor"]
}

func jaaSet(ids ...string) map[string]bool {
	s := map[string]bool{}
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func jaaSame(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	g, w := jaaSet(got...), jaaSet(want...)
	if len(got) != len(g) || len(g) != len(w) {
		t.Fatalf("%s: %v, want exactly %v", what, got, want)
	}
	for id := range w {
		if !g[id] {
			t.Fatalf("%s: %v, want exactly %v", what, got, want)
		}
	}
}

func TestJobsAPIAccount_ListStatusActiveIsTheJobsInFlightAndOnlyTheUsersOwn(t *testing.T) {
	e := newJaaEnv(t)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	start := time.Now().Add(2 * time.Hour)

	// the jobs that are NOT in flight come first: a terminal job holds no slot, so the slot of its module is free for the job that is
	succeeded, _ := e.confirmed(A, "m1", 2) // confirmed through the store, as the other tests do before they walk a job to its end
	jobWalkTo(t, e.admin, succeeded.ID, "succeeded")
	cancelled := e.jbsEnv.create(A, "m2", 2)
	if _, err := e.jobs.DiscardDraft(ctx, A, cancelled.ID); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	draft := e.jbsEnv.create(A, "m1", 2)
	previewedM1, _ := e.previewed(A, "m1", 2)

	// and the three that are: running (module 1), scheduled then paused (module 2), scheduled (module 3)
	e.fund(A, "m1", 1000)
	e.fund(A, "m2", 1000)
	e.enableM3(A, false)
	running, _ := e.confirmed(A, "m1", 3)
	jobWalkTo(t, e.admin, running.ID, "running")
	paused, pp := e.previewed(A, "m2", 2)
	if _, err := e.tryConfirm(A, paused.ID, pp.Fingerprint, e.key(), &start); err != nil {
		t.Fatalf("scheduling the module 2 job: %v", err)
	}
	if _, err := e.jobs.Pause(ctx, A, paused.ID); err != nil {
		t.Fatalf("pausing the scheduled module 2 job: %v", err)
	}
	scheduled, sp := e.previewed(A, "m3", 2)
	if _, err := e.tryConfirm(A, scheduled.ID, sp.Fingerprint, e.key(), &start); err != nil {
		t.Fatalf("scheduling the module 3 job: %v", err)
	}

	all, allStatus, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?limit=100", nil))
	jaaSame(t, "the whole list of A", all, succeeded.ID, cancelled.ID, draft.ID, previewedM1.ID, running.ID, paused.ID, scheduled.ID)
	want := map[string]string{succeeded.ID: "succeeded", cancelled.ID: "cancelled", draft.ID: "draft", previewedM1.ID: "previewed", running.ID: "running", paused.ID: "paused", scheduled.ID: "scheduled"}
	for id, st := range want {
		if allStatus[id] != st {
			t.Fatalf("job %s is %q, the fixture meant %q", id, allStatus[id], st)
		}
	}

	active, activeStatus, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=100", nil))
	jaaSame(t, "the active list of A", active, running.ID, paused.ID, scheduled.ID)
	for id, st := range activeStatus {
		if st != "running" && st != "paused" && st != "scheduled" {
			t.Errorf("job %s is %s in the active list", id, st)
		}
	}
	// the twin: no filter, and an empty one, are the whole list
	if ids, _, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=&limit=100", nil)); len(ids) != 7 {
		t.Errorf("an empty status gave %d jobs, want all 7", len(ids))
	}

	// the cursor still works under the filter: the same jobs, in the same order as the unfiltered list, none twice and none missing
	page1, _, next := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=2", nil))
	cur, _ := next.(string)
	if len(page1) != 2 || cur == "" {
		t.Fatalf("the first page of the active list = %v with cursor %v, want 2 and a cursor", page1, next)
	}
	page2, _, next2 := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=2&cursor="+cur, nil))
	if len(page2) != 1 || next2 != nil {
		t.Fatalf("the second page of the active list = %v with cursor %v, want 1 and no cursor", page2, next2)
	}
	jaaSame(t, "the two pages together", append(append([]string{}, page1...), page2...), running.ID, paused.ID, scheduled.ID)
	var order []string
	for _, id := range all {
		if id == running.ID || id == paused.ID || id == scheduled.ID {
			order = append(order, id)
		}
	}
	if strings.Join(append(append([]string{}, page1...), page2...), ",") != strings.Join(order, ",") {
		t.Errorf("the active list is in another order than the whole list: %v / %v", append(append([]string{}, page1...), page2...), order)
	}

	// the fourth state, cancelling, is in flight; a job that ends is not
	if got := e.json(A, "POST", "/v1/jobs/"+running.ID+"/cancel", nil).obj(t, 200); dig(got, "job", "status") != "cancelling" {
		t.Fatalf("cancelling the running job: %v", got)
	}
	_, st, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=100", nil))
	if st[running.ID] != "cancelling" || len(st) != 3 {
		t.Fatalf("the active list after the cancel request = %v, want the three with the cancelled-while-running one cancelling", st)
	}
	if got := e.json(A, "POST", "/v1/jobs/"+paused.ID+"/cancel", nil).obj(t, 200); dig(got, "job", "status") != "cancelled" {
		t.Fatalf("cancelling the job paused before its start: %v", got)
	}
	ids, _, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=100", nil))
	jaaSame(t, "the active list once one has ended", ids, running.ID, scheduled.ID)

	// THE CROSS-USER PAIR: B has a job in flight too; each list holds only its owner's, with the filter and without, and A's cursor leads B nowhere into A's jobs
	e.fund(B, "m1", 1000)
	bJob, _ := e.confirmed(B, "m1", 2)
	jobWalkTo(t, e.admin, bJob.ID, "running")
	bIDs, _, _ := jaaListIDs(t, e.json(B, "GET", "/v1/jobs?status=active&limit=100", nil))
	jaaSame(t, "the active list of B", bIDs, bJob.ID)
	aIDs, _, _ := jaaListIDs(t, e.json(A, "GET", "/v1/jobs?status=active&limit=100", nil))
	for _, id := range aIDs {
		if id == bJob.ID {
			t.Fatalf("B's job is in A's active list")
		}
	}
	bAll, _, _ := jaaListIDs(t, e.json(B, "GET", "/v1/jobs?limit=100", nil))
	for _, id := range bAll {
		if want[id] != "" {
			t.Fatalf("one of A's jobs (%s) is in B's list", id)
		}
	}
	bViaA, _, _ := jaaListIDs(t, e.json(B, "GET", "/v1/jobs?status=active&limit=100&cursor="+cur, nil))
	for _, id := range bViaA {
		if want[id] != "" {
			t.Fatalf("A's cursor led B to A's job %s", id)
		}
	}

	// a word that is not a filter is a 400 and never reaches the SQL
	for _, bad := range []string{"running", "ACTIVE", "all", "active'%20OR%20'1'='1"} {
		e.json(A, "GET", "/v1/jobs?status="+bad, nil).refused(t, 400, "validation_failed")
	}
}

func TestJobsAPIAccount_TheTemplateCheckAndTheSaveAreOneJudgementOnARealStore(t *testing.T) {
	e := newJaaEnv(t)
	A := mnyUserA
	campaigns := []map[string]any{
		{"subject": "Hello {{first_name|there}}", "html": "<p>Hi {{first_name}}</p>", "text": "Hi {a|b|c}"},
		{"subject": "Hello", "html": "<p>x</p>"},
		{"subject": "Hello {{name", "html": "<p>x</p>"},
		{"subject": "Hello", "html": "<p>{only}</p>"},
		{"subject": "Hello", "html": "<p>x</p>", "text": "line one\nline two {{open"},
		{"subject": "Hello", "html": "café über {{open"},
		{"subject": "Hello", "html": "{a|{{unsubscribe_url}}}"},
		{"subject": "{a|{{unsubscribe_url|x}}}", "html": "<p>x</p>"},
		{"subject": "Hello", "html": "<p>x</p>", "text": "x {a|b {{unsubscribe_url}}}"},
		{"subject": "Hello", "html": "<p><a href=\"{{unsubscribe_url}}\">stop</a> {a|b}</p>"},
	}
	jobsBefore := e.int("SELECT count(*) FROM platform.jobs")
	eventsBefore := e.int("SELECT count(*) FROM platform.job_events")
	var ok, broken int
	for i, c := range campaigns {
		chk := e.json(A, "POST", "/v1/templates/check", c).obj(t, 200)
		// the check wrote nothing (the save below is the only thing that may)
		if e.int("SELECT count(*) FROM platform.jobs") != jobsBefore || e.int("SELECT count(*) FROM platform.job_events") != eventsBefore {
			t.Fatalf("campaign %d: the check wrote to the job tables", i)
		}
		save := e.json(A, "POST", "/v1/jobs", map[string]any{"module": "m2", "rows": []map[string]any{{"email": fmt.Sprintf("jaa%d@example.test", i)}}, "content": c})
		if chk["ok"] == true {
			ok++
			if save.Status != 201 {
				t.Errorf("campaign %d: the check says it is fine and the save answers %d %s", i, save.Status, save.Body)
			}
			jobsBefore++ // the save wrote one draft, which is not the check's doing
			eventsBefore = e.int("SELECT count(*) FROM platform.job_events")
			continue
		}
		broken++
		errs, _ := chk["errors"].([]any)
		if len(errs) == 0 {
			t.Fatalf("campaign %d: ok is false and there are no errors: %v", i, chk)
		}
		first, _ := errs[0].(map[string]any)
		details := save.refused(t, 400, "validation_failed")
		for _, k := range []string{"part", "line", "column", "message"} {
			if fmt.Sprint(details[k]) != fmt.Sprint(first[k]) {
				t.Errorf("campaign %d: the save says %s = %v, the check says %v", i, k, details[k], first[k])
			}
		}
	}
	if ok < 3 || broken < 6 {
		t.Fatalf("the table has %d good and %d broken campaigns: the comparison matched too little", ok, broken)
	}
	// a template check is the user's own only so far as the login: an anonymous request is refused and nothing is judged
	e.json("", "POST", "/v1/templates/check", map[string]any{"subject": "x"}).refused(t, 401, "unauthenticated")
}

func TestJobsAPIAccount_ACSVNamesItsAddressColumnAndTheRowsLandAsTheyWereRead(t *testing.T) {
	e := newJaaEnv(t)
	A, B := mnyUserA, mnyUserB
	post := func(user, query, csv string) japResp {
		return e.callAs(user, "POST", "/v1/jobs"+query, "text/csv", []byte(csv))
	}
	id := jobIDOf(t, post(A, "?module=m1&email_column=Work%20Email", "Name,work email,City\nAnn,ann@example.test,Paris\nBob,bob@example.test,Oslo\n"), 201)
	rows := e.rowsOf(id)
	if len(rows) != 2 || rows[0].Raw != "ann@example.test" || rows[1].Raw != "bob@example.test" || rows[0].Norm != "ann@example.test" {
		t.Fatalf("the rows are %+v, want the address column's values in order", rows)
	}
	for ordinal, want := range map[int]string{1: "Ann|Paris|false|false", 2: "Bob|Oslo|false|false"} {
		got := e.str(`SELECT (fields->>'Name') || '|' || (fields->>'City') || '|' || (fields ? 'work email')::text || '|' || (fields ? 'email')::text FROM platform.job_rows WHERE job_id = $1 AND ordinal = $2`, id, ordinal)
		if got != want {
			t.Errorf("row %d's merge fields = %q, want %q: the other columns, and not the address column", ordinal, got, want)
		}
	}
	// the default is still `email`, and a file the create refuses writes no job
	idDefault := jobIDOf(t, post(A, "?module=m1", "Email,Name\nann@example.test,Ann\n"), 201)
	if rs := e.rowsOf(idDefault); len(rs) != 1 || rs[0].Raw != "ann@example.test" {
		t.Fatalf("the default address column: %+v", rs)
	}
	before := e.int("SELECT count(*) FROM platform.jobs")
	for name, c := range map[string]struct{ query, csv string }{
		"no such column":          {"?module=m1&email_column=work_email", "name,mail\nAnn,ann@example.test\n"},
		"the column twice":        {"?module=m1&email_column=work", "work,name,WORK\na@example.test,x,b@example.test\n"},
		"a reserved email column": {"?module=m1&email_column=work", "work,email\na@example.test,b@example.test\n"},
		"a reserved unsubscribe":  {"?module=m1&email_column=work", "work,Unsubscribe_URL\na@example.test,x\n"},
		"a blank parameter":       {"?module=m1&email_column=%20", "email\na@example.test\n"},
	} {
		details := post(A, c.query, c.csv).refused(t, 400, "validation_failed")
		if reason, _ := details["reason"].(string); reason == "" || strings.Contains(reason, "example.test") {
			t.Errorf("%s: the reason is %q", name, reason)
		}
		if n := e.int("SELECT count(*) FROM platform.jobs"); n != before {
			t.Fatalf("%s: a refused create wrote a job (%d jobs, was %d)", name, n, before)
		}
	}
	// the job is the principal's: B cannot read it, and B's own create with the same parameter is B's
	e.json(B, "GET", "/v1/jobs/"+id, nil).refused(t, 404, "not_found")
	idB := jobIDOf(t, post(B, "?module=m1&email_column=work", "work\nbob@example.test\n"), 201)
	if got := e.str("SELECT user_id::text FROM platform.jobs WHERE id = $1", idB); got != B {
		t.Fatalf("B's job belongs to %s", got)
	}
	var m map[string]any
	if err := json.Unmarshal(e.json(A, "GET", "/v1/jobs/"+idB, nil).Body, &m); err != nil || m["error"] == nil {
		t.Fatalf("A reads B's job: %v (%v)", m, err)
	}
}
