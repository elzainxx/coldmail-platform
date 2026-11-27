// Package suppress is the ONE place that writes platform.suppressions.
//
// THE RULE (owner's ruling, Phase 4 Part A item A2). The table's CHECK (suppressions_email_norm_v1, platform_0009) cannot decode punycode, so for two address shapes (a label such as xn--a that is
// not valid punycode, and one whose decoded form breaks a rule: the vectors domain-invalid-punycode-short and domain-punycode-of-ascii-garbage) the Go validator, internal/norm Validate, is the
// ONLY wall: the database would store them if they reached it. So no code may write the table except the function in this file, and that function validates first. The gate
// scripts/check-suppression-writers.sh fails when any other non-test Go file under internal/ or cmd/, or any SQL under db/ outside db/migrations/, writes the table, and when a write in THIS
// file is not preceded by a call of the validator in the same function. The write statement is therefore written INSIDE Add, after the validation, not as a package-level constant above it.
//
// WHAT IS STORED. Always the string the validator returned (the normalised form), never the caller's input: a suppressed address and a verified address are then the same address. Rows are never
// updated. A row goes only through Remove (Phase 6, platform_0022): the user's own entry, found by the address the validator returned, and the database records the removal in
// platform.suppression_removals by a trigger, so this file cannot skip the history. A second Add of an address the user already suppresses changes nothing: the first row stands, its source
// included, and Add reports Created=false.
//
// THE FORMS OF A WRITE HERE. Add (one row), AddMany (a list: every address is validated first, then the valid ones are written in batches, each batch one statement) and Remove (one row). Each validates
// with the platform's rule BEFORE its write statement, in its own function, which is what the gate reads.
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
	"strings"
	"unicode/utf8"

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

// Remove removes the user's own entry for email and says whether there was one: true when exactly that row was removed, false when the user has no such entry. It validates the address FIRST, like Add
// (an address the platform's rule refuses is an *InvalidAddressError and no statement is issued; the refusal never holds the address), then removes the row of THIS user and of the address the validator
// returned: the statement names the user id in its WHERE, so another user's entry for the same address is never touched, never counted and never told apart from an absent one (the answer for it is the
// same false as for an address nobody suppressed). A user id that is not a uuid is ErrBadUser, decided before the database.
//
// An address the table can never hold (a NUL, bytes that are not UTF-8) is on nobody's list: the answer is false and no statement is issued. The removal is recorded by the database (platform_0022: a
// trigger writes the entry, with its source and its creation time, to platform.suppression_removals in the same statement), so a caller that removes an entry cannot leave no trace of it.
func Remove(ctx context.Context, q Execer, userID, email string) (bool, error) {
	normalised, err := norm.Validate(email)
	if err != nil {
		return false, opErr("remove", invalidAddress(err))
	}
	if !uuidShape.MatchString(userID) {
		return false, opErr("remove", ErrBadUser)
	}
	if q == nil {
		return false, opErr("remove", errNoExecer)
	}
	if strings.IndexByte(normalised, 0) >= 0 || !utf8.ValidString(normalised) {
		return false, nil
	}

	tag, err := q.Exec(ctx, `DELETE FROM platform.suppressions WHERE user_id = $1::uuid AND email = $2`, userID, normalised)
	if err != nil {
		if mapped := mapDBError(err); mapped != nil {
			return false, opErr("remove", mapped)
		}
		return false, opErr("remove", unexpected(err))
	}
	return tag.RowsAffected() == 1, nil
}

// AddBatchSize is how many addresses one statement of AddMany writes.
const AddBatchSize = 2000

// Rejected is an input AddMany did not write: the position in the list it was given (0-based) and why, in a word of the closed vocabulary of reasons.go. It never holds the address.
type Rejected struct {
	Index  int
	Reason string
}

// ManyResult is what AddMany did: how many addresses it wrote, how many the user already had (an address repeated in the list counts again here), and the inputs it refused.
type ManyResult struct {
	Added        int
	AlreadyThere int
	Rejected     []Rejected
}

// AddMany suppresses every address of emails for the user with the given source, as Add does for one: each is validated FIRST with the platform's whole rule (a refusal is a Rejected entry with its reason
// word and the address is never written), and only the valid ones, in the normalised form the validator returned, are written, AddBatchSize to a statement, with ON CONFLICT DO NOTHING (so an address the user
// already has changes nothing: the first row stands, its source included). Order does not matter and a repeat in the list is "already there".
//
// A valid address the table still refuses (ErrCannotStore: the CHECK is stricter than the validator in twenty listed cases) would fail its whole statement, so that batch is written again one address at a
// time through Add and each such address is Rejected with the reason cannot_store; the others of the batch are written. This needs a connection that survives a failed statement (the pool or a plain
// connection), not a transaction: a failed statement poisons a pgx.Tx. Any other database refusal (an unknown user, the system user, a source outside the vocabulary) ends the call with that error and
// the batches written so far stay written. A user id that is not a uuid and a source outside the vocabulary are decided before anything is validated or written.
func AddMany(ctx context.Context, q Execer, userID string, emails []string, source string) (ManyResult, error) {
	if !uuidShape.MatchString(userID) {
		return ManyResult{}, opErr("add_many", ErrBadUser)
	}
	if !validSource(source) {
		return ManyResult{}, opErr("add_many", ErrBadSource)
	}
	if q == nil {
		return ManyResult{}, opErr("add_many", errNoExecer)
	}
	var res ManyResult
	valid := make([]string, 0, len(emails))
	at := make([]int, 0, len(emails))
	for i, e := range emails {
		normalised, err := norm.Validate(e)
		if err != nil {
			res.Rejected = append(res.Rejected, Rejected{Index: i, Reason: ReasonOf(invalidAddress(err))})
			continue
		}
		if strings.IndexByte(normalised, 0) >= 0 || !utf8.ValidString(normalised) {
			res.Rejected = append(res.Rejected, Rejected{Index: i, Reason: ReasonCannotStore})
			continue
		}
		valid = append(valid, normalised)
		at = append(at, i)
	}

	for start := 0; start < len(valid); start += AddBatchSize {
		if err := ctx.Err(); err != nil {
			return res, opErr("add_many", err)
		}
		end := min(start+AddBatchSize, len(valid))
		batch := valid[start:end]
		tag, err := q.Exec(ctx, `INSERT INTO platform.suppressions (user_id, email, source)
			SELECT $1::uuid, e, $3 FROM unnest($2::text[]) AS t(e)
			ON CONFLICT DO NOTHING`, userID, batch, source)
		if err == nil {
			added := int(tag.RowsAffected())
			res.Added += added
			res.AlreadyThere += len(batch) - added
			continue
		}
		mapped := mapDBError(err)
		if !errors.Is(mapped, ErrCannotStore) {
			if mapped != nil {
				return res, opErr("add_many", mapped)
			}
			return res, opErr("add_many", unexpected(err))
		}
		for j, e := range batch {
			one, err := Add(ctx, q, userID, e, source)
			switch {
			case err == nil && one.Created:
				res.Added++
			case err == nil:
				res.AlreadyThere++
			case errors.Is(err, ErrCannotStore):
				res.Rejected = append(res.Rejected, Rejected{Index: at[start+j], Reason: ReasonCannotStore})
			default:
				return res, err
			}
		}
	}
	return res, nil
}
