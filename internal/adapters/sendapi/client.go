// Package sendapi is the one body that serves both sender modules (module 2, send-managed; module 3, send-byo) on the four routes they answer identically: POST /v1/quote, POST /v1/run,
// and GET /v1/report in its two forms (one job; a whole campaign, paged). Phase 3, chunk B4 (module 2's adapter, internal/adapters/m2, is this package with module 2's Profile and
// nothing else but its own cancel route); module 3's adapter is built on the same body.
//
// # WHAT IT IS
//
// A thin mapping of the module's verbs onto platform types: a Quote, a RunResult, an Item (and, from a batch of items, the charge.Outcome of each terminal one and the money.SettleItem
// that money.Settle takes). It adds no storage, no job logic and no retries of its own (modclient retries a request marked idempotent, and all four routes are: a read, a pure price, and
// run, which the module deduplicates by job_item_id). Phase 4 owns jobs.
//
// # WHAT IT NEVER DOES
//
//   - Invent an outcome. A module that is down, slow, paused, refusing, faulty or answering something unexpected yields a typed *modclient.Error (modclient.KindOf), never a default verdict
//     and never a zero count that reads as an answer. Every method that returns an error returns the zero value beside it, and a campaign walk whose page could not be read returns no pages.
//   - Guess what a state means. The states the adapter handles are the union of the two modules' (stateClass); a state outside the profile's closed set is a Contract error naming the
//     key; only a TERMINAL item is ever settled; a terminal item whose (state, reason) the charge table (internal/charge/rules.psv) cannot classify is returned as Unmapped, never
//     charged.
//   - Act for a tenant it was not built for. Every request carries the user_id the Client was built with (the body for quote and run, the query for reports); a Client built with an empty
//     or all-zero id refuses before any network call.
//
// # WHERE THE TWO MODULES DIFFER
//
// In Profile, as data: the vocabularies each emits, whether the single report always carries its optional keys, module 3's account detail and feasibility block, whether a 409 on run
// means back-pressure (module 2) or an unexplained refusal (module 3). There is no `if module == ...` in the client.
package sendapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

const (
	// MaxRecipientCount is the most the adapter will ask a module to price. Module 2's quote has no upper bound and multiplies in 64 bits (research m2 oddity 9), so an unbounded count could
	// overflow into a plausible-looking cost; one million is far above any queue cap of either module and far from overflow at the largest price (1000 credits).
	MaxRecipientCount = 1_000_000
	// MaxPageSize is the largest page either module serves (it clamps silently above it, so the adapter refuses to ask for more).
	MaxPageSize = 200
	// DefaultPageSize is the page size asked when Options.PageSize is 0: the module's maximum, the fewest round trips (each costs a bcrypt verify and a database round trip).
	DefaultPageSize = MaxPageSize
	// DefaultMaxPages bounds a walk: with the maximum page size it is two million items, far above a campaign; a module whose cursor never ends is a Contract error, not a hang.
	DefaultMaxPages = 10000

	maxCursorBytes = 1024
	opQuote        = "quote"
	opRun          = "run"
	opReport       = "report"
	opReportCamp   = "report.campaign"
)

// Options are the constructor dials of a Client (not process dials: nothing here is read from the environment).
type Options struct {
	// PageSize is the page size asked of a campaign report, 1 to MaxPageSize; 0 means DefaultPageSize.
	PageSize int
	// MaxPages is the most pages one walk may read, at least 1; 0 means DefaultMaxPages.
	MaxPages int
}

// Client talks to one sender module for one tenant. Build it with New; it is safe for concurrent use and immutable.
type Client struct {
	mc   *modclient.Client
	p    Profile
	user string
	sh   shapeSet
	opts Options
}

// New builds a Client for the tenant userID (public.users.id, the lower-case 8-4-4-4-12 spelling Postgres prints). mc must have been built for p.Module. It refuses, before any network call,
// a tenant id that is empty or not a uuid (errors.Is(err, tenant.ErrBadID)), the all-zero system user (tenant.ErrSystemUser: it is never a tenant), a profile that could make the client
// invent something, and options out of range; all of them are modclient.Invalid errors.
func New(mc *modclient.Client, p Profile, userID string, opts Options) (*Client, error) {
	bad := func(msg string) error {
		return &modclient.Error{Kind: modclient.Invalid, Module: p.Module, Route: string(p.Module) + ".new", Message: msg}
	}
	if mc == nil {
		return nil, bad("there is no module client")
	}
	if mc.Module() != p.Module {
		return nil, bad("the module client was built for another module than the profile")
	}
	if err := p.validate(); err != nil {
		return nil, bad(err.Error())
	}
	switch {
	case userID == "":
		return nil, refine(tenant.ErrBadID, bad("the tenant's user id is empty"))
	case !CanonicalUUID(userID):
		return nil, refine(tenant.ErrBadID, bad("the tenant's user id is not a lower-case uuid"))
	case userID == tenant.SystemUserID:
		return nil, refine(tenant.ErrSystemUser, bad("the all-zero system user is never a tenant"))
	}
	switch {
	case opts.PageSize < 0 || opts.PageSize > MaxPageSize:
		return nil, bad("the page size must be between 1 and the module's maximum (0 means the default)")
	case opts.MaxPages < 0:
		return nil, bad("the page limit must not be negative (0 means the default)")
	}
	if opts.PageSize == 0 {
		opts.PageSize = DefaultPageSize
	}
	if opts.MaxPages == 0 {
		opts.MaxPages = DefaultMaxPages
	}
	return &Client{mc: mc, p: p.clone(), user: userID, sh: newShapes(p), opts: opts}, nil
}

// Profile returns a copy of the profile the client was built with.
func (c *Client) Profile() Profile { return c.p.clone() }

// UserID is the tenant the client acts for.
func (c *Client) UserID() string { return c.user }

// Module is the module client underneath, for the operations that are not tenant scoped (health, readiness, describe).
func (c *Client) Module() *modclient.Client { return c.mc }

// CanonicalUUID is true for exactly the lower-case 8-4-4-4-12 spelling Postgres prints (one spelling per id, as internal/tenant keeps it): the form every id the adapter sends must have.
func CanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// id refuses an id that is not a canonical uuid, before anything is sent.
func (c *Client) id(op, what, v string) error {
	// MODULE-FORCED(SA-F5): the module reports a malformed job_item_id as `required`, not `malformed` (module 2 handlers.go:134-138; module 3 handlers.go:137-141), so a bad id would read as a missing field; the adapter validates every id itself and names which one is wrong.
	if !CanonicalUUID(v) {
		return c.invalid(op, "the "+what+" is not a lower-case uuid")
	}
	return nil
}

func (c *Client) call(ctx context.Context, op, method, path string, q url.Values, body any, expect ...int) (*modclient.Response, error) {
	return c.mc.Do(ctx, modclient.Request{
		Module: c.p.Module, Route: c.p.route(op), Method: method, Path: path, Query: q, Body: body, Idempotent: true, Expect: expect,
	})
}

// ---- quote -----------------------------------------------------------------------------------------------------------

// Quote prices recipientCount recipients for the tenant (POST /v1/quote). It is pure arithmetic on module 2 and arithmetic plus a feasibility verdict on module 3; it reserves nothing and
// says nothing about whether the tenant can be served (module 2's capacity shows only after run). The count is bounded here (0 to MaxRecipientCount): the module's multiplication is not.
func (c *Client) Quote(ctx context.Context, recipientCount int64) (Quote, error) {
	// MODULE-FORCED(SA-F6): module 2's quote has no upper bound on recipient_count and multiplies it by a price of up to 1000 in 64 bits, so the adapter bounds the count itself rather than trust a cost that may have wrapped.
	if recipientCount < 0 || recipientCount > MaxRecipientCount {
		return Quote{}, c.invalid(opQuote, "the recipient count must be between 0 and MaxRecipientCount")
	}
	body := struct {
		RecipientCount int64  `json:"recipient_count"`
		UserID         string `json:"user_id"`
	}{recipientCount, c.user}
	resp, err := c.call(ctx, opQuote, http.MethodPost, "/v1/quote", nil, body)
	if err != nil {
		return Quote{}, err
	}
	var w struct {
		RecipientCount int64  `json:"recipient_count"`
		Cost           int64  `json:"cost"`
		Currency       string `json:"currency"`
		Unit           int64  `json:"unit_price_credits"`
		Tier           string `json:"tier"`
		Feasible       *bool  `json:"feasible"`
		Reasons        []struct {
			Code string `json:"code"`
		} `json:"reasons"`
	}
	if err := modclient.Decode(resp, &w, c.sh.quote); err != nil {
		return Quote{}, err
	}
	switch {
	case w.RecipientCount != recipientCount:
		return Quote{}, c.contract(resp, opQuote, "recipient_count", "the module priced another recipient count than the one asked")
	case w.Cost < 0:
		return Quote{}, c.contract(resp, opQuote, "cost", "the cost is negative")
	case w.Unit < 0:
		return Quote{}, c.contract(resp, opQuote, "unit_price_credits", "the unit price is negative")
	case c.p.LinearCost && (w.Unit != 0 && recipientCount > math.MaxInt64/w.Unit || w.Cost != recipientCount*w.Unit):
		return Quote{}, c.contract(resp, opQuote, "cost", "the cost is not the recipient count times the unit price")
	}
	q := Quote{RecipientCount: w.RecipientCount, Cost: w.Cost, UnitPriceCredits: w.Unit, Currency: w.Currency, Tier: w.Tier, Body: json.RawMessage(append([]byte(nil), resp.Body...))}
	if c.p.QuoteFeasibility {
		q.Feasible = w.Feasible
		for _, r := range w.Reasons {
			q.InfeasibleReasons = append(q.InfeasibleReasons, r.Code)
		}
	}
	return q, nil
}

// ---- run -------------------------------------------------------------------------------------------------------------

// Run hands the module one job item (POST /v1/run) for the tenant. The module reads the recipient and the rendered message from the platform's own public.job_items row, which must have been
// COMMITTED before this call. Run is idempotent on jobItemID: 202 is a job just created, 200 is the job that already existed with its CURRENT state (RunResult.Created tells them apart);
// both are success, and the same call may be repeated after any failure.
//
// Refusals: 409 is ErrBackpressure on module 2 and ErrConflictUnexplained on module 3 (both also unwrap to the Refused error); 503 `paused` is modclient.Paused (wait, do not page); a 404
// means the job item is not committed or is not this tenant's (a platform bug, not a retry).
func (c *Client) Run(ctx context.Context, jobItemID string) (RunResult, error) {
	if err := c.id(opRun, "job item id", jobItemID); err != nil {
		return RunResult{}, err
	}
	body := struct {
		JobItemID string `json:"job_item_id"`
		UserID    string `json:"user_id"`
	}{jobItemID, c.user}
	resp, err := c.call(ctx, opRun, http.MethodPost, "/v1/run", nil, body, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return RunResult{}, c.mapErr(opRun, err, false)
	}
	var w struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := modclient.Decode(resp, &w, c.sh.run); err != nil {
		return RunResult{}, err
	}
	if strings.TrimSpace(w.JobID) == "" {
		return RunResult{}, c.contract(resp, opRun, "job_id", "the job id is blank")
	}
	// A replay carries the state the job has NOW; map it like a report state, never assume queued.
	cls, err := c.classOf(resp, opRun, "state", w.State)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{JobID: w.JobID, State: w.State, Class: cls, Created: resp.Status == http.StatusAccepted}, nil
}

// classOf is the class of a state of the profile's closed set, or a Contract error naming key.
func (c *Client) classOf(resp *modclient.Response, op, key, state string) (Class, error) {
	if !c.p.hasState(state) {
		return 0, c.contract(resp, op, key, "the state is not one the module advertises to this adapter (a new state is never mapped by guesswork)")
	}
	cls, ok := stateClass(state)
	if !ok { // unreachable: validate refuses a profile state the switch does not handle
		return 0, c.contract(resp, op, key, "the state is not one the adapter handles")
	}
	return cls, nil
}

// ---- items -----------------------------------------------------------------------------------------------------------

// wireItem is one job as either report spells it: the single report carries job_id, a campaign item carries job_item_id; every other key is shared or one module's.
type wireItem struct {
	JobItemID string `json:"job_item_id"`
	JobID     string `json:"job_id"`
	State     string `json:"state"`
	// MODULE-FORCED(SA-F11): attempt_count is not the number of SMTP attempts (it saturates at the module's cap and is bumped by requeues that never reached SMTP), so the adapter carries it as an advisory number and nothing in settlement reads it.
	AttemptCount      int64   `json:"attempt_count"`
	Reason            *string `json:"reason"`
	ErrorClass        *string `json:"error_class"`
	ProviderMessageID *string `json:"provider_message_id"`
	CompletedAt       *string `json:"completed_at"`
	SMTPResponse      *string `json:"smtp_response"`
	FailedAt          *string `json:"failed_at"`
	AccountID         *string `json:"account_id"`
	AccountUsername   *string `json:"account_username"`
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// instant reads a timestamp key.
func (c *Client) instant(resp *modclient.Response, op, path, key string, s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	// MODULE-FORCED(SA-F7): completed_at has two formats (fractional seconds in the single report, whole seconds in a campaign item), so the adapter reads an RFC 3339 instant with or without a fraction and never compares the two forms.
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, c.contract(resp, op, joinKey(path, key), "the value is not an RFC 3339 instant")
	}
	return &t, nil
}

// item turns one decoded wire item into an Item, refusing a state outside the vocabulary, a blank id, a negative count and an unreadable instant. single is true for the one-job report.
func (c *Client) item(resp *modclient.Response, op, path string, w wireItem, single bool) (Item, error) {
	id, idKey := w.JobItemID, "job_item_id"
	if single {
		id, idKey = w.JobID, "job_id"
	}
	if strings.TrimSpace(id) == "" {
		return Item{}, c.contract(resp, op, joinKey(path, idKey), "the id is blank")
	}
	cls, err := c.classOf(resp, op, joinKey(path, "state"), w.State)
	if err != nil {
		return Item{}, err
	}
	if w.AttemptCount < 0 {
		return Item{}, c.contract(resp, op, joinKey(path, "attempt_count"), "the count is negative")
	}
	completed, err := c.instant(resp, op, path, "completed_at", w.CompletedAt)
	if err != nil {
		return Item{}, err
	}
	failed, err := c.instant(resp, op, path, "failed_at", w.FailedAt)
	if err != nil {
		return Item{}, err
	}
	it := Item{
		State: w.State, Class: cls, AttemptCount: w.AttemptCount, Reason: w.Reason, ErrorClass: w.ErrorClass,
		ProviderMessageID: w.ProviderMessageID, CompletedAt: completed,
	}
	if single {
		it.JobID = w.JobID
	} else {
		it.JobItemID = w.JobItemID
	}
	if c.p.AccountDetail {
		it.SMTPResponse, it.FailedAt, it.AccountID, it.AccountUsername = w.SMTPResponse, failed, w.AccountID, w.AccountUsername
	}
	return it, nil
}

// ---- report: one job -------------------------------------------------------------------------------------------------

// ReportJob reads one job by the platform's own key (GET /v1/report?job_item_id=), which is the key to use: the module answers with its own job id too, and Item.JobItemID is set to the key
// asked with because module 2's single report does not carry it back.
//
// A 404 is modclient.NotFound and means one of three things the module does not tell apart: the item was never run, it is another tenant's, or the id is a typo. It is NOT a failure to
// settle on: an item the module does not know is an unsettled item. NotFoundField names which resource the module said was missing.
func (c *Client) ReportJob(ctx context.Context, jobItemID string) (Item, error) {
	if err := c.id(opReport, "job item id", jobItemID); err != nil {
		return Item{}, err
	}
	it, err := c.reportJob(ctx, url.Values{"job_item_id": {jobItemID}})
	if err != nil {
		return Item{}, err
	}
	it.JobItemID = jobItemID
	return it, nil
}

// ReportJobByID reads one job by the module's own job id (GET /v1/report?job_id=), the id run answered with. Item.JobItemID is "" (the single report does not carry it).
func (c *Client) ReportJobByID(ctx context.Context, jobID string) (Item, error) {
	if err := c.id(opReport, "job id", jobID); err != nil {
		return Item{}, err
	}
	return c.reportJob(ctx, url.Values{"job_id": {jobID}})
}

func (c *Client) reportJob(ctx context.Context, q url.Values) (Item, error) {
	q.Set("user_id", c.user)
	resp, err := c.call(ctx, opReport, http.MethodGet, "/v1/report", q, nil)
	if err != nil {
		return Item{}, c.mapErr(opReport, err, false)
	}
	var w wireItem
	if err := modclient.Decode(resp, &w, c.sh.job); err != nil {
		return Item{}, err
	}
	return c.item(resp, opReport, "", w, true)
}
