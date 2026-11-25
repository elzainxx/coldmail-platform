package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/money"
)

// reasonPreviewStatus is the ErrNotAllowed reason of a preview of a job that is no longer draft or previewed.
const reasonPreviewStatus = "job_status_has_no_preview"

// Preview computes what the job's list will cost and the fingerprint of everything the numbers depend on, stores it (platform.jobs.preview, preview_fingerprint, previewed_at) and moves a
// draft to previewed (actor: the user, through the lifecycle's own guard, which writes the event). It is valid from draft and from previewed: a repeat recomputes and replaces the stored
// preview and moves nothing (no transition, no event). A job in any other status is ErrNotAllowed.
//
// The counts: rows, pending, malformed (with a count per reason), duplicates, excluded, suppressed (the pending rows whose address is on the user's suppression list: counted here, left in
// place; Confirm is what turns them into suppressed rows), and for a chained send the source job's verdict counts and how many the choice selects. For a metered module (m1, m2) the credits a
// confirm would hold: the units that would reach the module times the dearest REACHABLE unit's price (the very rate ReserveIn freezes), the user's balance and the shortfall. Module 3 holds no
// credits (Metered false). A list that suppression leaves empty is a valid preview (the numbers are the answer); Confirm refuses it.
func (s *Store) Preview(ctx context.Context, userID, jobID string) (Preview, error) {
	if err := checkUser(userID); err != nil {
		return Preview{}, opErr("preview", err)
	}
	if !isUUID(jobID) {
		return Preview{}, opErr("preview", ErrNotFound)
	}
	var out Preview
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		j, err := s.getJob(ctx, tx, userID, jobID, true)
		if err != nil {
			return err
		}
		if j.Status != StatusDraft && j.Status != StatusPreviewed {
			return notAllowed(reasonPreviewStatus)
		}
		p, err := s.computePreview(ctx, tx, j, false)
		if err != nil {
			return err
		}
		doc, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if j.Status == StatusDraft {
			// one statement moves the job and stores the preview: the guard reads the actor from the settings, the event trigger writes the event
			if err := s.setActor(ctx, tx, "user", userID, "preview requested by the user"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET status = 'previewed', preview = $3::jsonb, preview_fingerprint = $4, previewed_at = now()
				WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID, string(doc), p.Fingerprint); err != nil {
				return s.fail(err)
			}
			if err := s.clearActor(ctx, tx); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET preview = $3::jsonb, preview_fingerprint = $4, previewed_at = now()
				WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID, string(doc), p.Fingerprint); err != nil {
				return s.fail(err)
			}
		}
		out = p
		return nil
	})
	if err != nil {
		return Preview{}, opErr("preview", err)
	}
	return out, nil
}

// computePreview reads the job, applies the suppression list as a COUNT (nothing is written) and builds the preview. The job row is locked by the caller.
func (s *Store) computePreview(ctx context.Context, tx pgx.Tx, j Job, lockPrices bool) (Preview, error) {
	sn, err := s.takeSnapshot(ctx, tx, j, lockPrices)
	if err != nil {
		return Preview{}, err
	}
	found, err := suppressedAmong(ctx, tx, j.UserID, sn)
	if err != nil {
		return Preview{}, err
	}
	inList := 0
	for _, e := range sn.pendingEmails {
		if found[e] {
			inList++
		}
	}
	var bal money.Balance
	if sn.metered {
		if bal, err = s.mny.BalanceIn(ctx, tx, j.UserID, j.Module); err != nil {
			return Preview{}, err
		}
	}
	return buildPreview(sn, j.Settings.UnknownChoice, inList, bal, s.now().UTC()), nil
}

// buildPreview is the pure part: the snapshot, the number of pending rows the suppression list holds and the balance, as the Preview the user sees.
func buildPreview(sn *snapshot, unknownChoice string, onSuppressionList int, bal money.Balance, at time.Time) Preview {
	p := Preview{
		Rows:             sn.rows,
		Pending:          sn.pending + sn.suppressed,
		Malformed:        sn.malformed,
		MalformedReasons: sn.reasons,
		Duplicates:       sn.duplicates,
		Excluded:         sn.excluded,
		Suppressed:       sn.suppressed + onSuppressionList,
		MissingFields:    0,
		Verdicts:         sn.verdicts,
		Selected:         sn.selected,
		UnknownChoice:    unknownChoice,
		Metered:          sn.metered,
		Fingerprint:      sn.fingerprint,
		ComputedAt:       at,
	}
	if p.MalformedReasons == nil {
		p.MalformedReasons = map[string]int{}
	}
	p.Units = p.Pending - p.Suppressed
	if sn.metered {
		p.RateUnit, p.UnitCost = sn.quote.Unit, sn.quote.UnitCost
		p.Credits = int64(p.Units) * int64(p.UnitCost)
		p.Available = bal.Available
		if p.Credits > bal.Available {
			p.Shortfall = p.Credits - bal.Available
		}
	}
	return p
}
