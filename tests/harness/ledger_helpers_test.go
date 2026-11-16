//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Helpers shared by ledger_test.go, constraints_test.go and balance_invariant_test.go. Every name starts with `ledger` or
// `bal` so that it cannot clash with the helpers of the other test files in this package.
//
// The pattern all three files use: a migrated scratch database (migratedScratch), a transaction on its superuser connection
// that is ROLLED BACK when the test ends (ledgerBegin), fixtures written inside it (ledgerSeed), and every statement that is
// expected to fail run inside a SAVEPOINT (ledgerExpectRefused) so one refusal does not abort the transaction. A refusal is
// only accepted for the REASON the test names: the SQLSTATE AND the constraint, column, table or message text.

// The fixture identities. They are constants because every test has a scratch database of its own.
const (
	ledgerUserA      = "11111111-1111-1111-1111-111111111111"
	ledgerUserB      = "22222222-2222-2222-2222-222222222222"
	ledgerUserC      = "33333333-3333-3333-3333-333333333333"
	ledgerAdminA     = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	ledgerAdminB     = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	ledgerSystemUser = "00000000-0000-0000-0000-000000000000"
	ledgerNoSuchID   = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	ledgerJobA1  = "0a000000-0000-0000-0000-000000000001" // user A, module m1, price snapshot
	ledgerJobA2  = "0a000000-0000-0000-0000-000000000002" // user A, module m2, price snapshot
	ledgerJobB1  = "0a000000-0000-0000-0000-000000000003" // user B, module m1, price snapshot
	ledgerJobA1b = "0a000000-0000-0000-0000-000000000004" // user A, module m1, price snapshot (a second job)
	ledgerJobA0  = "0a000000-0000-0000-0000-000000000005" // user A, module m1, NO price snapshot
)

// ledgerDB is what the helpers need; *pgx.Conn and pgx.Tx both satisfy it.
type ledgerDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var ledgerSpaces = regexp.MustCompile(`\s+`)

func ledgerOneLine(sql string) string {
	return strings.TrimSpace(ledgerSpaces.ReplaceAllString(sql, " "))
}

var ledgerKeyCounter atomic.Int64

// ledgerNextKey is a fresh idempotency key.
func ledgerNextKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, ledgerKeyCounter.Add(1))
}

// ledgerUUID is a deterministic uuid for a test-made row: ledgerUUID("ee", 7).
func ledgerUUID(prefix string, n int) string {
	return fmt.Sprintf("%s000000-0000-0000-0000-%012d", prefix, n)
}

// ledgerBegin starts a transaction on the superuser connection and rolls it back when the test ends. Nothing a test does in
// it is ever committed.
func ledgerBegin(t *testing.T, c *pgx.Conn) pgx.Tx {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// ledgerMust runs a setup statement and fails the test if it fails.
func ledgerMust(t *testing.T, db ledgerDB, sql string, args ...any) pgconn.CommandTag {
	t.Helper()
	tag, err := db.Exec(testCtx(t), sql, args...)
	if err != nil {
		t.Fatalf("setup statement failed: %v\n  sql: %s", err, ledgerOneLine(sql))
	}
	return tag
}

// ledgerInt64 and ledgerString run a query that returns one value.
func ledgerInt64(t *testing.T, db ledgerDB, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(testCtx(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query failed: %v\n  sql: %s", err, ledgerOneLine(sql))
	}
	return n
}

func ledgerString(t *testing.T, db ledgerDB, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(testCtx(t), sql, args...).Scan(&s); err != nil {
		t.Fatalf("query failed: %v\n  sql: %s", err, ledgerOneLine(sql))
	}
	return s
}

func ledgerBool(t *testing.T, db ledgerDB, sql string, args ...any) bool {
	t.Helper()
	var b bool
	if err := db.QueryRow(testCtx(t), sql, args...).Scan(&b); err != nil {
		t.Fatalf("query failed: %v\n  sql: %s", err, ledgerOneLine(sql))
	}
	return b
}

// ledgerSeed writes the fixtures every ledger test starts from, in db (a transaction): three users, two admins (which are
// users too), five jobs, and the price snapshot of four of them (job A0 has none, on purpose).
func ledgerSeed(t *testing.T, db ledgerDB) {
	t.Helper()
	for _, u := range []struct{ id, email string }{
		{ledgerUserA, "a@example.test"}, {ledgerUserB, "b@example.test"}, {ledgerUserC, "c@example.test"},
		{ledgerAdminA, "admin-a@example.test"}, {ledgerAdminB, "admin-b@example.test"},
	} {
		ledgerMust(t, db, "INSERT INTO auth.users (id, email) VALUES ($1, $2)", u.id, u.email)
		ledgerMust(t, db, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", u.id, u.email)
	}
	ledgerMust(t, db, "INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Admin A'), ($2, 'Admin B')", ledgerAdminA, ledgerAdminB)
	for _, j := range []struct{ id, user, module string }{
		{ledgerJobA1, ledgerUserA, "m1"}, {ledgerJobA2, ledgerUserA, "m2"}, {ledgerJobB1, ledgerUserB, "m1"},
		{ledgerJobA1b, ledgerUserA, "m1"}, {ledgerJobA0, ledgerUserA, "m1"},
	} {
		ledgerMust(t, db, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, $3)", j.id, j.user, j.module)
	}
	for _, id := range []string{ledgerJobA1, ledgerJobA2, ledgerJobB1, ledgerJobA1b} {
		ledgerFreeze(t, db, id)
	}
}

// ledgerFreeze copies the CURRENT unit costs of the job's module into the job's price snapshot (what P2's confirm will do).
func ledgerFreeze(t *testing.T, db ledgerDB, job string) {
	t.Helper()
	tag := ledgerMust(t, db, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost)
		SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u
		WHERE u.module_id = (SELECT module_id FROM platform.jobs WHERE id = $1::uuid)`, job)
	if tag.RowsAffected() < 1 {
		t.Fatalf("the price snapshot of job %s copied %d rows: the instrument matched nothing", job, tag.RowsAffected())
	}
}

// ledgerNewJob inserts a draft job (and, when snapshot is true, freezes the current prices of its module) and returns its id.
func ledgerNewJob(t *testing.T, db ledgerDB, id, user, module string, snapshot bool) string {
	t.Helper()
	ledgerMust(t, db, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, $3)", id, user, module)
	if snapshot {
		ledgerFreeze(t, db, id)
	}
	return id
}

// ---- building ledger rows ----

// ledgerCols is the set of columns of one INSERT INTO platform.ledger; a nil value is an explicit NULL.
type ledgerCols map[string]any

// with returns a copy with the given name/value pairs set (a nil value sets NULL).
func (c ledgerCols) with(kv ...any) ledgerCols {
	out := ledgerCols{}
	for k, v := range c {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// without returns a copy with the named columns left out of the INSERT (their default or NULL applies).
func (c ledgerCols) without(names ...string) ledgerCols {
	out := ledgerCols{}
	for k, v := range c {
		out[k] = v
	}
	for _, n := range names {
		delete(out, n)
	}
	return out
}

func ledgerInsertSQL(c ledgerCols) (string, []any) {
	sql, args := ledgerInsertInto("platform.ledger", c)
	return sql + " RETURNING id", args
}

// ledgerInsertInto builds INSERT INTO table (cols...) VALUES ($1...) from the columns, in a fixed order.
func ledgerInsertInto(table string, c ledgerCols) (string, []any) {
	names := make([]string, 0, len(c))
	for k := range c {
		names = append(names, k)
	}
	sort.Strings(names)
	ph := make([]string, len(names))
	args := make([]any, len(names))
	for i, n := range names {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = c[n]
	}
	return "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(ph, ", ") + ")", args
}

// ledgerOKInsert inserts a legal row into any table (a control) and returns the command tag.
func ledgerOKInsert(t *testing.T, tx pgx.Tx, what, table string, c ledgerCols) pgconn.CommandTag {
	t.Helper()
	sql, args := ledgerInsertInto(table, c)
	return ledgerExpectOK(t, tx, what, sql, args...)
}

// ledgerRefusedInsert inserts a row into any table that must be refused with SQLSTATE state for the reason in why.
func ledgerRefusedInsert(t *testing.T, tx pgx.Tx, what, state string, why ledgerWhy, table string, c ledgerCols) {
	t.Helper()
	sql, args := ledgerInsertInto(table, c)
	ledgerExpectRefused(t, tx, what, state, why, sql, args...)
}

// The legal row of each kind. A test derives every refused row from one of these by changing exactly one thing, so a refusal
// can only be about that one thing.
func ledgerGrantCols(user, module string, amount int64) ledgerCols {
	return ledgerCols{"user_id": user, "module_id": module, "kind": "grant", "available_delta": amount, "reserved_delta": int64(0),
		"acting_admin_id": ledgerAdminA, "reason": "test grant", "idempotency_key": ledgerNextKey("grant")}
}

func ledgerReserveCols(user, module, job, unit string, units, cost int64) ledgerCols {
	return ledgerCols{"user_id": user, "module_id": module, "kind": "reserve", "available_delta": -units * cost, "reserved_delta": units * cost,
		"job_id": job, "unit": unit, "units": units, "unit_cost": cost, "idempotency_key": ledgerNextKey("reserve")}
}

func ledgerChargeCols(user, module, job string, reserveID int64, unit string, units, cost int64) ledgerCols {
	return ledgerCols{"user_id": user, "module_id": module, "kind": "charge", "available_delta": int64(0), "reserved_delta": -units * cost,
		"job_id": job, "reserve_id": reserveID, "unit": unit, "units": units, "unit_cost": cost, "idempotency_key": ledgerNextKey("charge")}
}

func ledgerReleaseCols(user, module, job string, reserveID, amount int64) ledgerCols {
	return ledgerCols{"user_id": user, "module_id": module, "kind": "release", "available_delta": amount, "reserved_delta": -amount,
		"job_id": job, "reserve_id": reserveID, "idempotency_key": ledgerNextKey("release")}
}

// ledgerAdjustmentCols is an adjustment (platform_0004, a clawback): an admin removes `amount` (> 0) AVAILABLE credits, with a reason, tied to no job, a reserve or a unit.
func ledgerAdjustmentCols(user, module string, amount int64) ledgerCols {
	return ledgerCols{"user_id": user, "module_id": module, "kind": "adjustment", "available_delta": -amount, "reserved_delta": int64(0),
		"acting_admin_id": ledgerAdminA, "reason": "test clawback", "idempotency_key": ledgerNextKey("adjustment")}
}

// ledgerM3Cols is an M3 entitlement event on module m3; enable and disable name an admin, expire does not.
func ledgerM3Cols(kind, user string) ledgerCols {
	c := ledgerCols{"user_id": user, "module_id": "m3", "kind": kind, "available_delta": int64(0), "reserved_delta": int64(0),
		"reason": "test entitlement", "idempotency_key": ledgerNextKey(kind)}
	if kind != "m3_expire" {
		c["acting_admin_id"] = ledgerAdminA
	}
	return c
}

// ledgerPut inserts the row, fails the test if it is refused, and returns the new ledger id.
func ledgerPut(t *testing.T, db ledgerDB, c ledgerCols) int64 {
	t.Helper()
	sql, args := ledgerInsertSQL(c)
	var id int64
	if err := db.QueryRow(testCtx(t), sql, args...).Scan(&id); err != nil {
		t.Fatalf("a legal %v row for user %v was refused: %v", c["kind"], c["user_id"], err)
	}
	return id
}

// ledgerFund grants credits to a user on a metered module.
func ledgerFund(t *testing.T, db ledgerDB, user, module string, amount int64) int64 {
	t.Helper()
	return ledgerPut(t, db, ledgerGrantCols(user, module, amount))
}

// ---- statements expected to succeed or to be refused, each in a savepoint ----

// ledgerWhy is the reason a refusal is accepted for: every non-empty field must match the database's error.
type ledgerWhy struct {
	Constraint string // exact constraint name (CHECK, UNIQUE, FOREIGN KEY, exclusion)
	Table      string // exact table name the error is about
	Column     string // exact column name (NOT NULL violations)
	Message    string // substring of the server's message (triggers, privileges, generated columns)
}

func (w ledgerWhy) empty() bool {
	return w.Constraint == "" && w.Table == "" && w.Column == "" && w.Message == ""
}

// ledgerTrial runs fn inside a savepoint that is ALWAYS rolled back, so a control can do what it needs (disable a trigger, drop a
// constraint, run the statement as the superuser) and leave the transaction exactly as it found it.
func ledgerTrial(t *testing.T, tx pgx.Tx, fn func(sp pgx.Tx)) {
	t.Helper()
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sp.Rollback(ctx) }()
	fn(sp)
}

// ledgerExpectOK runs the statement in a savepoint that is RELEASED (its effects stay in the transaction). It fails the test
// if the statement fails. This is the paired control of every refusal: the legal statement passes.
func ledgerExpectOK(t *testing.T, tx pgx.Tx, what, sql string, args ...any) pgconn.CommandTag {
	t.Helper()
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := sp.Exec(ctx, sql, args...)
	if err != nil {
		_ = sp.Rollback(ctx)
		t.Fatalf("%s: the control statement was refused (%v)\n  sql: %s", what, err, ledgerOneLine(sql))
	}
	if err := sp.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return tag
}

// ledgerExpectRefused runs the statement in a savepoint that is ROLLED BACK whatever happens, and fails the test unless the
// statement is refused with SQLSTATE state AND the reason in why. A refusal for any other reason (another constraint, another
// SQLSTATE) fails: that is how a test that passes for the wrong reason is caught.
func ledgerExpectRefused(t *testing.T, tx pgx.Tx, what, state string, why ledgerWhy, sql string, args ...any) {
	t.Helper()
	if why.empty() {
		t.Fatalf("%s: a refusal test must name the REASON (constraint, table, column or message), not only the SQLSTATE", what)
	}
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := sp.Exec(ctx, sql, args...)
	if err := sp.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if execErr == nil {
		t.Fatalf("%s: was ALLOWED (wanted SQLSTATE %s, %+v)\n  sql: %s", what, state, why, ledgerOneLine(sql))
	}
	var pe *pgconn.PgError
	if !errors.As(execErr, &pe) {
		t.Fatalf("%s: failed with a non-database error: %v", what, execErr)
	}
	var bad []string
	if pe.Code != state {
		bad = append(bad, fmt.Sprintf("SQLSTATE %s, want %s", pe.Code, state))
	}
	if why.Constraint != "" && pe.ConstraintName != why.Constraint {
		bad = append(bad, fmt.Sprintf("constraint %q, want %q", pe.ConstraintName, why.Constraint))
	}
	if why.Table != "" && pe.TableName != why.Table {
		bad = append(bad, fmt.Sprintf("table %q, want %q", pe.TableName, why.Table))
	}
	if why.Column != "" && pe.ColumnName != why.Column {
		bad = append(bad, fmt.Sprintf("column %q, want %q", pe.ColumnName, why.Column))
	}
	if why.Message != "" && !strings.Contains(pe.Message, why.Message) {
		bad = append(bad, fmt.Sprintf("message %q does not contain %q", pe.Message, why.Message))
	}
	if len(bad) > 0 {
		t.Fatalf("%s: refused, but not for the expected reason: %s\n  server said: %s (SQLSTATE %s, constraint %q)\n  sql: %s", what, strings.Join(bad, "; "), pe.Message, pe.Code, pe.ConstraintName, ledgerOneLine(sql))
	}
}

// ledgerOKRow inserts a legal ledger row (a control) and returns its id.
func ledgerOKRow(t *testing.T, tx pgx.Tx, what string, c ledgerCols) int64 {
	t.Helper()
	sql, args := ledgerInsertSQL(c)
	ctx := testCtx(t)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := sp.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		_ = sp.Rollback(ctx)
		t.Fatalf("%s: the legal control row was refused: %v\n  row: %v", what, err, c)
	}
	if err := sp.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// ledgerRefusedRow runs an INSERT of a ledger row that must be refused with SQLSTATE state for the reason in why.
func ledgerRefusedRow(t *testing.T, tx pgx.Tx, what, state string, why ledgerWhy, c ledgerCols) {
	t.Helper()
	sql, args := ledgerInsertSQL(c)
	ledgerExpectRefused(t, tx, what, state, why, sql, args...)
}

// ledgerRefusedCheck is the common case: a CHECK constraint (SQLSTATE 23514) by name.
func ledgerRefusedCheck(t *testing.T, tx pgx.Tx, what, constraint string, c ledgerCols) {
	t.Helper()
	ledgerRefusedRow(t, tx, what, "23514", ledgerWhy{Constraint: constraint}, c)
}

// ledgerRefusedFK is a foreign-key violation (SQLSTATE 23503) by constraint name.
func ledgerRefusedFK(t *testing.T, tx pgx.Tx, what, constraint string, c ledgerCols) {
	t.Helper()
	ledgerRefusedRow(t, tx, what, "23503", ledgerWhy{Constraint: constraint}, c)
}

// ---- observing balances and the two views ----

type ledgerBal struct {
	Available, Reserved int64
	Exists              bool
}

func ledgerBalance(t *testing.T, db ledgerDB, user, module string) ledgerBal {
	t.Helper()
	var b ledgerBal
	rows, err := db.Query(testCtx(t), "SELECT available, reserved FROM platform.balances WHERE user_id = $1 AND module_id = $2", user, module)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		b.Exists = true
		if err := rows.Scan(&b.Available, &b.Reserved); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b
}

// ledgerRequireBalance fails unless the user's balance on the module exists and is exactly available/reserved.
func ledgerRequireBalance(t *testing.T, db ledgerDB, when, user, module string, available, reserved int64) {
	t.Helper()
	b := ledgerBalance(t, db, user, module)
	if !b.Exists || b.Available != available || b.Reserved != reserved {
		t.Fatalf("%s: balance of %s on %s is available=%d reserved=%d (row exists: %v), want available=%d reserved=%d", when, user, module, b.Available, b.Reserved, b.Exists, available, reserved)
	}
}

func ledgerRows(t *testing.T, db ledgerDB, sql string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(testCtx(t), sql, args...)
	if err != nil {
		t.Fatalf("query failed: %v\n  sql: %s", err, ledgerOneLine(sql))
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

// ledgerDrift is the rows of platform.balance_drift as "user|module|stored_available|ledger_available|stored_reserved|ledger_reserved".
func ledgerDrift(t *testing.T, db ledgerDB) []string {
	t.Helper()
	return ledgerRows(t, db, `SELECT user_id::text, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY 1, 2`)
}

// ledgerOverdrawn is the rows of platform.reserve_overdrawn as "reserve_id|user|module|job|reserved_amount|settled_amount".
func ledgerOverdrawn(t *testing.T, db ledgerDB) []string {
	t.Helper()
	return ledgerRows(t, db, `SELECT reserve_id, user_id::text, module_id, job_id::text, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY 1`)
}

// ledgerRequireViewsEmpty fails if either invariant view lists a row.
func ledgerRequireViewsEmpty(t *testing.T, db ledgerDB, when string) {
	t.Helper()
	if d := ledgerDrift(t, db); len(d) != 0 {
		t.Fatalf("%s: platform.balance_drift is not empty: %v", when, d)
	}
	if o := ledgerOverdrawn(t, db); len(o) != 0 {
		t.Fatalf("%s: platform.reserve_overdrawn is not empty: %v", when, o)
	}
}

// ledgerDigest is a fingerprint of a table's content: row count and the md5 of every row's text in a fixed order.
func ledgerDigest(t *testing.T, db ledgerDB, table string) string {
	t.Helper()
	return ledgerString(t, db, "SELECT count(*)::text || ':' || COALESCE(md5(string_agg(x::text, '|' ORDER BY x::text)), '-') FROM "+table+" x")
}

// ---- switching roles, proven to bite ----

// ledgerSwitchRole does SET LOCAL ROLE inside tx and proves the switch is real and is not a superuser: current_user changed,
// session_user did not, the role is no superuser, and a statement only a superuser may run (SET session_replication_role)
// succeeds BEFORE the switch (the control) and is refused with 42501 after it. Without this a SET ROLE that silently did
// nothing would leave every refusal below it unproven. The technique is the one of roleSwitchBites in grants_test.go.
func ledgerSwitchRole(t *testing.T, tx pgx.Tx, role string) {
	t.Helper()
	ctx := testCtx(t)
	var sessionUser, currentUser string
	var superuser bool
	if err := tx.QueryRow(ctx, "SELECT session_user, current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&sessionUser, &currentUser, &superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser || currentUser != sessionUser {
		t.Fatalf("precondition: the connection must be a superuser running as itself, got session_user=%s current_user=%s superuser=%v", sessionUser, currentUser, superuser)
	}
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		if _, err := sp.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
			t.Fatalf("control: the superuser could not set session_replication_role, so the refusal below would prove nothing: %v", err)
		}
	})

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET LOCAL ROLE %s: %v", role, err)
	}
	var nowCurrent, nowSession string
	var nowSuper bool
	if err := tx.QueryRow(ctx, "SELECT current_user, session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)").Scan(&nowCurrent, &nowSession, &nowSuper); err != nil {
		t.Fatal(err)
	}
	if nowCurrent != role || nowSession != sessionUser || nowSuper {
		t.Fatalf("the role switch did not happen: current_user=%s session_user=%s superuser=%v, want current_user=%s session_user=%s superuser=false", nowCurrent, nowSession, nowSuper, role, sessionUser)
	}
	ledgerExpectRefused(t, tx, "as "+role+", SET session_replication_role (superuser only)", "42501", ledgerWhy{Message: `permission denied to set parameter "session_replication_role"`}, "SET LOCAL session_replication_role = replica")
}

func ledgerResetRole(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ledgerMust(t, tx, "RESET ROLE")
}

// ledgerSeedCommitted writes the append-only tests' starting state into the scratch database for real (a committed
// transaction), so that "the ledger is intact afterwards" compares real rows: fixtures, a short ledger history, one audit row.
// It returns the id of the reserve row.
func ledgerSeedCommitted(t *testing.T, c *pgx.Conn) (reserveID int64) {
	t.Helper()
	ctx := testCtx(t)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, tx)
	ledgerPut(t, tx, ledgerGrantCols(ledgerUserA, "m1", 100).with("idempotency_key", "ao-grant-a"))
	ledgerPut(t, tx, ledgerGrantCols(ledgerUserB, "m1", 50).with("idempotency_key", "ao-grant-b")) // the one row nothing references
	reserveID = ledgerPut(t, tx, ledgerReserveCols(ledgerUserA, "m1", ledgerJobA1, "m1_vendor", 10, 2))
	ledgerPut(t, tx, ledgerChargeCols(ledgerUserA, "m1", ledgerJobA1, reserveID, "m1_own", 3, 1))
	ledgerMust(t, tx, `INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, reason)
		VALUES ($1, 'action', 'seed-action', 'user', $2, 'the row the append-only tests try to change')`, ledgerAdminA, ledgerUserA)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return reserveID
}
