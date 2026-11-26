-- platform_0015_engine_schedule.up.sql — Phase 4 close-out, items D8 and D9. Three ANNOTATIONS of platform.jobs that only the job engine writes (the lifecycle guard of platform_0011 lets an UPDATE that leaves status
-- alone through, as it does for last_progress_at, stale_alerted_at and module_job_ref: they carry no lifecycle fact), and the one rule the database keeps about them.
--
--   next_poll_at      timestamptz   when the engine may look at this job again (D8). NULL means "due now". The engine's work query returns the jobs that are DUE by the database's clock and orders them oldest-due-first
--                                   (COALESCE(next_poll_at, confirmed_at)), so a job that makes no progress (its module is down) cannot keep the front of the queue for ever: after each step the engine writes the outcome
--                                   (platform.jobs.next_poll_at = now() after progress, now() + an exponential backoff after none), and 100 stuck jobs no longer starve a newer one.
--   idle_polls        integer       how many steps in a row made no progress (0 after progress). The exponent of the backoff, kept in the row so that a restarted engine, or the next leader, resumes it.
--   items_ensured_at  timestamptz   when the module-facing rows of a SEND job (public.job_items, one per pending row) were all written (D9): once per job, in one transaction, so a tick of a job whose module is down
--                                   writes nothing and a restarted engine does not write it again. Set once and never moved.
--
-- WHAT THE DATABASE KEEPS (trigger platform.jobs_engine_schedule, BEFORE UPDATE, every writer passes it):
--   1. A CHANGE OF STATUS WAKES THE JOB: next_poll_at becomes NULL and idle_polls 0, whoever moved it (the user's cancel, an administrator, the engine's own move). A cancelled running job does not wait out a backoff.
--   2. A BACKOFF CANNOT HIDE A JOB: next_poll_at may not be more than 600 seconds ahead (23514, constraint jobs_next_poll_within_the_cap). The engine's own cap is a dial (PLATFORM_JOB_BACKOFF_CAP_SEC, 1 to 600, 60 by
--      default) and is never more than a quarter of the module's staleness limit; this is the wall behind it: no value of the dial and no bug can push the next look past ten minutes, so a stale job is still noticed.
--   3. items_ensured_at is set once (23514, jobs_items_ensured_is_set_once): the rows of a job are frozen at its confirm, so what was written once is complete for good.
--
-- WHAT THIS DOES NOT DO. It does not decide what the engine does with a job that is due (that is internal/jobengine), and it does not touch public.job_items: the terminal marker of D9 (status 'cancelled' on a
-- never-submitted item of a job that ended) is written by internal/jobs in the transaction that ends the job, and the platform adds NO trigger, policy or row-level security to the two public tables (a test pins that: nothing the
-- platform does may change what modules 2 and 3 can do with them). Only the vocabulary is documented, in the column comment below.
--
-- NULL TRAP (CLAUDE.md rule 13): idle_polls is NOT NULL; the trigger compares with IS DISTINCT FROM / IS NOT NULL. Created AS platform_owner (see platform_0002).

SET LOCAL ROLE platform_owner;

ALTER TABLE platform.jobs ADD COLUMN next_poll_at     timestamptz;
ALTER TABLE platform.jobs ADD COLUMN idle_polls       integer NOT NULL DEFAULT 0;
ALTER TABLE platform.jobs ADD COLUMN items_ensured_at timestamptz;
ALTER TABLE platform.jobs ADD CONSTRAINT jobs_idle_polls_not_negative CHECK (idle_polls >= 0);

COMMENT ON COLUMN platform.jobs.next_poll_at IS 'Phase 4 D8: when the engine may look at the job again; NULL is due now. Written by the engine after each step (now() after progress, now() plus an exponential backoff after none) and cleared by jobs_engine_schedule on every change of status. Never more than 600 seconds ahead.';
COMMENT ON COLUMN platform.jobs.idle_polls IS 'Phase 4 D8: how many steps in a row of the engine made no progress on this job (0 after progress or a change of status): the exponent of the backoff.';
COMMENT ON COLUMN platform.jobs.items_ensured_at IS 'Phase 4 D9: when every pending row of this send job (modules 2 and 3) had its public.job_items row written (one transaction per job, set once). NULL: not yet, or not a send job.';
COMMENT ON COLUMN public.job_items.status IS 'pending (what module 3 writes and the default) or cancelled: the platform''s terminal marker on an item that was NEVER submitted to its module and whose job expired, was cancelled or failed (written by internal/jobs in the transaction that ends the job; never moved back). NO module reads this column (their only statements on the table are the run route''s SELECT of campaign_id and its INSERT ... SELECT into their own jobs table, both by id and user, and module 3''s test-send INSERT): the marker is the platform''s record, not a lock.';

CREATE FUNCTION platform.jobs_engine_schedule() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        -- a move of any kind wakes the job: the user's cancel of a running job, an administrator's move, the engine's own
        NEW.next_poll_at := NULL;
        NEW.idle_polls := 0;
    ELSIF NEW.next_poll_at IS NOT NULL AND NEW.next_poll_at IS DISTINCT FROM OLD.next_poll_at AND NEW.next_poll_at > now() + interval '600 seconds' THEN
        RAISE EXCEPTION 'platform.jobs: job % cannot be left alone until % (the engine looks at a job again within 600 seconds at the latest)', OLD.id, NEW.next_poll_at
            USING ERRCODE = '23514', CONSTRAINT = 'jobs_next_poll_within_the_cap';
    END IF;
    IF OLD.items_ensured_at IS NOT NULL AND NEW.items_ensured_at IS DISTINCT FROM OLD.items_ensured_at THEN
        RAISE EXCEPTION 'platform.jobs: job % had its module-facing rows written at % and that is set once', OLD.id, OLD.items_ensured_at
            USING ERRCODE = '23514', CONSTRAINT = 'jobs_items_ensured_is_set_once';
    END IF;
    RETURN NEW;
END
$fn$;
COMMENT ON FUNCTION platform.jobs_engine_schedule() IS 'SECURITY INVOKER: a change of status clears next_poll_at and idle_polls (the job is due now); next_poll_at is at most 600 seconds ahead (23514 jobs_next_poll_within_the_cap); items_ensured_at is set once (23514 jobs_items_ensured_is_set_once).';
REVOKE ALL ON FUNCTION platform.jobs_engine_schedule() FROM PUBLIC;
CREATE TRIGGER jobs_engine_schedule BEFORE UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_engine_schedule();

INSERT INTO platform.schema_migrations (version) VALUES ('0015');
