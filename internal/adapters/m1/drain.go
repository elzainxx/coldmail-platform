package m1

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The causes the typed errors of Drain carry (errors.Is finds them; modclient.KindOf says which class they are).
var (
	// ErrNoDeadline (Invalid): Drain polls until a job completes, so it refuses a context that carries no deadline (it could wait for ever).
	ErrNoDeadline = errors.New("m1: Drain needs a context with a deadline")
	// ErrCursorStuck (Contract): a page named, as the next position, the position it was asked from.
	ErrCursorStuck = errors.New("m1: the next cursor is the cursor that was sent: the listing does not advance")
	// ErrIncomplete (Contract): the job reports complete, but the rows read are not the rows its totals say are final (totals.settled + totals.cancelled), or its final rows are not all of
	// the rows it says were submitted (totals.settled + totals.cancelled != totals.submitted, that is, a row is still pending).
	ErrIncomplete = errors.New("m1: the job is complete but the results read do not match its totals")
)

// withCause is a typed refusal that errors.Is can also match against one of the causes above.
func withCause(kind modclient.Kind, cause error, format string, args ...any) *modclient.Error {
	e := newErr(kind, routeResults, fmt.Sprintf(format, args...))
	e.Cause = cause
	if kind == modclient.Contract {
		e.Sent = true
	}
	return e
}

// DrainStats says what a Drain did.
type DrainStats struct {
	// Pages is the number of non-empty pages handed to the callback; Rows the number of rows in them.
	Pages int
	Rows  int
	// Waits is how often Drain found the list exhausted while the job was still running and waited.
	Waits int
	// CatchUp is the number of rows delivered by the pass after completion that the streaming pass had not seen: rows that settled behind the cursor.
	CatchUp int
}

// Drain reads a job's results page by page and calls fn with each page's NEW rows as it arrives, so the caller settles per page, per item, and never has to wait for the whole job.
// It returns when the job is complete and every row it has has been delivered exactly once.
//
// A job that is NOT complete is not given up on and not read as if it were finished: Drain polls it (the one deliberate wait; the context's deadline bounds it) and hands over rows as
// they settle. Any read that fails (a page, the status) ends the whole drain with that typed error: rows already delivered stay delivered, none is ever reported as the rest.
//
// Why it is more than a loop over next_cursor. Module 1 lists a row once its work item has settled, in ordinal order, and a cursor is a position: while the job runs, a row that
// settles at a position the cursor already passed is not revisited by that walk. So Drain
//
//  1. follows next_cursor while there is one, handing fn the rows it has not delivered before (an ordinal is delivered once, whatever the page it appears on);
//  2. when the list is exhausted, asks the job: not complete means wait Poll and look again from the SAME cursor (the first position it has not finished);
//  3. complete means the listing is stable (module 1 documents and tests that two full walks of a complete job's results are identical): if the rows delivered are not the number of
//     rows the job's totals say are final (settled with a verdict, or cancelled since module 1's cancel), one full walk from the start delivers whatever settled behind the cursor; and if
//     they are still not, or the job's final rows are not all of its submitted rows, ErrIncomplete: the platform never reports a job it did not read in full.
//
// A callback error stops the drain and is returned unchanged. The context must carry a deadline (ErrNoDeadline otherwise); when it expires the error says what module 1 last
// reported, so a stuck job explains itself.
func (a *Adapter) Drain(ctx context.Context, jobID string, fn func(rows []Row) error) (DrainStats, error) {
	var st DrainStats
	if fn == nil {
		return st, newErr(modclient.Invalid, routeResults, "Drain needs a callback")
	}
	if _, ok := ctx.Deadline(); !ok {
		return st, withCause(modclient.Invalid, ErrNoDeadline, "Drain needs a context with a deadline: it polls until the job completes")
	}
	limit := a.PageLimit
	if limit == 0 {
		limit = DefaultPageLimit
	}
	poll := a.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	seen := map[int]struct{}{}
	deliver := func(rows []Row) error {
		var fresh []Row
		for _, r := range rows {
			if _, dup := seen[r.Ordinal]; dup {
				continue
			}
			seen[r.Ordinal] = struct{}{}
			fresh = append(fresh, r)
		}
		if len(fresh) == 0 {
			return nil
		}
		st.Pages++
		st.Rows += len(fresh)
		return fn(fresh)
	}

	// A drain that runs out of time must say what module 1 last reported, so a stuck job explains itself (the error still unwraps to the cause).
	var last JobStatus
	haveLast := false
	explain := func(err error) error {
		if !haveLast || ctx.Err() == nil {
			return err
		}
		return fmt.Errorf("%w (module 1 last said status %q, total %d, settled %d, pending %d, cancelled %d, processing_paused %t; %d row(s) delivered)",
			err, last.Status, last.Totals.Submitted, last.Totals.Settled, last.Totals.Pending, last.Totals.Cancelled, last.ProcessingPaused, len(seen))
	}
	cursor := ""
	for {
		rows, next, err := a.Results(ctx, jobID, cursor, limit)
		if err != nil {
			return st, explain(err)
		}
		if err := deliver(rows); err != nil {
			return st, err
		}
		if next != "" {
			if next == cursor {
				return st, withCause(modclient.Contract, ErrCursorStuck, "the next cursor is the cursor that was sent: the listing does not advance (%d row(s) delivered)", st.Rows)
			}
			cursor = next
			continue
		}
		// the list is exhausted as it stands now
		job, err := a.Status(ctx, jobID)
		if err != nil {
			return st, explain(err)
		}
		last, haveLast = job, true
		if job.Complete {
			// the listing of a job holds its verdict rows (settled) AND, since module 1's cancel (migration 0020), its cancelled rows: both are served as rows, so the rows a complete job lists
			// are settled + cancelled (cancelled is 0 for a job never cancelled and for an older module, which leaves the arithmetic exactly what it was)
			if len(seen) != job.Totals.Settled+job.Totals.Cancelled {
				before := st.Rows
				// MODULE-FORCED(M1-F4): while a job RUNS, module 1's listing is a moving target (documented: a row that settles at an ordinal BELOW a cursor already issued is not revisited
				// by that walk, and next_cursor "" means "nothing further as the listing stands now", not "done"), so a job that completes after the streaming pass is read once more
				// from the start and what settled behind the cursor is delivered. The re-walk is sound because module 1 now documents and tests that the listing is STABLE from the
				// moment the status says complete: two full walks of a complete job are identical, so the second walk is the whole listing and no later one can differ.
				if err := a.walk(ctx, jobID, limit, deliver); err != nil {
					return st, explain(err)
				}
				st.CatchUp = st.Rows - before
			}
			// complete means every submitted row is FINAL (settled with a verdict, or cancelled) and every such row was read: the totals come from one snapshot with `complete`, so rows
			// delivered = settled + cancelled = submitted, and pending is 0. A complete that is believed with a row still pending is exactly what this refuses.
			if len(seen) != job.Totals.Settled+job.Totals.Cancelled || job.Totals.Settled+job.Totals.Cancelled != job.Totals.Submitted {
				return st, withCause(modclient.Contract, ErrIncomplete, "the job is complete but %d row(s) were read and it says total %d, settled %d, pending %d, cancelled %d", len(seen), job.Totals.Submitted, job.Totals.Settled, job.Totals.Pending, job.Totals.Cancelled)
			}
			return st, nil
		}
		st.Waits++
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			e := newErr(modclient.Canceled, routeResults, "the caller's context ended while waiting for the job to complete")
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				e.Kind, e.Message = modclient.Timeout, "the caller's deadline passed while waiting for the job to complete"
			}
			e.Cause, e.Sent = ctx.Err(), true
			return st, explain(e)
		case <-t.C:
		}
	}
}

// walk reads the whole listing from the first position and hands every page to deliver.
func (a *Adapter) walk(ctx context.Context, jobID string, limit int, deliver func([]Row) error) error {
	cursor := ""
	for {
		rows, next, err := a.Results(ctx, jobID, cursor, limit)
		if err != nil {
			return err
		}
		if err := deliver(rows); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if next == cursor {
			return withCause(modclient.Contract, ErrCursorStuck, "the next cursor is the cursor that was sent: the listing does not advance")
		}
		cursor = next
	}
}
