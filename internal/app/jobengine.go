package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/entitlements"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// THE JOB ENGINE IN THE SERVICE (Phase 4 B7 to B9). What is wired here: the three module adapters, built from the configuration once (an adapter whose dials are unset is nil and its module's jobs
// WAIT: they are never failed for the platform's own configuration); the module 3 pre-check of a confirm (jobs.PreChecks over the module 3 adapter); the registry the engine talks to the modules
// through; and the engine itself, run under the SAME leader election as the entitlement loops and stopped BEFORE them (runLoops).

// moduleOneBatch is the most rows the engine offers module 1 in one call: it takes a job's whole list in ONE request (the idempotency key is the job id), and the store hands the engine at most this
// many pending rows at a time (jobs.MaxEngineIDs, the longest list a job may hold: module 1's own limit for one job). It is that constant, never a number of its own: PLATFORM_JOB_MAX_ROWS cannot be set
// above it (a test loads the dial at this number and one more), so no job the service accepts is larger than one call, and none of them can wait for want of one (m1Module.Submit would wait, free, until
// the job's start deadline).
const moduleOneBatch = jobs.MaxEngineIDs

// moduleAdapters are the adapters built from the configuration; a nil one is a module whose dials are unset.
type moduleAdapters struct {
	M1 *m1.Factory
	M2 *m2.Adapter
	M3 *m3.Adapter
}

// newModuleAdapters builds the three adapters from cfg: module 1's factory from PLATFORM_M1_URL (and the dials of every module call), module 2's from PLATFORM_M2_URL and
// PLATFORM_M2_SERVICE_TOKEN, module 3's from PLATFORM_M3_URL and PLATFORM_M3_SERVICE_TOKEN. A module whose dials are unset is nil and is logged once; a configuration that is WRONG (module 3's
// service token equal to its operator key, a URL the client refuses) stops the process like any other invalid dial.
func newModuleAdapters(cfg config.Config, log *slog.Logger) (moduleAdapters, error) {
	var a moduleAdapters
	var err error
	if cfg.M1URL != "" {
		if a.M1, err = m1.FromConfig(cfg); err != nil {
			return moduleAdapters{}, &config.Error{Problems: []config.Problem{{Name: config.VarM1URL, Reason: "is not usable by the module 1 client: " + modclientWord(err)}}}
		}
	}
	switch a.M2, err = m2.FromConfig(cfg); {
	case errors.Is(err, m2.ErrNotConfigured):
		a.M2, err = nil, nil
	case err != nil:
		return moduleAdapters{}, &config.Error{Problems: []config.Problem{{Name: config.VarM2URL, Reason: "is not usable by the module 2 client: " + modclientWord(err)}}}
	}
	switch a.M3, err = m3.FromConfig(cfg); {
	case errors.Is(err, m3.ErrNotConfigured):
		a.M3, err = nil, nil
	case errors.Is(err, m3.ErrSharedCredential):
		return moduleAdapters{}, &config.Error{Problems: []config.Problem{{Name: config.VarM3ServiceToken, Reason: "must differ from " + config.VarM3AdminToken + " (the two domains never share a credential)"}}}
	case err != nil:
		return moduleAdapters{}, &config.Error{Problems: []config.Problem{{Name: config.VarM3URL, Reason: "is not usable by the module 3 client: " + modclientWord(err)}}}
	}
	log.Info("module adapters for the job engine", "module1", a.M1 != nil, "module2", a.M2 != nil, "module3", a.M3 != nil,
		"effect", "a job of a module that is not configured waits and goes stale; it is never failed for it")
	return a, nil
}

// modclientWord is why a client was refused, in the words of the typed error (they never carry a URL or a key).
func modclientWord(err error) string {
	var e *modclient.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "invalid"
}

// ---- the module 3 pre-check of a confirm --------------------------------------------------------------------------------------------------------------------------------------------

// m3PreChecks is jobs.PreChecks over the module 3 adapter: before a module 3 job is confirmed the store asks module 3, with the module's own quote (which reads the very function run reads),
// whether the campaign can run, and the module names the causes when it cannot.
type m3PreChecks struct{ ad *m3.Adapter }

var _ jobs.PreChecks = m3PreChecks{}

// M3Feasibility asks module 3 whether recipients more recipients can be accepted into the campaign (the job's id) for the user. Causes are the causes of a run refusal the module predicts
// (suspended, campaign_active, campaign_cap, queue_cap), in its order. A module that cannot be asked now (unreachable, timed out, rate limited, paused by its operator) is Unavailable, which the store
// turns into `module_unavailable` for an immediate confirm and ignores for a scheduled one; any other failure is an error (the store refuses the confirm with it and holds no credits).
func (p m3PreChecks) M3Feasibility(ctx context.Context, userID, campaignID string, recipients int) (jobs.Feasibility, error) {
	c, err := p.ad.For(userID)
	if err != nil {
		return jobs.Feasibility{}, err
	}
	f, err := c.FeasibilityForCampaign(ctx, campaignID, int64(recipients))
	if err != nil {
		if modclient.Retryable(err) || modclient.KindOf(err) == modclient.Paused {
			return jobs.Feasibility{Unavailable: true}, nil
		}
		return jobs.Feasibility{}, err
	}
	causes := make([]string, 0, len(f.Causes()))
	for _, c := range f.Causes() {
		causes = append(causes, string(c))
	}
	return jobs.Feasibility{Feasible: f.Feasible, Causes: causes}, nil
}

// preChecksOf is the store's module 3 pre-check: m3PreChecks when module 3 is configured, and a nil interface (never a typed nil: the store tests it against nil) when it is not.
func preChecksOf(a moduleAdapters) jobs.PreChecks {
	if a.M3 == nil {
		return nil
	}
	return m3PreChecks{ad: a.M3}
}

// ---- the registry and the engine ---------------------------------------------------------------------------------------------------------------------------------------------------------

// newModuleRegistry builds the registry the engine reaches the modules through: the adapters, the tenant resolver (module 1's per-user key), the privileged-function ensurer of module 1's caller
// rows and PLATFORM_JOB_SUBMIT_CONCURRENCY. (Module 1's job reference is not read here: it arrives on the job the store hands the engine, jobs.EngineJob.ModuleRef.)
func newModuleRegistry(pool *pgxpool.Pool, cfg config.Config, a moduleAdapters, res *tenant.Resolver, log *slog.Logger) *jobengine.Registry {
	return jobengine.NewRegistry(jobengine.RegistryDeps{
		Resolver:          res,
		M1:                a.M1,
		M2:                a.M2,
		M3:                a.M3,
		Provisioner:       tenant.NewM1CallerEnsurer(res, pool),
		SubmitConcurrency: cfg.JobSubmitConcurrency,
		Log:               log,
	})
}

// engineConfig is the consumer of the job engine's dials: every PLATFORM_JOB_* dial but the switch (newJobEngine reads that one) and the concurrency (newModuleRegistry reads that one).
func engineConfig(cfg config.Config) jobengine.Config {
	return jobengine.Config{
		Tick:        cfg.JobTick,
		SettleEvery: cfg.JobSettleInterval,
		StaleAfter: map[string]time.Duration{
			jobs.ModuleVerify:      cfg.JobStaleAfterM1,
			jobs.ModuleSendManaged: cfg.JobStaleAfterM2,
			jobs.ModuleSendBYO:     cfg.JobStaleAfterM3,
		},
		SubmitBatch:    cfg.JobSubmitBatch,
		SubmitBatchFor: map[string]int{jobs.ModuleVerify: moduleOneBatch},
		StepTimeout:    cfg.JobStepTimeout,
	}
}

// newJobEngine builds the engine, or says why there is none: PLATFORM_JOB_ENGINE_ENABLED=false (logged once, at start-up), or a service built with no leader election (tests only: an engine
// with no gate would run its loops in every instance). It returns nil with no error in both cases.
func newJobEngine(cfg config.Config, deps apiDeps, leader *entitlements.Leader, log *slog.Logger) (*jobengine.Engine, error) {
	if !cfg.JobEngineEnabled {
		log.Info("job engine is switched off", "set", config.VarJobEngineEnabled, "effect", "no job is started, observed or completed by this instance: jobs stay where they are")
		return nil, nil
	}
	if leader == nil {
		log.Warn("job engine is not started: this service has no leader election, and an engine with no gate would run in every instance")
		return nil, nil
	}
	if deps.Jobs == nil || deps.Registry == nil {
		return nil, errors.New("the job store or the module registry was not built")
	}
	eng, err := jobengine.New(jobengine.Deps{Store: deps.Jobs, Modules: deps.Registry, Leader: leader, Log: log}, engineConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("build the job engine: %w", err)
	}
	return eng, nil
}
