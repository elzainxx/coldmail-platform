-- platform_0019_template_rows.down.sql — undo platform_0019: the two CHECKs are platform_0012's text again (five states, and only a malformed or excluded row must say why). REFUSES (55000) while any row is in
-- state missing_field or unrenderable: the older vocabulary cannot hold it, and the row has a platform result (P-09 or P-10) that is kept forever and is never destroyed by a down, so it is not rewritten
-- into another state either. (Rehearsal on an empty database or a deploy that failed before any send job was confirmed; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.)
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM platform.job_rows WHERE state IN ('missing_field', 'unrenderable');
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0019 down: % row(s) are in state missing_field or unrenderable; their results are kept forever and the older vocabulary cannot hold the states', n USING ERRCODE = '55000';
    END IF;
END
$$;

COMMENT ON COLUMN platform.job_rows.state IS NULL;

ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_reason_shape;
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_reason_shape CHECK (state NOT IN ('malformed', 'excluded') OR (reason IS NOT NULL AND btrim(reason) <> ''));

ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_state_vocabulary;
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_state_vocabulary CHECK (state IN ('pending', 'malformed', 'duplicate', 'suppressed', 'excluded'));

DELETE FROM platform.schema_migrations WHERE version = '0019';
