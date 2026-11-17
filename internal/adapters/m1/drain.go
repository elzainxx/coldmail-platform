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
	// ErrIncomplete (Contract): the job reports complete, but the rows read do not add up to the job's own total.
	ErrIncomplete = errors.New("m1: the job is complete but the results read do not match its total")
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
// Why it is more than a loop over next_cursor. Module 1 lists a row once its work item has settled, in ordinal order, and a cursor is a position: a row that settles at a position the
// cursor already passed is not revisited. So Drain
//
//  1. follows next_cursor while there is one, handing fn the rows it has not delivered before (an ordinal is delivered once, whatever the page it appears on);
//  2. when the list is exhausted, asks the job: not complete means wait Poll and look again from the SAME cursor (the first position it has not finished);
//  3. complete means the listing is immutable: if the rows delivered are not the job's own total, one full walk from the start delivers whatever settled behind the cursor; and if
//     they still are not, ErrIncomplete: the platform never reports a job it did not read in full.
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
		return fmt.Errorf("%w (module 1 last said status %q, total %d, settled %d, pending %d, processing_paused %t; %d row(s) delivered)",
			err, last.Status, last.Counts.Total, last.Counts.Settled, last.Counts.Pending, last.ProcessingPaused, len(seen))
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
			if len(seen) != job.Counts.Total {
				before := st.Rows
				// MODULE-FORCED(M1-F4): while a job runs, module 1's listing is a moving target: a row that settles at an ordinal BELOW a cursor already issued is never revisited and
				// next_cursor "" does not mean done, so once the job says complete the adapter walks the whole listing once more from the start and delivers what settled behind the
				// cursor, and refuses a job whose rows still do not add up to its total.
				if err := a.walk(ctx, jobID, limit, deliver); err != nil {
					return st, explain(err)
				}
				st.CatchUp = st.Rows - before
			}
			if len(seen) != job.Counts.Total {
				return st, withCause(modclient.Contract, ErrIncomplete, "the job is complete but %d row(s) were read and it says total %d, settled %d, pending %d", len(seen), job.Counts.Total, job.Counts.Settled, job.Counts.Pending)
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
