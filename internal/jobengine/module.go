// Package jobengine is the job engine: the leader-only loops that carry a confirmed job to its end (Docs/phase-4-spec.md, "The loops"). It starts a job (hands its rows to the module, then moves
// the job to running), observes what the module reports, records every final item (the settlement and the job's results, in one transaction), completes the job, releases what was not used,
// expires a job the module never took, repairs a terminal job whose reserve still has a remainder, and raises a `stale` event for a job that makes no progress. It writes nothing itself: every
// change goes through internal/jobs (the one writer of the job tables, scripts/check-job-writers.sh), and it talks to a module only through the Module interface below, which a later chunk
// implements over the adapters (internal/adapters/{m1,m2,m3}). No HTTP and no SQL lives here.
//
// THE ENGINE IS LEVEL-TRIGGERED. It keeps no retry state that matters: every tick asks the database what needs doing (jobs.Store.EngineWork) and does the next step of each job. A step that
// cannot be done (the module does not answer) is simply not done, and the same step is found again at the next tick; a step the process died in leaves a state the next tick continues from,
// because every step of the store is idempotent and one transaction (internal/jobs engine.go). What the engine remembers in memory (when it last observed a job, when it last flagged one) only
// spares the module a call or the log a line: losing it costs nothing, and a second engine with none behaves exactly like the first.
//
// NEVER AN INVENTED OUTCOME (Locked 27). An item the module did not report final is not recorded. A module that does not answer leaves the job where it is. A job whose module says it is complete
// while some submitted row has no recorded result is NOT completed: it is flagged, retried, and raises the stale event after the module's staleness limit. A job is never failed or expired for
// being slow, only for being refused for good (ErrRefused) or for never having started before its deadline.
//
// ONE LEADER. Every tick begins with Gate.Ensure (the entitlements Leader satisfies it): a standby does nothing at all. The database's own rules, not the gate, are what keep a deposed leader
// that is still finishing a step safe: the step holds the (user, module) advisory lock, so the new leader's step runs after it and reads what it left.
package jobengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Module is the engine's whole view of a module. A later chunk writes the real ones over the adapters; the tests use jobenginetest.FakeModule. A method never panics and never blocks past its
// context; its errors are one of the typed errors below (wrapped with %w) or anything else, which the engine treats as unexpected.
type Module interface {
	// Submit puts the rows into the module. IDEMPOTENT: a repeat with the same rows is the same job (module 1 keys on the job id, modules 2 and 3 on the row ids). It may take only SOME of the rows
	// (rate limits, queue caps): it returns which row ids the module now holds, and a row it holds from an earlier call may be returned again. ModuleRef is the module's own handle for the job
	// (module 1's job id) or "". An error means NONE of the rows is certain to be held (the engine offers them again).
	Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (SubmitResult, error)
	// Observe reads the module's state of the job and hands every TERMINAL item it can see to fn, in pages (idempotent: the engine records what it has not recorded, and fn may be called with an item
	// it has seen before). An item that is not final is NEVER passed. It reports the module's own totals and whether the module says the job is complete. An error from fn stops the walk and is
	// returned.
	Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (Observation, error)
	// Cancel asks the module to cancel the job (idempotent; unclaimed items become cancelled, an item in flight finishes). A module that cannot returns ErrNotSupported.
	Cancel(ctx context.Context, j jobs.EngineJob) error
}

// Modules gives the engine the Module of a job's module id, for the job's user (module 1 needs the user's own caller key).
type Modules interface {
	For(ctx context.Context, userID, moduleID string) (Module, error)
}

// SubmitResult is what a module holds after a Submit.
type SubmitResult struct {
	Accepted  []string // the ids of the rows the module now holds (a subset of the rows offered)
	ModuleRef string   // the module's own handle for the job, "" when it has none
}

// Observation is what a module says about a job.
type Observation struct {
	Complete bool           // the module says every item it holds is final
	Total    int            // the items the module holds
	Terminal int            // the items it holds that are final
	Detail   map[string]any // the module's own words, for the logs and the stale event (never a secret or an address)
}

// The errors the engine understands. A module wraps one of them with %w and says why in the text (a word, never an address or a secret).
var (
	// ErrWait: the module cannot answer now (Unavailable, Timeout, Paused, RateLimited, Backpressure, a campaign that is still active). The step stops, the job stays where it is, nothing is invented, and
	// the next tick tries again.
	ErrWait = errors.New("jobengine: the module cannot answer now")
	// ErrRefused: the module refused the job for good (its validation said no). The engine fails the job: the rows nobody settled are free, what was charged stays charged.
	ErrRefused = errors.New("jobengine: the module refused the job for good")
	// ErrNotSupported: the module cannot do what was asked (a cancel it does not have).
	ErrNotSupported = errors.New("jobengine: the module does not support this")
)

// Wait wraps ErrWait with a reason word.
func Wait(reason string) error { return fmt.Errorf("%w: %s", ErrWait, reason) }

// Refused wraps ErrRefused with a reason word.
func Refused(reason string) error { return fmt.Errorf("%w: %s", ErrRefused, reason) }

// Gate says whether this instance leads: the engine does nothing in a tick the gate refuses. The entitlements Leader satisfies it.
type Gate interface {
	Ensure(ctx context.Context) bool
}
