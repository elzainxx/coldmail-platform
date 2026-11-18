# Module 3's adapter against the real module 3: what to look for in the first full run

Phase 3, chunks B9 to B12 for module 3. Nothing here has run: there is no scoped run for module 3 (it is part of the ordinary full harness), so the first proof is a full run (about 25 minutes before these tests; about
8 minutes more with them, see the last section). Read this table beside the `---- module 3's adapter: the test's own NOTE lines ----` block of stage 6 and the sub-stage `9b-adapter` of the run log.

The tests: `tests/harness/m3_adapter_e2e_test.go` (five), `m3_adapter_e2e_outage_test.go` (three), helpers and oracle `m3_adapter_e2e_helpers_test.go`. All build tag `harness` only, all in the full binary: the floor
`tests/floors-harness.txt` rises by **8** (the three outage phases count in the ordinary run too: without `HARNESS_M3_OUTAGE=1` each runs its paired controls and passes; the sub-stage then requires their LIVE PASS
lines, see below). The stage script is `tests/harness/m3adapter/outage.sh` (sourced by `run.sh`, called once after stage 9b), its selftest `stage-selftest.sh` (33 cases, run by the `gates` workflow).

## Where each proof is

| Chunk | Test | Takes (estimate) |
|---|---|---|
| B9 | `TestM3Adapter_EndToEndAccountsRunReportClassify`: accounts through the adapter, quote, pre-check, a campaign of 12 (sent, bounced, deferred then sent) and a tenant whose items fail, report, Classify, the books | 1 to 1.5 min |
| B9 | `TestM3Adapter_SuspensionRoundTripAndTheUnexplainedConflict`: the platform's real suspension path, what the adapter reads back, the 409 and what the adapter says of it, the resume | about 40 s |
| B10 | `TestM3Adapter_PaginationAcrossPageBoundaries`: 45 items at seven page sizes, two look-ahead walkers | about 45 s |
| B10 | `TestM3Adapter_AccountListPaginationAcrossABoundary`: 203 accounts so the list has two pages | 1 to 2 min (the uncertain one: 203 verifications through three workers) |
| B11 | `TestM3Adapter_CrossTenantReadsAreRefused`: B asks for A's rows; the admin route refuses the service token | about 45 s |
| B12 | `TestM3Adapter_OutagePhase1Start`, `...Phase2Down`, `...Phase3Resume`, run live by the sub-stage `9b-adapter` | about 3 min with the stop and the start |

## The NOTE lines, and what each must say

Every number below is what the code of module 3 (research `Docs/history/p3-research/m3-api.md`, read at the pinned commit) says it must be. A line that disagrees is a finding about the module, a wrong guess of the
research, or a bug here: say which, with the line.

| NOTE line (prefix `NOTE m3a`) | What it shows | Expected | If it differs |
|---|---|---|---|
| `dial retry_backoff_schedule: ... -> {"v":{"ladder_seconds":[2,2,2],...}}` | the retry ladder is shortened for the run and restored (module 3 reads it fresh at every outcome) | the seeded 30/120/600 s ladder replaced, then put back (no restore error at the end) | a deferred item that never retries means module 3 caches the dial: the test then fails at `the sink never relented` |
| `accounts F: created while the sink refused AUTH: verification failed code verify_auth_failed` | a failed verification is a successful call; the account exists | failed / verify_auth_failed, alignment aligned | another code (`verify_protocol_error`, `verify_connection_lost`) is the module's closed set too: the test names it; adjust the expectation, it is not a bug of the adapter |
| `entitlement A: enabled (ledger row N), delivered by the service: ... want_version=1 converged_version=1 observed=active observed_version=1 m3_version=0` | the platform's real path delivered the enable | converged at version 1, `m3_version=0` (module 3 has never suspended this user) | `m3_version=-` would mean the read-back did not record the counter |
| `test button: ... verification verified / alignment aligned; with AUTH refused failed code verify_auth_failed; accepted again verified; GetAccount verified verified` | the module's test button against the sink, its failure path, and that the account verifies again (it must, or the campaign cannot send) | as written | a `GetAccount` that stays failed: the button does not re-verify; the campaign then times out with every item queued |
| `bulk add: 4 rows in ~8s: row 1 created, row 2 duplicate_account_for_user ..., row 3 validation_failed (field issues [...]), row 4 created` | the closed code set | two created, a duplicate, a validation refusal | the rejected rows' `code` outside the set is `unrecognised` (module-forced M3-F4): the test names it |
| `quote: 12 recipients: feasible true, reasons 0, cost 0 (unit 0, ..), caps queue 100000 / daily send N / accounts 20, capacity eligible 3 ...; with no account: feasible false reasons [{no_eligible_accounts ...}]` | the feasibility block, read; the caps equal module 3's own dials (read from `send_byo.config`) | eligible 3; queue cap and account cap equal the dials | a queue cap different from `max_queued_jobs` means a per-user override: look at `send_byo.user_config_overrides` |
| `pre-check: truthful facts ... -> nothing, not checked []; each false fact names its cause` | `PreCheckRun` with the platform's facts (campaign cap `m3aCampaignCap` = 5000, the platform's own) | nothing | |
| `run: A 12 new jobs (202 queued, class pending) ..., F 2 ...` | every item a new job | 202 each | a 409 here is `ErrConflictUnexplained`: the test fails naming it |
| `relent: each deferred item has been retried (attempt_count >= 2 ...)` | the deferred-then-sent path | once, a few seconds after the run | missing: the deferred items never reached attempt_count 2 |
| `report: A total=12 sent=10 bounced=2 ... ; F total=2 failed=2` | the campaign report read to the end | exactly that | `failed` items in A mean account exhaustion hit A: look at A's accounts |
| `item NN kind email: reported state/error_class/smtp_response ... by account xxxxxxxx; sink RCPT accepted=.. rejected=.. deferred=.. messages=..; charge.Decide free "" rule M3-01, hand oracle free; Classify settle=true` | one row per recipient: the report, the sink's counters, both oracles | ok: sent/`<null>`/`<null>`, accepted=1 messages=1; bounce: bounced/bounce_permanent/a text naming 550, rejected=1 accepted=0 messages=0; defer: sent/`<null>`/`<null>`, deferred >= 2 accepted=1 messages=1; fail (F): failed/`<null>`/`<null>`, RCPT all 0 | the table keyed by recipient is the first thing to read |
| `classify: A settle 12 (all free by M3-01, no unit), not yet 0, unmapped 0; F settle 2 ...; the ledger of A holds 1 row (its m3_enable), F's 0; balances 0/0, reserves 0/0, monetary rows 0/0; money.Settle on a module 3 job: ErrNotMetered` | module 3 is unmetered | as written | any other ledger row: a charge crept in |
| `replay: a second Run of item 01 answered the same job (200, not created, sent) ...` | the module's idempotency | 200, same job, the sink unchanged | |
| `controls: the comparison functions reported 18 of 18 corruptions` | each comparison fails on its corrupted copy, for its own reason | 18 of 18 | a `control: ... accepted a corrupted input` error names the comparison that cannot fail |
| `suspension before: adapter {suspended false version 0 suspended_at <nil>}; module 3's own row exists false; platform m3_sync ... m3_version=0` | the three readers agree on a never-suspended user | version 0 | |
| `suspension: disabled through the store (ledger row N); the service delivered it: ... observed=suspended ... m3_version=1; the adapter reads suspended=true version=1 at <instant>; module 3's own table: suspended=true at <the same instant>` | the adapter's read equals the module's own table and what the platform recorded | the same instant (to the microsecond), version 1, `observed_version=2` | |
| `conflict: PreCheckRun names m3: the user's module 3 entitlement is not active before the call; Run of a new item answered kind refused HTTP 409 code "conflict" ...: ExplainConflict keeps it ErrConflictUnexplained ... module 3 created no job; the replay of the first item is still a 200` | the platform names the cause before; the module does not | as written | the module naming its cause in the 409 (a `details` entry) would be an additive module change: record it, the adapter must still not guess |
| `resume: re-enabled through the store ...; the adapter reads suspended=false version=2 reinstated ...` | the resume | version 2, reinstated after suspended | |
| `resume run: the refused Run is accepted (202) after the resume; both items sent exactly once; S's ledger holds 3 entitlement rows and nothing else` | the same call now succeeds | | |
| `page size  NNN: P page(s) of [...], boundaries after item(s) [...]; (id, state, error class, provider message id) of all 45 items equal the baseline, in order` (seven lines) | B10 | 45, 7, 15, 23, 44, 45, 100 give 45, 7, 3, 2, 2, 1, 1 pages; sizes 15 and 45 end exactly at a boundary: no empty page after | a missing item at one size only is the look-ahead defect of the module: the control shows what it looks like |
| `controls: at page size 7 the look-ahead walker delivered 39 of 45 items and the skip-ahead walker 24: the same equality check reported 6 and 21 missing` | the pagination controls bite | 39 / 24 / 6 / 21 | |
| `accounts: 203 accounts created through the adapter in ...` and `account list: 203 accounts in 2 page(s) of [200 3]` | B10 for accounts | two pages, 200 and 3; the look-ahead walker delivers 202 and the check reports 1 missing | if `account_count_cap_per_user` cannot be raised or creation is slower than 4 minutes, drop this test first |
| `cross-tenant (B asks for A): campaign report total=0 items=0 complete=false pages=1; ReportJob of A's 4 items -> not found (job_item_id); ReportJobByID -> not found (job_id); Run -> not found (job_item_id), nothing created; GetAccount and the test button -> not found; events -> empty page; B's list holds only B's account; N answers scanned ...: none` | B11, research 3.4 | as written | a not found that names another field (or none) is a wrong guess of the research |
| `cross-tenant control (A asks for A): ... the leak scan finds N of A's ids in A's own answers` | the leak scan can fail | N >= 4 | |
| `credentials: the Domain A service token on the admin suspension read and on the suspend route -> 401 ...` | B cannot read or change a suspension through the user-facing credential | 401 `unauthenticated`, A stays unsuspended | |
| `outage phaseN controls: K fake inputs were each reported ...` (three lines, in the ordinary run too) | every judge of the outage proof fails on a fake | K = 28 in all three | |
| `outage phaseN control-only: HARNESS_M3_OUTAGE=1 is set only by the outage sub-stage` (ordinary run) | the three phases pass without doing anything live | present three times in stage 6 | missing: the guard is not what it says |
| `outage phase1 start: tenant ..., entitlement delivered (...), account ... verified at the sink; 24 items run through the adapter in ...; the sink holds every send 6000 ms; 3 lanes` | phase 1's setup | | |
| `outage phase1 live: N s after the run the adapter's report says total=24 sent=3 ... in_flight=3 pending=18, is_complete=false; the sink holds 3 send(s) in flight ...; 18+ item(s) are not terminal, about 36 s of work ...` | still running, proved twice | sent >= 3, is_complete false, a held send, at least 30 s of work left | the test fails with the judge's sentence if the campaign could end before the stage stops the module |
| `outage phase2 down: a TCP connection to module 3 from the test container fails: ...` | the module is really gone | `the name no longer resolves` (Docker removes a stopped container's name) or refused | a connection that succeeds: the stage did not stop it (the stage checks this too) |
| `outage phase2 calls: 17 calls made while the module is down, each typed unavailable or timeout with no value: ReportCampaign -> unavailable (dns lookup failed (no such host); request sent false); ...` | the typed errors | every call unavailable (or timeout), `request sent false` for the DNS failures | `contract`, `internal` or `refused` would be a wrong answer from something else answering on that name |
| `outage phase2 polling: a loop that polls the report and loops on the error made R round(s) and stopped by its 3 s context` | polling halts | R about 12 | |
| `outage phase2 live: the platform's books are unchanged from phase 1 (...); module 3's own tables still hold 24 job(s) (...) and none for the probe item; the sink holds M message(s) to the campaign (S at phase 1 ...) with no duplicate; the platform's /readyz is 200 ...` | nothing settled, invented or created | books equal; M >= S (sends in flight finish at the sink while the Control is down: the module's own background work, noted, not asserted); readyz 200 | a retry counter that rose (`attempts a -> b`) is the platform retrying, by design |
| `outage phase3 t+..s: a transient unavailable while the module recovers` | recovery | zero to a few lines (the stage already waited for `/readyz`) | a non-transient error after the restart fails the test with its kind |
| `outage phase3 live: ... every one of the 24 items is reported once as sent, has the one job phase 1 was given, has ONE message at the sink and a row in module 3's send log (24 rows, 24 distinct jobs; 0 extra RCPT acceptance(s) ...); 24 repeat Run calls were the module's replay ...` | exactly once | 24/24/24, extra acceptances 0 | extra acceptances > 0 with one message each: a session was cut before its final reply and retried: noted, not a duplicate; two messages for one recipient fail the test |
| `outage compare: phase 1 sampled ...; phase 2 saw the module's own jobs as "..." and M message(s) at the sink; phase 3 ended with 24 message(s)` | the three phases side by side | 24 at the end | |

The sub-stage's own evidence lines: `m3 is stopped: its container is not running, its health endpoint no longer answers on the private network, and the same probe does reach the controllable sink (the control)`,
`m3 is back: /readyz answers ready over the private network after N s of waiting`, `module 3's three workers are running and each heartbeated after <time>` (twice), `outage phase N (...): PASS (live)` (three
times), `floor: 3 TestM3Adapter_Outage tests declared, each run live and passed`, `M3A OK in N s`.

## What could make "the campaign is certainly in flight" racy

- The stage stops the module a few seconds after phase 1 returns (the test container exits, then `docker compose stop m3`; a graceful stop may take seconds). Phase 1 therefore does not sample at the first sign of
  progress: it waits until at least three items are terminal, the campaign is not complete and the sink holds a send in flight, and then requires about 36 s of work to remain (24 items, three lanes, a 6 s hold):
  more than the stage needs by a wide margin. A very slow runner that finishes the first three items late only makes the remaining work shorter in wall-clock terms after the sample, not before it.
- A lane's pause between two sends (0 to 1 s) and the dispatch tick (1 s) leave a worker idle for a moment: the sample waits for a held send at the sink at that instant, and the test does not assert that a send is
  in flight at the instant the module stops (it cannot know it); it asserts what the contract guarantees.
- The hold is the sink's 6 s per message, far under module 3's 30 s SMTP operation timeout and far over any reaction time.

## What could not be elicited, and what the first run may reveal

- The module 3 error cases the contracts list but this harness cannot produce: the From-gate paths (WARN, BLOCK, indeterminate: the gate is off in the harness, run.sh stage 5b), `rate_limited` (429) on the test
  button (the limits are 5 per account and 20 per user per day and the tests stay under them on purpose), a slow create that leaves the account `unverified` or answers 500 with the row created (no sink mode delays
  AUTH), a junk cursor that answers 500, a bulk add that times out, a `paused` rung (`503`) and the account routes that are not covered by the adapter (patch, delete, the lifecycle verbs, test-send).
- Module 3 has no cancel: nothing here cancels a campaign (open decision 1 of the spec).
- `failed` is produced by account exhaustion only (a tenant whose only account failed verification): module 3's other `failed` cause, an enforced campaign expiry, is off (`campaign_expiry_mode` is `shadow`).
- A sink that drops a connection, throttles or greylists as a real provider does is not available: the sink rejects (550), defers (451), holds the final reply and refuses AUTH, and that is what is used.
- Assumptions of the research that the first run settles (each is asserted, so a wrong one fails with a sentence naming it): the 404 `details` of Run and of the two single-job reports name `job_item_id` and `job_id`;
  a refused credential check is `verify_auth_failed`; a sent item's `error_class` and `smtp_response` are null in the campaign report; a failed item that never reached a worker has no account and no error class;
  `bulk_add` rejects a duplicate as `duplicate_account_for_user` and a malformed address as `validation_failed`; module 3 writes nothing to `public.job_items`; the events route answers an empty page for another
  tenant's account; the Control's boot rescan and the workers' outcome buffer give exactly one message per item across a restart.

## Time

Ordinary tests (stage 6): about 4 to 5 minutes (B9 1.5, suspension 0.7, pagination 0.8, account list 1 to 2, cross-tenant 0.8, the three outage phases' controls 3 s each). The sub-stage `9b-adapter`: about 3 minutes
(workers 0.5, phase 1 0.5, stop 0.3, phase 2 0.3, start 0.5, workers 0.5, phase 3 1). That is more than the 4 minutes asked for: the account-list test is the one to drop first, and the pagination test shortens
by lowering the item count (it needs more than 44 to put a boundary one item short of the end at the largest size below 45).
