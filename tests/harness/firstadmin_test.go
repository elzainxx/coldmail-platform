//go:build harness

package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// platform-migrate create-first-admin, driven as the real binary (HARNESS_MIGRATE_BIN) against a migrated scratch database whose
// auth.users is the fixture (id, email, created_at). The command creates the first admin from an existing Supabase login and refuses every
// later one; every refusal below asserts the EXIT CODE, the MESSAGE and that the database is UNCHANGED, and each is paired with a control
// that shows the same database accepting the command once the cause of the refusal is removed.

const (
	faUserA        = "a1a1a1a1-0000-4000-8000-00000000000a"
	faUserB        = "b2b2b2b2-0000-4000-8000-00000000000b"
	faSystemUser   = "00000000-0000-0000-0000-000000000000"
	faAuthEmailA   = "Boss@Example.com" // mixed case on purpose: public.users wants it lower-cased
	faStoredEmailA = "boss@example.com"
	faName         = "Ada Boss"
)

func faStr(s string) *string { return &s }

func faArgs(id, name string) []string { return []string{"--auth-user-id", id, "--display-name", name} }

// faRun runs create-first-admin against the scratch database.
func faRun(t *testing.T, s scratch, args []string) result {
	t.Helper()
	return runMigrator(t, migEnv(s.DSN, repoMigrationsDir(t), nil), append([]string{"create-first-admin"}, args...)...)
}

// faSeedLogin inserts a Supabase login into the fixture auth.users; a nil email is NULL.
func faSeedLogin(t *testing.T, c *pgx.Conn, id string, email *string) {
	t.Helper()
	if _, err := c.Exec(testCtx(t), "INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, email); err != nil {
		t.Fatalf("seed the login %s: %v", id, err)
	}
}

// faCount counts the rows of a table (the table name is always a literal of this file).
func faCount(t *testing.T, c *pgx.Conn, table string) int {
	t.Helper()
	var n int
	if err := c.QueryRow(testCtx(t), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// faSnapshot is everything create-first-admin may write: every platform.admins row and every public.users row, as text. Two snapshots
// that are equal mean the database did not change.
func faSnapshot(t *testing.T, c *pgx.Conn) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []struct{ label, sql string }{
		{"platform.admins", `SELECT id::text || ' | ' || display_name || ' | disabled=' || (disabled_at IS NOT NULL)::text FROM platform.admins ORDER BY id`},
		{"public.users", `SELECT id::text || ' | ' || COALESCE(auth_user_id::text, 'NULL') || ' | ' || email || ' | active=' || is_active::text FROM public.users ORDER BY id`},
	} {
		rows, err := c.Query(testCtx(t), q.sql)
		if err != nil {
			t.Fatalf("snapshot %s: %v", q.label, err)
		}
		fmt.Fprintf(&b, "%s:\n", q.label)
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("snapshot %s: %v", q.label, err)
			}
			fmt.Fprintf(&b, "  %s\n", line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot %s: %v", q.label, err)
		}
	}
	return b.String()
}

// faRequireFresh pins the starting point of a test: no admin, and public.users holds only the reserved system user.
func faRequireFresh(t *testing.T, c *pgx.Conn) {
	t.Helper()
	if n := faCount(t, c, "platform.admins"); n != 0 {
		t.Fatalf("precondition: a freshly migrated database has %d admins, want 0", n)
	}
	var n int
	var only string
	if err := c.QueryRow(testCtx(t), "SELECT count(*), COALESCE(min(id::text), '') FROM public.users").Scan(&n, &only); err != nil {
		t.Fatal(err)
	}
	if n != 1 || only != faSystemUser {
		t.Fatalf("precondition: public.users holds %d rows (first %q), want only the system user %s", n, only, faSystemUser)
	}
}

// faWithFirstAdmin is a migrated scratch database in which login A has become the first admin (asserted, as the precondition of the
// refusal tests that need an admin to exist).
func faWithFirstAdmin(t *testing.T) (scratch, *pgx.Conn) {
	t.Helper()
	s, admin := migratedScratch(t)
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	mustExit(t, faRun(t, s, faArgs(faUserA, faName)), 0, "the first create-first-admin (the precondition of this test)")
	if n := faCount(t, admin, "platform.admins"); n != 1 {
		t.Fatalf("precondition: %d admins after the first create-first-admin, want 1", n)
	}
	return s, admin
}

// faMustRefuse asserts one refusal: the exit code, every wanted text on standard error, nothing on standard output, and no connection
// string anywhere. The caller compares the database before and after.
func faMustRefuse(t *testing.T, what string, res result, code int, s scratch, want ...string) {
	t.Helper()
	mustExit(t, res, code, what)
	if res.Stdout != "" {
		t.Fatalf("%s: a refusal printed on standard output: %q", what, res.Stdout)
	}
	for _, w := range want {
		mustContain(t, res.Stderr, w, what)
	}
	if strings.Contains(res.all(), s.DSN) {
		t.Fatalf("%s: the output contains the connection string:\n%s", what, res.all())
	}
}

func faMustBeUnchanged(t *testing.T, what string, c *pgx.Conn, before string) {
	t.Helper()
	if after := faSnapshot(t, c); after != before {
		t.Fatalf("%s: the database changed.\nbefore:\n%s\nafter:\n%s", what, before, after)
	}
}

// ---- the command ----

// The control of every refusal below: with no admin and a valid login the command succeeds, prints exactly one line, and writes exactly
// one admins row and one public.users row (id = auth_user_id, the email lower-cased, active), leaving the system user alone.
func TestFirstAdmin_CreatesTheAdminAndItsUserRow(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	systemBefore := ""
	if err := admin.QueryRow(testCtx(t), "SELECT email || ' ' || is_active::text FROM public.users WHERE id = $1", faSystemUser).Scan(&systemBefore); err != nil {
		t.Fatal(err)
	}

	res := faRun(t, s, faArgs(faUserA, faName))
	mustExit(t, res, 0, "create-first-admin on a database with no admin")
	if want := "create-first-admin: created admin " + faUserA + " (" + faName + ") for " + faStoredEmailA + "\n"; res.Stdout != want {
		t.Fatalf("standard output %q, want exactly %q", res.Stdout, want)
	}
	if res.Stderr != "" {
		t.Fatalf("standard error %q, want nothing", res.Stderr)
	}
	if strings.Contains(res.all(), s.DSN) {
		t.Fatalf("the output contains the connection string:\n%s", res.all())
	}

	ctx := testCtx(t)
	if n := faCount(t, admin, "platform.admins"); n != 1 {
		t.Fatalf("platform.admins holds %d rows, want 1", n)
	}
	var id, display string
	var enabled bool
	if err := admin.QueryRow(ctx, "SELECT id::text, display_name, disabled_at IS NULL FROM platform.admins").Scan(&id, &display, &enabled); err != nil {
		t.Fatal(err)
	}
	if id != faUserA || display != faName || !enabled {
		t.Fatalf("the admin row is (%s, %q, enabled=%v), want (%s, %q, enabled=true)", id, display, enabled, faUserA, faName)
	}
	if n := faCount(t, admin, "public.users"); n != 2 {
		t.Fatalf("public.users holds %d rows, want the system user and the new admin", n)
	}
	var userID, authID, email string
	var active bool
	if err := admin.QueryRow(ctx, "SELECT id::text, auth_user_id::text, email, is_active FROM public.users WHERE id = $1", faUserA).Scan(&userID, &authID, &email, &active); err != nil {
		t.Fatal(err)
	}
	if userID != faUserA || authID != faUserA || email != faStoredEmailA || !active {
		t.Fatalf("the user row is (%s, %s, %q, active=%v), want (%s, %s, %q, active=true)", userID, authID, email, active, faUserA, faUserA, faStoredEmailA)
	}
	var systemAfter string
	if err := admin.QueryRow(ctx, "SELECT email || ' ' || is_active::text FROM public.users WHERE id = $1", faSystemUser).Scan(&systemAfter); err != nil || systemAfter != systemBefore {
		t.Fatalf("the system user row is %q (err %v), was %q: the command must not touch it", systemAfter, err, systemBefore)
	}

	// The service can see the admin: platform_app reads platform.admins (it can write nothing there).
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE platform_app"); err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM platform.admins WHERE id = $1", faUserA).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("platform_app sees %d admin rows for the new admin (err %v), want 1", seen, err)
	}
}

func TestFirstAdmin_RefusesASecondRunWithTheSameArguments(t *testing.T) {
	s, admin := faWithFirstAdmin(t)
	before := faSnapshot(t, admin)

	res := faRun(t, s, faArgs(faUserA, faName))
	faMustRefuse(t, "the same command again", res, 1, s, "an admin already exists", "nothing was written", "FIRST", "Phase 7")
	faMustBeUnchanged(t, "the same command again", admin, before)

	// ...and a different display name does not slip through either: the name is not what is checked.
	res = faRun(t, s, faArgs(faUserA, "Someone Else"))
	faMustRefuse(t, "the same login under another name", res, 1, s, "an admin already exists")
	faMustBeUnchanged(t, "the same login under another name", admin, before)
}

func TestFirstAdmin_ASecondDifferentLoginCannotBecomeAnAdminThroughIt(t *testing.T) {
	s, admin := faWithFirstAdmin(t)
	faSeedLogin(t, admin, faUserB, faStr("second@example.com"))
	before := faSnapshot(t, admin)

	res := faRun(t, s, faArgs(faUserB, "Second Admin"))
	faMustRefuse(t, "a second login", res, 1, s, "an admin already exists", "nothing was written")
	if strings.Contains(res.Stderr, "does not exist") {
		t.Fatalf("login B exists, so the refusal must be about the existing admin, not about the login:\n%s", res.Stderr)
	}
	faMustBeUnchanged(t, "a second login", admin, before)
	var n int
	if err := admin.QueryRow(testCtx(t), "SELECT count(*) FROM public.users WHERE id = $1", faUserB).Scan(&n); err != nil || n != 0 {
		t.Fatalf("login B has %d public.users rows (err %v) after a refused command, want 0", n, err)
	}

	// Control: the existing admin is the ONLY thing that stopped it. Take the admin row away (as the superuser; nothing references it)
	// and the very same command line for login B succeeds.
	if _, err := admin.Exec(testCtx(t), "DELETE FROM platform.admins WHERE id = $1", faUserA); err != nil {
		t.Fatalf("remove the first admin for the control: %v", err)
	}
	res = faRun(t, s, faArgs(faUserB, "Second Admin"))
	mustExit(t, res, 0, "login B once no admin exists")
	var id, display string
	if err := admin.QueryRow(testCtx(t), "SELECT id::text, display_name FROM platform.admins").Scan(&id, &display); err != nil || id != faUserB || display != "Second Admin" {
		t.Fatalf("after the control the admins table holds (%s, %q) (err %v), want login B as the only admin", id, display, err)
	}
}

func TestFirstAdmin_RefusesALoginThatDoesNotExistAndCreatesNothing(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserB, faStr("other@example.com")) // another login exists: only the id given is looked up
	before := faSnapshot(t, admin)

	res := faRun(t, s, faArgs(faUserA, faName))
	faMustRefuse(t, "an unknown login", res, 1, s, "does not exist", "nothing was written", "create the login first", "Authentication > Users", "--auth-user-id")
	faMustBeUnchanged(t, "an unknown login", admin, before)
	faRequireFresh(t, admin) // no admin, and public.users still holds only the system user

	// Control: create the login and the same command line succeeds.
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	mustExit(t, faRun(t, s, faArgs(faUserA, faName)), 0, "the same command line once the login exists")
	if n := faCount(t, admin, "platform.admins"); n != 1 {
		t.Fatalf("%d admins after the control, want 1", n)
	}
}

func TestFirstAdmin_RefusesALoginWithoutAnEmailAndCreatesNothing(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, nil)
	before := faSnapshot(t, admin)

	res := faRun(t, s, faArgs(faUserA, faName))
	faMustRefuse(t, "a login with a NULL email", res, 1, s, "no email", "nothing was written", "Authentication > Users")
	faMustBeUnchanged(t, "a login with a NULL email", admin, before)

	if _, err := admin.Exec(testCtx(t), "UPDATE auth.users SET email = '   ' WHERE id = $1", faUserA); err != nil {
		t.Fatal(err)
	}
	res = faRun(t, s, faArgs(faUserA, faName))
	faMustRefuse(t, "a login with a blank email", res, 1, s, "no email", "nothing was written")
	faMustBeUnchanged(t, "a login with a blank email", admin, before)

	// Control: give the login an email (with spaces around it, which the command trims) and the same command line succeeds.
	if _, err := admin.Exec(testCtx(t), "UPDATE auth.users SET email = '  Boss@Example.com ' WHERE id = $1", faUserA); err != nil {
		t.Fatal(err)
	}
	mustExit(t, faRun(t, s, faArgs(faUserA, faName)), 0, "the same command line once the login has an email")
	var email string
	if err := admin.QueryRow(testCtx(t), "SELECT email FROM public.users WHERE id = $1", faUserA).Scan(&email); err != nil || email != faStoredEmailA {
		t.Fatalf("the stored email is %q (err %v), want %q", email, err, faStoredEmailA)
	}
}

// Every bad command line is refused BEFORE the command connects, with exit 2, the flag named and the rule stated, and the database is
// not touched. The control is the last step: the valid command line, on the very same database, succeeds.
func TestFirstAdmin_RefusesBadArgumentsWithTheFlagNamedAndTouchesNothing(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	before := faSnapshot(t, admin)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"a blank name", faArgs(faUserA, "   "), []string{"--display-name", "must not be blank"}},
		{"an empty name", faArgs(faUserA, ""), []string{"--display-name", "must not be blank"}},
		{"a name over 200 bytes", faArgs(faUserA, strings.Repeat("x", 201)), []string{"--display-name", "at most 200 bytes"}},
		{"a control character in the name", faArgs(faUserA, "Bo\tss"), []string{"--display-name", "control characters"}},
		{"the all-zero id", faArgs(faSystemUser, faName), []string{"--auth-user-id", "all-zero uuid"}},
		{"an upper-case id", faArgs(strings.ToUpper(faUserA), faName), []string{"--auth-user-id", "lower-case hyphenated uuid"}},
		{"a malformed id", faArgs("not-a-uuid", faName), []string{"--auth-user-id", "lower-case hyphenated uuid"}},
		{"an id without hyphens", faArgs(strings.ReplaceAll(faUserA, "-", ""), faName), []string{"--auth-user-id", "lower-case hyphenated uuid"}},
		{"no name", []string{"--auth-user-id", faUserA}, []string{"--display-name is required"}},
		{"no id", []string{"--display-name", faName}, []string{"--auth-user-id is required"}},
		{"an unknown flag", []string{"--auth-user-id", faUserA, "--name", faName}, []string{"unknown flag --name"}},
		{"a repeated flag", []string{"--auth-user-id", faUserA, "--auth-user-id", faUserA, "--display-name", faName}, []string{"--auth-user-id was given more than once"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res := faRun(t, s, c.args)
			faMustRefuse(t, c.name, res, 2, s, append([]string{"config error"}, c.want...)...)
			faMustBeUnchanged(t, c.name, admin, before)
		})
	}

	// Control: every refusal above was about the input. The valid command line, on the same database, succeeds.
	mustExit(t, faRun(t, s, faArgs(faUserA, faName)), 0, "the valid command line on the database the refusals left untouched")
	if n := faCount(t, admin, "platform.admins"); n != 1 {
		t.Fatalf("%d admins after the control, want 1", n)
	}
}

// A public.users row that already exists for the login is the same person and is never rewritten: the command goes on to create the admin
// and says in one extra line that the stored email is not the login's. (The control is the first test: no existing row, one output line.)
func TestFirstAdmin_AnExistingPlatformUserRowIsKeptAndANoteIsPrinted(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	if _, err := admin.Exec(testCtx(t), "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'old@example.com')", faUserA); err != nil {
		t.Fatal(err)
	}

	res := faRun(t, s, faArgs(faUserA, faName))
	mustExit(t, res, 0, "create-first-admin for a login that already has a platform user")
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("standard output %q, want a note line and the success line", res.Stdout)
	}
	for _, want := range []string{"note", "old@example.com", faStoredEmailA, "left as it is"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the note %q does not say %q", lines[0], want)
		}
	}
	if want := "create-first-admin: created admin " + faUserA + " (" + faName + ") for " + faStoredEmailA; lines[1] != want {
		t.Fatalf("the last line is %q, want %q", lines[1], want)
	}

	var email string
	var active bool
	if err := admin.QueryRow(testCtx(t), "SELECT email, is_active FROM public.users WHERE id = $1", faUserA).Scan(&email, &active); err != nil {
		t.Fatal(err)
	}
	if email != "old@example.com" || !active {
		t.Fatalf("the existing user row is (%q, active=%v) after the command, want it untouched (\"old@example.com\", true)", email, active)
	}
	if n := faCount(t, admin, "public.users"); n != 2 {
		t.Fatalf("public.users holds %d rows, want the system user and the one existing row", n)
	}
	var id string
	if err := admin.QueryRow(testCtx(t), "SELECT id::text FROM platform.admins").Scan(&id); err != nil || id != faUserA {
		t.Fatalf("the admin is %q (err %v), want %s", id, err, faUserA)
	}
}

// Two invocations at once, for two different logins, create exactly one admin. Contention is PROVEN, not hoped for: the test holds
// the table lock itself, starts both real processes, waits until both are observed in pg_locks waiting for platform.admins, and only
// then lets go. Both therefore reach the lock at the same moment, whichever process the server then favours. The wait is the control
// too: a command that did not take the table lock would never be seen waiting, and the test would fail saying so.
func TestFirstAdmin_TwoInvocationsAtOnceCreateExactlyOneAdmin(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, faStr("a-login@example.com"))
	faSeedLogin(t, admin, faUserB, faStr("b-login@example.com"))
	ctx := testCtx(t)

	holder := s.admin(t)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "LOCK TABLE platform.admins IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		t.Fatalf("take the table lock: %v", err)
	}

	// A generous lock timeout: the processes must still be waiting when the observer has seen both, on a slow runner too.
	env := migEnv(s.DSN, repoMigrationsDir(t), map[string]string{"PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC": "120"})
	runs := map[string]*running{
		faUserA: startMigrator(t, env, append([]string{"create-first-admin"}, faArgs(faUserA, "Admin A")...)...),
		faUserB: startMigrator(t, env, append([]string{"create-first-admin"}, faArgs(faUserB, "Admin B")...)...),
	}

	waiting := 0
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'relation' AND NOT l.granted AND l.relation = 'platform.admins'::regclass
			  AND a.datname = current_database() AND a.application_name = 'platform-migrate'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
	}
	if waiting != 2 {
		_ = tx.Rollback(ctx) // let the processes finish so that their output can be shown
		var out []string
		for id, r := range runs {
			res := r.wait(t)
			out = append(out, fmt.Sprintf("login %s: exit %d\n  stdout: %s\n  stderr: %s", id, res.Code, res.Stdout, res.Stderr))
		}
		sort.Strings(out)
		t.Fatalf("saw %d invocations waiting for the lock on platform.admins, want 2: the command does not take LOCK TABLE platform.admins (or never got that far)\n%s", waiting, strings.Join(out, "\n"))
	}

	if err := tx.Rollback(ctx); err != nil { // release: the server now grants the lock to one of them, the other waits for it
		t.Fatalf("release the table lock: %v", err)
	}
	results := map[string]result{}
	for id, r := range runs {
		results[id] = r.wait(t)
	}

	var winners, losers []string
	for id, res := range results {
		if res.Code == 0 {
			winners = append(winners, id)
		} else {
			losers = append(losers, id)
		}
	}
	describe := func() string {
		var out []string
		for id, res := range results {
			out = append(out, fmt.Sprintf("login %s: exit %d\n  stdout: %s\n  stderr: %s", id, res.Code, res.Stdout, res.Stderr))
		}
		sort.Strings(out)
		return strings.Join(out, "\n")
	}
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("exactly one invocation must succeed and one must be refused, got %d and %d:\n%s", len(winners), len(losers), describe())
	}
	win, lose := winners[0], losers[0]

	loser := results[lose]
	mustExit(t, loser, 1, "the invocation that lost")
	mustContain(t, loser.Stderr, "an admin already exists", "the invocation that lost")
	if loser.Stdout != "" {
		t.Fatalf("the invocation that lost printed %q on standard output", loser.Stdout)
	}
	mustContain(t, results[win].Stdout, "create-first-admin: created admin "+win, "the invocation that won")

	rows, err := admin.Query(ctx, "SELECT id::text FROM platform.admins")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != win {
		t.Fatalf("platform.admins holds %v, want exactly the winner %s\n%s", ids, win, describe())
	}
	var n int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM public.users WHERE id = $1", lose).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the invocation that lost left %d public.users rows (err %v), want 0", n, err)
	}
	if n := faCount(t, admin, "public.users"); n != 2 {
		t.Fatalf("public.users holds %d rows, want the system user and the winner", n)
	}
}
