package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/faultpoint"
	"github.com/coldmail-platform/platform/internal/money"
)

// THE ENGINE'S HALF OF THE STORE. internal/jobengine (the leader-only loops) is the one caller of everything in this file; the API never reaches it. The engine is not a user: these methods take a
// JOB id and not a user id, read the job to learn its owner and module and act as the actor `engine` (platform.actor_kind = 'engine', no actor id). They are the one place the store reads or writes
// a job by id alone, and the one actor that may take the moves job_transitions reserves for the engine (confirmed|scheduled -> running, running|cancelling -> completing, completing -> succeeded|
// cancelled, ... -> expired, ... -> failed).
//
// THE RULES EVERY METHOD KEEPS.
//   - ONE TRANSACTION PER STEP, THE LOCK FIRST. A step that changes more than one thing (a move and its results, a settlement and its results, a move and the release) runs in one READ COMMITTED
//     transaction that first takes the (user, module) advisory lock (money.Store.LockIn, the same key every money writer of that pair takes) and then the job row FOR UPDATE: the order Confirm and
//     CancelBeforeStart use. A deposed leader finishing a step and a new leader starting the same one therefore run one after the other, and the second reads what the first left.
//   - EVERY METHOD IS SAFE TO REPEAT. A crash anywhere leaves a state the next call continues from: a replay of a step that already happened changes nothing and says so (an unchanged job, a count of
//     zero), and a replay of half a step cannot exist because a step is one transaction.
//   - EVERY TRANSITION IS THE LIFECYCLE'S. A move is `platform.job_transition(job, to, 'engine', NULL, reason)`: the database's guard decides whether it is legal, whether the engine may take it, whether
//     the clock allows it, and writes the event. A refusal arrives as ErrNotAllowed carrying the constraint's name; an unknown job is ErrNotFound; a request that cannot be right is ErrBadRequest. No
//     raw database error is returned for a refusal.
//   - THE DATABASE'S CLOCK. Deadlines, start times and staleness intervals are compared by the database (now()); the engine's own clock is used by the engine for nothing the database decides.
//   - NEVER AN INVENTED OUTCOME. A result is written only for an item the module reported final (RecordResults) or for a row the PLATFORM ended without the module (Expire, FailJob, CancelUnsubmitted:
//     the platform class, free, labelled with its charge-table rule).

// The ErrNotAllowed reasons of the engine's own (a refusal of the database's carries its constraint's name instead).
const (
	reasonNotSettling    = "not_settling"    // results are recorded for a running, cancelling or completing job only
	reasonNotCompleting  = "not_completing"  // Finish is for a job that is completing (or finished by an earlier Finish)
	reasonNotEnded       = "not_ended"       // Release is for a job that ended
	reasonNotActive      = "not_active"      // an annotation of a job that holds credits or a module
	reasonNotASendJob    = "not_a_send_job"  // module 1 has no campaign: its rows go to the module as a plan, not as public.job_items
	reasonNotSubmittable = "not_submittable" // rows are submitted for a job that is confirmed, scheduled or running
	reasonNotCancelling  = "not_cancelling"  // CancelUnsubmitted is for a cancelling job
)

// MaxDetailBytes bounds the detail one result item or one event may carry (a module's signals for one address, a flag's numbers): more is replaced by a note that says how much there was.
const MaxDetailBytes = 16 << 10

// maxReasonBytes bounds a reason the engine writes into an event.
const maxReasonBytes = 500

// MaxEngineIDs bounds the rows one PendingRows, EnsureModuleItems or MarkSubmitted call names. It is the longest list a job may hold (DefaultMaxRows, which is module 1's own limit for one job; a test pins
// the two equal): module 1 takes a job's WHOLE list in one request, so the engine must be able to read, hand over and stamp every row of the largest job in one call, or that job could never start.
// Nothing here is a statement per row: MarkSubmitted is one UPDATE over `= ANY(array)`, EnsureModuleItems one INSERT per rowBatch rows, PendingRows one SELECT.
const MaxEngineIDs = 100000

// IsTerminal says whether a status is one of the four that end a job.
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// EngineJob is a job as the engine's pass sees it: the job and the counts that decide its step, all read in ONE statement by the database's clock. Content is nil (the campaign's text can be a
// megabyte and the engine never needs it: the module reads public.job_items; EnsureModuleItems loads it itself).
type EngineJob struct {
	Job

	LastProgressAt *time.Time // when the job last made progress (set by the move to running and by NoteProgress); nil before the job ran
	StaleAlertedAt *time.Time // when a `stale` event was last written for it
	// ModuleRef is the module's own handle for the job (platform.jobs.module_job_ref, which Start writes: module 1's job id), "" when none was recorded (a sender has none; a job that has not started has
	// none yet). A module reads the handle of a job it did not submit in this process from here, never from a database of its own: the job the engine hands to Observe is the job as the store read it.
	ModuleRef string

	Metered        bool // modules 1 and 2 hold credits, module 3 does not
	DeadlinePassed bool // start_deadline <= now() by the DATABASE's clock (a confirmed, scheduled or paused job past it is the engine's to expire)

	Rows           int // every row of the list
	Pending        int // rows that reach the module (state pending)
	Submitted      int // pending rows whose submitted_at is stamped
	Results        int // results recorded (the platform's own and the module's)
	PendingResults int // results recorded for pending rows (the module's)
	Unsubmitted    int // pending rows with no stamp and no result: still to be handed to the module

	ReserveRemainder int64 // reserved - charged - released of the job's reserve; 0 when it holds none
}

// Unresolved is the number of pending rows that still have no result: a job cannot complete while it is above zero.
func (j EngineJob) Unresolved() int { return j.Pending - j.PendingResults }

// EngineRow is one pending row as the engine hands it to a module: its id (the item reference), its position, the normalised address and the row's merge data.
type EngineRow struct {
	ID        string
	Ordinal   int
	EmailNorm string
	Fields    map[string]string
}

// ResultItem is one item a module reported final: the item reference (job_rows.id), what happened (the charge table's input) and the module's own detail for it (a jsonb object, at most
// MaxDetailBytes; never a secret).
type ResultItem struct {
	ItemRef string
	Outcome charge.Outcome
	Detail  map[string]any
}

// RecordSummary is what one RecordResults call did. Recorded results are the ones this call wrote; AlreadyRecorded the final items whose result an earlier call had written (nothing changed for
// them); Charged and Credits what money.SettleIn newly charged; Unmapped the newly recorded results of decision unmapped (never charged, each flagged); NotFinal the items the charge table calls not
// final (not recorded: the module will report them again when they are); Rejected the items that name no pending row of this job or carry no state (not recorded, never charged).
type RecordSummary struct {
	Recorded        int
	AlreadyRecorded int
	Charged         int
	Credits         int64
	Unmapped        int
	NotFinal        int
	Rejected        int
}

// ---- reading what the engine must look at ------------------------------------------------------------------------------------------------------------------------------------------------

// engineJobColumns is jobColumns without the campaign (see EngineJob.Content) and with the engine's counts after it, read in the order scanEngineJob scans them.
var engineJobColumns = strings.Replace(jobColumns, "j.frozen_settings", "(j.frozen_settings - 'content')", 1) + `,
	j.last_progress_at, j.stale_alerted_at, m.metered, (j.start_deadline IS NOT NULL AND j.start_deadline <= now()),
	c.n_rows, c.n_pending, c.n_submitted, c.n_results, c.n_pending_results, c.n_unsubmitted,
	COALESCE(rs.reserved - rs.charged - rs.released, 0)::bigint, COALESCE(j.module_job_ref, '')`

// engineJobFrom joins what the counts need: the module, the reserve (one per job at most) and the rows with their results.
const engineJobFrom = `
	JOIN platform.modules m ON m.id = j.module_id
	LEFT JOIN platform.reserves rs ON rs.job_id = j.id
	CROSS JOIN LATERAL (
		SELECT count(*)::bigint AS n_rows,
		       count(*) FILTER (WHERE w.state = 'pending')::bigint AS n_pending,
		       count(*) FILTER (WHERE w.state = 'pending' AND w.submitted_at IS NOT NULL)::bigint AS n_submitted,
		       count(r.item_ref)::bigint AS n_results,
		       count(r.item_ref) FILTER (WHERE w.state = 'pending')::bigint AS n_pending_results,
		       count(*) FILTER (WHERE w.state = 'pending' AND w.submitted_at IS NULL AND r.item_ref IS NULL)::bigint AS n_unsubmitted
		FROM platform.job_rows w
		LEFT JOIN platform.job_results r ON r.job_id = w.job_id AND r.item_ref = w.id
		WHERE w.job_id = j.id
	) c`

func scanEngineJob(row pgx.Row) (EngineJob, error) {
	var e EngineJob
	var rows, pending, submitted, results, pendingResults, unsubmitted int64
	j, err := scanJobWith(row, &e.LastProgressAt, &e.StaleAlertedAt, &e.Metered, &e.DeadlinePassed, &rows, &pending, &submitted, &results, &pendingResults, &unsubmitted, &e.ReserveRemainder, &e.ModuleRef)
	if err != nil {
		return EngineJob{}, err
	}
	e.Job = j
	e.LastProgressAt, e.StaleAlertedAt = utc(e.LastProgressAt), utc(e.StaleAlertedAt)
	e.Rows, e.Pending, e.Submitted, e.Results, e.PendingResults, e.Unsubmitted = int(rows), int(pending), int(submitted), int(results), int(pendingResults), int(unsubmitted)
	return e, nil
}

// DefaultEngineWorkLimit and MaxEngineWorkLimit bound one EngineWork call.
const (
	DefaultEngineWorkLimit = 100
	MaxEngineWorkLimit     = 10000
)

// EngineWork returns the jobs the engine must look at, at most limit of them (0 means DefaultEngineWorkLimit), the ones that made progress longest ago first (COALESCE(last_progress_at,
// confirmed_at), then id), each with its counts. The work is, by the DATABASE's clock:
//   - every confirmed, running, cancelling and completing job;
//   - every scheduled job whose scheduled_start has come;
//   - every paused job whose start_deadline has passed (a job paused before its start that nobody resumed: the engine expires it and releases its reserve);
//   - the REPAIR set: every terminal job (succeeded, failed, cancelled, expired) whose reserve still holds a remainder (reserved - charged - released > 0): a crash window between the move and
//     the release, or a job an administrator ended by hand.
func (s *Store) EngineWork(ctx context.Context, limit int) ([]EngineJob, error) {
	if limit == 0 {
		limit = DefaultEngineWorkLimit
	}
	if limit < 0 || limit > MaxEngineWorkLimit {
		return nil, opErr("engine work", badRequest("the limit must be between 1 and %d", MaxEngineWorkLimit))
	}
	rows, err := s.pool.Query(ctx, `WITH cand AS (
			SELECT j.id, COALESCE(j.last_progress_at, j.confirmed_at) AS ord
			FROM platform.jobs j
			LEFT JOIN platform.reserves rs ON rs.job_id = j.id
			WHERE j.status IN ('confirmed', 'running', 'cancelling', 'completing')
			   OR (j.status = 'scheduled' AND j.scheduled_start IS NOT NULL AND j.scheduled_start <= now())
			   OR (j.status = 'paused' AND j.start_deadline IS NOT NULL AND j.start_deadline <= now())
			   OR (j.status IN ('succeeded', 'failed', 'cancelled', 'expired') AND COALESCE(rs.reserved - rs.charged - rs.released, 0) > 0)
			ORDER BY ord, j.id
			LIMIT $1)
		SELECT `+engineJobColumns+`
		FROM cand JOIN platform.jobs j ON j.id = cand.id`+engineJobFrom+`
		ORDER BY cand.ord, j.id`, limit)
	if err != nil {
		return nil, opErr("engine work", s.fail(err))
	}
	defer rows.Close()
	out := []EngineJob{}
	for rows.Next() {
		e, err := scanEngineJob(rows)
		if err != nil {
			return nil, opErr("engine work", s.fail(err))
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr("engine work", s.fail(err))
	}
	return out, nil
}

// EngineJobByID is one job with the same counts EngineWork reads (ErrNotFound for an id that is not a job): what the engine asks after a step that changed them.
func (s *Store) EngineJobByID(ctx context.Context, jobID string) (EngineJob, error) {
	if !isUUID(jobID) {
		return EngineJob{}, opErr("engine job", ErrNotFound)
	}
	e, err := scanEngineJob(s.pool.QueryRow(ctx, `SELECT `+engineJobColumns+` FROM platform.jobs j`+engineJobFrom+` WHERE j.id = $1::uuid`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return EngineJob{}, opErr("engine job", ErrNotFound)
	}
	if err != nil {
		return EngineJob{}, opErr("engine job", s.fail(err))
	}
	return e, nil
}

// getJobByID reads a job by its id alone (the engine is not a user): ErrNotFound when there is none. lock adds FOR UPDATE.
func (s *Store) getJobByID(ctx context.Context, q querier, jobID string, lock bool) (Job, error) {
	sql := `SELECT ` + jobColumns + ` FROM platform.jobs j WHERE j.id = $1::uuid`
	if lock {
		sql += ` FOR UPDATE`
	}
	j, err := scanJob(q.QueryRow(ctx, sql, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, s.fail(err)
	}
	return j, nil
}

// engineTx runs fn in ONE transaction on the job: the job's owner and module are read, the (user, module) advisory lock is taken FIRST (money.Store.LockIn), then the job row FOR UPDATE, then fn
// runs with the job as it is under both. The job is returned as it is after fn. ErrNotFound for an unknown job; a refusal of fn or of the database arrives typed (see fail); a lock wait that ran
// out is money.ErrLockTimeout.
func (s *Store) engineTx(ctx context.Context, op, jobID string, fn func(tx pgx.Tx, j Job, metered bool) error) (Job, error) {
	if !isUUID(jobID) {
		return Job{}, opErr(op, ErrNotFound)
	}
	var before, after Job
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var user, module string
		var metered bool
		if err := tx.QueryRow(ctx, `SELECT j.user_id::text, j.module_id, m.metered FROM platform.jobs j JOIN platform.modules m ON m.id = j.module_id WHERE j.id = $1::uuid`, jobID).Scan(&user, &module, &metered); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return s.fail(err)
		}
		if err := s.mny.LockIn(ctx, tx, user, module); err != nil {
			return err
		}
		j, err := s.getJobByID(ctx, tx, jobID, true)
		if err != nil {
			return err
		}
		before = j
		if err := fn(tx, j, metered); err != nil {
			return err
		}
		after, err = s.getJobByID(ctx, tx, jobID, false)
		return err
	})
	if err != nil {
		s.log.Info("engine step refused", "op", op, "job", jobID, "reason", refusalWord(err))
		return Job{}, opErr(op, err)
	}
	if before.Status != after.Status {
		s.log.Info("job moved by the engine", "op", op, "job", after.ID, "module", after.Module, "from", before.Status, "to", after.Status)
	}
	return after, nil
}

// engineMove is the engine's move: `platform.job_transition(job, to, 'engine', NULL, reason)`. The database's guard decides; a refusal is ErrNotAllowed carrying the constraint's name.
func (s *Store) engineMove(ctx context.Context, tx pgx.Tx, jobID, to, reason string) error {
	if _, err := tx.Exec(ctx, `SELECT platform.job_transition($1::uuid, $2, 'engine', NULL::uuid, $3)`, jobID, to, reason); err != nil {
		return s.fail(err)
	}
	return nil
}

// cleanReason makes a reason storable and readable: valid text, one line, no more than maxReasonBytes. A blank reason stays blank (the caller decides what that means).
func cleanReason(r string) string {
	r = strings.Join(strings.Fields(cleanText(r)), " ")
	return cutText(r, maxReasonBytes)
}

// detailJSON encodes a detail object for a jsonb column: nil is {}; one that does not encode, or is larger than MaxDetailBytes, is replaced by an object that says so (the item is still
// recorded: a result is never lost for the size of its detail).
func detailJSON(d map[string]any) string {
	if len(d) == 0 {
		return "{}"
	}
	b, err := json.Marshal(d)
	switch {
	case err != nil:
		return `{"detail":"not_encodable"}`
	case len(b) > MaxDetailBytes:
		return fmt.Sprintf(`{"detail":"too_large","bytes":%d}`, len(b))
	case !textOK(string(b)) || strings.Contains(string(b), `\u0000`): // jsonb refuses the escape of a NUL
		return `{"detail":"not_text"}`
	}
	return string(b)
}

// ---- handing rows to a module ------------------------------------------------------------------------------------------------------------------------------------------------------------

// PendingRows returns the rows of a confirmed, scheduled or running job that the module has not been given and has not answered for: state pending, no submitted_at stamp, no result, by ordinal,
// at most limit (1 to MaxEngineIDs). A job in any other state has nothing to submit (an empty answer, not an error: a cancelling job gets no more rows).
func (s *Store) PendingRows(ctx context.Context, jobID string, limit int) ([]EngineRow, error) {
	if !isUUID(jobID) {
		return nil, opErr("pending rows", ErrNotFound)
	}
	if err := checkPendingLimit(limit); err != nil {
		return nil, opErr("pending rows", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT w.id::text, w.ordinal, w.email_norm, w.fields
		FROM platform.job_rows w JOIN platform.jobs j ON j.id = w.job_id
		WHERE w.job_id = $1::uuid AND j.status IN ('confirmed', 'scheduled', 'running')
		  AND w.state = 'pending' AND w.submitted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM platform.job_results r WHERE r.job_id = w.job_id AND r.item_ref = w.id)
		ORDER BY w.ordinal
		LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, opErr("pending rows", s.fail(err))
	}
	defer rows.Close()
	out, err := s.scanEngineRows(rows)
	return out, opErr("pending rows", err)
}

// checkPendingLimit is the bound of one PendingRows call: 1 to MaxEngineIDs.
func checkPendingLimit(limit int) error {
	if limit < 1 || limit > MaxEngineIDs {
		return badRequest("the limit must be between 1 and %d", MaxEngineIDs)
	}
	return nil
}

// scanEngineRows reads rows of (id, ordinal, email_norm, fields) to the end.
func (s *Store) scanEngineRows(rows pgx.Rows) ([]EngineRow, error) {
	out := []EngineRow{}
	for rows.Next() {
		var r EngineRow
		var ord int32
		var email *string
		var fields []byte
		if err := rows.Scan(&r.ID, &ord, &email, &fields); err != nil {
			return nil, s.fail(err)
		}
		r.Ordinal, r.EmailNorm = int(ord), deref(email)
		r.Fields = map[string]string{}
		if len(fields) > 0 {
			if err := json.Unmarshal(fields, &r.Fields); err != nil {
				return nil, fmt.Errorf("the merge data of row %s is not a flat object: %w", r.ID, err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(err)
	}
	return out, nil
}

// checkIDs validates and de-duplicates the row ids a caller names: each must be the canonical lower-case uuid; at most MaxEngineIDs.
func checkIDs(ids []string) ([]string, error) {
	if len(ids) > MaxEngineIDs {
		return nil, badRequest("%d row ids in one call, the limit is %d", len(ids), MaxEngineIDs)
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !isUUID(id) {
			return nil, badRequest("a row id is not a canonical uuid")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// EnsureModuleItems writes the module-facing rows of a SEND job (modules 2 and 3): one public.job_items row per named pending row, with id = job_rows.id (the item reference), campaign_id = the job's
// id, user_id = the job's owner, recipient_email = the row's normalised address and the subject, body, plain text and reply-to of the job's frozen campaign through the store's Renderer (PlainRenderer
// in Phase 4). rendered_text is NULL when the text is blank (to both modules a blank text means "no plain-text part"). It is idempotent by id (ON CONFLICT (id) DO NOTHING) and returns how many rows
// it newly wrote: calling it again with the same ids writes nothing. Every id must be a pending row of the job (ErrBadRequest otherwise: nothing is written), the job a send job (module 1 has no
// campaign: ErrNotAllowed) in confirmed, scheduled or running, and every message must satisfy the size rules of the shared table and of the modules (checkRendered: ErrBadRequest, nothing written).
func (s *Store) EnsureModuleItems(ctx context.Context, jobID string, ids []string) (int, error) {
	want, err := checkIDs(ids)
	if err != nil {
		return 0, opErr("ensure items", err)
	}
	if len(want) == 0 {
		return 0, nil
	}
	inserted := 0
	_, err = s.engineTx(ctx, "ensure items", jobID, func(tx pgx.Tx, j Job, _ bool) error {
		if j.Content == nil {
			return notAllowed(reasonNotASendJob)
		}
		switch j.Status {
		case StatusConfirmed, StatusScheduled, StatusRunning:
		default:
			return notAllowed(reasonNotSubmittable)
		}
		rows, err := tx.Query(ctx, `SELECT id::text, ordinal, email_norm, fields FROM platform.job_rows
			WHERE job_id = $1::uuid AND state = 'pending' AND id = ANY($2::text[]::uuid[]) ORDER BY ordinal`, j.ID, want)
		if err != nil {
			return s.fail(err)
		}
		pend, err := s.scanEngineRows(rows)
		if err != nil {
			return err
		}
		if len(pend) != len(want) {
			return badRequest("%d of the %d named rows are not pending rows of the job", len(want)-len(pend), len(want))
		}
		msgs := make([]renderedMessage, len(pend))
		for i, r := range pend {
			if msgs[i], err = renderRow(s.render, *j.Content, r); err != nil {
				return err
			}
		}
		for start := 0; start < len(pend); start += rowBatch {
			end := min(start+rowBatch, len(pend))
			n := end - start
			rid, email, subject, body, text, reply := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
			for i := start; i < end; i++ {
				rid[i-start], email[i-start], subject[i-start], body[i-start], text[i-start], reply[i-start] = pend[i].ID, pend[i].EmailNorm, msgs[i].subject, msgs[i].html, msgs[i].text, msgs[i].replyTo
			}
			tag, err := tx.Exec(ctx, `INSERT INTO public.job_items (id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, reply_to)
				SELECT u.id::uuid, $1::uuid, $2::uuid, u.email, u.subject, u.body, NULLIF(u.txt, ''), NULLIF(u.reply, '')
				FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[]) AS u(id, email, subject, body, txt, reply)
				ON CONFLICT (id) DO NOTHING`, j.ID, j.UserID, rid, email, subject, body, text, reply)
			if err != nil {
				return s.fail(err)
			}
			inserted += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// MarkSubmitted stamps submitted_at = now() on the named rows the module now holds, for a job that is confirmed, scheduled, running, cancelling or completing (a stamp for a job in any other status is
// ErrNotAllowed, reason not_submittable: a draft's rows are nobody's to stamp). A row already stamped keeps its stamp (it is not an error and not a second write); a row that is not a pending row of
// the job is left alone. Idempotent by construction: it is one statement, and a stamp is written once (the table refuses a second). ErrNotFound for an unknown job.
func (s *Store) MarkSubmitted(ctx context.Context, jobID string, rowIDs []string) error {
	if !isUUID(jobID) {
		return opErr("mark submitted", ErrNotFound)
	}
	ids, err := checkIDs(rowIDs)
	if err != nil {
		return opErr("mark submitted", err)
	}
	if len(ids) == 0 {
		return nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE platform.job_rows w SET submitted_at = now()
		FROM platform.jobs j
		WHERE j.id = w.job_id AND w.job_id = $1::uuid AND j.status IN ('confirmed', 'scheduled', 'running', 'cancelling', 'completing')
		  AND w.state = 'pending' AND w.submitted_at IS NULL AND w.id = ANY($2::text[]::uuid[])`, jobID, ids)
	if err != nil {
		return opErr("mark submitted", s.fail(err))
	}
	if tag.RowsAffected() == 0 { // nothing newly stamped: they all were, or there is no such job, or the job is not in a state that holds rows: say which
		var status string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM platform.jobs WHERE id = $1::uuid`, jobID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return opErr("mark submitted", ErrNotFound)
			}
			return opErr("mark submitted", s.fail(err))
		}
		switch status {
		case StatusConfirmed, StatusScheduled, StatusRunning, StatusCancelling, StatusCompleting:
		default:
			return opErr("mark submitted", notAllowed(reasonNotSubmittable))
		}
	}
	return nil
}

// ---- starting ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// Start moves a confirmed or scheduled job to running as the engine, in ONE transaction, and records the module's own handle for the job (moduleRef: module 1's job id; "" for the senders) when
// one is given. The database refuses a scheduled job before its scheduled_start (ErrNotAllowed, job_started_early), a job in any other status that is not running (job_transition_illegal,
// jobs_terminal_is_final, ...) and a module reference another job of the module already holds (ErrModuleRefTaken). A job that is already running is returned unchanged (a replay). The fault point
// start.before_running is hit just before the commit: a process killed there rolls the move back and the module, which already holds the rows, is handed them again idempotently.
func (s *Store) Start(ctx context.Context, jobID, moduleRef string) (Job, error) {
	moduleRef = strings.TrimSpace(moduleRef)
	if moduleRef != "" && (len(moduleRef) > 200 || !textOK(moduleRef)) {
		return Job{}, opErr("start", badRequest("the module reference is not a short text"))
	}
	return s.engineTx(ctx, "start", jobID, func(tx pgx.Tx, j Job, _ bool) error {
		if j.Status == StatusRunning {
			return nil
		}
		if moduleRef != "" {
			if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET module_job_ref = $2 WHERE id = $1::uuid`, j.ID, moduleRef); err != nil {
				return s.fail(err)
			}
		}
		if err := s.engineMove(ctx, tx, j.ID, StatusRunning, "the engine submitted the job to its module"); err != nil {
			return err
		}
		faultpoint.Hit("start.before_running")
		return nil
	})
}

// ---- settling ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// preparedItem is one reported item the engine may record: the item reference, what the charge table decides about its outcome and its detail as JSON.
type preparedItem struct {
	ref      string
	outcome  charge.Outcome
	decision charge.Decision
	detail   string
}

// prepareItems is the pure half of RecordResults: it normalises the references (canonical lower-case uuids; a repeat within one call is the same item and counts once), leaves out what is not
// final (the charge table's NotFinal) and what can never be recorded (a reference that is no uuid, an outcome with no state), and decides every other item with charge.Decide, the function the money
// store settles with. An outcome that names another module than the job's is a bug of the caller and fails the whole call.
func prepareItems(module string, items []ResultItem) (final []preparedItem, rejected, notFinal int, err error) {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		ref := strings.ToLower(strings.TrimSpace(it.ItemRef))
		if !isUUID(ref) || strings.TrimSpace(it.Outcome.State) == "" {
			rejected++
			continue
		}
		if it.Outcome.Module != module {
			return nil, 0, 0, badRequest("an outcome names module %q in a job of module %q", clipWord(it.Outcome.Module), module)
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		d := charge.Decide(it.Outcome)
		if d.Kind == charge.NotFinal {
			notFinal++
			continue
		}
		final = append(final, preparedItem{ref: ref, outcome: it.Outcome, decision: d, detail: detailJSON(it.Detail)})
	}
	return final, rejected, notFinal, nil
}

// clipWord shortens a word for an error text.
func clipWord(s string) string {
	if len(s) > 32 {
		return cutText(s, 32) + "..."
	}
	return s
}

// resultDecision is what job_results records for one final item: the decision word, the unit (charged items only) and the rule.
type resultDecision struct {
	decision string
	unit     string
	rule     string
}

// decide turns the charge table's decision into the row's, given what the ledger and the settlement say: an item the ledger holds a charge for is `charge` under the LEDGER's unit (the first
// charge stands, so the result can never disagree with the ledger); an item the table would charge but the settlement could not price (its unit is not in the job's frozen snapshot), or that belongs
// to a module that holds no credits, is `unmapped`; a free item is `free`; an item the table cannot classify is `unmapped`. A charge the table asks for that the ledger does not hold is an error.
func decide(p preparedItem, ledgerUnit string, unpriced, metered bool) (resultDecision, error) {
	switch {
	case ledgerUnit != "":
		return resultDecision{"charge", ledgerUnit, p.decision.Rule}, nil
	case p.decision.Kind == charge.Charge && (unpriced || !metered):
		return resultDecision{"unmapped", "", p.decision.Rule}, nil
	case p.decision.Kind == charge.Charge:
		return resultDecision{}, fmt.Errorf("jobs: item %s was settled as a charge of rule %s but the ledger holds no charge for it", p.ref, p.decision.Rule)
	case p.decision.Kind == charge.Free:
		return resultDecision{"free", "", p.decision.Rule}, nil
	}
	return resultDecision{"unmapped", "", p.decision.Rule}, nil
}

// RecordResults records what a module reported final, in ONE transaction (the lock first): for a METERED module the items are settled with money.SettleIn (charge.Decide's decisions against the
// job's frozen prices and its reserve; an item the ledger already charged is AlreadySettled), and every item gets its job_results row (ON CONFLICT (job_id, item_ref) DO NOTHING, origin
// 'module', module_state = the outcome's state, decision/unit/rule from charge.Decide, the address and position read from the job's row in the same statement). The result of a charged item
// carries the unit the LEDGER holds. An item decided unmapped (the charge table cannot classify it, or its unit is not part of the job's price snapshot) is recorded unmapped, never charged, and
// raises one `flag` job event per rule naming it. NotFinal items are not recorded, and neither is an item that names no pending row of this job. Module 3 is not metered: no money call, every
// item is free (M3-99, M3-01). The job must be running, cancelling or completing. After the commit the fault point settle.after_commit is hit (when something was recorded).
//
// Items are decided in one pass and settled in pages of money.MaxSettleItems; the call is idempotent: the same items again change nothing (RecordSummary.AlreadyRecorded says so).
func (s *Store) RecordResults(ctx context.Context, jobID string, items []ResultItem) (RecordSummary, error) {
	var sum RecordSummary
	_, err := s.engineTx(ctx, "record results", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		switch j.Status {
		case StatusRunning, StatusCancelling, StatusCompleting:
		default:
			return notAllowed(reasonNotSettling)
		}
		var err error
		sum, err = s.recordResults(ctx, tx, j, metered, items)
		return err
	})
	if err != nil {
		return RecordSummary{}, err
	}
	if sum.Recorded > 0 {
		faultpoint.Hit("settle.after_commit")
	}
	return sum, nil
}

func (s *Store) recordResults(ctx context.Context, tx pgx.Tx, j Job, metered bool, items []ResultItem) (RecordSummary, error) {
	var sum RecordSummary
	final, rejected, notFinal, err := prepareItems(j.Module, items)
	if err != nil {
		return RecordSummary{}, err
	}
	sum.NotFinal, sum.Rejected = notFinal, rejected

	// only a PENDING ROW OF THIS JOB can be settled or recorded: an item the module reports that is not one is never charged
	if len(final) > 0 {
		refs := make([]string, len(final))
		for i, p := range final {
			refs[i] = p.ref
		}
		known, err := s.pendingAmong(ctx, tx, j.ID, refs)
		if err != nil {
			return RecordSummary{}, err
		}
		kept := final[:0]
		for _, p := range final {
			if known[p.ref] {
				kept = append(kept, p)
			} else {
				sum.Rejected++
			}
		}
		final = kept
	}
	if len(final) == 0 {
		return sum, nil
	}

	// the money: the items the table charges, settled against the job's reserve; then the ledger is read back, because the ledger is the authority on what was charged
	unpriced := map[string]bool{}
	ledgerUnit := map[string]string{}
	if metered {
		var toSettle []money.SettleItem
		for _, p := range final {
			if p.decision.Kind == charge.Charge {
				toSettle = append(toSettle, money.SettleItem{ItemRef: p.ref, Outcome: p.outcome})
			}
		}
		for start := 0; start < len(toSettle); start += money.MaxSettleItems {
			res, err := s.mny.SettleIn(ctx, tx, money.SettleReq{JobID: j.ID, Items: toSettle[start:min(start+money.MaxSettleItems, len(toSettle))]})
			if err != nil {
				return RecordSummary{}, err
			}
			sum.Charged += res.Charged
			sum.Credits += res.Credits
			for _, u := range res.Unmapped {
				unpriced[u.ItemRef] = true
			}
		}
		refs := make([]string, len(final))
		for i, p := range final {
			refs[i] = p.ref
		}
		rows, err := tx.Query(ctx, `SELECT item_ref, unit FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'charge' AND item_ref = ANY($2::text[])`, j.ID, refs)
		if err != nil {
			return RecordSummary{}, s.fail(err)
		}
		for rows.Next() {
			var ref, unit string
			if err := rows.Scan(&ref, &unit); err != nil {
				rows.Close()
				return RecordSummary{}, s.fail(err)
			}
			ledgerUnit[ref] = unit
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return RecordSummary{}, s.fail(err)
		}
	}

	// the results: one row per final item, the platform's idempotence being the primary key
	n := len(final)
	refs, states, reasons, decisions, units, rules, details := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, p := range final {
		d, err := decide(p, ledgerUnit[p.ref], unpriced[p.ref], metered)
		if err != nil {
			return RecordSummary{}, err
		}
		refs[i], states[i], reasons[i], decisions[i], units[i], rules[i], details[i] = p.ref, p.outcome.State, p.outcome.Reason, d.decision, d.unit, d.rule, p.detail
	}
	rows, err := tx.Query(ctx, `INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, reason, decision, unit, rule, detail)
		SELECT w.job_id, w.user_id, w.id, w.ordinal, COALESCE(w.email_norm, w.email_raw), 'module', u.state, NULLIF(u.reason, ''), u.decision, NULLIF(u.unit, ''), u.rule, u.detail::jsonb
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[]) AS u(ref, state, reason, decision, unit, rule, detail)
		JOIN platform.job_rows w ON w.job_id = $1::uuid AND w.id = u.ref::uuid AND w.state = 'pending'
		ORDER BY w.ordinal
		ON CONFLICT (job_id, item_ref) DO NOTHING
		RETURNING item_ref::text, decision, rule`, j.ID, refs, states, reasons, decisions, units, rules, details)
	if err != nil {
		return RecordSummary{}, s.fail(err)
	}
	unmappedBy := map[string][]string{} // rule -> the item references newly recorded unmapped under it
	for rows.Next() {
		var ref, decision, rule string
		if err := rows.Scan(&ref, &decision, &rule); err != nil {
			rows.Close()
			return RecordSummary{}, s.fail(err)
		}
		sum.Recorded++
		if decision == "unmapped" {
			sum.Unmapped++
			unmappedBy[rule] = append(unmappedBy[rule], ref)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RecordSummary{}, s.fail(err)
	}
	sum.AlreadyRecorded = len(final) - sum.Recorded

	// one flag per rule that newly recorded an item unmapped (a replay records nothing new, so it flags nothing new)
	rulesFlagged := make([]string, 0, len(unmappedBy))
	for r := range unmappedBy {
		rulesFlagged = append(rulesFlagged, r)
	}
	sort.Strings(rulesFlagged)
	for _, rule := range rulesFlagged {
		refs := unmappedBy[rule]
		sample := refs[:min(len(refs), 10)]
		detail, _ := json.Marshal(map[string]any{"rule": rule, "items": len(refs), "item_refs": sample})
		if _, err := tx.Exec(ctx, `INSERT INTO platform.job_events (job_id, kind, actor_kind, reason, detail) VALUES ($1::uuid, 'flag', 'engine', $2, $3::jsonb)`,
			j.ID, fmt.Sprintf("an outcome that does not map cleanly was recorded unmapped and never charged (rule %s)", rule), string(detail)); err != nil {
			return RecordSummary{}, s.fail(err)
		}
	}
	s.log.Info("results recorded", "job", j.ID, "module", j.Module, "recorded", sum.Recorded, "already", sum.AlreadyRecorded, "charged", sum.Charged, "unmapped", sum.Unmapped, "rejected", sum.Rejected, "not_final", sum.NotFinal)
	return sum, nil
}

// pendingAmong returns which of the references are pending rows of the job.
func (s *Store) pendingAmong(ctx context.Context, tx pgx.Tx, jobID string, refs []string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending' AND id = ANY($2::text[]::uuid[])`, jobID, refs)
	if err != nil {
		return nil, s.fail(err)
	}
	defer rows.Close()
	known := make(map[string]bool, len(refs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, s.fail(err)
		}
		known[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(err)
	}
	return known, nil
}

// ---- finishing --------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// missingResults is the number of pending rows of the job that have no result.
func (s *Store) missingResults(ctx context.Context, tx pgx.Tx, jobID string) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.job_rows w WHERE w.job_id = $1::uuid AND w.state = 'pending'
		AND NOT EXISTS (SELECT 1 FROM platform.job_results r WHERE r.job_id = w.job_id AND r.item_ref = w.id)`, jobID).Scan(&n); err != nil {
		return 0, s.fail(err)
	}
	return n, nil
}

// requireResults is the check Complete and Finish share: ErrIncomplete (an *IncompleteError with the number) while a pending row has no result.
func (s *Store) requireResults(ctx context.Context, tx pgx.Tx, jobID string) error {
	n, err := s.missingResults(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if n > 0 {
		return &IncompleteError{Missing: n}
	}
	return nil
}

// Complete moves a running or cancelling job to completing as the engine, when and only when every pending row has a result: the CALLER (the engine) asserts that the module reported every item
// final, this method refuses (ErrIncomplete, with the number of rows missing, nothing changed) while a pending row has no result, so that the platform can never end a job over an item it has no
// outcome for. A job that is already completing is returned unchanged (a replay); any other status is refused by the lifecycle.
func (s *Store) Complete(ctx context.Context, jobID string) (Job, error) {
	return s.engineTx(ctx, "complete", jobID, func(tx pgx.Tx, j Job, _ bool) error {
		if j.Status == StatusCompleting {
			return nil
		}
		if j.Status == StatusRunning || j.Status == StatusCancelling {
			if err := s.requireResults(ctx, tx, j.ID); err != nil {
				return err
			}
		}
		return s.engineMove(ctx, tx, j.ID, StatusCompleting, "the module reports every item final and every result is recorded")
	})
}

// releaseIn returns the remainder of a metered job's reserve in the caller's transaction (money.ReleaseIn: once, a replay finds nothing left). A job that holds no reserve, or whose module holds
// no credits, has nothing to release.
func (s *Store) releaseIn(ctx context.Context, tx pgx.Tx, j Job, metered bool) error {
	if !metered {
		return nil
	}
	if _, err := s.mny.ReleaseIn(ctx, tx, j.ID); err != nil && !errors.Is(err, money.ErrNoReserve) {
		return err
	}
	return nil
}

// Finish ends a completing job as the engine, in ONE transaction: every pending row must have a result (ErrIncomplete, nothing changed), then completing -> succeeded (no cancel was accepted) or
// completing -> cancelled (cancel_requested_at is set; the database refuses the other pairing), then, for a metered module, money.ReleaseIn gives the remainder back (once: a replay of the
// release finds nothing left). finished_at is the database's. The fault point release.mid_tx is hit between the move and the release: a process killed there rolls BOTH back, which leaves the job
// completing with its reserve intact for the next call. A job that already succeeded or was cancelled is returned as it is, after making sure its remainder went back (a replay).
func (s *Store) Finish(ctx context.Context, jobID string) (Job, error) {
	return s.engineTx(ctx, "finish", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		switch j.Status {
		case StatusSucceeded, StatusCancelled:
			return s.releaseIn(ctx, tx, j, metered)
		case StatusCompleting:
		default:
			return notAllowed(reasonNotCompleting)
		}
		if err := s.requireResults(ctx, tx, j.ID); err != nil {
			return err
		}
		to, reason := StatusSucceeded, "every result is recorded: the job succeeded"
		if j.CancelRequestedAt != nil {
			to, reason = StatusCancelled, "every result is recorded after the cancel the module accepted: the job is cancelled"
		}
		if err := s.engineMove(ctx, tx, j.ID, to, reason); err != nil {
			return err
		}
		faultpoint.Hit("release.mid_tx")
		return s.releaseIn(ctx, tx, j, metered)
	})
}

// writePlatformEnd gives every row that has no result yet the platform's own result of the given state (origin 'platform', free, labelled with the charge table's rule for it): rows that already
// have a result (malformed, duplicate, suppressed, excluded, or a module's) keep theirs. unsubmittedOnly restricts it to pending rows no module was given. It returns how many it wrote.
func (s *Store) writePlatformEnd(ctx context.Context, tx pgx.Tx, jobID, state string, unsubmittedOnly bool) (int, error) {
	rule, err := resultRule(state)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, reason, decision, rule)
		SELECT r.job_id, r.user_id, r.id, r.ordinal, COALESCE(r.email_norm, r.email_raw), 'platform', $2::text, NULL, 'free', $3::text
		FROM platform.job_rows r
		WHERE r.job_id = $1::uuid
		  AND ($4::boolean = false OR (r.state = 'pending' AND r.submitted_at IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM platform.job_results x WHERE x.job_id = r.job_id AND x.item_ref = r.id)`, jobID, state, rule, unsubmittedOnly)
	if err != nil {
		return 0, s.fail(err)
	}
	return int(tag.RowsAffected()), nil
}

// endedResultState is the platform result a row without one gets when its job ended some other way than through the engine: how the job ended names it (a succeeded job has none to write: Finish
// refuses to end a job while a pending row has no result).
func endedResultState(status string) string {
	switch status {
	case StatusFailed:
		return "infrastructure_failure"
	case StatusCancelled:
		return "cancelled"
	case StatusExpired:
		return "expired"
	}
	return ""
}

// Expire ends a confirmed, scheduled or paused job whose start deadline has passed (the database's clock: before it the guard refuses with job_expired_early) as the engine, in ONE transaction: the
// move to expired, a result for EVERY row that has none (the platform class: module_state `expired`, free, rule P-02; a row that already has a result keeps it) and, for a metered module, the
// release of the whole reserve. A job that is already expired is returned as it is. Any other status is refused by the lifecycle.
func (s *Store) Expire(ctx context.Context, jobID string) (Job, error) {
	return s.engineTx(ctx, "expire", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		if j.Status == StatusExpired {
			return nil
		}
		if err := s.engineMove(ctx, tx, j.ID, StatusExpired, "the module could not take the job before its start deadline"); err != nil {
			return err
		}
		if _, err := s.writePlatformEnd(ctx, tx, j.ID, "expired", false); err != nil {
			return err
		}
		return s.releaseIn(ctx, tx, j, metered)
	})
}

// FailJob ends a job the engine cannot carry on (the module refused it for good) as the engine, in ONE transaction: confirmed, scheduled, running, cancelling, completing or paused -> failed with
// the reason (one line, at most 500 bytes, never an address); a result `infrastructure_failure` (rule P-04, free) for every row that has none; the release of the remainder of a metered
// module's reserve. What the module already settled stays charged. A job that already failed is returned as it is. A blank reason is ErrBadRequest.
func (s *Store) FailJob(ctx context.Context, jobID, reason string) (Job, error) {
	reason = cleanReason(reason)
	if reason == "" {
		return Job{}, opErr("fail", badRequest("a job is failed with a reason"))
	}
	return s.engineTx(ctx, "fail", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		if j.Status == StatusFailed {
			return nil
		}
		if err := s.engineMove(ctx, tx, j.ID, StatusFailed, reason); err != nil {
			return err
		}
		if _, err := s.writePlatformEnd(ctx, tx, j.ID, "infrastructure_failure", false); err != nil {
			return err
		}
		return s.releaseIn(ctx, tx, j, metered)
	})
}

// CancelUnsubmitted gives the pending rows of a CANCELLING job that no module was ever given (no submitted_at stamp, no result) the platform's own result `cancelled` (rule P-01, free) and returns
// how many it wrote: the module cancels only what it holds, so a row the engine never handed over (a list that was only partly accepted when the cancel came) would otherwise block the job from ever
// completing. The engine calls it only after the module reported complete and every item it held was recorded, so a row without a stamp that the module nevertheless holds cannot exist. Any other
// status is ErrNotAllowed.
func (s *Store) CancelUnsubmitted(ctx context.Context, jobID string) (int, error) {
	n := 0
	_, err := s.engineTx(ctx, "cancel unsubmitted", jobID, func(tx pgx.Tx, j Job, _ bool) error {
		if j.Status != StatusCancelling {
			return notAllowed(reasonNotCancelling)
		}
		var err error
		n, err = s.writePlatformEnd(ctx, tx, j.ID, "cancelled", true)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Release is the repair step: a terminal job whose reserve still has a remainder gives it back, in its own transaction (the lock first). A job that somebody ended OUTSIDE the engine (an administrator
// who moved it to failed, cancelled or expired by hand) may also have rows with no result: the repair writes the platform's own free result for each of them (infrastructure_failure, cancelled or expired,
// by how the job ended) so that a terminal job has exactly as many results as rows, then gives the remainder back. Idempotent: a job with nothing left, a job of a module that holds no credits and a job
// that holds no reserve are no-ops. A job that has not ended is ErrNotAllowed.
func (s *Store) Release(ctx context.Context, jobID string) error {
	_, err := s.engineTx(ctx, "release", jobID, func(tx pgx.Tx, j Job, metered bool) error {
		if !IsTerminal(j.Status) {
			return notAllowed(reasonNotEnded)
		}
		if state := endedResultState(j.Status); state != "" {
			if _, err := s.writePlatformEnd(ctx, tx, j.ID, state, false); err != nil {
				return err
			}
		}
		return s.releaseIn(ctx, tx, j, metered)
	})
	return err
}

// ---- annotations and events --------------------------------------------------------------------------------------------------------------------------------------------------------------

// NoteProgress sets last_progress_at = now() (the database's clock) on a job that holds credits or a module: an annotation, which the lifecycle allows without a move. ErrNotFound for an unknown
// job, ErrNotAllowed for one that is draft, previewed or ended.
func (s *Store) NoteProgress(ctx context.Context, jobID string) error {
	if !isUUID(jobID) {
		return opErr("note progress", ErrNotFound)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE platform.jobs SET last_progress_at = now()
		WHERE id = $1::uuid AND status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')`, jobID)
	if err != nil {
		return opErr("note progress", s.fail(err))
	}
	if tag.RowsAffected() == 0 {
		return opErr("note progress", s.whyNotWritten(ctx, jobID))
	}
	return nil
}

// whyNotWritten tells a statement that matched no row because the job does not exist (ErrNotFound) from one that matched none because of its status (ErrNotAllowed).
func (s *Store) whyNotWritten(ctx context.Context, jobID string) error {
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM platform.jobs WHERE id = $1::uuid`, jobID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return s.fail(err)
	}
	return notAllowed(reasonNotActive)
}

// AlertStale writes ONE `stale` job event (actor engine) and sets stale_alerted_at = now() when the job holds credits or a module and stale_alerted_at is NULL or older than `after` (the
// database's clock), in one statement, and says whether it wrote. A job that is not active, or was alerted less than `after` ago, is (false, nil): the alert is once per interval, not once per pass.
// The job stays exactly as it is: staleness is a report, never a move. ErrNotFound for an unknown job.
func (s *Store) AlertStale(ctx context.Context, jobID string, after time.Duration, detail map[string]any) (bool, error) {
	if !isUUID(jobID) {
		return false, opErr("alert stale", ErrNotFound)
	}
	if after < 0 {
		return false, opErr("alert stale", badRequest("the interval between alerts cannot be negative"))
	}
	tag, err := s.pool.Exec(ctx, `WITH upd AS (
			UPDATE platform.jobs SET stale_alerted_at = now()
			WHERE id = $1::uuid AND status IN ('confirmed', 'scheduled', 'running', 'cancelling', 'completing')
			  AND (stale_alerted_at IS NULL OR stale_alerted_at <= now() - make_interval(secs => $2::double precision))
			RETURNING id)
		INSERT INTO platform.job_events (job_id, kind, actor_kind, reason, detail)
		SELECT id, 'stale', 'engine', $3, $4::jsonb FROM upd`,
		jobID, after.Seconds(), "the job made no progress for longer than its module's staleness limit; it stays as it is and nothing is invented", detailJSON(detail))
	if err != nil {
		return false, opErr("alert stale", s.fail(err))
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	// nothing written: an unknown job is an error, a job that is not active or was alerted lately is simply "did not write"
	if err := s.whyNotWritten(ctx, jobID); err != nil && !errors.Is(err, ErrNotAllowed) {
		return false, opErr("alert stale", err)
	}
	return false, nil
}

// Flag writes a `flag` job event (actor engine) with the detail: the engine says something is not right and nothing was invented (a module that reports complete with rows unrecorded). The
// event's reason is detail["reason"] when that is a non-blank string, else a standard sentence. ErrNotFound for an unknown job.
func (s *Store) Flag(ctx context.Context, jobID string, detail map[string]any) error {
	if !isUUID(jobID) {
		return opErr("flag", ErrNotFound)
	}
	reason := "flagged by the engine"
	if v, ok := detail["reason"].(string); ok {
		if r := cleanReason(v); r != "" {
			reason = r
		}
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO platform.job_events (job_id, kind, actor_kind, reason, detail)
		SELECT j.id, 'flag', 'engine', $2, $3::jsonb FROM platform.jobs j WHERE j.id = $1::uuid`, jobID, reason, detailJSON(detail))
	if err != nil {
		return opErr("flag", s.fail(err))
	}
	if tag.RowsAffected() == 0 {
		return opErr("flag", ErrNotFound)
	}
	return nil
}
