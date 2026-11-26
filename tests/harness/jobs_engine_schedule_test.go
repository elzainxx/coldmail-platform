//go:build harness

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D8 and D9 against a REAL Postgres: the engine's work order and backoff (platform_0015: platform.jobs.next_poll_at and idle_polls, the trigger that wakes a job on every move, the store's EngineWork
// and NotePoll), and the rows of a send job in public.job_items written once and marked when the job ends without sending them (EnsureJobItems, markNeverSubmittedItems). Helpers: jobs_engine_helpers_test.go.
// Every test starts with module 1's vendor unit OFF (newJeEnv) and has a paired control.
//
//	TheDatabase...                  the trigger and the walls of platform_0015: a move wakes the job (the user's, an administrator's, the engine's), an annotation does not; a wait past 600 s, a negative count and
//	                                a second mark of items_ensured_at are refused (each with its control)
//	NotePoll...                     the store's note: progress resets, no progress doubles up to the ceiling and never past it, a job that is no longer in the status the step read is left alone
//	EngineWork...                   the due jobs oldest-due-first; a start deadline and the repair of an ended job's reserve are due whatever their backoff
//	AHundredAndFifty...             THE EXIT TEST: 150 jobs whose module never answers and one new job: the new job is served within ONE tick, and the same arrangement without the engine's memory starves it
//	AModuleThatRecovers...          real time: a backed-off job is served again within the cap and its backoff resets
//	AStaleJob...                    real time: a backed-off stale job raises its stale event within the cap
//	AJobTheUserCancels...           a move wakes the job: the module is asked to cancel in the very next tick
//	TheRowsOfAWholeJob...           EnsureJobItems writes every pending row once, in pages, and records it; the engine asks once per job (a probe counts the INSERT statements)
//	EveryEnd...                     the terminal marker: Expire, CancelBeforeStart, FailJob and the cancelling end mark only the never-submitted items of THIS job, idempotently
//	NoModuleStatement...            module 2 and 3 never read an item's status (db/module-statements.txt), so the marker is the platform's record and not a lock

// ---- helpers --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jesRefusal is the SQLSTATE and the constraint of a refusal of the database (the test fails when the error is not one).
func jesRefusal(t *testing.T, what string, err error) (code, constraint string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: want a refusal of the database, got %v", what, err)
	}
	return pe.Code, pe.ConstraintName
}

// jesStuck is a module that cannot answer for the jobs it is told are stuck (every call is a wait) and is the fake for every other job. It counts the calls that reach it by job: who the engine looked at.
type jesStuck struct {
	*jobenginetest.FakeModule
	mu      sync.Mutex
	stuck   map[string]bool
	touched map[string]int
}

func newJesStuck(f *jobenginetest.FakeModule) *jesStuck {
	return &jesStuck{FakeModule: f, stuck: map[string]bool{}, touched: map[string]int{}}
}

func (s *jesStuck) set(job string, stuck bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck[job] = stuck
}

func (s *jesStuck) touch(job string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched[job]++
	return s.stuck[job]
}

func (s *jesStuck) touches(job string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.touched[job]
}

func (s *jesStuck) resetTouches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = map[string]int{}
}

func (s *jesStuck) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	if s.touch(j.ID) {
		return jobengine.SubmitResult{}, jobengine.Wait("the module cannot answer for this job")
	}
	return s.FakeModule.Submit(ctx, j, rows)
}

func (s *jesStuck) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	if s.touch(j.ID) {
		return jobengine.Observation{}, jobengine.Wait("the module cannot answer for this job")
	}
	return s.FakeModule.Observe(ctx, j, fn)
}

func (s *jesStuck) Cancel(ctx context.Context, j jobs.EngineJob) error {
	if s.touch(j.ID) {
		return jobengine.Wait("the module cannot answer for this job")
	}
	return s.FakeModule.Cancel(ctx, j)
}

// idle is idle_polls of the job.
func (e *jeEnv) idle(job string) int64 {
	return e.int("SELECT idle_polls FROM platform.jobs WHERE id = $1::uuid", job)
}

// waitTenths is how many tenths of a second the database says remain until the job's next_poll_at (negative: it has passed). The job must have one.
func (e *jeEnv) waitTenths(job string) int64 {
	e.t.Helper()
	return e.int("SELECT round(extract(epoch FROM next_poll_at - now()) * 10)::bigint FROM platform.jobs WHERE id = $1::uuid AND next_poll_at IS NOT NULL", job)
}

// wakeless says whether the job has NO next_poll_at (it is due now).
func (e *jeEnv) isDueNow(job string) bool {
	return e.int("SELECT count(*) FROM platform.jobs WHERE id = $1::uuid AND next_poll_at IS NULL AND idle_polls = 0", job) == 1
}

// items is the job's public.job_items rows by status: "cancelled=2,pending=3".
func (e *jeEnv) items(job string) string {
	e.t.Helper()
	return strings.Join(e.rows(`SELECT status || '=' || count(*) FROM public.job_items WHERE campaign_id = $1::uuid GROUP BY status ORDER BY status`, job), ",")
}

// ---- the database: a move wakes the job, the walls of the annotations ---------------------------------------------------------------------------------------------------------------------------

func TestJobEngineSchedule_TheDatabaseWakesAJobOnEveryMoveAndNotOnAnAnnotationAndHoldsTheWallsOfItsColumns(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.running(u, "m2", 2)

	backOff := func(job, status string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			if wrote, err := e.jobs.NotePoll(ctx, job, status, false, 10*time.Second, 40*time.Second); err != nil || !wrote {
				t.Fatalf("NotePoll (no progress) of job %s in %s: wrote %v, %v", job, status, wrote, err)
			}
		}
		if e.idle(job) != 2 || e.waitTenths(job) <= 0 {
			t.Fatalf("the fixture: the job is not backed off (idle %d)", e.idle(job))
		}
	}
	backOff(j.ID, jobs.StatusRunning)

	// the control: what is not a move does not wake it (an annotation, a stale alert, a flag)
	next := e.str("SELECT next_poll_at::text FROM platform.jobs WHERE id = $1::uuid", j.ID)
	if err := e.jobs.NoteProgress(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if wrote, err := e.jobs.AlertStale(ctx, j.ID, time.Hour, map[string]any{"k": "v"}); err != nil || !wrote {
		t.Fatalf("AlertStale: %v, %v", wrote, err)
	}
	if err := e.jobs.Flag(ctx, j.ID, map[string]any{"reason": "a flag"}); err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT next_poll_at::text FROM platform.jobs WHERE id = $1::uuid", j.ID); got != next || e.idle(j.ID) != 2 {
		t.Fatalf("an annotation changed the backoff: next_poll_at %s -> %s, idle %d", next, got, e.idle(j.ID))
	}

	// the user's cancel of a running job wakes it
	cj, err := e.jobs.CancelBeforeStart(ctx, u, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelling {
		t.Fatalf("the user's cancel: %v, %v", cj.Status, err)
	}
	if !e.isDueNow(j.ID) {
		t.Fatalf("a job the user cancelled kept its backoff (idle %d): it would wait it out before the module was asked to cancel", e.idle(j.ID))
	}
	if _, ok := e.workOf(j.ID); !ok {
		t.Fatal("a job the user cancelled is not work")
	}

	// an administrator's move wakes it too (the same job, backed off again, ended by hand through the one sanctioned function)
	backOff(j.ID, jobs.StatusCancelling)
	e.exec("SELECT platform.job_transition($1::uuid, 'failed', 'admin', $2::uuid, 'ended by hand: test')", j.ID, mnyAdminA)
	if e.jobStatus(j.ID) != jobs.StatusFailed || !e.isDueNow(j.ID) {
		t.Fatalf("after an administrator's move the job is %s and due now %v", e.jobStatus(j.ID), e.isDueNow(j.ID))
	}

	// the engine's own move wakes the job as well (confirmed -> running)
	c := e.confirmN(e.newUser(), "m2", 1)
	backOff(c.ID, jobs.StatusConfirmed)
	if r, err := e.jobs.Start(ctx, c.ID, ""); err != nil || r.Status != jobs.StatusRunning || !e.isDueNow(c.ID) {
		t.Fatalf("Start: %v, %v (due now %v)", r.Status, err, e.isDueNow(c.ID))
	}

	// ---- the walls, with their controls ----
	k := e.running(e.newUser(), "m2", 1)
	_, err = e.admin.Exec(ctx, "UPDATE platform.jobs SET next_poll_at = now() + interval '601 seconds' WHERE id = $1::uuid", k.ID)
	if code, con := jesRefusal(t, "a wait of 601 seconds", err); code != "23514" || con != "jobs_next_poll_within_the_cap" {
		t.Errorf("a wait of 601 seconds: %s / %s, want 23514 / jobs_next_poll_within_the_cap", code, con)
	}
	if _, err := e.admin.Exec(ctx, "UPDATE platform.jobs SET next_poll_at = now() + interval '600 seconds' WHERE id = $1::uuid", k.ID); err != nil {
		t.Errorf("the control: a wait of exactly 600 seconds was refused: %v", err)
	}
	_, err = e.admin.Exec(ctx, "UPDATE platform.jobs SET idle_polls = -1 WHERE id = $1::uuid", k.ID)
	if code, con := jesRefusal(t, "a negative count", err); code != "23514" || con != "jobs_idle_polls_not_negative" {
		t.Errorf("a negative idle_polls: %s / %s, want 23514 / jobs_idle_polls_not_negative", code, con)
	}
	if _, err := e.admin.Exec(ctx, "UPDATE platform.jobs SET idle_polls = 7 WHERE id = $1::uuid", k.ID); err != nil {
		t.Errorf("the control: idle_polls = 7 was refused: %v", err)
	}
	// items_ensured_at is set once
	if _, err := e.admin.Exec(ctx, "UPDATE platform.jobs SET items_ensured_at = now() WHERE id = $1::uuid", k.ID); err != nil {
		t.Fatalf("the control: the first mark was refused: %v", err)
	}
	if _, err := e.admin.Exec(ctx, "UPDATE platform.jobs SET items_ensured_at = items_ensured_at, module_job_ref = 'ref-1' WHERE id = $1::uuid", k.ID); err != nil {
		t.Errorf("the control: a write that leaves the mark as it is was refused: %v", err)
	}
	for what, set := range map[string]string{"a later time": "items_ensured_at + interval '1 second'", "no time": "NULL"} {
		_, err := e.admin.Exec(ctx, "UPDATE platform.jobs SET items_ensured_at = "+set+" WHERE id = $1::uuid", k.ID)
		if code, con := jesRefusal(t, "moving the mark to "+what, err); code != "23514" || con != "jobs_items_ensured_is_set_once" {
			t.Errorf("moving items_ensured_at to %s: %s / %s, want 23514 / jobs_items_ensured_is_set_once", what, code, con)
		}
	}
}

// ---- the store's note ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobEngineSchedule_NotePollDoublesTheWaitToTheCeilingAndNeverPastItResetsOnProgressAndLeavesAJobThatMovedAlone(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	j := e.running(e.newUser(), "m2", 2)

	// no progress: 2, 4, 8 and 8 seconds (the exponent is the count of idle steps before this one; the ceiling is 8)
	for i, want := range []int64{20, 40, 80, 80} { // tenths of a second
		if wrote, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusRunning, false, 2*time.Second, 8*time.Second); err != nil || !wrote {
			t.Fatalf("note %d: wrote %v, %v", i+1, wrote, err)
		}
		if got := e.idle(j.ID); got != int64(i+1) {
			t.Fatalf("after note %d: idle_polls = %d, want %d", i+1, got, i+1)
		}
		// the wait was counted from the statement's own now() and is read a moment later: it is the wanted wait less what has gone by (a second and a half is more than any runner needs)
		if got := e.waitTenths(j.ID); got > want || got < want-15 {
			t.Fatalf("after note %d: %d tenths of a second until the next look, want %d (or at most 1.5 s less)", i+1, got, want)
		}
	}
	// the control: progress resets the count, and the job is due at once (next_poll_at is now, so it queues behind jobs that became due before it)
	if wrote, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusRunning, true, 2*time.Second, 8*time.Second); err != nil || !wrote {
		t.Fatalf("a note of progress: %v, %v", wrote, err)
	}
	if e.idle(j.ID) != 0 || e.int("SELECT (next_poll_at <= now())::int FROM platform.jobs WHERE id = $1::uuid", j.ID) != 1 {
		t.Fatalf("after progress: idle %d, due %d", e.idle(j.ID), e.int("SELECT (next_poll_at <= now())::int FROM platform.jobs WHERE id = $1::uuid", j.ID))
	}
	// and the count starts again from the base: 2 s, not the 8 of the ceiling
	if _, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusRunning, false, 2*time.Second, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := e.waitTenths(j.ID); got > 20 || got < 5 {
		t.Errorf("the first wait after progress is %d tenths of a second, want about 20: the backoff did not reset", got)
	}

	// a base of zero is a backoff that is off: due again at once, and still counted
	if _, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusRunning, false, 0, 0); err != nil {
		t.Fatal(err)
	}
	if e.int("SELECT (next_poll_at <= now())::int FROM platform.jobs WHERE id = $1::uuid", j.ID) != 1 {
		t.Error("with the backoff off the job is not due again at once")
	}

	// a job that is no longer in the status the step read keeps what the database gave it: a note for the wrong status writes nothing
	before := e.str("SELECT idle_polls::text || '/' || COALESCE(next_poll_at::text, '-') FROM platform.jobs WHERE id = $1::uuid", j.ID)
	if wrote, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusConfirmed, false, 2*time.Second, 8*time.Second); err != nil || wrote {
		t.Fatalf("a note in the wrong status: wrote %v, %v", wrote, err)
	}
	if after := e.str("SELECT idle_polls::text || '/' || COALESCE(next_poll_at::text, '-') FROM platform.jobs WHERE id = $1::uuid", j.ID); after != before {
		t.Fatalf("a note in the wrong status changed the job: %s -> %s", before, after)
	}
	// an ended job is never written, an unknown job is nobody's, a bad id is not found
	if _, err := e.jobs.FailJob(ctx, j.ID, "ended for the test"); err != nil {
		t.Fatal(err)
	}
	if wrote, err := e.jobs.NotePoll(ctx, j.ID, jobs.StatusFailed, false, 2*time.Second, 8*time.Second); err != nil || wrote {
		t.Fatalf("a note for an ended job: wrote %v, %v", wrote, err)
	}
	if wrote, err := e.jobs.NotePoll(ctx, "00000000-0000-4000-8000-0000000000ff", jobs.StatusRunning, false, time.Second, 2*time.Second); err != nil || wrote {
		t.Fatalf("a note for an unknown job: wrote %v, %v", wrote, err)
	}
	if _, err := e.jobs.NotePoll(ctx, "not-a-uuid", jobs.StatusRunning, false, time.Second, 2*time.Second); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("a note for an id that is no uuid: %v, want ErrNotFound", err)
	}
	// the backoff the store takes: a base of 0 or more, a ceiling between it and ten minutes
	r := e.running(e.newUser(), "m2", 1)
	for what, c := range map[string]struct{ base, ceiling time.Duration }{
		"a negative base":                   {-time.Second, time.Second},
		"a ceiling under the base":          {5 * time.Second, time.Second},
		"a ceiling past the wall of the DB": {time.Second, jobs.MaxPollBackoff + time.Second},
	} {
		if _, err := e.jobs.NotePoll(ctx, r.ID, jobs.StatusRunning, false, c.base, c.ceiling); !errors.Is(err, jobs.ErrBadRequest) {
			t.Errorf("%s: %v, want ErrBadRequest", what, err)
		}
	}
	if e.idle(r.ID) != 0 {
		t.Fatal("a refused note was counted")
	}
	if _, err := e.jobs.NotePoll(ctx, r.ID, jobs.StatusRunning, false, time.Second, jobs.MaxPollBackoff); err != nil {
		t.Errorf("the control: a ceiling of exactly ten minutes was refused: %v", err)
	}
}

// ---- the work: due, oldest due first, and what is due whatever the backoff ----------------------------------------------------------------------------------------------------------------

func TestJobEngineSchedule_EngineWorkReturnsTheDueJobsOldestDueFirstAndTheDeadlineAndTheRepairAreDueWhateverTheirBackoff(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	a := e.running(e.newUser(), "m2", 1) // backed off for five minutes
	b := e.running(e.newUser(), "m2", 1) // due since an hour
	c := e.running(e.newUser(), "m2", 1) // never looked at: due since its confirm
	e.exec("UPDATE platform.jobs SET next_poll_at = now() + interval '5 minutes', idle_polls = 3 WHERE id = $1::uuid", a.ID)
	e.exec("UPDATE platform.jobs SET next_poll_at = now() - interval '1 hour', idle_polls = 2 WHERE id = $1::uuid", b.ID)

	got := func() []string {
		t.Helper()
		var ids []string
		for _, w := range e.work(1000) {
			ids = append(ids, w.ID)
		}
		return ids
	}
	if ids := got(); len(ids) != 2 || ids[0] != b.ID || ids[1] != c.ID {
		t.Fatalf("the work is %v, want the job due since an hour (%s) and then the job never looked at (%s), and not the backed-off one (%s)", ids, b.ID, c.ID, a.ID)
	}
	if one := e.work(1); len(one) != 1 || one[0].ID != b.ID {
		t.Errorf("a limit of one returned %d job(s), want the oldest due", len(one))
	}
	if w, ok := e.workOf(b.ID); !ok || w.IdlePolls != 2 || w.NextPollAt == nil || w.ItemsEnsuredAt != nil {
		t.Errorf("the engine's view of a backed-off job: %+v", w)
	}
	// the control: when its wait is over the backed-off job is work again
	e.exec("UPDATE platform.jobs SET next_poll_at = now() - interval '1 second' WHERE id = $1::uuid", a.ID)
	if _, ok := e.workOf(a.ID); !ok {
		t.Fatal("a job whose wait is over is not work")
	}

	// a start deadline that has come is due whatever the backoff (the job is expired on time); the control is the same job inside its deadline, which waits
	late, soon := "0e000000-0000-4000-8000-0000000000b1", "0e000000-0000-4000-8000-0000000000b2"
	jobInsertAtWith(t, e.admin, late, e.newUser(), "m2", "confirmed", jobOpts{Deadline: jobDeadlinePast})
	jobInsertAt(t, e.admin, soon, e.newUser(), "m2", "confirmed")
	e.exec("UPDATE platform.jobs SET next_poll_at = now() + interval '5 minutes', idle_polls = 4 WHERE id IN ($1::uuid, $2::uuid)", late, soon)
	if w, ok := e.workOf(late); !ok || !w.DeadlinePassed {
		t.Errorf("a confirmed job past its start deadline is not work while it is backed off (listed %v, deadline passed %v): it would not be expired on time", ok, w.DeadlinePassed)
	}
	if _, ok := e.workOf(soon); ok {
		t.Error("the control: a confirmed job INSIDE its start deadline and backed off is work")
	}

	// the repair of an ended job's reserve is due whatever the backoff; the control is an active job with the same wait
	r := e.confirmN(e.newUser(), "m2", 2)
	jobWalkTo(t, e.admin, r.ID, jobs.StatusFailed)
	e.exec("UPDATE platform.jobs SET next_poll_at = now() + interval '5 minutes', idle_polls = 4 WHERE id = $1::uuid", r.ID)
	if w, ok := e.workOf(r.ID); !ok || w.ReserveRemainder != 2 {
		t.Errorf("an ended job that still holds its reserve is not in the repair set while its backoff runs (listed %v, remainder %d)", ok, w.ReserveRemainder)
	}
	if err := e.jobs.Release(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.workOf(r.ID); ok {
		t.Error("a repaired job is still work")
	}
}

// ---- THE EXIT TEST of D8 ----------------------------------------------------------------------------------------------------------------------------------------------------------------------

// A hundred and fifty confirmed jobs of a hundred and fifty users whose module answers a wait for ever, and then ONE new job of a new user. The engine looks at most at a hundred jobs a tick. THE CONTROL is the same
// arrangement without the engine's memory of its own looks (the store's backoff columns are cleared before every tick, which is what an engine that never told the store anything had): the hundred oldest are
// looked at at every tick, the other fifty and the new job never are. THE TEST is the real thing: the stuck jobs back off (idle_polls grows, next_poll_at moves out and never past the cap), and the new job is
// submitted by the very next tick, a tick that looks at nothing else. Then the module recovers for one stuck job, which is served again within its cap with a backoff that has reset.
func TestJobEngineSchedule_AHundredAndFiftyStuckJobsDoNotKeepANewJobFromBeingServedWithinOneTick(t *testing.T) {
	e := newJeEnv(t)
	const stuck, limit = 150, 100
	fake := &jobenginetest.FakeModule{}
	mod := newJesStuck(fake)
	old := make([]string, stuck)
	for i := range old {
		old[i] = e.confirmN(e.newUser(), "m2", 1).ID
		mod.set(old[i], true)
	}
	fresh := e.confirmN(e.newUser(), "m2", 3) // confirmed after every stuck job: the youngest, the one the old order starves
	ctx := testCtx(t)                         // the time budget of the engine starts here: building the jobs above took most of what the creation needs
	// the waits are long (a minute to start with, four at most), so none ends while the test runs; the cap is the dial's, inside a quarter of the staleness limit
	cfg := jobengine.Config{Tick: time.Minute, BackoffCap: 4 * time.Minute, SettleEvery: -1, WorkLimit: limit, StaleAfter: map[string]time.Duration{"m2": time.Hour}}
	eng := e.engineOf(jeSingleModule{mod}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()), cfg)
	forget := func() {
		e.exec("UPDATE platform.jobs SET next_poll_at = NULL, idle_polls = 0 WHERE status = 'confirmed'")
	}

	// ---- the control: an engine with no memory of its looks ----
	for i := 0; i < 3; i++ {
		forget()
		eng.Tick(ctx)
	}
	for i, id := range old {
		want := 3 // the hundred oldest are looked at at every tick ...
		if i >= limit {
			want = 0 // ... and the rest of the stuck jobs never
		}
		if got := mod.touches(id); got != want {
			t.Fatalf("the control: stuck job %d was looked at %d times in three ticks, want %d: the arrangement does not starve what it is meant to starve", i+1, got, want)
		}
	}
	if got := mod.touches(fresh.ID); got != 0 || e.jobStatus(fresh.ID) != jobs.StatusConfirmed {
		t.Fatalf("the control: the new job was looked at %d times and is %s: with no memory of the engine's looks the hundred oldest jobs must keep it waiting", got, e.jobStatus(fresh.ID))
	}

	// ---- the engine with its memory: the stuck jobs back off ----
	mod.set(fresh.ID, true) // for the ramp the youngest job is one more whose module does not answer
	forget()
	mod.resetTouches()
	steps := eng.Stats().Steps
	eng.Tick(ctx) // a hundred jobs ...
	eng.Tick(ctx) // ... and the other fifty-one: the jobs that backed off are not due
	if got := eng.Stats().Steps - steps; got != stuck+1 {
		t.Fatalf("two ticks made %d steps, want %d: a hundred in the first and the fifty-one that were not looked at in the second, none twice", got, stuck+1)
	}
	for _, id := range append(append([]string(nil), old...), fresh.ID) {
		if mod.touches(id) != 1 {
			t.Fatalf("job %s was looked at %d times in the two ticks, want once", id, mod.touches(id))
		}
	}
	// the backoff is visible: every job has idle_polls = 1 and its next look is about a minute away (and never more than the first wait)
	inWindow := func(idle int, loSec, hiSec int) int64 {
		return e.int(`SELECT count(*) FROM platform.jobs WHERE status = 'confirmed' AND idle_polls = $1 AND next_poll_at > now() + make_interval(secs => $2::double precision)
			AND next_poll_at <= now() + make_interval(secs => $3::double precision)`, idle, float64(loSec), float64(hiSec))
	}
	if n := inWindow(1, 30, 60); n != stuck+1 {
		t.Fatalf("after the first look %d of %d jobs have idle_polls = 1 and a wait of up to a minute", n, stuck+1)
	}
	if w := e.work(1000); len(w) != 0 {
		t.Fatalf("%d job(s) are still due after every one of them backed off", len(w))
	}
	// the wait doubles with every look and stops at the cap: 2, 4 and then 4 minutes again (the waits are made to end by moving the stamps, which waiting does)
	for round, want := range []struct {
		idle, loSec, hiSec int
	}{{2, 90, 120}, {3, 210, 240}, {4, 210, 240}} {
		e.exec("UPDATE platform.jobs SET next_poll_at = now() - interval '1 second' WHERE status = 'confirmed'")
		eng.Tick(ctx)
		eng.Tick(ctx)
		if n := inWindow(want.idle, want.loSec, want.hiSec); n != stuck+1 {
			t.Fatalf("round %d: %d of %d jobs have idle_polls = %d and a wait between %d and %d seconds", round+2, n, stuck+1, want.idle, want.loSec, want.hiSec)
		}
	}
	if over := e.int(`SELECT count(*) FROM platform.jobs WHERE status = 'confirmed' AND next_poll_at > now() + interval '240 seconds'`); over != 0 {
		t.Fatalf("%d job(s) are left alone for longer than the cap", over)
	}

	// ---- the new job ----
	nu := e.newUser()
	n := e.confirmN(nu, "m2", 3)
	before, calls := eng.Stats().Steps, fake.Calls().Submit
	eng.Tick(ctx)
	if got := eng.Stats().Steps - before; got != 1 {
		t.Fatalf("the tick looked at %d jobs, want 1: only the new job is due, and not one of the stuck ones", got)
	}
	if e.jobStatus(n.ID) != jobs.StatusRunning || fake.HeldCount(n.ID) != 3 || fake.Calls().Submit != calls+1 {
		t.Fatalf("the new job is %s and the module holds %d of its rows after ONE tick (%d submit(s)): want running, 3 and 1", e.jobStatus(n.ID), fake.HeldCount(n.ID), fake.Calls().Submit-calls)
	}
	if e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND submitted_at IS NOT NULL", n.ID) != 3 {
		t.Fatal("the new job's rows were not stamped as handed over")
	}

	// ---- a module that recovers: its job is served again within the cap, and the backoff resets ----
	probe := old[0]
	mod.set(probe, false)
	if wait := e.waitTenths(probe); wait > 2400 || wait <= 0 {
		t.Fatalf("the probe's next look is %d tenths of a second away, want within the cap (240 s)", wait)
	}
	if _, ok := e.workOf(probe); ok {
		t.Fatal("the probe is due before its wait is over")
	}
	e.exec("UPDATE platform.jobs SET next_poll_at = next_poll_at - interval '240 seconds' WHERE id = $1::uuid", probe) // the whole cap passes
	eng.Tick(ctx)                                                                                                      // the new job, which runs now, is due again too: the tick looks at it and at the probe
	if e.jobStatus(probe) != jobs.StatusRunning || fake.HeldCount(probe) != 1 {
		t.Fatalf("a job whose module recovered: status %s, the module holds %d of its rows; want running and 1", e.jobStatus(probe), fake.HeldCount(probe))
	}
	if !e.isDueNow(probe) {
		t.Errorf("the recovered job's backoff did not reset (idle_polls %d)", e.idle(probe))
	}
	e.requireBooks("after the exit test of the work order")
}

// ---- real time ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobEngineSchedule_AModuleThatRecoversIsServedAgainWithinTheCapAndTheBackoffResets(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	j := e.confirmN(e.newUser(), "m2", 2)
	fake := &jobenginetest.FakeModule{}
	fake.SetDown(true)
	const capSec = 3
	eng := e.engineOf(jeSingleModule{fake}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()),
		jobengine.Config{Tick: 500 * time.Millisecond, BackoffCap: capSec * time.Second, SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}})

	// the engine looks, the module does not answer, the waits are 0.5, 1, 2 and 3 seconds (the cap)
	deadline := time.Now().Add(40 * time.Second)
	for e.idle(j.ID) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("after 40 seconds the engine had looked at the job %d times, want 4", e.idle(j.ID))
		}
		eng.Tick(ctx)
		time.Sleep(100 * time.Millisecond)
	}
	if w := e.waitTenths(j.ID); w <= 0 || w > capSec*10 {
		t.Fatalf("the next look is %d tenths of a second away, want a wait of more than nothing and at most the cap (%d s)", w, capSec)
	}
	// the module recovers: the control is that the job is NOT served at once, its wait is real
	fake.SetDown(false)
	calls := fake.Calls().Total
	eng.Tick(ctx)
	if fake.Calls().Total != calls || e.jobStatus(j.ID) != jobs.StatusConfirmed {
		t.Fatalf("a job that is backed off was served before its wait was over (module calls %d -> %d, status %s)", calls, fake.Calls().Total, e.jobStatus(j.ID))
	}
	// within the cap it is served again, and the backoff has reset
	time.Sleep(capSec*time.Second + 400*time.Millisecond)
	eng.Tick(ctx)
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 2 {
		t.Fatalf("a wait of at most %d s later the job is %s and the module holds %d of its rows; want running and 2", capSec, e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	if !e.isDueNow(j.ID) {
		t.Errorf("the backoff did not reset when the job was served (idle_polls %d)", e.idle(j.ID))
	}
}

func TestJobEngineSchedule_AStaleJobThatIsBackedOffStillRaisesItsStaleEventWithinTheCap(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	j := e.running(e.newUser(), "m2", 3)
	fake := &jobenginetest.FakeModule{}
	fake.SetDown(true)
	const capSec = 2
	eng := e.engineOf(jeSingleModule{fake}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()),
		jobengine.Config{Tick: 500 * time.Millisecond, BackoffCap: capSec * time.Second, SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}})
	deadline := time.Now().Add(40 * time.Second)
	for e.idle(j.ID) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("after 40 seconds the engine had looked at the job %d times, want 3", e.idle(j.ID))
		}
		eng.Tick(ctx)
		time.Sleep(100 * time.Millisecond)
	}
	if e.eventCount(j.ID, "stale") != 0 {
		t.Fatal("the job went stale inside its limit")
	}
	// by the DATABASE's clock the job has now been silent for two hours, a limit of one: the next look finds it stale. The backoff holds that look back for at most the cap
	e.ageProgress(j.ID, 2*time.Hour)
	eng.Tick(ctx)
	if e.eventCount(j.ID, "stale") != 0 {
		t.Fatalf("the control: a tick inside the wait raised the stale event (the job was looked at while it was backed off)")
	}
	time.Sleep(capSec*time.Second + 400*time.Millisecond)
	eng.Tick(ctx)
	if n := e.eventCount(j.ID, "stale"); n != 1 {
		t.Fatalf("%d stale events a cap (%d s) after the limit passed, want exactly 1", n, capSec)
	}
	if e.jobStatus(j.ID) != jobs.StatusRunning || e.resultCount(j.ID) != 0 || e.chargeRows(j.ID) != 0 {
		t.Fatal("a stale job was moved, or something was recorded for it")
	}
}

// A job the user moves is not left to wait out its backoff: the very next tick asks the module to cancel it. The control is the same job with no move: the module is not asked.
func TestJobEngineSchedule_AJobTheUserCancelsIsNotLeftToWaitOutItsBackoff(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmN(u, "m2", 3)
	fake := &jobenginetest.FakeModule{}
	eng := e.engineOf(jeSingleModule{fake}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()),
		jobengine.Config{Tick: 30 * time.Second, BackoffCap: 2 * time.Minute, SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}})
	eng.Tick(ctx) // the module takes the rows and the job runs
	if e.jobStatus(j.ID) != jobs.StatusRunning || fake.HeldCount(j.ID) != 3 {
		t.Fatalf("the fixture: %s, the module holds %d", e.jobStatus(j.ID), fake.HeldCount(j.ID))
	}
	fake.SetDown(true)
	eng.Tick(ctx) // the module cannot be asked: the job backs off for half a minute
	if e.idle(j.ID) != 1 || e.waitTenths(j.ID) <= 0 {
		t.Fatalf("the fixture: the job is not backed off (idle %d)", e.idle(j.ID))
	}
	fake.SetDown(false)
	calls := fake.Calls().Total
	eng.Tick(ctx)
	if fake.Calls().Total != calls {
		t.Fatal("the control: the engine called the module for a job that is backed off")
	}
	// the user cancels: the job is due at once and the next tick asks the module to cancel it
	if cj, err := e.jobs.CancelBeforeStart(ctx, u, j.ID); err != nil || cj.Status != jobs.StatusCancelling {
		t.Fatalf("the user's cancel: %v, %v", cj.Status, err)
	}
	eng.Tick(ctx)
	if fake.Calls().Cancel < 1 {
		t.Fatalf("the tick after the user's cancel did not ask the module to cancel (module calls %+v)", fake.Calls())
	}
	e.tickUntil(eng, j.ID, jobs.StatusCancelled, 5)
	e.requireEnded(j.ID, jobs.StatusCancelled, 1)
}

// ---- D9: the rows of a send job, written once ----------------------------------------------------------------------------------------------------------------------------------------------

func TestJobEngineItems_EnsureJobItemsWritesEveryPendingRowOnceInPagesAndRecordsIt(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	j := e.confirmRows(u, "m2", append(jeAddrs(3, "w"), jobs.RowIn{Email: "no at sign"})) // three pending rows and a malformed one
	if w, ok := e.workOf(j.ID); !ok || w.ItemsEnsuredAt != nil {
		t.Fatalf("the fixture: listed %v, items ensured %v", ok, w.ItemsEnsuredAt)
	}
	// the control of the replay below: one row written by the older call that names its rows
	ids := e.rowIDs(j.ID, "pending")
	if n, err := e.jobs.EnsureModuleItems(ctx, j.ID, ids[:1]); err != nil || n != 1 {
		t.Fatalf("EnsureModuleItems of one row: %d, %v", n, err)
	}
	n, err := e.jobs.EnsureJobItems(ctx, j.ID)
	if err != nil || n != 2 {
		t.Fatalf("EnsureJobItems wrote %d rows (%v), want the 2 that were missing", n, err)
	}
	if got := e.items(j.ID); got != "pending=3" {
		t.Fatalf("the job's items are %q, want one pending item per pending row and none for the malformed one", got)
	}
	w, ok := e.workOf(j.ID)
	if !ok || w.ItemsEnsuredAt == nil {
		t.Fatalf("the job does not say its items were written: %+v", w)
	}
	// the replay: nothing is written, nothing is read twice, the mark stays the same
	before, mark := e.fingerprint(), e.str("SELECT items_ensured_at::text FROM platform.jobs WHERE id = $1::uuid", j.ID)
	if n, err := e.jobs.EnsureJobItems(ctx, j.ID); err != nil || n != 0 || e.fingerprint() != before || e.str("SELECT items_ensured_at::text FROM platform.jobs WHERE id = $1::uuid", j.ID) != mark {
		t.Fatalf("a replay of EnsureJobItems: %d rows, %v (the job changed: %v)", n, err, e.fingerprint() != before)
	}
	// the message is the campaign's, written the way the older call writes it
	if got := e.rows(`SELECT DISTINCT rendered_subject || '|' || rendered_body || '|' || COALESCE(rendered_text, '-') || '|' || COALESCE(reply_to, '-') || '|' || user_id::text FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); len(got) != 1 ||
		got[0] != "Hello from the engine tests|<p>hello</p>|-|reply@example.test|"+u {
		t.Errorf("the items' message = %v", got)
	}

	// module 1 has no campaign, an ended job has nothing to submit, an unknown job is not found
	m1 := e.confirmN(e.newUser(), "m1", 2)
	_, err = e.jobs.EnsureJobItems(ctx, m1.ID)
	jeNotAllowed(t, "EnsureJobItems of a module 1 job", err, "not_a_send_job")
	cu := e.newUser()
	c := e.confirmN(cu, "m2", 1)
	if _, err := e.jobs.CancelBeforeStart(ctx, cu, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.jobs.EnsureJobItems(ctx, c.ID)
	jeNotAllowed(t, "EnsureJobItems of a cancelled job", err, "not_submittable")
	if _, err := e.jobs.EnsureJobItems(ctx, "00000000-0000-4000-8000-0000000000ff"); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("EnsureJobItems of an unknown job: %v", err)
	}

	// the page boundary: 5001 pending rows are two pages (the store writes five thousand to a statement), every one of them once
	big := e.confirmRows(e.newUser(), "m2", jeAddrs(5001, "z"), jeConfirmOpts{Fund: 6000})
	if n, err := e.jobs.EnsureJobItems(ctx, big.ID); err != nil || n != 5001 {
		t.Fatalf("EnsureJobItems of 5001 rows wrote %d (%v)", n, err)
	}
	if got := e.int("SELECT count(DISTINCT id) FROM public.job_items WHERE campaign_id = $1::uuid", big.ID); got != 5001 {
		t.Fatalf("%d distinct items for 5001 rows", got)
	}
	if n, err := e.jobs.EnsureJobItems(ctx, big.ID); err != nil || n != 0 {
		t.Fatalf("a replay of the 5001-row job wrote %d (%v)", n, err)
	}
}

// The engine asks once per job. A probe counts the INSERT statements the platform runs on public.job_items (a statement-level trigger of the scratch database only: it fires for a statement that inserts
// nothing too, which is what every tick of the older engine ran); the module is down for five ticks, the job's rows are written once, an engine built afterwards on the same database (a restart) writes
// nothing, and the control (the older call, five times) is seen by the probe five times.
func TestJobEngineItems_TheEngineWritesTheRowsOfAJobOnceWhateverTheTicksAndTheRestarts(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	e.exec(`CREATE SCHEMA zz_probe`)
	e.exec(`CREATE TABLE zz_probe.inserts (n integer NOT NULL)`)
	e.exec(`INSERT INTO zz_probe.inserts VALUES (0)`)
	e.exec(`CREATE FUNCTION zz_probe.count_insert() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN UPDATE zz_probe.inserts SET n = n + 1; RETURN NULL; END $$`)
	e.exec(`CREATE TRIGGER zz_probe_count BEFORE INSERT ON public.job_items FOR EACH STATEMENT EXECUTE FUNCTION zz_probe.count_insert()`)
	inserts := func() int64 { return e.int("SELECT n FROM zz_probe.inserts") }

	j := e.confirmN(e.newUser(), "m2", 4)
	fake := &jobenginetest.FakeModule{}
	fake.SetDown(true)
	cfg := jobengine.Config{SettleEvery: -1} // the backoff is off: every tick looks at the job
	eng := e.engineOf(jeSingleModule{fake}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()), cfg)
	for i := 0; i < 5; i++ {
		eng.Tick(ctx)
	}
	if fake.Calls().Submit < 5 {
		t.Fatalf("the engine called the module %d times in five ticks: the test would prove nothing", fake.Calls().Submit)
	}
	if got := inserts(); got != 1 {
		t.Fatalf("five ticks of a module that is down ran %d INSERT statements on public.job_items, want 1 (one per job, not one per tick)", got)
	}
	if got := e.items(j.ID); got != "pending=4" {
		t.Fatalf("the job's items are %q", got)
	}
	// a restarted engine, and the module back: the job runs and nothing more is written
	restarted := e.engineOf(jeSingleModule{fake}, jobenginetest.NewGate(true), jobenginetest.NewClock(time.Now()), cfg)
	fake.SetDown(false)
	e.tickUntil(restarted, j.ID, jobs.StatusSucceeded, 10)
	if got := inserts(); got != 1 {
		t.Fatalf("after a restart and the whole run the probe saw %d INSERT statements, want still 1", got)
	}
	// the control: the probe sees what the older engine ran at every tick (a statement that inserts nothing is still a statement)
	k := e.confirmN(e.newUser(), "m2", 2)
	before := inserts()
	kids := e.rowIDs(k.ID, "pending")
	for i := 0; i < 5; i++ {
		if _, err := e.jobs.EnsureModuleItems(ctx, k.ID, kids); err != nil {
			t.Fatal(err)
		}
	}
	if got := inserts() - before; got != 5 {
		t.Fatalf("the control: the probe saw %d statements for five calls of the older method, want 5 (it must see each of them)", got)
	}
}

// ---- D9: the terminal marker --------------------------------------------------------------------------------------------------------------------------------------------------------------------

// A job that ends without sending gives its never-submitted items 'cancelled', in the transaction that ends it: by the engine's expiry, by the user's cancel of a job that has not started, by the engine's
// failure of a job and by the end of a cancelled job; only the items of THIS job and only those of rows no module was given (a row that carries a stamp, or a result of the module, is the module's); never a
// delete; a replay changes nothing. The controls: before the end every item is pending, the item of a live job and of another user's job, a row of module 3's test-send and a module 1 job are untouched.
func TestJobEngineItems_EveryEndOfAJobBeforeItsSubmissionMarksItsNeverSubmittedItemsAndNothingElse(t *testing.T) {
	e := newJeEnv(t, jobs.WithMaxStartDelay(2*time.Second))
	ctx := testCtx(t)
	ensure := func(job string) {
		t.Helper()
		if _, err := e.jobs.EnsureJobItems(ctx, job); err != nil {
			t.Fatalf("EnsureJobItems(%s): %v", job, err)
		}
	}

	// the arrangement, every job with its items written and every item pending
	ux := e.newUser()
	x := e.confirmN(ux, "m2", 4) // will expire
	ensure(x.ID)
	uc := e.newUser()
	c := e.confirmN(uc, "m2", 3) // the user cancels it before it starts
	ensure(c.ID)
	f := e.running(e.newUser(), "m2", 4) // the engine fails it: two rows were handed over, one has a result of the module, one was never given
	ensure(f.ID)
	fIDs := e.rowIDs(f.ID, "pending")
	if err := e.jobs.MarkSubmitted(ctx, f.ID, fIDs[:2]); err != nil {
		t.Fatal(err)
	}
	e.record(f.ID, jeItem(fIDs[2], jeM2Sent))
	ucn := e.newUser()
	cn := e.running(ucn, "m2", 4) // cancelled while running: two rows were handed over, two never were
	ensure(cn.ID)
	cnIDs := e.rowIDs(cn.ID, "pending")
	if err := e.jobs.MarkSubmitted(ctx, cn.ID, cnIDs[:2]); err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE public.job_items SET status = 'sent' WHERE id = $1::uuid", cnIDs[3]) // a status that is neither: the marker moves pending to cancelled and nothing else
	live := e.running(e.newUser(), "m2", 2)
	ensure(live.ID)
	m1 := e.confirmN(e.newUser(), "m1", 2) // module 1 has no items at all
	// a row module 3's test-send would write for the user of the cancelled job (its campaign is the user's own id)
	e.exec(`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1::uuid, $1::uuid, 'test-send@example.test', 's', 'b', 'pending')`, uc)
	for what, got := range map[string]string{x.ID: e.items(x.ID), c.ID: e.items(c.ID), f.ID: e.items(f.ID), live.ID: e.items(live.ID)} {
		if !strings.HasPrefix(got, "pending=") {
			t.Fatalf("the fixture: the items of job %s are %q before it ends, want all pending", what, got)
		}
	}
	if e.items(m1.ID) != "" {
		t.Fatalf("the fixture: a module 1 job has items %q", e.items(m1.ID))
	}

	// ---- the ends ----
	if _, err := e.jobs.CancelBeforeStart(ctx, uc, c.ID); err != nil { // confirmed -> cancelled
		t.Fatal(err)
	}
	if _, err := e.jobs.FailJob(ctx, f.ID, "the module refused the job: test"); err != nil {
		t.Fatal(err)
	}
	if cj, err := e.jobs.CancelBeforeStart(ctx, ucn, cn.ID); err != nil || cj.Status != jobs.StatusCancelling { // running -> cancelling, then the engine's cancelling end
		t.Fatalf("the cancel of a running job: %v, %v", cj.Status, err)
	}
	if n, err := e.jobs.CancelUnsubmitted(ctx, cn.ID); err != nil || n != 2 {
		t.Fatalf("CancelUnsubmitted: %d, %v", n, err)
	}
	if _, err := e.jobs.FailJob(ctx, m1.ID, "the module refused the job: test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // the start deadline of x, by the database's clock
	if ex, err := e.jobs.Expire(ctx, x.ID); err != nil || ex.Status != jobs.StatusExpired {
		t.Fatalf("Expire: %v, %v", ex.Status, err)
	}

	// ---- what each end left ----
	for _, tc := range []struct{ name, job, want string }{
		{"an expired job: every item (no row was given to the module)", x.ID, "cancelled=4"},
		{"a job the user cancelled before it started", c.ID, "cancelled=3"},
		{"a failed job: the two rows handed over and the row that has the module's result keep theirs, the row never given is marked", f.ID, "cancelled=1,pending=3"},
		{"a cancelled running job: the two handed over keep theirs, the one never given is marked, a status that was not pending is left alone", cn.ID, "cancelled=1,pending=2,sent=1"},
		{"the control: a job that is running and goes on", live.ID, "pending=2"},
		{"a module 1 job has no items to mark", m1.ID, ""},
	} {
		if got := e.items(tc.job); got != tc.want {
			t.Errorf("%s: the items are %q, want %q", tc.name, got, tc.want)
		}
	}
	// the marked ones are exactly the rows that were never given: each marked item's row has no stamp and no result of the module
	if n := e.int(`SELECT count(*) FROM public.job_items i JOIN platform.job_rows w ON w.id = i.id
		WHERE i.status = 'cancelled' AND (w.submitted_at IS NOT NULL OR EXISTS (SELECT 1 FROM platform.job_results r WHERE r.job_id = w.job_id AND r.item_ref = w.id AND r.origin = 'module'))`); n != 0 {
		t.Errorf("%d marked item(s) belong to a row the module was given or answered for", n)
	}
	// the test-send row of the user, and every other job's items: untouched
	if got := e.str("SELECT status FROM public.job_items WHERE recipient_email = 'test-send@example.test'"); got != "pending" {
		t.Errorf("module 3's test-send row of the user is %q", got)
	}
	// nothing was deleted, and every job still has the items it was given
	if got := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = ANY($1::text[]::uuid[])", []string{x.ID, c.ID, f.ID, cn.ID, live.ID}); got != 4+3+4+4+2 {
		t.Errorf("public.job_items holds %d rows of the five jobs, want 17: the marker deletes nothing", got)
	}

	// a replay changes nothing: the repair pass over every ended job, and the engine's own ends again
	snapshot := func() string {
		return strings.Join(e.rows(`SELECT campaign_id::text || '|' || id::text || '|' || status FROM public.job_items ORDER BY 1`), ";")
	}
	before := snapshot()
	for _, id := range []string{x.ID, c.ID, f.ID, m1.ID} { // the jobs that have ended (the cancelling job has not: its own end is replayed below)
		if err := e.jobs.Release(ctx, id); err != nil {
			t.Fatalf("Release(%s): %v", id, err)
		}
	}
	if _, err := e.jobs.Expire(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.FailJob(ctx, f.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if n, err := e.jobs.CancelUnsubmitted(ctx, cn.ID); err != nil || n != 0 { // the cancelling end of a job, replayed
		t.Fatalf("a replay of CancelUnsubmitted: %d, %v, want 0 and no error", n, err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("a replay changed public.job_items")
	}

	// a job that an administrator ended by hand is marked by the repair pass (the same rule, for the one writer that did not end it)
	ua := e.newUser()
	a := e.confirmN(ua, "m2", 2)
	ensure(a.ID)
	jobWalkTo(t, e.admin, a.ID, jobs.StatusFailed)
	if got := e.items(a.ID); got != "pending=2" {
		t.Fatalf("the items of a job ended by hand are %q before the repair", got)
	}
	if err := e.jobs.Release(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.items(a.ID); got != "cancelled=2" {
		t.Errorf("the repair pass of a job ended by hand left the items %q, want cancelled=2", got)
	}
	e.requireBooks("after every end of a job before its submission")
}

// ---- D9: what the modules can do with such an item ----------------------------------------------------------------------------------------------------------------------------------------

// Since the Phase 4 close-out (D9) the marker IS a lock in the modules: the run route of module 2 and module 3 (statements J1, the read, and J2, the INSERT ... SELECT that freezes the item into the module's own job)
// refuses an item whose status is 'cancelled' (409 job_item_cancelled), in the SAME statement that reads or freezes it. db/module-statements.txt holds the exact texts the modules issue (the drift gate
// check-module-reads proves them against the pinned module code), so this test reads them: every statement of module 2 or 3 that touches public.job_items is one of the five known (J1 and J2 of each module, and
// module 3's test-send INSERT J4, which writes 'pending'), J1 reads the status and J2 excludes 'cancelled' in its WHERE, and a new statement (a scanner, a requeue) must be read for what it does with a
// marked item before this list is changed.
func TestJobItemsMarker_EveryModuleStatementThatTurnsAnItemIntoWorkRefusesAMarkedItem(t *testing.T) {
	root := requireEnv(t, envRepoRoot)
	raw, err := os.ReadFile(filepath.Join(root, "db", "module-statements.txt"))
	if err != nil {
		t.Fatalf("read db/module-statements.txt: %v", err)
	}
	type stmt struct{ module, role, label, file, sql string }
	var all []stmt
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "module-dir|") { // the two header lines name each module's directory; every other line is a statement
			continue
		}
		p := strings.SplitN(line, "|", 5)
		if len(p) != 5 {
			t.Fatalf("db/module-statements.txt: a line with %d fields: %.80s", len(p), line)
		}
		all = append(all, stmt{p[0], p[1], p[2], p[3], p[4]})
	}
	refusesMarked := func(label, sql string) bool {
		l := strings.ToLower(sql)
		switch label {
		case "J1": // the read: the module learns in the same statement whether the item is marked
			return strings.Contains(l, "status is not distinct from 'cancelled'")
		case "J2": // the freeze: the exclusion is in the WHERE of the statement that creates the module's job
			return strings.Contains(l, "ji.status is distinct from 'cancelled'")
		}
		return false
	}
	var touching []string
	for _, s := range all {
		if s.role == "ref" || !strings.Contains(strings.ToLower(s.sql), "public.job_items") {
			continue
		}
		touching = append(touching, s.module+":"+s.label)
		switch s.label {
		case "J1", "J2":
			if !refusesMarked(s.label, s.sql) {
				t.Errorf("%s:%s (%s) does not refuse an item marked cancelled: a module can act on a never-submitted item again. Statement: %.200s", s.module, s.label, s.file, s.sql)
			}
		case "J4": // module 3's test-send: it WRITES a new item with status 'pending' and reads nothing
			if !strings.HasPrefix(strings.ToLower(s.sql), "insert into public.job_items") || !strings.Contains(strings.ToLower(s.sql), "status") {
				t.Errorf("%s:%s is not the test-send INSERT of status 'pending' this test expects: %.100s", s.module, s.label, s.sql)
			}
		default:
			t.Errorf("%s:%s (%s) touches public.job_items and this test does not know it: read what it does with an item that was never submitted, then add it here: %.160s", s.module, s.label, s.file, s.sql)
		}
	}
	sort.Strings(touching)
	if want := []string{"m2:J1", "m2:J2", "m3:J1", "m3:J2", "m3:J4"}; strings.Join(touching, ",") != strings.Join(want, ",") {
		t.Errorf("the module statements that touch public.job_items are %v, want exactly %v", touching, want)
	}
	// the control: the check can fail: the OLD statements (before the guard) are read as NOT refusing a marked item
	if refusesMarked("J1", "SELECT campaign_id FROM public.job_items WHERE id = $1 AND user_id = $2") ||
		refusesMarked("J2", "INSERT INTO x SELECT ji.id FROM public.job_items ji WHERE ji.id = $1 AND ji.user_id = $2 RETURNING id") ||
		!refusesMarked("J1", "SELECT campaign_id, status IS NOT DISTINCT FROM 'cancelled' FROM public.job_items WHERE id = $1 AND user_id = $2") {
		t.Error("the control: the predicate does not tell a statement that refuses a marked item from one that does not")
	}
	if len(all) < 8 {
		t.Fatalf("db/module-statements.txt has %d statements: the instrument matched too little", len(all))
	}
	t.Logf("NOTE job items marker: %d module statements read, %d touch public.job_items (%s); the run statements of both senders refuse a cancelled item", len(all), len(touching), strings.Join(touching, " "))
}

// ---- the lifecycle of the migration -------------------------------------------------------------------------------------------------------------------------------------------------------------

// engineScheduleObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0015's down (TestRunnerLifecycle): before it, the three annotation columns of platform.jobs, the CHECK of the counter, the
// function and the trigger that wake a job on every move and keep its walls, and the comment that names the terminal marker of public.job_items.status are there; after it they are gone and the status column
// carries platform_0001's comment again. The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func engineScheduleObjects(t *testing.T, c ledgerDB, present bool, when string) {
	t.Helper()
	want := int64(0)
	if present {
		want = 1
	}
	expect := func(what string, perItem int64, sql string) {
		t.Helper()
		if got := ledgerInt64(t, c, sql); got != want*perItem {
			t.Fatalf("%s: %s: found %d, want %d", when, what, got, want*perItem)
		}
	}
	expect("the columns next_poll_at, idle_polls and items_ensured_at of platform.jobs", 3,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.jobs'::regclass AND NOT attisdropped AND attname::text IN ('next_poll_at', 'idle_polls', 'items_ensured_at')")
	expect("the CHECK jobs_idle_polls_not_negative", 1, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.jobs'::regclass AND contype = 'c' AND conname::text = 'jobs_idle_polls_not_negative'")
	expect("the function platform.jobs_engine_schedule()", 1, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_engine_schedule'")
	expect("the trigger jobs_engine_schedule on platform.jobs", 1, "SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'platform.jobs'::regclass AND tgname::text = 'jobs_engine_schedule'")
	expect("the comment of public.job_items.status naming the terminal marker", 1,
		`SELECT count(*) FROM pg_attribute a WHERE a.attrelid = 'public.job_items'::regclass AND a.attname::text = 'status' AND COALESCE(col_description(a.attrelid, a.attnum), '') LIKE '%terminal marker%'`)
	// the comment that the platform_0001 down restores is there exactly when the migration is not
	if got := ledgerInt64(t, c, `SELECT count(*) FROM pg_attribute a WHERE a.attrelid = 'public.job_items'::regclass AND a.attname::text = 'status' AND COALESCE(col_description(a.attrelid, a.attnum), '') LIKE '%never read by a module%'`); got != 1-want {
		t.Fatalf("%s: the older comment of public.job_items.status is present %d times, want %d", when, got, 1-want)
	}
}
