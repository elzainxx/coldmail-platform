// Package m3 is the platform's adapter for module 3 (coldmail-send-byo, user-owned SMTP accounts): the shared sender body's routes (quote, run, report single and campaign, and since Phase 3
// close-out C1 cancel), module 3's feasibility quote, the 409 refusal of a run explained by the causes the module itself names, the account surface the user app needs, the suspension read, and
// the module's describe with its vocabularies compared with what the adapter handles. Phase 3, chunk B5, and the close-out chunk D5.
//
// It is thin on purpose. Everything module 3 shares with module 2 lives in internal/adapters/sendapi (parameterised by a Profile); this package is module 3's Profile, the routes only
// module 3 has (the accounts), and the construction from the platform's configuration. It holds no storage, no retry loop and no job logic: Phase 4 owns jobs.
//
// # CANCEL AND THE CAUSES OF A REFUSED RUN
//
// Cancel is the shared body's (sendapi.Client.Cancel, POST /v1/cancel, the same on module 2): queued items become the terminal state `cancelled` (free by charge row M3-01), claimed items finish and
// are reported once, a repeat changes nothing, another tenant's or an unknown campaign answers {0, 0, 0}. A run the module refuses with 409 names its causes in details[] (suspended,
// campaign_active, campaign_cap, queue_cap): the error is a *sendapi.ConflictError (sendapi.CausesOf, errors.Is on sendapi.ErrRunSuspended and the other three), so the platform does not
// predict the refusal from its own state and does not guess: the quote answers from the very function run reads (Feasibility, FeasibilityForCampaign), reasons with their cause and the cap that
// bound included. Only a module 3 older than the close-out answers a 409 with no cause (sendapi.ErrConflictUnexplained).
//
// # TENANCY
//
// Every call of a Client carries the user_id the Client was built for (the body for quote, run, create and bulk-add; the query for everything else), and every account the module answers
// with must be that tenant's (a response naming another tenant is a Contract error, never a value). The module answers a stranger's id with "nothing there" (404 for a job or an account,
// an empty 200 for a campaign report and for an account's events: research 3.4), so the all-zero system user, an empty id and anything that is not a lower-case uuid are refused before any
// network call (Adapter.For).
//
// # TWO CREDENTIALS, NEVER ONE
//
// The user-facing calls carry module 3's Domain A SERVICE token (PLATFORM_M3_SERVICE_TOKEN). The operator's ADMIN key (PLATFORM_M3_ADMIN_TOKEN) is a different credential for a different
// domain and is used only by internal/m3admin; FromConfig refuses a service token equal to the admin key, so the operator key can never ride on a user-facing call by a configuration slip.
//
// # THE PLAIN-TEXT PART
//
// Module 3 sends the rendered body as text/html, and as multipart/alternative (text first, HTML last) when the platform has written `public.job_items.rendered_text` (Phase 3 close-out C5; the
// module copies the column at run time, so there is no field on run). The adapter has nothing to do about it; describe advertises capabilities.plain_text_alternative.
package m3

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/m3admin"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

var (
	// ErrNotConfigured: PLATFORM_M3_URL or PLATFORM_M3_SERVICE_TOKEN is not set, so no user-facing call to module 3 can be made. The service still starts without them; the call refuses
	// with this error (which is also a modclient.Invalid error that names the variable: nothing was sent).
	ErrNotConfigured = errors.New("m3: module 3 is not configured for user-facing calls")
	// ErrSharedCredential: the service token equals the admin key. The two domains never share a credential.
	ErrSharedCredential = errors.New("m3: the Domain A service token equals the admin key")
	// ErrNoSuspensionReader: the adapter was built without a suspension reader (WithSuspensionReader), so it cannot say what state module 3 holds a user in, and it will not say anything else.
	ErrNoSuspensionReader = errors.New("m3: no suspension reader is wired, so the suspension state of a user cannot be read")
)

// Profile is module 3's profile of the shared sender body (research m3 sections 4, 5 and 11.2, checked against the module at the pinned commit: the job states are send_byo.job_state in enum
// order, the error classes send_byo.error_class, the error codes the nine of describeErrorCodes in status order).
//
// What differs from module 2, as data: no `reason` key (so no ItemReasons and no charge reasons), a single report that omits its optional keys until they are set (AlwaysPresent false),
// per-account detail on items and totals_by_account on the campaign report (AccountDetail), a feasibility quote with an inert price (QuoteFeasibility, not LinearCost), and a 409 on run
// that names the causes it has (ConflictCauses). Like module 2 it has a cancel route and the `cancelled` state (since Phase 3 close-out C1).
func Profile() sendapi.Profile {
	return sendapi.Profile{
		Module:         modclient.M3,
		DescribeModule: "send_byo",
		JobStates:      []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"},
		ErrorClasses:   []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"},
		ErrorCodes:     []string{"validation_failed", "unauthenticated", "not_found", "method_not_allowed", "conflict", "unprocessable", "rate_limited", "internal_error", "paused"},
		ItemReasons:    nil,
		AlwaysPresent:  false,
		AccountDetail:  true,
		// The price is inert (flat subscription, unit price 0): the quote's cost is not an amount the platform can check against recipient_count * unit_price.
		QuoteFeasibility: true,
		LinearCost:       false,
		Cancel:           true,
		RunConflict:      sendapi.ConflictCauses,
	}
}

// The per-route budgets the module's own bounds imply (research m3 9.1, re-read in the module's handlers). Every database step of the module runs in a 10 second window (dbCtx), so the module
// client's default budget serves quote, run, report, the lists, get and events. Create and the test button run the From-gate on the request's own context (from_dns_total_budget_ms, 15 seconds
// by default), then the write of its verdict and the worker verification (each in a 10 second window): about 25 to 35 seconds in the worst case, so they get 40. A bulk add is one synchronous
// request that does that per row (the gate, the create, the verification) and pauses between rows (bulk_add_verify_interval_ms, 2 seconds seeded and an operator dial): about 27 seconds a row
// in the worst case, so a row gets 30 and the request 10 more. A raised pacing dial can outrun this budget: the call then times out with an open outcome (MaybeApplied), never with a guess.
const (
	// SlowAccountTimeout is the budget of create and of the test button.
	SlowAccountTimeout = 40 * time.Second
	// BulkRowBudget is the time a bulk add may take per row (the module's pause of 2 seconds, a From-gate lookup and a verification), and BulkBaseBudget the fixed part.
	BulkRowBudget  = 30 * time.Second
	BulkBaseBudget = 10 * time.Second
	// MaxBulkRows is the largest batch the adapter sends: the module's seeded bulk_add_max_rows. A larger batch is refused here (the module refuses it whole with 422 anyway) and the caller
	// splits it; it also keeps BulkTimeout far below modclient.MaxRequestTimeout.
	MaxBulkRows = 500
)

// BulkTimeout is the budget the adapter gives a bulk add of rows rows.
func BulkTimeout(rows int) time.Duration { return BulkBaseBudget + time.Duration(rows)*BulkRowBudget }

// SuspensionReader is what the adapter needs of the module 3 ADMIN client to read a user's suspension state: exactly what internal/m3admin exports (*m3admin.Client satisfies it). It is an
// interface so a test can inject a fake and so the adapter never builds an admin client of its own (the admin key stays with the code that already owns it).
type SuspensionReader interface {
	Suspension(ctx context.Context, userID string) (m3admin.SuspensionState, error)
}

// Option changes how an Adapter is built.
type Option func(*settings)

type settings struct {
	reader SuspensionReader
}

// WithSuspensionReader wires the reader of the suspension state (normally the *m3admin.Client the reconcile loop already uses).
func WithSuspensionReader(r SuspensionReader) Option {
	return func(s *settings) { s.reader = r }
}

// Adapter is the tenant-free part of the adapter: the module client (one service token serves every tenant) and the operations that are not scoped to a tenant's own client. Build it with
// FromConfig and get a tenant's Client with For.
type Adapter struct {
	mc *modclient.Client
	st settings
}

func notConfigured(msg string) error {
	return fmt.Errorf("%w: %w", ErrNotConfigured, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: "m3.new", Message: msg})
}

// FromConfig builds the adapter from PLATFORM_M3_URL and PLATFORM_M3_SERVICE_TOKEN (the Domain A service token, not the admin key) and the shared module-call dials (timeout, retries,
// backoff: modclient.FromConfig). A configuration with either missing is ErrNotConfigured and a modclient.Invalid error that names the variable(s) that are missing; nothing is sent, and
// there is no silent no-op. A service token equal to PLATFORM_M3_ADMIN_TOKEN is ErrSharedCredential. The transport has no proxy and follows no redirect (modclient).
func FromConfig(cfg config.Config, opts ...Option) (*Adapter, error) {
	var missing []string
	if cfg.M3URL == "" {
		missing = append(missing, config.VarM3URL)
	}
	tok := cfg.M3ServiceToken.Reveal()
	if tok == "" {
		missing = append(missing, config.VarM3ServiceToken)
	}
	if len(missing) > 0 {
		return nil, notConfigured("not configured: " + strings.Join(missing, " and "))
	}
	if admin := cfg.M3AdminToken.Reveal(); admin != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(admin)) == 1 {
		return nil, fmt.Errorf("%w: %w", ErrSharedCredential, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: "m3.new",
			Message: config.VarM3ServiceToken + " must differ from " + config.VarM3AdminToken + " (the two domains never share a credential)"})
	}
	mc, err := modclient.New(modclient.M3, cfg.M3URL, cfg.M3ServiceToken, nil, modclient.FromConfig(cfg))
	if err != nil {
		return nil, err
	}
	return build(mc, opts), nil
}

// NewAdapter wraps a module client built for module 3 (the harness builds one with its own transport).
func NewAdapter(mc *modclient.Client, opts ...Option) (*Adapter, error) {
	if mc == nil || mc.Module() != modclient.M3 {
		return nil, &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: "m3.new", Message: "the module client is missing or was built for another module"}
	}
	return build(mc, opts), nil
}

func build(mc *modclient.Client, opts []Option) *Adapter {
	a := &Adapter{mc: mc}
	for _, o := range opts {
		if o != nil {
			o(&a.st)
		}
	}
	return a
}

// For builds the Client of one tenant (public.users.id). The all-zero system user, an empty id and anything that is not a lower-case uuid are refused before any network call
// (sendapi.New: tenant.ErrSystemUser, tenant.ErrBadID).
func (a *Adapter) For(userID string) (*Client, error) {
	sc, err := sendapi.New(a.mc, Profile(), userID, sendapi.Options{PageSize: sendapi.MaxPageSize})
	if err != nil {
		return nil, err
	}
	return &Client{Client: sc}, nil
}

// Client is the adapter for one tenant: the shared routes (through the embedded sendapi.Client: quote, run, report, Cancel), the feasibility quote and the account surface.
type Client struct {
	*sendapi.Client
}

// Describe reads module 3's self-description, checking that it is module 3's (a describe that names another module is a wiring mistake). ON ANY ERROR THE RESULT IS NIL.
func (a *Adapter) Describe(ctx context.Context) (*modclient.Describe, error) {
	return sendapi.Describe(ctx, a.mc, Profile())
}

// Vocabularies are the value sets module 3 advertises (job_state, error_class, ...). ON ANY ERROR THE RESULT IS NIL.
func (a *Adapter) Vocabularies(ctx context.Context) (modclient.Vocabularies, error) {
	return sendapi.Vocabularies(ctx, a.mc, Profile())
}

// CheckVocabularies compares what module 3 advertises with what this adapter handles (job_state and error_class, and the nine error codes). A vocabulary that is gone, an expected value
// that is missing, a state the adapter does not handle, or an error code that is missing is a Contract error naming the vocabulary; an additive difference is in the report. ON ANY ERROR
// THE REPORT IS THE ZERO VALUE.
func (a *Adapter) CheckVocabularies(ctx context.Context) (sendapi.VocabReport, error) {
	return sendapi.CheckVocabularies(ctx, a.mc, Profile())
}

// ---- the suspension read -------------------------------------------------------------------------------------------------

// Suspension is module 3's own answer about one user's suspension: observed, never inferred (CLAUDE.md rule 21). Version is module 3's counter of suspension transitions (0 for a user it
// has never suspended), the marker a convergence is compared at.
type Suspension struct {
	UserID       string
	Suspended    bool
	SuspendedAt  *time.Time
	ReinstatedAt *time.Time
	Version      int64
}

// Suspension reads the suspension state of userID from module 3 (through the wired SuspensionReader, normally m3admin's GET .../suspension) and returns exactly what the module said: a
// state is returned only for a 200 the reader accepted for THIS user, and an answer for another user, or a version that is negative, is a Contract error. It answers nothing it was not told:
// no reader wired is ErrNoSuspensionReader, an unknown user is NotFound (never "not suspended"), and anything that confirmed nothing (a network error, a timeout, a 5xx, an unreadable body)
// is a modclient error of the kind that says so. The all-zero system user, an empty id and anything that is not a lower-case uuid are refused before the reader is called. ON ANY ERROR THE
// RESULT IS THE ZERO VALUE.
func (a *Adapter) Suspension(ctx context.Context, userID string) (Suspension, error) {
	const route = "m3.suspension"
	invalid := func(msg string, is error) error {
		e := &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: route, Message: msg}
		if is != nil {
			return fmt.Errorf("%w: %w", is, e)
		}
		return e
	}
	switch {
	case userID == "":
		return Suspension{}, invalid("the user id is empty", tenant.ErrBadID)
	case userID == tenant.SystemUserID:
		return Suspension{}, invalid("the all-zero system user is never a tenant", tenant.ErrSystemUser)
	case !sendapi.CanonicalUUID(userID):
		return Suspension{}, invalid("the user id is not a lower-case uuid", tenant.ErrBadID)
	case a.st.reader == nil:
		return Suspension{}, invalid("no suspension reader is wired", ErrNoSuspensionReader)
	}
	st, err := a.st.reader.Suspension(ctx, userID)
	if err != nil {
		return Suspension{}, adminError(route, err)
	}
	contract := func(key, msg string) error {
		return &modclient.Error{Kind: modclient.Contract, Module: modclient.M3, Route: route, Key: key, Message: msg, Sent: true}
	}
	switch {
	case st.User != userID:
		return Suspension{}, contract("user_id", "the answer is for another user than the one asked")
	case st.Version < 0:
		return Suspension{}, contract("version", "the suspension counter is negative")
	}
	return Suspension{UserID: st.User, Suspended: st.Suspended, SuspendedAt: st.SuspendedAt, ReinstatedAt: st.ReinstatedAt, Version: st.Version}, nil
}

// adminError turns an m3admin error into the modclient error of the kind it means, keeping the original reachable with errors.Is (m3admin.ErrUnknownUser and the rest). The text of the
// original is never copied: an m3admin error is already free of module text, and this one carries only a sentence written here.
func adminError(route string, err error) error {
	e := &modclient.Error{Module: modclient.M3, Route: route, Cause: err}
	var re *m3admin.RetryableError
	switch {
	case errors.Is(err, context.Canceled):
		e.Kind, e.Message = modclient.Canceled, "the caller's context was canceled before the suspension read completed"
	case errors.Is(err, context.DeadlineExceeded):
		e.Kind, e.Message, e.Sent = modclient.Timeout, "the suspension read did not complete in time", true
	case errors.Is(err, m3admin.ErrUnknownUser):
		e.Kind, e.Status, e.Code, e.Message, e.Sent = modclient.NotFound, 404, "not_found", "module 3 does not know this user", true
	case errors.Is(err, m3admin.ErrUnauthorized):
		e.Kind, e.Status, e.Code, e.Message, e.Sent = modclient.Refused, 401, "unauthenticated", "module 3 refused the admin key", true
	case errors.Is(err, m3admin.ErrInvalid):
		e.Kind, e.Message = modclient.Invalid, "the suspension read was refused before or by module 3 as invalid"
	case errors.As(err, &re):
		e.Kind, e.Status, e.Message, e.Sent = modclient.Unavailable, re.HTTPStatus, "the suspension state could not be confirmed (nothing was read)", true
	default:
		e.Kind, e.Message, e.Sent = modclient.Unavailable, "the suspension read failed in a way the adapter does not recognise (nothing was read)", true
	}
	return e
}

// ---- small helpers shared by the files of the package ---------------------------------------------------------------------

// invalid is an error raised before anything was sent (a bad argument).
func invalid(route, msg string) error {
	return &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: route, Message: msg}
}

// contract is a Contract error about a response that was received and decoded but whose content the adapter refuses: the key and a sentence of ours, never a value from the response.
func contract(resp *modclient.Response, route, key, msg string) error {
	e := &modclient.Error{Kind: modclient.Contract, Module: modclient.M3, Route: route, Key: key, Message: msg, Sent: true}
	if resp != nil {
		e.Status, e.RequestID, e.Attempts = resp.Status, resp.RequestID, resp.Attempts
	}
	return e
}

// restamp names the module and the route on a Contract error that came from decoding a body held in memory (modclient.Decode names the route only for a response it received itself).
func restamp(err error, route string) error {
	var e *modclient.Error
	if !errors.As(err, &e) {
		return err
	}
	c := *e
	c.Module, c.Route = modclient.M3, route
	return &c
}

// CheckAccountVocabularies compares the closed sets the account side of the adapter maps from (an account's verification and alignment states, its pause and quarantine reasons, a quote's
// reasons and a bulk row's status) with what module 3's describe publishes under the same names, one VocabDiff per set. The adapter maps a word it does not know to an Unrecognised word, so a
// value the module adds does not break a listing; this is where it is SEEN: an added value is in the diff (Extra) and is not an error, while a set that is gone, or a value the adapter handles
// that the module no longer lists, is a Contract error naming "vocabularies.<name>". ON ANY ERROR THE RESULT IS NIL.
func (a *Adapter) CheckAccountVocabularies(ctx context.Context) ([]modclient.VocabDiff, error) {
	d, err := sendapi.Describe(ctx, a.mc, Profile())
	if err != nil {
		return nil, err
	}
	diffs := make([]modclient.VocabDiff, 0, len(describedSets))
	for _, name := range describedSets {
		df := d.Vocabularies.Diff(name, sets[name])
		if df.Breaking() {
			msg := "the module no longer advertises this vocabulary"
			if df.Advertised {
				msg = "the module no longer lists the value(s) the adapter handles: " + strings.Join(df.Missing, ", ")
			}
			return nil, &modclient.Error{Kind: modclient.Contract, Module: modclient.M3, Route: "m3.describe", Key: "vocabularies." + name, Message: msg, RequestID: d.RequestID, Sent: true}
		}
		diffs = append(diffs, df)
	}
	return diffs, nil
}
