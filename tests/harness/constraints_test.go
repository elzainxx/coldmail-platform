//go:build harness

package harness

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The constraint tests: for every table of platform_0001..0004 the rules the database itself enforces. The shape of each test
// is the same: the legal row is the control, every refused row differs from it in one thing, and the refusal is accepted only
// for the SQLSTATE AND the constraint, column or message it is meant to fire (ledgerExpectRefused), so a row refused by some
// other rule fails the test. Each runs on a scratch database the real migrator migrated, inside a rolled-back transaction.

// ---------------------------------------------------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_Users covers public.users: the auth link (including the NULL trap: a CHECK that evaluates to NULL passes, so
// a row with NO auth link must be refused by users_auth_link and not slip through), the one system user, and the email rule.
// It fails if users_auth_link loses its COALESCE (the NULL-link row would then be accepted), if the system user row is not
// seeded, or if a role can delete a user (module foreign keys cascade from it).
func TestConstraints_Users(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	const n1, n2 = "ffffffff-0000-0000-0000-000000000001", "ffffffff-0000-0000-0000-000000000002"
	ledgerMust(t, tx, "INSERT INTO auth.users (id, email) VALUES ($1, 'n1@example.test'), ($2, 'n2@example.test')", n1, n2)

	t.Run("the system user row module 2 requires exists, has no auth link, and can never act", func(t *testing.T) {
		got := ledgerString(t, tx, "SELECT id::text || '|' || COALESCE(auth_user_id::text, '') || '|' || email || '|' || is_active::text FROM public.users WHERE id = $1", ledgerSystemUser)
		if want := ledgerSystemUser + "||system@platform.invalid|false"; got != want {
			t.Fatalf("the system user row is %q, want %q", got, want)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM public.users"); n != 1 {
			t.Fatalf("a freshly migrated database holds %d users, want exactly the system user", n)
		}
	})

	t.Run("users_auth_link: every row IS its auth user, except the one system row", func(t *testing.T) {
		const link = "users_auth_link"
		ledgerExpectRefused(t, tx, "a user with NO auth link (the NULL trap)", "23514", ledgerWhy{Constraint: link},
			"INSERT INTO public.users (id, email) VALUES (gen_random_uuid(), 'nolink@example.test')")
		ledgerExpectRefused(t, tx, "a user whose auth_user_id is another user's id", "23514", ledgerWhy{Constraint: link},
			"INSERT INTO public.users (id, auth_user_id, email) VALUES (gen_random_uuid(), $1, 'other@example.test')", n1)
		ledgerExpectRefused(t, tx, "the all-zero id WITH an auth link (only the system row may be zero, and only without a link)", "23514", ledgerWhy{Constraint: link},
			"INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $2, 'zero@example.test')", ledgerSystemUser, n1)
		ledgerExpectRefused(t, tx, "a second system user (the primary key says one)", "23505", ledgerWhy{Constraint: "users_pkey"},
			"INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, NULL, 'system2@platform.invalid')", ledgerSystemUser)
		ledgerExpectRefused(t, tx, "the link is a real foreign key: the auth user must exist", "23503", ledgerWhy{Constraint: "users_auth_user_id_fkey"},
			"INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'ghost@example.test')", ledgerNoSuchID)
		tag := ledgerExpectOK(t, tx, "control: a user whose id is its auth user", "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'n1@example.test')", n1)
		if tag.RowsAffected() != 1 {
			t.Fatalf("the control inserted %d rows", tag.RowsAffected())
		}
		if got := ledgerString(t, tx, "SELECT is_active::text || '|' || (created_at IS NOT NULL)::text FROM public.users WHERE id = $1", n1); got != "true|true" {
			t.Fatalf("a new user's defaults (is_active|created_at set) = %q, want true|true", got)
		}
		ledgerExpectRefused(t, tx, "deleting an auth user that a platform user points at (ON DELETE RESTRICT)", "23503", ledgerWhy{Constraint: "users_auth_user_id_fkey"},
			"DELETE FROM auth.users WHERE id = $1", n1)
	})

	t.Run("users_email_normalised and the unique email", func(t *testing.T) {
		const rule = "users_email_normalised"
		ins := "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)"
		for _, c := range []struct{ name, email string }{
			{"an empty email", ""}, {"a leading space", " n2@example.test"}, {"a trailing space", "n2@example.test "}, {"upper case", "N2@example.test"},
		} {
			ledgerExpectRefused(t, tx, c.name, "23514", ledgerWhy{Constraint: rule}, ins, n2, c.email)
		}
		ledgerExpectRefused(t, tx, "the same email as another user", "23505", ledgerWhy{Constraint: "users_email_key"}, ins, n2, "n1@example.test")
		ledgerExpectOK(t, tx, "control: a normalised email", ins, n2, "n2@example.test")
	})

	t.Run("who may touch the table: the app inserts and updates, nobody deletes, the break-glass role reads", func(t *testing.T) {
		const n3 = "ffffffff-0000-0000-0000-000000000003"
		ledgerMust(t, tx, "INSERT INTO auth.users (id, email) VALUES ($1, 'n3@example.test')", n3)
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "control: platform_app INSERT", "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'n3@example.test')", n3)
		ledgerExpectOK(t, tx, "control: platform_app UPDATE", "UPDATE public.users SET is_active = false WHERE id = $1", n3)
		ledgerExpectRefused(t, tx, "platform_app DELETE on users", "42501", ledgerWhy{Message: "permission denied for table users"}, "DELETE FROM public.users WHERE id = $1", n3)
		ledgerExpectRefused(t, tx, "platform_app DELETE on job_items", "42501", ledgerWhy{Message: "permission denied for table job_items"}, "DELETE FROM public.job_items")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM public.users"); n < 2 {
			t.Fatalf("platform_admin sees %d users, want the rows inserted above", n)
		}
		ledgerExpectRefused(t, tx, "platform_admin INSERT on users", "42501", ledgerWhy{Message: "permission denied for table users"},
			"INSERT INTO public.users (id, auth_user_id, email) VALUES (gen_random_uuid(), NULL, 'x@example.test')")
		ledgerExpectRefused(t, tx, "platform_admin UPDATE on users", "42501", ledgerWhy{Message: "permission denied for table users"}, "UPDATE public.users SET is_active = true")
		ledgerExpectRefused(t, tx, "platform_admin DELETE on users", "42501", ledgerWhy{Message: "permission denied for table users"}, "DELETE FROM public.users")
	})
}

// TestConstraints_AppSessionTimezoneIsUTC proves platform_0001's `ALTER ROLE platform_app IN DATABASE <this database> SET timezone = 'UTC'` (database-scoped: roles are cluster-wide) where it
// matters: on a REAL LOGIN as platform_app (a SET ROLE does not apply a role's settings, so it would prove nothing). The
// control is that the session setting is live (SET changes what SHOW reports) and that RESET returns to the role's setting.
// It fails if the role setting is missing, or is not UTC.
func TestConstraints_AppSessionTimezoneIsUTC(t *testing.T) {
	s, admin := migratedScratch(t)
	var settings []string
	if err := admin.QueryRow(testCtx(t), "SELECT COALESCE((SELECT setconfig FROM pg_db_role_setting WHERE setrole = 'platform_app'::regrole AND setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())), ARRAY[]::text[])").Scan(&settings); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kv := range settings {
		found = found || strings.EqualFold(kv, "timezone=UTC")
	}
	if !found {
		t.Fatalf("pg_db_role_setting holds %v for platform_app, want it to contain timezone=UTC", settings)
	}

	app := dial(t, s.DSN, appDial(requireEnv(t, envAppPassword)))
	show := func() string { return ledgerString(t, app, "SELECT current_setting('TimeZone')") }
	if got := ledgerString(t, app, "SELECT session_user"); got != "platform_app" {
		t.Fatalf("session_user = %q, want a real login as platform_app", got)
	}
	if got := show(); got != "UTC" {
		t.Fatalf("a platform_app session's TimeZone is %q, want UTC", got)
	}
	ledgerMust(t, app, "SET timezone = 'America/New_York'")
	if got := show(); got != "America/New_York" {
		t.Fatalf("control: after SET timezone the session reports %q: the instrument cannot see a change", got)
	}
	ledgerMust(t, app, "RESET timezone")
	if got := show(); got != "UTC" {
		t.Fatalf("after RESET the session's TimeZone is %q, want the role's setting UTC", got)
	}
	t.Logf("the superuser session of the same database reports TimeZone %q (the database default), the app session UTC", ledgerString(t, admin, "SELECT current_setting('TimeZone')"))
}

// ---------------------------------------------------------------------------------------------------------------------
// admins, modules
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_Admins covers platform.admins: an admin is a user, never the system user, with a name; the app only reads; and (platform_0004, OD-10) a DISABLED admin can no longer
// be named in the ledger, the audit log, M3 entitlements or the unit costs - the last subtest, which replaced the Phase 1 one that pinned the opposite.
// It fails if the system user can become an admin (it could then be named in the ledger), or the app can create admins, or a disabled admin can still act.
func TestConstraints_Admins(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	c := ledgerUserC

	ledgerExpectRefused(t, tx, "the system user as an admin", "23514", ledgerWhy{Constraint: "admins_not_the_system_user"},
		"INSERT INTO platform.admins (id, display_name) VALUES ($1, 'System')", ledgerSystemUser)
	ledgerExpectRefused(t, tx, "a blank display name", "23514", ledgerWhy{Constraint: "admins_display_name_not_blank"},
		"INSERT INTO platform.admins (id, display_name) VALUES ($1, '   ')", c)
	ledgerExpectRefused(t, tx, "an admin who is not a user", "23503", ledgerWhy{Constraint: "admins_id_fkey"},
		"INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Nobody')", ledgerNoSuchID)
	ledgerExpectOK(t, tx, "control: a user becomes an admin", "INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Carol')", c)
	ledgerExpectRefused(t, tx, "the same admin twice", "23505", ledgerWhy{Constraint: "admins_pkey"}, "INSERT INTO platform.admins (id, display_name) VALUES ($1, 'Carol again')", c)
	if got := ledgerString(t, tx, "SELECT (disabled_at IS NULL)::text FROM platform.admins WHERE id = $1", c); got != "true" {
		t.Fatalf("a new admin has disabled_at set (disabled_at IS NULL = %s)", got)
	}

	t.Run("the app reads admins and cannot write them", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admins"); n < 3 {
			t.Fatalf("platform_app sees %d admins, want 3", n)
		}
		why := ledgerWhy{Message: "permission denied for table admins"}
		ledgerExpectRefused(t, tx, "platform_app INSERT on admins", "42501", why, "INSERT INTO platform.admins (id, display_name) VALUES ($1, 'x')", ledgerUserB)
		ledgerExpectRefused(t, tx, "platform_app UPDATE on admins", "42501", why, "UPDATE platform.admins SET display_name = 'x'")
		ledgerExpectRefused(t, tx, "platform_app DELETE on admins", "42501", why, "DELETE FROM platform.admins")
		ledgerResetRole(t, tx)
	})

	// Phase 1 pinned the opposite here ("documented gap: disabled_at ends nothing by itself": a grant naming a disabled admin was accepted). The owner's ruling OD-10 reverses it and
	// platform_0004 makes the DATABASE refuse: every statement below names an admin, in the ledger, the audit log, M3 entitlements or the unit costs. Each is built for any admin id
	// (disabledAdminStmts), so the same statement is the paired control with an active admin.
	t.Run("a disabled admin is refused by the database wherever an admin is named (OD-10); an unknown one by the foreign key; NULL still passes", func(t *testing.T) {
		dis, act := ledgerAdminB, ledgerAdminA
		stmts := disabledAdminStmts()
		if len(stmts) < 8 {
			t.Fatalf("the proof covers %d statements, want at least 8 (ledger x3, audit log, entitlement insert and update, unit cost x2): the instrument matched too little", len(stmts))
		}
		// the fixtures the statements need: credits for the adjustment to draw on, the entitlement row the UPDATE hits (changed_by NULL), and history that names B while B is active
		ledgerFund(t, tx, ledgerUserA, "m1", 100)
		ledgerMust(t, tx, "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)", ledgerUserB)
		histGrant := ledgerPut(t, tx, ledgerGrantCols(ledgerUserA, "m1", 5).with("acting_admin_id", dis, "reason", "written while the admin was active"))
		ledgerMust(t, tx, `INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, reason)
			VALUES ($1, 'action', 'history-before-disable', 'user', 'x', 'written while the admin was active')`, dis)
		histRow := func() string {
			return ledgerString(t, tx, "SELECT x::text FROM platform.ledger x WHERE id = $1", histGrant)
		}
		histBefore := histRow()

		// 1. While B is ACTIVE every statement naming B passes (each in a savepoint that is rolled back): the statements are legal, so what follows is about disabled_at and nothing else.
		for _, s := range stmts {
			sql, args := s.Build(dis)
			ledgerTrial(t, tx, func(sp pgx.Tx) { ledgerExpectOK(t, sp, "control, B active: "+s.Name, sql, args...) })
		}

		// 2. B is disabled: every statement naming B is refused with 23514 and the constraint name admin_is_active, by the trigger of that column (the message names it); the same
		// statement naming an UNKNOWN admin is still the foreign key's to refuse (23503: the trigger leaves "not found" alone, and the foreign key is never the one that answers for a disabled
		// admin); the same statement naming the ACTIVE admin A passes (and is kept, so it comes last).
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", dis)
		if !ledgerBool(t, tx, "SELECT disabled_at IS NOT NULL FROM platform.admins WHERE id = $1", dis) {
			t.Fatal("admin B is not disabled after the UPDATE: every refusal below would be vacuous")
		}
		for _, s := range stmts {
			sql, args := s.Build(dis)
			ledgerExpectRefused(t, tx, "B disabled: "+s.Name, "23514", ledgerWhy{Constraint: "admin_is_active", Message: s.Where + " names admin " + dis}, sql, args...)
			sql, args = s.Build(ledgerNoSuchID)
			ledgerExpectRefused(t, tx, "an unknown admin: "+s.Name, "23503", ledgerWhy{Constraint: s.FK}, sql, args...)
			sql, args = s.Build(act)
			ledgerExpectOK(t, tx, "control, the same statement naming the ACTIVE admin A: "+s.Name, sql, args...)
		}

		// 3. NULL is not a disabled admin: where the database accepted no admin before, it still does (an entitlement with no changed_by, an automatic expiry), and where it demanded one it
		// still does, with its own message.
		ledgerExpectOK(t, tx, "NULL changed_by on an entitlement INSERT while B is disabled", "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)", ledgerAdminA)
		ledgerExpectOK(t, tx, "NULL changed_by on an entitlement UPDATE while B is disabled", "UPDATE platform.m3_entitlements SET enabled = false, changed_by = NULL WHERE user_id = $1", ledgerAdminA)
		// m3_expire names no admin, so it passes while B is disabled - once it is DUE (platform_0006 refuses an expiry unless the entitlement is enabled and its date has passed): user A's
		// entitlement was enabled by the m3_enable statement above (kept for the active admin A), and time is made to pass for it.
		ledgerMakeDue(t, tx, ledgerUserA)
		ledgerOKRow(t, tx, "m3_expire has no admin and passes while B is disabled", ledgerM3Cols("m3_expire", ledgerUserA))
		ledgerExpectRefused(t, tx, "a price change with NO admin is still refused by the older rule, not by admin_is_active", "23514", ledgerWhy{Message: "needs changed_by"},
			"UPDATE platform.unit_costs SET cost = 4, changed_by = NULL, change_reason = 'no admin' WHERE unit = 'm2_accepted'")

		// 4. Disabling an admin does not rewrite history: what B did while active is still there, byte for byte, and still readable by the application's roles.
		if got := histRow(); got != histBefore {
			t.Fatalf("the ledger row B wrote while active changed after B was disabled:\n  before: %s\n  after:  %s", histBefore, got)
		}
		if got := ledgerString(t, tx, "SELECT acting_admin_id::text FROM platform.ledger WHERE id = $1", histGrant); got != dis {
			t.Fatalf("the row B wrote now names %s, want %s", got, dis)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admin_audit WHERE admin_id = $1 AND action = 'history-before-disable'", dis); n != 1 {
			t.Fatalf("%d audit rows by B remain, want the 1 written while B was active", n)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admins WHERE id = $1 AND disabled_at IS NOT NULL", dis); n != 1 {
			t.Fatalf("admin B is not in platform.admins as a disabled admin (%d rows): disabling must keep the row that history points at", n)
		}
		ledgerRequireViewsEmpty(t, tx, "after B was disabled (the balance still equals the ledger)")
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE acting_admin_id = $1", dis); n < 1 {
				t.Fatalf("%s sees %d ledger rows naming the disabled admin, want the history to stay readable", role, n)
			}
			ledgerResetRole(t, tx)
		}

		// 5. The trigger reads the row live: re-enabling B lets B act, disabling B again stops B.
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = NULL WHERE id = $1", dis)
		sql, args := stmts[0].Build(dis)
		ledgerExpectOK(t, tx, "control: B re-enabled", sql, args...)
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", dis)
		sql, args = stmts[0].Build(dis)
		ledgerExpectRefused(t, tx, "B disabled again", "23514", ledgerWhy{Constraint: "admin_is_active", Message: stmts[0].Where + " names admin " + dis}, sql, args...)
	})
}

// disabledAdminStmt is one statement that names an admin, buildable for any admin id: the unit of the disabled-admin proofs (TestConstraints_Admins and
// TestConstraints_DisabledAdminThroughTheRoles). The same statement with an active admin is its paired control.
type disabledAdminStmt struct {
	Name  string
	Table string   // the table whose privilege the statement needs
	Where string   // the column the trigger names in its refusal
	FK    string   // the foreign key that refuses an UNKNOWN admin in this statement
	Roles []string // the roles that hold the privilege to run it
	Build func(admin string) (string, []any)
}

// disabledAdminStmts expects: user A funded on m1 and holding NO entitlement, an m3_entitlements row for user B (changed_by NULL), no entitlement for user C.
//
// Since platform_0006 no application role can write platform.m3_entitlements (the ledger trigger is its only writer), so the two statements that write it directly are runnable by nobody but the
// table's owner and a superuser (Roles is empty): TestConstraints_Admins still proves, as the superuser, that the entitlement trigger refuses a disabled admin; TestConstraints_DisabledAdminThroughTheRoles
// proves the roles are refused them by privilege (42501) and that the ledger path (an m3_enable naming the admin) is refused by the ledger's own trigger.
func disabledAdminStmts() []disabledAdminStmt {
	insert := func(table string, cols func(admin string) ledgerCols) func(string) (string, []any) {
		return func(ad string) (string, []any) { return ledgerInsertInto(table, cols(ad)) }
	}
	both, none := []string{"platform_app", "platform_admin"}, []string{}
	app := []string{"platform_app"}
	const (
		ledgerFKName = "ledger_acting_admin_id_fkey"
		m3FKName     = "m3_entitlements_changed_by_fkey"
		costFKName   = "unit_costs_changed_by_fkey"
		m3Where      = "platform.m3_entitlements.changed_by"
		costWhere    = "platform.unit_costs.changed_by"
		ledgerWhere  = "platform.ledger.acting_admin_id"
	)
	return []disabledAdminStmt{
		{"a ledger grant naming the admin", "ledger", ledgerWhere, ledgerFKName, both,
			insert("platform.ledger", func(ad string) ledgerCols { return ledgerGrantCols(ledgerUserA, "m1", 1).with("acting_admin_id", ad) })},
		{"a ledger adjustment naming the admin", "ledger", ledgerWhere, ledgerFKName, both,
			insert("platform.ledger", func(ad string) ledgerCols {
				return ledgerAdjustmentCols(ledgerUserA, "m1", 1).with("acting_admin_id", ad)
			})},
		// user A: the one user with no entitlement and no other statement of this list writing one (the statement writes the entitlement through the ledger trigger since platform_0006)
		{"an m3_enable event naming the admin", "ledger", ledgerWhere, ledgerFKName, both,
			insert("platform.ledger", func(ad string) ledgerCols { return ledgerM3Cols("m3_enable", ledgerUserA).with("acting_admin_id", ad) })},
		{"an admin_audit row written by the admin", "admin_audit", "platform.admin_audit.admin_id", "admin_audit_admin_id_fkey", both,
			insert("platform.admin_audit", func(ad string) ledgerCols {
				return ledgerCols{"admin_id": ad, "entry_type": "action", "action": "disabled-admin-proof", "target_type": "user", "target_id": "x", "reason": "proof"}
			})},
		{"an m3_entitlements INSERT with changed_by = the admin", "m3_entitlements", m3Where, m3FKName, none,
			insert("platform.m3_entitlements", func(ad string) ledgerCols {
				return ledgerCols{"user_id": ledgerUserC, "enabled": true, "changed_by": ad}
			})},
		{"an m3_entitlements UPDATE that changes changed_by to the admin", "m3_entitlements", m3Where, m3FKName, none,
			func(ad string) (string, []any) {
				return "UPDATE platform.m3_entitlements SET changed_by = $2 WHERE user_id = $1", []any{ledgerUserB, ad}
			}},
		{"a unit_costs cost change by the admin", "unit_costs", costWhere, costFKName, app,
			func(ad string) (string, []any) {
				return "UPDATE platform.unit_costs SET cost = 3, changed_by = $1, change_reason = 'disabled-admin proof' WHERE unit = 'm2_accepted'", []any{ad}
			}},
		{"a unit_costs changed_by change to the admin (the cost untouched)", "unit_costs", costWhere, costFKName, app,
			func(ad string) (string, []any) {
				return "UPDATE platform.unit_costs SET changed_by = $1 WHERE unit = 'm2_bounced'", []any{ad}
			}},
	}
}

// TestConstraints_DisabledAdminThroughTheRoles runs the disabled-admin proof as the two roles the service really runs as. The triggers that refuse a disabled admin run for the CALLER
// (they are not the owner's), so a role that cannot use what they use fails here: with an ACTIVE admin every statement the role has the privilege for must pass, with a DISABLED one it
// must be refused with 23514 admin_is_active (never with a permission error), and a statement the role has no privilege for is refused with 42501 before any trigger. It fails if a role
// cannot write at all since platform_0004, or if the refusal for a disabled admin comes from anywhere but the trigger of the column that names the admin.
func TestConstraints_DisabledAdminThroughTheRoles(t *testing.T) {
	_, admin := migratedScratch(t)
	dis, act := ledgerAdminB, ledgerAdminA
	for _, role := range []string{"platform_app", "platform_admin"} {
		role := role
		t.Run(role, func(t *testing.T) {
			tx := ledgerBegin(t, admin)
			ledgerSeed(t, tx)
			ledgerFund(t, tx, ledgerUserA, "m1", 100)
			ledgerMust(t, tx, "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)", ledgerUserB)
			ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", dis)
			if !ledgerBool(t, tx, "SELECT disabled_at IS NOT NULL FROM platform.admins WHERE id = $1", dis) {
				t.Fatal("admin B is not disabled: every refusal below would be vacuous")
			}
			ledgerSwitchRole(t, tx, role)
			ran, byPrivilege := 0, 0
			for _, s := range disabledAdminStmts() {
				if !containsString(s.Roles, role) {
					sql, args := s.Build(act)
					ledgerExpectRefused(t, tx, role+" holds no privilege for "+s.Name, "42501", ledgerWhy{Message: "permission denied for table " + s.Table}, sql, args...)
					byPrivilege++
					continue
				}
				sql, args := s.Build(dis)
				ledgerExpectRefused(t, tx, role+": "+s.Name+", the admin disabled", "23514", ledgerWhy{Constraint: "admin_is_active", Message: s.Where + " names admin " + dis}, sql, args...)
				sql, args = s.Build(act)
				ledgerExpectOK(t, tx, "control: "+role+": "+s.Name+", the admin active", sql, args...)
				ran++
			}
			if ran < 4 {
				t.Fatalf("%s ran only %d statements: the instrument matched too little", role, ran)
			}
			if role == "platform_admin" && byPrivilege < 4 {
				t.Fatalf("platform_admin was refused by privilege on only %d statements, want the 4 it holds no privilege for", byPrivilege)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// the system user (platform_0004, the owner's ruling OD-12)
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_SystemUserHoldsNothing proves that the all-zero system user - module 2's owner of shared pools, a real row of public.users with no login - can hold no balance, no
// ledger row, no entitlement and no suppression. The row EXISTS (checked first), so the foreign keys to public.users are satisfied and only the CHECK can be what refuses it; for each table the
// paired controls are the same statement for a normal user (passes), the same statement for the system user with the CHECK dropped (passes: the CHECK is the only barrier), a NULL user
// (refused by NOT NULL, so each CHECK is NULL-safe: CLAUDE.md rule 13) and a row MOVED to the system user by an UPDATE. The ledger path is two walls: the ledger CHECK fires first (so the
// balance trigger never runs), and with it removed the balance trigger's own INSERT is refused by the balances CHECK. It fails if any of the four CHECKs is missing, misses an UPDATE, or
// is renamed.
func TestConstraints_SystemUserHoldsNothing(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	sys, a, b := ledgerSystemUser, ledgerUserA, ledgerUserB

	if n := ledgerInt64(t, tx, "SELECT count(*) FROM public.users WHERE id = $1", sys); n != 1 {
		t.Fatalf("the system user row is missing from public.users (%d rows): a foreign key would refuse it and the CHECKs below would prove nothing", n)
	}

	for _, c := range []struct {
		table, constraint, insert, move string
		syncFollows                     bool // the row's key is also the key of a delivery row (platform_0006: platform.m3_sync), so it cannot be moved while that row exists
	}{
		{"platform.balances", "balances_not_the_system_user", "INSERT INTO platform.balances (user_id, module_id) VALUES ($1, 'm1')",
			"UPDATE platform.balances SET user_id = $1 WHERE user_id = $2 AND module_id = 'm1'", false},
		{"platform.m3_entitlements", "m3_entitlements_not_the_system_user", "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)",
			"UPDATE platform.m3_entitlements SET user_id = $1 WHERE user_id = $2", true},
		{"platform.suppressions", "suppressions_not_the_system_user", "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, 'sys@example.com', 'manual')",
			"UPDATE platform.suppressions SET user_id = $1 WHERE user_id = $2", false},
	} {
		c := c
		t.Run(c.table, func(t *testing.T) {
			ledgerExpectRefused(t, tx, "INSERT for the system user", "23514", ledgerWhy{Constraint: c.constraint}, c.insert, sys)
			ledgerExpectRefused(t, tx, "INSERT for a NULL user (the column is NOT NULL, so the CHECK never sees a NULL)", "23502", ledgerWhy{Column: "user_id"}, c.insert, nil)
			if tag := ledgerExpectOK(t, tx, "control: the same INSERT for a normal user", c.insert, a); tag.RowsAffected() != 1 {
				t.Fatalf("the control inserted %d rows", tag.RowsAffected())
			}
			ledgerExpectRefused(t, tx, "moving a normal user's row to the system user", "23514", ledgerWhy{Constraint: c.constraint}, c.move, sys, a)
			if c.syncFollows {
				// the entitlement trigger wrote the delivery row of this user (m3_sync, which references the entitlement's key): the key cannot move under it ...
				ledgerExpectRefused(t, tx, "moving the row to another normal user while its delivery row exists", "23503", ledgerWhy{Constraint: "m3_sync_user_id_fkey"}, c.move, b, a)
				// ... and with the delivery row gone the very same move is legal, so the refusal above was the delivery row's and the CHECK's refusal of the system user is not hiding it
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					if tag := ledgerMust(t, sp, "DELETE FROM platform.m3_sync WHERE user_id = $1", a); tag.RowsAffected() != 1 {
						t.Fatalf("the delivery row of user A was not there (%d rows deleted): the control below would prove nothing", tag.RowsAffected())
					}
					ledgerExpectOK(t, sp, "control: moving it to another normal user once nothing references the key", c.move, b, a)
				})
			} else {
				ledgerExpectOK(t, tx, "control: moving it to another normal user", c.move, b, a)
			}
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				ledgerMust(t, sp, "ALTER TABLE "+c.table+" DROP CONSTRAINT "+c.constraint)
				ledgerExpectOK(t, sp, "control: without the CHECK the same INSERT for the system user passes (the CHECK is the only barrier)", c.insert, sys)
			})
		})
	}

	t.Run("platform.ledger: every kind that names the system user is refused; the balance trigger is a second wall", func(t *testing.T) {
		const chk = "ledger_not_the_system_user"
		nLedger := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger")
		for _, c := range []struct {
			name string
			cols ledgerCols
		}{
			{"a grant", ledgerGrantCols(sys, "m1", 10)},
			{"an adjustment", ledgerAdjustmentCols(sys, "m1", 1)},
			{"an m3_enable event", ledgerM3Cols("m3_enable", sys)},
			{"an m3_expire event (no admin)", ledgerM3Cols("m3_expire", sys)},
		} {
			ledgerRefusedCheck(t, tx, "the system user: "+c.name, chk, c.cols)
		}
		ledgerRefusedRow(t, tx, "a grant for a NULL user", "23502", ledgerWhy{Column: "user_id"}, ledgerGrantCols(sys, "m1", 10).with("user_id", nil))
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"); got != nLedger {
			t.Fatalf("the refused rows left %d ledger rows behind", got-nLedger)
		}
		if bal := ledgerBalance(t, tx, sys, "m1"); bal.Exists {
			t.Fatalf("a balance row exists for the system user: %+v", bal)
		}
		ledgerOKRow(t, tx, "control: the same grant for a normal user", ledgerGrantCols(a, "m1", 10))

		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT "+chk)
			ledgerRefusedRow(t, sp, "with the ledger CHECK gone, the balance trigger's INSERT into platform.balances is refused", "23514",
				ledgerWhy{Constraint: "balances_not_the_system_user", Table: "balances"}, ledgerGrantCols(sys, "m1", 10))
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT "+chk)
			ledgerMust(t, sp, "ALTER TABLE platform.balances DROP CONSTRAINT balances_not_the_system_user")
			ledgerOKRow(t, sp, "control: with both walls gone the system user's grant passes", ledgerGrantCols(sys, "m1", 10))
		})
	})
}

// TestConstraints_ListableUsers proves platform.listable_users, "the only sanctioned way to list users": every real person (a Supabase login behind the row) and nothing else. The system user
// is a row of public.users, so the view must be what leaves it out: the count of the view is the count of public.users less that row. Each of the view's two clauses is proven ALONE, on a
// scratch copy where users_auth_link (the CHECK that makes the situation impossible) is dropped: a row with no auth link is left out by the second clause, and the system user is left out by the
// first even when it is given an auth link. A new real user shows at once (the view is a query, not a copy), and both application roles can read it. It fails if the view lists the system
// user, hides a real one, or loses a clause.
func TestConstraints_ListableUsers(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx) // three tenants and two admins (all real users) beside the system user platform_0001 seeds
	const view = "platform.listable_users"
	people := []string{ledgerUserA, ledgerUserB, ledgerUserC, ledgerAdminA, ledgerAdminB}
	listed := func(t *testing.T, db ledgerDB) string {
		t.Helper()
		return strings.Join(ledgerRows(t, db, "SELECT id::text FROM "+view+" ORDER BY 1"), ",")
	}

	t.Run("the view lists the five real users and not the system user, which public.users holds", func(t *testing.T) {
		if got, want := listed(t, tx), strings.Join(people, ","); got != want {
			t.Fatalf("%s lists %s, want %s", view, got, want)
		}
		if total := ledgerInt64(t, tx, "SELECT count(*) FROM public.users"); total != int64(len(people))+1 {
			t.Fatalf("public.users holds %d rows, want the %d real users and the system user: the comparison below would prove nothing", total, len(people))
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM public.users WHERE id = $1", ledgerSystemUser); n != 1 {
			t.Fatalf("the system user is not in public.users (%d): the view's exclusion would be vacuous", n)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM "+view+" WHERE id = $1", ledgerSystemUser); n != 0 {
			t.Fatalf("the view lists the system user (%d rows)", n)
		}
		if listedN, total := ledgerInt64(t, tx, "SELECT count(*) FROM "+view), ledgerInt64(t, tx, "SELECT count(*) FROM public.users"); listedN != total-1 {
			t.Fatalf("the view lists %d of the %d users of public.users, want exactly one fewer (the system user)", listedN, total)
		}
	})

	t.Run("the columns are the people-facing ones, straight from public.users", func(t *testing.T) {
		got := ledgerString(t, tx, "SELECT string_agg(attname::text, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'platform.listable_users'::regclass AND attnum > 0 AND NOT attisdropped")
		if want := "id,auth_user_id,email,is_active,created_at"; got != want {
			t.Fatalf("the view's columns are %s, want %s", got, want)
		}
		if n := ledgerInt64(t, tx, `SELECT count(*) FROM platform.listable_users v JOIN public.users u ON u.id = v.id
			WHERE v.auth_user_id IS NOT DISTINCT FROM u.auth_user_id AND v.email = u.email AND v.is_active = u.is_active AND v.created_at = u.created_at`); n != int64(len(people)) {
			t.Fatalf("%d of the %d listed rows equal their public.users row", n, len(people))
		}
	})

	t.Run("a new real user shows at once", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			const d = "ab000000-0000-0000-0000-0000000000d1"
			ledgerMust(t, sp, "INSERT INTO auth.users (id, email) VALUES ($1, 'd@example.test')", d)
			ledgerMust(t, sp, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'd@example.test')", d)
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM "+view+" WHERE id = $1", d); n != 1 {
				t.Fatalf("a user inserted into public.users is listed %d times, want once", n)
			}
		})
	})

	t.Run("a row with no auth link is left out by the second clause alone", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE public.users DROP CONSTRAINT users_auth_link") // the CHECK that makes this row impossible; dropped on this scratch copy only
			const nolink, linked = "ab000000-0000-0000-0000-0000000000e1", "ab000000-0000-0000-0000-0000000000e2"
			ledgerMust(t, sp, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, NULL, 'nolink@example.test')", nolink)
			ledgerMust(t, sp, "INSERT INTO auth.users (id, email) VALUES ($1, 'linked@example.test')", linked)
			ledgerMust(t, sp, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'linked@example.test')", linked)
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM public.users WHERE id = $1", nolink); n != 1 {
				t.Fatalf("the user with no auth link is not in public.users (%d): the exclusion below would be vacuous", n)
			}
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM "+view+" WHERE id = $1", nolink); n != 0 {
				t.Fatalf("the view lists a user with no auth link (%d rows)", n)
			}
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM "+view+" WHERE id = $1", linked); n != 1 {
				t.Fatalf("control: a user inserted the same way WITH an auth link is listed %d times, want once", n)
			}
			if listedN, total := ledgerInt64(t, sp, "SELECT count(*) FROM "+view), ledgerInt64(t, sp, "SELECT count(*) FROM public.users"); listedN != total-2 {
				t.Fatalf("the view lists %d of %d users, want two fewer (the system user and the user with no link)", listedN, total)
			}
		})
	})

	t.Run("the system user is left out by the first clause alone, even with an auth link", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE public.users DROP CONSTRAINT users_auth_link")
			const link = "ab000000-0000-0000-0000-0000000000f1"
			ledgerMust(t, sp, "INSERT INTO auth.users (id, email) VALUES ($1, 'sys-link@example.test')", link)
			ledgerMust(t, sp, "UPDATE public.users SET auth_user_id = $1 WHERE id = $2", link, ledgerSystemUser)
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM public.users WHERE id = $1 AND auth_user_id IS NOT NULL", ledgerSystemUser); n != 1 {
				t.Fatalf("the system row has no auth link after the UPDATE (%d): it would still be excluded by the second clause and this case would prove nothing about the first", n)
			}
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM "+view+" WHERE id = $1", ledgerSystemUser); n != 0 {
				t.Fatalf("the view lists the system user once it has an auth link (%d rows): the all-zero id is not excluded by itself", n)
			}
		})
	})

	t.Run("both application roles read it", func(t *testing.T) {
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			if got, want := listed(t, tx), strings.Join(people, ","); got != want {
				t.Fatalf("as %s, %s lists %s, want %s", role, view, got, want)
			}
			ledgerResetRole(t, tx)
		}
	})
}

// TestConstraints_ModulesSeed covers the registry: three rows, M3 unmetered, the id shape, uniqueness; the app cannot edit it.
// It fails if a module is missing or mis-flagged (M3 metered would give it a balance), or the registry becomes writable.
func TestConstraints_ModulesSeed(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)

	got := ledgerRows(t, tx, "SELECT id, name, schema_name, metered::text, active::text FROM platform.modules ORDER BY id")
	want := []string{"m1|verify|verify|true|true", "m2|send-managed|send_managed|true|true", "m3|send-byo|send_byo|false|true"}
	if len(got) != len(want) {
		t.Fatalf("platform.modules holds %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("module row %d is %q, want %q", i, got[i], want[i])
		}
	}

	t.Run("the id shape", func(t *testing.T) {
		for _, id := range []string{"M1", "1x", "m-1", "", "m 1"} {
			id := id
			ledgerExpectRefused(t, tx, fmt.Sprintf("module id %q", id), "23514", ledgerWhy{Constraint: "modules_id_shape"},
				fmt.Sprintf("INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('%s', 'n-%s', 's-%s', true)", id, id, id))
		}
		ledgerExpectOK(t, tx, "control: a well-formed id", "INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('m4', 'fourth', 'fourth_schema', true)")
		ledgerExpectRefused(t, tx, "a second module with the same name", "23505", ledgerWhy{Constraint: "modules_name_key"},
			"INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('m5', 'fourth', 's5', true)")
		ledgerExpectRefused(t, tx, "a second module with the same schema", "23505", ledgerWhy{Constraint: "modules_schema_name_key"},
			"INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('m5', 'n5', 'fourth_schema', true)")
	})

	t.Run("the app reads the registry and cannot write it", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.modules"); n < 3 {
			t.Fatalf("platform_app sees %d modules", n)
		}
		why := ledgerWhy{Message: "permission denied for table modules"}
		ledgerExpectRefused(t, tx, "platform_app INSERT on modules", "42501", why, "INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('m6', 'n6', 's6', true)")
		ledgerExpectRefused(t, tx, "platform_app UPDATE on modules (flipping m3 to metered)", "42501", why, "UPDATE platform.modules SET metered = true WHERE id = 'm3'")
		ledgerExpectRefused(t, tx, "platform_app DELETE on modules", "42501", why, "DELETE FROM platform.modules")
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// unit costs
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_UnitCosts covers unit_costs and its history: the five seeded units with their defaults, the cost bounds,
// the rule that a change needs a named admin and a reason, the history written by trigger (the seed rows included), and the
// column-level grant (the app may change the cost columns and nothing else). It fails if a seed or default changes, a bound
// is lost, the guard trigger is missing, or the history misses a change.
func TestConstraints_UnitCosts(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	adminA, plainUser := ledgerAdminA, ledgerUserB
	setCost := func(unit string, cost int64, by any, reason any) (string, []any) {
		return "UPDATE platform.unit_costs SET cost = $2, changed_by = $3, change_reason = $4 WHERE unit = $1", []any{unit, cost, by, reason}
	}

	t.Run("the five units and their defaults, and a history row for each seed", func(t *testing.T) {
		want := []string{"m1_cache|m1|1||", "m1_own|m1|1||", "m1_vendor|m1|2||", "m2_accepted|m2|1||", "m2_bounced|m2|1||"}
		got := ledgerRows(t, tx, "SELECT unit, module_id, cost, COALESCE(changed_by::text, ''), COALESCE(change_reason, '') FROM platform.unit_costs ORDER BY unit")
		if strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("platform.unit_costs = %v, want %v", got, want)
		}
		hist := ledgerRows(t, tx, "SELECT unit, COALESCE(old_cost::text, ''), new_cost, COALESCE(changed_by::text, ''), change_reason FROM platform.unit_cost_history ORDER BY id")
		wantHist := []string{"m1_own||1||seeded", "m1_cache||1||seeded", "m1_vendor||2||seeded", "m2_accepted||1||seeded", "m2_bounced||1||seeded"}
		if strings.Join(hist, ";") != strings.Join(wantHist, ";") {
			t.Fatalf("the seed history = %v, want %v (written by the trigger, so no writer can skip it)", hist, wantHist)
		}
	})

	t.Run("the bounds: cost >= 1 and the vendor premium > 1", func(t *testing.T) {
		for _, c := range []struct {
			unit string
			cost int64
		}{{"m1_own", 0}, {"m2_accepted", -1}} {
			sql, args := setCost(c.unit, c.cost, adminA, "bound test")
			ledgerExpectRefused(t, tx, fmt.Sprintf("%s set to %d", c.unit, c.cost), "23514", ledgerWhy{Constraint: "unit_costs_cost_at_least_one"}, sql, args...)
		}
		sql, args := setCost("m1_vendor", 1, adminA, "bound test")
		ledgerExpectRefused(t, tx, "the vendor fallback set to 1 (it must be a premium)", "23514", ledgerWhy{Constraint: "unit_costs_vendor_dearer"}, sql, args...)
		sql, args = setCost("m1_vendor", 3, adminA, "bound test")
		ledgerExpectOK(t, tx, "control: the vendor fallback set to 3", sql, args...)
		sql, args = setCost("m1_own", 1, adminA, "bound test")
		ledgerExpectOK(t, tx, "control: a non-vendor unit may cost exactly 1", sql, args...)
		ledgerExpectRefused(t, tx, "history cannot record a new cost of 0 either", "23514", ledgerWhy{Constraint: "unit_cost_history_new_cost_at_least_one"},
			"INSERT INTO platform.unit_cost_history (unit, old_cost, new_cost) VALUES ('m1_own', 1, 0)")
		ledgerExpectRefused(t, tx, "a unit of a module that does not exist", "23503", ledgerWhy{Constraint: "unit_costs_module_id_fkey"},
			"INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES ('m9_unit', 'm9', 'd', 1)")
	})

	t.Run("a change of cost needs a named admin and a non-blank reason", func(t *testing.T) {
		why := ledgerWhy{Message: "needs changed_by"}
		for _, c := range []struct {
			name   string
			by     any
			reason any
		}{{"no admin and no reason", nil, nil}, {"a reason but no admin", nil, "r"}, {"an admin but no reason", adminA, nil}, {"an admin and a blank reason", adminA, "   "}, {"an admin and an empty reason", adminA, ""}} {
			sql, args := setCost("m2_accepted", 3, c.by, c.reason)
			ledgerExpectRefused(t, tx, c.name, "23514", why, sql, args...)
		}
		sql, args := setCost("m2_accepted", 3, plainUser, "r")
		ledgerExpectRefused(t, tx, "changed_by names a user who is not an admin", "23503", ledgerWhy{Constraint: "unit_costs_changed_by_fkey"}, sql, args...)
		before := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history")
		sql, args = setCost("m2_accepted", 3, adminA, "price review")
		ledgerExpectOK(t, tx, "control: a named admin with a reason", sql, args...)
		if got := ledgerString(t, tx, "SELECT old_cost || '>' || new_cost || '|' || changed_by::text || '|' || change_reason FROM platform.unit_cost_history WHERE unit = 'm2_accepted' ORDER BY id DESC LIMIT 1"); got != "1>3|"+adminA+"|price review" {
			t.Fatalf("the history row written by the trigger is %q, want 1>3|%s|price review", got, adminA)
		}
		if after := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history"); after != before+1 {
			t.Fatalf("the change wrote %d history rows, want 1", after-before)
		}
	})

	t.Run("an UPDATE that does not change the cost needs no admin and writes no history; updated_at always moves", func(t *testing.T) {
		before := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history")
		ledgerExpectOK(t, tx, "a write of the same cost", "UPDATE platform.unit_costs SET cost = cost WHERE unit = 'm2_bounced'")
		if after := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history"); after != before {
			t.Fatalf("a no-op write added %d history rows", after-before)
		}
		ledgerExpectOK(t, tx, "a write that tries to set updated_at", "UPDATE platform.unit_costs SET updated_at = '2000-01-01T00:00:00Z' WHERE unit = 'm2_bounced'")
		if !ledgerBool(t, tx, "SELECT updated_at = now() FROM platform.unit_costs WHERE unit = 'm2_bounced'") {
			t.Fatal("updated_at is not now() after an UPDATE: the guard trigger does not own it")
		}
	})

	t.Run("who may change what: the app only the cost columns, the break-glass role nothing", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		why := ledgerWhy{Message: "permission denied for table unit_costs"}
		ledgerExpectRefused(t, tx, "platform_app UPDATE of description (a column it holds no grant on)", "42501", why, "UPDATE platform.unit_costs SET description = 'x' WHERE unit = 'm1_own'")
		ledgerExpectRefused(t, tx, "platform_app INSERT of a new unit", "42501", why, "INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES ('m1_new', 'm1', 'd', 1)")
		ledgerExpectRefused(t, tx, "platform_app DELETE of a unit", "42501", why, "DELETE FROM platform.unit_costs")
		sql, args := setCost("m2_bounced", 4, adminA, "through the column grant")
		ledgerExpectOK(t, tx, "control: platform_app UPDATE of cost, changed_by, change_reason", sql, args...)
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		sql, args = setCost("m2_bounced", 5, adminA, "break-glass")
		ledgerExpectRefused(t, tx, "platform_admin UPDATE of cost", "42501", why, sql, args...)
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_costs"); n != 5 {
			t.Fatalf("platform_admin sees %d unit costs, want 5", n)
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// jobs, price snapshots, balances, entitlements
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_Jobs covers platform.jobs: the status vocabulary, the scheduled-needs-a-start rule, the shape of
// frozen_settings, chaining (never its own source, same user), the unique module_job_ref, the one-ACTIVE-job slot (a scheduled,
// running, paused or cancelling job holds it since platform_0004, the owner's ruling OD-2; a draft or a finished job holds none; every
// held status is proven against every other status, and the slot is followed through a job's life), and jobs_guard (immutable columns,
// frozen_settings frozen once the job leaves draft, updated_at owned by the trigger). It fails if any of these rules is missing, or
// the slot is held by the wrong statuses.
func TestConstraints_Jobs(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b, c := ledgerUserA, ledgerUserB, ledgerUserC
	const tbl = "platform.jobs"
	n := 0
	job := func(user, module string, kv ...any) ledgerCols {
		n++
		return ledgerCols{"id": ledgerUUID("ee", n), "user_id": user, "module_id": module}.with(kv...)
	}
	start := "2030-01-01T00:00:00Z"
	ok := func(t *testing.T, what string, cols ledgerCols) { t.Helper(); ledgerOKInsert(t, tx, what, tbl, cols) }
	refused := func(t *testing.T, what, state string, why ledgerWhy, cols ledgerCols) {
		t.Helper()
		ledgerRefusedInsert(t, tx, what, state, why, tbl, cols)
	}

	t.Run("the status vocabulary", func(t *testing.T) {
		for _, s := range []struct{ status, user string }{{"draft", a}, {"running", a}, {"paused", b}, {"cancelling", c}, {"succeeded", a}, {"failed", a}, {"cancelled", a}} {
			ok(t, "control: status "+s.status, job(s.user, "m2", "status", s.status))
		}
		// since platform_0004 a SCHEDULED job holds the slot of its user and module, so this control takes a (user, module) pair no other job here is active on (user A has a running m2 job above)
		ok(t, "control: status scheduled with a start", job(ledgerAdminA, "m3", "status", "scheduled", "scheduled_start", start))
		for _, bad := range []string{"bogus", "Draft", "", "done"} {
			refused(t, fmt.Sprintf("status %q", bad), "23514", ledgerWhy{Constraint: "jobs_status_vocabulary"}, job(a, "m2", "status", bad))
		}
		refused(t, "scheduled without a start", "23514", ledgerWhy{Constraint: "jobs_scheduled_has_a_start"}, job(a, "m2", "status", "scheduled"))
	})

	t.Run("frozen_settings is a JSON object", func(t *testing.T) {
		ok(t, "control: the default {}", job(a, "m1"))
		ok(t, "control: an object", job(a, "m1", "frozen_settings", `{"a":1}`))
		for _, bad := range []string{`[]`, `"x"`, `null`, `1`} {
			refused(t, "frozen_settings "+bad, "23514", ledgerWhy{Constraint: "jobs_frozen_settings_is_an_object"}, job(a, "m1", "frozen_settings", bad))
		}
		refused(t, "frozen_settings SQL NULL", "23502", ledgerWhy{Column: "frozen_settings"}, job(a, "m1", "frozen_settings", nil))
	})

	t.Run("chaining: never its own source, and the source belongs to the same user", func(t *testing.T) {
		self := job(a, "m1")
		refused(t, "a job that is its own source", "23514", ledgerWhy{Constraint: "jobs_not_its_own_source"}, self.with("source_job_id", self["id"]))
		ok(t, "control: a send chained to another job of the same user", job(a, "m2", "source_job_id", ledgerJobA1))
		refused(t, "a source job that belongs to another user", "23503", ledgerWhy{Constraint: "jobs_source_same_user_fk"}, job(b, "m2", "source_job_id", ledgerJobA1))
		refused(t, "a source job that does not exist", "23503", ledgerWhy{Constraint: "jobs_source_same_user_fk"}, job(a, "m2", "source_job_id", ledgerUUID("ab", 1)))
		refused(t, "a module that does not exist", "23503", ledgerWhy{Constraint: "jobs_module_id_fkey"}, job(a, "m9"))
	})

	t.Run("module_job_ref is the module's own id, unique per module", func(t *testing.T) {
		ok(t, "control: a module job ref", job(a, "m1", "module_job_ref", "ref-1"))
		refused(t, "a blank ref", "23514", ledgerWhy{Constraint: "jobs_module_job_ref_not_blank"}, job(a, "m1", "module_job_ref", "   "))
		refused(t, "the same ref on the same module (another user)", "23505", ledgerWhy{Constraint: "jobs_module_job_ref_key"}, job(b, "m1", "module_job_ref", "ref-1"))
		ok(t, "control: the same ref on another module", job(b, "m2", "module_job_ref", "ref-1"))
		ok(t, "control: two jobs with no ref (NULLs are not equal)", job(a, "m1"))
	})

	// The owner's ruling OD-2 (Docs/phase-2-spec.md, Part A row 2): a SCHEDULED job holds the one-active-job slot, in the database. Phase 1 pinned the opposite here ("a scheduled job does NOT
	// hold the slot") against jobs_one_executing_per_user_module; platform_0004 replaced that index with jobs_one_active_per_user_module. Each holding status is tried against EVERY status
	// (so each of the eight is proven to take the slot or to leave it), each time in a savepoint that is rolled back, with the legal first insert as the control.
	t.Run("one ACTIVE job per user per module: scheduled, running, paused and cancelling hold the slot; draft and finished jobs hold none", func(t *testing.T) {
		u, other := ledgerAdminB, ledgerAdminA // two users that have no m1 or m2 job yet
		const slot = "jobs_one_active_per_user_module"
		statuses := []string{"draft", "scheduled", "running", "paused", "cancelling", "succeeded", "failed", "cancelled"}
		active := map[string]bool{"scheduled": true, "running": true, "paused": true, "cancelling": true}
		mk := func(user, module, status string) ledgerCols {
			c := job(user, module, "status", status)
			if status == "scheduled" {
				c = c.with("scheduled_start", start)
			}
			return c
		}
		held := 0
		for _, holder := range statuses {
			if !active[holder] {
				continue
			}
			holder := holder
			held++
			t.Run("held by a "+holder+" job", func(t *testing.T) {
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					ledgerOKInsert(t, sp, "control: the first "+holder+" m1 job is legal", tbl, mk(u, "m1", holder))
					for _, second := range statuses {
						if active[second] {
							ledgerRefusedInsert(t, sp, "a second "+second+" m1 job for the same user while a "+holder+" one holds the slot", "23505", ledgerWhy{Constraint: slot}, tbl, mk(u, "m1", second))
						} else {
							ledgerOKInsert(t, sp, "control: a "+second+" m1 job takes no slot, so it fits beside the "+holder+" one", tbl, mk(u, "m1", second))
						}
					}
					ledgerOKInsert(t, sp, "control: the OTHER module is another slot (a "+holder+" m2 job)", tbl, mk(u, "m2", holder))
					ledgerOKInsert(t, sp, "control: another USER has their own slot (a "+holder+" m1 job)", tbl, mk(other, "m1", holder))
				})
			})
		}
		if held != 4 {
			t.Fatalf("%d statuses were tried as slot holders, want the 4 of the ruling (scheduled, running, paused, cancelling)", held)
		}
	})

	t.Run("the slot follows the job: a second job cannot be confirmed while the first is active, and can once it has ended", func(t *testing.T) {
		const slot = "jobs_one_active_per_user_module"
		first, second := job(ledgerAdminB, "m1"), job(ledgerAdminB, "m1")
		confirm := "UPDATE platform.jobs SET status = 'scheduled', scheduled_start = now() WHERE id = $1"
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKInsert(t, sp, "control: a draft job", tbl, first)
			ledgerOKInsert(t, sp, "control: a second draft job of the same user and module (a draft holds no slot)", tbl, second)
			ledgerExpectOK(t, sp, "control: the first job is confirmed (draft -> scheduled) and takes the slot", confirm, first["id"])
			ledgerExpectRefused(t, sp, "confirming the second job while the first is scheduled", "23505", ledgerWhy{Constraint: slot}, confirm, second["id"])
			for _, st := range []string{"running", "paused", "cancelling"} {
				ledgerExpectOK(t, sp, "control: the first job moves on to "+st+" (it already holds the slot, so its own move cannot collide)", "UPDATE platform.jobs SET status = $2 WHERE id = $1", first["id"], st)
				ledgerExpectRefused(t, sp, "confirming the second job while the first is "+st, "23505", ledgerWhy{Constraint: slot}, confirm, second["id"])
			}
			ledgerExpectOK(t, sp, "control: the first job ends (cancelled) and frees the slot", "UPDATE platform.jobs SET status = 'cancelled' WHERE id = $1", first["id"])
			ledgerExpectOK(t, sp, "control: now the second job can be confirmed", confirm, second["id"])
			// since platform_0007 an ended (cancelled) job never moves again, so the move back to running is refused by jobs_terminal_is_final before the slot index is reached; the slot index is then proven
			// on the same move with the trigger switched off (a savepoint that is rolled back), and on a job that CAN legally move there (a draft one), so each rule is asserted alone
			ledgerExpectRefused(t, sp, "the first job (cancelled) moved back to running while the second holds the slot: the terminal rule answers first", "23001", ledgerWhy{Constraint: "jobs_terminal_is_final"},
				"UPDATE platform.jobs SET status = 'running' WHERE id = $1", first["id"])
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				ledgerMust(t, sp2, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_terminal_is_final")
				ledgerExpectRefused(t, sp2, "the first job moved back to running while the second holds the slot, the terminal trigger off (the index backs the rule)", "23505", ledgerWhy{Constraint: slot},
					"UPDATE platform.jobs SET status = 'running' WHERE id = $1", first["id"])
			})
			third := job(ledgerAdminB, "m1")
			ledgerOKInsert(t, sp, "control: a third draft job of the same user and module (a draft holds no slot)", tbl, third)
			ledgerExpectRefused(t, sp, "a draft job moved straight to running while the second job holds the slot", "23505", ledgerWhy{Constraint: slot},
				"UPDATE platform.jobs SET status = 'running' WHERE id = $1", third["id"])
		})
	})

	t.Run("jobs_guard: user, module, source and creation time never change", func(t *testing.T) {
		g1, g0 := job(c, "m1"), job(c, "m1")
		ok(t, "control: a draft job", g1)
		ok(t, "control: a second draft job of the same user", g0)
		id := g1["id"]
		why := ledgerWhy{Message: "never change"}
		ledgerExpectRefused(t, tx, "changing user_id", "23001", why, "UPDATE platform.jobs SET user_id = $2 WHERE id = $1", id, b)
		ledgerExpectRefused(t, tx, "changing module_id", "23001", why, "UPDATE platform.jobs SET module_id = 'm2' WHERE id = $1", id)
		ledgerExpectRefused(t, tx, "changing source_job_id", "23001", why, "UPDATE platform.jobs SET source_job_id = $2 WHERE id = $1", id, g0["id"])
		ledgerExpectRefused(t, tx, "changing created_at", "23001", why, "UPDATE platform.jobs SET created_at = created_at + interval '1 day' WHERE id = $1", id)

		t.Run("frozen_settings is editable in draft and frozen after", func(t *testing.T) {
			ledgerExpectOK(t, tx, "control: editing frozen_settings in draft", "UPDATE platform.jobs SET frozen_settings = '{\"a\":1}' WHERE id = $1", id)
			ledgerExpectOK(t, tx, "control: leaving draft", "UPDATE platform.jobs SET status = 'scheduled', scheduled_start = now() WHERE id = $1", id)
			frozen := ledgerWhy{Message: "frozen"}
			ledgerExpectRefused(t, tx, "changing frozen_settings of a scheduled job", "23001", frozen, "UPDATE platform.jobs SET frozen_settings = '{\"a\":2}' WHERE id = $1", id)
			ledgerExpectOK(t, tx, "control: writing the SAME settings back is not a change", "UPDATE platform.jobs SET frozen_settings = '{\"a\":1}' WHERE id = $1", id)
			ledgerExpectOK(t, tx, "control: the status itself moves on", "UPDATE platform.jobs SET status = 'running' WHERE id = $1", id)
			ledgerExpectRefused(t, tx, "changing frozen_settings of a running job", "23001", frozen, "UPDATE platform.jobs SET frozen_settings = '{}' WHERE id = $1", id)
		})
		t.Run("updated_at is owned by the trigger", func(t *testing.T) {
			ledgerExpectOK(t, tx, "an UPDATE that tries to set updated_at", "UPDATE platform.jobs SET updated_at = '2000-01-01T00:00:00Z', module_job_ref = 'r-x' WHERE id = $1", id)
			if !ledgerBool(t, tx, "SELECT updated_at = now() FROM platform.jobs WHERE id = $1", id) {
				t.Fatal("updated_at is not now() after an UPDATE: jobs_guard does not own it")
			}
		})
	})

	t.Run("who may touch the table: the app inserts and updates, nobody deletes, the break-glass role reads", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		ok(t, "control: platform_app INSERT", job(a, "m1"))
		ledgerExpectOK(t, tx, "control: platform_app UPDATE", "UPDATE platform.jobs SET module_job_ref = 'app-ref' WHERE id = $1", ledgerJobA1)
		why := ledgerWhy{Message: "permission denied for table jobs"}
		ledgerExpectRefused(t, tx, "platform_app DELETE on jobs", "42501", why, "DELETE FROM platform.jobs")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		if k := ledgerInt64(t, tx, "SELECT count(*) FROM platform.jobs"); k < 5 {
			t.Fatalf("platform_admin sees %d jobs", k)
		}
		ledgerExpectRefused(t, tx, "platform_admin UPDATE on jobs", "42501", why, "UPDATE platform.jobs SET status = 'failed'")
		sql, args := ledgerInsertInto(tbl, job(a, "m1"))
		ledgerExpectRefused(t, tx, "platform_admin INSERT on jobs", "42501", why, sql, args...)
	})
}

// TestConstraints_JobUnitPrices covers the price snapshot's own rules: one price per (job, unit), the price at least 1, the
// price row names the job's own module and a unit of that module. (Append-only is proven in TestLedger_AppendOnly_*.) It fails if
// a snapshot can hold a unit of another module, or two prices for one unit.
func TestConstraints_JobUnitPrices(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	job := ledgerJobA0 // an m1 job with no snapshot yet
	const tbl = "platform.job_unit_prices"
	row := func(module, unit string, cost int64) ledgerCols {
		return ledgerCols{"job_id": job, "module_id": module, "unit": unit, "cost": cost}
	}
	ledgerOKInsert(t, tx, "control: a price row of the job's own module", tbl, row("m1", "m1_own", 1))
	ledgerRefusedInsert(t, tx, "a second price for the same unit of the same job", "23505", ledgerWhy{Constraint: "job_unit_prices_pk"}, tbl, row("m1", "m1_own", 1))
	// since platform_0007 a BEFORE INSERT trigger refuses any price that differs from platform.unit_costs before the CHECK is reached, so the CHECK is proven alone with that trigger switched off (a
	// savepoint that is rolled back), and the trigger's own refusal of the same row is asserted beside it (TestCloseout_SnapshotPriceEqualsUnitCosts is the trigger's own test)
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerMust(t, sp, "ALTER TABLE platform.job_unit_prices DISABLE TRIGGER job_unit_prices_snapshot_equals_unit_costs")
		ledgerRefusedInsert(t, sp, "a price of 0 (the snapshot trigger switched off, so the CHECK is the rule proven)", "23514", ledgerWhy{Constraint: "job_unit_prices_cost_at_least_one"}, tbl, row("m1", "m1_cache", 0))
	})
	ledgerRefusedInsert(t, tx, "a price of 0 (the snapshot trigger answers first: the table says 1)", "23514", ledgerWhy{Constraint: "snapshot_equals_unit_costs"}, tbl, row("m1", "m1_cache", 0))
	ledgerRefusedInsert(t, tx, "a price row that says module m2 for an m1 job", "23503", ledgerWhy{Constraint: "job_unit_prices_job_fk"}, tbl, row("m2", "m2_accepted", 1))
	ledgerRefusedInsert(t, tx, "a unit of another module under the job's own module", "23503", ledgerWhy{Constraint: "job_unit_prices_unit_fk"}, tbl, row("m1", "m2_accepted", 1))
	ledgerRefusedInsert(t, tx, "a unit that does not exist", "23503", ledgerWhy{Constraint: "job_unit_prices_unit_fk"}, tbl, row("m1", "m1_bogus", 1))
	ledgerRefusedInsert(t, tx, "a price row of a job that does not exist", "23503", ledgerWhy{Constraint: "job_unit_prices_job_fk"}, tbl, row("m1", "m1_cache", 1).with("job_id", ledgerNoSuchID))
	ledgerOKInsert(t, tx, "control: the rest of the snapshot", tbl, row("m1", "m1_cache", 1))

	t.Run("who may write the snapshot: the app inserts, the break-glass role reads", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerOKInsert(t, tx, "control: platform_app INSERT", tbl, row("m1", "m1_vendor", 2))
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", job); n != 3 {
			t.Fatalf("platform_admin sees %d price rows of the job, want 3", n)
		}
		sql, args := ledgerInsertInto(tbl, row("m1", "m1_vendor", 2).with("job_id", ledgerJobA1b))
		ledgerExpectRefused(t, tx, "platform_admin INSERT on job_unit_prices", "42501", ledgerWhy{Message: "permission denied for table job_unit_prices"}, sql, args...)
	})
}

// TestConstraints_Balances covers the table the ledger trigger writes, with a superuser, which bypasses privileges but not
// constraints: per-user per-module primary key, balances only for metered modules (the generated metered column and the foreign
// key), and both CHECKs on direct writes. Who may write it is TestBalance_NoRoleWritesBalancesExceptTheOwner.
func TestConstraints_Balances(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA
	ins := "INSERT INTO platform.balances (user_id, module_id) VALUES ($1, $2)"
	ledgerExpectRefused(t, tx, "a balance for M3 (unmetered)", "23503", ledgerWhy{Constraint: "balances_module_is_metered_fk"}, ins, a, "m3")
	ledgerExpectRefused(t, tx, "a balance for a module that does not exist", "23503", ledgerWhy{Constraint: "balances_module_is_metered_fk"}, ins, a, "m9")
	ledgerExpectRefused(t, tx, "writing the generated metered column", "428C9", ledgerWhy{Message: `column "metered"`}, "INSERT INTO platform.balances (user_id, module_id, metered) VALUES ($1, 'm1', false)", a)
	ledgerExpectRefused(t, tx, "a negative available", "23514", ledgerWhy{Constraint: "balances_available_not_negative"}, "INSERT INTO platform.balances (user_id, module_id, available) VALUES ($1, 'm1', -1)", a)
	ledgerExpectRefused(t, tx, "a negative reserved", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative"}, "INSERT INTO platform.balances (user_id, module_id, reserved) VALUES ($1, 'm1', -1)", a)
	ledgerExpectRefused(t, tx, "a balance for a user that does not exist", "23503", ledgerWhy{Constraint: "balances_user_id_fkey"}, ins, ledgerNoSuchID, "m1")
	tag := ledgerExpectOK(t, tx, "control: a balance row of a metered module", ins, a, "m1")
	if tag.RowsAffected() != 1 {
		t.Fatalf("the control inserted %d rows", tag.RowsAffected())
	}
	ledgerExpectRefused(t, tx, "the same (user, module) twice", "23505", ledgerWhy{Constraint: "balances_pk"}, ins, a, "m1")
	if got := ledgerString(t, tx, "SELECT available::text || '|' || reserved::text || '|' || metered::text FROM platform.balances WHERE user_id = $1 AND module_id = 'm1'", a); got != "0|0|true" {
		t.Fatalf("a new balance row is %q, want 0|0|true", got)
	}
}

// TestConstraints_M3Entitlements covers platform.m3_entitlements. Since platform_0006 NO application role writes it (a trigger on the ledger kinds m3_enable, m3_disable and m3_expire is its only
// writer), so its own rules - an expiry date needs the entitlement to be enabled, one row per user, changed_by an admin, a real user - are the last wall UNDER that trigger, and are proven here as the
// SUPERUSER, whom no privilege stops. The other half is who may touch the table: both application roles read it and are refused INSERT, UPDATE, DELETE and TRUNCATE with 42501 (the paired control: the
// superuser's own statements work, and an entitlement written THROUGH THE LEDGER by each role appears). It fails if a disabled entitlement can carry a date (the expiry job would then re-disable an
// already-disabled user, or a date could outlive a disable), or if a role regains a write privilege.
func TestConstraints_M3Entitlements(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b, c := ledgerUserA, ledgerUserB, ledgerUserC
	const tbl = "platform.m3_entitlements"
	row := func(user string, enabled bool, kv ...any) ledgerCols {
		return ledgerCols{"user_id": user, "enabled": enabled}.with(kv...)
	}
	until := "2030-01-01T00:00:00Z"

	ledgerOKInsert(t, tx, "control: enabled until a date", tbl, row(a, true, "enabled_until", until, "changed_by", ledgerAdminA))
	ledgerOKInsert(t, tx, "control: disabled with no date", tbl, row(b, false))
	ledgerOKInsert(t, tx, "control: enabled for good", tbl, row(c, true))
	ledgerRefusedInsert(t, tx, "a disabled entitlement with an expiry date", "23514", ledgerWhy{Constraint: "m3_entitlements_until_needs_enabled"}, tbl, row(ledgerAdminB, false, "enabled_until", until))
	ledgerRefusedInsert(t, tx, "a second row for the same user", "23505", ledgerWhy{Constraint: "m3_entitlements_pkey"}, tbl, row(a, true))
	ledgerRefusedInsert(t, tx, "changed_by naming a user who is not an admin", "23503", ledgerWhy{Constraint: "m3_entitlements_changed_by_fkey"}, tbl, row(ledgerAdminB, true, "changed_by", b))
	ledgerRefusedInsert(t, tx, "an entitlement for a user that does not exist", "23503", ledgerWhy{Constraint: "m3_entitlements_user_id_fkey"}, tbl, row(ledgerNoSuchID, true))
	ledgerExpectRefused(t, tx, "disabling an entitlement that still has its date", "23514", ledgerWhy{Constraint: "m3_entitlements_until_needs_enabled"},
		"UPDATE platform.m3_entitlements SET enabled = false WHERE user_id = $1", a)
	ledgerExpectOK(t, tx, "control: disabling and clearing the date together", "UPDATE platform.m3_entitlements SET enabled = false, enabled_until = NULL WHERE user_id = $1", a)

	t.Run("who may touch the table: nobody but the ledger trigger; both roles read", func(t *testing.T) {
		type stmt struct {
			op, sql string
			args    []any
			prep    string // run first by the control: the delivery row (platform.m3_sync) restricts a delete of its entitlement, even for the superuser
		}
		stmts := []stmt{
			{"INSERT", "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)", []any{ledgerAdminB}, ""},
			{"UPDATE", "UPDATE platform.m3_entitlements SET enabled = true WHERE user_id = $1", []any{b}, ""},
			{"DELETE", "DELETE FROM platform.m3_entitlements WHERE user_id = $1", []any{b}, "DELETE FROM platform.m3_sync WHERE user_id = $1"},
			{"TRUNCATE", "TRUNCATE platform.m3_entitlements CASCADE", nil, ""},
		}
		// the control: the superuser's own statements work (each in a savepoint that is rolled back), so a refusal below is the role's, not a typo or a missing row
		for _, s := range stmts {
			s := s
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				if s.prep != "" {
					ledgerMust(t, sp, s.prep, s.args...)
				}
				tag := ledgerExpectOK(t, sp, "control: the superuser's "+s.op, s.sql, s.args...)
				if s.op != "TRUNCATE" && tag.RowsAffected() != 1 {
					t.Fatalf("control: the superuser's %s touched %d rows, want 1", s.op, tag.RowsAffected())
				}
			})
		}
		ledgerExpectRefused(t, tx, "even the superuser cannot delete an entitlement that has a delivery row", "23503", ledgerWhy{Constraint: "m3_sync_user_id_fkey"},
			"DELETE FROM platform.m3_entitlements WHERE user_id = $1", b)

		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_entitlements"); n != 3 {
				t.Fatalf("%s sees %d entitlements, want the 3 written above (SELECT is granted)", role, n)
			}
			why := ledgerWhy{Message: "permission denied for table m3_entitlements"}
			for _, s := range stmts {
				ledgerExpectRefused(t, tx, role+" "+s.op+" on m3_entitlements", "42501", why, s.sql, s.args...)
			}
			ledgerResetRole(t, tx)
		}

		// What the roles CAN do is put a row in the LEDGER: the entitlement appears through the trigger (and its delivery row with it), written by neither role.
		for _, w := range []struct{ role, user string }{{"platform_app", ledgerAdminB}, {"platform_admin", ledgerAdminA}} {
			ledgerSwitchRole(t, tx, w.role)
			ledgerOKRow(t, tx, "control: "+w.role+" enables an entitlement through the ledger", ledgerM3Cols("m3_enable", w.user))
			ledgerResetRole(t, tx)
			if got := ledgerString(t, tx, "SELECT enabled::text || '|' || COALESCE(changed_by::text, '') FROM platform.m3_entitlements WHERE user_id = $1", w.user); got != "true|"+ledgerAdminA {
				t.Fatalf("the entitlement %s's ledger row created reads %q, want true|%s", w.role, got, ledgerAdminA)
			}
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_sync WHERE user_id = $1", w.user); n != 1 {
				t.Fatalf("the entitlement of %s has %d delivery rows, want the 1 its trigger wrote", w.user, n)
			}
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_entitlements"); n != 5 {
			t.Fatalf("%d entitlements after the two ledger writes, want 3 + 2", n)
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// suppressions, audit
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_Suppressions covers platform.suppressions: the norm/v1 form the database can check (module 1's frozen address
// normalisation: trimmed, one @, lower-case, NFC, an ASCII domain without a leading or trailing dot or hyphen, at most 254
// bytes, NO dot-stripping and NO +tag folding), the source vocabulary, one row per (user, address), and the grants. Every
// refused form is refused by suppressions_email_norm_v1 specifically, and the accepted forms are the controls. It fails if the
// CHECK is weakened (any single accepted-by-mistake form below), or if a form that norm/v1 keeps (a +tag, dots) is refused.
func TestConstraints_Suppressions(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b := ledgerUserA, ledgerUserB
	const ins = "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)"
	const norm = "suppressions_email_norm_v1"
	long := func(n int) string { return strings.Repeat("a", n-len("@e.com")) + "@e.com" } // an address of exactly n characters
	// an address of exactly n bytes that module 1's syntax layer accepts (platform_0009): a 64-byte local part and a dotted domain of the rest (labels of 50 bytes, none empty)
	longOK := func(n int) string {
		left := n - 64 - 1
		var labels []string
		for left > 53 {
			labels = append(labels, strings.Repeat("b", 50))
			left -= 51
		}
		labels = append(labels, strings.Repeat("c", left))
		return strings.Repeat("a", 64) + "@" + strings.Join(labels, ".")
	}

	t.Run("accepted: the forms norm/v1 produces", func(t *testing.T) {
		for _, e := range []string{
			"a@example.com",
			"first.last+tag@example.co.uk", // dots and +tags are KEPT: norm/v1 never strips or folds them
			"a@xn--bcher-kva.example",      // the punycode of an IDN domain
			"\u00fc@example.com",           // a lower-case u-umlaut, precomposed (NFC)
			"a1@a-b.example", "a@123.example",
			longOK(254),                        // the boundary: exactly 254 bytes (a 64-byte local part and a dotted domain: the longest address module 1's syntax layer lets through under this limit)
			strings.Repeat("a", 64) + "@e.com", // the longest local part module 1's syntax layer accepts
			"a@a.b",                            // the smallest dotted domain
		} {
			e := e
			t.Run(fmt.Sprintf("%.40q", e), func(t *testing.T) { ledgerExpectOK(t, tx, "control: accepted form", ins, a, e, "unsubscribe") })
		}
	})

	t.Run("refused: every form the CHECK exists to stop", func(t *testing.T) {
		for _, c := range []struct{ why, email string }{
			{"a leading space", " a@example.com"}, {"a trailing space", "a@example.com "},
			{"an upper-case local part", "A@example.com"}, {"an upper-case domain", "a@Example.com"},
			{"a trailing dot on the domain", "a@example.com."}, {"two @ in a row", "a@@example.com"}, {"two @ apart", "a@b@c.com"},
			{"no @", "a.example.com"}, {"an empty local part", "@example.com"}, {"an empty domain", "a@"},
			{"a space in the local part", "a b@example.com"}, {"a tab in the local part", "a\tb@example.com"},
			{"a space in the domain", "a@exam ple.com"}, {"a trailing newline", "a@example.com\n"},
			{"a non-ASCII domain (it must be punycode)", "a@ex\u00e4mple.com"},
			{"a leading hyphen on the domain", "a@-example.com"}, {"a trailing hyphen on the domain", "a@example-"},
			{"a leading dot on the domain", "a@.example.com"},
			{"an underscore in the domain", "a@exam_ple.com"}, {"a bang in the domain", "a@exam!ple.com"},
			{"a decomposed (NFD) local part: u + combining diaeresis is not NFC", "u\u0308@example.com"},
			{"255 characters (one over)", long(255)}, {"256 characters", long(256)},
			{"an empty label in the domain (two dots)", "a@example..com"},
			{"a label that ends with a hyphen", "a@a-.example"}, {"a label that starts with a hyphen", "a@example.-com"},
			{"within 254 CHARACTERS but over 254 BYTES (two-byte letters in the local part)", strings.Repeat("ü", 130) + "@e.com"},
			// module 1's syntax layer and IDNA's hyphen rule (platform_0009)
			{"a local part of 65 bytes (module 1's layer allows 64)", strings.Repeat("a", 65) + "@example.com"},
			{"a local part of 33 two-byte letters (66 bytes)", strings.Repeat("ü", 33) + "@example.com"},
			{"a domain with no dot", "a@localhost"},
			{"a label with -- in its third and fourth bytes", "a@ab--cd.example"},
			{"the same label as the second one", "a@example.ab--cd"},
		} {
			c := c
			t.Run(c.why, func(t *testing.T) {
				ledgerExpectRefused(t, tx, c.why, "23514", ledgerWhy{Constraint: norm}, ins, b, c.email, "unsubscribe")
			})
		}
		ledgerExpectOK(t, tx, "control: the same user takes a normalised address", ins, b, "ok@example.com", "unsubscribe")
	})

	// Single-label domains (a@b, a@localhost) were ACCEPTED under the owner's ruling OD-11 (Docs/phase-2-spec.md Part A row 9) and are REFUSED since the Phase 3 close-out (OD-9, platform_0009): the platform's
	// address rule is norm/v1 plus module 1's syntax layer, which fails a domain with no dot, and suppressions store what that rule returns. The ruling is superseded, not forgotten: the refusal of an
	// empty label (a@b..c) is unchanged, and so is the acceptance of everything norm/v1 produces that the layer passes.
	t.Run("single-label domains are refused (OD-9 supersedes OD-11); a dotted one is accepted, an empty label is not", func(t *testing.T) {
		for _, c := range []struct{ why, email string }{
			{"a single-label domain (a@b)", "a@b"},
			{"a single-label domain (a@localhost)", "a@localhost"},
			{"an empty label after a single-letter one (a@b..c)", "a@b..c"},
			{"a single-label domain with a trailing dot", "a@b."},
		} {
			c := c
			t.Run(c.why, func(t *testing.T) {
				ledgerExpectRefused(t, tx, c.why, "23514", ledgerWhy{Constraint: norm}, ins, b, c.email, "manual")
			})
		}
		ledgerExpectOK(t, tx, "control: the smallest dotted domain", ins, a, "a@b.c", "manual")
		ledgerExpectOK(t, tx, "control: a punycode label may carry -- in its third and fourth bytes", ins, a, "a@xn--bcher-kva.example", "manual")
	})

	t.Run("the source vocabulary", func(t *testing.T) {
		for _, s := range []string{"unsubscribe", "manual", "import"} {
			ledgerExpectOK(t, tx, "control: source "+s, ins, a, "src-"+s+"@example.com", s)
		}
		ledgerExpectRefused(t, tx, "source whim", "23514", ledgerWhy{Constraint: "suppressions_source_vocabulary"}, ins, a, "src-whim@example.com", "whim")
	})

	t.Run("one row per (user, address)", func(t *testing.T) {
		ledgerExpectRefused(t, tx, "the same address twice for one user", "23505", ledgerWhy{Constraint: "suppressions_pk"}, ins, a, "a@example.com", "manual")
		ledgerExpectOK(t, tx, "control: the same address for another user", ins, b, "a@example.com", "manual")
		ledgerExpectRefused(t, tx, "a user that does not exist", "23503", ledgerWhy{Constraint: "suppressions_user_id_fkey"}, ins, ledgerNoSuchID, "x@example.com", "manual")
	})

	t.Run("who may touch the table: the app inserts and reads, nobody updates or deletes, the break-glass role reads", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "control: platform_app INSERT", ins, a, "app@example.com", "manual")
		why := ledgerWhy{Message: "permission denied for table suppressions"}
		ledgerExpectRefused(t, tx, "platform_app DELETE", "42501", why, "DELETE FROM platform.suppressions")
		ledgerExpectRefused(t, tx, "platform_app UPDATE", "42501", why, "UPDATE platform.suppressions SET source = 'manual'")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.suppressions"); n < 5 {
			t.Fatalf("platform_admin sees %d suppressions", n)
		}
		ledgerExpectRefused(t, tx, "platform_admin INSERT", "42501", why, ins, a, "adm@example.com", "manual")
	})
}

// TestConstraints_AdminAudit covers platform.admin_audit: the entry types, a content view always names a user, nothing blank,
// details an object, the actor a NAMED admin (a plain user is refused). (Append-only is TestLedger_AppendOnly_*.) It fails if
// a content view can be recorded without saying whose content, if a reason can be blank, or if a non-admin can be the actor.
func TestConstraints_AdminAudit(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	const tbl = "platform.admin_audit"
	row := func(kv ...any) ledgerCols {
		return ledgerCols{"admin_id": ledgerAdminA, "entry_type": "action", "action": "grant_credits", "target_type": "user", "target_id": "x", "reason": "welcome pack"}.with(kv...)
	}
	ok := func(t *testing.T, what string, c ledgerCols) { t.Helper(); ledgerOKInsert(t, tx, what, tbl, c) }
	refused := func(t *testing.T, what, state string, why ledgerWhy, c ledgerCols) {
		t.Helper()
		ledgerRefusedInsert(t, tx, what, state, why, tbl, c)
	}

	t.Run("the entry types", func(t *testing.T) {
		ok(t, "control: an admin action", row())
		ok(t, "control: an action about a user", row("target_user_id", ledgerUserA))
		ok(t, "control: a content view naming the user whose content was seen", row("entry_type", "content_view", "target_user_id", ledgerUserA, "details", `{"retention_days":7}`))
		refused(t, "an entry type that is not in the vocabulary", "23514", ledgerWhy{Constraint: "admin_audit_entry_type_vocabulary"}, row("entry_type", "peek"))
		refused(t, "a content view that names no user", "23514", ledgerWhy{Constraint: "admin_audit_content_view_names_a_user"}, row("entry_type", "content_view"))
	})
	t.Run("nothing blank", func(t *testing.T) {
		for _, r := range []string{"", "   "} {
			refused(t, fmt.Sprintf("reason %q", r), "23514", ledgerWhy{Constraint: "admin_audit_reason_not_blank"}, row("reason", r))
		}
		refused(t, "no reason (NULL)", "23502", ledgerWhy{Column: "reason"}, row("reason", nil))
		refused(t, "a blank action", "23514", ledgerWhy{Constraint: "admin_audit_action_not_blank"}, row("action", " "))
		refused(t, "a blank target type", "23514", ledgerWhy{Constraint: "admin_audit_target_not_blank"}, row("target_type", " "))
		refused(t, "an empty target id", "23514", ledgerWhy{Constraint: "admin_audit_target_not_blank"}, row("target_id", ""))
		for _, d := range []string{`[1]`, `"x"`, `null`} {
			refused(t, "details "+d, "23514", ledgerWhy{Constraint: "admin_audit_details_is_an_object"}, row("details", d))
		}
	})
	t.Run("the actor is a named admin and the target is a real user", func(t *testing.T) {
		refused(t, "a plain user as the actor", "23503", ledgerWhy{Constraint: "admin_audit_admin_id_fkey"}, row("admin_id", ledgerUserA))
		refused(t, "no actor (NULL)", "23502", ledgerWhy{Column: "admin_id"}, row("admin_id", nil))
		refused(t, "a target user that does not exist", "23503", ledgerWhy{Constraint: "admin_audit_target_user_id_fkey"}, row("target_user_id", ledgerNoSuchID))
	})
	t.Run("the app and the break-glass role both append", func(t *testing.T) {
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			ok(t, "control: "+role+" INSERT", row("action", "as-"+role))
			ledgerResetRole(t, tx)
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// job_items
// ---------------------------------------------------------------------------------------------------------------------

// TestConstraints_JobItems covers public.job_items, whose shape is fixed by what modules 2 and 3 do with it: the six-column
// INSERT module 3 issues works (campaign_id and user_id bound to ONE parameter), the five NOT NULL columns refuse NULL with the
// column named, status defaults to 'pending', campaign_id has NO foreign key (a random uuid is accepted), nothing else is
// NOT NULL without a default, there is no trigger and no row-level security, and no role may delete. It fails if any
// of these drifts, which would break a module at runtime (a NULL surfaces there as an opaque 500).
func TestConstraints_JobItems(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA

	t.Run("the six-column INSERT that module 3 issues", func(t *testing.T) {
		var id string
		if err := tx.QueryRow(testCtx(t), `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
			VALUES ($1,$1,$2,$3,$4,'pending') RETURNING id::text`, a, "r@example.com", "subject", "body").Scan(&id); err != nil {
			t.Fatalf("module 3's own INSERT is refused: %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("RETURNING id gave %q, want a uuid assigned by the default", id)
		}
		if got := ledgerString(t, tx, "SELECT status || '|' || COALESCE(reply_to, '<null>') || '|' || (created_at IS NOT NULL)::text FROM public.job_items WHERE id = $1::uuid", id); got != "pending|<null>|true" {
			t.Fatalf("the row module 3 wrote reads %q, want pending|<null>|true", got)
		}
	})

	t.Run("the five NOT NULL columns, and the defaults", func(t *testing.T) {
		const insert = `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $2, $3, $4, $5)`
		vals := []any{ledgerUserB, ledgerUserB, "x@example.com", "s", "b"}
		ledgerExpectOK(t, tx, "control: every column given", insert, vals...)
		for i, col := range []string{"campaign_id", "user_id", "recipient_email", "rendered_subject", "rendered_body"} {
			with := append([]any{}, vals...)
			with[i] = nil
			ledgerExpectRefused(t, tx, "a NULL "+col, "23502", ledgerWhy{Column: col}, insert, with...)
		}
		ledgerExpectRefused(t, tx, "a NULL status", "23502", ledgerWhy{Column: "status"},
			"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1, $1, 'x@example.com', 's', 'b', NULL)", a)
		ledgerExpectRefused(t, tx, "a blank status", "23514", ledgerWhy{Constraint: "job_items_status_not_blank"},
			"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1, $1, 'x@example.com', 's', 'b', '  ')", a)
		if got := ledgerString(t, tx, "SELECT status FROM public.job_items WHERE recipient_email = 'x@example.com'"); got != "pending" {
			t.Fatalf("status without a value is %q, want the default pending", got)
		}
	})

	t.Run("campaign_id has no foreign key: module 3's test-send writes the tenant's own id there", func(t *testing.T) {
		ledgerExpectOK(t, tx, "a random uuid as campaign_id", "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES (gen_random_uuid(), $1, 'c@example.com', 's', 'b')", a)
		ledgerExpectOK(t, tx, "the user's own id as campaign_id", "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'd@example.com', 's', 'b')", a)
		fks := ledgerRows(t, tx, `SELECT a.attname FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
			WHERE c.conrelid = 'public.job_items'::regclass AND c.contype = 'f' ORDER BY 1`)
		if len(fks) != 1 || fks[0] != "user_id" {
			t.Fatalf("job_items has foreign keys on %v, want exactly user_id", fks)
		}
		ledgerExpectRefused(t, tx, "a user_id that is not a user", "23503", ledgerWhy{Constraint: "job_items_user_id_fkey"},
			"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'e@example.com', 's', 'b')", ledgerNoSuchID)
		// user C has no job, no ledger row and no balance, so the job item is the only thing that can be refusing the delete
		ledgerExpectOK(t, tx, "control: a job item of user C", "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'cc@example.com', 's', 'b')", ledgerUserC)
		ledgerExpectRefused(t, tx, "deleting a user who has job items (module foreign keys cascade from users, so this must be a hard stop)", "23503", ledgerWhy{Constraint: "job_items_user_id_fkey"},
			"DELETE FROM public.users WHERE id = $1", ledgerUserC)
	})

	t.Run("every column module 3 does not write is nullable or defaulted", func(t *testing.T) {
		got := ledgerRows(t, tx, `SELECT attname FROM pg_attribute WHERE attrelid = 'public.job_items'::regclass AND attnum > 0 AND NOT attisdropped
			AND attnotnull AND NOT atthasdef AND attname NOT IN ('campaign_id', 'user_id', 'recipient_email', 'rendered_subject', 'rendered_body') ORDER BY 1`)
		if len(got) != 0 {
			t.Fatalf("job_items has NOT NULL columns without a default that module 3's INSERT does not supply: %v", got)
		}
	})

	t.Run("nothing may change what the modules can do: no trigger, no row-level security, no policy", func(t *testing.T) {
		query := func(tables string) []string {
			return ledgerRows(t, tx, fmt.Sprintf(`SELECT c.relname || ':' || c.relrowsecurity::text || ':' || c.relforcerowsecurity::text || ':' ||
				(SELECT count(*) FROM pg_trigger g WHERE g.tgrelid = c.oid AND NOT g.tgisinternal)::text || ':' ||
				(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)::text FROM pg_class c WHERE c.oid = ANY (ARRAY[%s]::regclass[]) ORDER BY 1`, tables))
		}
		got := query("'public.users', 'public.job_items'")
		want := []string{"job_items:false:false:0:0", "users:false:false:0:0"}
		if strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("relname:rls:forcerls:triggers:policies of the two public tables = %v, want %v", got, want)
		}
		// Control: the same query sees a trigger and row-level security when they exist, so "0" above is a measurement.
		if got := query("'platform.ledger'"); len(got) != 1 || !strings.HasPrefix(got[0], "ledger:false:false:") || strings.HasSuffix(got[0], ":0:0") {
			t.Fatalf("control: the query reports %v for platform.ledger, which has triggers", got)
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE public.job_items ENABLE ROW LEVEL SECURITY")
			if got := ledgerRows(t, sp, "SELECT relrowsecurity::text FROM pg_class WHERE oid = 'public.job_items'::regclass"); len(got) != 1 || got[0] != "true" {
				t.Fatalf("control: the catalog does not show row-level security after enabling it: %v", got)
			}
		})
	})

	t.Run("who may touch the table: the app inserts and updates, nobody deletes, the break-glass role reads", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "control: platform_app INSERT", "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'app@example.com', 's', 'b')", a)
		ledgerExpectOK(t, tx, "control: platform_app UPDATE", "UPDATE public.job_items SET status = 'sent' WHERE recipient_email = 'app@example.com'")
		why := ledgerWhy{Message: "permission denied for table job_items"}
		ledgerExpectRefused(t, tx, "platform_app DELETE", "42501", why, "DELETE FROM public.job_items")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM public.job_items"); n < 3 {
			t.Fatalf("platform_admin sees %d job items", n)
		}
		ledgerExpectRefused(t, tx, "platform_admin INSERT", "42501", why, "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'adm@example.com', 's', 'b')", a)
		ledgerExpectRefused(t, tx, "platform_admin UPDATE", "42501", why, "UPDATE public.job_items SET status = 'x'")
	})
}
