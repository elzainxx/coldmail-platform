package migrate

// create-first-admin: the one documented way the platform gets its first admin.
//
// WHY IT EXISTS. platform.admins is written out of band: platform_app holds SELECT on it and nothing else, and there is no admin
// screen before Phase 7, so before the first admin exists nobody can create one. The owner's ruling (Docs/phase-2-spec.md, Part A
// row 6) is a documented setup command, run by the migrating identity, that creates the FIRST admin and refuses every later one:
// admin management stays in Phase 7.
//
// WHAT IT DOES, in one transaction as the migrating identity. SET LOCAL ROLE platform_owner first (CLAUDE.md rule 16), then
// LOCK TABLE platform.admins IN SHARE ROW EXCLUSIVE MODE (that mode conflicts with itself, so two invocations at once take turns and
// the second one finds the first one's row), then: refuse if any admin exists; refuse if the Supabase login does not exist or has no
// email; refuse if the login's public.users row exists and is INACTIVE (owner ruling OD-8: an admin must be an active user, and the command
// never reactivates anyone); create the login's public.users row if it has none; create the platform.admins row; commit; print one line.
//
// THE ONE ROLE DETAIL. auth.users belongs to Supabase Auth: platform_owner has no grant on schema auth, so the login is read as the
// migrating identity (SET LOCAL ROLE NONE for that one statement) and every write runs as platform_owner. Reading it as the owner
// would fail on a Supabase project and on the harness's scratch databases alike.
//
// The database half is the firstAdminTx interface, so the unit tests drive the ORDER of the steps and every refusal with a fake that
// also enforces which role may do what; tests/harness drives the real binary against a real PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The flags of create-first-admin. Errors name the flag exactly as the operator typed it.
const (
	FlagAuthUserID  = "--auth-user-id"
	FlagDisplayName = "--display-name"
)

// MaxDisplayNameBytes bounds --display-name. Bytes, not characters: the limit is a storage and display bound, and a name of 200
// multi-byte characters is not a name of 200 bytes.
const MaxDisplayNameBytes = 200

// systemUserID is the reserved all-zero user (module 2's owner of shared pools). It has no login and can never be an admin
// (platform.admins_not_the_system_user), so the command refuses it before connecting.
const systemUserID = "00000000-0000-0000-0000-000000000000"

// ownerRole owns everything the platform creates; the migrator is made able to SET ROLE to it by platform_0000.
const ownerRole = "platform_owner"

// canonicalUUID is the only spelling accepted: lower-case hex, hyphens at 8-4-4-4-12. Postgres would accept upper case, braces and no
// hyphens, and users.id / auth.users.id would then compare equal to a spelling the operator never sees again; one spelling keeps the
// id in the success line, the log and the database identical.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The refusals that come from the database's state. Each exits 1 (a runtime error: the command line is fine, the database says no),
// each says that nothing was written, and each says what to do next.
var (
	// ErrAdminExists: any row in platform.admins, enabled or not, closes the door. The first admin is a one-time act.
	ErrAdminExists = errors.New("an admin already exists, so nothing was written: create-first-admin only creates the FIRST admin (managing admins comes with Phase 7)")
	// ErrAuthUserMissing: public.users.auth_user_id references auth.users, and the platform never creates logins.
	ErrAuthUserMissing = errors.New("that Supabase login does not exist (auth.users has no row with that id), so nothing was written: create the login first (Supabase dashboard > Authentication > Users) and pass its id as " + FlagAuthUserID)
	// ErrAuthEmailMissing: public.users.email is NOT NULL, non-blank and normalised, and it is taken from the login.
	ErrAuthEmailMissing = errors.New("the Supabase login has no email address, so nothing was written: give the login an email (Supabase dashboard > Authentication > Users) and run the command again")
	// ErrUserInactive: the login already has a public.users row and it is inactive (is_active = false). The command is for a person who can use the platform, and it never rewrites
	// a user row, so it will not make an inactive user the first admin (Docs/phase-2-spec.md, owner decision 8). The returned error is an *InactiveUserError naming the login.
	ErrUserInactive = errors.New("the platform user of that login is inactive")
)

// InactiveUserError is the refusal for an existing, inactive platform user. It names the login and what to do next, says that nothing was written, and says nothing about admins:
// no admin exists (that refusal comes first and is ErrAdminExists). It matches ErrUserInactive.
type InactiveUserError struct{ AuthUserID string }

func (e *InactiveUserError) Error() string {
	return "the platform user for the login " + e.AuthUserID + " exists but is inactive (public.users.is_active is false), so nothing was written: reactivate that user, or run the command for a different login (" + FlagAuthUserID + ")"
}

// Is makes errors.Is(err, ErrUserInactive) true for every InactiveUserError.
func (e *InactiveUserError) Is(target error) bool { return target == ErrUserInactive }

// existingUser is what the platform already holds for the login: the stored email and whether the user is active.
type existingUser struct {
	Email  string
	Active bool
}

// FirstAdminInput is the command line of create-first-admin.
type FirstAdminInput struct {
	AuthUserID  string // the Supabase login's id: a lower-case hyphenated uuid, not the all-zero uuid
	DisplayName string // the name an admin action is recorded under
}

// Name is the display name as it is stored: without leading or trailing space.
func (in FirstAdminInput) Name() string { return strings.TrimSpace(in.DisplayName) }

// Validate checks the input against its rules and reports the first one broken, naming the flag and the rule. Every problem is a
// *ConfigError, so the command exits 2, and none of them needs a database: the command validates before it connects.
func (in FirstAdminInput) Validate() error {
	if !canonicalUUID.MatchString(in.AuthUserID) {
		return configErr("%s must be a lower-case hyphenated uuid (8-4-4-4-12 hex digits, letters a to f in lower case)", FlagAuthUserID)
	}
	if in.AuthUserID == systemUserID {
		return configErr("%s must not be the all-zero uuid: that is the reserved system user, which has no login", FlagAuthUserID)
	}
	if !utf8.ValidString(in.DisplayName) {
		return configErr("%s must be valid UTF-8", FlagDisplayName)
	}
	name := in.Name()
	if name == "" {
		return configErr("%s must not be blank: it is the name the admin's actions are recorded under", FlagDisplayName)
	}
	// The raw value is checked, not the trimmed one: a name that ends in a newline is a shell quoting slip worth refusing, not
	// something to tidy up silently.
	for _, r := range in.DisplayName {
		if unicode.IsControl(r) {
			return configErr("%s must not contain control characters (tab, newline and the like)", FlagDisplayName)
		}
	}
	if len(name) > MaxDisplayNameBytes {
		return configErr("%s must be at most %d bytes (this one is %d)", FlagDisplayName, MaxDisplayNameBytes, len(name))
	}
	return nil
}

// ParseFirstAdminArgs reads the arguments after the command name: --auth-user-id and --display-name, each as `--flag value` or
// `--flag=value`, in either order, both required, each at most once. It then applies Validate, so a nil error means the input is fit
// to use. Every error is a *ConfigError (exit 2) that names the flag at fault.
func ParseFirstAdminArgs(args []string) (FirstAdminInput, error) {
	var in FirstAdminInput
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !strings.HasPrefix(name, "--") {
			return FirstAdminInput{}, configErr("%s takes only the flags %s and %s, no other arguments", CmdCreateFirstAdmin, FlagAuthUserID, FlagDisplayName)
		}
		var dst *string
		switch name {
		case FlagAuthUserID:
			dst = &in.AuthUserID
		case FlagDisplayName:
			dst = &in.DisplayName
		default:
			return FirstAdminInput{}, configErr("unknown flag %s: %s takes %s and %s", name, CmdCreateFirstAdmin, FlagAuthUserID, FlagDisplayName)
		}
		if seen[name] {
			return FirstAdminInput{}, configErr("%s was given more than once", name)
		}
		seen[name] = true
		if !hasValue {
			// A following token that is itself a flag means the value was forgotten; taking it as the value would make the next error
			// point at the wrong flag.
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return FirstAdminInput{}, configErr("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		*dst = value
	}
	if !seen[FlagAuthUserID] {
		return FirstAdminInput{}, configErr("%s is required: the id of the Supabase login that becomes the admin (Supabase dashboard > Authentication > Users)", FlagAuthUserID)
	}
	if !seen[FlagDisplayName] {
		return FirstAdminInput{}, configErr("%s is required: the name the admin's actions are recorded under", FlagDisplayName)
	}
	if err := in.Validate(); err != nil {
		return FirstAdminInput{}, err
	}
	return in, nil
}

// firstAdminTx is the database half of create-first-admin: one transaction, one method per step, in the order the command takes them.
// pgFirstAdminTx is the real one; the unit tests use a fake that records the calls and refuses a step taken as the wrong role.
type firstAdminTx interface {
	// AsOwner is SET LOCAL ROLE platform_owner: every write and the lock run as the owner of the tables.
	AsOwner(ctx context.Context) error
	// AsMigrator is SET LOCAL ROLE NONE: back to the migrating identity, which is the one that may read auth.users.
	AsMigrator(ctx context.Context) error
	// LockAdmins is LOCK TABLE platform.admins IN SHARE ROW EXCLUSIVE MODE (it waits for the lock_timeout, then fails).
	LockAdmins(ctx context.Context) error
	// AdminExists reports whether platform.admins holds any row at all.
	AdminExists(ctx context.Context) (bool, error)
	// AuthEmail reads the login's email from auth.users, as the database normalises it (lower(btrim(email))). found is false when the
	// login does not exist; a login with a NULL email is found with an empty email.
	AuthEmail(ctx context.Context, authUserID string) (email string, found bool, err error)
	// InsertUser creates the login's public.users row (id = auth_user_id) unless it exists; created says which happened. It never
	// rewrites an existing row.
	InsertUser(ctx context.Context, authUserID, email string) (created bool, err error)
	// UserState reads the existing platform user (its email and whether it is active) through the sanctioned view platform.listable_users, which lists an inactive user too;
	// found is false when the user has no row there. Every row of public.users except the system user is listed (users_auth_link: id = auth_user_id), so "not found" means "no row".
	UserState(ctx context.Context, userID string) (user existingUser, found bool, err error)
	// InsertAdmin creates the platform.admins row.
	InsertAdmin(ctx context.Context, id, displayName string) error
	// Commit ends the transaction. Nothing is visible, and nothing is printed, before it succeeds.
	Commit(ctx context.Context) error
}

// createFirstAdmin is the whole command after its input is validated and its transaction is open. The order is the contract:
// the role switch, the table lock, the admin check, the login check, the check of an existing user row (an inactive one is refused), the writes, the commit; the success line
// comes last, after the commit.
func createFirstAdmin(ctx context.Context, tx firstAdminTx, in FirstAdminInput, out io.Writer) error {
	if err := tx.AsOwner(ctx); err != nil {
		return err
	}
	if err := tx.LockAdmins(ctx); err != nil {
		return err
	}
	// From here until the commit this transaction is the only one that can add an admin, so the answer below cannot go stale.
	exists, err := tx.AdminExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return ErrAdminExists
	}

	if err := tx.AsMigrator(ctx); err != nil {
		return err
	}
	email, found, err := tx.AuthEmail(ctx, in.AuthUserID)
	if err != nil {
		return err
	}
	if err := tx.AsOwner(ctx); err != nil {
		return err
	}
	if !found {
		return ErrAuthUserMissing
	}
	if strings.TrimSpace(email) == "" {
		return ErrAuthEmailMissing
	}

	// An existing user row is judged BEFORE anything is written: an inactive user is refused (OD-8), an active one is used as it is.
	prior, seen, err := tx.UserState(ctx, in.AuthUserID)
	if err != nil {
		return err
	}
	if seen && !prior.Active {
		return &InactiveUserError{AuthUserID: in.AuthUserID}
	}

	created, err := tx.InsertUser(ctx, in.AuthUserID, email)
	if err != nil {
		return err
	}
	if !created && !seen {
		// The row did not exist when it was read and does now: another writer created it in between (the table lock covers platform.admins, not public.users). Judge what it
		// left in the same way. The transaction has written nothing yet, so a refusal here still leaves the database as it was.
		if prior, seen, err = tx.UserState(ctx, in.AuthUserID); err != nil {
			return err
		}
		if seen && !prior.Active {
			return &InactiveUserError{AuthUserID: in.AuthUserID}
		}
	}
	note := ""
	if !created && seen && prior.Email != email {
		note = fmt.Sprintf("create-first-admin: note: the platform user %s already existed with the email %s, not the login's %s; it was left as it is\n", in.AuthUserID, prior.Email, email)
	}
	if err := tx.InsertAdmin(ctx, in.AuthUserID, in.Name()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if note != "" {
		fmt.Fprint(out, note)
	}
	fmt.Fprintf(out, "create-first-admin: created admin %s (%s) for %s\n", in.AuthUserID, in.Name(), email)
	return nil
}

// CreateFirstAdmin creates the first admin. It validates before it connects, and it never takes the migration advisory lock: the table
// lock is what serialises two invocations, and it must run after `up` has finished (the schema it writes is the one `up` builds).
func (r *Runner) CreateFirstAdmin(ctx context.Context, in FirstAdminInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(c)
	// The same transaction opening as a migration file: SET LOCAL lock_timeout and statement_timeout, so waiting for the table lock is
	// bounded by PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC and neither setting outlives the transaction.
	tx, err := r.beginFileTx(ctx, c)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", CmdCreateFirstAdmin, err)
	}
	defer rollbackQuietly(tx)
	return createFirstAdmin(ctx, pgFirstAdminTx{tx: tx}, in, r.out())
}

// pgFirstAdminTx is firstAdminTx on a real transaction.
type pgFirstAdminTx struct{ tx pgx.Tx }

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func (p pgFirstAdminTx) AsOwner(ctx context.Context) error {
	if _, err := p.tx.Exec(ctx, "SET LOCAL ROLE "+ownerRole); err != nil {
		return fmt.Errorf("%s: SET LOCAL ROLE %s (is the database migrated, and is this the login that migrated it? platform_0000 lets that login switch to the role): %w", CmdCreateFirstAdmin, ownerRole, err)
	}
	return nil
}

func (p pgFirstAdminTx) AsMigrator(ctx context.Context) error {
	if _, err := p.tx.Exec(ctx, "SET LOCAL ROLE NONE"); err != nil {
		return fmt.Errorf("%s: SET LOCAL ROLE NONE: %w", CmdCreateFirstAdmin, err)
	}
	return nil
}

func (p pgFirstAdminTx) LockAdmins(ctx context.Context) error {
	if _, err := p.tx.Exec(ctx, "LOCK TABLE platform.admins IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		hint := ""
		switch pgCode(err) {
		case "42P01": // undefined_table
			hint = " (the schema is not migrated: run platform-migrate up first)"
		case "55P03": // lock_not_available: lock_timeout fired
			hint = " (another create-first-admin, or a migration, holds the table; raise " + EnvLockTimeoutSec + " to wait longer)"
		}
		return fmt.Errorf("%s: lock platform.admins%s: %w", CmdCreateFirstAdmin, hint, err)
	}
	return nil
}

func (p pgFirstAdminTx) AdminExists(ctx context.Context) (bool, error) {
	var exists bool
	if err := p.tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform.admins)").Scan(&exists); err != nil {
		return false, fmt.Errorf("%s: read platform.admins: %w", CmdCreateFirstAdmin, err)
	}
	return exists, nil
}

func (p pgFirstAdminTx) AuthEmail(ctx context.Context, authUserID string) (string, bool, error) {
	var email *string
	err := p.tx.QueryRow(ctx, "SELECT lower(btrim(email)) FROM auth.users WHERE id = $1::uuid", authUserID).Scan(&email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("%s: read auth.users: %w", CmdCreateFirstAdmin, err)
	case email == nil:
		return "", true, nil
	}
	return *email, true, nil
}

func (p pgFirstAdminTx) InsertUser(ctx context.Context, authUserID, email string) (bool, error) {
	// ON CONFLICT (id) DO NOTHING: an existing row is the same person (id = auth_user_id) and is never rewritten. A different row that
	// already has this email is NOT absorbed (only the named arbiter is): it fails on users_email_key and the transaction rolls back.
	tag, err := p.tx.Exec(ctx,
		"INSERT INTO public.users (id, auth_user_id, email, is_active) VALUES ($1::uuid, $1::uuid, $2::text, true) ON CONFLICT (id) DO NOTHING",
		authUserID, email)
	if err != nil {
		return false, fmt.Errorf("%s: create the platform user: %w", CmdCreateFirstAdmin, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (p pgFirstAdminTx) UserState(ctx context.Context, userID string) (existingUser, bool, error) {
	var u existingUser
	err := p.tx.QueryRow(ctx, "SELECT email, is_active FROM platform.listable_users WHERE id = $1::uuid", userID).Scan(&u.Email, &u.Active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return existingUser{}, false, nil
	case err != nil:
		return existingUser{}, false, fmt.Errorf("%s: read the existing platform user: %w", CmdCreateFirstAdmin, err)
	}
	return u, true, nil
}

func (p pgFirstAdminTx) InsertAdmin(ctx context.Context, id, displayName string) error {
	if _, err := p.tx.Exec(ctx, "INSERT INTO platform.admins (id, display_name) VALUES ($1::uuid, $2::text)", id, displayName); err != nil {
		return fmt.Errorf("%s: create the admin: %w", CmdCreateFirstAdmin, err)
	}
	return nil
}

func (p pgFirstAdminTx) Commit(ctx context.Context) error {
	if err := p.tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", CmdCreateFirstAdmin, err)
	}
	return nil
}
