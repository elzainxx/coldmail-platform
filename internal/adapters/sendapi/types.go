package sendapi

import (
	"encoding/json"
	"time"

	"github.com/coldmail-platform/platform/internal/money"
)

// Quote is a module's answer to POST /v1/quote: arithmetic and (module 3) feasibility, never a reservation. Module 2's quote validates nothing about capacity (research m2 2.1): a quote
// is a price, not a promise that the tenant can be served.
type Quote struct {
	// RecipientCount is the count the module priced; the client refuses an answer for any other count.
	RecipientCount int64
	// Cost is the module's total in credits. Module 3's is inert (0 at the seeded price).
	Cost int64
	// UnitPriceCredits is a live dial of the module (0..1000 on module 2): read it per use, never cache it.
	UnitPriceCredits int64
	Currency         string
	Tier             string
	// Feasible and InfeasibleReasons are set only by a profile with QuoteFeasibility (module 3): `feasible`, and the `code` of each `reasons[]` entry. Nil / empty otherwise.
	Feasible          *bool
	InfeasibleReasons []string
	// Body is the module's whole answer, for the keys only one module has (module 3's caps, capacity and warnings): decode it with the module's own shape.
	Body json.RawMessage
}

// RunResult is a module's answer to POST /v1/run.
type RunResult struct {
	JobID string
	// State is the job's state NOW: `queued` for a job just created, but whatever it has become for a replay (it can already be sent, bounced, failed or cancelled).
	State string
	// Class is the class of State.
	Class Class
	// Created is true for the module's 202 (a new job), false for its 200 (this job item already had a job: the replay of an idempotent call). Both are success.
	Created bool
}

// Item is one job as a module reported it: a campaign item, or the single-job report (JobID set; JobItemID is the key the caller asked with, because module 2's single report does not
// carry it back). Optional fields are nil when the module omitted them or sent null.
type Item struct {
	JobItemID string
	// JobID is the module's own job id: set by the single-job report, "" in a campaign item (which does not carry it).
	JobID string
	State string
	// Class is the class of State (stateClass).
	Class Class
	// AttemptCount is ADVISORY. It is not the number of SMTP attempts (it saturates at the module's cap and is bumped by requeues that never reached SMTP); nothing in settlement reads it.
	AttemptCount      int64
	Reason            *string
	ErrorClass        *string
	ProviderMessageID *string
	CompletedAt       *time.Time

	// Module 3 only (nil for a profile without AccountDetail).
	SMTPResponse    *string
	FailedAt        *time.Time
	AccountID       *string
	AccountUsername *string
}

// Totals are the campaign counts of the module's report. They are one query of the module, not a snapshot with the item list (research m2 4.2, m3 4.4).
type Totals struct {
	Total              int64
	Sent               int64
	Bounced            int64
	Failed             int64
	InFlight           int64
	Pending            int64
	WaitingForCapacity int64
	// Cancelled is 0 for a module without a `cancelled` state.
	Cancelled int64
}

// Progress is the module's own progress block.
type Progress struct {
	Terminal         int64
	FractionComplete float64
	IsComplete       bool
}

// AccountTotals is one row of module 3's totals_by_account.
type AccountTotals struct {
	AccountID       string
	AccountUsername string
	Sent            int64
	Bounced         int64
	Failed          int64
	InFlight        int64
}

// CampaignPage is one page of a campaign report.
type CampaignPage struct {
	CampaignID     string
	DispatchPaused bool
	Totals         Totals
	Progress       Progress
	// ByAccount is module 3's totals_by_account (nil for module 2).
	ByAccount []AccountTotals
	Items     []Item
	// NextCursor is the module's opaque cursor, passed back verbatim; "" on the last page.
	NextCursor string
	// PageSize is the page size the module says it used (the module clamps what is asked: read this, not what was sent).
	PageSize int
}

// CampaignReport is a whole campaign report: every page read, or nothing.
type CampaignReport struct {
	CampaignID     string
	DispatchPaused bool
	// Totals, Progress and ByAccount are those of the LAST page read, the freshest the module said (the totals are re-read with every page and are not a snapshot with the items).
	Totals    Totals
	Progress  Progress
	ByAccount []AccountTotals
	// Items are every item of the walk, in the module's order, each job item once.
	Items []Item
	Pages int
}

// ItemsMatchTotal reports whether the number of items read equals the module's total: true for a campaign that no longer receives run calls, and the check a caller makes before it
// believes a listing is the whole campaign. The module's `is_complete` is not a substitute: it is false for a campaign of total 0 and says nothing about what the platform submitted;
// compare Totals.Total with the number of items the platform itself submitted.
func (r CampaignReport) ItemsMatchTotal() bool { return int64(len(r.Items)) == r.Totals.Total }

// NothingKnown reports whether the module knows nothing under this campaign id for this user: no jobs and no items. Module 2 and module 3 answer an unknown campaign, a typo and another
// tenant's campaign with the same empty 200, never a 404, so "nothing known" is the strongest thing a caller can conclude: not "it does not exist", and not "it is complete".
func (r CampaignReport) NothingKnown() bool {
	// MODULE-FORCED(SA-F10): an unknown campaign, a typo and another tenant's campaign all answer an empty 200 (no 404 anywhere), so the adapter can only say that nothing is known under the id and never that the campaign does not exist.
	return r.Totals.Total == 0 && len(r.Items) == 0
}

// WalkStats says what a successful walk read.
type WalkStats struct {
	Pages int
	Items int
}

// Pending is an item that is not an outcome yet: reported back to the caller as "not yet", never settled.
type Pending struct {
	JobItemID string
	State     string
	Class     Class
}

// Unmapped is a terminal item whose outcome the charge table cannot classify: flagged to the caller, never charged, never guessed (CLAUDE.md rule 20).
type Unmapped struct {
	JobItemID string
	State     string
	Reason    string
	// Rule is the id of the charge-table row that said so, or "none".
	Rule string
	Note string
}

// Classified is what a batch of reported items becomes: the terminal items the charge table covers (to hand to money.Settle, which decides Charge or Free from the same table), the items
// that are not final yet, and the terminal items the table cannot classify.
type Classified struct {
	Settle   []money.SettleItem
	NotYet   []Pending
	Unmapped []Unmapped
}

// CancelPosition is a module's answer to a cancel: the campaign's position AFTER the call, not a delta (module 2, and module 3 since Phase 3 close-out C1).
type CancelPosition struct {
	CampaignID string
	// Cancelled is EVERY job of the campaign now in the terminal state `cancelled`, those cancelled by earlier calls included.
	Cancelled int64
	// InFlight are the jobs assigned or sending: they were already claimed, they finish and are reported once, truthfully; if their attempt fails they end cancelled instead of being retried.
	InFlight int64
	// AlreadyTerminal are the jobs that were sent, failed or bounced.
	AlreadyTerminal int64
}

// Total is the number of items the position accounts for: cancelled, in flight and already final. After a cancel every item of the campaign is one of the three (a queued item has become
// cancelled), so it is the number of items the module holds under the campaign.
func (p CancelPosition) Total() int64 { return p.Cancelled + p.InFlight + p.AlreadyTerminal }

// Empty is true for {0, 0, 0}: the campaign has nothing to cancel, or the module does not know it.
func (p CancelPosition) Empty() bool {
	// MODULE-FORCED(SA-F13): a cancel of a campaign that is unknown, a typo or another tenant's answers 200 with all zeros (never a 404), so the adapter cannot tell "nothing to cancel" from "no such campaign" and exposes only Empty, never a claim that the campaign existed.
	return p.Cancelled == 0 && p.InFlight == 0 && p.AlreadyTerminal == 0
}
