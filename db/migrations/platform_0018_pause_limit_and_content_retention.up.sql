-- platform_0018_pause_limit_and_content_retention.up.sql — Phase 5, chunks S7 and S8 (Docs/phase-5-spec.md, "Decisions taken" 7 and 9). Two rules that need a fact of the database's own: how long a job has been paused, and whether the content of an ended job was cleared.
--
-- S7. A JOB THAT STAYS PAUSED FOR LONGER THAN PLATFORM_JOB_PAUSE_MAX_DAYS IS CANCELLED BY THE ENGINE.
--   1. platform.jobs.paused_at (timestamptz): the DATABASE's now() of the move into paused, NULL in every other status. Like paused_from and the other lifecycle times it is the guard's to set (a writer's value is overwritten) and it
--      changes only together with a change of status (23001, job_lifecycle_columns_move_with_a_transition). CHECK jobs_paused_at_shape: a job has paused_at exactly when it is paused (so a paused job whose pause the engine could not time cannot exist).
--      Partial index jobs_paused_at_idx (status = 'paused') is what the engine's sweep reads.
--   2. THE ENGINE MAY TAKE TWO MORE MOVES (data, platform.job_transitions): paused -> cancelling and paused -> cancelled gain the actor `engine` beside user and admin. The table is insert-only, so the two rows are amended with the append-only trigger
--      switched off for this transaction, as the table's owner, and back on (the device platform_0014 uses for the results and platform_0016's down uses for these very rows). The WALLS OF platform_0016 DO NOT CHANGE and now govern the engine too:
--      paused -> cancelling is refused for a job that paused from SCHEDULED (job_unsubmitted_pause_never_cancelling: nothing was submitted, there is nothing for a module to cancel) and paused -> cancelled for a job that paused from RUNNING
--      (job_submitted_pause_cancels_through_the_engine: the module holds its items), so the engine takes a job that was running through cancelling (the module cancels what it has not claimed and the existing cancelling step settles the end)
--      and a job that never started straight to cancelled, and the database holds both facts whoever writes. What the database does NOT hold is the limit itself: it is a dial of the service (the store compares paused_at with the database's now(), never
--      the Go clock), so the guard cannot tell an early engine from a late one; the engine's moves are as honest as the role that makes them, as every actor claim of the lifecycle is (platform_0011, "WHAT THIS DOES NOT DO").
--   3. THE GUARD, jobs_transition_guard, replaced. platform_0016's text carried over EVERY rule, line for line (diffed against it): the born-draft rule; the annotation rule (a write that leaves the status alone may not change the lifecycle columns); the
--      legal pair, the actor, who and why (platform.actor_kind, platform.reason), the acting user is the owner, the engine carries no id; the capability check of running -> paused (can_pause); the capability check of a cancel, which covers a running job
--      and a job paused from running (can_cancel); a pause resumes only where it paused from; a job that was running never expires; the three walls of the pause pairs (job_unsubmitted_pause_never_cancelling, job_unsubmitted_pause_never_completing,
--      job_submitted_pause_cancels_through_the_engine); a scheduled job cannot start early (job_started_early); nothing expires before its deadline (job_expired_early); a cancel that the module accepted decides how the job ends
--      (job_cancelled_cannot_succeed, job_cancelled_needs_a_cancel); and the guard's own columns (paused_from, cancel_requested_at, confirmed_at, started_at, finished_at, start_deadline, and last_progress_at on a move into running). What changed is TWO LINES
--      and one word of a message: paused_at joins the list of columns an annotation may not change, and the guard sets NEW.paused_at (now() into paused, NULL out of it). Nothing else was edited.
--   UPGRADE: a job that is paused when this runs gets the time of its move into paused from its transition event (the event is written by the same transaction as the move), and updated_at when there is none; the old guard is still in place while
--   this is written, and it lets an annotation through. A job is therefore never paused with a NULL paused_at, which the CHECK would refuse.
--
-- S8. THE CONTENT OF A SEND JOB IS CLEARED A DIAL OF DAYS AFTER THE JOB ENDED (PLATFORM_CONTENT_RETENTION_DAYS, default 7).
--   1. platform.jobs.content_cleared_at (timestamptz): when the job store cleared the rendered content of the job's rows in public.job_items (rendered_subject and rendered_body become the placeholder [removed], rendered_text and unsubscribe_url NULL;
--      reply_to and the recipient stay, the outcomes are never touched). NULL: not cleared (yet). It is the mark that makes the purge idempotent and cheap: the purge selects only jobs whose mark is NULL, and sets it in the end, after the last page.
--      CHECK jobs_content_cleared_after_the_end: only a TERMINAL job has it, and never earlier than its finished_at (a job cannot be cleared before it ended). Trigger jobs_content_cleared_once: it is set once and never moved or cleared (23514,
--      jobs_content_cleared_is_set_once). Partial index jobs_content_retention_idx covers exactly the jobs the purge looks for (terminal, content-bearing, not yet cleared), so the select is cheap whatever the number of jobs that were cleared.
--   2. public.job_items needs NO change and gets none: no trigger, policy or row-level security is added to the two public tables (a test pins that), the modules' columns rendered_subject and rendered_body are NOT NULL (so the placeholder is a
--      non-blank string, which module 2's and module 3's pre-flight accept), and platform_app already holds UPDATE on the table. It does require public.job_items.unsubscribe_url (platform_0017): the purge clears it, so this migration refuses to run
--      before platform_0017 (55000) rather than leave a purge that fails at its first run.
--   3. platform_app may update the new column (it holds UPDATE on the table); the administrator role reads only.
--
-- NULL TRAP (CLAUDE.md rule 13): paused_at and content_cleared_at are nullable; each CHECK says IS NULL / IS NOT NULL; the guard compares with IS DISTINCT FROM / IS NOT DISTINCT FROM / IS NOT NULL.
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'public.job_items'::regclass AND attname::text = 'unsubscribe_url' AND NOT attisdropped) THEN
        RAISE EXCEPTION 'platform_0018: public.job_items.unsubscribe_url does not exist: platform_0017 must be applied first (the content purge clears that column)' USING ERRCODE = '55000';
    END IF;
END
$$;

-- ── 1. the time of a pause ─────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.jobs ADD COLUMN paused_at timestamptz;
-- the old guard (platform_0016) is still in place and lets an annotation through; the backfill writes the move's own time
UPDATE platform.jobs j SET paused_at = COALESCE(
        (SELECT max(e.created_at) FROM platform.job_events e WHERE e.job_id = j.id AND e.kind = 'transition' AND e.to_status = 'paused'),
        j.updated_at)
 WHERE j.status = 'paused';
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_paused_at_shape CHECK ((status = 'paused') = (paused_at IS NOT NULL));
CREATE INDEX jobs_paused_at_idx ON platform.jobs (paused_at) WHERE status = 'paused';
COMMENT ON COLUMN platform.jobs.paused_at IS 'Phase 5 S7: the database''s now() of the move into paused; NULL in every other status. Set by jobs_transition_guard (a writer''s value is overwritten), changes only with a change of status. The engine cancels a job whose pause is older than PLATFORM_JOB_PAUSE_MAX_DAYS, comparing it with the database''s clock.';

-- ── 2. the content mark ────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.jobs ADD COLUMN content_cleared_at timestamptz;
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_content_cleared_after_the_end CHECK (
    content_cleared_at IS NULL OR (status IN ('succeeded', 'failed', 'cancelled', 'expired') AND finished_at IS NOT NULL AND content_cleared_at >= finished_at));
CREATE INDEX jobs_content_retention_idx ON platform.jobs (finished_at)
    WHERE content_cleared_at IS NULL AND status IN ('succeeded', 'failed', 'cancelled', 'expired') AND jsonb_exists(frozen_settings, 'content');
COMMENT ON COLUMN platform.jobs.content_cleared_at IS 'Phase 5 S8: when the job store cleared the rendered content of this send job''s rows in public.job_items (rendered_subject and rendered_body = [removed], rendered_text and unsubscribe_url NULL), after the last page. NULL: not cleared. Only a terminal job has it, never earlier than its finished_at, and it is set once. Outcomes (job_rows, job_results, job_events, the ledger) are never touched.';

CREATE FUNCTION platform.jobs_content_cleared_once() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    RAISE EXCEPTION 'platform.jobs: job % had its content cleared at % and that is set once: it is never moved or cleared', OLD.id, OLD.content_cleared_at
        USING ERRCODE = '23514', CONSTRAINT = 'jobs_content_cleared_is_set_once';
END
$fn$;
COMMENT ON FUNCTION platform.jobs_content_cleared_once() IS 'SECURITY INVOKER: refuses (23514, jobs_content_cleared_is_set_once) every change of a content_cleared_at that is already set.';
REVOKE ALL ON FUNCTION platform.jobs_content_cleared_once() FROM PUBLIC;
CREATE TRIGGER jobs_content_cleared_once BEFORE UPDATE OF content_cleared_at ON platform.jobs FOR EACH ROW
    WHEN (OLD.content_cleared_at IS NOT NULL AND NEW.content_cleared_at IS DISTINCT FROM OLD.content_cleared_at) EXECUTE FUNCTION platform.jobs_content_cleared_once();

-- ── 3. the engine may end a pause that outlasted its limit ─────────────────────────────────────────────────────────────
ALTER TABLE platform.job_transitions DISABLE TRIGGER job_transitions_append_only;
UPDATE platform.job_transitions SET actors = ARRAY['user', 'admin', 'engine'],
       note = 'cancel a paused job that submitted nothing; the engine takes it too once the pause has outlasted PLATFORM_JOB_PAUSE_MAX_DAYS (platform_0018; refused for a job paused while running)'
 WHERE from_status = 'paused' AND to_status = 'cancelled';
UPDATE platform.job_transitions SET actors = ARRAY['user', 'admin', 'engine'],
       note = 'cancel a job that was paused while running: the module holds its items, so the engine tells the module and waits for it (never straight to cancelled; refused for a job paused before its start); the engine takes it too once the pause has outlasted PLATFORM_JOB_PAUSE_MAX_DAYS (platform_0018)'
 WHERE from_status = 'paused' AND to_status = 'cancelling';
ALTER TABLE platform.job_transitions ENABLE TRIGGER job_transitions_append_only;
COMMENT ON TABLE platform.job_transitions IS 'Every legal change of platform.jobs.status and the actors that may take it. Insert-only for everyone (a new pair is a migration; platform_0018 widened the actors of paused -> cancelled and paused -> cancelling to the engine, with the append-only trigger off for that one transaction); the application reads it. jobs_transition_guard refuses every change that is not a row. The notes of the platform_0011 rows that say no module can pause or cancel one job are history: platform_0016 made modules 2 and 3 able to pause and every module able to cancel.';

-- ── 4. the guard: platform_0016's text, with paused_at ─────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION platform.jobs_transition_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_actors text[];
    v_kind   text := NULLIF(btrim(current_setting('platform.actor_kind', true)), '');
    v_id     text := NULLIF(btrim(current_setting('platform.actor_id', true)), '');
    v_reason text := NULLIF(btrim(current_setting('platform.reason', true)), '');
    v_can    boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' THEN
            RAISE EXCEPTION 'platform.jobs: a job is born draft, not %: a job inserted past draft would bypass the preview, the reserve and the slot', NEW.status
                USING ERRCODE = '23001', CONSTRAINT = 'job_born_draft';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        -- an annotation (module_job_ref, last_progress_at, stale_alerted_at, a preview while previewed ...): not a transition. The columns that carry the lifecycle's own facts move ONLY with a transition: a writer that
        -- cleared cancel_requested_at on a cancelling job would let completing -> succeeded through, one that moved start_deadline would keep a job from expiring, one that wrote finished_at would back-date the end.
        -- (platform_0018) paused_at is the pause's own time: a writer that moved it would keep a paused job from ever reaching its pause limit.
        IF NEW.paused_from IS DISTINCT FROM OLD.paused_from OR NEW.paused_at IS DISTINCT FROM OLD.paused_at OR NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at OR NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at
           OR NEW.started_at IS DISTINCT FROM OLD.started_at OR NEW.finished_at IS DISTINCT FROM OLD.finished_at OR NEW.start_deadline IS DISTINCT FROM OLD.start_deadline THEN
            RAISE EXCEPTION 'platform.jobs: job % %: paused_from, paused_at, cancel_requested_at, confirmed_at, started_at, finished_at and start_deadline change only together with a change of status', OLD.id, OLD.status
                USING ERRCODE = '23001', CONSTRAINT = 'job_lifecycle_columns_move_with_a_transition';
        END IF;
        RETURN NEW;
    END IF;

    SELECT t.actors INTO v_actors FROM platform.job_transitions t WHERE t.from_status = OLD.status AND t.to_status = NEW.status;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'platform.jobs: job % cannot go from % to %: that move is not a row of platform.job_transitions', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_illegal';
    END IF;

    IF v_kind IS NULL OR v_reason IS NULL THEN
        RAISE EXCEPTION 'platform.jobs: job % %->%: a move names who (platform.actor_kind) and why (platform.reason); platform.job_transition() sets them', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_needs_an_actor';
    END IF;
    IF NOT (v_kind = ANY (v_actors)) THEN
        RAISE EXCEPTION 'platform.jobs: job % %->%: an actor of kind % may not take that move (allowed: %)', OLD.id, OLD.status, NEW.status, v_kind, v_actors
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_actor_not_allowed';
    END IF;
    IF v_kind IN ('user', 'admin') AND v_id IS NULL THEN
        RAISE EXCEPTION 'platform.jobs: job % %->%: a % move names who acts (platform.actor_id)', OLD.id, OLD.status, NEW.status, v_kind
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_needs_an_actor';
    END IF;
    IF v_kind = 'user' AND v_id IS DISTINCT FROM OLD.user_id::text THEN
        RAISE EXCEPTION 'platform.jobs: job % %->%: the acting user is not the job''s owner', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_by_another_user';
    END IF;
    IF v_kind = 'engine' AND v_id IS NOT NULL THEN
        RAISE EXCEPTION 'platform.jobs: job % %->%: the engine is not a person and carries no actor id', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'job_transition_needs_an_actor';
    END IF;

    -- what the module can do to one running job
    IF OLD.status = 'running' AND NEW.status = 'paused' THEN
        SELECT m.can_pause INTO v_can FROM platform.modules m WHERE m.id = NEW.module_id;
        IF v_can IS NOT TRUE THEN
            RAISE EXCEPTION 'platform.jobs: job % cannot be paused: module % has no per-job pause (cancel-and-resubmit would risk a duplicate send and is never done)', OLD.id, NEW.module_id
                USING ERRCODE = '23001', CONSTRAINT = 'job_module_cannot_pause';
        END IF;
    END IF;
    -- (platform_0016) a job that was paused while running is a running job for what a module can do to it: its cancel needs the same capability
    IF NEW.status = 'cancelling' AND (OLD.status = 'running' OR (OLD.status = 'paused' AND OLD.paused_from IS NOT DISTINCT FROM 'running')) THEN
        SELECT m.can_cancel INTO v_can FROM platform.modules m WHERE m.id = NEW.module_id;
        IF v_can IS NOT TRUE THEN
            RAISE EXCEPTION 'platform.jobs: job % cannot be cancelled while running: module % has no cancel for one job', OLD.id, NEW.module_id
                USING ERRCODE = '23001', CONSTRAINT = 'job_module_cannot_cancel';
        END IF;
    END IF;

    -- a pause resumes only where it paused from
    IF OLD.status = 'paused' THEN
        IF NEW.status IN ('scheduled', 'running') AND NEW.status IS DISTINCT FROM OLD.paused_from THEN
            RAISE EXCEPTION 'platform.jobs: job % paused from % and resumes only there, not to %', OLD.id, OLD.paused_from, NEW.status
                USING ERRCODE = '23001', CONSTRAINT = 'job_resume_to_where_it_paused_from';
        END IF;
        IF NEW.status = 'expired' AND OLD.paused_from IS DISTINCT FROM 'scheduled' THEN
            RAISE EXCEPTION 'platform.jobs: job % paused from % and a job that was running never expires', OLD.id, OLD.paused_from
                USING ERRCODE = '23001', CONSTRAINT = 'job_transition_illegal';
        END IF;
        -- (platform_0016) a job paused before its start submitted nothing: no module holds work for the engine to cancel or to finish
        IF NEW.status = 'cancelling' AND OLD.paused_from IS DISTINCT FROM 'running' THEN
            RAISE EXCEPTION 'platform.jobs: job % paused from % and submitted nothing: it is cancelled directly, never through the engine', OLD.id, OLD.paused_from
                USING ERRCODE = '23001', CONSTRAINT = 'job_unsubmitted_pause_never_cancelling';
        END IF;
        IF NEW.status = 'completing' AND OLD.paused_from IS DISTINCT FROM 'running' THEN
            RAISE EXCEPTION 'platform.jobs: job % paused from % and submitted nothing: no module reported anything to complete', OLD.id, OLD.paused_from
                USING ERRCODE = '23001', CONSTRAINT = 'job_unsubmitted_pause_never_completing';
        END IF;
        -- (platform_0016) a job paused while running is held by its module: it ends cancelled only after the engine has told the module and recorded what finished
        IF NEW.status = 'cancelled' AND OLD.paused_from IS NOT DISTINCT FROM 'running' THEN
            RAISE EXCEPTION 'platform.jobs: job % paused while running and its module holds its items: it is cancelled through cancelling, never directly', OLD.id
                USING ERRCODE = '23001', CONSTRAINT = 'job_submitted_pause_cancels_through_the_engine';
        END IF;
    END IF;

    -- the clock is the database's
    IF OLD.status = 'scheduled' AND NEW.status = 'running' AND NEW.scheduled_start IS NOT NULL AND NEW.scheduled_start > now() THEN
        RAISE EXCEPTION 'platform.jobs: job % is scheduled for % and cannot start before then', OLD.id, NEW.scheduled_start
            USING ERRCODE = '23001', CONSTRAINT = 'job_started_early';
    END IF;
    IF NEW.status = 'expired' AND (NEW.start_deadline IS NULL OR NEW.start_deadline > now()) THEN
        RAISE EXCEPTION 'platform.jobs: job % expires only after its start deadline (%)', OLD.id, NEW.start_deadline
            USING ERRCODE = '23001', CONSTRAINT = 'job_expired_early';
    END IF;

    -- a cancel that the module accepted decides how the job ends
    IF OLD.status = 'completing' AND NEW.status = 'succeeded' AND OLD.cancel_requested_at IS NOT NULL THEN
        RAISE EXCEPTION 'platform.jobs: job % had a cancel accepted and ends cancelled, never succeeded', OLD.id
            USING ERRCODE = '23001', CONSTRAINT = 'job_cancelled_cannot_succeed';
    END IF;
    IF OLD.status = 'completing' AND NEW.status = 'cancelled' AND OLD.cancel_requested_at IS NULL THEN
        RAISE EXCEPTION 'platform.jobs: job % had no cancel accepted and cannot end cancelled from completing', OLD.id
            USING ERRCODE = '23001', CONSTRAINT = 'job_cancelled_needs_a_cancel';
    END IF;

    -- THE LIFECYCLE'S OWN COLUMNS ARE THE GUARD'S TO SET, NEVER THE WRITER'S, not even inside a move (a move that carried cancel_requested_at = NULL would have let completing -> succeeded through):
    -- each is computed here from the old row and the clock, and whatever the statement said is overwritten.
    NEW.paused_from         := CASE WHEN NEW.status = 'paused' THEN OLD.status ELSE NULL END;
    NEW.paused_at           := CASE WHEN NEW.status = 'paused' THEN now() ELSE NULL END;   -- (platform_0018) the pause's own time: the database's, set at the move into paused and gone with the move out of it
    NEW.cancel_requested_at := CASE WHEN NEW.status = 'cancelling' THEN now() ELSE OLD.cancel_requested_at END;
    NEW.confirmed_at        := CASE WHEN OLD.status = 'previewed' AND NEW.status IN ('confirmed', 'scheduled') THEN now() ELSE OLD.confirmed_at END;
    NEW.started_at          := CASE WHEN NEW.status = 'running' THEN COALESCE(OLD.started_at, now()) ELSE OLD.started_at END;
    NEW.finished_at         := CASE WHEN NEW.status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN now() ELSE OLD.finished_at END;
    NEW.start_deadline      := CASE WHEN OLD.status = 'previewed' AND NEW.status IN ('confirmed', 'scheduled') THEN NEW.start_deadline ELSE OLD.start_deadline END;   -- the confirm move states it, no other move can
    IF NEW.status = 'running' THEN
        NEW.last_progress_at := now();   -- a resumed job starts its staleness clock again: a pause is not a stall
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_transition_guard() IS 'SECURITY INVOKER: the lifecycle. A job is born draft; a change of status must be a row of platform.job_transitions, taken by an actor that row allows (a user only on their own job), with a reason, within what the module can do and the database''s clock. 23001 with the constraint names job_born_draft, job_transition_illegal, job_transition_needs_an_actor, job_transition_actor_not_allowed, job_transition_by_another_user, job_module_cannot_pause, job_module_cannot_cancel, job_resume_to_where_it_paused_from, job_unsubmitted_pause_never_cancelling, job_unsubmitted_pause_never_completing, job_submitted_pause_cancels_through_the_engine, job_started_early, job_expired_early, job_cancelled_cannot_succeed, job_cancelled_needs_a_cancel, job_lifecycle_columns_move_with_a_transition (platform_0018 added paused_at, the time of the move into paused, to the columns the guard sets and an annotation may not change).';

INSERT INTO platform.schema_migrations (version) VALUES ('0018');
