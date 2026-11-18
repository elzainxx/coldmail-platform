-- platform_0010_job_items_rendered_text.down.sql — undo platform_0010: the column is dropped, and whatever plain-text parts it held are lost (the messages already sent were sent). FAILS with SQLSTATE 2BP01
-- only if a module object depends on the column (none does in the platform's own schema; a module that copied it into its own jobs table holds its own copy). Rehearsal or a failed deploy only (the
-- migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true). Roll the modules back first if they refuse to run without the column (their pre-flight).
SET LOCAL ROLE platform_owner;

ALTER TABLE public.job_items DROP COLUMN rendered_text;

DELETE FROM platform.schema_migrations WHERE version = '0010';
