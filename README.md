# coldmail-platform

The **coldmail platform**: the product shell that users and the admin touch, plus the cross-module record that the three modules were built against.
It is the first real caller of the module contract. Operating rules: [`CLAUDE.md`](CLAUDE.md). Roadmap and locked decisions: [`Docs/coldmail-platform-build-plan.md`](Docs/coldmail-platform-build-plan.md).
Phase 0 (groundwork) is specified in [`Docs/phase-0-spec.md`](Docs/phase-0-spec.md).

**Code** (Phase 0 ships skeletons only — no screens, no business tables): a Go service at the repository root (`cmd/`, `internal/`), a React + TypeScript + Tailwind app in `web/`,
migrations in `db/migrations/`, and a real-infrastructure harness in `tests/harness/` that builds the three modules from the commits pinned in `tools/modules.lock` and runs them as
real services beside the platform and one real Postgres.

**CI** (`.github/workflows/`, all on a GitHub runner — the only admissible evidence): `go`, `web`, `gates`, `harness`, `gitleaks` (full-history secret scan) and `platform-check`
(`tools/platform-check.sh`: every JSON file parses, every internal link resolves, every cross-repo citation resolves, every cited commit SHA exists). The last needs the three private
modules, which `tools/fetch-modules.sh` clones over SSH with one **read-only deploy key per module repository** (secrets `DEPLOY_KEY_COLDMAIL_VERIFY`,
`DEPLOY_KEY_COLDMAIL_SEND_MANAGED`, `DEPLOY_KEY_COLDMAIL_SEND_BYO` on this repository); a missing, wrong or revoked key turns the job red — it never skips. Hex strings that look like
commits but are not (agent ids, a commit of the private parent) are listed with reasons in `tools/non-commit-hex.txt`.

The three modules live as sibling directories under `coldmail/`, each its own independent git
repository with its own history, its own `CLAUDE.md` / `PROJECT_RULES.md`, and its own CI gates:

| directory | module | what it does |
|---|---|---|
| `coldmail-verify` | M1 | email verification — Go control plane + Ruby truemail worker |
| `coldmail-send-managed` | M2 | sending on operator-managed SMTP infrastructure |
| `coldmail-send-byo` | M3 | sending on a user's own (BYO) SMTP credentials; forked from M2 |

> The rename pass **has landed**: these are the real directory (and repository) names, and the three module
> repositories sit beside this one as siblings. The module table above is kept for orientation; the directory
> listing is still the authority.

## What is here

| file | what it is |
|---|---|
| `module-comparison-report.md` | The full three-way audit: where the modules agree, where they diverge, and which divergences are defects rather than deliberate differences. The long one — read it when you need the evidence for a specific claim. |
| `canonical-module-contract.md` | The shape a coldmail module is supposed to have: naming, routes, roles, schema layout, gate set. What the alignment work is aligning *to*. |
| `alignment-fix-plan.md` | The tiered fix list derived from the report, with cost estimates. The execution plan works through this. |
| `pass-1-findings.md` | What the first execution pass found, including the `FW-M2-*` headings that turned out to exist only here — see `deferrals-and-false-greens.md` §4. |
| `deferrals-and-false-greens.md` | **The permanent record.** Every deferral with what blocks it, every deferral that was itself mis-recorded with the corrected premise, and the **twenty false-green classes** with a citation each (the twentieth, *the environment-masked green*, is the one to read first: it is why a run on the developer's machine no longer counts as green). Read this before trusting any green in this body of work. |
| `verification-runs.md` | What was actually executed at the close of Chunk 4 — which commit, which container, what it produced, twice per repo. The evidence behind the word "verified". |
| `Docs/history/scratch/` | The raw per-module interface inventories the audit was built from (`_inv_*`; moved here from `scratch/` on 2026-11-09, with `Docs/history/audit-scratch/` — the logs and drafts the records cite, each with a manifest). Kept because a conclusion whose inputs were thrown away cannot be re-checked — not because anyone should read them straight through. Also `_deferrals.md`, the working notes that fed `deferrals-and-false-greens.md`. |

## Why the cross-module record lives here, and not in a module

The three modules are deliberately independent: separate histories, separate rule files, no shared
code path, no build-time coupling. A finding that spans them has nowhere honest to live inside any
one of them — filing it in M2 would make M1 and M3 look like M2's dependents, which they are not.
This repo is that neutral ground.

Anything that governs how ONE module is built belongs in that
module's own repo and rule file; this repo does not get to override them. Where a cross-module
decision needs to change a module's behaviour, the change lands in that module, under that
module's rules and gates.

## Reading order

1. `canonical-module-contract.md` — what the target shape is
2. `alignment-fix-plan.md` — what is being changed, in what order
3. `module-comparison-report.md` — the evidence, when you need to check a specific claim
4. `deferrals-and-false-greens.md` — what was deliberately **not** done and why, and the twenty ways
   a green signal in this work turned out not to be evidence. If you are about to trust a passing gate
   or reopen a deferred item, start here instead of at 1.
5. `verification-runs.md` — the executions behind the word "verified"

The local working folder `coldmail/` is **not** a git repository and must not become one: it sits inside a folder
holding unrelated projects, so initialising it would sweep them in. The four repositories are published as
`elzainxx/coldmail-verify`, `coldmail-send-managed`, `coldmail-send-byo` and `coldmail-platform`, and a separate parent
repository, `elzainxx/coldmail`, pins them as git submodules — it was created in its **own fresh directory**, not
in place, for exactly that reason. Clone the whole set with
`git clone --recurse-submodules https://github.com/elzainxx/coldmail.git`.
