// Package usersettings is the ONE place that reads and writes platform.user_settings, the per-user settings of the platform (Phase 5 S6, platform_0020).
//
// THE ONE SETTING TODAY: vendor_fallback_opt_out. A customer is opted IN to module 1's paid vendor fallback by default (no row, or a row that says false) and may opt out. The setting is read ONCE, when a
// module 1 job is created (internal/jobs CreateDraft, through VendorOptOutOf, in the job's own transaction), and kept in the job's frozen settings: a later change of the setting never alters a job that exists.
//
// WHO THE USER IS. Every function takes the user id of the AUTHENTICATED PRINCIPAL and nothing else (internal/api passes the principal's id; no path, query or body names a user). This repository does not use
// row-level security: tenancy is that every statement here is scoped by the user id it is given, in its WHERE or its VALUES, and the tests read another user's value to prove it never comes back.
//
// THE AUDIT. Every CHANGE of a value writes a row in platform.user_settings_history, by a trigger of the table (so no writer can skip it): the user as the actor, the old and the new value, the database's
// clock, and nothing else. A Set that changes nothing (the same value again; a first row that states the default) writes nothing but, at most, the row itself.
//
// IDEMPOTENT. Set is one INSERT ... ON CONFLICT DO UPDATE whose UPDATE has a WHERE: the same value twice is one change and one history row, and two requests racing for the same user end in one state.
package usersettings

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Settings is every setting of one user, with the defaults of a user who never stated one.
type Settings struct {
	// VendorFallbackOptOut is true when the user opted OUT of module 1's paid vendor fallback: their module 1 jobs are created under the policy without the external block and reserve at the non-vendor price.
	VendorFallbackOptOut bool
}

// Defaults are the settings of a user with no row: opted in.
func Defaults() Settings { return Settings{} }

var (
	// ErrBadUser: the user id is not the canonical lower-case uuid of a customer (or is the all-zero system user). It is the authenticated principal, so a bad one is a bug of the caller.
	ErrBadUser = errors.New("usersettings: the user id is not a customer's canonical uuid")
	// ErrUnknownUser: no such user (the foreign key user_settings_user_fk, SQLSTATE 23503).
	ErrUnknownUser = errors.New("usersettings: no such user")
	// ErrNoDB: the store was built with no database.
	ErrNoDB = errors.New("usersettings: no database")
)

// The database's own names, pinned by the harness tests against a migrated database.
const (
	sqlstateFKViolation    = "23503"
	sqlstateCheckViolation = "23514"
	conUserFK              = "user_settings_user_fk"
	conSystemUser          = "user_settings_not_the_system_user"
)

const systemUser = "00000000-0000-0000-0000-000000000000"

// Querier is what a read needs of a connection: *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes the settings over the platform's pool (a login as platform_app).
type Store struct {
	pool *pgxpool.Pool
}

// New builds the store over the pool.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, ErrNoDB
	}
	return &Store{pool: pool}, nil
}

// validUser is true for the canonical lower-case uuid of a customer: 36 characters, lower-case hex, hyphens at 8, 13, 18 and 23, and not the all-zero system user.
func validUser(id string) bool {
	if len(id) != 36 || id == systemUser {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// Get returns the user's settings: the stored ones, or the defaults (opted in) for a user with no row. Only that user's row is ever read.
func (s *Store) Get(ctx context.Context, userID string) (Settings, error) {
	if s == nil || s.pool == nil {
		return Settings{}, opErr("get", ErrNoDB)
	}
	optOut, err := VendorOptOutOf(ctx, s.pool, userID)
	if err != nil {
		return Settings{}, err
	}
	return Settings{VendorFallbackOptOut: optOut}, nil
}

// VendorOptOutOf reads the user's vendor opt-out through any connection (a transaction the caller owns included): the stored value, or false (opted in) for a user with no row. It is how a job reads the
// setting ONCE, in the transaction that creates it. The statement is scoped by the user id and by nothing else.
func VendorOptOutOf(ctx context.Context, q Querier, userID string) (bool, error) {
	if !validUser(userID) {
		return false, opErr("get", ErrBadUser)
	}
	if q == nil {
		return false, opErr("get", ErrNoDB)
	}
	var optOut bool
	err := q.QueryRow(ctx, `SELECT vendor_fallback_opt_out FROM platform.user_settings WHERE user_id = $1::uuid`, userID).Scan(&optOut)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil // no row: the defaults
	case err != nil:
		return false, opErr("get", unexpected(err))
	}
	return optOut, nil
}

// Set states the user's vendor opt-out and returns the settings now in force. It is idempotent: the same value again changes nothing (no history row, the time of the row is the time of the last change),
// a first row that states the default writes the row and no history, and a change writes exactly one history row, whose actor is the user. A user that does not exist is ErrUnknownUser.
func (s *Store) Set(ctx context.Context, userID string, vendorOptOut bool) (Settings, error) {
	if s == nil || s.pool == nil {
		return Settings{}, opErr("set", ErrNoDB)
	}
	if !validUser(userID) {
		return Settings{}, opErr("set", ErrBadUser)
	}
	// ONE statement: the row appears (or its value changes) and the trigger writes the history in the same statement. The UPDATE's WHERE makes a repeat of the same value a no-op that returns no row.
	var now bool
	err := s.pool.QueryRow(ctx, `INSERT INTO platform.user_settings AS s (user_id, vendor_fallback_opt_out)
		VALUES ($1::uuid, $2::boolean)
		ON CONFLICT (user_id) DO UPDATE SET vendor_fallback_opt_out = EXCLUDED.vendor_fallback_opt_out, updated_at = now()
			WHERE s.vendor_fallback_opt_out IS DISTINCT FROM EXCLUDED.vendor_fallback_opt_out
		RETURNING s.vendor_fallback_opt_out`, userID, vendorOptOut).Scan(&now)
	switch {
	case err == nil:
		return Settings{VendorFallbackOptOut: now}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// the row already says so (the UPDATE found nothing to change): read it, so the answer is what is stored
		return s.Get(ctx, userID)
	}
	if mapped := mapDBError(err); mapped != nil {
		return Settings{}, opErr("set", mapped)
	}
	return Settings{}, opErr("set", unexpected(err))
}

// opErr names the operation on an error: "usersettings.set: ...". nil stays nil.
func opErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("usersettings.%s: %w", op, err)
}

// mapDBError maps a database refusal to the store's own BY SQLSTATE AND CONSTRAINT NAME (never by text), or returns nil.
func mapDBError(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return nil
	}
	switch {
	case pe.Code == sqlstateFKViolation && pe.ConstraintName == conUserFK:
		return ErrUnknownUser
	case pe.Code == sqlstateCheckViolation && pe.ConstraintName == conSystemUser:
		return ErrBadUser
	}
	return nil
}

// unexpected turns an error the store does not map into its own. A database error is reported by SQLSTATE, constraint and the server's MESSAGE (never the Detail line, which quotes the row) and does
// not wrap the driver's error; any other error (a cancelled context, a closed connection) is returned as it is, so errors.Is(err, context.Canceled) still works.
func unexpected(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("unexpected database refusal (SQLSTATE %s, constraint %q): %s", pe.Code, pe.ConstraintName, pe.Message)
	}
	return err
}
