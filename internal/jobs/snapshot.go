package jobs

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// snapshot is everything a preview (and the confirm that checks it) reads about a job in one pass: the counts of its rows, the pending addresses the suppression list is asked about, the
// digest of the list, the source job's verdicts, the reserve rate and the fingerprint over all of it. Preview and Confirm both build it with takeSnapshot, so they compute one thing.
type snapshot struct {
	module  string // the job's module (m1 for a verify job: its suppressed rows stay in the job)
	metered bool

	rows       int
	pending    int // rows in state pending
	suppressed int // rows already in state suppressed (never before confirm; read as pending by the fingerprint)
	skipped    int // rows already in state missing_field or unrenderable (never before confirm; read as pending by the fingerprint)
	malformed  int
	duplicates int
	excluded   int
	reasons    map[string]int // malformed rows by reason word

	pendingEmails []string // the normalised addresses of the pending rows, for the suppression list

	verdicts map[string]int // the source job's module results by verdict (all four keys); nil when the job is not chained
	selected int

	quote money.RateQuote // zero for a module that is not metered

	// tpl is what the pass over the templates found (template_scan.go): set by computePreview for a send job in a store with a template renderer, zero otherwise. buildPreview reads it; it is not part of the
	// fingerprint (it is a function of what the fingerprint already names: the rows, their merge data and the campaign).
	tpl templateScan

	fingerprint string
}

// takeSnapshot reads the job's list, its source and its prices in the caller's transaction and computes the fingerprint. lockPrices takes the module's price rows FOR SHARE first (the confirm
// does: a price that changes after the comparison waits for the reserve, so the reserve is made at the prices that were compared). The job must be the user's (the caller read it).
func (s *Store) takeSnapshot(ctx context.Context, tx pgx.Tx, j Job, lockPrices bool) (*snapshot, error) {
	sn := &snapshot{module: j.Module, reasons: map[string]int{}}

	if err := tx.QueryRow(ctx, `SELECT metered FROM platform.modules WHERE id = $1`, j.Module).Scan(&sn.metered); err != nil {
		return nil, s.fail(err)
	}
	if sn.metered {
		// Phase 5 S6: the quote is over the units THIS job can produce: an opted-out job's leaves m1_vendor out, the very set ReserveIn freezes (the prices map is in the fingerprint, so the rate a preview shows is the rate a confirm freezes)
		q, err := s.mny.ReserveRateForIn(ctx, tx, j.Module, lockPrices, j.Settings.VendorOptOut)
		if err != nil {
			return nil, err
		}
		sn.quote = q
	}

	// the list: counts, the digest of every row and the addresses to ask the suppression list about, in ONE ordered pass
	rowsDigest, fieldsDigest := newLineDigest(), newLineDigest()
	// fh is the digest of the row's merge data, computed by the database (jsonb text is canonical) so that the data never travels: '' for a row with none. It names the data in the fingerprint of a SEND job, whose
	// templates (and so its missing-field count) depend on it.
	rows, err := tx.Query(ctx, `SELECT ordinal, state, email_norm, reason, CASE WHEN fields = '{}'::jsonb THEN '' ELSE md5(fields::text) END FROM platform.job_rows WHERE job_id = $1::uuid AND user_id = $2::uuid ORDER BY ordinal`, j.ID, j.UserID)
	if err != nil {
		return nil, s.fail(err)
	}
	for rows.Next() {
		var ordinal int32
		var state string
		var emailNorm, reason *string
		var fh string
		if err := rows.Scan(&ordinal, &state, &emailNorm, &reason, &fh); err != nil {
			rows.Close()
			return nil, s.fail(err)
		}
		en, rs := deref(emailNorm), deref(reason)
		rowsDigest.rowLine(int(ordinal), state, en, rs)
		if fh != "" && j.Content != nil {
			fieldsDigest.add(strconv.Itoa(int(ordinal)), fh)
		}
		sn.rows++
		switch state {
		case StatePending:
			sn.pending++
			sn.pendingEmails = append(sn.pendingEmails, en)
		case StateSuppressed:
			sn.suppressed++
		case StateMalformed:
			sn.malformed++
			sn.reasons[rs]++
		case StateDuplicate:
			sn.duplicates++
		case StateExcluded:
			sn.excluded++
		case StateMissingField, StateUnrenderable:
			// a row the confirm of a send job left out: it exists only after the confirm, which no preview or confirm reads again (the job is no longer previewed); counted as a skipped row, read as pending by the digest
			sn.skipped++
		default:
			rows.Close()
			return nil, fmt.Errorf("jobs: job %s holds a row in state %q, which this store does not know", j.ID, state)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(err)
	}
	if sn.rows == 0 {
		return nil, fmt.Errorf("jobs: job %s has no rows", j.ID) // a draft is never made without rows: a job like that was not made by this store
	}

	// the source job's module results: the digest of (item_ref, module_state) and the verdict counts
	sourceDigest := ""
	if j.SourceJobID != "" {
		d := newLineDigest()
		sn.verdicts = map[string]int{}
		for _, v := range Verdicts() {
			sn.verdicts[v] = 0
		}
		res, err := tx.Query(ctx, `SELECT item_ref::text, module_state FROM platform.job_results WHERE job_id = $1::uuid AND user_id = $2::uuid AND origin = 'module' ORDER BY item_ref`, j.SourceJobID, j.UserID)
		if err != nil {
			return nil, s.fail(err)
		}
		for res.Next() {
			var ref, verdict string
			if err := res.Scan(&ref, &verdict); err != nil {
				res.Close()
				return nil, s.fail(err)
			}
			d.add(ref, verdict)
			if _, known := sn.verdicts[verdict]; known {
				sn.verdicts[verdict]++
				if selects(verdict, j.Settings.UnknownChoice) {
					sn.selected++
				}
			}
		}
		if err := res.Err(); err != nil {
			return nil, s.fail(err)
		}
		sourceDigest = d.sum()
	}

	sn.fingerprint = fingerprintInput{
		JobID:         j.ID,
		Module:        j.Module,
		RowsDigest:    rowsDigest.sum(),
		ContentDigest: contentDigest(j.Content),
		UnknownChoice: j.Settings.UnknownChoice,
		SourceJobID:   j.SourceJobID,
		SourceDigest:  sourceDigest,
		FieldsDigest:  fieldsDigest.sum(),
		Prices:        sn.quote.Prices,
	}.fingerprint()
	return sn, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// suppressedAmong asks the user's suppression list which of the snapshot's pending addresses it holds. The answer is a set of addresses (the pending rows are distinct addresses: a repeat is a
// duplicate row, not a pending one).
func suppressedAmong(ctx context.Context, tx pgx.Tx, userID string, sn *snapshot) (map[string]bool, error) {
	return suppress.Filter(ctx, tx, userID, sn.pendingEmails)
}
