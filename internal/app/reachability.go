package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// THE REACHABILITY SYNC (Phase 4, chunk B3, source changed by C3; Docs/phase-4-spec.md, Locked 23). Whether module 1's vendor unit (m1_vendor, 2 credits) can be charged at all depends on a switch in MODULE 1's
// own configuration (its TM_EXTERNAL_* dials, shipped off), and module 1 says so itself: GET /v1/describe carries vendor_fallback.reachable, computed per request from the very predicate its external stage
// uses. This loop reads that answer (the provider internal/app builds over the module 1 adapter: m1Reachability, the same one the job store reads at every module 1 preview and confirm) and writes it into
// the one place the money store reads it, platform.unit_costs.reachable, through the one writer (money.Store.SetUnitReachable, source money.ReachableSourceM1Describe): at start and then every minute, in
// EVERY instance. It is idempotent (a call that finds the flag as module 1 says writes nothing, not even a history row), so any number of instances make it harmlessly. A describe that cannot be read (module
// 1 is down, refuses the key, or does not say) NEVER writes: the stored flag stands, the failure is logged once per class and the loop retries.
//
// There is no setting for any of this: the platform dial that used to carry the answer is removed (config.RemovedVars), and module 1's own TM_EXTERNAL_* dials are the one source of truth.

const (
	// reachabilityUnit is the unit module 1's answer decides: its vendor unit, the only unit whose reachability the platform learns from a module.
	reachabilityUnit = money.UnitM1Vendor
	// reachabilitySource is what the history of the change says who said so: module 1's own describe.
	reachabilitySource = money.ReachableSourceM1Describe

	// reachabilityEvery is the wait between two writes once the first has succeeded; reachabilityRetry is the wait after a failure before the first success. They are pacing, not dials: a flag that was
	// set by hand in the database is restored within the minute, and a database that comes up late is found within five seconds.
	reachabilityEvery = 60 * time.Second
	reachabilityRetry = 5 * time.Second
	// reachabilityPassTimeout bounds one write: the store's own lock wait (PLATFORM_MONEY_LOCK_TIMEOUT_MS) is shorter by default, and a database that never answers must not hold the loop for ever.
	reachabilityPassTimeout = 30 * time.Second
)

// The loop's log lines. They carry the unit, the flag and a failure CLASS, never the driver's text (a connection error names the host and the login) and never a connection string.
const (
	msgReachabilityStarted   = "reachability sync started"
	msgReachabilitySynced    = "unit reachability read from module 1"
	msgReachabilityFailed    = "unit reachability could not be read from module 1 or written; the stored flag stands and it retries"
	msgReachabilityRecovered = "unit reachability sync succeeded again"
	msgReachabilityFollowed  = "unit reachability followed module 1"
)

// reachabilitySetter is the part of the money store the loop calls.
type reachabilitySetter interface {
	SetUnitReachable(ctx context.Context, unit string, reachable bool, source string) (changed bool, err error)
}

// reachabilityReader is what the loop reads module 1's answer through: jobs.VendorReachability, the one provider (the job store reads the very same one before a preview and a confirm).
type reachabilityReader = jobs.VendorReachability

// errNotSaid is the failure of a describe that answered and did not say (an older module 1, whose describe has no vendor_fallback): nothing is written for it.
var errNotSaid = errors.New("module 1 does not say whether its vendor fallback is reachable")

// newReachabilitySetter builds what the loop writes through. By default that is THE money store of the process (the one the job store is built over: newMoneyStore, apiDeps.Money), so the loop
// waits for a reserve in progress exactly as long as PLATFORM_MONEY_LOCK_TIMEOUT_MS says and there is one money store, not one per consumer. It is a variable only so that a test can supply a
// fake (and see which pool and which store it was handed); nothing else assigns it.
var newReachabilitySetter = func(_ *pgxpool.Pool, mny *money.Store, _ config.Config) reachabilitySetter { return mny }

// waitFunc waits for d and reports true, or returns false as soon as ctx is done. It is the loop's only clock, injected so that a test drives the rhythm without waiting for it.
type waitFunc func(ctx context.Context, d time.Duration) bool

// sleepContext is the real waitFunc.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return ctx.Err() == nil
	}
}

// syncReachability runs until ctx is done. Each turn reads module 1's answer (src) and writes it for the vendor unit; the next turn is reachabilityRetry later while no write has yet succeeded and
// reachabilityEvery later once one has (a failure after that waits for the next turn). The service starts without a database and without module 1, so a failure is never fatal: it is logged at warn ONCE per
// class of failure (an outage costs one line, not one per turn), recovery is announced once, and a turn cut off by the shutdown is no failure at all. A read that fails, or that does not say, never writes.
// A write that CHANGES the flag after the first success means module 1's answer changed (its owner switched a TM_EXTERNAL_* dial and restarted it, or another instance or a preview got there first with
// the opposite, which cannot be: every writer writes module 1's answer): it is logged at info on every such turn.
func syncReachability(ctx context.Context, src reachabilityReader, set reachabilitySetter, log *slog.Logger, wait waitFunc) {
	log.InfoContext(ctx, msgReachabilityStarted, "unit", reachabilityUnit, "source", reachabilitySource, "every", reachabilityEvery.String())
	var (
		synced    bool   // a write has succeeded at least once
		lastClass string // the failure class last reported; "" while healthy
	)
	for ctx.Err() == nil {
		reachable, changed, err := reachabilityPass(ctx, src, set)
		switch {
		case err == nil:
			if lastClass != "" {
				log.InfoContext(ctx, msgReachabilityRecovered, "unit", reachabilityUnit)
				lastClass = ""
			}
			switch {
			case !synced:
				synced = true
				log.InfoContext(ctx, msgReachabilitySynced, "unit", reachabilityUnit, "reachable", reachable, "changed", changed)
			case changed:
				log.InfoContext(ctx, msgReachabilityFollowed, "unit", reachabilityUnit, "reachable", reachable)
			}
		case ctx.Err() != nil:
			return // cut off by the shutdown: not a failure worth a line
		default:
			if class := reachabilityClass(err); class != lastClass {
				lastClass = class
				log.WarnContext(ctx, msgReachabilityFailed, "unit", reachabilityUnit, "reason", class)
			}
		}
		d := reachabilityEvery
		if !synced {
			d = reachabilityRetry
		}
		if !wait(ctx, d) {
			return
		}
	}
}

// reachabilityPass is one read and one write, bounded by one timeout, in which a panic of the reader or the setter is an error and not the end of the process. The write happens only after a read that
// answered: an error, or an answer that is not one (known false), is returned before the setter is called.
func reachabilityPass(parent context.Context, src reachabilityReader, set reachabilitySetter) (reachable, changed bool, err error) {
	ctx, cancel := context.WithTimeout(parent, reachabilityPassTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			reachable, changed, err = false, false, &setterPanic{typ: fmt.Sprintf("%T", p)}
		}
	}()
	r, known, err := src.M1VendorReachable(ctx)
	if err != nil {
		return false, false, err
	}
	if !known {
		return false, false, errNotSaid
	}
	changed, err = set.SetUnitReachable(ctx, reachabilityUnit, r, reachabilitySource)
	if err != nil {
		return false, false, err
	}
	return r, changed, nil
}

// setterPanic is a panic of the reader or the setter turned into an error. It carries the TYPE of the panic value and never the value, which is not known to be safe to print.
type setterPanic struct{ typ string }

func (e *setterPanic) Error() string {
	return "the reachability read or write panicked (" + e.typ + ")"
}

// reachabilityClass names the class of a failure WITHOUT its text: the text of a connection error carries the host, the login and the database name, which have no business in a log line. An error of
// the store's own is named by what it says (another transaction held the unit row for too long; the unit is not priced), an infrastructure error by its kind, anything else by the Go type of the innermost error.
func reachabilityClass(err error) string {
	var (
		pe  *pgconn.PgError
		ce  *pgconn.ConnectError
		dns *net.DNSError
		ne  net.Error
		sp  *setterPanic
	)
	switch {
	case err == nil:
		return ""
	case errors.As(err, &sp):
		return sp.Error()
	case errors.Is(err, errNotSaid):
		return "module 1 does not say"
	case modclient.KindOf(err) != 0:
		return "module 1 " + modclient.KindOf(err).String() // the kind of the call's failure, never its text (a transport error's names an address)
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, money.ErrLockTimeout):
		return "waited too long for a lock on the unit"
	case errors.Is(err, money.ErrUnknownUnit):
		return "the unit is not priced"
	case errors.Is(err, money.ErrInvalid):
		return "refused as malformed"
	case errors.As(err, &pe):
		return "database error " + pe.Code
	case errors.As(err, &dns):
		return "dns lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(strings.ToLower(err.Error()), "refused"):
		return "connection refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &ce):
		return "database connection failed"
	}
	root := err
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	return fmt.Sprintf("error (%T)", root)
}
