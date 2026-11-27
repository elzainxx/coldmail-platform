// Package jobs is the job store: the ONE package that writes platform.jobs (status and the lifecycle columns), platform.job_rows, platform.job_results and platform.job_events
// (scripts/check-job-writers.sh keeps it so). The API and the job engine call it; neither writes those tables, and neither passes it a user id it did not get from the authenticated
// principal. Docs/phase-4-spec.md, "Locked here" 21-28.
//
// WHAT IT DOES. CreateDraft makes a job and classifies its rows once (pending, malformed, duplicate, or, for a send chained to a verify job, pending or excluded by the source verdict);
// Preview recomputes what the list will cost and a fingerprint of everything the numbers depended on; Confirm is ONE transaction that checks the fingerprint, applies the user's
// suppressions, reserves the credits (money.ReserveIn), takes the slot, moves the job to confirmed or scheduled and writes the platform's own results; the moves that need no module
// (discard, reopen, cancel before start, pause and resume a scheduled job) and the reads (Get, List, Events) complete the surface.
//
// EVERY STATEMENT IS SCOPED BY THE USER. Every method takes userID, the authenticated principal, and every statement carries `user_id = $n`: a job of another user is ErrNotFound, the same
// error as a job that does not exist. A user id that is not the canonical lower-case uuid of a customer is refused before the database is asked (two spellings of one user would be two
// advisory locks).
//
// WHERE THE RULES LIVE. The lifecycle is the database's: every change of status is `platform.job_transition(...)` or an UPDATE preceded by the three transaction-local settings (actor kind,
// actor id, reason), the guard (platform_0011) refuses a move that is not a row of platform.job_transitions, taken by an actor that row does not allow, or beyond what the module can do,
// and its refusal arrives here as ErrNotAllowed carrying the constraint's name. The slot (one active job per user and module) is the partial unique index
// jobs_one_active_per_user_module: the store does NOT check it in Go, the index decides, so that a confirm that loses leaves nothing behind (the whole transaction rolls back, the reserve
// with it). A confirm key is unique per user (jobs_confirm_key_key).
//
// SERIALISING. Confirm and CancelBeforeStart take the (user, module) advisory lock FIRST (money.Store.LockIn: the same key every money writer of that pair takes), then the job row
// (SELECT ... FOR UPDATE), the order money documents for callers of ReserveIn. A module 3 pre-check (an HTTP call) is made BEFORE the transaction, never under a lock.
//
// TIME. Deadlines are the database's: the store passes durations and lets SQL add them to now(). The store's own clock (WithClock) only stamps a preview's informational computed_at.
//
// WHAT THIS PACKAGE NEVER DOES: no float anywhere, no unqualified table name, no string-built SQL around a value, and no address in an error, a log line or a refusal's reason.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/money"
)

// Store is the job store. It holds a pool, the money store and its settings and nothing else, so it is safe for concurrent use.
type Store struct {
	pool          *pgxpool.Pool
	mny           *money.Store
	dsn           string // kept only to scrub error text (dsnscrub); never logged, never returned
	log           *slog.Logger
	now           func() time.Time
	maxRows       int
	maxStartDelay time.Duration
	pre           PreChecks
	render        Renderer
	vendor        vendorHook                          // module 1's vendor reachability, read before a module 1 preview and confirm (vendor.go)
	unsub         func(itemID string) (string, error) // the unsubscribe link of an item (WithUnsubscribe, unsubscribe.go); nil: none is configured
	prep          Preparer                            // the template renderer (WithPreparer, template.go): when set it is preferred to render; nil: only the Renderer, no template work at all
}

// Option configures a Store at New.
type Option func(*Store)

// WithClock replaces the store's clock. It is for tests of NON-database time only: the database's now() stays the authority for every deadline (the store passes durations, SQL adds them),
// and the clock stamps nothing but a preview's informational computed_at and ages the few seconds the store remembers module 1's vendor reachability for (vendor.go).
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger sets the logger (nothing is logged by default). The store logs no address, no secret and no SQL.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) {
		if l != nil {
			s.log = l
		}
	}
}

// WithMaxRows sets the longest list a job may hold (default DefaultMaxRows). A value outside 1..HardMaxRows makes New fail.
func WithMaxRows(n int) Option { return func(s *Store) { s.maxRows = n } }

// WithMaxStartDelay sets how long after its start a confirmed or scheduled job may wait for the engine before it expires (default DefaultMaxStartDelay): start_deadline is
// coalesce(scheduled_start, now()) plus this, added by the database. A value outside (0, MaxStartDelayLimit] makes New fail.
func WithMaxStartDelay(d time.Duration) Option { return func(s *Store) { s.maxStartDelay = d } }

// WithPreChecks sets the module 3 pre-check a confirm of a module 3 job makes before its transaction. Without it (or with nil) no pre-check is made.
func WithPreChecks(p PreChecks) Option { return func(s *Store) { s.pre = p } }

// WithRenderer sets the per-row renderer EnsureJobItems and EnsureModuleItems write a send job's messages through (default PlainRenderer, the identity: the campaign's subject, body and text as given). It
// takes the place of any Preparer given before it (the last of the two options wins): a store with only a Renderer does no template work. A nil renderer is ignored.
func WithRenderer(r Renderer) Option {
	return func(s *Store) {
		if r != nil {
			s.render, s.prep = r, nil
		}
	}
}

// WithPreparer gives the store the template renderer of Phase 5 (TemplateRenderer): the campaign is compiled when a send job is created (a syntax error is a 400), the preview counts the rows that lack a
// required merge field and shows sample messages, the confirm leaves those rows out, free, and every message is rendered through it. It is preferred to the Renderer, whichever option came first, unless
// WithRenderer comes after it. A nil preparer is ignored.
func WithPreparer(p Preparer) Option {
	return func(s *Store) {
		if p != nil {
			s.prep = p
		}
	}
}

// New builds the store over the platform's pool (a login as platform_app) and its money store. It fails on a nil pool or money store and on a setting out of range.
func New(pool *pgxpool.Pool, mny *money.Store, opts ...Option) (*Store, error) {
	if pool == nil {
		return nil, errors.New("jobs.New: no database pool")
	}
	if mny == nil {
		return nil, errors.New("jobs.New: no money store")
	}
	s := &Store{
		pool:          pool,
		mny:           mny,
		dsn:           pool.Config().ConnString(),
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:           time.Now,
		maxRows:       DefaultMaxRows,
		maxStartDelay: DefaultMaxStartDelay,
		render:        PlainRenderer{},
		vendor:        vendorHook{ttl: DefaultVendorReadTTL},
	}
	for _, o := range opts {
		o(s)
	}
	if s.maxRows < 1 || s.maxRows > HardMaxRows {
		return nil, fmt.Errorf("jobs.New: the row limit must be between 1 and %d, got %d", HardMaxRows, s.maxRows)
	}
	if s.maxStartDelay <= 0 || s.maxStartDelay > MaxStartDelayLimit {
		return nil, fmt.Errorf("jobs.New: the max start delay must be above 0 and at most %s, got %s", MaxStartDelayLimit, s.maxStartDelay)
	}
	if s.vendor.ttl < 0 {
		return nil, fmt.Errorf("jobs.New: the vendor reachability memory must be 0 or more, got %s", s.vendor.ttl)
	}
	return s, nil
}

// MaxRows is the longest list this store accepts.
func (s *Store) MaxRows() int { return s.maxRows }

// MaxStartDelay is the delay this store adds to a job's start to make its start deadline.
func (s *Store) MaxStartDelay() time.Duration { return s.maxStartDelay }

// querier is what the reads need; the pool and a transaction both satisfy it.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// inTx runs fn in ONE READ COMMITTED transaction: BEGIN, SET LOCAL lock_timeout (the money store's wait, so that a wait for a row ends with an error and not never), fn, COMMIT; ROLLBACK on
// any error or panic. The rollback uses a context that outlives the caller's cancellation, so a cancelled request still releases its locks at once. The money store's In functions assume READ
// COMMITTED (take the lock, then read what the holder left behind), which is why the isolation is set and not left to the pool's default.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return s.fail(err)
	}
	defer func() {
		rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = tx.Rollback(rb) // a no-op after a successful Commit (pgx.ErrTxClosed)
	}()
	if err := s.mny.BoundIn(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return s.fail(err)
	}
	return nil
}

// setActor states who moves a job and why, as three transaction-local settings, so that the UPDATE that follows may change the job's status (platform.jobs_transition_guard reads them and the
// event trigger writes the job_events row from them). kind is user, admin or engine; id is the acting user or admin ("" for the engine). They stay set until clearActor (or the transaction's
// end): clear them after the one statement that needs them.
func (s *Store) setActor(ctx context.Context, tx pgx.Tx, kind, id, reason string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('platform.actor_kind', $1, true), set_config('platform.actor_id', $2, true), set_config('platform.reason', $3, true)`, kind, id, reason); err != nil {
		return s.fail(err)
	}
	return nil
}

// clearActor ends setActor's settings, so that a later UPDATE of the same transaction cannot change a status on a claim made for an earlier one.
func (s *Store) clearActor(ctx context.Context, tx pgx.Tx) error {
	return s.setActor(ctx, tx, "", "", "")
}

// move is the store's plain move: `SELECT platform.job_transition(job, to, 'user', userID, reason)`. A refusal of the lifecycle is ErrNotAllowed carrying the constraint's name.
func (s *Store) move(ctx context.Context, tx pgx.Tx, userID, jobID, to, reason string) error {
	if _, err := tx.Exec(ctx, `SELECT platform.job_transition($1::uuid, $2, 'user', $3::uuid, $4)`, jobID, to, userID, reason); err != nil {
		return s.fail(err)
	}
	return nil
}
