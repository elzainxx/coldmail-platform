package jobs

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// THE PASS OVER THE ROWS (Phase 5, chunk S2). A send job's rows are read in pages of rowBatch, in the order of their ordinal, and each row that is still pending is judged by the job's templates:
//
//   - MISSING a field the templates need and give no fallback for (PreparedCampaign.Missing): the row cannot be rendered. The preview COUNTS such rows and lists the names of the fields; the confirm
//     LEAVES THEM OUT: state missing_field, the field names as the reason, a free platform result (P-09), no public.job_items row, no place in the reserve.
//   - UNRENDERABLE: the row has everything but its message is one the shared table refuses (checkRendered: a blank subject after rendering, a part over MaxBodyBytes, no body, text that is not UTF-8). The row is
//     rendered ONCE, exactly as EnsureJobItems will render it (messageFor: same link, same seed), and a refusal leaves it out the same way: state unrenderable, the refusal's word as the reason (WordSubjectBlank,
//     ...), P-10. Without this pass the engine meets the refusal after the credits are held and fails the WHOLE job on one row (free for the customer, but the job is dead); with it the one row is skipped.
//     The cost is one render of every row inside the confirm's transaction, in pages, with no statement per row: a CPU cost of the order of the templates' size times the number of rows, no more database
//     work than the page reads the fingerprint already does (the rows' merge data is read once, in the same pages).
//
// The same pass serves the preview (it counts and writes nothing, and keeps the ordinals of the rows that would be sent, so that the samples can be chosen) and the confirm (it writes). The user's suppression list
// is applied FIRST, in both: a row the list holds is a suppressed row whatever it lacks, so the preview's counts are exactly what the confirm will do.

type scanOpts struct {
	skipAddr map[string]bool // the addresses the user's suppression list holds (the preview): their rows are not judged. The confirm has already turned them into suppressed rows, which the pass does not read
	apply    bool            // the confirm: write the state and the reason of the rows left out
	eligible bool            // the preview: keep the ordinals of the rows that would be sent
}

// templateScan is what the pass found.
type templateScan struct {
	missing        int      // pending rows that lack a required merge field
	names          []string // the names of those fields, sorted, at most MaxMissingFieldNames
	namesTruncated bool     // there were more names than that
	unrenderable   int      // the other pending rows whose message the shared table refuses
	eligible       []int32  // the ordinals of the rows that would be sent, in order (only when asked for)
}

// WordMissingField is the refusal word of a row that lacks a required merge field when it is rendered anyway (a job confirmed by a store with no template renderer and run by one that has it).
const WordMissingField = "missing_field"

// messageFor is THE way a pending row becomes a message: the row's unsubscribe link first ("" with no maker), then the prepared campaign, then the shared table's checks. EnsureJobItems writes the result,
// a preview's sample shows it and the confirm's pass tries it, all through here, so that what is shown, what is checked and what is written are one message. A row that still lacks a required field is
// refused (a message with a hole where a name should be is never sent); the refusal names the fields, never a value. A refusal of the message carries its word (refusalWordOf); any other error is the
// store's (the link maker failing) and is not a property of the row.
func (s *Store) messageFor(prep PreparedCampaign, replyTo string, row EngineRow) (EngineRow, renderedMessage, error) {
	l, err := s.linkOf(row.ID)
	if err != nil {
		return row, renderedMessage{}, fmt.Errorf("row %d: %w", row.Ordinal, err)
	}
	row.UnsubscribeURL = l
	if miss := prep.Missing(row); len(miss) > 0 {
		return row, renderedMessage{}, refuse(WordMissingField, "row %d: the row lacks the merge field(s) %s", row.Ordinal, missingReason(miss))
	}
	m, err := renderPrepared(prep, replyTo, row)
	return row, m, err
}

// skipRowsSQL leaves out the named pending rows of a job: their new state and their reason, written while the job is previewed (the rows' guard allows it) and only for rows that are still pending.
const skipRowsSQL = `UPDATE platform.job_rows w SET state = $3, reason = u.reason
	FROM unnest($4::text[], $5::text[]) AS u(id, reason)
	WHERE w.job_id = $1::uuid AND w.user_id = $2::uuid AND w.id = u.id::uuid AND w.state = 'pending'`

// scanTemplates runs the pass over the job's pending rows (see above). The caller holds the job row FOR UPDATE in tx. The job must be a send job with a prepared campaign.
func (s *Store) scanTemplates(ctx context.Context, tx pgx.Tx, j Job, prep PreparedCampaign, o scanOpts) (templateScan, error) {
	var res templateScan
	names := map[string]struct{}{}
	after := 0 // ordinals start at 1: the page is the rows after the last one read
	for {
		rows, err := tx.Query(ctx, `SELECT id::text, ordinal, email_norm, fields FROM platform.job_rows
			WHERE job_id = $1::uuid AND user_id = $2::uuid AND state = 'pending' AND ordinal > $3 ORDER BY ordinal LIMIT $4`, j.ID, j.UserID, after, rowBatch)
		if err != nil {
			return templateScan{}, s.fail(err)
		}
		page, err := s.scanEngineRows(rows)
		if err != nil {
			return templateScan{}, err
		}
		var missIDs, missWhy, badIDs, badWhy []string
		for _, row := range page {
			if o.skipAddr[row.EmailNorm] {
				continue
			}
			if miss := prep.Missing(row); len(miss) > 0 {
				res.missing++
				for _, n := range miss {
					names[n] = struct{}{}
				}
				missIDs, missWhy = append(missIDs, row.ID), append(missWhy, missingReason(miss))
				continue
			}
			if _, _, err := s.messageFor(prep, j.Content.ReplyTo, row); err != nil {
				word := refusalWordOf(err)
				if word == "" {
					return templateScan{}, err // not a property of the row (the link maker failed): the whole call fails
				}
				res.unrenderable++
				badIDs, badWhy = append(badIDs, row.ID), append(badWhy, word)
				continue
			}
			if o.eligible {
				res.eligible = append(res.eligible, int32(row.Ordinal))
			}
		}
		if o.apply {
			if err := s.skipRows(ctx, tx, j, StateMissingField, missIDs, missWhy); err != nil {
				return templateScan{}, err
			}
			if err := s.skipRows(ctx, tx, j, StateUnrenderable, badIDs, badWhy); err != nil {
				return templateScan{}, err
			}
		}
		if len(page) < rowBatch {
			break
		}
		after = page[len(page)-1].Ordinal
	}
	res.names = make([]string, 0, len(names))
	for n := range names {
		res.names = append(res.names, n)
	}
	sort.Strings(res.names)
	if len(res.names) > MaxMissingFieldNames {
		res.names, res.namesTruncated = res.names[:MaxMissingFieldNames], true
	}
	return res, nil
}

// skipRows moves the named pending rows of the job to state with their reasons. Every row must change: the job is locked and its rows are frozen from anyone else, so a row that does not is a bug.
func (s *Store) skipRows(ctx context.Context, tx pgx.Tx, j Job, state string, ids, reasons []string) error {
	if len(ids) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, skipRowsSQL, j.ID, j.UserID, state, ids, reasons)
	if err != nil {
		return s.fail(err)
	}
	if int(tag.RowsAffected()) != len(ids) {
		return fmt.Errorf("jobs: %d of %d rows of job %s could not be left out as %s", len(ids)-int(tag.RowsAffected()), len(ids), j.ID, state)
	}
	return nil
}

// pickSamples chooses which of the rows that would be sent are shown: the first, the middle and the last by ordinal (fewer when the list is shorter, never the same row twice), so that the samples
// are spread over the list and the same list always shows the same rows.
func pickSamples(eligible []int32) []int32 {
	n := len(eligible)
	if n == 0 {
		return nil
	}
	out := []int32{eligible[0]}
	for _, i := range []int{(n - 1) / 2, n - 1} {
		if eligible[i] != out[len(out)-1] {
			out = append(out, eligible[i])
		}
	}
	return out
}

// sampleMessages renders the rows at the given ordinals exactly as they will be sent (messageFor: the same link, the same seed, the same checks) and returns them in the order of their ordinal.
func (s *Store) sampleMessages(ctx context.Context, tx pgx.Tx, j Job, prep PreparedCampaign, ordinals []int32) ([]Sample, error) {
	if len(ordinals) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text, ordinal, email_norm, fields FROM platform.job_rows
		WHERE job_id = $1::uuid AND user_id = $2::uuid AND state = 'pending' AND ordinal = ANY($3::int[]) ORDER BY ordinal`, j.ID, j.UserID, ordinals)
	if err != nil {
		return nil, s.fail(err)
	}
	page, err := s.scanEngineRows(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(page))
	for _, row := range page {
		_, m, err := s.messageFor(prep, j.Content.ReplyTo, row)
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{Ordinal: row.Ordinal, Subject: m.subject, HTML: m.html, Text: m.text})
	}
	return out, nil
}
