//go:build harness && m2worker

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// Helpers of the module 2 OUTAGE proof (m2_worker_outage_test.go): Phase 3 chunk B12 for module 2. Every name starts with `m2o`.
//
// THE SHAPE. Three tests, three separate `go test` invocations (fresh test containers) with module 2's platform API container `m2` STOPPED between the first and the second and STARTED between the
// second and the third by the stage script (tests/harness/m2worker/stage.sh, m2w_outage). What survives from one invocation to the next is one row per phase in a scratch table the first phase creates
// with the superuser connection: harness_outage.m2 (phase integer primary key, state jsonb, recorded_at timestamptz). It is never part of a migration, it is never read by anything but these three tests,
// and a phase that finds no row of the phase before it FAILS with a sentence (it never starts from nothing and never skips).
//
// THE GUARD. The three tests do their real work only when HARNESS_M2_OUTAGE=1, which only the outage sub-stage sets (docker compose run -e). Without it each runs its paired controls and nothing else (the
// controls need no stack), so an ordinary `go test` of the full harness, which lists every test, runs them harmlessly and the floor of tests that ran still counts them; the sub-stage requires, in the log of
// each invocation, both the `--- PASS` line AND the `NOTE outage phaseN live:` line only a live run writes (m2w_outage_require_phase), so a control-only PASS can never stand in for the real phase.
//
// WHAT "DOWN" MEANS HERE. `docker compose stop m2` is a graceful stop (SIGTERM; the service has a 90 s grace period): module 2's Control stops claiming, waits for the send in flight to report (at most
// shutdown_drain_timeout_sec, seeded 30), and leaves anything else exactly as stored; the Control's HTTP listener, which serves the platform API and the worker link, goes away. Postgres, the worker and
// the sink keep running. The worker dials the Control again every second and, after about 30 attempts with nothing unacknowledged in its buffer, exits (module 2's own design: "exiting for restart"); the
// stage plays the orchestrator and starts it again when it is gone (m2w_outage_worker).

const (
	m2oEnvOn    = "HARNESS_M2_OUTAGE" // "1" only inside the outage sub-stage's invocations
	m2oSchema   = "harness_outage"
	m2oTable    = "m2"
	m2oHoldMs   = 5000 // how long the sink holds the final reply of every accepted message in phase 1: far under module 2's 30 s smtp_operation_timeout_sec, far over the stage's reaction time
	m2oRunFirst = 10   // items handed to module 2 in phase 1; the last item of the scenario is the LATE one, handed over only after the module is back
	m2oMinLeft  = 4    // at the moment phase 1 samples the campaign at least this many items must not be terminal yet
)

// m2oKinds is the scenario: nine recipients the sink accepts, one it rejects at RCPT TO (bounced) and one it defers twice (failed / attempts_exhausted at the module's cap). The LAST entry is the late item:
// it is committed in public.job_items with the others but is not handed to module 2 until phase 3, so that phase 2 has a write that must fail and create nothing.
var m2oKinds = []string{"ok", "ok", "bounce", "ok", "ok", "defer", "ok", "ok", "ok", "ok", "ok"}

// m2oLive says whether this invocation is the outage sub-stage's. When it is not, it logs why the phase did nothing and returns false.
func m2oLive(t *testing.T, phase int) bool {
	t.Helper()
	if os.Getenv(m2oEnvOn) == "1" {
		return true
	}
	t.Logf("NOTE outage phase%d control-only: %s=1 is set only by the outage sub-stage (tests/harness/m2worker/stage.sh, m2w_outage), which runs the three phases against a stack it stops and starts between them; an ordinary run proves only that this phase's assertions can fail (the controls above)", phase, m2oEnvOn)
	return false
}

// ---- the state that survives between the three invocations ----

type m2oItem struct {
	Index int
	ID    string // public.job_items.id
	Email string
	Kind  string
	JobID string // module 2's job id; "" for the late item until phase 3 hands it over
}

type m2oTerm struct{ State, Reason string }

// m2oBooks is what the platform holds for the tenant at one moment: every number the outage must leave exactly as it was.
type m2oBooks struct {
	Ledger      int    // every ledger row of the tenant for module 2: the grant, the reserve and every charge
	Charges     int    // the charge rows of the job
	Newest      int64  // the newest ledger id of the tenant
	Available   int64  // platform.balances
	Reserved    int64  // platform.balances
	Rsv         int64  // platform.reserves of the job: reserved
	Chg         int64  // charged
	Rel         int64  // released
	JobStatus   string // platform.jobs.status
	Items       string // public.job_items of the campaign: count and a digest of every row
	Drift       int    // rows of platform.balance_drift (global: the invariant is empty at every moment)
	Overdrawn   int    // rows of platform.reserve_overdrawn (global)
	SampledAt   string // not compared: when it was read
	Fingerprint string // e.fingerprint: ledger count / newest id / balance / reserves
}

func (b m2oBooks) String() string {
	return fmt.Sprintf("ledger rows %d (newest id %d, %d charge row(s) of the job), balance available %d reserved %d, reserve %d/%d/%d (reserved/charged/released), job %s, public.job_items %s, drift rows %d, overdrawn rows %d",
		b.Ledger, b.Newest, b.Charges, b.Available, b.Reserved, b.Rsv, b.Chg, b.Rel, b.JobStatus, b.Items, b.Drift, b.Overdrawn)
}

// m2oModule is module 2's own job rows for the tenant (read from its tables with the superuser connection: evidence, never the module's answer to the adapter).
type m2oModule struct {
	Jobs   int
	IDs    map[string]string // job item id -> module job id
	States map[string]string // job item id -> module job state
}

type m2oPhase1 struct {
	RunID, UserID, CampaignID, PoolID, AccountID, From, Job string
	Items                                                   []m2oItem
	Run                                                     int // how many of Items were handed to module 2 in phase 1
	Dials                                                   map[string]json.RawMessage
	HoldMs                                                  int
	Reserved, UnitCost                                      int64
	Totals                                                  sendapi.Totals
	Terminal                                                map[string]m2oTerm // item id -> the terminal state module 2 reported at the sample
	Settled                                                 []string           // the item refs phase 1 settled: exactly the items that were final at the sample
	ChargedRefs                                             []string           // those of Settled the charge table charges
	Books                                                   m2oBooks
	Module                                                  m2oModule
	Sink                                                    map[string][]string // recipient -> Message-IDs the sink held
	SampleAt                                                time.Time
	SinceRunSec                                             float64
}

type m2oPhase2 struct {
	Books  m2oBooks
	Module m2oModule
	Sink   map[string][]string
	Calls  []string
	Polls  int
	At     time.Time
}

// tenant rebuilds the tenant of phase 1 for this process.
func (p *m2oPhase1) tenant() *m2wTenant {
	tn := &m2wTenant{Name: "outage", UserID: p.UserID, CampaignID: p.CampaignID, PoolID: p.PoolID, AccountID: p.AccountID, From: p.From, byID: map[string]*m2wItem{}}
	for _, it := range p.Items {
		x := &m2wItem{Index: it.Index, ID: it.ID, Email: it.Email, Kind: it.Kind, JobID: it.JobID}
		tn.Items = append(tn.Items, x)
		tn.byID[x.ID] = x
	}
	return tn
}

func m2oStateTable() string { return m2oSchema + "." + m2oTable }

// m2oCreateState creates the scratch schema and this module's table, empty. It is run by phase 1 only, with the superuser connection.
func m2oCreateState(t *testing.T, r *m2wRun) {
	t.Helper()
	for _, q := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + m2oSchema,
		"DROP TABLE IF EXISTS " + m2oStateTable(),
		"CREATE TABLE " + m2oStateTable() + " (phase integer PRIMARY KEY CHECK (phase BETWEEN 1 AND 3), state jsonb NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp())",
	} {
		if _, err := r.db.Exec(r.ctx, q); err != nil {
			t.Fatalf("could not create the scratch state of the outage proof (%s): %v", q, err)
		}
	}
}

func m2oSave(t *testing.T, r *m2wRun, phase int, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("the state of phase %d does not marshal: %v", phase, err)
	}
	if _, err := r.db.Exec(r.ctx, "INSERT INTO "+m2oStateTable()+" (phase, state) VALUES ($1, $2::jsonb) ON CONFLICT (phase) DO UPDATE SET state = EXCLUDED.state, recorded_at = clock_timestamp()", phase, string(raw)); err != nil {
		t.Fatalf("could not record the state of phase %d in %s: %v", phase, m2oStateTable(), err)
	}
}

// m2oLoad reads the row of an earlier phase into dst, or FAILS with a sentence: a phase never starts from nothing.
func m2oLoad(t *testing.T, r *m2wRun, phase, forPhase int, dst any) {
	t.Helper()
	var have bool
	if err := r.db.QueryRow(r.ctx, "SELECT to_regclass($1) IS NOT NULL", m2oStateTable()).Scan(&have); err != nil {
		t.Fatalf("could not look for %s: %v", m2oStateTable(), err)
	}
	if !have {
		t.Fatalf("phase %d of the outage proof found no state: the table %s does not exist, so TestM2Worker_OutagePhase1Start has not run on this database (the sub-stage runs the three phases in order, with module 2 stopped and started between them; a phase never starts from nothing)", forPhase, m2oStateTable())
	}
	var raw []byte
	err := r.db.QueryRow(r.ctx, "SELECT state FROM "+m2oStateTable()+" WHERE phase = $1", phase).Scan(&raw)
	if err != nil {
		t.Fatalf("phase %d of the outage proof found no row for phase %d in %s (%v): the phase before it did not run or did not finish; it never starts from nothing", forPhase, phase, m2oStateTable(), err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("the row of phase %d in %s is not the state this test wrote: %v", phase, m2oStateTable(), err)
	}
}

// ---- the adapter side, with dials of the phase's own choosing ----

// m2oEnv is newM2eEnv with chosen dials: the module client's timeout per attempt, retries and backoff.
func m2oEnv(t *testing.T, r *m2wRun, timeout time.Duration, retries int, backoff time.Duration) *m2eEnv {
	t.Helper()
	cfg := config.Config{M2URL: r.m2URL, M2ServiceToken: r.token, ModuleTimeout: timeout, ModuleRetries: retries, ModuleRetryBackoff: backoff}
	ad, err := m2.FromConfig(cfg)
	if err != nil {
		t.Fatalf("m2.FromConfig refused the harness configuration (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	mc, err := modclient.New(modclient.M2, r.m2URL, r.token, nil, modclient.FromConfig(cfg))
	if err != nil {
		t.Fatalf("modclient.New: %s", r.scrub(err.Error()))
	}
	pool := mnyPool(t, requireEnv(t, envAdminURL))
	res, err := tenant.NewResolver(pool, config.Secret(randomHex(t, 32)))
	if err != nil {
		t.Fatalf("tenant.NewResolver: %v", err)
	}
	e := &m2eEnv{r: r, cfg: cfg, mc: mc, ad: ad, pool: pool, res: res, store: money.New(pool)}
	if err := r.db.QueryRow(r.ctx, "SELECT id::text FROM platform.admins WHERE disabled_at IS NULL ORDER BY created_at LIMIT 1").Scan(&e.admin); err != nil {
		t.Fatalf("platform.admins has no active admin (the first-admin stage must have run): %v", err)
	}
	return e
}

// withDials is the same environment (pool, resolver, money store) with another adapter: other dials, nothing else.
func (e *m2eEnv) withDials(t *testing.T, timeout time.Duration, retries int, backoff time.Duration) *m2eEnv {
	t.Helper()
	cfg := e.cfg
	cfg.ModuleTimeout, cfg.ModuleRetries, cfg.ModuleRetryBackoff = timeout, retries, backoff
	ad, err := m2.FromConfig(cfg)
	if err != nil {
		t.Fatalf("m2.FromConfig: %s", e.r.scrub(err.Error()))
	}
	mc, err := modclient.New(modclient.M2, e.r.m2URL, e.r.token, nil, modclient.FromConfig(cfg))
	if err != nil {
		t.Fatalf("modclient.New: %s", e.r.scrub(err.Error()))
	}
	c := *e
	c.cfg, c.ad, c.mc = cfg, ad, mc
	return &c
}

// m2oWaitSinkIdle waits (bounded) until the sink holds no session: a send of an earlier test must not run under this scenario.
func m2oWaitSinkIdle(t *testing.T, r *m2wRun) {
	t.Helper()
	start := time.Now()
	for {
		if v := r.inflight(t); v.Inflight == 0 {
			return
		}
		if time.Since(start) > m2wDrainLimit {
			t.Fatalf("the sink still holds %d session(s) %s after the test began: a send of an earlier test is still in flight", r.inflight(t).Inflight, time.Since(start).Round(time.Second))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// m2oRunSome hands the items to module 2 through the adapter: each must be a NEW job (202, queued, class pending).
func m2oRunSome(t *testing.T, e *m2eEnv, ad *m2.Client, items []*m2wItem) {
	t.Helper()
	seen := map[string]string{}
	for _, it := range items {
		res, err := ad.Run(e.r.ctx, it.ID)
		if err != nil {
			t.Fatalf("Run of item %02d through the adapter failed (%s): %s", it.Index, modclient.KindOf(err), e.r.scrub(err.Error()))
		}
		if !res.Created || res.State != m2wStateQueued || res.Class != sendapi.ClassPending || res.JobID == "" {
			t.Fatalf("Run of item %02d answered %+v, want a new job (Created) in state queued of class pending with a job id", it.Index, res)
		}
		if prev, dup := seen[res.JobID]; dup {
			t.Fatalf("Run answered the job id %s for two items (%s and %s)", res.JobID, prev, it.ID)
		}
		seen[res.JobID] = it.ID
		it.JobID = res.JobID
	}
}

// ---- reading what the platform and the module hold ----

func (e *m2eEnv) m2oBooks(t *testing.T, tn *m2wTenant, job string) m2oBooks {
	t.Helper()
	r := e.r
	var b m2oBooks
	if err := r.db.QueryRow(r.ctx, "SELECT count(*), COALESCE(max(id), 0) FROM platform.ledger WHERE user_id = $1::uuid AND module_id = 'm2'", tn.UserID).Scan(&b.Ledger, &b.Newest); err != nil {
		t.Fatalf("read the ledger of %s: %v", tn.Name, err)
	}
	if err := r.db.QueryRow(r.ctx, "SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge'", job).Scan(&b.Charges); err != nil {
		t.Fatalf("read the charge rows of job %s: %v", job, err)
	}
	bal := e.balance(t, tn)
	b.Available, b.Reserved = int64(bal.Available), int64(bal.Reserved)
	b.Rsv, b.Chg, b.Rel = e.reserveRow(t, job)
	if err := r.db.QueryRow(r.ctx, "SELECT status FROM platform.jobs WHERE id = $1::uuid", job).Scan(&b.JobStatus); err != nil {
		t.Fatalf("read the platform job %s: %v", job, err)
	}
	if err := r.db.QueryRow(r.ctx, `SELECT count(*)::text || ':' || COALESCE(md5(string_agg(id::text || '|' || recipient_email || '|' || rendered_subject || '|' || rendered_body || '|' || status, ',' ORDER BY id)), '-')
		FROM public.job_items WHERE campaign_id = $1::uuid AND user_id = $2::uuid`, tn.CampaignID, tn.UserID).Scan(&b.Items); err != nil {
		t.Fatalf("read public.job_items of the campaign of %s: %v", tn.Name, err)
	}
	if err := r.db.QueryRow(r.ctx, "SELECT (SELECT count(*) FROM platform.balance_drift), (SELECT count(*) FROM platform.reserve_overdrawn)").Scan(&b.Drift, &b.Overdrawn); err != nil {
		t.Fatalf("read platform.balance_drift and platform.reserve_overdrawn: %v", err)
	}
	b.Fingerprint = e.fingerprint(t, tn)
	b.SampledAt = time.Now().UTC().Format(time.RFC3339)
	return b
}

// m2oBooksProblems compares two snapshots field by field, except the time of reading. It is the assertion of phase 2 ("exactly what phase 1 recorded"), phase 3's first look, and the thing the controls corrupt.
func m2oBooksProblems(want, got m2oBooks) []string {
	var p []string
	cmp := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			p = append(p, fmt.Sprintf("%s is %v, phase 1 recorded %v", name, b, a))
		}
	}
	cmp("the number of ledger rows of the tenant", want.Ledger, got.Ledger)
	cmp("the number of charge rows of the job", want.Charges, got.Charges)
	cmp("the newest ledger id of the tenant", want.Newest, got.Newest)
	cmp("the available balance", want.Available, got.Available)
	cmp("the reserved balance", want.Reserved, got.Reserved)
	cmp("the reserve's reserved amount", want.Rsv, got.Rsv)
	cmp("the reserve's charged amount", want.Chg, got.Chg)
	cmp("the reserve's released amount", want.Rel, got.Rel)
	cmp("the platform job's status", want.JobStatus, got.JobStatus)
	cmp("public.job_items of the campaign (count:digest)", want.Items, got.Items)
	cmp("the fingerprint of the books", want.Fingerprint, got.Fingerprint)
	if got.Drift != 0 {
		p = append(p, fmt.Sprintf("platform.balance_drift holds %d row(s), want none", got.Drift))
	}
	if got.Overdrawn != 0 {
		p = append(p, fmt.Sprintf("platform.reserve_overdrawn holds %d row(s), want none", got.Overdrawn))
	}
	return p
}

// m2oModuleJobs reads module 2's own job rows of the tenant.
func (r *m2wRun) m2oModuleJobs(t *testing.T, tn *m2wTenant) m2oModule {
	t.Helper()
	rows, err := r.db.Query(r.ctx, `SELECT j.job_item_id::text, j.id::text, j.state::text FROM send_managed.jobs j JOIN public.job_items ji ON ji.id = j.job_item_id
		WHERE ji.user_id = $1::uuid ORDER BY j.created_at, j.id`, tn.UserID)
	if err != nil {
		t.Fatalf("read module 2's job rows of %s: %v", tn.Name, err)
	}
	defer rows.Close()
	m := m2oModule{IDs: map[string]string{}, States: map[string]string{}}
	for rows.Next() {
		var item, job, state string
		if err := rows.Scan(&item, &job, &state); err != nil {
			t.Fatal(err)
		}
		m.Jobs++
		m.IDs[item], m.States[item] = job, state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

// m2oModuleProblems says how the module's own job rows differ from the earlier ones: the number of jobs, and for every job item the job id it has (a write that "failed" must have created nothing, and nothing may be
// created twice).
func m2oModuleProblems(want, got m2oModule) []string {
	var p []string
	if want.Jobs != got.Jobs {
		p = append(p, fmt.Sprintf("module 2 holds %d job(s) for the tenant, phase 1 recorded %d", got.Jobs, want.Jobs))
	}
	ids := make([]string, 0, len(got.IDs))
	for it := range got.IDs {
		ids = append(ids, it)
	}
	sort.Strings(ids)
	for _, it := range ids {
		if w, ok := want.IDs[it]; !ok {
			p = append(p, fmt.Sprintf("module 2 holds a job (%s) for item %s, which phase 1 had not handed to it", got.IDs[it], it))
		} else if w != got.IDs[it] {
			p = append(p, fmt.Sprintf("item %s has job %s, phase 1 recorded job %s", it, got.IDs[it], w))
		}
	}
	for it := range want.IDs {
		if _, ok := got.IDs[it]; !ok {
			p = append(p, fmt.Sprintf("module 2's job for item %s is gone", it))
		}
	}
	sort.Strings(p)
	return p
}

// m2oSinkIDs is the sink's view reduced to what the outage proof compares: recipient -> the Message-IDs it holds, sorted.
func m2oSinkIDs(v m2wView) map[string][]string {
	out := map[string][]string{}
	for a, msgs := range v.Msgs {
		var ids []string
		for _, m := range msgs {
			ids = append(ids, m.MessageID)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			out[a] = ids
		}
	}
	return out
}

// m2oSinkProblems is the exactly-once assertion of phase 3 against the sink: one message for every item the module reports sent, none for any other item, and the messages the sink already held before the module
// came back are still there, the same ones (a message sent again would carry another Message-ID). earlier is the sink as phase 2 recorded it.
func m2oSinkProblems(tn *m2wTenant, items []sendapi.Item, view m2wView, earlier map[string][]string) []string {
	var p []string
	byID := map[string]sendapi.Item{}
	for _, it := range items {
		byID[it.JobItemID] = it
	}
	sent, total := 0, 0
	for _, it := range tn.Items {
		rep, ok := byID[it.ID]
		if !ok {
			p = append(p, fmt.Sprintf("item %02d (%s) is not in the report", it.Index, it.Email))
			continue
		}
		want := 0
		if rep.State == "sent" {
			want = 1
			sent++
		}
		got := view.messages(it.Email)
		total += got
		if got != want {
			p = append(p, fmt.Sprintf("the sink holds %d message(s) for %s (item %02d, reported %s), want %d: a sent item is sent exactly once, any other has no message", got, it.Email, it.Index, rep.State, want))
		}
	}
	if total != sent {
		p = append(p, fmt.Sprintf("the sink holds %d message(s) of this run, the module reports %d item(s) sent", total, sent))
	}
	now := m2oSinkIDs(view)
	emails := make([]string, 0, len(earlier))
	for a := range earlier {
		emails = append(emails, a)
	}
	sort.Strings(emails)
	for _, a := range emails {
		if !reflect.DeepEqual(earlier[a], now[a]) {
			p = append(p, fmt.Sprintf("the sink held the message(s) %v for %s while the module was down and holds %v now: a message was sent again, or lost", earlier[a], a, now[a]))
		}
	}
	return p
}

// ---- phase 1: "the job is certainly in flight" ----

// m2oInFlightProblems is what phase 1 asserts about the sample of the campaign it takes just before the stage stops the module: module 2 counts exactly the items the platform handed it, progress has started (an item was
// sent), the campaign is not complete, and at least minLeft items are still not terminal (so that the stop, which comes some seconds after this sample, cannot find the job finished).
func m2oInFlightProblems(rep sendapi.CampaignReport, run, minLeft int) []string {
	var p []string
	if rep.Totals.Total != int64(run) || len(rep.Items) != run {
		p = append(p, fmt.Sprintf("the report counts %d item(s) (%d listed), the platform handed %d to the module", rep.Totals.Total, len(rep.Items), run))
	}
	if rep.Progress.IsComplete {
		p = append(p, "the module says the campaign is complete: the job is not in flight any more, a stop would interrupt nothing")
	}
	if rep.Totals.Sent < 1 {
		p = append(p, "no item has been sent: progress has not started, so a stop would interrupt nothing that was proven to work")
	}
	terminal := 0
	for _, it := range rep.Items {
		if it.Class == sendapi.ClassTerminal {
			terminal++
		}
	}
	if left := len(rep.Items) - terminal; left < minLeft {
		p = append(p, fmt.Sprintf("only %d item(s) are not terminal yet, want at least %d: the stage stops the module some seconds after this sample and the campaign could finish before it does (the proof would race)", left, minLeft))
	}
	return p
}

// m2oFakeReport builds a campaign report from item states, for the controls.
func m2oFakeReport(states ...string) sendapi.CampaignReport {
	var rep sendapi.CampaignReport
	terminal := int64(0)
	for i, s := range states {
		it := sendapi.Item{JobItemID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), State: s}
		switch s {
		case "sent", "bounced", "failed", "cancelled":
			it.Class = sendapi.ClassTerminal
			terminal++
		case "assigned", "sending":
			it.Class = sendapi.ClassInFlight
		default:
			it.Class = sendapi.ClassPending
		}
		switch s {
		case "sent":
			rep.Totals.Sent++
		case "bounced":
			rep.Totals.Bounced++
		case "failed":
			rep.Totals.Failed++
		case "cancelled":
			rep.Totals.Cancelled++
		case "assigned", "sending":
			rep.Totals.InFlight++
		default:
			rep.Totals.Pending++
		}
		rep.Items = append(rep.Items, it)
	}
	rep.Totals.Total = int64(len(states))
	rep.Progress.Terminal = terminal
	rep.Progress.IsComplete = len(states) > 0 && terminal == int64(len(states))
	rep.Pages = 1
	return rep
}

// ---- phase 2: what a stopped module may answer ----

func m2oIsZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return rv.IsZero()
}

// m2oCause is the fixed class word of the cause the module client attached to an error ("connection refused", "dns lookup failed (no such host)", "the request timed out"): never the raw text of the
// transport error, which can carry an address.
func m2oCause(err error) (cause string, sent bool, attempts int) {
	var me *modclient.Error
	if !errors.As(err, &me) {
		return "-", false, 0
	}
	cause = "-"
	if me.Cause != nil {
		cause = me.Cause.Error()
	}
	return cause, me.Sent, me.Attempts
}

// m2oJudgeCall is THE assertion of phase 2, per call against the stopped module: a typed error of kind Unavailable or Timeout (a DNS failure of the stopped container's name is Unavailable, a dial that hangs until the
// per-attempt timeout is Timeout: both are the module being gone), no value alongside it (the zero value of the call's type), a request that never left (Sent is false: it cannot be an orphan) and an answer in bounded
// time. It returns its problems as sentences so that the controls can show it fails on a fake that breaks each property.
func m2oJudgeCall(name string, v any, err error, took, bound time.Duration) []string {
	var p []string
	say := func(f string, a ...any) { p = append(p, name+": "+fmt.Sprintf(f, a...)) }
	if err == nil {
		say("returned NO error although module 2 is stopped: an answer without an error is something the platform would act on")
	} else {
		if k := modclient.KindOf(err); k != modclient.Unavailable && k != modclient.Timeout {
			say("failed as kind %q, want unavailable or timeout (an empty kind is an error that is not a modclient error at all)", k.String())
		}
		var me *modclient.Error
		if errors.As(err, &me) && me.Sent {
			say("the error says the request MAY have reached the module (Sent is true) although nothing listens: the platform would treat the call as a possible orphan")
		}
	}
	if !m2oIsZero(v) {
		say("returned a value (%T) alongside the call's result: after an error the value must be the zero value, no rows and no counts", v)
	}
	if bound > 0 && took > bound {
		say("took %s, longer than its bound of %s: a loop that polls with it could not halt promptly", took.Round(time.Millisecond), bound)
	}
	return p
}

// m2oPollStats is what a bounded polling loop saw.
type m2oPollStats struct {
	Calls, Nil, Values int
	Kinds              map[modclient.Kind]int
	Elapsed            time.Duration
}

type m2oReporter interface {
	ReportCampaign(ctx context.Context, campaignID string) (sendapi.CampaignReport, error)
}

// m2oFakeReporter is a stand-in for the adapter, for the controls.
type m2oFakeReporter func(ctx context.Context, campaignID string) (sendapi.CampaignReport, error)

func (f m2oFakeReporter) ReportCampaign(ctx context.Context, c string) (sendapi.CampaignReport, error) {
	return f(ctx, c)
}

// m2oPoll polls the way the platform's job engine will: it reads the campaign report, again and again, until the context it was given ends. It does nothing with an error but count it, which is all a loop that
// must be able to halt does.
func m2oPoll(ctx context.Context, ad m2oReporter, campaignID string, every time.Duration) m2oPollStats {
	st := m2oPollStats{Kinds: map[modclient.Kind]int{}}
	start := time.Now()
	for {
		rep, err := ad.ReportCampaign(ctx, campaignID)
		st.Calls++
		if err == nil {
			st.Nil++
		} else {
			st.Kinds[modclient.KindOf(err)]++
		}
		if !m2oIsZero(rep) {
			st.Values++
		}
		select {
		case <-ctx.Done():
			st.Elapsed = time.Since(start)
			return st
		case <-time.After(every):
		}
	}
}

// m2oPollProblems: the loop really looped, every answer was a typed unavailable or timeout, none carried a value, and it stopped by its context (within slack of the deadline it was given).
func m2oPollProblems(st m2oPollStats, ctxBound, slack time.Duration) []string {
	var p []string
	if st.Calls < 2 {
		p = append(p, fmt.Sprintf("the polling loop called module 2 %d time(s): it is not a loop", st.Calls))
	}
	if st.Nil > 0 {
		p = append(p, fmt.Sprintf("%d call(s) of the polling loop returned NO error although module 2 is stopped", st.Nil))
	}
	if st.Values > 0 {
		p = append(p, fmt.Sprintf("%d call(s) of the polling loop returned a non-zero report: after an error the report must be the zero value", st.Values))
	}
	kinds := make([]string, 0, len(st.Kinds))
	for k, n := range st.Kinds {
		if k != modclient.Unavailable && k != modclient.Timeout {
			kinds = append(kinds, fmt.Sprintf("%d x %q", n, k.String()))
		}
	}
	sort.Strings(kinds)
	if len(kinds) > 0 {
		p = append(p, "the polling loop saw errors that are neither unavailable nor timeout: "+strings.Join(kinds, ", "))
	}
	if st.Elapsed > ctxBound+slack {
		p = append(p, fmt.Sprintf("the polling loop ran %s, its context allowed %s (+ %s slack): the platform could not halt it", st.Elapsed.Round(time.Millisecond), ctxBound, slack))
	}
	return p
}

// m2oReadyz is the platform's own /readyz handler (internal/httpapi, the code the service serves) asked once with the given pinger. Its Deps hold a database pinger, a timeout and a draining flag and nothing
// of any module: readiness cannot depend on a module being up, and this asks it while module 2 is down.
func m2oReadyz(p httpapi.Pinger) (int, string) {
	h := httpapi.NewHandler(httpapi.Deps{Pinger: p, ReadyTimeout: 2 * time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func m2oReadyzProblems(code int, body string) []string {
	var p []string
	if code != http.StatusOK {
		p = append(p, fmt.Sprintf("/readyz answered %d (%s), want 200: the platform's readiness must not depend on a module", code, body))
	}
	if !strings.Contains(body, `"status":"ready"`) || !strings.Contains(body, `"db":true`) {
		p = append(p, fmt.Sprintf("/readyz body %q does not say ready with the database check true", body))
	}
	return p
}

type m2oPinger func(context.Context) error

func (f m2oPinger) Ping(ctx context.Context) error { return f(ctx) }

// m2oPlatformStep is the step the job engine will take on every poll: read the campaign report, and ONLY if that succeeded classify and settle what is final. Against a stopped module the read fails and the step
// must change nothing; the caller compares the books before and after.
func (e *m2eEnv) m2oPlatformStep(ctx context.Context, report m2oReporter, ad *m2.Client, job string, campaign string) error {
	rep, err := report.ReportCampaign(ctx, campaign)
	if err != nil {
		return err
	}
	cl, err := ad.Classify(rep.Items)
	if err != nil {
		return err
	}
	_, err = e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	return err
}

// ---- phase 3: telling a transient failure from a wrong answer ----

// m2oTransient is true for the two kinds a module that is coming back may answer with: unavailable (nothing listens yet, the name does not resolve, a reset) and timeout. Everything else (internal, contract, refused,
// not found, invalid, paused, rate limited, canceled, or an error that is not a modclient error) is an answer, and a wrong one.
func m2oTransient(err error) bool {
	k := modclient.KindOf(err)
	return k == modclient.Unavailable || k == modclient.Timeout
}

// m2oRegression compares the first report after the restart with what phase 1 saw: the module counts exactly the items handed to it, none it never had, every item that was terminal in phase 1 is still terminal with
// the same state and reason (a final outcome never changes), and the terminal counts only grow.
func m2oRegression(p1 *m2oPhase1, rep sendapi.CampaignReport) []string {
	var p []string
	if rep.Totals.Total != int64(p1.Run) || len(rep.Items) != p1.Run {
		p = append(p, fmt.Sprintf("the report counts %d item(s) (%d listed), the platform handed %d to the module before the outage", rep.Totals.Total, len(rep.Items), p1.Run))
	}
	known := map[string]bool{}
	for _, it := range p1.Items[:p1.Run] {
		known[it.ID] = true
	}
	byID := map[string]sendapi.Item{}
	for _, it := range rep.Items {
		if !known[it.JobItemID] {
			p = append(p, fmt.Sprintf("the report lists item %s, which the platform never handed to the module", it.JobItemID))
		}
		byID[it.JobItemID] = it
	}
	ids := make([]string, 0, len(p1.Terminal))
	for id := range p1.Terminal {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := p1.Terminal[id]
		it, ok := byID[id]
		switch {
		case !ok:
			p = append(p, fmt.Sprintf("item %s was %s in phase 1 and is missing from the report", id, w.State))
		case it.State != w.State || m2eReason(it.Reason) != w.Reason:
			p = append(p, fmt.Sprintf("item %s was %s/%q in phase 1 and is reported %s/%q: a final outcome changed", id, w.State, w.Reason, it.State, m2eReason(it.Reason)))
		}
	}
	if rep.Totals.Sent < p1.Totals.Sent || rep.Totals.Bounced < p1.Totals.Bounced || rep.Totals.Failed < p1.Totals.Failed {
		p = append(p, fmt.Sprintf("the terminal counts went down: sent %d->%d bounced %d->%d failed %d->%d", p1.Totals.Sent, rep.Totals.Sent, p1.Totals.Bounced, rep.Totals.Bounced, p1.Totals.Failed, rep.Totals.Failed))
	}
	return p
}

// m2oTransients counts the transient errors seen while the module came back.
type m2oTransients struct {
	N     int
	Kinds map[string]int
}

func (tr m2oTransients) String() string {
	if tr.N == 0 {
		return "none"
	}
	names := make([]string, 0, len(tr.Kinds))
	for k, n := range tr.Kinds {
		names = append(names, fmt.Sprintf("%s x%d", k, n))
	}
	sort.Strings(names)
	return fmt.Sprintf("%d (%s)", tr.N, strings.Join(names, ", "))
}

// m2oResume polls the campaign report through the adapter until the module answers for the first time after the restart. Until then a typed unavailable or timeout is the module coming back and is retried; any other
// error, and any answer that disagrees with phase 1, is a wrong answer and fails the test at once. The wait is bounded by budget.
func m2oResume(t *testing.T, r *m2wRun, ad *m2.Client, tn *m2wTenant, p1 *m2oPhase1, budget time.Duration) (sendapi.CampaignReport, m2oTransients, time.Duration) {
	t.Helper()
	tr := m2oTransients{Kinds: map[string]int{}}
	start := time.Now()
	for {
		rep, err := ad.ReportCampaign(r.ctx, tn.CampaignID)
		switch {
		case err == nil:
			if p := m2oRegression(p1, rep); len(p) > 0 {
				t.Fatalf("module 2 answered after the restart and the answer is WRONG, not a transient failure: %s", strings.Join(p, "; "))
			}
			return rep, tr, time.Since(start)
		case m2oTransient(err):
			tr.N++
			tr.Kinds[modclient.KindOf(err).String()]++
		default:
			t.Fatalf("module 2 answered after the restart with %s, which is a wrong answer and not a transient unavailable or timeout: %s", modclient.KindOf(err), r.scrub(err.Error()))
		}
		if time.Since(start) > budget {
			t.Fatalf("module 2 did not answer for %s after the restart (%s transient error(s) so far, the last: %s)", budget, tr, r.scrub(err.Error()))
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("the test's context ended while waiting for module 2 to come back: %v", r.ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// ---- the controls ----

// m2oCase is a control: input that breaks one property, and the words of the problem that must report it.
type m2oCase struct {
	name, want string
	problems   []string
}

// m2oRequire fails the test unless every case's problems contain its words (the assertion CAN fail, for the reason it names) and the good input, run through the same function, is accepted.
func m2oRequire(t *testing.T, what string, good []string, cases []m2oCase) {
	t.Helper()
	if len(good) != 0 {
		t.Errorf("control %s: the assertion rejects a good input: %s", what, strings.Join(good, "; "))
	}
	caught := 0
	for _, c := range cases {
		hit := false
		for _, s := range c.problems {
			if strings.Contains(s, c.want) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("control %s: the assertion did NOT report %q (want a problem containing %q), it said %q", what, c.name, c.want, c.problems)
			continue
		}
		caught++
	}
	t.Logf("NOTE outage control %s: the assertion accepts a good input and reported %d of %d corrupted ones, each for the reason it names", what, caught, len(cases))
}

func m2oUnavailable(sent bool) error {
	return &modclient.Error{Kind: modclient.Unavailable, Module: modclient.M2, Route: "m2.fake", Message: "the module is stopped", Sent: sent}
}

// m2oControlPhase1: the in-flight assertion reports a finished campaign, no progress, a campaign about to finish and a wrong count.
func m2oControlPhase1(t *testing.T) {
	t.Helper()
	good := m2oFakeReport("sent", "sending", "queued", "queued", "queued", "queued", "queued", "queued", "queued", "queued")
	two := func(s ...string) sendapi.CampaignReport { return m2oFakeReport(s...) }
	m2oRequire(t, "phase 1 (the job is in flight)", m2oInFlightProblems(good, 10, m2oMinLeft), []m2oCase{
		{"a campaign that has finished", "the job is not in flight any more", m2oInFlightProblems(two("sent", "sent", "bounced", "failed", "sent", "sent", "sent", "sent", "sent", "sent"), 10, m2oMinLeft)},
		{"a campaign that has not started", "progress has not started", m2oInFlightProblems(two("queued", "queued", "queued", "queued", "queued", "queued", "queued", "queued", "queued", "queued"), 10, m2oMinLeft)},
		{"a campaign one item from done", "the proof would race", m2oInFlightProblems(two("sent", "sent", "sent", "sent", "sent", "sent", "sent", "sent", "sent", "sending"), 10, m2oMinLeft)},
		{"a report of another size than what was handed over", "the platform handed 10", m2oInFlightProblems(two("sent", "queued", "queued", "queued", "queued"), 10, m2oMinLeft)},
	})
}

// m2oControlPhase2: the per-call assertion, the polling assertion, the books comparison and the readiness assertion each report what they exist to catch.
func m2oControlPhase2(t *testing.T) {
	t.Helper()
	const bound = 8 * time.Second
	ok := time.Millisecond
	goodCalls := append(m2oJudgeCall("Quote", sendapi.Quote{}, m2oUnavailable(false), ok, bound),
		m2oJudgeCall("Run", sendapi.RunResult{}, &modclient.Error{Kind: modclient.Timeout, Module: modclient.M2, Sent: false}, ok, bound)...) // unavailable and timeout are both "the module is gone"
	m2oRequire(t, "phase 2 (one call against a stopped module)", goodCalls, []m2oCase{
		{"a listing that returns zero values and a nil error", "returned NO error", m2oJudgeCall("ReportCampaign", sendapi.CampaignReport{}, nil, ok, bound)},
		{"a value alongside an unavailable error", "returned a value", m2oJudgeCall("Quote", sendapi.Quote{Cost: 3}, m2oUnavailable(false), ok, bound)},
		{"rows alongside an unavailable error", "returned a value", m2oJudgeCall("ReportCampaign", m2oFakeReport("sent"), m2oUnavailable(false), ok, bound)},
		{"an internal error", `kind "internal"`, m2oJudgeCall("Run", sendapi.RunResult{}, &modclient.Error{Kind: modclient.Internal, Module: modclient.M2}, ok, bound)},
		{"an error that is not a modclient error", "kind", m2oJudgeCall("Run", sendapi.RunResult{}, errors.New("connection refused"), ok, bound)},
		{"an error that claims the request may have left", "possible orphan", m2oJudgeCall("Run", sendapi.RunResult{}, m2oUnavailable(true), ok, bound)},
		{"a call that took longer than its bound", "could not halt promptly", m2oJudgeCall("Quote", sendapi.Quote{}, m2oUnavailable(false), 30*time.Second, bound)},
	})

	// the polling loop, run for real against two fakes: one that behaves like a stopped module and one that answers with a zero value and no error; the third ignores its context
	downFake := m2oFakeReporter(func(ctx context.Context, _ string) (sendapi.CampaignReport, error) {
		return sendapi.CampaignReport{}, m2oUnavailable(false)
	})
	zeroFake := m2oFakeReporter(func(ctx context.Context, _ string) (sendapi.CampaignReport, error) {
		return sendapi.CampaignReport{}, nil
	})
	stuckFake := m2oFakeReporter(func(ctx context.Context, _ string) (sendapi.CampaignReport, error) {
		time.Sleep(900 * time.Millisecond) // does not look at ctx: the loop it is in cannot stop by its context
		return sendapi.CampaignReport{}, m2oUnavailable(false)
	})
	loop := func(f m2oFakeReporter, d time.Duration) m2oPollStats {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return m2oPoll(ctx, f, "00000000-0000-4000-8000-000000000001", 20*time.Millisecond)
	}
	m2oRequire(t, "phase 2 (the polling loop halts by its context)", m2oPollProblems(loop(downFake, 300*time.Millisecond), 300*time.Millisecond, 500*time.Millisecond), []m2oCase{
		{"a fake that answers a zero value and no error", "returned NO error", m2oPollProblems(loop(zeroFake, 200*time.Millisecond), 200*time.Millisecond, 500*time.Millisecond)},
		{"a fake that ignores the context", "could not halt it", m2oPollProblems(loop(stuckFake, 200*time.Millisecond), 200*time.Millisecond, 100*time.Millisecond)},
		{"a loop that never loops", "it is not a loop", m2oPollProblems(m2oPollStats{Calls: 1, Kinds: map[modclient.Kind]int{modclient.Unavailable: 1}}, time.Second, time.Second)},
		{"errors of the wrong kind", "neither unavailable nor timeout", m2oPollProblems(m2oPollStats{Calls: 5, Kinds: map[modclient.Kind]int{modclient.Contract: 5}}, time.Second, time.Second)},
	})

	// the books: a fake that settles during the outage changes the ledger, the reserve and the balance, and a fake that invents a job item changes public.job_items
	base := m2oBooks{Ledger: 3, Charges: 1, Newest: 41, Available: 960, Reserved: 22, Rsv: 24, Chg: 2, Rel: 0, JobStatus: "running", Items: "11:abc", Fingerprint: "3/41/960:22/24:2:0"}
	moved := func(f func(b *m2oBooks)) []string {
		b := base
		f(&b)
		return m2oBooksProblems(base, b)
	}
	m2oRequire(t, "phase 2 (the books are exactly what phase 1 recorded)", m2oBooksProblems(base, base), []m2oCase{
		{"a settle during the outage (a charge row)", "the number of ledger rows", moved(func(b *m2oBooks) { b.Ledger++; b.Charges++; b.Newest++ })},
		{"a settle during the outage (the reserve's charged amount)", "the reserve's charged amount", moved(func(b *m2oBooks) { b.Chg += 2 })},
		{"a release during the outage", "the reserve's released amount", moved(func(b *m2oBooks) { b.Rel = 22 })},
		{"a changed balance", "the available balance", moved(func(b *m2oBooks) { b.Available -= 2 })},
		{"an invented job item", "public.job_items of the campaign", moved(func(b *m2oBooks) { b.Items = "12:def" })},
		{"a job that moved on", "the platform job's status", moved(func(b *m2oBooks) { b.JobStatus = "succeeded" })},
		{"a balance that does not equal its ledger", "platform.balance_drift", moved(func(b *m2oBooks) { b.Drift = 1 })},
		{"a reserve spent beyond itself", "platform.reserve_overdrawn", moved(func(b *m2oBooks) { b.Overdrawn = 1 })},
	})
	wantMod := m2oModule{Jobs: 2, IDs: map[string]string{"a": "ja", "b": "jb"}}
	m2oRequire(t, "phase 2 (the module's job count is unchanged)", m2oModuleProblems(wantMod, wantMod), []m2oCase{
		{"a job created by the write that failed", "holds 3 job(s)", m2oModuleProblems(wantMod, m2oModule{Jobs: 3, IDs: map[string]string{"a": "ja", "b": "jb", "c": "jc"}})},
		{"a job for an item phase 1 had not handed over", "had not handed to it", m2oModuleProblems(wantMod, m2oModule{Jobs: 2, IDs: map[string]string{"a": "ja", "c": "jc"}})},
		{"a job that disappeared", "is gone", m2oModuleProblems(wantMod, m2oModule{Jobs: 1, IDs: map[string]string{"a": "ja"}})},
		{"a job replaced by another", "phase 1 recorded job jb", m2oModuleProblems(wantMod, m2oModule{Jobs: 2, IDs: map[string]string{"a": "ja", "b": "jx"}})},
	})

	// readiness: a handler over a database that answers is ready; over one that does not it is not (so the assertion can fail)
	code, body := m2oReadyz(m2oPinger(func(context.Context) error { return nil }))
	failCode, failBody := m2oReadyz(m2oPinger(func(context.Context) error { return errors.New("the database is gone") }))
	m2oRequire(t, "phase 2 (the platform's /readyz does not follow a module)", m2oReadyzProblems(code, body), []m2oCase{
		{"a handler whose database does not answer", "want 200", m2oReadyzProblems(failCode, failBody)},
		{"an answer that is not the ready document", "does not say ready", m2oReadyzProblems(200, `{"status":"up"}`)},
	})
}

// m2oControlPhase3: a transient failure is told from a wrong answer; a regression, a stranger, a missing or a changed item is reported; a message sent twice, lost, replaced or sent for a failed item is reported.
func m2oControlPhase3(t *testing.T) {
	t.Helper()
	trans := []struct {
		name string
		err  error
		want bool
	}{
		{"unavailable", m2oUnavailable(false), true},
		{"timeout", &modclient.Error{Kind: modclient.Timeout}, true},
		{"unavailable wrapped by the adapter", fmt.Errorf("the walk failed on page 1: %w", m2oUnavailable(false)), true},
		{"internal", &modclient.Error{Kind: modclient.Internal}, false},
		{"contract", &modclient.Error{Kind: modclient.Contract}, false},
		{"refused", &modclient.Error{Kind: modclient.Refused}, false},
		{"not found", &modclient.Error{Kind: modclient.NotFound}, false},
		{"no error", nil, false},
		{"a plain error", errors.New("boom"), false},
	}
	var wrong []string
	for _, c := range trans {
		if got := m2oTransient(c.err); got != c.want {
			wrong = append(wrong, fmt.Sprintf("%s classified as transient=%v, want %v", c.name, got, c.want))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("control phase 3 (a transient failure is not a wrong answer): m2oTransient disagrees with its table: %s", strings.Join(wrong, "; "))
	}
	t.Logf("NOTE outage control phase 3 (a transient failure is not a wrong answer): m2oTransient agrees with all %d rows of its table (unavailable, timeout and the adapter's wrapping of unavailable are transient; internal, contract, refused, not found, no error and a plain error are not)", len(trans))

	items := make([]m2oItem, 0, 6)
	rep := m2oFakeReport("sent", "sent", "bounced", "sending", "queued", "queued")
	p1 := &m2oPhase1{Run: 6, Terminal: map[string]m2oTerm{}}
	for i, it := range rep.Items {
		items = append(items, m2oItem{Index: i + 1, ID: it.JobItemID, Email: fmt.Sprintf("m2w-0000-outage-ok%02d@example.test", i+1), Kind: "ok"})
		if it.Class == sendapi.ClassTerminal {
			p1.Terminal[it.JobItemID] = m2oTerm{State: it.State}
		}
	}
	p1.Items = items
	p1.Totals = rep.Totals
	later := m2oFakeReport("sent", "sent", "bounced", "sent", "sending", "queued")
	reg := func(f func(r *sendapi.CampaignReport)) []string {
		c := later
		c.Items = append([]sendapi.Item(nil), later.Items...)
		f(&c)
		return m2oRegression(p1, c)
	}
	m2oRequire(t, "phase 3 (the first answer after the restart is consistent with phase 1)", m2oRegression(p1, later), []m2oCase{
		{"a final item that went back to queued", "a final outcome changed", reg(func(r *sendapi.CampaignReport) { r.Items[0].State = "queued"; r.Items[0].Class = sendapi.ClassPending })},
		{"a final item that is missing", "is missing from the report", reg(func(r *sendapi.CampaignReport) { r.Items = r.Items[1:]; r.Totals.Total-- })},
		{"an item the platform never handed over", "never handed to the module", reg(func(r *sendapi.CampaignReport) { r.Items[5].JobItemID = "00000000-0000-4000-8000-0000000000ff" })},
		{"terminal counts that went down", "the terminal counts went down", reg(func(r *sendapi.CampaignReport) { r.Totals.Sent = 1 })},
		{"another number of items", "the platform handed 6", reg(func(r *sendapi.CampaignReport) { r.Totals.Total = 9 })},
	})

	tn := p1.tenantOf()
	final := m2oFakeReport("sent", "sent", "bounced", "sent", "sent", "sent")
	for i := range final.Items {
		final.Items[i].JobItemID = items[i].ID
	}
	view := func(mut func(v *m2wView)) []string {
		v := m2wView{Msgs: map[string][]m2wSinkMsg{}, Rcpt: map[string]m2wRcpt{}}
		for i, it := range items {
			if final.Items[i].State == "sent" {
				v.Msgs[it.Email] = []m2wSinkMsg{{MessageID: "<" + it.ID + "@example.test>", To: []string{it.Email}}}
			}
		}
		mut(&v)
		return m2oSinkProblems(tn, final.Items, v, map[string][]string{items[0].Email: {"<" + items[0].ID + "@example.test>"}})
	}
	m2oRequire(t, "phase 3 (nothing was sent twice because of the outage)", view(func(*m2wView) {}), []m2oCase{
		{"a message sent twice", "want 1", view(func(v *m2wView) {
			v.Msgs[items[1].Email] = append(v.Msgs[items[1].Email], m2wSinkMsg{MessageID: "<x@example.test>"})
		})},
		{"a sent item with no message", "want 1", view(func(v *m2wView) { delete(v.Msgs, items[3].Email) })},
		{"a message for a bounced item", "want 0", view(func(v *m2wView) { v.Msgs[items[2].Email] = []m2wSinkMsg{{MessageID: "<y@example.test>"}} })},
		{"a message sent again with another Message-ID", "a message was sent again, or lost", view(func(v *m2wView) {
			v.Msgs[items[0].Email] = []m2wSinkMsg{{MessageID: "<other@example.test>"}}
		})},
	})
}

// tenantOf builds a tenant from the items of a fake state, for the controls.
func (p *m2oPhase1) tenantOf() *m2wTenant {
	q := *p
	q.UserID, q.CampaignID = "00000000-0000-4000-8000-0000000000aa", "00000000-0000-4000-8000-0000000000bb"
	return q.tenant()
}

// m2oControlState: the state a phase writes survives the trip through jsonb and back unchanged (a field that did not serialise would make a later phase compare against a zero), and a changed copy is not equal.
func m2oControlState(t *testing.T) {
	t.Helper()
	want := m2oPhase1{
		RunID: "ab12cd34", UserID: "u", CampaignID: "c", PoolID: "p", AccountID: "a", From: "f@example.test", Job: "j", Run: 10,
		Items:  []m2oItem{{Index: 1, ID: "i1", Email: "e1@example.test", Kind: "ok", JobID: "j1"}, {Index: 2, ID: "i2", Email: "e2@example.test", Kind: "bounce"}},
		Dials:  map[string]json.RawMessage{"job_max_failed_attempts": json.RawMessage(`2`), "retry_backoff_schedule": json.RawMessage(`{"ladder_seconds":[1,1,1],"max_seconds":1}`)},
		HoldMs: m2oHoldMs, Reserved: 22, UnitCost: 2, Totals: sendapi.Totals{Total: 10, Sent: 2, Bounced: 1, InFlight: 1, Pending: 6},
		Terminal: map[string]m2oTerm{"i1": {State: "sent"}, "i2": {State: "bounced", Reason: "bounce_permanent"}}, Settled: []string{"i1", "i2"}, ChargedRefs: []string{"i1", "i2"},
		Books:  m2oBooks{Ledger: 4, Charges: 2, Newest: 99, Available: 970, Reserved: 18, Rsv: 22, Chg: 4, JobStatus: "running", Items: "11:abc", Fingerprint: "4/99/970:18/22:4:0", SampledAt: "2026-11-18T10:00:00Z"},
		Module: m2oModule{Jobs: 2, IDs: map[string]string{"i1": "j1", "i2": "j2"}, States: map[string]string{"i1": "sent", "i2": "bounced"}},
		Sink:   map[string][]string{"e1@example.test": {"<m1@example.test>"}}, SampleAt: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), SinceRunSec: 6.5,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("control state: the state does not marshal: %v", err)
	}
	var got m2oPhase1
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("control state: the state does not unmarshal: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("control state: the state of phase 1 does not survive the trip through json unchanged: wrote %+v, read %+v", want, got)
	}
	changed := got
	changed.Books.Ledger++
	if reflect.DeepEqual(want, changed) {
		t.Errorf("control state: a changed copy compares equal to the original: the comparison cannot fail")
	}
	t.Logf("NOTE outage control state: the state of phase 1 survives the trip through json unchanged (%d bytes) and a changed copy is not equal", len(raw))
}

// m2oControls runs the controls of a phase (cheap, no stack).
func m2oControls(t *testing.T, phase int) {
	t.Helper()
	m2oControlState(t)
	switch phase {
	case 1:
		m2oControlPhase1(t)
	case 2:
		m2oControlPhase2(t)
	case 3:
		m2oControlPhase3(t)
	}
}
