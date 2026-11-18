package m3

import (
	"context"
	"slices"
	"strconv"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// RouteQuote is the route the feasibility contracts name.
const RouteQuote = sendapi.RouteQuote

// Caps are the numbers module 3's quote states about the user's limits (quote `caps`).
//
// The quote states four numbers since the Phase 3 close-out (C2): the user's queue depth cap (`queue_depth_cap`, the user's resolved max_queued_jobs: the per-user override, else the global
// dial), the campaign size cap (`campaign_size_cap`, the resolved max_campaign_size: it was computed for the quote and never returned before), the daily send cap of the user's eligible
// accounts, and the account count cap.
type Caps struct {
	QueueDepthCap   int64
	CampaignSizeCap int64
	DailySendCap    int64
	AccountCountCap int64
}

// Capacity is the user's capacity at this moment (quote `capacity`): informational. Exceeding today's daily capacity is feasible with a long projection, never infeasible.
type Capacity struct {
	EligibleAccounts       int64
	DailyCapacityRemaining int64
	EffectiveConcurrency   int64
	// EstimatedDurationSec and EstimateBasis are set when the module projects a duration (it does for every feasible quote unless the operator turned the projection off, and never for an
	// infeasible one); nil and "" otherwise. The projection is "a projection, not a promise".
	EstimatedDurationSec *int64
	EstimateBasis        string
}

// InfeasibleReason is one reason a quote gave for `feasible: false`.
type InfeasibleReason struct {
	Code InfeasibleCode
	// Cause is the cause of a run 409 this reason predicts (suspended, campaign_active, campaign_cap, queue_cap), "" for a reason that predicts none (dispatch_disabled, no_eligible_accounts,
	// all_accounts_unverified, from_gate_blocked). It is the field that matches a quote to the refusal run would answer: sendapi.CausesOf of that error names the same causes.
	Cause sendapi.Cause
	// Limit, Requested and Current are set for the two caps (the cap, the count asked, and what the cap already holds); nil otherwise.
	Limit     *int64
	Requested *int64
	Current   *int64
	// CapName names the cap that bound ("queue_depth_cap" or "campaign_size_cap"); "" when none.
	CapName string
}

// Warning is one quote warning: `long_duration` when the projection crosses the module's threshold.
type Warning struct {
	Code                 string
	EstimatedDurationSec int64
	ThresholdSec         int64
}

// Feasibility is module 3's quote typed: whether this recipient count can be accepted as submitted, why not, the caps, the capacity and the warnings. The price fields are inert (flat
// subscription: unit price 0) and are carried because the shared body checks them; the module's raw body and raw reason codes are NOT carried (a reason it does not publish is the
// Unrecognised word, never its text).
//
// Since the Phase 3 close-out (C2) the verdict is exact about run: the quote and run read the same function, so a feasible quote is a prediction that run accepts the recipients one at a time
// right now (it cannot know the future: a campaign that finishes, or another request that fills the queue, between the quote and the run), and each cause of a run refusal that applies is a
// reason of its own with its Cause.
type Feasibility struct {
	// RecipientCount is the count the module priced (always the one asked); Cost, UnitPriceCredits, Currency and Tier are the inert price fields.
	RecipientCount   int64
	Cost             int64
	UnitPriceCredits int64
	Currency         string
	Tier             string
	// Feasible is the module's verdict. It is true exactly when Reasons is empty (anything else is a Contract error).
	Feasible bool
	Reasons  []InfeasibleReason
	Caps     Caps
	Capacity Capacity
	Warnings []Warning
}

// Causes are the causes of a run refusal the quote predicts, in the module's order (the order of the reasons): exactly the causes a run of the same recipients would name in its 409. Empty for a
// feasible quote and for one whose reasons predict no run refusal.
func (f Feasibility) Causes() []sendapi.Cause {
	var out []sendapi.Cause
	for _, r := range f.Reasons {
		if r.Cause != "" {
			out = append(out, r.Cause)
		}
	}
	return out
}

// Predicts reports whether the quote predicts the cause.
func (f Feasibility) Predicts(c sendapi.Cause) bool { return slices.Contains(f.Causes(), c) }

// reasonCause is the cause of a run refusal each reason predicts (run_causes.go of the module: the quote renders the run causes as reasons, queue_cap under the long-standing
// exceeds_queue_cap).
var reasonCause = map[InfeasibleCode]sendapi.Cause{
	InfeasibleSuspended: sendapi.CauseSuspended, InfeasibleCampaignActive: sendapi.CauseCampaignActive, InfeasibleCampaignCap: sendapi.CauseCampaignCap, InfeasibleExceedsQueueCap: sendapi.CauseQueueCap,
}

// feasibilityShape is what Feasibility decodes the quote body with (the keys module 3 adds to the shared four): the verdict, the reasons, the caps, the capacity and the warnings. It is
// listed as the Shape of the feasibility contracts and used by Feasibility, nowhere else.
var feasibilityShape = modclient.Object(
	modclient.Req("feasible", modclient.Bool),
	modclient.Req("reasons", modclient.ArrayOf(modclient.Object(
		modclient.Req("code", modclient.Str), modclient.Opt("limit", modclient.Int), modclient.Opt("requested", modclient.Int), modclient.Opt("cap_name", modclient.Str),
		// the additive keys of the close-out (C2): on the four reasons that mirror a run refusal, the cause and, on the two caps, what the cap already holds
		modclient.Opt("cause", modclient.Str), modclient.Opt("current", modclient.Int),
	))),
	modclient.Req("caps", modclient.Object(
		modclient.Req("queue_depth_cap", modclient.Int), modclient.Req("campaign_size_cap", modclient.Int), modclient.Req("daily_send_cap", modclient.Int), modclient.Req("account_count_cap", modclient.Int),
	)),
	modclient.Req("capacity", modclient.Object(
		modclient.Req("eligible_accounts", modclient.Int), modclient.Req("daily_capacity_remaining", modclient.Int), modclient.Req("effective_concurrency", modclient.Int),
		modclient.Opt("estimated_duration_sec", modclient.Int), modclient.Opt("estimate_basis", modclient.Str),
	)),
	modclient.Req("warnings", modclient.ArrayOf(modclient.Object(
		modclient.Req("code", modclient.Str), modclient.Req("estimated_duration_sec", modclient.Int), modclient.Req("threshold_sec", modclient.Int),
	))),
)

// Feasibility asks module 3 whether recipientCount recipients can be accepted for the tenant as a NEW campaign (POST /v1/quote without a campaign id: it starts with no job against the
// campaign size cap, and any job of the user that is not final blocks it, as another active campaign), read by the shared body, and types the answer. It reserves nothing and is pure. The
// count is bounded like Quote's (0 to sendapi.MaxRecipientCount). FeasibilityForCampaign is the same for recipients that join a campaign that already has jobs. A feasible verdict with reasons, an infeasible one without, a negative number anywhere, or a projection that
// has a duration and no basis (or the reverse) is a Contract error naming the key. ON ANY ERROR THE RESULT IS THE ZERO VALUE.
func (c *Client) Feasibility(ctx context.Context, recipientCount int64) (Feasibility, error) {
	return c.feasibility(ctx, "", recipientCount)
}

// FeasibilityForCampaign asks whether recipientCount MORE recipients can be accepted into the campaign campaignID (the optional campaign_id of module 3's quote): the jobs the campaign already
// holds count against the campaign size cap and its own active jobs are not "another campaign", while any job of the user in another campaign still blocks it. It is the question run answers
// one item at a time, asked before the first item. The id must be a lower-case uuid (refused before anything is sent).
func (c *Client) FeasibilityForCampaign(ctx context.Context, campaignID string, recipientCount int64) (Feasibility, error) {
	if !sendapi.CanonicalUUID(campaignID) {
		return Feasibility{}, invalid("m3.quote", "the campaign id is not a lower-case uuid")
	}
	return c.feasibility(ctx, campaignID, recipientCount)
}

func (c *Client) feasibility(ctx context.Context, campaignID string, recipientCount int64) (Feasibility, error) {
	const route = "m3.quote"
	var q sendapi.Quote
	var err error
	if campaignID == "" {
		q, err = c.Client.Quote(ctx, recipientCount)
	} else {
		q, err = c.Client.QuoteForCampaign(ctx, campaignID, recipientCount)
	}
	if err != nil {
		return Feasibility{}, err
	}
	var w struct {
		Feasible bool `json:"feasible"`
		Reasons  []struct {
			Code      string `json:"code"`
			Limit     *int64 `json:"limit"`
			Requested *int64 `json:"requested"`
			CapName   string `json:"cap_name"`
			Cause     string `json:"cause"`
			Current   *int64 `json:"current"`
		} `json:"reasons"`
		Caps struct {
			QueueDepthCap   int64 `json:"queue_depth_cap"`
			CampaignSizeCap int64 `json:"campaign_size_cap"`
			DailySendCap    int64 `json:"daily_send_cap"`
			AccountCountCap int64 `json:"account_count_cap"`
		} `json:"caps"`
		Capacity struct {
			EligibleAccounts       int64  `json:"eligible_accounts"`
			DailyCapacityRemaining int64  `json:"daily_capacity_remaining"`
			EffectiveConcurrency   int64  `json:"effective_concurrency"`
			EstimatedDurationSec   *int64 `json:"estimated_duration_sec"`
			EstimateBasis          string `json:"estimate_basis"`
		} `json:"capacity"`
		Warnings []struct {
			Code                 string `json:"code"`
			EstimatedDurationSec int64  `json:"estimated_duration_sec"`
			ThresholdSec         int64  `json:"threshold_sec"`
		} `json:"warnings"`
	}
	resp := &modclient.Response{Status: 200, Body: q.Body}
	if err := modclient.Decode(resp, &w, feasibilityShape); err != nil {
		return Feasibility{}, restamp(err, route)
	}
	bad := func(key, msg string) error { return contract(nil, route, key, msg) }
	for _, k := range []struct {
		key string
		n   int64
	}{
		{"caps.queue_depth_cap", w.Caps.QueueDepthCap}, {"caps.campaign_size_cap", w.Caps.CampaignSizeCap}, {"caps.daily_send_cap", w.Caps.DailySendCap}, {"caps.account_count_cap", w.Caps.AccountCountCap},
		{"capacity.eligible_accounts", w.Capacity.EligibleAccounts}, {"capacity.daily_capacity_remaining", w.Capacity.DailyCapacityRemaining},
		{"capacity.effective_concurrency", w.Capacity.EffectiveConcurrency},
	} {
		if k.n < 0 {
			return Feasibility{}, bad(k.key, "the number is negative")
		}
	}
	if w.Capacity.EstimatedDurationSec != nil && *w.Capacity.EstimatedDurationSec < 0 {
		return Feasibility{}, bad("capacity.estimated_duration_sec", "the number is negative")
	}
	if (w.Capacity.EstimatedDurationSec != nil) != (w.Capacity.EstimateBasis != "") {
		return Feasibility{}, bad("capacity.estimate_basis", "the projection names a duration without its basis, or a basis without its duration")
	}
	if w.Feasible != (len(w.Reasons) == 0) {
		return Feasibility{}, bad("feasible", "the verdict contradicts the reasons (feasible exactly when there are none)")
	}
	f := Feasibility{
		RecipientCount: q.RecipientCount, Cost: q.Cost, UnitPriceCredits: q.UnitPriceCredits, Currency: q.Currency, Tier: q.Tier, Feasible: w.Feasible,
		Caps:     Caps{QueueDepthCap: w.Caps.QueueDepthCap, CampaignSizeCap: w.Caps.CampaignSizeCap, DailySendCap: w.Caps.DailySendCap, AccountCountCap: w.Caps.AccountCountCap},
		Capacity: Capacity{EligibleAccounts: w.Capacity.EligibleAccounts, DailyCapacityRemaining: w.Capacity.DailyCapacityRemaining, EffectiveConcurrency: w.Capacity.EffectiveConcurrency, EstimatedDurationSec: w.Capacity.EstimatedDurationSec, EstimateBasis: fixedWord(w.Capacity.EstimateBasis)},
	}
	for i, r := range w.Reasons {
		key := "reasons[" + strconv.Itoa(i) + "]"
		if (r.Limit != nil && *r.Limit < 0) || (r.Requested != nil && *r.Requested < 0) || (r.Current != nil && *r.Current < 0) {
			return Feasibility{}, bad(key+".limit", "a number is negative")
		}
		code := quoteReason(r.Code)
		cause := sendapi.Cause(r.Cause)
		// the cause is one of the closed set, and a reason that mirrors a run refusal names the cause it predicts (the one pairing the module states): a word outside the set, or a pairing it
		// never makes, is a Contract error, never a guess (a reason code the adapter does not know keeps the cause it names)
		if r.Cause != "" && !cause.Valid() {
			return Feasibility{}, bad(key+".cause", "the cause is not one of the closed causes of a run refusal")
		}
		if want, ok := reasonCause[code]; ok && cause != want {
			return Feasibility{}, bad(key+".cause", "the reason does not name the cause of the run refusal it predicts")
		}
		f.Reasons = append(f.Reasons, InfeasibleReason{Code: code, Cause: cause, Limit: r.Limit, Requested: r.Requested, Current: r.Current, CapName: fixedWord(r.CapName)})
	}
	for i, x := range w.Warnings {
		if x.EstimatedDurationSec < 0 || x.ThresholdSec < 0 {
			return Feasibility{}, bad("warnings["+strconv.Itoa(i)+"].threshold_sec", "a number is negative")
		}
		f.Warnings = append(f.Warnings, Warning{Code: fixedWord(x.Code), EstimatedDurationSec: x.EstimatedDurationSec, ThresholdSec: x.ThresholdSec})
	}
	return f, nil
}
