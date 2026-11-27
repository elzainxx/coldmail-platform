//go:build harness

package harness

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S7 (platform_0018): a job that stays paused for longer than PLATFORM_JOB_PAUSE_MAX_DAYS is ended by the ENGINE. Real Postgres, the real stores, the engine driven tick by tick (Engine.Tick, whose last
// step is the sweep) over the deterministic fake module. THE DIAL IS A DURATION HERE: the engine and the store take seconds; the variable of the service is in days. The clock is the DATABASE's, so a test that wants
// a pause to be old ages paused_at by the database's clock (jplAge, with the lifecycle guard off for the one statement, as the other tests do for a deadline) and one test waits for real seconds. Every test has a
// paired control (a pause under the limit, a job that is not paused, a Go clock that moved a hundred days) that fails when the property breaks. The names of this file start with `jpl`.
//
//	AScheduledJobPausedOverTheLimit...  cancelled at once: free (the platform's own result for every row), the reserve released, the slot freed, one transition event and one note with the limit in its detail; the
//	                                    module is never called; a pause under the limit and a pause the Go clock thinks is old are left alone; a second pass changes nothing (module 2, then module 3)
//	ARunningJobPausedOverTheLimit...    goes through cancelling (module 2 and 3): the engine's own cancelling step then tells the module, records what finished (an item already sent stays sent and charged), releases the
//	                                    rest and ends the job cancelled; the slot stays held until then
//	AResumeAtTheMoment...               the user resumes at the moment of the engine's cancel: exactly one wins, the other is refused as the lifecycle refuses (forced interleave, both proven blocked)
//	ARealShortDial...                   a dial of three seconds, no aging: the pause is cancelled no sooner than three seconds by the database's clock

// jplAge makes a pause old BY THE DATABASE'S CLOCK: paused_at moves back by `by`, with the lifecycle guard switched off for the one statement (no role may write the column).
func (e *jeEnv) jplAge(job string, by time.Duration) {
	e.t.Helper()
	e.exec("ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	tag := e.exec("UPDATE platform.jobs SET paused_at = now() - make_interval(secs => $2::float8) WHERE id = $1::uuid AND status = 'paused'", job, by.Seconds())
	e.exec("ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
	if tag.RowsAffected() != 1 {
		e.t.Fatalf("could not age the pause of job %s: %d rows (it is not a paused job)", job, tag.RowsAffected())
	}
}

// jplTryConfirm creates, previews and confirms a fresh two-row job of the user's on the module and returns the confirm's error (nil: the slot was free and the job is confirmed). The module is not
// started: nothing here ticks the engine.
func (e *jeEnv) jplTryConfirm(user, module string) (jobs.Job, error) {
	e.t.Helper()
	ctx := testCtx(e.t)
	req := jobs.CreateReq{Module: module, Rows: jeAddrs(2, "slot")}
	if module != jobs.ModuleVerify {
		req.Content = jeContent()
	}
	j, err := e.jobs.CreateDraft(ctx, user, req)
	if err != nil {
		e.t.Fatalf("CreateDraft for the slot probe: %v", err)
	}
	p, err := e.jobs.Preview(ctx, user, j.ID)
	if err != nil {
		e.t.Fatalf("Preview for the slot probe: %v", err)
	}
	res, err := e.jobs.Confirm(ctx, user, jobs.ConfirmReq{JobID: j.ID, Fingerprint: p.Fingerprint, Key: e.key()})
	return res.Job, err
}

// jplLimitEvents is what the pause limit wrote in the job's history: the engine's transition (reason beginning pause_limit) and the note with its numbers.
func (e *jeEnv) jplLimitEvents(job, to string) (transitions, notes int64) {
	e.t.Helper()
	transitions = e.int(`SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'transition' AND from_status = 'paused' AND to_status = $2 AND actor_kind = 'engine'
		AND acting_user_id IS NULL AND acting_admin_id IS NULL AND reason LIKE 'pause_limit: %'`, job, to)
	notes = e.int(`SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'note' AND actor_kind = 'engine' AND detail->>'event' = 'pause_limit' AND reason LIKE 'pause_limit: %'`, job)
	return transitions, notes
}

func (e *jeEnv) jplNote(job, key string) string {
	e.t.Helper()
	return e.str(`SELECT detail->>($2::text) FROM platform.job_events WHERE job_id = $1 AND kind = 'note' AND detail->>'event' = 'pause_limit'`, job, key)
}

func (e *jeEnv) jplNoLimitEvents(job string) {
	e.t.Helper()
	if n := e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND reason LIKE 'pause_limit%'", job); n != 0 {
		e.t.Fatalf("job %s has %d event(s) of the pause limit: it was not over the limit", job, n)
	}
}

// ---- 1: a job paused before its start ------------------------------------------------------------------------------------------------------------------------------------------------

func TestPauseLimit_AScheduledJobPausedOverTheLimitIsCancelledAtOnceFreeReleasedItsSlotFreedAndTheLimitIsTheDatabasesClock(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	start := time.Now().Add(2 * time.Hour)
	over, under := e.newUser(), e.newUser()
	j := e.confirmRows(over, "m2", jeAddrs(3, "a"), jeConfirmOpts{Start: &start})
	control := e.confirmRows(under, "m2", jeAddrs(3, "c"), jeConfirmOpts{Start: &start}) // THE CONTROL: paused too, and under the limit
	for _, p := range []struct{ user, job string }{{over, j.ID}, {under, control.ID}} {
		pj, err := e.jobs.Pause(ctx, p.user, p.job)
		if err != nil || pj.Status != jobs.StatusPaused || pj.PausedFrom != jobs.StatusScheduled {
			t.Fatalf("the user's pause of a scheduled job: %v, %s from %q", err, pj.Status, pj.PausedFrom)
		}
	}
	e.requireBal("the credits are held while the job is paused", over, "m2", 997, 3)
	// the pause holds the slot: a second job of the same user on the same module is refused, and nothing it reserved stays behind
	if _, err := e.jplTryConfirm(over, "m2"); !errors.Is(err, jobs.ErrSlotTaken) {
		t.Fatalf("a second job while the first is paused: %v, want ErrSlotTaken", err)
	}
	e.requireBal("a refused confirm holds nothing", over, "m2", 997, 3)

	fake := &jobenginetest.FakeModule{Module: "m2", Manual: true}
	rig := e.newRig(fake, jobengine.Config{PauseMax: time.Hour, SweepEvery: -1})

	// control 1: both pauses are seconds old and the limit is an hour: the sweep runs and finds nothing
	rig.eng.Tick(ctx)
	if s := rig.eng.SweepStats(); s.PauseSweeps != 1 || s.PauseCancelled != 0 || s.PauseCancelling != 0 || s.PauseFailed != 0 {
		t.Fatalf("after the first tick the sweep stats are %+v: it must have run once and ended nothing", s)
	}
	for _, id := range []string{j.ID, control.ID} {
		if e.jobStatus(id) != jobs.StatusPaused {
			t.Fatalf("job %s is %s under the limit: a pause under the limit is left alone", id, e.jobStatus(id))
		}
	}
	// control 2: THE CLOCK IS THE DATABASE'S. A hundred days pass on the engine's own clock and the database's does not move: nothing changes
	rig.clock.Advance(100 * 24 * time.Hour)
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusPaused || e.jobStatus(control.ID) != jobs.StatusPaused {
		t.Fatal("a job was cancelled because the ENGINE's clock moved: the limit must be read from the database's clock")
	}

	// the pause of one job becomes old by the database's clock
	e.jplAge(j.ID, 2*time.Hour)
	rig.eng.Tick(ctx)
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
	if s := rig.eng.SweepStats(); s.PauseCancelled != 1 || s.PauseCancelling != 0 {
		t.Errorf("the sweep stats are %+v, want exactly one job ended directly", s)
	}
	// unsent items are cancelled and free: the platform's own result for every row, no charge
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND module_state = 'cancelled' AND decision = 'free' AND unit IS NULL", j.ID); n != 3 || e.resultCount(j.ID) != 3 {
		t.Errorf("%d free platform results of 3 (%d in all): every unsent row of the job is cancelled and free", n, e.resultCount(j.ID))
	}
	if got := e.ledgerCharges(j.ID); len(got) != 0 {
		t.Errorf("the job charged %v: nothing was sent", got)
	}
	// credits not consumed are released, the slot is freed
	if r, c, l := e.reserveOf(j.ID); r != 3 || c != 0 || l != 3 {
		t.Errorf("the reserve held %d, charged %d, released %d; want 3, 0 and 3", r, c, l)
	}
	e.requireBal("the whole reserve is back", over, "m2", 1000, 0)
	if !ledgerBool(t, e.admin, "SELECT paused_at IS NULL AND paused_from IS NULL AND finished_at IS NOT NULL AND cancel_requested_at IS NULL FROM platform.jobs WHERE id = $1", j.ID) {
		t.Error("the cancelled job still carries its pause, or has no end time, or a cancel request nobody made")
	}
	if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID); n != 0 {
		t.Errorf("a job that never started has %d module-facing items", n)
	}
	if fake.Calls().Total != 0 {
		t.Errorf("the module was called %d times for a job that never reached it", fake.Calls().Total)
	}
	// an event says why, with the limit in its detail
	if tr, notes := e.jplLimitEvents(j.ID, jobs.StatusCancelled); tr != 1 || notes != 1 {
		t.Fatalf("%d transition event(s) and %d note(s) of the pause limit, want 1 and 1 (who: the engine, why: pause_limit)", tr, notes)
	}
	for key, want := range map[string]string{"limit_seconds": "3600", "paused_from": "scheduled", "moved_to": "cancelled", "credits_released": "3", "rows_cancelled_unsent": "3", "event": "pause_limit"} {
		if got := e.jplNote(j.ID, key); got != want {
			t.Errorf("the note says %s = %q, want %q", key, got, want)
		}
	}
	if s := e.jplNote(j.ID, "paused_for_seconds"); s == "" {
		t.Error("the note does not say how long the pause lasted")
	} else if n, err := strconv.Atoi(s); err != nil || n < 7200 || n > 7200+600 {
		t.Errorf("the note says the pause lasted %s seconds, want about two hours (the age the test gave it)", s)
	}
	// the control job and its money are untouched
	if e.jobStatus(control.ID) != jobs.StatusPaused || !ledgerBool(t, e.admin, "SELECT paused_at IS NOT NULL FROM platform.jobs WHERE id = $1", control.ID) {
		t.Errorf("the job paused under the limit is %s", e.jobStatus(control.ID))
	}
	e.requireBal("the control's credits are still held", under, "m2", 997, 3)
	e.jplNoLimitEvents(control.ID)
	// idempotent: the next passes change nothing at all
	again := e.fingerprint()
	rig.eng.Tick(ctx)
	rig.eng.Tick(ctx)
	if got := e.fingerprint(); got != again {
		t.Errorf("a second sweep changed the books or the jobs:\n  before %s\n  after  %s", again, got)
	}
	if tr, notes := e.jplLimitEvents(j.ID, jobs.StatusCancelled); tr != 1 || notes != 1 {
		t.Errorf("after two more passes %d transition(s) and %d note(s) of the pause limit, want still 1 and 1", tr, notes)
	}
	// a resume of the cancelled job is the lifecycle's normal refusal
	_, err := e.jobs.Resume(ctx, over, j.ID)
	jeNotAllowed(t, "the resume of a job the engine cancelled", err, "not_paused")
	// the slot is free: the same user can confirm another job of the same module
	if nj, err := e.jplTryConfirm(over, "m2"); err != nil || nj.Status != jobs.StatusConfirmed {
		t.Fatalf("the slot of a cancelled job is not free: %v, %s", err, nj.Status)
	}
	// and the control, pushed over the limit in its turn, is ended the same way (the sweep was never broken, only not due)
	e.jplAge(control.ID, 3*time.Hour)
	rig.eng.Tick(ctx)
	e.requireEnded(control.ID, jobs.StatusCancelled, 1)
	e.requireBal("the control's credits are back too", under, "m2", 1000, 0)
}

func TestPauseLimit_AModule3JobPausedBeforeItsStartIsCancelledToo_WithNoCreditsToRelease(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	start := time.Now().Add(2 * time.Hour)
	u := e.newUser()
	j := e.confirmRows(u, "m3", jeAddrs(3, "m"), jeConfirmOpts{Start: &start})
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	rig := e.newRig(&jobenginetest.FakeModule{Module: "m3", Manual: true}, jobengine.Config{PauseMax: time.Hour, SweepEvery: -1})
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusPaused {
		t.Fatalf("a module 3 job paused under the limit is %s", e.jobStatus(j.ID))
	}
	e.jplAge(j.ID, 2*time.Hour)
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusCancelled {
		t.Fatalf("a module 3 job paused over the limit is %s", e.jobStatus(j.ID))
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND module_state = 'cancelled' AND decision = 'free'", j.ID); n != 3 {
		t.Errorf("%d free results, want 3", n)
	}
	if n := e.int("SELECT count(*) FROM platform.reserves WHERE job_id = $1", j.ID); n != 0 {
		t.Errorf("a module 3 job holds %d reserves: it holds no credits", n)
	}
	if tr, notes := e.jplLimitEvents(j.ID, jobs.StatusCancelled); tr != 1 || notes != 1 {
		t.Errorf("%d transition event(s) and %d note(s), want 1 and 1", tr, notes)
	}
	if got := e.jplNote(j.ID, "credits_released"); got != "0" {
		t.Errorf("the note says %s credits were released for a module that holds none", got)
	}
	e.requireBooks("after the module 3 job was cancelled by the pause limit")
	if nj, err := e.jplTryConfirm(u, "m3"); err != nil || nj.Status != jobs.StatusConfirmed {
		t.Fatalf("the slot of a cancelled module 3 job is not free: %v, %s", err, nj.Status)
	}
}

// ---- 2: a job paused while running -------------------------------------------------------------------------------------------------------------------------------------------------

// jplRunning is a job that is paused while it runs: the engine started it (the fake module holds every row, settles nothing by itself), one item was in flight and finished (it is recorded and, for module 2,
// charged), the user paused it and the module reported the hold.
func jplRunning(t *testing.T, module string) (e *jeEnv, rig *jeRig, fake *jobenginetest.FakeModule, user string, job jobs.Job, sent string) {
	t.Helper()
	e = newJeEnv(t)
	ctx := testCtx(t)
	user = e.newUser()
	job = e.confirmN(user, module, 6)
	fake = &jobenginetest.FakeModule{Module: module, Manual: true, Plan: jpcPlan(module, 0)}
	rig = e.newRig(fake, jobengine.Config{PauseMax: time.Hour, SweepEvery: -1})
	rig.eng.Tick(ctx)
	if e.jobStatus(job.ID) != jobs.StatusRunning || fake.HeldCount(job.ID) != 6 || fake.ClaimInFlight(job.ID, 1) != 1 {
		t.Fatalf("the fixture: %s, the module holds %d of 6", e.jobStatus(job.ID), fake.HeldCount(job.ID))
	}
	sent = e.rowIDs(job.ID, "pending")[0]
	if !fake.Settle(job.ID, sent, charge.Outcome{Module: module, State: "sent"}) {
		t.Fatal("the in-flight item could not be settled")
	}
	rig.eng.Tick(ctx)
	if e.resultCount(job.ID) != 1 {
		t.Fatalf("%d results after the in-flight item finished, want 1", e.resultCount(job.ID))
	}
	if _, err := e.jobs.Pause(ctx, user, job.ID); err != nil {
		t.Fatalf("the user's pause: %v", err)
	}
	for i := 0; i < 3 && !fake.Paused(job.ID); i++ {
		rig.eng.Tick(ctx)
	}
	if !fake.Paused(job.ID) || e.jobStatus(job.ID) != jobs.StatusPaused {
		t.Fatal("the fixture: the job is not paused and held by its module")
	}
	return e, rig, fake, user, job, sent
}

func TestPauseLimit_AModule2JobPausedWhileRunningOverTheLimitGoesThroughCancellingAndTheEngineEndsItCancelled(t *testing.T) {
	e, rig, fake, u, j, sent := jplRunning(t, "m2")
	ctx := testCtx(t)
	// control: under the limit the paused running job is left alone by the sweep (the module's hold is the other steps' business)
	pausedFor := rig.eng.SweepStats()
	rig.eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusPaused || rig.eng.SweepStats().PauseCancelling != pausedFor.PauseCancelling {
		t.Fatalf("a running job paused under the limit is %s", e.jobStatus(j.ID))
	}
	e.jplNoLimitEvents(j.ID)
	e.requireBal("a paused running job holds its credits", u, "m2", 994, 5) // six reserved, one charged

	e.jplAge(j.ID, 2*time.Hour)
	cancelCalls := fake.Calls().Cancel
	rig.eng.Tick(ctx)
	// THE MOVE: cancelling, never cancelled; the reserve and the slot are held until the engine has ended the job
	if got := e.jobStatus(j.ID); got != jobs.StatusCancelling {
		t.Fatalf("the job paused while running and over the limit is %s, want cancelling (its module holds its items)", got)
	}
	if !ledgerBool(t, e.admin, "SELECT cancel_requested_at IS NOT NULL AND finished_at IS NULL AND paused_at IS NULL AND paused_from IS NULL FROM platform.jobs WHERE id = $1", j.ID) {
		t.Error("the move to cancelling must record the cancel request, end nothing and carry no pause")
	}
	if e.transitionCount(j.ID, jobs.StatusCancelled) != 0 {
		t.Error("the engine took a job its module holds straight to cancelled")
	}
	if fake.Calls().Cancel != cancelCalls {
		t.Errorf("the module was asked to cancel in the same pass that moved the job (%d -> %d): the module is told at the engine's next look", cancelCalls, fake.Calls().Cancel)
	}
	e.requireBal("the credits are held until the engine has ended the job", u, "m2", 994, 5)
	if _, err := e.jplTryConfirm(u, "m2"); !errors.Is(err, jobs.ErrSlotTaken) {
		t.Errorf("a second job while the first is cancelling: %v, want ErrSlotTaken (the slot is freed when the job ends)", err)
	}
	if tr, notes := e.jplLimitEvents(j.ID, jobs.StatusCancelling); tr != 1 || notes != 1 {
		t.Fatalf("%d transition event(s) and %d note(s) of the pause limit, want 1 and 1", tr, notes)
	}
	for key, want := range map[string]string{"limit_seconds": "3600", "paused_from": "running", "moved_to": "cancelling", "credits_released": "0", "rows_cancelled_unsent": "0"} {
		if got := e.jplNote(j.ID, key); got != want {
			t.Errorf("the note says %s = %q, want %q", key, got, want)
		}
	}
	// the engine's own cancelling step does the rest: it tells the module, records what finished and ends the job
	e.tickUntil(rig.eng, j.ID, jobs.StatusCancelled, 6)
	if fake.Calls().Cancel < 1 || fake.Paused(j.ID) {
		t.Errorf("the engine asked the module to cancel %d time(s) and the module still holds the campaign: %v", fake.Calls().Cancel, fake.Paused(j.ID))
	}
	// the item that was sent stays sent and charged; the five that were not are cancelled and free (the module's own report for the ones it held, the platform's for none: the fake held all six)
	got := e.rows(`SELECT ordinal::text || '|' || origin || '|' || module_state || '|' || decision FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal`, j.ID)
	want := []string{"1|module|sent|charge", "2|module|cancelled|free", "3|module|cancelled|free", "4|module|cancelled|free", "5|module|cancelled|free", "6|module|cancelled|free"}
	jpcEqual(t, "the results of the job the pause limit cancelled", got, want)
	if st := e.str("SELECT module_state FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", j.ID, sent); st != "sent" {
		t.Errorf("the item that was sent is now %s: nothing already sent is touched", st)
	}
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
	e.requireSettledLikeTheModule(fake, j.ID)
	if r, c, l := e.reserveOf(j.ID); r != 6 || c != 1 || l != 5 {
		t.Errorf("the reserve held %d, charged %d, released %d; want 6, 1 and 5 (what was sent is charged and not refunded; the rest is released)", r, c, l)
	}
	e.requireBal("after the cancel", u, "m2", 999, 0)
	if e.transitionCount(j.ID, jobs.StatusSucceeded) != 0 {
		t.Error("a job whose cancel was accepted ended succeeded")
	}
	jpcEqual(t, "the history of the job the pause limit cancelled", e.jpcTrail(j.ID), []string{
		"transition:running", "transition:paused", "note:module_paused", "transition:cancelling", "note:pause_limit", "note:module_resumed", "transition:completing", "transition:cancelled"})
	// the slot is free now, and the user's late resume is the lifecycle's normal refusal
	if nj, err := e.jplTryConfirm(u, "m2"); err != nil || nj.Status != jobs.StatusConfirmed {
		t.Fatalf("the slot of the cancelled job is not free: %v, %s", err, nj.Status)
	}
	_, err := e.jobs.Resume(ctx, u, j.ID)
	jeNotAllowed(t, "the resume of a job the engine cancelled", err, "not_paused")
}

func TestPauseLimit_AModule3JobPausedWhileRunningOverTheLimitGoesThroughCancellingAndEndsCancelled(t *testing.T) {
	e, rig, fake, u, j, sent := jplRunning(t, "m3")
	ctx := testCtx(t)
	e.jplAge(j.ID, 2*time.Hour)
	rig.eng.Tick(ctx)
	if got := e.jobStatus(j.ID); got != jobs.StatusCancelling {
		t.Fatalf("the module 3 job paused while running and over the limit is %s, want cancelling", got)
	}
	if n := e.int("SELECT count(*) FROM platform.reserves WHERE job_id = $1", j.ID); n != 0 {
		t.Errorf("a module 3 job holds %d reserves", n)
	}
	e.tickUntil(rig.eng, j.ID, jobs.StatusCancelled, 6)
	if fake.Calls().Cancel < 1 {
		t.Error("the engine never asked module 3 to cancel the campaign it held")
	}
	if st := e.str("SELECT module_state FROM platform.job_results WHERE job_id = $1 AND item_ref = $2", j.ID, sent); st != "sent" {
		t.Errorf("the item that was sent is now %s", st)
	}
	if n := e.resultCount(j.ID); n != 6 {
		t.Errorf("%d results, want 6 (one sent, five cancelled)", n)
	}
	e.requireBooks("after the module 3 job was cancelled by the pause limit")
	if tr, notes := e.jplLimitEvents(j.ID, jobs.StatusCancelling); tr != 1 || notes != 1 {
		t.Errorf("%d transition event(s) and %d note(s) of the pause limit, want 1 and 1", tr, notes)
	}
	if nj, err := e.jplTryConfirm(u, "m3"); err != nil || nj.Status != jobs.StatusConfirmed {
		t.Fatalf("the slot of the cancelled module 3 job is not free: %v, %s", err, nj.Status)
	}
}

// ---- 3: the race ---------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jplBlockedBehind waits until `n` platform_app backends wait for a ROW (the job's, locked FOR UPDATE by the store) behind the holder: each is blocked by the holder or by another waiter, which is how a queue of row locks looks
// to pg_blocking_pids.
func (e *jeEnv) jplBlockedBehind(holder int32, n int) {
	e.t.Helper()
	ws, ok := e.waitUntil(30*time.Second, func(ws []mnyWait) bool {
		var waiters []mnyWait
		for _, w := range ws {
			if w.Event == "transactionid" || w.Event == "tuple" { // a wait for a row (pg_stat_activity cuts a long statement, so its text is not what proves it)
				waiters = append(waiters, w)
			}
		}
		if len(waiters) < n {
			return false
		}
		for _, w := range waiters {
			blocked := false
			for _, b := range w.Blockers {
				if b == holder {
					blocked = true
				}
				for _, o := range waiters {
					if o.PID == b {
						blocked = true
					}
				}
			}
			if !blocked {
				return false
			}
		}
		return true
	})
	if !ok {
		e.t.Fatalf("%d call(s) were never seen waiting for the job's row behind the holder (pid %d): %s", n, holder, mnyDescribe(ws))
	}
}

func TestPauseLimit_AResumeAtTheMomentOfTheEnginesCancelHasExactlyOneWinnerAndTheLoserIsRefusedAsTheLifecycleRefuses(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	start := time.Now().Add(2 * time.Hour)
	// a paused job, its pause two hours old, one dial of an hour
	paused := func() (string, jobs.Job) {
		u := e.newUser()
		j := e.confirmRows(u, "m2", jeAddrs(3, "r"), jeConfirmOpts{Start: &start})
		if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
			t.Fatal(err)
		}
		e.jplAge(j.ID, 2*time.Hour)
		return u, j
	}
	sweep := func() (jobs.PauseLimitResult, error) { return e.jobs.CancelOverLimitPaused(testCtx(t), time.Hour, 0) }

	// A. THE USER IS FIRST. The resume is made and not yet committed (the user's move holds the row), the engine arrives and is PROVEN to wait for it, the user commits: the user won, the engine found a job that is
	// not paused any more and wrote nothing.
	uA, jA := paused()
	h := newJplHolder(t, e)
	pid := h.pid
	h.exec(jpcMoveSQL, jA.ID, jobs.StatusScheduled, "user", uA, "resumed by the user")
	var resA jobs.PauseLimitResult
	var errA error
	doneA := make(chan struct{})
	go func() { defer close(doneA); resA, errA = sweep() }()
	e.jplBlockedBehind(pid, 1)
	select {
	case <-doneA:
		h.rollback()
		t.Fatal("the engine's cancel returned while the user's resume was still open: it did not wait for the row")
	default:
	}
	h.commit()
	<-doneA
	if errA != nil || resA.Found != 1 || resA.Skipped != 1 || resA.Cancelled != 0 || resA.Cancelling != 0 {
		t.Fatalf("the engine that lost the race: %+v, %v; want it to have found the job, skipped it and ended nothing", resA, errA)
	}
	if e.jobStatus(jA.ID) != jobs.StatusScheduled {
		t.Fatalf("the user resumed first and the job is %s", e.jobStatus(jA.ID))
	}
	e.jplNoLimitEvents(jA.ID)
	e.requireBal("the winner's credits are held", uA, "m2", 997, 3)
	// the control of the engine: with the resume gone and the job paused again, a new count starts (a pause under the limit), and when that pause is old the same sweep ends the job
	if _, err := e.jobs.Pause(ctx, uA, jA.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := sweep(); err != nil || res.Found != 0 {
		t.Fatalf("a job paused again right now: %+v, %v; want it not found (a resumed and re-paused job starts a new count)", res, err)
	}
	e.jplAge(jA.ID, 2*time.Hour)
	if res, err := sweep(); err != nil || res.Cancelled != 1 {
		t.Fatalf("the same job, its new pause old: %+v, %v; want it cancelled", res, err)
	}

	// B. THE ENGINE IS FIRST. Its move is open (the row is held), the user's resume arrives and is PROVEN to wait; the engine commits: the user is refused with the lifecycle's own words, typed, with no raw error
	uB, jB := paused()
	h = newJplHolder(t, e)
	pid = h.pid
	h.exec("SELECT 1 FROM platform.jobs WHERE id = $1::uuid FOR UPDATE", jB.ID) // the engine's side holds the row while it decides
	var errB error
	doneB := make(chan struct{})
	go func() { defer close(doneB); _, errB = e.jobs.Resume(testCtx(t), uB, jB.ID) }()
	e.jplBlockedBehind(pid, 1)
	// the engine's work, as the store does it, while the user waits (one statement of the lifecycle as the actor engine, then the commit)
	h.exec(jpcMoveSQL, jB.ID, jobs.StatusCancelled, "engine", nil, "pause_limit: the engine won the race")
	h.commit()
	<-doneB
	jeNotAllowed(t, "the user's resume, which lost", errB, "not_paused")
	if e.jobStatus(jB.ID) != jobs.StatusCancelled {
		t.Fatalf("the engine moved first and the job is %s", e.jobStatus(jB.ID))
	}
	if err := e.jobs.Release(ctx, jB.ID); err != nil { // the move above was a bare one (the holder is no store): the repair step gives the rows their results and the reserve back, as it does for any job ended outside the engine
		t.Fatalf("the repair of the job the holder ended: %v", err)
	}

	// C. THE REAL RACE. Both calls are made while a third session holds the row; both are PROVEN to wait; the session lets go. Exactly one of them wins, and the other gets the answer the lifecycle gives.
	uC, jC := paused()
	h = newJplHolder(t, e)
	pid = h.pid
	h.exec("SELECT 1 FROM platform.jobs WHERE id = $1::uuid FOR UPDATE", jC.ID)
	var rerr, eerr error
	var eres jobs.PauseLimitResult
	doneR, doneE := make(chan struct{}), make(chan struct{})
	go func() { defer close(doneR); _, rerr = e.jobs.Resume(testCtx(t), uC, jC.ID) }()
	go func() { defer close(doneE); eres, eerr = sweep() }()
	e.jplBlockedBehind(pid, 2)
	h.rollback()
	<-doneR
	<-doneE
	if eerr != nil {
		t.Fatalf("the engine's sweep failed in the race: %v", eerr)
	}
	switch e.jobStatus(jC.ID) {
	case jobs.StatusScheduled: // the user won
		if rerr != nil || eres.Cancelled != 0 || eres.Skipped != 1 {
			t.Errorf("the resume won: resume %v, engine %+v; want no error and the engine skipping the job", rerr, eres)
		}
		e.jplNoLimitEvents(jC.ID)
		e.requireBal("the winner's credits are held", uC, "m2", 997, 3)
		t.Logf("NOTE jpl race: the user's resume won the race")
	case jobs.StatusCancelled: // the engine won
		jeNotAllowed(t, "the resume that lost", rerr, "not_paused")
		if eres.Cancelled != 1 {
			t.Errorf("the engine won: %+v, want one job cancelled", eres)
		}
		e.requireEnded(jC.ID, jobs.StatusCancelled, 1)
		t.Logf("NOTE jpl race: the engine's cancel won the race")
	default:
		t.Fatalf("after the race the job is %s: neither the resume nor the cancel took it", e.jobStatus(jC.ID))
	}
	if (rerr == nil) == (eres.Cancelled == 1) {
		t.Fatalf("resume error %v and engine %+v: exactly one of the two must have won", rerr, eres)
	}
	e.requireBooks("after the races")
}

// jplHolder is a session of its own with a transaction open: what a request in flight looks like to the database. It is a superuser connection (the test's), never the code under test.
type jplHolder struct {
	t    *testing.T
	tx   pgx.Tx
	pid  int32
	done bool
}

func newJplHolder(t *testing.T, e *jeEnv) *jplHolder {
	t.Helper()
	ctx := testCtx(t)
	conn := dial(t, e.s.DSN, nil)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("the holder cannot begin: %v", err)
	}
	h := &jplHolder{t: t, tx: tx}
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&h.pid); err != nil {
		t.Fatalf("the holder's pid: %v", err)
	}
	t.Cleanup(func() {
		if !h.done {
			_ = tx.Rollback(context.Background())
		}
	})
	return h
}

func (h *jplHolder) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.tx.Exec(testCtx(h.t), sql, args...); err != nil {
		h.t.Fatalf("the holder's statement failed: %v (sql: %s)", err, mnyOneLine(sql))
	}
}

func (h *jplHolder) commit() {
	h.t.Helper()
	h.done = true
	if err := h.tx.Commit(testCtx(h.t)); err != nil {
		h.t.Fatalf("the holder's commit: %v", err)
	}
}

func (h *jplHolder) rollback() {
	h.t.Helper()
	h.done = true
	if err := h.tx.Rollback(testCtx(h.t)); err != nil {
		h.t.Fatalf("the holder's rollback: %v", err)
	}
}

// ---- 4: a real short dial -----------------------------------------------------------------------------------------------------------------------------------------------------------

func TestPauseLimit_ARealShortDialCancelsNoSoonerThanItsSecondsByTheDatabasesClock(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	start := time.Now().Add(2 * time.Hour)
	u := e.newUser()
	j := e.confirmRows(u, "m2", jeAddrs(2, "s"), jeConfirmOpts{Start: &start})
	if _, err := e.jobs.Pause(ctx, u, j.ID); err != nil {
		t.Fatal(err)
	}
	rig := e.newRig(&jobenginetest.FakeModule{Module: "m2", Manual: true}, jobengine.Config{PauseMax: 3 * time.Second, SweepEvery: -1})
	deadline := time.Now().Add(90 * time.Second)
	ticks := 0
	for e.jobStatus(j.ID) == jobs.StatusPaused {
		if time.Now().After(deadline) {
			t.Fatalf("the pause was not cancelled within 90 seconds of a dial of three (%d ticks, sweeps %+v)", ticks, rig.eng.SweepStats())
		}
		rig.eng.Tick(ctx)
		ticks++
		time.Sleep(300 * time.Millisecond)
	}
	if ticks < 2 {
		t.Errorf("the pause was cancelled at the first tick: a pause that was seconds old was judged over a limit of three")
	}
	// by the database's clock: the two events are its own, and the cancel came no sooner than the dial after the pause
	gap := e.str(`SELECT extract(epoch FROM (c.created_at - p.created_at))::text FROM platform.job_events p, platform.job_events c
		WHERE p.job_id = $1 AND p.kind = 'transition' AND p.to_status = 'paused' AND c.job_id = $1 AND c.kind = 'transition' AND c.to_status = 'cancelled'`, j.ID)
	secs, err := strconv.ParseFloat(gap, 64)
	if err != nil || secs < 3 || secs > 60 {
		t.Errorf("the job was cancelled %s seconds after it was paused, want at least 3 (the dial) and not a minute (%v)", gap, err)
	}
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
}
