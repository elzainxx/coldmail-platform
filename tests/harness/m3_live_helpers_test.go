//go:build harness

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
)

// Helpers of TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes (m3_live_test.go). Every name starts with `m3l` so it cannot clash with another file's helpers.
//
// The test talks to the REAL module 3 (control and worker) and a REAL SMTP sink over HTTP, and to the live database twice: as a SUPERUSER (seeding, and reading what module 3 and the ledger recorded) and as
// platform_app through the real entitlements.Store (the writes an admin makes). It never calls Store.ExpireDue or Reconciler.RunOnce and builds no m3admin client: the platform SERVICE running in the same stack
// does that, exactly as in production, and this test only writes the admin's entitlement changes and OBSERVES the service's work in the database.

const (
	m3lEnvURL      = "HARNESS_M3_URL"       // module 3's control: the service token (Domain A) and the admin key (Domain B) on the same port
	m3lEnvToken    = "HARNESS_M3_TOKEN"     // Domain A
	m3lEnvAdminKey = "HARNESS_M3_ADMIN_KEY" // Domain B
	m3lEnvSinkURL  = "HARNESS_SINK_URL"     // the SMTP sink's HTTP API (Mailpit)

	m3lSinkPageSize = 500
)

// ---- the HTTP side: module 3 and the sink ----

type m3lEnv struct {
	ctx      context.Context
	m3URL    string
	sinkURL  string
	token    config.Secret
	adminKey config.Secret
	hc       *http.Client
}

// m3lBaseURL reads an http(s) base URL from the environment (FAILS when it is missing or not a URL) and strips a trailing slash.
func m3lBaseURL(t *testing.T, name string) string {
	t.Helper()
	raw := strings.TrimSpace(requireEnv(t, name))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		t.Fatalf("%s must be an http(s) URL with a host and no credentials", name)
	}
	return strings.TrimRight(raw, "/")
}

func m3lNewEnv(t *testing.T, ctx context.Context) *m3lEnv {
	t.Helper()
	e := &m3lEnv{
		ctx:      ctx,
		m3URL:    m3lBaseURL(t, m3lEnvURL),
		sinkURL:  m3lBaseURL(t, m3lEnvSinkURL),
		token:    config.Secret(strings.TrimSpace(requireEnv(t, m3lEnvToken))),
		adminKey: config.Secret(strings.TrimSpace(requireEnv(t, m3lEnvAdminKey))),
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a credential must never be handed to a proxy
	tr.DisableKeepAlives = true
	e.hc = &http.Client{
		Timeout:       15 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e
}

// scrub removes both credentials from any text that is about to be printed.
func (e *m3lEnv) scrub(s string) string {
	for _, secret := range []string{e.token.Reveal(), e.adminKey.Reveal()} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// snip is a response body shortened to 500 bytes and scrubbed: what a failure message may quote.
func (e *m3lEnv) snip(b []byte) string {
	s := string(b)
	if len(s) > 500 {
		s = s[:500] + "...(truncated)"
	}
	return e.scrub(s)
}

// do makes one request and returns the status and the body (at most 16 MiB). key is sent as a Bearer credential when it is not empty. body, when not nil, is sent as JSON. A transport failure FAILS the test
// (a GET is tried three times first: the answer to a read cannot change anything). The credential is never in the URL, so it is in no error text; scrub is applied anyway.
func (e *m3lEnv) do(t *testing.T, base, method, path string, key config.Secret, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("%s %s: cannot encode the request body: %v", method, m3lPathOnly(path), err)
		}
		payload = b
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-e.ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(e.ctx, method, base+path, rdr)
		if err != nil {
			t.Fatalf("%s %s: cannot build the request: %s", method, m3lPathOnly(path), e.scrub(err.Error()))
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key.Reveal())
		}
		resp, err := e.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		return resp.StatusCode, b
	}
	t.Fatalf("%s %s: transport error: %s", method, m3lPathOnly(path), e.scrub(lastErr.Error()))
	return 0, nil
}

func m3lPathOnly(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

// m3lErrCode is the `error.code` of module 3's error envelope {"error":{"code","message","details"?,"request_id"}}, "" when the body is not one.
func m3lErrCode(body []byte) string {
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// expect fails unless status is want and (when wantCode is not empty) the error envelope carries wantCode. The message names the status AND the code module 3 answered with.
func (e *m3lEnv) expect(t *testing.T, what string, status int, body []byte, want int, wantCode string) {
	t.Helper()
	code := m3lErrCode(body)
	if status != want || (wantCode != "" && code != wantCode) {
		t.Fatalf("%s: module 3 answered HTTP %d with error code %q, want HTTP %d%s\n  body: %s", what, status, code, want, m3lWantCode(wantCode), e.snip(body))
	}
}

func m3lWantCode(code string) string {
	if code == "" {
		return ""
	}
	return fmt.Sprintf(" with error code %q", code)
}

// ---- the sink (Mailpit) ----

type m3lSinkAddr struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

// m3lSinkMsg is the part of a Mailpit message summary this test reads. The field names are INFERRED for Mailpit v1.33.1 (Go's decoder ignores case); sinkMessages fails with the raw JSON when the shape surprises it.
type m3lSinkMsg struct {
	ID  string        `json:"ID"`
	To  []m3lSinkAddr `json:"To"`
	Cc  []m3lSinkAddr `json:"Cc"`
	Bcc []m3lSinkAddr `json:"Bcc"`
}

// addresses is every recipient address of the message (To, Cc, Bcc), lower-cased, each once.
func (m m3lSinkMsg) addresses() map[string]bool {
	out := map[string]bool{}
	for _, list := range [][]m3lSinkAddr{m.To, m.Cc, m.Bcc} {
		for _, a := range list {
			if s := strings.ToLower(strings.TrimSpace(a.Address)); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// sinkMessages lists every message in the sink (it is cumulative), paging with start and limit=500 and dropping a message seen twice (a message arriving between two pages shifts the offsets). On any decode surprise it FAILS
// with the raw JSON truncated to 500 bytes.
func (e *m3lEnv) sinkMessages(t *testing.T) []m3lSinkMsg {
	t.Helper()
	var out []m3lSinkMsg
	seen := map[string]bool{}
	want := -1
	for start := 0; ; {
		status, body := e.do(t, e.sinkURL, http.MethodGet, fmt.Sprintf("/api/v1/messages?start=%d&limit=%d", start, m3lSinkPageSize), "", nil)
		if status != http.StatusOK {
			t.Fatalf("the sink answered HTTP %d to GET /api/v1/messages (is HARNESS_SINK_URL the sink's HTTP API?): %s", status, e.snip(body))
		}
		var pg struct {
			Total         *int            `json:"total"`
			MessagesCount *int            `json:"messages_count"`
			Messages      json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &pg); err != nil {
			t.Fatalf("the sink's message list is not the JSON this test decodes (%v): %s", err, e.snip(body))
		}
		if want < 0 {
			switch {
			case pg.Total != nil:
				want = *pg.Total
			case pg.MessagesCount != nil:
				want = *pg.MessagesCount
			}
		}
		raw := bytes.TrimSpace(pg.Messages)
		var page []m3lSinkMsg
		switch {
		case len(raw) == 0:
			t.Fatalf("the sink's message list has no \"messages\" field (the field names are inferred for Mailpit v1.33.1): %s", e.snip(body))
		case string(raw) == "null":
			if want > 0 {
				t.Fatalf("the sink reports %d message(s) but its \"messages\" is null: %s", want, e.snip(body))
			}
		default:
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatalf("the sink's \"messages\" is not a list of messages (%v): %s", err, e.snip(body))
			}
		}
		if len(page) > 0 {
			addressed := false
			for _, m := range page {
				if len(m.addresses()) > 0 {
					addressed = true
					break
				}
			}
			if !addressed {
				t.Fatalf("the sink lists %d message(s) but not one carries a To, Cc or Bcc address (the field names To/Cc/Bcc/Address are inferred): %s", len(page), e.snip(body))
			}
		}
		for _, m := range page {
			if m.ID != "" {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
			}
			out = append(out, m)
		}
		start += len(page)
		switch {
		case len(page) == 0, want >= 0 && start >= want, want < 0 && len(page) < m3lSinkPageSize:
			return out
		case start > 100000:
			t.Fatalf("the sink listing did not end after %d messages", start)
		}
	}
}

// ---- tenants and the run ----

// m3lTenant is one customer of the test: a user, one campaign, its job items and the recipients they send to.
type m3lTenant struct {
	Name       string // "u1" (the entitlement under test) or "u2" (the control, never suspended)
	UserID     string
	CampaignID string
	Prefix     string // the local-part prefix of every recipient: "<name>-<run id>-"
	ItemIDs    []string
	Recipients []string // Recipients[i] is the recipient of ItemIDs[i]; distinct
	AccountID  string
}

// m3lRun is the state shared by the phases of one test.
type m3lRun struct {
	ctx          context.Context
	env          *m3lEnv
	db           *pgx.Conn // a SUPERUSER, read-write: seeds and observes, never the code under test
	pool         *pgxpool.Pool
	store        *entitlements.Store
	runID        string
	admin        string // an active admin's id (platform.admins)
	u1, u2       *m3lTenant
	sinkBaseline int // messages in the (cumulative) sink before this test sent anything

	// Evidence carried from one phase to the next. Every instant is the DATABASE's clock.
	redatedUntil  time.Time // the enabled-until date the admin set on U1 to make it expire
	expireAt      time.Time // created_at of U1's m3_expire ledger row (written by the service's expiry loop)
	observedAt    time.Time // the database clock when this test first saw U1's entitlement disabled
	k0            int       // U1's messages in the sink at that moment
	suspendedAt   time.Time // when module 3 recorded the suspension (send_byo.user_suspensions.suspended_at)
	expiryVersion int64     // U1's want_version after the expiry
	s1            int       // U1's messages in the sink once nothing of U1 was in flight
}

// m3lAppPool opens a pool of real logins as platform_app on the live database: the identity the platform service has, so the store runs with exactly its privileges.
func m3lAppPool(t *testing.T, adminURL, password string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("cannot parse %s: %s", envAdminURL, scrubbed(err, adminURL))
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "platform_app", password
	delete(cfg.ConnConfig.RuntimeParams, "timezone") // nothing from the test process may override the role's settings
	delete(cfg.ConnConfig.RuntimeParams, "options")
	cfg.ConnConfig.RuntimeParams["application_name"] = "m3-live-test"
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open the platform_app pool: %s", scrubbed(err, adminURL))
	}
	t.Cleanup(pool.Close)
	var who string
	var super bool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, "SELECT session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&who, &super); err != nil {
		t.Fatalf("the platform_app pool cannot run a query: %s", scrubbed(err, adminURL))
	}
	if who != "platform_app" || super {
		t.Fatalf("the pool logged in as %q (superuser %v), want a real login as platform_app and no superuser: the store would not run with the service's privileges", who, super)
	}
	return pool
}

func m3lNewRun(t *testing.T, ctx context.Context) *m3lRun {
	t.Helper()
	adminURL := requireEnv(t, envAdminURL)
	password := requireEnv(t, envAppPassword)
	env := m3lNewEnv(t, ctx)
	db := dial(t, adminURL, func(cfg *pgx.ConnConfig) { cfg.RuntimeParams["application_name"] = "m3-live-test-superuser" })
	pool := m3lAppPool(t, adminURL, password)
	return &m3lRun{ctx: ctx, env: env, db: db, pool: pool, store: entitlements.NewStore(pool), runID: randomHex(t, 4)}
}

// ---- the database, as the superuser ----

// dbNow is the DATABASE's clock, the only clock the test's evidence uses.
func (r *m3lRun) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := r.db.QueryRow(r.ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("read the database clock: %v", err)
	}
	return now.UTC()
}

func (r *m3lRun) uuid(t *testing.T) string {
	t.Helper()
	var id string
	if err := r.db.QueryRow(r.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

// count runs a query that returns one integer.
func (r *m3lRun) count(t *testing.T, what, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(r.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v\n  sql: %s", what, err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

// requireColumns fails, naming what is missing, unless schema.table exists and has every column. A table or column that is not there is a wrong assumption about module 3's schema, and it is far better found in the
// first second than after eight minutes of sending.
func (r *m3lRun) requireColumns(t *testing.T, schema, table string, cols ...string) {
	t.Helper()
	rows, err := r.db.Query(r.ctx, "SELECT column_name::text FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2", schema, table)
	if err != nil {
		t.Fatalf("read the columns of %s.%s: %v", schema, table, err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		have[c] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(have) == 0 {
		trows, err := r.db.Query(r.ctx, "SELECT table_name::text FROM information_schema.tables WHERE table_schema = $1 ORDER BY 1", schema)
		if err != nil {
			t.Fatalf("list the tables of schema %s: %v", schema, err)
		}
		var tables []string
		for trows.Next() {
			var n string
			if err := trows.Scan(&n); err != nil {
				trows.Close()
				t.Fatal(err)
			}
			tables = append(tables, n)
		}
		trows.Close()
		t.Fatalf("%s.%s does not exist: this test reads it and its name is inferred from module 3's migrations; the tables in schema %s are: %s", schema, table, schema, strings.Join(tables, ", "))
	}
	var missing []string
	for _, c := range cols {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		var got []string
		for c := range have {
			got = append(got, c)
		}
		sort.Strings(got)
		t.Fatalf("%s.%s lacks column(s) %s (it has: %s): this test's reads are inferred from module 3's migrations", schema, table, strings.Join(missing, ", "), strings.Join(got, ", "))
	}
}

type m3lEntRow struct {
	Exists    bool
	Enabled   bool
	Until     *time.Time
	ChangedBy *string
	DBNow     time.Time // the database clock when the row was read
}

// entRow reads platform.m3_entitlements as the superuser (a path independent of the store's own Get) with the database clock of the same instant.
func (r *m3lRun) entRow(t *testing.T, user string) m3lEntRow {
	t.Helper()
	var row m3lEntRow
	err := r.db.QueryRow(r.ctx, "SELECT enabled, enabled_until, changed_by::text, clock_timestamp() FROM platform.m3_entitlements WHERE user_id = $1::uuid", user).
		Scan(&row.Enabled, &row.Until, &row.ChangedBy, &row.DBNow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row.DBNow = r.dbNow(t)
		return row
	case err != nil:
		t.Fatalf("read platform.m3_entitlements for %s: %v", user, err)
	}
	row.Exists = true
	row.DBNow = row.DBNow.UTC()
	if row.Until != nil {
		u := row.Until.UTC()
		row.Until = &u
	}
	return row
}

type m3lSusp struct {
	Exists       bool
	Suspended    bool
	SuspendedAt  *time.Time
	ReinstatedAt *time.Time
	Reason       *string
}

// suspension reads module 3's own record of a user's suspension (send_byo.user_suspensions: no other role may read it, the superuser can). A missing row means "not suspended".
func (r *m3lRun) suspension(t *testing.T, user string) m3lSusp {
	t.Helper()
	var s m3lSusp
	err := r.db.QueryRow(r.ctx, "SELECT suspended, suspended_at, reinstated_at, reason::text FROM send_byo.user_suspensions WHERE user_id = $1::uuid", user).
		Scan(&s.Suspended, &s.SuspendedAt, &s.ReinstatedAt, &s.Reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s
	case err != nil:
		t.Fatalf("read send_byo.user_suspensions for %s: %v", user, err)
	}
	s.Exists = true
	for _, p := range []**time.Time{&s.SuspendedAt, &s.ReinstatedAt} {
		if *p != nil {
			u := (**p).UTC()
			*p = &u
		}
	}
	return s
}

// ---- the entitlement, through the real store ----

// mustEnable enables (or re-dates) a tenant's entitlement through entitlements.Store (the admin's write path) with a key of its own and requires that something was written.
func (r *m3lRun) mustEnable(t *testing.T, tn *m3lTenant, until *time.Time, step string) entitlements.Entry {
	t.Helper()
	en, err := r.store.Enable(r.ctx, entitlements.EnableReq{
		UserID:  tn.UserID,
		Until:   until,
		AdminID: r.admin,
		Reason:  "m3 live harness: " + step,
		Key:     fmt.Sprintf("m3live-%s-%s-%s", r.runID, tn.Name, step),
	})
	if err != nil {
		t.Fatalf("Store.Enable(%s, %s): %v", tn.Name, step, err)
	}
	if en.Replayed || en.Kind != "m3_enable" {
		t.Fatalf("Store.Enable(%s, %s) returned %+v: want a fresh m3_enable ledger row", tn.Name, step, en)
	}
	return en
}

func (r *m3lRun) syncString(st entitlements.SyncState) string {
	last := "-"
	if st.LastError != nil {
		last = r.env.scrub(*st.LastError)
	}
	return fmt.Sprintf("exists=%v want_suspended=%v want_version=%d converged_version=%d observed=%s observed_version=%s m3_version=%s attempts=%d last_error=%q",
		st.Exists, st.WantSuspended, st.WantVersion, st.ConvergedVersion, st.Observed, entObsStr(st.ObservedVersion), entObsStr(st.M3Version), st.Attempts, last)
}

// waitSync waits (bounded) until the SERVICE's reconcile loop has delivered the wanted state to the real module 3 and recorded what module 3 answered: the delivery row is at wantVersion, wants wantSuspended, is
// converged (Sync(...).Converged(): converged_version = want_version and observed is the wanted state) and observed is the given state.
func (r *m3lRun) waitSync(t *testing.T, tn *m3lTenant, wantVersion int64, wantSuspended bool, observed string, within time.Duration) entitlements.SyncState {
	t.Helper()
	var got entitlements.SyncState
	m3lWait(t, r.ctx, fmt.Sprintf("the platform service's reconcile loop converges %s at want_version %d (observed %s)", tn.Name, wantVersion, observed), within, 500*time.Millisecond, func() (bool, string) {
		st, err := r.store.Sync(r.ctx, tn.UserID)
		if err != nil {
			t.Fatalf("Store.Sync(%s): %v", tn.Name, err)
		}
		got = st
		return st.Exists && st.WantVersion == wantVersion && st.WantSuspended == wantSuspended && st.Converged() && st.Observed == observed && st.ObservedAt != nil, r.syncString(st)
	})
	return got
}

// ---- waiting ----

// m3lWait polls check every `every` until it returns true; check FAILS the test itself on a hard error and returns, with false, a description of what it saw. A wait that is not over within `within` fails the test naming
// the last observation. Nothing sleeps blindly: every wait ends at its deadline.
func m3lWait(t *testing.T, ctx context.Context, what string, within, every time.Duration, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, seen := check()
		if ok {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s: not reached within %s; last observation: %s", what, within, seen)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: the test's overall time budget ended first; last observation: %s", what, seen)
		case <-time.After(every):
		}
	}
}

// ---- the sink, as a view of this run's recipients ----

type m3lSinkView struct {
	Counts map[string]int // this run's recipients (exact lower-cased address) -> copies in the sink
	Total  int            // every message in the (cumulative) sink
}

// of is the number of messages in the sink to the tenant's recipients.
func (v m3lSinkView) of(tn *m3lTenant) int {
	n := 0
	for _, rc := range tn.Recipients {
		n += v.Counts[rc]
	}
	return n
}

// sinkView reads the sink once. It FAILS on what can never be true at any instant: a recipient of this run (matched by exact address, never by substring: the sink's search is fuzzy) with more than one copy (a duplicate
// send), or an address carrying this run's prefix that was never one of its recipients.
func (r *m3lRun) sinkView(t *testing.T) m3lSinkView {
	t.Helper()
	msgs := r.env.sinkMessages(t)
	v := m3lSinkView{Counts: map[string]int{}, Total: len(msgs)}
	for _, m := range msgs {
		for a := range m.addresses() {
			if strings.HasPrefix(a, r.u1.Prefix) || strings.HasPrefix(a, r.u2.Prefix) {
				v.Counts[a]++
			}
		}
	}
	known := map[string]bool{}
	for _, tn := range []*m3lTenant{r.u1, r.u2} {
		for _, rc := range tn.Recipients {
			known[rc] = true
		}
	}
	for a, n := range v.Counts {
		if !known[a] {
			t.Fatalf("the sink holds a message to %s: it carries this run's prefix but was never one of this run's recipients", a)
		}
		if n > 1 {
			t.Fatalf("the sink holds %d copies of the message to %s: a duplicate send (exactly-once is violated)", n, a)
		}
	}
	return v
}

// ---- module 3's campaign report ----

type m3lTotals struct {
	Total, Sent, Bounced, Failed, InFlight, Pending, Waiting int
}

// Queued is every item still queued: dispatchable now (pending) plus those waiting for an eligible account.
func (x m3lTotals) Queued() int { return x.Pending + x.Waiting }

func (x m3lTotals) String() string {
	return fmt.Sprintf("total=%d sent=%d bounced=%d failed=%d in_flight=%d pending=%d waiting_for_capacity=%d", x.Total, x.Sent, x.Bounced, x.Failed, x.InFlight, x.Pending, x.Waiting)
}

type m3lReportItem struct {
	JobItemID string `json:"job_item_id"`
	State     string `json:"state"`
}

type m3lReport struct {
	Totals         m3lTotals
	DispatchPaused bool
	Terminal       int
	IsComplete     bool
	Items          []m3lReportItem
	NextCursor     string
}

// report reads GET /v1/report for the tenant's campaign with the service token. A field of the documented shape that is absent FAILS with the body (an absent field must not read as zero).
func (r *m3lRun) report(t *testing.T, tn *m3lTenant, pageSize int) m3lReport {
	t.Helper()
	path := fmt.Sprintf("/v1/report?campaign_id=%s&user_id=%s&page_size=%d", tn.CampaignID, tn.UserID, pageSize)
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, path, r.env.token, nil)
	r.env.expect(t, "GET /v1/report for "+tn.Name, status, body, http.StatusOK, "")
	return r.parseReport(t, tn.Name, body)
}

func (r *m3lRun) parseReport(t *testing.T, who string, body []byte) m3lReport {
	t.Helper()
	var raw struct {
		DispatchPaused *bool           `json:"dispatch_paused"`
		Totals         map[string]*int `json:"totals"`
		Progress       *struct {
			Terminal   *int  `json:"terminal"`
			IsComplete *bool `json:"is_complete"`
		} `json:"progress"`
		Items *struct {
			Results    []m3lReportItem `json:"results"`
			NextCursor *string         `json:"next_cursor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the report of %s is not JSON (%v): %s", who, err, r.env.snip(body))
	}
	var rep m3lReport
	field := func(name string) int {
		p, ok := raw.Totals[name]
		if !ok || p == nil {
			t.Fatalf("the report of %s has no totals.%s (the documented shape): %s", who, name, r.env.snip(body))
		}
		return *p
	}
	rep.Totals = m3lTotals{Total: field("total"), Sent: field("sent"), Bounced: field("bounced"), Failed: field("failed"), InFlight: field("in_flight"), Pending: field("pending")}
	if p := raw.Totals["waiting_for_capacity"]; p != nil {
		rep.Totals.Waiting = *p
	}
	if raw.Progress == nil || raw.Progress.Terminal == nil || raw.Progress.IsComplete == nil {
		t.Fatalf("the report of %s has no progress.terminal / progress.is_complete (the documented shape): %s", who, r.env.snip(body))
	}
	rep.Terminal, rep.IsComplete = *raw.Progress.Terminal, *raw.Progress.IsComplete
	if raw.DispatchPaused == nil {
		t.Fatalf("the report of %s has no dispatch_paused (the documented shape): %s", who, r.env.snip(body))
	}
	rep.DispatchPaused = *raw.DispatchPaused
	if raw.Items != nil {
		rep.Items = raw.Items.Results
		if raw.Items.NextCursor != nil {
			rep.NextCursor = *raw.Items.NextCursor
		}
	}
	return rep
}

// m3lSinkPassword is the password the live test gives module 3 for the SMTP sink's account. The sink (Mailpit, MP_SMTP_AUTH_ACCEPT_ANY) accepts ANY credential, so nothing is protected by it, but no
// literal of that shape may live in the tree (a secret scanner cannot tell a placeholder from a secret, and an exception for one would be a hole): run.sh mints a fresh random value per run and hands
// it to the test in HARNESS_SINK_PASSWORD. A missing value FAILS the test (rule 1: a test that cannot run fails, it never skips).
func m3lSinkPassword(t *testing.T) string {
	t.Helper()
	return requireEnv(t, "HARNESS_SINK_PASSWORD")
}
