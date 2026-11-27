//go:build harness

package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// userSettingsObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0020's down: before it, the two tables (the per-user settings and their append-only history), the history trigger and
// its function, the four refusing triggers, the unit flag needs_vendor (and m1_vendor holding it), the two CHECKs on the frozen key of platform.jobs and the reserve check that knows the opt-out are there, and the
// rules bite (lifecycleUserSettings); after it none of them is, the older reserve check (platform_0013's text: both of its refusals) is back, and a frozen key is no longer judged.
func userSettingsObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the two tables platform.user_settings and platform.user_settings_history", 2,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text IN ('user_settings', 'user_settings_history')")
	expect("the history writer and its function", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'user_settings_record_history' AND prosecdef")
	expect("the five triggers of 0020 (the history writer, the append-only pair of the history, the refusals of a delete and a truncate of a setting)", 5,
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text IN ('user_settings_record_history', 'user_settings_history_append_only', 'user_settings_history_no_truncate', 'user_settings_no_delete', 'user_settings_no_truncate')")
	expect("the column platform.unit_costs.needs_vendor", 1,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema = 'platform' AND table_name = 'unit_costs' AND column_name = 'needs_vendor' AND data_type = 'boolean' AND is_nullable = 'NO'")
	if present { // (the column does not exist without the migration: a statement that names it would be refused at parse time)
		if got := ledgerInt64(t, c, "SELECT count(*) FROM platform.unit_costs WHERE needs_vendor AND unit = 'm1_vendor'"); got != 1 {
			t.Fatalf("%s: m1_vendor does not hold the flag needs_vendor (found %d)", when, got)
		}
		if got := ledgerInt64(t, c, "SELECT count(*) FROM platform.unit_costs WHERE needs_vendor AND unit <> 'm1_vendor'"); got != 0 {
			t.Fatalf("%s: %d unit(s) other than m1_vendor hold the flag needs_vendor, want none", when, got)
		}
	}
	expect("the two CHECKs on the frozen key of platform.jobs", 2,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.jobs'::regclass AND contype = 'c' AND conname::text IN ('jobs_vendor_opt_out_is_boolean', 'jobs_vendor_opt_out_only_on_module_one')")
	expect("the reserve check that holds the opted-out wall", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'ledger_check_reserve' AND prosrc LIKE '%reserve_no_vendor_unit_for_an_opted_out_job%'")
	// whatever the height, the reserve check holds both of its older refusals (the down puts platform_0013's text back, it does not drop the function)
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'ledger_check_reserve' AND prosrc LIKE '%reserve_needs_the_complete_snapshot%' AND prosrc LIKE '%reserve_at_the_dearest_price%' AND prosrc LIKE '%reachable%'"); got != 1 {
		t.Fatalf("%s: platform.ledger_check_reserve is not there with its older refusals (found %d)", when, got)
	}
	lifecycleUserSettings(t, c, present, when)
}

// lifecycleUserSettings is the behavioural half of userSettingsObjects, in a transaction that is ALWAYS rolled back. With platform_0020 a module 1 job may carry the frozen key as a JSON boolean, a send job
// may not carry it, and a key that is not a boolean is refused (each for its named constraint); a setting that changes writes one history row (the user as the actor) and one that does not writes none; and a reserve for an
// opted-out job whose snapshot holds the vendor unit is refused for its own reason. Without it the very same statements are accepted, and the table is not there.
func lifecycleUserSettings(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	const set = "UPDATE platform.jobs SET frozen_settings = $2::jsonb WHERE id = $1"
	const optedOut = `{"settings":{"unknown_choice":"","vendor_opt_out":true}}`
	if !enforced {
		ledgerExpectOK(t, tx, when+": a key that is not a boolean on a module 1 job (nothing judges it any more)", set, ledgerJobA0, `{"settings":{"vendor_opt_out":"yes"}}`)
		ledgerExpectOK(t, tx, when+": the key on a send job", set, ledgerJobA2, optedOut)
		ledgerExpectRefused(t, tx, when+": naming the table that is gone", "42P01", ledgerWhy{Message: "user_settings"}, "SELECT count(*) FROM platform.user_settings")
		return
	}
	ledgerExpectOK(t, tx, when+": the key as a boolean on a module 1 job", set, ledgerJobA0, optedOut)
	ledgerExpectRefused(t, tx, when+": a key that is a string", "23514", ledgerWhy{Constraint: "jobs_vendor_opt_out_is_boolean"}, set, ledgerJobA0, `{"settings":{"vendor_opt_out":"yes"}}`)
	ledgerExpectRefused(t, tx, when+": a key that is null", "23514", ledgerWhy{Constraint: "jobs_vendor_opt_out_is_boolean"}, set, ledgerJobA0, `{"settings":{"vendor_opt_out":null}}`)
	ledgerExpectRefused(t, tx, when+": the key on a send job", "23514", ledgerWhy{Constraint: "jobs_vendor_opt_out_only_on_module_one"}, set, ledgerJobA2, optedOut)
	ledgerExpectOK(t, tx, when+": a job with no key (every job of before the migration)", set, ledgerJobA0, `{"settings":{"unknown_choice":""}}`)

	// the history: a first row that opts out is one change, the same value again none, and the way back one more
	ledgerMust(t, tx, "INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ($1, true)", ledgerUserA)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.user_settings_history WHERE user_id = $1 AND old_value = false AND new_value = true AND actor_user_id = $1", ledgerUserA); got != 1 {
		t.Fatalf("%s: %d history row(s) after the first row that opts out, want one (false -> true, the user as the actor)", when, got)
	}
	ledgerMust(t, tx, "UPDATE platform.user_settings SET vendor_fallback_opt_out = true, updated_at = now() WHERE user_id = $1", ledgerUserA)
	ledgerMust(t, tx, "UPDATE platform.user_settings SET vendor_fallback_opt_out = false WHERE user_id = $1", ledgerUserA)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.user_settings_history WHERE user_id = $1", ledgerUserA); got != 2 {
		t.Fatalf("%s: %d history row(s) after a repeat and a way back, want two (the repeat writes none)", when, got)
	}
	ledgerMust(t, tx, "INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ($1, false)", ledgerUserB)
	if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.user_settings_history WHERE user_id = $1", ledgerUserB); got != 0 {
		t.Fatalf("%s: a first row that states the default (opted in) wrote %d history row(s), want none", when, got)
	}
}

// TestRunnerLifecycle_Down0020RefusesWhileACustomersChoiceWouldBeLost: platform_0020's down says no (SQLSTATE 55000, naming how many) while a user is opted out (dropping the setting would opt them back in to the
// paid vendor without their say) and while a job that is opted out has not ended (its snapshot lacks the vendor unit, which the older reserve rule would not accept). The database is left exactly as it was (still
// at 0020). The control is the same down once nothing is left of either (the user opts back in, the job's key is gone): it passes and the tables are gone.
// It fails if either guard of the down is missing, if it counts wrongly, or if a refused down changes anything.
func TestRunnerLifecycle_Down0020RefusesWhileACustomersChoiceWouldBeLost(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0020" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0020 after other migrations", chain)
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

	refused := func(what, want string) {
		t.Helper()
		res := runMigrator(t, envDown, "down", "0020")
		mustExit(t, res, 1, "down 0020 with "+what)
		mustContain(t, res.Stderr, want, "down 0020 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0020 with "+what)
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0020 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if n := ledgerInt64(t, admin, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relname::text = 'user_settings'"); n != 1 {
			t.Fatalf("a refused down 0020 (%s) left %d settings table(s), want 1", what, n)
		}
	}

	// a user who is opted out
	ledgerMust(t, admin, "INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ($1, true)", ledgerUserA)
	refused("a user who is opted out", fmt.Sprintf("platform_0020 down: %d user(s) are opted out", 1))
	// the user opts back in: that alone does not let the down through while a job that is opted out has not ended
	ledgerMust(t, admin, "UPDATE platform.user_settings SET vendor_fallback_opt_out = false WHERE user_id = $1", ledgerUserA)
	ledgerMust(t, admin, `UPDATE platform.jobs SET frozen_settings = '{"settings":{"unknown_choice":"","vendor_opt_out":true}}' WHERE id = $1`, ledgerJobA0)
	refused("a draft job that is opted out", fmt.Sprintf("platform_0020 down: %d job(s) opted out", 1))

	// the control: with nothing of either left the same down passes, and what it built is gone
	ledgerMust(t, admin, `UPDATE platform.jobs SET frozen_settings = '{"settings":{"unknown_choice":""}}' WHERE id = $1`, ledgerJobA0)
	mustExit(t, runMigrator(t, envDown, "down", "0020"), 0, "down 0020 once no customer's choice is left")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0020 that passed: applied = %v, want %v", got, chain[:at])
	}
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relname::text IN ('user_settings', 'user_settings_history')"); n != 0 {
		t.Fatalf("after the down 0020 that passed %d settings table(s) are left", n)
	}
}
