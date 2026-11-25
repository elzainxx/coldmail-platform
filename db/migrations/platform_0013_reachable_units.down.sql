-- platform_0013_reachable_units.down.sql — undo platform_0013: the reachability flag, its history, its writer and the two checks; ledger_check_reserve goes back to counting EVERY unit of the module (platform_0005). A job
-- confirmed while a unit was unreachable keeps its snapshot (the ledger is untouched); a NEW reserve after the down needs the full snapshot again, which is the Phase 2 behaviour (module 1 reserved at the vendor price).
-- Never refuses (rehearsal or a failed deploy only; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

CREATE OR REPLACE FUNCTION platform.ledger_check_reserve() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_snapshot integer;
    v_units    integer;
    v_dearest  integer;
BEGIN
    SELECT count(*), COALESCE(max(cost), 0) INTO v_snapshot, v_dearest FROM platform.job_unit_prices WHERE job_id = NEW.job_id;
    SELECT count(*) INTO v_units FROM platform.unit_costs WHERE module_id = NEW.module_id;
    IF v_snapshot = 0 OR v_snapshot <> v_units THEN
        RAISE EXCEPTION 'a reserve needs the job''s complete price snapshot: job % has % price row(s), module % has % unit(s)', NEW.job_id, v_snapshot, NEW.module_id, v_units
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_needs_the_complete_snapshot';
    END IF;
    IF NEW.unit_cost IS DISTINCT FROM v_dearest THEN
        RAISE EXCEPTION 'a reserve is held at the DEAREST frozen price of the job (%), not at %', v_dearest, NEW.unit_cost
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_at_the_dearest_price';
    END IF;
    RETURN NEW;
END
$fn$;

DROP TRIGGER job_unit_prices_only_reachable ON platform.job_unit_prices;
DROP FUNCTION platform.job_unit_prices_only_reachable();
DROP FUNCTION platform.set_unit_reachable(text, boolean, text);
DROP TRIGGER unit_costs_record_reachability ON platform.unit_costs;
DROP FUNCTION platform.unit_costs_record_reachability();
DROP TABLE platform.unit_reachability_history;
ALTER TABLE platform.unit_costs DROP CONSTRAINT unit_costs_reachable_source_not_blank;
ALTER TABLE platform.unit_costs DROP COLUMN reachable_changed_at, DROP COLUMN reachable_source, DROP COLUMN reachable;

DELETE FROM platform.schema_migrations WHERE version = '0013';
