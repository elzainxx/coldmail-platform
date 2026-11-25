# Module 2 recorded shapes

Every file under `contract/` is a response body of module 2 (`coldmail-send-managed` at the pin in `tools/modules.lock`), one file per route and per response whose shape differs. `m2.Contracts()`
names each file once (`Route`, `Case`, `Fixture`, `Status`, the `Shape` the adapter decodes it with and the `Vocab` it maps from). The tests serve each file from an `httptest` server and decode
it through the adapter's real code, refuse a file no contract names and a contract whose file is missing, and refuse mutated copies (a required key removed, a key's type changed, a state
outside the vocabulary).

All nineteen files are recordings. The test `TestReadmeNamesTheRecordedAndHandWrittenFixtures` holds this list to the directory: a file named in the wrong section, in both or in neither is a failure.

## RECORDED (19 files, from the real module)

Recorded from module 2 running as a real service with a real worker and a controllable SMTP sink, by the contract drift sessions in record mode on the runner, 2026-11-18: the full record run
44225747596 and the scoped record run 44225754836 (an earlier scoped run, 44211663760, recorded the first ten), then the Phase 3 close-out's full record run 44395876225 at module 2 `dc2b532`
(the pin after the close-out's B1 to B4). Which file comes from which (each is a byte-for-byte copy of what that run printed, checked against the log):

- run 44395876225 (module 2 `dc2b532`, twelve files): `cancel.error.validation_failed`, `describe.ok` (the one that now carries `capabilities.plain_text_alternative: true`, the module's own answer, no longer an edit of
  an older recording), `report.campaign.empty`, `report.campaign.error.cursor_malformed`, `report.job.queued`, `run.created`, `run.error.conflict`, `run.error.internal_error`, `run.error.not_found`,
  `run.error.paused`, `run.error.unauthenticated`, `run.error.validation_failed`;
- the earlier runs (seven files): `cancel.ok`, `quote.ok`, `report.campaign.last_page`, `report.campaign.page`, `report.job.error.not_found`, `report.job.sent`, `run.replay`. Run 44395876225 recorded the same
  routes and cases again (the recorder keeps the answer with the fewest gaps, then the most positions, then the shortest body), with other counts, ids or states (a cancel that cancelled none where the earlier
  one cancelled four, a quote for five where the earlier was for ten, a middle page of one item where the earlier held 41): the same shape, a different instance. The earlier recording was kept because it holds the
  richer case the tests were written against (they read every number from the file, so either would pass, and neither is edited);
- `report.campaign.error.cursor_forged` was removed in Phase 4 A1: no module answers a forged cursor with a 500 any more (module 2 has answered the 400 of `report.campaign.error.cursor_malformed` since `dc2b532`, module 3 does now), so the shared contract that pinned the old 500 was removed with its file.

They are normalised by `contractcheck.Normalise`: uuids become
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
- `report.campaign.empty.json`: a campaign of five items on one page (page size 200, no next cursor): one sent and four `cancelled` (reason `cancelled`), all final, `progress.is_complete` true: not an empty campaign.
- `report.campaign.error.cursor_malformed.json`: 400, details `cursor` `malformed`.
- `describe.ok.json`: the module's self-description (`send_managed`, `v1`, seven job states, twelve item reasons, seven error codes, `capabilities.plain_text_alternative` true).
- `cancel.ok.json`: the module's answer to a cancel: four cancelled, none in flight, one already terminal.
- `cancel.error.validation_failed.json`: 400, details `campaign_id` `required`.

Three things the recordings do not hold, and what the tests do about each (none of it is presented as a recording):

- A recorded page that names a next cursor is not a whole campaign, and the page after it was not recorded. `TestARecordedMiddlePageIsWalkedThroughItsCursor` serves the recorded page, then a
  tail DERIVED from it (the same page with the missing items copied from its first item under fresh ids, no next cursor) for the request that carries exactly the recorded cursor; a stub that serves
  the first page again is refused as the same item on two pages.
- An empty answer (a campaign the module does not know) was not asked for. `TestAnEmptyPageIsNothingKnown` DERIVES one from a recorded page by taking its content out; the recording named
  `empty` is asserted for what its file holds, whatever that is.
- A cancel with something left to cancel is a recording whose counts the test fills in (`TestCancelIsAPositionAndASecondCancelIsANoOp`), for the position that is not empty.

## HAND-WRITTEN (none)

No fixture of module 2 is hand-written: the record runs reached all nineteen contracts (the describe fixture was edited by hand for a while during the close-out, with `capabilities.plain_text_alternative: true` added, and is the module's own recorded answer since run 44395876225).
A contract added to `m2.Contracts()` before a run reaches it is hand-written until then, and this section names it.

## One file's provenance

`contract/report.campaign.empty.json` was, until the close-out, a response module 2 gave to an unknown campaign (no items, totals zero) that had been installed under this name by hand because the matcher then filed that answer under another contract name. Run 44395876225 recorded this contract natively (the page of a campaign whose items were cancelled), the file is that recording byte for byte, and the old installation is gone. The name stays a shape family, not a promise about the content.
