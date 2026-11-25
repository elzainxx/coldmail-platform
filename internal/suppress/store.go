// Package suppress is the ONE place that writes platform.suppressions.
//
// THE RULE (owner's ruling, Phase 4 Part A item A2). The table's CHECK (suppressions_email_norm_v1, platform_0009) cannot decode punycode, so for two address shapes (a label such as xn--a that is
// not valid punycode, and one whose decoded form breaks a rule: the vectors domain-invalid-punycode-short and domain-punycode-of-ascii-garbage) the Go validator, internal/norm Validate, is the
// ONLY wall: the database would store them if they reached it. So no code may write the table except the function in this file, and that function validates first. The gate
// scripts/check-suppression-writers.sh fails when any other non-test Go file under internal/ or cmd/, or any SQL under db/ outside db/migrations/, writes the table, and when a write in THIS
// file is not preceded by a call of the validator in the same function. The write statement is therefore written INSIDE Add, after the validation, not as a package-level constant above it.
//
// WHAT IS STORED. Always the string the validator returned (the normalised form), never the caller's input: a suppressed address and a verified address are then the same address. Rows are never
// updated and nothing deletes one (the table's comment; a removal flow is not designed). A second Add of an address the user already suppresses changes nothing: the first row stands, its source
// included, and Add reports Created=false.
//
// WHAT THE DATABASE STILL REFUSES. The CHECK is stricter than the validator in twenty listed cases (Docs/install.md, "One address rule"); Add reports those as ErrCannotStore (SQLSTATE 23514, or
// 22021 for a NUL). It also refuses the system user (ErrSystemUser) and an unknown user (ErrUnknownUser). Every one is mapped by constraint name (errors.go), and no error ever carries the address.
//
// THE CONNECTION. Add takes an Execer and Filter a Querier: the platform's pool (a login as platform_app), a *pgx.Conn and a pgx.Tx all satisfy them, so a caller that writes a suppression inside
// its own transaction passes its pgx.Tx and owns the commit and the rollback (like the money store's In functions). Add issues exactly one statement and takes no lock.
//
// This file holds no block comments on purpose: the gate reads code and cannot tell a block comment from code, so it refuses one in this file.
package suppress

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/norm"
)

// The vocabulary of the source column (the table's CHECK suppressions_source_vocabulary).
const (
	// SourceUnsubscribe: the recipient asked not to be mailed.
	SourceUnsubscribe = "unsubscribe"
	// SourceManual: the user added the address by hand.
	SourceManual = "manual"
	// SourceImport: the address came in with an imported list.
	SourceImport = "import"
)

// Execer is what Add needs of a connection: *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Result is what Add did. On an error it is the zero value.
type Result struct {
	// Email is the NORMALISED address the table was offered (what the validator returned): the form to ask about it again with, in Filter or anywhere else.
	Email string
	// Created is true when this call inserted the row, false when the user already had the address suppressed (the existing row stands, source included).
	Created bool
}

var (
	errNoExecer = errors.New("suppress: no database connection")
	uuidShape   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func validSource(s string) bool {
	return s == SourceUnsubscribe || s == SourceManual || s == SourceImport
}

// Add suppresses email for the user: it validates the address FIRST (the platform's whole address rule: norm/v1, then module 1's syntax layer), and only a valid address is offered to the
// table, in the normalised form the validator returned. An address the rule refuses is an *InvalidAddressError (errors.Is(err, ErrInvalidAddress), norm.ErrMalformed or norm.ErrSyntax; the
// reason is its Reason field) and no statement is issued: the refusal is decided before the database is asked, and its text never holds the address. A user id that is not a uuid is
// ErrBadUser and a source outside the vocabulary is ErrBadSource, both decided before the database too.
//
// The statement is one insert with ON CONFLICT DO NOTHING and every value a parameter, so a second Add of the same (user, address) is not an error: it returns Created=false. A refusal by
// the database is mapped by constraint name: ErrCannotStore, ErrSystemUser, ErrUnknownUser (see errors.go).
func Add(ctx context.Context, q Execer, userID, email, source string) (Result, error) {
	normalised, err := norm.Validate(email)
	if err != nil {
		return Result{}, opErr("add", invalidAddress(err))
	}
	if !uuidShape.MatchString(userID) {
		return Result{}, opErr("add", ErrBadUser)
	}
	if !validSource(source) {
		return Result{}, opErr("add", ErrBadSource)
	}
	if q == nil {
		return Result{}, opErr("add", errNoExecer)
	}

	// The one write of the platform to this table (scripts/check-suppression-writers.sh keeps it the only one). Written here, after the validation above, on purpose.
	tag, err := q.Exec(ctx, `INSERT INTO platform.suppressions (user_id, email, source)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT DO NOTHING`, userID, normalised, source)
	if err != nil {
		if mapped := mapDBError(err); mapped != nil {
			return Result{}, opErr("add", mapped)
		}
		return Result{}, opErr("add", unexpected(err))
	}
	return Result{Email: normalised, Created: tag.RowsAffected() == 1}, nil
}
