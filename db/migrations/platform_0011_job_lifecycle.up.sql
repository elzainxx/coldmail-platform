-- platform_0011_job_lifecycle.up.sql — Phase 4, chunk B1. The job lifecycle becomes a table and a trigger, so that EVERY writer of platform.jobs (the job engine, the API's store, the admin console of Phase 7, a script) is held to the
-- same rules by the database. Docs/phase-4-spec.md, "Locked here" 21, 22 and 26.
--
--   1. THE VOCABULARY. draft, previewed, confirmed, scheduled, running, paused, cancelling, completing and the terminal succeeded, failed, cancelled, expired. (cancelling is Phase 1's: an item in flight finishes after a cancel
--      and is reported once, so a cancel is not instantaneous. completing is the state in which the module has reported every item final and the platform is recording the last results and releasing the remainder.)
--   2. THE LEGAL MOVES ARE DATA. platform.job_transitions holds every legal (from, to) pair and which actors may take it (user, admin, engine). It is insert-only for everybody, the application roles hold SELECT only, and
--      jobs_transition_guard refuses any change of status that is not a row (SQLSTATE 23001, constraint job_transition_illegal). A job is BORN draft (23001, job_born_draft): a job inserted as running would bypass the
--      preview, the reserve and the slot.
--   3. EVERY MOVE HAS A WHO, A WHEN AND A WHY. The writer states them in three transaction-local settings (platform.actor_kind, platform.actor_id, platform.reason: the store's one statement is
--      `SELECT platform.job_transition(job, to, kind, id, reason)`, or the same three set_config calls before its own UPDATE). The guard refuses a move without them (job_transition_needs_an_actor), a user who is not the
--      job's owner (job_transition_by_another_user), an actor the pair does not allow (job_transition_actor_not_allowed); the AFTER trigger writes the platform.job_events row (append-only for everyone, a disabled admin
--      refused as everywhere else, Locked 12) in the same transaction, so a move without its event cannot commit.
--   4. WHAT A MODULE CAN DO IS DATA. platform.modules.can_pause and can_cancel are seeded from the module code (Docs/phase-4-spec.md, "Findings"): no module can pause one job, modules 2 and 3 can cancel one, module 1 can do
--      neither. The guard refuses running -> paused unless the module can pause (job_module_cannot_pause) and running -> cancelling unless it can cancel (job_module_cannot_cancel). Pause is never faked by cancel and resubmit.
--   5. THE TIMES ARE THE DATABASE'S. confirmed_at, started_at, finished_at and cancel_requested_at are set by the guard from now() at the move and change ONLY with a move (23001, job_lifecycle_columns_move_with_a_transition, also for paused_from and start_deadline); a scheduled job cannot start before its time (job_started_early) and nothing expires before its
--      start_deadline (job_expired_early). A paused job resumes only to the state it paused from (paused_from; job_resume_to_where_it_paused_from). A job that was cancelled cannot end succeeded and the reverse.
--   6. THE SLOT covers every state in which credits are held or a module may be working: confirmed, scheduled, running, paused, cancelling, completing (it replaces jobs_one_active_per_user_module of platform_0004). A job in
--      draft or previewed holds nothing and does not hold the slot.
--   7. THE SHAPE of a job that has left previewed is a CHECK (jobs_confirmed_shape): a preview, its fingerprint, the client's confirm key, confirmed_at and, for confirmed and scheduled, a start_deadline. A confirmed job
--      has no scheduled_start (it starts as soon as the engine can), a scheduled one has (platform_0002).
--
-- WHAT THIS DOES NOT DO. It does not decide WHEN the engine moves a job (that is the engine), what a preview contains, or whether the credits cover the list (the store and money.ReserveIn). A writer that sets the three
-- settings itself can claim any actor: the audit rows are as honest as the role that writes them. The compensating controls are that only platform_app holds UPDATE on platform.jobs (the admin role is read-only), that the
-- store functions of internal/jobs are the only code that writes the table (scripts/check-job-writers.sh, Phase 4 chunk B6) and that a user's move must carry the owner's own id.
--
-- AN UPGRADE needs no job in confirmed, scheduled, running, paused, cancelling or completing (jobs_confirmed_shape would have nothing to validate against); the platform had no job engine before this migration, so none exists outside tests.
-- NULL TRAP (CLAUDE.md rule 13): every comparison on a nullable value is IS DISTINCT FROM / IS NOT NULL, and each CHECK is built from boolean expressions that cannot be NULL. A custom setting that was SET LOCAL in an earlier
-- transaction of the session reads back as the empty string, not NULL: both are "absent" (NULLIF).
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── 1. the vocabulary ────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_status_vocabulary;
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_status_vocabulary CHECK (status IN
    ('draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing', 'succeeded', 'failed', 'cancelled', 'expired'));

-- ── 2. what a module can do to ONE running job ───────────────────────────────────────────────────────────────────
ALTER TABLE platform.modules ADD COLUMN can_pause boolean NOT NULL DEFAULT false;
ALTER TABLE platform.modules ADD COLUMN can_cancel boolean NOT NULL DEFAULT false;
UPDATE platform.modules SET can_cancel = true WHERE id IN ('m2', 'm3');
COMMENT ON COLUMN platform.modules.can_pause IS 'Phase 4: the module can stop and later continue ONE job. False for every module today (no module has a per-job pause: Docs/phase-4-spec.md, Findings). The guard refuses running -> paused while this is false. Flipping it is a migration that ships with the adapter that calls the module''s pause route.';
COMMENT ON COLUMN platform.modules.can_cancel IS 'Phase 4: the module can cancel ONE job (unclaimed items become cancelled, an item in flight finishes). True for modules 2 and 3 (POST /v1/cancel), false for module 1 (no cancel route). The guard refuses running -> cancelling while this is false.';

-- ── 3. the columns the lifecycle needs ───────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.jobs ADD COLUMN paused_from          text;
ALTER TABLE platform.jobs ADD COLUMN confirm_key          text;
ALTER TABLE platform.jobs ADD COLUMN preview              jsonb;
ALTER TABLE platform.jobs ADD COLUMN preview_fingerprint  text;
ALTER TABLE platform.jobs ADD COLUMN previewed_at         timestamptz;
ALTER TABLE platform.jobs ADD COLUMN confirmed_at         timestamptz;
ALTER TABLE platform.jobs ADD COLUMN started_at           timestamptz;
ALTER TABLE platform.jobs ADD COLUMN finished_at          timestamptz;
ALTER TABLE platform.jobs ADD COLUMN start_deadline       timestamptz;
ALTER TABLE platform.jobs ADD COLUMN last_progress_at     timestamptz;
ALTER TABLE platform.jobs ADD COLUMN stale_alerted_at     timestamptz;
ALTER TABLE platform.jobs ADD COLUMN cancel_requested_at  timestamptz;

ALTER TABLE platform.jobs ADD CONSTRAINT jobs_paused_from_vocabulary CHECK (paused_from IS NULL OR paused_from IN ('scheduled', 'running'));
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_paused_from_shape      CHECK ((status = 'paused') = (paused_from IS NOT NULL));
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_confirm_key_shape      CHECK (confirm_key IS NULL OR (length(confirm_key) BETWEEN 1 AND 128 AND confirm_key ~ '^[A-Za-z0-9._:-]+$'));
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_preview_is_an_object   CHECK (preview IS NULL OR jsonb_typeof(preview) = 'object');
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_fingerprint_shape      CHECK (preview_fingerprint IS NULL OR preview_fingerprint ~ '^[0-9a-f]{64}$');
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_confirmed_shape        CHECK (
    status NOT IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')
    OR (preview IS NOT NULL AND preview_fingerprint IS NOT NULL AND confirm_key IS NOT NULL AND confirmed_at IS NOT NULL));
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_start_needs_a_deadline CHECK (status NOT IN ('confirmed', 'scheduled') OR start_deadline IS NOT NULL);
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_confirmed_starts_now   CHECK (status <> 'confirmed' OR scheduled_start IS NULL);
CREATE UNIQUE INDEX jobs_confirm_key_key ON platform.jobs (user_id, confirm_key) WHERE confirm_key IS NOT NULL;
COMMENT ON COLUMN platform.jobs.confirm_key IS 'The client-supplied idempotency key of the confirm (the API''s Idempotency-Key). Unique per user: confirming twice with one key is one confirm. Frozen once the job has left previewed.';
COMMENT ON COLUMN platform.jobs.preview IS 'The preview the user confirmed (counts, credits to reserve, per-verdict counts when chained): a snapshot, frozen once the job has left previewed. preview_fingerprint is the hash of everything the preview depended on; a confirm whose fingerprint no longer matches is refused (409 preview_stale).';
COMMENT ON COLUMN platform.jobs.start_deadline IS 'coalesce(scheduled_start, confirmed_at) plus the max start delay: a confirmed or scheduled job the engine cannot start by then expires (its reserve is released, charge row P-02).';
COMMENT ON COLUMN platform.jobs.paused_from IS 'The state a paused job paused from (scheduled or running): it resumes only there.';

-- ── 4. the slot: every state that holds credits or lets a module work ──────────────────────────────────────────────
DROP INDEX platform.jobs_one_active_per_user_module;
CREATE UNIQUE INDEX jobs_one_active_per_user_module ON platform.jobs (user_id, module_id)
    WHERE status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing');
COMMENT ON INDEX platform.jobs_one_active_per_user_module IS 'One active job per module per user (Locked 14, amended by 22): a job that is confirmed, scheduled, running, paused, cancelling or completing holds the slot. Another module is a different slot; a draft, a previewed or a terminal job holds nothing.';

-- ── 5. the legal moves, as data ──────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.job_transitions (
    from_status text   NOT NULL,
    to_status   text   NOT NULL,
    actors      text[] NOT NULL,
    note        text   NOT NULL,
    CONSTRAINT job_transitions_pk PRIMARY KEY (from_status, to_status),
    CONSTRAINT job_transitions_from_vocabulary CHECK (from_status IN ('draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')),
    CONSTRAINT job_transitions_to_vocabulary CHECK (to_status IN ('draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing', 'succeeded', 'failed', 'cancelled', 'expired')),
    CONSTRAINT job_transitions_moves CHECK (from_status <> to_status),
    CONSTRAINT job_transitions_actors CHECK (cardinality(actors) >= 1 AND actors <@ ARRAY['user', 'admin', 'engine']::text[]),
    CONSTRAINT job_transitions_note_not_blank CHECK (btrim(note) <> '')
);
COMMENT ON TABLE platform.job_transitions IS 'Every legal change of platform.jobs.status and the actors that may take it. Insert-only for everyone (a new pair is a migration); the application reads it. jobs_transition_guard refuses every change that is not a row.';
CREATE TRIGGER job_transitions_append_only BEFORE UPDATE OR DELETE ON platform.job_transitions FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER job_transitions_no_truncate BEFORE TRUNCATE ON platform.job_transitions FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

INSERT INTO platform.job_transitions (from_status, to_status, actors, note) VALUES
    ('draft',      'previewed',  ARRAY['user', 'admin'],           'the user asks for a preview of the list'),
    ('draft',      'cancelled',  ARRAY['user', 'admin'],           'a draft is discarded'),
    ('previewed',  'draft',      ARRAY['user', 'admin'],           'the list or the settings change: the preview no longer describes the job'),
    ('previewed',  'confirmed',  ARRAY['user', 'admin'],           'confirm: the slot, the reserve and the snapshot in one transaction; starts as soon as the engine can'),
    ('previewed',  'scheduled',  ARRAY['user', 'admin'],           'confirm with a start time: holds the slot and the reserve until then'),
    ('previewed',  'cancelled',  ARRAY['user', 'admin'],           'a previewed job is discarded'),
    ('confirmed',  'running',    ARRAY['engine'],                  'the engine submitted the job to its module'),
    ('confirmed',  'cancelled',  ARRAY['user', 'admin'],           'cancelled before anything was submitted: the reserve is released'),
    ('confirmed',  'expired',    ARRAY['engine'],                  'the module could not be reached before the start deadline: the reserve is released (P-02)'),
    ('confirmed',  'failed',     ARRAY['admin', 'engine'],         'the module refused the job for good'),
    ('scheduled',  'running',    ARRAY['engine'],                  'its time came and the engine submitted it (never before scheduled_start)'),
    ('scheduled',  'paused',     ARRAY['user', 'admin'],           'the start is deferred until the user resumes: nothing was submitted, the slot and the reserve are held'),
    ('scheduled',  'cancelled',  ARRAY['user', 'admin'],           'cancelled before its time: the reserve is released'),
    ('scheduled',  'expired',    ARRAY['engine'],                  'the module could not be reached before the start deadline: the reserve is released (P-02)'),
    ('scheduled',  'failed',     ARRAY['admin', 'engine'],         'the module refused the job for good'),
    ('paused',     'scheduled',  ARRAY['user', 'admin'],           'resume a job that was paused before its start (only to the state it paused from)'),
    ('paused',     'running',    ARRAY['user', 'admin'],           'resume a running job (only for a module that can pause one: none today)'),
    ('paused',     'cancelled',  ARRAY['user', 'admin'],           'cancel a paused job that submitted nothing'),
    ('paused',     'expired',    ARRAY['engine'],                  'a job paused before its start was not resumed by its start deadline'),
    ('paused',     'failed',     ARRAY['admin', 'engine'],         'ended by hand or by the engine'),
    ('running',    'paused',     ARRAY['user', 'admin'],           'pause a running job (refused unless the module can pause one: none can today)'),
    ('running',    'cancelling', ARRAY['user', 'admin'],           'cancel a running job (refused unless the module can cancel one: modules 2 and 3)'),
    ('running',    'completing', ARRAY['engine'],                  'the module reports every item final'),
    ('running',    'failed',     ARRAY['admin', 'engine'],         'ended by hand or by the engine'),
    ('cancelling', 'completing', ARRAY['engine'],                  'the module reports every item final after the cancel'),
    ('cancelling', 'failed',     ARRAY['admin', 'engine'],         'ended by hand or by the engine'),
    ('completing', 'succeeded',  ARRAY['engine'],                  'every result recorded, the remainder released (never for a job whose cancel was accepted)'),
    ('completing', 'cancelled',  ARRAY['engine'],                  'every result recorded after an accepted cancel, the remainder released'),
    ('completing', 'failed',     ARRAY['admin', 'engine'],         'ended by hand or by the engine');

-- ── 6. the events: who, when, why ────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.job_events (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id          uuid        NOT NULL REFERENCES platform.jobs (id) ON DELETE RESTRICT,
    kind            text        NOT NULL,
    from_status     text,
    to_status       text,
    actor_kind      text        NOT NULL,
    acting_user_id  uuid        REFERENCES public.users (id) ON DELETE RESTRICT,
    acting_admin_id uuid        REFERENCES platform.admins (id) ON DELETE RESTRICT,
    reason          text        NOT NULL,
    detail          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_events_kind_vocabulary CHECK (kind IN ('created', 'transition', 'stale', 'flag', 'note')),
    CONSTRAINT job_events_actor_vocabulary CHECK (actor_kind IN ('user', 'admin', 'engine')),
    CONSTRAINT job_events_actor_shape CHECK (
        (actor_kind = 'user'   AND acting_user_id IS NOT NULL AND acting_admin_id IS NULL)
        OR (actor_kind = 'admin'  AND acting_admin_id IS NOT NULL AND acting_user_id IS NULL)
        OR (actor_kind = 'engine' AND acting_user_id IS NULL AND acting_admin_id IS NULL)),
    CONSTRAINT job_events_transition_shape CHECK ((kind = 'transition') = (from_status IS NOT NULL AND to_status IS NOT NULL)),
    CONSTRAINT job_events_reason_not_blank CHECK (btrim(reason) <> ''),
    CONSTRAINT job_events_detail_is_an_object CHECK (jsonb_typeof(detail) = 'object')
);
CREATE INDEX job_events_job_idx ON platform.job_events (job_id, id);
COMMENT ON TABLE platform.job_events IS 'Append-only history of every job: its creation, every status change (written by trigger from the three transaction-local settings), a stale alert, a flag (a vendor answer for an unreachable unit...) and notes. Nobody updates or deletes a row.';
CREATE TRIGGER job_events_append_only BEFORE UPDATE OR DELETE ON platform.job_events FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER job_events_no_truncate BEFORE TRUNCATE ON platform.job_events FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- A disabled admin cannot be named (Locked 12), and a user can only be recorded acting on their OWN job. SECURITY DEFINER for the same reason as the other three callers of require_active_admin (platform_0004, F28).
CREATE FUNCTION platform.job_events_check_actor() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_owner uuid;
BEGIN
    PERFORM platform.require_active_admin(NEW.acting_admin_id, 'platform.job_events.acting_admin_id');
    IF NEW.actor_kind = 'user' THEN
        SELECT j.user_id INTO v_owner FROM platform.jobs j WHERE j.id = NEW.job_id;
        IF FOUND AND NEW.acting_user_id IS DISTINCT FROM v_owner THEN
            RAISE EXCEPTION 'platform.job_events: user % is recorded acting on job %, which belongs to another user', NEW.acting_user_id, NEW.job_id
                USING ERRCODE = '23514', CONSTRAINT = 'job_event_actor_is_the_owner';
        END IF;
    END IF;
    RETURN NEW;
END
$fn$;
REVOKE ALL ON FUNCTION platform.job_events_check_actor() FROM PUBLIC;
CREATE TRIGGER job_events_check_actor BEFORE INSERT ON platform.job_events FOR EACH ROW EXECUTE FUNCTION platform.job_events_check_actor();

-- ── 7. the guard: legal pair, actor, capability, time ───────────────────────────────────────────────────────────────
CREATE FUNCTION platform.jobs_transition_guard() RETURNS trigger
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
REVOKE ALL ON FUNCTION platform.jobs_transition_guard() FROM PUBLIC;
CREATE TRIGGER jobs_transition_guard BEFORE INSERT OR UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_transition_guard();

-- the event of every move, in the same transaction
CREATE FUNCTION platform.jobs_record_transition() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_kind   text := NULLIF(btrim(current_setting('platform.actor_kind', true)), '');
    v_id     text := NULLIF(btrim(current_setting('platform.actor_id', true)), '');
    v_reason text := NULLIF(btrim(current_setting('platform.reason', true)), '');
BEGIN
    INSERT INTO platform.job_events (job_id, kind, from_status, to_status, actor_kind, acting_user_id, acting_admin_id, reason)
    VALUES (NEW.id, 'transition', OLD.status, NEW.status, v_kind,
            CASE WHEN v_kind = 'user'  THEN v_id::uuid END,
            CASE WHEN v_kind = 'admin' THEN v_id::uuid END,
            v_reason);
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_record_transition() IS 'SECURITY INVOKER: writes the platform.job_events row of a status change from the three transaction-local settings the guard already checked. A failure here (an unknown or disabled admin) aborts the UPDATE.';
REVOKE ALL ON FUNCTION platform.jobs_record_transition() FROM PUBLIC;
CREATE TRIGGER jobs_record_transition AFTER UPDATE OF status ON platform.jobs FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION platform.jobs_record_transition();

-- ── 8. the one sanctioned way to move a job ──────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.job_transition(p_job uuid, p_to text, p_actor_kind text, p_actor_id uuid, p_reason text) RETURNS platform.jobs
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    r platform.jobs;
    v_rows integer;
BEGIN
    PERFORM set_config('platform.actor_kind', COALESCE(p_actor_kind, ''), true);
    PERFORM set_config('platform.actor_id', COALESCE(p_actor_id::text, ''), true);
    PERFORM set_config('platform.reason', COALESCE(p_reason, ''), true);
    UPDATE platform.jobs SET status = p_to WHERE id = p_job RETURNING * INTO r;
    GET DIAGNOSTICS v_rows = ROW_COUNT;                -- (FOUND would be overwritten by the PERFORMs below)
    -- the settings were for this one statement: they do not stay behind for the next UPDATE of the transaction
    PERFORM set_config('platform.actor_kind', '', true);
    PERFORM set_config('platform.actor_id', '', true);
    PERFORM set_config('platform.reason', '', true);
    IF v_rows = 0 THEN
        RAISE EXCEPTION 'platform.job_transition: job % does not exist', p_job USING ERRCODE = 'P0002';
    END IF;
    RETURN r;
END
$fn$;
COMMENT ON FUNCTION platform.job_transition(uuid, text, text, uuid, text) IS 'SECURITY INVOKER: moves a job to a status with its actor and reason (SQLSTATE and constraint names of jobs_transition_guard when the move is refused). The store''s way for a plain move; a move that changes other columns in the same statement sets the same three settings and updates itself.';
REVOKE ALL ON FUNCTION platform.job_transition(uuid, text, text, uuid, text) FROM PUBLIC;

-- ── 9. the frozen columns, amended: a previewed job may still change its preview ───────────────────────────────────
CREATE OR REPLACE FUNCTION platform.jobs_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.module_id IS DISTINCT FROM OLD.module_id OR NEW.source_job_id IS DISTINCT FROM OLD.source_job_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'platform.jobs: user_id, module_id, source_job_id and created_at never change' USING ERRCODE = '23001';
    END IF;
    IF OLD.status NOT IN ('draft', 'previewed') AND (NEW.frozen_settings IS DISTINCT FROM OLD.frozen_settings OR NEW.preview IS DISTINCT FROM OLD.preview
       OR NEW.preview_fingerprint IS DISTINCT FROM OLD.preview_fingerprint OR NEW.confirm_key IS DISTINCT FROM OLD.confirm_key) THEN
        RAISE EXCEPTION 'platform.jobs: frozen_settings, the preview and the confirm key are frozen once the job has left previewed (it was % )', OLD.status USING ERRCODE = '23001';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;

-- the terminal set gains expired
CREATE OR REPLACE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF OLD.status IN ('succeeded', 'failed', 'cancelled', 'expired') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'platform.jobs: job % is % and a terminal status is final: it cannot become %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'jobs_terminal_is_final';   -- restrict_violation
    END IF;
    RETURN NEW;
END
$fn$;

-- ── 10. privileges ──────────────────────────────────────────────────────────────────────────────────────────────────
GRANT SELECT ON platform.job_transitions TO platform_app, platform_admin;
GRANT SELECT, INSERT ON platform.job_events TO platform_app;
GRANT SELECT ON platform.job_events TO platform_admin;
GRANT EXECUTE ON FUNCTION platform.job_transition(uuid, text, text, uuid, text) TO platform_app;

INSERT INTO platform.schema_migrations (version) VALUES ('0011');
