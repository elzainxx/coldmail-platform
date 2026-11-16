package money

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// HOW LONG A MONEY OPERATION WAITS (Phase 2 close-out D8, finding F35). Every operation on the money of one (user, module) takes the advisory lock LockKey first, and before this
// change it waited for a holder WITHOUT A BOUND: one stuck transaction (a caller that opened a transaction through an In twin and never finished it, a backend that hung)
// held every other operation on that balance for as long as it lived. Now every money transaction begins with SET LOCAL lock_timeout, so a wait that runs out fails with
// PostgreSQL's lock_not_available (55P03), which the store reports as ErrLockTimeout.
//
// The setting is a configuration dial (PLATFORM_MONEY_LOCK_TIMEOUT_MS, Docs/config-reference.md) handed to the store with WithLockTimeout. A store built without the option
// (and the zero value of Store) uses DefaultLockTimeout: the wait is NEVER unbounded and never 0, which PostgreSQL would read as "no timeout".

// DefaultLockTimeout is the wait of a store that was not given one. It equals the default of PLATFORM_MONEY_LOCK_TIMEOUT_MS.
const DefaultLockTimeout = 10 * time.Second

// maxLockTimeoutMS is the largest lock_timeout PostgreSQL accepts (an int4 of milliseconds); a longer wait is clamped to it, never wrapped round to a small or negative one.
const maxLockTimeoutMS = math.MaxInt32

// Option configures a Store at New.
type Option func(*Store)

// WithLockTimeout sets how long an operation waits for the (user, module) lock, and for any row another transaction holds, before it gives up with ErrLockTimeout. The wait is
// applied in whole milliseconds, rounded UP, and is at least 1 ms. A zero or negative d is ignored: the store keeps DefaultLockTimeout, because "no timeout" is not an option.
func WithLockTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.lockTimeout = d
		}
	}
}

// LockTimeout is the wait the store applies to every money transaction: what WithLockTimeout set, or DefaultLockTimeout. It is never 0 and never negative.
func (s *Store) LockTimeout() time.Duration {
	if s == nil || s.lockTimeout <= 0 {
		return DefaultLockTimeout
	}
	return s.lockTimeout
}

// lockTimeoutMS is LockTimeout in the whole milliseconds handed to the database: rounded up, at least 1 (0 would switch the timeout off), at most maxLockTimeoutMS.
func (s *Store) lockTimeoutMS() int64 {
	d := s.LockTimeout()
	ms := int64(d / time.Millisecond)
	if d%time.Millisecond != 0 {
		ms++
	}
	return min(max(ms, 1), maxLockTimeoutMS)
}

// lockTimeoutSetting is the value of the lock_timeout setting: whole milliseconds with the unit spelled out ("300ms"). It is built from a number, never from input, and it
// travels as a bind parameter of set_config, never inside the statement's text.
func (s *Store) lockTimeoutSetting() string { return strconv.FormatInt(s.lockTimeoutMS(), 10) + "ms" }

// bound is the FIRST statement of every money transaction: SET LOCAL lock_timeout (set_config with is_local = true), so every wait of the transaction that follows (the advisory
// lock, a job row, the price rows, a balance row) is bounded by the store's wait, and the setting ends with the transaction instead of staying on the pooled connection.
//
// In a transaction the CALLER owns (the In twins) it applies to the rest of that transaction too: the caller's later statements run under the same wait.
func (s *Store) bound(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", s.lockTimeoutSetting()); err != nil {
		return s.fail(err, pgCtx{})
	}
	return nil
}

// lockUser is lock, and a wait that ran out says whose credits it was waiting for. lock itself is the single statement that takes the advisory lock; keeping it free of the
// error plumbing keeps the fault proofs (ci/faults) that remove or replace that one statement applying.
func (s *Store) lockUser(ctx context.Context, tx pgx.Tx, user, module string) error {
	err := s.lock(ctx, tx, user, module)
	var lt *LockTimeoutError
	if errors.As(err, &lt) {
		lt.User, lt.Module = user, module
	}
	return err
}
