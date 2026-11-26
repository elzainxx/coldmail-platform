// Package m2 is the platform's adapter for module 2 (coldmail-send-managed): quote, run (idempotent on the platform's job_item_id), report (one job; a campaign with full pagination),
// cancel (the shared body's: module 3 has the same route since Phase 3 close-out C1), and the module's describe with its vocabularies compared with what the adapter handles. Phase 3, chunk B4.
//
// It is thin on purpose. Everything module 2 shares with module 3 lives in internal/adapters/sendapi (parameterised by a Profile); this package is module 2's Profile, the construction from the platform's configuration, and the poll cadence the research derived. It holds no storage, no retry loop and no job logic: Phase 4
// owns jobs.
//
// Tenancy: every call of a Client carries the user_id the Client was built for. The module does not check it against a user list on these routes and answers a stranger's id with "nothing
// there" (404, an empty 200, {0,0,0}), so a tenant mix-up surfaces as nothing, not as an error: build one Client per tenant with Adapter.For and never share it.
package m2

import (
	"context"
	"errors"
	"fmt"
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
		Cancel:           true,  // POST /v1/cancel (the shared body's Cancel)
		// POST /v1/pause and POST /v1/resume (send_managed_0031, additive; the shared body's Pause and Resume): describe.capabilities.campaign_pause_resume says the module has them. A describe without the key
		// is a module older than that migration and means "cannot", never an error.
		PauseCapability: "campaign_pause_resume",
		// MODULE-FORCED(M2-F3): module 2 answers run's 409 with code conflict and no details (owner ruling OD-5: back-pressure, the platform's job engine of phase 4 serialises a user's campaigns), so the adapter can only call it back-pressure and can never say which of its causes it was.
		RunConflict: sendapi.ConflictBackpressure,
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

// Client is the adapter for one tenant: the four shared routes and cancel, all through the embedded sendapi.Client (Cancel is the shared body's: POST /v1/cancel, the same on module 3).
type Client struct {
	*sendapi.Client
}

// CancelPosition is module 2's answer to a cancel: the campaign's position AFTER the call, not a delta. It is the shared body's (sendapi.CancelPosition): module 3 answers the same.
type CancelPosition = sendapi.CancelPosition
