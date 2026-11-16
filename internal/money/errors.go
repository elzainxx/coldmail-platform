package money

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// The errors of the store. A caller tests them with errors.Is (the sentinels) or errors.As (ErrInsufficientCredits, InvalidError); every error the
// store returns is wrapped with the operation ("money.reserve: ...") and none of them ever carries a connection string or a password.
//
// A refusal that has a NAME in the database (a constraint) is mapped by the constraint's name, never by the text of the message: the text is the
// server's wording and changes, the name is the contract (mapPgError).
var (
	// ErrInvalid is wrapped by every InvalidError: the request is malformed and was refused BEFORE the database was asked.
	ErrInvalid = errors.New("invalid request")
	// ErrNotMetered: credits exist only for the metered modules m1 and m2; module 3 is entitlement-based and never has a balance.
	ErrNotMetered = errors.New("the module is not metered (only m1 and m2 hold credits)")
	// ErrAdminDisabled: the acting admin has been disabled, and the database refuses a disabled admin (constraint admin_is_active).
	ErrAdminDisabled = errors.New("the acting admin is disabled")
	// ErrAdminUnknown: the acting admin is not a row of platform.admins.
	ErrAdminUnknown = errors.New("the acting admin does not exist")
	// ErrUserNotFound: the user is not a row of public.users.
	ErrUserNotFound = errors.New("the user does not exist")
	// ErrIdempotencyMismatch: the idempotency key (or the job, for a reserve) was already used for a DIFFERENT request. The first request stands.
	ErrIdempotencyMismatch = errors.New("the idempotency key was already used for a different request")
	// ErrJobNotFound: no job has that id.
	ErrJobNotFound = errors.New("the job does not exist")
	// ErrJobState: the job's status does not allow the operation (a reserve needs a draft job, a settle needs a job that has left draft).
	ErrJobState = errors.New("the job's status does not allow this")
	// ErrJobNotEnded: a release needs a job that ended (succeeded, failed or cancelled).
	ErrJobNotEnded = errors.New("the job has not ended")
	// ErrNoReserve: the job holds no reserve (nothing was ever reserved for it).
	ErrNoReserve = errors.New("the job holds no reserve")
	// ErrReserveExceeded: the charges of this call, or one charge written by anything else, would exceed what the job's reserve still holds
	// (reserved - charged - released). Nothing is written.
	ErrReserveExceeded = errors.New("the charges would exceed what the job's reserve still holds")
	// ErrUnpriced: a charge names a unit that has no price in the job's frozen price snapshot. Nothing is written.
	ErrUnpriced = errors.New("the unit has no price in the job's frozen price snapshot")
	// ErrBadItem: a settle item is malformed (a blank or over-long item_ref, a repeated item_ref).
	ErrBadItem = errors.New("a settle item is malformed")
	// ErrSnapshot: the database refused the job's price snapshot (not the dearest price, not complete) or the module has no prices to freeze.
	ErrSnapshot = errors.New("the job's price snapshot is not acceptable")
	// ErrConflict: a write lost to an earlier or concurrent one that holds the same database key (a second reserve for the job, a second charge for the
	// item, a second release). The store's lock makes this unreachable through the store; it is the database's second wall, reported by name.
	ErrConflict = errors.New("an earlier or concurrent write holds the same key")
)

// ErrInsufficientCredits: the user's available credits do not cover what was asked. Shortfall = Needed - Available. It is a struct (not a sentinel):
// test it with errors.As into a money.ErrInsufficientCredits VALUE. Raced is true only when the store's own read said the credits were there and the
// database's CHECK (balances_available_not_negative) refused all the same, which means a writer got in without taking the (user, module) lock.
type ErrInsufficientCredits struct {
	Available int64
	Needed    int64
	Shortfall int64
	Raced     bool
}

func (e ErrInsufficientCredits) Error() string {
	s := fmt.Sprintf("insufficient credits: available %d, needed %d, short by %d", e.Available, e.Needed, e.Shortfall)
	if e.Raced {
		s += " (refused by the database's balances CHECK after a read that said it fitted: another writer got in)"
	}
	return s
}

// InvalidError is a request refused before the database was asked: Field names the field, Reason says what is wrong with it. It matches ErrInvalid.
type InvalidError struct {
	Field  string
	Reason string
}

func (e *InvalidError) Error() string { return "invalid " + e.Field + ": " + e.Reason }

// Is makes errors.Is(err, ErrInvalid) true for every InvalidError.
func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(field, format string, args ...any) error {
	return &InvalidError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// pgCtx is what the operation had read under the lock when the database refused it: it fills ErrInsufficientCredits when the CHECK is the one that
// refuses. Zero when the operation read nothing.
type pgCtx struct {
	available int64
	needed    int64
}

// mapPgError maps a database error to the store's own, BY CONSTRAINT NAME, or returns nil when the error is not one the store knows (the caller then
// wraps it as unexpected). err may be wrapped; the first *pgconn.PgError in the chain is judged.
func mapPgError(err error, c pgCtx) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return nil
	}
	switch pe.ConstraintName {
	case "balances_available_not_negative":
		short := c.needed - c.available
		if short < 0 {
			short = 0
		}
		return ErrInsufficientCredits{Available: c.available, Needed: c.needed, Shortfall: short, Raced: true}
	case "reserves_not_overspent":
		return fmt.Errorf("%w (refused by the database: constraint reserves_not_overspent)", ErrReserveExceeded)
	case "admin_is_active":
		return ErrAdminDisabled
	case "ledger_acting_admin_id_fkey", "admin_audit_admin_id_fkey": // the audit row of an adjustment is written by a trigger of the same statement
		return ErrAdminUnknown
	case "ledger_user_id_fkey", "balances_user_id_fkey": // the balance row is written by a trigger of the same statement
		return ErrUserNotFound
	case "ledger_not_the_system_user":
		return invalid("user_id", "the system user holds no credits (constraint ledger_not_the_system_user)")
	case "ledger_one_charge_per_item", "ledger_one_reserve_per_job", "ledger_one_release_per_reserve", "ledger_idempotency_key", "release_is_the_remainder", "job_unit_prices_pk":
		return fmt.Errorf("%w (constraint %s)", ErrConflict, pe.ConstraintName)
	case "reserve_at_the_dearest_price", "reserve_needs_the_complete_snapshot":
		return fmt.Errorf("%w (constraint %s)", ErrSnapshot, pe.ConstraintName)
	}
	return nil
}

// opErr names the operation on an error: "money.reserve: ...". nil stays nil.
func opErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("money.%s: %w", op, err)
}
