package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// GET /v1/me and GET /v1/balances (account.go; Phase 6 A3). What the API owns here is that the user is the authenticated principal's and nothing else, that the address is the DATABASE's and never a claim
// of the token, that what the services answer is what the client is told, and that the module 3 state is only ever one of the platform's three words. What the services read (the view, the balances, the
// entitlement and its bookkeeping, every statement scoped by the user) is proved against a real PostgreSQL by tests/harness/jobs_api_account_test.go.

type accountCall struct {
	Method string
	User   string
	Module string // of a Balance
}

// fakeAccount is the account service as the API sees it: one set of numbers per user, every call recorded with the user it was given, and an error to answer when told.
type fakeAccount struct {
	mu       sync.Mutex
	calls    []accountCall
	emails   map[string]string
	balances map[string]map[string]money.Balance // user -> module -> balance
	ents     map[string]entitlements.Entitlement
	syncs    map[string]entitlements.SyncState
	fail     map[string]error // by method name
}

func newFakeAccount() *fakeAccount {
	return &fakeAccount{
		emails:   map[string]string{},
		balances: map[string]map[string]money.Balance{},
		ents:     map[string]entitlements.Entitlement{},
		syncs:    map[string]entitlements.SyncState{},
		fail:     map[string]error{},
	}
}

func (f *fakeAccount) rec(method, user, module string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountCall{method, user, module})
	return f.fail[method]
}

func (f *fakeAccount) Email(_ context.Context, user string) (string, error) {
	if err := f.rec("Email", user, ""); err != nil {
		return "", err
	}
	return f.emails[user], nil
}

func (f *fakeAccount) Balance(_ context.Context, user, module string) (money.Balance, error) {
	if err := f.rec("Balance", user, module); err != nil {
		return money.Balance{}, err
	}
	return f.balances[user][module], nil
}

func (f *fakeAccount) Entitlement(_ context.Context, user string) (entitlements.Entitlement, error) {
	if err := f.rec("Entitlement", user, ""); err != nil {
		return entitlements.Entitlement{}, err
	}
	return f.ents[user], nil
}

func (f *fakeAccount) M3Sync(_ context.Context, user string) (entitlements.SyncState, error) {
	if err := f.rec("M3Sync", user, ""); err != nil {
		return entitlements.SyncState{}, err
	}
	return f.syncs[user], nil
}

func (f *fakeAccount) seen() []accountCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]accountCall(nil), f.calls...)
}

func (f *fakeAccount) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// seedTwoUsers gives each of the two users numbers that differ in every member, so that an answer holding the other user's number is caught by any one of them.
func (f *fakeAccount) seedTwoUsers() {
	until := t0.Add(48 * time.Hour)
	f.emails[userA], f.emails[userB] = "ann@example.test", "bob@example.test"
	f.balances[userA] = map[string]money.Balance{"m1": {Available: 1000, Reserved: 25, Exists: true}, "m2": {Available: 70, Reserved: 5, Exists: true}}
	f.balances[userB] = map[string]money.Balance{"m1": {Available: 9, Reserved: 1, Exists: true}, "m2": {Available: 4000, Reserved: 300, Exists: true}}
	f.ents[userA] = entitlements.Entitlement{UserID: userA, Exists: true, Enabled: true, Current: true, Until: &until}
	f.ents[userB] = entitlements.Entitlement{UserID: userB, Exists: true, Enabled: false}
	f.syncs[userA] = entitlements.SyncState{UserID: userA, Exists: true, Observed: "active"}
	f.syncs[userB] = entitlements.SyncState{UserID: userB, Exists: true, Observed: "suspended"}
}

func TestMe_IsTheDatabasesAddressOfThePrincipalAndNeverAClaimOfTheToken(t *testing.T) {
	r := newRig(t)
	r.account.seedTwoUsers()

	resp, body := r.get(loginA, "/v1/me")
	m := r.wantStatus(resp, body, 200)
	if len(m) != 1 || m["email"] != "ann@example.test" {
		t.Fatalf("GET /v1/me = %s, want exactly {\"email\":\"ann@example.test\"}", body)
	}
	if got := r.account.seen(); len(got) != 1 || got[0] != (accountCall{"Email", userA, ""}) {
		t.Fatalf("the service was asked %+v, want ONE read for the PLATFORM user of the login (%s), never the login's own id", got, userA)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q on an authenticated answer", resp.Header.Get("Cache-Control"))
	}

	// a token that CLAIMS another address (and another user's subject is not claimed here: the subject is the login) is read for the claim by no one
	claimed := r.mint.Mint(r.mint.Good(loginA).With("email", "forged@example.test", "user_metadata", map[string]any{"email": "forged@example.test"}))
	resp, body = r.do(req{method: http.MethodGet, path: "/v1/me", token: claimed})
	if m := r.wantStatus(resp, body, 200); m["email"] != "ann@example.test" || strings.Contains(string(body), "forged") {
		t.Fatalf("a token that claims an address changed the answer: %s", body)
	}
	// the twin: the same token without the claim gets the same answer, so the claim was in the token and the answer ignored it
	if _, plain := r.get(loginA, "/v1/me"); sameBody(plain) != sameBody(body) {
		t.Errorf("the answer with a claimed address differs from the answer without: %s / %s", body, plain)
	}

	// THE CROSS-USER PAIR: user B's token answers user B's address, and user A's address is in no answer to B
	resp, body = r.get(loginB, "/v1/me")
	if m := r.wantStatus(resp, body, 200); m["email"] != "bob@example.test" || strings.Contains(string(body), "ann@") {
		t.Fatalf("user B read %s", body)
	}
	for _, c := range r.account.seen() {
		if c.User != userA && c.User != userB {
			t.Errorf("the service was given the user %q", c.User)
		}
	}
}

func TestMe_AnUnknownOrMissingLoginIsRefusedBeforeTheServiceIsAsked(t *testing.T) {
	r := newRig(t)
	r.account.seedTwoUsers()
	unknown := r.token("c3c3c3c3-0000-4000-8000-0000000000cc") // a login with no platform user
	for name, q := range map[string]req{
		"no token":              {method: http.MethodGet, path: "/v1/me"},
		"a token of an unknown": {method: http.MethodGet, path: "/v1/me", token: unknown},
	} {
		resp, body := r.do(q)
		r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
		if n := len(r.account.seen()); n != 0 {
			t.Fatalf("%s: the service was asked %d time(s) for a request the login refused", name, n)
		}
	}
	// the control: a legal token of a known user is answered
	if resp, body := r.get(loginA, "/v1/me"); resp.StatusCode != 200 {
		t.Fatalf("control failed: %d %s", resp.StatusCode, body)
	}
}

func TestMe_AUserSwitchedOffBetweenTheDoorAndTheReadIsTheSame401AsAtTheDoor(t *testing.T) {
	r := newRig(t)
	r.account.seedTwoUsers()
	_, door := r.do(req{method: http.MethodGet, path: "/v1/me"}) // the 401 of a request with no token
	for name, err := range map[string]error{
		"inactive":    tenant.ErrInactive,
		"unknown":     tenant.ErrUnknownLogin,
		"the system":  tenant.ErrSystemUser,
		"a bad id":    tenant.ErrBadID,
		"wrapped one": errors.Join(errors.New("while reading"), tenant.ErrInactive),
	} {
		r.account.fail["Email"] = err
		resp, body := r.get(loginA, "/v1/me")
		r.wantError(resp, body, http.StatusUnauthorized, "unauthenticated")
		if sameBody(body) != sameBody(door) || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: the refusal is %s, want the body of every refusal of a login", name, body)
		}
	}
	// a failure of the database is NOT a refusal of the login: it is the 500, and the cause is in the log under the request id and never in the answer
	r.account.fail["Email"] = errors.New("connection refused for ann@example.test")
	resp, body := r.get(loginA, "/v1/me")
	r.wantError(resp, body, http.StatusInternalServerError, "internal_error")
	if strings.Contains(string(body), "connection refused") || strings.Contains(string(body), "ann@") || strings.Contains(r.logs.String(), "ann@example.test") {
		t.Fatalf("the cause or an address reached the answer or the log: %s / %s", body, r.logs.String())
	}
	delete(r.account.fail, "Email")
	if resp, _ := r.get(loginA, "/v1/me"); resp.StatusCode != 200 { // the twin
		t.Fatalf("control failed: %d", resp.StatusCode)
	}
}

func TestBalances_AreThePrincipalsAndEveryNumberIsHisNotTheOthers(t *testing.T) {
	r := newRig(t)
	r.account.seedTwoUsers()

	resp, body := r.get(loginA, "/v1/balances")
	m := r.wantStatus(resp, body, 200)
	if len(m) != 2 {
		t.Fatalf("the answer has members %v, want exactly modules and m3", keysOf(m))
	}
	if got := acctDig(m, "modules", "m1", "available"); got != float64(1000) {
		t.Errorf("m1 available = %v", got)
	}
	for path, want := range map[string]any{"modules.m1.available": float64(1000), "modules.m1.reserved": float64(25), "modules.m2.available": float64(70), "modules.m2.reserved": float64(5),
		"m3.enabled": true, "m3.until": t0.Add(48 * time.Hour).UTC().Format(time.RFC3339Nano), "m3.state": "active"} {
		if got := acctDig(m, strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if mods, _ := m["modules"].(map[string]any); len(mods) != 2 {
		t.Errorf("modules holds %v, want exactly m1 and m2 (module 3 is not metered)", keysOf(mods))
	}
	for _, mod := range []string{"m1", "m2"} {
		if b, _ := acctDig(m, "modules", mod).(map[string]any); len(b) != 2 {
			t.Errorf("%s holds %v, want exactly available and reserved", mod, keysOf(b))
		}
	}
	// the calls are the principal's and are exactly: the two metered modules, the entitlement and its bookkeeping
	want := []accountCall{{"Balance", userA, "m1"}, {"Balance", userA, "m2"}, {"Entitlement", userA, ""}, {"M3Sync", userA, ""}}
	got := r.account.seen()
	if len(got) != len(want) {
		t.Fatalf("the service was called %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// THE CROSS-USER PAIR: B's token answers B's numbers, every one of them, and not one of A's is in the answer
	r.account.reset()
	resp, body = r.get(loginB, "/v1/balances")
	mb := r.wantStatus(resp, body, 200)
	for path, want := range map[string]any{"modules.m1.available": float64(9), "modules.m1.reserved": float64(1), "modules.m2.available": float64(4000), "modules.m2.reserved": float64(300),
		"m3.enabled": false, "m3.until": nil, "m3.state": "suspended"} {
		if got := acctDig(mb, strings.Split(path, ".")...); got != want {
			t.Errorf("user B: %s = %v, want %v", path, got, want)
		}
	}
	for _, c := range r.account.seen() {
		if c.User != userB {
			t.Errorf("a request of user B read the books of %q", c.User)
		}
	}
	for _, a := range []string{"1000", "\"available\":70", "active"} {
		if strings.Contains(string(body), a) {
			t.Errorf("user B's answer holds %s, which is user A's: %s", a, body)
		}
	}
}

func TestBalances_ModuleThreeIsOnlyEverOneOfThreeWordsAndEnabledIsTheDatabasesJudgement(t *testing.T) {
	until := t0.Add(-time.Hour)
	later := t0.Add(time.Hour)
	for _, c := range []struct {
		name        string
		ent         entitlements.Entitlement
		sync        entitlements.SyncState
		wantEnabled bool
		wantUntil   any
		wantState   string
	}{
		{"a user with nothing", entitlements.Entitlement{}, entitlements.SyncState{}, false, nil, "unknown"},
		{"enabled, never observed", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: true, Observed: "unknown"}, true, nil, "unknown"},
		{"enabled and seen active", entitlements.Entitlement{Exists: true, Enabled: true, Current: true, Until: &later}, entitlements.SyncState{Exists: true, Observed: "active"}, true, later.Format(time.RFC3339Nano), "active"},
		{"enabled and seen suspended", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: true, Observed: "suspended"}, true, nil, "suspended"},
		// the entitlement's date has passed by the DATABASE's clock but the expiry loop has not written the end yet: the row says Enabled, the database's judgement (Current) says no, and the answer follows Current
		{"past its end, not yet expired by the loop", entitlements.Entitlement{Exists: true, Enabled: true, Current: false, Until: &until}, entitlements.SyncState{Exists: true, Observed: "active"}, false, until.Format(time.RFC3339Nano), "active"},
		{"disabled", entitlements.Entitlement{Exists: true, Enabled: false}, entitlements.SyncState{Exists: true, Observed: "suspended"}, false, nil, "suspended"},
		{"a bookkeeping row that says something else", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: true, Observed: "paused-by-the-moon"}, true, nil, "unknown"},
		{"no bookkeeping row but an observation text", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: false, Observed: "suspended"}, true, nil, "unknown"},
		{"an empty observation", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: true, Observed: ""}, true, nil, "unknown"},
		{"upper case is not a word the platform writes", entitlements.Entitlement{Exists: true, Enabled: true, Current: true}, entitlements.SyncState{Exists: true, Observed: "SUSPENDED"}, true, nil, "unknown"},
		// the row says "enabled" but nobody asked the database whether it is current: Current is false, and the answer is "not enabled" (the row is not the judgement)
		{"enabled by the row, not current", entitlements.Entitlement{Exists: true, Enabled: true, Current: false}, entitlements.SyncState{Exists: true, Observed: "active"}, false, nil, "active"},
		{"current without a row", entitlements.Entitlement{Exists: false, Enabled: true, Current: true}, entitlements.SyncState{}, false, nil, "unknown"},
	} {
		r := newRig(t)
		r.account.ents[userA], r.account.syncs[userA] = c.ent, c.sync
		resp, body := r.get(loginA, "/v1/balances")
		m := r.wantStatus(resp, body, 200)
		if got := acctDig(m, "m3", "enabled"); got != c.wantEnabled {
			t.Errorf("%s: m3.enabled = %v, want %v", c.name, got, c.wantEnabled)
		}
		if got := acctDig(m, "m3", "until"); got != c.wantUntil {
			t.Errorf("%s: m3.until = %v, want %v", c.name, got, c.wantUntil)
		}
		if got := acctDig(m, "m3", "state"); got != c.wantState {
			t.Errorf("%s: m3.state = %v, want %v", c.name, got, c.wantState)
		}
		if st, _ := acctDig(m, "m3", "state").(string); st != "active" && st != "suspended" && st != "unknown" {
			t.Errorf("%s: m3.state is %q, which is none of the three words", c.name, st)
		}
	}
	// a user who has no balance row has zeros, not an error and not a missing member
	r := newRig(t)
	m := func() map[string]any {
		resp, body := r.get(loginA, "/v1/balances")
		return r.wantStatus(resp, body, 200)
	}()
	if acctDig(m, "modules", "m1", "available") != float64(0) || acctDig(m, "modules", "m2", "reserved") != float64(0) {
		t.Errorf("a user with no balance reads %v, want zeros", m)
	}
}

func TestBalances_AFailureOfAnyReadIsA500WithNothingPartialAndNoCause(t *testing.T) {
	for _, method := range []string{"Balance", "Entitlement", "M3Sync"} {
		r := newRig(t)
		r.account.seedTwoUsers()
		r.account.fail[method] = errors.New("deadlock detected while reading ann@example.test")
		resp, body := r.get(loginA, "/v1/balances")
		r.wantError(resp, body, http.StatusInternalServerError, "internal_error")
		if strings.Contains(string(body), "deadlock") || strings.Contains(string(body), "1000") || strings.Contains(r.logs.String(), "ann@example.test") {
			t.Errorf("%s failed: the cause, a number or an address reached the answer or the log: %s / %s", method, body, r.logs.String())
		}
		// the twin: with the failure gone the same request is answered in full
		delete(r.account.fail, method)
		if resp, body := r.get(loginA, "/v1/balances"); resp.StatusCode != 200 || !strings.Contains(string(body), `"available":1000`) {
			t.Errorf("%s: control failed: %d %s", method, resp.StatusCode, body)
		}
	}
}

func TestAccountRoutes_TakeNoQueryNoBodyOfAnyUserAndOnlyGET(t *testing.T) {
	r := newRig(t)
	r.account.seedTwoUsers()
	for _, path := range []string{"/v1/me", "/v1/balances"} {
		r.account.reset()
		for _, q := range []string{"?user_id=" + userB, "?user=" + userB, "?owner=x", "?email=a@example.test", "?limit=5"} {
			resp, body := r.get(loginA, path+q)
			r.wantError(resp, body, http.StatusBadRequest, "bad_request")
		}
		if n := len(r.account.seen()); n != 0 {
			t.Fatalf("%s: the service was asked %d time(s) for requests the query refused", path, n)
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			resp, body := r.do(req{method: m, path: path, token: r.token(loginA)})
			r.wantError(resp, body, http.StatusMethodNotAllowed, "method_not_allowed")
			if !strings.Contains(resp.Header.Get("Allow"), "GET") {
				t.Errorf("%s %s: Allow = %q", m, path, resp.Header.Get("Allow"))
			}
		}
		if resp, _ := r.get(loginA, path); resp.StatusCode != 200 { // the twin
			t.Fatalf("control failed: GET %s = %d", path, resp.StatusCode)
		}
	}
}

func TestAccountRoutes_AreNotMountedWithoutAnAccountServiceAndTheRestOfTheAPIIsUnaffected(t *testing.T) {
	r := newRig(t, withoutAccount())
	for _, path := range []string{"/v1/me", "/v1/balances"} {
		resp, body := r.get(loginA, path)
		r.wantError(resp, body, http.StatusNotFound, "not_found")
	}
	// the twin, and the neighbours: with the service they are served; without it the job routes, the settings and the template check still are
	if resp, _ := newRig(t).get(loginA, "/v1/me"); resp.StatusCode != 200 {
		t.Fatalf("control failed: %d", resp.StatusCode)
	}
	if resp, _ := r.get(loginA, "/v1/jobs"); resp.StatusCode != 200 {
		t.Errorf("GET /v1/jobs without an account service = %d", resp.StatusCode)
	}
	if resp, _ := r.get(loginA, "/v1/settings"); resp.StatusCode != 200 {
		t.Errorf("GET /v1/settings without an account service = %d", resp.StatusCode)
	}
	if resp, body := r.post(loginA, "/v1/templates/check", `{"subject":"Hi","html":"<p>x</p>"}`); resp.StatusCode != 200 {
		t.Errorf("POST /v1/templates/check without an account service = %d %s", resp.StatusCode, body)
	}
}

func TestM3State_MapsTheObservationToOneOfThreeWords(t *testing.T) {
	for _, c := range []struct {
		in   entitlements.SyncState
		want string
	}{
		{entitlements.SyncState{Exists: true, Observed: "active"}, "active"},
		{entitlements.SyncState{Exists: true, Observed: "suspended"}, "suspended"},
		{entitlements.SyncState{Exists: true, Observed: "unknown"}, "unknown"},
		{entitlements.SyncState{Exists: true}, "unknown"},
		{entitlements.SyncState{Exists: true, Observed: "Active"}, "unknown"},
		{entitlements.SyncState{Exists: true, Observed: " active"}, "unknown"},
		{entitlements.SyncState{Exists: false, Observed: "active"}, "unknown"},
		{entitlements.SyncState{}, "unknown"},
	} {
		if got := m3State(c.in); got != c.want {
			t.Errorf("m3State(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// acctDig walks a decoded JSON object by member names; nil when a step is not an object or is not there.
func acctDig(m map[string]any, path ...string) any {
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
