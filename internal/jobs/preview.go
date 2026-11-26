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
// place; Confirm is what applies the list), and for a chained send the source job's verdict counts, how many of them are `unknown` (and so need the user's choice) and how many the choice
// selects. For a metered module (m1, m2) the credits a confirm would hold: the units that would reach the module times the dearest REACHABLE unit's price (the very rate ReserveIn freezes), the
// user's balance and the shortfall. For a SEND job the units are the pending rows the suppression list does not hold; for a VERIFY job they are every pending row: a suppressed address is
// still verified and charged, and the preview says so (SuppressedStillVerified). Module 3 holds no credits (Metered false). A send list that suppression leaves empty is a valid preview (the
// numbers are the answer); Confirm refuses it.
//
// PreviewUnknownChoice records the user's choice for the `unknown` verdicts of a chained send BEFORE the preview is computed: the job's frozen settings change and its unknown rows are
// reclassified (pending for include, excluded for exclude), both only while the job is draft or previewed, so the answer is the snapshot of the list under the choice, with the fingerprint that
// names it. On a job that is not chained, or with a value that is neither include nor exclude, it is a bad request and nothing is written.
func (s *Store) Preview(ctx context.Context, userID, jobID string, opts ...PreviewOption) (Preview, error) {
	choice, choiceGiven := PreviewChoice(opts...)
	if err := checkUser(userID); err != nil {
		return Preview{}, opErr("preview", err)
	}
	if !isUUID(jobID) {
		return Preview{}, opErr("preview", ErrNotFound)
	}
	if choiceGiven && !validChoice(choice) {
		return Preview{}, opErr("preview", badRequest("unknown_choice must be %q or %q", UnknownInclude, UnknownExclude))
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
		if choiceGiven {
			if j.SourceJobID == "" {
				return badRequest("unknown_choice only applies to a send chained to a verify job")
			}
			if err := s.applyUnknownChoice(ctx, tx, j, choice); err != nil {
				return err
			}
			j.Settings.UnknownChoice = choice
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

// applyUnknownChoice records the user's choice in the job's frozen settings and reclassifies the rows of the chained send whose source verdict is `unknown`, in the caller's transaction (the job is
// locked and is draft or previewed, the only states in which the settings and the rows may change: the database refuses both otherwise). include turns the unknown rows that are excluded into pending
// rows; exclude turns the pending ones back into excluded rows with the verdict as the reason, exactly the states CreateDraft gives them. Every statement is scoped by the user and by the job's source
// job, and idempotent: the same choice again changes nothing. A source job's module results are distinct addresses (one per pending row of a verify job, which the unique pending-address index makes
// distinct), so a row that becomes pending never collides with another pending row of the job.
func (s *Store) applyUnknownChoice(ctx context.Context, tx pgx.Tx, j Job, choice string) error {
	if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET frozen_settings = jsonb_set(frozen_settings, '{settings,unknown_choice}', to_jsonb($3::text), true)
		WHERE id = $1::uuid AND user_id = $2::uuid`, j.ID, j.UserID, choice); err != nil {
		return s.fail(err)
	}
	const unknownOfTheSource = `w.source_item IN (SELECT r.item_ref FROM platform.job_results r WHERE r.job_id = $3::uuid AND r.user_id = $2::uuid AND r.origin = 'module' AND r.module_state = 'unknown')`
	if choice == UnknownInclude {
		if _, err := tx.Exec(ctx, `UPDATE platform.job_rows w SET state = 'pending', reason = NULL WHERE w.job_id = $1::uuid AND w.user_id = $2::uuid AND w.state = 'excluded' AND `+unknownOfTheSource,
			j.ID, j.UserID, j.SourceJobID); err != nil {
			return s.fail(err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.job_rows w SET state = 'excluded', reason = $4::text WHERE w.job_id = $1::uuid AND w.user_id = $2::uuid AND w.state = 'pending' AND `+unknownOfTheSource,
		j.ID, j.UserID, j.SourceJobID, excludedReason(VerdictUnknown)); err != nil {
		return s.fail(err)
	}
	return nil
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

// buildPreview is the pure part: the snapshot, the choice recorded on the job, the number of pending rows the suppression list holds and the balance, as the Preview the user sees.
func buildPreview(sn *snapshot, unknownChoice string, onSuppressionList int, bal money.Balance, at time.Time) Preview {
	p := Preview{
		Rows:                    sn.rows,
		Pending:                 sn.pending + sn.suppressed,
		Malformed:               sn.malformed,
		MalformedReasons:        sn.reasons,
		Duplicates:              sn.duplicates,
		Excluded:                sn.excluded,
		Suppressed:              sn.suppressed + onSuppressionList,
		SuppressedStillVerified: sn.module == ModuleVerify,
		MissingFields:           0,
		Verdicts:                sn.verdicts,
		Selected:                sn.selected,
		UnknownChoice:           unknownChoice,
		UnknownCount:            sn.verdicts[VerdictUnknown], // a nil map reads as zero
		Metered:                 sn.metered,
		Fingerprint:             sn.fingerprint,
		ComputedAt:              at,
	}
	p.UnknownChoiceRequired = sn.verdicts != nil && p.UnknownCount > 0
	if p.MalformedReasons == nil {
		p.MalformedReasons = map[string]int{}
	}
	// a send job leaves its suppressed rows out; a verify job verifies and charges them like any other row (the results flag them)
	p.Units = p.Pending - p.Suppressed
	if p.SuppressedStillVerified {
		p.Units = p.Pending
	}
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
