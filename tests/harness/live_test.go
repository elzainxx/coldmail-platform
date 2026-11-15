//go:build harness

package harness

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The tests in this file look at the LIVE database `platform`, the one the harness stack migrated and the running
// platform service logs into. They only read it: the admin connection is read-only by construction, and the one write
// attempted (as platform_app, which must be refused) is inside a transaction that is rolled back.

// TestLive_GrantsInvariant runs the same checker as TestGrantsInvariant against the live database.
func TestLive_GrantsInvariant(t *testing.T) {
	live := liveAdmin(t)
	v, err := checkGrants(testCtx(t), live)
	requireNoViolations(t, v, err, "the live database")
}

// appDial logs in to the live database over the network AS platform_app (not by SET ROLE), with the given password.
func appDial(password string) func(*pgx.ConnConfig) {
	return func(cfg *pgx.ConnConfig) { cfg.User, cfg.Password = "platform_app", password }
}

// TestLive_AppLoginWorks is what the running service depends on: platform_app can log in with the password it was given,
// reads the tracking table, cannot create anything, and is not a superuser. The paired control is the same dial with a
// wrong password, which must be refused with 28P01; without it a server that accepts anyone would pass the rest.
func TestLive_AppLoginWorks(t *testing.T) {
	pw := requireEnv(t, envAppPassword)
	adminURL := requireEnv(t, envAdminURL)
	ctx := testCtx(t)

	app := dial(t, adminURL, appDial(pw))
	var sessionUser string
	if err := app.QueryRow(ctx, "SELECT session_user").Scan(&sessionUser); err != nil {
		t.Fatal(err)
	}
	if sessionUser != "platform_app" {
		t.Fatalf("session_user = %q, want platform_app: this must be a real login, not a role switch", sessionUser)
	}

	rows, err := app.Query(ctx, "SELECT version FROM platform.schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("platform_app cannot read platform.schema_migrations: %v", err)
	}
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range versions {
		found = found || v == "0000"
	}
	if !found {
		t.Fatalf("platform.schema_migrations as platform_app lists %v, want it to contain 0000", versions)
	}

	// No DDL: CREATE TABLE in the schema is refused with insufficient_privilege. In a transaction that is never committed,
	// so that if the grants were wrong this test would not leave a table in the live database.
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, "CREATE TABLE platform.x (id int)")
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("platform_app CREATE TABLE platform.x succeeded: the runtime identity can run DDL")
	}
	if code := sqlState(err); code != "42501" {
		t.Fatalf("CREATE TABLE platform.x failed with SQLSTATE %q (%v), want 42501", code, err)
	}

	var super bool
	if err := app.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Fatal("platform_app is a superuser")
	}

	// The paired control: a wrong password is refused, with the password-failure code.
	_, err = tryDial(adminURL, appDial(pw+"-wrong"))
	if err == nil {
		t.Fatal("platform_app logged in with a wrong password: the server is not enforcing passwords on this connection, so the successful login above proves nothing")
	}
	if code := sqlState(err); code != "28P01" {
		t.Fatalf("a wrong password failed with SQLSTATE %q (%s), want 28P01 invalid_password", code, scrubbed(err, adminURL))
	}
}

// TestLive_StackCoexists is for the FULL harness stack only: it needs the platform, module 2 and module 3 migrated into
// one database, which is what tests/harness/run.sh builds. It asserts the platform's chain is intact beside them and that
// platform_app, the platform's runtime identity, cannot reach the modules' schemas. If the module schemas are not there
// the test FAILS: a stack that has not migrated the modules proves nothing about coexistence, and a pass would hide that.
func TestLive_StackCoexists(t *testing.T) {
	live := liveAdmin(t)
	ctx := testCtx(t)

	// The platform's chain is intact: exactly the versions on disk are applied, contiguous from 0000.
	want := chainOnDisk(t, repoMigrationsDir(t))
	if got := appliedVersions(t, live); !sameStrings(got, want) {
		t.Fatalf("the live platform chain is %v, the repository has %v", got, want)
	}

	// The modules are there (module 1's tables in public are not asserted: that is module 1's own tests' business).
	for _, schema := range []string{"send_managed", "send_byo"} {
		if !schemaExists(t, live, schema) {
			t.Fatalf("the harness stack has not migrated the modules: schema %s does not exist in the live database", schema)
		}
	}

	// Positive control for the privilege function: platform_app does have USAGE on its own schema.
	var own bool
	if err := live.QueryRow(ctx, "SELECT has_schema_privilege('platform_app', 'platform', 'USAGE')").Scan(&own); err != nil || !own {
		t.Fatalf("platform_app has no USAGE on its own schema (%v, err %v): the question below would be meaningless", own, err)
	}

	app := dial(t, requireEnv(t, envAdminURL), appDial(requireEnv(t, envAppPassword)))
	for _, schema := range []string{"send_managed", "send_byo"} {
		var usage bool
		if err := live.QueryRow(ctx, "SELECT has_schema_privilege('platform_app', $1, 'USAGE')", schema).Scan(&usage); err != nil {
			t.Fatal(err)
		}
		if usage {
			t.Fatalf("platform_app has USAGE on schema %s: the platform's runtime identity can reach a module's objects", schema)
		}
		// And in practice, as a real login: naming an object in the schema is refused for want of USAGE on it, whether or
		// not the object exists (the schema is checked first).
		_, err := app.Exec(ctx, "SELECT 1 FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize())
		if err == nil {
			t.Fatalf("platform_app read %s.schema_migrations", schema)
		}
		if code := sqlState(err); code != "42501" || !strings.Contains(err.Error(), "schema "+schema) {
			t.Fatalf("platform_app reading %s.schema_migrations: SQLSTATE %q (%v), want 42501 permission denied for schema %s", schema, code, err, schema)
		}
	}
}
