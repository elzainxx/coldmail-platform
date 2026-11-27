package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CONTENT RETENTION (Phase 5 S8, Docs/phase-5-spec.md decisions 7 and 15). The platform keeps what a user uploaded and wrote for a job: the message it rendered for every recipient of a SEND job (public.job_items, because
// that is where modules 2 and 3 read it), the campaign template (platform.jobs.frozen_settings, content) and the merge data of every uploaded row (platform.job_rows.fields). Once the job has ended and
// PLATFORM_CONTENT_RETENTION_DAYS have passed, the platform has no reason to hold any of it: this file clears it.
//
// WHAT IS CLEARED, for every ended job: the merge data of its rows (fields becomes {}) and its campaign template (the content's subject and html become the placeholder ContentPlaceholder, its text becomes empty);
// and, for a SEND job, the rendered messages of its items (rendered_subject and rendered_body become the placeholder: the modules' columns are NOT NULL and their pre-flight accepts a non-blank string;
// rendered_text and unsubscribe_url become NULL).
//
// WHAT IS KEPT FOR EVER: the recipient address (email_raw and email_norm of the rows, email of the results, recipient_email of the items), the per-item outcome (the results), the charge (the ledger), the job's dates and
// counts (the job row, its preview), the events, and what is not content: reply_to (an address), the settings, the row's state and its reason (a word, or the NAMES of the fields a row lacked, never a value), the status marker of an item.
//
// THE DATABASE HOLDS THE RULE, not only this file (platform_0021): platform.job_rows_frozen() lets the merge data of a row of an ended job change to exactly {} and nothing else beside it, platform.jobs_guard() lets the
// frozen template of an ended job change to exactly its cleared form and nothing else of the frozen document, the preview or the confirm key, and the lifecycle's CHECK and trigger on content_cleared_at (platform_0018) keep
// the mark to an ended job, never before its end, once. platform_app gets no grant for it: it already could update these columns, and the guards are what bind it.
//
// HOW IT RUNS. One job at a time, in pages of rowBatch rows, each page ITS OWN short statement and transaction, so a job of 100,000 rows is never one long lock. The order is: (1) the rendered items of a send job, a page of
// items still holding content at a time; (2) the merge data, a page of rows still holding any at a time, the LAST of which, in the same transaction, also clears the template and sets the mark platform.jobs.content_cleared_at.
// What is left to do is read from the rows themselves (an item that holds the placeholder and no text and no link, a row whose fields are {} no longer match), never from a memory of the purge: a call that stops at its page
// budget, a crash, a second instance all find the same next page, so a job needs as many calls as it has pages whatever the budget of one call, and the purge selects only jobs without the mark, so it is idempotent and cheap.
// A page that changes nothing is free (it does not use the call's budget: a job whose items were cleared by an earlier call goes straight to its rows).
//
// THE CLOCK IS THE DATABASE'S: "older than the dial" is finished_at <= now() - interval in SQL.

// ContentPlaceholder is what rendered_subject, rendered_body and the template's subject and html hold after the purge.
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

// RetentionResult is what one call did. Found is how many ended jobs were due; Cleared how many it finished and marked; Skipped how many it finished but another call had marked first (or that stopped being due: nothing of
// its own to say); Unfinished how many it left for the next call because its page budget ran out; Pages how many pages it wrote; Items how many rows of public.job_items and Rows how many rows of platform.job_rows it
// changed; Failed how many jobs a database error stopped (the first error is returned beside the result).
type RetentionResult struct {
	Found      int
	Cleared    int
	Skipped    int
	Unfinished int
	Failed     int
	Pages      int
	Items      int64
	Rows       int64
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

// The conditions that make a job one whose content may be cleared: it has ENDED, long enough ago, and is not marked yet. They are written ONCE and used by the select of the candidates, by every page and by the mark: a
// row or an item can only be changed through a job that is terminal, older than the dial and not marked, and the same words are in the partial index jobs_content_retention_idx (platform_0021).
const retentionDue = `j.content_cleared_at IS NULL AND j.status IN ('succeeded', 'failed', 'cancelled', 'expired')
	AND j.finished_at IS NOT NULL AND j.finished_at <= now() - make_interval(secs => $%d::double precision)`

// templateCleared is the frozen document of the job with its template cleared: the content's subject and html the placeholder and its text empty, everything else as it is (reply_to, the settings; a job that has no
// content is returned unchanged: jsonb_set does nothing when the path is not there). It is the ONE value platform.jobs_guard() lets frozen_settings of an ended job become (platform_0021 holds the same expression),
// and a test holds the two texts and ContentPlaceholder together.
const templateCleared = `jsonb_set(jsonb_set(jsonb_set(j.frozen_settings, '{content,subject}', '"[removed]"'::jsonb), '{content,html}', '"[removed]"'::jsonb), '{content,text}', '""'::jsonb)`

// ClearRetainedContent clears the rendered messages, the template and the merge data of every ended job whose finished_at is older than after (by the database's clock) and that is not cleared yet, at most maxJobs jobs
// and at most maxPages pages in all (0 means the defaults), the job that ended longest ago first, and marks each job it finishes. It is idempotent and safe to run in two instances at once (the pages are the same
// statements, what is already cleared is skipped, the mark is set once and the second writer changes nothing). A job whose pages did not all fit in this call stays unmarked: the next call reads it again and goes on. The
// first database error is returned beside the result and the call goes on to the next job. An invalid dial or bound is ErrBadRequest and nothing is read.
func (s *Store) ClearRetainedContent(ctx context.Context, after time.Duration, maxJobs, maxPages int) (RetentionResult, error) {
	if err := checkRetention(after); err != nil {
		return RetentionResult{}, opErr("content retention", err)
	}
	maxJobs, maxPages, err := retentionBounds(maxJobs, maxPages)
	if err != nil {
		return RetentionResult{}, opErr("content retention", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT j.id::text, jsonb_exists(j.frozen_settings, 'content') FROM platform.jobs j WHERE `+fmt.Sprintf(retentionDue, 1)+` ORDER BY j.finished_at, j.id LIMIT $2`, after.Seconds(), maxJobs)
	if err != nil {
		return RetentionResult{}, opErr("content retention", s.fail(err))
	}
	type candidate struct {
		id         string
		hasContent bool // a send job: it carries a campaign, so its items hold rendered messages
	}
	var due []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.hasContent); err != nil {
			rows.Close()
			return RetentionResult{}, opErr("content retention", s.fail(err))
		}
		due = append(due, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RetentionResult{}, opErr("content retention", s.fail(err))
	}

	res := RetentionResult{Found: len(due)}
	var first error
	pages := maxPages
	for _, c := range due {
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
		done, marked, err := s.clearJobContent(ctx, c.id, c.hasContent, after, &pages, &res)
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

// clearJobContent clears one job while the budget lasts and marks it with the last page. done says every page was written; marked says THIS call set the mark (another call may have set it first, or the job stopped
// being due: the statements hold the job's conditions, so a job that is not due is never changed). pages is the call's remaining budget and res its running totals.
//
// A PAGE IS THE FIRST rowBatch ROWS, BY ID, THAT STILL HOLD CONTENT (see the header): no cursor, nothing remembered. Every statement names the job and holds the job's own conditions again (retentionDue), so a caller that
// names a job that has not ended, or an item that belongs to no job, changes nothing.
func (s *Store) clearJobContent(ctx context.Context, jobID string, hasContent bool, after time.Duration, pages *int, res *RetentionResult) (done, marked bool, err error) {
	// (1) the rendered messages of a send job
	for hasContent {
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
		if n := tag.RowsAffected(); n > 0 { // a page that changed nothing is free
			res.Items += n
			res.Pages++
			*pages--
		}
		if tag.RowsAffected() < rowBatch {
			break // no full page of items is left
		}
	}
	// (2) the merge data of the rows, a page at a time; the page that is the last one also clears the template and sets the mark, in ITS transaction
	for {
		if *pages <= 0 {
			return false, false, nil
		}
		var n int64
		var last bool
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			tag, xerr := tx.Exec(ctx, `WITH due AS (SELECT j.id FROM platform.jobs j WHERE j.id = $1::uuid AND `+fmt.Sprintf(retentionDue, 2)+`),
				page AS (SELECT w.id FROM platform.job_rows w JOIN due ON w.job_id = due.id WHERE w.fields <> '{}'::jsonb ORDER BY w.id LIMIT $3)
				UPDATE platform.job_rows w SET fields = '{}'::jsonb FROM page WHERE w.id = page.id`, jobID, after.Seconds(), rowBatch)
			if xerr != nil {
				return s.fail(xerr)
			}
			n = tag.RowsAffected()
			if n >= rowBatch {
				return nil // a full page: there may be more
			}
			last = true
			mark, xerr := tx.Exec(ctx, `UPDATE platform.jobs j SET frozen_settings = `+templateCleared+`, content_cleared_at = now() WHERE j.id = $1::uuid AND `+fmt.Sprintf(retentionDue, 2), jobID, after.Seconds())
			if xerr != nil {
				return s.fail(xerr)
			}
			marked = mark.RowsAffected() == 1
			return nil
		})
		if err != nil {
			return false, false, err
		}
		res.Rows += n
		res.Pages++ // every page here counts: a full one changed rows, and the last one ends the job
		*pages--
		if last {
			return true, marked, nil
		}
	}
}
