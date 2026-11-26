package tenant

// Module 1's caller key for a user is registered AUTOMATICALLY, at the user's first module 1 job, through ONE narrow privileged function (db/install/postmodule1.sql, Phase 4 chunk B16):
//
//	SELECT platform.provision_m1_caller($1::uuid, $2::bytea)        ->  'created' | 'exists'   (or a refusal, below)
//
// The function is SECURITY DEFINER, owned by module 1's grant-admin (the only role that may add a row to module 1's caller table), executable by platform_app and nobody else, and it writes exactly one kind of
// row. The platform service runs as platform_app, which has no right on module 1's schema and must never hold module 1's admin credential: this file is the service's half of the door (the key is derived
// here exactly as ProvisionM1CallerVersion derives it, with the same functions; only the way the row gets written differs). ProvisionM1Caller / ProvisionM1CallerVersion in m1key.go stay what they were:
// the operator tools' and the tests' writer, through a privileged connection.
//
// THE CACHE IS AN OPTIMISATION, NEVER A SOURCE OF TRUTH. The function is idempotent (a second call for a registered key answers 'exists' and changes nothing), so a restarted service, a second instance or an
// evicted entry simply calls it once more. The set of (user, key version) pairs already ensured is bounded (DefaultM1EnsureCacheEntries, first in first out) so that a hot loop does not ask the database
// at every tick and a long run does not grow without bound. Only a call the database answered with 'created' or 'exists' is remembered: a refusal or a failure is asked again at the next call.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

// FuncDB is what the ensurer calls the function through: the platform's runtime pool (platform_app) satisfies it, and so do a *pgx.Conn and a pgx.Tx. It is NOT the privileged connection of AdminDB: the
// function holds the privilege, the connection does not.
type FuncDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DefaultM1EnsureCacheEntries is how many (user, key version) pairs an ensurer remembers when it is not told otherwise: about 200 KiB at the size of a canonical user id, far above the number of users a
// single instance serves between two restarts of a hot loop, and small enough never to matter. An evicted pair is simply ensured once more.
const DefaultM1EnsureCacheEntries = 4096

// The function's answer and the names it raises with (db/install/postmodule1.sql; Go reads the CONSTRAINT name, never the message text).
const (
	m1ProvisionCall    = `SELECT platform.provision_m1_caller($1::uuid, $2::bytea)`
	m1ProvisionCreated = "created"
	m1ProvisionExists  = "exists"

	m1pgUserRequired   = "m1_caller_user_required"         // 22004: the user id was NULL
	m1pgSystemUser     = "m1_caller_not_the_system_user"   // 22023: the all-zero user
	m1pgDigestLength   = "m1_caller_digest_is_32_bytes"    // 22023: the digest was not a SHA-256
	m1pgRowAfter       = "m1_caller_row_after_insert"      // 55000: the row was not there after the insert (a concurrent delete by an operator)
	m1pgRowNotTheUsers = "m1_caller_row_acts_for_the_user" // 23505: the row exists but is another user's, has no owner or is outside its window
	m1pgUndefinedFunc  = "42883"                           // the function is not installed: db/install/postmodule1.sql has not been run
	m1pgDenied         = "42501"                           // platform_app may not execute it (or a schema right is missing): the install is incomplete
	m1InstallHint      = "run db/install/postmodule1.sql (Docs/install.md step 3b)"
)

// M1CallerEnsurer registers a user's module 1 caller key through platform.provision_m1_caller, once per (user, key version) per process. It is safe for concurrent use.
//
// Its one exported method is EnsureCaller(ctx, userID string) error: the interface the job engine takes (jobengine.M1Provisioner), kept to exactly that.
type M1CallerEnsurer struct {
	res   *Resolver
	db    FuncDB
	dsn   string // kept only to scrub error text (dsnscrub); never logged, never returned
	cache *ensureCache
}

// EnsurerOption adjusts an M1CallerEnsurer at construction.
type EnsurerOption func(*M1CallerEnsurer)

// WithEnsureCacheEntries sets how many (user, key version) pairs the ensurer remembers. A value below 1 turns the memory off: every call then goes to the database (which is idempotent), the setting
// the tests use to see the database's own answers.
func WithEnsureCacheEntries(n int) EnsurerOption {
	return func(e *M1CallerEnsurer) { e.cache = newEnsureCache(n) }
}

// NewM1CallerEnsurer builds the ensurer over the resolver (which holds the key secret and the key version to present: the same resolver the service builds scopes with, so the key registered is the key
// presented) and the runtime database handle (the platform_app pool). Neither is checked here: a nil resolver, an unset secret or a nil handle answers the typed error at the first EnsureCaller
// (ErrNoM1KeySecret, ErrNoDatabase), like the resolver's own calls.
func NewM1CallerEnsurer(resolver *Resolver, db FuncDB, opts ...EnsurerOption) *M1CallerEnsurer {
	e := &M1CallerEnsurer{res: resolver, db: db, cache: newEnsureCache(DefaultM1EnsureCacheEntries)}
	if p, ok := db.(*pgxpool.Pool); ok && p != nil {
		e.dsn = p.Config().ConnString()
	} else if resolver != nil {
		e.dsn = resolver.dsn
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// EnsureCaller makes sure the user's module 1 caller key, derived at the resolver's CURRENT key version, is registered, and returns nil when module 1 will recognise it as that user's. It is idempotent
// and cheap on the second call (the in-memory set); it never creates a second row for a key and never changes an existing one.
//
// Its refusals, in the order they are decided (the first three before the database is asked):
//
//	ErrBadID            the user id is empty, not a uuid, or not the lower-case spelling, or the function was handed a NULL one
//	ErrSystemUser       the all-zero system user
//	ErrNoM1KeySecret    the resolver has no secret (or is nil); ErrWeakSecret / ErrBadKeyVersion as the derivation says
//	ErrNoDatabase       the ensurer has no database handle
//	ErrM1CallerConflict a row for this user's key exists but would not authenticate the user (another owner, no owner, or outside its window); nothing was changed, and it is asked again next time
//
// Any other failure (the database is down, the function is not installed, platform_app may not execute it) is returned wrapped, with the SQLSTATE and, for the two install problems, the step that fixes
// them; the caller leaves the job where it is and tries again. No error carries the key, its digest or the secret.
func (e *M1CallerEnsurer) EnsureCaller(ctx context.Context, userID string) error {
	// the same decisions, in the same order, as ProvisionM1CallerVersion: the id first (it needs no secret), then the secret, then the connection
	if userID == "" {
		return fmt.Errorf("%w: the user id is empty (a module 1 key with no owner can submit jobs nobody can read)", ErrBadID)
	}
	if !isCanonical(userID) {
		return badID("user id", userID)
	}
	if userID == SystemUserID {
		return ErrSystemUser
	}
	if e == nil || e.res == nil {
		return ErrNoM1KeySecret
	}
	version := e.res.keyVersion()
	k := ensureKey{user: userID, version: version}
	if e.cache.has(k) {
		return nil
	}
	key, err := DeriveM1KeyVersion(e.res.secret, version, userID)
	if err != nil {
		return err
	}
	if e.db == nil {
		return fmt.Errorf("%w: EnsureCaller needs the platform's database handle", ErrNoDatabase)
	}
	digest := M1KeyDigest(key)
	var answer string
	if err := e.db.QueryRow(ctx, m1ProvisionCall, userID, digest).Scan(&answer); err != nil {
		return e.mapFailure(userID, err, key, digest)
	}
	if answer != m1ProvisionCreated && answer != m1ProvisionExists {
		// the function's vocabulary is two words; anything else is a function that is not the one this code was written for, and nothing is remembered
		return fmt.Errorf("tenant: ensure the module 1 caller of user %s: the database answered with a word that is neither %q nor %q (%d bytes): the installed function is not the one this version expects", userID, m1ProvisionCreated, m1ProvisionExists, len(answer))
	}
	e.cache.add(k)
	return nil
}

// mapFailure turns the database's refusal into the package's typed errors, and any other failure into a wrapped one whose text carries no key material.
func (e *M1CallerEnsurer) mapFailure(userID string, err error, key config.Secret, digest []byte) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case pe.ConstraintName == m1pgUserRequired:
			return fmt.Errorf("%w: the database refused a NULL user id", ErrBadID)
		case pe.ConstraintName == m1pgSystemUser:
			return ErrSystemUser
		case pe.ConstraintName == m1pgRowNotTheUsers:
			return fmt.Errorf("%w (user %s)", ErrM1CallerConflict, userID)
		case pe.ConstraintName == m1pgDigestLength:
			return fmt.Errorf("tenant: ensure the module 1 caller of user %s: the database refused the key digest as not 32 bytes (SQLSTATE %s): this is a defect in the platform, not in the data", userID, pe.Code)
		case pe.ConstraintName == m1pgRowAfter:
			return fmt.Errorf("tenant: ensure the module 1 caller of user %s: the row was not there after the insert (SQLSTATE %s): module 1's table was changed concurrently, try again", userID, pe.Code)
		case pe.Code == m1pgUndefinedFunc:
			return fmt.Errorf("tenant: ensure the module 1 caller of user %s: platform.provision_m1_caller does not exist (SQLSTATE %s): %s", userID, pe.Code, m1InstallHint)
		case pe.Code == m1pgDenied:
			return fmt.Errorf("tenant: ensure the module 1 caller of user %s: the platform's database role may not execute platform.provision_m1_caller (SQLSTATE %s): %s", userID, pe.Code, m1InstallHint)
		}
	}
	return fmt.Errorf("tenant: ensure the module 1 caller of user %s: %w", userID, e.redacted(err, key, digest))
}

// redactedError is a driver error whose TEXT has had every spelling of the key, its digest and the secret taken out; Unwrap keeps errors.Is and errors.As reaching the original (a caller may test for a
// context error or a pgconn.PgError), and only the string, the thing that reaches a log, is replaced. A driver does not normally put a bound parameter into an error; this is the wall for the day one does.
type redactedError struct {
	err error
	msg string
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

func (e *M1CallerEnsurer) redacted(err error, key config.Secret, digest []byte) error {
	err = dsnscrub.Scrub(err, e.dsn)
	msg := err.Error()
	var olds []string
	add := func(s string) {
		if s != "" {
			olds = append(olds, s, "[redacted]")
		}
	}
	add(key.Reveal())
	add(e.res.secret.Reveal())
	add(hex.EncodeToString(digest))
	add(strings.ToUpper(hex.EncodeToString(digest)))
	add(base64.StdEncoding.EncodeToString(digest))
	add(base64.RawStdEncoding.EncodeToString(digest))
	add(base64.URLEncoding.EncodeToString(digest))
	add(base64.RawURLEncoding.EncodeToString(digest))
	add(fmt.Sprint(digest))
	add(fmt.Sprintf("%#v", digest))
	return &redactedError{err: err, msg: strings.NewReplacer(olds...).Replace(msg)}
}

// ---- the bounded memory ----

// ensureKey is what has been ensured: the user and the key version the key was derived at (a rotation to version 2 is a new key and so a new call).
type ensureKey struct {
	user    string
	version int
}

// ensureCache is a fixed-capacity set that forgets its oldest entry first. Safe for concurrent use; a capacity below 1 stores nothing.
type ensureCache struct {
	mu    sync.Mutex
	max   int
	set   map[ensureKey]struct{}
	order []ensureKey // filled up to max, then overwritten in a ring from next
	next  int
}

func newEnsureCache(max int) *ensureCache {
	return &ensureCache{max: max, set: map[ensureKey]struct{}{}}
}

func (c *ensureCache) has(k ensureKey) bool {
	if c == nil || c.max < 1 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.set[k]
	return ok
}

func (c *ensureCache) add(k ensureKey) {
	if c == nil || c.max < 1 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.set[k]; ok {
		return
	}
	if len(c.order) < c.max {
		c.order = append(c.order, k)
	} else {
		delete(c.set, c.order[c.next])
		c.order[c.next] = k
		c.next = (c.next + 1) % c.max
	}
	c.set[k] = struct{}{}
}

func (c *ensureCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.set)
}
