package suppress

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/norm"
)

// The errors of the store. A caller tests them with errors.Is (the sentinels) or errors.As (InvalidAddressError); every error the store returns is wrapped with the operation
// ("suppress.add: ...") and NONE of them ever carries the address: a refusal names its reason and, for a database refusal, the constraint, never the row.
//
// A refusal that has a NAME in the database (a constraint) is mapped by the constraint's name, never by the text of the message: the text is the server's wording and changes, the name is the
// contract (mapDBError). A mapped error deliberately does NOT wrap the driver's error: a *pgconn.PgError carries a Detail line that quotes the failing row (the address included).
var (
	// ErrInvalid is wrapped by every refusal made BEFORE the database was asked: a malformed user id, a source outside the vocabulary, an address the platform's rule refuses.
	ErrInvalid = errors.New("invalid request")
	// ErrInvalidAddress: the address fails the platform's one address rule (norm.Validate: norm/v1, then module 1's syntax layer). Always wrapped by an *InvalidAddressError, which also
	// unwraps to the norm error (errors.Is(err, norm.ErrMalformed) or norm.ErrSyntax) so a caller can tell the two halves apart. It matches ErrInvalid.
	ErrInvalidAddress = fmt.Errorf("%w: the address is refused by the platform's address rule", ErrInvalid)
	// ErrBadUser: the user id is not a uuid. It matches ErrInvalid.
	ErrBadUser = fmt.Errorf("%w: the user id is not a uuid", ErrInvalid)
	// ErrBadSource: the source is not one of the vocabulary the table's CHECK allows (SourceUnsubscribe, SourceManual, SourceImport). It matches ErrInvalid.
	ErrBadSource = fmt.Errorf("%w: the source is not one of unsubscribe, manual, import", ErrInvalid)

	// ErrCannotStore: the address passed the validator and the table refuses it all the same. The table's CHECK (suppressions_email_norm_v1, SQLSTATE 23514) is STRICTER than the validator in
	// twenty listed cases (a NUL, white space in the local part, an empty label or a leading dot in the domain, an address over 254 bytes: Docs/install.md, "One address rule"), and a NUL
	// never reaches the CHECK (SQLSTATE 22021). The address is valid and cannot be suppressed here; it is not a bug of the caller's input.
	ErrCannotStore = errors.New("the address is valid but the suppressions table cannot store it")
	// ErrSystemUser: the all-zero uuid (module 2's shared-pool owner, no login) can hold no suppression (platform_0004, constraint suppressions_not_the_system_user).
	ErrSystemUser = errors.New("the system user cannot hold a suppression")
	// ErrUnknownUser: no such user (foreign key suppressions_user_id_fkey, SQLSTATE 23503).
	ErrUnknownUser = errors.New("no such user")
	// ErrConflict: a unique violation reached the caller. Unreachable through Add (its statement says ON CONFLICT DO NOTHING, so a duplicate is reported as Created=false); it is the database's
	// second wall, reported by name like the money store's.
	ErrConflict = errors.New("an earlier or concurrent write holds the same key")
)

// InvalidAddressError is a refusal of the platform's address rule: Reason is the stable word norm gives (empty, no_at, multiple_at, empty_local, empty_domain, bad_idna, internal, part_empty,
// local_too_long, domain_too_long, domain_no_dot). Its text carries the reason and nothing of the address. It matches ErrInvalidAddress, ErrInvalid, and the norm error it wraps
// (norm.ErrMalformed or norm.ErrSyntax, norm.ReasonAny(err)).
type InvalidAddressError struct {
	Reason norm.Reason
	cause  error
}

func (e *InvalidAddressError) Error() string {
	return fmt.Sprintf("%s: %s", ErrInvalidAddress.Error(), e.Reason)
}

// Unwrap returns the sentinel and the norm error, so errors.Is and errors.As reach both.
func (e *InvalidAddressError) Unwrap() []error { return []error{ErrInvalidAddress, e.cause} }

func invalidAddress(err error) error {
	r, _ := norm.ReasonAny(err) // "" for an error that is neither half of the rule: still a refusal
	return &InvalidAddressError{Reason: r, cause: err}
}

// opErr names the operation on an error: "suppress.add: ...". nil stays nil.
func opErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("suppress.%s: %w", op, err)
}

// The SQLSTATEs and constraint names the store maps. They are the names the migrations declare (or PostgreSQL derives for a column foreign key); the harness test pins each against a migrated
// database.
const (
	sqlstateInvalidByteSequence = "22021" // a NUL (or invalid UTF-8) in a text value: refused before any CHECK runs
	sqlstateCheckViolation      = "23514"
	sqlstateFKViolation         = "23503"
	sqlstateUniqueViolation     = "23505"

	conEmailNorm  = "suppressions_email_norm_v1"
	conSystemUser = "suppressions_not_the_system_user"
	conSource     = "suppressions_source_vocabulary"
	conUserFK     = "suppressions_user_id_fkey"
	conPK         = "suppressions_pk"
)

// mapDBError maps a database refusal to the store's own, BY SQLSTATE AND CONSTRAINT NAME, or returns nil when the error is not a *pgconn.PgError the store knows (the caller then calls
// unexpected). err may be wrapped; the first *pgconn.PgError in the chain is judged.
func mapDBError(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return nil
	}
	switch {
	case pe.Code == sqlstateInvalidByteSequence:
		return fmt.Errorf("%w (SQLSTATE %s: the text holds a byte the database cannot store)", ErrCannotStore, pe.Code)
	case pe.Code == sqlstateCheckViolation && pe.ConstraintName == conEmailNorm:
		return fmt.Errorf("%w (SQLSTATE %s, constraint %s)", ErrCannotStore, pe.Code, pe.ConstraintName)
	case pe.Code == sqlstateCheckViolation && pe.ConstraintName == conSystemUser:
		return ErrSystemUser
	case pe.Code == sqlstateCheckViolation && pe.ConstraintName == conSource:
		return fmt.Errorf("%w (refused by the database: constraint %s)", ErrBadSource, pe.ConstraintName)
	case pe.Code == sqlstateFKViolation && pe.ConstraintName == conUserFK:
		return ErrUnknownUser
	case pe.Code == sqlstateUniqueViolation && pe.ConstraintName == conPK:
		return fmt.Errorf("%w (SQLSTATE %s, constraint %s)", ErrConflict, pe.Code, pe.ConstraintName)
	}
	return nil
}

// unexpected turns an error the store does not map into its own. A database error is reported by SQLSTATE, constraint and the server's MESSAGE and nothing else (never the Detail line, which
// quotes the row), and does not wrap the driver's error; any other error (a cancelled context, a closed connection) is returned as it is, so errors.Is(err, context.Canceled) still works.
func unexpected(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("unexpected database refusal (SQLSTATE %s, constraint %q): %s", pe.Code, pe.ConstraintName, pe.Message)
	}
	return err
}
