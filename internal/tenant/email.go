package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/dsnscrub"
)

// The one read of a user's own address (Phase 6, GET /v1/me). It reads the same sanctioned view as the resolver (platform.listable_users: never the system user, and the gate
// scripts/check-user-lists.sh keeps every other read of the users table out), by the platform user's id, and takes the address from the DATABASE: the API never shows an address taken from a token's
// claims, which the user's identity provider chose and the platform did not check.
const selectEmailOf = `SELECT email, is_active FROM platform.listable_users WHERE id = $1::uuid`

// Email is the address of the platform user with this id (public.users.email: normalised, trimmed and lower case by the table's own CHECK). The id is the principal's, and the answer is for that user
// alone. Its refusals are the resolver's own, so a caller answers them the same way: ErrBadID (not a canonical lower-case uuid, decided before the database is asked), ErrSystemUser (the all-zero id,
// likewise), ErrUnknownLogin (no such user) and ErrInactive (the user was switched off since the request was authenticated). A database failure is wrapped with the connection string taken out. On any
// error the address is empty.
func (r *Resolver) Email(ctx context.Context, userID string) (string, error) {
	if !isCanonical(userID) {
		return "", badID("user id", userID)
	}
	if userID == SystemUserID {
		return "", ErrSystemUser
	}
	if r == nil || r.db == nil {
		return "", ErrNoDatabase
	}
	var email string
	var active bool
	err := r.db.QueryRow(ctx, selectEmailOf, userID).Scan(&email, &active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrUnknownLogin
	case err != nil:
		return "", fmt.Errorf("tenant: read the address of user %s: %w", userID, dsnscrub.Scrub(err, r.dsn))
	case !active:
		return "", ErrInactive
	}
	return email, nil
}
