package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxSourceBytes bounds the source of a reachability change (who said so: "config: PLATFORM_M1_VENDOR_REACHABLE"), after trimming. The history table keeps it as text; this keeps a hostile or
// runaway caller from filling it.
const MaxSourceBytes = 200

// maxUnitBytes bounds a unit's name before the database is asked: the units are short identifiers (m1_vendor), and a megabyte "unit" must cost no round trip.
const maxUnitBytes = 64

// The SQLSTATEs platform.set_unit_reachable (platform_0013) raises by itself. The function names no constraint for them, so these two codes ARE the contract (the other refusals of this package are
// mapped by constraint name, mapPgError); they are only ever read in the one statement that calls the function.
const (
	sqlstateInvalidParameter = "22023" // invalid_parameter_value: a NULL flag, a NULL or blank source
	sqlstateNoUnit           = "P0002" // no_data_found: no such unit (a NULL unit included)
)

// SetUnitReachable says whether a job of the unit's module can be charged the unit under the module's current configuration (platform.unit_costs.reachable). The flag decides which units a NEW reserve
// freezes into the job's price snapshot and therefore the reserve rate (Docs/phase-4-spec.md, Locked 23: module 1 with its vendor fallback off reserves at 1 credit per row, with it on at 2); it moves no
// job that already reserved, because a job's snapshot is frozen at its reserve.
//
// It is the ONE writer of the flag the platform has: it calls the SECURITY DEFINER function platform.set_unit_reachable, which platform_app may execute and which the table's own grants do not allow it to
// do by an UPDATE. changed is true when the flag moved (the database wrote one history row, with source), false when it already held (nothing is written, not even a history row): so the call is idempotent
// and any number of instances may make it with the same configuration.
//
// The call waits for a reserve in progress (the function row-locks the unit; a reserve share-locks every unit of the module) and a reserve waits for it, so a snapshot is never taken half-way through a
// flip. The wait is bounded like every money wait (ErrLockTimeout after the store's lock timeout, nothing written). The source is trimmed and must be 1 to MaxSourceBytes bytes of UTF-8 without NUL.
//
// Errors: ErrInvalid (an InvalidError naming "unit" or "source": a blank or over-long unit, a blank, over-long or non-UTF-8 source; decided before the database is asked), ErrUnknownUnit (the unit is not a
// row of platform.unit_costs), ErrLockTimeout. Anything else is the database's error with the connection string taken out.
func (s *Store) SetUnitReachable(ctx context.Context, unit string, reachable bool, source string) (changed bool, err error) {
	clean, err := validateReachable(unit, source)
	if err != nil {
		return false, opErr("set_unit_reachable", err)
	}
	err = s.run(ctx, func(tx pgx.Tx) error {
		var err error
		changed, err = s.setUnitReachable(ctx, tx, unit, reachable, clean)
		return err
	})
	if err != nil {
		return false, opErr("set_unit_reachable", err)
	}
	return changed, nil
}

// setUnitReachable is the transaction of SetUnitReachable: the bound first, like every money transaction, then the one statement. source is already validated.
func (s *Store) setUnitReachable(ctx context.Context, tx pgx.Tx, unit string, reachable bool, source string) (changed bool, err error) {
	if err := s.bound(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT platform.set_unit_reachable($1::text, $2::boolean, $3::text)`, unit, reachable, source).Scan(&changed); err != nil {
		return false, s.failReachable(err, unit)
	}
	return changed, nil
}

// validateReachable checks a SetUnitReachable request and returns the source as it is stored (trimmed). Both refusals are InvalidErrors that name the field.
func validateReachable(unit, source string) (string, error) {
	if !utf8.ValidString(unit) || strings.ContainsRune(unit, 0) {
		return "", invalid("unit", "must be valid UTF-8 text without NUL")
	}
	if strings.TrimSpace(unit) == "" {
		return "", invalid("unit", "must not be blank")
	}
	if len(unit) > maxUnitBytes {
		return "", invalid("unit", "is %d bytes, the limit is %d", len(unit), maxUnitBytes)
	}
	if !utf8.ValidString(source) || strings.ContainsRune(source, 0) {
		return "", invalid("source", "must be valid UTF-8 text without NUL")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return "", invalid("source", "must not be blank: a change of reachability says who said so")
	}
	if len(source) > MaxSourceBytes {
		return "", invalid("source", "is %d bytes, the limit is %d", len(source), MaxSourceBytes)
	}
	return source, nil
}

// failReachable turns the database's error from platform.set_unit_reachable into the store's own: the function's two refusals by SQLSTATE (see sqlstateNoUnit), anything else as every other statement of
// the store (a lock timeout, a scrubbed driver error). unit is only named in the error text, clipped.
func (s *Store) failReachable(err error, unit string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case sqlstateNoUnit:
			return fmt.Errorf("%w: %q", ErrUnknownUnit, clip(unit))
		case sqlstateInvalidParameter:
			// the Go checks above make this unreachable through the store; it is the database's second wall, reported as what it is
			return invalid("source", "refused by the database (SQLSTATE %s): a change of reachability needs a flag and a non-blank source", pe.Code)
		}
	}
	return s.fail(err, pgCtx{})
}
