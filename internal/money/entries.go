package money

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GrantReq adds credits: an admin gives Amount credits of Module to UserID, for a Reason, under an idempotency Key.
type GrantReq struct {
	UserID  string
	Module  string // m1 or m2
	Amount  int64  // credits to ADD, 1 .. 2^53
	AdminID string // the named admin (platform.admins)
	Reason  string // non-blank, at most 1000 bytes after trimming
	Key     string // non-blank, at most 200 bytes; the same key with the same request is a replay
}

// AdjustReq removes credits (a clawback): the same shape as a grant, and Amount is the credits to REMOVE (> 0). It removes AVAILABLE credits only: reserved
// credits belong to jobs and are never touched, so an adjustment larger than the available balance is ErrInsufficientCredits.
type AdjustReq struct {
	UserID  string
	Module  string
	Amount  int64
	AdminID string
	Reason  string
	Key     string
}

// Entry is the ledger row of a grant or an adjustment. Amount is always positive: credits added by a grant, removed by an adjustment. Replayed is true
// when the same request had already been written and this is that earlier row, returned again, with nothing written now.
type Entry struct {
	ID        int64
	UserID    string
	Module    string
	Kind      string // "grant" or "adjustment"
	Amount    int64
	AdminID   string
	Reason    string
	Key       string
	CreatedAt time.Time
	Replayed  bool
}

const (
	kindGrant      = "grant"
	kindAdjustment = "adjustment"
)

// Grant adds credits in ONE transaction. It is idempotent on (user, module, Key): the same request again returns the first row (Replayed), a DIFFERENT
// request under the same key is ErrIdempotencyMismatch and writes nothing. A disabled admin is ErrAdminDisabled (the database refuses it), an unknown one
// ErrAdminUnknown, module m3 ErrNotMetered; a malformed request is an InvalidError and never reaches the database.
func (s *Store) Grant(ctx context.Context, req GrantReq) (Entry, error) {
	r, err := validateEntry(kindGrant, req.UserID, req.Module, req.Amount, req.AdminID, req.Reason, req.Key)
	if err != nil {
		return Entry{}, opErr("grant", err)
	}
	return s.entry(ctx, "grant", r)
}

// GrantIn is Grant inside a transaction the caller owns.
func (s *Store) GrantIn(ctx context.Context, tx pgx.Tx, req GrantReq) (Entry, error) {
	r, err := validateEntry(kindGrant, req.UserID, req.Module, req.Amount, req.AdminID, req.Reason, req.Key)
	if err != nil {
		return Entry{}, opErr("grant", err)
	}
	e, err := s.writeEntry(ctx, tx, r)
	return e, opErr("grant", err)
}

// Adjust removes credits in ONE transaction. Under the (user, module) lock it first looks for the same key (a replay returns the first row even when the
// credits it removed are gone), then reads the available balance: Amount greater than available is ErrInsufficientCredits{Available, Needed: Amount,
// Shortfall} and nothing changes. The database writes the admin_audit row in the same transaction.
func (s *Store) Adjust(ctx context.Context, req AdjustReq) (Entry, error) {
	r, err := validateEntry(kindAdjustment, req.UserID, req.Module, req.Amount, req.AdminID, req.Reason, req.Key)
	if err != nil {
		return Entry{}, opErr("adjust", err)
	}
	return s.entry(ctx, "adjust", r)
}

// AdjustIn is Adjust inside a transaction the caller owns.
func (s *Store) AdjustIn(ctx context.Context, tx pgx.Tx, req AdjustReq) (Entry, error) {
	r, err := validateEntry(kindAdjustment, req.UserID, req.Module, req.Amount, req.AdminID, req.Reason, req.Key)
	if err != nil {
		return Entry{}, opErr("adjust", err)
	}
	e, err := s.writeEntry(ctx, tx, r)
	return e, opErr("adjust", err)
}

func (s *Store) entry(ctx context.Context, op string, r entryReq) (Entry, error) {
	var e Entry
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		e, err = s.writeEntry(ctx, tx, r)
		return err
	})
	if err != nil {
		return Entry{}, opErr(op, err)
	}
	return e, nil
}

// entryRow is a ledger row of kind grant or adjustment as read back for a replay.
type entryRow struct {
	id        int64
	kind      string
	delta     int64
	adminID   string
	reason    string
	createdAt time.Time
}

// writeEntry is the whole of a grant or an adjustment, inside the caller's transaction.
func (s *Store) writeEntry(ctx context.Context, tx pgx.Tx, r entryReq) (Entry, error) {
	if err := s.bound(ctx, tx); err != nil {
		return Entry{}, err
	}
	if err := s.lockUser(ctx, tx, r.userID, r.module); err != nil {
		return Entry{}, err
	}
	var c pgCtx
	delta := r.amount
	if r.kind == kindAdjustment {
		delta = -r.amount
		// A replay comes first: the credits the first call removed may be gone, and the second call must still say "done", not "insufficient".
		row, found, err := s.entryByKey(ctx, tx, r)
		if err != nil {
			return Entry{}, err
		}
		if found {
			return s.replayOf(r, row)
		}
		bal, err := s.readBalance(ctx, tx, r.userID, r.module)
		if err != nil {
			return Entry{}, err
		}
		if bal.Available < r.amount {
			return Entry{}, ErrInsufficientCredits{Available: bal.Available, Needed: r.amount, Shortfall: r.amount - bal.Available}
		}
		c = pgCtx{available: bal.Available, needed: r.amount}
	}

	const insert = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1::uuid, $2, $3, $4, 0, $5::uuid, $6, $7)
		ON CONFLICT (user_id, module_id, idempotency_key) DO NOTHING
		RETURNING id, created_at`
	e := Entry{UserID: r.userID, Module: r.module, Kind: r.kind, Amount: r.amount, AdminID: r.adminID, Reason: r.reason, Key: r.key}
	err := tx.QueryRow(ctx, insert, r.userID, r.module, r.kind, delta, r.adminID, r.reason, r.key).Scan(&e.ID, &e.CreatedAt)
	switch {
	case err == nil:
		return e, nil
	case errors.Is(err, pgx.ErrNoRows):
		// ON CONFLICT DO NOTHING wrote nothing: the key is taken. Compare what holds it with this request.
		row, found, err := s.entryByKey(ctx, tx, r)
		if err != nil {
			return Entry{}, err
		}
		if !found {
			return Entry{}, fmt.Errorf("%w: the insert reported a conflict but no row holds the key", ErrConflict)
		}
		return s.replayOf(r, row)
	}
	return Entry{}, s.fail(err, c)
}

// entryByKey reads the ledger row that holds this request's idempotency key, whatever kind it is.
func (s *Store) entryByKey(ctx context.Context, tx pgx.Tx, r entryReq) (entryRow, bool, error) {
	const q = `SELECT id, kind, available_delta, COALESCE(acting_admin_id::text, ''), COALESCE(reason, ''), created_at
		FROM platform.ledger WHERE user_id = $1::uuid AND module_id = $2 AND idempotency_key = $3`
	var row entryRow
	err := tx.QueryRow(ctx, q, r.userID, r.module, r.key).Scan(&row.id, &row.kind, &row.delta, &row.adminID, &row.reason, &row.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return entryRow{}, false, nil
	}
	if err != nil {
		return entryRow{}, false, s.fail(err, pgCtx{})
	}
	return row, true, nil
}

// replayOf judges a row that already holds the key: the SAME request (kind, amount, admin, reason) is a replay, anything else is a mismatch, and the
// first request stands either way.
func (s *Store) replayOf(r entryReq, row entryRow) (Entry, error) {
	want := r.amount
	if r.kind == kindAdjustment {
		want = -r.amount
	}
	if row.kind != r.kind || row.delta != want || row.adminID != r.adminID || row.reason != r.reason {
		return Entry{}, fmt.Errorf("%w: key %q already holds a %s of %d credit(s) by admin %s, not this %s of %d by admin %s", ErrIdempotencyMismatch,
			clip(r.key), row.kind, abs(row.delta), row.adminID, r.kind, r.amount, r.adminID)
	}
	return Entry{ID: row.id, UserID: r.userID, Module: r.module, Kind: row.kind, Amount: r.amount, AdminID: row.adminID, Reason: row.reason, Key: r.key,
		CreatedAt: row.createdAt, Replayed: true}, nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
