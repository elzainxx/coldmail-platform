package suppress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/coldmail-platform/platform/internal/norm"
)

// Phase 6 A4: the list a user reads, the removal of an entry and the import. These tests need no database, like the others of this package: they cover what is decided BEFORE a statement is issued, what
// the statements are (every value a parameter, the user in every WHERE), and how the answers are counted. Every refusal has its paired control (the same call with a good input reaches the fake); what only a
// real PostgreSQL can show (that user A never sees user B's rows, that `%` is data, that the trigger writes the history) is in tests/harness/suppress_removal_test.go.

// scriptExec is an Execer whose answer is a function of the statement and its arguments; it records every call.
type scriptExec struct {
	calls []execCall
	fn    func(n int, sql string, args []any) (pgconn.CommandTag, error)
}

func (s *scriptExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, execCall{sql: sql, args: args})
	return s.fn(len(s.calls), sql, args)
}

func tagOf(verb string, n int) pgconn.CommandTag {
	if verb == "INSERT" {
		return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", n))
	}
	return pgconn.NewCommandTag(fmt.Sprintf("%s %d", verb, n))
}

// ---- Remove ----------------------------------------------------------------------------------------------------------------

func TestRemove_ValidatesTheAddressBeforeAnyStatement(t *testing.T) {
	for _, bad := range []string{"", "   ", "no-at-sign", "a@b@c.com", "@example.com", "user@", "user@example", "user@xn--a.com", strings.Repeat("a", 70) + "@example.com"} {
		f := &fakeExec{}
		removed, err := Remove(context.Background(), f, userA, bad)
		var inv *InvalidAddressError
		if !errors.As(err, &inv) || !errors.Is(err, ErrInvalidAddress) || removed {
			t.Errorf("%q: removed=%v err=%v, want an *InvalidAddressError and false", bad, removed, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%q: %d statement(s) were issued for an address the rule refuses", bad, len(f.calls))
		}
		if err != nil && strings.TrimSpace(bad) != "" && strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Errorf("%q: the error holds the address: %v", bad, err)
		}
	}
	// the control: a valid address reaches the database, in the normalised form, as the one statement
	f := &fakeExec{tag: "DELETE 1"}
	removed, err := Remove(context.Background(), f, userA, "  Jane.Doe@EXAMPLE.com ")
	if err != nil || !removed || len(f.calls) != 1 {
		t.Fatalf("control: removed=%v err=%v after %d statement(s)", removed, err, len(f.calls))
	}
	if a := f.calls[0].args; len(a) != 2 || a[0] != userA || a[1] != "jane.doe@example.com" {
		t.Fatalf("control: the arguments are %v, want [%s jane.doe@example.com]", a, userA)
	}
}

func TestRemove_NamesTheUserInTheStatementAndSaysOnlyWhetherARowWentAway(t *testing.T) {
	for name, c := range map[string]struct {
		tag  string
		want bool
	}{
		"the user's own entry":                      {"DELETE 1", true},
		"no such entry (absent, or another user's)": {"DELETE 0", false},
	} {
		f := &fakeExec{tag: c.tag}
		got, err := Remove(context.Background(), f, userA, "a@example.com")
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v, want %v", name, got, err, c.want)
		}
		sql := f.calls[0].sql
		if !strings.Contains(sql, "user_id = $1::uuid") || !strings.Contains(sql, "email = $2") || strings.Count(sql, "$") != 2 || !strings.HasPrefix(strings.TrimSpace(sql), "DELETE FROM platform.suppressions") {
			t.Errorf("%s: the statement is not the user-scoped removal of one address: %q", name, sql)
		}
		if strings.Contains(sql, "a@example.com") || strings.Contains(sql, userA) {
			t.Errorf("%s: a value is in the statement text, not a parameter: %q", name, sql)
		}
	}
	// two users asking for the same address send their OWN id: the statement a user's removal issues never carries another's
	fa, fb := &fakeExec{tag: "DELETE 1"}, &fakeExec{tag: "DELETE 0"}
	if _, err := Remove(context.Background(), fa, userA, "same@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(context.Background(), fb, userB, "same@example.com"); err != nil {
		t.Fatal(err)
	}
	if fa.calls[0].args[0] != userA || fb.calls[0].args[0] != userB {
		t.Fatalf("the user ids sent are %v and %v", fa.calls[0].args[0], fb.calls[0].args[0])
	}
}

func TestRemove_RefusesAUserItCannotUseAndAnAddressTheTableCannotHoldBeforeTheDatabase(t *testing.T) {
	f := &fakeExec{tag: "DELETE 1"}
	for _, u := range []string{"", "not-a-uuid", userA + "0", strings.ToUpper(userA) + "x"} {
		if _, err := Remove(context.Background(), f, u, "a@example.com"); !errors.Is(err, ErrBadUser) {
			t.Errorf("user %q: %v, want ErrBadUser", u, err)
		}
	}
	if _, err := Remove(context.Background(), nil, userA, "a@example.com"); err == nil {
		t.Error("a nil connection is not an error")
	}
	// an address that passes the rule and holds a NUL can never be on the list: false, and the database is not asked (it would fail the statement with 22021)
	removed, err := Remove(context.Background(), f, userA, "a\x00b@example.com")
	if err != nil || removed {
		t.Errorf("an address with a NUL: removed=%v err=%v, want false and no error", removed, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("%d statement(s) for refusals decided before the database", len(f.calls))
	}
	if ok, err := Remove(context.Background(), f, userA, "a@example.com"); err != nil || !ok || len(f.calls) != 1 {
		t.Fatalf("control: %v %v after %d statement(s)", ok, err, len(f.calls))
	}
}

func TestRemove_MapsADatabaseRefusalWithoutTheAddress(t *testing.T) {
	canary := "canary-address-7f3a@example.com"
	f := &fakeExec{err: pgErr("23503", "suppressions_user_id_fkey", "boom", "Key (email)=("+canary+")")}
	_, err := Remove(context.Background(), f, userA, canary)
	if !errors.Is(err, ErrUnknownUser) || strings.Contains(err.Error(), canary) {
		t.Fatalf("a foreign-key refusal: %v", err)
	}
	f = &fakeExec{err: pgErr("XX000", "x", "failed for "+"row", "Key ("+canary+")")}
	_, err = Remove(context.Background(), f, userA, canary)
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("an unexpected refusal: %v", err)
	}
	// the control: a cancelled context keeps its identity
	f = &fakeExec{err: context.Canceled}
	if _, err := Remove(context.Background(), f, userA, canary); !errors.Is(err, context.Canceled) {
		t.Fatalf("control: %v", err)
	}
}

// ---- AddMany ---------------------------------------------------------------------------------------------------------------

func TestAddMany_ValidatesEveryAddressFirstAndWritesOnlyTheValidOnesNormalised(t *testing.T) {
	var sent [][]string
	f := &scriptExec{fn: func(_ int, _ string, args []any) (pgconn.CommandTag, error) {
		sent = append(sent, args[1].([]string))
		return tagOf("INSERT", len(args[1].([]string))), nil
	}}
	emails := []string{"A@Example.com", "bad", "  b@EXAMPLE.com ", "c@@d.com", "e@example.com", "x@y"}
	res, err := AddMany(context.Background(), f, userA, emails, SourceImport)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 3 || res.AlreadyThere != 0 || len(res.Rejected) != 3 {
		t.Fatalf("result %+v, want 3 added and 3 rejected", res)
	}
	if len(sent) != 1 || strings.Join(sent[0], ",") != "a@example.com,b@example.com,e@example.com" {
		t.Fatalf("the statement carried %v, want the three valid addresses in their normalised form", sent)
	}
	var got []string
	for _, r := range res.Rejected {
		got = append(got, fmt.Sprintf("%d:%s", r.Index, r.Reason))
	}
	if strings.Join(got, ",") != "1:no_at,3:multiple_at,5:domain_no_dot" {
		t.Fatalf("rejected = %v", got)
	}
	if f.calls[0].args[0] != userA || f.calls[0].args[2] != SourceImport {
		t.Fatalf("the statement's user and source are %v and %v", f.calls[0].args[0], f.calls[0].args[2])
	}
	// the control of "nothing is written for an address the rule refuses": a list of only refusals issues no statement
	g := &scriptExec{fn: func(int, string, []any) (pgconn.CommandTag, error) { return tagOf("INSERT", 0), nil }}
	res, err = AddMany(context.Background(), g, userA, []string{"bad", "", "x@"}, SourceManual)
	if err != nil || len(g.calls) != 0 || len(res.Rejected) != 3 || res.Added != 0 {
		t.Fatalf("a list of refusals: %+v %v after %d statement(s)", res, err, len(g.calls))
	}
}

func TestAddMany_CountsWhatTheUserAlreadyHadAndWritesInBatches(t *testing.T) {
	n := AddBatchSize*2 + 5
	emails := make([]string, n)
	for i := range emails {
		emails[i] = fmt.Sprintf("u%d@example.com", i)
	}
	f := &scriptExec{fn: func(call int, _ string, args []any) (pgconn.CommandTag, error) {
		size := len(args[1].([]string))
		if call == 1 {
			return tagOf("INSERT", size-10), nil // ten of the first batch were already there
		}
		return tagOf("INSERT", size), nil
	}}
	res, err := AddMany(context.Background(), f, userA, emails, SourceImport)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("%d statements for %d addresses at %d a batch, want 3", len(f.calls), n, AddBatchSize)
	}
	if res.Added != n-10 || res.AlreadyThere != 10 || len(res.Rejected) != 0 {
		t.Fatalf("result %+v, want %d added and 10 already there", res, n-10)
	}
	if got := len(f.calls[0].args[1].([]string)) + len(f.calls[1].args[1].([]string)) + len(f.calls[2].args[1].([]string)); got != n {
		t.Fatalf("the batches carry %d addresses, want %d", got, n)
	}
}

func TestAddMany_ABatchTheTableRefusesIsWrittenOneAddressAtATimeAndOnlyTheRefusedOnesAreRejected(t *testing.T) {
	// the CHECK is stricter than the validator: here one address of the batch is one the table cannot store
	f := &scriptExec{fn: func(_ int, _ string, args []any) (pgconn.CommandTag, error) {
		if _, ok := args[1].([]string); ok { // the batch statement
			return pgconn.CommandTag{}, pgErr("23514", "suppressions_email_norm_v1", "violates", "Key (email)=(secret@example.com)")
		}
		if args[1] == "bad row@example.com" || args[1] == "bad row@example.com " {
			return pgconn.CommandTag{}, pgErr("23514", "suppressions_email_norm_v1", "violates", "")
		}
		return tagOf("INSERT", 1), nil
	}}
	res, err := AddMany(context.Background(), f, userA, []string{"a@example.com", "bad row@example.com", "c@example.com"}, SourceImport)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || len(res.Rejected) != 1 || res.Rejected[0].Index != 1 || res.Rejected[0].Reason != ReasonCannotStore {
		t.Fatalf("result %+v, want 2 added and the second address rejected as cannot_store", res)
	}
	if len(f.calls) != 4 { // the batch, then the three, one by one
		t.Fatalf("%d statements, want the batch and three single writes", len(f.calls))
	}
	// the control: another refusal of the database (an unknown user) is not a reason to try the addresses one by one: it ends the call with the error
	g := &scriptExec{fn: func(int, string, []any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, pgErr("23503", "suppressions_user_id_fkey", "no such user", "")
	}}
	_, err = AddMany(context.Background(), g, userA, []string{"a@example.com", "b@example.com"}, SourceImport)
	if !errors.Is(err, ErrUnknownUser) || len(g.calls) != 1 {
		t.Fatalf("an unknown user: %v after %d statement(s)", err, len(g.calls))
	}
}

func TestAddMany_RefusesWhatIsWrongWithTheCallBeforeAnythingIsValidatedOrWritten(t *testing.T) {
	f := &scriptExec{fn: func(int, string, []any) (pgconn.CommandTag, error) { return tagOf("INSERT", 1), nil }}
	if _, err := AddMany(context.Background(), f, "x", []string{"a@example.com"}, SourceImport); !errors.Is(err, ErrBadUser) {
		t.Errorf("a bad user: %v", err)
	}
	if _, err := AddMany(context.Background(), f, userA, []string{"a@example.com"}, "whim"); !errors.Is(err, ErrBadSource) {
		t.Errorf("a bad source: %v", err)
	}
	if _, err := AddMany(context.Background(), nil, userA, []string{"a@example.com"}, SourceImport); err == nil {
		t.Error("no connection is not an error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("%d statement(s) for refused calls", len(f.calls))
	}
	// a NUL passes the rule and cannot be stored: it is rejected here, never sent (it would fail its whole batch with 22021)
	res, err := AddMany(context.Background(), f, userA, []string{"a\x00@example.com", "b@example.com"}, SourceImport)
	if err != nil || res.Added != 1 || len(res.Rejected) != 1 || res.Rejected[0].Reason != ReasonCannotStore || res.Rejected[0].Index != 0 {
		t.Fatalf("a NUL: %+v %v", res, err)
	}
	if got := f.calls[0].args[1].([]string); len(got) != 1 || got[0] != "b@example.com" {
		t.Fatalf("the statement carried %v", got)
	}
	// a cancelled context stops before the next batch is written
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &scriptExec{fn: func(int, string, []any) (pgconn.CommandTag, error) { return tagOf("INSERT", 1), nil }}
	if _, err := AddMany(ctx, h, userA, []string{"a@example.com"}, SourceImport); !errors.Is(err, context.Canceled) || len(h.calls) != 0 {
		t.Fatalf("a cancelled context: %v after %d statement(s)", err, len(h.calls))
	}
}

// ---- the closed vocabulary of reasons -------------------------------------------------------------------------------------------

func TestReasons_TheVocabularyIsClosedAndEveryRefusalOfTheRuleHasAWordOfIt(t *testing.T) {
	want := map[string]bool{"empty": true, "no_at": true, "multiple_at": true, "empty_local": true, "empty_domain": true, "bad_idna": true, "part_empty": true, "local_too_long": true,
		"domain_too_long": true, "domain_no_dot": true, "cannot_store": true, "bad_line": true, "invalid_address": true}
	got := Reasons()
	if len(got) != len(want) {
		t.Fatalf("the vocabulary has %d words, want %d: %v", len(got), len(want), got)
	}
	for _, w := range got {
		if !want[w] {
			t.Errorf("the word %q is not one of the documented ones (a new word needs the documents and the copy catalogue)", w)
		}
	}
	// every reason norm can give maps to a word of the list (the "internal" one, which never happens, to the catch-all), and an error that is not a refusal of an address maps to nothing
	for _, r := range append(norm.Reasons(), norm.SyntaxReasons()...) {
		mapped := ReasonOf(invalidAddress(&norm.MalformedError{Reason: r}))
		if syn := map[norm.Reason]bool{norm.ReasonPartEmpty: true, norm.ReasonLocalTooLong: true, norm.ReasonDomainTooLong: true, norm.ReasonDomainNoDot: true}; syn[r] {
			mapped = ReasonOf(invalidAddress(&norm.SyntaxError{Reason: r}))
		}
		if !want[mapped] {
			t.Errorf("the reason %q maps to %q, which is not in the vocabulary", r, mapped)
		}
		if r == norm.ReasonInternal && mapped != ReasonInvalidAddress {
			t.Errorf("the internal reason maps to %q, want %q", mapped, ReasonInvalidAddress)
		}
		if r != norm.ReasonInternal && mapped != string(r) {
			t.Errorf("the reason %q maps to %q, want itself", r, mapped)
		}
	}
	if ReasonOf(nil) != "" || ReasonOf(errors.New("the database is gone")) != "" || ReasonOf(fmt.Errorf("x: %w", ErrCannotStore)) != ReasonCannotStore {
		t.Fatal("ReasonOf: nil and a foreign error map to nothing, ErrCannotStore to cannot_store")
	}
}

// ---- List ------------------------------------------------------------------------------------------------------------------

// fakeRows is the rows of a List query: the entries to hand out, in order.
type fakeRows struct {
	entries []Entry
	i       int
	err     error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool {
	if r.i >= len(r.entries) {
		return false
	}
	r.i++
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	e := r.entries[r.i-1]
	if len(dest) != 3 {
		return fmt.Errorf("fakeRows: %d destinations, want 3", len(dest))
	}
	*(dest[0].(*string)), *(dest[1].(*string)), *(dest[2].(*time.Time)) = e.Email, e.Source, e.CreatedAt
	return nil
}
func (r *fakeRows) Values() ([]any, error) { return nil, nil }
func (r *fakeRows) RawValues() [][]byte    { return nil }
func (r *fakeRows) Conn() *pgx.Conn        { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map   { return nil }

type listCall struct {
	sql  string
	args []any
}

// fakeLister answers every query with the entries it holds (cut at the LIMIT the statement carries, as the database would) and records the call.
type fakeLister struct {
	entries []Entry
	calls   []listCall
	err     error
}

func (f *fakeLister) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.calls = append(f.calls, listCall{sql, args})
	if f.err != nil {
		return nil, f.err
	}
	limit := args[len(args)-1].(int)
	out := f.entries
	if len(out) > limit {
		out = out[:limit]
	}
	return &fakeRows{entries: out}, nil
}

func entries(n int, at time.Time) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{Email: fmt.Sprintf("u%03d@example.com", n-i), Source: SourceManual, CreatedAt: at.Add(-time.Duration(i) * time.Second)}
	}
	return out
}

func TestList_AQueryIsAParameterNeverTheStatementAndNeverAPattern(t *testing.T) {
	f := &fakeLister{}
	var statements []string
	for _, q := range []string{"", "a@example", "%", "_", `\`, "100%_\\", `x' OR '1'='1`, "'; DROP TABLE platform.suppressions; --", "  MiXeD@Example.COM  "} {
		if _, _, err := List(context.Background(), f, userA, q, "", 10); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		c := f.calls[len(f.calls)-1]
		statements = append(statements, c.sql)
		want := strings.ToLower(strings.TrimSpace(q))
		if c.args[0] != userA || c.args[1] != want {
			t.Errorf("query %q: the user and the search sent are %v and %q, want %s and %q", q, c.args[0], c.args[1], userA, want)
		}
		if trimmed := strings.TrimSpace(q); len(trimmed) >= 5 && strings.Contains(c.sql, trimmed) { // a shorter text (a single "%") is a character the statement holds anyway
			t.Errorf("query %q is in the statement text", q)
		}
	}
	for _, s := range statements {
		if s != statements[0] {
			t.Fatalf("the statement changed with the search text:\n%s\n%s", statements[0], s)
		}
	}
	// not a pattern: no LIKE of any kind, the search is a position test on a parameter, and the user is in the WHERE
	up := strings.ToUpper(statements[0])
	if strings.Contains(up, "LIKE") || strings.Contains(up, "SIMILAR") || strings.Contains(up, " ~ ") || !strings.Contains(statements[0], "strpos(email, $2::text) > 0") || !strings.Contains(statements[0], "user_id = $1::uuid") {
		t.Fatalf("the statement is not the scoped position test: %s", statements[0])
	}
}

func TestList_RefusesWhatItCannotUseBeforeTheDatabase(t *testing.T) {
	f := &fakeLister{}
	long := strings.Repeat("a", MaxQueryBytes+1)
	for name, c := range map[string]struct {
		user, q, cursor string
		want            error
	}{
		"a user that is not a uuid":  {"nope", "", "", ErrBadUser},
		"a search over 254 bytes":    {userA, long, "", ErrBadQuery},
		"a search with a NUL":        {userA, "a\x00b", "", ErrBadQuery},
		"a search that is not UTF-8": {userA, "a\xffb", "", ErrBadQuery},
		"a cursor nobody gave":       {userA, "", "not-a-cursor", ErrBadCursor},
	} {
		if _, _, err := List(context.Background(), f, c.user, c.q, c.cursor, 10); !errors.Is(err, c.want) || !errors.Is(err, ErrInvalid) && c.want != ErrBadUser {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, _, err := List(context.Background(), nil, userA, "", "", 10); err == nil {
		t.Error("no connection is not an error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("%d query(ies) for refusals decided before the database", len(f.calls))
	}
	// the controls: a search of exactly 254 bytes and the plainest call reach it
	if _, _, err := List(context.Background(), f, userA, strings.Repeat("a", MaxQueryBytes), "", 10); err != nil || len(f.calls) != 1 {
		t.Fatalf("control: %v after %d query(ies)", err, len(f.calls))
	}
}

func TestList_APageIsCutAtTheLimitAndTheCursorPointsAfterItsLastRow(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 123456000, time.UTC)
	all := entries(5, at)
	f := &fakeLister{entries: all}
	page, next, err := List(context.Background(), f, userA, "", "", 3)
	if err != nil || len(page) != 3 || next == "" {
		t.Fatalf("first page: %d rows, next %q, %v", len(page), next, err)
	}
	if f.calls[0].args[len(f.calls[0].args)-1] != 4 {
		t.Fatalf("the statement asked for %v rows, want the page and one more (4)", f.calls[0].args[len(f.calls[0].args)-1])
	}
	// the cursor round-trips to exactly the last row of the page, and the next call sends it as the position
	cat, cemail, ok := decodeCursor(next)
	if !ok || !cat.Equal(page[2].CreatedAt) || cemail != page[2].Email {
		t.Fatalf("the cursor says (%v, %q), the last row is (%v, %q)", cat, cemail, page[2].CreatedAt, page[2].Email)
	}
	if _, _, err := List(context.Background(), f, userA, "", next, 3); err != nil {
		t.Fatal(err)
	}
	a := f.calls[1].args
	if got, _ := a[2].(*time.Time); got == nil || !got.Equal(cat) {
		t.Fatalf("the position sent is %v, want %v", a[2], cat)
	}
	if got, _ := a[3].(*string); got == nil || *got != cemail {
		t.Fatalf("the address sent is %v, want %q", a[3], cemail)
	}
	// the last page names no cursor, and the first call named no position
	short := &fakeLister{entries: all[:2]}
	page, next, err = List(context.Background(), short, userA, "", "", 3)
	if err != nil || len(page) != 2 || next != "" {
		t.Fatalf("last page: %d rows, next %q, %v", len(page), next, err)
	}
	if first := f.calls[0].args; first[2].(*time.Time) != nil || first[3].(*string) != nil {
		t.Fatalf("the first page names a position: %v %v", first[2], first[3])
	}
	// an exactly full last page does not point at an empty one
	exact := &fakeLister{entries: all[:3]}
	if _, next, _ := List(context.Background(), exact, userA, "", "", 3); next != "" {
		t.Fatalf("a page that is exactly the rest names the cursor %q", next)
	}
	// the page size: below one is the default, above the maximum is the maximum
	for in, want := range map[int]int{-5: DefaultListLimit, 0: DefaultListLimit, 1: 1, MaxListLimit: MaxListLimit, MaxListLimit + 1: MaxListLimit, 100000: MaxListLimit} {
		g := &fakeLister{}
		if _, _, err := List(context.Background(), g, userA, "", "", in); err != nil {
			t.Fatal(err)
		}
		if got := g.calls[0].args[len(g.calls[0].args)-1]; got != want+1 {
			t.Errorf("limit %d asked the database for %v rows, want %d", in, got, want+1)
		}
	}
	// the database failing is reported and holds no address
	bad := &fakeLister{err: pgErr("XX000", "c", "boom", "Key (email)=(canary@example.com)")}
	if _, _, err := List(context.Background(), bad, userA, "", "", 3); err == nil || strings.Contains(err.Error(), "canary@") {
		t.Fatalf("a failing database: %v", err)
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestCursor_IsStrictlyWhatTheListWritesAndNothingElse(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 999999000, time.UTC)
	good := encodeCursor(at, "a|b@example.com") // an address may hold the separator
	if got, email, ok := decodeCursor(good); !ok || !got.Equal(at) || email != "a|b@example.com" {
		t.Fatalf("control: the cursor does not round-trip: %v %q %v", got, email, ok)
	}
	enc := func(s string) string { return b64(s) }
	for name, c := range map[string]string{
		"empty text":             "",
		"not base64":             "***",
		"another version":        enc("2|5|a@example.com"),
		"no address":             enc("1|5|"),
		"no separator":           enc("1-5-a@example.com"),
		"a number with a sign":   enc("1|+5|a@example.com"),
		"a number with zeros":    enc("1|005|a@example.com"),
		"a number that is text":  enc("1|five|a@example.com"),
		"a NUL in the address":   enc("1|5|a\x00@example.com"),
		"an address not UTF-8":   enc("1|5|a\xff@example.com"),
		"an address over 254":    enc("1|5|" + strings.Repeat("a", 255)),
		"over 512 bytes of text": strings.Repeat("A", 513),
		"padding":                good + "=",
	} {
		if _, _, ok := decodeCursor(c); ok {
			t.Errorf("%s: a forged cursor was accepted", name)
		}
	}
}
