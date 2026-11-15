# Phase 0 — Groundwork: spec

Roadmap: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md) (locked decisions are not reopened here). Diagram: [`coldmail-platform-architecture.mermaid`](coldmail-platform-architecture.mermaid).
False-green classes every gate below must avoid: [`../deferrals-and-false-greens.md`](../deferrals-and-false-greens.md) (the build plan cites it as `Docs/deferrals-and-false-greens.md`; the file lives at the repository root — see Findings F8).

**Status: in progress.** Evidence rule: Go tests, the harness and every gate count only from a **GitHub runner** run. A Windows-host run is rehearsal, never evidence.

## Design choices (mine — overturn any of them and I rework only that chunk)

- **One Go module at the repository root** (`github.com/coldmail-platform/platform`): `cmd/platform`, `cmd/platform-migrate`, `cmd/platform-probe`, `internal/…`. Integration tests live in `tests/harness` behind the build tag `harness`, so `go test ./...` never needs a database and an integration test can never silently skip into a green.
- **Web** in `web/` (Vite + React + TypeScript + Tailwind). Migrations in `db/migrations/platform_NNNN_*.{up,down}.sql` (the M2/M3 layout).
- **Modules are pinned in `tools/modules.lock`** (three SHAs, in this repository). `tools/fetch-modules.sh --lock` clones with the existing read-only deploy keys, checks out the pinned SHA and asserts `HEAD == pin`. The parent's gitlinks are the same three SHAs; `tools/check-modules-lock.sh <parent-dir>` compares them at re-pin time (the parent has no CI that can read private submodules — F9).
- **Harness shape:** one real Postgres (the modules' own `postgres:16-alpine` digest), the three module Controls built **from the pinned checkouts' own Dockerfiles** (M1 has none — F2 — so M1 gets a platform-owned Dockerfile that builds the pinned source), on a Docker network with `internal: true` and **no published ports**. Everything that talks to the modules runs *inside* that network (the `platform-probe` image, and the harness test binary built with `go test -c` and run in a digest-pinned alpine). Docker is not needed on the developer machine.
- **One advisory-lock band for the platform: `7_xxx_xxx_xxx`** (M3 holds 8_, M2 holds 9_ — the owner-action M2-GL-3 / M3 §3 asks the platform to stay out of both).
- **Roles:** `platform_owner` NOLOGIN (owns everything), `platform_app` LOGIN non-super (runtime), `platform_admin` NOLOGIN (break-glass, no members; M1's `cp_admin` precedent). No module has all three; M2/M3 have owner+app, M1 has runtime+admin (F6).
- **Fault branches** are named `fault/p0-<gate>`. I may not delete anything on GitHub, so they **stay** (a revert commit goes on each); they are listed in `OWNER-ACTIONS.md` for you to delete. Because the secret scan reads every fetched branch (`--log-opts=--all`), the one planted-token branch would otherwise redden `main` for ever; its single finding fingerprint goes in `.gitleaksignore` with a comment, and a gate fails if that file holds anything else.

## Chunks (each ≈1–2 h; every chunk has an exit check, all on the runner)

| # | Chunk | Exit check |
|---|---|---|
| 0 | **Records and housekeeping** — plan + diagram committed, status table, this spec; PARKED headings in M1/M2 `future-work.md`; M1's four merged `phase-*` branches deleted (ancestry proven first); OWNER-ACTIONS updated | Spec committed; M1 and M2 CI green on their PARKED commits |
| 1 | **Gates, first** — `check-test-floors.sh` (declared vs ran per package, nonzero, no skips, package set == floors file), `check-image-pins.sh`, each with a selftest whose reject cases fail *for the named reason*; `tools/modules.lock` + `fetch-modules.sh --lock` | `gates` workflow green; every selftest case prints the failure it provoked |
| 2 | **Go service skeleton** — env config with documented defaults (a test fails when code and `Docs/config-reference.md` disagree, floor-guarded), `slog` JSON logging with request ids, `/healthz`, `/readyz` (db ping), the modules' error envelope, graceful shutdown (paired control: a non-draining variant must fail the same test) | `go` workflow green, per-package counts printed and asserted |
| 3 | **React + TS + Tailwind skeleton** — one placeholder page, `tsc --noEmit`, build, one render test; no screens, no design system | `web` workflow green, test count asserted ≥ floor |
| 4 | **Database and Supabase wiring** — bootstrap migration (`platform` schema, three roles, `schema_migrations`, default privileges), `platform-migrate` (up/down/status/set-app-password, advisory lock, contiguity guard), migration lint, grant-proof tests with an anti-vacuity control. **No business tables.** The migrator needs a *direct* (session-mode) connection, not the pooler — documented | `harness` workflow: DB tests ran against a real Postgres, counts asserted, grants proven per role |
| 5 | **Modules as real services** — `docker-compose` harness: Postgres; fixtures `public.users` / `public.job_items` (FIXTURE ONLY, taken from the pinned M2 checkout, P1 builds the real ones); migrate platform + M1 (psql loop) + M2 + M3; mint certs, KMS keys and service tokens; start the three Controls and the platform | All five services healthy on the runner, built from the pinned SHAs (the SHAs are printed) |
| 6 | **The end-to-end call** — `platform-probe` calls each module over the private network: health, readyz, and `describe` (M2, M3) with the service token; M1 (no `describe`) gets health, readyz and an authenticated-vs-unauthenticated pair. Exposure check (no published ports, network internal). The harness prints what it did **not** check | Probe green on the runner; exposure check green; "not checked" list printed |
| 7 | **`CLAUDE.md`** — inherited rules, repository layout, how to run CI; `README.md` corrected (it still says "documentation only") | The docs link check (`platform-check`) green with the new files |
| 8 | **Every gate proven red** — one throwaway branch per gate, fault committed, red run linked, reverted (table below) | A red run URL per gate, and a green run URL after each revert |
| 9 | **Close** — status table P0 done with the final head and the green run; module PARKED commits re-pinned in `modules.lock`; parent re-pinned | Table row names the SHA and the run |

## Gates and the fault each one is proven against

| Gate | Fault injected on a throwaway branch | Red run | Reverted/green run |
|---|---|---|---|
| Go build / vet / gofmt | compile error | _pending_ | _pending_ |
| Unit tests | assertion made false | _pending_ | _pending_ |
| Test-count floor | a test deleted from a package (everything still passes) and, separately, a `t.Skip` | _pending_ | _pending_ |
| Digest-pin gate | `FROM alpine:latest` | _pending_ | _pending_ |
| Web build and test | type error; render test broken | _pending_ | _pending_ |
| Migration lint | an up file without its down / a gap in the chain | _pending_ | _pending_ |
| Grant proof | `GRANT UPDATE` on `schema_migrations` to `platform_app` | _pending_ | _pending_ |
| Pinned-module check | `modules.lock` names a commit that does not exist in the module | _pending_ | _pending_ |
| Harness probe | wrong service token for one module | _pending_ | _pending_ |
| Exposure check | `ports:` added to one module | _pending_ | _pending_ |
| Secret scan | planted token (fingerprint then ignored — see above) | _pending_ | _pending_ |
| Ignore-file gate (`check-gitleaksignore`) | an ignore entry for a commit on `main` | _pending_ | _pending_ |
| Docs check (`platform-check`) | a broken internal link | _pending_ | _pending_ |

## What the harness will state it did NOT check (printed on every run)

Workers, SMTP and the mail sink (not started); real Supabase (stock Postgres with superuser `postgres`; Supabase's `postgres` role is not a superuser); the pooler; external reachability from another host or firewall; the platform's *real* `public.users`/`public.job_items` (P1); module behaviour beyond health/readyz/describe (P3); TLS termination.

## Findings (from the module code, not the docs — flagged, not fixed)

- **F1.** M1 has **no `describe`** and exposes six routes (`/healthz`, `/readyz`, `POST /v1/verify`, bulk, job status, results). Its `/readyz` 503 body is not the error envelope. The Tier 4 decision stays in P3.
- **F2.** M1 has **no control-plane Dockerfile, no compose file, no migration tool and no preflight**; its CI builds natively and applies migrations with a `psql` loop. The harness supplies its own Dockerfile and loop.
- **F3.** M1's runtime DSN must connect as owner or superuser (startup writes `policies`, `cp_runtime` has only SELECT). Inferred from the code, not measured.
- **F4.** M1 creates its tables **unqualified in `public`**, with no schema, against the plan's "each in its own schema" (the known deferral D7). P1 must keep the platform's own tables out of `public` except `users` and `job_items`.
- **F5.** M2 and M3 `/v1/describe` exist with different shapes by design (M3 adds `extension_routes`, `schemas`, `bulk_add`, two more error codes); `unit_price_credits` is 1 in M2 and 0 in M3 (`cost_fields_inert`).
- **F6.** Role patterns differ across modules (owner+app / runtime+admin); none uses `SET ROLE` at runtime; M2/M3 migrations need a superuser (`CREATE ROLE`, `OWNER TO`).
- **F7.** M2/M3 `docker-compose.yml` refuses to start `control` alone (`${DEV_WORKER_TOKEN…:?}` is interpolated for every service) and publishes ports; the harness therefore defines its own compose and reuses only the modules' Dockerfiles and secret tools.
- **F8.** `deferrals-and-false-greens.md` is at the repository root, not `Docs/` as the build plan says; the build plan's attached copies had landed in the older `D:\Mnfi\coldmail\` tree. **M1 has `PROJECT_RULES.md`, no `CLAUDE.md`.**
- **F9.** Nothing automated ties `modules.lock` to the parent's gitlinks.

## Open decisions (yours)

1. **Delete the `fault/p0-*` branches** on `elzainxx/coldmail-platform` when the table above is complete (no deletion authority was given for them), then remove the entry from `.gitleaksignore`.
2. **Supabase project:** none is created in Phase 0 (wiring and a runner only). Creating one, and confirming M2-GL-3 / M3 §3 (which admin role runs migrations) against it, is yours.
3. **F1/F2/F4** are recorded for P3/P1; no action requested now.

## Housekeeping log

- 2026-11-15 — M1 `phase-2-smtp-mocks` (`c71c0e9`), `phase-3-first-real-box` (`1e6cf2c`), `phase-4-resilience-pool` (`525d1c0`), `phase-5-queue` (`062c7a0`): exactly four `phase-*` branches existed; each head is an ancestor of `main` (`ahead_by=0` by the compare API, and `git merge-base --is-ancestor` locally; no open PRs); deleted. Remaining branch: `main`. Their commits stay reachable through `main`.
- 2026-11-15 — PARKED: M1 `future-work.md` §5 retitled (text unchanged — FW-6…FW-9 already lived there in full); M2 `future-work.md` new §7b holds `M2-FW-1…7` verbatim from `OWNER-ACTIONS.md` §7, where the ids had been defined nowhere else; M3 has none. `OWNER-ACTIONS.md` §7 removed.
