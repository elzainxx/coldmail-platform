//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 2 close-out, items D8 (lock waits) and E1 (the forced interleaves that were still missing). Every test here has its own scratch database, runs the REAL money store as a real
// platform_app login, and proves contention from the server (pg_stat_activity) instead of hoping for it. Helpers are named com*.
//
// D8: a money operation waits for the (user, module) advisory lock for at most PLATFORM_MONEY_LOCK_TIMEOUT_MS (money.WithLockTimeout), then fails with money.ErrLockTimeout.
//
//	TestCloseoutMoney_LockWaitTimesOutWithATypedError         one subtest per operation (Reserve, Grant, Settle, Release, Adjust): a test session holds the lock, the call times out
//	                                                          within a bound with the typed error, writes nothing, the holder is unaffected; the control is the same call after release
//	TestCloseoutMoney_EveryMoneyTransactionBoundsItsLockWait  inside a transaction the caller owns, every In twin leaves lock_timeout at the store's wait (the default store: 10 s)
//
// E1: settle against release, and settle against an adjustment, on ONE (user, module), each in both orders, each in two forced shapes:
//
//	..._QueuedOnTheLock               the test holds the advisory lock, both real calls are PROVEN queued on it (two distinct backends, wait_event advisory), the first one strictly
//	                                  before the second, then the holder lets go: the first queued runs first (the queue is first come, first served), the second runs on its result
//	..._InterleavedAtTheBalanceWrite  the test holds the balance ROW instead: the first call gets past its reads and blocks at its write, and only then is the second started; the
//	                                  store's lock makes the second wait at the advisory lock. Without the lock this is exactly the interleave that corrupts (the fault proofs)
//
// The books are judged by comViolations after every interleave, BEFORE any assertion about who won: money conserved, balance_drift and reserve_overdrawn empty, platform.reserves
// equal to a recompute from the ledger, every reserve within what it held, the balance's reserved credits equal to the open reserves' remainders. The paired controls drive the same
// raw interleaves on scratch databases whose wall has been taken away and show that comViolations goes red on them.

// comLockWait is the wait the D8 tests give the store: long enough that a healthy runner does not trip it by accident, short enough to wait for.
const comLockWait = 300 * time.Millisecond

// comEnv is newMnyEnv with a store of the given wait.
func comEnv(t *testing.T, wait time.Duration) *mnyEnv {
	t.Helper()
	e := newMnyEnv(t)
	e.store = money.New(e.pool, money.WithLockTimeout(wait))
	return e
}

// ---- holding the advisory lock the store takes ----

// comHolder is a session of the test that holds the (user, module) advisory lock, with a second session that proves it is held.
type comHolder struct {
	e     *mnyEnv
	conn  *pgx.Conn
	probe *pgx.Conn
	key   int64
	pid   int32
	held  bool
}

func (e *mnyEnv) comHold(user, module string) *comHolder {
	t := e.t
	t.Helper()
	ctx := testCtx(t)
	h := &comHolder{e: e, conn: dial(t, e.s.DSN, nil), probe: dial(t, e.s.DSN, nil), key: money.LockKey(user, module)}
	if err := h.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&h.pid); err != nil {
		t.Fatalf("the holder's pid: %v", err)
	}
	if _, err := h.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", h.key); err != nil {
		t.Fatalf("the test cannot take the advisory lock %d: %v", h.key, err)
	}
	h.held = true
	t.Cleanup(func() { _ = h.conn.Close(context.Background()) }) // closing the session releases the lock whatever happened
	h.requireStillHeld("right after taking it")
	return h
}

// requireStillHeld proves from ANOTHER session that the lock is held, and that the holder's own session is alive.
func (h *comHolder) requireStillHeld(when string) {
	t := h.e.t
	t.Helper()
	ctx := testCtx(t)
	var got bool
	if err := h.probe.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", h.key).Scan(&got); err != nil || got {
		t.Fatalf("%s: another session took advisory lock %d while the test holds it (%v, %v): the test does not hold what it thinks it holds", when, h.key, got, err)
	}
	var one int
	if err := h.conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("%s: the holder's session no longer answers: %v", when, err)
	}
}

// release lets go of the lock; pg_advisory_unlock answers true only if the holder still held it, which is the proof that nothing took it from the holder.
func (h *comHolder) release() {
	t := h.e.t
	t.Helper()
	if !h.held {
		return
	}
	h.held = false
	var ok bool
	if err := h.conn.QueryRow(testCtx(t), "SELECT pg_advisory_unlock($1)", h.key).Scan(&ok); err != nil || !ok {
		t.Fatalf("the test cannot release its advisory lock %d: %v, %v (the holder did not hold it any more)", h.key, ok, err)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// D8: the wait is bounded
// ---------------------------------------------------------------------------------------------------------------------

// comExpectTimeout holds the (user, module) lock, runs the call, and asserts: it came back with ErrLockTimeout naming that user, module and the store's wait, after at least the wait
// and at most five times it, having written nothing (the ledger, snapshots, audit, balances, reserves and job statuses are as they were), while the holder was untouched. Then the
// holder lets go and the SAME call succeeds and writes: the control that the refusal was the held lock and nothing else.
func (e *mnyEnv) comExpectTimeout(op, user, module string, wait time.Duration, call func(ctx context.Context) error) {
	t := e.t
	t.Helper()
	before := e.state()
	statuses := e.str(`SELECT COALESCE(string_agg(id::text || ':' || status, ',' ORDER BY id), '-') FROM platform.jobs`)
	h := e.comHold(user, module)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		err := call(ctx)
		done <- outcome{err, time.Since(start)}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * wait):
		t.Fatalf("UNBOUNDED LOCK WAIT: %s was still waiting for the (user, module) lock %s after ten times its wait of %s while the test held it: the store does not bound its wait", op, user+"/"+module, wait)
	}

	var lt *money.LockTimeoutError
	if !errors.Is(got.err, money.ErrLockTimeout) || !errors.As(got.err, &lt) {
		t.Fatalf("%s while the lock is held: got %v, want money.ErrLockTimeout", op, got.err)
	}
	if lt.User != user || lt.Module != module || lt.Wait != wait {
		t.Fatalf("%s: the timeout says user %q module %q wait %s, want %q, %q and %s", op, lt.User, lt.Module, lt.Wait, user, module, wait)
	}
	for _, banned := range []string{"SELECT", "pg_advisory", "lock_timeout", "55P03", "SQLSTATE", "platform."} {
		if strings.Contains(got.err.Error(), banned) {
			t.Fatalf("%s: the error message names SQL or internals (%q): %v", op, banned, got.err)
		}
	}
	if got.elapsed < wait {
		t.Fatalf("%s gave up after %s, before its wait of %s: it did not wait for the lock", op, got.elapsed, wait)
	}
	if got.elapsed > 5*wait {
		t.Fatalf("%s gave up after %s, more than five times its wait of %s: the timeout is not at the dial", op, got.elapsed, wait)
	}
	e.requireSame(op+" that timed out", before)
	if after := e.str(`SELECT COALESCE(string_agg(id::text || ':' || status, ',' ORDER BY id), '-') FROM platform.jobs`); after != statuses {
		t.Fatalf("%s that timed out changed the job statuses: %s -> %s", op, statuses, after)
	}
	h.requireStillHeld(op + " after its timeout") // the holder is unaffected
	h.release()

	// THE CONTROL: the lock is free, the very same call goes through and writes.
	if err := call(testCtx(t)); err != nil {
		t.Fatalf("CONTROL: the same %s after the holder let go was refused: %v", op, err)
	}
	if e.state() == before {
		t.Fatalf("CONTROL: the same %s after the holder let go wrote nothing: the timed-out call and the control are not the same request", op)
	}
}

// TestCloseoutMoney_LockWaitTimesOutWithATypedError (D8, finding F35): one subtest per operation. A test session holds the advisory lock the store takes for the operation's (user,
// module); the real store, built with a 300 ms wait (money.WithLockTimeout, the dial PLATFORM_MONEY_LOCK_TIMEOUT_MS), returns money.ErrLockTimeout naming the user, module and wait
// after between 300 ms and 1.5 s, writes nothing, and leaves the holder holding. The control in every subtest is the same call after the holder let go.
// It fails with UNBOUNDED LOCK WAIT if the store waits for ever (the lock_timeout statement removed), and with a named reason if the error is another one, comes too early or too late,
// or if anything was written.
func TestCloseoutMoney_LockWaitTimesOutWithATypedError(t *testing.T) {
	e := comEnv(t, comLockWait)
	if got := e.store.LockTimeout(); got != comLockWait {
		t.Fatalf("the store under test waits %s, want %s", got, comLockWait)
	}
	type call = func(ctx context.Context) error
	cases := []struct {
		name    string
		arrange func(e *mnyEnv) (user, module string, call call)
	}{
		{"Reserve", func(e *mnyEnv) (string, string, call) {
			e.fund(mnyUserA, "m2", 100)
			job := e.newJob(mnyUserA, "m2")
			return mnyUserA, "m2", func(ctx context.Context) error {
				_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 5})
				return err
			}
		}},
		{"Grant", func(e *mnyEnv) (string, string, call) {
			return mnyUserB, "m1", func(ctx context.Context) error {
				_, err := e.store.Grant(ctx, money.GrantReq{UserID: mnyUserB, Module: "m1", Amount: 7, AdminID: mnyAdminA, Reason: "lock wait test", Key: "lw-grant"})
				return err
			}
		}},
		{"Settle", func(e *mnyEnv) (string, string, call) {
			e.fund(mnyUserC, "m2", 100)
			job := e.newJob(mnyUserC, "m2")
			e.reserve(job, 10)
			e.setStatus(job, "running")
			return mnyUserC, "m2", func(ctx context.Context) error {
				_, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("lw-1", mnyM2Sent), mnyItem("lw-2", mnyM2Sent), mnyItem("lw-3", mnyM2Sent)}})
				return err
			}
		}},
		{"Release", func(e *mnyEnv) (string, string, call) {
			e.fund(mnyUserA, "m1", 100)
			job := e.newJob(mnyUserA, "m1")
			e.reserve(job, 10)
			e.setStatus(job, "running")
			e.setStatus(job, "succeeded")
			return mnyUserA, "m1", func(ctx context.Context) error {
				_, err := e.store.Release(ctx, job)
				return err
			}
		}},
		{"Adjust", func(e *mnyEnv) (string, string, call) {
			e.fund(mnyUserB, "m2", 50)
			return mnyUserB, "m2", func(ctx context.Context) error {
				_, err := e.store.Adjust(ctx, money.AdjustReq{UserID: mnyUserB, Module: "m2", Amount: 10, AdminID: mnyAdminA, Reason: "lock wait test clawback", Key: "lw-adjust"})
				return err
			}
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			et := e.at(t)
			user, module, call := c.arrange(et)
			et.comExpectTimeout(c.name, user, module, comLockWait, call)
		})
	}
	e.requireViewsEmpty("after the lock-wait cases")
}

// comLockTimeoutMS is the lock_timeout in force in the session or transaction, in milliseconds (0 = no timeout).
func comLockTimeoutMS(t *testing.T, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) int64 {
	t.Helper()
	var ms int64
	if err := q.QueryRow(testCtx(t), "SELECT setting::bigint FROM pg_settings WHERE name = 'lock_timeout'").Scan(&ms); err != nil {
		t.Fatalf("read lock_timeout: %v", err)
	}
	return ms
}

// TestCloseoutMoney_EveryMoneyTransactionBoundsItsLockWait: the five In twins run inside a transaction the TEST owns, and after each the transaction's lock_timeout is the store's wait:
// 10000 ms for a store built with no option (the default, never 0), 300 ms for a store built with WithLockTimeout(300 ms). Before the twin runs the setting is the session's own (0), so
// the number is the store's doing, and after the transaction ends the pooled connection is back at 0: SET LOCAL did not leak.
// It fails if any operation does not set the bound, sets another value, or leaves it on the connection.
func TestCloseoutMoney_EveryMoneyTransactionBoundsItsLockWait(t *testing.T) {
	e := newMnyEnv(t) // its store has no option: the default
	ctx := testCtx(t)
	configured := money.New(e.pool, money.WithLockTimeout(comLockWait))

	e.fund(mnyUserA, "m1", 100)
	e.fund(mnyUserA, "m2", 100)
	e.fund(mnyUserB, "m2", 100)
	reserveJob := e.newJob(mnyUserA, "m2") // stays a draft: ReserveIn needs one
	settleJob := e.newJob(mnyUserA, "m1")
	e.reserve(settleJob, 5)
	e.setStatus(settleJob, "running")
	releaseJob := e.newJob(mnyUserB, "m2")
	e.reserve(releaseJob, 5)
	e.setStatus(releaseJob, "running")
	e.setStatus(releaseJob, "succeeded")

	ops := []struct {
		name string
		run  func(s *money.Store, tx pgx.Tx) error
	}{
		{"GrantIn", func(s *money.Store, tx pgx.Tx) error {
			_, err := s.GrantIn(ctx, tx, money.GrantReq{UserID: mnyUserA, Module: "m1", Amount: 3, AdminID: mnyAdminA, Reason: "bound test", Key: "bound-grant"})
			return err
		}},
		{"AdjustIn", func(s *money.Store, tx pgx.Tx) error {
			_, err := s.AdjustIn(ctx, tx, money.AdjustReq{UserID: mnyUserA, Module: "m1", Amount: 1, AdminID: mnyAdminA, Reason: "bound test", Key: "bound-adjust"})
			return err
		}},
		{"ReserveIn", func(s *money.Store, tx pgx.Tx) error {
			_, err := s.ReserveIn(ctx, tx, money.ReserveReq{JobID: reserveJob, Units: 2})
			return err
		}},
		{"SettleIn", func(s *money.Store, tx pgx.Tx) error {
			_, err := s.SettleIn(ctx, tx, money.SettleReq{JobID: settleJob, Items: []money.SettleItem{mnyItem("bound-1", mnyM1Accepted)}})
			return err
		}},
		{"ReleaseIn", func(s *money.Store, tx pgx.Tx) error {
			_, err := s.ReleaseIn(ctx, tx, releaseJob)
			return err
		}},
	}
	checked := 0
	for _, st := range []struct {
		name  string
		store *money.Store
		want  int64
	}{
		{"the default store (no option)", e.store, 10000},
		{"a store with WithLockTimeout(300ms)", configured, 300},
	} {
		for _, op := range ops {
			tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mnyNoErr(t, "begin the caller's transaction", err)
			if got := comLockTimeoutMS(t, tx); got != 0 {
				_ = tx.Rollback(ctx)
				t.Fatalf("control: a fresh transaction already has lock_timeout %d ms before the store touched it: the number below would prove nothing", got)
			}
			if err := op.run(st.store, tx); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("%s: %s in the caller's transaction: %v", st.name, op.name, err)
			}
			if got := comLockTimeoutMS(t, tx); got != st.want {
				_ = tx.Rollback(ctx)
				t.Fatalf("%s: after %s the transaction's lock_timeout is %d ms, want %d: the operation does not begin with the bound", st.name, op.name, got, st.want)
			}
			mnyNoErr(t, "roll back the caller's transaction", tx.Rollback(ctx))
			checked++
		}
	}
	if checked != 10 {
		t.Fatalf("checked %d (store, operation) pairs, want 10: the table lost rows", checked)
	}
	// SET LOCAL ended with the transactions: every pooled connection, looked at all at once, is back at the session's own value
	var held []*pgxpool.Conn
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := e.pool.Acquire(ctx)
		mnyNoErr(t, "acquire a pooled connection", err)
		held = append(held, c)
		if got := comLockTimeoutMS(t, c); got != 0 {
			t.Fatalf("pooled connection %d carries lock_timeout %d ms after the transactions ended: the setting leaked", i, got)
		}
	}
	e.requireViewsEmpty("at the end")
}

// ---------------------------------------------------------------------------------------------------------------------
// E1: the books, and the two kinds of forced interleave
// ---------------------------------------------------------------------------------------------------------------------

// comViolations is the invariant checker: every way the books of (user, module) are wrong, as sentences; empty when they are right. total is what the user was granted minus what
// was clawed back, which the credits (available + reserved + charged) must add up to. The checks are independent of one another on purpose: the views are the database's own alarm,
// the recomputes read the ledger alone.
func (e *mnyEnv) comViolations(user, module string, total int64) []string {
	e.t.Helper()
	var v []string
	for _, r := range e.rows(`SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`) {
		v = append(v, "platform.balance_drift has a row (user|module|stored available|ledger available|stored reserved|ledger reserved): "+r)
	}
	for _, r := range e.rows(`SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`) {
		v = append(v, "platform.reserve_overdrawn has a row (reserve|user|module|job|reserved|settled): "+r)
	}
	// every reserve recomputed from the ledger ALONE: what it held against what charges and releases drew from it
	for _, r := range e.rows(`SELECT l.id, l.job_id::text, l.reserved_delta, COALESCE(ch.charged, 0), COALESCE(rl.released, 0)
		FROM platform.ledger l
		LEFT JOIN LATERAL (SELECT sum(-c.reserved_delta)::bigint AS charged FROM platform.ledger c WHERE c.kind = 'charge' AND c.reserve_id = l.id) ch ON true
		LEFT JOIN LATERAL (SELECT sum(x.available_delta)::bigint AS released FROM platform.ledger x WHERE x.kind = 'release' AND x.reserve_id = l.id) rl ON true
		WHERE l.kind = 'reserve' AND COALESCE(ch.charged, 0) + COALESCE(rl.released, 0) > l.reserved_delta ORDER BY 1`) {
		v = append(v, "a reserve was spent beyond what it held, recomputed from the ledger (reserve|job|held|charged|released): "+r)
	}
	// platform.reserves against that recompute
	for _, r := range e.rows(`SELECT r.reserve_id, r.reserved, r.charged, r.released FROM platform.reserves r JOIN platform.ledger l ON l.id = r.reserve_id
		WHERE r.reserved <> l.reserved_delta
		   OR r.charged  <> COALESCE((SELECT sum(-c.reserved_delta)::bigint FROM platform.ledger c WHERE c.kind = 'charge'  AND c.reserve_id = r.reserve_id), 0)
		   OR r.released <> COALESCE((SELECT sum(c.available_delta)::bigint FROM platform.ledger c WHERE c.kind = 'release' AND c.reserve_id = r.reserve_id), 0)
		ORDER BY 1`) {
		v = append(v, "platform.reserves differs from the recompute of the ledger (reserve|reserved|charged|released): "+r)
	}
	// the balance's reserved credits are exactly what the open reserves still hold
	for _, r := range e.rows(`SELECT b.reserved, COALESCE((SELECT sum(r.reserved - r.charged - r.released) FROM platform.reserves r WHERE r.user_id = b.user_id AND r.module_id = b.module_id), 0)::bigint
		FROM platform.balances b
		WHERE b.user_id = $1 AND b.module_id = $2
		  AND b.reserved <> COALESCE((SELECT sum(r.reserved - r.charged - r.released) FROM platform.reserves r WHERE r.user_id = b.user_id AND r.module_id = b.module_id), 0)`, user, module) {
		v = append(v, "the balance's reserved credits are not the open reserves' remainders (balance|reserves): "+r)
	}
	// money is conserved: what the user holds (available + reserved) plus what was charged is what was granted less what was clawed back
	got := e.int(`SELECT (COALESCE((SELECT available FROM platform.balances WHERE user_id = $1 AND module_id = $2), 0)
		+ COALESCE((SELECT reserved FROM platform.balances WHERE user_id = $1 AND module_id = $2), 0)
		+ COALESCE((SELECT sum(-reserved_delta) FROM platform.ledger WHERE kind = 'charge' AND user_id = $1 AND module_id = $2), 0))::bigint`, user, module)
	if got != total {
		v = append(v, fmt.Sprintf("money is not conserved: available + reserved + charged = %d, want granted - adjusted = %d", got, total))
	}
	return v
}

// comRequireBooks fails the test, with the invariant named, if comViolations finds anything.
func (e *mnyEnv) comRequireBooks(when, user, module string, total int64) {
	e.t.Helper()
	if v := e.comViolations(user, module, total); len(v) != 0 {
		e.t.Fatalf("MONEY INVARIANT VIOLATED %s:\n  %s", when, strings.Join(v, "\n  "))
	}
}

// comArrange gives the user 100 credits of module 2 and two reserves of 10 units (10 credits each): the job under test, left running or ended (succeeded), and a BYSTANDER job whose
// reserve keeps the balance's reserved credits positive, so that only the per-reserve rule (and not the balance CHECK) can stop one job spending another's reservation.
func (e *mnyEnv) comArrange(user, status string) (job, bystander string) {
	e.t.Helper()
	e.fund(user, "m2", 100)
	job = e.newJob(user, "m2")
	e.reserve(job, 10)
	bystander = e.newJob(user, "m2")
	e.reserve(bystander, 10)
	e.setStatus(job, "running")
	if status == "succeeded" {
		e.setStatus(job, "succeeded")
	}
	e.requireBal("the arrangement", user, "m2", 80, 20)
	return job, bystander
}

func (e *mnyEnv) comReserveOf(job string) string {
	e.t.Helper()
	return e.str(`SELECT reserved::text || '|' || charged::text || '|' || released::text FROM platform.reserves WHERE job_id = $1`, job)
}

// comTrue runs a query that returns one boolean and reports it (the rows helper prints values, which a boolean column cannot be scanned into a string for).
func (e *mnyEnv) comTrue(sql string, args ...any) bool {
	e.t.Helper()
	got := e.rows(sql, args...)
	return len(got) == 1 && got[0] == "true"
}

// comCall is one real store call to run in a goroutine; its result is what the store returned.
type comCall struct {
	name string
	run  func(ctx context.Context) (any, error)
}

type comResult struct {
	val any
	err error
}

func (e *mnyEnv) comSettleCall(job string, items int) comCall {
	list := make([]money.SettleItem, items)
	for i := range list {
		list[i] = mnyItem(fmt.Sprintf("s-%d", i+1), mnyM2Sent)
	}
	return comCall{"Settle", func(ctx context.Context) (any, error) {
		return e.store.Settle(ctx, money.SettleReq{JobID: job, Items: list})
	}}
}

func (e *mnyEnv) comReleaseCall(job string) comCall {
	return comCall{"Release", func(ctx context.Context) (any, error) { return e.store.Release(ctx, job) }}
}

func (e *mnyEnv) comAdjustCall(user, key string, amount int64) comCall {
	return comCall{"Adjust", func(ctx context.Context) (any, error) {
		return e.store.Adjust(ctx, money.AdjustReq{UserID: user, Module: "m2", Amount: amount, AdminID: mnyAdminA, Reason: "close-out interleave: clawback", Key: key})
	}}
}

// comMode is what the test holds while the two calls start.
type comMode int

const (
	comHoldAdvisory comMode = iota // the (user, module) advisory lock: both calls are queued on it from the start
	comHoldRow                     // the balance row: the first call is in flight (past its reads, blocked at its write) when the second starts
)

func (m comMode) String() string {
	if m == comHoldAdvisory {
		return "queued on the advisory lock"
	}
	return "interleaved at the balance write"
}

// comInterleave runs `first` and then `second` as real store calls in separate goroutines, with the test holding the lock the mode names, proves from pg_stat_activity how they
// waited, lets go, and returns both results. serialised is true only if the proof of the mode holds: advisory mode, two distinct backends queued on the advisory lock behind the
// holder and neither call back; row mode, the first call blocked at the balance row behind the holder and the second waiting at the ADVISORY lock behind the first. It does not fail the
// test itself: the caller judges the books first (a store without the lock corrupts, and that is the loudest thing to say) and then asks for the proof.
func (e *mnyEnv) comInterleave(mode comMode, user, module string, first, second comCall) (r1, r2 comResult, serialised bool, detail string) {
	t := e.t
	t.Helper()
	ctx := testCtx(t)
	start := func(c comCall) <-chan comResult {
		ch := make(chan comResult, 1)
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			v, err := c.run(cctx)
			ch <- comResult{v, err}
		}()
		return ch
	}
	advisory := func(ws []mnyWait) []mnyWait {
		var out []mnyWait
		for _, w := range ws {
			if w.Event == "advisory" {
				out = append(out, w)
			}
		}
		return out
	}

	var release func()
	var holderPID int32
	switch mode {
	case comHoldAdvisory:
		h := e.comHold(user, module)
		holderPID, release = h.pid, h.release
	case comHoldRow:
		rowConn := dial(t, e.s.DSN, nil)
		rtx, err := rowConn.Begin(ctx)
		mnyNoErr(t, "begin the row-lock transaction", err)
		t.Cleanup(func() { _ = rtx.Rollback(context.Background()) })
		var one int
		if err := rtx.QueryRow(ctx, "SELECT 1 FROM platform.balances WHERE user_id = $1 AND module_id = $2 FOR UPDATE", user, module).Scan(&one); err != nil || one != 1 {
			t.Fatalf("the test cannot lock the balance row of %s/%s (it must exist: the user was funded): %v", user, module, err)
		}
		mnyNoErr(t, "the row holder's pid", rtx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID))
		release = func() { mnyNoErr(t, "release the balance row", rtx.Rollback(ctx)) }
	}

	c1 := start(first)
	var ws1, ws2 []mnyWait
	var ok1, ok2 bool
	var pid1 int32
	if mode == comHoldAdvisory {
		ws1, ok1 = e.waitUntil(15*time.Second, func(ws []mnyWait) bool {
			a := advisory(ws)
			return len(a) == 1 && mnyHasPID(a[0].Blockers, holderPID)
		})
	} else {
		ws1, ok1 = e.waitUntil(15*time.Second, func(ws []mnyWait) bool {
			return len(ws) == 1 && ws[0].Event != "advisory" && mnyHasPID(ws[0].Blockers, holderPID)
		})
	}
	if len(ws1) == 1 {
		pid1 = ws1[0].PID
	}
	var c2 <-chan comResult
	early := 0
	if ok1 {
		c2 = start(second)
		if mode == comHoldAdvisory {
			ws2, ok2 = e.waitUntil(15*time.Second, func(ws []mnyWait) bool {
				a := advisory(ws)
				if len(a) != 2 {
					return false
				}
				return mnyHasPID(a[0].Blockers, holderPID) && mnyHasPID(a[1].Blockers, holderPID) && a[0].PID != a[1].PID
			})
		} else {
			ws2, ok2 = e.waitUntil(15*time.Second, func(ws []mnyWait) bool { return len(ws) == 2 })
		}
		early = len(c1)
		if c2 != nil {
			early += len(c2)
		}
	}
	// the proof the mode asks for
	switch {
	case !ok1:
		detail = fmt.Sprintf("the first call (%s) was never seen waiting where the %s proof needs it: %s", first.name, mode, mnyDescribe(ws1))
	case !ok2:
		detail = fmt.Sprintf("the second call (%s) was never seen waiting where the %s proof needs it: %s", second.name, mode, mnyDescribe(ws2))
	case early != 0:
		detail = fmt.Sprintf("%d call(s) came back while the test still held the lock", early)
	case mode == comHoldAdvisory:
		serialised = true
		detail = "two distinct backends queued on the advisory lock behind the holder: " + mnyDescribe(ws2)
	default:
		a := advisory(ws2)
		if len(a) == 1 && a[0].PID != pid1 && mnyHasPID(a[0].Blockers, pid1) {
			serialised = true
			detail = "the first call blocked at the balance row, the second waiting at the advisory lock behind it: " + mnyDescribe(ws2)
		} else {
			detail = fmt.Sprintf("with the balance row held, %d backend(s) waited on the advisory lock behind the first call, want exactly one: %s", len(a), mnyDescribe(ws2))
		}
	}
	release()
	if c2 == nil { // the first call never queued (a store that takes no lock finished at once): the second still has to run
		c2 = start(second)
	}
	collect := func(ch <-chan comResult, name string) comResult {
		select {
		case r := <-ch:
			return r
		case <-time.After(120 * time.Second):
			t.Fatalf("%s did not return within 120s after the test let go", name)
			return comResult{}
		}
	}
	r1 = collect(c1, first.name)
	r2 = collect(c2, second.name)
	return r1, r2, serialised, detail
}

// comOrders runs one interleave in both orders, each as a subtest of its own that reports to ITS OWN T (run gets an environment whose t is the subtest's). Each order gets its own user,
// so the two share nothing in the books.
func comOrders(t *testing.T, e *mnyEnv, names [2]string, run func(et *mnyEnv, user string, order int)) {
	t.Helper()
	for i, name := range names {
		i, name := i, name
		t.Run(name, func(t *testing.T) {
			run(e.at(t), []string{mnyUserA, mnyUserB}[i], i)
		})
	}
}

// comSettleVsRelease (E1): a job that has ended holds a reserve of 10 credits; a late page of 6 charges (Settle) and the release of the remainder (Release) race for it. Whichever the lock
// hands out first, money is conserved and the reserve is never spent twice: settle first, it charges 6 and the release returns exactly the 4 left; release first, it returns all 10 and the
// settle is refused with ErrReserveExceeded and writes NOTHING. The bystander job's reserve is untouched either way.
func comSettleVsRelease(t *testing.T, mode comMode) {
	e := comEnv(t, 90*time.Second)
	comOrders(t, e, [2]string{"settle_first", "release_first"}, func(e *mnyEnv, user string, order int) {
		t := e.t // the subtest's own T: a Fatal on the parent's from here would end the wrong test
		job, bystander := e.comArrange(user, "succeeded")
		ledgerBefore := e.int("SELECT count(*) FROM platform.ledger")
		settle, release := e.comSettleCall(job, 6), e.comReleaseCall(job)
		first, second := settle, release
		if order == 1 {
			first, second = release, settle
		}
		r1, r2, serialised, detail := e.comInterleave(mode, user, "m2", first, second)
		rSettle, rRelease := r1, r2
		if order == 1 {
			rSettle, rRelease = r2, r1
		}

		// 1. the books, before anything else: a store without its lock corrupts here
		e.comRequireBooks("after settle and release raced ("+mode.String()+")", user, "m2", 100)
		e.requireViewsEmpty("after settle and release raced")
		e.requireReservesMatchLedger("after settle and release raced")
		// 2. the proof that the interleave was forced
		if !serialised {
			t.Fatalf("NOT SERIALISED (%s): settle and release on one (user, module) were not forced through the lock: %s", mode, detail)
		}
		t.Logf("proof (%s, order %d): %s", mode, order, detail)

		// 3. who won, and what each call did
		if order == 0 { // settle was first
			if rSettle.err != nil {
				t.Fatalf("the settle that ran first was refused: %v", rSettle.err)
			}
			if sr := rSettle.val.(money.SettleResult); sr.Charged != 6 || sr.Credits != 6 {
				t.Fatalf("the settle that ran first charged %d items / %d credits, want 6 / 6", sr.Charged, sr.Credits)
			}
			if rRelease.err != nil {
				t.Fatalf("the release that ran second was refused: %v", rRelease.err)
			}
			if rr := rRelease.val.(money.ReleaseResult); rr.Released != 4 || rr.Replayed {
				t.Fatalf("the release that ran second returned %+v, want exactly the 4 credits left, not a replay", rr)
			}
			e.requireBal("settle first, then release", user, "m2", 84, 10)
			if got := e.comReserveOf(job); got != "10|6|4" {
				t.Fatalf("the job's reserve is reserved|charged|released = %s, want 10|6|4", got)
			}
			if !e.comTrue(`SELECT (SELECT max(id) FROM platform.ledger WHERE job_id = $1 AND kind = 'charge') < (SELECT id FROM platform.ledger WHERE job_id = $1 AND kind = 'release')`, job) {
				t.Fatalf("the release row is not after the charge rows: the order the test forced is not the order the ledger shows")
			}
		} else { // release was first
			if rRelease.err != nil {
				t.Fatalf("the release that ran first was refused: %v", rRelease.err)
			}
			if rr := rRelease.val.(money.ReleaseResult); rr.Released != 10 || rr.Replayed {
				t.Fatalf("the release that ran first returned %+v, want all 10 credits, not a replay", rr)
			}
			// the settle that lost: the exact typed refusal, and nothing of it written
			if !errors.Is(rSettle.err, money.ErrReserveExceeded) || errors.Is(rSettle.err, money.ErrJobState) || errors.Is(rSettle.err, money.ErrNoReserve) {
				t.Fatalf("the settle that lost to the release returned %v, want exactly money.ErrReserveExceeded", rSettle.err)
			}
			for _, want := range []string{"has 0 left", "released 10"} {
				if !strings.Contains(rSettle.err.Error(), want) {
					t.Fatalf("the refusal %q does not say %q", rSettle.err, want)
				}
			}
			if n := e.chargeRows(job); n != 0 {
				t.Fatalf("the settle that was refused left %d charge row(s)", n)
			}
			if got := e.int("SELECT count(*) FROM platform.ledger"); got != ledgerBefore+1 {
				t.Fatalf("the ledger has %d rows, want %d: only the release was written", got, ledgerBefore+1)
			}
			e.requireBal("release first, then the refused settle", user, "m2", 90, 10)
			if got := e.comReserveOf(job); got != "10|0|10" {
				t.Fatalf("the job's reserve is reserved|charged|released = %s, want 10|0|10", got)
			}
		}
		if got := e.comReserveOf(bystander); got != "10|0|0" {
			t.Fatalf("the bystander's reserve is %s, want 10|0|0: one job spent another job's reservation", got)
		}
	})
}

// comSettleVsAdjust (E1): the job is running with a reserve of 10; a page of 6 charges (Settle) and the clawback of ALL 80 available credits (Adjust) race. Charges consume RESERVED
// credits and an adjustment removes AVAILABLE ones, so whichever goes first both succeed and the end state is the same: available 0, reserved 14, 6 charged. After the race another
// credit cannot be clawed back (ErrInsufficientCredits: available 0), because the 14 reserved credits are not available to an adjustment.
func comSettleVsAdjust(t *testing.T, mode comMode) {
	e := comEnv(t, 90*time.Second)
	comOrders(t, e, [2]string{"settle_first", "adjust_first"}, func(e *mnyEnv, user string, order int) {
		t := e.t // the subtest's own T
		job, bystander := e.comArrange(user, "running")
		settle, adjust := e.comSettleCall(job, 6), e.comAdjustCall(user, fmt.Sprintf("e1-adjust-%d", order), 80)
		first, second := settle, adjust
		if order == 1 {
			first, second = adjust, settle
		}
		r1, r2, serialised, detail := e.comInterleave(mode, user, "m2", first, second)
		rSettle, rAdjust := r1, r2
		if order == 1 {
			rSettle, rAdjust = r2, r1
		}

		e.comRequireBooks("after settle and adjust raced ("+mode.String()+")", user, "m2", 20) // 100 granted - 80 clawed back
		e.requireViewsEmpty("after settle and adjust raced")
		e.requireReservesMatchLedger("after settle and adjust raced")
		if !serialised {
			t.Fatalf("NOT SERIALISED (%s): settle and adjust on one (user, module) were not forced through the lock: %s", mode, detail)
		}
		t.Logf("proof (%s, order %d): %s", mode, order, detail)

		// both succeed, in either order
		if rSettle.err != nil {
			t.Fatalf("the settle was refused (order %d): %v: charges consume reserved credits, so an adjustment of the available ones cannot starve it", order, rSettle.err)
		}
		if sr := rSettle.val.(money.SettleResult); sr.Charged != 6 || sr.Credits != 6 {
			t.Fatalf("the settle charged %d items / %d credits, want 6 / 6", sr.Charged, sr.Credits)
		}
		if rAdjust.err != nil {
			t.Fatalf("the adjustment of the 80 available credits was refused (order %d): %v: a settle moves reserved credits only", order, rAdjust.err)
		}
		if en := rAdjust.val.(money.Entry); en.Amount != 80 || en.Replayed || en.Kind != "adjustment" {
			t.Fatalf("the adjustment returned %+v, want a new adjustment of 80", en)
		}
		e.requireBal("after settle and adjust", user, "m2", 0, 14)
		if got := e.comReserveOf(job); got != "10|6|0" {
			t.Fatalf("the job's reserve is reserved|charged|released = %s, want 10|6|0", got)
		}
		if got := e.comReserveOf(bystander); got != "10|0|0" {
			t.Fatalf("the bystander's reserve is %s, want 10|0|0", got)
		}
		// the charges drew from RESERVED credits, never from available ones
		if n := e.int(`SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'charge' AND available_delta <> 0`, job); n != 0 {
			t.Fatalf("%d charge row(s) moved available credits: a charge consumes reserved credits only", n)
		}
		// which went first, as the ledger shows it
		if order == 0 {
			if !e.comTrue(`SELECT (SELECT max(id) FROM platform.ledger WHERE job_id = $1 AND kind = 'charge') < (SELECT id FROM platform.ledger WHERE user_id = $2 AND kind = 'adjustment')`, job, user) {
				t.Fatalf("the adjustment row is not after the charge rows: the order the test forced is not the order the ledger shows")
			}
		} else if !e.comTrue(`SELECT (SELECT id FROM platform.ledger WHERE user_id = $2 AND kind = 'adjustment') < (SELECT min(id) FROM platform.ledger WHERE job_id = $1 AND kind = 'charge')`, job, user) {
			t.Fatalf("the adjustment row is not before the charge rows: the order the test forced is not the order the ledger shows")
		}
		// an adjustment can never take reserved credits: with 0 available, one more credit is refused although 14 are reserved, and nothing is written
		before := e.state()
		_, err := e.store.Adjust(testCtx(t), money.AdjustReq{UserID: user, Module: "m2", Amount: 1, AdminID: mnyAdminA, Reason: "close-out interleave: one more", Key: fmt.Sprintf("e1-adjust-more-%d", order)})
		mnyInsufficient(t, "an adjustment of 1 credit with 0 available and 14 reserved", err, 0, 1, 1)
		e.requireSame("the refused adjustment", before)
		e.requireBal("reserved credits are never touched by an adjustment", user, "m2", 0, 14)
	})
}

// TestCloseoutMoney_SettleVsRelease_QueuedOnTheLock (E1): settle against release, the test holding the advisory lock, both calls proven queued on it, in both orders.
// It fails with NOT SERIALISED if the two calls were not both held behind the (user, module) lock, and with MONEY INVARIANT VIOLATED if the books are wrong afterwards.
func TestCloseoutMoney_SettleVsRelease_QueuedOnTheLock(t *testing.T) {
	comSettleVsRelease(t, comHoldAdvisory)
}

// TestCloseoutMoney_SettleVsRelease_InterleavedAtTheBalanceWrite (E1): the same race with the test holding the balance ROW, so that the first call is in flight (past its reads, blocked at
// its write) when the second starts and the second can only be held back by the store's advisory lock. This is the interleave the lock exists for; with the lock and the per-reserve
// CHECK gone it charges and releases the same credits (see the fault proof interleave-settle-release and the controls below).
// It fails with MONEY INVARIANT VIOLATED if the reserve is spent twice, and with NOT SERIALISED unless one call waited at the row and the other at the advisory lock.
func TestCloseoutMoney_SettleVsRelease_InterleavedAtTheBalanceWrite(t *testing.T) {
	comSettleVsRelease(t, comHoldRow)
}

// TestCloseoutMoney_SettleVsAdjust_QueuedOnTheLock (E1): settle against an adjustment, the advisory lock held by the test, both calls proven queued, both orders.
func TestCloseoutMoney_SettleVsAdjust_QueuedOnTheLock(t *testing.T) {
	comSettleVsAdjust(t, comHoldAdvisory)
}

// TestCloseoutMoney_SettleVsAdjust_InterleavedAtTheBalanceWrite (E1): settle against an adjustment with the balance row held (the first call in flight, the second behind the
// advisory lock), both orders. The fault proof interleave-settle-adjust removes the lock and must turn this red with NOT SERIALISED.
func TestCloseoutMoney_SettleVsAdjust_InterleavedAtTheBalanceWrite(t *testing.T) {
	comSettleVsAdjust(t, comHoldRow)
}

// ---------------------------------------------------------------------------------------------------------------------
// the paired controls: the same races, raw, with a wall taken away
// ---------------------------------------------------------------------------------------------------------------------

// comRawSettleThenRelease is the check-then-act the store exists to prevent, in raw SQL on two real platform_app logins that take NO advisory lock: session S inserts `charges` charge
// rows of one credit each against the reserve and holds its transaction open; session L then reads the reserve's remainder (the settle has not committed, so it still sees the whole
// reserve) and inserts a release of exactly that remainder. The test PROVES L is blocked behind S (pg_stat_activity / pg_blocking_pids), then commits S. L then commits too, or is
// refused by the database; both outcomes are returned. remainderSeen is what L read.
func (e *mnyEnv) comRawSettleThenRelease(user, job string, reserveID int64, charges int) (errS, errL error, remainderSeen int64) {
	t := e.t
	t.Helper()
	pw := requireEnv(t, envAppPassword)
	connS, connL := dial(t, e.s.DSN, appDial(pw)), dial(t, e.s.DSN, appDial(pw))
	ctx := testCtx(t)

	txS, err := connS.Begin(ctx)
	mnyNoErr(t, "begin the raw settle", err)
	t.Cleanup(func() { _ = txS.Rollback(context.Background()) })
	var sPID int32
	mnyNoErr(t, "the raw settle's pid", txS.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&sPID))
	for i := 1; i <= charges; i++ {
		_, err := txS.Exec(ctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, item_ref, idempotency_key)
			VALUES ($1, 'm2', 'charge', 0, -1, $2, $3, 'm2_accepted', 1, 1, $4, $5)`, user, job, reserveID, fmt.Sprintf("raw-%d", i), fmt.Sprintf("raw-charge:%s:%d", job, i))
		mnyNoErr(t, "a raw charge", err)
	}

	seen := make(chan int64, 1)
	doneL := make(chan error, 1)
	go func() {
		lctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		txL, err := connL.Begin(lctx)
		if err != nil {
			seen <- -1
			doneL <- err
			return
		}
		defer func() { _ = txL.Rollback(context.Background()) }()
		var left int64
		if err := txL.QueryRow(lctx, `SELECT reserved - charged - released FROM platform.reserves WHERE reserve_id = $1`, reserveID).Scan(&left); err != nil {
			seen <- -1
			doneL <- err
			return
		}
		seen <- left
		if _, err := txL.Exec(lctx, `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, idempotency_key)
			VALUES ($1, 'm2', 'release', $2, $3, $4, $5, $6)`, user, left, -left, job, reserveID, "raw-release:"+job); err != nil {
			doneL <- err
			return
		}
		doneL <- txL.Commit(lctx)
	}()

	select {
	case remainderSeen = <-seen:
	case <-time.After(60 * time.Second):
		t.Fatal("CONTROL: the raw release did not read the remainder within 60s")
	}
	if remainderSeen < 0 {
		t.Fatalf("CONTROL: the raw release could not read the remainder: %v", <-doneL)
	}
	ws, blocked := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		for _, w := range ws {
			if mnyHasPID(w.Blockers, sPID) {
				return true
			}
		}
		return false
	})
	if !blocked {
		_ = txS.Rollback(ctx)
		t.Fatalf("CONTROL: the raw release was not blocked behind the raw settle (%s): the interleave did not happen", mnyDescribe(ws))
	}
	errS = txS.Commit(ctx)
	select {
	case errL = <-doneL:
	case <-time.After(60 * time.Second):
		t.Fatal("CONTROL: the raw release did not finish within 60s after the raw settle committed")
	}
	return errS, errL, remainderSeen
}

// TestCloseoutMoney_ControlSettleReleaseCorruptsWithoutTheProtection (E1, the paired control of the two settle-against-release tests): on a scratch database from which the CHECK
// reserves_not_overspent has been dropped, a raw settle (6 charges) and a raw release (the remainder it read before the settle committed, 10) both commit against a reserve of 10, with
// no advisory lock. The reserve is then charged 6 and released 10 and the books are WRONG: comViolations must say so (platform.reserve_overdrawn and the recompute from the ledger).
// The checker is also shown green on the same books before the corruption, so the red is the corruption's.
// It fails if the raw interleave does not corrupt (then the real tests above prove nothing about the lock), or if the checker stays silent on the corrupt books.
func TestCloseoutMoney_ControlSettleReleaseCorruptsWithoutTheProtection(t *testing.T) {
	e := newMnyEnv(t)
	job, _ := e.comArrange(mnyUserA, "succeeded")
	if v := e.comViolations(mnyUserA, "m2", 100); len(v) != 0 {
		t.Fatalf("CONTROL: the checker is red on healthy books before any corruption: %v", v)
	}
	e.exec("ALTER TABLE platform.reserves DROP CONSTRAINT reserves_not_overspent")
	if n := e.int("SELECT count(*) FROM pg_constraint WHERE conname = 'reserves_not_overspent'"); n != 0 {
		t.Fatalf("the CHECK is still there after the drop (%d)", n)
	}
	reserveID := e.int("SELECT reserve_id FROM platform.reserves WHERE job_id = $1", job)

	errS, errL, seen := e.comRawSettleThenRelease(mnyUserA, job, reserveID, 6)
	if errS != nil || errL != nil {
		t.Fatalf("CONTROL: with no lock and no CHECK both the raw settle (%v) and the raw release (%v) must commit", errS, errL)
	}
	if seen != 10 {
		t.Fatalf("CONTROL: the raw release read a remainder of %d, want 10: it was not racing the settle", seen)
	}
	if got := e.comReserveOf(job); got != "10|6|10" {
		t.Fatalf("CONTROL: the reserve is reserved|charged|released = %s, want 10|6|10 (6 charged and 10 released against 10 held)", got)
	}
	v := e.comViolations(mnyUserA, "m2", 100)
	if len(v) == 0 {
		t.Fatalf("CONTROL FAILED: the books hold a reserve that was charged 6 and released 10 against 10 and the checker is silent: it cannot fail, so the real interleave tests prove nothing")
	}
	if all := strings.Join(v, "\n"); !strings.Contains(all, "reserve_overdrawn") || !strings.Contains(all, "spent beyond what it held") {
		t.Fatalf("CONTROL: the checker is red but not for the overspent reserve:\n  %s", all)
	}
	t.Logf("CONTROL: with no lock and no CHECK the interleave overspent the reserve and the checker said:\n  %s", strings.Join(v, "\n  "))
}

// TestCloseoutMoney_DatabaseCheckRefusesTheRacedRelease (E1, the second wall alone): the same raw interleave with the CHECK in place and the advisory lock out of the picture. The
// release read 10, the settle committed 6 meanwhile, and the database refuses the release with 23514 reserves_not_overspent; the settle stands, the release wrote nothing, and the
// books are right (6 charged, 4 still held). It is the wall behind the store's lock, and the paired control of the previous test.
// It fails if the raw release commits, or is refused for another reason, or if the books are wrong afterwards.
func TestCloseoutMoney_DatabaseCheckRefusesTheRacedRelease(t *testing.T) {
	e := newMnyEnv(t)
	job, _ := e.comArrange(mnyUserA, "succeeded")
	reserveID := e.int("SELECT reserve_id FROM platform.reserves WHERE job_id = $1", job)

	errS, errL, seen := e.comRawSettleThenRelease(mnyUserA, job, reserveID, 6)
	mnyNoErr(t, "the raw settle", errS)
	if seen != 10 {
		t.Fatalf("the raw release read a remainder of %d, want 10: it was not racing the settle", seen)
	}
	mnyPgRefusal(t, "the raw release that raced the settle", errL, "23514", "reserves_not_overspent")
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'release'", job); n != 0 {
		t.Fatalf("the refused release left %d ledger row(s)", n)
	}
	if got := e.comReserveOf(job); got != "10|6|0" {
		t.Fatalf("the reserve is reserved|charged|released = %s, want 10|6|0", got)
	}
	e.requireBal("after the settle and the refused release", mnyUserA, "m2", 80, 14)
	e.comRequireBooks("after the database refused the raced release", mnyUserA, "m2", 100)
	e.requireReservesMatchLedger("after the database refused the raced release")
}

// TestCloseoutMoney_ControlAnAdjustmentThatTakesReservedCreditsIsCaught (E1, the control of the two settle-against-adjust tests): an adjustment may remove only AVAILABLE credits, and
// the wall is the ledger's shape CHECK ledger_shape_adjustment. With the CHECK in place a raw adjustment that also takes 10 reserved credits is refused with 23514 and nothing changes;
// with the CHECK dropped from the scratch database the same raw adjustment commits, a settle then charges 6 against a reserve whose credits the adjustment took, and the books are
// WRONG: the balance's reserved credits (4) are no longer the open reserves' remainders (14), and comViolations says so. (Settle and adjust move different columns of the balance, so
// the corruption here is not an overspend but the adjustment reaching into reserved credits: that is what the interleave tests above show cannot happen.)
// It fails if the checker is green on the corrupt books, or if the CHECK does not refuse the raw adjustment.
func TestCloseoutMoney_ControlAnAdjustmentThatTakesReservedCreditsIsCaught(t *testing.T) {
	e := newMnyEnv(t)
	job, _ := e.comArrange(mnyUserA, "running")
	if v := e.comViolations(mnyUserA, "m2", 100); len(v) != 0 {
		t.Fatalf("CONTROL: the checker is red on healthy books before any corruption: %v", v)
	}
	rawAdjust := func(key string) error {
		_, err := e.pool.Exec(testCtx(t), `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
			VALUES ($1, 'm2', 'adjustment', -70, -10, $2, 'control: an adjustment that takes reserved credits', $3)`, mnyUserA, mnyAdminA, key)
		return err
	}
	before := e.state()
	mnyPgRefusal(t, "a raw adjustment that takes reserved credits, the CHECK in place", rawAdjust("ctl-adjust-refused"), "23514", "ledger_shape_adjustment")
	e.requireSame("the refused raw adjustment", before)

	e.exec("ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_adjustment")
	if n := e.int("SELECT count(*) FROM pg_constraint WHERE conname = 'ledger_shape_adjustment'"); n != 0 {
		t.Fatalf("the CHECK is still there after the drop (%d)", n)
	}
	mnyNoErr(t, "the raw adjustment once the CHECK is gone", rawAdjust("ctl-adjust-allowed"))
	if res, err := e.store.Settle(testCtx(t), money.SettleReq{JobID: job, Items: []money.SettleItem{
		mnyItem("c-1", mnyM2Sent), mnyItem("c-2", mnyM2Sent), mnyItem("c-3", mnyM2Sent), mnyItem("c-4", mnyM2Sent), mnyItem("c-5", mnyM2Sent), mnyItem("c-6", mnyM2Sent)}}); err != nil || res.Charged != 6 {
		t.Fatalf("CONTROL: the settle after the raw adjustment returned %+v, %v, want 6 charged", res, err)
	}
	e.requireBal("after the adjustment that took reserved credits and the settle", mnyUserA, "m2", 10, 4)
	v := e.comViolations(mnyUserA, "m2", 20) // 100 granted - 80 removed (70 available, 10 reserved)
	if len(v) == 0 {
		t.Fatalf("CONTROL FAILED: an adjustment took reserved credits and the checker is silent: it cannot fail, so the interleave tests prove nothing about adjustments")
	}
	if all := strings.Join(v, "\n"); !strings.Contains(all, "reserved credits are not the open reserves' remainders") {
		t.Fatalf("CONTROL: the checker is red but not for the reserved credits:\n  %s", all)
	}
	t.Logf("CONTROL: with the shape CHECK gone an adjustment took reserved credits and the checker said:\n  %s", strings.Join(v, "\n  "))
}
