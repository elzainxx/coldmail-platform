-- platform_0014_suppressed_flag.up.sql — Phase 4 close-out, rule D2 (owner ruling OD-3): the user's suppression list applies to SEND jobs only. Docs/phase-4-spec.md, Part D.
--
-- WHY. Suppression exists so that nobody who unsubscribed is e-mailed. A verify job e-mails nobody: it asks module 1 whether an address exists. So a suppressed address in a VERIFY job is verified and charged like any
-- other (it stays `pending`, reaches the module, counts in the reserve) and the user is TOLD, in the results, that the address is on their suppression list. In a SEND job (modules 2 and 3) nothing changes: the row
-- becomes state `suppressed`, gets the platform's free result P-SUP and never reaches the module. A chained send takes the verify job's results as its rows and applies the list again at ITS confirm.
--
--   1. platform.job_rows.suppressed (boolean NOT NULL DEFAULT false): the user's list held this address when the job was confirmed. Set ONLY by the confirm (the frozen-rows guard lets it change only while the job is
--      draft or previewed, like every column of a row); for a send job the same statement also makes the row's state `suppressed`, for a verify job the state stays `pending`.
--        job_rows_suppressed_state_has_the_flag      a row whose state is `suppressed` has the flag (a writer cannot make a suppressed row look like any other)
--        job_rows_flag_only_on_listable_rows         the flag is only on a row that is pending or suppressed (a malformed, duplicate or excluded row was never going to be sent: it has no address on the list)
--        job_rows_pending_flag_is_verify_only        (the row guard) a PENDING row that carries the flag exists only in a module 1 job: in a send job a flagged row is state `suppressed`, so a flagged pending row would reach the
--                                                    module although the user suppressed it
--   2. platform.job_results.suppressed (boolean NOT NULL DEFAULT false): the same fact on the result, so that the results (JSON and CSV) say it. The table is insert-only, so the flag is DERIVED at insert by the
--      existing BEFORE INSERT guard (job_results_after_confirm) from the result's own row, whatever the writer said: ONE place derives it, and RecordResults, the confirm, a cancel and every writer that comes later
--      (the admin surface) get it without naming it. It is never taken from the writer.
--        job_results_suppressed_platform_outcome     the platform's own result `suppressed` carries the flag
--   3. Privileges: platform_app may UPDATE job_rows.suppressed (the confirm sets it); the results' table-level SELECT, INSERT grants already cover the new column; platform_admin still only reads.
--
-- AN UPGRADE of a database that already holds jobs: the rows whose state is `suppressed` get the flag, and the results of those rows too (the one-time backfill switches the append-only trigger of the results off for
-- the duration of this transaction, as the table's owner, and back on). The old row guard is still in place while the rows are backfilled: it lets a change of this one column through.
-- NULL TRAP (CLAUDE.md rule 13): each CHECK is a boolean expression over NOT NULL columns (state, origin, module_state, suppressed), so none can evaluate to NULL.
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── 1. the rows ───────────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.job_rows ADD COLUMN suppressed boolean NOT NULL DEFAULT false;
UPDATE platform.job_rows SET suppressed = true WHERE state = 'suppressed';
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_suppressed_state_has_the_flag CHECK (state <> 'suppressed' OR suppressed);
ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_flag_only_on_listable_rows CHECK (NOT suppressed OR state IN ('pending', 'suppressed'));
COMMENT ON COLUMN platform.job_rows.suppressed IS 'Phase 4 D2: the user''s suppression list held this address when the job was confirmed. A send job''s suppressed row is state suppressed (never sent); a verify job''s stays pending (verified, charged) and carries the flag into its results. Set only by the confirm.';

-- ── 2. the results ────────────────────────────────────────────────────────────────────────────────────────────────────
ALTER TABLE platform.job_results ADD COLUMN suppressed boolean NOT NULL DEFAULT false;
ALTER TABLE platform.job_results DISABLE TRIGGER job_results_append_only;
UPDATE platform.job_results r SET suppressed = true FROM platform.job_rows w WHERE w.job_id = r.job_id AND w.id = r.item_ref AND w.suppressed;
ALTER TABLE platform.job_results ENABLE TRIGGER job_results_append_only;
ALTER TABLE platform.job_results ADD CONSTRAINT job_results_suppressed_platform_outcome CHECK (NOT (origin = 'platform' AND module_state = 'suppressed') OR suppressed);
COMMENT ON COLUMN platform.job_results.suppressed IS 'Phase 4 D2: the address was on the user''s suppression list when the job was confirmed (the row''s flag, copied at insert by job_results_after_confirm: no writer states it). A verify job''s result carries it next to its verdict; a send job''s suppressed row has it on the platform''s P-SUP result.';

-- ── 3. the guards ─────────────────────────────────────────────────────────────────────────────────────────────────────
-- the rows: platform_0012's guard, with the new column in the comparison that lets only the engine's submitted_at through, and the rule that a pending row with the flag is a verify job's
CREATE OR REPLACE FUNCTION platform.job_rows_frozen() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_module text;
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
           AND NEW.source_item IS NOT DISTINCT FROM OLD.source_item AND NEW.suppressed IS NOT DISTINCT FROM OLD.suppressed THEN
            RETURN NEW;                   -- only submitted_at moves
        END IF;
    END IF;
    SELECT j.status, j.module_id INTO v_status, v_module FROM platform.jobs j WHERE j.id = v_job;
    IF v_status IS NULL OR v_status NOT IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_rows: job % is % and its list can no longer change (a row changes only while the job is draft or previewed)', v_job, v_status
            USING ERRCODE = '23001', CONSTRAINT = 'job_rows_frozen';
    END IF;
    IF TG_OP <> 'DELETE' THEN
        IF NEW.suppressed AND NEW.state = 'pending' AND v_module IS DISTINCT FROM 'm1' THEN
            RAISE EXCEPTION 'platform.job_rows: row % is pending and flagged suppressed in a job of module %: only a verify job keeps a suppressed address pending (a send job''s suppressed row is state suppressed and never sent)', NEW.id, v_module
                USING ERRCODE = '23514', CONSTRAINT = 'job_rows_pending_flag_is_verify_only';
        END IF;
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END
$fn$;
COMMENT ON FUNCTION platform.job_rows_frozen() IS 'SECURITY INVOKER: a row of a job changes only while the job is draft or previewed (23001 job_rows_frozen), the flag suppressed included; submitted_at, the engine''s stamp, is the one column that moves later, once. A pending row flagged suppressed is a verify job''s (23514 job_rows_pending_flag_is_verify_only).';

-- the results: platform_0012's guard, which also DERIVES the flag from the result's own row (the writer's value is overwritten)
CREATE OR REPLACE FUNCTION platform.job_results_after_confirm() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_status text;
    v_email  text;
    v_flag   boolean;
BEGIN
    SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = NEW.job_id;
    IF v_status IS NULL OR v_status IN ('draft', 'previewed') THEN
        RAISE EXCEPTION 'platform.job_results: job % is % and has no results before it is confirmed', NEW.job_id, v_status
            USING ERRCODE = '23514', CONSTRAINT = 'jobs_results_after_confirm';
    END IF;
    -- a result names the address of ITS row (the normalised one, or the raw text of a row that did not normalise): the foreign key already ties the position
    SELECT COALESCE(w.email_norm, w.email_raw), w.suppressed INTO v_email, v_flag FROM platform.job_rows w WHERE w.job_id = NEW.job_id AND w.id = NEW.item_ref;
    IF v_email IS DISTINCT FROM NEW.email THEN
        RAISE EXCEPTION 'platform.job_results: the result of row % names an address that is not the row''s', NEW.item_ref
            USING ERRCODE = '23514', CONSTRAINT = 'job_results_match_their_row';
    END IF;
    NEW.suppressed := COALESCE(v_flag, false);     -- derived, never stated: the row's own flag
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.job_results_after_confirm() IS 'SECURITY INVOKER: a result can be recorded only for a job that has left draft and previewed (23514 jobs_results_after_confirm) and must name the address of its own row (23514 job_results_match_their_row); its suppressed flag is the row''s, set here whatever the writer said.';

-- ── 4. privileges ───────────────────────────────────────────────────────────────────────────────────────────────────────
GRANT UPDATE (suppressed) ON platform.job_rows TO platform_app;

INSERT INTO platform.schema_migrations (version) VALUES ('0014');
