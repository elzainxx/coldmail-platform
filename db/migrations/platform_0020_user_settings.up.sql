-- platform_0020_user_settings.up.sql — Phase 5, chunk S6: the per-user vendor opt-out (Docs/phase-5-spec.md, "Decisions taken" 8; module 1's ADR-0024).
--
-- THE RULING. A customer is opted IN to module 1's paid vendor fallback by default and may opt out in their settings. The setting is read ONCE, when a module 1 job is created, and kept in the job's frozen
-- settings (platform.jobs.frozen_settings, settings.vendor_opt_out): a later change never alters a job that exists. An opted-out job is created at module 1 under the policy that has no `external` block (so it
-- never reaches the vendor) and its reserve is the NON-VENDOR price. This migration is the database half of that:
--
--   1. platform.user_settings         one row per user who ever stated a setting: vendor_fallback_opt_out boolean NOT NULL DEFAULT false (no row = opted in). platform_app inserts and updates exactly the value and its
--                                     time; both roles read; nobody deletes (a trigger refuses the owner too: a deleted row would silently opt the user back in). RLS is not used in this repository: every query is scoped by
--                                     the user id of the authenticated principal (internal/usersettings). The module roles hold nothing on schema platform at all.
--   2. platform.user_settings_history append-only; one row per CHANGE of a setting's value, written by trigger (so no writer can skip it), with the user as the actor and the old and new value, and nothing else: no
--                                     address, no token, no text of the user's. A write that changes nothing (the same value again, or a first row that states the default) writes no history row. platform.admin_audit is the
--                                     audit of an ADMIN's actions (admin_id NOT NULL, a named admin): a user's own change has no admin to name, so it has a history of its own, the shape of platform.unit_reachability_history.
--   3. platform.unit_costs.needs_vendor  a unit that is charged only for an answer the paid vendor gave (m1_vendor). It is data, not a name in code: the reserve of a job that is opted out of the vendor takes the
--                                     module's reachable units WITHOUT these.
--   4. jobs_vendor_opt_out_is_boolean, jobs_vendor_opt_out_only_on_module_one   the frozen key, when present, is a JSON boolean and only a module 1 job carries it (a send job has no vendor: the setting is never read for it).
--   5. platform.ledger_check_reserve (replaced; its text of platform_0013 plus the opt-out): for a job that is opted out the complete snapshot is the module's reachable units that do not need the vendor, and a
--      snapshot that holds a vendor unit is refused (reserve_no_vendor_unit_for_an_opted_out_job). Together with the ledger's composite foreign key to the snapshot this makes the vendor unit UNCHARGEABLE for an
--      opted-out job: a result that still maps to m1_vendor cannot be priced and takes the existing unpriced path (internal/money settle: never charged, flagged, free), whatever any Go code believes.
--
-- A job created before this migration has no key and is opted in; nothing is rewritten. The vendor unit's price and reachability are not touched.
-- NULL TRAP (CLAUDE.md rule 13): every CHECK below is over NOT NULL columns, or says IS NULL / IS NOT NULL for the one nullable expression (the frozen key).
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── the setting ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.user_settings (
    user_id                 uuid        NOT NULL,
    vendor_fallback_opt_out boolean     NOT NULL DEFAULT false,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_settings_pk PRIMARY KEY (user_id),
    CONSTRAINT user_settings_user_fk FOREIGN KEY (user_id) REFERENCES public.users (id) ON DELETE RESTRICT,
    CONSTRAINT user_settings_not_the_system_user CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid)    -- module 2's shared-pool owner holds no login and no choice
);
COMMENT ON TABLE platform.user_settings IS 'Phase 5 S6. One row per user who stated a setting; no row means the defaults (vendor_fallback_opt_out false: opted IN). Written only through internal/usersettings, scoped by the authenticated user. Never deleted. Every change of a value is in platform.user_settings_history.';
COMMENT ON COLUMN platform.user_settings.vendor_fallback_opt_out IS 'true: the user''s module 1 jobs are created under module 1''s policy without the external (paid vendor) block and reserve at the non-vendor price. Read once into a job when it is created (platform.jobs.frozen_settings, settings.vendor_opt_out).';

CREATE TABLE platform.user_settings_history (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       uuid        NOT NULL,
    setting       text        NOT NULL,
    old_value     boolean     NOT NULL,
    new_value     boolean     NOT NULL,
    actor_user_id uuid        NOT NULL,
    changed_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_settings_history_user_fk FOREIGN KEY (user_id) REFERENCES public.users (id) ON DELETE RESTRICT,
    CONSTRAINT user_settings_history_setting_vocabulary CHECK (setting IN ('vendor_fallback_opt_out')),
    CONSTRAINT user_settings_history_a_change CHECK (old_value <> new_value),
    CONSTRAINT user_settings_history_the_user_is_the_actor CHECK (actor_user_id = user_id)
);
CREATE INDEX user_settings_history_user_idx ON platform.user_settings_history (user_id, id DESC);
COMMENT ON TABLE platform.user_settings_history IS 'Append-only. One row per change of a user setting''s value, written by the trigger of platform.user_settings, with the user as the actor and the old and new value (the default counts as the old value of a first row). Nothing else is recorded.';

CREATE FUNCTION platform.user_settings_record_history() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_old boolean := false;     -- the value before: the default when there was no row
BEGIN
    IF TG_OP = 'UPDATE' THEN
        v_old := OLD.vendor_fallback_opt_out;
    END IF;
    IF NEW.vendor_fallback_opt_out IS DISTINCT FROM v_old THEN
        INSERT INTO platform.user_settings_history (user_id, setting, old_value, new_value, actor_user_id)
        VALUES (NEW.user_id, 'vendor_fallback_opt_out', v_old, NEW.vendor_fallback_opt_out, NEW.user_id);
    END IF;
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.user_settings_record_history() IS 'SECURITY DEFINER (owner platform_owner): writes the history row of a changed setting, so platform_app needs no INSERT on the history table. The actor is the setting''s own user: no other writer of this table exists.';
REVOKE ALL ON FUNCTION platform.user_settings_record_history() FROM PUBLIC;
CREATE TRIGGER user_settings_record_history AFTER INSERT OR UPDATE ON platform.user_settings FOR EACH ROW EXECUTE FUNCTION platform.user_settings_record_history();

CREATE TRIGGER user_settings_history_append_only BEFORE UPDATE OR DELETE ON platform.user_settings_history FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER user_settings_history_no_truncate BEFORE TRUNCATE ON platform.user_settings_history FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();
-- a setting row is never deleted or truncated (platform.reject_mutation's text says "append-only": for this table read it as "never deleted: the value is changed by an UPDATE, and the history says when")
CREATE TRIGGER user_settings_no_delete BEFORE DELETE ON platform.user_settings FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER user_settings_no_truncate BEFORE TRUNCATE ON platform.user_settings FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- privileges: the application states a user's setting (insert, and update of the value and its time); both roles read the setting and its history, and no role writes the history (the trigger does). Nobody else
-- holds anything (the module roles hold nothing in schema platform).
GRANT SELECT, INSERT ON platform.user_settings TO platform_app;
GRANT UPDATE (vendor_fallback_opt_out, updated_at) ON platform.user_settings TO platform_app;
GRANT SELECT ON platform.user_settings TO platform_admin;
GRANT SELECT ON platform.user_settings_history TO platform_app, platform_admin;

-- ── the unit the vendor answers ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.unit_costs ADD COLUMN needs_vendor boolean NOT NULL DEFAULT false;
UPDATE platform.unit_costs SET needs_vendor = true WHERE unit = 'm1_vendor';
COMMENT ON COLUMN platform.unit_costs.needs_vendor IS 'Phase 5 S6: true for a unit that is charged only for an answer the paid vendor gave (m1_vendor). A job opted out of the vendor never reserves or freezes a unit that needs it. Data of this migration; no application role may write it.';

-- ── the frozen key ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_vendor_opt_out_is_boolean
    CHECK ((frozen_settings #> '{settings,vendor_opt_out}') IS NULL OR jsonb_typeof(frozen_settings #> '{settings,vendor_opt_out}') = 'boolean');
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_vendor_opt_out_only_on_module_one
    CHECK ((frozen_settings #> '{settings,vendor_opt_out}') IS NULL OR module_id = 'm1');

-- ── the reserve: complete over the units the job can produce, and never a vendor unit for an opted-out job (replaces ledger_check_reserve of platform_0013) ────────────────────────────────────
CREATE OR REPLACE FUNCTION platform.ledger_check_reserve() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_snapshot   integer;
    v_units      integer;
    v_dearest    integer;
    v_opted_out  boolean := false;
BEGIN
    SELECT count(*), COALESCE(max(cost), 0) INTO v_snapshot, v_dearest FROM platform.job_unit_prices WHERE job_id = NEW.job_id;
    -- the job's own frozen choice (a job with no key, or a row that cannot be read, is opted in: the stricter wall is the one that asks for the vendor's unit too)
    SELECT COALESCE((j.frozen_settings #>> '{settings,vendor_opt_out}')::boolean, false) INTO v_opted_out FROM platform.jobs j WHERE j.id = NEW.job_id;
    v_opted_out := COALESCE(v_opted_out, false);
    SELECT count(*) INTO v_units FROM platform.unit_costs WHERE module_id = NEW.module_id AND reachable AND NOT (v_opted_out AND needs_vendor);
    IF v_snapshot = 0 OR v_snapshot <> v_units THEN
        RAISE EXCEPTION 'a reserve needs the job''s complete price snapshot: job % has % price row(s), module % has % unit(s) this job can be charged', NEW.job_id, v_snapshot, NEW.module_id, v_units
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_needs_the_complete_snapshot';
    END IF;
    IF v_opted_out AND EXISTS (SELECT 1 FROM platform.job_unit_prices p JOIN platform.unit_costs u ON u.unit = p.unit WHERE p.job_id = NEW.job_id AND u.needs_vendor) THEN
        RAISE EXCEPTION 'job % is opted out of the vendor and its price snapshot holds a unit that needs the vendor', NEW.job_id
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_no_vendor_unit_for_an_opted_out_job';
    END IF;
    IF NEW.unit_cost IS DISTINCT FROM v_dearest THEN
        RAISE EXCEPTION 'a reserve is held at the DEAREST frozen price of the job (%), not at %', v_dearest, NEW.unit_cost
            USING ERRCODE = '23514', CONSTRAINT = 'reserve_at_the_dearest_price';
    END IF;
    RETURN NEW;
END
$fn$;

INSERT INTO platform.schema_migrations (version) VALUES ('0020');
