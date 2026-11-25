//go:build harness

package harness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 chunks B7 to B9, the engine (internal/jobengine) against a REAL Postgres, driven tick by tick (Engine.Tick) over a deterministic fake module (jobenginetest.FakeModule) and a gate the test
// flips. Every job is built through the real store; the money is the real money store; the fake is the only double, and what it reports is what the ledger, the results and the job must equal. Helpers:
// jobs_engine_helpers_test.go. Every test starts with module 1's vendor unit OFF and has a paired control.
//
//	Module1Shape / Module2Shape           the happy paths: one Submit holding every row and paged observation; batches, partial acceptance and a "campaign active" wait
//	CrashAndRestart...                    for EVERY module-call boundary of a run: the first engine dies (before call n+1, or right after call n), a second engine on the same database finishes the job
//	AModuleOutage...                      the module is down: the job does not move, nothing is invented, the stale event comes only after the limit, and the job completes once after recovery
//	TwoEngines...                         only the leader's calls reach the module; a handover finishes the job; two leaders (the control) still settle once
//	AStartDeadline...                     a module that never answers: the job expires at its deadline and the reserve goes back
//	Cancelling...                         the in-flight item is charged once, the cancelled ones are free, the job ends cancelled; module 1 cannot be cancelled
//	AModuleComplete...                    a module that says complete with a result missing: not completed, flagged, stale after the limit, completed when the result arrives
//	NotFinal / Repair / Refused           what the engine never invents, the repair pass, a module that refuses the job

// The module-2 shaped fixture of the crash tests: five pending rows, a malformed one and a duplicate; the module takes two rows per call and finishes two items per observation.
func jeCrashFake() *jobenginetest.FakeModule {
	return &jobenginetest.FakeModule{AcceptPerCall: 2, FinalizePerObserve: 2, PageSize: 2, Plan: func(r jobs.EngineRow) charge.Outcome {
		switch {
		case r.Ordinal == 4:
			return charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"} // free: one credit of the reserve is left to release
		case r.Ordinal%2 == 0:
			return charge.Outcome{Module: "m2", State: "bounced"}
		}
		return charge.Outcome{Module: "m2", State: "sent"}
	}}
}

// jePlan finishes every row sent except the one at freeOrdinal, which fails (free): a job that ends with one credit of its reserve left, so that the release is a real write.
func jePlan(freeOrdinal int) func(jobs.EngineRow) charge.Outcome {
	return func(r jobs.EngineRow) charge.Outcome {
		if r.Ordinal == freeOrdinal {
			return charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"}
		}
		return charge.Outcome{Module: "m2", State: "sent"}
	}
}

func jeCrashRows() []jobs.RowIn {
	return append(jeAddrs(5, "c"), jobs.RowIn{Email: "oops, not an address"}, jobs.RowIn{Email: "C00001@example.test"})
}

// requireFinishedOnce is what a finished job must look like whatever happened to the engines on the way: one move to running, one to completing, one to the end, every row answered exactly once, the ledger
// and the results equal to what the module reported, the whole reserve accounted for with one release, every pending row stamped and handed over once.
func (e *jeEnv) requireFinishedOnce(fake *jobenginetest.FakeModule, job, status string, rows, pending, items int) {
	e.t.Helper()
	e.requireEnded(job, status, 1)
	e.requireSettledLikeTheModule(fake, job)
	if got := e.resultCount(job); got != int64(rows) {
		e.t.Fatalf("job %s has %d results for %d rows", job, got, rows)
	}
	if got := e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' AND submitted_at IS NULL", job); got != 0 {
		e.t.Fatalf("job %s ended with %d pending rows never stamped as submitted", job, got)
	}
	if got := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", job); got != int64(items) {
		e.t.Fatalf("job %s has %d public.job_items rows, want %d (one per pending row of a send job, none for module 1)", job, got, items)
	}
	for _, to := range []string{jobs.StatusRunning, jobs.StatusCompleting, status} {
		if n := e.transitionCount(job, to); n != 1 {
			e.t.Fatalf("job %s has %d moves to %s, want exactly 1", job, n, to)
		}
	}
	if got := fake.HeldCount(job); got != pending {
		e.t.Fatalf("the module holds %d items of job %s, want %d (no row lost, none held twice)", got, job, pending)
	}
}

func TestJobEngineLoop_Module1ShapeOneSubmitHoldsEveryRowAndTheLedgerEqualsTheReport(t *testing.T) {
	e := newJeEnv(t)
	u := e.newUser()
	rows := append(jeAddrs(4, "m"), jobs.RowIn{Email: "no at sign"}, jobs.RowIn{Email: "M00001@example.test"}) // 4 pending, a malformed row, a repeat of the first
	j := e.confirmRows(u, "m1", rows)
	fake := &jobenginetest.FakeModule{Module: "m1", PageSize: 2, Plan: func(r jobs.EngineRow) charge.Outcome {
		switch r.Ordinal {
		case 1:
			return charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"} // m1_own
		case 2:
			return charge.Outcome{Module: "m1", State: "valid", Reason: "accepted", FromCache: true} // m1_cache
		case 3:
			return charge.Outcome{Module: "m1", State: "invalid", Reason: "no_mx"} // m1_own
		}
		return charge.Outcome{Module: "m1", State: "unknown", Reason: "unknown_error", FailureOrigin: "own"} // free: our own failure
	}}
	rig := e.newRig(fake, jobengine.Config{SubmitBatchFor: map[string]int{"m1": 1000}})

	ticks := e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	if ticks > 3 {
		t.Errorf("the job needed %d ticks: a start, an observation that finishes it", ticks)
	}
	if c := fake.Calls(); c.Submit != 1 || c.Cancel != 0 || c.Observe < 1 {
		t.Errorf("module calls %+v: one Submit holds the whole list", c)
	}
	if ref := e.str("SELECT module_job_ref FROM platform.jobs WHERE id = $1", j.ID); ref != "fake-verify-"+j.ID {
		t.Errorf("module_job_ref = %q: the module's handle from the first Submit", ref)
	}
	e.requireFinishedOnce(fake, j.ID, jobs.StatusSucceeded, 6, 4, 0)
	if got := e.ledgerCharges(j.ID); len(got) != 3 {
		t.Errorf("%d charges, want 3 (the free unknown is not charged): %v", len(got), got)
	}
	units := map[string]int{}
	for _, unit := range e.ledgerCharges(j.ID) {
		units[unit]++
	}
	if units["m1_own"] != 2 || units["m1_cache"] != 1 {
		t.Errorf("units charged: %v, want 2 m1_own and 1 m1_cache", units)
	}
	if r, c, l := e.reserveOf(j.ID); r != 4 || c != 3 || l != 1 {
		t.Errorf("reserve held %d, charged %d, released %d; want 4, 3 and 1", r, c, l)
	}
	if s := rig.eng.Stats(); s.Unexpected != 0 || s.Waits != 0 || s.Panics != 0 {
		t.Errorf("engine stats %+v: a clean run counts nothing unexpected", s)
	}
	// the job is done: another tick makes no call and writes nothing
	before, calls := e.fingerprint(), fake.Calls()
	rig.eng.Tick(testCtx(t))
	if e.fingerprint() != before || fake.Calls() != calls {
		t.Error("a tick after the end of the job did something")
	}
}

func TestJobEngineLoop_Module2ShapeBatchesPartialAcceptanceAndACampaignActiveWait(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmRows(u, "m2", jeAddrs(6, "b"))
	fake := &jobenginetest.FakeModule{AcceptPerCall: 2, WaitSubmits: 1, FinalizePerObserve: 3, PageSize: 2, Plan: func(r jobs.EngineRow) charge.Outcome {
		switch {
		case r.Ordinal == 5:
			return charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"} // free: our failure
		case r.Ordinal%2 == 0:
			return charge.Outcome{Module: "m2", State: "bounced"}
		}
		return charge.Outcome{Module: "m2", State: "sent"}
	}}
	rig := e.newRig(fake, jobengine.Config{SubmitBatch: 3})

	// the first tick: the module answers "a campaign is active" to the first Submit. The job stays confirmed, nothing is stamped, the rows of the first batch were prepared for the module
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusConfirmed || fake.HeldCount(j.ID) != 0 || rig.eng.Stats().Waits != 1 {
		t.Fatalf("after the wait: %s, the module holds %d, stats %+v", e.jobStatus(j.ID), fake.HeldCount(j.ID), rig.eng.Stats())
	}
	if e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", j.ID) != 0 || e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID) != 3 {
		t.Fatal("after the wait: rows were stamped, or the first batch of public.job_items is not there")
	}
	if e.resultCount(j.ID) != 0 || e.chargeRows(j.ID) != 0 {
		t.Fatal("after the wait: something was recorded or charged")
	}
	// the module answers now: it takes two rows per call, so the job starts with two and the rest follows
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 2 || e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", j.ID) != 2 {
		t.Fatalf("after the first accepted batch: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 20)
	e.requireFinishedOnce(fake, j.ID, jobs.StatusSucceeded, 6, 6, 6)
	if got := e.ledgerCharges(j.ID); len(got) != 5 {
		t.Errorf("%d charges, want 5 (the failed item is free): %v", len(got), got)
	}
	units := map[string]int{}
	for _, unit := range e.ledgerCharges(j.ID) {
		units[unit]++
	}
	if units["m2_accepted"] != 2 || units["m2_bounced"] != 3 {
		t.Errorf("units charged: %v, want 2 m2_accepted and 3 m2_bounced", units)
	}
	if r, c, l := e.reserveOf(j.ID); r != 6 || c != 5 || l != 1 {
		t.Errorf("reserve held %d, charged %d, released %d; want 6, 5 and 1", r, c, l)
	}
	if s := rig.eng.Stats(); s.Waits != 1 || s.Unexpected != 0 {
		t.Errorf("engine stats %+v: one wait and nothing unexpected", s)
	}
	for _, id := range e.rowIDs(j.ID, "pending") {
		if fake.Held(id) < 1 {
			t.Errorf("row %s was never held by the module", id)
		}
	}
}

// jeCrashRun runs the crash scenario once. crashAt < 0: no crash (the clean run: it returns how many module calls the whole job took). Otherwise the first engine dies BEFORE module call crashAt+1
// (afterCall false) or right AFTER module call crashAt (afterCall true): the module cancels the context of the engine, whose next database statement therefore fails, and the engine is abandoned (it
// is never ticked again: the process died). A SECOND engine, built fresh on the same database (no memory of the first), then ticks until the job is done.
func jeCrashRun(t *testing.T, e *jeEnv, crashAt int, afterCall bool) int {
	t.Helper()
	user := e.newUser()
	rows := jeCrashRows()
	j := e.confirmRows(user, "m2", rows)
	fake := jeCrashFake()
	cfg := jobengine.Config{SubmitBatch: 3}

	ctx1, cancel1 := context.WithCancel(testCtx(t))
	defer cancel1()
	first := e.newRig(fake, cfg)
	if crashAt >= 0 {
		fake.Arm(crashAt, afterCall, cancel1)
	}
	for i := 0; i < 15 && ctx1.Err() == nil && e.jobStatus(j.ID) != jobs.StatusSucceeded; i++ {
		first.eng.Tick(ctx1)
	}
	if crashAt < 0 {
		if e.jobStatus(j.ID) != jobs.StatusSucceeded {
			t.Fatalf("the clean run did not finish: %s", e.jobStatus(j.ID))
		}
		e.requireFinishedOnce(fake, j.ID, jobs.StatusSucceeded, len(rows), 5, 5)
		return fake.Calls().Total
	}
	if fake.Armed() || ctx1.Err() == nil {
		t.Fatalf("the crash never happened (armed %v, engine cancelled %v): the run proves nothing", fake.Armed(), ctx1.Err() != nil)
	}
	if e.jobStatus(j.ID) == jobs.StatusSucceeded {
		t.Fatal("the job finished before the engine died: the crash point is not inside the run")
	}
	fake.Disarm()

	second := e.newRig(fake, cfg) // a fresh engine, a fresh gate and clock: nothing of the first one's memory
	e.tickUntil(second.eng, j.ID, jobs.StatusSucceeded, 30)
	e.requireFinishedOnce(fake, j.ID, jobs.StatusSucceeded, len(rows), 5, 5)
	return fake.Calls().Total
}

func TestJobEngineLoop_CrashAndRestartAtEveryModuleCallBoundaryLosesDuplicatesAndDoubleChargesNothing(t *testing.T) {
	e := newJeEnv(t)
	k := jeCrashRun(t, e.at(t), -1, false)
	if k < 6 {
		t.Fatalf("the clean run made only %d module calls: the matrix below would prove too little", k)
	}
	t.Logf("a clean run makes %d module calls; the matrix kills the first engine before each of calls 1..%d and right after each of calls 1..%d", k, k, k)
	for n := 0; n < k; n++ {
		n := n
		t.Run(fmt.Sprintf("dies_before_call_%d", n+1), func(t *testing.T) { jeCrashRun(t, e.at(t), n, false) })
	}
	for n := 1; n <= k; n++ {
		n := n
		t.Run(fmt.Sprintf("dies_right_after_call_%d", n), func(t *testing.T) { jeCrashRun(t, e.at(t), n, true) })
	}
}

func TestJobEngineLoop_AModuleOutageHaltsTheJobInventsNothingRaisesStaleOnlyAfterTheLimitAndTheJobCompletesOnceAfterRecovery(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 4)
	fake := &jobenginetest.FakeModule{AcceptPerCall: 2, Plan: jePlan(4)}
	rig := e.newRig(fake, jobengine.Config{SubmitBatch: 4, StaleAfter: map[string]time.Duration{"m2": time.Hour}})

	rig.eng.Tick(ctx) // the job starts with two rows held by the module
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 2 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}

	fake.SetDown(true)
	callsBefore := fake.Calls().Total
	before := e.fingerprint()
	for i := 0; i < 5; i++ {
		rig.eng.Tick(ctx)
	}
	if fake.Calls().Total <= callsBefore {
		t.Fatal("the engine did not even try the module during the outage: the test would prove nothing")
	}
	if e.fingerprint() != before {
		t.Fatal("during the outage something was written: the job moved, a result or a charge was invented, or progress was noted")
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning || e.resultCount(j.ID) != 0 || e.chargeRows(j.ID) != 0 || e.eventCount(j.ID, "stale") != 0 {
		t.Fatal("during the outage inside the staleness limit: the job moved, recorded something, or raised a stale event")
	}
	if s := rig.eng.Stats(); s.Waits < 5 || s.Unexpected != 0 {
		t.Errorf("engine stats %+v: every tick of the outage is a wait", s)
	}

	// the limit passes by the engine's clock: ONE stale event, and the job is still running (staleness never fails or invents anything)
	rig.clock.Advance(2 * time.Hour)
	rig.eng.Tick(ctx)
	rig.eng.Tick(ctx)
	if e.eventCount(j.ID, "stale") != 1 {
		t.Fatalf("%d stale events after the limit, want exactly 1 (once per interval)", e.eventCount(j.ID, "stale"))
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning || e.resultCount(j.ID) != 0 || e.chargeRows(j.ID) != 0 {
		t.Fatal("a stale job was moved or settled")
	}

	// the module comes back: the job completes, exactly once
	fake.SetDown(false)
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
	if e.resultCount(j.ID) != 4 || e.transitionCount(j.ID, jobs.StatusSucceeded) != 1 || e.eventCount(j.ID, "stale") != 1 {
		t.Fatalf("after the recovery: %d results, %d moves to succeeded, %d stale events; want 4, 1 and 1", e.resultCount(j.ID), e.transitionCount(j.ID, jobs.StatusSucceeded), e.eventCount(j.ID, "stale"))
	}
}

// jeCountingModules counts the calls that reach the module through ONE engine: the proof of which engine talked to the module.
type jeCountingModules struct {
	inner jobengine.Modules
	calls atomic.Int64
}

func (m *jeCountingModules) For(ctx context.Context, user, module string) (jobengine.Module, error) {
	mod, err := m.inner.For(ctx, user, module)
	if err != nil {
		return nil, err
	}
	return &jeCountingModule{Module: mod, n: &m.calls}, nil
}

type jeCountingModule struct {
	jobengine.Module
	n *atomic.Int64
}

func (c *jeCountingModule) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	c.n.Add(1)
	return c.Module.Submit(ctx, j, rows)
}

func (c *jeCountingModule) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	c.n.Add(1)
	return c.Module.Observe(ctx, j, fn)
}

func (c *jeCountingModule) Cancel(ctx context.Context, j jobs.EngineJob) error {
	c.n.Add(1)
	return c.Module.Cancel(ctx, j)
}

func TestJobEngineLoop_TwoEnginesOnOneDatabaseOnlyTheLeaderCallsTheModuleAndAHandoverSettlesOnce(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	fake := jeCrashFake()
	mods := &jobenginetest.Modules{M: fake}
	cmA, cmB := &jeCountingModules{inner: mods}, &jeCountingModules{inner: mods}
	gateA, gateB := jobenginetest.NewGate(true), jobenginetest.NewGate(false)
	clock := jobenginetest.NewClock(time.Now())
	engA, engB := e.engineOf(cmA, gateA, clock, jobengine.Config{SubmitBatch: 3, SettleEvery: -1}), e.engineOf(cmB, gateB, clock, jobengine.Config{SubmitBatch: 3, SettleEvery: -1})

	// 1. A leads, B does not: B makes no module call, does not even read the work, and A finishes the job
	j1 := e.confirmRows(e.newUser(), "m2", jeCrashRows())
	for i := 0; i < 15 && e.jobStatus(j1.ID) != jobs.StatusSucceeded; i++ {
		engA.Tick(ctx)
		engB.Tick(ctx)
	}
	e.requireFinishedOnce(fake, j1.ID, jobs.StatusSucceeded, 7, 5, 5)
	if cmB.calls.Load() != 0 || engB.Stats().Ticks != 0 || engB.Stats().Standby < 1 || engB.Stats().Steps != 0 {
		t.Fatalf("the standby acted: %d module calls, stats %+v", cmB.calls.Load(), engB.Stats())
	}
	if cmA.calls.Load() != int64(fake.Calls().Total) || cmA.calls.Load() == 0 {
		t.Fatalf("A made %d calls, the module saw %d", cmA.calls.Load(), fake.Calls().Total)
	}

	// 2. a handover in the middle of a job: A runs two ticks and is abandoned (it simply stops being ticked), the gates flip, B finishes the job
	j2 := e.confirmRows(e.newUser(), "m2", jeCrashRows())
	engA.Tick(ctx)
	engA.Tick(ctx)
	if e.jobStatus(j2.ID) != jobs.StatusRunning {
		t.Fatalf("the fixture: the job is %s after two ticks, it must be mid-run for the handover to mean something", e.jobStatus(j2.ID))
	}
	aCalls, bCalls := cmA.calls.Load(), cmB.calls.Load()
	gateA.Set(false)
	gateB.Set(true)
	for i := 0; i < 15 && e.jobStatus(j2.ID) != jobs.StatusSucceeded; i++ {
		engA.Tick(ctx)
		engB.Tick(ctx)
	}
	e.requireFinishedOnce(fake, j2.ID, jobs.StatusSucceeded, 7, 5, 5)
	if cmA.calls.Load() != aCalls || cmB.calls.Load() <= bCalls {
		t.Fatalf("after the handover A made %d more calls and B %d; want none and some", cmA.calls.Load()-aCalls, cmB.calls.Load()-bCalls)
	}

	// 3. the control: BOTH engines lead, at the same time. The module sees both, and the database still settles once (the advisory lock and the keys are what keep it safe, not the gate)
	gateA.Set(true)
	j3 := e.confirmRows(e.newUser(), "m2", jeCrashRows())
	aCalls, bCalls = cmA.calls.Load(), cmB.calls.Load()
	for i := 0; i < 15 && e.jobStatus(j3.ID) != jobs.StatusSucceeded; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); engA.Tick(ctx) }()
		go func() { defer wg.Done(); engB.Tick(ctx) }()
		wg.Wait()
	}
	if cmA.calls.Load() <= aCalls || cmB.calls.Load() <= bCalls {
		t.Fatalf("the control did not have two active engines (A %d calls, B %d)", cmA.calls.Load()-aCalls, cmB.calls.Load()-bCalls)
	}
	e.requireFinishedOnce(fake, j3.ID, jobs.StatusSucceeded, 7, 5, 5)
	if s := engA.Stats(); s.Panics != 0 {
		t.Errorf("engine A stats %+v", s)
	}
	e.requireBooks("after two engines")
}

func TestJobEngineLoop_AStartDeadlineThatPassesExpiresTheJobAndReleasesTheReserve(t *testing.T) {
	e := newJeEnv(t, jobs.WithMaxStartDelay(3*time.Second))
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmRows(u, "m2", append(jeAddrs(3, "d"), jobs.RowIn{Email: "no at sign"}))
	fake := &jobenginetest.FakeModule{}
	fake.SetDown(true) // the module never answers
	rig := e.newRig(fake, jobengine.Config{})

	for i := 0; i < 3; i++ {
		rig.eng.Tick(ctx)
	}
	if e.jobStatus(j.ID) != jobs.StatusConfirmed || e.resultCount(j.ID) != 1 || e.chargeRows(j.ID) != 0 || fake.HeldCount(j.ID) != 0 {
		t.Fatalf("inside the deadline: %s, %d results, %d charges, the module holds %d; want the job untouched (only the platform's own malformed result)", e.jobStatus(j.ID), e.resultCount(j.ID), e.chargeRows(j.ID), fake.HeldCount(j.ID))
	}
	if fake.Calls().Submit < 3 || rig.eng.Stats().Waits < 3 {
		t.Fatalf("the engine did not try the module at each tick: %+v, %+v", fake.Calls(), rig.eng.Stats())
	}
	if e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND submitted_at IS NOT NULL", j.ID) != 0 {
		t.Fatal("rows were stamped as submitted to a module that never held them")
	}

	time.Sleep(3500 * time.Millisecond) // the deadline, by the database's clock
	rig.eng.Tick(ctx)
	if got := e.jobStatus(j.ID); got != jobs.StatusExpired {
		t.Fatalf("after the deadline the job is %s, want expired (engine stats %+v)", got, rig.eng.Stats())
	}
	if fake.HeldCount(j.ID) != 0 {
		t.Fatal("the module took rows after all")
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|platform|expired|P-02", "2|platform|expired|P-02", "3|platform|expired|P-02", "4|platform|malformed|P-08"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	e.requireEnded(j.ID, jobs.StatusExpired, 1)
	e.requireBal("after the expiry", u, "m2", 1000, 0)
	before := e.fingerprint()
	rig.eng.Tick(ctx)
	if e.fingerprint() != before {
		t.Error("a tick after the expiry changed something")
	}
}

func TestJobEngineLoop_CancellingChargesTheItemInFlightOnceFreesTheCancelledOnesAndEndsCancelled(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)

	// 1. the module holds every row: one item is in flight when the user cancels, the rest are unclaimed
	u := e.newUser()
	j := e.confirmN(u, "m2", 4)
	fake := &jobenginetest.FakeModule{}
	rig := e.newRig(fake, jobengine.Config{})
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 4 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	if fake.ClaimInFlight(j.ID, 1) != 1 {
		t.Fatal("no item could be put in flight")
	}
	cj, err := e.jobs.CancelBeforeStart(ctx, u, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelling {
		t.Fatalf("the user's cancel: %v, %v", cj.Status, err)
	}
	e.tickUntil(rig.eng, j.ID, jobs.StatusCancelled, 5)
	if fake.Calls().Cancel < 1 {
		t.Fatal("the module was never asked to cancel")
	}
	e.requireSettledLikeTheModule(fake, j.ID)
	if got := e.ledgerCharges(j.ID); len(got) != 1 {
		t.Fatalf("%d charges, want exactly 1 (the item that was in flight and finished sent): %v", len(got), got)
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state = 'cancelled' AND decision = 'free' AND rule = 'M2-08' AND origin = 'module'", j.ID); n != 3 {
		t.Fatalf("%d cancelled free results, want 3", n)
	}
	if r, c, l := e.reserveOf(j.ID); r != 4 || c != 1 || l != 3 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 4, 1 and 3", r, c, l)
	}
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
	e.requireBal("after the cancel", u, "m2", 999, 0)
	if e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 || e.transitionCount(j.ID, jobs.StatusCancelled) != 1 {
		t.Fatal("a job whose cancel was accepted must end cancelled, never succeeded")
	}
	before := e.fingerprint()
	rig.eng.Tick(ctx)
	if e.fingerprint() != before {
		t.Error("a tick after the end of the job changed something")
	}

	// 2. the module took only part of the list when the cancel came: the rows it never held end as the platform's own cancelled results
	u2 := e.newUser()
	j2 := e.confirmN(u2, "m2", 4)
	fake2 := &jobenginetest.FakeModule{AcceptPerCall: 2}
	rig2 := e.newRig(fake2, jobengine.Config{})
	rig2.eng.Tick(ctx)
	if e.jobStatus(j2.ID) != jobs.StatusRunning || fake2.HeldCount(j2.ID) != 2 {
		t.Fatalf("the second fixture: %s, the module holds %d", e.jobStatus(j2.ID), fake2.HeldCount(j2.ID))
	}
	fake2.ClaimInFlight(j2.ID, 1)
	if _, err := e.jobs.CancelBeforeStart(ctx, u2, j2.ID); err != nil {
		t.Fatal(err)
	}
	e.tickUntil(rig2.eng, j2.ID, jobs.StatusCancelled, 5)
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j2.ID)
	want := []string{"1|module|sent|charge|M2-01", "2|module|cancelled|free|M2-08", "3|platform|cancelled|free|P-01", "4|platform|cancelled|free|P-01"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	e.requireEnded(j2.ID, jobs.StatusCancelled, 1)
	e.requireSettledLikeTheModule(fake2, j2.ID)
	e.requireBal("after the second cancel", u2, "m2", 999, 0)

	// 3. module 1 has no cancel: the database refuses it, so the engine never sees a cancelling module 1 job and never asks that module to cancel
	u3 := e.newUser()
	m1 := e.confirmN(u3, "m1", 2)
	fake1 := &jobenginetest.FakeModule{Module: "m1"}
	rig1 := e.newRig(fake1, jobengine.Config{SubmitBatchFor: map[string]int{"m1": 100}})
	rig1.eng.Tick(ctx)
	_, err = e.jobs.CancelBeforeStart(ctx, u3, m1.ID)
	jeNotAllowed(t, "the cancel of a running module 1 job", err, "job_module_cannot_cancel")
	if e.jobStatus(m1.ID) != jobs.StatusRunning {
		t.Fatalf("the refused cancel left the job %s", e.jobStatus(m1.ID))
	}
	e.tickUntil(rig1.eng, m1.ID, jobs.StatusSucceeded, 5)
	if fake1.Calls().Cancel != 0 {
		t.Fatal("the engine asked module 1 to cancel")
	}
	e.requireEnded(m1.ID, jobs.StatusSucceeded, 0) // two rows, two charges at 1: nothing left to release
}

// jeDropper is a module that says it is complete while leaving one finished item out of its report: the engine must not complete the job over a result it does not have.
type jeDropper struct {
	*jobenginetest.FakeModule
	drop string
	on   atomic.Bool
}

func (d *jeDropper) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	return d.FakeModule.Observe(ctx, j, func(items []jobs.ResultItem) error {
		if d.on.Load() {
			kept := make([]jobs.ResultItem, 0, len(items))
			for _, it := range items {
				if it.ItemRef != d.drop {
					kept = append(kept, it)
				}
			}
			items = kept
		}
		return fn(items)
	})
}

type jeSingleModule struct{ m jobengine.Module }

func (s jeSingleModule) For(context.Context, string, string) (jobengine.Module, error) {
	return s.m, nil
}

func TestJobEngineLoop_AModuleCompleteWithAResultMissingIsNotCompletedFlaggedStaleAfterTheLimitAndCompletedWhenTheResultArrives(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 3)
	ids := e.rowIDs(j.ID, "pending")
	fake := &jobenginetest.FakeModule{Plan: jePlan(3)}
	d := &jeDropper{FakeModule: fake, drop: ids[1]}
	d.on.Store(true)
	clock := jobenginetest.NewClock(time.Now())
	eng := e.engineOf(jeSingleModule{d}, jobenginetest.NewGate(true), clock, jobengine.Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}})

	for i := 0; i < 4; i++ {
		eng.Tick(ctx)
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning || e.transitionCount(j.ID, jobs.StatusCompleting) != 0 {
		t.Fatalf("the job is %s: a module that says complete with a result missing must not complete it", e.jobStatus(j.ID))
	}
	if e.resultCount(j.ID) != 2 || e.chargeRows(j.ID) != 1 {
		t.Fatalf("%d results and %d charges, want the two items the module reported (one charged, one free) and no charge for the one it left out", e.resultCount(j.ID), e.chargeRows(j.ID))
	}
	if e.eventCount(j.ID, "flag") != 1 || e.eventCount(j.ID, "stale") != 0 {
		t.Fatalf("%d flags and %d stale events after four ticks inside the limit; want 1 and 0 (flagged once, not at every tick)", e.eventCount(j.ID, "flag"), e.eventCount(j.ID, "stale"))
	}
	// the staleness limit passes: the stale event is raised, the flag is renewed, the job is still running
	clock.Advance(2 * time.Hour)
	eng.Tick(ctx)
	if e.eventCount(j.ID, "stale") != 1 || e.eventCount(j.ID, "flag") != 2 || e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatalf("after the limit: %d stale events, %d flags, status %s; want 1, 2, running", e.eventCount(j.ID, "stale"), e.eventCount(j.ID, "flag"), e.jobStatus(j.ID))
	}
	// the result arrives: the job completes
	d.on.Store(false)
	e.tickUntil(eng, j.ID, jobs.StatusSucceeded, 5)
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
	if e.resultCount(j.ID) != 3 || e.chargeRows(j.ID) != 2 {
		t.Fatalf("%d results and %d charges after the result arrived, want 3 and 2", e.resultCount(j.ID), e.chargeRows(j.ID))
	}
}

func TestJobEngineLoop_ItemsTheModuleCallsNotFinalAreNeverRecorded(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	j := e.confirmN(e.newUser(), "m2", 3)
	fake := &jobenginetest.FakeModule{FinalizePerObserve: 1, LeakNotFinal: true, Plan: jePlan(3)} // every report also lists the items still queued
	rig := e.newRig(fake, jobengine.Config{})
	rig.eng.Tick(ctx) // the start
	rig.eng.Tick(ctx) // one item finishes; two queued ones are leaked into the report
	if e.resultCount(j.ID) != 1 || e.chargeRows(j.ID) != 1 {
		t.Fatalf("%d results and %d charges after one finished item; want 1 and 1 (the queued items are not final)", e.resultCount(j.ID), e.chargeRows(j.ID))
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatalf("the job is %s while two items are still queued", e.jobStatus(j.ID))
	}
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
}

func TestJobEngineLoop_TheRepairPassReleasesAJobAnAdministratorEndedByHandWithoutCallingTheModule(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 3)
	jobWalkTo(t, e.admin, j.ID, jobs.StatusFailed)
	if e.remainder(j.ID) != 3 {
		t.Fatalf("the fixture: remainder %d, want 3", e.remainder(j.ID))
	}
	fake := &jobenginetest.FakeModule{}
	rig := e.newRig(fake, jobengine.Config{})
	rig.eng.Tick(ctx)
	if e.remainder(j.ID) != 0 || e.releaseRows(j.ID) != 1 || e.jobStatus(j.ID) != jobs.StatusFailed {
		t.Fatalf("after the repair pass: remainder %d, %d release rows, status %s", e.remainder(j.ID), e.releaseRows(j.ID), e.jobStatus(j.ID))
	}
	e.requireBal("after the repair", u, "m2", 1000, 0)
	if fake.Calls().Total != 0 {
		t.Fatalf("the repair pass called the module %d times", fake.Calls().Total)
	}
	before := e.fingerprint()
	rig.eng.Tick(ctx)
	if e.fingerprint() != before {
		t.Fatal("a second pass released something again")
	}
	e.requireBooks("after the repair pass")
}

type jeRefuser struct{ *jobenginetest.FakeModule }

func (jeRefuser) Submit(context.Context, jobs.EngineJob, []jobs.EngineRow) (jobengine.SubmitResult, error) {
	return jobengine.SubmitResult{}, jobengine.Refused("the subject is not accepted")
}

func TestJobEngineLoop_AModuleThatRefusesTheJobForGoodFailsItFreeAndGivesTheReserveBack(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 3)
	eng := e.engineOf(jeSingleModule{jeRefuser{&jobenginetest.FakeModule{}}}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()), jobengine.Config{})
	eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusFailed || eng.Stats().Refused != 1 {
		t.Fatalf("the job is %s (refused %d), want failed", e.jobStatus(j.ID), eng.Stats().Refused)
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND rule = 'P-04' AND module_state = 'infrastructure_failure'", j.ID); n != 3 {
		t.Fatalf("%d P-04 results, want 3", n)
	}
	if why := e.str("SELECT reason FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND to_status = 'failed'", j.ID); why == "" || len(why) > 500 {
		t.Errorf("the failure's reason = %q", why)
	}
	e.requireEnded(j.ID, jobs.StatusFailed, 1)
	e.requireBal("after the refusal", u, "m2", 1000, 0)
}
