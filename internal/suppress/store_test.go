package suppress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/norm"
)

// These tests need no database: they cover what the store decides BEFORE it asks one, and how it maps what the database answers. The fake connection records every statement, so "refused before
// the database" is proven by the fake having been called ZERO times, and every refusal has its paired control (the same call with a valid address reaches the fake). The facts that only a real
// PostgreSQL can show (the stored form, the CHECK's own refusals, a second user's rows) are in tests/harness/suppress_store_test.go.

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
	sysID = "00000000-0000-0000-0000-000000000000"
)

type execCall struct {
	sql  string
	args []any
}

// fakeExec is an Execer that records every call and answers with a canned tag or error.
type fakeExec struct {
	calls []execCall
	tag   string // the command tag to answer with; "INSERT 0 1" when empty
	err   error
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, execCall{sql: sql, args: args})
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	tag := f.tag
	if tag == "" {
		tag = "INSERT 0 1"
	}
	return pgconn.NewCommandTag(tag), nil
}

// fakeRow answers a QueryRow: it scans a []string (the one column Filter reads) or fails.
type fakeRow struct {
	found []string
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("fakeRow: %d destinations, want 1", len(dest))
	}
	p, ok := dest[0].(*[]string)
	if !ok {
		return fmt.Errorf("fakeRow: destination is %T, want *[]string", dest[0])
	}
	*p = append([]string(nil), r.found...)
	return nil
}

type queryCall struct {
	sql   string
	user  string
	asked []string
}

// fakeQuerier is a Querier whose table is a set of (user, address) pairs: it answers each query with the asked addresses that are in the user's set, like the real statement.
type fakeQuerier struct {
	table map[string]map[string]bool // user -> address -> suppressed
	calls []queryCall
	err   error
}

func (f *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	c := queryCall{sql: sql}
	if len(args) == 2 {
		c.user, _ = args[0].(string)
		c.asked, _ = args[1].([]string)
	}
	f.calls = append(f.calls, c)
	if f.err != nil {
		return fakeRow{err: f.err}
	}
	var found []string
	for _, e := range c.asked {
		if f.table[c.user][e] {
			found = append(found, e)
		}
	}
	return fakeRow{found: found}
}

func pgErr(code, constraint, message, detail string) *pgconn.PgError {
	return &pgconn.PgError{Code: code, ConstraintName: constraint, Message: message, Detail: detail}
}

func TestAdd_StoresTheNormalisedFormNotTheInput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"mixed case and surrounding space", "  Jane.Doe@EXAMPLE.com \t", "jane.doe@example.com"},
		{"an IDN domain becomes punycode", "user@exämple.com", "user@xn--exmple-cua.com"},
		{"a trailing dot of the domain goes", "a@example.com.", "a@example.com"},
		{"the +tag and the dots stay", "A.B+Tag@Example.COM", "a.b+tag@example.com"},
		{"CONTROL: an address already in the form is stored unchanged", "jane@example.com", "jane@example.com"},
	}
	for _, c := range cases {
		f := &fakeExec{}
		res, err := Add(context.Background(), f, userA, c.in, SourceManual)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(f.calls) != 1 {
			t.Errorf("%s: %d statements, want exactly one", c.name, len(f.calls))
			continue
		}
		args := f.calls[0].args
		if len(args) != 3 || args[0] != userA || args[1] != c.want || args[2] != SourceManual {
			t.Errorf("%s: the statement's arguments are %v, want [%s %s %s]", c.name, args, userA, c.want, SourceManual)
		}
		if res.Email != c.want || !res.Created {
			t.Errorf("%s: result %+v, want Email %q Created true", c.name, res, c.want)
		}
	}
}

func TestAdd_RefusesAnInvalidAddressBeforeTheDatabase(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name   string
		in     string
		reason norm.Reason
		syntax bool // the refusal is module 1's syntax layer (norm.ErrSyntax), not norm/v1 (norm.ErrMalformed)
	}{
		{"empty", "", norm.ReasonEmpty, false},
		{"only white space", " \t\n", norm.ReasonEmpty, false},
		{"no @", "jane.example.com", norm.ReasonNoAt, false},
		{"two @", "a@b@example.com", norm.ReasonMultipleAt, false},
		{"nothing before the @", "@example.com", norm.ReasonEmptyLocal, false},
		{"nothing after the @", "jane@", norm.ReasonEmptyDomain, false},
		{"a domain that is only dots", "jane@...", norm.ReasonEmptyDomain, false},
		{"a domain IDNA refuses", "jane@exa mple.com", norm.ReasonBadIDNA, false},
		{"an invalid punycode label (the vector domain-invalid-punycode-short)", "jane@xn--a.com", norm.ReasonBadIDNA, false},
		{"punycode of ascii garbage (the vector domain-punycode-of-ascii-garbage)", "jane@xn--zzzzzzzz.com", norm.ReasonBadIDNA, false},
		{"a 65-byte local part", long + "@example.com", norm.ReasonLocalTooLong, true},
		{"a domain with no dot", "jane@localhost", norm.ReasonDomainNoDot, true},
	}
	for _, c := range cases {
		f := &fakeExec{}
		res, err := Add(context.Background(), f, userA, c.in, SourceManual)
		if err == nil {
			t.Errorf("%s: accepted (%+v), want a refusal", c.name, res)
			continue
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: refused, but the database was asked %d time(s): the validator must decide before any SQL", c.name, len(f.calls))
		}
		var ia *InvalidAddressError
		switch {
		case !errors.Is(err, ErrInvalidAddress) || !errors.Is(err, ErrInvalid):
			t.Errorf("%s: %v does not match ErrInvalidAddress and ErrInvalid", c.name, err)
		case !errors.As(err, &ia) || ia.Reason != c.reason:
			t.Errorf("%s: %v, want an *InvalidAddressError with reason %q", c.name, err, c.reason)
		case !norm.Rejected(err):
			t.Errorf("%s: norm.Rejected(%v) is false, want the norm error reachable through the chain", c.name, err)
		case c.syntax && (!errors.Is(err, norm.ErrSyntax) || errors.Is(err, norm.ErrMalformed)):
			t.Errorf("%s: %v, want norm.ErrSyntax and not norm.ErrMalformed", c.name, err)
		case !c.syntax && (!errors.Is(err, norm.ErrMalformed) || errors.Is(err, norm.ErrSyntax)):
			t.Errorf("%s: %v, want norm.ErrMalformed and not norm.ErrSyntax", c.name, err)
		}
		if res != (Result{}) {
			t.Errorf("%s: a refusal returned the non-zero result %+v", c.name, res)
		}
		// the text names the reason and never the address (or any piece of it that is not also part of the reason)
		text := err.Error()
		if !strings.Contains(text, string(c.reason)) {
			t.Errorf("%s: the error text %q does not name the reason %q", c.name, text, c.reason)
		}
		for _, piece := range []string{strings.TrimSpace(c.in), "xn--a", "example.com", "localhost"} {
			if piece != "" && strings.Contains(text, piece) {
				t.Errorf("%s: the error text %q contains %q: an error must never carry the address", c.name, text, piece)
			}
		}
	}

	// CONTROL: the boundary one byte inside each limit, and a valid punycode label, reach the database
	ok := []string{strings.Repeat("a", 64) + "@example.com", "jane@example.co", "jane@xn--exmple-cua.com"}
	for _, in := range ok {
		f := &fakeExec{}
		if _, err := Add(context.Background(), f, userA, in, SourceManual); err != nil || len(f.calls) != 1 {
			t.Errorf("control %q: err %v, %d statement(s): a valid address must reach the database exactly once", in, err, len(f.calls))
		}
	}
}

func TestAdd_RefusesAUserOrASourceItCannotUseBeforeTheDatabase(t *testing.T) {
	type try struct {
		name         string
		user, source string
		want         error
	}
	bad := []try{
		{"user: empty", "", SourceManual, ErrBadUser},
		{"user: not a uuid", "not-a-uuid", SourceManual, ErrBadUser},
		{"user: a suffix", userA + "x", SourceManual, ErrBadUser},
		{"user: no hyphens", strings.ReplaceAll(userA, "-", ""), SourceManual, ErrBadUser},
		{"source: empty", userA, "", ErrBadSource},
		{"source: outside the vocabulary", userA, "bounce", ErrBadSource},
		{"source: upper case is another word", userA, "MANUAL", ErrBadSource},
		{"source: a suffix", userA, "manual ", ErrBadSource},
	}
	for _, c := range bad {
		f := &fakeExec{}
		_, err := Add(context.Background(), f, c.user, "jane@example.com", c.source)
		if !errors.Is(err, c.want) || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want %v (and ErrInvalid)", c.name, err, c.want)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: refused, but the database was asked %d time(s)", c.name, len(f.calls))
		}
	}
	// CONTROL: each of the three sources, and an upper-case uuid, reach the database
	for _, src := range []string{SourceUnsubscribe, SourceManual, SourceImport} {
		f := &fakeExec{}
		if _, err := Add(context.Background(), f, strings.ToUpper(userA), "jane@example.com", src); err != nil || len(f.calls) != 1 {
			t.Errorf("control source %q: err %v, %d statement(s), want none and one", src, err, len(f.calls))
		}
	}
}

func TestAdd_ADuplicateIsNotCreatedAndNotAnError(t *testing.T) {
	first, dup := &fakeExec{tag: "INSERT 0 1"}, &fakeExec{tag: "INSERT 0 0"}
	if res, err := Add(context.Background(), first, userA, "Jane@Example.com", SourceManual); err != nil || !res.Created || res.Email != "jane@example.com" {
		t.Errorf("the first add: %+v, %v, want Created true", res, err)
	}
	res, err := Add(context.Background(), dup, userA, "jane@example.com", SourceUnsubscribe)
	if err != nil || res.Created || res.Email != "jane@example.com" {
		t.Errorf("a duplicate: %+v, %v, want Created false, Email set and no error", res, err)
	}
}

func TestAdd_TheStatementIsOneParameterisedInsertThatNeverHoldsTheAddress(t *testing.T) {
	f := &fakeExec{}
	const addr = "Distinctive.Local.Part@Example.COM"
	if _, err := Add(context.Background(), f, userA, addr, SourceImport); err != nil || len(f.calls) != 1 {
		t.Fatalf("add: %v, %d statement(s)", err, len(f.calls))
	}
	sql := strings.Join(strings.Fields(f.calls[0].sql), " ")
	for _, want := range []string{"INSERT INTO platform.suppressions (user_id, email, source)", "VALUES ($1::uuid, $2, $3)", "ON CONFLICT DO NOTHING"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the statement %q lacks %q", sql, want)
		}
	}
	if strings.Contains(strings.ToLower(sql), "distinctive") || strings.Contains(sql, userA) {
		t.Errorf("the statement text %q holds a value: every value must be a parameter", sql)
	}
	// CONTROL: the parameters do carry the values (so the check above could have failed)
	if a := f.calls[0].args; a[0] != userA || a[1] != "distinctive.local.part@example.com" || a[2] != SourceImport {
		t.Errorf("the parameters are %v", a)
	}
}

func TestAdd_MapsEachDatabaseRefusalByConstraintName(t *testing.T) {
	const secretDetail = `Failing row contains (` + userA + `, jane@example.com, manual, 2026-01-01).`
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"23514 on the email CHECK is 'cannot be stored'", pgErr("23514", "suppressions_email_norm_v1", "new row violates check constraint", secretDetail), ErrCannotStore},
		{"22021 (a NUL never reaches the CHECK) is 'cannot be stored'", pgErr("22021", "", "invalid byte sequence for encoding UTF8: 0x00", ""), ErrCannotStore},
		{"23514 on the system user CHECK", pgErr("23514", "suppressions_not_the_system_user", "new row violates check constraint", secretDetail), ErrSystemUser},
		{"23514 on the source CHECK", pgErr("23514", "suppressions_source_vocabulary", "new row violates check constraint", secretDetail), ErrBadSource},
		{"23503 on the user foreign key", pgErr("23503", "suppressions_user_id_fkey", "violates foreign key constraint", `Key (user_id)=(`+userA+`) is not present in table "users".`), ErrUnknownUser},
		{"23505 on the primary key", pgErr("23505", "suppressions_pk", "duplicate key value violates unique constraint", secretDetail), ErrConflict},
		{"a wrapped driver error is judged too", fmt.Errorf("exec: %w", pgErr("23503", "suppressions_user_id_fkey", "violates foreign key constraint", "")), ErrUnknownUser},
	}
	for _, c := range cases {
		_, err := Add(context.Background(), &fakeExec{err: c.err}, userA, "jane@example.com", SourceManual)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want it to match %v", c.name, err, c.want)
			continue
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			t.Errorf("%s: the mapped error still wraps the driver's *PgError, whose Detail quotes the row", c.name)
		}
		if strings.Contains(err.Error(), "jane@example.com") || strings.Contains(err.Error(), "Failing row") {
			t.Errorf("%s: the error text %q carries the row", c.name, err.Error())
		}
		if !strings.HasPrefix(err.Error(), "suppress.add: ") {
			t.Errorf("%s: %q does not name the operation", c.name, err.Error())
		}
	}
	// the three the validator refuses first, and one that is mapped, are separate kinds: a refusal by the database is never an ErrInvalid
	_, err := Add(context.Background(), &fakeExec{err: pgErr("23514", "suppressions_email_norm_v1", "m", "")}, userA, "jane@example.com", SourceManual)
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrInvalidAddress) {
		t.Errorf("a CHECK refusal of a VALID address is %v: it must not read as an invalid request", err)
	}
	// CONTROL: no database error, no error
	if _, err := Add(context.Background(), &fakeExec{}, userA, "jane@example.com", SourceManual); err != nil {
		t.Errorf("control: %v", err)
	}
}

func TestAdd_AnUnknownDatabaseErrorKeepsNoDetailAndANonDatabaseErrorKeepsItsCause(t *testing.T) {
	// a constraint the store does not know: reported by SQLSTATE, constraint and message, with no Detail and without wrapping the driver's error
	unknown := pgErr("23514", "some_other_check", "violates check constraint some_other_check", "Failing row contains (jane@example.com)")
	_, err := Add(context.Background(), &fakeExec{err: unknown}, userA, "jane@example.com", SourceManual)
	var pe *pgconn.PgError
	switch {
	case err == nil:
		t.Fatal("an unknown database refusal was swallowed")
	case errors.As(err, &pe):
		t.Errorf("%v still wraps the driver's *PgError", err)
	case !strings.Contains(err.Error(), "SQLSTATE 23514") || !strings.Contains(err.Error(), "some_other_check"):
		t.Errorf("%q names neither the SQLSTATE nor the constraint", err.Error())
	case strings.Contains(err.Error(), "jane@example.com"):
		t.Errorf("%q carries the row", err.Error())
	}
	for _, kind := range []error{ErrCannotStore, ErrSystemUser, ErrUnknownUser, ErrBadSource, ErrConflict} {
		if errors.Is(err, kind) {
			t.Errorf("an UNKNOWN constraint was mapped to %v", kind)
		}
	}
	// a cancelled context keeps its cause
	_, err = Add(context.Background(), &fakeExec{err: context.Canceled}, userA, "jane@example.com", SourceManual)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%v: errors.Is(err, context.Canceled) is false", err)
	}
	// CONTROL: the same known constraint with a different SQLSTATE is not mapped either (the pair is the key)
	_, err = Add(context.Background(), &fakeExec{err: pgErr("23503", "suppressions_email_norm_v1", "m", "")}, userA, "jane@example.com", SourceManual)
	if errors.Is(err, ErrCannotStore) {
		t.Errorf("SQLSTATE 23503 with the CHECK's name was mapped to ErrCannotStore")
	}
}

func TestAdd_NoConnectionIsAnErrorNotAPanic(t *testing.T) {
	if _, err := Add(context.Background(), nil, userA, "jane@example.com", SourceManual); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("a nil connection: %v, want an error that is not an invalid request", err)
	}
	if _, err := Filter(context.Background(), nil, userA, []string{"a@example.com"}); err == nil {
		t.Errorf("Filter with a nil connection: want an error")
	}
	// CONTROL: an invalid address is still refused as invalid first (the order the doc states)
	if _, err := Add(context.Background(), nil, userA, "nope", SourceManual); !errors.Is(err, ErrInvalidAddress) {
		t.Errorf("an invalid address with no connection: %v, want ErrInvalidAddress", err)
	}
}

func TestErrors_Taxonomy(t *testing.T) {
	invalid := []error{ErrInvalidAddress, ErrBadUser, ErrBadSource}
	for _, e := range invalid {
		if !errors.Is(e, ErrInvalid) {
			t.Errorf("%v must match ErrInvalid", e)
		}
	}
	for _, e := range []error{ErrCannotStore, ErrSystemUser, ErrUnknownUser, ErrConflict} {
		if errors.Is(e, ErrInvalid) {
			t.Errorf("%v is a refusal by the database and must not match ErrInvalid", e)
		}
	}
	// the sentinels are distinct
	all := append(append([]error{}, invalid...), ErrCannotStore, ErrSystemUser, ErrUnknownUser, ErrConflict)
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v matches %v: the sentinels must be distinct", a, b)
			}
		}
	}
}

func TestFilter_ReturnsExactlyTheSuppressedOnes(t *testing.T) {
	q := &fakeQuerier{table: map[string]map[string]bool{
		userA: {"a@example.com": true, "c@example.com": true},
		userB: {"b@example.com": true, "d@example.com": true}, // another user's rows: never returned for userA
	}}
	got, err := Filter(context.Background(), q, userA, []string{"a@example.com", "b@example.com", "c@example.com", "e@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["a@example.com"] || !got["c@example.com"] || got["b@example.com"] || got["e@example.com"] {
		t.Errorf("Filter returned %v, want exactly a and c", got)
	}
	// CONTROL: the same question for the other user gives the other answer, so the fake (and the user argument) can tell them apart
	got, err = Filter(context.Background(), q, userB, []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"})
	if err != nil || len(got) != 2 || !got["b@example.com"] || !got["d@example.com"] {
		t.Errorf("Filter for the other user returned %v, %v, want exactly b and d", got, err)
	}
	// nothing suppressed is an empty, non-nil map; an empty list asks nothing
	none, err := Filter(context.Background(), q, userA, []string{"z@example.com"})
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("nothing suppressed: %v, %v, want an empty non-nil map", none, err)
	}
	before := len(q.calls)
	empty, err := Filter(context.Background(), q, userA, nil)
	if err != nil || empty == nil || len(empty) != 0 || len(q.calls) != before {
		t.Errorf("an empty list: %v, %v, %d query(ies), want an empty map and no query", empty, err, len(q.calls)-before)
	}
}

func TestFilter_AsksInBatchesNeverPerAddress(t *testing.T) {
	n := 2*FilterBatchSize + 1
	emails := make([]string, n)
	table := map[string]bool{}
	for i := range emails {
		emails[i] = fmt.Sprintf("u%d@example.com", i)
		if i%1000 == 0 {
			table[emails[i]] = true
		}
	}
	q := &fakeQuerier{table: map[string]map[string]bool{userA: table}}
	got, err := Filter(context.Background(), q, userA, emails)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 3 {
		t.Errorf("%d addresses asked in %d queries, want 3 (batches of %d)", n, len(q.calls), FilterBatchSize)
	}
	total := 0
	for i, c := range q.calls {
		total += len(c.asked)
		if len(c.asked) > FilterBatchSize {
			t.Errorf("query %d asked %d addresses, over the batch size %d", i, len(c.asked), FilterBatchSize)
		}
		sql := strings.Join(strings.Fields(c.sql), " ")
		if !strings.Contains(sql, "email = ANY($2::text[])") || !strings.Contains(sql, "user_id = $1::uuid") || !strings.Contains(sql, "FROM platform.suppressions") {
			t.Errorf("query %d is %q, want the user-scoped ANY form over platform.suppressions", i, sql)
		}
	}
	if total != n {
		t.Errorf("the queries asked %d addresses in all, want %d (none lost, none repeated)", total, n)
	}
	if len(got) != len(table) {
		t.Errorf("%d suppressed returned, want %d", len(got), len(table))
	}
	for e := range table {
		if !got[e] {
			t.Errorf("%s is suppressed and was not returned (a batch boundary lost it?)", e)
			break
		}
	}
	// CONTROL: exactly one batch is one query
	q1 := &fakeQuerier{table: map[string]map[string]bool{userA: table}}
	if _, err := Filter(context.Background(), q1, userA, emails[:FilterBatchSize]); err != nil || len(q1.calls) != 1 {
		t.Errorf("one full batch: %v, %d query(ies), want 1", err, len(q1.calls))
	}
}

func TestFilter_SkipsWhatTheTableCannotHoldAndAsksEachAddressOnce(t *testing.T) {
	q := &fakeQuerier{table: map[string]map[string]bool{userA: {"a@example.com": true}}}
	got, err := Filter(context.Background(), q, userA, []string{"a@example.com", "a\x00b@example.com", "bad\xffbyte@example.com", "a@example.com", "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 1 || len(q.calls[0].asked) != 1 || q.calls[0].asked[0] != "a@example.com" {
		t.Errorf("the queries were %+v, want one query asking only a@example.com once", q.calls)
	}
	if len(got) != 1 || !got["a@example.com"] {
		t.Errorf("Filter returned %v, want a@example.com", got)
	}
	// a list of only unaskable addresses asks nothing
	q2 := &fakeQuerier{}
	if got, err := Filter(context.Background(), q2, userA, []string{"a\x00b@example.com"}); err != nil || len(got) != 0 || len(q2.calls) != 0 {
		t.Errorf("only a NUL address: %v, %v, %d query(ies), want an empty map and none", got, err, len(q2.calls))
	}
}

func TestFilter_RefusesABadUserBeforeTheDatabaseAndReportsADatabaseError(t *testing.T) {
	q := &fakeQuerier{}
	for _, u := range []string{"", "nope", userA + "x"} {
		if _, err := Filter(context.Background(), q, u, []string{"a@example.com"}); !errors.Is(err, ErrBadUser) || !errors.Is(err, ErrInvalid) {
			t.Errorf("user %q: %v, want ErrBadUser", u, err)
		}
	}
	if len(q.calls) != 0 {
		t.Errorf("a bad user reached the database %d time(s)", len(q.calls))
	}
	// a database error stops the walk and is reported without the Detail; the cause of a non-database error is kept
	boom := &fakeQuerier{err: pgErr("42501", "", "permission denied for table suppressions", "Detail: jane@example.com")}
	_, err := Filter(context.Background(), boom, userA, []string{"a@example.com"})
	var pe *pgconn.PgError
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "SQLSTATE 42501") || strings.Contains(err.Error(), "jane@example.com") || !strings.HasPrefix(err.Error(), "suppress.filter: ") {
		t.Errorf("a database error: %v, want suppress.filter: ... SQLSTATE 42501 with no Detail and no driver error inside", err)
	}
	if _, err := Filter(context.Background(), &fakeQuerier{err: context.DeadlineExceeded}, userA, []string{"a@example.com"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a deadline: %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	// CONTROL: a good user and a working connection answer
	if _, err := Filter(context.Background(), q, userA, []string{"a@example.com"}); err != nil {
		t.Errorf("control: %v", err)
	}
}
