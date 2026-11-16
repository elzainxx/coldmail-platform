-- platform_0006_m3_entitlement_flow.up.sql — module 3's entitlements, written like balances: by the ledger and nothing else, with delivery to module 3 tracked separately. Phase 2, Part B.
--
--   * platform.m3_entitlements is written ONLY by a trigger on the ledger kinds m3_enable / m3_disable / m3_expire (the same single-writer pattern as platform.balances): the stored entitlement
--     cannot change without the ledger row that explains it, and the application roles lose INSERT and UPDATE on it. An m3_enable row carries the optional `until_at` (the enabled-until date).
--   * m3_expire is the automatic end of an entitlement: the DATABASE refuses it unless the entitlement is enabled and its enabled_until has passed (an entitlement cannot be expired early or twice).
--   * Delivery to module 3 (suspend / unsuspend through its admin route) is bookkeeping in platform.m3_sync: the entitlement trigger raises want_version whenever the wanted state changes, and
--     converged_version may be set equal to want_version ONLY while `observed` (the state module 3 itself answered with) equals the wanted state: the platform cannot record "converged" for a state
--     that module 3 was not seen to be in.
--
-- NULL TRAP (CLAUDE.md rule 13): every new CHECK states IS NULL / IS NOT NULL or COALESCEs. Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── the enabled-until date travels on the ledger row that enables ──────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.ledger ADD COLUMN until_at timestamptz;
COMMENT ON COLUMN platform.ledger.until_at IS 'm3_enable only: the entitlement is enabled until this instant and then expires automatically (an m3_expire row). NULL = no end date.';
ALTER TABLE platform.ledger ADD CONSTRAINT ledger_until_at_shape CHECK (until_at IS NULL OR (kind = 'm3_enable' AND until_at > created_at));

-- ── the entitlement is written by the ledger ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.ledger_apply_to_entitlements() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.kind = 'm3_enable' THEN
        INSERT INTO platform.m3_entitlements (user_id, enabled, enabled_until, changed_by, changed_at)
        VALUES (NEW.user_id, true, NEW.until_at, NEW.acting_admin_id, NEW.created_at)
        ON CONFLICT (user_id) DO UPDATE
           SET enabled = true, enabled_until = EXCLUDED.enabled_until, changed_by = EXCLUDED.changed_by, changed_at = EXCLUDED.changed_at;
    ELSIF NEW.kind = 'm3_disable' THEN
        INSERT INTO platform.m3_entitlements (user_id, enabled, enabled_until, changed_by, changed_at)
        VALUES (NEW.user_id, false, NULL, NEW.acting_admin_id, NEW.created_at)
        ON CONFLICT (user_id) DO UPDATE
           SET enabled = false, enabled_until = NULL, changed_by = EXCLUDED.changed_by, changed_at = EXCLUDED.changed_at;
    ELSIF NEW.kind = 'm3_expire' THEN
        -- the automatic end: only an enabled entitlement whose date has passed (by the ledger row's own timestamp, the database's clock) may expire
        UPDATE platform.m3_entitlements
           SET enabled = false, enabled_until = NULL, changed_by = NULL, changed_at = NEW.created_at
         WHERE user_id = NEW.user_id AND enabled AND enabled_until IS NOT NULL AND enabled_until <= NEW.created_at;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'user % has no enabled entitlement that is due to expire: an entitlement is not expired early or twice', NEW.user_id
                USING ERRCODE = '23514', CONSTRAINT = 'm3_expire_is_due';
        END IF;
    END IF;
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_apply_to_entitlements() IS 'SECURITY DEFINER (owner platform_owner): the only code path that writes platform.m3_entitlements. m3_expire is refused unless the entitlement is enabled and due.';
CREATE TRIGGER ledger_apply_to_entitlements AFTER INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind IN ('m3_enable', 'm3_disable', 'm3_expire')) EXECUTE FUNCTION platform.ledger_apply_to_entitlements();
REVOKE ALL ON FUNCTION platform.ledger_apply_to_entitlements() FROM PUBLIC;

-- an entitlement that is enabled with a date has a future end; the existing CHECK (enabled_until IS NULL OR enabled) stays. No role writes the table any more.
REVOKE INSERT, UPDATE ON platform.m3_entitlements FROM platform_app;
COMMENT ON TABLE platform.m3_entitlements IS 'M3 is not metered: an admin enables it per user, optionally until a date, after which it expires automatically. NO ROLE WRITES THIS TABLE: a trigger on the ledger kinds m3_enable, m3_disable and m3_expire is its only writer, so every change has the ledger row that explains it. changed_by is NULL only for the automatic expiry.';

-- ── delivery to module 3: what the platform wants, and what module 3 was SEEN to answer ──────────────────────────────────────────────────────────────────────
CREATE TABLE platform.m3_sync (
    user_id           uuid        PRIMARY KEY REFERENCES platform.m3_entitlements (user_id) ON DELETE RESTRICT,
    want_suspended    boolean     NOT NULL,
    want_version      bigint      NOT NULL DEFAULT 1,
    converged_version bigint      NOT NULL DEFAULT 0,
    observed          text        NOT NULL DEFAULT 'unknown',
    observed_at       timestamptz,
    attempts          integer     NOT NULL DEFAULT 0,
    last_attempt_at   timestamptz,
    last_error        text,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT m3_sync_observed_vocabulary CHECK (observed IN ('unknown', 'suspended', 'active')),
    CONSTRAINT m3_sync_versions_are_positive CHECK (want_version >= 1 AND converged_version >= 0),
    CONSTRAINT m3_sync_converged_not_ahead CHECK (converged_version <= want_version),
    CONSTRAINT m3_sync_attempts_not_negative CHECK (attempts >= 0),
    -- THE RULE THAT STOPS A FALSE SUCCESS: "converged" (converged_version = want_version) may be recorded only while module 3 was observed in the wanted state
    CONSTRAINT m3_sync_converged_only_when_observed CHECK (
        converged_version < want_version
        OR (want_suspended AND observed = 'suspended')
        OR (NOT want_suspended AND observed = 'active'))
);
COMMENT ON TABLE platform.m3_sync IS 'Delivery of each entitlement to module 3. want_* is raised by a trigger when the entitlement changes; the reconcile loop records what module 3 answered (observed) and may set converged_version = want_version only while observed equals the wanted state (m3_sync_converged_only_when_observed): the platform never reports a state module 3 was not seen to be in.';
COMMENT ON COLUMN platform.m3_sync.want_suspended IS 'true when the entitlement is disabled or expired: module 3''s per-user suspend must be in force.';

CREATE FUNCTION platform.m3_entitlements_raise_sync() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES (NEW.user_id, NOT NEW.enabled);
    ELSIF NEW.enabled IS DISTINCT FROM OLD.enabled THEN
        -- an upsert, not a bare UPDATE: an entitlement that predates this migration has no m3_sync row, and a silent no-op would leave module 3 never told
        INSERT INTO platform.m3_sync (user_id, want_suspended, want_version) VALUES (NEW.user_id, NOT NEW.enabled, 2)
        ON CONFLICT (user_id) DO UPDATE SET want_suspended = EXCLUDED.want_suspended, want_version = platform.m3_sync.want_version + 1, attempts = 0, last_attempt_at = NULL, last_error = NULL, updated_at = now();   -- a NEW wanted state starts with a clean retry history
    END IF;
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.m3_entitlements_raise_sync() IS 'SECURITY DEFINER (owner platform_owner): keeps the wanted module-3 state of platform.m3_sync in step with the entitlement (a new want_version whenever enabled changes).';
CREATE TRIGGER m3_entitlements_raise_sync AFTER INSERT OR UPDATE ON platform.m3_entitlements FOR EACH ROW EXECUTE FUNCTION platform.m3_entitlements_raise_sync();
REVOKE ALL ON FUNCTION platform.m3_entitlements_raise_sync() FROM PUBLIC;

-- ── privileges: the reconcile loop records its observations (columns only); nobody inserts or deletes ─────────────────────────────────────────────────────────
GRANT SELECT ON platform.m3_sync TO platform_app, platform_admin;
GRANT UPDATE (observed, observed_at, attempts, last_attempt_at, last_error, converged_version, updated_at) ON platform.m3_sync TO platform_app;

INSERT INTO platform.schema_migrations (version) VALUES ('0006');
