# Fixtures of the module 1 adapter

Everything under `contract/` is hand-written from the module's code, to be replaced by a recording from the real module in the harness (Phase 3 B6).

They are written from module 1's handlers (`control-plane/internal/api/api.go`, `bulk_rows.go`, `errors.go`, `verdict/assemble.go`) at the pinned commit, and from
`Docs/history/p3-research/m1-api-and-node.md` Part 1. Ids and times are obviously synthetic (a zeroed uuid, 2026-01-01), the addresses are on `.example`, and no file holds a secret,
a host name or an IP address.

One file per route and case; `Contracts()` in `contracts.go` names each file exactly once (a test fails on a stray file and on a dangling name):

| File | Route | Case |
|---|---|---|
| `verify-ok.json` | `POST /v1/verify` | a probed, accepted address |
| `submit-accepted.json` | `POST /v1/verify/bulk` | the 202 for a plan of three rows, one repeating the first |
| `submit-error-validation.json`, `-unauthenticated`, `-too-large`, `-paused`, `-unavailable`, `-internal` | `POST /v1/verify/bulk` | one error envelope per class the adapter reacts to |
| `job-running.json`, `job-complete.json` | `GET /v1/jobs/{id}` | a job in progress, a finished one |
| `status-error-not-found.json` | `GET /v1/jobs/{id}` | absent, or another tenant's job |
| `results-page.json` | `GET /v1/jobs/{id}/results` | a page with a first answer, an unknown with its origin, a repeat (`duplicate_of`), a degraded answer with a skip token and a malformed row |
| `results-empty.json` | `GET /v1/jobs/{id}/results` | the last page: no results, no next cursor |
| `results-error-bad-cursor.json` | `GET /v1/jobs/{id}/results` | a cursor module 1 did not issue |

`/healthz` and `/readyz` have no fixture here: the adapter passes them through `modclient`, which owns their shapes (see `UncoveredRoutes`).
