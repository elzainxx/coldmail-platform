# tests/supabase — every migration, run as the non-superuser `postgres` of a Supabase image

**Purpose.** The platform will run on Supabase, where the migrating role `postgres` is *not* a superuser (the real superuser is `supabase_admin`). The main harness runs stock Postgres with a superuser, so any migration that needs one (`CREATE ROLE` with attributes, `ALTER ... OWNER TO` a role the migrator cannot `SET ROLE` to, `CREATE SCHEMA ... AUTHORIZATION`, extensions, `ALTER DEFAULT PRIVILEGES FOR ROLE`, `GRANT ... WITH ADMIN`) has never been exercised. This job runs **every** migration — the platform's and all three modules' — as that non-superuser against the digest-pinned `supabase/postgres` image of [`supabase.yml`](../../.github/workflows/supabase.yml), and **reports every statement that fails**. It is a reporting instrument first: it changes no migration.

| File | What |
|---|---|
| [`run.sh`](run.sh) | the job: instrument check, pre-flight, the four chains, the table of failures, the comparison with the known list, and the list of what it did not check |
| [`classify.sh`](classify.sh) | the text half: psql's failure output to records, the BLOCKER / CASCADE classification, the comparison. No database needed |
| [`classify-selftest.sh`](classify-selftest.sh) | the paired control for `classify.sh`: recorded-shape psql output and every way the comparison can go red, each reject case asserting its reason |
| [`known-blockers.txt`](known-blockers.txt) | the committed list of real blockers. The job is green only when the observed blockers are exactly this set |

## How to read a run

1. **Stage a, instrument.** The migrating role must be `(rolsuper, rolcreaterole) = (f, t)`; a superuser is red ("not Supabase-shaped"). The facts printed there (version, whether `auth.users` exists, the `supabase*` / `anon` / `authenticated` / `service_role` roles, the memberships of `postgres`) describe the image. A **control file** of three statements with a known outcome is then run through the real psql, and the classifier must read exactly that; if it cannot, the run says `INSTRUMENT INVALID` and nothing after it is trusted.
2. **Stage b, pre-flight.** Module 2's own fixture creates `public.users` / `public.job_items` (reported, never a blocker); each module's `check_platform_tables.sql` must then pass.
3. **Stage c, chains.** Each migration file runs on its own with `psql -b -v ON_ERROR_STOP=0 -v VERBOSITY=verbose -f FILE`, one statement per autocommit, so one failure does not hide the next. The table lists every failure: chain, `file:line`, SQLSTATE, class, the first 160 characters of the statement, and the message. A chain with no files is red.
   - **BLOCKER**: SQLSTATE `42501`, or a message such as `must be superuser`, `permission denied to ...`, `must be able to SET ROLE`, `only roles with ...`, `is a reserved role`. These are the findings.
   - **CASCADE**: everything else (`42P01` relation does not exist, `3F000` schema does not exist, `42710` duplicate ...). Listed, never judged: they usually follow an earlier blocker and disappear when it is fixed. **Read them anyway**: a non-privilege failure that is a real incompatibility (a missing extension) also lands here.
4. **Stage d, compare.** `NEW BLOCKER` (observed, not listed) and `STALE ENTRY` (listed, no longer observed) are both red, as are malformed and duplicate lines in the known file. The run ends by printing the observed blockers as ready-to-paste lines, and the list of what it did **not** check (the pooler, Auth/GoTrue, PostgREST and RLS defaults, hosted-service extensions, a real project's role grants, module runtime behaviour, and that psql ran statements in autocommit rather than in the migrators' per-file transaction).

`last-report.tsv` (eight tab-separated columns: chain, file, line, SQLSTATE, class, message, statement, detail; the last three escaped so a record is one line) is written under the work directory, `S_WORK` or a temp directory, never into the repository; its path is printed. The raw psql output of every file is kept beside it in `raw/`.

## Updating known-blockers.txt

One line per blocker: `FILE|SQLSTATE|MESSAGE-SUBSTRING` (file = basename; `#` comments and blank lines are ignored). The first run on the runner is expected to be red with every blocker as `NEW BLOCKER`; the lead copies the printed lines into the file. After that:

- **Module 2 / module 3 / module 1 migrations are not changed from this repository.** An entry for one of them is added **only with the owner's ruling** that the failure is accepted or is being fixed in that module's own repository; when that module is re-pinned and the statement no longer fails, the job goes red with a `STALE ENTRY` and the line is removed.
- **A `platform_*` migration may be changed.** Fix the migration and remove its entry in the same commit; the job is red until both are done.
- Never list a CASCADE; never loosen a substring to cover two different failures.

## What this decides

The goal state is an **empty** list: nothing in the platform's or the modules' migrations needs a privilege that Supabase's `postgres` lacks. Only if the list is empty **and** the main harness's tests pass on this image may the harness itself move onto it (replacing stock Postgres with a superuser). That is a later decision; this job does not make it, and a green run here is not that evidence on its own: it says the migrations apply, not that the services run.
