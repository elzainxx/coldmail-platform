# Verification runs — what was actually executed at the close of Chunk 4

**Recorded 2026-08-07.** Chunk 4 required, per repo: *clean-slate + full suite + all gates, in Docker,
TWICE, pasted.* This file is the record of what ran, at which commit, and what it produced. It exists
because **"verified" is a claim about a specific execution**, and a claim about an execution that is
not written down is indistinguishable from one that did not happen.

**Every result below came from a container.** Nothing was run on the Windows host: this host silently
does not execute freshly-built Go test binaries, and a host-run Go result is therefore not admissible
evidence — a green from it would be the absence of an execution, not the success of one.

---

> **Two records live in this file.** The section directly below is the **current** one (2026-11-07, after the
> fix-and-publish pass). Everything from "The four repos" onward is the **August 2026 record, kept as written**:
> it is history, and its commit hashes and counts describe the trees as they were on 2026-08-07.

---

## Re-verification, 2026-11-07 — after the fix-and-publish pass

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
record excluded ("8 agent ids excluded"), named in `scratch/_inv_*.json`, not commit hashes. A green here means the
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

### What this section does not claim

- **That the campaign-expiry gap the local test run recorded is closed.** Its evidence is in none of the four
  repositories, so it could not be located. `campaign_expiry_mode` defaults to `shadow`, so expiry does not enforce until promoted.
- **That a real provider works.** Nothing here touched one. The real-provider smoke test (M3 go-live §11) is still ahead.
- **That module 2 has #131 fixed.** It does not; it is registered in `coldmail-send-managed/Docs/future-work.md`.
- **That the CI workflows were run.** No GitHub Actions run happened; the *steps* of each were run locally in Docker.
  M1's `code-review` job is a placeholder that is not locally executable.
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
- **That the gates are sufficient.** Nineteen false-green classes are recorded next door precisely
  because green has repeatedly meant less than it appeared to.
- **That a re-run today would reproduce these numbers.** It should, and the runners are kept outside
  the repos so they can be re-run without altering what they verify — but that is a prediction, and
  this file records measurements.
