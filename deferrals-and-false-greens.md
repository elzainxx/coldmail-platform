# Deferrals and false greens — the permanent cross-module record

**Written 2026-08-07, at the close of Chunk 4.** This is the PASS-3 deliverable that
[`scratch/_deferrals.md`](scratch/_deferrals.md) existed to feed. That file is working notes and stays
as provenance; **this file is the record.** Two things live here and nowhere else:

1. **Every deferral**, with what blocks it and — where the deferral was itself mis-recorded — what the
   corrected premise is.
2. **The false-green classes** found across this work, each with a citation to the artifact that
   demonstrates it. There are **seventeen**, up from the fourteen named at the start of Chunk 4.

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

## 6. The seventeen false-green classes

Each is a distinct mechanism by which a passing signal stopped being evidence. Fourteen were known
entering Chunk 4; **three were found during it** (15, 16, 17). Citations are to the artifact that
demonstrates the class, so each row is checkable rather than a claim.

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
| 13 | **The green earned by a stale gitignored build artifact** | A `bin/` present on the dev host and absent on every fresh checkout — the reason M3's runner now deletes it and proves the absence first |
| 14 | **The ADR that names a bug then falsely claims the mechanism prevents it** | An over-suppression defect described accurately in an ADR whose stated mechanism did not prevent it |
| 15 | **The anti-vacuity check that is FLAKY IN THE PASSING DIRECTION** ← *new* | My own worker check, state-only form (`running=true && RestartCount=0`). The register reject lands **~1s after start**, so a sample taken the instant `up` returns reads clean on a doomed worker. Reproduced deliberately: **4 unseeded attempts — attempt 1 PASSED (false green), 2–4 failed. 1 in 4.** Fixed in both senders with log-based assertions on terminal, monotonic lines; re-verified M3 4/4 RED + 3/3 GREEN, M2 3/3 RED + 3/3 GREEN |
| 16 | **The report that TRUNCATES ITS OWN EVIDENCE** ← *new* | `m2run.sh` piped per-package `counts:` lines through `tail -30`, keeping only the **last** package's arithmetic. A `declared>ran` shortfall in any earlier package would have been absent from the pasted evidence while the run still read ALL GREEN. **A report that truncates its evidence is indistinguishable from one with nothing to report.** Fixed to print all lines and to *assert* `declared==ran && declared>0`; break-verified on synthetic inputs including a shortfall in first position |
| 17 | **The mis-recorded deferral** ← *new* | §4 above, in both directions: two items recorded as OPEN in a file they never lived in (both actually FIXED, in `ae9f2a7` and `29e3bcb`), two ids cited but **never defined at all**, and one ruled *"register it"* whose registration never happened. **A deferral ledger is an artifact, and nothing was checking it** |

**What the three new ones have in common, and why it is worth saying.** All three are failures in the
**verification apparatus**, not in the systems under test — a flaky check, a truncating report, and a
rotten register. Classes 1–14 were mostly found by pointing tools at the repos. **These were found by
pointing them at the tools.** Class 15 in particular was my own fix from earlier in the same work, and
it passed 1 time in 4 on a stack that was definitively broken; had I run it once and moved on, it would
have shipped as a control that certified the exact defect it was written to catch.

---

## 7. What is NOT recorded here

- **Anything already recorded in a repo's own docs** — M1's rollback posture, M2's and M3's R2-5
  rulings, the per-repo future-work registers. This file points at them; it does not restate them.
- **The task list.** Reconciled separately at the close of PASS 3.
- **A claim of completeness about the seventeen classes.** They are the ones **found**, each with a
  citation. An eighteenth is likely, and the useful thing this list provides is not a count but a set of
  questions to ask of the next green signal: *could this have been red? did anything verify the
  verifier? is the evidence complete or truncated? is the subject still there?*
