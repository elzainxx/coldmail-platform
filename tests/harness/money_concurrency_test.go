//go:build harness

package harness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/money"
)

// The concurrency proof of the money core (Docs/phase-2-spec.md, "Concurrency proof"). A FORCED INTERLEAVE, never hope: each test makes the contention happen by holding a lock itself, PROVES
// from the server (pg_stat_activity) that the backends are blocked on it before it lets them go, and only then releases. The paired controls prove the interleave is dangerous: with the lock
// and the CHECK taken away, the same interleave overdraws. The last assertion of every real test names OVERDRAW, so that a fault that removes the protection turns it red with that word.
//
//   (a) TwoReservesOneBalance      the test holds the (user, module) ADVISORY lock the store takes; two real Store.Reserve calls for two jobs of ONE user (the balance covers exactly one) are
//                                  proven blocked on it, then released: one wins, one gets ErrInsufficientCredits, the balance never goes negative.
//   (b) ReservesInterleavedAtTheBalanceWrite   the test holds the balance ROW instead, so both reserves get past their own read before either writes: the store's lock still lets exactly one
//                                  through (one waits on the advisory lock, one on the row); with the lock gone this is the interleave that overdraws.
//   (c) ControlOverdrawsWithoutProtection      raw check-then-act, no lock, and the CHECK dropped from the scratch database: both read, both write, the balance goes NEGATIVE.
//   (d) DatabaseCheckRefusesTheSecondReserve   the same raw interleave with the CHECK in place: the second is refused 23514 balances_available_not_negative (the database's wall alone).

// mnyNaiveReserves is the check-then-act the store exists to prevent, in raw SQL on one real platform_app login per job: each session READS the user's available module-2 credits, then WAITS until every session
// has read, and only then writes its snapshot and its reserve row. It takes no advisory lock. It returns each session's outcome (nil = it committed).
func mnyNaiveReserves(t *testing.T, e *mnyEnv, user string, jobs []string, needed int64) []error {
	t.Helper()
	pw := requireEnv(t, envAppPassword)
	conns := make([]*pgx.Conn, len(jobs))
	for i := range jobs {
		conns[i] = dial(t, e.s.DSN, appDial(pw))
	}
	var read, done sync.WaitGroup
	read.Add(len(jobs))
	done.Add(len(jobs))
	release := make(chan struct{})
	errs := make([]error, len(jobs))
	for i := range jobs {
		i := i
		go func() {
			defer done.Done()
			var once sync.Once
			markRead := func() { once.Do(read.Done) }
			defer markRead() // whatever happens, the barrier is never left waiting for this session
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			tx, err := conns[i].Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			var available int64
			if err := tx.QueryRow(ctx, `SELECT available FROM platform.balances WHERE user_id = $1 AND module_id = 'm2'`, user).Scan(&available); err != nil {
				errs[i] = err
				return
			}
			markRead()
			select {
			case <-release:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			if available < needed {
				errs[i] = fmt.Errorf("the naive check saw %d available, %d needed", available, needed)
				return
			}
			if _, err := tx.Exec(ctx, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = 'm2'`, jobs[i]); err != nil {
				errs[i] = err
				return
			}
			var id int64
			if err := tx.QueryRow(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, unit, units, unit_cost, idempotency_key)
				VALUES ($1::uuid, 'm2', 'reserve', $2, $3, $4::uuid, 'm2_accepted', $5, 1, $6) RETURNING id`, user, -needed, needed, jobs[i], int(needed), "naive:"+jobs[i]).Scan(&id); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
		}()
	}
	allRead := make(chan struct{})
	go func() { read.Wait(); close(allRead) }()
	select {
	case <-allRead:
	case <-time.After(60 * time.Second):
		close(release)
		t.Fatal("the naive sessions did not all read the balance within 60s")
	}
	close(release) // both have read: now both write
	done.Wait()
	return errs
}

// mnyRequireOneReserveWon asserts the end state of two competing reserves of 10 credits against a balance of 10 on module 2: exactly one result is a success and the other is
// ErrInsufficientCredits refused by the STORE's own read (Raced false) with available 0; the winner's snapshot is whole, the loser left none; the balance is 0 available and 10 reserved; the books agree.
func (e *mnyEnv) requireOneReserveWon(jobs []string, errs []error) {
	e.t.Helper()
	wins := 0
	for i, err := range errs {
		prices := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", jobs[i])
		if err == nil {
			wins++
			if prices != 2 {
				e.t.Fatalf("the reserve that won left %d snapshot row(s), want the module's 2", prices)
			}
			continue
		}
		mnyInsufficient(e.t, "the reserve that lost", err, 0, 10, 10)
		if prices != 0 {
			e.t.Fatalf("the reserve that lost left %d snapshot row(s) behind", prices)
		}
	}
	if wins != 1 {
		e.t.Fatalf("%d of 2 reserves succeeded against a balance that covers exactly one: results %v", wins, errs)
	}
	e.requireBal("after the two reserves", mnyUserA, "m2", 0, 10)
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE user_id = $1 AND kind = 'reserve'", mnyUserA); n != 1 {
		e.t.Fatalf("%d reserve rows in the ledger, want exactly 1", n)
	}
	e.requireViewsEmpty("after the two reserves")
	e.requireReservesMatchLedger("after the two reserves")
}

// mnyTwoReserves is two Store.Reserve calls in flight.
type mnyTwoReserves struct {
	e       *mnyEnv
	results chan mnyReserveOutcome
	n       int
}

type mnyReserveOutcome struct {
	i   int
	err error
}

// startTwoReserves runs Store.Reserve for both jobs (10 units each) on their own goroutines.
func (e *mnyEnv) startTwoReserves(jobs []string) *mnyTwoReserves {
	r := &mnyTwoReserves{e: e, results: make(chan mnyReserveOutcome, len(jobs)), n: len(jobs)}
	for i, j := range jobs {
		i, j := i, j
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: j, Units: 10})
			r.results <- mnyReserveOutcome{i, err}
		}()
	}
	return r
}

// returned is how many of the calls have already come back.
func (r *mnyTwoReserves) returned() int { return len(r.results) }

// collect waits for both calls and returns their outcomes in job order.
func (r *mnyTwoReserves) collect() []error {
	out := make([]error, r.n)
	for k := 0; k < r.n; k++ {
		select {
		case o := <-r.results:
			out[o.i] = o.err
		case <-time.After(120 * time.Second):
			r.e.t.Fatal("a Store.Reserve did not return within 120s after the lock was released")
		}
	}
	return out
}

// TestMoneyConcurrency_TwoReservesOneBalance (a): the test takes the very advisory lock the store takes, for the (user, module) of a balance that covers exactly ONE reserve of 10 credits, then starts two real
// Store.Reserve calls for two jobs of that user. It PROVES from pg_stat_activity that two distinct platform_app backends wait on an advisory lock (wait_event_type Lock, wait_event advisory) and that neither call
// has returned, and only then releases the lock. Exactly one reserve succeeds; the other is ErrInsufficientCredits{available 0, needed 10, shortfall 10} from the store's own read; the balance is 0 available and
// 10 reserved, never negative, and the books agree. The control inside is that the lock is really held (a try-lock from another session fails).
// It fails with OVERDRAW if available < 0, and with NOT SERIALISED if the two reserves were not both held behind the (user, module) lock.
func TestMoneyConcurrency_TwoReservesOneBalance(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 10)
	jobs := []string{e.newJob(mnyUserA, "m2"), e.newJob(mnyUserA, "m2")}

	holder := dial(t, e.s.DSN, nil)
	key := money.LockKey(mnyUserA, "m2")
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		t.Fatalf("the test cannot take the advisory lock %d: %v", key, err)
	}
	held := true
	unlock := func() {
		if !held {
			return
		}
		held = false
		var ok bool
		if err := holder.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&ok); err != nil || !ok {
			t.Fatalf("the test cannot release its advisory lock %d: %v, %v", key, ok, err)
		}
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) }) // closing the session releases the lock whatever happened
	probe := dial(t, e.s.DSN, nil)
	var got bool
	if err := probe.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil || got {
		t.Fatalf("control: another session took advisory lock %d while the test holds it (%v, %v): the test does not hold what it thinks it holds", key, got, err)
	}

	run := e.startTwoReserves(jobs)
	ws, blocked := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		pids := map[int32]bool{}
		for _, w := range ws {
			if w.Event == "advisory" {
				pids[w.PID] = true
			}
		}
		return len(pids) >= 2
	})
	early := run.returned() // calls that came back although the test held the lock
	unlock()
	errs := run.collect()

	e.requireNoOverdraw(mnyUserA, "m2")
	if !blocked || early != 0 {
		t.Fatalf("NOT SERIALISED: the two reserves were not both held behind the (user, module) advisory lock %d while the test held it (both blocked: %v, calls that returned meanwhile: %d; %s): the store does not take the lock before it reads the balance", key, blocked, early, mnyDescribe(ws))
	}
	e.requireOneReserveWon(jobs, errs)
}

// TestMoneyConcurrency_ReservesInterleavedAtTheBalanceWrite (b): the same two reserves, but the test holds the balance ROW (SELECT ... FOR UPDATE as a superuser) instead of the advisory lock, which lets the
// reserve that holds the store's lock read its balance, write its ledger row and then wait at the balance write, while the other waits at the advisory lock. The test proves two platform_app backends wait on a
// Lock, one on the advisory lock and one on the row (so the store really does serialise), then releases the row. One wins, one loses with ErrInsufficientCredits from the store's own read. Without the store's lock
// this interleave is exactly the overdraw: both would have read 10 and both would write (see the control, and the fault patch that removes the lock and the CHECK).
// It fails with OVERDRAW if available < 0, and with NOT SERIALISED unless one backend waited on the advisory lock and one on the row.
func TestMoneyConcurrency_ReservesInterleavedAtTheBalanceWrite(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m2", 10)
	jobs := []string{e.newJob(mnyUserA, "m2"), e.newJob(mnyUserA, "m2")}

	rowConn := dial(t, e.s.DSN, nil)
	rtx, err := rowConn.Begin(ctx)
	mnyNoErr(t, "begin the row-lock transaction", err)
	t.Cleanup(func() { _ = rtx.Rollback(context.Background()) })
	var one int
	if err := rtx.QueryRow(ctx, "SELECT 1 FROM platform.balances WHERE user_id = $1 AND module_id = 'm2' FOR UPDATE", mnyUserA).Scan(&one); err != nil || one != 1 {
		t.Fatalf("the test cannot lock the balance row (it must exist: the user was funded): %v", err)
	}

	run := e.startTwoReserves(jobs)
	ws, blocked := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		pids := map[int32]bool{}
		for _, w := range ws {
			pids[w.PID] = true
		}
		return len(pids) >= 2
	})
	advisory, row := 0, 0
	for _, w := range ws {
		if w.Event == "advisory" {
			advisory++
		} else {
			row++
		}
	}
	mnyNoErr(t, "release the balance row", rtx.Rollback(ctx))
	errs := run.collect()

	e.requireNoOverdraw(mnyUserA, "m2")
	if !blocked {
		t.Fatalf("NOT SERIALISED: two reserves were not both blocked while the test held the balance row (%s)", mnyDescribe(ws))
	}
	if advisory != 1 || row != 1 {
		t.Fatalf("NOT SERIALISED: with the balance row held, %d backend(s) waited on the advisory lock and %d on the row, want exactly one of each (the store's lock lets one reserve reach the balance write): %s", advisory, row, mnyDescribe(ws))
	}
	e.requireOneReserveWon(jobs, errs)
}

// TestMoneyConcurrency_ControlOverdrawsWithoutProtection (c), the paired control: in a scratch database from which the CHECK balances_available_not_negative has been dropped (superuser, this database only), two
// raw sessions run the check-then-act the store replaces: both read 10 available, both then write a reserve of 10. Both commit and the balance goes NEGATIVE. This is the failure the lock and the CHECK exist to
// prevent, and the test proves it can happen: without it the real tests above would pass on a database that cannot overdraw for any reason.
// It fails if the naive interleave does NOT overdraw (then the interleave of the real tests proves nothing).
func TestMoneyConcurrency_ControlOverdrawsWithoutProtection(t *testing.T) {
	e := newMnyEnv(t)
	e.exec("ALTER TABLE platform.balances DROP CONSTRAINT balances_available_not_negative")
	if n := e.int("SELECT count(*) FROM pg_constraint WHERE conname = 'balances_available_not_negative'"); n != 0 {
		t.Fatalf("the CHECK is still there after the drop (%d)", n)
	}
	e.fund(mnyUserA, "m2", 10)
	jobs := []string{e.newJob(mnyUserA, "m2"), e.newJob(mnyUserA, "m2")}

	errs := mnyNaiveReserves(t, e, mnyUserA, jobs, 10)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("CONTROL: the naive reserve %d failed (%v): with no lock and no CHECK both must commit", i, err)
		}
	}
	available, reserved, _ := e.bal(mnyUserA, "m2")
	if available >= 0 {
		t.Fatalf("CONTROL FAILED: the naive check-then-act did not overdraw (available=%d reserved=%d): the interleave of the real tests proves nothing about the lock", available, reserved)
	}
	if available != -10 || reserved != 20 {
		t.Fatalf("CONTROL: available=%d reserved=%d, want -10 and 20 (two reserves of 10 against 10)", available, reserved)
	}
	t.Logf("CONTROL: with no lock and no CHECK the interleave overdrew: available=%d reserved=%d (the OVERDRAW the real tests name)", available, reserved)
	// the books still balance: the drift view compares the stored balance with the ledger sum, and both are -10. Only the CHECK (and the lock before it) can refuse the overdraw.
	e.requireViewsEmpty("after the control's overdraw")
}

// TestMoneyConcurrency_DatabaseCheckRefusesTheSecondReserve (d): the database layer alone. The same raw interleave as the control, with the lock out of the picture (these sessions never take it) and the CHECK IN
// PLACE: both read 10, both write, and the second is refused by the database with 23514 balances_available_not_negative while the first commits. The balance is 0 available and 10 reserved, never negative, and
// the refused session left no snapshot row. It is the second wall: the store's lock gives a clean ErrInsufficientCredits, this CHECK is what holds when something else writes.
// It fails with OVERDRAW if available < 0, and if both reserves commit or the refusal is for another reason.
func TestMoneyConcurrency_DatabaseCheckRefusesTheSecondReserve(t *testing.T) {
	e := newMnyEnv(t)
	e.fund(mnyUserA, "m2", 10)
	jobs := []string{e.newJob(mnyUserA, "m2"), e.newJob(mnyUserA, "m2")}

	errs := mnyNaiveReserves(t, e, mnyUserA, jobs, 10)
	e.requireNoOverdraw(mnyUserA, "m2")
	committed, refused := 0, 0
	for i, err := range errs {
		prices := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", jobs[i])
		if err == nil {
			committed++
			if prices != 2 {
				t.Fatalf("the session that committed left %d snapshot row(s), want 2", prices)
			}
			continue
		}
		refused++
		mnyPgRefusal(t, "the second raw reserve", err, "23514", "balances_available_not_negative")
		if prices != 0 {
			t.Fatalf("the refused session left %d snapshot row(s) behind", prices)
		}
	}
	if committed != 1 || refused != 1 {
		t.Fatalf("%d raw reserve(s) committed and %d were refused, want exactly one of each: results %v", committed, refused, errs)
	}
	e.requireBal("after the two raw reserves", mnyUserA, "m2", 0, 10)
	e.requireViewsEmpty("after the two raw reserves")
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE user_id = $1 AND kind = 'reserve'", mnyUserA); n != 1 {
		t.Fatalf("%d reserve rows in the ledger, want exactly 1", n)
	}
}
