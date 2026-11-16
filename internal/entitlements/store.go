// Package entitlements is the platform's side of module 3's entitlement flow.
//
// An entitlement is "this user may use module 3, optionally until a date". It is WRITTEN ONLY BY THE LEDGER
// (platform_0006): enabling, disabling and expiring are ledger rows of kind m3_enable, m3_disable and m3_expire,
// and a trigger derives platform.m3_entitlements from them, so a stored entitlement cannot change without the
// row that explains it. This package has three parts:
//
//   - Store: the writers (Enable, Disable, ExpireDue) and the readers (Get, Sync). Every rule that can be a
//     database rule is one (the due check, the disabled admin, the system user, the idempotency key); the store
//     validates what the database cannot know before it asks, and maps each refusal by constraint name.
//   - Reconciler: delivers the wanted state to module 3 (suspend or resume through its admin route) and records
//     what module 3 ITSELF answered. It never records convergence for a state module 3 was not seen to be in.
//   - Service (Run): the expiry loop and the reconcile loop, started and stopped with the process.
//
// TIME IS THE DATABASE'S. "Due", "in the future" and "expired" are decided by the database's now(); the Go clock is
// used for backoff arithmetic and health timestamps only, never for what an entitlement is.
package entitlements

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits of a request, checked before the database is asked.
const (
	// MaxReasonBytes is the longest reason an admin action may carry.
	MaxReasonBytes = 1000
	// MaxKeyBytes is the longest idempotency key accepted (the ledger column is text; this keeps one row small).
	MaxKeyBytes = 200

	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

// The statements of the store. Every one names its schema (CLAUDE.md rule 12).
const (
	sqlEntryByKey = `SELECT id, kind, acting_admin_id::text, until_at, reason, created_at
		FROM platform.ledger WHERE user_id = $1::uuid AND module_id = 'm3' AND idempotency_key = $2`

	sqlInsertEntry = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, until_at, reason, idempotency_key)
		VALUES ($1::uuid, 'm3', $2, 0, 0, $3::uuid, $4::timestamptz, $5, $6) RETURNING id, created_at`

	// The enabled-until date must be at least one second after the database's now() (which is also the
	// created_at of the row this transaction inserts, so the ledger's own CHECK until_at > created_at agrees).
	sqlUntilIsFuture = `SELECT $1::timestamptz >= now() + interval '1 second'`

	sqlAdvisoryLock = `SELECT pg_advisory_xact_lock($1)`

	// The users whose entitlement is due, oldest first. now() is the database's.
	sqlDueUsers = `SELECT user_id::text, enabled_until FROM platform.m3_entitlements
		WHERE enabled AND enabled_until IS NOT NULL AND enabled_until <= now()
		ORDER BY enabled_until, user_id LIMIT $1`

	// The same predicate, re-read under the user's advisory lock: it is the one the database's trigger applies.
	sqlStillDue = `SELECT enabled_until FROM platform.m3_entitlements
		WHERE user_id = $1::uuid AND enabled AND enabled_until IS NOT NULL AND enabled_until <= now()`

	sqlInsertExpiry = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1::uuid, 'm3', 'm3_expire', 0, 0, NULL, $2, $3) RETURNING id`

	sqlGetEntitlement = `SELECT enabled, enabled_until, changed_by::text, changed_at FROM platform.m3_entitlements WHERE user_id = $1::uuid`

	sqlGetSync = `SELECT want_suspended, want_version, converged_version, observed, observed_at, attempts, last_attempt_at, last_error, updated_at
		FROM platform.m3_sync WHERE user_id = $1::uuid`

	sqlCountUnconverged = `SELECT count(*) FROM platform.m3_sync WHERE converged_version < want_version`
)

// Constraint names the store maps. They are the names the migrations declare (or PostgreSQL derives for a
// column foreign key); a test pins each of them against a migrated database.
const (
	conAdminIsActive       = "admin_is_active"
	conAdminFK             = "ledger_acting_admin_id_fkey"
	conUserFK              = "ledger_user_id_fkey"
	conEntAdminFK          = "m3_entitlements_changed_by_fkey" // the same refusal, raised by the entitlement the ledger trigger writes
	conEntUserFK           = "m3_entitlements_user_id_fkey"
	conSystemUser          = "ledger_not_the_system_user"
	conUntilShape          = "ledger_until_at_shape"
	conIdempotencyKey      = "ledger_idempotency_key"
	conExpireIsDue         = "m3_expire_is_due"
	sqlstateCheckViolation = "23514"
	sqlstateFKViolation    = "23503"
	sqlstateUniqueViol     = "23505"
)

var (
	errNoPool = errors.New("entitlements: the store has no database pool")
	uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Store reads and writes module 3 entitlements through the platform_app connection pool.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// EnableReq enables (or re-dates) a user's entitlement.
type EnableReq struct {
	UserID string
	// Until is the end of the entitlement; nil means no end date. When set it must be at least one second
	// after the DATABASE's now().
	Until   *time.Time
	AdminID string
	Reason  string
	// Key is the caller's retry key: the same key with the same parameters returns the original entry.
	Key string
}

// DisableReq disables a user's entitlement.
type DisableReq struct {
	UserID  string
	AdminID string
	Reason  string
	Key     string
}

// Entry is one entitlement ledger row, as written (or as found, for a replay).
type Entry struct {
	LedgerID  int64
	UserID    string
	Kind      string
	AdminID   *string // nil for the automatic expiry
	Until     *time.Time
	Reason    string
	Key       string
	CreatedAt time.Time
	// Replayed is true when the key had been used before with the same parameters: nothing was written.
	Replayed bool
}

// Entitlement is a user's current entitlement (platform.m3_entitlements). Exists is false for a user who has
// never been enabled, disabled or expired.
type Entitlement struct {
	UserID    string
	Exists    bool
	Enabled   bool
	Until     *time.Time
	ChangedBy *string
	ChangedAt time.Time
}

// SyncState is the delivery bookkeeping of one user (platform.m3_sync). Exists is false when the user has no row.
type SyncState struct {
	UserID           string
	Exists           bool
	WantSuspended    bool
	WantVersion      int64
	ConvergedVersion int64
	Observed         string // "unknown" until module 3 has answered, then "suspended" or "active"
	ObservedAt       *time.Time
	Attempts         int
	LastAttemptAt    *time.Time
	LastError        *string
	UpdatedAt        time.Time
}

// Converged reports whether the wanted state is the state module 3 was last seen in: the same condition the
// database's CHECK m3_sync_converged_only_when_observed enforces on the columns.
func (s SyncState) Converged() bool {
	if !s.Exists || s.ConvergedVersion != s.WantVersion {
		return false
	}
	return s.Observed == wantedObservation(s.WantSuspended)
}

// ---- writers -----------------------------------------------------------------------------------------------------

// Enable records an m3_enable ledger row (the trigger makes the entitlement enabled, until req.Until when it is
// set). Enabling an already enabled user only re-dates it.
func (s *Store) Enable(ctx context.Context, req EnableReq) (Entry, error) {
	r, err := normalise("m3_enable", req.UserID, req.AdminID, req.Reason, req.Key, req.Until)
	if err != nil {
		return Entry{}, fmt.Errorf("entitlements: enable: %w", err)
	}
	e, err := s.record(ctx, r)
	if err != nil {
		return Entry{}, fmt.Errorf("entitlements: enable: %w", err)
	}
	return e, nil
}

// Disable records an m3_disable ledger row (the trigger makes the entitlement disabled and clears its date).
func (s *Store) Disable(ctx context.Context, req DisableReq) (Entry, error) {
	r, err := normalise("m3_disable", req.UserID, req.AdminID, req.Reason, req.Key, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("entitlements: disable: %w", err)
	}
	e, err := s.record(ctx, r)
	if err != nil {
		return Entry{}, fmt.Errorf("entitlements: disable: %w", err)
	}
	return e, nil
}

// request is a validated, canonical Enable or Disable.
type request struct {
	kind   string
	user   string
	admin  string
	reason string // trimmed; "" is stored as NULL
	key    string
	until  *time.Time // UTC, truncated to the microsecond PostgreSQL stores
}

// normalise validates everything the database cannot judge by itself, BEFORE it is asked: the shapes of the
// ids, the system user, the sizes. (That the date lies in the future is the database's clock to judge, so it
// is checked inside the transaction that writes the row.)
func normalise(kind, userID, adminID, reason, key string, until *time.Time) (request, error) {
	user, ok := canonicalUUID(userID)
	if !ok {
		return request{}, fmt.Errorf("%w: the user id is not a uuid", ErrInvalid)
	}
	if user == zeroUUID {
		return request{}, invalid(ErrSystemUser, "")
	}
	admin, ok := canonicalUUID(adminID)
	if !ok {
		return request{}, fmt.Errorf("%w: the admin id is not a uuid", ErrInvalid)
	}
	if admin == zeroUUID {
		return request{}, fmt.Errorf("%w: the system user cannot be an admin", ErrInvalid)
	}
	if strings.TrimSpace(key) == "" || len(key) > MaxKeyBytes || !cleanText(key) {
		return request{}, fmt.Errorf("%w: the idempotency key must be 1 to %d bytes of text and not blank", ErrInvalid, MaxKeyBytes)
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > MaxReasonBytes {
		return request{}, invalid(ErrReasonTooLong, fmt.Sprintf("%d bytes, at most %d", len(reason), MaxReasonBytes))
	}
	if !cleanText(reason) {
		return request{}, fmt.Errorf("%w: the reason is not valid text", ErrInvalid)
	}
	r := request{kind: kind, user: user, admin: admin, reason: reason, key: key}
	if until != nil {
		if until.IsZero() {
			return request{}, fmt.Errorf("%w: the enabled-until date is the zero time (leave it nil for no end date)", ErrInvalid)
		}
		u := until.UTC().Truncate(time.Microsecond)
		r.until = &u
	}
	return r, nil
}

// canonicalUUID returns the lower-case hyphenated form of a uuid, and whether s was one.
func canonicalUUID(s string) (string, bool) {
	if !uuidShape.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// cleanText is true for valid UTF-8 without a NUL (PostgreSQL text cannot hold one).
func cleanText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// record writes the request as one ledger row, or recognises it as a replay.
func (s *Store) record(ctx context.Context, r request) (Entry, error) {
	if s == nil || s.pool == nil {
		return Entry{}, errNoPool
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Entry{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The user's module-3 writers serialise (enable, disable, expiry), like every money writer of a (user,
	// module). The database's row locks would keep the result correct without it; the lock keeps the order
	// clean and makes a replay see the row its twin just wrote.
	if _, err := tx.Exec(ctx, sqlAdvisoryLock, lockKey(r.user, moduleM3)); err != nil {
		return Entry{}, fmt.Errorf("advisory lock: %w", err)
	}

	// A replay is answered from the ledger before anything else is judged: it must keep working after the date
	// has passed or the admin has been disabled, because it writes nothing.
	if prior, found, err := entryByKey(ctx, tx, r.user, r.key); err != nil {
		return Entry{}, err
	} else if found {
		return replayOf(prior, r)
	}

	if r.until != nil {
		var future bool
		if err := tx.QueryRow(ctx, sqlUntilIsFuture, *r.until).Scan(&future); err != nil {
			return Entry{}, fmt.Errorf("check the date against the database clock: %w", err)
		}
		if !future {
			return Entry{}, invalid(ErrUntilNotFuture, "")
		}
	}

	var (
		admin  = &r.admin
		reason *string
		id     int64
		at     time.Time
	)
	if r.reason != "" {
		reason = &r.reason
	}
	if err := tx.QueryRow(ctx, sqlInsertEntry, r.user, r.kind, admin, r.until, reason, r.key).Scan(&id, &at); err != nil {
		if isConstraint(err, sqlstateUniqueViol, conIdempotencyKey) {
			// Another writer recorded this key between our lookup and our insert (it did not take the lock).
			// Our transaction is aborted; read what it wrote and compare.
			_ = tx.Rollback(context.WithoutCancel(ctx))
			prior, found, lerr := entryByKey(ctx, s.pool, r.user, r.key)
			if lerr != nil {
				return Entry{}, lerr
			}
			if found {
				return replayOf(prior, r)
			}
		}
		return Entry{}, mapWriteError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, fmt.Errorf("commit: %w", err)
	}
	return Entry{LedgerID: id, UserID: r.user, Kind: r.kind, AdminID: &r.admin, Until: r.until, Reason: r.reason, Key: r.key, CreatedAt: at}, nil
}

// replayOf is the answer to a key that was used before: the original entry when the parameters are the same,
// ErrIdempotencyMismatch when they are not.
func replayOf(prior Entry, r request) (Entry, error) {
	if !prior.sameRequest(r) {
		return Entry{}, ErrIdempotencyMismatch
	}
	prior.Replayed = true
	return prior, nil
}

// sameRequest compares every parameter a caller chooses: the kind, the admin, the date and the reason.
func (e Entry) sameRequest(r request) bool {
	if e.Kind != r.kind || e.AdminID == nil || *e.AdminID != r.admin || e.Reason != r.reason {
		return false
	}
	if (e.Until == nil) != (r.until == nil) {
		return false
	}
	return e.Until == nil || e.Until.Equal(*r.until)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// entryByKey reads the m3 ledger row of (user, key).
func entryByKey(ctx context.Context, q queryRower, user, key string) (Entry, bool, error) {
	var (
		e      Entry
		admin  *string
		until  *time.Time
		reason *string
	)
	err := q.QueryRow(ctx, sqlEntryByKey, user, key).Scan(&e.LedgerID, &e.Kind, &admin, &until, &reason, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("look up the idempotency key: %w", err)
	}
	e.UserID, e.Key, e.AdminID = user, key, admin
	if until != nil {
		u := until.UTC()
		e.Until = &u
	}
	if reason != nil {
		e.Reason = *reason
	}
	return e, true, nil
}

// isConstraint is true for a database error with this SQLSTATE and this constraint name.
func isConstraint(err error, sqlstate, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlstate && pe.ConstraintName == constraint
}

// mapWriteError turns the database's refusal of a ledger insert into the named error, by SQLSTATE and
// CONSTRAINT NAME (never by message text). A refusal the store does not know stays an error wrapping the
// original, so nothing is hidden.
func mapWriteError(err error) error {
	switch {
	case isConstraint(err, sqlstateCheckViolation, conAdminIsActive):
		return fmt.Errorf("%w: %w", ErrAdminDisabled, err)
	case isConstraint(err, sqlstateFKViolation, conAdminFK), isConstraint(err, sqlstateFKViolation, conEntAdminFK):
		return fmt.Errorf("%w: %w", ErrUnknownAdmin, err)
	case isConstraint(err, sqlstateFKViolation, conUserFK), isConstraint(err, sqlstateFKViolation, conEntUserFK):
		return fmt.Errorf("%w: %w", ErrUnknownUser, err)
	case isConstraint(err, sqlstateCheckViolation, conSystemUser):
		return fmt.Errorf("%w: %w", ErrSystemUser, err)
	case isConstraint(err, sqlstateCheckViolation, conUntilShape):
		return fmt.Errorf("%w: %w", ErrUntilNotFuture, err)
	}
	return fmt.Errorf("write the ledger row: %w", err)
}

// ---- expiry ------------------------------------------------------------------------------------------------------

// Expired is one entitlement the expiry ended.
type Expired struct {
	UserID       string
	EnabledUntil time.Time // the date that had passed
	LedgerID     int64     // the m3_expire row
}

// ExpireReport is the outcome of one ExpireDueReport pass.
type ExpireReport struct {
	// Due is how many users the first read found due (at most the limit).
	Due int
	// Expired lists the users this pass expired.
	Expired []Expired
	// Skipped counts the users that were due when read but not expired by this pass: another instance expired
	// them first (a unique violation on the expiry's key, or the row no longer due when re-read under the
	// lock, or the database's own m3_expire_is_due refusal). Not an error.
	Skipped int
}

// ExpireDue ends every entitlement whose enabled-until date has passed (the database's clock), at most limit per
// call, and returns the users it expired. See ExpireDueReport for the skip count.
func (s *Store) ExpireDue(ctx context.Context, limit int) ([]Expired, error) {
	rep, err := s.ExpireDueReport(ctx, limit)
	return rep.Expired, err
}

// ExpireDueReport is ExpireDue with the whole outcome. Each due user is expired in its OWN transaction, under that
// user's advisory lock, re-checking that the entitlement is still due before it writes the m3_expire row; the
// idempotency key `m3-expire:<user>:<enabled_until in unix microseconds>` makes a second instance's attempt at
// the same expiry a no-op. A failure for one user does not stop the others: the returned error joins every
// failure, and the report holds what did happen.
func (s *Store) ExpireDueReport(ctx context.Context, limit int) (ExpireReport, error) {
	var rep ExpireReport
	if limit < 1 {
		return rep, fmt.Errorf("entitlements: expire: %w: the limit must be at least 1", ErrInvalid)
	}
	if s == nil || s.pool == nil {
		return rep, errNoPool
	}
	due, err := s.dueUsers(ctx, limit)
	if err != nil {
		return rep, fmt.Errorf("entitlements: expire: list the due entitlements: %w", err)
	}
	rep.Due = len(due)

	var errs []error
	for _, user := range due {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		ex, expired, err := s.expireOne(ctx, user)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("entitlements: expire %s: %w", user, err))
		case expired:
			rep.Expired = append(rep.Expired, ex)
		default:
			rep.Skipped++
		}
	}
	return rep, errors.Join(errs...)
}

// dueUsers reads the due users and closes the cursor before any per-user transaction starts.
func (s *Store) dueUsers(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, sqlDueUsers, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var (
			user  string
			until time.Time
		)
		if err := rows.Scan(&user, &until); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// expireOne expires one user in its own transaction. It returns (entry, true, nil) when it wrote the row,
// (zero, false, nil) when the user is correctly skipped, and an error for anything else.
func (s *Store) expireOne(ctx context.Context, user string) (Expired, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Expired{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, sqlAdvisoryLock, lockKey(user, moduleM3)); err != nil {
		return Expired{}, false, fmt.Errorf("advisory lock: %w", err)
	}

	// Re-check under the lock: another instance (or an admin re-dating the entitlement) may have changed it
	// since the list was read.
	var until time.Time
	switch err := tx.QueryRow(ctx, sqlStillDue, user).Scan(&until); {
	case errors.Is(err, pgx.ErrNoRows):
		return Expired{}, false, nil
	case err != nil:
		return Expired{}, false, fmt.Errorf("re-read the entitlement: %w", err)
	}

	var id int64
	key := expiryKey(user, until)
	reason := "entitlement expired: enabled until " + until.UTC().Format(time.RFC3339Nano)
	if err := tx.QueryRow(ctx, sqlInsertExpiry, user, reason, key).Scan(&id); err != nil {
		// Two refusals that mean "somebody else already did it": the key was used (another instance expired
		// this very date) and the database's own is-due check (no longer due by the ledger row's timestamp).
		if isConstraint(err, sqlstateUniqueViol, conIdempotencyKey) || isConstraint(err, sqlstateCheckViolation, conExpireIsDue) {
			return Expired{}, false, nil
		}
		return Expired{}, false, fmt.Errorf("write the m3_expire row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Expired{}, false, fmt.Errorf("commit: %w", err)
	}
	return Expired{UserID: user, EnabledUntil: until.UTC(), LedgerID: id}, true, nil
}

// expiryKey is the idempotency key of the expiry of one entitlement date: the same user and the same date give
// the same key on every instance.
func expiryKey(user string, until time.Time) string {
	return fmt.Sprintf("m3-expire:%s:%d", user, until.UnixMicro())
}

// ---- readers -----------------------------------------------------------------------------------------------------

// Get reads a user's entitlement. A user who has none yields Exists == false, not an error.
func (s *Store) Get(ctx context.Context, userID string) (Entitlement, error) {
	user, ok := canonicalUUID(userID)
	if !ok {
		return Entitlement{}, fmt.Errorf("entitlements: get: %w: the user id is not a uuid", ErrInvalid)
	}
	if s == nil || s.pool == nil {
		return Entitlement{}, errNoPool
	}
	e := Entitlement{UserID: user}
	var until *time.Time
	err := s.pool.QueryRow(ctx, sqlGetEntitlement, user).Scan(&e.Enabled, &until, &e.ChangedBy, &e.ChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entitlement{UserID: user}, nil
	}
	if err != nil {
		return Entitlement{}, fmt.Errorf("entitlements: get: %w", err)
	}
	e.Exists = true
	if until != nil {
		u := until.UTC()
		e.Until = &u
	}
	return e, nil
}

// Sync reads a user's delivery bookkeeping. A user who has none yields Exists == false, not an error.
func (s *Store) Sync(ctx context.Context, userID string) (SyncState, error) {
	user, ok := canonicalUUID(userID)
	if !ok {
		return SyncState{}, fmt.Errorf("entitlements: sync: %w: the user id is not a uuid", ErrInvalid)
	}
	if s == nil || s.pool == nil {
		return SyncState{}, errNoPool
	}
	st := SyncState{UserID: user}
	err := s.pool.QueryRow(ctx, sqlGetSync, user).Scan(&st.WantSuspended, &st.WantVersion, &st.ConvergedVersion, &st.Observed,
		&st.ObservedAt, &st.Attempts, &st.LastAttemptAt, &st.LastError, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncState{UserID: user}, nil
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("entitlements: sync: %w", err)
	}
	st.Exists = true
	return st, nil
}

// Unconverged counts the users whose wanted state module 3 has not yet been seen in.
func (s *Store) Unconverged(ctx context.Context) (int64, error) {
	return countUnconverged(ctx, s.poolOrNil())
}

func (s *Store) poolOrNil() queryRower {
	if s == nil || s.pool == nil {
		return nil
	}
	return s.pool
}

func countUnconverged(ctx context.Context, q queryRower) (int64, error) {
	if q == nil {
		return 0, errNoPool
	}
	var n int64
	if err := q.QueryRow(ctx, sqlCountUnconverged).Scan(&n); err != nil {
		return 0, fmt.Errorf("entitlements: count the unconverged users: %w", err)
	}
	return n, nil
}

// wantedObservation is the value of platform.m3_sync.observed that matches a wanted state.
func wantedObservation(wantSuspended bool) string {
	if wantSuspended {
		return "suspended"
	}
	return "active"
}
