//go:build harness

package harness

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/usersettings"
)

// The per-user settings (Phase 5 S6; internal/usersettings, platform_0020) against a REAL PostgreSQL, through the store as a login of platform_app. What is proved: that a user who never stated a setting is
// opted in, that the value round-trips and is the user's alone (another user's value is never read), that a write is idempotent and a CHANGE writes exactly one history row whose actor is the user and which
// holds nothing else, that the roles hold only what platform_0020 grants (and the module roles hold nothing), that the system user and an unknown user are refused, and that a setting is never deleted.

type usEnv struct {
	*mnyEnv
	us *usersettings.Store
}

func newUsEnv(t *testing.T) *usEnv {
	t.Helper()
	e := &usEnv{mnyEnv: newMnyEnv(t)}
	us, err := usersettings.New(e.pool)
	if err != nil {
		t.Fatalf("usersettings.New: %v", err)
	}
	e.us = us
	return e
}

// history is the history rows of one user, oldest first, as "old>new by actor" (true/false words).
func (e *usEnv) history(user string) []string {
	e.t.Helper()
	return e.rows(`SELECT old_value::text || '>' || new_value::text || ' by ' || (CASE WHEN actor_user_id = user_id THEN 'the user' ELSE 'SOMEONE ELSE' END) FROM platform.user_settings_history WHERE user_id = $1 ORDER BY id`, user)
}

func (e *usEnv) mustSet(user string, v bool) usersettings.Settings {
	e.t.Helper()
	s, err := e.us.Set(testCtx(e.t), user, v)
	if err != nil {
		e.t.Fatalf("Set(%s, %v): %v", user, v, err)
	}
	return s
}

func (e *usEnv) mustGet(user string) usersettings.Settings {
	e.t.Helper()
	s, err := e.us.Get(testCtx(e.t), user)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", user, err)
	}
	return s
}

func TestUserSettings_AUserWhoNeverStatedASettingIsOptedInAndTheValueRoundTrips(t *testing.T) {
	e := newUsEnv(t)
	if got := e.mustGet(mnyUserA); got.VendorFallbackOptOut {
		t.Fatal("a user with no row is opted OUT: the default is opted in (owner ruling)")
	}
	if n := e.int("SELECT count(*) FROM platform.user_settings WHERE user_id = $1", mnyUserA); n != 0 {
		t.Fatalf("a read made a row (%d)", n)
	}
	if got := e.mustSet(mnyUserA, true); !got.VendorFallbackOptOut {
		t.Fatalf("Set(true) answered %+v", got)
	}
	if got := e.mustGet(mnyUserA); !got.VendorFallbackOptOut {
		t.Fatalf("after Set(true) the user reads %+v", got)
	}
	if got := e.str("SELECT vendor_fallback_opt_out::text FROM platform.user_settings WHERE user_id = $1", mnyUserA); got != "true" {
		t.Fatalf("the table holds %q", got)
	}
	if got := e.mustSet(mnyUserA, false); got.VendorFallbackOptOut {
		t.Fatalf("Set(false) answered %+v", got)
	}
	if got := e.mustGet(mnyUserA); got.VendorFallbackOptOut {
		t.Fatalf("after Set(false) the user reads %+v", got)
	}
	// the read inside a transaction the caller owns (how a job reads it when it is created) says the same
	tx, err := e.pool.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(testCtx(t)) }()
	e.mustSet(mnyUserB, true)
	if v, err := usersettings.VendorOptOutOf(testCtx(t), tx, mnyUserB); err != nil || !v {
		t.Errorf("VendorOptOutOf(B) in a transaction = %v, %v, want true", v, err)
	}
	if v, err := usersettings.VendorOptOutOf(testCtx(t), tx, mnyUserC); err != nil || v {
		t.Errorf("VendorOptOutOf(C) in a transaction = %v, %v, want false (no row)", v, err)
	}
}

func TestUserSettings_TheValueIsTheUsersAloneAndAnotherUsersValueIsNeverRead(t *testing.T) {
	e := newUsEnv(t)
	e.mustSet(mnyUserA, true)
	// the control that the instrument can see a difference: A is opted out
	if !e.mustGet(mnyUserA).VendorFallbackOptOut {
		t.Fatal("control: A is not opted out")
	}
	// B and C never stated one: opted in, whatever A said
	for _, u := range []string{mnyUserB, mnyUserC} {
		if e.mustGet(u).VendorFallbackOptOut {
			t.Errorf("user %s reads A's value", u)
		}
	}
	// B opts out, then A opts back in: each change is its own
	e.mustSet(mnyUserB, true)
	e.mustSet(mnyUserA, false)
	if e.mustGet(mnyUserA).VendorFallbackOptOut || !e.mustGet(mnyUserB).VendorFallbackOptOut || e.mustGet(mnyUserC).VendorFallbackOptOut {
		t.Errorf("A, B, C read %+v %+v %+v after B opted out and A opted back in, want false, true, false", e.mustGet(mnyUserA), e.mustGet(mnyUserB), e.mustGet(mnyUserC))
	}
	// every history row is its user's: A has two, B one, C none
	if got := e.history(mnyUserA); strings.Join(got, ";") != "false>true by the user;true>false by the user" {
		t.Errorf("A's history is %v", got)
	}
	if got := e.history(mnyUserB); strings.Join(got, ";") != "false>true by the user" {
		t.Errorf("B's history is %v", got)
	}
	if got := e.history(mnyUserC); len(got) != 0 {
		t.Errorf("C's history is %v, want none", got)
	}
}

func TestUserSettings_AWriteIsIdempotentAndOnlyAChangeWritesOneHistoryRowAndTheRowKeepsTheTimeOfItsLastChange(t *testing.T) {
	e := newUsEnv(t)
	// a first row that states the default is a row and no change
	e.mustSet(mnyUserC, false)
	if n := e.int("SELECT count(*) FROM platform.user_settings WHERE user_id = $1", mnyUserC); n != 1 {
		t.Fatalf("a first Set(false) made %d row(s), want 1", n)
	}
	if got := e.history(mnyUserC); len(got) != 0 {
		t.Fatalf("a first row that states the default (opted in) wrote history %v, want none", got)
	}
	// a change writes one row; the same value three times more writes none and moves nothing
	e.mustSet(mnyUserA, true)
	at := e.str("SELECT updated_at::text FROM platform.user_settings WHERE user_id = $1", mnyUserA)
	time.Sleep(20 * time.Millisecond) // a clock that could move is given the time to
	for i := 0; i < 3; i++ {
		if got := e.mustSet(mnyUserA, true); !got.VendorFallbackOptOut {
			t.Fatalf("a repeat of Set(true) answered %+v", got)
		}
	}
	if got := e.history(mnyUserA); len(got) != 1 || got[0] != "false>true by the user" {
		t.Fatalf("after one change and three repeats the history is %v, want ONE row (false>true by the user)", got)
	}
	if got := e.str("SELECT updated_at::text FROM platform.user_settings WHERE user_id = $1", mnyUserA); got != at {
		t.Errorf("a repeat moved updated_at from %s to %s: the row keeps the time of its last CHANGE", at, got)
	}
	// the way back is one more row, and moves the time
	e.mustSet(mnyUserA, false)
	if got := e.history(mnyUserA); len(got) != 2 || got[1] != "true>false by the user" {
		t.Fatalf("after the way back the history is %v, want a second row (true>false by the user)", got)
	}
	if got := e.str("SELECT updated_at::text FROM platform.user_settings WHERE user_id = $1", mnyUserA); got == at {
		t.Errorf("a change did not move updated_at (%s)", got)
	}
	// two writers at once end in one state and one history row (the statement is the idempotence, not a lock in Go)
	e.mustSet(mnyUserB, false)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := e.us.Set(testCtx(t), mnyUserB, true)
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("a concurrent Set: %v", err)
		}
	}
	if got := e.history(mnyUserB); len(got) != 1 || got[0] != "false>true by the user" {
		t.Errorf("eight concurrent Set(true) left the history %v, want exactly one row", got)
	}
	e.requireViewsEmpty("after the settings")
}

// boolQ runs a one-value boolean query as the superuser.
func (e *usEnv) boolQ(sql string, args ...any) bool {
	e.t.Helper()
	var b bool
	if err := e.admin.QueryRow(testCtx(e.t), sql, args...).Scan(&b); err != nil {
		e.t.Fatalf("query failed: %v\n  sql: %s", err, mnyOneLine(sql))
	}
	return b
}

func TestUserSettings_TheHistoryHoldsExactlyTheChangeTheUserTheTimeAndNothingElseAndIsAppendOnly(t *testing.T) {
	e := newUsEnv(t)
	e.mustSet(mnyUserA, true)
	cols := e.rows("SELECT column_name FROM information_schema.columns WHERE table_schema = 'platform' AND table_name = 'user_settings_history' ORDER BY column_name")
	if strings.Join(cols, ",") != "actor_user_id,changed_at,id,new_value,old_value,setting,user_id" {
		t.Fatalf("the history's columns are %v: it records the user, the setting, the old and new value and the time, and nothing else (no address, no token, no text)", cols)
	}
	if got := e.rows("SELECT setting || '|' || actor_user_id::text FROM platform.user_settings_history WHERE user_id = $1", mnyUserA); len(got) != 1 || got[0] != "vendor_fallback_opt_out|"+mnyUserA {
		t.Fatalf("the history row is %v", got)
	}
	// append-only for everyone (the superuser meets the triggers), and its CHECKs bite: a row that is no change, a row whose actor is not the user, a setting that is not the one setting
	tx := ledgerBegin(t, e.admin)
	const ins = "INSERT INTO platform.user_settings_history (user_id, setting, old_value, new_value, actor_user_id) VALUES ($1, $2, $3, $4, $5)"
	ledgerExpectOK(t, tx, "control: a legal history row", ins, mnyUserB, "vendor_fallback_opt_out", false, true, mnyUserB)
	ledgerExpectRefused(t, tx, "a row that records no change", "23514", ledgerWhy{Constraint: "user_settings_history_a_change"}, ins, mnyUserB, "vendor_fallback_opt_out", true, true, mnyUserB)
	ledgerExpectRefused(t, tx, "a row whose actor is not the user", "23514", ledgerWhy{Constraint: "user_settings_history_the_user_is_the_actor"}, ins, mnyUserB, "vendor_fallback_opt_out", false, true, mnyUserC)
	ledgerExpectRefused(t, tx, "a row about a setting that does not exist", "23514", ledgerWhy{Constraint: "user_settings_history_setting_vocabulary"}, ins, mnyUserB, "colour", false, true, mnyUserB)
	ledgerExpectRefused(t, tx, "an UPDATE of a history row", "23001", ledgerWhy{Message: "append-only"}, "UPDATE platform.user_settings_history SET new_value = new_value")
	ledgerExpectRefused(t, tx, "a DELETE of a history row", "23001", ledgerWhy{Message: "append-only"}, "DELETE FROM platform.user_settings_history")
	ledgerExpectRefused(t, tx, "a TRUNCATE of the history", "23001", ledgerWhy{Message: "append-only"}, "TRUNCATE platform.user_settings_history")
}

func TestUserSettings_ARowIsNeverDeletedNotEvenByTheOwnerAndTheSystemUserHasNone(t *testing.T) {
	e := newUsEnv(t)
	e.mustSet(mnyUserA, true)
	tx := ledgerBegin(t, e.admin)
	ledgerExpectRefused(t, tx, "a DELETE of a setting (it would silently opt the user back in)", "23001", ledgerWhy{Message: "user_settings"}, "DELETE FROM platform.user_settings WHERE user_id = $1", mnyUserA)
	ledgerExpectRefused(t, tx, "a TRUNCATE of the settings", "23001", ledgerWhy{Message: "user_settings"}, "TRUNCATE platform.user_settings")
	// the system user (module 2's shared-pool owner) holds no choice: the table's CHECK is the second wall, the store's own check the first
	const systemUser = "00000000-0000-0000-0000-000000000000"
	ledgerExpectRefused(t, tx, "a row for the system user", "23514", ledgerWhy{Constraint: "user_settings_not_the_system_user"}, "INSERT INTO platform.user_settings (user_id) VALUES ($1)", systemUser)
	if _, err := e.us.Set(testCtx(t), systemUser, true); !errors.Is(err, usersettings.ErrBadUser) {
		t.Errorf("Set for the system user = %v, want ErrBadUser", err)
	}
	if _, err := e.us.Get(testCtx(t), systemUser); !errors.Is(err, usersettings.ErrBadUser) {
		t.Errorf("Get for the system user = %v, want ErrBadUser", err)
	}
	// a user that does not exist
	if _, err := e.us.Set(testCtx(t), "f0000000-0000-4000-8000-0000000000aa", true); !errors.Is(err, usersettings.ErrUnknownUser) {
		t.Errorf("Set for a user that does not exist = %v, want ErrUnknownUser", err)
	}
	// the control: a legal Set still works after all those refusals
	e.mustSet(mnyUserC, true)
}

func TestUserSettings_ThePlatformAppRoleHoldsExactlyWhatTheMigrationGrantsAndTheModuleRolesNothing(t *testing.T) {
	e := newUsEnv(t)
	e.mustSet(mnyUserA, true)
	ctx := testCtx(t)
	app := func(sql string, args ...any) error {
		_, err := e.pool.Exec(ctx, sql, args...) // the pool is real platform_app logins
		return err
	}
	refusedAs42501 := func(what string, err error) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("%s: %v, want SQLSTATE 42501 (permission denied)", what, err)
		}
	}
	// allowed: insert a row, update the value and its time
	if err := app("INSERT INTO platform.user_settings (user_id, vendor_fallback_opt_out) VALUES ($1, false)", mnyUserB); err != nil {
		t.Errorf("platform_app cannot insert a setting: %v", err)
	}
	if err := app("UPDATE platform.user_settings SET vendor_fallback_opt_out = true, updated_at = now() WHERE user_id = $1", mnyUserB); err != nil {
		t.Errorf("platform_app cannot update the value and its time: %v", err)
	}
	// refused: the user of a row, a delete, the history (which only the trigger writes)
	refusedAs42501("an UPDATE of user_id", app("UPDATE platform.user_settings SET user_id = $2 WHERE user_id = $1", mnyUserB, mnyUserC))
	refusedAs42501("a DELETE of a setting", app("DELETE FROM platform.user_settings WHERE user_id = $1", mnyUserB))
	refusedAs42501("an INSERT into the history", app("INSERT INTO platform.user_settings_history (user_id, setting, old_value, new_value, actor_user_id) VALUES ($1, 'vendor_fallback_opt_out', false, true, $1)", mnyUserC))
	refusedAs42501("an UPDATE of the history", app("UPDATE platform.user_settings_history SET new_value = new_value"))
	refusedAs42501("a TRUNCATE of the settings", app("TRUNCATE platform.user_settings"))
	// the trigger wrote the history although the role cannot: the change above is in it
	if got := e.history(mnyUserB); len(got) != 1 || got[0] != "false>true by the user" {
		t.Errorf("B's history after platform_app's own UPDATE is %v, want one row written by the trigger", got)
	}
	// the module roles, where their modules have been migrated here, hold nothing on either table, and neither does PUBLIC: the control is that platform_app, platform_admin and the owner are there and DO hold what they should
	for _, role := range []string{"platform_app", "platform_admin"} {
		if !e.boolQ("SELECT has_table_privilege($1, 'platform.user_settings', 'SELECT')", role) {
			t.Fatalf("control: %s cannot read the settings", role)
		}
	}
	checked := 0
	for _, role := range e.rows("SELECT rolname FROM pg_roles WHERE rolname IN ('verify_app', 'send_managed_app', 'send_byo_app', 'anon', 'authenticated', 'service_role') ORDER BY 1") {
		for _, table := range []string{"platform.user_settings", "platform.user_settings_history"} {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
				if e.boolQ("SELECT has_table_privilege($1, $2, $3)", role, table, priv) {
					t.Errorf("%s holds %s on %s", role, priv, table)
				}
			}
		}
		if e.boolQ("SELECT has_schema_privilege($1, 'platform', 'USAGE')", role) {
			t.Errorf("%s can use schema platform", role)
		}
		checked++
	}
	if n := e.int("SELECT count(*) FROM pg_class c, aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a WHERE c.relnamespace = 'platform'::regnamespace AND c.relname::text IN ('user_settings', 'user_settings_history') AND a.grantee = 0"); n != 0 {
		t.Errorf("PUBLIC holds %d privilege(s) on the settings tables", n)
	}
	t.Logf("module and API roles present in this database and checked: %d (a role whose module is not migrated here is not asked about)", checked)
}
