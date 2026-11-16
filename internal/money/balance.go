package money

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Balance is a user's credits on one metered module. Exists is false when the user has no balance row yet (nothing was ever written for them: both
// amounts are 0).
type Balance struct {
	Available int64
	Reserved  int64
	Exists    bool
}

// queryRower is what a balance read needs; the pool and a transaction both satisfy it.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Balance reads the user's balance on a metered module (m1 or m2). It takes no lock: it is a read, and the value is the committed one at that instant.
func (s *Store) Balance(ctx context.Context, userID, module string) (Balance, error) {
	if err := validateBalance(userID, module); err != nil {
		return Balance{}, opErr("balance", err)
	}
	b, err := s.readBalance(ctx, s.pool, userID, module)
	return b, opErr("balance", err)
}

// BalanceIn is Balance inside a transaction the caller owns (it sees that transaction's own writes).
func (s *Store) BalanceIn(ctx context.Context, tx pgx.Tx, userID, module string) (Balance, error) {
	if err := validateBalance(userID, module); err != nil {
		return Balance{}, opErr("balance", err)
	}
	b, err := s.readBalance(ctx, tx, userID, module)
	return b, opErr("balance", err)
}

func (s *Store) readBalance(ctx context.Context, q queryRower, userID, module string) (Balance, error) {
	var b Balance
	err := q.QueryRow(ctx, "SELECT available, reserved FROM platform.balances WHERE user_id = $1::uuid AND module_id = $2", userID, module).Scan(&b.Available, &b.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, nil
	}
	if err != nil {
		return Balance{}, s.fail(err, pgCtx{})
	}
	b.Exists = true
	return b, nil
}
