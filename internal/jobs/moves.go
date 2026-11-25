package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The ErrNotAllowed reasons of the store's own (a refusal of the database's carries its constraint's name instead).
const (
	reasonNotADraft      = "not_a_draft"                           // a discard is for a draft or a previewed job: a confirmed job is cancelled by CancelBeforeStart, which releases its reserve
	reasonAlreadyDraft   = "already_a_draft"                       // nothing to reopen
	reasonNotConfirmed   = "not_confirmed"                         // CancelBeforeStart of a job that holds nothing yet: discard it
	reasonNotPaused      = "not_paused"                            // a resume of a job that is not paused
	reasonDeadlinePassed = "start_deadline_passed"                 // a paused job cannot resume after its start deadline: the engine expires it
	reasonNeedsTheEngine = "submitted_job_ends_through_the_engine" // a job paused from running was submitted to its module: only the engine can end it
)

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

// CancelBeforeStart cancels a job that holds credits and has not been submitted to its module: confirmed, scheduled, or paused from scheduled. In ONE transaction (the advisory lock first,
// then the job row): the move to cancelled (actor: the user), the platform's result `cancelled` (rule P-01, free) for every row that has no result yet, and, for a metered module, the release
// of the whole reserve (money.ReleaseIn): the credits come back, the slot is free.
//
// A RUNNING job is not this store's to cancel: the move is only ATTEMPTED (running -> cancelling, which records the request, sets cancel_requested_at and writes the event) and the DATABASE
// decides whether the module can do it: module 1 has no per-job cancel, so the refusal arrives as ErrNotAllowed with Reason job_module_cannot_cancel.
// Telling the module, finishing the job and releasing its reserve is the engine's. A draft or previewed job is DiscardDraft's (ErrNotAllowed), a job paused from running is the engine's
// (ErrNotAllowed), and every other status is refused by the lifecycle (job_transition_illegal, job_transition_actor_not_allowed, jobs_terminal_is_final).
func (s *Store) CancelBeforeStart(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "cancel", userID, jobID, true, func(tx pgx.Tx, j Job) error {
		switch j.Status {
		case StatusDraft, StatusPreviewed:
			return notAllowed(reasonNotConfirmed)
		case StatusRunning:
			return s.move(ctx, tx, userID, jobID, StatusCancelling, "cancel requested by the user")
		case StatusPaused:
			if j.PausedFrom != StatusScheduled {
				return notAllowed(reasonNeedsTheEngine)
			}
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

// PauseScheduled defers the start of a scheduled job until the user resumes it (actor: the user): nothing was submitted, the slot and the reserve are held. The move is only ATTEMPTED for
// every status: the database accepts a scheduled job, refuses a running job whose module has no per-job pause (ErrNotAllowed, Reason job_module_cannot_pause: no module has one today) and
// refuses every other status (job_transition_illegal). The store does not fake a pause by cancel and resubmit.
func (s *Store) PauseScheduled(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "pause", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		return s.move(ctx, tx, userID, jobID, StatusPaused, "paused by the user")
	})
}

// ResumeScheduled puts a paused job back to scheduled (actor: the user). It is refused as ErrNotAllowed when the job is not paused, when it paused from running (the database:
// job_resume_to_where_it_paused_from) and when its start deadline has passed by the database's clock (start_deadline_passed: the engine expires such a job and releases its reserve).
func (s *Store) ResumeScheduled(ctx context.Context, userID, jobID string) (Job, error) {
	return s.lockedMove(ctx, "resume", userID, jobID, false, func(tx pgx.Tx, j Job) error {
		if j.Status != StatusPaused {
			return notAllowed(reasonNotPaused)
		}
		var passed bool
		if err := tx.QueryRow(ctx, `SELECT start_deadline IS NOT NULL AND start_deadline <= now() FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, jobID, userID).Scan(&passed); err != nil {
			return s.fail(err)
		}
		if passed {
			return notAllowed(reasonDeadlinePassed)
		}
		return s.move(ctx, tx, userID, jobID, StatusScheduled, "resumed by the user")
	})
}
