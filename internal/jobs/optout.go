package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coldmail-platform/platform/internal/usersettings"
)

// THE PER-USER VENDOR OPT-OUT, in the job store (Phase 5 S6; Docs/phase-5-spec.md, decision 8; module 1's ADR-0024).
//
// WHAT IT IS. A customer is opted IN to module 1's paid vendor fallback by default and may opt out in their settings (internal/usersettings, GET and PUT /v1/settings). The setting is read ONCE, in the
// transaction that creates a module 1 job (freezeVendorChoice, below), and kept in the job's frozen settings (Settings.VendorOptOut, platform.jobs.frozen_settings, settings.vendor_opt_out). From then on the
// job is its own authority: a later change of the setting never alters a job that exists, and a job created before platform_0020 has no key and is opted in. A SEND job (modules 2 and 3) has no vendor: the
// setting is not read for it, and a key on one is refused by the database (jobs_vendor_opt_out_only_on_module_one).
//
// WHAT AN OPTED-OUT JOB DOES DIFFERENTLY (and only that; an opted-in job is exactly what it was):
//
//	the price      its reserve, preview and confirm use the NON-VENDOR price: the dearest reachable unit among the units the job can produce, which leaves out m1_vendor (money.ReserveRateForIn, ReserveIn:
//	               both read the job's own frozen choice). The vendor's reachability does not enter into the price at all, so an opted-out job is priced WITHOUT reading module 1's describe: syncVendor skips the
//	               reachability read for it (Preview.VendorReachableKnown is false: nothing was read for it), and it can never be refused for module 1's reachability.
//	the module     the engine submits the bulk job to module 1 under the policy module 1 itself published as vendor_fallback.opt_out_policy (internal/jobengine, m1Module), and checks that every result it reads says it was
//	               made under that policy.
//	the confirm    it is refused unless that key is known now (requireOptOutPolicy, below): ErrNotAllowed with ReasonOptOutPolicyUnavailable, nothing held. There is no fall-through to the default policy anywhere:
//	               a confirmed job whose key later becomes unknown WAITS in the engine (free, and it expires at its start deadline like any job a module never took) rather than be submitted under the policy that
//	               can reach the vendor.
//
// THE SAFETY NET. If a result of an opted-out job still maps to m1_vendor (module 1 was told not to use the vendor, so it must not happen), the job's price snapshot never held that unit, so money.SettleIn
// cannot price it: it is never charged, it is recorded as an unmapped result and flagged on the job (the existing "unpriced" path of Phase 4), and the credits stay in the reserve until release. The ledger's
// composite foreign key to the snapshot makes a charge for the unit impossible whatever any Go code believes.

// ReasonOptOutPolicyUnavailable is the Reason of the NotAllowedError a confirm of an opted-out module 1 job gets when module 1's opt-out policy key is not known: module 1 cannot be asked, or it is an older
// module 1 whose describe publishes none, or the platform is not wired to ask. The customer tries again once it is (the platform never submits an opted-out job under the default policy).
const ReasonOptOutPolicyUnavailable = "opt_out_policy_unavailable"

// ErrNoOptOutPolicy is what an OptOutPolicySource returns when module 1 ANSWERED and publishes no opt-out policy (an older module 1, or one that could not seed it): not a failure to ask, an answer. Callers
// treat it exactly like any other error: no key, no opted-out job. errors.Is finds it through wrapping.
var ErrNoOptOutPolicy = errors.New("module 1 publishes no opt-out policy")

// OptOutPolicySource is where the platform learns the key of module 1's opt-out policy: module 1's own describe (vendor_fallback.opt_out_policy). It is the ONE source of truth: the platform holds no
// constant for the key. It returns a non-empty key or an error (ErrNoOptOutPolicy when module 1 answered without one; any other error when it could not be asked); it never returns ("", nil). The error
// must carry no secret and no address. internal/app implements it over the module 1 adapter, tests inject a fake.
type OptOutPolicySource interface {
	M1OptOutPolicy(ctx context.Context) (key string, err error)
}

// WithOptOutPolicy sets the source of module 1's opt-out policy key that a confirm of an opted-out job asks. Without it (or with nil) no opted-out job can be confirmed (ReasonOptOutPolicyUnavailable).
func WithOptOutPolicy(src OptOutPolicySource) Option {
	return func(s *Store) { s.vendor.optOut = src }
}

// OptOutPolicyConfigured says whether the store was given a source for the key.
func (s *Store) OptOutPolicyConfigured() bool { return s.vendor.optOut != nil }

// requireOptOutPolicy is the confirm's check for an opted-out module 1 job: the key must be known NOW. It asks the source (bounded by vendorReadTimeout), outside every transaction like the vendor read, and
// writes nothing. A context the CALLER cancelled is returned as it is (it says nothing about module 1).
func (s *Store) requireOptOutPolicy(ctx context.Context, jobID string) error {
	src := s.vendor.optOut
	if src == nil {
		s.log.Info("confirm refused: the platform has no source for module 1's opt-out policy", "job", jobID, "reason", ReasonOptOutPolicyUnavailable)
		return notAllowed(ReasonOptOutPolicyUnavailable)
	}
	cctx, cancel := context.WithTimeout(ctx, vendorReadTimeout)
	defer cancel()
	key, err := src.M1OptOutPolicy(cctx)
	if err == nil && key != "" {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	word := "not_said"
	if err != nil && !errors.Is(err, ErrNoOptOutPolicy) {
		word = vendorFailureWord(err)
	}
	s.log.Info("confirm refused: module 1's opt-out policy is not known", "job", jobID, "reason", ReasonOptOutPolicyUnavailable, "module_1", word)
	return notAllowed(ReasonOptOutPolicyUnavailable)
}

// freezeVendorChoice makes the frozen settings of a NEW job: the settings and the campaign, with the one choice that is not the caller's. For a module 1 job it reads the user's setting NOW, in the creating
// transaction (usersettings.VendorOptOutOf, scoped by the user), and freezes it; whatever the caller put in Settings.VendorOptOut is replaced by what was read (a request cannot set it). For any other
// module the key is never set and the setting is not read. The settings it returns are what the job will hold.
func freezeVendorChoice(ctx context.Context, q querier, userID, module string, settings Settings, content *Content, mergeFields []string) ([]byte, Settings, error) {
	settings.VendorOptOut = false
	if module == ModuleVerify {
		optOut, err := usersettings.VendorOptOutOf(ctx, q, userID)
		if err != nil {
			return nil, Settings{}, err
		}
		settings.VendorOptOut = optOut
	}
	b, err := json.Marshal(frozenSettings{Settings: settings, Content: content, MergeFields: mergeFields})
	return b, settings, err
}

// OptOutPolicyReader keeps module 1's opt-out policy key for a few seconds, so that a burst of confirms and the engine's every pass cost one describe: it wraps a source and is one itself. The answer is
// kept, key or failure, for the TTL by its clock; an answer cut off by the CALLER's own context is returned and never kept. It never invents a key: what it returns is what the source returned, or an error.
type OptOutPolicyReader struct {
	src OptOutPolicySource
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	last *optOutReading
}

type optOutReading struct {
	key string
	err error
	at  time.Time
}

// NewOptOutPolicyReader wraps src with a memory of ttl (0 keeps nothing: every call asks) on the clock now (nil is time.Now). A nil src is a reader that never knows a key.
func NewOptOutPolicyReader(src OptOutPolicySource, ttl time.Duration, now func() time.Time) *OptOutPolicyReader {
	if now == nil {
		now = time.Now
	}
	return &OptOutPolicyReader{src: src, ttl: ttl, now: now}
}

// M1OptOutPolicy implements OptOutPolicySource.
func (r *OptOutPolicyReader) M1OptOutPolicy(ctx context.Context) (string, error) {
	if r == nil || r.src == nil {
		return "", ErrNoOptOutPolicy
	}
	at := r.now()
	if r.ttl > 0 {
		r.mu.Lock()
		if l := r.last; l != nil {
			if age := at.Sub(l.at); age >= 0 && age < r.ttl {
				cp := *l
				r.mu.Unlock()
				return cp.key, cp.err
			}
		}
		r.mu.Unlock()
	}
	key, err := r.src.M1OptOutPolicy(ctx)
	if err == nil && key == "" {
		err = ErrNoOptOutPolicy
	}
	if err != nil {
		key = ""
	}
	if r.ttl > 0 && ctx.Err() == nil {
		r.mu.Lock()
		r.last = &optOutReading{key: key, err: err, at: at}
		r.mu.Unlock()
	}
	return key, err
}
