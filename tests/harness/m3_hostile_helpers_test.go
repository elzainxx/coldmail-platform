//go:build harness

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Helpers of TestM3Live_SeveralLanesAndAHostileSink (m3_hostile_test.go). Every name starts with `m3h` so it cannot clash with another file's helpers. The test is the sibling of
// TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes: it reuses that file's HTTP client (m3lEnv: do, expect, snip), its waiting loop (m3lWait), its instants (m3lT) and its tenant record
// (m3lTenant), and it talks to the same real module 3 (control and workers); the sink is the CONTROLLABLE one (tests/harness/sink), reached by its control API.

const (
	m3hEnvCtlURL    = "HARNESS_CTL_URL"       // the controllable sink's control API (http://smtp-ctl:8025)
	m3hEnvWorkerIDs = "HARNESS_M3_WORKER_IDS" // the ids of module 3's three workers, comma-separated
	m3hCtlHost      = "smtp-ctl"              // the sink's service name: resolved to its private address, which is what the SMTP account names
	m3hSMTPPort     = 1025
)

// ---- the sink's control API ----

// m3hMode is the body of POST /api/mode. The sink REPLACES its whole mode with what it is sent, so a zero m3hMode is "no hostility at all".
type m3hMode struct {
	RejectRcpt      []string `json:"reject_rcpt"`
	RejectCode      int      `json:"reject_code,omitempty"`
	DeferRcpt       []string `json:"defer_rcpt"`
	DataDelayMs     int      `json:"data_delay_ms"`
	DelayRecipients []string `json:"delay_recipients"`
	FailAuth        bool     `json:"fail_auth"`
}

type m3hSinkMsg struct {
	Seq       int64    `json:"seq"`
	At        string   `json:"at"`
	To        []string `json:"to"`
	From      string   `json:"from"`
	Subject   string   `json:"subject"`
	MessageID string   `json:"message_id"`
	HeldMs    int64    `json:"held_ms"`
}

type m3hHeld struct {
	To          []string `json:"to"`
	AgeMs       int64    `json:"age_ms"`
	RemainingMs int64    `json:"remaining_ms"`
}

type m3hInflight struct {
	Inflight  int       `json:"inflight"`
	Peak      int       `json:"peak"`
	HeldTotal int       `json:"held_total"`
	Held      []m3hHeld `json:"held"`
}

type m3hRcpt struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
}

// ctl is one call to the sink's control API; a transport failure or an unexpected status FAILS the test with the sink's own words.
func (r *m3hRun) ctl(t *testing.T, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	status, out := r.env.do(t, r.ctlURL, method, path, "", body)
	if status != wantStatus {
		t.Fatalf("the controllable sink answered HTTP %d to %s %s, want %d: %s", status, method, path, wantStatus, r.env.snip(out))
	}
	return out
}

// setMode replaces the sink's mode and returns what the sink says it now is.
func (r *m3hRun) setMode(t *testing.T, m m3hMode) {
	t.Helper()
	if m.RejectRcpt == nil {
		m.RejectRcpt = []string{}
	}
	if m.DeferRcpt == nil {
		m.DeferRcpt = []string{}
	}
	if m.DelayRecipients == nil {
		m.DelayRecipients = []string{}
	}
	out := r.ctl(t, http.MethodPost, "/api/mode", m, http.StatusOK)
	var got struct {
		RejectRcpt      []string `json:"reject_rcpt"`
		RejectCode      int      `json:"reject_code"`
		DeferRcpt       []string `json:"defer_rcpt"`
		DataDelayMs     int      `json:"data_delay_ms"`
		DelayRecipients []string `json:"delay_recipients"`
		FailAuth        bool     `json:"fail_auth"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the sink's answer to POST /api/mode is not a mode (%v): %s", err, r.env.snip(out))
	}
	wantCode := m.RejectCode
	if wantCode == 0 {
		wantCode = 550
	}
	if len(got.RejectRcpt) != len(m.RejectRcpt) || len(got.DeferRcpt) != len(m.DeferRcpt) || got.DataDelayMs != m.DataDelayMs || len(got.DelayRecipients) != len(m.DelayRecipients) || got.FailAuth != m.FailAuth || got.RejectCode != wantCode {
		t.Fatalf("the sink did not take the mode it was sent: sent %d reject / %d defer / delay %d ms / %d delay recipient(s) / fail_auth %v / code %d, the sink says %s", len(m.RejectRcpt), len(m.DeferRcpt), m.DataDelayMs, len(m.DelayRecipients), m.FailAuth, wantCode, r.env.snip(out))
	}
}

func (r *m3hRun) sinkMessages(t *testing.T) []m3hSinkMsg {
	t.Helper()
	out := r.ctl(t, http.MethodGet, "/api/messages", nil, http.StatusOK)
	var msgs []m3hSinkMsg
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("the sink's message list is not a JSON list of messages (%v): %s", err, r.env.snip(out))
	}
	return msgs
}

func (r *m3hRun) inflight(t *testing.T) m3hInflight {
	t.Helper()
	out := r.ctl(t, http.MethodGet, "/api/inflight", nil, http.StatusOK)
	var v m3hInflight
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("the sink's /api/inflight is not the documented shape (%v): %s", err, r.env.snip(out))
	}
	return v
}

func (r *m3hRun) rcptCounts(t *testing.T) map[string]m3hRcpt {
	t.Helper()
	out := r.ctl(t, http.MethodGet, "/api/rcpt", nil, http.StatusOK)
	var v map[string]m3hRcpt
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("the sink's /api/rcpt is not a map of counters (%v): %s", err, r.env.snip(out))
	}
	return v
}

// restoreSink puts the sink back to its default mode when the test ends, pass or fail: a delay left on would hold every later send to it. It never fails the test (it runs in a cleanup).
func (r *m3hRun) restoreSink() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ctlURL+"/api/reset", nil)
	if err != nil {
		return
	}
	if resp, err := r.env.hc.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// ---- the sink, as a view of this run's recipients ----

type m3hView struct {
	Counts map[string]int // this run's recipients (exact lower-cased address) -> messages in the sink
	Msgs   []m3hSinkMsg   // this run's messages
}

func (v m3hView) of(tn *m3lTenant) int {
	n := 0
	for _, rc := range tn.Recipients {
		n += v.Counts[rc]
	}
	return n
}

// sinkView reads the sink once and FAILS on what can never be true at any instant: a recipient of this run with more than one message (a duplicate send), an address carrying a prefix of this run that
// was never one of its recipients, a message of this run without a Message-ID, or a Message-ID seen twice (two messages for one job).
func (r *m3hRun) sinkView(t *testing.T) m3hView {
	t.Helper()
	v := m3hView{Counts: map[string]int{}}
	known := map[string]bool{}
	for _, tn := range r.tenants {
		for _, rc := range tn.Recipients {
			known[rc] = true
		}
	}
	ids := map[string]string{}
	for _, m := range r.sinkMessages(t) {
		mine := false
		for _, raw := range m.To {
			a := strings.ToLower(strings.TrimSpace(raw))
			for _, tn := range r.tenants {
				if strings.HasPrefix(a, tn.Prefix) {
					mine = true
					if !known[a] {
						t.Fatalf("the sink holds a message to %s: it carries a prefix of this run but was never one of its recipients", a)
					}
					v.Counts[a]++
					if v.Counts[a] > 1 {
						t.Fatalf("the sink holds %d messages to %s: a duplicate send (exactly-once is violated)", v.Counts[a], a)
					}
				}
			}
		}
		if !mine {
			continue
		}
		if m.MessageID == "" {
			t.Fatalf("the sink holds a message of this run without a Message-ID (to %v, subject %q): module 3 builds one for every send", m.To, m.Subject)
		}
		if prev, dup := ids[m.MessageID]; dup {
			t.Fatalf("the sink holds two messages with the Message-ID %s (to %s and to %v): one job was sent twice", m.MessageID, prev, m.To)
		}
		ids[m.MessageID] = strings.Join(m.To, ",")
		v.Msgs = append(v.Msgs, m)
	}
	return v
}

// ---- the run ----

type m3hRun struct {
	ctx        context.Context
	env        *m3lEnv
	ctlURL     string
	db         *pgx.Conn // a SUPERUSER, read-write: seeds and observes, never the code under test
	runID      string
	sinkIP     string   // the controllable sink's private address: what the SMTP accounts name (the worker skips certificate validation only for private addresses)
	workerIDs  []string // module 3's three workers
	admin      string   // an active platform admin (for the rolled-back ledger control of the last phase)
	a, b, c, d *m3lTenant
	tenants    []*m3lTenant
	jobIDs     map[string][]string // tenant name -> the job ids POST /v1/run answered, in item order
	seen       map[string]int      // every item state module 3's reports showed, over the whole test
}

func m3hNewRun(t *testing.T, ctx context.Context) *m3hRun {
	t.Helper()
	adminURL := requireEnv(t, envAdminURL)
	env := m3lNewEnv(t, ctx)
	r := &m3hRun{
		ctx:    ctx,
		env:    env,
		ctlURL: m3lBaseURL(t, m3hEnvCtlURL),
		db:     dial(t, adminURL, func(cfg *pgx.ConnConfig) { cfg.RuntimeParams["application_name"] = "m3-hostile-test" }),
		runID:  randomHex(t, 4),
		jobIDs: map[string][]string{},
		seen:   map[string]int{},
	}
	for _, id := range strings.Split(requireEnv(t, m3hEnvWorkerIDs), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			r.workerIDs = append(r.workerIDs, id)
		}
	}
	if len(r.workerIDs) != 3 {
		t.Fatalf("%s names %d worker(s), want module 3's three workers", m3hEnvWorkerIDs, len(r.workerIDs))
	}
	t.Cleanup(r.restoreSink)
	return r
}

func (r *m3hRun) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := r.db.QueryRow(r.ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("read the database clock: %v", err)
	}
	return now.UTC()
}

func (r *m3hRun) uuid(t *testing.T) string {
	t.Helper()
	var id string
	if err := r.db.QueryRow(r.ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

func (r *m3hRun) count(t *testing.T, what, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(r.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v\n  sql: %s", what, err, strings.Join(strings.Fields(sql), " "))
	}
	return n
}

// requireColumns fails, naming what is missing, unless schema.table exists and has every column: every table and column this test reads is inferred from module 3's migrations, and a wrong guess
// is far better found in the first second than after minutes of sending.
func (r *m3hRun) requireColumns(t *testing.T, schema, table string, cols ...string) {
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
		t.Fatalf("%s.%s does not exist: this test reads it and its name is inferred from the migrations", schema, table)
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
		t.Fatalf("%s.%s lacks column(s) %s (it has: %s): this test's reads are inferred from the migrations", schema, table, strings.Join(missing, ", "), strings.Join(got, ", "))
	}
}

// m3hPrivateIPv4 is true for the three private ranges module 3's worker treats as "a test/sink target on a private network" (and only for them skips certificate validation): 10/8, 172.16/12, 192.168/16.
func m3hPrivateIPv4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168)
}

// ---- items: POST /v1/run ----

func (r *m3hRun) run(t *testing.T, tn *m3lTenant, itemID string) {
	t.Helper()
	status, body := r.env.do(t, r.env.m3URL, http.MethodPost, "/v1/run", r.env.token, map[string]string{"job_item_id": itemID, "user_id": tn.UserID})
	r.env.expect(t, fmt.Sprintf("POST /v1/run for a job item of %s", tn.Name), status, body, http.StatusAccepted, "")
	var ok struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || ok.JobID == "" || ok.State != "queued" {
		t.Fatalf("POST /v1/run for a job item of %s answered 202 with %s, want {job_id, state: queued}", tn.Name, r.env.snip(body))
	}
	r.jobIDs[tn.Name] = append(r.jobIDs[tn.Name], ok.JobID)
}

// runAll accepts every item of the tenant (the user must not be suspended: a suspended user's /v1/run is refused) and returns how long that took on the database clock.
func (r *m3hRun) runAll(t *testing.T, tn *m3lTenant) time.Duration {
	t.Helper()
	start := r.dbNow(t)
	for _, id := range tn.ItemIDs {
		r.run(t, tn, id)
	}
	return r.dbNow(t).Sub(start)
}

// ---- module 3's campaign report ----

type m3hItem struct {
	JobItemID         string  `json:"job_item_id"`
	State             string  `json:"state"`
	ProviderMessageID *string `json:"provider_message_id"`
	AttemptCount      int     `json:"attempt_count"`
	SMTPResponse      *string `json:"smtp_response"`
	ErrorClass        *string `json:"error_class"`
}

func (it m3hItem) class() string {
	if it.ErrorClass == nil {
		return ""
	}
	return *it.ErrorClass
}

func (it m3hItem) response() string {
	if it.SMTPResponse == nil {
		return ""
	}
	return *it.SMTPResponse
}

type m3hReport struct {
	Totals         m3lTotals
	DispatchPaused bool
	IsComplete     bool
	Items          []m3hItem
	NextCursor     string
	ByID           map[string]m3hItem
}

// report reads GET /v1/report for the tenant's campaign with the service token (one page; every campaign of this test has at most 200 items) and records every state it shows. A field of the
// documented shape that is absent FAILS with the body (an absent field must not read as zero).
func (r *m3hRun) report(t *testing.T, tn *m3lTenant) m3hReport {
	t.Helper()
	path := fmt.Sprintf("/v1/report?campaign_id=%s&user_id=%s&page_size=200", tn.CampaignID, tn.UserID)
	status, body := r.env.do(t, r.env.m3URL, http.MethodGet, path, r.env.token, nil)
	r.env.expect(t, "GET /v1/report for "+tn.Name, status, body, http.StatusOK, "")
	var raw struct {
		DispatchPaused *bool           `json:"dispatch_paused"`
		Totals         map[string]*int `json:"totals"`
		Progress       *struct {
			IsComplete *bool `json:"is_complete"`
		} `json:"progress"`
		Items *struct {
			Results    []m3hItem `json:"results"`
			NextCursor *string   `json:"next_cursor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the report of %s is not JSON (%v): %s", tn.Name, err, r.env.snip(body))
	}
	field := func(name string) int {
		p, ok := raw.Totals[name]
		if !ok || p == nil {
			t.Fatalf("the report of %s has no totals.%s (the documented shape): %s", tn.Name, name, r.env.snip(body))
		}
		return *p
	}
	rep := m3hReport{ByID: map[string]m3hItem{}}
	rep.Totals = m3lTotals{Total: field("total"), Sent: field("sent"), Bounced: field("bounced"), Failed: field("failed"), InFlight: field("in_flight"), Pending: field("pending")}
	if p := raw.Totals["waiting_for_capacity"]; p != nil {
		rep.Totals.Waiting = *p
	}
	if raw.Progress == nil || raw.Progress.IsComplete == nil || raw.DispatchPaused == nil || raw.Items == nil {
		t.Fatalf("the report of %s lacks progress.is_complete, dispatch_paused or items (the documented shape): %s", tn.Name, r.env.snip(body))
	}
	rep.IsComplete, rep.DispatchPaused, rep.Items = *raw.Progress.IsComplete, *raw.DispatchPaused, raw.Items.Results
	if raw.Items.NextCursor != nil {
		rep.NextCursor = *raw.Items.NextCursor
	}
	if rep.NextCursor != "" || len(rep.Items) > 200 {
		t.Fatalf("the report of %s does not fit one page (next_cursor %q, %d items): this test reads totals and items from one page", tn.Name, rep.NextCursor, len(rep.Items))
	}
	for _, it := range rep.Items {
		rep.ByID[it.JobItemID] = it
		r.seen[it.State]++
	}
	if len(rep.ByID) != len(rep.Items) {
		t.Fatalf("the report of %s lists %d items but only %d distinct job item ids: %s", tn.Name, len(rep.Items), len(rep.ByID), r.env.snip(body))
	}
	if rep.DispatchPaused {
		t.Fatalf("the report of %s says dispatch_paused: the global rung is paused, so nothing this test proves can be attributed to the sink or to the user", tn.Name)
	}
	return rep
}

// mustNotFail fails the moment a campaign shows a failed item: no phase of this test may end in module 3 giving up on an item.
func (r *m3hRun) mustNotFail(t *testing.T, tn *m3lTenant, rep m3hReport) {
	t.Helper()
	if rep.Totals.Failed > 0 {
		var failed []string
		for _, it := range rep.Items {
			if it.State == "failed" {
				failed = append(failed, fmt.Sprintf("%s (class %q, response %q)", it.JobItemID, it.class(), it.response()))
			}
		}
		t.Fatalf("%s's campaign shows %d failed item(s): %s; totals %s", tn.Name, rep.Totals.Failed, strings.Join(failed, "; "), rep.Totals)
	}
}

// ---- module 3's own records ----

type m3hSusp struct {
	Exists       bool
	Suspended    bool
	SuspendedAt  *time.Time
	ReinstatedAt *time.Time
}

func (r *m3hRun) suspension(t *testing.T, user string) m3hSusp {
	t.Helper()
	var s m3hSusp
	err := r.db.QueryRow(r.ctx, "SELECT suspended, suspended_at, reinstated_at FROM send_byo.user_suspensions WHERE user_id = $1::uuid", user).Scan(&s.Suspended, &s.SuspendedAt, &s.ReinstatedAt)
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

// jobIDsOf is module 3's job id for every job item of the tenant (send_byo.jobs), keyed by job item id.
func (r *m3hRun) jobIDsOf(t *testing.T, tn *m3lTenant) map[string]string {
	t.Helper()
	rows, err := r.db.Query(r.ctx, "SELECT job_item_id::text, id::text FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid", tn.UserID, tn.CampaignID)
	if err != nil {
		t.Fatalf("read %s's jobs: %v", tn.Name, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var item, job string
		if err := rows.Scan(&item, &job); err != nil {
			t.Fatal(err)
		}
		out[item] = job
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// jobStates is the number of the tenant's jobs in each state, from send_byo.jobs (independent of the report).
func (r *m3hRun) jobStates(t *testing.T, tn *m3lTenant) map[string]int {
	t.Helper()
	rows, err := r.db.Query(r.ctx, "SELECT state::text, count(*) FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid GROUP BY 1", tn.UserID, tn.CampaignID)
	if err != nil {
		t.Fatalf("read %s's job states: %v", tn.Name, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			t.Fatal(err)
		}
		out[st] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// workersOf is how many jobs of the user each module-3 worker handled: the jobs that went assigned -> sending, grouped by the worker the event names (send_byo.job_events.box_id).
func (r *m3hRun) workersOf(t *testing.T, tn *m3lTenant) map[string]int {
	t.Helper()
	rows, err := r.db.Query(r.ctx, `SELECT box_id::text, count(DISTINCT job_id) FROM send_byo.job_events
		WHERE user_id = $1::uuid AND from_state = 'assigned' AND to_state = 'sending' AND box_id IS NOT NULL GROUP BY 1`, tn.UserID)
	if err != nil {
		t.Fatalf("read which workers handled %s's jobs: %v", tn.Name, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var box string
		var n int
		if err := rows.Scan(&box, &n); err != nil {
			t.Fatal(err)
		}
		out[strings.ToLower(box)] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkMessageIDs asserts that every message of the tenant in the sink carries its job's id in the Message-ID that module 3 builds from it (<job id@sending domain>): the sink's messages are the
// SAME jobs the report lists, not merely the same recipients.
func (r *m3hRun) checkMessageIDs(t *testing.T, tn *m3lTenant, v m3hView) {
	t.Helper()
	rows, err := r.db.Query(r.ctx, "SELECT recipient_email, id::text FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid", tn.UserID, tn.CampaignID)
	if err != nil {
		t.Fatalf("read %s's jobs: %v", tn.Name, err)
	}
	job := map[string]string{}
	for rows.Next() {
		var rc, id string
		if err := rows.Scan(&rc, &id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		job[strings.ToLower(rc)] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, m := range v.Msgs {
		for _, to := range m.To {
			if !strings.HasPrefix(to, tn.Prefix) {
				continue
			}
			id, ok := job[to]
			if !ok {
				t.Fatalf("the sink holds a message to %s but module 3 has no job of %s for it", to, tn.Name)
			}
			if !strings.HasPrefix(m.MessageID, "<"+id+"@") {
				t.Fatalf("the message to %s carries the Message-ID %s, which does not start with its job id <%s@: the sink's message is not the job module 3 reports", to, m.MessageID, id)
			}
			checked++
		}
	}
	if checked != v.of(tn) {
		t.Fatalf("checked the Message-ID of %d message(s) of %s, but the sink holds %d", checked, tn.Name, v.of(tn))
	}
}

// settle polls the sink for a few seconds (the sink fails the test at once on a duplicate): a late duplicate would show up here.
func (r *m3hRun) settle(t *testing.T, seconds int) {
	t.Helper()
	for i := 0; i < seconds; i++ {
		select {
		case <-r.ctx.Done():
			t.Fatal("the test's overall time budget ended while settling")
		case <-time.After(time.Second):
		}
		r.sinkView(t)
	}
}
