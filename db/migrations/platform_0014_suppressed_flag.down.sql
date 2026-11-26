-- platform_0014_suppressed_flag.down.sql — undo platform_0014: the flag on the rows and on the results, their three CHECKs, and the amendments of the two guards (they go back to platform_0012's text). REFUSES (55000) while
-- a verify job holds the flag on a PENDING row or on a result of the module: those say that an address was on the user's suppression list although it was verified and charged, which the older schema cannot
-- say, and a result is kept forever and never destroyed by a down. What the older schema can say stays: a row whose state is `suppressed` and the platform's own P-SUP result keep their state. (Rehearsal on an empty database or
-- a deploy that failed before any verify job was confirmed; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.)
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n bigint;
BEGIN
    SELECT (SELECT count(*) FROM platform.job_rows WHERE suppressed AND state = 'pending') + (SELECT count(*) FROM platform.job_results WHERE suppressed AND origin = 'module') INTO n;
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0014 down: % flagged row(s) and result(s) of verify jobs exist; the flag is kept forever and is never dropped by a down', n USING ERRCODE = '55000';
    END IF;
END
$$;

-- the guards of platform_0012
CREATE OR REPLACE FUNCTION platform.job_rows_frozen() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
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
           AND NEW.source_item IS NOT DISTINCT FROM OLD.source_item THEN
            RETURN NEW;                   -- only submitted_at moves
        END IF;
    END IF;
    SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = v_job;
    IF v_status IS NULL OR v_status NOT IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_rows: job % is % and its list can no longer change (a row changes only while the job is draft or previewed)', v_job, v_status
            USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END
$fn$;
COMMENT ON FUNCTION platform.job_rows_frozen() IS 'SECURITY INVOKER: a row of a job changes only while the job is draft or previewed (23001 job_rows_frozen); submitted_at, the engine''s stamp, is the one column that moves later, once.';

CREATE OR REPLACE FUNCTION platform.job_results_after_confirm() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_email  text;
BEGIN
    SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = NEW.job_id;
    IF v_status IS NULL OR v_status IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_results: job % is % and has no results before it is confirmed', NEW.job_id, v_status
            USING ERRCODE = '23514', CONSTRAINT = 'jobs_results_after_confirm';
    END IF;
    -- a result names the address of ITS row (the normalised one, or the raw text of a row that did not normalise): the foreign key already ties the position
    SELECT COALESCE(w.email_norm, w.email_raw) INTO v_email FROM platform.job_rows w WHERE w.job_id = NEW.job_id AND w.id = NEW.item_ref;
    IF v_email IS DISTINCT FROM NEW.email THEN
        RAISE EXCEPTION 'platform.job_results: the result of row % names an address that is not the row''s', NEW.item_ref
            USING ERRCODE = '23514', CONSTRAINT = 'job_results_match_their_row';
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.job_results_after_confirm() IS 'SECURITY INVOKER: a result can be recorded only for a job that has left draft and previewed (23514 jobs_results_after_confirm) and must name the address of its own row (23514 job_results_match_their_row).';

ALTER TABLE platform.job_results DROP CONSTRAINT job_results_suppressed_platform_outcome;
ALTER TABLE platform.job_results DROP COLUMN suppressed;
ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_flag_only_on_listable_rows;
ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_suppressed_state_has_the_flag;
ALTER TABLE platform.job_rows DROP COLUMN suppressed;

DELETE FROM platform.schema_migrations WHERE version = '0014';
