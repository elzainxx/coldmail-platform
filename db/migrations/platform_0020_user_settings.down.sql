-- platform_0020_user_settings.down.sql — undo platform_0020: the settings table and its history, the unit's needs_vendor flag, the two CHECKs on the frozen key, and ledger_check_reserve goes back to platform_0013's text
-- (a complete snapshot is every REACHABLE unit of the module, whoever the job is).
-- REFUSES (55000) while a customer's choice would be lost or contradicted: a user who is opted OUT (dropping the table would silently opt them back in to the paid vendor), or a job that is opted out and has not
-- ended (its snapshot, by design, lacks the vendor unit, so the older complete-snapshot rule would refuse the reserve of one that has not reserved yet). The history of changes is dropped with the table.
-- (Rehearsal on an empty database or a deploy that failed before any customer opted out; the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true.)
SET LOCAL ROLE platform_owner;

DO $$
DECLARE
    n_users bigint;
    n_jobs  bigint;
BEGIN
    SELECT count(*) INTO n_users FROM platform.user_settings WHERE vendor_fallback_opt_out;
    IF n_users > 0 THEN
        RAISE EXCEPTION 'platform_0020 down: % user(s) are opted out of the vendor fallback; dropping the setting would opt them back in without their say', n_users USING ERRCODE = '55000';
    END IF;
    SELECT count(*) INTO n_jobs FROM platform.jobs
        WHERE (frozen_settings #>> '{settings,vendor_opt_out}') = 'true' AND status NOT IN ('succeeded', 'failed', 'cancelled', 'expired');
    IF n_jobs > 0 THEN
        RAISE EXCEPTION 'platform_0020 down: % job(s) opted out of the vendor fallback have not ended; their price snapshot lacks the vendor unit, which the older reserve rule would not accept', n_jobs USING ERRCODE = '55000';
    END IF;
END
$$;

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

ALTER TABLE platform.jobs DROP CONSTRAINT jobs_vendor_opt_out_only_on_module_one;
ALTER TABLE platform.jobs DROP CONSTRAINT jobs_vendor_opt_out_is_boolean;
ALTER TABLE platform.unit_costs DROP COLUMN needs_vendor;

DROP TRIGGER user_settings_record_history ON platform.user_settings;
DROP FUNCTION platform.user_settings_record_history();
DROP TABLE platform.user_settings_history;
DROP TABLE platform.user_settings;

DELETE FROM platform.schema_migrations WHERE version = '0020';
