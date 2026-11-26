package jobengine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 4 close-out D1: pause and resume of a RUNNING job of module 2 or 3, and the cancel of a module 1 job, as the ENGINE does them (the decisions, with doubles for the store and the module; the
// statements and the whole loop against a real Postgres are tests/harness/jobs_pause_cancel_test.go). Every test has a control: the same step with the one thing that matters taken away.

// pausedJob is a job the user paused while it was running (status paused, paused_from running).
func pausedJob(module string) jobs.EngineJob {
	j := ej(jobA, module, jobs.StatusPaused)
	j.PausedFrom = jobs.StatusRunning
	return j
}

// moduleHold is the state of a module's campaign as a test script drives it: what its report says (held) and how many calls it has answered.
type moduleHold struct {
	held, complete bool
	total          int
	items          []jobs.ResultItem // handed to the callback at every observation (the module reports what is final, every time)
}

// wire makes the stub module report the hold and make Pause/Resume take effect at once (takes true) or never (takes false: a module whose report does not change).
func (m *stubMod) wire(h *moduleHold, takes bool) {
	m.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		if len(h.items) > 0 {
			if err := fn(h.items); err != nil {
				return Observation{}, err
			}
		}
		return Observation{Complete: h.complete, Total: max(h.total, 4), Terminal: len(h.items), Paused: h.held}, nil
	}
	m.pause = func() error {
		if takes {
			h.held = true
		}
		return nil
	}
	m.resume = func() error {
		if takes {
			h.held = false
		}
		return nil
	}
}

func notSupported(what string) error { return fmt.Errorf("%w: %s", ErrNotSupported, what) }

// The engine's whole behaviour for a job paused while running: observe, hold until the REPORT says held, observe again, and NEVER submit.
func TestPausedRunning_CallsPauseUntilTheReportSaysPaused_ThenOnlyObservesAndNeverSubmits(t *testing.T) {
	h := &moduleHold{}
	r := newRig(t, Config{SettleEvery: time.Hour}, pausedJob("m2")) // an hour of SettleEvery: a paused job is observed at every look all the same
	r.st.pending = rows(3)                                          // rows the engine has not handed over: they wait for the resume
	r.mod.wire(h, true)
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,Observe,Pause,Observe" {
		t.Fatalf("first look: %s, want EngineWork,Observe,Pause,Observe (observe, ask for the hold, observe AGAIN so the hold is seen in this very step)", got)
	}
	r.e.Tick(context.Background())
	r.e.Tick(context.Background())
	if r.st.log.count("Pause") != 1 || r.st.log.count("Observe") != 4 {
		t.Errorf("after the report said paused: %v (want ONE Pause and an Observe at every look, SettleEvery notwithstanding)", r.st.log.list())
	}
	for _, never := range []string{"Submit", "PendingRows", "EnsureJobItems", "MarkSubmitted", "Start", "Expire", "FailJob", "Complete", "Finish"} {
		if r.st.log.count(never) != 0 {
			t.Errorf("a paused job called %s: %v (no row is offered to a held campaign, and nothing ends it)", never, r.st.log.list())
		}
	}
	if len(r.st.pending) != 3 {
		t.Errorf("%d rows left, want 3: a paused job stamps nothing", len(r.st.pending))
	}
	// the module's report is the witness: Pause is asked again at every look until it says so
	c := &moduleHold{}
	rc := newRig(t, Config{SettleEvery: time.Hour}, pausedJob("m2"))
	rc.mod.wire(c, false) // a module that answers the call and holds nothing
	for i := 0; i < 3; i++ {
		rc.e.Tick(context.Background())
	}
	if rc.st.log.count("Pause") != 3 {
		t.Errorf("a module whose report never says paused was asked %d times in 3 looks, want 3 (an answered call is not a held campaign)", rc.st.log.count("Pause"))
	}
	// the control: the same job RUNNING is never asked to pause (the pause is the platform's move, and the engine reads it from the status)
	run := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	run.mod.wire(&moduleHold{}, true)
	run.e.Tick(context.Background())
	if run.st.log.count("Pause") != 0 {
		t.Errorf("a running job was asked to pause: %v", run.st.log.list())
	}
}

func TestPausedRunning_AModuleThatIsCompleteOrAlreadyHoldingIsNotAskedToPause(t *testing.T) {
	// holding: no call
	hold := newRig(t, Config{}, pausedJob("m3"))
	hold.mod.wire(&moduleHold{held: true}, true)
	hold.e.Tick(context.Background())
	if got := hold.st.log.joined(); got != "EngineWork,Observe" {
		t.Errorf("a campaign the module holds: %s", got)
	}
	// complete, every row has its result: the job completes the normal way, and Pause is never called
	done := newRig(t, Config{}, pausedJob("m2"))
	done.mod.wire(&moduleHold{complete: true, total: 3}, true)
	done.st.fresh = []jobs.EngineJob{freshJob(3, 3, 0)}
	done.e.Tick(context.Background())
	if got := done.st.log.joined(); got != "EngineWork,Observe,EngineJobByID,Complete,Finish" {
		t.Errorf("a complete paused job: %s, want EngineWork,Observe,EngineJobByID,Complete,Finish (paused -> completing -> succeeded)", got)
	}
	if n := len(done.st.polls); n != 1 || !done.st.polls[0].progressed || done.st.polls[0].status != jobs.StatusPaused {
		t.Errorf("what the engine told the store of the step: %+v, want one note, progressed, in status paused", done.st.polls)
	}
	// complete but rows still to be handed over: nothing is finished, nothing is flagged, nothing is paused
	rest := newRig(t, Config{}, pausedJob("m2"))
	rest.mod.wire(&moduleHold{complete: true, total: 3}, true)
	rest.st.fresh = []jobs.EngineJob{freshJob(5, 3, 2)}
	rest.e.Tick(context.Background())
	if rest.st.log.count("Complete")+rest.st.log.count("Finish")+rest.st.log.count("Flag")+rest.st.log.count("Pause") != 0 {
		t.Errorf("a paused job with rows still to submit: %v", rest.st.log.list())
	}
	// complete but a SUBMITTED row has no result: held back and flagged, as for a running job
	miss := newRig(t, Config{}, pausedJob("m2"))
	miss.mod.wire(&moduleHold{complete: true, total: 3}, true)
	miss.st.fresh = []jobs.EngineJob{freshJob(3, 2, 0)}
	miss.e.Tick(context.Background())
	if miss.st.log.count("Complete") != 0 || miss.st.log.count("Flag") != 1 {
		t.Errorf("a paused job complete over a missing result: %v", miss.st.log.list())
	}
	// the control: the module is neither complete nor holding: it IS asked
	ctl := newRig(t, Config{}, pausedJob("m2"))
	ctl.mod.wire(&moduleHold{}, true)
	ctl.e.Tick(context.Background())
	if ctl.st.log.count("Pause") != 1 {
		t.Errorf("the control was not asked to pause: %v", ctl.st.log.list())
	}
}

// A job that was running has no start to miss, and a held job is not expected to progress: neither expiry nor the stale event may touch it.
func TestPausedRunning_NeverExpiresAndNeverRaisesTheStaleEvent(t *testing.T) {
	last := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": 15 * time.Minute}}
	pj := pausedJob("m2")
	pj.DeadlinePassed = true // its start deadline is long past (it was confirmed hours ago): that is what a running job's deadline is
	pj.LastProgressAt = &last
	r := newRig(t, cfg, pj)
	*r.now = last.Add(20 * time.Hour)
	r.mod.wire(&moduleHold{held: true}, true)
	r.e.Tick(context.Background())
	r.e.Tick(context.Background())
	if r.st.log.count("Expire")+r.st.log.count("AlertStale")+r.st.log.count("Flag")+r.st.log.count("FailJob") != 0 {
		t.Errorf("a held job, hours without progress and past its deadline: %v (no expiry, no stale event, nothing invented)", r.st.log.list())
	}
	if r.st.log.count("Observe") != 2 {
		t.Errorf("the held job was not looked at: %v", r.st.log.list())
	}
	// control 1: a job paused BEFORE its start, past its deadline, IS expired (and the module is not asked anything)
	pre := ej(jobA, "m2", jobs.StatusPaused)
	pre.PausedFrom = jobs.StatusScheduled
	pre.DeadlinePassed = true
	c1 := newRig(t, cfg, pre)
	c1.e.Tick(context.Background())
	if got := c1.st.log.joined(); got != "EngineWork,Expire" {
		t.Errorf("a job paused before its start, past its deadline: %s, want EngineWork,Expire", got)
	}
	// control 2: the SAME job, running, no progress for 20 hours: the stale event is raised (so its absence above is the pause's)
	run := ej(jobA, "m2", jobs.StatusRunning)
	run.DeadlinePassed = true
	run.LastProgressAt = &last
	c2 := newRig(t, cfg, run)
	*c2.now = last.Add(20 * time.Hour)
	c2.mod.wire(&moduleHold{}, true)
	c2.e.Tick(context.Background())
	if c2.st.log.count("AlertStale") != 1 || c2.st.log.count("Expire") != 0 {
		t.Errorf("the control (the same job running): %v, want one stale event and no expiry", c2.st.log.list())
	}
}

func TestPausedRunning_RecordsWhatWasInFlightAndNotesProgressOnlyWhenSomethingIsNew(t *testing.T) {
	two := []jobs.ResultItem{{ItemRef: rows(2)[0].ID}, {ItemRef: rows(2)[1].ID}}
	r := newRig(t, Config{}, pausedJob("m2"))
	r.mod.wire(&moduleHold{held: true, items: two}, true)
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,Observe,RecordResults,NoteProgress" {
		t.Fatalf("a held job whose in-flight items finished: %s, want EngineWork,Observe,RecordResults,NoteProgress", got)
	}
	if n := len(r.st.polls); n != 1 || !r.st.polls[0].progressed {
		t.Errorf("the step made progress and the store was told %+v", r.st.polls)
	}
	// the control: a held job with nothing new records nothing, notes no progress and is told "no progress" (so it backs off instead of being looked at every tick)
	q := newRig(t, Config{}, pausedJob("m2"))
	q.mod.wire(&moduleHold{held: true}, true)
	q.e.Tick(context.Background())
	if got := q.st.log.joined(); got != "EngineWork,Observe" || len(q.st.polls) != 1 || q.st.polls[0].progressed {
		t.Errorf("a quiet held job: %s / %+v", got, q.st.polls)
	}
	// a page the store refuses ends the step with its error; what was recorded before stays recorded and the module is not asked to pause over it
	f := newRig(t, Config{}, pausedJob("m2"))
	f.mod.wire(&moduleHold{items: two}, true)
	f.st.errs["RecordResults"] = errors.New("the database refused")
	f.e.Tick(context.Background())
	if f.st.log.count("Pause") != 0 || f.e.Stats().Unexpected != 1 {
		t.Errorf("a failing page of a paused job: %v / %+v", f.st.log.list(), f.e.Stats())
	}
}

func TestPausedRunning_ANotSupportedPauseIsFlaggedOncePerIntervalAndTheStepReturnsTheError(t *testing.T) {
	r := newRig(t, Config{StaleAfter: map[string]time.Duration{"m2": time.Hour}}, pausedJob("m2"))
	r.mod.wire(&moduleHold{}, true)
	r.mod.pause = func() error { return notSupported("wrapper-text-that-must-not-reach-an-event") }
	for i := 0; i < 4; i++ {
		r.e.Tick(context.Background())
	}
	if r.st.log.count("Pause") != 4 || r.st.log.count("Flag") != 1 {
		t.Fatalf("four looks inside the interval: %v (want a Pause at every look, ONE flag)", r.st.log.list())
	}
	if r.e.Stats().Unexpected != 4 {
		t.Errorf("the step did not return the error: %+v", r.e.Stats())
	}
	d := r.st.flags[0]
	if d["operation"] != "pause" || d["module"] != "m2" {
		t.Errorf("the flag names %v / %v, want the operation and the module", d["operation"], d["module"])
	}
	for k, v := range d {
		if strings.Contains(fmt.Sprint(v), "wrapper-text") {
			t.Errorf("the flag's %q carries the module wrapper's text: %v", k, v)
		}
	}
	*r.now = r.now.Add(61 * time.Minute)
	r.e.Tick(context.Background())
	if r.st.log.count("Flag") != 2 {
		t.Errorf("after the interval: %d flags, want a second one", r.st.log.count("Flag"))
	}
	// a flag that could not be written is tried again at the next look and is no reason to fail anything
	f := newRig(t, Config{}, pausedJob("m2"))
	f.mod.wire(&moduleHold{}, true)
	f.mod.pause = func() error { return notSupported("x") }
	f.st.errs["Flag"] = errors.New("the database is down")
	f.e.Tick(context.Background())
	delete(f.st.errs, "Flag")
	f.e.Tick(context.Background())
	if f.st.log.count("Flag") != 2 || len(f.st.flags) != 1 || f.st.log.count("FailJob") != 0 {
		t.Errorf("a failed flag write: %v / %d written", f.st.log.list(), len(f.st.flags))
	}
	// the control: a pause the module accepts flags nothing
	ok := newRig(t, Config{}, pausedJob("m2"))
	ok.mod.wire(&moduleHold{}, true)
	ok.e.Tick(context.Background())
	if ok.st.log.count("Flag") != 0 {
		t.Errorf("an accepted pause was flagged: %v", ok.st.log.list())
	}
	// and an ErrWait of the module is NOT the module declining: no flag
	w := newRig(t, Config{}, pausedJob("m2"))
	w.mod.wire(&moduleHold{}, true)
	w.mod.pause = func() error { return Wait("unavailable") }
	w.e.Tick(context.Background())
	if w.st.log.count("Flag") != 0 || w.e.Stats().Waits != 1 {
		t.Errorf("a module that cannot answer: %v / %+v", w.st.log.list(), w.e.Stats())
	}
}

func TestPausedRunning_AModuleThatCannotAnswerLeavesEverythingAsItIs(t *testing.T) {
	// the module cannot even be found
	a := newRig(t, Config{}, pausedJob("m2"))
	a.e.mods = stubMods{err: Wait("no key yet")}
	a.e.Tick(context.Background())
	if got := a.st.log.joined(); got != "EngineWork" || a.e.Stats().Waits != 1 {
		t.Errorf("a module that cannot be found: %s / %+v", got, a.e.Stats())
	}
	// it cannot report: nothing is asked of it, nothing is recorded or noted
	b := newRig(t, Config{}, pausedJob("m2"))
	b.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{}, Wait("down")
	}
	b.e.Tick(context.Background())
	if got := b.st.log.joined(); got != "EngineWork,Observe" || len(b.st.holds) != 0 || b.e.Stats().Waits != 1 {
		t.Errorf("a module that cannot report: %s / %v / %+v", got, b.st.holds, b.e.Stats())
	}
	// it reports but cannot be told: one observation only, the step is a wait
	c := newRig(t, Config{}, pausedJob("m2"))
	c.mod.wire(&moduleHold{}, true)
	c.mod.pause = func() error { return Wait("unavailable") }
	c.e.Tick(context.Background())
	if got := c.st.log.joined(); got != "EngineWork,Observe,Pause" || c.e.Stats().Waits != 1 {
		t.Errorf("a module that cannot be told: %s / %+v", got, c.e.Stats())
	}
	// the control: the same module answering is observed twice
	d := newRig(t, Config{}, pausedJob("m2"))
	d.mod.wire(&moduleHold{}, true)
	d.e.Tick(context.Background())
	if got := d.st.log.joined(); got != "EngineWork,Observe,Pause,Observe" {
		t.Errorf("the control: %s", got)
	}
	// a second observation that fails after the pause was accepted is the step's error, and what the first one recorded stays recorded
	e := newRig(t, Config{}, pausedJob("m2"))
	calls := 0
	e.mod.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		calls++
		if calls == 2 {
			return Observation{}, Wait("went away")
		}
		return Observation{Total: 2}, nil
	}
	e.e.Tick(context.Background())
	if e.st.log.count("Pause") != 1 || e.e.Stats().Waits != 1 {
		t.Errorf("a second observation that fails: %v / %+v", e.st.log.list(), e.e.Stats())
	}
}

// The other direction: the platform's job is running and the module still holds the campaign (the user resumed, or the engine died between the move and the call).
func TestRunning_AReportThatSaysHeldIsResumed_AndAReportThatDoesNotIsLeftAlone(t *testing.T) {
	h := &moduleHold{held: true}
	r := newRig(t, Config{SettleEvery: time.Hour}, ej(jobA, "m2", jobs.StatusRunning))
	r.mod.wire(h, true)
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,PendingRows,Observe,Resume" {
		t.Fatalf("a running job the module holds: %s, want EngineWork,PendingRows,Observe,Resume", got)
	}
	// the module let go: the next look sees it (a held report is looked at again at once, inside SettleEvery) and asks for nothing more
	r.e.Tick(context.Background())
	if r.st.log.count("Resume") != 1 || r.st.log.count("Observe") != 2 {
		t.Errorf("after the resume took: %v (want one Resume and a second observation)", r.st.log.list())
	}
	// and once the report says it is not held, SettleEvery throttles the observation again
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != 2 {
		t.Errorf("observed again at once with nothing new and the hold gone: %d observations", r.st.log.count("Observe"))
	}
	if !reflect.DeepEqual(r.st.holds, []bool{true, false}) {
		t.Errorf("what the history was told: %v, want [true false] (module_paused, then module_resumed)", r.st.holds)
	}
	// the control: a running job whose report says not held is never resumed
	c := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	c.mod.wire(&moduleHold{}, true)
	for i := 0; i < 3; i++ {
		c.e.Tick(context.Background())
	}
	if c.st.log.count("Resume") != 0 {
		t.Errorf("a campaign that is not held was asked to resume: %v", c.st.log.list())
	}
	// a Resume the module cannot be told is a wait; a module that DECLINES it is flagged once per interval
	w := newRig(t, Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}}, ej(jobA, "m2", jobs.StatusRunning))
	w.mod.wire(&moduleHold{held: true}, false)
	w.mod.resume = func() error { return Wait("unavailable") }
	w.e.Tick(context.Background())
	if w.e.Stats().Waits != 1 || w.st.log.count("Flag") != 0 {
		t.Errorf("a resume that waited: %v / %+v", w.st.log.list(), w.e.Stats())
	}
	n := newRig(t, Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}}, ej(jobA, "m2", jobs.StatusRunning))
	n.mod.wire(&moduleHold{held: true}, false)
	n.mod.resume = func() error { return notSupported("no such route") }
	for i := 0; i < 3; i++ {
		n.e.Tick(context.Background())
	}
	if n.st.log.count("Resume") != 3 || n.st.log.count("Flag") != 1 {
		t.Fatalf("a module that declines the resume: %v (want a Resume at every look and ONE flag)", n.st.log.list())
	}
	if d := n.st.flags[0]; d["operation"] != "resume" || d["module"] != "m2" {
		t.Errorf("the flag: %v", d)
	}
}

// A job the user resumed between two looks is observed at once, inside SettleEvery: the module may still hold it.
func TestRunning_AJobThatMovedSinceTheLastLookIsObservedAtOnce(t *testing.T) {
	h := &moduleHold{}
	r := newRig(t, Config{SettleEvery: time.Hour}, pausedJob("m2"))
	r.mod.wire(h, false) // a module whose report keeps saying "not held": the only thing that can make the next look observe is the job's own move
	r.e.Tick(context.Background())
	before := r.st.log.count("Observe")
	if before != 2 {
		t.Fatalf("the paused look observed %d times, want 2 (before and after the pause)", before)
	}
	r.st.work = []jobs.EngineJob{ej(jobA, "m2", jobs.StatusRunning)} // resumed
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != before+1 {
		t.Errorf("a job that moved from paused to running was not observed at once: %v", r.st.log.list())
	}
	r.e.Tick(context.Background()) // the control: the same job in the same status, nothing new, inside SettleEvery
	if r.st.log.count("Observe") != before+1 {
		t.Errorf("a job that did not move was observed again inside SettleEvery: %v", r.st.log.list())
	}
}

// What the history is told: the module's hold, when it is NEW to the engine, and never twice for the same thing.
func TestNoteHold_IsToldOnTheFirstLookAndOnEveryChangeAndNotOnARepeat(t *testing.T) {
	h := &moduleHold{}
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.mod.wire(h, false)
	steps := []struct {
		held bool
		want []bool
	}{
		{false, []bool{false}},              // the first look of a job this engine has no memory of: asked (the store writes nothing for "not held" after nothing)
		{false, []bool{false}},              // a repeat
		{true, []bool{false, true}},         // module_paused
		{true, []bool{false, true}},         // a repeat
		{false, []bool{false, true, false}}, // module_resumed
	}
	for i, s := range steps {
		h.held = s.held
		r.e.Tick(context.Background())
		if !reflect.DeepEqual(r.st.holds, s.want) {
			t.Fatalf("look %d (report held=%v): the store was told %v, want %v", i+1, s.held, r.st.holds, s.want)
		}
	}
	// memory inside the staleness horizon is kept when the job is merely out of this tick's work (a backoff): no new statement
	keep := len(r.st.holds)
	job := r.st.work
	r.st.work = nil
	r.e.Tick(context.Background())
	r.st.work = job
	r.e.Tick(context.Background())
	if len(r.st.holds) != keep {
		t.Errorf("the memory of a job that waited out one tick was lost: told %v", r.st.holds)
	}
	// the control: older than every staleness limit it is forgotten, and the job is asked about again (the store dedupes: it writes only a change)
	r.st.work = nil
	*r.now = r.now.Add(DefaultStaleAfter + time.Minute)
	r.e.Tick(context.Background())
	r.st.work = job
	r.e.Tick(context.Background())
	if len(r.st.holds) != keep+1 {
		t.Errorf("a forgotten job was not asked about again: told %v", r.st.holds)
	}
}

func TestNoteHold_AFailedNoteIsNotAFailedStepAndIsTriedAgainAtTheNextLook(t *testing.T) {
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.mod.wire(&moduleHold{}, true)
	r.st.errs["NoteModuleHold"] = errors.New("the database is down")
	r.e.Tick(context.Background())
	s := r.e.Stats()
	if len(r.st.holds) != 1 || s.Unexpected != 0 || s.Waits != 0 || s.Panics != 0 {
		t.Fatalf("a failed note: told %v / %+v (it is a report: it must not fail or wait the step)", r.st.holds, s)
	}
	delete(r.st.errs, "NoteModuleHold")
	r.e.Tick(context.Background())
	if len(r.st.holds) != 2 {
		t.Errorf("the note was not tried again at the next look: %v", r.st.holds)
	}
	r.e.Tick(context.Background())
	if len(r.st.holds) != 2 {
		t.Errorf("a note that was written was asked again: %v", r.st.holds)
	}
}

// Module 1 can be cancelled since the close-out: its cancelling job is asked at every tick like a sender's, its cancelled rows arrive as results of the charge table's own row (free), and the job
// ends when module 1 says every row is final.
func TestCancelling_Module1IsAskedToCancelLikeTheSendersAndItsCancelledRowsAreRecorded(t *testing.T) {
	j := ej(jobA, "m1", jobs.StatusCancelling)
	j.ModuleRef = "module-one-id"
	cancelled := jobs.ResultItem{ItemRef: rows(2)[1].ID, Outcome: charge.Outcome{Module: "m1", State: "cancelled", Reason: "cancelled_by_caller"}}
	settled := jobs.ResultItem{ItemRef: rows(2)[0].ID, Outcome: charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}}
	complete := false
	r := newRig(t, Config{}, j)
	r.mod.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		if err := fn([]jobs.ResultItem{settled, cancelled}); err != nil {
			return Observation{}, err
		}
		return Observation{Complete: complete, Total: 2, Terminal: 2}, nil
	}
	r.e.Tick(context.Background())
	r.e.Tick(context.Background())
	if r.st.log.count("Cancel") != 2 || r.st.log.count("Complete") != 0 || r.st.log.count("FailJob") != 0 {
		t.Errorf("a module 1 job whose module is not complete: %v (want the cancel asked at every tick, nothing ended)", r.st.log.list())
	}
	if len(r.mod.observed) != 2 || r.mod.observed[0].ModuleRef != "module-one-id" {
		t.Errorf("Observe was not handed the job as the store read it, module reference included: %+v", r.mod.observed)
	}
	var got []jobs.ResultItem
	for _, page := range r.st.recorded {
		got = append(got, page...)
	}
	if len(got) < 2 || got[1].Outcome != (charge.Outcome{Module: "m1", State: "cancelled", Reason: "cancelled_by_caller"}) {
		t.Errorf("the cancelled row was handed to the store as %+v, want module 1's own words", got)
	}
	// module 1 reports every row final: the job completes and finishes (it ends cancelled: the database holds the pairing)
	complete = true
	r.st.fresh = []jobs.EngineJob{freshJob(2, 2, 0)}
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); !strings.HasSuffix(got, "Cancel,Observe,RecordResults,NoteProgress,EngineJobByID,Complete,Finish") {
		t.Errorf("a module 1 cancel that finished: %s", got)
	}
	// the control: a module 1 JOB THAT IS RUNNING is never asked to cancel
	run := ej(jobA, "m1", jobs.StatusRunning)
	run.ModuleRef = "module-one-id"
	c := newRig(t, Config{SettleEvery: -1}, run)
	c.mod.observe = r.mod.observe
	c.e.Tick(context.Background())
	if c.st.log.count("Cancel") != 0 {
		t.Errorf("a running module 1 job was asked to cancel: %v", c.st.log.list())
	}
}
