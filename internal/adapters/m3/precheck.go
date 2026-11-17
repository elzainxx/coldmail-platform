package m3

import (
	"errors"
	"fmt"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The causes the platform can name before it asks, from its OWN state. Each is carried by an error that also unwraps to a modclient.Invalid error (the request was refused here, before
// anything was sent): errors.Is(err, ErrOverQueueCap), modclient.KindOf(err) == modclient.Invalid.
var (
	// ErrNotEntitled: the user's module 3 entitlement is not active (platform.m3_entitlements). Module 3 itself serves any user that exists and was never suspended, so the entitlement gate is
	// the platform's alone (research m3-api section 13).
	ErrNotEntitled = errors.New("m3: the user's module 3 entitlement is not active")
	// ErrJobActive: the platform's own rule, one active job per module per user, says a job of this user is active. The module's own version of the rule (another CAMPAIGN is active) answers
	// 409; the platform's is stricter and is checked first.
	ErrJobActive = errors.New("m3: a module 3 job of this user is already active")
	// ErrOverCampaignCap: the campaign has more recipients than the module's campaign size cap (max_campaign_size): the module refuses the first run past it.
	ErrOverCampaignCap = errors.New("m3: the campaign is larger than the module's campaign size cap")
	// ErrOverQueueCap: the user's live queue plus this campaign is more than the module's queue depth cap (max_queued_jobs): the module refuses the first run past it.
	ErrOverQueueCap = errors.New("m3: the campaign does not fit the module's queue depth cap")
	// ErrConflictUnexplained is sendapi's: module 3's 409 on run whose cause the module does not say. ExplainConflict wraps it; nothing in this package ever replaces it with one of the
	// four causes above.
	ErrConflictUnexplained = sendapi.ErrConflictUnexplained
)

// Limit is a number the platform may or may not know. The zero Limit is "not known": a check that needs a number nobody supplied is not made, and says so (RunFacts.NotChecked); it is never
// made against zero.
type Limit struct {
	N     int64
	Known bool
}

// KnownLimit is a limit the platform knows.
func KnownLimit(n int64) Limit { return Limit{N: n, Known: true} }

// RunFacts are the platform's own facts about a run it is about to submit: all of them are known to the platform before any call to module 3.
type RunFacts struct {
	// Entitled: the user's module 3 entitlement is active now.
	Entitled bool
	// JobActive: the platform's one-active-job-per-module-per-user rule says a job of this user is active at module 3.
	JobActive bool
	// RecipientCount is the number of items the platform will submit as this campaign: at least 1.
	RecipientCount int64
	// LiveQueuedJobs is the number of this user's jobs the platform knows to be queued, assigned or sending at module 3 already (0 under the one-active-job rule, apart from a test-send, which
	// the platform cannot see).
	LiveQueuedJobs int64
	// QueueDepthCap is the module's queue depth cap: the quote states it (Feasibility.QueueDepthLimit).
	QueueDepthCap Limit
	// CampaignSizeCap is the module's campaign size cap. The module states it nowhere on its user-facing surface, so only the platform can supply it (its own knowledge of the dial); unknown
	// means the campaign size is not checked.
	//
	// MODULE-FORCED(M3-F1): the module computes max_campaign_size for the quote and never returns it (no Domain A route does), so this cap cannot be taken from the quote like the queue depth cap and is supplied by the platform or left unchecked.
	CampaignSizeCap Limit
}

// NotChecked names the checks PreCheckRun cannot make with these facts, because a number it needs was not supplied. A caller that logs or surfaces the pre-check also surfaces this list:
// a check that covers only part of its domain says how much it did not check.
func (f RunFacts) NotChecked() []string {
	var out []string
	if !f.CampaignSizeCap.Known {
		out = append(out, "campaign_size_cap")
	}
	if !f.QueueDepthCap.Known {
		out = append(out, "queue_depth_cap")
	}
	return out
}

// refusal is a pre-check error: a sentinel and the modclient.Invalid error that says the request was refused here.
type refusal struct {
	sentinel error
	err      *modclient.Error
}

func (r *refusal) Error() string   { return r.sentinel.Error() }
func (r *refusal) Unwrap() []error { return []error{r.sentinel, r.err} }

func refuse(sentinel error) error {
	return &refusal{sentinel: sentinel, err: &modclient.Error{Kind: modclient.Invalid, Module: modclient.M3, Route: "m3.run", Message: "the run was refused before it was sent, from the platform's own state"}}
}

// PreCheckRun decides from the platform's own facts, before any network call, whether a run can possibly be accepted, so that an unexplained 409 from the module is the exception. It
// returns nil when no cause is found, and otherwise a typed refusal naming EVERY cause that applies (errors.Join when more than one), in the order the module itself tests them:
//
//	ErrNotEntitled      the entitlement is not active (the module's own suspension, which an entitlement lapse becomes, is its first gate)
//	ErrJobActive        a job of this user is active (the module's "another campaign active")
//	ErrOverCampaignCap  RecipientCount > CampaignSizeCap (the module refuses the run that would be job number cap+1 of one campaign)
//	ErrOverQueueCap     LiveQueuedJobs + RecipientCount > QueueDepthCap (the module refuses the run that would make the live queue longer than the cap)
//
// The queue check is the module's own quote rule made exact: it treats every item as queued at once, which is how run is called (one request per item, all of them before the first send
// finishes, at the module's pace). It can therefore refuse a campaign that would have squeezed in because the queue drained while it was submitted; that is the safe direction.
//
// A nil result is NOT a promise that the module will accept: it knows nothing of a suspension the platform has not converged, of a test-send in flight, or of a cap nobody supplied
// (RunFacts.NotChecked says which). When the module answers 409 anyway, ExplainConflict records that the cause is unknown.
//
// Facts that cannot describe a run (no recipients, a negative count, a negative limit) are a modclient.Invalid error, not a cause: nothing is decided from nonsense.
func PreCheckRun(f RunFacts) error {
	const route = "m3.run"
	switch {
	case f.RecipientCount < 1:
		return invalid(route, "the campaign has no recipients to run")
	case f.LiveQueuedJobs < 0:
		return invalid(route, "the number of live queued jobs is negative")
	case f.QueueDepthCap.Known && f.QueueDepthCap.N < 0, f.CampaignSizeCap.Known && f.CampaignSizeCap.N < 0:
		return invalid(route, "a cap is negative")
	}
	var causes []error
	if !f.Entitled {
		causes = append(causes, refuse(ErrNotEntitled))
	}
	if f.JobActive {
		causes = append(causes, refuse(ErrJobActive))
	}
	if f.CampaignSizeCap.Known && f.RecipientCount > f.CampaignSizeCap.N {
		causes = append(causes, refuse(ErrOverCampaignCap))
	}
	// Live + N > cap, written as a subtraction so that no sum of two large counts can wrap into a small one (cap >= 0 and N >= 1, so cap - N cannot underflow).
	if f.QueueDepthCap.Known && f.LiveQueuedJobs > f.QueueDepthCap.N-f.RecipientCount {
		causes = append(causes, refuse(ErrOverQueueCap))
	}
	switch len(causes) {
	case 0:
		return nil
	case 1:
		return causes[0]
	}
	return errors.Join(causes...)
}

// ExplainConflict is what the platform records when module 3 answers a run with 409 although PreCheckRun passed. It returns err unchanged unless err is the refusal of a RUN (route m3.run,
// Refused, HTTP 409); for that refusal it returns an error that is ErrConflictUnexplained (and still the modclient Refused error underneath, so modclient.KindOf and errors.As keep working),
// worded "refused, cause unknown". It NEVER names a cause: the module's 409 is one body for four causes (suspended, another campaign active, campaign size cap, queue depth cap), and the
// answer carries nothing that tells them apart, so any cause picked here would be a guess, and a body that pretends to say (a detail naming a field, a different sentence) is not believed.
func ExplainConflict(err error) error {
	var e *modclient.Error
	if err == nil || !errors.As(err, &e) {
		return err
	}
	if e.Route != "m3.run" || e.Kind != modclient.Refused || e.Status != 409 {
		return err
	}
	// MODULE-FORCED(M3-F3): the module answers run's 409 with one body for four causes (suspended, another campaign active, campaign size cap, queue depth cap) and the quote predicts two of them wrongly, so the adapter can only record "refused, cause unknown" and must never pick one.
	return fmt.Errorf("%w: m3 run refused with HTTP 409, cause unknown (suspended, another campaign active, campaign size cap or queue depth cap: the module does not say which): %w", ErrConflictUnexplained, err)
}
