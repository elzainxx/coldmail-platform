# coldmail-platform

Cross-module documentation for the **coldmail** platform. This repo holds nothing that runs —
it holds the decisions that span the three modules, plus the evidence behind them.

The three modules live as sibling directories under `coldmail/`, each its own independent git
repository with its own history, its own `CLAUDE.md` / `PROJECT_RULES.md`, and its own CI gates:

| directory | module | what it does |
|---|---|---|
| `coldmail-verify` | M1 | email verification — Go control plane + Ruby truemail worker |
| `coldmail-send-managed` | M2 | sending on operator-managed SMTP infrastructure |
| `coldmail-send-byo` | M3 | sending on a user's own (BYO) SMTP credentials; forked from M2 |

> Those are the **target** directory names. Until the rename pass lands they are still
> `Codmailling`, `Codmailling 2`, and `Codmailling 3`. This table is the mapping, not a claim
> that the rename has happened — check the directory listing, not this file.

## What is here

| file | what it is |
|---|---|
| `module-comparison-report.md` | The full three-way audit: where the modules agree, where they diverge, and which divergences are defects rather than deliberate differences. The long one — read it when you need the evidence for a specific claim. |
| `canonical-module-contract.md` | The shape a coldmail module is supposed to have: naming, routes, roles, schema layout, gate set. What the alignment work is aligning *to*. |
| `alignment-fix-plan.md` | The tiered fix list derived from the report, with cost estimates. The execution plan works through this. |
| `pass-1-findings.md` | What the first execution pass found, including the `FW-M2-*` headings that turned out to exist only here — see `deferrals-and-false-greens.md` §4. |
| `deferrals-and-false-greens.md` | **The permanent record.** Every deferral with what blocks it, every deferral that was itself mis-recorded with the corrected premise, and the **eighteen false-green classes** with a citation each. Read this before trusting any green in this body of work. |
| `verification-runs.md` | What was actually executed at the close of Chunk 4 — which commit, which container, what it produced, twice per repo. The evidence behind the word "verified". |
| `scratch/` | The raw per-module interface inventories the audit was built from (`_inv_*`). Kept because a conclusion whose inputs were thrown away cannot be re-checked — not because anyone should read them straight through. Also `_deferrals.md`, the working notes that fed `deferrals-and-false-greens.md`. |

## Why this is a separate repo

The three modules are deliberately independent: separate histories, separate rule files, no shared
code path, no build-time coupling. A finding that spans them has nowhere honest to live inside any
one of them — filing it in M2 would make M1 and M3 look like M2's dependents, which they are not.
This repo is that neutral ground.

It also stays deliberately small. Anything that governs how ONE module is built belongs in that
module's own repo and rule file; this repo does not get to override them. Where a cross-module
decision needs to change a module's behaviour, the change lands in that module, under that
module's rules and gates.

## Reading order

1. `canonical-module-contract.md` — what the target shape is
2. `alignment-fix-plan.md` — what is being changed, in what order
3. `module-comparison-report.md` — the evidence, when you need to check a specific claim
4. `deferrals-and-false-greens.md` — what was deliberately **not** done and why, and the eighteen ways
   a green signal in this work turned out not to be evidence. If you are about to trust a passing gate
   or reopen a deferred item, start here instead of at 1.
5. `verification-runs.md` — the executions behind the word "verified"

The parent `coldmail/` directory is **not** a git repository and must not become one: it sits
inside a folder holding unrelated projects, so initialising it would sweep them in.
