-- platform_0017_job_items_unsubscribe_url.down.sql — undo platform_0017: the column and its CHECK are dropped, and whatever links it held are lost (the messages already sent were sent, and their links keep working: a
-- link is derived from the item id, not read back from this column). FAILS with SQLSTATE 2BP01 only if a module object depends on the column (none does in the platform's own schema; a module that copied it
-- into its own jobs table holds its own copy). Rehearsal or a failed deploy only (the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true). Roll the modules back first if they refuse to run without
-- the column (their pre-flight).
SET LOCAL ROLE platform_owner;

ALTER TABLE public.job_items DROP COLUMN unsubscribe_url;

DELETE FROM platform.schema_migrations WHERE version = '0017';
