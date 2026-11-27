package jobengine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// THE TWO HOUSEKEEPING STEPS OF THE ENGINE (Phase 5 S7 and S8). Both run at the end of Tick, after the pass over the jobs that are due, so they act on what that pass left and never on a job it was just about to
// move, and both run only in the instance that leads (Tick asks the gate first): there is one gate, one set of counters, one stop (the engine ends first, see internal/app) and no loop of their own to start,
// stop or keep from running in two instances. They are STEPS and not a loop of their own because that is the cheapest way to hold all of that, and because a test that ticks the engine then runs them with the rest.
//   - THE PAUSE LIMIT (jobs.Store.CancelOverLimitPaused): a job paused for longer than Config.PauseMax by the DATABASE's clock is cancelled; one that its module holds goes through cancelling, which this engine's
//     own cancelling step then settles on the next tick (the status change wakes the job).
//   - CONTENT RETENTION (jobs.Store.ClearRetainedContent): the rendered content of an ended send job is cleared Config.RetentionAfter after it ended, a bounded number of pages per pass, so a job of 100,000
//     rows is cleared over a few passes and no pass holds the others up.
//
// THE CADENCE is Config.SweepEvery by the engine's own clock (a throttle, like SettleEvery: every deadline and every comparison is the database's). A step is bounded by Config.StepTimeout, a failure is
// counted and logged and never ends the tick, and a panic is recovered. Both are idempotent and safe in two instances at once, so a deposed leader that finishes a sweep while its successor starts one is harmless.

// Defaults and limits of the two dials (each is a PLATFORM_* dial wired by internal/app: PLATFORM_JOB_PAUSE_MAX_DAYS and PLATFORM_CONTENT_RETENTION_DAYS; a zero Config field means the default).
const (
	DefaultPauseMax       = 7 * 24 * time.Hour
	MaxPauseMax           = jobs.MaxPauseLimit
	DefaultRetentionAfter = 7 * 24 * time.Hour
	MaxRetentionAfter     = jobs.MaxContentRetention
	DefaultSweepEvery     = 30 * time.Second

	pauseJobsPerSweep      = 50 // over-limit jobs ended in one pass (the rest wait for the next)
	retentionJobsPerSweep  = 20 // ended jobs looked at in one pass
	retentionPagesPerSweep = 8  // pages of jobs.rowBatch rows written in one pass (8 x 5000 rows)
)

// withSweepDefaults validates and defaults the three fields of Config the sweeps read. A zero field means its default; a negative PauseMax or RetentionAfter is refused, and so is one outside what the store takes
// (at least a second, so that a test can use seconds; at most 90 and 365 days, the tops of the two dials); a negative SweepEvery means "every tick" (for the tests that tick back to back).
func (c Config) withSweepDefaults() (Config, error) {
	if c.PauseMax != 0 && (c.PauseMax < jobs.MinPauseLimit || c.PauseMax > MaxPauseMax) {
		return Config{}, fmt.Errorf("jobengine: Config.PauseMax must be between %s and %s, got %s", jobs.MinPauseLimit, MaxPauseMax, c.PauseMax)
	}
	if c.RetentionAfter != 0 && (c.RetentionAfter < jobs.MinContentRetention || c.RetentionAfter > MaxRetentionAfter) {
		return Config{}, fmt.Errorf("jobengine: Config.RetentionAfter must be between %s and %s, got %s", jobs.MinContentRetention, MaxRetentionAfter, c.RetentionAfter)
	}
	if c.PauseMax == 0 {
		c.PauseMax = DefaultPauseMax
	}
	if c.RetentionAfter == 0 {
		c.RetentionAfter = DefaultRetentionAfter
	}
	if c.SweepEvery == 0 {
		c.SweepEvery = DefaultSweepEvery
	}
	return c, nil
}

// pauseLimiter and contentRetainer are the parts of *jobs.Store the sweeps use. They are their own small interfaces (and not methods of the engine's store interface) so that the unit tests of the passes over
// jobs, which count and fail calls without a database, need not know them; New sets both from the job store, and an engine built without them (the unit tests of the passes) skips the sweeps.
type pauseLimiter interface {
	CancelOverLimitPaused(ctx context.Context, limit time.Duration, max int) (jobs.PauseLimitResult, error)
}

type contentRetainer interface {
	ClearRetainedContent(ctx context.Context, after time.Duration, maxJobs, maxPages int) (jobs.RetentionResult, error)
}

var (
	_ pauseLimiter    = (*jobs.Store)(nil)
	_ contentRetainer = (*jobs.Store)(nil)
)

// sweepState is what the engine remembers of the sweeps: when each last started (its own clock; memory only, a restart simply sweeps at once) and what they have done.
type sweepState struct {
	mu          sync.Mutex
	pauseAt     time.Time
	retentionAt time.Time

	pauseSweeps, pauseCancelled, pauseCancelling, pauseFailed      atomic.Int64
	retentionSweeps, contentCleared, contentItems, retentionFailed atomic.Int64
}

// SweepStats counts what the two steps have done since the engine was built (for the logs and the tests).
type SweepStats struct {
	PauseSweeps     int64 // passes of the pause-limit step that ran
	PauseCancelled  int64 // paused jobs ended cancelled directly (they had submitted nothing)
	PauseCancelling int64 // paused jobs handed to the cancelling step (their module held items)
	PauseFailed     int64 // paused jobs the lifecycle or the database refused
	RetentionSweeps int64 // passes of the retention step that ran
	ContentCleared  int64 // ended jobs whose content was cleared and marked
	ContentItems    int64 // rows of public.job_items cleared
	RetentionFailed int64 // jobs the database stopped
}

// SweepStats returns the counters of the two housekeeping steps.
func (e *Engine) SweepStats() SweepStats {
	s := &e.sweeps
	return SweepStats{PauseSweeps: s.pauseSweeps.Load(), PauseCancelled: s.pauseCancelled.Load(), PauseCancelling: s.pauseCancelling.Load(), PauseFailed: s.pauseFailed.Load(),
		RetentionSweeps: s.retentionSweeps.Load(), ContentCleared: s.contentCleared.Load(), ContentItems: s.contentItems.Load(), RetentionFailed: s.retentionFailed.Load()}
}

// sweep runs the two steps that are due. It is called by Tick, by the leader only.
func (e *Engine) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if e.pl != nil {
		e.sweepStep(ctx, "pause limit", &e.sweeps.pauseAt, e.pauseLimitPass)
	}
	if e.ret != nil {
		e.sweepStep(ctx, "content retention", &e.sweeps.retentionAt, e.retentionPass)
	}
}

// sweepStep runs one step if its cadence says so, under its own timeout, and turns a failure or a panic into a count and a log line.
func (e *Engine) sweepStep(ctx context.Context, name string, last *time.Time, pass func(context.Context) error) {
	e.sweeps.mu.Lock()
	now := e.now()
	due := e.cfg.SweepEvery < 0 || last.IsZero() || now.Sub(*last) >= e.cfg.SweepEvery
	if due {
		*last = now
	}
	e.sweeps.mu.Unlock()
	if !due || ctx.Err() != nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			e.panics.Add(1)
			e.log.ErrorContext(ctx, "a housekeeping step panicked; the loop goes on", "step", name, "panic", fmt.Sprint(p))
		}
	}()
	sctx, cancel := context.WithTimeout(ctx, e.cfg.StepTimeout)
	defer cancel()
	if err := pass(sctx); err != nil {
		e.account(ctx, "", "", err)
		e.log.WarnContext(ctx, "a housekeeping step failed; it is tried again at its next pass", "step", name, "why", err.Error())
	}
}

// pauseLimitPass ends the paused jobs that outlasted Config.PauseMax and logs each.
func (e *Engine) pauseLimitPass(ctx context.Context) error {
	res, err := e.pl.CancelOverLimitPaused(ctx, e.cfg.PauseMax, pauseJobsPerSweep)
	e.sweeps.pauseSweeps.Add(1)
	e.sweeps.pauseCancelled.Add(int64(res.Cancelled))
	e.sweeps.pauseCancelling.Add(int64(res.Cancelling))
	e.sweeps.pauseFailed.Add(int64(res.Failed))
	for _, j := range res.Jobs {
		e.log.InfoContext(ctx, "a paused job outlasted the pause limit and was cancelled by the engine", "job", j.JobID, "module", j.Module, "paused_from", j.PausedFrom, "moved_to", j.To, "limit", e.cfg.PauseMax.String())
	}
	return err
}

// retentionPass clears the content of the ended send jobs that are due, a bounded number of pages.
func (e *Engine) retentionPass(ctx context.Context) error {
	res, err := e.ret.ClearRetainedContent(ctx, e.cfg.RetentionAfter, retentionJobsPerSweep, retentionPagesPerSweep)
	e.sweeps.retentionSweeps.Add(1)
	e.sweeps.contentCleared.Add(int64(res.Cleared))
	e.sweeps.contentItems.Add(res.Items)
	e.sweeps.retentionFailed.Add(int64(res.Failed))
	if res.Cleared > 0 || res.Items > 0 {
		e.log.InfoContext(ctx, "the rendered content of ended jobs was cleared", "jobs", res.Cleared, "rows", res.Items, "unfinished", res.Unfinished, "after", e.cfg.RetentionAfter.String())
	}
	return err
}
