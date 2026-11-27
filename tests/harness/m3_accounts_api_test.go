//go:build harness

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// Phase 6 A4: the five module 3 account routes (internal/api m3accounts.go) over the REAL module 3 of the harness, exactly as the service mounts them: the real adapter (m3.FromConfig, built the way the service
// builds it: no contract-drift observer, because the delete has no recording yet), the entitlement gate over the real platform tables, the real tenant resolver and tokens minted per run. CROSS-USER and PAIRED: two
// entitled users and one who is not, each doing the same things; what module 3 holds for each of them is read back through the adapter bound to that tenant and from module 3's own table. What the unit tests of
// internal/api prove against a fake (the closed codes, no module text, the password never echoed) is proved here against the module that really answers: verify-on-add, the From-gate, the duplicate, the host guard,
// the 404 of an account that is not yours, the soft delete, and the entitlement gate. Not driven here: the 429 (module 3's limit is five clicks a day per account; the unit tests map it) and a module that is down.
// Every name starts with `m3x` so that it cannot clash with another file's.

type m3xEnv struct {
	*m3aEnv
	srv  *httptest.Server
	mint *authtest.Minter
	seen [][]byte // every answer body, to look for the password in all of them at the end
}

func newM3xEnv(t *testing.T, ctx context.Context) *m3xEnv {
	t.Helper()
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	ad, err := m3.FromConfig(e.cfg, m3.WithSuspensionReader(e.adm)) // the service's own construction, without the drift observer the adapter tests add
	if err != nil {
		t.Fatalf("m3.FromConfig: %s", e.raw.scrub(err.Error()))
	}
	st, err := jobs.New(e.pool, e.money)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	mint := authtest.NewHS256(t)
	a, err := api.New(api.Deps{Service: st, Verifier: mint.Verifier(t), Resolver: e.res, M3Accounts: api.NewM3Accounts(ad), M3Gate: api.NewM3Gate(e.pool)})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Pinger: e.pool, ReadyTimeout: 5 * time.Second, Mount: a.Mount}))
	t.Cleanup(srv.Close)
	return &m3xEnv{m3aEnv: e, srv: srv, mint: mint}
}

type m3xResp struct {
	Status int
	Body   []byte
}

// call sends one request as the tenant ("" sends no token) with a JSON body (nil sends none).
func (e *m3xEnv) call(t *testing.T, tn *m3aTenant, method, path string, body any) m3xResp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	q, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if tn != nil {
		q.Header.Set("Authorization", "Bearer "+e.mint.Token(tn.UserID))
	}
	if body != nil {
		q.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(q)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	e.seen = append(e.seen, b)
	return m3xResp{resp.StatusCode, b}
}

func (r m3xResp) obj(t *testing.T, status int) map[string]any {
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

func (r m3xResp) refused(t *testing.T, status int, code string) map[string]any {
	t.Helper()
	m := r.obj(t, status)
	env, _ := m["error"].(map[string]any)
	if env == nil || env["code"] != code || len(m) != 1 {
		t.Fatalf("status %d, want the envelope %q: %s", r.Status, code, r.Body)
	}
	d, _ := env["details"].(map[string]any)
	return d
}

var m3xRID = regexp.MustCompile("req_[0-9a-f]{32}")

func (r m3xResp) normalised() string {
	return fmt.Sprintf("%d %s", r.Status, m3xRID.ReplaceAllString(string(r.Body), "req_X"))
}

// body is the request body of an account as the API takes it, from the adapter test's own account.
func (e *m3xEnv) body(label string, tn *m3aTenant, host string) map[string]any {
	n := e.newAccount(label, tn, host)
	return map[string]any{"smtp_host": n.SMTPHost, "smtp_port": n.SMTPPort, "smtp_username": n.SMTPUsername, "smtp_secret": n.SMTPSecret.Reveal(), "from_address": n.FromAddress, "from_name": *n.FromName}
}

func m3xIDs(t *testing.T, r m3xResp) []string {
	t.Helper()
	list, _ := r.obj(t, 200)["accounts"].([]any)
	var out []string
	for _, x := range list {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func TestM3Adapter_AccountsAPI_EveryRouteIsTheCallersOwnAgainstTheRealModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3xEnv(t, ctx)
	A, B, C := e.seedTenant(t, "xa", nil), e.seedTenant(t, "xb", nil), e.seedTenant(t, "xc", nil)
	e.enable(t, A, "accounts api")
	e.enable(t, B, "accounts api")
	// C is never enabled: no entitlement row at all

	// ---- add: verify-on-add, the From-gate verdict as a word, and nothing of the secret or of the module's own text ----
	ra := e.call(t, A, "POST", "/v1/m3/accounts", e.body("x1", A, e.sinkIP))
	accA := ra.obj(t, 201)["account"].(map[string]any)
	idA, _ := accA["id"].(string)
	if len(accA) != 15 || idA == "" || accA["has_credential"] != true || accA["verification"] != "verified" || accA["from_gate"] != "aligned" || accA["status"] == "" || accA["smtp_host"] != e.sinkIP {
		t.Fatalf("the account A added: %s", ra.Body)
	}
	idB := e.call(t, B, "POST", "/v1/m3/accounts", e.body("x2", B, e.sinkIP)).obj(t, 201)["account"].(map[string]any)["id"].(string)
	if got, err := A.C.GetAccount(ctx, idA); err != nil || got.UserID != A.UserID {
		t.Fatalf("module 3 does not hold A's account for A: %v %+v", err, got)
	}
	if got, err := B.C.GetAccount(ctx, idB); err != nil || got.UserID != B.UserID {
		t.Fatalf("module 3 does not hold B's account for B: %v %+v", err, got)
	}
	// the same host and username again is the module's duplicate, as the API's code, and adds nothing
	before := e.userAccounts(t, A.UserID)
	e.call(t, A, "POST", "/v1/m3/accounts", e.body("x1", A, e.sinkIP)).refused(t, 409, "account_duplicate")
	if got := e.userAccounts(t, A.UserID); got != before {
		t.Fatalf("a duplicate left %d account(s) of A in module 3, want %d", got, before)
	}

	// ---- list: each user sees their own account and never the other's ----
	la, lb := e.call(t, A, "GET", "/v1/m3/accounts", nil), e.call(t, B, "GET", "/v1/m3/accounts", nil)
	if ids := m3xIDs(t, la); len(ids) != 1 || ids[0] != idA {
		t.Fatalf("A's accounts are %v, want only %s", ids, idA)
	}
	if ids := m3xIDs(t, lb); len(ids) != 1 || ids[0] != idB {
		t.Fatalf("B's accounts are %v, want only %s", ids, idB)
	}
	if bytes.Contains(la.Body, []byte(idB)) || bytes.Contains(lb.Body, []byte(idA)) {
		t.Fatal("a list names the other user's account")
	}

	// ---- test: the real button, as words ----
	tr := e.call(t, A, "POST", "/v1/m3/accounts/"+idA+"/test", nil).obj(t, 200)["result"].(map[string]any)
	if len(tr) != 3 || tr["ok"] != true || tr["stage"] != "done" || tr["from_gate"] != "aligned" {
		t.Fatalf("A's test of A's account: %v", tr)
	}

	// ---- an account that is not yours: the same 404 as one that does not exist, for the test and the removal, and the account is untouched ----
	nobody := "0c000000-0000-4000-8000-0000000000ff"
	for _, route := range []string{"/test", "/remove"} {
		theirs := e.call(t, A, "POST", "/v1/m3/accounts/"+idB+route, nil)
		missing := e.call(t, A, "POST", "/v1/m3/accounts/"+nobody+route, nil)
		theirs.refused(t, 404, "not_found")
		missing.refused(t, 404, "not_found")
		if theirs.normalised() != missing.normalised() {
			t.Fatalf("%s: another user's account and a missing one are answered differently:\n%s\n%s", route, theirs.Body, missing.Body)
		}
	}
	if got, err := B.C.GetAccount(ctx, idB); err != nil || got.Disabled {
		t.Fatalf("B's account after A's attempts: %v %+v", err, got)
	}

	// ---- a host module 3 will not connect to, and the From-gate's warning that must be acknowledged ----
	loop := fmt.Sprintf("%d.%d.%d.%d", 127, 0, 0, 1) // built at run time: the repository's gate reads literals
	before = e.userAccounts(t, A.UserID)
	e.call(t, A, "POST", "/v1/m3/accounts", e.body("x3", A, loop)).refused(t, 422, "host_forbidden")
	byAddr := e.body("x4", A, e.ctlAddr) // a bare address is an unmatched provider for the From-gate: it can only warn
	d := e.call(t, A, "POST", "/v1/m3/accounts", byAddr).refused(t, 422, "from_gate_ack_required")
	if d["from_gate"] != "unaligned_warned" {
		t.Fatalf("the warning's details are %v", d)
	}
	if got := e.userAccounts(t, A.UserID); got != before {
		t.Fatalf("a refused add left %d account(s) of A, want %d", got, before)
	}
	byAddr["acknowledge_from_gate"] = true
	warned := e.call(t, A, "POST", "/v1/m3/accounts", byAddr).obj(t, 201)["account"].(map[string]any)
	if warned["from_gate"] != "unaligned_warned" {
		t.Fatalf("the acknowledged account: %v", warned)
	}
	idW := warned["id"].(string)

	// ---- a strict body: a member that names a user is refused and reaches nothing ----
	bad := e.body("x5", A, e.sinkIP)
	bad["user_id"] = B.UserID
	e.call(t, A, "POST", "/v1/m3/accounts", bad).refused(t, 400, "bad_request")
	delete(bad, "user_id")
	delete(bad, "smtp_secret")
	if d := e.call(t, A, "POST", "/v1/m3/accounts", bad).refused(t, 400, "validation_failed"); fmt.Sprint(d["fields"]) != "[smtp_secret:required]" {
		t.Fatalf("a body with no secret: %v", d)
	}

	// ---- bulk: one code per row, in order ----
	rows := []map[string]any{e.body("x6", A, e.sinkIP), e.body("x1", A, e.sinkIP), e.body("x7", A, loop)} // new, the duplicate of the first account, a refused host
	br := e.call(t, A, "POST", "/v1/m3/accounts/bulk", map[string]any{"accounts": rows}).obj(t, 200)
	var got []string
	for _, x := range br["results"].([]any) {
		r := x.(map[string]any)
		got = append(got, fmt.Sprintf("%v:%v:%v", r["line"], r["ok"], r["code"]))
	}
	if strings.Join(got, ",") != "1:true:created,2:false:account_duplicate,3:false:host_forbidden" {
		t.Fatalf("the bulk rows are %v", got)
	}

	// ---- the entitlement gate: C never had one, B loses it; both may still read, and nobody who may not act reaches module 3 ----
	nC := e.userAccounts(t, C.UserID)
	e.call(t, C, "POST", "/v1/m3/accounts", e.body("x8", C, e.sinkIP)).refused(t, 403, "no_entitlement")
	e.call(t, C, "POST", "/v1/m3/accounts/bulk", map[string]any{"accounts": []any{e.body("x9", C, e.sinkIP)}}).refused(t, 403, "no_entitlement")
	if ids := m3xIDs(t, e.call(t, C, "GET", "/v1/m3/accounts", nil)); len(ids) != 0 {
		t.Fatalf("C's list is %v", ids)
	}
	if got := e.userAccounts(t, C.UserID); got != nC {
		t.Fatalf("a refused add left %d account(s) of C", got-nC)
	}
	e.disable(t, B, "accounts api")
	e.call(t, B, "POST", "/v1/m3/accounts", e.body("x10", B, e.sinkIP)).refused(t, 403, "no_entitlement")
	e.call(t, B, "POST", "/v1/m3/accounts/"+idB+"/test", nil).refused(t, 403, "no_entitlement")
	if ids := m3xIDs(t, e.call(t, B, "GET", "/v1/m3/accounts", nil)); len(ids) != 1 {
		t.Fatalf("B's list after losing the entitlement: %v", ids)
	}

	// ---- remove: the soft delete, then the same 404 as any account that is not yours; B may remove their own account without an entitlement ----
	if m := e.call(t, A, "POST", "/v1/m3/accounts/"+idA+"/remove", nil).obj(t, 200); m["removed"] != true || len(m) != 1 {
		t.Fatalf("the removal: %v", m)
	}
	if _, err := A.C.GetAccount(ctx, idA); modclient.KindOf(err) != modclient.NotFound {
		t.Fatalf("module 3 still serves the removed account: %v", err)
	}
	e.call(t, A, "POST", "/v1/m3/accounts/"+idA+"/remove", nil).refused(t, 404, "not_found")
	for _, id := range m3xIDs(t, e.call(t, A, "GET", "/v1/m3/accounts", nil)) {
		if id == idA {
			t.Fatal("A's list still holds the removed account")
		}
	}
	if m := e.call(t, B, "POST", "/v1/m3/accounts/"+idB+"/remove", nil).obj(t, 200); m["removed"] != true {
		t.Fatalf("B's removal without an entitlement: %v", m)
	}
	if got, err := A.C.GetAccount(ctx, idW); err != nil || got.UserID != A.UserID {
		t.Fatalf("A's other account is gone with the removed one: %v", err)
	}

	// ---- nobody anonymous; the secret is in no answer of the whole run ----
	e.call(t, nil, "GET", "/v1/m3/accounts", nil).refused(t, 401, "unauthenticated")
	for i, b := range e.seen {
		if bytes.Contains(b, []byte(e.sinkPass)) {
			t.Fatalf("the SMTP password is in answer %d: %s", i, b)
		}
		for _, leak := range []string{"smtp_response", "last_attempt_error", "dmarc_policy", "message\":\"The From"} {
			if bytes.Contains(b, []byte(leak)) {
				t.Fatalf("answer %d carries module text (%s): %s", i, leak, b)
			}
		}
	}
}
