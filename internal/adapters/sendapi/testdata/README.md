# Recorded shapes for the shared sender body

`contract/` holds the response bodies of a module-3-SHAPED test profile (omitted-until-set keys in the single report, account detail and `totals_by_account`, no `cancelled`, a feasibility block in
the quote), hand-written from the module's code (`Docs/history/p3-research/m3-api.md`), to be replaced by a recording from the real module in the harness (Phase 3 B6). They exist so that the
tests of the shared body prove it serves both modules, not only module 2; module 3's own adapter keeps its own set under its own `testdata/contract/` and may start from these files. Module 2's
set is `internal/adapters/m2/testdata/contract/`, and the shared body's tests read it from there too.

Ids and times are obviously synthetic and there is no secret in any file.

Request ids and cursors are written in the harness's normalised spelling (a run of `a`, a run of `A`): the fixture check of the drift gate reads a long mixed run of characters as a
secret, and a recording of the real module is normalised the same way.

## Phase 3 close-out (all HAND-WRITTEN, like every file here)

The module-3-shaped profile now has the `cancelled` state and a cancel route (C1) and a 409 on run that names its causes (C2), so: `cancel.ok.json` and `cancel.error.validation_failed.json` are new, the campaign
reports carry `totals.cancelled`, `run.error.conflict.json` carries two `details` entries (`campaign_active`, and `queue_cap` with `current` and `limit`), the quote's caps carry `campaign_size_cap`
and `describe.ok.json` lists `cancelled` last in `job_state`, the vocabulary `run_conflict_cause` and `capabilities.plain_text_alternative`. Module 2's set (`internal/adapters/m2/testdata/contract/`) is
read by this package's tests too: its cancel files are the same two shapes. None of these is a recording; the lead re-records every fixture of the three packages from the real modules.

## Phase 4 A1

`contract/report.campaign.error.cursor_forged.json` (a 500 for a cursor that decodes but is not a timestamp and an id) and its contract were removed: no module answers a forged cursor with a 500 any more, so a forged cursor is the 400 of `report.campaign.error.cursor_malformed.json` and a 500 is never read as a bad cursor. The package now holds 19 files.

## Phase 4 D1: three recordings are pending (nothing here is written for them)

The answers of POST /v1/pause and POST /v1/resume and the 409 `job_item_cancelled` of POST /v1/run are contracts that sendapi.PauseContracts DECLARES and sendapi.Contracts LISTS only once the
switch `recordedPauseFixtures` (contracts.go) is true. Their files are pause.ok.json, resume.ok.json and run.error.job_item_cancelled.json (sendapi.PauseFixtureNames); they are not in this directory
or in module 2's or module 3's yet, and none is written by hand: the drift sessions of the harness print them in record mode (CONTRACT_RECORD=1) and cmd/contract-fixtures installs them. This
directory holds a module-3-SHAPED set, so the lead installs here a copy of module 3's recording of each. TestPauseFixturesAreInstalledWhenTheSwitchIsOn and TestPauseFixturesDoNotExistWhileTheSwitchIsOff
keep the switch and the files together; delete this section when the switch is flipped.
