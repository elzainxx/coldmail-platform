# Module 2's real worker in the harness (Phase 3, chunk B8): what the first hosted run showed

**Status.** The stack ran GREEN on its first hosted run: https://github.com/elzainxx/coldmail-platform/actions/runs/44183533432 (2026-11-17, dispatched with `scope=m2worker` at platform head `aeb29ed`; the harness step took 4 min 18 s from banner [1]
to the result line, against about 20 minutes for the full harness). All five `TestM2Worker_*` tests passed, the floor gate counted `declared=5 ran=5 pass=5 skip=0 fail=0`, the log secret check was clean, the exposure check was clean, and the run ended
`HARNESS RESULT: GREEN (scope m2worker: 5 TestM2Worker tests) — module 2 built from bffd424`.

The stack is now part of the full harness. It is switched on in ONE place, `FULL_INCLUDES` in `tests/harness/run.sh` (the lead set it to `m2worker m1node` after this run, in commit `2e577a5`, and raised `tests/floors-harness.txt`). With the stage included the full run
starts the same pieces (the compose override, the `m2worker` build tag in `TEST_TAGS`, the credentials, stage 5c after module 3's workers, the five tests among all the others, the PASS-line and log-secret checks, `m2-worker` in the exposure list). **What this
file does NOT show is that inclusion itself:** the run above is the scoped one, so the stage inside a full run (module 2's tests before module 3's on the shared controllable sink, one more container beside module 3's three workers) is NOT OBSERVED here.
To run only this stage again: `gh workflow run harness.yml --ref <branch> -f scope=m2worker`. To take it out of the full run again, remove `m2worker` from `FULL_INCLUDES` and lower the floor by 5.

**How to read this file.** Every claim below was read from the job log of that run. `L<n>` is a line of the full job log as exported for the review (one line per log line, each prefixed with job, step and timestamp; 1533 lines): download the log from the run
page and the numbers line up. Quoted lines have the timestamp prefix removed. Each item is marked **CONFIRMED** (the run showed what this file expected), **CONTRADICTED** (it showed something else) or **NOT OBSERVED** (the log does not say). A green
run only prints what the stage chooses to print: where a fact rests on an assertion that would have turned the test red, that is said.

Modules built (stage 1, L313-L315): `1fa2550` (module 1), `bffd424` (module 2), `5dde0c0` (module 3), each "tree clean, not shallow". Module 2 is at `bffd424`, the Phase 3 pin; the research and the first version of this file were written against `ff8c302`. The two
differ in five files only (LICENSE, NOTICE, README and two lines of two documents: no code), so every `file:line` of the research still points at the same code.

## 1. Evidence, stage by stage

| Stage | The line the file said to look for, as the run printed it | Verdict |
|---|---|---|
| [2] secrets | L325 `  minted for module 2's real worker: 1 operator key (+ bcrypt hash), 1 worker id, 1 worker token (+ bcrypt hash) — masked, none printed`. No literal key, token or hash appears anywhere in the log (the step's `env:` block shows the deploy keys as `***`). | CONFIRMED |
| [3] build | L829 `  built from the pinned sources (scope m2worker: only migrate-platform migrate-m1 migrate-m2 migrate-m3 m2 m2-worker smtp-ctl); base images are the digests pinned in each Dockerfile`. The worker image built from module 2's own Dockerfile (L532 `RUN apk add --no-cache curl && adduser -D -u 10002 worker`, then `Image coldmail-platform-harness-m2-worker Built`). Stage 3 took 1 min 50 s (21:48:26 to 21:50:17). | CONFIRMED |
| [4] database | the four chains ran as the non-superuser: platform `up: applied 9 migration(s)` (L1127), module 2 `applied 29` (L1185), module 3 `applied 55` (L1211), module 1 `applied 18 of 18 files and a second run was a no-op` (L1214). The scope does not skip them (they are needed by the shared checks); stages 4 and 4b took about 33 s. | CONFIRMED |
| [5] services | L1238-L1240 `Container coldmail-platform-harness-m2-1 Healthy` and `| m2         running   healthy`; the Control was healthy about 4 s after it was created. | CONFIRMED |
| [5c] worker | L1244 `  key files: worker.key is mode 600 owned by uid 10002, control.key and dev-master.key mode 600 owned by uid 10001 (Linux host); worker.crt verifies as a client certificate, control.crt as a server certificate and carries DNS:control`; L1261 `  module 2's worker registered and heartbeated after 0 s (send_managed.boxes, database clock)`; L1262 `m2-worker-1  | worker <id>: registered over mTLS stream; awaiting pushed jobs (buffered outcomes replayed on connect)`; L1263 `m2-1  | control 2026/11/17 21:51:01 workerlink stream: worker registered id="<id>" (token verified, mTLS client cert)`; L1264 `  the worker is registered and heartbeating; the sink answers; module 2's account will name the sink's PRIVATE address (the test asserts the range)`. The stage took 6.0 s (21:50:55 to 21:51:01), 3.5 s of it waiting for the sink's health check. | CONFIRMED |
| [6] tests | the evidence block L1334 `---- module 2's real worker: the test's own log lines (the sink's per-recipient counts, timings, snapshots) ----` with every NOTE line (L1335-L1487); L1488 `  module 2's real worker: 5 TestM2Worker_* tests have a '--- PASS' line`; L1491 `github.com/coldmail-platform/platform/tests/harness declared=5 ran=5 pass=5 skip=0 fail=0 floor=5`; L1495 `  no credential of this run (operator key, worker token, service token, SMTP password) appears in the logs of m2, m2-worker or smtp-ctl`. | CONFIRMED |
| [8] exposure | L1498 `check-exposure: 4 services examined, 0 publish a port to the host, failures: 0`; L1501 `  second instrument: 4 containers inspected, none has a host port binding; network coldmail-platform-harness_private internal=true`. | CONFIRMED |
| result | L1514 `HARNESS RESULT: GREEN (scope m2worker: 5 TestM2Worker tests) — module 2 built from bffd424`. | CONFIRMED |

Every red outcome the earlier version of this file listed for these stages (a mint failure, a missing compose variable, a key file with the wrong owner or mode, a certificate that does not verify, a missing `DNS:control`, a worker that never heartbeats, a sink that is
not healthy, a leaked credential) is **NOT OBSERVED**: none of them happened. They stay in section 6 as the triage guide for a future red run.

## 2. The four outcomes, measured

The dials of module 2 for the run, as every test prints them (for example L1335): `job_max_failed_attempts=2 retry_backoff_schedule={"ladder_seconds":[1,1,1],"max_seconds":1} send_delay_min_sec=0 send_delay_max_sec=0 concurrency_per_user_default=1 dispatch_tick_interval_ms=250`.
The sink's address was `172.18.0.4:1025`, "(a private address)" (L1335): the runner's Docker network is inside 172.16.0.0/12.

| Outcome | Scenario | What the run showed | Measured |
|---|---|---|---|
| `sent` | control (default sink), 8 items (`TestM2Worker_ControlAllSent`) | all 8 `state=sent reason=<null> error_class=<null> provider_message_id=set` (L1358-L1365); totals `total=8 sent=8` (L1357); the sink holds 8 messages, each `RCPT accepted=1` (L1375-L1382) | `POST /v1/run` accepted the 8 jobs in 561 ms (L1341); first send done 0.1 s after, all 8 terminal after 1.5 s; the sends completed 0.13 to 0.33 s apart (mean 0.22 s, one worker, one lane) |
| `bounced` | fault campaign: 2 of 8 recipients rejected with 550 at RCPT TO (`TestM2Worker_SentBouncedAndAttemptsExhausted`) | both `state=bounced reason=bounce_permanent error_class=bounce_permanent provider_message_id=<null>` (L1408, L1411); the sink: `RCPT accepted=0 rejected=1 deferred=0, messages accepted=0` for each (L1421, L1424): tried once, never retried | one SMTP attempt each; the first bounce was reported 0.12 s after the campaign's first send (completed_at 32.021 then 32.141), the second 0.73 s after it (32.747) |
| `failed` / `attempts_exhausted` | same campaign: 2 of 8 recipients deferred with 451 at RCPT TO | both `state=failed reason=attempts_exhausted error_class=transport_4xx provider_message_id=<null> attempt_count=2` (L1409, L1412); in between, `state=queued reason=transport_4xx error_class=transport_4xx attempt_count=1` (L1389, L1392); the sink: `RCPT accepted=0 rejected=0 deferred=2, messages accepted=0` for each (L1422, L1425): exactly the cap, no message; totals `total=8 sent=4 bounced=2 failed=2` (L1406) | first deferral visible 0.3 s after the runs; failed 1.77 s and 2.29 s after the campaign's first send (completed_at 33.792 and 34.307); all 8 terminal after 2.8 s (the poll at which the test saw it). With module 2's defaults the same two items would have taken about half an hour |
| `cancelled` | held campaign of 5 (the sink holds every final reply 7 s), cancelled with one item in flight (`TestM2Worker_CancelStopsSending`) | L1464 `NOTE before the cancel: 1 session(s) in flight [<the first item>] (6823 ms of the hold left), 4 of 5 items not started`; L1465 `NOTE cancel #1 (the owner): cancelled=4 in_flight=1 already_terminal=0; cancel #2 straight after: cancelled=4 in_flight=1 already_terminal=0; the sink holds 1 session(s) at that point; before the cancel returned the sink had answered RCPT TO for [<the same item>]`; L1475 `NOTE the window: from the cancel's return to 10.0 s after it, no recipient other than [<the first item>] was tried`; the in-flight item ends `state=sent` (once, `provider_message_id=set`), the four others `state=cancelled reason=cancelled error_class=<null>`; the sink: item 01 `RCPT accepted=1, messages accepted=1`, items 02-05 `RCPT accepted=0 rejected=0 deferred=0, messages accepted=0` | the cancel came 177 ms after the send reached DATA; the four cancelled items carry one `completed_at` (21:52:18.542564, one transaction); the held send was released 6.64 s later (21:52:25.185) and the campaign was complete 7 s after the cancel; the test took 13.59 s (L1332) |

The held control (`TestM2Worker_ControlHeldCampaignAllSent`, the paired control of the cancel test) sent all 5 items one after the other: completions 7.21 to 7.22 s apart, the sink's `held` 7000 to 7002 ms for every message (L1452-L1456), the campaign complete
after 36.5 s, the test 39.45 s (L1300). The same five items, the same hold and no cancel end with five messages at the sink, so the one message of the cancel test is the cancel's doing.

Across the five tests the sink's message sequence runs 1 to 18 without a gap (8 control, 4 fault, 5 held control, 1 cancel): nothing else sent and nothing was sent twice.

**Heartbeat.** The worker registered within one 2-second step of its start (`after 0 s`, L1261; container `Started` at 21:51:00.9, and the Control logged the registration in the same second, L1263). Its heartbeat interval is 10 s: the Preconditions test found the box row `active, last heartbeat 3.4 s ago` (L1337).

**Dials.** All six were accepted by `PUT /v1/admin/config/{key}` and stored as sent: each test's setup writes a dial, reads it back and stops (`t.Fatalf`) if the stored value differs, and all five tests passed. The log line that lists the values (L1335, L1339, L1384, L1429, L1463) prints
the intended constants; the evidence that the module CONSUMED them is the behaviour: eight sends in 1.5 s (the default `send_delay` of 3 to 7 s per lane would have taken at least 24 s), exhaustion in about 2 s (the default ladder starts at 30 s), `deferred=2` at the sink
(the cap), a deterministic 4/1 cancel split (one lane) and `attempt_count=0` on every sent item (no claim-and-requeue churn). That the global config was RESTORED at the end of each test is NOT OBSERVED: nothing prints the restored values (a failed restore would have turned the test red).

## 3. Where the other three PASS lines are

The stage's own line says 5 tests have a PASS line (L1488), but the job log shows the PASS line of only two: `--- PASS: TestM2Worker_ControlHeldCampaignAllSent (39.45s)` (L1300) and `--- PASS: TestM2Worker_CancelStopsSending (13.59s)` (L1332).
The three that are not printed are `TestM2Worker_Preconditions`, `TestM2Worker_ControlAllSent` and `TestM2Worker_SentBouncedAndAttemptsExhausted`. **They did run and pass.** Their PASS lines are in the test log file on the runner (`h-run.log`) and are not in the job log, because
the stage prints only the last 60 lines of that file (`tail -60`, L1274-L1333, which hold the end of the held control and the whole cancel test) and then only the lines of the evidence block, which keeps a line only when it carries the prefix `m2_worker_test.go:<n>:`. A PASS line has no such
prefix. Three independent facts show the three tests ran to the end and passed:

1. L1488 `module 2's real worker: 5 TestM2Worker_* tests have a '--- PASS' line` is printed by `m2w_require_pass`, which greps the whole file for `^--- PASS: <name> ` once per test name and stops the run (`die`) at the first missing one. Its selftest (`stage-selftest.sh`) proves it goes red for a
   missing, failed or indented line, naming the test.
2. L1491 `declared=5 ran=5 pass=5 skip=0 fail=0 floor=5`: the floor gate counts column-0 `--- PASS|FAIL|SKIP: Test*` lines in the same file against the five names the binary declared.
3. The last statement of each test body printed its NOTE lines, and a test that stopped early (`t.Fatalf`) could not have printed them: Preconditions L1335-L1338 (the last NOTE, the sink's mode after the refused one, is L1338), ControlAllSent L1339-L1383 (ends with the sink counters and the job-rows header),
   SentBouncedAndAttemptsExhausted L1384-L1428 (same).

A gap in what the stage prints (a suggestion, not changed here): `m2w_evidence` could also print the `--- PASS|FAIL|SKIP: TestM2Worker_` lines (so all five PASS lines and their durations are in the log) and the continuation lines of the job-rows NOTE, see item 3 of section 5.

## 4. The unknowns of the research, settled

| Unknown (research section 11) | What the run showed | Verdict |
|---|---|---|
| the worker registers and heartbeats in this compose | L1261-L1263: registered in under 2 s, "token verified, mTLS client cert", heartbeat 3.4 s old at the first test (L1337) | CONFIRMED |
| STARTTLS and AUTH LOGIN against `smtp-ctl` complete with module 2's worker | 8 control sends and every later send reached the sink: messages `seq` 1 to 18 (L1366-L1373, L1415-L1418, L1452-L1456 with `held 7000 ms`, L1481). The sink's own log lines (`smtp: accepted message`) are not printed on a green run | CONFIRMED (by the sink's counters; its log NOT OBSERVED) |
| RCPT 550 reaches `bounced`; RCPT 451 reaches `failed/attempts_exhausted` end to end | see section 2: `bounced` with `bounce_permanent` twice (tried once), `failed` with `attempts_exhausted` and error class `transport_4xx` twice (deferred exactly twice). No module defect was found | CONFIRMED |
| timing: 1 to 3 s per send, 3 to 5 s for the exhaustion, hold plus 2 s for a held send | a send took 0.13 to 0.33 s, the exhaustion about 2 s, and the held campaign was complete 7 s after a cancel made 0.18 s into a 7 s hold | CONTRADICTED, in the safe direction (everything faster than estimated) |
| the cancel split | `cancelled=4 in_flight=1 already_terminal=0`, identical for the second call (L1465) | CONFIRMED (deterministic with one lane; the test still asserts only the partition) |
| the runner's Docker network is in the private ranges | `module 2's sink target is 172.18.0.4:1025 (a private address)` (L1335) | CONFIRMED |
| `send_delay_min_sec` / `send_delay_max_sec` = 0 are accepted | stored as sent (the setup would have stopped otherwise) and consumed (8 sends in 1.5 s) | CONFIRMED |
| the account's state after repeated `transport_4xx` | L1419 `NOTE account of fault after fault: paused=false quarantined=false disabled=false daily_sent_count=10 (every attempt, failed or not, consumes a slot) account_status_score=76`. `daily_sent_count=10` is 4 sent + 2 bounced + 4 deferral attempts; the control's was 8 (L1374, score 100) | CONFIRMED (see section 5, item 6) |
| `docker compose` interpolation of `H_M2_*` | no compose error at stages 3 and 5; the merged override took effect (the operator surface answered, the worker started) | CONFIRMED |

Unknowns added by this chunk:

| Unknown | What the run showed | Verdict |
|---|---|---|
| `concurrency_per_user_default` = 1 removes the claim-and-requeue churn and makes the cancel split deterministic | split 4/1/0 twice (L1465); every sent and cancelled item has `attempt_count=0` and `last_error_class=""` in the job rows (L1295-L1299, L1327-L1331); the cancelled items' `error_class` is `<null>` (L1307-L1310, L1477-L1480) | CONFIRMED |
| `dispatch_tick_interval_ms` = 250 is accepted and consumed | stored as sent; sends 0.13 to 0.33 s apart (a 1000 ms tick could not do that) | CONFIRMED |
| the sink's Message-ID contains the job id | the assertion (an `Errorf` naming the Message-ID) did not fire in any test, and every Message-ID has the form `<uuid@example.test>` (L1366-L1373, for example `message-id "<170f7f10-...@example.test>"`) | CONFIRMED (by the passed assertion; the job id itself is not printed beside it) |
| module 2's job columns used for the evidence lines (`terminal_reason`, `last_error_class`, `failed_attempt_count`, `assigned_box_id`, `cancel_requested_at`) | the rows print for the held control (L1294-L1299) and the cancel test (L1326-L1331): `terminal_reason="cancelled" ... worker=none cancel_requested=true` for the cancelled items, `worker=the harness worker` for the sent ones, `cancel_requested=true` also on the in-flight item. For the control and the fault campaign the rows are NOT in the log (section 5, item 3), so `failed_attempt_count` and `terminal_reason` of the exhausted items are NOT OBSERVED | CONFIRMED for 2 of 4 campaigns; NOT OBSERVED for the other 2 |
| a cancelled item's `error_class` | `<null>` for all four (L1307-L1310, L1477-L1480) | CONFIRMED |
| the single-job report and the campaign report agree on every item | the assertion did not fire; the log shows both formats as the research read them: `completed_at` with microseconds in the single-job report and whole seconds in the campaign report, `provider_message_id` absent (single) versus null (campaign) for items that were not sent | CONFIRMED |

Not asked in the original list, seen anyway and in line with the research: a queued item mid-retry shows `reason=transport_4xx` (L1389); `error_class` persists on a `sending` item after an earlier failure (L1399); a repeat cancel answers the same position (L1465); the
cancelled campaign items carry a `completed_at` (L1307-L1310).

## 5. Surprising or fragile, although the run was green

1. **Three PASS lines and part of the evidence are not in the job log** (section 3). The stage prints `tail -60` of the test log and an evidence filter that keeps first lines only. Nothing is wrong with the tests; the log is thinner than the stage's own checks.
2. **The tail and the evidence block overlap** (L1274-L1333 repeat L1334-L1487 for the held control and the cancel test). Cosmetic: about 60 duplicate lines.
3. **Job rows of two campaigns are missing.** The NOTE `module 2's job rows of ctl ...` (L1383) and `... of fault ...` (L1428) are followed directly by the next test's line: the rows are continuation lines of a multi-line `t.Logf`, which have no `m2_worker_test.go:<n>:` prefix and are dropped by the filter. They show only for the held control (L1294-L1299) and
   the cancel test (L1326-L1331), because those happen to sit inside the `tail -60` window (the held control and cancel evidence blocks, L1462 and L1487, have the same gap). The fault campaign's `failed_attempt_count` and `terminal_reason='attempts_exhausted'` are therefore NOT OBSERVED, although the
   report (`attempt_count=2`) and the sink (`deferred=2`) show the same facts.
4. **A transient disagreement between two reads is by design, not a defect.** At t+1.5 s of the fault campaign the campaign report said `state=sending ... attempt_count=1` for the first deferred item (L1399) while the single-job report, read a few milliseconds later in the same poll, said `state=failed reason=attempts_exhausted attempt_count=2` (L1398). Module 2
   reads the two reports with separate queries; the final comparison happens only after the campaign is complete, and it matched.
5. **The 1 s poll shows gaps that are not module delays.** In the held control item 02 first shows `state=sending` at t+8.1 s although item 01 was sent at t+6.9 s (L1437-L1438); the completion timestamps are 7.21 s apart, which is the 7 s hold plus 0.2 s. Read `completed_at`, not the poll times.
6. **The account score falls with every `transport_4xx`.** After 4 failed attempts the score was 76 (from 100); a bounce does not touch it (the sent and bounced items did not move the control's 100 to anything: L1374). The quarantine threshold is 30 and `autoquarantine_mode` is at its seeded `shadow`, so
   nothing acted. A fault campaign with many more deferrals on one account could approach that threshold; at `shadow` that would only record an observation, but check the account's mode before making the campaign bigger.
7. **`attempt_count=2` on the exhausted items equals the number of failed attempts only because one lane means no churn.** The research warns that `attempt_count` is not an attempt count (it is also bumped by dispatcher-side requeues that never reached SMTP); the test asserts it nowhere, and the dial `concurrency_per_user_default=1` is what makes it look like one.
8. **No retry, warning or timeout margin in the harness part of the log.** There is no `retry`, `not ready`, `denied` or `timed out` line in stages 1 to 8. The only warnings are the runner's: Node 20 deprecation (L1533), `DEP0040 punycode` from the actions (L130, L1516) and a git hint (L69). The worker's own console is not printed on a green run, so whether
   Node warned about an IP address as TLS server name (the research expected it) is NOT OBSERVED. Margins against timeouts: the 7 s hold is 23 % of module 2's 30 s SMTP operation timeout (by design; nothing within 20 % of a limit); the cancel test needs 3.5 s of the hold left and had 6.8 s; the heartbeat the test accepts is under 60 s and was 3.4 s;
   the longest wait (the held control, bound 135 s) finished in 36.5 s; the worker's registration wait (up to 120 s) finished in the first step.
9. **A cosmetic `<no value>`** heads the health column of stage 5's service table (L1239: `| SERVICE    STATE     <no value>`): an old template detail of `docker compose ps --format table`, not from this chunk.
10. **Stage 4 migrates modules 1 and 3 as well** (about 33 s with stage 4b) although the scope starts neither: the shared checks need all four chains. If a faster scope is wanted, that is the place.
11. **What a green scoped run cannot say about the full run:** the stage runs there after module 3's three workers and the shared sink; module 2's tests (alphabetically before module 3's) leave the sink in its default mode and holding nothing (each test's cleanup waits for it to drain), which the scoped run shows for the end of its own last test only
    (`the sink is clean, in its default mode and holds nothing` at the start of every test, L1335 and the four after it).

## 6. If a stage goes red: the triage guide

Each banner reads `================ [<stage>] <title>`; "red" means `HARNESS RED at stage [<name>]: <message>` followed by the tail of every service's log. Send back the whole job log.

| Stage and message | Meaning and cheapest check |
|---|---|
| [2] `could not mint module 2's operator key and worker credentials` | `provision-worker admin-key` printed no `ADMIN_API_KEY=` / `ADMIN_API_KEY_HASH=` line: its output format changed |
| [2] `... must be three different credentials` | two of the service token, the operator key and the worker token are equal: impossible unless the tool is broken |
| [3] `required variable H_M2_... is missing a value` | the env file lacks a line: `m2w_mint` did not run (the `M2W_ON` guard in run.sh) |
| [3] the `m2-worker` build fails at `apk add curl` | the build needs the network and the build network is not the internal one: a runner or registry problem |
| [5c] `module 2's worker.key has (mode, uid) = (...), expected (600 10002)` (or control.key / dev-master.key) | `own-secrets.sh` did not chown (no sudo, no root container) or a later step re-created the key: the container's user would get `permission denied` |
| [5c] `... does not verify ... as a TLS client certificate` / `... as a TLS server certificate` | the extended key usage of the dev certificates changed (`gen-certs.sh` writes `serverAuth, clientAuth`) |
| [5c] `... has no subject alternative name DNS:control` | the worker verifies the NAME `control` (`CONTROL_SERVER_NAME`) while dialling the host `m2`; the certificate must carry it |
| [5c] `the reserved system user is not in public.users` / `could not seed module 2's worker box row` | `platform_0001` did not insert the all-zero user, or the `send_managed.boxes` insert failed (read psql's message: a wrong column would mean the table changed) |
| [5c] `module 2's worker never heartbeated after <time>` | in the service logs: `register REJECTED` = the token hash in the row is not the hash of the worker's token (an escaping error: the SQL uses the raw hash, the env file the `$$`-escaped one); `connect attempt N not ready (...)` with `self-signed certificate` / `Hostname/IP does not match` = TLS names or CA; `permission denied` on `worker.key` = ownership; no worker log = the container did not start |
| [5c] `the controllable SMTP sink (smtp-ctl) did not become healthy` | the sink exits at start when it cannot read module 3's dev sink key (it runs as the harness user's uid) |
| a test: `... resolves to [...] and not one of these is in 10.0.0.0/8 ...` | the runner's Docker network left the three private ranges: every send would fail as `starttls_failed` (class `timeout`) |
| a test: `GET /v1/admin/config/<dial> ... 404` / `PUT ... answered 200 but its value is` | a dial that does not exist at this pin / the module stored something else |
| a control test: items stay `queued` with `waiting_for_capacity` | no worker is connected or the account is not eligible; the failure message prints module 2's health, the sink's counters and the job rows |
| a control test: items cycle `queued` with `transport_4xx` or `timeout` | the SMTP session fails before the end: the sink's and the worker's logs say where; `starttls_failed` is the private-range guard (the account must hold the address, never the name `smtp-ctl`) |
| the fault test: a `defer` item `failed` with an error class other than `transport_4xx` | module 2's classifier did not read the worker's `451 4.7.1 try later` (its code says `transport_4xx`): **flag it as a module finding, never patch module 2** |
| the fault test: the sink's `deferred` is not 2, or a `bounce` item is retried | the cap or the ladder was not applied, or the worker did not report `bounced` for a 550 at RCPT TO |
| the cancel test: `a send STARTED after the cancel` / the sink holds more than one message | cancel did not stop the queue: the finding that matters |
| the cancel test: `too late to cancel while it is in flight` / `the held send finished before the second cancel was made` | the runner is slow: raise `m2wHoldMs` (the hold must stay far under module 2's 30 s operation timeout) |

## 7. What the run does not vouch for

One worker only; no reclaim of a dead worker, no Control restart, no 535 (the quarantine path), no pool with several accounts, no throttle class (a rate word in a 4xx), no reply that never comes (the 30 s timeout), no public internet (the sink's address is private, the opposite of
module 2's production rule), no look at the body bytes (variation is on by default; the sink is read for recipients and ids only), no campaign of more than a handful of items, no job engine (the tests call module 2's routes themselves). The module's own default cadence (a ladder of 30 s,
120 s and 600 s, six attempts) is NOT what ran: the dials were shortened through the admin API and restored afterwards.

## 8. Module defects

None was found. Module 2 did what its code says on every path the run drove, including the two that no module-2 test had ever driven against a real server (a 550 at RCPT TO ending as `bounced`, a 451 ending as `failed/attempts_exhausted`). If a later run shows module 2 doing something its code does not
say, the finding goes in `Docs/phase-3-findings.md` with the run URL and the NOTE lines; the test that found it stays red until the module changes, and module 2 is never edited from here.

## 9. The adapter tests (B9, B10, B11, B13): NOT YET RUN, what to look for

Everything in sections 1 to 8 is about the first five tests, which drive module 2 with raw HTTP. Four more tests (`tests/harness/m2_worker_e2e_test.go`, `m2_worker_e2e_helpers_test.go`, same build tag, the same worker and sink) drive the platform's module 2 ADAPTER
(`internal/adapters/m2` over `internal/adapters/sendapi`) against it, with the platform's money core behind it. They are in `M2W_TESTS`, so the stage requires their PASS lines, the scoped floor counts them (9 in all) and the full harness's floor in `tests/floors-harness.txt` must rise by 4.
**Nothing below has run on a runner**: the section is the look-for list, not a result. Every check that can fail prints a sentence naming the item and the number; the lines to read are the `NOTE e2e` ones (the stage's evidence block prints them: its filter now matches `m2_worker_e2e_test.go`).

How they differ from the first five, so that a red is read correctly: every call to module 2's platform API (quote, run, report, cancel) goes through the adapter, never by hand; the tenant is resolved through `internal/tenant` (login, then platform user, then scope); the money core runs on the LIVE
database as `platform_app` (grant, a platform job, reserve, settle, release): module 2 reads the live `public.users` and `public.job_items`, and the scoped run has no migration binary to build a scratch database with. Each test has its own users, so nothing of another test's money is touched; the checks of the books are
per user and per job, and the two global invariants (`platform.balance_drift`, `platform.reserve_overdrawn`) must be empty on the live database. The four tests run BEFORE the first five (file order) on the same worker, one after the other; each leaves the sink idle.

| Test | What it proves | Lines to look for (after `NOTE e2e `) |
|---|---|---|
| `TestM2Worker_AdapterEndToEndSettlesItemByItem` (B9) | one campaign of 10 through the adapter (6 sent, 2 bounced, 2 exhausted): quote, reserve, run, poll, classify, settle, replay; every item compared three ways (module's report, sink, ledger) and the ledger against `charge.Decide` AND a hand-written oracle | `quote: 10 recipients -> 10 credits (unit 1, tier shared); module 2's own unit_price_credits 1; platform price table map[m2_accepted:1 m2_bounced:1], dearest 1`; `money: granted 1000 credits; reserved 10 for 10 unit(s) at 1`; `run: 10 new jobs (202 queued, class pending) in <ms>`; `report: total=10 sent=6 bounced=2 failed=2 cancelled=0 ...`; ten `item NN <kind> <address>: job <id> reported <state>/<reason>/<error class> ...; sink RCPT accepted=.. rejected=.. deferred=.. messages=..; charge.Decide <kind> "<unit>", hand oracle <kind> "<unit>"; ledger entries <0 or 1>`; `settle: charged 8 item(s) = 8 credits (map[m2_accepted:6 m2_bounced:2]), free 2 (no ledger row ...), unmapped 0, not final 0; the ledger holds 8 charge row(s) = 8 credits`; `books: after the release available 992 reserved 0; platform.reserves 10/8/2; replay wrote charged 0, already-settled 8, free 2`; `controls: m2eCompare reported 10 of 10 corruptions, each for its own reason: ...` |
| `TestM2Worker_AdapterPaginationAcrossPageBoundaries` (B10) | 42 sent items read at page sizes 1, 5, 13, 21, 41, 42 and 100: the right number of pages of the right size (the module echoes the size), a next cursor on every page but the last, and exactly the baseline's (id, state, reason) rows in run order; the single-job reports equal them; two look-ahead walkers are caught by the same check | `baseline: 42 item(s) in 1 page(s) ...`; `singles: 42 single-job reports equal the campaign report's (id, state, reason) rows, in order`; seven `page size N: P page(s) of [sizes], boundaries after item(s) [...]; (id, state, reason) of all 42 items equal the baseline, in order`; `controls: at page size 5 the look-ahead walker delivered 34 of 42 items and the skip-ahead walker 22: the same equality check reported 8 and 20 missing; a duplicate and a reordered pair are reported too` |
| `TestM2Worker_AdapterCrossTenantReadsAndCancelAreRefused` (B11) | tenant B, through its own adapter, gets the module's documented refusals for tenant A's campaign (an empty 200), job items (not found, field `job_item_id`), job id (not found, field `job_id`), run (not found) and cancel (`{0,0,0}`), with none of A's ids, addresses, user, pool or account in the answers; A's own adapter gets A's rows (control); A's campaign still completes, all sent, none cancelled, every message at the sink once; B's own campaign works and A's adapter is refused B's the same way | `cross-tenant (B asks for A): campaign report total=0 items=0 complete=false pages=1; ReportJob of A's 5 items -> not found (job_item_id); ReportJobByID -> not found (job_id); Run -> not found; Cancel -> {... 0 0 0} (empty true); 9 answers scanned for A's ids, addresses, user, pool and account: none`; `cross-tenant control (A asks for A): 5 item(s) of total=5 ...; ReportJob and ReportJobByID answer A's jobs; the leak scan finds 5 of A's ids in A's own report`; `cross-tenant, the other way: B's own campaign of 3 item(s) finished through B's adapter ...` |
| `TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve` (B13) | a held campaign of 5 cancelled through the adapter with one item in flight: the position adds up, the second cancel is the same position, nothing new reaches the sink from the cancel's return until 3 s after the end, the in-flight item is reported once as sent with one message and ONE ledger entry, the four cancelled items are free with NO ledger row, sent + bounced + failed + cancelled = total, the release returns the reserve of the cancelled items, the third cancel is the final position | `quote and reserve: 5 recipients quoted 5 credits; reserved 5 at 1`; `cancel #1: cancelled=4 in_flight=1 already_terminal=0; #2 straight after: cancelled=4 in_flight=1 already_terminal=0; the sink holds 1 session(s); before the cancel returned the sink had answered RCPT TO for [<the first item>]`; `settle round 1 (straight after the cancel): 4 cancelled item(s) -> Classify settle list (charge.Decide: free, rule M2-08), 1 in flight -> not yet; Settle: charged 0, free 4, ledger rows 0`; `the window: from the cancel's return to ~10 s after it, no recipient other than [<the first item>] was tried (the dispatch tick is 250 ms here)`; `settle round 2 (after the end): charged 1 = 1 credits (the in-flight item <id>, unit m2_accepted), free 4 (...money.Settle writes NO ledger row for a free item, so the cancelled items have none), unmapped 0; the ledger holds 1 charge row(s)`; `after the end: 1 sent (exactly one message at the sink, one ledger entry), 4 cancelled (free, no ledger row), totals (total=5 sent=1 ... cancelled=4 ...); release returned 4 credits (reserve 5 - charged 1); balance available 999 reserved 0; cancel #3 {... 4 0 1}` |

Expected time added to the stage (estimates from the first run's figures, UNPROVEN): B9 about 10 s, B10 about 25 s (42 runs, 42 sends at about 0.2 s, 61 page reads and 42 single reads at about 0.1 s each), B11 about 15 s (two held sends of 3 s), B13 about 15 s (the 7 s hold), plus about 2 s of setup and cleanup each: about 70 to 90 s, inside the 3 minutes the brief allows.

What the first run must settle, each with the cheapest check (each is a reading of code, none was measured):

| Unknown | The cheapest check |
|---|---|
| the money core works on the LIVE database from the test (a `platform_app` pool, the first admin as the acting admin, a platform job inserted by the superuser, grant, reserve, settle, release) | the `money:` line of B9 and B13; a refusal names the call (`grant for ...`, `reserve ...`, `Settle refused ...`) |
| `internal/tenant` resolves the login of a freshly inserted user (`platform.listable_users`) and the scope names the same id | the first lines of any of the four: `tenant.ByAuthUser(...)` / `tenant.Scope(...)` in a failure message |
| the adapter's decoding accepts the live module's answers on every path used here: a new run (202), the empty campaign report, a 404 on a job item, a cancel of someone else's campaign | a `Contract` kind in a failure message names the key (`modclient.KindOf`); B11 is where the empty and not-found shapes are first read live |
| the hand oracle and `charge.Decide` agree on every state module 2 reports here (sent, bounced, failed/attempts_exhausted, cancelled) | an `m2eCompare` problem containing `the hand oracle says` |
| `Settle` of a free item writes no ledger row and counts it in `Free` (also on a replay), while a charged item replays as `AlreadySettled` | the `settle:` / `books:` lines of B9; `again.Free` in a failure message |
| the platform's dearest m2 unit price (the reserve rate) equals module 2's own `unit_price_credits` (both seeded 1; two independent dials) | the `quote:` line; a failure says the dials "have drifted apart" |
| page boundaries of the live module at sizes 1, 5, 13, 21, 41, 42, 100 give exactly the expected page shapes (the module echoes the size; a full last page has no next cursor) | the seven `page size N:` lines; a `page size N: page P holds ...` or `... has next cursor` problem |
| B11's A campaign is still in flight when B's cancel is made (two held sends of 3 s: a window of about 6 s against about 2.5 s of calls) | a fatal `A's campaign was already complete (...) before B's cancel: the runner is too slow` (red, never a silent green) |
| B13: the in-flight item ends `sent` (a send held for 7 s is far inside the 30 s operation timeout) and nothing but it reaches the sink | `a send STARTED after the cancel`, or the in-flight item reported as anything but `sent` (the brief allows `failed` too; the test names `sent` because the sink must then hold its message) |

Risks, in the order they would bite: (1) the money is real ledger on the live database: a test that fails halfway leaves a grant and a reserve behind (the books stay consistent, `balance_drift` and `reserve_overdrawn` are empty by construction; nothing deletes them); (2) the four tests run on the one worker beside
the five older ones, so a held send of a failed test delays the next one (each test's cleanup cancels what it started and waits for the sink to drain, up to 45 s); (3) the leak scan of B11 reads the text of the adapter's answers (a struct and error strings), not the module's raw body, which the adapter does not expose; (4) B10 builds its
page-size clients with `sendapi.New` directly, because `m2.Adapter.For` fixes the page size at the module's maximum; (5) the tests need `HARNESS_PLATFORM_APP_PASSWORD` and `HARNESS_ADMIN_DATABASE_URL` in the test container (both are in the base compose file).

## What is in this directory

| File | What |
|---|---|
| `stage.sh` | sourced by run.sh: the credentials (`m2w_mint`), stage 5c, the evidence and PASS-line checks, the log secret check, the scoped run (`m2w_scope_run`); all guarded by `M2W_ON`. `M2W_TESTS` lists the nine tests whose PASS lines are required |
| `stage-selftest.sh` | run by the `gates` workflow (no Docker, no keys): 17 cases for `m2w_mint`, `m2w_require_pass` (its test list is read from `stage.sh`, so a test added there is covered) and `m2w_secret_leak`, each asserting the reason it fails for, plus a control that the verdict function itself reports a wrong expectation |
| `FIRST-RUN.md` | this file |

The tests are `tests/harness/m2_worker_test.go`, `m2_worker_e2e_test.go` and their helper files (build tag `harness && m2worker`); the compose override is `tests/harness/docker-compose.m2worker.yml` (merged, with `--profile m2worker`, only when the stage is on). No module source is modified, and the workflow uploads no artifact
(if one is ever added it must exclude every `secrets/` directory of the module checkouts).
