// Package jobenginetest holds the test doubles of the job engine: a deterministic FakeModule that behaves like a module (it holds rows, works them, reports the final ones, cancels what it has not
// claimed), a stub Gate, a Clock the test moves, and the crash switch the engine's restart tests use. It is imported by tests only: the platform binary does not link it (nothing under cmd/ or
// internal/app imports it, and a test of this package would fail the day one does: see TestNothingOutsideTestsImportsThisPackage).
package jobenginetest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// The states of a fake item, the senders' own words: queued and sending are not final; sent, bounced, failed and cancelled are.
const (
	StateQueued    = "queued"
	StateSending   = "sending"
	StateSent      = "sent"
	StateBounced   = "bounced"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Item is one row as the fake module holds it.
type Item struct {
	Ref     string         // the platform's item reference (job_rows.id)
	Email   string         // the normalised address
	Ordinal int            // its position in the job
	State   string         // queued, sending or a final state
	Final   charge.Outcome // what it becomes when it is worked (for module 1, its answer)
}

func (it Item) isFinal() bool {
	switch it.State {
	case StateQueued, StateSending:
		return false
	}
	return true
}

// Counts is how many times each method was called (every call counts, whether it answered or not).
type Counts struct{ Submit, Observe, Cancel, Total int }

type fakeJob struct {
	items     map[string]*Item
	order     []string // refs in the order the module accepted them
	cancelled bool
	ref       string
}

// FakeModule is a module the tests drive. Every field is set before the engine runs (or between ticks, from the test's own goroutine) and read under the lock. The zero value is a module 2 that
// accepts every row, works every item on the first Observe and reports everything in one page.
type FakeModule struct {
	// Module is the module id the fake plays: "m1" (verify: answers valid/accepted, returns a module reference which Observe then requires on the job it is handed, cannot cancel), "m2" or "m3"
	// (senders: answer sent, can cancel). Empty means "m2".
	Module string
	// Plan decides what a row becomes when the module has worked it (nil: sent for a sender, valid/accepted for module 1).
	Plan func(r jobs.EngineRow) charge.Outcome
	// AcceptPerCall is how many NEW rows one Submit takes (0: every row offered). Rows it already holds are returned again without counting (a replay).
	AcceptPerCall int
	// WaitSubmits makes the first n Submit calls answer ErrWait (a campaign that is still active, a backpressure 409).
	WaitSubmits int
	// FinalizePerObserve is how many items the module's worker finishes per Observe (0: all of them). An item in flight (sending) finishes first.
	FinalizePerObserve int
	// PageSize is how many items one call of Observe's callback carries (0: all in one page).
	PageSize int
	// LeakNotFinal makes Observe hand over the items that are not final too (a misbehaving module): the platform must record none of them.
	LeakNotFinal bool
	// Manual makes the module's worker settle NOTHING by itself: an item becomes final only when a test calls Settle for it (or Cancel takes it back). It is the lever of ModuleContract, which has
	// to say exactly which item is final and with what outcome, the way a stand-in of a real module is driven.
	Manual bool

	mu          sync.Mutex
	down        bool
	jobs        map[string]*fakeJob
	counts      Counts
	offers      map[string]int // item ref -> how many times a Submit call offered it
	held        map[string]int // item ref -> how many times a Submit call answered that it holds it
	submitsSeen int

	armed    bool
	armN     int
	armAfter bool
	armKill  func()
}

func (f *FakeModule) module() string {
	if f.Module == "" {
		return jobs.ModuleSendManaged
	}
	return f.Module
}

func (f *FakeModule) job(id string) *fakeJob {
	if f.jobs == nil {
		f.jobs = map[string]*fakeJob{}
	}
	j, ok := f.jobs[id]
	if !ok {
		j = &fakeJob{items: map[string]*Item{}}
		f.jobs[id] = j
	}
	return j
}

func (f *FakeModule) plan(r jobs.EngineRow) charge.Outcome {
	if f.Plan != nil {
		return f.Plan(r)
	}
	if f.module() == jobs.ModuleVerify {
		return charge.Outcome{Module: jobs.ModuleVerify, State: "valid", Reason: "accepted"}
	}
	return charge.Outcome{Module: f.module(), State: StateSent}
}

// ---- switches -------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// SetDown makes every method answer ErrWait without touching its state (the module is unreachable) until it is set false.
func (f *FakeModule) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// Arm makes the module end the engine that is calling it, ONCE. kill is called (a test cancels the context of the engine it is about to abandon: the process "died"). With after false the module
// dies BEFORE it executes call number n+1 (n calls completed; the call has no effect and answers context.Canceled); with after true it dies right AFTER call number n executed (its effect stands,
// its answer is returned, and the engine's very next database call fails with the cancelled context). Arm(0, false, ...) dies before the first call; Arm(0, true, ...) never fires.
func (f *FakeModule) Arm(n int, after bool, kill func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.armN, f.armAfter, f.armKill = true, n, after, kill
}

// Armed says whether the crash switch is still waiting to fire.
func (f *FakeModule) Armed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.armed
}

// Disarm removes the crash switch.
func (f *FakeModule) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.armKill = false, nil
}

// ---- observing the fake ----------------------------------------------------------------------------------------------------------------------------------------------------------------

// Calls returns how many times each method was called.
func (f *FakeModule) Calls() Counts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts
}

// Offers is how many times a Submit call was offered the row (a replay offers it again).
func (f *FakeModule) Offers(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offers[ref]
}

// Held is how many times a Submit call answered that it holds the row.
func (f *FakeModule) Held(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[ref]
}

// Items returns the module's items of a job, in the order it accepted them.
func (f *FakeModule) Items(jobID string) []Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[jobID]
	if j == nil {
		return nil
	}
	out := make([]Item, 0, len(j.order))
	for _, ref := range j.order {
		out = append(out, *j.items[ref])
	}
	return out
}

// HeldCount is how many rows the module holds for the job.
func (f *FakeModule) HeldCount(jobID string) int { return len(f.Items(jobID)) }

// FinalOutcomes is the outcome of every item of the job that is final, by item reference: what a correct platform must have settled and recorded.
func (f *FakeModule) FinalOutcomes(jobID string) map[string]charge.Outcome {
	out := map[string]charge.Outcome{}
	for _, it := range f.Items(jobID) {
		if it.isFinal() {
			out[it.Ref] = f.reportedOf(it)
		}
	}
	return out
}

// reportedOf is what the module reports for a final item: its planned outcome, or `cancelled` for an item the module took back before it was claimed.
func (f *FakeModule) reportedOf(it Item) charge.Outcome {
	if it.State == StateCancelled {
		return charge.Outcome{Module: f.module(), State: StateCancelled}
	}
	return it.Final
}

// Settle makes the module's worker settle one item of the job with outcome o: it becomes final, in the state o names, and Observe reports it with exactly that outcome. It returns false for a job or an
// item the module does not hold (nothing changes) and for an item that is already final (a final item never changes).
func (f *FakeModule) Settle(jobID, ref string, o charge.Outcome) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[jobID]
	if j == nil {
		return false
	}
	it := j.items[ref]
	if it == nil || it.isFinal() {
		return false
	}
	it.Final, it.State = o, o.State
	return true
}

// ClaimInFlight puts the first n queued items of the job in flight (state sending): the worker has claimed them, so a cancel cannot take them back.
func (f *FakeModule) ClaimInFlight(jobID string, n int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[jobID]
	if j == nil {
		return 0
	}
	claimed := 0
	for _, ref := range j.order {
		if claimed == n {
			break
		}
		if it := j.items[ref]; it.State == StateQueued {
			it.State = StateSending
			claimed++
		}
	}
	return claimed
}

// ---- the crash switch and the common prologue of every method -----------------------------------------------------------------------------------------------------------------------------------

// errDied is what a call that the crash switch ended before it began answers. It wraps context.Canceled: the engine sees what it would see if its own context had ended.
var errDied = fmt.Errorf("the process died: %w", context.Canceled)

// begin counts the call and applies the crash switch and the outage. The caller holds f.mu. It returns an error when the call must not run, and a func to run (still under the lock) after the
// call executed, which fires an "after" crash.
func (f *FakeModule) begin(method string) (err error, afterCall func()) {
	if f.armed && !f.armAfter && f.counts.Total == f.armN {
		kill := f.armKill
		f.armed, f.armKill = false, nil
		if kill != nil {
			kill()
		}
		return errDied, nil
	}
	f.counts.Total++
	switch method {
	case "submit":
		f.counts.Submit++
	case "observe":
		f.counts.Observe++
	case "cancel":
		f.counts.Cancel++
	}
	if f.down {
		return jobengine.Wait("the module is down"), nil
	}
	if f.armed && f.armAfter && f.counts.Total == f.armN {
		kill := f.armKill
		f.armed, f.armKill = false, nil
		return nil, func() {
			if kill != nil {
				kill()
			}
		}
	}
	return nil, nil
}

// ---- the Module interface -------------------------------------------------------------------------------------------------------------------------------------------------------------------

// Submit holds the offered rows (idempotent), at most AcceptPerCall new ones per call.
func (f *FakeModule) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (jobengine.SubmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, after := f.begin("submit"); err != nil {
		return jobengine.SubmitResult{}, err
	} else if after != nil {
		defer after()
	}
	if f.offers == nil {
		f.offers, f.held = map[string]int{}, map[string]int{}
	}
	for _, r := range rows {
		f.offers[r.ID]++
	}
	f.submitsSeen++
	if f.submitsSeen <= f.WaitSubmits {
		return jobengine.SubmitResult{}, jobengine.Wait("a campaign is active")
	}
	fj := f.job(j.ID)
	if fj.cancelled {
		return jobengine.SubmitResult{}, jobengine.Refused("the job was cancelled")
	}
	res := jobengine.SubmitResult{}
	fresh := 0
	for _, r := range rows {
		if _, ok := fj.items[r.ID]; !ok {
			if f.AcceptPerCall > 0 && fresh >= f.AcceptPerCall {
				continue
			}
			fresh++
			fj.items[r.ID] = &Item{Ref: r.ID, Email: r.EmailNorm, Ordinal: r.Ordinal, State: StateQueued, Final: f.plan(r)}
			fj.order = append(fj.order, r.ID)
		}
		f.held[r.ID]++
		res.Accepted = append(res.Accepted, r.ID)
	}
	if f.module() == jobs.ModuleVerify {
		if fj.ref == "" {
			fj.ref = "fake-verify-" + j.ID
		}
		res.ModuleRef = fj.ref
	}
	return res, nil
}

// work moves the module's worker on by one Observe: items in flight finish first, then queued ones are claimed and finished, FinalizePerObserve in all (0: every one), unless the job is cancelled
// (nothing new is claimed once the module has cancelled the job).
func (f *FakeModule) work(fj *fakeJob) {
	if f.Manual {
		return
	}
	budget := f.FinalizePerObserve
	if budget == 0 {
		budget = len(fj.order)
	}
	for _, ref := range fj.order {
		if budget == 0 {
			return
		}
		if it := fj.items[ref]; it.State == StateSending {
			it.State = it.Final.State
			budget--
		}
	}
	if fj.cancelled {
		return
	}
	for _, ref := range fj.order {
		if budget == 0 {
			return
		}
		if it := fj.items[ref]; it.State == StateQueued {
			it.State = it.Final.State
			budget--
		}
	}
}

// Observe works the job and hands every final item (all of them, every time: the engine must tolerate repeats) to fn in pages.
func (f *FakeModule) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (jobengine.Observation, error) {
	f.mu.Lock()
	if err, after := f.begin("observe"); err != nil {
		f.mu.Unlock()
		return jobengine.Observation{}, err
	} else if after != nil {
		defer after()
	}
	fj := f.job(j.ID)
	if f.module() == jobs.ModuleVerify && (j.ModuleRef == "" || j.ModuleRef != fj.ref) {
		// module 1 knows its job by the handle it returned from Submit, and the engine hands it back on the job as the store read it (Start recorded it in platform.jobs.module_job_ref, EngineWork and
		// EngineJobByID carry it). A job without it, or with another, is a job this module cannot say anything about: it waits, like the real wrapper, and the engine's tests prove the handle arrives.
		f.mu.Unlock()
		return jobengine.Observation{}, jobengine.Wait("module 1 is not given its reference of the job")
	}
	f.work(fj)
	var items []jobs.ResultItem
	terminal := 0
	for _, ref := range fj.order {
		it := fj.items[ref]
		if it.isFinal() {
			terminal++
			items = append(items, jobs.ResultItem{ItemRef: it.Ref, Outcome: f.reportedOf(*it), Detail: map[string]any{"fake": true, "ordinal": it.Ordinal}})
		} else if f.LeakNotFinal {
			items = append(items, jobs.ResultItem{ItemRef: it.Ref, Outcome: charge.Outcome{Module: f.module(), State: StateQueued}})
		}
	}
	obs := jobengine.Observation{Complete: len(fj.order) > 0 && terminal == len(fj.order), Total: len(fj.order), Terminal: terminal, Detail: map[string]any{"fake": true}}
	page := f.PageSize
	if page <= 0 {
		page = len(items)
	}
	// the callback runs the platform's database transaction: the module's lock is not held across it, so a test's second engine could call in meanwhile (it never does in these tests, but the lock
	// of a real module would not be held either)
	f.mu.Unlock()
	for start := 0; start < len(items); start += page {
		if err := fn(items[start:min(start+page, len(items))]); err != nil {
			return jobengine.Observation{}, err
		}
	}
	return obs, nil
}

// Cancel cancels the job: every item that is still queued becomes cancelled, an item in flight finishes as it was going to, nothing new is claimed. Module 1 cannot.
func (f *FakeModule) Cancel(ctx context.Context, j jobs.EngineJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, after := f.begin("cancel"); err != nil {
		return err
	} else if after != nil {
		defer after()
	}
	if f.module() == jobs.ModuleVerify {
		return jobengine.ErrNotSupported
	}
	fj := f.job(j.ID)
	fj.cancelled = true
	for _, ref := range fj.order {
		if it := fj.items[ref]; it.State == StateQueued {
			it.State = StateCancelled
		}
	}
	return nil
}

var _ jobengine.Module = (*FakeModule)(nil)

// Modules is a jobengine.Modules that answers with one FakeModule (or, by module id, the ones in ByModule), or with an error while Err is set.
type Modules struct {
	M        *FakeModule
	ByModule map[string]*FakeModule
	mu       sync.Mutex
	err      error
}

// SetErr makes For fail with err (nil: answer again).
func (m *Modules) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// For returns the module of the job.
func (m *Modules) For(ctx context.Context, userID, moduleID string) (jobengine.Module, error) {
	m.mu.Lock()
	err := m.err
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if fm, ok := m.ByModule[moduleID]; ok {
		return fm, nil
	}
	if m.M == nil {
		return nil, errors.New("jobenginetest: no module for " + moduleID)
	}
	return m.M, nil
}

var _ jobengine.Modules = (*Modules)(nil)

// Gate is a jobengine.Gate a test flips: it leads while Lead is true.
type Gate struct {
	lead  atomic.Bool
	calls atomic.Int64
}

// NewGate returns a gate that leads (or not).
func NewGate(leading bool) *Gate {
	g := &Gate{}
	g.lead.Store(leading)
	return g
}

// Set says whether the gate leads from now on.
func (g *Gate) Set(leading bool) { g.lead.Store(leading) }

// Calls is how many times the engine asked.
func (g *Gate) Calls() int { return int(g.calls.Load()) }

// Ensure answers.
func (g *Gate) Ensure(ctx context.Context) bool {
	g.calls.Add(1)
	return g.lead.Load() && ctx.Err() == nil
}

var _ jobengine.Gate = (*Gate)(nil)

// Clock is a clock the test moves; the engine's staleness arithmetic reads it.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts the clock at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// SortedRefs returns the references of a set of outcomes in a fixed order (for stable test output).
func SortedRefs(m map[string]charge.Outcome) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
