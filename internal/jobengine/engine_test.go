package jobengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// The decisions of the engine, proved without a database: the order of its calls, what it does with a module that cannot answer, what it refuses to do (complete a job over a row it has no result
// for), and that a panic, a timeout or a standby gate cannot stop or double it. The statements those calls run, and the whole engine against a real Postgres and a fake module, are
// tests/harness/jobs_engine_test.go and jobs_engine_loop_test.go.

// ---- doubles ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// log is the shared record of every call the doubles receive, in order.
type callLog struct {
	mu sync.Mutex
	c  []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.c = append(l.c, s)
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.c...)
}

func (l *callLog) joined() string { return strings.Join(l.list(), ",") }

func (l *callLog) count(prefix string) int {
	n := 0
	for _, c := range l.list() {
		if c == prefix || strings.HasPrefix(c, prefix+"(") {
			n++
		}
	}
	return n
}

type fstore struct {
	log       *callLog
	mu        sync.Mutex
	work      []jobs.EngineJob
	pending   []jobs.EngineRow // the rows PendingRows hands out until MarkSubmitted stamps them
	fresh     []jobs.EngineJob // successive answers of EngineJobByID (the last one repeats)
	errs      map[string]error // method name -> the error it answers
	panicOn   string           // method name that panics
	started   bool
	cancelled int
	flags     []map[string]any    // the detail of every Flag call, in order
	polls     []pollNote          // every NotePoll call, in order
	holds     []bool              // the `held` of every NoteModuleHold call, in order (kept out of the call log like NotePoll: a note after every observation would be in every sequence the other tests assert)
	recorded  [][]jobs.ResultItem // the items of every RecordResults call, in order
}

// pollNote is one outcome the engine told the store.
type pollNote struct {
	id, status    string
	progressed    bool
	base, ceiling time.Duration
}

func newFstore() *fstore { return &fstore{log: &callLog{}, errs: map[string]error{}} }

func (s *fstore) hit(name string) error {
	s.log.add(name)
	if s.panicOn == name {
		panic("boom in " + name)
	}
	return s.errs[name]
}

func (s *fstore) EngineWork(ctx context.Context, limit int) ([]jobs.EngineJob, error) {
	if err := s.hit("EngineWork"); err != nil {
		return nil, err
	}
	return s.work, nil
}

func (s *fstore) EngineJobByID(ctx context.Context, id string) (jobs.EngineJob, error) {
	s.log.add("EngineJobByID")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errs["EngineJobByID"]; err != nil {
		return jobs.EngineJob{}, err
	}
	if len(s.fresh) == 0 {
		return jobs.EngineJob{Job: jobs.Job{ID: id}}, nil
	}
	j := s.fresh[0]
	if len(s.fresh) > 1 {
		s.fresh = s.fresh[1:]
	}
	return j, nil
}

func (s *fstore) PendingRows(ctx context.Context, id string, limit int) ([]jobs.EngineRow, error) {
	if err := s.hit("PendingRows"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]jobs.EngineRow(nil), s.pending[:min(limit, len(s.pending))]...), nil
}

func (s *fstore) EnsureJobItems(ctx context.Context, id string) (int, error) {
	if err := s.hit("EnsureJobItems"); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), nil
}

func (s *fstore) MarkSubmitted(ctx context.Context, id string, ids []string) error {
	if err := s.hit("MarkSubmitted"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := map[string]bool{}
	for _, i := range ids {
		gone[i] = true
	}
	kept := s.pending[:0:0]
	for _, r := range s.pending {
		if !gone[r.ID] {
			kept = append(kept, r)
		}
	}
	s.pending = kept
	return nil
}

func (s *fstore) Start(ctx context.Context, id, ref string) (jobs.Job, error) {
	if err := s.hit("Start(" + ref + ")"); err != nil {
		return jobs.Job{}, err
	}
	s.started = true
	return jobs.Job{ID: id, Status: jobs.StatusRunning}, nil
}

func (s *fstore) RecordResults(ctx context.Context, id string, items []jobs.ResultItem) (jobs.RecordSummary, error) {
	if err := s.hit("RecordResults"); err != nil {
		return jobs.RecordSummary{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = append(s.recorded, append([]jobs.ResultItem(nil), items...))
	return jobs.RecordSummary{Recorded: len(items)}, nil
}

func (s *fstore) Complete(ctx context.Context, id string) (jobs.Job, error) {
	return jobs.Job{ID: id}, s.hit("Complete")
}
func (s *fstore) Finish(ctx context.Context, id string) (jobs.Job, error) {
	return jobs.Job{ID: id}, s.hit("Finish")
}
func (s *fstore) Expire(ctx context.Context, id string) (jobs.Job, error) {
	return jobs.Job{ID: id}, s.hit("Expire")
}
func (s *fstore) FailJob(ctx context.Context, id, reason string) (jobs.Job, error) {
	s.log.add("FailJob")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs["FailJob.reason"] = errors.New(reason)
	return jobs.Job{ID: id}, s.errs["FailJob"]
}
func (s *fstore) CancelUnsubmitted(ctx context.Context, id string) (int, error) {
	return 3, s.hit("CancelUnsubmitted")
}
func (s *fstore) Release(ctx context.Context, id string) error { return s.hit("Release") }
func (s *fstore) NoteProgress(ctx context.Context, id string) error {
	return s.hit("NoteProgress")
}

// NotePoll records the outcome the engine told the store in its OWN list: the sequences the other tests assert (log) are the calls that act on a job, and a note after every step would be in all of them.
func (s *fstore) NotePoll(ctx context.Context, id, status string, progressed bool, base, ceiling time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls = append(s.polls, pollNote{id: id, status: status, progressed: progressed, base: base, ceiling: ceiling})
	return true, s.errs["NotePoll"]
}
func (s *fstore) AlertStale(ctx context.Context, id string, after time.Duration, d map[string]any) (bool, error) {
	if err := s.hit(fmt.Sprintf("AlertStale(%s)", after)); err != nil {
		return false, err
	}
	return true, nil
}
func (s *fstore) Flag(ctx context.Context, id string, d map[string]any) error {
	if err := s.hit("Flag"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags = append(s.flags, d)
	return nil
}

// NoteModuleHold records what the engine told the store about the module's hold of a job, in its OWN list (see holds); errs["NoteModuleHold"] makes it fail, after recording the attempt.
func (s *fstore) NoteModuleHold(ctx context.Context, id string, held bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = append(s.holds, held)
	return true, s.errs["NoteModuleHold"]
}

var _ store = (*fstore)(nil)

type stubMod struct {
	log     *callLog
	accept  int // how many of the offered rows Submit takes (-1: all)
	ref     string
	submit  func(ctx context.Context, rows []jobs.EngineRow) (SubmitResult, error)
	observe func(ctx context.Context, fn func([]jobs.ResultItem) error) (Observation, error)
	cancel  func() error
	pause   func() error // nil: the call answers nil
	resume  func() error
	// what the module was handed, as the engine handed it
	submitted, observed []jobs.EngineJob
}

func (m *stubMod) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (SubmitResult, error) {
	m.log.add("Submit")
	m.submitted = append(m.submitted, j)
	if m.submit != nil {
		return m.submit(ctx, rows)
	}
	n := len(rows)
	if m.accept >= 0 && m.accept < n {
		n = m.accept
	}
	res := SubmitResult{ModuleRef: m.ref}
	for _, r := range rows[:n] {
		res.Accepted = append(res.Accepted, r.ID)
	}
	return res, nil
}

func (m *stubMod) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (Observation, error) {
	m.log.add("Observe")
	m.observed = append(m.observed, j)
	if m.observe != nil {
		return m.observe(ctx, fn)
	}
	return Observation{}, nil
}

func (m *stubMod) Cancel(ctx context.Context, j jobs.EngineJob) error {
	m.log.add("Cancel")
	if m.cancel != nil {
		return m.cancel()
	}
	return nil
}

func (m *stubMod) Pause(ctx context.Context, j jobs.EngineJob) error {
	m.log.add("Pause")
	if m.pause != nil {
		return m.pause()
	}
	return nil
}

func (m *stubMod) Resume(ctx context.Context, j jobs.EngineJob) error {
	m.log.add("Resume")
	if m.resume != nil {
		return m.resume()
	}
	return nil
}

type stubMods struct {
	m   Module
	err error
}

func (s stubMods) For(ctx context.Context, user, module string) (Module, error) { return s.m, s.err }

type gateFn func() bool

func (g gateFn) Ensure(ctx context.Context) bool { return g() }

const (
	jobA = "aaaaaaaa-0000-4000-8000-00000000000a"
	jobB = "bbbbbbbb-0000-4000-8000-00000000000b"
)

func rows(n int) []jobs.EngineRow {
	out := make([]jobs.EngineRow, n)
	for i := range out {
		out[i] = jobs.EngineRow{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Ordinal: i + 1, EmailNorm: fmt.Sprintf("r%d@example.test", i+1)}
	}
	return out
}

func ej(id, module, status string) jobs.EngineJob {
	return jobs.EngineJob{Job: jobs.Job{ID: id, UserID: "u", Module: module, Status: status}}
}

type rig struct {
	e   *Engine
	st  *fstore
	mod *stubMod
	now *time.Time
}

func newRig(t *testing.T, cfg Config, jobList ...jobs.EngineJob) *rig {
	t.Helper()
	st := newFstore()
	st.work = jobList
	mod := &stubMod{log: st.log, accept: -1}
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &rig{st: st, mod: mod, now: &now}
	e, err := newEngine(st, stubMods{m: mod}, gateFn(func() bool { return true }), nil, func() time.Time { return *r.now }, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

// ---- construction and configuration --------------------------------------------------------------------------------------------------------------------------------------------------------

func TestNew_RefusesWhatItCannotRunWith(t *testing.T) {
	if _, err := New(Deps{}, Config{}); err == nil || !strings.Contains(err.Error(), "Store") {
		t.Errorf("no store: %v", err)
	}
	if _, err := New(Deps{Store: new(jobs.Store)}, Config{}); err == nil || !strings.Contains(err.Error(), "Modules") {
		t.Errorf("no modules: %v", err)
	}
	if _, err := New(Deps{Store: new(jobs.Store), Modules: stubMods{}}, Config{}); err == nil || !strings.Contains(err.Error(), "Leader") {
		t.Errorf("no gate: %v", err)
	}
	// the control: all three given and a zero Config is an engine with the defaults
	e, err := New(Deps{Store: new(jobs.Store), Modules: stubMods{}, Leader: gateFn(func() bool { return true })}, Config{})
	if err != nil || e == nil {
		t.Fatalf("a complete Deps was refused: %v", err)
	}
	if e.cfg.Tick != DefaultTick || e.cfg.SettleEvery != DefaultSettleEvery || e.cfg.SubmitBatch != DefaultSubmitBatch || e.cfg.WorkLimit != DefaultWorkLimit || e.cfg.StepTimeout != DefaultStepTimeout {
		t.Errorf("defaults: %+v", e.cfg)
	}
	if DefaultTick != 1500*time.Millisecond || DefaultSettleEvery != 10*time.Second || DefaultStaleAfter != 15*time.Minute || DefaultSubmitBatch != 500 {
		t.Error("a default dial moved: Docs/phase-4-spec.md open decision 8 says tick 1.5 s, settle 10 s, staleness 15 minutes, 500 rows per submit")
	}
}

func TestConfig_NegativeAndNonsenseDialsAreRefusedAndTheLegalTwinsPass(t *testing.T) {
	for name, c := range map[string]Config{
		"a negative tick":           {Tick: -1},
		"a negative step timeout":   {StepTimeout: -time.Second},
		"a negative batch":          {SubmitBatch: -1},
		"a negative work limit":     {WorkLimit: -5},
		"a zero staleness limit":    {StaleAfter: map[string]time.Duration{"m1": 0}},
		"a negative staleness":      {StaleAfter: map[string]time.Duration{"m2": -time.Minute}},
		"a zero per-module batch":   {SubmitBatchFor: map[string]int{"m1": 0}},
		"a negative per-module one": {SubmitBatchFor: map[string]int{"m1": -3}},
	} {
		if _, err := newEngine(newFstore(), stubMods{}, gateFn(func() bool { return true }), nil, nil, c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, c := range map[string]Config{
		"nothing":                         {},
		"a negative SettleEvery (always)": {SettleEvery: -1},
		"explicit dials":                  {Tick: time.Second, SettleEvery: time.Minute, SubmitBatch: 2, WorkLimit: 1, StepTimeout: time.Hour, StaleAfter: map[string]time.Duration{"m1": time.Hour}, SubmitBatchFor: map[string]int{"m1": 100000}},
	} {
		if _, err := newEngine(newFstore(), stubMods{}, gateFn(func() bool { return true }), nil, nil, c); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	c, _ := Config{StaleAfter: map[string]time.Duration{"m1": 30 * time.Minute}, SubmitBatch: 7, SubmitBatchFor: map[string]int{"m1": 100000}}.withDefaults()
	if c.staleAfter("m1") != 30*time.Minute || c.staleAfter("m2") != 15*time.Minute || c.staleAfter("") != DefaultStaleAfter {
		t.Errorf("staleAfter: m1 %s, m2 %s", c.staleAfter("m1"), c.staleAfter("m2"))
	}
	if c.batch("m1") != 100000 || c.batch("m2") != 7 {
		t.Errorf("batch: m1 %d, m2 %d", c.batch("m1"), c.batch("m2"))
	}
}

// ---- the gate, a panic, a timeout -------------------------------------------------------------------------------------------------------------------------------------------------------

func TestTick_AStandbyDoesNothingAtAll_TheLeaderLooksAtTheWork(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
	lead := false
	r.e.gate = gateFn(func() bool { return lead })
	r.e.Tick(context.Background())
	if len(r.st.log.list()) != 0 || r.e.Stats().Standby != 1 || r.e.Stats().Ticks != 0 {
		t.Errorf("a standby made calls %v / stats %+v", r.st.log.list(), r.e.Stats())
	}
	lead = true
	r.e.Tick(context.Background())
	if r.st.log.count("EngineWork") != 1 || r.e.Stats().Ticks != 1 || r.e.Stats().Steps != 1 {
		t.Errorf("the leader's tick: %v / %+v", r.st.log.list(), r.e.Stats())
	}
}

func TestTick_APanickingGateIsNotLeading_AndAPanickingStepDoesNotStopTheNextJob(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed), ej(jobB, "m2", jobs.StatusCompleting))
	r.e.gate = gateFn(func() bool { panic("the election exploded") })
	r.e.Tick(context.Background())
	if r.st.log.count("EngineWork") != 0 || r.e.Stats().Panics != 1 || r.e.Stats().Standby != 1 {
		t.Errorf("a panicking gate: %v / %+v", r.st.log.list(), r.e.Stats())
	}
	r.e.gate = gateFn(func() bool { return true })
	r.st.panicOn = "PendingRows" // job A (confirmed) panics in its first call; job B (completing) never asks for rows and must still be looked at
	r.st.pending = rows(1)
	r.e.Tick(context.Background())
	if r.e.Stats().Panics != 2 || r.e.Stats().Steps != 2 {
		t.Errorf("a panic in one step: %+v", r.e.Stats())
	}
	if r.st.log.count("Finish") != 1 {
		t.Errorf("the second job's step did not run after the first one panicked: %v", r.st.log.list())
	}
	// the control: without the panic both steps complete and nothing is counted
	r2 := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed), ej(jobB, "m2", jobs.StatusCompleting))
	r2.e.Tick(context.Background())
	if r2.e.Stats().Panics != 0 || r2.st.log.count("Finish") != 1 {
		t.Errorf("a clean tick: %+v / %v", r2.e.Stats(), r2.st.log.list())
	}
}

func TestTick_AStepIsBoundedByItsOwnTimeout(t *testing.T) {
	r := newRig(t, Config{StepTimeout: 50 * time.Millisecond}, ej(jobA, "m2", jobs.StatusConfirmed), ej(jobB, "m2", jobs.StatusRunning))
	r.st.pending = rows(2)
	r.mod.submit = func(ctx context.Context, rows []jobs.EngineRow) (SubmitResult, error) {
		<-ctx.Done() // a module that hangs: only the step's own timeout ends the call
		return SubmitResult{}, ctx.Err()
	}
	start := time.Now()
	r.e.Tick(context.Background())
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the tick took %s: a hanging module held it", d)
	}
	if r.st.log.count("Submit") != 2 || r.e.Stats().Steps != 2 {
		t.Errorf("each job's step got its own bound and both ran: %v / %+v", r.st.log.list(), r.e.Stats())
	}
	if r.e.Stats().Unexpected != 0 {
		t.Errorf("a step cut off by its timeout was counted as unexpected: %+v", r.e.Stats())
	}
}

func TestTick_ACancelledContextStopsThePassAndWorkErrorsAreCounted(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusRunning), ej(jobB, "m2", jobs.StatusRunning))
	ctx, cancel := context.WithCancel(context.Background())
	r.mod.observe = func(ctx context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		cancel() // the process is ending during the first job's step
		return Observation{}, nil
	}
	r.e.Tick(ctx)
	if r.e.Stats().Steps != 1 {
		t.Errorf("a cancelled context did not stop the pass: %+v", r.e.Stats())
	}
	r2 := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusRunning))
	r2.st.errs["EngineWork"] = errors.New("the database is down")
	r2.e.Tick(context.Background())
	if r2.e.Stats().Unexpected != 1 || r2.e.Stats().Steps != 0 {
		t.Errorf("a failed read of the work: %+v", r2.e.Stats())
	}
}

func TestRun_TicksUntilTheContextEndsAndReturnsNil(t *testing.T) {
	r := newRig(t, Config{Tick: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.e.Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for r.st.log.count("EngineWork") < 3 {
		select {
		case <-deadline:
			t.Fatal("Run did not tick three times in ten seconds")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// ---- accounting ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestAccount_ClassifiesWhatAStepReturned(t *testing.T) {
	r := newRig(t, Config{})
	ctx := context.Background()
	for _, err := range []error{nil, Wait("busy"), fmt.Errorf("x: %w", ErrWait), context.Canceled, fmt.Errorf("y: %w", context.DeadlineExceeded), jobs.ErrNotFound, fmt.Errorf("z: %w", jobs.ErrNotAllowed), errors.New("a database error")} {
		r.e.account(ctx, jobA, "m2", err)
	}
	s := r.e.Stats()
	if s.Waits != 2 || s.Conflicts != 2 || s.Unexpected != 1 {
		t.Errorf("stats %+v: want 2 waits, 2 conflicts (not allowed, not found), 1 unexpected; a cancelled context is none of them", s)
	}
}

// ---- starting --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestStarting_SubmitsThenStartsThenStamps_InThatOrder(t *testing.T) {
	r := newRig(t, Config{SubmitBatch: 3}, ej(jobA, "m2", jobs.StatusConfirmed))
	r.st.pending = rows(5)
	r.mod.accept = 2
	r.mod.ref = "handle-1"
	r.e.Tick(context.Background())
	want := "EngineWork,PendingRows,EnsureJobItems,Submit,Start(handle-1),MarkSubmitted"
	if got := r.st.log.joined(); got != want {
		t.Errorf("calls = %s\nwant    %s (the module holds the rows, THEN the job runs, THEN the stamps: a crash between any two is repaired by offering the rows again)", got, want)
	}
	if len(r.st.pending) != 3 {
		t.Errorf("%d rows still unstamped, want 3 (only the two the module took are stamped)", len(r.st.pending))
	}
	// the control: module 1 is handed its list, not rows in public.job_items
	r1 := newRig(t, Config{}, ej(jobA, "m1", jobs.StatusConfirmed))
	r1.st.pending = rows(2)
	r1.e.Tick(context.Background())
	if r1.st.log.count("EnsureJobItems") != 0 || r1.st.log.count("Start") != 1 {
		t.Errorf("module 1 calls: %v", r1.st.log.list())
	}
}

func TestStarting_AModuleThatHoldsNothingStartsNothing(t *testing.T) {
	for name, mod := range map[string]func(*stubMod){
		"accepts none": func(m *stubMod) { m.accept = 0 },
		"cannot answer": func(m *stubMod) {
			m.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
				return SubmitResult{}, Wait("unavailable")
			}
		},
		"answers a row nobody offered": func(m *stubMod) {
			m.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
				return SubmitResult{Accepted: []string{"99999999-0000-4000-8000-000000000099"}}, nil
			}
		},
	} {
		r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
		r.st.pending = rows(3)
		mod(r.mod)
		r.e.Tick(context.Background())
		if r.st.log.count("Start") != 0 || r.st.log.count("MarkSubmitted") != 0 || len(r.st.pending) != 3 {
			t.Errorf("a module that %s: calls %v, %d rows left (nothing may be started or stamped)", name, r.st.log.list(), len(r.st.pending))
		}
	}
	// the control: the same job with a module that takes the rows is started
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
	r.st.pending = rows(3)
	r.e.Tick(context.Background())
	if r.st.log.count("Start") != 1 {
		t.Errorf("the control did not start: %v", r.st.log.list())
	}
}

func TestStarting_AnAnswerForARowNobodyOfferedIsNotStamped(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
	r.st.pending = rows(2)
	r.mod.submit = func(_ context.Context, offered []jobs.EngineRow) (SubmitResult, error) {
		return SubmitResult{Accepted: []string{offered[0].ID, offered[0].ID, "99999999-0000-4000-8000-000000000099"}}, nil
	}
	r.e.Tick(context.Background())
	if len(r.st.pending) != 1 || r.st.pending[0].ID != rows(2)[1].ID {
		t.Errorf("pending after the tick: %v; want only the first row stamped, once", r.st.pending)
	}
}

func TestStarting_PastTheDeadlineTheJobExpiresAndNothingIsSubmitted(t *testing.T) {
	j := ej(jobA, "m2", jobs.StatusConfirmed)
	j.DeadlinePassed = true
	r := newRig(t, Config{}, j)
	r.st.pending = rows(2)
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,Expire" {
		t.Errorf("calls = %s, want EngineWork,Expire", got)
	}
	// the control: the same job before its deadline submits
	r2 := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
	r2.st.pending = rows(2)
	r2.e.Tick(context.Background())
	if r2.st.log.count("Expire") != 0 || r2.st.log.count("Submit") != 1 {
		t.Errorf("before the deadline: %v", r2.st.log.list())
	}
	// a scheduled job past its deadline expires the same way; a paused one only when its deadline passed
	js := ej(jobA, "m2", jobs.StatusScheduled)
	js.DeadlinePassed = true
	jpb := ej(jobB, "m2", jobs.StatusPaused)
	jpb.PausedFrom = jobs.StatusScheduled
	rs := newRig(t, Config{}, js, jpb)
	rs.e.Tick(context.Background())
	if rs.st.log.count("Expire") != 1 {
		t.Errorf("scheduled past its deadline and paused before it: %v", rs.st.log.list())
	}
	jp := ej(jobB, "m2", jobs.StatusPaused)
	jp.PausedFrom = jobs.StatusScheduled // a job paused BEFORE its start: the one a missed deadline expires (a job paused while running never is: TestPausedRunning_*)
	jp.DeadlinePassed = true
	rp := newRig(t, Config{}, jp)
	rp.e.Tick(context.Background())
	if rp.st.log.count("Expire") != 1 {
		t.Errorf("a paused job past its deadline was not expired: %v", rp.st.log.list())
	}
}

func TestStarting_AModuleThatCannotAnswerLeavesTheJobAndIsCounted(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusConfirmed))
	r.st.pending = rows(2)
	r.mod.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
		return SubmitResult{}, Wait("a campaign is active")
	}
	r.e.Tick(context.Background())
	if r.e.Stats().Waits != 1 || r.e.Stats().Unexpected != 0 || r.st.log.count("Start") != 0 || r.st.log.count("FailJob") != 0 || r.st.log.count("Expire") != 0 {
		t.Errorf("a wait: %v / %+v", r.st.log.list(), r.e.Stats())
	}
	// the module lookup itself may wait (module 1's caller key is not provisioned yet)
	r2 := newRig(t, Config{}, ej(jobA, "m1", jobs.StatusConfirmed))
	r2.e.mods = stubMods{err: Wait("no caller key yet")}
	r2.e.Tick(context.Background())
	if r2.e.Stats().Waits != 1 || r2.st.log.count("PendingRows") != 0 {
		t.Errorf("a module that cannot be found: %v / %+v", r2.st.log.list(), r2.e.Stats())
	}
}

func TestStarting_AModuleThatRefusesForGoodFailsTheJobWhileItHoldsNoRowOfIt(t *testing.T) {
	for _, status := range []string{jobs.StatusConfirmed, jobs.StatusScheduled} {
		r := newRig(t, Config{}, ej(jobA, "m2", status))
		r.st.pending = rows(2)
		r.mod.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
			return SubmitResult{}, Refused("the subject is not accepted")
		}
		r.e.Tick(context.Background())
		reason := r.st.errs["FailJob.reason"]
		if r.st.log.count("FailJob") != 1 || reason == nil || !strings.Contains(reason.Error(), "refused the job for good") || !strings.Contains(reason.Error(), "the subject is not accepted") {
			t.Errorf("%s: calls %v, reason %v", status, r.st.log.list(), reason)
		}
		if r.e.Stats().Refused != 1 || r.e.Stats().Unexpected != 0 {
			t.Errorf("%s: %+v", status, r.e.Stats())
		}
		if r.st.log.count("Flag") != 0 {
			t.Errorf("%s: a job that was failed is also flagged: %v", status, r.st.log.list())
		}
	}
}

// ---- a refusal for good: the job is failed only while the module holds no row of it ---------------------------------------------------------------------------------------------------

// refusalText is a module's reason for refusing, written the way a module could: with an address in it. It must reach no job event.
const refusalText = "the row for jane.doe@leak-check.example is not accepted"

func refuseWith(code string) func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
	return func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
		return SubmitResult{}, RefusedWithCode(code, refusalText)
	}
}

func TestHoldsRows_IsWhatTheEngineKnowsOfWhatTheModuleHolds(t *testing.T) {
	at := func(status string, submitted int) jobs.EngineJob {
		j := ej(jobA, "m2", status)
		j.Submitted = submitted
		return j
	}
	for _, c := range []struct {
		name    string
		job     jobs.EngineJob
		stamped int
		want    bool
	}{
		{"a confirmed job nothing of which was handed over", at(jobs.StatusConfirmed, 0), 0, false},
		{"a scheduled job nothing of which was handed over", at(jobs.StatusScheduled, 0), 0, false},
		{"rows stamped before the step", at(jobs.StatusConfirmed, 3), 0, true},
		{"rows stamped in this very step", at(jobs.StatusConfirmed, 0), 2, true},
		{"both", at(jobs.StatusConfirmed, 3), 2, true},
		{"a running job: the move to running follows the module taking a row, whatever the stamps say (a crash between the two)", at(jobs.StatusRunning, 0), 0, true},
	} {
		if got := holdsRows(c.job, c.stamped); got != c.want {
			t.Errorf("%s: holdsRows = %v, want %v", c.name, got, c.want)
		}
	}
}

// A refusal arrives after rows of the job were handed to the module: failing the job would write infrastructure_failure for every row without a result while the module keeps sending the queued ones.
// The engine does not fail it, flags it, waits, and goes on to read what the module reports. Each case has its control: the same refusal with nothing held fails the job.
func TestRefusal_WhileTheModuleHoldsRowsTheJobIsNotFailedItIsFlaggedAndWaitsAndTheModuleIsStillObserved(t *testing.T) {
	running := func(submitted int) jobs.EngineJob {
		j := ej(jobA, "m2", jobs.StatusRunning)
		j.Submitted, j.Pending = submitted, 6
		return j
	}
	for name, c := range map[string]struct {
		job    jobs.EngineJob
		accept int // how many Submit calls the module accepts before it starts to refuse
	}{
		"rows stamped before the step":                                       {running(3), 0},
		"a running job with no stamp at all (a crash after the move)":        {running(0), 0},
		"the first batch of the step accepted and stamped, the next refused": {running(0), 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, c.job)
			r.st.pending = rows(6)
			calls := 0
			r.mod.submit = func(ctx context.Context, offered []jobs.EngineRow) (SubmitResult, error) {
				calls++
				if calls <= c.accept {
					res := SubmitResult{}
					for _, o := range offered {
						res.Accepted = append(res.Accepted, o.ID)
					}
					return res, nil
				}
				return SubmitResult{}, RefusedWithCode("validation_failed", refusalText)
			}
			r.e.Tick(context.Background())
			s := r.e.Stats()
			if r.st.log.count("FailJob") != 0 || s.Refused != 0 {
				t.Fatalf("a job whose rows the module holds was failed: %v / %+v", r.st.log.list(), s)
			}
			if s.Waits != 1 || s.Unexpected != 0 || r.st.log.count("Flag") != 1 {
				t.Errorf("the refusal was not a wait with ONE flag: %v / %+v", r.st.log.list(), s)
			}
			if r.st.log.count("Observe") != 1 {
				t.Errorf("the module was not observed after the refusal (it goes on with the rows it holds, and their results must be recorded): %v", r.st.log.list())
			}
			if c.accept > 0 && r.st.log.count("MarkSubmitted") != c.accept {
				t.Errorf("the rows the module accepted before it refused were not stamped: %v", r.st.log.list())
			}
			if len(r.st.flags) != 1 {
				t.Fatalf("%d flag details", len(r.st.flags))
			}
			d := r.st.flags[0]
			if d["refusal"] != "validation_failed" || d["module"] != "m2" {
				t.Errorf("the flag names %v / %v, want the module's code word and its id", d["refusal"], d["module"])
			}
			for k, v := range d {
				if strings.Contains(fmt.Sprint(v), "jane.doe") || strings.Contains(fmt.Sprint(v), "leak-check") || strings.Contains(fmt.Sprint(v), "not accepted") {
					t.Errorf("the flag's %q carries the module's text: %v", k, v)
				}
			}
			// the control: the same refusal, with nothing held, fails the job
			ctl := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, ej(jobA, "m2", jobs.StatusConfirmed))
			ctl.st.pending = rows(6)
			ctl.mod.submit = refuseWith("validation_failed")
			ctl.e.Tick(context.Background())
			if ctl.st.log.count("FailJob") != 1 || ctl.st.log.count("Flag") != 0 || ctl.e.Stats().Refused != 1 {
				t.Errorf("the control (nothing held) did not fail the job: %v / %+v", ctl.st.log.list(), ctl.e.Stats())
			}
		})
	}
}

func TestRefusal_TheFlagIsRaisedOncePerStalenessIntervalAndAFailedWriteIsTriedAgain(t *testing.T) {
	j := ej(jobA, "m2", jobs.StatusRunning)
	j.Submitted, j.Pending = 2, 4
	r := newRig(t, Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}}, j)
	r.st.pending = rows(2)
	r.mod.submit = refuseWith("not_found")
	for i := 0; i < 4; i++ {
		r.e.Tick(context.Background())
	}
	if r.st.log.count("Flag") != 1 || r.st.log.count("FailJob") != 0 {
		t.Fatalf("four ticks inside the interval: %v (want one flag and no failure)", r.st.log.list())
	}
	*r.now = r.now.Add(61 * time.Minute)
	j.LastProgressAt = timePtr(r.now.Add(-2 * time.Hour))
	r.st.work = []jobs.EngineJob{j}
	r.e.Tick(context.Background())
	if r.st.log.count("Flag") != 2 || r.st.log.count("FailJob") != 0 {
		t.Errorf("after the staleness interval: %v (want a second flag, still no failure)", r.st.log.list())
	}
	// the refusal never keeps the job from going stale: it made no progress, and the stale event says so
	if r.st.log.count("AlertStale(1h0m0s)") == 0 {
		t.Errorf("the job that cannot go on never raised the stale event: %v", r.st.log.list())
	}
	// a flag that could not be written is tried again at the next tick, and is no reason to fail the job
	f := newRig(t, Config{SettleEvery: -1}, j)
	f.st.pending = rows(2)
	f.mod.submit = refuseWith("not_found")
	f.st.errs["Flag"] = errors.New("the database is down")
	f.e.Tick(context.Background())
	delete(f.st.errs, "Flag")
	f.e.Tick(context.Background())
	if f.st.log.count("Flag") != 2 || len(f.st.flags) != 1 || f.st.log.count("FailJob") != 0 {
		t.Errorf("a failed flag write: %v / %d written (want two attempts, one written, no failure)", f.st.log.list(), len(f.st.flags))
	}
	// the memory of a job that is no longer work (it finished, or is only waiting out a backoff: D8) is kept while it could still hold a flag back, and dropped once it is older than every staleness limit
	f.st.work = nil
	f.e.Tick(context.Background())
	if len(f.e.refusals) != 1 {
		t.Errorf("the refusal flag of a job that left the work was forgotten at once, inside the staleness interval: %v", f.e.refusals)
	}
	*f.now = f.now.Add(DefaultStaleAfter + time.Minute)
	f.e.Tick(context.Background())
	if len(f.e.refusals) != 0 {
		t.Errorf("the refusals of a finished job were kept past every staleness limit: %v", f.e.refusals)
	}
}

func TestRefusalCode_IsAWordTheModuleGaveAndNeverItsText(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a code word", RefusedWithCode("validation_failed", "x"), "validation_failed"},
		{"upper case and padded", RefusedWithCode("  Not_Found ", "x"), "not_found"},
		{"wrapped", fmt.Errorf("the step: %w", RefusedWithCode("not_found", "x")), "not_found"},
		{"no code", Refused("a reason"), "unspecified"},
		{"an address where a word belongs", RefusedWithCode("jane.doe@leak-check.example", "x"), "unspecified"},
		{"a sentence", RefusedWithCode("the row is bad", "x"), "unspecified"},
		{"too long", RefusedWithCode(strings.Repeat("a", 41), "x"), "unspecified"},
		{"not a refusal at all", errors.New("boom"), "unspecified"},
	} {
		if got := RefusalCode(c.err); got != c.want {
			t.Errorf("%s: RefusalCode = %q, want %q", c.name, got, c.want)
		}
	}
	// the typed refusal is an ErrRefused with the text Refused always had
	e := RefusedWithCode("validation_failed", "the job is bad")
	if !errors.Is(e, ErrRefused) || errors.Is(e, ErrWait) || e.Error() != ErrRefused.Error()+": the job is bad" || Refused("the job is bad").Error() != e.Error() {
		t.Errorf("RefusedWithCode = %q, want an ErrRefused with the text of Refused", e.Error())
	}
}

// ---- the job as the store read it ---------------------------------------------------------------------------------------------------------------------------------------------------

// The module's own handle of a job (module 1's job id) is on the job the store reads, and it is that job, unchanged, which the engine hands to Observe: the engine reads no reference of its own and
// invents none. A job that started in a step is not observed in the same step (the handle reaches Observe on the next tick's read of the job, where Start's write is).
func TestRunning_TheJobHandedToObserveIsTheJobAsTheStoreReadItModuleReferenceIncluded(t *testing.T) {
	j := ej(jobA, "m1", jobs.StatusRunning)
	j.ModuleRef = "module-one-id"
	r := newRig(t, Config{SettleEvery: -1}, j)
	r.e.Tick(context.Background())
	if len(r.mod.observed) != 1 || r.mod.observed[0].ModuleRef != "module-one-id" || r.mod.observed[0].ID != jobA {
		t.Fatalf("Observe was handed %+v, want the job with the reference the store read", r.mod.observed)
	}
	// the control: a job the store read with no reference reaches Observe with none (nothing is filled in from anywhere else), and the engine read nothing but the work
	n := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m1", jobs.StatusRunning))
	n.e.Tick(context.Background())
	if len(n.mod.observed) != 1 || n.mod.observed[0].ModuleRef != "" || n.st.log.count("EngineJobByID") != 0 {
		t.Errorf("a job with no reference: Observe was handed %+v after %v", n.mod.observed, n.st.log.list())
	}
	// the same for the steps of a cancelling and a completing job
	for _, status := range []string{jobs.StatusCancelling, jobs.StatusCompleting} {
		jj := ej(jobA, "m2", status)
		jj.ModuleRef = "a-handle"
		c := newRig(t, Config{}, jj)
		c.e.Tick(context.Background())
		if len(c.mod.observed) != 1 || c.mod.observed[0].ModuleRef != "a-handle" {
			t.Errorf("%s: Observe was handed %+v", status, c.mod.observed)
		}
	}
}

func TestStarting_AJobThatStartsIsNotObservedInThatStep_TheReferenceReachesObserveWithTheNextRead(t *testing.T) {
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m1", jobs.StatusConfirmed))
	r.st.pending = rows(2)
	r.mod.ref = "handle-1"
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,PendingRows,Submit,Start(handle-1),MarkSubmitted" {
		t.Fatalf("calls = %s: the job that starts is not observed in the same step, and nothing is read between the submit and the stamps", got)
	}
	if len(r.mod.observed) != 0 {
		t.Fatalf("Observe was called in the step that started the job: %+v", r.mod.observed)
	}
	// the next tick reads the job as the store now has it: running, with the reference Start recorded; that is what the module is handed
	started := ej(jobA, "m1", jobs.StatusRunning)
	started.ModuleRef = "handle-1"
	r.st.work = []jobs.EngineJob{started}
	r.e.Tick(context.Background())
	if len(r.mod.observed) != 1 || r.mod.observed[0].ModuleRef != "handle-1" {
		t.Errorf("the next tick's Observe was handed %+v, want the reference Start recorded", r.mod.observed)
	}
}

// ---- running ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestRunning_SubmitsTheRestInBatchesAndStopsWhenTheModuleIsFull(t *testing.T) {
	r := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.st.pending = rows(5)
	r.e.Tick(context.Background())
	if r.st.log.count("Submit") != 3 || len(r.st.pending) != 0 || r.st.log.count("Start") != 0 {
		t.Errorf("a module that takes everything: %v, %d left (3 batches of 2, 2, 1; a running job is not started again)", r.st.log.list(), len(r.st.pending))
	}
	// the control: a module that takes one row of two is full; the engine stops and asks again at the next tick
	f := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	f.st.pending = rows(5)
	f.mod.accept = 1
	f.e.Tick(context.Background())
	if f.st.log.count("Submit") != 1 || len(f.st.pending) != 4 {
		t.Errorf("a full module: %v, %d left; want one submit, one row stamped", f.st.log.list(), len(f.st.pending))
	}
	f.mod.accept = -1
	f.e.Tick(context.Background())
	if len(f.st.pending) != 0 {
		t.Errorf("the next tick did not finish the submitting: %d left", len(f.st.pending))
	}
}

func TestRunning_AStampThatDoesNotTakeNeverMakesTheEngineSpin(t *testing.T) {
	r := newRig(t, Config{SubmitBatch: 2, SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.st.pending = rows(4)
	r.st.errs["MarkSubmitted"] = nil
	// a store that stamps nothing: PendingRows keeps answering the same rows
	r.e.st = &noStampStore{fstore: r.st}
	done := make(chan struct{})
	go func() { r.e.Tick(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine spins on rows whose stamp does not take")
	}
	if r.st.log.count("Submit") > 2 {
		t.Errorf("%d submits for rows that stay pending", r.st.log.count("Submit"))
	}
}

type noStampStore struct{ *fstore }

func (s *noStampStore) MarkSubmitted(ctx context.Context, id string, ids []string) error {
	s.log.add("MarkSubmitted")
	return nil
}

func TestRunning_ASubmitThatCannotAnswerStillObserves(t *testing.T) {
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.st.pending = rows(2)
	r.mod.submit = func(context.Context, []jobs.EngineRow) (SubmitResult, error) {
		return SubmitResult{}, Wait("a campaign is active")
	}
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != 1 || r.e.Stats().Waits != 1 {
		t.Errorf("a module that refuses new rows still reports the old ones: %v / %+v", r.st.log.list(), r.e.Stats())
	}
}

func TestRunning_ObserveIsThrottledByTheEnginesClockUnlessThereIsSomethingToRecord(t *testing.T) {
	j := ej(jobA, "m2", jobs.StatusRunning)
	r := newRig(t, Config{SettleEvery: 10 * time.Second}, j)
	r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Total: 4, Terminal: 1}, nil
	}
	r.e.Tick(context.Background()) // never observed: observe
	r.e.Tick(context.Background()) // the module showed 1 terminal item and 0 are recorded: observe again, without waiting
	if r.st.log.count("Observe") != 2 {
		t.Fatalf("observations = %d, want 2 (an unrecorded terminal item is not waited for)", r.st.log.count("Observe"))
	}
	r.st.work[0].PendingResults = 1 // now recorded
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != 2 {
		t.Errorf("observed again at once with nothing new and SettleEvery not elapsed: %d observations", r.st.log.count("Observe"))
	}
	*r.now = r.now.Add(11 * time.Second)
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != 3 {
		t.Errorf("not observed after SettleEvery: %d observations", r.st.log.count("Observe"))
	}
	// a negative SettleEvery observes at every tick
	n := newRig(t, Config{SettleEvery: -1}, j)
	n.mod.observe = r.mod.observe
	n.st.work[0].PendingResults = 1
	for i := 0; i < 3; i++ {
		n.e.Tick(context.Background())
	}
	if n.st.log.count("Observe") != 3 {
		t.Errorf("a negative SettleEvery observed %d times in 3 ticks", n.st.log.count("Observe"))
	}
}

func TestRunning_RecordsEveryPageAndNotesProgress_OnlyWhenSomethingWasNew(t *testing.T) {
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	r.mod.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		for i := 0; i < 3; i++ {
			if err := fn([]jobs.ResultItem{{ItemRef: rows(3)[i].ID}}); err != nil {
				return Observation{}, err
			}
		}
		return Observation{Total: 6, Terminal: 3}, nil
	}
	r.e.Tick(context.Background())
	if r.st.log.count("RecordResults") != 3 || r.st.log.count("NoteProgress") != 1 {
		t.Errorf("three pages: %v", r.st.log.list())
	}
	// an observation with nothing new notes no progress
	q := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	q.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Total: 2}, nil
	}
	q.e.Tick(context.Background())
	if q.st.log.count("NoteProgress") != 0 {
		t.Errorf("progress was noted for nothing: %v", q.st.log.list())
	}
	// a page the store refuses stops the walk and is the step's error (what was recorded stays recorded)
	f := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning))
	f.st.errs["RecordResults"] = errors.New("the database refused")
	f.mod.observe = r.mod.observe
	f.e.Tick(context.Background())
	if f.st.log.count("RecordResults") != 1 || f.e.Stats().Unexpected != 1 {
		t.Errorf("a failing page: %v / %+v", f.st.log.list(), f.e.Stats())
	}
}

// completeTick runs one tick of a running job whose module says complete and whose fresh counts are the given ones.
func completeTick(t *testing.T, complete bool, fresh ...jobs.EngineJob) *rig {
	t.Helper()
	r := newRig(t, Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}}, ej(jobA, "m2", jobs.StatusRunning))
	r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Complete: complete, Total: 3, Terminal: 3}, nil
	}
	r.st.fresh = fresh
	r.e.Tick(context.Background())
	return r
}

func freshJob(pending, pendingResults, unsubmitted int) jobs.EngineJob {
	j := ej(jobA, "m2", jobs.StatusRunning)
	j.Pending, j.PendingResults, j.Unsubmitted = pending, pendingResults, unsubmitted
	return j
}

func TestRunning_CompletesAndFinishesInOneStepOnlyWhenTheModuleSaysCompleteAndEveryRowHasAResult(t *testing.T) {
	r := completeTick(t, true, freshJob(3, 3, 0))
	if !strings.HasSuffix(r.st.log.joined(), "EngineJobByID,Complete,Finish") {
		t.Errorf("a finished job: %s", r.st.log.joined())
	}
	// the controls, one property at a time
	for name, c := range map[string]struct {
		complete bool
		fresh    jobs.EngineJob
		flag     int
	}{
		"the module does not say complete":                       {false, freshJob(3, 3, 0), 0},
		"complete, rows still to be submitted (still running)":   {true, freshJob(5, 3, 2), 0},
		"complete but a submitted row has no recorded result":    {true, freshJob(3, 2, 0), 1},
		"complete, one unsubmitted AND one submitted unrecorded": {true, freshJob(4, 2, 1), 0},
	} {
		r := completeTick(t, c.complete, c.fresh)
		if r.st.log.count("Complete") != 0 || r.st.log.count("Finish") != 0 {
			t.Errorf("%s: the job was completed: %s", name, r.st.log.joined())
		}
		if r.st.log.count("Flag") != c.flag {
			t.Errorf("%s: %d flags, want %d (%s)", name, r.st.log.count("Flag"), c.flag, r.st.log.joined())
		}
	}
}

func TestRunning_AModuleCompleteWithMissingResultsIsFlaggedOncePerStalenessInterval_ThenRaisesTheStaleEvent(t *testing.T) {
	j := ej(jobA, "m2", jobs.StatusRunning)
	j.StartedAt, j.LastProgressAt = timePtr(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)), timePtr(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	r := newRig(t, Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": time.Hour}}, j)
	r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Complete: true, Total: 3, Terminal: 3}, nil
	}
	r.st.fresh = []jobs.EngineJob{freshJob(3, 2, 0)}
	for i := 0; i < 3; i++ {
		r.e.Tick(context.Background())
	}
	if r.st.log.count("Flag") != 1 || r.st.log.count("Complete") != 0 || r.st.log.count("AlertStale") != 0 {
		t.Errorf("three ticks inside the interval: %v (want one flag, no completion, no stale event yet)", r.st.log.list())
	}
	*r.now = r.now.Add(61 * time.Minute)
	r.e.Tick(context.Background())
	if r.st.log.count("Flag") != 2 || r.st.log.count("AlertStale") != 1 || r.st.log.count("Complete") != 0 {
		t.Errorf("after the staleness limit: %v (want a second flag, one stale event, still no completion)", r.st.log.list())
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestStale_IsRaisedOnlyForAJobWithNoProgressOlderThanItsModulesLimit(t *testing.T) {
	last := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	mk := func(module string, progress *time.Time) jobs.EngineJob {
		j := ej(jobA, module, jobs.StatusRunning)
		j.LastProgressAt = progress
		return j
	}
	cfg := Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m1": 30 * time.Minute, "m2": 15 * time.Minute}}
	for _, c := range []struct {
		name  string
		job   jobs.EngineJob
		later time.Duration
		want  string // the AlertStale call, "" for none
	}{
		{"fresh progress", mk("m2", &last), 5 * time.Minute, ""},
		{"m2 past its 15 minutes", mk("m2", &last), 16 * time.Minute, "AlertStale(15m0s)"},
		{"m1 at 16 minutes is not stale yet (its limit is 30)", mk("m1", &last), 16 * time.Minute, ""},
		{"m1 past its 30 minutes", mk("m1", &last), 31 * time.Minute, "AlertStale(30m0s)"},
		{"a module with no dial gets 15 minutes", mk("m3", &last), 16 * time.Minute, "AlertStale(15m0s)"},
		{"no progress ever recorded: the start counts", func() jobs.EngineJob { j := mk("m2", nil); j.StartedAt = &last; return j }(), 16 * time.Minute, "AlertStale(15m0s)"},
	} {
		r := newRig(t, cfg, c.job)
		*r.now = last.Add(c.later)
		r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
			return Observation{Total: 2}, nil
		}
		r.e.Tick(context.Background())
		got := ""
		for _, call := range r.st.log.list() {
			if strings.HasPrefix(call, "AlertStale") {
				got = call
			}
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q (calls %v)", c.name, got, c.want, r.st.log.list())
		}
		if r.st.log.count("Complete")+r.st.log.count("FailJob")+r.st.log.count("Expire")+r.st.log.count("Finish") != 0 {
			t.Errorf("%s: a stale job was moved: %v (staleness is a report, never a move)", c.name, r.st.log.list())
		}
	}
	// a job that made progress this very tick is not stale whatever the clock says
	p := newRig(t, cfg, mk("m2", &last))
	*p.now = last.Add(5 * time.Hour)
	p.mod.observe = func(_ context.Context, fn func([]jobs.ResultItem) error) (Observation, error) {
		fn([]jobs.ResultItem{{ItemRef: rows(1)[0].ID}})
		return Observation{Total: 2, Terminal: 1}, nil
	}
	p.e.Tick(context.Background())
	if p.st.log.count("AlertStale") != 0 {
		t.Errorf("a job that progressed was called stale: %v", p.st.log.list())
	}
}

// ---- cancelling, completing, repair ------------------------------------------------------------------------------------------------------------------------------------------------------

func TestCancelling_AsksTheModuleAtEveryTickAndEndsTheRowsNoModuleHeldBeforeItCompletes(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCancelling))
	r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Complete: false, Total: 2, Terminal: 1}, nil
	}
	r.e.Tick(context.Background())
	r.e.Tick(context.Background())
	if r.st.log.count("Cancel") != 2 || r.st.log.count("Complete") != 0 || r.st.log.count("CancelUnsubmitted") != 0 {
		t.Errorf("not complete yet: %v", r.st.log.list())
	}
	// complete: the rows never given to a module are ended as the platform's own cancelled results, and only then is the job completed
	c := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCancelling))
	c.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Complete: true, Total: 2, Terminal: 2}, nil
	}
	c.st.fresh = []jobs.EngineJob{freshJob(5, 2, 3), freshJob(5, 5, 0)}
	c.e.Tick(context.Background())
	if got := c.st.log.joined(); !strings.HasSuffix(got, "Cancel,Observe,EngineJobByID,CancelUnsubmitted,EngineJobByID,Complete,Finish") {
		t.Errorf("calls = %s", got)
	}
	// the control: a module that is complete with a SUBMITTED row unrecorded is not completed even in a cancel
	h := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCancelling))
	h.mod.observe = c.mod.observe
	h.st.fresh = []jobs.EngineJob{freshJob(5, 3, 0)}
	h.e.Tick(context.Background())
	if h.st.log.count("Complete") != 0 || h.st.log.count("Flag") != 1 {
		t.Errorf("a cancel completed over a missing result: %v", h.st.log.list())
	}
}

// Module 1 CAN cancel since the close-out (POST /v1/jobs/{id}/cancel: TestCancelling_Module1IsAskedToCancelLikeTheSenders), so no seeded module answers ErrNotSupported to a cancel; the engine's branch
// stays for a module whose capability row was flipped or whose route is older than its seed, which this test drives with a stub on module 2.
func TestCancelling_AModuleThatCannotCancelIsLoggedAndObservedNotInvented(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCancelling))
	r.mod.cancel = func() error { return ErrNotSupported }
	r.e.Tick(context.Background())
	if r.st.log.count("Observe") != 1 || r.st.log.count("Complete") != 0 || r.st.log.count("FailJob") != 0 {
		t.Errorf("calls %v", r.st.log.list())
	}
}

func TestCompleting_AFinalPassThenFinish_AndAMissingResultIsFlaggedNotInvented(t *testing.T) {
	r := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCompleting))
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork,Observe,Finish" {
		t.Errorf("calls = %s, want EngineWork,Observe,Finish", got)
	}
	// Finish refuses (a row has no result): flagged, never forced; the module being down does not keep Finish from being asked
	f := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusCompleting))
	f.st.errs["Finish"] = &jobs.IncompleteError{Missing: 2}
	f.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{}, Wait("down")
	}
	f.st.fresh = []jobs.EngineJob{freshJob(3, 1, 0)}
	f.e.Tick(context.Background())
	if f.st.log.count("Finish") != 1 || f.st.log.count("Flag") != 1 || f.st.log.count("FailJob") != 0 {
		t.Errorf("an incomplete completing job: %v", f.st.log.list())
	}
}

func TestRepair_ReleasesATerminalJobAndLeavesEveryOtherStatusAlone(t *testing.T) {
	for _, status := range []string{jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusExpired} {
		j := ej(jobA, "m2", status)
		j.ReserveRemainder = 7
		r := newRig(t, Config{}, j)
		r.e.Tick(context.Background())
		if got := r.st.log.joined(); got != "EngineWork,Release" {
			t.Errorf("%s: calls = %s", status, got)
		}
	}
	// a paused job inside its deadline, and a status the engine has no step for, do nothing / are reported
	pj := ej(jobA, "m2", jobs.StatusPaused)
	pj.PausedFrom = jobs.StatusScheduled // paused before its start: nothing to look at (a job paused while running is looked at: TestPausedRunning_*)
	r := newRig(t, Config{}, pj)
	r.e.Tick(context.Background())
	if got := r.st.log.joined(); got != "EngineWork" {
		t.Errorf("a paused job: %s", got)
	}
	w := newRig(t, Config{}, ej(jobA, "m2", jobs.StatusDraft))
	w.e.Tick(context.Background())
	if w.e.Stats().Unexpected != 1 {
		t.Errorf("a draft in the work was not reported: %+v", w.e.Stats())
	}
}

func TestPrune_ForgetsTheJobsThatAreNoLongerWork(t *testing.T) {
	r := newRig(t, Config{SettleEvery: -1}, ej(jobA, "m2", jobs.StatusRunning), ej(jobB, "m2", jobs.StatusRunning))
	r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
		return Observation{Total: 1}, nil
	}
	r.e.Tick(context.Background())
	if len(r.e.lastObs) != 2 {
		t.Fatalf("lastObs has %d entries after observing two jobs", len(r.e.lastObs))
	}
	r.st.work = r.st.work[:1]
	r.e.Tick(context.Background())
	if _, gone := r.e.lastObs[jobB]; gone || len(r.e.lastObs) != 1 {
		t.Errorf("the finished job's memory was kept: %v", r.e.lastObs)
	}
}

func TestShouldObserve_Table(t *testing.T) {
	r := newRig(t, Config{SettleEvery: time.Minute})
	j := ej(jobA, "m2", jobs.StatusRunning)
	j.PendingResults = 2
	set := func(o observed) {
		r.e.mu.Lock()
		r.e.lastObs[jobA] = o
		r.e.mu.Unlock()
	}
	now := *r.now
	for _, c := range []struct {
		name string
		o    *observed
		want bool
	}{
		{"never observed", nil, true},
		{"observed a second ago, nothing new", &observed{at: now.Add(-time.Second), terminal: 2, status: jobs.StatusRunning}, false},
		{"observed a second ago, the module showed one more terminal item", &observed{at: now.Add(-time.Second), terminal: 3, status: jobs.StatusRunning}, true},
		{"observed a second ago, the module said complete (the completion is retried)", &observed{at: now.Add(-time.Second), terminal: 2, complete: true, status: jobs.StatusRunning}, true},
		{"observed a minute ago", &observed{at: now.Add(-time.Minute), terminal: 2, status: jobs.StatusRunning}, true},
		{"observed a second ago, nothing new, but the module said it HOLDS the campaign (the engine must see the hold end at once)", &observed{at: now.Add(-time.Second), terminal: 2, paused: true, status: jobs.StatusRunning}, true},
		{"observed a second ago, nothing new, in ANOTHER status (the job was resumed or moved since)", &observed{at: now.Add(-time.Second), terminal: 2, status: jobs.StatusPaused}, true},
	} {
		r.e.mu.Lock()
		delete(r.e.lastObs, jobA)
		r.e.mu.Unlock()
		if c.o != nil {
			set(*c.o)
		}
		if got := r.e.shouldObserve(j); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCutReason(t *testing.T) {
	if got := cutReason("short"); got != "short" {
		t.Errorf("cutReason = %q", got)
	}
	if got := cutReason(strings.Repeat("a", 1000)); len(got) != 300 {
		t.Errorf("cutReason kept %d bytes, want 300", len(got))
	}
}

// The staleness of a job is the database's clock against the database's last_progress_at: a host whose own clock is wrong neither hides a stale job nor raises a false alarm.
func TestStale_IsJudgedByTheDatabasesClockNotTheHosts(t *testing.T) {
	last := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{SettleEvery: -1, StaleAfter: map[string]time.Duration{"m2": 15 * time.Minute}}
	for _, c := range []struct {
		name    string
		host    time.Time // the engine's own clock
		dbNow   time.Time // the database's clock when the job was read
		wantAlt bool
	}{
		{"the host is hours behind and the job is stale by the database's clock", last.Add(-3 * time.Hour), last.Add(16 * time.Minute), true},
		{"the host is hours ahead and the job is fresh by the database's clock", last.Add(5 * time.Hour), last.Add(5 * time.Minute), false},
		{"both agree and the job is stale (the control: the alert exists at all)", last.Add(16 * time.Minute), last.Add(16 * time.Minute), true},
	} {
		j := ej(jobA, "m2", jobs.StatusRunning)
		j.LastProgressAt = &last
		j.DBNow = c.dbNow
		r := newRig(t, cfg, j)
		*r.now = c.host
		r.mod.observe = func(context.Context, func([]jobs.ResultItem) error) (Observation, error) {
			return Observation{Total: 2}, nil
		}
		r.e.Tick(context.Background())
		if got := r.st.log.count("AlertStale") > 0; got != c.wantAlt {
			t.Errorf("%s: alert raised %v, want %v (calls %v)", c.name, got, c.wantAlt, r.st.log.list())
		}
	}
}
