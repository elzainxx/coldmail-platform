package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/money"
)

// THE VENDOR REACHABILITY HOOK (Phase 4, owner ruling OD-7 / row C3; Docs/phase-4-spec.md, Locked 23).
//
// What a module 1 job reserves depends on whether module 1's paid vendor fallback can answer at all: with it unreachable a job reserves 1 credit per row, with it reachable 2 (the dearest unit the
// job can be charged, m1_vendor). The platform does not decide that and has no setting for it: MODULE 1 says so, in its describe (vendor_fallback.reachable, computed per request from the very switches
// its external stage uses). The store reads it BEFORE the transaction of a module 1 Preview and Confirm (never an HTTP call under a lock: the rule of the module 3 pre-check), keeps the answer for a few
// seconds, and, when it is known and differs from the stored platform.unit_costs.reachable of m1_vendor, writes it through money.SetUnitReachable (the audited, idempotent SECURITY DEFINER function:
// source money.ReachableSourceM1Describe, no history row when nothing changes). The transaction that follows then reads the CURRENT truth: the reserve rate, the price snapshot the reserve freezes
// and the fingerprint's map of reachable prices are all computed from that flag, so a preview made under the other answer is a stale preview at confirm (the existing machinery), and a job already
// confirmed is not touched (its snapshot was frozen at its reserve).
//
// WHEN MODULE 1 CANNOT BE READ (it is down, slow, refuses the platform's key, or an older module 1 whose describe has no vendor_fallback) nothing is written and nobody is told the stored flag is fresh:
// Preview carries vendor_reachable_known false. The rule for a confirm is the customer-safe direction, a reserve at the dearest price and never an uncovered one:
//
//	reading         stored flag                                    immediate confirm                           scheduled confirm
//	known           (any)                                          the flag is set to the reading, then reserve the same
//	unreadable      reachable = true (whatever its source)         kept: reserves at the dearest price         the same
//	unreadable      reachable = false, source m1_describe          kept: module 1 itself said so last          the same
//	unreadable      reachable = false, any other source            REFUSED: ErrModuleUnavailable, nothing held the same (ignored: module 1 may be back by the start)
//	                (the seed of platform_0013, or the word of an earlier build's own configuration: never module 1's)
//
// so the one case that refuses is the seeded "off" that module 1 has never confirmed: a reserve at 1 per row would be a guess that module 1 could be charging the vendor's 2, and the platform will not
// hold credits for a job whose price it cannot know when the job could not start anyway (the same reasoning as module 3's pre-check, which also refuses an immediate confirm for a module that cannot be
// asked and ignores it for a scheduled one). Preview never refuses: it shows the numbers under the stored flag and says they are not fresh. A store with no provider (nil: module 1 is not configured
// in this process) makes no read and refuses nothing: the job waits for its module like every module 1 job of an unconfigured platform.

// VendorReachability is what the store asks of module 1 before a module 1 preview and confirm; internal/app implements it over the module 1 adapter's describe, tests inject a fake. known is false
// when module 1 did not say (an older module 1 whose describe has no vendor_fallback): reachable then means nothing. An error means module 1 could not be asked; it must carry no secret and no address
// (the store logs a word for it, never its text).
type VendorReachability interface {
	M1VendorReachable(ctx context.Context) (reachable, known bool, err error)
}

// DefaultVendorReadTTL is how long the store keeps module 1's last answer (or its failure) before it asks again: a few seconds, so that a burst of previews and confirms costs one describe, and a
// change of module 1's switches is seen by the next job after that. WithVendorReachabilityTTL replaces it; 0 turns the memory off (every preview and confirm asks).
const DefaultVendorReadTTL = 5 * time.Second

// vendorReadTimeout bounds one describe, whatever the module dials say: a preview and a confirm wait for it, and a module that hangs must cost seconds, not the module call timeout times its retries.
const vendorReadTimeout = 5 * time.Second

// WithVendorReachability sets the provider of module 1's vendor reachability. Without it (or with nil) the store reads nothing and refuses nothing for it.
func WithVendorReachability(p VendorReachability) Option {
	return func(s *Store) { s.vendor.provider = p }
}

// WithVendorReachabilityTTL sets how long the store keeps module 1's last answer (default DefaultVendorReadTTL; 0 means every call asks; a negative value is refused by New). The clock is the
// store's (WithClock), so a test moves it instead of waiting.
func WithVendorReachabilityTTL(d time.Duration) Option { return func(s *Store) { s.vendor.ttl = d } }

// VendorReachability is the provider the store was built with (nil when none): the service builds ONE provider and gives it to the store and to the reachability loop, and a test holds the two equal.
func (s *Store) VendorReachability() VendorReachability { return s.vendor.provider }

// vendorHook is the store's state for the hook: the provider, the memory and its lock.
type vendorHook struct {
	provider VendorReachability
	ttl      time.Duration
	// optOut is where a confirm of an opted-out job learns module 1's opt-out policy key (optout.go); nil: none is known, and no opted-out job can be confirmed.
	optOut OptOutPolicySource

	mu   sync.Mutex
	last *vendorReading
}

// vendorReading is one answer of the provider, kept for the TTL.
type vendorReading struct {
	reachable bool
	known     bool
	failure   string // "" when module 1 answered; the word of the failure otherwise
	at        time.Time
}

// read returns the provider's answer: the remembered one while it is younger than the TTL, a fresh one otherwise. fresh says it was asked now (so the caller logs a failure once, not on every
// preview of an outage). The asking is bounded by vendorReadTimeout and happens outside the lock; two callers that miss together both ask, which costs one extra describe and no more. An answer cut
// off by the CALLER's own context is returned but never remembered (it says nothing about module 1).
func (s *Store) readVendor(ctx context.Context) (r vendorReading, fresh bool) {
	h := &s.vendor
	now := s.now()
	if h.ttl > 0 {
		h.mu.Lock()
		if l := h.last; l != nil {
			if age := now.Sub(l.at); age >= 0 && age < h.ttl {
				cp := *l
				h.mu.Unlock()
				return cp, false
			}
		}
		h.mu.Unlock()
	}
	cctx, cancel := context.WithTimeout(ctx, vendorReadTimeout)
	reachable, known, err := h.provider.M1VendorReachable(cctx)
	cancel()
	r = vendorReading{at: now}
	switch {
	case err != nil:
		r.failure = vendorFailureWord(err)
	case !known:
		r.failure = "not_said"
	default:
		r.reachable, r.known = reachable, true
	}
	if h.ttl > 0 && ctx.Err() == nil {
		h.mu.Lock()
		cp := r
		h.last = &cp
		h.mu.Unlock()
	}
	return r, true
}

// vendorFailureWord is the word a log line says about a failed read: never the error's text.
func vendorFailureWord(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "unreadable"
}

// vendorRefuses is the one rule that refuses (see the table above): an IMMEDIATE confirm whose reachability module 1 could not give, while the stored flag is off and was never set from module 1's own answer.
func vendorRefuses(immediate bool, stored money.UnitReachability) bool {
	return immediate && !stored.Reachable && !stored.FromModule()
}

// syncVendor is the hook of Preview and Confirm. It does nothing for a store with no provider, for a job that does not exist or is not the user's (the transaction that follows says so) and for a
// job that is not a module 1 job. For a module 1 job it reads module 1 and: on a known answer it makes the stored flag equal to it (known true is returned); on an unreadable one it keeps the stored
// flag, returns known false and, for an immediate confirm only, refuses by vendorRefuses with ErrModuleUnavailable. Its other errors are the money store's (a lock timeout while a reserve in
// progress holds the unit rows, a connection) and stop the call before anything is held.
//
// A module 1 job that is OPTED OUT of the vendor (Phase 5 S6, optout.go) is priced without the vendor unit whatever module 1 says, so for it module 1's reachability is NOT read and nothing about it can
// refuse the job: known is false (nothing was read for it), the stored flag is left alone, and a confirm is refused only by requireOptOutPolicy (module 1's opt-out policy key must be known). A store with a
// provider OR a source for that key looks at the job first (one small read) and holds an opted-out job to that rule; a store with neither (module 1 is not configured in this process) reads nothing at all
// and refuses nothing: the job waits for its module, and the engine will not submit an opted-out job without the key either.
func (s *Store) syncVendor(ctx context.Context, userID, jobID string, confirm, immediate bool) (known bool, err error) {
	if s.vendor.provider == nil && s.vendor.optOut == nil {
		return false, nil // module 1 is not configured in this process: nothing is read and nothing is refused (the job waits for its module like every module 1 job of an unconfigured platform)
	}
	var module string
	var optOut bool
	switch err := s.pool.QueryRow(ctx, `SELECT module_id, COALESCE((frozen_settings #>> '{settings,vendor_opt_out}')::boolean, false) FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID).Scan(&module, &optOut); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, s.fail(err)
	}
	if module != ModuleVerify {
		return false, nil
	}
	if optOut {
		if confirm {
			return false, s.requireOptOutPolicy(ctx, jobID)
		}
		return false, nil
	}
	if s.vendor.provider == nil {
		return false, nil
	}
	r, fresh := s.readVendor(ctx)
	if r.known {
		stored, err := s.mny.UnitReachability(ctx, money.UnitM1Vendor)
		if err != nil {
			return false, err
		}
		if stored.Reachable != r.reachable {
			changed, err := s.mny.SetUnitReachable(ctx, money.UnitM1Vendor, r.reachable, money.ReachableSourceM1Describe)
			if err != nil {
				return false, err
			}
			if changed {
				s.log.Info("module 1's vendor reachability changed the price flag", "reachable", r.reachable, "while", whileWord(confirm))
			}
		}
		return true, nil
	}
	if fresh {
		s.log.Warn("module 1's vendor reachability could not be read: the stored flag stands", "reason", r.failure, "while", whileWord(confirm))
	}
	if !confirm {
		return false, nil
	}
	stored, err := s.mny.UnitReachability(ctx, money.UnitM1Vendor)
	if err != nil {
		return false, err
	}
	if vendorRefuses(immediate, stored) {
		s.log.Info("confirm refused: module 1's vendor reachability is unknown and the stored flag was never module 1's word", "job", jobID, "reason", refusalWord(ErrModuleUnavailable))
		return false, ErrModuleUnavailable
	}
	return false, nil
}

func whileWord(confirm bool) string {
	if confirm {
		return "confirm"
	}
	return "preview"
}
