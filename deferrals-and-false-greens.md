# Deferrals and false greens — the permanent cross-module record

**Written 2026-08-07, at the close of Chunk 4.** This is the PASS-3 deliverable that
[`scratch/_deferrals.md`](scratch/_deferrals.md) existed to feed. That file is working notes and stays
as provenance; **this file is the record.** Two things live here and nowhere else:

1. **Every deferral**, with what blocks it and — where the deferral was itself mis-recorded — what the
   corrected premise is.
2. **The false-green classes** found across this work, each with a citation to the artifact that
   demonstrates it. There are **twenty**, up from the fourteen named at the start of Chunk 4 (the twentieth was added
   2026-11-08, from the first real GitHub Actions runs — see class 20).

The organising claim, stated once: **a green signal is evidence only if something about it could have
been red.** Every class below is a way that stopped being true while the signal kept its colour.

---

## 1. Deferred by ruling — Tier 4 (items 16 and 17)

**Ruled out of scope by the brief, not by a technical finding.** Module 1 adopting the senders'
`quote`/`run`/`report`/`describe` surface. Nothing was built toward it and nothing depends on it.

Recorded here because "M1 lacks a `describe` endpoint" is the sort of gap a later audit rediscovers and
files as a defect. It is a deliberate scope boundary: M1 is a verifier with its own contract, and
convergence on the sender contract was considered and declined for this arc.

---

## 2. Deferred by ruling — items 15 and 15b, with the 15-C redesign requirement

**Tier 3 parity. Not blocking. Deferred to future work, with one requirement that must survive the
deferral**, because a future implementer who reads the item as filed will make a specific mistake.

### 15-C — the migration/runtime role split, and why it needs a REDESIGN and not an implementation

**As filed, 15-C would make [`TestPGGrants_RoleSwitchActuallyBites`](../coldmail-verify) VACUOUS.**
The mechanism, stated exactly so it cannot be re-derived incorrectly: from a session already connected
**as** `cp_runtime`, `SET LOCAL ROLE cp_runtime` is a **no-op**. The test proves the role switch "bites"
by showing one direction permitted and the other refused; from a `cp_runtime` session **both directions
pass**, so the test goes green while asserting nothing. That is not a bug in the test — it is what
happens when you change the connecting identity underneath an assertion written against a different one.

**So the deferral carries a requirement:** whoever takes up 15-C must **redesign that test first**, to
assert against the *effective* role and the *connecting* role separately, and must break-verify the
redesign (make the switch not bite; watch it go red) **before** changing any connection identity. In the
other order the test silently stops testing, and the change looks like a success.

### The corrected premise — the "shared work" discount was FALSE

The brief bundled 15-C with item 3 on the reasoning that item 3 needed `cp_runtime` to be
**connectable**, so the two could share the cost. **That premise is false.** Item 3 shipped via
`SET LOCAL ROLE`, which requires no connectable role at all — `cp_runtime` never needed to be
connectable and still isn't. There is no shared work and no discount: 15-C stands alone, at its own
full cost, plus the test redesign above.

**Worth keeping as a class.** A cost estimate that depends on two items sharing a prerequisite is only
as good as that prerequisite, and here the prerequisite was never checked against the shipped
implementation. The estimate was wrong in the *cheap* direction, which is the dangerous one.

### 15b's other rows

Carried forward as filed, with two corrections established during Chunk 3 and preserved here:

- **`DisallowUnknownFields()` is a SEQUENCING problem, not a deferral.** It would destroy
  `pg_tenancy_test.go:287-293`'s forged-owner proof, which requires **202** on a body carrying forged
  `user_id`/`owner_user_id`/`owner`. **Re-express that case to assert the STORED owner rather than the
  status code FIRST, then tighten the decoder.** In the other order, the obvious way to make the test
  pass is to stop sending the forged fields — which silently deletes the proof.
- **M3's bare DB timeouts (D10): the plan names 1 site; there are 42.** An ~8× miss, so this is not the
  LOW chore it is listed as. Re-estimated, not absorbed.

---

## 3. Deferrals blocked on a DECISION, not on work

Each was pulled out of C3 because starting before the ruling means building the wrong thing cheaply or
the right thing expensively.

| Item | What blocks it |
|---|---|
| **`request_id` in every log line** | A **scope ruling**, not work. `canonical-module-contract.md:297` says "Every log line"; `:74` says "at least one". M1 satisfies the narrow clause and fails the broad one at **47 Go sites plus 2 Ruby logger references**. LOW vs HIGH turns entirely on which clause governs. **The contract contradicts itself** — that is the thing to fix first. |
| **Dedicated schema (D7)** and **migration tracking (D6)** | Both interact with M1 having **no down-migration mechanism at all**. A tracking table recording what ran, in a system that cannot un-run anything, records a fact nobody can act on. See [`coldmail-verify/Docs/go-live-rollback-posture.md`](../coldmail-verify/Docs/go-live-rollback-posture.md) — tracking is worth adopting **before the second environment exists**, and that is the one time-sensitive item in this whole record. |
| **Prometheus `/metrics` (all three modules)** | Nothing exists in any module. Genuinely MEDIUM and correctly sized — the only row here that was estimated right. |
| **GAP-37 adjudication** | Re-estimated **MEDIUM–HIGH**. `SKIP LOCKED` never lock-waits, so M3's `pg_stat_activity`-blocked-count harness is **structurally inapplicable** and a different instrument is required. Two claimed defects here (hardcoded `WorkerID`, per-batch `locked_at`) were investigated and **REJECTED** — unreachable in the single-drainer shipped topology. |
| **M1 `TM_*` → `VERIFY_*` env prefix** | **RULED: keep `TM_*`**, honouring ADR-0005. A rename needs a superseding ADR, not a silent change. |
| **M1 schema introduction (D7), `cp_runtime` login + third role, the 12 missing `.down.sql`** | Migration-shaped changes, out of a rename pass's scope. Note the `.down.sql` row is **superseded**: M1's forward-only posture is now a documented deliberate design with its own rationale, not a gap awaiting 12 files. |
| **A request-path leak scan for M1** | Cannot run until item 15's compose stack exists. Genuinely blocked on infrastructure. |

---

## 4. Deferrals mis-recorded in the ledger itself — found by checking, 2026-08-07

**The deferral ledger had its own stale rows.** Both directions of error appeared, which is why this
section exists: a register of deferred work is itself an artifact that rots, and nothing was checking it.

### `FW-M2-A` and `FW-M2-D` were recorded as OPEN in M2's `Docs/future-work.md`. Both are FIXED, and neither ever lived in that file.

`scratch/_deferrals.md:20` reads *"**M2** — FW-M2-A, FW-M2-B, FW-M2-C, FW-M2-D (`Docs/future-work.md`)"*.
Measured: **no `FW-M2-*` id has ever appeared anywhere in the `coldmail-send-managed` tree.** The ids
exist only as headings in this repo's own [`pass-1-findings.md`](pass-1-findings.md). Their real state:

- **`FW-M2-A` — M2's harness never built its `migrate` binary. FIXED in M2 `ae9f2a7`.** The fix is
  `tests/harness/run.sh:42`, which now self-builds it (`GOTOOLCHAIN=local go build -o
  bin/send-managed-migrate .`). **This is a genuine per-module asymmetry worth recording:** M2's harness
  builds its own binary, so M2's CI needs no separate build step, whereas M3 builds it as an explicit CI
  step (`real-infra.yml:34`) — the same tool, two different wiring points. Porting either repo's CI step
  to the other would be wrong.
- **`FW-M2-D` — M2's SUITE 5 fails on a re-run. FIXED in M2 `29e3bcb`.**
  `tests/harness/phase3_send.sh:148` hard-kills the worker so nothing can report on the stranded T3 job;
  `:179` now brings it back (`up -d --no-deps worker`). The original defect is worth keeping for its
  *shape*: a dead container made `docker compose exec` return rc≠0, which made SUITE 5's **negative**
  KMS check pass spuriously ("path absent"), leaving only its anti-vacuity control failing — the
  observed PASS/**FAIL**/PASS. **A negative assertion that passes because the probe could not run is
  class 10 below.**
- **`FW-M2-B` and `FW-M2-C` do not exist.** They are cited once, at `pass-1-findings.md:89`
  (*"Also open from earlier passes: FW-M3-13/14 and FW-M2-B/C"*), and **defined nowhere** — no heading,
  no body, no subject. There is nothing to close and nothing to carry, because there is nothing there.
  Recorded rather than deleted, because a future reader will find that citation too.

### `FW-M3-6` was RULED into existence and never registered

M3-P8-OD-6 was RULED-ACCEPTED on 2026-07-31 with the disposition *"Register as `FW-M3-6`, do not build
in Phase 8."* The do-not-build half was honoured; **the register half was not.** `FW-M3-6`, `-7`, and
`-8` were cited by other documents while being defined nowhere in `Docs/future-work.md` — including
`shared-code-manifest.md:184`, which claimed of `FW-M3-8`: *"Also tracked here, not only in
`future-work.md`"*, an explicit and false claim about a file. **All now registered** in that file's new
§9a, along with `FW-M3-18` (an id-resolvability gate) and `FW-M3-19` (a link-integrity gate). The M3
ledger's own disposition table also still said **"STILL NEEDING A RULING — 2"** while its prose said, correctly, that both were ruled on 2026-08-04; corrected in place.

**Both are the same failure:** a ruling whose entire disposition is *"file this"* is not self-executing,
and nothing checks that a disposition of that form was carried out.

---

## 5. Genuinely unclosable — module 1's twenty real-world-gated rows

[`coldmail-verify/Docs/pending-ledger.md`](../coldmail-verify/Docs/pending-ledger.md) holds twenty rows
(B1–B8, V1–V5, S1–S5, H1–H2) and **all twenty stay open**. Each was tested against one question: is the
blocker a real-world input that does not exist yet, or is it a decision/build someone is avoiding?
**All twenty fail to be closable for the first reason** — §6 of that file records the per-row grouping.
Summarised: 6 need a machine with a specific network identity · 3 need an un-simulatable counterparty ·
2 need a commercial or legal act · 2 need a system that does not exist yet · 4 need accumulated time or
volume · 3 need go-live infrastructure. **Zero are blocked on a decision.**

One nearby item is **not** in the twenty and is called out to keep the distinction visible: the
`port25_open` write path is real, unbuilt work with a known shape (no operator/env/admin command exists
to flip it true; interim is a `cp_admin` break-glass `UPDATE`). **The reputation behaviour is
unclosable; the plumbing to record it is merely unbuilt.**

---

## 6. The twenty false-green classes

Each is a distinct mechanism by which a passing signal stopped being evidence. Fourteen were known
entering Chunk 4; **five were found during it** (15, 16, 17, 18, 19); **one more on 2026-11-08** (20).
Citations are to the artifact that demonstrates the class, so each row is checkable rather than a claim.

**§7 below predicted an eighteenth, then a nineteenth, then a twentieth. All three arrived** — 18 before this
file was first finished, 19 during the task-list reconciliation immediately after, and 20 three months later, from
outside the verification tooling: from running the same repos on a machine that was not the developer's. The first
five of Chunk 4's new classes all came from the tooling itself; class 20 did not, which is part of what it adds.

| # | Class | Where it was demonstrated |
|---|---|---|
| 1 | **The phantom fix** — the defect is described, a fix is written, and the fix addresses something else | 0013's REVOKE, believed to be an unrevertable privilege drop; measured on two live databases, `cp_runtime` holds exactly `SELECT` on `api_callers` **with and without** it. Had I "fixed" it, the fix would have protected nothing |
| 2 | **The vacuous test** — passes without exercising the property | `TestPGGrants_RoleSwitchActuallyBites` under 15-C: from a `cp_runtime` session, both directions pass |
| 3 | **The claimed-but-unwritten test** — a doc asserts a test exists; it does not | M3's Phase-7 spec asserts *"this phase is that consumer"* of `score_alert_threshold` as **fact**. It was never built, and the claim survived Phase 7's own closeout |
| 4 | **The unwired test** — exists, compiles, is never run | Six Go test packages in M2+M3 that no runner enumerated (C3.0) |
| 5 | **The gate satisfied by PROSE about the thing it guards** | A gate whose condition was that a *heading* existed in `future-work.md` — kept forever at no cost, so it passed every run and never came due. Ruled WITHDRAWN 2026-08-05; now a hard failure in `config_consumption.sh` |
| 6 | **The stale assertion** — still true, no longer means what a reader takes it to mean | `security-posture.md`'s "the database password never appears in a log line", true as written and false in effect at pgx v5.7.4 |
| 7 | **The gate inheriting a green from the previous commit's diff** | `check-docs-updated`: with every doc reverted so the tree touched **no** doc, it still printed `ok (code_changed=true docs_changed=true)`, rc=0 (N2) |
| 8 | **The vanished subject** — `declared=0` compares equal to `ran=0` and passes trivially | Exactly how **3 of 10** selftest branches passed by omission. Closed by a nonzero floor (C4.1), break-verified |
| 9 | **The unreachable guard producing the right exit code** | A guard that cannot be reached still returns 0, which is indistinguishable from a guard that ran and approved |
| 10 | **The reject-case fixture that also fails for an unrelated reason** | `FW-M2-D`: a dead container made `exec` return rc≠0, so SUITE 5's **negative** KMS check passed as "path absent" |
| 11 | **The window-based gate crediting a NEIGHBOURING call's fix** | A scrub gate whose match window spanned an adjacent, already-fixed call site |
| 12 | **The counterweight test whose FIXTURE SHAPE dodges the bug** | The pgx DSN trigger differs by version: `sslmode=bogusmode` fires at v5.7.4 where M1's `pool_max_conns=notanumber` does not. **Porting M1's test verbatim would have been vacuous** (N1) |
| 13 | **The green earned by a stale build artifact** | Two instances. **(a)** A `bin/` present on the dev host and absent on every fresh checkout — the reason M3's runner now deletes it and proves the absence first. **(b) OBSERVED LIVE 2026-08-07, not inferred:** M1's Door-5 `docker build` failed (a Windows path-form error in my own runner), and the rspec leg on the next line still reported **`71 examples, 0 failures`, rc=0** — against a **three-day-old** image sitting under the same `truemail-verifier-node:ci` tag. Two steps red, the suite they depend on green, asserting a property of a three-day-old `Gemfile.lock`. Fixed by `docker rmi -f` on the tag before building, which makes "the build worked" a precondition rather than an assumption |
| 14 | **The ADR that names a bug then falsely claims the mechanism prevents it** | An over-suppression defect described accurately in an ADR whose stated mechanism did not prevent it |
| 15 | **The anti-vacuity check that is FLAKY IN THE PASSING DIRECTION** ← *new* | My own worker check, state-only form (`running=true && RestartCount=0`). The register reject lands **~1s after start**, so a sample taken the instant `up` returns reads clean on a doomed worker. Reproduced deliberately: **4 unseeded attempts — attempt 1 PASSED (false green), 2–4 failed. 1 in 4.** Fixed in both senders with log-based assertions on terminal, monotonic lines; re-verified M3 4/4 RED + 3/3 GREEN, M2 3/3 RED + 3/3 GREEN |
| 16 | **The report that TRUNCATES ITS OWN EVIDENCE** ← *new* | `m2run.sh` piped per-package `counts:` lines through `tail -30`, keeping only the **last** package's arithmetic. A `declared>ran` shortfall in any earlier package would have been absent from the pasted evidence while the run still read ALL GREEN. **A report that truncates its evidence is indistinguishable from one with nothing to report.** Fixed to print all lines and to *assert* `declared==ran && declared>0`; break-verified on synthetic inputs including a shortfall in first position |
| 17 | **The mis-recorded deferral** ← *new* | §4 above, in both directions: two items recorded as OPEN in a file they never lived in (both actually FIXED, in `ae9f2a7` and `29e3bcb`), two ids cited but **never defined at all**, and one ruled *"register it"* whose registration never happened. **A deferral ledger is an artifact, and nothing was checking it** |
| 18 | **The check whose PATTERN MATCHES NOTHING, so its subject is empty and its arithmetic passes** ← *new* | This file's own citation checker, `_verify/cprun.sh` step 4, written to verify that every commit sha cited here is real. Its extraction used `` \` `` where `` [\`] `` was required; `grep -E` reads the escaped form as a literal backslash-plus-backtick, so it matched **zero of the 22 ids present** and the check reported *"0 of 0 cited shas resolve"*. **Only the nonzero floor turned that into a red** — the floor from class 8, added days earlier for a different reason, catching a defect in the tool that was auditing this document. Related but distinct from 8: there the subject vanished from the system under test; here the **instrument never found it**, and a green would have certified 22 unchecked provenance claims |
| 19 | **The check whose ASSERTION IS NARROWER THAN THE CLAIM ITS GREEN INVITES** ← *new* | The same checker, one step later. `cprun.sh` step 4 asserts a cited sha is a real commit in **some** sibling repo; it never checked **which**. During the 2026-08-07 task-list reconciliation I wrote `` `0b42e1b` `` (M3) into `alignment-fix-plan.md` — a real commit, in **M2**. Step 4 passed it, correctly by its own definition, and printed **"43 of 43 cited shas resolve to a real commit."** Nobody reads that and concludes *"…though possibly attributed to the wrong module"* — yet in a document whose entire value is per-module provenance, the module is the load-bearing half. The misattribution was caught only because I happened to re-derive all 24 rows by hand against each named repo; nothing in the tooling would have. Fixed by **adding step 4b** (attribution, where the text states a repo) rather than widening 4, since attribution is only checkable where it is claimed — 24 of 43 shas carry an explicit repo, and that ratio is now printed so the unchecked remainder stays visible instead of hiding inside a green. Break-verified by re-injecting the exact real mistake: RED, naming where the sha actually lives; GREEN on restore |
| 20 | **The environment-masked green** ← *new, 2026-11-08* | A green earned on the wrong machine. Until this date "green" in all three code repos had only ever meant *a run on the Windows developer host with Docker Desktop*; the first run on a real GitHub Actions runner was **red in all three**, and each layer peeled off showed another, every one invisible locally because the dev environment differed from a Linux runner in a way that decided the outcome — and every one in the **passing** direction. Measured, not inferred: **(a) bind-mount ownership** — Docker Desktop ignores POSIX ownership, so keys that were mode 600 and owned by the host user were unreadable to the Control's uid 10001 on Linux (`FATAL kms load … permission denied`); **(b) a warm cache over a dead base** — `ruby:3.2.0-slim` is Debian 11, whose security repository is gone, so `apt-get` 404s on any cold build, and every local build reused a layer; **(c) a host-timing race** — the test SMTP sink greeted twice after STARTTLS, which the Windows host happened to swallow; on Linux (Node 20 *and* 24, 3/3 runs each) it shifted every reply by one and 10 worker tests failed, the S1/S3 credential-scrub tests among them; **(d) shell semantics** — Windows bash strips `\r` from `$(…)`, so header checks written `^…$` passed there and could never match Mailpit's CRLF; **(e) `bash -e`** — `grep -c` exits 1 on zero matches and killed a CI step on its *healthy* path; **(f) toolchain version** — `node --test "glob"` expands only on Node ≥ 21, so on Node 20 the worker unit suite ran **zero** tests; **(g) a SIGPIPE race** — `sed \| grep -q` under `pipefail` made the consumption gate call keys that are plainly read "ONLY INSIDE COMMENTS" (the committed gate failed 15/24 runs in M3 and 11/24 in M2 under CPU contention, the fixed gate 0/24), and the same pattern in the harness's `go_suite` could record a failing Go package as PASSING once its output outgrew a pipe buffer (demonstrated on a throwaway package; none found in the green logs); **(h) a resolver search domain** — the From-gate's DNS-failure handling depends on the host's `/etc/resolv.conf`: a runner (which has a `search` line) fails `TestEvaluate_T5…`, the dev host and the replica (which have none) pass; reproduced by changing that one line. **(h) is the one that is not a test artefact — it is a property of the product gate** (M3-CI-OD-1 — since ruled and fixed; the same class was then found in module 1, see the second-pass bullet below). |

**What class 20 adds, and what now controls it (2026-11-08).** Classes 15–19 are failures *in the verification
apparatus*. This one is different: the apparatus was sound and ran correctly — on a machine that is not the one the claim is
about. The question it adds is *where did this green run?* Four things are worth stating, each from this work and not
from theory:

- **The investigator's own tools shared the mask.** The first diagnosis of (d) ruled CRLF *out*, because host-side `sed` and
  `awk` strip `\r` when they read a file, so the failing log looked LF-only; `tr -cd '\r' < file` printed 15 (one per header
  line) and was misread as unrelated; and a "replay" built to confirm the check passed for the same reason, because its input
  had been rebuilt through the same tools. Only `cat -A` run *inside a Linux container* showed `^M` on every line. The wrong
  diagnosis cost a CI cycle.
- **A rehearsal that passes is not a pass.** A Linux replica of the runner (uid 1001, the repo on the VM's own ext4,
  `tools/ci-replica.sh` in the parent repo) found (a)–(f) in minutes, then passed modules 2 and 3 outright — and both then
  failed on the real runner, for (g) and (h). The replica is worth having and is **rehearsal only**; the GitHub Actions run is
  the only citation.
- **Controls, each shown to fail when its property is broken:** CI on a real runner as the source of truth; base images
  pinned by digest and built `--pull --no-cache` (`check-base-image-pinned.sh` + an 11-case selftest); `secret_ownership.sh`
  (fails if a key is loosened or the worker uid can read the KMS key); `sink_fidelity.test.mjs` (93 pass / 11 fail on the old
  sink, 104 / 0 on the fixed); a new `config_consumption_selftest` case (exit 1 against the old gate); `go_suite_selftest.sh`
  (3 of 5 assertions fail against the old `run.sh`); the T1 header check now names the condition that tripped and dumps
  `cat -A`; a failing Go package now prints its assertion text instead of only its verdict.
- **Added in the second pass (same day), each shown to fail when its property is broken.** (h) is RULED and fixed in module 3
  (M3-CI-OD-1: every lookup fully qualified, "no record" a narrow door), and the same class was found and fixed in **module 1**,
  where it was the bigger risk — a SERVFAIL overwritten by the suffixed name's NXDOMAIN made the MX layer say `invalid / no_mx`,
  *our DNS outage stated as a fact about the user's address* (ADR-0019). Both have a permanent search-domain CI fixture that
  injects a `search` line into `resolv.conf`, a canary that proves the hazard is live under it, and a paired MUTANT control that
  disables the fix and must go red (the literal pre-fix code also reds: T5 passes with no search domain, 3.00 s, and fails with
  one). GitHub's own runners have an Azure search domain — module 1's *plain* `go test` printed the canary there. `image_pins`
  (no `:latest`/untagged image anywhere; Mailpit, CoreDNS and the DNS black-hole fixture pinned by digest; 13-case selftest);
  `check-gemfile-lock` (a committed `Gemfile.lock` installed frozen, agreeing with Door 5's pin; 14-case selftest); and the
  Postgres-outage drill on a runner for both senders (22/22 each — see `verification-runs.md`).
- **Not controlled — stated so it is not mistaken for coverage** (rewritten 2026-11-09; four items that stood here are now controls — see
  `verification-runs.md`, "Third pass"): Ruby 3.2 is past end-of-life (M1 ADR-0018) and the 3.3/3.4 move has not been attempted — tied to
  M1's go-live; `node:20-alpine` and `alpine:3.20` (now digest-pinned, so frozen) are old releases whose support status was **not verified**
  from the build host — treat as the same shape as the Ruby question; the SMTP probe's **destination is unfiltered** (loopback, RFC 1918,
  link-local, metadata on port 25; pre-existing, found by the implicit-MX audit, M1 ADR-0020 — needs an egress policy or a fork pre-flight);
  the `image_pins` shell-tooling scan is a **heuristic** (an image from a repository name it does not know is invisible to it);
  macro-bearing SPF targets are looked up as literals and can reach a false BLOCK under posture `block` (M3-CI-OD-3, flagged, awaiting a ruling).
  *Closed 2026-11-09:* builder/base tags are digest-pinned and the gate rejects any unpinned reference (all three repos + the parent's tool
  images); the node's resolver rule is a control (the audit goes red; M1 ADR-0019 addendum); the A-only false `invalid` is fixed (M1 ADR-0020);
  the SPF `a/24` misparse is fixed (M3-CI-OD-2).
  One failure remains **unexplained**: module 3's `Phase 3 dispatch` package once failed with all 231 of 231 tests having run and
  no `--- FAIL` line (run 42438687947), never again in the harness runs since. It is deliberately not being chased;
  if it recurs, check the `go_suite` SIGPIPE race first (`go_suite_selftest.sh` pins that class).

**What the five new ones from Chunk 4 have in common, and why it is worth saying.** All five are failures in the
**verification apparatus**, not in the systems under test — a flaky check, a truncating report, a
rotten register, an instrument whose pattern matched nothing, and an instrument whose green overstated
itself. Classes 1–14 were mostly found by
pointing tools at the repos. **These were found by pointing them at the tools.** Class 15 in particular
was my own fix from earlier in the same work, and it passed 1 time in 4 on a stack that was definitively
broken; had I run it once and moved on, it would have shipped as a control that certified the exact
defect it was written to catch. **Class 18 is the sharpest of the four**, because it was caught by
another class's fix: the nonzero floor built for class 8, in a different repo, days earlier, is the
only reason a checker that examined **zero of 22** citations reported red instead of green. A control
built for one hole caught a different hole in a different tool. That is the argument for floors over
comments, made by the artifacts rather than by me.

**Instance (b) of class 13 is a further apparatus failure, and it is filed under 13 rather than as its
own class on purpose.** It was found the same way — in my own M1 runner, on its first execution. It is
not a new mechanism: it is the *same* one as the stale `bin/`, which is why it belongs there. What it
adds is that the class had been **inferred** from a gitignored directory and is now **observed**, with
two red steps sitting directly above a green suite that depended on them. The sharper lesson is about
ORDER: the runner recorded the build as FAILED, correctly, and then ran the next step anyway. **A step
that depends on a previous step's artifact must either check that the artifact is fresh or refuse to
run** — recording the dependency as failed is not the same as declining to trust it. Both halves were
fixed (`docker rmi -f` on the tag, plus an explicit refusal when the build is red) and break-verified
in `_verify/.breakverify-m1-guard.sh`: build-failed now yields RED / RED / suite-not-invoked.

**Two more findings from that same break-verify, kept because they are instructive and not because
they were consequential.** Both happened *inside* a control written to catch class 13:

- **The break-verify's reject case first passed FOR THE WRONG REASON — class 10, inside a class-13
  control.** The probe tag was `_bv_should_not_exist:x`, and Docker rejects a leading underscore as an
  invalid reference *before* it ever resolves the context path. The case went red on tag validation
  while the thing it meant to exercise was never reached; it would have read OK forever. What caught
  it was printing the actual failure line and requiring it to **match the expected signature** rather
  than merely being a failure. **A reject case must fail for the reason it names**, and the only way
  to know it does is to look at the reason.
- **Fixing that exposed a MISDIAGNOSIS in my own fix comment.** I had recorded the cause as the
  `/d/...` path *form*. It is not: Git Bash auto-converts that form for the Docker CLI and the bare
  path builds fine. The real cause is `MSYS_NO_PATHCONV=1` **suppressing** the conversion — correct
  for `-v`, wrong for a build context. Measured both ways, which is the only reason the cause is now
  stated correctly. **A fix that works can still be recorded with the wrong cause**, and the wrong
  cause is what the next person reasons from.

---

## 7. What is NOT recorded here

- **Anything already recorded in a repo's own docs** — M1's rollback posture, M2's and M3's R2-5
  rulings, the per-repo future-work registers. This file points at them; it does not restate them.
- **The task list.** Reconciled separately at the close of PASS 3.
- **A claim of completeness about the twenty classes.** They are the ones **found**, each with a
  citation. The useful thing this list provides is not a count but a set of questions to ask of the next
  green signal: *could this have been red? did anything verify the verifier? is the evidence complete or
  truncated? is the subject still there? did the instrument actually find it? **and does its green mean
  as much as it sounds like it means?***

  **This section has now made the same prediction twice and been right twice.** It first read *"an
  eighteenth is likely"*; class 18 arrived within the hour, in the tool written to check this file. It
  was then edited to read *"a nineteenth is likely too"* — and **class 19 arrived during the very next
  task**, in the same tool, one step further along, and was caught by hand rather than by any control.
  Both predictions are left standing rather than smoothed away, because two-for-two is a stronger
  statement than either was alone: **a twentieth is likely.** The count is a running tally of what has
  been looked for, not a measure of what is there. Treat any future green in this body of work as
  provisional in exactly the way each of these nineteen turned out to be.

  **Update 2026-11-08 — three-for-three.** The twentieth arrived, as predicted, and from a direction the first nineteen did
  not come from: not from pointing tools at the tools, but from running the same repos on a machine that was not the
  developer's. It also arrived as eight findings at once (class 20, (a)–(h)), one of which — the From-gate's DNS handling
  under a resolver search domain — is a defect in the product and not in any test. The prediction stands for the same
  reason it has stood before: **a twenty-first is likely**, and the likeliest place to look is wherever a check has so far
  run on only one kind of machine.

  **The sharpest thing class 19 adds is a question the first eighteen do not ask.** Classes 4, 8 and 18
  are all *"the check did not run on its subject."* Class 19 is different and harder: the check ran, on
  the right subject, and **its assertion was true** — `43 of 43` was an accurate sentence. The defect
  was the gap between what it asserted and what its wording invited a reader to conclude. That cannot be
  closed by a floor or a break-verify, because there is nothing broken to break. It is closed only by
  **reading your own green out loud and asking what a reasonable person would take it to mean** — which
  is why step 4b's output now prints `24 of 43 carry an explicit repo`, making the unchecked 19 part of
  the result instead of part of the silence.
