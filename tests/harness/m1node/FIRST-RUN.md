# The module-1 node stack: what to look for, and what the first run showed

**Status.** The stack ran GREEN on its first hosted run: scope `m1node`, platform head `aeb29ed`, run
<https://github.com/elzainxx/coldmail-platform/actions/runs/44183527048>. Module 1 was built at `1fa2550` (the research read `4adc316`), the node from module 1's own Dockerfile
(Ruby 3.4.12, truemail 3.3.1, thin 1.8.2, eventmachine 1.2.7), on Docker 29.4.2 and Compose v5.1.3. All six stages passed and all nine `TestM1Node_*` tests passed
(`declared=9 ran=9 pass=9`). The whole scope took 246 s, of which 90 s was the image build and about 104 s the module-1 stages. The stack is now part of the full harness:
`FULL_INCLUDES="m2worker m1node"` in `tests/harness/run.sh` (flipped by the lead after this run), so `m1n_run` runs after stage 10 of every `full` run and the file below is what to read
when one of its stages goes red. **That full-scope path has not run yet** (this run is the `m1node` scope only: see "Surprising or fragile", items 12 and 13).

How to read this file. Each table row says what to look for and what it means (written before the run), and now what the run showed:
**CONFIRMED** (the run showed what was predicted), **CONTRADICTED** (it showed something else) or **NOT OBSERVED** (the run did not exercise it). Evidence lines are quoted from the run's log
without their timestamps, with the fixture network's addresses abbreviated (`<fx>` is the fixture, `<node>` the verifier node, `<selfcheck>` the one-off container of stage M1N-1, all
inside the fixture network's three-octet prefix); `(log N)` is the line number in the saved log of the run. The scope builds postgres, the four migration chains, the fixture (`fx`), module 1's own
verifier-node image and a second module-1 control plane (`m1n`), then runs six stages. When one is red, **the stage name in `HARNESS RED at stage [...]` says where**.

| Banner | `STAGE` in the red line | What it proves | First run |
|---|---|---|---|
| `[M1N-1]` | `m1n-fixtures` | the fixtures alone, before the node or module 1 exists | `M1N-1 OK in 8 s` (log 1691) |
| `[M1N-2]` | `m1n-node` | the node alone: its audit, then `POST /v1/verify` directly for each recipe | `M1N-2 OK in 12 s` (log 1794) |
| `[M1N-3]` | `m1n-wired` | module 1 wired: boot, first audit row, promotion, a synchronous verdict with `meta.node_id` | `M1N-3 OK in 6 s` (log 1834) |
| `[M1N-4]` | `m1n-tests` | the `TestM1Node_*` tests (the recipe table in one bulk job, the cache, five control families) | `M1N-4 OK in 77 s` (log 1942): about 17 s to compile and list, 60 s of tests |
| `[M1N-5]` | `m1n-exposure` | no published port, the fixture network internal | `second instrument: 3 containers inspected, none has a host port binding; network coldmail-platform-harness_fixtures internal=true` (log 1948) |
| `[M1N-6]` | `m1n-secret-leak` | none of this run's credentials is in the stack's logs | `scanned 6 logs for 6 credentials: none present` (log 1951) |

## The unknowns of the research ([U1]-[U10]), settled

| # | The question | Verdict | What the run showed |
|---|---|---|---|
| U1 | Docker's embedded resolver forwarding to a user-given `dns:` server from an internal-only container | **NOT OBSERVED** (avoided by design: the node and `m1n` use a bind-mounted resolv.conf). The converse, the fixture forwarding Docker's own names to Docker's resolver, is **CONFIRMED** | `[ok  ] dns dotless forwarder: postgres -> <a private Docker address> (resolved by Docker behind the fixture)` (log 1655); `m1n` then reached Postgres with the fixture as its only nameserver: `module 1 /readyz: status ready, checks map[db:true pepper:true queue:true]` (log 1846) |
| U2 | A bind mount over `/etc/resolv.conf` is honoured and the audit has no `resolver` warning | **CONFIRMED** | `HYGIENE hostname="849cb0ebdde0"` (12 hex, no dot), `HYGIENE   \| nameserver <fx>`, `HYGIENE   \| options timeout:2 attempts:1`, no search or domain line (log 1706-1709); the audit carries `"warnings":{}` (log 1717) |
| U3 | `SSL_CERT_FILE` is honoured by Ruby's `Net::HTTP`; glibc uses the mounted resolv.conf | **CONFIRMED** | `STEP ipify ok=true body="<node>" bytes=11` (log 1712): the TLS chain verified against the run's CA, the body is the node's own address, 11 bytes, no newline. `HYGIENE SSL_CERT_FILE="/fx/ca.pem" DNS="<fx>" SMTP_PORT="2525" PROBE_ALLOW_NON_PUBLIC=nil` (log 1710) |
| U4 | Docker accepts the /24 on an internal network and the containers reach each other | **CONFIRMED** | `internal=true subnet=<prefix>.0/24 ip_range=<prefix>.128/25` and `coldmail-platform-harness-fx-1 <fx>/24` (log 1600-1601). The first dynamic address handed out was `.129` (the selfcheck container), not `.128`. Reachability across containers: the selfcheck from a separate container passed 84/84, the node reached the fixture on 53, 443 and 2525, `m1n` reached the node on 3000 |
| U5 | The single-threaded Thin server starves `/v1/audit` behind a stalled check | **NOT OBSERVED** | no audit ever overlapped a stall: the node's direct audit (log 1716-1717) and `m1n`'s one boot audit (1-2 s after the stall recipe had finished) ran while the node was idle. The stall recipe took 2.0 s and the calls after it were again 0.0 s (log 1740-1749) |
| U6 | Nothing promotes a node from `provisioning` to `active`; `port25_open` is never set; a restart resets both | promotion **CONFIRMED**; restart reset **NOT OBSERVED** | after a green boot audit: `node row: id=harness-node-1 status=provisioning port25_open=false audit_status=green base_url_set=true` and `latest audit: green current_host_ip=<node> warnings={}` (log 1826-1827); no `NOTE:` line was printed, so the status was `provisioning`. After the SQL promotion: `node row now: status=active port25_open=true` (log 1829); the next synchronous verify was served by the node. The paired control: with the row set back to `provisioning`, `sync pre002@valid.fixture.test with the node provisioning -> risky/plausible conf=40 node_id= signals=map[failure_origin:own ... smtp_skipped_reason:no_eligible_node ...]` (log 1849): module 1 reads the node's health on every request |
| U7 | Restart behaviour of the stack through stage 9 (Postgres stopped) | **NOT OBSERVED** | the scope has no outage stage and `m1n` was started once |
| U8 | A non-root user can bind ports 53 and 443 in the fixture container | **CONFIRMED** (what the run cannot separate: whether the compose `sysctls` entry or Docker's own default allowed it) | `fx: ready: roles=dns,https,smtp,api uid=1001 dns="[::]:53" https="[::]:443" smtp="[::]:2525" api="[::]:8025" zones=3 dns_names=44 smtp_domains=17 dns_upstream="127.0.0.11:53"` (log 1598), with `read_only`, `cap_drop: [ALL]` and `net.ipv4.ip_unprivileged_port_start=0` in the compose file |
| U9 | The exact edge of the 30 starts per 60 s rate bucket | **NOT OBSERVED** | `rate_limited` is not a recipe of this stack (the largest per-domain load was 9 starts in one job) |
| U10 | What CoreDNS plugins can do | **NOT OBSERVED** (moot) | the DNS role is the fixture's own Go responder (live per-name SERVFAIL, REFUSED, silence, NXDOMAIN, A-record changes for the controls) |

Also settled by this run, beyond the numbered list:

- **The destination guard accepted the network: CONFIRMED.** Every domain whose MX host sits in the `100.128.0` prefix reached the node's SMTP stage (`rcpt="250(clean)"`, log 1720), and none of them was `probe_refused`. The control is in the same stack: the `nonpublic` recipe answers `"probe_refused":{"reason":"non_public_target","count":1,"targets":[{"address":"10.9.8.7","range":"private-rfc1918"}]}` (log 1751). The guard is ON (`PROBE_ALLOW_NON_PUBLIC=nil`, log 1710) and the audit was green.
- **The audit is green with the three fixtures alone: CONFIRMED.** `STEP a_record ok=true name=verifier.fixture.test value="<node>" equals_ipify=true` (log 1713), `STEP ptr ok=true reverse=<reverse of node> names=["verifier.fixture.test"] includes_verifier_domain=true` (log 1714), and `{"wrapper_api_version":"v1","current_host_ip":"<node>","warnings":{},"audit_status":"green","audited_at":"..."}` (log 1717).
- **One audit only on the control plane side (`TM_WATCHER_CADENCE_SECONDS=0`): CONFIRMED.** The fixture's `ipify_answers` counter was 1 after the selfcheck and 4 at the end (log 1690, 1941): the selfcheck, the node probe's steps, the node's direct audit and `m1n`'s boot audit, nothing more during the 77 s of stage 4.
- **A clean 250 comes back as `smtp_debug: null`, and the gem's own error strings are what module 1's classifier reads: CONFIRMED.** `"success":true,"errors":null,"smtp_debug":null` (log 1721); `rcpt="server response timeout"` (log 1741) and `rcpt="server dropped connection after response"` (log 1744).
- **The gem opens and closes a connection with no bytes before each session: CONFIRMED.** The fixture counted `smtp_connections_without_commands":142` and 171 sessions at the end (log 1941); 171 minus the selfcheck's 29 is exactly 142, one port check per session.
- **The node's HELO is the verifier domain and MAIL FROM is the verifier address: CONFIRMED.** `"helo":"verifier.fixture.test","mail_from":"verifier@verifier.fixture.test"` (log 1831).

## The network plan

`H_FX_PREFIX` (default the three-octet prefix `100.128.0`, so the network is that prefix plus `.0/24`; nothing writes a four-octet address). The fixture is at `.2`, the node at `.3`, everything
else from `.128/25`. Why the guard calls it public: `verifier-node/truemail-rack/app/truemail_server/wrapper/probe_guard.rb:48` ends the CGNAT block at 100.127.255.255 and no other
entry of `V4_RANGES` (lines 45-61) covers the half of `100/8` above it; `spec/truemail_server/wrapper/probe_guard_spec.rb:38` lists the first address after the CGNAT block among the
addresses that must be let through. The guard is ON (`PROBE_ALLOW_NON_PUBLIC` is never set; the harness asserts it is `nil` in the node). The recipe `non_public_mx` (an MX host in
private space, answered from the same fixture) is the live proof that the guard is on in this very stack: **CONFIRMED** in the first run (above).

## M1N-1, the fixtures alone

| Look for | Meaning | First run |
|---|---|---|
| `fx: ready: roles=dns,https,smtp,api uid=<n> dns=":53" https=":443" smtp=":2525" api=":8025" ...` | **[U8]**: a non-root user (`uid`, the runner's) bound ports 53 and 443. Red here with `cannot listen for HTTPS on ":443" ... permission denied`: the sysctl was not applied; switch the fixture to `user: "0:0"`, keep `cap_drop: [ALL]` and add `cap_add: [NET_BIND_SERVICE]` | **CONFIRMED**: the line quoted at U8 (log 1598). The listeners print as `[::]:port`: dual-stack wildcard binds |
| `internal=true subnet=<prefix>.0/24 ip_range=<prefix>.128/25` and `fx <prefix>.2/24` | **[U4]**: Docker accepted the /24 on an internal network. Compose failing earlier with `Pool overlaps` means another network holds the range: set `H_FX_PREFIX` to another three-octet prefix outside the guard's blocked list | **CONFIRMED** (log 1600-1601) |
| `[ok  ] dns ...`, `[ok  ] https api.ipify.org: certificate chain verified ...`, `[ok  ] smtp <domain> rule ...`, then `fx selfcheck: N check(s), 0 failed` | the fixtures answer exactly what `m1node/fixture.json` says, over the network, from a **separate container**. N is 84 (44 name records and the PTR, 3 modes, the negative answers, TCP, the forwarder, https, the API, 29 SMTP rules) | **CONFIRMED**: `fx selfcheck: 84 check(s), 0 failed` (log 1688). The prediction "about 85" was right to one |
| `[FAIL] dns ...` | the DNS role is wrong or unreachable: the line names the record and what it answered | **NOT OBSERVED** (no failure) |
| `[FAIL] dns dotless forwarder: postgres ...` | the fixture could not get `postgres` answered by Docker's embedded resolver (127.0.0.11) behind it; module 1's control plane resolves its database host through the fixture, so M1N-3 would fail at boot. Fix: give `m1n` `extra_hosts` or point its `TM_DATABASE_URL` at the container's address | **NOT OBSERVED**; the `[ok  ]` form is U1 above |
| `[FAIL] https api.ipify.org: ...certificate...` | the run's CA does not verify the leaf (`m1n_mint`), or the https role is not reachable | **NOT OBSERVED**; the `[ok  ]` line: `[ok  ] https api.ipify.org: certificate chain verified against the run's CA; body "<selfcheck>" is this caller's address, 13 bytes, no newline` (log 1656) |
| `[FAIL] smtp <domain> rule "<match>": ...` | the fixture answered a different reply than the file says | **NOT OBSERVED** |

## M1N-2, the node alone

| Look for | Meaning | First run |
|---|---|---|
| `HYGIENE hostname="<12 hex>"`, `HYGIENE   \| nameserver <prefix>.2`, `HYGIENE   \| options timeout:2 attempts:1`, `PROBE_ALLOW_NON_PUBLIC=nil` | **[U2]**: the bind mount over `/etc/resolv.conf` is honoured and is clean. A `search` or `domain` line, or a different nameserver: Docker rewrote it (red: `... the bind mount did not take`); the alternative is `dns_search: ["."]` plus `dns:` on the node | **CONFIRMED** (log 1706-1710) |
| `STEP ipify ok=true body="<node ip>" bytes=<n>` | **[U3]**. `ok=false error=OpenSSL::SSL::SSLError ... certificate verify failed`: `SSL_CERT_FILE` not honoured (or the CA file is not the one the leaf was signed by); `SocketError ... getaddrinfo`: resolv.conf or the fixture's A record for `api.ipify.org`; `ECONNREFUSED`: the https role | **CONFIRMED**: `STEP ipify ok=true body="<node>" bytes=11` (log 1712) |
| `STEP a_record ok=true ... equals_ipify=true`, `STEP ptr ok=true ... includes_verifier_domain=true` | the audit's second and third step (the gem compares them as strings with the first) | **CONFIRMED** (log 1713-1714) |
| `AUDIT http=200` then `{"...","current_host_ip":"<node ip>","warnings":{},"audit_status":"green",...}` | the node's own audit is green. Red: the warning keys name the step: `ip` (the IPify step), `dns` (the A record), `ptr`, `resolver` (hygiene), `probe_guard` (the guard is off) | **CONFIRMED** (log 1716-1717) |
| `== <recipe> (<addr>): VERIFY http=200 seconds=<n>` then `SUMMARY ...` and `RAW {...}` | the node's raw answer for each recipe that reaches it; `MISMATCH: the summary lacks [..]` names what was expected. **The `seconds` are the first real timings** (the research's figures were derived, not measured) | **CONFIRMED** (all 11 matched; no `MISMATCH`). Measured: 0.0 s for ten recipes and **2.0 s** for `smtp_timeout` (the table below). **CONTRADICTED**: the research's "about 1 s per item" for the node and "about 4-5 s" for the stall |
| `smtp_timeout`: `rcpt="server response timeout"`; `smtp_error_dropped`: `rcpt="server dropped connection after response"` | the gem's own error strings; the dropped one carries no digits, so no SMTP code | **CONFIRMED** (log 1741, 1744) |
| in `[M1N-2.5]` the session list: `remote` is the node's address, two RCPTs per recipe (the target, then `nx-<hex>`) | the node's conversation with the SMTP fixture is what the research predicted | **CONFIRMED, thinly**: the `remote` of the node's lines is `<node>` and every target is followed by an `nx-<24 hex>` probe, but the printed list is cut at 40 lines and starts with the selfcheck's 29 (see item 1 below). The complete proof is in M1N-4: `the SMTP fixture saw 18 RCPT(s) in this job` ... `all from the node` (log 1873-1891, 1893) |

Measured by the node alone (`seconds` as the probe printed them, rounded to 0.1 s, so 0.0 means under 50 ms):

| Recipe | Seconds | What the node answered |
|---|---|---|
| valid, mailbox_full, mailbox_not_found, catch_all, greylisted, smtp_blocked, smtp_error_remote | 0.0 each | `rcpt` `250(clean)` / `552 5.2.2 Mailbox full` / `550 5.1.1 <probe@gone.fixture.test> User unknown` / `250(clean)` / `450 4.2.0 ...` / `554 5.7.1 ...` / `550 5.7.0 Message rejected`; catch-all conclusions `not_catchall`, `not_catchall`, `not_catchall`, `catchall`, `inconclusive`, `not_catchall`, `not_catchall` |
| smtp_timeout | **2.0** | `rcpt="server response timeout"`, `catchall=not_catchall`: the target's RCPT waited the gem's 2 s response timeout once, no session retry, then the probe ran at once |
| smtp_error_dropped | 0.0 | `rcpt="server dropped connection after response"`, `catchall=not_catchall` |
| connection_failed | 0.0 | `port_opened=false rcpt=""`, `catchall=inconclusive`, `mx=["<node>"]`: refused at once (the MX host of this recipe is the node's own address, where nothing listens on 2525) |
| non_public_mx | 0.0 | `probe_refused=true`, `rcpt="not-attempted"`, no `mx` block |

**[U5]** (single-threaded Thin starves the audit behind a stall) was not provoked: see the table at the top. If M1N-3's first audit is red while M1N-2's was green, suspect it.

## M1N-3, module 1 wired

| Look for | Meaning | First run |
|---|---|---|
| `verify: SMTP node=harness-node-1 config_fingerprint=cfg_sha256:...` and `verify: orchestrator wired (SMTP-enabled (against node list), ...)` | the one-shot `GET /v1/config` at boot succeeded: the stage is wired. If `/v1/config fetch failed` appears instead, module 1 stays unwired for its whole life (every verdict carries `smtp_unwired`) | **CONFIRMED**: `verify: SMTP node=harness-node-1 config_fingerprint=cfg_sha256:211570d3708c0950` and `verify: orchestrator wired (SMTP-enabled (against node list), policy=cold-email v1, cache_ttl=1h0m0s)` (log 1822, 1824) |
| `verify: WARNING provider-routing drift ...` | expected and harmless: the node's `VALIDATION_TYPE_FOR` is empty, the policy routes google/yahoo/o365 to `:mx` (a log warning only) | **CONFIRMED**: `verify: WARNING provider-routing drift (engine validation_type_for out of sync with policy source, §5.1): [missing on box: gmail.com->mx missing on box: googlemail.com->mx ...` (log 1823) |
| `node row: ... status=provisioning port25_open=false audit_status=green` | **[U6]**: after a green audit module 1 leaves the node `provisioning` with `port25_open=false`; `status=active` here would contradict the research | **CONFIRMED** (log 1826) |
| `latest audit: green current_host_ip=<node ip> warnings={}` | the boot audit row (`verify.node_audits`) | **CONFIRMED** (log 1827) |
| `node row now: status=active port25_open=true` | the promotion `UPDATE` (SQL, after the final start of `m1n`; a restart of `m1n` would undo it) | **CONFIRMED** (log 1829) |
| the synchronous verify JSON: `"status":"valid"`, `"reason":"accepted"`, `"node_id":"harness-node-1"`, `"verifier_ip":"<node ip>"` | module 1 produced a verdict through its real node. `smtp_unwired`: see the first row. `no_eligible_node` / `audit_red`: the promotion did not take, or `TM_NODE_PUBLIC_EGRESS_IP` differs from the audit's `current_host_ip` | **CONFIRMED**: `"status":"valid","reason":"accepted","confidence":90,"recommendation":"send" ... "latency_ms":83 ... "verifier_ip":"<node>","node_id":"harness-node-1","engine_version":null ...` (log 1831) |
| the fixture's session lines for `gate-sync@valid.fixture.test` | exactly one RCPT for the target (the probe is `nx-...`), from the node | **CONFIRMED** (log 1833): one line, `"remote":"<node>"`, `"helo":"verifier.fixture.test"`, `"reply":"250 2.1.5 OK"` |

**[U7]** (restart behaviour through stage 9) is **NOT OBSERVED**: the scope has no outage stage, and `m1n` is started once. It stays open for the full-scope inclusion.

## M1N-4, the tests

The tests print their own lines (`m1_node_test.go:NN: ...`); the stage prints them whole.

| Look for | Meaning | First run |
|---|---|---|
| `job <id>: 21 row(s) settled in <s>; the 202's counts {Total:21 Enqueued:20 Duplicate:1 Malformed:0}` | the table's job; the time is the real per-item cost with the profile's dials | **CONFIRMED**: `job 01a1eb7e-...: 21 row(s) settled in 7.6s; the 202's counts {Total:21 Enqueued:20 Duplicate:1 Malformed:0}` (log 1850). That is 7.6 s for 21 rows: one 2 s DNS silence, one 2 s SMTP stall, and for each SMTP item the control plane's 50-500 ms start jitter; the node itself adds almost nothing (M1N-2) |
| `the recipe table as module 1 reported it` followed by 20 lines `ordinal recipe address status/reason conf=.. failure_origin=.. smtp_code=.. ...` | what module 1 said for each recipe: compare with the failure list | **CONFIRMED**, every row exactly as the table says (log 1852-1871): for example `valid/accepted conf=90`, `valid/mailbox_full conf=90 smtp_code=552`, `risky/non_public_mx conf=20 probe_refused_ranges=[private-rfc1918]`, `unknown/dns_error ... failure_origin=remote` (SERVFAIL), `own` (silence), `undetermined` (REFUSED), `unknown/smtp_timeout ... failure_origin=remote retry_eligible=true`, `unknown/smtp_blocked ... failure_origin=own` |
| `N difference(s) between the recipe table and module 1's report:` then one line per difference | the exact mismatch (verdict, confidence, origin, a signal, a flag) | **NOT OBSERVED** (none). Its replacement: `every one of the 20 recipe rows, and the duplicate, is exactly what the table says; 18 session(s) at the fixture, all from the node` (log 1893) |
| `the SMTP fixture saw N RCPT(s) in this job:` and the session lines | proof of which recipes reached the node (18 RCPTs: nine recipes reach the node, two RCPTs each; `noconn` and `nonpublic` never reach the fixture) | **CONFIRMED**: `saw 18 RCPT(s) in this job` (log 1873); `session 68 ... RCPT oli@stall.fixture.test -> rule "*", stall (no reply: stalled)` followed by the `nx-` probe answered normally (log 1888-1889); `session 58 ... RCPT quin@dropped.fixture.test -> rule "*", drop (no reply: connection closed)` (log 1878) |
| `the DNS fixture answered N query(ies): X from the node, Y from module 1` | both resolvers ask the fixture | **CONFIRMED**: `answered 106 query(ies) in this job: 82 from the node, 24 from module 1 (and the fixture network's other clients)` (log 1892) |
| `<family> step <n> <knob> <domain> -> <verdict> \| bystanders: ...` | the controls: one line per step; the changed row differs from the step before and the bystanders do not | **CONFIRMED**: 5 families, 27 steps (rcpt 7, catch-all 3, transport 6, dns 6, lists 5), every bystander `valid/accepted conf=90 ...` and `invalid/no_mx conf=96` (log 1896-1922) |
| `--- PASS: TestM1Node_...` for all nine, then `check-test-floors: ok` | the tests ran and are counted (the floor of this scope is the length of `M1N_TESTS` in `m1node.sh`) | **CONFIRMED**: Preconditions 0.76 s, BulkRecipeTable 7.56 s, CacheHit 5.75 s, ControlRCPTFamily 11.79 s, ControlCatchAllFamily 5.14 s, ControlTransportFamily 11.19 s, ControlDNSFamily 10.28 s, ControlListsFamily 7.57 s, ExpectationsBite 0.00 s (log 1924-1932); `check-test-floors: ok — 1 go package(s), declared=9 ran=9 pass=9 skip=0 fail=0 (floor-sum 9)` (log 1939) |

Also from the tests: the cache control behaved as predicted (`second job: cache004@valid.fixture.test -> valid/accepted ... from_cache=true; cache005@grey.fixture.test -> unknown/greylisted ... from_cache=false`, log 1895: the cached row keeps the original signals, the unknown is checked again), and the fixture's counters at the end were `dns_queries 836, smtp_sessions 171, smtp_rcpts 171, smtp_connections_without_commands 142, ipify_answers 4` (log 1941).

What a mismatch usually means (still the first thing to read when a full run goes red here):

- **every SMTP recipe `risky/non_public_mx`**: the guard refused the fixture's address: the network prefix is inside a blocked range (check `H_FX_PREFIX`) or the A records resolved to something else.
- **`unknown/dns_error` with origin `own` for a SERVFAIL or the valid recipe**: module 1's resolver cannot reach the fixture (its resolv.conf mount) or `postgres` forwarding failed; look at the DNS counts.
- **`risky/plausible` + a skip token**: the node is not eligible (see M1N-3), the job then waits up to 90 s (`TM_QUEUE_MAX_RETRY_AGE_SECONDS`) and settles that.
- **a stall recipe `unknown/smtp_timeout` with origin `undetermined` or `own`**: the node call failed instead of timing out inside the session (the control plane's 30 s deadline, or the node answered 500).
- **`smtp_enhanced_code` mismatches**: the classifier reads the code out of the node's raw string after module 1 redacts the address; check the `RAW` of M1N-2.
- **a greylisted row retried (job slow)**: `TM_QUEUE_MAX_ATTEMPTS=1` did not take, or the item was not the first attempt.

## Surprising or fragile, although the run was green

Line numbers are those of the run's saved log.

1. **The M1N-2.5 evidence is truncated and mixed (log 1753-1793).** The list is cut by `head -40` and begins with the 29 sessions of the stage-1 selfcheck (`remote` is the selfcheck container), so only 11 of the node's lines show (log 1782-1792: the targets and probes of valid, full, gone, catchall, grey and the first of blocked). `sed: couldn't flush stdout: Broken pipe` (log 1793) is `head` closing the pipe: harmless while `run.sh` has no `pipefail`, but it would turn red the day someone adds it. The die check next to it reads the whole JSON and passed. Fix in `m1node.sh`: select sessions with `remote` equal to the node and drop `head`.
2. **A misleading test log line (log 1872).** `m1_node_test.go:194` prints `duplicate_of=0x11d4f844088`: the `*int` printed with `%v`. The assertion compares the pointed value, so the test is right, but the log never shows the ordinal (it must be 0). Print `*dup.DuplicateOf`.
3. **The research's two timings were wrong, in opposite directions.** The stall took 2.0 s (log 1740), not "4-5 s": the RCPT timeout is rescued inside the gem's session and the session is not retried; the fixture saw one stalled target and one probe per validation (log 1888-1889). Every other recipe took under 50 ms on the node (log 1719-1749), not "about 1 s": the "about 1 s per item" of the research is the control plane's own 50-500 ms start jitter (log 1850: 21 rows in 7.6 s).
4. **`seconds` has 0.1 s resolution** (the probe rounds), so `0.0` proves only "under 50 ms"; nothing asserts a time. The only measured wait is the stall's 2.0 s.
5. **A warning in a green run (log 1823).** `verify: WARNING provider-routing drift ... missing on box: gmail.com->mx ...` is expected and was predicted, but it is a standing warning: setting `VALIDATION_TYPE_FOR` in the node's environment to the policy's render would silence it.
6. **No retry anywhere and no timing within 20 percent of a timeout.** The greylisted row settled at its first attempt (`TM_QUEUE_MAX_ATTEMPTS=1`; two RCPTs, log 1874-1875: one target and one probe, sessions 54-55). The nearest margins: the stall (fixture holds 6 s, the node times out at 2 s: 3x) and the 30 s control-plane deadline against a 2 s node call (15x); the 2 s DNS silence is the resolver's own timeout (`timeout:2 attempts:1`), which is the thing measured. The fixture's 1.5 s forward deadline against the resolvers' 2 s was not exercised (Docker answered at once). Container health took 3.5 s (fixture), 6.3 s (node: bundler and thin boot) and about 5 s (`m1n`) against retries of 90 s, 120 s and 90 s.
7. **Stage 4 compiles the tests with a module download (log 1837-1844).** `go: downloading github.com/jackc/pgx/v5 v5.13.0` and six more modules: about 17 s of the 77 s, and a dependence on the Go module proxy at that point (the full harness's own compile in stage 6 may have warmed the cache by then; in this scope nothing had).
8. **The image build is the long pole (log 1207: node image built).** The `--no-cache` build of the node image compiles `eventmachine 1.2.7` and `thin 1.8.2` with native extensions and fetches `truemail 3.3.1` (about 53 s of the 90 s build stage, log 1093 for the last gem); this cost now recurs in every full run. Debian's `update-alternatives: warning: skip creation of ... lzma ...` lines (log 896-905) are slim-image noise, not a failure.
9. **The module-1 commit differs from the one the research read** (`1fa2550`, log 313, against `4adc316`): the `main.go:485-492` style citations in the research are from the older commit. Everything the research predicted about module 1 held, so the code did not move in the places that matter.
10. **`check-exposure` examined 4 services (log 1945) though three are named**: the fourth is Postgres (the whole stack is listed); harmless, the claim is about the whole stack.
11. **Two actions-runner warnings** (log 130-131 and 1964-1965, the `punycode` deprecation; log 1981, Node.js 20 actions forced to Node 24) are the runner's, unrelated to the stack.
12. **The full-scope path is unobserved.** In this scope `DC stop m1` had nothing to stop (`m1` never existed, log 1797-1798); in the full run `m1n_stage3` stops a running `m1` after the platform instances and module 3's workers have been through stages 9-10, and the whole stack then starts on top of that. The first `full` run with `m1node` in `FULL_INCLUDES` is the first time that happens.
13. **Combined scopes are unobserved.** `m2worker` and `m1node` were each proven alone (`m2worker` in its own run); the merged compose model with both overrides, and a run that builds both images in one stage 3, was only validated offline (`docker compose config`), not run.

## If a stage is red and the log is long

Send: the banner of the red stage and everything after it up to `---- service logs (tail) ----`, plus the `fx`, `verifier-node` and `m1n` sections of that block. Do not paste the tokens
(there are none in the log: they are masked and the secret-leak stage looks for them).
