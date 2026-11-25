package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/money"
)

// The reachability sync (reachability.go; Phase 4, chunk B3). No database is needed: the setter is a fake, and the clock the loop waits on is injected, so the rhythm is asserted exactly and takes no
// time. What SetUnitReachable does against a real Postgres is proved by tests/harness (TestMoneyReachable_*).

type reachCall struct {
	unit        string
	reachable   bool
	source      string
	hadDeadline bool
}

// fakeReach is a reachabilitySetter that records its calls. fn decides each answer (n counts from 1); nil means "changed nothing, no error".
type fakeReach struct {
	mu    sync.Mutex
	calls []reachCall
	fn    func(ctx context.Context, n int) (bool, error)
}

func (f *fakeReach) SetUnitReachable(ctx context.Context, unit string, reachable bool, source string) (bool, error) {
	_, has := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, reachCall{unit, reachable, source, has})
	n := len(f.calls)
	f.mu.Unlock()
	if f.fn == nil {
		return false, nil
	}
	return f.fn(ctx, n)
}

func (f *fakeReach) got() []reachCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reachCall(nil), f.calls...)
}

// stepper is the injected clock: it records every wait the loop asks for and answers at once; at the limit-th wait it cancels the loop's context, which ends it.
type stepper struct {
	mu     sync.Mutex
	waits  []time.Duration
	limit  int
	cancel context.CancelFunc
}

func (s *stepper) wait(ctx context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	n := len(s.waits)
	s.mu.Unlock()
	if n >= s.limit {
		s.cancel()
		return false
	}
	return ctx.Err() == nil
}

// runSync runs syncReachability for exactly `turns` writes on the injected clock and returns the waits it asked for and its log.
func runSync(t *testing.T, set reachabilitySetter, reachable bool, turns int) ([]time.Duration, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &stepper{limit: turns, cancel: cancel}
	logs := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncReachability(ctx, set, reachable, slog.New(slog.NewJSONHandler(logs, nil)), st.wait)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("syncReachability did not stop at the end of the injected clock")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]time.Duration(nil), st.waits...), logs
}

func sameWaits(got []time.Duration, want ...time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

var (
	errRefused = errors.New("money.set_unit_reachable: failed to connect to `host=127.0.0.1 user=platform_app database=platform`: dial error (dial tcp 127.0.0.1:1: connect: connection refused)")
	errDown    = &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command (secretword)"}
)

const (
	s5  = 5 * time.Second
	s60 = 60 * time.Second
)

func TestSyncReachability_FiveSecondsUntilTheFirstSuccessThenSixty(t *testing.T) {
	// three failures, then it works for good: 5 s, 5 s, 5 s, then the minute
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n <= 3 {
			return false, errRefused
		}
		return false, nil
	}}
	waits, _ := runSync(t, f, true, 7)
	if !sameWaits(waits, s5, s5, s5, s60, s60, s60, s60) {
		t.Errorf("waits = %v, want three of 5s (failing), then 60s from the first success on", waits)
	}
	if n := len(f.got()); n != 7 {
		t.Errorf("%d writes in 7 turns", n)
	}

	// control: a database that is there from the start never makes the loop wait 5 s
	ok := &fakeReach{}
	if waits, _ := runSync(t, ok, true, 4); !sameWaits(waits, s60, s60, s60, s60) {
		t.Errorf("a healthy start waited %v, want 60s every turn", waits)
	}
	// control: a database that never comes up is retried at 5 s for ever (the loop does not slow down to the minute without a success)
	down := &fakeReach{fn: func(context.Context, int) (bool, error) { return false, errRefused }}
	if waits, _ := runSync(t, down, true, 4); !sameWaits(waits, s5, s5, s5, s5) {
		t.Errorf("a database that stays down waited %v, want 5s every turn", waits)
	}
	// a failure AFTER the first success waits for the next minute, not five seconds (the flag was set; this is only the re-assertion)
	later := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n == 2 || n == 3 {
			return false, errRefused
		}
		return false, nil
	}}
	if waits, _ := runSync(t, later, true, 4); !sameWaits(waits, s60, s60, s60, s60) {
		t.Errorf("failures after the first success waited %v, want 60s every turn", waits)
	}
}

func TestSyncReachability_WritesTheDialsValueForTheVendorUnitWithItsSource(t *testing.T) {
	for _, want := range []bool{true, false} {
		f := &fakeReach{}
		runSync(t, f, want, 3)
		calls := f.got()
		if len(calls) != 3 {
			t.Fatalf("value %v: %d writes in 3 turns", want, len(calls))
		}
		for i, c := range calls {
			if c.unit != "m1_vendor" || c.reachable != want || c.source != "config: PLATFORM_M1_VENDOR_REACHABLE" {
				t.Errorf("value %v, write %d: %+v, want m1_vendor, %v and the source naming the variable", want, i+1, c, want)
			}
			if !c.hadDeadline {
				t.Errorf("value %v, write %d had no deadline: a database that never answers would hold the loop for ever", want, i+1)
			}
		}
	}
	// the two runs differ only in the value, so the check above is not satisfied by a constant
	a, b := &fakeReach{}, &fakeReach{}
	runSync(t, a, true, 1)
	runSync(t, b, false, 1)
	if a.got()[0].reachable == b.got()[0].reachable {
		t.Error("control failed: the dial's two values produced the same write")
	}
	if reachabilitySource != "config: "+config.VarM1VendorReachable || reachabilityUnit != "m1_vendor" {
		t.Errorf("the unit and the source are %q and %q", reachabilityUnit, reachabilitySource)
	}
}

func TestSyncReachability_AFailureIsReportedOncePerClassAndNeverEndsTheLoop(t *testing.T) {
	// the sequence: the database is down (3 turns), the port is refused (2), it works (2), it goes down again (1)
	seq := []error{errDown, errDown, errDown, errRefused, errRefused, nil, nil, errDown}
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) { return false, seq[n-1] }}
	waits, logs := runSync(t, f, true, len(seq))
	if len(waits) != len(seq) || len(f.got()) != len(seq) {
		t.Fatalf("the loop made %d writes and %d waits for %d turns: a failure ended it", len(f.got()), len(waits), len(seq))
	}
	lines := logs.lines(t)
	var reasons []string
	for _, l := range lines {
		if l["msg"] == msgReachabilityFailed {
			reasons = append(reasons, fmt.Sprint(l["reason"]))
			if l["level"] != "WARN" {
				t.Errorf("a failure was logged at %v, want WARN", l["level"])
			}
		}
	}
	if want := []string{"database error 57P01", "connection refused", "database error 57P01"}; strings.Join(reasons, "|") != strings.Join(want, "|") {
		t.Errorf("the warnings carry the classes %v, want %v (once per class and per outage: 8 turns, 6 failures, 3 lines)", reasons, want)
	}
	if n := countLines(lines, msgReachabilityRecovered); n != 1 {
		t.Errorf("recovery was announced %d times, want once", n)
	}
	if n := countLines(lines, msgReachabilitySynced); n != 1 {
		t.Errorf("the first success was announced %d times, want once", n)
	}
	// the class is logged, the driver's text is not: not the host, the login or the message
	for _, leak := range []string{"secretword", "platform_app", "127.0.0.1", "terminating connection"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the log carries %q:\n%s", leak, logs.String())
		}
	}
}

func TestSyncReachability_StopsWithTheContextAndAShutdownIsNoFailure(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := &fakeReach{fn: func(ctx context.Context, _ int) (bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done() // a database that never answers: only the shutdown frees it
		return false, fmt.Errorf("money.set_unit_reachable: %w", ctx.Err())
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncReachability(ctx, f, true, slog.New(slog.NewJSONHandler(logs, nil)), sleepContext)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncReachability did not return after the cancel: a pass in flight held it")
	}
	if n := countLines(logs.lines(t), msgReachabilityFailed); n != 0 {
		t.Errorf("a shutdown in the middle of a pass was logged as a failure %d time(s):\n%s", n, logs.String())
	}
	if n := len(f.got()); n != 1 {
		t.Errorf("%d writes: the loop went on after the cancel", n)
	}

	// the control: the same error on a LIVE context is a failure and is logged
	live := &fakeReach{fn: func(context.Context, int) (bool, error) {
		return false, fmt.Errorf("money.set_unit_reachable: %w", context.DeadlineExceeded)
	}}
	_, logs2 := runSync(t, live, true, 1)
	if l := find(logs2.lines(t), msgReachabilityFailed); l == nil || l["reason"] != "timeout" {
		t.Errorf("a pass that timed out on a live context was logged as %v, want the warning with the class timeout", l)
	}

	// the real clock: it waits what it is asked, is cut short by the context, and is false for a context that is already done
	t0 := time.Now()
	if !sleepContext(context.Background(), 40*time.Millisecond) || time.Since(t0) < 40*time.Millisecond {
		t.Error("sleepContext returned early or false on a live context")
	}
	cctx, ccancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); ccancel() }()
	t0 = time.Now()
	if sleepContext(cctx, time.Hour) || time.Since(t0) > 5*time.Second {
		t.Error("sleepContext did not return false promptly when its context was cancelled")
	}
	if sleepContext(cctx, 0) {
		t.Error("sleepContext returned true for a context that is done")
	}
}

func TestSyncReachability_APanickingSetterNeverEndsTheLoop(t *testing.T) {
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) {
		if n <= 2 {
			panic("boom-secretvalue")
		}
		return false, nil
	}}
	waits, logs := runSync(t, f, true, 4)
	if !sameWaits(waits, s5, s5, s60, s60) || len(f.got()) != 4 {
		t.Fatalf("waits %v and %d writes: a panic ended or slowed the loop", waits, len(f.got()))
	}
	lines := logs.lines(t)
	if l := find(lines, msgReachabilityFailed); l == nil || !strings.Contains(fmt.Sprint(l["reason"]), "setter panicked (string)") || countLines(lines, msgReachabilityFailed) != 1 {
		t.Errorf("the panic was logged as %v (%d lines), want one warning naming the type of the panic value", l, countLines(lines, msgReachabilityFailed))
	}
	if strings.Contains(logs.String(), "boom-secretvalue") {
		t.Errorf("the VALUE of a panic reached the log:\n%s", logs.String())
	}
	if find(lines, msgReachabilitySynced) == nil {
		t.Errorf("the loop never reported its first success after the panics:\n%s", logs.String())
	}
}

func TestSyncReachability_AFlagMovedBySomeoneElseIsPutBackAndSaidSo(t *testing.T) {
	// the first write changes the flag (the usual start); a later write that changes it again means another writer moved it
	f := &fakeReach{fn: func(_ context.Context, n int) (bool, error) { return n == 1 || n == 3, nil }}
	_, logs := runSync(t, f, true, 4)
	lines := logs.lines(t)
	if l := find(lines, msgReachabilitySynced); l == nil || l["changed"] != true || l["level"] != "INFO" {
		t.Errorf("the first success was logged as %v, want INFO with changed=true", l)
	}
	if n := countLines(lines, msgReachabilityMoved); n != 1 {
		t.Errorf("the flag moved once after the first success and was reported %d times, want once", n)
	}
	if l := find(lines, msgReachabilityMoved); l == nil || l["level"] != "WARN" {
		t.Errorf("the report is %v, want a warning", l)
	}
	// the control: a flag nobody touches is never reported, and the first success says changed=false when it found the flag as configured
	quiet := &fakeReach{}
	_, logs2 := runSync(t, quiet, false, 4)
	lines2 := logs2.lines(t)
	if countLines(lines2, msgReachabilityMoved) != 0 || countLines(lines2, msgReachabilityFailed) != 0 {
		t.Errorf("a quiet flag produced warnings:\n%s", logs2.String())
	}
	if l := find(lines2, msgReachabilitySynced); l == nil || l["changed"] != false || l["reachable"] != false {
		t.Errorf("the first success of a flag already as configured was logged as %v, want changed=false", l)
	}
}

type weirdError struct{}

func (*weirdError) Error() string { return "something with host=db.internal user=platform_app in it" }

func TestReachabilityClass_NamesTheKindNeverTheText(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"nothing", nil, ""},
		{"a database error", fmt.Errorf("money: %w", &pgconn.PgError{Code: "40001", Message: "x"}), "database error 40001"},
		{"a refused connection by its text (Windows spells the errno differently)", errRefused, "connection refused"},
		{"a deadline", fmt.Errorf("money: %w", context.DeadlineExceeded), "timeout"},
		{"a cancel", fmt.Errorf("money: %w", context.Canceled), "canceled"},
		{"a lock that ran out", fmt.Errorf("money: %w", &money.LockTimeoutError{Wait: time.Second}), "waited too long for a lock on the unit"},
		{"an unknown unit", fmt.Errorf("money: %w: %q", money.ErrUnknownUnit, "m1_vendor"), "the unit is not priced"},
		{"a malformed request", fmt.Errorf("money: %w", &money.InvalidError{Field: "source", Reason: "blank"}), "refused as malformed"},
		{"a dns failure", fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "db.internal"}), "dns lookup failed"},
		{"a closed connection", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), "connection closed"},
		{"a panic", &setterPanic{typ: "string"}, "the setter panicked (string)"},
		{"an error of a type nobody listed", fmt.Errorf("wrapped: %w", &weirdError{}), "error (*app.weirdError)"},
	} {
		got := reachabilityClass(c.err)
		if got != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got, c.want)
		}
		for _, leak := range []string{"platform_app", "db.internal", "127.0.0.1"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: the class %q carries %q", c.name, got, leak)
			}
		}
	}
}

// ---- the service: the dial reaches the call, and the loop stops before the pool closes -------------------------------------------------------

// withReachabilitySetter replaces the builder of the loop's setter for one test.
func withReachabilitySetter(t *testing.T, build func(pool *pgxpool.Pool, mny *money.Store, cfg config.Config) reachabilitySetter) {
	t.Helper()
	old := newReachabilitySetter
	newReachabilitySetter = build
	t.Cleanup(func() { newReachabilitySetter = old })
}

// poolIsClosed asks a pool a question and reports whether the answer was "closed pool": on a pool that is open and whose database is not there the answer is a refused connection.
func poolIsClosed(pool *pgxpool.Pool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := pool.Ping(ctx)
	return err != nil && strings.Contains(err.Error(), "closed pool")
}

func TestRun_TheVendorReachableDialReachesTheSetterAndTheLoopStopsBeforeThePoolCloses(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t)

	// the instrument can see a closed pool (control), and an open one is not "closed" even though its database is not there
	probe, err := pgxpool.New(context.Background(), closedPortDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if poolIsClosed(probe) {
		t.Fatal("control failed: an open pool whose database is down was reported closed")
	}
	probe.Close()
	if !poolIsClosed(probe) {
		t.Fatal("control failed: a closed pool was not reported closed, so the ordering check below proves nothing")
	}

	// dial = true, and a pass in flight at shutdown that takes 300 ms to wind down: Run must wait for it, and the pool must still be open when it asks its question
	for _, dial := range []string{"true", "false", ""} {
		var (
			pools    atomic.Int32
			finished atomic.Bool
			sawClose atomic.Bool
		)
		f := &fakeReach{fn: func(ctx context.Context, _ int) (bool, error) {
			<-ctx.Done()
			time.Sleep(300 * time.Millisecond) // winding down: a loop that Run did not wait for would still be here when the pool closes
			sawClose.Store(poolIsClosed(lastPool.Load()))
			finished.Store(true)
			return false, ctx.Err()
		}}
		withReachabilitySetter(t, func(pool *pgxpool.Pool, _ *money.Store, cfg config.Config) reachabilitySetter {
			lastPool.Store(pool)
			pools.Add(1)
			return f
		})
		env := base(closedPortDSN(t))
		if dial != "" {
			env[config.VarM1VendorReachable] = dial
		}
		r := startApp(t, env)
		waitUntil(t, "the first write of the reachability loop", func() bool { return len(f.got()) >= 1 })
		if err := r.stop(t); err != nil {
			t.Fatalf("dial %q: Run returned %v after the cancel", dial, err)
		}
		if !finished.Load() {
			t.Errorf("dial %q: Run returned while a write was still in flight: the loop was not waited for", dial)
		}
		if sawClose.Load() {
			t.Errorf("dial %q: the pool was already closed while the loop's last write was still running", dial)
		}
		if pools.Load() != 1 || lastPool.Load() == nil {
			t.Errorf("dial %q: the setter was built %d times, want once from the service's pool", dial, pools.Load())
		}
		calls := f.got()
		want := dial == "true"
		if len(calls) != 1 || calls[0].unit != "m1_vendor" || calls[0].reachable != want || calls[0].source != "config: PLATFORM_M1_VENDOR_REACHABLE" {
			t.Errorf("dial %q: the loop wrote %+v, want one write of m1_vendor=%v from the configuration", dial, calls, want)
		}
		if l := find(r.logs.lines(t), msgReachabilityStarted); l == nil || l["reachable"] != want {
			t.Errorf("dial %q: the start line is %v, want reachable=%v", dial, l, want)
		}
		// nothing runs after Run returned (paired with the wait for the first write above, which proves it was running)
		time.Sleep(100 * time.Millisecond)
		if n := len(f.got()); n != 1 {
			t.Errorf("dial %q: the loop wrote again after Run returned (%d writes)", dial, n)
		}
	}
}

// lastPool is the pool the service handed to the builder of the setter.
var lastPool atomic.Pointer[pgxpool.Pool]

func TestRun_AnInvalidVendorReachableValueStopsTheProcessWithExitStatus2(t *testing.T) {
	for _, bad := range []string{"maybe-XYZ123", "TRUE", "yes", "1", "on", "enabled-XYZ123"} {
		logs := &syncBuffer{}
		// a bounded context: a service that wrongly accepts the value serves until it ends, and the test must then fail, not hang
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		err := Run(ctx, envOf(merge(base(closedPortDSN(t)), map[string]string{config.VarM1VendorReachable: bad})), logs)
		cancel()
		var ce *config.Error
		if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
			t.Errorf("%q: Run = %v (exit %d), want a configuration error, exit status 2", bad, err, ExitCode(err))
			continue
		}
		if len(ce.Problems) != 1 || ce.Problems[0].Name != config.VarM1VendorReachable || !strings.Contains(err.Error(), "true or false") {
			t.Errorf("%q: problems %+v, want exactly one naming %s and saying what is accepted", bad, ce.Problems, config.VarM1VendorReachable)
		}
		if len(bad) >= 5 && (strings.Contains(err.Error(), bad) || strings.Contains(logs.String(), bad)) {
			t.Errorf("%q: the value reached the error or the log", bad)
		}
		if logs.String() != "" {
			t.Errorf("%q: a process that could not read its configuration wrote logs: %s", bad, logs.String())
		}
	}

	// the twins: the two accepted spellings start the service (the setter is a fake, so no loop touches a database)
	withSuspender(t, &fakeSuspender{}, nil)
	withReachabilitySetter(t, func(*pgxpool.Pool, *money.Store, config.Config) reachabilitySetter { return &fakeReach{} })
	for _, ok := range []string{"true", "false"} {
		r := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarM1VendorReachable: ok}))
		if err := r.stop(t); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

// The reachability loop writes through THE money store of the process: the one newMoneyStore builds (waiting as long as PLATFORM_MONEY_LOCK_TIMEOUT_MS says) and apiDeps.Money keeps for the job
// store, not a second one of its own.
func TestDefaultReachabilitySetterIsTheOneMoneyStore(t *testing.T) {
	cfg, err := config.Load(envOf(base("postgres://platform_app:x@h/db")))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://platform_app:x@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := newMoneyStore(pool, cfg)
	if st.LockTimeout() != cfg.MoneyLockTimeout {
		t.Errorf("the store waits %s, want PLATFORM_MONEY_LOCK_TIMEOUT_MS's %s", st.LockTimeout(), cfg.MoneyLockTimeout)
	}
	// the control: another value of the dial is another wait (the line above is not satisfied by a constant)
	cfg2, err := config.Load(envOf(merge(base("postgres://platform_app:x@h/db"), map[string]string{config.VarMoneyLockTimeoutMS: "2500"})))
	if err != nil {
		t.Fatal(err)
	}
	if got := newMoneyStore(pool, cfg2).LockTimeout(); got != 2500*time.Millisecond || got == st.LockTimeout() {
		t.Errorf("a configured lock timeout of 2.5s reached the store as %s", got)
	}
	// the default setter IS the store it is handed (one store, not a copy of it), and a store that was not handed is not made up
	if s, ok := newReachabilitySetter(pool, st, cfg).(*money.Store); !ok || s != st {
		t.Fatalf("the default builder returned %T (%p), want the very store it was given (%p)", s, s, st)
	}
	if other := newMoneyStore(pool, cfg); other == st {
		t.Fatal("two calls of newMoneyStore returned one store: the identity check above proves nothing")
	}
}
