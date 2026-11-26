package jobengine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// maxM1Pages bounds one walk of module 1's results: a job holds at most 100,000 rows and a page 1,000, so a listing that does not end within 2,000 pages is a module that never ends.
const maxM1Pages = 2000

// m1Module is the Module of module 1 (verify) for one user: ONE bulk job per platform job, keyed by the platform job id.
//
// SUBMIT. Module 1 takes the whole list in one request and deduplicates by the idempotency key, and the key is the platform job id: a repeat of the same list is the same module job (a replay), and
// a DIFFERENT list under that key is a 409 (m1.ErrIdempotencyKeyReused: it can only be a platform bug, so it is the one refusal for good). A job's list that does not fit in ONE call is therefore
// never submitted in parts (a second call under the same key would be that 409, and a second key would be a second module job the platform has no place to record): it waits, free, until its
// start deadline (see Submit).
//
// OBSERVE. The module job's own id is the module reference the engine's Start recorded in platform.jobs.module_job_ref: it arrives on the job the engine hands to Observe (jobs.EngineJob.ModuleRef,
// read with the job by the store), so a restart or a new leader needs nothing of its own, and a job that carries none WAITS (nothing is read, nothing is invented). Status is read first and is
// cheap; the results are walked only when the engine has something to gain (the module settled more rows than the job has results for) or the module says it is complete, and then from the first
// page, each row an item: ItemRef the row id the platform sent, Outcome the row's own words (m1.Row.Outcome), Detail its position. A complete job is believed only when the listing read equals the
// module's own totals.
type m1Module struct {
	reg *Registry
	ad  *m1.Adapter
}

var _ Module = (*m1Module)(nil)

// Submit hands the whole list to module 1. The rows must be the job's whole list (the engine's batch for module 1 is the job's size: Config.SubmitBatchFor); anything else waits.
func (m *m1Module) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (SubmitResult, error) {
	if len(rows) == 0 {
		return SubmitResult{}, nil
	}
	if j.Submitted > 0 || len(rows) != j.Pending {
		return SubmitResult{}, Wait(fmt.Sprintf("m1 submit: module 1 takes a job's whole list in ONE request and this call offers %d of the job's %d rows (%d already handed over): the job waits and expires free rather than being split into a second module job",
			len(rows), j.Pending, j.Submitted))
	}
	inputs := make([]m1.Input, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		inputs[i] = m1.Input{ItemRef: r.ID, Address: r.EmailNorm}
		ids[i] = r.ID
	}
	plan := m1.Prepare(inputs)
	if err := plan.Err(); err != nil {
		return SubmitResult{}, m.reg.failed(ctx, jobs.ModuleVerify, phaseSubmit, err, false)
	}
	if n := len(plan.Malformed); n > 0 {
		// the store classified these rows as good addresses and module 1's rule (the same function) says otherwise: the two have drifted. Nothing is sent; a partial list would leave rows that can
		// never be handed over and a job that can never complete.
		m.reg.log.WarnContext(ctx, "pending rows are malformed by module 1's address rule although the store called them good: the platform's classification and the adapter's have drifted; nothing was sent", "job", j.ID, "rows", n)
		return SubmitResult{}, Wait(fmt.Sprintf("m1 submit: %d pending row(s) are malformed by the address rule the adapter applies, which the store did not find: nothing is sent (a platform bug to find)", n))
	}
	res, err := m.ad.Submit(ctx, plan, j.ID)
	if err != nil {
		var mm *m1.MismatchError
		if errors.As(err, &mm) {
			// module 1 HOLDS the job (its id is a validated uuid) but counted it differently from the plan: nothing is believed and nothing is invented; the repeat under the same key finds the same job
			m.reg.log.WarnContext(ctx, "module 1 accepted the job but its counts are not the plan's; the job waits", "job", j.ID, "module_job", mm.JobID)
		}
		return SubmitResult{}, m.reg.failed(ctx, jobs.ModuleVerify, phaseSubmit, err, false)
	}
	return SubmitResult{Accepted: ids, ModuleRef: res.JobID}, nil
}

// Observe reads module 1's state of the job and hands over every settled row the engine may not have.
func (m *m1Module) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (Observation, error) {
	ref := strings.TrimSpace(j.ModuleRef)
	if ref == "" {
		return Observation{}, Wait("m1 observe: the job carries no module 1 id (the engine records it when it starts the job, and hands the job to Observe as the store read it): nothing is read and nothing is invented")
	}
	st, err := m.ad.Status(ctx, ref)
	if err != nil {
		return Observation{}, m.reg.failed(ctx, jobs.ModuleVerify, phaseObserve, err, false)
	}
	// a row is FINAL when it has a verdict (settled) or was cancelled before its check began (cancelled: no verdict, free, an item of the listing of its own): submitted = settled + cancelled + pending
	final := st.Totals.Settled + st.Totals.Cancelled
	obs := Observation{Complete: st.Complete, Total: st.Totals.Submitted, Terminal: final, Detail: map[string]any{
		"module": jobs.ModuleVerify, "status": st.Status, "submitted": st.Totals.Submitted, "settled": st.Totals.Settled, "cancelled": st.Totals.Cancelled, "pending": st.Totals.Pending,
		"duplicates": st.Totals.Duplicates, "malformed": st.Totals.Malformed, "processing_paused": st.ProcessingPaused, "cancel_requested": st.CancelRequested,
	}}
	if !st.Complete && final <= j.PendingResults {
		return obs, nil // nothing the engine does not have: the status alone answers
	}
	delivered, skipped, err := m.walk(ctx, ref, fn)
	if err != nil {
		var ff fnFailure
		if errors.As(err, &ff) {
			return Observation{}, ff.err // the engine's own callback failed: its error, as it is
		}
		return Observation{}, m.reg.failed(ctx, jobs.ModuleVerify, phaseObserve, err, false)
	}
	if skipped > 0 {
		obs.Detail["skipped_duplicates"] = skipped
	}
	if st.Complete && (delivered != final || final != st.Totals.Submitted) {
		// the module says complete and its own listing says otherwise: the job is NEVER believed complete on that (the rows that were read stay recorded). The listing of a finished job holds
		// settled + cancelled rows, which are all of the submitted ones.
		m.reg.unexpected.Add(1)
		m.reg.log.WarnContext(ctx, "module 1 says the job is complete but its results listing contradicts its totals; the job is not completed", "job", j.ID,
			"read", delivered, "settled", st.Totals.Settled, "cancelled", st.Totals.Cancelled, "submitted", st.Totals.Submitted)
		return Observation{}, Wait(fmt.Sprintf("m1 observe: the job is complete but %d row(s) were read and it says submitted %d, settled %d, cancelled %d: the listing contradicts the totals", delivered, st.Totals.Submitted, st.Totals.Settled, st.Totals.Cancelled))
	}
	return obs, nil
}

// Cancel asks module 1 to cancel the job (POST /v1/jobs/{id}/cancel with the user's own key, idempotent): every row not yet claimed becomes `cancelled` (free, no verdict) and arrives at the next
// Observe as a result of its own, every row already leased or in flight finishes and settles as a verdict, charged as the table says. The module's three answers (cancelled, already_cancelled,
// already_complete) are all success: a repeat is never an error, and a cancel of a job that had nothing left to cancel changes nothing (the platform's job still ends cancelled, because the user's
// request was accepted; every row it settled stays settled and charged). The job needs its module reference (Start recorded it): without it the cancel waits like Observe does.
func (m *m1Module) Cancel(ctx context.Context, j jobs.EngineJob) error {
	ref := strings.TrimSpace(j.ModuleRef)
	if ref == "" {
		return Wait("m1 cancel: the job carries no module 1 id (the engine records it when it starts the job): nothing is sent")
	}
	res, err := m.ad.Cancel(ctx, ref)
	if err != nil {
		return m.reg.failed(ctx, jobs.ModuleVerify, phaseCancel, err, false)
	}
	m.reg.log.DebugContext(ctx, "module 1 answered the cancel", "job", j.ID, "outcome", res.Outcome)
	return nil
}

// Pause: module 1 has no per-job pause (its only "paused" is the operator's front-door kill switch, module-wide and not the caller's choice). The database refuses a pause of a module 1 job before the
// engine is asked (platform.modules.can_pause is false for it), and pausing by cancel and resubmit is never done.
func (m *m1Module) Pause(context.Context, jobs.EngineJob) error {
	return fmt.Errorf("%w: module 1 has no per-job pause", ErrNotSupported)
}

// Resume: module 1 has no per-job pause, so there is nothing to resume.
func (m *m1Module) Resume(context.Context, jobs.EngineJob) error {
	return fmt.Errorf("%w: module 1 has no per-job pause", ErrNotSupported)
}

// fnFailure carries an error of the engine's own callback through the walk, so that it is returned to the engine as it came and never taken for an answer of the module.
type fnFailure struct{ err error }

func (f fnFailure) Error() string { return f.err.Error() }

// walk reads module 1's results from the first page to the last and hands each page's rows to fn. It returns how many rows it read and how many it did not hand over (a row module 1 marks as a
// repeat of an earlier address is the PLATFORM's duplicate, which the store never submits: handing it over as module 1's verdict would charge the address twice, and as the platform's outcome
// it would be refused by the store, so it is counted and left to the engine's completeness check).
func (m *m1Module) walk(ctx context.Context, ref string, fn func([]jobs.ResultItem) error) (read, skipped int, err error) {
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxM1Pages {
			return read, skipped, Wait(fmt.Sprintf("m1 observe: the results listing did not end within %d pages", maxM1Pages))
		}
		rows, next, err := m.ad.Results(ctx, ref, cursor, m1.MaxPageLimit)
		if err != nil {
			return read, skipped, err
		}
		items := make([]jobs.ResultItem, 0, len(rows))
		for _, r := range rows {
			read++
			if r.DuplicateOf != nil {
				skipped++
				continue
			}
			items = append(items, jobs.ResultItem{ItemRef: r.ItemID, Outcome: r.Outcome(), Detail: map[string]any{"ordinal": r.Ordinal}})
		}
		if len(items) > 0 {
			if err := fn(items); err != nil {
				return read, skipped, fnFailure{err}
			}
		}
		switch {
		case next == "":
			return read, skipped, nil
		case next == cursor:
			return read, skipped, Wait("m1 observe: the results listing's next cursor is the cursor that was sent: it does not advance")
		}
		cursor = next
	}
}
