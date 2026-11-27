package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The ErrNotAllowed reasons of the store's own (a refusal of the database's carries its constraint's name instead).
const (
	reasonNotADraft      = "not_a_draft"                           // a discard is for a draft or a previewed job: a confirmed job is cancelled by Cancel, which releases its reserve
	reasonAlreadyDraft   = "already_a_draft"                       // nothing to reopen
	reasonNotConfirmed   = "not_confirmed"                         // Cancel of a job that holds nothing yet: discard it
	reasonNotPaused      = "not_paused"                            // a resume of a job that is not paused
	reasonDeadlinePassed = "start_deadline_passed"                 // a paused job cannot resume after its start deadline: the engine expires it
	reasonNeedsTheEngine = "submitted_job_ends_through_the_engine" // the API's word for a job the module holds (paused while running) that somebody tried to END directly: the database's wall is job_submitted_pause_cancels_through_the_engine (see internal/api notAllowedWords); Cancel itself now takes such a job to cancelling, through the engine
)

// ReasonSubmittedJobCancelsThroughTheEngine is the constraint name of the database's wall behind reasonNeedsTheEngine (platform_0016): a job paused while running goes to cancelling, never straight to cancelled.
const ReasonSubmittedJobCancelsThroughTheEngine = "job_submitted_pause_cancels_through_the_engine"

// lockedMove runs fn in ONE transaction on the user's job, locked FOR UPDATE (ErrNotFound when it is not theirs), and returns the job as it is afterwards. withMoney takes the (user,
// module) advisory lock first, as every money writer of that pair does: a move that releases credits must.
func (s *Store) lockedMove(ctx context.Context, op, userID, jobID string, withMoney bool, fn func(tx pgx.Tx, j Job) error) (Job, error) {
	if err := checkUser(userID); err != nil {
		return Job{}, opErr(op, err)
	}
	if !isUUID(jobID) {
		return Job{}, opErr(op, ErrNotFound)
	}
	var out Job
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if withMoney {
			var module string
			if err := tx.QueryRow(ctx, `SELECT module_id FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID).Scan(&module); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return s.fail(err)
			}
			if err := s.mny.LockIn(ctx, tx, userID, module); err != nil {
				return err
			}
		}
		j, err := s.getJob(ctx, tx, userID, jobID, true)
		if err != nil {
			return err
		}
		if err := fn(tx, j); err != nil {
			return err
		}
		out, err = s.getJob(ctx, tx, userID, jobID, false)
		return err
	})
	if err != nil {
		s.log.Info("job move refused", "op", op, "job", jobID, "reason", refusalWord(err))
		return Job{}, opErr(op, err)
	}
	s.log.Info("job moved", "op", op, "job", out.ID, "status", out.Status)
	return out, nil
}

// DiscardDraft cancels a draft or a previewed job (actor: the user, reason "discarded by the user"). A draft and a previewed job hold nothing: no credits, no slot. Any other status is
// ErrNotAllowed (a confirmed job is cancelled by CancelBeforeStart, which releases what it holds).
func (s *Store) DiscardDraft(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "discard", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		if j.Status != StatusDraft && j.Status != StatusPreviewed {
			return notAllowed(reasonNotADraft)
		}
		return s.move(ctx, tx, userID, jobID, StatusCancelled, "discarded by the user")
	})
}

// Reopen takes a previewed job back to draft (actor: the user): the list or the settings are about to change, so the preview no longer describes the job and its columns are cleared. A
// job that is already a draft is ErrNotAllowed; any other status is refused by the lifecycle (job_transition_illegal).
func (s *Store) Reopen(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "reopen", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		if j.Status == StatusDraft {
			return notAllowed(reasonAlreadyDraft)
		}
		// the move FIRST and alone, so that for a job that is not previewed the refusal is the lifecycle's (job_transition_illegal), not the frozen-column guard's; then, the job being a draft, the
		// preview columns it no longer has a right to are cleared
		if err := s.move(ctx, tx, userID, jobID, StatusDraft, "reopened by the user: the preview no longer describes the job"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET preview = NULL, preview_fingerprint = NULL, previewed_at = NULL WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID); err != nil {
			return s.fail(err)
		}
		return nil
	})
}

// Cancel is THE user's cancel: the move the job's state calls for, in ONE transaction (the advisory lock first, then the job row), and the database decides what is legal and whether the module can do
// it. The store picks the move; it does not decide:
//   - confirmed, scheduled, or paused from SCHEDULED (nothing was submitted): the move to cancelled (actor: the user), the platform's result `cancelled` (rule P-01, free) for every row that has no
//     result yet, the terminal marker (public.job_items.status 'cancelled') on the items of the rows no module was ever given (markNeverSubmittedItems), and, for a metered module, the release of the
//     whole reserve (money.ReleaseIn): the credits come back, the slot is free;
//   - running, or paused from RUNNING (the module holds items): the move to cancelling only, which records the request (cancel_requested_at, the event with its actor and reason). Telling the module,
//     recording what finished, ending the job and releasing the remainder is the ENGINE's: a module 2 cancel also releases the pause, a module 3 cancel of a paused campaign cancels its unclaimed
//     items, and every item in flight finishes and is reported once. The reserve and the slot stay held until then. The refusal of a module that cannot cancel (platform.modules.can_cancel) is the
//     database's (ErrNotAllowed, Reason job_module_cannot_cancel; the API words it cancel_not_supported): no seeded module is refused any more.
//
// A draft or previewed job is DiscardDraft's (ErrNotAllowed), and every other status is refused by the lifecycle (job_transition_illegal, job_transition_actor_not_allowed, jobs_terminal_is_final).
// A job paused while running is NEVER taken to cancelled directly: the database refuses that pair too (job_submitted_pause_cancels_through_the_engine, which the API words
// submitted_job_ends_through_the_engine), so a writer that tries it, the admin surface included, is stopped where every writer passes.
func (s *Store) Cancel(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "cancel", userID, jobID, true, func(tx pgx.Tx, j Job) error {
		switch {
		case j.Status == StatusDraft || j.Status == StatusPreviewed:
			return notAllowed(reasonNotConfirmed)
		case j.Status == StatusRunning:
			return s.move(ctx, tx, userID, jobID, StatusCancelling, "cancel requested by the user")
		case j.Status == StatusPaused && j.PausedFrom == StatusRunning:
			return s.move(ctx, tx, userID, jobID, StatusCancelling, "cancel requested by the user while the job was paused: the module is told and releases what it holds")
		}
		if err := s.move(ctx, tx, userID, jobID, StatusCancelled, "cancelled by the user before it started"); err != nil {
			return err
		}
		rule, err := resultRule("cancelled")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, reason, decision, rule)
			SELECT r.job_id, r.user_id, r.id, r.ordinal, COALESCE(r.email_norm, r.email_raw), 'platform', 'cancelled', NULL, 'free', $3::text
			FROM platform.job_rows r
			WHERE r.job_id = $1::uuid AND r.user_id = $2::uuid
			  AND NOT EXISTS (SELECT 1 FROM platform.job_results x WHERE x.job_id = r.job_id AND x.item_ref = r.id)`, jobID, userID, rule); err != nil {
			return s.fail(err)
		}
		if _, err := s.markNeverSubmittedItems(ctx, tx, jobID); err != nil { // the job ended without sending: the items of its rows get the terminal marker (D9)
			return err
		}
		var metered bool
		if err := tx.QueryRow(ctx, `SELECT metered FROM platform.modules WHERE id = $1`, j.Module).Scan(&metered); err != nil {
			return s.fail(err)
		}
		if metered {
			if _, err := s.mny.ReleaseIn(ctx, tx, jobID); err != nil {
				return err
			}
		}
		return nil
	})
}

// CancelBeforeStart is Cancel under its older name (every caller before the module routes of the close-out used it): the same move, the same refusals.
func (s *Store) CancelBeforeStart(ctx context.Context, userID, jobID string) (Job, error) {
	return s.Cancel(ctx, userID, jobID)
}

// Pause holds a job (actor: the user): the move to paused is only ATTEMPTED for every status, and the DATABASE decides (platform.job_transitions, platform.modules.can_pause):
//   - a scheduled job: its start is deferred until the user resumes it; nothing was submitted, the slot and the reserve are held;
//   - a RUNNING job of a module that can pause one (modules 2 and 3): the job is paused the moment this returns, with paused_from = running. The store tells the module nothing: the engine does, at its
//     next tick (Module.Pause until the module's own report says paused), so the module stops claiming within one engine tick of this call, and the items it had already claimed finish and are reported
//     once. Nothing is submitted to the module while the job is paused. The reserve and the slot stay held until the user resumes or cancels it or, after PLATFORM_JOB_PAUSE_MAX_DAYS
//     (Phase 5 S7: pause_limit.go, by the database's clock from paused_at), the engine cancels it;
//   - a RUNNING job of a module that cannot (module 1): refused as ErrNotAllowed, Reason job_module_cannot_pause (the API words it pause_not_supported). The store does not fake a pause by cancel and
//     resubmit (a duplicate-send risk), and no Go branch decides which module may: the capability is a row.
//
// Every other status is refused by the lifecycle (job_transition_illegal).
func (s *Store) Pause(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "pause", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		return s.move(ctx, tx, userID, jobID, StatusPaused, "paused by the user")
	})
}

// PauseScheduled is Pause under its older name (when only a scheduled job could be paused): the same move, the same refusals.
func (s *Store) PauseScheduled(ctx context.Context, userID, jobID string) (Job, error) {
	return s.Pause(ctx, userID, jobID)
}

// Resume lets a paused job go on (actor: the user), to the status it paused FROM: scheduled (the engine starts it at its time) or running (the engine tells the module to resume at its next tick; until
// then the module still holds the campaign, which is why the engine, not the store, resumes it). It is refused as ErrNotAllowed when the job is not paused (not_paused) and, for a job paused
// before its start, when its start deadline has passed by the database's clock (start_deadline_passed: the engine expires such a job and releases its reserve). A job paused while running has no start
// deadline to pass: it never expires. The target is read from the row, and the database holds the rule that a pause resumes only where it paused from (job_resume_to_where_it_paused_from).
func (s *Store) Resume(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "resume", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		if j.Status != StatusPaused {
			return notAllowed(reasonNotPaused)
		}
		if j.PausedFrom == StatusScheduled {
			var passed bool
			if err := tx.QueryRow(ctx, `SELECT start_deadline IS NOT NULL AND start_deadline <= now() FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID).Scan(&passed); err != nil {
				return s.fail(err)
			}
			if passed {
				return notAllowed(reasonDeadlinePassed)
			}
		}
		return s.move(ctx, tx, userID, jobID, j.PausedFrom, "resumed by the user")
	})
}

// ResumeScheduled is Resume under its older name (when only a job paused before its start could be resumed): the same move, the same refusals.
func (s *Store) ResumeScheduled(ctx context.Context, userID, jobID string) (Job, error) {
	return s.Resume(ctx, userID, jobID)
}
