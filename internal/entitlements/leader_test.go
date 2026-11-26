package entitlements

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Leader election without a database (Phase 2 close-out D9). The lock the instances contend for is a FAKE here, a table that has at most one holder, so that what is under test is the state machine
// of the election and what the Service does with its answer: acquire, lose, re-acquire, release on shutdown, and a standby that never calls a pass. The real lock, taken on a real connection, is
// proved by tests/harness (TestLeader_*) against a real PostgreSQL.

// lockTable stands for the database's lock: it has at most one holder, named by its taker.
type lockTable struct {
	mu     sync.Mutex
	holder string
}

func (t *lockTable) set(h string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.holder = h
}

func (t *lockTable) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.holder
}

// fakeTaker is a lockTaker over a lockTable: one session that holds the lock or does not.
type fakeTaker struct {
	name  string
	table *lockTable

	// guarded by table.mu:
	has        bool
	tryErr     error
	heldErr    error
	releaseErr error
	panics     int // the next this many TryAcquire calls panic

	tries, checks, releases atomic.Int64
}

func newFakeTaker(name string, table *lockTable) *fakeTaker {
	return &fakeTaker{name: name, table: table}
}

func (f *fakeTaker) TryAcquire(context.Context) (bool, error) {
	f.tries.Add(1)
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	if f.panics > 0 {
		f.panics--
		panic("the lock taker exploded")
	}
	if f.tryErr != nil {
		return false, f.tryErr
	}
	if f.table.holder == "" {
		f.table.holder, f.has, f.heldErr = f.name, true, nil // a new session: it works
		return true, nil
	}
	return false, nil
}

func (f *fakeTaker) StillHeld(context.Context) (bool, error) {
	f.checks.Add(1)
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	if !f.has {
		return false, nil
	}
	if f.heldErr != nil {
		f.has = false // the taker drops a session that failed
		return false, f.heldErr
	}
	if f.table.holder != f.name {
		f.has = false
		return false, nil
	}
	return true, nil
}

func (f *fakeTaker) Release(context.Context) error {
	f.releases.Add(1)
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	if f.has {
		if f.table.holder == f.name {
			f.table.holder = ""
		}
		f.has = false
	}
	return f.releaseErr
}

// crash is the leader's session being terminated: the server frees the lock at once and the next use of the session fails the way a terminated backend does (57P01).
func (f *fakeTaker) crash() {
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	if f.table.holder == f.name {
		f.table.holder = ""
	}
	f.heldErr = &pgconn.PgError{Code: "57P01"}
}

func (f *fakeTaker) setTryErr(err error) {
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	f.tryErr = err
}

func (f *fakeTaker) setPanics(n int) {
	f.table.mu.Lock()
	defer f.table.mu.Unlock()
	f.panics = n
}

var leaderClock = time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)

func fixedClock() time.Time { return leaderClock }

// leaderService builds a Service over a leader with 5 ms loops.
func leaderService(t *testing.T, ex expirer, rec reconciler, l *Leader) *Service {
	t.Helper()
	s, err := newServiceLeader(Config{Log: debugLogger(&lockedBuf{}), ExpireEvery: 5 * time.Millisecond, ReconcileEvery: 5 * time.Millisecond}, ex, rec, l)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func count(buf *lockedBuf, needle string) int { return strings.Count(buf.String(), needle) }

func TestLeader_TheStateMachine(t *testing.T) {
	table := &lockTable{holder: "other"}
	f := newFakeTaker("me", table)
	buf := &lockedBuf{}
	l := newLeader(f, "me", debugLogger(buf), fixedClock)
	ctx := context.Background()

	if st := l.Status(); st.Decided || st.Leading {
		t.Fatalf("before the first Ensure the status is %+v, want nothing decided", st)
	}

	// 1. Another instance holds the lock: a standby asks at every tick, is told no, and says so once.
	for i := 0; i < 3; i++ {
		if l.Ensure(ctx) {
			t.Fatalf("Ensure %d answered true while another instance holds the lock", i)
		}
	}
	if st := l.Status(); !st.Decided || st.Leading || !st.Since.IsZero() {
		t.Errorf("a standby's status is %+v, want decided, not leading, no date", st)
	}
	if n := f.tries.Load(); n != 3 {
		t.Errorf("three standby ticks asked for the lock %d times, want 3", n)
	}
	if n := count(buf, "another instance holds the leader lock"); n != 1 {
		t.Errorf("the standby was announced %d times in three ticks, want once:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("being a standby is not a warning:\n%s", buf.String())
	}

	// 2. The lock frees: the next tick takes it, and says so.
	table.set("")
	if !l.Ensure(ctx) {
		t.Fatal("Ensure answered false with the lock free")
	}
	if st := l.Status(); !st.Leading || !st.Since.Equal(leaderClock) {
		t.Errorf("the new leader's status is %+v, want leading since %v", st, leaderClock)
	}
	if n := count(buf, "loops: this instance is now the leader"); n != 1 {
		t.Errorf("the takeover was logged %d times, want once:\n%s", n, buf.String())
	}
	if table.get() != "me" {
		t.Errorf("the lock is held by %q, want me", table.get())
	}

	// 3. A leader CHECKS its session at every tick and never takes the lock a second time (a session lock stacks, and one unlock would then not free it).
	for i := 0; i < 3; i++ {
		if !l.Ensure(ctx) {
			t.Fatalf("a leader's tick %d answered false with its session intact", i)
		}
	}
	if f.tries.Load() != 4 || f.checks.Load() != 3 {
		t.Errorf("tries = %d, checks = %d, want 4 tries in all (3 standby, 1 taken) and 3 checks", f.tries.Load(), f.checks.Load())
	}
	if n := count(buf, "loops: this instance is now the leader"); n != 1 {
		t.Errorf("three more leader ticks logged the takeover again (%d in all)", n)
	}

	// 4. The session is terminated: the leadership is dropped at once, once, at warn, and the next tick takes the lock again.
	f.crash()
	if l.Ensure(ctx) {
		t.Fatal("Ensure answered true after the leader's session was terminated")
	}
	if st := l.Status(); st.Leading || !st.Since.IsZero() || !st.Decided {
		t.Errorf("after the loss the status is %+v, want not leading, no date", st)
	}
	if n := count(buf, "lost the leader lock"); n != 1 || !strings.Contains(buf.String(), "database error 57P01") {
		t.Errorf("the loss was logged %d times (want once, with its class):\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "terminating connection") {
		t.Errorf("the log carries the driver's text, not the failure class:\n%s", buf.String())
	}
	if !l.Ensure(ctx) {
		t.Fatal("the tick after a loss did not take the free lock again")
	}
	if n := count(buf, "loops: this instance is now the leader"); n != 2 || f.tries.Load() != 5 {
		t.Errorf("after the re-acquisition: %d takeover lines (want 2), %d tries (want 5)", n, f.tries.Load())
	}

	// 5. The session answers but the lock is not its own any more (another holder): a loss too, and the stretch of standby that follows is announced again.
	table.set("other")
	if l.Ensure(ctx) {
		t.Fatal("Ensure answered true although the lock belongs to another holder")
	}
	if n := count(buf, "lost the leader lock"); n != 2 || !strings.Contains(buf.String(), "no longer held") {
		t.Errorf("the second loss was logged %d times in all (want 2), or without its reason:\n%s", n, buf.String())
	}
	if l.Ensure(ctx) {
		t.Fatal("a standby answered true")
	}
	if n := count(buf, "another instance holds the leader lock"); n != 2 {
		t.Errorf("the second stretch of standby was announced %d times in all, want 2", n)
	}

	// 6. Shutdown: the leader releases the lock explicitly and the status says so.
	table.set("")
	if !l.Ensure(ctx) {
		t.Fatal("could not lead again before the shutdown")
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release = %v", err)
	}
	if f.releases.Load() != 1 || table.get() != "" {
		t.Errorf("after Release: %d releases (want 1), the lock is held by %q (want nobody)", f.releases.Load(), table.get())
	}
	if st := l.Status(); st.Leading {
		t.Errorf("after Release the status is %+v, want not leading", st)
	}
	if n := count(buf, "released the leader lock"); n != 1 {
		t.Errorf("the release was logged %d times, want once", n)
	}
	// A second Release (the shutdown path may run it twice) is harmless.
	if err := l.Release(ctx); err != nil || count(buf, "released the leader lock") != 1 {
		t.Errorf("a second Release = %v (logged %d times), want a quiet no-op", err, count(buf, "released the leader lock"))
	}
}

func TestLeader_AnOutageIsWarnedOncePerClassNotOncePerTick(t *testing.T) {
	table := &lockTable{}
	f := newFakeTaker("me", table)
	buf := &lockedBuf{}
	l := newLeader(f, "me", debugLogger(buf), fixedClock)
	ctx := context.Background()

	f.setTryErr(&pgconn.PgError{Code: "57P03", Message: "the database system is starting up"})
	for i := 0; i < 6; i++ {
		if l.Ensure(ctx) {
			t.Fatal("Ensure answered true although the lock cannot be tried")
		}
	}
	if n := count(buf, "the leader lock cannot be tried"); n != 1 {
		t.Errorf("six identical failures logged %d warnings, want exactly one:\n%s", n, buf.String())
	}
	f.setTryErr(&pgconn.PgError{Code: "53300", Message: "too many connections"})
	l.Ensure(ctx)
	if n := count(buf, "the leader lock cannot be tried"); n != 2 {
		t.Errorf("a new failure class logged %d warnings in all, want 2", n)
	}
	if !strings.Contains(buf.String(), "database error 57P03") || !strings.Contains(buf.String(), "database error 53300") {
		t.Errorf("the warnings must carry the failure classes:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "starting up") || strings.Contains(buf.String(), "too many connections") {
		t.Errorf("the log carries the driver's text:\n%s", buf.String())
	}
	if st := l.Status(); st.Leading || !st.Decided {
		t.Errorf("status during an outage = %+v, want decided and not leading (a standby idles)", st)
	}
	// The outage ends: the instance leads again, and a NEW outage of the first class warns again.
	f.setTryErr(nil)
	if !l.Ensure(ctx) {
		t.Fatal("the instance did not lead once the lock could be tried again")
	}
	f.crash()
	l.Ensure(ctx) // the loss
	f.setTryErr(&pgconn.PgError{Code: "57P03"})
	l.Ensure(ctx)
	if n := count(buf, "the leader lock cannot be tried"); n != 3 {
		t.Errorf("a second outage of the first class logged %d warnings in all, want 3:\n%s", n, buf.String())
	}
}

func TestLeader_AContextThatIsCancelledAnswersFalseAndTouchesNothing(t *testing.T) {
	table := &lockTable{}
	f := newFakeTaker("me", table)
	buf := &lockedBuf{}
	l := newLeader(f, "me", debugLogger(buf), fixedClock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.Ensure(ctx) {
		t.Error("Ensure answered true for a cancelled context")
	}
	if f.tries.Load() != 0 || buf.String() != "" || table.get() != "" {
		t.Errorf("a cancelled Ensure touched the lock (%d tries, holder %q) or logged: %s", f.tries.Load(), table.get(), buf.String())
	}
	// Control: the same election with a live context takes the lock.
	if !l.Ensure(context.Background()) {
		t.Error("control failed: a live context did not take the free lock")
	}
}

func TestLeader_AFailedReleaseIsLoggedAndTheLeadershipIsGoneAnyway(t *testing.T) {
	table := &lockTable{}
	f := newFakeTaker("me", table)
	buf := &lockedBuf{}
	l := newLeader(f, "me", debugLogger(buf), fixedClock)
	ctx := context.Background()
	if !l.Ensure(ctx) {
		t.Fatal("could not lead")
	}
	f.table.mu.Lock()
	f.releaseErr = &pgconn.PgError{Code: "57P01"}
	f.table.mu.Unlock()
	err := l.Release(ctx)
	if err == nil {
		t.Fatal("Release hid the failure of its taker")
	}
	if st := l.Status(); st.Leading {
		t.Errorf("a failed release left the status %+v: the loops must not go on believing they lead", st)
	}
	if n := count(buf, "releasing the leader lock failed"); n != 1 || !strings.Contains(buf.String(), "database error 57P01") {
		t.Errorf("the failed release was logged %d times (want once, with its class):\n%s", n, buf.String())
	}
}

// Two instances on one lock: at every moment at most one of them leads, however the startup is raced, and the lock is never taken twice.
func TestLeader_RacingForTheFakeLockNeverGrantsItTwice(t *testing.T) {
	for round := 0; round < 20; round++ {
		table := &lockTable{}
		fa, fb := newFakeTaker("a", table), newFakeTaker("b", table)
		la, lb := newLeader(fa, "a", nil, nil), newLeader(fb, "b", nil, nil)

		var barrier, done sync.WaitGroup
		barrier.Add(1)
		answers := make([]bool, 2)
		for i, l := range []*Leader{la, lb} {
			done.Add(1)
			go func() {
				defer done.Done()
				barrier.Wait() // both instances start in the same instant
				answers[i] = l.Ensure(context.Background())
			}()
		}
		barrier.Done()
		done.Wait()
		if answers[0] == answers[1] {
			t.Fatalf("round %d: the two instances answered %v and %v, want exactly one leader", round, answers[0], answers[1])
		}
		winner, loser := la, lb
		if answers[1] {
			winner, loser = lb, la
		}
		if !winner.Status().Leading || loser.Status().Leading {
			t.Fatalf("round %d: statuses %+v and %+v disagree with the answers", round, winner.Status(), loser.Status())
		}
		// Many more rounds of both: the winner keeps it, the loser never gets it.
		for i := 0; i < 20; i++ {
			if !winner.Ensure(context.Background()) || loser.Ensure(context.Background()) {
				t.Fatalf("round %d tick %d: leadership moved while the leader's session was intact", round, i)
			}
		}
		if err := winner.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !loser.Ensure(context.Background()) {
			t.Fatalf("round %d: the standby did not take over the released lock at its next tick", round)
		}
	}
}

// The Service: a standby never calls a pass, a leader calls them, shutdown releases.
func TestLeader_AStandbyServiceRunsNoPassAndTakesOverWhenTheLockFrees(t *testing.T) {
	table := &lockTable{holder: "other"}
	f := newFakeTaker("me", table)
	lbuf := &lockedBuf{}
	l := newLeader(f, "me", debugLogger(lbuf), fixedClock)
	ex, rec := &fakeExpirer{}, &fakeReconciler{}
	s := leaderService(t, ex, rec, l)
	stop := start(t, s)

	waitFor(t, "the first answer of the election", func() bool { return s.Health().Standby })
	time.Sleep(150 * time.Millisecond) // about thirty ticks of each loop
	if ex.calls.Load() != 0 || rec.calls.Load() != 0 {
		t.Fatalf("a standby ran %d expiry and %d reconcile passes: it must do nothing", ex.calls.Load(), rec.calls.Load())
	}
	if f.tries.Load() < 10 {
		t.Errorf("the standby asked for the lock %d times in thirty ticks: it must ask at every tick", f.tries.Load())
	}
	if h := s.Health(); h.Leader || !h.Standby || !h.LeaderSince.IsZero() || !h.LastExpiry.IsZero() || !h.LastReconcile.IsZero() || h.Unconverged != -1 {
		t.Errorf("a standby's health = %+v, want standby, no pass ever recorded, nothing known about the users", h)
	}

	// The other instance stops: this one takes over and its passes begin (the paired control of the silence above).
	table.set("")
	waitFor(t, "three passes of each loop after the takeover", func() bool { return ex.calls.Load() >= 3 && rec.calls.Load() >= 3 })
	if h := s.Health(); !h.Leader || h.Standby || !h.LeaderSince.Equal(leaderClock) {
		t.Errorf("the leader's health = %+v, want leader since %v and not standby", h, leaderClock)
	}
	if n := count(lbuf, "loops: this instance is now the leader"); n != 1 {
		t.Errorf("the two loops racing to the same free lock took it %d times, want once:\n%s", n, lbuf.String())
	}

	// Shutdown: the leader hands the lock back BEFORE Run returns, so a caller that closes the pool next never waits for this connection.
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after the cancel", err)
	}
	if f.releases.Load() != 1 || table.get() != "" {
		t.Errorf("after Run returned: %d releases (want 1) and the lock is held by %q (want nobody)", f.releases.Load(), table.get())
	}
	if h := s.Health(); h.Leader {
		t.Errorf("a stopped service still reports itself the leader: %+v", h)
	}
}

func TestLeader_OnlyOneOfTwoServicesRunsPassesAndTheOtherTakesOverWhenItsSessionDies(t *testing.T) {
	table := &lockTable{}
	fa, fb := newFakeTaker("a", table), newFakeTaker("b", table)
	exA, recA, exB, recB := &fakeExpirer{}, &fakeReconciler{}, &fakeExpirer{}, &fakeReconciler{}
	sa := leaderService(t, exA, recA, newLeader(fa, "a", nil, nil))
	sb := leaderService(t, exB, recB, newLeader(fb, "b", nil, nil))
	start(t, sa)
	start(t, sb)

	waitFor(t, "one of the two to run passes", func() bool { return exA.calls.Load() >= 3 || exB.calls.Load() >= 3 })
	time.Sleep(100 * time.Millisecond)
	lead, other := sa, sb
	leadFake, otherFake := fa, fb
	leadEx, leadRec, otherEx, otherRec := exA, recA, exB, recB
	if exB.calls.Load() > exA.calls.Load() {
		lead, other, leadFake, otherFake = sb, sa, fb, fa
		leadEx, leadRec, otherEx, otherRec = exB, recB, exA, recA
	}
	if otherEx.calls.Load() != 0 || otherRec.calls.Load() != 0 {
		t.Fatalf("the standby ran %d expiry and %d reconcile passes while the other led: two runners", otherEx.calls.Load(), otherRec.calls.Load())
	}
	if leadEx.calls.Load() < 3 || leadRec.calls.Load() < 3 {
		t.Fatalf("the leader ran %d expiry and %d reconcile passes, want both loops running", leadEx.calls.Load(), leadRec.calls.Load())
	}
	if h := lead.Health(); !h.Leader || h.Standby {
		t.Errorf("the leader's health = %+v", h)
	}
	if h := other.Health(); h.Leader || !h.Standby {
		t.Errorf("the standby's health = %+v", h)
	}

	// The leader's session is terminated. The standby takes over at its next tick; the old leader notices at ITS next tick and runs no pass after that. The old instance cannot reach the database
	// again in this test (its tries fail), so the standby is the only candidate and the outcome does not depend on which of them ticks first.
	leadFake.crash()
	leadFake.setTryErr(&pgconn.PgError{Code: "57P03"})
	waitFor(t, "the standby's first passes", func() bool { return otherEx.calls.Load() >= 3 && otherRec.calls.Load() >= 3 })
	time.Sleep(30 * time.Millisecond) // anything the old leader had already started has finished
	e, r := leadEx.calls.Load(), leadRec.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if leadEx.calls.Load() != e || leadRec.calls.Load() != r {
		t.Errorf("the old leader went on running passes after the standby took over: expiry %d -> %d, reconcile %d -> %d", e, leadEx.calls.Load(), r, leadRec.calls.Load())
	}
	if h := other.Health(); !h.Leader || h.Standby {
		t.Errorf("the new leader's health = %+v", h)
	}
	if h := lead.Health(); h.Leader || !h.Standby {
		t.Errorf("the old leader's health = %+v, want standby (it lost its session and the lock is another's)", h)
	}
	if otherFake.checks.Load() == 0 {
		t.Error("the new leader never checked its own session")
	}
}

func TestLeader_APanicInTheElectionDoesNotEndTheLoops(t *testing.T) {
	table := &lockTable{}
	f := newFakeTaker("me", table)
	f.setPanics(3)
	buf := &lockedBuf{}
	ex, rec := &fakeExpirer{}, &fakeReconciler{}
	s, err := newServiceLeader(Config{Log: debugLogger(buf), ExpireEvery: 5 * time.Millisecond, ReconcileEvery: 5 * time.Millisecond}, ex, rec, newLeader(f, "me", debugLogger(buf), fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	start(t, s)
	waitFor(t, "passes once the election stops panicking", func() bool { return ex.calls.Load() >= 3 && rec.calls.Load() >= 3 })
	if f.tries.Load() < 4 {
		t.Errorf("the election was asked %d times, want at least 4 (three panics, then an answer)", f.tries.Load())
	}
	if n := count(buf, "the leader check panicked"); n != 3 || !strings.Contains(buf.String(), "the lock taker exploded") {
		t.Errorf("the panics were logged %d times (want 3):\n%s", n, buf.String())
	}
}

// What Status and Health report must never wait for a database call: Ensure holds the operation lock for the length of one, and a readiness probe that reads Health must not queue behind it.
type gateTaker struct {
	entered chan struct{}
	gate    chan struct{}
}

func (g *gateTaker) TryAcquire(context.Context) (bool, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.gate
	return false, nil
}
func (g *gateTaker) StillHeld(context.Context) (bool, error) { return false, nil }
func (g *gateTaker) Release(context.Context) error           { return nil }

func TestLeader_StatusAndHealthNeverWaitForADatabaseCall(t *testing.T) {
	g := &gateTaker{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	l := newLeader(g, "me", nil, nil)
	s := leaderService(t, &fakeExpirer{}, &fakeReconciler{}, l)
	answered := make(chan bool, 1)
	go func() { answered <- l.Ensure(context.Background()) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Ensure never reached the lock taker")
	}
	read := make(chan struct{})
	go func() {
		_ = l.Status()
		_ = s.Health()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		close(g.gate)
		t.Fatal("Status or Health waited for a database call that is in flight")
	}
	// Control: a second Ensure DOES wait (the taker is not safe for concurrent use), so the immediate answers above are the status lock's doing.
	second := make(chan bool, 1)
	go func() { second <- l.Ensure(context.Background()) }()
	select {
	case <-second:
		t.Error("control failed: a second Ensure did not wait for the one in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(g.gate)
	for i := 0; i < 2; i++ {
		select {
		case <-answered:
		case <-second:
		case <-time.After(5 * time.Second):
			t.Fatal("an Ensure never returned after the gate opened")
		}
	}
}

func TestLeader_TheKeyIsInThePlatformBandAndIsNotAnotherLock(t *testing.T) {
	if LeaderLockKey != 7_200_000_002 {
		t.Fatalf("LeaderLockKey = %d; tests/harness (TestLeader_*) and run.sh stage 9c read exactly 7200000002: change them together", LeaderLockKey)
	}
	if LeaderLockKey < lockBand || LeaderLockKey >= lockBand+lockBandWidth {
		t.Errorf("LeaderLockKey = %d is outside the platform band 7_xxx_xxx_xxx (CLAUDE.md rule 9)", LeaderLockKey)
	}
	const migratorKey int64 = 7_100_000_001 // internal/migrate's advisory lock, written out here and not imported (the packages agree by construction)
	if LeaderLockKey == migratorKey {
		t.Errorf("LeaderLockKey equals the migrator's lock %d", migratorKey)
	}
	// A sweep of per-user keys: none of these equals the leader's (a sample of the one-in-10^9 collision, not a proof that it cannot happen: the doc on LeaderLockKey says what one would do).
	sweep := func(target int64) int {
		for i := 0; i < 100_000; i++ {
			if lockKey(fmt.Sprintf("00000000-0000-4000-8000-%012x", i), moduleM3) == target {
				return i
			}
		}
		return -1
	}
	if i := sweep(LeaderLockKey); i >= 0 {
		t.Fatalf("user %d hashes to the leader lock", i)
	}
	// Control: the same sweep finds a key that IS one of the users' (so a clean answer above means something).
	if i := sweep(lockKey("00000000-0000-4000-8000-00000000030a", moduleM3)); i != 0x30a {
		t.Errorf("control failed: the sweep looked for user 0x30a's key and found user %d", i)
	}
}

func TestInstanceID_TheNameRules(t *testing.T) {
	long63, long64 := strings.Repeat("x", 63), strings.Repeat("x", 64)
	for _, c := range []struct {
		name string
		id   string
		ok   bool
	}{
		{"a typical name", "platform-1", true},
		{"one byte", "a", true},
		{"63 bytes", long63, true},
		{"64 bytes", long64, false},
		{"empty", "", false},
		{"a tab", "plat\tform", false},
		{"a newline", "platform\n", false},
		{"a NUL", "plat\x00form", false},
		{"a delete character", "plat\x7fform", false},
		{"a non-ASCII letter", "platéform", false},
		{"printable punctuation and a space", "a b!~", true},
	} {
		if got := ValidInstanceID(c.id); got != c.ok {
			t.Errorf("%s: ValidInstanceID(%q) = %v, want %v", c.name, c.id, got, c.ok)
		}
		if c.id == "" {
			continue // the empty id means "the host name" to ResolveInstanceID, and is covered below
		}
		got, err := ResolveInstanceID(c.id)
		if (err == nil) != c.ok || (c.ok && got != c.id) {
			t.Errorf("%s: ResolveInstanceID(%q) = %q, %v, want it kept when valid and refused when not", c.name, c.id, got, err)
		}
	}
	// The default is a valid name of its own.
	def, err := ResolveInstanceID("")
	if err != nil || !ValidInstanceID(def) {
		t.Errorf("ResolveInstanceID(\"\") = %q, %v, want a valid name (the host name, or \"platform\")", def, err)
	}
	if host, herr := os.Hostname(); herr == nil && host != "" && ValidInstanceID(host) && def != host {
		t.Errorf("the default is %q, want the host name %q", def, host)
	}
}

func TestNew_BuildsTheElectionUnlessTestsSwitchItOff(t *testing.T) {
	cfg := Config{Pool: lazyPool(t), M3: nopSuspender{}, ExpireEvery: time.Second, ReconcileEvery: time.Second, ReverifyEvery: time.Minute, InstanceID: "platform-9"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.leader == nil || s.leader.Instance() != "platform-9" {
		t.Fatalf("New built no election for the instance platform-9: %+v", s.leader)
	}
	if h := s.Health(); h.Leader || h.Standby {
		t.Errorf("a Service that has not ticked yet reports a role: %+v", h)
	}
	// Control: the test-only option removes it.
	off, err := New(cfg, WithoutLeaderElection())
	if err != nil {
		t.Fatal(err)
	}
	if off.leader != nil {
		t.Error("WithoutLeaderElection left an election in place")
	}
	// The workers can be replaced (the harness tests count passes), and the real ones are the default.
	ex, rec := &fakeExpirer{}, &fakeReconciler{}
	w, err := New(cfg, WithWorkers(ex, rec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.expire != expirer(ex) || w.reconcile != reconciler(rec) {
		t.Error("WithWorkers did not replace the workers")
	}
	if _, isStore := s.expire.(*Store); !isStore {
		t.Errorf("the default expiry worker is %T, want *Store", s.expire)
	}
	if _, isRec := s.reconcile.(*Reconciler); !isRec {
		t.Errorf("the default reconcile worker is %T, want *Reconciler", s.reconcile)
	}
	// A name that cannot be an application_name is refused before anything runs.
	bad := cfg
	bad.InstanceID = strings.Repeat("x", 64)
	if _, err := New(bad); err == nil {
		t.Error("New accepted a 64-byte instance name")
	}
	if _, err := New(bad, WithoutLeaderElection()); err != nil {
		t.Errorf("with no election the name is not used, but New refused it: %v", err)
	}
	// An unset name is the host name.
	unset := cfg
	unset.InstanceID = ""
	u, err := New(unset)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := ResolveInstanceID(""); u.leader.Instance() != want {
		t.Errorf("an unset name became %q, want %q", u.leader.Instance(), want)
	}
}

// The job engine asks the very election the loops ask: Service.Leader is that one, and it is the same *Leader that Health reports on; a Service built without an election has none (the engine
// is then not started: a gate that leads everywhere would run its loops in every instance).
func TestService_LeaderIsTheElectionTheLoopsAsk(t *testing.T) {
	cfg := Config{Pool: lazyPool(t), M3: nopSuspender{}, ExpireEvery: time.Second, ReconcileEvery: time.Second, ReverifyEvery: time.Minute, InstanceID: "platform-7"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Leader() == nil || s.Leader() != s.leader || s.Leader().Instance() != "platform-7" {
		t.Fatalf("Leader() = %v, want the election the Service was built with", s.Leader())
	}
	// it is the election that Health reads: a change of its state is seen through both
	s.Leader().publish(true)
	if h := s.Health(); !h.Leader {
		t.Errorf("the election Leader() returns is not the one Health reports: %+v", h)
	}
	// the control: a Service built without an election has none
	off, err := New(cfg, WithoutLeaderElection())
	if err != nil {
		t.Fatal(err)
	}
	if off.Leader() != nil {
		t.Errorf("a Service without an election returned %v", off.Leader())
	}
}

// ---- the election cannot be switched off from production code --------------------------------------------------------------

// testOnlyOptions are the options that change how the Service is wired for a test. A non-test file outside this package that names one has put a test seam on a production path.
var testOnlyOptions = []string{"WithoutLeaderElection", "WithWorkers"}

// namesTestOnlyOption returns the options src names.
func namesTestOnlyOption(src string) []string {
	var hits []string
	for _, n := range testOnlyOptions {
		if strings.Contains(src, n) {
			hits = append(hits, n)
		}
	}
	return hits
}

// scanProduction reads every non-test Go file under the given directories that is not part of skipDir (this package) and returns "<file>: <option>" for every test-only option it names, and how
// many files it read.
func scanProduction(dirs []string, skipDir string) (hits []string, files int, err error) {
	for _, root := range dirs {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				if abs, aerr := filepath.Abs(path); aerr == nil && abs == skipDir {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			files++
			for _, n := range namesTestOnlyOption(string(b)) {
				hits = append(hits, filepath.ToSlash(path)+": "+n)
			}
			return nil
		})
		if err != nil {
			return nil, files, err
		}
	}
	return hits, files, nil
}

func TestLeader_NoProductionPathChangesTheWiring(t *testing.T) {
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	hits, files, err := scanProduction([]string{"../../cmd", "../../internal"}, here)
	if err != nil {
		t.Fatalf("cannot read the sources: %v (a test that cannot run fails)", err)
	}
	if files < 20 {
		t.Fatalf("read only %d non-test Go files under cmd and internal: the scan matched almost nothing", files)
	}
	if len(hits) != 0 {
		t.Errorf("production code names a test-only option of the entitlement service, so the leader election could be off in the running service:\n  %s", strings.Join(hits, "\n  "))
	}

	// Paired control: the scanner SEES a planted use (a file outside the package that names an option) and a clean file passes.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wiring.go"), []byte("package x\n\nvar _ = entitlements.WithoutLeaderElection()\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wiring_test.go"), []byte("package x\n\nvar _ = entitlements.WithWorkers(nil, nil)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted, n, err := scanProduction([]string{dir}, "")
	if err != nil || n != 2 || len(planted) != 1 || !strings.Contains(planted[0], "wiring.go: WithoutLeaderElection") {
		t.Errorf("control failed: the scan of a planted file = %v over %d files (%v), want exactly wiring.go flagged and the _test.go file ignored", planted, n, err)
	}
	if got := namesTestOnlyOption("package x\n"); len(got) != 0 {
		t.Errorf("control failed: a clean source names %v", got)
	}
	if got := namesTestOnlyOption("x := entitlements.WithWorkers(a, b)"); len(got) != 1 {
		t.Errorf("control failed: a source using WithWorkers was not flagged: %v", got)
	}
}

// The engine and the loops share one Leader and the engine stops first: an Ensure that waits behind another one (a hung connect) must give up the moment ITS context ends, or a shutdown takes as long
// as the other call's own bounded wait (found by internal/app's TestRun_TheDatabaseURLAndTheReadyzTimeoutAreConsumed: 9.7 s).
func TestLeader_AnEnsureWaitingBehindAnotherGivesUpWhenItsContextEnds(t *testing.T) {
	g := &gateTaker{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	l := newLeader(g, "me", nil, nil)
	first := make(chan bool, 1)
	go func() { first <- l.Ensure(context.Background()) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Ensure never reached the lock taker")
	}
	// Control: an Ensure with a context that does not end waits (it is behind the first), exactly as before.
	waiting := make(chan bool, 1)
	go func() { waiting <- l.Ensure(context.Background()) }()
	select {
	case <-waiting:
		t.Fatal("control failed: a second Ensure did not wait for the one in flight")
	case <-time.After(100 * time.Millisecond):
	}
	// The property: one whose context ends while it waits answers false at once, without the first having finished.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan bool, 1)
	go func() { gaveUp <- l.Ensure(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case got := <-gaveUp:
		if got {
			t.Error("an Ensure whose context ended answered true")
		}
	case <-time.After(2 * time.Second):
		close(g.gate)
		t.Fatal("an Ensure whose context ended kept waiting for the one in flight")
	}
	close(g.gate)
	<-first
	<-waiting
}
