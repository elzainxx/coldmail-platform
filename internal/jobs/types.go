package jobs

import (
	"context"
	"time"
)

// The module ids a job can belong to, and the two kinds of module.
const (
	ModuleVerify      = "m1" // verify: metered, no content
	ModuleSendManaged = "m2" // send-managed: metered, carries a campaign's content
	ModuleSendBYO     = "m3" // send-byo: entitlement-based (no credits), carries a campaign's content
)

// The statuses of a job (platform.job_transitions holds the legal moves between them; platform_0011).
const (
	StatusDraft      = "draft"
	StatusPreviewed  = "previewed"
	StatusConfirmed  = "confirmed"
	StatusScheduled  = "scheduled"
	StatusRunning    = "running"
	StatusPaused     = "paused"
	StatusCancelling = "cancelling"
	StatusCompleting = "completing"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusExpired    = "expired"
)

// The unknown-verdict choice of a chained send (Settings.UnknownChoice).
const (
	UnknownExclude = "exclude" // the default, the one that is safe for the customer
	UnknownInclude = "include"
)

// Job is a row of platform.jobs as the store returns it. Times are the database's; a time the job has not reached is nil.
type Job struct {
	ID     string
	UserID string
	Module string // m1, m2 or m3
	Status string

	SourceJobID string // the succeeded verify job a chained send takes its rows from; "" when none

	ScheduledStart *time.Time // set only by a confirm that names a start time
	StartDeadline  *time.Time // coalesce(scheduled_start, confirmed_at) + the max start delay; set at confirm
	PausedFrom     string     // the status a paused job paused from ("scheduled"); "" otherwise

	Preview            *Preview // the last preview (nil before the first); frozen once the job has left previewed
	PreviewFingerprint string
	ConfirmKey         string // the client's idempotency key of the confirm; "" before it

	PreviewedAt       *time.Time
	ConfirmedAt       *time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	CancelRequestedAt *time.Time

	Settings Settings
	Content  *Content // the campaign a send job carries (modules 2 and 3); nil for a verify job

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Settings are the job's frozen choices (platform.jobs.frozen_settings, key "settings").
type Settings struct {
	// UnknownChoice is what a chained send does with the source job's `unknown` verdicts: UnknownExclude (the default, written when the field is empty) or UnknownInclude. It is meaningful
	// only for a chained send (a request that sets "include" on a job with no source is refused).
	UnknownChoice string `json:"unknown_choice"`
}

// Content is the campaign a send job (modules 2 and 3) carries, frozen in platform.jobs.frozen_settings under the key "content". A verify job has none.
type Content struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to"`
}

// RowIn is one line of the list the user supplied: the address as typed and the row's merge data (the hook of phase 5; the store only keeps it, size-capped).
type RowIn struct {
	Email  string
	Fields map[string]string
}

// CreateReq is a new draft. For a chained send, Rows must be empty and SourceJobID names the user's succeeded verify job: the rows are that job's module results, selected by verdict.
type CreateReq struct {
	Module      string
	Rows        []RowIn
	Content     *Content // required for m2 and m3, absent for m1
	SourceJobID string
	Settings    Settings
}

// Preview is the snapshot a user confirms: what the list holds, what it will cost and the fingerprint of everything it depended on. It is stored in platform.jobs.preview.
type Preview struct {
	Rows             int            `json:"rows"`              // every row of the job
	Pending          int            `json:"pending"`           // rows that passed classification (before the user's suppression list)
	Malformed        int            `json:"malformed"`         // rows norm.Validate refused
	MalformedReasons map[string]int `json:"malformed_reasons"` // malformed rows by reason word (never an address)
	Duplicates       int            `json:"duplicates"`        // rows that repeat an earlier row's normalised address
	Excluded         int            `json:"excluded"`          // a chained send's rows whose source verdict was not selected
	Suppressed       int            `json:"suppressed"`        // pending rows on the user's suppression list: left out at confirm, counted here
	MissingFields    int            `json:"missing_fields"`    // 0 until phase 5 (merge fields)

	Units int `json:"units"` // rows that would reach the module: Pending - Suppressed

	// Chained sends only: the source job's module results by verdict (all four keys present) and how many the choice selects.
	Verdicts      map[string]int `json:"verdicts,omitempty"`
	Selected      int            `json:"selected"`
	UnknownChoice string         `json:"unknown_choice"`

	// Credits. Metered is false for module 3 (entitlement, no credits): the rest is then 0.
	Metered   bool   `json:"metered"`
	RateUnit  string `json:"rate_unit,omitempty"` // the dearest REACHABLE unit
	UnitCost  int    `json:"unit_cost"`           // its price: the reserve rate
	Credits   int64  `json:"credits"`             // Units x UnitCost: what a confirm would hold
	Available int64  `json:"available"`           // the user's available credits when the preview was made
	Shortfall int64  `json:"shortfall"`           // max(0, Credits - Available)

	Fingerprint string    `json:"fingerprint"`
	ComputedAt  time.Time `json:"computed_at"` // the store's clock (WithClock); informational, never part of the fingerprint
}

// ConfirmReq confirms a previewed job. Fingerprint is the fingerprint of the preview the user saw; Key is the client's idempotency key (the API's Idempotency-Key).
type ConfirmReq struct {
	JobID          string
	Fingerprint    string
	Key            string
	ScheduledStart *time.Time // nil: start as soon as the engine can; otherwise between one minute and 366 days ahead by the database's clock
}

// ConfirmResult is what a confirm did. Replayed is true when the key had already confirmed this job: the job is then returned as it is now and nothing was written.
type ConfirmResult struct {
	Job        Job
	Replayed   bool
	Reserved   int64 // credits held (0 for module 3)
	Pending    int   // rows that will reach the module
	Suppressed int   // rows left out because the user suppresses the address
}

// ListOpts pages the user's jobs, newest first. After is the cursor a previous page returned; Limit is 1..100 (0 means 25).
type ListOpts struct {
	After string
	Limit int
}

// Event is a row of platform.job_events.
type Event struct {
	ID            int64
	JobID         string
	Kind          string // created, transition, stale, flag or note
	FromStatus    string
	ToStatus      string
	ActorKind     string // user, admin or engine
	ActingUserID  string
	ActingAdminID string
	Reason        string
	Detail        []byte // a JSON object
	CreatedAt     time.Time
}

// PreChecks is what the store asks of the outside world before a module 3 confirm; the app implements it over the module 3 adapter (the store knows nothing of HTTP) and tests inject a fake.
// A Store with no PreChecks makes no module 3 pre-check.
type PreChecks interface {
	// M3Feasibility asks module 3 whether the campaign (the job's id) can run for recipients addresses. It is called BEFORE the confirm's transaction, never under a lock.
	M3Feasibility(ctx context.Context, userID string, campaignID string, recipients int) (Feasibility, error)
}

// Feasibility is module 3's answer to PreChecks.M3Feasibility. Unavailable means the module could not be asked (its answer is unknown: Feasible and Causes mean nothing then).
type Feasibility struct {
	Feasible    bool
	Causes      []string
	Unavailable bool
}
