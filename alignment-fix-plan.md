# The Ordered Alignment Fix Plan

**Companion to** `module-comparison-report.md` (the audit and its evidence) and
`canonical-module-contract.md` (the target state).

> ## THIS PLAN HAS NOW BEEN PERFORMED — 17 of 21 items DONE, 1 partial, 3 deferred
>
> **Superseded 2026-08-07.** This banner previously read *"THIS PLAN HAS NOT BEEN PERFORMED — every item
> below is a RECOMMENDATION."* That was true when the audit closed on 2026-08-04 and became false over the
> four execution chunks that followed. It is replaced rather than deleted because **the stale version is the
> exact hazard this repo documents**: a confident claim, prominently placed, that a reader has no reason to
> doubt and every reason to act on. It survived four passes of work that falsified it.
>
> **Per-item status now lives in two places:** a `STATUS:` line directly under each item heading, and the
> reconciled [Ordered summary](#ordered-summary) table at the end. Both were established from the three
> module repos' commit histories and the trees themselves, not from this document's own prior text.
>
> What has **not** changed: the *recommendations* below are the originals, including the ones later found to
> rest on a wrong premise (items 3/15's shared-work discount, D10's site count). They are left as written so
> the reasoning can be audited against what actually shipped. Where a recommendation was overtaken, the
> STATUS line says so.

**Date:** 2026-08-04. **Reconciled against the shipped repos:** 2026-08-07.

### How to read this

- **BLOCKS-PLATFORM-INTEGRATION** — a single platform integration cannot drive all three modules until this
  is done. **Marked in bold on the item.**
- **Cost** — LOW (hours), MEDIUM (days), HIGH (a phase of work).
- **Risk if skipped** — what actually goes wrong, not a severity word.
- Every item cites the finding ID from the audit (D1–D16) or the contract clause (§) it satisfies.

### The two decision points this plan does not make for you

1. **Whether M1 gets a second platform surface at all** (Tier 4). Everything up to Tier 3 is worth doing
   regardless; Tier 4 is a phase of work whose only justification is "one integration instead of two."
2. **The A4/A6/A7 assumptions** (`canonical-module-contract.md` Part 2). **A6 — which tenant identity space
   M1 uses — is a hard precondition for item 3 below**, the highest-priority item in the plan. Answer it
   first or item 3 stalls.

---

## TIER 0 — Do these first: they are wrong today, independent of any alignment goal

These four are defects with no upside. None depends on a platform decision. None is contract work. **Tier 0
is worth doing even if the platform decides never to unify the three modules.**

### 1. M1: scope job reads to the authenticated caller — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — `29e6204` (M1).** The follow-up `88b6dd0` fixed a cross-package test-isolation defect that commit introduced.

- **Finding:** D1. Contract §1.4.
- **Severity:** **MEDIUM as shipped, HIGH the moment a second key is issued** (revised 2026-08-04 by the
  refutation pass — report §0b.3; originally filed HIGH). Still **the only FAIL grade in the parity
  scorecard**, and still item 1, because the fix must land **before** multi-tenant key issuance, not after.
- **Three severity corrections, all verified first-hand:** the exposure is verdicts/domains/flags/counts but
  **not** email addresses (`api.go:444` sets `Email: ""`, *"purged on settle (Door 3)"*); job ids are **not
  enumerable** (74 `crypto/rand` bits at `idgen.go:35`), so an attacker must already know a victim `job_id`;
  and **no second untrusted caller exists yet** — `api_callers` writes are reserved to `cp_admin`
  (`0010_api_callers.sql:43`) with no issuance route or CLI anywhere in the tree.
- **Read this as a prerequisite gate, not a live breach.** The mechanism is airtight and unmitigated
  (`callerID` discarded at `api.go:251`, `:273`, `:313`; no owner column; all three store reads scope on
  `job_id` alone; RLS *deliberately* absent per `0005_grants.sql:112`) — it simply has no second tenant to
  exploit it yet, and `api_callers` structurally permits one.
- **What:** any authenticated caller can read any other caller's bulk job status and full verification
  results by `job_id`. Job ids are UUIDv7 (time-ordered, not random), so a caller who knows roughly when
  another submitted has a materially reduced search space.
- **Recommendation:** add an owner column to `jobs`; populate it from the authenticated `caller_id` at
  submit (`api.go:251` currently discards it — `_ = callerID`); add the predicate to all three read paths
  (`GetJob`, `JobCompletion`, `ListJobResults`, `postgres.go:801-892`). **Return 404, not 403** — identical
  to a nonexistent id, matching the senders' no-existence-disclosure posture.
- **Cost:** MEDIUM. One migration, one backfill decision for existing rows, three query predicates, one
  handler change.
- **Depends on:** **A6** — whether the owner column is `caller_id` (text, M1's own space) or platform
  `users.id` (UUID, the senders' space). **Do not start until this is answered**; picking wrong means doing
  the migration twice.
- **Risk if skipped:** wiring M1 into a multi-tenant platform exposes every caller's verification results —
  which are PII — to every other caller. **This blocks M1's platform integration on security grounds
  independent of contract shape.**
- **Note:** M1's `jobs` table comment says "NO user id" *by design*, on the premise that the platform owns
  the user-facing job. That premise is correct and this fix does not contradict it: an *owner* column is not
  user-facing job state, and the co-existing `api_callers` multi-caller auth already falsified the
  "single client" assumption the omission rested on. Expect to update that comment as part of the change.

### 2. M1: make the test suite able to catch item 1's regression

**STATUS: DONE — `29e6204` (M1).** The control is a real-Postgres cross-tenant test, not an assertion on mocked rows.

- **Finding:** §8.3, and the mechanism behind D1.
- **What:** M1's route-level tests inject `Verifier`/`CallerAuth`/`BulkSubmitter`/`JobReader` over an
  in-memory store. **With a fake store there is no SQL for a missing tenant predicate to be visible in** —
  so D1 is structurally invisible to M1's own suite, and item 1 could regress silently.
- **Recommendation:** add a real-Postgres cross-tenant test at the route level (M1 already has the
  infrastructure — `TM_TEST_DATABASE_URL` is set in CI and every migration is applied). Follow the senders'
  anti-vacuity control: assert the cross-tenant 404 is **byte-identical** to a nonexistent-id 404, and
  assert zero rows leak. Add M1's own `FAIL-VERIFY` comment naming the mutation that breaks it.
- **Cost:** LOW. The harness exists; this is one test file.
- **Risk if skipped:** item 1 lands and silently regresses later. **Do this in the same change as item 1**,
  not after — a fix whose regression cannot be caught is half a fix.

### 3. M1: connect grant-enforcement tests as `cp_runtime`, or delete the false claim

**STATUS: DONE — `29e6204` (M1), via `SET LOCAL ROLE` from the existing session.** It therefore did **not** require a connectable `cp_runtime`, which retires the "shared work with item 15" premise recorded below — see [`deferrals-and-false-greens.md`](deferrals-and-false-greens.md).

- **Finding:** D13. Contract §1.10.6.
- **Severity:** MEDIUM — false assurance, which is the dangerous kind.
- **What:** `TestPGSendOutcome_RuntimeCannotDelete` and `TestPGRateBudget_DeleteDeniedForRuntime` skip
  whenever the `DELETE` succeeds ("connected as a privileged role"). CI connects as superuser
  `postgres://postgres:ci@…` (`ci.yml:55-58`), and `cp_runtime` is `NOLOGIN` and **never referenced anywhere
  in `ci.yml`** — so both tests **always skip**. Meanwhile `ci.yml:23-25` claims the real Postgres exists so
  append-only tests *"actually RUN in CI … proving append-only on every push."*
- **Recommendation:** follow M3's pattern (`tests/harness/run.sh:77-85`,
  `platformapi/harness_test.go:49`) — give `cp_runtime` a login and a CI password out-of-band, run
  migrations as superuser, run the grant tests as `cp_runtime`. **If that is not wanted, delete the claim
  from `ci.yml` instead.** Either is fine; the current state is the only unacceptable one.
- **Cost:** LOW.
- **Risk if skipped:** M1's append-only posture is documented as proven and is not. The same failure mode as
  D2: a control that will be trusted because something says it works.

### 4. M1: wire `accept_and_defer`, or reject it at load

**STATUS: DONE — `29e6204` (M1), wired (not rejected).** Control: `control-plane/internal/api/frontdoor_defer_test.go`.

- **Finding:** D2. Contract §1.11.1, §1.11.5.
- **Severity:** MEDIUM — safety.
- **What:** `FrontDoorDefer = "accept_and_defer"` (`config.go:59`) passes `Validate()` (`config.go:750-753`)
  and has **no consumer**. Both readers of `KillSwitch.FrontDoor` (`api.go:144`, `api.go:223`) compare only
  to `FrontDoorReject`. So `TM_FRONTDOOR_MODE=accept_and_defer` validates and then behaves exactly like
  `serving`.
- **Recommendation:** either implement the deferring behaviour (accept and enqueue without dispatching) or
  remove the value from the accepted set so it is rejected at load. **A silently inert safety control is
  worse than an absent one, because an operator reaching for the middle rung during an incident gets full
  serving with no error and no warning.**
- **Cost:** LOW to reject; MEDIUM to implement.
- **Risk if skipped:** an operator believes they have degraded the front door and has not.

---

## TIER 1 — Unblock a single client for the two senders

**This tier is small, well-defined, and has a clear finish line.** After it, one platform client drives both
M2 and M3 — three of the four core routes are already uniform (`run` and `report` are character-identical;
`report` and `describe` responses are strictly additive).

### 5. M2: accept an optional `user_id` on `/v1/quote` — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — `ba9312a` (M2).** Accepted and **inert** in M2, which is the shape that lets one client body serve both senders.

- **Finding:** D4. Contract §1.6.
- **What:** M2's `quoteReq{RecipientCount}` and M3's `quoteReq{RecipientCount, UserID}` both decode with
  `DisallowUnknownFields()`. So `{recipient_count}` → M2 OK / M3 rejects; `{recipient_count, user_id}` → M3
  OK / M2 rejects. **No single body both accept.** ~~This is the one route blocking a shared client.~~
  **REVISED 2026-08-04:** still the cheapest single unblock, but **not the only blocker** — see item 5b.
  Report §7.1/§0b retract "exactly one route": `describe`'s changed `module`/`pricing.model` values and
  M3's always-zero `quote` cost also break a shared client, both **silently**.
- **Recommendation:** add `UserID string \`json:"user_id"\`` to M2's `quoteReq` and ignore it. Validate it as
  a UUID **if present** so a malformed value is not silently accepted. Do not make it required in M2 — that
  would break M2's existing callers, and additive-only forbids it.
- **Cost:** **LOW.** One struct field, one optional validation, one test.
- **Direction matters:** fix M2, not M3. M3's requirement is right (a feasibility answer is meaningless
  without a tenant) and removing it would break its feasibility block.
- **Risk if skipped:** the platform needs per-module `quote` request construction for a route that is
  supposed to be uniform — the exact special-casing this audit exists to eliminate.

### 5b. M3: resolve `quote`'s always-zero cost — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — ruled (b), then implemented in BOTH senders: `4abd8eb` (M3) + `0b42e1b` (M2).** `describe` now declares the cost fields inert rather than reporting a silent 0; `148f63f` (M3) had already zeroed the flat-subscription pricing. The M2 commit exists because the same ambiguity is present there with the **opposite** literal — a fix in one sender would have left the other misleading in the mirror direction.

- **Finding:** **D22** (added 2026-08-04 by the refutation pass; report §0b.7, §7.1).
- **Severity:** **higher than D4**, because it is silent. D4 returns a detectable 400; this returns **200 with
  a wrong number**.
- **What:** `byosmtp_0048_unit_price_zero.up.sql:23` reseeds **both** `value` and `default_value` to `{"v":0}`
  and `defaultQuoteUnitCredits = 0` (`handlers.go:25`), so `cost = recipient_count × 0` is **always 0**. M2's
  is seeded `{"v":1}` and admin-retunable, and M3's own migration comment calls M2's behaviour *"real,
  non-zero, per-recipient billing."* A platform cost-preview UI calling M3 displays 0 credits with no error.
- **Recommendation:** **this is an Open Decision, not a code fix — do not just change a number.** Two coherent
  answers: (a) if flat subscription means quote should not report a per-recipient cost, `cost`/
  `unit_price_credits` should be **absent or explicitly null** with a `pricing.model` a client can branch on,
  not silently 0; (b) if the field must stay for contract compatibility, `describe` must declare it inert.
  M3's own migration raises the deeper question: **a config key whose only consumer computes a
  guaranteed-zero product is arguably a Rule 1 "real consumer" violation.**
- **Cost:** LOW to implement once ruled; the ruling is the work.
- **Risk if skipped:** the platform shows users a wrong price for one of two sender modules, undetectably.

### 6. Declare `pricing.model`'s value set — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — `dde94e0` (M2), `3595117` (M3).**

- **Finding:** classification 7.4. Contract §1.7.
- **What:** M2 reports `flat_per_recipient`, M3 `flat_subscription`, with **no enum declaring what values
  exist.** A client switching on it must already know both.
- **Recommendation:** declare `{flat_per_recipient, flat_subscription, free}`, additive-only, and expose it
  in `describe` (item 8).
- **Cost:** LOW — documentation plus the `describe` field.
- **Risk if skipped:** a client cannot handle a third module's pricing model without a code change.

### 7. Contract clause: clients MUST tolerate unknown error codes

**STATUS: DONE — [`canonical-module-contract.md`](canonical-module-contract.md) §1.3.6 clause 6.** Additive-only, with a mapping rule for codes a client does not recognise.

- **Finding:** classification 2.5. Contract §1.3.6.
- **What:** M2 implements five error codes, M3 seven (`unprocessable` 422, `rate_limited` 429), at the same
  `/v1`. Additive within M3, divergent across modules.
- **Recommendation:** **this is a fix to the platform client, not to either module.** Neither sender is
  wrong. The client must map an unrecognised code to a generic failure by HTTP status.
- **Cost:** LOW.
- **Risk if skipped:** a client with an exhaustive switch over M2's set breaks on M3's 422/429.

### 7b. All three: route mux-level 405/404 through the error envelope

**STATUS: DONE — `e3fee99` + `de01752` (M2), `5924fbc` (M3), `29caf15` (M1).** In M1 the defect was found *by* item 14's route-drift gate, which is the one place in this plan where two items caught each other.

- **Finding:** **D17** (added 2026-08-04 by the one adversarial verification that completed — see report
  §0a.1). Contract §1.3.3.
- **What:** all three modules register Go 1.22 method+path patterns, so `net/http.ServeMux` answers a method
  mismatch or unmatched `/v1/*` path **before any handler or middleware runs**: `405` with
  `Content-Type: text/plain`, body `Method Not Allowed\n`, or `404 page not found\n`. **No `code`, no
  `request_id`, and unauthenticated.** A client that assumes `error.code` exists on every non-2xx
  **nil-derefs on a wrong-verb call.**
- **Why no one noticed:** the senders' `contract_drift.sh:79-88` probes each route with its
  method-appropriate verb only, so **no gate in any repo ever observes the 405 shape.** M1 has no equivalent
  gate at all.
- **Recommendation:** a catch-all (`mux.HandleFunc("/", …)` plus explicit per-path method fallbacks, or a
  middleware owning method dispatch) that emits the envelope with `not_found` or a new additive
  `method_not_allowed` code. **Add a gate probe that calls each route with a wrong verb** and asserts the
  envelope — otherwise this regresses invisibly, exactly as it arose.
- **Cost:** LOW per module. **Not BLOCKS-PLATFORM-INTEGRATION** — all three behave identically, so it forces
  no per-module special-casing. It is a client-robustness and contract-honesty fix, not an alignment one.
- **Risk if skipped:** every platform client needs a defensive non-JSON branch on every call, and the
  contract's "`request_id` is MANDATORY on every error" clause stays false in all three modules.
- **Note on the contract:** this is the one place where the canonical contract as written is **not satisfied
  by any module**, including the two that otherwise set the standard for it.

---

## TIER 2 — Close the UI-readiness gaps. Highest leverage per unit of work in the whole plan.

**Four LOW-cost items closing nine of the nineteen UI-blocking questions.** Every value involved is already
known internally by each module — this tier exposes what exists rather than deciding anything new.

### 8. All three: enrich `describe` with `vocabularies{}`, `error_codes[]`, declared `pricing.model` — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — `dde94e0` (M2), `3595117` (M3).** All three lists are **derived from the served surface**, never hand-maintained, so they cannot drift from it.

- **Finding:** UI questions 1–4. Contract §1.7.
- **What:** no module exposes its enum value sets or error codes programmatically, so a UI hardcodes three
  vocabularies from documentation — per-module special-casing by definition.
- **Recommendation:** add to `describe`: `vocabularies{}` (every enum a response can contain), `error_codes[]`
  (what this module can emit), and `pricing.model`'s declared set. M1 needs a `describe` route first
  (item 16), so for M1 this folds into that work.
- **Cost:** **LOW** for the senders (the values are compile-time constants and DB enums they already own);
  part of item 16 for M1.
- **Risk if skipped:** every UI hardcodes every vocabulary, and a module adding an enum value breaks clients
  silently — which additive-only versioning was supposed to prevent.
- **Why this is the standout item:** it is the cheapest change in the plan that most directly attacks the
  audit's central question. Every question it closes is one a UI would otherwise answer by hardcoding.

### 9. M3: add a notifications read route

**STATUS: DONE — `1d9a042` (M3).**

- **Finding:** D5, UI question 13. Contract §1.9.5.
- **What:** `notifications` (migrations 0033/0047) has two INSERT sites and is read only by
  `dispatch/notify.go` for webhook egress. **No platform or admin route reads it.** The module records
  user-actionable events a UI cannot fetch — contradicting M3's own stated principle that users are notified
  about everything actionable.
- **Recommendation:** `GET /v1/byosmtp/notifications?user_id=` in the extension registry (so `describe`
  declares it automatically), tenant-scoped like every other M3 read, paginated. Consider a
  mark-as-read/acknowledge companion, since `notification_state` already exists as an enum.
- **Cost:** **LOW** — the data, the table, the enums, and the tenancy pattern all already exist.
- **Risk if skipped:** the notification surface of a UI cannot be built at all for M3.

### 10. All three: uniform `/readyz`, and make M1's readiness check the database

**STATUS: DONE — `bcf6324` (M1), `9c74fa8` (M2), `1df98b0` (M3).** M1's readiness now measures the database; the senders answer in the platform envelope without breaking the monitor already reading them.

- **Finding:** classification 9.2/9.3, D-list. UI questions 18–19. Contract §1.9.1-2.
- **What:** **not one field name is shared** across the three readiness bodies. Worse, M1's checks only that
  the pepper secret loaded — not the DB — so it can report `ready: true` while `POST /v1/verify` 503s for a
  missing verifier or store (`api.go:157-160`).
- **Recommendation:** the contract's envelope —
  `{"status":"ready"|"not_ready","checks":{"<name>":bool},"at":"<rfc3339>"}` — so per-module dependency names
  live inside a uniform shape. **Add a DB ping to M1's readiness.** Add `at` to M1's `/healthz`.
- **Cost:** LOW per module.
- **Risk if skipped:** three health parsers for one dashboard, and M1's readiness gate does not reflect
  whether it can serve — which orchestrators act on.

### 11. M3: pick one null convention for the five widened report fields

**STATUS: DONE — `22b6c9d` (M3).** The finding on landing: the convention was **documented and not enforced**, so the divergence could have returned silently. It is now under test.

- **Finding:** classification 9.8, UI question 12.
- **What:** `account_id`, `account_username`, `smtp_response`, `error_class`, `failed_at` serialize as
  explicit `null` in the campaign shape and are **omitted** in the single-job shape. M3's own code documents
  this as deliberate (M3-P7-OD-4, `campaign_report.go:38-49`).
- **Recommendation:** **this is a real documented decision, so treat it as a decision to revisit, not a bug
  to fix.** The audit's only observation is that it costs a UI two null-handling paths for one module. If it
  stays, say so in the contract so a UI is not surprised. Additive-only means changing the campaign shape to
  omit keys is arguably a breaking change, so **the honest answer may be to document it rather than change
  it.**
- **Cost:** LOW either way.
- **Risk if skipped:** minor, and already documented in-repo. Lowest-priority item in Tier 2.

---

## TIER 3 — Operational parity. Real value, no platform decision required.

### 12. Senders: adopt a graduated kill-switch ladder for dispatch

**STATUS: DONE — `e939c4f` + `c4b2086` (M3), `5af51f8` (M2), `9741986` (M1's half of the shared `paused` code).** `c4b2086` is worth reading: the first assertion scanned the whole **file** and therefore proved nothing about the **function**.

- **Finding:** classification 13.1, §7.4. Contract §1.11.1.
- **What:** `dispatch_enabled` is binary — dispatch runs or stops entirely. During a partial incident (one
  provider degrading, a suspected bad config, one user's accounts misbehaving) the only available action
  halts every tenant.
- **Recommendation:** rungs — serving → accept-but-defer → refuse-new-but-drain-in-flight → stopped. As a
  config-table string enum with a fail-safe default, exactly like `autoquarantine_mode` already does.
- **Cost:** MEDIUM.
- **Cheaper than it looks:** the senders already have ladder discipline — `autoquarantine_mode`'s
  `off|shadow|enforce` with shadow-by-default and a fail-safe on unrecognised values. It was applied to
  autonomous account-scoring and not to dispatch. **This is a gap in coverage, not in understanding.**
- **Risk if skipped:** every incident response is all-or-nothing for every tenant.
- **And do not "copy M1":** M1's ladder declares four rungs and one is inert (item 4). M1 has the design
  without the wiring; the senders have wiring without the design.

### 13. Senders: shadow-mode-by-default for every autonomous mutator

**STATUS: DONE — `c300fe6` + `0e4d398` (M2), `ac70c38` (M3).** **Came in HIGH against this MEDIUM estimate** — four dials per sender, plus a sweep of every assertion that had assumed the mutators ran unconditionally. This overrun is what triggered the §15b re-estimate below.

- **Finding:** classification 13.3. Contract §1.11.3.
- **What:** M1 has four independent mutators all defaulting to `recommend_only`, with the recommend-only path
  proven to produce `Action{Apply:false, Recommend:…}` and zero live mutation. The senders retrofitted one
  shadow dial (`autoquarantine_mode`) late.
- **Recommendation:** audit every autonomous mutator in both senders; give each a mode dial defaulting to
  shadow. **Adopt M1's default wholesale** — this is the clearest case in the audit of M1 being ahead.
- **Cost:** MEDIUM.
- **Risk if skipped:** a new autonomous behaviour ships enforcing on day one, with no observation period.

### 14. Senders: adopt M1's in-code response-version assertion; M1: adopt the senders' route-drift gate

**STATUS: DONE — `0cacc36` + `29caf15` (M1), `80f3a69` (M2), `9988e53` (M3).** `api_version` is now derived from the served route table and the service refuses to boot without one. `80f3a69` also closed 15b's route-miscount row (D11).

- **Finding:** classification 4.2/4.3. Contract §1.1.3-4.
- **What:** each side has half the mechanism. M1 asserts `r.APIVersion == APIVersion` before every sync write
  and returns 500 rather than shipping an off-contract body. The senders gate served-set == documented-set in
  CI. **No module has both.**
- **Recommendation:** both directions. Note M1's own gap: `verdictFromRow` (`api.go:428-451`) coerces
  `api_version` via `firstNonEmpty(row.APIVersion, "v1")` and — unlike the sync path — **never calls
  `Valid()`**, so a stale stored row is silently coerced on the bulk-results path rather than caught.
- **Cost:** LOW-MEDIUM per module.
- **Risk if skipped:** contract drift goes undetected in the half each module lacks.

### 15. M1: package the control-plane

**STATUS: DEFERRED — nothing built.** `coldmail-verify` has no `Dockerfile` and no compose file; verified by inspection at the close of Chunk 4, not inferred. This is the one deferral genuinely blocked on missing infrastructure rather than on a ruling: **M1's request-path secret-leak scan cannot run until this compose stack exists.** Note that the "shares work with item 3" discount written above is **false** — item 3 shipped via `SET LOCAL ROLE` and never needed a connectable `cp_runtime`.

- **Finding:** D15. Contract §1.12.
- **What:** verified by `find` — the **only** Docker artifacts in the repo are under `verifier-node/`. There
  is **no Dockerfile and no compose file for the Go control-plane**, the deployable half of the module. No
  Terraform, no Helm, no manifest. CI builds it with `go build ./...` and never packages it.
- **Recommendation:** a Dockerfile for the control-plane and a compose file standing up Postgres +
  control-plane + verifier-node, and make CI use it — the senders' dev==CI loop. Include the migration/runtime
  role split.
- ~~item 3 needs `cp_runtime` to be connectable anyway, so these two items share work~~ — **THIS
  PREMISE IS FALSE, corrected 2026-08-07.** Item 3 shipped in `29e6204` using `SET LOCAL ROLE
  cp_runtime` inside the *existing* superuser test session (`control-plane/internal/store/pg_grants_test.go`,
  helper `asRuntime`), so `cp_runtime` never had to be a connectable login role at all. The two items
  share no work, and item 15's cost estimate must not be discounted against item 3. **Worse, the role
  split as written would make the item-3 control VACUOUS:** from a session already connected *as*
  `cp_runtime`, `SET LOCAL ROLE cp_runtime` is a no-op, so `TestPGGrants_RoleSwitchActuallyBites`
  would pass in both directions while proving nothing about a privilege drop. See the 15-C row below.
- **Cost:** MEDIUM.
- **Risk if skipped:** **M1 is not independently deployable** — the one place the brief's deployability
  question returns a plain no.

### 15b. Lower-priority parity items

**STATUS: PARTIAL — 5 of 16 rows closed, the rest DEFERRED.** Closed: `d481c97` (M1, Bearer scheme mandatory, row 11), `80f3a69` (M2, route miscount D11), `29e6204` (M1, D12 DSN leak), `abc86c5` (M2) + `b457748` (M3) (N1, the same leak in both senders), and `check-no-secret-leak.sh` (row 14.4). See the re-estimate below for why "LOW each" was wrong about the remainder.

Grouped because each is LOW cost and none blocks anything:

| Item | Finding | Fix | Cost |
|---|---|---|---|
| M1: `DisallowUnknownFields()` on request decode | 1.6 | Strict decode is what makes additive contracts enforceable; today a client typo is accepted and dropped | LOW |
| M1: `request_id` in every log line | D3, 14.2 | Compounds with item 18; do them together | LOW |
| ~~M1: reject bare tokens without the `Bearer` scheme~~ | 3.2 | ~~`trimBearer` accepts a scheme-less header~~ — **DONE 2026-08-07, `coldmail-verify@d481c97`.** `bearerToken` replaces `trimBearer`; the scheme is mandatory and matched case-INSENSITIVELY (RFC 9110 §11.1, which both senders already honour and M1 alone violated — the estimate missed that this was TWO properties, not one). 3 tests / 9 cases, break-verified B110–B113. Estimate LOW, actual LOW: the only correct one in this table so far | ~~LOW~~ **DONE** |
| M1: remove the per-caller→static-token fallthrough | D8 | Revoking a per-caller key does not fully revoke sync access while any static token exists | LOW |
| M1: migration tracking table | D6 | Re-apply safety rests entirely on per-file idempotence guards; nothing records what ran | LOW-MED |
| M1: move tables into a dedicated schema | D7 | **21 base tables + 3 monthly partitions** in `public` incl. `jobs`; the senders already FK into `public`. **Collision risk in a shared DB** (count corrected 2026-08-07 — see note below) | MEDIUM |
| M1: expose `internal/metrics` over HTTP | D16 | It computes block/unknown/cache rates and latency p50/95/99 with **no exposure and no caller** | LOW |
| M2: seed `page_size` default/max as config | D9 | 100/200 hardcoded in `parseReportPageSize` — **governs a platform-visible response** | LOW |
| M3: seed the bare `10*time.Second` DB timeout | D10 | Every comparable timeout is a config key; this one is not | LOW |
| ~~M2: fix `contract_drift.sh`'s "22 routes gated" log~~ | D11 | ~~Registration and probe map are both 21~~ — **DONE, `coldmail-send-managed@80f3a69`** (landed as part of item 14's cross-module sweep). A success line that miscounts what it checked is the same family as a gate whose canaries are unreachable while its log claims otherwise | ~~LOW~~ **DONE** |
| ~~M1: verify whether pgx v5 can embed DSN fragments in the error logged at `main.go:113`~~ | D12 | ~~Unverified risk~~ — **VERIFIED AND FIXED in `29e6204`. It was a REAL LEAK, not a risk:** pgx v5.10.0 embeds the whole connection string, password included, in its parse error. Fixed at the SOURCE (`store.OpenPG` wraps both returns in `scrubDSNError`, `postgres.go:52,56`) rather than at the log call. `main.go:113` is now a comment; the live log site is `:115`. Unit-tested in `internal/store/dsn_redaction_test.go` | ~~LOW~~ **DONE** |
| All three: `/metrics` in Prometheus format | 9.5 | No module has one | MEDIUM total |
| ~~M1: adopt a `no_secret_leak`-style CI gate~~ | 14.4 | ~~No automated log-scanning gate exists~~ — **DONE. `scripts/check-no-secret-leak.sh` exists and is WIRED (`.github/workflows/ci.yml:145`).** It boots the real binary with canary secrets and scans the process log, rather than grepping source — deliberately, because the leak it had to catch had no credential-looking token in the source at all. Scope is stated honestly in the gate itself: BOOT path only, since M1 has no compose stack to drive request traffic against (that is item 15) | ~~LOW~~ **DONE** |
| Senders: adopt M1's `Dial{Value,Set}` unset-vs-zero distinction | 11.4 | Consuming code returns a do-nothing zero rather than inventing a value | LOW |
| ~~M3: read `account_cap_race_test.go` / `account_unquarantine_race_test.go` bodies~~ | §8.7 | ~~Not verified in this audit~~ — **CLOSED BY VERIFICATION, no code change needed.** Both bodies read; both DO meet M3's scoped-`pg_stat_activity` contention requirement. This row was an unread-file placeholder, not a defect | ~~LOW~~ **CLOSED** |
| M1: prove GAP-37 (exactly-once under slow-holder + TTL reclaim) | D14 | Scaffold names the right five scenarios and `t.Skip`s. No singleton guard exists, so the property rests entirely on `SKIP LOCKED` + CAS | **HIGH** |

**On the last row:** D14 is LOW-priority-by-order but HIGH-cost, and it is the item most likely to be
mis-sized. The design is present and reasonable; what is missing is proof. Tolerating a second instance via
`SKIP LOCKED` + CAS is arguably more robust than forbidding it with a lock — **but it is exactly the property
GAP-37 was written to prove and does not.**

**Re-estimated 2026-08-07 to MEDIUM–HIGH, and the reason is structural.** M3's anti-vacuity harness for
its own race tests counts blocked backends in `pg_stat_activity` to prove two workers genuinely contended.
That instrument **cannot be ported here**: `FOR UPDATE SKIP LOCKED` (`postgres.go:709-725`) never
lock-waits — the loser skips the row and returns empty, so there is nothing blocked to observe. Proving
two workers raced one ready row needs a different instrument entirely (advisory-lock choreography or a
committed-state differential), which is the part the HIGH estimate was hiding.

**Two claimed defects in this area were investigated and REJECTED.** The hardcoded `WorkerID:
"drainer-1"` (`main.go:229`) and the per-batch `locked_at` cannot fire in the shipped topology: there is
exactly one sequential drain goroutine (`drainer.go:145`; the only `go func` in `main.go` is at `:77` and
is not the drainer), reclaim runs only at the top of `drainTick`, and multi-drainer coordination is an
explicit documented deferral (`phase-5-queue-spec.md:425` GAP-34, `arc-closing.md:56`,
`config-manifest.md:276`, GAP-25/OQ-P4-8). The load-bearing CAS clause is `AND status='in_flight'`.
**This is a test-strength defect, not a production one** — which is what the row should have said.

### 15b, re-estimated — what the LOW column got wrong

Requested before item 15 opened, because item 13 came in HIGH against a MEDIUM estimate. As written this
table is **~133–140h (~17 working days)**, not "LOW each". After deferring the 7 rows that are blocked on
a ruling rather than on work, **~5 days**. Item 15 A+B is **14–17h (~2–2.5 days)**; 15-C is MEDIUM but
ruling-blocked. C3's trimmed remainder is **~52–55h (~7–8 days)**.

**Four rows marked LOW are really HIGH.** Worst is M3's bare DB timeout (row `D10`): the plan names one
site; there are **42**, an ~8× miss.

**Five planned fixes would WEAKEN an existing control if done as written.** These are the ones that
matter, because each trades a real proof for a nominal improvement:

1. **`DisallowUnknownFields()` (row 1).** Strict decode would destroy
   `control-plane/internal/api/pg_tenancy_test.go:287-293`, which proves a forged `user_id` /
   `owner_user_id` / `owner` in the request body is IGNORED rather than honoured. That test submits the
   forged fields and requires **202** (`submitAs` does `t.Fatalf` on anything else, `:146`). Under strict
   decode the request 400s, the test goes red, and the obvious "fix" is to stop sending the forged
   fields — which deletes the proof that forging them is harmless. The tenancy property is strictly more
   valuable than the typo-rejection property; sequence this so the forged-owner case is re-expressed
   FIRST (assert the stored owner, not the status code), or it will be quietly discarded.
2. **15-C, the migration/runtime role split.** Makes `TestPGGrants_RoleSwitchActuallyBites` vacuous —
   from a session connected as `cp_runtime`, `SET LOCAL ROLE cp_runtime` is a no-op and BOTH directions
   still pass. See the corrected note under item 15 above.
3. **`request_id` in every log line.** Not one estimate but two, and **which one applies is a ruling, not
   a measurement**: `canonical-module-contract.md:297` says "**Every** log line carries the
   `request_id`", while `:74` says "at least **one**". M1 satisfies the narrow clause today and fails the
   broad one at **47 sites** plus a Ruby half with 2 logger references. LOW vs HIGH turns entirely on
   which clause governs. **Do not start this until that is decided.**
4. **Dedicated schema (D7)** and **migration tracking (D6)** — both interact with the fact that M1 has
   **no down-migration mechanism at all** (zero `.down.sql`, forward-only). A tracking table that records
   what ran, in a system that cannot un-run anything, records a fact nobody can act on; that is a design
   question, not a LOW chore.

**Three rows closed for free** (verification, not work): `dsn-in-error` (D12), `m3-race-tests` (§8.7),
`m2-contract-drift-log` (D11). All three were "unverified" placeholders that turned out already true or
already fixed.

### THE STOP BOUNDARY, and why it is here

**B0 = this plan-truth commit + the bearer fix + filing the new items below — landed BEFORE item 15
opens.** The reason is not tidiness. A half-landed item 15 falsifies five statements already in the
trees, and **no gate objects to any of them**: `ci.yml:138`, `check-no-secret-leak.sh:18`,
`Docs/api-contract.md:25`, `ADR-0016:124`, `routes_contract_test.go:12`. Each currently says, correctly,
"M1 has no compose stack". The moment one exists and the work stops mid-flight, all five are lies that
CI reports green.

### NEW ITEMS FOUND WHILE VERIFYING, 2026-08-07

| # | Item | Module | Cost | Note |
|---|---|---|---|---|
| **N1** | **Senders leak the DSN in a pool-open error — the SAME defect as M1's D12, unfixed** | M2+M3 | LOW | `services/control/internal/db/db.go:21-23` returns `fmt.Errorf("parse dsn: %w", err)` unscrubbed, and `cmd/control/main.go:50` logs it. Both pin pgx **v5.7.4**. **MEASURED LEAK=true**, not inferred. M1's fix (`store.scrubDSNError`) is the template. **The fixture trigger DIFFERS at 5.7.4** — use `sslmode=bogusmode`, NOT `pool_max_conns=notanumber`; a naive port of M1's test would be VACUOUS (it would assert on an error that never carries the DSN at this pin). Neither sender has a boot-path leak gate, though both have a live-log `no_secret_leak.sh` for request traffic |
| **N2** | **`check-docs-updated` inherits its green from the PREVIOUS commit** | M1 | LOW | With no `origin/main`, `base=HEAD~1` and the gate diffs `$base...HEAD` UNIONed with the working tree. **Measured:** with `Docs/api-contract.md` reverted so the tree touched NO doc, the gate still printed `docs-updated: ok (code_changed=true docs_changed=true)`, rc=0 — because HEAD `0cacc36` had touched a doc. On a remote-less clone this gate is satisfied by whatever the previous commit happened to touch. Fix: when there is no upstream, diff the WORKING TREE + INDEX only and drop the base-ref union |
| **N3** | **The Windows host silently does not execute freshly-built Go test binaries** | infra | — | `check-test-execution-selftest` failed 3 of 10 on the host (cases 2, 3, 9) with `exit 0, want 1`, and in each the just-appended fixture package was **absent from the gate's output entirely** (declared=0). Same three pass in Docker; `scripts/` unmodified. This is the Norton interference in a quieter form than hanging — not a timeout, a silent non-execution that reads as "nothing declared". **Consequence for C4: every run must go through the Docker toolchain.** A host run can produce a green that means nothing |

---

## TIER 4 — The decision: does M1 get a uniform platform surface?

**Everything above is worth doing regardless.** This tier exists only if the answer to "one integration
instead of two" is yes. **It is a phase of work, not a fix pass.**

### 16. M1: implement `quote` / `run` / `report` / `describe` — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DEFERRED — Tier 4, ruled out of scope.** Not a technical blocker and not an oversight: a deliberate boundary, recorded in [`deferrals-and-false-greens.md`](deferrals-and-false-greens.md).

- **Finding:** §1 (the confirmed hypothesis), classifications 1.1, 1.7, 7.1. Contract §1.5.
- **What:** M1 implements **no part** of the uniform verb set — zero occurrences of quote/run/report/describe
  anywhere in its Go tree or its docs. It implements a disjoint set (`POST /v1/verify`,
  `POST /v1/verify/bulk`, `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/results`), has **no cost or feasibility
  verb of any kind**, and no self-description verb.
- **Recommendation:** an additive second surface — `/v1/quote|run|report|describe` alongside the existing
  routes, which stay (additive-only; never remove). `run` maps to bulk submit, `report` to job status +
  results, `describe` declares verification capabilities and vocabularies, `quote` reports cost and/or
  feasibility for a recipient count.
- **Cost:** **HIGH.** New handlers, a new error envelope (item 18), a progress model that supplies
  `fraction_complete`, a cost model that does not exist today, and behavioural tests against real
  infrastructure rather than fakes.
- **Depends on:** items 1 (tenancy — `run`/`report` are meaningless without it), 18 (envelope), A6 (identity
  space).
- **Risk if skipped:** the platform maintains **two** integrations permanently — one shared by the senders,
  one bespoke for M1. **That is a legitimate outcome and this plan does not presume against it.** Two
  integrations for two genuinely different domains may well be cheaper than this item.

### 17. M1: a cost or feasibility verb — **BLOCKS-PLATFORM-INTEGRATION** (part of 16)

**STATUS: DEFERRED — Tier 4, with item 16.**

- **Finding:** classification 7.1, UI questions 15-16. Contract §1.6.
- **What:** M1 has no cost verb, field, or dry-run. Verified across every request/response struct: cost is
  discoverable **only by submitting real work.** The one `cost_units` in the codebase is on the internal
  `external_calls` vendor-spend ledger, never exposed and never accepted as input.
- **Recommendation:** at minimum the cost half of the contract's `quote`
  (`{recipient_count, cost, currency, unit_price_credits, tier}`), even at `unit_price_credits: 0` — M3
  already sets the precedent for a module whose real pricing lives elsewhere. Optionally M3's feasibility
  block, which fits M1 well (it has real rate budgets and node capacity to report).
- **Cost:** MEDIUM-HIGH — the pricing model itself is a product decision, not just a route.
- **Risk if skipped:** the platform cannot show a user a price or a pre-check before committing verification
  work.

### 18. M1: coded error envelope with `request_id` — **BLOCKS-PLATFORM-INTEGRATION**

**STATUS: DONE — `cc3ce0e` (M1)**, with a scan that makes a 27th unenveloped site impossible rather than a list of the 26 that were fixed. One sub-question is **separately deferred**: whether `request_id` must appear on every log line. That deferral is on a **contract contradiction**, not on remaining work.

- **Finding:** D3, classifications 2.1-2.4, 2.7. UI questions 5-7. Contract §1.3.
- **What:** ~25 hand-rolled `{"error":"<free text>"}` sites, no codes, **no `request_id` in any error body**,
  and a second shape on one route (`{error, max, got}` at `api.go:242-244`).
- **Recommendation:** one `writeError` serializer, the contract's envelope, the seven-code enum, and
  `request_id` on every error. **M1 already mints request ids** and returns one in the success path's
  `meta.request_id` — the error path just drops it, so the plumbing largely exists.
- **Cost:** MEDIUM — mechanical but touches ~25 sites, and each needs a code chosen deliberately rather than
  mapped by rote.
- **Risk if skipped:** **a user-reported M1 failure is correlatable to nothing** — no code, no request id in
  the body, and none in any log line either. Support triage is guesswork. This is the most operationally
  expensive small gap in the audit, and it is worth doing **even if Tier 4 is declined**, because it improves
  M1's existing surface on its own terms.

---

## Ordered summary

**RECONCILED AGAINST THE SHIPPED REPOS 2026-08-07, at the close of Chunk 4.** Every status below was
established from the module repos' commit histories and, where a claim was checkable, from the tree
itself — not from this document's own prior text. That distinction matters here: **the item bodies
above carry no status markers at all**, so before this reconciliation the only way to learn what had
shipped was to read three git logs. A plan that cannot tell you its own state is the planning form of
the unwired test.

The sha in each row is the commit that closed it, and every one of them is checked by
`_verify/cprun.sh` step 4 to be a real commit in the repo it is attributed to.

| # | Item | Module | Cost | Blocks integration | Status |
|---|---|---|---|---|---|
| **1** | Scope job reads to the authenticated caller (D1) | M1 | MEDIUM | **YES** | **DONE** `29e6204` |
| **2** | Real-PG cross-tenant test for item 1 | M1 | LOW | no | **DONE** `29e6204` (isolation defect it introduced fixed in `88b6dd0`) |
| **3** | Grant tests as `cp_runtime`, or drop the CI claim (D13) | M1 | LOW | no | **DONE** `29e6204` — via `SET LOCAL ROLE` from the existing session, so it did **not** require a connectable `cp_runtime`; **item 15 shares no work with it** |
| **4** | Wire or reject `accept_and_defer` (D2) | M1 | LOW–MED | no | **DONE** `29e6204` — the rung is real; `internal/api/frontdoor_defer_test.go` is the control |
| **5** | Accept optional `user_id` on `quote` (D4) | M2 | **LOW** | **YES** | **DONE** `ba9312a` |
| **5b** | M3: resolve `quote`'s always-zero cost | M3 | LOW | **YES** | **DONE** `4abd8eb` (M3) + `0b42e1b` (**M2**) — closed by declaring the cost fields INERT in `describe`, in both senders. The ruling was (b): the field stays, `describe` declares it inert. `148f63f` had already zeroed the flat-subscription pricing |
| **6** | Declare `pricing.model`'s value set | M2+M3 | LOW | **YES** | **DONE** `dde94e0` (M2), `3595117` (M3) |
| **7** | Client tolerates unknown error codes | *platform* | LOW | no | **DONE** — `canonical-module-contract.md:119` clause 6, additive-only with a mapping rule |
| **7b** | Route mux-level 405/404 through the envelope (**D17**) | all | LOW | no | **DONE** `e3fee99`+`de01752` (M2), `5924fbc` (M3), `29caf15` (M1, where the route-drift gate found the defect) |
| **8** | `describe`: vocabularies + error codes + pricing set | all | **LOW** | **YES** | **DONE** `dde94e0` (M2), `3595117` (M3) — all three DERIVED, never hand-listed |
| **9** | Notifications read route (D5) | M3 | **LOW** | no | **DONE** `1d9a042` |
| **10** | Uniform `/readyz`; M1 readiness checks the DB | all | LOW | no | **DONE** `bcf6324` (M1), `9c74fa8` (M2), `1df98b0` (M3) |
| **11** | One null convention for the five widened fields | M3 | LOW | no | **DONE** `22b6c9d` — the divergence was DOCUMENTED, not enforced; now tested |
| **12** | Graduated kill-switch ladder for dispatch | M2+M3 | MEDIUM | no | **DONE** `e939c4f`+`c4b2086` (M3), `5af51f8` (M2), `9741986` (M1's half of the shared error code) |
| **13** | Shadow-mode-by-default for autonomous mutators | M2+M3 | MEDIUM | no | **DONE** `c300fe6`+`0e4d398` (M2), `ac70c38` (M3). Came in **HIGH against a MEDIUM estimate** — the trigger for §15b's re-estimate |
| **14** | Response-version assertion ↔ route-drift gate | all | LOW–MED | no | **DONE** `0cacc36`+`29caf15` (M1), `80f3a69` (M2), `9988e53` (M3) |
| **15** | Package the control-plane (D15) | M1 | MEDIUM | no | **DEFERRED** — nothing built; no Dockerfile and no compose file exist in `coldmail-verify`. Blocks the M1 request-path leak scan, which is the one item genuinely blocked on infrastructure |
| **15b** | Sixteen lower-priority parity items | all | ~~LOW each~~ **see §15b re-estimate** | no | **PARTIAL — 5 closed** (`d481c97` Bearer scheme, `80f3a69` route miscount D11, `29e6204` D12 M1 DSN leak, `abc86c5`/`b457748` N1 sender DSN leak, `check-no-secret-leak.sh` 14.4). **Rest DEFERRED**, re-estimated to ~133–140h as written / ~5 days after deferring the 7 ruling-blocked rows |
| **16** | Implement quote/run/report/describe | M1 | **HIGH** | **YES** | **DEFERRED — Tier 4, ruled out of scope by the brief.** Not a technical finding: a deliberate boundary |
| **17** | A cost or feasibility verb | M1 | MED–HIGH | **YES** | **DEFERRED — Tier 4**, with 16 |
| **18** | Coded error envelope with `request_id` (D3) | M1 | MEDIUM | **YES** | **DONE** `cc3ce0e` — with a scan that makes a 27th unenveloped site impossible. Note the `request_id`-in-every-log-line scope question is **separately deferred on a contract contradiction**, not on work |

**Where that leaves the plan: 17 of 21 items DONE, 1 partial (15b), 3 deferred (15, 16, 17).** Twenty-one,
not eighteen, because 5b, 7b and 15b were added after the audit closed — 5b and 7b by the refutation pass
that went looking for the audit's own errors, which is the only reason either exists. The three deferrals
plus 15b's remainder are each recorded in
[`deferrals-and-false-greens.md`](deferrals-and-false-greens.md) with what blocks them — including the
two whose *premises* were wrong (15-C's shared-work discount, which did not exist, and D10's 1-vs-42
site count).

**Also delivered, and NOT in this table because the plan never contained them.** **40 commits** of gate,
harness and CI work across the three repos since this plan was written — 8 in M1, 16 in M2, 16 in M3
(`git log --since=2026-08-04 -- scripts/ .github/workflows/ tests/`, so the figure is re-derivable rather
than asserted). The audit did not anticipate any of it, because the audit was looking at the modules and
these are defects in the things that were *checking* the modules: six unwired test packages, three gates
satisfied by prose or by a commented-out step, a gate inheriting its verdict from the previous commit, a
0-of-0 comparison passing as green, an anti-vacuity check flaky in the **passing** direction, and a report
that truncated its own evidence.

**Eighteen false-green classes; this plan predicted none of them.** Out of 66 post-audit commits total,
40 are apparatus. That ratio is the single most useful thing to carry out of this arc: the fix list was
substantially right about the systems and had nothing at all to say about the machinery that was supposed
to prove the systems right — which is where most of the real defects turned out to live.

### The shape of it

**Six items block a single platform integration.** Of those, **three are LOW cost** (5, 6, 8) and **three are
the M1 platform surface** (16, 17, 18) — which is a phase of work whose only justification is avoiding a
second integration.

**So there are two coherent plans, and the choice between them is a product decision the audit does not
make:**

- **Plan A — two integrations.** Do Tiers 0–3 plus items 5, 6, 8. One client drives both senders; M1 keeps
  its own bespoke integration and gets materially better on its own terms (tenancy fixed, errors coded,
  packaged, tests honest). **Item 18 is worth doing here anyway** — coded errors with correlation IDs improve
  M1's existing surface regardless of which plan wins.
- **Plan B — one integration.** Plan A plus Tier 4. Higher cost, and its value is entirely "one integration
  instead of two."

**One caution on Plan B, from the audit's evidence:** the uniform contract M1 would adopt has been validated
by exactly **one** independent implementation. M2 and M3 both implement it, but M3 inherited it by fork —
`auth.go` is byte-identical, `run` and `report` are character-identical. And the one place M3 genuinely had to
think rather than copy — `quote`, where its billing model differs — is precisely the one place the contract
broke (item 5). **Before committing M1 to a HIGH-cost adoption, review the contract on its own merits.**
M3's agreement is not independent corroboration, and item 5 is evidence that the contract has not been
stress-tested by a module with genuinely different needs.

---

## Not in this plan, deliberately

Ten differences the audit classified **DELIBERATE** — correct as they stand. Aligning them would destroy
information, and the brief was explicit that classifying a deliberate design difference as a defect is as
wrong as missing a real one:

M3's 17 `/v1/byosmtp/*` extension routes · M3's `quote` as feasibility rather than cost · M1's nodes carrying
IP reputation, warmup, quarantine, and audit state · the three different status vocabularies · M1's
`caller_ref` as an opaque platform correlation echo · M3's admin-only quarantine verbs · M3's split
user-writable/admin-only settings tables · campaign-scoped reports returning an empty aggregate rather than
404 · M1's forward-only migrations (the *absence of a ledger* is item 15b row 13, not the convention) ·
M1's bulk results omitting `email` (plaintext purged on settle, a deliberate privacy property).

> **One correction to that first entry, added 2026-08-07.** Calling M1's forward-only migrations
> deliberate-and-correct is right about the *convention* and was incomplete about its *consequence*. M1 has
> **zero `.down.sql` files and no down-migration mechanism at all**, and `0013_job_owner.sql` contains a
> `REVOKE` on `api_callers` that **`git revert` cannot undo** — reverting the commit removes the SQL, not its
> effect on the live database. So M1 has no rollback path for a bad deploy, which is a go-live blocker that
> this section's phrasing would have let a reader classify as a settled design choice. Recorded in
> [`../coldmail-verify/Docs/go-live-rollback-posture.md`](../coldmail-verify/Docs/go-live-rollback-posture.md).
> The convention is still deliberate; what was missing was anyone having written down what it costs.

Also not in this plan: the credential-hash choice (bcrypt vs sha256-indexed-lookup are both correct for their
access patterns), and RLS for M1 — classified **GAP-OPTIONAL** because in the senders RLS is an explicit
`USING(true)` backstop rather than the isolation mechanism, and **requiring it of M1 would not have prevented
D1.** Recommend it as defence-in-depth; do not make it a contract condition.
