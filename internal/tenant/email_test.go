package tenant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Resolver.Email (Phase 6, GET /v1/me): the address of the platform user, read from the sanctioned view by the user's id. The fakes are the file's own (tenant_test.go): a recording DB and a row that
// scans what it is given.

func emailRow(email string, active bool) pgx.Row {
	return rowFunc(func(dest ...any) error {
		if len(dest) != 2 {
			return fmt.Errorf("the read scans %d columns, the query returns 2", len(dest))
		}
		*(dest[0].(*string)), *(dest[1].(*bool)) = email, active
		return nil
	})
}

func TestEmail_ReadsTheAddressOfTheUserFromTheSanctionedViewAndNothingElse(t *testing.T) {
	db := &fakeDB{answer: func(int, string, []any) pgx.Row { return emailRow("ann@example.test", true) }}
	got, err := resolverOver(t, db, "").Email(context.Background(), userA)
	if err != nil || got != "ann@example.test" {
		t.Fatalf("Email = %q, %v; want the address", got, err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("the database was asked %d times, want once", len(db.calls))
	}
	q := strings.ToLower(db.calls[0].sql)
	if !strings.Contains(q, "platform.listable_users") || strings.Contains(q, "public.users") || strings.Contains(q, "insert") || strings.Contains(q, "update") || strings.Contains(q, "delete") {
		t.Fatalf("the query is %q: it must read platform.listable_users, the one sanctioned list of users, and write nothing", db.calls[0].sql)
	}
	if len(db.calls[0].args) != 1 || db.calls[0].args[0] != userA {
		t.Fatalf("the query's arguments are %v, want the user's id alone", db.calls[0].args)
	}
	if !strings.Contains(q, "where id = $1") {
		t.Fatalf("the query is not scoped by the user's id: %q", db.calls[0].sql)
	}
}

func TestEmail_RefusesWhatTheResolverRefusesAndNeverReturnsAnAddressWithAnError(t *testing.T) {
	boom := errors.New("connection refused")
	for _, c := range []struct {
		name   string
		id     string
		answer func(int, string, []any) pgx.Row
		want   error // nil: any error that is none of the typed ones
		asked  int
	}{
		{"not a uuid", "not-a-uuid", nil, ErrBadID, 0},
		{"the upper-case spelling (the principal's id is canonical)", strings.ToUpper(userA), nil, ErrBadID, 0},
		{"the system user", SystemUserID, nil, ErrSystemUser, 0},
		{"no such user", userA, func(int, string, []any) pgx.Row { return errRow(pgx.ErrNoRows) }, ErrUnknownLogin, 1},
		{"a user switched off since the login", userA, func(int, string, []any) pgx.Row { return emailRow("ann@example.test", false) }, ErrInactive, 1},
		{"a database failure", userA, func(int, string, []any) pgx.Row { return errRow(boom) }, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{answer: c.answer}
			if c.answer == nil {
				db = forbiddenDB(t)
			}
			got, err := resolverOver(t, db, "").Email(context.Background(), c.id)
			if err == nil || got != "" {
				t.Fatalf("Email = %q, %v; want an error and no address", got, err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
			if c.want == nil && (errors.Is(err, ErrUnknownLogin) || errors.Is(err, ErrInactive) || errors.Is(err, ErrBadID) || errors.Is(err, ErrSystemUser)) {
				t.Fatalf("a database failure is a refusal of the login: %v", err)
			}
			if len(db.calls) != c.asked {
				t.Fatalf("the database was asked %d times, want %d", len(db.calls), c.asked)
			}
			if strings.Contains(err.Error(), "ann@example.test") {
				t.Fatalf("the error holds an address: %v", err)
			}
		})
	}
	// the twin of all of them: the same id with a good row is answered
	db := &fakeDB{answer: func(int, string, []any) pgx.Row { return emailRow("ann@example.test", true) }}
	if got, err := resolverOver(t, db, "").Email(context.Background(), userA); err != nil || got == "" {
		t.Fatalf("control failed: %q, %v", got, err)
	}
	// a resolver with no database says so, and a nil resolver does not panic
	if _, err := (&Resolver{}).Email(context.Background(), userA); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("a resolver with no database: %v", err)
	}
	var nilResolver *Resolver
	if _, err := nilResolver.Email(context.Background(), userA); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("a nil resolver: %v", err)
	}
}
