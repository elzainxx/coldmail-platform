# Fixtures for module 3's adapter

`contract/` holds one response body per route and case that the adapter relies on: the shared four routes and describe (the same names `internal/adapters/sendapi` uses), module 3's
feasibility quote, and the account routes (list, get, create, the test button, bulk add, events). Each file is named by exactly one entry of `m3.Contracts()` and every entry names an existing file (a test
holds both). `m3.ContractFixtures()` serves them from the binary (`fixtures.go`), for the live drift gate.

Ids and times are synthetic or normalised, addresses are in the documentation, private or special-use ranges, and there is no secret in any file. Two lists below say which files are which;
`TestReadmeNamesWhichFixturesAreRecorded` holds the lists to the directory, so a file cannot move from one to the other without this document saying so.

## Recorded

These 33 files are RECORDED from the real module 3 in the harness (full record run 44225747596, 2026-11-18, the third record run of `platform` Phase 3 B6) and normalised by
`contractcheck.Normalise`: every uuid of a file becomes `00000000-0000-4000-8000-00000000000N` (N counting by first appearance in that file), every timestamp `2026-01-01T00:00:00Z`, every
request id a run of `a`.

The file a recording is filed under is the harness's classification of the answer it saw, not a promise about what the answer holds. Read from the files themselves, these are not what their
names suggest (the matcher that files them is not part of this package, and none of this was changed here):

- `report.job.sent.json` is a BOUNCED item (550 at the recipient), not a sent one.
- `report.campaign.last_page.json` is a page with a cursor (2 of 5 items, all queued, page size 2), not a last page; `report.campaign.empty.json` is a page with a cursor (44 of 45 items, page
  size 44), not an empty campaign. `report.campaign.page.json` is the one that is a whole campaign in one page (12 items, no cursor).
- `accounts.list.last_page.json` is an EMPTY listing and `accounts.list.empty.json` is a last page of six accounts (the two names are the other way round); `accounts.list.page.json` is a full
  page of 200 accounts with a cursor.
- `accounts.list.error.validation_failed.json` names the cursor (`cursor` / `malformed`) and `accounts.list.error.cursor_malformed.json` names the tenant (`user_id` / `required`): the
  two names are the other way round.
- `quote.ok.json` and `quote.feasible.json` are the same feasible quote for 100 recipients; the second carries the `long_duration` warning (170 s against a threshold of 60 s), the first has none.
- `accounts.test.failed.json` is a failed credential with an ALIGNED From address (no warning, no `error` key); `accounts.test.ok.json` carries no alignment message.
- `accounts.create.error.duplicate.json` is a real duplicate (it names `smtp_username` in `details`), and `accounts.create.error.conflict.json` is the module's opaque 409 (no details).

The tests do not hard-code what these files say. They read it from the file (an independent parse of the document, field by field), name ids after the normalisation (`norm1`, `norm2`),
and where a test needs a KIND of answer they pick the recording by what it holds (a whole campaign in one page, a listing with accounts in it) and fail naming the files when none holds it.
What the recordings lack is built FROM the recordings (an empty campaign, a stranger's quote, an acknowledgment instant, a provider message id, the test button's `error` key, a bulk batch with an
internal reason), and each such test has the recording itself as its control.

- `run.created.json`
- `run.replay.json`
- `run.error.validation_failed.json`
- `run.error.unauthenticated.json`
- `run.error.not_found.json`
- `run.error.paused.json`
- `report.job.sent.json`
- `report.job.queued.json`
- `report.job.error.not_found.json`
- `report.campaign.error.cursor_malformed.json`
- `report.campaign.error.cursor_forged.json`
- `accounts.list.page.json`
- `accounts.list.last_page.json`
- `accounts.list.empty.json`
- `accounts.list.error.validation_failed.json`
- `accounts.list.error.cursor_malformed.json`
- `accounts.get.ok.json`
- `accounts.get.error.not_found.json`
- `accounts.create.created.json`
- `accounts.create.created_failed.json`
- `accounts.create.error.validation_failed.json`
- `accounts.create.error.duplicate.json`
- `accounts.create.error.conflict.json`
- `accounts.test.ok.json`
- `accounts.test.failed.json`
- `accounts.test.error.rate_limited.json`
- `accounts.test.error.conflict.json`
- `accounts.test.error.not_found.json`
- `accounts.bulk.ok.json`
- `accounts.bulk.error.batch_too_large.json`
- `accounts.bulk.error.conflict.json`
- `accounts.bulk.error.validation_failed.json`
- `accounts.events.empty.json`

## Hand-written

These 14 files are hand-written (the first four from the module's code, the other ten for the Phase 3 close-out, see below) (`Docs/history/p3-research/m3-api.md`, checked against the module at the pinned commit), to be replaced by a recording when the
harness reaches them. The record runs did not reach these responses:

- `run.error.internal_error.json`: a 500 on run needs a fault inside module 3, which the harness does not provoke.
- `accounts.create.error.unprocessable.json`: the 422 that refuses an unacknowledged From-gate warning (`from_alignment_warning_unacknowledged`) exists only when the From-gate verdict is a
  warning, and the harness runs the gate off (research m3-api section 12.2), so no create there can reach it.
- `accounts.test.unavailable.json`: the test button answers `unavailable` only when its credential check cannot run (`verify_error`, `verification_unavailable`) and `indeterminate` only when
  the From check cannot get an answer from DNS; the harness has a worker and no From gate, so neither happens there. It is allowlisted for the drift gate.
- `accounts.events.page.json`: no populated events listing was ever observed by the drift session (only an empty page), so the page with events in it is hand-written; it will be allowlisted.

Ten more are hand-written for the Phase 3 close-out (module 3 at the pin `8855a52`, the lead re-records every one from the real module and then moves it to the list above): the two files of the new route `POST /v1/cancel` (`cancel.ok.json`, `cancel.error.validation_failed.json`), a 409 of run that names its causes (`run.error.conflict.json`: another campaign active and the queue cap, the cap with `current` and `limit`; it REPLACES the recording of the unexplained 409, which an older module would answer), and seven RECORDINGS that were edited by hand to what the new module answers: `quote.ok.json`, `quote.feasible.json` (`caps.campaign_size_cap` added), `quote.infeasible.json` (a quote of 100001 recipients now holds a `campaign_cap` reason beside the queue cap, each with `cause`, `current` and `limit`), `report.campaign.page.json`, `report.campaign.last_page.json`, `report.campaign.empty.json` (`totals.cancelled` added) and `describe.ok.json` (`capabilities.plain_text_alternative`, `cancelled` last in `job_state`, the vocabulary `run_conflict_cause`, the three new `quote_reason_code` values; its `schemas` still say what the module said before C6). Where a file is a recording that was edited, nothing but the additions named here was changed.

They use the same normalised ids as the recordings (`00000000-0000-4000-8000-000000000001` for the first, `...02` for the tenant of an account file) so that one tenant serves every file. Their request ids and cursors are the normalised forms too (a run of `a`): contractcheck's no-rot check reads a long opaque run as secret-shaped.

- `run.error.internal_error.json`
- `accounts.create.error.unprocessable.json`
- `accounts.test.unavailable.json`
- `accounts.events.page.json`
- `describe.ok.json`
- `quote.ok.json`
- `quote.feasible.json`
- `quote.infeasible.json`
- `run.error.conflict.json`
- `report.campaign.page.json`
- `report.campaign.last_page.json`
- `report.campaign.empty.json`
- `cancel.ok.json`
- `cancel.error.validation_failed.json`
