-- platform_0016_pause_cancel_capabilities.down.sql — undo platform_0016: the capabilities go back to what platform_0011 seeded (no module can pause one job, only modules 2 and 3 can cancel one), the two moves
-- paused -> cancelling and paused -> completing leave platform.job_transitions, and jobs_transition_guard is platform_0011's text again (the capability check of a cancel covers only a running job, and the three walls
-- of the new pairs are gone with the pairs). REFUSES while a job is paused while running, or is a job of module 1 that is cancelling: the older lifecycle has no way for the engine to end the first (paused -> completing
-- is gone, and paused -> cancelled would end it with its module still holding items) and cannot cancel the second (module 1 cannot cancel a job); ending such a job is a decision about money and a customer's work and is
-- not made here. The two rows of the append-only job_transitions are removed with its append-only trigger switched off for this transaction, as the table's owner, and back on (the same device platform_0014 uses for the
-- results); the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n integer;
BEGIN
    SELECT count(*) INTO n FROM platform.jobs
     WHERE (status = 'paused' AND paused_from IS NOT DISTINCT FROM 'running') OR (status IN ('cancelling', 'completing') AND module_id = 'm1' AND cancel_requested_at IS NOT NULL);
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0016 down: % job(s) are paused while running, or are module 1 jobs whose cancel was accepted; the older lifecycle cannot end them: end them first', n USING ERRCODE = '55000';
    END IF;
END
$$;

ALTER TABLE platform.job_transitions DISABLE TRIGGER job_transitions_append_only;
DELETE FROM platform.job_transitions WHERE (from_status, to_status) IN (('paused', 'cancelling'), ('paused', 'completing'));
ALTER TABLE platform.job_transitions ENABLE TRIGGER job_transitions_append_only;
COMMENT ON TABLE platform.job_transitions IS 'Every legal change of platform.jobs.status and the actors that may take it. Insert-only for everyone (a new pair is a migration); the application reads it. jobs_transition_guard refuses every change that is not a row.';

UPDATE platform.modules SET can_pause = false WHERE id IN ('m2', 'm3');
UPDATE platform.modules SET can_cancel = false WHERE id = 'm1';
COMMENT ON COLUMN platform.modules.can_pause IS 'Phase 4: the module can stop and later continue ONE job. False for every module today (no module has a per-job pause: Docs/phase-4-spec.md, Findings). The guard refuses running -> paused while this is false. Flipping it is a migration that ships with the adapter that calls the module''s pause route.';
COMMENT ON COLUMN platform.modules.can_cancel IS 'Phase 4: the module can cancel ONE job (unclaimed items become cancelled, an item in flight finishes). True for modules 2 and 3 (POST /v1/cancel), false for module 1 (no cancel route). The guard refuses running -> cancelling while this is false.';

-- the guard of platform_0011
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
    IF OLD.status = 'running' AND NEW.status = 'cancelling' THEN
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
        NEW.last_progress_at := now();
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_transition_guard() IS 'SECURITY INVOKER: the lifecycle. A job is born draft; a change of status must be a row of platform.job_transitions, taken by an actor that row allows (a user only on their own job), with a reason, within what the module can do and the database''s clock. 23001 with the constraint names job_born_draft, job_transition_illegal, job_transition_needs_an_actor, job_transition_actor_not_allowed, job_transition_by_another_user, job_module_cannot_pause, job_module_cannot_cancel, job_resume_to_where_it_paused_from, job_started_early, job_expired_early, job_cancelled_cannot_succeed, job_cancelled_needs_a_cancel.';

COMMENT ON COLUMN public.job_items.status IS 'pending (what module 3 writes and the default) or cancelled: the platform''s terminal marker on an item that was NEVER submitted to its module and whose job expired, was cancelled or failed (written by internal/jobs in the transaction that ends the job; never moved back). NO module reads this column (their only statements on the table are the run route''s SELECT of campaign_id and its INSERT ... SELECT into their own jobs table, both by id and user, and module 3''s test-send INSERT): the marker is the platform''s record, not a lock.';

DELETE FROM platform.schema_migrations WHERE version = '0016';
