# Phase 2 close-out: every open item of Phase 2, closed before Phase 3

Previous: [`phase-2-spec.md`](phase-2-spec.md) (its charge table, module gaps M1-G1…G7 / M2-G1…G5, open decisions 1–15 and findings F28–F37 are what this closes). Roadmap: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md). False-green classes every gate below must avoid: [`../deferrals-and-false-greens.md`](../deferrals-and-false-greens.md).

**Status: in progress, and BLOCKED on an owner action since 2026-11-17 ~00:07 UTC: GitHub Actions refuses to start any job for the account ("recent account payments have failed or your spending limit needs to be increased", Settings → Billing and plans).** Evidence rule: only a GitHub-runner run counts. **Do not start Phase 3.** Nothing may stay open at the end except owner actions (non-code) and items the owner marked PARKED (none so far).

**Where it stands when the runners come back (do these in order):**
1. Re-run a platform workflow to see that runs start again (`gh run rerun <id>`). Heads pushed and never run: platform `8fbae10` (the run at its parent `8ddf638` was refused too), send-managed `4ea078b`. Heads that ARE green: verify `e32c1b9` ([ci](https://github.com/elzainxx/coldmail-verify/actions/runs/43984668683), [gitleaks attempt 1](https://github.com/elzainxx/coldmail-verify/actions/runs/43984668658/attempts/1)), send-byo `238bd0a` ([real-infra](https://github.com/elzainxx/coldmail-send-byo/actions/runs/43982469095)), platform `31e85c4` (all eight workflows, [harness](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564587): D3 D4 D6 D7 D8 D9 E1 E2).
2. Fix whatever the first run of platform `8fbae10` finds (D1 charge table, D2 live M1 bulk settlement, D5 migration `platform_0008` and reconciler were verified only by unit tests, vet and the gates here; the migration, `TestEntObserved_*`, the live D2 test and the 0008 lifecycle never ran).
3. send-managed: get `4ea078b` green (two known fixes in it: UTC-hour-23 skips, the lock-probe text); then pin it in `tools/modules.lock`.
4. Dispatch `fault-proofs` once for the new/changed legs only (`only=` the names under `ci/faults/p2c/` plus the retargeted `p2/m3-converged`, `p2/charge-rules`, `p2/charge-table`); the module legs need the lock to name the module commit the patch applies to (m1-*: `e32c1b9`, m2-*: `4ea078b`, m3-*: `238bd0a`).
5. Re-pin the parent (`coldmail` gitlinks = the lock; `bash tools/check-modules-lock.sh <parent>` must say OK), close E3 (Phase 2 spec decisions CLOSED with ruling, commit, run URL; build plan status table) and print E4.
Rows below say `IN PROGRESS` where the code is written and pushed but the runner evidence (or the fault proof) is still missing; nothing is `CLOSED` without a run URL.

## How to resume from this file alone

1. Read the table below: the first row whose status is not `CLOSED` is where to continue; a row marked `IN PROGRESS` names the repo, commit and run it was last seen at in its "Evidence" cell.
2. Parts run in order **A (module 1) → B (module 2) → C (module 3) → D (platform) → E (coverage and docs)**; a module's CI must be green before the platform consumes it, and the parent is re-pinned at the end of each part (`tools/modules.lock` + the parent's gitlinks, `bash tools/check-modules-lock.sh <parent>` must say OK).
3. Module fault proofs are platform patches with `TARGET=module:<repo>` run by the `fault-proofs` workflow (`ci/faults/p2c/`, phase directory `p2c`): the lock must name the module commit the patch applies to. One batched dispatch per part for the new or changed gates only.
4. After every chunk: update the row (status, repo@commit, run URL), commit, push.

## Rulings carried into this session (the owner's; nothing here reopens them)

- **No GitHub deletions, no history rewrites.** Module API changes are **additive only** (no renames, no removed fields); M1's five doors and the M2/M3 state enums stay frozen except the additions ruled below. **No real vendor calls in CI**, no real ZeroBounce key anywhere.
- **Unknown rule (verbatim, goes into the build plan's Money section):** *"An unknown caused by the recipient's server is charged; one caused by us, or that can't be proven either way, is free."*
- **M1 dedup (A2):** identical addresses inside one job are checked once and every input row gets that result (charged once by the platform); **across jobs and tenants there is no dedup** (the cache may answer; a cache answer is charged as a cache answer); forward migration.
- **M1 vendor (A6):** wire ZeroBounce behind M1's existing external-stage design, **ships disabled**, anti-laundering (a vendor "valid" is `risky/external_verified` at capped confidence, never `valid`), spend-once by the existing compare-and-set, global kill switch; a vendor non-answer is not a vendor-charged outcome.
- **M2 (B3/B4):** attempt cap (max attempts and max age) ends in `failed` / `attempts_exhausted`, not charged; `POST /v1/cancel` makes every not-yet-claimed item the new terminal state `cancelled`.
- **M3 (C2–C4):** a suspension read route; suspension gates everything that touches SMTP; the halt semantics ("no new claims; at most `concurrency_per_user` in-flight sends finish") are accepted as they are.
- **Gitleaks (D7):** the narrow exception stays (no history rewrite); the harness's SMTP-sink password is generated at run time, so no literal remains in the tree.

## The items

Status values: `OPEN`, `IN PROGRESS`, `CLOSED`. "Evidence" = repo@commit and the run URL that closes it.

### Part A — Module 1 (`coldmail-verify`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| A1 | **Per-item identity on bulk (M1-G1).** Bulk submit takes a caller-supplied `item_id` per row (additive `items: [{item_id, address}]` beside `addresses`); every bulk result echoes `item_id` and the row's `ordinal`; no plaintext address in a result (the hashing door stays) | every result attributes to exactly one input row; a pagination test across page boundaries finds no gap and no duplicate | IN PROGRESS | verify@e32c1b9: [ci](https://github.com/elzainxx/coldmail-verify/actions/runs/43984668683) + [gitleaks attempt 1](https://github.com/elzainxx/coldmail-verify/actions/runs/43984668658/attempts/1) green on the runner (bulk items, ordinal listing, exact-once pagination across page sizes, LOG row per malformed row); platform@8fbae10 pins it and the live D2 test is written; fault patch (`p2c/m1-*`) and the live test await a runner |
| A2 | **Dedup scope (M1-G2).** Per-job dedup only: identical addresses in one job are checked once, every input row gets the result; none across jobs or tenants (the cache may answer, charged as a cache answer); the global UNIQUE becomes per job, by a forward migration; the code comment's intent ("still-pending identical check") is made true | tenant B gets a result for an address tenant A verified yesterday; one duplicate inside a job = one check and N result rows; a paired control shows the old behaviour fails the test | IN PROGRESS | verify@e32c1b9 (same runs as A1): migration 0018 (`verify.job_rows`, per-job idempotency), tenant-B-gets-results test with the old global scope as the paired control green on the runner; fault patch awaits dispatch |
| A3 | **Failure origin (M1-G4).** `signals.failure_origin = own \| remote \| undetermined` on every `unknown` (additive); `smtp_blocked` is always own; greylisted with retries exhausted is remote; DNS: our resolver unreachable/timeout is own, the domain's authoritative servers failing is remote; timeouts, errors, refused connections by who is at fault; anything not provable is `undetermined`. The full mapping is a table in M1's docs with file:line per branch; cases that cannot be classified honestly are flagged, not forced | a test per origin and per reason, through M1's real layers with the DNS and SMTP fixtures already in CI | IN PROGRESS | verify@e32c1b9 (same runs): `signals.failure_origin` on every unknown, table with file:line in `Docs/failure-origin.md`, a test per origin and per reason through the real DNS and SMTP layers; fault patches await dispatch |
| A4 | **SMTP unwired at boot (M1-G6)**: a skip token (`smtp_unwired`) tells it from a genuine cheap-layers answer | SMTP unwired shows the token; the wired control shows none | IN PROGRESS | verify@e32c1b9 (same runs): `smtp_unwired` skip token, not cached; wired control shows none; fault patch awaits dispatch |
| A5 | **Errors that never settle (M1-G7)**: decrypt and compute errors get an attempt cap (a config dial, documented default); at the cap the item settles `unknown` with `failure_origin = own` | a forced repeated error reaches a final state at the cap | IN PROGRESS | verify@e32c1b9 (same runs): `TM_QUEUE_ERROR_MAX_ATTEMPTS` (default 5), a forced repeated error settles `unknown/unknown_error` origin own at the cap; fault patch awaits dispatch |
| A6 | **ZeroBounce vendor fallback.** `buildExternal` as a ZeroBounce client behind the existing design: residue only, anti-laundering, trust "no" over "yes", spend-once CAS, global kill switch, **ships disabled**; every ZeroBounce status/sub-status mapped to M1's enum from ZeroBounce's published API docs (cited, a table in M1's docs, unmappable statuses flagged); a vendor non-answer is not a vendor outcome (M1's own result stands); dials: API key REFERENCE (an env var name), daily spend cap, timeout, enabled (default off); CI: a digest-pinned stand-in ZeroBounce HTTP server returning each documented status plus errors, timeouts, rate limits, malformed bodies | tests for every mapping row, the kill switch, the spend cap, anti-laundering and "disabled sends nothing", each with a paired control | IN PROGRESS | verify@e32c1b9 (same runs): ZeroBounce client behind the existing design, ships disabled, digest-pinned stand-in server in CI, mapping in `Docs/vendor-zerobounce.md` (from the official SDKs: the owner verifies it against the live docs before enabling: OWNER-ACTIONS §3), key canary in the no-secret-leak gate; fault patches await dispatch |
| A7 | **Same-bug sweep:** M1's bulk pagination vs the M2-G5 page-boundary skip | result reported either way, with a test if a bug exists | IN PROGRESS | verify@e32c1b9: M1's bulk listing had no page-boundary skip; the new ordinal listing is pinned exact-once at several page sizes with paired controls (green on the runner, same runs) |
| A-end | Part A close: M1 CI green, the platform consumes it (pin), batched fault dispatch for Part A, parent re-pinned | `check-modules-lock` OK; dispatch URL recorded | IN PROGRESS | verify@e32c1b9 green and pinned (platform lock, 8ddf638+); ten M1 fault patches `p2c/m1-*` (platform@ae786d2) rehearsed here control-green/fault-red; the Part A `fault-proofs` dispatch and the parent re-pin wait for a runner (GitHub Actions refused: payments/spending limit) |

### Part B — Module 2 (`coldmail-send-managed`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| B1 | **Pagination fix (M2-G5)**: the campaign report skips one item per page boundary | a multi-page test with exact item-set equality and a paired control | IN PROGRESS | send-managed@4ea078b (pushed; **no runner run exists for it**: GitHub Actions refused the account, payments/spending limit); the ancestor 7740aa8 ran: [gitleaks](https://github.com/elzainxx/coldmail-send-managed/actions/runs/43981010150) green, [real-infra](https://github.com/elzainxx/coldmail-send-managed/actions/runs/43981010125) red only for the UTC-hour-23 skips and one lock-probe text, both fixed at 4ea078b; B1 also fixed three more second-truncated cursors (B5). Fault patch `p2c/m2-pagination` awaits dispatch |
| B2 | **Reason / error class on report items (M2-G1)**, additive | the platform can tell why an item ended; tests per class | IN PROGRESS | send-managed@4ea078b: `reason` and `error_class` on every report item (additive), vocabulary published in `/v1/describe`; runner run of 4ea078b pending (see B1) |
| B3 | **Attempt cap (M2-G2)**: dials for max attempts and max age (documented defaults); at the cap `failed` / `attempts_exhausted` (not charged; pool exhaustion keeps its own reason); every assertion that assumed "only pool exhaustion writes failed" updated | a forced permanent failure reaches `failed` at the cap | IN PROGRESS | send-managed@4ea078b: dials `job_max_failed_attempts` (6) and `job_failed_max_age_sec` (86400), `failed`/`attempts_exhausted`, not charged; runner run pending; fault patch `p2c/m2-attempt-cap` |
| B4 | **Cancel (M2-G3)**: `POST /v1/cancel` for a campaign: every not-yet-claimed item becomes the additive terminal state `cancelled` (an event row per transition); in-flight items finish and report truthfully; idempotent; tenant-scoped | after cancel nothing new is claimed or sent (against the sink); in-flight items reported exactly once; a second cancel is a no-op | IN PROGRESS | send-managed@4ea078b: `POST /v1/cancel`, terminal `cancelled`, SUITE 11 (cancel) green at ancestor 7740aa8 ([real-infra 43981010125](https://github.com/elzainxx/coldmail-send-managed/actions/runs/43981010125)); runner run of 4ea078b pending; fault patch `p2c/m2-cancel` |
| B5 | **Same-bug sweep:** other page-boundary or cursor bugs in M2's report and admin listings | reported either way | IN PROGRESS | send-managed@4ea078b: sweep table in the M2 agent's report: campaign report (fixed), admin accounts / pools / account events (second-truncated cursor, fixed), config and health unpaged (no change); runner run pending |
| B-end | Part B close: M2 CI green, pin, fault dispatch, parent re-pinned | | OPEN | BLOCKED: M2 CI has not run on 4ea078b (GitHub Actions refused: payments/spending limit); the platform lock still pins send-managed 02a71fc until it is green |

### Part C — Module 3 (`coldmail-send-byo`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| C1 | **Pagination sweep** (M3 was forked from M2): the report's page-boundary skip | fixed with the same test if present | IN PROGRESS | send-byo@238bd0a: [real-infra 43982469095](https://github.com/elzainxx/coldmail-send-byo/actions/runs/43982469095) + [gitleaks attempt 1](https://github.com/elzainxx/coldmail-send-byo/actions/runs/43982469176/attempts/1) green: five of the six cursored listings had the defect (campaign report skip, four second-precision cursors), all fixed with paired controls; fault patch `p2c/m3-pagination` awaits dispatch |
| C2 | **Suspension read route**: an admin route returning a user's suspended flag, `suspended_at` and a version/marker | the platform's reconcile loop reads it back | IN PROGRESS | send-byo@238bd0a (same runs): `GET /v1/admin/users/{id}/suspension` + `version` (migration 0054); the platform reconciler reads it (D5, platform@8fbae10, live test pending a runner); fault patch `p2c/m3-suspension-read` |
| C3 | **Suspension gates everything that touches SMTP**: test-send, verify-on-add and the account test button are refused while suspended, with a user-facing error that names no internal mechanics | each refused while suspended and allowed after reinstatement, paired controls | IN PROGRESS | send-byo@238bd0a (same runs): test-send, account test button, account create and bulk-add refused while suspended (409, no internals named), allowed after reinstatement; fault patch `p2c/m3-suspension-gate` |
| C4 | **Document the halt semantics** (accepted as is) | docs state it | IN PROGRESS | send-byo@238bd0a: halt semantics documented in api-contract, operator-runbook, install-and-deploy (runs above) |
| C-end | Part C close: M3 CI green, pin, fault dispatch, parent re-pinned | | IN PROGRESS | send-byo pinned in the platform lock (8ddf638); the platform harness on that pin has not run (GitHub Actions refused at 8ddf638); parent re-pin and the Part C fault dispatch pending a runner |

### Part D — Platform

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| D1 | **Charge table update** (rows listed in the table below), `check-charge-table` byte-equal; the build plan's Money section states the unknown rule verbatim | a test per new/changed row | IN PROGRESS | platform@8fbae10: `origin` column, the owner's unknown rule verbatim in the build plan, gate + selftest (62 cases), unit tests per row; unit suite green locally, **runner pending**; fault patches `p2/charge-rules`, `p2/charge-table` (regenerated) and `p2c/charge-origin` |
| D2 | **Per-item settlement for M1**: the echoed `item_id` is `item_ref`; the "settle only when the whole job is complete" workaround removed | a real M1 bulk job in the harness settles item by item and matches the module's report exactly | IN PROGRESS | platform@8fbae10: `internal/m1results` (29 unit tests, green locally) and `TestM1Live_BulkJobSettlesItemByItem`; the live test needs a runner (module 1 e32c1b9 is pinned) |
| D3 | **Snapshot prices enforced by the database (OD-10)**: a trigger compares every `job_unit_prices` row with `unit_costs` at insert; the tests that fabricate prices updated | fault patch | IN PROGRESS | platform@31e85c4: migration `platform_0007` trigger + tests green on the runner ([db-tests](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564600), [harness](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564587)); fault patch `p2c/snapshot-equals-table` awaits the Part D dispatch |
| D4 | **Terminal job states are final (OD-11)**: a transition trigger | a test per terminal state with a paired control | IN PROGRESS | platform@31e85c4: migration `platform_0007` trigger + a test per terminal state green on the runner (same runs as D3); fault patch `p2c/terminal-final` awaits the Part D dispatch |
| D5 | **Convergence freshness by the database (OD-12)**: `observed_version`, the CHECK needs `observed_version = want_version` and `observed = wanted`; the reconciler reads M3's new read route (C2) | the stale-observation case is refused by the database | IN PROGRESS | platform@8fbae10: `platform_0008`, `m3_sync_converged_only_when_observed_at_version`, reconciler reads M3's suspension route, `m3admin.Suspension`; unit tests green locally, the migration and the nine `TestEntObserved_*` + live test **never run: runner pending**; fault patch `p2c/observed-version`, `p2/m3-converged` retargeted |
| D6 | **First admin (OD-8)**: an existing inactive `public.users` row is refused with a clear message | tests updated | IN PROGRESS | platform@31e85c4: `ErrUserInactive` + tests green on the runner (db-tests, harness); fault patch `p2c/first-admin-inactive` awaits the Part D dispatch |
| D7 | **Gitleaks (OD-15)**: the exception stays; the harness's sink password is generated at run time | a fresh gitleaks run on the head finds nothing outside the allowlisted commits | IN PROGRESS | platform@31e85c4: sink password minted per run (`run.sh`, masked); [gitleaks](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564498) green with the new no-history head-tree scan (a fresh scan of the head finds nothing outside the allowlisted commits); the fault proof for `gitleaks-config` is the existing one |
| D8 | **Lock waits**: a config dial for the advisory-lock wait (documented default) | a held lock times out at the dial with a typed error | IN PROGRESS | platform@31e85c4: `PLATFORM_MONEY_LOCK_TIMEOUT_MS` (default 10 s), typed `ErrLockTimeout`, tests green on the runner; fault patch `p2c/money-lock-timeout` awaits the Part D dispatch |
| D9 | **One loop runner**: the expiry and reconcile loops run only in the instance holding a leader advisory lock; a second instance idles and takes over when the leader dies | a harness test with two platform instances: exactly one acts; the second takes over after the first is killed | IN PROGRESS | platform@31e85c4: leader lock + `TestLeader_*` + harness stage 9c (two instances, leader killed, takeover) green on the runner ([harness](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564587)); fault patch `p2c/loops-leader` awaits the Part D dispatch |
| D10 | **Owner actions** (parent `OWNER-ACTIONS.md`): ZeroBounce account, API key in the env var named by the dial, a signed DPA, the daily spend cap, then the vendor dial on; all existing items kept | present in the re-pinned parent | IN PROGRESS | parent `OWNER-ACTIONS.md` rewritten (ZeroBounce account, key in the env var the dial names, DPA, daily spend cap, mapping verification against the live docs, vendor unit price, then switch the dials on; the GitHub Actions billing block); parent commit pending the re-pin |

### Part E — Coverage and docs

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| E1 | **Forced interleaves still missing**: settle vs release, settle vs adjustment, on one (user, module) | each with a paired control and a fault patch | IN PROGRESS | platform@31e85c4: settle-vs-release and settle-vs-adjustment forced interleaves with paired controls green on the runner (harness); fault patches `p2c/interleave-settle-release`, `p2c/interleave-settle-adjust` await the Part D dispatch |
| E2 | **M3 coverage**: several lanes and a sink that rejects or delays; halt and resume stay lossless, rejected items reported truthfully, charge decisions follow the table | harness run | IN PROGRESS | platform@31e85c4: `TestM3Live_SeveralLanesAndAHostileSink` (three workers, four lanes, a sink that rejects 550, defers 451 and delays; halt and resume lossless; the table's Free for M3) green on the runner ([harness](https://github.com/elzainxx/coldmail-platform/actions/runs/43981564587)); no fault patch: the test's own paired controls are the proof |
| E3 | **Docs never stale**: the Phase 2 spec closes every open decision as CLOSED (ruling, commit, run URL); the build plan status table and Money section current; M1/M2/M3 docs describe the new fields, routes, dials; `install.md` has the vendor dials | doc gates green | IN PROGRESS | platform@ae786d2+: Phase 2 spec decisions 1-15 and findings F35-F37 closed with ruling, closing item and evidence (runner URLs where they exist, 'runner pending' elsewhere); the build plan's Money section states the unknown rule verbatim and its P2 row names the close-out; `Docs/install.md` has a section with module 1's vendor dials (`TM_FALLBACK_MODE`, `TM_EXTERNAL_DPA_CONFIRMED`, `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV`, `TM_EXTERNAL_DAILY_SPEND_CAP`, timeout, base URL); module docs updated by the module agents (M1 bulk-item-identity, failure-origin, vendor-zerobounce and the stale vendor docs, M2 ADR-0016, M3 api-contract / runbook); the final run URLs wait for a runner |
| E4 | **"Not checked" list** reprinted at the end; anything left is a later phase's exit criterion (named) or an owner action | list in the report | IN PROGRESS | the list is written (platform `Docs/phase-2-spec.md`, 'What this phase did NOT check': every entry a later phase's named exit criterion or an owner action; two Phase 8 exit criteria added to the build plan) and reprinted in the final report; CLOSED when a runner has run the gates on the commit that holds it (the `check-verified-heads` gate refuses a CLOSED row without a head and a run) |

## Charge-table changes (D1), decided here and implemented in D1

| Outcome | Decision |
|---|---|
| M1 `unknown` + `failure_origin = remote` | **charge** `m1_own` |
| M1 `unknown` + `failure_origin = own` | free |
| M1 `unknown` + `failure_origin = undetermined` | free |
| M1 `unknown` with no `failure_origin` (an older module) | unmapped (never charged, reported) |
| M1 `unknown/greylisted` | follows its origin (remote once retries are exhausted → charged) |
| M1 skip token `smtp_unwired` | free |
| M1 attempt-cap settlement (`unknown`, origin own) | free |
| M1 ZeroBounce answers (`risky\|invalid` / `external_verified`, not cached) | `m1_vendor` per M1's mapping; a vendor non-answer is no vendor charge |
| M2 `failed` / `attempts_exhausted` | free |
| M2 `cancelled` | free |

## Open decisions of the Phase 2 spec and how each is closed here

| Phase 2 decision | Closed by |
|---|---|
| 1 M1 unknown outcomes mixing our failure with a genuine answer | A3 + D1 (ruled: charge remote) |
| 2 M1 bulk results carry no per-item id | A1 + D2 |
| 3 M1 dedup global and permanent | A2 |
| 4 `m1_vendor` unreachable | A6 + D1 (vendor ships disabled; the owner switches it on after D10) |
| 5 M2 uncapped retries and no cancel | B3 + B4 + D1 |
| 6 M3 suspend not instant / no read route | C2 + C3 + C4 + D5 |
| 7 `m3_entitlements` UPDATE vs a disabled admin | moot since Phase 2 (entitlements are ledger-written); recorded CLOSED in E3 |
| 8 first admin with an inactive user | D6 |
| 9 Supabase-side items | owner actions (D10 / install doc) |
| 10 snapshot content only store-enforced | D3 |
| 11 terminal job states not final in the database | D4 |
| 12 convergence wall "observed matches" | D5 |
| 13 a scheduled job holds the slot but nothing moves a job there | Phase 4's confirm transaction (`ReserveIn`): a later phase's exit criterion, named in E4 |
| 14 positive correction is a grant | a ruling-free design note; recorded CLOSED in E3 |
| 15 the gitleaks exception vs a history rewrite | D7 (ruled: keep) |
| Findings F35 (lock waits), F36 (loops race), F37 | D8, D9, D1 |
