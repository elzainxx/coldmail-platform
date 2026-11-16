//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/entitlements"
)

// One loop runner (Phase 2 close-out D9): the expiry and reconcile loops run ONLY in the platform instance that holds the leader advisory lock; a second instance idles its loops and takes over
// when the leader's database session ends. These are the database tests of that rule: TWO real instances (two Services over two real platform_app pools) on one migrated scratch database,
// counting what each one's loops actually RUN (a counting expiry worker and a counting reconcile worker stand in for the store and the reconciler, which the other entitlement tests cover), and a
// superuser connection that reads the lock itself from pg_locks and pg_stat_activity. The same stack with TWO REAL platform containers is harness stage 9c in run.sh. Every name here starts with
// `ldr` so that it cannot clash with another file's helpers.

// ldrKey is the leader lock, written out here and NOT imported: the number is a contract that run.sh stage 9c reads too, and TestLeader_TheKeyIsThePlatformsAndTheStageReadsTheSameNumber fails
// if the code, this file and the stage disagree.
const ldrKey int64 = 7200000002

// ldrTick is the loops' interval in these tests: a standby asks for the lock at this rhythm, so a takeover is expected within a few of them; the deadline the tests give it is 100 times that.
const ldrTick = 100 * time.Millisecond

// ldrDeadline is how long a takeover may take: ten seconds, with a 100 ms tick.
const ldrDeadline = 10 * time.Second

type ldrCounter struct{ expiry, reconcile atomic.Int64 }

// ldrExpirer and ldrReconciler count the passes the loops RUN. A standby must make none.
type ldrExpirer struct{ c *ldrCounter }

func (e ldrExpirer) ExpireDueReport(context.Context, int) (entitlements.ExpireReport, error) {
	e.c.expiry.Add(1)
	return entitlements.ExpireReport{}, nil
}

type ldrReconciler struct{ c *ldrCounter }

func (r ldrReconciler) RunOnce(context.Context) (entitlements.Report, error) {
	r.c.reconcile.Add(1)
	return entitlements.Report{Unconverged: 0}, nil
}

// ldrInstance is one platform instance: its own pool, its own Service and its own counters.
type ldrInstance struct {
	id    string
	pool  *pgxpool.Pool
	svc   *entitlements.Service
	count *ldrCounter
	logs  *entLogs
	stop  func() error // cancels the Service and waits (bounded) for Run to return; safe to call twice
}

// ldrStart builds an instance and starts its Service once gate is closed. election=false builds it WITHOUT the leader election (the control). tick is both loops' interval. Its pool is closed by
// the cleanup of the pool, which runs AFTER the cleanup that stops the Service (cleanups run last-in first-out), so a connection the Service still held would hang the pool's Close: the
// tests prove it does not.
func ldrStart(t *testing.T, dsn, id string, election bool, tick time.Duration, gate <-chan struct{}) *ldrInstance {
	t.Helper()
	in := &ldrInstance{id: id, count: &ldrCounter{}, logs: &entLogs{}}
	in.pool = entPool(t, dsn, "ldr-pool-"+id)
	cfg := entitlements.Config{
		Pool: in.pool, M3: &entFake{}, InstanceID: id, ExpireEvery: tick, ReconcileEvery: tick,
		Log: slog.New(slog.NewTextHandler(in.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	opts := []entitlements.Option{entitlements.WithWorkers(ldrExpirer{in.count}, ldrReconciler{in.count})}
	if !election {
		opts = append(opts, entitlements.WithoutLeaderElection())
	}
	svc, err := entitlements.New(cfg, opts...)
	if err != nil {
		t.Fatalf("build the service of %s: %v", id, err)
	}
	in.svc = svc
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		select {
		case <-gate:
		case <-ctx.Done(): // the test ended (or failed) before the gate opened: nothing was started, so there is nothing to wait for
			done <- nil
			return
		}
		done <- svc.Run(ctx)
	}()
	var once sync.Once
	var result error
	in.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(30 * time.Second):
				result = errors.New("Run did not return within 30 s of the cancel")
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := in.stop(); err != nil {
			t.Errorf("stopping %s: %v", id, err)
		}
	})
	return in
}

// ldrOpen is a gate that is already open.
func ldrOpen() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// ldrActors is THE checker of "how many instances run passes": the ids of the instances whose loops have run at least one pass. With the election on it must say one; with the election removed (the
// control) it must say two, which is what shows that the checker can see a double runner.
func ldrActors(ins ...*ldrInstance) []string {
	var out []string
	for _, in := range ins {
		if in.count.expiry.Load() > 0 || in.count.reconcile.Load() > 0 {
			out = append(out, in.id)
		}
	}
	return out
}

// ldrWait polls cond every 20 ms until it holds or `within` has passed, and fails the test naming what it waited for.
func ldrWait(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ldrFirstRunner waits until some instance has run five passes of each loop and returns it.
func ldrFirstRunner(t *testing.T, ins ...*ldrInstance) *ldrInstance {
	t.Helper()
	var first *ldrInstance
	ldrWait(t, "an instance to run five passes of each loop", 20*time.Second, func() bool {
		for _, in := range ins {
			if in.count.expiry.Load() >= 5 && in.count.reconcile.Load() >= 5 {
				first = in
				return true
			}
		}
		return false
	})
	return first
}

// ldrHolder is one row of the lock in the server's own view.
type ldrHolder struct {
	PID     int
	App     string
	Granted bool
}

// ldrHolders reads, as the superuser, every lock on the leader key in the scratch database the connection is on, with the application_name of the session that holds it. advisory locks are
// per database, so the filter on the database keeps another test's scratch database out of the answer.
func ldrHolders(t *testing.T, admin *pgx.Conn) []ldrHolder {
	t.Helper()
	rows, err := admin.Query(testCtx(t), `SELECT l.pid, COALESCE(a.application_name, ''), l.granted
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objsubid = 1
		  AND l.database = (SELECT d.oid FROM pg_database d WHERE d.datname = current_database())
		  AND ((l.classid::bigint << 32) | l.objid::bigint) = $1::bigint
		ORDER BY l.pid`, ldrKey)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	defer rows.Close()
	var out []ldrHolder
	for rows.Next() {
		var h ldrHolder
		if err := rows.Scan(&h.PID, &h.App, &h.Granted); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ldrWaitHolder waits until exactly one session holds the leader lock, granted, under this application_name, and returns it.
func ldrWaitHolder(t *testing.T, admin *pgx.Conn, app string, within time.Duration) ldrHolder {
	t.Helper()
	var last []ldrHolder
	var found ldrHolder
	ldrWait(t, "exactly one granted holder of the leader lock named "+app, within, func() bool {
		last = ldrHolders(t, admin)
		if len(last) == 1 && last[0].Granted && last[0].App == app {
			found = last[0]
			return true
		}
		return false
	})
	_ = last
	return found
}

// ldrWaitNoHolder waits until nobody holds the leader lock.
func ldrWaitNoHolder(t *testing.T, admin *pgx.Conn, within time.Duration) {
	t.Helper()
	ldrWait(t, "the leader lock to be free", within, func() bool { return len(ldrHolders(t, admin)) == 0 })
}

func ldrTerminate(t *testing.T, admin *pgx.Conn, pid int) {
	t.Helper()
	var ok bool
	if err := admin.QueryRow(testCtx(t), "SELECT pg_terminate_backend($1)", pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v: the leader's session was not ended", pid, ok, err)
	}
}

// ---- the proofs ------------------------------------------------------------------------------------------------------------------------------------------------------

// TestLeader_ExactlyOneInstanceRunsThePasses: two instances started in the same instant on one database. Exactly one runs the passes of its two loops; the other runs none; the server shows
// exactly one granted lock on the leader key and its holder's application_name is the leader's instance id. (The fault proof `loops-leader` removes the election: this test must go red.)
func TestLeader_ExactlyOneInstanceRunsThePasses(t *testing.T) {
	s, admin := migratedScratch(t)
	gate := make(chan struct{})
	a := ldrStart(t, s.DSN, "leader-test-a", true, ldrTick, gate)
	b := ldrStart(t, s.DSN, "leader-test-b", true, ldrTick, gate)
	close(gate)

	lead := ldrFirstRunner(t, a, b)
	time.Sleep(1500 * time.Millisecond) // fifteen more ticks of each loop, in which a second runner would show itself
	actors := ldrActors(a, b)
	if len(actors) != 1 {
		t.Fatalf("DOUBLE RUNNER: %d instances ran passes (%v; a ran %d expiry and %d reconcile, b ran %d and %d), want exactly one", len(actors), actors,
			a.count.expiry.Load(), a.count.reconcile.Load(), b.count.expiry.Load(), b.count.reconcile.Load())
	}
	other := a
	if lead == a {
		other = b
	}
	if actors[0] != lead.id {
		t.Fatalf("the instance that ran passes is %s but the first runner was %s", actors[0], lead.id)
	}
	if other.count.expiry.Load() != 0 || other.count.reconcile.Load() != 0 {
		t.Fatalf("the standby %s ran %d expiry and %d reconcile passes", other.id, other.count.expiry.Load(), other.count.reconcile.Load())
	}
	if lead.count.expiry.Load() < 10 || lead.count.reconcile.Load() < 10 {
		t.Errorf("the leader ran only %d expiry and %d reconcile passes in about two seconds of 100 ms ticks", lead.count.expiry.Load(), lead.count.reconcile.Load())
	}

	// The server's own view: exactly one granted lock on the leader key, and the session that holds it is named after the leader.
	holders := ldrHolders(t, admin)
	if len(holders) != 1 || !holders[0].Granted || holders[0].App != lead.id {
		t.Fatalf("pg_locks shows %+v on the leader key, want exactly one granted lock held by a session named %q", holders, lead.id)
	}

	// The instances' own account of themselves agrees.
	if h := lead.svc.Health(); !h.Leader || h.Standby || h.LeaderSince.IsZero() || h.LastExpiry.IsZero() {
		t.Errorf("the leader's health = %+v, want leader, not standby, a date, and passes recorded", h)
	}
	if h := other.svc.Health(); h.Leader || !h.Standby || !h.LeaderSince.IsZero() || !h.LastExpiry.IsZero() || !h.LastReconcile.IsZero() {
		t.Errorf("the standby's health = %+v, want standby, not leader, and no pass ever recorded", h)
	}
	if n := lead.pool.Stat().AcquiredConns(); n != 1 {
		t.Errorf("the leader's pool has %d connections checked out, want exactly 1 (the dedicated session that holds the lock)", n)
	}
	if !strings.Contains(lead.logs.String(), "loops: this instance is now the leader") {
		t.Errorf("the leader never logged that it is the leader:\n%s", lead.logs.String())
	}
	if strings.Contains(other.logs.String(), "now the leader") || !strings.Contains(other.logs.String(), "another instance holds the leader lock") {
		t.Errorf("the standby's log is wrong:\n%s", other.logs.String())
	}

	// Paired control (the same checker, the same database): see TestLeader_ControlWithoutElectionBothInstancesRunThePasses.
}

// TestLeader_ControlWithoutElectionBothInstancesRunThePasses is the control: with the leader election removed, the same two instances on the same database BOTH run passes, the same checker
// reports two actors, and nobody holds the leader lock. It is what shows that the test above can see a double runner.
func TestLeader_ControlWithoutElectionBothInstancesRunThePasses(t *testing.T) {
	s, admin := migratedScratch(t)
	gate := make(chan struct{})
	a := ldrStart(t, s.DSN, "control-a", false, ldrTick, gate)
	b := ldrStart(t, s.DSN, "control-b", false, ldrTick, gate)
	close(gate)
	ldrWait(t, "both uninhibited instances to run five passes of each loop", 20*time.Second, func() bool {
		return a.count.expiry.Load() >= 5 && a.count.reconcile.Load() >= 5 && b.count.expiry.Load() >= 5 && b.count.reconcile.Load() >= 5
	})
	if actors := ldrActors(a, b); len(actors) != 2 {
		t.Fatalf("the checker reports %d actors (%v) for two instances with no election, want 2: the checker cannot see a double runner", len(actors), actors)
	}
	if h := ldrHolders(t, admin); len(h) != 0 {
		t.Errorf("without the election someone holds the leader lock: %+v", h)
	}
	if h := a.svc.Health(); h.Leader || h.Standby {
		t.Errorf("an instance with no election reports a role: %+v", h)
	}
	for _, in := range []*ldrInstance{a, b} {
		if n := in.pool.Stat().AcquiredConns(); n != 0 {
			t.Errorf("%s holds %d connections without an election", in.id, n)
		}
	}
}

// TestLeader_AStandbyTakesOverWhenTheLeadersSessionIsTerminated simulates a crash: the superuser terminates the leader's backend (pg_terminate_backend), which is what the server does when the leader
// process dies. The standby takes the lock within the deadline (ten seconds, with a 100 ms tick), exactly one holder remains, the new holder is named after the standby, the standby's loops
// start running passes, and the old leader notices, becomes a standby and runs no pass after that.
func TestLeader_AStandbyTakesOverWhenTheLeadersSessionIsTerminated(t *testing.T) {
	s, admin := migratedScratch(t)
	// The first instance ticks slowly (once a second) and is started alone, so it is the leader and, when its session is terminated, it cannot ask for the lock again before the standby (which
	// ticks every 100 ms) has taken it: the outcome does not depend on a scheduler race between the two.
	old := ldrStart(t, s.DSN, "crash-old", true, time.Second, ldrOpen())
	ldrWaitHolder(t, admin, old.id, ldrDeadline)
	ldrWait(t, "the old leader's first passes", 10*time.Second, func() bool { return old.count.expiry.Load() >= 1 && old.count.reconcile.Load() >= 1 })
	standby := ldrStart(t, s.DSN, "crash-new", true, ldrTick, ldrOpen())
	time.Sleep(700 * time.Millisecond) // seven ticks of the standby
	if standby.count.expiry.Load() != 0 || standby.count.reconcile.Load() != 0 {
		t.Fatalf("the standby ran %d expiry and %d reconcile passes while the leader lived", standby.count.expiry.Load(), standby.count.reconcile.Load())
	}
	h := ldrHolders(t, admin)
	if len(h) != 1 || h[0].App != old.id {
		t.Fatalf("before the crash pg_locks shows %+v, want one lock held by %s", h, old.id)
	}
	oldPID := h[0].PID

	// The crash.
	ldrTerminate(t, admin, oldPID)
	t0 := time.Now()
	taken := ldrWaitHolder(t, admin, standby.id, ldrDeadline)
	took := time.Since(t0)
	if taken.PID == oldPID {
		t.Fatalf("the lock is held by the terminated session %d", oldPID)
	}
	t.Logf("takeover: the terminated leader's session %d freed the lock and the standby held it on session %d after %s (deadline %s, tick %s)", oldPID, taken.PID, took.Round(time.Millisecond), ldrDeadline, ldrTick)
	if took > ldrDeadline {
		t.Errorf("the takeover took %s, deadline %s", took, ldrDeadline)
	}
	if n := len(ldrHolders(t, admin)); n != 1 {
		t.Errorf("%d sessions hold the leader lock after the takeover, want exactly one", n)
	}
	ldrWait(t, "the new leader to run five passes of each loop", ldrDeadline, func() bool { return standby.count.expiry.Load() >= 5 && standby.count.reconcile.Load() >= 5 })
	if h := standby.svc.Health(); !h.Leader || h.Standby {
		t.Errorf("the new leader's health = %+v", h)
	}

	// The old leader notices at its next tick (one second), drops the leadership once, at warn, and idles; it runs no pass from then on.
	ldrWait(t, "the old leader to notice that it lost the lock", 10*time.Second, func() bool { h := old.svc.Health(); return h.Standby && !h.Leader })
	e, r := old.count.expiry.Load(), old.count.reconcile.Load()
	time.Sleep(2500 * time.Millisecond) // two or three ticks of the old leader
	if old.count.expiry.Load() != e || old.count.reconcile.Load() != r {
		t.Errorf("the old leader went on running passes after losing the lock: expiry %d -> %d, reconcile %d -> %d", e, old.count.expiry.Load(), r, old.count.reconcile.Load())
	}
	if n := strings.Count(old.logs.String(), "lost the leader lock"); n != 1 {
		t.Errorf("the old leader logged its loss %d times, want once:\n%s", n, old.logs.String())
	}
	if holders := ldrHolders(t, admin); len(holders) != 1 || holders[0].App != standby.id {
		t.Errorf("at the end pg_locks shows %+v, want one lock held by %s", holders, standby.id)
	}
}

// TestLeader_ASingleInstanceRecoversFromALostSession: with no other instance, a leader whose session is terminated drops the leadership (once, at warn), tries again at its next tick and leads
// again on a NEW session; its passes go on.
func TestLeader_ASingleInstanceRecoversFromALostSession(t *testing.T) {
	s, admin := migratedScratch(t)
	solo := ldrStart(t, s.DSN, "solo", true, ldrTick, ldrOpen())
	before := ldrWaitHolder(t, admin, solo.id, ldrDeadline)
	since := func() time.Time { return solo.svc.Health().LeaderSince }
	ldrWait(t, "the first passes", ldrDeadline, func() bool { return solo.count.expiry.Load() >= 3 })
	firstSince := since()
	if firstSince.IsZero() {
		t.Fatal("the leader reports no date for its leadership")
	}

	ldrTerminate(t, admin, before.PID)
	ldrWait(t, "a NEW session to hold the lock under the same name", ldrDeadline, func() bool {
		h := ldrHolders(t, admin)
		return len(h) == 1 && h[0].Granted && h[0].App == solo.id && h[0].PID != before.PID
	})
	ldrWait(t, "the leadership to be dated again", ldrDeadline, func() bool { return since().After(firstSince) })
	e := solo.count.expiry.Load()
	ldrWait(t, "passes to go on after the recovery", ldrDeadline, func() bool { return solo.count.expiry.Load() >= e+5 })
	if n := strings.Count(solo.logs.String(), "lost the leader lock"); n != 1 {
		t.Errorf("the loss was logged %d times, want once:\n%s", n, solo.logs.String())
	}
	if n := strings.Count(solo.logs.String(), "loops: this instance is now the leader"); n != 2 {
		t.Errorf("the leadership was announced %d times, want twice (the start and the recovery):\n%s", n, solo.logs.String())
	}
	if n := solo.pool.Stat().AcquiredConns(); n != 1 {
		t.Errorf("after the recovery the pool has %d connections checked out, want 1 (the dropped session's slot was freed)", n)
	}
}

// TestLeader_RacingAtStartupNeverBothLead: two instances ask for the lock in the same instant, twenty times over, behind a barrier. Each round exactly one is told it leads, the server shows exactly
// one granted lock held by the session named after the winner, and repeating the question does not stack the lock (one release frees it). The loser never leads.
func TestLeader_RacingAtStartupNeverBothLead(t *testing.T) {
	s, admin := migratedScratch(t)
	poolA, poolB := entPool(t, s.DSN, "ldr-race-a"), entPool(t, s.DSN, "ldr-race-b")
	ctx := testCtx(t)
	wins := map[string]int{}
	for round := 0; round < 20; round++ {
		la, err := entitlements.NewLeader(poolA, "race-a", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		lb, err := entitlements.NewLeader(poolB, "race-b", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var barrier, done sync.WaitGroup
		barrier.Add(1)
		answers := make([]bool, 2)
		for i, l := range []*entitlements.Leader{la, lb} {
			done.Add(1)
			go func() {
				defer done.Done()
				barrier.Wait() // both start in the same instant
				answers[i] = l.Ensure(ctx)
			}()
		}
		barrier.Done()
		done.Wait()
		if answers[0] == answers[1] {
			t.Fatalf("round %d: the two instances were told %v and %v, want exactly one leader", round, answers[0], answers[1])
		}
		winner, loser, winName := la, lb, "race-a"
		if answers[1] {
			winner, loser, winName = lb, la, "race-b"
		}
		wins[winName]++
		if h := ldrHolders(t, admin); len(h) != 1 || !h[0].Granted || h[0].App != winName {
			t.Fatalf("round %d: pg_locks shows %+v, want exactly one granted lock held by %s", round, h, winName)
		}
		for i := 0; i < 5; i++ { // asking again neither moves the lock nor stacks it
			if !winner.Ensure(ctx) || loser.Ensure(ctx) {
				t.Fatalf("round %d, ask %d: the leadership moved while the leader's session was intact", round, i)
			}
		}
		if st := loser.Status(); st.Leading || !st.Decided {
			t.Fatalf("round %d: the loser's status is %+v", round, st)
		}
		if err := winner.Release(ctx); err != nil {
			t.Fatalf("round %d: release: %v", round, err)
		}
		if err := loser.Release(ctx); err != nil {
			t.Fatalf("round %d: the loser's release (nothing held): %v", round, err)
		}
		// ONE release freed it: had the question stacked the lock, it would still be held.
		if h := ldrHolders(t, admin); len(h) != 0 {
			t.Fatalf("round %d: after the winner's release pg_locks still shows %+v (the lock was taken more than once on one session)", round, h)
		}
		for _, p := range []*pgxpool.Pool{poolA, poolB} {
			if n := p.Stat().AcquiredConns(); n != 0 {
				t.Fatalf("round %d: a pool still has %d connections checked out after the releases", round, n)
			}
		}
	}
	t.Logf("20 races, each with exactly one leader: wins %v", wins)
}

// TestLeader_AStoppedLeaderHandsOverAtOnceAndReturnsItsConnection: shutdown is explicit. When the leader's Service is stopped (the context cancelled) it unlocks and returns its dedicated
// connection to the pool BEFORE Run returns: its pool has nothing checked out, the pool closes (it would wait for ever for a connection that was never returned), and the standby takes the lock
// at its next tick, not when a socket is noticed to be closed.
func TestLeader_AStoppedLeaderHandsOverAtOnceAndReturnsItsConnection(t *testing.T) {
	s, admin := migratedScratch(t)
	a := ldrStart(t, s.DSN, "stop-a", true, ldrTick, ldrOpen())
	ldrWaitHolder(t, admin, a.id, ldrDeadline)
	b := ldrStart(t, s.DSN, "stop-b", true, ldrTick, ldrOpen())
	ldrWait(t, "the leader's first passes", ldrDeadline, func() bool { return a.count.expiry.Load() >= 3 })
	if n := a.pool.Stat().AcquiredConns(); n != 1 {
		t.Fatalf("the leader's pool has %d connections checked out before the stop, want the one dedicated session", n)
	}

	t0 := time.Now()
	if err := a.stop(); err != nil {
		t.Fatalf("stopping the leader: %v", err)
	}
	if n := a.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("Run returned with %d connections still checked out: the pool would never close", n)
	}
	closed := make(chan struct{})
	go func() { a.pool.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader's pool did not close within 10 s of Run returning: a connection was never given back")
	}
	h := ldrWaitHolder(t, admin, b.id, ldrDeadline)
	t.Logf("hand-over: the stopped leader's pool closed and the standby held the lock on session %d, %s after the stop began", h.PID, time.Since(t0).Round(time.Millisecond))
	if !strings.Contains(a.logs.String(), "loops: this instance released the leader lock") {
		t.Errorf("the stopped leader did not log its release:\n%s", a.logs.String())
	}
	ldrWait(t, "the new leader's passes", ldrDeadline, func() bool { return b.count.expiry.Load() >= 5 && b.count.reconcile.Load() >= 5 })
	// A standby that is stopped holds nothing and the pool closes at once too.
	if err := b.stop(); err != nil {
		t.Fatalf("stopping the second instance: %v", err)
	}
	ldrWaitNoHolder(t, admin, ldrDeadline)
}

// TestLeader_TheKeyIsThePlatformsAndTheStageReadsTheSameNumber: the key is in the platform band, differs from the migrator's, equals the one this file and run.sh stage 9c read from pg_locks, and the
// compose file runs two platform instances that name themselves.
func TestLeader_TheKeyIsThePlatformsAndTheStageReadsTheSameNumber(t *testing.T) {
	if entitlements.LeaderLockKey != ldrKey {
		t.Fatalf("entitlements.LeaderLockKey = %d, this file reads %d: change them together", entitlements.LeaderLockKey, ldrKey)
	}
	if ldrKey < 7_000_000_000 || ldrKey > 7_999_999_999 {
		t.Errorf("the leader key %d is outside the platform band 7_xxx_xxx_xxx (CLAUDE.md rule 9)", ldrKey)
	}
	if ldrKey == migratorLockKey {
		t.Errorf("the leader key equals the migrator's lock %d", migratorLockKey)
	}
	root := requireEnv(t, envRepoRoot)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || len(b) == 0 {
			t.Fatalf("cannot read %s (%v): a test that cannot run fails", rel, err)
		}
		return strings.ReplaceAll(string(b), "\r", "")
	}
	run := read("tests/harness/run.sh")
	if !strings.Contains(run, fmt.Sprintf("LEADER_KEY=%d", ldrKey)) {
		t.Errorf("tests/harness/run.sh does not read the leader key %d (stage 9c: LEADER_KEY=%d)", ldrKey, ldrKey)
	}
	compose := read("tests/harness/docker-compose.yml")
	for _, want := range []string{"\n  platform-2:", "PLATFORM_INSTANCE_ID: platform-1", "PLATFORM_INSTANCE_ID: platform-2"} {
		if !strings.Contains(compose, want) {
			t.Errorf("tests/harness/docker-compose.yml lacks %q: the stack must run two platform instances that name themselves", strings.TrimSpace(want))
		}
	}
	if strings.Count(compose, "PLATFORM_INSTANCE_ID:") != 2 {
		t.Errorf("tests/harness/docker-compose.yml sets PLATFORM_INSTANCE_ID %d times, want exactly twice (one per platform instance)", strings.Count(compose, "PLATFORM_INSTANCE_ID:"))
	}
}
