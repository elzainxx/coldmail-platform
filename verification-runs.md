# Verification runs — what was actually executed at the close of Chunk 4

**Recorded 2026-08-07.** Chunk 4 required, per repo: *clean-slate + full suite + all gates, in Docker,
TWICE, pasted.* This file is the record of what ran, at which commit, and what it produced. It exists
because **"verified" is a claim about a specific execution**, and a claim about an execution that is
not written down is indistinguishable from one that did not happen.

**Every result below came from a container.** Nothing was run on the Windows host: this host silently
does not execute freshly-built Go test binaries, and a host-run Go result is therefore not admissible
evidence — a green from it would be the absence of an execution, not the success of one.

---

> **Five records live in this file.** The section directly below is the **current** one (2026-11-09, third pass: all three code
> repositories green on GitHub Actions). Under it, the second pass (2026-11-08) and the *first pass* of the same day (module 3 still
> red) are kept as written, as is the 2026-11-07 section, which describes **local** Docker Desktop runs — read that one as a record of what ran
> locally, **not** of what is green. Everything from "The four repos" onward is the **August 2026 record, kept as written**:
> its commit hashes and counts describe the trees as they were on 2026-08-07.

---

## Third pass, 2026-11-09 — implicit MX, SPF CIDR lengths, resolver hygiene and digest pins; all three green on GitHub Actions

**The result.** Every run below was checked against GitHub (`gh run view`) for its head commit and conclusion before being written here.

| repo | head | GitHub Actions run | result |
|---|---|---|---|
| `coldmail-verify` (M1) | `848fc7e` | [42616710285](https://github.com/elzainxx/coldmail-verify/actions/runs/42616710285) | **success, 10 of 10 jobs** — incl. the DNS job (search-domain gate + implicit-MX gate, each with its self-test), the new resolver-hygiene job, RSpec (91 examples) and Door 5 |
| `coldmail-send-managed` (M2) | `15a6b90` | [42616715632](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42616715632) | **success** — harness `62 passed, 0 failed`; `image_pins` 9 references, 23-case self-test |
| `coldmail-send-byo` (M3) | `36bc7ed` | [42616719618](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42616719618) | **success** — harness `100 passed, 0 failed`; `image_pins` 11 references; `dns_search_domain` **45 of 45** under an injected search domain |

Intermediate green runs, one per chunk: M1 C1 [42610984679](https://github.com/elzainxx/coldmail-verify/actions/runs/42610984679) (`1b3d083`, 9/9);
M3 C2 [42612618352](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42612618352) (`ce206aa`, harness 100/0); M1 C3a
[42614985930](https://github.com/elzainxx/coldmail-verify/actions/runs/42614985930) (`f092e02`, 10/10).

### C1 — M1: implicit MX (RFC 5321 §5.1), M1 ADR-0020
A domain with no MX but an A/AAAA record receives mail at that host; M1 called it `invalid / no_mx`. The old A/AAAA fallback was **dead code
against real DNS**: Go reports NODATA on an MX query as a not-found *error*, never an empty list, and every fixture was a fake that returned the
impossible empty list. Ruling implemented: no MX + A/AAAA → that host is the mail host and the SMTP probe runs; probe cannot run → `risky`
(never `invalid`); null MX stays `invalid` even with an A record; no MX and no A/AAAA stays `invalid`; an outage of ours is `unknown`.
Found while building it: Go's combined A+AAAA lookup keeps whichever error arrives *last* (a goroutine race), so A=NODATA + AAAA=SERVFAIL
answered "not found" or "SERVFAIL" at random — the follow-up asks `ip4` and `ip6` separately. Proof: a real-protocol in-process DNS server
(real NODATA shapes, SOA in authority) driving the production resolver; paired controls (A-only → not invalid; no records, NXDOMAIN, null MX
→ invalid); a frozen copy of the old logic that must fail the same fixture; two mutants (follow-up removed; AAAA failure counted as absent)
that must go red in the right test, end to end through `Verify`. The ADR-0019 search-domain gate's mutant signature had to move (the follow-up
masks the plain-SERVFAIL collapse) — found by re-running the mutant instead of assuming it.

### C2 — M3: SPF `a`/`mx` CIDR lengths (M3-CI-OD-2)
`a/24` was looked up as the host "24". One RFC 7208 §5.3/§5.4 parser now serves the grammar check, bare detection and the void-lookup name.
Verdicts that flipped, each with a control that did not: `a/24`, `mx/24`, `a/24//64 mx/24//64` from warn → **allow** (self-hosted exception);
a valid record with four length-bearing terms under `p=reject` + posture `block` from **BLOCK** → warn; `a/33` is now `from_spf_record_invalid`.
A 20-form table drives parser, lookup name, bare detection and grammar; a frozen copy of the old logic is wrong on every length-bearing form;
with the three behaviours reverted in a scratch copy the verdict tests fail with exactly the old verdicts (a one-off, stated as such).
**Flagged, not changed (M3-CI-OD-3):** a macro-bearing target is looked up as a literal and counted void; with three of them
`macroPresent=true` *and* a lookup-limit PermError that `decide()` reads first → a false BLOCK under posture `block` + `p=reject`.

### C3 — hardening
(a) **Resolver hygiene is a control (M1 ADR-0019 addendum, FORK.md row 16).** The node's `GET /v1/audit` adds a `resolver` warning for a
`search`/`domain` line, `ndots` > 1 — and a dotted hostname, **added beyond the ruling**: Ruby derives a search domain from the hostname
whenever none is explicit, including in `dns:`-servers mode where resolv.conf is never read, so a resolv.conf-only check would have passed
the PTR-style FQDN boxes most at risk. Gate: the real fork module under the node image's digest-pinned Ruby against a real mounted
resolv.conf, with Ruby's own `Resolv::DNS` as an oracle (the check may over-approximate, never miss), three mutants (one per rule). The brain
needed no change; pinned on the exact wrapper JSON and end to end (real scheduler → node never called → `risky/plausible`). Operational
consequence: **give the verifier box a short hostname** (runbook Stage 1 step 2b).
(b) **Every image pinned by digest.** `postgres:16-alpine`, `golang:1.26-alpine`, `alpine:3.20`, `node:20-alpine` in M2/M3, M1's CI postgres
service, and the parent's tool images (`ubuntu`, `golang`, `node`, `docker:27-cli`, `alpine:3`). `image_pins` (M2/M3) now rejects *any*
reference without a digest, with no exception list; its scope was widened because the old one is how `alpine:3` sat in `tools/own-secrets.sh`
(the key-ownership tool the CI gate runs) while the gate said OK — now `tools/*/Dockerfile`, workflow `image:` lines and a **heuristic** scan of
shell tooling. Against the pre-pin trees the new gate fails on all 7 tag-only references. M1's base-image gate gained the same rule for
workflow images. **Not verified:** `node:20-alpine` and `alpine:3.20` are old releases; the lookup of their support status failed from the
build host, so nothing is asserted — flagged for the owner.

**A mistake of mine, kept visible.** My first image inventory was cut off at 30 lines by `head`, which hid `alpine:3` in `own-secrets.sh`; the
parent's `docker:27-cli` was hidden by a name list that did not include `docker`. Both were found by redoing the inventory with the noise filtered
instead of truncated — the same shape of error as the gate scope they exposed.

---

## Second pass, 2026-11-08 — all three code repositories green on GitHub Actions

**The result.** Every head below was checked against GitHub for its commit and conclusion. "Green" is a GitHub Actions run on a
real runner; a local run and the Linux replica are rehearsal.

| repo | head | GitHub Actions run | result |
|---|---|---|---|
| `coldmail-verify` (M1) | `82644ae` | [42498731680](https://github.com/elzainxx/coldmail-verify/actions/runs/42498731680) | **success, 9 of 9 jobs** — incl. `verifier-node (RSpec)` and `Door 5` on an image built from scratch with `--pull --no-cache` against the **frozen `Gemfile.lock`**, the new `gemfile-lock` gate, and the `control-plane DNS` search-domain fixture |
| `coldmail-send-managed` (M2) | `51e081d` | [42502196556](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42502196556) | **success** — harness `62 passed, 0 failed`; 29 steps succeeded, 1 skipped (`Diagnostics on failure`, `if: failure()`); `image_pins OK` |
| `coldmail-send-byo` (M3) | `9aad2db` | [42502198054](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42502198054) | **success** — harness `100 passed, 0 failed`; 33 steps succeeded, 1 skipped (`Diagnostics on failure`); `image_pins OK`; the search-domain fixture and its mutant control both pass |

Module 3 is therefore **no longer red**: the first pass's single failing test (`TestEvaluate_T5…`) was a real defect in the
From-gate, ruled (M3-CI-OD-1) and fixed — not loosened, not skipped.

### C1 — M3-CI-OD-1 ruled and fixed (module 3)

Every From-gate lookup is made for a fully-qualified name (trailing dot), so Go's resolver never applies the `resolv.conf` search
list; "no record" is a narrow door (`IsNotFound` and neither timeout nor temporary); SERVFAIL, timeout and every other error are
`indeterminate / from_dns_unavailable` and are never negative-cached. Two lookups that bypassed the rule were brought under it: the
self-hosted exception's MX check (a second implementation called `net.DefaultResolver` directly and mapped every failure to "not
self-hosted") and SPF's `a:`/`mx:` void-target lookup (which swallowed a SERVFAIL as "not void").
- **Proof it reds without the fix, on the literal pre-fix code (Linux, search domain injected):** T5 *passes* with no search domain
  (3.00 s — why every local green hid it) and *fails* with one, with the exact message the GitHub runner printed.
- **Permanent CI fixture:** `dns_search_domain.sh` runs all 34 fromgate tests in a digest-pinned container whose `resolv.conf` has a
  `search` line; on the runner it reported `canary: search list [search-domain.test] is live`. The paired `…_selftest.sh` builds a
  mutant with rooting disabled and requires T5 to fail with the collapse signature while the canary passes.
- **One interpretation, recorded in the ledger:** "only a genuine NXDOMAIN" is implemented as genuine *not-found* (NXDOMAIN or NODATA),
  because Go cannot tell them apart and M3-P4-OD-13 depends on NODATA being absence.

### C2 — the same class in module 1 (the bigger risk) and the audit

M1's `NetResolver` used `net.DefaultResolver` with unrooted names and `asDNSNotFound` returned `IsNotFound` alone. On a host with a
search domain a SERVFAIL for the user's domain was overwritten by the suffixed name's NXDOMAIN, `resolveMX` returned `mxNone`, and
the policy said **`invalid / no_mx`: our DNS outage stated as a fact about the user's address**. Fixed (ADR-0019): rooted names, the
narrow door, and a permanent search-domain fixture (real DNS protocol over loopback from inside the test binary — nothing to start,
nothing skips) with a canary and a MUTANT control (mutant: 3 failures incl. the SERVFAIL case, canary green; old non-strict door
restored: `TestAsDNSNotFound_IsTheNarrowDoor` fails on its two OLD-BUG rows). **GitHub's own runner has an Azure search domain**: even
the plain, fixture-less `go test` in the `control-plane` job printed `canary: search list [….internal.cloudapp.net] is live`.

Audit results: the Go layers are the only explicit DNS in M1's control plane. **The Ruby node** (`truemail` 3.3.1, read but never
changed — Door 5) resolves with `Resolv`, measured: it returns `[]` for NXDOMAIN, SERVFAIL and timeout alike; with only `dns:`
servers configured it derives a search list from the container's hostname; an unrooted nonexistent name followed by a wildcard under
the search domain resolved **another domain's MX** (Ruby falls through on NXDOMAIN, not on SERVFAIL). It cannot mint an `invalid`
(a response with no SMTP transcript classifies `unknown / connection_failed`), so the residue is deployment hygiene (no search
domain, no dotted hostname) — recorded as a Stage 0 item, not changed. **M2 has no explicit DNS lookups**; its worker's implicit
dial error becomes `connect_refused` → `classTimeout` (requeue, no penalty).

**Found, NOT fixed, needs a ruling:** M1 reports a domain with an **A record and no MX** as `invalid / no_mx`. `resolveMX` stops at
the first not-found MX and never reaches the A/AAAA implicit-MX fallback its comments describe; Go reports NODATA on an MX query as a
not-found error, so the fallback is dead code against real DNS (measured through the production layers). It changes which real
domains are called invalid, so it was not folded into a hygiene fix. (Null-MX detection was probed and works.)

### C3 — the paused work, the pins, and the lockfile

- **Pins:** Mailpit (`v1.33.1`), CoreDNS (`1.15.0`) and the DNS black-hole fixture (`python 3.15.2-alpine`) are pinned by digest;
  `grep ':latest'` over every compose, Dockerfile and workflow in the three code repos is empty (the replica tooling's `yq` too).
  The `image_pins` gate keeps it so; its 13-case selftest rejects each way a pin can be lost. Builder/base tags (postgres, golang,
  alpine, node) are tagged but not digest-pinned — reported on every run, by decision (pinning a builder freezes its security patches).
- **`Gemfile.lock`:** committed for M1's verifier node, generated in the image's own Ruby; the Dockerfile installs it frozen, runs no
  `bundle lock`, and pins bundler to the lock's `BUNDLED WITH`. `assert-gem-unmodified.sh` passes unchanged (Door 5), and the new
  `check-gemfile-lock.sh` (14-case selftest) additionally asserts the lock's `truemail` equals the Gemfile's exact pin.
- **Default-mode harness and the real `docker stop postgres` drill, on BOTH senders, on a real runner** (the local VM could not hold
  them — Claude Code reaped the local runs twice for host memory, and they were **not** restarted; they moved to GitHub via the
  on-demand `outage-drill` workflow):

| | whole harness, default (non-RAM) mode | drill (`tests/harness/drills/postgres_outage.sh`) | snapshot before the plug (jobs / events / send_logs / max(seq)) | after recovery | run |
|---|---|---|---|---|---|
| M2 | **62 passed, 0 failed** | **22 passed, 0 failed** | 6 / 22 / 4 / 22 | 6 / 22 / 4 / 22 | [42499590634](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42499590634) |
| M3 | **100 passed, 0 failed** | **22 passed, 0 failed** | 8 / 28 / 5 / 28 | 8 / 28 / 5 / 28 | [42500897743](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42500897743) |

In both: a job genuinely **in flight** (worker paused) when Postgres was stopped for 50 s; the Control stayed running with **0
restarts**; health and `run` **failed closed** with only `{code, message, request_id}` and no leaked detail; a bounded `loops remain
PAUSED, no state invented` backoff (8 lines in 50 s); `db outage RESOLVED after 55 s / 54 s`; **nothing invented** — jobs, events and
`max(seq)` unchanged, the run refused during the outage left no job behind, and the in-flight job stayed `sending` while its worker
was frozen; then, once the worker was released, the job was delivered **exactly once** (one `send_logs` row, one new message) and the
retried request went through once. M3's first drill run ([42499593300](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42499593300))
failed at *fixtures* — the drill reused the system user, who already had an account, and module 3 allows one per user — and was fixed
(`00b780b`); it never reached Postgres, and it is a defect in the script, not in the product.

### Standing instructions, and what was NOT done

- **The unexplained `Phase 3 dispatch` red (231/231 tests, no `--- FAIL`, run 42438687947) was not chased,** as instructed. It has not
  recurred in the seven M3 harness runs since (five CI runs, two drill runs). **If it recurs, check the `go_suite` SIGPIPE race first**
  (`go_suite_selftest.sh` pins that class: an early `--- FAIL` followed by >64 KB of output, and a package that fails with every test
  passing).
- **The Ruby 3.3/3.4 move was not attempted** — a separate decision tied to M1's go-live (ADR-0018 reports what it would take).
- **Open, needing a ruling:** M1's A-only-domain `invalid / no_mx` (above); the Ruby 3.2 end-of-life decision; the node's resolver
  hygiene as a deployment rule.

---

## Re-verification, 2026-11-08 (first pass) — GitHub Actions is the source of truth

> *Superseded by the second pass above; kept as written. In this pass module 3 was still red on `TestEvaluate_T5…`.*

**Why this section exists.** The 2026-11-07 section below says the published commits were green. That was true of a
**local Docker Desktop run on a Windows host** and of nothing else: the first runs of those same commits on real GitHub
runners were **red in all three code repos**. From this date, **a result counts only if it is a GitHub Actions run**; a
local run, and the Linux replica of the runner (`tools/ci-replica.sh`), are rehearsal. Why local green stopped being
evidence is false-green **class 20** in [`deferrals-and-false-greens.md`](deferrals-and-false-greens.md).

### The result — and it is not "all green"

| repo | commit | GitHub Actions run | result |
|---|---|---|---|
| `coldmail-verify` (M1) | `c58dd3f` | [42433757109](https://github.com/elzainxx/coldmail-verify/actions/runs/42433757109) | **success** — 7 of 7 jobs, none skipped, including the two that had never passed and the new base-image gate |
| `coldmail-send-managed` (M2) | `098427e` (head) | [42439729294](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42439729294) | **success** — 27 steps succeeded, 1 skipped (`Diagnostics on failure`, `if: failure()`); harness `62 passed, 0 failed`; 80 worker unit tests. **Second consecutive green**: the previous head `50d33db` is [42437412115](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42437412115), also success |
| `coldmail-send-byo` (M3) | `e53f263` (head) | [42439730973](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42439730973) | **FAILURE** — harness `99 passed, 1 failed`; the one red is `TestEvaluate_T5_TimeoutServfailVsNXDOMAIN`. Every gate, incl. `config_consumption` (89 keys) and `go_suite_selftest` (7 assertions), passed |

**Module 3's CI history, all runs of code since the fixes — stated in full because one of them is unexplained:**
`c1e4eb5` [42437413708](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42437413708) **failure, 99/1** (T5);
`40a2478` (docs only) [42438687947](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42438687947) **failure, 98/2** — T5 *and*
`Phase 3 dispatch`, a package that failed with all 231 of 231 tests having run and **no `--- FAIL` line**, so no test failed and
the package still did; `e53f263` (above) **failure, 99/1** (T5 only). So the dispatch failure appeared in **one of three** runs of
the same code: it is **intermittent and its cause is unknown**. It did not occur in three full local rehearsals. The harness
could not explain it (its failure-detail printer, then, showed passing-test chatter and stopped short of the end of the output);
that printer is now fixed and proven on a fixture of exactly that shape, so **the next occurrence will say what it was**. Nothing
is claimed about its cause or about whether it is related to T5.

**Module 3's T5 is red, deliberately, and this is not a flake.** The failure is reproducible by changing one line of
`/etc/resolv.conf` (a `search` domain, which a runner has and the dev host does not), and it is a property of the
**product's** From-gate, not of the test: a DNS SERVFAIL can be rewritten into an NXDOMAIN and treated as "no records". It
is recorded as `M3-CI-OD-1` in module 3's open-decisions ledger and left for a ruling; the test was neither loosened nor
skipped. **The brief's goal — all three code repos green on GitHub Actions — is therefore met for two of three.**

For the record, the runs of the commits published on 2026-11-07, all **failure**: M1 `b212524`
[42391530406](https://github.com/elzainxx/coldmail-verify/actions/runs/42391530406), M2 `8afd970`
[42391533710](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42391533710), M3 `99e51ae`
[42391536944](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42391536944). In between: M2 `4d0f62c`
[42433760361](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42433760361) and M3 `03ec161`
[42433763630](https://github.com/elzainxx/coldmail-send-byo/actions/runs/42433763630) got the stack up (the original failure
was fixed) and failed only on the next layer: the consumption gate's broken pipe (M2, M3) and T5 (M3).

### What was found and fixed (each with a control that fails when the property breaks)

| finding | repo | control |
|---|---|---|
| dev keys mode 600 but owned by the host user; the Control (uid 10001) cannot read its KMS key on Linux — fixed by **ownership**, never by loosening | M2, M3 | `secret_ownership.sh` (fails if a key is loosened or the worker uid can read the KMS key) |
| `ruby:3.2.0-slim` = Debian 11, security repo gone, `apt-get` 404 on a cold build — rebuilt on `3.2.11-slim-bookworm` pinned by digest, built `--pull --no-cache` (ADR-0018; Door 5 untouched) | M1 | `check-base-image-pinned.sh` + an 11-case selftest |
| test SMTP sink greeted twice after STARTTLS; 10 worker tests (S1/S3 credential-scrub among them) failed on Linux, Node 20 and 24 | M2, M3 | `sink_fidelity.test.mjs` — 93 pass / 11 fail on the old sink, 104 / 0 on the fixed |
| `node --test "glob"` ran zero tests on Node 20; CI now pins Node 20 | M2, M3 | the existing zero-test guard (it is what caught it) |
| header checks assumed LF, Mailpit returns CRLF | M2, M3 | the T1 check now names the condition that tripped and dumps `cat -A` |
| `grep -c` killed a `bash -e` step on its healthy path | M2, M3 | found by the replica; `|| true` with the reason |
| `sed \| grep -q` under pipefail: consumption gate false red (15/24, 11/24 runs under contention → 0/24) | M2, M3 | new `config_consumption_selftest` case — exit 1 against the old gate |
| `go_suite` could record a failing Go package as PASSING on large output (latent; none found in the green logs) | M2, M3 | `go_suite_selftest.sh` — 3 of 5 assertions fail against the old `run.sh` |
| a failing Go package printed only its verdict line; then my first fix for that printed the wrong lines (passing-test chatter, cut off before the end) and could not explain a package that failed with every test passing | M2, M3 | `go_suite_selftest.sh`, now 7 assertions: reverting `go_fail_detail` to the first version fails exactly the explanation assertion |
| #131 ported: RAM-Postgres opt-in in `dev.sh` | M2 | `postgres_durability.sh` |
| the From-gate's DNS fail-closed property depends on the host's resolver search domain | M3 | **none yet — open: `M3-CI-OD-1`** |

### What was rehearsed, and what was NOT run

Rehearsal on the Linux replica (**not** citations): M3 real-infra `100 passed, 0 failed`; M2 `62 passed, 0 failed`; M1's
`base-image-pinned`, `door5-gem-unmodified`, `verifier-node-rspec` (71 examples, 0 failures, built with `--pull --no-cache`)
and `no-secret-leak` all SUCCESS. **Both sender modules passed the replica and then failed on the real runner** (see class 20).

**Not run, because the host ran low on memory partway through and the sequence was stopped:** M2's full harness in the
*default* (non-RAM) Postgres mode, and a real `docker stop postgres` survival drill (both recorded as open in M2's
`future-work.md`); and the Ruby 3.3/3.4 build experiment (ADR-0018 says so). None of these is claimed anywhere as done.

---

## Re-verification, 2026-11-07 — after the fix-and-publish pass (LOCAL runs; see the section above)

**Why this section exists.** Three months after the August record, the audit re-ran everything and found one
suite red for a reason that had nothing to do with any change: `coldmail-verify`'s raw-sidecar table had
partitions for 2026-07..09 only, so from 2026-10-01 every SMTP verdict write failed. A suite that was green
on 2026-08-07 was red on 2026-11-07 because the **date** moved. This section records the fixes and the runs
that cover them. The same rule applies as in August: **"verified" is a claim about a specific execution.**

### What changed (and where it is argued)

| change | repo | where |
|---|---|---|
| Vulnerable pgx / `x/text` / `x/net` and Go stdlib cleared by upgrading the **builder** (`golang:1.23-alpine` → `golang:1.26-alpine`, go1.26.10), not by reverting | M2, M3 (and `x/net`, `x/text` in M1) | `future-work.md` row + `go-live-checklist.md` §9 in each |
| `verification_raw` partition cliff: migration `0014`, a maintenance loop, dial `TM_RAW_PARTITION_MONTHS_AHEAD` | M1 | `Docs/adr/ADR-0017-raw-partition-maintenance.md` |
| #141: `acknowledge_alignment_warning` accepted but not recorded (admin create; both PATCH handlers) | M3 | `Docs/api-contract.md` |
| #131: RAM-backed Postgres made opt-in (`DEV_RAM_PG=1`), with a new gate `postgres_durability.sh` | M3 | `scripts/dev.sh`, `Docs/module-3-local-test-guide.md` |
| LICENSE + NOTICE (M2), `.gitignore` (platform), default branch `main` | M2, platform; M2/M3/platform | each repo |
| `.gitattributes` forcing LF — **found while verifying the published result**, see below | all four | each repo's `.gitattributes` |

`govulncheck` (live call paths), before → after, on the builder named above: **M1** `control-plane` 17 → 0;
**M2 / M3** `services/control` 41 → 0, `send-*-migrate` 27 → 0, `tests/harness` 27 → 0, `provision-worker` 0 → 0.
It reported "No vulnerabilities found" for all nine Go modules. **This is a dated result**: the advisory
database moves, so it is re-run at pilot time (each go-live document says so).

### The runs that cover the published commits

Cloned with a plain `git clone --recurse-submodules https://github.com/elzainxx/coldmail.git` (parent `1a6536b`),
then each repo cloned again from that checkout (`core.autocrlf=false`) into a clean directory and run in Docker:
clean-slate database/stack, full suite, every gate. Go ran only in containers. Nothing was run on the Windows host.

| Repo | Commit | Result | Evidence |
|---|---|---|---|
| **coldmail-verify** (M1) | `b212524` | **18 steps, 0 failed, ALL GREEN** | `PASS=401 FAIL=0 SKIP=5` (was `388 / 1 / 5` in the audit), 23 packages ok; `401 of 406 declared test(s) EXECUTED`, the 5 skips being the sanctioned PENDING-GAP harness; RSpec **71 examples, 0 failures**; `truemail (3.3.1)`; 14 migrations, tables 0 → 25 |
| **coldmail-send-managed** (M2) | `8afd970` | **19 steps, 0 failed, ALL GREEN** | `HARNESS RESULT: 62 passed, 0 failed`; 10 packages, every one `declared==ran skipped=0` (9, 16, 16, 2, 1, 9, 72, 168, 4, 54); 10 of 10 gates |
| **coldmail-send-byo** (M3) | `99e51ae` | **23 steps, 0 failed, ALL GREEN** — run in the **new default (non-RAM) mode** | `HARNESS RESULT: 100 passed, 0 failed`; 12 packages, every one `declared==ran skipped=0` (adminapi 105, platformapi 160 — up from 102 and 158 by exactly the tests added for #141); 12 of 12 gates including `postgres_durability` |
| **coldmail-platform** | `f461ee2` (this record is the next commit) | docs-only; citation checks below | see below |

The failing M1 test was `TestPGAppendWithRawSidecar`. It and every new test pass in that run:
`TestPGRawPartitions_LegacyCliffIsReal`, `…_MaintenanceKeepsNMonthsAhead`, `…_NoopMaintenanceLeavesTheCliff`
(the standing control: with a tick that does nothing the headroom guard **fails**), `…_RuntimeDoorIsNarrow`,
`…_BoundsAreUTCMonthAligned`, `…_ConcurrentTicksDoNotCollide`, and `TestBuildAPI_StartsPartitionMaintenance`.

**Earlier rounds, for completeness** (same method, the committed HEADs of that moment): after the
security/partition/#141/#131 commits — M1 `61ef119` 18 steps, M2 `4640dfb` 19, M3 `50dd2cb` 23, 0 failed each (M3 in
the RAM mode CI uses); after the hygiene commits — M1 `e5b1472` 18, M2 `ffe42d5` 19, M3 `e464b39` 23, 0 failed each.
The table above is the one that covers what was published.

**Every fix was break-verified, not just run green.** The guard was neutered and restored byte-identical each time:
the partition tick disabled (3 tests red), the `buildAPI` wiring call removed (the end-to-end test red while every
store test stayed green), the advisory lock removed (the concurrency test fails 3 of 3 with `relation … already
exists`), admin create no longer recording the acknowledgment, both PATCH handlers ignoring an unchanged-address
acknowledgment (each turns exactly its own test red), `dev.sh` re-merging the RAM override (the new gate fails on three
assertions), and an unrenderable compose config (the gate fails loudly rather than passing on an empty render).

### The platform repo's own check

`coldmail-platform` has no code; its "suite" is whether its citations resolve. The August runner (`cprun.sh`) was kept
outside the repos and **no longer exists**, so `tools/platform-check.sh` (outside the repos, in `coldmail/tools/`) is a
**re-implementation of four of its five checks** — not the original. The fifth (every sha attributed to a *named* repo
is in that repo) was **not** re-implemented. The new checker was itself checked against a throwaway copy with a broken
link, a broken cross-repo citation, invalid JSON and bogus shas planted: it failed on all of them (exit 1) and passes the
real tree (exit 0). That control caught one bug in the checker (a failure count lost in a pipe subshell).

Result on the published layout: **3 JSON files parse; 7 internal links resolve; 3 cross-repo citations resolve; 55 cited
commit shas resolve** (the August record plus the 11 this record cites itself; the parent repo's commit is searched too). Eight further hex strings do not resolve to commits — they are the same **subagent ids** the August
record excluded ("8 agent ids excluded"), named in `Docs/history/scratch/_inv_*.json`, not commit hashes. A green here means the
citations resolve, **not** that the prose is correct.

### Found while verifying the published result (not by the suites)

- **Submodule checkouts came out with CRLF.** `git clone -c core.autocrlf=false` configures only the **top-level**
  repository, so a `--recurse-submodules` clone on a `core.autocrlf=true` host checked the three modules out with CRLF —
  13 / 18 / 21 shell scripts — which bash and `gofmt` treat as content. Every suite had been green only because the
  runners clone with an explicit `core.autocrlf=false`. It was found by cloning the *published* parent and counting, and
  fixed in each repo with a `.gitattributes` (`* text=auto eol=lf`, plus an exact `-text` carve-out for every file already
  stored with CRLF, so no vendored or frozen byte changed; `git add --renormalize .` stages nothing but that file). A plain
  clone now has **0** non-exempt CRLF files; the exempt files still read CRLF by design (76 / 1 / 1 / 0).
- **The Docker engine dropped three times** on this 8 GB WSL VM (a `govulncheck` call-graph run, twice; a `compose build`
  that compiles two Go images at once). It is an engine failure (`rpc error … EOF`), not a build error; the retries used
  `--memory` / `GOMEMLIMIT` caps and `COMPOSE_PARALLEL_LIMIT=1`. The VM size is the owner's setting and was left alone.
- **Runner-process hygiene.** An earlier runner survived a `pkill` and started a second, concurrent M3 run; both were
  stopped and the affected runs redone from clean directories. Their output is not counted anywhere above.

- **GitHub Actions: red, and it has never been green.** Publishing the repos ran their workflows for the first time on a
  real Linux runner (identical results on the first push and on the final one). At `b212524` / `8afd970` / `99e51ae`:
  - **M1:** 4 of 6 jobs pass — docs-updated + config-reference freshness, no-secret-leak, **control-plane Go build + vet +
    test against real Postgres** (so the partition fix holds on a runner dated after the cliff), and the code-reviewer
    placeholder. The two `verifier-node` jobs fail: `FROM ruby:3.2.0-slim` is a Debian 11 (bullseye) image whose
    `debian-security` repository has been removed, so `apt-get install` returns 404 on a from-scratch build. Local runs reused
    cached image layers and never reached it — a second date-triggered defect of the same family as the partition cliff.
  - **M2 / M3:** 17 and 20 steps pass (lint, vet, certs, image build, and in M3 the new `postgres_durability` gate) and 3 fail,
    all downstream of one cause: the Control exits with `FATAL kms load … /secrets/kms/dev-master.key: permission denied`.
    `gen-secrets.sh` / `gen-certs.sh` deliberately `chmod 600` the dev keys; Docker Desktop's bind mounts hide that and a Linux
    runner enforces it against the non-root Control.
  - **Pre-existing, not introduced by this pass:** the original `zexSites/codmailling-module-3` repository's `real-infra` workflow
    failed on all four of its runs on 2026-07-31. Every green in this record is a local Docker Desktop run.
  - **Not fixed here, on purpose.** The M2/M3 fix is small to write but can only be verified on a runner, and more Linux-only
    failures probably sit behind it; the M1 fix replaces the base image of the component that holds Door 5 (the pinned engine).
    Both are owner decisions, recorded here so they are not discovered a third time.
### What this section does not claim

- **That the campaign-expiry gap the local test run recorded is closed.** Its evidence is in none of the four
  repositories, so it could not be located. `campaign_expiry_mode` defaults to `shadow`, so expiry does not enforce until promoted.
- **That a real provider works.** Nothing here touched one. The real-provider smoke test (M3 go-live §11) is still ahead.
- **That module 2 has #131 fixed.** It does not; it is registered in `coldmail-send-managed/Docs/future-work.md`.
- **That GitHub's CI is green.** It is not (see "GitHub Actions: red" above). The local runs execute each workflow's steps in
  Docker on a Windows host; a real runner is a different environment, and these workflows had never passed on one. M1's
  `code-review` job is a placeholder that is not locally executable.
- **That the secret sweep proves absence.** Two independent scanners (gitleaks, and a hand-written pattern sweep with its
  own planted-secret control) found nothing in the full history of any repo, except four lines of vendored upstream sample
  text in M1, accepted by exact fingerprint in `.gitleaksignore`. Pattern scanners do not find every secret.
- **That a re-run later would reproduce these numbers.** M1's own history is the counter-example.

---

## The four repos

| Repo | Commit verified | RUN 1 | RUN 2 | Toolchain |
|---|---|---|---|---|
| **coldmail-verify** (M1) | `c3f4d7d` | 18 steps, 0 failed, ALL GREEN | 18 steps, 0 failed, ALL GREEN | `golang:1.26` (CI pin) + `postgres:16-alpine` + the real node image |
| **coldmail-send-managed** (M2) | `9d7bba0` | 16 steps, 0 failed, ALL GREEN | 17 steps, 0 failed, ALL GREEN | `golang:1.23` (CI pin) + full compose stack |
| **coldmail-send-byo** (M3) | `b6db9e6`, then **re-run at HEAD `7805a1c`** | 18 steps → **19 steps** at HEAD, 0 failed, ALL GREEN | 19 steps → **19 steps** at HEAD, 0 failed, ALL GREEN | `golang:1.23` (CI pin) + full compose stack |
| **coldmail-platform** | `f2db5e5` | 4 checks, 0 failed, ALL GREEN | 4 checks, 0 failed, ALL GREEN | none required (docs only — see below) |

**On the differing step counts between RUN 1 and RUN 2 for M2 and M3:** the runner gained a step
between the two runs — the per-package `declared==ran && declared>0` assertion (class 16's fix). The
later run is the stronger one. This is noted rather than smoothed over, because two runs reporting
different step counts is exactly the kind of discrepancy that should not be left for a reader to
wonder about.

### The commit column is the commit RUN AT, not each repo's current HEAD

**Stated because the difference is exactly the kind of thing this file exists to prevent someone
assuming.** After these runs, each repo took one further docs-only commit closing its decision ledger,
so three of the four HEADs have moved past the verified commit:

| Repo | Verified at | HEAD after the ledger-close commit |
|---|---|---|
| M1 | `c3f4d7d` | `c3f4d7d` — **same**, the ledger close *is* this commit |
| M2 | `9d7bba0` | `9d7bba0` — **same** |
| M3 | `b6db9e6` | **`7805a1c`** — moved |
| platform | `f2db5e5` | moved (this file and its siblings are the reason) |

For M3 that drift is real and was **not** cosmetic bookkeeping: `7805a1c` was sitting uncommitted in the
working tree during both runs, so the runs did cover its content — but they were recorded against the
prior sha, and "the runs covered content that was uncommitted at the time" is a weaker and much less
checkable statement than a sha. **So M3 was re-run at `7805a1c` rather than argued about.** Both runs,
`dirty=0`, step-for-step identical:

```
HEAD: 7805a1c Ledger close: rule R2-5, register three ids that were ruled but never filed   dirty=0
  tools/send-byo-migrate/bin present? no
  gofmt + go vet (4 modules)                                 rc=0  OK
  go build tools/send-byo-migrate                            rc=0  OK
  scripts/dev.sh certs                                       rc=0  OK
  scripts/dev.sh reset (postgres->migrate->seed->rest)       rc=0  OK
  worker registered cleanly on FIRST attempt                 rc=0  OK
  scripts/dev.sh test (real-infra harness)                   rc=0  OK
        HARNESS RESULT: 100 passed, 0 failed
  per-package arithmetic: declared==ran, declared>0 (12 pkgs) rc=0  OK
  checks/config_drift.sh · contract_drift · migration_lint · config_consumption (+selftest)
  test_enumeration (+selftest) · drain_grace · no_secret_leak · dsn_scrub_wiring · user_output_scrub
                                                             all rc=0  OK
  scripts/dev.sh down                                        rc=0  OK
================ M3 SUMMARY ================
  19 steps, 0 failed
  ALL GREEN
```

RUN 1 and RUN 2 produced byte-identical step lists and both cleared the worker check on the **first**
attempt — the class-15 flake point, which passes 1-in-4 in its old state, so two clean first-attempts is
a weak but real signal on top of the fix's own break-verification.

The platform repo is re-verified by `cprun.sh` on every change; its latest run is in its section below.

---

## What each repo's run actually covered

### M1 — all six CI jobs, and why the runner had to be rebuilt to get there

`ci.yml` defines **six** jobs. The runner used through Chunk 4's earlier passes covered **one**
(`control-plane`) plus the gate scripts, because it executed entirely inside `golang:1.26` — and the
two Ruby jobs both `docker build` and `docker run`, which a container with no Docker client cannot do.
**A runner that omits two of six jobs cannot certify the repo**; it is a green that proves less than
its reader will take it to prove. Rebuilt host-driven, delegating Go work to the pinned container:

- `control-plane` — build, vet, **13 migrations applied to a dropped-and-recreated database** (0 tables
  before → 24 after), `go test -v ./...` against real Postgres, then `check-test-execution.sh` and its
  self-test. Result both runs: **`PASS=389 FAIL=0 SKIP=5`**, 22 packages ok. The five skips are the
  sanctioned PENDING GAP harness (`TestGAP37`, `TestGAP49`, `TestGAP24`, `TestGAP35`, `TestGAP14`) and
  are named in the output rather than aggregated into a count.
- `door5-gem-unmodified` — `assert-gem-unmodified.sh`, then a real `docker build` and
  `bundle list` resolving **`truemail (3.3.1)`**.
- `verifier-node-rspec` — **71 examples, 0 failures**, in the freshly built image.
- `no-secret-leak` — `check-no-secret-leak.sh`, boots the real binary with canary secrets.
- `docs-discipline` — `check-docs-updated.sh HEAD~1` (**RANGE mode, passed explicitly**, which is the
  mode CI uses and the one the N2 fix bounded), its self-test, `check-config-reference-fresh.sh`, and
  the enumeration meta-gate plus its self-test.
- `code-review` — **not locally executable** and not silently skipped. It is wired to the org reviewer
  at integration; what keeps it from vanishing is `check-test-enumeration.sh` **rule 6**, which fails
  if a PROJECT_RULES.md §7 job is absent from `ci.yml`, and which ran and passed in both runs.

**The clean slate is a `DROP DATABASE`, not a fresh container.** CI gets a new Postgres service every
run; a long-lived `m1-pg` does not. Without dropping, a migration that only works against existing
state would pass here and fail in CI. The two roles (`cp_runtime`, `cp_admin`) are cluster-wide and
survive the drop, which is deliberate: they are created once by `0005_grants.sql` behind
`IF NOT EXISTS` guards, and leaving them exercises that guard rather than bypassing it.

### M2 and M3 — the compose stacks

Both: gofmt + vet across all four modules, `dev.sh certs`, `dev.sh reset` in the **seeded order**
(postgres → migrate → seed → rest), the log-based worker anti-vacuity check, the real-infra harness,
then every blocking structural gate (10 for M2, 11 for M3), then teardown. M3 additionally proves
`tools/send-byo-migrate/bin` is **absent** first — it is gitignored, so a stale copy on the dev host
would earn a green no fresh checkout could reproduce — and builds it as an explicit step, which M2 does
not need because its harness self-builds at `tests/harness/run.sh:42`. That asymmetry is real and
porting either repo's wiring to the other would be wrong.

Final results: **M2 `HARNESS RESULT: 62 passed, 0 failed`** across 10 packages, every one
`declared==ran skipped=0`. **M3 `HARNESS RESULT: 100 passed, 0 failed`** across 12 packages, likewise.

### coldmail-platform — what "verification" means with no code

10 markdown files, 3 JSON files, **no code, no workflow, no suite**. "Run the suite twice" has no
referent here, and reporting *verified* because nothing failed would be the emptiest green in this
whole body of work — **a pass earned by the absence of anything that could fail.** So the property
being checked is stated instead of assumed. This repo's distinguishing feature is that it **cites the
other three**, and a citation to a path that no longer exists is the documentation form of the unwired
test: it reads as evidence and is not. Four checks at the time of these runs, five now, in
`_verify/cprun.sh`:

1. **Every JSON artifact parses** — 3 files.
2. **Internal markdown links resolve** — 5 relative targets.
3. **Cross-repo citations resolve** — 3 `../coldmail-*/…` targets. The property most likely to be
   false, because it depends on three other repos' trees and nothing in any of them would notice this
   file going stale. Two of the four broken links found during C4.5 were rename fallout of this kind.
4. **Every cited commit sha is a real commit** — **14 of 14 resolved** at the time of these runs,
   including `ae9f2a7` and `29e3bcb`, the two fix claims that `deferrals-and-false-greens.md` §4 makes
   about M2. A provenance claim is the one kind of claim here that can be flatly fabricated and the one
   a reader is least able to check by eye.
5. **Every sha attributed to a NAMED repo is in that repo** — added after these runs, during the
   task-list reconciliation, because step 4 had checked existence and not attribution and a real
   misattribution passed it. See class 19.

**Re-run 2026-08-07 after the reconciliation:** 5 checks, 0 failed, **43 of 43 shas resolve** (8 agent
ids excluded) and **24 of 24 repo-attributed shas are in the named repo**. The sha count tripled because
the reconciliation added a shipping commit to every item row — which is the point of check 4 existing:
the reconciliation's whole value is those citations, and they are now the most-checked claims here.

**A green here means its citations resolve. It does not mean its prose is correct** — and the file says
so in its own summary output, so the limit travels with the result.

---

## Two defects found in the verification tooling during these runs

Both are recorded in [`deferrals-and-false-greens.md`](deferrals-and-false-greens.md); they are noted
here because they are the reason the runs above are trustworthy and the earlier ones were not.

- **A failed build produced a green suite (class 13, instance b).** M1's Door-5 `docker build` failed
  and the rspec leg on the next line still reported `71 examples, 0 failures` — against a **three-day-old
  image** under the same tag. Two red steps, a green suite depending on them. Fixed by deleting the tag
  before building *and* refusing to run the suite when the build is red; break-verified.
- **A citation checker that matched nothing (class 18).** `cprun.sh` step 4's pattern used `` \` ``
  instead of `` [\`] ``, matching **zero of 22** ids and reporting *"0 of 0 resolve"*. The class-8
  nonzero floor is the only reason that was a red.

Both were in tooling I wrote earlier in the same work. **Neither was found by running the tools; both
were found by pointing tools at the tools.**

---

## What this file does not claim

- **That the systems are correct.** It claims a specific set of suites and gates executed at a specific
  commit and reported green, in a container, twice.
- **That the gates are sufficient.** Twenty false-green classes are recorded next door precisely
  because green has repeatedly meant less than it appeared to.
- **That a re-run today would reproduce these numbers.** It should, and the runners are kept outside
  the repos so they can be re-run without altering what they verify — but that is a prediction, and
  this file records measurements.
