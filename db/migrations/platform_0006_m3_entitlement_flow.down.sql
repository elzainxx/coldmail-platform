-- platform_0006_m3_entitlement_flow.down.sql — undo platform_0006: the delivery bookkeeping, the ledger-written entitlements, the `until_at` column.
-- The ledger rows stay (a rehearsal or a failed deploy only; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true). Dropping `until_at` loses the end dates the m3_enable rows carried.
SET LOCAL ROLE platform_owner;

DROP TRIGGER m3_entitlements_raise_sync ON platform.m3_entitlements;
DROP FUNCTION platform.m3_entitlements_raise_sync();
DROP TABLE platform.m3_sync;

GRANT INSERT, UPDATE ON platform.m3_entitlements TO platform_app;
COMMENT ON TABLE platform.m3_entitlements IS 'M3 is not metered: an admin enables it per user, optionally until a date, after which it auto-disables. The ledger records every change (kinds m3_enable, m3_disable, m3_expire). changed_by is NULL only for the automatic expiry.';
DROP TRIGGER ledger_apply_to_entitlements ON platform.ledger;
DROP FUNCTION platform.ledger_apply_to_entitlements();

ALTER TABLE platform.ledger DROP CONSTRAINT ledger_until_at_shape;
ALTER TABLE platform.ledger DROP COLUMN until_at;

DELETE FROM platform.schema_migrations WHERE version = '0006';
