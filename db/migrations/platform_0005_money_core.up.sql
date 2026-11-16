-- platform_0005_money_core.up.sql — what the money core (Phase 2, Part B) needs the DATABASE to guarantee, so that every writer, not only the Go store, is held to it.
--
--   1. SETTLE IS IDEMPOTENT PER JOB ITEM, BY A DATABASE KEY. A charge row is exactly ONE unit of ONE job item: it carries `item_ref` (the module's own item id) and
--      UNIQUE (job_id, item_ref) WHERE kind = 'charge'. The idempotency key the code derives is a second line; this index is the one that holds if a code path forgets to derive it.
--      Billing is per job item, never per delivery.
--   2. ONE RESERVE PER JOB. UNIQUE (job_id) WHERE kind = 'reserve'.
--   3. A RESERVE IS AT THE DEAREST FROZEN PRICE, AGAINST A COMPLETE SNAPSHOT. The reserve row's unit_cost must be the dearest price in the job's price snapshot and the snapshot must hold a price
--      for EVERY unit of the job's module (it is copied from platform.unit_costs in the same transaction as the reserve). Otherwise a settlement at a dearer unit (module 1's vendor) could not be
--      paid for by what was held.
--   4. A CHARGE CONSUMES THE RESERVE IT NAMES, AND ONLY THAT. platform.reserves holds, per reserve, what was reserved, charged and released; it is written ONLY by a trigger on the ledger and
--      CHECK (charged + released <= reserved) makes it impossible to spend more than a reserve held. The pooled balance cannot hide one job spending another job's reservation.
--   5. A RELEASE RETURNS EXACTLY THE REMAINDER, ONCE. UNIQUE (reserve_id) WHERE kind = 'release' and a trigger that refuses any amount but reserved - charged - released.
--
-- Amends Docs/phase-1-spec.md "Locked here" item 7 (a charge is one unit of one job item, with an item_ref); recorded in Docs/phase-2-spec.md. NULL TRAP (CLAUDE.md rule 13): every new CHECK states
-- IS NULL / IS NOT NULL or COALESCEs. Created AS platform_owner (see platform_0002).
--
-- Adding a column to the append-only ledger and adding CHECKs and unique indexes is DDL: it fires no row trigger and changes no existing value. A ledger that already holds a multi-unit charge (units > 1) or
-- a charge without an item_ref cannot take these constraints, and the migration then fails loudly: Phase 1 and Phase 2 have written no real charge.

SET LOCAL ROLE platform_owner;

-- ── 0. the ledger's clock is the DATABASE's ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
-- created_at defaults to now(), but a column default is only a default: any role with INSERT could write another value, and two rules lean on it (the m3_expire "is due" check and the until_at > created_at
-- check). This trigger overwrites whatever the writer supplied with the transaction's timestamp, so "time is the database's" is a fact for every row of the ledger and not a habit of the store.
CREATE FUNCTION platform.ledger_stamp_created_at() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    NEW.created_at := now();
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_stamp_created_at() IS 'Forces ledger.created_at to the database''s transaction timestamp, whatever the writer supplied.';
CREATE TRIGGER ledger_stamp_created_at BEFORE INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_stamp_created_at();
REVOKE ALL ON FUNCTION platform.ledger_stamp_created_at() FROM PUBLIC;

-- ── 1. one unit, one job item, one charge ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.ledger ADD COLUMN item_ref text;
COMMENT ON COLUMN platform.ledger.item_ref IS 'The module''s own id of the job item a charge settles (module 2: the job_item_id; module 1: <job>:<ordinal> until module 1 returns an item id). Charge rows only; unique per job: an item is billed once.';
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_item_ref_shape CHECK (
    (kind = 'charge' AND item_ref IS NOT NULL AND btrim(item_ref) <> '' AND octet_length(item_ref) <= 200)
    OR (kind <> 'charge' AND item_ref IS NULL));
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_charge_is_one_item CHECK (kind <> 'charge' OR COALESCE(units = 1, false));
CREATE UNIQUE INDEX ledger_one_charge_per_item ON platform.ledger (job_id, item_ref) WHERE kind = 'charge';
COMMENT ON INDEX platform.ledger_one_charge_per_item IS 'Settle is idempotent per job item: a second charge for the same (job, item) is refused here whatever idempotency key the writer derived.';

-- ── 2. one reserve per job; one release per reserve ────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX ledger_one_reserve_per_job ON platform.ledger (job_id) WHERE kind = 'reserve';
CREATE UNIQUE INDEX ledger_one_release_per_reserve ON platform.ledger (reserve_id) WHERE kind = 'release';
COMMENT ON INDEX platform.ledger_one_reserve_per_job IS 'A job holds credits once: reserve at confirm, exactly one reserve row.';
COMMENT ON INDEX platform.ledger_one_release_per_reserve IS 'The unused remainder of a reserve is returned once.';

-- ── 3. a reserve is at the dearest frozen price, against a complete snapshot ───────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.ledger_check_reserve() RETURNS trigger
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
COMMENT ON FUNCTION platform.ledger_check_reserve() IS 'SECURITY DEFINER (owner platform_owner): reads the price snapshot and unit costs, which a role may not row-lock. Refuses a reserve that is not at the dearest frozen price of a complete snapshot.';
CREATE TRIGGER ledger_check_reserve BEFORE INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind = 'reserve') EXECUTE FUNCTION platform.ledger_check_reserve();
REVOKE ALL ON FUNCTION platform.ledger_check_reserve() FROM PUBLIC;

-- ── 4. what each reserve held, charged and released: written only by the ledger trigger ────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.reserves (
    reserve_id bigint      PRIMARY KEY REFERENCES platform.ledger (id) ON DELETE RESTRICT,
    user_id    uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    module_id  text        NOT NULL REFERENCES platform.modules (id),
    job_id     uuid        NOT NULL UNIQUE REFERENCES platform.jobs (id) ON DELETE RESTRICT,
    reserved   bigint      NOT NULL,
    charged    bigint      NOT NULL DEFAULT 0,
    released   bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT reserves_reserved_positive CHECK (reserved >= 1),
    CONSTRAINT reserves_charged_not_negative CHECK (charged >= 0),
    CONSTRAINT reserves_released_not_negative CHECK (released >= 0),
    CONSTRAINT reserves_not_overspent CHECK (charged + released <= reserved)
);
COMMENT ON TABLE platform.reserves IS 'Per reserve: what was held (reserved), charged against it and released from it. NO ROLE WRITES THIS TABLE: the ledger trigger is its only writer, so charged + released <= reserved is a database fact (a job can never spend another job''s reservation).';

CREATE FUNCTION platform.ledger_apply_to_reserves() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.kind = 'reserve' THEN
        INSERT INTO platform.reserves (reserve_id, user_id, module_id, job_id, reserved) VALUES (NEW.id, NEW.user_id, NEW.module_id, NEW.job_id, NEW.reserved_delta);
    ELSIF NEW.kind = 'charge' THEN
        UPDATE platform.reserves SET charged = charged - NEW.reserved_delta, updated_at = now() WHERE reserve_id = NEW.reserve_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'reserve % has no platform.reserves row (a reserve written before platform_0005 has to be backfilled first)', NEW.reserve_id USING ERRCODE = '23514', CONSTRAINT = 'reserve_row_exists';
        END IF;
    ELSIF NEW.kind = 'release' THEN
        UPDATE platform.reserves SET released = released + NEW.available_delta, updated_at = now() WHERE reserve_id = NEW.reserve_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'reserve % has no platform.reserves row (a reserve written before platform_0005 has to be backfilled first)', NEW.reserve_id USING ERRCODE = '23514', CONSTRAINT = 'reserve_row_exists';
        END IF;
    END IF;
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_apply_to_reserves() IS 'SECURITY DEFINER (owner platform_owner): the only code path that writes platform.reserves.';
CREATE TRIGGER ledger_apply_to_reserves AFTER INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind IN ('reserve', 'charge', 'release')) EXECUTE FUNCTION platform.ledger_apply_to_reserves();
REVOKE ALL ON FUNCTION platform.ledger_apply_to_reserves() FROM PUBLIC;

-- ── 5. a release returns exactly the remainder ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.ledger_check_release() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_remainder bigint;
BEGIN
    SELECT reserved - charged - released INTO v_remainder FROM platform.reserves WHERE reserve_id = NEW.reserve_id;
    IF NOT FOUND OR NEW.available_delta IS DISTINCT FROM v_remainder THEN
        RAISE EXCEPTION 'a release returns exactly the unused remainder of its reserve (%), not %', v_remainder, NEW.available_delta
            USING ERRCODE = '23514', CONSTRAINT = 'release_is_the_remainder';
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_check_release() IS 'SECURITY DEFINER (owner platform_owner): refuses a release that is not exactly reserved - charged - released.';
CREATE TRIGGER ledger_check_release BEFORE INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind = 'release') EXECUTE FUNCTION platform.ledger_check_release();
REVOKE ALL ON FUNCTION platform.ledger_check_release() FROM PUBLIC;

-- ── privileges: read-only to both roles, like balances ─────────────────────────────────────────────────────────────────────────────────────────────────────────
GRANT SELECT ON platform.reserves TO platform_app, platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0005');
