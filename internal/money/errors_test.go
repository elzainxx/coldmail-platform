package money

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func pgErr(code, constraint string) error {
	return &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "the server's wording, which the mapping must never read"}
}

func TestMapPgError_ByConstraintNameNotByText(t *testing.T) {
	for _, c := range []struct {
		code, constraint string
		check            func(error) bool
		what             string
	}{
		{"23514", "reserves_not_overspent", func(e error) bool { return errors.Is(e, ErrReserveExceeded) }, "ErrReserveExceeded"},
		{"23514", "admin_is_active", func(e error) bool { return errors.Is(e, ErrAdminDisabled) }, "ErrAdminDisabled"},
		{"23503", "ledger_acting_admin_id_fkey", func(e error) bool { return errors.Is(e, ErrAdminUnknown) }, "ErrAdminUnknown"},
		{"23503", "admin_audit_admin_id_fkey", func(e error) bool { return errors.Is(e, ErrAdminUnknown) }, "ErrAdminUnknown"},
		{"23503", "ledger_user_id_fkey", func(e error) bool { return errors.Is(e, ErrUserNotFound) }, "ErrUserNotFound"},
		{"23503", "balances_user_id_fkey", func(e error) bool { return errors.Is(e, ErrUserNotFound) }, "ErrUserNotFound"},
		{"23514", "ledger_not_the_system_user", func(e error) bool { return errors.Is(e, ErrInvalid) }, "ErrInvalid"},
		{"23505", "ledger_one_charge_per_item", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23505", "ledger_one_reserve_per_job", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23505", "ledger_one_release_per_reserve", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23505", "ledger_idempotency_key", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23514", "release_is_the_remainder", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23505", "job_unit_prices_pk", func(e error) bool { return errors.Is(e, ErrConflict) }, "ErrConflict"},
		{"23514", "reserve_at_the_dearest_price", func(e error) bool { return errors.Is(e, ErrSnapshot) }, "ErrSnapshot"},
		{"23514", "reserve_needs_the_complete_snapshot", func(e error) bool { return errors.Is(e, ErrSnapshot) }, "ErrSnapshot"},
	} {
		got := mapPgError(pgErr(c.code, c.constraint), pgCtx{})
		if got == nil || !c.check(got) {
			t.Errorf("constraint %s (SQLSTATE %s): mapped to %v, want %s", c.constraint, c.code, got, c.what)
		}
	}
}

func TestMapPgError_BalancesCheckIsInsufficientCreditsWithWhatWasRead(t *testing.T) {
	got := mapPgError(pgErr("23514", "balances_available_not_negative"), pgCtx{available: 7, needed: 10})
	var e ErrInsufficientCredits
	if !errors.As(got, &e) {
		t.Fatalf("mapped to %v, want ErrInsufficientCredits", got)
	}
	if e.Available != 7 || e.Needed != 10 || e.Shortfall != 3 || !e.Raced {
		t.Fatalf("got %+v, want available 7, needed 10, shortfall 3, raced", e)
	}
	// a read that said it fitted, refused all the same: the shortfall is never negative
	if e := mapPgError(pgErr("23514", "balances_available_not_negative"), pgCtx{available: 12, needed: 10}); e.(ErrInsufficientCredits).Shortfall != 0 {
		t.Fatalf("shortfall of a stale read = %d, want 0", e.(ErrInsufficientCredits).Shortfall)
	}
}

func TestMapPgError_UnknownAndNonDatabaseErrorsAreNotMapped(t *testing.T) {
	// the control of the table above: a constraint the store does not know, the same code with no constraint, and a plain error all stay unmapped
	for _, err := range []error{
		pgErr("23514", "some_other_check"),
		pgErr("23514", ""),
		&pgconn.PgError{Code: "23514", Message: "reserves_not_overspent appears in the TEXT only"},
		errors.New("reserves_not_overspent"),
		nil,
	} {
		if got := mapPgError(err, pgCtx{}); got != nil {
			t.Errorf("mapPgError(%v) = %v, want nil: only a *pgconn.PgError's constraint NAME maps", err, got)
		}
	}
	// a wrapped PgError is found through the chain
	if got := mapPgError(fmt.Errorf("batch: %w", pgErr("23514", "admin_is_active")), pgCtx{}); !errors.Is(got, ErrAdminDisabled) {
		t.Errorf("a wrapped PgError mapped to %v, want ErrAdminDisabled", got)
	}
}

func TestInvalidError_MatchesErrInvalidAndNamesTheField(t *testing.T) {
	err := fmt.Errorf("money.grant: %w", invalid("amount", "must be positive, got %d", -3))
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("an InvalidError must match ErrInvalid through a wrap")
	}
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Field != "amount" || !strings.Contains(ie.Reason, "-3") {
		t.Fatalf("errors.As gave %+v", ie)
	}
	if errors.Is(ErrNotMetered, ErrInvalid) {
		t.Fatal("ErrNotMetered must not match ErrInvalid: they are different refusals")
	}
}

func TestStoreFail_NeverCarriesTheConnectionStringOrPassword(t *testing.T) {
	const dsn = "postgres://platform_app:Sup3rS3cretPw@db.internal:5432/platform"
	s := &Store{dsn: dsn}
	for _, raw := range []error{
		errors.New("failed to connect to `" + dsn + "`: refused"),
		errors.New("could not parse: password=Sup3rS3cretPw in the string"),
	} {
		got := s.fail(raw, pgCtx{})
		if strings.Contains(got.Error(), "Sup3rS3cretPw") || strings.Contains(got.Error(), dsn) {
			t.Errorf("fail leaked the credential: %q", got)
		}
		if !errors.Is(got, raw) {
			t.Errorf("fail must keep the original error reachable with errors.Is")
		}
	}
	// the control: an error that does not mention the connection string is returned as it is (the instrument is not suppressing everything)
	plain := errors.New("context deadline exceeded")
	if got := s.fail(plain, pgCtx{}); got != plain {
		t.Errorf("an error without a credential was changed: %v", got)
	}
	if got := opErr("reserve", plain); !strings.HasPrefix(got.Error(), "money.reserve: ") || !errors.Is(got, plain) {
		t.Errorf("opErr = %q, want the money.reserve prefix and the cause reachable", got)
	}
	if opErr("reserve", nil) != nil {
		t.Error("opErr(nil) must stay nil")
	}
}
