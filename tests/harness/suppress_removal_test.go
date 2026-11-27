//go:build harness

package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/suppress"
)

// Phase 6 A4, the suppression list a user reads, removes from and imports into (internal/suppress List, Remove, AddMany, ImportCSV; platform_0022), against a REAL PostgreSQL, over a pool of real logins as
// platform_app (what the service runs as) with a superuser connection that seeds and observes. What a fake cannot show and these tests do: that user A never sees, removes or counts user B's rows; that `%`, `_`
// and `\` in a search are three characters and never a pattern; that the keyset paging neither repeats nor skips a row (two rows with one creation time included); that a removal writes exactly one history row
// with the entry's own source and time, by a TRIGGER (so even a superuser's DELETE writes it); that the history is append-only and written by nobody else; that Remove, like Add, refuses an address the rule refuses
// BEFORE it asks the database (a row of the punycode shape the table's CHECK cannot see survives a removal of that spelling); and the counts and the reasons of an import against the real CHECK.
// Every refusal has its paired control. Every name starts with `sprm` so that it cannot clash with another file's.

const sprmSource = "SELECT email FROM platform.suppressions WHERE user_id = $1 ORDER BY created_at DESC, email DESC"

func sprmStore(t *testing.T, e *mnyEnv) *suppress.Store {
	t.Helper()
	s, err := suppress.NewStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sprmSeed(t *testing.T, e *mnyEnv, user, source string, emails ...string) {
	t.Helper()
	for _, a := range emails {
		supstAddOK(t, e.pool, user, a, source)
	}
}

func sprmEmails(es []suppress.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Email
	}
	return out
}

func sprmPG(err error) (code, msg string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.Message
	}
	return "", ""
}

// requireSQLState fails unless err is a database refusal with the SQLSTATE.
func sprmRequireSQLState(t *testing.T, what string, err error, want string) {
	t.Helper()
	if code, msg := sprmPG(err); code != want {
		t.Fatalf("%s: SQLSTATE %q (%s), want %s; err %v", what, code, msg, want, err)
	}
}

func TestSuppressRemoval_ListIsTheCallersOwnAndASearchIsNeverAPattern(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	sprmSeed(t, e, A, "manual", "alpha@example.com", "pct%off@example.com", "pctxoff@example.com", "u_s@example.com", "uxs@example.com", `bs\x@example.com`, "shared@example.com")
	sprmSeed(t, e, B, "unsubscribe", "only-b@example.com", "shared@example.com", "alphab@example.com")

	list := func(user, q string) []string {
		t.Helper()
		page, _, err := s.List(ctx, user, q, "", 100)
		if err != nil {
			t.Fatalf("List(%q) as %s: %v", q, user, err)
		}
		return supstSet(sprmEmails(page))
	}
	// the caller's own list, and only it
	if got := list(A, ""); len(got) != 7 || strings.Contains(strings.Join(got, ","), "only-b") || strings.Contains(strings.Join(got, ","), "alphab") {
		t.Fatalf("A's list is %v: it must be A's seven and nothing of B's", got)
	}
	if got := list(B, ""); len(got) != 3 || strings.Contains(strings.Join(got, ","), "alpha@") {
		t.Fatalf("B's list is %v: it must be B's three and nothing of A's", got)
	}
	// a search can only narrow the caller's own list: B's address is found by B and by nobody else (the control and the refusal)
	if got := list(B, "only-b"); len(got) != 1 || got[0] != "only-b@example.com" {
		t.Fatalf("control: B's own search found %v", got)
	}
	if got := list(A, "only-b"); len(got) != 0 {
		t.Fatalf("A's search found %v of B's list", got)
	}
	// anywhere in the address, in any letter case, and a prefix is a substring
	if got := list(A, "ALPHA@"); len(got) != 1 || got[0] != "alpha@example.com" {
		t.Fatalf("a search in upper case found %v", got)
	}
	if got := list(A, "ample.com"); len(got) != 7 {
		t.Fatalf("a search of the domain's end found %d of A's seven", len(got))
	}
	// the wildcards of LIKE are three characters: each finds the one address that holds it and NOT the neighbour a pattern would also match
	for _, c := range []struct{ q, want string }{{"pct%", "pct%off@example.com"}, {"u_s", "u_s@example.com"}, {`bs\`, `bs\x@example.com`}, {"%", "pct%off@example.com"}, {"_", "u_s@example.com"}, {`\`, `bs\x@example.com`}} {
		if got := list(A, c.q); len(got) != 1 || got[0] != c.want {
			t.Errorf("the search %q found %v, want exactly %q (a wildcard would have found more)", c.q, got, c.want)
		}
	}
	if got := list(A, "%zz%"); len(got) != 0 {
		t.Errorf("a search of percent signs around text found %v", got)
	}
	// hostile text is only text
	for _, q := range []string{"'; DROP TABLE platform.suppressions; --", "' OR '1'='1", `"; --`} {
		if got := list(A, q); len(got) != 0 {
			t.Errorf("the search %q found %v", q, got)
		}
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions"); n != 10 {
		t.Fatalf("the table holds %d rows after the hostile searches, want 10", n)
	}
	// the refusals of the store, before the database: a search over 254 bytes, a forged cursor
	if _, _, err := s.List(ctx, A, strings.Repeat("a", 255), "", 10); !errors.Is(err, suppress.ErrBadQuery) {
		t.Fatalf("a search of 255 bytes: %v", err)
	}
	if _, _, err := s.List(ctx, A, "", "not-a-cursor", 10); !errors.Is(err, suppress.ErrBadCursor) {
		t.Fatalf("a forged cursor: %v", err)
	}
}

func TestSuppressRemoval_PagingWalksTheWholeListOnceInOrderAndACursorCannotLeaveTheCallersList(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	names := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}
	for _, n := range names {
		sprmSeed(t, e, A, "import", n+"@example.com")
	}
	sprmSeed(t, e, B, "manual", "b1@example.com", "b2@example.com", "b3@example.com")
	e.exec("UPDATE platform.suppressions SET created_at = timestamptz '2025-12-31 00:00:00+00' WHERE user_id = $1", B) // older than every row of A's: after A's cursor, in the newest-first order
	// distinct times for most, one time shared by three rows (the keyset must break the tie by address, or a page boundary inside it repeats or skips)
	for i, n := range names {
		minute := i
		if i >= 3 && i <= 5 {
			minute = 3 // p4, p5, p6 share a creation time
		}
		e.exec("UPDATE platform.suppressions SET created_at = timestamptz '2026-01-01 00:00:00+00' + make_interval(mins => $1) WHERE user_id = $2 AND email = $3", minute, A, n+"@example.com")
	}
	want := e.rows(sprmSource, A)
	if len(want) != 8 {
		t.Fatalf("the oracle holds %d rows", len(want))
	}
	for _, size := range []int{1, 2, 3, 5, 8, 100} {
		var got []string
		cursor, pages := "", 0
		for {
			page, next, err := s.List(ctx, A, "", cursor, size)
			if err != nil {
				t.Fatalf("size %d, page %d: %v", size, pages+1, err)
			}
			if len(page) > size || len(page) == 0 {
				t.Fatalf("size %d: a page of %d rows", size, len(page))
			}
			got = append(got, sprmEmails(page)...)
			pages++
			if next == "" {
				break
			}
			cursor = next
			if pages > 20 {
				t.Fatalf("size %d: the walk does not end", size)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("size %d: the walk read %v, the table's order is %v (a row was repeated, skipped or out of order)", size, got, want)
		}
		if wantPages := (8 + size - 1) / size; pages != wantPages {
			t.Fatalf("size %d: %d pages, want %d (the last page must not point at an empty one)", size, pages, wantPages)
		}
	}
	// a cursor of A's, handed to B, can only point somewhere in B's own list
	_, next, err := s.List(ctx, A, "", "", 2)
	if err != nil || next == "" {
		t.Fatalf("A's first page: next %q err %v", next, err)
	}
	page, _, err := s.List(ctx, B, "", next, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := supstSet(sprmEmails(page)); strings.Join(got, ",") != "b1@example.com,b2@example.com,b3@example.com" {
		t.Fatalf("a cursor of A's gave B %v, want exactly B's three (older than the cursor) and nothing of A's", got)
	}
	// the control: B without a cursor holds three
	if all, _, _ := s.List(ctx, B, "", "", 100); len(all) != 3 {
		t.Fatalf("control: B's list holds %d", len(all))
	}
	// a page is at most 100 rows whatever is asked
	e.exec("INSERT INTO platform.suppressions (user_id, email, source) SELECT $1, 'bulk' || g || '@example.com', 'import' FROM generate_series(1, 150) g", B)
	page, next, err = s.List(ctx, B, "", "", 100000)
	if err != nil || len(page) != 100 || next == "" {
		t.Fatalf("a limit of 100000 gave %d rows, next %q, %v: want 100 and another page", len(page), next, err)
	}
	// every entry says where it came from and when, as the table has it
	got, ok, err := suppress.Get(ctx, e.pool, A, "p1@example.com")
	if err != nil || !ok || got.Source != "import" || got.CreatedAt.Before(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Get: %+v %v %v", got, ok, err)
	}
	if _, ok, _ := suppress.Get(ctx, e.pool, B, "p1@example.com"); ok {
		t.Fatal("Get found A's address on B's list")
	}
}

func TestSuppressRemoval_RemoveIsScopedAndWritesExactlyOneHistoryRowWithTheEntrysOwnSourceAndTime(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	sprmSeed(t, e, A, "unsubscribe", "same@example.com")
	sprmSeed(t, e, A, "manual", "a-only@example.com")
	sprmSeed(t, e, B, "import", "same@example.com")
	e.exec("UPDATE platform.suppressions SET created_at = timestamptz '2026-03-04 05:06:07.123456+00' WHERE user_id = $1 AND email = 'same@example.com'", A)
	e.exec("UPDATE platform.suppressions SET created_at = timestamptz '2026-05-06 07:08:09.654321+00' WHERE user_id = $1 AND email = 'same@example.com'", B)
	history := func(user string) []string {
		return e.rows("SELECT email || '|' || source || '|' || to_char(entry_created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') FROM platform.suppression_removals WHERE user_id = $1 ORDER BY id", user)
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals"); n != 0 {
		t.Fatalf("the history holds %d row(s) before any removal", n)
	}

	// B asks to remove an address that is ONLY on A's list: false, A's entry stays, nothing is recorded, and the answer is the one for an address nobody suppressed
	removed, err := s.Remove(ctx, B, "a-only@example.com")
	if err != nil || removed {
		t.Fatalf("B removing A's address: %v %v", removed, err)
	}
	if absent, err := s.Remove(ctx, B, "nobody-has-this@example.com"); err != nil || absent != removed {
		t.Fatalf("an address nobody suppressed: %v %v (must be the same answer as another user's address)", absent, err)
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE user_id = $1 AND email = 'a-only@example.com'", A); n != 1 {
		t.Fatal("B's removal took A's entry")
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals"); n != 0 {
		t.Fatalf("a removal that removed nothing wrote %d history row(s)", n)
	}

	// B removes their own entry of the same address, spelled differently: it goes, A's entry of the same address stays, and the history is B's entry's own
	before := e.str("SELECT clock_timestamp()::text")
	removed, err = s.Remove(ctx, B, "  SAME@Example.COM ")
	after := e.str("SELECT clock_timestamp()::text")
	if err != nil || !removed {
		t.Fatalf("B removing their own entry: %v %v", removed, err)
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE user_id = $1 AND email = 'same@example.com'", A); n != 1 {
		t.Fatal("B's removal of the shared address took A's entry too")
	}
	if got := history(B); len(got) != 1 || got[0] != "same@example.com|import|2026-05-06 07:08:09.654321" {
		t.Fatalf("B's history is %v, want exactly one row: the address, the entry's own source (import) and its own creation time", got)
	}
	if got := history(A); len(got) != 0 {
		t.Fatalf("B's removal wrote %v on A's history", got)
	}
	if n := e.int(fmt.Sprintf("SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1 AND removed_at >= '%s'::timestamptz AND removed_at <= '%s'::timestamptz", before, after), B); n != 1 { // the two instants are the database's own text
		t.Fatalf("the removal's time is not the database's clock between %s and %s", before, after)
	}
	// the same removal twice is false the second time and writes nothing more
	if again, err := s.Remove(ctx, B, "same@example.com"); err != nil || again {
		t.Fatalf("a second removal: %v %v", again, err)
	}
	if got := history(B); len(got) != 1 {
		t.Fatalf("a second removal changed B's history: %v", got)
	}

	// A removes theirs: the source is the unsubscribe it came from, and each user has exactly one row
	if removed, err = s.Remove(ctx, A, "same@example.com"); err != nil || !removed {
		t.Fatalf("A removing their own entry: %v %v", removed, err)
	}
	if got := history(A); len(got) != 1 || got[0] != "same@example.com|unsubscribe|2026-03-04 05:06:07.123456" {
		t.Fatalf("A's history is %v", got)
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals"); n != 2 {
		t.Fatalf("the history holds %d rows after two removals, want 2", n)
	}
	// and what was removed is gone from its owner's list and only from it
	if rows := e.rows("SELECT user_id::text || '|' || email FROM platform.suppressions ORDER BY 1"); len(rows) != 1 || rows[0] != A+"|a-only@example.com" {
		t.Fatalf("the table holds %v, want only A's a-only@example.com", rows)
	}
}

func TestSuppressRemoval_RemoveValidatesTheAddressBeforeAskingTheDatabase(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	A := mnyUserA
	// the row a validator-less writer could leave behind: a punycode label that is not punycode, which the table's CHECK cannot see (the Go validator is the only wall: install.md, "One address rule"); the superuser plants it
	const odd = "user@xn--a.com"
	e.exec("INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')", A, odd)
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE email = $1", odd); n != 1 {
		t.Fatalf("the database refused to hold the shape the test needs (%d rows): the premise of the test is gone", n)
	}
	for _, bad := range []string{odd, "", "no-at", "a@@b.com", "x@y"} {
		removed, err := suppress.Remove(ctx, e.pool, A, bad)
		var inv *suppress.InvalidAddressError
		if !errors.As(err, &inv) || removed {
			t.Errorf("Remove(%q): %v %v, want an *InvalidAddressError and false", bad, removed, err)
		}
		if err != nil && bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("Remove(%q): the error holds the address: %v", bad, err)
		}
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE email = $1", odd); n != 1 {
		t.Fatal("a removal of an address the rule refuses reached the table and took the row: the address was not validated first")
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals"); n != 0 {
		t.Fatalf("%d history row(s) for removals that were refused", n)
	}
	// the control: the same call with a valid address, whose row exists, removes it
	e.exec("INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, 'fine@example.com', 'manual')", A)
	if removed, err := suppress.Remove(ctx, e.pool, A, "fine@example.com"); err != nil || !removed {
		t.Fatalf("control: %v %v", removed, err)
	}
	// an address the table can never hold (a NUL) is on nobody's list: false, no error, no statement the database would refuse
	if removed, err := suppress.Remove(ctx, e.pool, A, "a\x00b@example.com"); err != nil || removed {
		t.Fatalf("an address with a NUL: %v %v", removed, err)
	}
}

func TestSuppressRemoval_TheHistoryIsAppendOnlyWrittenByNobodyButTheTriggerAndTheRolesHoldExactlyTheirPrivileges(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	sprmSeed(t, e, A, "manual", "a1@example.com", "a2@example.com")
	sprmSeed(t, e, B, "manual", "b1@example.com")

	// platform_app (a real login): may remove a row of the list, still may not change one, reads the history and cannot write it
	if _, err := e.pool.Exec(ctx, "DELETE FROM platform.suppressions WHERE user_id = $1 AND email = 'a1@example.com'", A); err != nil {
		t.Fatalf("control: platform_app cannot delete a row of the list: %v", err)
	}
	if n := e.int("SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1 AND email = 'a1@example.com'", A); n != 1 {
		t.Fatalf("a DELETE by platform_app wrote %d history row(s) (the trigger, not the caller, must write it), want 1", n)
	}
	_, err := e.pool.Exec(ctx, "UPDATE platform.suppressions SET source = 'import' WHERE user_id = $1", A)
	sprmRequireSQLState(t, "platform_app UPDATE of the list", err, "42501")
	for what, sql := range map[string]string{
		"INSERT":   "INSERT INTO platform.suppression_removals (user_id, email, source, entry_created_at) VALUES ($1, 'forged@example.com', 'manual', now())",
		"UPDATE":   "UPDATE platform.suppression_removals SET source = 'import' WHERE user_id = $1",
		"DELETE":   "DELETE FROM platform.suppression_removals WHERE user_id = $1",
		"TRUNCATE": "TRUNCATE platform.suppression_removals",
	} {
		var err error
		if what == "TRUNCATE" {
			_, err = e.pool.Exec(ctx, sql)
		} else {
			_, err = e.pool.Exec(ctx, sql, A)
		}
		sprmRequireSQLState(t, "platform_app "+what+" of the history", err, "42501")
	}
	var n int64
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM platform.suppression_removals").Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: platform_app reads the history: %d, %v", n, err)
	}

	// platform_admin reads both and removes nothing
	tx := ledgerBegin(t, e.admin)
	ledgerSwitchRole(t, tx, "platform_admin")
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.suppression_removals"); got != 1 {
		t.Fatalf("platform_admin reads %d history row(s)", got)
	}
	ledgerExpectRefused(t, tx, "platform_admin DELETE of the list", "42501", ledgerWhy{Message: "permission denied for table suppressions"}, "DELETE FROM platform.suppressions")
	ledgerExpectRefused(t, tx, "platform_admin INSERT into the history", "42501", ledgerWhy{Message: "permission denied for table suppression_removals"},
		"INSERT INTO platform.suppression_removals (user_id, email, source, entry_created_at) VALUES ($1, 'x@example.com', 'manual', now())", A)
	ledgerResetRole(t, tx)

	// the owner and a superuser are refused a change of the history by the triggers, and their DELETE of the list writes the history all the same: no writer can skip it
	ledgerExpectRefused(t, tx, "UPDATE of the history by the superuser", "23001", ledgerWhy{Message: "append-only"}, "UPDATE platform.suppression_removals SET source = 'import'")
	ledgerExpectRefused(t, tx, "DELETE of the history by the superuser", "23001", ledgerWhy{Message: "append-only"}, "DELETE FROM platform.suppression_removals")
	ledgerExpectRefused(t, tx, "TRUNCATE of the history by the superuser", "23001", ledgerWhy{Message: "append-only"}, "TRUNCATE platform.suppression_removals")
	ledgerExpectOK(t, tx, "the superuser's DELETE of a row of the list", "DELETE FROM platform.suppressions WHERE user_id = $1 AND email = 'b1@example.com'", B)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1 AND email = 'b1@example.com' AND source = 'manual'", B); got != 1 {
		t.Fatalf("a superuser's DELETE wrote %d history row(s), want 1: the trigger must fire for every writer", got)
	}
	// a DELETE that removes many rows writes one history row each
	ledgerExpectOK(t, tx, "a DELETE of the rest of A's list", "DELETE FROM platform.suppressions WHERE user_id = $1", A)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1", A); got != 2 {
		t.Fatalf("A's history holds %d rows, want 2 (a1 and a2)", got)
	}
}

func TestSuppressRemoval_ImportCountsAndNamesTheReasonOfEveryInvalidLineAgainstTheRealTable(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	sprmSeed(t, e, A, "unsubscribe", "known@example.com")
	sprmSeed(t, e, B, "manual", "ada@example.com")
	csv := "email,name\n" + // line 1
		"Ada@Example.com,Ada\n" + // 2 added (B's identical address is B's own row: it does not make this one "already there")
		"known@example.com,Known\n" + // 3 already there (the first row stands, its source included)
		"bob@example.com,Bob\n" + // 4 added
		"BOB@example.com,Bob again\n" + // 5 already there (named twice in the file)
		"no-at-sign,X\n" + // 6 invalid: no_at
		",Blank\n" + // 7 invalid: empty
		"a b@example.com,Space\n" + // 8 invalid: cannot_store (the validator accepts it, the table's CHECK refuses white space in the local part)
		"cy@example.com\n" // 9 added (a row with only the email cell)
	res, err := s.Import(ctx, A, strings.NewReader(csv), 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 3 || res.AlreadyThere != 2 || res.Invalid != 3 {
		t.Fatalf("the counts are %+v, want 3 added, 2 already there and 3 invalid", res)
	}
	var sample []string
	for _, l := range res.Sample {
		sample = append(sample, fmt.Sprintf("%d:%s", l.Line, l.Reason))
	}
	if strings.Join(sample, ",") != "6:no_at,7:empty,8:cannot_store" {
		t.Fatalf("the sample is %v", sample)
	}
	if rows := e.rows("SELECT email || '|' || source FROM platform.suppressions WHERE user_id = $1 ORDER BY email", A); strings.Join(rows, ",") != "ada@example.com|import,bob@example.com|import,cy@example.com|import,known@example.com|unsubscribe" {
		t.Fatalf("A's list is %v: the new entries are `import`, and the entry the user already had keeps its own source", rows)
	}
	if rows := e.rows("SELECT email || '|' || source FROM platform.suppressions WHERE user_id = $1 ORDER BY email", B); strings.Join(rows, ",") != "ada@example.com|manual" {
		t.Fatalf("B's list is %v: an import of A's touched it", rows)
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE email LIKE '% %'"); n != 0 {
		t.Fatal("the address with white space was stored")
	}
	// importing the same file again is harmless: everything that was written is "already there", and the invalid lines are the same
	again, err := s.Import(ctx, A, strings.NewReader(csv), 100)
	if err != nil || again.Added != 0 || again.AlreadyThere != 5 || again.Invalid != 3 {
		t.Fatalf("the second import: %+v %v, want 0 added, 5 already there and 3 invalid", again, err)
	}
	// the refusals of the file: the limit and the header; nothing is written
	total := e.int("SELECT count(*) FROM platform.suppressions")
	var many *suppress.TooManyRowsError
	if _, err := s.Import(ctx, A, strings.NewReader("email\nz1@example.com\nz2@example.com\nz3@example.com\n"), 2); !errors.As(err, &many) || many.Limit != 2 {
		t.Fatalf("three rows against a limit of two: %v", err)
	}
	var head *suppress.ImportHeaderError
	if _, err := s.Import(ctx, A, strings.NewReader("name\nz1@example.com\n"), 10); !errors.As(err, &head) || head.Reason != suppress.HeaderNoEmail {
		t.Fatalf("a file with no email column: %v", err)
	}
	if got := e.int("SELECT count(*) FROM platform.suppressions"); got != total {
		t.Fatalf("a refused file wrote %d row(s)", got-total)
	}
}

func TestSuppressRemoval_ABigImportIsWrittenInBatchesAndOneBadAddressSpoilsNeitherItsBatchNorTheRest(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A := mnyUserA
	var b strings.Builder
	b.WriteString("email\n")
	n := suppress.AddBatchSize*2 + 500 // three batches
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "big%05d@example.com\n", i)
		if i == 10 { // a valid address the table cannot hold, inside the first batch
			b.WriteString("has space@example.com\n")
		}
	}
	res, err := s.Import(ctx, A, strings.NewReader(b.String()), n+10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != n || res.AlreadyThere != 0 || res.Invalid != 1 || len(res.Sample) != 1 || res.Sample[0].Reason != suppress.ReasonCannotStore || res.Sample[0].Line != 13 {
		t.Fatalf("result %+v, want %d added and the one address with white space invalid at line 13 as cannot_store", res, n)
	}
	if got := e.int("SELECT count(*) FROM platform.suppressions WHERE user_id = $1 AND source = 'import'", A); got != int64(n) {
		t.Fatalf("the table holds %d imported rows, want %d", got, n)
	}
	// the control of "the statement failed whole and was retried one by one": the same list without the bad address needs no retry and gives the same counts
	again, err := s.Import(ctx, A, strings.NewReader(strings.Replace(b.String(), "has space@example.com\n", "", 1)), n+10)
	if err != nil || again.Added != 0 || again.AlreadyThere != n || again.Invalid != 0 {
		t.Fatalf("the second import: %+v %v", again, err)
	}
}

func TestSuppressRemoval_StoreAddReturnsTheRowThatStandsAndAddManyIsScopedToItsUser(t *testing.T) {
	e := newMnyEnv(t)
	s := sprmStore(t, e)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	sprmSeed(t, e, A, "unsubscribe", "was-unsubscribed@example.com")
	// Add of an address the user already has returns THAT row: its source (unsubscribe) and its time, not a manual one
	entry, created, err := s.Add(ctx, A, " Was-Unsubscribed@EXAMPLE.com")
	if err != nil || created || entry.Source != "unsubscribe" || entry.Email != "was-unsubscribed@example.com" {
		t.Fatalf("Add of an address the user already had: %+v %v %v", entry, created, err)
	}
	// a new address is created as manual, and the same address for the other user is that user's own row
	entry, created, err = s.Add(ctx, A, "fresh@example.com")
	if err != nil || !created || entry.Source != "manual" || entry.CreatedAt.IsZero() {
		t.Fatalf("Add of a new address: %+v %v %v", entry, created, err)
	}
	if _, created, err = s.Add(ctx, B, "fresh@example.com"); err != nil || !created {
		t.Fatalf("the same address for B: %v %v", created, err)
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE email = 'fresh@example.com'"); n != 2 {
		t.Fatalf("%d rows for the address, want one per user", n)
	}
	// an address the rule refuses is the store's refusal, and nothing was written
	before := e.int("SELECT count(*) FROM platform.suppressions")
	var inv *suppress.InvalidAddressError
	if _, _, err := s.Add(ctx, A, "not-an-address"); !errors.As(err, &inv) || inv.Reason != "no_at" {
		t.Fatalf("an invalid address: %v", err)
	}
	if e.int("SELECT count(*) FROM platform.suppressions") != before {
		t.Fatal("a refused address was written")
	}
	// AddMany for A writes only A's rows even when the list holds B's addresses
	res, err := suppress.AddMany(ctx, e.pool, A, []string{"m1@example.com", "m2@example.com", "fresh@example.com"}, suppress.SourceImport)
	if err != nil || res.Added != 2 || res.AlreadyThere != 1 {
		t.Fatalf("AddMany: %+v %v", res, err)
	}
	if n := e.int("SELECT count(*) FROM platform.suppressions WHERE user_id = $1 AND email IN ('m1@example.com', 'm2@example.com')", B); n != 0 {
		t.Fatal("AddMany for A wrote rows of B")
	}
}
