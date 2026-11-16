-- platform_0007_snapshot_and_terminal_states.down.sql — undo platform_0007: the snapshot-price check and the terminal-state rule. Both are triggers and their functions, so the older tables, rows and
-- constraints are untouched and jobs_guard was never replaced: nothing has to be restored. Never refuses: no data depends on either rule. A snapshot written while the check was off may differ from the table, and a
-- terminal job may have been moved by hand; the down does not look at either (rehearsal or a failed deploy only; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

DROP TRIGGER jobs_terminal_is_final ON platform.jobs;
DROP FUNCTION platform.jobs_terminal_is_final();
DROP TRIGGER job_unit_prices_snapshot_equals_unit_costs ON platform.job_unit_prices;
DROP FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs();

DELETE FROM platform.schema_migrations WHERE version = '0007';
