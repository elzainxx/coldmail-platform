-- platform_0004_phase1_rulings.up.sql — the owner's rulings on the Phase 1 open decisions, as one forward migration. Phase 2, Part A (chunks A1 and A2).
--
--   1. OD-2   A SCHEDULED job holds the "one active job per module per user" slot: the partial unique index now covers scheduled, running, paused and cancelling.
--   2. OD-4   Ledger kind `adjustment` (a clawback): admin only, reason required, removes only AVAILABLE credits (never reserved), never below zero (the balance CHECK), and writes an
--             admin_audit row by trigger so no writer can skip it. It is a monetary kind: the balance trigger and the balance_drift gate cover it.
--   3. OD-10  The DATABASE refuses a disabled admin in the ledger, the audit log, M3 entitlements and unit costs (platform.require_active_admin).
--   4. OD-12  The all-zero system user (module 2's shared-pool owner, no login) can hold no balance, ledger row, entitlement or suppression, and platform.listable_users is the only
--             sanctioned way to list users (scripts/check-user-lists.sh fails when platform code selects public.users anywhere else).
--
-- Amends Docs/phase-1-spec.md "Locked here" item 12 (a disabled admin can still be named: no longer) and item 7 (the kinds); recorded in Docs/phase-2-spec.md.
-- NULL TRAP (CLAUDE.md rule 13): every new CHECK is over NOT NULL columns or says IS NOT NULL. Created AS platform_owner (see platform_0002).
--
-- ledger.monetary is a GENERATED column: its expression cannot be altered in place before PostgreSQL 17 (this must also run on 16), so the column is dropped and added again. Dropping it
-- drops the foreign key that uses it and requires dropping balance_drift first; both are re-created below. Adding a stored generated column rewrites the table, which is DDL and fires no
-- row trigger, so the append-only triggers are not involved and no row changes value.

SET LOCAL ROLE platform_owner;

-- ── 1. a scheduled job holds the slot ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
DROP INDEX platform.jobs_one_executing_per_user_module;
CREATE UNIQUE INDEX jobs_one_active_per_user_module ON platform.jobs (user_id, module_id) WHERE status IN ('scheduled', 'running', 'paused', 'cancelling');
COMMENT ON INDEX platform.jobs_one_active_per_user_module IS 'One active job per module per user: a job that is scheduled (credits reserved, not started), running, paused or cancelling holds the slot. Another module is a different slot; a draft or a finished job holds none.';

-- ── 2. the system user holds nothing ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
-- All four columns are NOT NULL, so each CHECK is NULL-safe. The system user is module 2's owner of shared pools: it has no login and must never own credits, money events, an entitlement or a list.
ALTER TABLE platform.balances        ADD CONSTRAINT balances_not_the_system_user        CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid);
ALTER TABLE platform.ledger          ADD CONSTRAINT ledger_not_the_system_user          CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid);
ALTER TABLE platform.m3_entitlements ADD CONSTRAINT m3_entitlements_not_the_system_user CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid);
ALTER TABLE platform.suppressions    ADD CONSTRAINT suppressions_not_the_system_user    CHECK (user_id <> '00000000-0000-0000-0000-000000000000'::uuid);

-- The only sanctioned way to list users: the real people (a Supabase login behind every row). Anything that lists users for a screen, a count or a job must read this view, never public.users.
CREATE VIEW platform.listable_users AS
SELECT u.id, u.auth_user_id, u.email, u.is_active, u.created_at
FROM public.users u
WHERE u.id <> '00000000-0000-0000-0000-000000000000'::uuid AND u.auth_user_id IS NOT NULL;
COMMENT ON VIEW platform.listable_users IS 'Every user the platform may show or count: public.users without the all-zero system user (module 2''s shared-pool owner, no login). The only sanctioned way to list users; scripts/check-user-lists.sh fails on platform code that selects public.users elsewhere.';

-- ── 3. the database refuses a disabled admin ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
-- FOR SHARE: a concurrent disabling of the admin (UPDATE platform.admins) waits for this transaction, so "active when written" cannot be raced. SECURITY DEFINER: the application roles may
-- read platform.admins but a row lock needs UPDATE, which only the owner holds.
CREATE FUNCTION platform.require_active_admin(p_admin uuid, p_where text) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_disabled timestamptz;
BEGIN
    IF p_admin IS NULL THEN
        RETURN;                                   -- not an admin action (the automatic expiry, a user's own action)
    END IF;
    SELECT disabled_at INTO v_disabled FROM platform.admins WHERE id = p_admin FOR SHARE;
    -- not found: the foreign key on the column refuses an unknown admin (23503), as it always did; found and disabled: refused here
    IF FOUND AND v_disabled IS NOT NULL THEN
        RAISE EXCEPTION '% names admin %, who was disabled at %: a disabled admin can no longer act', p_where, p_admin, v_disabled
            USING ERRCODE = '23514', CONSTRAINT = 'admin_is_active';
    END IF;
END
$fn$;
COMMENT ON FUNCTION platform.require_active_admin(uuid, text) IS 'Raises 23514 (constraint admin_is_active) when the admin exists and has been disabled; NULL passes (not an admin action); an unknown admin is left to the foreign key (23503). Called by the triggers on every table that names an admin.';
REVOKE ALL ON FUNCTION platform.require_active_admin(uuid, text) FROM PUBLIC;

-- The four functions that call require_active_admin (these three and unit_costs_guard below) are SECURITY DEFINER: a function called FROM a trigger function is checked against the EXECUTE privilege of the
-- role whose statement fired the trigger, and no role holds EXECUTE on require_active_admin (it is revoked from PUBLIC and granted to nobody). Without it every application write would fail 42501.
CREATE FUNCTION platform.ledger_require_active_admin() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    PERFORM platform.require_active_admin(NEW.acting_admin_id, 'platform.ledger.acting_admin_id');
    RETURN NEW;
END
$fn$;
CREATE TRIGGER ledger_require_active_admin BEFORE INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_require_active_admin();
REVOKE ALL ON FUNCTION platform.ledger_require_active_admin() FROM PUBLIC;

CREATE FUNCTION platform.admin_audit_require_active_admin() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    PERFORM platform.require_active_admin(NEW.admin_id, 'platform.admin_audit.admin_id');
    RETURN NEW;
END
$fn$;
CREATE TRIGGER admin_audit_require_active_admin BEFORE INSERT ON platform.admin_audit FOR EACH ROW EXECUTE FUNCTION platform.admin_audit_require_active_admin();
REVOKE ALL ON FUNCTION platform.admin_audit_require_active_admin() FROM PUBLIC;

CREATE FUNCTION platform.m3_entitlements_require_active_admin() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    PERFORM platform.require_active_admin(NEW.changed_by, 'platform.m3_entitlements.changed_by');
    RETURN NEW;
END
$fn$;
CREATE TRIGGER m3_entitlements_require_active_admin BEFORE INSERT OR UPDATE ON platform.m3_entitlements FOR EACH ROW EXECUTE FUNCTION platform.m3_entitlements_require_active_admin();
REVOKE ALL ON FUNCTION platform.m3_entitlements_require_active_admin() FROM PUBLIC;

-- unit_costs: the existing guard already demands a named admin and a reason for a price change; it now also demands that the admin is active. (CREATE OR REPLACE keeps the trigger.)
CREATE OR REPLACE FUNCTION platform.unit_costs_guard() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.cost IS DISTINCT FROM OLD.cost THEN
        IF NEW.changed_by IS NULL OR NEW.change_reason IS NULL OR btrim(NEW.change_reason) = '' THEN
            RAISE EXCEPTION 'changing platform.unit_costs.cost for % needs changed_by (a named admin) and a non-blank change_reason', OLD.unit
                USING ERRCODE = '23514';
        END IF;
    END IF;
    IF NEW.cost IS DISTINCT FROM OLD.cost OR NEW.changed_by IS DISTINCT FROM OLD.changed_by THEN
        PERFORM platform.require_active_admin(NEW.changed_by, 'platform.unit_costs.changed_by');
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;

-- ── 4. ledger kind `adjustment` (a clawback) ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
DROP VIEW platform.balance_drift;                                           -- uses ledger.monetary
ALTER TABLE platform.ledger DROP CONSTRAINT ledger_module_metering_fk;       -- uses ledger.monetary (dropping the column would drop it silently)
ALTER TABLE platform.ledger DROP COLUMN monetary;
ALTER TABLE platform.ledger ADD COLUMN monetary boolean GENERATED ALWAYS AS (kind IN ('grant', 'reserve', 'charge', 'release', 'adjustment')) STORED;
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_module_metering_fk FOREIGN KEY (module_id, monetary) REFERENCES platform.modules (id, metered);

ALTER TABLE platform.ledger DROP CONSTRAINT ledger_kind_vocabulary;
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_kind_vocabulary CHECK (kind IN ('grant', 'reserve', 'charge', 'release', 'adjustment', 'm3_enable', 'm3_disable', 'm3_expire'));
-- an adjustment: an admin removes AVAILABLE credits, with a reason, tied to no job. Never reserved credits; never below zero (platform.balances' CHECK refuses the overdraw).
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_shape_adjustment CHECK (kind <> 'adjustment' OR (
    available_delta < 0 AND reserved_delta = 0 AND job_id IS NULL AND reserve_id IS NULL AND unit IS NULL AND units IS NULL AND unit_cost IS NULL
    AND acting_admin_id IS NOT NULL AND reason IS NOT NULL AND btrim(reason) <> ''));

COMMENT ON TABLE platform.ledger IS 'THE money record. Append-only: UPDATE, DELETE and TRUNCATE are refused for every role (privileges) and for the owner too (triggers). Each row carries signed integer deltas; balances is their sum, maintained by trigger. Kinds: grant, reserve, charge, release, adjustment (monetary, M1/M2 only) and m3_enable, m3_disable, m3_expire (no credits, M3 only).';

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

-- An adjustment is the one money event that REMOVES credits a user holds, so it leaves a second, independent record: an admin_audit row naming the same admin, written by trigger so no writer
-- can skip it. SECURITY DEFINER because platform_app and platform_admin may INSERT into admin_audit but the function must not depend on that.
CREATE FUNCTION platform.ledger_audit_adjustment() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, target_user_id, reason, details)
    VALUES (NEW.acting_admin_id, 'action', 'ledger.adjustment', 'ledger', NEW.id::text, NEW.user_id, NEW.reason,
            jsonb_build_object('module_id', NEW.module_id, 'credits_removed', -NEW.available_delta, 'idempotency_key', NEW.idempotency_key));
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_audit_adjustment() IS 'SECURITY DEFINER (owner platform_owner): writes the admin_audit row of every adjustment, in the same transaction as the ledger row.';
CREATE TRIGGER ledger_audit_adjustment AFTER INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind = 'adjustment') EXECUTE FUNCTION platform.ledger_audit_adjustment();
REVOKE ALL ON FUNCTION platform.ledger_audit_adjustment() FROM PUBLIC;

-- ── privileges: the re-created view and the new view, nothing else ───────────────────────────────────────────────────────────────────────────────────────────────
GRANT SELECT ON platform.balance_drift, platform.listable_users TO platform_app, platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0004');
