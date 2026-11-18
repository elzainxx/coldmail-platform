package m3_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// ---- routes with their own budgets, and no automatic retry -----------------------------------------------------------------------

func slowReply(d time.Duration, status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		time.Sleep(d)
		sendapitest.JSON(w, status, body)
	}
}

// The test button, create and bulk add are long calls and carry a budget of their own (the module's bounds, research 9.1), where every other route keeps the shared dial. The same slow module
// that makes a read time out is served by the three long ones: the delay is the paired control, the budget is the difference.
func TestLongRoutesGetTheirOwnBudget(t *testing.T) {
	if m3.SlowAccountTimeout != 40*time.Second || m3.BulkRowBudget != 30*time.Second || m3.MaxBulkRows != 500 {
		t.Fatalf("the documented budgets changed: %v %v %d", m3.SlowAccountTimeout, m3.BulkRowBudget, m3.MaxBulkRows)
	}
	if m3.BulkTimeout(1) >= m3.BulkTimeout(2) || m3.BulkTimeout(m3.MaxBulkRows) >= modclient.MaxRequestTimeout || m3.BulkTimeout(5) != m3.BulkBaseBudget+5*m3.BulkRowBudget {
		t.Fatalf("the bulk budget: %v %v", m3.BulkTimeout(1), m3.BulkTimeout(m3.MaxBulkRows))
	}
	delay := 500 * time.Millisecond
	opts := modclient.Options{Timeout: 150 * time.Millisecond}
	good := func(name string) string { return string(sendapitest.Read(t, fixtures, name)) }
	// the reads give up at the shared budget
	reads := map[string]func(c *m3.Client) error{
		"list":   func(c *m3.Client) error { _, err := c.ListAccounts(ctxT(t), ""); return err },
		"get":    func(c *m3.Client) error { _, err := c.GetAccount(ctxT(t), acct1); return err },
		"events": func(c *m3.Client) error { _, err := c.AccountEvents(ctxT(t), acct1, ""); return err },
	}
	for name, read := range reads {
		s := newStack(t, slowReply(delay, 200, good("accounts.list.empty.json")), opts)
		if err := read(s.client(t, userA)); modclient.KindOf(err) != modclient.Timeout {
			t.Fatalf("%s: %v, want a timeout at the shared budget", name, err)
		}
	}
	// the three long routes are served by the same slow module
	longs := map[string]struct {
		body string
		code int
		call func(c *m3.Client) error
	}{
		"create": {good("accounts.create.created.json"), 201, func(c *m3.Client) error { _, err := c.CreateAccount(ctxT(t), newAcct(t, 1)); return err }},
		"test":   {good("accounts.test.ok.json"), 200, func(c *m3.Client) error { _, err := c.TestAccount(ctxT(t), acct1, false); return err }},
		"bulk":   {good("accounts.bulk.ok.json"), 200, func(c *m3.Client) error { _, err := c.BulkAddAccounts(ctxT(t), batch(t, bulkRows(t))); return err }},
	}
	for name, l := range longs {
		s := newStack(t, slowReply(delay, l.code, l.body), opts)
		if err := l.call(s.client(t, userA)); err != nil {
			t.Fatalf("%s: a long route gave up at the shared budget: %v", name, err)
		}
	}
}

// Create, the test button and bulk add are never retried by the adapter, whatever the retry dial says: a repeat after a lost answer is a 409 duplicate, a click is a real authentication
// attempt against the user's provider, and a batch is paced row by row. The reads are retried by the same dial (the paired control: the dial is on).
func TestWritesAreNeverRetriedAndReadsAre(t *testing.T) {
	opts := modclient.Options{Retries: 2}
	for name, tc := range map[string]struct {
		call func(c *m3.Client) error
		want int
	}{
		"create": {func(c *m3.Client) error { _, err := c.CreateAccount(ctxT(t), newAcct(t, 1)); return err }, 1},
		"test":   {func(c *m3.Client) error { _, err := c.TestAccount(ctxT(t), acct1, false); return err }, 1},
		"bulk":   {func(c *m3.Client) error { _, err := c.BulkAddAccounts(ctxT(t), batch(t, 2)); return err }, 1},
		"list":   {func(c *m3.Client) error { _, err := c.ListAccounts(ctxT(t), ""); return err }, 3},
		"get":    {func(c *m3.Client) error { _, err := c.GetAccount(ctxT(t), acct1); return err }, 3},
		"events": {func(c *m3.Client) error { _, err := c.AccountEvents(ctxT(t), acct1, ""); return err }, 3},
	} {
		s := newStack(t, reply(503, sendapitest.Envelope("unavailable")), opts)
		err := tc.call(s.client(t, userA))
		wantKind(t, err, modclient.Unavailable)
		if got := s.stub.Count(); got != tc.want {
			t.Fatalf("%s: %d requests, want %d", name, got, tc.want)
		}
	}
	// a rate limit on the test button is a refusal to wait out, not to retry, and the module sends no Retry-After
	s := newStack(t, reply(429, sendapitest.Envelope("rate_limited")), opts)
	_, err := s.client(t, userA).TestAccount(ctxT(t), acct1, false)
	if e := wantKind(t, err, modclient.RateLimited); e.RetryAfter != 0 || s.stub.Count() != 1 {
		t.Fatalf("a 429: retry-after %v after %d requests", e.RetryAfter, s.stub.Count())
	}
}

// MaybeApplied is true exactly when the request was sent and no usable answer came back, so that the caller lists before it retries a create; it is false for a refusal the module made, for a
// request that never left and for no error at all.
func TestMaybeAppliedIsTrueOnlyWhereTheOutcomeIsOpen(t *testing.T) {
	create := func(c *m3.Client) error { _, err := c.CreateAccount(ctxT(t), newAcct(t, 1)); return err }
	for name, tc := range map[string]struct {
		h    handler
		want bool
	}{
		"module unavailable":  {reply(503, sendapitest.Envelope("unavailable")), true},
		"module fault":        {reply(500, sendapitest.Envelope("internal_error")), true},
		"a truncated answer":  {reply(201, `{"id":"`), true},
		"not json":            {reply(201, `<html>`), true},
		"a refusal":           {reply(400, sendapitest.Envelope("validation_failed")), false},
		"a duplicate":         {reply(409, sendapitest.Envelope("conflict", [2]string{"smtp_username", "conflict"})), false},
		"a rate limit":        {reply(429, sendapitest.Envelope("rate_limited")), false},
		"the recorded answer": {replyFixture(201, "accounts.create.created.json", t), false},
	} {
		s := newStack(t, tc.h, modclient.Options{})
		err := create(s.client(t, userA))
		if got := m3.MaybeApplied(err); got != tc.want {
			t.Fatalf("%s: MaybeApplied = %v for %v", name, got, err)
		}
	}
	// a module that is down was never reached
	down := newStack(t, reply(201, "{}"), modclient.Options{})
	down.stub.Close()
	if err := create(down.client(t, userA)); modclient.KindOf(err) != modclient.Unavailable || m3.MaybeApplied(err) {
		t.Fatalf("a refused connection: %v", err)
	}
	// the caller's own deadline or cancel after the request left
	slow := newStack(t, slowReply(600*time.Millisecond, 201, "{}"), modclient.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := slow.client(t, userA).CreateAccount(ctx, newAcct(t, 1)); modclient.KindOf(err) != modclient.Timeout || !m3.MaybeApplied(err) {
		t.Fatalf("a deadline in flight: %v", err)
	}
	// the module refused the host (Phase 3 close-out C4: its destination guard answers 400 naming {smtp_host, forbidden_value}): a refusal, so nothing was created and the outcome is not open
	bad := newStack(t, reply(201, "{}"), modclient.Options{})
	// the control: a good account reaches the stub, whose answer is not an account, and then the outcome IS open
	if err := create(bad.client(t, userA)); modclient.KindOf(err) != modclient.Contract || !m3.MaybeApplied(err) {
		t.Fatalf("an answer that is not an account: %v", err)
	}
	forbidden := newStack(t, reply(400, sendapitest.Envelope("validation_failed", [2]string{"smtp_host", "forbidden_value"})), modclient.Options{})
	n := newAcct(t, 1)
	n.SMTPHost = "localhost"
	_, err := forbidden.client(t, userA).CreateAccount(ctxT(t), n)
	if modclient.KindOf(err) != modclient.Refused || m3.MaybeApplied(err) || !errors.Is(err, m3.ErrHostForbidden) || forbidden.stub.Count() != 1 {
		t.Fatalf("a host the module refused: %v (%d requests)", err, forbidden.stub.Count())
	}
	if m3.MaybeApplied(nil) || m3.MaybeApplied(errors.New("plain")) {
		t.Fatal("no error, or a foreign one, is not an open outcome")
	}
}

// ---- the SMTP host rule --------------------------------------------------------------------------------------------------------

// What is sent is what the module reads: the tenant in the body, the secret as itself in smtp_secret and nowhere else, only the fields the module's strict decoder knows, and a bulk row without
// a user_id (the module refuses an unknown field). The secret prints as [redacted] in every fmt verb and in JSON, and is in no error.
func TestTheSecretIsSentOnceAndPrintedNever(t *testing.T) {
	s := newStack(t, replyFixture(201, "accounts.create.created.json", t), modclient.Options{})
	n := newAcct(t, 1)
	secret := n.SMTPSecret.Reveal()
	port, limit := 465, 250
	n.SMTPPort, n.DailyLimit, n.AcknowledgeAlignmentWarning, n.FromName = port, limit, true, str("Sender One")
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(verb, n), secret) {
			t.Fatalf("%s of a NewAccount prints the secret", verb)
		}
	}
	if b, err := json.Marshal(n); err != nil || strings.Contains(string(b), secret) {
		t.Fatalf("json of a NewAccount: %v %s", err, b)
	}
	if _, err := s.client(t, userA).CreateAccount(ctxT(t), n); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(s.stub.Requests()[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	if got["smtp_secret"] != secret || got["user_id"] != userA || got["smtp_host"] != "smtp.example.test" || got["smtp_port"] != float64(port) || got["daily_limit"] != float64(limit) ||
		got["acknowledge_alignment_warning"] != true || got["from_name"] != "Sender One" {
		t.Fatalf("the body: %v", keys(got))
	}
	known := map[string]bool{"user_id": true, "smtp_host": true, "smtp_port": true, "smtp_username": true, "smtp_secret": true, "from_address": true, "from_name": true, "daily_limit": true, "acknowledge_alignment_warning": true}
	for k := range got {
		if !known[k] {
			t.Fatalf("the body names %q, which the module's strict decoder refuses", k)
		}
	}
	// the defaults are left to the module: no port, no limit, no name, no acknowledgment
	s2 := newStack(t, replyFixture(201, "accounts.create.created.json", t), modclient.Options{})
	if _, err := s2.client(t, userA).CreateAccount(ctxT(t), newAcct(t, 2)); err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	_ = json.Unmarshal(s2.stub.Requests()[0].Body, &plain)
	for _, k := range []string{"smtp_port", "daily_limit", "from_name", "acknowledge_alignment_warning"} {
		if _, ok := plain[k]; ok {
			t.Fatalf("a default was sent as %s", k)
		}
	}
	// bulk: the tenant once at the top, never in a row
	s3 := newStack(t, replyFixture(200, "accounts.bulk.ok.json", t), modclient.Options{})
	if _, err := s3.client(t, userA).BulkAddAccounts(ctxT(t), batch(t, bulkRows(t))); err != nil {
		t.Fatal(err)
	}
	var bulk struct {
		UserID   string           `json:"user_id"`
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(s3.stub.Requests()[0].Body, &bulk); err != nil || bulk.UserID != userA || len(bulk.Accounts) != bulkRows(t) {
		t.Fatalf("the bulk body: %v user %s, %d accounts", err, bulk.UserID, len(bulk.Accounts))
	}
	for i, row := range bulk.Accounts {
		if _, ok := row["user_id"]; ok || row["smtp_secret"] == "" || row["smtp_secret"] == nil {
			t.Fatalf("row %d: %v", i+1, keys(row))
		}
	}
	// no error carries it
	bad := newStack(t, reply(500, sendapitest.Envelope("internal_error")), modclient.Options{})
	_, err := bad.client(t, userA).CreateAccount(ctxT(t), n)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), secret) {
		t.Fatalf("an error: %v", err)
	}
	empty := newAcct(t, 3)
	empty.SMTPSecret = config.Secret("")
	if _, err := s.client(t, userA).CreateAccount(ctxT(t), empty); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("an empty secret: %v", err)
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- bulk add: what comes back ---------------------------------------------------------------------------------------------------

// leaks lists the internal-looking pieces of the module's own error text that appear in s.
func leaks(s string) []string {
	var out []string
	for _, piece := range []string{"account_write_error", "dial tcp", "203.0.113.9", "connection refused", "5432"} {
		if strings.Contains(s, piece) {
			out = append(out, piece)
		}
	}
	return out
}

// What cannot be sent is refused before anything is: no accounts, too many, or a row with no secret (named by its row). The SMTP host is not judged here: the module's destination guard does.
func TestBulkRefusesWhatCannotBeSentBeforeAnyCall(t *testing.T) {
	s := newStack(t, replyFixture(200, "accounts.bulk.ok.json", t), modclient.Options{})
	c := s.client(t, userA)
	if _, err := c.BulkAddAccounts(ctxT(t), nil); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("no accounts: %v", err)
	}
	if _, err := c.BulkAddAccounts(ctxT(t), batch(t, m3.MaxBulkRows+1)); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("too many: %v", err)
	}
	rows := batch(t, 4)
	rows[1].SMTPSecret = config.Secret("")
	if _, err := c.BulkAddAccounts(ctxT(t), rows); modclient.KindOf(err) != modclient.Invalid || !strings.Contains(err.Error(), "row 2") {
		t.Fatalf("no secret in row 2: %v", err)
	}
	if s.stub.Count() != 0 {
		t.Fatalf("%d request(s) were made for a batch that could not be sent", s.stub.Count())
	}
	// control: the largest batch is sent (the answer is refused because it holds five rows, which is the point: the count is checked against what was sent)
	_, err := c.BulkAddAccounts(ctxT(t), batch(t, m3.MaxBulkRows))
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "results" || s.stub.Count() != 1 {
		t.Fatalf("the largest batch: %v (%d requests)", err, s.stub.Count())
	}
	var sent struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(s.stub.Requests()[0].Body, &sent); err != nil || len(sent.Accounts) != m3.MaxBulkRows {
		t.Fatalf("%v %d", err, len(sent.Accounts))
	}
}

// ---- ids and cursors are checked before the network ---------------------------------------------------------------------------------

func TestAccountIDsAndCursorsAreCheckedBeforeTheNetwork(t *testing.T) {
	s := newStack(t, moduleStub(t), modclient.Options{})
	c := s.client(t, userA)
	for _, bad := range []string{"", "x", strings.ToUpper(userB), acct1 + "/test", "../" + acct1} {
		if _, err := c.GetAccount(ctxT(t), bad); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("get %q: %v", bad, err)
		}
		if _, err := c.TestAccount(ctxT(t), bad, false); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("test %q: %v", bad, err)
		}
		if _, err := c.AccountEvents(ctxT(t), bad, ""); modclient.KindOf(err) != modclient.Invalid {
			t.Fatalf("events %q: %v", bad, err)
		}
	}
	long := strings.Repeat("a", 2000)
	if _, err := c.ListAccounts(ctxT(t), long); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a long cursor: %v", err)
	}
	if _, err := c.AccountEvents(ctxT(t), acct1, long); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a long events cursor: %v", err)
	}
	if s.stub.Count() != 0 {
		t.Fatalf("%d request(s) were made", s.stub.Count())
	}
	if _, err := c.GetAccount(ctxT(t), acct1); err != nil {
		t.Fatalf("control: %v", err)
	}
	// the page size is the module's maximum and the cursor is passed back as it came
	if _, err := c.ListAccounts(ctxT(t), "opaque_cursor-1"); err != nil {
		t.Fatal(err)
	}
	q := s.stub.Requests()[1].Query
	if q.Get("limit") != "200" || q.Get("cursor") != "opaque_cursor-1" || q.Get("user_id") != userA {
		t.Fatalf("query: %v", q)
	}
}

// ---- the whole listing ----------------------------------------------------------------------------------------------------------

func acctJSON(id, user string) string {
	return `{"id":"` + id + `","user_id":"` + user + `","smtp_host":"smtp.example.test","smtp_port":587,"smtp_username":"u@example.test","from_address":"u@example.test","from_name":null,` +
		`"has_credential":true,"status":{"paused":false,"pause_reason":null,"quarantined":false,"disabled":false},"verification":{"state":"verified"},` +
		`"from_alignment":{"state":"aligned"},"daily_limit":500,"created_at":"2026-11-17T12:00:00Z","updated_at":"2026-11-17T12:00:00Z"}`
}

func nthID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func pageJSON(user string, next string, ids ...int) string {
	var items []string
	for _, n := range ids {
		items = append(items, acctJSON(nthID(n), user))
	}
	cur := "null"
	if next != "" {
		cur = `"` + next + `"`
	}
	return `{"items":[` + strings.Join(items, ",") + `],"next_cursor":` + cur + `}`
}

// AllAccounts reads every page, in the module's order, each account once, passing the cursor back as it came; and it is all or nothing: a page that cannot be read gives an error and no accounts,
// a cursor that repeats, an account on two pages, or a cursor that never ends is a Contract error naming the key, and the walk stops at MaxPages.
func TestAllAccountsReadsEveryPageOrNothing(t *testing.T) {
	pages := map[string]string{"": pageJSON(userA, "c1", 1, 2), "c1": pageJSON(userA, "c2", 3, 4), "c2": pageJSON(userA, "", 5)}
	serve := func(pages map[string]string) handler {
		return func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if p, ok := pages[r.URL.Query().Get("cursor")]; ok {
				sendapitest.JSON(w, 200, p)
				return
			}
			sendapitest.JSON(w, 500, sendapitest.Envelope("internal_error"))
		}
	}
	s := newStack(t, serve(pages), modclient.Options{})
	all, err := s.client(t, userA).AllAccounts(ctxT(t))
	if err != nil || len(all) != 5 {
		t.Fatalf("%v %d", err, len(all))
	}
	var got []string
	for _, a := range all {
		got = append(got, a.ID)
	}
	if p := sendapitest.ExactSet(got, []string{nthID(1), nthID(2), nthID(3), nthID(4), nthID(5)}); len(p) != 0 {
		t.Fatalf("%v", p)
	}
	var asked []string
	for _, r := range s.stub.Requests() {
		asked = append(asked, r.Query.Get("cursor"))
	}
	if strings.Join(asked, ",") != ",c1,c2" {
		t.Fatalf("cursors asked: %v", asked)
	}

	// a page that cannot be read: an error and no accounts, never the two pages that were read
	partial := map[string]string{"": pageJSON(userA, "c1", 1, 2)}
	got2, err := newStack(t, serve(partial), modclient.Options{}).client(t, userA).AllAccounts(ctxT(t))
	if wantKind(t, err, modclient.Internal); got2 != nil || !strings.Contains(err.Error(), "NOT complete") {
		t.Fatalf("a failed page: %v %v", err, got2)
	}
	// a cursor that repeats, an account on two pages
	for name, tc := range map[string]struct {
		pages map[string]string
		key   string
	}{
		"a repeated cursor":   {map[string]string{"": pageJSON(userA, "c1", 1), "c1": pageJSON(userA, "c1", 2)}, "next_cursor"},
		"the same account":    {map[string]string{"": pageJSON(userA, "c1", 1), "c1": pageJSON(userA, "", 1)}, "items"},
		"an empty first page": {map[string]string{"": pageJSON(userA, "c1")}, "next_cursor"},
	} {
		got, err := newStack(t, serve(tc.pages), modclient.Options{}).client(t, userA).AllAccounts(ctxT(t))
		if ce := wantKind(t, err, modclient.Contract); ce.Key != tc.key || got != nil {
			t.Fatalf("%s: %v %v", name, err, got)
		}
	}
	// a cursor that never ends stops at MaxPages
	var n atomic.Int32
	endless := newStack(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		i := int(n.Add(1))
		sendapitest.JSON(w, 200, pageJSON(userA, fmt.Sprintf("c%d", i), i))
	}, modclient.Options{})
	got3, err := endless.client(t, userA).AllAccounts(ctxT(t))
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "next_cursor" || got3 != nil || endless.stub.Count() != m3.MaxPages {
		t.Fatalf("an endless listing: %v after %d requests", err, endless.stub.Count())
	}
}

// ---- cross-tenant: what the module answers, per route family ---------------------------------------------------------------------------

// tenantModule plays module 3 for a single owner, answering a stranger the way the module's code does (research m3-api 3.4): a job, a report by job and an account are 404 not_found (the
// report names the selector the caller sent), a campaign report and an account's events are an empty 200, a list is the stranger's own (here: nothing), a quote is the stranger's own
// feasibility (zero accounts), and an unknown tenant on create and bulk is a 400 that names user_id. The live proof of every row is B11.
func tenantModule(t testing.TB) handler {
	good := func(name string) string { return string(sendapitest.Read(t, fixtures, name)) }
	// the answers that depend on what a recording holds, chosen by what it holds (recorded_helpers_test.go): a whole campaign in one page, a listing with accounts, the quote of a count
	campaignOwner, campaignStranger := good(terminalCampaign(t)), string(emptyCampaignBody(t))
	listOwner, quoteStranger := good(populatedList(t)), string(strangerQuoteBody(t))
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		p, q := r.URL.Path, r.URL.Query()
		user := q.Get("user_id")
		if user == "" {
			user = bodyUser(body)
		}
		owner := user == userA
		notFound := func(field string) {
			if field == "" {
				sendapitest.JSON(w, 404, sendapitest.Envelope("not_found"))
				return
			}
			sendapitest.JSON(w, 404, sendapitest.Envelope("not_found", [2]string{field, "not_found"}))
		}
		switch {
		case p == "/v1/run":
			if owner {
				sendapitest.JSON(w, 202, good("run.created.json"))
				return
			}
			notFound("job_item_id")
		case p == "/v1/report" && q.Get("campaign_id") != "":
			if owner {
				sendapitest.JSON(w, 200, campaignOwner)
				return
			}
			sendapitest.JSON(w, 200, campaignStranger)
		case p == "/v1/report":
			if owner {
				sendapitest.JSON(w, 200, good("report.job.sent.json"))
				return
			}
			if q.Get("job_id") != "" {
				notFound("job_id")
				return
			}
			notFound("job_item_id")
		case p == "/v1/quote":
			if owner {
				sendapitest.JSON(w, 200, good("quote.feasible.json"))
				return
			}
			sendapitest.JSON(w, 200, quoteStranger)
		case p == "/v1/send-byo/accounts" && r.Method == http.MethodPost, p == "/v1/send-byo/accounts/bulk":
			if owner {
				if p == "/v1/send-byo/accounts" {
					sendapitest.JSON(w, 201, good("accounts.create.created.json"))
				} else {
					sendapitest.JSON(w, 200, good("accounts.bulk.ok.json"))
				}
				return
			}
			sendapitest.JSON(w, 400, sendapitest.Envelope("validation_failed", [2]string{"user_id", "not_found"}))
		case p == "/v1/send-byo/accounts":
			if owner {
				sendapitest.JSON(w, 200, listOwner)
				return
			}
			sendapitest.JSON(w, 200, `{"items":[],"next_cursor":null}`)
		case strings.HasSuffix(p, "/events"):
			if owner {
				sendapitest.JSON(w, 200, good("accounts.events.page.json"))
				return
			}
			sendapitest.JSON(w, 200, good("accounts.events.empty.json"))
		case strings.HasSuffix(p, "/test"):
			if owner {
				sendapitest.JSON(w, 200, good("accounts.test.ok.json"))
				return
			}
			notFound("")
		default:
			if owner {
				sendapitest.JSON(w, 200, good("accounts.get.ok.json"))
				return
			}
			notFound("")
		}
	}
}

// The cross-tenant matrix as the adapter sees it: tenant B asks for tenant A's resource and gets the answer the module's code gives (404, or an empty 200, or its own empty set), each as a typed
// result and never as A's data; and the same call by the owner succeeds (the paired control of every row).
func TestCrossTenantMatrixAsTheModuleAnswers(t *testing.T) {
	notFound := func(field string) func(t testing.TB, v any, err error) {
		return func(t testing.TB, v any, err error) {
			wantKind(t, err, modclient.NotFound)
			if f, ok := sendapi.NotFoundField(err); field != "" && (!ok || f != field) {
				t.Fatalf("names %q %v, want %q", f, ok, field)
			}
			if !isZero(v) {
				t.Fatalf("a value came back: %+v", v)
			}
		}
	}
	type row struct {
		name  string
		do    func(c *m3.Client) (any, error)
		owner func(t testing.TB, v any, err error)
		other func(t testing.TB, v any, err error)
	}
	ctx := ctxT(t)
	ok := func(t testing.TB, v any, err error) {
		if err != nil || isZero(v) {
			t.Fatalf("the owner: %v %+v", err, v)
		}
	}
	for _, r := range []row{
		{"run with another tenant's job item", func(c *m3.Client) (any, error) { return c.Run(ctx, jobItem) }, ok, notFound("job_item_id")},
		{"report by job item", func(c *m3.Client) (any, error) { return c.ReportJob(ctx, jobItem) }, ok, notFound("job_item_id")},
		{"report by job id", func(c *m3.Client) (any, error) { return c.ReportJobByID(ctx, jobID) }, ok, notFound("job_id")},
		{"campaign report", func(c *m3.Client) (any, error) { return c.ReportCampaign(ctx, campaign) },
			func(t testing.TB, v any, err error) {
				rep := v.(sendapi.CampaignReport)
				if err != nil || rep.NothingKnown() || len(rep.Items) != length(t, doc(t, terminalCampaign(t)), "items.results") || !rep.ItemsMatchTotal() {
					t.Fatalf("the owner: %v %+v", err, rep)
				}
			},
			func(t testing.TB, v any, err error) {
				// the module answers a stranger's campaign with an EMPTY 200, so all the adapter can say is "nothing known"
				if rep := v.(sendapi.CampaignReport); err != nil || !rep.NothingKnown() || len(rep.Items) != 0 || rep.Progress.IsComplete {
					t.Fatalf("a stranger: %v %+v", err, rep)
				}
			}},
		{"feasibility", func(c *m3.Client) (any, error) { return c.Feasibility(ctx, quoteCount(t, "quote.feasible.json")) },
			func(t testing.TB, v any, err error) {
				f := v.(m3.Feasibility)
				if err != nil || !f.Feasible || f.Capacity.EligibleAccounts != asInt(t, doc(t, "quote.feasible.json"), "capacity.eligible_accounts") {
					t.Fatalf("the owner: %v %+v", err, f)
				}
			},
			func(t testing.TB, v any, err error) {
				// the stranger's own feasibility: no accounts, so not feasible, for a reason the module names
				if f := v.(m3.Feasibility); err != nil || f.Feasible || len(f.Reasons) != 1 || f.Reasons[0].Code != m3.InfeasibleNoEligibleAccounts || f.Capacity.EligibleAccounts != 0 {
					t.Fatalf("a stranger: %v %+v", err, f)
				}
			}},
		{"get an account", func(c *m3.Client) (any, error) { return c.GetAccount(ctx, acct1) }, ok, notFound("")},
		{"test an account", func(c *m3.Client) (any, error) { return c.TestAccount(ctx, acct1, false) }, ok, notFound("")},
		{"events of an account", func(c *m3.Client) (any, error) { return c.AccountEvents(ctx, acct1, "") },
			func(t testing.TB, v any, err error) {
				if p := v.(m3.EventPage); err != nil || len(p.Items) != 2 {
					t.Fatalf("the owner: %v %+v", err, p)
				}
			},
			func(t testing.TB, v any, err error) {
				// an EMPTY 200, not a 404: the module does no existence check there, so an empty page is no evidence about the account
				if p := v.(m3.EventPage); err != nil || len(p.Items) != 0 || p.NextCursor != "" {
					t.Fatalf("a stranger: %v %+v", err, p)
				}
			}},
		{"list the accounts", func(c *m3.Client) (any, error) { return c.ListAccounts(ctx, "") },
			func(t testing.TB, v any, err error) {
				p := v.(m3.AccountPage)
				if err != nil || len(p.Items) != length(t, doc(t, populatedList(t)), "items") || len(p.Items) == 0 {
					t.Fatalf("the owner: %v %+v", err, p)
				}
				for _, a := range p.Items {
					if a.UserID != userA {
						t.Fatalf("the owner's listing holds an account of %s", a.UserID)
					}
				}
			},
			func(t testing.TB, v any, err error) {
				if p := v.(m3.AccountPage); err != nil || len(p.Items) != 0 {
					t.Fatalf("a stranger: %v %+v", err, p)
				}
			}},
		{"create for an unknown tenant", func(c *m3.Client) (any, error) { return c.CreateAccount(ctx, newAcct(t, 1)) }, ok,
			func(t testing.TB, v any, err error) {
				wantKind(t, err, modclient.Refused)
				if d := detailFields(err); len(d) != 1 || d[0] != "user_id/not_found" || !isZero(v) {
					t.Fatalf("details %v: %+v", d, v)
				}
			}},
		{"bulk add for an unknown tenant", func(c *m3.Client) (any, error) { return c.BulkAddAccounts(ctx, batch(t, bulkRows(t))) }, ok,
			func(t testing.TB, v any, err error) {
				wantKind(t, err, modclient.Refused)
				if d := detailFields(err); len(d) != 1 || d[0] != "user_id/not_found" || !isZero(v) {
					t.Fatalf("details %v: %+v", d, v)
				}
			}},
	} {
		t.Run(r.name, func(t *testing.T) {
			s := newStack(t, tenantModule(t), modclient.Options{})
			v, err := r.do(s.client(t, userA))
			r.owner(t, v, err)
			v, err = r.do(s.client(t, userB))
			r.other(t, v, err)
		})
	}
}

// A listing that the module fills with another tenant's account is refused, not shown: the answer's own tenant is checked against the one the client was built for.
func TestAnotherTenantsAccountIsNeverReturned(t *testing.T) {
	s := newStack(t, reply(200, pageJSON(userB, "", 1)), modclient.Options{})
	got, err := s.client(t, userA).ListAccounts(ctxT(t), "")
	if ce := wantKind(t, err, modclient.Contract); ce.Key != "items[0].user_id" || !isZero(got) {
		t.Fatalf("%v %+v", err, got)
	}
	// control: the same page for its own tenant is accepted
	own := newStack(t, reply(200, pageJSON(userA, "", 1)), modclient.Options{})
	if p, err := own.client(t, userA).ListAccounts(ctxT(t), ""); err != nil || len(p.Items) != 1 {
		t.Fatalf("%v %+v", err, p)
	}
}

// ---- the feasibility quote, beyond the recorded bodies ---------------------------------------------------------------------------------

// A reason the adapter does not know is still an infeasible quote, with the Unrecognised word and none of the module's text; a feasible quote whose projection is switched off (no duration and no
// basis) is accepted with a nil duration; the count is bounded before anything is sent.
func TestFeasibilityBeyondTheRecordedBodies(t *testing.T) {
	infeasible := sendapitest.Read(t, fixtures, "quote.infeasible.json")
	rec := doc(t, "quote.infeasible.json")
	count, nReasons := asInt(t, rec, "recipient_count"), length(t, rec, "reasons")
	if nReasons == 0 {
		t.Fatal("the recorded infeasible quote holds no reason: the subject is empty")
	}
	// the control: the recording itself, every reason of which the adapter knows (so that an Unrecognised below is the mutation's doing)
	control, err := newStack(t, reply(200, string(infeasible)), modclient.Options{}).client(t, userA).Feasibility(ctxT(t), count)
	if err != nil || len(control.Reasons) != nReasons {
		t.Fatalf("the recorded quote: %v %+v", err, control)
	}
	for i, r := range control.Reasons {
		if r.Code == m3.InfeasibleUnrecognised {
			t.Fatalf("the recorded reason %d is read as unrecognised: %+v", i, r)
		}
	}
	// each recorded reason in turn, with a code the adapter does not know: still an infeasible quote, the reason is the Unrecognised word with the cause the module named (read from the
	// recording) and none of the module's text, and the other reasons are read as they were
	for i := 0; i < nReasons; i++ {
		body, err := sendapitest.Mutate(infeasible, fmt.Sprintf("reasons[%d].code", i), sendapitest.Set, "brand_new_reason_zzz")
		if err != nil {
			t.Fatal(err)
		}
		f, err := newStack(t, reply(200, string(body)), modclient.Options{}).client(t, userA).Feasibility(ctxT(t), count)
		if err != nil || f.Feasible || len(f.Reasons) != nReasons || f.Reasons[i].Code != m3.InfeasibleUnrecognised || f.Reasons[i].Cause != sendapi.Cause(asStr(t, rec, fmt.Sprintf("reasons[%d].cause", i))) || strings.Contains(reflectDump(f), "zzz") {
			t.Fatalf("an unknown reason at %d: %v %+v", i, err, f)
		}
		for j, r := range f.Reasons {
			if j != i && r.Code != control.Reasons[j].Code {
				t.Fatalf("the unknown reason at %d changed the reading of reason %d: %+v", i, j, f)
			}
		}
	}
	// the same beside a second, known reason: a quote of both caps (built from the recording: it holds the queue cap's reason only) whose first reason has a code the adapter does not know
	both, nBoth := bothCapsQuote(t)
	if f, err := newStack(t, reply(200, string(both)), modclient.Options{}).client(t, userA).Feasibility(ctxT(t), nBoth); err != nil || len(f.Reasons) != 2 || f.Reasons[0].Code != m3.InfeasibleCampaignCap || f.Reasons[1].Code != m3.InfeasibleExceedsQueueCap {
		t.Fatalf("the control (both caps, every code known): %v %+v", err, f)
	}
	unknown, err := sendapitest.Mutate(both, "reasons[0].code", sendapitest.Set, "brand_new_reason_zzz")
	if err != nil {
		t.Fatal(err)
	}
	s := newStack(t, reply(200, string(unknown)), modclient.Options{})
	f, err := s.client(t, userA).Feasibility(ctxT(t), nBoth)
	if err != nil || f.Feasible || len(f.Reasons) != 2 || f.Reasons[0].Code != m3.InfeasibleUnrecognised || f.Reasons[0].Cause != sendapi.CauseCampaignCap || f.Reasons[1].Code != m3.InfeasibleExceedsQueueCap || strings.Contains(reflectDump(f), "zzz") {
		t.Fatalf("an unknown reason: %v %+v", err, f)
	}
	feasible := sendapitest.Read(t, fixtures, "quote.feasible.json")
	off, err := sendapitest.Mutate(feasible, "capacity.estimated_duration_sec", sendapitest.Delete, nil)
	if err != nil {
		t.Fatal(err)
	}
	off, _ = sendapitest.Mutate(off, "capacity.estimate_basis", sendapitest.Delete, nil)
	off, _ = sendapitest.Mutate(off, "warnings", sendapitest.Set, []any{})
	s2 := newStack(t, reply(200, string(off)), modclient.Options{})
	f2, err := s2.client(t, userA).Feasibility(ctxT(t), quoteCount(t, "quote.feasible.json"))
	if err != nil || !f2.Feasible || f2.Capacity.EstimatedDurationSec != nil || f2.Capacity.EstimateBasis != "" || len(f2.Warnings) != 0 {
		t.Fatalf("a quote without a projection: %v %+v", err, f2)
	}
	// the count: bounded before the network
	s3 := newStack(t, replyFixture(200, "quote.feasible.json", t), modclient.Options{})
	for _, n := range []int64{-1, sendapi.MaxRecipientCount + 1} {
		f, err := s3.client(t, userA).Feasibility(ctxT(t), n)
		if modclient.KindOf(err) != modclient.Invalid || !isZero(f) {
			t.Fatalf("%d: %v", n, err)
		}
	}
	if s3.stub.Count() != 0 {
		t.Fatal("a bad count was sent")
	}
	// a module that is down is a typed error and the zero verdict, never "feasible"
	down := newStack(t, replyFixture(200, "quote.feasible.json", t), modclient.Options{})
	down.stub.Close()
	if f, err := down.client(t, userA).Feasibility(ctxT(t), quoteCount(t, "quote.feasible.json")); modclient.KindOf(err) != modclient.Unavailable || !isZero(f) || f.Feasible {
		t.Fatalf("a module that is down: %v %+v", err, f)
	}
}
