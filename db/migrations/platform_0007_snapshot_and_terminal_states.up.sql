-- platform_0007_snapshot_and_terminal_states.up.sql — two rules Phase 2 left to the Go store, now held by the DATABASE so that every writer (the admin surface, a script, a later phase) is held to them. Phase 2 close-out, items D3 and D4.
--
--   1. D3 / open decision 10  SNAPSHOT PRICES ARE ENFORCED BY THE DATABASE. A row of platform.job_unit_prices must carry the cost that platform.unit_costs holds for the same unit AT THE INSTANT OF THE INSERT
--      (BEFORE INSERT trigger; SQLSTATE 23514, constraint name snapshot_equals_unit_costs). A legal snapshot is a copy of the table (the store's one INSERT ... SELECT ... FROM platform.unit_costs, after FOR SHARE on
--      those rows), so it always passes; a forged price (the vendor price as 1, or any number the table does not hold) is refused. A unit the table does not know is left to the existing foreign key
--      (job_unit_prices_unit_fk, 23503); a NULL cost is refused here (IS DISTINCT FROM), not left to NOT NULL.
--   2. D4 / open decision 11  TERMINAL JOB STATES ARE FINAL. Once a job's status is succeeded, failed or cancelled no UPDATE may change it (BEFORE UPDATE trigger; SQLSTATE 23001 restrict_violation, constraint
--      name jobs_terminal_is_final). Release and the settle checks assume a terminal job stays terminal; until now jobs_guard let `succeeded` go back to `draft`. An UPDATE that leaves the status as it is stays legal
--      (module_job_ref, scheduled_start, updated_at ... whatever jobs_guard already permits), so a terminal job can still be annotated.
--
-- WHAT THIS DOES NOT DO (no lifecycle is invented here). Only terminal -> any OTHER status is refused. A job in draft, scheduled, running, paused or cancelling still moves to any status the schema accepts
-- today: the vocabulary CHECK (jobs_status_vocabulary), a scheduled job needs a start (jobs_scheduled_has_a_start) and the one-active-job slot (jobs_one_active_per_user_module). Which of those non-terminal
-- moves are legal (draft -> scheduled -> running -> paused -> running -> cancelling -> cancelled, ...) is the job engine's rule, Phase 4's; tests/harness/closeout_schema_test.go records what the schema
-- allows today so that tightening it later is a deliberate change of a test.
--
-- WHAT THE PRICE CHECK DOES NOT DO. It compares at the instant of the INSERT and takes no lock: "the snapshot equals the table until the transaction ends" stays the store's FOR SHARE on the module's unit_costs rows
-- (TestMoneyStore_SnapshotEqualsTheTableAtThatInstant, with its paired control that proves the lock is what holds a price change back). A role that holds UPDATE(cost, changed_by, change_reason) on unit_costs can
-- still change the table, snapshot and change it back inside one transaction: that leaves two rows in platform.unit_cost_history naming the admin, and is the audited admin path, not a forgery.
-- A price change that commits between the copy and the check (a writer that did not lock) makes the check see the new price and refuse the copy: refusing is the safe side; the store's lock makes it impossible.
--
-- Both functions are SECURITY INVOKER with a pinned search_path: the price check reads platform.unit_costs, which platform_app (the only role that inserts a snapshot) may SELECT, and the job check reads only the
-- row it is given. No function is granted to anybody (a trigger function's EXECUTE is not checked when the trigger fires). Created AS platform_owner (see platform_0002).
-- NULL TRAP (CLAUDE.md rule 13): both comparisons are IS DISTINCT FROM; status and cost are NOT NULL columns, and a NULL on either side is refused, never waved through.

SET LOCAL ROLE platform_owner;

-- ── 1. a snapshot price is the table's price ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_cost integer;
BEGIN
    SELECT u.cost INTO v_cost FROM platform.unit_costs u WHERE u.unit = NEW.unit;
    -- FOUND is false for a unit the table does not hold (and for a NULL unit): the foreign key job_unit_prices_unit_fk (23503) or NOT NULL (23502) answers, as it always did
    IF FOUND AND NEW.cost IS DISTINCT FROM v_cost THEN
        RAISE EXCEPTION 'platform.job_unit_prices: job % freezes unit % at %, but platform.unit_costs holds % for it: a price snapshot is a copy of the table at the instant of the insert, never another number', NEW.job_id, NEW.unit, NEW.cost, v_cost
            USING ERRCODE = '23514', CONSTRAINT = 'snapshot_equals_unit_costs';
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs() IS 'SECURITY INVOKER: raises 23514 (constraint snapshot_equals_unit_costs) when a price snapshot row carries a cost other than the one platform.unit_costs holds for the unit at the instant of the insert. A unit the table does not hold is left to the foreign key.';
REVOKE ALL ON FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs() FROM PUBLIC;
CREATE TRIGGER job_unit_prices_snapshot_equals_unit_costs BEFORE INSERT ON platform.job_unit_prices FOR EACH ROW EXECUTE FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs();
COMMENT ON TRIGGER job_unit_prices_snapshot_equals_unit_costs ON platform.job_unit_prices IS 'D3 (open decision 10): the database, not only the store, refuses a snapshot price that differs from platform.unit_costs. The only barrier: disabling this trigger lets a forged price in (proved by a test).';

-- ── 2. a terminal job stays terminal ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF OLD.status IN ('succeeded', 'failed', 'cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'platform.jobs: job % is % and a terminal status is final: it cannot become %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = '23001', CONSTRAINT = 'jobs_terminal_is_final';   -- restrict_violation
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_terminal_is_final() IS 'SECURITY INVOKER: raises 23001 (constraint jobs_terminal_is_final) when an UPDATE changes the status of a job that is succeeded, failed or cancelled. An UPDATE that leaves the status as it is passes.';
REVOKE ALL ON FUNCTION platform.jobs_terminal_is_final() FROM PUBLIC;
CREATE TRIGGER jobs_terminal_is_final BEFORE UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_terminal_is_final();
COMMENT ON TRIGGER jobs_terminal_is_final ON platform.jobs IS 'D4 (open decision 11): a succeeded, failed or cancelled job never changes status again. It fires after jobs_guard (alphabetical), which keeps owning the immutable columns and frozen_settings. The only barrier: disabling this trigger lets a terminal job move (proved by a test).';

INSERT INTO platform.schema_migrations (version) VALUES ('0007');
