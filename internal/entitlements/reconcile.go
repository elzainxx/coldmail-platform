package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/m3admin"
)

// Suspender is what the reconciler needs of module 3: put one user in the suspended or the active state through
// module 3's admin route and say what module 3 answered. *m3admin.Client is the real one; a test supplies a fake.
//
// The answer is the confirmation: module 3 has no read route, so a 200 means it changed and a 422 conflict means
// it already was in that state, and both carry the state module 3 now reports in Result.State.
type Suspender interface {
	SetSuspended(ctx context.Context, userID string, suspended bool, reason string) (m3admin.Result, error)
}

// Reconcile bounds. Not dials: they keep one pass small, so that a slow module 3 cannot make a pass unbounded.
const (
	// reconcileBatch is how many never-converged rows, and separately how many re-verifications, one pass takes.
	reconcileBatch = 50
	// maxReportErrors caps Report.Errors.
	maxReportErrors = 20
)

// The statements of the reconciler. Every one names its schema.
const (
	// The rows module 3 has never been seen to agree with, oldest change first (a failure moves a row to the back,
	// so one stuck user cannot starve the rest). `auto` is true when the entitlement ended by itself (the expiry
	// writes changed_by NULL), which only chooses the wording of the audit note sent to module 3.
	sqlSelectUnconverged = `SELECT s.user_id::text, s.want_suspended, s.want_version, s.converged_version, s.attempts, s.last_attempt_at, s.updated_at, (e.changed_by IS NULL)
		FROM platform.m3_sync s LEFT JOIN platform.m3_entitlements e ON e.user_id = s.user_id
		WHERE s.converged_version < s.want_version
		ORDER BY s.updated_at, s.user_id LIMIT $1`

	// The rows that are converged but have not been looked at for a while (the database's clock). The same
	// request is sent again: its answer tells whether module 3 still holds the wanted state.
	sqlSelectReverify = `SELECT s.user_id::text, s.want_suspended, s.want_version, s.converged_version, s.attempts, s.last_attempt_at, s.updated_at, (e.changed_by IS NULL)
		FROM platform.m3_sync s LEFT JOIN platform.m3_entitlements e ON e.user_id = s.user_id
		WHERE s.converged_version = s.want_version
		  AND (s.observed_at IS NULL OR s.observed_at < now() - make_interval(secs => $1::double precision))
		ORDER BY s.observed_at NULLS FIRST, s.user_id LIMIT $2`

	// sqlConverge records a confirmed delivery. COMPARE-AND-SET on the version acted on: the row changes only
	// while its want_version and want_suspended are still the ones this attempt delivered ($3, $4). If the
	// entitlement changed while module 3 was being called, zero rows match and the stale answer marks nothing.
	// The database's CHECK m3_sync_converged_only_when_observed is the second wall; the code does not rely on it
	// (judge never lets a state other than the wanted one reach this statement).
	sqlConverge = `UPDATE platform.m3_sync
		SET observed = $2, observed_at = now(), attempts = 0, last_error = NULL, last_attempt_at = now(), converged_version = $3, updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3 AND want_suspended = $4`

	// sqlFail records a failed attempt (module 3 unreachable, refused, or unintelligible). It never touches
	// converged_version or observed. Guarded by the version like sqlConverge: a failure about a stale want
	// must not delay the new one.
	sqlFail = `UPDATE platform.m3_sync
		SET attempts = attempts + 1, last_attempt_at = now(), last_error = $2, updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3`

	// sqlMismatch records that module 3 ANSWERED, but with a state other than the wanted one: the answer is
	// recorded as observed (it is what module 3 said), the attempt counts as a failure, and a row that had been
	// converged is taken back to "not converged" (converged_version below want_version), because module 3 has
	// just contradicted it. converged_version only ever goes DOWN here.
	sqlMismatch = `UPDATE platform.m3_sync
		SET observed = $2, observed_at = now(), attempts = attempts + 1, last_attempt_at = now(), last_error = $4,
		    converged_version = LEAST(converged_version, want_version - 1), updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3`
)

// Report is what one RunOnce did. Every attempt ends in exactly one of Converged, Failed or StaleSkipped, so
// Attempted == Converged + Failed + StaleSkipped (Balanced).
type Report struct {
	// Attempted is the number of calls to module 3 whose outcome was recorded.
	Attempted int
	// Converged: module 3 answered with the wanted state and the database recorded it.
	Converged int
	// Failed: the call failed, or module 3 answered with another state; the row records the failure.
	Failed int
	// Drifted: a re-verification of an already converged user found module 3 NOT in the wanted state (its answer
	// said it changed): someone had un-suspended or re-suspended the user, and the call corrected it.
	Drifted int
	// StaleSkipped: the user's wanted state changed while module 3 was being called; the stale answer was not
	// recorded and the next pass handles the new version.
	StaleSkipped int
	// BackoffSkipped counts rows left alone because their retry delay had not passed.
	BackoffSkipped int
	// Unconverged is the number of users still not converged after the pass; -1 when it could not be read.
	Unconverged int
	// ConfigFault is true when module 3 rejected the admin key (401): a configuration problem, not a failure of
	// one user. The pass stops at the first one.
	ConfigFault bool
	// DriftedUsers names the users counted in Drifted.
	DriftedUsers []string
	// Errors holds the scrubbed text of each failure, "<user>: <text>", at most maxReportErrors.
	Errors []string
}

// Balanced is true when the counters add up: every attempt has exactly one outcome.
func (r Report) Balanced() bool { return r.Attempted == r.Converged+r.Failed+r.StaleSkipped }

func (r *Report) addError(user, text string) {
	if len(r.Errors) < maxReportErrors {
		r.Errors = append(r.Errors, user+": "+text)
	}
}

// DefaultBackoff is the retry delay after a failed attempt: 2^attempts seconds, at most 60. Plain arithmetic, no
// jitter, so the schedule is the same on every run.
func DefaultBackoff(attempts int) time.Duration {
	const ceiling = 60 * time.Second
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= 6 { // 2^6 = 64 s is already over the cap; this also keeps the shift far from overflow
		return ceiling
	}
	return min(time.Duration(1<<attempts)*time.Second, ceiling)
}

// Reconciler delivers each user's wanted module-3 state and records what module 3 answered.
type Reconciler struct {
	Pool *pgxpool.Pool
	M3   Suspender
	// Now is the clock of the retry arithmetic ONLY (last_attempt_at + backoff against now). Entitlement truth
	// is never read from it. Defaults to time.Now.
	Now func() time.Time
	// Backoff is the delay after a failed attempt, by the number of failures so far. Defaults to DefaultBackoff.
	Backoff func(attempts int) time.Duration
	// ReverifyEvery is how stale a converged row's observation may get (database clock) before the same request
	// is sent again to confirm module 3 still holds the wanted state. Zero or negative turns re-verification off.
	ReverifyEvery time.Duration
	Log           *slog.Logger
	// Redact lists secrets (module 3's admin key) that are removed from every error text before it is stored,
	// reported or logged.
	Redact []string
}

type syncRow struct {
	UserID           string
	WantSuspended    bool
	WantVersion      int64
	ConvergedVersion int64
	Attempts         int
	LastAttemptAt    *time.Time
	UpdatedAt        time.Time
	Auto             bool
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) backoff(attempts int) time.Duration {
	if r.Backoff != nil {
		return r.Backoff(attempts)
	}
	return DefaultBackoff(attempts)
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// RunOnce makes one pass: it lists the users module 3 has not been seen to agree with (at most reconcileBatch,
// oldest change first) and, when ReverifyEvery is set, the converged users not looked at for ReverifyEvery
// (the same number at most), and makes ONE call to module 3 for each row that is not waiting out its retry
// delay. The Report says what happened; the returned error is for the reconciler's own failures (it could not
// read or write the database, or the pass was cancelled), never for a module 3 failure, which is data in the
// Report and in the row.
func (r *Reconciler) RunOnce(ctx context.Context) (Report, error) {
	rep := Report{Unconverged: -1}
	if r == nil || r.Pool == nil || r.M3 == nil {
		return rep, errors.New("entitlements: a Reconciler needs a Pool and an M3 client")
	}

	pending, err := r.list(ctx, sqlSelectUnconverged, reconcileBatch)
	if err != nil {
		return rep, fmt.Errorf("entitlements: reconcile: list the unconverged users: %w", err)
	}
	type item struct {
		row      syncRow
		reverify bool
	}
	work := make([]item, 0, len(pending))
	for _, row := range pending {
		work = append(work, item{row, false})
	}
	var errs []error
	if r.ReverifyEvery > 0 {
		stale, err := r.list(ctx, sqlSelectReverify, r.ReverifyEvery.Seconds(), reconcileBatch)
		if err != nil {
			errs = append(errs, fmt.Errorf("entitlements: reconcile: list the users to re-verify: %w", err))
		}
		for _, row := range stale {
			work = append(work, item{row, true})
		}
	}

	for _, it := range work {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if r.waitingOutBackoff(it.row) {
			rep.BackoffSkipped++
			continue
		}
		stop, err := r.attempt(ctx, it.row, it.reverify, &rep)
		if err != nil {
			errs = append(errs, fmt.Errorf("entitlements: reconcile %s: %w", it.row.UserID, err))
		}
		if stop {
			break
		}
	}

	if ctx.Err() == nil {
		if n, err := countUnconverged(ctx, r.Pool); err == nil {
			rep.Unconverged = int(n)
		} else {
			errs = append(errs, err)
		}
	}
	return rep, errors.Join(errs...)
}

// list reads sync rows with one of the two select statements; args are its parameters.
func (r *Reconciler) list(ctx context.Context, sql string, args ...any) ([]syncRow, error) {
	rows, err := r.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []syncRow
	for rows.Next() {
		var row syncRow
		if err := rows.Scan(&row.UserID, &row.WantSuspended, &row.WantVersion, &row.ConvergedVersion, &row.Attempts, &row.LastAttemptAt, &row.UpdatedAt, &row.Auto); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// waitingOutBackoff is true when the row failed recently enough that its retry delay has not passed. A row
// whose wanted state has CHANGED since the last attempt (the entitlement trigger bumps updated_at, while the
// reconciler writes updated_at and last_attempt_at with the same now()) has not failed at its new version at
// all, so it is never held back by the delay of the old one.
func (r *Reconciler) waitingOutBackoff(row syncRow) bool {
	if row.LastAttemptAt == nil || row.UpdatedAt.After(*row.LastAttemptAt) {
		return false
	}
	return r.now().Before(row.LastAttemptAt.Add(r.backoff(row.Attempts)))
}

// attempt makes the one call for a row and records its outcome. stop is true when the pass must not go on (module
// 3 rejected the admin key, the pass was cancelled, or the database could not be written).
func (r *Reconciler) attempt(ctx context.Context, row syncRow, reverify bool, rep *Report) (stop bool, err error) {
	res, callErr := r.M3.SetSuspended(ctx, row.UserID, row.WantSuspended, reasonFor(row, reverify))
	if ctx.Err() != nil {
		// The pass ended while module 3 was being called: the outcome is unknown, so nothing is recorded. The
		// next pass repeats the call, which module 3 answers the same way.
		return true, ctx.Err()
	}
	v := judge(row.WantSuspended, res, callErr, r.Redact)

	var affected int64
	switch v.kind {
	case outConverged:
		tag, werr := r.Pool.Exec(ctx, sqlConverge, row.UserID, string(v.state), row.WantVersion, row.WantSuspended)
		if werr != nil {
			return true, fmt.Errorf("record the delivery: %w", werr)
		}
		affected = tag.RowsAffected()
	case outMismatch:
		tag, werr := r.Pool.Exec(ctx, sqlMismatch, row.UserID, string(v.state), row.WantVersion, v.text)
		if werr != nil {
			return true, fmt.Errorf("record the mismatch: %w", werr)
		}
		affected = tag.RowsAffected()
	default: // outFailed, outConfigFault
		tag, werr := r.Pool.Exec(ctx, sqlFail, row.UserID, v.text, row.WantVersion)
		if werr != nil {
			return true, fmt.Errorf("record the failure: %w", werr)
		}
		affected = tag.RowsAffected()
	}

	rep.Attempted++
	stale := affected == 0
	switch {
	case v.kind == outConverged && stale:
		rep.StaleSkipped++
	case v.kind == outConverged:
		rep.Converged++
	case stale:
		rep.StaleSkipped++
	default:
		rep.Failed++
	}

	log := r.log().With("user", row.UserID, "want_suspended", row.WantSuspended, "want_version", row.WantVersion)
	if v.kind == outConverged {
		if reverify && v.changed {
			rep.Drifted++
			rep.DriftedUsers = append(rep.DriftedUsers, row.UserID)
			log.WarnContext(ctx, "module 3 had drifted from the wanted state and was corrected by the re-verification", "state", string(v.state))
		}
		if stale {
			log.InfoContext(ctx, "module 3 answered for a version that has since changed: not recorded", "state", string(v.state))
		} else {
			log.InfoContext(ctx, "module 3 holds the wanted state", "state", string(v.state), "changed", v.changed, "reverify", reverify)
		}
		return false, nil
	}

	rep.addError(row.UserID, v.text)
	switch v.kind {
	case outConfigFault:
		rep.ConfigFault = true
		log.ErrorContext(ctx, "module 3 rejected the admin key: CONFIGURATION FAULT, check PLATFORM_M3_ADMIN_TOKEN; the pass stops", "error", v.text)
		return true, nil
	case outMismatch:
		log.ErrorContext(ctx, "module 3 answered with a state other than the wanted one: not trusted", "error", v.text, "answered", string(v.state))
	default:
		log.WarnContext(ctx, "module 3 call failed; will retry after the backoff", "error", v.text, "retryable", v.retryable, "attempts", row.Attempts+1)
	}
	return false, nil
}

// reasonFor is the audit note module 3 stores when the call changes something.
func reasonFor(row syncRow, reverify bool) string {
	s := "platform entitlement: enabled"
	switch {
	case row.WantSuspended && row.Auto:
		s = "platform entitlement: expired"
	case row.WantSuspended:
		s = "platform entitlement: disabled"
	}
	if reverify {
		s += " (re-verify)"
	}
	return s
}

// ---- the decision, as a pure function ----------------------------------------------------------------------------

type outcome int

const (
	// outConverged: module 3 answered, and with exactly the wanted state.
	outConverged outcome = iota
	// outMismatch: module 3 answered with the other recognised state.
	outMismatch
	// outFailed: no usable answer.
	outFailed
	// outConfigFault: module 3 rejected the credential.
	outConfigFault
)

type verdict struct {
	kind      outcome
	state     m3admin.State // the recognised state module 3 answered with (converged, mismatch)
	changed   bool
	text      string // the scrubbed explanation stored as last_error (failures); empty for a convergence
	retryable bool
}

// stateFor is the state module 3 must be seen in for the wanted suspension.
func stateFor(wantSuspended bool) m3admin.State {
	if wantSuspended {
		return m3admin.StateSuspended
	}
	return m3admin.StateActive
}

// judge is the ONLY place that decides whether a call confirmed the wanted state: convergence is returned for
// exactly one case, a successful call whose answer is the wanted state. A mismatched, unrecognised or missing
// answer is never a convergence, and a transport error says nothing about module 3's state at all.
func judge(wantSuspended bool, res m3admin.Result, callErr error, secrets []string) verdict {
	if callErr != nil {
		if errors.Is(callErr, m3admin.ErrUnauthorized) {
			return verdict{kind: outConfigFault, text: "module 3 rejected the admin key (401): check PLATFORM_M3_ADMIN_TOKEN"}
		}
		return verdict{kind: outFailed, text: scrubError(callErr, secrets...), retryable: m3admin.Retryable(callErr)}
	}
	want := stateFor(wantSuspended)
	switch res.State {
	case want:
		return verdict{kind: outConverged, state: want, changed: res.Changed}
	case m3admin.StateSuspended, m3admin.StateActive:
		return verdict{kind: outMismatch, state: res.State,
			text: fmt.Sprintf("module 3 answered %q but the wanted state is %q: the answer is not trusted", string(res.State), string(want))}
	}
	return verdict{kind: outFailed, text: "module 3 answered with a state this platform does not recognise"}
}
