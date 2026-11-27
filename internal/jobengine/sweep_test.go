package jobengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S7 and S8: the two housekeeping steps of the engine (sweep.go), against fakes of the two store methods they call, so that what is proved here is the ENGINE's part: when they run, in whose instance,
// with which dial, in what order after the jobs, and what a failure or a panic costs. What the store methods do to a database is proved by tests/harness/jobs_pause_limit_test.go and
// jobs_content_retention_test.go.

type fakeSweeps struct {
	mu        sync.Mutex
	log       *callLog
	pauseCall []time.Duration // the limit each call carried
	pauseMax  []int
	keepCall  []time.Duration
	keepJobs  []int
	keepPages []int
	deadline  []time.Duration // how much time each call was given
	pauseRes  jobs.PauseLimitResult
	keepRes   jobs.RetentionResult
	pauseErr  error
	keepErr   error
	panicOn   string // "pause" or "retention"
}

func (f *fakeSweeps) CancelOverLimitPaused(ctx context.Context, limit time.Duration, max int) (jobs.PauseLimitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log.add("CancelOverLimitPaused")
	f.pauseCall, f.pauseMax = append(f.pauseCall, limit), append(f.pauseMax, max)
	if d, ok := ctx.Deadline(); ok {
		f.deadline = append(f.deadline, time.Until(d))
	}
	if f.panicOn == "pause" {
		panic("the pause step blew up")
	}
	return f.pauseRes, f.pauseErr
}

func (f *fakeSweeps) ClearRetainedContent(ctx context.Context, after time.Duration, maxJobs, maxPages int) (jobs.RetentionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log.add("ClearRetainedContent")
	f.keepCall, f.keepJobs, f.keepPages = append(f.keepCall, after), append(f.keepJobs, maxJobs), append(f.keepPages, maxPages)
	if f.panicOn == "retention" {
		panic("the retention step blew up")
	}
	return f.keepRes, f.keepErr
}

func (f *fakeSweeps) calls() (pause, keep int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pauseCall), len(f.keepCall)
}

func sweepRig(t *testing.T, cfg Config, jobList ...jobs.EngineJob) (*rig, *fakeSweeps) {
	t.Helper()
	r := newRig(t, cfg, jobList...)
	f := &fakeSweeps{log: r.st.log}
	r.e.pl, r.e.ret = f, f
	return r, f
}

func TestSweepDials_DefaultsBoundsAndTheStoresOwnLimitsAreOneNumber(t *testing.T) {
	c, err := Config{}.withDefaults()
	if err != nil || c.PauseMax != 7*24*time.Hour || c.RetentionAfter != 7*24*time.Hour || c.SweepEvery != DefaultSweepEvery {
		t.Fatalf("the defaults are %s, %s, %s (%v), want 7 days, 7 days and %s", c.PauseMax, c.RetentionAfter, c.SweepEvery, err, DefaultSweepEvery)
	}
	if DefaultPauseMax != 7*24*time.Hour || DefaultRetentionAfter != 7*24*time.Hour || MaxPauseMax != 90*24*time.Hour || MaxRetentionAfter != 365*24*time.Hour {
		t.Errorf("a dial moved: the spec says pause 7 days (1 to 90) and retention 7 days (1 to 365): %s %s %s %s", DefaultPauseMax, DefaultRetentionAfter, MaxPauseMax, MaxRetentionAfter)
	}
	if MaxPauseMax != jobs.MaxPauseLimit || MaxRetentionAfter != jobs.MaxContentRetention {
		t.Errorf("the engine takes at most %s and %s, the store at most %s and %s: one number in two places", MaxPauseMax, MaxRetentionAfter, jobs.MaxPauseLimit, jobs.MaxContentRetention)
	}
	refused := map[string]Config{
		"a negative pause limit": {PauseMax: -time.Hour}, "a pause limit under a second": {PauseMax: 500 * time.Millisecond}, "a pause limit over 90 days": {PauseMax: MaxPauseMax + time.Second},
		"a negative retention": {RetentionAfter: -1}, "a retention under a second": {RetentionAfter: time.Millisecond}, "a retention over 365 days": {RetentionAfter: MaxRetentionAfter + time.Second},
	}
	for name, cfg := range refused {
		if _, err := newEngine(newFstore(), stubMods{}, gateFn(func() bool { return true }), nil, nil, cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, cfg := range map[string]Config{
		"the lowest limits": {PauseMax: jobs.MinPauseLimit, RetentionAfter: jobs.MinContentRetention}, "the highest limits": {PauseMax: MaxPauseMax, RetentionAfter: MaxRetentionAfter},
		"a sweep at every tick": {SweepEvery: -1}, "seconds, as the tests use": {PauseMax: 2 * time.Second, RetentionAfter: 3 * time.Second},
	} {
		if _, err := newEngine(newFstore(), stubMods{}, gateFn(func() bool { return true }), nil, nil, cfg); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

func TestTick_TheSweepsRunAfterTheJobsWithTheDialsOfTheConfigAndEachUnderTheStepTimeout(t *testing.T) {
	r, f := sweepRig(t, Config{PauseMax: 36 * time.Hour, RetentionAfter: 48 * time.Hour, StepTimeout: 90 * time.Second}, ej(jobA, "m2", jobs.StatusConfirmed))
	r.e.Tick(context.Background())
	log := r.st.log.list()
	if len(log) < 3 || log[len(log)-2] != "CancelOverLimitPaused" || log[len(log)-1] != "ClearRetainedContent" {
		t.Fatalf("the calls of the tick are %v: the two sweeps must come LAST, after the pass over the due jobs", log)
	}
	if first := log[0]; first != "EngineWork" {
		t.Errorf("the tick began with %q, want the read of the work", first)
	}
	if len(f.pauseCall) != 1 || f.pauseCall[0] != 36*time.Hour || f.pauseMax[0] != pauseJobsPerSweep {
		t.Errorf("the pause step was called with %v and %v, want the dial 36h and a batch of %d", f.pauseCall, f.pauseMax, pauseJobsPerSweep)
	}
	if len(f.keepCall) != 1 || f.keepCall[0] != 48*time.Hour || f.keepJobs[0] != retentionJobsPerSweep || f.keepPages[0] != retentionPagesPerSweep {
		t.Errorf("the retention step was called with %v, %v and %v, want the dial 48h, %d jobs and %d pages", f.keepCall, f.keepJobs, f.keepPages, retentionJobsPerSweep, retentionPagesPerSweep)
	}
	if len(f.deadline) != 1 || f.deadline[0] <= 0 || f.deadline[0] > 90*time.Second {
		t.Errorf("the pause step was given %v, want a deadline within the 90s step timeout", f.deadline)
	}
	if s := r.e.SweepStats(); s.PauseSweeps != 1 || s.RetentionSweeps != 1 {
		t.Errorf("sweep stats %+v, want one pass of each", s)
	}
}

func TestTick_ASweepRunsEvenWhenNoJobIsDue(t *testing.T) {
	r, f := sweepRig(t, Config{})
	r.e.Tick(context.Background())
	if p, k := f.calls(); p != 1 || k != 1 {
		t.Errorf("with no job due the sweeps ran %d and %d times, want 1 and 1: a paused job and an ended job are not in the work and must not wait for one that is", p, k)
	}
	// the control: the default dials are the spec's
	if f.pauseCall[0] != 7*24*time.Hour || f.keepCall[0] != 7*24*time.Hour {
		t.Errorf("an engine with no dials swept with %v and %v, want 7 days each", f.pauseCall, f.keepCall)
	}
}

func TestTick_ASweepIsLeaderOnlyAndAStandbyCallsNothing(t *testing.T) {
	r, f := sweepRig(t, Config{})
	lead := false
	r.e.gate = gateFn(func() bool { return lead })
	for i := 0; i < 3; i++ {
		r.e.Tick(context.Background())
	}
	if p, k := f.calls(); p != 0 || k != 0 || len(r.st.log.list()) != 0 {
		t.Fatalf("a standby swept: %d and %d calls, log %v", p, k, r.st.log.list())
	}
	lead = true // the control: the same engine, leading, sweeps
	r.e.Tick(context.Background())
	if p, k := f.calls(); p != 1 || k != 1 {
		t.Errorf("the same engine, leading, ran the sweeps %d and %d times, want 1 and 1", p, k)
	}
}

func TestSweep_RunsOnItsCadenceByTheEnginesClockAndAtEveryTickWhenTheCadenceIsNegative(t *testing.T) {
	r, f := sweepRig(t, Config{SweepEvery: time.Minute})
	tick := func(advance time.Duration) (int, int) {
		*r.now = r.now.Add(advance)
		r.e.Tick(context.Background())
		return f.calls()
	}
	if p, k := tick(0); p != 1 || k != 1 {
		t.Fatalf("the first tick ran the sweeps %d and %d times, want 1 and 1 (a restarted engine sweeps at once)", p, k)
	}
	if p, k := tick(59 * time.Second); p != 1 || k != 1 {
		t.Errorf("59 seconds later the sweeps had run %d and %d times, want still 1 and 1 (the cadence is a minute)", p, k)
	}
	if p, k := tick(time.Second); p != 2 || k != 2 {
		t.Errorf("a minute after the first the sweeps had run %d and %d times, want 2 and 2", p, k)
	}
	// the control: a negative cadence is every tick
	r2, f2 := sweepRig(t, Config{SweepEvery: -1})
	for i := 0; i < 3; i++ {
		r2.e.Tick(context.Background())
	}
	if p, k := f2.calls(); p != 3 || k != 3 {
		t.Errorf("a negative cadence ran the sweeps %d and %d times in three ticks, want 3 and 3", p, k)
	}
}

func TestSweep_AFailureOrAPanicIsCountedAndNeverStopsTheOtherStepNorTheTick(t *testing.T) {
	r, f := sweepRig(t, Config{SweepEvery: -1}) // no job is due: only the sweeps can fail
	f.pauseErr = errors.New("the database is gone")
	r.e.Tick(context.Background())
	if p, k := f.calls(); p != 1 || k != 1 {
		t.Fatalf("after a failing pause step the sweeps ran %d and %d times, want 1 and 1: one step's failure must not skip the other", p, k)
	}
	if got := r.e.Stats().Unexpected; got != 1 {
		t.Errorf("an unexpected failure of a sweep is counted %d times, want 1", got)
	}
	// a refusal of the lifecycle is a conflict like any step's, not an unexpected failure
	f.pauseErr = jobs.ErrNotAllowed
	r.e.Tick(context.Background())
	if r.e.Stats().Conflicts != 1 {
		t.Errorf("a refusal of the lifecycle in a sweep is counted %d times as a conflict, want 1", r.e.Stats().Conflicts)
	}

	for _, which := range []string{"pause", "retention"} {
		r2, f2 := sweepRig(t, Config{SweepEvery: -1})
		f2.panicOn = which
		r2.e.Tick(context.Background()) // must not panic
		r2.e.Tick(context.Background())
		if p, k := f2.calls(); p != 2 || k != 2 {
			t.Errorf("a panic in the %s step: the sweeps ran %d and %d times in two ticks, want 2 and 2", which, p, k)
		}
		if got := r2.e.Stats().Panics; got != 2 {
			t.Errorf("a panic in the %s step is counted %d times, want 2", which, got)
		}
	}
}

func TestSweepStats_CountWhatTheStepsReport(t *testing.T) {
	r, f := sweepRig(t, Config{SweepEvery: -1})
	f.pauseRes = jobs.PauseLimitResult{Found: 4, Cancelled: 1, Cancelling: 2, Failed: 1, Jobs: []jobs.PauseLimitJob{{JobID: jobA, Module: "m2", PausedFrom: "running", To: "cancelling"}}}
	f.keepRes = jobs.RetentionResult{Found: 3, Cleared: 2, Items: 12000, Failed: 1}
	r.e.Tick(context.Background())
	r.e.Tick(context.Background())
	want := SweepStats{PauseSweeps: 2, PauseCancelled: 2, PauseCancelling: 4, PauseFailed: 2, RetentionSweeps: 2, ContentCleared: 4, ContentItems: 24000, RetentionFailed: 2}
	if got := r.e.SweepStats(); got != want {
		t.Errorf("sweep stats %+v, want %+v", got, want)
	}
}

func TestSweep_AnEngineBuiltWithoutTheStoresSkipsBothSteps(t *testing.T) {
	r := newRig(t, Config{SweepEvery: -1}) // the rig of the passes over jobs sets neither
	r.e.Tick(context.Background())
	if strings.Contains(r.st.log.joined(), "CancelOverLimitPaused") || strings.Contains(r.st.log.joined(), "ClearRetainedContent") || r.e.SweepStats() != (SweepStats{}) {
		t.Errorf("an engine with no sweeping stores swept: %v %+v", r.st.log.list(), r.e.SweepStats())
	}
	// the control: New sets both from the job store it is given
	e, err := New(Deps{Store: new(jobs.Store), Modules: stubMods{}, Leader: gateFn(func() bool { return true })}, Config{})
	if err != nil || e.pl == nil || e.ret == nil {
		t.Fatalf("New left the sweeping stores unset (%v): the production engine would never sweep", err)
	}
}
