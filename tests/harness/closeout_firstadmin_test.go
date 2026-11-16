//go:build harness

package harness

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase 2 close-out D6 (owner ruling OD-8): `platform-migrate create-first-admin` REFUSES a login whose platform user already exists and is INACTIVE (is_active = false). The tests drive
// the real binary against a migrated scratch database with the helpers of firstadmin_test.go (fa*): every refusal asserts the exit code (1), the message, an empty standard output and
// an UNCHANGED database, and each is paired with a control that shows the same command line accepted once the cause is removed.

const cfaOldEmail = "old@example.com"

// cfaInsertUser inserts the login's public.users row as the superuser, active or not (the id is the login's id, as users_auth_link demands).
func cfaInsertUser(t *testing.T, c *pgx.Conn, id, email string, active bool) {
	t.Helper()
	if _, err := c.Exec(testCtx(t), "INSERT INTO public.users (id, auth_user_id, email, is_active) VALUES ($1, $1, $2, $3)", id, email, active); err != nil {
		t.Fatalf("seed the platform user %s (active=%v): %v", id, active, err)
	}
}

// cfaSetActive flips the user's activity as the superuser (the platform itself never does: the command does not reactivate anyone).
func cfaSetActive(t *testing.T, c *pgx.Conn, id string, active bool) {
	t.Helper()
	if _, err := c.Exec(testCtx(t), "UPDATE public.users SET is_active = $2 WHERE id = $1", id, active); err != nil {
		t.Fatalf("set is_active=%v on %s: %v", active, id, err)
	}
}

// TestCloseoutFirstAdmin_RefusesAnInactiveExistingUserAndWritesNothing: the login exists in auth.users and already has a public.users row that is INACTIVE. The command exits 1 with the
// login named, the inactivity, "nothing was written" and what to do (reactivate it or use a different login), says NOTHING about an admin existing (none does), prints nothing on standard
// output, and leaves both tables byte for byte as they were: no admin, the inactive row not touched, no second user row.
// The paired control is the same command line once the user is ACTIVE: it succeeds, prints the note about the stored email, keeps the row as it was and creates the one admin.
// It fails if an inactive user is made an admin, if the row is rewritten or reactivated, or if the refusal is worded as another one.
func TestCloseoutFirstAdmin_RefusesAnInactiveExistingUserAndWritesNothing(t *testing.T) {
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	faSeedLogin(t, admin, faUserA, faStr(faAuthEmailA))
	cfaInsertUser(t, admin, faUserA, cfaOldEmail, false)
	before := faSnapshot(t, admin)
	if !strings.Contains(before, "active=false") {
		t.Fatalf("precondition: the seeded user is not inactive:\n%s", before)
	}

	res := faRun(t, s, faArgs(faUserA, faName))
	faMustRefuse(t, "an inactive existing platform user", res, 1, s,
		faUserA, "inactive", "is_active", "nothing was written", "reactivate", "different login", "--auth-user-id")
	for _, banned := range []string{"admin already exists", "FIRST", "Phase 7", "does not exist", "no email"} {
		if strings.Contains(res.Stderr, banned) {
			t.Fatalf("the refusal for an inactive user says %q, which is another refusal's wording (no admin exists):\n%s", banned, res.Stderr)
		}
	}
	faMustBeUnchanged(t, "an inactive existing platform user", admin, before)
	if n := faCount(t, admin, "platform.admins"); n != 0 {
		t.Fatalf("platform.admins holds %d row(s) after a refused command, want 0", n)
	}
	if n := faCount(t, admin, "public.users"); n != 2 {
		t.Fatalf("public.users holds %d rows, want the system user and the one inactive row", n)
	}

	// the same answer a second time (nothing was left behind that changes it), and with the other spelling of the flags
	res = faRun(t, s, []string{"--auth-user-id=" + faUserA, "--display-name=" + faName})
	faMustRefuse(t, "the same command line again", res, 1, s, faUserA, "inactive", "nothing was written")
	faMustBeUnchanged(t, "the same command line again", admin, before)

	// THE CONTROL: reactivate the user (the operator's act, here the superuser's) and the same command line goes through; the stored email is kept and the note says so.
	cfaSetActive(t, admin, faUserA, true)
	res = faRun(t, s, faArgs(faUserA, faName))
	mustExit(t, res, 0, "the same command line once the platform user is active")
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "note") || !strings.Contains(lines[0], cfaOldEmail) {
		t.Fatalf("standard output %q, want the note about the stored email and the success line", res.Stdout)
	}
	if want := "create-first-admin: created admin " + faUserA + " (" + faName + ") for " + faStoredEmailA; lines[1] != want {
		t.Fatalf("the last line is %q, want %q", lines[1], want)
	}
	var email string
	var active bool
	if err := admin.QueryRow(testCtx(t), "SELECT email, is_active FROM public.users WHERE id = $1", faUserA).Scan(&email, &active); err != nil || email != cfaOldEmail || !active {
		t.Fatalf("the user row is (%q, active=%v) after the control (err %v), want it untouched (%q, true)", email, active, err, cfaOldEmail)
	}
	if n := faCount(t, admin, "platform.admins"); n != 1 {
		t.Fatalf("%d admins after the control, want 1", n)
	}
}

// TestCloseoutFirstAdmin_TheOtherRefusalsStillComeFirst: an inactive user does not change the order of the refusals. (1) The system user id (whose row IS inactive, by construction) is
// refused at the command line (exit 2, before the command connects): the message is the all-zero-uuid rule, not the inactivity. (2) Once an admin exists, any later run is the "an admin already
// exists" refusal, for the admin's own login after it was deactivated and for another login whose user is inactive: the inactivity is never reported over it.
// Every refusal leaves the database unchanged, and the control is the first test's reactivation.
func TestCloseoutFirstAdmin_TheOtherRefusalsStillComeFirst(t *testing.T) {
	// (1) the system user, whose row is inactive by construction (platform_0001): the premise is asserted, not assumed
	s, admin := migratedScratch(t)
	faRequireFresh(t, admin)
	var systemActive bool
	if err := admin.QueryRow(testCtx(t), "SELECT is_active FROM public.users WHERE id = $1", faSystemUser).Scan(&systemActive); err != nil || systemActive {
		t.Fatalf("precondition: the system user's is_active is %v (err %v), want false: the test is about an inactive row", systemActive, err)
	}
	before := faSnapshot(t, admin)
	res := faRun(t, s, faArgs(faSystemUser, faName))
	faMustRefuse(t, "the system user, inactive", res, 2, s, "config error", "--auth-user-id", "all-zero uuid")
	if strings.Contains(res.Stderr, "inactive") {
		t.Fatalf("the system user was refused for inactivity, not by the command-line rule:\n%s", res.Stderr)
	}
	faMustBeUnchanged(t, "the system user, inactive", admin, before)
	if n := faCount(t, admin, "platform.admins"); n != 0 {
		t.Fatalf("%d admins after the refused system user, want 0", n)
	}

	// (2) an admin exists: it wins over the inactivity, for the admin's own login and for another one
	s2, admin2 := faWithFirstAdmin(t)
	faSeedLogin(t, admin2, faUserB, faStr("second@example.com"))
	cfaInsertUser(t, admin2, faUserB, "second@example.com", false)
	cfaSetActive(t, admin2, faUserA, false) // the first admin's own user is deactivated afterwards
	before2 := faSnapshot(t, admin2)
	for _, id := range []string{faUserA, faUserB} {
		res := faRun(t, s2, faArgs(id, faName))
		faMustRefuse(t, "an admin exists and the user "+id+" is inactive", res, 1, s2, "an admin already exists", "nothing was written")
		if strings.Contains(res.Stderr, "inactive") {
			t.Fatalf("with an admin present the refusal for %s names the inactivity:\n%s", id, res.Stderr)
		}
		faMustBeUnchanged(t, "an admin exists and the user "+id+" is inactive", admin2, before2)
	}
}
