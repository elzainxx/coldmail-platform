package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Bounds of the two loops. They are pacing and sanity limits, not dials (a dial is declared in
// internal/config.Vars with a consumer; these keep one pass from running unbounded).
const (
	// DefaultExpireTimeout bounds one pass of the expiry loop.
	DefaultExpireTimeout = 30 * time.Second
	// DefaultReconcileTimeout bounds one pass of the reconcile loop: a hanging module 3 costs one request
	// timeout per row, and this keeps the whole pass finite.
	DefaultReconcileTimeout = 2 * time.Minute
	// DefaultExpireBatch is how many due entitlements one expiry pass takes.
	DefaultExpireBatch = 100
	// ConfigFaultInterval is the least wait between reconcile passes after module 3 rejected the admin key:
	// module 3 compares the key with bcrypt on every request, so a wrong key must not be retried at the
	// reconcile interval, and nothing is lost by waiting (no user can converge until the key is fixed).
	ConfigFaultInterval = 30 * time.Second
)

// Config is what Run needs. The intervals come from the configuration (internal/config); the rest is wiring.
type Config struct {
	Pool *pgxpool.Pool
	// M3 is module 3's admin client (or a fake).
	M3  Suspender
	Log *slog.Logger

	// ExpireEvery is the wait between expiry passes; ReconcileEvery between reconcile passes; ReverifyEvery
	// is the age after which a converged user is checked against module 3 again. All must be positive.
	ExpireEvery    time.Duration
	ReconcileEvery time.Duration
	ReverifyEvery  time.Duration

	// ExpireTimeout and ReconcileTimeout bound one pass; zero means the Default constants.
	ExpireTimeout    time.Duration
	ReconcileTimeout time.Duration
	// ExpireBatch is how many due entitlements one pass takes; zero means DefaultExpireBatch.
	ExpireBatch int

	// Redact lists secrets removed from every error text that is stored or logged (module 3's admin key).
	Redact []string
	// Now stamps the health snapshot; it defaults to time.Now and decides nothing about entitlements.
	Now func() time.Time
}

// Health is a snapshot of what the loops have done, for a later readiness probe (this phase does not wire it to
// /readyz). Times are the process clock's and say when a pass finished, nothing about an entitlement.
type Health struct {
	// LastExpiry is when the last expiry pass finished (zero before the first); LastExpiryError is its failure
	// class, "" when it succeeded.
	LastExpiry      time.Time
	LastExpiryError string
	// LastReconcile and LastReconcileError are the same for the reconcile loop. A module-3 failure for one user
	// is not a pass failure: it shows in Unconverged.
	LastReconcile      time.Time
	LastReconcileError string
	// Unconverged is the number of users module 3 has not been seen in the wanted state for, as of the last
	// reconcile pass; -1 before the first or when it could not be read.
	Unconverged int
	// ConfigFault is true while module 3 rejects the admin key.
	ConfigFault bool
	// Expired and Delivered are totals since the process started: entitlements expired by this process, and
	// confirmed deliveries recorded by it.
	Expired   int64
	Delivered int64
}

// expirer and reconciler are what the loops call; the real ones are *Store and *Reconciler, a unit test
// supplies fakes.
type expirer interface {
	ExpireDueReport(ctx context.Context, limit int) (ExpireReport, error)
}

type reconciler interface {
	RunOnce(ctx context.Context) (Report, error)
}

// Service runs the expiry loop and the reconcile loop.
type Service struct {
	log       *slog.Logger
	expire    expirer
	reconcile reconciler
	now       func() time.Time

	expireEvery, reconcileEvery     time.Duration
	expireTimeout, reconcileTimeout time.Duration
	batch                           int

	mu       sync.Mutex
	health   Health
	lastFail map[string]string // loop name -> the failure class last reported, "" while the loop is healthy
}

// New builds a Service. It validates before anything runs: a missing pool or client, or an interval that is not
// positive, is an error here and not a loop that spins or never ticks.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Pool == nil:
		return nil, errors.New("entitlements: Config.Pool is required")
	case cfg.M3 == nil:
		return nil, errors.New("entitlements: Config.M3 is required")
	}
	rec := &Reconciler{Pool: cfg.Pool, M3: cfg.M3, ReverifyEvery: cfg.ReverifyEvery, Log: cfg.Log, Redact: cfg.Redact, Now: cfg.Now}
	return newService(cfg, NewStore(cfg.Pool), rec)
}

// newService is New with the two workers supplied (the seam of the unit tests).
func newService(cfg Config, ex expirer, rec reconciler) (*Service, error) {
	for name, d := range map[string]time.Duration{"ExpireEvery": cfg.ExpireEvery, "ReconcileEvery": cfg.ReconcileEvery} {
		if d <= 0 {
			return nil, fmt.Errorf("entitlements: Config.%s must be positive, got %s", name, d)
		}
	}
	s := &Service{
		log:              cfg.Log,
		expire:           ex,
		reconcile:        rec,
		now:              cfg.Now,
		expireEvery:      cfg.ExpireEvery,
		reconcileEvery:   cfg.ReconcileEvery,
		expireTimeout:    cfg.ExpireTimeout,
		reconcileTimeout: cfg.ReconcileTimeout,
		batch:            cfg.ExpireBatch,
		health:           Health{Unconverged: -1},
		lastFail:         map[string]string{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.expireTimeout <= 0 {
		s.expireTimeout = DefaultExpireTimeout
	}
	if s.reconcileTimeout <= 0 {
		s.reconcileTimeout = DefaultReconcileTimeout
	}
	if s.batch <= 0 {
		s.batch = DefaultExpireBatch
	}
	return s, nil
}

// Run builds a Service from cfg and runs it until ctx is cancelled (see Service.Run).
func Run(ctx context.Context, cfg Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// Health returns a copy of the current snapshot.
func (s *Service) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// Run starts the two loops and returns, nil, only after both have stopped, which happens when ctx is cancelled.
// A pass that fails, times out or panics is logged and the loop carries on at its next interval: nothing a pass
// does ends a loop.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, l := range []struct {
		name string
		pass func(context.Context) time.Duration
		wait time.Duration
	}{
		{"expiry", s.expiryPass, s.expireEvery},
		{"reconcile", s.reconcilePass, s.reconcileEvery},
	} {
		s.log.InfoContext(ctx, "entitlement "+l.name+" loop started", "every", l.wait.String())
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, l.name, l.pass)
		}()
	}
	wg.Wait()
	s.log.Info("entitlement loops stopped")
	return nil
}

// loop runs pass now and then after each wait it returns, until ctx is cancelled.
func (s *Service) loop(ctx context.Context, name string, pass func(context.Context) time.Duration) {
	for ctx.Err() == nil {
		wait := s.safely(ctx, name, pass)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// safely runs one pass and turns a panic into a log line and the loop's ordinary wait.
func (s *Service) safely(ctx context.Context, name string, pass func(context.Context) time.Duration) (wait time.Duration) {
	wait = s.intervalOf(name)
	defer func() {
		if p := recover(); p != nil {
			s.log.ErrorContext(ctx, "entitlement "+name+" pass panicked; the loop carries on", "panic", scrubText(fmt.Sprint(p)))
			s.mu.Lock()
			defer s.mu.Unlock()
			if name == "expiry" {
				s.health.LastExpiry, s.health.LastExpiryError = s.now(), "panic"
			} else {
				s.health.LastReconcile, s.health.LastReconcileError = s.now(), "panic"
			}
		}
	}()
	return pass(ctx)
}

func (s *Service) intervalOf(name string) time.Duration {
	if name == "expiry" {
		return s.expireEvery
	}
	return s.reconcileEvery
}

// edge notes the outcome of a pass of the named loop and says what to log about it: a failure class that differs
// from the one last reported is a warning, the same class again is quiet (the debug line of every pass still
// shows it), and the first success after failures is announced once. A database outage therefore costs one
// warning per loop, not one per pass.
func (s *Service) edge(name, class string) (changed, recovered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.lastFail[name]
	s.lastFail[name] = class
	return class != "" && class != prev, class == "" && prev != ""
}

// expiryPass is one pass of the expiry loop, bounded by its own timeout. It returns the wait before the next.
func (s *Service) expiryPass(parent context.Context) time.Duration {
	ctx, cancel := context.WithTimeout(parent, s.expireTimeout)
	defer cancel()
	rep, err := s.expire.ExpireDueReport(ctx, s.batch)
	class := errClass(err)

	for _, ex := range rep.Expired {
		s.log.InfoContext(parent, "entitlement expired", "user", ex.UserID, "enabled_until", ex.EnabledUntil.UTC().Format(time.RFC3339Nano), "ledger_id", ex.LedgerID)
	}
	s.log.DebugContext(parent, "entitlement expiry pass", "due", rep.Due, "expired", len(rep.Expired), "skipped", rep.Skipped, "error_class", class)
	if parent.Err() == nil { // a pass cut off by shutdown is not a failure worth a warning
		changed, recovered := s.edge("expiry", class)
		if changed {
			s.log.WarnContext(parent, "entitlement expiry pass failed; the loop retries at its next interval", "reason", class, "due", rep.Due, "expired", len(rep.Expired))
		}
		if recovered {
			s.log.InfoContext(parent, "entitlement expiry pass succeeded again")
		}
	}

	s.mu.Lock()
	s.health.LastExpiry = s.now()
	s.health.LastExpiryError = class
	s.health.Expired += int64(len(rep.Expired))
	s.mu.Unlock()
	return s.expireEvery
}

// reconcilePass is one pass of the reconcile loop, bounded by its own timeout. It returns the wait before the next:
// the interval, or at least ConfigFaultInterval while module 3 rejects the admin key.
func (s *Service) reconcilePass(parent context.Context) time.Duration {
	ctx, cancel := context.WithTimeout(parent, s.reconcileTimeout)
	defer cancel()
	rep, err := s.reconcile.RunOnce(ctx)
	class := errClass(err)

	s.log.DebugContext(parent, "entitlement reconcile pass", "attempted", rep.Attempted, "converged", rep.Converged, "failed", rep.Failed,
		"drifted", rep.Drifted, "stale", rep.StaleSkipped, "backoff_skipped", rep.BackoffSkipped, "unconverged", rep.Unconverged, "error_class", class)
	if parent.Err() == nil {
		changed, recovered := s.edge("reconcile", class)
		if changed {
			s.log.WarnContext(parent, "entitlement reconcile pass failed; the loop retries at its next interval", "reason", class, "attempted", rep.Attempted, "converged", rep.Converged)
		}
		if recovered {
			s.log.InfoContext(parent, "entitlement reconcile pass succeeded again")
		}
	}

	s.mu.Lock()
	s.health.LastReconcile = s.now()
	s.health.LastReconcileError = class
	s.health.ConfigFault = rep.ConfigFault
	if rep.Unconverged >= 0 {
		s.health.Unconverged = rep.Unconverged
	}
	s.health.Delivered += int64(rep.Converged)
	s.mu.Unlock()

	if rep.ConfigFault && s.reconcileEvery < ConfigFaultInterval {
		return ConfigFaultInterval
	}
	return s.reconcileEvery
}
