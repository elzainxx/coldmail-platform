# The Canonical Module Contract

**Companion to** `module-comparison-report.md` (the audit) and `alignment-fix-plan.md` (the ordered work).
**Status: PROPOSAL.** Nothing here is implemented. This document describes what the platform↔module boundary
**should** be so that one integration can drive every module. No repo was changed to produce it.
**Date:** 2026-08-04.

**Read the provenance warning first.** This contract is derived from what M2 and M3 implement, because they
are the only modules that implement a uniform contract at all. **M3 inherited it from M2 by fork** — their
`platformapi/auth.go` is byte-identical and their `run` and `report` handlers are character-identical. So the
contract below has been validated by **exactly one** independent implementation, not two. Wherever a clause
rests only on M2↔M3 agreement it is marked **[weak]**; where M1 independently arrived at the same answer it
is marked **[strong]**. The one place M3 genuinely had to think rather than copy — `quote`, where its billing
model differs — is precisely the one place the contract broke (D4). **Treat that as a signal about the
contract, not only about M3, and review the clauses below on their merits before asking any module to adopt
them.**

---

## Part 1 — THE CONTRACT

### 1.1 Transport and versioning **[strong]**

- All routes under a `/v1/` URL prefix. Version in the path, never in a header.
- Go-style method+path routing; every route declares its method.
- **Rationale for strong confidence:** M1 and M2 arrived at this independently. It is the single most
  reliable agreement in the audit and should not be revisited.

**REQUIRED of every module:**

1. A `/v1/`-prefixed path for every platform-facing route.
2. No version negotiation header. A new major version is a new path prefix.
3. **An in-code assertion that the response matches the declared version** — M1's pattern
   (`const APIVersion = "v1"` plus `Valid()` before every write, returning 500 rather than shipping an
   off-contract body). Neither sender has this; both should adopt it.
4. **A CI gate proving served-set == documented-set** — the senders' `contract_drift` pattern (probe every
   documented route unauthenticated, assert 401 to prove it is mounted and gated; assert an undocumented
   path 404s; assert every served path appears in the contract doc). M1 has no equivalent; it should adopt it.

Clauses 3 and 4 are two halves of one property, and **no module currently has both.**

### 1.2 Authentication **[strong on the header, contested on the model]**

**REQUIRED:**

1. `Authorization: Bearer <token>`. **The scheme is mandatory** — a bare token MUST be rejected. (M1 accepts
   a bare token today; that is a laxness to close, not a variation to accommodate.)
2. Comparison MUST be constant-time with respect to which credential matched. Both patterns in use qualify:
   bcrypt looped over all hashes with no early return (senders), or an indexed lookup on a digest plus
   `crypto/subtle.ConstantTimeCompare` for any direct comparison (M1).
3. **Fail-closed when unconfigured.** No credentials configured MUST mean no service, never open service.
   M1's dev-open path is acceptable only because it is gated on `Env == "development"`; a production build
   MUST NOT have it. M3's behaviour — unmounting the surface entirely — is the strongest form.
4. **The platform credential and the operator credential are never the same credential, and never share a
   store.** Distinct type, distinct env var, distinct constructor, no import between the packages.
5. **The credential MUST resolve to a caller identity, and that identity MUST be recorded on the data it
   creates.** This is M1's `api_callers` model, which is the best of the three — and clause 1.4 below is why
   it is stated here as mandatory rather than optional.

**Explicitly NOT required:** a specific hash function. bcrypt-over-a-small-set and sha256-indexed-lookup are
both correct for their respective access patterns; forcing one would be cargo-culting.

### 1.3 Error envelope

**REQUIRED — exactly one shape, on every route, on every failure:**

```json
{ "error": { "code": "<enum>", "message": "<human, safe>", "details": [{"field":"…","issue":"…"}], "request_id": "…" } }
```

1. `code` from a declared, additive-only enum. `message` is human-readable and MUST be safe to display.
   `details[]` is optional. **`request_id` is MANDATORY on every error response** — this is what makes a
   user-reported failure correlatable to a server-side record.
2. **The same `request_id` MUST appear in at least one log line** for that request. M1 satisfies neither
   this nor clause 1 today.
3. **Exactly one serializer.** Not "handlers agree on a shape" — one function. M1 hand-rolls at ~25 sites
   and already has a second shape as a result (D-list).
   > **A single serializer is necessary but NOT sufficient, and no module satisfies this clause today
   > (D17).** All three register Go 1.22 method+path patterns, so `net/http.ServeMux` answers a method
   > mismatch or an unmatched path **before any handler or middleware runs**: `405` / `404` in
   > `text/plain`, no `code`, no `request_id`, unauthenticated. **The senders' own `contract_drift.sh`
   > cannot catch it** — it probes each route with the method-appropriate verb only. Satisfying this
   > clause therefore requires a **catch-all handler** (`mux.HandleFunc("/", …)` plus explicit
   > per-path method fallbacks, or a middleware that owns method dispatch) that routes these through
   > the envelope as `not_found` / a new `method_not_allowed` code. Until then, **clause 1's "MANDATORY
   > `request_id` on every error response" is aspirational, not implemented, in every module.**
4. **Two-vocabulary rule.** An internal reason string is NEVER serialized to a client. Internal detail goes
   to logs keyed by `request_id`. Never emit credentials, ciphertext, key ids, SQL, SQLSTATE, stack traces,
   internal hostnames, or internal reason slugs across the boundary.
5. **Enforce clause 4 mechanically, not by discipline.** M3's `user_output_scrub` gate is the only mechanism
   in any of the three repos that does this: every read of the SMTP-response column must go through one named
   accessor, and the internal-slug column must have **zero** readers in the platform package. A one-writer
   convention (M2) is good but is a discipline; a static gate is a barrier.

**The declared code set** — the union of what the senders implement, since M3's additions are the only
evidence of what real modules need:

| Code | HTTP | Meaning |
|---|---|---|
| `validation_failed` | 400 | Request shape or field invalid |
| `unauthenticated` | 401 | Missing/invalid credential |
| `not_found` | 404 | Not found, **or not yours** (see 1.4) |
| `conflict` | 409 | Idempotency or state conflict |
| `unprocessable` | 422 | Well-formed but semantically rejected |
| `rate_limited` | 429 | Throttled |
| `internal_error` | 500 | Module fault |

Issue slugs for `details[].issue`: `required`, `malformed`, `out_of_range`, `not_found`. **[weak — M2↔M3
inheritance; lock these by contract rather than assuming they stay stable.]**

6. **CLIENTS MUST TOLERATE UNKNOWN CODES.** A module MAY add a code (additive-only); a client MUST map an
   unrecognised code to a generic failure by HTTP status, never crash or fall through. **This clause exists
   because the divergence already happened** — M2 implements five codes and M3 seven, at the same `/v1`, and
   a client with an exhaustive switch over M2's set breaks against M3.

### 1.4 Tenancy

**REQUIRED:**

1. **Every platform-facing route that touches tenant data takes an explicit tenant selector.** The senders'
   `user_id` (a platform `public.users.id` UUID) in the body for writes, the query string for reads.
2. **The selector is validated as a UUID, with `required` and `malformed` reported distinctly.**
3. **Every read and write query carries the tenant predicate.** Not middleware, not RLS — the predicate is in
   the SQL. RLS is a defence-in-depth backstop and MUST NOT be the isolation mechanism.
4. **404, never 403, for another tenant's resource — and the 404 MUST be byte-identical to a genuinely
   nonexistent resource's 404** (same code, same message, same status, same details). No existence
   disclosure.
5. **A request MUST NOT be able to change ownership.** Prefer structural prevention: M3 omits `user_id` from
   its update request type entirely so strict decoding rejects it, rather than checking at runtime.
6. **A module MUST NOT rely on the credential to identify the tenant, and MUST NOT rely on the caller to
   assert its own identity, at the same time.** Pick one and make it load-bearing:
   - **Selector-based** (senders): the caller supplies `user_id`; the credential authorises the *platform*,
     not the user. The platform is trusted to pass the right `user_id`. Coherent.
   - **Credential-based** (what M1's `api_callers` makes possible): the identity comes from the key and the
     caller cannot assert it. Coherent, and stronger.
   - **What M1 actually does is neither**, and that is the defect: it authenticates a caller, then reads by
     id alone. **This clause is the contract form of D1.**

**Tenant identity space — an open item, not a settled clause.** The senders key on the platform's
`users.id` UUID; M1 keys on its own `caller_id` text and stores it nowhere. **These are different identity
spaces and the contract cannot paper over it.** Either M1 adopts platform `users.id`, or the platform
maintains a `caller_id ↔ user_id` mapping. This is a platform decision; the register (Part 2, A6) states
what breaks either way.

### 1.5 The verb set

**REQUIRED — four core verbs:**

| Verb | Method + path | Purpose | Must be side-effect-free |
|---|---|---|---|
| `describe` | `GET /v1/describe` | Module self-description | **Yes** |
| `quote` | `POST /v1/quote` | Pre-flight cost and/or feasibility | **Yes** |
| `run` | `POST /v1/run` | Accept work, return immediately | No |
| `report` | `GET /v1/report` | Poll state | **Yes** |

**`run` MUST be async-accept.** Return 202 with a handle; never block on the work. Duplicate submission of
the same unit returns 200 with the existing handle and its current state — never an error, never a second
unit of work.

**`report` MUST support both a single-unit and an aggregate mode.** The senders' shape: `job_id` /
`job_item_id` for one unit, `campaign_id` for the aggregate, with `campaign_id` taking precedence if both are
sent. An aggregate query for a foreign or not-yet-started campaign returns a **clean empty aggregate**
(`total: 0`), not 404 — because the campaign id is platform-minted and "no rows" is the honest answer.

**Extension routes** are permitted and encouraged under `/v1/<module>/*` — M3's 17 `/v1/byosmtp/*` routes are
correct by design. **REQUIRED of any extension surface:** it is generated from the same in-code registry that
`describe` renders, so a route cannot exist without being described and cannot be described without existing.
M3's `extensionRoutes()` slice is the reference implementation and the best pattern found in any of the three
repos. Extension routes MUST NOT include operator routes — enumerating the admin surface to a platform token
is a free map of it.

### 1.6 `quote` — the clause that must be written carefully, because it is the one that broke

**This is the only route where a real module divergence blocks a shared client today (D4).** M2's request is
`{recipient_count}`; M3's is `{recipient_count, user_id}`; both decode with `DisallowUnknownFields`, so each
rejects the other's body. There is no single body both accept.

**REQUIRED request:**

```json
{ "recipient_count": <int >= 0>, "user_id": "<uuid>" }
```

- `recipient_count` REQUIRED, `>= 0`, zero valid.
- **`user_id` REQUIRED.** M3's position is right: a feasibility answer is meaningless without a tenant, and
  a module that does not need it can accept and ignore it. Requiring it costs a cost-only module nothing and
  is the only choice that admits a module that needs it.

**REQUIRED response — the cost half, always present:**

```json
{ "recipient_count": int, "cost": int, "currency": "credits",
  "unit_price_credits": int, "tier": "<string>" }
```

A module with no per-unit charge reports `unit_price_credits: 0` and `cost: 0`. M3 does exactly this and
retains the fields for compatibility — the right call, and the contract should bless it explicitly so it does
not read as an oversight.

**BLESSED, with the gap D22 identified closed (fix-plan 5b, landed 2026-08-04).** Reporting `0` is correct;
leaving the caller to *infer* what the `0` means is not. A structurally-zero cost and a broken price lookup
are byte-identical on the wire, and both return `200` — so a module whose cost half is inert **MUST declare
it** on `describe` as `pricing.cost_fields_inert` (bool). M3 now does. **The declaration MUST be derived from
the live unit price, never a literal:** `unit_price_credits` is live-retunable in both senders, so a
hardcoded `true` becomes false-and-costly the first time an operator retunes — a client told the cost half is
inert while the module charges. A client rendering a price MUST branch on `cost_fields_inert`, not on
`cost == 0`.

**OPTIONAL response — the feasibility half.** M3's block, verbatim in shape, is the reference:
`feasible` (bool), `reasons[{code, limit, requested, cap_name}]`, `caps{}`, `capacity{}`,
`warnings[{code, …}]`. A module that cannot assess feasibility omits the whole block; it MUST NOT report
`feasible: true` it cannot substantiate — never invent an outcome.

**`feasible` MUST mean "can this be accepted as submitted", NEVER "will this be fast."** Work that exceeds
today's capacity but drains over days is **feasible**. M3 states this explicitly and it is the kind of clause
that gets silently reinterpreted without one.

**REQUIRED of `quote` and `describe` both:** strictly side-effect-free. M2 proves this with a paired control
(`cost == 250` **and** zero jobs created after the call). That control is part of the contract, not optional
polish.

### 1.7 `describe` — what a UI is entitled to without asking a question

`describe` is the anti-special-casing verb: everything a client would otherwise hardcode per module belongs
here.

**REQUIRED today (both senders implement all of these):**

```json
{ "module": "<id>", "api_version": "v1",
  "capabilities": { "<name>": <bool>, … },
  "pricing": { "currency": "credits", "model": "<enum>",
               "unit_price_credits": int, "tiers": [{id, unit_price_credits, description}] } }
```

**`pricing.model` MUST come from a declared value set.** Today M2 reports `flat_per_recipient` and M3
`flat_subscription` with **no enum declaring what values exist** — so a client switching on it must already
know both. Declared set: `flat_per_recipient`, `flat_subscription`, `free`. Additive-only.

**REQUIRED additions (neither implemented anywhere — these close the UI-readiness gaps in Part 3):**

1. **`vocabularies{}`** — every enum the module's responses can contain, as declared value lists
   (`job_state`, `error_class`, verdict `status`/`reason`/`recommendation`, quote `reasons[].code`, …).
   **This is the single highest-value addition in the contract.** All three modules today force a UI to
   hardcode their vocabularies from documentation, which is per-module special-casing by definition.
2. **`error_codes[]`** — the codes this module can emit, so a client can verify its handling is complete
   rather than discovering 422 in production (the D-list divergence).
3. **`extension_routes[]`** — M3's shape, generated from the mount registry.
4. **`schemas{}`** — M3 already returns this.

**Also REQUIRED of `describe`: honesty.** M2's comment — *"no sequences, no open/click tracking"*, with
`describeTiers` shared with `quote` so the two "can never disagree" — is the right instinct. A capability
reported true that is not implemented is worse than one reported false.

### 1.8 Idempotency and correlation

**REQUIRED:**

1. **Every unit of work has a caller-supplied idempotency key**, and re-submitting it returns the original
   handle rather than creating a second unit. The senders use `job_item_id` with a `UNIQUE` constraint
   (`byosmtp_0007_create_jobs.up.sql:33`) — a database constraint, not an application check, so it holds
   under concurrency. **[weak on the mechanism — M2↔M3 inheritance — but the DB-constraint choice is right
   and should be locked.]**
2. **Every unit carries an opaque platform correlation string the module echoes and never interprets.** M1's
   `caller_ref` is the reference, and its documentation is exactly right: *"an OPAQUE correlation string the
   brain echoes but never interprets — NOT a user id; the platform maps its users↔caller_ref"*
   (`api.go:187-188`). **[strong — M1 and the senders both separate module work-identity from platform
   job-identity.]**
3. **Never fabricate an outcome.** If the worker did not confirm a result, the module does not claim one.

**A caution the audit turned up:** M1's `caller_ref` design is sound *and* is the premise that produced D1.
Its `jobs` table comment reasons "the platform owns the user-facing job, so the module needs no user id" —
correct about `caller_ref`, wrong as a reason to omit the *authenticated* caller from the data. **Clause 1.4.6
and this clause are independent: an opaque platform correlation string does not substitute for a tenant
predicate.** Stating both together, because conflating them is how the defect happened.

### 1.9 Health, readiness, observability

**REQUIRED — one shape across every module:**

```
GET /healthz  → 200 {"status":"ok","at":"<rfc3339>"}                       (liveness; unauthenticated)
GET /readyz   → 200|503 {"status":"ready"|"not_ready","checks":{"<name>":bool},"at":"<rfc3339>"}
```

1. **`/readyz` MUST check every dependency required to serve** — at minimum the database. **M1's readiness
   checks only that a secret loaded**, so it can report `ready: true` while every request 503s. Readiness
   that does not track serving capability is worse than no readiness endpoint, because orchestrators act on
   it.
2. **No two modules currently share a single `/readyz` field name.** M1 returns
   `{ready, config, pepper, supabaseKey}`; the senders `{status, db, singleton_lock, at}`. The `checks{}` map
   above is proposed precisely so per-module dependency names live inside a uniform envelope.
3. **`GET /metrics` in Prometheus text format is REQUIRED.** **No module has one.** Two modules
   independently computed observability data a consumer cannot reach — M1's `internal/metrics` (block rate,
   unknown rate, cache hit rate, latency p50/95/99, no HTTP exposure, no caller) and M3's `notifications`
   table (write-only, no reader on any route). The data exists in both; only the exposure is missing.
4. **Every log line carries the `request_id`** matching §1.3. M1 logs none.
5. **Any user-actionable event a module records MUST be readable over the platform surface.** M3 records
   notifications and exposes no route to fetch them, contradicting its own stated principle that users are
   notified about everything actionable.

### 1.10 Data-plane properties **[strong — all three independently agree]**

These are the audit's most reassuring convergences. Lock them permanently.

1. **A dedicated, named schema per module.** Never bare `public`. M1 violates this — 22 unqualified tables
   including `jobs`, in the same namespace the platform owns and the senders FK into. In a shared database
   `public.jobs` and `public.users` are names a platform is likely to want.
2. **A non-login owner role, distinct from a login runtime role.** All three do this.
3. **Per-table enumerated grants. Never blanket default privileges** — they key on the creator and silently
   do not fire for a table created by an admin and then reowned.
4. **Append-only tables enforced by grant revocation** (`REVOKE UPDATE, DELETE`), not by trigger and not by
   application discipline. All three do this.
5. **The worker holds no database credential and no key-management credential — enforced structurally by
   shipping no driver and no SDK**, not by configuration. **This is the strongest agreement in the audit**
   (M1's Ruby node and M2's Node box arrived at it independently) and it should be treated as inviolable.
6. **Grant and role posture MUST be tested as the least-privileged role.** M3's harness connects as
   `byosmtp_module` and sets that password out-of-band specifically so role-isolation tests can run. **M1's
   equivalent tests skip whenever connected privileged, and its CI connects as superuser — so they always
   skip, while the CI comment claims append-only is proven on every push.** A test that cannot assert is not
   evidence, and one that is documented as evidence is a liability.

### 1.11 Operational safety

**REQUIRED:**

1. **A graduated kill-switch ladder for the primary work path, not a binary halt.** At minimum: serving →
   accept-but-defer → refuse-new-but-drain-in-flight → fully stopped. **No module has a complete one.** M1
   declares four rungs but `accept_and_defer` has no consumer, so it validates and then behaves as
   `serving` — a silently inert safety control, worse than an absent one because it will be trusted. The
   senders have only a binary `dispatch_enabled`, so a partial incident forces a full stop for every tenant.
2. **A halt MUST be clean, lossless, and resumable.** No work dropped, no double-send on resume.
3. **Every autonomous mutator ships with a mode dial defaulting to shadow/recommend-only.** M1 does this for
   four mutators from day one and is unambiguously ahead; the senders retrofitted one. **Adopt M1's default
   wholesale.**
4. **An illegal switch value is REJECTED at load, never clamped or auto-corrected** — except where the safe
   direction is unambiguous, in which case fail toward the safer rung. The senders' `autoquarantine_mode`
   falling back to `shadow` and never `enforce` on an unrecognised value is the right instinct.
5. **A dial that validates MUST have a consumer.** The senders enforce this with a blocking
   `config_consumption` gate; M1 has no such gate and has exactly the defect the gate exists to catch.
6. **Config governing runtime behaviour SHOULD be hot-retunable** (DB rows) rather than requiring a restart
   (env vars). Marked SHOULD, not MUST: env-var config is a real operational cost during an incident but
   does not break the contract.

### 1.12 Deployment **[the clause M1 fails outright]**

**REQUIRED:** every module ships a container definition and a compose file (or equivalent manifest) that
stands up the module and its dependencies locally, and that is the same definition CI uses.

**M1 has no Dockerfile and no compose file for its Go control-plane** — the deployable half of the module.
Only the Ruby verifier-node is containerised. CI builds the control-plane with `go build ./...` and never
packages it. This is the one place the brief's "is each module independently deployable and operable"
question returns a plain no.

---

## Part 2 — THE PLATFORM ASSUMPTIONS REGISTER

Every assumption a module makes about the platform, found in code. For each: **which module assumes it**,
**what breaks if it is wrong**, and **how expensive the fix is**. These are the questions the platform team
must answer; the modules cannot answer them for themselves.

### A1 — `public.users` exists with `(id uuid pk, email, is_active, created_at)`

- **Assumed by:** M2, M3 (identical). **Not** M1.
- **In code:** `GRANT SELECT ON public.users TO {sender,byosmtp}_module`
  (`*_0000_bootstrap.up.sql:49`). No module migration creates the table. Shape recorded only as a comment in
  `db/preflight/check_platform_tables.sql`, which **checks existence only — deliberately not shape**,
  because a shape check would break whenever the platform legitimately adds a column.
- **If wrong:** both senders fail to migrate. The preflight fails loudly with
  *"Deploy the platform schema first"* — so a missing table is caught. **A renamed or retyped `id` column is
  NOT caught** and breaks seven foreign keys plus every runtime query.
- **Fix cost if wrong:** LOW if caught at deploy (preflight fires). **HIGH if the platform renames the column
  later** — seven FK definitions across migrations 0004/0005/0006/0008/0009/0014/0027 in each of two repos,
  each needing a new migration.
- **Platform must confirm:** the exact column name and type of the user primary key, and that it will never
  be renamed.

### A2 — `public.job_items` exists with seven specific columns

- **Assumed by:** M2, M3. **Not** M1.
- **In code:** M3 reads `ji.id, ji.campaign_id, ji.user_id, ji.recipient_email, ji.rendered_subject,
  ji.rendered_body, ji.reply_to` (`platformapi/store.go:145-152`). One FK to `public.job_items(id)`
  (`*_0007:45`). The preflight comments the assumed shape and does not verify it.
- **If wrong:** every `/v1/run` fails at runtime with an internal error. **The preflight will not catch it** —
  it checks existence, and the table would exist.
- **Fix cost if wrong:** MEDIUM per column (one query change plus a migration if the FK is involved), in two
  repos.
- **Platform must confirm:** that these seven columns are stable, and specifically that **the message content
  is already rendered** when a job item is created. The senders read `rendered_subject`/`rendered_body` and
  never template anything — if the platform expects the module to render, nothing works.

### A3 — The platform owns the user-facing job; the module owns only the work

- **Assumed by:** **all three, independently** — the strongest agreement in the register.
- **In code:** M1's `jobs` table comment: *"the BRAIN's notion of a bulk verification job (NOT the
  platform's user-facing job). Holds verification-work state ONLY: counts, status, policy, and an OPAQUE
  caller_ref the brain never interprets. NO user id, credits, CSV, filename, or per-row UI state."* The
  senders mirror it: one module job per platform `job_item`, module state only.
- **If wrong** (i.e. the platform expects modules to hold user-facing job state — filenames, CSV, per-row UI
  state): all three modules are wrong together, and the platform needs a job-tracking layer it does not have.
- **Fix cost if wrong:** HIGH, but it is a **platform-side** build, not a module change. The modules'
  position is defensible and consistent.
- **Note the trap:** in M1 this correct premise was used to justify omitting the *authenticated caller* too,
  which is D1. The premise is right; that particular inference from it was not.

### A4 — The platform enforces quotas, credits, and billing. **No module does.**

- **CORRECTED 2026-08-04 (report §0b.5). The original claim was two-thirds wrong.** The **credit-balance**
  half stands: no module has a wallet, ledger of user credits, balance column, credit deduction,
  affordability pre-check, or 402 path — verified by grep across all Go/SQL/Ruby. But "no quota enforcement
  or billing logic **anywhere**" is false on three counts:
  - **M2 has live, non-zero per-recipient pricing** — `cost = recipient_count × unit_price_credits`, read
    fresh from `sender.config`, seeded `{"v":1}`, admin-retunable, range-validated 0..1000. M3's own
    `byosmtp_0048` migration calls it *"real, non-zero, per-recipient billing"* and reseeds it to **0**.
  - **M1 enforces a money-denominated spend quota** — `external/breaker.go:102-117` refuses vendor calls on
    per-job/daily/monthly caps and a rate ceiling, over durable spend summed off the append-only
    `external_calls.cost_units` ledger, wired into the live escalation path and **failing closed**. Caveat:
    all four caps default UNSET and `buildExternal` returns nil (PENDING-REAL-VENDOR), so nothing egresses
    yet.
  - **M3 enforces per-user volume quotas that function as the overspend backstop** — `platformapi/store.go
    :117-128` rejects the (cap+1)th `/v1/run` with 409 `max_campaign_size_reached` /
    `max_queued_jobs_reached` (50 000 / 100 000, per-user admin overrides); accounts capped at 20;
    `daily_limit <= 100_000` backed by an un-bypassable CHECK. `byosmtp_0042:37` states the purpose verbatim:
    *"bounding runaway/abuse volume under flat pricing."*
- **The corrected exposure is sharper than the original, and inverted:** the "unbounded overspend with no
  module-side backstop" charge holds **for M2 only** — and M2 is precisely the module with **non-zero pricing
  and no volume caps at all** (no campaign-size, queued-jobs, or account-count cap; D28). **The two senders
  have the caps and the pricing on opposite sides.** That is the finding the platform should act on.
- **In code:** the senders' `campaign_report.go:10` names the ownership explicitly — a failed job is
  *"terminal, true pool exhaustion — refundable, **platform's policy**"*. `eligibility.go:13` states daily
  headroom *"is spent the moment it is consumed, even if the send later fails/requeues/bounces — refunding a
  … is deferred"*.
- **If wrong** (i.e. the platform expects the module to enforce): **a user can exceed their credit balance
  without limit on any of the three modules.** There is no enforcement point to fall back on.
- **Fix cost if wrong:** HIGH in all three — a new pre-flight check on the write path in each, plus a
  reconciliation story.
- **Platform must confirm:** that it debits before calling `run`, and its refund policy for
  `failed`/`bounced`. **The modules have deliberately left refunds to the platform and say so in code** —
  this is the single most explicitly-flagged assumption in the register, and the one most likely to be
  missed because it is stated in comments rather than in an API.

### A5 — Tenant identity is passed in the request, not derived from the credential

- **Assumed by:** M2, M3 (`user_id` in body/query, *"never derived from the auth credential — caller-supplied
  (OD-25)"*). **M1 contradicts it** — it has per-caller credentials and could derive identity, and instead
  derives nothing.
- **If wrong** (i.e. the platform expects the module to derive the tenant from the key): the senders would
  need per-tenant credentials, which they do not have — one flat token set, no caller identity at all.
- **Fix cost if wrong:** HIGH for the senders (a whole credential model), LOW-MEDIUM for M1 (the identity
  already exists; it just is not used).
- **Platform must confirm:** that it is trusted to pass a correct `user_id` — because **the senders' entire
  tenancy model rests on that trust.** A compromised platform token can read any tenant's data on either
  sender by supplying any `user_id`. That is an accepted and documented design (the platform is inside the
  trust boundary), but it should be an accepted decision rather than a discovered one.

### A6 — Two incompatible tenant identity spaces coexist

- **M2, M3:** platform `public.users.id` (UUID), stored on every module row.
- **M1:** its own `api_callers.caller_id` (text), stored on **no** module row.
- **If unresolved:** no cross-module per-user view is possible without a mapping layer, and any
  platform-level "show me everything for this user" query spans two identity spaces.
- **Fix cost:** MEDIUM for M1 to adopt `users.id` (a column, a backfill, and predicates on three read paths
  — which is the same work as fixing D1, so the two should be done together). LOW for the platform to
  maintain a mapping table instead, but that leaves D1 unfixed, so it is not an alternative to the module
  change.
- **Platform must decide.** This one cannot be deferred: it is a precondition for either fix.

### A7 — Async-run + polled-report is how the platform drives work

- **Assumed by:** all three. Every module accepts work, returns a handle, and expects polling. **No module
  has a webhook, callback, or push mechanism to the platform.** (M2/M3 have an operator alert webhook, which
  is outbound egress to an operator endpoint and explicitly not a platform callback.)
- **If wrong** (i.e. the platform wants push notification on completion): all three need a new outbound
  mechanism.
- **Fix cost if wrong:** MEDIUM per module — but note this is *exactly* the shape of the existing alert
  webhook in both senders, so they have a pattern to extend. M1 has none.
- **Platform must confirm:** that it will poll. **Register this as the assumption most likely to be wrong**,
  because the brief itself raised it: for a UI showing live campaign progress, polling three modules with
  three different report shapes is the expensive path, and every module has assumed it without asking.

### A8 — Deleting a platform user has module-side consequences the modules chose

- **Assumed by:** M2, M3.
- **In code:** **six FKs `ON DELETE CASCADE`** into module data, and **one `ON DELETE RESTRICT`**
  (`*_0027`, on `accounts`). So a `DELETE FROM public.users` **silently destroys module history through six
  paths and is refused outright by the seventh**.
- **If wrong:** either the platform cannot delete users at all (the `RESTRICT` blocks it), or it deletes them
  and loses module audit history it expected to retain.
- **Fix cost:** LOW-MEDIUM (migrations changing FK actions), but the *policy* question is the hard part.
- **Platform must confirm:** its user-deletion semantics — hard delete, soft delete, or anonymise. **Today
  the answer is dictated by module migrations, inconsistently between them, and neither module chose it as
  a platform-wide policy.**

### A9 — The `currency` unit is an abstract integer credit, not money

- **Assumed by:** M2, M3 — hardcoded literal `"credits"`, integers only, **no decimal handling anywhere**.
- **If wrong** (real money crosses the boundary): a contract change in both senders, plus a decimal
  representation decision.
- **Fix cost:** MEDIUM, and it is an **additive contract change** (a new currency field or a versioned
  route), not a rename — Rule-5 additive-only forbids repurposing `currency`.
- **Platform must confirm:** that credits stay abstract, or plan the additive change now.

### A10 — Operator actions are invisible to the platform

- **Assumed by:** M2, M3 (21 admin routes each, mutating the same rows the platform reads).
- **In code:** an operator pause/quarantine/disable changes state a platform client is concurrently polling,
  and **the platform learns only by re-polling.** No notification channel exists. M3 records notifications
  and exposes no route to read them.
- **If wrong** (the platform expects to be told): needs A7's push mechanism plus a read route for M3's
  notifications.
- **Fix cost:** LOW for the notifications read route (the data is already there); MEDIUM for push.
- **Platform must confirm:** that polling is sufficient to observe operator-initiated state changes.

### Register summary

| # | Assumption | Modules | Breaks if wrong | Fix cost |
|---|---|---|---|---|
| A1 | `public.users(id uuid pk, …)` | M2, M3 | Migration fails (caught) / FK break on rename (not caught) | LOW–**HIGH** |
| A2 | `public.job_items` 7 columns, **pre-rendered** | M2, M3 | Every `run` fails at runtime; preflight misses it | MEDIUM |
| A3 | Platform owns the user-facing job | **all three** | Platform needs a job layer it lacks | HIGH (platform-side) |
| A4 | **Platform enforces credits/quotas/refunds** | **all three** | **Unlimited overspend; no fallback** | HIGH |
| A5 | Tenant passed in request, not derived | M2, M3 (M1 contradicts) | Senders need a credential model | HIGH / LOW-MED |
| A6 | **Two tenant identity spaces** | M1 vs senders | No cross-module per-user view | MEDIUM |
| A7 | **Async-run + polled-report** | **all three** | All need outbound push | MEDIUM each |
| A8 | User deletion cascades (6) / restricts (1) | M2, M3 | History lost, or deletion blocked | LOW-MED + policy |
| A9 | `credits` are abstract integers | M2, M3 | Additive contract change in both | MEDIUM |
| A10 | Operator actions invisible to platform | M2, M3 | Stale platform view | LOW–MEDIUM |

**The three to answer first, because everything else can wait:** **A4** (nothing enforces credits anywhere —
an unbounded exposure with no module-side fallback), **A7** (polling is assumed by all three and is the one
the brief itself doubted), and **A6** (a precondition for fixing D1 at all).

---

## Part 3 — THE UI-READINESS ASSESSMENT

**Test of done, as the brief set it:** could a competent UI team build against all three modules **without
asking a single question**?

## VERDICT: NO — and not close.

Below is every question a UI team would be forced to ask, derived from what the code does and does not
expose. Each is a real blocker, not a preference.

### Questions forced by missing self-description

1. **"What are the possible values of `state`?"** No module exposes its vocabulary programmatically. A UI
   rendering job states must hardcode `queued|assigned|sending|sent|failed|bounced` from documentation, and
   M1's `valid|invalid|risky|unknown` plus ~30 reason values separately. **Applies to all three.**
2. **"What error codes can I receive?"** M2 emits five, M3 seven, M1 none at all (free-text prose). A UI
   cannot enumerate what it must handle. It will discover 422 and 429 in production.
3. **"What does `pricing.model` mean, and what values exist?"** `flat_per_recipient` vs `flat_subscription`
   with no declared set. A UI showing pricing must special-case both strings it happens to have seen.
4. **"Which of these capabilities are real?"** `capabilities{}` is a flat bool map with no schema and no
   versioning; two modules report identical keys, M1 reports none.

### Questions forced by M1's error surface

5. **"How do I tell 'unknown policy' from 'internal failure'?"** Both arrive as `{"error":"<english>"}` with
   no code. The UI must string-match prose that no contract freezes — **any wording change in M1 is a silent
   breaking change for every client.**
6. **"What do I show the user when M1 fails, and what do I put in a support ticket?"** No `request_id` in
   any error body **and none in any log line** — so a user-reported M1 failure is correlatable to nothing.
   This is the most operationally expensive small gap in the audit.
7. **"Why does the 413 have a different shape?"** `{error, max, got}` on one route only.

### Questions forced by the tenancy model

8. **"How do I scope a request to a user?"** Three different answers: senders take a `user_id` UUID; M1
   takes nothing and returns any caller's data. A UI cannot write one "fetch this user's jobs" function.
9. **"Is a 404 'gone' or 'not yours'?"** Deliberately indistinguishable on the senders (correct — no
   existence disclosure). A UI must not offer "this may belong to someone else" as a message. Worth a
   contract note, since the instinct is to add exactly that.
10. **"Why can I see other users' verification results?"** The question a UI team would ask on day one of
    integrating M1, and the answer is D1.

### Questions forced by report-shape divergence

11. **"How do I render progress?"** M1: `{counts{total,settled,pending}}` polled from
    `/v1/jobs/{id}`. Senders: `{totals{7 fields}, progress{terminal, fraction_complete, is_complete}}` from
    `/v1/report?campaign_id=`. Two entirely different progress models — and only the senders supply
    `fraction_complete`, so for M1 the UI must compute it.
12. **"Why is `smtp_response` `null` here and absent there?"** Within M3 alone, the same five logical fields
    are explicit `null` in the campaign shape and **omitted** in the single-job shape. M3's code documents
    this as deliberate; the UI still needs two null-handling paths for one module.
13. **"Where do I get the notification list?"** M3 records user-actionable notifications and **exposes no
    route to read them.** A UI cannot build the notification surface at all — the data exists and is
    unreachable.
14. **"Why is `email` empty in bulk results?"** M1 purges plaintext on settle by design (a good privacy
    property) — but the sync and bulk shapes differ silently, and nothing in the response says why.

### Questions forced by cost and pre-flight

15. **"What will this cost before I submit it?"** Answerable for the senders, **unanswerable for M1** — it
    has no cost verb, field, or dry-run. A UI cannot show a confirmation screen with a price.
16. **"Can this even run?"** Only M3 answers (`feasible` + `reasons[]`). M2 has no feasibility notion; M1 has
    neither.
17. **"Which `quote` body do I send?"** The two senders reject each other's body (D4). A UI needs
    per-module request construction for a route that is supposed to be uniform.

### Questions forced by observability

18. **"Is this module up?"** Three different `/readyz` bodies with **not one shared field name.** Three
    parsers for one dashboard.
19. **"Is M1 actually able to serve?"** `ready: true` can coexist with every request 503ing, because
    readiness checks only that a secret loaded.

### What a UI *can* do today without asking

Stated for fairness, because the negative verdict is not the whole picture:

- Submit work and poll to completion on **all three**, once per-module code exists.
- Full account self-service on M3 (17 well-described extension routes, generated from the mount registry —
  a UI can enumerate this surface programmatically, and it is the **only** surface in any module where that
  is true).
- Trust that no error body leaks credentials, SQL, or internal reasons on any of the three.
- Trust tenant isolation on the senders — with byte-identical 404s and behavioural proof.
- Render M1's verdicts with real confidence: its vocabulary is frozen, DB-mirrored, composability-checked
  (`invalid ⇒ skip` only, enforced in code), and later phases' values are pre-reserved so additions are
  additive by construction. **It is the best-designed vocabulary of the three — it is simply not
  discoverable at runtime.**

### The shortest path to a "yes"

Ordered by questions-closed per unit of work:

| Step | Closes | Cost |
|---|---|---|
| Add `vocabularies{}`, `error_codes[]`, declared `pricing.model` set to `describe` | Q1, Q2, Q3, Q4 | **LOW** — all three modules already know these values internally |
| Give M1 the coded error envelope with `request_id` (§1.3) | Q5, Q6, Q7 | MEDIUM |
| Fix M1 tenant scoping (D1) | Q8, Q10 | MEDIUM |
| Align `quote` request (D4) | Q17 | **LOW** — one optional field in M2 |
| Add M3's notifications read route | Q13 | **LOW** — data already exists |
| Uniform `/readyz` with a `checks{}` map; make M1's readiness check the DB | Q18, Q19 | LOW |
| Give M1 a cost/feasibility verb | Q15, Q16 | MEDIUM-HIGH |
| Uniform progress shape, or a platform-side adapter | Q11 | MEDIUM |

**Four of the eight steps are LOW cost and together close nine of the nineteen questions.** The `describe`
enrichment is the standout: every module already knows its own vocabularies, error codes, and pricing model
internally — the work is exposing what exists, not deciding anything new. **That is the single
highest-leverage change in this document**, and it is the one that most directly attacks per-module
special-casing, since every question it closes is one a UI would otherwise answer by hardcoding.
