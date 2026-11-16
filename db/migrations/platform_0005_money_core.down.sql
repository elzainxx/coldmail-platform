-- platform_0005_money_core.down.sql — undo platform_0005: the per-item charge key, the one-reserve and one-release indexes, the reserve and release checks, platform.reserves.
-- Refuses (the column or constraint cannot be dropped cleanly) never on money history: the ledger rows stay; only the structure that guarded them goes. A charge row loses its item_ref column value
-- with the column (the ledger is append-only for ROW changes, not for dropping a column), so run this only on a rehearsal or a failed deploy; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
SET LOCAL ROLE platform_owner;

DROP TRIGGER ledger_stamp_created_at ON platform.ledger;
DROP FUNCTION platform.ledger_stamp_created_at();
DROP TRIGGER ledger_check_release ON platform.ledger;
DROP FUNCTION platform.ledger_check_release();
DROP TRIGGER ledger_apply_to_reserves ON platform.ledger;
DROP FUNCTION platform.ledger_apply_to_reserves();
DROP TABLE platform.reserves;
DROP TRIGGER ledger_check_reserve ON platform.ledger;
DROP FUNCTION platform.ledger_check_reserve();
DROP INDEX platform.ledger_one_release_per_reserve;
DROP INDEX platform.ledger_one_reserve_per_job;
DROP INDEX platform.ledger_one_charge_per_item;
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_charge_is_one_item;
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_item_ref_shape;
ALTER TABLE platform.ledger DROP COLUMN item_ref;

DELETE FROM platform.schema_migrations WHERE version = '0005';
