package m3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The account surface the user app needs (research m3 section 8; Domain A, under /v1/send-byo/accounts): list, get, create, the test button, bulk add and an account's events. Every
// route is tenant scoped: user_id is in the body of create and bulk add and in the query of everything else, and every account an answer names must be the Client's tenant's.
//
// Not covered, deliberately: change (PATCH), delete and the lifecycle verbs (disable, enable, pause, unpause, unquarantine), test-send, settings, usage and notifications. They are
// pass-throughs the user app does not need to SEE account state; the lead adds one when a screen needs it, on the same helpers.

// The route labels of the account surface (the `Route` of a modclient request and of an error).
const (
	routeList   = "m3.accounts.list"
	routeGet    = "m3.accounts.get"
	routeCreate = "m3.accounts.create"
	routeTest   = "m3.accounts.test"
	routeBulk   = "m3.accounts.bulk"
	routeEvents = "m3.accounts.events"
)

// The routes as the contracts name them: the method and the path template (never a real URL).
const (
	RouteAccountsList   = "GET /v1/send-byo/accounts"
	RouteAccountsGet    = "GET /v1/send-byo/accounts/{id}"
	RouteAccountsCreate = "POST /v1/send-byo/accounts"
	RouteAccountsTest   = "POST /v1/send-byo/accounts/{id}/test"
	RouteAccountsBulk   = "POST /v1/send-byo/accounts/bulk"
	RouteAccountsEvents = "GET /v1/send-byo/accounts/{id}/events"
)

const (
	pathAccounts = "/v1/send-byo/accounts"
	// PageLimit is the page size asked of a list: the module's maximum (it clamps silently above it).
	PageLimit      = 200
	maxCursorBytes = 1024
	// MaxPages bounds a walk of a list: with PageLimit it is two hundred thousand accounts, far above the account count cap; a module whose cursor never ends is a Contract error.
	MaxPages = 1000
)

var (
	// ErrAccountDuplicate: the user already has an account with this host and username (create's 409 that names smtp_username). The first call may have created it: list before retrying.
	ErrAccountDuplicate = errors.New("m3: the user already has an account with this SMTP host and username")
	// ErrActionUnavailable: module 3 answered 409 to the test button or to a bulk add. On those two routes the only cause of a 409 is the user's suspension (the module's one fixed sentence,
	// "not available for your account right now", which deliberately does not say why); the adapter names the route's only cause as "unavailable", not as "suspended".
	ErrActionUnavailable = errors.New("m3: module 3 refuses this account action for this user right now")
	// ErrHostNotAllowed: the SMTP host is a private, loopback, link-local or otherwise non-routable address, or a name that can only mean one (a single label, localhost, .local), and the
	// adapter was not built with AllowPrivateSMTPHosts.
	ErrHostNotAllowed = errors.New("m3: the SMTP host is not a public address")
)

// ---- the account object ------------------------------------------------------------------------------------------------

// Account is one of the user's sender accounts as module 3 reports it. The secret, the ciphertext, the nonce and the key id are never returned by the module, and nothing here could hold
// them: HasCredential is all there is.
type Account struct {
	ID            string
	UserID        string
	SMTPHost      string
	SMTPPort      int
	SMTPUsername  string
	FromAddress   string
	FromName      *string
	HasCredential bool

	Paused bool
	// PauseReason is "daily_limit", "throttle" or "manual" while paused, "" when not paused, "unrecognised" for a word the adapter does not know.
	PauseReason string
	Quarantined bool
	// QuarantineReason is "auth_failure", "consecutive_failures", "failure_rate" or "manual" while quarantined, "" otherwise, "unrecognised" for another word.
	QuarantineReason string
	QuarantinedAt    *time.Time
	Disabled         bool

	Verification  AccountVerification
	FromAlignment AccountAlignment

	DailyLimit int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AccountVerification is the credential check of an account.
type AccountVerification struct {
	State      VerificationState
	VerifiedAt *time.Time
	VerifyAt   *time.Time
	// Code says why the last check failed ("" when it did not).
	Code VerifyCode
	// SMTPResponse is the mail provider's own (scrubbed) answer to the last check: the provider's words, not the module's, bounded to MaxProviderTextBytes.
	SMTPResponse *string
}

// AccountAlignment is the From-gate verdict on the account's from address.
type AccountAlignment struct {
	State AlignmentState
	// DMARCPolicy is the policy found ("none", "quarantine", "reject") or "".
	DMARCPolicy string
	CheckedAt   *time.Time
	// Message is the module's user-facing sentence for the verdict (its copy ledger), bounded to MaxProviderTextBytes.
	Message        *string
	AcknowledgedAt *time.Time
}

// MaxProviderTextBytes bounds every piece of free text the adapter carries from the module: the provider's SMTP answer and the module's own user-facing alignment sentence.
const MaxProviderTextBytes = 2000

// accountShape is what an account object is decoded with (single account, a list element, the create answer). It is built from the handlers' own types (accountResource), NOT from
// `describe.schemas`, which declares the account flat where the module nests status, verification and from_alignment.
var accountShape = modclient.Object(
	modclient.Req("id", modclient.Str), modclient.Req("user_id", modclient.Str),
	modclient.Req("smtp_host", modclient.Str), modclient.Req("smtp_port", modclient.Int), modclient.Req("smtp_username", modclient.Str),
	modclient.Req("from_address", modclient.Str), modclient.Req("from_name", modclient.Str.OrNull()), modclient.Req("has_credential", modclient.Bool),
	modclient.Req("status", modclient.Object(
		modclient.Req("paused", modclient.Bool), modclient.Req("pause_reason", modclient.Str.OrNull()), modclient.Req("quarantined", modclient.Bool),
		modclient.Opt("quarantine_reason", modclient.Str.OrNull()), modclient.Opt("quarantined_at", modclient.Str.OrNull()), modclient.Req("disabled", modclient.Bool),
	)),
	modclient.Req("verification", modclient.Object(
		modclient.Req("state", modclient.Str), modclient.Opt("verified_at", modclient.Str.OrNull()), modclient.Opt("verify_at", modclient.Str.OrNull()),
		modclient.Opt("code", modclient.Str.OrNull()), modclient.Opt("smtp_response", modclient.Str.OrNull()),
	)),
	modclient.Req("from_alignment", modclient.Object(
		modclient.Req("state", modclient.Str), modclient.Opt("dmarc_policy", modclient.Str.OrNull()), modclient.Opt("checked_at", modclient.Str.OrNull()),
		modclient.Opt("last_attempt_at", modclient.Str.OrNull()), modclient.Opt("last_attempt_error", modclient.Str.OrNull()), modclient.Opt("message", modclient.Str.OrNull()),
		modclient.Opt("acknowledged_at", modclient.Str.OrNull()),
	)),
	modclient.Req("daily_limit", modclient.Int), modclient.Req("created_at", modclient.Str), modclient.Req("updated_at", modclient.Str),
)

// accountListShape is the list answer: {"items": [...], "next_cursor": ...}.
//
// MODULE-FORCED(M3-F5): the module's list handlers answer the array under `items` while describe.schemas names it `results` (the campaign report's key), so this shape is built from the handlers and describe.schemas is never read for it.
var accountListShape = modclient.Object(
	modclient.Req("items", modclient.ArrayOf(accountShape)),
	modclient.Req("next_cursor", modclient.Str.OrNull()),
)

type wireAccount struct {
	ID            string  `json:"id"`
	UserID        string  `json:"user_id"`
	SMTPHost      string  `json:"smtp_host"`
	SMTPPort      int     `json:"smtp_port"`
	SMTPUsername  string  `json:"smtp_username"`
	FromAddress   string  `json:"from_address"`
	FromName      *string `json:"from_name"`
	HasCredential bool    `json:"has_credential"`
	Status        struct {
		Paused           bool    `json:"paused"`
		PauseReason      *string `json:"pause_reason"`
		Quarantined      bool    `json:"quarantined"`
		QuarantineReason *string `json:"quarantine_reason"`
		QuarantinedAt    *string `json:"quarantined_at"`
		Disabled         bool    `json:"disabled"`
	} `json:"status"`
	Verification struct {
		State        string  `json:"state"`
		VerifiedAt   *string `json:"verified_at"`
		VerifyAt     *string `json:"verify_at"`
		Code         *string `json:"code"`
		SMTPResponse *string `json:"smtp_response"`
	} `json:"verification"`
	FromAlignment struct {
		State          string  `json:"state"`
		DMARCPolicy    *string `json:"dmarc_policy"`
		CheckedAt      *string `json:"checked_at"`
		Message        *string `json:"message"`
		AcknowledgedAt *string `json:"acknowledged_at"`
		// last_attempt_error is NOT read: it holds the From-gate's raw DNS error text, which the adapter never passes on.
	} `json:"from_alignment"`
	DailyLimit int    `json:"daily_limit"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func at(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// instant reads a timestamp. The module spells its account timestamps to the second ("2026-11-17T12:00:01Z"); an RFC 3339 reader serves with or without a fraction.
func instant(resp *modclient.Response, route, path, key string, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, contract(resp, route, at(path, key), "the value is not an RFC 3339 instant")
	}
	return t, nil
}

func optInstant(resp *modclient.Response, route, path, key string, s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := instant(resp, route, path, key, *s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// providerText bounds a piece of free text from the module: valid UTF-8, at most MaxProviderTextBytes.
func providerText(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.ToValidUTF8(*s, "")
	if len(v) > MaxProviderTextBytes {
		v = strings.ToValidUTF8(v[:MaxProviderTextBytes], "")
	}
	return &v
}

func word(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// account turns one decoded wire account into an Account, refusing an id that is not a uuid, an account of another tenant and an unreadable timestamp.
func (c *Client) account(resp *modclient.Response, route, path string, w *wireAccount) (Account, error) {
	if !sendapi.CanonicalUUID(w.ID) {
		return Account{}, contract(resp, route, at(path, "id"), "the account id is not a lower-case uuid")
	}
	if !strings.EqualFold(w.UserID, c.UserID()) {
		return Account{}, contract(resp, route, at(path, "user_id"), "the account belongs to another tenant than the one asked for")
	}
	created, err := instant(resp, route, path, "created_at", w.CreatedAt)
	if err != nil {
		return Account{}, err
	}
	updated, err := instant(resp, route, path, "updated_at", w.UpdatedAt)
	if err != nil {
		return Account{}, err
	}
	a := Account{
		ID: w.ID, UserID: w.UserID, SMTPHost: w.SMTPHost, SMTPPort: w.SMTPPort, SMTPUsername: w.SMTPUsername, FromAddress: w.FromAddress, FromName: w.FromName,
		HasCredential: w.HasCredential, Paused: w.Status.Paused, Quarantined: w.Status.Quarantined, Disabled: w.Status.Disabled, DailyLimit: w.DailyLimit,
		CreatedAt: created, UpdatedAt: updated,
	}
	if w.Status.PauseReason != nil {
		a.PauseReason = pauseReason(*w.Status.PauseReason)
	}
	if w.Status.QuarantineReason != nil {
		a.QuarantineReason = quarantineReason(*w.Status.QuarantineReason)
	}
	if a.QuarantinedAt, err = optInstant(resp, route, at(path, "status"), "quarantined_at", w.Status.QuarantinedAt); err != nil {
		return Account{}, err
	}
	v := &a.Verification
	v.State = verificationState(w.Verification.State)
	v.Code = verifyCode(word(w.Verification.Code))
	v.SMTPResponse = providerText(w.Verification.SMTPResponse)
	if v.VerifiedAt, err = optInstant(resp, route, at(path, "verification"), "verified_at", w.Verification.VerifiedAt); err != nil {
		return Account{}, err
	}
	if v.VerifyAt, err = optInstant(resp, route, at(path, "verification"), "verify_at", w.Verification.VerifyAt); err != nil {
		return Account{}, err
	}
	al := &a.FromAlignment
	al.State = alignmentState(w.FromAlignment.State)
	al.DMARCPolicy = word(w.FromAlignment.DMARCPolicy)
	al.Message = providerText(w.FromAlignment.Message)
	if al.CheckedAt, err = optInstant(resp, route, at(path, "from_alignment"), "checked_at", w.FromAlignment.CheckedAt); err != nil {
		return Account{}, err
	}
	if al.AcknowledgedAt, err = optInstant(resp, route, at(path, "from_alignment"), "acknowledged_at", w.FromAlignment.AcknowledgedAt); err != nil {
		return Account{}, err
	}
	return a, nil
}

// ---- errors of the account routes --------------------------------------------------------------------------------------

func hasDetail(e *modclient.Error, field, issue string) bool {
	for _, d := range e.Details {
		if d.Field == field && d.Issue == issue {
			return true
		}
	}
	return false
}

// accountErr refines the 409 of an account route, never changes a Kind and never turns an error into a value.
func accountErr(route string, err error) error {
	var e *modclient.Error
	if !errors.As(err, &e) || e.Kind != modclient.Refused || e.Status != http.StatusConflict {
		return err
	}
	switch route {
	case routeCreate:
		if hasDetail(e, "smtp_username", "conflict") {
			return fmt.Errorf("%w: %w", ErrAccountDuplicate, err)
		}
		// MODULE-FORCED(M3-F6): create answers 409 for the account cap and for a suspended user with the same code and no details (only a duplicate names smtp_username), so the adapter can only say "conflict, cause not stated".
		return fmt.Errorf("%w: %w", sendapi.ErrConflictUnexplained, err)
	case routeTest, routeBulk:
		// MODULE-FORCED(M3-F9): the 409 of the test button and of a bulk add is one fixed sentence that deliberately does not say why, and the route has one possible cause (the suspension gate), so the adapter names it "unavailable" and never "suspended".
		return fmt.Errorf("%w: %w", ErrActionUnavailable, err)
	}
	return err
}

// MaybeApplied reports whether err leaves it open that a create, a bulk add or a test button click took effect at the module: the request was sent and no usable answer came back (a
// timeout, an unavailable module, a fault of the module's, an answer that could not be read, a canceled call). It is false for a refusal the module made (nothing was done) and for an
// error raised before anything was sent. Account creation is not idempotent and is never retried by the adapter, so after a true here the caller lists the accounts before it tries again.
func MaybeApplied(err error) bool {
	var e *modclient.Error
	if !errors.As(err, &e) || !e.Sent {
		return false
	}
	switch e.Kind {
	case modclient.Timeout, modclient.Unavailable, modclient.Internal, modclient.Contract, modclient.Canceled:
		return true
	}
	return false
}

// ---- list, get ---------------------------------------------------------------------------------------------------------

// AccountPage is one page of the user's accounts.
type AccountPage struct {
	Items []Account
	// NextCursor is the module's opaque cursor, passed back verbatim; "" on the last page.
	NextCursor string
}

func validCursor(route, cursor string) error {
	if len(cursor) > maxCursorBytes {
		return invalid(route, "the cursor is longer than any cursor the module issues")
	}
	return nil
}

func (c *Client) get(ctx context.Context, route, path string, q url.Values) (*modclient.Response, error) {
	q.Set("user_id", c.UserID())
	return c.Module().Do(ctx, modclient.Request{Module: modclient.M3, Route: route, Method: http.MethodGet, Path: path, Query: q, Idempotent: true})
}

// ListAccounts reads one page of the user's accounts (GET /v1/send-byo/accounts), oldest first. cursor is "" for the first page and otherwise exactly the NextCursor of the page before it.
func (c *Client) ListAccounts(ctx context.Context, cursor string) (AccountPage, error) {
	if err := validCursor(routeList, cursor); err != nil {
		return AccountPage{}, err
	}
	q := url.Values{"limit": {strconv.Itoa(PageLimit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := c.get(ctx, routeList, pathAccounts, q)
	if err != nil {
		return AccountPage{}, err
	}
	var w struct {
		Items      []wireAccount `json:"items"`
		NextCursor *string       `json:"next_cursor"`
	}
	if err := modclient.Decode(resp, &w, accountListShape); err != nil {
		return AccountPage{}, err
	}
	if len(w.Items) > PageLimit {
		return AccountPage{}, contract(resp, routeList, "items", "the page holds more accounts than the page size asked")
	}
	next, err := nextCursor(resp, routeList, w.NextCursor, len(w.Items))
	if err != nil {
		return AccountPage{}, err
	}
	p := AccountPage{Items: make([]Account, 0, len(w.Items)), NextCursor: next}
	for i := range w.Items {
		a, err := c.account(resp, routeList, "items["+strconv.Itoa(i)+"]", &w.Items[i])
		if err != nil {
			return AccountPage{}, err
		}
		p.Items = append(p.Items, a)
	}
	return p, nil
}

func nextCursor(resp *modclient.Response, route string, next *string, n int) (string, error) {
	if next == nil {
		return "", nil
	}
	if *next == "" || len(*next) > maxCursorBytes {
		return "", contract(resp, route, "next_cursor", "the cursor is empty or longer than any cursor the module issues")
	}
	if n == 0 {
		return "", contract(resp, route, "next_cursor", "an empty page names a next cursor, so the cursor does not advance")
	}
	return *next, nil
}

// AllAccounts reads every page of the user's accounts and returns the union, or an error and NO accounts: a page that cannot be read means the listing is not complete, and a partial
// listing is never returned as the whole. It refuses what would loop or lie: a cursor that repeats, the same account on two pages, more than MaxPages pages.
func (c *Client) AllAccounts(ctx context.Context) ([]Account, error) {
	var all []Account
	asked := map[string]bool{}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= MaxPages {
			return nil, contract(nil, routeList, "next_cursor", fmt.Sprintf("the listing did not end within %d pages", MaxPages))
		}
		asked[cursor] = true
		p, err := c.ListAccounts(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("m3: the listing of the accounts failed on page %d (the listing is NOT complete): %w", pages+1, err)
		}
		for _, a := range p.Items {
			if seen[a.ID] {
				return nil, contract(nil, routeList, "items", "the same account is on two pages, so the cursor does not hold the exact-once order")
			}
			seen[a.ID] = true
		}
		all = append(all, p.Items...)
		if p.NextCursor == "" {
			return all, nil
		}
		if asked[p.NextCursor] {
			return nil, contract(nil, routeList, "next_cursor", "the next cursor was already used, so the cursor does not advance")
		}
		cursor = p.NextCursor
	}
}

// GetAccount reads one account (GET /v1/send-byo/accounts/{id}). A 404 is modclient.NotFound and means the account does not exist, is deleted or is another tenant's: the module
// answers the three the same.
func (c *Client) GetAccount(ctx context.Context, accountID string) (Account, error) {
	if !sendapi.CanonicalUUID(accountID) {
		return Account{}, invalid(routeGet, "the account id is not a lower-case uuid")
	}
	resp, err := c.get(ctx, routeGet, pathAccounts+"/"+url.PathEscape(accountID), url.Values{})
	if err != nil {
		return Account{}, err
	}
	var w wireAccount
	if err := modclient.Decode(resp, &w, accountShape); err != nil {
		return Account{}, err
	}
	a, err := c.account(resp, routeGet, "", &w)
	if err != nil {
		return Account{}, err
	}
	if a.ID != accountID {
		return Account{}, contract(resp, routeGet, "id", "the answer is for another account than the one asked")
	}
	return a, nil
}

// ---- events -------------------------------------------------------------------------------------------------------------

// Flags is an account's flags as an audit row recorded them.
type Flags struct {
	Paused      bool
	PauseReason string
	Quarantined bool
	Disabled    bool
}

// AccountEvent is one row of an account's audit trail. Actor ("dispatcher", "box", "reclaim_loop", "admin", "system", "user") and Action ("created", "verified", "disabled", ...) are the
// module's own fixed words, carried as they are and never branched on here.
type AccountEvent struct {
	ID        string
	AccountID string
	Actor     string
	Action    string
	From      *Flags
	To        *Flags
	// Reason is the note recorded with the action (an operator's or the user's own), bounded to MaxProviderTextBytes.
	Reason    *string
	RequestID string
	CreatedAt time.Time
}

// EventPage is one page of an account's events.
type EventPage struct {
	Items      []AccountEvent
	NextCursor string
}

var flagsShape = modclient.Object(
	modclient.Req("paused", modclient.Bool), modclient.Opt("pause_reason", modclient.Str.OrNull()), modclient.Req("quarantined", modclient.Bool), modclient.Req("disabled", modclient.Bool),
).OrNull()

// eventsShape is the events answer: {"items": [...], "next_cursor": ...}, the same two keys as the account list.
var eventsShape = modclient.Object(
	modclient.Req("items", modclient.ArrayOf(modclient.Object(
		modclient.Req("id", modclient.Str), modclient.Req("account_id", modclient.Str.OrNull()), modclient.Req("user_id", modclient.Str),
		modclient.Req("actor", modclient.Str), modclient.Req("action", modclient.Str),
		modclient.Req("from_flags", flagsShape), modclient.Req("to_flags", flagsShape),
		modclient.Req("reason", modclient.Str.OrNull()), modclient.Req("request_id", modclient.Str.OrNull()), modclient.Req("created_at", modclient.Str),
	))),
	modclient.Req("next_cursor", modclient.Str.OrNull()),
)

type wireFlags struct {
	Paused      bool    `json:"paused"`
	PauseReason *string `json:"pause_reason"`
	Quarantined bool    `json:"quarantined"`
	Disabled    bool    `json:"disabled"`
}

func flagsOf(f *wireFlags) *Flags {
	if f == nil {
		return nil
	}
	return &Flags{Paused: f.Paused, PauseReason: pauseReasonOrNone(f.PauseReason), Quarantined: f.Quarantined, Disabled: f.Disabled}
}

func pauseReasonOrNone(s *string) string {
	if s == nil {
		return ""
	}
	return pauseReason(*s)
}

// AccountEvents reads one page of an account's audit trail (GET /v1/send-byo/accounts/{id}/events), oldest first. An account that is unknown, deleted or another tenant's answers an EMPTY
// page (HTTP 200, no 404: the module does no existence check there), so an empty page says nothing about whether the account exists: ask GetAccount for that.
func (c *Client) AccountEvents(ctx context.Context, accountID, cursor string) (EventPage, error) {
	if !sendapi.CanonicalUUID(accountID) {
		return EventPage{}, invalid(routeEvents, "the account id is not a lower-case uuid")
	}
	if err := validCursor(routeEvents, cursor); err != nil {
		return EventPage{}, err
	}
	q := url.Values{"limit": {strconv.Itoa(PageLimit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := c.get(ctx, routeEvents, pathAccounts+"/"+url.PathEscape(accountID)+"/events", q)
	if err != nil {
		return EventPage{}, err
	}
	var w struct {
		Items []struct {
			ID        string     `json:"id"`
			AccountID *string    `json:"account_id"`
			UserID    string     `json:"user_id"`
			Actor     string     `json:"actor"`
			Action    string     `json:"action"`
			FromFlags *wireFlags `json:"from_flags"`
			ToFlags   *wireFlags `json:"to_flags"`
			Reason    *string    `json:"reason"`
			RequestID *string    `json:"request_id"`
			CreatedAt string     `json:"created_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := modclient.Decode(resp, &w, eventsShape); err != nil {
		return EventPage{}, err
	}
	if len(w.Items) > PageLimit {
		return EventPage{}, contract(resp, routeEvents, "items", "the page holds more events than the page size asked")
	}
	next, err := nextCursor(resp, routeEvents, w.NextCursor, len(w.Items))
	if err != nil {
		return EventPage{}, err
	}
	p := EventPage{Items: make([]AccountEvent, 0, len(w.Items)), NextCursor: next}
	for i, it := range w.Items {
		path := "items[" + strconv.Itoa(i) + "]"
		if !strings.EqualFold(it.UserID, c.UserID()) {
			return EventPage{}, contract(resp, routeEvents, at(path, "user_id"), "the event belongs to another tenant than the one asked for")
		}
		if strings.TrimSpace(it.ID) == "" {
			return EventPage{}, contract(resp, routeEvents, at(path, "id"), "the id is blank")
		}
		created, err := instant(resp, routeEvents, path, "created_at", it.CreatedAt)
		if err != nil {
			return EventPage{}, err
		}
		p.Items = append(p.Items, AccountEvent{
			ID: it.ID, AccountID: word(it.AccountID), Actor: fixedWord(it.Actor), Action: fixedWord(it.Action), From: flagsOf(it.FromFlags), To: flagsOf(it.ToFlags),
			Reason: providerText(it.Reason), RequestID: word(it.RequestID), CreatedAt: created,
		})
	}
	return p, nil
}

// ---- create, test ---------------------------------------------------------------------------------------------------------

// NewAccount is the account a user adds: an SMTP login and the address mail is sent from.
type NewAccount struct {
	SMTPHost string
	// SMTPPort is the port; 0 leaves the module's default (587; 465 is implicit TLS, anything else STARTTLS).
	SMTPPort     int
	SMTPUsername string
	// SMTPSecret is the SMTP password. It prints as [redacted] in every fmt verb and in every log; it is sent in the request body and nowhere else.
	SMTPSecret  config.Secret
	FromAddress string
	FromName    *string
	// DailyLimit is the account's own daily send limit; 0 leaves the module's default.
	DailyLimit int
	// AcknowledgeAlignmentWarning accepts a From-gate warning for the current verdict (without it the module refuses the account with 422 when the verdict is a warning).
	AcknowledgeAlignmentWarning bool
}

// wireNew is a NewAccount as the module reads it. Only the fields the module knows: its decoder refuses any other, a bulk row refuses user_id, and the secret is the one place Reveal is
// called.
type wireNew struct {
	SMTPHost     string  `json:"smtp_host"`
	SMTPPort     *int    `json:"smtp_port,omitempty"`
	SMTPUsername string  `json:"smtp_username"`
	SMTPSecret   string  `json:"smtp_secret"`
	FromAddress  string  `json:"from_address"`
	FromName     *string `json:"from_name,omitempty"`
	DailyLimit   *int    `json:"daily_limit,omitempty"`
	Acknowledge  bool    `json:"acknowledge_alignment_warning,omitempty"`
}

func (n NewAccount) wire() wireNew {
	w := wireNew{SMTPHost: n.SMTPHost, SMTPUsername: n.SMTPUsername, SMTPSecret: n.SMTPSecret.Reveal(), FromAddress: n.FromAddress, FromName: n.FromName, Acknowledge: n.AcknowledgeAlignmentWarning}
	if n.SMTPPort != 0 {
		p := n.SMTPPort
		w.SMTPPort = &p
	}
	if n.DailyLimit != 0 {
		d := n.DailyLimit
		w.DailyLimit = &d
	}
	return w
}

// check refuses before anything is sent: no secret, and (unless private hosts are allowed) a host that is not a public address.
func (c *Client) check(route string, n NewAccount) error {
	if n.SMTPSecret.Reveal() == "" {
		return invalid(route, "the SMTP secret is empty")
	}
	if !c.privateHosts {
		if err := checkHost(n.SMTPHost); err != nil {
			return fmt.Errorf("%w: %w", ErrHostNotAllowed, invalid(route, err.Error()))
		}
	}
	return nil
}

// CreateAccount adds an account for the tenant (POST /v1/send-byo/accounts) and returns it as the module stored it, verified or not: verify-on-add runs inside the request and a failed or
// missing verification is NOT an error (read Account.Verification). It is not idempotent and is never retried by the adapter: after an error for which MaybeApplied is true, list the
// accounts before trying again (a repeat after a lost answer is a 409 duplicate, ErrAccountDuplicate). It has its own timeout (SlowAccountTimeout).
//
// The SMTP host is refused unless it is a public address (ErrHostNotAllowed), see checkHost. A 409 is ErrAccountDuplicate when it names the username, and otherwise
// sendapi.ErrConflictUnexplained (the account cap and a suspended user answer alike); a 422 refusal of an unacknowledged From-gate warning is a Refused error whose details name
// acknowledge_alignment_warning.
func (c *Client) CreateAccount(ctx context.Context, n NewAccount) (Account, error) {
	if err := c.check(routeCreate, n); err != nil {
		return Account{}, err
	}
	body := struct {
		UserID string `json:"user_id"`
		wireNew
	}{c.UserID(), n.wire()}
	resp, err := c.Module().Do(ctx, modclient.Request{
		Module: modclient.M3, Route: routeCreate, Method: http.MethodPost, Path: pathAccounts, Body: body, Timeout: SlowAccountTimeout, Expect: []int{http.StatusCreated},
	})
	if err != nil {
		return Account{}, accountErr(routeCreate, err)
	}
	var w wireAccount
	if err := modclient.Decode(resp, &w, accountShape); err != nil {
		return Account{}, err
	}
	return c.account(resp, routeCreate, "", &w)
}

// TestResult is the answer of the test button: the credential check and the From-gate re-check, which are independent.
type TestResult struct {
	Verification TestVerification
	Alignment    TestAlignment
}

// TestVerification is the credential check of the test button. A failed credential is still a successful call (State failed, Code says why); State unavailable means the check could not
// run at all (Code verify_error or verification_unavailable).
type TestVerification struct {
	State        VerificationState
	Code         VerifyCode
	SMTPResponse *string
}

// TestAlignment is the From-gate re-check of the test button. State indeterminate means the DNS did not answer: nothing was decided and nothing was stored. NotStored is true when the
// module decided but could not store the verdict.
type TestAlignment struct {
	State     AlignmentState
	Message   *string
	NotStored bool
}

var testShape = modclient.Object(
	modclient.Req("verification", modclient.Object(modclient.Req("state", modclient.Str), modclient.Opt("smtp_response", modclient.Str.OrNull()), modclient.Opt("code", modclient.Str.OrNull()))),
	modclient.Req("from_alignment", modclient.Object(
		modclient.Req("state", modclient.Str), modclient.Opt("message", modclient.Str.OrNull()), modclient.Opt("code", modclient.Str.OrNull()), modclient.Opt("error", modclient.Str.OrNull()),
	)),
)

// TestAccount runs the module's test button on an account (POST /v1/send-byo/accounts/{id}/test): a credential check through a worker and a re-check of the From address. It is a long
// call (SlowAccountTimeout) and is never retried: every click is a real authentication attempt against the user's mail provider and counts against the module's limits (5 per account and
// 20 per user, rolling 24 hours, shared with create and bulk add; the module's 429 is modclient.RateLimited, with no Retry-After). acknowledge accepts a From-gate warning for the verdict
// the click finds. A 409 is ErrActionUnavailable.
func (c *Client) TestAccount(ctx context.Context, accountID string, acknowledge bool) (TestResult, error) {
	if !sendapi.CanonicalUUID(accountID) {
		return TestResult{}, invalid(routeTest, "the account id is not a lower-case uuid")
	}
	body := struct {
		Acknowledge bool `json:"acknowledge_alignment_warning"`
	}{acknowledge}
	resp, err := c.Module().Do(ctx, modclient.Request{
		Module: modclient.M3, Route: routeTest, Method: http.MethodPost, Path: pathAccounts + "/" + url.PathEscape(accountID) + "/test",
		Query: url.Values{"user_id": {c.UserID()}}, Body: body, Timeout: SlowAccountTimeout,
	})
	if err != nil {
		return TestResult{}, accountErr(routeTest, err)
	}
	var w struct {
		Verification struct {
			State        string  `json:"state"`
			SMTPResponse *string `json:"smtp_response"`
			Code         *string `json:"code"`
		} `json:"verification"`
		FromAlignment struct {
			State   string  `json:"state"`
			Message *string `json:"message"`
			Error   *string `json:"error"`
		} `json:"from_alignment"`
	}
	if err := modclient.Decode(resp, &w, testShape); err != nil {
		return TestResult{}, err
	}
	r := TestResult{
		Verification: TestVerification{State: testVerificationState(w.Verification.State), Code: verifyCode(word(w.Verification.Code)), SMTPResponse: providerText(w.Verification.SMTPResponse)},
		Alignment:    TestAlignment{State: testAlignmentState(w.FromAlignment.State), Message: providerText(w.FromAlignment.Message)},
	}
	// MODULE-FORCED(M3-F7): when the test button decided the From verdict but could not store it, it puts an internal reason slug in from_alignment.error, so the adapter reduces that key to a boolean and never carries its text.
	r.Alignment.NotStored = w.FromAlignment.Error != nil && *w.FromAlignment.Error != ""
	return r, nil
}
