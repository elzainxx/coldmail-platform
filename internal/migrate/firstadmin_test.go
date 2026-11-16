package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// These tests need no database. They prove the validation, the ORDER of the steps and every refusal of create-first-admin against a fake
// of the transaction that also enforces which role may take which step. The command against a real PostgreSQL (the lock, the
// constraints, the exit statuses of the real binary) is proven in tests/harness (firstadmin_test.go).

const (
	adminTestID    = "123e4567-e89b-12d3-a456-426614174000"
	adminTestEmail = "boss@example.com"
	fakeOwner      = "owner"
	fakeMigrator   = "migrator"
)

// fakeAdminTx is the transaction of create-first-admin as a test double: it records every call, can fail one named step, and refuses a
// step taken as the wrong role (the lock and every write need platform_owner; reading auth.users needs the migrating identity, because
// platform_owner has no grant on schema auth).
type fakeAdminTx struct {
	role  string
	calls []string

	admins       bool    // platform.admins already holds a row
	login        bool    // auth.users has the login
	loginEmail   *string // its email; nil is NULL
	userExists   bool    // public.users already has the login's row...
	userEmail    string  // ...with this email...
	userInactive bool    // ...and it is inactive (is_active = false)

	appearsOnInsert bool // another writer creates the user row between the read and our insert: the insert then finds it (conflict) although the read did not

	failAt  string // the step that fails
	failErr error

	committed  bool
	userWrites []string
	adminNames []string
}

func newFakeAdminTx(email string) *fakeAdminTx {
	return &fakeAdminTx{role: fakeMigrator, login: true, loginEmail: &email}
}

func (f *fakeAdminTx) step(name, needRole string) error {
	f.calls = append(f.calls, name)
	if f.failAt == name {
		return f.failErr
	}
	if needRole != "" && f.role != needRole {
		return fmt.Errorf("fake: %s ran as %s but needs %s (permission denied)", name, f.role, needRole)
	}
	return nil
}

func (f *fakeAdminTx) AsOwner(context.Context) error {
	if err := f.step("as-owner", ""); err != nil {
		return err
	}
	f.role = fakeOwner
	return nil
}

func (f *fakeAdminTx) AsMigrator(context.Context) error {
	if err := f.step("as-migrator", ""); err != nil {
		return err
	}
	f.role = fakeMigrator
	return nil
}

func (f *fakeAdminTx) LockAdmins(context.Context) error { return f.step("lock-admins", fakeOwner) }

func (f *fakeAdminTx) AdminExists(context.Context) (bool, error) {
	if err := f.step("admin-exists", fakeOwner); err != nil {
		return false, err
	}
	return f.admins, nil
}

func (f *fakeAdminTx) AuthEmail(_ context.Context, _ string) (string, bool, error) {
	if err := f.step("auth-email", fakeMigrator); err != nil {
		return "", false, err
	}
	switch {
	case !f.login:
		return "", false, nil
	case f.loginEmail == nil:
		return "", true, nil
	}
	return *f.loginEmail, true, nil
}

func (f *fakeAdminTx) InsertUser(_ context.Context, id, _ string) (bool, error) {
	if err := f.step("insert-user", fakeOwner); err != nil {
		return false, err
	}
	if f.appearsOnInsert {
		f.userExists = true
	}
	if f.userExists {
		return false, nil
	}
	f.userWrites = append(f.userWrites, id)
	return true, nil
}

func (f *fakeAdminTx) UserState(_ context.Context, _ string) (existingUser, bool, error) {
	if err := f.step("user-state", fakeOwner); err != nil {
		return existingUser{}, false, err
	}
	if !f.userExists {
		return existingUser{}, false, nil
	}
	return existingUser{Email: f.userEmail, Active: !f.userInactive}, true, nil
}

func (f *fakeAdminTx) InsertAdmin(_ context.Context, _, name string) error {
	if err := f.step("insert-admin", fakeOwner); err != nil {
		return err
	}
	f.adminNames = append(f.adminNames, name)
	return nil
}

func (f *fakeAdminTx) Commit(context.Context) error {
	if err := f.step("commit", ""); err != nil {
		return err
	}
	f.committed = true
	return nil
}

func (f *fakeAdminTx) sequence() string { return strings.Join(f.calls, " > ") }

// wroteSomething reports whether the fake saw any step that writes or ends the transaction successfully.
func (f *fakeAdminTx) wroteSomething() bool {
	return f.committed || len(f.userWrites) > 0 || len(f.adminNames) > 0
}

func validAdminInput() FirstAdminInput {
	return FirstAdminInput{AuthUserID: adminTestID, DisplayName: "Ada Boss"}
}

// ---- validation ----

// Every bad input is refused with the flag named and the rule stated, as a *ConfigError (exit 2); the control rows are accepted, so the
// table can say yes as well as no.
func TestFirstAdminInput_Validate(t *testing.T) {
	name200 := strings.Repeat("x", MaxDisplayNameBytes)
	cases := []struct {
		label, id, display string
		want               string // "" means accepted; otherwise a text the error must contain
	}{
		// accepted (the control: a validator that refuses everything would pass the rows below)
		{"a plain name", adminTestID, "Ada Boss", ""},
		{"exactly 200 bytes", adminTestID, name200, ""},
		{"exactly 200 bytes of two-byte characters", adminTestID, strings.Repeat("é", 100), ""},
		{"a name with accents and inner spaces", adminTestID, "Zoë Ünal-O'Brien", ""},
		{"a name with spaces around it (stored without them)", adminTestID, "  Ada Boss ", ""},
		{"another valid id", "ffffffff-ffff-4fff-8fff-fffffffffff0", "Ada", ""},

		// --auth-user-id
		{"an empty id", "", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"not a uuid", "not-a-uuid", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"an upper-case uuid", "123E4567-E89B-12D3-A456-426614174000", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a mixed-case uuid", "123e4567-E89b-12d3-a456-426614174000", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid without hyphens", "123e4567e89b12d3a456426614174000", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid in braces", "{123e4567-e89b-12d3-a456-426614174000}", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a urn", "urn:uuid:123e4567-e89b-12d3-a456-426614174000", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid with a leading space", " 123e4567-e89b-12d3-a456-426614174000", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid with a trailing newline", "123e4567-e89b-12d3-a456-426614174000\n", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid one digit short", "123e4567-e89b-12d3-a456-42661417400", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a uuid with a non-hex letter", "123e4567-e89b-12d3-a456-42661417400g", "Ada", FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"the all-zero uuid", "00000000-0000-0000-0000-000000000000", "Ada", FlagAuthUserID + " must not be the all-zero uuid"},

		// --display-name
		{"an empty name", adminTestID, "", FlagDisplayName + " must not be blank"},
		{"a name of spaces", adminTestID, "    ", FlagDisplayName + " must not be blank"},
		{"a name of a no-break space", adminTestID, "\u00a0", FlagDisplayName + " must not be blank"},
		{"a name of a tab", adminTestID, "\t", FlagDisplayName + " must not be blank"},
		{"a tab inside the name", adminTestID, "Bo\tss", FlagDisplayName + " must not contain control characters"},
		{"a trailing newline", adminTestID, "Boss\n", FlagDisplayName + " must not contain control characters"},
		{"a NUL byte", adminTestID, "Bo\x00ss", FlagDisplayName + " must not contain control characters"},
		{"a DEL", adminTestID, "Bo\x7fss", FlagDisplayName + " must not contain control characters"},
		{"a C1 control (next line)", adminTestID, "Bo\u0085ss", FlagDisplayName + " must not contain control characters"},
		{"invalid UTF-8", adminTestID, "Bo\xff\xfess", FlagDisplayName + " must be valid UTF-8"},
		{"201 bytes", adminTestID, name200 + "x", FlagDisplayName + " must be at most 200 bytes"},
		{"101 two-byte characters (202 bytes, 101 characters)", adminTestID, strings.Repeat("é", 101), FlagDisplayName + " must be at most 200 bytes"},
	}
	accepted, refused := 0, 0
	for _, c := range cases {
		err := FirstAdminInput{AuthUserID: c.id, DisplayName: c.display}.Validate()
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.label, err)
			}
			accepted++
			continue
		}
		refused++
		var ce *ConfigError
		switch {
		case err == nil:
			t.Errorf("%s: accepted, want %q", c.label, c.want)
		case !errors.As(err, &ce):
			t.Errorf("%s: error is %T (%v), want *ConfigError so the command exits 2", c.label, err, err)
		case !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q does not say %q", c.label, err, c.want)
		case c.id != "" && strings.Contains(err.Error(), c.id):
			t.Errorf("%s: the error echoes the id it refused: %q", c.label, err)
		}
	}
	if accepted < 5 || refused < 20 {
		t.Fatalf("the table covered %d accepted and %d refused inputs: it lost its rows", accepted, refused)
	}
	if got := (FirstAdminInput{DisplayName: "  Ada Boss "}).Name(); got != "Ada Boss" {
		t.Errorf("Name() = %q, want the display name without the spaces around it", got)
	}
}

func TestParseFirstAdminArgs(t *testing.T) {
	ok := []struct {
		label string
		args  []string
		want  FirstAdminInput
	}{
		{"flag then value", []string{FlagAuthUserID, adminTestID, FlagDisplayName, "Ada Boss"}, FirstAdminInput{adminTestID, "Ada Boss"}},
		{"the other order", []string{FlagDisplayName, "Ada Boss", FlagAuthUserID, adminTestID}, FirstAdminInput{adminTestID, "Ada Boss"}},
		{"flag=value", []string{FlagAuthUserID + "=" + adminTestID, FlagDisplayName + "=Ada Boss"}, FirstAdminInput{adminTestID, "Ada Boss"}},
		{"a value that contains =", []string{FlagAuthUserID, adminTestID, FlagDisplayName + "=Ada=Boss"}, FirstAdminInput{adminTestID, "Ada=Boss"}},
		{"a value that starts with one dash", []string{FlagAuthUserID, adminTestID, FlagDisplayName, "-Ada"}, FirstAdminInput{adminTestID, "-Ada"}},
	}
	for _, c := range ok {
		got, err := ParseFirstAdminArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.label, got, err, c.want)
		}
	}

	bad := []struct {
		label string
		args  []string
		want  string
	}{
		{"no arguments", nil, FlagAuthUserID + " is required"},
		{"only the id", []string{FlagAuthUserID, adminTestID}, FlagDisplayName + " is required"},
		{"only the name", []string{FlagDisplayName, "Ada"}, FlagAuthUserID + " is required"},
		{"the old spelling --name", []string{FlagAuthUserID, adminTestID, "--name", "Ada"}, "unknown flag --name"},
		{"an unknown flag with a value", []string{FlagAuthUserID, adminTestID, FlagDisplayName, "Ada", "--admin=yes"}, "unknown flag --admin"},
		{"a positional argument", []string{adminTestID}, "no other arguments"},
		{"a single-dash flag", []string{"-auth-user-id", adminTestID}, "no other arguments"},
		{"the id twice", []string{FlagAuthUserID, adminTestID, FlagAuthUserID, adminTestID, FlagDisplayName, "Ada"}, FlagAuthUserID + " was given more than once"},
		{"the name twice", []string{FlagDisplayName, "A", FlagDisplayName + "=B", FlagAuthUserID, adminTestID}, FlagDisplayName + " was given more than once"},
		{"the last flag without a value", []string{FlagDisplayName, "Ada", FlagAuthUserID}, FlagAuthUserID + " needs a value"},
		{"a forgotten value (the next token is a flag)", []string{FlagAuthUserID, FlagDisplayName, "Ada"}, FlagAuthUserID + " needs a value"},
		{"an empty id after =", []string{FlagAuthUserID + "=", FlagDisplayName, "Ada"}, FlagAuthUserID + " must be a lower-case hyphenated uuid"},
		{"a blank name reaches the validation", []string{FlagAuthUserID, adminTestID, FlagDisplayName, "  "}, FlagDisplayName + " must not be blank"},
		{"the all-zero id reaches the validation", []string{FlagAuthUserID, "00000000-0000-0000-0000-000000000000", FlagDisplayName, "Ada"}, FlagAuthUserID + " must not be the all-zero uuid"},
	}
	for _, c := range bad {
		got, err := ParseFirstAdminArgs(c.args)
		var ce *ConfigError
		switch {
		case err == nil:
			t.Errorf("%s: accepted %+v, want %q", c.label, got, c.want)
		case !errors.As(err, &ce):
			t.Errorf("%s: error is %T, want *ConfigError so the command exits 2", c.label, err)
		case !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q does not say %q", c.label, err, c.want)
		case got != (FirstAdminInput{}):
			t.Errorf("%s: a refused command line returned input %+v", c.label, got)
		}
	}
}

// ---- the steps ----

func TestCreateFirstAdmin_SuccessTakesTheStepsInOrderAndPrintsOneLine(t *testing.T) {
	f := newFakeAdminTx("boss@example.com")
	var out bytes.Buffer
	in := FirstAdminInput{AuthUserID: adminTestID, DisplayName: "  Ada Boss "}
	if err := createFirstAdmin(context.Background(), f, in, &out); err != nil {
		t.Fatalf("createFirstAdmin: %v\ncalls: %s", err, f.sequence())
	}
	// The order is the contract: the role switch, the table lock, the admin check, then the login is read as the migrating identity and
	// the writes run as the owner again (the existing user row is read first, then inserted if there is none), and the commit is last.
	want := "as-owner > lock-admins > admin-exists > as-migrator > auth-email > as-owner > user-state > insert-user > insert-admin > commit"
	if got := f.sequence(); got != want {
		t.Fatalf("calls:\n got  %s\n want %s", got, want)
	}
	if !f.committed || len(f.userWrites) != 1 || f.userWrites[0] != adminTestID || len(f.adminNames) != 1 || f.adminNames[0] != "Ada Boss" {
		t.Errorf("writes: committed=%v users=%v admins=%q, want one user and one admin named %q, committed", f.committed, f.userWrites, f.adminNames, "Ada Boss")
	}
	if wantOut := "create-first-admin: created admin " + adminTestID + " (Ada Boss) for boss@example.com\n"; out.String() != wantOut {
		t.Errorf("output %q, want exactly %q", out.String(), wantOut)
	}
}

// The role switch is the first thing that happens and the lock the second, and each step runs as the role it needs. The fake refuses a
// step taken as the wrong role, so a command that skipped or reordered the switches would fail the success test above; the first half of
// this test shows the fake really does refuse.
func TestCreateFirstAdmin_TheRoleSwitchComesFirstAndEveryStepRunsAsItsRole(t *testing.T) {
	ctx := context.Background()

	// Control: the fake bites. As the migrating identity the lock is refused, as the owner the auth.users read is refused.
	f := newFakeAdminTx(adminTestEmail)
	if err := f.LockAdmins(ctx); err == nil || !strings.Contains(err.Error(), "needs "+fakeOwner) {
		t.Fatalf("control: the lock taken before the role switch should be refused, got %v", err)
	}
	_ = f.AsOwner(ctx)
	if _, _, err := f.AuthEmail(ctx, adminTestID); err == nil || !strings.Contains(err.Error(), "needs "+fakeMigrator) {
		t.Fatalf("control: reading auth.users as the owner should be refused, got %v", err)
	}

	// Every path through the command starts with the switch and then the lock.
	scenarios := map[string]func(*fakeAdminTx){
		"success":         func(*fakeAdminTx) {},
		"an admin exists": func(f *fakeAdminTx) { f.admins = true },
		"no such login":   func(f *fakeAdminTx) { f.login = false },
		"no email":        func(f *fakeAdminTx) { f.loginEmail = nil },
	}
	for name, setup := range scenarios {
		f := newFakeAdminTx(adminTestEmail)
		setup(f)
		_ = createFirstAdmin(ctx, f, validAdminInput(), &bytes.Buffer{})
		if len(f.calls) < 3 || f.calls[0] != "as-owner" || f.calls[1] != "lock-admins" || f.calls[2] != "admin-exists" {
			t.Errorf("%s: the calls begin %v, want as-owner, lock-admins, admin-exists", name, f.calls)
		}
	}

	// A step that cannot switch role stops the command before it touches a table.
	f = newFakeAdminTx(adminTestEmail)
	f.failAt, f.failErr = "as-owner", errors.New("must be able to SET ROLE")
	if err := createFirstAdmin(ctx, f, validAdminInput(), &bytes.Buffer{}); !errors.Is(err, f.failErr) || f.sequence() != "as-owner" {
		t.Errorf("a failed role switch: err = %v, calls = %s; want the error and no other call", err, f.sequence())
	}
}

func TestCreateFirstAdmin_RefusesBeforeWritingWhenAnAdminExists(t *testing.T) {
	f := newFakeAdminTx(adminTestEmail)
	f.admins = true
	f.login = false // the login is missing too: the admin check comes first, so that is the refusal the operator sees
	var out bytes.Buffer
	err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
	if !errors.Is(err, ErrAdminExists) {
		t.Fatalf("err = %v, want ErrAdminExists", err)
	}
	for _, want := range []string{"already exists", "nothing was written", "FIRST", "Phase 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if got, want := f.sequence(), "as-owner > lock-admins > admin-exists"; got != want {
		t.Errorf("calls:\n got  %s\n want %s (the refusal must come before the login is read and before any write)", got, want)
	}
	if f.wroteSomething() || out.Len() != 0 {
		t.Errorf("a refused command wrote %+v or printed %q", f, out.String())
	}

	// Control: with no admin the same command line goes through, so the refusal above is the admin check and nothing else.
	c := newFakeAdminTx(adminTestEmail)
	if err := createFirstAdmin(context.Background(), c, validAdminInput(), &bytes.Buffer{}); err != nil || !c.committed {
		t.Fatalf("control: err = %v, committed = %v, want success", err, c.committed)
	}
}

func TestCreateFirstAdmin_RefusesWhenTheLoginDoesNotExist(t *testing.T) {
	f := newFakeAdminTx(adminTestEmail)
	f.login = false
	var out bytes.Buffer
	err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
	if !errors.Is(err, ErrAuthUserMissing) {
		t.Fatalf("err = %v, want ErrAuthUserMissing", err)
	}
	for _, want := range []string{"does not exist", "nothing was written", "create the login first", "Supabase dashboard > Authentication > Users", FlagAuthUserID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if f.wroteSomething() || out.Len() != 0 {
		t.Errorf("a refused command wrote %+v or printed %q", f, out.String())
	}
	// It read the login (so the refusal is about the login), and it did so after the admin check.
	if got, want := f.sequence(), "as-owner > lock-admins > admin-exists > as-migrator > auth-email > as-owner"; got != want {
		t.Errorf("calls:\n got  %s\n want %s", got, want)
	}

	// Control: the login exists, the same command line succeeds.
	f.login = true
	f.calls = nil
	if err := createFirstAdmin(context.Background(), f, validAdminInput(), &out); err != nil || !f.committed {
		t.Fatalf("control: err = %v, committed = %v, want success", err, f.committed)
	}
}

func TestCreateFirstAdmin_RefusesALoginWithoutAnEmail(t *testing.T) {
	empty, spaces, tab := "", "   ", "\t"
	for label, email := range map[string]*string{"NULL": nil, "empty": &empty, "spaces": &spaces, "a tab": &tab} {
		f := newFakeAdminTx(adminTestEmail)
		f.loginEmail = email
		var out bytes.Buffer
		err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
		if !errors.Is(err, ErrAuthEmailMissing) {
			t.Errorf("%s: err = %v, want ErrAuthEmailMissing", label, err)
			continue
		}
		for _, want := range []string{"no email", "nothing was written", "Supabase dashboard > Authentication > Users"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not say %q", label, err, want)
			}
		}
		if f.wroteSomething() || out.Len() != 0 {
			t.Errorf("%s: a refused command wrote %+v or printed %q", label, f, out.String())
		}
	}
	// Control: an email makes the same login acceptable.
	f := newFakeAdminTx(adminTestEmail)
	if err := createFirstAdmin(context.Background(), f, validAdminInput(), &bytes.Buffer{}); err != nil || !f.committed {
		t.Fatalf("control: err = %v, committed = %v, want success", err, f.committed)
	}
}

// A public.users row that already exists is the same person (id = auth_user_id) and is never rewritten: the command goes on to create
// the admin, and says so when the stored email is not the login's.
func TestCreateFirstAdmin_AnExistingPlatformUserIsKeptAndANoteSaysIfItsEmailDiffers(t *testing.T) {
	// the same email: no note, one line
	same := newFakeAdminTx(adminTestEmail)
	same.userExists, same.userEmail = true, adminTestEmail
	var out bytes.Buffer
	if err := createFirstAdmin(context.Background(), same, validAdminInput(), &out); err != nil {
		t.Fatal(err)
	}
	if want := "create-first-admin: created admin " + adminTestID + " (Ada Boss) for " + adminTestEmail + "\n"; out.String() != want {
		t.Errorf("same email: output %q, want exactly %q", out.String(), want)
	}
	if len(same.userWrites) != 0 || len(same.adminNames) != 1 || !same.committed {
		t.Errorf("same email: users written %v, admins %v, committed %v; want no user written, one admin, committed", same.userWrites, same.adminNames, same.committed)
	}

	// a different email: the note comes first, the success line is last and there is still exactly one
	diff := newFakeAdminTx(adminTestEmail)
	diff.userExists, diff.userEmail = true, "old@example.com"
	out.Reset()
	if err := createFirstAdmin(context.Background(), diff, validAdminInput(), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("different email: output %q, want a note and the success line", out.String())
	}
	for _, want := range []string{"note", "old@example.com", adminTestEmail, "left as it is", adminTestID} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the note %q does not say %q", lines[0], want)
		}
	}
	if want := "create-first-admin: created admin " + adminTestID + " (Ada Boss) for " + adminTestEmail; lines[1] != want {
		t.Errorf("the last line is %q, want %q", lines[1], want)
	}
	if strings.Count(out.String(), "created admin") != 1 {
		t.Errorf("output %q has more than one success line", out.String())
	}
	if len(diff.userWrites) != 0 || !diff.committed {
		t.Errorf("different email: users written %v, committed %v; the existing row must not be written", diff.userWrites, diff.committed)
	}
}

// OD-8 (Phase 2 close-out D6): a public.users row that exists for the login and is INACTIVE is refused, with the login named, nothing written and no claim that an admin
// exists. The refusal comes after the checks that precede it and BEFORE any write, so the call sequence ends at the read of the user. The control is the same login with an ACTIVE
// row, which is used as it is, so the refusal is the inactivity and nothing else.
func TestCreateFirstAdmin_RefusesAnInactiveExistingUserBeforeAnyWrite(t *testing.T) {
	for label, email := range map[string]string{"the same email": adminTestEmail, "another email": "old@example.com"} {
		f := newFakeAdminTx(adminTestEmail)
		f.userExists, f.userEmail, f.userInactive = true, email, true
		var out bytes.Buffer
		err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
		var iu *InactiveUserError
		if !errors.Is(err, ErrUserInactive) || !errors.As(err, &iu) || iu.AuthUserID != adminTestID {
			t.Errorf("%s: err = %v, want an *InactiveUserError for %s that matches ErrUserInactive", label, err, adminTestID)
			continue
		}
		for _, want := range []string{adminTestID, "inactive", "is_active", "nothing was written", "reactivate", "different login", FlagAuthUserID} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not say %q", label, err, want)
			}
		}
		// it must not say an admin exists (none does), and it is not the admin refusal
		if errors.Is(err, ErrAdminExists) || strings.Contains(err.Error(), "admin already exists") || strings.Contains(err.Error(), "FIRST") {
			t.Errorf("%s: the refusal talks about an existing admin: %q", label, err)
		}
		if got, want := f.sequence(), "as-owner > lock-admins > admin-exists > as-migrator > auth-email > as-owner > user-state"; got != want {
			t.Errorf("%s: calls:\n got  %s\n want %s (the refusal must come before the user row is inserted and before the admin is)", label, got, want)
		}
		if f.wroteSomething() || out.Len() != 0 {
			t.Errorf("%s: a refused command wrote %+v or printed %q", label, f, out.String())
		}
	}

	// The control: the same login with an ACTIVE row goes through and the row is kept (the email note is the existing behaviour).
	c := newFakeAdminTx(adminTestEmail)
	c.userExists, c.userEmail = true, "old@example.com"
	var out bytes.Buffer
	if err := createFirstAdmin(context.Background(), c, validAdminInput(), &out); err != nil || !c.committed || len(c.userWrites) != 0 || len(c.adminNames) != 1 {
		t.Fatalf("control: err = %v, committed = %v, user writes %v, admins %v; want an active existing user used as it is", err, c.committed, c.userWrites, c.adminNames)
	}
	if want := "create-first-admin: created admin " + adminTestID + " (Ada Boss) for " + adminTestEmail + "\n"; !strings.HasSuffix(out.String(), want) {
		t.Errorf("control: output %q does not end with the success line", out.String())
	}

	// The checks that come first still win: an existing admin is the refusal when the user is inactive too, and a missing login is.
	both := newFakeAdminTx(adminTestEmail)
	both.admins, both.userExists, both.userInactive = true, true, true
	if err := createFirstAdmin(context.Background(), both, validAdminInput(), &bytes.Buffer{}); !errors.Is(err, ErrAdminExists) || errors.Is(err, ErrUserInactive) {
		t.Errorf("an admin and an inactive user: err = %v, want only ErrAdminExists", err)
	}
	gone := newFakeAdminTx(adminTestEmail)
	gone.login, gone.userExists, gone.userInactive = false, true, true
	if err := createFirstAdmin(context.Background(), gone, validAdminInput(), &bytes.Buffer{}); !errors.Is(err, ErrAuthUserMissing) {
		t.Errorf("no login and an inactive user: err = %v, want ErrAuthUserMissing", err)
	}
}

// The same refusal when the row is not there at the first read and lands (from another writer) before our insert: the insert finds it, the command reads it again and refuses an
// inactive one. Nothing has been written at that point, so the refusal leaves the database as it was. The control: the same race with an active row goes through.
func TestCreateFirstAdmin_ARowThatAppearsBetweenTheReadAndTheInsertIsJudgedToo(t *testing.T) {
	f := newFakeAdminTx(adminTestEmail)
	f.appearsOnInsert, f.userEmail, f.userInactive = true, adminTestEmail, true
	var out bytes.Buffer
	err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
	if !errors.Is(err, ErrUserInactive) {
		t.Fatalf("err = %v, want ErrUserInactive", err)
	}
	if got, want := f.sequence(), "as-owner > lock-admins > admin-exists > as-migrator > auth-email > as-owner > user-state > insert-user > user-state"; got != want {
		t.Errorf("calls:\n got  %s\n want %s", got, want)
	}
	if f.wroteSomething() || out.Len() != 0 {
		t.Errorf("a refused command wrote %+v or printed %q", f, out.String())
	}

	c := newFakeAdminTx(adminTestEmail)
	c.appearsOnInsert, c.userEmail = true, "old@example.com"
	out.Reset()
	if err := createFirstAdmin(context.Background(), c, validAdminInput(), &out); err != nil || !c.committed || len(c.userWrites) != 0 {
		t.Fatalf("control: err = %v, committed = %v, user writes %v; want the active row that appeared to be used as it is", err, c.committed, c.userWrites)
	}
	if !strings.Contains(out.String(), "note") || !strings.Contains(out.String(), "old@example.com") {
		t.Errorf("control: output %q lacks the note about the other stored email", out.String())
	}
}

// A database error at ANY step stops the command: the error comes back (so the command exits 1), nothing later runs, nothing is
// committed and above all nothing is printed, because the success line is only ever printed after the commit.
func TestCreateFirstAdmin_ADatabaseErrorAtAnyStepStopsItWithNothingPrinted(t *testing.T) {
	steps := []string{"as-owner", "lock-admins", "admin-exists", "as-migrator", "auth-email", "user-state", "insert-user", "insert-admin", "commit"}
	for _, step := range steps {
		f := newFakeAdminTx(adminTestEmail)
		f.failAt, f.failErr = step, errors.New("boom at "+step)
		var out bytes.Buffer
		err := createFirstAdmin(context.Background(), f, validAdminInput(), &out)
		if !errors.Is(err, f.failErr) {
			t.Errorf("%s: err = %v, want the step's own error", step, err)
		}
		if last := f.calls[len(f.calls)-1]; last != step {
			t.Errorf("%s: the last call was %s: a step after the failure ran (%s)", step, last, f.sequence())
		}
		if f.committed || out.Len() != 0 {
			t.Errorf("%s: committed = %v, printed %q: a failed run must commit and print nothing", step, f.committed, out.String())
		}
	}
	// Control: with no failure the same fake commits and prints, so the silence above is the failure's doing.
	f := newFakeAdminTx(adminTestEmail)
	var out bytes.Buffer
	if err := createFirstAdmin(context.Background(), f, validAdminInput(), &out); err != nil || !f.committed || out.Len() == 0 {
		t.Fatalf("control: err = %v, committed = %v, printed %q", err, f.committed, out.String())
	}
}

// The command validates BEFORE it connects: the DSN is unreachable, so an attempt to connect shows up as "connect:".
func TestRunnerCreateFirstAdmin_ValidatesBeforeConnecting(t *testing.T) {
	r := &Runner{Cfg: Config{DatabaseURL: unreachableDSN, LockTimeoutSec: 1, StatementTimeoutSec: 1}, Out: &bytes.Buffer{}}
	for _, in := range []FirstAdminInput{
		{AuthUserID: "", DisplayName: "Ada"},
		{AuthUserID: "00000000-0000-0000-0000-000000000000", DisplayName: "Ada"},
		{AuthUserID: strings.ToUpper(adminTestID), DisplayName: "Ada"},
		{AuthUserID: adminTestID, DisplayName: "   "},
		{AuthUserID: adminTestID, DisplayName: "Bo\tss"},
	} {
		err := r.CreateFirstAdmin(context.Background(), in)
		var ce *ConfigError
		if !errors.As(err, &ce) {
			t.Errorf("%+v: err = %v, want a *ConfigError", in, err)
			continue
		}
		if strings.Contains(err.Error(), "connect") {
			t.Errorf("%+v: it connected before validating: %v", in, err)
		}
	}
	// Control: valid input goes on to the connection and fails there, with neither secret in the message.
	err := r.CreateFirstAdmin(context.Background(), validAdminInput())
	if err == nil || !strings.HasPrefix(err.Error(), "connect: ") {
		t.Fatalf("control: err = %v, want a connect failure", err)
	}
	assertNoLeak(t, "the create-first-admin error", err.Error(), unreachableDSN)
}

// ---- the reference document ----

// missingCommands lists the commands the reference document does not have a row for in its Commands table: a line that starts with the
// command in code font, alone (`up`) or followed by its arguments (`down <NNNN>`).
func missingCommands(md string, commands []string) []string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var missing []string
	for _, c := range commands {
		found := false
		for _, l := range lines {
			if strings.HasPrefix(l, "| `"+c+"`") || strings.HasPrefix(l, "| `"+c+" ") {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, c)
		}
	}
	return missing
}

// Every command in Commands() has a row in the document's Commands table, and the first-admin section names both flags and its refusals.
func TestMigratorReference_DocumentsEveryCommandAndCreateFirstAdmin(t *testing.T) {
	raw, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	md := string(raw)
	cmds := Commands()
	if len(cmds) < 6 {
		t.Fatalf("Commands() lists %d commands, want at least the six the runner has", len(cmds))
	}
	if missing := missingCommands(md, cmds); len(missing) != 0 {
		t.Errorf("%s has no Commands-table row for %v", referencePath, missing)
	}
	for _, want := range []string{FlagAuthUserID, FlagDisplayName, "SHARE ROW EXCLUSIVE", "platform_owner", "Authentication > Users", "Phase 7", "exit", "is_active", "reactivate"} {
		if !strings.Contains(md, want) {
			t.Errorf("%s does not mention %q: the create-first-admin section is incomplete", referencePath, want)
		}
	}

	// Control: the instrument notices a missing row, so an empty answer above means the rows are there.
	without := dropLine(md, "| `"+CmdCreateFirstAdmin+" ")
	if without == md {
		t.Fatal("control: the document has no create-first-admin row to drop, so this control proves nothing")
	}
	if got := missingCommands(without, cmds); len(got) != 1 || got[0] != CmdCreateFirstAdmin {
		t.Errorf("control: with the row removed missingCommands = %v, want [%s]", got, CmdCreateFirstAdmin)
	}
	// `up` must not be satisfied by the row of up-and-set-password.
	if got := missingCommands("| `up-and-set-password` | x |\n", []string{CmdUp}); len(got) != 1 {
		t.Errorf("control: `up` was matched by the up-and-set-password row: %v", got)
	}
}
