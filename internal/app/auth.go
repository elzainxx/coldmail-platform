package app

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/api"
	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// apiDeps is what the job API (Phase 4, chunk B14b) and, later, the job engine are built from. It is built in two steps: the verifier at start-up, before anything is opened (newAPIDeps),
// and the stores once the pool exists (build).
type apiDeps struct {
	// Auth verifies the bearer token of a request. nil means neither PLATFORM_JWT_SECRET nor PLATFORM_JWKS_URL is set: the job API is not served.
	Auth *auth.Verifier

	// Money is THE money store of the process: the reachability loop, the job store and (later) the engine all share this one, so that they wait for a lock as long as one dial says
	// (PLATFORM_MONEY_LOCK_TIMEOUT_MS) and take the (user, module) advisory lock in one way.
	Money *money.Store
	// Jobs is the job store, built with the two limits (PLATFORM_JOB_MAX_ROWS, PLATFORM_JOB_MAX_START_DELAY_MIN) and, when module 3 is configured, with its pre-check (m3PreChecks over the module 3
	// adapter). The API and the engine use this one store.
	Jobs *jobs.Store
	// PreChecks is the module 3 pre-check the job store was given: nil when module 3 is not configured (the store then makes none).
	PreChecks jobs.PreChecks
	// Adapters are the three module adapters, built once from the configuration; a nil one is a module whose dials are unset (its jobs wait).
	Adapters moduleAdapters
	// Registry is what the job engine reaches the modules through (jobengine.Registry over Adapters).
	Registry *jobengine.Registry
	// Tenants resolves a login to a platform user (the API's authentication) and, later, builds each module's scope for the engine.
	Tenants *tenant.Resolver
	// API is the job API's handler set; nil unless Auth is set (a service with no key source does not serve it).
	API *api.API
}

// newAPIDeps builds the verifier from the configuration at start-up, so that an invalid combination stops the process before anything is served (exit status 2, naming the variable, never a
// value), and says in ONE log line which key sources are on. The line carries booleans, the audience and the leeway: never a secret, a key or the URL.
func newAPIDeps(cfg config.Config, log *slog.Logger) (apiDeps, error) {
	v, err := auth.FromConfig(cfg)
	if err != nil {
		return apiDeps{}, err
	}
	if v == nil {
		log.Info("job API authentication is not configured", "set", config.VarJWTSecret+" or "+config.VarJWKSURL, "effect", "the job API is not served")
		return apiDeps{}, nil
	}
	s := v.Summary()
	log.Info("job API authentication configured",
		"hs256", s.HMAC, "jwks", s.JWKS, "issuer_pinned", s.IssuerPinned, "audience", s.Audience, "leeway", s.Leeway.String())
	return apiDeps{Auth: v}, nil
}

// newMoneyStore builds THE money store over the pool, waiting for a lock as long as PLATFORM_MONEY_LOCK_TIMEOUT_MS says. It is the one place a money store is made.
func newMoneyStore(pool *pgxpool.Pool, cfg config.Config) *money.Store {
	return money.New(pool, money.WithLockTimeout(cfg.MoneyLockTimeout))
}

// newJobStore builds the job store over the pool and the money store with the dials that bound a job (the longest list and how long a start may wait) and any further options (build passes the
// module 3 pre-check). It logs through the service's logger (the store logs no address, no secret and no SQL).
func newJobStore(pool *pgxpool.Pool, mny *money.Store, cfg config.Config, log *slog.Logger, more ...jobs.Option) (*jobs.Store, error) {
	opts := append([]jobs.Option{jobs.WithMaxRows(cfg.JobMaxRows), jobs.WithMaxStartDelay(cfg.JobMaxStartDelay), jobs.WithLogger(log)}, more...)
	return jobs.New(pool, mny, opts...)
}

// apiResolver is what the API authenticates logins through: the tenant resolver. It is a variable only so that a test of the whole service, which has no database to resolve a login in,
// can put a fake in its place; nothing else assigns it.
var apiResolver = func(t *tenant.Resolver) api.Resolver { return t }

// build makes what needs the pool: the money store, the module adapters, the job store (with module 3's pre-check when module 3 is configured), the tenant resolver, the module registry and, when
// the verifier exists, the job API. It runs once, after the pool is open and before the server listens. With no key source the API is not built and its routes are not mounted: a request for one is the 404 of any unknown path.
func (d *apiDeps) build(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) error {
	d.Money = newMoneyStore(pool, cfg)
	var err error
	if d.Adapters, err = newModuleAdapters(cfg, log); err != nil {
		return err
	}
	var more []jobs.Option
	if d.PreChecks = preChecksOf(d.Adapters); d.PreChecks != nil {
		more = append(more, jobs.WithPreChecks(d.PreChecks))
	}
	if d.Jobs, err = newJobStore(pool, d.Money, cfg, log, more...); err != nil {
		return fmt.Errorf("build the job store: %w", err)
	}
	if d.Tenants, err = tenant.NewResolver(pool, cfg.M1KeySecret, tenant.WithM1KeyVersion(cfg.M1KeyVersion)); err != nil {
		return fmt.Errorf("build the tenant resolver: %w", err)
	}
	d.Registry = newModuleRegistry(pool, cfg, d.Adapters, d.Tenants, log)
	if d.Auth == nil {
		return nil
	}
	if d.API, err = api.New(api.Deps{Service: d.Jobs, Verifier: d.Auth, Resolver: apiResolver(d.Tenants), Logger: log, ResultsPageMax: cfg.ResultsPageMax}); err != nil {
		return fmt.Errorf("build the job API: %w", err)
	}
	log.Info("job API mounted", "routes", len(api.Routes()), "max_rows", d.Jobs.MaxRows(), "max_start_delay", d.Jobs.MaxStartDelay().String(), "results_page_max", cfg.ResultsPageMax)
	return nil
}

// mount is httpapi.Deps.Mount: the API's routes, or nil when there is no API to mount.
func (d apiDeps) mount() func(*http.ServeMux) {
	if d.API == nil {
		return nil
	}
	return d.API.Mount
}
