package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetUnitReachable (reachable.go): what is decided BEFORE the database is asked, and how the database's own refusals of platform.set_unit_reachable come back. What the function does against a real
// Postgres (a flip that waits for a reserve, changed true or false, the history) is proved by tests/harness (TestMoneyReachable_*).

// deadStore is a store over a pool that connects lazily to a port nothing listens on: a request that reaches the database fails on the connection, and one that is refused before it does not.
func deadStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("a lazy pool must be creatable without a database: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

func TestSetUnitReachable_RefusesWhatIsMalformedBeforeTheDatabaseIsAsked(t *testing.T) {
	st := deadStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Control first: a LEGAL request reaches the pool and fails on the connection, so the pool really is dead and the refusals below were decided before it.
	changed, err := st.SetUnitReachable(ctx, "m1_vendor", true, "config: a legal source")
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnknownUnit) || changed {
		t.Fatalf("control: a legal request on a dead pool returned changed=%v, %v; want a connection error that is not a validation error", changed, err)
	}
	if !strings.HasPrefix(err.Error(), "money.set_unit_reachable: ") {
		t.Errorf("the error does not name the operation: %v", err)
	}

	for _, c := range []struct {
		name         string
		unit, source string
		field        string
	}{
		{"a blank unit", "", "config: x", "unit"},
		{"a unit of spaces", "   ", "config: x", "unit"},
		{"a unit of 65 bytes", strings.Repeat("u", 65), "config: x", "unit"},
		{"a unit that is not UTF-8", "m1_\xffvendor", "config: x", "unit"},
		{"a unit with a NUL", "m1\x00vendor", "config: x", "unit"},
		{"a blank source", "m1_vendor", "", "source"},
		{"a source of spaces and a tab", "m1_vendor", "  \t ", "source"},
		{"a source of 201 bytes", "m1_vendor", strings.Repeat("s", 201), "source"},
		{"a source that is not UTF-8", "m1_vendor", "bad \xff byte", "source"},
		{"a source with a NUL", "m1_vendor", "a\x00b", "source"},
	} {
		changed, err := st.SetUnitReachable(ctx, c.unit, true, c.source)
		var ie *InvalidError
		if changed || !errors.Is(err, ErrInvalid) || !errors.As(err, &ie) || ie.Field != c.field {
			t.Errorf("%s: got changed=%v, %v; want an InvalidError on field %q", c.name, changed, err, c.field)
		}
	}

	// the legal edges are not refused (they get as far as the connection): a unit of 64 bytes, a source of 200 bytes, a source with white space around it
	for _, c := range []struct{ name, unit, source string }{
		{"a unit of exactly 64 bytes", strings.Repeat("u", 64), "config: x"},
		{"a source of exactly 200 bytes", "m1_vendor", strings.Repeat("s", 200)},
		{"a source padded with white space", "m1_vendor", "  config: x \n"},
	} {
		if _, err := st.SetUnitReachable(ctx, c.unit, false, c.source); err == nil || errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want a connection error (the edge is legal)", c.name, err)
		}
	}

	// what is stored is the TRIMMED source; a blank one is never "fixed" into something else
	if got, err := validateReachable("m1_vendor", "  config: PLATFORM_M1_VENDOR_REACHABLE \t"); err != nil || got != "config: PLATFORM_M1_VENDOR_REACHABLE" {
		t.Errorf("validateReachable trimmed to %q, %v", got, err)
	}
}

func TestSetUnitReachable_TheDatabasesRefusalsComeBackTyped(t *testing.T) {
	st := &Store{dsn: "postgres://platform_app:Sup3rS3cretPw@db.internal:5432/platform"}
	pg := func(code, msg string) error {
		return fmt.Errorf("while scanning: %w", &pgconn.PgError{Code: code, Message: msg})
	}

	// P0002: no such unit. By SQLSTATE: the text of the message is never read.
	err := st.failReachable(pg("P0002", "wording that may change"), "m9_nothing")
	if !errors.Is(err, ErrUnknownUnit) || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"m9_nothing"`) {
		t.Errorf("P0002 mapped to %v, want ErrUnknownUnit naming the unit and not ErrInvalid", err)
	}
	// 22023: the function's own "a unit is reachable or not, and the change says who said so"
	var ie *InvalidError
	if err := st.failReachable(pg("22023", "x"), "m1_vendor"); !errors.Is(err, ErrInvalid) || !errors.As(err, &ie) || ie.Field != "source" || errors.Is(err, ErrUnknownUnit) {
		t.Errorf("22023 mapped to %v, want an InvalidError on source", err)
	}
	// a lock that ran out is the store's ErrLockTimeout, as for every other operation
	if err := st.failReachable(pg("55P03", "canceling statement due to lock timeout"), "m1_vendor"); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("55P03 mapped to %v, want ErrLockTimeout", err)
	}

	// the controls: the words of the two refusals in a message with ANOTHER code, other codes, and a plain error are not typed as either
	for name, raw := range map[string]error{
		"the words of P0002 under another code":  pg("23514", "platform.set_unit_reachable: no unit m9_nothing"),
		"the words of 22023 under another code":  pg("23514", "a unit is reachable or not, and the change says who said so"),
		"another database error":                 pg("40001", "could not serialize access"),
		"a plain error that mentions the codes":  errors.New("P0002 22023 no unit"),
		"a driver error that carries the secret": errors.New("failed to connect to `postgres://platform_app:Sup3rS3cretPw@db.internal:5432/platform`: refused"),
	} {
		got := st.failReachable(raw, "m1_vendor")
		if got == nil || errors.Is(got, ErrUnknownUnit) || errors.Is(got, ErrInvalid) || errors.Is(got, ErrLockTimeout) {
			t.Errorf("%s: mapped to %v, want it passed through untyped", name, got)
		}
		if strings.Contains(got.Error(), "Sup3rS3cretPw") {
			t.Errorf("%s: the connection string's password reached the error: %v", name, got)
		}
		if !errors.Is(got, raw) {
			t.Errorf("%s: the original error is no longer reachable with errors.Is", name)
		}
	}

	// a hostile unit name is clipped in the message, not echoed whole
	huge := strings.Repeat("x", 5000)
	if got := st.failReachable(pg("P0002", "x"), huge); len(got.Error()) > 300 {
		t.Errorf("a %d-byte unit made an error of %d bytes", len(huge), len(got.Error()))
	}
}

// reachTx is a pgx.Tx that records what it is asked to run and answers the one query of setUnitReachable: changed, or rowErr. Every other method of the embedded interface is nil and panics, which is the
// test's way of saying "the operation went further than the test expected".
type reachTx struct {
	pgx.Tx
	stmts   []stmtRecord
	changed bool
	rowErr  error
}

func (f *reachTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.stmts = append(f.stmts, stmtRecord{sql, args})
	return pgconn.CommandTag{}, nil
}

func (f *reachTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.stmts = append(f.stmts, stmtRecord{sql, args})
	return reachRow{f.changed, f.rowErr}
}

type reachRow struct {
	changed bool
	err     error
}

func (r reachRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("reachRow: %d destinations for one value", len(dest))
	}
	*dest[0].(*bool) = r.changed
	return nil
}

// The transaction of SetUnitReachable runs exactly two statements, in this order: the bound (SET LOCAL lock_timeout, the store's wait as a bind parameter) and the writer, with the unit, the flag and the
// trimmed source as parameters and nothing in the text but the function: no other write, no read of the table. The answer of the writer is what the call returns, and each refusal is typed through THIS path.
func TestSetUnitReachable_RunsTheBoundThenOnlyTheWriterAndTypesItsAnswer(t *testing.T) {
	s := New(nil, WithLockTimeout(300*time.Millisecond))
	for _, answer := range []bool{true, false} {
		tx := &reachTx{changed: answer}
		clean, err := validateReachable("m1_vendor", "  config: x ")
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.setUnitReachable(context.Background(), tx, "m1_vendor", true, clean)
		if err != nil || got != answer {
			t.Fatalf("the writer said %v and the call returned %v, %v", answer, got, err)
		}
		sqls := tx.sqls()
		if len(sqls) != 2 || sqls[0] != setConfigSQL || sqls[1] != "SELECT platform.set_unit_reachable($1::text, $2::boolean, $3::text)" {
			t.Fatalf("the transaction ran %q, want the bound and then the writer and nothing else", sqls)
		}
		if a := tx.stmts[0].args; len(a) != 1 || a[0] != "300ms" {
			t.Errorf("the bound carried %v, want the store's wait 300ms as its one parameter", a)
		}
		if a := tx.stmts[1].args; len(a) != 3 || a[0] != "m1_vendor" || a[1] != true || a[2] != "config: x" {
			t.Errorf("the writer was given %v, want the unit, the flag and the TRIMMED source as parameters", a)
		}
	}

	// the same path types the database's refusals, and says false when it failed
	for name, c := range map[string]struct {
		code string
		is   error
	}{"no such unit": {"P0002", ErrUnknownUnit}, "a malformed change": {"22023", ErrInvalid}, "a lock that ran out": {"55P03", ErrLockTimeout}} {
		tx := &reachTx{changed: true, rowErr: &pgconn.PgError{Code: c.code}}
		got, err := s.setUnitReachable(context.Background(), tx, "m1_vendor", true, "config: x")
		if got || !errors.Is(err, c.is) {
			t.Errorf("%s: returned %v, %v; want false and errors.Is(%v)", name, got, err, c.is)
		}
	}
	// the control: the same fake with no error and the OTHER answer returns it (the fake does not always say true or always fail)
	if got, err := s.setUnitReachable(context.Background(), &reachTx{changed: false}, "m1_vendor", true, "config: x"); err != nil || got {
		t.Errorf("control: returned %v, %v; want false, nil", got, err)
	}
}

// sqls is what the transaction was asked to run, each statement on one line.
func (f *reachTx) sqls() []string {
	out := make([]string, len(f.stmts))
	for i, s := range f.stmts {
		out[i] = strings.Join(strings.Fields(s.sql), " ")
	}
	return out
}
