# coldmail-platform — build plan

The platform is the product users touch. The three modules do the work; the platform owns users, money, jobs, authoring and administration, and is the **first real caller of the module contract**.

This is the roadmap, not the specs. Each phase gets its own detailed written spec before any code for that phase.

Ordering principle: the most irreversible parts and the contract test come first; screens and polish come last.

## Status

| Phase | Status |
|---|---|
| P0 Groundwork | **done** — platform `main` @ `a6709c9`, all six workflows green: [harness run 43731666838](https://github.com/elzainxx/coldmail-platform/actions/runs/43731666838) (the three modules built from their pins, probe 42/42, exposure, outage). Spec and evidence: [`phase-0-spec.md`](phase-0-spec.md) |
| P1 Schema | **done** — platform `main` @ `6565cd8`, every workflow green: [harness run 43774222470](https://github.com/elzainxx/coldmail-platform/actions/runs/43774222470) (module 1 moved into its own `verify` schema and running as `verify_app`; the three modules built from their pins against the platform's real `public.users` / `public.job_items`; 76/76 harness tests; probe 42/42). Spec, evidence and open decisions: [`phase-1-spec.md`](phase-1-spec.md) |
| P2 Money core | **done** — platform `main`: Part A `e5c58f4`, Part B code head `7b05c78`, and **every workflow green on `3927998`**: [harness 43952993617](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993617) (the three modules from their pins on the digest-pinned Supabase image; module 3 REALLY sending: an expired entitlement halts it and re-enabling resumes it; module 3 stopped at expiry and the platform converging once it is back; 158/158 harness tests; probe 42/42) · [db-tests 43952993468](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993468) · [go 43952993510](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993510) · [gates 43952993596](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993596) · [gitleaks 43952993470](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993470) · [supabase 43952993579](https://github.com/elzainxx/coldmail-platform/actions/runs/43952993579). Spec, the charge table, evidence and open decisions: [`phase-2-spec.md`](phase-2-spec.md) |
| P2 close-out (every open Phase 2 item, before P3) | **done** — platform@4ecc704, every workflow green on that head: [harness 44144622152](https://github.com/elzainxx/coldmail-platform/actions/runs/44144622152) (195/195, the three modules built from their pins, probe 42/42) · [db-tests 44144622289](https://github.com/elzainxx/coldmail-platform/actions/runs/44144622289) · [go 44144622159](https://github.com/elzainxx/coldmail-platform/actions/runs/44144622159) · [gates 44144622701](https://github.com/elzainxx/coldmail-platform/actions/runs/44144622701) · [gitleaks 44144622214](https://github.com/elzainxx/coldmail-platform/actions/runs/44144622214) · [web 44145501647](https://github.com/elzainxx/coldmail-platform/actions/runs/44145501647) · [platform-check 44145510731](https://github.com/elzainxx/coldmail-platform/actions/runs/44145510731) · [supabase 44145517992](https://github.com/elzainxx/coldmail-platform/actions/runs/44145517992) · fault proofs [p2c 44145564837](https://github.com/elzainxx/coldmail-platform/actions/runs/44145564837) and [p2 44145573456](https://github.com/elzainxx/coldmail-platform/actions/runs/44145573456); modules: verify@4adc316 ([ci 44139987820](https://github.com/elzainxx/coldmail-verify/actions/runs/44139987820), [gitleaks 44139447501](https://github.com/elzainxx/coldmail-verify/actions/runs/44139447501)), send-managed@ff8c302 ([real-infra 44139993785](https://github.com/elzainxx/coldmail-send-managed/actions/runs/44139993785), [gitleaks 44139624801](https://github.com/elzainxx/coldmail-send-managed/actions/runs/44139624801)), send-byo@c70f4b6 ([real-infra 44140002556](https://github.com/elzainxx/coldmail-send-byo/actions/runs/44140002556), [gitleaks 44139642359](https://github.com/elzainxx/coldmail-send-byo/actions/runs/44139642359)). Per-item evidence: [`phase-2-closeout-spec.md`](phase-2-closeout-spec.md). The repositories became public on 2026-11-17 after the audit [`public-release-audit.md`](public-release-audit.md). **P3 has not been started.** |
| P3 Module integration | **done** — `platform@52fe035` (every workflow green on that exact SHA: harness [44246863794](https://github.com/elzainxx/coldmail-platform/actions/runs/44246863794), db-tests [44249664219](https://github.com/elzainxx/coldmail-platform/actions/runs/44249664219), supabase [44249671445](https://github.com/elzainxx/coldmail-platform/actions/runs/44249671445), go [44249655943](https://github.com/elzainxx/coldmail-platform/actions/runs/44249655943), gates [44249659723](https://github.com/elzainxx/coldmail-platform/actions/runs/44249659723), gitleaks [44249651672](https://github.com/elzainxx/coldmail-platform/actions/runs/44249651672), web [44249675190](https://github.com/elzainxx/coldmail-platform/actions/runs/44249675190), platform-check [44249668028](https://github.com/elzainxx/coldmail-platform/actions/runs/44249668028)) and the Part B fault proofs [44249695726](https://github.com/elzainxx/coldmail-platform/actions/runs/44249695726). Part A closed earlier (code head e486dd1, see the spec). Three adapters (`internal/adapters/{m1,sendapi,m2,m3}`) on one plumbing client (`internal/modclient`), driven end to end against the REAL modules in the harness (item-by-item equality, pagination across page boundaries, cross-tenant refusal, outage mid-job, M2 cancel), and a contract drift gate (`internal/contractcheck`) with fixtures recorded from the real modules; findings and the Tier 4 recommendation (the owner decides): `Docs/phase-3-findings.md`; evidence row by row: `Docs/phase-3-spec.md` |
| P3 close-out (every open decision and noticed item of Phase 3, before P4) | **done** — `platform@2bc21c4` (every workflow green on that exact SHA: harness [44423183589](https://github.com/elzainxx/coldmail-platform/actions/runs/44423183589), db-tests [44423188038](https://github.com/elzainxx/coldmail-platform/actions/runs/44423188038), supabase [44423192568](https://github.com/elzainxx/coldmail-platform/actions/runs/44423192568), go [44423166724](https://github.com/elzainxx/coldmail-platform/actions/runs/44423166724), gates [44423166736](https://github.com/elzainxx/coldmail-platform/actions/runs/44423166736), gitleaks [44423166751](https://github.com/elzainxx/coldmail-platform/actions/runs/44423166751), web [44423196895](https://github.com/elzainxx/coldmail-platform/actions/runs/44423196895), platform-check [44423201437](https://github.com/elzainxx/coldmail-platform/actions/runs/44423201437)) and the four fault-proof dispatches ([44423205785](https://github.com/elzainxx/coldmail-platform/actions/runs/44423205785) p3ca, [44423210661](https://github.com/elzainxx/coldmail-platform/actions/runs/44423210661) p3cb, [44423214938](https://github.com/elzainxx/coldmail-platform/actions/runs/44423214938) p3cc, [44423219384](https://github.com/elzainxx/coldmail-platform/actions/runs/44423219384) p3cd); the three modules at their released heads: `verify@fad942e` ([ci 44352471306](https://github.com/elzainxx/coldmail-verify/actions/runs/44352471306), [gitleaks 44352471259](https://github.com/elzainxx/coldmail-verify/actions/runs/44352471259)), `send-managed@dc2b532` ([real-infra 44354675361](https://github.com/elzainxx/coldmail-send-managed/actions/runs/44354675361), [gitleaks 44354675334](https://github.com/elzainxx/coldmail-send-managed/actions/runs/44354675334)), `send-byo@8855a52` ([real-infra 44371713201](https://github.com/elzainxx/coldmail-send-byo/actions/runs/44371713201), [gitleaks 44371713288](https://github.com/elzainxx/coldmail-send-byo/actions/runs/44371713288)). Every owner ruling is built (module 1: describe, idempotent create, totals, a 400 for a bad job id, node self-promotion, key rotation, one address rule `norm.Validate`; module 2: the worker reconnects forever, a forged cursor is 400, the optional plain-text part; module 3: `POST /v1/cancel`, the run 409 names its causes, no internal text on the wire, the SMTP host destination guard, the plain-text part, `describe` says `items`; the platform: module 3's From-gate ON in the harness, the enforced health action observed, the liveness cell of `Health`, the adapters for all of it), every open decision CLOSED with its evidence, the scratch branches deleted, the not-checked list reprinted with an owner for each line: [`phase-3-closeout-spec.md`](phase-3-closeout-spec.md) |
| P4 Job engine (with its close-out: owner rulings on module pause, resume and cancel, the vendor field, suppression scope, the unknown choice, fair work order, the sender crash matrix) | **done** — `platform@5c0f2da` (every workflow green on that exact SHA: harness [45907519507](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519507), db-tests [45907519672](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519672), supabase [45907519544](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519544), go [45907519528](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519528), gates [45907519509](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519509), gitleaks [45907519525](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519525), web [45907519506](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519506), platform-check [45907519543](https://github.com/elzainxx/coldmail-platform/actions/runs/45907519543)) and the scope proofs that need a killed process or a stopped container, on the same SHA (jobs [45907586193](https://github.com/elzainxx/coldmail-platform/actions/runs/45907586193), sendjobs [45907594809](https://github.com/elzainxx/coldmail-platform/actions/runs/45907594809), gotrue [45907603019](https://github.com/elzainxx/coldmail-platform/actions/runs/45907603019)) and the fault-proof dispatches (p4a [45907606872](https://github.com/elzainxx/coldmail-platform/actions/runs/45907606872), p4b [45907615376](https://github.com/elzainxx/coldmail-platform/actions/runs/45907615376), p4c [45907623332](https://github.com/elzainxx/coldmail-platform/actions/runs/45907623332)); the three modules at their released heads: `verify@c516f28` ([ci 45868763567](https://github.com/elzainxx/coldmail-verify/actions/runs/45868763567), [gitleaks 45868763610](https://github.com/elzainxx/coldmail-verify/actions/runs/45868763610)), `send-managed@d023fdb` ([real-infra 45869828717](https://github.com/elzainxx/coldmail-send-managed/actions/runs/45869828717), [gitleaks 45869828795](https://github.com/elzainxx/coldmail-send-managed/actions/runs/45869828795)), `send-byo@59b2d2e` ([real-infra 45872669121](https://github.com/elzainxx/coldmail-send-byo/actions/runs/45872669121), [gitleaks 45872669139](https://github.com/elzainxx/coldmail-send-byo/actions/runs/45872669139)); every row of [`phase-4-spec.md`](phase-4-spec.md) is CLOSED with its runs |
| P5 Templates and rendering (with the owner decisions of Phase 4 closed) | in progress — one phase, one session: [`phase-5-spec.md`](phase-5-spec.md) |
| P6 User app | not started |
| P7 Admin console | not started |
| P8 Hardening and launch (the former P8 and P9, merged) | not started |

**Keep this table current from the first commit.** The alignment fix plan once claimed it had never been performed while most of it was done; the real state was recoverable only from git logs.

---

## Locked decisions

### Scope
- All three modules at launch: **verify** (M1), **send-managed** (M2), **send-byo** (M3)
- First users: a few hand-picked customers
- Full platform built directly (no throwaway spike). The contract risk is contained by **P3 coming before any user-facing screen**.
- M1 is assumed to have its port-25 host, so it launches fully capable
- Repositories are public (owner decision 2026-11-17): source visible for transparency, all rights reserved; nothing real (secret, host, address, personal path) may be committed.

### Money
- **Per-module credits.** Each user has a separate balance for M1 and for M2. **Never transferable** between modules.
- **1 credit = 1 unit of work.** Costs per unit live in a platform config table:

| Unit | Default cost | Notes |
|---|---|---|
| M1 verification, own infrastructure | 1 | |
| M1 verification, answered from cache | 1 | Full price — the answer is the value |
| M1 verification, answered by vendor fallback | admin-set, > 1 | Premium; it costs real money |
| M2 send, accepted | 1 | |
| M2 send, bounced (550) | 1 | Work was done; the bad address is the user's list |

- **M3 is not metered.** Admin **enables it per user**, with an optional **enabled-until date** that auto-disables. No per-send charge. Disabling uses M3's existing per-user suspend, which halts losslessly.
- **One append-only ledger** for everything: grants, reserves, charges, releases, and M3 enable/disable events, every row tagged with its module.
- **Reserve at confirm, settle on report, release the remainder.** The **cost per unit is frozen at reserve** and settlement uses it, never the current table.
- **Charge for an answer, or a message that reached the outside world. Never charge for our own failure or for work that wasn't done.**
  - Not charged: our infrastructure failures, pool exhaustion, cancelled, expired, quarantined, M1 `unknown` from our own failure or that cannot be proven either way (see the unknown rule below), a user's own SMTP rejecting the login, a **malformed address** (one that fails the platform's one address rule, `norm.Validate`: module 1's frozen `norm/v1` and then module 1's own syntax layer exactly as module 1 implements it (a local part over 64 bytes, a domain over 255 bytes or with no dot); the platform rejects it **before any module is called**, counts and skips it, so it is never sent and never charged; charge-table row P-08; module 1's own `invalid_syntax` answer, row M1-10, is unreachable from the platform; the Phase 3 close-out (OD-9) extended the rule from `norm/v1` alone to both halves and the suppressions table follows it (platform_0009); the owner's Phase 3 ruling; module 1's own `invalid_syntax` row M1-10 stays for what its syntax layer refuses after normalising)
  - The rare crash-window duplicate is charged once — billing is per job item, not per delivery
  - **The unknown rule (the owner's, ruled in the Phase 2 close-out):** *"An unknown caused by the recipient's server is charged; one caused by us, or that can't be proven either way, is free."* The platform reads module 1's `signals.failure_origin` (`own` | `remote` | `undetermined`, on every `unknown`) and the table decides: `remote` is charged as `m1_own`; `own` and `undetermined` are free; an `unknown` that names no origin (an older module) is **unmapped** (never charged, reported); a greylisted `unknown` follows its origin; module 1's `smtp_unwired` skip token (SMTP not wired at boot) is free; an item that reached module 1's attempt cap (settled `unknown/unknown_error`, origin `own`) is free. Module 1's vendor answers (ZeroBounce, shipped disabled) are charged as `m1_vendor`; a vendor non-answer leaves module 1's own result standing and is never a vendor charge. Module 2's `failed` (including `attempts_exhausted`) and `cancelled` are free. A repeat of an address inside one module-1 job (`duplicate_of`) is checked once and charged once, on the first row. The table itself is data (`internal/charge/rules.psv`, mirrored in [`phase-2-spec.md`](phase-2-spec.md), gate `check-charge-table`).
- A job whose balance doesn't cover the whole list is **blocked at start**, with the shortfall shown.
- Scheduled campaigns **reserve at confirm**, so they are guaranteed to start.

### Identity
- **The tenant is a person:** one login = one account = one set of balances. Moving to companies later would mean re-keying every module's tenant scoping and history; this is a deliberate lock.
- Supabase Auth, email + password, **admin-created** with an invite link. No public signup.
- **Read-only impersonation**: audited, time-limited, reason-required. Acting for a user is a separate admin action recorded as the admin.

### Admin and monitoring
- **The earlier rule "admin never sees user content" is retired.** A monitoring dashboard shows all content.
  - Every admin view of content is **logged** — a record, not a restriction.
  - **SMTP credentials are not content.** They stay behind M3's break-glass.
  - Message bodies are viewable for the debug retention window (**7 days**); outcomes are kept forever.
  - Customers must be told in the terms that the operator can view campaign content.
- **One admin console** for everything: users, credits, entitlements, unit costs, impersonation, audit, monitoring, and every module's operations.
- **One admin now, roles later.** Every admin action is recorded against a **named admin user** from day one. The modules' APIs only know a single operator key, so the platform audit log is the only place that knows which admin acted.

### Module integration
- **Plan A+:** one plumbing layer (auth, error envelope, health, vocabularies, `describe`) plus a **thin adapter per module**. M1 keeps `/verify` and bulk as its domain verbs.
- **The platform fits the module tables as built.** `public.users` and `public.job_items` take exactly the shapes the modules already read. Design from the modules' **code**, not their docs. This closes OD-8.
- **Polling, as built:** `report` every 1–2 seconds.
- Platform → module auth: the service token over the private network. Modules are never public (the running services are never exposed; the repositories themselves are public, see Scope).
- **Tier 4 (decided, Phase 3 close-out):** module 1 adopted a PARTIAL Tier 4, additively: `GET /v1/describe`, an idempotent bulk job creation on an `Idempotency-Key`, and totals on the job status; **no** quote/run/report parity (module 1's price depends on the answer, so a quote could only be an estimate). The evidence is `phase-3-findings.md` section 5 and its close-out addendum; the rulings are in `phase-3-closeout-spec.md`.
- **Module 3 has a cancel** (`POST /v1/cancel`, the same semantics as module 2's: queued items become `cancelled`, in-flight items finish and are reported once; the item state is free, charge row M3-01), a `details[]` code on the `run` 409 (suspended, campaign_active, campaign_cap, queue_cap), user-vocabulary codes only on its test button and bulk add, and a destination guard on every SMTP host a user gives it. Modules 2 and 3 take an optional plain-text part (the platform's `public.job_items.rendered_text`, platform_0010) and send `multipart/alternative` when it is set.
- **One caller key per user** for module 1, derived from `PLATFORM_M1_KEY_SECRET` and a key version (`PLATFORM_M1_KEY_VERSION`) so the secret can be rotated while the old keys still work (`Docs/config-reference.md`).

### Jobs
- **One active job per module per user.** A user can verify while a campaign sends.
- **Verify-then-send chaining:** a send can take a verification's results as input. Default: **exclude `invalid`, include `valid` and `risky`, user chooses for `unknown`**, with counts per verdict on the preview. (M1 marks every Gmail, Outlook and Yahoo address `risky` by design, so "valid only" would silently drop most consumer addresses.)
- Chaining checks every balance and entitlement involved before starting.
- Preview → confirm → reserve → run → poll → settle, with pause, cancel and crash-resume. Results stored forever, paginated and downloadable as CSV.

### Authoring
- **HTML with an auto-generated plain-text part.**
- **Merge fields with fallback:** `{{first_name|there}}`. A row missing a field with no fallback is counted and skipped at preview, never sent broken.
- **Spintax** `{Hi|Hello}`, **seeded deterministically per job and recipient**, so a retried send renders identically — preserving the senders' byte-identical duplicate guarantee. The preview shows several sample renders.
- **No template library:** content is written per campaign.
- **Reply-To per campaign.** From is fixed per account (M2) or per SMTP account behind the From gate (M3).
- **Unsubscribe link plus a per-user suppression list**, applied to every future job. Needs a public unsubscribe endpoint on the platform.
- Hard bounces are **not** auto-suppressed (decided).
- Send now, or at a scheduled start time.

### Stack
- **Backend:** Go service — the gateway, ledger, job engine and polling loops
- **Frontend:** React + TypeScript + Tailwind, hand-built
- **Data and auth:** Supabase (Postgres + Auth), the same database the modules use, each in its own schema
- **App shell:** built from scratch
- **Design:** a new design system for this product

---

## Inherited rules (from building the three modules)

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
- [`deferrals-and-false-greens.md`](../deferrals-and-false-greens.md) (repository root) lists the twenty false-green classes found so far. Read it before writing gates.

---

## Phases

### P0 — Groundwork
**Builds**
- Go service and React app skeletons in `coldmail-platform`
- Supabase project wiring, migration runner, a `platform` schema boundary
- `CLAUDE.md` operating rules (the inherited rules above)
- A real-infrastructure test harness, and **CI on a GitHub runner from the first commit**
- The modules available to CI as real running services (via the parent repo's submodules)

**Exit criteria**
- A trivial end-to-end call — platform → each module's `describe` — passes **on the runner**
- Every gate proven to fail on an injected fault

### P1 — Schema
The most irreversible phase.

**Builds**
- `public.users` and `public.job_items` **in exactly the shapes the three modules read**, derived from their code
- jobs (with module, source job for chaining, schedule time)
- **Balances per user per module**, the append-only ledger (module-tagged, cost frozen at reserve)
- M3 entitlements (enabled, enabled-until)
- The unit-cost table, the module registry
- The per-user suppression list
- The admin audit log, with **named admin identity** and content-view logging

**Exit criteria**
- Every module's pre-flight table-shape check passes against the platform's tables
- The ledger rejects UPDATE and DELETE at the database
- Grants proven per role

### P2 — Money core
**Builds**
- Grant, reserve, settle, release — per module, atomic
- M3 enable/disable/expire, driving M3's per-user suspend
- The charge rules as one function from module outcome to credits

**Exit criteria**
- Two concurrent reserves can never overdraw a balance — a forced interleave, with a control
- A reserve settles at its frozen cost after the cost table changes
- Every charge rule has a test per outcome class, including the not-charged ones
- An expired M3 entitlement halts sending losslessly

### P3 — Module integration (the contract test)
**Builds**
- The plumbing client: auth, envelope, health/readyz, vocabularies, `describe`
- Adapters for M1 (`/verify` + bulk), M2 and M3
- The tenant id flowing from login into each module's tenant scoping

**Exit criteria**
- Each adapter driven against the **real module** on the runner — quote/run/report or verify/bulk, end to end
- A cross-tenant read through the platform is refused by the module
- A written finding: did the contract hold? How thick did each adapter get? **The Tier 4 decision for M1, made on this evidence.**

### P4 — Job engine
**Builds**
- The lifecycle: preview → confirm → reserve → run → poll → settle → release
- Pause, cancel, resume after a platform crash
- One active job per module per user
- Verify-then-send chaining with the verdict defaults
- Scheduling, with reserve at confirm
- Suppression filtering at job creation

**Exit criteria**
- A platform crash mid-job resumes with no item lost, duplicated or double-charged
- A module outage halts polling cleanly and **invents no outcome**
- Settlement matches the module report exactly, item by item
- Chained jobs move exactly the selected verdicts

### P5 — Templates and rendering
**Builds**
- The HTML editor model and plain-text generation
- Merge fields with fallback; missing-field accounting at preview
- Seeded spintax and sample renders
- The unsubscribe link and the public unsubscribe endpoint, with the RFC 8058 headers on every send (modules 2 and 3 carry them)
- The per-user vendor opt-out (a policy without the `external` block for module 1), the paused-job limit (7 days) and the 7-day content retention (added by the owner rulings of Phase 4)

**Exit criteria**
- The same job and recipient always render byte-identically
- A row without a field or fallback is never sent
- An unsubscribe lands on the suppression list and is excluded from the next job
- An opted-out user's module 1 job carries no `external` block and reserves at 1; a paused job is cancelled at the limit; content is cleared 7 days after a job ends and outcomes are kept

### P6 — User app
**Builds**
- The app shell and design system
- Invite login, dashboard, balances per module, M3 status
- CSV upload, the authoring screen, the **preview with exact cost and counts per verdict**
- Live job table with partial results, history, CSV download
- M3 SMTP account management through its extension routes, including the test button and bulk add
- The suppression list screen

**Exit criteria**
- Every screen works against the real backend
- Every user-facing string reviewed against the copy rules (no internal mechanics, no false certainty)

### P7 — Admin console
**Builds**
- Users, invites, credit grants per module, M3 entitlements, unit costs
- The monitoring dashboard: all content, every view logged
- Read-only impersonation
- The audit log
- Module operations — health and alerts first, then M2's pool, M1's nodes, quarantine, config dials
- M3 credential break-glass, kept separate from monitoring

**Exit criteria**
- Every admin action appears in the audit log under the named admin
- Impersonation cannot act, only view

### P8 — Hardening and launch (the former P8 Hardening and P9 Close, merged by the owner in Phase 5)
**Builds**
- A platform-wide panic switch and per-module kill switches
- Observability and alerts for the platform itself
- Backups and a restore drill, including the M3 master key
- A security review of the public surfaces (login, unsubscribe endpoint)
- Load test against realistic job sizes
- The deployment: one public edge (Caddy, automatic HTTPS) in front of the platform API and the web app only, the modules on the private network (the trusted forwarded-for header for the unsubscribe rate limit is set here)
- Operational docs, the runbook, the go-live checklist, with the dated promotion steps (retention at L+14, account purge at L+28, the shadow safeguards after 14 quiet days)
- An onboarding runbook for the first hand-picked customers
- The terms and the disclosure wordings (content monitoring, vendor egress with its opt-out, "your sends train our accuracy", unsubscribe handling)

**Exit criteria**
- The panic switch halts every module losslessly
- An instance cut off from the database by a network partition loses the platform's leader role (the expiry and reconcile loops) within the TCP keepalive window, and a second instance takes over; no entitlement is expired or reconciled twice (added by the Phase 2 close-out, E4)
- Settlement of a job far larger than the harness's (the load test) matches the module report exactly, item by item (added by the Phase 2 close-out, E4)
- A restore drill passes from backup alone
- A clean install reproduced from the docs alone
- Every open decision closed or moved to future work

---

## Owner actions that gate launch (not code)

- The real-provider smoke test for both senders
- The port-25 host for M1
- The vendor data-processing agreement, if the vendor fallback is used
- Terms covering content monitoring and unsubscribe handling (legal review recommended)
- Docker repaired on the dev machine
