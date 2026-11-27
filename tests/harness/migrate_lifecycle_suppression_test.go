//go:build harness

package harness

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// suppressionRemovalObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0022's down: before it, the history table, the trigger of the suppression list and its SECURITY DEFINER function, the two
// refusing triggers of the history, the paging index, platform_app's DELETE on the list and the table comment that says how a row goes are there, and the rules bite (lifecycleSuppressionRemoval); after it none
// of them is, platform_app is refused the DELETE, and the comment is platform_0009's again.
func suppressionRemovalObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the table platform.suppression_removals", 1,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text = 'suppression_removals'")
	expect("the history writer's function, a SECURITY DEFINER", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'suppressions_record_removal' AND prosecdef")
	expect("the three triggers of 0022 (the history writer on the list and the append-only pair of the history)", 3,
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text IN ('suppressions_record_removal', 'suppression_removals_append_only', 'suppression_removals_no_truncate')")
	expect("the paging index of the list", 1,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'i' AND relname::text = 'suppressions_user_created_idx'")
	expect("platform_app's DELETE on platform.suppressions", 1,
		"SELECT CASE WHEN has_table_privilege('platform_app', 'platform.suppressions', 'DELETE') THEN 1 ELSE 0 END")
	expect("the table comment that says every removal is recorded", 1,
		"SELECT count(*) FROM (SELECT 1) x WHERE obj_description('platform.suppressions'::regclass, 'pg_class') LIKE '%suppression_removals%'")
	// whatever the height, the list's own terms are there (the down puts the comment back, it does not drop the table): the grants of platform_0003 and the CHECKs
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.suppressions'::regclass AND contype = 'c' AND conname::text IN ('suppressions_email_norm_v1', 'suppressions_source_vocabulary', 'suppressions_not_the_system_user')"); got != 3 {
		t.Fatalf("%s: the list holds %d of its three CHECKs, want 3", when, got)
	}
	if !ledgerBool(t, c, "SELECT has_table_privilege('platform_app', 'platform.suppressions', 'INSERT') AND NOT has_table_privilege('platform_app', 'platform.suppressions', 'UPDATE')") {
		t.Fatalf("%s: platform_app does not hold exactly INSERT and no UPDATE on the list", when)
	}
	lifecycleSuppressionRemoval(t, c, present, when)
}

// lifecycleSuppressionRemoval is the behavioural half of suppressionRemovalObjects, in a transaction that is ALWAYS rolled back. With platform_0022 platform_app removes a row of the list and the database writes the
// history row (the entry's address, source and creation time) by trigger; without it platform_app is refused the removal and the history table is not there.
func lifecycleSuppressionRemoval(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// a user of its own, fresh per call: the database may already hold the ledger fixtures (a test that committed them, then calls this at its own height), and a second ledgerSeed would collide with them
	u := ledgerString(t, tx, "SELECT gen_random_uuid()::text")
	mail := "sprm-" + u + "@example.test"
	ledgerMust(t, tx, "INSERT INTO auth.users (id, email) VALUES ($1, $2)", u, mail)
	ledgerMust(t, tx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", u, mail)
	ledgerMust(t, tx, "INSERT INTO platform.suppressions (user_id, email, source, created_at) VALUES ($1, 'life@example.com', 'unsubscribe', timestamptz '2026-02-03 04:05:06+00')", u)
	ledgerSwitchRole(t, tx, "platform_app")
	const del = "DELETE FROM platform.suppressions WHERE user_id = $1 AND email = 'life@example.com'"
	if !enforced {
		ledgerExpectRefused(t, tx, when+": platform_app removing a row (the privilege is gone)", "42501", ledgerWhy{Message: "permission denied for table suppressions"}, del, u)
		ledgerResetRole(t, tx)
		ledgerExpectRefused(t, tx, when+": the history (the table is gone)", "42P01", ledgerWhy{Message: "suppression_removals"}, "SELECT count(*) FROM platform.suppression_removals")
		return
	}
	ledgerExpectOK(t, tx, when+": platform_app removing a row of the list", del, u)
	ledgerResetRole(t, tx)
	if got := ledgerInt64(t, tx, `SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1 AND email = 'life@example.com' AND source = 'unsubscribe'
		AND entry_created_at = timestamptz '2026-02-03 04:05:06+00'`, u); got != 1 {
		t.Fatalf("%s: %d history row(s) with the entry's own source and time after a removal, want one", when, got)
	}
	// a removal that finds nothing writes nothing
	ledgerSwitchRole(t, tx, "platform_app")
	ledgerExpectOK(t, tx, when+": platform_app removing a row that is not there", del, u)
	ledgerResetRole(t, tx)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.suppression_removals WHERE user_id = $1", u); got != 1 {
		t.Fatalf("%s: a removal that removed nothing left %d history row(s), want still one", when, got)
	}
}

// TestRunnerLifecycle_Down0022RefusesWhileTheHistoryHoldsARow: platform_0022's down says no (SQLSTATE 55000, naming how many) while a removal of a suppression is on record (dropping the history would erase the only
// trace of it), and the database is left exactly as it was (still at 0022, the history and its row still there). The history is append-only for everybody, so the refusal has no moving-it-away control of its own:
// the control is the same down on a database where nobody removed anything, which passes and drops what 0022 built.
// It fails if the guard of the down is missing, if it counts wrongly, if it drops what it refused to, or if it refuses an empty history.
func TestRunnerLifecycle_Down0022RefusesWhileTheHistoryHoldsARow(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0022" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0022 after other migrations", chain)
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

	// one removal on record: a row of the list is added and removed (by the superuser: the trigger writes the history for every writer)
	ledgerMust(t, admin, "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, 'on-record@example.com', 'manual')", ledgerUserA)
	ledgerMust(t, admin, "DELETE FROM platform.suppressions WHERE user_id = $1", ledgerUserA)
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.suppression_removals"); n != 1 {
		t.Fatalf("the premise of the test: %d history row(s), want 1", n)
	}
	res := runMigrator(t, envDown, "down", "0022")
	mustExit(t, res, 1, "down 0022 with a removal on record")
	mustContain(t, res.Stderr, "platform_0022 down: 1 removal(s)", "down 0022 with a removal on record")
	mustContain(t, res.Stderr, "55000", "down 0022 with a removal on record")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
		t.Fatalf("a refused down 0022 changed the database: applied = %v, want %v", got, chain[:at+1])
	}
	suppressionRemovalObjects(t, admin, true, "after a refused down 0022")
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.suppression_removals"); n != 1 {
		t.Fatalf("a refused down 0022 left %d history row(s), want 1", n)
	}

	// the control: a database where nobody removed anything lets the same down through, and what it built is gone
	s2, admin2 := migratedScratch(t)
	env2 := migEnv(s2.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	mustExit(t, runMigrator(t, env2, "down", "0022"), 0, "down 0022 with an empty history")
	if got := appliedVersions(t, admin2); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0022 that passed: applied = %v, want %v", got, chain[:at])
	}
	suppressionRemovalObjects(t, admin2, false, "after the down 0022 that passed")
}
