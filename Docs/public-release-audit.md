# Public-release audit (five repositories, private → public)

**Owner ruling (2026-11-17):** GitHub Actions minutes on the private repositories were exhausted, so the five repositories become public (public repositories get free standard GitHub-hosted runners). Making a repository public cannot really be undone (anyone can clone or fork it, and making it private again does not recall those copies), so each repository was audited **first**, flipped **second**, and the Phase 2 close-out resumed **third**.

Scope: `elzainxx/coldmail-verify` (M1), `elzainxx/coldmail-send-managed` (M2), `elzainxx/coldmail-send-byo` (M3), `elzainxx/coldmail-platform`, `elzainxx/coldmail` (the parent). `zexSites/codmailling-module-3` was **not touched and not audited** (it stays private).

Stop rule the audit ran under: if any real credential, key, token, password or personal data were found in any commit of any branch, **nothing is flipped** and the owner rotates first (a history rewrite cannot help once a repository is public). No history was rewritten and nothing was deleted on GitHub.

## Method

- **What was scanned:** a fresh `git clone --mirror` of each repository from GitHub (every ref the server holds: branches, tags and anything else; each repository has only `main` plus, for M1, M2 and M3, a few tags; no `refs/pull/*`, no other branches), 742 commits in total (parent 27, M1 96, M2 229, M3 306, platform 84).
- **Secrets, no allowlist:** gitleaks 8.30.1 (checksum-verified release binary), full history of all refs (`--log-opts="--all"`), run with a config that is *only* the default rule set (`useDefault = true`, no `[[allowlists]]`) and an empty ignore file, so the repositories' own exceptions (`.gitleaks.toml`, `.gitleaksignore`) played no part. Every hit was classified with evidence.
- **Files that must never be public:** the path list of every commit (`git log --all --name-only`) was matched against key, certificate, `.env`, dump, keystore, kubeconfig and SSH patterns.
- **Personal data and infrastructure details:** every *added line of every commit* was scanned for home paths, personal names, phone numbers, e-mail addresses, public IPv4 addresses, Supabase references, AWS account ids and external domains; every distinct match was reviewed by a person.
- **Password-/token-shaped assignments:** every added line was scanned for `password|secret|token|api_key|private_key|dsn|database_url …= <value>` shapes and each distinct value was reviewed.
- **GitHub-side objects that become public with the repository:** issues and pull requests, releases and their assets, webhooks, Actions artifacts, Actions secrets/variables (names only), deploy keys, wiki, forks, descriptions.
- **Workflow logs:** a sample of 38 recent run logs (all five repositories, every workflow type, including the heavy harness and the fault-proof legs) was downloaded and scanned with the same no-allowlist gitleaks, and searched for private-key blocks.
- **Workflow safety and log safety by reading:** every workflow and every script, compose file and Makefile it runs, for `pull_request_target`, secrets reachable from forks, token permissions, self-hosted runners, unpinned actions, `set -x`, environment dumps and artifact uploads (the forward fixes are listed below).

## Result per repository

| Repository | Verdict | One-line reason |
|---|---|---|
| `coldmail` (parent) | **CLEAN** | 0 gitleaks hits; no secret or sensitive file in history; flagged (not blocking): the owner's home path and name in `OWNER-ACTIONS.md` |
| `coldmail-verify` (M1) | **CLEAN** | 4 gitleaks hits, all one vendored upstream README example; flagged: the owner's name, and one public IP address from a 2026-07-17 audit log, in `Docs/history/` |
| `coldmail-send-managed` (M2) | **CLEAN** | 0 gitleaks hits; dev placeholders only |
| `coldmail-send-byo` (M3) | **CLEAN** | 0 gitleaks hits; dev placeholders only |
| `coldmail-platform` | **CLEAN** | 299 hits, all classified: 297 request-id strings in preserved CI logs, 2 the test-only SMTP-sink password literal |

No repository is BLOCKED. The flags below are for the owner to acknowledge; none is a credential and none can be fixed without rewriting history.

## Gitleaks hits and their classification (no allowlist)

| Repo | Hits | Rule | Where | Classification and evidence |
|---|---|---|---|---|
| parent | 0 | | | |
| M1 | 4 (one commit, `d582ab41`) | `generic-api-key` ×1, `curl-auth-header` ×3 | `box/truemail-rack/README.md` (since renamed `verifier-node/truemail-rack/README.md`) | **Placeholder (upstream documentation).** The token `a262d915-…` is the example access token in the upstream `truemail-rack` README (the same UUID occurs 5 times in `github.com/truemail-rb/truemail-rack/blob/master/README.md`, checked through the GitHub API); the e-mail addresses in the same lines are upstream's examples too. It never matched any system of ours. |
| M2 | 0 | | | |
| M3 | 0 | | | |
| platform | 297 | `generic-api-key` | `Docs/history/audit-scratch/cm-audit/**/*.log` (six preserved CI logs of module-3 runs, one commit `9436e7f7`) | **Not secrets: request correlation ids.** All 297 matched values were extracted and every one is `req_<32 hex>` (the `request_id=`/`rid=` field of the modules' own log lines). |
| platform | 2 (commits `355299a6`, `7b05c78`) | `generic-api-key` | `tests/harness/m3_live_test.go:201` and a spec paragraph quoting it | **Placeholder, test-only.** The literal `m3-live-harness` is the SMTP password of the *test* account the live module-3 test creates against the harness's own SMTP sink, which accepts any credential (the harness prints "dev smtp-sink accepts any AUTH — verified as expected"); the sink exists only inside the harness's internal Docker network. It never matched a real system. The literal is no longer in the tree: the harness mints the sink password at run time, and gitleaks now also scans the head's files with no history. |

The earlier Phase 2 commit-scoped exceptions in the platform's `.gitleaks.toml` are exactly these two commits and the audit-scratch path; they remain, policed by `check-gitleaks-config`.

## Files that must never be public

Across every commit of every repository, **no** `.env`, `secrets/` content, private key, certificate (the Norton CA certificate included: only the PowerShell script that *exports* it, `scripts/export-norton-ca.ps1` in M1, is committed, and the exported `norton-ssl-proxy-ca.crt` never was), keystore, database dump, `.p12`, kubeconfig or SSH config was ever committed (`git log --all -- <path>` for each pattern is empty; the only matches are `.env.example`, `secrets/.gitkeep` and Go source named `secrets*.go`). The `.env.example` files hold obviously fake development values (`devsuperpass`, `devmodulepass`, `dev-platform-service-token`, `replace-with-…`).

## Personal data (flag, do not fix: history cannot be rewritten)

- **Commit author/committer e-mail addresses (not printed here):** 4 distinct addresses over all five repositories (parent 1, M1 3, M2 3, M3 3, platform 2). **A non-noreply address exists**: one personal Gmail address (the owner's, used as the author of the owner's commits) plus three placeholder `*.local` addresses used by tooling. The author name on those commits is the owner's name. These are public the moment a repository is public and cannot be removed without a history rewrite. If the owner wants to avoid exposing the address in *future* commits: GitHub → Settings → Emails → "Keep my email addresses private" and "Block command line pushes that expose my email", then use the `noreply` address in `git config user.email`.
- **Home paths / names in files (type and place only):** parent `OWNER-ACTIONS.md` (the Windows user name in a home path, the `D:\` directory layout of the workstation, the owner's name); M1 `Docs/history/phase-5-readiness-assessment.md` (the owner's name, 2 lines); M3 and platform: the `D:\` workstation path in a few research/inventory files under `Docs/` and `Docs/history/`. **Update 2026-11-17:** these paths are replaced by generic forms in the current files of the parent, the platform and modules 2 and 3 by forward commits (git history keeps the originals, as stated above), and the gate `scripts/check-personal-details.sh` (allowlists in `tests/personal-details/`) now fails on any new Windows home path, drive-letter path or IPv4 address that is not an allowed class or a justified fixture. No phone number, no customer data and no other person's data was found.
- **A public IP address of the owner's host:** M1 `Docs/history/phase-0-signoff-sheet.md:33` and `Docs/history/truemail-ground-truth.md:84` contain the output of a truemail audit run on 2026-07-17 with the `current_host_ip` field: the (dynamic, residential) address the owner's machine had that day. It reveals an approximate location and ISP, not a credential. It stays in history.
- **No third party's personal data:** every e-mail address in files is a test fixture on a reserved or fictional domain (`acmecorp.com`, `ourdomain.com`, `example.*`, `*.test`, `x.com`…) or a mail provider's public example; no real customer or user exists yet.

## Infrastructure details

No real hostname, internal URL, Supabase project reference, cloud account id, API endpoint or private IP of ours appears anywhere. The Supabase mentions are the placeholders `db.<project>.supabase.co` / `db.[PROJECT].supabase.co`; the AWS account id is `000000000000`; every public-looking IPv4 address is a documentation range, a mail provider's public MX/SPF range, a version string or a test fixture (the one exception is the owner-host address above); the external domains are public documentation and mail-provider hosts and test fixtures. No ZeroBounce or other vendor key exists in any repository (the stand-in uses fixed fake keys).

## GitHub-side objects

For all five repositories: 0 issues or pull requests, 0 releases (so no release assets), 0 webhooks, 0 Actions artifacts, 0 Actions variables, wiki disabled, 0 forks. Actions secrets: only the three read-only deploy keys on the platform repository (`DEPLOY_KEY_COLDMAIL_VERIFY`, `…_SEND_MANAGED`, `…_SEND_BYO`); the matching public deploy keys ("coldmail-platform CI (read-only)") on the three module repositories. Repository descriptions are public-safe. **Past workflow runs and their logs become publicly visible with the repository** (586 platform runs, 34 + 48 + 33 module runs, 15 parent runs): the 38-log sample above found no secret (64 `generic-api-key` hits in M2/M3 logs, all `req_<32 hex>` request ids; deploy keys appear only as `***`); the logs of runs that were never sampled are the residual risk, bounded by the fact that every secret in these pipelines is generated at run time for a throwaway stack or is a masked deploy key.

## Forward fixes made before the flip (one commit per repository, no history touched)

| Repo | Commit | What |
|---|---|---|
| parent | `f09df58` | checkout pinned by SHA; `concurrency`; `NOTICE`; README line |
| M1 | `4adc316` | both actions pinned by SHA; `paths-ignore` (prose docs; the eight docs a gate reads still run CI); `workflow_dispatch` and `v*` tags; `concurrency`; `NOTICE` extended with the all-rights-reserved line and the third-party paragraph (the vendored `truemail-rack` keeps its MIT licence) |
| M2 | `ff8c302` | three actions pinned; `permissions: contents: read` added to three workflows; a script-injection path in `consume-contention-repeat` closed (the dispatch input no longer expands into the shell); `scripts/dev.sh` masks the four run-time credentials it mints; path filters and `concurrency`; `NOTICE` |
| M3 | `c70f4b6` | three actions pinned by SHA; `permissions: contents: read` added to three workflows; a script-injection path in `consume-contention-repeat` closed; `scripts/dev.sh` masks every credential it mints (two worker tokens, admin key, platform token, their hashes) before it is written or used; the `no_secret_leak` gate now also looks for the admin key, the platform token, their hashes and the KMS dev master key (hex and base64 forms) in the service logs (the dev master key is generated at run time into `secrets/dev/kms/`, mode 600, never printed, never uploaded); path filters and `concurrency`; `NOTICE` |
| platform | `5624892`, `8c5a78f` | all 15 `uses:` pinned by SHA; `contents: read` everywhere (`gates` adds `actions: read` for the new gate); fork-PR guard on the four jobs that read `secrets.DEPLOY_KEY_*`; two unmasked run-time values now masked (`tests/harness/run-db-tests.sh` application password; the throwaway key in `platform-check`); `tools/fetch-modules.sh` clones anonymously over https when a module's deploy key is empty (selftest 8 → 14 cases); the trigger redesign below; the new gate `check-verified-heads`; `NOTICE` |

None of the five repositories uses `pull_request_target`, a self-hosted runner, `workflow_run` or `upload-artifact`.

## Repository settings applied

Before the flip, for every repository: default `GITHUB_TOKEN` permissions `read`, "Actions can approve pull requests" off (it already was). The fork-pull-request approval policy (`all_external_contributors`) can only be set on a public repository (the API refuses it on a private one), so it is set **immediately after** each flip (see the flip log).

## Licence and notice

No licence file was added. Each repository carries `NOTICE`: *"Source visible for transparency. All rights reserved. No license is granted to use, copy, modify or distribute."* (M1 and M2 already had a `NOTICE` listing third-party notices and a `LICENSE` that says "PROPRIETARY AND CONFIDENTIAL"; the new statement was put above the existing content and `LICENSE` was left as it was. **Owner decision:** the word "confidential" in those `LICENSE` files, and M1's `Docs/open-decisions.md` §6.1 ("mechanics and data moat … deliberately non-public"), sit oddly beside a public repository.)

**Update 2026-11-17 (Phase 3, Part A, after the audit): the owner ruled the wording and it is done.** Every repository now carries the same one-paragraph `LICENSE` ("Copyright (c) 2026 Zain. All rights reserved. The source is visible for transparency only. No license is granted to use, copy, modify, distribute or run this software, in whole or in part."), the same words open every `NOTICE` (the third-party notices below them are unchanged), every `README` carries the line "Source visible for transparency only; all rights reserved (see LICENSE and NOTICE).", and the word "confidential" is gone from every `LICENSE` and `NOTICE` (technical uses, such as the confidentiality of encrypted data, stay). The gate `scripts/check-license.sh` (text in `tests/license-canonical.txt`) keeps the five copies identical: platform `gates`, platform `platform-check` (the three module clones) and the parent's `modules-lock` (all five). The paragraph above is the record of what the audit found.

## Flip log

Each repository: default token permissions set (read-only), flipped with `gh repo edit … --visibility public --accept-visibility-change-consequences`, confirmed with `gh repo view` (`PUBLIC`), then the fork-pull-request approval policy set to `all_external_contributors` (read back), then `gitleaks.yml` dispatched to prove the free hosted runners work.

| Order | Repository | Flipped (UTC, 2026-11-17) | Fork-PR approval | First run on a GitHub-hosted runner |
|---|---|---|---|---|
| 1 | `coldmail-verify` | 17:39:36 | all external contributors | [gitleaks 44139447501](https://github.com/elzainxx/coldmail-verify/actions/runs/44139447501) success at `4adc316` |
| 2 | `coldmail-send-managed` | 17:40:43 | all external contributors | [gitleaks 44139624801](https://github.com/elzainxx/coldmail-send-managed/actions/runs/44139624801) success at `ff8c302` |
| 3 | `coldmail-send-byo` | 17:40:48 | all external contributors | [gitleaks 44139642359](https://github.com/elzainxx/coldmail-send-byo/actions/runs/44139642359) success at `c70f4b6` |
| 4 | `coldmail-platform` | 17:41:01 | all external contributors | [gitleaks 44139681598](https://github.com/elzainxx/coldmail-platform/actions/runs/44139681598) success at `279f5ad` |
| 5 | `coldmail` (parent) | 17:41:07 | all external contributors | [gitleaks 44139695337](https://github.com/elzainxx/coldmail/actions/runs/44139695337) success at `f09df58` |

The billing refusal ended with the flip (the push runs of these same commits, made minutes earlier while the repositories were still private, were refused; the dispatches after the flip ran). One deviation from the requested order: the fork-pull-request approval policy cannot be set while a repository is private (the API answers 422), so it was set immediately after each flip; and the workflow edits of the forward fixes were made before the flip, together with the pins and permissions, because they touch the same files.

## CI trigger economy and its proof (platform; the modules keep their one heavy workflow with the ignore lists described above)

Design: `gitleaks` runs on every push and pull request; `gates`, `platform-check`, `go` and `web` run on pushes and pull requests with path filters (documentation no gate reads is ignored); the heavy workflows `harness`, `db-tests` and `supabase` run on `workflow_dispatch`, on tag pushes (`v*`, `phase-*`), on the weekly schedule where they had one, and on a push to `main` or a pull request that touches `db/`, `internal/`, `cmd/`, `tests/`, `tools/modules.lock`, the fetch script and its selftest, `go.mod`/`go.sum` or `Dockerfile*` (each workflow lists its own set); `concurrency` with `cancel-in-progress` on every workflow except `fault-proofs` (dispatch-only, never cancelled).

Proof runs (all on GitHub-hosted runners, platform repository):

| Push | What it changed | Workflows that started | What did NOT start |
|---|---|---|---|
| `5716216` (documentation only: this audit document) | `Docs/public-release-audit.md` | `gates`, `platform-check`, `gitleaks` | `go`, `web`, `harness`, `db-tests`, `supabase` |
| `426c9ce` (a code push: `tools/modules.lock` and the selftest it ships with) | the three module pins | `harness`, `supabase`, `gates`, `gitleaks`, `platform-check` | `go`, `web`, `db-tests` (their path sets do not name the lock) |
| `4ecc704` (a code push: `tests/harness/`) | one harness test file | `harness`, `db-tests`, `go`, `gates`, `gitleaks` | `supabase`, `web`, `platform-check` |

`cancel-in-progress` was observed: pushing `4ecc704` while `1da61b8`'s `harness` and `db-tests` were still running cancelled both (runs 44144423176 and 44144423306, conclusion `cancelled`) and started the new ones. The first hosted runs also found two real reds that the trigger work did not cause and that are fixed forward: the new anonymous-clone selftest of `tools/fetch-modules.sh` built three fixture repositories whose first commits had identical ids on a fast runner, and `platform-check`'s control "another repository's key is refused" cannot be red once the module repositories are public (GitHub serves a public repository to any authenticated SSH identity); and `gofmt` found one file (`go` workflow). New gate: `scripts/check-verified-heads.sh` (86-case selftest) refuses a "done" or `CLOSED` row of the build plan or the close-out spec unless it cites the head and the run and the heavy workflows ran green on that exact SHA; its first real run, against the live API, parsed 37 claims over 7 heads and 24 runs and passed.

## Owner cleanup after the flip

- The three `DEPLOY_KEY_COLDMAIL_*` Actions secrets and the three read-only deploy keys on the module repositories are no longer needed (the platform's workflows now clone the public modules anonymously when a key is absent); they were left in place as instructed.
- The repository description of `coldmail-platform` still says "docs only".
- Consider "Keep my email addresses private" (see above) and, for the repositories' own sake, branch protection (only available to public repositories on the free plan: not enabled here).
