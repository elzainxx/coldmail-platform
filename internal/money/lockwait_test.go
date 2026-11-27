package money

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The wait of a money operation (Phase 2 close-out D8). These tests need no database: they prove the DEFAULT is bounded, how an option and an odd duration become the value the
// database receives, how the database's lock_not_available becomes ErrLockTimeout, and, through a fake transaction that records its statements, that every operation begins with
// the bound and then takes the lock. The wait against a real PostgreSQL (a held lock that really times out, nothing written, the holder untouched) is in tests/harness
// (closeout_money_test.go).

func TestLockTimeout_TheZeroValueAndTheDefaultAreBoundedAtTenSeconds(t *testing.T) {
	if DefaultLockTimeout != 10*time.Second {
		t.Fatalf("DefaultLockTimeout = %s, want 10s (the default of PLATFORM_MONEY_LOCK_TIMEOUT_MS)", DefaultLockTimeout)
	}
	for name, s := range map[string]*Store{
		"the zero value of Store": {},
		"New with no option":      New(nil),
		"a nil Store":             nil,
		"an explicit zero":        New(nil, WithLockTimeout(0)),
		"a negative wait":         New(nil, WithLockTimeout(-time.Second)),
		"a negative then a zero":  New(nil, WithLockTimeout(-1), WithLockTimeout(0)),
	} {
		if got := s.LockTimeout(); got != 10*time.Second {
			t.Errorf("%s: LockTimeout() = %s, want 10s: the wait must be bounded and never 0 or negative", name, got)
		}
		if name == "a nil Store" {
			continue // the milliseconds and the setting are only asked of a real store
		}
		if got := s.lockTimeoutMS(); got != 10000 {
			t.Errorf("%s: %d ms reach the database, want 10000: 0 would mean no timeout at all", name, got)
		}
		if got := s.lockTimeoutSetting(); got != "10000ms" {
			t.Errorf("%s: the setting is %q, want \"10000ms\"", name, got)
		}
	}
}

func TestWithLockTimeout_IsWhatTheDatabaseReceives(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{300 * time.Millisecond, "300ms"},
		{100 * time.Millisecond, "100ms"},
		{time.Second, "1000ms"},
		{10 * time.Minute, "600000ms"},
		{1500 * time.Microsecond, "2ms"}, // rounded UP: never shorter than asked
		{time.Nanosecond, "1ms"},         // never 0: PostgreSQL reads 0 as "wait for ever"
		{999 * time.Microsecond, "1ms"},
		{time.Millisecond, "1ms"},
		{time.Duration(math.MaxInt64), strconv.Itoa(math.MaxInt32) + "ms"}, // clamped to what lock_timeout can hold, never wrapped to a small or negative number
		{25 * 24 * time.Hour, strconv.Itoa(math.MaxInt32) + "ms"},          // 600 h = 2.16e9 ms, more than an int4 holds
	} {
		s := New(nil, WithLockTimeout(c.d))
		if got := s.lockTimeoutSetting(); got != c.want {
			t.Errorf("WithLockTimeout(%s): the setting is %q, want %q", c.d, got, c.want)
		}
		if ms := s.lockTimeoutMS(); ms < 1 || ms > math.MaxInt32 {
			t.Errorf("WithLockTimeout(%s): %d ms is outside 1..%d", c.d, ms, math.MaxInt32)
		}
		if got := s.LockTimeout(); got != c.d {
			t.Errorf("WithLockTimeout(%s): LockTimeout() = %s, want what was asked (the milliseconds are the rounding, not the accessor)", c.d, got)
		}
	}
	// the last valid option wins, and an invalid one after a valid one changes nothing
	if got := New(nil, WithLockTimeout(time.Second), WithLockTimeout(2*time.Second)).LockTimeout(); got != 2*time.Second {
		t.Errorf("two options: %s, want the later one, 2s", got)
	}
	if got := New(nil, WithLockTimeout(time.Second), WithLockTimeout(0)).LockTimeout(); got != time.Second {
		t.Errorf("a valid then an ignored option: %s, want 1s", got)
	}
}

func TestLockTimeoutError_MatchesTheSentinelAndNamesNoSQL(t *testing.T) {
	var err error = &LockTimeoutError{User: lockUserA, Module: "m2", Wait: 300 * time.Millisecond}
	wrapped := opErr("reserve", err)
	if !errors.Is(wrapped, ErrLockTimeout) {
		t.Fatalf("%v does not match ErrLockTimeout through the operation's wrap", wrapped)
	}
	var lt *LockTimeoutError
	if !errors.As(wrapped, &lt) || lt.User != lockUserA || lt.Module != "m2" || lt.Wait != 300*time.Millisecond {
		t.Fatalf("errors.As gave %+v", lt)
	}
	for _, e := range []error{wrapped, &LockTimeoutError{Wait: time.Second}} {
		msg := e.Error()
		for _, banned := range []string{"SELECT", "select", "pg_", "lock_timeout", "SQLSTATE", "55P03", "advisory", "INSERT", "platform."} {
			if strings.Contains(msg, banned) {
				t.Errorf("the message %q names SQL or internals (%q)", msg, banned)
			}
		}
	}
	msg := wrapped.Error()
	for _, want := range []string{"money.reserve", "300ms", lockUserA, "m2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message %q does not say %q", msg, want)
		}
	}
	// the control of the match: another sentinel is not a lock timeout, and the lock timeout is none of the others
	for _, other := range []error{ErrConflict, ErrJobState, ErrReserveExceeded, ErrInvalid, ErrIdempotencyMismatch} {
		if errors.Is(err, other) || errors.Is(other, ErrLockTimeout) {
			t.Errorf("ErrLockTimeout and %v must not match each other", other)
		}
	}
	// without a user (a row lock ran out, not the balance's lock) the message still says how long it waited and names no one
	if msg := (&LockTimeoutError{Wait: 2 * time.Second}).Error(); !strings.Contains(msg, "2s") || strings.Contains(msg, "user") || strings.Contains(msg, "module") {
		t.Errorf("the message without a user is %q, want the wait and no user or module", msg)
	}
}

func TestStoreFail_MapsLockNotAvailableByItsCodeToErrLockTimeout(t *testing.T) {
	s := New(nil, WithLockTimeout(300*time.Millisecond))
	for name, raw := range map[string]error{
		"the error as the driver returns it": pgErr("55P03", ""),
		"wrapped by the caller":              fmt.Errorf("batch: %w", pgErr("55P03", "")),
		"with a constraint name beside it":   pgErr("55P03", "balances_available_not_negative"), // the code decides, not a name that happens to be there
	} {
		got := s.fail(raw, pgCtx{})
		var lt *LockTimeoutError
		if !errors.Is(got, ErrLockTimeout) || !errors.As(got, &lt) {
			t.Errorf("%s: fail returned %v, want a *LockTimeoutError that matches ErrLockTimeout", name, got)
			continue
		}
		if lt.Wait != 300*time.Millisecond || lt.User != "" || lt.Module != "" {
			t.Errorf("%s: %+v, want the store's wait 300ms and no user (only lockUser knows whose credits)", name, lt)
		}
	}
	// the paired control: the neighbouring failures are NOT lock timeouts, whatever their text says. A deadlock is its own error and a text that mentions the timeout is not the code.
	for name, raw := range map[string]error{
		"a deadlock (40P01)":         pgErr("40P01", ""),
		"a serialization failure":    pgErr("40001", ""),
		"a check violation":          pgErr("23514", ""),
		"the words, another code":    &pgconn.PgError{Code: "57014", Message: "canceling statement due to lock timeout"},
		"a plain error that says it": errors.New("55P03 lock timeout"),
	} {
		if got := s.fail(raw, pgCtx{}); errors.Is(got, ErrLockTimeout) {
			t.Errorf("%s was taken for a lock timeout: %v", name, got)
		}
	}
}

// ---- the statements of an operation, against a fake transaction ----

// stmtRecord is one statement the fake transaction was asked to run.
type stmtRecord struct {
	sql  string
	args []any
}

// fakeTx is a pgx.Tx that records what it is asked to run and answers the job read; every other method of the embedded interface is nil and panics, which is the test's way of
// saying "the operation went further than the test expected". The advisory-lock statement fails with lockErr.
type fakeTx struct {
	pgx.Tx
	stmts   []stmtRecord
	lockErr error
	job     []any // what the job read returns: user, module, status, metered
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.stmts = append(f.stmts, stmtRecord{sql, args})
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		return pgconn.CommandTag{}, f.lockErr
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.stmts = append(f.stmts, stmtRecord{sql, args})
	return fakeRow{f.job}
}

type fakeRow struct{ vals []any }

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("fakeRow: %d destinations for %d values: the operation read more than the test expected", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *bool:
			*p = r.vals[i].(bool)
		default:
			return fmt.Errorf("fakeRow: cannot scan into %T", d)
		}
	}
	return nil
}

const setConfigSQL = "SELECT set_config('lock_timeout', $1, true)"

func (f *fakeTx) sqls() []string {
	out := make([]string, len(f.stmts))
	for i, s := range f.stmts {
		out[i] = strings.Join(strings.Fields(s.sql), " ")
	}
	return out
}

// Every operation's FIRST statement is the bound (SET LOCAL lock_timeout through set_config, the value a bind parameter, never inside the text), the advisory lock for exactly its
// (user, module) comes after what it must read to know them, and a lock that runs out is an ErrLockTimeout that names them. Nothing else is run (the fake would panic).
func TestEveryOperationBeginsWithTheBoundThenTakesTheLock(t *testing.T) {
	const wait = 300 * time.Millisecond
	jobID := "0b000000-0000-4000-8000-000000000001"
	job := func(status string) []any { return []any{lockUserA, "m2", status, true, false} } // user, module, status, metered, opted out of the vendor (platform_0020)
	entry := entryReq{kind: kindGrant, userID: lockUserA, module: "m2", amount: 5, adminID: lockUserB, reason: "r", key: "k"}

	ops := []struct {
		name  string
		job   []any
		reads int // statements between the bound and the lock: the job read
		run   func(s *Store, tx pgx.Tx) error
	}{
		{"reserve", job("draft"), 1, func(s *Store, tx pgx.Tx) error {
			_, err := s.reserve(context.Background(), tx, ReserveReq{JobID: jobID, Units: 1})
			return err
		}},
		{"settle", job("running"), 1, func(s *Store, tx pgx.Tx) error {
			_, err := s.settle(context.Background(), tx, SettleReq{JobID: jobID})
			return err
		}},
		{"release", job("succeeded"), 1, func(s *Store, tx pgx.Tx) error { _, err := s.release(context.Background(), tx, jobID); return err }},
		{"grant", nil, 0, func(s *Store, tx pgx.Tx) error { _, err := s.writeEntry(context.Background(), tx, entry); return err }},
		{"adjust", nil, 0, func(s *Store, tx pgx.Tx) error {
			e := entry
			e.kind = kindAdjustment
			_, err := s.writeEntry(context.Background(), tx, e)
			return err
		}},
	}
	for _, op := range ops {
		s := New(nil, WithLockTimeout(wait))
		tx := &fakeTx{lockErr: pgErr("55P03", ""), job: op.job}
		err := op.run(s, tx)
		got := tx.sqls()
		if len(got) != 2+op.reads {
			t.Errorf("%s: ran %d statements %q, want the bound, %d read(s) and the lock", op.name, len(got), got, op.reads)
			continue
		}
		if got[0] != setConfigSQL {
			t.Errorf("%s: the first statement is %q, want %q", op.name, got[0], setConfigSQL)
		}
		if a := tx.stmts[0].args; len(a) != 1 || a[0] != "300ms" {
			t.Errorf("%s: the bound's parameters are %v, want [300ms] as a bind parameter", op.name, a)
		}
		if strings.Contains(tx.stmts[0].sql, "300") {
			t.Errorf("%s: the value is inside the statement text %q: it must be a parameter", op.name, tx.stmts[0].sql)
		}
		last := tx.stmts[len(tx.stmts)-1]
		if !strings.Contains(last.sql, "pg_advisory_xact_lock") || len(last.args) != 1 || last.args[0] != lockKey(lockUserA, "m2") {
			t.Errorf("%s: the last statement is %q %v, want the advisory lock of (%s, m2)", op.name, last.sql, last.args, lockUserA)
		}
		var lt *LockTimeoutError
		if !errors.Is(err, ErrLockTimeout) || !errors.As(err, &lt) {
			t.Errorf("%s: err = %v, want ErrLockTimeout", op.name, err)
			continue
		}
		if lt.User != lockUserA || lt.Module != "m2" || lt.Wait != wait {
			t.Errorf("%s: the timeout says %+v, want user %s, module m2, wait %s", op.name, lt, lockUserA, wait)
		}
	}

	// Paired control: a lock that fails for ANOTHER reason (a deadlock) is not a lock timeout, and the same statements were still run in the same order.
	s := New(nil, WithLockTimeout(wait))
	tx := &fakeTx{lockErr: pgErr("40P01", "")}
	err := func() error { _, err := s.writeEntry(context.Background(), tx, entry); return err }()
	if err == nil || errors.Is(err, ErrLockTimeout) {
		t.Errorf("a deadlock at the lock was reported as %v, want an error that is not ErrLockTimeout", err)
	}
	if got := tx.sqls(); len(got) != 2 || got[0] != setConfigSQL {
		t.Errorf("the control ran %q, want the bound and the lock", got)
	}

	// And when the lock IS granted the grant goes on to its next statement (which the fake refuses to answer: the order of the first two statements is what is asserted).
	tx = &fakeTx{}
	if _, err := s.writeEntry(context.Background(), tx, entry); err == nil || errors.Is(err, ErrLockTimeout) {
		t.Errorf("a grant that got its lock returned %v, want the fake's refusal of the statement after it", err)
	}
	if got := tx.sqls(); len(got) < 3 || got[0] != setConfigSQL || !strings.Contains(got[1], "pg_advisory_xact_lock") {
		t.Errorf("a grant that gets its lock ran %q, want the bound first, the lock second and the write third", got)
	}
}

// A release needs a job that ENDED: succeeded, failed, cancelled or (platform_0011) expired. Every other status of the lifecycle - the eight that are not final, completing above all: the module has
// reported every item, the platform has not recorded them yet - is ErrJobNotEnded before the (user, module) lock is even asked for; each of the four that ended gets as far as the lock (the fake refuses
// it with lock_not_available, so the lock is what the release stopped at). The two halves are each other's control: the same read and the same fake, a different status.
func TestRelease_OnlyAnEndedJobGetsAsFarAsTheLock(t *testing.T) {
	const jobID = "0b000000-0000-4000-8000-000000000001"
	ended := map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "expired": true}
	statuses := []string{"draft", "previewed", "confirmed", "scheduled", "running", "paused", "cancelling", "completing", "succeeded", "failed", "cancelled", "expired"}
	s := New(nil, WithLockTimeout(300*time.Millisecond))
	atTheLock := 0
	for _, status := range statuses {
		tx := &fakeTx{lockErr: pgErr("55P03", ""), job: []any{lockUserA, "m2", status, true, false}}
		_, err := s.release(context.Background(), tx, jobID)
		got := tx.sqls()
		if ended[status] {
			atTheLock++
			if !errors.Is(err, ErrLockTimeout) || len(got) != 3 || !strings.Contains(got[len(got)-1], "pg_advisory_xact_lock") {
				t.Errorf("a %s job: err = %v after %q, want it to get as far as the lock (ErrLockTimeout after the bound, the read and the lock)", status, err, got)
			}
			continue
		}
		if !errors.Is(err, ErrJobNotEnded) || len(got) != 2 || !strings.Contains(err.Error(), status) {
			t.Errorf("a %s job: err = %v after %q, want ErrJobNotEnded naming the status, before any lock", status, err, got)
		}
	}
	if atTheLock != 4 {
		t.Fatalf("%d statuses got as far as the lock, want the 4 that ended (succeeded, failed, cancelled, expired)", atTheLock)
	}
}
