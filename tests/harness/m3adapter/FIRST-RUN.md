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

- The module 3 error cases the contracts list but this harness cannot produce: `rate_limited` (429) on the test
  button (the limits are 5 per account and 20 per user per day and the tests stay under them on purpose), a slow create that leaves the account `unverified` or answers 500 with the row created (no sink mode delays
  AUTH), a junk cursor that answers 500, a bulk add that times out, a `paused` rung (`503`) and the account routes that are not covered by the adapter (patch, delete, the lifecycle verbs, test-send).
- Module 3 had no cancel when this was written: nothing here cancels a campaign (open decision 1 of the spec). Since the Phase 3 close-out module 3 has `POST /v1/cancel` and `m3_adapter_closeout_cancel_test.go` drives it; this file stays the record of the first run.
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

## B6c: `TestM3Adapter_ContractEdgeCases` (the contract drift gate's edge cases)

`m3_adapter_e2e_edge_test.go`, one test, two tenants (`edge` for everything but the system event, `ev` for it), about 1 to 1.5 minutes; it brings the count of tests of the binary up by one (the floor rises by 1 for it; `TestM3Adapter_ZContractDrift` is the coordinator's). Every call
goes through a client whose options carry the drift session's observer (the adapters of `m3aEnv`, its module client `e.mc` for the raw calls the adapter never lets the platform ask for, an adapter built over a random
token), so each real answer is compared by `TestM3Adapter_ZContractDrift`, which runs after it (`zz_`). A step that finds another answer than the one it asked for reports the module's own answer (kind, HTTP status,
code, details, request id) and the next step still runs. The steps lower four dials of module 3 (`quote_long_duration_warn_sec`, `bulk_add_max_rows`, `account_count_cap_per_user`, `dispatch_mode`) for ONE call each and
put the original back at once (deferred, so a step that dies restores too); each logs `NOTE m3a dial <key>: <old> -> {"v":..} for one step`.

| NOTE line (prefix `NOTE m3a edge`) | Contract reached | Expected |
|---|---|---|
| `describe: module send_byo api v1, N capabilities, M vocabularies, K error codes` | `GET /v1/describe` ok | the module's self-description decodes |
| `quote: 100 recipients, threshold 60 s: feasible true, projection N s, warnings [{long_duration N 60}]` | `POST /v1/quote` feasible (a feasible quote WITH its warning: the module's default threshold is 172800 s, the hand-written fixture's warning needs a long projection, so the dial is lowered) | one long_duration warning; the control quote with the dial restored has none |
| `create validation_failed: HTTP 400 code validation_failed details [{from_address malformed}]` | `POST /v1/send-byo/accounts` error validation_failed | 400; the same account with a good address is created (control) |
| `create duplicate: HTTP 409 code conflict details [{smtp_username conflict}]` | `... error duplicate` | 409 with the detail naming smtp_username |
| `create conflict: HTTP 409 code conflict details [] at a cap of N accounts` | `... error conflict` (the account cap: the generic 409 with no detail) | no detail; with the cap restored the same create succeeds (control) |
| `list cursor_malformed`, `list validation_failed` | `GET /v1/send-byo/accounts` errors | 400 with details {cursor, malformed} and {user_id, required} |
| `list pages: limit=2 over 3 accounts: a page of 2 with a next cursor, then a last page of 1 with none` | `GET /v1/send-byo/accounts` page and last page | as written |
| `events: limit=1 over N event(s): a page of 1 with a next cursor, then a page of 1` | `GET /v1/send-byo/accounts/{id}/events` page | a next cursor on the first page |
| `test rate_limited: account_test_rate_limit_per_day 5; 4 click(s) accepted after the create, then HTTP 429 rate_limited` | `POST /v1/send-byo/accounts/{id}/test` error rate_limited | the create is one of the five; 4 clicks, then 429 (a count one off is noted, not asserted) |
| `bulk validation_failed: HTTP 400 ... details [{accounts required}]`, `bulk batch_too_large: HTTP 422 code unprocessable details [{accounts out_of_range}]` | `POST /v1/send-byo/accounts/bulk` errors | 400 and 422; the refused batch creates nothing; the same two rows with the dial restored are created (control) |
| `run unauthenticated: HTTP 401 code unauthenticated` | `POST /v1/run` error unauthenticated (an adapter over a random token) | 401, no job created |
| `run validation_failed: HTTP 400 ... details [{job_item_id required}]` (as the second record run showed) | `POST /v1/run` error validation_failed (an empty body, raw) | 400 |
| `run paused: HTTP 503 code paused ... (the module's Paused kind: wait, do not page)` | `POST /v1/run` error paused (`dispatch_mode` at `refuse_new` for the one call) | 503, no job created; the same Run with the rung restored is accepted and the item is sent (control) |
| `held queue: 5 items run in ...; after N s the report says total=5 ... pending=5: 1 item(s) tried (the sink answered 451), 4 not tried (the lane is held for 60 s)` | the held campaign: ONE lane, a 60 s pause after every push to a worker (module 3 stamps the lane when a job is pushed, whatever the outcome), the sink answering 451 | one tried, four not tried; if all five were tried the lane did not hold them and the last-page observation will have accounts |
| `job queued (tried): state queued attempt_count 1 error_class transport_4xx smtp_response "451 ..."; the account of the failed attempt is still attached: <8 hex>` | `GET /v1/report` job queued | the first record run showed the account stays attached to a requeued job (the research said omit-until-set): asserted only as both-or-neither and, when set, one of the tenant's own accounts; the id is printed |
| `job queued (not tried): state queued attempt_count 0, no error class, no provider text; account none` | `GET /v1/report` job queued, the other shape | attempt_count 0 and nothing else; the account is printed, not asserted |
| `campaign pages: 5 queued items at page size 2: pages of [2,2,1], the last with no next cursor; N of the 5 items carry an account` | `GET /v1/report` campaign page and last page | 3 pages; the LAST page holds a never-tried item with no account, which is what the hand-written last-page fixture shows (account_id and account_username null) |
| `campaign cursor_malformed: HTTP 400 code validation_failed details [{cursor malformed}]`, `campaign cursor_forged: HTTP 500 code internal_error` | `GET /v1/report` campaign errors | the 500 is confirmed by the second record run |
| `auth failure: the item is queued attempt_count 1 error_class auth_failure smtp_response "535 ..."` | (setup of the events step) a send whose AUTH the sink refuses | auth_failure |
| `events: N event(s) [user:created user:verified system:updated ...], K with flags before (a system row); the raw walk at limit=1 read N page(s) of 1 event, every one but the last with a next cursor` | `GET /v1/send-byo/accounts/{id}/events` page, the populated page with flags and a reason | N >= 3, K >= 1: at autoquarantine_mode=shadow module 3 records the quarantine it would have made as an `updated` event with flags before and after and a reason (`dispatch/account_effect.go`, writeShadowQuarantineEvent); if K is 0 the second NOTE line says so |

Allowlisted (`zz_m3_adapter_contract_drift_test.go`, each with its reason): `POST /v1/run` error internal_error, `POST /v1/send-byo/accounts` error unprocessable, `POST /v1/send-byo/accounts/{id}/test` unavailable.

Since Phase 4 A1 (this file is a first-run record and stays as it was written): the row `campaign cursor_forged: HTTP 500 code internal_error` is not what module 3 does any more. A forged cursor is now a 400 `validation_failed` with the detail `cursor/malformed`, the same answer as `cursor_malformed`, reported as `ErrBadCursor`; the contract `cursor_forged` was removed, and the adapter no longer maps a 500 to `ErrBadCursor`.

Matching pitfalls to read the drift report with (the coordinator's matcher, not changed here):
- `accounts.create.error.duplicate.json` was RECORDED from the generic 409 of a suspended user's create ("This action is not available for your account right now.", no details), not from a duplicate; this test now produces a
  real duplicate (409 with `details: [{smtp_username, conflict}]`), so expect `KeyAdded error.details` against that fixture until it is re-recorded. The generic 409s (the cap, a suspended user) fit `duplicate` and `conflict`
  equally and the matcher gives the second distinct one to `conflict`.
- `quote.ok.json` was recorded from a feasible quote with an empty `warnings`; the `feasible` contract's body is a feasible quote WITH a warning, which is what the quote step now produces.
- the two 400 contracts of the account list (`validation_failed`, `cursor_malformed`) have the same shape and tie: each of the two real answers lands on one of them.
- Second record run (44219152386), what it showed and what this edit does about it: every step of the edge test but one answered as the research said (the 500 for a forged cursor included); the one difference was
  that a queued item the sink answered 451 still carries the account of the failed attempt (the research said omit-until-set), which is now asserted as "both or neither, and one of the tenant's own accounts", with the
  account id printed. Still unexercised were `campaign last page` (the live last page had accounts; the hand-written fixture has none, so a populated page always fits `campaign page` better: the held campaign now has ONE
  lane held for 60 s, so only one item is tried and the last page holds a never-tried item with no account) and `events page` (the first-created and verified rows carry no flags before and no reason, so a populated page
  fits the `empty` contract's one-sided gaps as well as the page's: the new `ev` step makes module 3 write a system row with flags and a reason, so that the page's fixture has nothing the live page lacks).

## The From-gate ON, and the account events of an enforced health action (Phase 3 close-out D2 and D3)

Every `TestM3Adapter_*` test now runs with module 3's `from_gate_mode=enforce` and `from_dns_resolver=m3dns:53` (`newM3aEnv` writes both with `setDial` and the test's cleanup puts the originals back; the raw module 3 tests keep the gate off).
`m3dns` is the compose service built from `tests/harness/fx` with `FX_ROLES=dns,api` and `m3adapter/m3dns.json`; run.sh stage 5b starts it and waits for it, and the exposure check lists it. What to look for on the first run:

- `NOTE m3a fromgate ...` lines (from `m3_adapter_closeout_fromgate_test.go`): one per create, with the verdict, the DMARC policy and the credential check's state and code. An account at `smtp.fastmail.com` or `smtp.gmail.com` shows credential check `failed` with
  code `verify_tls_failed` (the worker validates the sink's certificate for a host that is not private-looking): that is expected and is not asserted; the From verdict is.
- Every default account of every other adapter test is `aligned` by the self-hosted exception (`example.test`: SPF with a bare `mx`, MX `smtp-sink` and `smtp-ctl`; the controllable sink is named `10.smtp-ctl.example.test`, which the worker reads as private and
  module 3 reads as below the From domain). A `createVerified` failure says which of the two it was (the hint names `verify_tls_failed` or the From verdict and its message).
- The first thing to check when the From-gate test fails is the fixture's log: every step asserts the questions module 3 asked (`m3dRequireAsked`) and a failure prints what it did ask.
- `NOTE m3a events enforce ...` (from `m3_adapter_closeout_events_test.go`): the trail of the quarantined account (`user:created`, `user:verified (verify: success)`, `system:quarantine (auth_failure)`) and the shadow control's (`system:updated (shadow_would_quarantine: trigger=auth_failure)`).
- These two files are named `m3_adapter_closeout_*`, which `m3a_evidence` (outage.sh: the NOTE lines it prints) does not match (it reads `m3_adapter_e2e*`): their NOTE lines are in the test log tail and in the `-test.v` log only.
