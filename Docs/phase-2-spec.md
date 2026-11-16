# Phase 2 — Phase 1 rulings, then the Money core: spec

Roadmap and locked decisions: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md) (not reopened here — a contradiction with module code is **flagged**, not fixed). Previous phase: [`phase-1-spec.md`](phase-1-spec.md) (its "Locked here" section 1–12 stands; each change below is a forward migration and is recorded in "Amendments to the Locked section"). False-green classes every gate below must avoid: [`../deferrals-and-false-greens.md`](../deferrals-and-false-greens.md).

**Status: in progress** (Part A first; Part B starts only when Part A is green on the runner). Evidence rule: only a GitHub-runner run counts; a Windows-host run is rehearsal (Docker is down on the owner's machine; freshly built executables stall under the workstation's antivirus, so Go tests are first run by the runner).

## How the phase is run

- Spec first (this file), committed before code; chunks of 1–2 hours, each with an exit check; Sonnet subagents for narrow chunks (tests written blind are first run by the `db-tests` workflow, ~5 minutes, which is the fast loop); my decisions go in "Open decisions" and work continues.
- **Batched fault proofs** (rule 11): no `fault/*` branches. Each new or changed gate gets `ci/faults/p2/<gate>.patch` + `.expect` (command, step, message). One `fault-proofs` dispatch after Part A and one after Part B; run URLs are recorded below.
- Review gates run sequentially, one concern each: (1) migrations and constraints, (2) store code and idempotency, (3) concurrency, (4) M3 entitlements, (5) harness and docs. Each is a pass over the diff looking for that one concern only.
- Part A and Part B each end with every workflow green on the head and the parent re-pinned.

## Part A — the Phase 1 rulings (decided by the owner; implemented here)

| # | Ruling | What is built | Exit evidence (runner) |
|---|---|---|---|
| 1 | **Supabase setup (OD-7)** is a required install step; the main harness moves onto the Supabase image; no change to M2/M3 | `tests/supabase/preprovision.sql` becomes `db/install/preprovision.sql` (a product install file, not a test file); `Docs/install.md` names it as step 2; `harness` and `db-tests` run on the digest-pinned `supabase/postgres` with the migrators connecting as the non-superuser `postgres`, the preprovision step run first exactly as the install doc says; the `supabase` workflow keeps its BASELINE leg (the evidence the step is needed) | `harness` + `db-tests` green on the Supabase image; the install doc's commands are the commands CI runs (`scripts/check-install-doc.sh`) |
| 2 | **A scheduled job holds the one-active-job slot (OD-2)**, in the database | `platform_0004`: the partial unique index covers `scheduled`, `running`, `paused`, `cancelling` | test: a second job for the same user and module is refused (23505, the index named) while one is scheduled, allowed for the other module; fault patch |
| 3 | **Clawback (OD-4)**: ledger kind `adjustment` | `platform_0004`: kind `adjustment` (admin only, reason required, removes only `available`, never `reserved`, never below zero by the existing CHECK), its row-shape CHECK, an `admin_audit` row written by trigger, covered by the balance trigger and the drift gate | constraint tests with paired controls; the drift gate is zero after an adjustment; fault patch |
| 4 | **A disabled admin is refused by the database (OD-10)** in the ledger, the audit log and entitlements (and `unit_costs`) | `platform_0004`: `platform.require_active_admin` and a trigger per admin-naming column | the Phase 1 test that pinned "a disabled admin can still be named" is rewritten to pin the refusal; Locked #12 amended |
| 5 | Vendor price stays seeded at 2; documented as an admin setting | `Docs/install.md` ("Settings an admin owns") | doc gate |
| 6 | **First admin**: a documented setup command; admin management stays in P7 | `platform-migrate create-first-admin --auth-user-id … --name …` (refuses when any admin exists; creates the `public.users` row and the `platform.admins` row in one transaction, as the privileged identity) | harness test runs the real binary: creates, refuses a second, refuses an unknown auth user |
| 7 | **Data API (OD-6)**: no RLS; keep the access removals | parent `OWNER-ACTIONS.md` line "In Supabase, remove `public` (and `platform`) from the exposed API schemas" | present in the parent at the re-pin |
| 8 | **M1 readiness (OD-8)** stays all-or-nothing; document why | `Docs/install.md` and M1's operator docs: the platform always submits bulk jobs, so a deployment without `TM_QUEUE_ADDR_KEY` can never serve the platform | doc gate |
| 9 | **Single-label domains (OD-11)** are accepted; document it | `Docs/install.md`, Locked #4 note; a test pins `a@b` accepted and `a@b..c` refused | db-tests |
| 10 | **M1 migration tracker**: exactly once, in order; twice is a no-op; fresh = upgraded; 0016's refusal guard kept | M1 `0017_schema_migrations.sql` (`verify.schema_migrations`) and a migration runner in the M1 repo that applies each file once in order under an advisory lock, records `(version, sha256)`, refuses a changed or out-of-order history, and adopts a database that already ran the old psql loop; the platform's `migrate-m1` uses it | M1 `ci`: fresh == upgraded (catalog fingerprint) from both an empty and a 0015-era database, the chain twice is a no-op, a tampered file is refused; the harness migrates M1 with it |
| 11 | **M3 comments**: every comment or doc saying M3 is read-only on `public.job_items` is fixed | M3 comment/doc edits only | M3 `ci` green; a grep gate in the platform repo's `check-module-reads` equivalent is not needed (M3 is pinned, never patched from here) |
| 12 | **The system user (all-zero uuid) can hold no balance, ledger row, entitlement or suppression** and is excluded from every user list | `platform_0004`: a CHECK on each of the four tables; view `platform.listable_users` (the only sanctioned way to list users); `scripts/check-user-lists.sh` fails when platform Go code or SQL selects `public.users` outside the listed exceptions | tests per table with paired controls; the gate selftest; fault patches |

## Part B — Money core

### Charge rules (one pure function)

`internal/charge`: `Decide(module, outcome) → Decision{Kind: Charge | Free | NotFinal | Unmapped, Unit, Reason}`. The table is **derived from module code** (three read-only reports, kept in `Docs/history/p2-research/`) and committed as data (`internal/charge/rules.tsv`, embedded) with one row per outcome; a test compares the Go table with the file, and `scripts/check-charge-table.sh` compares this section with the file, so the prose cannot drift from the code. **An outcome that does not map cleanly is `Unmapped` — flagged here and returned as an error, never guessed.** The table below is filled in chunk B1 (it needs the module reports).

*(filled in B1)*

### Design (what is built and why)

1. **One transaction per operation** (grant, reserve, settle, release, adjustment); the store takes an interface so the job engine (P4) can run a reserve inside its own confirm transaction.
2. **All money writers of one (user, module) serialise on one advisory lock** in the platform band, `7_000_000_000 + hash(user, module) mod 10^9`, taken first in every money transaction. The balance trigger and `CHECK (available >= 0 AND reserved >= 0)` stay as the second wall; the store's lock is what gives callers a clean `InsufficientCredits{shortfall}` instead of a CHECK error. (The roles cannot `SELECT … FOR UPDATE` the balance row — they hold SELECT only, by design — so the advisory lock is the serialiser.)
3. **Reserve** (`platform_0005`): in one transaction, with the job row locked, the module's unit prices are copied into `platform.job_unit_prices` by one `INSERT … SELECT` while `FOR SHARE` locks on the module's `unit_costs` rows hold off a concurrent price change until commit, so *the snapshot equals the table at that instant* is a fact and not a race; the reserve row freezes the **dearest** snapshot price × units. The database refuses a reserve whose rate is not the dearest snapshot price, a second reserve for one job, and a reserve with an incomplete snapshot.
4. **Settle** is idempotent **per job item by a database key**: charge rows carry `item_ref` (the module's own item id) and `UNIQUE (job_id, item_ref) WHERE kind = 'charge'`, independent of the idempotency key the code derives. A charge is exactly one unit (`units = 1`). A charge consumes the reserve **it names**, capped per reserve by `platform.reserves` (written only by the ledger trigger, `CHECK (charged + released <= reserved)`) — the pooled balance cannot hide one job spending another's reservation. Items whose outcome is `Free` write nothing; `NotFinal` items stay unsettled.
5. **Release** returns exactly the remainder `reserved − charged − released`, once (`UNIQUE (reserve_id) WHERE kind = 'release'`, amount checked by trigger against `platform.reserves`), and only for a job that has ended; zero remainder writes no row.
6. **Adjustment** removes only `available`, names an admin and a reason, and writes `admin_audit` by trigger.
7. **M3 entitlements**: `platform.m3_entitlements` is written **only** by a trigger on the ledger kinds `m3_enable` / `m3_disable` / `m3_expire` (same single-writer pattern as the balance, with an `m3_entitlement_drift` view in the drift gate); `m3_expire` is refused by the database unless the entitlement is enabled and its `enabled_until` has passed. Delivery to M3 is separate bookkeeping, `platform.m3_sync`: the entitlement trigger raises `want_version`; **only a read-back of M3's state that matches the wanted state may set `converged_version`**, so the platform never reports "suspended" it did not confirm. Two platform loops run in `cmd/platform`: **expiry** (due entitlements → `m3_expire`) and **reconcile** (suspend/resume through M3's real admin route, then read back; retry with backoff until converged; periodically re-verify converged rows).
8. **Time is the database's**: expiry compares `enabled_until` with the database's `now()`, never an application clock.

### Concurrency proof (exit criterion)

A forced interleave, not hope: a test connection holds the (user, module) advisory lock; two real store reserves are started in separate sessions; the test proves **both backends are blocked** on it (`pg_stat_activity.wait_event_type = 'Lock'`, `pg_blocking_pids`), then releases it; exactly one reserve succeeds, the other returns `InsufficientCredits`, the balance is never negative, `balance_drift` is empty. The paired control runs the same interleave with both protections removed (a raw check-then-act and no `CHECK`) and **must overdraw**; the fault patch removes the lock and the CHECK from the real code and the test goes red naming `OVERDRAW`.

## Chunks (each ≈1–2 h; every chunk has an exit check, all on the runner)

### Part A

| # | Chunk | Exit check |
|---|---|---|
| A0 | **Records** — this spec; status table (P2 in progress); research reports kept as records | `platform-check` green |
| A1 | **`platform_0004`, part 1** — active-slot index incl. `scheduled`; system-user CHECKs; `listable_users`; `check-user-lists.sh` + selftest | `db-tests` green; tests with paired controls |
| A2 | **`platform_0004`, part 2** — `adjustment` kind (`monetary` regenerated, views rebuilt, shape CHECK, audit trigger); disabled-admin triggers; the Phase 1 test that pinned the old behaviour rewritten | `db-tests` green; drift zero after an adjustment |
| A3 | **First-admin command** + install doc + OWNER-ACTIONS line + the doc gate | harness test of the real binary; `gates` green |
| A4 | **M1 migration tracker** (M1 repo: `0017`, the runner, its tests) | M1 `ci` green: fresh == upgraded, twice == no-op, tamper refused |
| A5 | **M3 comments** (M3 repo) | M3 `ci` green |
| A6 | **Harness and `db-tests` on the Supabase image**; preprovision moved to `db/install/`; `migrate-m1` uses the tracker runner | `harness`, `db-tests`, `supabase` green |
| A7 | **Part A fault dispatch** — patches for the slot index, adjustment, disabled admin, system user, the user-list gate, the M1 tracker, the install-doc gate | one `fault-proofs` URL, every leg green |
| A8 | **Part A close** — status; parent re-pinned; every workflow green | recorded below |

### Part B

| # | Chunk | Exit check |
|---|---|---|
| B1 | **Charge table** from module code; `internal/charge`; `rules.tsv`; `check-charge-table.sh` | every row of the table has a test; unmapped outcomes returned as errors |
| B2 | **`platform_0005`** — `item_ref`, one-unit charges, `platform.reserves` + trigger, one reserve per job, release exactness, reserve-rate and snapshot-completeness triggers | constraint tests with paired controls |
| B3 | **Store: grant, adjustment, reserve** (snapshot, lock, errors, idempotency) | tests incl. *snapshot equals the table at that instant* and *settles at the frozen cost after `unit_costs` changes* |
| B4 | **Store: settle and release** | settle twice charges once; release returns the remainder once; full cycle leaves zero drift |
| B5 | **Concurrency proof** — forced interleave with control | green; fault patch red naming `OVERDRAW` |
| B6 | **M3 entitlements** — `platform_0006` (ledger `until_at`, trigger-written `m3_entitlements`, `m3_sync`, drift view), store functions | constraint and store tests |
| B7 | **M3 admin client + loops** wired into `cmd/platform` (config dials with consumers) | unit tests; harness reaches M3's real suspend route |
| B8 | **Harness: M3 worker, SMTP sink** — real sends; **halt and resume** | nothing after the halt is sent; nothing in flight is lost; resume completes |
| B9 | **Harness: M3 down at expiry** — stop M3, expire, the loop retries, start M3, convergence | converged only after a read-back; no false success while M3 was down |
| B10 | **Part B fault dispatch + close** — patches for the new gates; status P2 done; parent re-pinned | fault-proofs URL; every workflow green on the head |

## Amendments to the Locked section (Phase 1 items 1–12)

*(filled as each lands)*

## Open decisions (yours)

*(filled as they arise)*

## Findings

*(filled as they arise)*

## Evidence

*(filled as chunks close)*
