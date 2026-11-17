package tenant

// Module 1's tenant is the user_id of the caller key that submits a job, and module 1 offers no way to ask for a key: verify.api_callers is written only by its privileged identity, in SQL
// (migration 0010: the runtime role has SELECT and nothing else). So the platform gives every user ONE key, DERIVED from a secret and the user's id, and writes the one row that lets module 1
// recognise it. Nothing is stored on the platform's side: the key is recomputed on every call, and module 1 keeps only its SHA-256.
//
// THE DERIVATION (pinned by a test vector; changing any byte of it changes every user's key and strands every row already provisioned):
//
//	key    = base64url-without-padding( HMAC-SHA256( secret, "m1-caller|" + userID ) )      43 characters from A-Z a-z 0-9 - _
//	digest = SHA-256( the 43 ASCII bytes of the key )                                          32 bytes, stored as verify.api_callers.key_sha256 (bytea)
//
// userID is the platform user id in the lower-case spelling Postgres prints; secret is PLATFORM_M1_KEY_SECRET (the raw bytes of the value, at least config.MinM1KeySecretBytes).
//
// THE DIGEST RULE is module 1's, read from its code: the front door takes the bearer token (the text after "Bearer ", surrounding space trimmed: api/helpers.go bearerToken), computes
// sha256.Sum256([]byte(presented)) and looks the 32 bytes up in key_sha256 (api/api.go:810-845), inside the validity window not_before <= now AND (not_after IS NULL OR not_after > now)
// (store/postgres.go:1345-1363, module 1 at 4adc316). It is the digest of the key's TEXT, not of the 32 bytes the HMAC produced.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/config"
)

// m1CallerPrefix is what the HMAC is taken over, in front of the user id. It names the purpose of the key, so the same secret could derive a key for something else without the two
// colliding.
const m1CallerPrefix = "m1-caller|"

// M1CallerIDPrefix starts the caller_id of every row ProvisionM1Caller writes. caller_id is module 1's opaque authenticated principal (no PII there: a uuid is not one); module 1 reads it
// into a request context and does not interpret it.
const M1CallerIDPrefix = "platform-user-"

// m1NotBeforeSlack is how far back a provisioned key's not_before is set: module 1 compares it with its OWN clock, and a key provisioned a moment ago must not be refused by a clock a few
// seconds behind the database's. An hour is the margin the harness's own seeded caller uses.
const m1NotBeforeSlack = "1 hour"

// M1CallerID is the caller_id of a user's module 1 caller row.
func M1CallerID(userID string) string { return M1CallerIDPrefix + userID }

// DeriveM1Key is the module 1 bearer key of one platform user. It refuses an unconfigured secret (ErrNoM1KeySecret), a short one (ErrWeakSecret), an id that is not in the lower-case
// canonical spelling (ErrBadID: two spellings of one user would be two keys) and the system user (ErrSystemUser). The same secret and user always give the same key; two users, or two secrets,
// never the same one. The error never carries the secret.
func DeriveM1Key(secret config.Secret, userID string) (config.Secret, error) {
	s := secret.Reveal()
	switch {
	case s == "":
		return "", ErrNoM1KeySecret
	case len(s) < config.MinM1KeySecretBytes:
		return "", fmt.Errorf("%w: it must be at least %d bytes", ErrWeakSecret, config.MinM1KeySecretBytes)
	case !isCanonical(userID):
		return "", badID("user id", userID)
	case userID == SystemUserID:
		return "", ErrSystemUser
	}
	mac := hmac.New(sha256.New, []byte(s))
	_, _ = mac.Write([]byte(m1CallerPrefix + userID))
	return config.Secret(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), nil
}

// M1KeyDigest is what module 1 stores for a key: the SHA-256 of the key's text (see the rule above). It is secret-derived material, so it is a plain byte slice that no log line may carry.
func M1KeyDigest(key config.Secret) []byte {
	sum := sha256.Sum256([]byte(key.Reveal()))
	return sum[:]
}

func (r *Resolver) deriveKey(userID string) (config.Secret, error) {
	return DeriveM1Key(r.secret, userID)
}

// AdminDB is the PRIVILEGED connection ProvisionM1Caller writes through: the identity that may INSERT into module 1's verify.api_callers (module 1's grant-admin, or the migrating login that
// holds it), connected to the database that holds module 1's schema. It is never the platform's runtime pool: platform_app, like module 1's own runtime role, cannot write that table, and
// the statement fails with SQLSTATE 42501 when it is tried. *pgx.Conn, pgx.Tx and *pgxpool.Pool satisfy it.
type AdminDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Both statements name their schema (CLAUDE.md rule 12). user_id is bound as a uuid and is NEVER NULL here: a key with a NULL user_id still authenticates and still accepts a job, and the
// job is then unreadable by anyone (module 1 migration 0013; research m1-api-and-node.md 1.2), which is why the function refuses an empty id before it builds the statement, and why the row it
// leaves behind is read back and judged.
const (
	insertM1Caller = `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id)
VALUES ($1, $2, now() - interval '` + m1NotBeforeSlack + `', $3::uuid)
ON CONFLICT (key_sha256) DO NOTHING`
	readM1Caller = `SELECT COALESCE(user_id::text, ''), (not_before <= now() AND (not_after IS NULL OR not_after > now()))
FROM verify.api_callers WHERE key_sha256 = $1`
)

// ProvisionM1Caller writes the module 1 caller row that makes the user's derived key authenticate as that user: caller_id = M1CallerID(userID), key_sha256 = M1KeyDigest of the derived key,
// not_before an hour back, no expiry, user_id = userID. It runs as the privileged identity (admin), never as the runtime role.
//
// It is idempotent: the insert is ON CONFLICT (key_sha256) DO NOTHING, so a second call changes nothing, and two users never collide (their digests differ). After the insert it READS the row
// for the digest back and requires that it names this user and is inside its validity window; a row that exists but would not authenticate this user (a NULL owner left by a hand-written
// insert, another owner, an expired window) is ErrM1CallerConflict and is left as it is, so a silent DO NOTHING can never hide the trap of an ownerless key.
//
// Refusals before any statement: ErrBadID (an empty or malformed id, or one that is not the lower-case spelling), ErrSystemUser, ErrNoM1KeySecret (or ErrWeakSecret), and a nil admin.
func (r *Resolver) ProvisionM1Caller(ctx context.Context, admin AdminDB, userID string) error {
	if userID == "" {
		return fmt.Errorf("%w: the user id is empty (a module 1 key with no owner can submit jobs nobody can read)", ErrBadID)
	}
	if !isCanonical(userID) {
		return badID("user id", userID)
	}
	if userID == SystemUserID {
		return ErrSystemUser
	}
	if r == nil {
		return ErrNoM1KeySecret
	}
	key, err := r.deriveKey(userID)
	if err != nil {
		return err
	}
	if admin == nil {
		return fmt.Errorf("%w: ProvisionM1Caller needs the privileged connection to module 1's database", ErrNoDatabase)
	}
	digest := M1KeyDigest(key)
	if _, err := admin.Exec(ctx, insertM1Caller, M1CallerID(userID), digest, userID); err != nil {
		return fmt.Errorf("tenant: provision the module 1 caller of user %s: %w", userID, err)
	}
	var owner string
	var valid bool
	switch err := admin.QueryRow(ctx, readM1Caller, digest).Scan(&owner, &valid); {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("tenant: provision the module 1 caller of user %s: the row is not there after the insert", userID)
	case err != nil:
		return fmt.Errorf("tenant: read back the module 1 caller of user %s: %w", userID, err)
	}
	if owner != userID || !valid {
		return fmt.Errorf("%w (user %s)", ErrM1CallerConflict, userID)
	}
	return nil
}
