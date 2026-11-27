-- platform_0021_retention_clears_template_and_merge_data.up.sql — Phase 5, S8 extended by an owner ruling (Docs/phase-5-spec.md, decision 15). At the same mark as the rendered messages (PLATFORM_CONTENT_RETENTION_DAYS after a job
-- ended), the platform also clears (1) the campaign template of the job (subject and HTML in the job's frozen content become '[removed]', the text becomes empty; reply_to stays: it is an address) and (2) every merge-data
-- value of the uploaded rows (platform.job_rows.fields becomes {}). The rule applies to EVERY terminal job (a verify job's rows may carry merge columns too); the rendered public.job_items clearing stays for send jobs. KEPT FOR EVER:
-- the recipient address (email_raw, email_norm, platform.job_results.email), the per-item outcome, the charge, the job's dates and counts, the events. platform.jobs.content_cleared_at is set ONCE, in the transaction of the LAST
-- page of all of it (internal/jobs retention.go: the rendered items first, then the pages of merge data, the last of which also clears the template and sets the mark).
--
-- WHAT THE DATABASE HOLDS (the store only does what these two guards allow, and nothing else is let through):
--   1. platform.job_rows_frozen() (the rows' guard, latest text platform_0014): a row of a job that has left draft and previewed still changes in no column but submitted_at, once. NEW: the ONE other change it lets through is
--      `fields` going to exactly '{}' with every other column of the row as it was, on a row of a job in a TERMINAL status (succeeded, failed, cancelled, expired). A fields update to anything else, on any job that is not terminal,
--      and any other column beside it are refused as before (23001, job_rows_frozen). A draft's or a previewed job's list stays editable as platform_0012 made it (this migration does not tighten or widen that).
--   2. platform.jobs_guard() (the job's own guard, latest text platform_0011): frozen_settings, the preview, its fingerprint and the confirm key never change once a job has left previewed (23001). NEW: the ONE other change it
--      lets through is the template clearing: on a job in a TERMINAL status, frozen_settings becoming EXACTLY what jsonb_set makes of the old document with content.subject = "[removed]", content.html = "[removed]" and
--      content.text = "", with the preview, its fingerprint and the confirm key unchanged. reply_to, the settings and every other key stay, and a change to any other value or any other key is refused as before.
--   3. NO NEW GRANT. platform_app already holds UPDATE on platform.job_rows (fields) since platform_0012 (column-level: email_raw, email_norm, state, reason, duplicate_of, fields, source_item, submitted_at; suppressed since
--      platform_0014) and the table-level UPDATE on platform.jobs: the guards above are what bind them, and the administrator role still reads only. No function is added, so no EXECUTE is granted.
--
-- WHAT IT CARRIES OVER (each replaced function diffed against its latest text): platform.job_rows_frozen() CARRIES every term of platform_0014's text (the identity columns never change; the submit stamp is written once; the
-- write that moves only submitted_at passes; a list changes only while the job is draft or previewed; a pending row flagged suppressed is a verify job's, 23514 job_rows_pending_flag_is_verify_only; the DELETE and INSERT paths)
-- and ADDS the one branch above inside its UPDATE path, before the status test. platform.jobs_guard() CARRIES every term of platform_0011's text (user_id, module_id, source_job_id and created_at never change; the frozen
-- columns are frozen after previewed, 23001; updated_at is the guard's) and ADDS the term `AND NOT (the template clearing)` to the frozen-columns test. No CHECK is replaced. The partial index jobs_content_retention_idx
-- (platform_0018) is replaced because the rule now covers every terminal job: it loses the term that limited it to jobs with a campaign (jsonb_exists(frozen_settings, 'content')), and keeps the other two.
-- UPGRADE: a job already marked under platform_0018 was cleared of its rendered messages only, so its mark is taken back (the set-once trigger is switched off for that one statement, as the table's owner) and the purge goes
-- over it once more; the items that already hold the placeholder are skipped, the template and the merge data are cleared, and it is marked again. The down REFUSES while a cleared job exists (see it).
-- NULL TRAP (CLAUDE.md rule 13): every comparison on a nullable value is IS (NOT) DISTINCT FROM; the jsonb comparisons are over NOT NULL columns (fields, frozen_settings).
-- Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

-- ── 1. the rows: the merge data may be cleared to {} on an ended job, and nothing else may change with it ───────────────────────────────────────────────────────
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
        -- (platform_0021) THE RETENTION PURGE: the merge data of a row of an ENDED job may be cleared to exactly {}, and nothing else about the row may change with it
        IF NEW.fields IS DISTINCT FROM OLD.fields AND NEW.fields = '{}'::jsonb
           AND NEW.email_raw IS NOT DISTINCT FROM OLD.email_raw AND NEW.email_norm IS NOT DISTINCT FROM OLD.email_norm AND NEW.state IS NOT DISTINCT FROM OLD.state
           AND NEW.reason IS NOT DISTINCT FROM OLD.reason AND NEW.duplicate_of IS NOT DISTINCT FROM OLD.duplicate_of AND NEW.source_item IS NOT DISTINCT FROM OLD.source_item
           AND NEW.suppressed IS NOT DISTINCT FROM OLD.suppressed AND NEW.submitted_at IS NOT DISTINCT FROM OLD.submitted_at THEN
            SELECT j.status INTO v_status FROM platform.jobs j WHERE j.id = v_job;
            IF v_status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN
                RETURN NEW;
            END IF;
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
COMMENT ON FUNCTION platform.job_rows_frozen() IS 'SECURITY INVOKER: a row of a job changes only while the job is draft or previewed (23001 job_rows_frozen), the flag suppressed included; submitted_at, the engine''s stamp, is the one column that moves later, once. A pending row flagged suppressed is a verify job''s (23514 job_rows_pending_flag_is_verify_only). Since platform_0021 the merge data (fields) of a row of an ended job may also be cleared to exactly {}, with no other column changed (the content retention).';

-- ── 2. the job: the template may be cleared to its placeholders on an ended job, and nothing else of the frozen columns may change with it ──────────────────────
CREATE OR REPLACE FUNCTION platform.jobs_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.module_id IS DISTINCT FROM OLD.module_id OR NEW.source_job_id IS DISTINCT FROM OLD.source_job_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'platform.jobs: user_id, module_id, source_job_id and created_at never change' USING ERRCODE = '23001';
    END IF;
    IF OLD.status NOT IN ('draft', 'previewed') AND (NEW.frozen_settings IS DISTINCT FROM OLD.frozen_settings OR NEW.preview IS DISTINCT FROM OLD.preview
       OR NEW.preview_fingerprint IS DISTINCT FROM OLD.preview_fingerprint OR NEW.confirm_key IS DISTINCT FROM OLD.confirm_key)
       -- (platform_0021) THE RETENTION PURGE: unless the job has ended and the frozen document becomes exactly the old one with its template cleared (subject and HTML '[removed]', text empty) and nothing else changed
       AND NOT (OLD.status IN ('succeeded', 'failed', 'cancelled', 'expired')
                AND NEW.preview IS NOT DISTINCT FROM OLD.preview AND NEW.preview_fingerprint IS NOT DISTINCT FROM OLD.preview_fingerprint AND NEW.confirm_key IS NOT DISTINCT FROM OLD.confirm_key
                AND NEW.frozen_settings IS NOT DISTINCT FROM jsonb_set(jsonb_set(jsonb_set(OLD.frozen_settings, '{content,subject}', '"[removed]"'::jsonb), '{content,html}', '"[removed]"'::jsonb), '{content,text}', '""'::jsonb)) THEN
        RAISE EXCEPTION 'platform.jobs: frozen_settings, the preview and the confirm key are frozen once the job has left previewed (it was % )', OLD.status USING ERRCODE = '23001';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_guard() IS 'SECURITY INVOKER: user_id, module_id, source_job_id and created_at never change; frozen_settings, the preview, its fingerprint and the confirm key are frozen once the job has left previewed (23001), except that an ended job''s campaign template may be cleared to its placeholders (the content retention, platform_0021): content.subject and content.html "[removed]", content.text empty, nothing else changed; updated_at is the guard''s.';

-- ── 3. the mark now covers every ended job: a job marked under platform_0018 is looked at once more ───────────────────────────────────────────────────────────────
DROP INDEX platform.jobs_content_retention_idx;
CREATE INDEX jobs_content_retention_idx ON platform.jobs (finished_at)
    WHERE content_cleared_at IS NULL AND status IN ('succeeded', 'failed', 'cancelled', 'expired');
ALTER TABLE platform.jobs DISABLE TRIGGER jobs_content_cleared_once;
UPDATE platform.jobs SET content_cleared_at = NULL WHERE content_cleared_at IS NOT NULL;
ALTER TABLE platform.jobs ENABLE TRIGGER jobs_content_cleared_once;
COMMENT ON COLUMN platform.jobs.content_cleared_at IS 'Phase 5 S8 (extended by platform_0021): when the job store finished clearing what the platform keeps of the job''s content: for a send job the rendered messages in public.job_items (subject and body [removed], text and link NULL), for every ended job the merge data of its rows (job_rows.fields = {}) and the campaign template in frozen_settings (subject and html [removed], text empty), the last page and this mark in one transaction. NULL: not cleared. Only an ended job has it, never earlier than its finished_at, and it is set once. The addresses, the results, the events, the charges and the counts are never touched.';

INSERT INTO platform.schema_migrations (version) VALUES ('0021');
