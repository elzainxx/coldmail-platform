-- platform_0011_job_lifecycle.down.sql — undo platform_0011: the lifecycle guard, the transition table, the event log, the capability columns, the new job columns and the wider slot index; the status vocabulary, jobs_guard,
-- jobs_terminal_is_final and the slot index go back to what platform_0002, platform_0004 and platform_0007 made. REFUSES while a job is in a state the older vocabulary does not know (previewed, confirmed, completing,
-- expired): ending such a job is a decision about money and is not made here (the older CHECK would refuse the row anyway; this says so first). The events and the preview data of the new columns are discarded (rehearsal or a
-- failed deploy only; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n integer;
BEGIN
    SELECT count(*) INTO n FROM platform.jobs WHERE status IN ('previewed', 'confirmed', 'completing', 'expired');
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0011 down: % job(s) are previewed, confirmed, completing or expired, states the older vocabulary does not have; end or move them first', n USING ERRCODE = '55000';
    END IF;
END
$$;

DROP TRIGGER jobs_record_transition ON platform.jobs;
DROP TRIGGER jobs_transition_guard ON platform.jobs;
DROP FUNCTION platform.jobs_record_transition();
DROP FUNCTION platform.job_transition(uuid, text, text, uuid, text);
DROP FUNCTION platform.jobs_transition_guard();

DROP TABLE platform.job_events;
DROP FUNCTION platform.job_events_check_actor();
DROP TABLE platform.job_transitions;

-- the older frozen-columns guard (platform_0002) and the older terminal set (platform_0007)
CREATE OR REPLACE FUNCTION platform.jobs_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.module_id IS DISTINCT FROM OLD.module_id OR NEW.source_job_id IS DISTINCT FROM OLD.source_job_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'platform.jobs: user_id, module_id, source_job_id and created_at never change' USING ERRCODE = '23001';
    END IF;
    IF OLD.status <> 'draft' AND NEW.frozen_settings IS DISTINCT FROM OLD.frozen_settings THEN
        RAISE EXCEPTION 'platform.jobs: frozen_settings is frozen once the job has left draft (it was % )', OLD.status USING ERRCODE = '23001';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;
CREATE OR REPLACE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF OLD.status IN ('succeeded', 'failed', 'cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'platform.jobs: job % is % and a terminal status is final: it cannot become %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'jobs_terminal_is_final';   -- restrict_violation
    END IF;
    RETURN NEW;
END
$fn$;

DROP INDEX platform.jobs_one_active_per_user_module;
CREATE UNIQUE INDEX jobs_one_active_per_user_module ON platform.jobs (user_id, module_id) WHERE status IN ('scheduled', 'running', 'paused', 'cancelling');

DROP INDEX platform.jobs_confirm_key_key;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirmed_starts_now;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_start_needs_a_deadline;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirmed_shape;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_fingerprint_shape;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_preview_is_an_object;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirm_key_shape;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_paused_from_shape;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_paused_from_vocabulary;
ALTER TABLE platform.jobs DROP COLUMN cancel_requested_at, DROP COLUMN stale_alerted_at, DROP COLUMN last_progress_at, DROP COLUMN start_deadline, DROP COLUMN finished_at,
    DROP COLUMN started_at, DROP COLUMN confirmed_at, DROP COLUMN previewed_at, DROP COLUMN preview_fingerprint, DROP COLUMN preview, DROP COLUMN confirm_key, DROP COLUMN paused_from;

ALTER TABLE platform.modules DROP COLUMN can_cancel, DROP COLUMN can_pause;

ALTER TABLE platform.jobs DROP CONSTRAINT jobs_status_vocabulary;
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_status_vocabulary CHECK (status IN ('draft', 'scheduled', 'running', 'paused', 'cancelling', 'succeeded', 'failed', 'cancelled'));

DELETE FROM platform.schema_migrations WHERE version = '0011';
