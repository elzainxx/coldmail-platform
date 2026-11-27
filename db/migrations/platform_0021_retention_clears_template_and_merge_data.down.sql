-- platform_0021_retention_clears_template_and_merge_data.down.sql — undo platform_0021: platform.job_rows_frozen() is platform_0014's text again (nothing about a row of an ended job changes but submitted_at), platform.jobs_guard() is
-- platform_0011's text again (no frozen column of an ended job changes, the template included), the comment of the two functions and of platform.jobs.content_cleared_at are the older ones, and jobs_content_retention_idx is platform_0018's
-- index again (only the jobs that carry a campaign).
-- REFUSES (SQLSTATE 55000, naming how many) while a job has been cleared: its template and its merge data are gone for good, and the older guards, which refuse any such change, would then stand over a database in which the
-- older purge's mark (the rendered messages only) can no longer be told from this one's. Putting that back is a decision about the customers' data and is not made here; the content that was cleared is NOT restored, and cannot be.
-- The migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n integer;
BEGIN
    SELECT count(*) INTO n FROM platform.jobs WHERE content_cleared_at IS NOT NULL;
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0021 down: % job(s) have had their template and merge data cleared by the content retention; that cannot be undone and the older guards would stand over it: leave platform_0021 in place', n USING ERRCODE = '55000';
    END IF;
END
$$;

-- the rows' guard of platform_0014
CREATE OR REPLACE FUNCTION platform.job_rows_frozen() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_module text;
    v_job    uuid := CASE WHEN TG_OP = 'INSERT' THEN NEW.job_id ELSE OLD.job_id END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id OR NEW.job_id IS DISTINCT FROM OLD.job_id OR NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
           OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'platform.job_rows: id, job_id, user_id, ordinal and created_at never change' USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
        END IF;
        IF NEW.submitted_at IS DISTINCT FROM OLD.submitted_at AND OLD.submitted_at IS NOT NULL THEN
            RAISE EXCEPTION 'platform.job_rows: row % was already submitted at %: the stamp is written once', OLD.id, OLD.submitted_at USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
        END IF;
        IF NEW.email_raw IS NOT DISTINCT FROM OLD.email_raw AND NEW.email_norm IS NOT DISTINCT FROM OLD.email_norm AND NEW.state IS NOT DISTINCT FROM OLD.state
           AND NEW.reason IS NOT DISTINCT FROM OLD.reason AND NEW.duplicate_of IS NOT DISTINCT FROM OLD.duplicate_of AND NEW.fields IS NOT DISTINCT FROM OLD.fields
           AND NEW.source_item IS NOT DISTINCT FROM OLD.source_item AND NEW.suppressed IS NOT DISTINCT FROM OLD.suppressed THEN
            RETURN NEW;                   -- only submitted_at moves
        END IF;
    END IF;
    SELECT j.status, j.module_id INTO v_status, v_module FROM platform.jobs j WHERE j.id = v_job;
    IF v_status IS NULL OR v_status NOT IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_rows: job % is % and its list can no longer change (a row changes only while the job is draft or previewed)', v_job, v_status
            USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
    END IF;
    IF TG_OP <> 'DELETE' THEN
        IF NEW.suppressed AND NEW.state = 'pending' AND v_module IS DISTINCT FROM 'm1' THEN
            RAISE EXCEPTION 'platform.job_rows: row % is pending and flagged suppressed in a job of module %: only a verify job keeps a suppressed address pending (a send job''s suppressed row is state suppressed and never sent)', NEW.id, v_module
                USING ERRCODE = '23514', CONSTRAINT = 'job_rows_pending_flag_is_verify_only';
        END IF;
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END
$fn$;
COMMENT ON FUNCTION platform.job_rows_frozen() IS 'SECURITY INVOKER: a row of a job changes only while the job is draft or previewed (23001 job_rows_frozen), the flag suppressed included; submitted_at, the engine''s stamp, is the one column that moves later, once. A pending row flagged suppressed is a verify job''s (23514 job_rows_pending_flag_is_verify_only).';

-- the job's guard of platform_0011
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
COMMENT ON FUNCTION platform.jobs_guard() IS NULL;

-- the index and the comment of platform_0018
DROP INDEX platform.jobs_content_retention_idx;
CREATE INDEX jobs_content_retention_idx ON platform.jobs (finished_at)
    WHERE content_cleared_at IS NULL AND status IN ('succeeded', 'failed', 'cancelled', 'expired') AND jsonb_exists(frozen_settings, 'content');
COMMENT ON COLUMN platform.jobs.content_cleared_at IS 'Phase 5 S8: when the job store cleared the rendered content of this send job''s rows in public.job_items (rendered_subject and rendered_body = [removed], rendered_text and unsubscribe_url NULL), after the last page. NULL: not cleared. Only a terminal job has it, never earlier than its finished_at, and it is set once. Outcomes (job_rows, job_results, job_events, the ledger) are never touched.';

DELETE FROM platform.schema_migrations WHERE version = '0021';
