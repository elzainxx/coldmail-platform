-- platform_0017_job_items_unsubscribe_url.up.sql — the per-recipient unsubscribe link of a message, as ONE MORE nullable column of the shared table. Phase 5, chunk S3 (decision 6 of Docs/phase-5-spec.md).
--
-- The message does not reach a module through its API: the platform writes one row of public.job_items per recipient and calls POST /v1/run with only the row's id; the module reads the row and freezes
-- it into its own jobs table (db/module-statements.txt, statement J2). The unsubscribe link travels the same way, exactly as rendered_text does (platform_0010):
--
--   * public.job_items.unsubscribe_url   text, NULL, no default. NULL means "no link": the message goes out as it always has, with no List-Unsubscribe header. A value is ONE https URL, the link the
--     recipient is shown (internal/unsubscribe: the platform's public base URL, "/u/" and the item's signed token). A module that knows the column (Phase 5, chunk S4) sends it as
--     `List-Unsubscribe: <url>` with `List-Unsubscribe-Post: List-Unsubscribe=One-Click`; a module that does not read it is unaffected, which is why the column is additive.
--
-- WHY A SINGLE URL AND NOT A HEADER MAP. A free map of headers would be an injection surface in two services. A URL the database has already checked cannot carry a line break, white space, an angle
-- bracket (the header's own delimiters), a control character or another scheme, so a module that puts it between the angle brackets of one header can never be made to write another header.
-- The CHECK below is that wall, in the database, where every writer passes it (the platform's store writes the column today; nothing else may write it, and the modules' role holds SELECT only).
--
-- THE CHECK job_items_unsubscribe_url_shape (rule 13: a CHECK over a nullable column says IS NULL first, so the NULL case passes by its own clause and not by accident):
--   NULL, or all of
--     * it starts with https:// and holds at least one more character, none of them white space, "<" or ">" (the regular expression ^https://[^[:space:]<>]+$; "$" matches only at the end of the
--       text in PostgreSQL's default mode, so a trailing line break is refused by the character class and not forgiven by the anchor);
--     * it is at most 2000 bytes (octet_length: a link the platform makes is ASCII, so this is also 2000 characters; the byte count is the stricter of the two for anything else);
--     * it holds no control character (the POSIX class [:cntrl:]: NUL cannot be stored in text, and DEL, which the first rule does not exclude, is refused here).
--
-- The rule of db/module-reads.txt holds: EVERY column of public.job_items that module 3's test-send INSERT does not supply must be nullable or defaulted, and this one is nullable, so that INSERT (six
-- columns, statement J4) is unchanged. The privileges are the table's (SELECT for the two module roles and platform_admin, SELECT/INSERT/UPDATE for platform_app, all at table level: platform_0001):
-- a table-level grant covers a new column, so no grant is issued here and none is needed.
--
-- A module that reads the column (Phase 5, S4) refuses to run against a database that lacks it (its pre-flight), so this migration must be applied BEFORE such a module is migrated or started: the
-- harness applies the platform chain first, and install.md says so.
--
-- Created AS platform_owner (see platform_0001: the public tables are re-owned to it).

SET LOCAL ROLE platform_owner;

ALTER TABLE public.job_items ADD COLUMN unsubscribe_url text;
ALTER TABLE public.job_items ADD CONSTRAINT job_items_unsubscribe_url_shape CHECK (
    unsubscribe_url IS NULL
    OR (
        unsubscribe_url ~ '^https://[^[:space:]<>]+$'
        AND octet_length(unsubscribe_url) <= 2000
        AND unsubscribe_url !~ '[[:cntrl:]]'
    )
);
COMMENT ON COLUMN public.job_items.unsubscribe_url IS 'OPTIONAL per-recipient unsubscribe link (Phase 5): NULL, or one https URL with no white space, angle bracket or control character, at most 2000 bytes (CHECK job_items_unsubscribe_url_shape). Written by the platform''s job store only, derived from the item id (internal/unsubscribe); a sender that knows the column sends it as List-Unsubscribe with List-Unsubscribe-Post: List-Unsubscribe=One-Click. Module 3''s test-send INSERT omits it.';

INSERT INTO platform.schema_migrations (version) VALUES ('0017');
