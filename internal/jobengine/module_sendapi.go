package jobengine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// sendapiModule is the Module of modules 2 and 3 (the senders) for one user: ONE implementation over the shared sender client (sendapi.Client, whose Profile says what differs between the two:
// module 3's 409 names its causes, module 2's is back-pressure). The platform job id IS the module's campaign id, and a row's id is the module's job_item_id (EnsureJobItems wrote
// public.job_items with those ids before the engine calls Submit).
//
// SUBMIT. Each row is one POST /v1/run, idempotent on the row id at the module (202 is a job just created, 200 is the job that already existed: both are accepted). The FIRST row goes alone,
// then the rest with at most `conc` calls in flight: module 2 allows one active campaign per user through a unique index, so two concurrent FIRST runs would race for it and one would be told the
// campaign is busy by a campaign that is its own. The first answer that stops the batch (a conflict, a module that cannot answer, a refusal) stops new calls from being started (a call in flight
// finishes: its row is accepted or offered again, never lost) and Submit answers with what it holds: some rows accepted is a PARTIAL acceptance, which is an answer and not an error (the engine
// stamps those rows and offers the others again at its next tick); no row accepted answers the error, mapped by mapError. A refusal for good is ErrRefused ONLY when nothing of the job is held at the
// module: with rows already held (this call or an earlier one) failing the job would leave them running at the module, charged to nobody, so the job waits and an administrator decides.
//
// OBSERVE. The campaign report is read page by page (sendapi.Client.Walk: it refuses a cursor that loops, a page that repeats an item, a page of another campaign). The first page's totals
// decide whether anything is new (terminal items beyond what the job has results for, or the module says it is complete): when not, the walk stops there, so a job in a long quiet stretch costs one
// request per observation and not one per page. Each TERMINAL item is a result (ItemRef the module's job_item_id, Outcome the profile's: module, state, reason); a queued, assigned or sending item is
// never handed over. A campaign the module does not know answers an empty page: nothing, not complete. A module that says complete beside a listing that does not hold as many terminal items as its
// own progress block says is never believed.
//
// CANCEL. POST /v1/cancel for the campaign (idempotent: queued items become cancelled, an item in flight finishes), asked again at every tick of a cancelling job by the engine.
type sendapiModule struct {
	reg    *Registry
	module string // "m2" or "m3": the charge table's module column and the label of every error
	c      *sendapi.Client
	conc   int
}

var _ Module = (*sendapiModule)(nil)

// Submit runs the rows at the module; see the type's comment for what its answer means.
func (m *sendapiModule) Submit(ctx context.Context, j jobs.EngineJob, rows []jobs.EngineRow) (SubmitResult, error) {
	if len(rows) == 0 {
		return SubmitResult{}, nil
	}
	ok := make([]bool, len(rows))
	var (
		mu   sync.Mutex
		stop error // the first error that stopped the batch
	)
	stopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stop != nil
	}
	run := func(i int) {
		if _, err := m.c.Run(ctx, rows[i].ID); err != nil {
			mu.Lock()
			if stop == nil {
				stop = err
			}
			mu.Unlock()
			return
		}
		mu.Lock()
		ok[i] = true
		mu.Unlock()
	}

	run(0) // the first row alone
	if !stopped() && len(rows) > 1 {
		work := make(chan int)
		var wg sync.WaitGroup
		workers := min(m.conc, len(rows)-1)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					run(i)
				}
			}()
		}
		for i := 1; i < len(rows) && !stopped() && ctx.Err() == nil; i++ {
			work <- i
		}
		close(work)
		wg.Wait()
	}

	accepted := make([]string, 0, len(rows))
	for i, good := range ok {
		if good {
			accepted = append(accepted, rows[i].ID)
		}
	}
	if stop == nil {
		return SubmitResult{Accepted: accepted}, nil
	}
	err := m.reg.failed(ctx, m.module, phaseSubmit, stop, j.Submitted > 0 || len(accepted) > 0)
	if len(accepted) > 0 && ctx.Err() == nil {
		// a partial acceptance is an answer: the module holds these rows and the engine must stamp them (and start the job) whatever stopped the rest
		m.reg.log.DebugContext(ctx, "a batch was stopped by the module; the rows it accepted are kept and the rest is offered again", "job", j.ID, "module", m.module,
			"accepted", len(accepted), "offered", len(rows), "why", err.Error())
		return SubmitResult{Accepted: accepted}, nil
	}
	return SubmitResult{}, err
}

// errNothingNew ends a walk after its first page: the module has nothing the engine does not have.
var errNothingNew = errors.New("jobengine: nothing new at the module")

// Observe reads the campaign's report and hands over every terminal item of it that the engine may not have.
func (m *sendapiModule) Observe(ctx context.Context, j jobs.EngineJob, fn func([]jobs.ResultItem) error) (Observation, error) {
	var (
		last      sendapi.CampaignPage
		first     = true
		delivered int
		fnErr     error
		fullWalk  bool
	)
	prof := m.c.Profile()
	_, err := m.c.Walk(ctx, j.ID, func(p sendapi.CampaignPage) error {
		last = p
		if first {
			first = false
			if !p.Progress.IsComplete && int(p.Progress.Terminal) <= j.PendingResults {
				return errNothingNew
			}
		}
		items := make([]jobs.ResultItem, 0, len(p.Items))
		for _, it := range p.Items {
			if it.Class != sendapi.ClassTerminal {
				continue
			}
			items = append(items, jobs.ResultItem{ItemRef: it.JobItemID, Outcome: prof.Outcome(it), Detail: itemDetail(it)})
		}
		delivered += len(items)
		if len(items) > 0 {
			if err := fn(items); err != nil {
				fnErr = err
				return err
			}
		}
		return nil
	})
	switch {
	case err == nil:
		fullWalk = true
	case errors.Is(err, errNothingNew):
	case fnErr != nil && errors.Is(err, fnErr):
		return Observation{}, err // the engine's own callback failed: its error, as it is
	default:
		return Observation{}, m.reg.failed(ctx, m.module, phaseObserve, err, false)
	}
	obs := Observation{Complete: last.Progress.IsComplete, Total: int(last.Totals.Total), Terminal: int(last.Progress.Terminal), Detail: map[string]any{
		"module": m.module, "total": last.Totals.Total, "sent": last.Totals.Sent, "bounced": last.Totals.Bounced, "failed": last.Totals.Failed, "cancelled": last.Totals.Cancelled,
		"in_flight": last.Totals.InFlight, "pending": last.Totals.Pending, "waiting_for_capacity": last.Totals.WaitingForCapacity, "dispatch_paused": last.DispatchPaused,
	}}
	if fullWalk && last.Progress.IsComplete && int64(delivered) != last.Progress.Terminal {
		m.reg.unexpected.Add(1)
		m.reg.log.WarnContext(ctx, "the module says the campaign is complete but its listing holds a different number of terminal items than its own progress block; the job is not completed",
			"job", j.ID, "module", m.module, "read", delivered, "terminal", last.Progress.Terminal)
		return Observation{}, Wait(fmt.Sprintf("%s observe: the campaign is complete but %d terminal item(s) were read and the module says %d: the listing contradicts the totals", m.module, delivered, last.Progress.Terminal))
	}
	return obs, nil
}

// Cancel asks the module to cancel the campaign.
func (m *sendapiModule) Cancel(ctx context.Context, j jobs.EngineJob) error {
	if _, err := m.c.Cancel(ctx, j.ID); err != nil {
		return m.reg.failed(ctx, m.module, phaseCancel, err, false)
	}
	return nil
}

// itemDetail is what the platform keeps of a reported item besides its outcome: words and counts only (never the module's SMTP response, its account name or the provider's message id, which can
// carry an address).
func itemDetail(it sendapi.Item) map[string]any {
	d := map[string]any{"attempts": it.AttemptCount}
	if it.ErrorClass != nil && *it.ErrorClass != "" {
		d["error_class"] = *it.ErrorClass
	}
	if it.CompletedAt != nil {
		d["completed_at"] = it.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return d
}
