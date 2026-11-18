# Module 2 recorded shapes

Every file under `contract/` is a response body of module 2 (`coldmail-send-managed` at the pin in `tools/modules.lock`), one file per route and per response whose shape differs. `m2.Contracts()`
names each file once (`Route`, `Case`, `Fixture`, `Status`, the `Shape` the adapter decodes it with and the `Vocab` it maps from). The tests serve each file from an `httptest` server and decode
it through the adapter's real code, refuse a file no contract names and a contract whose file is missing, and refuse mutated copies (a required key removed, a key's type changed, a state
outside the vocabulary).

Twenty files, two origins. The test `TestReadmeNamesTheRecordedAndHandWrittenFixtures` holds this list to the directory: a file named in the wrong section, in both or in neither is a failure.

## RECORDED (10 files, from the real module)

Recorded from module 2 running as a real service with a real worker and a controllable SMTP sink, by the contract drift session in record mode: harness run 44211663760, 2026-11-18, and
normalised by `contractcheck.Normalise` (uuids become `00000000-0000-4000-8000-0000000000NN` by first appearance, instants `2026-01-01T00:00:00Z`, provider ids a run of `A`, request ids a
run of `a`; `TestRecordedFixturesAreNormalised` pins that). Their shapes were identical to the hand-written bodies they replaced in all ten cases; their CONTENT is not what the
file names suggest, and the tests read what each file says instead of assuming it:

- `quote.ok.json`: a quote for 10 recipients at 1 credit each.
- `run.created.json`: the 202 of a new job, state `queued`.
- `run.replay.json`: the 200 of a repeated run, with the state the job had when the replay was made: `sending`, not `sent`.
- `run.error.not_found.json`: 404 `not_found`, details `job_item_id`.
- `report.job.sent.json`: a single-job report of a job that ended `bounced` (reason and error class `bounce_permanent`), not `sent`; the name is the contract case.
- `report.job.error.not_found.json`: 404 `not_found` of a report asked by job item id: the module names the selector the caller used, `job_item_id` (the research could only infer that).
- `report.campaign.page.json`: one page, no next cursor, ten items (six sent, two bounced, two failed with `attempts_exhausted`).
- `report.campaign.last_page.json`: one page, no next cursor, five items (one sent, four cancelled).
- `report.campaign.empty.json`: NOT an empty campaign: the same ten-item campaign while one item was still queued (reason and error class `transport_4xx`). The real run did not ask for a
  campaign the module does not know, so the empty answer is not recorded; `TestAnEmptyPageIsNothingKnown` derives it from `report.campaign.page.json` by taking the content out.
- `cancel.ok.json`: the module's answer to a cancel of a campaign with nothing left to cancel: `{0, 0, 0}`. A position with counts in it is that recording with its counts filled in, in
  `TestCancelIsAPositionAndASecondCancelIsANoOp`.

A cursor between two pages was not recorded either (every recorded report fits in one page): the tests cut a recorded page in two (`TestRecordedPagesSettleAsTheChargeTableSays`), so the
cursor path runs over real items with a cursor value that is not a recording.

## HAND-WRITTEN (10 files, the contracts the real run did not reach)

Written by hand from the module's code (the Phase 3 research `Docs/history/p3-research/m2-api-and-worker.md` and the handlers it cites), to be replaced by a recording when a run reaches the
response (Phase 3 B6). Ids and times are obviously synthetic and there is no secret in any of them. These stay UNPROVEN against the real module until then.

- `run.error.validation_failed.json`: 400 with details `user_id` `required`.
- `run.error.unauthenticated.json`: 401.
- `run.error.conflict.json`: 409 `conflict`, the back-pressure of a second active campaign.
- `run.error.paused.json`: 503 `paused`.
- `run.error.internal_error.json`: 500.
- `report.job.queued.json`: a single-job report of a job waiting for a retry (reason and error class `transport_4xx`).
- `report.campaign.error.cursor_malformed.json`: 400 with details `cursor` `malformed`.
- `report.campaign.error.cursor_forged.json`: 500 for a cursor that decodes but is not a timestamp and an id.
- `describe.ok.json`: the module's self-description.
- `cancel.error.validation_failed.json`: 400 with details `campaign_id` `required`.
