-- platform_0013_reachable_units.up.sql — Phase 4, chunk B3. A reserve is whole, dearest and once, OVER THE UNITS A JOB CAN ACTUALLY REACH (Docs/phase-4-spec.md, "Locked here" 23; amends Phase 2's Locked 17).
--
-- THE PROBLEM. Phase 2 reserved at the dearest price of EVERY unit of the module. Module 1's vendor unit (m1_vendor, 2 credits) exists in the price table but the vendor fallback ships DISABLED (the owner switches it on in
-- module 1's own configuration), so with it off every module 1 job would have reserved at 2 per row and a customer would have needed double the credits for a price the job could never be charged.
--
-- THE RULE. platform.unit_costs gains `reachable`: a unit that a job of its module can be charged under the module's CURRENT configuration. The price snapshot of a job holds exactly the reachable units, the reserve rate is the
-- dearest of those, and therefore a charge for an unreachable unit CANNOT be written: the ledger's composite foreign key to the snapshot (platform_0002) has no such unit. Both directions fail safe for the customer: the vendor
-- off but the platform told it is on reserves too much (released at the end); the vendor on but the platform told it is off reserves too little, a vendor answer is then flagged and never charged (the store, internal/money).
--
--   1. unit_costs.reachable (default true; m1_vendor is seeded false), reachable_source and reachable_changed_at. The ONLY writer is platform.set_unit_reachable(unit, reachable, source) (SECURITY DEFINER: the
--      application role holds UPDATE on three other columns of unit_costs and none on these), which the platform runs at start-up and on every engine pass from PLATFORM_M1_VENDOR_REACHABLE.
--   2. platform.unit_reachability_history: append-only, one row per change, written by trigger.
--   3. job_unit_prices_only_reachable: a snapshot row for an unreachable unit is refused (23514, constraint snapshot_only_reachable_units).
--   4. ledger_check_reserve counts the REACHABLE units of the module as the complete snapshot (reserve_needs_the_complete_snapshot), and still demands the dearest price of what the snapshot holds.
--
-- The change of a flag moves no existing job: a job's snapshot was frozen at its confirm and settlement uses it (Locked 7). A module with every unit unreachable would have an empty snapshot and no reserve: refused by the
-- existing rule (v_snapshot = 0), never silently reserved at nothing.
-- NULL TRAP (CLAUDE.md rule 13): reachable is NOT NULL and every comparison is boolean over NOT NULL columns. Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

ALTER TABLE platform.unit_costs ADD COLUMN reachable            boolean     NOT NULL DEFAULT true;
ALTER TABLE platform.unit_costs ADD COLUMN reachable_source     text;
ALTER TABLE platform.unit_costs ADD COLUMN reachable_changed_at timestamptz;
UPDATE platform.unit_costs SET reachable = false, reachable_source = 'seed: the vendor fallback ships disabled', reachable_changed_at = now() WHERE unit = 'm1_vendor';
ALTER TABLE platform.unit_costs ADD CONSTRAINT unit_costs_reachable_source_not_blank CHECK (reachable_source IS NULL OR btrim(reachable_source) <> '');
COMMENT ON COLUMN platform.unit_costs.reachable IS 'Phase 4: true when a job of the unit''s module can be charged this unit under the module''s current configuration. The price snapshot of a job holds only reachable units and the reserve rate is the dearest of them. Written only by platform.set_unit_reachable. m1_vendor is false until the owner switches the vendor fallback on (PLATFORM_M1_VENDOR_REACHABLE, mirrored from module 1''s TM_EXTERNAL_* dials).';

CREATE TABLE platform.unit_reachability_history (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    unit       text        NOT NULL REFERENCES platform.unit_costs (unit),
    reachable  boolean     NOT NULL,
    source     text        NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT unit_reachability_history_source_not_blank CHECK (btrim(source) <> '')
);
COMMENT ON TABLE platform.unit_reachability_history IS 'Append-only. One row per change of platform.unit_costs.reachable, written by trigger, with who said so (the source).';
CREATE TRIGGER unit_reachability_history_append_only BEFORE UPDATE OR DELETE ON platform.unit_reachability_history FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER unit_reachability_history_no_truncate BEFORE TRUNCATE ON platform.unit_reachability_history FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- the state the table starts in is the first thing the history says
INSERT INTO platform.unit_reachability_history (unit, reachable, source) SELECT unit, reachable, COALESCE(reachable_source, 'seed: reachable by default') FROM platform.unit_costs;

CREATE FUNCTION platform.unit_costs_record_reachability() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    INSERT INTO platform.unit_reachability_history (unit, reachable, source) VALUES (NEW.unit, NEW.reachable, COALESCE(NEW.reachable_source, 'unspecified'));
    RETURN NULL;
END
$fn$;
REVOKE ALL ON FUNCTION platform.unit_costs_record_reachability() FROM PUBLIC;
CREATE TRIGGER unit_costs_record_reachability AFTER UPDATE OF reachable ON platform.unit_costs FOR EACH ROW WHEN (OLD.reachable IS DISTINCT FROM NEW.reachable) EXECUTE FUNCTION platform.unit_costs_record_reachability();

-- the one writer of the flag
CREATE FUNCTION platform.set_unit_reachable(p_unit text, p_reachable boolean, p_source text) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_old boolean;
BEGIN
    IF p_reachable IS NULL OR p_source IS NULL OR btrim(p_source) = '' THEN
        RAISE EXCEPTION 'platform.set_unit_reachable: a unit is reachable or not, and the change says who said so (source)' USING ERRCODE = '22023';
    END IF;
    -- every instance asks once a minute: a call that changes nothing must take NO row lock (a pending FOR UPDATE would queue every new reserve, which holds FOR SHARE on these rows, behind it)
    SELECT reachable INTO v_old FROM platform.unit_costs WHERE unit = p_unit;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'platform.set_unit_reachable: no unit %', p_unit USING ERRCODE = 'P0002';
    END IF;
    IF v_old IS NOT DISTINCT FROM p_reachable THEN
        RETURN false;                   -- already so: nothing written, no lock, no history row
    END IF;
    SELECT reachable INTO v_old FROM platform.unit_costs WHERE unit = p_unit FOR UPDATE;   -- a change: wait for the reserves that hold the row, then decide again
    IF v_old IS DISTINCT FROM p_reachable THEN
        UPDATE platform.unit_costs SET reachable = p_reachable, reachable_source = p_source, reachable_changed_at = now() WHERE unit = p_unit;
        RETURN true;                    -- changed
    END IF;
    RETURN false;                       -- another session made the same change while we waited
END
$fn$;
COMMENT ON FUNCTION platform.set_unit_reachable(text, boolean, text) IS 'SECURITY DEFINER (owner platform_owner): the only writer of unit_costs.reachable. Returns true when the flag changed (a history row is written by trigger), false when it already held. Granted to platform_app.';
REVOKE ALL ON FUNCTION platform.set_unit_reachable(text, boolean, text) FROM PUBLIC;

-- a snapshot holds only reachable units
CREATE FUNCTION platform.job_unit_prices_only_reachable() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_reachable boolean;
BEGIN
    SELECT u.reachable INTO v_reachable FROM platform.unit_costs u WHERE u.unit = NEW.unit;
    IF FOUND AND v_reachable IS NOT TRUE THEN
        RAISE EXCEPTION 'platform.job_unit_prices: unit % is not reachable under the module''s current configuration and is not part of a job''s price snapshot', NEW.unit
            USING ERRCODE = '23514', CONSTRAINT = 'snapshot_only_reachable_units';
    END IF;
    RETURN NEW;
END
$fn$;
REVOKE ALL ON FUNCTION platform.job_unit_prices_only_reachable() FROM PUBLIC;
CREATE TRIGGER job_unit_prices_only_reachable BEFORE INSERT ON platform.job_unit_prices FOR EACH ROW EXECUTE FUNCTION platform.job_unit_prices_only_reachable();

-- the complete snapshot is the REACHABLE units of the module (it replaces ledger_check_reserve of platform_0005)
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
    SELECT count(*) INTO v_units FROM platform.unit_costs WHERE module_id = NEW.module_id AND reachable;
    IF v_snapshot = 0 OR v_snapshot <> v_units THEN
        RAISE EXCEPTION 'a reserve needs the job''s complete price snapshot: job % has % price row(s), module % has % reachable unit(s)', NEW.job_id, v_snapshot, NEW.module_id, v_units
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_needs_the_complete_snapshot';
    END IF;
    IF NEW.unit_cost IS DISTINCT FROM v_dearest THEN
        RAISE EXCEPTION 'a reserve is held at the DEAREST frozen price of the job (%), not at %', v_dearest, NEW.unit_cost
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_at_the_dearest_price';
    END IF;
    RETURN NEW;
END
$fn$;

GRANT SELECT ON platform.unit_reachability_history TO platform_app, platform_admin;
GRANT EXECUTE ON FUNCTION platform.set_unit_reachable(text, boolean, text) TO platform_app;

INSERT INTO platform.schema_migrations (version) VALUES ('0013');
