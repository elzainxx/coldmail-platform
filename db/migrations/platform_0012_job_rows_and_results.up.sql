-- platform_0012_job_rows_and_results.up.sql — Phase 4, chunk B2. A job's LIST and its RESULTS are the platform's own tables, and the results are kept forever. Docs/phase-4-spec.md, "Locked here" 25.
--
--   platform.job_rows     one row per input line of a job, classified ONCE at creation or preview: pending (it will reach the module), malformed (norm.Validate refused it: the reason is kept), duplicate (a repeat of an
--                         earlier row's normalised address inside the job), suppressed (the user's own suppression list) or excluded (a chained job's source verdict that the user did not select). The row's id is the
--                         ITEM REFERENCE for every module: module 1's item_id, modules 2 and 3's job_item_id (public.job_items.id), the ledger's item_ref. A row can be added, changed or removed only while its job is draft or
--                         previewed (jobs_frozen_rows trigger), except submitted_at, which the engine stamps when the module has the item.
--   platform.job_results  one row per job row, written ONCE when its outcome is final, in the same transaction as the settlement it explains; never updated, never deleted, for anybody (the owner included). It holds the
--                         platform's own outcomes too (malformed, duplicate, suppressed, excluded, and expired, cancelled or failed for a row the module never saw), so a terminal job has exactly as many results as rows
--                         (platform.job_settlement_drift says when it has not). decision/unit/rule are what internal/charge decided; a charged row names the unit that the ledger's charge row of the same (job, item) holds.
--   platform.job_settlement_drift  the three-way proof's database half: a ledger charge without its result (or with another unit), a charged result without its ledger charge, a confirmed job that ended with fewer results than rows
--                         (a result of a row that is not in the job cannot exist: the composite foreign key). Empty means the ledger and the results agree; the module's report is compared by the harness tests.
--
-- A job that has not left previewed holds nothing; results exist only for jobs that have (jobs_results_after_confirm, 23514). Every table carries user_id and a composite foreign key to the job's (id, user_id) so that a
-- tenant filter is always one column away and a row can never name another user's job.
--
-- NULL TRAP (CLAUDE.md rule 13): each CHECK below is built from boolean expressions over NOT NULL columns, or states IS NULL / IS NOT NULL.
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

CREATE TABLE platform.job_rows (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id       uuid        NOT NULL,
    user_id      uuid        NOT NULL,
    ordinal      integer     NOT NULL,
    email_raw    text        NOT NULL,
    email_norm   text,
    state        text        NOT NULL DEFAULT 'pending',
    reason       text,
    duplicate_of uuid,
    fields       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    source_item  uuid,
    submitted_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_rows_job_fk        FOREIGN KEY (job_id, user_id) REFERENCES platform.jobs (id, user_id) ON DELETE RESTRICT,
    CONSTRAINT job_rows_job_id_key    UNIQUE (job_id, id),
    CONSTRAINT job_rows_ordinal_key   UNIQUE (job_id, ordinal),
    CONSTRAINT job_rows_id_ordinal_key UNIQUE (job_id, id, ordinal),     -- the target of the result's foreign key: a result carries the position of ITS row
    CONSTRAINT job_rows_duplicate_fk  FOREIGN KEY (job_id, duplicate_of) REFERENCES platform.job_rows (job_id, id) ON DELETE RESTRICT,
    CONSTRAINT job_rows_ordinal_positive CHECK (ordinal >= 1),
    CONSTRAINT job_rows_state_vocabulary CHECK (state IN ('pending', 'malformed', 'duplicate', 'suppressed', 'excluded')),
    CONSTRAINT job_rows_email_raw_bounded CHECK (octet_length(email_raw) <= 4096),
    CONSTRAINT job_rows_email_norm_shape CHECK ((state = 'malformed') = (email_norm IS NULL)),
    CONSTRAINT job_rows_duplicate_shape CHECK ((state = 'duplicate') = (duplicate_of IS NOT NULL)),
    CONSTRAINT job_rows_duplicate_not_itself CHECK (duplicate_of IS NULL OR duplicate_of <> id),
    CONSTRAINT job_rows_reason_shape CHECK (state NOT IN ('malformed', 'excluded') OR (reason IS NOT NULL AND btrim(reason) <> '')),
    CONSTRAINT job_rows_fields_is_an_object CHECK (jsonb_typeof(fields) = 'object'),
    CONSTRAINT job_rows_only_pending_is_submitted CHECK (submitted_at IS NULL OR state = 'pending')
);
-- an address is never sent twice in one job: at most one row per normalised address can be pending (or suppressed, which is a pending row the user's own list took out at confirm); every later one is a duplicate
CREATE UNIQUE INDEX job_rows_one_pending_per_address ON platform.job_rows (job_id, email_norm) WHERE state IN ('pending', 'suppressed');
CREATE INDEX job_rows_to_submit_idx ON platform.job_rows (job_id, ordinal) WHERE state = 'pending' AND submitted_at IS NULL;
CREATE INDEX job_rows_user_idx ON platform.job_rows (user_id, job_id);
COMMENT ON TABLE platform.job_rows IS 'One row per input line of a job, classified once. id is the item reference for every module (module 1''s item_id, modules 2 and 3''s job_item_id, the ledger''s item_ref). Changes only while the job is draft or previewed, except submitted_at.';

CREATE TABLE platform.job_results (
    job_id       uuid        NOT NULL,
    user_id      uuid        NOT NULL,
    item_ref     uuid        NOT NULL,
    ordinal      integer     NOT NULL,
    email        text        NOT NULL,
    origin       text        NOT NULL,
    module_state text        NOT NULL,
    reason       text,
    decision     text        NOT NULL,
    unit         text,
    rule         text        NOT NULL,
    detail       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_results_pk PRIMARY KEY (job_id, item_ref),
    CONSTRAINT job_results_job_fk FOREIGN KEY (job_id, user_id) REFERENCES platform.jobs (id, user_id) ON DELETE RESTRICT,
    CONSTRAINT job_results_row_fk FOREIGN KEY (job_id, item_ref, ordinal) REFERENCES platform.job_rows (job_id, id, ordinal) ON DELETE RESTRICT,
    CONSTRAINT job_results_origin_vocabulary CHECK (origin IN ('module', 'platform')),
    CONSTRAINT job_results_decision_vocabulary CHECK (decision IN ('charge', 'free', 'unmapped')),
    CONSTRAINT job_results_charged_names_its_unit CHECK ((decision = 'charge') = (unit IS NOT NULL)),
    CONSTRAINT job_results_state_not_blank CHECK (btrim(module_state) <> '' AND btrim(rule) <> ''),
    CONSTRAINT job_results_detail_is_an_object CHECK (jsonb_typeof(detail) = 'object')
);
CREATE INDEX job_results_page_idx ON platform.job_results (job_id, ordinal);
CREATE INDEX job_results_user_idx ON platform.job_results (user_id, job_id);
COMMENT ON TABLE platform.job_results IS 'Every row''s final outcome, forever. Insert-only for everybody. decision, unit and rule are internal/charge''s; origin says whether the module or the platform decided.';

CREATE TRIGGER job_results_append_only BEFORE UPDATE OR DELETE ON platform.job_results FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER job_results_no_truncate BEFORE TRUNCATE ON platform.job_results FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- rows change only while the job can still be edited; submitted_at is the engine's stamp and may change (once) at any time
CREATE FUNCTION platform.job_rows_frozen() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_job    uuid := CASE WHEN TG_OP = 'INSERT' THEN NEW.job_id ELSE OLD.job_id END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id OR NEW.job_id IS DISTINCT FROM OLD.job_id OR NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
           OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'platform.job_rows: id, job_id, user_id, ordinal and created_at never change' USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
        END IF;
        IF NEW.submitted_at IS DISTINCT FROM OLD.submitted_at AND OLD.submitted_at IS NOT NULL THEN
            RAISE EXCEPTION 'platform.job_rows: row % was already submitted at %: the stamp is written once', OLD.id, OLD.submitted_at USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
        END IF;
        IF NEW.email_raw IS NOT DISTINCT FROM OLD.email_raw AND NEW.email_norm IS NOT DISTINCT FROM OLD.email_norm AND NEW.state IS NOT DISTINCT FROM OLD.state
           AND NEW.reason IS NOT DISTINCT FROM OLD.reason AND NEW.duplicate_of IS NOT DISTINCT FROM OLD.duplicate_of AND NEW.fields IS NOT DISTINCT FROM OLD.fields
           AND NEW.source_item IS NOT DISTINCT FROM OLD.source_item THEN
            RETURN NEW;                   -- only submitted_at moves
        END IF;
    END IF;
    SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = v_job;
    IF v_status IS NULL OR v_status NOT IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_rows: job % is % and its list can no longer change (a row changes only while the job is draft or previewed)', v_job, v_status
            USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END
$fn$;
COMMENT ON FUNCTION platform.job_rows_frozen() IS 'SECURITY INVOKER: a row of a job changes only while the job is draft or previewed (23001 job_rows_frozen); submitted_at, the engine''s stamp, is the one column that moves later, once.';
REVOKE ALL ON FUNCTION platform.job_rows_frozen() FROM PUBLIC;
CREATE TRIGGER job_rows_frozen BEFORE INSERT OR UPDATE OR DELETE ON platform.job_rows FOR EACH ROW EXECUTE FUNCTION platform.job_rows_frozen();

-- no result for a job that has not been confirmed
CREATE FUNCTION platform.job_results_after_confirm() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_email  text;
BEGIN
    SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = NEW.job_id;
    IF v_status IS NULL OR v_status IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_results: job % is % and has no results before it is confirmed', NEW.job_id, v_status
            USING ERRCODE = '23514', CONSTRAINT = 'jobs_results_after_confirm';
    END IF;
    -- a result names the address of ITS row (the normalised one, or the raw text of a row that did not normalise): the foreign key already ties the position
    SELECT COALESCE(w.email_norm, w.email_raw) INTO v_email FROM platform.job_rows w WHERE w.job_id = NEW.job_id AND w.id = NEW.item_ref;
    IF v_email IS DISTINCT FROM NEW.email THEN
        RAISE EXCEPTION 'platform.job_results: the result of row % names an address that is not the row''s', NEW.item_ref
            USING ERRCODE = '23514', CONSTRAINT = 'job_results_match_their_row';
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.job_results_after_confirm() IS 'SECURITY INVOKER: a result can be recorded only for a job that has left draft and previewed (23514 jobs_results_after_confirm) and must name the address of its own row (23514 job_results_match_their_row).';
REVOKE ALL ON FUNCTION platform.job_results_after_confirm() FROM PUBLIC;
CREATE TRIGGER job_results_after_confirm BEFORE INSERT ON platform.job_results FOR EACH ROW EXECUTE FUNCTION platform.job_results_after_confirm();

-- ── the database half of the three-way proof ─────────────────────────────────────────────────────────────────────────
CREATE VIEW platform.job_settlement_drift AS
    -- a ledger charge with no result of the same (job, item), or a result that names another unit
    SELECT l.job_id, l.item_ref, 'charge_without_result'::text AS problem
      FROM platform.ledger l
     WHERE l.kind = 'charge'
       AND NOT EXISTS (SELECT 1 FROM platform.job_results r WHERE r.job_id = l.job_id AND r.item_ref::text = l.item_ref AND r.decision = 'charge' AND r.unit = l.unit)
    UNION ALL
    -- a charged result with no ledger charge
    SELECT r.job_id, r.item_ref::text, 'result_without_charge'
      FROM platform.job_results r
     WHERE r.decision = 'charge'
       AND NOT EXISTS (SELECT 1 FROM platform.ledger l WHERE l.kind = 'charge' AND l.job_id = r.job_id AND l.item_ref = r.item_ref::text AND l.unit = r.unit)
    UNION ALL
    -- a terminal job that has fewer results than rows
    SELECT j.id, NULL::text, 'terminal_job_missing_results'
      FROM platform.jobs j
     WHERE j.status IN ('succeeded', 'failed', 'cancelled', 'expired')
       AND j.confirmed_at IS NOT NULL
       AND (SELECT count(*) FROM platform.job_rows w WHERE w.job_id = j.id) <> (SELECT count(*) FROM platform.job_results r WHERE r.job_id = j.id);
COMMENT ON VIEW platform.job_settlement_drift IS 'Empty when the ledger, the results and the rows agree: every charge has its result with the same unit, every charged result has its charge, every confirmed job that ended has exactly as many results as rows. The module''s report is compared by the harness tests.';

-- ── privileges ──────────────────────────────────────────────────────────────────────────────────────────────────────
GRANT SELECT, INSERT, DELETE ON platform.job_rows TO platform_app;
GRANT UPDATE (email_raw, email_norm, state, reason, duplicate_of, fields, source_item, submitted_at) ON platform.job_rows TO platform_app;
GRANT SELECT ON platform.job_rows TO platform_admin;
GRANT SELECT, INSERT ON platform.job_results TO platform_app;
GRANT SELECT ON platform.job_results TO platform_admin;
GRANT SELECT ON platform.job_settlement_drift TO platform_app, platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0012');
