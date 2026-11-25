//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// Helpers of the money-store tests (money_store_test.go, money_concurrency_test.go). Every name starts with `mny` or `Money` so that it cannot clash with the helpers of
// the other files of this package.
//
// The shape every test shares: a migrated scratch database of its own (migratedScratch), a SUPERUSER connection to it (e.admin: seeding and observing, never the code under
// test), and the STORE under test over a pgxpool of REAL logins as platform_app (e.pool: not a role switch). The store therefore runs with exactly the privileges the live
// service has: SELECT and INSERT on the ledger, SELECT on the balances and reserves, no UPDATE on them, so the advisory lock is the only serialiser it has.
// Fixtures are committed for real (the store runs on other connections and could not see an open transaction).

// The fixture identities: customers and admins (an admin is a user too).
const (
	mnyUserA  = "a1a1a1a1-0000-4000-8000-000000000001"
	mnyUserB  = "b2b2b2b2-0000-4000-8000-000000000002"
	mnyUserC  = "c3c3c3c3-0000-4000-8000-000000000003"
	mnyAdminA = "ad000000-0000-4000-8000-00000000000a"
	mnyAdminB = "ad000000-0000-4000-8000-00000000000b"
	// mnyNobody is a well-formed id that no table holds.
	mnyNobody = "0f0f0f0f-0000-4000-8000-000000000000"
)

var (
	mnyJobSeq atomic.Int64
	mnyKeySeq atomic.Int64
)

// mnyOneLine puts a statement on one line for a failure message.
func mnyOneLine(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// mnyDB is what the observing helpers need; *pgx.Conn and pgx.Tx both satisfy it.
type mnyDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type mnyEnv struct {
	t     *testing.T
	s     scratch
	admin *pgx.Conn     // a superuser connection to the scratch database: seeding and observing
	pool  *pgxpool.Pool // real platform_app logins, what the store runs on
	store *money.Store
}

// newMnyEnv migrates a scratch database with the real binary, opens the store over a platform_app pool and seeds three customers and two admins.
func newMnyEnv(t *testing.T) *mnyEnv {
	t.Helper()
	s, admin := migratedScratch(t)
	e := &mnyEnv{t: t, s: s, admin: admin}
	e.pool = mnyPool(t, s.DSN)
	e.store = money.New(e.pool)
	for _, u := range []struct{ id, email string }{
		{mnyUserA, "money-a@example.test"}, {mnyUserB, "money-b@example.test"}, {mnyUserC, "money-c@example.test"},
		{mnyAdminA, "money-admin-a@example.test"}, {mnyAdminB, "money-admin-b@example.test"},
	} {
		e.exec("INSERT INTO auth.users (id, email) VALUES ($1, $2)", u.id, u.email)
		e.exec("INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", u.id, u.email)
	}
	e.exec("INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Money Admin A'), ($2, 'Money Admin B')", mnyAdminA, mnyAdminB)
	// Since platform_0013 module 1's vendor unit ships UNREACHABLE and the store freezes the reachable units only. The tests of this package were written for the vendor fallback ON (three units, the dearest at 2),
	// so the environment switches it on, once, through the one writer. The tests of the reachability rule itself (TestReachableUnits_*, and whatever tests the store against it) start from a database of their own.
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', true, 'money store tests: the vendor fallback is on')")
	return e
}

// at returns the same environment reporting to t: a subtest must fail ITS OWN T, never its parent's (a Fatal on the parent from the subtest's goroutine ends the wrong one).
func (e *mnyEnv) at(t *testing.T) *mnyEnv {
	c := *e
	c.t = t
	return &c
}

// mnyPool opens a pool of real logins as platform_app on the scratch database and proves it is that identity, not a superuser.
func mnyPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("cannot parse the scratch connection string: %s", scrubbed(err, dsn))
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "platform_app", requireEnv(t, envAppPassword)
	delete(cfg.ConnConfig.RuntimeParams, "timezone") // like rolesAppLogin: nothing from the test process's environment may override the role's settings
	delete(cfg.ConnConfig.RuntimeParams, "options")
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open the platform_app pool: %s", scrubbed(err, dsn))
	}
	t.Cleanup(pool.Close)
	var who string
	var super bool
	if err := pool.QueryRow(testCtx(t), "SELECT session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&who, &super); err != nil {
		t.Fatalf("the platform_app pool cannot run a query: %s", scrubbed(err, dsn))
	}
	if who != "platform_app" || super {
		t.Fatalf("the pool logged in as %q (superuser %v), want a real login as platform_app and no superuser: the store would not run with the service's privileges", who, super)
	}
	return pool
}

// mnyDeadPool is a pool whose database does not exist: nothing listens on port 1. It is built without a password and connects lazily, so a store over it answers a request
// that never reaches the database and fails any that does. A refusal that comes back as a validation error from it was decided BEFORE the database.
func mnyDeadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = "platform_app"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a lazy pool must be creatable without a database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---- seeding, through the superuser connection ----

func (e *mnyEnv) exec(sql string, args ...any) pgconn.CommandTag {
	e.t.Helper()
	tag, err := e.admin.Exec(testCtx(e.t), sql, args...)
	if err != nil {
		e.t.Fatalf("setup statement failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return tag
}

func (e *mnyEnv) int(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return n
}

func (e *mnyEnv) str(sql string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&s); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return s
}

// rows returns every row of a query as "a|b|c" text.
func (e *mnyEnv) rows(sql string, args ...any) []string {
	e.t.Helper()
	return mnyRows(e.t, e.admin, sql, args...)
}

func mnyRows(t *testing.T, db mnyDB, sql string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(testCtx(t), sql, args...)
	if err != nil {
		t.Fatalf("query failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// newJob inserts a DRAFT job of the user on the module (no price snapshot: the store freezes it at reserve) and returns its id.
func (e *mnyEnv) newJob(user, module string) string {
	e.t.Helper()
	id := fmt.Sprintf("0b000000-0000-4000-8000-%012d", mnyJobSeq.Add(1))
	e.exec("INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, $3)", id, user, module)
	return id
}

// setStatus walks a job to a status along the lifecycle of platform_0011, as the job engine will (jobWalkTo: previewed, confirmed and so on, each hop a platform.job_transition with its actor and
// reason). One active job per user and module is a database rule (confirmed, scheduled, running, paused, cancelling and completing hold the slot), so a test that needs several jobs of one user
// leaves all but one of them in draft, previewed or a terminal status. A status the lifecycle cannot reach from the job's current one (draft after previewed is the only way back; paused for a
// running job; cancelling for a module 1 job) fails the test: those are the rules, not something the helper works around.
func (e *mnyEnv) setStatus(job, status string) {
	e.t.Helper()
	jobWalkTo(e.t, e.admin, job, status)
}

// fund gives the user credits by a RAW ledger insert (no store code): the setup of every test that is not about grants.
func (e *mnyEnv) fund(user, module string, amount int64) {
	e.t.Helper()
	e.exec(`INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, acting_admin_id, reason, idempotency_key)
		VALUES ($1, $2, 'grant', $3, 0, $4, 'test funding', $5)`, user, module, amount, mnyAdminA, fmt.Sprintf("test-fund-%d", mnyKeySeq.Add(1)))
}

// ---- observing ----

// bal reads a balance straight from the table.
func (e *mnyEnv) bal(user, module string) (available, reserved int64, exists bool) {
	e.t.Helper()
	rows, err := e.admin.Query(testCtx(e.t), "SELECT available, reserved FROM platform.balances WHERE user_id = $1 AND module_id = $2", user, module)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		exists = true
		if err := rows.Scan(&available, &reserved); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return available, reserved, exists
}

func (e *mnyEnv) requireBal(when, user, module string, available, reserved int64) {
	e.t.Helper()
	a, r, ok := e.bal(user, module)
	if !ok || a != available || r != reserved {
		e.t.Fatalf("%s: the balance of %s on %s is available=%d reserved=%d (row exists: %v), want available=%d reserved=%d", when, user, module, a, r, ok, available, reserved)
	}
}

// state is a fingerprint of everything an operation could write: the ledger, the price snapshots, the audit log, the balances and the per-reserve table. Two equal states
// mean that nothing was written between them.
func (e *mnyEnv) state() string {
	e.t.Helper()
	return e.str(`SELECT (SELECT count(*) FROM platform.ledger)::text || '/' || (SELECT count(*) FROM platform.job_unit_prices)::text || '/' || (SELECT count(*) FROM platform.admin_audit)::text
		|| '/' || COALESCE((SELECT md5(string_agg(user_id::text || module_id || ':' || available::text || ':' || reserved::text, ',' ORDER BY user_id, module_id)) FROM platform.balances), '-')
		|| '/' || COALESCE((SELECT md5(string_agg(reserve_id::text || ':' || reserved::text || ':' || charged::text || ':' || released::text, ',' ORDER BY reserve_id)) FROM platform.reserves), '-')`)
}

func (e *mnyEnv) requireSame(what, before string) {
	e.t.Helper()
	if after := e.state(); after != before {
		e.t.Fatalf("%s: the database changed although nothing should have been written (ledger/prices/audit/balances/reserves fingerprint %s -> %s)", what, before, after)
	}
}

// requireViewsEmpty fails if the balance invariant or the per-reserve invariant is broken, printing the rows that break it.
func (e *mnyEnv) requireViewsEmpty(when string) {
	e.t.Helper()
	if d := e.rows(`SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`); len(d) != 0 {
		e.t.Fatalf("%s: platform.balance_drift is not empty (user|module|stored available|ledger available|stored reserved|ledger reserved): %v", when, d)
	}
	if o := e.rows(`SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`); len(o) != 0 {
		e.t.Fatalf("%s: platform.reserve_overdrawn is not empty (reserve|user|module|job|reserved|settled): %v", when, o)
	}
}

// requireReservesMatchLedger recomputes every reserve from the ledger (what it held, what charges and releases drew from it) and fails on any difference from
// platform.reserves, or on a reserve row of the ledger that has no row there. It returns how many reserves it compared and fails when that is 0: an empty comparison proves nothing.
func (e *mnyEnv) requireReservesMatchLedger(when string) int64 {
	e.t.Helper()
	n := e.int(`SELECT count(*) FROM platform.ledger WHERE kind = 'reserve'`)
	if n == 0 {
		e.t.Fatalf("%s: the ledger holds no reserve row: the recompute would compare nothing", when)
	}
	if missing := e.int(`SELECT count(*) FROM platform.ledger l WHERE l.kind = 'reserve' AND NOT EXISTS (SELECT 1 FROM platform.reserves r WHERE r.reserve_id = l.id)`); missing != 0 {
		e.t.Fatalf("%s: %d reserve row(s) of the ledger have no row in platform.reserves", when, missing)
	}
	if d := e.rows(`SELECT r.reserve_id, r.reserved, r.charged, r.released FROM platform.reserves r JOIN platform.ledger l ON l.id = r.reserve_id
		WHERE r.reserved <> l.reserved_delta
		   OR r.charged  <> COALESCE((SELECT sum(-c.reserved_delta)::bigint FROM platform.ledger c WHERE c.kind = 'charge'  AND c.reserve_id = r.reserve_id), 0)
		   OR r.released <> COALESCE((SELECT sum(c.available_delta)::bigint FROM platform.ledger c WHERE c.kind = 'release' AND c.reserve_id = r.reserve_id), 0)`); len(d) != 0 {
		e.t.Fatalf("%s: platform.reserves differs from the recompute of the ledger (reserve|reserved|charged|released): %v", when, d)
	}
	if got := e.int(`SELECT count(*) FROM platform.reserves`); got != n {
		e.t.Fatalf("%s: platform.reserves has %d row(s), the ledger %d reserve row(s)", when, got, n)
	}
	return n
}

// frozen is the job's price snapshot as {unit: cost}.
func (e *mnyEnv) frozen(job string) map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, r := range e.rows("SELECT unit, cost FROM platform.job_unit_prices WHERE job_id = $1", job) {
		var unit string
		var cost int
		if _, err := fmt.Sscanf(strings.Replace(r, "|", " ", 1), "%s %d", &unit, &cost); err != nil {
			e.t.Fatalf("cannot read the snapshot row %q: %v", r, err)
		}
		out[unit] = cost
	}
	return out
}

func (e *mnyEnv) chargeRows(job string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND kind = 'charge'", job)
}

func (e *mnyEnv) jobStatus(job string) string {
	e.t.Helper()
	return e.str("SELECT status FROM platform.jobs WHERE id = $1", job)
}

// ---- running the store ----

// reserve is Store.Reserve that fails the test on an error.
func (e *mnyEnv) reserve(job string, units int) money.ReserveResult {
	e.t.Helper()
	res, err := e.store.Reserve(testCtx(e.t), money.ReserveReq{JobID: job, Units: units})
	if err != nil {
		e.t.Fatalf("Reserve(%s, %d units) was refused: %v", job, units, err)
	}
	return res
}

// settle is Store.Settle that fails the test on an error.
func (e *mnyEnv) settle(job string, items ...money.SettleItem) money.SettleResult {
	e.t.Helper()
	res, err := e.store.Settle(testCtx(e.t), money.SettleReq{JobID: job, Items: items})
	if err != nil {
		e.t.Fatalf("Settle(%s, %d items) was refused: %v", job, len(items), err)
	}
	return res
}

// mnyItem builds a settle item.
func mnyItem(ref string, o charge.Outcome) money.SettleItem {
	return money.SettleItem{ItemRef: ref, Outcome: o}
}

// The outcomes the tests settle with, each the module's own words (charge table rows in internal/charge/rules.psv).
var (
	mnyM1Accepted = charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}                              // M1-03: m1_own
	mnyM1Cached   = charge.Outcome{Module: "m1", State: "valid", Reason: "accepted", FromCache: true}             // M1-01: m1_cache
	mnyM1Vendor   = charge.Outcome{Module: "m1", State: "valid", Reason: "external_verified"}                     // M1-02: m1_vendor
	mnyM1Capacity = charge.Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "rate_limited"} // M1-15: free (our capacity failed)
	mnyM1DNS      = charge.Outcome{Module: "m1", State: "unknown", Reason: "dns_error"}                           // M1-18: unmapped
	mnyM2Sent     = charge.Outcome{Module: "m2", State: "sent"}                                                   // M2-01: m2_accepted
	mnyM2Bounced  = charge.Outcome{Module: "m2", State: "bounced"}                                                // M2-02: m2_bounced
	mnyM2Failed   = charge.Outcome{Module: "m2", State: "failed"}                                                 // M2-03: free (pool exhaustion)
	mnyM2Queued   = charge.Outcome{Module: "m2", State: "queued"}                                                 // M2-04: not final
	mnyM2Weird    = charge.Outcome{Module: "m2", State: "teleported"}                                             // M2-99: unmapped
)

// ---- asserting the store's answers ----

func mnyIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got error %v, want errors.Is(%v)", what, err, want)
	}
}

func mnyNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

// mnyInsufficient asserts the error is ErrInsufficientCredits with exactly these numbers and that the store's own read (not the database's CHECK) refused it.
func mnyInsufficient(t *testing.T, what string, err error, available, needed, shortfall int64) {
	t.Helper()
	var ic money.ErrInsufficientCredits
	if !errors.As(err, &ic) {
		t.Fatalf("%s: got %v, want ErrInsufficientCredits{available %d, needed %d, shortfall %d}", what, err, available, needed, shortfall)
	}
	if ic.Available != available || ic.Needed != needed || ic.Shortfall != shortfall {
		t.Fatalf("%s: ErrInsufficientCredits is %+v, want available %d, needed %d, shortfall %d", what, ic, available, needed, shortfall)
	}
	if ic.Raced {
		t.Fatalf("%s: the refusal came from the database's CHECK after a read that said it fitted (Raced), not from the store's own read under the lock: %+v", what, ic)
	}
}

// mnyInvalid asserts the error is an InvalidError on the named field.
func mnyInvalid(t *testing.T, what string, err error, field string) {
	t.Helper()
	var ie *money.InvalidError
	if !errors.Is(err, money.ErrInvalid) || !errors.As(err, &ie) || ie.Field != field {
		t.Fatalf("%s: got %v, want an InvalidError on field %q", what, err, field)
	}
}

// mnyPgRefusal asserts err is a database error with this SQLSTATE and constraint name (the REASON, not only the code).
func mnyPgRefusal(t *testing.T, what string, err error, code, constraint string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: got %v, want a database error with SQLSTATE %s and constraint %s", what, err, code, constraint)
	}
	if pe.Code != code || pe.ConstraintName != constraint {
		t.Fatalf("%s: refused with SQLSTATE %s constraint %q (%s), want SQLSTATE %s constraint %q", what, pe.Code, pe.ConstraintName, pe.Message, code, constraint)
	}
}

func mnyPricesEqual(t *testing.T, what string, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: prices are %v, want %v", what, got, want)
	}
	for u, c := range want {
		if g, ok := got[u]; !ok || g != c {
			t.Fatalf("%s: prices are %v, want %v", what, got, want)
		}
	}
}

// ---- watching backends wait ----

// mnyWait is one platform_app backend of the scratch database that is waiting on a heavyweight lock.
type mnyWait struct {
	PID      int32
	Event    string // pg_stat_activity.wait_event: "advisory" for an advisory lock, "transactionid" / "tuple" for a row
	Query    string
	Blockers []int32 // pg_blocking_pids(pid)
}

// waiting lists the platform_app backends of this database that wait on a Lock right now. It runs on the superuser connection, which must not be used by another goroutine.
func (e *mnyEnv) waiting() []mnyWait {
	e.t.Helper()
	rows, err := e.admin.Query(testCtx(e.t), `SELECT pid, COALESCE(wait_event, ''), COALESCE(query, ''), pg_blocking_pids(pid)
		FROM pg_stat_activity
		WHERE datname = current_database() AND usename = 'platform_app' AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
		ORDER BY pid`)
	if err != nil {
		e.t.Fatalf("cannot read pg_stat_activity: %v", err)
	}
	defer rows.Close()
	var out []mnyWait
	for rows.Next() {
		var w mnyWait
		if err := rows.Scan(&w.PID, &w.Event, &w.Query, &w.Blockers); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// waitUntil polls the lock waiters until cond holds or the deadline passes, and returns the last list and whether cond held. It never sleeps blindly: it asks the server.
func (e *mnyEnv) waitUntil(within time.Duration, cond func([]mnyWait) bool) ([]mnyWait, bool) {
	e.t.Helper()
	deadline := time.Now().Add(within)
	for {
		ws := e.waiting()
		if cond(ws) {
			return ws, true
		}
		if time.Now().After(deadline) {
			return ws, false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func mnyDescribe(ws []mnyWait) string {
	if len(ws) == 0 {
		return "no backend is waiting on a lock"
	}
	parts := make([]string, len(ws))
	for i, w := range ws {
		q := w.Query
		if len(q) > 60 {
			q = q[:60] + "..."
		}
		parts[i] = fmt.Sprintf("pid %d waits on %q (blocked by %v) running %q", w.PID, w.Event, w.Blockers, q)
	}
	return strings.Join(parts, "; ")
}

func mnyHasPID(pids []int32, pid int32) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// startPriceChange runs, on another connection of the pool, what an admin does to change a unit's price (the roles hold a column-level UPDATE on cost, changed_by and
// change_reason), and returns a channel that receives the statement's outcome when it finishes.
func (e *mnyEnv) startPriceChange(unit string, cost int, reason string) <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, err := e.pool.Exec(ctx, "UPDATE platform.unit_costs SET cost = $1, changed_by = $2, change_reason = $3 WHERE unit = $4", cost, mnyAdminA, reason, unit)
		done <- err
	}()
	return done
}

// requireNoOverdraw is the last assertion of every concurrency test: the available balance of a user is never negative. It names OVERDRAW, so that a fault that removes
// the lock and the CHECK turns the test red with that word.
func (e *mnyEnv) requireNoOverdraw(user, module string) {
	e.t.Helper()
	a, r, _ := e.bal(user, module)
	if a < 0 {
		e.t.Fatalf("OVERDRAW: available=%d (reserved=%d) for user %s on %s: credits were spent that the user did not have", a, r, user, module)
	}
}
