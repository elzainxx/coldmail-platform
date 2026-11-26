//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The modules live. These tests run against the LIVE database of the full harness stack, where the platform's real public.users / public.job_items sit beside the real schemas of modules 2 and 3,
// migrated by the modules' own tools, with the modules' own roles. They prove, as each module's role, what the declaration in db/module-statements.txt only states:
//   (a) the module roles hold exactly the privileges the modules' own tests assert,
//   (b) each module's own pre-flight SQL passes,
//   (c) every declared statement RUNS as that role against the real tables, with real parameters bound as strings (as the modules bind them),
//   (d) the writes the modules' own tests assert are refused, are refused (42501),
//   (e) the loop that runs the statements provably runs them (a deliberately broken statement fails with 42703).
// Everything that writes does so in a transaction that is ALWAYS rolled back; liveAdmin itself is read-only. HARNESS_MODULES_ROOT (the module checkouts, mounted read-only) is required: a test
// that cannot run fails, it never skips.

type shapeModule struct {
	ID, Dir, Role, Schema string
	JobItemPrivs          []string // the exact set the module's migrations grant on public.job_items (public.users is SELECT for both)
}

var shapeModules = []shapeModule{
	{"m2", "coldmail-send-managed", "send_managed_app", "send_managed", []string{"SELECT"}},
	{"m3", "coldmail-send-byo", "send_byo_app", "send_byo", []string{"INSERT", "SELECT"}},
}

func shapeModulesRoot(t *testing.T) string {
	t.Helper()
	root := requireEnv(t, shapeEnvModules)
	for _, m := range shapeModules {
		if st, err := os.Stat(filepath.Join(root, m.Dir)); err != nil || !st.IsDir() {
			t.Fatalf("%s=%q has no module checkout %s: the harness-tests service must mount the module checkouts", shapeEnvModules, root, m.Dir)
		}
	}
	return root
}

// ---- (a) privileges ----

// shapeModuleGrantViolations compares what the catalog says each module role may do to public.users and public.job_items with the exact sets the modules' migrations grant (and their tests assert):
// module 2 SELECT on both; module 3 SELECT on users, SELECT and INSERT on job_items. It also reports a column-level privilege, a grant option and anything PUBLIC holds on the two tables. It
// never calls t.Error: the control below breaks the grants inside a rolled-back transaction and requires the report.
func shapeModuleGrantViolations(ctx context.Context, db shapeDB) ([]string, error) {
	var v []string
	for _, m := range shapeModules {
		var exists bool
		if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", m.Role).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			v = append(v, fmt.Sprintf("role %s does not exist: module %s has not been migrated into this database", m.Role, m.ID))
			continue
		}
		actual := map[string]map[string]bool{} // table -> privilege -> grantable
		rows, err := db.Query(ctx, `
			SELECT c.relname::text, a.privilege_type, a.is_grantable
			FROM pg_class c
			CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname IN ('users', 'job_items')
			  AND a.grantee = (SELECT r.oid FROM pg_roles r WHERE r.rolname = $1)`, m.Role)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tbl, priv string
			var grantable bool
			if err := rows.Scan(&tbl, &priv, &grantable); err != nil {
				rows.Close()
				return nil, err
			}
			if actual[tbl] == nil {
				actual[tbl] = map[string]bool{}
			}
			actual[tbl][priv] = grantable
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		want := map[string][]string{"users": {"SELECT"}, "job_items": m.JobItemPrivs}
		for _, tbl := range []string{"job_items", "users"} {
			wantSet := map[string]bool{}
			for _, p := range want[tbl] {
				wantSet[p] = true
				if _, ok := actual[tbl][p]; !ok {
					v = append(v, fmt.Sprintf("%s on public.%s: missing grant %s (declared: %s)", m.Role, tbl, p, strings.Join(want[tbl], ", ")))
				}
			}
			privs := make([]string, 0, len(actual[tbl]))
			for p := range actual[tbl] {
				privs = append(privs, p)
			}
			sort.Strings(privs)
			for _, p := range privs {
				if !wantSet[p] {
					v = append(v, fmt.Sprintf("%s on public.%s: over-grant %s (declared: %s)", m.Role, tbl, p, strings.Join(want[tbl], ", ")))
				}
				if actual[tbl][p] {
					v = append(v, fmt.Sprintf("%s on public.%s: %s WITH GRANT OPTION", m.Role, tbl, p))
				}
			}
		}
		rows, err = db.Query(ctx, `
			SELECT c.relname::text, att.attname::text, a.privilege_type
			FROM pg_attribute att
			JOIN pg_class c ON c.oid = att.attrelid
			CROSS JOIN LATERAL aclexplode(att.attacl) a
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname IN ('users', 'job_items') AND att.attacl IS NOT NULL
			  AND a.grantee = (SELECT r.oid FROM pg_roles r WHERE r.rolname = $1)
			ORDER BY 1, 2, 3`, m.Role)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tbl, col, priv string
			if err := rows.Scan(&tbl, &col, &priv); err != nil {
				rows.Close()
				return nil, err
			}
			v = append(v, fmt.Sprintf("%s on public.%s: column privilege %s on column %s (none are declared)", m.Role, tbl, priv, col))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	rows, err := db.Query(ctx, `
		SELECT c.relname::text, a.privilege_type
		FROM pg_class c
		CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE c.relnamespace = 'public'::regnamespace AND c.relname IN ('users', 'job_items') AND a.grantee = 0
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tbl, priv string
		if err := rows.Scan(&tbl, &priv); err != nil {
			rows.Close()
			return nil, err
		}
		v = append(v, fmt.Sprintf("PUBLIC holds %s on public.%s (nothing may be granted to PUBLIC)", priv, tbl))
	}
	rows.Close()
	return v, rows.Err()
}

// TestModulesLive_PrivilegeSets: send_managed_app holds SELECT on public.users and on public.job_items and nothing else; send_byo_app holds SELECT on users and SELECT + INSERT on job_items (the sets
// the modules' own migrations grant and their own tests assert exactly). Fails when a platform migration grants a module role more, takes one away, or grants PUBLIC anything. Control: each kind of
// breakage (an over-grant, a missing grant, a column privilege, a grant to PUBLIC, a grant option) made inside a rolled-back transaction on the live database is reported by role, table and privilege.
func TestModulesLive_PrivilegeSets(t *testing.T) {
	shapeModulesRoot(t)
	requireLiveMigrated(t)
	live := liveAdmin(t)
	ctx := testCtx(t)
	v, err := shapeModuleGrantViolations(ctx, live)
	shapeRequireClean(t, v, err, "the live database")

	tx := shapeRWTx(t, live)
	controls := []struct {
		name, sql string
		want      []string
	}{
		{"UPDATE granted to module 2 on job_items", "GRANT UPDATE ON public.job_items TO send_managed_app", []string{"send_managed_app", "public.job_items", "over-grant UPDATE"}},
		{"INSERT granted to module 3 on users", "GRANT INSERT ON public.users TO send_byo_app", []string{"send_byo_app", "public.users", "over-grant INSERT"}},
		{"DELETE granted to module 3 on job_items", "GRANT DELETE ON public.job_items TO send_byo_app", []string{"send_byo_app", "public.job_items", "over-grant DELETE"}},
		{"module 3's INSERT revoked", "REVOKE INSERT ON public.job_items FROM send_byo_app", []string{"send_byo_app", "public.job_items", "missing grant INSERT"}},
		{"module 2's SELECT on users revoked", "REVOKE SELECT ON public.users FROM send_managed_app", []string{"send_managed_app", "public.users", "missing grant SELECT"}},
		{"a column privilege", "GRANT UPDATE (status) ON public.job_items TO send_byo_app", []string{"send_byo_app", "column privilege UPDATE on column status"}},
		{"a grant to PUBLIC", "GRANT SELECT ON public.users TO PUBLIC", []string{"PUBLIC holds SELECT on public.users"}},
		{"a grant option", "GRANT SELECT ON public.job_items TO send_managed_app WITH GRANT OPTION", []string{"send_managed_app", "SELECT WITH GRANT OPTION"}},
	}
	for _, c := range controls {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer sp.Rollback(ctx)
			if _, err := sp.Exec(ctx, c.sql); err != nil {
				t.Fatalf("setup %q: %v", c.sql, err)
			}
			got, err := shapeModuleGrantViolations(ctx, sp)
			shapeRequireReported(t, got, err, c.want...)
		})
	}
	v, err = shapeModuleGrantViolations(ctx, live)
	shapeRequireClean(t, v, err, "the live database after the rolled-back breakages")
}

// ---- (b) the modules' own pre-flight ----

// TestModulesLive_Preflight runs each module's OWN pre-flight SQL (db/preflight/check_platform_tables.sql from its checkout, the file its migrator runs before the chain) against the live
// database: it must pass. It checks existence only (finding F20); the shape is checked by TestShape_*. Control: the same file with the table name changed to one that does not exist FAILS, with
// the module's own message, so running the file really checks something.
func TestModulesLive_Preflight(t *testing.T) {
	root := shapeModulesRoot(t)
	requireLiveMigrated(t)
	live := liveAdmin(t)
	ctx := testCtx(t)
	for _, m := range shapeModules {
		p := filepath.Join(root, m.Dir, "db", "preflight", "check_platform_tables.sql")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("module %s: cannot read its pre-flight %s: %v", m.ID, p, err)
		}
		sql := strings.ReplaceAll(string(b), "\r", "")
		if !strings.Contains(sql, "pg_tables") || !strings.Contains(sql, "RAISE EXCEPTION") {
			t.Fatalf("module %s: %s is not the existence pre-flight this test knows (no pg_tables or RAISE EXCEPTION):\n%s", m.ID, p, sql)
		}
		if _, err := live.Exec(ctx, sql); err != nil {
			t.Fatalf("module %s's own pre-flight FAILED against the live database: %v", m.ID, err)
		}
		for _, tbl := range []string{"users", "job_items"} {
			broken := strings.Replace(sql, "tablename='"+tbl+"'", "tablename='shape_no_such_"+tbl+"'", 1)
			if broken == sql {
				t.Fatalf("module %s: the pre-flight no longer contains tablename='%s': the control cannot break it", m.ID, tbl)
			}
			_, err := live.Exec(ctx, broken)
			if err == nil {
				t.Fatalf("module %s: the pre-flight PASSED with the %s check pointed at a table that does not exist: running the file proves nothing", m.ID, tbl)
			}
			if sqlState(err) != "P0001" || !strings.Contains(err.Error(), "public."+tbl+" does not exist") {
				t.Fatalf("module %s: the broken %s check failed with SQLSTATE %q (%v), want P0001 and the module's own message %q", m.ID, tbl, sqlState(err), err, "public."+tbl+" does not exist")
			}
		}
	}
}

// ---- the statements: which text, which role, which parameters ----

// TestModulesLive_StatementsAreTheModulesOwn re-reads the module source at the harness's module root and requires every declared statement to be in it (whitespace-normalised, comment-only lines
// dropped), so that what the tests below EXECUTE is the text the pinned modules run, even if scripts/check-module-reads.sh was not run. Fails when the checkout's code and db/module-statements.txt
// disagree. Control: a statement with one character changed is NOT found.
func TestModulesLive_StatementsAreTheModulesOwn(t *testing.T) {
	root := shapeModulesRoot(t)
	stm := shapeLoadStatements(t)
	for _, st := range stm.All {
		dir := stm.Dirs[st.Module]
		b, err := os.ReadFile(filepath.Join(root, dir, filepath.FromSlash(st.File)))
		if err != nil {
			t.Fatalf("%s %s: cannot read the module source %s/%s: %v", st.Module, st.Label, dir, st.File, err)
		}
		var kept []string
		for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				kept = append(kept, line)
			}
		}
		src := shapeNormSQL(strings.Join(kept, "\n"))
		if !strings.Contains(src, st.SQL) {
			t.Fatalf("%s %s: the declared text is not in %s/%s any more:\n  %s", st.Module, st.Label, dir, st.File, st.SQL)
		}
		mid := len(st.SQL) / 2
		if mangled := st.SQL[:mid] + "~" + st.SQL[mid:]; strings.Contains(src, mangled) {
			t.Fatalf("%s %s: a statement with one extra character was found in the source: the comparison does not discriminate", st.Module, st.Label)
		}
	}
}

// ---- (c) execute every statement as the module's role ----

type shapeLiveFixture struct {
	UserID, LoneUserID, RandomID, ItemID, CampaignID, AuthOnlyID string
}

// shapeLiveSeed inserts, as the superuser and inside the caller's rolled-back transaction: auth.users rows, two public.users rows (auth_user_id = id) and one public.job_items row of the first user.
func shapeLiveSeed(ctx context.Context, tx shapeDB) (shapeLiveFixture, error) {
	var f shapeLiveFixture
	if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text").
		Scan(&f.UserID, &f.LoneUserID, &f.RandomID, &f.ItemID, &f.CampaignID, &f.AuthOnlyID); err != nil {
		return f, err
	}
	for _, id := range []string{f.UserID, f.LoneUserID, f.AuthOnlyID} {
		if _, err := tx.Exec(ctx, "INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, "shape-"+id+"@example.test"); err != nil {
			return f, fmt.Errorf("seed auth.users: %w", err)
		}
	}
	for _, id := range []string{f.UserID, f.LoneUserID} {
		if _, err := tx.Exec(ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", id, "shape-"+id+"@example.test"); err != nil {
			return f, fmt.Errorf("seed public.users: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO public.job_items (id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $2, $3, 'recipient@example.test', 'subject', 'body')",
		f.ItemID, f.CampaignID, f.UserID); err != nil {
		return f, fmt.Errorf("seed public.job_items: %w", err)
	}
	return f, nil
}

// shapeSwitchRole does SET LOCAL ROLE and PROVES it bit: current_user is now the role and session_user did not change. A switch that silently did nothing would leave every denial and every
// success below meaning nothing.
func shapeSwitchRole(ctx context.Context, tx shapeDB, role string) error {
	var session0, current0 string
	if err := tx.QueryRow(ctx, "SELECT session_user::text, current_user::text").Scan(&session0, &current0); err != nil {
		return err
	}
	if session0 != current0 {
		return fmt.Errorf("precondition: the connection is already running as %s (session user %s)", current0, session0)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("SET LOCAL ROLE %s: %w", role, err)
	}
	var session1, current1 string
	if err := tx.QueryRow(ctx, "SELECT session_user::text, current_user::text").Scan(&session1, &current1); err != nil {
		return err
	}
	if current1 != role || session1 != session0 {
		return fmt.Errorf("the role switch did not bite: current_user=%s session_user=%s, want current_user=%s session_user=%s", current1, session1, role, session0)
	}
	return nil
}

type shapeExec func(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error

// shapeExecUsers: U* is `SELECT EXISTS(SELECT 1 FROM public.users WHERE id=$1)`: true for the seeded user and for the all-zero system user, false for a random uuid.
func shapeExecUsers(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error {
	for _, c := range []struct {
		what, id string
		want     bool
	}{
		{"the seeded user", f.UserID, true},
		{"a random uuid", f.RandomID, false},
		{"the all-zero system user", shapeSystemUserID, true},
	} {
		var got bool
		if err := sp.QueryRow(ctx, s.SQL, c.id).Scan(&got); err != nil {
			return fmt.Errorf("%s for %s: %w", s.Label, c.what, err)
		}
		if got != c.want {
			return fmt.Errorf("%s for %s: EXISTS = %v, want %v", s.Label, c.what, got, c.want)
		}
	}
	return nil
}

// shapeExecRead: J1 returns the campaign_id of the item, scoped to the calling tenant (another user's id finds nothing).
func shapeExecRead(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error {
	var campaign string
	var marked bool // since the Phase 4 close-out the read also says whether the platform marked the item cancelled (the run route then refuses it)
	if err := sp.QueryRow(ctx, s.SQL, f.ItemID, f.UserID).Scan(&campaign, &marked); err != nil {
		return fmt.Errorf("%s for the owner: %w", s.Label, err)
	}
	if campaign != f.CampaignID {
		return fmt.Errorf("%s returned campaign_id %s, want %s", s.Label, campaign, f.CampaignID)
	}
	if marked {
		return fmt.Errorf("%s says the freshly seeded item is marked cancelled: the seed is pending", s.Label)
	}
	if err := sp.QueryRow(ctx, s.SQL, f.ItemID, f.RandomID).Scan(&campaign, &marked); !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s for another tenant: %v, want no rows (tenant scoping)", s.Label, err)
	}
	return nil
}

// shapeExecFreeze: J2 (INSERT ... SELECT into the module's jobs table) returns the new job id; the module's genesis job_events row exists; another tenant's id inserts nothing; and a second
// freeze of the same item is refused by the module's own unique constraint (23505 jobs_job_item_id_unique), so the first one really inserted.
func shapeExecFreeze(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error {
	var jobID string
	if err := sp.QueryRow(ctx, s.SQL, f.ItemID, f.UserID).Scan(&jobID); err != nil {
		return fmt.Errorf("%s: %w", s.Label, err)
	}
	if len(jobID) != 36 {
		return fmt.Errorf("%s: RETURNING id gave %q, want a uuid", s.Label, jobID)
	}
	var genesis int
	if err := sp.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s.job_events WHERE job_id = $1 AND from_state IS NULL AND to_state = 'queued'", m.Schema), jobID).Scan(&genesis); err != nil {
		return fmt.Errorf("%s: read the genesis event: %w", s.Label, err)
	}
	if genesis != 1 {
		return fmt.Errorf("%s: %d genesis job_events row(s) for job %s, want exactly 1 (the trigger must have fired)", s.Label, genesis, jobID)
	}
	other, err := sp.Begin(ctx)
	if err != nil {
		return err
	}
	var none string
	err = other.QueryRow(ctx, s.SQL, f.ItemID, f.RandomID).Scan(&none)
	_ = other.Rollback(ctx)
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s for another tenant: %v, want no rows (tenant scoping)", s.Label, err)
	}
	again, err := sp.Begin(ctx)
	if err != nil {
		return err
	}
	err = again.QueryRow(ctx, s.SQL, f.ItemID, f.UserID).Scan(&none)
	_ = again.Rollback(ctx)
	if err == nil || sqlState(err) != "23505" || shapeConstraint(err) != "jobs_job_item_id_unique" {
		return fmt.Errorf("%s a second time: SQLSTATE %q constraint %q (%v), want 23505 jobs_job_item_id_unique", s.Label, sqlState(err), shapeConstraint(err), err)
	}
	return nil
}

// shapeExecCampaign: J3 upserts the campaign row (module 3): one row the first time, none the second (ON CONFLICT DO NOTHING).
func shapeExecCampaign(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error {
	for i, want := range []int64{1, 0} {
		tag, err := sp.Exec(ctx, s.SQL, f.CampaignID, f.UserID, 3600)
		if err != nil {
			return fmt.Errorf("%s (run %d): %w", s.Label, i+1, err)
		}
		if tag.RowsAffected() != want {
			return fmt.Errorf("%s (run %d): %d row(s) affected, want %d", s.Label, i+1, tag.RowsAffected(), want)
		}
	}
	return nil
}

// shapeExecTestSend: J4 is module 3's own INSERT into public.job_items; it returns an id, and the row has campaign_id = user_id and status 'pending'.
func shapeExecTestSend(ctx context.Context, sp pgx.Tx, m shapeModule, s shapeStmt, f shapeLiveFixture) error {
	var id string
	if err := sp.QueryRow(ctx, s.SQL, f.UserID, "recipient@example.test", "Test message from your account", "<p>This is a test message.</p>").Scan(&id); err != nil {
		return fmt.Errorf("%s: %w", s.Label, err)
	}
	if len(id) != 36 {
		return fmt.Errorf("%s: RETURNING id gave %q, want a uuid", s.Label, id)
	}
	var campaign, user, status string
	if err := sp.QueryRow(ctx, "SELECT campaign_id::text, user_id::text, status FROM public.job_items WHERE id = $1", id).Scan(&campaign, &user, &status); err != nil {
		return fmt.Errorf("%s: read the inserted row back as %s: %w", s.Label, m.Role, err)
	}
	if campaign != f.UserID || user != f.UserID || status != "pending" {
		return fmt.Errorf("%s: the row has campaign_id=%s user_id=%s status=%s, want %s %s pending", s.Label, campaign, user, status, f.UserID, f.UserID)
	}
	return nil
}

// shapeExecutors is the table of what to do with each declared statement. A declared statement without an entry FAILS (below), and an entry without a declared statement fails
// TestModulesLive_ExecutorsMatchDeclaration: a statement that is listed but never run proves nothing.
func shapeExecutors(m shapeModule) map[string]shapeExec {
	ex := map[string]shapeExec{"U1": shapeExecUsers, "U2": shapeExecUsers, "J1": shapeExecRead, "J2": shapeExecFreeze}
	if m.ID == "m3" {
		ex["J3"] = shapeExecCampaign
		ex["J4"] = shapeExecTestSend
	}
	return ex
}

// shapeRunStatements switches to the module's role and runs every statement in its own savepoint (rolled back afterwards), returning each statement's outcome by label: nil means it ran and every
// assertion of its executor held. The error is for the machinery itself failing.
func shapeRunStatements(ctx context.Context, tx pgx.Tx, m shapeModule, stmts []shapeStmt, f shapeLiveFixture) (map[string]error, error) {
	if err := shapeSwitchRole(ctx, tx, m.Role); err != nil {
		return nil, err
	}
	ex := shapeExecutors(m)
	out := map[string]error{}
	for _, st := range stmts {
		run, ok := ex[st.Label]
		if !ok {
			out[st.Label] = fmt.Errorf("no executor for the declared statement %s: add one to modules_live_test.go", st.Label)
			continue
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		out[st.Label] = run(ctx, sp, m, st, f)
		_ = sp.Rollback(ctx)
	}
	return out, nil
}

func shapeLiveBegin(t *testing.T) (context.Context, pgx.Tx, shapeLiveFixture) {
	t.Helper()
	requireLiveMigrated(t)
	ctx := testCtx(t)
	tx := shapeRWTx(t, liveAdmin(t))
	f, err := shapeLiveSeed(ctx, tx)
	if err != nil {
		t.Fatalf("cannot seed the live database (the harness must have run platform_0001 and have auth.users): %v", err)
	}
	return ctx, tx, f
}

// TestModulesLive_StatementsExecuteAsModuleRole EXECUTES every statement of db/module-statements.txt as the module's own database role against the real tables, in a transaction that is rolled back:
// seeded (as superuser) with an auth.users row, a public.users row linked to it and a public.job_items row; SET LOCAL ROLE with a proof that it bit; each statement run with real parameters
// bound as strings as the modules bind them. U* answers true for the seeded user and false for a random uuid; J1 returns the campaign; J2 returns a job id and the module's genesis job_events row
// exists; J3 upserts the campaign; J4 (module 3) returns an id. Fails when a statement errors, returns the wrong thing, or has no executor. The paired control is
// TestModulesLive_BrokenStatementFails, which runs this same machinery on statements broken on purpose.
func TestModulesLive_StatementsExecuteAsModuleRole(t *testing.T) {
	shapeModulesRoot(t)
	stm := shapeLoadStatements(t)
	for _, m := range shapeModules {
		m := m
		t.Run(m.ID, func(t *testing.T) {
			sqls := stm.SQL(m.ID)
			if len(sqls) < 1 {
				t.Fatalf("%s declares no statement for module %s: the instrument matched nothing", shapeStatementsRel, m.ID)
			}
			ctx, tx, f := shapeLiveBegin(t)
			res, err := shapeRunStatements(ctx, tx, m, sqls, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != len(sqls) {
				t.Fatalf("%d outcome(s) for %d declared statement(s): the loop did not run them all", len(res), len(sqls))
			}
			for _, st := range sqls {
				if res[st.Label] != nil {
					t.Errorf("module %s statement %s as %s: %v\n  %s", m.ID, st.Label, m.Role, res[st.Label], st.SQL)
				}
			}
		})
	}
}

// shapeBreakers say how to break each declared statement into one that names a column that does not exist.
var shapeBreakers = map[string][2]string{
	"U1": {"WHERE id=$1", "WHERE no_such_column=$1"},
	"U2": {"WHERE id=$1", "WHERE no_such_column=$1"},
	"J1": {"SELECT campaign_id", "SELECT no_such_column"},
	"J2": {"ji.campaign_id,", "ji.no_such_column,"},
	"J3": {"user_id, expires_at", "no_such_column, expires_at"},
	"J4": {"rendered_body, status)", "rendered_body, no_such_column)"},
}

// TestModulesLive_BrokenStatementFails is the paired control of the execution loop: the SAME machinery, given each declared statement with a column that does not exist, fails every one of them with
// SQLSTATE 42703 (undefined column), while the unbroken statements pass in the same transaction. So a green TestModulesLive_StatementsExecuteAsModuleRole means the statements ran, not that the
// loop did nothing. Fails when the machinery reports success for a broken statement.
func TestModulesLive_BrokenStatementFails(t *testing.T) {
	shapeModulesRoot(t)
	stm := shapeLoadStatements(t)
	for _, m := range shapeModules {
		m := m
		t.Run(m.ID, func(t *testing.T) {
			sqls := stm.SQL(m.ID)
			broken := make([]shapeStmt, 0, len(sqls))
			for _, st := range sqls {
				b, ok := shapeBreakers[st.Label]
				if !ok {
					t.Fatalf("statement %s has no breaker in shapeBreakers: add one", st.Label)
				}
				bst := st
				bst.SQL = strings.Replace(st.SQL, b[0], b[1], 1)
				if bst.SQL == st.SQL {
					t.Fatalf("statement %s no longer contains %q: the control cannot break it", st.Label, b[0])
				}
				broken = append(broken, bst)
			}
			ctx, tx, f := shapeLiveBegin(t)
			res, err := shapeRunStatements(ctx, tx, m, broken, f)
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range broken {
				if res[st.Label] == nil {
					t.Errorf("module %s: the broken statement %s was reported as passing: the loop does not execute statements\n  %s", m.ID, st.Label, st.SQL)
				} else if sqlState(res[st.Label]) != "42703" {
					t.Errorf("module %s: the broken statement %s failed with SQLSTATE %q (%v), want 42703 undefined_column", m.ID, st.Label, sqlState(res[st.Label]), res[st.Label])
				}
			}
			// the unbroken statements still pass in this same kind of transaction
			ctx2, tx2, f2 := shapeLiveBegin(t)
			res, err = shapeRunStatements(ctx2, tx2, m, sqls, f2)
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range sqls {
				if res[st.Label] != nil {
					t.Errorf("module %s: the unbroken statement %s failed (%v): the 42703 above proves nothing", m.ID, st.Label, res[st.Label])
				}
			}
		})
	}
}

// TestModulesLive_ExecutorsMatchDeclaration: every declared SQL statement has an executor and a breaker, and every executor has a declared statement, so the three lists cannot drift apart.
func TestModulesLive_ExecutorsMatchDeclaration(t *testing.T) {
	shapeModulesRoot(t)
	stm := shapeLoadStatements(t)
	for _, m := range shapeModules {
		declared := map[string]bool{}
		for _, st := range stm.SQL(m.ID) {
			declared[st.Label] = true
			if _, ok := shapeExecutors(m)[st.Label]; !ok {
				t.Errorf("module %s: statement %s is declared but has no executor", m.ID, st.Label)
			}
			if _, ok := shapeBreakers[st.Label]; !ok {
				t.Errorf("module %s: statement %s is declared but has no breaker", m.ID, st.Label)
			}
		}
		for label := range shapeExecutors(m) {
			if !declared[label] {
				t.Errorf("module %s: an executor exists for %s, which db/module-statements.txt does not declare", m.ID, label)
			}
		}
		if len(declared) < 3 {
			t.Errorf("module %s declares only %d SQL statement(s), want at least 3 (U, J1, J2)", m.ID, len(declared))
		}
	}
}

// ---- (d) the writes the modules' own tests assert are refused ----

type shapeWrite struct {
	what, table, sql string
	args             []any
	truncate         bool
}

// TestModulesLive_DeniedWrites: as send_managed_app, UPDATE and INSERT on public.users, and INSERT, UPDATE and DELETE on public.job_items, are refused with 42501 (permission denied for table ...);
// as send_byo_app, UPDATE and INSERT on public.users are refused, its INSERT on public.job_items SUCCEEDS, and then UPDATE and DELETE on job_items are refused AFTER that insert (on the row it just
// wrote and on another). DELETE and TRUNCATE on both tables are refused for both roles. Controls: each non-TRUNCATE statement succeeds as the superuser first, so a denial is the role's doing, not a
// typo or a missing row; the role switch is proven to bite; and module 3's allowed INSERT is the positive control for the table-level grants.
func TestModulesLive_DeniedWrites(t *testing.T) {
	shapeModulesRoot(t)
	for _, m := range shapeModules {
		m := m
		t.Run(m.ID, func(t *testing.T) {
			ctx, tx, f := shapeLiveBegin(t)
			writes := func(itemID string) []shapeWrite {
				return []shapeWrite{
					{"INSERT", "users", "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", []any{f.AuthOnlyID, "shape-denied@example.test"}, false},
					{"UPDATE", "users", "UPDATE public.users SET is_active = false WHERE id = $1", []any{f.LoneUserID}, false},
					{"DELETE", "users", "DELETE FROM public.users WHERE id = $1", []any{f.LoneUserID}, false},
					{"TRUNCATE", "users", "TRUNCATE public.users", nil, true},
					{"UPDATE", "job_items", "UPDATE public.job_items SET status = 'compromised' WHERE id = $1", []any{itemID}, false},
					{"DELETE", "job_items", "DELETE FROM public.job_items WHERE id = $1", []any{itemID}, false},
					{"TRUNCATE", "job_items", "TRUNCATE public.job_items", nil, true},
				}
			}
			insertItem := shapeWrite{"INSERT", "job_items",
				"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1, $1, 'denied@example.test', 's', 'b', 'pending') RETURNING id",
				[]any{f.UserID}, false}

			// control: as the superuser every statement that is not a TRUNCATE succeeds and touches its row
			for _, w := range append(writes(f.ItemID), insertItem) {
				if w.truncate {
					continue
				}
				sp, err := tx.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				tag, err := sp.Exec(ctx, w.sql, w.args...)
				_ = sp.Rollback(ctx)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("control: %s on public.%s as the superuser: rows=%d err=%v: the denial below would prove nothing", w.what, w.table, tag.RowsAffected(), err)
				}
			}

			if err := shapeSwitchRole(ctx, tx, m.Role); err != nil {
				t.Fatal(err)
			}
			refuse := func(w shapeWrite) {
				t.Helper()
				sp, err := tx.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, err = sp.Exec(ctx, w.sql, w.args...)
				_ = sp.Rollback(ctx)
				if err == nil {
					t.Errorf("%s as %s: %s on public.%s was ALLOWED", m.ID, m.Role, w.what, w.table)
					return
				}
				if sqlState(err) != "42501" || !strings.Contains(err.Error(), "permission denied for table "+w.table) {
					t.Errorf("%s as %s: %s on public.%s failed with SQLSTATE %q (%v), want 42501 permission denied for table %s", m.ID, m.Role, w.what, w.table, sqlState(err), err, w.table)
				}
			}

			itemID := f.ItemID
			if shapeContains(m.JobItemPrivs, "INSERT") {
				// module 3: its INSERT on job_items must SUCCEED; the denials that follow are "AFTER its own INSERT"
				var own string
				if err := tx.QueryRow(ctx, insertItem.sql, insertItem.args...).Scan(&own); err != nil {
					t.Fatalf("%s as %s: its own INSERT on public.job_items was refused: %v", m.ID, m.Role, err)
				}
				itemID = own
			} else {
				refuse(insertItem)
			}
			for _, w := range writes(itemID) {
				refuse(w)
			}
			if itemID != f.ItemID { // and on the row the module did not write
				for _, w := range writes(f.ItemID) {
					if w.table == "job_items" && !w.truncate {
						refuse(w)
					}
				}
			}
		})
	}
}

// TestModulesLive_RoleSwitchBites proves the instrument behind (c) and (d): for each module role, SET LOCAL ROLE changes current_user and not session_user, the role cannot see the platform schema
// (permission denied for schema platform, 42501) although the superuser can, and after RESET ROLE the same query works again. Fails when the switch does nothing.
func TestModulesLive_RoleSwitchBites(t *testing.T) {
	shapeModulesRoot(t)
	for _, m := range shapeModules {
		m := m
		t.Run(m.ID, func(t *testing.T) {
			requireLiveMigrated(t)
			ctx := testCtx(t)
			tx := shapeRWTx(t, liveAdmin(t))
			const q = "SELECT count(*) FROM platform.schema_migrations"
			var n int
			if err := tx.QueryRow(ctx, q).Scan(&n); err != nil || n < 1 {
				t.Fatalf("control: the superuser cannot read platform.schema_migrations (n=%d, err=%v)", n, err)
			}
			if err := shapeSwitchRole(ctx, tx, m.Role); err != nil {
				t.Fatal(err)
			}
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// a DIFFERENT text from the control above: pgx caches the prepared statement by its text, and a statement already parsed as the superuser would skip the schema-USAGE check at parse time
			// (only the table privilege is re-checked at execution), so the message would name the table instead of the schema
			err = sp.QueryRow(ctx, q+" AS role_probe").Scan(&n)
			_ = sp.Rollback(ctx)
			if err == nil || sqlState(err) != "42501" || !strings.Contains(err.Error(), "permission denied for schema platform") {
				t.Fatalf("as %s: reading platform.schema_migrations: SQLSTATE %q (%v), want 42501 permission denied for schema platform", m.Role, sqlState(err), err)
			}
			if _, err := tx.Exec(ctx, "RESET ROLE"); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(ctx, q).Scan(&n); err != nil || n < 1 {
				t.Fatalf("after RESET ROLE the superuser cannot read platform.schema_migrations again (n=%d, err=%v)", n, err)
			}
		})
	}
}

// TestModulesLive_TheRunStatementsRefuseAMarkedItem (Phase 4 close-out D9): the platform marks a job's never-submitted items `cancelled` when the job ends before submission, and modules 2 and 3 must not turn such an item into work. The two statements of the run route (J1, the read; J2, the INSERT ... SELECT that freezes the item into the module's own jobs table) are executed as each module's role against the real tables: with the item PENDING the read says not marked and the freeze inserts a job (the control); with the item marked by the platform (superuser, as internal/jobs does) the read says marked and the freeze inserts NOTHING (no row: the status test is in the same statement as the insert).
func TestModulesLive_TheRunStatementsRefuseAMarkedItem(t *testing.T) {
	shapeModulesRoot(t)
	stm := shapeLoadStatements(t)
	for _, m := range shapeModules {
		m := m
		t.Run(m.ID, func(t *testing.T) {
			ctx, tx, f := shapeLiveBegin(t)
			var j1, j2 string
			for _, st := range stm.SQL(m.ID) {
				switch st.Label {
				case "J1":
					j1 = st.SQL
				case "J2":
					j2 = st.SQL
				}
			}
			if j1 == "" || j2 == "" {
				t.Fatalf("%s declares no J1 or no J2 for module %s: the instrument matched nothing", shapeStatementsRel, m.ID)
			}
			freeze := func(label string) (string, error) {
				sp, err := tx.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = sp.Rollback(ctx) }()
				var id string
				err = sp.QueryRow(ctx, j2, f.ItemID, f.UserID).Scan(&id)
				return id, err
			}
			read := func() (campaign string, marked bool, err error) {
				sp, e := tx.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer func() { _ = sp.Rollback(ctx) }()
				err = sp.QueryRow(ctx, j1, f.ItemID, f.UserID).Scan(&campaign, &marked)
				return
			}
			// the CONTROL: the item is pending, as the platform seeds it
			if err := shapeSwitchRole(ctx, tx, m.Role); err != nil {
				t.Fatal(err)
			}
			if c, marked, err := read(); err != nil || marked || c != f.CampaignID {
				t.Fatalf("control: module %s J1 on a pending item answered campaign %q marked %v (error %v), want %s and false", m.ID, c, marked, err, f.CampaignID)
			}
			if id, err := freeze("J2"); err != nil || id == "" {
				t.Fatalf("control: module %s J2 on a pending item inserted nothing (id %q, error %v): the control job must be created", m.ID, id, err)
			}
			// the platform marks the item (superuser: the module roles cannot update the table)
			if _, err := tx.Exec(ctx, "RESET ROLE"); err != nil {
				t.Fatal(err)
			}
			if tag, err := tx.Exec(ctx, "UPDATE public.job_items SET status = 'cancelled' WHERE id = $1", f.ItemID); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("mark the seeded item cancelled: %v (rows %d)", err, tag.RowsAffected())
			}
			if err := shapeSwitchRole(ctx, tx, m.Role); err != nil {
				t.Fatal(err)
			}
			if c, marked, err := read(); err != nil || !marked || c != f.CampaignID {
				t.Fatalf("module %s J1 on a marked item answered campaign %q marked %v (error %v), want %s and true", m.ID, c, marked, err, f.CampaignID)
			}
			if id, err := freeze("J2"); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("module %s J2 on a MARKED item answered id %q error %v, want no row: a module started a job for an item the platform marked cancelled", m.ID, id, err)
			}
		})
	}
}
