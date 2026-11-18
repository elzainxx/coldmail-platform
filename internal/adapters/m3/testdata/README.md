# Fixtures for module 3's adapter

`contract/` holds one response body per route and case that the adapter relies on: the shared four routes and describe (the same names `internal/adapters/sendapi` uses), module 3's feasibility
quote, and the account routes (list, get, create, the test button, bulk add, events). Each file is named by exactly one entry of `m3.Contracts()` and every entry names an existing file (a test
holds both). `m3.ContractFixtures()` serves them from the binary (`fixtures.go`), for the live drift gate.

Ids and times are synthetic or normalised, addresses are in the documentation, private or special-use ranges, and there is no secret in any file. Two lists below say which files are which;
`TestReadmeNamesWhichFixturesAreRecorded` holds the lists to the directory, so a file cannot move from one to the other without this document saying so.

## Recorded

These 24 files are RECORDED from the real module 3 in the harness (record run 44211654839, 2026-11-18, the drift session of `platform` Phase 3 B6) and normalised by
`contractcheck.Normalise`: every uuid of a file becomes `00000000-0000-4000-8000-00000000000N` (N counting by first appearance in that file), every timestamp `2026-01-01T00:00:00Z`, every
request id a run of `a`. 23 of them had the shape of the hand-written file they replaced; the one that differed is `report.job.sent.json` (the real item carries `error_class`, `failed_at`
and `smtp_response`, which the module omits until set).

Some recordings are not what their file name says, because the harness recorded what the module answered when it was driven, and the name is the contract's:

- `report.job.sent.json` is a BOUNCED item (550 at the recipient), not a sent one.
- `accounts.list.empty.json` is the last page of three accounts (`next_cursor` null), not an empty page; `accounts.list.page.json` is a full page of 200 accounts with a cursor.
- `accounts.create.error.duplicate.json` is the module's opaque 409 ("not available for your account right now", no `details`), not a duplicate (a duplicate names `smtp_username` in
  `details`): that is what was recorded, and this document does not know why the duplicate answer was not. The duplicate path is covered by a test that adds that detail to this recording, and the bulk recording does
  hold a duplicate row.
- `accounts.test.failed.json` is a failed credential with an ALIGNED From address (no warning, no `error` key).

The tests do not hard-code what these files say: they read it from the file, or name ids after the normalisation (`norm1`, `norm2`), and build the variants the recordings lack (an
acknowledgment instant, a provider message id, the test button's `error` key, a bulk batch with an internal reason) from the recordings themselves.

- `quote.ok.json`
- `quote.infeasible.json`
- `run.created.json`
- `run.replay.json`
- `run.error.not_found.json`
- `run.error.conflict.json`
- `report.job.sent.json`
- `report.job.error.not_found.json`
- `report.campaign.page.json`
- `report.campaign.empty.json`
- `accounts.list.page.json`
- `accounts.list.empty.json`
- `accounts.get.ok.json`
- `accounts.get.error.not_found.json`
- `accounts.create.created.json`
- `accounts.create.created_failed.json`
- `accounts.create.error.duplicate.json`
- `accounts.test.ok.json`
- `accounts.test.failed.json`
- `accounts.test.error.conflict.json`
- `accounts.test.error.not_found.json`
- `accounts.bulk.ok.json`
- `accounts.bulk.error.conflict.json`
- `accounts.events.empty.json`

## Hand-written

These 21 files are still hand-written from the module's code (`Docs/history/p3-research/m3-api.md`, checked against the module at the pinned commit), to be replaced by a recording when the
harness reaches them: the drift session did not reach these responses (the describe, a feasible quote with a warning, the validation, unauthenticated, paused and internal-error answers of run, a queued
report, the last page of a campaign report and its cursor errors, the validation and cursor errors of the list, the validation, conflict and unprocessable answers of create, an unavailable or
rate-limited test button, the bulk validation and size errors, and a page of an account's events). They use the same normalised ids as the
recordings (`00000000-0000-4000-8000-000000000001` for the first, `...02` for the tenant of an account file) so that one tenant serves every file.

The describe body is trimmed (eight of the eighteen extension routes, one schema stub) because the adapter reads only what `modclient.Describe` reads.

- `describe.ok.json`
- `quote.feasible.json`
- `run.error.validation_failed.json`
- `run.error.unauthenticated.json`
- `run.error.paused.json`
- `run.error.internal_error.json`
- `report.job.queued.json`
- `report.campaign.last_page.json`
- `report.campaign.error.cursor_malformed.json`
- `report.campaign.error.cursor_forged.json`
- `accounts.list.last_page.json`
- `accounts.list.error.validation_failed.json`
- `accounts.list.error.cursor_malformed.json`
- `accounts.create.error.validation_failed.json`
- `accounts.create.error.conflict.json`
- `accounts.create.error.unprocessable.json`
- `accounts.test.unavailable.json`
- `accounts.test.error.rate_limited.json`
- `accounts.bulk.error.batch_too_large.json`
- `accounts.bulk.error.validation_failed.json`
- `accounts.events.page.json`
