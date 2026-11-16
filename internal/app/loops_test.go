package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/m3admin"
)

// The entitlement loops are wired into the service here. No database is needed: the pool points at a closed
// port, so every pass of both loops FAILS, which is exactly the case that must not stop a loop or the server. What
// they do against a real database and a real module 3 is proven by the harness (tests/harness).

// fakeSuspender is a Suspender that is never reached in these tests (there are no rows to deliver without a database);
// it exists so a test can see which configuration the service built its client from.
type fakeSuspender struct{ calls atomic.Int64 }

func (f *fakeSuspender) SetSuspended(context.Context, string, bool, string) (m3admin.Result, error) {
	f.calls.Add(1)
	return m3admin.Result{}, errors.New("fake: not reachable in this test")
}

// withSuspender replaces the module 3 client builder for one test and returns what it was asked to build from.
func withSuspender(t *testing.T, fake entitlements.Suspender, buildErr error) *atomic.Pointer[config.Config] {
	t.Helper()
	var got atomic.Pointer[config.Config]
	old := newSuspender
	newSuspender = func(cfg config.Config) (entitlements.Suspender, error) {
		got.Store(&cfg)
		return fake, buildErr
	}
	t.Cleanup(func() { newSuspender = old })
	return &got
}

// countLines counts the log lines with this message.
func countLines(lines []map[string]any, msg string) int {
	n := 0
	for _, l := range lines {
		if l["msg"] == msg {
			n++
		}
	}
	return n
}

func indexOf(lines []map[string]any, msg string) int {
	for i, l := range lines {
		if l["msg"] == msg {
			return i
		}
	}
	return -1
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRun_TheEntitlementLoopsStartWithTheServiceAndStopBeforeItReturns(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t) // this test is about the loops' own passes against a database that is not there; with the election on no instance could lead here (leader_test.go proves that)
	env := merge(base(closedPortDSN(t)), map[string]string{
		config.VarLogLevel:                     "debug",
		config.VarEntitlementExpireIntervalSec: "1",
		config.VarM3ReconcileIntervalSec:       "1",
	})
	r := startApp(t, env)

	waitUntil(t, "both loops to announce themselves", func() bool {
		l := r.logs.lines(t)
		return find(l, "entitlement expiry loop started") != nil && find(l, "entitlement reconcile loop started") != nil
	})
	// The loops make their passes (one at once, then one per second) and every one FAILS here, since there is no
	// database: that must not stop either loop, nor the server.
	waitUntil(t, "two passes of each loop", func() bool {
		l := r.logs.lines(t)
		return countLines(l, "entitlement reconcile pass") >= 2 && countLines(l, "entitlement expiry pass") >= 2
	})
	if status, _, _ := fetch(t, "http://"+r.addr+"/healthz"); status != http.StatusOK {
		t.Errorf("healthz = %d while the loops are failing: liveness must not depend on them", status)
	}
	if status, body, _ := fetch(t, "http://"+r.addr+"/readyz"); status != http.StatusServiceUnavailable || body["db"] != false {
		t.Errorf("readyz = %d %v: this phase must not change what readiness says (the database is down, so 503)", status, body)
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("the service returned %v after the cancel, want a clean drain", err)
	}
	lines := r.logs.lines(t)
	stopped, final := indexOf(lines, "entitlement loops stopped"), indexOf(lines, "platform stopped")
	if stopped < 0 || final < 0 || stopped > final {
		t.Errorf("the loops must have stopped (line %d) before the service reports it stopped (line %d)", stopped, final)
	}
	// Nothing runs after Run returned: the pass count is frozen (paired control: it was moving while the service ran).
	before := countLines(r.logs.lines(t), "entitlement reconcile pass")
	time.Sleep(1300 * time.Millisecond)
	if after := countLines(r.logs.lines(t), "entitlement reconcile pass"); after != before {
		t.Errorf("a loop kept running after the service returned: %d -> %d reconcile passes", before, after)
	}
	// The failure of the passes was reported once as a class, never with the driver's text, the login or the key.
	logs := r.logs.String()
	if !strings.Contains(logs, "entitlement reconcile pass failed") || !strings.Contains(logs, "connection refused") {
		t.Errorf("the failing passes were not reported with their class:\n%s", logs)
	}
	for _, leak := range []string{canary, "platform_app", m3Canary} {
		if strings.Contains(logs, leak) {
			t.Errorf("the log carries %q:\n%s", leak, logs)
		}
	}
}

func TestRun_TheIntervalDialsSetTheRhythmOfTheLoops(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	withoutElection(t) // the rhythm is counted in passes, and a standby makes none
	count := func(env map[string]string) (reconcile, expiry int) {
		r := startApp(t, env)
		waitUntil(t, "the first pass of each loop", func() bool {
			l := r.logs.lines(t)
			return countLines(l, "entitlement reconcile pass") >= 1 && countLines(l, "entitlement expiry pass") >= 1
		})
		time.Sleep(2500 * time.Millisecond)
		l := r.logs.lines(t)
		if err := r.stop(t); err != nil {
			t.Fatal(err)
		}
		return countLines(l, "entitlement reconcile pass"), countLines(l, "entitlement expiry pass")
	}
	fast := merge(base(closedPortDSN(t)), map[string]string{
		config.VarLogLevel: "debug", config.VarEntitlementExpireIntervalSec: "1", config.VarM3ReconcileIntervalSec: "1",
	})
	slow := merge(base(closedPortDSN(t)), map[string]string{
		config.VarLogLevel: "debug", config.VarEntitlementExpireIntervalSec: "3600", config.VarM3ReconcileIntervalSec: "3600",
	})
	fastRec, fastExp := count(fast)
	slowRec, slowExp := count(slow)
	if fastRec < 3 || fastExp < 3 {
		t.Errorf("with 1 s intervals the loops made %d reconcile and %d expiry passes in about 2.5 s, want at least 3 each", fastRec, fastExp)
	}
	// Paired control: with hour-long intervals the same service made exactly the immediate first pass and no more.
	if slowRec != 1 || slowExp != 1 {
		t.Errorf("with 3600 s intervals the loops made %d reconcile and %d expiry passes, want exactly the first one each", slowRec, slowExp)
	}
}

func TestRun_TheModule3DialsReachTheClientBuilder(t *testing.T) {
	fake := &fakeSuspender{}
	got := withSuspender(t, fake, nil)
	env := merge(base(closedPortDSN(t)), map[string]string{
		config.VarM3URL:                        "https://m3.internal:9443",
		config.VarM3AdminToken:                 "another-key-" + m3Canary,
		config.VarM3RequestTimeoutSec:          "7",
		config.VarM3ReverifyIntervalSec:        "600",
		config.VarM3ReconcileIntervalSec:       "9",
		config.VarEntitlementExpireIntervalSec: "77",
	})
	r := startApp(t, env)
	waitUntil(t, "both loops to announce themselves", func() bool {
		l := r.logs.lines(t)
		return find(l, "entitlement expiry loop started") != nil && find(l, "entitlement reconcile loop started") != nil
	})
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	cfg := got.Load()
	if cfg == nil {
		t.Fatal("the module 3 client builder was never called")
	}
	if cfg.M3URL != "https://m3.internal:9443" || cfg.M3AdminToken.Reveal() != "another-key-"+m3Canary || cfg.M3RequestTimeout != 7*time.Second {
		t.Errorf("the builder received %v, want the configured URL, key and request timeout", *cfg)
	}
	if cfg.M3ReverifyInterval != 600*time.Second || cfg.M3ReconcileInterval != 9*time.Second || cfg.EntitlementExpireInterval != 77*time.Second {
		t.Errorf("the builder's configuration lost the loop dials: %v", *cfg)
	}
	// The loops announce the intervals they were built with: the dials reach the loops, not only the builder.
	logs := r.logs.String()
	if !strings.Contains(logs, `"msg":"entitlement expiry loop started"`) || !strings.Contains(logs, `"every":"1m17s"`) || !strings.Contains(logs, `"every":"9s"`) {
		t.Errorf("the loops did not start with the configured intervals (77 s = 1m17s and 9 s):\n%s", logs)
	}
	if strings.Contains(logs, m3Canary) {
		t.Errorf("the module 3 key reached the log:\n%s", logs)
	}
}

func TestRun_AClientThatCannotBeBuiltStopsStartupBeforeAnythingListens(t *testing.T) {
	withSuspender(t, nil, errors.New("build the module 3 client: the base URL is unusable"))
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), envOf(base(closedPortDSN(t))), logs) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "module 3 client") {
			t.Fatalf("Run = %v, want the client build error", err)
		}
		if ExitCode(err) != ExitFailure {
			t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitFailure)
		}
		if strings.Contains(err.Error(), m3Canary) || strings.Contains(logs.String(), m3Canary) {
			t.Error("the module 3 key reached the error or the log")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if find(logs.lines(t), "http server listening") != nil || find(logs.lines(t), "entitlement expiry loop started") != nil {
		t.Errorf("the service started although its module 3 client could not be built:\n%s", logs.String())
	}
	// Control: with a client that builds, the same environment listens (the failure above is about the client).
	withSuspender(t, &fakeSuspender{}, nil)
	r := startApp(t, base(closedPortDSN(t)))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestRun_MissingModule3VariablesAreConfigErrorsThatNameThem(t *testing.T) {
	logs := &syncBuffer{}
	env := merge(base(closedPortDSN(t)), nil)
	delete(env, config.VarM3URL)
	delete(env, config.VarM3AdminToken)
	err := Run(context.Background(), envOf(env), logs)
	var ce *config.Error
	if !errors.As(err, &ce) || ExitCode(err) != ExitConfig {
		t.Fatalf("Run = %v (exit %d), want a configuration error with exit status %d", err, ExitCode(err), ExitConfig)
	}
	for _, name := range []string{config.VarM3URL, config.VarM3AdminToken} {
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "required") {
			t.Errorf("the error does not say that %s is required: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), config.VarDatabaseURL) {
		t.Errorf("the error names %s, which is set: %v", config.VarDatabaseURL, err)
	}
	if logs.String() != "" {
		t.Errorf("a process that could not read its configuration wrote logs: %s", logs.String())
	}
}

func TestNewM3HTTPClient_FollowsTheHouseStyle(t *testing.T) {
	var hits atomic.Int64
	var final atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		final.Add(1)
	}))
	defer srv.Close()

	c := newM3HTTPClient(1500 * time.Millisecond)
	if c.Timeout != 1500*time.Millisecond {
		t.Errorf("the timeout is %s, want the configured one", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableKeepAlives {
		t.Errorf("transport = %#v, want no proxy and a fresh connection per request", c.Transport)
	}
	resp, err := c.Get(srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || final.Load() != 0 {
		t.Errorf("the client followed a redirect (status %d, %d hits on the target): the key must never travel to another URL", resp.StatusCode, final.Load())
	}
	// Control: a client that does follow redirects reaches the target, so the assertion above can fail.
	follow := &http.Client{Timeout: time.Second}
	resp2, err := follow.Get(srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if final.Load() != 1 {
		t.Errorf("control failed: the default client did not follow the redirect (%d hits on the target)", final.Load())
	}
}

// TestDefaultSuspenderIsTheRealClient pins that, outside tests, the builder produces the m3admin client (and that the
// client's own refusals reach the caller): it is the one assignment of newSuspender the tests above replace.
func TestDefaultSuspenderIsTheRealClient(t *testing.T) {
	cfg, err := config.Load(envOf(base("postgres://platform_app:x@h/db")))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSuspender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := s.(*m3admin.Client)
	if !ok || c == nil {
		t.Fatalf("the default builder returned %T, want *m3admin.Client", s)
	}
	if strings.Contains(c.String(), m3Canary) {
		t.Errorf("the client renders its key: %s", c.String())
	}
}
