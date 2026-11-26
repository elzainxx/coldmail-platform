package jobengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// The schedule of the engine, proved without a database (Phase 4 close-out D8 and D9): what the engine tells the store about every look at a job (progress, or a step that made none and the backoff it
// asks for), when it tells nothing, the rows of a whole job written once, and what the engine keeps in memory about a job that is only waiting out a backoff. The statements those calls run, and the
// fairness of the work order against a real Postgres, are tests/harness/jobs_engine_schedule_test.go and jobs_engine_test.go.

func TestConfig_TheBackoffCapDefaultsToAMinuteIsRefusedPastTheDatabasesWallAndNeverExceedsAQuarterOfTheStalenessLimit(t *testing.T) {
	c, err := Config{}.withDefaults()
	if err != nil || c.BackoffCap != DefaultBackoffCap || DefaultBackoffCap != time.Minute || MaxBackoffCap != 10*time.Minute {
		t.Fatalf("the default cap: %+v, %v (a minute by default, ten minutes at most)", c, err)
	}
	if want := jobs.MaxPollBackoff; MaxBackoffCap != want {
		t.Fatalf("the engine's highest cap is %s, the store's wall is %s: one number", MaxBackoffCap, want)
	}
	if _, err := (Config{BackoffCap: MaxBackoffCap}).withDefaults(); err != nil {
		t.Errorf("the highest cap was refused: %v", err)
	}
	if _, err := (Config{BackoffCap: MaxBackoffCap + time.Second}).withDefaults(); err == nil || !strings.Contains(err.Error(), "BackoffCap") {
		t.Errorf("a cap past the wall of the database was accepted or not named: %v", err)
	}
	// the wait starts at one tick, ends at the cap, and is never more than a quarter of the module's staleness limit
	for _, tc := range []struct {
		name         string
		cfg          Config
		module       string
		base, ceilng time.Duration
	}{
		{"the defaults for module 2 (a quarter of 15 minutes is more than the cap)", Config{}, "m2", DefaultTick, time.Minute},
		{"a short staleness limit lowers the ceiling to a quarter of it", Config{StaleAfter: map[string]time.Duration{"m1": time.Minute}}, "m1", DefaultTick, 15 * time.Second},
		{"the same configuration for a module with a long limit", Config{StaleAfter: map[string]time.Duration{"m1": time.Minute}}, "m2", DefaultTick, time.Minute},
		{"a tick longer than the ceiling: the first wait is the ceiling", Config{Tick: 30 * time.Second, BackoffCap: 10 * time.Second}, "m2", 10 * time.Second, 10 * time.Second},
		{"a dial of ten minutes with a limit of three hours", Config{BackoffCap: MaxBackoffCap, StaleAfter: map[string]time.Duration{"m3": 3 * time.Hour}}, "m3", DefaultTick, 10 * time.Minute},
		{"a dial of ten minutes with a limit of twenty minutes is cut to a quarter of it", Config{BackoffCap: MaxBackoffCap, StaleAfter: map[string]time.Duration{"m3": 20 * time.Minute}}, "m3", DefaultTick, 5 * time.Minute},
		{"switched off", Config{BackoffCap: -1}, "m2", 0, 0},
	} {
		cfg, err := tc.cfg.withDefaults()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if base, ceiling := cfg.backoff(tc.module); base != tc.base || ceiling != tc.ceilng {
			t.Errorf("%s: backoff = (%s, %s), want (%s, %s)", tc.name, base, ceiling, tc.base, tc.ceilng)
		}
	}
}

// pollsOf is the notes the store got, failing the test when it got another number than want.
func pollsOf(t *testing.T, r *rig, want int) []pollNote {
	t.Helper()
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	if len(r.st.polls) != want {
		t.Fatalf("the store was told %d outcomes %+v, want %d", len(r.st.polls), r.st.polls, want)
	}
	return append([]pollNote(nil), r.st.polls...)
}

func TestNotePoll_ProgressIsTold_AStepThatMadeNoneIsToldWithTheBackoff(t *testing.T) {
	cfg := Config{Tick: time.Second, SettleEvery: -1, BackoffCap: 30 * time.Second, StaleAfter: map[string]time.Duration{"m2": time.Hour}}

	// progress: the module takes the rows and the job starts
	r := newRig(t, cfg, ej(jobA, "m2", jobs.StatusConfirmed))
	r.st.pending = rows(2)
	r.e.Tick(context.Background())
	if p := pollsOf(t, r, 1)[0]; p.id != jobA || p.status != jobs.StatusConfirmed || !p.progressed || p.base != time.Second || p.ceiling != 30*time.Second {
		t.Errorf("a job that started was told as %+v, want progress, in the status the step read (confirmed)", p)
	}

	// no progress: a module that cannot answer, and a module that takes none of the rows
	for name, mod := range map[string]func(*stubMod){
		"cannot answer": func(m *stubMod) {
			m.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
				return SubmitResult{}, Wait("unavailable")
			}
		},
		"accepts none": func(m *stubMod) { m.accept = 0 },
	} {
		w := newRig(t, cfg, ej(jobA, "m2", jobs.StatusConfirmed))
		w.st.pending = rows(2)
		mod(w.mod)
		w.e.Tick(context.Background())
		if p := pollsOf(t, w, 1)[0]; p.progressed || p.status != jobs.StatusConfirmed || p.base != time.Second || p.ceiling != 30*time.Second {
			t.Errorf("a module that %s: told as %+v, want NO progress with the engine's backoff (one tick up to the cap)", name, p)
		}
	}

	// a running job: results recorded are progress; an observation with nothing new is not
	rec := newRig(t, cfg, ej(jobA, "m2", jobs.StatusRunning))
	rec.mod.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Total: 1, Terminal: 1}, fn([]jobs.ResultItem{{ItemRef: rows(1)[0].ID}})
	}
	rec.e.Tick(context.Background())
	if p := pollsOf(t, rec, 1)[0]; !p.progressed || p.status != jobs.StatusRunning {
		t.Errorf("a job that recorded a result was told as %+v", p)
	}
	idle := newRig(t, cfg, ej(jobA, "m2", jobs.StatusRunning))
	idle.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Total: 2}, nil
	}
	idle.e.Tick(context.Background())
	if p := pollsOf(t, idle, 1)[0]; p.progressed {
		t.Errorf("a job whose observation showed nothing new was told as progress: %+v", p)
	}

	// the end of a job is progress
	j := ej(jobA, "m2", jobs.StatusConfirmed)
	j.DeadlinePassed = true
	ex := newRig(t, cfg, j)
	ex.e.Tick(context.Background())
	if p := pollsOf(t, ex, 1)[0]; !p.progressed {
		t.Errorf("an expiry was told as %+v", p)
	}

	// an error that is not a wait is no progress either: the job is looked at again after a backoff, like a wait
	f := newRig(t, cfg, ej(jobA, "m2", jobs.StatusRunning))
	f.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{}, errors.New("the module answered garbage")
	}
	f.e.Tick(context.Background())
	if p := pollsOf(t, f, 1)[0]; p.progressed {
		t.Errorf("a step that failed was told as progress: %+v", p)
	}
}

func TestNotePoll_ARefusedStepAnEndedJobAnEndingPassAndAPanicAreToldRight(t *testing.T) {
	cfg := Config{Tick: time.Second, SettleEvery: -1, BackoffCap: 30 * time.Second}

	// the lifecycle refused the step, or a race overtook it: somebody moved the job and the database woke it. The engine tells nothing, so the wake-up is not overwritten by a wait.
	for name, err := range map[string]error{"not allowed": fmt.Errorf("x: %w", jobs.ErrNotAllowed), "not found": jobs.ErrNotFound} {
		r := newRig(t, cfg, ej(jobA, "m2", jobs.StatusConfirmed))
		r.st.pending = rows(1)
		r.mod.ref = "handle-1"
		r.mod.accept = 0 // the module takes nothing: no progress to report, only the refusal
		r.st.errs["EnsureJobItems"] = err
		r.e.Tick(context.Background())
		if r.st.log.count("EnsureJobItems") != 1 {
			t.Fatalf("%s: the step never reached the store's refusal: %v", name, r.st.log.list())
		}
		pollsOf(t, r, 0)
	}

	// an ended job (the repair of its reserve) is due whatever its backoff: nothing is told
	rep := newRig(t, cfg, ej(jobA, "m2", jobs.StatusFailed))
	rep.e.Tick(context.Background())
	if rep.st.log.count("Release") != 1 {
		t.Fatalf("the repair did not run: %v", rep.st.log.list())
	}
	pollsOf(t, rep, 0)

	// the pass itself is ending: nothing is told
	c := newRig(t, cfg, ej(jobA, "m2", jobs.StatusRunning))
	ctx, cancel := context.WithCancel(context.Background())
	c.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		cancel()
		return Observation{}, nil
	}
	c.e.Tick(ctx)
	pollsOf(t, c, 0)

	// a step that panics is told as a step that made no progress: a job that panics at every look is not looked at at every tick
	p := newRig(t, cfg, ej(jobA, "m2", jobs.StatusConfirmed))
	p.st.pending = rows(1)
	p.st.panicOn = "PendingRows"
	p.e.Tick(context.Background())
	if p.e.Stats().Panics != 1 {
		t.Fatalf("the step did not panic: %+v", p.e.Stats())
	}
	if n := pollsOf(t, p, 1)[0]; n.progressed {
		t.Errorf("a panic was told as progress: %+v", n)
	}

	// the switch: with the backoff off the idle job is told with a base and a ceiling of zero (due again at the next tick)
	off := newRig(t, Config{SettleEvery: -1, BackoffCap: -1}, ej(jobA, "m2", jobs.StatusRunning))
	off.e.Tick(context.Background())
	if n := pollsOf(t, off, 1)[0]; n.progressed || n.base != 0 || n.ceiling != 0 {
		t.Errorf("a job with the backoff switched off was told as %+v, want no progress with (0, 0)", n)
	}

	// a failed note costs nothing: it is logged, and the tick goes on to the next job
	bad := newRig(t, cfg, ej(jobA, "m2", jobs.StatusRunning), ej(jobB, "m2", jobs.StatusRunning))
	bad.st.errs["NotePoll"] = errors.New("the database refused")
	bad.e.Tick(context.Background())
	if bad.e.Stats().Steps != 2 || bad.e.Stats().Unexpected != 0 {
		t.Errorf("a note that failed stopped the pass or was counted as a failed step: %+v", bad.e.Stats())
	}
}

func TestSubmit_TheItemsOfAWholeJobAreWrittenOncePerJobNotPerBatchPerTickOrPerRestart(t *testing.T) {
	// one step with three batches: one call, however many batches follow
	r := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.st.pending = rows(5)
	r.e.Tick(context.Background())
	if len(r.st.pending) != 0 || r.st.log.count("Submit") != 3 {
		t.Fatalf("the fixture: %d rows left, %d submits (want 0 and 3)", len(r.st.pending), r.st.log.count("Submit"))
	}
	if n := r.st.log.count("EnsureJobItems"); n != 1 {
		t.Errorf("the rows of the job were ensured %d times in one step of three batches, want once: %v", n, r.st.log.list())
	}

	// a job that has them (items_ensured_at, read with the work: a later tick, another instance, a restarted engine) asks for nothing
	done := ej(jobA, "m2", jobs.StatusRunning)
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	done.ItemsEnsuredAt = &at
	d := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, done)
	d.st.pending = rows(5)
	d.e.Tick(context.Background())
	if d.st.log.count("EnsureJobItems") != 0 || d.st.log.count("Submit") != 3 {
		t.Errorf("a job whose rows were written: %v", d.st.log.list())
	}

	// a module that is down for three ticks: the job whose rows were written costs the store no write at all (the 500-row INSERT of every tick of the old engine), the control asks at each tick
	// because the fake store keeps no state and hands out the job without the mark, as a real store hands out a job whose ensure has not committed
	down := func(context.Context, []jobs.EngineRow) (SubmitResult, error) { return SubmitResult{}, Wait("down") }
	w := newRig(t, Config{BackoffCap: -1}, ej(jobA, "m2", jobs.StatusConfirmed))
	w.st.pending = rows(3)
	w.mod.submit = down
	for i := 0; i < 3; i++ {
		w.e.Tick(context.Background())
	}
	if w.st.log.count("Submit") != 3 || w.st.log.count("EnsureJobItems") != 3 {
		t.Errorf("the control (a job with no mark): %v", w.st.log.list())
	}
	mk := ej(jobA, "m2", jobs.StatusConfirmed)
	mk.ItemsEnsuredAt = &at
	k := newRig(t, Config{BackoffCap: -1}, mk)
	k.st.pending = rows(3)
	k.mod.submit = down
	for i := 0; i < 3; i++ {
		k.e.Tick(context.Background())
	}
	if k.st.log.count("Submit") != 3 || k.st.log.count("EnsureJobItems") != 0 {
		t.Errorf("three ticks of a down module for a job whose rows are written: %v", k.st.log.list())
	}

	// module 1 has no public.job_items rows at all, and a failed ensure leaves the job exactly where it was: nothing is submitted, started or stamped, and the next tick asks again
	m1 := newRig(t, Config{}, ej(jobA, "m1", jobs.StatusConfirmed))
	m1.st.pending = rows(2)
	m1.e.Tick(context.Background())
	if m1.st.log.count("EnsureJobItems") != 0 {
		t.Errorf("module 1 was asked to write items: %v", m1.st.log.list())
	}
	f := newRig(t, Config{BackoffCap: -1}, ej(jobA, "m2", jobs.StatusConfirmed))
	f.st.pending = rows(2)
	f.st.errs["EnsureJobItems"] = errors.New("the database refused")
	f.e.Tick(context.Background())
	if f.st.log.count("Submit") != 0 || f.st.log.count("Start") != 0 || f.st.log.count("MarkSubmitted") != 0 || f.e.Stats().Unexpected != 1 {
		t.Errorf("a failed ensure: %v / %+v", f.st.log.list(), f.e.Stats())
	}
	delete(f.st.errs, "EnsureJobItems")
	f.e.Tick(context.Background())
	if f.st.log.count("EnsureJobItems") != 2 || f.st.log.count("Start") != 1 {
		t.Errorf("after the failure the next tick did not ask again and start: %v", f.st.log.list())
	}
}

func TestPrune_AFlagIsRememberedWhileAJobWaitsOutItsBackoffAndForgottenOnceNoLimitCouldHoldIt(t *testing.T) {
	r := newRig(t, Config{StaleAfter: map[string]time.Duration{"m2": time.Hour, "m3": 3 * time.Hour}})
	if got := r.e.cfg.longestStale(); got != 3*time.Hour {
		t.Fatalf("longestStale = %s, want 3h", got)
	}
	now := *r.now
	r.e.mu.Lock()
	r.e.flagged[jobA], r.e.refusals[jobA] = now.Add(-2*time.Hour), now.Add(-2*time.Hour) // inside the longest limit: it can still hold a flag back
	r.e.flagged[jobB], r.e.refusals[jobB] = now.Add(-4*time.Hour), now.Add(-4*time.Hour) // older than every limit: it can hold nothing back
	r.e.lastObs[jobA] = observed{at: now}
	r.e.mu.Unlock()
	r.e.prune(nil) // neither job is in this tick's work: both are waiting out a backoff, or finished
	r.e.mu.Lock()
	if _, ok := r.e.flagged[jobA]; !ok {
		t.Error("the flag of a job that is only waiting was forgotten: it would be raised again at every look instead of once per staleness interval")
	}
	if _, ok := r.e.refusals[jobA]; !ok {
		t.Error("the refusal flag of a job that is only waiting was forgotten")
	}
	if _, ok := r.e.flagged[jobB]; ok {
		t.Error("a flag older than every staleness limit was kept for ever")
	}
	if _, ok := r.e.refusals[jobB]; ok {
		t.Error("a refusal older than every staleness limit was kept for ever")
	}
	if _, ok := r.e.lastObs[jobA]; ok {
		t.Error("the last observation of a job that is not in the work was kept (a missing one only makes the next look observe)")
	}
	r.e.mu.Unlock()

	// the control: a job in the work keeps everything, however old
	live := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusRunning))
	live.e.mu.Lock()
	live.e.flagged[jobA] = now.Add(-100 * time.Hour)
	live.e.mu.Unlock()
	live.e.prune([]jobs.EngineJob{ej(jobA, "m2", jobs.StatusRunning)})
	live.e.mu.Lock()
	defer live.e.mu.Unlock()
	if _, ok := live.e.flagged[jobA]; !ok {
		t.Error("a job in the work lost its flag")
	}
}
