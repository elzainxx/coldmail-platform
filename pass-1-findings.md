# Pass 1 findings — carried into Pass 2

Pass 1 was RENAME ONLY: "Nothing but identity moves in this pass. If you find a bug,
note it for Pass 2; do not fix it here." This file is that note. Nothing below was
fixed in Pass 1.

Pass 1 commits: coldmail-verify `c8f74a0` · coldmail-send-managed `29e3bcb` ·
coldmail-send-byo `468d902`. All three trees clean, all histories preserved
(each rename commit is a direct child of its pre-rename baseline).

---

## 1. M1's Pass-1 rename was far smaller than the brief's generic list — by ruling, not by shortcut

The brief lists a generic rename surface for all three repos (brain→control, box/node→worker,
schemas, roles, migration prefixes, routes, env prefixes, compose/image names). For
coldmail-verify, most of that surface was either already done or ruled into Pass 2. Verified
against the tree rather than assumed:

| Brief item | M1 status | Why |
|---|---|---|
| brain → control | ALREADY DONE | ADR-0005 (ACCEPTED 2026-07-17) named the deployables `control-plane` / `verifier-node`, with the brief's own reasoning ("a directory named `brain/` reads as jargon"). |
| box/node → worker | MUST NOT MOVE | `node` is DB CONTRACT: tables `nodes`/`node_audits`/`node_warmup`, column + Door-1 wire field `json:"node_id"`, index `idx_node_audits_node_time`. M1's five one-way doors: "a rename or removal is a break, never allowed." |
| routes | ALREADY CONFORM | `api.go:116-123` registers `/v1/verify`, `/v1/verify/bulk`, `/v1/jobs/{id}`, `/v1/jobs/{id}/results`, `/healthz`, `/readyz`. M1 keeps `/verify` + bulk as domain verbs (PLAN A+); `/v1` root is protected. |
| env prefix | RULED KEPT | `TM_*` (84 vars). ADR-0005 lists them as deliberately unchanged. Optionally revisited in Pass 2 **with a superseding ADR**. |
| schema | PASS 2 | M1's 25 tables are unqualified in `public`. Schema introduction is fix-plan item D7. |
| roles | PASS 2 | `cp_runtime` / `cp_admin`, both NOLOGIN. The `<schema>_owner/_app/_admin` triple presupposes D7's schema, and making `cp_runtime` connectable is fix-plan item 3 — the plan itself says the two "share work". |

Net Pass-1 change: the Go module path (`github.com/mnfi/codmailling/control-plane` →
`github.com/coldmail-verify/control-plane`) and the repo's own name in prose. 53 files,
+164/−164. The whole diff normalises to 24 import-path shapes, the `go.mod` module line and
the README tree-diagram root; masking the module prefix makes the `+` and `−` side sets hash
identically, so nothing but the prefix moved. No `.sql`, script, Dockerfile, workflow or Ruby
file changed.

---

## 2. Two green runs were materially misleading. Both are now fixed or recorded.

Both are the same failure class the audit flagged elsewhere: **a green run that proved nothing.**

### FW-M2-A — M2's harness never built its `migrate` binary (repo defect, PRE-DATES the rename)
`git show d34e57a:` confirms this predates Pass 1, so the **Pass 0 M2 baseline green was itself
partly vacuous**. Recorded for Pass 2; not a rename regression.

### My own `go` wrapper silently skipped M1's 30 real-Postgres tests (tooling defect, FIXED)
Every `TestPG*` in `control-plane/internal/store/` opens with
`if os.Getenv("TM_TEST_DATABASE_URL") == "" { t.Skip(...) }`. My PATH-injected `go` wrapper
runs the toolchain in a container and did not forward that variable, so 30 tests SKIPped while
`go test ./...` still printed `ok ... 0.011s`. Fixed by forwarding the whole `TM_*` surface.
Effect: `internal/store` **0.011s → ~3.0s**, SKIPs **30 → 7**, PASS **345 → 368**.

This mattered for the Pass-1 gate. The pre-rename baseline number (368/7) had been recorded
*after* the wrapper fix but its log had not been kept, so rather than reason by arithmetic I
re-ran the **pre-rename tree** (detached worktree at `fe8fde6`) through the **same fixed
wrapper**: 368 PASS / 7 SKIP / 0 FAIL, and the 375 verdicts are **identical name-for-name** to
the post-rename run. The only variable between the two runs was the rename.

---

## 3. FW-M2-D — M2's SUITE 5 fails on a re-run, and it is not a rename defect

`tests/harness/phase3_send.sh:148` hard-kills the worker (`$DC kill worker`) so no worker can
report on the stranded T3 job. `scripts/dev.sh test` (`:110` → `tests/harness/run.sh`) never
runs `up -d --wait`, so the *next* run's SUITE 5 execs into a dead container. A failed
`docker compose exec` returns rc≠0, which makes SUITE 5's **negative** KMS check pass
spuriously ("path absent") and leaves only its anti-vacuity control failing — exactly the
observed PASS/**FAIL**/PASS.

Proven, not inferred: a liveness monitor caught `exit=137`, and a deterministic control
(`suite5_control.sh`) reproduced the exact signature on an **unmodified tree** — live worker →
PASS/PASS/PASS, killed worker → PASS/**FAIL**/PASS. Re-run after `up -d`: **58 passed, 0 failed**.

---

## 4. Smaller Pass-2 items found during Pass 1

- **M1 CI migration glob will pick up `.down.sql`.** `.github/workflows/ci.yml:49` applies
  `migrations/[0-9][0-9][0-9][0-9]_*.sql`. Today M1 has no down-migrations, so the glob is
  correct. Pass 2 adds 12 `.down.sql` files — at which point this glob applies them in lexical
  order alongside the up-migrations and the gate breaks. Fix the glob **in the same change**
  that adds the down-migrations.
- **M1's verifier-node compose has no `name:` key**, so its compose project name is derived
  from the directory (`verifier-node`). M2 and M3 both declare `name:` explicitly and produce
  the brief's `<repo>-control-1` / `<repo>-worker-1` container shape. Not a collision, but not
  the specified naming either. Adding the key is a behaviour change, hence Pass 2.
- **M1 has no `Docs/future-work.md`.** Its deferral ledger is `Docs/pending-ledger.md` (renamed `Docs/future-work.md` on 2026-11-13, so M1 now has the same file name as M2 and M3)
  (M2 and M3 both use `Docs/future-work.md`). Pass 3 must empty the right file per repo.
- Also open from earlier passes: **FW-M3-13/14** and **FW-M2-B/C**.

---

## 5. Host ports and compose projects — the brief's trap (b), verified

Host ports are not namespaced by compose project, so all three stacks were checked together
with `${VAR:-default}` forms resolved. **Twelve distinct host ports, zero collisions:**

| Repo | Host ports |
|---|---|
| coldmail-verify | 3000 |
| coldmail-send-managed | 5432, 8025, 8080, 8443, 8444 |
| coldmail-send-byo | 6432, 9025, 9053, 9080, 9443, 9444 |

M1's throwaway test Postgres runs on **5434** (ad-hoc tooling, not compose), clear of both
5432 and 6432. M2 and M3 stacks were live simultaneously during verification.

---

## 6. Norton 360 is an environmental blocker on this host, not a repo defect

Two distinct behaviours, both worked around **externally with no repo edits**:

1. **Quarantines every freshly-linked Windows Go binary.** Reproduced on a 3-line hello-world
   and on pre-rename binaries, so it is not caused by any change in these repos. `go build`
   output is unusable and `go test` hangs unkillably (even `timeout 25` could not reap it).
   Worked around with a PATH-injected `go` shim that runs `golang:1.23-alpine` with the repo
   bind-mounted at its identical Git Bash absolute path, so harness-exported absolute paths
   resolve unchanged inside the container.
2. **Transiently locks newly-written `.git/objects/**` files**, breaking `git add` and
   sometimes `git commit` with `Permission denied`. Worked around by pre-hashing changed files
   (`git hash-object -w`, one retry each) before `git add`. The M1 commit hit this on one object
   during `commit`; git's own retry succeeded and `git fsck --no-dangling` came back clean with
   the flagged object present, HEAD readable end-to-end and the tree clean.

**Recommended host change (not a code change): add Norton exclusions for the Go build/temp
directories and the three repos' `.git` directories.** That removes the need for both
workarounds entirely.
