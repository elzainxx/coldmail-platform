# Audit scratch that the records cite (kept, not deleted)

These are working files from the 2026-11-07 → 2026-11-09 audit and fix passes that a record in this repository **cites** — by run ID, by a commit SHA that
`verification-runs.md` / an ADR / a ledger names, or by file name. They were moved here on 2026-11-09 by the cleanup pass; everything those passes left behind that
nothing cites was deleted. They are evidence, not documentation: read them as "what the tool printed at the time", not as current guidance.

| directory | came from | kept | deleted (uncited) |
|---|---|---|---|
| `cm-audit/` | `%TEMP%\cm-audit` — the CI-replica logs (`replica/`), GitHub run logs (`gh/`), per-chain results (`logs/`), commit-message drafts (`msg-*.txt`), sweep results (`sweep/`) | 48 files, 1.66 MB | 256 files, 1.54 MB |
| `claude-job-776392d8/` | `~/.claude/jobs/776392d8` — an August Claude Code job ("PASS 0 — baseline all three repos green + restructure") that stopped `blocked` on an SSL-proxy error: its timeline, commit-message drafts, the rename diff, and the pristine copies of files the ADRs name | 33 files, 1.73 MB | 297 files, 4.34 MB |
| `../scratch/` | the audit's raw per-module interface inventories (`_inv_*`), tracked since the initial commit | 7 files | — |

Each directory has a `MANIFEST.tsv` (bytes, relative path, **why it is cited**). The citing rule, applied mechanically: the file's name appears in a tracked doc/script
(and is distinctive — `go.mod`, `main.go`, `README*` and names under 14 characters do not count); or its name carries a GitHub run ID that a tracked `.md` cites; or
it contains a 7-character SHA that a tracked `.md` cites **and** that is a real commit in one of the repositories (so `go.sum` pseudo-versions and image digests do not
count). The rule errs towards keeping: some `pristine*` source copies are kept only because a doc names the *real* file of the same name.

Nothing in this directory is read by any gate. The platform runner (`tools/platform-check.sh` in the parent checkout) re-checks the hashes in `verification-runs.md`
against the repositories, not these files.
