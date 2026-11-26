//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// Helpers of the job-engine tests (jobs_engine_test.go: the engine-facing half of the store; jobs_engine_loop_test.go: the engine's loops against a fake module). Every name starts with `je` so that
// it cannot clash with the helpers of the other files of this package. The helpers do not use the job-store tests' helpers (jbs*): the two sets of tests are written and changed apart.
//
// The shape every test shares: a migrated scratch database of its own (newMnyEnv), a SUPERUSER connection to it (e.admin: seeding and observing, never the code under test), the money store and the
// job store under test over a pool of REAL logins as platform_app. Jobs are built through the real store (CreateDraft, Preview, Confirm) and not walked by hand, except where a test needs a state
// the store never produces (an administrator ending a job by hand, a deadline in the past): those use the fixtures of jobs_helpers_test.go.
//
// EVERY TEST STARTS WITH MODULE 1'S VENDOR UNIT OFF. The legacy money fixtures (newMnyEnv) switch m1_vendor ON, which would price every module 1 job here at 2 per row; newJeEnv switches it off through
// the one writer (platform.set_unit_reachable) and restores what it found when the test ends. A test that wants it on says so (e.vendor(true)).

type jeEnv struct {
	*mnyEnv
	jobs  *jobs.Store
	users *atomic.Int64 // the next fixture user, shared by every view of the environment (at) so that subtests never meet the same user twice
}

// newJeEnv migrates a scratch database and builds the job store over it with the options.
func newJeEnv(t *testing.T, opts ...jobs.Option) *jeEnv {
	t.Helper()
	e := &jeEnv{mnyEnv: newMnyEnv(t), users: new(atomic.Int64)}
	// the tests that hold the (user, module) lock and wait for a call to queue behind it need the store's wait to outlast the polling, whatever the runner's speed
	e.store = money.New(e.pool, money.WithLockTimeout(45*time.Second))
	st, err := jobs.New(e.pool, e.store, opts...)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	e.jobs = st
	was := ledgerBool(t, e.admin, "SELECT reachable FROM platform.unit_costs WHERE unit = 'm1_vendor'")
	e.vendor(false)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.admin.Exec(ctx, "SELECT platform.set_unit_reachable('m1_vendor', $1, 'test: the flag the engine test found')", was); err != nil {
			t.Errorf("cannot restore the vendor flag: %v", err)
		}
	})
	return e
}

// vendor switches module 1's vendor unit on or off through the one writer of the flag.
func (e *jeEnv) vendor(on bool) {
	e.t.Helper()
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', $1, 'test: the engine tests set the vendor flag')", on)
	if got := ledgerBool(e.t, e.admin, "SELECT reachable FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != on {
		e.t.Fatalf("m1_vendor reachable = %v after asking for %v", got, on)
	}
}

// at returns the same environment reporting to t (a subtest must fail ITS OWN T, never its parent's).
func (e *jeEnv) at(t *testing.T) *jeEnv {
	return &jeEnv{mnyEnv: e.mnyEnv.at(t), jobs: e.jobs, users: e.users}
}

var jeKeySeq atomic.Int64

// key is a fresh, valid confirm key.
func (e *jeEnv) key() string { return fmt.Sprintf("je-key-%d", jeKeySeq.Add(1)) }

// newUser inserts a fixture customer and returns the id.
func (e *jeEnv) newUser() string {
	e.t.Helper()
	n := e.users.Add(1)
	id := fmt.Sprintf("e0000000-0000-4000-8000-%012d", n)
	e.exec("INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, fmt.Sprintf("je-%d@example.test", n))
	e.exec("INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", id, fmt.Sprintf("je-%d@example.test", n))
	return id
}

// jeAddrs makes n distinct, valid addresses: <tag>00001@example.test, ...
func jeAddrs(n int, tag string) []jobs.RowIn {
	out := make([]jobs.RowIn, n)
	for i := range out {
		out[i] = jobs.RowIn{Email: fmt.Sprintf("%s%05d@example.test", tag, i+1)}
	}
	return out
}

// jeContent is a valid campaign with a blank plain-text part: public.job_items.rendered_text must be NULL for it.
func jeContent() *jobs.Content {
	return &jobs.Content{Subject: "Hello from the engine tests", HTML: "<p>hello</p>", Text: "", ReplyTo: "reply@example.test"}
}

// enableM3 enables module 3 for the user through the ledger (the only writer of the entitlement).
func (e *jeEnv) enableM3(user string) {
	e.t.Helper()
	e.exec(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key, until_at)
		VALUES ($1, 'm3', 'm3_enable', 0, 0, $2, 'test entitlement', $3, now() + interval '1 hour')`, user, mnyAdminA, e.key())
}

// confirmOpts are the choices of a confirmed fixture job.
type jeConfirmOpts struct {
	Content *jobs.Content // nil: jeContent() for a send module
	Start   *time.Time    // a scheduled start
	Fund    int64         // credits to grant first (0: 1000 for a metered module)
}

// confirmRows creates, previews and confirms a job over the rows for a user who is funded (or, for module 3, entitled), through the real store, and returns the job as it is after the confirm.
func (e *jeEnv) confirmRows(user, module string, rows []jobs.RowIn, o ...jeConfirmOpts) jobs.Job {
	e.t.Helper()
	var opt jeConfirmOpts
	if len(o) > 0 {
		opt = o[0]
	}
	switch module {
	case jobs.ModuleSendBYO:
		e.enableM3(user)
	default:
		fund := opt.Fund
		if fund == 0 {
			fund = 1000
		}
		e.fund(user, module, fund)
	}
	req := jobs.CreateReq{Module: module, Rows: rows}
	if module != jobs.ModuleVerify {
		req.Content = opt.Content
		if req.Content == nil {
			req.Content = jeContent()
		}
	}
	ctx := testCtx(e.t)
	j, err := e.jobs.CreateDraft(ctx, user, req)
	if err != nil {
		e.t.Fatalf("CreateDraft(%s, %d rows): %v", module, len(rows), err)
	}
	p, err := e.jobs.Preview(ctx, user, j.ID)
	if err != nil {
		e.t.Fatalf("Preview(%s): %v", j.ID, err)
	}
	res, err := e.jobs.Confirm(ctx, user, jobs.ConfirmReq{JobID: j.ID, Fingerprint: p.Fingerprint, Key: e.key(), ScheduledStart: opt.Start})
	if err != nil {
		e.t.Fatalf("Confirm(%s): %v", j.ID, err)
	}
	return res.Job
}

// confirmN is confirmRows over n distinct valid addresses.
func (e *jeEnv) confirmN(user, module string, n int, o ...jeConfirmOpts) jobs.Job {
	e.t.Helper()
	return e.confirmRows(user, module, jeAddrs(n, "r"), o...)
}

// running is a confirmed job that the engine has started (no module reference).
func (e *jeEnv) running(user, module string, n int) jobs.Job {
	e.t.Helper()
	j := e.confirmN(user, module, n)
	r, err := e.jobs.Start(testCtx(e.t), j.ID, "")
	if err != nil {
		e.t.Fatalf("Start(%s): %v", j.ID, err)
	}
	if r.Status != jobs.StatusRunning {
		e.t.Fatalf("Start left the job %s", r.Status)
	}
	return r
}

// rowIDs are the ids of the job's rows in the given state ("" for every row), by ordinal.
func (e *jeEnv) rowIDs(job, state string) []string {
	e.t.Helper()
	return e.rows(`SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND ($2 = '' OR state = $2) ORDER BY ordinal`, job, state)
}

// work is EngineWork, failing the test on an error.
func (e *jeEnv) work(limit int) []jobs.EngineJob {
	e.t.Helper()
	w, err := e.jobs.EngineWork(testCtx(e.t), limit)
	if err != nil {
		e.t.Fatalf("EngineWork(%d): %v", limit, err)
	}
	return w
}

// workOf is the job's entry of EngineWork, if it has one.
func (e *jeEnv) workOf(job string) (jobs.EngineJob, bool) {
	e.t.Helper()
	for _, j := range e.work(1000) {
		if j.ID == job {
			return j, true
		}
	}
	return jobs.EngineJob{}, false
}

// ---- items the module reports ----

func jeItem(ref string, o charge.Outcome) jobs.ResultItem {
	return jobs.ResultItem{ItemRef: ref, Outcome: o, Detail: map[string]any{"k": "v"}}
}

var (
	jeM2Sent    = charge.Outcome{Module: "m2", State: "sent"}
	jeM2Bounced = charge.Outcome{Module: "m2", State: "bounced"}
	jeM2Failed  = charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"}
	jeM2Cancel  = charge.Outcome{Module: "m2", State: "cancelled"}
	jeM2Queued  = charge.Outcome{Module: "m2", State: "queued"}
)

// record is RecordResults, failing the test on an error.
func (e *jeEnv) record(job string, items ...jobs.ResultItem) jobs.RecordSummary {
	e.t.Helper()
	s, err := e.jobs.RecordResults(testCtx(e.t), job, items)
	if err != nil {
		e.t.Fatalf("RecordResults(%s, %d items): %v", job, len(items), err)
	}
	return s
}

// ---- observing the tables ----

func (e *jeEnv) resultCount(job string) int64 {
	return e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", job)
}

func (e *jeEnv) releaseRows(job string) int64 {
	return e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", job)
}

func (e *jeEnv) eventCount(job, kind string) int64 {
	return e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = $2", job, kind)
}

func (e *jeEnv) transitionCount(job, to string) int64 {
	return e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND to_status = $2", job, to)
}

// reserveOf is what the job's reserve held, charged and released.
func (e *jeEnv) reserveOf(job string) (reserved, charged, released int64) {
	e.t.Helper()
	if err := e.admin.QueryRow(testCtx(e.t), "SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1", job).Scan(&reserved, &charged, &released); err != nil {
		e.t.Fatalf("the reserve of job %s: %v", job, err)
	}
	return reserved, charged, released
}

func (e *jeEnv) remainder(job string) int64 {
	r, c, l := e.reserveOf(job)
	return r - c - l
}

// ledgerCharges is {item_ref: unit} of the job's charge rows; resultUnits is the same read from the job's results.
func (e *jeEnv) ledgerCharges(job string) map[string]string {
	return e.pairs("SELECT item_ref, unit FROM platform.ledger WHERE job_id = $1 AND kind = 'charge'", job)
}

func (e *jeEnv) resultUnits(job string) map[string]string {
	return e.pairs("SELECT item_ref::text, unit FROM platform.job_results WHERE job_id = $1 AND unit IS NOT NULL", job)
}

func (e *jeEnv) pairs(sql string, args ...any) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, r := range e.rows(sql, args...) {
		k, v, _ := strings.Cut(r, "|")
		out[k] = v
	}
	return out
}

// requireBooks fails when the balance invariant, the per-reserve invariant or the settlement proof (ledger, results and rows agree) is broken.
func (e *jeEnv) requireBooks(when string) {
	e.t.Helper()
	e.requireViewsEmpty(when)
	if d := e.rows(`SELECT job_id::text, COALESCE(item_ref, '-'), problem FROM platform.job_settlement_drift ORDER BY 1, 2, 3`); len(d) != 0 {
		e.t.Fatalf("%s: platform.job_settlement_drift is not empty (job|item|problem): %v", when, d)
	}
}

// fingerprint is a digest of everything an engine step could write: the money state and the job tables. Two equal values mean that nothing was written between them.
func (e *jeEnv) fingerprint() string {
	e.t.Helper()
	return e.state() + "/" + e.str(`SELECT (SELECT count(*) FROM platform.job_events)::text || ':' || (SELECT count(*) FROM platform.job_results)::text
		|| ':' || (SELECT count(*) FROM public.job_items)::text
		|| ':' || COALESCE((SELECT md5(string_agg(id::text || status || COALESCE(module_job_ref, '') || COALESCE(last_progress_at::text, '') || COALESCE(stale_alerted_at::text, ''), ',' ORDER BY id)) FROM platform.jobs), '-')
		|| ':' || COALESCE((SELECT md5(string_agg(id::text || state || COALESCE(submitted_at::text, ''), ',' ORDER BY id)) FROM platform.job_rows), '-')`)
}

// ---- asserting the store's answers ----

func jeIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got error %v, want errors.Is(%v)", what, err, want)
	}
}

// jeNotAllowed asserts the error is ErrNotAllowed with one of the reasons (the database's constraint name, or the store's word) and no raw database error.
func jeNotAllowed(t *testing.T, what string, err error, reasons ...string) {
	t.Helper()
	var na *jobs.NotAllowedError
	if !errors.Is(err, jobs.ErrNotAllowed) || !errors.As(err, &na) || !containsString(reasons, na.Reason) {
		t.Fatalf("%s: got %v, want ErrNotAllowed with the reason %v", what, err, reasons)
	}
	jeNoRawSQL(t, what, err)
}

// jeNoRawSQL fails when a driver's database error is still reachable in the chain: a refusal must arrive typed.
func jeNoRawSQL(t *testing.T, what string, err error) {
	t.Helper()
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		t.Fatalf("%s: a raw database error escaped (SQLSTATE %s, constraint %q): %v", what, pe.Code, pe.ConstraintName, err)
	}
}

// ---- the engine and its doubles ----

// jeRig is one engine over the environment's job store and a fake module, with a gate and a clock the test moves.
type jeRig struct {
	e     *jeEnv
	fake  *jobenginetest.FakeModule
	mods  *jobenginetest.Modules
	gate  *jobenginetest.Gate
	clock *jobenginetest.Clock
	eng   *jobengine.Engine
	cfg   jobengine.Config
}

// newRig builds an engine over a fake module that plays the module id. The clock starts at the real time (the database's last_progress_at is real), the gate leads, and SettleEvery is negative (the
// engine observes at every tick: the tests drive time themselves).
func (e *jeEnv) newRig(fake *jobenginetest.FakeModule, cfg jobengine.Config) *jeRig {
	e.t.Helper()
	if cfg.SettleEvery == 0 {
		cfg.SettleEvery = -1
	}
	r := &jeRig{e: e, fake: fake, mods: &jobenginetest.Modules{M: fake}, gate: jobenginetest.NewGate(true), clock: jobenginetest.NewClock(time.Now()), cfg: cfg}
	r.eng = e.engineOf(r.mods, r.gate, r.clock, cfg)
	return r
}

// engineOf builds a further engine (a second process) over the same database.
func (e *jeEnv) engineOf(mods jobengine.Modules, gate jobengine.Gate, clock *jobenginetest.Clock, cfg jobengine.Config) *jobengine.Engine {
	e.t.Helper()
	eng, err := jobengine.New(jobengine.Deps{Store: e.jobs, Modules: mods, Leader: gate, Now: clock.Now}, cfg)
	if err != nil {
		e.t.Fatalf("jobengine.New: %v", err)
	}
	return eng
}

// tickUntil ticks the engine until the job has the status or max ticks have passed (which fails the test, naming where the job is and what the engine did). It returns the ticks it took.
func (e *jeEnv) tickUntil(eng *jobengine.Engine, job, status string, max int) int {
	e.t.Helper()
	for i := 0; i < max; i++ {
		if got := e.jobStatus(job); got == status {
			return i
		}
		eng.Tick(testCtx(e.t))
	}
	if got := e.jobStatus(job); got != status {
		e.t.Fatalf("after %d ticks job %s is %s, want %s (engine stats %+v)", max, job, got, status, eng.Stats())
	}
	return max
}

// expectedCharges is what a correct platform must have charged for the items the fake module finished: {item_ref: unit}, from the charge table.
func jeExpectedCharges(fake *jobenginetest.FakeModule, job string) map[string]string {
	out := map[string]string{}
	for ref, o := range fake.FinalOutcomes(job) {
		if d := charge.Decide(o); d.Kind == charge.Charge {
			out[ref] = d.Unit
		}
	}
	return out
}

// requireSettledLikeTheModule is the three-way proof of Locked 25 for one job: the ledger's charge rows, the results that name a unit and the charges the fake module's report implies are the same set
// of (item, unit), and every item the module finished has exactly one result of origin module. It fails the test naming the difference.
func (e *jeEnv) requireSettledLikeTheModule(fake *jobenginetest.FakeModule, job string) {
	e.t.Helper()
	want, ledger, results := jeExpectedCharges(fake, job), e.ledgerCharges(job), e.resultUnits(job)
	jeSameMap(e.t, "the module's report against the ledger", want, ledger)
	jeSameMap(e.t, "the ledger against the job_results that name a unit", ledger, results)
	final := fake.FinalOutcomes(job)
	if got := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'module'", job); got != int64(len(final)) {
		e.t.Fatalf("the module finished %d items and the job has %d results of origin module", len(final), got)
	}
	for ref, o := range final {
		var state, origin string
		if err := e.admin.QueryRow(testCtx(e.t), "SELECT module_state, origin FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", job, ref).Scan(&state, &origin); err != nil {
			e.t.Fatalf("item %s finished %s in the module and has no result: %v", ref, o.State, err)
		}
		if origin != "module" || state != o.State {
			e.t.Fatalf("item %s: the module says %s, the result says %s from %s", ref, o.State, state, origin)
		}
	}
}

func jeSameMap(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	if len(a) == len(b) {
		same := true
		for k, v := range a {
			if b[k] != v {
				same = false
			}
		}
		if same {
			return
		}
	}
	t.Fatalf("%s differ: %s  <>  %s", what, jeShow(a), jeShow(b))
}

func jeShow(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k[:min(8, len(k))]+"="+m[k])
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d{%s}", len(m), strings.Join(keys, ","))
}

// requireEnded is what a finished metered job must look like: the status, a finished_at, the whole reserve accounted for (charged + released = reserved, one release row when anything was left) and
// the books in order.
func (e *jeEnv) requireEnded(job, status string, wantReleaseRows int64) {
	e.t.Helper()
	if got := e.jobStatus(job); got != status {
		e.t.Fatalf("job %s is %s, want %s", job, got, status)
	}
	if !ledgerBool(e.t, e.admin, "SELECT finished_at IS NOT NULL FROM platform.jobs WHERE id = $1", job) {
		e.t.Fatalf("job %s ended %s with no finished_at", job, status)
	}
	if got := e.releaseRows(job); got != wantReleaseRows {
		e.t.Fatalf("job %s has %d release rows, want %d", job, got, wantReleaseRows)
	}
	if rem := e.remainder(job); rem != 0 {
		e.t.Fatalf("job %s ended with %d credits still held by its reserve", job, rem)
	}
	e.requireBooks("job " + job + " ended " + status)
}

// requireBlockedOnTheLock runs call on another connection while the test holds the (user, module) advisory lock, PROVES from the server that the call is blocked on that very lock (a platform_app
// backend waiting on an advisory lock held by the test, whose current statement is the store's pg_advisory_xact_lock) and has not returned, then lets go and returns the call's outcome. A test that
// hopes for contention proves nothing.
func (e *jeEnv) requireBlockedOnTheLock(user, module string, call func(ctx context.Context) error) error {
	t := e.t
	t.Helper()
	h := e.comHold(user, module)
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		done <- call(ctx)
	}()
	ws, ok := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		for _, w := range ws {
			if w.Event == "advisory" && mnyHasPID(w.Blockers, h.pid) && strings.Contains(w.Query, "pg_advisory_xact_lock") {
				return true
			}
		}
		return false
	})
	if !ok {
		h.release()
		t.Fatalf("the call was never seen blocked on the (user, module) advisory lock behind the test's holder: %s", mnyDescribe(ws))
	}
	select {
	case err := <-done:
		h.release()
		t.Fatalf("the call returned (%v) while the test still held the lock the store takes first", err)
	default:
	}
	h.requireStillHeld("while the engine call was blocked")
	h.release()
	select {
	case err := <-done:
		return err
	case <-time.After(120 * time.Second):
		t.Fatalf("the call did not return within 120s after the test let go")
	}
	return nil
}

// ageProgress moves the job's last_progress_at into the past BY THE DATABASE'S CLOCK: the engine judges how long a job has been silent against the database's now() and the database's last_progress_at (EngineJob.DBNow), so a test that wants the staleness limit to pass advances the stamp, not only the engine's injected clock (which still throttles how often a flag is renewed).
func (e *jeEnv) ageProgress(job string, by time.Duration) {
	e.t.Helper()
	if _, err := e.admin.Exec(testCtx(e.t), "UPDATE platform.jobs SET last_progress_at = now() - make_interval(secs => $2::float8) WHERE id = $1::uuid", job, by.Seconds()); err != nil {
		e.t.Fatalf("could not age the progress stamp of job %s: %v", job, err)
	}
}
