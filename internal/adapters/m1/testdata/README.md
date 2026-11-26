# Fixtures of the module 1 adapter

Everything under `contract/` is one recorded or hand-written response body of module 1, one file per route and case (20 files: 14 recorded, 6 written by hand). `Contracts()` in `contracts.go` names each file exactly once (a test
fails on a stray file and on a dangling name). No file holds a secret or a real host name (the domains are the harness fixture's, `*.fixture.test`), and no IP address except the documentation addresses `Normalise` writes (see `verify-ok.json`).

## RECORDED from the real module 1 (14 files)

These fourteen files are what the real module 1 answered, written by `cmd/contract-fixtures` after `contractcheck.Normalise` rewrote them, and never edited afterwards (a fixture is a recording: when the
real answer changes it is re-recorded, never edited). The harness made each call against the real module 1 (built from the pinned commit, running beside the platform, with its real verifier node) and printed the
live response in a delimited block of its log. They come from two rounds:

- **round 1**: the full record run **44225747596** and the scoped record run **44225751255**, both on **2026-11-18**. They reached 14 of the 17 module 1 contracts of that time. Eleven of the fourteen files below are from this round.
- **D1**, the Phase 3 close-out re-record against module 1 as it is after the close-out (`GET /v1/describe`, the idempotent create, `totals` on the job status), made by the record runs `Docs/phase-3-findings.md` names (44395876225 and 44399346732; this directory does not say which of the two produced which file). Three files below are from it: `describe-ok.json`, `job-running.json` and `results-page.json`. The two round 1 recordings whose real answer had the same shape with other values (`verify-ok.json`, `submit-error-validation.json`) were kept as they were recorded.

The unit tests derive every expected value from the file itself (see "How the unit tests use them"), never from a literal.

| File | Route | Case | From |
|---|---|---|---|
| `submit-accepted.json` | `POST /v1/verify/bulk` | the 202 of a raw submission of three rows, two of them malformed (counts 3 / 1 / 0 / 2), so it is NOT the 202 a plan would get: a plan never sends a malformed row (`TestSubmit_ACountThatDisagreesWithThePlanIsAContractError` uses it as exactly that case). It was recorded before the idempotent create existed and a request with no key answers exactly so; the answer to a keyed request is the same body | round 1 |
| `submit-error-unauthenticated.json` | `POST /v1/verify/bulk` | 401 `unauthenticated` | round 1 |
| `submit-error-validation.json` | `POST /v1/verify/bulk` | 400 `validation_failed`, details `items[0].item_id` and `items[1].item_id` both `duplicate` (the module names EVERY row that shares the label) | round 1, kept |
| `submit-error-too-large.json` | `POST /v1/verify/bulk` | 413 `payload_too_large`, detail `items` / `out_of_range` | round 1 |
| `job-running.json` | `GET /v1/jobs/{id}` | a real running job with its `totals`: `status` `running`, `complete` false, `processing_paused` false, 2 rows submitted and none settled yet (`counts` total 2 / settled 0 / pending 2; `totals` submitted 2, settled 0, pending 2, duplicates 0, malformed 0 and all four `by_status` counts 0). It replaces the round 1 job of 220 rows (20 settled) whose `totals` had been added by hand | D1 |
| `status-error-not-found.json` | `GET /v1/jobs/{id}` | 404 `not_found` (absent, or another tenant's job) | round 1 |
| `status-error-unauthenticated.json` | `GET /v1/jobs/{id}` | 401 `unauthenticated` | round 1 |
| `results-page.json` | `GET /v1/jobs/{id}/results` | contract case `page`: a page of 7 rows from the middle of a job (ordinals 7 to 13) with `next_cursor` `"00"` (7,280 bytes). It replaces the round 1 page of 220 rows (about 170 KB). See "The two results recordings" | D1 |
| `results-empty.json` | `GET /v1/jobs/{id}/results` | contract case `empty_page`: an EMPTY page, no rows and `next_cursor` `""` (93 bytes). See "The two results recordings" | round 1 |
| `results-error-bad-cursor.json` | `GET /v1/jobs/{id}/results` | 400 `validation_failed`, detail `cursor` / `malformed` | round 1 |
| `results-error-unauthenticated.json` | `GET /v1/jobs/{id}/results` | 401 `unauthenticated` | round 1 |
| `results-error-not-found.json` | `GET /v1/jobs/{id}/results` | 404 `not_found` | round 1 |
| `verify-ok.json` | `POST /v1/verify` | the real synchronous verdict: valid / accepted, confidence 90, recommend `send`, for `valid.fixture.test`; its `signals.engine_mx_hosts` and `meta.verifier_ip` are the documentation addresses `192.0.2.1` and `192.0.2.2` that `Normalise` wrote in place of the real ones | round 1, kept |
| `describe-ok.json` | `GET /v1/describe` | the whole self-description, as the real module answers it: module `verify`, api version `v1`, capabilities (`bulk_verify`, `sync_verify`, `idempotent_bulk_create` and `job_totals` true; `quote`, `run`, `report` false), seven routes, nine schemas, eight vocabularies (the seven the adapter maps and `detail_issue`), ten error codes, and the `idempotency` contract. No `pricing`. A contract of its own, `Contracts()` case `ok` | D1 |

### The two results recordings

`results-page.json` (contract case `page`) and `results-empty.json` (case `empty_page`) are the two live responses of `GET /v1/jobs/{id}/results` the recorder kept. The tests never look at the names: `recordedResultsPages` and `TestContracts_AMutatedFixtureIsRefusedNamingTheKey` decide what a file is from what it contains (rows or none), so row mutations go to the page that has rows and the mutations of an empty page go to the one that has none, and a recording set with only one of the two kinds is red.

How they came to be what they are: in round 1 the recorder filed the two live responses under the contract cases the harness assigned them, and assigned them the other way round from their names, so the two files were SWAPPED BY HAND (contents untouched); `results-empty.json` is that round 1 recording, as it was recorded. The page it kept for `page` was then a 220-row page of 169,783 bytes (96 percent of the whole directory), because the recorder's tie-break took the longest of the bodies that said the same. Phase 3 close-out D1 changed the tie-break (`pickIndex` in `internal/contractcheck/session.go`: the fewest positions that tell nothing, then the most positions, then the SHORTEST body) and the page was re-recorded from a smaller harness job; `results-page.json` is that recording and was not edited.

The 7-row page, for what it holds: ordinals 7 to 13 in order (it starts at 7, so no row's `ordinal` is its position in the page), `next_cursor` `"00"` (it is not the last page), four `risky` rows (`catch_all`, `disposable`, `provider_no_smtp` with `signals.smtp_skipped_reason` `provider_mx_routed`, `non_public_mx`), one `valid` / `accepted` row (ordinal 10) that repeats the first row of the job (`duplicate_of` 0: the original is not on this page), and two `unknown` rows (`greylisted` and `dns_error`, ordinals 12 and 13) that both carry `signals.failure_origin` `remote`. What it does NOT hold: a row from the cache (`flags.from_cache` is false throughout), an origin of `own` or `undetermined`, a skip token on a row that also has an origin, a malformed row, a row with ordinal 0, and a repeat whose original is on the same page; those are tested on inline bodies (`TestResults_ReadsOriginSkipTokenAndCache`, `TestResults_ReadsTheCasesTheRecordingsDoNotHold`). `TestResults_ReadsEveryRecordedPage` compares every row with the file (a row's expected ordinal is the page's first ordinal plus its position, not its position) and logs how many repeats, origins, skip tokens and cache hits the recordings hold, so a field the recording does not carry is visibly compared only as empty. A page that replaces it must keep at least three rows and one row with `duplicate_of` (`rowMutations` fails without them).

### What `Normalise` rewrote

`contractcheck.Normalise` makes a recording safe to commit and comparable later. Keys, key order and structure are unchanged; numbers, booleans, null and every other string are
untouched. What it rewrites:

- every UUID becomes `00000000-0000-4000-8000-00000000000N`, numbered by order of FIRST APPEARANCE IN THAT FILE. A job answer's `job_id` is the first UUID of its body, so it is `...0001`
  in every recorded job fixture, and so is the `request_id` of every recorded error envelope. That is why the unit tests' one job id constant, `testJob`, is `...0001`: the adapter
  refuses an answer about another job than the one it asked for.
- every RFC 3339 timestamp becomes `2026-01-01T00:00:00Z`.
- every IPv4 address becomes a documentation address by order of first appearance (`192.0.2.N`, then `198.51.100.N`, then `203.0.113.N`).
- a string under a key that names an id, cursor, hash or the like, and any long opaque string, becomes one repeated character of the same length. So the `item_id` of a recorded result
  is `aaaaaaaaaa`, not the label that was sent, and rows of equal label length share one value: the recorded item_ids are NOT unique and are not keys to settle with.
- a string under a key that names a secret becomes `REDACTED` (none of these bodies has one).

The files are written with two-space indentation, the keys in the order the module wrote them, and end with one newline.

## WRITTEN BY HAND from the module's code (6 files): not recordings

### Three that the close-out record run confirmed

Module 1 gained three additive features in the Phase 3 close-out (partial Tier 4: `GET /v1/describe`, an idempotent bulk create on an `Idempotency-Key` header, `totals` on the job status) after the round 1 recordings were made. These three files were written from the module's handlers (`control-plane/internal/api/api.go`, `errors.go`, `describe.go`) and its `api-contract.md` before it could be recorded. The D1 record run then compared them with what the real module answered, and `Docs/phase-3-findings.md` says the answer was byte for byte the same, so they were not replaced: what they hold is what the module says, but they are written files, not recordings.

| File | Route | Case |
|---|---|---|
| `job-complete.json` | `GET /v1/jobs/{id}` | a finished job of 2 rows, both settled `invalid` (`complete` true, `status` `complete`). The round 1 recording of the job, WITH a `totals` object ADDED BY HAND (`submitted` 2, `settled` 2, `pending` 0, `duplicates` 0, `malformed` 0, `by_status` `invalid` 2, consistent with the file's own `counts`): the only edit of a recorded file in this directory. Re-recording it would not change a byte |
| `cancel-ok.json` | `POST /v1/jobs/{id}/cancel` | Phase 4 close-out C2 (recorded from module 1 at `verify@c516f28` by the m1node stack's record run): the job-status body plus `cancel_outcome` (one shape for `cancelled`, `already_cancelled` and `already_complete`; the file holds the first answer, the session compares the repeats with it). |
| `cancel-error-not-found.json` | `POST /v1/jobs/{id}/cancel` | the 404 of a cancel of a job that does not exist (the same envelope as another caller's job). |
| `results-page-cancelled.json` | `GET /v1/jobs/{id}/results` | a page that holds a row cancelled before its check began: status `cancelled`, reason `cancelled_by_caller`, an empty `flags`. |
| `submit-error-key-reused.json` | `POST /v1/verify/bulk` | 409 `idempotency_key_reused`, detail `Idempotency-Key` / `conflict` |
| `describe-error-unauthenticated.json` | `GET /v1/describe` | 401 `unauthenticated` |

The replay of an idempotent create (a repeat of the same key and request) is the 202 of `submit-accepted.json` again, byte for byte, plus the response header `Idempotent-Replayed: true`: it is no contract of
its own, and a 409 `idempotency_key_reused` is the same error envelope as the others (the adapter's `errorCodeValues` lists the code, and `describe-ok.json` lists it in `error_codes`).

### Three that no record run reached (Phase 3 B6)

The record runs did not reach these three (each needs a state the harness does not produce: the kill switch pulled, the queue key missing, a store fault); the drift test allowlists
them (`tests/harness/zz_m1_node_contract_drift_test.go`). They are written from module 1's handlers (`control-plane/internal/api/api.go`, `errors.go`) at the pinned commit and from
`Docs/history/p3-research/m1-api-and-node.md` Part 1, with the same synthetic-id convention (`...0001`).

| File | Route | Case |
|---|---|---|
| `submit-error-paused.json` | `POST /v1/verify/bulk` | 503 `paused` (needs the kill switch pulled) |
| `submit-error-unavailable.json` | `POST /v1/verify/bulk` | 503 `unavailable` (needs the queue key missing) |
| `submit-error-internal.json` | `POST /v1/verify/bulk` | 500 `internal_error` |

## Vocabularies and where they live (`VocabAt`)

A contract lists a vocabulary only when its own fixture has a string at the path the set constrains (`RouteContract.VocabAt`; a bare set name would be read by the drift gate as a path to a
top-level key, which most of these bodies do not have, and the vocabulary would constrain nothing). `TestEveryVocabularyConstrainsSomethingInItsOwnFixture` holds that for every contract.

| Set | Path | On |
|---|---|---|
| `status`, `reason`, `recommendation` | `status`, `reason`, `recommendation` | `POST /v1/verify` `ok` |
| `submit_status` | `status` | `POST /v1/verify/bulk` `accepted` |
| `job_status` | `status` | `GET /v1/jobs/{id}` `running`, `complete` |
| `status`, `reason` | `results[].status`, `results[].reason` | `GET /v1/jobs/{id}/results` `page` (the page with rows) |
| `error_code` (module 1's ten codes) | `error.code` | every error contract (13) |
| the seven sets the adapter maps (`status`, `reason`, `recommendation`, `failure_origin`, `smtp_skipped_reason`, `job_status`, `submit_status`) | `vocabularies.<name>.values[]` | `GET /v1/describe` `ok` |
| `error_code` | `error_codes[].code` | `GET /v1/describe` `ok` |

`failure_origin` and `smtp_skipped_reason` live in `signals`, on an unknown answer and on an answer that skipped the SMTP stage. No recorded verdict holds either; since the D1 re-record the
recorded results page does (two rows carry `failure_origin` `remote`, one a `smtp_skipped_reason`), but the `page` contract does not list them on its rows (it lists `results[].status` and
`results[].reason` only): they are published on the describe contract, where module 1 lists every set it has, and the tests also prove the adapter's handling of both on inline bodies and on the
recorded page's rows. `GET /v1/jobs/{id}/results` `empty_page` lists no row set: it has no row to hold one.

## How the unit tests use them

A test that serves a RECORDED fixture takes what it expects from the file: `recordedStatus`, `recordedCounts`, `recordedRows`, `recordedDoc` and the summaries of `contracts_test.go`
parse the body with `encoding/json` into generic maps, sharing no code with the adapter's decoding, and the test compares the adapter's result with that parse. Where a test needs a body
the recording cannot be (a 202 whose counts a plan predicts: `acceptedFor`; a running job with some rows settled, which the recorded running job, with none settled, cannot be:
`TestStatus_AnAnswerThatContradictsItselfIsAContractError`), it derives one from the recording, keeping its keys and job id, with every number it changes derived from the recording's own. Mutated
bodies are made by editing the parsed tree (`mutation.apply`), and a mutation that does not apply to the body it is given fails the test as proving nothing. Every comparison has a paired
control that changes one value and must be reported. Cases the recordings do not hold (an origin of `own`, a row from the cache, a degraded answer with a skip token and an origin, a repeat of
a row on the same page, a malformed row) are inline bodies in the tests, written from the module's code.

`/healthz` and `/readyz` have no fixture here: the adapter passes them through `modclient`, which owns their shapes (see `UncoveredRoutes`).

## Phase 4 D1: three recordings are pending (nothing here is written for them)

Module 1's cancel is a contract that CancelContracts DECLARES and Contracts LISTS only once the switch `recordedPauseFixtures` (contracts.go) is true: cancel-ok.json (POST /v1/jobs/{id}/cancel, 200:
ONE shape for the outcomes `cancelled`, `already_cancelled` and `already_complete`, so one file, recorded from the first answer, the outcome `cancelled` of a running job), cancel-error-not-found.json (the 404
of a job that does not exist) and results-page-cancelled.json (a results page that holds a row cancelled before its check began: status `cancelled`, reason `cancelled_by_caller`, an empty `flags`). The
status of a cancelled job is the body of any job's status (counts.cancelled, totals.cancelled and cancel_requested are keys of job-running.json and job-complete.json) and has no file of its own. None of the
three files is in this directory yet and none is written by hand: the drift session in record mode (zz_m1_node_contract_drift_test.go, CONTRACT_RECORD=1) prints them and cmd/contract-fixtures installs
them. TestCancelFixturesAreInstalledWhenTheSwitchIsOn and TestCancelFixturesDoNotExistWhileTheSwitchIsOff keep the switch and the files together; when the switch is flipped, move these three into the
tables above (recorded) and delete this section.
