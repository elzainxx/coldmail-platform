# Phase 3 findings: did the contract hold, how thick are the adapters, and what the evidence says about Tier 4

Spec: [`phase-3-spec.md`](phase-3-spec.md) (row B14). Research the work stood on: [`history/p3-research/`](history/p3-research/). Every claim below cites a test, a fixture, a run or a `file:line`; where a number comes from a command the command is named. **The Tier 4 decision is the owner's**; this document recommends and implements nothing of Tier 4.

## Addendum: the Phase 3 close-out (written after the owner ruled on section 8; evidence in [`phase-3-closeout-spec.md`](phase-3-closeout-spec.md))

Everything below this addendum is the record of Phase 3 as it ended (`platform@52fe035`); it is kept as it was written. The close-out built what the owner ruled, and these statements of the sections below are therefore **no longer true**:

- Section 1 and 3 (B13): "module 3 has no cancel" and "the Tier 4 decision is the owner's" are history: module 3 has `POST /v1/cancel` (ruling OD-1, `send-byo@8855a52`) and module 1 has the partial Tier 4 the section 5 recommends (ruling OD-6/OD-8, `verify@fad942e`).
- Section 4's counts. After the close-out the adapters are (non-test, non-comment, non-blank lines, tests by `grep -c '^func Test'`, contracts by `len(Contracts())`):

| Package | Lines | Unit tests | Contracts | `MODULE-FORCED` cases (was) |
|---|---|---|---|---|
| `modclient` | 1,588 | 69 | n/a | n/a |
| `adapters/m1` | 1,312 | 86 | 20 | **9** (13) |
| `adapters/sendapi` (modules 2 and 3 share it) | 1,270 | 60 | shared | 13 (11) |
| `adapters/m2` | 90 | 22 | 20 | 1 (2) |
| `adapters/m3` | 1,546 | 44 | 49 | **2** (9) |

  The 35 forced cases are **25**. Module 1 grew by 308 lines (describe and its vocabulary check, the idempotent submission, the totals) and lost four special cases; the shared body grew by the cancel and the cause mapping that module 3 now offers (and took over module 2's two cancel cases, SA-F12 and SA-F13); module 3's adapter lost its whole prediction layer (`PreCheckRun`, `RunFacts`, `ExplainConflict`, its own host rule) and seven special cases because the module now says what it used to leave to guessing.
- **The 13 module 1 special cases (section 4 and 5), each removed or kept:**

| Case | Now | Why |
|---|---|---|
| M1-F1 a non-UUID job id is a 500 | **removed** | the module answers a 400 `validation_failed` (A3); a cheap local check stays as plain input validation |
| M1-F2 the bulk submission is not idempotent ("orphan job possible") | **removed** | `Idempotency-Key`: a repeat is the same job; the adapter always sends a key and retries safely; the orphan handling is gone |
| M1-F3 no describe | **removed** | `GET /v1/describe`; the adapter asks the module (`CheckVocabularies`) like it does modules 2 and 3 |
| M1-F4 a running job's results are a moving target | **kept, narrowed** | still true while a job runs; the module now documents and tests that the listing is stable once `complete` is true, which is what the re-walk after completion relies on; the completeness check is against the job's totals |
| M1-F5, F6, F7 data shape (duplicate rows carry the first verdict, the cache flag's place, the open `signals` bag) | kept | per-route quirks no verb touches |
| M1-F8 counts only in the 202 | **removed** | the counts are `totals` of the job status |
| M1-F9 to F12 item_id rules, the 100,000-row cap, the 64 MiB body, the silent limit clamp | kept | per-route limits |
| M1-F13 the job status word passes through | kept | the boolean `complete` stays authoritative |

  Four of the 13 are gone and one is narrower: the section 5 estimate ("a shared job core would save about a quarter of the adapter") was not what happened: the three additive changes cost 308 lines of adapter and removed the three failure classes (an unconfirmable submission, a vocabulary only CI could see, a count that exists once), which is what the recommendation said they would.
- **Module 3's nine special cases (section 4):** M3-F1 (the campaign cap is returned), F2 (the quote reads the same function as run), F3 (the 409 names its causes), F4 and F7 (closed code sets, no internal text), F5 (describe agrees with the handlers) and F8 (the module guards the host, at add and at connect time) are **removed**; F6 and F9 (a create 409 and a test-button 409 with one fixed sentence) are kept.
- **Section 6 (what the modules showed):** the node registration and lease items are closed by A2 and documented (`operator-runbook` §5.1 and §5.2); module 2's worker never gives up (B1); module 3's internal reasons no longer cross the boundary (C3, with a gate); the `Health` liveness cell is in the no-value table (D4).
- **Section 7 (what the gates do not see):** the From-gate WARN and BLOCK paths are now run (D2), the enforced health action is observed (D3), module 1's results fixture is a recording of a 7-row page (7 KB, it was 170 KB; D1). Still not seen: the list in row E3 of the close-out spec, each with the later phase or owner action that covers it.
- **Section 8 (decisions):** every one is ruled and closed in the close-out spec's table; the only thing still to decide is listed in the report (the forged-cursor 500 that module 3 still answers).

What the close-out found that this document did not expect, in the order it was found:
1. The shared vector file's expectations had to be produced by module 1's real syntax layer; the platform's own rule is now `norm/v1` plus that layer (`norm.Validate`), which **supersedes the Phase 2 ruling that `a@b` is storable** (OD-11).
2. The hand-written fixtures, compared with the recordings of the real modules (record runs 44395876225 and 44399346732 on the runner): module 1: 12 of the 17 recorded files are byte-identical to what was committed (three of them, the `totals`-bearing complete job, the 409 for a reused key and the 401 of describe, were written by hand from the module's code and the module answered exactly that); the other five are the same shape with other values (the describe, the running job, the verdict and the validation error) except the results page, whose shape differs because the smaller job it now comes from holds other optional keys. Module 2: nine files differ in values, two in shape. Module 3: twelve files differ in shape, every one of them by keys the module added in the close-out (`from_alignment`, `cancelled`, `campaign_size_cap`, the cause fields). The recording also showed that the recorder's tie-break had made a 220-row page of 170 KB the fixture of module 1's results route: it now keeps the shortest body that says the most, and the fixture is a 7-row page of 7 KB (a recording of a smaller harness job; no recording was edited).
3. Module 3's own prediction code in the platform was deleted rather than updated: a second copy of the module's rules is a source of disagreement, so the adapter reads the module's `details[]` and the quote's own reasons.

## 1. The verdict in short

- **The contract held, with no module change.** Three modules whose APIs were designed separately were driven through three thin adapters end to end (submit, poll, read every page, settle through the money core), item by item, against the REAL modules in the harness: nothing was mocked and no module was edited. Every module-side behaviour the adapters needed already existed; what the adapters had to absorb is a list of **35 quirks** (section 4), none of which blocked a flow.
- **Our reading of the modules was right.** Hand-written fixtures derived from module source were compared with the first recordings from the real modules: **40 of 41 were identical in shape** and the one difference is a set of three optional keys module 3 omits until set (`report.job.sent`: `error_class`, `failed_at`, `smtp_response`). Across all 75 contracts the harness finally reached (module 1 14 of 17, module 2 20 of 20, module 3 41 of 45) the fixture standing before a contract was (re)recorded differed in shape from the real answer in **3**: module 1's synchronous verdict (2 positions), module 3's `describe` (17 positions, the hand-written copy in `sendapi` was known to be inexact) and module 3's duplicate-account 409 (the `details` key). The research documents were correct about routes, keys, types and vocabularies; the surprises were in behaviour (limits, ambiguity, ordering), not in shape.
- **The drift gate found its own holes before it found a module's.** The first compare-mode runs on the real modules went red naming route, case and key, and every red was a defect of how the gate described a module, not a module change: keys a module sets or omits by state (`completed_at`, `provider_message_id`, `flags.is_disposable`, `flags.is_spam_trap`) that the contract had to declare optional, an open bag (`signals`) whose keys differ row by row (`Shape.Open()`), and, worst, **vocabularies that constrained nothing**: the adapters named their closed sets ("job_status"...) instead of the JSON path they constrain, so the gate read each name as a key the body does not have and no vocabulary of any module was ever compared with a live response until `RouteContract.VocabAt` and the check that a vocabulary constrains something in its own fixture (`VocabInert`) existed. A third class was the matcher itself (a body of another structure given to a contract because an empty array fits anything): fixed by ranking contracts by differences plus gaps plus vocabulary mismatches, with a stress test over all contract orders.
- **Most first runs on the runner were green first time** (module 1 node 9 of 9, module 2 worker 5 of 5, module 2 adapter 9 of 9, module 1 adapter 13 of 13, both outage proofs, the whole module 3 set inside a 234-test full harness). That is evidence for the design only together with the controls: every comparison has a paired control that is proven to fail (counts in section 3), the authors mutated the code 100+ ways and each mutation is caught by a named test, and nine fault proofs make the gates themselves go red on a planted defect. It is also why section 7 lists what the gates cannot see.
- **Adapter thickness is real but concentrated.** The four routes modules 2 and 3 share are ONE body (`sendapi`, 1,099 lines) read by 9 profile branches; module 2 on top of it is 150 lines, module 3 adds 1,568 (more than half of it the account surface the user app needs); module 1, which has a different verb model, is 1,004 lines on its own.
- **Tier 4 recommendation (section 5): a partial Tier 4.** Add `describe`, an idempotent job creation and totals on the job status to module 1 (three additive changes that remove three real failure classes); do NOT force `quote`/`run`/`report` parity on it.

## 2. What was built (and where each exit criterion is proven)

| Piece | Where | Proof |
|---|---|---|
| Plumbing client: bearer auth, error envelope into typed errors, no value on error, size limits, timeouts and retries as dials, an observer hook | `internal/modclient` | 63 unit tests; the no-value table of about 176 cells; fault proof `modclient-no-value` |
| Tenant flow: `auth.users.id` to `public.users.id` to each module's scoping (module 1: a per-user derived caller key provisioned as a `verify.api_callers` row) | `internal/tenant` | 13 unit tests; harness tests on REAL `auth.users` rows; fault proof `tenant-m1-key-vector` |
| Module 1 adapter (Prepare with the malformed filter, Submit never retried, Status, Results, Drain, Verify) | `internal/adapters/m1` | 66 unit tests; harness `TestM1Node_E2E*` against the real node |
| Shared sender core (quote, run, single and campaign report, the walk, Classify) | `internal/adapters/sendapi` | 48 unit tests; used by both of the next two |
| Module 2 adapter (+ cancel) | `internal/adapters/m2` | 21 unit tests; harness `TestM2Worker_Adapter*`, `_Outage*` |
| Module 3 adapter (+ feasibility, pre-check, accounts, suspension read) | `internal/adapters/m3` | 44 unit tests; harness `TestM3Adapter_*` |
| Contract drift gate: recorded fixtures, observer, matching, coverage, record mode | `internal/contractcheck`, `cmd/contract-fixtures`, the three `zz_*_contract_drift_test.go` | 253 + 6 unit tests; fault proofs `contract-fixture-key`, `contract-compare-type` |
| Real module 1 node with fixture DNS, SMTP and ipify; real module 2 worker with the controllable sink | `tests/harness/m1node`, `tests/harness/m2worker`, `tests/harness/fx` | the stage floors; see the spec rows B7 and B8 |

## 3. Did the contract hold: what each exit criterion showed

(The run URLs of each criterion are in the spec's rows B7 to B13; this section says what the runs showed.)

- **End to end with item-by-item equality (B9).** Module 1: a job of 27 inputs (20 recipes, a cache hit, the same address in three spellings, the greylisted address again, three malformed with three different `norm` reasons) settled with the ledger equal to `charge.Decide` over the reported outcomes AND to a hand-written oracle per recipe; duplicates were one check and one charge with every row answered; the fixture's RCPT counter showed one probe per distinct address; the malformed three never reached module 1. Module 2: ten items (six sent, two bounced, two exhausted) equal three ways (module report, sink counters, ledger), the settle replayed writes nothing, ten different corruptions of the settle list each rejected for its own reason. Module 3 (not metered): a campaign of twelve equal to the sink's counters, `Classify` equal to the charge table and to the oracle (free by M3-01), nothing reached the ledger.
- **Pagination across boundaries (B10).** Module 1 at limits 0 (the default of 100), 1, 110, 219, 220, 221 and 1000 over 220 rows; module 2 at page sizes 1, 5, 13, 21, 41, 42 and 100 over 42 items; module 3 at seven sizes over 45 items and over 203 accounts across the 200-row account page. Each is exact set equality with a baseline, and each has two look-ahead walkers (written in the test) that the same check catches.
- **Cross-tenant (B11).** Module 1: tenant B's `Status`, `Results` (even with a garbage cursor) and `Drain` on A's job are the module's 404 `not_found`, identical to the answer for a job that does not exist; an unprovisioned key is 401. Module 2: an empty 200 for A's campaign report in B's hands, not-found for A's items and job, `{0,0,0}` for B's cancel of A's campaign (A's campaign still ends all sent). Module 3: the documented empty 200 or 404, and the service token refused on the admin routes. In every case the same call with the owner's adapter succeeds (the paired control) and no id, address or key of A appears in B's answers.
- **Outage mid-job (B12).** The module's API container is stopped with a job certainly in flight. Every call the platform polls with fails typed `unavailable` (the classification the client really gave: `dns lookup failed`, request not sent) with no value; a write against the dead module fails typed and creates nothing (module 1: not called an orphan, because the request never left); the platform's ledger, reserve and `job_items` rows equal the pre-outage record; polling loops stop by their context. After the restart the job completes exactly once (module 1: one job for the tenant and one charge per charged item; module 2 and 3: one message per sent item at the sink, every item reported once with the job id it had before, a repeat `Run` is the module's replay).
- **Cancel through the adapter (B13).** Module 2: nothing new reaches the sink after the cancel returns, the item in flight is reported once and charged once, the cancelled items are free (no ledger row: `money.Settle` writes nothing for a free item), the reserve is released, a second cancel returns the same position. Module 3 has no cancel (open decision 1): a test holds the method absent so nobody fakes one.
- **Typed errors, never an invented outcome.** The `no value beside the error` assertion held on every call of every outage phase; the rule is also a table over every operation and every failure class in `modclient`.

## 4. Thickness and every place a module forced a special case

Non-test, non-comment, non-blank lines (`internal/adapters/*`, counted by the command in the footer):

| Package | Lines | Unit tests | Contracts | `MODULE-FORCED` cases |
|---|---|---|---|---|
| `modclient` (shared transport, all three) | 1,522 | 63 | n/a | n/a |
| `adapters/m1` | 1,004 | 66 | 17 | 13 |
| `adapters/sendapi` (modules 2 and 3 share it) | 1,099 | 48 | 18 | 11 |
| `adapters/m2` (on sendapi) | 150 | 21 | 20 | 2 |
| `adapters/m3` (on sendapi): account surface (accounts, bulk, hosts) 831; feasibility and pre-check 224; the rest 513 | 1,568 | 44 | 45 | 9 |

- **Module-specific branches.** The shared core branches on the module in exactly 9 places, all through the `Profile` value: `LinearCost` (`client.go:197`), `QuoteFeasibility` (`client.go:201`, `shapes.go:34`), `AccountDetail` (`client.go:332`, `shapes.go:51,62,84`), `AlwaysPresent` (`shapes.go:41`), the run-409 reading (`errors.go:61`). Module 2 versus module 3 on the four routes is therefore four booleans, one enum and five vocabulary lists.
- **Where the thickness is.** Module 2 costs 150 lines beyond the core. Module 3 costs 1,568, but 831 of those are the user-facing account routes that module 2 does not have, and 224 are the feasibility block and the pre-check that exist because module 3's `run` answers one 409 for four causes. Module 1 costs 1,004 because it has a different verb model (a bulk job plus a results listing) and the most quirks; its drain and status code (about 225 lines) is the part that resembles `sendapi`'s walk (163 lines).

**The 35 forced special cases** (the registry is `forced.txt` in each package, held in step with the code tags by `TestForcedSpecialCasesAreRegistered` and its fault proof):

| Class | Cases | What they are |
|---|---|---|
| Ambiguous answers (the module cannot say why) | SA-F1, SA-F3, SA-F10, M2-F2, M3-F3, M3-F6, M3-F9 | one 409 body for several causes; a 404 or an empty 200 for "never run", "someone else's" and "typo" alike; a cancel of an unknown campaign answers zeros |
| Missing capability | M1-F2, M1-F3, M1-F4, M1-F8, M3-F1, M3-F2 | no idempotent create (M1), no describe (M1), results of a running job are a moving target (M1), counts only in the 202 (M1), the campaign cap is never returned (M3), the quote ignores suspension, queue and campaign state (M3) |
| Limits and silent clamps | M1-F9, M1-F10, M1-F11, M1-F12, SA-F6, SA-F8, M1-F1 | item_id rules, the 100,000-row cap answered late, a 64 MiB body answered as 400, a limit outside 1..1000 silently replaced, an unbounded quote count, a clamped page size, a non-uuid id answered 500 |
| Error reporting that crosses the boundary | SA-F2, SA-F5, M3-F4, M3-F7, M1-F13 | a forged cursor answers 500, a malformed id is reported as "required", raw internal error text in bulk-add codes and the test button, a status word passed through |
| Data shape quirks | SA-F4, SA-F7, SA-F9, SA-F11, M1-F5, M1-F6, M1-F7, M3-F5, M2-F1 | omit-until-set keys, two timestamp formats, totals and items not a snapshot, `attempt_count` that is not an attempt count, duplicate rows carrying the first row's verdict, the cache flag in a different place, an open `signals` bag, `describe` naming `results` where handlers return `items`, cancel answering a position and not a delta |
| Safety | M3-F8 | `smtp_host` accepts any IP, so the adapter refuses a non-public host unless built to allow it |

## 5. Tier 4: should module 1 adopt quote/run/report/describe?

**Recommendation: a partial Tier 4, additive, three changes; not the full verb parity.** The owner decides.

Evidence, by the 13 module 1 special cases:

1. **`describe` (fixes M1-F3).** Module 1 is the only module whose vocabularies the platform must hold as constants (`internal/adapters/m1/vocab.go`, 77 lines) because it cannot ask. Modules 2 and 3 are checked at runtime (`CheckVocabularies`: a vocabulary the module advertises that the adapter does not handle is a typed error before any money moves); module 1 is checked only by the CI drift gate, so a vocabulary change shipped in production between two CI runs would be seen as a `Contract` error on a live row, not before. Cheapest of the three.
2. **An idempotent job creation (fixes M1-F2).** `POST /v1/verify/bulk` has no key and the module stores a `caller_ref` it cannot be asked about, so an unconfirmed submit leaves "an orphan job is possible" that no one can resolve (the B12 phase 2 evidence: the write fails typed, the platform cannot know whether the module created a job). Making `caller_ref` a per-caller unique key (or accepting an `Idempotency-Key`) lets Phase 4 retry safely; today the adapter must refuse to retry (`internal/adapters/m1/submit.go:54`).
3. **Totals on the job status and a stable listing (fixes M1-F4 and M1-F8).** The duplicate and malformed counts exist only in the 202, and a running job's results are a moving target, so the adapter walks again from the start after completion and cross-checks the 202. Adding the counts to `GET /v1/jobs/{id}` and defining the listing as stable once `complete` is true would remove both workarounds.

What full parity would NOT fix: 9 of the 13 module 1 cases (M1-F1, F5 to F7, F9 to F13) are per-route quirks (a 500 for a malformed id, a limit that clamps, the verdict of a duplicate row, the place of a flag) that `quote`/`run`/`report` verbs do not touch. And module 1's price cannot be quoted: what the platform charges depends on the answer (a valid row, an unknown caused by the recipient's server, an unknown caused by us; `internal/charge/rules.psv`), so a `quote` would be an estimate that the adapter must treat as one. The upper bound of what a shared job core could save on the platform side is module 1's drain and status code (about 225 of 1,004 lines, about a quarter), against the risk of changing a module that has passed its own go-live checklist. Hence the three additive changes and no more.

## 6. What the modules showed that is not a contract problem (module findings, nothing changed here)

- **Module 1, node registration.** Every control-plane boot registers the node as `provisioning` with `port25_open=false`; nothing in the code promotes it (open decision 4, confirmed on the runner: scope `m1node`, the node row after the first green audit). After a restart the harness promotes it again in SQL.
- **Module 1, crash recovery.** A stop leaves the leased batch of up to 10 work items stranded until the lease TTL and a reclaim sweep: 150 s plus up to another 150 s by default. The harness runs the control plane with `TM_QUEUE_LEASE_MARGIN_SECONDS=5` so recovery takes about a minute (`tests/harness/docker-compose.m1node.yml`). The proof is a graceful stop; a SIGKILL mid-send is not covered.
- **Module 2, the worker gives up.** After about 30 reconnect attempts without unacknowledged work its worker exits; an outage longer than that needs the worker started again (the stage does, `m2w_outage_worker`).
- **Module 3, internal reasons cross the boundary** in more places than the research listed (the test button's `from_alignment.error`, bulk-add `code` forms such as `unique_violation:<constraint>` and `tx_begin_error`); the adapter drops the text and keeps a closed set plus an `Unrecognised` bucket.
- **Quote of module 3** predicts two of the four 409 causes wrongly and never returns the campaign size cap; the adapter's `PreCheckRun` takes the platform's own facts and `NotChecked()` names what it could not check.
- **The research versus the code**: the run 409 of module 2 is code `conflict` with no details (not `user_campaign_active`); module 1's job status word has four values, not two; module 3's quote caps are `queue_depth_cap`, `daily_send_cap` and `account_count_cap`.

## 7. What the gates do not see (printed by the drift gate on every run)

- Responses for inputs the harness never sends, fields a module adds in states the harness never reaches, anything a module does only in production.
- The contracts the harness cannot elicit are listed with their reasons in `m1DriftNotElicited`, `m2DriftNotElicited` and `m3DriftNotElicited` (`tests/harness/zz_*_contract_drift_test.go`); an entry for a contract that WAS exercised is red.
- A SIGKILL of a module mid-write; a module restarted by an operator with a different configuration; module 3's From-gate WARN and BLOCK paths (the gate is off in the harness); the `rate_limited` answers unless a test reaches them.
- A mutation of the liveness `status` branch of `modclient.Health` is not caught by the no-value table (a 200 body whose status is not ok is not one of its failure cells): found while rehearsing the `modclient-no-value` fault proof, noted, not fixed.
- The fixtures of the contracts the harness did not reach are still hand-written from module source (each package's `testdata/README.md` says which are recorded).

## 8. Decisions that are the owner's (carried from the spec, with this phase's evidence)

1. Module 3 has no cancel: the platform-side rule (suspension only) or an additive `POST /v1/cancel`.
2. One caller key per platform user (built) or one service key.
3. An additive `details[]` on module 3's `run` 409.
4. The module 1 node promotion (documentation or code) before go-live.
5. Tier 4 for module 1: section 5.
6. Whether the platform should pre-validate more than `norm/v1` (module 1 charges an address that normalises but fails its syntax layer under M1-10).

---

Measured with: non-test, non-comment, non-blank lines of each package's `.go` files (`grep -v '^[[:space:]]*$' | grep -v '^[[:space:]]*//'`); tests by `grep -c '^func Test'` over `*_test.go`; contracts by `len(Contracts())`.
