# coldmail-platform — operating rules

The platform is the product users and the admin touch. The three modules do the work (verify = M1, send-managed = M2, send-byo = M3); the platform owns users, money, jobs, authoring and administration, and is the **first real caller of the module contract**.

- The roadmap and the **locked decisions**: [`Docs/coldmail-platform-build-plan.md`](Docs/coldmail-platform-build-plan.md) (diagram: [`Docs/coldmail-platform-architecture.mermaid`](Docs/coldmail-platform-architecture.mermaid)). Locked decisions are **not reopened here**: if one contradicts the module code, **flag it, do not fix it**, and put it in the phase spec's open decisions.
- The spec of the phase being built: `Docs/phase-N-spec.md`. **No code for a phase before its written spec**, and the build plan's status table is kept current from the first commit of the phase.
- The twenty false-green classes: [`deferrals-and-false-greens.md`](deferrals-and-false-greens.md). **Read it before writing a gate.** (The build plan cites it as `Docs/deferrals-and-false-greens.md`; the file is at the repository root.)

## Inherited rules (from building the three modules — verbatim from the build plan)

- **CI on a real GitHub runner is the source of truth.** Local runs are rehearsal. Docker Desktop masked two failures that a Linux runner caught.
- Every test has a **paired control that fails when the property breaks**. A test that cannot fail is not a test.
- Concurrency tests **prove contention** (a forced interleave, or a backend proven blocked), never hope for it.
- **Gates, not comments.** "Remember to do X" becomes a CI check that fails when X is missing.
- Every new test or gate is **proven to run in CI**, not merely to exist; test counts are asserted per package with a nonzero floor.
- Any check that covers only part of its domain **prints how much it did not check**.
- When a behaviour is put behind a switch, sweep every assertion that assumed it was unconditional.
- **The admin surface is a parallel write path.** Every user-path rule is enforced in the store layer, where all writers pass.
- Config, not code. Every dial is seeded with a consumer at the commit that seeds it.
- Base images pinned by digest; CI builds without cache.

## Rules added in Phase 0

1. **A test that cannot run FAILS; it never skips.** The floor gate counts a skipped test as red. Integration tests live behind the build tag `harness` so that `go test ./...` never needs a database and can never skip into a green.
2. **A gate whose subject is empty is red.** Every gate states a nonzero floor on what it found ("found 0 packages/images/migrations — the instrument matched nothing").
3. **Every reject case of a selftest asserts the REASON it fails for**, not only a non-zero exit (class 10). Every selftest has a hard minimum case count and a control that proves the selftest itself can fail.
4. **A gate that reads a log reads the log the step actually produced**, captured with `tee` under `pipefail`, never a summary the step wrote about itself.
5. **The modules are never mocked and never modified.** The harness builds them from the commits in `tools/modules.lock`, unmodified, and runs them as real services. Where a module ships no recipe (M1 has no control-plane Dockerfile), the platform supplies one beside the harness — it does not patch the module. A module defect is **flagged** in the phase spec's findings, not fixed from here.
6. **Nothing the platform can reach is public.** The harness network is `internal: true` and publishes no port; the exposure check reads two independent instruments.
7. **Secrets are minted per run, written mode 600 under the run's temp directory, masked in the log, and never printed or committed.** gitleaks scans the full history of every branch on every push. A fault-injection branch that plants a token adds one fingerprint to `.gitleaksignore`, listed in the phase spec; nothing else may be ignored.
8. **Line endings are LF everywhere** (`.gitattributes`); every script strips `\r` from what it reads.
9. **The platform uses advisory-lock band `7_xxx_xxx_xxx`** and no other (M3 holds `8_`, M2 holds `9_`).
10. **The platform's database identity is `platform_app`** (LOGIN, not a superuser, no DDL). Migrations run as a separate, privileged identity and are never run by the service.

## Repository layout

| Path | What |
|---|---|
| `cmd/platform`, `internal/…` | the Go service (`internal/config`, `logging`, `httpapi`, `server`, `app`, `dsnscrub`, `migrate`, `probe`) — one Go module at the repository root, `github.com/coldmail-platform/platform` |
| `cmd/platform-migrate` | the migration runner (`up`, `down`, `status`, `set-app-password`, `up-and-set-password`) |
| `cmd/platform-probe` | the Phase 0 end-to-end caller (platform → each module's health, readiness, describe) |
| `db/migrations/platform_NNNN_*.{up,down}.sql` | the `platform` schema, three roles, tracking table. No business tables before Phase 1 |
| `web/` | React + TypeScript + Tailwind (Vite). A placeholder page only until Phase 6 |
| `tests/harness/` | the real-infrastructure harness: `run.sh`, `docker-compose.yml`, the module-1 Dockerfile, the integration tests (`*_test.go`, tag `harness`), the exposure check |
| `tests/floors-go.txt`, `floors-web.txt`, `floors-harness.txt` | the per-package test-count floors |
| `scripts/` | the gates and their selftests: `check-test-floors`, `check-image-pins`, `check-migrations` |
| `tools/` | `fetch-modules.sh` (clone + `--lock`), `modules.lock`, `check-modules-lock.sh`, `platform-check.sh` (docs citations) |
| `Docs/` | the build plan, the diagram, the phase specs |
| repository root `*.md` | the cross-module record: contract, comparison report, deferrals and false greens, verification runs |

## How CI runs (all on `ubuntu-latest`; a Windows run is rehearsal)

| Workflow | What it proves |
|---|---|
| `go` | gofmt clean, build, vet (plain and `-tags harness`), unit tests, the per-package floor |
| `web` | `npm ci` from the lockfile, type-check, build, the built CSS contains the Tailwind utilities, tests, the test-count floor |
| `gates` | every gate's selftest, then `check-migrations` and `check-image-pins` on the real repository |
| `harness` | the three modules built from their pinned commits and running as real services beside the platform and one real Postgres; the integration tests; the end-to-end probe; the exposure check; the outage check |
| `gitleaks` | the full-history secret scan, with the scanner proven before it is trusted on every run |
| `platform-check` | every JSON file parses, every internal link resolves, every cross-repo citation resolves, every cited commit exists |

To run the harness by hand you need Docker, Go, Node, openssl and the three module checkouts at the pinned commits:
`bash tools/fetch-modules.sh --lock tools/modules.lock <dir>` (needs the three `DEPLOY_KEY_COLDMAIL_*` environment variables — read-only deploy keys) then `H_MODULES_ROOT=<dir> bash tests/harness/run.sh`.
Docker is currently down on the owner's machine: **only the runner counts.**

## Re-pinning the modules

1. Edit `tools/modules.lock` (three 40-hex commits), push, and let `harness` go green on the runner.
2. In the parent repository (`elzainxx/coldmail`) move the three submodule pointers to the same commits.
3. `bash tools/check-modules-lock.sh <parent-dir>` must say OK. (No CI does this: the parent is private and cannot read the module repositories.)
