-- platform_0019_template_rows.up.sql — Phase 5, chunk S2. A SEND job's confirm leaves out the rows its templates cannot be rendered for, free: two new states of platform.job_rows. Docs/templates.md, "In a job".
--
--   missing_field   the row lacks a merge field the job's templates need and give no fallback for (Docs/templates.md, "What missing means"). The row's reason names the FIELDS (never a value).
--                   The platform's own result of the row is rule P-09 (internal/charge/rules.psv), free.
--   unrenderable    the row's rendered message is one the shared table refuses (a blank subject after rendering, a rendered part over the size limit, no body). The row's reason is a WORD naming the refusal
--                   (subject_blank, html_too_large, ...). The platform's own result is rule P-10, free.
--
-- Both are set ONLY by the confirm of a send job (internal/jobs), next to the user's suppression list and before the reserve: a row in either state is never given a public.job_items row, is not counted in the
-- reserve, is never handed to a module and gets its free platform result in the same transaction. The rows are written while the job is previewed, so the frozen-rows guard (platform.job_rows_frozen) lets
-- them through: it does not enumerate states and is NOT replaced here.
--
-- WHAT THIS MIGRATION REPLACES (each diffed against its latest definition, which is platform_0012's: platform_0014 added constraints beside these two and replaced two guard functions, never these):
--   job_rows_state_vocabulary  CARRIED: pending, malformed, duplicate, suppressed, excluded; ADDED: missing_field, unrenderable.
--   job_rows_reason_shape      CARRIED: a malformed or excluded row says why (reason IS NOT NULL and not blank); ADDED: the same for missing_field and unrenderable (the reason is what the user is told).
-- WHAT IT LEAVES AS IT IS, ON PURPOSE: the unique index job_rows_one_pending_per_address (a skipped row does not take the seat of its address: nothing is sent to it and the job is no longer editable once a row is
-- skipped); job_rows_email_norm_shape (a skipped row keeps the normalised address it had); job_rows_only_pending_is_submitted (a skipped row was never submitted); job_rows_flag_only_on_listable_rows and
-- job_rows_suppressed_state_has_the_flag (the flag is the suppression list's and a row it holds is already state suppressed when the pass over the templates runs); the results' module_state, which is free text
-- (the platform's own result of a skipped row carries the row's state); platform.job_settlement_drift (a result per row, whatever the state); the two guards of platform_0014.
-- NULL TRAP (CLAUDE.md rule 13): both CHECKs are boolean expressions over the NOT NULL column state, and the reason test says IS NOT NULL.
-- An UPGRADE of a database that holds jobs: no row is in either new state, so nothing is rewritten; the two constraints are re-validated over the existing rows (they were all valid under the older, narrower text).
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_state_vocabulary;
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_state_vocabulary
    CHECK (state IN ('pending', 'malformed', 'duplicate', 'suppressed', 'excluded', 'missing_field', 'unrenderable'));

ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_reason_shape;
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_reason_shape
    CHECK (state NOT IN ('malformed', 'excluded', 'missing_field', 'unrenderable') OR (reason IS NOT NULL AND btrim(reason) <> ''));

COMMENT ON COLUMN platform.job_rows.state IS 'pending (will reach the module), malformed, duplicate, suppressed (the user''s list, at confirm), excluded (a chained send''s verdict), missing_field and unrenderable (a send job''s templates cannot be rendered for the row, set at confirm: free, P-09 and P-10). The vocabulary is the CHECK job_rows_state_vocabulary.';

INSERT INTO platform.schema_migrations (version) VALUES ('0019');
