# Fixtures of the module 1 adapter

Everything under `contract/` is one recorded or hand-written response body of module 1, one file per route and case. `Contracts()` in `contracts.go` names each file exactly once (a test
fails on a stray file and on a dangling name). No file holds a secret, a host name or an IP address.

## RECORDED from the real module 1 (10 files)

Recorded by the contract drift session in the harness, scoped record run **44211659227** on **2026-11-18**: the harness made each call against the real module 1 (built from the pinned
commit, running beside the platform, with its real verifier node), printed the live response in a delimited block of its log, and the lead's `cmd/contract-fixtures` wrote the files after
`contractcheck.Normalise` rewrote them. They replaced the hand-written files of the same names (which had the same SHAPE in all seven cases); the unit tests derive every expected value
from the file itself (see below), never from a literal.

| File | Route | Case |
|---|---|---|
| `submit-accepted.json` | `POST /v1/verify/bulk` | the 202 of a raw submission of three rows, two of them malformed (counts 3 / 1 / 0 / 2), so it is NOT the 202 a plan would get: a plan never sends a malformed row (`TestSubmit_ACountThatDisagreesWithThePlanIsAContractError` uses it as exactly that case) |
| `submit-error-unauthenticated.json` | `POST /v1/verify/bulk` | 401 `unauthenticated` |
| `job-running.json` | `GET /v1/jobs/{id}` | a job in progress (220 rows, 10 settled) |
| `job-complete.json` | `GET /v1/jobs/{id}` | a finished job |
| `status-error-not-found.json` | `GET /v1/jobs/{id}` | 404 `not_found` (absent, or another tenant's job) |
| `status-error-unauthenticated.json` | `GET /v1/jobs/{id}` | 401 `unauthenticated` (declared after the run showed the adapter depends on it: printed as an undeclared call, `UNDECLARED-m1-1`) |
| `results-empty.json` | `GET /v1/jobs/{id}/results` | contract case `empty_page`, but what the real module answered is the LAST page of a 220-row job: 220 rows, `next_cursor` `""`. A page with nothing on it is not recorded; the unit tests build one from this file by emptying `results` |
| `results-error-bad-cursor.json` | `GET /v1/jobs/{id}/results` | 400 `validation_failed`, detail `cursor` / `malformed` |
| `results-error-unauthenticated.json` | `GET /v1/jobs/{id}/results` | 401 `unauthenticated` (`UNDECLARED-m1-2`) |
| `results-error-not-found.json` | `GET /v1/jobs/{id}/results` | 404 `not_found` (`UNDECLARED-m1-3`) |

### What `Normalise` rewrote

`contractcheck.Normalise` makes a recording safe to commit and comparable later. Keys, key order and structure are unchanged; numbers, booleans, null and every other string are
untouched. What it rewrites:

- every UUID becomes `00000000-0000-4000-8000-00000000000N`, numbered by order of FIRST APPEARANCE IN THAT FILE. A job answer's `job_id` is the first UUID of its body, so it is `...0001`
  in every recorded job fixture, and so is the `request_id` of every recorded error envelope. That is why the unit tests' one job id constant, `testJob`, is `...0001`: the adapter
  refuses an answer about another job than the one it asked for.
- every RFC 3339 timestamp becomes `2026-01-01T00:00:00Z`.
- a string under a key that names an id, cursor, hash or the like, and any long opaque string, becomes one repeated character of the same length. So the `item_id` of a recorded result
  is `aaaaaaaaaa`, not the label that was sent, and rows of equal label length share one value: the recorded item_ids are NOT unique and are not keys to settle with.
- a string under a key that names a secret becomes `REDACTED` (none of these bodies has one).

The files are written with two-space indentation, the keys in the order the module wrote them, and end with one newline.

## HAND-WRITTEN from the module's code (7 files), to be replaced by a recording from the real module in the harness (Phase 3 B6)

Not yet exercised by the real run ("7 contracts still unexercised" in the record run's report). They are written from module 1's handlers (`control-plane/internal/api/api.go`,
`bulk_rows.go`, `errors.go`, `verdict/assemble.go`) at the pinned commit and from `Docs/history/p3-research/m1-api-and-node.md` Part 1. Their ids and times are synthetic by hand,
following the same convention (`...0001` for the job), and their addresses are on `.example`.

| File | Route | Case |
|---|---|---|
| `verify-ok.json` | `POST /v1/verify` | a probed, accepted address |
| `results-page.json` | `GET /v1/jobs/{id}/results` | a page with a first answer, an unknown with its origin, a repeat (`duplicate_of`), a degraded answer with a skip token and a malformed row |
| `submit-error-validation.json` | `POST /v1/verify/bulk` | 400 `validation_failed` (a duplicate item_id) |
| `submit-error-too-large.json` | `POST /v1/verify/bulk` | 413 `payload_too_large` (needs 100,001 rows to provoke) |
| `submit-error-paused.json` | `POST /v1/verify/bulk` | 503 `paused` (needs the kill switch pulled) |
| `submit-error-unavailable.json` | `POST /v1/verify/bulk` | 503 `unavailable` (needs the queue key missing) |
| `submit-error-internal.json` | `POST /v1/verify/bulk` | 500 `internal_error` |

## How the unit tests use them

A test that serves a RECORDED fixture takes what it expects from the file: `recordedStatus`, `recordedCounts`, `recordedRows` and the summaries of `contracts_test.go` parse the body with
`encoding/json` into generic maps, sharing no code with the adapter's decoding, and the test compares the adapter's result with that parse. Where a test needs a body the recording
cannot be (a 202 whose counts a plan predicts; a page with nothing on it) it derives one from the recording (`acceptedFor`, `withoutResults`), keeping its keys and job id. Every
comparison has a paired control that changes one value and must be reported.

`/healthz` and `/readyz` have no fixture here: the adapter passes them through `modclient`, which owns their shapes (see `UncoveredRoutes`).
