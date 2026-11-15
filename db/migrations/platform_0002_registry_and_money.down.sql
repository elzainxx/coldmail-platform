-- platform_0002_registry_and_money.down.sql — remove everything platform_0002 created, in dependency order.
-- Destroys the ledger and every balance. Meant for a database that holds no real money (a rehearsal or a failed deploy); the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
-- The ledger's append-only triggers do not stop DROP TABLE (they guard rows, not the table): that is the owner's residual power, stated in Docs/phase-1-spec.md.
SET LOCAL ROLE platform_owner;

DROP VIEW  platform.reserve_overdrawn;
DROP VIEW  platform.balance_drift;
DROP TABLE platform.m3_entitlements;
DROP TABLE platform.ledger;
DROP TABLE platform.balances;
DROP TABLE platform.job_unit_prices;
DROP TABLE platform.jobs;
DROP TABLE platform.unit_cost_history;
DROP TABLE platform.unit_costs;
DROP TABLE platform.modules;
DROP TABLE platform.admins;
DROP FUNCTION platform.ledger_apply_to_balance();
DROP FUNCTION platform.jobs_guard();
DROP FUNCTION platform.unit_costs_record_history();
DROP FUNCTION platform.unit_costs_guard();
DROP FUNCTION platform.reject_mutation();

DELETE FROM platform.schema_migrations WHERE version = '0002';
