package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The module 3 account routes (m3accounts.go) over a fake of module 3's client. The fake keeps an owner for every account it knows, as the module scopes by user_id, so another user's account is a 404 for
// the same reason it is one at the module; it records the user every client was built for and every call, and the SMTP secret it was handed (so that "the secret never comes back" is a test that can fail:
// the secret DID get to the adapter). The adapter's own behaviour is in internal/adapters/m3; the routes against the real module 3 are in tests/harness/m3_accounts_api_test.go.

const (
	acctA1 = "0a000000-0000-4000-8000-0000000000a1" // an account of userA
	acctB1 = "0b000000-0000-4000-8000-0000000000b1" // an account of userB
	acctNo = "0c000000-0000-4000-8000-0000000000ff" // an account nobody has
	canary = "pw-canary-4f9d8e7a"                   // an SMTP password the tests send and look for everywhere
)

type fakeM3 struct {
	mu      sync.Mutex
	calls   []call
	fors    []string
	fail    map[string]error
	forErr  error
	owners  map[string]string // account id -> user; the module answers any other id with its 404
	page    m3.AccountPage
	created m3.Account
	test    m3.TestResult
	bulk    m3.BulkResult
	secrets []string
	news    []m3.NewAccount
}

func newFakeM3() *fakeM3 {
	f := &fakeM3{fail: map[string]error{}, owners: map[string]string{acctA1: userA, acctB1: userB, jobIDA: userA}} // jobIDA: the id the every-route test puts in a path
	f.created = fixtureAccount(acctA1)
	f.test = m3.TestResult{Verification: m3.TestVerification{State: m3.VerificationVerified}, Alignment: m3.TestAlignment{State: m3.AlignmentAligned}}
	return f
}

func fixtureAccount(id string) m3.Account {
	name := "Sender"
	rawProvider, rawGate := "535 5.7.8 provider-text-canary", "module-sentence-canary"
	return m3.Account{
		ID: id, UserID: userA, SMTPHost: "smtp.example.test", SMTPPort: 587, SMTPUsername: "u@example.test", FromAddress: "u@example.test", FromName: &name, HasCredential: true,
		DailyLimit: 500, CreatedAt: t0,
		Verification:  m3.AccountVerification{State: m3.VerificationVerified, SMTPResponse: &rawProvider},
		FromAlignment: m3.AccountAlignment{State: m3.AlignmentWarned, DMARCPolicy: "none", Message: &rawGate},
	}
}

func (f *fakeM3) For(user string) (M3Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fors = append(f.fors, user)
	if f.forErr != nil {
		return nil, f.forErr
	}
	return &fakeM3Client{f: f, user: user}, nil
}

type fakeM3Client struct {
	f    *fakeM3
	user string
}

func (c *fakeM3Client) rec(method, id string, arg any) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.calls = append(c.f.calls, call{Method: method, User: c.user, Job: id, Arg: arg})
	return c.f.fail[method]
}

func (c *fakeM3Client) notMine(id string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if owner, ok := c.f.owners[id]; ok && owner == c.user {
		return nil
	}
	return &modclient.Error{Kind: modclient.NotFound, Module: modclient.M3, Route: "m3.accounts.test", Status: 404, Code: "not_found", Message: "the module has no such resource", Sent: true}
}

func (c *fakeM3Client) ListAccounts(_ context.Context, cursor string) (m3.AccountPage, error) {
	if err := c.rec("ListAccounts", "", cursor); err != nil {
		return m3.AccountPage{}, err
	}
	return c.f.page, nil
}

func (c *fakeM3Client) CreateAccount(_ context.Context, n m3.NewAccount) (m3.Account, error) {
	c.f.mu.Lock()
	c.f.secrets = append(c.f.secrets, n.SMTPSecret.Reveal())
	c.f.news = append(c.f.news, n)
	c.f.mu.Unlock()
	if err := c.rec("CreateAccount", "", n.SMTPHost); err != nil {
		return m3.Account{}, err
	}
	return c.f.created, nil
}

func (c *fakeM3Client) TestAccount(_ context.Context, id string, ack bool) (m3.TestResult, error) {
	if err := c.rec("TestAccount", id, ack); err != nil {
		return m3.TestResult{}, err
	}
	if err := c.notMine(id); err != nil {
		return m3.TestResult{}, err
	}
	return c.f.test, nil
}

func (c *fakeM3Client) BulkAddAccounts(_ context.Context, accounts []m3.NewAccount) (m3.BulkResult, error) {
	c.f.mu.Lock()
	for _, n := range accounts {
		c.f.secrets = append(c.f.secrets, n.SMTPSecret.Reveal())
	}
	c.f.news = append(c.f.news, accounts...)
	c.f.mu.Unlock()
	if err := c.rec("BulkAddAccounts", "", len(accounts)); err != nil {
		return m3.BulkResult{}, err
	}
	if c.f.bulk.Rows != nil {
		return c.f.bulk, nil
	}
	res := m3.BulkResult{}
	for i := range accounts {
		res.Rows = append(res.Rows, m3.BulkRow{Row: i + 1, Created: true, AccountID: acctA1, Verification: m3.VerificationVerified})
	}
	return res, nil
}

func (c *fakeM3Client) DeleteAccount(_ context.Context, id string) error {
	if err := c.rec("DeleteAccount", id, nil); err != nil {
		return err
	}
	if err := c.notMine(id); err != nil {
		return err
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	delete(c.f.owners, id)
	return nil
}

func (f *fakeM3) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeM3) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.fors, f.secrets, f.news = nil, nil, nil, nil
}

// fakeM3Gate is the entitlement gate: it answers err (nil lets the user through) and records who was asked.
type fakeM3Gate struct {
	mu   sync.Mutex
	err  error
	asks []string
}

func (g *fakeM3Gate) Check(_ context.Context, user string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asks = append(g.asks, user)
	return g.err
}

// ---- the mapping of module 3's refusals -----------------------------------------------------------------------------------------

func m3Refused(status int, code string, d ...[2]string) *modclient.Error {
	e := &modclient.Error{Kind: modclient.Refused, Module: modclient.M3, Route: "m3.accounts.create", Status: status, Code: code, Message: "the module refused the request", Sent: true, Enveloped: true}
	for _, x := range d {
		e.Details = append(e.Details, modclient.Detail{Field: x[0], Issue: x[1]})
	}
	return e
}

func m3Kind(k modclient.Kind, status int, sent bool) *modclient.Error {
	return &modclient.Error{Kind: k, Module: modclient.M3, Route: "m3.accounts.create", Status: status, Message: "a sentence of the adapter's", Sent: sent}
}

type m3Row struct {
	name       string
	err        error
	write      bool
	status     int
	code       string
	details    map[string]any
	retryAfter string // the Retry-After header, "" for none
}

func m3Rows() []m3Row {
	rl := m3Kind(modclient.RateLimited, 429, true)
	rlAfter := m3Kind(modclient.RateLimited, 429, true)
	rlAfter.RetryAfter = 90 * time.Second
	return []m3Row{
		{"a duplicate account", fmt.Errorf("%w: %w", m3.ErrAccountDuplicate, m3Refused(409, "conflict", [2]string{"smtp_username", "conflict"})), true, 409, "account_duplicate", nil, ""},
		{"a host the module will not connect to", fmt.Errorf("%w: %w", m3.ErrHostForbidden, m3Refused(400, "validation_failed", [2]string{"smtp_host", "forbidden_value"})), true, 422, "host_forbidden", nil, ""},
		{"the action the module refuses for a suspended user", fmt.Errorf("%w: %w", m3.ErrActionUnavailable, m3Refused(409, "conflict")), true, 409, "account_action_unavailable", nil, ""},
		{"a create conflict whose cause the module does not say", fmt.Errorf("%w: %w", sendapi.ErrConflictUnexplained, m3Refused(409, "conflict")), true, 409, "account_action_unavailable", nil, ""},
		{"a From-gate warning that was not acknowledged", m3Refused(422, "unprocessable", [2]string{"acknowledge_alignment_warning", "required"}), true, 422, "from_gate_ack_required", map[string]any{"from_gate": "unaligned_warned"}, ""},
		{"fields the module rejected (only words of the module's own sets are carried)", m3Refused(400, "validation_failed", [2]string{"smtp_port", "out_of_range"}, [2]string{"user_id", "required"}, [2]string{"smtp_host", "a free sentence"}, [2]string{"daily_limit", "malformed"}), true, 400, "validation_failed",
			map[string]any{"reason": "the account is not valid", "fields": []any{"smtp_port:out_of_range", "daily_limit:malformed"}}, ""},
		{"a validation refusal that names no field", m3Refused(400, "validation_failed"), true, 400, "validation_failed", map[string]any{"reason": "the account is not valid", "fields": []any{}}, ""},
		{"an account that is not the user's, is gone or never was", m3Kind(modclient.NotFound, 404, true), true, 404, "not_found", nil, ""},
		{"a rate limit with no Retry-After of the module's", rl, true, 429, "rate_limited", nil, "3600"},
		{"a rate limit with the module's Retry-After", rlAfter, true, 429, "rate_limited", nil, "90"},
		{"a write that timed out after it was sent", m3Kind(modclient.Timeout, 0, true), true, 503, "account_outcome_unknown", nil, ""},
		{"a write the module failed on", m3Kind(modclient.Internal, 500, true), true, 503, "account_outcome_unknown", nil, ""},
		{"a write whose answer was not understood", m3Kind(modclient.Contract, 201, true), true, 503, "account_outcome_unknown", nil, ""},
		{"a write the module could not be reached for", m3Kind(modclient.Unavailable, 0, false), true, 503, "module_unavailable", nil, ""},
		{"a write the module's operator paused (the module refused it, nothing was done)", m3Kind(modclient.Paused, 503, true), true, 503, "module_unavailable", nil, ""},
		{"a READ that timed out (nothing changed)", m3Kind(modclient.Timeout, 0, true), false, 503, "module_unavailable", nil, ""},
		{"a read the module failed on", m3Kind(modclient.Internal, 500, true), false, 503, "module_unavailable", nil, ""},
		{"a read the module is unavailable for", m3Kind(modclient.Unavailable, 503, true), false, 503, "module_unavailable", nil, ""},
		{"a refusal the API does not know (a revoked service token)", m3Refused(401, "unauthenticated"), true, 500, "internal_error", nil, ""},
		{"a request the adapter refused before it left (a bug of ours)", m3Kind(modclient.Invalid, 0, false), true, 500, "internal_error", nil, ""},
	}
}

// extraMappingTable is the rows of m3Rows in the shape of errors_test.go's table, for the document test: every code this file's routes can answer, with its status.
func extraMappingTable() []mapping {
	var out []mapping
	for _, r := range m3Rows() {
		out = append(out, mapping{name: r.name, err: r.err, status: r.status, code: r.code, details: r.details})
	}
	return out
}

func okAccountBody() map[string]any {
	return map[string]any{"smtp_host": "smtp.example.test", "smtp_username": "u@example.test", "smtp_secret": canary, "from_address": "u@example.test"}
}

func TestM3Accounts_EveryRefusalOfTheModuleIsOneOfTheClosedCodesWithNoModuleText(t *testing.T) {
	for _, row := range m3Rows() {
		r := newRig(t)
		method, path := "ListAccounts", "/v1/m3/accounts"
		if row.write {
			method = "CreateAccount"
		}
		r.m3.fail[method] = row.err
		var resp *http.Response
		var body []byte
		if row.write {
			resp, body = r.post(loginA, "/v1/m3/accounts", okAccountBody())
		} else {
			resp, body = r.get(loginA, path)
		}
		e := r.wantError(resp, body, row.status, row.code)
		if e.Message != messages[row.code] {
			t.Errorf("%s: message %q, want the fixed %q", row.name, e.Message, messages[row.code])
		}
		if fmt.Sprint(e.Details) != fmt.Sprint(row.details) && !(len(e.Details) == 0 && len(row.details) == 0) {
			t.Errorf("%s: details %v, want %v", row.name, e.Details, row.details)
		}
		if got := resp.Header.Get("Retry-After"); got != row.retryAfter {
			t.Errorf("%s: Retry-After %q, want %q", row.name, got, row.retryAfter)
		}
		// nothing of the module's text, of the adapter's sentences, of the user's secret or of a user id reaches the client or the log
		for _, leak := range []string{"a sentence of the adapter", "the module refused", "modclient", "a free sentence", "the module has no such resource", canary, "user_id", "m3.accounts"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s: the answer carries %q: %s", row.name, leak, body)
			}
		}
		if strings.Contains(r.logs.String(), canary) {
			t.Errorf("%s: the password is in the log", row.name)
		}
	}
	// the control: the fake that answers properly is a 201
	r := newRig(t)
	if resp, body := r.post(loginA, "/v1/m3/accounts", okAccountBody()); resp.StatusCode != 201 {
		t.Fatalf("control: %d %s", resp.StatusCode, body)
	}
}

func TestM3Accounts_AnAdapterErrorThatDidNotComeFromAnAccountRouteKeepsItsOldMapping(t *testing.T) {
	// the job store wraps what module 3's pre-check says; a timeout of that is NOT an account route's open outcome, and classify leaves it as it was (a 500, the cause in the log)
	r := newRig(t)
	r.store.fail["Get"] = fmt.Errorf("jobs.confirm: %w", m3Kind(modclient.Timeout, 0, true))
	resp, body := r.get(loginA, "/v1/jobs/"+jobIDA)
	r.wantError(resp, body, 500, "internal_error")
	// the control: the same error marked as the error of an account route is the 503 of its own
	if p, ok := classifyM3(m3Err(m3Kind(modclient.Timeout, 0, true), true)); !ok || p.code != codeOutcomeUnknown {
		t.Fatalf("the marked error: %+v %v", p, ok)
	}
}

func TestM3Accounts_ListShowsWordsAndHasNoSecretAndNoModuleText(t *testing.T) {
	r := newRig(t)
	next := "AAAA-cursor_1="
	r.m3.page = m3.AccountPage{Items: []m3.Account{fixtureAccount(acctA1)}, NextCursor: next}
	resp, body := r.get(loginA, "/v1/m3/accounts?cursor=abc_DEF-1")
	m := r.wantStatus(resp, body, 200)
	accounts, _ := m["accounts"].([]any)
	if len(accounts) != 1 || m["next_cursor"] != next {
		t.Fatalf("the list: %s", body)
	}
	a := accounts[0].(map[string]any)
	want := map[string]any{"id": acctA1, "smtp_host": "smtp.example.test", "smtp_port": float64(587), "smtp_username": "u@example.test", "from_address": "u@example.test", "from_name": "Sender",
		"daily_limit": float64(500), "has_credential": true, "status": "active", "pause_reason": nil, "quarantine_reason": nil, "verification": "verified", "verify_code": nil, "from_gate": "unaligned_warned",
		"created_at": stamp(t0)}
	if len(a) != len(want) {
		t.Fatalf("the account has %d members, want exactly %d: %v", len(a), len(want), a)
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("member %s = %v, want %v", k, a[k], v)
		}
	}
	for _, leak := range []string{"provider-text-canary", "module-sentence-canary", "dmarc", "535", "smtp_secret", "password"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Errorf("the list carries %q: %s", leak, body)
		}
	}
	if c := r.m3.seen(); len(c) != 1 || c[0].User != userA || c[0].Arg != "abc_DEF-1" {
		t.Fatalf("the module was asked %+v", c)
	}
	// the cursor is data of a closed alphabet: anything else, and a parameter that names a user, never reach the module
	r.m3.reset()
	for _, bad := range []string{"/v1/m3/accounts?cursor=" + urlEscape("a b"), "/v1/m3/accounts?cursor=" + urlEscape("x'; --"), "/v1/m3/accounts?cursor=" + strings.Repeat("a", 1025)} {
		resp, body := r.get(loginA, bad)
		r.wantError(resp, body, 400, "validation_failed")
	}
	resp, body = r.get(loginA, "/v1/m3/accounts?user_id="+userB)
	r.wantError(resp, body, 400, "bad_request")
	if len(r.m3.seen()) != 0 {
		t.Fatalf("a refused request reached the module: %+v", r.m3.seen())
	}
	// an empty list is an empty array and no cursor
	r2 := newRig(t)
	resp, body = r2.get(loginA, "/v1/m3/accounts")
	if m := r2.wantStatus(resp, body, 200); string(mustJSON(t, m["accounts"])) != "[]" || m["next_cursor"] != nil {
		t.Fatalf("an empty list: %s", body)
	}
}

func TestM3Accounts_TheStatusWordsAndTheClosedSetsOfTheViews(t *testing.T) {
	base := fixtureAccount(acctA1)
	for name, c := range map[string]struct {
		mutate func(*m3.Account)
		want   string
	}{
		"active": {func(a *m3.Account) {}, "active"}, "paused": {func(a *m3.Account) { a.Paused, a.PauseReason = true, "daily_limit" }, "paused"},
		"quarantined": {func(a *m3.Account) { a.Quarantined, a.QuarantineReason = true, "auth_failure" }, "quarantined"},
		"disabled":    {func(a *m3.Account) { a.Disabled, a.Paused, a.Quarantined = true, true, true }, "disabled"},
		"quarantined and paused: quarantine wins": {func(a *m3.Account) { a.Quarantined, a.Paused = true, true }, "quarantined"},
	} {
		a := base
		c.mutate(&a)
		if got := viewM3Account(a).Status; got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	for in, want := range map[m3.AlignmentState]string{"unverified": "unverified", "aligned": "aligned", "unaligned_warned": "unaligned_warned", "blocked": "blocked", "indeterminate": "indeterminate", "": "indeterminate", "teleported": "indeterminate"} {
		if got := fromGateWord(in); got != want {
			t.Errorf("from gate %q is told as %q, want %q", in, got, want)
		}
	}
	for in, want := range map[m3.VerificationState]string{"verified": "verified", "failed": "failed", "unverified": "unverified", "unavailable": "unverified", "": "unverified"} {
		if got := verificationWord(in); got != want {
			t.Errorf("verification %q is told as %q, want %q", in, got, want)
		}
	}
	for code, want := range map[m3.VerifyCode]string{
		m3.VerifyConnectFailed: "connect", m3.VerifyTimeout: "connect", m3.VerifyConnectionLost: "connect", m3.VerifyHostNotAllowed: "host", m3.VerifyTLSUnavailable: "tls", m3.VerifyTLSFailed: "tls",
		m3.VerifyAuthFailed: "login", m3.VerifyProtocolError: "protocol", m3.VerifyError: "unavailable", m3.VerifyServiceUnavailable: "unavailable", m3.VerifyNone: "unavailable",
	} {
		if got := testStage(m3.TestVerification{State: m3.VerificationFailed, Code: code}); got != want {
			t.Errorf("stage of %q is %q, want %q", code, got, want)
		}
	}
	if got := testStage(m3.TestVerification{State: m3.VerificationVerified}); got != "done" {
		t.Errorf("a verified test is at stage %q", got)
	}
}

func TestM3Accounts_AddSendsThePasswordToTheModuleAndItIsInNothingTheClientOrTheLogSees(t *testing.T) {
	r := newRig(t)
	body := okAccountBody()
	body["smtp_port"], body["from_name"], body["daily_limit"], body["acknowledge_from_gate"] = 465, "Sender", 100, true
	resp, raw := r.post(loginA, "/v1/m3/accounts", body)
	m := r.wantStatus(resp, raw, 201)
	if a, _ := m["account"].(map[string]any); a["id"] != acctA1 || a["has_credential"] != true || len(m) != 1 {
		t.Fatalf("the answer: %s", raw)
	}
	// the control of the whole test: the password DID reach the adapter, as the secret type, with the rest of what was sent
	r.m3.mu.Lock()
	secrets, news := append([]string(nil), r.m3.secrets...), append([]m3.NewAccount(nil), r.m3.news...)
	r.m3.mu.Unlock()
	if len(secrets) != 1 || secrets[0] != canary || len(news) != 1 || news[0].SMTPPort != 465 || news[0].DailyLimit != 100 || !news[0].AcknowledgeAlignmentWarning || news[0].FromName == nil || *news[0].FromName != "Sender" {
		t.Fatalf("what the adapter received: %v %+v", secrets, news)
	}
	if fmt.Sprintf("%v %+v %s", news[0].SMTPSecret, news[0], news[0].SMTPSecret) == "" || strings.Contains(fmt.Sprintf("%v %+v", news[0].SMTPSecret, news[0]), canary) {
		t.Fatal("the secret prints as itself")
	}
	if strings.Contains(string(raw), canary) || strings.Contains(r.logs.String(), canary) {
		t.Fatalf("the password is in the answer or the log: %s", raw)
	}
	// for a request that fails in every way a body can: the password is in none of the answers, none of the log lines
	for name, b := range map[string]any{
		"an unknown member":          map[string]any{"smtp_host": "h", "smtp_username": "u", "smtp_secret": canary, "from_address": "f", "user_id": userB},
		"a member of the wrong type": `{"smtp_host":"h","smtp_username":"u","smtp_secret":"` + canary + `","from_address":"f","smtp_port":"465"}`,
		"a missing host":             map[string]any{"smtp_username": "u", "smtp_secret": canary, "from_address": "f"},
		"truncated JSON":             `{"smtp_secret":"` + canary + `"`,
	} {
		resp, raw := r.post(loginA, "/v1/m3/accounts", b)
		if resp.StatusCode != 400 || strings.Contains(string(raw), canary) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, raw)
		}
	}
	if strings.Contains(r.logs.String(), canary) {
		t.Fatalf("the password is in the log: %s", r.logs.String())
	}
}

func TestM3Accounts_AddDecodesStrictlyAndNeverReachesTheModuleWithAnIncompleteAccount(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		body   any
		status int
		code   string
		fields []any
	}{
		"a member naming a user":     {map[string]any{"smtp_host": "h", "smtp_username": "u", "smtp_secret": "s", "from_address": "f", "user_id": userB}, 400, "bad_request", nil},
		"a member the route lacks":   {map[string]any{"smtp_host": "h", "smtp_username": "u", "smtp_secret": "s", "from_address": "f", "smtp_password": "s"}, 400, "bad_request", nil},
		"no secret":                  {map[string]any{"smtp_host": "h", "smtp_username": "u", "from_address": "f"}, 400, "validation_failed", []any{"smtp_secret:required"}},
		"an empty secret":            {map[string]any{"smtp_host": "h", "smtp_username": "u", "smtp_secret": "", "from_address": "f"}, 400, "validation_failed", []any{"smtp_secret:required"}},
		"every required member gone": {map[string]any{}, 400, "validation_failed", []any{"smtp_host:required", "smtp_username:required", "from_address:required", "smtp_secret:required"}},
		"a host of white space":      {map[string]any{"smtp_host": "   ", "smtp_username": "u", "smtp_secret": "s", "from_address": "f"}, 400, "validation_failed", []any{"smtp_host:required"}},
		"a username over the bound":  {map[string]any{"smtp_host": "h", "smtp_username": strings.Repeat("u", 1025), "smtp_secret": "s", "from_address": "f"}, 400, "validation_failed", []any{"smtp_username:malformed"}},
		"a NUL in the host":          {`{"smtp_host":"h\u0000","smtp_username":"u","smtp_secret":"s","from_address":"f"}`, 400, "validation_failed", []any{"smtp_host:malformed"}},
		"a port of the wrong type":   {`{"smtp_host":"h","smtp_username":"u","smtp_secret":"s","from_address":"f","smtp_port":"587"}`, 400, "bad_request", nil},
		"an array":                   {`[]`, 400, "bad_request", nil},
	} {
		r.m3.reset()
		resp, raw := r.post(loginA, "/v1/m3/accounts", c.body)
		e := r.wantError(resp, raw, c.status, c.code)
		if c.fields != nil {
			if got, _ := e.Details["fields"].([]any); fmt.Sprint(got) != fmt.Sprint(c.fields) {
				t.Errorf("%s: fields %v, want %v", name, got, c.fields)
			}
		}
		if len(r.m3.seen()) != 0 {
			t.Errorf("%s: the module was reached", name)
		}
	}
	// the media type, and a query parameter
	resp, raw := r.do(req{method: http.MethodPost, path: "/v1/m3/accounts", token: r.token(loginA), ctype: "text/csv", body: []byte("a,b\n")})
	r.wantError(resp, raw, 415, "unsupported_media_type")
	resp, raw = r.post(loginA, "/v1/m3/accounts?user_id="+userB, okAccountBody())
	r.wantError(resp, raw, 400, "bad_request")
	if len(r.m3.seen()) != 0 {
		t.Fatal("a refused request reached the module")
	}
	// the control: a complete account is added
	if resp, raw := r.post(loginA, "/v1/m3/accounts", okAccountBody()); resp.StatusCode != 201 || len(r.m3.seen()) != 1 {
		t.Fatalf("control: %d %s", resp.StatusCode, raw)
	}
}

func TestM3Accounts_TheClientIsBuiltForThePrincipalOnlyAndAnotherUsersAccountIsA404ThatTellsNothing(t *testing.T) {
	r := newRig(t)
	// every client is built for the user of the token (never the login id)
	r.get(loginA, "/v1/m3/accounts")
	r.post(loginB, "/v1/m3/accounts", okAccountBody())
	r.post(loginA, "/v1/m3/accounts/"+acctA1+"/test", nil)
	r.post(loginB, "/v1/m3/accounts/"+acctB1+"/test", nil)
	r.m3.mu.Lock()
	fors := append([]string(nil), r.m3.fors...)
	r.m3.mu.Unlock()
	if strings.Join(fors, ",") != userA+","+userB+","+userA+","+userB {
		t.Fatalf("the clients were built for %v", fors)
	}
	for _, c := range r.m3.seen() {
		if c.User != userA && c.User != userB {
			t.Fatalf("a call was made as %q", c.User)
		}
	}
	// B's account through A's token is the 404 of an account nobody has, byte for byte but for the request id; so is an id that cannot be an account (and the module is not asked about it)
	for _, route := range []string{"/test", "/remove"} {
		r.m3.reset()
		resp1, body1 := r.post(loginA, "/v1/m3/accounts/"+acctB1+route, nil)
		resp2, body2 := r.post(loginA, "/v1/m3/accounts/"+acctNo+route, nil)
		r.wantError(resp1, body1, 404, "not_found")
		r.wantError(resp2, body2, 404, "not_found")
		if sameBody(body1) != sameBody(body2) {
			t.Fatalf("%s: another user's account and a missing one are answered differently:\n%s\n%s", route, body1, body2)
		}
		before := len(r.m3.seen())
		for _, id := range []string{"not-a-uuid", strings.ToUpper(acctA1), acctA1 + "0", "..%2f..%2fx"} {
			resp, body := r.post(loginA, "/v1/m3/accounts/"+id+route, nil)
			r.wantError(resp, body, 404, "not_found")
		}
		if len(r.m3.seen()) != before {
			t.Fatalf("%s: the module was asked about ids that cannot be accounts", route)
		}
	}
	// B's account is still B's, and its owner can still remove it (the control)
	if owner := r.m3.owners[acctB1]; owner != userB {
		t.Fatalf("A's request changed B's account: owner %q", owner)
	}
	resp, body := r.post(loginB, "/v1/m3/accounts/"+acctB1+"/remove", nil)
	if m := r.wantStatus(resp, body, 200); m["removed"] != true {
		t.Fatalf("the owner's removal: %s", body)
	}
}

func TestM3Accounts_TheTestButtonSaysWordsAndTakesAnOptionalAcknowledgement(t *testing.T) {
	r := newRig(t)
	r.m3.test = m3.TestResult{
		Verification: m3.TestVerification{State: m3.VerificationFailed, Code: m3.VerifyAuthFailed, SMTPResponse: strPtr("535 provider-canary")},
		Alignment:    m3.TestAlignment{State: m3.AlignmentIndeterminate, Code: m3.AlignmentDNSUnavailable, Message: strPtr("module-sentence-canary")},
	}
	resp, body := r.post(loginA, "/v1/m3/accounts/"+acctA1+"/test", nil) // no body at all is the plain test
	m := r.wantStatus(resp, body, 200)
	res, _ := m["result"].(map[string]any)
	if res["ok"] != false || res["stage"] != "login" || res["from_gate"] != "indeterminate" || len(res) != 3 || len(m) != 1 {
		t.Fatalf("the result: %s", body)
	}
	for _, leak := range []string{"canary", "535", "dns"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Fatalf("the result carries %q: %s", leak, body)
		}
	}
	if c := r.m3.seen(); len(c) != 1 || c[0].Method != "TestAccount" || c[0].Job != acctA1 || c[0].Arg != false {
		t.Fatalf("the module was asked %+v", c)
	}
	// a body with the acknowledgement; one with an unknown member or the wrong type is refused and the module not asked
	r.m3.reset()
	resp, body = r.post(loginA, "/v1/m3/accounts/"+acctA1+"/test", map[string]any{"acknowledge": true})
	r.wantStatus(resp, body, 200)
	if c := r.m3.seen(); len(c) != 1 || c[0].Arg != true {
		t.Fatalf("the acknowledgement was not passed on: %+v", c)
	}
	r.m3.reset()
	for _, bad := range []any{map[string]any{"acknowledge": true, "user_id": userB}, `{"acknowledge":"yes"}`, `{"acknowledge":true} x`} {
		resp, body := r.post(loginA, "/v1/m3/accounts/"+acctA1+"/test", bad)
		r.wantError(resp, body, 400, "bad_request")
	}
	if len(r.m3.seen()) != 0 {
		t.Fatalf("a refused test reached the module: %+v", r.m3.seen())
	}
	// a click that verified is done, and its words are the module's closed set
	r.m3.test = m3.TestResult{Verification: m3.TestVerification{State: m3.VerificationVerified}, Alignment: m3.TestAlignment{State: m3.AlignmentBlocked}}
	resp, body = r.post(loginA, "/v1/m3/accounts/"+acctA1+"/test", nil)
	if res := r.wantStatus(resp, body, 200)["result"].(map[string]any); res["ok"] != true || res["stage"] != "done" || res["from_gate"] != "blocked" {
		t.Fatalf("a verified test: %s", body)
	}
}

func TestM3Accounts_TheGateStopsTheThreeRealAttemptsAndLetsAUserListAndRemove(t *testing.T) {
	for name, refusal := range map[string]struct {
		err  error
		code string
	}{"no entitlement": {jobs.ErrNoEntitlement, "no_entitlement"}, "suspended": {jobs.ErrSuspended, "suspended"}} {
		r := newRig(t)
		r.m3gate.err = refusal.err
		for _, a := range []struct {
			what, path string
			body       any
		}{
			{"add", "/v1/m3/accounts", okAccountBody()},
			{"test", "/v1/m3/accounts/" + acctA1 + "/test", nil},
			{"bulk", "/v1/m3/accounts/bulk", map[string]any{"accounts": []any{okAccountBody()}}},
		} {
			r.m3.reset()
			resp, body := r.post(loginA, a.path, a.body)
			r.wantError(resp, body, 403, refusal.code)
			if len(r.m3.seen()) != 0 {
				t.Errorf("%s, %s: the module was reached", name, a.what)
			}
		}
		// the user still reads and removes their own accounts
		if resp, body := r.get(loginA, "/v1/m3/accounts"); resp.StatusCode != 200 {
			t.Errorf("%s: list %d %s", name, resp.StatusCode, body)
		}
		if resp, body := r.post(loginA, "/v1/m3/accounts/"+acctA1+"/remove", nil); resp.StatusCode != 200 {
			t.Errorf("%s: remove %d %s", name, resp.StatusCode, body)
		}
		if asks := r.m3gate.asks; len(asks) != 3 || asks[0] != userA {
			t.Errorf("%s: the gate was asked for %v, want the principal three times (add, test, bulk)", name, asks)
		}
	}
	// the control: a gate that lets the user through lets the three reach the module
	r := newRig(t)
	for _, path := range []string{"/v1/m3/accounts", "/v1/m3/accounts/" + acctA1 + "/test", "/v1/m3/accounts/bulk"} {
		var body any = okAccountBody()
		if strings.HasSuffix(path, "/test") {
			body = nil
		}
		if strings.HasSuffix(path, "/bulk") {
			body = map[string]any{"accounts": []any{okAccountBody()}}
		}
		if resp, raw := r.post(loginA, path, body); resp.StatusCode/100 != 2 {
			t.Fatalf("control %s: %d %s", path, resp.StatusCode, raw)
		}
	}
	// a failure of the database in the gate is the server's fault
	r.m3gate.err = errBoom
	resp, body := r.post(loginA, "/v1/m3/accounts", okAccountBody())
	r.wantError(resp, body, 500, "internal_error")
}

func TestM3Accounts_RemoveIsADeleteTheModuleAnswersWithTheOwnersAnd404ForTheRest(t *testing.T) {
	r := newRig(t)
	resp, body := r.post(loginA, "/v1/m3/accounts/"+acctA1+"/remove", nil)
	if m := r.wantStatus(resp, body, 200); m["removed"] != true || len(m) != 1 {
		t.Fatalf("removed: %s", body)
	}
	// the account is gone for its owner: the repeat is the module's 404, which is the API's
	resp, body = r.post(loginA, "/v1/m3/accounts/"+acctA1+"/remove", nil)
	r.wantError(resp, body, 404, "not_found")
	// a body is not part of the route: one over the small cap is refused, a query parameter too
	resp, body = r.do(req{method: http.MethodPost, path: "/v1/m3/accounts/" + acctB1 + "/remove?user_id=" + userA, token: r.token(loginB)})
	r.wantError(resp, body, 400, "bad_request")
	r.m3.fail["DeleteAccount"] = m3Kind(modclient.Timeout, 0, true)
	resp, body = r.post(loginB, "/v1/m3/accounts/"+acctB1+"/remove", nil)
	r.wantError(resp, body, 503, "account_outcome_unknown")
}

func TestM3Accounts_BulkAddsByJSONOrCSVAndAnswersOneCodePerRow(t *testing.T) {
	r := newRig(t)
	rows := []map[string]any{okAccountBody(), okAccountBody(), okAccountBody(), okAccountBody(), okAccountBody(), okAccountBody(), okAccountBody()}
	hostRow := m3.BulkRow{Row: 3, Reason: m3.RejectValidation, Fields: []m3.FieldIssue{{Field: "smtp_host", Issue: "forbidden_value"}}}
	r.m3.bulk = m3.BulkResult{Rows: []m3.BulkRow{
		{Row: 1, Created: true, Verification: m3.VerificationVerified}, {Row: 2, Created: true, Verification: m3.VerificationFailed, VerifyCode: m3.VerifyAuthFailed},
		hostRow, {Row: 4, Reason: m3.RejectDuplicate}, {Row: 5, Reason: m3.RejectAlignmentWarning}, {Row: 6, Reason: m3.RejectAccountCap}, {Row: 7, Reason: m3.RejectInternal},
	}}
	resp, body := r.post(loginA, "/v1/m3/accounts/bulk", map[string]any{"accounts": rows})
	m := r.wantStatus(resp, body, 200)
	results, _ := m["results"].([]any)
	var got []string
	for _, x := range results {
		row := x.(map[string]any)
		if len(row) != 3 {
			t.Fatalf("a row holds %v, want exactly line, ok and code", row)
		}
		got = append(got, fmt.Sprintf("%v:%v:%v", row["line"], row["ok"], row["code"]))
	}
	want := "1:true:created,2:true:created_unverified,3:false:host_forbidden,4:false:account_duplicate,5:false:from_gate_ack_required,6:false:account_limit_reached,7:false:internal_error"
	if strings.Join(got, ",") != want {
		t.Fatalf("rows %v, want %s", got, want)
	}
	if strings.Contains(string(body), canary) || strings.Contains(r.logs.String(), canary) {
		t.Fatal("the password is in the answer or the log")
	}
	if c := r.m3.seen(); len(c) != 1 || c[0].User != userA || c[0].Arg != 7 {
		t.Fatalf("the module was asked %+v", c)
	}
	// the same batch as a CSV: the line of a row is its line in the file; column names in any case; the secrets reach the adapter
	r.m3.reset()
	r.m3.bulk = m3.BulkResult{}
	csvBody := "SMTP_Host, smtp_username ,smtp_secret,From_Address,smtp_port,acknowledge_from_gate\n" +
		"h1.example.test,u1,pw1-" + canary + ",f1@example.test,465,true\n" +
		"h2.example.test,u2,pw2,f2@example.test,,\n"
	resp, body = csvReq(r, loginA, "/v1/m3/accounts/bulk", csvBody)
	m = r.wantStatus(resp, body, 200)
	results, _ = m["results"].([]any)
	if len(results) != 2 || results[0].(map[string]any)["line"] != float64(2) || results[1].(map[string]any)["line"] != float64(3) {
		t.Fatalf("the CSV answer: %s", body)
	}
	r.m3.mu.Lock()
	news := append([]m3.NewAccount(nil), r.m3.news...)
	r.m3.mu.Unlock()
	if len(news) != 2 || news[0].SMTPPort != 465 || !news[0].AcknowledgeAlignmentWarning || news[0].SMTPSecret.Reveal() != "pw1-"+canary || news[1].AcknowledgeAlignmentWarning || news[1].SMTPPort != 0 {
		t.Fatalf("the adapter received %+v", news)
	}
	if strings.Contains(string(body), canary) || strings.Contains(r.logs.String(), canary) {
		t.Fatal("a password of the CSV is in the answer or the log")
	}
}

func TestM3Accounts_BulkRefusesABadBatchWholeAndNamesTheLineNeverACell(t *testing.T) {
	r := newRig(t)
	good := okAccountBody()
	many := make([]map[string]any, m3.MaxBulkRows+1)
	for i := range many {
		many[i] = good
	}
	for name, c := range map[string]struct {
		ctype, body string
		status      int
		code        string
		check       func(envelopeOf)
	}{
		"no accounts":                 {"application/json", `{"accounts":[]}`, 400, "validation_failed", nil},
		"no member":                   {"application/json", `{}`, 400, "validation_failed", nil},
		"a list instead of an object": {"application/json", `[` + string(mustJSON(t, good)) + `]`, 400, "bad_request", nil},
		"a member naming a user":      {"application/json", `{"accounts":[{"smtp_host":"h","smtp_username":"u","smtp_secret":"s","from_address":"f","user_id":"` + userB + `"}]}`, 400, "bad_request", nil},
		"a user at the top":           {"application/json", `{"user_id":"` + userB + `","accounts":[]}`, 400, "bad_request", nil},
		"501 accounts":                {"application/json", `{"accounts":` + string(mustJSON(t, many)) + `}`, 400, "validation_failed", nil},
		"a row without a secret": {"application/json", `{"accounts":[` + string(mustJSON(t, good)) + `,{"smtp_host":"h","smtp_username":"u","from_address":"f"}]}`, 400, "validation_failed", func(e envelopeOf) {
			if e.Details["line"] != float64(2) || fmt.Sprint(e.Details["fields"]) != "[smtp_secret:required]" {
				t.Errorf("details %v, want line 2 and smtp_secret:required", e.Details)
			}
		}},
		"CSV with a column the API lacks": {"text/csv", "smtp_host,smtp_username,smtp_secret,from_address,user_id\nh,u,s,f," + userB + "\n", 400, "validation_failed", func(e envelopeOf) {
			if !strings.Contains(e.Details["reason"].(string), "column 5") || strings.Contains(e.Details["reason"].(string), "user_id") {
				t.Errorf("reason %v", e.Details["reason"])
			}
		}},
		"CSV with a column twice": {"text/csv", "smtp_host,smtp_host,smtp_username,smtp_secret,from_address\nh,h,u,s,f\n", 400, "validation_failed", nil},
		"CSV with no rows":        {"text/csv", "smtp_host,smtp_username,smtp_secret,from_address\n", 400, "validation_failed", nil},
		"CSV with no header":      {"text/csv", "", 400, "validation_failed", nil},
		"CSV with a short row": {"text/csv", "smtp_host,smtp_username,smtp_secret,from_address\nh,u,s\n", 400, "validation_failed", func(e envelopeOf) {
			if !strings.Contains(e.Details["reason"].(string), "line 2") || strings.Contains(e.Details["reason"].(string), "h,u") {
				t.Errorf("reason %v", e.Details["reason"])
			}
		}},
		"CSV with a port that is no number": {"text/csv", "smtp_host,smtp_username,smtp_secret,from_address,smtp_port\nh,u,pw-" + canary + ",f,x587\n", 400, "validation_failed", func(e envelopeOf) {
			if strings.Contains(e.Details["reason"].(string), canary) || strings.Contains(e.Details["reason"].(string), "x587") {
				t.Errorf("reason %v", e.Details["reason"])
			}
		}},
		"CSV with a bad acknowledgement": {"text/csv", "smtp_host,smtp_username,smtp_secret,from_address,acknowledge_from_gate\nh,u,s,f,maybe\n", 400, "validation_failed", nil},
		"a media type nobody takes":      {"text/plain", "x", 415, "unsupported_media_type", nil},
	} {
		r.m3.reset()
		resp, raw := r.do(req{method: http.MethodPost, path: "/v1/m3/accounts/bulk", token: r.token(loginA), ctype: c.ctype, body: []byte(c.body)})
		e := r.wantError(resp, raw, c.status, c.code)
		if c.check != nil {
			c.check(e)
		}
		if len(r.m3.seen()) != 0 {
			t.Errorf("%s: the module was reached", name)
		}
		if strings.Contains(string(raw), canary) || strings.Contains(r.logs.String(), canary) {
			t.Errorf("%s: the password is in the answer or the log", name)
		}
	}
	// 500 accounts is the most there can be (the control of the 501 above)
	r.m3.reset()
	many = many[:m3.MaxBulkRows]
	if resp, raw := r.post(loginA, "/v1/m3/accounts/bulk", map[string]any{"accounts": many}); resp.StatusCode != 200 || len(r.m3.seen()) != 1 {
		t.Fatalf("500 accounts: %d %.200s", resp.StatusCode, raw)
	}
	// a query parameter is refused
	resp, raw := r.post(loginA, "/v1/m3/accounts/bulk?user_id="+userB, map[string]any{"accounts": []any{good}})
	r.wantError(resp, raw, 400, "bad_request")
}

func TestM3Accounts_WithModule3NotConfiguredEveryAccountRouteIs503AndNeverReachesTheGateOrTheModule(t *testing.T) {
	r := newRig(t)
	r.api.m3 = nil
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/m3/accounts", nil}, {"POST", "/v1/m3/accounts", okAccountBody()}, {"POST", "/v1/m3/accounts/" + acctA1 + "/test", nil},
		{"POST", "/v1/m3/accounts/bulk", map[string]any{"accounts": []any{okAccountBody()}}}, {"POST", "/v1/m3/accounts/" + acctA1 + "/remove", nil},
	} {
		var resp *http.Response
		var body []byte
		if c.method == "GET" {
			resp, body = r.get(loginA, c.path)
		} else {
			resp, body = r.post(loginA, c.path, c.body)
		}
		r.wantError(resp, body, 503, "module_unavailable")
	}
	if len(r.m3.seen()) != 0 {
		t.Fatal("the module was reached")
	}
}

func TestM3Accounts_AClientThatCannotBeBuiltIsTheServersFaultNotAnAnswerAboutTheAccount(t *testing.T) {
	r := newRig(t)
	r.m3.forErr = errors.New("m3: the user id is not a lower-case uuid")
	resp, body := r.get(loginA, "/v1/m3/accounts")
	if e := r.wantError(resp, body, 500, "internal_error"); e.Details != nil || strings.Contains(string(body), "lower-case") {
		t.Fatalf("the cause reached the client: %s", body)
	}
}

// ---- the production wiring -----------------------------------------------------------------------------------------------------

func TestM3Accounts_TheProductionFactoryIsNilWithoutAnAdapterAndNeverATypedNil(t *testing.T) {
	if got := NewM3Accounts(nil); got != nil {
		t.Fatalf("NewM3Accounts(nil) = %v, want a nil interface", got)
	}
	if NewM3Gate(nil) != nil {
		t.Fatal("NewM3Gate(nil) is not nil")
	}
	// Deps with a typed nil of the stores the API tests for is "not given", never a panic at the first request
	if _, err := New(Deps{Service: newFakeStore(), Verifier: newRig(t).mint.Verifier(t), Resolver: &fakeResolver{}, M3Accounts: nil, M3Gate: nil, Suppressions: nil}); err != nil {
		t.Fatal(err)
	}
}

// pgRow is a pgx.Row that scans what it was given.
type pgRow struct {
	vals []any
	err  error
}

func (r pgRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *bool:
			*p = r.vals[i].(bool)
		case *string:
			*p = r.vals[i].(string)
		}
	}
	return nil
}

// gateDB answers the gate's two queries in turn.
type gateDB struct {
	rows  []pgRow
	asked []string
}

func (d *gateDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	d.asked = append(d.asked, sql+"|"+fmt.Sprint(args...))
	if len(d.rows) == 0 {
		return pgRow{err: pgx.ErrNoRows}
	}
	r := d.rows[0]
	d.rows = d.rows[1:]
	return r
}

func TestM3Gate_FollowsTheRuleOfTheJobStoreAndAsksAboutThePrincipalOnly(t *testing.T) {
	for name, c := range map[string]struct {
		rows []pgRow
		want error
	}{
		"no entitlement row":                    {nil, jobs.ErrNoEntitlement},
		"an entitlement that is not enabled":    {[]pgRow{{vals: []any{false, true}}}, jobs.ErrNoEntitlement},
		"an entitlement that has run out":       {[]pgRow{{vals: []any{true, false}}}, jobs.ErrNoEntitlement},
		"entitled and observed suspended":       {[]pgRow{{vals: []any{true, true}}, {vals: []any{"suspended"}}}, jobs.ErrSuspended},
		"entitled and observed active":          {[]pgRow{{vals: []any{true, true}}, {vals: []any{"active"}}}, nil},
		"entitled and nothing observed yet":     {[]pgRow{{vals: []any{true, true}}, {err: pgx.ErrNoRows}}, nil},
		"entitled and observation not known":    {[]pgRow{{vals: []any{true, true}}, {vals: []any{"unknown"}}}, nil},
		"the database fails on the entitlement": {[]pgRow{{err: errBoom}}, errBoom},
		"the database fails on the observation": {[]pgRow{{vals: []any{true, true}}, {err: errBoom}}, errBoom},
	} {
		db := &gateDB{rows: c.rows}
		err := NewM3Gate(db).Check(context.Background(), userA)
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		for _, q := range db.asked {
			if !strings.HasSuffix(q, "|"+userA) || !strings.Contains(q, "$1::uuid") {
				t.Errorf("%s: a statement was not scoped to the user: %s", name, q)
			}
		}
	}
}
