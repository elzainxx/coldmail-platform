package entitlements

import (
	"errors"
	"fmt"
)

// The named refusals of the store. A caller (the admin surface, a test) branches on these with errors.Is and
// never on message text; every database refusal is mapped by SQLSTATE and CONSTRAINT NAME, not by the
// server's wording.
var (
	// ErrInvalid is the base of every refusal made before the database is asked: a malformed id, a reason
	// that is too long, a missing key. The specific errors below wrap it, so errors.Is(err, ErrInvalid)
	// answers "the request itself is wrong".
	ErrInvalid = errors.New("invalid request")

	// ErrSystemUser: the all-zero uuid (module 2's shared-pool owner, no login) can hold no entitlement
	// (platform_0004, ledger_not_the_system_user).
	ErrSystemUser = errors.New("the system user cannot hold an entitlement")
	// ErrUntilNotFuture: the enabled-until date must lie at least one second after the DATABASE's now().
	ErrUntilNotFuture = errors.New("the enabled-until date must be at least one second in the future by the database's clock")
	// ErrReasonTooLong: a reason is at most MaxReasonBytes bytes.
	ErrReasonTooLong = errors.New("the reason is too long")

	// ErrAdminDisabled: the named admin was disabled; the database refuses to record an action by one
	// (platform_0004, constraint admin_is_active).
	ErrAdminDisabled = errors.New("the admin is disabled and can no longer act")
	// ErrUnknownAdmin: no such admin (foreign key on platform.ledger.acting_admin_id).
	ErrUnknownAdmin = errors.New("no such admin")
	// ErrUnknownUser: no such user (foreign key on platform.ledger.user_id).
	ErrUnknownUser = errors.New("no such user")
	// ErrIdempotencyMismatch: the key was used before for a DIFFERENT request. The same key with the same
	// parameters is a replay and returns the original entry instead.
	ErrIdempotencyMismatch = errors.New("the idempotency key was already used for a different request")
)

// invalid wraps a specific refusal under ErrInvalid so that both errors.Is(err, ErrInvalid) and
// errors.Is(err, specific) hold. detail never carries a secret: callers pass field names, not values.
func invalid(specific error, detail string) error {
	if detail == "" {
		return fmt.Errorf("%w: %w", ErrInvalid, specific)
	}
	return fmt.Errorf("%w: %w (%s)", ErrInvalid, specific, detail)
}
