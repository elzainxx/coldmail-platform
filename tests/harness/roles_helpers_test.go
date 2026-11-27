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

// Helpers of the role proofs (roles_test.go) and of the lifecycle test's ownership assertions (migrate_lifecycle_test.go). Everything here is prefixed `roles` or `platform` so that it cannot
// collide with the helpers of the other harness files.

// ---- the rows every role proof starts from ----

// The identities the proofs use. All of them are made up: nothing here is a real user.
const (
	rolesU1    = "11111111-1111-4111-8111-111111111111" // a tenant that owns a row in every table that has a user
	rolesU2    = "22222222-2222-4222-8222-222222222222" // a tenant that owns nothing: safe to insert for, update and delete
	rolesU3    = "33333333-3333-4333-8333-333333333333" // has an auth.users row but no public.users row yet: the user an INSERT creates
	rolesAdm   = "44444444-4444-4444-8444-444444444444" // the named admin that ledger, audit and price-change rows refer to
	rolesAdm2  = "55555555-5555-4555-8555-555555555555" // an admin nothing refers to: safe to delete
	rolesJob   = "66666666-6666-4666-8666-666666666666" // a job the price snapshot and the ledger may refer to
	rolesJob2  = "77777777-7777-4777-8777-777777777777" // a job nothing refers to: safe to delete, and to insert a price row for
	rolesJob3  = "88888888-8888-4888-8888-888888888888" // a CONFIRMED module 2 job: its two rows are frozen, one has its result (platform.job_rows, platform.job_results)
	rolesRow1  = "99999999-9999-4999-8999-999999999991" // the one row of rolesJob (a draft: the row may be changed and deleted)
	rolesRow3A = "99999999-9999-4999-8999-99999999999a" // a row of rolesJob3 that has its result
	rolesRow3B = "99999999-9999-4999-8999-99999999999b" // a row of rolesJob3 that has none yet (the proof inserts its result)
)

// rolesExpand puts the identities into a statement: {U1} becomes '<uuid>'::uuid, and so on.
func rolesExpand(sql string) string {
	return strings.NewReplacer(
		"{U1}", "'"+rolesU1+"'::uuid", "{U2}", "'"+rolesU2+"'::uuid", "{U3}", "'"+rolesU3+"'::uuid",
		"{ADM2}", "'"+rolesAdm2+"'::uuid", "{ADM}", "'"+rolesAdm+"'::uuid",
		"{JOB2}", "'"+rolesJob2+"'::uuid", "{JOB3}", "'"+rolesJob3+"'::uuid", "{JOB}", "'"+rolesJob+"'::uuid",
		"{ROW1}", "'"+rolesRow1+"'::uuid", "{ROW3A}", "'"+rolesRow3A+"'::uuid", "{ROW3B}", "'"+rolesRow3B+"'::uuid",
	).Replace(sql)
}

// rolesSeedSQL gives every table of the platform at least one row, run as the superuser: a refusal or a COUNT on an empty table proves less than one on a table with rows, and a
// trigger that guards a row (the append-only ones) only fires when a row exists. The ledger inserts also give balances its row and (the reserve, at the dearest price of the job's complete
// snapshot) platform.reserves its row, and the entitlement insert gives platform.m3_sync its row: those four tables have one writer each, a trigger.
const rolesSeedSQL = `
INSERT INTO auth.users (id, email) VALUES ({U1}, 'roles-u1@example.test'), ({U2}, 'roles-u2@example.test'), ({U3}, 'roles-u3@example.test'), ({ADM}, 'roles-adm@example.test'), ({ADM2}, 'roles-adm2@example.test');
INSERT INTO public.users (id, auth_user_id, email) VALUES ({U1}, {U1}, 'roles-u1@example.test'), ({U2}, {U2}, 'roles-u2@example.test'), ({ADM}, {ADM}, 'roles-adm@example.test'), ({ADM2}, {ADM2}, 'roles-adm2@example.test');
INSERT INTO platform.admins (id, display_name) VALUES ({ADM}, 'Roles Admin'), ({ADM2}, 'Roles Admin Two');
DO $do$ BEGIN IF to_regprocedure('platform.set_unit_reachable(text, boolean, text)') IS NOT NULL THEN PERFORM platform.set_unit_reachable('m1_vendor', true, 'roles seed: the vendor fallback is on'); END IF; END $do$;
INSERT INTO platform.jobs (id, user_id, module_id) VALUES ({JOB}, {U1}, 'm1'), ({JOB2}, {U1}, 'm1');
INSERT INTO platform.job_events (job_id, kind, actor_kind, reason) VALUES ({JOB}, 'created', 'engine', 'roles seed');
INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT {JOB}, module_id, unit, cost FROM platform.unit_costs WHERE module_id = 'm1';
INSERT INTO platform.jobs (id, user_id, module_id, preview, preview_fingerprint, confirm_key) VALUES ({JOB3}, {U1}, 'm2', '{}', repeat('a', 64), 'roles-seed');
INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm) VALUES ({ROW1}, {JOB}, {U1}, 1, 'seed@example.com', 'seed@example.com'), ({ROW3A}, {JOB3}, {U1}, 1, 'a@example.com', 'a@example.com'), ({ROW3B}, {JOB3}, {U1}, 2, 'b@example.com', 'b@example.com');
SELECT platform.job_transition({JOB3}, 'previewed', 'user', {U1}, 'roles seed');
DO $do$ BEGIN
  PERFORM set_config('platform.actor_kind', 'user', true); PERFORM set_config('platform.actor_id', {U1}::text, true); PERFORM set_config('platform.reason', 'roles seed', true);
  UPDATE platform.jobs SET status = 'confirmed', start_deadline = now() + interval '1 day' WHERE id = {JOB3};
  PERFORM set_config('platform.actor_kind', '', true); PERFORM set_config('platform.actor_id', '', true); PERFORM set_config('platform.reason', '', true);
END $do$;
INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ({JOB3}, {U1}, {ROW3A}, 1, 'a@example.com', 'module', 'sent', 'free', 'seed');
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key) VALUES ({U1}, 'm1', 'grant', 100, {ADM}, 'roles seed', 'roles-seed-grant');
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, unit, units, unit_cost, idempotency_key) VALUES ({U1}, 'm1', 'reserve', -20, 20, {JOB}, 'm1_vendor', 10, 2, 'roles-seed-reserve');
INSERT INTO platform.m3_entitlements (user_id, enabled, changed_by) VALUES ({U1}, true, {ADM});
INSERT INTO platform.suppressions (user_id, email, source) VALUES ({U1}, 'seed@example.com', 'manual');
INSERT INTO platform.suppression_removals (user_id, email, source, entry_created_at) VALUES ({U1}, 'seed-removed@example.com', 'manual', now());
INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, reason) VALUES ({ADM}, 'action', 'roles-seed', 'user', 'x', 'seeding');
INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ({U1}, {U1}, 'seed@example.com', 'subject', 'body');
INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ({U1}, true);
`

// rolesBegin opens the transaction every role proof runs in (never committed) and checks that its session is a superuser running as itself, so the seeding and the controls
// below are not themselves subject to any grant.
func rolesBegin(t *testing.T, ctx context.Context, admin *pgx.Conn) pgx.Tx {
	t.Helper()
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) }) // nothing here is ever committed
	var sessionUser, currentUser string
	var superuser bool
	if err := tx.QueryRow(ctx, "SELECT session_user, current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&sessionUser, &currentUser, &superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser || currentUser != sessionUser {
		t.Fatalf("precondition: the admin connection must be a superuser running as itself, got session_user=%s current_user=%s superuser=%v", sessionUser, currentUser, superuser)
	}
	return tx
}

// rolesSeed runs rolesSeedSQL as the superuser inside tx.
func rolesSeed(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, rolesExpand(rolesSeedSQL)); err != nil {
		t.Fatalf("seed the rows of the role proofs: %v", err)
	}
}

// rolesRequireRows fails unless every table in rels has at least one row (as the current role): the proofs would otherwise be about empty tables.
func rolesRequireRows(t *testing.T, ctx context.Context, tx pgx.Tx, rels []string) {
	t.Helper()
	for _, rel := range rels {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+rel).Scan(&n); err != nil {
			t.Fatalf("count(*) FROM %s: %v", rel, err)
		}
		if n < 1 {
			t.Fatalf("%s has no rows after seeding: a proof about it would be about an empty table", rel)
		}
	}
}

// rolesTry runs one statement in a savepoint that is always rolled back, and returns the statement's own error (an error of the savepoint machinery fails the test).
func rolesTry(t *testing.T, ctx context.Context, tx pgx.Tx, sql string) error {
	t.Helper()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, execErr := sp.Exec(ctx, sql)
	if err := sp.Rollback(ctx); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
	return execErr
}

// rolesSwitch makes tx run as role and fails unless the switch really happened: current_user changed, session_user did not. A switch that silently did nothing would turn every refusal
// that follows into a pass for the wrong reason, or every allowed statement into a superuser's.
func rolesSwitch(t *testing.T, ctx context.Context, tx pgx.Tx, role string) {
	t.Helper()
	var sessionBefore string
	if err := tx.QueryRow(ctx, "SELECT session_user").Scan(&sessionBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET LOCAL ROLE %s: %v", role, err)
	}
	var nowCurrent, nowSession string
	var super bool
	if err := tx.QueryRow(ctx, "SELECT current_user, session_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)").Scan(&nowCurrent, &nowSession, &super); err != nil {
		t.Fatal(err)
	}
	if nowCurrent != role || nowSession != sessionBefore || super {
		t.Fatalf("the role switch did not happen: current_user=%s session_user=%s superuser=%v, want current_user=%s session_user=%s superuser=false", nowCurrent, nowSession, super, role, sessionBefore)
	}
}

// ---- the platform_app login and its role-level settings ----

// rolesTimezoneSetting is the VALUE of the timezone setting stored for role in pg_db_role_setting for the CURRENT database (platform_0001 scopes it with IN DATABASE; roles are cluster-wide), or "" when there is none. ALTER ROLE ... SET writes
// it (the catalogue stores the name as "TimeZone=UTC", whatever case was typed); it is what makes a login session as that role start in that zone.
func rolesTimezoneSetting(ctx context.Context, q querier, role string) (string, error) {
	var settings []string
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT s.setconfig FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1 AND s.setdatabase = (SELECT d.oid FROM pg_database d WHERE d.datname = current_database())), ARRAY[]::text[])`, role).Scan(&settings)
	if err != nil {
		return "", err
	}
	for _, s := range settings {
		if name, value, ok := strings.Cut(s, "="); ok && strings.EqualFold(name, "timezone") {
			return value, nil
		}
	}
	return "", nil
}

// rolesAppLogin opens a real network login to the database named by dsn AS platform_app (not a role switch), with the password the harness stack gave the service. mutate may change the
// parsed connection config (for example to override a session parameter). The driver turns PGTZ and PGOPTIONS in the test process's environment into startup parameters, which would
// override a role's timezone setting and hide what the role setting does: they are removed first.
func rolesAppLogin(t *testing.T, dsn string, mutate func(*pgx.ConnConfig)) *pgx.Conn {
	t.Helper()
	pw := requireEnv(t, envAppPassword)
	return dial(t, dsn, func(cfg *pgx.ConnConfig) {
		cfg.User, cfg.Password = "platform_app", pw
		delete(cfg.RuntimeParams, "timezone")
		delete(cfg.RuntimeParams, "options")
		if mutate != nil {
			mutate(cfg)
		}
	})
}

// ---- who owns what (the lifecycle test's ownership assertions) ----

// pgObject is one catalogue object and its owner.
type pgObject struct{ Kind, Name, Owner string }

// platformObjects lists every relation of schema platform (tables, views, sequences, indexes, anything else pg_class holds there) and every function, each with its owner.
func platformObjects(t *testing.T, c *pgx.Conn) []pgObject {
	t.Helper()
	return catalogObjects(t, c, `
		SELECT CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table'
		                      WHEN 'S' THEN 'sequence' WHEN 'i' THEN 'index' WHEN 'I' THEN 'index' ELSE 'relation of kind ' || c.relkind::text END,
		       c.relname::text, pg_get_userbyid(c.relowner)
		FROM pg_class c WHERE c.relnamespace = 'platform'::regnamespace
		UNION ALL
		SELECT 'function', p.proname::text || '(' || pg_get_function_identity_arguments(p.oid) || ')', pg_get_userbyid(p.proowner)
		FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace
		ORDER BY 1, 2`)
}

// publicPlatformObjects lists the platform's two tables in schema public and the indexes that belong to them (they have no sequences: their ids are uuids), each with its owner. (Nothing else in schema public is the
// platform's: the allowlist gate judges the rest.)
func publicPlatformObjects(t *testing.T, c *pgx.Conn) []pgObject {
	t.Helper()
	return catalogObjects(t, c, `
		SELECT CASE c.relkind WHEN 'r' THEN 'table' WHEN 'i' THEN 'index' ELSE 'relation of kind ' || c.relkind::text END, c.relname::text, pg_get_userbyid(c.relowner)
		FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace
		  AND (c.relname::text IN ('users', 'job_items')
		       OR c.oid IN (SELECT i.indexrelid FROM pg_index i JOIN pg_class t ON t.oid = i.indrelid WHERE t.relnamespace = 'public'::regnamespace AND t.relname::text IN ('users', 'job_items')))
		ORDER BY 1, 2`)
}

func catalogObjects(t *testing.T, c *pgx.Conn, sql string) []pgObject {
	t.Helper()
	rows, err := c.Query(testCtx(t), sql)
	if err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	defer rows.Close()
	var out []pgObject
	for rows.Next() {
		var o pgObject
		if err := rows.Scan(&o.Kind, &o.Name, &o.Owner); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// namesOfKind returns the names of the objects of one kind, in the order they were listed (sorted by name).
func namesOfKind(objs []pgObject, kinds ...string) []string {
	var out []string
	for _, o := range objs {
		for _, k := range kinds {
			if o.Kind == k {
				out = append(out, o.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// requireOwnedBy fails unless every object in objs is owned by owner, naming the first that is not.
func requireOwnedBy(t *testing.T, objs []pgObject, owner, what string) {
	t.Helper()
	for _, o := range objs {
		if o.Owner != owner {
			t.Fatalf("%s: %s %s is owned by %s, want %s", what, o.Kind, o.Name, o.Owner, owner)
		}
	}
}

// requireKinds fails unless objs holds at least one object of every kind listed: the instrument must have matched something of each kind it claims to check.
func requireKinds(t *testing.T, objs []pgObject, what string, kinds ...string) {
	t.Helper()
	for _, k := range kinds {
		if len(namesOfKind(objs, k)) == 0 {
			t.Fatalf("%s: found 0 objects of kind %q: the ownership check matched nothing of that kind", what, k)
		}
	}
}

func describeObjects(objs []pgObject) string {
	parts := make([]string, 0, len(objs))
	for _, o := range objs {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", o.Kind, o.Name, o.Owner))
	}
	return strings.Join(parts, ", ")
}
