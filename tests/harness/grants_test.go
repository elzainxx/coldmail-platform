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

// querier is what the checker needs; a *pgx.Conn and a pgx.Tx both satisfy it, so the anti-vacuity test can run the
// checker inside a transaction it then rolls back.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// privClass is the DECLARED privilege class of one table in schema platform: its owner, and for every role that holds
// anything the exact set of privileges. A role not named here holds nothing, and neither does PUBLIC. A table with no
// class in declaredClasses is a failure, so a migration that adds a table cannot ship without saying who may touch it.
type privClass struct {
	Owner  string
	Grants map[string][]string
}

var declaredClasses = map[string]privClass{
	"schema_migrations": {
		Owner:  "platform_owner",
		Grants: map[string][]string{"platform_app": {"SELECT"}, "platform_admin": {"SELECT"}},
	},
}

const grantSchema = "platform"

// checkGrants compares what the database actually grants in schema platform with declaredClasses, and returns every
// difference as a sentence. It never calls t.Error: the anti-vacuity test asserts on what it returns.
//
// What it covers: every table in the schema (pg_tables), its owner, its table-level privileges as the ACL stores them
// (aclexplode, with the owner's default ACL when none is stored), any column-level privilege, and the schema's own
// USAGE/CREATE for PUBLIC, platform_app and platform_admin. What it does NOT cover: sequences, views, functions and
// types in the schema (the platform has none yet), default privileges, and anything outside schema platform.
func checkGrants(ctx context.Context, q querier) ([]string, error) {
	var v []string

	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", grantSchema).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []string{"schema " + grantSchema + " does not exist"}, nil
	}

	owners := map[string]string{}
	rows, err := q.Query(ctx, "SELECT tablename, tableowner FROM pg_tables WHERE schemaname = $1 ORDER BY tablename", grantSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, owner string
		if err := rows.Scan(&table, &owner); err != nil {
			rows.Close()
			return nil, err
		}
		owners[table] = owner
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		v = append(v, "found 0 tables in schema "+grantSchema+": the instrument matched nothing (the bootstrap creates schema_migrations)")
	}

	// actual[table][grantee][privilege] = grantable
	actual := map[string]map[string]map[string]bool{}
	rows, err = q.Query(ctx, `
		SELECT c.relname,
		       CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,
		       a.privilege_type, a.is_grantable
		FROM pg_class c
		CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r', 'p')`, grantSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, grantee, priv string
		var grantable bool
		if err := rows.Scan(&table, &grantee, &priv, &grantable); err != nil {
			rows.Close()
			return nil, err
		}
		if actual[table] == nil {
			actual[table] = map[string]map[string]bool{}
		}
		if actual[table][grantee] == nil {
			actual[table][grantee] = map[string]bool{}
		}
		actual[table][grantee][priv] = grantable
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tables := make([]string, 0, len(owners))
	for table := range owners {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		name := grantSchema + "." + table
		class, declared := declaredClasses[table]
		if !declared {
			v = append(v, name+": no declared privilege class (add it to declaredClasses with its owner and exact grants)")
			continue
		}
		if owners[table] != class.Owner {
			v = append(v, fmt.Sprintf("%s: owned by %s, declared owner is %s", name, owners[table], class.Owner))
		}
		want := map[string]map[string]bool{}
		for role, privs := range class.Grants {
			want[role] = map[string]bool{}
			for _, p := range privs {
				want[role][p] = true
			}
		}
		for grantee, privs := range actual[table] {
			if grantee == owners[table] {
				continue // the owner's own privileges are implicit
			}
			for _, priv := range sortedKeys(privs) {
				if !want[grantee][priv] {
					declaredText := "nothing"
					if len(class.Grants[grantee]) > 0 {
						declaredText = strings.Join(class.Grants[grantee], ", ")
					}
					v = append(v, fmt.Sprintf("%s: over-grant: %s has %s (declared: %s)", name, grantee, priv, declaredText))
				}
				if privs[priv] {
					v = append(v, fmt.Sprintf("%s: %s has %s WITH GRANT OPTION", name, grantee, priv))
				}
			}
		}
		for _, role := range sortedKeys2(want) {
			for _, priv := range sortedKeys(want[role]) {
				if _, ok := actual[table][role][priv]; !ok {
					v = append(v, fmt.Sprintf("%s: missing grant: %s lacks %s", name, role, priv))
				}
			}
		}
	}
	for table := range declaredClasses {
		if _, ok := owners[table]; !ok && len(owners) > 0 {
			v = append(v, fmt.Sprintf("%s.%s has a declared privilege class but the table does not exist (the declaration is stale)", grantSchema, table))
		}
	}

	// Column-level privileges are none by declaration, and they do not show in the table ACL.
	rows, err = q.Query(ctx, `
		SELECT c.relname, att.attname,
		       CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, a.privilege_type
		FROM pg_class c
		JOIN pg_attribute att ON att.attrelid = c.oid AND att.attnum > 0 AND NOT att.attisdropped AND att.attacl IS NOT NULL
		CROSS JOIN LATERAL aclexplode(att.attacl) a
		WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r', 'p')
		ORDER BY 1, 2, 3, 4`, grantSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, col, grantee, priv string
		if err := rows.Scan(&table, &col, &grantee, &priv); err != nil {
			rows.Close()
			return nil, err
		}
		v = append(v, fmt.Sprintf("%s.%s: column privilege: %s has %s on column %s (none are declared)", grantSchema, table, grantee, priv, col))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The schema itself.
	var owner string
	if err := q.QueryRow(ctx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1", grantSchema).Scan(&owner); err != nil {
		return nil, err
	}
	if owner != "platform_owner" {
		v = append(v, fmt.Sprintf("schema %s: owned by %s, declared owner is platform_owner", grantSchema, owner))
	}
	for _, c := range []struct {
		role, priv string
		want       bool
	}{
		{"public", "USAGE", false}, {"public", "CREATE", false},
		{"platform_app", "USAGE", true}, {"platform_app", "CREATE", false},
		{"platform_admin", "USAGE", true}, {"platform_admin", "CREATE", false},
	} {
		var got bool
		if err := q.QueryRow(ctx, "SELECT has_schema_privilege($1, $2, $3)", c.role, grantSchema, c.priv).Scan(&got); err != nil {
			return nil, fmt.Errorf("has_schema_privilege(%s, %s, %s): %w", c.role, grantSchema, c.priv, err)
		}
		role := c.role
		if role == "public" {
			role = "PUBLIC"
		}
		switch {
		case got && !c.want:
			v = append(v, fmt.Sprintf("schema %s: %s has %s (it must not)", grantSchema, role, c.priv))
		case !got && c.want:
			v = append(v, fmt.Sprintf("schema %s: %s lacks %s (it must have it)", grantSchema, role, c.priv))
		}
	}
	return v, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func requireNoViolations(t *testing.T, v []string, err error, where string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: the grants check could not run: %v", where, err)
	}
	if len(v) != 0 {
		t.Fatalf("%s: %d grant violation(s):\n  %s", where, len(v), strings.Join(v, "\n  "))
	}
}

func hasViolation(v []string, parts ...string) bool {
	for _, line := range v {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// migratedScratch is a scratch database the real binary has migrated with the repository's chain.
func migratedScratch(t *testing.T) (scratch, *pgx.Conn) {
	t.Helper()
	s := newScratch(t)
	admin := s.admin(t)
	res := runMigrator(t, migEnv(s.DSN, repoMigrationsDir(t), nil), "up")
	mustExit(t, res, 0, "up on the scratch database")
	if got, want := appliedVersions(t, admin), chainOnDisk(t, repoMigrationsDir(t)); !sameStrings(got, want) {
		t.Fatalf("the scratch database holds %v after up, want %v", got, want)
	}
	return s, admin
}

// TestGrantsInvariant: on a freshly migrated database every table in schema platform has exactly its declared privileges.
func TestGrantsInvariant(t *testing.T) {
	_, admin := migratedScratch(t)
	v, err := checkGrants(testCtx(t), admin)
	requireNoViolations(t, v, err, "a freshly migrated scratch database")
}

// TestGrantsInvariant_RoleSwitchActuallyBites is the paired control for the grants proof: the declared privileges are not
// merely what the catalog says, they are what a session running as the role can and cannot do. SET LOCAL ROLE is checked
// to have really switched (current_user changed, session_user did not), because a switch that silently does nothing
// would leave every denial below unproven.
func TestGrantsInvariant_RoleSwitchActuallyBites(t *testing.T) {
	_, admin := migratedScratch(t)
	for _, role := range []string{"platform_app", "platform_admin"} {
		role := role
		t.Run(role, func(t *testing.T) { roleSwitchBites(t, admin, role) })
	}
}

func roleSwitchBites(t *testing.T, admin *pgx.Conn, role string) {
	t.Helper()
	ctx := testCtx(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) // nothing here is ever committed

	var sessionUser, currentUser string
	var superuser bool
	if err := tx.QueryRow(ctx, "SELECT session_user, current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = session_user)").Scan(&sessionUser, &currentUser, &superuser); err != nil {
		t.Fatal(err)
	}
	if !superuser || currentUser != sessionUser {
		t.Fatalf("precondition: the admin connection must be a superuser running as itself, got session_user=%s current_user=%s superuser=%v", sessionUser, currentUser, superuser)
	}

	denied := []struct{ what, sql string }{
		{"INSERT", "INSERT INTO platform.schema_migrations (version) VALUES ('zz-denied')"},
		{"UPDATE", "UPDATE platform.schema_migrations SET version = version"},
		{"DELETE", "DELETE FROM platform.schema_migrations"},
		{"TRUNCATE", "TRUNCATE platform.schema_migrations"},
		{"DROP TABLE", "DROP TABLE platform.schema_migrations"},
		{"CREATE TABLE in the schema", "CREATE TABLE platform.zz_created_by_role (id int)"},
	}

	// Control: before the switch the same statements all SUCCEED, each in its own savepoint that is rolled back. So a
	// denial after the switch is the role's doing, not a missing table or a typo.
	for _, d := range denied {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sp.Exec(ctx, d.sql); err != nil {
			t.Fatalf("control: %s as the superuser failed, so the denial below would prove nothing: %v", d.what, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET LOCAL ROLE %s: %v", role, err)
	}
	var nowCurrent, nowSession string
	if err := tx.QueryRow(ctx, "SELECT current_user, session_user").Scan(&nowCurrent, &nowSession); err != nil {
		t.Fatal(err)
	}
	if nowCurrent != role || nowSession != sessionUser {
		t.Fatalf("the role switch did not happen: current_user=%s session_user=%s, want current_user=%s session_user=%s", nowCurrent, nowSession, role, sessionUser)
	}

	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM platform.schema_migrations").Scan(&n); err != nil {
		t.Fatalf("as %s: SELECT on platform.schema_migrations failed: %v", role, err)
	}
	if n < 1 {
		t.Fatalf("as %s: platform.schema_migrations has %d rows: the SELECT proves nothing on an empty table", role, n)
	}

	for _, d := range denied {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, d.sql)
		if err == nil {
			t.Fatalf("as %s: %s was ALLOWED", role, d.what)
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("as %s: %s failed with SQLSTATE %q (%v), want 42501 insufficient_privilege", role, d.what, code, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestGrantsInvariant_AntiVacuity proves the checker can fail. On a database where nothing is wrong it reports nothing;
// against each of the breakages below it reports that breakage, by the name of the table, role and privilege. Every
// breakage is made inside a transaction that is rolled back, and the database is checked clean again at the end.
func TestGrantsInvariant_AntiVacuity(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)

	v, err := checkGrants(ctx, admin)
	requireNoViolations(t, v, err, "the control: a freshly migrated scratch database")

	cases := []struct {
		name  string
		setup []string
		want  [][]string // each entry: substrings that must all appear in one violation
	}{
		{"a table nobody declared",
			[]string{"CREATE TABLE platform.zz_ungranted (id int)", "ALTER TABLE platform.zz_ungranted OWNER TO platform_owner"},
			[][]string{{"platform.zz_ungranted", "no declared privilege class"}}},
		{"an over-grant to platform_app",
			[]string{"GRANT UPDATE ON platform.schema_migrations TO platform_app"},
			[][]string{{"platform_app", "UPDATE", "over-grant"}}},
		{"an over-grant to platform_admin",
			[]string{"GRANT DELETE ON platform.schema_migrations TO platform_admin"},
			[][]string{{"platform_admin", "DELETE", "over-grant"}}},
		{"a grant to PUBLIC",
			[]string{"GRANT SELECT ON platform.schema_migrations TO PUBLIC"},
			[][]string{{"PUBLIC", "SELECT", "over-grant"}}},
		{"a column-level grant",
			[]string{"GRANT UPDATE (version) ON platform.schema_migrations TO platform_app"},
			[][]string{{"column privilege", "platform_app", "UPDATE", "version"}}},
		{"a grant option",
			[]string{"GRANT SELECT ON platform.schema_migrations TO platform_app WITH GRANT OPTION"},
			[][]string{{"platform_app", "SELECT", "WITH GRANT OPTION"}}},
		{"a missing grant",
			[]string{"REVOKE SELECT ON platform.schema_migrations FROM platform_admin"},
			[][]string{{"missing grant", "platform_admin", "SELECT"}}},
		{"the wrong owner",
			[]string{"ALTER TABLE platform.schema_migrations OWNER TO platform_admin"},
			[][]string{{"owned by platform_admin", "platform_owner"}}},
		{"PUBLIC allowed into the schema",
			[]string{"GRANT USAGE ON SCHEMA platform TO PUBLIC"},
			[][]string{{"schema platform", "PUBLIC has USAGE"}}},
		{"platform_app allowed to create in the schema",
			[]string{"GRANT CREATE ON SCHEMA platform TO platform_app"},
			[][]string{{"schema platform", "platform_app has CREATE"}}},
		{"platform_app locked out of the schema",
			[]string{"REVOKE USAGE ON SCHEMA platform FROM platform_app"},
			[][]string{{"schema platform", "platform_app lacks USAGE"}}},
		{"an empty schema",
			[]string{"DROP TABLE platform.schema_migrations"},
			[][]string{{"found 0 tables"}, {"declared privilege class but the table does not exist"}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, stmt := range c.setup {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			got, err := checkGrants(ctx, tx)
			if err != nil {
				t.Fatalf("the checker could not run: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("the checker reported nothing for a broken database: it cannot fail")
			}
			for _, want := range c.want {
				if !hasViolation(got, want...) {
					t.Fatalf("no violation mentions %v; the checker said:\n  %s", want, strings.Join(got, "\n  "))
				}
			}
		})
	}

	// Every breakage was rolled back: the database is clean again.
	v, err = checkGrants(ctx, admin)
	requireNoViolations(t, v, err, "the database after the rolled-back breakages")
}
