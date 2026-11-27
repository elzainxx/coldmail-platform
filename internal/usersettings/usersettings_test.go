package usersettings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The store's pure parts, with no database (the table, its trigger and its grants are proved against a real PostgreSQL by tests/harness/usersettings_test.go). These tests hold what is decided in Go:
// who counts as a user, that a bad user never reaches the database, that the read is scoped by the id it is given, and how the database's refusals are named.

const (
	userA = "a1a1a1a1-0000-4000-8000-000000000001"
	userB = "b2b2b2b2-0000-4000-8000-000000000002"
)

// fakeRow answers a Scan with the value, or with the error.
type fakeRow struct {
	val bool
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.val
	return nil
}

// fakeQ records every statement it is asked and the arguments, and answers from a table keyed by user.
type fakeQ struct {
	calls [][]any
	rows  map[string]fakeRow
	err   error
}

func (q *fakeQ) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.calls = append(q.calls, append([]any{sql}, args...))
	if q.err != nil {
		return fakeRow{err: q.err}
	}
	if r, ok := q.rows[args[0].(string)]; ok {
		return r
	}
	return fakeRow{err: pgx.ErrNoRows}
}

func TestValidUser_OnlyTheCanonicalUUIDOfACustomerIsOne(t *testing.T) {
	good := []string{userA, userB, "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90"}
	bad := map[string]string{
		"empty":             "",
		"upper case":        strings.ToUpper(userA),
		"no hyphens":        strings.ReplaceAll(userA, "-", ""),
		"too long":          userA + "0",
		"a letter past f":   "g1a1a1a1-0000-4000-8000-000000000001",
		"the system user":   systemUser,
		"hyphen misplaced":  "a1a1a1a10-000-4000-8000-000000000001",
		"a space":           " " + userA[1:],
		"an address":        "someone@example.test",
		"sql in the second": userA[:30] + "';--",
	}
	for _, id := range good {
		if !validUser(id) {
			t.Errorf("%q is a customer's id and was refused", id)
		}
	}
	for name, id := range bad {
		if validUser(id) {
			t.Errorf("%s: %q was accepted as a customer's id", name, id)
		}
	}
}

func TestNew_NoPoolIsRefusedAndAStoreWithNoneNeverAnswers(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrNoDB) {
		t.Fatalf("New(nil) = %v, want ErrNoDB", err)
	}
	var s *Store
	if _, err := s.Get(context.Background(), userA); !errors.Is(err, ErrNoDB) {
		t.Errorf("Get on a nil store = %v, want ErrNoDB", err)
	}
	if _, err := s.Set(context.Background(), userA, true); !errors.Is(err, ErrNoDB) {
		t.Errorf("Set on a nil store = %v, want ErrNoDB", err)
	}
	if _, err := (&Store{}).Set(context.Background(), userA, true); !errors.Is(err, ErrNoDB) {
		t.Errorf("Set on a store with no pool = %v, want ErrNoDB", err)
	}
}

func TestSetAndGet_ABadUserIsRefusedBeforeTheDatabaseIsAsked(t *testing.T) {
	// a store whose pool is not usable: any call that reached it would panic or fail with something else than ErrBadUser
	s := &Store{pool: new(pgxpool.Pool)}
	for _, id := range []string{"", "not-a-uuid", systemUser, strings.ToUpper(userA)} {
		if _, err := s.Get(context.Background(), id); !errors.Is(err, ErrBadUser) {
			t.Errorf("Get(%q) = %v, want ErrBadUser", id, err)
		}
		if _, err := s.Set(context.Background(), id, true); !errors.Is(err, ErrBadUser) {
			t.Errorf("Set(%q) = %v, want ErrBadUser", id, err)
		}
	}
}

func TestVendorOptOutOf_ReadsOnlyTheRowOfTheUserItIsGivenAndNoRowIsOptedIn(t *testing.T) {
	q := &fakeQ{rows: map[string]fakeRow{userA: {val: true}, userB: {val: false}}}
	got, err := VendorOptOutOf(context.Background(), q, userA)
	if err != nil || !got {
		t.Fatalf("user A (a row that says true) read as %v, %v", got, err)
	}
	if len(q.calls) != 1 || q.calls[0][1] != userA || len(q.calls[0]) != 2 {
		t.Fatalf("the read was %v: it must be one statement with the user's id as its only argument", q.calls)
	}
	if !strings.Contains(q.calls[0][0].(string), "WHERE user_id = $1") {
		t.Errorf("the statement is not scoped by the user: %s", q.calls[0][0])
	}
	// the control: another user's row is another answer (the value is the row's, not a constant), and a user with no row is opted in
	if got, err := VendorOptOutOf(context.Background(), q, userB); err != nil || got {
		t.Errorf("user B (a row that says false) read as %v, %v", got, err)
	}
	if got, err := VendorOptOutOf(context.Background(), q, "c3c3c3c3-0000-4000-8000-000000000003"); err != nil || got {
		t.Errorf("a user with no row read as %v, %v, want opted in (false) and no error", got, err)
	}
}

func TestVendorOptOutOf_ABadUserNeverReachesTheDatabaseAndAnErrorCarriesNoDetail(t *testing.T) {
	q := &fakeQ{rows: map[string]fakeRow{}}
	if _, err := VendorOptOutOf(context.Background(), q, "x"); !errors.Is(err, ErrBadUser) || len(q.calls) != 0 {
		t.Fatalf("a bad user: %v after %d call(s), want ErrBadUser and none", err, len(q.calls))
	}
	if _, err := VendorOptOutOf(context.Background(), nil, userA); !errors.Is(err, ErrNoDB) {
		t.Errorf("no connection: %v, want ErrNoDB", err)
	}
	// a database error is reported by its code and its message, never by the Detail line (which quotes the row)
	q.err = &pgconn.PgError{Code: "57014", Message: "canceling statement", Detail: "Key (user_id)=(" + userA + ") secret detail"}
	_, err := VendorOptOutOf(context.Background(), q, userA)
	if err == nil || strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), userA) || !strings.Contains(err.Error(), "57014") {
		t.Errorf("a database error read as %v, want its code and message and nothing of the row", err)
	}
	// a cancelled context stays a cancelled context
	q.err = context.Canceled
	if _, err := VendorOptOutOf(context.Background(), q, userA); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read is %v, want it to unwrap to context.Canceled", err)
	}
}

func TestMapDBError_ByCodeAndConstraintNameNeverByText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"the foreign key to the user", &pgconn.PgError{Code: "23503", ConstraintName: conUserFK}, ErrUnknownUser},
		{"the system user", &pgconn.PgError{Code: "23514", ConstraintName: conSystemUser}, ErrBadUser},
		{"wrapped", errors.Join(errors.New("x"), &pgconn.PgError{Code: "23503", ConstraintName: conUserFK}), ErrUnknownUser},
	}
	for _, c := range cases {
		if got := mapDBError(c.err); !errors.Is(got, c.want) {
			t.Errorf("%s: mapped to %v, want %v", c.name, got, c.want)
		}
	}
	// the controls: the right code under another constraint, the right constraint under another code, a text that says the name and a plain error map to nothing
	for name, err := range map[string]error{
		"another constraint": &pgconn.PgError{Code: "23503", ConstraintName: "some_other_fk"},
		"another code":       &pgconn.PgError{Code: "23505", ConstraintName: conUserFK},
		"the name in text":   &pgconn.PgError{Code: "23503", Message: conUserFK},
		"not a pg error":     errors.New(conUserFK),
	} {
		if got := mapDBError(err); got != nil {
			t.Errorf("%s: mapped to %v, want nothing", name, got)
		}
	}
}

func TestDefaults_AUserWhoNeverStatedASettingIsOptedIn(t *testing.T) {
	if Defaults().VendorFallbackOptOut {
		t.Fatal("the defaults are opted OUT: a customer would lose the vendor's answers without having said so (owner ruling: opted in by default)")
	}
}
