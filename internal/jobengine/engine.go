package jobengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coldmail-platform/platform/internal/faultpoint"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Defaults of the dials (Docs/phase-4-spec.md, open decision 8; each is a PLATFORM_* dial wired by internal/app, a zero Config field means the default).
const (
	DefaultTick        = 1500 * time.Millisecond
	DefaultSettleEvery = 10 * time.Second
	DefaultStaleAfter  = 15 * time.Minute
	DefaultSubmitBatch = 500
	DefaultWorkLimit   = 100
	DefaultStepTimeout = 2 * time.Minute
	// DefaultBackoffCap is the longest the engine leaves a job alone after a step that made no progress (Phase 4 close-out D8); MaxBackoffCap is the most the dial may say and the wall the database keeps
	// (platform.jobs_engine_schedule). Whatever the dial says, the engine never waits longer than a quarter of the module's staleness limit (Config.backoff).
	DefaultBackoffCap = 60 * time.Second
	MaxBackoffCap     = jobs.MaxPollBackoff
)

// Config is what the engine needs. A zero field means its default; a negative one is refused by New, except SettleEvery, where a negative value means "observe a running job at every tick"
// (for the tests: a zero must not read as "never throttle" when the injected clock does not move).
type Config struct {
	Tick        time.Duration // the wait between two ticks of Run
	SettleEvery time.Duration // the least wait between two observations of a RUNNING job, unless the last one showed terminal items not yet recorded or said the job was complete
	// StaleAfter is, per module id, how long a running or cancelling job may make no progress before the engine raises a `stale` event for it (the event repeats at most once per that long, and
	// the job stays as it is). A module that is not in the map gets DefaultStaleAfter.
	StaleAfter map[string]time.Duration
	// SubmitBatch is how many rows one Module.Submit call is offered; SubmitBatchFor overrides it per module id (module 1 takes a whole list in one call).
	SubmitBatch    int
	SubmitBatchFor map[string]int
	WorkLimit      int           // how many jobs one tick looks at
	StepTimeout    time.Duration // the bound of ONE job's step
	// BackoffCap is the longest the engine leaves a job alone after a step that made no progress: the wait starts at one Tick and doubles with every such step in a row up to this (and never past a
	// quarter of the module's staleness limit, so a stale job is still noticed on time and a module that recovers is polled again within it). Zero means DefaultBackoffCap; more than MaxBackoffCap is
	// refused; a negative value switches the backoff off (every idle job is due again at the next tick: for the tests of the engine that tick back to back).
	BackoffCap time.Duration
}

func (c Config) withDefaults() (Config, error) {
	for name, d := range map[string]time.Duration{"Tick": c.Tick, "StepTimeout": c.StepTimeout} {
		if d < 0 {
			return Config{}, fmt.Errorf("jobengine: Config.%s must not be negative, got %s", name, d)
		}
	}
	if c.BackoffCap > MaxBackoffCap {
		return Config{}, fmt.Errorf("jobengine: Config.BackoffCap must not be more than %s, got %s", MaxBackoffCap, c.BackoffCap)
	}
	for name, n := range map[string]int{"SubmitBatch": c.SubmitBatch, "WorkLimit": c.WorkLimit} {
		if n < 0 {
			return Config{}, fmt.Errorf("jobengine: Config.%s must not be negative, got %d", name, n)
		}
	}
	for m, d := range c.StaleAfter {
		if d <= 0 {
			return Config{}, fmt.Errorf("jobengine: Config.StaleAfter[%q] must be positive, got %s", m, d)
		}
	}
	for m, n := range c.SubmitBatchFor {
		if n < 1 {
			return Config{}, fmt.Errorf("jobengine: Config.SubmitBatchFor[%q] must be positive, got %d", m, n)
		}
	}
	if c.Tick == 0 {
		c.Tick = DefaultTick
	}
	if c.SettleEvery == 0 {
		c.SettleEvery = DefaultSettleEvery
	}
	if c.SubmitBatch == 0 {
		c.SubmitBatch = DefaultSubmitBatch
	}
	if c.WorkLimit == 0 {
		c.WorkLimit = DefaultWorkLimit
	}
	if c.StepTimeout == 0 {
		c.StepTimeout = DefaultStepTimeout
	}
	if c.BackoffCap == 0 {
		c.BackoffCap = DefaultBackoffCap
	}
	return c, nil
}

// staleAfter is the staleness limit of a module.
func (c Config) staleAfter(module string) time.Duration {
	if d, ok := c.StaleAfter[module]; ok {
		return d
	}
	return DefaultStaleAfter
}

// longestStale is the longest staleness limit of any module: a memory of the engine older than this can no longer hold anything back.
func (c Config) longestStale() time.Duration {
	longest := DefaultStaleAfter
	for _, d := range c.StaleAfter {
		longest = max(longest, d)
	}
	return longest
}

// backoff is how the engine spaces its looks at a job of the module that made no progress: the first wait is one Tick, each further wait in a row doubles, and none is longer than the ceiling,
// which is BackoffCap but never more than a quarter of the module's staleness limit (a job is judged stale only when a step finds it so, and a step happens at least once per ceiling: a quarter
// of the limit makes the alert at most a quarter late, whatever the dial says). A switched-off backoff is (0, 0): due again at the next tick.
func (c Config) backoff(module string) (base, ceiling time.Duration) {
	if c.BackoffCap < 0 {
		return 0, 0
	}
	ceiling = min(c.BackoffCap, c.staleAfter(module)/4)
	return min(c.Tick, ceiling), ceiling
}

// batch is the number of rows one Submit call of the module is offered.
func (c Config) batch(module string) int {
	if n, ok := c.SubmitBatchFor[module]; ok {
		return n
	}
	return c.SubmitBatch
}

// store is the part of *jobs.Store the engine uses: an interface only so that the unit tests can count and fail calls without a database. *jobs.Store satisfies it (the compile-time check below).
type store interface {
	EngineWork(ctx context.Context, limit int) ([]jobs.EngineJob, error)
	EngineJobByID(ctx context.Context, jobID string) (jobs.EngineJob, error)
	PendingRows(ctx context.Context, jobID string, limit int) ([]jobs.EngineRow, error)
	EnsureJobItems(ctx context.Context, jobID string) (int, error)
	MarkSubmitted(ctx context.Context, jobID string, rowIDs []string) error
	Start(ctx context.Context, jobID, moduleRef string) (jobs.Job, error)
	RecordResults(ctx context.Context, jobID string, items []jobs.ResultItem) (jobs.RecordSummary, error)
	Complete(ctx context.Context, jobID string) (jobs.Job, error)
	Finish(ctx context.Context, jobID string) (jobs.Job, error)
	Expire(ctx context.Context, jobID string) (jobs.Job, error)
	FailJob(ctx context.Context, jobID, reason string) (jobs.Job, error)
	CancelUnsubmitted(ctx context.Context, jobID string) (int, error)
	Release(ctx context.Context, jobID string) error
	NoteProgress(ctx context.Context, jobID string) error
	NotePoll(ctx context.Context, jobID, status string, progressed bool, base, ceiling time.Duration) (bool, error)
	AlertStale(ctx context.Context, jobID string, after time.Duration, detail map[string]any) (bool, error)
	Flag(ctx context.Context, jobID string, detail map[string]any) error
}

var _ store = (*jobs.Store)(nil)

// Deps is what the engine is built from.
type Deps struct {
	Store   *jobs.Store
	Modules Modules
	Leader  Gate
	Log     *slog.Logger     // nil: nothing is logged
	Now     func() time.Time // the engine's own clock, used for staleness arithmetic and throttles only (every deadline is the database's); nil: time.Now
}

// Stats counts what an engine has done since it was built (for the logs, the health report and the tests).
type Stats struct {
	Ticks      int64 // ticks that led
	Standby    int64 // ticks the gate refused
	Steps      int64 // job steps run
	Waits      int64 // steps stopped by a module that could not answer (ErrWait)
	Conflicts  int64 // steps the lifecycle or a race refused (ErrNotAllowed, ErrNotFound): the next tick reads the new state
	Unexpected int64 // steps that failed for any other reason (counted, logged, retried like a wait)
	Panics     int64 // steps that panicked (logged; the loop went on)
	Refused    int64 // jobs failed because the module refused them for good
}

// Engine is the job engine. Tick is one pass; Run ticks until its context ends. It is safe for one goroutine to Tick at a time (two engines are two instances).
type Engine struct {
	st   store
	mods Modules
	gate Gate
	log  *slog.Logger
	now  func() time.Time
	cfg  Config

	mu       sync.Mutex
	lastObs  map[string]observed  // job id -> what the last observation said (memory only)
	flagged  map[string]time.Time // job id -> when the engine last raised a flag for it (memory only)
	refusals map[string]time.Time // job id -> when the engine last flagged a refusal of a job whose rows the module holds (memory only)

	ticks, standby, steps, waits, conflicts, unexpected, panics, refused atomic.Int64
}

// observed is what the engine remembers of the last observation of a job.
type observed struct {
	at       time.Time
	terminal int
	complete bool
}

// New builds an engine. It refuses a missing store, modules or gate (an engine with no gate would lead everywhere) and a Config with a negative dial.
func New(d Deps, cfg Config) (*Engine, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("jobengine: Deps.Store is required")
	case d.Modules == nil:
		return nil, errors.New("jobengine: Deps.Modules is required")
	case d.Leader == nil:
		return nil, errors.New("jobengine: Deps.Leader is required: an engine with no gate would run its loops in every instance")
	}
	return newEngine(d.Store, d.Modules, d.Leader, d.Log, d.Now, cfg)
}

func newEngine(st store, mods Modules, gate Gate, log *slog.Logger, now func() time.Time, cfg Config) (*Engine, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Engine{st: st, mods: mods, gate: gate, log: log, now: now, cfg: cfg, lastObs: map[string]observed{}, flagged: map[string]time.Time{}, refusals: map[string]time.Time{}}, nil
}

// Stats returns the counters.
func (e *Engine) Stats() Stats {
	return Stats{Ticks: e.ticks.Load(), Standby: e.standby.Load(), Steps: e.steps.Load(), Waits: e.waits.Load(), Conflicts: e.conflicts.Load(),
		Unexpected: e.unexpected.Load(), Panics: e.panics.Load(), Refused: e.refused.Load()}
}

// Run ticks until ctx ends, waiting Config.Tick between ticks, and returns nil. Nothing a tick does ends it.
func (e *Engine) Run(ctx context.Context) error {
	e.log.InfoContext(ctx, "job engine started", "tick", e.cfg.Tick.String())
	for ctx.Err() == nil {
		e.Tick(ctx)
		t := time.NewTimer(e.cfg.Tick)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	e.log.Info("job engine stopped")
	return nil
}

// Tick is one pass: ask the gate (a standby does nothing else), read the work, and run each job's step in turn. A step that fails, times out or panics is logged and counted and the next job's
// step runs; a context that ends stops the pass at once.
func (e *Engine) Tick(ctx context.Context) {
	if !e.leads(ctx) {
		e.standby.Add(1)
		return
	}
	e.ticks.Add(1)
	work, err := e.st.EngineWork(ctx, e.cfg.WorkLimit)
	if err != nil {
		e.account(ctx, "", "", err)
		return
	}
	e.prune(work)
	for _, j := range work {
		if ctx.Err() != nil {
			return
		}
		e.safely(ctx, j)
	}
}

// leads asks the gate and turns a panic in it into "not the leader": the failure of the election can no more end the loop than the failure of a step.
func (e *Engine) leads(ctx context.Context) (leading bool) {
	defer func() {
		if p := recover(); p != nil {
			leading = false
			e.panics.Add(1)
			e.log.ErrorContext(ctx, "the job engine's leader check panicked; this tick is skipped", "panic", fmt.Sprint(p))
		}
	}()
	return e.gate.Ensure(ctx)
}

// safely runs one job's step under its own timeout and turns a panic into a log line, a count and the next job. Whatever came of the step is then told to the store (notePoll): a job whose step
// panicked is told as one that made no progress, so a job that panics at every look is not looked at at every tick.
func (e *Engine) safely(ctx context.Context, j jobs.EngineJob) {
	defer func() {
		if p := recover(); p != nil {
			e.panics.Add(1)
			e.log.ErrorContext(ctx, "a job step panicked; the loop goes on", "job", j.ID, "module", j.Module, "status", j.Status, "panic", fmt.Sprint(p))
			e.notePoll(ctx, j, false, nil)
		}
	}()
	sctx, cancel := context.WithTimeout(ctx, e.cfg.StepTimeout)
	defer cancel()
	e.steps.Add(1)
	progressed, err := e.step(sctx, j)
	e.account(ctx, j.ID, j.Module, err)
	e.notePoll(ctx, j, progressed, err)
}

// notePoll tells the store what came of a step (jobs.Store.NotePoll, D8): progress puts the job at the back of the queue, due at the next tick; no progress (a module that cannot answer, an
// error, a step cut off) leaves it alone for a backoff that doubles from one tick up to the cap. Nothing is told for a job that has ended (the repair set is due whatever its backoff), for a step
// the lifecycle refused or a race overtook (somebody moved the job and the database woke it) and when the pass itself is ending. A failed note is logged and costs nothing: the job simply stays due.
func (e *Engine) notePoll(ctx context.Context, j jobs.EngineJob, progressed bool, err error) {
	if jobs.IsTerminal(j.Status) || ctx.Err() != nil {
		return
	}
	if !progressed && (errors.Is(err, jobs.ErrNotAllowed) || errors.Is(err, jobs.ErrNotFound)) {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			e.panics.Add(1)
			e.log.ErrorContext(ctx, "the job engine's poll note panicked", "job", j.ID, "panic", fmt.Sprint(p))
		}
	}()
	base, ceiling := e.cfg.backoff(j.Module)
	if _, nerr := e.st.NotePoll(ctx, j.ID, j.Status, progressed, base, ceiling); nerr != nil {
		e.log.WarnContext(ctx, "the engine could not note what came of a step; the job stays due", "job", j.ID, "module", j.Module, "why", nerr.Error())
	}
}

// account classifies what a step returned: nothing, a module that could not answer, a refusal by the lifecycle or a race (the next tick reads the new state), a context that ended, or anything else
// (unexpected: logged and counted, retried like a wait).
func (e *Engine) account(ctx context.Context, jobID, module string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, ErrWait):
		e.waits.Add(1)
		e.log.DebugContext(ctx, "the module cannot answer; the job stays where it is", "job", jobID, "module", module, "why", err.Error())
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		e.log.DebugContext(ctx, "a job step was cut off", "job", jobID, "module", module)
	case errors.Is(err, jobs.ErrNotAllowed) || errors.Is(err, jobs.ErrNotFound):
		e.conflicts.Add(1)
		e.log.InfoContext(ctx, "a job step was refused by the lifecycle or overtaken; the next tick reads the new state", "job", jobID, "module", module, "why", err.Error())
	default:
		e.unexpected.Add(1)
		e.log.WarnContext(ctx, "a job step failed for an unexpected reason; it is retried at the next tick", "job", jobID, "module", module, "why", err.Error())
	}
}

// prune forgets what the engine remembers of jobs that are not in this tick's work. The last observation of a job is forgotten at once (a missing one only makes the next look observe, never skip: it
// is a throttle). A flag's time is kept until it is older than the longest staleness limit: a job that is not in the work may only be waiting out a backoff (D8), and forgetting the time of its
// flag would raise the same flag again at every look instead of once per staleness interval; an entry older than every limit can hold nothing back, so it goes (a finished job's entry with it).
func (e *Engine) prune(work []jobs.EngineJob) {
	live := make(map[string]bool, len(work))
	for _, j := range work {
		live[j.ID] = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.lastObs {
		if !live[id] {
			delete(e.lastObs, id)
		}
	}
	horizon := e.now().Add(-e.cfg.longestStale())
	for _, memory := range []map[string]time.Time{e.flagged, e.refusals} {
		for id, at := range memory {
			if !live[id] && at.Before(horizon) {
				delete(memory, id)
			}
		}
	}
}

// step is the next step of one job, by the status the work read. It says whether the step made PROGRESS (a row handed to the module, a result recorded, the job moved or ended): what the engine
// tells the store about the look (notePoll), and with it when the job is looked at again. A step that returns an error may still have progressed (it handed over rows and then met a module that was full).
func (e *Engine) step(ctx context.Context, j jobs.EngineJob) (progressed bool, err error) {
	switch j.Status {
	case jobs.StatusConfirmed, jobs.StatusScheduled:
		return e.starting(ctx, j)
	case jobs.StatusPaused:
		if j.DeadlinePassed {
			return e.expire(ctx, j)
		}
		return false, nil
	case jobs.StatusRunning:
		return e.running(ctx, j)
	case jobs.StatusCancelling:
		return e.cancelling(ctx, j)
	case jobs.StatusCompleting:
		return e.completing(ctx, j)
	}
	if jobs.IsTerminal(j.Status) {
		return e.repair(ctx, j)
	}
	return false, fmt.Errorf("jobengine: job %s is in status %q, which the engine has no step for", j.ID, j.Status)
}

func (e *Engine) expire(ctx context.Context, j jobs.EngineJob) (bool, error) {
	_, err := e.st.Expire(ctx, j.ID)
	return err == nil, err
}

// repair gives back what a terminal job's reserve still holds (a crash between the move and the release, or an administrator who ended the job).
func (e *Engine) repair(ctx context.Context, j jobs.EngineJob) (bool, error) {
	if err := e.st.Release(ctx, j.ID); err != nil {
		return false, err
	}
	e.log.InfoContext(ctx, "the remainder of an ended job's reserve was released", "job", j.ID, "module", j.Module, "status", j.Status, "remainder", j.ReserveRemainder)
	return true, nil
}

// ---- starting -------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// starting is the step of a confirmed or scheduled job: past its start deadline (the database's clock, read with the work) it expires and its reserve goes back; otherwise the first batch of
// rows is handed to the module and, as soon as the module holds at least one, the job is moved to running. The rest of the rows is the running step's. A module that cannot answer leaves the job
// where it is (it expires at its deadline, never before); a module that refuses the job for good, while it holds none of its rows, fails it (see refusal).
func (e *Engine) starting(ctx context.Context, j jobs.EngineJob) (bool, error) {
	if j.DeadlinePassed {
		return e.expire(ctx, j)
	}
	mod, err := e.mods.For(ctx, j.UserID, j.Module)
	if err != nil {
		return false, err
	}
	rows, err := e.st.PendingRows(ctx, j.ID, e.cfg.batch(j.Module))
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		// a confirmed job always has a pending row without a result; none to hand over means another instance did it a moment ago and the next tick reads the new state
		return false, nil
	}
	stamped, err := e.submit(ctx, &j, mod, rows)
	failed, err := e.refusal(ctx, j, stamped, err)
	return stamped > 0 || failed, err
}

// submit hands one batch of rows to the module and records what it took: the module-facing rows for a send job first (public.job_items, idempotent), then Module.Submit, then, for a job that is not
// yet running, the move to running (as soon as the module holds a row), and last the stamps. The order is what makes a crash anywhere harmless: a row is stamped only after the module holds it
// AND the job is running, so there is no state in which a job is confirmed with every row stamped and nothing to start it; a crash before the stamps is repaired by offering the same rows again
// (Submit is idempotent). It returns how many rows it STAMPED in this call (the rows the module took and the store now says were handed over); an error returns 0, so a count above zero always means
// rows of the job that the module holds.
func (e *Engine) submit(ctx context.Context, j *jobs.EngineJob, mod Module, rows []jobs.EngineRow) (int, error) {
	offered := make(map[string]bool, len(rows))
	for _, r := range rows {
		offered[r.ID] = true
	}
	// modules 2 and 3 read their rows from public.job_items; module 1 is handed the list itself. The rows of the WHOLE job are written once (D9): a job that has them (items_ensured_at, read with
	// the work) costs nothing here, at this tick, at the next and after a restart; the copy of the job this step holds learns it, so the batches of one step ask once.
	if j.Module != jobs.ModuleVerify && j.ItemsEnsuredAt == nil {
		if _, err := e.st.EnsureJobItems(ctx, j.ID); err != nil {
			return 0, err
		}
		at := e.now()
		j.ItemsEnsuredAt = &at
	}
	res, err := mod.Submit(ctx, *j, rows)
	if err != nil {
		return 0, err
	}
	accepted := make([]string, 0, len(res.Accepted))
	seen := make(map[string]bool, len(res.Accepted))
	for _, id := range res.Accepted {
		if offered[id] && !seen[id] {
			seen[id] = true
			accepted = append(accepted, id)
		} else if !offered[id] {
			e.log.WarnContext(ctx, "a module says it holds a row nobody offered it; ignored", "job", j.ID, "module", j.Module)
		}
	}
	faultpoint.Hit("submit.after_module")
	if len(accepted) == 0 {
		return 0, nil
	}
	if j.Status != jobs.StatusRunning {
		if _, err := e.st.Start(ctx, j.ID, res.ModuleRef); err != nil {
			return 0, err
		}
	}
	if err := e.st.MarkSubmitted(ctx, j.ID, accepted); err != nil {
		return 0, err
	}
	return len(accepted), nil
}

// refusal is what the engine does with the answer of a Submit. Anything but ErrRefused passes through (failed false). A module's refusal for good ends the job (failed, the unsettled rows free, the
// remainder released) ONLY while the job holds no row at the module: j.Submitted rows were stamped before this step, stamped were stamped in this step before the refusal, and a RUNNING job was moved
// to running only after the module held a row (a crash between that move and the stamps leaves it running with none stamped). With rows held, failing the job would write infrastructure_failure for
// every row without a result while the module goes on sending the queued ones, rows that would be sent and never recorded or charged: so the job is NOT failed. The engine flags it (once per
// staleness interval, the module's refusal code word only), returns a wait (the job stays where it is, goes stale and raises the stale event) and the running step goes on to observe what the module
// holds. Nothing is invented. The engine does not depend on the wrappers for this (they too turn a refusal into a wait when rows are held): it is the engine's own rule.
func (e *Engine) refusal(ctx context.Context, j jobs.EngineJob, stamped int, err error) (failed bool, out error) {
	if err == nil || !errors.Is(err, ErrRefused) {
		return false, err
	}
	if holdsRows(j, stamped) {
		code := RefusalCode(err)
		e.flagRefusal(ctx, j, j.Submitted+stamped, code)
		return false, Wait(fmt.Sprintf("the module refused rows of a job it already holds (%s): the job is not failed and waits", code))
	}
	e.refused.Add(1)
	reason := cutReason("the module refused the job for good: " + err.Error())
	e.log.WarnContext(ctx, "the module refused the job for good; the job fails", "job", j.ID, "module", j.Module)
	_, ferr := e.st.FailJob(ctx, j.ID, reason)
	return true, ferr
}

// holdsRows says whether the module holds at least one row of the job, as far as the engine can know: rows stamped before this step (j is the job as the step began), rows stamped in this step before the
// refusal, or a job that is already running (the move to running follows the module taking a row, and a crash between that move and the stamps leaves it running with none stamped).
func holdsRows(j jobs.EngineJob, stamped int) bool {
	return j.Submitted+stamped > 0 || j.Status == jobs.StatusRunning
}

// flagRefusal writes the `flag` event of a refusal the engine did not fail the job for, at most once per staleness interval of the job's module (a refusal repeats at every tick). The event names the
// module's code word and the counts, never the module's text.
func (e *Engine) flagRefusal(ctx context.Context, j jobs.EngineJob, held int, code string) {
	e.mu.Lock()
	last, seen := e.refusals[j.ID]
	due := !seen || e.now().Sub(last) >= e.cfg.staleAfter(j.Module)
	if due {
		e.refusals[j.ID] = e.now()
	}
	e.mu.Unlock()
	e.log.WarnContext(ctx, "the module refused rows of a job it already holds; the job is not failed and waits", "job", j.ID, "module", j.Module, "refusal", code, "submitted", held, "pending", j.Pending)
	if !due {
		return
	}
	if err := e.st.Flag(ctx, j.ID, map[string]any{
		"reason":  "the module refused rows of a job it already holds: the job is not failed (the module goes on with the rows it holds) and nothing is invented; an administrator decides",
		"refusal": code, "module": j.Module, "submitted": held, "pending": j.Pending,
	}); err != nil {
		e.mu.Lock()
		delete(e.refusals, j.ID) // not written: the next tick tries again
		e.mu.Unlock()
		e.log.WarnContext(ctx, "the engine could not write its flag", "job", j.ID, "why", err.Error())
	}
}

func cutReason(s string) string {
	const max = 300
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ---- running --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// running is the step of a running job: submit the rows the module does not hold yet (in batches, until the module takes fewer than it was offered), observe what it reports (at most every
// SettleEvery unless it showed unrecorded terminal items) and record every page, note progress when anything was submitted or recorded, and, when the module says it is complete and every pending
// row has a result, complete and finish the job in this same step. A submit that fails does not keep the engine from observing: a module that refuses new rows still reports the old ones (a refusal for good, which fails the job only when the module holds none of its rows, is the one that ends the step: see refusal).
func (e *Engine) running(ctx context.Context, j jobs.EngineJob) (bool, error) {
	mod, err := e.mods.For(ctx, j.UserID, j.Module)
	if err != nil {
		return false, err
	}
	progressed := false
	var firstErr error

	submitted, err := e.submitRest(ctx, &j, mod)
	progressed = progressed || submitted > 0
	if err != nil {
		failed, rerr := e.refusal(ctx, j, submitted, err)
		if failed {
			return true, rerr
		}
		firstErr = rerr
	}

	var obs Observation
	observed := false
	if e.shouldObserve(j) {
		recorded, o, err := e.observe(ctx, j, mod)
		obs, observed = o, err == nil
		progressed = progressed || recorded > 0
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if progressed {
		if err := e.st.NoteProgress(ctx, j.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if observed && obs.Complete {
		finished, err := e.settleCompletion(ctx, j, false)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if finished {
			return true, firstErr
		}
	}
	if !progressed {
		e.checkStale(ctx, j)
	}
	return progressed, firstErr
}

// submitRest hands the job's remaining rows to the module in batches and returns how many it took in all.
func (e *Engine) submitRest(ctx context.Context, j *jobs.EngineJob, mod Module) (int, error) {
	total := 0
	prevFirst := ""
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		rows, err := e.st.PendingRows(ctx, j.ID, e.cfg.batch(j.Module))
		if err != nil {
			return total, err
		}
		if len(rows) == 0 || rows[0].ID == prevFirst { // nothing left, or the same rows again (a stamp that did not take): never spin
			return total, nil
		}
		prevFirst = rows[0].ID
		n, err := e.submit(ctx, j, mod, rows)
		total += n
		if err != nil {
			return total, err
		}
		if n < len(rows) { // the module took only some: it is full for now, ask again at the next tick
			return total, nil
		}
	}
}

// shouldObserve says whether a RUNNING job is observed this tick: always for a job the engine has not observed yet (a restart included), for one whose last observation showed terminal items
// that are not recorded yet or said the job was complete (a completion that did not go through is retried), and otherwise when SettleEvery has passed.
func (e *Engine) shouldObserve(j jobs.EngineJob) bool {
	e.mu.Lock()
	o, ok := e.lastObs[j.ID]
	e.mu.Unlock()
	switch {
	case !ok, o.complete, o.terminal > j.PendingResults, e.cfg.SettleEvery < 0:
		return true
	}
	return e.now().Sub(o.at) >= e.cfg.SettleEvery
}

// observe reads the module's state of the job and records every page through the store (settlement and results in one transaction per page). It returns how many results were newly recorded and the
// module's observation; what was recorded before an error stays recorded.
func (e *Engine) observe(ctx context.Context, j jobs.EngineJob, mod Module) (int, Observation, error) {
	recorded := 0
	obs, err := mod.Observe(ctx, j, func(items []jobs.ResultItem) error {
		if len(items) == 0 {
			return nil
		}
		sum, err := e.st.RecordResults(ctx, j.ID, items)
		if err != nil {
			return err
		}
		recorded += sum.Recorded
		if sum.Rejected > 0 {
			e.log.WarnContext(ctx, "the module reported items that are no pending row of the job; they were not recorded", "job", j.ID, "module", j.Module, "items", sum.Rejected)
		}
		return nil
	})
	if err != nil {
		return recorded, Observation{}, err
	}
	e.mu.Lock()
	e.lastObs[j.ID] = observed{at: e.now(), terminal: obs.Terminal, complete: obs.Complete}
	e.mu.Unlock()
	return recorded, obs, nil
}

// settleCompletion is what a job does when its module says it is complete: re-read the counts the recording just changed; when every pending row has a result, complete and finish the job (finished
// is true); when rows are still to be handed to the module, say nothing (the job is still submitting); when a cancelling job has rows no module was ever given, end them as the platform's own
// cancelled results and look again; and when the module says complete but a SUBMITTED row has no result, do NOT complete: flag it (once per staleness interval) and retry at the next tick.
func (e *Engine) settleCompletion(ctx context.Context, j jobs.EngineJob, cancelling bool) (finished bool, err error) {
	fresh, err := e.st.EngineJobByID(ctx, j.ID)
	if err != nil {
		return false, err
	}
	if cancelling && fresh.Unsubmitted > 0 {
		if _, err := e.st.CancelUnsubmitted(ctx, j.ID); err != nil {
			return false, err
		}
		if fresh, err = e.st.EngineJobByID(ctx, j.ID); err != nil {
			return false, err
		}
	}
	switch {
	case fresh.Unresolved() == 0:
		if err := e.finish(ctx, j.ID); err != nil {
			return false, err
		}
		return true, nil
	case fresh.Unsubmitted > 0:
		return false, nil // the module is complete with what it holds; the rest of the rows is still to be submitted
	}
	e.holdBack(ctx, fresh)
	return false, nil
}

// finish completes and finishes a job whose every pending row has a result.
func (e *Engine) finish(ctx context.Context, jobID string) error {
	if _, err := e.st.Complete(ctx, jobID); err != nil {
		return err
	}
	_, err := e.st.Finish(ctx, jobID)
	return err
}

// holdBack is the engine refusing to invent an outcome: the module says the job is complete and rows it submitted have no recorded result. The job stays as it is; the engine flags it, at most
// once per staleness interval, and the next tick looks again. (If the module never reports the missing rows the stale event says the job is stuck, and an administrator decides.)
func (e *Engine) holdBack(ctx context.Context, j jobs.EngineJob) {
	e.mu.Lock()
	last, seen := e.flagged[j.ID]
	due := !seen || e.now().Sub(last) >= e.cfg.staleAfter(j.Module)
	if due {
		e.flagged[j.ID] = e.now()
	}
	e.mu.Unlock()
	e.log.WarnContext(ctx, "the module says the job is complete but submitted rows have no recorded result; the job is not completed", "job", j.ID, "module", j.Module, "unresolved", j.Unresolved(), "results", j.PendingResults, "submitted", j.Submitted)
	if !due {
		return
	}
	if err := e.st.Flag(ctx, j.ID, map[string]any{
		"reason":     "the module reports the job complete, but submitted rows have no recorded result: the job is not completed and nothing is invented",
		"unresolved": j.Unresolved(), "submitted": j.Submitted, "results": j.PendingResults, "pending": j.Pending, "module": j.Module,
	}); err != nil {
		e.log.WarnContext(ctx, "the engine could not write its flag", "job", j.ID, "why", err.Error())
	}
}

// checkStale raises the stale event for a job that made no progress in this step and whose last progress is older than its module's limit. The job is not touched.
func (e *Engine) checkStale(ctx context.Context, j jobs.EngineJob) {
	last := j.LastProgressAt
	if last == nil {
		last = j.StartedAt
	}
	if last == nil {
		last = j.ConfirmedAt
	}
	limit := e.cfg.staleAfter(j.Module)
	now := j.DBNow // both ends of the subtraction are the database's: a host whose clock is skewed neither hides a stale job nor raises a false alarm
	if now.IsZero() {
		now = e.now()
	}
	if last == nil || now.Sub(*last) < limit {
		return
	}
	wrote, err := e.st.AlertStale(ctx, j.ID, limit, map[string]any{
		"module": j.Module, "status": j.Status, "stale_after_seconds": int64(limit / time.Second), "last_progress_at": last.UTC().Format(time.RFC3339),
		"pending": j.Pending, "submitted": j.Submitted, "results": j.PendingResults, "unresolved": j.Unresolved(),
	})
	switch {
	case err != nil:
		e.log.WarnContext(ctx, "the engine could not write a stale event", "job", j.ID, "why", err.Error())
	case wrote:
		e.log.WarnContext(ctx, "a job made no progress for longer than its module's limit; it stays as it is", "job", j.ID, "module", j.Module, "status", j.Status, "limit", limit.String())
	}
}

// ---- cancelling and completing -------------------------------------------------------------------------------------------------------------------------------------------------------

// cancelling is the step of a cancelling job: ask the module to cancel (idempotent, at every tick until it reports complete), observe and record, and, once the module is complete, end the rows no
// module was ever given as the platform's own cancelled results and complete and finish the job (it ends cancelled: the cancel was accepted).
func (e *Engine) cancelling(ctx context.Context, j jobs.EngineJob) (bool, error) {
	mod, err := e.mods.For(ctx, j.UserID, j.Module)
	if err != nil {
		return false, err
	}
	var firstErr error
	if err := mod.Cancel(ctx, j); err != nil {
		if errors.Is(err, ErrNotSupported) {
			// the database refuses a cancel of a job whose module cannot cancel, so this is a configuration the platform did not intend: keep observing, never invent
			e.log.ErrorContext(ctx, "a module that cannot cancel has a cancelling job", "job", j.ID, "module", j.Module)
		}
		firstErr = err
	}
	recorded, obs, err := e.observe(ctx, j, mod)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if recorded > 0 {
		if err := e.st.NoteProgress(ctx, j.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err == nil && obs.Complete {
		finished, serr := e.settleCompletion(ctx, j, true)
		if serr != nil && firstErr == nil {
			firstErr = serr
		}
		if finished {
			return true, firstErr
		}
	}
	if recorded == 0 {
		e.checkStale(ctx, j)
	}
	return recorded > 0, firstErr
}

// completing is the step of a job whose module reported every item final and that was moved to completing in an earlier step (a crash between the two): a final pass of observe and record, then
// Finish, which re-checks the results and releases the remainder in one transaction.
func (e *Engine) completing(ctx context.Context, j jobs.EngineJob) (bool, error) {
	mod, err := e.mods.For(ctx, j.UserID, j.Module)
	if err != nil {
		return false, err
	}
	var firstErr error
	if _, _, err := e.observe(ctx, j, mod); err != nil {
		firstErr = err
	}
	finished := true
	if _, err := e.st.Finish(ctx, j.ID); err != nil {
		finished = false
		var inc *jobs.IncompleteError
		if errors.As(err, &inc) {
			if fresh, rerr := e.st.EngineJobByID(ctx, j.ID); rerr == nil {
				e.holdBack(ctx, fresh)
			}
			e.checkStale(ctx, j)
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return finished, firstErr
}
