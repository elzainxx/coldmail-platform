//go:build harness

package harness

import (
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

// assertDesignedState is what a migrated database must look like, whichever way it got there.
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
// roles and ownership, down refused, down, up again.
func TestRunnerLifecycle(t *testing.T) {
	requireLiveMigrated(t) // the down below must not be able to drop the cluster-global roles the live service uses

	// The executable expectation of the migration chain. scripts/check-migrations.sh reads this literal and fails when the
	// newest migration is not named in it, so a migration cannot be added without extending this test.
	wantChain := []string{"0000"}

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
	mustContain(t, res.Stdout, "summary: 0 applied, 1 pending", "status on an empty database")
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
	mustContain(t, status.Stdout, "summary: 1 applied, 0 pending", "status after up")

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
		res = runMigrator(t, envDown, "down", wantChain[i])
		mustExit(t, res, 0, "down "+wantChain[i])
		mustContain(t, res.Stdout, "rolled back "+wantChain[i]+"_", "down "+wantChain[i])
		if i > 0 {
			if got := appliedVersions(t, admin); !sameStrings(got, wantChain[:i]) {
				t.Fatalf("after down %s: applied = %v, want %v", wantChain[i], got, wantChain[:i])
			}
		}
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
