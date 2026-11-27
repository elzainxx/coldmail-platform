package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/faultpoint"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/suppress"
)

// Confirm turns a previewed job into a confirmed (or, with a start time, a scheduled) one, in ONE transaction (Locked 24). The steps, in this order:
//
//  0. the key is validated (1 to 128 characters of letters, digits and . _ : -) and, before the transaction, the cheap replay is answered: a job of this user that already holds the key is
//     returned as Replayed when it is this job (its status as it is now, nothing written) and ErrConfirmKeyReused when it is another. A module 3 job is pre-checked (PreChecks.M3Feasibility,
//     never an HTTP call under the lock). Before that, a SEND job (m2, m3) in a store that has no unsubscribe link maker (WithUnsubscribe) is ErrNotAllowed with ReasonUnsubscribeNotConfigured: a send
//     without a working link is not allowed (a verify job is not asked). The module could not be asked is ErrModuleUnavailable for an immediate confirm (ignored for a scheduled one), a campaign it cannot run is ErrInfeasible.
//     A module 1 job reads module 1's vendor reachability in the same place (syncVendor, vendor.go): the stored flag becomes module 1's current answer, so the reserve below is made at the rate it can
//     really charge, and a price the platform cannot know (module 1 unreadable and the flag off but never set by module 1) is refused the same way for an immediate confirm.
//  1. BEGIN; the (user, module) advisory lock FIRST (money.Store.LockIn), then the job row FOR UPDATE, scoped by the user (ErrNotFound). The job must be previewed: one that was already confirmed
//     with this key is the replay the cheap read could not see (two requests racing), any other status is ErrNotPreviewed.
//  2. the module's price rows are held FOR SHARE and the fingerprint is recomputed exactly as Preview computes it; it must equal both the one the client sent and the one stored, else
//     ErrStalePreview (the list, the price of a reachable unit, the content, the choice or the source job's results changed). Just before that, a chained send whose source job holds `unknown`
//     results needs the user's explicit choice (ConfirmReq.UnknownChoice): none is ErrUnknownChoiceRequired (with the number of unknown results; nothing was written), one that is not the choice
//     the job was previewed under is a StalePreviewError (reason unknown_choice_changed): the user previews again under the choice they want. It is judged BEFORE the fingerprint, so that the
//     refusal names its cause. A job that is not chained takes no choice (a bad request).
//  3. the user's suppression list is applied NOW, as the module's kind of job needs (owner ruling OD-3). A SEND job (m2, m3): the pending rows whose address is on the list become state suppressed
//     (and carry the flag), are left out of the reserve and never reach the module; no pending row left is ErrNothingToDo. A VERIFY job (m1): the same rows only get the flag, stay pending, are
//     verified and charged like any other row and are flagged `suppressed` in the results; a verify list that the suppression covers entirely is not ErrNothingToDo (every row is verified).
//     THEN, for a SEND job in a store with a template renderer (WithPreparer), the rows the templates cannot be rendered for are left out the same way (template_scan.go): a pending row that lacks a merge
//     field its templates need and give no fallback for becomes state missing_field (reason: the field names), and a pending row whose rendered message the shared table refuses (checkRendered, the very
//     render EnsureJobItems will make) becomes state unrenderable (reason: the word of the refusal). Both are left out of the reserve, never given a public.job_items row and written as free platform
//     results in step 6 (P-09, P-10); no pending row left (all suppressed, missing or unrenderable) is ErrNothingToDo and the transaction rolls back, the suppression pass with it. A verify job has
//     no templates and is not touched.
//  4. a metered module (m1, m2): money.ReserveIn holds credits for exactly the rows that reach the module (a verify job's suppressed rows among them), freezing the price snapshot of the reachable units; a list the balance does not cover is
//     ErrInsufficientCredits with the exact shortfall and the transaction rolls back. Module 3: the entitlement must be enabled and unexpired by the database's clock (ErrNoEntitlement) and the
//     user not observed suspended (ErrSuspended).
//  5. ONE UPDATE moves the job (previewed -> confirmed, or -> scheduled when ScheduledStart is given: between 1 minute and 366 days ahead by the database's clock, else ErrBadRequest) and sets
//     confirm_key, scheduled_start and start_deadline = coalesce(scheduled_start, now()) + the max start delay, after the three transaction-local settings (actor user, the user's id, the
//     reason): the guard sets confirmed_at itself. The slot is the unique index jobs_one_active_per_user_module: a loser gets ErrSlotTaken (23505) and the whole transaction rolls back, the
//     reserve with it; a key held by another job is ErrConfirmKeyReused (23505).
//  6. AFTER the move (the table refuses results before the job is confirmed) the platform's own results are written, free, one per row that is not pending: malformed (P-08), duplicate (P-07),
//     suppressed (P-SUP), excluded (P-EXC), missing_field (P-09) and unrenderable (P-10). Their suppressed flag is derived from the row by the database (platform_0014), not stated here.
//
// The slot is not checked in Go, on purpose: the index is what decides (the concurrency test shows both confirms succeed when it is dropped).
func (s *Store) Confirm(ctx context.Context, userID string, req ConfirmReq) (ConfirmResult, error) {
	if err := checkUser(userID); err != nil {
		return ConfirmResult{}, opErr("confirm", err)
	}
	if !isUUID(req.JobID) {
		return ConfirmResult{}, opErr("confirm", ErrNotFound)
	}
	if !validKey(req.Key) {
		return ConfirmResult{}, opErr("confirm", badRequest("the confirm key must be 1 to 128 characters of letters, digits and . _ : -"))
	}
	if !validFingerprint(req.Fingerprint) {
		return ConfirmResult{}, opErr("confirm", badRequest("the fingerprint must be 64 lower-case hex digits"))
	}
	if req.UnknownChoice != "" && !validChoice(req.UnknownChoice) {
		return ConfirmResult{}, opErr("confirm", badRequest("unknown_choice must be %q or %q", UnknownInclude, UnknownExclude))
	}

	// 0. the cheap replay, outside the transaction
	var holder string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM platform.jobs WHERE user_id = $1::uuid AND confirm_key = $2`, userID, req.Key).Scan(&holder)
	switch {
	case err == nil && holder != req.JobID:
		return ConfirmResult{}, opErr("confirm", ErrConfirmKeyReused)
	case err == nil:
		j, err := s.getJob(ctx, s.pool, userID, req.JobID, false)
		if err != nil {
			return ConfirmResult{}, opErr("confirm", err)
		}
		res, err := s.replayResult(ctx, s.pool, j)
		return res, opErr("confirm", err)
	case !errors.Is(err, pgx.ErrNoRows):
		return ConfirmResult{}, opErr("confirm", s.fail(err))
	}
	// a send job needs a working unsubscribe link (Phase 5 S3): a store that cannot make one refuses it here, before module 3 is asked anything and before anything is held (unsubscribe.go); a replay
	// of a job that was confirmed under a key is answered above, whatever the configuration is now
	if err := s.requireUnsubscribe(ctx, userID, req.JobID); err != nil {
		s.log.Info("confirm refused", "job", req.JobID, "reason", refusalWord(err))
		return ConfirmResult{}, opErr("confirm", err)
	}
	if err := s.preCheck(ctx, userID, req); err != nil {
		return ConfirmResult{}, opErr("confirm", err)
	}
	// a module 1 job: module 1's vendor reachability is read now, before the transaction, and the stored flag made equal to it, so that the reserve below is made at the rate module 1 can really
	// charge; an immediate confirm that module 1 cannot price (unreadable, and the flag off but never module 1's word) is refused here, with nothing held (vendor.go has the whole rule)
	if _, err := s.syncVendor(ctx, userID, req.JobID, true, req.ScheduledStart == nil); err != nil {
		s.log.Info("confirm refused", "job", req.JobID, "reason", refusalWord(err))
		return ConfirmResult{}, opErr("confirm", err)
	}

	var res ConfirmResult
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// 1. the lock first (it is named by the job's module, which never changes), then the job row
		var module string
		if err := tx.QueryRow(ctx, `SELECT module_id FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, req.JobID, userID).Scan(&module); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return s.fail(err)
		}
		if err := s.mny.LockIn(ctx, tx, userID, module); err != nil {
			return err
		}
		j, err := s.getJob(ctx, tx, userID, req.JobID, true)
		if err != nil {
			return err
		}
		if j.Status != StatusPreviewed {
			if j.ConfirmKey == req.Key { // confirmed with this very key while this request waited: the replay
				r, err := s.replayResult(ctx, tx, j)
				if err != nil {
					return err
				}
				res = r
				return nil
			}
			return ErrNotPreviewed
		}
		if req.ScheduledStart != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT $1::timestamptz >= now() + make_interval(secs => $2::double precision) AND $1::timestamptz <= now() + make_interval(secs => $3::double precision)`,
				*req.ScheduledStart, MinScheduleAhead.Seconds(), MaxScheduleAhead.Seconds()).Scan(&ok); err != nil {
				return s.fail(err)
			}
			if !ok {
				return badRequest("a scheduled start must be between 1 minute and 366 days from now")
			}
		}

		// 2. the user's choice for the unknown verdicts, then the stale-preview check, over the prices this transaction now holds
		sn, err := s.takeSnapshot(ctx, tx, j, true)
		if err != nil {
			return err
		}
		if err := checkUnknownChoice(j.SourceJobID != "", j.Settings.UnknownChoice, sn.verdicts[VerdictUnknown], req.UnknownChoice); err != nil {
			return err
		}
		if sn.fingerprint != req.Fingerprint || sn.fingerprint != j.PreviewFingerprint {
			return ErrStalePreview
		}

		// 3. the suppression list, applied now: a send job leaves the rows out, a verify job verifies them and flags them
		found, err := suppress.Filter(ctx, tx, userID, sn.pendingEmails)
		if err != nil {
			return err
		}
		var suppressed, pending, missing, unrenderable int
		if j.Module == ModuleVerify {
			if suppressed, err = s.flagRows(ctx, tx, j.ID, userID, found); err != nil {
				return err
			}
			pending = sn.pending // every pending row reaches module 1, the suppressed ones too
		} else {
			if suppressed, err = s.suppressRows(ctx, tx, j.ID, userID, found); err != nil {
				return err
			}
			pending = sn.pending - suppressed
			// 3b. the templates (a send job): the rows that lack a required field or render to a message the table refuses are left out now, in this transaction, next to the suppression list
			if j.Content != nil && s.prep != nil && pending > 0 {
				prep, err := s.prepare(j.ID, *j.Content)
				if err != nil {
					return err
				}
				ts, err := s.scanTemplates(ctx, tx, j, prep, scanOpts{apply: true})
				if err != nil {
					return err
				}
				missing, unrenderable = ts.missing, ts.unrenderable
				pending -= missing + unrenderable
			}
		}
		if pending < 1 {
			return ErrNothingToDo
		}

		// 4. the credits (m1, m2) or the entitlement (m3)
		var reserved int64
		if sn.metered {
			rr, err := s.mny.ReserveIn(ctx, tx, money.ReserveReq{JobID: j.ID, Units: pending})
			if err != nil {
				return err
			}
			reserved = rr.Reserved
		} else if err := s.checkEntitlement(ctx, tx, userID); err != nil {
			return err
		}

		// 5. the move: one statement, after the three settings
		to, reason := StatusConfirmed, "confirmed by the user"
		if req.ScheduledStart != nil {
			to, reason = StatusScheduled, "scheduled by the user"
		}
		if err := s.setActor(ctx, tx, "user", userID, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.jobs SET status = $3, confirm_key = $4, scheduled_start = $5::timestamptz,
				start_deadline = COALESCE($5::timestamptz, now()) + make_interval(secs => $6::double precision)
			WHERE id = $1::uuid AND user_id = $2::uuid`, j.ID, userID, to, req.Key, req.ScheduledStart, s.maxStartDelay.Seconds()); err != nil {
			return s.fail(err)
		}
		if err := s.clearActor(ctx, tx); err != nil {
			return err
		}

		// 6. the platform's own results, now that the job is confirmed
		if err := s.writePlatformResults(ctx, tx, j.ID, userID); err != nil {
			return err
		}
		confirmed, err := s.getJob(ctx, tx, userID, j.ID, false)
		if err != nil {
			return err
		}
		res = ConfirmResult{Job: confirmed, Reserved: reserved, Pending: pending, Suppressed: suppressed + sn.suppressed, MissingFields: missing, Unrenderable: unrenderable}
		faultpoint.Hit("confirm.before_commit")
		return nil
	})
	if err != nil {
		s.log.Info("confirm refused", "job", req.JobID, "reason", refusalWord(err))
		return ConfirmResult{}, opErr("confirm", err)
	}
	s.log.Info("confirm done", "job", res.Job.ID, "module", res.Job.Module, "status", res.Job.Status, "replayed", res.Replayed, "reserved", res.Reserved, "pending", res.Pending, "suppressed", res.Suppressed, "missing_fields", res.MissingFields, "unrenderable", res.Unrenderable)
	return res, nil
}

// preCheck is the module 3 pre-check, made BEFORE the transaction. It runs only for a previewed module 3 job when the store has PreChecks; every other case is judged by the transaction.
func (s *Store) preCheck(ctx context.Context, userID string, req ConfirmReq) error {
	if s.pre == nil {
		return nil
	}
	j, err := s.getJob(ctx, s.pool, userID, req.JobID, false)
	if err != nil {
		return err
	}
	if j.Module != ModuleSendBYO || j.Status != StatusPreviewed || j.Preview == nil {
		return nil
	}
	// a request the transaction will refuse for its unknown choice does not ask the module anything (the stored preview knows how many unknown results the source holds)
	if err := checkUnknownChoice(j.SourceJobID != "", j.Settings.UnknownChoice, j.Preview.UnknownCount, req.UnknownChoice); err != nil {
		return err
	}
	f, err := s.pre.M3Feasibility(ctx, userID, j.ID, j.Preview.Units)
	if err != nil {
		s.log.Warn("the module 3 pre-check failed", "job", j.ID)
		return fmt.Errorf("the module 3 pre-check failed: %w", err)
	}
	if f.Unavailable {
		s.log.Info("module 3 could not be asked", "job", j.ID, "scheduled", req.ScheduledStart != nil)
	}
	switch {
	case f.Unavailable && req.ScheduledStart == nil:
		return ErrModuleUnavailable
	case f.Unavailable: // a scheduled job is not pre-checked against availability: the module may be back by its start
		return nil
	case !f.Feasible:
		return ErrInfeasible{Causes: append([]string(nil), f.Causes...)}
	}
	return nil
}

// checkUnknownChoice is the rule of the user's choice for the `unknown` verdicts of a chained send, in one place for the transaction (which decides) and the module 3 pre-check (which only avoids
// asking the module for a request the transaction will refuse). chained says whether the job has a source job; recorded is the choice the previewed job was classified under ("" when none was
// made); unknown is how many results of the source are `unknown`; requested is what the confirm carries.
//
//   - not chained: a choice is a bad request (there is nothing to choose), none is fine
//   - chained, no unknown result: nothing to choose; a choice (a valid one: the request was checked) is accepted and changes nothing
//   - chained, some unknown, no choice: ErrUnknownChoiceRequired with the count (never a default)
//   - chained, some unknown, a choice that is not the recorded one: a StalePreviewError (unknown_choice_changed): the user never saw the list under that choice, so they preview again
func checkUnknownChoice(chained bool, recorded string, unknown int, requested string) error {
	switch {
	case !chained && requested != "":
		return badRequest("unknown_choice only applies to a send chained to a verify job")
	case !chained, unknown == 0:
		return nil
	case requested == "":
		return &UnknownChoiceRequiredError{Unknown: unknown}
	case requested != recorded:
		return &StalePreviewError{Reason: ReasonUnknownChoiceChanged}
	}
	return nil
}

// suppressRows turns the job's pending rows whose address the user suppresses into suppressed rows (the job is previewed, so its rows may still change) and returns how many it changed. It is
// for a SEND job: the row's state and its suppressed flag change in one statement (the database ties them: a suppressed row carries the flag), and the row never reaches the module.
func (s *Store) suppressRows(ctx context.Context, tx pgx.Tx, jobID, userID string, found map[string]bool) (int, error) {
	return s.markRows(ctx, tx, jobID, userID, found, `UPDATE platform.job_rows SET state = 'suppressed', suppressed = true
			WHERE job_id = $1::uuid AND user_id = $2::uuid AND state = 'pending' AND email_norm = ANY($3::text[])`)
}

// flagRows sets the suppressed flag on the job's pending rows whose address the user suppresses and returns how many it flagged. It is for a VERIFY job (owner ruling OD-3): the rows stay pending,
// are verified and charged like any other row, and the flag reaches the results; the state does not change.
func (s *Store) flagRows(ctx context.Context, tx pgx.Tx, jobID, userID string, found map[string]bool) (int, error) {
	return s.markRows(ctx, tx, jobID, userID, found, `UPDATE platform.job_rows SET suppressed = true
			WHERE job_id = $1::uuid AND user_id = $2::uuid AND state = 'pending' AND email_norm = ANY($3::text[])`)
}

// markRows runs one of the two statements above over the addresses in batches and returns how many rows it changed.
func (s *Store) markRows(ctx context.Context, tx pgx.Tx, jobID, userID string, found map[string]bool, statement string) (int, error) {
	if len(found) == 0 {
		return 0, nil
	}
	list := make([]string, 0, len(found))
	for e := range found {
		list = append(list, e)
	}
	sort.Strings(list)
	changed := 0
	for start := 0; start < len(list); start += rowBatch {
		tag, err := tx.Exec(ctx, statement, jobID, userID, list[start:min(start+rowBatch, len(list))])
		if err != nil {
			return 0, s.fail(err)
		}
		changed += int(tag.RowsAffected())
	}
	return changed, nil
}

// checkEntitlement is module 3's replacement for credits: an enabled, unexpired entitlement (by the DATABASE's clock) and a user the platform has not observed suspended. An observation of
// 'suspended' counts whatever version it was made at: the platform never reports module 3 in a state it was not seen in, so a user re-enabled a moment ago is refused until the reconcile
// loop has read their un-suspension back.
func (s *Store) checkEntitlement(ctx context.Context, tx pgx.Tx, userID string) error {
	var enabled, current bool
	err := tx.QueryRow(ctx, `SELECT enabled, enabled_until IS NULL OR enabled_until > clock_timestamp() FROM platform.m3_entitlements WHERE user_id = $1::uuid`, userID).Scan(&enabled, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoEntitlement
	}
	if err != nil {
		return s.fail(err)
	}
	if !enabled || !current {
		return ErrNoEntitlement
	}
	var observed string
	err = tx.QueryRow(ctx, `SELECT observed FROM platform.m3_sync WHERE user_id = $1::uuid`, userID).Scan(&observed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing was ever observed
	}
	if err != nil {
		return s.fail(err)
	}
	if observed == "suspended" {
		return ErrSuspended
	}
	return nil
}

// writePlatformResults records the platform's own outcome of every row that is not pending, free: malformed (P-08), duplicate (P-07), suppressed (P-SUP), excluded (P-EXC), missing_field (P-09) and
// unrenderable (P-10): the row's reason is the result's (field names or a refusal word for the last two, never a value). One statement; the
// job must already be confirmed (the table refuses a result for a draft or previewed job). The rule labels of malformed and duplicate come from the charge table, not from this file.
func (s *Store) writePlatformResults(ctx context.Context, tx pgx.Tx, jobID, userID string) error {
	rules := map[string]string{}
	for _, st := range []string{StateMalformed, StateDuplicate, StateSuppressed, StateExcluded, StateMissingField, StateUnrenderable} {
		r, err := resultRule(st)
		if err != nil {
			return err
		}
		rules[st] = r
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, reason, decision, rule)
		SELECT r.job_id, r.user_id, r.id, r.ordinal, COALESCE(r.email_norm, r.email_raw), 'platform', r.state, r.reason, 'free',
			CASE r.state WHEN 'malformed' THEN $3::text WHEN 'duplicate' THEN $4::text WHEN 'suppressed' THEN $5::text WHEN 'excluded' THEN $6::text WHEN 'missing_field' THEN $7::text WHEN 'unrenderable' THEN $8::text END
		FROM platform.job_rows r WHERE r.job_id = $1::uuid AND r.user_id = $2::uuid AND r.state <> 'pending'`,
		jobID, userID, rules[StateMalformed], rules[StateDuplicate], rules[StateSuppressed], rules[StateExcluded], rules[StateMissingField], rules[StateUnrenderable]); err != nil {
		return s.fail(err)
	}
	return nil
}

// replayResult is the answer of a confirm that had already happened: the job as it is now, with the numbers the first answer carried, read from what was written (the reserve row of the
// ledger and the rows). Nothing is written.
func (s *Store) replayResult(ctx context.Context, q querier, j Job) (ConfirmResult, error) {
	var reserved int64
	var pending, suppressed, missing, unrenderable int
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT reserved_delta FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'), 0)`, j.ID).Scan(&reserved); err != nil {
		return ConfirmResult{}, s.fail(err)
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE suppressed), count(*) FILTER (WHERE state = 'missing_field'), count(*) FILTER (WHERE state = 'unrenderable')
		FROM platform.job_rows WHERE job_id = $1::uuid AND user_id = $2::uuid`, j.ID, j.UserID).Scan(&pending, &suppressed, &missing, &unrenderable); err != nil {
		return ConfirmResult{}, s.fail(err)
	}
	return ConfirmResult{Job: j, Replayed: true, Reserved: reserved, Pending: pending, Suppressed: suppressed, MissingFields: missing, Unrenderable: unrenderable}, nil
}
