//go:build harness

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D1 (platform_0016, the modules' new routes), the store, the engine and the database together: the pause and resume of a RUNNING job of module 2 or 3 and the cancel of a RUNNING module 1
// job (and of a job that was paused while running). Real Postgres, the real store, the engine driven tick by tick (Engine.Tick) over the deterministic fake module (jobenginetest.FakeModule: Pause, Resume,
// Claim, Paused, the lag knobs PauseAckAfter and ResumeAckAfter, module 1's cancel). The fake is the only double; what it reports is what the ledger and the results must equal. Helpers:
// jobs_engine_helpers_test.go (every name `je...`); the names of this file start with `jpc`. Every test starts with module 1's vendor unit off (newJeEnv) and has a paired control.
//
// The fake worker settles an item when the engine OBSERVES the job (FakeModule.work), so a test that must keep an item unfinished while the campaign is held uses Manual: nothing settles but what the test
// settles (Settle) and nothing is in flight but what the test claims (ClaimInFlight). The engine's backoff is off (newRig): every tick looks at every job that is due.
//
//	APausedRunningModule2Job...        pause: the engine asks until the module's REPORT says held (and then stops asking), the item in flight finishes and is settled while held, no row is submitted while held,
//	                                   no stale event (the same job running and quiet gets one), the notes module_paused / module_resumed once each after the API's own paused event, the resume, the rest, the end
//	APausedRunningModule3Job...        the same for a module that holds no credits
//	ACrashBetweenTheMoveAndTheCall...  the engine that was to tell the module never does: a second engine converges; a kill right after the module took the pause converges with ONE note
//	AJobPausedWhileRunningCompletes... the module says every item it holds is final: paused -> completing -> succeeded; with rows still to be handed over it stays paused (control)
//	CancellingARunningModule1Job...    module 1 is asked to cancel: unclaimed rows free by M1-24, in-flight probes charged, the remainder released once, ends cancelled
//	CancellingAPausedRunningJob...     cancelling, never cancelled: the engine's cancel ends the hold, the events run paused -> cancelling -> completing -> cancelled
//	AJobPausedBeforeItsStart...        cancelled directly with the reserve released; the engine never calls its module; the two new walls; it expires, and a job paused while running never does (control)
//	EngineWorkOffers...                a job paused while running is due; idle it backs off, a move wakes it, a past deadline never offers it
//	UsersAndIdempotence / APauseOfAModule1Job...   another user's moves are not found; a second pause is a no-op; module 1's pause is refused and writes nothing

const jpcMoveSQL = "SELECT platform.job_transition($1::uuid, $2::text, $3::text, $4::uuid, $5::text)"

// jpcRefuseMove tries one raw move (what a writer that skips the store would do) in a transaction that is rolled back and requires the database to refuse it with 23001 and exactly this constraint.
func jpcRefuseMove(t *testing.T, admin *pgx.Conn, job, to, kind string, actor any, constraint string) {
	t.Helper()
	ctx := testCtx(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerExpectRefused(t, tx, fmt.Sprintf("a raw move to %s by %s", to, kind), "23001", ledgerWhy{Constraint: constraint}, jpcMoveSQL, job, to, kind, actor, "a raw move of the test")
}

// jpcPastDeadline puts the job's start deadline one second in the past (with the lifecycle guard switched off for the one statement, as the store tests do: no role can move a deadline).
func (e *jeEnv) jpcPastDeadline(job string) {
	e.t.Helper()
	e.exec("ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	e.exec("UPDATE platform.jobs SET start_deadline = now() - interval '1 second' WHERE id = $1", job)
	e.exec("ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
}

// jpcTrail is the job's history as the moves that matter and the notes of the module's hold, oldest first: `transition:<to>` and `note:<event>`.
func (e *jeEnv) jpcTrail(job string) []string {
	e.t.Helper()
	return e.rows(`SELECT kind || ':' || COALESCE(to_status, detail->>'event', '') FROM platform.job_events
		WHERE job_id = $1 AND (kind = 'note' OR (kind = 'transition' AND to_status IN ('running', 'paused', 'cancelling', 'completing', 'cancelled', 'succeeded', 'expired')))
		ORDER BY id`, job)
}

func (e *jeEnv) jpcNotes(job, event string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'note' AND detail->>'event' = $2", job, event)
}

// jpcPlan finishes every row sent for a sender, except the row at freeOrdinal, which fails (free): the outcomes name the module the job belongs to.
func jpcPlan(module string, freeOrdinal int) func(jobs.EngineRow) charge.Outcome {
	return func(r jobs.EngineRow) charge.Outcome {
		if r.Ordinal == freeOrdinal {
			return charge.Outcome{Module: module, State: "failed", Reason: "attempts_exhausted"}
		}
		return charge.Outcome{Module: module, State: "sent"}
	}
}

func jpcEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s:\n  got  %v\n  want %v", what, got, want)
	}
}

// ---- 1 and 2: a job paused while running ----------------------------------------------------------------------------------------------------------------------------------------------------

// jpcHeldAndResumed is the whole life of a sender job that is paused while it runs, for module 2 (metered) and module 3 (entitlement, no credits).
func jpcHeldAndResumed(t *testing.T, module string) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	metered := module == jobs.ModuleSendManaged
	u, ctl := e.newUser(), e.newUser()
	j := e.confirmN(u, module, 6)
	control := e.confirmN(ctl, module, 2) // THE CONTROL of the staleness check: a job of the same module that is RUNNING and quiet
	// the module takes three rows per call and settles nothing by itself; it holds the campaign only from the third Pause call and lets go only from the second Resume call (a lagging module)
	fake := &jobenginetest.FakeModule{Module: module, Manual: true, AcceptPerCall: 3, PauseAckAfter: 3, ResumeAckAfter: 2, Plan: jpcPlan(module, 4)}
	rig := e.newRig(fake, jobengine.Config{})

	rig.eng.Tick(ctx) // both jobs start: three rows of the six are held, the other three are still to be handed over
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 3 || e.jobStatus(control.ID) != jobs.StatusRunning {
		t.Fatalf("the fixture: %s, the module holds %d of 6 rows", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	if fake.ClaimInFlight(j.ID, 1) != 1 {
		t.Fatal("no item could be put in flight")
	}
	refs := e.rowIDs(j.ID, "pending")
	submits := fake.Calls().Submit
	if metered {
		e.requireBal("before the pause", u, module, 994, 6)
	}

	// the user pauses: the job is paused the moment the store answers, from running; nothing is told to the module yet
	pj, err := e.jobs.Pause(ctx, u, j.ID)
	if err != nil || pj.Status != jobs.StatusPaused || pj.PausedFrom != jobs.StatusRunning {
		t.Fatalf("the user's pause: %v, %s from %q", err, pj.Status, pj.PausedFrom)
	}
	if fake.Calls().Pause != 0 || fake.Paused(j.ID) {
		t.Fatal("the store told the module: that is the engine's, at its next tick")
	}
	lines := e.jpcEventLines(j.ID)
	if last := lines[len(lines)-1]; last != "transition|running>paused|user|"+u+"|paused by the user" {
		t.Fatalf("the last event is %q: who, when and why of the pause", last)
	}

	// the engine asks the module to hold the campaign, at every look, until the module's REPORT says it does: three calls, then it stops
	for i := 1; i <= 3; i++ {
		rig.eng.Tick(ctx)
		if c := fake.Calls().Pause; c != i {
			t.Fatalf("after look %d the module has been asked to pause %d time(s), want %d", i, c, i)
		}
		if held := fake.Paused(j.ID); held != (i == 3) {
			t.Fatalf("after look %d the module holds the campaign: %v (it holds it from the third call on)", i, held)
		}
	}
	for i := 0; i < 3; i++ {
		rig.eng.Tick(ctx)
	}
	if c := fake.Calls().Pause; c != 3 {
		t.Fatalf("the module was asked to pause %d times: the engine must stop asking once the report says paused", c)
	}
	if e.jpcNotes(j.ID, jobs.NoteModulePaused) != 1 {
		t.Fatalf("%d module_paused notes after the module held the campaign, want exactly 1", e.jpcNotes(j.ID, jobs.NoteModulePaused))
	}
	if fake.Calls().Submit != submits {
		t.Fatal("a row was offered to the module while the job was paused")
	}
	if w, ok := e.workOf(j.ID); !ok || w.Unsubmitted != 3 || w.Status != jobs.StatusPaused {
		t.Fatalf("the paused job's work entry: %+v (found %v), want paused with its three unsubmitted rows still waiting", w.Status, ok)
	}

	// the item that was IN FLIGHT finishes while the campaign is held: it is recorded and settled (charged for a sender that charges); the queued ones are not claimed, so nothing else is reported
	if !fake.Settle(j.ID, refs[0], charge.Outcome{Module: module, State: "sent"}) {
		t.Fatal("the in-flight item could not be settled")
	}
	rig.eng.Tick(ctx)
	if got := e.resultCount(j.ID); got != 1 {
		t.Fatalf("%d results after the in-flight item finished, want exactly 1 (no outcome is invented for the held ones)", got)
	}
	if metered {
		if got := e.ledgerCharges(j.ID); len(got) != 1 || got[refs[0]] != "m2_accepted" {
			t.Fatalf("charges while held: %v, want the one item that finished, charged m2_accepted", got)
		}
		e.requireBal("an item settled while the job is paused", u, module, 994, 5)
	}
	if fake.Calls().Submit != submits || fake.Held(refs[3]) != 0 {
		t.Fatal("a row was submitted while the job was paused")
	}

	// NO STALE ALERT for a job nothing is expected to move: both jobs are silent for three hours by the database's clock and the engine's; the running one is stale, the paused one is not
	e.ageProgress(j.ID, 3*time.Hour)
	e.ageProgress(control.ID, 3*time.Hour)
	rig.clock.Advance(3 * time.Hour)
	for i := 0; i < 3; i++ {
		rig.eng.Tick(ctx)
	}
	if e.eventCount(control.ID, "stale") != 1 {
		t.Fatalf("the control (running, quiet for three hours) has %d stale events, want 1: the staleness check is not seeing quiet jobs", e.eventCount(control.ID, "stale"))
	}
	if e.eventCount(j.ID, "stale") != 0 || e.jobStatus(j.ID) != jobs.StatusPaused {
		t.Fatalf("the paused job has %d stale events and is %s: a held job is not stale, and a job that was running is never expired", e.eventCount(j.ID, "stale"), e.jobStatus(j.ID))
	}

	// the resume: the job is running again at once; the engine submits the rest and tells the module (it lets go from the second call on)
	rj, err := e.jobs.Resume(ctx, u, j.ID)
	if err != nil || rj.Status != jobs.StatusRunning || rj.PausedFrom != "" {
		t.Fatalf("the user's resume: %v, %s from %q", err, rj.Status, rj.PausedFrom)
	}
	for i := 1; i <= 3; i++ {
		rig.eng.Tick(ctx)
	}
	if c := fake.Calls().Resume; c != 2 || fake.Paused(j.ID) {
		t.Fatalf("the module was asked to resume %d time(s) and holds the campaign: %v; want 2 calls (it lets go from the second) and no hold, and no call after the report said it was free", c, fake.Paused(j.ID))
	}
	if fake.HeldCount(j.ID) != 6 {
		t.Fatalf("the module holds %d rows after the resume, want all 6: the rest is submitted once the job runs again", fake.HeldCount(j.ID))
	}
	if e.jpcNotes(j.ID, jobs.NoteModuleResumed) != 1 || e.jpcNotes(j.ID, jobs.NoteModulePaused) != 1 {
		t.Fatalf("notes: %d module_paused and %d module_resumed, want one each", e.jpcNotes(j.ID, jobs.NoteModulePaused), e.jpcNotes(j.ID, jobs.NoteModuleResumed))
	}
	if detail := e.rows(`SELECT detail->>'event' || ':' || (detail->>'paused') || ':' || (detail->>'module') FROM platform.job_events WHERE job_id = $1 AND kind = 'note' ORDER BY id`, j.ID); fmt.Sprint(detail) != fmt.Sprint([]string{"module_paused:true:" + module, "module_resumed:false:" + module}) {
		t.Fatalf("the notes' detail: %v, want the module's own paused value (true, then false) and the module", detail)
	}

	// the rest runs to the end by itself: the worker settles what is left, and the three ways agree
	fake.Manual = false
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	e.requireSettledLikeTheModule(fake, j.ID)
	if got := e.resultCount(j.ID); got != 6 {
		t.Fatalf("%d results, want 6", got)
	}
	if metered {
		e.requireEnded(j.ID, jobs.StatusSucceeded, 1) // six reserved, five charged, the failed one released: ONE release
		if r, c, l := e.reserveOf(j.ID); r != 6 || c != 5 || l != 1 {
			t.Fatalf("reserve held %d, charged %d, released %d; want 6, 5 and 1", r, c, l)
		}
	} else if e.int("SELECT count(*) FROM platform.reserves WHERE job_id = $1", j.ID) != 0 {
		t.Fatal("a module 3 job holds a reserve: it holds no credits")
	}
	e.requireBooks("the held job ended")
	jpcEqual(t, "the history of the held job", e.jpcTrail(j.ID), []string{
		"transition:running", "transition:paused", "note:module_paused", "transition:running", "note:module_resumed", "transition:completing", "transition:succeeded"})
	for _, to := range []string{jobs.StatusCompleting, jobs.StatusSucceeded, jobs.StatusPaused} {
		if n := e.transitionCount(j.ID, to); n != 1 {
			t.Fatalf("%d moves to %s, want 1", n, to)
		}
	}
	if fake.Calls().Pause != 3 || fake.Calls().Resume != 2 {
		t.Fatalf("after the end the module was asked %d times to pause and %d to resume, want 3 and 2: nothing repeats once the report is what the engine wants", fake.Calls().Pause, fake.Calls().Resume)
	}
	if e.jobStatus(control.ID) != jobs.StatusRunning {
		t.Fatalf("the control job is %s: nothing may have touched it", e.jobStatus(control.ID))
	}
}

func TestJobsPauseCancel_APausedRunningModule2JobIsHeldObservedSettledNeverFedAndNeverStaleThenResumesAndCompletes(t *testing.T) {
	jpcHeldAndResumed(t, jobs.ModuleSendManaged)
}

func TestJobsPauseCancel_APausedRunningModule3JobIsHeldAndResumesLikeModule2WithNoCredits(t *testing.T) {
	jpcHeldAndResumed(t, jobs.ModuleSendBYO)
}

// ---- 3: the crash between the move and the call --------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_ACrashBetweenTheMoveAndTheModuleCallConvergesAndTheNoteIsWrittenOnce(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)

	// ---- A: the user pauses and the engine that was to tell the module is not leading (it died): a second engine on the same database converges
	u := e.newUser()
	j := e.confirmN(u, "m2", 4)
	fake := &jobenginetest.FakeModule{Manual: true}
	first := e.newRig(fake, jobengine.Config{})
	first.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 4 || fake.ClaimInFlight(j.ID, 1) != 1 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	first.gate.Set(false) // the first engine is gone
	calls, before := fake.Calls(), e.fingerprint()
	first.eng.Tick(ctx)
	if fake.Calls() != calls || e.fingerprint() != before || fake.Paused(j.ID) {
		t.Fatal("an engine that does not lead called the module or wrote something: the CONTROL of the convergence is not a control")
	}
	second := e.newRig(fake, jobengine.Config{}) // a fresh engine: no memory of the first
	for i := 0; i < 5 && !fake.Paused(j.ID); i++ {
		second.eng.Tick(ctx)
	}
	if !fake.Paused(j.ID) {
		t.Fatalf("a second engine did not hold the campaign of the paused job (module calls %+v)", fake.Calls())
	}
	for i := 0; i < 3; i++ {
		second.eng.Tick(ctx)
	}
	if fake.Calls().Pause != 1 || e.jpcNotes(j.ID, jobs.NoteModulePaused) != 1 {
		t.Fatalf("the module was asked to pause %d time(s) and %d module_paused notes were written, want 1 and 1", fake.Calls().Pause, e.jpcNotes(j.ID, jobs.NoteModulePaused))
	}

	// ... and the same for the RESUME: the job is running, the module still holds the campaign and the engine that was to tell it died; a third engine, fresh, converges, and its first look
	// (the module says paused) must not write the paused note a second time
	if _, err := e.jobs.Resume(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	if !fake.Paused(j.ID) {
		t.Fatal("the fixture: the module must still hold the campaign")
	}
	third := e.newRig(fake, jobengine.Config{})
	for i := 0; i < 5 && fake.Paused(j.ID); i++ {
		third.eng.Tick(ctx)
	}
	if fake.Paused(j.ID) || fake.Calls().Resume != 1 {
		t.Fatalf("a third engine did not let the campaign go (holds %v, %d resume call(s))", fake.Paused(j.ID), fake.Calls().Resume)
	}
	for i := 0; i < 3; i++ {
		third.eng.Tick(ctx)
	}
	if e.jpcNotes(j.ID, jobs.NoteModulePaused) != 1 || e.jpcNotes(j.ID, jobs.NoteModuleResumed) != 1 || fake.Calls().Resume != 1 {
		t.Fatalf("%d module_paused and %d module_resumed notes and %d resume calls, want 1, 1 and 1: a restart writes nothing twice and asks nothing twice",
			e.jpcNotes(j.ID, jobs.NoteModulePaused), e.jpcNotes(j.ID, jobs.NoteModuleResumed), fake.Calls().Resume)
	}
	e.requireBooks("after the crash and the convergence")
}

// The engine is killed right AFTER the module took the pause, before it recorded anything: the effect stands, the next engine finds the campaign held and asks for nothing, and writes the one note. (Its own
// test and database: the crash switch counts every module call, so no other job may be in the engine's work.)
func TestJobsPauseCancel_AKillRightAfterTheModuleTookThePauseConvergesWithOneNoteAndNoSecondCall(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u2 := e.newUser()
	j2 := e.confirmN(u2, "m2", 4)
	fake2 := &jobenginetest.FakeModule{Manual: true}
	e1 := e.newRig(fake2, jobengine.Config{})
	e1.eng.Tick(ctx)
	if e.jobStatus(j2.ID) != jobs.StatusRunning {
		t.Fatalf("the fixture: %s", e.jobStatus(j2.ID))
	}
	if _, err := e.jobs.Pause(ctx, u2, j2.ID); err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	n := fake2.Calls().Total
	fake2.Arm(n+2, true, cancel1) // the paused step's first module call is an Observe (n+1), its second is the Pause: the process dies right after it executed
	e1.eng.Tick(ctx1)
	if fake2.Armed() || ctx1.Err() == nil {
		t.Fatalf("the crash never happened (armed %v, engine cancelled %v): the run proves nothing", fake2.Armed(), ctx1.Err() != nil)
	}
	if !fake2.Paused(j2.ID) || e.jpcNotes(j2.ID, jobs.NoteModulePaused) != 0 {
		t.Fatalf("after the kill the module holds the campaign: %v, %d module_paused notes; want held and none written (the engine died before it wrote)", fake2.Paused(j2.ID), e.jpcNotes(j2.ID, jobs.NoteModulePaused))
	}
	e2 := e.newRig(fake2, jobengine.Config{})
	for i := 0; i < 3; i++ {
		e2.eng.Tick(ctx)
	}
	if e.jpcNotes(j2.ID, jobs.NoteModulePaused) != 1 || fake2.Calls().Pause != 1 {
		t.Fatalf("after the restart: %d module_paused notes and %d pause calls, want exactly 1 and 1", e.jpcNotes(j2.ID, jobs.NoteModulePaused), fake2.Calls().Pause)
	}
	if e.jobStatus(j2.ID) != jobs.StatusPaused {
		t.Fatalf("the job is %s", e.jobStatus(j2.ID))
	}
	e.requireBooks("after the kill right after the pause")
}

// ---- 4: a job paused while running completes the normal way only when there is nothing left to hand over ----------------------------------------------------------------------------

func TestJobsPauseCancel_AJobPausedWhileRunningCompletesTheNormalWayOnlyWhenEveryRowIsDone(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)

	// (a) every row was claimed before the pause and finishes while the job is held: the module says complete, every row has a result, so the job ends as a running one would
	u := e.newUser()
	j := e.confirmN(u, "m2", 3)
	fake := &jobenginetest.FakeModule{Manual: true, Plan: jpcPlan("m2", 3)}
	rig := e.newRig(fake, jobengine.Config{})
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.ClaimInFlight(j.ID, 3) != 3 {
		t.Fatalf("the fixture: %s", e.jobStatus(j.ID))
	}
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	refs := e.rowIDs(j.ID, "pending")
	fake.Settle(j.ID, refs[0], charge.Outcome{Module: "m2", State: "sent"})
	fake.Settle(j.ID, refs[1], charge.Outcome{Module: "m2", State: "bounced"})
	fake.Settle(j.ID, refs[2], charge.Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted"})
	rig.eng.Tick(ctx)
	e.requireEnded(j.ID, jobs.StatusSucceeded, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
	jpcEqual(t, "the history of the job that finished while held", e.jpcTrail(j.ID), []string{"transition:running", "transition:paused", "transition:completing", "transition:succeeded"})
	if fake.Calls().Pause != 0 {
		t.Fatalf("the module was asked to pause %d time(s): a campaign that is complete holds nothing to hold", fake.Calls().Pause)
	}
	if r, c, l := e.reserveOf(j.ID); r != 3 || c != 2 || l != 1 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 3, 2 and 1", r, c, l)
	}

	// (b) THE CONTROL: the same, but two rows were never handed over to the module when the user paused (it takes two rows per call): the module is complete with what it holds, and the job is NOT
	// completed (it would end over rows nobody answered): it stays paused, nothing is submitted, until the user resumes; then the rest is handed over and the job ends
	u2 := e.newUser()
	j2 := e.confirmN(u2, "m2", 4)
	fake2 := &jobenginetest.FakeModule{Manual: true, AcceptPerCall: 2, Plan: jpcPlan("m2", 0)}
	rig2 := e.newRig(fake2, jobengine.Config{})
	rig2.eng.Tick(ctx)
	if e.jobStatus(j2.ID) != jobs.StatusRunning || fake2.HeldCount(j2.ID) != 2 || fake2.ClaimInFlight(j2.ID, 2) != 2 {
		t.Fatalf("the control fixture: %s, %d held", e.jobStatus(j2.ID), fake2.HeldCount(j2.ID))
	}
	if _, err := e.jobs.Pause(ctx, u2, j2.ID); err != nil {
		t.Fatal(err)
	}
	refs2 := e.rowIDs(j2.ID, "pending")
	fake2.Settle(j2.ID, refs2[0], charge.Outcome{Module: "m2", State: "sent"})
	fake2.Settle(j2.ID, refs2[1], charge.Outcome{Module: "m2", State: "sent"})
	submits := fake2.Calls().Submit
	for i := 0; i < 4; i++ {
		rig2.eng.Tick(ctx)
	}
	if e.jobStatus(j2.ID) != jobs.StatusPaused || e.transitionCount(j2.ID, jobs.StatusCompleting) != 0 || e.resultCount(j2.ID) != 2 {
		t.Fatalf("the control: the job is %s with %d results and %d moves to completing; want paused, 2 results (the held rows have none) and no completing", e.jobStatus(j2.ID), e.resultCount(j2.ID), e.transitionCount(j2.ID, jobs.StatusCompleting))
	}
	if fake2.Calls().Submit != submits {
		t.Fatal("a row was submitted while the job was paused")
	}
	if _, err := e.jobs.Resume(ctx, u2, j2.ID); err != nil {
		t.Fatal(err)
	}
	fake2.Manual = false
	e.tickUntil(rig2.eng, j2.ID, jobs.StatusSucceeded, 10)
	if fake2.HeldCount(j2.ID) != 4 {
		t.Fatalf("the module holds %d rows at the end, want 4: the rest was handed over after the resume", fake2.HeldCount(j2.ID))
	}
	e.requireEnded(j2.ID, jobs.StatusSucceeded, 0) // four reserved, four charged
	e.requireSettledLikeTheModule(fake2, j2.ID)
}

// ---- 5: cancel of a running module 1 job ---------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_CancellingARunningModule1JobFreesTheUnclaimedChargesTheInFlightAndEndsCancelled(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	plan := func(r jobs.EngineRow) charge.Outcome {
		if r.Ordinal == 2 {
			return charge.Outcome{Module: "m1", State: "unknown", Reason: "unknown_error", FailureOrigin: "own"} // our own failure: free (M1-19)
		}
		return charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"} // m1_own (M1-03)
	}
	u := e.newUser()
	j := e.confirmN(u, "m1", 6)
	fake := &jobenginetest.FakeModule{Module: "m1", Plan: plan}
	rig := e.newRig(fake, jobengine.Config{SubmitBatchFor: map[string]int{"m1": 1000}})
	rig.eng.Tick(ctx) // one Submit holds the whole list
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 6 || fake.ClaimInFlight(j.ID, 2) != 2 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	cj, err := e.jobs.Cancel(ctx, u, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.CancelRequestedAt == nil {
		t.Fatalf("the user's cancel of a running module 1 job: %v, %s", err, cj.Status)
	}
	e.requireBal("the cancel request holds the reserve", u, "m1", 994, 6)
	rig.eng.Tick(ctx)
	if fake.Calls().Cancel < 1 {
		t.Fatal("the engine never asked module 1 to cancel")
	}
	e.requireEnded(j.ID, jobs.StatusCancelled, 1) // six reserved, one charged: five released in ONE release
	if e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 || e.transitionCount(j.ID, jobs.StatusCancelled) != 1 {
		t.Fatal("a job whose cancel was accepted must end cancelled, never succeeded")
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|module|valid|charge|M1-03", "2|module|unknown|free|M1-19", "3|module|cancelled|free|M1-24", "4|module|cancelled|free|M1-24", "5|module|cancelled|free|M1-24", "6|module|cancelled|free|M1-24"}
	jpcEqual(t, "the results of the cancelled module 1 job", got, want)
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state = 'cancelled' AND reason = 'cancelled_by_caller' AND unit IS NULL", j.ID); n != 4 {
		t.Fatalf("%d cancelled results carry module 1's reason and no unit, want 4", n)
	}
	if ch := e.ledgerCharges(j.ID); len(ch) != 1 {
		t.Fatalf("%d charges, want exactly 1 (the probe that was in flight and finished valid; a cancelled row is never charged): %v", len(ch), ch)
	}
	if r, c, l := e.reserveOf(j.ID); r != 6 || c != 1 || l != 5 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 6, 1 and 5", r, c, l)
	}
	e.requireSettledLikeTheModule(fake, j.ID) // the ledger, job_results and the module's report: three ways equal
	// submitted = settled + cancelled: two rows have a verdict, four were cancelled, and that is every row
	if settled, cancelled := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state <> 'cancelled'", j.ID), e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state = 'cancelled'", j.ID); settled != 2 || cancelled != 4 || settled+cancelled != 6 {
		t.Fatalf("settled %d + cancelled %d, want 2 + 4 = 6", settled, cancelled)
	}
	e.requireBal("after the cancel", u, "m1", 999, 0)
	before, calls := e.fingerprint(), fake.Calls()
	rig.eng.Tick(ctx)
	rig.eng.Tick(ctx)
	if e.fingerprint() != before || fake.Calls() != calls {
		t.Error("a tick after the end of the job did something: the remainder is released once")
	}

	// THE CONTROL: the same job, not cancelled, is charged for every row it verified
	u2 := e.newUser()
	j2 := e.confirmN(u2, "m1", 6)
	fake2 := &jobenginetest.FakeModule{Module: "m1"}
	rig2 := e.newRig(fake2, jobengine.Config{SubmitBatchFor: map[string]int{"m1": 1000}})
	e.tickUntil(rig2.eng, j2.ID, jobs.StatusSucceeded, 10)
	if ch := e.ledgerCharges(j2.ID); len(ch) != 6 {
		t.Fatalf("the control job was charged %d times, want 6: without a cancel every row is verified", len(ch))
	}
	if e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND module_state = 'cancelled'", j2.ID) != 0 {
		t.Fatal("the control job has cancelled results")
	}
	e.requireEnded(j2.ID, jobs.StatusSucceeded, 0)
}

// ---- 6: cancel of a job paused while running -----------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_CancellingAPausedRunningJobGoesThroughCancellingAndEndsTheModulesHold(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 6)
	fake := &jobenginetest.FakeModule{Manual: true, Plan: jpcPlan("m2", 0)}
	rig := e.newRig(fake, jobengine.Config{})
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 6 || fake.ClaimInFlight(j.ID, 2) != 2 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	refs := e.rowIDs(j.ID, "pending")
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3 && !fake.Paused(j.ID); i++ {
		rig.eng.Tick(ctx)
	}
	if !fake.Paused(j.ID) {
		t.Fatal("the fixture: the module does not hold the campaign")
	}

	// the user cancels the PAUSED job: it is cancelling, never cancelled; the module is told, the reserve stays held until the engine ends the job
	cj, err := e.jobs.Cancel(ctx, u, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.PausedFrom != "" {
		t.Fatalf("the cancel of a paused running job: %v, %s from %q, want cancelling", err, cj.Status, cj.PausedFrom)
	}
	if e.transitionCount(j.ID, jobs.StatusCancelled) != 0 {
		t.Fatal("the store took a job the module holds straight to cancelled")
	}
	e.requireBal("a cancelling job holds its reserve", u, "m2", 994, 6)
	// the two items in flight finish while the cancel is being carried out
	fake.Settle(j.ID, refs[0], charge.Outcome{Module: "m2", State: "sent"})
	fake.Settle(j.ID, refs[1], charge.Outcome{Module: "m2", State: "bounced"})
	e.tickUntil(rig.eng, j.ID, jobs.StatusCancelled, 5)
	if fake.Calls().Cancel < 1 || fake.Paused(j.ID) {
		t.Fatalf("the engine asked the module to cancel %d time(s) and the module still holds the campaign: %v; a cancel ends the hold", fake.Calls().Cancel, fake.Paused(j.ID))
	}
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision || '|' || rule FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|module|sent|charge|M2-01", "2|module|bounced|charge|M2-02", "3|module|cancelled|free|M2-08", "4|module|cancelled|free|M2-08", "5|module|cancelled|free|M2-08", "6|module|cancelled|free|M2-08"}
	jpcEqual(t, "the results of the cancelled paused job", got, want)
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
	if r, c, l := e.reserveOf(j.ID); r != 6 || c != 2 || l != 4 {
		t.Fatalf("reserve held %d, charged %d, released %d; want 6, 2 and 4", r, c, l)
	}
	e.requireBal("after the cancel", u, "m2", 998, 0)
	jpcEqual(t, "the history of the cancelled paused job", e.jpcTrail(j.ID), []string{
		"transition:running", "transition:paused", "note:module_paused", "transition:cancelling", "note:module_resumed", "transition:completing", "transition:cancelled"})
	if n := e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND from_status = 'paused' AND to_status = 'cancelled'", j.ID); n != 0 {
		t.Fatalf("%d direct moves from paused to cancelled: a job the module holds ends through the engine", n)
	}
	if e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 {
		t.Fatal("a job whose cancel was accepted ended succeeded")
	}
}

// ---- 7: a job paused before its start --------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_AJobPausedBeforeItsStartIsCancelledDirectlyNeverFedToTheEngineAndExpiresWhileOneThatWasRunningNeverDoes(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	start := time.Now().Add(2 * time.Hour)
	fake := &jobenginetest.FakeModule{Manual: true}
	rig := e.newRig(fake, jobengine.Config{})

	// (a) paused from scheduled: the database refuses the two moves that belong to a job the module holds; the engine never calls the module for it
	u1 := e.newUser()
	s1 := e.confirmRows(u1, "m2", jeAddrs(3, "s"), jeConfirmOpts{Start: &start})
	if s1.Status != jobs.StatusScheduled {
		t.Fatalf("the fixture: %s", s1.Status)
	}
	ps, err := e.jobs.Pause(ctx, u1, s1.ID)
	if err != nil || ps.Status != jobs.StatusPaused || ps.PausedFrom != jobs.StatusScheduled {
		t.Fatalf("the pause of a scheduled job: %v, %s from %q", err, ps.Status, ps.PausedFrom)
	}
	jpcRefuseMove(t, e.admin, s1.ID, "cancelling", "user", u1, "job_unsubmitted_pause_never_cancelling")
	jpcRefuseMove(t, e.admin, s1.ID, "cancelling", "admin", mnyAdminA, "job_unsubmitted_pause_never_cancelling")
	jpcRefuseMove(t, e.admin, s1.ID, "completing", "engine", nil, "job_unsubmitted_pause_never_completing")
	if e.jobStatus(s1.ID) != jobs.StatusPaused {
		t.Fatalf("a refused raw move left the job %s", e.jobStatus(s1.ID))
	}
	for i := 0; i < 3; i++ {
		rig.eng.Tick(ctx)
	}
	if fake.Calls().Total != 0 {
		t.Fatalf("the engine called the module %d time(s) for a job paused before its start: nothing was submitted, there is nothing to tell it", fake.Calls().Total)
	}

	// (b) the user's cancel of such a job is the OLD path: straight to cancelled, the platform's own results, the whole reserve released
	u2 := e.newUser()
	s2 := e.confirmRows(u2, "m2", jeAddrs(3, "t"), jeConfirmOpts{Start: &start})
	if _, err := e.jobs.Pause(ctx, u2, s2.ID); err != nil {
		t.Fatal(err)
	}
	c2, err := e.jobs.Cancel(ctx, u2, s2.ID)
	if err != nil || c2.Status != jobs.StatusCancelled || c2.CancelRequestedAt != nil {
		t.Fatalf("the cancel of a job paused before its start: %v, %s (cancel requested %v), want cancelled at once and no cancel request", err, c2.Status, c2.CancelRequestedAt)
	}
	e.requireEnded(s2.ID, jobs.StatusCancelled, 1)
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND module_state = 'cancelled' AND rule = 'P-01'", s2.ID); n != 3 {
		t.Fatalf("%d platform cancelled results, want 3", n)
	}
	e.requireBal("the reserve came back", u2, "m2", 1000, 0)

	// (c) it expires at its start deadline, and its reserve goes back
	e.jpcPastDeadline(s1.ID)
	rig.eng.Tick(ctx)
	if e.jobStatus(s1.ID) != jobs.StatusExpired {
		t.Fatalf("the job paused before its start is %s after its deadline, want expired", e.jobStatus(s1.ID))
	}
	e.requireEnded(s1.ID, jobs.StatusExpired, 1)
	e.requireBal("the expired job's reserve came back", u1, "m2", 1000, 0)
	if fake.Calls().Total != 0 {
		t.Fatal("the engine called the module for a job that never reached it")
	}

	// (d) THE CONTROL: a job paused WHILE RUNNING has a start deadline in its past too and is never expired: the engine goes on holding it
	u3 := e.newUser()
	r3 := e.confirmN(u3, "m2", 2)
	rig.eng.Tick(ctx)
	if e.jobStatus(r3.ID) != jobs.StatusRunning {
		t.Fatalf("the control fixture: %s", e.jobStatus(r3.ID))
	}
	if _, err := e.jobs.Pause(ctx, u3, r3.ID); err != nil {
		t.Fatal(err)
	}
	e.jpcPastDeadline(r3.ID)
	for i := 0; i < 4; i++ {
		rig.eng.Tick(ctx)
	}
	if e.jobStatus(r3.ID) != jobs.StatusPaused || e.transitionCount(r3.ID, jobs.StatusExpired) != 0 || !fake.Paused(r3.ID) {
		t.Fatalf("the job paused while running is %s (holds %v) after its deadline: it is held, never expired", e.jobStatus(r3.ID), fake.Paused(r3.ID))
	}
	if w, ok := e.workOf(r3.ID); !ok || !w.DeadlinePassed {
		t.Fatalf("the control does not see a passed deadline (found %v, passed %v): it would prove nothing", ok, w.DeadlinePassed)
	}
	e.requireBal("a held job keeps its reserve", u3, "m2", 998, 2)
	e.requireBooks("after the paused-before-start jobs")
}

// ---- 8: what the engine is offered ------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_EngineWorkOffersAJobPausedWhileRunningBacksOffWhenIdleIsWokenByAMoveAndNeverOffersItForExpiry(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.running(u, "m2", 2)
	pj, err := e.jobs.Pause(ctx, u, j.ID)
	if err != nil || pj.PausedFrom != jobs.StatusRunning {
		t.Fatalf("the pause: %v, from %q", err, pj.PausedFrom)
	}
	w, ok := e.workOf(j.ID)
	if !ok || w.Status != jobs.StatusPaused || w.PausedFrom != jobs.StatusRunning {
		t.Fatalf("a job paused while running is not in the engine's work (found %v, %s from %q): the module would never be told", ok, w.Status, w.PausedFrom)
	}
	// an idle look backs it off (the database's clock): it is no longer due
	if wrote, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusPaused, false, 30*time.Second, 60*time.Second); err != nil || !wrote {
		t.Fatalf("NotePoll of a paused job: %v, wrote %v", err, wrote)
	}
	if _, ok := e.workOf(j.ID); ok {
		t.Fatal("an idle paused job that backed off is still offered at every tick")
	}
	// a move wakes it, whoever makes it: the resume, then a pause again
	if _, err := e.jobs.Resume(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	if w, ok := e.workOf(j.ID); !ok || w.Status != jobs.StatusRunning {
		t.Fatalf("a resumed job did not wake (found %v, %s)", ok, w.Status)
	}
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.workOf(j.ID); !ok {
		t.Fatal("a job paused again is not due at once")
	}

	// the deadline: a past start deadline offers a job paused BEFORE its start whatever its backoff (it is expired), and never a job paused while running (it is not expired)
	e.jpcPastDeadline(j.ID)
	if _, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusPaused, false, 30*time.Second, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.workOf(j.ID); ok {
		t.Fatalf("a job paused while running with a past deadline and a backoff is offered (deadline passed %v): the deadline must not be a way back into the work for it", got.DeadlinePassed)
	}
	start := time.Now().Add(2 * time.Hour)
	u2 := e.newUser()
	s := e.confirmRows(u2, "m2", jeAddrs(2, "w"), jeConfirmOpts{Start: &start})
	if _, err := e.jobs.Pause(ctx, u2, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.workOf(s.ID); ok {
		t.Fatal("a job paused before its start is offered before its deadline: there is nothing for the engine to do")
	}
	e.jpcPastDeadline(s.ID)
	if _, err := e.jobs.NotePoll(ctx, s.ID, jobs.StatusPaused, false, 30*time.Second, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if w, ok := e.workOf(s.ID); !ok || !w.DeadlinePassed {
		t.Fatalf("THE CONTROL: a job paused before its start with a past deadline is not offered (found %v): it would never expire", ok)
	}
}

// ---- 9: users, idempotence and module 1's pause ------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsPauseCancel_UsersAndIdempotenceOfTheMovesOfARunningJob(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u, other := e.newUser(), e.newUser()
	j := e.running(u, "m2", 3)

	// another user's moves are not found, whatever they are, and write nothing
	before := e.fingerprint()
	for name, call := range map[string]func() error{
		"Pause":  func() error { _, err := e.jobs.Pause(ctx, other, j.ID); return err },
		"Resume": func() error { _, err := e.jobs.Resume(ctx, other, j.ID); return err },
		"Cancel": func() error { _, err := e.jobs.Cancel(ctx, other, j.ID); return err },
	} {
		jeIs(t, name+" of another user's job", call(), jobs.ErrNotFound)
	}
	if e.fingerprint() != before {
		t.Fatal("another user's moves wrote something")
	}
	// a resume of a running job is the store's not_paused
	_, err := e.jobs.Resume(ctx, u, j.ID)
	jeNotAllowed(t, "the resume of a running job", err, "not_paused")
	// the pause: once, and a second pause is the idempotent no-op of a status written back to itself (the job as it was, no second event)
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	events := e.eventCount(j.ID, "transition")
	again, err := e.jobs.Pause(ctx, u, j.ID)
	if err != nil || again.Status != jobs.StatusPaused || again.PausedFrom != jobs.StatusRunning || e.eventCount(j.ID, "transition") != events {
		t.Fatalf("the second pause: %v, %s from %q, %d transition events (was %d): a no-op", err, again.Status, again.PausedFrom, e.eventCount(j.ID, "transition"), events)
	}
	// the cancel: once; a second is refused by the lifecycle, and so is a pause of a cancelling job, and nothing is written
	if c, err := e.jobs.Cancel(ctx, u, j.ID); err != nil || c.Status != jobs.StatusCancelling {
		t.Fatalf("the cancel of the paused job: %v, %s", err, c.Status)
	}
	before = e.fingerprint()
	_, err = e.jobs.Cancel(ctx, u, j.ID)
	jeNotAllowed(t, "a second cancel", err, "job_transition_illegal")
	_, err = e.jobs.Pause(ctx, u, j.ID)
	jeNotAllowed(t, "the pause of a cancelling job", err, "job_transition_illegal")
	_, err = e.jobs.Resume(ctx, u, j.ID)
	jeNotAllowed(t, "the resume of a cancelling job", err, "not_paused")
	if e.fingerprint() != before {
		t.Fatal("the refused moves of a cancelling job wrote something")
	}
	e.requireBooks("after the idempotence checks")
}

func TestJobsPauseCancel_APauseOfARunningModule1JobIsRefusedByTheDatabaseWritesNothingAndTheJobGoesOn(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m1", 3)
	fake := &jobenginetest.FakeModule{Module: "m1"}
	rig := e.newRig(fake, jobengine.Config{SubmitBatchFor: map[string]int{"m1": 100}})
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatalf("the fixture: %s", e.jobStatus(j.ID))
	}
	before := e.fingerprint()
	_, err := e.jobs.Pause(ctx, u, j.ID)
	jeNotAllowed(t, "the pause of a running module 1 job", err, "job_module_cannot_pause")
	if e.fingerprint() != before || e.jobStatus(j.ID) != jobs.StatusRunning {
		t.Fatal("a refused pause wrote something or moved the job")
	}
	// the control: the capability is the row's. Module 1 given the capability, the same attempt is accepted (and put back)
	e.exec("UPDATE platform.modules SET can_pause = true WHERE id = 'm1'")
	if pj, err := e.jobs.Pause(ctx, u, j.ID); err != nil || pj.Status != jobs.StatusPaused {
		t.Fatalf("with can_pause true the pause of the module 1 job: %v, %s", err, pj.Status)
	}
	e.exec("UPDATE platform.modules SET can_pause = false WHERE id = 'm1'")
	if _, err := e.jobs.Resume(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	if fake.Calls().Pause != 0 {
		t.Fatalf("the engine asked module 1 to pause %d time(s)", fake.Calls().Pause)
	}
	e.requireEnded(j.ID, jobs.StatusSucceeded, 0)
	if strings.Contains(fmt.Sprint(e.jpcTrail(j.ID)), "note:") {
		t.Fatalf("a module 1 job has a module hold note: %v", e.jpcTrail(j.ID))
	}
}

// jpcEventLines is the job's history as one line per event: kind, move, actor, acting user and reason (the same shape the job store tests read).
func (e *jeEnv) jpcEventLines(job string) []string {
	e.t.Helper()
	return e.rows(`SELECT kind || '|' || COALESCE(from_status, '') || '>' || COALESCE(to_status, '') || '|' || actor_kind || '|' || COALESCE(acting_user_id::text, '') || '|' || reason
		FROM platform.job_events WHERE job_id = $1 ORDER BY id`, job)
}
