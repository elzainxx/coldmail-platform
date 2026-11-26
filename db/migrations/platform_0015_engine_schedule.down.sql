-- platform_0015_engine_schedule.down.sql — undo platform_0015: the schedule trigger and its function, the three annotation columns and the comment on public.job_items.status go back to what platform_0001 made. Never refuses: the
-- columns are the engine's scheduling hints and the record of one idempotent write, nothing a customer or the ledger depends on (a job whose items_ensured_at is dropped has its module-facing rows written again, harmlessly, by
-- the next engine pass). The terminal marker 'cancelled' that internal/jobs wrote on never-submitted items stays: the down never edits a row of a table the modules read (the migrator refuses `down` unless
-- PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

DROP TRIGGER jobs_engine_schedule ON platform.jobs;
DROP FUNCTION platform.jobs_engine_schedule();

ALTER TABLE platform.jobs DROP CONSTRAINT jobs_idle_polls_not_negative;
ALTER TABLE platform.jobs DROP COLUMN items_ensured_at, DROP COLUMN idle_polls, DROP COLUMN next_poll_at;

COMMENT ON COLUMN public.job_items.status IS 'Written as ''pending'' by module 3, never read by a module. The platform owns the vocabulary; it must keep accepting ''pending''.';

DELETE FROM platform.schema_migrations WHERE version = '0015';
