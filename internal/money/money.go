// Package money is the platform's money store: grant, adjust, reserve, settle, release and the balance read. It is pure database code (no HTTP, no
// module call) and it runs as the platform's own database identity, platform_app, which can INSERT into the ledger but can write no balance: the balance
// has exactly one writer, the ledger trigger, so a stored balance is always the sum of the ledger rows that explain it.
//
// ONE OPERATION IS ONE TRANSACTION. Every method has an In twin that runs inside a pgx.Tx the CALLER owns (the job engine confirms a job in its own
// transaction through ReserveIn); the pool methods begin a READ COMMITTED transaction, call the In logic, commit, and roll back on any error. On any
// error from an In function the caller must roll its transaction back: a database error aborts it, and a refusal that happens after a write (there is none
// by design: every refusal below is decided before the first write) would otherwise leave that write standing.
//
// SERIALISING. Every operation on the money of one (user, module) takes the transaction-scoped advisory lock LockKey(user, module) FIRST (the platform's
// band is 7_000_000_000..7_999_999_999) and then READS the state it decides on, so two operations on one balance run one after the other and the second
// sees the first's effect: that is what gives a caller a clean ErrInsufficientCredits{Shortfall} instead of a CHECK violation. The roles cannot
// SELECT ... FOR UPDATE the balance (they hold SELECT only, by design), which is why the serialiser is an advisory lock. An operation takes exactly ONE lock
// (never two), so the store cannot deadlock with itself. The database's CHECKs and triggers stay as the second wall, and the tests prove each wall alone.
//
// ISOLATION: the In functions assume the caller's transaction is READ COMMITTED (the pool methods begin one that is): "take the lock, then read" only reads the state
// the lock holder left behind if every statement takes a fresh snapshot, which REPEATABLE READ and SERIALIZABLE do not.
//
// LOCK ORDER FOR CALLERS OF ReserveIn: the advisory lock is taken first and the job row (SELECT ... FOR UPDATE) second. A caller that locks the job row before
// calling ReserveIn must be the only writer of that job, or two such callers can deadlock (Postgres then aborts one with 40P01; nothing is written).
//
// WHAT THIS PACKAGE NEVER DOES: no float anywhere (credits are int64, unit prices int), no unqualified table name (every statement says platform.*), no
// application clock (every time is the database's now()), no string-built SQL (every value is a parameter), and no connection string or password in an error.
package money

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

// The advisory-lock band of the platform: 7_000_000_000 + (hash mod 10^9). Module 3 holds 8_xxx_xxx_xxx, module 2 holds 9_xxx_xxx_xxx (CLAUDE.md rule 9).
const (
	lockBand int64 = 7_000_000_000
	lockSpan       = 1_000_000_000
)

// lockKey is the advisory-lock key of the money of one (user, module): the FNV-1a 64-bit hash of "<user_id>|<module_id>" reduced into the platform's band.
// The ORDER of the pair matters ("a|b" and "b|a" are different keys). The ids must be in canonical form (lower-case uuid, module id as stored): the
// store validates that before it asks for a key, because two spellings of one user would be two locks.
func lockKey(user, module string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(user + "|" + module))
	return lockBand + int64(h.Sum64()%lockSpan)
}

// LockKey is lockKey, exported so a test (or an operator) can take the very lock the store takes and hold every operation on that balance behind it.
func LockKey(user, module string) int64 { return lockKey(user, module) }

// Store is the money store. It holds a pool and nothing else, so it is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	dsn  string // kept only to scrub error text (dsnscrub); never logged, never returned
}

// New builds the store over the platform's pool (a login as platform_app).
func New(pool *pgxpool.Pool) *Store {
	s := &Store{pool: pool}
	if pool != nil {
		s.dsn = pool.Config().ConnString()
	}
	return s
}

// run is the pool methods' transaction: BEGIN (read committed), fn, COMMIT; ROLLBACK on any error or panic. The rollback uses a context that outlives
// the caller's cancellation, so a cancelled request still releases its locks at once.
func (s *Store) run(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return s.fail(err, pgCtx{})
	}
	defer func() {
		rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = tx.Rollback(rb) // a no-op after a successful Commit (pgx.ErrTxClosed)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return s.fail(err, pgCtx{})
	}
	return nil
}

// fail turns a database error into the store's error: a known constraint becomes its named error, anything else is returned with the connection string and
// password taken out of its text (and still unwrappable, so errors.Is(err, context.Canceled) works).
func (s *Store) fail(err error, c pgCtx) error {
	if err == nil {
		return nil
	}
	if mapped := mapPgError(err, c); mapped != nil {
		return mapped
	}
	return dsnscrub.Scrub(err, s.dsn)
}

// lock takes the (user, module) advisory lock for the rest of the transaction. It waits for any other holder.
func (s *Store) lock(ctx context.Context, tx pgx.Tx, user, module string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey(user, module)); err != nil {
		return s.fail(err, pgCtx{})
	}
	return nil
}

// scanPrices reads rows of (unit, cost) and closes them; errors raised while the statement runs (a constraint, a trigger) surface here.
func (s *Store) scanPrices(rows pgx.Rows, c pgCtx) ([]price, error) {
	defer rows.Close()
	var out []price
	for rows.Next() {
		var p price
		if err := rows.Scan(&p.unit, &p.cost); err != nil {
			return nil, s.fail(err, c)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(err, c)
	}
	return out, nil
}

// price is one unit and its cost in credits.
type price struct {
	unit string
	cost int32
}

func priceMap(ps []price) map[string]int {
	m := make(map[string]int, len(ps))
	for _, p := range ps {
		m[p.unit] = int(p.cost)
	}
	return m
}
