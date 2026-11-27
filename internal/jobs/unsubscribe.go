package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// THE UNSUBSCRIBE LINK OF A MESSAGE (Phase 5, chunk S3). Every message of a send job (modules 2 and 3) carries a per-recipient link. The store does not know how it is made: it is given a function
// (WithUnsubscribe: the item's id to the link, internal/unsubscribe makes it from the platform's secret and public address) and does three things with it.
//
//  1. insertItems (engine.go) asks it for every row it writes, puts the link in public.job_items.unsubscribe_url and on the EngineRow the renderer sees (EngineRow.UnsubscribeURL), so that the
//     message and the header a sender builds from the column carry the same link. With no function the column is NULL and the field empty: that is the state of a test and of a service whose two
//     dials (PLATFORM_UNSUBSCRIBE_SECRET, PLATFORM_PUBLIC_BASE_URL) are unset, and it is a state in which no send job can be confirmed (item 3).
//  2. ItemOwner answers the public endpoint's one question: whose item is this and at what address (the endpoint adds the address to that user's suppression list through internal/suppress).
//  3. Confirm refuses a send job when there is no function (requireUnsubscribe): a send without a working link is not allowed.
//
// The link is judged here before it is written, by the same rule as the column's CHECK (platform_0017), so a maker that returns something the database would refuse is an error with a reason, not a
// constraint violation in the middle of a page of rows.

// ReasonUnsubscribeNotConfigured is the Reason of the NotAllowedError a confirm of a send job gets when the store has no unsubscribe link maker.
const ReasonUnsubscribeNotConfigured = "unsubscribe_not_configured"

// MaxUnsubscribeURLBytes is the longest link the column accepts (its CHECK).
const MaxUnsubscribeURLBytes = 2000

// WithUnsubscribe gives the store the function that makes the unsubscribe link of an item: the item's id (the canonical lower-case uuid of the row, which is also public.job_items.id) to the link,
// one https URL. nil (the default) means none is configured: items are written with no link and a send job cannot be confirmed (ReasonUnsubscribeNotConfigured). The function must be pure and safe
// for concurrent use: the same item gives the same link, because a replay of an insert must not change what an earlier one wrote.
func WithUnsubscribe(mk func(itemID string) (string, error)) Option {
	return func(s *Store) { s.unsub = mk }
}

// UnsubscribeConfigured says whether the store has a link maker.
func (s *Store) UnsubscribeConfigured() bool { return s.unsub != nil }

// linkOf is the link of one item: "" when no maker is configured (the column is then NULL), the maker's link when it is a link the column would accept, an error otherwise. The error says that the
// link of a row was refused and why in a word; it never carries the link (a link names an item, and the item names an address).
func (s *Store) linkOf(itemID string) (string, error) {
	if s.unsub == nil {
		return "", nil
	}
	link, err := s.unsub(itemID)
	if err != nil {
		return "", errors.New("the unsubscribe link of a row could not be made")
	}
	if reason := badLink(link); reason != "" {
		return "", fmt.Errorf("the unsubscribe link of a row is refused: %s", reason)
	}
	return link, nil
}

// badLink is "" for a link the column's CHECK accepts and the reason in a few words for one it refuses: https://, then at least one character, none of them white space, "<", ">" or a control
// character; at most MaxUnsubscribeURLBytes bytes.
func badLink(link string) string {
	switch {
	case !strings.HasPrefix(link, "https://") || len(link) == len("https://"):
		return "it is not an https URL"
	case len(link) > MaxUnsubscribeURLBytes:
		return fmt.Sprintf("it is longer than %d bytes", MaxUnsubscribeURLBytes)
	}
	if !utf8.ValidString(link) {
		return "it is not valid UTF-8"
	}
	for _, r := range link {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' { // the stricter side of the database's classes, whose reach on non-ASCII text depends on its locale
			return "it holds white space, a control character or an angle bracket"
		}
	}
	return ""
}

// ItemOwner resolves an item id (platform.job_rows.id, which is also public.job_items.id and the id inside an unsubscribe token) to the user who owns the job and the NORMALISED address of the row.
// An id that is not the canonical lower-case uuid, an id no row of a send job has, and a row with no address are the same ErrNotFound: the caller cannot tell them apart and neither can the person
// who asked.
//
// THIS IS THE ONE READ OF THE STORE THAT IS NOT SCOPED BY A USER. Every other method takes the authenticated principal's id; this one is what the unauthenticated unsubscribe endpoint calls with an
// id it has verified the signature of (internal/unsubscribe), and its answer is the user to act for. It reads; it writes nothing, and it never logs the address or the id.
func (s *Store) ItemOwner(ctx context.Context, itemID string) (userID, email string, err error) {
	if !isUUID(itemID) {
		return "", "", opErr("item owner", ErrNotFound)
	}
	var addr *string
	err = s.pool.QueryRow(ctx, `SELECT w.user_id::text, w.email_norm
		FROM platform.job_rows w JOIN platform.jobs j ON j.id = w.job_id
		WHERE w.id = $1::uuid AND j.module_id IN ('m2', 'm3')`, itemID).Scan(&userID, &addr)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", opErr("item owner", ErrNotFound)
	case err != nil:
		return "", "", opErr("item owner", s.fail(err))
	case addr == nil || *addr == "":
		return "", "", opErr("item owner", ErrNotFound)
	}
	return userID, *addr, nil
}

// requireUnsubscribe is the refusal of a confirm of a send job in a service that cannot make a link: ErrNotAllowed with ReasonUnsubscribeNotConfigured. It costs nothing in a service that can (the
// maker is there: no query), and in one that cannot it reads the job's module once. A job that does not exist for the user is ErrNotFound, as everywhere; a verify job is not refused.
func (s *Store) requireUnsubscribe(ctx context.Context, userID, jobID string) error {
	if s.unsub != nil {
		return nil
	}
	var module string
	err := s.pool.QueryRow(ctx, `SELECT module_id FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID).Scan(&module)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return s.fail(err)
	case module == ModuleSendManaged || module == ModuleSendBYO:
		return notAllowed(ReasonUnsubscribeNotConfigured)
	}
	return nil
}
