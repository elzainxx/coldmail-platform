// Package api is the platform's job HTTP API (Phase 4, chunk B14b): the routes a signed-in user creates, previews, confirms, watches and downloads a job through. P6 builds only UI on it.
//
// WHO THE CALLER IS. Every route sits behind one middleware (auth.go): the request must carry exactly one `Authorization: Bearer <token>` header, the token is verified by internal/auth
// (signature, exp/nbf, iss, aud) and its subject is resolved through tenant.Resolver.ByAuthUser (an unknown, inactive or system login is refused). EVERY failure there is the same 401 (except the one 503 auth_unavailable: the platform could not obtain the keys to judge the token, internal/auth ErrKeysUnavailable)
// `unauthenticated`, with the same body and `WWW-Authenticate: Bearer`; the reason goes to the log under the request id and is never the token, a part of it, or anything in the response.
// The user the request acts for is `public.users.id` of that principal, put in the request context, and it is the ONLY source of the user id of every store call: no path, query, header or
// body names a user (a body or query that tries is refused as an unknown field, and a header is never read).
//
// WHAT IT CALLS. Only the Service interface below, which *jobs.Store satisfies: every rule of a job (the lifecycle, the slot, the reserve, the idempotency of a confirm, the ownership of a job)
// is the store's and the database's, never this package's; the API translates a request into a store call and the store's answer into JSON. A job of another user is the store's
// ErrNotFound, answered exactly as a job that does not exist.
//
// ERRORS. The platform envelope `{"error":{"code","message","request_id"}}` (httpapi.WriteError), plus a `details` object ONLY where a code has numbers or a word to give. Every error the store
// can return is mapped in ONE function (errors.go: classify) that a table test covers sentinel by sentinel; anything it does not know is a 500 `internal_error` whose text is logged under the
// request id and never sent.
//
// SETTINGS (Phase 5 S6). GET and PUT /v1/settings (settings.go) are the user's own settings, through a second small interface (SettingsService); the user is the same principal and nothing else.
//
// STREAMING. results.csv writes through internal/csvsafe a page of results at a time and flushes after each; once the first byte is out a failure can only abort the connection (a status line
// cannot be taken back), which the client sees as a truncated body, never as a complete-looking file.
//
// NOT HERE: CORS (the UI is served from the API's own origin or through a proxy that adds what it needs), rate limiting, and any admin route (the admin console is a parallel writer through the
// same store, with its own authentication).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/tenant"
	"github.com/coldmail-platform/platform/internal/usersettings"
)

// Service is what the API needs of the job store. *jobs.Store satisfies it; the unit tests use a fake. Every method takes the acting user from the authenticated principal.
type Service interface {
	CreateDraft(ctx context.Context, userID string, req jobs.CreateReq) (jobs.Job, error)
	Get(ctx context.Context, userID, jobID string) (jobs.Job, error)
	List(ctx context.Context, userID string, opts jobs.ListOpts) ([]jobs.Job, string, error)
	Preview(ctx context.Context, userID, jobID string, opts ...jobs.PreviewOption) (jobs.Preview, error)
	Confirm(ctx context.Context, userID string, req jobs.ConfirmReq) (jobs.ConfirmResult, error)
	DiscardDraft(ctx context.Context, userID, jobID string) (jobs.Job, error)
	Reopen(ctx context.Context, userID, jobID string) (jobs.Job, error)
	Cancel(ctx context.Context, userID, jobID string) (jobs.Job, error)
	Pause(ctx context.Context, userID, jobID string) (jobs.Job, error)
	Resume(ctx context.Context, userID, jobID string) (jobs.Job, error)
	Events(ctx context.Context, userID, jobID string, afterID int64, limit int) ([]jobs.Event, error)
	Results(ctx context.Context, userID, jobID string, afterOrdinal, limit int) ([]jobs.Result, error)
	EachResult(ctx context.Context, userID, jobID string, fn func(jobs.Result) error) error
	// MaxRows is the longest list a job holds (the store's limit): a CSV body is cut off at it while it is read.
	MaxRows() int
}

var _ Service = (*jobs.Store)(nil)

// Verifier is what the authentication middleware asks of internal/auth. *auth.Verifier satisfies it.
type Verifier interface {
	Verify(ctx context.Context, bearer string) (auth.Claims, error)
}

// Resolver is what it asks of internal/tenant. *tenant.Resolver satisfies it.
type Resolver interface {
	ByAuthUser(ctx context.Context, authUserID string) (tenant.Tenant, error)
}

var (
	_ Verifier = (*auth.Verifier)(nil)
	_ Resolver = (*tenant.Resolver)(nil)
)

// DefaultResultsPageMax is the largest page of results the JSON route serves unless the dial says otherwise (PLATFORM_RESULTS_PAGE_MAX): the same size the CSV stream reads at a time.
const DefaultResultsPageMax = jobs.ResultsPageSize

// Deps are what the API is built from. Service, Verifier and Resolver are required.
type Deps struct {
	Service  Service
	Verifier Verifier
	Resolver Resolver
	Logger   *slog.Logger
	// ResultsPageMax is the largest `limit` of GET /v1/jobs/{id}/results (1 to jobs.ResultsPageSize); 0 means DefaultResultsPageMax. It is the dial PLATFORM_RESULTS_PAGE_MAX.
	ResultsPageMax int
	// Settings is the user settings store (Phase 5 S6: GET and PUT /v1/settings, settings.go). nil means the two settings routes are not served (a request for one is the 404 of any unknown path) and the
	// job routes are unaffected: the production wiring (internal/app) always gives it.
	Settings SettingsService
}

// API serves the job routes. It holds nothing that changes after New, so it is safe for concurrent use.
type API struct {
	svc      Service
	verifier Verifier
	resolver Resolver
	log      *slog.Logger
	pageMax  int
	settings SettingsService // nil: the settings routes are not mounted
}

// New builds the API. A missing dependency or a ResultsPageMax out of range is a wiring bug and an error at start-up, never a route that fails later.
func New(d Deps) (*API, error) {
	switch {
	case isNil(d.Service):
		return nil, errors.New("api.New: no job service")
	case isNil(d.Verifier):
		return nil, errors.New("api.New: no token verifier")
	case isNil(d.Resolver):
		return nil, errors.New("api.New: no tenant resolver")
	}
	pageMax := d.ResultsPageMax
	if pageMax == 0 {
		pageMax = DefaultResultsPageMax
	}
	if pageMax < 1 || pageMax > jobs.ResultsPageSize {
		return nil, fmt.Errorf("api.New: the results page maximum must be between 1 and %d, got %d", jobs.ResultsPageSize, pageMax)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var settings SettingsService // optional: nil (or a nil *usersettings.Store) means the settings routes are not mounted
	if !isNil(d.Settings) {
		settings = d.Settings
	}
	return &API{svc: d.Service, verifier: d.Verifier, resolver: d.Resolver, log: logging.Contextual(log), pageMax: pageMax, settings: settings}, nil
}

// isNil is true for a nil interface and for an interface holding a nil pointer (a wiring bug that would otherwise panic at the first request).
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch p := v.(type) {
	case *jobs.Store:
		return p == nil
	case *auth.Verifier:
		return p == nil
	case *tenant.Resolver:
		return p == nil
	case *usersettings.Store:
		return p == nil
	}
	return false
}

// route is one row of the route table: the table is what Mount registers, what Routes returns and what the documentation is checked against.
type route struct {
	method  string
	pattern string // the path, with {id} where a job id goes
	handle  func(a *API, w http.ResponseWriter, r *http.Request, p principal)
}

// settingsPath is the one path of the settings store (settings.go): its routes are mounted only when the API was given a store (Deps.Settings), so a service built without it answers the path with the
// 404 of any unknown path and every other route is unaffected.
const settingsPath = "/v1/settings"

// routes is the whole surface of the API.
var routes = []route{
	{http.MethodPost, "/v1/jobs", (*API).create},
	{http.MethodGet, "/v1/jobs", (*API).list},
	{http.MethodGet, "/v1/jobs/{id}", (*API).get},
	{http.MethodPost, "/v1/jobs/{id}/preview", (*API).preview},
	{http.MethodPost, "/v1/jobs/{id}/confirm", (*API).confirm},
	{http.MethodPost, "/v1/jobs/{id}/discard", (*API).discard},
	{http.MethodPost, "/v1/jobs/{id}/reopen", (*API).reopen},
	{http.MethodPost, "/v1/jobs/{id}/cancel", (*API).cancel},
	{http.MethodPost, "/v1/jobs/{id}/pause", (*API).pause},
	{http.MethodPost, "/v1/jobs/{id}/resume", (*API).resume},
	{http.MethodGet, "/v1/jobs/{id}/events", (*API).events},
	{http.MethodGet, "/v1/jobs/{id}/results", (*API).results},
	{http.MethodGet, "/v1/jobs/{id}/results.csv", (*API).resultsCSV},
	{http.MethodGet, settingsPath, (*API).getSettings},
	{http.MethodPut, settingsPath, (*API).putSettings},
}

// Routes lists the routes as "METHOD /path", in the order they are registered.
func Routes() []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.method + " " + r.pattern
	}
	return out
}

// Mount registers every route on the mux, each behind the authentication middleware. It is httpapi.Deps.Mount.
func (a *API) Mount(mux *http.ServeMux) {
	for _, r := range routes {
		if r.pattern == settingsPath && a.settings == nil {
			continue // no settings store was given: these two routes are not here
		}
		h := r.handle
		mux.Handle(r.method+" "+r.pattern, a.authenticated(func(w http.ResponseWriter, req *http.Request, p principal) { h(a, w, req, p) }))
	}
}
