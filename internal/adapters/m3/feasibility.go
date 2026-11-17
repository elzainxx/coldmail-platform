package m3

import (
	"context"
	"strconv"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// RouteQuote is the route the feasibility contracts name.
const RouteQuote = sendapi.RouteQuote

// Caps are the numbers module 3's quote states about the user's limits (quote `caps`).
//
// WHAT THE MODULE SAYS, AND WHAT IT CANNOT (spec open decision 3). The quote states exactly three numbers: the user's queue depth cap (`queue_depth_cap`, which is the user's resolved
// max_queued_jobs: the per-user override, else the global dial), the daily send cap of the user's eligible accounts, and the account count cap. It does NOT state the campaign size cap
// (max_campaign_size, default 50000, per-user overridable): the module computes it for the quote and never returns it, and no Domain A route returns it (only the admin routes can read it).
// So the field a campaign size pre-check needs is not here, and RunFacts.CampaignSizeCap is the platform's to supply or to leave unchecked. And the quote predicts a refusal of `run` only
// partly: it never reads the suspension, it does not know whether another campaign of the user is active, it ignores jobs already queued (it compares the recipient count with the cap
// alone), and it never applies the campaign size cap. A quote that says feasible is therefore not a promise that run will be accepted.
type Caps struct {
	QueueDepthCap   int64
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
	// Limit and Requested are set for exceeds_queue_cap (the cap and the count asked); nil otherwise.
	Limit     *int64
	Requested *int64
	// CapName names the cap that bound ("queue_depth_cap"); "" when none.
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

// QueueDepthLimit is the queue depth cap the module stated, as the fact RunFacts takes.
func (f Feasibility) QueueDepthLimit() Limit { return KnownLimit(f.Caps.QueueDepthCap) }

// feasibilityShape is what Feasibility decodes the quote body with (the keys module 3 adds to the shared four): the verdict, the reasons, the caps, the capacity and the warnings. It is
// listed as the Shape of the feasibility contracts and used by Feasibility, nowhere else.
var feasibilityShape = modclient.Object(
	modclient.Req("feasible", modclient.Bool),
	modclient.Req("reasons", modclient.ArrayOf(modclient.Object(
		modclient.Req("code", modclient.Str), modclient.Opt("limit", modclient.Int), modclient.Opt("requested", modclient.Int), modclient.Opt("cap_name", modclient.Str),
	))),
	modclient.Req("caps", modclient.Object(
		modclient.Req("queue_depth_cap", modclient.Int), modclient.Req("daily_send_cap", modclient.Int), modclient.Req("account_count_cap", modclient.Int),
	)),
	modclient.Req("capacity", modclient.Object(
		modclient.Req("eligible_accounts", modclient.Int), modclient.Req("daily_capacity_remaining", modclient.Int), modclient.Req("effective_concurrency", modclient.Int),
		modclient.Opt("estimated_duration_sec", modclient.Int), modclient.Opt("estimate_basis", modclient.Str),
	)),
	modclient.Req("warnings", modclient.ArrayOf(modclient.Object(
		modclient.Req("code", modclient.Str), modclient.Req("estimated_duration_sec", modclient.Int), modclient.Req("threshold_sec", modclient.Int),
	))),
)

// Feasibility asks module 3 whether recipientCount recipients can be accepted for the tenant (POST /v1/quote, read by the shared body) and types the answer. It reserves nothing and is
// pure. The count is bounded like Quote's (0 to sendapi.MaxRecipientCount). A feasible verdict with reasons, an infeasible one without, a negative number anywhere, or a projection that
// has a duration and no basis (or the reverse) is a Contract error naming the key. ON ANY ERROR THE RESULT IS THE ZERO VALUE.
func (c *Client) Feasibility(ctx context.Context, recipientCount int64) (Feasibility, error) {
	const route = "m3.quote"
	q, err := c.Client.Quote(ctx, recipientCount)
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
		} `json:"reasons"`
		Caps struct {
			QueueDepthCap   int64 `json:"queue_depth_cap"`
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
		{"caps.queue_depth_cap", w.Caps.QueueDepthCap}, {"caps.daily_send_cap", w.Caps.DailySendCap}, {"caps.account_count_cap", w.Caps.AccountCountCap},
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
	// MODULE-FORCED(M3-F2): the quote never reads the suspension, another active campaign, the jobs already queued or the campaign size cap, so a feasible verdict is returned as the module's verdict about the request alone and never as a prediction that run will accept (PreCheckRun re-derives the causes from the platform's own state).
	f := Feasibility{
		RecipientCount: q.RecipientCount, Cost: q.Cost, UnitPriceCredits: q.UnitPriceCredits, Currency: q.Currency, Tier: q.Tier, Feasible: w.Feasible,
		Caps:     Caps{QueueDepthCap: w.Caps.QueueDepthCap, DailySendCap: w.Caps.DailySendCap, AccountCountCap: w.Caps.AccountCountCap},
		Capacity: Capacity{EligibleAccounts: w.Capacity.EligibleAccounts, DailyCapacityRemaining: w.Capacity.DailyCapacityRemaining, EffectiveConcurrency: w.Capacity.EffectiveConcurrency, EstimatedDurationSec: w.Capacity.EstimatedDurationSec, EstimateBasis: fixedWord(w.Capacity.EstimateBasis)},
	}
	for i, r := range w.Reasons {
		if (r.Limit != nil && *r.Limit < 0) || (r.Requested != nil && *r.Requested < 0) {
			return Feasibility{}, bad("reasons["+strconv.Itoa(i)+"].limit", "a number is negative")
		}
		f.Reasons = append(f.Reasons, InfeasibleReason{Code: quoteReason(r.Code), Limit: r.Limit, Requested: r.Requested, CapName: fixedWord(r.CapName)})
	}
	for i, x := range w.Warnings {
		if x.EstimatedDurationSec < 0 || x.ThresholdSec < 0 {
			return Feasibility{}, bad("warnings["+strconv.Itoa(i)+"].threshold_sec", "a number is negative")
		}
		f.Warnings = append(f.Warnings, Warning{Code: fixedWord(x.Code), EstimatedDurationSec: x.EstimatedDurationSec, ThresholdSec: x.ThresholdSec})
	}
	return f, nil
}
