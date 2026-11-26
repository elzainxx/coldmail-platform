//go:build harness

package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type roleShape struct {
	login, super, createRole, createDB, replication, bypassRLS bool
}

func assertRole(t *testing.T, c *pgx.Conn, name string, want roleShape) {
	t.Helper()
	var got roleShape
	err := c.QueryRow(testCtx(t),
		`SELECT rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls FROM pg_roles WHERE rolname = $1`, name).
		Scan(&got.login, &got.super, &got.createRole, &got.createDB, &got.replication, &got.bypassRLS)
	if err != nil {
		t.Fatalf("role %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("role %s: login/super/createrole/createdb/replication/bypassrls = %+v, want %+v", name, got, want)
	}
}

func schemaOwner(t *testing.T, c *pgx.Conn, schema string) string {
	t.Helper()
	var owner string
	if err := c.QueryRow(testCtx(t), "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1", schema).Scan(&owner); err != nil {
		t.Fatalf("owner of schema %s: %v", schema, err)
	}
	return owner
}

func tableOwner(t *testing.T, c *pgx.Conn, schema, table string) string {
	t.Helper()
	var owner string
	if err := c.QueryRow(testCtx(t), "SELECT tableowner FROM pg_tables WHERE schemaname = $1 AND tablename = $2", schema, table).Scan(&owner); err != nil {
		t.Fatalf("owner of %s.%s: %v", schema, table, err)
	}
	return owner
}

// assertDesignedState is what a FULLY migrated database must look like, whichever way it got there: the three roles, the schema, and the ownership of everything the platform created -
// every table, view, sequence, index and function in schema platform, and the platform's two tables (with their indexes) in schema public, is owned by platform_owner and by nobody else
// (a migrator that left an object to itself would make it undroppable by the owner and invisible to the grants). The relations of schema platform are exactly the declared ones, and the
// platform_app login starts in UTC.
func assertDesignedState(t *testing.T, c *pgx.Conn, what string) {
	t.Helper()
	assertRole(t, c, "platform_owner", roleShape{})
	assertRole(t, c, "platform_app", roleShape{login: true})
	assertRole(t, c, "platform_admin", roleShape{})
	if got := schemaOwner(t, c, "platform"); got != "platform_owner" {
		t.Fatalf("%s: schema platform is owned by %s, want platform_owner", what, got)
	}
	if got := tableOwner(t, c, "platform", "schema_migrations"); got != "platform_owner" {
		t.Fatalf("%s: platform.schema_migrations is owned by %s, want platform_owner", what, got)
	}

	objs := platformObjects(t, c)
	requireKinds(t, objs, what+": schema platform", "table", "view", "sequence", "index", "function") // the instrument matched something of every kind it checks
	requireOwnedBy(t, objs, "platform_owner", what+": schema platform")
	if got, want := namesOfKind(objs, "table", "view"), sortedClassNames(declaredClasses); !sameStrings(got, want) {
		t.Fatalf("%s: the tables and views of schema platform are %v, the declared ones are %v", what, got, want)
	}
	pub := publicPlatformObjects(t, c)
	requireKinds(t, pub, what+": the platform's tables in schema public", "table", "index")
	if got, want := namesOfKind(pub, "table"), []string{"job_items", "users"}; !sameStrings(got, want) {
		t.Fatalf("%s: the platform's tables in schema public are %v, want %v", what, got, want)
	}
	requireOwnedBy(t, pub, "platform_owner", what+": the platform's tables in schema public")

	// "The platform_app session timezone is UTC (role setting)": locked decision 3, made by platform_0001.
	if got, err := rolesTimezoneSetting(testCtx(t), c, "platform_app"); err != nil || got != "UTC" {
		t.Fatalf("%s: the stored timezone setting of platform_app is %q (err %v), want UTC", what, got, err)
	}
}

// What each migration of the chain adds to schema platform (relations only: tables and views), so that its down can be checked to remove exactly that. 0001 adds only public.users and
// public.job_items and 0000 the schema itself. A new migration needs a line here, in downStepBefore and downStepAfter; the lifecycle test fails until it has them.
var chainRelations = map[string][]string{
	"0000": {"schema_migrations"},
	"0002": {"admins", "balance_drift", "balances", "job_unit_prices", "jobs", "ledger", "m3_entitlements", "modules", "reserve_overdrawn", "unit_cost_history", "unit_costs"},
	"0003": {"admin_audit", "suppressions"},
	// platform_0004 adds one view (listable_users). It also drops and re-creates balance_drift (listed under 0002, where it first appears), so its relations are unchanged.
	"0004": {"listable_users"},
	"0005": {"reserves"},
	"0006": {"m3_sync"},
	// platform_0007 adds no relation: two trigger functions and their two triggers (the snapshot price check on job_unit_prices, the terminal-state rule on jobs)
	"0007": {},
	// platform_0008 adds no relation: two columns of platform.m3_sync (observed_version, m3_version) and a replacement of its convergence CHECK
	"0008": {},
	// platform_0009 adds no relation: it replaces the CHECK suppressions_email_norm_v1 of platform.suppressions (module 1's syntax layer and IDNA's hyphen rule)
	"0009": {},
	// platform_0010 adds no relation: one nullable column of public.job_items (rendered_text, the optional plain-text part)
	"0010": {},
	// platform_0011 adds two relations: platform.job_transitions (the legal moves of a job, as data) and platform.job_events (the history of every job)
	"0011": {"job_events", "job_transitions"},
	// platform_0012 adds a job's list and its results (tables) and the proof view job_settlement_drift
	"0012": {"job_results", "job_rows", "job_settlement_drift"},
	// platform_0013 adds the history of the reachability flag (the flag itself is three columns of platform.unit_costs)
	"0013": {"unit_reachability_history"},
	// platform_0014 adds no relation: the column suppressed on platform.job_rows and on platform.job_results, three CHECKs and the amended text of two guard functions
	"0014": {},
	// platform_0015 adds no relation: three annotation columns of platform.jobs (next_poll_at, idle_polls, items_ensured_at), a CHECK, the trigger jobs_engine_schedule and the comment of public.job_items.status
	"0015": {},
	// platform_0016 adds no relation: the capability seeds of platform.modules (data), two rows of platform.job_transitions (data) and the amended text of platform.jobs_transition_guard
	"0016": {},
}

// relationsThrough is the sorted union of what the listed versions add.
func relationsThrough(versions ...string) []string {
	var out []string
	for _, v := range versions {
		out = append(out, chainRelations[v]...)
	}
	sort.Strings(out)
	return out
}

func requireRelations(t *testing.T, c *pgx.Conn, want []string, when string) {
	t.Helper()
	objs := platformObjects(t, c)
	if got := namesOfKind(objs, "table", "view"); !sameStrings(got, want) {
		t.Fatalf("%s: the tables and views of schema platform are %v, want %v\nobjects: %s", when, got, want, describeObjects(objs))
	}
}

func requirePublicTables(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	for _, r := range []string{"public.users", "public.job_items"} {
		if got := relationExists(t, c, r); got != present {
			t.Fatalf("%s: %s exists = %v, want %v", when, r, got, present)
		}
	}
	if !present {
		if left := publicPlatformObjects(t, c); len(left) != 0 {
			t.Fatalf("%s: the platform's objects in schema public are not gone: %s", when, describeObjects(left))
		}
	}
}

// downStepBefore is the CONTROL of each down step: what the down is about to remove is there, so that the absence checked afterwards is an effect of the down and not of a test that never
// looked. It also pins what the chain holds at that height.
func downStepBefore(t *testing.T, c *pgx.Conn, v string) {
	t.Helper()
	when := "before down " + v
	switch v {
	case "0016":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015", "0016"), when)
		pauseCancelObjects(t, c, true, when)
		engineScheduleObjects(t, c, true, when) // the migration below is whole
	case "0015":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015"), when)
		engineScheduleObjects(t, c, true, when)
		suppressedFlagObjects(t, c, true, when) // the migration below is whole
	case "0014":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014"), when)
		suppressedFlagObjects(t, c, true, when)
		reachableUnitsObjects(t, c, true, when) // the migration below is whole
	case "0013":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013"), when)
		reachableUnitsObjects(t, c, true, when)
		jobRowsObjects(t, c, true, when) // the migration below is whole
	case "0012":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012"), when)
		jobRowsObjects(t, c, true, when)
		jobLifecycleObjects(t, c, true, when) // the migration below is whole
	case "0011":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011"), when)
		jobLifecycleObjects(t, c, true, when)
		jobItemsTextObjects(t, c, true, when) // the migration below is whole
		suppressionSyntaxObjects(t, c, true, when)
	case "0010":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010"), when)
		jobItemsTextObjects(t, c, true, when)
		suppressionSyntaxObjects(t, c, true, when)
	case "0009":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009"), when)
		suppressionSyntaxObjects(t, c, true, when)
		observedVersionObjects(t, c, true, when)
	case "0008":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008"), when)
		observedVersionObjects(t, c, true, when)
		snapshotTerminalObjects(t, c, true, when) // the migration below is whole (entitlementFlowObjects is not asked here: platform_0008 replaced the one CHECK of 0006 it pins)
	case "0007":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007"), when)
		snapshotTerminalObjects(t, c, true, when)
		entitlementFlowObjects(t, c, true, when) // the migration below is whole
	case "0006":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006"), when)
		entitlementFlowObjects(t, c, true, when)
		moneyCoreObjects(t, c, true, when) // the migration below is whole
	case "0005":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005"), when)
		moneyCoreObjects(t, c, true, when)
	case "0004":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004"), when)
		phase2Objects(t, c, true, when)
	case "0003":
		requireRelations(t, c, relationsThrough("0000", "0002", "0003"), when)
	case "0002":
		requireRelations(t, c, relationsThrough("0000", "0002"), when)
		objs := platformObjects(t, c)
		if len(namesOfKind(objs, "function")) == 0 || len(namesOfKind(objs, "sequence")) == 0 {
			t.Fatalf("%s: schema platform has no functions or no sequences, so the check that the down removes them would be vacuous: %s", when, describeObjects(objs))
		}
	case "0001":
		requirePublicTables(t, c, true, when)
		if got, err := rolesTimezoneSetting(testCtx(t), c, "platform_app"); err != nil || got != "UTC" {
			t.Fatalf("%s: platform_app's timezone setting is %q (err %v), want UTC", when, got, err)
		}
	case "0000":
		if !schemaExists(t, c, "platform") {
			t.Fatalf("%s: schema platform does not exist", when)
		}
	default:
		t.Fatalf("the lifecycle test has no before-the-down control for migration %s: add one to downStepBefore (and an assertion to downStepAfter)", v)
	}
}

// downStepAfter is what each down must leave: the objects of its migration gone, and everything below it untouched.
func downStepAfter(t *testing.T, c *pgx.Conn, v string) {
	t.Helper()
	when := "after down " + v
	switch v {
	case "0016":
		// the capabilities are platform_0011's seeds again (no module pauses one job, only modules 2 and 3 cancel one), the two moves paused -> cancelling and paused -> completing are gone from the table, the guard is
		// platform_0011's text (a job paused before its start is no longer judged by walls that only existed for the new moves) and everything of 0015 and below is untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015"), when)
		pauseCancelObjects(t, c, false, when)
		engineScheduleObjects(t, c, true, when)
		suppressedFlagObjects(t, c, true, when)
		jobLifecycleObjects(t, c, true, when)
	case "0015":
		// the three columns, the CHECK, the trigger and its function are gone and the comment of public.job_items.status is the one of platform_0001 again; the suppressed flag of 0014 and everything below is untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014"), when)
		engineScheduleObjects(t, c, false, when)
		suppressedFlagObjects(t, c, true, when)
		jobLifecycleObjects(t, c, true, when)
	case "0014":
		// the column on the rows and on the results, the three CHECKs and the amended guards are gone (the guards are platform_0012's text again); the reachable units of 0013 and the list and results of 0012 are untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013"), when)
		suppressedFlagObjects(t, c, false, when)
		reachableUnitsObjects(t, c, true, when)
		jobRowsObjects(t, c, true, when)
	case "0013":
		// the flag, its history, its writer and the two checks are gone and the older reserve check (every unit of the module is the complete snapshot) is back; the list and the results of 0012 are untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012"), when)
		reachableUnitsObjects(t, c, false, when)
		jobRowsObjects(t, c, true, when)
		jobLifecycleObjects(t, c, true, when)
	case "0012":
		// the proof view, the two tables, their triggers and the two guards are gone; the lifecycle of 0011 is untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011"), when)
		jobRowsObjects(t, c, false, when)
		jobLifecycleObjects(t, c, true, when)
	case "0011":
		// the lifecycle is gone (the two tables, the twelve columns and two capability columns, the function, the guard, the slot index of platform_0004 is back and a previewed status is refused again by the
		// CHECK of platform_0002); jobs_guard and the terminal rule are the ones of 0002 and 0007 (snapshotTerminalObjects reads them), and everything of 0010 and below is still there
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010"), when)
		jobLifecycleObjects(t, c, false, when)
		snapshotTerminalObjects(t, c, true, when)
		jobItemsTextObjects(t, c, true, when)
		suppressionSyntaxObjects(t, c, true, when)
		observedVersionObjects(t, c, true, when)
	case "0010":
		// the column is gone; platform_0009's CHECK and everything below it are still there
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009"), when)
		jobItemsTextObjects(t, c, false, when)
		suppressionSyntaxObjects(t, c, true, when)
		observedVersionObjects(t, c, true, when)
	case "0009":
		// the CHECK of platform_0003 is back: a local part of 65 bytes and a domain with no dot are accepted again; everything of 0008 and below is still there
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007", "0008"), when)
		suppressionSyntaxObjects(t, c, false, when)
		observedVersionObjects(t, c, true, when)
		snapshotTerminalObjects(t, c, true, when)
		moneyCoreObjects(t, c, true, when)
	case "0008":
		// the two columns and the new CHECK are gone, the platform_0006 CHECK is back (the entitlement flow is whole again, which entitlementFlowObjects proves), the same stale-looking observation is accepted again; 0007 and below are untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006", "0007"), when)
		observedVersionObjects(t, c, false, when)
		snapshotTerminalObjects(t, c, true, when)
		entitlementFlowObjects(t, c, true, when)
		moneyCoreObjects(t, c, true, when)
	case "0007":
		// the two functions and the two triggers are gone (jobs_guard, which 0007 never replaced, is still the guard of 0002), the forged price is accepted again and a succeeded job moves back; everything of
		// 0006 and below is still there
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005", "0006"), when)
		snapshotTerminalObjects(t, c, false, when)
		entitlementFlowObjects(t, c, true, when)
		moneyCoreObjects(t, c, true, when)
	case "0006":
		// the delivery table, its writer, the entitlement writer, the date column are gone; the privileges of the application role on the entitlements are back; everything of 0005 and below is still there
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004", "0005"), when)
		entitlementFlowObjects(t, c, false, when)
		moneyCoreObjects(t, c, true, when)
	case "0005":
		// the reserves table, the four triggers and functions (the created_at stamp among them), the three unique indexes, the two CHECKs and the item_ref column are gone and the older money rules are back; 0004 is untouched
		requireRelations(t, c, relationsThrough("0000", "0002", "0003", "0004"), when)
		moneyCoreObjects(t, c, false, when)
		phase2Objects(t, c, true, when)
	case "0004":
		// the view is gone and everything of 0002 and 0003 is still there (balance_drift was dropped and re-created by 0004's up and its down: it must be back)
		requireRelations(t, c, relationsThrough("0000", "0002", "0003"), when)
		phase2Objects(t, c, false, when)
		requirePublicTables(t, c, true, when)
	case "0003":
		requireRelations(t, c, relationsThrough("0000", "0002"), when)
		for _, r := range []string{"platform.suppressions", "platform.admin_audit"} {
			if relationExists(t, c, r) {
				t.Fatalf("%s: %s still exists", when, r)
			}
		}
		requirePublicTables(t, c, true, when) // 0001 is below: untouched
	case "0002":
		// No platform table except the tracking table remains, and no function or sequence either.
		requireRelations(t, c, relationsThrough("0000"), when)
		objs := platformObjects(t, c)
		if fn, seq := namesOfKind(objs, "function"), namesOfKind(objs, "sequence"); len(fn) != 0 || len(seq) != 0 {
			t.Fatalf("%s: functions %v and sequences %v are left in schema platform: %s", when, fn, seq, describeObjects(objs))
		}
		requirePublicTables(t, c, true, when)
	case "0001":
		requirePublicTables(t, c, false, when)
		if got, err := rolesTimezoneSetting(testCtx(t), c, "platform_app"); err != nil || got != "" {
			t.Fatalf("%s: platform_app still has the timezone setting %q (err %v): the down must reset what the up set", when, got, err)
		}
		requireRelations(t, c, relationsThrough("0000"), when)
	case "0000":
		if schemaExists(t, c, "platform") {
			t.Fatalf("%s: the platform schema still exists", when)
		}
	default:
		t.Fatalf("the lifecycle test has no after-the-down assertion for migration %s: add one to downStepAfter (and a control to downStepBefore)", v)
	}
}

// phase2Objects is the CONTROL (present) and the ASSERTION (absent) of platform_0004's down: before it, everything the migration adds is there, what it replaced is gone and an adjustment is
// accepted; after it, everything it added is gone, what it replaced is back as it was (the old slot index with its old predicate, the old unit_costs_guard, the old vocabulary, the drift
// view readable by both roles) and an adjustment is refused by the vocabulary CHECK. The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func phase2Objects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	want := int64(0)
	if present {
		want = 1
	}
	expect := func(what string, perItem int64, sql string, args ...any) {
		t.Helper()
		if got := ledgerInt64(t, c, sql, args...); got != want*perItem {
			t.Fatalf("%s: %s: found %d, want %d", when, what, got, want*perItem)
		}
	}
	expectOpposite := func(what string, sql string, args ...any) {
		t.Helper()
		if got := ledgerInt64(t, c, sql, args...); got != 1-want {
			t.Fatalf("%s: %s: found %d, want %d", when, what, got, 1-want)
		}
	}
	funcs := []string{"require_active_admin", "ledger_require_active_admin", "admin_audit_require_active_admin", "m3_entitlements_require_active_admin", "ledger_audit_adjustment"}
	triggers := []string{"ledger_require_active_admin", "ledger_audit_adjustment", "admin_audit_require_active_admin", "m3_entitlements_require_active_admin"}
	checks := []string{"balances_not_the_system_user", "ledger_not_the_system_user", "m3_entitlements_not_the_system_user", "suppressions_not_the_system_user", "ledger_shape_adjustment"}

	expect("the view platform.listable_users", 1, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'v' AND relname::text = 'listable_users'")
	expect("the slot index jobs_one_active_per_user_module", 1, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'i' AND relname::text = 'jobs_one_active_per_user_module'")
	expectOpposite("the OLD slot index jobs_one_executing_per_user_module", "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'i' AND relname::text = 'jobs_one_executing_per_user_module'")
	if !present {
		if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname = 'jobs_one_executing_per_user_module' AND indexdef LIKE '%running%' AND indexdef LIKE '%cancelling%' AND indexdef NOT LIKE '%scheduled%'"); got != 1 {
			t.Fatalf("%s: the old slot index is not back with its old predicate (running, paused, cancelling; no scheduled): %d match", when, got)
		}
	}
	expect("the functions of 0004 (the admin check, its three trigger functions, the audit-row writer)", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the triggers of 0004 (ledger x2, admin_audit, m3_entitlements)", int64(len(triggers)),
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text = ANY($1::text[])", triggers)
	expect("the CHECKs of 0004 (four system-user checks and ledger_shape_adjustment)", int64(len(checks)),
		"SELECT count(*) FROM pg_constraint WHERE conname::text = ANY($1::text[])", checks)
	expect("the vocabulary CHECK naming the adjustment kind", 1,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.ledger'::regclass AND conname::text = 'ledger_kind_vocabulary' AND pg_get_constraintdef(oid) LIKE '%adjustment%'")
	expect("unit_costs_guard calling the admin check", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'unit_costs_guard' AND prosrc LIKE '%require_active_admin%'")

	// what 0004 drops and re-creates is there either way, with the grants of the roles
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'v' AND relname::text = 'balance_drift'"); got != 1 {
		t.Fatalf("%s: platform.balance_drift: found %d, want 1", when, got)
	}
	for _, role := range []string{"platform_app", "platform_admin"} {
		if !ledgerBool(t, c, "SELECT has_table_privilege($1::text, 'platform.balance_drift', 'SELECT')", role) {
			t.Fatalf("%s: %s cannot SELECT platform.balance_drift: the view was re-created without its grants", when, role)
		}
	}
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.ledger'::regclass AND conname::text IN ('ledger_kind_vocabulary', 'ledger_module_metering_fk')"); got != 2 {
		t.Fatalf("%s: found %d of the ledger's vocabulary CHECK and module-metering key, want both", when, got)
	}
	lifecycleAdjustment(t, c, present, when)
}

// lifecycleAdjustment is the behavioural half of phase2Objects: in a transaction that is ALWAYS rolled back (a transaction left open on this connection would hold the locks the next
// down needs), one funded user and one adjustment. With platform_0004 the adjustment is accepted; without it the kind is not in the vocabulary and the row is refused for that reason.
func lifecycleAdjustment(t *testing.T, c *pgx.Conn, accepted bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	ledgerFund(t, tx, ledgerUserA, "m1", 5)
	adj := ledgerAdjustmentCols(ledgerUserA, "m1", 1)
	if accepted {
		ledgerOKRow(t, tx, when+": an adjustment (the kind exists)", adj)
		ledgerRequireBalance(t, tx, when+": after the adjustment", ledgerUserA, "m1", 4, 0)
		return
	}
	ledgerRefusedCheck(t, tx, when+": an adjustment (the kind is not in the vocabulary of the older chain)", "ledger_kind_vocabulary", adj)
}

// lifecycleCounter is the shape of every presence check below: `want` is 1 when the objects must be there and 0 when they must be gone, perItem is how many of the counted catalog rows one
// listed object stands for, and the count must be exactly want * perItem - so a missing object cannot hide behind another's presence.
func lifecycleCounter(t *testing.T, c *pgx.Conn, present bool, when string) func(what string, perItem int64, sql string, args ...any) {
	return func(what string, perItem int64, sql string, args ...any) {
		t.Helper()
		want := int64(0)
		if present {
			want = 1
		}
		if got := ledgerInt64(t, c, sql, args...); got != want*perItem {
			t.Fatalf("%s: %s: found %d, want %d", when, what, got, want*perItem)
		}
	}
}

// moneyCoreObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0005's down: before it, everything the migration adds is there and the rules it states bite (a reserve
// below the dearest price, a charge without an item or of three units, a release that is not the remainder are all refused, each for its named reason, and a written created_at is overwritten); after it, the table, the four
// triggers and functions (the created_at stamp among them), the three unique indexes, the two CHECKs and the item_ref column are gone and the OLDER rules are back (the same four rows are accepted, and a written created_at is kept).
func moneyCoreObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	funcs := []string{"ledger_check_reserve", "ledger_apply_to_reserves", "ledger_check_release", "ledger_stamp_created_at"}
	indexes := []string{"ledger_one_charge_per_item", "ledger_one_reserve_per_job", "ledger_one_release_per_reserve"}
	checks := []string{"ledger_item_ref_shape", "ledger_charge_is_one_item", "reserves_not_overspent"}
	expect("the table platform.reserves", 1, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text = 'reserves'")
	expect("the functions of 0005 (the reserve check, the reserves writer, the release check, the created_at stamp)", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the triggers of 0005 on the ledger", int64(len(funcs)),
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'platform.ledger'::regclass AND tgname::text = ANY($1::text[])", funcs)
	expect("the unique indexes of 0005", int64(len(indexes)),
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'i' AND relname::text = ANY($1::text[])", indexes)
	expect("the CHECKs of 0005 (item reference, one unit, the per-reserve cap)", int64(len(checks)),
		"SELECT count(*) FROM pg_constraint WHERE conname::text = ANY($1::text[])", checks)
	expect("the column platform.ledger.item_ref", 1,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.ledger'::regclass AND attname::text = 'item_ref' AND NOT attisdropped")
	if present {
		for _, role := range []string{"platform_app", "platform_admin"} {
			if !ledgerBool(t, c, "SELECT has_table_privilege($1::text, 'platform.reserves', 'SELECT')", role) {
				t.Fatalf("%s: %s cannot SELECT platform.reserves", when, role)
			}
		}
	}
	lifecycleMoneyCore(t, c, present, when)
}

// lifecycleMoneyCore is the behavioural half of moneyCoreObjects, in a transaction that is ALWAYS rolled back (a transaction left open on this connection would hold the locks the next down
// needs). With platform_0005 the four rows below are refused, each for its named reason, and platform.reserves holds the one reserve; without it the same kinds of row are accepted.
func lifecycleMoneyCore(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, job := ledgerUserA, ledgerJobA1
	ledgerSeed(t, tx)
	ledgerFund(t, tx, a, "m1", 100)
	if enforced {
		ledgerRefusedCheck(t, tx, when+": a reserve below the dearest price of the snapshot", "reserve_at_the_dearest_price", ledgerReserveCols(a, "m1", job, "m1_own", 10, 1))
		reserve := ledgerOKRow(t, tx, when+": a reserve at the dearest price", ledgerReserveCols(a, "m1", job, "m1_vendor", 10, 2))
		ledgerRefusedCheck(t, tx, when+": a charge with no item_ref", "ledger_item_ref_shape", ledgerChargeCols(a, "m1", job, reserve, "m1_own", 1, 1).without("item_ref"))
		ledgerRefusedCheck(t, tx, when+": a charge of 3 units", "ledger_charge_is_one_item", ledgerChargeCols(a, "m1", job, reserve, "m1_own", 3, 1))
		ledgerRefusedCheck(t, tx, when+": a release of 5 from a reserve with 20 left", "release_is_the_remainder", ledgerReleaseCols(a, "m1", job, reserve, 5))
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves"); n != 1 {
			t.Fatalf("%s: platform.reserves holds %d rows after one reserve, want 1", when, n)
		}
		stamped := ledgerPut(t, tx, ledgerGrantCols(a, "m1", 1).with("created_at", "2999-01-01T00:00:00Z"))
		if !ledgerBool(t, tx, "SELECT created_at = now() FROM platform.ledger WHERE id = $1", stamped) {
			t.Fatalf("%s: a grant that supplied created_at = 2999 kept it: the ledger's clock is not the database's", when)
		}
		return
	}
	kept := ledgerPut(t, tx, ledgerGrantCols(a, "m1", 1).with("created_at", "2999-01-01T00:00:00Z"))
	if !ledgerBool(t, tx, "SELECT created_at = '2999-01-01T00:00:00Z'::timestamptz FROM platform.ledger WHERE id = $1", kept) {
		t.Fatalf("%s: a grant that supplied created_at = 2999 did not keep it: something still stamps the ledger's time", when)
	}
	reserve := ledgerOKRow(t, tx, when+": a reserve below the dearest price (the older chain has no such rule)", ledgerReserveCols(a, "m1", job, "m1_own", 10, 1))
	ledgerOKRow(t, tx, when+": a second reserve for the same job", ledgerReserveCols(a, "m1", job, "m1_own", 5, 1))
	ledgerOKRow(t, tx, when+": a charge of 3 units with no item_ref", ledgerChargeCols(a, "m1", job, reserve, "m1_own", 3, 1).without("item_ref"))
	ledgerOKRow(t, tx, when+": a release of 5, which is not the remainder", ledgerReleaseCols(a, "m1", job, reserve, 5))
	if ledgerBool(t, tx, "SELECT to_regclass('platform.reserves') IS NOT NULL") {
		t.Fatalf("%s: platform.reserves still exists", when)
	}
}

// entitlementFlowObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0006's down: before it the delivery table, the two trigger functions and triggers, the end-date column
// and its CHECK are there, platform_app holds no INSERT or UPDATE on the entitlements (and its column-level UPDATE on the delivery table), and the ledger writes the entitlement; after it all
// of that is gone, platform_app holds INSERT and UPDATE on the entitlements again, and the ledger no longer writes them.
func entitlementFlowObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	funcs := []string{"ledger_apply_to_entitlements", "m3_entitlements_raise_sync"}
	checks := []string{"ledger_until_at_shape", "m3_sync_converged_only_when_observed", "m3_sync_converged_not_ahead", "m3_sync_observed_vocabulary", "m3_sync_versions_are_positive", "m3_sync_attempts_not_negative"}
	expect("the table platform.m3_sync", 1, "SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text = 'm3_sync'")
	expect("the functions of 0006", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the triggers of 0006 (one on the ledger, one on the entitlements)", int64(len(funcs)),
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text = ANY($1::text[])", funcs)
	expect("the CHECKs of 0006 (the end-date rule and the delivery table's five)", int64(len(checks)),
		"SELECT count(*) FROM pg_constraint WHERE conname::text = ANY($1::text[])", checks)
	expect("the column platform.ledger.until_at", 1,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.ledger'::regclass AND attname::text = 'until_at' AND NOT attisdropped")
	// what 0006 takes from platform_app and its down gives back (the control is the same query before and after: it says false while 0006 is up and true once it is down)
	for _, priv := range []string{"INSERT", "UPDATE"} {
		if got := ledgerBool(t, c, "SELECT has_table_privilege('platform_app', 'platform.m3_entitlements', $1::text)", priv); got == present {
			t.Fatalf("%s: platform_app's %s on platform.m3_entitlements is %v, want %v (platform_0006 revokes it and its down restores it)", when, priv, got, !present)
		}
	}
	if present {
		if !ledgerBool(t, c, "SELECT has_column_privilege('platform_app', 'platform.m3_sync', 'observed', 'UPDATE') AND NOT has_column_privilege('platform_app', 'platform.m3_sync', 'want_version', 'UPDATE')") {
			t.Fatalf("%s: platform_app must hold UPDATE on m3_sync.observed and not on m3_sync.want_version", when)
		}
	}
	lifecycleEntitlementFlow(t, c, present, when)
}

// observedVersionObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0008's down: before it the two columns of platform.m3_sync, the replacement CHECK
// m3_sync_converged_only_when_observed_at_version and platform_app's column grants on the two columns are there, and the platform_0006 CHECK is not; after it the columns, the replacement and the grants are gone
// and the platform_0006 CHECK is back. The behavioural half (lifecycleObservedVersion) runs the same statement either way: a convergence claimed for an observation that was not made at this version.
func observedVersionObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the column platform.m3_sync.observed_version", 1,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.m3_sync'::regclass AND attname::text = 'observed_version' AND NOT attisdropped")
	expect("the column platform.m3_sync.m3_version", 1,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.m3_sync'::regclass AND attname::text = 'm3_version' AND NOT attisdropped")
	expect("the CHECK m3_sync_converged_only_when_observed_at_version", 1,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.m3_sync'::regclass AND contype = 'c' AND conname::text = 'm3_sync_converged_only_when_observed_at_version'")
	// the platform_0006 CHECK is the opposite: it is there exactly when platform_0008 is not
	wantOlder := int64(1)
	if present {
		wantOlder = 0
	}
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.m3_sync'::regclass AND contype = 'c' AND conname::text = 'm3_sync_converged_only_when_observed'"); got != wantOlder {
		t.Fatalf("%s: the platform_0006 CHECK m3_sync_converged_only_when_observed: found %d, want %d (platform_0008 replaces it and its down puts it back)", when, got, wantOlder)
	}
	if present {
		if !ledgerBool(t, c, "SELECT has_column_privilege('platform_app', 'platform.m3_sync', 'observed_version', 'UPDATE') AND has_column_privilege('platform_app', 'platform.m3_sync', 'm3_version', 'UPDATE') AND NOT has_column_privilege('platform_app', 'platform.m3_sync', 'want_version', 'UPDATE')") {
			t.Fatalf("%s: platform_app must hold UPDATE on m3_sync.observed_version and m3_sync.m3_version and not on m3_sync.want_version", when)
		}
	}
	lifecycleObservedVersion(t, c, present, when)
}

// jobItemsTextObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0010's down: the column public.job_items.rendered_text is there, nullable, with no default, and a row of the six
// columns module 3's test-send supplies is still accepted (the column is optional); after the down the column is gone and a statement that names it fails with 42703.
func jobItemsTextObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the column public.job_items.rendered_text (text, nullable, no default)", 1,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'job_items' AND column_name = 'rendered_text' AND data_type = 'text' AND is_nullable = 'YES' AND column_default IS NULL")
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a := ledgerUserA
	ledgerSeed(t, tx)
	const six = "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1, $1, 'r@example.com', 's', '<p>b</p>', 'pending')"
	ledgerExpectOK(t, tx, when+": module 3's six-column test-send INSERT (it does not name rendered_text)", six, a)
	const withText = "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text) VALUES ($1, $1, 'r2@example.com', 's', '<p>b</p>', 'b')"
	if present {
		ledgerExpectOK(t, tx, when+": a row with a plain-text part", withText, a)
	} else {
		ledgerExpectRefused(t, tx, when+": a row naming rendered_text", "42703", ledgerWhy{Message: "rendered_text"}, withText, a)
	}
}

// jobLifecycleObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0011's down: before it, the two tables (the legal moves and the events), the twelve new columns of platform.jobs and the
// two capability columns of platform.modules, the four functions, the two triggers on platform.jobs, the eight CHECKs, the unique index of the confirm key and the slot index covering confirmed and completing
// are there, and the rules bite (lifecycleJobLifecycle); after it none of them is, the slot index of platform_0004 is back (scheduled, running, paused, cancelling; no confirmed), and the older rules are back (a
// previewed status is refused by the CHECK of platform_0002, a draft job moves straight to running). The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func jobLifecycleObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	cols := []string{"paused_from", "confirm_key", "preview", "preview_fingerprint", "previewed_at", "confirmed_at", "started_at", "finished_at", "start_deadline", "last_progress_at", "stale_alerted_at", "cancel_requested_at"}
	funcs := []string{"jobs_transition_guard", "jobs_record_transition", "job_events_check_actor", "job_transition"}
	checks := []string{"jobs_paused_from_vocabulary", "jobs_paused_from_shape", "jobs_confirm_key_shape", "jobs_preview_is_an_object", "jobs_fingerprint_shape", "jobs_confirmed_shape", "jobs_start_needs_a_deadline", "jobs_confirmed_starts_now"}
	expect("the tables platform.job_transitions and platform.job_events", 2,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text IN ('job_transitions', 'job_events')")
	expect("the twelve columns of platform.jobs (confirm_key among them)", int64(len(cols)),
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.jobs'::regclass AND NOT attisdropped AND attname::text = ANY($1::text[])", cols)
	expect("the columns platform.modules.can_pause and can_cancel", 2,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.modules'::regclass AND NOT attisdropped AND attname::text IN ('can_pause', 'can_cancel')")
	expect("the function platform.job_transition (uuid, text, text, uuid, text)", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'job_transition' AND oidvectortypes(proargtypes) = 'uuid, text, text, uuid, text'")
	expect("the four functions of 0011 (the guard, the event writer, the actor check, the move)", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the trigger jobs_transition_guard on platform.jobs", 1,
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'platform.jobs'::regclass AND tgname::text = 'jobs_transition_guard'")
	expect("the trigger jobs_record_transition on platform.jobs", 1,
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'platform.jobs'::regclass AND tgname::text = 'jobs_record_transition'")
	expect("the CHECKs of 0011 on platform.jobs", int64(len(checks)),
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.jobs'::regclass AND contype = 'c' AND conname::text = ANY($1::text[])", checks)
	expect("the unique index jobs_confirm_key_key", 1,
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND tablename = 'jobs' AND indexname = 'jobs_confirm_key_key'")
	expect("the slot index jobs_one_active_per_user_module covering confirmed and completing", 1,
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname = 'jobs_one_active_per_user_module' AND indexdef LIKE '%confirmed%' AND indexdef LIKE '%completing%'")
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname = 'jobs_one_active_per_user_module'"); got != 1 {
		t.Fatalf("%s: the slot index jobs_one_active_per_user_module: found %d, want it there either way (platform_0004's or platform_0011's)", when, got)
	}
	if !present {
		if got := ledgerInt64(t, c, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname = 'jobs_one_active_per_user_module'
			AND indexdef LIKE '%scheduled%' AND indexdef LIKE '%running%' AND indexdef LIKE '%paused%' AND indexdef LIKE '%cancelling%' AND indexdef NOT LIKE '%confirmed%' AND indexdef NOT LIKE '%completing%'`); got != 1 {
			t.Fatalf("%s: the slot index of platform_0004 is not back with its predicate (scheduled, running, paused, cancelling; no confirmed, no completing): %d match", when, got)
		}
		if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_guard' AND prosrc LIKE '%left draft%' AND prosrc NOT LIKE '%previewed%'"); got != 1 {
			t.Fatalf("%s: platform.jobs_guard() is not the function of platform_0002 (found %d that is): the down must put it back", when, got)
		}
	} else {
		if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_guard' AND prosrc LIKE '%previewed%'"); got != 1 {
			t.Fatalf("%s: platform.jobs_guard() is not the amended function of platform_0011 (found %d that is)", when, got)
		}
		for _, g := range []struct{ rel, role, priv string }{
			{"platform.job_transitions", "platform_app", "SELECT"}, {"platform.job_transitions", "platform_admin", "SELECT"},
			{"platform.job_events", "platform_app", "SELECT"}, {"platform.job_events", "platform_app", "INSERT"}, {"platform.job_events", "platform_admin", "SELECT"},
		} {
			if !ledgerBool(t, c, "SELECT has_table_privilege($1::text, $2::text, $3::text)", g.role, g.rel, g.priv) {
				t.Fatalf("%s: %s cannot %s %s", when, g.role, g.priv, g.rel)
			}
		}
		if !ledgerBool(t, c, "SELECT has_function_privilege('platform_app', 'platform.job_transition(uuid, text, text, uuid, text)', 'EXECUTE')") {
			t.Fatalf("%s: platform_app cannot EXECUTE platform.job_transition", when)
		}
	}
	lifecycleJobLifecycle(t, c, present, when)
}

// lifecycleJobLifecycle is the behavioural half of jobLifecycleObjects, in a transaction that is ALWAYS rolled back (a transaction left open on this connection would hold the locks the next down needs). With
// platform_0011 a job inserted previewed is refused by the lifecycle (job_born_draft, 23001), a draft job written running by a bare UPDATE too (job_transition_illegal) and the same job walked there the legal way
// (previewed, confirmed, running: platform.job_transition) is accepted; without it a previewed status is refused by the CHECK of platform_0002 (jobs_status_vocabulary, 23514), a job can be inserted running and a
// draft job written running.
func lifecycleJobLifecycle(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	const insert = "INSERT INTO platform.jobs (id, user_id, module_id, status) VALUES ($1, $2, 'm1', $3)"
	if enforced {
		ledgerExpectRefused(t, tx, when+": a job inserted previewed", "23001", ledgerWhy{Constraint: "job_born_draft"}, insert, ledgerUUID("fe", 1), ledgerUserC, "previewed")
		ledgerExpectRefused(t, tx, when+": a draft job written running by a bare UPDATE", "23001", ledgerWhy{Constraint: "job_transition_illegal"}, clsSetStatus, ledgerJobA1, "running")
		jobWalkTo(t, tx, ledgerJobA1, "running")
		if got := jobStatus(t, tx, ledgerJobA1); got != "running" {
			t.Fatalf("%s: the job walked to running is %s", when, got)
		}
		return
	}
	ledgerExpectRefused(t, tx, when+": a job inserted previewed (the vocabulary of the older chain has no such status)", "23514", ledgerWhy{Constraint: "jobs_status_vocabulary"}, insert, ledgerUUID("fe", 1), ledgerUserC, "previewed")
	ledgerExpectOK(t, tx, when+": a job inserted running (the older chain has no lifecycle)", insert, ledgerUUID("fe", 2), ledgerUserC, "running")
	ledgerExpectOK(t, tx, when+": a draft job written running by a bare UPDATE", clsSetStatus, ledgerJobA1, "running")
}

// TestRunnerLifecycle_Down0011RefusesAJobThatTheOlderVocabularyDoesNotKnow: platform_0011's down says no (SQLSTATE 55000, naming how many) while a job is previewed, confirmed, completing or expired, the four
// statuses platform_0002's CHECK does not have, and leaves the database exactly as it was (it is still at 0011). The control of each refusal is the same down once no such job is left: with jobs that are
// cancelled and failed (statuses the older vocabulary knows, ended by the legal path) it succeeds, and the older vocabulary is back. The expired job is made after that, on the chain brought up again (an expired job
// is final: it cannot be ended to let the down pass, so it is the one case that has no moving-it-away control of its own: the control is the down that passed).
// It fails if the guard of the down is missing, if it counts wrongly, or if a refused down changes anything.
func TestRunnerLifecycle_Down0011RefusesAJobThatTheOlderVocabularyDoesNotKnow(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	env := migEnv(s.DSN, dir, nil)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1 // the index of 0011: a migration above it (a later chunk of Phase 4) is rolled back first, highest first, and put back by the up below
	for i, v := range chain {
		if v == "0011" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0011 after other migrations", chain)
	}
	toHeight := func() { // roll back, highest first, whatever is applied above 0011
		t.Helper()
		for i := len(chain) - 1; i > at; i-- {
			if containsString(appliedVersions(t, admin), chain[i]) {
				mustExit(t, runMigrator(t, envDown, "down", chain[i]), 0, "down "+chain[i]+" on the way down to 0011")
			}
		}
	}
	seed, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSeed(t, seed)
	if err := seed.Commit(ctx); err != nil { // the migrator runs on its own connection: the jobs must be committed
		t.Fatal(err)
	}
	toHeight()

	refused := func(what string) {
		t.Helper()
		res := runMigrator(t, envDown, "down", "0011")
		mustExit(t, res, 1, "down 0011 with "+what)
		mustContain(t, res.Stderr, "platform_0011 down: 1 job(s) are previewed, confirmed, completing or expired", "down 0011 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0011 with "+what)
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0011 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.job_transitions"); n != 29 {
			t.Fatalf("a refused down 0011 (%s) left %d rows in platform.job_transitions, want its 29", what, n)
		}
	}
	for _, c := range []struct{ what, job, state string }{
		{"a previewed job", ledgerJobA1, "previewed"},
		{"a confirmed job", ledgerJobB1, "confirmed"},
		{"a completing job", ledgerJobA2, "completing"},
	} {
		jobWalkTo(t, admin, c.job, c.state)
		if got := ledgerInt64(t, admin, "SELECT count(*) FROM platform.jobs WHERE status IN ('previewed', 'confirmed', 'completing', 'expired')"); got != 1 {
			t.Fatalf("the instrument: %d jobs are in the four statuses before the down with %s, want exactly 1", got, c.what)
		}
		refused(c.what)
		jobEnd(t, admin, c.job) // the paired control's setup: cancelled (a previewed job) and failed (the others), statuses the older vocabulary has
	}

	// the control: no job is left in a status the older vocabulary lacks, so the same down passes, and the older vocabulary is back
	res := runMigrator(t, envDown, "down", "0011")
	mustExit(t, res, 0, "down 0011 with only cancelled and failed jobs")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0011 that passed: applied = %v, want %v", got, chain[:at])
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledgerExpectRefused(t, tx, "after down 0011, a job inserted previewed", "23514", ledgerWhy{Constraint: "jobs_status_vocabulary"},
		"INSERT INTO platform.jobs (id, user_id, module_id, status) VALUES ($1, $2, 'm1', 'previewed')", ledgerUUID("fd", 1), ledgerUserC)
	_ = tx.Rollback(ctx)

	// up again over the jobs that were left behind, then an expired job: the down refuses it too
	mustExit(t, runMigrator(t, env, "up"), 0, "up after the down, over a database with ended jobs")
	if got := appliedVersions(t, admin); !sameStrings(got, chain) {
		t.Fatalf("after up again: applied = %v, want %v", got, chain)
	}
	toHeight()
	jobWalkTo(t, admin, ledgerJobA0, "expired")
	refused("an expired job")
}

// jobRowsObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0012's down: before it, the two tables, the proof view, the two guard functions and their triggers (the append-only pair of the
// results among them), the thirteen CHECKs and the grants (platform_app: SELECT, INSERT, DELETE of the rows and UPDATE of submitted_at but not of job_id; the results SELECT and INSERT only) are there and the
// rules bite (lifecycleJobRows); after it none of them is, and a statement that names the list fails with 42P01. The counts are of a fixed list each.
func jobRowsObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	funcs := []string{"job_rows_frozen", "job_results_after_confirm"}
	triggers := []string{"job_rows_frozen", "job_results_after_confirm", "job_results_append_only", "job_results_no_truncate"}
	checks := []string{"job_rows_ordinal_positive", "job_rows_state_vocabulary", "job_rows_email_raw_bounded", "job_rows_email_norm_shape", "job_rows_duplicate_shape", "job_rows_reason_shape", "job_rows_fields_is_an_object",
		"job_rows_only_pending_is_submitted", "job_results_origin_vocabulary", "job_results_decision_vocabulary", "job_results_charged_names_its_unit", "job_results_state_not_blank", "job_results_detail_is_an_object"}
	expect("the tables platform.job_rows and platform.job_results", 2,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text IN ('job_rows', 'job_results')")
	expect("the view platform.job_settlement_drift", 1,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'v' AND relname::text = 'job_settlement_drift'")
	expect("the two guard functions of 0012", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the four triggers of 0012 (the rows guard, the results guard and the results' append-only pair)", int64(len(triggers)),
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN (to_regclass('platform.job_rows'), to_regclass('platform.job_results')) AND tgname::text = ANY($1::text[])", triggers)
	expect("the CHECKs of 0012", int64(len(checks)),
		"SELECT count(*) FROM pg_constraint WHERE contype = 'c' AND conname::text = ANY($1::text[])", checks)
	if present {
		for _, g := range []struct{ rel, role, priv string }{
			{"platform.job_rows", "platform_app", "SELECT"}, {"platform.job_rows", "platform_app", "INSERT"}, {"platform.job_rows", "platform_app", "DELETE"}, {"platform.job_rows", "platform_admin", "SELECT"},
			{"platform.job_results", "platform_app", "SELECT"}, {"platform.job_results", "platform_app", "INSERT"}, {"platform.job_results", "platform_admin", "SELECT"},
			{"platform.job_settlement_drift", "platform_app", "SELECT"}, {"platform.job_settlement_drift", "platform_admin", "SELECT"},
		} {
			if !ledgerBool(t, c, "SELECT has_table_privilege($1::text, $2::text, $3::text)", g.role, g.rel, g.priv) {
				t.Fatalf("%s: %s cannot %s %s", when, g.role, g.priv, g.rel)
			}
		}
		if !ledgerBool(t, c, "SELECT has_column_privilege('platform_app', 'platform.job_rows', 'submitted_at', 'UPDATE') AND NOT has_column_privilege('platform_app', 'platform.job_rows', 'job_id', 'UPDATE') AND NOT has_table_privilege('platform_app', 'platform.job_results', 'UPDATE')") {
			t.Fatalf("%s: platform_app must update submitted_at of a row and neither job_id of a row nor a result", when)
		}
	}
	lifecycleJobRows(t, c, present, when)
}

// lifecycleJobRows is the behavioural half of jobRowsObjects, in a transaction that is ALWAYS rolled back. With platform_0012 a draft job takes a row, the job confirmed takes a result and no more rows (23001,
// job_rows_frozen), and a result for a draft job is refused (23514, jobs_results_after_confirm); without it a statement that names the list is 42P01.
func lifecycleJobRows(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	if !enforced {
		ledgerExpectRefused(t, tx, when+": a statement that names the list of a job", "42P01", ledgerWhy{Message: "job_rows"}, "SELECT count(*) FROM platform.job_rows")
		return
	}
	const row = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm) VALUES ($1, $2, $3, $4, 'a@example.com', 'a@example.com')"
	const result = "INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ($1, $2, $3, 1, 'a@example.com', 'module', 'sent', 'free', 'M2-04')"
	first, second := ledgerUUID("fc", 1), ledgerUUID("fc", 2)
	ledgerExpectOK(t, tx, when+": a row of a draft job", row, first, ledgerJobA2, ledgerUserA, 1)
	ledgerExpectRefused(t, tx, when+": a result for a draft job", "23514", ledgerWhy{Constraint: "jobs_results_after_confirm"}, result, ledgerJobA2, ledgerUserA, first)
	jobWalkTo(t, tx, ledgerJobA2, "confirmed")
	ledgerExpectOK(t, tx, when+": a result for a confirmed job", result, ledgerJobA2, ledgerUserA, first)
	ledgerExpectRefused(t, tx, when+": a row added to a confirmed job", "23001", ledgerWhy{Constraint: "job_rows_frozen"}, row, second, ledgerJobA2, ledgerUserA, 2)
}

// reachableUnitsObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0013's down: before it, the three columns of platform.unit_costs, the history table, the three functions (the writer, the
// snapshot check, the history trigger function), the four triggers, the CHECK and the grants are there, platform.ledger_check_reserve counts the REACHABLE units, and the vendor unit is refused as a snapshot row
// once it is unreachable; after it none of them is and the OLDER reserve check is back: every unit of the module is the complete snapshot (a snapshot without the vendor unit is refused, and the full one is
// reserved at the vendor price 2). The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func reachableUnitsObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	funcs := []string{"set_unit_reachable", "job_unit_prices_only_reachable", "unit_costs_record_reachability"}
	expect("the columns reachable, reachable_source and reachable_changed_at of platform.unit_costs", 3,
		"SELECT count(*) FROM pg_attribute WHERE attrelid = 'platform.unit_costs'::regclass AND NOT attisdropped AND attname::text IN ('reachable', 'reachable_source', 'reachable_changed_at')")
	expect("the table platform.unit_reachability_history", 1,
		"SELECT count(*) FROM pg_class WHERE relnamespace = 'platform'::regnamespace AND relkind = 'r' AND relname::text = 'unit_reachability_history'")
	expect("the three functions of 0013", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the triggers of 0013 (the snapshot check, the history trigger and the history's append-only pair)", 4,
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text IN ('job_unit_prices_only_reachable', 'unit_costs_record_reachability', 'unit_reachability_history_append_only', 'unit_reachability_history_no_truncate')")
	expect("the CHECK unit_costs_reachable_source_not_blank", 1,
		"SELECT count(*) FROM pg_constraint WHERE conrelid = 'platform.unit_costs'::regclass AND contype = 'c' AND conname::text = 'unit_costs_reachable_source_not_blank'")
	expect("platform.ledger_check_reserve counting the reachable units", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'ledger_check_reserve' AND prosrc LIKE '%reachable%'")
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'ledger_check_reserve' AND prosrc LIKE '%reserve_needs_the_complete_snapshot%' AND prosrc LIKE '%reserve_at_the_dearest_price%'"); got != 1 {
		t.Fatalf("%s: platform.ledger_check_reserve is not there with both of its refusals (found %d): the down must put the older one back, not drop it", when, got)
	}
	if present {
		if !ledgerBool(t, c, "SELECT has_function_privilege('platform_app', 'platform.set_unit_reachable(text, boolean, text)', 'EXECUTE') AND NOT has_function_privilege('platform_admin', 'platform.set_unit_reachable(text, boolean, text)', 'EXECUTE') AND NOT has_column_privilege('platform_app', 'platform.unit_costs', 'reachable', 'UPDATE')") {
			t.Fatalf("%s: platform_app must execute set_unit_reachable and not write the flag itself, and platform_admin must not execute it", when)
		}
	}
	lifecycleReachableUnits(t, c, present, when)
}

// lifecycleReachableUnits is the behavioural half of reachableUnitsObjects, in a transaction that is ALWAYS rolled back. With platform_0013 and the vendor switched off a snapshot row for it is refused (23514,
// snapshot_only_reachable_units) and a snapshot of the two reachable units is reserved at 1 and not at 2; without it the flag does not exist (42703), a snapshot of two units is not complete
// (reserve_needs_the_complete_snapshot) and the three are reserved at 2.
func lifecycleReachableUnits(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx) // with platform_0013 the seed switches the vendor on; the job A0 has no snapshot
	a, job := ledgerUserA, ledgerJobA0
	ledgerFund(t, tx, a, "m1", 100)
	const snap = "INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.unit = ANY($2::text[])"
	if enforced {
		ledgerMust(t, tx, "SELECT platform.set_unit_reachable('m1_vendor', false, 'lifecycle test: switched off')")
		ledgerExpectRefused(t, tx, when+": a snapshot row for the vendor unit once it is unreachable", "23514", ledgerWhy{Constraint: "snapshot_only_reachable_units"}, snap, job, []string{"m1_vendor"})
		ledgerExpectOK(t, tx, when+": a snapshot of the two reachable units", snap, job, []string{"m1_own", "m1_cache"})
		ledgerRefusedCheck(t, tx, when+": a reserve at the vendor price (not the dearest of what is reachable)", "reserve_at_the_dearest_price", ledgerReserveCols(a, "m1", job, "m1_own", 10, 2))
		ledgerOKRow(t, tx, when+": a reserve at the dearest REACHABLE price", ledgerReserveCols(a, "m1", job, "m1_own", 10, 1))
		return
	}
	ledgerExpectRefused(t, tx, when+": the flag does not exist", "42703", ledgerWhy{Message: "reachable"}, "SELECT reachable FROM platform.unit_costs")
	ledgerExpectOK(t, tx, when+": a snapshot of two units", snap, job, []string{"m1_own", "m1_cache"})
	ledgerRefusedCheck(t, tx, when+": a reserve against a snapshot that lacks a unit of the module", "reserve_needs_the_complete_snapshot", ledgerReserveCols(a, "m1", job, "m1_own", 10, 1))
	ledgerExpectOK(t, tx, when+": the vendor unit joins the snapshot", snap, job, []string{"m1_vendor"})
	ledgerOKRow(t, tx, when+": a reserve at the vendor price over the complete snapshot", ledgerReserveCols(a, "m1", job, "m1_vendor", 10, 2))
}

// migRollBackAbove rolls the chain back, highest first, until nothing above chain[keep] is applied (what a down test needs when a later chunk of Phase 4 has added migrations above the one it is about).
func migRollBackAbove(t *testing.T, admin *pgx.Conn, envDown map[string]string, chain []string, keep int) {
	t.Helper()
	for i := len(chain) - 1; i > keep; i-- {
		if containsString(appliedVersions(t, admin), chain[i]) {
			mustExit(t, runMigrator(t, envDown, "down", chain[i]), 0, "down "+chain[i]+" on the way down to "+chain[keep])
		}
	}
}

// TestRunnerLifecycle_Down0012RefusesWhileARowOrAResultExists: platform_0012's down says no (SQLSTATE 55000, naming how many) while any job row or result exists, and leaves the database exactly as it was (the rows are
// still there, the chain still at 0012): a result is a record of money and of a customer's outcome, kept forever, and a down never destroys it. The control of the first refusal is the same down once the row is
// deleted (a draft's list may be): it passes and the tables are gone. After an up again a job is confirmed and has a row and a result; the down is refused again (2), and a result cannot be deleted to make it
// pass (append-only): that case has no moving-it-away control of its own, the down that passed is its control.
// It fails if the guard of the down is missing, if it counts wrongly, if it drops what it refused to, or if it refuses an empty list.
func TestRunnerLifecycle_Down0012RefusesWhileARowOrAResultExists(t *testing.T) {
	s, admin := migratedScratch(t)
	ctx := testCtx(t)
	dir := repoMigrationsDir(t)
	env := migEnv(s.DSN, dir, nil)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})
	chain := chainOnDisk(t, dir)
	at := -1
	for i, v := range chain {
		if v == "0012" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the chain on disk is %v: it holds no 0012 after other migrations", chain)
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

	refused := func(what string, n int) {
		t.Helper()
		res := runMigrator(t, envDown, "down", "0012")
		mustExit(t, res, 1, "down 0012 with "+what)
		mustContain(t, res.Stderr, fmt.Sprintf("platform_0012 down: %d job row(s) and result(s) exist", n), "down 0012 with "+what)
		mustContain(t, res.Stderr, "55000", "down 0012 with "+what)
		if got := appliedVersions(t, admin); !sameStrings(got, chain[:at+1]) {
			t.Fatalf("a refused down 0012 (%s) changed the database: applied = %v, want %v", what, got, chain[:at+1])
		}
		if got := ledgerInt64(t, admin, "SELECT (SELECT count(*) FROM platform.job_rows) + (SELECT count(*) FROM platform.job_results)"); got != int64(n) {
			t.Fatalf("a refused down 0012 (%s) left %d row(s) and result(s), want %d", what, got, n)
		}
	}
	const row = "INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm) VALUES ($1, $2, $3, 1, 'a@example.com', 'a@example.com')"
	rowID := ledgerUUID("fb", 1)
	ledgerMust(t, admin, row, rowID, ledgerJobA2, ledgerUserA)
	refused("a row", 1)

	ledgerMust(t, admin, "DELETE FROM platform.job_rows WHERE id = $1", rowID) // the control's setup: a draft's list may be emptied
	mustExit(t, runMigrator(t, envDown, "down", "0012"), 0, "down 0012 over an empty list")
	if got := appliedVersions(t, admin); !sameStrings(got, chain[:at]) {
		t.Fatalf("after the down 0012 that passed: applied = %v, want %v", got, chain[:at])
	}
	if relationExists(t, admin, "platform.job_rows") || relationExists(t, admin, "platform.job_results") || relationExists(t, admin, "platform.job_settlement_drift") {
		t.Fatal("after the down 0012 that passed a table or the view of 0012 is still there")
	}

	mustExit(t, runMigrator(t, env, "up"), 0, "up after the down 0012")
	migRollBackAbove(t, admin, envDown, chain, at)
	ledgerMust(t, admin, row, rowID, ledgerJobA2, ledgerUserA)
	jobWalkTo(t, admin, ledgerJobA2, "confirmed")
	ledgerMust(t, admin, "INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ($1, $2, $3, 1, 'a@example.com', 'module', 'sent', 'free', 'M2-04')", ledgerJobA2, ledgerUserA, rowID)
	refused("a row and its result", 2)
}

// suppressionSyntaxObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0009's down, in a transaction that is ALWAYS rolled back: with the migration the CHECK
// suppressions_email_norm_v1 refuses an address module 1's syntax layer fails (a local part of 65 bytes, a domain with no dot) and a label with "--" in its third and fourth bytes, and accepts the
// boundary (a local part of exactly 64 bytes, the smallest dotted domain, an "xn--" label); without it (platform_0003's CHECK) all of them are accepted. The ordinary address is accepted either way.
func suppressionSyntaxObjects(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a := ledgerUserA
	ledgerSeed(t, tx)
	const ins = "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')"
	ledgerExpectOK(t, tx, when+": an ordinary address", ins, a, "ada@example.com")
	ledgerExpectOK(t, tx, when+": a local part of exactly 64 bytes", ins, a, strings.Repeat("a", 64)+"@example.com")
	ledgerExpectOK(t, tx, when+": the smallest dotted domain", ins, a, "ada@a.b")
	ledgerExpectOK(t, tx, when+": a punycode label with -- in its third and fourth bytes", ins, a, "ada@xn--bcher-kva.example")
	for _, c := range []struct{ what, email string }{
		{"a local part of 65 bytes", strings.Repeat("a", 65) + "@example.com"},
		{"a domain with no dot", "ada@localhost"},
		{"a label with -- in its third and fourth bytes", "ada@ab--cd.example"},
	} {
		if enforced {
			ledgerExpectRefused(t, tx, when+": "+c.what, "23514", ledgerWhy{Constraint: "suppressions_email_norm_v1"}, ins, a, c.email)
		} else {
			ledgerExpectOK(t, tx, when+": "+c.what+" (platform_0003 knows no syntax layer)", ins, a, c.email)
		}
	}
}

// lifecycleObservedVersion is the behavioural half of observedVersionObjects, in a transaction that is ALWAYS rolled back. With platform_0008 a delivery row whose observation carries no version cannot be
// converged (23514, m3_sync_converged_only_when_observed_at_version) and the same row with the observation made at this version can; without it the platform_0006 rule accepts the first statement (the right state
// was observed) and the column does not exist (42703).
func lifecycleObservedVersion(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a := ledgerUserA
	ledgerSeed(t, tx)
	ledgerOKRow(t, tx, when+": an m3_enable (the entitlement trigger writes the delivery row: want_version 1, wanted active)", ledgerM3Cols("m3_enable", a))
	const noVersion = "UPDATE platform.m3_sync SET observed = 'active', converged_version = 1 WHERE user_id = $1"
	const withVersion = "UPDATE platform.m3_sync SET observed = 'active', observed_version = 1, converged_version = 1 WHERE user_id = $1"
	if enforced {
		ledgerExpectRefused(t, tx, when+": converged with the right state observed but no version of the observation", "23514", ledgerWhy{Constraint: "m3_sync_converged_only_when_observed_at_version"}, noVersion, a)
		ledgerExpectOK(t, tx, when+": converged with the right state observed at this version", withVersion, a)
		return
	}
	ledgerExpectOK(t, tx, when+": the platform_0006 rule accepts the right state observed (it knows no version)", noVersion, a)
	ledgerExpectRefused(t, tx, when+": the column observed_version does not exist", "42703", ledgerWhy{Message: "observed_version"}, withVersion, a)
}

// lifecycleEntitlementFlow is the behavioural half of entitlementFlowObjects, in a transaction that is ALWAYS rolled back. With platform_0006 an m3_enable row with an end date creates the
// entitlement and its delivery row, an m3_expire that is not due is refused (m3_expire_is_due) and platform_app is refused a direct INSERT (42501); without it the date column does not exist
// (42703), an m3_enable creates no entitlement, an m3_expire for a user with none is accepted, and platform_app writes the entitlements itself again.
func lifecycleEntitlementFlow(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	a, b, cc := ledgerUserA, ledgerUserB, ledgerUserC
	ledgerSeed(t, tx)
	const enableWithDate = `INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key, until_at)
		VALUES ($1, 'm3', 'm3_enable', $2, 'lifecycle', $3, now() + interval '1 day')`
	const directInsert = "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)"
	if enforced {
		ledgerExpectOK(t, tx, when+": an m3_enable with an end date", enableWithDate, a, ledgerAdminA, ledgerNextKey("lifecycle"))
		if got := ledgerRows(t, tx, "SELECT enabled::text, (enabled_until IS NOT NULL)::text FROM platform.m3_entitlements WHERE user_id = $1", a); len(got) != 1 || got[0] != "true|true" {
			t.Fatalf("%s: the entitlement written by the m3_enable reads %v, want [true|true]", when, got)
		}
		if got := ledgerRows(t, tx, "SELECT want_suspended::text, want_version, converged_version, observed FROM platform.m3_sync WHERE user_id = $1", a); len(got) != 1 || got[0] != "false|1|0|unknown" {
			t.Fatalf("%s: the delivery row written by the entitlement trigger reads %v, want [false|1|0|unknown]", when, got)
		}
		ledgerRefusedCheck(t, tx, when+": an m3_expire that is not due", "m3_expire_is_due", ledgerM3Cols("m3_expire", a))
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectRefused(t, tx, when+": platform_app writing the entitlement itself", "42501", ledgerWhy{Message: "permission denied for table m3_entitlements"}, directInsert, b)
		ledgerResetRole(t, tx)
		return
	}
	ledgerExpectRefused(t, tx, when+": an end date on an m3_enable (the older ledger has no such column)", "42703", ledgerWhy{Message: "until_at"}, enableWithDate, a, ledgerAdminA, ledgerNextKey("lifecycle"))
	ledgerOKRow(t, tx, when+": an m3_enable", ledgerM3Cols("m3_enable", a))
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_entitlements WHERE user_id = $1", a); n != 0 {
		t.Fatalf("%s: an m3_enable wrote %d entitlement rows: something other than the application still writes them", when, n)
	}
	ledgerOKRow(t, tx, when+": an m3_expire for a user with no entitlement (the older chain checks nothing)", ledgerM3Cols("m3_expire", b))
	ledgerSwitchRole(t, tx, "platform_app")
	ledgerExpectOK(t, tx, when+": platform_app inserting an entitlement itself", directInsert, cc)
	ledgerExpectOK(t, tx, when+": platform_app updating an entitlement itself", "UPDATE platform.m3_entitlements SET enabled = false WHERE user_id = $1", cc)
	ledgerResetRole(t, tx)
	if ledgerBool(t, tx, "SELECT to_regclass('platform.m3_sync') IS NOT NULL") {
		t.Fatalf("%s: platform.m3_sync still exists", when)
	}
}

func appliedAt(t *testing.T, c *pgx.Conn) map[string]string {
	t.Helper()
	rows, err := c.Query(testCtx(t), "SELECT version, applied_at::text FROM platform.schema_migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var v, at string
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		out[v] = at
	}
	return out
}

// TestRunnerLifecycle drives the real binary through the whole life of a database: empty, up, up again, the designed
// roles and ownership, down refused, down (step by step, highest first, each step checked against a control that the
// objects were there before it), up again.
//
// It fails if: a migration is added without extending wantChain (and the per-version checks), an object of the platform is
// left owned by the migrator, a down leaves an object of its migration behind (or takes one of a lower migration with it),
// platform_0001 stops setting the platform_app timezone or its down stops resetting it, or the chain no longer rolls back.
func TestRunnerLifecycle(t *testing.T) {
	requireLiveMigrated(t) // the down below must not be able to drop the cluster-global roles the live service uses

	// The executable expectation of the migration chain. scripts/check-migrations.sh reads this literal and fails when the
	// newest migration is not named in it, so a migration cannot be added without extending this test.
	wantChain := []string{"0000", "0001", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015", "0016"}

	dir := repoMigrationsDir(t)
	if onDisk := chainOnDisk(t, dir); !sameStrings(onDisk, wantChain) {
		t.Fatalf("wantChain = %v but db/migrations holds %v: update wantChain, and the assertions below, with the migration", wantChain, onDisk)
	}
	s := newScratch(t)
	admin := s.admin(t)
	ctx := testCtx(t)
	env := migEnv(s.DSN, dir, nil)
	envDown := migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "true"})

	// An empty database: status says nothing is applied.
	if schemaExists(t, admin, "platform") {
		t.Fatal("precondition: the scratch database already has a platform schema, so the empty-database half of this test would be vacuous")
	}
	res := runMigrator(t, env, "status")
	mustExit(t, res, 0, "status on an empty database")
	mustContain(t, res.Stdout, fmt.Sprintf("summary: 0 applied, %d pending", len(wantChain)), "status on an empty database")
	for _, v := range wantChain {
		if !strings.Contains(res.Stdout, v+"_") || !strings.Contains(res.Stdout, "PENDING") {
			t.Fatalf("status on an empty database should list %s as PENDING:\n%s", v, res.Stdout)
		}
	}

	// up applies exactly the chain.
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up")
	if got := appliedVersions(t, admin); !sameStrings(got, wantChain) {
		t.Fatalf("after up: applied = %v, want exactly %v\n%s", got, wantChain, res.all())
	}
	assertDesignedState(t, admin, "after up")
	status := runMigrator(t, env, "status")
	mustExit(t, status, 0, "status after up")
	mustContain(t, status.Stdout, fmt.Sprintf("summary: %d applied, 0 pending", len(wantChain)), "status after up")

	// A second up applies nothing: the same versions, and the very same applied_at timestamps (nothing was re-run).
	before := appliedAt(t, admin)
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "second up")
	mustContain(t, res.Stdout, "no pending migrations", "second up")
	if strings.Contains(res.Stdout, "applied  ") {
		t.Fatalf("the second up applied something:\n%s", res.Stdout)
	}
	after := appliedAt(t, admin)
	same := len(after) == len(before)
	for v, at := range before {
		same = same && after[v] == at
	}
	if !same {
		t.Fatalf("the second up changed the tracking table: before %v, after %v", before, after)
	}

	// down is refused without PLATFORM_MIGRATE_ALLOW_DOWN=true, and nothing is touched.
	newest := wantChain[len(wantChain)-1]
	res = runMigrator(t, env, "down", newest)
	mustExit(t, res, 2, "down without the flag")
	mustContain(t, res.Stderr, "down is forbidden unless PLATFORM_MIGRATE_ALLOW_DOWN=true", "down without the flag")
	if got := appliedVersions(t, admin); !sameStrings(got, wantChain) {
		t.Fatalf("a refused down changed the database: applied = %v", got)
	}
	// ...and an explicit false is the same refusal.
	res = runMigrator(t, migEnv(s.DSN, dir, map[string]string{"PLATFORM_MIGRATE_ALLOW_DOWN": "false"}), "down", newest)
	mustExit(t, res, 2, "down with the flag false")

	// With the flag, down rolls the chain back highest-first, each step leaving exactly the versions below it.
	for i := len(wantChain) - 1; i >= 0; i-- {
		downStepBefore(t, admin, wantChain[i]) // the control: what this down removes is there now
		res = runMigrator(t, envDown, "down", wantChain[i])
		mustExit(t, res, 0, "down "+wantChain[i])
		mustContain(t, res.Stdout, "rolled back "+wantChain[i]+"_", "down "+wantChain[i])
		if i > 0 {
			if got := appliedVersions(t, admin); !sameStrings(got, wantChain[:i]) {
				t.Fatalf("after down %s: applied = %v, want %v", wantChain[i], got, wantChain[:i])
			}
		}
		downStepAfter(t, admin, wantChain[i])
	}
	if schemaExists(t, admin, "platform") {
		t.Fatal("after down 0000 the platform schema still exists")
	}
	// The bootstrap's down drops the cluster-global roles best-effort and KEEPS any still referenced elsewhere (the live
	// database references them), so whether they are gone is not asserted: only that this run tolerated either.
	var retained int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname IN ('platform_owner','platform_app','platform_admin')").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	t.Logf("after down 0000: %d of the 3 platform roles are retained (still referenced by the live database)", retained)

	// up again restores everything.
	res = runMigrator(t, env, "up")
	mustExit(t, res, 0, "up after down")
	if got := appliedVersions(t, admin); !sameStrings(got, wantChain) {
		t.Fatalf("after up again: applied = %v, want %v", got, wantChain)
	}
	assertDesignedState(t, admin, "after up again")
}
