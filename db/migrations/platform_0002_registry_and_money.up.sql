-- platform_0002_registry_and_money.up.sql — the module registry, admins, unit costs (with history), jobs (with their frozen price snapshot), per-module balances, the append-only ledger
-- and M3 entitlements. Phase 1, chunk B3. Every rule below is enforced by the DATABASE; the reason each exists is in Docs/phase-1-spec.md ("Locked here").
--
--   * Credits are integers: bigint amounts, integer unit costs >= 1 (the vendor price > 1). No numeric, real, double precision or money column anywhere in this schema.
--   * Every instant is timestamptz; ids are uuid (entities) or bigint identity (append-only logs).
--   * The BALANCE has exactly one writer: an AFTER INSERT trigger on the ledger. No role can write platform.balances; CHECK (available >= 0 AND reserved >= 0) makes an overdraw a
--     database error. platform.balance_drift recomputes SUM(ledger) and lists every disagreement; scripts/check-balance-invariant.sh fails on any row.
--   * The ledger, the price history, the price snapshot and (0003) the admin audit are APPEND-ONLY: privileges (platform_app / platform_admin hold SELECT and INSERT only) AND row/statement
--     triggers that raise even for the owner. What the owner can still do (disable or drop the trigger) is demonstrated by a test, not hidden.
--   * Each ledger kind has its own CHECK of sign pattern, required references and the arithmetic units x unit_cost; the unit cost is FROZEN on the reserve row, and a charge row must name a
--     (job, unit, price) that exists in that job's price snapshot (foreign key), so settlement cannot read the current price table.
--
-- NULL TRAP, read before editing a CHECK: a CHECK that evaluates to NULL PASSES. Every constraint below states IS NOT NULL / IS NULL explicitly instead of relying on comparison with NULL.
--
-- Everything is created AS platform_owner (SET LOCAL ROLE below; the migrator was made able to SET ROLE to it by platform_0000), so platform_owner owns it with no ALTER OWNER step.

SET LOCAL ROLE platform_owner;

-- ── functions that more than one table uses ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE FUNCTION platform.reject_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    RAISE EXCEPTION '% on %.% is not allowed: the table is append-only (corrections are new rows)', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
        USING ERRCODE = '23001';   -- restrict_violation
END
$fn$;
COMMENT ON FUNCTION platform.reject_mutation() IS 'Trigger body shared by every append-only table: UPDATE, DELETE and TRUNCATE all raise 23001, for every role including the owner.';
REVOKE ALL ON FUNCTION platform.reject_mutation() FROM PUBLIC;

-- ── admins: the NAMED human behind every admin action ──────────────────────────────────────────────────────────────────────────────────────────────────────────
-- Not in the plan's table list, but "recorded against a named admin user" needs a row to be a foreign key to. An admin IS a platform user (a Supabase login); admins are created out of
-- band by the owner, never by the application (platform_app holds SELECT only).
CREATE TABLE platform.admins (
    id           uuid        PRIMARY KEY REFERENCES public.users (id) ON DELETE RESTRICT,
    display_name text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    disabled_at  timestamptz,
    CONSTRAINT admins_not_the_system_user CHECK (id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CONSTRAINT admins_display_name_not_blank CHECK (btrim(display_name) <> '')
);
COMMENT ON TABLE platform.admins IS 'Named admins. id = the admin''s public.users.id. Created out of band; the application only reads it. disabled_at ends an admin without deleting the history that names them.';

-- ── modules: the registry ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.modules (
    id          text        PRIMARY KEY,
    name        text        NOT NULL UNIQUE,
    schema_name text        NOT NULL UNIQUE,
    metered     boolean     NOT NULL,
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT modules_id_shape CHECK (id ~ '^[a-z][a-z0-9_]*$'),
    CONSTRAINT modules_id_metered_key UNIQUE (id, metered)    -- the target of "credits exist only for metered modules" (balances, ledger)
);
COMMENT ON TABLE platform.modules IS 'The three modules. metered = credits are charged (M1, M2); M3 is entitlement-based and never has a balance (locked decision: Money).';
INSERT INTO platform.modules (id, name, schema_name, metered) VALUES
    ('m1', 'verify',        'verify',       true),
    ('m2', 'send-managed',  'send_managed', true),
    ('m3', 'send-byo',      'send_byo',     false);

-- ── unit costs, editable, WITH HISTORY ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.unit_costs (
    unit          text        PRIMARY KEY,
    module_id     text        NOT NULL REFERENCES platform.modules (id),
    description   text        NOT NULL,
    cost          integer     NOT NULL,
    changed_by    uuid        REFERENCES platform.admins (id),
    change_reason text,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT unit_costs_cost_at_least_one CHECK (cost >= 1),
    CONSTRAINT unit_costs_vendor_dearer CHECK (unit <> 'm1_vendor' OR cost > 1),      -- "admin-set, > 1": the fallback vendor is a premium
    CONSTRAINT unit_costs_unit_module_key UNIQUE (unit, module_id)                    -- the target of "a job's price row names a unit of the job's own module"
);
COMMENT ON TABLE platform.unit_costs IS 'The five billable units and their CURRENT cost in credits. A job freezes a copy of its module''s rows at confirm (platform.job_unit_prices); settlement never reads this table.';
COMMENT ON COLUMN platform.unit_costs.cost IS 'Integer credits per unit, >= 1 (m1_vendor > 1). Changing it needs changed_by (a named admin) and change_reason; the change is recorded in platform.unit_cost_history by trigger.';

CREATE TABLE platform.unit_cost_history (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    unit          text        NOT NULL REFERENCES platform.unit_costs (unit),
    old_cost      integer,
    new_cost      integer     NOT NULL,
    changed_by    uuid        REFERENCES platform.admins (id),
    change_reason text,
    changed_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT unit_cost_history_new_cost_at_least_one CHECK (new_cost >= 1)
);
COMMENT ON TABLE platform.unit_cost_history IS 'Append-only. One row per seed and per change of a unit cost, written by the unit_costs trigger (so no writer can skip it).';

CREATE FUNCTION platform.unit_costs_guard() RETURNS trigger
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
CREATE TRIGGER unit_costs_guard BEFORE UPDATE ON platform.unit_costs FOR EACH ROW EXECUTE FUNCTION platform.unit_costs_guard();

CREATE FUNCTION platform.unit_costs_record_history() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO platform.unit_cost_history (unit, old_cost, new_cost, changed_by, change_reason) VALUES (NEW.unit, NULL, NEW.cost, NEW.changed_by, COALESCE(NEW.change_reason, 'seeded'));
    ELSIF NEW.cost IS DISTINCT FROM OLD.cost THEN
        INSERT INTO platform.unit_cost_history (unit, old_cost, new_cost, changed_by, change_reason) VALUES (NEW.unit, OLD.cost, NEW.cost, NEW.changed_by, NEW.change_reason);
    END IF;
    RETURN NULL;
END
$fn$;
CREATE TRIGGER unit_costs_record_history AFTER INSERT OR UPDATE ON platform.unit_costs FOR EACH ROW EXECUTE FUNCTION platform.unit_costs_record_history();
REVOKE ALL ON FUNCTION platform.unit_costs_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION platform.unit_costs_record_history() FROM PUBLIC;

CREATE TRIGGER unit_cost_history_append_only BEFORE UPDATE OR DELETE ON platform.unit_cost_history FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER unit_cost_history_no_truncate BEFORE TRUNCATE ON platform.unit_cost_history FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- The five units of the build plan. m1_vendor is seeded 2 so the table is complete: the plan says "admin-set, > 1" and the real number is the owner's (Docs/phase-1-spec.md, open decision 3).
INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES
    ('m1_own',      'm1', 'M1 verification answered by our own infrastructure', 1),
    ('m1_cache',    'm1', 'M1 verification answered from cache (full price: the answer is the value)', 1),
    ('m1_vendor',   'm1', 'M1 verification answered by the vendor fallback (a premium: it costs real money)', 2),
    ('m2_accepted', 'm2', 'M2 send accepted by the receiving server', 1),
    ('m2_bounced',  'm2', 'M2 send bounced (550): the work was done, the bad address is the user''s list', 1);

-- ── jobs, and the price snapshot a job freezes at confirm ──────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.jobs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    module_id       text        NOT NULL REFERENCES platform.modules (id),
    status          text        NOT NULL DEFAULT 'draft',
    source_job_id   uuid,
    scheduled_start timestamptz,
    frozen_settings jsonb       NOT NULL DEFAULT '{}'::jsonb,
    module_job_ref  text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT jobs_status_vocabulary CHECK (status IN ('draft', 'scheduled', 'running', 'paused', 'cancelling', 'succeeded', 'failed', 'cancelled')),
    CONSTRAINT jobs_scheduled_has_a_start CHECK (status <> 'scheduled' OR scheduled_start IS NOT NULL),
    CONSTRAINT jobs_frozen_settings_is_an_object CHECK (jsonb_typeof(frozen_settings) = 'object'),
    CONSTRAINT jobs_not_its_own_source CHECK (source_job_id IS NULL OR source_job_id <> id),
    CONSTRAINT jobs_module_job_ref_not_blank CHECK (module_job_ref IS NULL OR btrim(module_job_ref) <> ''),
    CONSTRAINT jobs_id_user_key UNIQUE (id, user_id),                  -- chaining: a source job belongs to the same user
    CONSTRAINT jobs_id_module_key UNIQUE (id, module_id),              -- the price snapshot: a job's prices are its own module's units
    CONSTRAINT jobs_id_user_module_key UNIQUE (id, user_id, module_id),   -- the ledger: a job's rows belong to its user and module
    CONSTRAINT jobs_source_same_user_fk FOREIGN KEY (source_job_id, user_id) REFERENCES platform.jobs (id, user_id) ON DELETE RESTRICT
);
-- "One active job per module per user": a job that is RUNNING, PAUSED or CANCELLING holds the slot. A scheduled job (credits reserved, not started) does not; the start rule is P4's
-- (Docs/phase-1-spec.md, open decision 2).
CREATE UNIQUE INDEX jobs_one_executing_per_user_module ON platform.jobs (user_id, module_id) WHERE status IN ('running', 'paused', 'cancelling');
CREATE UNIQUE INDEX jobs_module_job_ref_key ON platform.jobs (module_id, module_job_ref) WHERE module_job_ref IS NOT NULL;
CREATE INDEX jobs_user_idx ON platform.jobs (user_id, created_at DESC);
COMMENT ON TABLE platform.jobs IS 'One verify or send job. source_job_id chains a send to the verification whose results it uses (same user). frozen_settings is fixed once the job leaves draft. module_job_ref is the module''s own id for it.';

CREATE FUNCTION platform.jobs_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.module_id IS DISTINCT FROM OLD.module_id OR NEW.source_job_id IS DISTINCT FROM OLD.source_job_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'platform.jobs: user_id, module_id, source_job_id and created_at never change' USING ERRCODE = '23001';
    END IF;
    IF OLD.status <> 'draft' AND NEW.frozen_settings IS DISTINCT FROM OLD.frozen_settings THEN
        RAISE EXCEPTION 'platform.jobs: frozen_settings is frozen once the job has left draft (it was % )', OLD.status USING ERRCODE = '23001';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;
CREATE TRIGGER jobs_guard BEFORE UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_guard();
REVOKE ALL ON FUNCTION platform.jobs_guard() FROM PUBLIC;

-- Why a snapshot TABLE and not one cost on the reserve row. Module 1's price depends on the OUTCOME (own 1, cache 1, vendor > 1), known only at settlement, while the credits are reserved at
-- confirm. So the job freezes EVERY unit price of its module here, the reserve row freezes the reserve rate (one of these prices), and each charge row must name a price from this snapshot.
CREATE TABLE platform.job_unit_prices (
    job_id    uuid    NOT NULL,
    module_id text    NOT NULL,
    unit      text    NOT NULL,
    cost      integer NOT NULL,
    CONSTRAINT job_unit_prices_pk PRIMARY KEY (job_id, unit),
    CONSTRAINT job_unit_prices_cost_at_least_one CHECK (cost >= 1),
    CONSTRAINT job_unit_prices_job_unit_cost_key UNIQUE (job_id, unit, cost),            -- the target of the ledger's frozen-price foreign key
    CONSTRAINT job_unit_prices_job_fk  FOREIGN KEY (job_id, module_id) REFERENCES platform.jobs (id, module_id) ON DELETE RESTRICT,
    CONSTRAINT job_unit_prices_unit_fk FOREIGN KEY (unit, module_id)   REFERENCES platform.unit_costs (unit, module_id)
);
COMMENT ON TABLE platform.job_unit_prices IS 'Insert-only. The price of every unit of the job''s module, copied from platform.unit_costs when the job is confirmed. Settlement uses THESE rows, never the current table.';
CREATE TRIGGER job_unit_prices_append_only BEFORE UPDATE OR DELETE ON platform.job_unit_prices FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER job_unit_prices_no_truncate BEFORE TRUNCATE ON platform.job_unit_prices FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- ── balances: one row per user per METERED module, written only by the ledger trigger ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.balances (
    user_id    uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    module_id  text        NOT NULL,
    metered    boolean     GENERATED ALWAYS AS (true) STORED,     -- always true: the foreign key below then admits only metered modules (M1, M2), never M3
    available  bigint      NOT NULL DEFAULT 0,
    reserved   bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT balances_pk PRIMARY KEY (user_id, module_id),
    CONSTRAINT balances_available_not_negative CHECK (available >= 0),
    CONSTRAINT balances_reserved_not_negative CHECK (reserved >= 0),
    CONSTRAINT balances_module_is_metered_fk FOREIGN KEY (module_id, metered) REFERENCES platform.modules (id, metered)
);
COMMENT ON TABLE platform.balances IS 'Credits per user per metered module (M1, M2). Credits never move between modules. NO ROLE WRITES THIS TABLE: the ledger trigger is its only writer, so the stored balance is the sum of its ledger rows by construction (platform.balance_drift proves it).';
COMMENT ON COLUMN platform.balances.available IS 'Credits the user can still spend. = SUM(ledger.available_delta). Never negative: an overdraw is a database error.';
COMMENT ON COLUMN platform.balances.reserved IS 'Credits held for confirmed, unsettled jobs. = SUM(ledger.reserved_delta). Never negative.';

-- ── the ledger ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.ledger (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    module_id        text        NOT NULL,
    kind             text        NOT NULL,
    monetary         boolean     GENERATED ALWAYS AS (kind IN ('grant', 'reserve', 'charge', 'release')) STORED,
    reserve_kind     text        GENERATED ALWAYS AS (CASE WHEN kind IN ('charge', 'release') THEN 'reserve' END) STORED,
    available_delta  bigint      NOT NULL DEFAULT 0,
    reserved_delta   bigint      NOT NULL DEFAULT 0,
    job_id           uuid,
    reserve_id       bigint,
    unit             text,
    units            integer,
    unit_cost        integer,
    acting_admin_id  uuid        REFERENCES platform.admins (id) ON DELETE RESTRICT,
    reason           text,
    idempotency_key  text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_kind_vocabulary CHECK (kind IN ('grant', 'reserve', 'charge', 'release', 'm3_enable', 'm3_disable', 'm3_expire')),
    CONSTRAINT ledger_idempotency_key_not_blank CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT ledger_idempotency_key UNIQUE (user_id, module_id, idempotency_key),
    CONSTRAINT ledger_reserve_target_key UNIQUE (id, user_id, job_id, kind),            -- the target of charge/release -> reserve
    -- a grant: an admin adds credits, with a reason, tied to no job
    CONSTRAINT ledger_shape_grant CHECK (kind <> 'grant' OR (
        available_delta > 0 AND reserved_delta = 0 AND job_id IS NULL AND reserve_id IS NULL AND unit IS NULL AND units IS NULL AND unit_cost IS NULL
        AND acting_admin_id IS NOT NULL AND reason IS NOT NULL AND btrim(reason) <> '')),
    -- a reserve: moves units x frozen unit cost from available to reserved; the frozen cost is HERE
    CONSTRAINT ledger_shape_reserve CHECK (kind <> 'reserve' OR (
        available_delta < 0 AND reserved_delta = -available_delta AND job_id IS NOT NULL AND reserve_id IS NULL
        AND unit IS NOT NULL AND units IS NOT NULL AND unit_cost IS NOT NULL AND units >= 1 AND unit_cost >= 1
        AND -available_delta = units::bigint * unit_cost::bigint)),
    -- a charge: consumes reserved credits at the price frozen for the job
    CONSTRAINT ledger_shape_charge CHECK (kind <> 'charge' OR (
        available_delta = 0 AND reserved_delta < 0 AND job_id IS NOT NULL AND reserve_id IS NOT NULL
        AND unit IS NOT NULL AND units IS NOT NULL AND unit_cost IS NOT NULL AND units >= 1 AND unit_cost >= 1
        AND -reserved_delta = units::bigint * unit_cost::bigint)),
    -- a release: returns reserved credits to available
    CONSTRAINT ledger_shape_release CHECK (kind <> 'release' OR (
        available_delta > 0 AND reserved_delta = -available_delta AND job_id IS NOT NULL AND reserve_id IS NOT NULL
        AND unit IS NULL AND units IS NULL AND unit_cost IS NULL)),
    -- M3 entitlement events move no credits; enable and disable name an admin, expiry is the system
    CONSTRAINT ledger_shape_m3 CHECK (kind NOT IN ('m3_enable', 'm3_disable', 'm3_expire') OR (
        available_delta = 0 AND reserved_delta = 0 AND job_id IS NULL AND reserve_id IS NULL AND unit IS NULL AND units IS NULL AND unit_cost IS NULL
        AND ((acting_admin_id IS NOT NULL) = (kind <> 'm3_expire')))),
    -- a monetary row belongs to a METERED module and an M3 row to the unmetered one (M3 has no balance)
    CONSTRAINT ledger_module_metering_fk FOREIGN KEY (module_id, monetary) REFERENCES platform.modules (id, metered),
    -- a job's rows belong to that job's user and module
    CONSTRAINT ledger_job_fk FOREIGN KEY (job_id, user_id, module_id) REFERENCES platform.jobs (id, user_id, module_id) ON DELETE RESTRICT,
    -- charge and release point at a RESERVE row of the same user and job (reserve_kind is 'reserve' only for those two kinds)
    CONSTRAINT ledger_reserve_fk FOREIGN KEY (reserve_id, user_id, job_id, reserve_kind) REFERENCES platform.ledger (id, user_id, job_id, kind) ON DELETE RESTRICT,
    -- the unit and its price must exist in the job's frozen price snapshot: settlement cannot use today's price
    CONSTRAINT ledger_frozen_price_fk FOREIGN KEY (job_id, unit, unit_cost) REFERENCES platform.job_unit_prices (job_id, unit, cost) ON DELETE RESTRICT
);
CREATE INDEX ledger_user_module_idx ON platform.ledger (user_id, module_id, id);
CREATE INDEX ledger_job_idx ON platform.ledger (job_id) WHERE job_id IS NOT NULL;
COMMENT ON TABLE platform.ledger IS 'THE money record. Append-only: UPDATE, DELETE and TRUNCATE are refused for every role (privileges) and for the owner too (triggers). Each row carries signed integer deltas; balances is their sum, maintained by trigger. Kinds: grant, reserve, charge, release (monetary, M1/M2 only) and m3_enable, m3_disable, m3_expire (no credits, M3 only).';
COMMENT ON COLUMN platform.ledger.unit_cost IS 'Integer credits per unit, FROZEN: on a reserve row the reserve rate, on a charge row the price of the unit actually delivered, both taken from the job''s snapshot (platform.job_unit_prices).';
COMMENT ON COLUMN platform.ledger.idempotency_key IS 'The caller''s retry key: the same (user, module, key) can be written once, so a retried grant, reserve or charge cannot double-count.';
COMMENT ON COLUMN platform.ledger.acting_admin_id IS 'The named admin who did it. NULL for a user''s own action and for the automatic M3 expiry.';

-- the ONLY writer of platform.balances (the balance invariant is enforced here)
CREATE FUNCTION platform.ledger_apply_to_balance() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.monetary THEN
        -- Two statements, not one INSERT ... ON CONFLICT DO UPDATE: a CHECK is evaluated on the PROPOSED insert row before the conflict is resolved, so a reserve (negative
        -- available_delta) would be refused even when the existing balance can pay for it. The UPDATE takes the row lock, which also serialises concurrent writers of one balance.
        INSERT INTO platform.balances (user_id, module_id) VALUES (NEW.user_id, NEW.module_id) ON CONFLICT (user_id, module_id) DO NOTHING;
        UPDATE platform.balances
           SET available = available + NEW.available_delta, reserved = reserved + NEW.reserved_delta, updated_at = now()
         WHERE user_id = NEW.user_id AND module_id = NEW.module_id;
    END IF;
    RETURN NULL;
END
$fn$;
COMMENT ON FUNCTION platform.ledger_apply_to_balance() IS 'SECURITY DEFINER (owner platform_owner): the only code path that writes platform.balances.';
CREATE TRIGGER ledger_apply_to_balance AFTER INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_apply_to_balance();
REVOKE ALL ON FUNCTION platform.ledger_apply_to_balance() FROM PUBLIC;

CREATE TRIGGER ledger_append_only BEFORE UPDATE OR DELETE ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON platform.ledger FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- ── the recompute: the alarm that notices if the trigger was ever disabled or bypassed ───────────────────────────────────────────────────────────────────────────
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

CREATE VIEW platform.reserve_overdrawn AS
SELECT r.id AS reserve_id, r.user_id, r.module_id, r.job_id, (-r.available_delta)::bigint AS reserved_amount,
       COALESCE(sum(CASE WHEN s.kind = 'charge' THEN -s.reserved_delta ELSE s.available_delta END), 0)::bigint AS settled_amount
FROM platform.ledger r
JOIN platform.ledger s ON s.reserve_id = r.id
WHERE r.kind = 'reserve'
GROUP BY r.id, r.user_id, r.module_id, r.job_id, r.available_delta
HAVING COALESCE(sum(CASE WHEN s.kind = 'charge' THEN -s.reserved_delta ELSE s.available_delta END), 0) > -r.available_delta;
COMMENT ON VIEW platform.reserve_overdrawn IS 'Every reserve whose charges plus releases exceed what it reserved (the per-reserve half of the invariant, which the per-balance check cannot see). Empty when it holds.';

-- ── M3 entitlements ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
CREATE TABLE platform.m3_entitlements (
    user_id       uuid        PRIMARY KEY REFERENCES public.users (id) ON DELETE RESTRICT,
    enabled       boolean     NOT NULL,
    enabled_until timestamptz,
    changed_by    uuid        REFERENCES platform.admins (id) ON DELETE RESTRICT,
    changed_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT m3_entitlements_until_needs_enabled CHECK (enabled_until IS NULL OR enabled)
);
COMMENT ON TABLE platform.m3_entitlements IS 'M3 is not metered: an admin enables it per user, optionally until a date, after which it auto-disables. The ledger records every change (kinds m3_enable, m3_disable, m3_expire). changed_by is NULL only for the automatic expiry.';

-- ── privileges: explicit per table, nothing by default (the grants test fails on a table or privilege nobody declared) ───────────────────────────────────────────
GRANT SELECT ON platform.admins, platform.modules, platform.unit_cost_history, platform.balances, platform.balance_drift, platform.reserve_overdrawn TO platform_app, platform_admin;
GRANT SELECT ON platform.unit_costs TO platform_app, platform_admin;
GRANT UPDATE (cost, changed_by, change_reason) ON platform.unit_costs TO platform_app;
GRANT SELECT, INSERT, UPDATE ON platform.jobs TO platform_app;
GRANT SELECT ON platform.jobs TO platform_admin;
GRANT SELECT, INSERT ON platform.job_unit_prices TO platform_app;
GRANT SELECT ON platform.job_unit_prices TO platform_admin;
GRANT SELECT, INSERT ON platform.ledger TO platform_app, platform_admin;
GRANT SELECT, INSERT, UPDATE ON platform.m3_entitlements TO platform_app;
GRANT SELECT ON platform.m3_entitlements TO platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0002');
