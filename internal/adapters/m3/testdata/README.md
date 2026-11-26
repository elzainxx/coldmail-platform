# Fixtures for module 3's adapter

`contract/` holds one response body per route and case that the adapter relies on: the shared four routes and describe (the same names `internal/adapters/sendapi` uses), module 3's
feasibility quote, and the account routes (list, get, create, the test button, bulk add, events). Each file is named by exactly one entry of `m3.Contracts()` and every entry names an existing file (a test
holds both). `m3.ContractFixtures()` serves them from the binary (`fixtures.go`), for the live drift gate.

Ids and times are synthetic or normalised, addresses are in the documentation, private or special-use ranges, and there is no secret in any file. Two lists below say which files are which;
`TestReadmeNamesWhichFixturesAreRecorded` holds the lists to the directory, so a file cannot move from one to the other without this document saying so.

## Recorded

These 46 files are RECORDINGS of the real module 3 in the harness, normalised by `contractcheck.Normalise`: every uuid of a file becomes `00000000-0000-4000-8000-00000000000N` (N counting by first
appearance in that file), every timestamp `2026-01-01T00:00:00Z`, every request id a run of `a`. They come from two full record runs, both of 2026-11-18:

- Record run 44395876225 (the Phase 3 close-out D1 run: module 3 at its close-out pin, the From gate on in the adapter tests) recorded 46 files of the 49 then in the directory (the three that are
  hand-written below are the ones it did not reach); one of the 46, `report.campaign.error.cursor_forged.json` (the 500 for a cursor that decodes but is not a timestamp and an id), was removed in Phase 4 A1
  together with its contract, because no module answers a forged cursor with a 500 any more (module 3 and module 2 answer the 400 of `report.campaign.error.cursor_malformed.json`), so 45 of the 48 files of
  the directory now are its recordings. The file is what that run recorded for 41 of the 45: 18 are files the run CHANGED (seven earlier recordings whose shape changed: `accounts.create.created.json`,
  `accounts.create.created_failed.json`, `accounts.get.ok.json`, `accounts.list.page.json`, `accounts.list.empty.json`, `accounts.test.ok.json`, `report.job.queued.json`; and eleven files that were
  hand-written before: `accounts.create.error.unprocessable.json`, `accounts.events.last_page.json`, `accounts.test.indeterminate.json`, `cancel.ok.json`, `describe.ok.json`, `quote.ok.json`,
  `quote.infeasible.json`, `report.campaign.page.json`, `report.campaign.last_page.json`, `report.campaign.empty.json`, `run.error.conflict.json`); two were hand-written and the module answered
  exactly what they held (`cancel.error.validation_failed.json`, `quote.feasible.json`), so they are recordings now; and 21 were already recordings of the earlier run and came out byte for byte the same.
- Record run 45706128441 (Phase 4 A3, the full harness in record mode on platform `fe595f3`) recorded `accounts.test.unavailable.json`: the test button's answer when the credential check could not run (`verification` `unavailable` with `verify_error`, `from_alignment` `indeterminate` with `from_dns_unavailable`), which `TestM3Adapter_TestButtonSaysUnavailableWhenTheCredentialCheckCannotRun` makes the module give on purpose. The file is that run's printed block installed byte for byte; it replaces the hand-written one.
- Record run 44225747596 (the third record run of `platform` Phase 3 B6) recorded the earlier files. Four of them keep their recording of that run, because the answer of the new run had the same shape
  (only values differed): `accounts.bulk.ok.json`, `accounts.create.error.validation_failed.json`, `report.job.error.not_found.json`, `report.job.sent.json`.

The file a recording is filed under is the harness's classification of the answer it saw, not a promise about what the answer holds. The harness files an answer under the contract of its route and status
that the answer's shape fits best (`contractcheck.Match`: the nearest fixture, and on a tie the first contract nothing was filed under yet) and, for each contract, keeps the observation with the fewest positions
that tell nothing, then the most positions, then the SHORTEST body (`contractcheck`'s pick). A contract case name is therefore a SHAPE FAMILY (`ok`, `failed`, `last page`, `empty`: the family of answers of one
shape), and the real module's answer of that shape can hold another state than the name says. Read from the files themselves, these are not what their names suggest (the matcher that files them is not part of
this package, and none of this was changed here; `TestReadmeClaimsAboutFilesWhoseContentDiffersFromTheirNameAreTrue` holds each claim below to the file):

- `report.job.sent.json` is a BOUNCED item (550 at the recipient), not a sent one.
- `report.campaign.last_page.json` is a page with a cursor (2 of 5 items, all queued, page size 2), not a last page; `report.campaign.empty.json` is a page with a cursor (1 of 45 items, a sent one, page size 1),
  not an empty campaign. `report.campaign.page.json` is the one that is a whole campaign in one page (12 items, no cursor).
- `accounts.list.last_page.json` is an EMPTY listing and `accounts.list.empty.json` is a last page of ONE account (a quarantined one); `accounts.list.page.json` is a page of two accounts with a cursor.
- `accounts.list.error.validation_failed.json` names the cursor (`cursor` / `malformed`) and `accounts.list.error.cursor_malformed.json` names the tenant (`user_id` / `required`): the
  two names are the other way round.
- `accounts.get.ok.json` is a QUARANTINED account (reason `auth_failure`, with its instant) whose credential check shows verified and whose From verdict is aligned, not a healthy one: there is no recording
  of a healthy account.
- `accounts.create.created.json` is a create whose credential verified and whose From verdict is `unaligned_warned` and already acknowledged (`acknowledged_at` is set); `accounts.create.created_failed.json`
  is a create whose credential check failed with `verify_connect_failed` and an empty provider text (not `verify_auth_failed`), with the same warned and acknowledged From verdict.
- `accounts.test.ok.json` is a click whose credential check FAILED (`verify_tls_failed`, an empty provider text) with an aligned From address, not a click that verified: there is no recording of a click
  that verified. `accounts.test.failed.json` is a failed credential (`verify_auth_failed`, the provider's text) with an ALIGNED From address (no warning, no `error` key). `accounts.test.indeterminate.json`
  is the TLS failure with a From check that could not get an answer from DNS (`from_dns_unavailable`).
- `quote.ok.json` is a feasible quote for 0 recipients (campaign size cap 2, no warning, a projection of 0 s) and `quote.feasible.json` one for 100 recipients that carries the `long_duration` warning (170 s
  against a threshold of 60 s). `quote.infeasible.json` is a quote for ONE recipient that is infeasible for ONE reason, the queue depth cap (a cap of 3 that already holds 3): the campaign size cap (50000)
  is not hit, so there is no recording of a quote that names the campaign cap or of one with two reasons.
- `run.error.conflict.json` is the 409 of the queue cap alone (`current` 3, `limit` 3), not one that names two causes.
- `accounts.create.error.duplicate.json` is a real duplicate (it names `smtp_username` in `details`), and `accounts.create.error.conflict.json` is the module's opaque 409 (no details).
- `accounts.events.last_page.json` is the three events of an account that was created, verified and quarantined by an enforced health action (a last page, no cursor). Its ids are numbered by first
  appearance in THE FILE, which opens with the event's own id: the event is `...01`, its account `...02` and its tenant `...03` (in an account file the account is `...01` and the tenant `...02`), so the
  contract case builds its client for the tenant the file holds.

The tests do not hard-code what these files say. They read it from the file (an independent parse of the document, field by field), name ids after the normalisation (`norm1`, `norm2`),
and where a test needs a KIND of answer they pick the recording by what it holds (a whole campaign in one page, a listing with accounts in it) and fail naming the files when none holds it.
What the recordings lack is built FROM the recordings (a healthy account, a paused one, a created account with each verification code, a click that verified, a quote that breaks both caps, an empty
campaign, a stranger's quote, an injected provider text or acknowledgment instant, a bulk batch with an internal reason), and each such test has the recording itself as its control.

- `run.created.json`
- `run.replay.json`
- `run.error.validation_failed.json`
- `run.error.unauthenticated.json`
- `run.error.not_found.json`
- `run.error.paused.json`
- `run.error.conflict.json`
- `report.job.sent.json`
- `report.job.queued.json`
- `report.job.error.not_found.json`
- `report.campaign.page.json`
- `report.campaign.last_page.json`
- `report.campaign.empty.json`
- `report.campaign.error.cursor_malformed.json`
- `cancel.ok.json`
- `cancel.error.validation_failed.json`
- `describe.ok.json`
- `quote.ok.json`
- `quote.feasible.json`
- `quote.infeasible.json`
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
- `accounts.create.error.unprocessable.json`
- `accounts.test.ok.json`
- `accounts.test.failed.json`
- `accounts.test.indeterminate.json`
- `accounts.test.error.rate_limited.json`
- `accounts.test.error.conflict.json`
- `accounts.test.error.not_found.json`
- `accounts.bulk.ok.json`
- `accounts.bulk.error.batch_too_large.json`
- `accounts.bulk.error.conflict.json`
- `accounts.bulk.error.validation_failed.json`
- `accounts.events.empty.json`
- `accounts.events.last_page.json`
- `accounts.test.unavailable.json`

## Hand-written

These 2 files are hand-written (`Docs/history/p3-research/m3-api.md`, checked against the module at the pinned commit), to be replaced by a recording when the harness reaches them. Neither
record run reached these responses (the record runs produced the other 46):

- `run.error.internal_error.json`: a 500 on run needs a fault inside module 3, which the harness does not provoke.
- `accounts.events.page.json`: a page with a next cursor and a PAUSE event (`pause_reason` `daily_limit`). The contract matcher gives a live body to the fixture it differs from least, and a page of this
  module never carries a pause reason (the harness does not pause an account) or a cursor (a page of 200), so every populated page lands on `accounts.events.last_page.json` (or, for a page of one event, on
  the empty one) and this case stays unexercised: the file is allowlisted for the drift gate.

They use the same normalised ids as the recordings of the account files (`00000000-0000-4000-8000-000000000001` for the first, `...02` for the tenant) so that one tenant serves every account file. Their request ids
and cursors are the normalised forms too (a run of `a`): contractcheck's no-rot check reads a long opaque run as secret-shaped.

- `run.error.internal_error.json`
- `accounts.events.page.json`

## Phase 4 D1: three recordings are pending (nothing here is written for them)

The answers of module 3 to POST /v1/pause and POST /v1/resume ({campaign_id, state, paused}: a pause and its repeat are the same bytes) and its 409 `job_item_cancelled` on POST /v1/run are contracts that
m3.PauseContracts DECLARES and m3.Contracts LISTS only once the switch `recordedPauseFixtures` (sendapi/contracts.go) is true. Their files are pause.ok.json, resume.ok.json and
run.error.job_item_cancelled.json; they are not in this directory yet, and none is written by hand: the drift session in record mode (zz_m3_adapter_contract_drift_test.go, CONTRACT_RECORD=1) prints them
and cmd/contract-fixtures installs them. When they are installed and the switch is flipped, list them under "Recorded" with the record run's id and delete this section
(TestReadmeNamesWhichFixturesAreRecorded is red until then).
