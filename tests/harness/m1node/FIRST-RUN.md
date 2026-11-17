# First run of the module-1 node stack: what to look for

For the lead. Dispatch the `harness` workflow with `scope = m1node` on a branch and send the log. Nothing of this stack runs in the `full` scope (it is dormant until `m1node` is
named in `FULL_INCLUDES` in `tests/harness/run.sh`). The scope builds postgres, the four migration chains, the fixture (`fx`), module 1's own verifier-node image and a second module-1
control plane (`m1n`), then runs six stages, each with its own banner. The first red stage is the answer: **the stage name in `HARNESS RED at stage [...]` says where**, the lines
below say what each outcome means. Nothing here was run (no Docker on the author's machine): every statement about the node, Docker and Ruby is a prediction until this run.

| Banner | `STAGE` in the red line | What it proves |
|---|---|---|
| `[M1N-1]` | `m1n-fixtures` | the fixtures alone, before the node or module 1 exists |
| `[M1N-2]` | `m1n-node` | the node alone: its audit, then `POST /v1/verify` directly for each recipe |
| `[M1N-3]` | `m1n-wired` | module 1 wired: boot, first audit row, promotion, a synchronous verdict with `meta.node_id` |
| `[M1N-4]` | `m1n-tests` | the `TestM1Node_*` tests (the recipe table in one bulk job, the cache, five control families) |
| `[M1N-5]` | `m1n-exposure` | no published port, the fixture network internal |
| `[M1N-6]` | `m1n-secret-leak` | none of this run's credentials is in the stack's logs |

Time budget: every stage prints `M1N-n OK in <s> s`. Expect M1N-1 under 15 s, M1N-2 about 40 s (a stall is 4-5 s), M1N-3 under 30 s, M1N-4 2-3 minutes.

## The network plan

`H_FX_PREFIX` (default the three-octet prefix `100.128.0`, so the network is that prefix plus `.0/24`; nothing writes a four-octet address). The fixture is at `.2`, the node at `.3`, everything
else from `.128/25`. Why the guard calls it public: `verifier-node/truemail-rack/app/truemail_server/wrapper/probe_guard.rb:48` ends the CGNAT block at 100.127.255.255 and no other
entry of `V4_RANGES` (lines 45-61) covers the half of `100/8` above it; `spec/truemail_server/wrapper/probe_guard_spec.rb:38` lists the first address after the CGNAT block among the
addresses that must be let through. The guard is ON (`PROBE_ALLOW_NON_PUBLIC` is never set; the harness asserts it is `nil` in the node). The recipe `non_public_mx` (an MX host in
private space, answered from the same fixture) is the live proof that the guard is on in this very stack.

## M1N-1, the fixtures alone

| Look for | Meaning |
|---|---|
| `fx: ready: roles=dns,https,smtp,api uid=<n> dns=":53" https=":443" smtp=":2525" api=":8025" ...` | **[U8] settled**: a non-root user (`uid`, the runner's) bound ports 53 and 443 through the namespaced sysctl `net.ipv4.ip_unprivileged_port_start=0`. Red here with `cannot listen for HTTPS on ":443" ... permission denied`: the sysctl was not applied; switch the fixture to `user: "0:0"` (the key is then root-readable anyway), keep `cap_drop: [ALL]` and add `cap_add: [NET_BIND_SERVICE]` |
| `internal=true subnet=<prefix>.0/24 ip_range=<prefix>.128/25` and `fx <prefix>.2/24` | **[U4] settled**: Docker accepted the /24 on an internal network. Compose failing earlier with `Pool overlaps` means another network holds the range: set `H_FX_PREFIX` to another three-octet prefix outside the guard's blocked list |
| `[ok  ] dns ...`, `[ok  ] https api.ipify.org: certificate chain verified ...`, `[ok  ] smtp <domain> rule ...`, then `fx selfcheck: N check(s), 0 failed` | the fixtures answer exactly what `m1node/fixture.json` says, over the network, from a **separate container** (so reachability across Docker is proven, not just the process). N is about 85 (45 records, 3 modes, the negative answers, TCP, the forwarder, https, the API, 29 SMTP rules) |
| `[FAIL] dns ...` | the DNS role is wrong or unreachable: the line names the record and what it answered |
| `[FAIL] dns dotless forwarder: postgres ...` | the fixture could not get `postgres` answered by Docker's embedded resolver (127.0.0.11) behind it. Module 1's control plane resolves its database host through the fixture, so M1N-3 would fail at boot. Fix: give `m1n` `extra_hosts` or point its `TM_DATABASE_URL` at the container's address, or give the fixture another upstream |
| `[FAIL] https api.ipify.org: ...certificate...` | the run's CA does not verify the leaf (`m1n_mint`), or the https role is not reachable |
| `[FAIL] smtp <domain> rule "<match>": ...` | the fixture answered a different reply than the file says (names the address, what it got, what it wanted) |

## M1N-2, the node alone

| Look for | Meaning |
|---|---|
| `HYGIENE hostname="<12 hex>"`, `HYGIENE   \| nameserver <prefix>.2`, `HYGIENE   \| options timeout:2 attempts:1`, `PROBE_ALLOW_NON_PUBLIC=nil` | **[U2] settled**: the bind mount over `/etc/resolv.conf` is honoured and is clean. A `search` or `domain` line, or a nameserver that is not `<prefix>.2`: Docker rewrote it (red: `... the bind mount did not take`); the alternative is `dns_search: ["."]` plus `dns:` on the node |
| `STEP ipify ok=true body="<node ip>" bytes=<n>` | **[U3] settled**: Ruby's `Net::HTTP` honoured `SSL_CERT_FILE` and glibc used the mounted resolv.conf. `ok=false error=OpenSSL::SSL::SSLError ... certificate verify failed`: `SSL_CERT_FILE` not honoured (or the CA file is not the one the leaf was signed by); `SocketError ... getaddrinfo`: resolv.conf or the fixture's A record for `api.ipify.org`; `ECONNREFUSED`: the https role |
| `STEP a_record ok=true ... equals_ipify=true`, `STEP ptr ok=true ... includes_verifier_domain=true` | the audit's second and third step (the gem compares them as strings with the first) |
| `AUDIT http=200` then `{"...","current_host_ip":"<node ip>","warnings":{},"audit_status":"green",...}` | the node's own audit is green. Red: the warning keys name the step: `ip` (the IPify step), `dns` (the A record), `ptr`, `resolver` (hygiene), `probe_guard` (the guard is off) |
| `== <recipe> (<addr>): VERIFY http=200 seconds=<n>` then `SUMMARY ...` and `RAW {...}` | the node's raw answer for each recipe that reaches it; `MISMATCH: the summary lacks [..]` names what was expected. **The `seconds` are the first real timings** (the research's figures were derived, not measured) |
| `smtp_timeout`: `rcpt="server response timeout"`; `smtp_error_dropped`: `rcpt="server dropped connection after response"` | the gem's own error strings are what module 1's classifier needs (`isTimeout` matches "timeout"; the dropped one carries no digits, so no SMTP code) |
| in `[M1N-2.5]` the session list: `remote` is the node's address, two RCPTs per recipe (the target, then `nx-<hex>`) | the node's conversation with the SMTP fixture is exactly what the research predicted |

**[U5]** (single-threaded Thin starves the audit behind a stall) is not provoked here: the control plane's one boot audit and the sequential probes never overlap. If M1N-3's first audit
is red while M1N-2's was green, suspect it (look at the timing of the audit against a stalled recipe in the node's log).

## M1N-3, module 1 wired

| Look for | Meaning |
|---|---|
| `verify: SMTP node=harness-node-1 config_fingerprint=cfg_sha256:...` and `verify: orchestrator wired (SMTP-enabled (against node list), ...)` | the one-shot `GET /v1/config` at boot succeeded: the stage is wired. If `/v1/config fetch failed` appears instead, module 1 stays unwired for its whole life (every verdict carries `smtp_unwired`): the node was not reachable at `http://<prefix>.3:3000` or the token is wrong |
| `verify: WARNING provider-routing drift ...` | expected and harmless: the node's `VALIDATION_TYPE_FOR` is empty, the policy routes google/yahoo/o365 to `:mx` (a log warning only, `main.go:485-492`) |
| `node row: ... status=provisioning port25_open=false audit_status=green` | **[U6] confirmed**: after a green audit module 1 leaves the node `provisioning` with `port25_open=false`; nothing in its code promotes it. `status=active` here would contradict the research |
| `latest audit: green current_host_ip=<node ip> warnings={}` | the boot audit row (`verify.node_audits`) |
| `node row now: status=active port25_open=true` | the promotion `UPDATE` (SQL, after the final start of `m1n`; a restart of `m1n` would undo it) |
| the synchronous verify JSON: `"status":"valid"`, `"reason":"accepted"`, `"node_id":"harness-node-1"`, `"verifier_ip":"<node ip>"` | **module 1 produced a verdict through its real node**. `"smtp_skipped_reason":"smtp_unwired"`: see the first row. `no_eligible_node` / `audit_red`: the promotion did not take, or `TM_NODE_PUBLIC_EGRESS_IP` differs from the audit's `current_host_ip` (the GAP-24 cross-check) |
| the fixture's session lines for `gate-sync@valid.fixture.test` | exactly one RCPT for the target (the probe is `nx-...`), from the node |

**[U7]** (restart behaviour through stage 9) is not exercised in this scope: it has no outage stage, and `m1n` is started once. It stays open for the full-scope inclusion.

## M1N-4, the tests

The tests print their own lines (`m1_node_test.go:NN: ...`); the stage prints them whole.

| Look for | Meaning |
|---|---|
| `job <id>: 21 row(s) settled in <s>; the 202's counts {Total:21 Enqueued:20 Duplicate:1 Malformed:0}` | the table's job; the time is the **real per-item cost** with the profile's dials |
| `the recipe table as module 1 reported it` followed by 20 lines `ordinal recipe address status/reason conf=.. failure_origin=.. smtp_code=.. ...` | what module 1 said for each recipe: compare with the failure list |
| `N difference(s) between the recipe table and module 1's report:` then one line per difference `<recipe> (<addr>): <field>` | the exact mismatch (verdict, confidence, origin, a signal, a flag). The first run is exploratory: a mismatch here is information; send the whole stage |
| `the SMTP fixture saw N RCPT(s) in this job:` and the session lines | proof of which recipes reached the node (18 RCPTs are expected: nine recipes reach the node, two RCPTs each; `noconn` and `nonpublic` never reach the fixture) |
| `the DNS fixture answered N query(ies): X from the node, Y from module 1` | both resolvers ask the fixture (the node through `DNS=`, module 1 through its mounted resolv.conf); one at zero means a resolver is not using it |
| `<family> step <n> <knob> <domain> -> <verdict> \| bystanders: ...` | the controls: one line per step; the changed row differs from the step before and the bystanders do not |
| `--- PASS: TestM1Node_...` for all nine, then `check-test-floors: ok` | the tests ran and are counted (the floor of this scope is the length of `M1N_TESTS` in `m1node.sh`) |

What a mismatch usually means:

- **every SMTP recipe `risky/non_public_mx`**: the guard refused the fixture's address: the network prefix is inside a blocked range (check `H_FX_PREFIX`) or the A records resolved to something else.
- **`unknown/dns_error` with origin `own` for a SERVFAIL or the valid recipe**: module 1's resolver cannot reach the fixture (its resolv.conf mount) or `postgres` forwarding failed; look at the DNS counts.
- **`risky/plausible` + a skip token**: the node is not eligible (see M1N-3), the job then waits up to 90 s (`TM_QUEUE_MAX_RETRY_AGE_SECONDS`) and settles that.
- **a stall recipe `unknown/smtp_timeout` with origin `undetermined` or `own`**: the node call failed instead of timing out inside the session (the control plane's 30 s deadline, or the node answered 500).
- **`smtp_enhanced_code` mismatches**: the classifier reads the code out of the node's raw string after module 1 redacts the address; check the `RAW` of M1N-2.
- **a greylisted row retried (job slow)**: `TM_QUEUE_MAX_ATTEMPTS=1` did not take, or the item was not the first attempt.

## [U9] and [U10]

[U9] (the rate bucket's edge) is not covered: `rate_limited` needs 31 session starts at one domain inside a minute and is listed as not producible in the test's doc comment. [U10] (CoreDNS) is moot:
the DNS role is the fixture's own Go responder, because the controls change one name's behaviour (SERVFAIL, REFUSED, silence, NXDOMAIN, an A record) live and a static zone file cannot.

## If a stage is red and the log is long

Send: the banner of the red stage and everything after it up to `---- service logs (tail) ----`, plus the `fx`, `verifier-node` and `m1n` sections of that block. Do not paste the tokens
(there are none in the log: they are masked and the secret-leak stage looks for them).
