package jobs

import (
	"context"
	"errors"
	"math"
	"time"
)

// The reads of a job's results (platform.job_results): READ-ONLY, so the writers gate (scripts/check-job-writers.sh) has nothing to say about this file. The table is written by the engine and
// by Confirm and CancelBeforeStart, once per row and never changed; a result of a row whose outcome is not final yet is simply not there.
//
// Results are immutable and a page is a keyset on the row's ordinal, so a client that walks the pages of a FINISHED job with the last ordinal it saw neither repeats a row nor skips one. While a job
// is still running its results appear in the order the module settles the rows, not in the order of the list, so a result can be recorded with an ordinal the walk has already passed: such a walk
// is a snapshot of what was final when it went by, and a client that wants the whole list walks again once the job has finished.

const (
	// ResultsPageSize is the page EachResult reads: the results of a job are read, and written to a client, this many at a time, so memory stays bounded however long the list is.
	ResultsPageSize = 1000
	// MaxResultsLimit is the largest limit Results accepts: ResultsPageSize, plus the one extra row a caller reads to learn whether another page follows (the API's largest page is
	// ResultsPageSize, and it asks for one more).
	MaxResultsLimit = ResultsPageSize + 1
)

// Result is a row of platform.job_results as the store returns it: what became of one row of the job. Decision, Unit and Rule are internal/charge's (a result that was charged names its unit);
// Origin says whether the module or the platform decided (the platform's own outcomes are malformed, duplicate, suppressed, excluded, cancelled, expired and failed). The module's own detail
// for the row is not part of the result: it is the module's and not the customer's.
type Result struct {
	Ordinal  int    // the row's position in the job's list, from 1
	Email    string // the normalised address; for a malformed row, the address as it was typed
	Origin   string // module or platform
	State    string // the module's state for the row, or the platform's own word
	Reason   string // "" when there is none
	Decision string // charge, free or unmapped
	Unit     string // the unit charged; "" unless Decision is charge
	Rule     string // the charge table's rule label

	RecordedAt time.Time // when the result was written (the database's clock, UTC)
}

// Results returns the results of the user's job with an ordinal above afterOrdinal (0 starts at the first), in ordinal order, at most limit of them (1 to MaxResultsLimit). The job must be
// the user's: another user's job and a job that does not exist are both ErrNotFound, even when the page is empty (an empty page of a job the user owns is an empty page).
func (s *Store) Results(ctx context.Context, userID, jobID string, afterOrdinal, limit int) ([]Result, error) {
	if err := checkUser(userID); err != nil {
		return nil, opErr("results", err)
	}
	if !isUUID(jobID) {
		return nil, opErr("results", ErrNotFound)
	}
	if afterOrdinal < 0 {
		return nil, opErr("results", badRequest("the results cursor must not be negative"))
	}
	if limit < 1 || limit > MaxResultsLimit {
		return nil, opErr("results", badRequest("limit must be between 1 and %d", MaxResultsLimit))
	}
	// the user's own column is the ownership check: a result row carries the user of its job (the composite foreign key makes the two agree)
	rows, err := s.pool.Query(ctx, `SELECT r.ordinal, r.email, r.origin, r.module_state, COALESCE(r.reason, ''), r.decision, COALESCE(r.unit, ''), r.rule, r.created_at
		FROM platform.job_results r
		WHERE r.job_id = $1::uuid AND r.user_id = $2::uuid AND r.ordinal > $3
		ORDER BY r.ordinal LIMIT $4`, jobID, userID, int32OrMax(afterOrdinal), limit)
	if err != nil {
		return nil, opErr("results", s.fail(err))
	}
	defer rows.Close()
	out := make([]Result, 0, min(limit, ResultsPageSize))
	for rows.Next() {
		var r Result
		var ord int32
		if err := rows.Scan(&ord, &r.Email, &r.Origin, &r.State, &r.Reason, &r.Decision, &r.Unit, &r.Rule, &r.RecordedAt); err != nil {
			return nil, opErr("results", s.fail(err))
		}
		r.Ordinal, r.RecordedAt = int(ord), r.RecordedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr("results", s.fail(err))
	}
	rows.Close()
	if len(out) == 0 {
		// told apart from a job that is not the user's by reading the job itself, as Events does
		if _, err := s.getJob(ctx, s.pool, userID, jobID, false); err != nil {
			return nil, opErr("results", err)
		}
	}
	return out, nil
}

// int32OrMax keeps a cursor inside the column's type: an ordinal is an integer, so a cursor beyond it is past the last row of any job (a page that is empty), not an error of the database.
func int32OrMax(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

// ResultPager is what EachResultOf pages through; *Store satisfies it.
type ResultPager interface {
	Results(ctx context.Context, userID, jobID string, afterOrdinal, limit int) ([]Result, error)
}

// ErrPagerStuck is returned by EachResultOf when a pager answered a full page that does not advance: the walk would never end.
var ErrPagerStuck = errors.New("jobs: the results pager returned a full page that did not advance")

// EachResult calls fn for every result of the user's job, in ordinal order, reading ResultsPageSize at a time (the memory held is one page, however many results there are). It stops at the
// first error: fn's (returned as it is, so the caller can tell its own write failing from the store's), the store's, or the context's (checked before each page). A job that is not the user's is
// ErrNotFound before fn is ever called.
func (s *Store) EachResult(ctx context.Context, userID, jobID string, fn func(Result) error) error {
	return EachResultOf(ctx, s, userID, jobID, fn)
}

// EachResultOf is EachResult over any pager: the store's own, or a test's.
func EachResultOf(ctx context.Context, p ResultPager, userID, jobID string, fn func(Result) error) error {
	after := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := p.Results(ctx, userID, jobID, after, ResultsPageSize)
		if err != nil {
			return err
		}
		before := after
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
			after = r.Ordinal
		}
		if len(page) < ResultsPageSize {
			return nil
		}
		if after <= before { // cannot happen with the store: a pager that repeats its page for ever must not hold the caller
			return ErrPagerStuck
		}
	}
}
