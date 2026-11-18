-- platform_0010_job_items_rendered_text.up.sql — the optional plain-text part of a rendered message. Phase 3 close-out, items B3 and C5 (the plain-text part modules 2 and 3 now build, which P5's locked "HTML plus a generated plain-text part" needs).
--
-- The message body does not reach a module through its API: the platform writes one row of public.job_items per recipient (campaign_id, user_id, recipient_email, rendered_subject, rendered_body,
-- reply_to, ...) and calls POST /v1/run with only the row's id; the module reads the row and freezes it into its own jobs table (db/module-statements.txt, statement J2). The plain-text alternative
-- therefore travels the same way, as ONE MORE COLUMN of the shared table:
--
--   * public.job_items.rendered_text   text, NULL, no default. NULL, the empty string and white space only all mean "no plain-text part": the module sends the single text/html part it has always sent,
--     byte for byte. A real text makes the module build multipart/alternative (text/plain first, then text/html). The module caps it at 1 MiB (its own jobs table has the matching CHECK).
--
-- The rule of module-reads.txt holds: EVERY column of public.job_items that module 3's test-send INSERT does not supply must be nullable or defaulted, and this one is nullable, so that INSERT (six
-- columns) is unchanged. The privileges are the table's (SELECT for the two module roles, SELECT/INSERT/UPDATE for platform_app): a table-level grant covers a new column.
--
-- The modules refuse to run against a database that lacks the column (module 2's pre-flight, db/preflight/check_platform_tables.sql), so this migration must be applied BEFORE a module that reads
-- rendered_text is migrated or started: the harness applies the platform chain first, and install.md says so.
--
-- Created AS platform_owner (see platform_0001: the public tables are re-owned to it).

SET LOCAL ROLE platform_owner;

ALTER TABLE public.job_items ADD COLUMN rendered_text text;
COMMENT ON COLUMN public.job_items.rendered_text IS 'OPTIONAL plain-text alternative of rendered_body. NULL, empty or white space only: no plain-text part, the message is the single text/html part. Otherwise modules 2 and 3 send multipart/alternative with this text first. Read by the modules (statement J2); module 3''s test-send INSERT omits it.';

INSERT INTO platform.schema_migrations (version) VALUES ('0010');
