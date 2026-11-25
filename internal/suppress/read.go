package suppress

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// FilterBatchSize is how many addresses one Filter query asks about. A job's recipient list is checked in batches of this size, never one query per address.
const FilterBatchSize = 5000

// Querier is what Filter needs of a connection: *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it (the same small interface the money and entitlement stores read through).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Filter returns which of emails the user has suppressed: a map holding exactly the suppressed ones (value true), never nil on success. The addresses must already be NORMALISED (what the
// validator returned, what Result.Email holds): they are compared as given, so an address in another spelling is simply not found; Filter does not normalise and does not validate. It asks the
// table in batches of FilterBatchSize with `email = ANY($2)`, one query per batch (none for an empty list), reading only the user's own rows: another user's suppression of the same address
// is never returned. A repeated address is asked once.
//
// An address the table can never hold (a NUL, bytes that are not UTF-8) cannot be suppressed, so it is not asked about: sending it would fail its whole batch with SQLSTATE 22021.
// A user id that is not a uuid is ErrBadUser, decided before the database is asked.
//
// This is a read: the gate scripts/check-suppression-writers.sh polices writes only.
func Filter(ctx context.Context, q Querier, userID string, emails []string) (map[string]bool, error) {
	if !uuidShape.MatchString(userID) {
		return nil, opErr("filter", ErrBadUser)
	}
	if q == nil {
		return nil, opErr("filter", errNoExecer)
	}
	out := make(map[string]bool)
	asked := make(map[string]struct{}, len(emails))
	batch := make([]string, 0, min(len(emails), FilterBatchSize))
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var found []string
		err := q.QueryRow(ctx, `SELECT COALESCE(array_agg(email), ARRAY[]::text[])
			FROM platform.suppressions
			WHERE user_id = $1::uuid AND email = ANY($2::text[])`, userID, batch).Scan(&found)
		if err != nil {
			if mapped := mapDBError(err); mapped != nil {
				return opErr("filter", mapped)
			}
			return opErr("filter", unexpected(err))
		}
		for _, e := range found {
			out[e] = true
		}
		batch = batch[:0]
		return nil
	}
	for _, e := range emails {
		if strings.IndexByte(e, 0) >= 0 || !utf8.ValidString(e) {
			continue
		}
		if _, dup := asked[e]; dup {
			continue
		}
		asked[e] = struct{}{}
		batch = append(batch, e)
		if len(batch) == FilterBatchSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}
