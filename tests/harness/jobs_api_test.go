//go:build harness

package harness

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/csvsafe"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Phase 4 B14b: the job API over the whole stack in ONE process, against a real database. httptest.NewServer serves the real httpapi handler with the API mounted (Deps.Mount), over the real
// job store, money store and tenant resolver on a migrated scratch database; the tokens are minted per run by authtest (a random HS256 secret) and verified by the real internal/auth verifier. The
// unit tests of internal/api prove the HTTP behaviour against a fake store; what only a database can prove is proved here: the credits arithmetic against the ledger, the ownership of every route,
// the idempotency of a confirm, the numbers of a refusal, and a CSV that holds the platform's own results.
//
// Every name starts with `jap` so that it cannot clash with the other files of this package. Every test starts with module 1's vendor unit OFF (newJbsEnv: platform.set_unit_reachable, restored when
// the test ends), so a module 1 job reserves 1 credit per row; the one test that switches it on says so and switches it off again.

type japEnv struct {
	*jbsEnv
	srv  *httptest.Server
	mint *authtest.Minter
}

func newJapEnv(t *testing.T) *japEnv {
	t.Helper()
	e := newJbsEnv(t)
	mint := authtest.NewHS256(t)
	res, err := tenant.NewResolver(e.pool, "")
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	a, err := api.New(api.Deps{Service: e.jobs, Verifier: mint.Verifier(t), Resolver: res})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	h := httpapi.NewHandler(httpapi.Deps{Pinger: e.pool, ReadyTimeout: 5 * time.Second, Mount: a.Mount})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &japEnv{jbsEnv: e, srv: srv, mint: mint}
}

type japResp struct {
	Status int
	Header http.Header
	Body   []byte
}

// callAs sends one request as the user (a token of the login equal to the user's id; "" sends none).
func (e *japEnv) callAs(user, method, path, ctype string, body []byte, hdr ...string) japResp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	q, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if user != "" {
		q.Header.Set("Authorization", "Bearer "+e.mint.Token(user))
	}
	if ctype != "" {
		q.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		q.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(q)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return japResp{resp.StatusCode, resp.Header, b}
}

// json calls with a JSON body (nil sends none).
func (e *japEnv) json(user, method, path string, body any, hdr ...string) japResp {
	e.t.Helper()
	if body == nil {
		return e.callAs(user, method, path, "", nil, hdr...)
	}
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.callAs(user, method, path, "application/json", b, hdr...)
}

// obj decodes a JSON answer of the expected status.
func (r japResp) obj(t *testing.T, status int) map[string]any {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("the body is not a JSON object: %q", r.Body)
	}
	return m
}

// refused asserts an error envelope with this status and code and returns its details.
func (r japResp) refused(t *testing.T, status int, code string) map[string]any {
	t.Helper()
	m := r.obj(t, status)
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"] != code || len(m) != 1 {
		t.Fatalf("status %d, want the envelope %q: %s", r.Status, code, r.Body)
	}
	details, _ := e["details"].(map[string]any)
	return details
}

// normalised is the body with its request id blanked, to compare two answers that must be one answer.
func (r japResp) normalised() string {
	return fmt.Sprintf("%d %s", r.Status, regexpRID.ReplaceAllString(string(r.Body), "req_X"))
}

var regexpRID = regexp.MustCompile("req_[0-9a-f]{32}")

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func jobIDOf(t *testing.T, r japResp, status int) string {
	t.Helper()
	id, _ := dig(r.obj(t, status), "job", "id").(string)
	if id == "" {
		t.Fatalf("the answer holds no job: %s", r.Body)
	}
	return id
}

// create makes a JSON draft of the rows (each a plain address) over HTTP.
func (e *japEnv) create(user, module string, emails ...string) string {
	e.t.Helper()
	rows := make([]map[string]any, len(emails))
	for i, a := range emails {
		rows[i] = map[string]any{"email": a}
	}
	body := map[string]any{"module": module, "rows": rows}
	if module != "m1" {
		body["content"] = map[string]any{"subject": "Hello", "html": "<p>hi</p>", "text": "hi"}
	}
	return jobIDOf(e.t, e.json(user, "POST", "/v1/jobs", body), 201)
}

func (e *japEnv) previewJob(user, id string) (fingerprint string, preview map[string]any) {
	e.t.Helper()
	m := e.json(user, "POST", "/v1/jobs/"+id+"/preview", nil).obj(e.t, 200)
	preview, _ = dig(m, "job", "preview").(map[string]any)
	fingerprint, _ = preview["fingerprint"].(string)
	if fingerprint == "" {
		e.t.Fatalf("the preview has no fingerprint: %v", m)
	}
	return fingerprint, preview
}

func (e *japEnv) confirmJob(user, id, fingerprint, key string) japResp {
	e.t.Helper()
	return e.json(user, "POST", "/v1/jobs/"+id+"/confirm", map[string]any{"fingerprint": fingerprint}, "Idempotency-Key", key)
}

// recordFreeModuleResults stands in for the engine, which a later chunk builds: it writes a free result of the module for every PENDING row of the job, as the superuser, so that a job that was only
// confirmed has the result of every row. They are free, so the ledger is untouched; the settlement proof is therefore not asked about these jobs.
func (e *japEnv) recordFreeModuleResults(job string) {
	e.t.Helper()
	e.exec(`INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, reason, decision, unit, rule)
		SELECT r.job_id, r.user_id, r.id, r.ordinal, r.email_norm, 'module', 'valid', NULL, 'free', NULL, 'M1-15'
		FROM platform.job_rows r WHERE r.job_id = $1 AND r.state = 'pending'`, job)
}

// ---- the credits, end to end -------------------------------------------------------------------------------------

func TestJobsAPI_CreatePreviewAndConfirmModuleOneEndToEndWithTheCreditsOfTheLedger(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 1000)
	e.requireBal("funded", A, "m1", 1000, 0)

	// a list with every kind of row: four that reach the module, one that is not an address, one that repeats a row
	created := e.json(A, "POST", "/v1/jobs", map[string]any{"module": "m1", "rows": []map[string]any{
		{"email": "alice@example.test"}, {"email": "Bob@Example.TEST"}, {"email": "carol@example.test"}, {"email": "not-an-address"}, {"email": "ALICE@example.test"}, {"email": "dave@example.test"},
	}})
	job := created.obj(t, 201)["job"].(map[string]any)
	id := job["id"].(string)
	if job["status"] != "draft" || job["module"] != "m1" || created.Header.Get("Location") != "/v1/jobs/"+id {
		t.Fatalf("the created job is %v (Location %q)", job, created.Header.Get("Location"))
	}
	if _, has := job["user_id"]; has || strings.Contains(string(created.Body), A) {
		t.Fatalf("the job shows its owner: %s", created.Body)
	}
	if got := e.str("SELECT user_id::text FROM platform.jobs WHERE id = $1", id); got != A {
		t.Fatalf("the job belongs to %s, want the principal %s", got, A)
	}
	if got := e.stateCounts(id); !reflect.DeepEqual(got, map[string]int{"pending": 4, "malformed": 1, "duplicate": 1}) {
		t.Fatalf("the rows were classified as %v", got)
	}
	if got := e.json(A, "GET", "/v1/jobs/"+id, nil).obj(t, 200); dig(got, "job", "id") != id {
		t.Fatalf("GET of the job: %v", got)
	}

	// the preview: the numbers, hand-computed (4 rows reach the module, at 1 credit each with the vendor off)
	fp, p := e.previewJob(A, id)
	want := map[string]float64{"rows": 6, "pending": 4, "malformed": 1, "duplicates": 1, "suppressed": 0, "units": 4, "unit_cost": 1, "credits": 4, "available": 1000, "shortfall": 0}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("the preview says %s = %v, want %v", k, p[k], v)
		}
	}
	if p["metered"] != true || p["malformed_reasons"].(map[string]any)["no_at"] != float64(1) || len(fp) != 64 {
		t.Errorf("the preview is %v", p)
	}
	if got := e.json(A, "GET", "/v1/jobs/"+id, nil).obj(t, 200); dig(got, "job", "status") != "previewed" || dig(got, "job", "preview_fingerprint") != fp {
		t.Fatalf("after the preview the job is %v", got["job"])
	}

	// the confirm: the credits move exactly as the arithmetic says, and the ledger holds ONE reserve row of 4 units at 1
	key := e.key()
	c := e.confirmJob(A, id, fp, key).obj(t, 200)
	if dig(c, "job", "status") != "confirmed" || c["reserved"] != float64(4) || c["pending"] != float64(4) || c["suppressed"] != float64(0) || c["replayed"] != false {
		t.Fatalf("the confirm answered %v", c)
	}
	e.requireBal("after the confirm", A, "m1", 996, 4)
	if e.reserveRows(id) != 1 || e.int("SELECT reserved_delta FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", id) != 4 ||
		e.int("SELECT units FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", id) != 4 || e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", id) != 1 {
		t.Fatalf("the ledger does not hold one reserve of 4 units at 1 credit")
	}
	// credits are neither made nor lost: available plus reserved is what was granted
	if got := e.int("SELECT COALESCE(sum(available_delta + reserved_delta), 0) FROM platform.ledger WHERE user_id = $1 AND module_id = 'm1'", A); got != 1000 {
		t.Fatalf("the ledger of the user sums to %d, want the 1000 that was granted", got)
	}

	// the history over HTTP is the history the database wrote: created, then the two moves by the user
	ev := e.json(A, "GET", "/v1/jobs/"+id+"/events", nil).obj(t, 200)["events"].([]any)
	var got []string
	for _, x := range ev {
		m := x.(map[string]any)
		got = append(got, fmt.Sprintf("%v|%v>%v|%v", m["kind"], orEmpty(m["from_status"]), orEmpty(m["to_status"]), m["actor_kind"]))
	}
	if want := []string{"created|>|user", "transition|draft>previewed|user", "transition|previewed>confirmed|user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events over HTTP %v, want %v (the database holds %v)", got, want, e.eventLines(id))
	}
	if strings.Contains(string(e.json(A, "GET", "/v1/jobs/"+id+"/events", nil).Body), A) {
		t.Fatal("the events show the user's id")
	}

	// the results over HTTP are the platform's own, one per row that never reaches the module, with the rules of the charge table
	res := e.json(A, "GET", "/v1/jobs/"+id+"/results", nil).obj(t, 200)["results"].([]any)
	if len(res) != 2 {
		t.Fatalf("%d results, want the malformed row and the repeat", len(res))
	}
	rules := map[string]int{}
	for _, x := range res {
		m := x.(map[string]any)
		rules[m["rule"].(string)]++
		if m["origin"] != "platform" || m["decision"] != "free" || m["unit"] != nil {
			t.Errorf("a platform result is %v", m)
		}
	}
	if !reflect.DeepEqual(rules, e.ruleCounts(id)) {
		t.Fatalf("results over HTTP %v, the database holds %v", rules, e.ruleCounts(id))
	}
	e.requireBooks("after the confirm over HTTP")

	// the CONTROL of the arithmetic: with the vendor unit switched ON the same shape of job reserves at 2 per row. Switched off again at the end (the environment restores what it found as well).
	B := mnyUserB
	e.fund(B, "m1", 1000)
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', true, 'test: the control of the credits arithmetic')")
	idB := e.create(B, "m1", "x1@example.test", "x2@example.test", "x3@example.test")
	fpB, pB := e.previewJob(B, idB)
	if pB["unit_cost"] != float64(2) || pB["credits"] != float64(6) {
		t.Fatalf("with the vendor on the preview says %v, want 2 credits per row and 6", pB)
	}
	if cb := e.confirmJob(B, idB, fpB, e.key()).obj(t, 200); cb["reserved"] != float64(6) {
		t.Fatalf("with the vendor on the confirm reserved %v, want 6", cb["reserved"])
	}
	e.requireBal("the control job", B, "m1", 994, 6)
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', false, 'test: the vendor is off again')")
}

func orEmpty(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// ---- ownership -------------------------------------------------------------------------------------------------

// Every route of the API that names a job, asked by a SECOND user about the first user's job, is the answer a job that does not exist gets: the same status, the same body but for the request id, and
// not one byte written. The owner, asking the same, is served (the paired control).
func TestJobsAPI_ASecondUserCanNeverReachTheFirstUsersJobByAnyRoute(t *testing.T) {
	e := newJapEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 1000)
	e.fund(B, "m1", 1000)

	// A: a confirmed job with its results (the platform's, from the confirm, and a module's) and, beside it, a previewed one (a draft and a previewed job hold no slot)
	j1 := e.create(A, "m1", "a1@example.test", "a2@example.test", "not-an-address", "A1@example.test")
	fp1, _ := e.previewJob(A, j1)
	key1 := e.key()
	e.confirmJob(A, j1, fp1, key1).obj(t, 200)
	e.recordFreeModuleResults(j1)
	j2 := e.create(A, "m1", "b1@example.test", "b2@example.test")
	fp2, _ := e.previewJob(A, j2)
	// B has a job of his own, so that the lists can be told apart
	jb := e.create(B, "m1", "z1@example.test")

	missing := "0b000000-0000-4000-8000-0000000000aa"
	type route struct {
		name, method, path string
		body               any
		hdr                []string
	}
	routes := func(id, fp string) []route {
		return []route{
			{"get", "GET", "/v1/jobs/" + id, nil, nil},
			{"preview", "POST", "/v1/jobs/" + id + "/preview", nil, nil},
			{"confirm", "POST", "/v1/jobs/" + id + "/confirm", map[string]any{"fingerprint": fp}, []string{"Idempotency-Key", e.key()}},
			{"discard", "POST", "/v1/jobs/" + id + "/discard", nil, nil},
			{"reopen", "POST", "/v1/jobs/" + id + "/reopen", nil, nil},
			{"cancel", "POST", "/v1/jobs/" + id + "/cancel", nil, nil},
			{"pause", "POST", "/v1/jobs/" + id + "/pause", nil, nil},
			{"resume", "POST", "/v1/jobs/" + id + "/resume", nil, nil},
			{"events", "GET", "/v1/jobs/" + id + "/events", nil, nil},
			{"results", "GET", "/v1/jobs/" + id + "/results", nil, nil},
			{"results.csv", "GET", "/v1/jobs/" + id + "/results.csv", nil, nil},
		}
	}

	before := e.writes()
	for _, target := range []struct{ id, fp string }{{j1, fp1}, {j2, fp2}} {
		foreign, absent := routes(target.id, target.fp), routes(missing, target.fp)
		for i, r := range foreign {
			got := e.json(B, r.method, r.path, r.body, r.hdr...)
			got.refused(t, 404, "not_found")
			twin := e.json(B, absent[i].method, absent[i].path, absent[i].body, absent[i].hdr...)
			if got.normalised() != twin.normalised() {
				t.Errorf("%s: the job of another user is answered\n  %s\nand a job that does not exist\n  %s", r.name, got.normalised(), twin.normalised())
			}
		}
	}
	e.requireWrites("everything the second user tried on the first user's jobs", before)
	if e.jobStatus(j1) != "confirmed" || e.jobStatus(j2) != "previewed" {
		t.Fatalf("the first user's jobs are %s and %s after the second user's attempts", e.jobStatus(j1), e.jobStatus(j2))
	}

	// the lists are partitioned: each user sees his own job and not the other's
	ids := func(user string) []string {
		var out []string
		for _, x := range e.json(user, "GET", "/v1/jobs", nil).obj(t, 200)["jobs"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids(B); !reflect.DeepEqual(got, []string{jb}) {
		t.Fatalf("the second user's list is %v, want only his own job %s", got, jb)
	}
	if got := ids(A); len(got) != 2 || !((got[0] == j2 && got[1] == j1) || (got[0] == j1 && got[1] == j2)) {
		t.Fatalf("the first user's list is %v, want his two jobs", got)
	}

	// THE CONTROL: the owner is served by every one of those routes (the moves that the job's state refuses are refused for the state, not as a missing job)
	for _, r := range routes(j1, fp1) {
		got := e.json(A, r.method, r.path, r.body, r.hdr...)
		if got.Status == 404 || got.Status == 401 || got.Status >= 500 {
			t.Errorf("%s: the owner was answered %d: %s", r.name, got.Status, got.Body)
		}
	}
	for _, p := range []string{"/v1/jobs/" + j1, "/v1/jobs/" + j1 + "/events", "/v1/jobs/" + j1 + "/results", "/v1/jobs/" + j1 + "/results.csv"} {
		if got := e.json(A, "GET", p, nil); got.Status != 200 {
			t.Errorf("the owner's GET %s: %d %s", p, got.Status, got.Body)
		}
	}
	if got := e.json(A, "POST", "/v1/jobs/"+j2+"/preview", nil); got.Status != 200 {
		t.Errorf("the owner's preview: %d %s", got.Status, got.Body)
	}
	if got := e.json(B, "POST", "/v1/jobs/"+jb+"/preview", nil); got.Status != 200 {
		t.Errorf("the second user's preview of his own job: %d %s", got.Status, got.Body)
	}
}

// The real resolver, over the real view: a token that verifies for a login the platform will not act for is the same 401 as a forged one, and an active user's token beside it is served.
func TestJobsAPI_ATokenForALoginThePlatformWillNotActForIsRefusedByTheRealResolver(t *testing.T) {
	e := newJapEnv(t)
	inactive := e.newUser()
	e.exec("UPDATE public.users SET is_active = false WHERE id = $1", inactive)
	const authOnly = "d1d1d1d1-0000-4000-8000-0000000000d4" // a login in auth.users that no admin made a platform user of
	e.exec("INSERT INTO auth.users (id, email) VALUES ($1, 'jap-authonly@example.test')", authOnly)
	system := tenant.SystemUserID
	active := e.newUser()

	reference := ""
	for name, user := range map[string]string{"an inactive user": inactive, "a login with no platform user": authOnly, "the system user": system, "a login nobody has heard of": "e2e2e2e2-0000-4000-8000-0000000000e5"} {
		r := e.callAs(user, "GET", "/v1/jobs", "", nil)
		r.refused(t, 401, "unauthenticated")
		if r.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q", name, r.Header.Get("WWW-Authenticate"))
		}
		if reference == "" {
			reference = r.normalised()
		} else if r.normalised() != reference {
			t.Errorf("%s: the 401 differs from the others: %s", name, r.normalised())
		}
	}
	// forged and expired tokens are the very same answer
	forged := e.callAsToken(e.mint.Forged(e.mint.Good(active)), "GET", "/v1/jobs")
	expired := e.callAsToken(e.mint.Mint(e.mint.Good(active).With("exp", time.Now().Unix()-3600)), "GET", "/v1/jobs")
	for name, r := range map[string]japResp{"a forged token": forged, "an expired token": expired} {
		if r.refused(t, 401, "unauthenticated"); r.normalised() != reference {
			t.Errorf("%s: the 401 differs from the others: %s", name, r.normalised())
		}
	}
	// the control: the active user is served; and when the inactive user is switched back on, the very same token is served too
	if r := e.callAs(active, "GET", "/v1/jobs", "", nil); r.Status != 200 {
		t.Fatalf("an active user: %d %s", r.Status, r.Body)
	}
	e.exec("UPDATE public.users SET is_active = true WHERE id = $1", inactive)
	if r := e.callAs(inactive, "GET", "/v1/jobs", "", nil); r.Status != 200 {
		t.Fatalf("the user switched back on: %d %s", r.Status, r.Body)
	}
}

// callAsToken sends a GET with the token as given.
func (e *japEnv) callAsToken(token, method, path string) japResp {
	e.t.Helper()
	q, _ := http.NewRequest(method, e.srv.URL+path, nil)
	q.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(q)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return japResp{resp.StatusCode, resp.Header, b}
}

// ---- idempotency, staleness, shortfall -------------------------------------------------------------------------

func TestJobsAPI_TheSameIdempotencyKeyTwiceIsOneConfirmOverHTTP(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)
	id := e.create(A, "m1", "a1@example.test", "a2@example.test", "a3@example.test")
	fp, _ := e.previewJob(A, id)
	key := e.key()

	first := e.confirmJob(A, id, fp, key).obj(t, 200)
	afterFirst := e.writes()
	second := e.confirmJob(A, id, fp, key).obj(t, 200)
	if first["replayed"] != false || second["replayed"] != true {
		t.Fatalf("replayed: first %v, second %v, want false then true", first["replayed"], second["replayed"])
	}
	for _, k := range []string{"reserved", "pending", "suppressed"} {
		if first[k] != second[k] {
			t.Errorf("%s: first %v, second %v: a replay says what the first confirm did", k, first[k], second[k])
		}
	}
	if dig(first, "job", "id") != dig(second, "job", "id") || dig(second, "job", "status") != "confirmed" {
		t.Errorf("the replay answered %v", second["job"])
	}
	e.requireWrites("the replay", afterFirst) // nothing was written by the second request
	if e.reserveRows(id) != 1 {
		t.Fatalf("%d reserve rows after two confirms with one key, want 1", e.reserveRows(id))
	}
	e.requireBal("one confirm's credits", A, "m1", 97, 3)

	// the control: ANOTHER key on the confirmed job is not a confirm (the job is no longer previewed), and nothing changes
	e.confirmJob(A, id, fp, e.key()).refused(t, 409, "not_previewed")
	e.requireWrites("a confirm of a confirmed job under another key", afterFirst)

	// the same key for ANOTHER job of the user is refused as a reused key, and holds nothing
	id2 := e.create(A, "m1", "b1@example.test")
	fp2, _ := e.previewJob(A, id2)
	before := e.writes()
	e.confirmJob(A, id2, fp2, key).refused(t, 409, "idempotency_key_reused")
	e.requireWrites("a key that already confirmed another job", before)

	// a confirm with no key is refused before anything is read, and one with two keys too
	e.json(A, "POST", "/v1/jobs/"+id2+"/confirm", map[string]any{"fingerprint": fp2}).refused(t, 400, "validation_failed")
	e.json(A, "POST", "/v1/jobs/"+id2+"/confirm", map[string]any{"fingerprint": fp2}, "Idempotency-Key", e.key(), "Idempotency-Key", e.key()).refused(t, 400, "validation_failed")
	e.requireWrites("confirms without exactly one key", before)
	e.requireBooks("after the idempotency checks")
}

func TestJobsAPI_AStalePreviewIs409AndPreviewingAgainConfirmsAtTheNewPrice(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 1000)
	id := e.create(A, "m1", "a1@example.test", "a2@example.test", "a3@example.test", "a4@example.test", "a5@example.test")
	fp, p := e.previewJob(A, id)
	if p["credits"] != float64(5) {
		t.Fatalf("the preview says %v credits, want 5", p["credits"])
	}

	// the price of the unit changes after the preview: the confirm is refused, and nothing was written
	e.exec("UPDATE platform.unit_costs SET cost = 2, changed_by = $1, change_reason = 'stale test' WHERE unit = 'm1_own'", mnyAdminA)
	before := e.writes()
	e.confirmJob(A, id, fp, e.key()).refused(t, 409, "preview_stale")
	e.requireWrites("a stale confirm", before)
	if e.jobStatus(id) != "previewed" || e.reserveRows(id) != 0 {
		t.Fatalf("after the refusal the job is %s with %d reserve rows", e.jobStatus(id), e.reserveRows(id))
	}

	// a fingerprint that is not the job's, well formed or not
	e.confirmJob(A, id, strings.Repeat("0", 64), e.key()).refused(t, 409, "preview_stale")
	if d := e.confirmJob(A, id, "not-a-fingerprint", e.key()).refused(t, 400, "validation_failed"); d == nil || !strings.Contains(fmt.Sprint(d["reason"]), "fingerprint") {
		t.Errorf("a malformed fingerprint was refused with %v", d)
	}
	e.requireWrites("the wrong fingerprints", before)

	// THE CONTROL: preview again, and confirm what the new preview says: the reserve is at the new price
	fp2, p2 := e.previewJob(A, id)
	if fp2 == fp || p2["credits"] != float64(10) || p2["unit_cost"] != float64(2) {
		t.Fatalf("the second preview is %v (fingerprint changed: %v)", p2, fp2 != fp)
	}
	if c := e.confirmJob(A, id, fp2, e.key()).obj(t, 200); c["reserved"] != float64(10) {
		t.Fatalf("the confirm reserved %v, want 10", c["reserved"])
	}
	e.requireBal("after the confirm at the new price", A, "m1", 990, 10)
	e.requireBooks("after the stale-preview round")
}

func TestJobsAPI_AShortfallIs402WithTheNumbersAndNothingIsHeld(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 3)
	id := e.create(A, "m1", "a1@example.test", "a2@example.test", "a3@example.test", "a4@example.test", "a5@example.test")
	fp, p := e.previewJob(A, id)
	if p["credits"] != float64(5) || p["available"] != float64(3) || p["shortfall"] != float64(2) {
		t.Fatalf("the preview says %v, want 5 credits against 3 available (short by 2)", p)
	}
	before := e.writes()
	d := e.confirmJob(A, id, fp, e.key()).refused(t, 402, "insufficient_credits")
	if d["available"] != float64(3) || d["needed"] != float64(5) || d["shortfall"] != float64(2) || len(d) != 3 {
		t.Fatalf("the refusal's numbers are %v, want available 3, needed 5, shortfall 2", d)
	}
	e.requireWrites("a confirm the balance does not cover", before)
	e.requireBal("nothing is held", A, "m1", 3, 0)

	// THE CONTROL: two more credits and the very same confirm goes through
	e.fund(A, "m1", 2)
	fp, _ = e.previewJob(A, id)
	if c := e.confirmJob(A, id, fp, e.key()).obj(t, 200); c["reserved"] != float64(5) {
		t.Fatalf("with the credits the confirm reserved %v, want 5", c["reserved"])
	}
	e.requireBal("after funding", A, "m1", 0, 5)
	e.requireBooks("after the shortfall round")
}

// ---- the moves, at the boundary ----------------------------------------------------------------------------------

func TestJobsAPI_TheMovesAreTypedWhereAModuleCannotAndFreeWhereNothingStarted(t *testing.T) {
	e := newJapEnv(t)
	A, B := mnyUserA, mnyUserB
	e.fund(A, "m1", 100)
	e.fund(B, "m2", 100)

	// a confirmed job that has not started: cancelled over HTTP, and the credits come back
	id := e.create(A, "m1", "a1@example.test", "a2@example.test")
	fp, _ := e.previewJob(A, id)
	e.confirmJob(A, id, fp, e.key()).obj(t, 200)
	e.requireBal("held", A, "m1", 98, 2)
	if got := e.json(A, "POST", "/v1/jobs/"+id+"/cancel", nil).obj(t, 200); dig(got, "job", "status") != "cancelled" {
		t.Fatalf("the cancel answered %v", got)
	}
	e.requireBal("released", A, "m1", 100, 0)
	e.requireBooks("after the cancel")

	// a running module 1 job cannot be cancelled or paused: the database says so, the API says it in words
	id2 := e.create(A, "m1", "b1@example.test", "b2@example.test", "b3@example.test")
	fp2, _ := e.previewJob(A, id2)
	e.confirmJob(A, id2, fp2, e.key()).obj(t, 200)
	jobWalkTo(t, e.admin, id2, "running")
	before := e.writes()
	if d := e.json(A, "POST", "/v1/jobs/"+id2+"/cancel", nil).refused(t, 409, "not_allowed"); d["reason"] != "cancel_not_supported" {
		t.Errorf("cancelling a running module 1 job: %v", d)
	}
	if d := e.json(A, "POST", "/v1/jobs/"+id2+"/pause", nil).refused(t, 409, "not_allowed"); d["reason"] != "pause_not_supported" {
		t.Errorf("pausing a running job: %v", d)
	}
	e.requireWrites("the refused moves of a running job", before)

	// a confirmed job is not discarded (it holds credits): the store's word, not a constraint name
	id3 := e.create(B, "m2", "c1@example.test", "c2@example.test")
	fp3, _ := e.previewJob(B, id3)
	e.confirmJob(B, id3, fp3, e.key()).obj(t, 200)
	d := e.json(B, "POST", "/v1/jobs/"+id3+"/discard", nil).refused(t, 409, "not_allowed")
	if d["reason"] != "not_a_draft" || strings.Contains(fmt.Sprint(d), "job_") {
		t.Errorf("discarding a confirmed job: %v", d)
	}

	// THE CONTROL of the refusals above: module 2 CAN be cancelled while it runs (the database's capability, not a rule of the API): the same request is a 200 and the job is cancelling
	jobWalkTo(t, e.admin, id3, "running")
	if got := e.json(B, "POST", "/v1/jobs/"+id3+"/cancel", nil).obj(t, 200); dig(got, "job", "status") != "cancelling" || dig(got, "job", "cancel_requested_at") == nil {
		t.Fatalf("cancelling a running module 2 job answered %v", got["job"])
	}
	e.requireBal("a cancelling job still holds its reserve", B, "m2", 98, 2)

	// a scheduled job is paused and resumed over HTTP (nothing was submitted: the slot and the reserve stay), and refuses a resume that is not one
	e.fund(A, "m2", 100)
	id4 := e.create(A, "m2", "d1@example.test")
	fp4, _ := e.previewJob(A, id4)
	start := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	sch := e.json(A, "POST", "/v1/jobs/"+id4+"/confirm", map[string]any{"fingerprint": fp4, "scheduled_start": start}, "Idempotency-Key", e.key()).obj(t, 200)
	if dig(sch, "job", "status") != "scheduled" || dig(sch, "job", "scheduled_start") == nil {
		t.Fatalf("the scheduled confirm answered %v", sch["job"])
	}
	if got := e.json(A, "POST", "/v1/jobs/"+id4+"/pause", nil).obj(t, 200); dig(got, "job", "status") != "paused" || dig(got, "job", "paused_from") != "scheduled" {
		t.Fatalf("the pause answered %v", got["job"])
	}
	e.requireBal("a paused job holds its reserve", A, "m2", 99, 1)
	e.json(A, "POST", "/v1/jobs/"+id4+"/resume", nil).obj(t, 200)
	if d := e.json(A, "POST", "/v1/jobs/"+id4+"/resume", nil).refused(t, 409, "not_allowed"); d["reason"] != "not_paused" {
		t.Errorf("resuming a job that is not paused: %v", d)
	}
	// and reopen takes a previewed job back to draft; a draft cannot be reopened
	id5 := e.create(A, "m1", "e1@example.test")
	e.previewJob(A, id5)
	if got := e.json(A, "POST", "/v1/jobs/"+id5+"/reopen", nil).obj(t, 200); dig(got, "job", "status") != "draft" || dig(got, "job", "preview") != nil {
		t.Fatalf("the reopen answered %v", got["job"])
	}
	if d := e.json(A, "POST", "/v1/jobs/"+id5+"/reopen", nil).refused(t, 409, "not_allowed"); d["reason"] != "already_a_draft" {
		t.Errorf("reopening a draft: %v", d)
	}
	e.requireBooks("after the moves")
}

// ---- the results and the CSV ---------------------------------------------------------------------------------------

// The typed addresses of a list that are not addresses and read like formulas or like CSV: what a customer's own file can hold.
var japAwkward = []string{
	"=cmd|' /C calc'!A0", `"quoted", and, comma`, "@SUM(1,1)", "+1 line\nbreak", "'leading-apostrophe", "-1@", "\ttab-led",
}

func TestJobsAPI_TheCSVOfAConfirmedJobHoldsOneLinePerRowAndReadsBackExactly(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 1000)
	e.suppressFor(A, "bob@example.test")

	inputs := []string{"alice@example.test", "bob@example.test", japAwkward[0], "ALICE@example.test", japAwkward[1], japAwkward[2], japAwkward[3], "carol@example.test", japAwkward[4], japAwkward[5], japAwkward[6]}
	rows := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		rows[i] = map[string]any{"email": in}
	}
	id := jobIDOf(t, e.json(A, "POST", "/v1/jobs", map[string]any{"module": "m1", "rows": rows}), 201)
	fp, p := e.previewJob(A, id)
	if p["rows"] != float64(11) || p["pending"] != float64(3) || p["malformed"] != float64(7) || p["duplicates"] != float64(1) {
		t.Fatalf("the preview is %v", p)
	}
	// owner ruling OD-3 (this test was written when a verify job left a suppressed address out): bob is counted as suppressed and is STILL verified, so the preview prices all three rows
	if p["suppressed"] != float64(1) || p["suppressed_still_verified"] != true || p["units"] != float64(3) || p["credits"] != float64(3) {
		t.Fatalf("the preview of a verify job with a suppressed address is %v, want suppressed 1, still verified, 3 units, 3 credits", p)
	}
	c := e.confirmJob(A, id, fp, e.key()).obj(t, 200)
	if c["pending"] != float64(3) || c["suppressed"] != float64(1) || c["reserved"] != float64(3) {
		t.Fatalf("the confirm is %v: alice, bob and carol reach the module (bob is on the user's own suppression list and is verified all the same), one row carries the flag", c)
	}
	// the confirm wrote the platform's own results (7 malformed, 1 repeat: a verify job's suppressed row is the module's, not the platform's); the module's three are written here (the engine does it later), free
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", id); n != 8 {
		t.Fatalf("%d results after the confirm, want the platform's 8", n)
	}
	e.recordFreeModuleResults(id)

	resp := e.callAs(A, "GET", "/v1/jobs/"+id+"/results.csv", "", nil)
	if resp.Status != 200 || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" || resp.Header.Get("Content-Disposition") != `attachment; filename="job-`+id+`.csv"` {
		t.Fatalf("%d %v", resp.Status, resp.Header)
	}
	cr := csv.NewReader(bytes.NewReader(resp.Body))
	cr.FieldsPerRecord = 9
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("the file is not a CSV: %v\n%s", err, resp.Body)
	}
	if len(recs) != 1+len(inputs) {
		t.Fatalf("%d lines, want the header and one per row (%d)", len(recs), len(inputs))
	}
	if strings.Join(recs[0], ",") != "ordinal,email,origin,state,reason,decision,unit,rule,suppressed" {
		t.Fatalf("the header is %v", recs[0])
	}
	// what the database holds, row by row, against what the file says once the escape is undone; and the typed text of a malformed row exactly as it was typed
	db := e.rows(`SELECT ordinal, origin, module_state, COALESCE(reason, ''), decision, COALESCE(unit, ''), rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, id)
	wantStates := map[int][2]string{ // ordinal -> origin, state: written out by hand from the list above
		1: {"module", "valid"}, 2: {"module", "valid"}, 3: {"platform", "malformed"}, 4: {"platform", "duplicate"}, 5: {"platform", "malformed"}, 6: {"platform", "malformed"},
		7: {"platform", "malformed"}, 8: {"module", "valid"}, 9: {"platform", "malformed"}, 10: {"platform", "malformed"}, 11: {"platform", "malformed"},
	}
	for i, rec := range recs[1:] {
		ord := i + 1
		if rec[0] != fmt.Sprint(ord) {
			t.Errorf("line %d has ordinal %s", ord, rec[0])
		}
		for col, cell := range rec {
			if csvsafe.Triggers(cell) {
				t.Errorf("line %d column %d: the cell %q opens as a formula", ord, col, cell)
			}
		}
		if rec[2] != wantStates[ord][0] || rec[3] != wantStates[ord][1] {
			t.Errorf("line %d is %s/%s, want %v", ord, rec[2], rec[3], wantStates[ord])
		}
		// the new last column: true for bob (line 2) and for no other line (the eight older columns are where they were)
		if want := fmt.Sprint(ord == 2); rec[8] != want {
			t.Errorf("line %d: the suppressed column is %q, want %q", ord, rec[8], want)
		}
		if got := rec[2] + "|" + rec[3]; !strings.Contains(db[i], "|"+rec[2]+"|"+rec[3]+"|") {
			t.Errorf("line %d (%s) does not match the database row %q", ord, got, db[i])
		}
		if wantStates[ord][1] == "malformed" {
			if got := csvsafe.Unescape(rec[1]); got != inputs[i] {
				t.Errorf("line %d: the malformed address reads back as %q, want exactly what was typed, %q", ord, got, inputs[i])
			}
			if rec[5] != "free" || rec[7] != "P-08" || rec[6] != "" {
				t.Errorf("line %d: a malformed row is %v, want free, no unit, rule P-08", ord, rec)
			}
		}
	}
	// the cells that start with a trigger really carry the apostrophe in the file (the control of the check above: the raw text of the file holds them)
	for _, in := range []string{japAwkward[0], japAwkward[2], japAwkward[5]} {
		if !bytes.Contains(resp.Body, []byte("'"+in)) {
			t.Errorf("the file does not hold %q with its escape", in)
		}
	}
	// the same job over the JSON route says the same, row for row
	var jsonOrdinals []int
	for _, x := range e.json(A, "GET", "/v1/jobs/"+id+"/results?limit=100", nil).obj(t, 200)["results"].([]any) {
		m := x.(map[string]any)
		jsonOrdinals = append(jsonOrdinals, int(m["ordinal"].(float64)))
		if want := m["ordinal"] == float64(2); m["suppressed"] != want { // the flag over JSON is the CSV's column: true on bob alone
			t.Errorf("JSON result %v: suppressed = %v, want %v", m["ordinal"], m["suppressed"], want)
		}
	}
	if len(jsonOrdinals) != len(inputs) || jsonOrdinals[0] != 1 || jsonOrdinals[len(jsonOrdinals)-1] != len(inputs) {
		t.Fatalf("the JSON results are %v", jsonOrdinals)
	}
}

func TestJobsAPI_ResultsPaginateByCursorAcrossAPageBoundaryWithNoRepeatAndNoSkip(t *testing.T) {
	e := newJapEnv(t)
	A := mnyUserA
	e.fund(A, "m1", 100)
	emails := make([]string, 25)
	for i := range emails {
		emails[i] = fmt.Sprintf("p%02d@example.test", i+1)
	}
	id := e.create(A, "m1", emails...)
	fp, _ := e.previewJob(A, id)
	e.confirmJob(A, id, fp, e.key()).obj(t, 200)
	e.recordFreeModuleResults(id)
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", id); n != 25 {
		t.Fatalf("%d results, want 25", n)
	}

	var seen []int
	cursor, pages := "", 0
	for {
		path := "/v1/jobs/" + id + "/results?limit=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		m := e.json(A, "GET", path, nil).obj(t, 200)
		page := m["results"].([]any)
		for _, x := range page {
			seen = append(seen, int(x.(map[string]any)["ordinal"].(float64)))
		}
		pages++
		next, _ := m["next_cursor"].(string)
		if next == "" {
			if len(page) != 5 {
				t.Fatalf("the last page has %d results, want 5", len(page))
			}
			break
		}
		if len(page) != 10 {
			t.Fatalf("a page of %d came with a next cursor", len(page))
		}
		cursor = next
		if pages > 5 {
			t.Fatal("the walk does not end")
		}
	}
	if pages != 3 || len(seen) != 25 {
		t.Fatalf("%d pages with %d results, want 3 pages and 25 results", pages, len(seen))
	}
	for i, o := range seen {
		if o != i+1 {
			t.Fatalf("result %d of the walk has ordinal %d: a repeat or a skip across a boundary (%v)", i+1, o, seen)
		}
	}

	// the controls: one page of 25 has no next cursor; exactly 20 results with a page of 10 end on the second page; a cursor nobody gave is refused
	one := e.json(A, "GET", "/v1/jobs/"+id+"/results?limit=25", nil).obj(t, 200)
	if len(one["results"].([]any)) != 25 || one["next_cursor"] != nil {
		t.Fatalf("a page of 25: %d results, next cursor %v", len(one["results"].([]any)), one["next_cursor"])
	}
	e.json(A, "GET", "/v1/jobs/"+id+"/results?cursor=not-a-cursor", nil).refused(t, 400, "validation_failed")
	e.json(A, "GET", "/v1/jobs/"+id+"/results?limit=1001", nil).refused(t, 400, "validation_failed")
	// and the stream of the same job is the same 25 rows, in order, after its header
	recs, err := csv.NewReader(bytes.NewReader(e.callAs(A, "GET", "/v1/jobs/"+id+"/results.csv", "", nil).Body)).ReadAll()
	if err != nil || len(recs) != 26 {
		t.Fatalf("the CSV has %d records (%v), want the header and 25", len(recs), err)
	}
	for i, rec := range recs[1:] {
		if rec[0] != fmt.Sprint(i+1) || rec[1] != emails[i] {
			t.Fatalf("CSV line %d is %v", i+1, rec)
		}
	}
}
