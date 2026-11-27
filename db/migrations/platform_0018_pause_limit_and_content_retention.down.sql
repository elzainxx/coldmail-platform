-- platform_0018_pause_limit_and_content_retention.down.sql — undo platform_0018: the guard is platform_0016's text again (no paused_at), the two moves paused -> cancelled and paused -> cancelling belong to user and admin only (the rows and
-- their notes as platform_0011 and platform_0016 wrote them), the set-once trigger of the content mark, its function, the two CHECKs, the two partial indexes and the two columns (paused_at, content_cleared_at) are gone.
-- WHAT IS LOST ON PURPOSE: the mark of the content purge (a re-run of the purge after an up finds every ended job unmarked and clears again, which changes nothing: the rows already hold the placeholder) and the time of a pause (an up
-- backfills it from the transition event). The content that was cleared is NOT restored, and cannot be: that is the point of the purge. The two rows of the append-only job_transitions are amended with its append-only trigger switched off for
-- this transaction, as the table's owner, and back on (the device platform_0014 and platform_0016 use); the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
SET LOCAL ROLE platform_owner;

-- the engine's own cancel of a paused job: the older lifecycle has no such move, and a job paused while running that the engine cancelled is simply cancelling or cancelled by then, which platform_0016 knows
ALTER TABLE platform.job_transitions DISABLE TRIGGER job_transitions_append_only;
UPDATE platform.job_transitions SET actors = ARRAY['user', 'admin'], note = 'cancel a paused job that submitted nothing'
 WHERE from_status = 'paused' AND to_status = 'cancelled';
UPDATE platform.job_transitions SET actors = ARRAY['user', 'admin'],
       note = 'cancel a job that was paused while running: the module holds its items, so the engine tells the module and waits for it (never straight to cancelled; refused for a job paused before its start)'
 WHERE from_status = 'paused' AND to_status = 'cancelling';
ALTER TABLE platform.job_transitions ENABLE TRIGGER job_transitions_append_only;
COMMENT ON TABLE platform.job_transitions IS 'Every legal change of platform.jobs.status and the actors that may take it. Insert-only for everyone (a new pair is a migration); the application reads it. jobs_transition_guard refuses every change that is not a row. The notes of the platform_0011 rows that say no module can pause or cancel one job are history: platform_0016 made modules 2 and 3 able to pause and every module able to cancel.';

DROP TRIGGER jobs_content_cleared_once ON platform.jobs;
DROP FUNCTION platform.jobs_content_cleared_once();
DROP INDEX platform.jobs_content_retention_idx;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_content_cleared_after_the_end;
ALTER TABLE platform.jobs DROP COLUMN content_cleared_at;

-- the guard of platform_0016 (the CHECK and the index of paused_at go first: the old function does not know the column, and the CHECK would refuse a move out of paused that leaves it set)
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_paused_at_shape;
DROP INDEX platform.jobs_paused_at_idx;
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
        IF NEW.paused_from IS DISTINCT FROM OLD.paused_from OR NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at OR NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at
           OR NEW.started_at IS DISTINCT FROM OLD.started_at OR NEW.finished_at IS DISTINCT FROM OLD.finished_at OR NEW.start_deadline IS DISTINCT FROM OLD.start_deadline THEN
            RAISE EXCEPTION 'platform.jobs: job % %: paused_from, cancel_requested_at, confirmed_at, started_at, finished_at and start_deadline change only together with a change of status', OLD.id, OLD.status
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
COMMENT ON FUNCTION platform.jobs_transition_guard() IS 'SECURITY INVOKER: the lifecycle. A job is born draft; a change of status must be a row of platform.job_transitions, taken by an actor that row allows (a user only on their own job), with a reason, within what the module can do and the database''s clock. 23001 with the constraint names job_born_draft, job_transition_illegal, job_transition_needs_an_actor, job_transition_actor_not_allowed, job_transition_by_another_user, job_module_cannot_pause, job_module_cannot_cancel, job_resume_to_where_it_paused_from, job_unsubmitted_pause_never_cancelling, job_unsubmitted_pause_never_completing, job_submitted_pause_cancels_through_the_engine, job_started_early, job_expired_early, job_cancelled_cannot_succeed, job_cancelled_needs_a_cancel (platform_0016 added the three job_*_pause_* names and widened job_module_cannot_cancel to a job paused while running).';
ALTER TABLE platform.jobs DROP COLUMN paused_at;

DELETE FROM platform.schema_migrations WHERE version = '0018';
