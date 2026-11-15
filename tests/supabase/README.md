# tests/supabase — every migration, run as the non-superuser `postgres` of a Supabase image

**Purpose.** The platform will run on Supabase, where the migrating role `postgres` is *not* a superuser (the real superuser is `supabase_admin`). The main harness runs stock Postgres with a superuser, so any migration that needs one (`CREATE ROLE` with attributes, `ALTER ... OWNER TO` a role the migrator cannot `SET ROLE` to, `CREATE SCHEMA ... AUTHORIZATION`, extensions, `ALTER DEFAULT PRIVILEGES FOR ROLE`, `GRANT ... WITH ADMIN`) has never been exercised. This job runs **every** migration — the platform's and all three modules' — as that non-superuser against the digest-pinned `supabase/postgres` image of [`supabase.yml`](../../.github/workflows/supabase.yml), and **reports every statement that fails**. It is a reporting instrument first: it changes no migration.

| File | What |
|---|---|
| [`run.sh`](run.sh) | one pass: instrument check, pre-flight, optional provisioning, the four chains, the table of failures, the comparison with a known list, and the list of what it did not check |
| [`classify.sh`](classify.sh) | the text half: psql's failure output to records, the BLOCKER / CASCADE classification, the comparison. No database needed |
| [`classify-selftest.sh`](classify-selftest.sh) | the paired control: recorded-shape psql output, every way the comparison can go red, and `run.sh` itself driven with a fake psql (options, provisioning failure path, the two-pass design of the workflow); each reject case asserts its reason |
| [`preprovision.sql`](preprovision.sql) | the proposed no-module-change fix (see below) |
| [`baseline-blockers.txt`](baseline-blockers.txt) | the blockers expected WITHOUT the fix (pass BASELINE) |
| [`known-blockers.txt`](known-blockers.txt) | the blockers expected WITH the fix (pass FIXED); the goal is an empty list |

## Two passes, one job

The workflow runs `run.sh` twice against the **same cluster**, in two **different databases**, so that the effect of the fix is measured and not assumed:

| Pass | Database | Provisioning | Compared with |
|---|---|---|---|
| `BASELINE` | `postgres` | none | `baseline-blockers.txt` — what fails without the fix: the evidence that it is needed |
| `FIXED` | `fixed` (created by the step as `postgres`, which must hold `CREATEDB`; asserted) | `preprovision.sql`, applied with `ON_ERROR_STOP=1` before the chains | `known-blockers.txt` |

- Both passes run even if the first is red (`if: always()` on the FIXED step only), and the job fails if either failed.
- **Roles are cluster-wide**, so pass FIXED sees the roles pass BASELINE created. That is intended: `preprovision.sql` is idempotent and grants the migrator on roles that already exist, which is the state of a real project whose roles an earlier, failed migration created. Schemas, tables and the module fixtures (`public.users` / `public.job_items`) are per database, so each pass applies its own. `fixed` is created from `template1`: it does not carry the image's own `postgres`-database content (the `auth` schema and the like); stage a prints what is there.
- `run.sh` options: `--known FILE` (the list of this pass), `--preprovision FILE` (applied as `postgres` after the pre-flight and before the chains; if it fails the pass is RED and the chains are **not** run), `--label NAME` (printed in every banner and in the result line).

**Why the fix.** On PostgreSQL 16+ the creator of a role gets only `ADMIN OPTION` on it (no `SET`, no `INHERIT`), so on Supabase the migrator `postgres` cannot `CREATE SCHEMA ... AUTHORIZATION <owner>` (`must be able to SET ROLE`) or `ALTER DEFAULT PRIVILEGES FOR ROLE <owner>` (`permission denied to change default privileges`). The platform's own bootstrap and module 1's `0016` now grant the migrator their owner roles themselves. Modules 2 and 3 may not be changed from this repository, but their bootstraps create their owner role only if it is missing, so `preprovision.sql` — run once by the migrator before their chains — creates `send_managed_owner` and `send_byo_owner` (NOLOGIN) and grants them to the migrator `WITH INHERIT TRUE, SET TRUE`. It is a proposal: pass FIXED is what shows whether it is enough.

## How to read a run

1. **Stage a, instrument.** The migrating role must be `(rolsuper, rolcreaterole) = (f, t)`; a superuser is red ("not Supabase-shaped"). The facts printed there (version, whether `auth.users` exists, the `supabase*` / `anon` / `authenticated` / `service_role` roles, the memberships of `postgres`) describe the image. A **control file** of three statements with a known outcome is then run through the real psql, and the classifier must read exactly that; if it cannot, the run says `INSTRUMENT INVALID` and nothing after it is trusted.
2. **Stage b, pre-flight.** Module 2's own fixture creates `public.users` / `public.job_items` in the pass's database (reported, never a blocker); each module's `check_platform_tables.sql` must then pass.
3. **Stage p, provisioning.** `--preprovision FILE`, or "none". The migrator's memberships of the two module owner roles are printed afterwards.
4. **Stage c, chains.** Each migration file runs on its own with `psql -b -v ON_ERROR_STOP=0 -v VERBOSITY=verbose -f FILE`, one statement per autocommit, so one failure does not hide the next. The table lists every failure: chain, `file:line`, SQLSTATE, class, the first 160 characters of the statement, and the message. A chain with no files is red.
   - **BLOCKER**: SQLSTATE `42501`, or a message such as `must be superuser`, `permission denied to ...`, `must be able to SET ROLE`, `only roles with ...`, `is a reserved role`. These are the findings.
   - **CASCADE**: everything else (`42P01` relation does not exist, `3F000` schema does not exist, `42710` duplicate ...). Listed, never judged: they usually follow an earlier blocker and disappear when it is fixed. **Read them anyway**: a non-privilege failure that is a real incompatibility (a missing extension) also lands here.
5. **Stage d, compare.** `NEW BLOCKER` (observed, not listed) and `STALE ENTRY` (listed, no longer observed) are both red, as are malformed and duplicate lines in the known file. The run ends by printing the observed blockers as ready-to-paste lines, and the list of what it did **not** check (the pooler, Auth/GoTrue, PostgREST and RLS defaults, hosted-service extensions, a real project's role grants, module runtime behaviour, and that psql ran statements in autocommit rather than in the migrators' per-file transaction).

`last-report.tsv` (eight tab-separated columns: chain, file, line, SQLSTATE, class, message, statement, detail; the last three escaped so a record is one line) is written under the work directory, `S_WORK` or a temp directory, never into the repository; its path is printed. The workflow gives each pass its own directory. The raw psql output of every file is kept beside it in `raw/`.

## Updating the lists

One line per blocker: `FILE|SQLSTATE|MESSAGE-SUBSTRING` (file = basename; `#` comments and blank lines are ignored), in `baseline-blockers.txt` for pass BASELINE and `known-blockers.txt` for pass FIXED. The lead fills both from the first runner run: each pass prints its observed blockers as ready-to-paste lines. After that:

- **Module 1 / module 2 / module 3 migrations are not changed from this repository.** An entry for one of them is added **only with the owner's ruling** that the failure is accepted or is being fixed in that module's own repository; when that module is re-pinned and the statement no longer fails, the job goes red with a `STALE ENTRY` and the line is removed.
- **A `platform_*` migration may be changed.** Fix the migration and remove its entry in the same commit; the job is red until both are done.
- `preprovision.sql` is the platform's provisioning proposal, not a module file: changing it is changing what pass FIXED measures. If pass FIXED lists nothing, the proposal is sufficient for the pinned modules.
- Never list a CASCADE; never loosen a substring to cover two different failures.

## What this decides

The goal state is an **empty** `known-blockers.txt`: with the provisioning step, nothing in the platform's or the modules' migrations needs a privilege that Supabase's `postgres` lacks. `baseline-blockers.txt` says what the provisioning is for. Only if pass FIXED is empty **and** the main harness's tests pass on this image may the harness itself move onto it (replacing stock Postgres with a superuser). That is a later decision; this job does not make it, and a green run here is not that evidence on its own: it says the migrations apply, not that the services run.
