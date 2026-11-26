-- platform_0016_pause_cancel_capabilities.up.sql — Phase 4 close-out, item D1 (owner rulings C1m2, C1m3 and C2: the modules gained their routes). Docs/phase-4-spec.md, Locked 21 and 26, "Findings: what pause can honestly mean per module".
--
-- WHAT CHANGED IN THE MODULES (additive, read from their Docs/api-contract.md): module 2 and module 3 serve POST /v1/pause and POST /v1/resume for ONE campaign (the platform job id), and module 1 serves POST /v1/jobs/{id}/cancel.
-- So the three facts platform_0011 seeded as "no module can pause one job, module 1 cannot cancel" are no longer true, and the lifecycle needs the moves that follow from them.
--
--   1. THE CAPABILITIES (data). platform.modules.can_pause becomes true for m2 and m3 (module 1 has no per-job pause: it stays false) and can_cancel becomes true for m1 (m2 and m3 already could). From here
--      the guard accepts running -> paused for modules 2 and 3 and running -> cancelling for every module; the refusals it keeps for a module whose flag is false (job_module_cannot_pause, job_module_cannot_cancel)
--      are now reachable only through the data (a test flips a row in a scratch transaction), and the API words them pause_not_supported (module 1) and cancel_not_supported (no seed reaches it any more).
--   2. TWO NEW LEGAL MOVES (data, platform.job_transitions, insert-only):
--        paused -> cancelling   user, admin   cancel a job that was paused WHILE RUNNING: the module holds items, so the cancel goes through the engine (which tells the module and waits for it), never straight to
--                                             cancelled. A module 2 cancel also releases the pause; a module 3 cancel of a paused campaign cancels its unclaimed items.
--        paused -> completing   engine        the module reports every item it holds final while the job is paused (everything had been claimed before the pause and has now finished): the job ends the normal way,
--                                             completing -> succeeded. Nothing about the pause changes what a finished job is.
--      The pairs that already existed and that this design uses as they are: running -> paused and paused -> running (user, admin: the pause and the resume), paused -> cancelled (a job paused BEFORE it
--      started: nothing was submitted), paused -> failed (admin, engine), paused -> expired (engine, only a job paused before its start).
--   3. THE GUARD (jobs_transition_guard, amended only where the old text blocked these moves, and with the walls the new pairs need). Every wall of platform_0011 stays, text for text; what changes:
--        - the cancel capability check (job_module_cannot_cancel) also covers paused -> cancelling (a paused job that was running is a running job for what a module can do to it);
--        - paused -> cancelling and paused -> completing are refused when the job paused from SCHEDULED (23001, job_unsubmitted_pause_never_cancelling / job_unsubmitted_pause_never_completing): such a job submitted
--          nothing, there is no module work to cancel or to finish, and sending it through the engine would invent a cancel the module never saw;
--        - paused -> cancelled is refused when the job paused from RUNNING (23001, job_submitted_pause_cancels_through_the_engine): the module holds its items, and a job that ends cancelled without the engine would
--          release its reserve and its slot while the module still had work. Before this migration a job could not be paused from running, so the pair was only ever taken by an unsubmitted job; now it is reachable and the wall
--          is the database's, not only the store's (internal/jobs refuses it too, with the word submitted_job_ends_through_the_engine, for every writer that goes through it).
--      paused_from stays the guard's to set (NEW.paused_from := the status the job paused from, NULL for every other status), cancel_requested_at is set by the move into cancelling whichever status it comes from,
--      and the started-early and expired-early checks, the resume-only-where-it-paused-from rule and the rule that a job that was running never expires stay as they were.
--
-- WHAT THIS DOES NOT DO. It does not decide WHEN the engine calls a module (internal/jobengine) or what a pause means at the module (the modules' contracts). The old notes of the platform_0011 rows ("none today") are
-- history: the table is insert-only, so they stay and this comment is their correction. Nothing here touches money: a paused job holds its reserve and its slot (every paused job is in the slot index of platform_0011), and
-- there is NO automatic expiry of a pause that was taken while running (a job paused from running never expires: the engine has nothing to expire it with, by rule), so the credits are held until the user resumes or cancels.
--
-- UPGRADE: nothing to migrate. No job can be paused from running before this migration (no module could pause one), so no existing row is in a state the new walls would judge.
-- NULL TRAP (CLAUDE.md rule 13): every comparison on a nullable value (paused_from, cancel_requested_at, the settings read back) is IS DISTINCT FROM / IS NOT DISTINCT FROM / IS NOT NULL; the capability flags are NOT NULL.
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── 1. what a module can do to ONE running job ───────────────────────────────────────────────────────────────────
UPDATE platform.modules SET can_pause = true WHERE id IN ('m2', 'm3');
UPDATE platform.modules SET can_cancel = true WHERE id = 'm1';
COMMENT ON COLUMN platform.modules.can_pause IS 'Phase 4: the module can stop and later continue ONE job. True for modules 2 and 3 (POST /v1/pause and /v1/resume per campaign, send_managed_0031 and Phase 4 C1 of module 3), false for module 1 (no per-job pause). The guard refuses running -> paused while this is false. Flipping it is a migration that ships with the adapter that calls the module''s pause route.';
COMMENT ON COLUMN platform.modules.can_cancel IS 'Phase 4: the module can cancel ONE job (unclaimed items become cancelled, an item in flight finishes). True for every module today (modules 2 and 3: POST /v1/cancel; module 1: POST /v1/jobs/{id}/cancel since its migration 0020). The guard refuses running -> cancelling, and paused -> cancelling of a job that was running, while this is false.';

-- ── 2. the two new legal moves ───────────────────────────────────────────────────────────────────────────────────
INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES
    ('paused', 'cancelling', ARRAY['user', 'admin'], 'cancel a job that was paused while running: the module holds its items, so the engine tells the module and waits for it (never straight to cancelled; refused for a job paused before its start)'),
    ('paused', 'completing', ARRAY['engine'],        'the module reports every item it holds final while the job is paused: it ends the normal way (refused for a job paused before its start)');
COMMENT ON TABLE platform.job_transitions IS 'Every legal change of platform.jobs.status and the actors that may take it. Insert-only for everyone (a new pair is a migration); the application reads it. jobs_transition_guard refuses every change that is not a row. The notes of the platform_0011 rows that say no module can pause or cancel one job are history: platform_0016 made modules 2 and 3 able to pause and every module able to cancel.';

-- ── 3. the guard: platform_0011's text, with the capability check widened and the three walls of the new pairs ──────
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

COMMENT ON COLUMN public.job_items.status IS 'pending (what module 3 writes and the default) or cancelled: the platform''s terminal marker on an item that was NEVER submitted to its module and whose job expired, was cancelled or failed (written by internal/jobs in the transaction that ends the job; never moved back). Since the Phase 4 close-out (D9) the run route of modules 2 and 3 READS it in the same statement that reads and freezes the item and refuses a cancelled one (409 job_item_cancelled); every other value, and NULL, runs. The platform owns the rest of the vocabulary; the column must keep accepting pending and cancelled.';

INSERT INTO platform.schema_migrations (version) VALUES ('0016');
