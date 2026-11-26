//go:build harness

package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The migration lifecycle of platform_0014 (the suppressed flag, owner ruling OD-3). The chain test (migrate_lifecycle_test.go) calls suppressedFlagObjects before and after the down of 0014; the
// refusal of the down while a verify job holds the flag is its own test below.

// suppressedFlagObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0014's down: before it, the column suppressed (boolean, NOT NULL, default false) on platform.job_rows and on
// platform.job_results, the three CHECKs, the row guard that keeps a flagged pending row to a verify job, the result guard that derives the flag, and platform_app's UPDATE of the rows' column (not
// platform_admin's) are there and the rules bite (lifecycleSuppressedFlag); after it none of them is, both guards are still there as platform_0012 wrote them (they do not mention the flag), and a statement that
// names the flag fails with 42703. The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func suppressedFlagObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	for _, table := range []string{"job_rows", "job_results"} {
		expect("the column platform."+table+".suppressed (boolean, NOT NULL, default false)", 1,
			"SELECT count(*) FROM information_schema.columns WHERE table_schema = 'platform' AND table_name = '"+table+"' AND column_name = 'suppressed' AND data_type = 'boolean' AND is_nullable = 'NO' AND column_default = 'false'")
	}
	expect("the three CHECKs of 0014", 3,
		"SELECT count(*) FROM pg_constraint WHERE contype = 'c' AND conname::text IN ('job_rows_suppressed_state_has_the_flag', 'job_rows_flag_only_on_listable_rows', 'job_results_suppressed_platform_outcome')")
	expect("the row guard that keeps a flagged pending row to a verify job", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'job_rows_frozen' AND prosrc LIKE '%job_rows_pending_flag_is_verify_only%'")
	expect("the result guard that derives the flag from the row", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'job_results_after_confirm' AND prosrc LIKE '%suppressed%'")
	// the two guards and their triggers are there either way (platform_0012's, or amended by platform_0014)
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text IN ('job_rows_frozen', 'job_results_after_confirm')"); got != 2 {
		t.Fatalf("%s: found %d of the two guard functions of the rows and the results, want both (the down puts platform_0012's back, it does not drop them)", when, got)
	}
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text IN ('job_rows_frozen', 'job_results_after_confirm')"); got != 2 {
		t.Fatalf("%s: found %d of the two guard triggers, want both", when, got)
	}
	if present {
		if !ledgerBool(t, c, "SELECT has_column_privilege('platform_app', 'platform.job_rows', 'suppressed', 'UPDATE') AND NOT has_column_privilege('platform_admin', 'platform.job_rows', 'suppressed', 'UPDATE') AND NOT has_table_privilege('platform_app', 'platform.job_results', 'UPDATE')") {
			t.Fatalf("%s: platform_app must update the rows' flag, platform_admin must not, and nobody updates a result", when)
		}
	}
	lifecycleSuppressedFlag(t, c, present, when)
}

// lifecycleSuppressedFlag is the behavioural half of suppressedFlagObjects, in a transaction that is ALWAYS rolled back (a transaction left open on this connection would hold the locks the next down
// needs). With platform_0014 a suppressed row with no flag is refused (23514, job_rows_suppressed_state_has_the_flag) and the same row with the flag is accepted, a pending row with the flag in a module 2
// job is refused (23514, job_rows_pending_flag_is_verify_only), and a result of a flagged row holds the flag whatever its writer said; without it the first row is accepted (the older chain has no flag)
// and a statement that names the flag is 42703.
func lifecycleSuppressedFlag(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	const noFlag = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state) VALUES ($1, $2, $3, 1, 'a@example.com', 'a@example.com', 'suppressed')"
	if !enforced {
		ledgerExpectOK(t, tx, when+": a suppressed row with no flag (the older chain has no flag to carry)", noFlag, ledgerUUID("fa", 1), ledgerJobA2, ledgerUserA)
		ledgerExpectRefused(t, tx, when+": a statement that names the flag", "42703", ledgerWhy{Message: "suppressed"}, "SELECT suppressed FROM platform.job_rows")
		return
	}
	const withFlag = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, suppressed) VALUES ($1, $2, $3, $4, $5, $5, $6, true)"
	ledgerExpectRefused(t, tx, when+": a suppressed row with no flag", "23514", ledgerWhy{Constraint: "job_rows_suppressed_state_has_the_flag"}, noFlag, ledgerUUID("fa", 1), ledgerJobA2, ledgerUserA)
	ledgerExpectOK(t, tx, when+": the same row with the flag", withFlag, ledgerUUID("fa", 1), ledgerJobA2, ledgerUserA, 1, "a@example.com", "suppressed")
	ledgerExpectRefused(t, tx, when+": a pending row with the flag in a module 2 job", "23514", ledgerWhy{Constraint: "job_rows_pending_flag_is_verify_only"}, withFlag, ledgerUUID("fa", 2), ledgerJobA2, ledgerUserA, 2, "b@example.com", "pending")
	ledgerExpectOK(t, tx, when+": a pending row with the flag in a module 1 job (verified, charged, flagged)", withFlag, ledgerUUID("fa", 3), ledgerJobA1, ledgerUserA, 1, "c@example.com", "pending")
	jobWalkTo(t, tx, ledgerJobA1, "confirmed")
	ledgerExpectOK(t, tx, when+": a result that states no flag", "INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ($1, $2, $3, 1, 'c@example.com', 'module', 'valid', 'free', 'M1-19')",
		ledgerJobA1, ledgerUserA, ledgerUUID("fa", 3))
	if got := ledgerRows(t, tx, "SELECT suppressed::text FROM platform.job_results WHERE item_ref = $1", ledgerUUID("fa", 3)); len(got) != 1 || got[0] != "true" {
		t.Fatalf("%s: the result of a flagged row holds %v, want true (derived from the row)", when, got)
	}
}

// TestRunnerLifecycle_Down0014RefusesWhileAVerifyJobHoldsTheFlag: platform_0014's down says no (SQLSTATE 55000, naming how many) while a PENDING row carries the flag or a result of the module does: that is a verify job's
// record that an address was on the user's suppression list although it was verified and charged, which the older schema cannot say, and a result is kept forever. The database is left exactly as it was
// (still at 0014, the flag still there). The control of the first refusal is the same down once the flag is cleared (a draft's list may change): it passes and the columns are gone; after an up again a
// confirmed job holds a flagged row and its flagged result, and the down is refused again (2), with no moving-it-away control of its own (a result cannot be changed: the down that passed is its control). A row in
// the state `suppressed` and the platform's own P-SUP result, which the older schema can say, never refuse it (the control of the other direction).
// It fails if the guard of the down is missing, if it counts wrongly, if it refuses what the older schema can hold, or if a refused down changes anything.
func TestRunnerLifecycle_Down0014RefusesWhileAVerifyJobHoldsTheFlag(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	env := migEnv(s.DSN, dir, nil)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0014" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0014 after other migrations", chain)
	}
	seed, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, seed)
	if err := seed.Commit(ctx); err != nil { // the migrator runs on its own connection: the rows must be committed
		t.Fatal(err)
	}
	migRollBackAbove(t, admin, envDown, chain, at)

	flagged := func() int64 {
		return ledgerInt64(t, admin, "SELECT (SELECT count(*) FROM platform.job_rows WHERE suppressed AND state = 'pending') + (SELECT count(*) FROM platform.job_results WHERE suppressed AND origin = 'module')")
	}
	refused := func(what string, n int) {
		t.Helper()
		res := runMigrator(t, envDown, "down", "0014")
		mustExit(t, res, 1, "down 0014 with "+what)
		mustContain(t, res.Stderr, fmt.Sprintf("platform_0014 down: %d flagged row(s) and result(s) of verify jobs exist", n), "down 0014 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0014 with "+what)
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0014 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if got := flagged(); got != int64(n) {
			t.Fatalf("a refused down 0014 (%s) left %d flagged row(s) and result(s), want %d", what, got, n)
		}
	}
	const row = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, suppressed) VALUES ($1, $2, $3, 1, 'a@example.com', 'a@example.com', true)"
	rowID := ledgerUUID("f9", 1)
	ledgerMust(t, admin, row, rowID, ledgerJobA1, ledgerUserA) // a verify job's suppressed address: pending, flagged
	refused("a flagged pending row", 1)

	ledgerMust(t, admin, "UPDATE platform.job_rows SET suppressed = false WHERE id = $1", rowID) // the control's setup: a draft's list may change
	mustExit(t, runMigrator(t, envDown, "down", "0014"), 0, "down 0014 once nothing is flagged")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0014 that passed: applied = %v, want %v", got, chain[:at])
	}
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'platform' AND column_name = 'suppressed'"); n != 0 {
		t.Fatalf("after the down 0014 that passed %d column(s) named suppressed are still there", n)
	}

	// up again over the rows that were left behind: a row in the state `suppressed` and its P-SUP result are what the older schema can say, so they never hold the down back
	mustExit(t, runMigrator(t, env, "up"), 0, "up after the down 0014")
	migRollBackAbove(t, admin, envDown, chain, at)
	ledgerMust(t, admin, "UPDATE platform.job_rows SET suppressed = true WHERE id = $1", rowID)
	sendRow := ledgerUUID("f9", 2)
	ledgerMust(t, admin, "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, suppressed) VALUES ($1, $2, $3, 1, 'b@example.com', 'b@example.com', 'suppressed', true)", sendRow, ledgerJobA2, ledgerUserA)
	jobWalkTo(t, admin, ledgerJobA1, "confirmed")
	jobWalkTo(t, admin, ledgerJobA2, "confirmed")
	ledgerMust(t, admin, "INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ($1, $2, $3, 1, 'a@example.com', 'module', 'valid', 'free', 'M1-19')", ledgerJobA1, ledgerUserA, rowID)
	ledgerMust(t, admin, "INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ($1, $2, $3, 1, 'b@example.com', 'platform', 'suppressed', 'free', 'P-SUP')", ledgerJobA2, ledgerUserA, sendRow)
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_rows WHERE suppressed") + ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_results WHERE suppressed"); n != 4 {
		t.Fatalf("the instrument: %d flagged rows and results in all, want 4 (a verify job's row and result, a send job's suppressed row and its P-SUP result)", n)
	}
	refused("a flagged pending row and its flagged result (the send job's suppressed row and its P-SUP result are not counted)", 2)
}
