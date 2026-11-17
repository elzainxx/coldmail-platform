// Package m2 is the platform's adapter for module 2 (coldmail-send-managed): quote, run (idempotent on the platform's job_item_id), report (one job; a campaign with full pagination),
// cancel, and the module's describe with its vocabularies compared with what the adapter handles. Phase 3, chunk B4.
//
// It is thin on purpose. Everything module 2 shares with module 3 lives in internal/adapters/sendapi (parameterised by a Profile); this package is module 2's Profile, the one route only
// module 2 has (cancel), the construction from the platform's configuration, and the poll cadence the research derived. It holds no storage, no retry loop and no job logic: Phase 4
// owns jobs.
//
// Tenancy: every call of a Client carries the user_id the Client was built for. The module does not check it against a user list on these routes and answers a stranger's id with "nothing
// there" (404, an empty 200, {0,0,0}), so a tenant mix-up surfaces as nothing, not as an error: build one Client per tenant with Adapter.For and never share it.
package m2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// ErrNotConfigured: PLATFORM_M2_URL or PLATFORM_M2_SERVICE_TOKEN is not set, so no call to module 2 can be made. The service still starts without them; the call refuses with this error
// (which is also a modclient.Invalid error: nothing was sent).
var ErrNotConfigured = errors.New("m2: module 2 is not configured (PLATFORM_M2_URL and PLATFORM_M2_SERVICE_TOKEN)")

// The poll cadence the research derived from module 2's code (m2-api-and-worker.md 4.6). Documentation as constants: there is no polling loop here, Phase 4 owns jobs.
//
// Module 2 has no rate limit on its platform API, but every request costs a bcrypt verify of the service token against every configured hash and a database round trip with a 10 second
// bound; its campaign report runs the totals query (with the eligibility subquery) and reads three config rows on every call. Dispatch itself is a polling loop (dispatch_tick_interval_ms,
// seeded 1000, plus an immediate kick on every run), so polling faster than the tick finds nothing new. Poll a campaign every PollInterval, read pages of ReportPageSize, and stop when the
// module says is_complete AND every item the platform submitted is terminal (module 2's totals count only the jobs run has accepted, so is_complete can be true at a partial total).
const (
	// PollIntervalMin and PollIntervalMax bound a reasonable per-campaign poll; PollInterval is the default.
	PollIntervalMin = 2 * time.Second
	PollInterval    = 3 * time.Second
	PollIntervalMax = 5 * time.Second
	// ReportPageSize is the page size of a campaign report walk: the module's maximum, the fewest round trips.
	ReportPageSize = sendapi.MaxPageSize
	// DispatchTick is module 2's seeded dispatch tick (dispatch_tick_interval_ms): the floor under how soon a submitted item can start.
	DispatchTick = time.Second
)

// Profile is module 2's profile of the shared sender body (research m2 sections 2 and 4.3, and describe's own vocabularies).
func Profile() sendapi.Profile {
	return sendapi.Profile{
		Module:         modclient.M2,
		DescribeModule: "send_managed",
		// send_managed.job_state in enum order. Terminal: sent, failed, bounced, cancelled.
		JobStates:    []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"},
		ErrorClasses: []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		// The closed list in the module's contract order (item_reason.go:65-78).
		ItemReasons: []string{
			"pool_exhausted", "attempts_exhausted", "bounce_permanent", "cancelled", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown",
			"no_eligible_account", "waiting_for_capacity", "dispatch_paused",
		},
		// Domain A's closed set (platformapi/errors.go): module 3 adds unprocessable and rate_limited.
		ErrorCodes:       []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "internal_error", "paused"},
		AlwaysPresent:    true,  // the single report carries reason and error_class as keys, null when there is nothing to say
		AccountDetail:    false, // operator-owned pools: no user accounts
		QuoteFeasibility: false, // quote is arithmetic and nothing else
		LinearCost:       true,  // cost = recipient_count * unit_price_credits
		RunConflict:      sendapi.ConflictBackpressure,
	}
}

// Adapter is the tenant-free part of the adapter: the module client (one service token serves every tenant) and the operations that are not scoped to a tenant. Build it with FromConfig and
// get a tenant's Client with For.
type Adapter struct {
	mc *modclient.Client
}

// FromConfig builds the adapter from PLATFORM_M2_URL and PLATFORM_M2_SERVICE_TOKEN and the shared module-call dials (timeout, retries, backoff: modclient.FromConfig). A configuration with
// either missing is ErrNotConfigured, which is also a modclient.Invalid error; nothing is sent. The transport has no proxy and follows no redirect (modclient).
func FromConfig(cfg config.Config) (*Adapter, error) {
	if cfg.M2URL == "" || cfg.M2ServiceToken.Reveal() == "" {
		return nil, fmt.Errorf("%w: %w", ErrNotConfigured, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M2, Route: "m2.new", Message: "the base URL or the service token is not configured"})
	}
	mc, err := modclient.New(modclient.M2, cfg.M2URL, cfg.M2ServiceToken, nil, modclient.FromConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &Adapter{mc: mc}, nil
}

// NewAdapter wraps a module client built for module 2 (the harness builds one with its own transport).
func NewAdapter(mc *modclient.Client) (*Adapter, error) {
	if mc == nil || mc.Module() != modclient.M2 {
		return nil, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M2, Route: "m2.new", Message: "the module client is missing or was built for another module"}
	}
	return &Adapter{mc: mc}, nil
}

// For builds the Client of one tenant (public.users.id). The all-zero system user, an empty id and anything that is not a lower-case uuid are refused before any network call
// (sendapi.New: tenant.ErrSystemUser, tenant.ErrBadID).
func (a *Adapter) For(userID string) (*Client, error) {
	sc, err := sendapi.New(a.mc, Profile(), userID, sendapi.Options{PageSize: ReportPageSize})
	if err != nil {
		return nil, err
	}
	return &Client{Client: sc}, nil
}

// Describe reads module 2's self-description, checking that it is module 2's (a describe that names another module is a wiring mistake). ON ANY ERROR THE RESULT IS NIL.
func (a *Adapter) Describe(ctx context.Context) (*modclient.Describe, error) {
	return sendapi.Describe(ctx, a.mc, Profile())
}

// Vocabularies are the value sets module 2 advertises (job_state, error_class, item_reason, ...). ON ANY ERROR THE RESULT IS NIL.
func (a *Adapter) Vocabularies(ctx context.Context) (modclient.Vocabularies, error) {
	return sendapi.Vocabularies(ctx, a.mc, Profile())
}

// CheckVocabularies compares what module 2 advertises with what this adapter handles (the VocabDiff of each vocabulary and the error codes). A vocabulary that is gone, an expected value that
// is missing, a state the adapter does not handle, or an error code that is missing is a Contract error naming the vocabulary (sendapi.VocabReport.Breaking); an additive difference is in the
// report. ON ANY ERROR THE REPORT IS THE ZERO VALUE.
func (a *Adapter) CheckVocabularies(ctx context.Context) (sendapi.VocabReport, error) {
	return sendapi.CheckVocabularies(ctx, a.mc, Profile())
}

// Client is the adapter for one tenant: the four shared routes (through the embedded sendapi.Client) and cancel.
type Client struct {
	*sendapi.Client
}

// CancelPosition is module 2's answer to a cancel: the campaign's position AFTER the call, not a delta.
type CancelPosition struct {
	CampaignID string
	// Cancelled is EVERY job of the campaign now in the terminal state `cancelled`, those cancelled by earlier calls included.
	Cancelled int64
	// InFlight are the jobs assigned or sending: they were already claimed, they finish and are reported once, truthfully; if their attempt fails they end cancelled instead of being retried.
	InFlight int64
	// AlreadyTerminal are the jobs that were sent, failed or bounced.
	AlreadyTerminal int64
}

// Empty is true for {0, 0, 0}: the campaign has nothing to cancel, or the module does not know it.
func (p CancelPosition) Empty() bool {
	// MODULE-FORCED(M2-F2): a cancel of a campaign that is unknown, a typo or another tenant's answers 200 with all zeros (never a 404), so the adapter cannot tell "nothing to cancel" from "no such campaign" and exposes only Empty, never a claim that the campaign existed.
	return p.Cancelled == 0 && p.InFlight == 0 && p.AlreadyTerminal == 0
}

var cancelShape = modclient.Object(
	modclient.Req("campaign_id", modclient.Str), modclient.Req("cancelled", modclient.Int), modclient.Req("in_flight", modclient.Int), modclient.Req("already_terminal", modclient.Int),
)

// Cancel asks module 2 to cancel a campaign (POST /v1/cancel). What it does (research 2.5, 4.5):
//
//   - Every job of the campaign that is still `queued` becomes `cancelled`: terminal, never sent, and free (charge rule M2-08).
//   - A job already assigned or sending is not stopped: it finishes, is reported once, and if its attempt fails it ends cancelled instead of returning to the queue.
//   - A second cancel changes nothing and answers the same numbers (it is a no-op, so it is safe to repeat and the call is retried like a read).
//   - It does NOT freeze the campaign: a run for a new job item of the same campaign afterwards is accepted and sent, so the platform must stop submitting.
//   - It is not gated by the dispatch rung: it works while the module is paused.
//
// The answer is a position (see CancelPosition), and the split between Cancelled and InFlight is timing dependent while anything is claimed: assert the partition and the end state, not the
// split. ON ANY ERROR THE POSITION IS THE ZERO VALUE.
func (c *Client) Cancel(ctx context.Context, campaignID string) (CancelPosition, error) {
	const op = "cancel"
	route := "m2." + op
	if !sendapi.CanonicalUUID(campaignID) {
		return CancelPosition{}, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M2, Route: route, Message: "the campaign id is not a lower-case uuid"}
	}
	body := struct {
		CampaignID string `json:"campaign_id"`
		UserID     string `json:"user_id"`
	}{campaignID, c.UserID()}
	resp, err := c.Module().Do(ctx, modclient.Request{Module: modclient.M2, Route: route, Method: http.MethodPost, Path: "/v1/cancel", Body: body, Idempotent: true})
	if err != nil {
		return CancelPosition{}, err
	}
	var w struct {
		CampaignID      string `json:"campaign_id"`
		Cancelled       int64  `json:"cancelled"`
		InFlight        int64  `json:"in_flight"`
		AlreadyTerminal int64  `json:"already_terminal"`
	}
	if err := modclient.Decode(resp, &w, cancelShape); err != nil {
		return CancelPosition{}, err
	}
	bad := func(key, msg string) error {
		return &modclient.Error{Kind: modclient.Contract, Module: modclient.M2, Route: route, Status: resp.Status, RequestID: resp.RequestID, Key: key, Message: msg, Sent: true}
	}
	switch {
	case !strings.EqualFold(w.CampaignID, campaignID):
		return CancelPosition{}, bad("campaign_id", "the answer is for another campaign than the one cancelled")
	case w.Cancelled < 0:
		return CancelPosition{}, bad("cancelled", "the count is negative")
	case w.InFlight < 0:
		return CancelPosition{}, bad("in_flight", "the count is negative")
	case w.AlreadyTerminal < 0:
		return CancelPosition{}, bad("already_terminal", "the count is negative")
	}
	// MODULE-FORCED(M2-F1): cancel answers the campaign's position after the call (cancelled counts every cancelled job, earlier calls included) and its cancelled / in_flight split depends on timing while anything is claimed, so the adapter returns the position as it is and computes no delta.
	return CancelPosition{CampaignID: campaignID, Cancelled: w.Cancelled, InFlight: w.InFlight, AlreadyTerminal: w.AlreadyTerminal}, nil
}
