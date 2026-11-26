package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/money"
)

func pgErr(code, constraint string) error {
	return fmt.Errorf("wrapped by a driver layer: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "refused", Detail: "Key (user_id, email)=(secret@example.com) is the row"})
}

// Every refusal the store maps is mapped by SQLSTATE and CONSTRAINT NAME; each has a control that shares the SQLSTATE but not the name (or the reverse) and is NOT mapped.
func TestMapDBError(t *testing.T) {
	s := &Store{}
	notAllowedWith := func(reason string) func(error) string {
		return func(err error) string {
			var na *NotAllowedError
			if !errors.Is(err, ErrNotAllowed) || !errors.As(err, &na) || na.Reason != reason {
				return fmt.Sprintf("want ErrNotAllowed with reason %q", reason)
			}
			return ""
		}
	}
	is := func(want error) func(error) string {
		return func(err error) string {
			if !errors.Is(err, want) {
				return fmt.Sprintf("want errors.Is(%v)", want)
			}
			return ""
		}
	}
	for _, c := range []struct {
		name  string
		err   error
		check func(error) string
	}{
		{"the slot index", pgErr("23505", "jobs_one_active_per_user_module"), is(ErrSlotTaken)},
		{"the confirm key index", pgErr("23505", "jobs_confirm_key_key"), is(ErrConfirmKeyReused)},
		{"an illegal move", pgErr("23001", "job_transition_illegal"), notAllowedWith("job_transition_illegal")},
		{"a module that cannot cancel", pgErr("23001", "job_module_cannot_cancel"), notAllowedWith("job_module_cannot_cancel")},
		{"a module that cannot pause", pgErr("23001", "job_module_cannot_pause"), notAllowedWith("job_module_cannot_pause")},
		{"a resume to the wrong place", pgErr("23001", "job_resume_to_where_it_paused_from"), notAllowedWith("job_resume_to_where_it_paused_from")},
		{"a guard that names no constraint", pgErr("23001", ""), notAllowedWith("restricted")},
		{"an unknown user", pgErr("23503", "jobs_user_id_fkey"), is(ErrNotFound)},
		{"a job that does not exist (job_transition)", pgErr("P0002", ""), is(ErrNotFound)},
		{"a lock wait that ran out", pgErr("55P03", ""), is(money.ErrLockTimeout)},
	} {
		got := s.fail(c.err)
		if msg := c.check(got); msg != "" {
			t.Errorf("%s: got %v, %s", c.name, got, msg)
		}
		var pe *pgconn.PgError
		if errors.As(got, &pe) {
			t.Errorf("%s: the mapped error still carries the driver's error, whose Detail quotes the row", c.name)
		}
		if strings.Contains(got.Error(), "secret@example.com") {
			t.Errorf("%s: the error quotes the row: %v", c.name, got)
		}
	}
	// the controls: the same SQLSTATE under another constraint, and a constraint name under another SQLSTATE, are not these refusals
	for name, e := range map[string]error{
		"a unique violation of another index":     pgErr("23505", "jobs_pkey"),
		"the slot's name under a check violation": pgErr("23514", "jobs_one_active_per_user_module"),
		"a foreign key of another table":          pgErr("23503", "job_rows_job_fk"),
	} {
		got := s.fail(e)
		if errors.Is(got, ErrSlotTaken) || errors.Is(got, ErrConfirmKeyReused) || errors.Is(got, ErrNotFound) || errors.Is(got, ErrNotAllowed) {
			t.Errorf("%s was mapped to %v", name, got)
		}
		var pe *pgconn.PgError
		if !errors.As(got, &pe) {
			t.Errorf("%s: an unmapped database error must stay reachable for errors.As", name)
		}
	}
	// a cancelled request stays a cancelled request
	if got := s.fail(fmt.Errorf("x: %w", context.Canceled)); !errors.Is(got, context.Canceled) {
		t.Errorf("a cancelled context became %v", got)
	}
	if s.fail(nil) != nil {
		t.Error("fail(nil) is not nil")
	}
}

func TestFail_ScrubsTheConnectionString(t *testing.T) {
	s := &Store{dsn: "postgres://platform_app:pw-9f3k2x@db.internal:5432/platform"}
	got := s.fail(errors.New(`dial error (dial tcp): postgres://platform_app:pw-9f3k2x@db.internal:5432/platform refused`))
	if strings.Contains(got.Error(), "pw-9f3k2x") {
		t.Errorf("the password reached the error text: %v", got)
	}
	if s2 := (&Store{dsn: s.dsn}); s2.fail(errors.New("plain failure")).Error() != "plain failure" {
		t.Error("an error that does not hold the DSN was changed")
	}
}

func TestTypedErrors_MatchTheirSentinelsAndSayNoAddress(t *testing.T) {
	cases := []struct {
		err  error
		is   error
		text string
	}{
		{badRequest("row %d is bad", 3), ErrBadRequest, "row 3 is bad"},
		{&TooManyRowsError{Limit: 10}, ErrTooManyRows, "at most 10"},
		{notAllowed("job_transition_illegal"), ErrNotAllowed, "job_transition_illegal"},
		{&UnknownChoiceRequiredError{Unknown: 4}, ErrUnknownChoiceRequired, "4 unknown result(s)"},
		{&StalePreviewError{Reason: ReasonUnknownChoiceChanged}, ErrStalePreview, "unknown_choice_changed"},
	}
	for _, c := range cases {
		if !errors.Is(c.err, c.is) || errors.Is(c.err, ErrNotFound) || !strings.Contains(c.err.Error(), c.text) {
			t.Errorf("%T: errors.Is(%v) must hold (and ErrNotFound must not), text %q", c.err, c.is, c.err.Error())
		}
		wrapped := opErr("confirm", c.err)
		if !errors.Is(wrapped, c.is) || !strings.HasPrefix(wrapped.Error(), "jobs.confirm: ") {
			t.Errorf("%T wrapped by opErr: %v", c.err, wrapped)
		}
	}
	// ErrInsufficientCredits is money's, re-exported: errors.As reaches the shortfall through the wrapping
	var in ErrInsufficientCredits
	wrapped := opErr("confirm", fmt.Errorf("money.reserve: %w", money.ErrInsufficientCredits{Available: 3, Needed: 10, Shortfall: 7}))
	if !errors.As(wrapped, &in) || in.Shortfall != 7 || in.Needed != 10 || in.Available != 3 {
		t.Errorf("errors.As(ErrInsufficientCredits) = %+v from %v", in, wrapped)
	}
	// ErrInfeasible carries its causes
	var inf ErrInfeasible
	if !errors.As(opErr("confirm", ErrInfeasible{Causes: []string{"no_account", "over_quota"}}), &inf) || len(inf.Causes) != 2 || inf.Causes[0] != "no_account" {
		t.Errorf("errors.As(ErrInfeasible) = %+v", inf)
	}
	if opErr("x", nil) != nil {
		t.Error("opErr(nil) is not nil")
	}
}

func TestNew_RefusesWhatItCannotRunWith(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1") // lazy: nothing connects
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mny := money.New(pool)

	if _, err := New(nil, mny); err == nil {
		t.Error("New accepted a nil pool")
	}
	if _, err := New(pool, nil); err == nil {
		t.Error("New accepted a nil money store")
	}
	for name, opts := range map[string][]Option{
		"zero rows":        {WithMaxRows(0)},
		"negative rows":    {WithMaxRows(-5)},
		"too many rows":    {WithMaxRows(HardMaxRows + 1)},
		"zero delay":       {WithMaxStartDelay(0)},
		"negative delay":   {WithMaxStartDelay(-time.Second)},
		"a month and more": {WithMaxStartDelay(MaxStartDelayLimit + time.Second)},
	} {
		if _, err := New(pool, mny, opts...); err == nil {
			t.Errorf("New accepted %s", name)
		}
	}
	// the legal twins: the limits themselves, and the defaults
	for name, opts := range map[string][]Option{
		"one row":      {WithMaxRows(1)},
		"the max rows": {WithMaxRows(HardMaxRows)},
		"one second":   {WithMaxStartDelay(time.Second)},
		"the max":      {WithMaxStartDelay(MaxStartDelayLimit)},
		"nothing":      nil,
		"a nil clock, logger and checker are ignored": {WithClock(nil), WithLogger(nil), WithPreChecks(nil)},
	} {
		if _, err := New(pool, mny, opts...); err != nil {
			t.Errorf("New refused %s: %v", name, err)
		}
	}
	s, _ := New(pool, mny)
	if s.MaxRows() != DefaultMaxRows || s.MaxStartDelay() != DefaultMaxStartDelay || DefaultMaxRows != 100000 || DefaultMaxStartDelay != 30*time.Minute {
		t.Errorf("defaults: %d rows, %s delay; want 100000 rows, 30m0s", s.MaxRows(), s.MaxStartDelay())
	}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if s2, _ := New(pool, mny, WithClock(func() time.Time { return fixed })); !s2.now().Equal(fixed) {
		t.Error("WithClock did not set the clock")
	}
}

// A user id that is not a customer's canonical uuid is refused before the database is asked: with a pool nothing listens behind, a refusal that is quick and typed was decided in Go.
func TestEveryMethodRefusesABadUserAndABadJobBeforeTheDatabase(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, _ := New(pool, money.New(pool))
	ctx := context.Background()
	const goodUser = "0b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	const goodJob = "1b6f6d5e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	fp := strings.Repeat("a", 64)

	calls := map[string]func(user, job string) error{
		"Get":     func(u, j string) error { _, err := s.Get(ctx, u, j); return err },
		"List":    func(u, j string) error { _, _, err := s.List(ctx, u, ListOpts{}); return err },
		"Events":  func(u, j string) error { _, err := s.Events(ctx, u, j, 0, 0); return err },
		"Preview": func(u, j string) error { _, err := s.Preview(ctx, u, j); return err },
		"Confirm": func(u, j string) error {
			_, err := s.Confirm(ctx, u, ConfirmReq{JobID: j, Fingerprint: fp, Key: "k"})
			return err
		},
		"DiscardDraft":      func(u, j string) error { _, err := s.DiscardDraft(ctx, u, j); return err },
		"Reopen":            func(u, j string) error { _, err := s.Reopen(ctx, u, j); return err },
		"CancelBeforeStart": func(u, j string) error { _, err := s.CancelBeforeStart(ctx, u, j); return err },
		"PauseScheduled":    func(u, j string) error { _, err := s.PauseScheduled(ctx, u, j); return err },
		"ResumeScheduled":   func(u, j string) error { _, err := s.ResumeScheduled(ctx, u, j); return err },
		"CreateDraft": func(u, j string) error {
			_, err := s.CreateDraft(ctx, u, CreateReq{Module: ModuleVerify, Rows: []RowIn{{Email: "a@example.com"}}})
			return err
		},
	}
	for name, call := range calls {
		if err := call("not-a-uuid", goodJob); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s with a bad user id: %v, want ErrBadRequest decided before the database", name, err)
		}
		if err := call(systemUser, goodJob); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s with the system user: %v, want ErrBadRequest", name, err)
		}
		if name == "List" || name == "CreateDraft" {
			continue // they take no job id
		}
		if err := call(goodUser, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s with a bad job id: %v, want ErrNotFound (a job that cannot exist is a job that does not)", name, err)
		}
		// the control: a well-formed pair gets past Go and fails at the database (nothing listens), not as a refusal of the request
		if err := call(goodUser, goodJob); errors.Is(err, ErrBadRequest) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s with a well-formed user and job: %v, want a connection failure (the request is well-formed)", name, err)
		}
	}
}

// A log line says one word about a refusal and never the error's text: every refusal has its word, an error the store does not know is just "error", and a wrapped one is still itself.
func TestRefusalWord(t *testing.T) {
	for want, err := range map[string]error{
		"not_found": ErrNotFound, "bad_request": badRequest("x"), "too_many_rows": &TooManyRowsError{Limit: 1}, "nothing_to_do": ErrNothingToDo, "not_allowed": notAllowed("x"),
		"stale_preview": ErrStalePreview, "slot_taken": ErrSlotTaken, "confirm_key_reused": ErrConfirmKeyReused, "not_previewed": ErrNotPreviewed, "no_entitlement": ErrNoEntitlement,
		"suspended": ErrSuspended, "module_unavailable": ErrModuleUnavailable, "lock_timeout": &money.LockTimeoutError{Wait: time.Second},
		"insufficient_credits": money.ErrInsufficientCredits{Needed: 2}, "infeasible": ErrInfeasible{Causes: []string{"c"}},
		"unknown_choice_required": &UnknownChoiceRequiredError{Unknown: 1}, // the typed stale preview is the stale preview's word, with its reason apart
	} {
		if got := refusalWord(opErr("confirm", err)); got != want {
			t.Errorf("refusalWord(%v) = %q, want %q", err, got, want)
		}
	}
	// the control: nothing the store does not know is given a word of a refusal, and the word never carries the error's text
	if got := refusalWord(errors.New("connection reset by peer, user@example.test")); got != "error" {
		t.Errorf("an unknown error has the word %q, want \"error\"", got)
	}
}
