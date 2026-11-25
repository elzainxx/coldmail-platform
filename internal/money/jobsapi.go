package money

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// What the job store (internal/jobs) needs of this package and must not copy: the lock it takes first in every transaction of one (user, module), and the reserve rate a
// preview shows. Both are the SAME code the reserve runs, so a preview and the reserve it announces cannot drift apart.

// LockIn takes the (user, module) advisory lock inside a transaction the caller owns, exactly as every money operation does: SET LOCAL lock_timeout to the store's wait (bound),
// then pg_advisory_xact_lock(LockKey(user, module)). A wait that runs out is ErrLockTimeout naming whose credits it was. The lock is held until the caller's transaction ends and is
// re-entrant within it, so the money operations the caller calls afterwards (ReserveIn, ReleaseIn) take it again at no cost. The module may be any module id of platform.modules,
// metered or not: the lock serialises every writer of one user's one module (module 3 included), and module 3 holds no credits.
//
// The caller's rule (see ReserveIn): the advisory lock first, the job row second.
func (s *Store) LockIn(ctx context.Context, tx pgx.Tx, user, module string) error {
	if err := checkUser("user_id", user); err != nil {
		return opErr("lock", err)
	}
	if !moduleIDShape(module) {
		return opErr("lock", invalid("module", "must be a module id (lower-case letters, digits and underscore), got %q", clip(module)))
	}
	if err := s.bound(ctx, tx); err != nil {
		return opErr("lock", err)
	}
	return opErr("lock", s.lockUser(ctx, tx, user, module))
}

// BoundIn is the first statement of every money transaction, exported for a caller whose own transaction waits for rows (a job's row) and should wait no longer than the store's lock wait:
// SET LOCAL lock_timeout to the store's wait (PLATFORM_MONEY_LOCK_TIMEOUT_MS, WithLockTimeout). A wait that runs out then fails with PostgreSQL's lock_not_available. The setting ends with the
// caller's transaction.
func (s *Store) BoundIn(ctx context.Context, tx pgx.Tx) error {
	return opErr("bound", s.bound(ctx, tx))
}

// moduleIDShape is the CHECK modules_id_shape of platform.modules: ^[a-z][a-z0-9_]*$.
func moduleIDShape(m string) bool {
	if m == "" || m[0] < 'a' || m[0] > 'z' {
		return false
	}
	for i := 1; i < len(m); i++ {
		c := m[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// RateQuote is what a reserve on a module would freeze right now: the reachable units and their prices (the snapshot) and the reserve rate, the dearest of them.
type RateQuote struct {
	Unit     string         // the unit whose price is the reserve rate: the dearest REACHABLE unit; a tie goes to the lexicographically first unit
	UnitCost int            // credits per unit at that rate
	Prices   map[string]int // every reachable unit of the module and its price: what ReserveIn copies into the job's price snapshot
}

// ReserveRateIn reads the module's reserve rate in a transaction the caller owns: the REACHABLE units (platform_0013) and their prices, and the dearest of them by the rule ReserveIn uses
// (a tie goes to the lexicographically first unit). It writes nothing and takes no advisory lock. With lockPrices true it first takes the same row locks ReserveIn takes (every unit price
// row of the module, FOR SHARE), so that a price change or a flip of the reachable flag waits until the caller's transaction ends: a caller that compares what it reads here with a reserve
// it makes in the same transaction (the confirm of a job) then reserves at exactly the prices it compared. A module with no reachable unit is ErrSnapshot (the reserve would refuse it too).
func (s *Store) ReserveRateIn(ctx context.Context, tx pgx.Tx, module string, lockPrices bool) (RateQuote, error) {
	if !moduleIDShape(module) {
		return RateQuote{}, opErr("rate", invalid("module", "must be a module id (lower-case letters, digits and underscore), got %q", clip(module)))
	}
	if lockPrices {
		// the statement of ReserveIn's step 6: platform_app holds a column-level UPDATE on unit_costs, which is what a row lock needs
		rows, err := tx.Query(ctx, `SELECT unit FROM platform.unit_costs WHERE module_id = $1 ORDER BY unit FOR SHARE`, module)
		if err != nil {
			return RateQuote{}, opErr("rate", s.fail(err, pgCtx{}))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return RateQuote{}, opErr("rate", s.fail(err, pgCtx{}))
		}
	}
	rows, err := tx.Query(ctx, `SELECT unit, cost FROM platform.unit_costs WHERE module_id = $1 AND reachable ORDER BY unit`, module)
	if err != nil {
		return RateQuote{}, opErr("rate", s.fail(err, pgCtx{}))
	}
	ps, err := s.scanPrices(rows, pgCtx{})
	if err != nil {
		return RateQuote{}, opErr("rate", err)
	}
	if len(ps) == 0 {
		return RateQuote{}, opErr("rate", fmt.Errorf("%w: module %s has no reachable unit price to freeze", ErrSnapshot, module))
	}
	d := dearest(ps)
	return RateQuote{Unit: d.unit, UnitCost: int(d.cost), Prices: priceMap(ps)}, nil
}
