package jobengine

// THE REAL MODULES. The engine knows a module only through the Module interface (module.go); this file and its two siblings (module_m1.go, module_sendapi.go) are the production implementations
// over the adapters (internal/adapters/m1, m2, m3 and sendapi). Nothing here decides money or lifecycle: a Module hands the engine what the module said and the engine hands it to the store.
//
// THE RULES THAT BIND EVERY METHOD OF EVERY MODULE HERE.
//   - A MODULE THAT CANNOT ANSWER NEVER FAILS A CUSTOMER'S JOB. Unavailable, Timeout, Paused, RateLimited, Canceled, and also Contract and Internal (a module that answers something this platform
//     does not understand), come out as ErrWait: the job stays where it is, nothing is invented, the next tick asks again, and a job that makes no progress raises the stale event. The same holds
//     for the platform's own configuration: an adapter that is not configured, a key the module refuses (401 or 403), a user whose module 1 key cannot be built, a provisioning that did not happen.
//   - ONLY A REFUSAL FOR GOOD IS ErrRefused, and only at Submit: the module said no to the job itself (validation, not found) and nothing of it is held at the module. (See mapError for the whole table.)
//   - NEVER AN INVENTED OUTCOME. Observe hands the engine the TERMINAL items the module reported and nothing else, the outcome exactly as the module's own words say it; the charge table decides
//     what it costs, inside the store's transaction. A listing that contradicts the module's own totals is never believed as complete.
//   - NO ADDRESS AND NO SECRET IN AN ERROR. Every error text built here is made of words (the module's code, an HTTP status, a cause) and is passed through scrubText besides.
//
// ONE MODULE, ONE USER. A Module is built per call of Registry.For for the job's owner: module 1 needs the user's own derived caller key, modules 2 and 3 carry the user id on every request.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The bounds of the submit concurrency (PLATFORM_JOB_SUBMIT_CONCURRENCY is validated by internal/config; these keep a Registry built by hand sane).
const (
	DefaultSubmitConcurrency = 8
	MaxSubmitConcurrency     = 64
)

// M1Provisioner makes sure module 1 has the caller row that lets the user's derived key authenticate as that user. It is idempotent. Another part of the platform implements it (the privileged
// function of Phase 4 chunk B16); nil means every user is already provisioned.
type M1Provisioner interface {
	EnsureCaller(ctx context.Context, userID string) error
}

// Scoper is what the registry needs of tenant.Resolver: the module scope of a tenant. *tenant.Resolver satisfies it.
type Scoper interface {
	Scope(ctx context.Context, t tenant.Tenant) (tenant.Scope, error)
}

// RegistryDeps is what a Registry is built from. Every adapter may be nil: the module is then served by a Module whose every call is ErrWait with the reason, so a platform that is not (yet)
// configured for a module holds that module's jobs where they are instead of failing them.
type RegistryDeps struct {
	Resolver *tenant.Resolver // module 1's scope (the user's derived caller key); nil: no module 1 job can be served
	Scoper   Scoper           // replaces Resolver when set (a test double)
	M1       *m1.Factory
	M2       *m2.Adapter
	M3       *m3.Adapter
	// Provisioner makes module 1's caller row for a user before the first call made as that user. nil: already provisioned.
	Provisioner M1Provisioner
	// SubmitConcurrency is how many run calls one module 2 or 3 batch has in flight at once (PLATFORM_JOB_SUBMIT_CONCURRENCY). 0 means DefaultSubmitConcurrency; more than MaxSubmitConcurrency is
	// cut to it.
	SubmitConcurrency int
	Log               *slog.Logger // nil: nothing is logged
}

// Registry is the production Modules: it gives the engine the Module of a job's module id for the job's user.
type Registry struct {
	scoper Scoper
	m1     *m1.Factory
	m2     *m2.Adapter
	m3     *m3.Adapter
	prov   M1Provisioner
	conc   int
	log    *slog.Logger

	provisioned sync.Map     // user id -> true: users whose module 1 caller row was ensured by this process (it is idempotent: this only spares the call)
	unexpected  atomic.Int64 // module answers this platform did not understand (Contract, Internal, an error of no known kind)
}

// NewRegistry builds the Registry.
func NewRegistry(d RegistryDeps) *Registry {
	r := &Registry{m1: d.M1, m2: d.M2, m3: d.M3, prov: d.Provisioner, conc: d.SubmitConcurrency, log: d.Log}
	switch {
	case d.Scoper != nil:
		r.scoper = d.Scoper
	case d.Resolver != nil:
		r.scoper = d.Resolver
	}
	switch {
	case r.conc <= 0:
		r.conc = DefaultSubmitConcurrency
	case r.conc > MaxSubmitConcurrency:
		r.conc = MaxSubmitConcurrency
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return r
}

var _ Modules = (*Registry)(nil)

// UnexpectedAnswers is how many module answers this registry has met that the platform does not understand (a contract violation, a module fault, an error of no known kind): each one made a
// step wait. It is what "counted" means in the logs of those waits.
func (r *Registry) UnexpectedAnswers() int64 { return r.unexpected.Load() }

// For returns the Module of module moduleID for the job's owner userID.
//   - An adapter that is not configured, a module 1 scope that cannot be built or a module 1 caller that cannot be provisioned is not an error of the job: For answers a Module whose every call is
//     ErrWait (or itself the ErrWait) with the reason, which names the dial, never its value.
//   - A module id that is none of m1, m2, m3 is an ordinary error (the jobs table cannot hold one).
func (r *Registry) For(ctx context.Context, userID, moduleID string) (Module, error) {
	switch moduleID {
	case jobs.ModuleVerify:
		return r.forM1(ctx, userID)
	case jobs.ModuleSendManaged:
		if r.m2 == nil {
			return waiting("module 2 is not configured (" + config.VarM2URL + " and " + config.VarM2ServiceToken + ")"), nil
		}
		c, err := r.m2.For(userID)
		if err != nil {
			return nil, Wait("module 2's client for the job's owner cannot be built: " + scrubText(err.Error()))
		}
		return &sendapiModule{reg: r, module: jobs.ModuleSendManaged, c: c.Client, conc: r.conc}, nil
	case jobs.ModuleSendBYO:
		if r.m3 == nil {
			return waiting("module 3 is not configured (" + config.VarM3URL + " and " + config.VarM3ServiceToken + ")"), nil
		}
		c, err := r.m3.For(userID)
		if err != nil {
			return nil, Wait("module 3's client for the job's owner cannot be built: " + scrubText(err.Error()))
		}
		return &sendapiModule{reg: r, module: jobs.ModuleSendBYO, c: c.Client, conc: r.conc}, nil
	}
	return nil, fmt.Errorf("jobengine: the registry has no module %q", scrubText(moduleID))
}

// forM1 builds module 1's Module for a user: the user's own derived key (a tenant scope read from the database at every call, so a user switched off since the job started is not served),
// their caller row ensured once per process, and the adapter over the shared transport.
func (r *Registry) forM1(ctx context.Context, userID string) (Module, error) {
	if r.m1 == nil {
		return waiting("module 1 is not configured (" + config.VarM1URL + ")"), nil
	}
	if r.scoper == nil {
		return waiting("no tenant resolver is configured, so module 1's caller key of the job's owner cannot be derived"), nil
	}
	scope, err := r.scoper.Scope(ctx, tenant.Tenant{UserID: userID, AuthUserID: userID, Active: true}) // a real platform user IS its login (public.users.id = auth_user_id)
	if err != nil {
		return nil, Wait("module 1's scope of the job's owner cannot be built: " + tenantWord(err))
	}
	if r.prov != nil {
		if _, done := r.provisioned.Load(userID); !done {
			if err := r.prov.EnsureCaller(ctx, userID); err != nil {
				return nil, Wait("module 1's caller key of the job's owner cannot be provisioned: " + scrubText(err.Error()))
			}
			r.provisioned.Store(userID, true)
		}
	}
	ad, err := r.m1.ForScope(scope)
	if err != nil {
		return nil, Wait("module 1's adapter for the job's owner cannot be built: " + scrubText(err.Error()))
	}
	ad.PageLimit = m1.MaxPageLimit
	return &m1Module{reg: r, ad: ad}, nil
}

// tenantWord names why a scope could not be built, in words (no id, no key).
func tenantWord(err error) string {
	switch {
	case errors.Is(err, tenant.ErrNoM1KeySecret):
		return config.VarM1KeySecret + " is not set"
	case errors.Is(err, tenant.ErrWeakSecret):
		return config.VarM1KeySecret + " is too short"
	case errors.Is(err, tenant.ErrInactive):
		return "the user is inactive"
	case errors.Is(err, tenant.ErrUnknownLogin):
		return "no platform user has this login"
	case errors.Is(err, tenant.ErrSystemUser):
		return "the system user is never a tenant"
	case errors.Is(err, tenant.ErrBadID), errors.Is(err, tenant.ErrTenantMismatch):
		return "the job's owner is not a valid platform user"
	}
	return "the database could not be read"
}

// ---- a module that is not there -------------------------------------------------------------------------------------------------------------------------------------------------------

// waitingModule is the Module of a module that cannot be served at all: every call is ErrWait with the reason. The job waits (and goes stale), it is never failed for the platform's own configuration.
type waitingModule struct{ reason string }

func waiting(reason string) Module { return waitingModule{reason: reason} }

func (w waitingModule) Submit(context.Context, jobs.EngineJob, []jobs.EngineRow) (SubmitResult, error) {
	return SubmitResult{}, Wait(w.reason)
}

func (w waitingModule) Observe(context.Context, jobs.EngineJob, func([]jobs.ResultItem) error) (Observation, error) {
	return Observation{}, Wait(w.reason)
}

func (w waitingModule) Cancel(context.Context, jobs.EngineJob) error { return Wait(w.reason) }

// ---- the one error mapping ------------------------------------------------------------------------------------------------------------------------------------------------------------

// phase is what a Module was doing when a call failed: it decides whether a refusal is for good.
type phase string

const (
	phaseSubmit  phase = "submit"
	phaseObserve phase = "observe"
	phaseCancel  phase = "cancel"
)

// mapError is THE place a module's error becomes the engine's: ONE function, and its table (TestMapError_Table) is the contract. module is the module id ("m1", "m2", "m3").
//
//	an error that is already ErrWait, ErrRefused or ErrNotSupported ........ unchanged
//	Unavailable, Timeout, Paused, RateLimited, Canceled ...................... ErrWait (the module cannot answer now)
//	Contract, Internal ........................................................ ErrWait too, said to be an answer this platform does not understand (the caller counts it): a module that
//	                                                                            speaks outside its contract never fails the job
//	an error that is no *modclient.Error at all ................................ ErrWait, text scrubbed
//	NotSupported ............................................................... ErrNotSupported at Cancel, ErrWait elsewhere
//	Refused, Invalid, NotFound ................................................ at Submit: ErrRefused with the module's code word (never its text), EXCEPT
//	                                                                              - a conflict (HTTP 409 or code conflict: module 2's back-pressure, module 3's named causes): ErrWait
//	                                                                              - a 401 or 403 or code unauthenticated (our key, our configuration): ErrWait
//	                                                                              - ErrIdempotencyKeyReused: the same key for another request can only be a platform bug: ErrRefused
//	                                                                            at Observe and Cancel: ErrWait
func mapError(module string, p phase, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrWait) || errors.Is(err, ErrRefused) || errors.Is(err, ErrNotSupported) {
		return err
	}
	var e *modclient.Error
	if !errors.As(err, &e) {
		return Wait(fmt.Sprintf("%s %s: an error of no known kind (%s)", module, p, scrubText(err.Error())))
	}
	switch e.Kind {
	case modclient.Unavailable, modclient.Timeout, modclient.Paused, modclient.RateLimited, modclient.Canceled:
		return Wait(describe(module, p, e, err, "the module cannot answer now"))
	case modclient.Contract, modclient.Internal:
		return Wait(describe(module, p, e, err, "the module answered something this platform does not understand"))
	case modclient.NotSupported:
		if p == phaseCancel {
			return fmt.Errorf("%w: %s", ErrNotSupported, describe(module, p, e, err, "the module has no such operation"))
		}
		return Wait(describe(module, p, e, err, "the module has no such operation"))
	case modclient.Refused, modclient.Invalid, modclient.NotFound:
		switch {
		case errors.Is(err, m1.ErrIdempotencyKeyReused):
			if p == phaseSubmit {
				return RefusedWithCode(e.Code, describe(module, p, e, err, "the idempotency key of the job was already used for a different request (a platform bug)"))
			}
		case isConflict(e, err):
			return Wait(describe(module, p, e, err, "the module is busy with a conflicting request"))
		case isOurCredential(e):
			return Wait(describe(module, p, e, err, "the module refuses the platform's credential (a configuration fault: nothing is failed for it)"))
		case p == phaseSubmit:
			return RefusedWithCode(e.Code, describe(module, p, e, err, "the module refused the job"))
		}
		return Wait(describe(module, p, e, err, "the module refused this request"))
	}
	return Wait(describe(module, p, e, err, "the module's error is of no known kind"))
}

// isConflict is true for the answers that mean "not now": module 2's back-pressure, module 3's named causes (suspended, campaign_active, campaign_cap, queue_cap) or one it does not name.
func isConflict(e *modclient.Error, err error) bool {
	return e.Status == 409 || e.Code == "conflict" || errors.Is(err, sendapi.ErrBackpressure) || errors.Is(err, sendapi.ErrConflictUnexplained) || len(sendapi.CausesOf(err)) > 0
}

// isOurCredential is true for the answers that mean the module did not accept the platform's own credential: that is the platform's configuration, never the customer's job.
func isOurCredential(e *modclient.Error) bool {
	return e.Status == 401 || e.Status == 403 || e.Code == "unauthenticated"
}

// describe is the text of a mapped error: the module, what it was doing, the class of the answer, its HTTP status and code word and, for module 3's refused run, the causes. Never a message of
// the module, a URL, an address or a key: only the fixed words of modclient's Error, and scrubText besides.
func describe(module string, p phase, e *modclient.Error, err error, why string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s (%s", module, p, why, e.Kind)
	if e.Status != 0 {
		fmt.Fprintf(&b, ", HTTP %d", e.Status)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, ", code %s", e.Code)
	}
	if causes := sendapi.CausesOf(err); len(causes) > 0 {
		words := make([]string, len(causes))
		for i, c := range causes {
			words[i] = string(c)
		}
		fmt.Fprintf(&b, ", causes %s", strings.Join(words, "+"))
	}
	if e.Kind == modclient.Contract || e.Kind == modclient.Internal {
		if e.Key != "" {
			fmt.Fprintf(&b, ", key %s", e.Key)
		}
	}
	b.WriteString(")")
	return scrubText(b.String())
}

// unexpectedKind is true for the answers mapError counts as ones the platform does not understand.
func unexpectedKind(err error) bool {
	var e *modclient.Error
	if !errors.As(err, &e) {
		return !errors.Is(err, ErrWait) && !errors.Is(err, ErrRefused) && !errors.Is(err, ErrNotSupported)
	}
	return e.Kind == modclient.Contract || e.Kind == modclient.Internal
}

// failed maps err for module in phase p, counts and logs an answer the platform does not understand (a warning with the running count) and returns the engine's error. held says whether any row
// of the job is held at the module (rows stamped before, or accepted in this call): a refusal for good is then NOT a failure of the job (see sendapiModule.Submit), it waits.
func (r *Registry) failed(ctx context.Context, module string, p phase, err error, held bool) error {
	mapped := mapError(module, p, err)
	if held && errors.Is(mapped, ErrRefused) {
		mapped = Wait("rows of this job are already held at the module, so a refusal is not a reason to fail it (an administrator decides): " + strings.TrimPrefix(mapped.Error(), ErrRefused.Error()+": "))
	}
	if unexpectedKind(err) {
		n := r.unexpected.Add(1)
		r.log.WarnContext(ctx, "a module answered something the platform does not understand; the job waits", "module", module, "phase", string(p), "why", mapped.Error(), "unexpected_answers", n)
	}
	return mapped
}

// ---- scrubbing ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

var (
	reAddress    = regexp.MustCompile(`[A-Za-z0-9._%+'\-]+@[A-Za-z0-9\-]+(\.[A-Za-z0-9\-]+)*`)
	reUserinfo   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s@]*@`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=\-]{8,}`)
	reNamedValue = regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|token|api[_-]?key|apikey|key|password|passwd|pwd|secret|authorization|auth|signature|sig)=[^&\s"';,]+`)
)

// maxDetailText bounds an error text that reaches a log or a job event.
const maxDetailText = 400

// scrubText makes a text safe to log or to put in a job event: URL credentials, bearer credentials, secret-looking name=value pairs and every address-shaped word are replaced, control
// characters (a newline would forge a log line) and invalid UTF-8 become spaces, and the result is cut to maxDetailText bytes on a character boundary. The adapters' errors are made of fixed words
// already; this is the wall behind them for an error of no known kind.
func scrubText(s string) string {
	s = reUserinfo.ReplaceAllString(s, "${1}[redacted]@")
	s = reBearer.ReplaceAllString(s, "${1} [redacted]")
	s = reNamedValue.ReplaceAllString(s, "${1}=[redacted]")
	s = reAddress.ReplaceAllString(s, "[address]")
	s = strings.ToValidUTF8(s, " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxDetailText {
		cut := maxDetailText
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}
