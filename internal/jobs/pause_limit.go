package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/money"
)

// THE PAUSE LIMIT (Phase 5 S7, Docs/phase-5-spec.md decision 9). A job that has been paused for longer than PLATFORM_JOB_PAUSE_MAX_DAYS is ended by the ENGINE, so that a forgotten pause cannot hold a
// user's credits and their one-active-job slot for ever. The rule is split the way every rule of the lifecycle is:
//   - THE TIME IS THE DATABASE'S. platform.jobs.paused_at is set by jobs_transition_guard at the move into paused (platform_0018) and nobody else writes it; the comparison with the limit is made by the
//     database's now() in SQL, twice (the select that finds the candidates, and again under the lock before the move). The engine's own clock is not used for any of it, so a host with a skewed clock neither
//     cancels a job early nor keeps one for ever.
//   - WHAT THE ENGINE MAY DO IS THE DATABASE'S. platform.job_transitions gives the actor `engine` the two moves paused -> cancelling and paused -> cancelled (platform_0018), and the walls of platform_0016
//     decide which of the two a job may take: a job that paused from RUNNING (its module holds its items) goes to cancelling and the engine's own cancelling step tells the module, records what finished, ends the
//     job cancelled and releases what was not consumed; a job that paused from SCHEDULED (nothing was submitted) goes straight to cancelled, with the platform's own free result for every row, the terminal
//     marker on its items and the release of the whole reserve, in the same transaction. Nothing already sent is touched or refunded.
//   - THE LIMIT ITSELF IS A DIAL. The database does not know it, so no wall can say "not yet": that is this file's SQL. The refusal a user meets when they resume at the very moment the engine cancels is the
//     lifecycle's own (see cancelPausedOverLimit).

// The bounds of the limit the store accepts: one second (so that a test can use seconds; the dial of the service is in days) to 90 days, the top of PLATFORM_JOB_PAUSE_MAX_DAYS (pinned equal by a test in
// internal/config).
const (
	MinPauseLimit = time.Second
	MaxPauseLimit = 90 * 24 * time.Hour
)

// DefaultPauseLimitBatch and MaxPauseLimitBatch bound how many over-limit jobs one CancelOverLimitPaused call ends.
const (
	DefaultPauseLimitBatch = 50
	MaxPauseLimitBatch     = 1000
)

// ReasonPauseLimit is how every event the pause limit writes begins: the word an operator and a test look for.
const ReasonPauseLimit = "pause_limit"

// PauseLimitJob is one over-limit job as the sweep left it: the job, the status it paused from and the status the engine took it to (cancelling for a job its module holds, cancelled for one that never started).
type PauseLimitJob struct {
	JobID      string
	Module     string
	PausedFrom string
	To         string
}

// PauseLimitResult is what one sweep did. Found is how many paused jobs were over the limit when the sweep looked; Cancelled and Cancelling how many it ended directly and how many it handed to the engine's
// cancelling step; Skipped how many were no longer over the limit or no longer paused when the lock was taken (the user resumed, another instance got there first); Failed how many the lifecycle or the database
// refused (the job is left as it is and the next sweep looks again).
type PauseLimitResult struct {
	Found      int
	Cancelled  int
	Cancelling int
	Skipped    int
	Failed     int
	Jobs       []PauseLimitJob
}

// checkPauseLimit is the pure half of the dial's check.
func checkPauseLimit(limit time.Duration) error {
	if limit < MinPauseLimit || limit > MaxPauseLimit {
		return badRequest("the pause limit must be between %s and %s", MinPauseLimit, MaxPauseLimit)
	}
	return nil
}

// pauseLimitWords says a limit in the unit a person reads it in: whole days when it is a whole number of them, else the duration as Go writes it.
func pauseLimitWords(limit time.Duration) string {
	const day = 24 * time.Hour
	if limit%day != 0 {
		return limit.String()
	}
	if n := int64(limit / day); n != 1 {
		return fmt.Sprintf("%d days", n)
	}
	return "1 day"
}

// pauseLimitReason is the reason of the move and of its note: one line that begins with the word ReasonPauseLimit and names the limit.
func pauseLimitReason(limit time.Duration) string {
	return cleanReason(fmt.Sprintf("%s: the job was paused for longer than the limit of %s and the engine cancelled it; what was already sent is untouched and the credits not consumed are released", ReasonPauseLimit, pauseLimitWords(limit)))
}

// CancelOverLimitPaused ends, as the actor engine, every job that has been paused for longer than limit by the DATABASE's clock, at most max of them (0 means DefaultPauseLimitBatch), the ones paused longest
// first. Each job is its own transaction (cancelPausedOverLimit: the (user, module) lock first, then the job row FOR UPDATE, the condition judged again under both), so a failure of one leaves the others done
// and a repeat changes nothing: a job that is no longer paused, or no longer over the limit, is skipped and counted as such. Two sweeps at once (a deposed leader and its successor) take the lock one after the other,
// and the second finds nothing to do. The first error is returned beside the result (the sweep goes on past it); an invalid limit or batch is ErrBadRequest and nothing is read.
func (s *Store) CancelOverLimitPaused(ctx context.Context, limit time.Duration, max int) (PauseLimitResult, error) {
	if err := checkPauseLimit(limit); err != nil {
		return PauseLimitResult{}, opErr("pause limit", err)
	}
	if max == 0 {
		max = DefaultPauseLimitBatch
	}
	if max < 0 || max > MaxPauseLimitBatch {
		return PauseLimitResult{}, opErr("pause limit", badRequest("the batch must be between 1 and %d", MaxPauseLimitBatch))
	}
	rows, err := s.pool.Query(ctx, `SELECT j.id::text FROM platform.jobs j
		WHERE j.status = 'paused' AND j.paused_at IS NOT NULL AND j.paused_at <= now() - make_interval(secs => $1::double precision)
		ORDER BY j.paused_at, j.id LIMIT $2`, limit.Seconds(), max)
	if err != nil {
		return PauseLimitResult{}, opErr("pause limit", s.fail(err))
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return PauseLimitResult{}, opErr("pause limit", s.fail(err))
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PauseLimitResult{}, opErr("pause limit", s.fail(err))
	}

	res := PauseLimitResult{Found: len(ids)}
	var first error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			if first == nil {
				first = err
			}
			break
		}
		out, err := s.cancelPausedOverLimit(ctx, id, limit)
		switch {
		case err != nil:
			res.Failed++
			if first == nil {
				first = err
			}
		case out == nil:
			res.Skipped++
		default:
			res.Jobs = append(res.Jobs, *out)
			if out.To == StatusCancelling {
				res.Cancelling++
			} else {
				res.Cancelled++
			}
		}
	}
	return res, first
}

// cancelPausedOverLimit is one job's step, in ONE transaction, the lock first (engineTx). Under the lock the job is judged again: not paused any more (the user resumed or cancelled it, another instance
// ended it) or not over the limit any more (the user resumed and paused it again: paused_at is the database's time of THAT pause) is a nil outcome and nothing written. A user who resumes at the same moment
// therefore meets one of two things and never a half: the user's move wins the row lock and the engine finds a job that is running; or the engine wins and the user's resume finds a job that is cancelling or
// cancelled and is refused as every resume of a job that is not paused is (ErrNotAllowed, reason not_paused). The result is one `transition` event (reason beginning `pause_limit`, written by the lifecycle's own
// trigger with its actor engine) and one `note` event with the limit and the numbers in its detail.
func (s *Store) cancelPausedOverLimit(ctx context.Context, jobID string, limit time.Duration) (*PauseLimitJob, error) {
	var out *PauseLimitJob
	_, err := s.engineTx(ctx, "pause limit cancel", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		if j.Status != StatusPaused {
			return nil
		}
		var pausedAt time.Time
		var over bool
		var aged float64
		if err := tx.QueryRow(ctx, `SELECT paused_at, paused_at <= now() - make_interval(secs => $2::double precision), EXTRACT(EPOCH FROM (now() - paused_at))::double precision
			FROM platform.jobs WHERE id = $1::uuid AND paused_at IS NOT NULL`, j.ID, limit.Seconds()).Scan(&pausedAt, &over, &aged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // a paused job with no time cannot exist (jobs_paused_at_shape); if one did, it is not this sweep's to judge
			}
			return s.fail(err)
		}
		if !over {
			return nil
		}
		reason := pauseLimitReason(limit)
		to := StatusCancelled
		if j.PausedFrom == StatusRunning {
			to = StatusCancelling // the module holds the job's items: the engine's cancelling step tells it, records what finished, ends the job and releases the rest
		}
		if err := s.engineMove(ctx, tx, j.ID, to, reason); err != nil {
			return err
		}
		var unsent, marked int
		var released int64
		if to == StatusCancelled { // nothing was submitted: the end is this transaction's, like the user's own cancel of such a job
			var err error
			if unsent, err = s.writePlatformEnd(ctx, tx, j.ID, "cancelled", false); err != nil {
				return err
			}
			if marked, err = s.markNeverSubmittedItems(ctx, tx, j.ID); err != nil {
				return err
			}
			if metered {
				r, err := s.mny.ReleaseIn(ctx, tx, j.ID)
				if err != nil && !errors.Is(err, money.ErrNoReserve) {
					return err
				}
				released = r.Released
			}
		}
		detail, err := json.Marshal(map[string]any{
			"event": ReasonPauseLimit, "limit_seconds": int64(limit / time.Second), "paused_from": j.PausedFrom, "paused_at": pausedAt.UTC().Format(time.RFC3339),
			"paused_for_seconds": int64(aged), "moved_to": to, "rows_cancelled_unsent": unsent, "items_marked_cancelled": marked, "credits_released": released,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.job_events (job_id, kind, actor_kind, reason, detail) VALUES ($1::uuid, 'note', 'engine', $2, $3::jsonb)`, j.ID, reason, string(detail)); err != nil {
			return s.fail(err)
		}
		out = &PauseLimitJob{JobID: j.ID, Module: j.Module, PausedFrom: j.PausedFrom, To: to}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
