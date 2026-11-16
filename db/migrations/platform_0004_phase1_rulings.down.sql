-- platform_0004_phase1_rulings.down.sql — undo platform_0004: the scheduled-job slot, the system-user checks and view, the disabled-admin triggers, the `adjustment` ledger kind.
-- Refuses (the CHECK cannot be re-created) when an `adjustment` row exists: the ledger is append-only, so the down never deletes money history. Meant for a rehearsal or a failed deploy; the migrator
-- refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.
SET LOCAL ROLE platform_owner;

-- adjustment kind
DROP TRIGGER ledger_audit_adjustment ON platform.ledger;
DROP FUNCTION platform.ledger_audit_adjustment();
DROP VIEW platform.balance_drift;
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_adjustment;
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_kind_vocabulary;
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_kind_vocabulary CHECK (kind IN ('grant', 'reserve', 'charge', 'release', 'm3_enable', 'm3_disable', 'm3_expire'));
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_module_metering_fk;
ALTER TABLE platform.ledger DROP COLUMN monetary;
ALTER TABLE platform.ledger ADD COLUMN monetary boolean GENERATED ALWAYS AS (kind IN ('grant', 'reserve', 'charge', 'release')) STORED;
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_module_metering_fk FOREIGN KEY (module_id, monetary) REFERENCES platform.modules (id, metered);
COMMENT ON TABLE platform.ledger IS 'THE money record. Append-only: UPDATE, DELETE and TRUNCATE are refused for every role (privileges) and for the owner too (triggers). Each row carries signed integer deltas; balances is their sum, maintained by trigger. Kinds: grant, reserve, charge, release (monetary, M1/M2 only) and m3_enable, m3_disable, m3_expire (no credits, M3 only).';
CREATE VIEW platform.balance_drift AS
SELECT COALESCE(l.user_id, b.user_id)     AS user_id,
       COALESCE(l.module_id, b.module_id) AS module_id,
       COALESCE(b.available, 0)           AS stored_available,
       COALESCE(l.available, 0)           AS ledger_available,
       COALESCE(b.reserved, 0)            AS stored_reserved,
       COALESCE(l.reserved, 0)            AS ledger_reserved
FROM (SELECT user_id, module_id, sum(available_delta)::bigint AS available, sum(reserved_delta)::bigint AS reserved
        FROM platform.ledger WHERE monetary GROUP BY user_id, module_id) l
FULL JOIN platform.balances b ON b.user_id = l.user_id AND b.module_id = l.module_id
WHERE COALESCE(b.available, 0) <> COALESCE(l.available, 0) OR COALESCE(b.reserved, 0) <> COALESCE(l.reserved, 0);
COMMENT ON VIEW platform.balance_drift IS 'Every (user, module) whose stored balance differs from the sum of its ledger rows. Empty when the invariant holds. scripts/check-balance-invariant.sh fails on any row.';
GRANT SELECT ON platform.balance_drift TO platform_app, platform_admin;

-- disabled admins
DROP TRIGGER m3_entitlements_require_active_admin ON platform.m3_entitlements;
DROP FUNCTION platform.m3_entitlements_require_active_admin();
DROP TRIGGER admin_audit_require_active_admin ON platform.admin_audit;
DROP FUNCTION platform.admin_audit_require_active_admin();
DROP TRIGGER ledger_require_active_admin ON platform.ledger;
DROP FUNCTION platform.ledger_require_active_admin();
CREATE OR REPLACE FUNCTION platform.unit_costs_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.cost IS DISTINCT FROM OLD.cost THEN
        IF NEW.changed_by IS NULL OR NEW.change_reason IS NULL OR btrim(NEW.change_reason) = '' THEN
            RAISE EXCEPTION 'changing platform.unit_costs.cost for % needs changed_by (a named admin) and a non-blank change_reason', OLD.unit
                USING ERRCODE = '23514';
        END IF;
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;
DROP FUNCTION platform.require_active_admin(uuid, text);

-- the system user
DROP VIEW platform.listable_users;
ALTER TABLE platform.suppressions    DROP CONSTRAINT suppressions_not_the_system_user;
ALTER TABLE platform.m3_entitlements DROP CONSTRAINT m3_entitlements_not_the_system_user;
ALTER TABLE platform.ledger          DROP CONSTRAINT ledger_not_the_system_user;
ALTER TABLE platform.balances        DROP CONSTRAINT balances_not_the_system_user;

-- the slot
DROP INDEX platform.jobs_one_active_per_user_module;
CREATE UNIQUE INDEX jobs_one_executing_per_user_module ON platform.jobs (user_id, module_id) WHERE status IN ('running', 'paused', 'cancelling');

DELETE FROM platform.schema_migrations WHERE version = '0004';
