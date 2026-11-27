package jobs

import (
	"context"
	"fmt"
	"time"
)

// CONTENT RETENTION (Phase 5 S8, Docs/phase-5-spec.md decision 7). The platform keeps the rendered message of every recipient of a SEND job in public.job_items, because that is where modules 2 and 3 read it. Once the
// job has ended and PLATFORM_CONTENT_RETENTION_DAYS have passed, the platform has no reason to hold it: this file clears it.
//
// WHAT IS CLEARED, per row of the ended job: rendered_subject and rendered_body become the placeholder ContentPlaceholder (the modules' own columns are NOT NULL, and their pre-flight accepts a non-blank
// string), rendered_text and unsubscribe_url become NULL. reply_to is not the content of the message and stays; so do the recipient, the status marker and the row's id (the item reference every outcome names).
//
// WHAT IS NEVER TOUCHED: the outcomes. platform.job_rows, platform.job_results, platform.job_events and the ledger are not written here at all (this file's only writes are public.job_items and the one mark of
// platform.jobs), and a test compares a digest of them before and after. The items of a job that has NOT ended, and the rows of public.job_items that belong to no job (module 3's test-send writes its own,
// with the user's id as campaign_id), are never matched: every page of the purge names the job and its owner and the job's terminal status in the statement itself, whatever the caller believes.
//
// HOW IT RUNS. One job at a time, in pages of rowBatch items by id, each page ITS OWN short statement and transaction, so a job of 100,000 rows is never one long lock. The mark platform.jobs.content_cleared_at is
// set only after the last page, and the purge selects only jobs without it, so a crash in the middle simply resumes: what is left to clear is read from the items themselves (see clearJobContent), never from a
// memory of the purge. The caller bounds the work of one call with maxPages; a job it could not finish is left unmarked and the next call goes on with it, from where the items say.
//
// THE CLOCK IS THE DATABASE'S: "older than the dial" is finished_at <= now() - interval in SQL.

// ContentPlaceholder is what rendered_subject and rendered_body hold after the purge.
const ContentPlaceholder = "[removed]"

// The bounds of the retention the store accepts: one second (so that a test can use seconds; the dial of the service is in days) to 365 days, the top of PLATFORM_CONTENT_RETENTION_DAYS (pinned equal by a test in
// internal/config).
const (
	MinContentRetention = time.Second
	MaxContentRetention = 365 * 24 * time.Hour
)

// The bounds of one ClearRetainedContent call: how many ended jobs it looks at and how many pages (of rowBatch rows) it may write in all.
const (
	DefaultRetentionJobs  = 20
	MaxRetentionJobs      = 1000
	DefaultRetentionPages = 8
	MaxRetentionPages     = 1000
)

// RetentionResult is what one call did. Found is how many ended send jobs were due; Cleared how many it finished and marked; Skipped how many it finished but another call had marked first (nothing of its own
// to say); Unfinished how many it left for the next call because its page budget ran out; Pages and Items how many pages it wrote and how many rows of public.job_items it changed; Failed how many jobs a
// database error stopped (the first error is returned beside the result).
type RetentionResult struct {
	Found      int
	Cleared    int
	Skipped    int
	Unfinished int
	Failed     int
	Pages      int
	Items      int64
}

// checkRetention is the pure half of the dial's check.
func checkRetention(after time.Duration) error {
	if after < MinContentRetention || after > MaxContentRetention {
		return badRequest("the content retention must be between %s and %s", MinContentRetention, MaxContentRetention)
	}
	return nil
}

// retentionBounds applies the defaults and the limits of one call.
func retentionBounds(maxJobs, maxPages int) (int, int, error) {
	if maxJobs == 0 {
		maxJobs = DefaultRetentionJobs
	}
	if maxPages == 0 {
		maxPages = DefaultRetentionPages
	}
	if maxJobs < 0 || maxJobs > MaxRetentionJobs {
		return 0, 0, badRequest("the number of jobs must be between 1 and %d", MaxRetentionJobs)
	}
	if maxPages < 0 || maxPages > MaxRetentionPages {
		return 0, 0, badRequest("the number of pages must be between 1 and %d", MaxRetentionPages)
	}
	return maxJobs, maxPages, nil
}

// The conditions that make a job one whose content may be cleared. They are written ONCE and used by the select of the candidates, by every page and by the mark: an item can only be changed through a job
// that is terminal, older than the dial and not marked yet, and the same words are in the partial index jobs_content_retention_idx (platform_0018).
const retentionDue = `j.content_cleared_at IS NULL AND j.status IN ('succeeded', 'failed', 'cancelled', 'expired') AND jsonb_exists(j.frozen_settings, 'content')
	AND j.finished_at IS NOT NULL AND j.finished_at <= now() - make_interval(secs => $%d::double precision)`

// ClearRetainedContent clears the rendered content of every ended send job whose finished_at is older than after (by the database's clock) and that is not cleared yet, at most maxJobs jobs and at most maxPages
// pages in all (0 means the defaults), the job that ended longest ago first, and marks each job it finishes. It is idempotent and safe to run in two instances at once (the pages are the same statements, a row
// already cleared is skipped, the mark is set once and the second writer changes nothing). A job whose pages did not all fit in this call stays unmarked: the next call reads it again and goes on. The first
// database error is returned beside the result and the call goes on to the next job. An invalid dial or bound is ErrBadRequest and nothing is read.
func (s *Store) ClearRetainedContent(ctx context.Context, after time.Duration, maxJobs, maxPages int) (RetentionResult, error) {
	if err := checkRetention(after); err != nil {
		return RetentionResult{}, opErr("content retention", err)
	}
	maxJobs, maxPages, err := retentionBounds(maxJobs, maxPages)
	if err != nil {
		return RetentionResult{}, opErr("content retention", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT j.id::text FROM platform.jobs j WHERE `+fmt.Sprintf(retentionDue, 1)+` ORDER BY j.finished_at, j.id LIMIT $2`, after.Seconds(), maxJobs)
	if err != nil {
		return RetentionResult{}, opErr("content retention", s.fail(err))
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return RetentionResult{}, opErr("content retention", s.fail(err))
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RetentionResult{}, opErr("content retention", s.fail(err))
	}

	res := RetentionResult{Found: len(ids)}
	var first error
	pages := maxPages
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			if first == nil {
				first = err
			}
			break
		}
		if pages <= 0 {
			res.Unfinished++
			continue
		}
		done, marked, err := s.clearJobContent(ctx, id, after, &pages, &res)
		switch {
		case err != nil:
			res.Failed++
			if first == nil {
				first = opErr("content retention", err)
			}
		case done && marked:
			res.Cleared++
		case done:
			res.Skipped++
		default:
			res.Unfinished++
		}
	}
	return res, first
}

// clearJobContent clears one job's pages while the budget lasts and marks the job after the last one. done says every page was written; marked says THIS call set the mark (another call may have set it first, or
// the job stopped being due: the statement of the mark holds the job's conditions, so a job that is not due is never marked). pages is the call's remaining budget and res its running totals.
//
// A PAGE IS THE FIRST rowBatch ITEMS OF THE JOB, BY ID, THAT STILL HOLD CONTENT. There is no cursor to remember: what is left to do is written in the rows themselves (an item that holds the placeholder and no text and
// no link no longer matches), so a call that stops at its page budget, a crash and a second instance all find the same thing, the next page, and a job needs as many pages in all as it has rows over rowBatch
// whatever the budget of one call. A page that changes fewer than rowBatch items is the last. The statement names the job and its owner and holds the job's own conditions again (retentionDue), so a caller
// that names a job that has not ended, or an item that belongs to no job, changes nothing.
func (s *Store) clearJobContent(ctx context.Context, jobID string, after time.Duration, pages *int, res *RetentionResult) (done, marked bool, err error) {
	for {
		if *pages <= 0 {
			return false, false, nil
		}
		tag, xerr := s.pool.Exec(ctx, `WITH due AS (SELECT j.id, j.user_id FROM platform.jobs j WHERE j.id = $1::uuid AND `+fmt.Sprintf(retentionDue, 2)+`),
			page AS (SELECT i.id FROM public.job_items i JOIN due ON i.campaign_id = due.id AND i.user_id = due.user_id
			         WHERE i.rendered_subject IS DISTINCT FROM $3 OR i.rendered_body IS DISTINCT FROM $3 OR i.rendered_text IS NOT NULL OR i.unsubscribe_url IS NOT NULL
			         ORDER BY i.id LIMIT $4)
			UPDATE public.job_items i SET rendered_subject = $3, rendered_body = $3, rendered_text = NULL, unsubscribe_url = NULL FROM page WHERE i.id = page.id`,
			jobID, after.Seconds(), ContentPlaceholder, rowBatch)
		if xerr != nil {
			return false, false, s.fail(xerr)
		}
		res.Items += tag.RowsAffected()
		res.Pages++
		*pages--
		if tag.RowsAffected() < rowBatch {
			break // the last page: fewer items than a page still held content
		}
	}
	// the mark: after the last page, once. A second writer (another instance, a repeat) changes nothing; the CHECK and the trigger of platform_0018 hold the rest.
	tag, merr := s.pool.Exec(ctx, `UPDATE platform.jobs j SET content_cleared_at = now() WHERE j.id = $1::uuid AND `+fmt.Sprintf(retentionDue, 2), jobID, after.Seconds())
	if merr != nil {
		return false, false, s.fail(merr)
	}
	return true, tag.RowsAffected() == 1, nil
}
