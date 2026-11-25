-- platform_0012_job_rows_and_results.down.sql — undo platform_0012: the proof view, the two tables and their triggers. REFUSES while any job row or result exists: a result is a record of money and of a customer's outcome, kept
-- forever by the locked rule, and the down never destroys it (rehearsal on an empty database, or a deploy that failed before any job was created; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n bigint;
BEGIN
    SELECT (SELECT count(*) FROM platform.job_rows) + (SELECT count(*) FROM platform.job_results) INTO n;
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0012 down: % job row(s) and result(s) exist; results are kept forever and are never dropped by a down', n USING ERRCODE = '55000';
    END IF;
END
$$;

DROP VIEW platform.job_settlement_drift;
DROP TABLE platform.job_results;
DROP FUNCTION platform.job_results_after_confirm();
DROP TABLE platform.job_rows;
DROP FUNCTION platform.job_rows_frozen();

DELETE FROM platform.schema_migrations WHERE version = '0012';
