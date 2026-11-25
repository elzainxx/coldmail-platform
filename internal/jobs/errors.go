package jobs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
	"github.com/coldmail-platform/platform/internal/money"
)

// The errors of the store. A caller tests them with errors.Is (the sentinels) or errors.As (the typed ones); every error is wrapped with the operation ("jobs.confirm: ...") and NONE
// of them carries an address, a connection string or a password: a refusal names its reason (a word, a row's ordinal, a constraint) and never the row.
//
// A refusal that has a NAME in the database (a constraint) is mapped by SQLSTATE and the constraint's name, never by the text of the message: the text is the server's wording, the name is
// the contract (mapDBError).
var (
	// ErrNotFound: no such job FOR THIS USER. A job of another user is exactly this error, indistinguishable from a job that does not exist.
	ErrNotFound = errors.New("job not found")
	// ErrBadRequest is wrapped by every *BadRequestError: the request is malformed or names something that cannot be done with it, and was refused for a reason a caller can show.
	ErrBadRequest = errors.New("bad request")
	// ErrTooManyRows is wrapped by every *TooManyRowsError: the list is longer than the store's limit (WithMaxRows, default DefaultMaxRows).
	ErrTooManyRows = errors.New("too many rows")
	// ErrNothingToDo: not one row would reach the module (every row is malformed, a repeat, excluded or suppressed), so there is nothing to confirm.
	ErrNothingToDo = errors.New("no row would reach the module")
	// ErrNotAllowed is wrapped by every *NotAllowedError: the job's lifecycle refuses the move. When the refusal is the database's (SQLSTATE 23001, platform.jobs_transition_guard), Reason is the
	// constraint's name (job_transition_illegal, job_module_cannot_cancel, job_module_cannot_pause, job_resume_to_where_it_paused_from, ...).
	ErrNotAllowed = errors.New("the job's lifecycle does not allow this")
	// ErrStalePreview: what the preview depended on has changed (the list, the price of a reachable unit, the content, the choice or the source job's results) or the fingerprint is not the
	// job's: preview again and confirm what the new preview says.
	ErrStalePreview = errors.New("the preview is stale")
	// ErrSlotTaken: the user already has an active job on this module (the partial unique index jobs_one_active_per_user_module, SQLSTATE 23505). The whole confirm was rolled back: no credits are held.
	ErrSlotTaken = errors.New("the user already has an active job on this module")
	// ErrConfirmKeyReused: the confirm key already confirmed ANOTHER job of this user (the unique index jobs_confirm_key_key, or the store's own read of it).
	ErrConfirmKeyReused = errors.New("the confirm key belongs to another job")
	// ErrNotPreviewed: a confirm needs a job that is previewed (and, if it was confirmed before, with another key).
	ErrNotPreviewed = errors.New("the job is not previewed")
	// ErrNoEntitlement: module 3 needs an enabled, unexpired entitlement (platform.m3_entitlements) and the user has none.
	ErrNoEntitlement = errors.New("the user has no enabled module 3 entitlement")
	// ErrSuspended: the platform has observed module 3 holding the user suspended (platform.m3_sync), so a job could not run.
	ErrSuspended = errors.New("module 3 is observed holding the user suspended")
	// ErrModuleUnavailable: module 3 could not be asked whether the campaign is feasible, and an immediate confirm will not hold anything for a job that cannot start. A scheduled confirm is not refused for it.
	ErrModuleUnavailable = errors.New("the module is unavailable")
)

// ErrInsufficientCredits is money.ErrInsufficientCredits, re-exported: errors.As(err, &jobs.ErrInsufficientCredits{}) reaches the shortfall (Available, Needed, Shortfall).
type ErrInsufficientCredits = money.ErrInsufficientCredits

// ErrInfeasible: module 3 says the campaign cannot run, with its causes (words the module names; never an address). Test it with errors.As into an ErrInfeasible VALUE.
type ErrInfeasible struct {
	Causes []string
}

func (e ErrInfeasible) Error() string {
	return "the campaign is not feasible on module 3: " + strings.Join(e.Causes, ", ")
}

// BadRequestError is an ErrBadRequest with the reason, which never contains an address (it may name a row's ordinal, a field, a limit).
type BadRequestError struct {
	Reason string
}

func (e *BadRequestError) Error() string { return "bad request: " + e.Reason }

// Is makes errors.Is(err, ErrBadRequest) true.
func (e *BadRequestError) Is(target error) bool { return target == ErrBadRequest }

func badRequest(format string, args ...any) error {
	return &BadRequestError{Reason: fmt.Sprintf(format, args...)}
}

// TooManyRowsError is an ErrTooManyRows with the limit that was exceeded.
type TooManyRowsError struct {
	Limit int
}

func (e *TooManyRowsError) Error() string {
	return fmt.Sprintf("too many rows: a job holds at most %d", e.Limit)
}

// Is makes errors.Is(err, ErrTooManyRows) true.
func (e *TooManyRowsError) Is(target error) bool { return target == ErrTooManyRows }

// NotAllowedError is an ErrNotAllowed with its Reason: the database's constraint name when the lifecycle itself refused, a word of the store's otherwise.
type NotAllowedError struct {
	Reason string
}

func (e *NotAllowedError) Error() string {
	return "the job's lifecycle does not allow this: " + e.Reason
}

// Is makes errors.Is(err, ErrNotAllowed) true.
func (e *NotAllowedError) Is(target error) bool { return target == ErrNotAllowed }

func notAllowed(reason string) error { return &NotAllowedError{Reason: reason} }

// opErr names the operation on an error: "jobs.confirm: ...". nil stays nil.
func opErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("jobs.%s: %w", op, err)
}

// The SQLSTATEs and constraint names the store maps; the harness tests pin each against a migrated database.
const (
	sqlstateRestrict    = "23001" // restrict_violation: the lifecycle guard (platform_0011) and the other guards of platform.jobs
	sqlstateUnique      = "23505"
	sqlstateFK          = "23503"
	sqlstateLockTimeout = "55P03"
	sqlstateNoData      = "P0002" // platform.job_transition: the job does not exist
	conSlot             = "jobs_one_active_per_user_module"
	conConfirmKey       = "jobs_confirm_key_key"
	conJobUserFK        = "jobs_user_id_fkey"
	reasonRestricted    = "restricted" // a 23001 refusal that names no constraint
)

// mapDBError maps a database refusal to the store's own error, BY SQLSTATE AND CONSTRAINT NAME, or returns nil when the error is not one the store knows. err may be wrapped; the first
// *pgconn.PgError in the chain is judged. A mapped error deliberately does not wrap the driver's error: a PgError's Detail line quotes the failing row.
func (s *Store) mapDBError(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return nil
	}
	switch {
	case pe.Code == sqlstateRestrict:
		reason := pe.ConstraintName
		if reason == "" {
			reason = reasonRestricted
		}
		return notAllowed(reason)
	case pe.Code == sqlstateUnique && pe.ConstraintName == conSlot:
		return ErrSlotTaken
	case pe.Code == sqlstateUnique && pe.ConstraintName == conConfirmKey:
		return ErrConfirmKeyReused
	case pe.Code == sqlstateFK && pe.ConstraintName == conJobUserFK:
		return ErrNotFound
	case pe.Code == sqlstateNoData:
		return ErrNotFound
	case pe.Code == sqlstateLockTimeout:
		return &money.LockTimeoutError{Wait: s.mny.LockTimeout()}
	}
	return nil
}

// fail turns a database error into the store's error: a known refusal becomes its named error, anything else is returned with the connection string and password taken out of its text
// (and still unwrappable, so errors.Is(err, context.Canceled) works). Errors that are already the store's (or money's) pass through unchanged.
func (s *Store) fail(err error) error {
	if err == nil {
		return nil
	}
	if mapped := s.mapDBError(err); mapped != nil {
		return mapped
	}
	return dsnscrub.Scrub(err, s.dsn)
}

// refusalWord is the one word a log line says about a refusal (never the error's text: that may name a constraint or a row's ordinal and is the caller's to show).
func refusalWord(err error) string {
	for _, c := range []struct {
		is   error
		word string
	}{
		{ErrNotFound, "not_found"}, {ErrBadRequest, "bad_request"}, {ErrTooManyRows, "too_many_rows"}, {ErrNothingToDo, "nothing_to_do"}, {ErrNotAllowed, "not_allowed"},
		{ErrStalePreview, "stale_preview"}, {ErrSlotTaken, "slot_taken"}, {ErrConfirmKeyReused, "confirm_key_reused"}, {ErrNotPreviewed, "not_previewed"},
		{ErrNoEntitlement, "no_entitlement"}, {ErrSuspended, "suspended"}, {ErrModuleUnavailable, "module_unavailable"}, {money.ErrLockTimeout, "lock_timeout"},
	} {
		if errors.Is(err, c.is) {
			return c.word
		}
	}
	var ic ErrInsufficientCredits
	if errors.As(err, &ic) {
		return "insufficient_credits"
	}
	var inf ErrInfeasible
	if errors.As(err, &inf) {
		return "infeasible"
	}
	return "error"
}
