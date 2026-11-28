package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// jobColumns is the select list of a job, in the order scanJob reads it.
const jobColumns = `j.id::text, j.user_id::text, j.module_id, j.status, COALESCE(j.source_job_id::text, ''), j.scheduled_start, j.start_deadline,
	j.preview, COALESCE(j.preview_fingerprint, ''), COALESCE(j.confirm_key, ''), COALESCE(j.paused_from, ''), j.frozen_settings,
	j.previewed_at, j.confirmed_at, j.started_at, j.finished_at, j.cancel_requested_at, j.created_at, j.updated_at, j.content_cleared_at`

// frozenSettings is the shape of platform.jobs.frozen_settings: the settings, for a send job the campaign, and the names of the merge data of the job's rows (merge_fields.go; absent from a job made before
// they were kept). The content retention (platform_0021) rewrites only `content` of this document: every other key, this one included, stays as it was.
type frozenSettings struct {
	Settings    Settings `json:"settings"`
	Content     *Content `json:"content,omitempty"`
	MergeFields []string `json:"merge_fields,omitempty"`
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// scanJob reads one row of jobColumns.
func scanJob(row pgx.Row) (Job, error) { return scanJobWith(row) }

// scanJobWith reads one row of jobColumns followed by the columns the caller selected after them (extra: the engine's counts), scanned into the destinations given.
func scanJobWith(row pgx.Row, extra ...any) (Job, error) {
	var j Job
	var preview, frozen []byte
	dest := []any{&j.ID, &j.UserID, &j.Module, &j.Status, &j.SourceJobID, &j.ScheduledStart, &j.StartDeadline,
		&preview, &j.PreviewFingerprint, &j.ConfirmKey, &j.PausedFrom, &frozen,
		&j.PreviewedAt, &j.ConfirmedAt, &j.StartedAt, &j.FinishedAt, &j.CancelRequestedAt, &j.CreatedAt, &j.UpdatedAt, &j.ContentClearedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Job{}, err
	}
	j.ScheduledStart, j.StartDeadline, j.PreviewedAt, j.ConfirmedAt = utc(j.ScheduledStart), utc(j.StartDeadline), utc(j.PreviewedAt), utc(j.ConfirmedAt)
	j.StartedAt, j.FinishedAt, j.CancelRequestedAt = utc(j.StartedAt), utc(j.FinishedAt), utc(j.CancelRequestedAt)
	j.ContentClearedAt = utc(j.ContentClearedAt)
	j.CreatedAt, j.UpdatedAt = j.CreatedAt.UTC(), j.UpdatedAt.UTC()
	if len(preview) > 0 {
		var p Preview
		if err := json.Unmarshal(preview, &p); err != nil {
			return Job{}, fmt.Errorf("the stored preview of job %s is not a preview: %w", j.ID, err)
		}
		j.Preview = &p
	}
	var f frozenSettings
	if err := json.Unmarshal(frozen, &f); err != nil {
		return Job{}, fmt.Errorf("the frozen settings of job %s are not settings: %w", j.ID, err)
	}
	j.Settings, j.Content = f.Settings, f.Content // an empty choice stays empty: the user has not chosen (no default is invented here)
	j.MergeFields = f.MergeFields
	return j, nil
}

// getJob reads the user's job, or ErrNotFound. lock adds FOR UPDATE: the row is the caller's until its transaction ends.
func (s *Store) getJob(ctx context.Context, q querier, userID, jobID string, lock bool) (Job, error) {
	sql := `SELECT ` + jobColumns + ` FROM platform.jobs j WHERE j.id = $1::uuid AND j.user_id = $2::uuid`
	if lock {
		sql += ` FOR UPDATE`
	}
	j, err := scanJob(q.QueryRow(ctx, sql, jobID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, s.fail(err)
	}
	return j, nil
}

// Get returns the user's job. A job of another user is ErrNotFound, exactly as a job that does not exist.
func (s *Store) Get(ctx context.Context, userID, jobID string) (Job, error) {
	if err := checkUser(userID); err != nil {
		return Job{}, opErr("get", err)
	}
	if !isUUID(jobID) {
		return Job{}, opErr("get", ErrNotFound)
	}
	j, err := s.getJob(ctx, s.pool, userID, jobID, false)
	return j, opErr("get", err)
}

// List returns the user's jobs, newest first (created_at descending, then id), one page of at most opts.Limit (default 25, at most 100), and the cursor of the next page ("" on the last).
// A cursor that this store did not hand out is an ErrBadRequest.
func (s *Store) List(ctx context.Context, userID string, opts ListOpts) ([]Job, string, error) {
	if err := checkUser(userID); err != nil {
		return nil, "", opErr("list", err)
	}
	limit, err := pageLimit(opts.Limit, DefaultListLimit, MaxListLimit)
	if err != nil {
		return nil, "", opErr("list", err)
	}
	statuses, err := listStatusFilter(opts.Status) // nil: no filter (a NULL parameter keeps every status)
	if err != nil {
		return nil, "", opErr("list", err)
	}
	var rows pgx.Rows
	if opts.After == "" {
		rows, err = s.pool.Query(ctx, `SELECT `+jobColumns+` FROM platform.jobs j WHERE j.user_id = $1::uuid AND ($3::text[] IS NULL OR j.status = ANY($3::text[]))
			ORDER BY j.created_at DESC, j.id DESC LIMIT $2`, userID, limit+1, statuses)
	} else {
		at, id, derr := decodeCursor(opts.After)
		if derr != nil {
			return nil, "", opErr("list", derr)
		}
		rows, err = s.pool.Query(ctx, `SELECT `+jobColumns+` FROM platform.jobs j WHERE j.user_id = $1::uuid AND (j.created_at, j.id) < ($2::timestamptz, $3::uuid)
			AND ($5::text[] IS NULL OR j.status = ANY($5::text[]))
			ORDER BY j.created_at DESC, j.id DESC LIMIT $4`, userID, at, id, limit+1, statuses)
	}
	if err != nil {
		return nil, "", opErr("list", s.fail(err))
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, "", opErr("list", s.fail(err))
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, "", opErr("list", s.fail(err))
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Events returns the job's events with an id above afterID, oldest first, at most limit of them (default 100, at most 1000). The job must be the user's (ErrNotFound otherwise, also for a
// job that has no events yet, which is told apart from a job that does not exist by the job itself being read first).
func (s *Store) Events(ctx context.Context, userID, jobID string, afterID int64, limit int) ([]Event, error) {
	if err := checkUser(userID); err != nil {
		return nil, opErr("events", err)
	}
	if !isUUID(jobID) {
		return nil, opErr("events", ErrNotFound)
	}
	n, err := pageLimit(limit, DefaultEvents, MaxEvents)
	if err != nil {
		return nil, opErr("events", err)
	}
	if afterID < 0 {
		return nil, opErr("events", badRequest("the event cursor must not be negative"))
	}
	// one statement: the events of THE USER'S job (the join is the ownership check); an empty answer is told from an unknown job by a second read
	rows, err := s.pool.Query(ctx, `SELECT e.id, e.job_id::text, e.kind, COALESCE(e.from_status, ''), COALESCE(e.to_status, ''), e.actor_kind,
			COALESCE(e.acting_user_id::text, ''), COALESCE(e.acting_admin_id::text, ''), e.reason, e.detail, e.created_at
		FROM platform.job_events e JOIN platform.jobs j ON j.id = e.job_id
		WHERE e.job_id = $1::uuid AND j.user_id = $2::uuid AND e.id > $3
		ORDER BY e.id LIMIT $4`, jobID, userID, afterID, n)
	if err != nil {
		return nil, opErr("events", s.fail(err))
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.JobID, &e.Kind, &e.FromStatus, &e.ToStatus, &e.ActorKind, &e.ActingUserID, &e.ActingAdminID, &e.Reason, &e.Detail, &e.CreatedAt); err != nil {
			return nil, opErr("events", s.fail(err))
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr("events", s.fail(err))
	}
	if len(out) == 0 {
		if _, err := s.getJob(ctx, s.pool, userID, jobID, false); err != nil {
			return nil, opErr("events", err)
		}
	}
	return out, nil
}
