# Running deferral ledger (working notes for PASS 3)

Scratch. PASS 3's deliverable is a proper record in this repo; this file exists so the list
survives a context loss mid-Pass-2. Append as items are deferred — do not prune.

## Deferred by ruling (Pass 2 scope decisions)

- **Tier 4 in full** — M1 adopting quote/run/report/describe. Ruled out of scope by the brief.
- **M1 schema introduction (audit D7)** — M1 keeps its unprefixed tables; introducing a
  `verify` schema is a migration-shaped change, deferred from Pass 2's rename.
- **M1 `cp_runtime` login + the third role** — the two-role model stays for now.
- **M1's 12 missing `.down.sql` files** — every M1 migration is up-only.
- **`TM_*` → `VERIFY_*` env prefix** — ruled: keep `TM_*`, honour ADR-0005. If it is ever
  renamed it needs a superseding ADR, not a silent change.
- **A request-path leak scan for M1** — cannot run until item 15's compose stack exists.

## Deferred future-work items already ledgered in each repo

- **M3** — FW-M3-13, FW-M3-14, FW-M3-15, FW-M3-16 (`Docs/future-work.md`).
- **M2** — FW-M2-A, FW-M2-B, FW-M2-C, FW-M2-D (`Docs/future-work.md`).
- **M1** — `Docs/pending-ledger.md`.

Pass 3 must EMPTY these three ledgers (the brief), which means each entry is either done or
consciously recorded here.

## Deferred by ruling — added 2026-08-07 (the 15b re-estimate)

Each of these is blocked on a DECISION, not on work. They were pulled out of C3 because starting any
of them before the ruling means building the wrong thing at LOW cost or the right thing at HIGH.

- **15-C, the migration/runtime role split.** Blocked twice: it would make
  `TestPGGrants_RoleSwitchActuallyBites` VACUOUS (from a session connected as `cp_runtime`,
  `SET LOCAL ROLE cp_runtime` is a no-op and both directions still pass), and it no longer has
  item 3 to share cost with — the "item 3 needs `cp_runtime` connectable" premise was FALSE.
- **`request_id` in every log line — scope is a RULING.** `canonical-module-contract.md:297` says
  "Every log line", `:74` says "at least one". M1 satisfies the narrow clause and fails the broad
  one at 47 Go sites plus 2 Ruby logger references. LOW vs HIGH turns entirely on which governs.
- **Dedicated schema (D7)** and **migration tracking (D6)** — both interact with M1 having NO
  down-migration mechanism at all. A tracking table recording what ran, in a system that cannot
  un-run anything, records a fact nobody can act on.
- **M3's bare DB timeouts (D10)** — the plan names 1 site; there are **42**. An ~8× miss, so this
  is not the LOW chore it is listed as.
- **Prometheus `/metrics` (all three)** — no module has one; genuinely MEDIUM, correctly sized.
- **GAP-37 adjudication** — re-estimated MEDIUM–HIGH. `SKIP LOCKED` never lock-waits, so M3's
  `pg_stat_activity`-blocked-count harness is structurally inapplicable and a different instrument
  is required. Two claimed defects here (hardcoded `WorkerID`, per-batch `locked_at`) were
  investigated and REJECTED — unreachable in the single-drainer shipped topology.
- **`DisallowUnknownFields()` (15b row 1) — SEQUENCING, not a deferral.** It would destroy
  `pg_tenancy_test.go:287-293`'s forged-owner proof, which requires 202 on a body carrying forged
  `user_id`/`owner_user_id`/`owner`. Re-express that case to assert the STORED owner rather than
  the status code FIRST, then tighten the decoder. In the other order the obvious fix is to stop
  sending the forged fields, which silently deletes the proof.

## New defects found during C3 verification, filed not fixed (2026-08-07)

- **N1 — the senders leak the DSN on pool-open, the same defect as M1's D12.** M2+M3
  `services/control/internal/db/db.go:21-23` + `cmd/control/main.go:50`, pgx v5.7.4. MEASURED
  LEAK=true. **The fixture trigger differs at 5.7.4: `sslmode=bogusmode`, NOT
  `pool_max_conns=notanumber`** — porting M1's test verbatim would be VACUOUS.
- **N2 — `check-docs-updated` inherits its green from HEAD~1.** Measured: with every doc reverted
  so the tree touched NO doc, it still printed `ok (code_changed=true docs_changed=true)`, rc=0.
- **N3 — the Windows host silently does not execute freshly-built Go test binaries.** 3 of 10
  selftest branches went red on the host with the just-appended fixture package absent from the
  gate's output entirely; all 10 pass in Docker. **C4 must run entirely through Docker.**

## Found during Pass 2, not fixed in it

- **The item-8 / item-18 ordering note for M1** — item 18 touches the same surface item 8's
  M1 share does; whichever lands second must not re-derive the first's declarations.
- **M1's item-7b share is SEQUENCED WITH ITEM 18, not deferred.** 7b's requirement is that a
  mux-level 405/404 leaves "through the error envelope". M1 has no envelope: all 26 of its error
  sites are hand-rolled `{"error":"<free text>"}` (verified 2026-08-05 — the plan's "~25" is
  accurate; all are in `control-plane/internal/api/api.go`), and one is a SECOND shape
  (`{error, max, got}`). Item 18 is what creates the envelope. Doing 7b first would mean inventing
  a coded envelope used by the mux fallback alone — a THIRD shape on the same surface, which is the
  exact defect item 18 exists to remove. So M1's 7b lands in C3 with 18, on 18's `writeError`.
  Not a deferral: it is in Pass 2, one group later than the brief's C2 grouping places it.
  M2 (`e3fee99` + `de01752`) and M3 (`5924fbc`) are done, so 7b is 2/3 complete.
  Note for whoever writes 18: M1's request-id minter is `orchestrator.mustID` (bare UUIDv7). There
  is no minter in `internal/api` — `newRequestID` was DELETED as audit D23 because it returned
  `req-<hex>`, which the LOG's `uuid` column rejects. `helpers.go` carries that note and names
  item 18 explicitly. A second minter must not be reintroduced.
- **M1 has the SAME Pass-1 gofmt regression, 30 files, and NO CI STEP THAT WOULD CATCH IT.**
  Found 2026-08-05 while doing item 10: `gofmt -l` over `control-plane/internal cmd` lists 30 files
  at `29e6204`, most untouched by any of my work (e.g. `main.go:331`'s alignment in
  `configToBudget`). M2/M3 had this discharged in `a026e96`/`0c22c8e` because THEIR CI runs gofmt as
  step 1; M1's `.github/workflows/ci.yml` runs only `go build`, `go vet`, and `go test`, so it is
  not a red gate here — which is also why it went unnoticed. Needs its own M1 commit (sweep +
  arguably a gofmt CI step, since a sweep with nothing enforcing it will just re-rot).
- **Pass 1's verification did not cover `real-infra.yml`'s gofmt step** in either Go repo.
  The gofmt regression it would have caught was found later and discharged separately
  (`a026e96` in M3, `0c22c8e` in M2), but the CI path itself was never exercised in Pass 1.
- **`rebuild_m2_templates.sh` prints a misleading "32 enum types"** — that counts ALL types in
  the database. The real `send_managed` enum count is 7. Cosmetic, but it is the kind of number
  someone later treats as an assertion.
- ~~**M2's unguarded describe summary line**~~ · ~~**M2's `run.sh` never runs `internal/httpapi`**~~ ·
  ~~**M2's `contract_drift.sh` does not read its own api-contract prose count**~~ — all three
  **DISCHARGED 2026-08-05 in M2 `de01752`**, not deferred. Judged incomplete delivery of my own
  `e3fee99` rather than a new item: two of the three were defects in that commit's own work (nine
  tests nothing ran; a count word nothing checked). Fail-verified five ways, including a control
  that removes the guard and shows the misleading line return. M2's harness moved 58 → 59.
  Kept here because the REASONING is worth carrying: `fail` is a 0/1 LATCH, so any
  `baseline="$fail"` … `[[ "$fail" -eq "$baseline" ]]` guard anywhere in either module's gate layer
  is jammed once an earlier check has fired. If a third gate block is ever given a summary line,
  it needs a block-local flag, not a baseline.
- **Cross-module count sentences are not gate-checkable.** M2's `api-contract.md` said "Module 3
  emits seven" — stale the moment M3's 7b added an eighth. Corrected and dated in `de01752`, but
  no gate can enforce it: the other repo's `codeStatus` is not readable from inside this one.
  Every such cross-reference is a manual-sync liability. Pass 3 should sweep for others.

## Environment facts worth carrying forward (not deferrals, but load-bearing)

- **Concurrent harness runs corrupt each other's results.** On 2026-08-04 an aborted run left a
  `run.sh` orphaned; a second run then produced 6 FAILs in the live suites (the tell was
  `Mailpit count=3` where the test expects 1). Neither was a defect. `ps -ef` shows the MSYS
  pid but `taskkill` needs the WINPID from `ps -W` column 4 — killing by the MSYS pid reports
  "process not found" while leaving it alive.
