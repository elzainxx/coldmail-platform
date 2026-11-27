-- platform_0022_suppression_removal.up.sql — Phase 6, chunk A4: a user may remove an address from their own suppression list, and every removal is recorded (Docs/phase-6-spec.md, decision 7).
--
-- THE RULING. Removal is allowed (owner brief) and recorded: the screen asks for a confirmation and says plainly that removing an address that unsubscribed itself makes the user responsible for mailing it again. The
-- list was append-only by privilege alone (platform_0003 granted platform_app SELECT and INSERT and nothing else; no trigger stands on the table), so this migration is two things:
--
--   1. platform.suppression_removals   an append-only history, one row per removed entry: the user whose list it was (the only person who can remove an entry: the store's removal is scoped by the user id of the
--                                      authenticated principal, so the user is the actor), the address, the entry's own source and the instant the entry was created, and the database's clock for the removal. It is written by a TRIGGER
--                                      of the suppression list (AFTER the removal of a row, FOR EACH ROW), so no writer can skip it, the owner and a superuser included: only a TRUNCATE could, and no application role holds that
--                                      privilege (the table is left as platform_0003 left it in that respect: nothing here adds a trigger that refuses it). The history refuses UPDATE, DELETE and TRUNCATE for every role, like the other
--                                      history tables (platform.user_settings_history of platform_0020 is the pattern copied). Nobody writes it by hand: platform_app and platform_admin hold SELECT on it and nothing else.
--   2. the grant                       platform_app may remove a row of platform.suppressions (table-level, the one new privilege). The rows are still never updated, and platform_admin still only reads.
--   3. the index                       suppressions_user_created_idx, for the newest-first keyset paging of the list a user reads (an index changes no rule).
--
-- WHAT IT CARRIES OVER. No trigger on platform.suppressions is replaced (there is none: the ban on removal was the missing privilege), so nothing has to be carried; the CHECK constraints, the primary key and the grants of
-- platform_0003, platform_0004 and platform_0009 are untouched. The one thing replaced is the TABLE COMMENT, whose last sentence said that nothing deletes a row until a removal flow is designed: it now says how a row goes. The
-- gate scripts/check-suppression-writers.sh keeps the removal where the insert is: the one statement that removes a row sits in internal/suppress/store.go after a call of the address validator, in the same function.
-- NULL TRAP (CLAUDE.md rule 13): every CHECK below is over NOT NULL columns.
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

CREATE TABLE platform.suppression_removals (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id          uuid        NOT NULL,
    email            text        NOT NULL,
    source           text        NOT NULL,
    entry_created_at timestamptz NOT NULL,
    removed_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT suppression_removals_user_fk FOREIGN KEY (user_id) REFERENCES public.users (id) ON DELETE RESTRICT,
    CONSTRAINT suppression_removals_not_the_system_user CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CONSTRAINT suppression_removals_source_vocabulary CHECK (source IN ('unsubscribe', 'manual', 'import'))
);
CREATE INDEX suppression_removals_user_idx ON platform.suppression_removals (user_id, id DESC);
COMMENT ON TABLE platform.suppression_removals IS 'Append-only. One row per entry a user removed from their own suppression list, written by the trigger of platform.suppressions: who (the user whose list it was), which address, the entry''s own source and creation time, and when it was removed (the database''s clock). The address is kept on purpose: it is the evidence that an address which unsubscribed itself was put back on a user''s list of recipients.';
COMMENT ON COLUMN platform.suppression_removals.entry_created_at IS 'The created_at of the entry that was removed (when the address was first suppressed); removed_at is when it stopped being.';

CREATE FUNCTION platform.suppressions_record_removal() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    INSERT INTO platform.suppression_removals (user_id, email, source, entry_created_at)
    VALUES (OLD.user_id, OLD.email, OLD.source, OLD.created_at);
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.suppressions_record_removal() IS 'SECURITY DEFINER (owner platform_owner): writes the history row of a removed suppression, so platform_app needs no INSERT on the history table. Fires for every removed row, whoever removes it.';
REVOKE ALL ON FUNCTION platform.suppressions_record_removal() FROM PUBLIC;
CREATE TRIGGER suppressions_record_removal AFTER DELETE ON platform.suppressions FOR EACH ROW EXECUTE FUNCTION platform.suppressions_record_removal();

CREATE TRIGGER suppression_removals_append_only BEFORE UPDATE OR DELETE ON platform.suppression_removals FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER suppression_removals_no_truncate BEFORE TRUNCATE ON platform.suppression_removals FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- the list a user reads is paged newest first by (created_at, address): the key of GET /v1/suppressions (internal/suppress List). A keyset page then reads only its own rows of the user, not the whole list.
CREATE INDEX suppressions_user_created_idx ON platform.suppressions (user_id, created_at DESC, email DESC);

-- privileges: the application removes a row of the list (and still inserts and reads it); both roles read the history and nobody writes it (the trigger does)
GRANT DELETE ON platform.suppressions TO platform_app;
GRANT SELECT ON platform.suppression_removals TO platform_app, platform_admin;

COMMENT ON TABLE platform.suppressions IS 'Per-user suppressed recipient addresses, in the platform''s one address form (internal/norm Validate: module 1''s norm/v1, then its syntax layer), applied to every future job. Hard bounces are NOT auto-suppressed (locked decision). Rows are never updated; a row goes only when its user removes it (internal/suppress Remove, scoped by the user id of the authenticated principal), and every removal is recorded in platform.suppression_removals by a trigger.';

INSERT INTO platform.schema_migrations (version) VALUES ('0022');
