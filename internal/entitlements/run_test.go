package entitlements

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/m3admin"
)

// The loops are tested without a database: the Service takes its two workers through small interfaces, and these
// fakes stand in for the Store and the Reconciler. What is under test is the machinery around them (start, stop,
// survive a failure, bound a pass), not the SQL, which the harness tests run against a real PostgreSQL.

type fakeExpirer struct {
	calls atomic.Int64
	fn    func(ctx context.Context, n int64) (ExpireReport, error)
}

func (f *fakeExpirer) ExpireDueReport(ctx context.Context, limit int) (ExpireReport, error) {
	n := f.calls.Add(1)
	if f.fn == nil {
		return ExpireReport{}, nil
	}
	return f.fn(ctx, n)
}

type fakeReconciler struct {
	calls atomic.Int64
	fn    func(ctx context.Context, n int64) (Report, error)
}

func (f *fakeReconciler) RunOnce(ctx context.Context) (Report, error) {
	n := f.calls.Add(1)
	if f.fn == nil {
		return Report{Unconverged: 0}, nil
	}
	return f.fn(ctx, n)
}

// lockedBuf is a log sink that can be read while the loops write to it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func debugLogger(buf *lockedBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testService(t *testing.T, ex expirer, rec reconciler, mutate func(*Config)) (*Service, *lockedBuf) {
	t.Helper()
	buf := &lockedBuf{}
	cfg := Config{Log: debugLogger(buf), ExpireEvery: 5 * time.Millisecond, ReconcileEvery: 5 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := newService(cfg, ex, rec)
	if err != nil {
		t.Fatal(err)
	}
	return s, buf
}

// start runs the service and returns a stop function that cancels it and waits (bounded) for Run to return.
func start(t *testing.T, s *Service) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return within 10s of the cancel: a loop is stuck")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// lazyPool is a real pool that has not connected to anything: pgxpool opens connections on demand, so building
// one needs no database (the service starts the same way when the database is down).
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type nopSuspender struct{}

func (nopSuspender) SetSuspended(context.Context, string, bool, string) (m3admin.Result, error) {
	return m3admin.Result{}, nil
}

func (nopSuspender) Suspension(context.Context, string) (m3admin.SuspensionState, error) {
	return m3admin.SuspensionState{}, nil
}

func TestNew_RefusesWhatCannotRun(t *testing.T) {
	ok := Config{Pool: lazyPool(t), M3: nopSuspender{}, ExpireEvery: time.Second, ReconcileEvery: time.Second, ReverifyEvery: time.Minute}
	// Control: the complete configuration builds, so each refusal below is about the one thing taken away.
	if _, err := New(ok); err != nil {
		t.Fatalf("control failed: a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no pool":                     func(c *Config) { c.Pool = nil },
		"no client":                   func(c *Config) { c.M3 = nil },
		"expire interval zero":        func(c *Config) { c.ExpireEvery = 0 },
		"expire interval negative":    func(c *Config) { c.ExpireEvery = -time.Second },
		"reconcile interval zero":     func(c *Config) { c.ReconcileEvery = 0 },
		"reconcile interval negative": func(c *Config) { c.ReconcileEvery = -time.Second },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted a configuration that cannot run (a loop would spin or never tick)", name)
		}
		if err := Run(context.Background(), cfg); err == nil {
			t.Errorf("%s: Run accepted it", name)
		}
	}
	// Defaults are applied where the configuration leaves a bound at zero.
	s, _ := testService(t, &fakeExpirer{}, &fakeReconciler{}, nil)
	if s.expireTimeout != DefaultExpireTimeout || s.reconcileTimeout != DefaultReconcileTimeout || s.batch != DefaultExpireBatch {
		t.Errorf("defaults not applied: %s %s %d", s.expireTimeout, s.reconcileTimeout, s.batch)
	}
	// And a bound that is set is kept.
	s2, _ := testService(t, &fakeExpirer{}, &fakeReconciler{}, func(c *Config) { c.ExpireTimeout, c.ReconcileTimeout, c.ExpireBatch = time.Second, 2*time.Second, 7 })
	if s2.expireTimeout != time.Second || s2.reconcileTimeout != 2*time.Second || s2.batch != 7 {
		t.Errorf("configured bounds not kept: %s %s %d", s2.expireTimeout, s2.reconcileTimeout, s2.batch)
	}
}

func TestService_BothLoopsRunAndStopTogether(t *testing.T) {
	ex, rec := &fakeExpirer{}, &fakeReconciler{}
	s, buf := testService(t, ex, rec, nil)
	stop := start(t, s)

	waitFor(t, "three passes of each loop", func() bool { return ex.calls.Load() >= 3 && rec.calls.Load() >= 3 })
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after the cancel, want nil", err)
	}

	// After Run has returned nothing runs any more (paired control: before the stop, the counters moved).
	e, r := ex.calls.Load(), rec.calls.Load()
	time.Sleep(60 * time.Millisecond)
	if ex.calls.Load() != e || rec.calls.Load() != r {
		t.Errorf("a loop kept running after Run returned: expiry %d -> %d, reconcile %d -> %d", e, ex.calls.Load(), r, rec.calls.Load())
	}
	for _, line := range []string{"entitlement expiry loop started", "entitlement reconcile loop started", "entitlement loops stopped"} {
		if !strings.Contains(buf.String(), line) {
			t.Errorf("the log has no %q line:\n%s", line, buf.String())
		}
	}
}

func TestService_TheFirstPassIsImmediateAndTheIntervalIsTheWait(t *testing.T) {
	ex, rec := &fakeExpirer{}, &fakeReconciler{}
	s, _ := testService(t, ex, rec, func(c *Config) { c.ExpireEvery, c.ReconcileEvery = time.Hour, time.Hour })
	start(t, s)
	waitFor(t, "the first pass of each loop", func() bool { return ex.calls.Load() == 1 && rec.calls.Load() == 1 })
	time.Sleep(80 * time.Millisecond)
	if ex.calls.Load() != 1 || rec.calls.Load() != 1 {
		t.Errorf("an hour's interval ran %d and %d passes in 80ms: the interval is not the wait", ex.calls.Load(), rec.calls.Load())
	}
	// Control: the 5ms interval of the other tests ticks many times in the same time.
	fast := &fakeExpirer{}
	s2, _ := testService(t, fast, &fakeReconciler{}, nil)
	start(t, s2)
	time.Sleep(80 * time.Millisecond)
	if fast.calls.Load() < 5 {
		t.Errorf("control failed: a 5ms interval ran only %d passes in 80ms", fast.calls.Load())
	}
}

func TestService_AFailingOrPanickingPassNeverStopsALoop(t *testing.T) {
	boom := errors.New("pass failed")
	ex := &fakeExpirer{fn: func(_ context.Context, n int64) (ExpireReport, error) {
		switch {
		case n == 1:
			panic("expiry exploded")
		case n < 5:
			return ExpireReport{Due: 2}, boom
		}
		return ExpireReport{}, nil
	}}
	rec := &fakeReconciler{fn: func(_ context.Context, n int64) (Report, error) {
		if n == 2 {
			panic(errors.New("reconcile exploded"))
		}
		return Report{Unconverged: -1}, boom
	}}
	s, buf := testService(t, ex, rec, nil)
	start(t, s)

	waitFor(t, "ten passes of each loop after the panics and failures", func() bool { return ex.calls.Load() >= 10 && rec.calls.Load() >= 10 })
	// The expiry loop recovered: its later passes succeeded and the health says so.
	waitFor(t, "the expiry health to clear", func() bool { return s.Health().LastExpiryError == "" && !s.Health().LastExpiry.IsZero() })
	if h := s.Health(); h.LastReconcileError == "" {
		t.Errorf("the reconcile loop fails every pass but its health shows no error: %+v", h)
	}
	logs := buf.String()
	if !strings.Contains(logs, "pass panicked") || !strings.Contains(logs, "expiry exploded") || !strings.Contains(logs, "reconcile exploded") {
		t.Errorf("the panics were not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "entitlement expiry pass succeeded again") {
		t.Errorf("the recovery of the expiry loop was not announced:\n%s", logs)
	}
}

func TestService_EachPassIsBoundedByItsOwnTimeout(t *testing.T) {
	var sawDeadline atomic.Bool
	ex := &fakeExpirer{fn: func(ctx context.Context, n int64) (ExpireReport, error) {
		if _, ok := ctx.Deadline(); ok {
			sawDeadline.Store(true)
		}
		<-ctx.Done() // a database that never answers: only the pass's own timeout frees it
		return ExpireReport{}, ctx.Err()
	}}
	rec := &fakeReconciler{}
	s, _ := testService(t, ex, rec, func(c *Config) { c.ExpireTimeout = 30 * time.Millisecond })
	start(t, s)

	waitFor(t, "three timed-out expiry passes", func() bool { return ex.calls.Load() >= 3 })
	if !sawDeadline.Load() {
		t.Error("the pass context had no deadline")
	}
	// The hanging loop does not hold the other one back (paired control).
	if rec.calls.Load() < 5 {
		t.Errorf("the reconcile loop ran only %d passes while the expiry loop was hanging", rec.calls.Load())
	}
	if h := s.Health(); h.LastExpiryError != "timeout" {
		t.Errorf("the expiry health says %q, want the class \"timeout\"", h.LastExpiryError)
	}
}

func TestService_AShutdownDuringAPassIsNotLoggedAsAFailure(t *testing.T) {
	entered := make(chan struct{}, 1)
	ex := &fakeExpirer{fn: func(ctx context.Context, _ int64) (ExpireReport, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ExpireReport{}, ctx.Err()
	}}
	s, buf := testService(t, ex, &fakeReconciler{}, nil)
	stop := start(t, s)
	<-entered
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("shutting down mid-pass produced a warning:\n%s", buf.String())
	}
}

func TestService_ConfigFaultSlowsTheReconcileLoopButKeepsItRunning(t *testing.T) {
	fault := &fakeReconciler{fn: func(context.Context, int64) (Report, error) {
		return Report{ConfigFault: true, Unconverged: 3, Attempted: 1, Failed: 1}, nil
	}}
	s, _ := testService(t, &fakeExpirer{}, fault, func(c *Config) { c.ReconcileEvery = 5 * time.Millisecond })
	if wait := s.reconcilePass(context.Background()); wait != ConfigFaultInterval {
		t.Errorf("after a config fault the next pass is in %s, want %s", wait, ConfigFaultInterval)
	}
	if h := s.Health(); !h.ConfigFault || h.Unconverged != 3 {
		t.Errorf("health = %+v, want ConfigFault and 3 unconverged", h)
	}
	// An interval already longer than the pacing is kept (paired control), and a healthy pass uses the interval.
	long, _ := testService(t, &fakeExpirer{}, fault, func(c *Config) { c.ReconcileEvery = time.Hour })
	if wait := long.reconcilePass(context.Background()); wait != time.Hour {
		t.Errorf("an hour's interval became %s after a config fault", wait)
	}
	healthy := &fakeReconciler{}
	s2, _ := testService(t, &fakeExpirer{}, healthy, nil)
	if wait := s2.reconcilePass(context.Background()); wait != 5*time.Millisecond {
		t.Errorf("a healthy pass waits %s, want the interval", wait)
	}
	if h := s2.Health(); h.ConfigFault {
		t.Error("a healthy pass left ConfigFault set")
	}
	// The loop keeps trying: the fault is not a reason to stop.
	if _, err := newService(Config{ExpireEvery: time.Second, ReconcileEvery: time.Second}, &fakeExpirer{}, fault); err != nil {
		t.Fatal(err)
	}
}

func TestService_HealthSnapshot(t *testing.T) {
	clock := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	ex := &fakeExpirer{fn: func(context.Context, int64) (ExpireReport, error) {
		return ExpireReport{Due: 3, Expired: []Expired{{UserID: userA}, {UserID: adminA}}, Skipped: 1}, nil
	}}
	rec := &fakeReconciler{fn: func(context.Context, int64) (Report, error) {
		return Report{Attempted: 4, Converged: 3, Failed: 1, Unconverged: 7}, nil
	}}
	s, _ := testService(t, ex, rec, func(c *Config) { c.Now = func() time.Time { return clock } })

	if h := s.Health(); !h.LastExpiry.IsZero() || !h.LastReconcile.IsZero() || h.Unconverged != -1 {
		t.Fatalf("health before any pass = %+v, want zero times and Unconverged -1 (unknown)", h)
	}
	s.expiryPass(context.Background())
	s.reconcilePass(context.Background())
	s.expiryPass(context.Background())
	h := s.Health()
	if !h.LastExpiry.Equal(clock) || !h.LastReconcile.Equal(clock) {
		t.Errorf("last pass times = %v, %v, want the injected clock %v", h.LastExpiry, h.LastReconcile, clock)
	}
	if h.Unconverged != 7 || h.Expired != 4 || h.Delivered != 3 {
		t.Errorf("health = %+v, want 7 unconverged, 4 expired (two passes of two), 3 delivered", h)
	}
	if h.LastExpiryError != "" || h.LastReconcileError != "" || h.ConfigFault {
		t.Errorf("healthy passes left an error behind: %+v", h)
	}
	// A pass that could not read the count leaves the previous value (-1 is "unknown", not zero users).
	rec.fn = func(context.Context, int64) (Report, error) { return Report{Unconverged: -1}, errors.New("db down") }
	s.reconcilePass(context.Background())
	if h := s.Health(); h.Unconverged != 7 || h.LastReconcileError == "" {
		t.Errorf("after a failed count: %+v, want the previous 7 and an error class", h)
	}
	// The returned snapshot is a copy.
	h.Expired = 1_000_000
	if s.Health().Expired == 1_000_000 {
		t.Error("Health returned a reference into the service")
	}
}

func TestService_ALoggedFailureIsReportedOncePerClassNotOncePerPass(t *testing.T) {
	down := &pgconn.PgError{Code: "57P01", Message: "terminating connection"}
	var failing atomic.Bool
	failing.Store(true)
	ex := &fakeExpirer{fn: func(context.Context, int64) (ExpireReport, error) {
		if failing.Load() {
			return ExpireReport{}, down
		}
		return ExpireReport{}, nil
	}}
	s, buf := testService(t, ex, &fakeReconciler{}, nil)
	for i := 0; i < 6; i++ {
		s.expiryPass(context.Background())
	}
	if n := strings.Count(buf.String(), "entitlement expiry pass failed"); n != 1 {
		t.Errorf("six identical failures logged %d warnings, want exactly one:\n%s", n, buf.String())
	}
	// A different class is a new warning; recovery is announced once; a new outage warns again.
	ex.fn = func(context.Context, int64) (ExpireReport, error) { return ExpireReport{}, context.DeadlineExceeded }
	s.expiryPass(context.Background())
	if n := strings.Count(buf.String(), "entitlement expiry pass failed"); n != 2 {
		t.Errorf("a new failure class logged %d warnings in all, want 2", n)
	}
	ex.fn = func(context.Context, int64) (ExpireReport, error) { return ExpireReport{}, nil }
	s.expiryPass(context.Background())
	s.expiryPass(context.Background())
	if n := strings.Count(buf.String(), "succeeded again"); n != 1 {
		t.Errorf("recovery announced %d times, want once", n)
	}
	ex.fn = func(context.Context, int64) (ExpireReport, error) { return ExpireReport{}, down }
	s.expiryPass(context.Background())
	if n := strings.Count(buf.String(), "entitlement expiry pass failed"); n != 3 {
		t.Errorf("a second outage logged %d warnings in all, want 3", n)
	}
	// The class is logged, the driver's text is not.
	if !strings.Contains(buf.String(), "database error 57P01") || strings.Contains(buf.String(), "terminating connection") {
		t.Errorf("the log must carry the failure class and not the driver's message:\n%s", buf.String())
	}
}

func TestService_ExpiredUsersAreLogged(t *testing.T) {
	ex := &fakeExpirer{fn: func(context.Context, int64) (ExpireReport, error) {
		return ExpireReport{Due: 1, Expired: []Expired{{UserID: userA, EnabledUntil: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC), LedgerID: 42}}}, nil
	}}
	s, buf := testService(t, ex, &fakeReconciler{}, nil)
	s.expiryPass(context.Background())
	for _, want := range []string{"entitlement expired", "user=" + userA, "ledger_id=42", "enabled_until=2031-01-01T00:00:00Z"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, buf.String())
		}
	}
}
