# Verification runs — what was actually executed at the close of Chunk 4

**Recorded 2026-08-07.** Chunk 4 required, per repo: *clean-slate + full suite + all gates, in Docker,
TWICE, pasted.* This file is the record of what ran, at which commit, and what it produced. It exists
because **"verified" is a claim about a specific execution**, and a claim about an execution that is
not written down is indistinguishable from one that did not happen.

**Every result below came from a container.** Nothing was run on the Windows host: this host silently
does not execute freshly-built Go test binaries, and a host-run Go result is therefore not admissible
evidence — a green from it would be the absence of an execution, not the success of one.

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
