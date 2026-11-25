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
	"github.com/coldmail-platform/platform/internal/money"
)

// THE REACHABILITY SYNC (Phase 4, chunk B3; Docs/phase-4-spec.md, Locked 23). Whether module 1's vendor unit (m1_vendor, 2 credits) can be charged at all depends on a switch in MODULE 1's own configuration
// (its TM_EXTERNAL_* dials, shipped off). The platform cannot read that switch, so the owner tells it the same thing in PLATFORM_M1_VENDOR_REACHABLE, and this loop writes the answer into the one place the
// money store reads it, platform.unit_costs.reachable, through the one writer (money.Store.SetUnitReachable): at start and then every minute, in EVERY instance. It is idempotent (a call that finds the flag as
// it should be writes nothing, not even a history row), so any number of instances with one configuration make it harmlessly; that is the reason the dial's documentation says all instances share one value.

const (
	// reachabilityUnit is the unit the dial mirrors: module 1's vendor unit, the only unit whose reachability the owner decides.
	reachabilityUnit = "m1_vendor"
	// reachabilitySource is what the history of the change says who said so: the configuration, by the name of the variable.
	reachabilitySource = "config: " + config.VarM1VendorReachable

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
	msgReachabilitySynced    = "unit reachability synced from configuration"
	msgReachabilityFailed    = "unit reachability sync failed; it retries"
	msgReachabilityRecovered = "unit reachability sync succeeded again"
	msgReachabilityMoved     = "unit reachability was changed by someone else and has been restored from configuration"
)

// reachabilitySetter is the part of the money store the loop calls.
type reachabilitySetter interface {
	SetUnitReachable(ctx context.Context, unit string, reachable bool, source string) (changed bool, err error)
}

// newReachabilitySetter builds what the loop writes through: the money store over the platform's pool, waiting for a reserve in progress as long as PLATFORM_MONEY_LOCK_TIMEOUT_MS says. It is a variable only
// so that a test can supply a fake (and see which pool it was handed); nothing else assigns it.
var newReachabilitySetter = func(pool *pgxpool.Pool, cfg config.Config) reachabilitySetter {
	return money.New(pool, money.WithLockTimeout(cfg.MoneyLockTimeout))
}

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

// syncReachability runs until ctx is done. Each turn writes the dial's value (reachable) for the vendor unit; the next turn is reachabilityRetry later while no write has yet succeeded and
// reachabilityEvery later once one has (a failure after that waits for the next turn). The service starts without a database, so a failure is never fatal: it is logged at warn ONCE per class of
// failure (a database outage costs one line, not one per turn), recovery is announced once, and a turn cut off by the shutdown is no failure at all. A write that CHANGES the flag after the first success
// means somebody else moved it (another instance given a different value, or a hand-written UPDATE): it has been put back, and says so at warn on every such turn.
func syncReachability(ctx context.Context, set reachabilitySetter, reachable bool, log *slog.Logger, wait waitFunc) {
	log.InfoContext(ctx, msgReachabilityStarted, "unit", reachabilityUnit, "reachable", reachable, "every", reachabilityEvery.String())
	var (
		synced    bool   // a write has succeeded at least once
		lastClass string // the failure class last reported; "" while healthy
	)
	for ctx.Err() == nil {
		changed, err := reachabilityPass(ctx, set, reachable)
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
				log.WarnContext(ctx, msgReachabilityMoved, "unit", reachabilityUnit, "reachable", reachable)
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

// reachabilityPass is one write, bounded by its own timeout, in which a panic of the setter is an error and not the end of the process.
func reachabilityPass(parent context.Context, set reachabilitySetter, reachable bool) (changed bool, err error) {
	ctx, cancel := context.WithTimeout(parent, reachabilityPassTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			changed, err = false, &setterPanic{typ: fmt.Sprintf("%T", p)}
		}
	}()
	return set.SetUnitReachable(ctx, reachabilityUnit, reachable, reachabilitySource)
}

// setterPanic is a panic of the setter turned into an error. It carries the TYPE of the panic value and never the value, which is not known to be safe to print.
type setterPanic struct{ typ string }

func (e *setterPanic) Error() string { return "the setter panicked (" + e.typ + ")" }

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
