# First run of module 2's real worker in the harness (Phase 3, chunk B8): what to look for

Nothing here has run. Docker, Postgres and the harness cannot run on the author's machine, so every statement about run-time behaviour below is a reading of module 2's code at the pin (`ff8c302`, `tools/modules.lock`) and is
marked UNPROVEN where it matters. Only a GitHub-runner run settles anything. This file is for the person who dispatches that run and reads its log.

## How to run it

The whole stage is DORMANT in the normal harness. With `H_SCOPE` unset (every push, the weekly schedule, a plain dispatch) nothing of it compiles into the test binary, merges a compose file, starts a container, mints a credential or prints a line;
the full harness is exactly what it was before this chunk. It runs only when asked for:

1. Push the branch, then dispatch only the new stage: `gh workflow run harness.yml --ref <branch> -f scope=m2worker`. That scope is Postgres, the four migration chains, the first admin, module 2's Control, its real worker, the
   controllable sink and the five `TestM2Worker_*` tests, then the exposure check. It does not start module 1, module 3, the platform service or the probe, so it needs far less than the 20 minutes of the full run.
2. When the scoped run is green on the runner, include the stage in the full harness by changing ONE line of `tests/harness/run.sh`: `FULL_INCLUDES=""` to `FULL_INCLUDES="m2worker"` (add `m1node` beside it when that stage is proven too), and in the same
   commit raise `tests/floors-harness.txt` by 5 (the five tests then compile into the full binary: build tag `m2worker`, added to `TEST_TAGS` by run.sh). Then stage 5c runs after module 3's workers, the tests run among all the others, the
   PASS-line, evidence and log-secret checks run, and `m2-worker` joins the exposure list.
3. To rehearse the Go side without Docker: `go vet -tags harness ./tests/harness/` (the files are NOT compiled: they carry `//go:build harness && m2worker`) and `go vet -tags "harness m2worker" ./tests/harness/` (they are) are both clean
   here, and the `go` workflow vets `-tags "harness m1node m2worker"` so the dormant files cannot rot. `bash -n` on `run.sh` and `m2worker/stage.sh` is clean and `m2worker/stage-selftest.sh` (15 cases, run by the `gates` workflow) passes.
   They prove it compiles and parses, nothing more.

The run is red at the first failed assertion and prints the tail of every service's log (`---- service logs (tail) ----`). Send back the whole job log; the sections below say which parts matter.

## What was added (and what was not touched)

| File | What |
|---|---|
| `tests/harness/docker-compose.m2worker.yml` | an override file run.sh merges after `docker-compose.yml` ONLY when the stage is on (`M2W_ON=1`; the service also carries `profiles: [m2worker]`): the service `m2-worker` (built from the pinned module-2 checkout with the module's own `services/worker/Dockerfile`), the one environment line `ADMIN_API_KEY_HASHES` on `m2` (without it the operator surface is not mounted), the `HARNESS_M2_*` variables of the test container. No `image:`, no published port. |
| `tests/harness/m2worker/stage.sh` | sourced by run.sh: the credentials (`m2w_mint`), stage 5c (`m2w_stage_5c`), the evidence and PASS-line checks, the log secret check, the scoped run (`m2w_scope_run`). |
| `tests/harness/m2worker/stage-selftest.sh` | run by the `gates` workflow's selftest step (no Docker, no keys): 15 cases prove `m2w_mint` (four env lines, hash escaping, every credential masked before it is written, equal or missing credentials refused), `m2w_require_pass` (a missing, failed or indented line is red, naming the test) and `m2w_secret_leak` (red, naming the service, never printing the log or the credential), plus a control that the verdict function itself reports a wrong expectation. NOT covered there: `m2w_preflight` (file owner and mode, certificate names), 5c and the scoped run: they need Linux and Docker. |
| `tests/harness/m2_worker_test.go`, `m2_worker_helpers_test.go` | the five tests and their helpers (build tag `harness && m2worker`: not compiled by a plain `-tags harness` build, by the `db-tests` workflow or by the full harness). |
| shared files, small edits only | `run.sh` (`FULL_INCLUDES`, `M2W_ON` / `TEST_TAGS` / the compose merge, the call sites guarded by `M2W_ON`, the stage-6 compile tags, the exposure variable, the header comments, `H_SCOPE`), `.github/workflows/harness.yml` (the `scope` input and `H_SCOPE`), `.github/workflows/gates.yml` (the selftest in the loop), `.github/workflows/go.yml` (one vet line with all the scope tags). `tests/floors-harness.txt` is NOT changed by this chunk: the floor stays that of the full harness. |

No module source is modified. The workflow uploads no artifact; if one is ever added it must exclude every `secrets/` directory of the module checkouts.

## Stage by stage

Each banner reads `================ [<stage>] <title>`. "Red" means `HARNESS RED at stage [<name>]: <message>` followed by the service logs.

### [2] secrets (stage name `secrets`)

Look for, after module 3's line: `  minted for module 2's real worker: 1 operator key (+ bcrypt hash), 1 worker id, 1 worker token (+ bcrypt hash) — masked, none printed`.

| Outcome | Meaning |
|---|---|
| that line | the operator key, the worker's id and token were minted with module 2's own `provision-worker` and appended (mode 600) to the env file |
| red `could not mint module 2's operator key and worker credentials` | `provision-worker admin-key` printed no `ADMIN_API_KEY=` / `ADMIN_API_KEY_HASH=` line: its output format changed |
| red `... must be three different credentials` | two of the service token, the operator key and the worker token are equal: impossible unless the tool is broken |
| a literal key, token or hash anywhere in the log | a defect: `::add-mask::` runs before the value is written anywhere and nothing prints it, so it should read `***`. The keys die with the run, but the log is public: fix the leak |

The key files themselves were made by module 2's tools earlier in this stage (`gen-certs.sh`, `gen-secrets.sh`, `own-secrets.sh`, which also PROVES each key is readable by its own container uid and by no other on Linux).

### [3] build

Look for `built from the pinned sources` (scoped: `(scope m2worker: only migrate-platform migrate-m1 migrate-m2 migrate-m3 m2 m2-worker smtp-ctl)`).

| Outcome | Meaning |
|---|---|
| `required variable H_M2_... is missing a value` from compose | the env file lacks a line: `m2w_mint` did not run (run.sh edit lost) |
| the `m2-worker` build fails at `apk add curl` | the build needs the network and the build network is not the internal one: a runner or registry problem, not a defect |
| `an image failed to build` for any other service | the same as in any earlier run |

### [5] services (scoped: `scope m2worker: module 2's Control only`)

`m2` must come up healthy. A healthy Control says nothing about the operator surface; that is proven by the tests (`GET /v1/admin/health` answers 200 with the operator key) and by 5c.

### [5c] module 2's real worker

The banner is `[5c] module 2's real worker (its box row seeded BEFORE it starts, a heartbeat newer than the insert) and the controllable sink as its SMTP target`. Look for these lines, in order:

1. `  key files: worker.key is mode 600 owned by uid 10002, control.key and dev-master.key mode 600 owned by uid 10001 (Linux host); worker.crt verifies as a client certificate, control.crt as a server certificate and carries DNS:control`
2. `  module 2's worker registered and heartbeated after N s (send_managed.boxes, database clock)`, then up to six log lines mentioning registration (the Control's `worker registered id=...`, the worker's `registered over mTLS stream`)
3. `  the worker is registered and heartbeating; the sink answers; module 2's account will name the sink's PRIVATE address (the test asserts the range)`

| Outcome | Meaning and cheapest check |
|---|---|
| red `module 2's worker.key has (mode, uid) = (...), expected (600 10002)` (or control.key / dev-master.key) | `own-secrets.sh` did not chown (no sudo, no root container) or a later step re-created the key. The container's user would get `permission denied`. Look at the stage-2 `own-secrets` output |
| red `... worker certificate does not verify ... as a TLS client certificate` / `... Control certificate does not verify ... as a TLS server certificate` | the extended key usage of the dev certificates changed (`gen-certs.sh` writes `serverAuth, clientAuth`) |
| red `... has no subject alternative name DNS:control` | the worker verifies the NAME `control` (`CONTROL_SERVER_NAME`) while dialling the host `m2`; the certificate must carry it (`gen-certs.sh` line `leaf control "control" "DNS:control,DNS:localhost,IP:127.0.0.1"`) |
| red `the reserved system user is not in public.users` | `platform_0001` did not insert the all-zero user; the box row's foreign key needs it |
| red `could not seed module 2's worker box row` | the `INSERT INTO send_managed.boxes (id, user_id, token_hash, status)` failed: read psql's message above (a wrong column name would mean module 2's table changed) |
| red `module 2's worker never heartbeated after <time>` | the worker did not register within 120 s. In the service logs: `register REJECTED` means the token hash in the row is not the hash of the worker's token (an escaping error: the SQL uses the raw hash, the env file the `$$`-escaped one); `connect attempt N not ready (...)` with `self-signed certificate` / `Hostname/IP does not match` / `certificate` means TLS (names or CA); `permission denied` on `worker.key` means ownership (the preflight should have caught it); no worker log at all means the container did not start (`depends_on` m2 healthy, image build) |
| red `the controllable SMTP sink (smtp-ctl) did not become healthy` | the sink exits at start when it cannot read module 3's dev sink key (it runs as the harness user's uid) |

### [6] tests (and, in the scoped run, the scoped stage 6)

After the Go tests: `  ---- module 2's real worker: the test's own log lines (the sink's per-recipient counts, timings, snapshots) ----` followed by every `NOTE` line, then on success
`  module 2's real worker: 5 TestM2Worker_* tests have a '--- PASS' line` and `  no credential of this run (operator key, worker token, service token, SMTP password) appears in the logs of m2, m2-worker or smtp-ctl`.

Read the NOTE lines in this order. Every number below is an estimate from module 2's code (UNPROVEN); the run replaces it. While a campaign runs the test polls module 2's campaign report every second (page size 200, every page walked) and, for the first
item of each kind, the single-job report as well; every change of an item's state prints as `NOTE t+  N.Ns <tenant> item NN <kind> <address>: state=... reason=... error_class=...` (campaign report) or `... single-job report: ...`. At the deadline the failure
message prints the last campaign report, the sink's held sessions and per-recipient counters, module 2's health document and module 2's own job rows (read as the superuser).

1. **`NOTE preconditions: run <hex>; the worker <uuid> is active; module 2's sink target is <private address>:1025 ...; the dials are job_max_failed_attempts=2 retry_backoff_schedule={...} ...`** (one per test). A red before it:
   `... resolves to [...] and not one of these is in 10.0.0.0/8 ...` means the runner's Docker network is outside the three private ranges: the worker would validate the sink's self-signed certificate and every send would fail as `starttls_failed` (class `timeout`).
   `GET /v1/admin/config/<dial> ... 404` names a dial that does not exist at this pin. `PUT ... answered 200 but its value is` means the module stored something else.
2. **`TestM2Worker_Preconditions`**: the credential controls (the platform token on the operator surface is 401, the operator key on the platform surface is 401, no credential is 401), the dial controls (an unknown dial 404, `job_max_failed_attempts` = 0 is 400 and leaves 2), the sink's strictness control. A red here is infrastructure, not behaviour.
3. **`TestM2Worker_ControlAllSent`** (the paired control: the SAME eight-item campaign, the sink in its default mode): expect `NOTE t+ ...s ctl item NN ...: state=queued ...` then `state=sent`, `NOTE ctl: every item terminal and the campaign complete after ...`, `NOTE sink ... accepted=1 rejected=0 deferred=0, messages accepted=1` for all eight, and the `NOTE sink message ...` lines (one per message: seq, from, subject, message-id). Expected time: 1 to 3 s per item at the 250 ms tick. If this is red the worker or the SMTP dialogue is broken, and the next test proves nothing. Reds to expect here, with the cheapest check:
   - items stay `queued` with reason `waiting_for_capacity` (the diagnostics block prints module 2's health, `boxes`, and the job rows): no worker is connected or the account is not eligible. `dispatch_paused` would mean the rung is not `serving`.
   - items go `queued` with `transport_4xx`/`timeout` over and over: the SMTP session fails before the end. The sink's log (`smtp: TLS handshake failed`, `smtp: AUTH refused`) and the worker's log say where. `starttls_failed` is the private-range guard: the account must hold the address, never the name `smtp-ctl`.
   - `the sink's message has the Message-ID "..." which does not contain the job id`: the worker assembles the message differently from what module 2's code says (it is built as `<job id@sending domain>`); a finding, not a harness fault.
   - `a second POST /v1/run answered ...`: the idempotency of `run` (200, the same job, its current state) differs from module 2's code.
4. **`TestM2Worker_SentBouncedAndAttemptsExhausted`** (the fault scenario: four accepted, two rejected with 550, two deferred with 451, disjoint address sets, cap of two failed attempts, ladder `[1,1,1]` capped at 1 s):
   - expect `ok` items `state=sent`, `bounce` items `state=bounced reason=bounce_permanent error_class=bounce_permanent`, `defer` items first `state=queued reason=transport_4xx` and then, 1 to 3 s later, `state=failed reason=attempts_exhausted error_class=transport_4xx`;
   - the sink counters: `ok` accepted=1 messages=1; `bounce` rejected=1 (tried once) messages=0; `defer` deferred=2 (exactly the cap) messages=0;
   - the totals line `total=8 sent=4 bounced=2 failed=2 cancelled=0 in_flight=0 pending=0 waiting_for_capacity=0`;
   - the account lines (`paused=false quarantined=false disabled=false`, `daily_sent_count` around 10: every attempt, failed or not, consumes a slot).
   - **This is the path no module-2 test has ever driven against a real server (research item: "UNPROVEN end to end").** If the run is red here and the control was green, the candidate causes, each readable from the NOTE lines and the job rows, are:
     - a `defer` item `failed` with `error_class` other than `transport_4xx` (for example `unknown`): module 2's classifier did not read the worker's `451 4.7.1 try later`. Module 2's code says `transport_4xx` (`classify.go` rows 4 and 5; the text has no rate word). **FLAG IT as a module finding; do not patch module 2.**
     - the sink's `deferred` is not 2 (3 or more): the cap was not applied (`job_max_failed_attempts` read once, or the dial is not what the code consumes) or a retry happened that was not a failed attempt.
     - a `bounce` item retried or `deferred`/`failed`: the worker's RCPT 550 handling (`smtp.js` line `if (rcpt.code === 550 ...) throw new SMTPError("bounced", ...)`) did not report `bounced`.
     - items stuck `queued` with `transport_4xx` for long: the ladder dial is not what the retry uses; the job rows print `failed_attempt_count` and the diagnostics print the dial values read back.
5. **`TestM2Worker_ControlHeldCampaignAllSent`** (the paired control of the cancel test: five items, every send held 7 s at the end of DATA, no cancel): expect five `sent`, each message `held ~7000 ms`, total about 40 s. A `the sink held the final reply ... for N ms, want about 7000` error means the sink's delay mode did not apply and "in flight" in the next test means nothing.
6. **`TestM2Worker_CancelStopsSending`** (last: one worker takes one job at a time). Expect, in order:
   - `NOTE before the cancel: 1 session(s) in flight [<address>] (about 6800 ms of the hold left), 4 of 5 items not started`;
   - `NOTE cancel #1 (the owner): cancelled=4 in_flight=1 already_terminal=0; cancel #2 straight after: cancelled=4 in_flight=1 already_terminal=0; the sink holds 1 session(s) at that point; before the cancel returned the sink had answered RCPT TO for [<the same address>]`. With one worker and one lane the split is deterministic in theory (UNPROVEN: the test asserts only that the three numbers add up to 5, that at least one item is in flight and one cancelled, and that the second cancel equals the first; the split is a NOTE);
   - `NOTE the window: from the cancel's return to N s after it, no recipient other than [<address>] was tried`;
   - the final states: the in-flight item `sent` (it finished and reported truthfully, once), four `cancelled` with reason `cancelled`; the sink holds exactly one message, to the in-flight address;
   - a third cancel `cancelled=4 in_flight=0 already_terminal=1`, and a re-run of every item that answers 200 with the same job and its final state.
   A red `a send STARTED after the cancel` or `the sink holds 5 message(s)` is the one that matters: cancel did not stop the queue. `too late to cancel while it is in flight` / `the held send finished before the second cancel was made` means the runner is slow: raise `m2wHoldMs` (the hold must stay far under module 2's 30 s operation timeout).

The tests run about two minutes in all (estimate). Every test restores the global dials and the sink's mode when it ends and cancels what it started, so a failed test does not leave a held send under the next one.

### [8] exposure

`check-exposure.sh` lists `postgres m2 m2-worker smtp-ctl` (scoped run), or `m2-worker` in addition to the usual services in a full run that includes the stage (`M2W_EXPOSE`); a normal full run does not list it, because nothing starts it. The worker publishes no port and sits on the internal network.

### Scoped run result

`HARNESS RESULT: GREEN (scope m2worker: 5 TestM2Worker tests) — module 2 built from <sha>` followed by the NOT CHECKED list of this stage. The scoped run never runs the probe, the outage stages, the leader stage or the ledger check.

## The unknowns of the research, and the cheapest check for each

| Unknown (research section 11) | The cheapest check in the first run |
|---|---|
| the worker registers and heartbeats in this compose | stage 5c: the line `module 2's worker registered and heartbeated after N s` or the service logs of `m2` and `m2-worker` |
| STARTTLS and AUTH LOGIN against `smtp-ctl` complete with module 2's worker | `TestM2Worker_ControlAllSent`: eight `sent`, the sink's `smtp: accepted message` lines in the dumped `smtp-ctl` log |
| RCPT 550 reaches `bounced`, RCPT 451 reaches `failed/attempts_exhausted` end to end | `TestM2Worker_SentBouncedAndAttemptsExhausted`: the report states and the sink's `rejected=1` / `deferred=2` counters |
| timing (1 to 3 s per send, 3 to 5 s for the exhaustion, hold plus 2 s for a held send) | the `NOTE t+N.Ns` lines and the `every item terminal ... after N` line of each test |
| the cancel split | the `NOTE cancel #1` line (only the partition and the end state are asserted) |
| the runner's Docker network is in the private ranges | the first assertion of every test (its failure message names the three ranges) |
| `send_delay_min_sec` / `send_delay_max_sec` = 0 are accepted | the PUT in `setDials` answering 200 and reading back 0 (a 400 would name the key) |
| the account's state after repeated `transport_4xx` | the `NOTE account of fault after fault: paused=... quarantined=... disabled=...` line (asserted: all false) |
| `docker compose` interpolation of `H_M2_*` | stages 3 and 5 (a missing variable stops compose with its name) |

Unknowns added by this chunk (each a reading, not a measurement):

| Unknown | The cheapest check |
|---|---|
| `concurrency_per_user_default` = 1 removes the claim-and-requeue churn of one worker with three lanes (research risk 6: `assigned` to `queued`, `last_error_class` `unknown`), and so makes the cancel split deterministic (4 cancelled, 1 in flight) | the `NOTE cancel #1` line; the job rows after each test (`attempt_count` stays small, `last_error_class` empty for sent and cancelled items) |
| `dispatch_tick_interval_ms` = 250 is accepted and consumed | the PUT read back; the per-item timings |
| the sink's recorded Message-ID contains the job id (module 2 builds `<job id@sending domain>`; the worker sends the header as given) | an `Errorf` naming the Message-ID in the control tests |
| module 2's job columns used for the evidence lines (`terminal_reason`, `last_error_class`, `failed_attempt_count`, `assigned_box_id`, `cancel_requested_at`) | a `NOTE could not read module 2's job rows` line (non-fatal) instead of the rows |
| a cancelled item's `error_class` | printed, not asserted |
| the single-job report and the campaign report agree on every item | an `Errorf` `the two reports disagree` |

## What the run does NOT vouch for (also printed by the run)

One worker only; no reclaim of a dead worker, no Control restart, no 535 (the quarantine path), no pool with several accounts, no throttle class (a rate word in a 4xx), no reply that never comes (the 30 s timeout), no public internet (the sink's
address is private, which is the opposite of module 2's production rule), no look at the body bytes (variation is on by default and the sink is read for recipients and ids only), no campaign of more than a handful of items, no job engine
(the tests call module 2's routes themselves). The module's own default cadence (a ladder of 30 s, 120 s and 600 s, six attempts) is NOT what ran: the dials were shortened through the admin API for the run and restored afterwards.

## Module defects: flagged, never patched

If the first run shows module 2 doing something its code does not say (for example a 451 that does not end as `failed/attempts_exhausted`, a cancel that lets a send start, a `bounced` that is retried, a report that disagrees with the sink), the
finding goes in `Docs/phase-3-findings.md` with the run URL and the NOTE lines that show it. The test that found it stays red until the module changes; it is never weakened and module 2 is never edited from here.
