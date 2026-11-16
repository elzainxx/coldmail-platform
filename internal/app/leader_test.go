package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
)

// The service builds its entitlement loops WITH leader election (Phase 2 close-out D9): the loops run only in the instance that holds the leader advisory lock. No database is needed here: the pool
// points at a closed port, where no instance can ever take the lock, which is exactly the case in which the loops must idle and the server must not notice. What two real instances do against a real
// PostgreSQL is proved by tests/harness (TestLeader_*) and by the harness stage 9c.

// setElection replaces entitlementOptions for the rest of a test and returns what restores it. on=false builds the loops WITHOUT the election (the test-only option), on=true is the wiring the service has.
func setElection(on bool) (restore func()) {
	old := entitlementOptions
	if !on {
		entitlementOptions = func() []entitlements.Option { return []entitlements.Option{entitlements.WithoutLeaderElection()} }
	}
	return func() { entitlementOptions = old }
}

// withoutElection switches the election off for the whole of one test: for the tests that are about the loops' own passes against a database that is not there.
func withoutElection(t *testing.T) {
	t.Helper()
	t.Cleanup(setElection(false))
}

func TestRun_AStandbyInstanceRunsNoPassAndKeepsServing(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	env := merge(base(closedPortDSN(t)), map[string]string{
		config.VarLogLevel:                     "debug",
		config.VarEntitlementExpireIntervalSec: "1",
		config.VarM3ReconcileIntervalSec:       "1",
	})

	// The wiring the service has: the election is on, and nobody can take the lock here.
	r := startApp(t, env)
	const tried = "loops: the leader lock cannot be tried; this instance idles its loops and tries again at the next tick"
	waitUntil(t, "the election to try the lock and fail", func() bool { return find(r.logs.lines(t), tried) != nil })
	time.Sleep(3500 * time.Millisecond) // about three more ticks of each loop
	lines := r.logs.lines(t)
	if n, m := countLines(lines, "entitlement expiry pass"), countLines(lines, "entitlement reconcile pass"); n != 0 || m != 0 {
		t.Errorf("a standby made %d expiry and %d reconcile passes: with no leader lock the loops must do nothing (not even fail a pass)", n, m)
	}
	// The failure of the election is reported once as a class, not once per tick, and never with the driver's text.
	if n := countLines(lines, tried); n != 1 {
		t.Errorf("the election's failure was logged %d times in about four ticks of two loops, want exactly once", n)
	}
	if l := find(lines, tried); l == nil || l["reason"] != "connection refused" || l["instance"] == nil || l["instance"] == "" {
		t.Errorf("the election's warning = %v, want the class \"connection refused\" and the instance's name", l)
	}
	// The loops announced themselves (they are running, idle) and the server did not notice.
	if find(lines, "entitlement expiry loop started") == nil || find(lines, "entitlement reconcile loop started") == nil {
		t.Errorf("the loops did not start:\n%s", r.logs.String())
	}
	if status, _, _ := fetch(t, "http://"+r.addr+"/healthz"); status != http.StatusOK {
		t.Errorf("healthz = %d while this instance is a standby: serving HTTP does not depend on leading", status)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("the service returned %v after the cancel, want a clean drain", err)
	}
	for _, leak := range []string{canary, "platform_app", m3Canary} {
		if strings.Contains(r.logs.String(), leak) {
			t.Errorf("the log carries %q:\n%s", leak, r.logs.String())
		}
	}

	// Paired control: the same environment WITHOUT the election makes its passes at once (and they fail, against the closed port), so the silence above is the election's doing, not a quiet loop.
	restore := setElection(false)
	defer restore()
	c := startApp(t, env)
	waitUntil(t, "passes of both loops without the election", func() bool {
		l := c.logs.lines(t)
		return countLines(l, "entitlement expiry pass") >= 1 && countLines(l, "entitlement reconcile pass") >= 1
	})
	if err := c.stop(t); err != nil {
		t.Fatal(err)
	}
	if find(c.logs.lines(t), tried) != nil {
		t.Errorf("control failed: the service built without the election still asked for the lock:\n%s", c.logs.String())
	}
}

func TestRun_TheInstanceIDDialReachesTheStartupLineAndIsValidated(t *testing.T) {
	withSuspender(t, &fakeSuspender{}, nil)
	named := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarInstanceID: "platform-7"}))
	if err := named.stop(t); err != nil {
		t.Fatal(err)
	}
	if l := find(named.logs.lines(t), "platform starting"); l == nil || l["instance"] != "platform-7" {
		t.Errorf("the startup line = %v, want instance platform-7", l)
	}
	// Control: with the variable unset the instance is the host name (what the service resolves), so the line above names the dial's value and not a constant.
	unset := startApp(t, base(closedPortDSN(t)))
	if err := unset.stop(t); err != nil {
		t.Fatal(err)
	}
	want, err := entitlements.ResolveInstanceID("")
	if err != nil || want == "" || want == "platform-7" {
		t.Fatalf("the default instance name is %q (%v): the control needs a different name", want, err)
	}
	if l := find(unset.logs.lines(t), "platform starting"); l == nil || l["instance"] != want {
		t.Errorf("the startup line = %v, want the default instance %q", l, want)
	}

	// A name that cannot be an application_name stops the process at start-up with exit status 2, naming the variable and not echoing the value.
	logs := &syncBuffer{}
	tooLong := strings.Repeat("x", 64)
	err = Run(t.Context(), envOf(merge(base(closedPortDSN(t)), map[string]string{config.VarInstanceID: tooLong})), logs)
	if err == nil || ExitCode(err) != ExitConfig || !strings.Contains(err.Error(), config.VarInstanceID) {
		t.Fatalf("Run with a 64-byte instance name = %v (exit %d), want a configuration error naming %s", err, ExitCode(err), config.VarInstanceID)
	}
	if strings.Contains(err.Error(), tooLong) {
		t.Errorf("the error echoes the rejected value: %v", err)
	}
	if logs.String() != "" {
		t.Errorf("a process that could not read its configuration wrote logs: %s", logs.String())
	}
}
