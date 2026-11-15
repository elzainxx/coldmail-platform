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

// privClass is the DECLARED privilege class of one relation (table or view): its owner, and for every role that holds
// anything the exact set of privileges. A role not named here holds nothing, and neither does PUBLIC. A relation with no
// class is a failure, so a migration that adds a table or a view cannot ship without saying who may touch it.
//
// ColumnGrants declares the privileges held at COLUMN level (GRANT UPDATE (a, b) ON ...), which never show in the table's
// ACL: role -> privilege -> the exact columns. A column privilege nobody declared is a violation, and so is a declared one
// that is missing. (Phase 0 declared none; platform.unit_costs is the one relation that needs them.)
type privClass struct {
	Owner        string
	Grants       map[string][]string
	ColumnGrants map[string]map[string][]string
}

var (
	privSelect       = []string{"SELECT"}
	privSelectInsert = []string{"SELECT", "INSERT"}
	privSelectInsUpd = []string{"SELECT", "INSERT", "UPDATE"}
)

// classOf is the class of a relation owned by platform_owner on which platform_app and platform_admin hold exactly these table-level privileges.
func classOf(app, admin []string) privClass {
	return privClass{Owner: "platform_owner", Grants: map[string][]string{"platform_app": app, "platform_admin": admin}}
}

// declaredClasses: every table AND VIEW in schema platform. The privileges are the ones the migrations grant (platform_0000, 0002, 0003), written out here independently of
// them: the checker compares the two, so a migration that grants more, or less, fails until the declaration is changed on purpose. (The behavioural twin of this table, written
// separately, is roleExpectations in roles_test.go; TestRoles_ExpectationsAgreeWithDeclaredClasses makes the two agree.)
var declaredClasses = map[string]privClass{
	"schema_migrations": classOf(privSelect, privSelect),
	// the registry, the admins, the price history, the balances (written only by the ledger trigger) and the two recompute views: read-only for both roles
	"admins":            classOf(privSelect, privSelect),
	"modules":           classOf(privSelect, privSelect),
	"unit_cost_history": classOf(privSelect, privSelect),
	"balances":          classOf(privSelect, privSelect),
	"balance_drift":     classOf(privSelect, privSelect),
	"reserve_overdrawn": classOf(privSelect, privSelect),
	// the unit costs: read-only at table level; platform_app may change exactly the three columns an admin price change needs (the trigger demands a named admin and a reason)
	"unit_costs": {
		Owner:        "platform_owner",
		Grants:       map[string][]string{"platform_app": privSelect, "platform_admin": privSelect},
		ColumnGrants: map[string]map[string][]string{"platform_app": {"UPDATE": {"cost", "changed_by", "change_reason"}}},
	},
	"jobs":            classOf(privSelectInsUpd, privSelect),
	"job_unit_prices": classOf(privSelectInsert, privSelect),
	"ledger":          classOf(privSelectInsert, privSelectInsert),
	"m3_entitlements": classOf(privSelectInsUpd, privSelect),
	"suppressions":    classOf(privSelectInsert, privSelect),
	"admin_audit":     classOf(privSelectInsert, privSelectInsert),
}

const grantSchema = "platform"

// publicClasses: the two tables the platform owns in schema public (platform_0001) because modules 2 and 3 read them. No other relation of schema public is judged here: the
// allowlist gate (scripts/check-public-schema.sh) owns that question. Nobody holds DELETE or TRUNCATE on either: module foreign keys cascade from them.
var publicClasses = map[string]privClass{
	"users":     classOf(privSelectInsUpd, privSelect),
	"job_items": classOf(privSelectInsUpd, privSelect),
}

const publicSchema = "public"

// moduleRole is a module's own runtime role and what the MODULE'S OWN migrations grant it on the platform's two public tables (module 2: send_managed_0000; module 3:
// send_byo_0000 and send_byo_0037). The platform grants these roles nothing; the checker allows exactly these sets, and demands them only when the module's schema is present in
// the database being checked.
type moduleRole struct {
	Role   string
	Schema string // the module's own schema: present in this database iff the module's migrations ran here
	Grants map[string][]string
}

var moduleRoles = []moduleRole{
	{Role: "send_managed_app", Schema: "send_managed", Grants: map[string][]string{"users": privSelect, "job_items": privSelect}},
	{Role: "send_byo_app", Schema: "send_byo", Grants: map[string][]string{"users": privSelect, "job_items": privSelectInsert}},
}

// supabaseAPIRoles exist on a Supabase project only; they must hold nothing on the platform's public tables (platform_0001 revokes them).
var supabaseAPIRoles = []string{"anon", "authenticated", "service_role"}

// relkinds the checker judges: tables, partitioned tables, views, materialized views, foreign tables.
const judgedRelkinds = "('r', 'p', 'v', 'm', 'f')"

// checkGrants compares what the database actually grants with the declarations, and returns every difference as a
// sentence. It never calls t.Error: the anti-vacuity test asserts on what it returns.
//
// What it covers:
//   - schema platform: EVERY table, partitioned table, view, materialized view and foreign table (pg_class) - its owner, its table-level privileges as the ACL stores
//     them (aclexplode, with the owner's default ACL when none is stored), every COLUMN-level privilege against the declared columns, and the schema's own
//     USAGE/CREATE for PUBLIC, platform_app and platform_admin;
//   - public.users and public.job_items: the same per-relation judgement (owner, table ACL, column privileges), the module roles send_managed_app and send_byo_app
//     allowed EXACTLY their declared sets (demanded only when the role exists AND the module's schema is in this database: a role is cluster-wide, so a scratch database
//     can see a module role whose module never ran in it), and the question "can anyone DELETE or TRUNCATE?" asked, by EFFECTIVE privilege (has_table_privilege, so a grant
//     inherited through role membership counts), of PUBLIC, platform_app, platform_admin, the two module roles and the Supabase API roles (anon, authenticated, service_role).
//
// What it does NOT cover, and says so: sequences (identity or serial), functions and procedures, types, default privileges (ALTER DEFAULT PRIVILEGES), row-level security;
// an inherited DELETE/TRUNCATE of a role that is not in the list above (a direct grant to ANY role is caught by the ACL comparison; only the inheritance question is limited
// to the listed subjects); schema public itself (its USAGE/CREATE are not judged) and every other relation in it (the allowlist gate does that); every other schema.
func checkGrants(ctx context.Context, q querier) ([]string, error) {
	var v []string

	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", grantSchema).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []string{"schema " + grantSchema + " does not exist"}, nil
	}

	// ---- schema platform: every relation ----
	rels, err := loadRelations(ctx, q, grantSchema, nil)
	if err != nil {
		return nil, err
	}
	acl, err := loadTableACL(ctx, q, grantSchema, nil)
	if err != nil {
		return nil, err
	}
	cols, err := loadColumnACL(ctx, q, grantSchema, nil)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		v = append(v, "found 0 tables in schema "+grantSchema+": the instrument matched nothing (the bootstrap creates schema_migrations)")
	}
	v = append(v, judgeRelations(grantSchema, rels, acl, cols, declaredClasses, nil, true)...)

	// ---- the platform's two tables in schema public ----
	publicNames := sortedClassNames(publicClasses)
	classes, optional, err := publicExpectation(ctx, q)
	if err != nil {
		return nil, err
	}
	prels, err := loadRelations(ctx, q, publicSchema, publicNames)
	if err != nil {
		return nil, err
	}
	pacl, err := loadTableACL(ctx, q, publicSchema, publicNames)
	if err != nil {
		return nil, err
	}
	pcols, err := loadColumnACL(ctx, q, publicSchema, publicNames)
	if err != nil {
		return nil, err
	}
	v = append(v, judgeRelations(publicSchema, prels, pacl, pcols, classes, optional, false)...)
	nv, err := checkNoDestructivePrivileges(ctx, q, publicNames, prels)
	if err != nil {
		return nil, err
	}
	v = append(v, nv...)

	// ---- the schema itself (platform only: schema public is not judged here) ----
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

// relation is one pg_class row the checker judges.
type relation struct{ Name, Kind, Owner string }

// tableACL[relation][grantee][privilege] = grantable
type tableACL map[string]map[string]map[string]bool

// columnACL is one column-level privilege as the catalogue stores it.
type columnACL struct {
	Rel, Col, Grantee, Priv string
	Grantable               bool
}

// loadRelations reads the judged relations of a schema (only: when non-nil, just those names).
func loadRelations(ctx context.Context, q querier, schema string, only []string) (map[string]relation, error) {
	sql := `SELECT c.relname::text, c.relkind::text, pg_get_userbyid(c.relowner)
	        FROM pg_class c WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ` + judgedRelkinds
	args := []any{schema}
	if only != nil {
		sql += ` AND c.relname::text = ANY($2::text[])`
		args = append(args, only)
	}
	rows, err := q.Query(ctx, sql+` ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]relation{}
	for rows.Next() {
		var r relation
		if err := rows.Scan(&r.Name, &r.Kind, &r.Owner); err != nil {
			return nil, err
		}
		out[r.Name] = r
	}
	return out, rows.Err()
}

func loadTableACL(ctx context.Context, q querier, schema string, only []string) (tableACL, error) {
	sql := `SELECT c.relname::text,
	               CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,
	               a.privilege_type, a.is_grantable
	        FROM pg_class c
	        CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
	        WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ` + judgedRelkinds
	args := []any{schema}
	if only != nil {
		sql += ` AND c.relname::text = ANY($2::text[])`
		args = append(args, only)
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := tableACL{}
	for rows.Next() {
		var table, grantee, priv string
		var grantable bool
		if err := rows.Scan(&table, &grantee, &priv, &grantable); err != nil {
			return nil, err
		}
		if out[table] == nil {
			out[table] = map[string]map[string]bool{}
		}
		if out[table][grantee] == nil {
			out[table][grantee] = map[string]bool{}
		}
		out[table][grantee][priv] = grantable
	}
	return out, rows.Err()
}

// loadColumnACL reads the column-level privileges, which do not show in the table ACL.
func loadColumnACL(ctx context.Context, q querier, schema string, only []string) ([]columnACL, error) {
	sql := `SELECT c.relname::text, att.attname::text,
	               CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, a.privilege_type, a.is_grantable
	        FROM pg_class c
	        JOIN pg_attribute att ON att.attrelid = c.oid AND att.attnum > 0 AND NOT att.attisdropped AND att.attacl IS NOT NULL
	        CROSS JOIN LATERAL aclexplode(att.attacl) a
	        WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ` + judgedRelkinds
	args := []any{schema}
	if only != nil {
		sql += ` AND c.relname::text = ANY($2::text[])`
		args = append(args, only)
	}
	rows, err := q.Query(ctx, sql+` ORDER BY 1, 2, 3, 4`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []columnACL
	for rows.Next() {
		var c columnACL
		if err := rows.Scan(&c.Rel, &c.Col, &c.Grantee, &c.Priv, &c.Grantable); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// judgeRelations is the pure comparison: what the catalogue says (rels, acl, cols) against what is declared (classes). optional names roles whose declared grants are ALLOWED but
// not demanded (a module role whose module has not run in this database). When the schema holds no relation at all and emptyIsSaidOnce is set, the stale-declaration lines are left out:
// the caller reports an empty schema once, as "found 0 tables".
func judgeRelations(schema string, rels map[string]relation, acl tableACL, cols []columnACL, classes map[string]privClass, optional map[string]bool, emptyIsSaidOnce bool) []string {
	var v []string
	names := make([]string, 0, len(rels))
	for n := range rels {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, table := range names {
		rel := rels[table]
		name := schema + "." + table
		class, declared := classes[table]
		if !declared {
			v = append(v, name+": no declared privilege class (add it to declaredClasses with its owner and exact grants)")
			continue
		}
		if rel.Owner != class.Owner {
			v = append(v, fmt.Sprintf("%s: owned by %s, declared owner is %s", name, rel.Owner, class.Owner))
		}
		want := map[string]map[string]bool{}
		for role, privs := range class.Grants {
			want[role] = map[string]bool{}
			for _, p := range privs {
				want[role][p] = true
			}
		}
		grantees := make([]string, 0, len(acl[table]))
		for g := range acl[table] {
			grantees = append(grantees, g)
		}
		sort.Strings(grantees)
		for _, grantee := range grantees {
			if grantee == rel.Owner {
				continue // the owner's own privileges are implicit
			}
			privs := acl[table][grantee]
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
			if optional[role] {
				continue
			}
			for _, priv := range sortedKeys(want[role]) {
				if _, ok := acl[table][role][priv]; !ok {
					v = append(v, fmt.Sprintf("%s: missing grant: %s lacks %s", name, role, priv))
				}
			}
		}
	}
	for _, table := range sortedClassNames(classes) {
		if _, ok := rels[table]; !ok && !(emptyIsSaidOnce && len(rels) == 0) {
			v = append(v, fmt.Sprintf("%s.%s has a declared privilege class but the table or view does not exist (the declaration is stale)", schema, table))
		}
	}

	// Column-level privileges: every one found must be declared (role, privilege, column), and every declared one must be found. They do not show in the table ACL.
	haveCol := map[string]bool{}
	for _, c := range cols {
		name := schema + "." + c.Rel
		haveCol[c.Rel+"\x00"+c.Grantee+"\x00"+c.Priv+"\x00"+c.Col] = true
		declaredCols := classes[c.Rel].ColumnGrants[c.Grantee][c.Priv] // nil for an undeclared relation, role or privilege
		if !containsString(declaredCols, c.Col) {
			text := "none"
			if len(declaredCols) > 0 {
				text = strings.Join(declaredCols, ", ")
			}
			v = append(v, fmt.Sprintf("%s: column privilege: %s has %s on column %s (declared columns for %s %s: %s)", name, c.Grantee, c.Priv, c.Col, c.Grantee, c.Priv, text))
		}
		if c.Grantable {
			v = append(v, fmt.Sprintf("%s: column privilege: %s has %s on column %s WITH GRANT OPTION", name, c.Grantee, c.Priv, c.Col))
		}
	}
	for _, table := range sortedClassNames(classes) {
		if _, ok := rels[table]; !ok {
			continue
		}
		for _, role := range sortedMapKeys(classes[table].ColumnGrants) {
			for _, priv := range sortedMapKeys(classes[table].ColumnGrants[role]) {
				for _, col := range classes[table].ColumnGrants[role][priv] {
					if !haveCol[table+"\x00"+role+"\x00"+priv+"\x00"+col] {
						v = append(v, fmt.Sprintf("%s.%s: missing column grant: %s lacks %s on column %s", schema, table, role, priv, col))
					}
				}
			}
		}
	}
	return v
}

// publicExpectation returns the classes of the two public tables as they apply to THIS database: the platform's own declaration plus, for each module role that exists in the
// cluster, the exact set that module's migrations grant it. optional names the module roles whose module schema is not in this database: allowed their set, not required to hold it.
func publicExpectation(ctx context.Context, q querier) (map[string]privClass, map[string]bool, error) {
	classes := map[string]privClass{}
	for table, c := range publicClasses {
		g := map[string][]string{}
		for role, privs := range c.Grants {
			g[role] = privs
		}
		classes[table] = privClass{Owner: c.Owner, Grants: g, ColumnGrants: c.ColumnGrants}
	}
	optional := map[string]bool{}
	for _, m := range moduleRoles {
		var roleExists, moduleRan bool
		if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1), to_regnamespace($2) IS NOT NULL", m.Role, m.Schema).Scan(&roleExists, &moduleRan); err != nil {
			return nil, nil, err
		}
		if !roleExists {
			continue // nothing can be granted to a role that does not exist
		}
		for table, privs := range m.Grants {
			classes[table].Grants[m.Role] = privs
		}
		if !moduleRan {
			optional[m.Role] = true
		}
	}
	return classes, optional, nil
}

// checkNoDestructivePrivileges asks, of every subject that could plausibly hold it, whether it can DELETE or TRUNCATE a platform-owned public table by EFFECTIVE privilege (membership
// inheritance included): nobody may, because module foreign keys cascade from both tables and a delete destroys module history (Docs/phase-1-spec.md, F22).
func checkNoDestructivePrivileges(ctx context.Context, q querier, tables []string, rels map[string]relation) ([]string, error) {
	candidates := append([]string{"platform_app", "platform_admin"}, supabaseAPIRoles...)
	for _, m := range moduleRoles {
		candidates = append(candidates, m.Role)
	}
	rows, err := q.Query(ctx, "SELECT rolname::text, rolsuper FROM pg_roles WHERE rolname::text = ANY($1::text[])", candidates)
	if err != nil {
		return nil, err
	}
	subjects := []string{"public"} // PUBLIC itself
	for rows.Next() {
		var name string
		var super bool
		if err := rows.Scan(&name, &super); err != nil {
			rows.Close()
			return nil, err
		}
		if !super { // a superuser can do anything; that is not a grant
			subjects = append(subjects, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(subjects)
	var v []string
	for _, table := range tables {
		if _, ok := rels[table]; !ok {
			continue // a missing table is reported as stale by judgeRelations
		}
		for _, subject := range subjects {
			for _, priv := range []string{"DELETE", "TRUNCATE"} {
				var can bool
				if err := q.QueryRow(ctx, "SELECT has_table_privilege($1::name, $2::text, $3::text)", subject, publicSchema+"."+table, priv).Scan(&can); err != nil {
					return nil, fmt.Errorf("has_table_privilege(%s, %s.%s, %s): %w", subject, publicSchema, table, priv, err)
				}
				if can {
					who := subject
					if who == "public" {
						who = "PUBLIC"
					}
					v = append(v, fmt.Sprintf("%s.%s: %s can %s (nobody may: module foreign keys cascade from this table, so a delete destroys module history)", publicSchema, table, who, priv))
				}
			}
		}
	}
	return v, nil
}

func sortedClassNames(m map[string]privClass) []string {
	return sortedMapKeys(m)
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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

// TestGrantsInvariant: on a freshly migrated database every table and view in schema platform, and the platform's two tables in public, have exactly their declared privileges.
func TestGrantsInvariant(t *testing.T) {
	_, admin := migratedScratch(t)
	v, err := checkGrants(testCtx(t), admin)
	requireNoViolations(t, v, err, "a freshly migrated scratch database")
}

// TestGrantsInvariant_RoleSwitchActuallyBites is the paired control for the grants proof: the declared privileges are not
// merely what the catalog says, they are what a session running as the role can and cannot do. SET LOCAL ROLE is checked
// to have really switched (current_user changed, session_user did not), because a switch that silently does nothing
// would leave every denial below unproven. (The per-table proof of what each role can and cannot do is TestRoles_*.)
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

// grantsAfter runs setup (as the superuser, in a transaction that is rolled back) and returns what the checker says about the result.
func grantsAfter(t *testing.T, admin *pgx.Conn, setup []string) []string {
	t.Helper()
	ctx := testCtx(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, stmt := range setup {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	got, err := checkGrants(ctx, tx)
	if err != nil {
		t.Fatalf("the checker could not run: %v", err)
	}
	return got
}

// ensureRoleSQL creates the role only if the cluster has none. Roles are cluster-wide: the statement runs inside a transaction that is rolled back, so a role it creates never exists
// outside that transaction (nothing to clean up, and nothing another session can see); a role that already exists (the module roles, once the harness stack has migrated the modules)
// is used as it is.
func ensureRoleSQL(role string) string {
	return "DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '" + role + "') THEN CREATE ROLE " + role + " NOLOGIN; END IF; END $$"
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
		// ---- Phase 0 cases (the tracking table) ----
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
		// Phase 0 emptied the schema by dropping its one table. The chain has many tables now, so the schema is emptied for real (dropped and re-created, still owned by platform_owner).
		// An empty schema is reported ONCE, as "the instrument matched nothing": the stale-declaration lines are deliberately not repeated for it (judgeRelations adds them only
		// when some relation exists).
		{"an empty schema",
			[]string{"DROP SCHEMA platform CASCADE", "CREATE SCHEMA platform AUTHORIZATION platform_owner"},
			[][]string{{"found 0 tables"}}},
		{"the tracking table dropped (a stale declaration)",
			[]string{"DROP TABLE platform.schema_migrations"},
			[][]string{{"platform.schema_migrations", "declared privilege class", "does not exist"}}},

		// ---- Phase 1: every table of schema platform ----
		{"UPDATE on the ledger granted to platform_app",
			[]string{"GRANT UPDATE ON platform.ledger TO platform_app"},
			[][]string{{"platform.ledger", "platform_app", "UPDATE", "over-grant"}}},
		{"DELETE on the ledger granted to platform_admin",
			[]string{"GRANT DELETE ON platform.ledger TO platform_admin"},
			[][]string{{"platform.ledger", "platform_admin", "DELETE", "over-grant"}}},
		{"INSERT on balances granted to platform_app",
			[]string{"GRANT INSERT ON platform.balances TO platform_app"},
			[][]string{{"platform.balances", "platform_app", "INSERT", "over-grant"}}},
		{"INSERT on suppressions taken from platform_app",
			[]string{"REVOKE INSERT ON platform.suppressions FROM platform_app"},
			[][]string{{"platform.suppressions", "missing grant", "platform_app", "INSERT"}}},
		{"a grant on the audit log to PUBLIC",
			[]string{"GRANT SELECT ON platform.admin_audit TO PUBLIC"},
			[][]string{{"platform.admin_audit", "PUBLIC", "SELECT", "over-grant"}}},
		{"a table of schema platform owned by the wrong role",
			[]string{"ALTER TABLE platform.ledger OWNER TO platform_app"},
			[][]string{{"platform.ledger", "owned by platform_app", "platform_owner"}}},

		// ---- Phase 1: column-level privileges are declared now ----
		{"a column grant widening unit_costs",
			[]string{"GRANT UPDATE (description) ON platform.unit_costs TO platform_app"},
			[][]string{{"platform.unit_costs", "column privilege", "platform_app", "UPDATE", "description"}}},
		{"a declared column grant taken away",
			[]string{"REVOKE UPDATE (changed_by) ON platform.unit_costs FROM platform_app"},
			[][]string{{"platform.unit_costs", "missing column grant", "platform_app", "UPDATE", "changed_by"}}},
		{"a declared column grant made grantable",
			[]string{"GRANT UPDATE (cost) ON platform.unit_costs TO platform_app WITH GRANT OPTION"},
			[][]string{{"platform.unit_costs", "column privilege", "platform_app", "UPDATE", "cost", "WITH GRANT OPTION"}}},
		{"a column grant to PUBLIC",
			[]string{"GRANT SELECT (description) ON platform.unit_costs TO PUBLIC"},
			[][]string{{"platform.unit_costs", "column privilege", "PUBLIC", "SELECT", "description"}}},
		{"table-level UPDATE on unit_costs given to platform_app on top of its columns",
			[]string{"GRANT UPDATE ON platform.unit_costs TO platform_app"},
			[][]string{{"platform.unit_costs", "platform_app", "UPDATE", "over-grant"}}},
		{"the unit_costs column grant given to the wrong role",
			[]string{"GRANT UPDATE (cost) ON platform.unit_costs TO platform_admin"},
			[][]string{{"platform.unit_costs", "column privilege", "platform_admin", "UPDATE", "cost"}}},

		// ---- Phase 1: views are relations too ----
		{"a grant on a view to PUBLIC",
			[]string{"GRANT SELECT ON platform.balance_drift TO PUBLIC"},
			[][]string{{"platform.balance_drift", "PUBLIC", "SELECT", "over-grant"}}},
		{"a missing grant on a view",
			[]string{"REVOKE SELECT ON platform.reserve_overdrawn FROM platform_admin"},
			[][]string{{"platform.reserve_overdrawn", "missing grant", "platform_admin", "SELECT"}}},
		{"a view nobody declared",
			[]string{"CREATE VIEW platform.zz_view AS SELECT 1 AS one", "ALTER VIEW platform.zz_view OWNER TO platform_owner"},
			[][]string{{"platform.zz_view", "no declared privilege class"}}},
		{"a view owned by the wrong role",
			[]string{"ALTER VIEW platform.balance_drift OWNER TO platform_admin"},
			[][]string{{"platform.balance_drift", "owned by platform_admin", "platform_owner"}}},
		{"INSERT on a view granted to platform_app",
			[]string{"GRANT INSERT ON platform.reserve_overdrawn TO platform_app"},
			[][]string{{"platform.reserve_overdrawn", "platform_app", "INSERT", "over-grant"}}},
		{"a view dropped (a stale declaration)",
			[]string{"DROP VIEW platform.balance_drift"},
			[][]string{{"platform.balance_drift", "declared privilege class", "does not exist"}}},

		// ---- Phase 1: the platform's two tables in schema public ----
		{"DELETE on public.users granted to platform_app",
			[]string{"GRANT DELETE ON public.users TO platform_app"},
			[][]string{{"public.users", "platform_app", "DELETE", "over-grant"}, {"public.users", "platform_app can DELETE"}}},
		{"TRUNCATE on public.job_items granted to PUBLIC",
			[]string{"GRANT TRUNCATE ON public.job_items TO PUBLIC"},
			[][]string{{"public.job_items", "PUBLIC", "TRUNCATE", "over-grant"}, {"public.job_items", "PUBLIC can TRUNCATE"}}},
		{"DELETE on public.users granted to a Supabase API role",
			[]string{ensureRoleSQL("anon"), "GRANT DELETE ON public.users TO anon"},
			[][]string{{"public.users", "anon", "DELETE", "over-grant"}, {"public.users", "anon can DELETE"}}},
		{"SELECT on public.users granted to a Supabase API role",
			[]string{ensureRoleSQL("authenticated"), "GRANT SELECT ON public.users TO authenticated"},
			[][]string{{"public.users", "authenticated", "SELECT", "over-grant"}}},
		{"DELETE on public.users inherited through role membership",
			[]string{"CREATE ROLE zz_grants_inherit NOLOGIN", "GRANT DELETE ON public.users TO zz_grants_inherit", "GRANT zz_grants_inherit TO platform_admin"},
			[][]string{{"public.users", "zz_grants_inherit", "DELETE", "over-grant"}, {"public.users", "platform_admin can DELETE"}}},
		{"platform_admin allowed to write public.users",
			[]string{"GRANT INSERT ON public.users TO platform_admin"},
			[][]string{{"public.users", "platform_admin", "INSERT", "over-grant"}}},
		{"UPDATE on public.users taken from platform_app",
			[]string{"REVOKE UPDATE ON public.users FROM platform_app"},
			[][]string{{"public.users", "missing grant", "platform_app", "UPDATE"}}},
		{"a public table owned by the wrong role",
			[]string{"ALTER TABLE public.users OWNER TO platform_admin"},
			[][]string{{"public.users", "owned by platform_admin", "platform_owner"}}},
		{"a column privilege on a public table",
			[]string{"GRANT UPDATE (email) ON public.users TO platform_admin"},
			[][]string{{"public.users", "column privilege", "platform_admin", "UPDATE", "email"}}},
		{"a public table dropped (a stale declaration)",
			[]string{"DROP TABLE public.job_items"},
			[][]string{{"public.job_items", "declared privilege class", "does not exist"}}},
		{"both public tables dropped (nothing is left to judge, and that is reported)",
			[]string{"DROP TABLE public.job_items", "DROP TABLE public.users CASCADE"},
			[][]string{{"public.users", "declared privilege class", "does not exist"}, {"public.job_items", "declared privilege class", "does not exist"}}},

		// ---- Phase 1: the module roles on the platform's public tables ----
		{"a module role holding a privilege its module never issues itself (module not migrated here)",
			[]string{ensureRoleSQL("send_byo_app"), "GRANT UPDATE ON public.users TO send_byo_app"},
			[][]string{{"public.users", "send_byo_app", "UPDATE", "over-grant"}}},
		{"module 2's role given the INSERT that only module 3 holds",
			[]string{ensureRoleSQL("send_managed_app"), "GRANT INSERT ON public.job_items TO send_managed_app"},
			[][]string{{"public.job_items", "send_managed_app", "INSERT", "over-grant"}}},
		{"a module role with DELETE on job_items",
			[]string{ensureRoleSQL("send_byo_app"), "GRANT DELETE ON public.job_items TO send_byo_app"},
			[][]string{{"public.job_items", "send_byo_app", "DELETE", "over-grant"}, {"public.job_items", "send_byo_app can DELETE"}}},
		{"module 2's schema is here but its role holds nothing",
			[]string{ensureRoleSQL("send_managed_app"), "CREATE SCHEMA send_managed"},
			[][]string{{"public.users", "missing grant", "send_managed_app", "SELECT"}, {"public.job_items", "missing grant", "send_managed_app", "SELECT"}}},
		{"module 3's schema is here but its role lacks the job_items INSERT",
			[]string{ensureRoleSQL("send_byo_app"), "CREATE SCHEMA send_byo", "GRANT SELECT ON public.users, public.job_items TO send_byo_app"},
			[][]string{{"public.job_items", "missing grant", "send_byo_app", "INSERT"}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := grantsAfter(t, admin, c.setup)
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

// TestGrantsInvariant_ScopeControls: what the checker must NOT report. Each case is the control of a case above: the same kind of object, in the state the design allows, is accepted -
// so the violations above are the checker's judgement and not an artefact of something it always says.
//   - a module role that exists in the cluster without its module's schema in THIS database is not judged (a scratch database in the harness stack sees the roles the live database's
//     module migrations created: demanding their grants here would turn every scratch database red);
//   - the module roles holding exactly the sets their own migrations grant are accepted once the module's schema is here;
//   - an undeclared table in schema public is not judged (the allowlist gate owns it), whatever it is granted, while the same table in schema platform IS (the first case above).
func TestGrantsInvariant_ScopeControls(t *testing.T) {
	_, admin := migratedScratch(t)
	cases := []struct {
		name  string
		setup []string
	}{
		{"module roles that exist without their modules are not judged",
			[]string{ensureRoleSQL("send_managed_app"), ensureRoleSQL("send_byo_app")}},
		{"module roles holding exactly their declared sets are accepted",
			[]string{ensureRoleSQL("send_managed_app"), ensureRoleSQL("send_byo_app"), "CREATE SCHEMA send_managed", "CREATE SCHEMA send_byo",
				"GRANT SELECT ON public.users, public.job_items TO send_managed_app",
				"GRANT SELECT ON public.users TO send_byo_app", "GRANT SELECT, INSERT ON public.job_items TO send_byo_app"}},
		{"an undeclared table in schema public is not judged",
			[]string{"CREATE TABLE public.zz_not_ours (id int)", "GRANT ALL ON public.zz_not_ours TO PUBLIC", "GRANT ALL ON SCHEMA public TO platform_app"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := grantsAfter(t, admin, c.setup)
			if len(got) != 0 {
				t.Fatalf("the checker reported %d violation(s) where the design allows the state:\n  %s", len(got), strings.Join(got, "\n  "))
			}
		})
	}

	// The paired control of the last case: the same undeclared table inside schema platform IS reported (and so is the broken state of a module whose schema is here).
	got := grantsAfter(t, admin, []string{"CREATE TABLE platform.zz_not_ours (id int)", "ALTER TABLE platform.zz_not_ours OWNER TO platform_owner"})
	if !hasViolation(got, "platform.zz_not_ours", "no declared privilege class") {
		t.Fatalf("an undeclared table in schema platform was not reported; the checker said: %v", got)
	}
}
