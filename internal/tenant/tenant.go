// Package tenant turns an authenticated Supabase login into each module's tenant scoping. It is the one function from "who logged in" to "whose data a module call may touch".
//
// THE CHAIN. A login is an auth.users row (Supabase Auth). Every real platform user IS its login (public.users.id = public.users.auth_user_id, constraint users_auth_link) except the all-zero
// SYSTEM user, which module 2 requires as the owner of shared pools and which has no login at all. ByAuthUser reads that link through the one sanctioned view, platform.listable_users
// (platform_0004; the gate scripts/check-user-lists.sh fails on any other read of the users table), and returns the platform user. From the platform user, Scope derives what each module needs:
//
//   - Module 2 and module 3 take the platform user id as `user_id` on every call (the body for quote, run, account create and bulk add; the query for everything else) and scope every
//     statement WHERE user_id = $tenant: neither module checks it against a user list on those routes, so a wrong id is a wrong tenant, silently (Docs/history/p3-research/m2-api-and-worker.md
//     section 3; m3-api.md section 3; module 2 services/control/internal/platformapi/store.go:84-94 at ff8c302).
//   - Module 1 has no `user_id` parameter at all. Its tenant is the user_id of the CALLER KEY that submits a job: the front door hashes the presented bearer with SHA-256, looks the digest
//     up in verify.api_callers and stamps the row's user_id on the job, and every job read predicates on it (control-plane/internal/api/api.go:810-845 and internal/store/postgres.go:1345-1363
//     at 4adc316; migrations 0010 and 0013). So the platform gives each user ONE KEY, derived and never stored: see DeriveM1Key, M1CallerEnsurer (the service registers the key at the first module 1 job, through the privileged function platform.provision_m1_caller) and ProvisionM1Caller (the same row, through a privileged connection: operator tools and tests).
//
// WHAT NEVER HAPPENS. (1) A public user id supplied by a caller is never trusted: the only input is the authenticated login id, and Scope re-reads the database and refuses a Tenant whose
// UserID is not the user of its AuthUserID (a Tenant is a plain struct, so anyone can write one). (2) A login with no platform user is never given one: users are created by an admin, so the
// answer is ErrUnknownLogin and nothing is written. (3) The all-zero user, an inactive user and a malformed id are refused with their own typed errors, and a function that returns an error
// returns no value. (4) No secret, key or key digest is ever put into an error, a log line or a printed value.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

// SystemUserID is the reserved all-zero user (module 2's owner of shared pools; platform_0001). It has no login, holds nothing (platform_0004) and is never a tenant. A test pins it equal to
// money.SystemUser, which says the same thing for the money store.
const SystemUserID = "00000000-0000-0000-0000-000000000000"

// The refusals of the package. A caller tests them with errors.Is; none of them carries a secret, a key, a digest or the value that was refused.
var (
	// ErrUnknownLogin: no platform user exists for the login. Users are created by an admin, never by a login, so nothing was created.
	ErrUnknownLogin = errors.New("tenant: no platform user exists for this login")
	// ErrInactive: the platform user exists and is switched off (public.users.is_active = false). The answer is read from the database on every call; nothing is cached.
	ErrInactive = errors.New("tenant: the platform user is inactive")
	// ErrSystemUser: the all-zero system user is never a login and never a tenant.
	ErrSystemUser = errors.New("tenant: the system user is never a login")
	// ErrBadID: the value is not a uuid in the 8-4-4-4-12 hexadecimal form (or, where the spelling matters, not the lower-case spelling Postgres prints).
	ErrBadID = errors.New("tenant: the id is not a uuid")
	// ErrTenantMismatch: a Tenant whose UserID is not the platform user of its AuthUserID. The only way to build one is to write it by hand.
	ErrTenantMismatch = errors.New("tenant: the tenant's user id is not the platform user of its login")
	// ErrNoM1KeySecret: PLATFORM_M1_KEY_SECRET is not set, so no module 1 key can be derived.
	ErrNoM1KeySecret = errors.New("tenant: the module 1 key secret is not configured")
	// ErrWeakSecret: the module 1 key secret is shorter than config.MinM1KeySecretBytes.
	ErrWeakSecret = errors.New("tenant: the module 1 key secret is too short")
	// ErrBadKeyVersion: a module 1 key version outside config.MinM1KeyVersion to config.MaxM1KeyVersion (or a negative grace period).
	ErrBadKeyVersion = errors.New("tenant: the module 1 key version is not valid")
	// ErrM1CallerNotFound: RetireM1CallerVersion found no module 1 caller row of this user for the key of that version. Nothing was changed.
	ErrM1CallerNotFound = errors.New("tenant: no module 1 caller row of this user for that key version")
	// ErrNoDatabase: the call needs a database connection the resolver (or the caller) does not have.
	ErrNoDatabase = errors.New("tenant: no database connection")
	// ErrM1CallerConflict: an api_callers row for this user's key already exists but would not authenticate the user (another owner, no owner, or outside its validity window). Nothing was changed.
	ErrM1CallerConflict = errors.New("tenant: a module 1 caller row for this key exists but does not act for this user")
)

// Tenant is a platform user as resolved from a login.
type Tenant struct {
	// UserID is public.users.id: the id every module call carries (module 2 and 3 `user_id`, the ledger's user, the owner of a job).
	UserID string
	// AuthUserID is auth.users.id, the login the Tenant was resolved from. Scope re-reads the database with it.
	AuthUserID string
	// Active is public.users.is_active at the time of the read. ByAuthUser refuses an inactive user (ErrInactive), so on a Tenant it returned this is true; the field says what the Tenant was
	// resolved as and is what a caller that keeps a Tenant for a while must not rely on (Scope reads again).
	Active bool
}

// Scope is what each module needs to act for one tenant.
type Scope struct {
	// M2UserID is module 2's `user_id`: the platform user id.
	M2UserID string
	// M3UserID is module 3's `user_id`: the platform user id.
	M3UserID string
	// M1Key is the bearer key of the tenant's module 1 caller row (see DeriveM1Key). A config.Secret: every way fmt, json and slog print it yields "[redacted]".
	M1Key config.Secret
}

// DB is what the resolver reads through. *pgxpool.Pool (the service's platform_app pool), *pgx.Conn and pgx.Tx all satisfy it.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Resolver resolves logins and builds scopes. It holds a database handle and the module 1 key secret and nothing else, so it is safe for concurrent use.
type Resolver struct {
	db      DB
	secret  config.Secret // PLATFORM_M1_KEY_SECRET; "" means not configured
	version int           // PLATFORM_M1_KEY_VERSION: the version Scope presents and ProvisionM1Caller writes; 0 means the first (config.MinM1KeyVersion)
	dsn     string        // kept only to scrub error text (dsnscrub); never logged, never returned
}

// NewResolver builds a resolver over the platform's pool (a login as platform_app; nil is allowed for a resolver that only derives and provisions keys, and every call that needs the
// database then answers ErrNoDatabase). m1KeySecret is PLATFORM_M1_KEY_SECRET: empty means not configured (Scope and ProvisionM1Caller then answer ErrNoM1KeySecret), anything else shorter than
// config.MinM1KeySecretBytes is refused with ErrWeakSecret and the error never carries the value.
func NewResolver(db DB, m1KeySecret config.Secret, opts ...Option) (*Resolver, error) {
	if s := m1KeySecret.Reveal(); s != "" && len(s) < config.MinM1KeySecretBytes {
		return nil, fmt.Errorf("%w: it must be at least %d bytes", ErrWeakSecret, config.MinM1KeySecretBytes)
	}
	r := &Resolver{db: db, secret: m1KeySecret}
	for _, o := range opts {
		if err := o(r); err != nil {
			return nil, err
		}
	}
	if p, ok := db.(*pgxpool.Pool); ok && p != nil {
		r.dsn = p.Config().ConnString()
	}
	return r, nil
}

// Option adjusts a Resolver at construction.
type Option func(*Resolver) error

// WithM1KeyVersion sets the key version the resolver presents (Scope) and provisions (ProvisionM1Caller): PLATFORM_M1_KEY_VERSION, config.MinM1KeyVersion to config.MaxM1KeyVersion. Without it the
// version is the first (1). A version outside the range is refused with ErrBadKeyVersion.
func WithM1KeyVersion(v int) Option {
	return func(r *Resolver) error {
		if v < config.MinM1KeyVersion || v > config.MaxM1KeyVersion {
			return fmt.Errorf("%w: %d is not in %d to %d", ErrBadKeyVersion, v, config.MinM1KeyVersion, config.MaxM1KeyVersion)
		}
		r.version = v
		return nil
	}
}

// keyVersion is the version the resolver presents and provisions.
func (r *Resolver) keyVersion() int {
	if r.version == 0 {
		return config.MinM1KeyVersion
	}
	return r.version
}

// Format, String and GoString render the resolver without its secret for EVERY verb: a struct field of type config.Secret is not enough on its own, because fmt does not call methods on an
// unexported field and would print the raw string (config.Secret documents the hole).
func (r Resolver) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

func (r Resolver) String() string {
	state := "[redacted]"
	if r.secret == "" {
		state = "(unset)"
	}
	return "tenant.Resolver{m1_key_secret: " + state + ", m1_key_version: " + strconv.Itoa(r.keyVersion()) + "}"
}

func (r Resolver) GoString() string { return r.String() }

// The one read of the package. platform.listable_users is the sanctioned list of users: every real person, active or not, never the system user. auth_user_id is NOT NULL in it.
const selectByAuthUser = `SELECT id::text, auth_user_id::text, is_active FROM platform.listable_users WHERE auth_user_id = $1::uuid`

// ByAuthUser resolves an authenticated login id (auth.users.id, the `sub` of the session) to its platform user. The id is accepted in either letter case and is looked up in the lower-case
// spelling Postgres prints. It never creates a user. Its refusals, in the order they are decided:
//
//	ErrBadID        not a uuid (decided before the database is asked)
//	ErrSystemUser   the all-zero uuid (decided before the database is asked, whether or not a login with that id exists)
//	ErrUnknownLogin no platform user has this login
//	ErrInactive     the platform user is switched off
//
// A database failure is returned wrapped (the connection string and password taken out of its text) and is none of the above. On any error the Tenant is the zero value.
func (r *Resolver) ByAuthUser(ctx context.Context, authUserID string) (Tenant, error) {
	id, ok := parseID(authUserID)
	if !ok {
		return Tenant{}, badID("login id", authUserID)
	}
	if id == SystemUserID {
		return Tenant{}, ErrSystemUser
	}
	if r == nil || r.db == nil {
		return Tenant{}, ErrNoDatabase
	}
	var t Tenant
	err := r.db.QueryRow(ctx, selectByAuthUser, id).Scan(&t.UserID, &t.AuthUserID, &t.Active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Tenant{}, ErrUnknownLogin
	case err != nil:
		return Tenant{}, fmt.Errorf("tenant: read the platform user of login %s: %w", id, dsnscrub.Scrub(err, r.dsn))
	}
	// The view cannot return these; they are checked because a wrong answer here is a wrong tenant on every module.
	switch {
	case t.UserID == SystemUserID || t.AuthUserID == SystemUserID:
		return Tenant{}, ErrSystemUser
	case t.AuthUserID != id || !isCanonical(t.UserID):
		return Tenant{}, fmt.Errorf("tenant: the database answered a platform user that does not belong to login %s", id)
	case !t.Active:
		return Tenant{}, ErrInactive
	}
	return t, nil
}

// Scope builds the module scoping of a tenant. It does NOT trust the Tenant it is given: it reads the database again with the Tenant's AuthUserID and refuses (ErrTenantMismatch) a UserID that
// is not that login's platform user, and refuses (ErrInactive, ErrUnknownLogin) a user that was switched off or removed since the Tenant was resolved. Every id in the result comes from the
// database, never from the argument. It writes nothing: the module 1 caller row is registered by M1CallerEnsurer.EnsureCaller (the service, at the first module 1 job) or by ProvisionM1Caller (a privileged connection), once per user and key version.
//
// ErrNoM1KeySecret is returned when PLATFORM_M1_KEY_SECRET is not configured: a Scope with an empty module 1 key would be a trap, so there is no partial Scope. On any error the Scope is the
// zero value.
func (r *Resolver) Scope(ctx context.Context, t Tenant) (Scope, error) {
	if !isCanonical(t.UserID) {
		return Scope{}, badID("tenant user id", t.UserID)
	}
	if t.UserID == SystemUserID {
		return Scope{}, ErrSystemUser
	}
	fresh, err := r.ByAuthUser(ctx, t.AuthUserID)
	if err != nil {
		return Scope{}, err
	}
	if fresh.UserID != t.UserID {
		return Scope{}, ErrTenantMismatch
	}
	key, err := r.deriveKey(fresh.UserID)
	if err != nil {
		return Scope{}, err
	}
	return Scope{M2UserID: fresh.UserID, M3UserID: fresh.UserID, M1Key: key}, nil
}

// badID is ErrBadID with the size of what was refused and never its text: the value may be anything a caller passed, a token included.
func badID(what, v string) error {
	return fmt.Errorf("%w: the %s must be 36 characters, hexadecimal digits in groups of 8-4-4-4-12 separated by hyphens (got %d bytes)", ErrBadID, what, len(v))
}

// parseID accepts the 36-character hyphenated uuid in either letter case and returns the lower-case spelling Postgres prints. Nothing else is a uuid here: not braces, not the form without
// hyphens, not a hyphen after every group of four digits: Postgres would accept those, and one spelling per id keeps the id in a log line, in a lock key and in a derived key the same
// everywhere.
func parseID(s string) (string, bool) {
	if len(s) != 36 {
		return "", false
	}
	b := make([]byte, 36)
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", false
			}
		default:
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			case c >= 'A' && c <= 'F':
				c += 'a' - 'A'
			default:
				return "", false
			}
		}
		b[i] = c
	}
	return string(b), true
}

// isCanonical is true for exactly the lower-case spelling Postgres prints.
func isCanonical(s string) bool {
	c, ok := parseID(s)
	return ok && c == s
}
