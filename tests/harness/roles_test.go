//go:build harness

package harness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The role proofs (Phase 1, chunk B5). TestGrantsInvariant says what the CATALOGUE grants; these tests say what a session running as each role can and cannot DO, one statement per
// privilege of every table and view (and of the platform's two tables in schema public), run as the role with SET LOCAL ROLE in a transaction that is rolled back.
//
// Why each can fail (the paired controls are in the tests themselves):
//   - a refused statement is first run as the SUPERUSER, where it must not be refused for privilege: it either succeeds or fails for a named data-level reason (the append-only trigger,
//     a foreign key, "cannot insert into view"). So a refusal as the role is the role's, not a typo or a missing table;
//   - the switch is checked to have happened (current_user is the role, session_user is not, the role is not a superuser);
//   - every table has rows, so a SELECT is a read of data and an UPDATE or DELETE has a row to hit (the append-only triggers only fire on a row);
//   - every refusal asserts SQLSTATE 42501 AND the reason ("permission denied for table <name>"), never only "an error";
//   - the expectations below are written separately from declaredClasses; TestRoles_ExpectationsAgreeWithDeclaredClasses makes the two agree and is itself proven able to fail.

// roleStmt is one statement of the catalogue: what to run to exercise one privilege of one relation.
//
//	Rel   the relation, schema-qualified
//	Priv  SELECT, INSERT, UPDATE, DELETE or TRUNCATE (table level), or UPDATE(col,col) (column level)
//	SQL   the statement; {U1} {U2} {U3} {ADM} {ADM2} {JOB} {JOB2} are the seeded identities (rolesExpand)
//	Ctrl  what the SUPERUSER gets from it: "" = it succeeds, else the SQLSTATE of the data-level error (never 42501)
//	Why   a substring of that error's message (when Ctrl is set)
//	Only  column-level statements only: the roles that may run it ("" = nobody)
type roleStmt struct{ Rel, Priv, SQL, Ctrl, Why, Only string }

// roleStatements holds one statement per (relation, privilege). The append-only tables raise 23001 for the superuser (their triggers guard rows even from the owner), a few tables are
// referenced by foreign keys (23503), and a view can be written by nobody (55000): in each case the superuser's result is stated so the control is exact.
var roleStatements = []roleStmt{
	{"platform.schema_migrations", "INSERT", `INSERT INTO platform.schema_migrations (version) VALUES ('zz-denied')`, "", "", ""},
	{"platform.schema_migrations", "UPDATE", `UPDATE platform.schema_migrations SET version = version`, "", "", ""},
	{"platform.schema_migrations", "DELETE", `DELETE FROM platform.schema_migrations`, "", "", ""},
	{"platform.schema_migrations", "TRUNCATE", `TRUNCATE platform.schema_migrations`, "", "", ""},

	{"platform.admins", "INSERT", `INSERT INTO platform.admins (id, display_name) VALUES ({U2}, 'Roles Proof Admin')`, "", "", ""},
	{"platform.admins", "UPDATE", `UPDATE platform.admins SET display_name = display_name`, "", "", ""},
	{"platform.admins", "DELETE", `DELETE FROM platform.admins WHERE id = {ADM2}`, "", "", ""},
	{"platform.admins", "TRUNCATE", `TRUNCATE platform.admins CASCADE`, "23001", "append-only", ""},

	{"platform.modules", "INSERT", `INSERT INTO platform.modules (id, name, schema_name, metered) VALUES ('m9', 'proof', 'proof_schema', true)`, "", "", ""},
	{"platform.modules", "UPDATE", `UPDATE platform.modules SET active = active`, "", "", ""},
	{"platform.modules", "DELETE", `DELETE FROM platform.modules WHERE id = 'm3'`, "", "", ""},
	{"platform.modules", "TRUNCATE", `TRUNCATE platform.modules CASCADE`, "23001", "append-only", ""},

	{"platform.unit_costs", "INSERT", `INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES ('m1_proof', 'm1', 'roles proof', 1)`, "", "", ""},
	{"platform.unit_costs", "UPDATE", `UPDATE platform.unit_costs SET description = description`, "", "", ""},
	{"platform.unit_costs", "DELETE", `DELETE FROM platform.unit_costs WHERE unit = 'm2_bounced'`, "23503", "unit_cost_history", ""},
	{"platform.unit_costs", "TRUNCATE", `TRUNCATE platform.unit_costs CASCADE`, "23001", "append-only", ""},
	// column level: platform_app may change the price (with a named admin and a reason) and nothing else on the row; a statement that also touches description is refused
	{"platform.unit_costs", "UPDATE(cost,changed_by,change_reason)", `UPDATE platform.unit_costs SET cost = cost + 1, changed_by = {ADM}, change_reason = 'roles proof' WHERE unit = 'm2_accepted'`, "", "", "platform_app"},
	{"platform.unit_costs", "UPDATE(cost,changed_by,change_reason,description)", `UPDATE platform.unit_costs SET cost = cost + 1, changed_by = {ADM}, change_reason = 'roles proof', description = 'sneaky' WHERE unit = 'm2_accepted'`, "", "", ""},

	{"platform.unit_cost_history", "INSERT", `INSERT INTO platform.unit_cost_history (unit, new_cost) VALUES ('m1_own', 1)`, "", "", ""},
	{"platform.unit_cost_history", "UPDATE", `UPDATE platform.unit_cost_history SET change_reason = 'edited'`, "23001", "append-only", ""},
	{"platform.unit_cost_history", "DELETE", `DELETE FROM platform.unit_cost_history`, "23001", "append-only", ""},
	{"platform.unit_cost_history", "TRUNCATE", `TRUNCATE platform.unit_cost_history`, "23001", "append-only", ""},

	{"platform.jobs", "INSERT", `INSERT INTO platform.jobs (user_id, module_id) VALUES ({U2}, 'm1')`, "", "", ""},
	// DELIBERATE CHANGE (platform_0011): the statement used to move the job to scheduled; a change of status now needs an actor and a reason and a row of platform.job_transitions, so a bare status write is
	// refused for every role (the privilege proof of the move is TestJobLifecycle_ThePrivilegesOfTheLifecycle). What this statement proves is the table-level UPDATE privilege, on an annotation.
	{"platform.jobs", "UPDATE", `UPDATE platform.jobs SET scheduled_start = now() WHERE id = {JOB}`, "", "", ""},
	{"platform.jobs", "DELETE", `DELETE FROM platform.jobs WHERE id = {JOB2}`, "", "", ""},
	{"platform.jobs", "TRUNCATE", `TRUNCATE platform.jobs CASCADE`, "23001", "append-only", ""},

	// platform_0011: the lifecycle table (insert-only for the owner and nobody else: the superuser's INSERT succeeds, the roles hold SELECT only) and the history of every job (platform_app inserts, nobody
	// updates or deletes: the superuser's UPDATE, DELETE and TRUNCATE meet the append-only triggers)
	{"platform.job_transitions", "INSERT", `INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES ('previewed', 'running', ARRAY['engine'], 'roles proof')`, "", "", ""},
	{"platform.job_transitions", "UPDATE", `UPDATE platform.job_transitions SET note = 'edited'`, "23001", "append-only", ""},
	{"platform.job_transitions", "DELETE", `DELETE FROM platform.job_transitions`, "23001", "append-only", ""},
	{"platform.job_transitions", "TRUNCATE", `TRUNCATE platform.job_transitions`, "23001", "append-only", ""},
	{"platform.job_events", "INSERT", `INSERT INTO platform.job_events (job_id, kind, actor_kind, reason) VALUES ({JOB}, 'note', 'engine', 'roles proof')`, "", "", ""},
	{"platform.job_events", "UPDATE", `UPDATE platform.job_events SET reason = 'edited'`, "23001", "append-only", ""},
	{"platform.job_events", "DELETE", `DELETE FROM platform.job_events`, "23001", "append-only", ""},
	{"platform.job_events", "TRUNCATE", `TRUNCATE platform.job_events`, "23001", "append-only", ""},

	{"platform.job_unit_prices", "INSERT", `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) VALUES ({JOB2}, 'm1', 'm1_own', 1)`, "", "", ""},
	{"platform.job_unit_prices", "UPDATE", `UPDATE platform.job_unit_prices SET cost = cost`, "23001", "append-only", ""},
	{"platform.job_unit_prices", "DELETE", `DELETE FROM platform.job_unit_prices`, "23001", "append-only", ""},
	{"platform.job_unit_prices", "TRUNCATE", `TRUNCATE platform.job_unit_prices CASCADE`, "23001", "append-only", ""},

	// platform_0012: a job's list (platform_app inserts and deletes rows and updates the eight columns the engine and the preview write, never id, job_id, user_id, ordinal or created_at: the table-level UPDATE
	// statement names ordinal, so it is the column-level statement that proves what platform_app may do) and its results (insert-only for everybody: the superuser's UPDATE, DELETE and TRUNCATE meet the append-only
	// triggers; TRUNCATE of the rows cascades into the results, whose trigger answers); the proof view cannot be written by anybody
	{"platform.job_rows", "INSERT", `INSERT INTO platform.job_rows (job_id, user_id, ordinal, email_raw, email_norm) VALUES ({JOB}, {U1}, 2, 'proof@example.com', 'proof@example.com')`, "", "", ""},
	{"platform.job_rows", "UPDATE", `UPDATE platform.job_rows SET ordinal = ordinal WHERE id = {ROW1}`, "", "", ""},
	{"platform.job_rows", "DELETE", `DELETE FROM platform.job_rows WHERE id = {ROW1}`, "", "", ""},
	{"platform.job_rows", "TRUNCATE", `TRUNCATE platform.job_rows CASCADE`, "23001", "append-only", ""},
	{"platform.job_rows", "UPDATE(email_raw,email_norm,state,reason,duplicate_of,fields,source_item,submitted_at,suppressed)", `UPDATE platform.job_rows SET email_raw = email_raw, email_norm = email_norm, state = state, reason = reason, duplicate_of = duplicate_of, fields = fields, source_item = source_item, submitted_at = submitted_at, suppressed = suppressed WHERE id = {ROW1}`, "", "", "platform_app"},
	{"platform.job_results", "INSERT", `INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule) VALUES ({JOB3}, {U1}, {ROW3B}, 2, 'b@example.com', 'module', 'sent', 'free', 'roles proof')`, "", "", ""},
	{"platform.job_results", "UPDATE", `UPDATE platform.job_results SET rule = 'edited'`, "23001", "append-only", ""},
	{"platform.job_results", "DELETE", `DELETE FROM platform.job_results`, "23001", "append-only", ""},
	{"platform.job_results", "TRUNCATE", `TRUNCATE platform.job_results`, "23001", "append-only", ""},
	{"platform.job_settlement_drift", "INSERT", `INSERT INTO platform.job_settlement_drift DEFAULT VALUES`, "55000", "cannot insert into view", ""},
	{"platform.job_settlement_drift", "UPDATE", `UPDATE platform.job_settlement_drift SET job_id = job_id`, "55000", "cannot update view", ""},
	{"platform.job_settlement_drift", "DELETE", `DELETE FROM platform.job_settlement_drift`, "55000", "cannot delete from view", ""},

	// platform_0013: every change of a unit's reachable flag, written by trigger (the flag itself is written only by platform.set_unit_reachable)
	{"platform.unit_reachability_history", "INSERT", `INSERT INTO platform.unit_reachability_history (unit, reachable, source) VALUES ('m1_own', true, 'roles proof')`, "", "", ""},
	{"platform.unit_reachability_history", "UPDATE", `UPDATE platform.unit_reachability_history SET source = 'edited'`, "23001", "append-only", ""},
	{"platform.unit_reachability_history", "DELETE", `DELETE FROM platform.unit_reachability_history`, "23001", "append-only", ""},
	{"platform.unit_reachability_history", "TRUNCATE", `TRUNCATE platform.unit_reachability_history`, "23001", "append-only", ""},

	// platform_0020: the per-user settings (platform_app inserts a row and updates its value and its time, never the user; nobody deletes: the superuser's DELETE finds no row to meet and its TRUNCATE meets the refusing
	// trigger) and their history (no role writes it: the trigger of the settings does; the superuser's UPDATE and DELETE find no row here, its TRUNCATE meets the append-only trigger; tests/harness/usersettings_test.go
	// proves the row triggers against rows)
	{"platform.user_settings", "INSERT", `INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ({U2}, true)`, "", "", ""},
	{"platform.user_settings", "UPDATE", `UPDATE platform.user_settings SET user_id = user_id`, "", "", ""},
	{"platform.user_settings", "DELETE", `DELETE FROM platform.user_settings`, "23001", "append-only", ""},
	{"platform.user_settings", "TRUNCATE", `TRUNCATE platform.user_settings`, "23001", "append-only", ""},
	{"platform.user_settings", "UPDATE(vendor_fallback_opt_out,updated_at)", `UPDATE platform.user_settings SET vendor_fallback_opt_out = vendor_fallback_opt_out, updated_at = updated_at`, "", "", "platform_app"},
	{"platform.user_settings_history", "INSERT", `INSERT INTO platform.user_settings_history (user_id, setting, old_value, new_value, actor_user_id) VALUES ({U2}, 'vendor_fallback_opt_out', false, true, {U2})`, "", "", ""},
	{"platform.user_settings_history", "UPDATE", `UPDATE platform.user_settings_history SET new_value = new_value`, "23001", "append-only", ""},
	{"platform.user_settings_history", "DELETE", `DELETE FROM platform.user_settings_history`, "23001", "append-only", ""},
	{"platform.user_settings_history", "TRUNCATE", `TRUNCATE platform.user_settings_history`, "23001", "append-only", ""},

	{"platform.balances", "INSERT", `INSERT INTO platform.balances (user_id, module_id) VALUES ({U2}, 'm1')`, "", "", ""},
	{"platform.balances", "UPDATE", `UPDATE platform.balances SET available = available + 1`, "", "", ""},
	{"platform.balances", "DELETE", `DELETE FROM platform.balances`, "", "", ""},
	{"platform.balances", "TRUNCATE", `TRUNCATE platform.balances`, "", "", ""},

	{"platform.ledger", "INSERT", `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key) VALUES ({U1}, 'm1', 'grant', 7, {ADM}, 'roles proof', 'roles-proof-insert')`, "", "", ""},
	{"platform.ledger", "UPDATE", `UPDATE platform.ledger SET reason = 'tampered'`, "23001", "append-only", ""},
	{"platform.ledger", "DELETE", `DELETE FROM platform.ledger`, "23001", "append-only", ""},
	{"platform.ledger", "TRUNCATE", `TRUNCATE platform.ledger CASCADE`, "23001", "append-only", ""},

	// platform_0006: nobody but the ledger trigger writes the entitlements, so every write is refused to both roles (the superuser's result is stated for each: its DELETE is stopped by the
	// delivery row that references the entitlement, and its TRUNCATE needs CASCADE for the same reason)
	{"platform.m3_entitlements", "INSERT", `INSERT INTO platform.m3_entitlements (user_id, enabled, changed_by) VALUES ({U2}, true, {ADM})`, "", "", ""},
	{"platform.m3_entitlements", "UPDATE", `UPDATE platform.m3_entitlements SET enabled = false, enabled_until = NULL, changed_by = {ADM}, changed_at = now() WHERE user_id = {U1}`, "", "", ""},
	{"platform.m3_entitlements", "DELETE", `DELETE FROM platform.m3_entitlements`, "23503", "m3_sync", ""},
	{"platform.m3_entitlements", "TRUNCATE", `TRUNCATE platform.m3_entitlements CASCADE`, "", "", ""},

	// platform_0005: what each reserve held, charged and released. Written only by the ledger trigger: both roles read it and nothing else.
	{"platform.reserves", "INSERT", `INSERT INTO platform.reserves (reserve_id, user_id, module_id, job_id, reserved) SELECT id, {U1}, 'm1', {JOB2}, 5 FROM platform.ledger WHERE idempotency_key = 'roles-seed-grant'`, "", "", ""},
	{"platform.reserves", "UPDATE", `UPDATE platform.reserves SET charged = charged`, "", "", ""},
	{"platform.reserves", "DELETE", `DELETE FROM platform.reserves`, "", "", ""},
	{"platform.reserves", "TRUNCATE", `TRUNCATE platform.reserves`, "", "", ""},

	// platform_0006: delivery of each entitlement to module 3. Both roles read it; platform_app may update exactly the nine columns the reconcile loop records (seven since platform_0006, observed_version and m3_version since platform_0008; the column-level statements
	// below); nobody inserts or deletes a row (the entitlement trigger inserts it). The superuser's INSERT meets the row the trigger already wrote for the seeded entitlement.
	{"platform.m3_sync", "INSERT", `INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES ({U1}, false)`, "23505", "m3_sync_pkey", ""},
	{"platform.m3_sync", "UPDATE", `UPDATE platform.m3_sync SET want_version = want_version + 1`, "", "", ""},
	{"platform.m3_sync", "DELETE", `DELETE FROM platform.m3_sync`, "", "", ""},
	{"platform.m3_sync", "TRUNCATE", `TRUNCATE platform.m3_sync`, "", "", ""},
	{"platform.m3_sync", "UPDATE(observed,observed_at,attempts,last_attempt_at,last_error,converged_version,updated_at,observed_version,m3_version)", `UPDATE platform.m3_sync SET observed = 'active', observed_at = now(), attempts = attempts + 1, last_attempt_at = now(), last_error = NULL, converged_version = converged_version, updated_at = now(), observed_version = observed_version, m3_version = 3 WHERE user_id = {U1}`, "", "", "platform_app"},
	{"platform.m3_sync", "UPDATE(want_suspended,want_version)", `UPDATE platform.m3_sync SET want_suspended = NOT want_suspended, want_version = want_version + 1 WHERE user_id = {U1}`, "", "", ""},

	{"platform.suppressions", "INSERT", `INSERT INTO platform.suppressions (user_id, email, source) VALUES ({U1}, 'proof@example.com', 'manual')`, "", "", ""},
	{"platform.suppressions", "UPDATE", `UPDATE platform.suppressions SET source = 'import'`, "", "", ""},
	{"platform.suppressions", "DELETE", `DELETE FROM platform.suppressions`, "", "", ""},
	{"platform.suppressions", "TRUNCATE", `TRUNCATE platform.suppressions`, "", "", ""},

	{"platform.admin_audit", "INSERT", `INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, reason) VALUES ({ADM}, 'action', 'roles-proof', 'user', 'x', 'proof')`, "", "", ""},
	{"platform.admin_audit", "UPDATE", `UPDATE platform.admin_audit SET reason = 'edited'`, "23001", "append-only", ""},
	{"platform.admin_audit", "DELETE", `DELETE FROM platform.admin_audit`, "23001", "append-only", ""},
	{"platform.admin_audit", "TRUNCATE", `TRUNCATE platform.admin_audit`, "23001", "append-only", ""},

	// the views: nobody can write them, whatever they hold (they group and join); the real privilege proof for a view is its SELECT, below and in TestRoles_UngrantedRoleIsRefusedEverywhere
	{"platform.balance_drift", "INSERT", `INSERT INTO platform.balance_drift DEFAULT VALUES`, "55000", "cannot insert into view", ""},
	{"platform.balance_drift", "UPDATE", `UPDATE platform.balance_drift SET user_id = user_id`, "55000", "cannot update view", ""},
	{"platform.balance_drift", "DELETE", `DELETE FROM platform.balance_drift`, "55000", "cannot delete from view", ""},
	{"platform.reserve_overdrawn", "INSERT", `INSERT INTO platform.reserve_overdrawn DEFAULT VALUES`, "55000", "cannot insert into view", ""},
	{"platform.reserve_overdrawn", "UPDATE", `UPDATE platform.reserve_overdrawn SET user_id = user_id`, "55000", "cannot update view", ""},
	{"platform.reserve_overdrawn", "DELETE", `DELETE FROM platform.reserve_overdrawn`, "55000", "cannot delete from view", ""},
	// platform_0004's user view is different: it selects one table without a join or an aggregate, so PostgreSQL makes it AUTO-UPDATABLE and the superuser's writes through it SUCCEED (Ctrl ""
	// - roleWritableViews). What refuses the roles is therefore the privilege alone ("permission denied for view listable_users"), which is exactly what the grants must guarantee: a write
	// privilege on this view would be a way to write public.users.
	{"platform.listable_users", "INSERT", `INSERT INTO platform.listable_users (id, auth_user_id, email) VALUES ({U3}, {U3}, 'roles-u3-view@example.test')`, "", "", ""},
	{"platform.listable_users", "UPDATE", `UPDATE platform.listable_users SET is_active = is_active WHERE id = {U2}`, "", "", ""},
	{"platform.listable_users", "DELETE", `DELETE FROM platform.listable_users WHERE id = {U2}`, "", "", ""},

	{"public.users", "INSERT", `INSERT INTO public.users (id, auth_user_id, email) VALUES ({U3}, {U3}, 'roles-u3@example.test')`, "", "", ""},
	{"public.users", "UPDATE", `UPDATE public.users SET is_active = is_active WHERE id = {U2}`, "", "", ""},
	{"public.users", "DELETE", `DELETE FROM public.users WHERE id = {U2}`, "", "", ""},
	{"public.users", "TRUNCATE", `TRUNCATE public.users CASCADE`, "23001", "append-only", ""},

	{"public.job_items", "INSERT", `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ({U1}, {U1}, 'proof@example.com', 'subject', 'body')`, "", "", ""},
	{"public.job_items", "UPDATE", `UPDATE public.job_items SET status = 'pending'`, "", "", ""},
	{"public.job_items", "DELETE", `DELETE FROM public.job_items`, "", "", ""},
	{"public.job_items", "TRUNCATE", `TRUNCATE public.job_items`, "", "", ""},
}

// roleExpectations is what each role must be able to do, per relation, as table-level privilege letters: S SELECT, I INSERT, U UPDATE, D DELETE, T TRUNCATE. It is written by hand and
// separately from declaredClasses (which the catalogue check uses): the two must agree (TestRoles_ExpectationsAgreeWithDeclaredClasses), and this one is what the sessions below are
// held to. Nobody holds D or T anywhere: the ledger and the other logs are append-only, and module foreign keys cascade from the two public tables.
var roleExpectations = map[string]map[string]string{
	"platform_app": {
		"platform.schema_migrations": "S", "platform.admins": "S", "platform.modules": "S", "platform.unit_costs": "S", "platform.unit_cost_history": "S",
		"platform.jobs": "SIU", "platform.job_unit_prices": "SI", "platform.balances": "S", "platform.ledger": "SI", "platform.m3_entitlements": "S",
		"platform.suppressions": "SI", "platform.admin_audit": "SI", "platform.balance_drift": "S", "platform.reserve_overdrawn": "S", "platform.listable_users": "S",
		"platform.reserves": "S", "platform.m3_sync": "S", // m3_sync: plus UPDATE of nine columns (the column-level statement), which a table-level letter cannot say
		"platform.job_transitions": "S", "platform.job_events": "SI",
		"platform.job_rows": "SID", "platform.job_results": "SI", "platform.job_settlement_drift": "S", "platform.unit_reachability_history": "S", "platform.user_settings": "SI", "platform.user_settings_history": "S", // user_settings: plus UPDATE of two columns (the column-level statement); job_rows: plus UPDATE of eight columns (the column-level statement)
		"public.users": "SIU", "public.job_items": "SIU",
	},
	"platform_admin": {
		"platform.schema_migrations": "S", "platform.admins": "S", "platform.modules": "S", "platform.unit_costs": "S", "platform.unit_cost_history": "S",
		"platform.jobs": "S", "platform.job_unit_prices": "S", "platform.balances": "S", "platform.ledger": "SI", "platform.m3_entitlements": "S",
		"platform.suppressions": "S", "platform.admin_audit": "SI", "platform.balance_drift": "S", "platform.reserve_overdrawn": "S", "platform.listable_users": "S",
		"platform.reserves": "S", "platform.m3_sync": "S",
		"platform.job_transitions": "S", "platform.job_events": "S",
		"platform.job_rows": "S", "platform.job_results": "S", "platform.job_settlement_drift": "S", "platform.unit_reachability_history": "S", "platform.user_settings": "S", "platform.user_settings_history": "S",
		"public.users": "S", "public.job_items": "S",
	},
}

// roleViews are the relations that are views (the proof checks the catalogue agrees).
var roleViews = map[string]bool{"platform.balance_drift": true, "platform.reserve_overdrawn": true, "platform.listable_users": true, "platform.job_settlement_drift": true}

// roleWritableViews are the views a SUPERUSER can write through (an auto-updatable view: one table, no join, no aggregate). A role is refused them by privilege exactly like a table
// ("permission denied for view <name>"); the other views are refused by the rewriter whoever runs the statement (55000, the Ctrl of their statements).
var roleWritableViews = map[string]bool{"platform.listable_users": true}

var rolePrivWords = map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE", 'T': "TRUNCATE"}

// roleCase is one cell of the matrix: one statement, and whether the role must be able to run it.
type roleCase struct {
	Role    string
	Stmt    roleStmt
	Allowed bool
}

func (c roleCase) name() string {
	verdict := "refused"
	if c.Allowed {
		verdict = "allowed"
	}
	return c.Stmt.Rel + " " + c.Stmt.Priv + " " + verdict
}

// rolesPrivsFor lists the table-level privileges the matrix covers for a relation. TRUNCATE of a view is a wrong-object-type error whichever role runs it, so it is left out for views.
func rolesPrivsFor(rel string) []string {
	if roleViews[rel] {
		return []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	}
	return []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"}
}

// rolesMatrix builds the cases for one role from the expectations and the statements, and returns an error naming anything incomplete: a relation without a statement for one of its
// privileges, a statement for a relation nobody expects anything of, a duplicate, a malformed letter. Every relation gets at least one allowed and, unless the role holds every privilege, one
// refused case.
func rolesMatrix(role string) ([]roleCase, error) {
	exp, ok := roleExpectations[role]
	if !ok || len(exp) == 0 {
		return nil, fmt.Errorf("no expectations for %s", role)
	}
	byKey := map[string]roleStmt{}
	for _, s := range roleStatements {
		k := s.Rel + "\x00" + s.Priv
		if _, dup := byKey[k]; dup {
			return nil, fmt.Errorf("two statements for %s %s", s.Rel, s.Priv)
		}
		byKey[k] = s
		if _, known := exp[s.Rel]; !known {
			return nil, fmt.Errorf("a statement for %s %s, a relation with no expectation for %s", s.Rel, s.Priv, role)
		}
	}
	var out []roleCase
	for _, rel := range sortedMapKeys(exp) {
		letters := exp[rel]
		for i := 0; i < len(letters); i++ {
			if _, ok := rolePrivWords[letters[i]]; !ok {
				return nil, fmt.Errorf("%s %s: %q is not a privilege letter (S I U D T)", role, rel, string(letters[i]))
			}
		}
		for _, p := range rolesPrivsFor(rel) {
			st := roleStmt{Rel: rel, Priv: p, SQL: "SELECT count(*) FROM " + rel}
			if p != "SELECT" {
				var found bool
				if st, found = byKey[rel+"\x00"+p]; !found {
					return nil, fmt.Errorf("no statement for %s %s: every privilege of every relation needs one", rel, p)
				}
			}
			l := letterFor(p)
			out = append(out, roleCase{Role: role, Stmt: st, Allowed: l != 0 && strings.IndexByte(letters, l) >= 0})
		}
		for _, s := range roleStatements {
			if s.Rel == rel && strings.Contains(s.Priv, "(") {
				out = append(out, roleCase{Role: role, Stmt: s, Allowed: containsString(strings.Split(s.Only, ","), role)})
			}
		}
	}
	allowed, refused := map[string]int{}, map[string]int{}
	for _, c := range out {
		if c.Allowed {
			allowed[c.Stmt.Rel]++
		} else {
			refused[c.Stmt.Rel]++
		}
	}
	for rel := range exp {
		if allowed[rel] == 0 {
			return nil, fmt.Errorf("%s on %s: no allowed case: a role that can do nothing to a table is a role that cannot read it", role, rel)
		}
		if refused[rel] == 0 {
			return nil, fmt.Errorf("%s on %s: no refused case", role, rel)
		}
	}
	return out, nil
}

func letterFor(priv string) byte {
	for l, w := range rolePrivWords {
		if w == priv {
			return l
		}
	}
	return 0
}

// rolesRefusal is what a refusal must look like: SQLSTATE and a substring of the message. A table: 42501 and the table's name. A write to a view is refused by the rewriter, for the
// superuser too, with the state the catalogue states.
func rolesRefusal(c roleCase) (state, why string) {
	if roleViews[c.Stmt.Rel] && !roleWritableViews[c.Stmt.Rel] && c.Stmt.Priv != "SELECT" {
		return c.Stmt.Ctrl, c.Stmt.Why
	}
	kind := "table"
	if roleViews[c.Stmt.Rel] {
		kind = "view" // a refusal of a relation of kind view names it as a view
	}
	return "42501", "permission denied for " + kind + " " + c.Stmt.Rel[strings.IndexByte(c.Stmt.Rel, '.')+1:]
}

// rolesWantResult asserts a statement's outcome: success when wantState is "", else exactly that SQLSTATE and a message containing why.
func rolesWantResult(t *testing.T, err error, wantState, why, who string) {
	t.Helper()
	if wantState == "" {
		if err != nil {
			t.Fatalf("%s: the statement failed (SQLSTATE %q): %v", who, sqlState(err), err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s: the statement succeeded, want SQLSTATE %s (%s)", who, wantState, why)
	}
	if got := sqlState(err); got != wantState {
		t.Fatalf("%s: SQLSTATE %q (%v), want %s (%s)", who, got, err, wantState, why)
	}
	if !strings.Contains(err.Error(), why) {
		t.Fatalf("%s: the error does not say %q: %v", who, why, err)
	}
}

// rolesProof is the proof for one role: every case of its matrix, run as the role.
func rolesProof(t *testing.T, role string) {
	cases, err := rolesMatrix(role)
	if err != nil {
		t.Fatalf("the proof for %s is incomplete: %v", role, err)
	}
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	tx := rolesBegin(t, ctx, admin)
	rolesSeed(t, ctx, tx)

	var tables []string
	for rel := range roleExpectations[role] {
		var kind string
		if err := tx.QueryRow(ctx, "SELECT c.relkind::text FROM pg_class c WHERE c.oid = to_regclass($1::text)", rel).Scan(&kind); err != nil {
			t.Fatalf("relkind of %s: %v", rel, err)
		}
		if want := map[bool]string{true: "v", false: "r"}[roleViews[rel]]; kind != want {
			t.Fatalf("%s is relkind %q, the proof treats it as %q: roleViews is out of date", rel, kind, want)
		}
		if !roleViews[rel] {
			tables = append(tables, rel)
		}
	}
	sort.Strings(tables)
	rolesRequireRows(t, ctx, tx, tables) // as the superuser: every table has the rows the proof needs

	// CONTROL, before the switch: every statement the role must be refused is not refused for privilege when the superuser runs it. It succeeds or fails for the reason the catalogue names.
	refused := 0
	for _, c := range cases {
		if c.Allowed {
			continue
		}
		refused++
		c := c
		t.Run("control superuser/"+c.name(), func(t *testing.T) {
			rolesWantResult(t, rolesTry(t, ctx, tx, rolesExpand(c.Stmt.SQL)), c.Stmt.Ctrl, c.Stmt.Why, "as the superuser (the control for the refusal below)")
		})
	}

	rolesSwitch(t, ctx, tx, role)
	rolesRequireRows(t, ctx, tx, tables) // as the role: it reads every table and sees rows

	allowed := 0
	for _, c := range cases {
		c := c
		if c.Allowed {
			allowed++
		}
		t.Run(c.name(), func(t *testing.T) {
			err := rolesTry(t, ctx, tx, rolesExpand(c.Stmt.SQL))
			if c.Allowed {
				if err != nil {
					t.Fatalf("as %s: %s was refused (SQLSTATE %q): %v", role, c.Stmt.Priv, sqlState(err), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("as %s: %s on %s was ALLOWED", role, c.Stmt.Priv, c.Stmt.Rel)
			}
			state, why := rolesRefusal(c)
			rolesWantResult(t, err, state, why, "as "+role)
		})
	}
	t.Logf("%s: %d allowed and %d refused statements over %d relations, each refused one first run as the superuser", role, allowed, refused, len(roleExpectations[role]))
}

// TestRoles_PlatformApp: platform_app, the service's identity, does exactly what its grants say on every table and view - and every other statement is refused with 42501.
// Fails if any grant is wider or narrower than roleExpectations["platform_app"] (an allowed statement refused, a refused statement allowed), if the role switch does nothing, or if a
// refused statement is not valid for the superuser.
func TestRoles_PlatformApp(t *testing.T) { rolesProof(t, "platform_app") }

// TestRoles_PlatformAdmin: platform_admin, the break-glass role, reads everything, writes the ledger and the audit log (SELECT, INSERT) and nothing else - INSERT into jobs, UPDATE of a
// price, writing a user are all refused. Fails on the same breakages as TestRoles_PlatformApp.
func TestRoles_PlatformAdmin(t *testing.T) { rolesProof(t, "platform_admin") }

// TestRoles_UngrantedRoleIsRefusedEverywhere is the paired control of the two proofs above: the SELECTs they allow are allowed because of the grants. A role that exists, may enter both
// schemas and holds NOTHING is refused SELECT on every table and every view with 42501 and the relation's name - which also shows no relation is readable through PUBLIC. The same SELECTs
// succeed for the superuser, for platform_app and for platform_admin. Fails if any table or view grants SELECT to PUBLIC, or if the role switch does nothing.
func TestRoles_UngrantedRoleIsRefusedEverywhere(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	tx := rolesBegin(t, ctx, admin)
	rolesSeed(t, ctx, tx)

	rels := sortedMapKeys(roleExpectations["platform_app"])
	if len(rels) < 25 {
		t.Fatalf("found %d relations in the expectations, want at least 25 (23 in schema platform, 2 in public)", len(rels))
	}
	// A role created inside the transaction is gone when it rolls back: nothing to clean up in the cluster.
	for _, stmt := range []string{"CREATE ROLE zz_roles_ungranted NOLOGIN", "GRANT USAGE ON SCHEMA platform, public TO zz_roles_ungranted"} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	selectAll := func(who string) {
		for _, rel := range rels {
			if err := rolesTry(t, ctx, tx, "SELECT count(*) FROM "+rel); err != nil {
				t.Fatalf("control: SELECT FROM %s as %s was refused: %v", rel, who, err)
			}
		}
	}
	selectAll("the superuser")
	for _, role := range []string{"platform_app", "platform_admin"} {
		rolesSwitch(t, ctx, tx, role)
		selectAll(role)
		if _, err := tx.Exec(ctx, "RESET ROLE"); err != nil {
			t.Fatal(err)
		}
	}

	rolesSwitch(t, ctx, tx, "zz_roles_ungranted")
	for _, rel := range rels {
		kind := "table"
		if roleViews[rel] {
			kind = "view"
		}
		err := rolesTry(t, ctx, tx, "SELECT count(*) FROM "+rel)
		if err == nil {
			t.Fatalf("a role holding nothing could SELECT from %s: a grant to PUBLIC, or the switch did nothing", rel)
		}
		rolesWantResult(t, err, "42501", "permission denied for "+kind+" "+rel[strings.IndexByte(rel, '.')+1:], "as a role that holds nothing, SELECT FROM "+rel)
	}
}

// rolesDisagreements compares the behavioural expectations with the declared classes (both passed in so the control can feed it a broken copy) and returns every difference.
func rolesDisagreements(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []string {
	var v []string
	declared := map[string]privClass{}
	for k, c := range platform {
		declared["platform."+k] = c
	}
	for k, c := range public {
		declared["public."+k] = c
	}
	for _, role := range []string{"platform_app", "platform_admin"} {
		for rel := range declared {
			if _, ok := exp[role][rel]; !ok {
				v = append(v, fmt.Sprintf("%s: declared in declaredClasses/publicClasses but %s has no behavioural expectation for it", rel, role))
			}
		}
		for _, rel := range sortedMapKeys(exp[role]) {
			class, ok := declared[rel]
			if !ok {
				v = append(v, fmt.Sprintf("%s: %s has an expectation for a relation that has no declared class", rel, role))
				continue
			}
			want := map[string]bool{}
			for i := 0; i < len(exp[role][rel]); i++ {
				want[rolePrivWords[exp[role][rel][i]]] = true
			}
			have := map[string]bool{}
			for _, p := range class.Grants[role] {
				have[p] = true
			}
			for _, p := range sortedKeys(want) {
				if !have[p] {
					v = append(v, fmt.Sprintf("%s on %s: the proof expects %s to be allowed, the declared class does not grant it", role, rel, p))
				}
			}
			for _, p := range sortedKeys(have) {
				if !want[p] {
					v = append(v, fmt.Sprintf("%s on %s: the declared class grants %s, the proof does not expect it to be allowed", role, rel, p))
				}
			}
		}
	}
	for _, s := range stmts {
		class, ok := declared[s.Rel]
		if !ok {
			v = append(v, fmt.Sprintf("a statement for %s %s, a relation with no declared class", s.Rel, s.Priv))
			continue
		}
		open := strings.IndexByte(s.Priv, '(')
		if open < 0 {
			continue
		}
		priv, cols := s.Priv[:open], strings.Split(strings.TrimSuffix(s.Priv[open+1:], ")"), ",")
		for _, role := range []string{"platform_app", "platform_admin"} {
			byDeclaration := containsString(class.Grants[role], priv)
			if !byDeclaration {
				byDeclaration = true
				for _, col := range cols {
					byDeclaration = byDeclaration && containsString(class.ColumnGrants[role][priv], col)
				}
			}
			claimed := containsString(strings.Split(s.Only, ","), role)
			if claimed != byDeclaration {
				v = append(v, fmt.Sprintf("%s on %s: the statement for %s is claimed %s, the declared class says %s", role, s.Rel, s.Priv, allowedWord(claimed), allowedWord(byDeclaration)))
			}
		}
	}
	return v
}

func allowedWord(b bool) string {
	if b {
		return "allowed"
	}
	return "refused"
}

func copyClasses(m map[string]privClass) map[string]privClass {
	out := map[string]privClass{}
	for k, c := range m {
		g := map[string][]string{}
		for r, p := range c.Grants {
			g[r] = append([]string(nil), p...)
		}
		cg := map[string]map[string][]string{}
		for r, byPriv := range c.ColumnGrants {
			cg[r] = map[string][]string{}
			for p, cols := range byPriv {
				cg[r][p] = append([]string(nil), cols...)
			}
		}
		out[k] = privClass{Owner: c.Owner, Grants: g, ColumnGrants: cg}
	}
	return out
}

func copyExpectations(m map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for role, rels := range m {
		out[role] = map[string]string{}
		for rel, letters := range rels {
			out[role][rel] = letters
		}
	}
	return out
}

// TestRoles_ExpectationsAgreeWithDeclaredClasses: what the sessions are held to (roleExpectations, roleStatements) and what the catalogue check is held to (declaredClasses, publicClasses)
// are two statements of one design, written separately. This test fails on any difference between them, and fails if the matrix of either role is incomplete (a privilege of a relation
// with no statement). Its controls feed the comparison broken copies and require it to say what is broken, so it cannot agree with everything.
func TestRoles_ExpectationsAgreeWithDeclaredClasses(t *testing.T) {
	if v := rolesDisagreements(roleExpectations, declaredClasses, publicClasses, roleStatements); len(v) != 0 {
		t.Fatalf("%d disagreement(s) between the role proofs and the declared classes:\n  %s", len(v), strings.Join(v, "\n  "))
	}
	for _, role := range []string{"platform_app", "platform_admin"} {
		cases, err := rolesMatrix(role)
		if err != nil {
			t.Fatalf("the matrix of %s is incomplete: %v", role, err)
		}
		t.Logf("%s: %d cases over %d relations", role, len(cases), len(roleExpectations[role]))
	}

	breaks := []struct {
		name   string
		mutate func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt
		want   []string
	}{
		{"UPDATE granted on the ledger to platform_app (declared only)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			c := platform["ledger"]
			c.Grants["platform_app"] = append(c.Grants["platform_app"], "UPDATE")
			platform["ledger"] = c
			return stmts
		}, []string{"platform_app on platform.ledger", "declared class grants UPDATE"}},
		{"DELETE expected allowed on public.users for platform_admin (expectation only)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			exp["platform_admin"]["public.users"] += "D"
			return stmts
		}, []string{"platform_admin on public.users", "expects DELETE", "does not grant"}},
		{"INSERT expected allowed on the user view for platform_app (expectation only: the view is auto-updatable, so that would be a write path into public.users)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			exp["platform_app"]["platform.listable_users"] += "I"
			return stmts
		}, []string{"platform_app on platform.listable_users", "expects INSERT", "does not grant"}},
		{"UPDATE granted on the user view to platform_admin (declared only)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			c := platform["listable_users"]
			c.Grants["platform_admin"] = append(c.Grants["platform_admin"], "UPDATE")
			platform["listable_users"] = c
			return stmts
		}, []string{"platform_admin on platform.listable_users", "declared class grants UPDATE"}},
		{"a table declared but with no expectation", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			platform["zz_new_table"] = classOf(privSelect, privSelect)
			return stmts
		}, []string{"platform.zz_new_table", "no behavioural expectation"}},
		{"an expectation for a relation nobody declared", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			delete(platform, "suppressions")
			return stmts
		}, []string{"platform.suppressions", "no declared class"}},
		{"the column grant widened in the declaration only", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			c := platform["unit_costs"]
			c.ColumnGrants["platform_app"]["UPDATE"] = append(c.ColumnGrants["platform_app"]["UPDATE"], "description")
			platform["unit_costs"] = c
			return stmts
		}, []string{"platform_app on platform.unit_costs", "UPDATE(cost,changed_by,change_reason,description)", "claimed refused", "says allowed"}},
		{"the column statement claimed for the wrong role", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			out := append([]roleStmt(nil), stmts...)
			for i := range out {
				if out[i].Priv == "UPDATE(cost,changed_by,change_reason)" {
					out[i].Only = "platform_app,platform_admin"
				}
			}
			return out
		}, []string{"platform_admin on platform.unit_costs", "UPDATE(cost,changed_by,change_reason)", "claimed allowed", "says refused"}},
		{"INSERT granted on reserves to platform_app (declared only: nobody writes it but the ledger trigger)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			c := platform["reserves"]
			c.Grants["platform_app"] = append(c.Grants["platform_app"], "INSERT")
			platform["reserves"] = c
			return stmts
		}, []string{"platform_app on platform.reserves", "declared class grants INSERT"}},
		{"UPDATE expected allowed on m3_entitlements for platform_app (expectation only: since platform_0006 the ledger trigger is its only writer)", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			exp["platform_app"]["platform.m3_entitlements"] += "U"
			return stmts
		}, []string{"platform_app on platform.m3_entitlements", "expects UPDATE", "does not grant"}},
		{"the m3_sync column grant widened to the wanted state in the declaration only", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			c := platform["m3_sync"]
			c.ColumnGrants["platform_app"]["UPDATE"] = append(c.ColumnGrants["platform_app"]["UPDATE"], "want_suspended", "want_version")
			platform["m3_sync"] = c
			return stmts
		}, []string{"platform_app on platform.m3_sync", "UPDATE(want_suspended,want_version)", "claimed refused", "says allowed"}},
		{"the m3_sync column statement claimed for platform_admin", func(exp map[string]map[string]string, platform, public map[string]privClass, stmts []roleStmt) []roleStmt {
			out := append([]roleStmt(nil), stmts...)
			for i := range out {
				if strings.HasPrefix(out[i].Priv, "UPDATE(observed,") {
					out[i].Only = "platform_app,platform_admin"
				}
			}
			return out
		}, []string{"platform_admin on platform.m3_sync", "claimed allowed", "says refused"}},
	}
	for _, b := range breaks {
		b := b
		t.Run("control: "+b.name, func(t *testing.T) {
			exp, platform, public := copyExpectations(roleExpectations), copyClasses(declaredClasses), copyClasses(publicClasses)
			stmts := b.mutate(exp, platform, public, roleStatements)
			got := rolesDisagreements(exp, platform, public, stmts)
			if len(got) == 0 {
				t.Fatal("the comparison reported nothing for a broken copy: it cannot fail")
			}
			if !hasViolation(got, b.want...) {
				t.Fatalf("no disagreement mentions %v; the comparison said:\n  %s", b.want, strings.Join(got, "\n  "))
			}
		})
	}
	// The matrix builder fails on an incomplete catalogue: the same control for its own checks.
	saved := roleStatements
	defer func() { roleStatements = saved }()
	roleStatements = saved[1:] // the first statement (INSERT on schema_migrations) goes missing
	if _, err := rolesMatrix("platform_app"); err == nil || !strings.Contains(err.Error(), "no statement for platform.schema_migrations INSERT") {
		t.Fatalf("a catalogue without a statement was accepted by the matrix builder (err = %v)", err)
	}
}

// tzUTCOffset matches an instant rendered in UTC: timestamptz text ends in +00.
var tzUTCOffset = regexp.MustCompile(`\+00$`)

// TestRoles_PlatformAppLoginIsUTC: Phase 1 locked decision 3 - every instant is timestamptz, stored UTC, and the platform_app SESSION timezone is UTC (a role setting, made by platform_0001).
// This test logs in as the REAL platform_app over the network (the password the harness stack gave the service) and asks the session, rather than reading the migration.
//
// It fails when the `ALTER ROLE platform_app SET timezone = 'UTC'` line is removed (no pg_db_role_setting row, and the session shows the server's default zone), and each half is guarded by a
// control: (1) the reader of pg_db_role_setting reports nothing for roles that have no setting, and reports the removal when the setting is reset inside a transaction that is then rolled
// back; (2) a login that names another zone in its startup parameters shows that zone, so SHOW timezone is a live instrument and "UTC" below is not a constant.
func TestRoles_PlatformAppLoginIsUTC(t *testing.T) {
	adminURL := requireEnv(t, envAdminURL)
	ctx := testCtx(t)
	live := liveAdmin(t)

	// the role-level setting exists (the cause)
	got, err := rolesTimezoneSetting(ctx, live, "platform_app")
	if err != nil {
		t.Fatal(err)
	}
	if got != "UTC" {
		t.Fatalf("pg_db_role_setting for platform_app (this database) holds timezone %q for this database, want UTC: platform_0001's ALTER ROLE ... IN DATABASE ... SET timezone is missing or was reset", got)
	}
	// control: roles that have no such setting are reported as having none
	for _, role := range []string{"platform_owner", "platform_admin"} {
		if s, err := rolesTimezoneSetting(ctx, live, role); err != nil || s != "" {
			t.Fatalf("control: %s reads as having the timezone setting %q (err %v): the reader cannot tell a role with the setting from one without", role, s, err)
		}
	}
	// control: the reader notices the setting being removed. ALTER ROLE is transactional: the change is made on a scratch connection and rolled back, so nobody else ever sees it.
	_, sc := migratedScratch(t) // a MIGRATED scratch database: the setting is database-scoped, so the control needs a database that has it
	tx, err := sc.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var scDB string
	if err := sc.QueryRow(ctx, "SELECT current_database()").Scan(&scDB); err != nil {
		t.Fatal(err)
	}
	if s, err := rolesTimezoneSetting(ctx, tx, "platform_app"); err != nil || s != "UTC" {
		t.Fatalf("control precondition: the migrated scratch database %s has the setting %q (err %v), want UTC", scDB, s, err)
	}
	if _, err := tx.Exec(ctx, "ALTER ROLE platform_app IN DATABASE "+pgx.Identifier{scDB}.Sanitize()+" RESET timezone"); err != nil {
		t.Fatalf("ALTER ROLE platform_app IN DATABASE %s RESET timezone: %v", scDB, err)
	}
	if s, err := rolesTimezoneSetting(ctx, tx, "platform_app"); err != nil || s != "" {
		t.Fatalf("control: after RESET timezone the reader says %q (err %v), want no setting: it cannot fail", s, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := rolesTimezoneSetting(ctx, sc, "platform_app"); err != nil || s != "UTC" {
		t.Fatalf("after the rollback the setting reads %q (err %v), want UTC restored", s, err)
	}

	// the effect: a real login as platform_app starts in UTC
	app := rolesAppLogin(t, adminURL, nil)
	var sessionUser, currentUser, zone, instant string
	var super bool
	if err := app.QueryRow(ctx, "SELECT session_user, current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user), current_setting('TimeZone'), now()::text").Scan(&sessionUser, &currentUser, &super, &zone, &instant); err != nil {
		t.Fatal(err)
	}
	if sessionUser != "platform_app" || currentUser != "platform_app" {
		t.Fatalf("session_user=%q current_user=%q, want platform_app for both: this must be a real login, not a role switch", sessionUser, currentUser)
	}
	if super {
		t.Fatal("platform_app is a superuser")
	}
	var show string
	if err := app.QueryRow(ctx, "SHOW timezone").Scan(&show); err != nil {
		t.Fatal(err)
	}
	if show != "UTC" || zone != "UTC" {
		t.Fatalf("a platform_app login shows timezone %q (current_setting %q), want UTC", show, zone)
	}
	if !tzUTCOffset.MatchString(instant) {
		t.Fatalf("now()::text = %q in a platform_app session: want an instant rendered at offset +00", instant)
	}

	// control: SHOW timezone reports what the session was started with
	tokyo := rolesAppLogin(t, adminURL, func(cfg *pgx.ConnConfig) { cfg.RuntimeParams["timezone"] = "Asia/Tokyo" })
	var other string
	if err := tokyo.QueryRow(ctx, "SHOW timezone").Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != "Asia/Tokyo" {
		t.Fatalf("control: a login that asks for Asia/Tokyo shows %q: SHOW timezone is not reporting the session's zone, so the UTC above proves nothing", other)
	}
}
