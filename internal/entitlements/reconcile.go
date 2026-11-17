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
// module 3's admin route (SetSuspended), and read the state back through module 3's suspension read route
// (Suspension). *m3admin.Client is the real one; a test supplies a fake.
//
// THE READ IS THE CONFIRMATION. A 200 from SetSuspended means module 3 changed the state and a 422 conflict means
// it already was in that state, but neither is trusted as a convergence: a row converges only when Suspension, asked
// AFTER the call, says the state module 3 is in equals the wanted state (judgeRead), and the database refuses a
// convergence whose observation was not made at the wanted version (m3_sync_converged_only_when_observed_at_version).
type Suspender interface {
	SetSuspended(ctx context.Context, userID string, suspended bool, reason string) (m3admin.Result, error)
	Suspension(ctx context.Context, userID string) (m3admin.SuspensionState, error)
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
	sqlSelectUnconverged = `SELECT s.user_id::text, s.want_suspended, s.want_version, s.converged_version, s.attempts, s.last_attempt_at, s.updated_at, s.observed, s.observed_version, s.m3_version, (e.changed_by IS NULL)
		FROM platform.m3_sync s LEFT JOIN platform.m3_entitlements e ON e.user_id = s.user_id
		WHERE s.converged_version < s.want_version
		ORDER BY s.updated_at, s.user_id LIMIT $1`

	// The rows that are converged but have not been looked at for a while (the database's clock). The same
	// request is sent again: its answer tells whether module 3 still holds the wanted state.
	sqlSelectReverify = `SELECT s.user_id::text, s.want_suspended, s.want_version, s.converged_version, s.attempts, s.last_attempt_at, s.updated_at, s.observed, s.observed_version, s.m3_version, (e.changed_by IS NULL)
		FROM platform.m3_sync s LEFT JOIN platform.m3_entitlements e ON e.user_id = s.user_id
		WHERE s.converged_version = s.want_version
		  AND (s.observed_at IS NULL OR s.observed_at < now() - make_interval(secs => $1::double precision))
		ORDER BY s.observed_at NULLS FIRST, s.user_id LIMIT $2`

	// sqlConverge records a delivery confirmed by the READ-BACK: the state module 3's suspension route reported ($2,
	// equal to the wanted state), the version it was compared against ($3, which is also the version it converges
	// at) and module 3's own counter ($5). COMPARE-AND-SET on the version acted on: the row changes only while its
	// want_version and want_suspended are still the ones this attempt delivered ($3, $4). If the entitlement changed
	// while module 3 was being called or read, zero rows match and the stale observation marks nothing. The
	// database's CHECK m3_sync_converged_only_when_observed_at_version is the second wall (it refuses any
	// observation whose version is not the wanted one); the code does not rely on it (judgeRead never lets a state
	// other than the wanted one reach this statement).
	sqlConverge = `UPDATE platform.m3_sync
		SET observed = $2, observed_at = now(), observed_version = $3, m3_version = $5::bigint, attempts = 0, last_error = NULL, last_attempt_at = now(), converged_version = $3, updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3 AND want_suspended = $4`

	// sqlFail records a failed attempt (module 3 unreachable, refused, or unintelligible). It never touches
	// converged_version or observed. Guarded by the version like sqlConverge: a failure about a stale want
	// must not delay the new one.
	sqlFail = `UPDATE platform.m3_sync
		SET attempts = attempts + 1, last_attempt_at = now(), last_error = $2, updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3`

	// sqlMismatch records that module 3 ANSWERED, but with a state other than the wanted one (to the call, or to the
	// read-back): the answer is recorded as observed (it is what module 3 said) at the version it was compared
	// against ($3), with module 3's own counter when the answer came from the read route ($5, NULL for an answer to
	// the call, which keeps the last one), the attempt counts as a failure, and a row that had been converged is
	// taken back to "not converged" (converged_version below want_version), because module 3 has just contradicted
	// it. converged_version only ever goes DOWN here.
	sqlMismatch = `UPDATE platform.m3_sync
		SET observed = $2, observed_at = now(), observed_version = $3, m3_version = COALESCE($5::bigint, m3_version), attempts = attempts + 1, last_attempt_at = now(), last_error = $4,
		    converged_version = LEAST(converged_version, want_version - 1), updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3`

	// sqlPostAnswered records that the CALL was answered with the wanted state ($2) but the read-back could not be
	// completed ($4 says why): the answer is recorded as observed at its version ($3), the attempt counts as a
	// failure, and nothing converges. It is what lets the next pass finish with the read alone (postAnswered)
	// instead of making the call again. Guarded by the version like every statement here.
	sqlPostAnswered = `UPDATE platform.m3_sync
		SET observed = $2, observed_at = now(), observed_version = $3, attempts = attempts + 1, last_attempt_at = now(), last_error = $4, updated_at = now()
		WHERE user_id = $1::uuid AND want_version = $3`
)

// Report is what one RunOnce did. Every attempt ends in exactly one of Converged, Failed or StaleSkipped, so
// Attempted == Converged + Failed + StaleSkipped (Balanced).
type Report struct {
	// Attempted is the number of rows whose delivery to module 3 (the call and the read-back, or the read-back alone) was recorded.
	Attempted int
	// Converged: module 3's suspension route, read back, said it is in the wanted state and the database recorded it.
	Converged int
	// Failed: the call or the read-back failed, or module 3 answered (to the call or to the read) with another state; the row records the failure.
	Failed int
	// Drifted: a re-verification of an already converged user found module 3 NOT in the wanted state (its answer
	// said it changed): someone had un-suspended or re-suspended the user, and the call corrected it.
	Drifted int
	// StaleSkipped: the user's wanted state changed while module 3 was being called or read; the stale answer was not
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
	Observed         string
	ObservedVersion  *int64 // the want_version Observed was recorded at; nil before the first observation
	M3Version        *int64 // module 3's own counter as the read route last answered it; nil before the first read-back
	Auto             bool
}

// postAnswered is true when module 3's answer to the call for THIS want_version was already recorded as the wanted
// state (an earlier pass made the call, module 3 confirmed it, and the read-back could not be completed): the row is
// not converged, and only the read is missing. The pass then makes the read alone and no second call. It never holds
// for a row whose observation is of an older version (a stale observation confirms nothing) or of the other state
// (module 3 contradicted the wanted state, which is the call's business again).
func (row syncRow) postAnswered() bool {
	return row.ConvergedVersion < row.WantVersion && row.ObservedVersion != nil && *row.ObservedVersion == row.WantVersion &&
		row.Observed == wantedObservation(row.WantSuspended)
}

// alreadyDisagreed is true when the observation recorded for THIS want_version is a state that contradicts the
// wanted one: a disagreement an earlier pass already logged. It is what makes a stretch of passes that all find the
// same disagreement warn once (a new stretch starts when the row converges again, or wants a new version).
func (row syncRow) alreadyDisagreed() bool {
	return row.ObservedVersion != nil && *row.ObservedVersion == row.WantVersion &&
		(row.Observed == string(m3admin.StateSuspended) || row.Observed == string(m3admin.StateActive)) && row.Observed != wantedObservation(row.WantSuspended)
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
// (the same number at most), and for each row that is not waiting out its retry delay makes ONE call to module 3
// and reads the state back from its suspension route (attempt: a row whose call module 3 already confirmed needs
// the read alone). The Report says what happened; the returned error is for the reconciler's own failures (it
// could not read or write the database, or the pass was cancelled), never for a module 3 failure, which is data
// in the Report and in the row.
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
		if err := rows.Scan(&row.UserID, &row.WantSuspended, &row.WantVersion, &row.ConvergedVersion, &row.Attempts, &row.LastAttemptAt, &row.UpdatedAt, &row.Observed, &row.ObservedVersion, &row.M3Version, &row.Auto); err != nil {
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

// attempt delivers one row and records its outcome: the call that puts module 3 in the wanted state, then the
// READ-BACK of the state module 3 is in; only the read can converge the row. stop is true when the pass must not go
// on (module 3 rejected the admin key, the pass was cancelled, or the database could not be written).
//
//   - The call (SetSuspended) is skipped when the row postAnswered (module 3 already confirmed it for this version and
//     only the read is missing) and made again for a re-verification, whose point is to put a drifted module 3 right.
//   - A call that fails, or is answered with another state, is recorded as before (sqlFail, sqlMismatch).
//   - After a call that confirmed the wanted state, module 3's suspension route is read (judgeRead). The state it
//     reports equal to the wanted one: converged (sqlConverge: the observation, the version it was compared against
//     and module 3's own counter). Another state: recorded as what module 3 said and not converged (sqlMismatch),
//     warned about once per stretch. No answer (5xx, a transport error, an unknown user): not converged; for a first
//     delivery the call's confirmation is kept (sqlPostAnswered), so that the next pass only reads.
//   - Every write is guarded by the want_version this attempt acted on: when the entitlement changed meanwhile, the
//     observation is stale and is recorded as nothing.
func (r *Reconciler) attempt(ctx context.Context, row syncRow, reverify bool, rep *Report) (stop bool, err error) {
	var (
		post    verdict // the call's verdict; the zero value (outConverged) when the call was not made
		changed bool    // module 3 changed state on the call (a drift, when this is a re-verification)
	)
	readOnly := !reverify && row.postAnswered()
	if !readOnly {
		res, callErr := r.M3.SetSuspended(ctx, row.UserID, row.WantSuspended, reasonFor(row, reverify))
		if ctx.Err() != nil {
			// The pass ended while module 3 was being called: the outcome is unknown, so nothing is recorded. The
			// next pass repeats the call, which module 3 answers the same way.
			return true, ctx.Err()
		}
		post = judge(row.WantSuspended, res, callErr, r.Redact)
		changed = post.changed
	}

	// v is what is recorded: the call's verdict when the call did not confirm the wanted state, else the read's.
	v := post
	var m3Version *int64
	if post.kind == outConverged {
		st, readErr := r.M3.Suspension(ctx, row.UserID)
		if ctx.Err() != nil {
			return true, ctx.Err() // the same: nothing is recorded for a read the end of the pass interrupted
		}
		v = judgeRead(row.UserID, row.WantSuspended, st, readErr, r.Redact)
		if v.kind == outConverged || v.kind == outMismatch { // module 3 itself answered: its counter is data
			ver := st.Version
			m3Version = &ver
			if row.M3Version != nil && ver < *row.M3Version {
				r.log().WarnContext(ctx, "module 3's own suspension version went BACKWARDS (a restored module 3 database, or another module 3?): the state it reports is still judged, the counter is only recorded",
					"user", row.UserID, "was", *row.M3Version, "now", ver)
			}
		}
	}

	var affected int64
	switch v.kind {
	case outConverged:
		tag, werr := r.Pool.Exec(ctx, sqlConverge, row.UserID, string(v.state), row.WantVersion, row.WantSuspended, *m3Version)
		if werr != nil {
			return true, fmt.Errorf("record the delivery: %w", werr)
		}
		affected = tag.RowsAffected()
	case outMismatch:
		tag, werr := r.Pool.Exec(ctx, sqlMismatch, row.UserID, string(v.state), row.WantVersion, v.text, m3Version)
		if werr != nil {
			return true, fmt.Errorf("record the mismatch: %w", werr)
		}
		affected = tag.RowsAffected()
	default: // outFailed, outConfigFault
		sql, args := sqlFail, []any{row.UserID, v.text, row.WantVersion}
		if post.kind == outConverged && !readOnly && !reverify {
			// the call confirmed the wanted state and the read-back could not be made: keep the confirmation, so that the next pass reads and does not call again
			sql, args = sqlPostAnswered, []any{row.UserID, string(post.state), row.WantVersion, v.text}
		}
		tag, werr := r.Pool.Exec(ctx, sql, args...)
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
		if reverify && changed {
			rep.Drifted++
			rep.DriftedUsers = append(rep.DriftedUsers, row.UserID)
			log.WarnContext(ctx, "module 3 had drifted from the wanted state and was corrected by the re-verification", "state", string(v.state))
		}
		if stale {
			log.InfoContext(ctx, "module 3 was read in the wanted state for a version that has since changed: not recorded", "state", string(v.state))
		} else {
			log.InfoContext(ctx, "module 3 holds the wanted state (read back from its suspension route)", "state", string(v.state), "changed", changed, "reverify", reverify, "read_only", readOnly, "m3_version", *m3Version)
		}
		return false, nil
	}

	rep.addError(row.UserID, v.text)
	switch {
	case v.kind == outConfigFault:
		rep.ConfigFault = true
		log.ErrorContext(ctx, "module 3 rejected the admin key: CONFIGURATION FAULT, check PLATFORM_M3_ADMIN_TOKEN; the pass stops", "error", v.text)
		return true, nil
	case v.kind == outMismatch && post.kind != outConverged:
		log.ErrorContext(ctx, "module 3 answered with a state other than the wanted one: not trusted", "error", v.text, "answered", string(v.state))
	case v.kind == outMismatch:
		// the call was confirmed and the read-back says otherwise: warn at the first such read of a stretch, and quietly after it
		if row.alreadyDisagreed() {
			log.DebugContext(ctx, "module 3's suspension read still disagrees with the wanted state: not converged; will retry", "answered", string(v.state))
		} else {
			log.WarnContext(ctx, "module 3 confirmed the call but its suspension read says another state: not converged; will retry (warned once per stretch)", "error", v.text, "answered", string(v.state))
		}
	case post.kind == outConverged:
		log.WarnContext(ctx, "module 3 suspension read-back failed; will retry after the backoff", "error", v.text, "retryable", v.retryable, "attempts", row.Attempts+1)
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

// judge decides what a CALL (SetSuspended) proves: outConverged for exactly one case, a successful call whose
// answer is the wanted state. That is not yet a convergence: attempt reads the state back after such a call, and
// judgeRead decides the convergence. A mismatched, unrecognised or missing answer is never a convergence, and a
// transport error says nothing about module 3's state at all.
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

// judgeRead is the ONLY place that decides a convergence: outConverged for exactly one case, a successful read of
// module 3's suspension route for THIS user whose state equals the wanted one. A read that failed says nothing about
// module 3's state, and in particular a 404 (module 3 does not know the user) is a failure and never an "active"
// answer; a state read for another user, or for no user, is a failure; a state that is not the wanted one is a
// mismatch (module 3 said so). Every text is scrubbed of the secrets it is given.
func judgeRead(user string, wantSuspended bool, st m3admin.SuspensionState, readErr error, secrets []string) verdict {
	const prefix = "the suspension read-back failed: "
	switch {
	case errors.Is(readErr, m3admin.ErrUnauthorized):
		return verdict{kind: outConfigFault, text: "module 3 rejected the admin key (401) on the suspension read: check PLATFORM_M3_ADMIN_TOKEN"}
	case errors.Is(readErr, m3admin.ErrUnknownUser):
		return verdict{kind: outFailed, text: prefix + "module 3 does not know this user (404): there is no state to confirm"}
	case readErr != nil:
		return verdict{kind: outFailed, text: scrubText(prefix+readErr.Error(), secrets...), retryable: m3admin.Retryable(readErr)}
	case st.User == "":
		return verdict{kind: outFailed, text: prefix + "module 3 answered no user"}
	case st.User != user:
		return verdict{kind: outFailed, text: prefix + "module 3 answered for another user"}
	}
	want, got := stateFor(wantSuspended), st.State()
	if got == want {
		return verdict{kind: outConverged, state: want}
	}
	return verdict{kind: outMismatch, state: got,
		text: fmt.Sprintf("module 3's suspension read says %q but the wanted state is %q: not converged", string(got), string(want))}
}
