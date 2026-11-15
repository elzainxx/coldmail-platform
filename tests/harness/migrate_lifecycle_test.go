//go:build harness

package harness

import (
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
	wantChain := []string{"0000", "0001", "0002", "0003"}

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
