# Module 2 recorded shapes

Every file under `contract/` is a response body of module 2 (`coldmail-send-managed` at the pin in `tools/modules.lock`), one file per route and per response whose shape differs. `m2.Contracts()`
names each file once (`Route`, `Case`, `Fixture`, `Status`, the `Shape` the adapter decodes it with and the `Vocab` it maps from). The tests serve each file from an `httptest` server and decode
it through the adapter's real code, refuse a file no contract names and a contract whose file is missing, and refuse mutated copies (a required key removed, a key's type changed, a state
outside the vocabulary).

Nineteen of the twenty files are recordings and one is a recording that was then edited by hand (named in the last section). The test `TestReadmeNamesTheRecordedAndHandWrittenFixtures` holds this list to the directory: a file named in the wrong section, in both or in neither is a failure.

## RECORDED (19 files, from the real module)

Recorded from module 2 running as a real service with a real worker and a controllable SMTP sink, by the contract drift sessions in record mode on the runner, 2026-11-18: the full record run
44225747596 and the scoped record run 44225754836 (an earlier scoped run, 44211663760, recorded the first ten). They are normalised by `contractcheck.Normalise`: uuids become
`00000000-0000-4000-8000-0000000000NN` by first appearance, instants `2026-01-01T00:00:00Z`, provider ids a run of `A`, request ids a run of `a` (`TestRecordedFixturesAreNormalised` pins that).
The tests read what each file says (an id, a count, a state, a cursor) from the file and never from a constant, so a re-recording changes no test. What the files hold, as of this recording:

- `quote.ok.json`: a quote for 10 recipients at 1 credit each.
- `run.created.json`: the 202 of a new job, state `queued`.
- `run.replay.json`: the 200 of a repeated run, with the state the job had when the replay was made: `sending`.
- `run.error.validation_failed.json`: 400, details `user_id` `required`.
- `run.error.unauthenticated.json`: 401.
- `run.error.not_found.json`: 404 `not_found`, details `job_item_id`.
- `run.error.conflict.json`: 409 `conflict`, no details.
- `run.error.paused.json`: 503 `paused`.
- `run.error.internal_error.json`: 500 `internal_error`.
- `report.job.sent.json`: a single-job report of a job that ended `bounced` (reason and error class `bounce_permanent`); the file name is the contract case, not the state.
- `report.job.queued.json`: a single-job report of a job that is `queued`, with no reason and no error class yet.
- `report.job.error.not_found.json`: 404 `not_found` of a report asked by job item id: the module names the selector the caller used, `job_item_id`.
- `report.campaign.page.json`: one whole page (no next cursor, page size 200), ten items taken while the campaign was running: six sent, two bounced, one failed with `attempts_exhausted` and
  one still queued after a `transport_4xx`.
- `report.campaign.last_page.json`: a page that DOES name a next cursor (41 of the 42 items of a campaign, all sent, page size 41): a middle page, not a last one. The page after it was not recorded.
- `report.campaign.empty.json`: a campaign of ten items that all reached a final state (six sent, two bounced, two failed with `attempts_exhausted`), no next cursor: not an empty campaign.
- `report.campaign.error.cursor_malformed.json`: 400, details `cursor` `malformed`.
- `report.campaign.error.cursor_forged.json`: 500 `internal_error` for a cursor that decodes but is not a timestamp and an id (the module answers a 500 where its documentation says 400).
- `cancel.ok.json`: the module's answer to a cancel: four cancelled, none in flight, one already terminal.
- `cancel.error.validation_failed.json`: 400, details `campaign_id` `required`.

Three things the recordings do not hold, and what the tests do about each (none of it is presented as a recording):

- A recorded page that names a next cursor is not a whole campaign, and the page after it was not recorded. `TestARecordedMiddlePageIsWalkedThroughItsCursor` serves the recorded page, then a
  tail DERIVED from it (the same page with the missing items copied from its first item under fresh ids, no next cursor) for the request that carries exactly the recorded cursor; a stub that serves
  the first page again is refused as the same item on two pages.
- An empty answer (a campaign the module does not know) was not asked for. `TestAnEmptyPageIsNothingKnown` DERIVES one from a recorded page by taking its content out; the recording named
  `empty` is asserted for what its file holds, whatever that is.
- A cancel with something left to cancel is a recording whose counts the test fills in (`TestCancelIsAPositionAndASecondCancelIsANoOp`), for the position that is not empty.

## HAND-WRITTEN (1 file)

- `describe.ok.json`: the module's self-description (`send_managed`, `v1`, seven job states, twelve item reasons, seven error codes), RECORDED by the runs above and then EDITED BY HAND for the Phase 3 close-out (B3): `capabilities.plain_text_alternative: true` was added. The lead re-records it from the real module at the new pin, and then it is a recording again.

A contract added to `m2.Contracts()` before a run reaches it is hand-written until then, and this section names it.

## One file's provenance

`contract/report.campaign.empty.json` is a response the real module 2 gave to an unknown campaign (an empty campaign: no items, totals all zero), recorded by the second record run (scoped run 44219160097, 2026-11-18), where the matcher still filed it under another contract name. In the later runs the stale fixture of the `campaign empty` contract kept attracting populated pages, so the recording from that run was installed under this name by hand. It is a recording, not hand-written.
