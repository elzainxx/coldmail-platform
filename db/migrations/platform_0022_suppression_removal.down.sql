-- platform_0022_suppression_removal.down.sql — undo platform_0022: platform_app loses the removal privilege, the history trigger and its function go, the history table is dropped, and the suppression list's comment is platform_0009's text again.
-- REFUSES (55000) while the history holds a row: dropping it would erase the only record that a user removed an address (an address that may have unsubscribed itself and was put back on their list of recipients).
-- (Rehearsal on an empty database or a deploy that failed before any user removed an entry; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.)
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM platform.suppression_removals;
    IF n > 0 THEN
        RAISE EXCEPTION 'platform_0022 down: % removal(s) of a suppression are on record; dropping the history would erase the only trace of them', n USING ERRCODE = '55000';
    END IF;
END
$$;

REVOKE DELETE ON platform.suppressions FROM platform_app;
DROP INDEX platform.suppressions_user_created_idx;
DROP TRIGGER suppressions_record_removal ON platform.suppressions;
DROP FUNCTION platform.suppressions_record_removal();
DROP TABLE platform.suppression_removals;

COMMENT ON TABLE platform.suppressions IS 'Per-user suppressed recipient addresses, in the platform''s one address form (internal/norm Validate: module 1''s norm/v1, then its syntax layer), applied to every future job. Hard bounces are NOT auto-suppressed (locked decision). Rows are never updated; nothing deletes one until a removal flow is designed.';

DELETE FROM platform.schema_migrations WHERE version = '0022';
