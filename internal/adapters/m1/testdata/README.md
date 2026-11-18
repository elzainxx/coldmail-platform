# Fixtures of the module 1 adapter

Everything under `contract/` is one recorded or hand-written response body of module 1, one file per route and case. `Contracts()` in `contracts.go` names each file exactly once (a test
fails on a stray file and on a dangling name). No file holds a secret or a real host name (the domains are the harness fixture's, `*.fixture.test`), and no IP address except the documentation addresses `Normalise` writes (see `verify-ok.json`).

## RECORDED from the real module 1 (14 files)

Recorded by the contract drift session in the harness: the full record run **44225747596** and the scoped record run **44225751255**, both on **2026-11-18**. The harness made each call
against the real module 1 (built from the pinned commit, running beside the platform, with its real verifier node), printed the live response in a delimited block of its log, and
`cmd/contract-fixtures` wrote the files after `contractcheck.Normalise` rewrote them. They reached 14 of the 17 module 1 contracts. The unit tests derive every expected value from the
file itself (see "How the unit tests use them"), never from a literal.

| File | Route | Case |
|---|---|---|
| `submit-accepted.json` | `POST /v1/verify/bulk` | the 202 of a raw submission of three rows, two of them malformed (counts 3 / 1 / 0 / 2), so it is NOT the 202 a plan would get: a plan never sends a malformed row (`TestSubmit_ACountThatDisagreesWithThePlanIsAContractError` uses it as exactly that case). It was recorded before the idempotent create existed and a request with no key answers exactly so; the answer to a keyed request is the same body |
| `submit-error-unauthenticated.json` | `POST /v1/verify/bulk` | 401 `unauthenticated` |
| `submit-error-validation.json` | `POST /v1/verify/bulk` | 400 `validation_failed`, details `items[0].item_id` and `items[1].item_id` both `duplicate` (the module names EVERY row that shares the label) |
| `submit-error-key-reused.json` | `POST /v1/verify/bulk` | 409 `idempotency_key_reused`, detail `Idempotency-Key` / `conflict`: HAND-WRITTEN from module 1's code (Phase 3 close-out), to be replaced by a recording |
| `describe-error-unauthenticated.json` | `GET /v1/describe` | 401 `unauthenticated`: HAND-WRITTEN, to be replaced by a recording |
| `submit-error-too-large.json` | `POST /v1/verify/bulk` | 413 `payload_too_large`, detail `items` / `out_of_range` |
| `job-running.json` | `GET /v1/jobs/{id}` | a job in progress (220 rows, 20 settled, 200 pending); its `totals` object was added by hand, see "HAND-WRITTEN for the Phase 3 close-out" |
| `job-complete.json` | `GET /v1/jobs/{id}` | a finished job (2 rows); its `totals` object was added by hand, likewise |
| `status-error-not-found.json` | `GET /v1/jobs/{id}` | 404 `not_found` (absent, or another tenant's job) |
| `status-error-unauthenticated.json` | `GET /v1/jobs/{id}` | 401 `unauthenticated` |
| `results-page.json` | `GET /v1/jobs/{id}/results` | contract case `page`: a FULL last page, 220 rows and `next_cursor` `""` (about 170 KB). See "The two results recordings" |
| `results-empty.json` | `GET /v1/jobs/{id}/results` | contract case `empty_page`: an EMPTY page, no rows and `next_cursor` `""` (93 bytes). See "The two results recordings" |
| `results-error-bad-cursor.json` | `GET /v1/jobs/{id}/results` | 400 `validation_failed`, detail `cursor` / `malformed` |
| `results-error-unauthenticated.json` | `GET /v1/jobs/{id}/results` | 401 `unauthenticated` |
| `results-error-not-found.json` | `GET /v1/jobs/{id}/results` | 404 `not_found` |
| `verify-ok.json` | `POST /v1/verify` | the real synchronous verdict: valid / accepted, confidence 90, recommend `send`, for `valid.fixture.test`; its `signals.engine_mx_hosts` and `meta.verifier_ip` are the documentation addresses `192.0.2.1` and `192.0.2.2` that `Normalise` wrote in place of the real ones |

### The two results recordings

The recorder (in the three record runs) filed the two live responses of `GET /v1/jobs/{id}/results` under the contract cases the harness assigned them, and it assigned them the other way round from their names. The two files were SWAPPED BY HAND afterwards, so that `results-page.json` (case `page`) holds the page with 220 rows and `results-empty.json` (case `empty_page`) the page with no rows; the contents themselves are exactly what the real module answered (the bodies are not edited). The tests never look at the names: `recordedResultsPages` and `TestContracts_AMutatedFixtureIsRefusedNamingTheKey` decide what a file is from what it contains (rows or none), so row mutations go to the page that has rows and the mutations of an empty page go to the one that has none, and a recording set with only one of the two kinds is red.

The 220-row page, for what it holds: 177 `invalid` and 43 `risky` rows (91 `no_mx`, 43 `null_mx`, 43 `spam_trap`, 43 `disposable`), ordinals 0 to 219 in order, three rows that repeat an
address (`duplicate_of`: 217 to 10, 218 to 100, 219 to 150: none repeats row 0), no `unknown` row, and no row with a `failure_origin` or an `smtp_skipped_reason`; so the origin and
skip-token decoding is tested on inline bodies (`TestResults_ReadsOriginSkipTokenAndCache`, `TestResults_ReadsTheCasesTheRecordingsDoNotHold`), not on a recording.

**Is the 220-row page worth shrinking later?** It is 169,783 bytes, 96 percent of the whole fixture directory and the only large file in the package. What it buys: the only recorded
rows, the only recorded `duplicate_of` rows (the unit tests pick them out by parsing the file), a page that is longer than any `limit` the tests use. What it costs: 170 KB in every
re-record diff, and the drift gate does not need it (it folds an array into one element). Shrinking is worth doing once the harness job that produces it is made smaller (20 or so
rows is enough); do it by changing that job and re-recording, never by editing the recording. A smaller recording must keep at least three rows, one row with `duplicate_of`, and (for
the tests that exercise the charge table over a recording) one `risky` row. Nothing breaks if it stays as it is.

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

## HAND-WRITTEN from the module's code (3 files), to be replaced by a recording from the real module in the harness (Phase 3 B6)

The record runs did not reach these three (each needs a state the harness does not produce: the kill switch pulled, the queue key missing, a store fault); the drift test allowlists
them (`tests/harness/zz_m1_node_contract_drift_test.go`). They are written from module 1's handlers (`control-plane/internal/api/api.go`, `errors.go`) at the pinned commit and from
`Docs/history/p3-research/m1-api-and-node.md` Part 1, with the same synthetic-id convention (`...0001`).

| File | Route | Case |
|---|---|---|
| `submit-error-paused.json` | `POST /v1/verify/bulk` | 503 `paused` (needs the kill switch pulled) |
| `submit-error-unavailable.json` | `POST /v1/verify/bulk` | 503 `unavailable` (needs the queue key missing) |
| `submit-error-internal.json` | `POST /v1/verify/bulk` | 500 `internal_error` |

## HAND-WRITTEN for the Phase 3 close-out (module 1 at `fad942e`), to be re-recorded from the real module by the harness

Module 1 gained three additive features (partial Tier 4: `GET /v1/describe`, an idempotent bulk create on an `Idempotency-Key` header, `totals` on the job status) after the recordings above
were made. Nothing below was recorded: it was written from the module's handlers (`control-plane/internal/api/describe.go`, `api.go`) and its `api-contract.md`, and the values come
from the module's own constants (the vocabularies as the adapter's `vocab.go` lists them; `reason` and `smtp_skipped_reason` sorted as the module sorts them). Until the record run
replaces them, the live drift gate would report any difference between these files and the real module, which is what it is for.

| File | Route | Case |
|---|---|---|
| `describe-ok.json` | `GET /v1/describe` | the whole self-description: module `verify`, capabilities (`idempotent_bulk_create` and `job_totals` true; `quote`, `run`, `report` false), seven routes, the schemas, eight vocabularies, ten error codes, and the `idempotency` contract. No `pricing`. A new contract, `Contracts()` case `ok` |
| `job-running.json`, `job-complete.json` | `GET /v1/jobs/{id}` | the recordings above WITH a `totals` object ADDED BY HAND (the only edit of a recorded file in this directory; the rest of each file is the recording): `submitted`, `settled`, `pending`, `duplicates`, `malformed` and the four `by_status` keys, consistent with the file's own `counts` (the job of 220 rows has 20 settled, all `invalid`, and the three repeats the results recording holds; the finished job of 2 rows has both settled `invalid`) |

The replay of an idempotent create (a repeat of the same key and request) is the 202 above again, byte for byte, plus the response header `Idempotent-Replayed: true`: it is no contract of
its own, and a 409 `idempotency_key_reused` is the same error envelope as the others (the adapter's `errorCodeValues` lists the code, and `describe-ok.json` lists it in `error_codes`).

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

`failure_origin` and `smtp_skipped_reason` live in `signals`, on an unknown answer and on an answer that skipped the SMTP stage, and no recorded verdict or results row is either, so a
contract that listed them at a row would constrain nothing. They are published on the describe contract only (module 1 lists every set it has there), which is the first body in this
directory that holds them; the tests also prove the adapter's handling of both on inline bodies. `GET /v1/jobs/{id}/results` `empty_page` lists no row set: it has no row to hold one.

## How the unit tests use them

A test that serves a RECORDED fixture takes what it expects from the file: `recordedStatus`, `recordedCounts`, `recordedRows`, `recordedDoc` and the summaries of `contracts_test.go`
parse the body with `encoding/json` into generic maps, sharing no code with the adapter's decoding, and the test compares the adapter's result with that parse. Where a test needs a body
the recording cannot be (a 202 whose counts a plan predicts: `acceptedFor`), it derives one from the recording, keeping its keys and job id. Mutated bodies are made by editing the
parsed tree (`mutation.apply`), and a mutation that does not apply to the body it is given fails the test as proving nothing. Every comparison has a paired control that changes one value
and must be reported. Cases the recordings do not hold (an unknown row with its origin, a degraded answer with a skip token, `duplicate_of` 0, a malformed row) are inline bodies in the
tests, written from the module's code.

`/healthz` and `/readyz` have no fixture here: the adapter passes them through `modclient`, which owns their shapes (see `UncoveredRoutes`).
