package suppress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// ---- the list a user reads ------------------------------------------------------------------------------------------------

// Entry is one suppressed address as the user sees it: the normalised address, where it came from (SourceUnsubscribe, SourceManual or SourceImport) and when it was suppressed.
type Entry struct {
	Email     string
	Source    string
	CreatedAt time.Time
}

// The bounds of a page of the list and of a search.
const (
	// DefaultListLimit is the page size when the caller names none; MaxListLimit is the largest page. A limit above it is cut to it, one below one is the default.
	DefaultListLimit = 50
	MaxListLimit     = 100
	// MaxQueryBytes is the longest search text: the longest address the table holds (254 bytes). A longer one cannot match anything and is refused, not cut.
	MaxQueryBytes = 254
	// maxCursorBytes bounds a cursor before it is decoded: an address, an instant and a few separators, base64-encoded.
	maxCursorBytes = 512
)

// Lister is what List and Get need of a connection beyond Querier: *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it.
type Lister interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrBadQuery: the search text is longer than any address, holds a NUL or is not UTF-8. It matches ErrInvalid.
var ErrBadQuery = fmt.Errorf("%w: the search text is not usable (longer than 254 bytes, a NUL or not UTF-8)", ErrInvalid)

// ErrBadCursor: the cursor is not one List gave. It matches ErrInvalid.
var ErrBadCursor = fmt.Errorf("%w: the cursor is not one this list gave", ErrInvalid)

// List returns one page of the user's own suppressions, newest first (by created_at, then by address, both descending: a key that is unique, so a page boundary never repeats or skips a row), and the
// cursor of the next page ("" on the last one). It reads only the rows of userID: another user's suppression of the same address is never returned, and a search text can only narrow the user's own list.
//
// query is a SEARCH, not a pattern: it is lower-cased (the stored form is lower-case) and a row matches when it occurs anywhere in the address, which covers a prefix. It is a parameter of the
// position test strpos(email, $2) > 0, so `%`, `_` and `\` are three more characters and never a wildcard (no LIKE is involved), and nothing of it is SQL. An empty query matches every row.
//
// cursor is the opaque text a previous call returned: it names the (created_at, address) of the last row of that page and the next page starts after it. A cursor that is not exactly one this function
// writes is ErrBadCursor (it can only point somewhere in the user's own list: the user id is in the WHERE whatever the cursor says). limit is DefaultListLimit when it is below one and at most
// MaxListLimit. A user id that is not a uuid is ErrBadUser, decided before the database. This is a read: the gate scripts/check-suppression-writers.sh polices writes only.
func List(ctx context.Context, q Lister, userID, query, cursor string, limit int) ([]Entry, string, error) {
	if !uuidShape.MatchString(userID) {
		return nil, "", opErr("list", ErrBadUser)
	}
	if q == nil {
		return nil, "", opErr("list", errNoExecer)
	}
	// the text is judged BEFORE its case is folded: ToLower turns a byte that is not UTF-8 into U+FFFD, which would then pass
	if !utf8.ValidString(query) || strings.IndexByte(query, 0) >= 0 {
		return nil, "", opErr("list", ErrBadQuery)
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if len(query) > MaxQueryBytes {
		return nil, "", opErr("list", ErrBadQuery)
	}
	var afterAt *time.Time
	var afterEmail *string
	if cursor != "" {
		at, email, ok := decodeCursor(cursor)
		if !ok {
			return nil, "", opErr("list", ErrBadCursor)
		}
		afterAt, afterEmail = &at, &email
	}
	switch {
	case limit < 1:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	// one more than the page: its presence says that another page follows, so the last page never points at an empty one
	rows, err := q.Query(ctx, `SELECT email, source, created_at FROM platform.suppressions
		WHERE user_id = $1::uuid
		  AND ($2::text = '' OR strpos(email, $2::text) > 0)
		  AND ($3::timestamptz IS NULL OR (created_at, email) < ($3::timestamptz, $4::text))
		ORDER BY created_at DESC, email DESC
		LIMIT $5`, userID, query, afterAt, afterEmail, limit+1)
	if err != nil {
		return nil, "", listErr(err)
	}
	defer rows.Close()
	out := make([]Entry, 0, limit+1)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Email, &e.Source, &e.CreatedAt); err != nil {
			return nil, "", listErr(err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", listErr(err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.Email)
	}
	return out, next, nil
}

func listErr(err error) error {
	if mapped := mapDBError(err); mapped != nil {
		return opErr("list", mapped)
	}
	return opErr("list", unexpected(err))
}

// Get returns the user's own entry for an address that is already NORMALISED (what Result.Email holds), and whether there is one. It does not validate: an address in another spelling is simply not found.
func Get(ctx context.Context, q Querier, userID, email string) (Entry, bool, error) {
	if !uuidShape.MatchString(userID) {
		return Entry{}, false, opErr("get", ErrBadUser)
	}
	if q == nil {
		return Entry{}, false, opErr("get", errNoExecer)
	}
	if strings.IndexByte(email, 0) >= 0 || !utf8.ValidString(email) {
		return Entry{}, false, nil
	}
	e := Entry{Email: email}
	err := q.QueryRow(ctx, `SELECT source, created_at FROM platform.suppressions WHERE user_id = $1::uuid AND email = $2`, userID, email).Scan(&e.Source, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		if mapped := mapDBError(err); mapped != nil {
			return Entry{}, false, opErr("get", mapped)
		}
		return Entry{}, false, opErr("get", unexpected(err))
	}
	e.CreatedAt = e.CreatedAt.UTC()
	return e, true, nil
}

// The cursor is base64url (no padding) of "1|<unix microseconds>|<address>": a version, the instant of the last row of the page (the table keeps microseconds) and its address. decodeCursor is strict:
// anything that is not exactly what encodeCursor writes (another version, a number that is not canonical, an address that is not UTF-8 or holds a NUL, a re-encoding that differs) is refused.
func encodeCursor(at time.Time, email string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("1|" + strconv.FormatInt(at.UnixMicro(), 10) + "|" + email))
}

func decodeCursor(cursor string) (time.Time, string, bool) {
	if len(cursor) > maxCursorBytes {
		return time.Time{}, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 || parts[0] != "1" || parts[2] == "" || len(parts[2]) > MaxQueryBytes || strings.IndexByte(parts[2], 0) >= 0 || !utf8.ValidString(parts[2]) {
		return time.Time{}, "", false
	}
	micro, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || strconv.FormatInt(micro, 10) != parts[1] {
		return time.Time{}, "", false
	}
	at := time.UnixMicro(micro).UTC()
	if encodeCursor(at, parts[2]) != cursor {
		return time.Time{}, "", false
	}
	return at, parts[2], true
}
