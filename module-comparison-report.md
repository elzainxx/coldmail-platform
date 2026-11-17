# Cross-Module Compatibility Audit — Comparison Report

**Scope:** three sibling repos behind one platform.
**Status:** AUDIT ONLY. Nothing in any repo was created, edited, or deleted. Every fix named here is a
RECOMMENDATION for a separate fix pass.
**Date:** 2026-08-03.

| Tag | Repo path | Identity (from code) | Language / shape |
|---|---|---|---|
| **M1** | `Codmailling` | Truemail Verification System — email validator | Go control-plane + Ruby verifier-node |
| **M2** | `Codmailling 2` | Sender module — shared SMTP pool | Go "Brain" + Node.js sender boxes |
| **M3** | `Codmailling 3` | BYOSMTP sender — user-owned SMTP | Go "Brain" + Node.js sender boxes |

**The boundary under audit** is PLATFORM ↔ MODULE. The Brain↔box and control-plane↔verifier-node splits
INSIDE each module are coordinator-worker, not service boundaries, and were audited only for whether each
module is independently deployable and operable — never for service-mesh, saga, circuit-breaker, or
service-discovery patterns.

**Evidence rule applied throughout:** claims are traced to `.go` / `.sql` / `.rb` / `.sh` / `.yml` source at
`file:line`. Spec and design documents were used for orientation only and are never cited as evidence for a
behavioural claim. Where a doc and the code disagree, the code is treated as truth and the disagreement is
itself recorded as a finding.

**Weighting rule applied throughout (stated because it changes what the evidence means):** M3 is a fork of
M2 (`Codmailling 3/CLAUDE.md` records the fork point `21b65e65eba8f2cc16a84408bac6e018511ec46b`). Where M2
and M3 agree, that is **WEAK evidence** — it proves copy inheritance, not a validated contract. Where **M1
and M2 agree, that is STRONG evidence** — two independently built modules converging. Every three-way match
below is annotated accordingly. One case is called out explicitly: `platformapi/auth.go` is **byte-identical**
between M2 and M3 (verified by `diff`, zero differences) — that is inheritance, and it is reported as
inheritance, not as a validated two-module agreement.

---

## Section 0 — Method, and an honest note on agent reliability

Three inventory subagents (one per module, Sonnet, parallel, independent) produced the raw category sweeps.
Adversarial verification agents were then launched to attack those inventories.

**Reporting honestly, per the brief's requirement that a failed agent is UNANSWERED, not "no finding":**

| Verification agent | Target | Outcome |
|---|---|---|
| `a3a34c35` (inventory) | M1 | COMPLETED — result folded in below |
| `adb4a833` (inventory) | M2 | COMPLETED — result folded in below |
| `a3e64160` (inventory) | M3 | COMPLETED — result folded in below |
| `af46521e` (verify) | **M2 inventory** | **COMPLETED — folded in, see §0a** |
| `a057ed3c` (verify) | M3 inventory | **INTERRUPTED mid-run — UNANSWERED** |
| `a47cf8a0` (verify) | — | **INTERRUPTED mid-run — UNANSWERED** |
| `abb27526` (verify) | — | **never returned a result — UNANSWERED** |
| `a47dd609` (verify) | — | **never returned a result — UNANSWERED** |

**Corrected count (2026-08-04).** The inventory workflow's own completion tally reported 6 logical agents,
4 done / 2 errored. Its journal (`journal.jsonl`, one `result` line per completed agent) records the true
figures: **17 agents started, 4 returned a result.** The M1 and M3 verification stages each consumed **six
stalled retry attempts** (180 s no-progress timeout each) before being abandoned, which the logical-agent
count hides. So the adversarial layer was even thinner than first reported — **one** verification of three
ran, not zero, and the other two are UNANSWERED after six attempts each. Per the brief, those are UNANSWERED,
not "no finding"; no absence of a finding from them is treated as evidence of absence.

**M1's and M3's inventories therefore remain unverified by any second party.** Every load-bearing claim about
them in this report was re-verified first-hand, but that is my own check, not independent corroboration.

Because the adversarial verification layer effectively did not run, **every load-bearing claim in this
report was re-verified first-hand** against the actual working trees rather than accepted from a subagent.
That pass found a real subagent error, which is recorded here rather than silently corrected:

> **Subagent error corrected:** the M3 inventory reported "exactly 19" admin routes and then attributed the
> discrepancy to the CI gate's own probe map counting differently. Direct count of the canonical slice
> `adminRoutes()` (`Codmailling 3/services/brain/internal/adminapi/server.go:104-148`) is **21**, and the
> gate's `ADMIN_ROUTES` probe map is also **21** — they agree exactly, and the gate's log string ("21 routes
> gated", `contract_drift.sh:71`) is correct. The subagent's count was wrong and its explanation of the
> discrepancy was an invented rationalisation for its own miscount. This is exactly the class of error the
> brief's "verify each against the actual tree before folding in" instruction exists to catch.

A genuine off-by-one **does** exist, but in M2, not M3, and only in a log string: M2 registers 21 admin
routes (`Codmailling 2/.../adminapi/server.go:43-74`) and its gate probes 21, but the gate's log line says
"22 routes gated" (`Codmailling 2/tests/harness/checks/contract_drift.sh:69`). Cosmetic; the probe map is
correct, so the gate cannot pass with a missing route.

---

## Section 0a — The one adversarial verification that DID complete, and what it refuted

The M2 verification agent (`af46521e`) returned after this report's body was written. It found **7 wrong
claims and 7 omissions** in the M2 inventory. Each is re-verified first-hand below and **corrected in place**
in the sections that follow. Recorded here rather than silently fixed, per Rule 2 / the brief's "flag, never
silently resolve."

**Two of its findings change claims in this report. Both are corrections against my own text.**

### 0a.1 REFUTED — "exactly one serializer" is not true of the served surface (affects §3.2, contract §1.3.3)

I wrote that in both senders `writeError` is *the only serializer* and that no handler hand-rolls a different
shape, and the canonical contract elevated this to a REQUIRED clause reading "**Exactly one serializer** —
one function, so uniformity is **structural**." **The handler-level claim is true. The "structural
uniformity" conclusion is not.** Verified first-hand:

- All three modules register with Go 1.22 method+path patterns — `s.mux.HandleFunc("POST /v1/quote", …)`
  (`Codmailling 2/.../platformapi/server.go:56-59`; M3 identically at `server.go:140-143`; **M1 too** at
  `Codmailling/control-plane/internal/api/api.go:116-123`).
- The parent mux mounts each platform path **explicitly** (`Codmailling 2/.../httpapi/health.go:43-46`).
- So `GET /v1/quote` matches the *path* but not the *method*. `net/http.ServeMux` answers it —
  `405`, `Content-Type: text/plain; charset=utf-8`, body `Method Not Allowed\n`, `Allow: POST` — **before
  `requireAuth` runs**, so it is both unauthenticated and outside the envelope. An unmatched `/v1/*` path
  gives `404 page not found\n`, likewise text/plain.
- No test observes this: `contract_drift.sh:79-88` probes each route with its **method-appropriate** verb
  only, so the 405 shape is never exercised by any gate in any repo.

**Corrected claim:** at least three distinct error shapes are reachable on each sender's platform surface —
the JSON envelope, a text/plain 405, and a text/plain 404. **A client that assumes `response.error.code`
exists on every non-2xx will nil-deref on a wrong-verb or wrong-path call.**

**This is a three-way ADDITIONAL finding, not a divergence** — M1 shares it identically, so it does not
change any comparative verdict. It is filed as **D17** and added to the contract as an explicit clause,
because a canonical contract that says "every error carries a code and a request_id" is contradicted by
all three implementations at the mux layer. **This is the audit's own error, caught by verification** —
exactly what the layer exists for, and the reason its near-total absence for M1 and M3 matters.

### 0a.2 CORRECTED — M2 has 54 config keys, not 53 (affects §3.11, §8 config row)

I reported 53 from `Docs/config-manifest.txt`'s phase breakdown (14+3+2+6+17+11). That arithmetic reproduces
the manifest's own **stale header comment**, written when `sender_0023` was the newest config migration.
Counted directly just now: `grep -v '^#' Docs/config-manifest.txt | grep -cv '^$'` = **54**, and
`config-reference.md`'s key rows = **54** — the two sides `config_drift.sh` compares agree at 54. The 54th is
`brain_singleton_reacquire_interval_sec` (seeded by `sender_0023`), confirmed present in the manifest.

**This is precisely the failure mode the brief warned about** — I quoted a document's summary of the code
instead of counting the code. The brief's rule ("where a doc and the code disagree, the CODE is truth and the
disagreement is itself a finding") applies, and the disagreement is a finding: **M2's `config-manifest.txt`
header comment is stale at lines 5-6** — filed as **D18** (cosmetic; the gate compares counted content, not
the comment, so it cannot pass with a missing key).

### 0a.3 Findings that do NOT change this report, but sharpen it

Verified and worth recording; none contradicts a comparative verdict:

- **A third auth domain and a fourth listener exist in both senders, unmentioned in §3.3's "two credential
  types."** `boxlink` registers `POST /v1/box/register` and `/v1/box/heartbeat` on a **separate mTLS server**
  (`boxlink.go:74-75`) with the box registration token as a **third** credential type, and hand-rolls
  `http.Error(w, '{"code":"unauthenticated"}', 401)` at **five sites** (`boxlink.go:96,101,107,121,131`) — a
  bare `{code}` with **no `message` and no `request_id`**, a fourth error shape. Confirmed first-hand.
  **Not a platform-boundary finding** — the box channel is the coordinator-worker split the brief placed
  explicitly out of scope — so it does not alter any verdict. But §3.3's "exactly one credential type on the
  platform surface, two in the module" should read **three in the module**.
- **`alert_webhook_url` is not read on a dispatch tick.** `LoadConfig` assigns `Config.AlertWebhookURL`
  (`dispatch/config.go:331`) and **nothing ever reads that field** — it is dead. The live consumer reads the
  key freshly inside the admin health handler (`health_handler.go:123`, firing `fireWebhooks` at `:128`).
  **Consequence, which matters operationally:** alerting fires **only when an operator GETs
  `/v1/admin/health`**, not on any autonomous cadence. §7's kill-switch row overstated the consumption path.
- **`/v1/report` has two more platform-facing query parameters** absent from §3.1: `cursor` (base64 RawURL,
  malformed → 400) and `page_size` (`campaign_report.go:324`, **silently clamped** — non-numeric or ≤0 falls
  back to 100 rather than erroring). This **strengthens D9**: the hardcoded 100/200 is not just a literal, it
  is a literal governing a platform-visible parameter that fails **silently**.
- **Three more fallback literals in the same file**, same class as D9/D10: `daily_reset_timezone` "UTC"
  (`campaign_report.go:118`), **`throttle_pause_seconds` 900** (`:122`), `daily_reset_hour` 0 (`:126`). The
  900 s window feeds the `waiting_for_capacity` predicate (`:185,190`), so a config-read blip silently
  changes which jobs a platform client sees as waiting.
- **RLS is on 8 of 9 M2 tables, not "all."** `sender.schema_migrations` has no RLS and no policy while
  holding `GRANT SELECT … TO sender_module` (`sender_0000_bootstrap.up.sql:38`). **M3's equivalent claim in
  this report checks out** — verified independently just now: 13 `ENABLE ROW LEVEL SECURITY` statements
  across 14 `byosmtp.*` tables, with all five Phase-1 tables (`campaigns`, `notifications`,
  `user_settings`, `user_config_overrides`, `user_suspensions`) covered; `byosmtp.schema_migrations` is the
  uncovered one, same as M2.
- **`brain_singleton_acquire_timeout_sec` is an ordinary seeded config key**, not the "structural,
  non-config-table switch" §3.13 called it — it is manifest key 49, read live at `main.go:312-323`, with the
  env var only as the read-failure fallback.

---

## Section 0b — The adversarial refutation pass (2026-08-04): 13 claims, 0 refuted, 6 corrected

Because the original verification layer had effectively not run (§0), the audit's **13 load-bearing claims**
were each handed to a skeptic instructed to **refute** them, with an independent second pass on anything not
cleanly confirmed. **20 agents, 0 failures** — so unlike §0, nothing here is UNANSWERED.

| Claim | Verdict | Effect |
|---|---|---|
| **D4** quote bodies mutually rejecting | **CONFIRMED** | Unchanged. The cleanest survivor. |
| **D2** `accept_and_defer` inert | **CONFIRMED** | Strengthened — see below. |
| **D3** M1 no coded envelope / no `request_id` | **CONFIRMED** | Strengthened — see below. |
| **D13** grant tests always skip | **CONFIRMED** | One path typo corrected. |
| **D15** no control-plane container | **CONFIRMED** | One inference softened. |
| **U1** M3's race tests vs its own Rule 10 | **CONFIRMED — closed** | **No finding.** See §0b.1. |
| **U2** pgx DSN leak | **CONFIRMED — closed** | **Escalated.** See §0b.2. |
| **D1** M1 cross-tenant read | PARTIAL | **Severity down-graded — §0b.3.** |
| **C1** senders one route apart | PARTIAL | **Headline retracted — §7.1.** |
| **H1** M1 lacks the verb set | PARTIAL | **Sharpened — §0b.4.** |
| **A4** no credits/quotas anywhere | PARTIAL | **Two-thirds wrong — §0b.5.** |
| **D5** M3 notifications unreadable | PARTIAL | **Worse than claimed — §0b.6.** |
| **D16** M1 metrics unwired | PARTIAL | "No caller" → no *production* caller. |

**Zero claims were refuted. Every mechanism the audit identified is real.** What failed repeatedly was
**enumeration and absolute quantifiers** — route counts, field lists, migration attribution, "NO caller",
"ANYWHERE", "only quote". A consistent, correctable pattern: totals asserted without exhaustive counting.

### 0b.1 U1 CLOSED — M3's race tests comply with Rule 10. No finding.

The audit left this open, having not read the two test bodies. **Answer: both comply, and they are
exemplary.** Each uses Rule 10's *preferred* mechanism (a) — a dedicated connection holding the exact lock
the production path takes — **and** mechanism (b), with a `pg_stat_activity` filter scoped by **both**
`datname = current_database()` **and** a query-text fragment verified to match the real production statement
verbatim. Neither test's assertion set is "no `40P01` occurred"; both assert concrete post-state and ship a
paired control. **The audit's cited paths were wrong**: both files are under
`services/brain/internal/platformapi/`, not `internal/dispatch/`.

Two cosmetic in-file notes (neither is vacuity): the comments claim to prove **both** requests queued while
the assertion is `blocked > 0` (at least one); and `account_cap_race_test.go` asserts only status 409 for the
loser, never the error code, so a different 409 would be accepted.

### 0b.2 U2 CLOSED — and it escalates from "unverified risk" to CONFIRMED-CONDITIONAL

The audit filed this as a risk it had not verified. **Verified now, with the mechanism named.** pgx v5.10.0
(pinned in `control-plane/go.mod`) embeds the **full connection string** — not a fragment — in
`ParseConfigError.Error()` on every ParseConfig-stage failure. Redaction is **incomplete**: `redactURL`
rewrites only the userinfo password and returns `u.String()`, emitting `RawQuery` verbatim, so a `password`
or `sslpassword` passed as a **URL query parameter** (an honored credential) is logged **in cleartext**; and
the keyword/value regex `password=[^ ]*` misses the honored `password = SECRET` spelling. Even when the
password *is* redacted, user/host/port/database are logged verbatim.

**Honest offsetting precision:** the likelier failure at this exact call site is a *ConnectError* (DB
unreachable), which exposes only user/database/host:port and **cannot** leak a password. The leak requires a
ParseConfig-stage failure **and** a DSN shape carrying the password in a query param or the spaced spelling.
So: **conditional, not unconditional** — but no longer speculative, and **M1 has no secret-leak CI gate**
(its code-review job is an `echo` placeholder) that would ever catch it.

### 0b.3 D1 — mechanism CONFIRMED and unmitigated, but severity comes down from HIGH to MEDIUM-as-shipped

This was the audit's only HIGH and its only FAIL grade. **The mechanism is airtight**: `callerID` is
discarded at submit (`api.go:251`) *and* at both read handlers (`api.go:273`, `:313`); `jobs` has no
authenticated-owner column; all three store reads scope on `job_id` alone; and `0005_grants.sql:112` states
RLS is **deliberately absent**. None of that changes.

**Three corrections, all reducing severity:**

1. **Not "full verification results."** `verdictFromRow` sets `Email: ""` — verified first-hand at
   `api.go:444`, commented *"purged on settle (Door 3) — a bulk result never echoes the raw address."* The
   exposure is verdicts, domains, flags/signals, counts, and the victim's opaque `caller_ref` — **not
   email addresses or `email_hash`.** The PII characterisation in §2 was too strong.
2. **Job ids are not enumerable.** UUIDv7's 48-bit ms prefix is predictable, but `idgen.go:35` fills the
   remaining **74 bits from `crypto/rand`**. "Time-ordered, so partially guessable" was misleading:
   **exploitation requires already knowing a victim `job_id`.**
3. **No second untrusted caller exists yet.** `api_callers` INSERT/UPDATE/DELETE are reserved to `cp_admin`
   (`0010_api_callers.sql:43`) and there is **no issuance route or CLI** — grep confirms `api_callers` appears
   only in the store layer and `main.go`. The discard is documented as intentional under a
   single-platform-client assumption.

**Corrected framing:** MEDIUM as shipped; a **hard prerequisite gate on multi-tenant key issuance**. The
moment a second mutually-untrusted key is issued — which `api_callers` structurally permits — it is HIGH.
Fix-plan item 1 keeps its position, since it must land *before* that, not after.

### 0b.4 H1 — verdict stands, but two absolutes were wrong

**The hypothesis is still confirmed**: M1 implements no route named `quote`/`run`/`report`/`describe`, and
the two verbs it genuinely lacks are **`quote` and `describe`** — the pre-flight and self-description halves.
But: (a) M1 registers **six** routes on that mux, not four (`GET /healthz` and `GET /readyz` are on the same
one); (b) "no self-description verb" is too absolute — `GET /readyz` returns a config/capability posture
(ladder rung, smtp/fallback/frontdoor mode, node count, mutator mode), and the Ruby verifier-node has
`/v1/config`, `/v1/stats`, `/version` on the **internal** channel. The defensible form: **no *platform-facing*
describe verb** — nothing enumerating capabilities, pricing, routes, or schemas to a platform caller.
(c) "No cost verb of ANY kind" is right as a *verb* but should not read as "no cost concept": M1 has
`external.CostUnits`, per-job/daily/monthly caps, and returns `signals.external.cost_units` **after the
fact**. That is spend reporting on a finished verdict, not a quote.

Also worth stating: `/v1/verify/bulk` is **run-shaped** and `GET /v1/jobs/{id}` is **report-shaped**. M1 is
the outlier on *naming and contract*, not on capability.

### 0b.5 A4 — the register's headline was two-thirds wrong. This is the largest correction.

The audit asserted "**no** module enforces credits or quotas **anywhere**" and called it the register's
biggest exposure. **The credit-balance half is right** — no wallet, ledger, balance column, deduction,
affordability pre-check, or 402 path in any repo. **The quota half is wrong on three counts:**

- **M2 has live, non-zero per-recipient pricing.** `cost = recipient_count × unit_price_credits`, read fresh
  from `sender.config`, seeded `{"v":1}`, admin-retunable, range-validated 0..1000. M3's own `byosmtp_0048`
  migration calls M2's behaviour *"real, non-zero, per-recipient billing"* and reseeds it to 0.
- **M1 enforces a money-denominated spend quota.** `external/breaker.go:102-117` refuses vendor calls on
  PerJob/Daily/Monthly caps and a rate ceiling, over durable spend summed off the append-only
  `external_calls.cost_units` ledger, wired into the live escalation path and **failing closed**. Caveat: all
  four caps default UNSET and `buildExternal` returns nil (PENDING-REAL-VENDOR), so nothing egresses yet.
- **M3 enforces per-user volume quotas that are the overspend backstop.** `platformapi/store.go:117-128`
  rejects the (cap+1)th `/v1/run` with 409 `max_campaign_size_reached` / `max_queued_jobs_reached` (50 000 /
  100 000, per-user admin overrides); accounts capped at 20; `daily_limit <= 100_000` backed by an
  un-bypassable CHECK. `byosmtp_0042:37` states the purpose verbatim: *"bounding runaway/abuse volume under
  flat pricing."*

**Corrected conclusion:** the "unbounded overspend with no module-side backstop" charge holds **for M2 only**
— and M2 is precisely the module with **non-zero pricing and no volume caps at all**. **The two senders have
the caps and the pricing on opposite sides.** That inversion is a sharper finding than the original claim,
and it is new.

### 0b.6 D5 — understated, not overstated

The audit said M3's `notifications` is read only by `notify.go` "for outbound webhook egress." **There is no
egress at all**: `internal/dispatch` imports no `net/http` anywhere, so `notify.go`'s "delivery" is a pure
in-DB state flip to `delivered`/`suppressed`. M3's only webhook egress is `adminapi/health_alerts.go:257`,
fired from `GET /v1/admin/health`, whose conditions never touch the table. **Severity is therefore higher
than filed:** rows are stamped `delivered` with `notified_at` set, so they are permanently marked as sent to
a recipient that does not exist — **and no retry or backfill will revisit them once a read surface is added.**
Also: the table is created solely by `0033`; `0047` only adds the `suppressed` enum value.

### 0b.7 Nine bonus defects the audit missed entirely

Surfaced by skeptics while hunting refutations. Filed as **D21–D29**; none changes a comparative verdict.

| # | Module | Finding |
|---|---|---|
| **D21** | M3 | Campaign report 500s on a missing config row where M2 returned data — `!ok` fallback unreachable (`campaign_report.go:160-162`) |
| **D22** | M3 | `quote`'s `cost` silently always 0 (`0048` reseeds `default_value` too). A config key whose only consumer computes a guaranteed-zero product is arguably a Rule 1 "real consumer" violation |
| **D23** | M1 | `newRequestID` (`helpers.go:12`) is **dead code — zero call sites repo-wide.** Verified first-hand. Directly explains D3: the api layer has a request-id minter it never uses |
| **D24** | M1 | `store.MetricRowsSince` dead in production — a store method, interface entry, memory impl and stub maintained for a consumer that never runs |
| **D25** | M1 | `TM_RATE_SATURATION_COUNT` parsed, validated, defaulted, never delivered. `Docs/config-manifest.md:300` concedes it |
| **D26** | M3 | `campaignItem`'s five new fields lack `omitempty` — always-emitted explicit nulls, inconsistent with the single-job shape two files away |
| **D27** | M1 | The grant tests would **also** pass vacuously on a *different* failure (missing table / dropped connection → no assertions follow). `postgres_test.go:212` closes the gap circularly: *"proven in the migration itself + CI"* — CI **is** the superuser DSN |
| **D28** | M2 | **No per-user volume caps at all** (no campaign-size, queued-jobs, or account-count cap) — in the repo *with* non-zero pricing. See §0b.5 |
| **D29** | M1 | `Docs/kill-switch-ladder.md:53` and `OPERATIONS.md:143` (as audited; both files have since moved — `Docs/history/kill-switch-ladder.md`, `Docs/operator-runbook.md`) document a mechanism that does not exist (*"accept-and-defer stops the drainer"*); `drainer.go` has **zero** config references, and `secrets.example.env:50` advertises the setting to operators |

**D29 strengthens D2 materially:** the rung is not merely unconsumed on the API side — the drainer-side
mechanism its own operator documentation names does not exist either, and the value is actively advertised in
the example env file. That makes it a live footgun, not a dormant constant. (Precision, in fairness: the mode
*is* echoed in `Summary()` → startup log and `/readyz`, so "no warning" should not read as "no trace.")

---

## Section 1 — The primary hypothesis, tested first

The brief named one hypothesis to test before anything else: M1 was built before the others; its docs
describe `/verify` plus a bulk job handle and never mention quote/run/report/describe. If M1 does not
implement the uniform contract, that is the structural divergence the audit exists to find.

### VERDICT: CONFIRMED. M1 implements no part of the quote/run/report/describe verb set.

Exhaustive search across every `.go`, `.rb`, and `.sql` file in `Codmailling` for
`v1/quote|v1/run|v1/report|v1/describe|handleQuote|handleRun|handleReport|handleDescribe`:
**zero hits.** The only textual match anywhere in the tree is an unrelated `regexp.QuoteMeta` call in
`smtp_stage.go`. Zero hits in M1's `Docs/` either — so this is not a code-lags-docs situation; the verb set
was never part of M1's design.

M1 implements **its own verb set** (`control-plane/internal/api/api.go:114-125`):

```go
mux.HandleFunc("POST /v1/verify",             s.handleVerify)       // api.go:132
mux.HandleFunc("POST /v1/verify/bulk",        s.handleBulkSubmit)   // api.go:216
mux.HandleFunc("GET /v1/jobs/{id}",           s.handleJobStatus)    // api.go:272
mux.HandleFunc("GET /v1/jobs/{id}/results",   s.handleJobResults)   // api.go:312
mux.HandleFunc("GET /healthz",                s.handleHealthz)      // api.go:355
mux.HandleFunc("GET /readyz",                 s.handleReadyz)       // api.go:361
```

Both senders implement the uniform four (`Codmailling 2/.../platformapi/server.go:56-60`;
`Codmailling 3/.../platformapi/server.go:140-152`) — but that agreement is M2↔M3, i.e. **fork
inheritance, weak evidence**. There is **no third-party corroboration that the quote/run/report/describe
shape is right**, because the one independently built module never adopted it.

**Consequence, stated plainly:** the platform cannot drive all three modules through one integration today.
M1 needs per-module handling. This is the structural divergence the audit was built to find. Section 3 of
the C2 diff quantifies exactly how much per-module handling, and the fix plan sizes the alternative.

**What M1 lacks entirely, not merely names differently:**

- **No cost or feasibility verb of any kind.** Verified by reading every request/response struct in
  `api.go`, `verdict/verdict.go`, and `store/postgres.go`: `verifyRequest` has only `email`/`policy`;
  `bulkRequest` has only `addresses`/`policy`/`caller_ref`; the 202 response carries only
  `job_id`/`status`/`counts`. No cost, credit, price, currency, unit, dry-run, or estimate field exists on
  any route. The only `cost_units` in the codebase is on the internal `external_calls` table
  (`migrations/0011_external_calls.sql`), an operator-side vendor-spend ledger for the Phase-6 fallback
  layer's own budget breaker — never exposed on any route, never accepted as a request parameter.
- **No self-description verb.** A UI cannot ask M1 what it can do; capabilities must be hardcoded by the
  caller.
- **No admin/operator HTTP surface at all.** Search for `adminapi|/v1/admin|X-Admin|AdminKey|platform_admin`
  across all `.go`, `.sql`, `.rb`, `.yml`: **zero hits**, verified first-hand. Exactly one `func main` exists
  in the entire repo (`cmd/control-plane/main.go:36`), serving exactly the six routes above. Every operator
  action is instead direct SQL under the `cp_admin` Postgres role — per-caller key issuance is
  `cp_admin`-only, with `INSERT/UPDATE/DELETE` explicitly REVOKEd from the runtime role
  (`migrations/0010_api_callers.sql:58-60`).

---

## Section 2 — Finding of record: M1 cross-tenant read gap

This is the highest-severity finding in the audit. It is a security finding, not a contract-shape finding,
and it is reported first for that reason. It was found first-hand, then independently reproduced by the M1
inventory agent with identical evidence.

**Any authenticated caller can read any other caller's bulk job status and full verification results.**

The chain, every link verified in the tree:

1. **`jobs` has no owner column.** `CREATE TABLE jobs` (`migrations/0009_work_items_jobs.sql:99-116`)
   declares exactly: `id, policy_key, policy_version, status, total_count, settled_count, submitted_at,
   completed_at, caller_ref`. The table comment states the omission is deliberate: *"Holds
   verification-work state ONLY … NO user id, credits, CSV, filename, or per-row UI state"* — on the premise
   that the platform owns the user-facing job and the module owns only the work.

2. **The authenticated principal is explicitly discarded.** `handleBulkSubmit` resolves `callerID` from
   `authenticate()` and then throws it away (`api.go:251`, quoted verbatim):
   ```go
   _ = callerID // authenticated principal; rate-limiting/attribution by caller_id is a later dial
   ```
   `CreateJob` (`store/postgres.go:783-798`) takes no caller parameter, because there is no column to put it in.

3. **The read paths filter on the id alone.** `GetJob` (`postgres.go:801-822`), `JobCompletion`
   (`postgres.go:825-841`), and `ListJobResults` (`postgres.go:844-892`) filter only `WHERE id=$1` /
   `WHERE w.job_id=$1`. No `caller_id` predicate exists in any of them.

4. **Auth proves *a* caller, never *the* caller.** `handleJobStatus` (`api.go:272`) and `handleJobResults`
   (`api.go:312`) call `s.authenticate(r, true)` — which correctly enforces the stronger per-caller-key
   requirement and correctly rejects the legacy static token on the PII surface — and then pass only the
   path's `jobID` to the store. The gate proves the caller holds *some* valid key; nothing checks the job is
   theirs.

5. **No RLS backstop exists.** Search for `row level security|FORCE ROW|ENABLE ROW` across all of M1's
   migrations and Go tree: **zero hits**, verified first-hand. `migrations/0005_grants.sql:112-113` records
   this as a deliberate Phase-1 decision: *"Phase 1 uses service-role key only (OQ-20); no RLS (single
   client = brain). RLS designed if tenant identities appear later."*

6. **Job ids are guessable-adjacent.** They are UUIDv7 (`idgen.NewV7String`, `queue/enqueue.go:70,88`) —
   time-ordered, not cryptographically random. A caller who knows roughly when another caller submitted has
   a materially reduced search space compared to a random v4.

**The contradiction that makes this a live defect rather than a documented design choice:** the `jobs`
comment's premise ("single client = brain", no tenant identities) was **already false when
`0010_api_callers.sql` shipped**. That migration introduces multiple distinct authenticated principals —
`api_callers(caller_id, key_sha256, not_before, not_after)`, with per-caller keys, rotation overlap, and a
tested lifecycle. Two premises coexist in the same schema: "there is only one client, so scoping is
unnecessary" and "there are many callers, each separately authenticated." The first premise is what leaves
the read paths unscoped; the second is what makes that exploitable.

**What is NOT wrong here, stated so the finding is not overread:** M1's auth is in several respects the
strongest of the three. It is the only module with a real per-caller identity in the database (M2 and M3
share one flat bearer-token set with no caller identity at all). It correctly requires a per-caller key —
refusing the legacy static token — on the bulk PII surface (`api.go:218,273,313`), and that refusal is
behaviourally tested (`bulk_test.go:95-115`, `auth_test.go:55-99`, including rotation overlap and
`not_before` enforcement). The gap is precisely that the identity it establishes is never used as a data
predicate.

**Recommendation (not applied):** add an owner column to `jobs`, populate it from the authenticated
`caller_id` at submit, and add the predicate to all three read paths — returning 404 (not 403) for
"exists but not yours", matching the senders' no-existence-disclosure posture. Sized in the fix plan.

---

## Section 3 — Category inventories (all three, from code)

Fifteen categories per the brief. Every row is traced to source. Weak-vs-strong evidence is annotated on
every three-way match.

### 3.1 Platform-facing routes

| | M1 | M2 | M3 |
|---|---|---|---|
| Core verbs | `POST /v1/verify`, `POST /v1/verify/bulk`, `GET /v1/jobs/{id}`, `GET /v1/jobs/{id}/results` | `POST /v1/quote`, `POST /v1/run`, `GET /v1/report`, `GET /v1/describe` | same four as M2 |
| Extension verbs | none | none | **17** under `/v1/byosmtp/*` |
| Registration | `mux.HandleFunc` inline, auth called *inside* each handler (`api.go:114-125`) | `mux.HandleFunc` inline, wrapped `s.requireAuth(...)` at registration (`server.go:56-60`) | **table-driven** slice → one mount loop (`server.go:140-152`) |
| Router | Go 1.22+ `http.ServeMux` method+path patterns | same | same |
| Strict decode | **no** — plain `json.Decode` | `DisallowUnknownFields()` (`handlers.go:302`) | `DisallowUnknownFields()` (`handlers.go:374`) |

M3's route registration is the strongest pattern of the three and is a **structural** auth guarantee rather
than a per-handler discipline: `extensionRoutes()` (`describe_routes.go`) is a single slice that is both
mounted onto the mux *and* rendered into `/v1/describe`'s `extension_routes` — a route cannot exist without
being described, and cannot be described without existing. Verified first-hand: the slice contains exactly
**17** entries. M2's decorator-at-registration is next strongest. M1's authenticate-inside-each-handler is
weakest: adding a route and forgetting the auth call is a silent, greppable-only omission, with no
structural or CI barrier.

M1's request/response shapes (verified in `api.go`, `verdict/verdict.go`):

- `POST /v1/verify` → `{email, policy?}`; 200 returns the frozen `verdict.Response` — `api_version, status,
  reason, confidence (0-100), recommendation, email, domain, flags, signals, meta{request_id, checked_at,
  latency_ms, policy, policy_version, verifier_ip, node_id, engine_version, config_fingerprint, from_cache},
  raw`. `Valid()` is asserted before every sync write (`api.go:178-182`), returning a defensive 500 rather
  than shipping an off-contract body. Body cap `1<<16`.
- `POST /v1/verify/bulk` → `{addresses[], policy?, caller_ref?}`; 202 returns
  `{job_id, status:"accepted", counts{total, enqueued, duplicate, malformed}}`. Body cap `16<<20`.
- `GET /v1/jobs/{id}` → `{job_id, status, complete, caller_ref, counts{total, settled, pending}}`, with
  `status` overridden to `"complete"` when the authoritative NOT-EXISTS completion check beats the stored
  counter (`api.go:296-299`).
- `GET /v1/jobs/{id}/results?cursor=&limit=` → `{job_id, results[verdict.Response], next_cursor}`. Results
  deliberately differ from the sync shape in two ways: `email` is always `""` (plaintext purged on settle)
  and `meta` carries only `policy`/`policy_version`/`from_cache`. Note `verdictFromRow` (`api.go:428-451`)
  sets `api_version` via `firstNonEmpty(row.APIVersion, "v1")` and — unlike the sync path — never calls
  `Valid()`, so a stored row with a stale version is silently coerced rather than caught.

M3's extension surface (17 routes, verified by direct extraction from the registry slice): accounts
create/bulk/list/get/update/delete/events, lifecycle disable/enable/pause/unpause, unquarantine, test,
test-send, settings GET/PUT, usage GET. All declared `Auth:"platform_service_token"`, `Scope:"user"`.
`quarantine`/`unquarantine` as *admin* verbs are deliberately absent from this surface and 404 (not 401)
there — asserted by the CI gate.

### 3.2 Error envelope — **STRUCTURAL divergence**

**M2 / M3** (`platformapi/errors.go`) — one shape, one serializer:

```go
type errorEnvelope struct { Error errorBody `json:"error"` }
type errorBody struct {
    Code      string        `json:"code"`
    Message   string        `json:"message"`
    Details   []fieldDetail `json:"details,omitempty"`
    RequestID string        `json:"request_id"`
}
```

`newErr` builds it; `writeError` is the only serializer; every handler ends its error path through the single
`s.fail` choke point. No handler in either sender hand-rolls a different shape. Internal reasons

> **CORRECTED (§0a.1).** True at the handler layer, but it does **not** make uniformity structural. All three
> modules register Go 1.22 method+path patterns, so a **method mismatch is answered by `net/http.ServeMux`
> before `requireAuth` runs** — `405`, `text/plain`, body `Method Not Allowed\n`, no envelope, no code, no
> `request_id`. An unmatched `/v1/*` path likewise returns text/plain `404 page not found`. Three shapes are
> reachable on each sender's platform surface, not one, and no CI gate probes the 405 because
> `contract_drift.sh:79-88` only ever uses the method-appropriate verb. **M1 shares this identically**, so it
> is a three-way finding (**D17**), not a divergence. The senders' handler-level discipline is still the
> strongest of the three — the correction is to the word *structural*, not to the comparative verdict.

(`job_item_not_found`, `user_suspended`, `panic_recovered`, …) are logged against the request id and never
serialized — the two-vocabulary rule, structurally enforced by there being exactly one writer.

Codes verified by direct enumeration:

| Code | HTTP | M2 | M3 |
|---|---|---|---|
| `validation_failed` | 400 | ✅ | ✅ |
| `unauthenticated` | 401 | ✅ | ✅ |
| `not_found` | 404 | ✅ | ✅ |
| `conflict` | 409 | ✅ | ✅ |
| `unprocessable` | 422 | ❌ | ✅ |
| `rate_limited` | 429 | ❌ | ✅ |
| `internal_error` | 500 | ✅ | ✅ |

M3 adds two codes additively (`errors.go`, diffed against M2 first-hand). This is legitimate additive
evolution *within* a module — but it means **a platform client written against M2's five-code set will
receive 422 and 429 from M3**. A client that maps unknown codes to a generic failure copes; one that
switches exhaustively does not. This belongs in the canonical contract as "clients MUST tolerate unknown
codes," and is recorded as a real integration constraint rather than a defect in either module.

**M1** — no envelope exists. There is no shared error constructor; every handler hand-rolls a map literal
passed to `writeJSON` (`api.go:460-464`). The dominant shape at ~25 call sites is:

```json
{"error": "<free-text lowercase english>"}
```

with literals such as `"unauthorized"`, `"service paused"`, `"email is required"`, `"not ready"`,
`"unknown policy"`, `"verification failed"`, `"internal contract violation"`, `"invalid request body"`,
`"addresses is required and must be non-empty"`, `"bulk submit failed"`, `"job read failed"`,
`"job not found"`, `"job status failed"`, `"job results failed"`.

Three concrete consequences, each verified:

1. **No machine-readable codes at all.** A client must string-match English prose to distinguish failures.
   There is no `code` field and no frozen error-code enum comparable to M1's own — otherwise
   exemplary — `vocab.Status`/`Reason`/`Recommendation` sets.
2. **No `request_id` in any error body.** Confirmed by reading every `writeJSON` call site in `api.go`: none
   passes a request id. So a client-reported M1 failure **cannot be correlated to a server-side log line**.
   M1 does mint request ids and returns one in the *success* path's `meta.request_id`, and stores it on the
   verification row — but the error path drops it. Operationally this is the single most expensive small gap
   in the audit: it makes support triage of a reported error guesswork.
3. **A second, divergent error shape on one route** (`api.go:242-244`) — the bulk over-cap 413 alone
   returns three keys instead of one:
   ```json
   {"error": "too many addresses", "max": 100000, "got": 250000}
   ```
   A client parsing `{error: string}` uniformly still works here, but the shape is genuinely
   non-uniform *within M1's own surface*, which is worth noting because it shows the absence of a choke
   point, not merely the absence of codes.

This is the clearest STRUCTURAL divergence after the verb set. It is also the cheapest major one to fix,
because M1's internal error information already exists — it is simply not shaped or correlated.

### 3.3 Auth

| | M1 | M2 | M3 |
|---|---|---|---|
| Header | `Authorization` | `Authorization` | `Authorization` |
| Scheme | ~~`Bearer <t>` **or bare token** (`trimBearer`, `helpers.go:20-28`)~~ → **`Bearer` only, case-insensitive** (`bearerToken`, `helpers.go:34-41`; fixed `d481c97`) | `Bearer` only, case-insensitive | `Bearer` only, case-insensitive |
| Credential | per-caller key (DB) + legacy static token (env) + dev-open | one flat bearer set (env) | one flat bearer set (env) |
| Storage | `sha256` digest in `api_callers.key_sha256` | **bcrypt** hashes, `PLATFORM_TOKEN_HASHES` | **bcrypt** hashes, `PLATFORM_TOKEN_HASHES` |
| Compare | indexed SQL equality on digest; static token via `subtle.ConstantTimeCompare` (`api.go:402`) | all hashes, no early return (`auth.go:37-48`) | byte-identical to M2 |
| Caller identity | **yes** — `caller_id` resolved per key | **no** | **no** |
| Unconfigured | dev-open only when `Env=="development"` and nothing configured | fail-closed | fail-closed, **surface unmounted entirely** (`main.go:272`) |
| Middleware | called inside each handler | `requireAuth` decorator + panic-recover | same as M2 |

M2↔M3 `auth.go` is **byte-identical** (`diff` → no output). **Weak evidence: inheritance, not validation.**

Three genuine divergences worth the platform's attention:

- ~~**M1 accepts a bare token with no scheme.**~~ **FIXED 2026-08-07 (`coldmail-verify@d481c97`); this row
  is now a convergence.** `trimBearer` stripped an optional `"Bearer "` prefix and otherwise returned the
  header verbatim, so a missing scheme was not rejected. **The audit found one direction and missed the
  other:** M1 was ALSO the only module rejecting `bearer <t>` lower-case, a request RFC 9110 §11.1 makes
  conforming and both senders accept (`strings.EqualFold`). So the divergence was two-way, and a fix that
  only closed the laxness would have hardened the second half into a permanent asymmetry. `bearerToken`
  closes both. Recorded because "M1 is more permissive here" was the wrong frame — it was more permissive
  about the token and stricter about the scheme, and only reading both senders' code showed the second.
- **M1 has caller identity; the senders do not.** M1 resolves a `caller_id` per key with validity windows and
  rotation overlap. The senders have a flat token set with no per-caller notion at all — tenancy travels
  entirely in the request body/query as `user_id`, explicitly never derived from the credential. **M1's model
  is the better one** and, ironically, the module with real caller identity is the one that fails to use it
  for scoping (Section 2), while the modules with no caller identity scope correctly on every query.
- **M1's fallthrough is a real weakness.** A per-caller lookup miss falls through to the legacy static token
  comparison (`api.go:399-407`), so a wrong/revoked per-caller key can still authenticate a sync call if a
  static token happens to be configured. Bounded — `requirePerCaller=true` on the bulk and job-read routes
  rejects the static token outright — but it means revoking a per-caller key does not fully revoke access on
  the sync surface while a static token exists.

### 3.4 Versioning

All three use a `/v1/` URL prefix and none reads a version header. **This is the one strong three-way
match in the audit** (M1↔M2 independent agreement, M3 inheriting) and should be locked into the canonical
contract as-is.

Enforcement differs sharply:

- **M1** — enforced in code, and in the strongest form found anywhere in the three repos *for its response
  body*: `const APIVersion = "v1"` (`verdict.go:13`) plus `Valid()` asserting `r.APIVersion == APIVersion`
  before every sync write. Additive-only vocabularies are double-enforced — the `vocab` guard sets
  (`vocab.go:79-102`) **and** mirrored DB `CHECK` constraints (`migrations/0001_verifications.sql:61-79`),
  with later widenings done as explicit supersets (`0008_rate_budgets_recovering.sql:24-32` comments *"The
  CHECK is REPLACED with a superset (adds recovering); every previously-valid value stays"*). No
  route-surface drift gate exists.
- **M2 / M3** — enforced by CI probe: `contract_drift.sh` asserts every documented route returns 401
  unauthenticated (proving mounted-and-gated), an undocumented path returns 404, and each served path
  literally appears in the contract doc. M3 adds the structural registry guarantee (§3.1). No in-code
  response-version constant comparable to M1's `Valid()`.

Each module has half the mechanism the other has. The canonical contract should require both: M1's
in-code response-shape assertion and the senders' served-set-equals-documented-set gate.

### 3.5 Tenant scoping — **STRUCTURAL divergence**

| | M1 | M2 | M3 |
|---|---|---|---|
| Tenant field | none on data rows | `user_id` (UUID) | `user_id` (UUID) |
| Source | — | caller-supplied, body or query | caller-supplied, body or query |
| Identity origin | own `api_callers.caller_id` (text) | platform's `public.users.id` | platform's `public.users.id` |
| Enforcement | **none on reads** | every query carries `user_id = $N` | every query carries `user_id = $N` |
| Cross-tenant | full read access (§2) | 404, identical to nonexistent | 404, identical to nonexistent |
| RLS | **none** | ENABLEd 8 tables, FORCEd on `accounts` | ENABLEd 13 tables, FORCEd on `accounts` |

M2↔M3 agreement here is inheritance — **weak evidence** — but the property is independently proven by
behavioural tests in each (§3.15), which raises confidence above pure inheritance.

The senders' pattern, verified in the source: explicit selector plus 404-not-403. Representative M3 queries —
`getAccount`: `WHERE a.id=$1 AND a.user_id=$2 AND a.deleted_at IS NULL` (`account_store.go:182-184`);
report: `WHERE j.id = $1 AND j.user_id = $2` (`store.go:273-286`); job creation:
`WHERE id = $1 AND user_id = $2` (`store.go:84-86`). Account events are scoped by the event row's *own*
stamped `user_id` (`account_events.go:46`), not by joining through the account — so a purged account cannot
leak cross-tenant. M3's `accountUpdateReq` deliberately has **no** `user_id` field
(`account_update.go:12-16`), closing an owner-reassignment hole structurally (via
`DisallowUnknownFields`) rather than by a runtime check that could be forgotten.

Both senders return the **same 404 for "another tenant's" as for "nonexistent"** — no existence disclosure —
and both assert the two 404s are byte-identical in code, which is the anti-vacuity control that makes the
test meaningful. `campaign_id` is a deliberate exception in both: foreign-or-not-yet-run yields a clean empty
aggregate (`total:0`) rather than 404, because `campaign_id` is platform-minted rather than
module-allocated.

RLS in both senders is explicitly a defense-in-depth backstop against a future misgrant, not the isolation
mechanism: `byosmtp_module` holds a permissive `USING(true) WITH CHECK(true)` policy. Tenant isolation is a
query-layer responsibility in both. Verified first-hand that M3 extended RLS to all five of its new tables
at their creation migrations (`user_settings` 0029, `user_config_overrides` 0030, `user_suspensions` 0031,
`campaigns` 0032, `notifications` 0033) — the discipline held across the fork rather than decaying, which is
worth recording as a positive.

### 3.6 Status / state vocabulary — **DELIBERATE divergence (domains differ)**

The domains genuinely differ, so different vocabularies are correct. What matters for the platform is
whether each is *frozen and discoverable*, and there the modules diverge.

- **M1** — verdict vocabulary, frozen, DB-mirrored: `Status` = `valid|invalid|risky|unknown`;
  `Recommendation` = `send|review|skip`; ~30 `Reason` values grouped by the phase that first emits them,
  with later phases' values **pre-reserved from Phase 1** so additions are additive by construction.
  A composability invariant is enforced in code (`vocab.ComposabilityOK`, `vocab.go:108-119`):
  `invalid⇒{skip}`, `valid⇒{send,review}`, `risky⇒{review,skip}`, `unknown⇒{review,skip}` — so
  `invalid+send` is structurally impossible. Job status: `submitted|running|complete|complete_with_unknowns`.
  **This is the best-designed vocabulary of the three** and the canonical contract should adopt its
  pre-reservation and composability-invariant patterns.
- **M2 / M3** — Postgres enums, additive-only: `job_state` =
  `queued|assigned|sending|sent|failed|bounced` (identical in both). M3 adds seven enums for its own
  domain (`quarantine_reason`, `from_verification_state`, `dmarc_policy`, `suspend_reason`,
  `notification_severity`, `notification_state`, `credential_verification_state`).
  In both, `waiting_for_capacity` is a campaign-report **aggregate** only, never a per-job state — a job
  waiting for capacity reads `state:"queued"`, never `failed`.

**No module exposes its vocabulary programmatically.** M3's `/v1/describe` returns capabilities, pricing,
extension routes, and schemas — but not the enum value sets. So a UI must hardcode all three vocabularies
from documentation. That is a GAP against the "UI without a single question" test, and it applies to all
three modules equally.

### 3.7 Cost / feasibility — **STRUCTURAL divergence, three different answers**

Three modules, three incompatible models. This is the second-largest structural finding.

- **M1** — nothing. No verb, no field, no dry-run. Cost is learned only by submitting real work.
- **M2** — `POST /v1/quote` is a per-recipient cost multiplier: `{recipient_count}` →
  `{recipient_count, cost, currency:"credits", unit_price_credits, tier:"shared"}`. Pure, side-effect-free,
  proven zero-job-created (`quote_test.go:36-42`). **No feasibility check at all** — it has no tenant context
  to check against.
- **M3** — the same route is a genuine **feasibility** check, deliberately rewritten because M3's billing is
  a flat monthly subscription with no per-send charge. Returns `feasible`, `reasons[{code,limit,requested,
  cap_name}]`, `caps{queue_depth_cap,daily_send_cap,account_count_cap}`,
  `capacity{eligible_accounts,daily_capacity_remaining,effective_concurrency,estimated_duration_sec,
  estimate_basis}`, `warnings[{code,estimated_duration_sec,threshold_sec}]`. The cost fields are retained
  for contract compatibility but are **inert**: `unit_price_credits` is seeded `0` by
  `byosmtp_0048_unit_price_zero.up.sql`, so `cost` is always 0. Reason codes: `dispatch_disabled`,
  `exceeds_queue_cap`, `no_eligible_accounts`, `all_accounts_unverified`, `from_gate_blocked` — only the
  first binding reason is reported. `feasible` deliberately means "can this be accepted as submitted", never
  "will this be fast": a campaign exceeding today's capacity is feasible because it drains over days.

**The irreconcilability, verified first-hand and load-bearing for C2:**

```go
// M2 handlers.go:49-51
type quoteReq struct { RecipientCount *int `json:"recipient_count"` }

// M3 handlers.go:52-55
type quoteReq struct {
    RecipientCount *int   `json:"recipient_count"`
    UserID         string `json:"user_id"`  // REQUIRED
}
```

Both decode with `DisallowUnknownFields()` (M2 `handlers.go:302`, M3 `handlers.go:374`). Therefore:

- `{"recipient_count":100}` → **M2 OK; M3 rejects** (`user_id` required).
- `{"recipient_count":100,"user_id":"<uuid>"}` → **M3 OK; M2 rejects** (unknown field).

**There is no single quote request body that both senders accept.** Not a naming difference, not a
cosmetic one: strict decoding makes it a hard incompatibility between two modules that share the same
route on the same version. Sized in the fix plan; it is cheap to fix (M2 accepting an optional `user_id`)
and it BLOCKS-PLATFORM-INTEGRATION until then.

Both senders' `currency` is the hardcoded literal `"credits"` — an abstract integer unit, not ISO-4217, with
no decimal handling anywhere. If real money ever crosses this boundary, that is a contract change in both.

### 3.8 Admin surface

| | M1 | M2 | M3 |
|---|---|---|---|
| HTTP admin routes | **0** | **21** | **21** |
| Auth | n/a (direct SQL as `cp_admin`) | separate bcrypt set, `ADMIN_API_KEY_HASHES` | separate bcrypt set, `ADMIN_API_KEY_HASHES` |
| Credential sharing | n/a | never shared with platform token | never shared with platform token |
| Registration | n/a | inline `s.requireAuth(...)` per route | table-driven `adminRoutes()` slice |

Counts verified first-hand by direct enumeration (see §0 for the subagent's miscount and the gates' log-string
off-by-one). M2's 21 include five pool routes; M3 dropped those at G2 and added six of its own
(`users/{id}/suspend|unsuspend`, `users/{id}/config` GET/PUT, `config/{dial}` DELETE) — arriving at the same
total by a different composition.

Both senders keep Domain A (platform service token) and Domain B (operator key) structurally separate: two
distinct `Authenticator` types, two env vars, two constructor call sites, and no import between the packages.

**The admin bypass surface is a real parallel write path** in both senders, and a standing hazard the repos
themselves flag: admin routes write the same `accounts` rows the platform surface writes, via independent
handler implementations that must both honour the same preconditions. M3's own operating rules state gates
belong in the store layer "where all writers pass," and that this has bitten three times. **For the
platform's purposes the relevant fact is narrower:** an operator action taken through the admin surface
changes state a platform client is concurrently reading, with no notification channel to the platform. See
§3.9 on M3's notifications being write-only.

**M1's zero-admin-surface is a genuine operability gap, not a simplicity win.** Issuing or rotating a
platform caller key requires an operator to run SQL against production as `cp_admin`. There is no HTTP path,
and no CLI tool exists in the repo (verified: exactly one `func main`). That is a real operational burden and
an audit-trail weakness relative to the senders' audited admin routes.

### 3.9 Health / observability — **STRUCTURAL divergence in shape**

| | M1 | M2 | M3 |
|---|---|---|---|
| `/healthz` | `{"status":"ok"}` | `{"status":"ok","at":<ts>}` | `{"status":"ok","at":<ts>}` |
| `/readyz` | `{ready, config, pepper, supabaseKey}` | `{status, db, singleton_lock, at}` | `{status, db, singleton_lock, at}` |
| Ready checks | pepper loaded only | DB ping **and** singleton lock | DB ping **and** singleton lock |
| Operator health | none | `GET /v1/admin/health` | `GET /v1/admin/health` + per-user headroom + destination throttles |
| `/metrics` | **none** | **none** | **none** |
| Logging | stdlib `log`, plain text, **no request id** | stdlib `log`, plain text, request id on every line | same as M2 |

**No two modules agree on the readiness body.** Not one field name is shared: M1 reports `ready` (bool),
`config` (a free-text summary string), `pepper`, `supabaseKey`; the senders report `status` (string), `db`,
`singleton_lock`, `at`. A platform health dashboard needs three separate parsers today. This is cosmetic in
difficulty and structural in effect — cheap to fix, but genuinely blocking uniform monitoring until fixed.

Two substantive differences behind the shape:

- **M1's readiness is materially weaker.** It checks only that the pepper secret loaded — not the DB, not
  the verifier wiring. So M1 can report `ready:true` while `POST /v1/verify` itself returns 503 for a
  missing verifier or store (`api.go:157-160`). Readiness and actual serving capability are **different
  tests** in M1, which means an orchestrator's readiness gate does not reflect whether the module can serve.
  The senders' readiness checks both real dependencies (DB ping and genuine singleton-lock holding).
- **No module exposes `/metrics`.** Verified all three: no Prometheus endpoint, no exporter, no metrics
  library in any `go.mod`. M1 has an `internal/metrics` package computing block rate, unknown rate, cache
  hit rate, and latency p50/95/99 — but it is a pure in-process library with **no HTTP exposure and no
  caller wiring it to a handler**. The senders expose a rich custom JSON snapshot at `/v1/admin/health`
  instead. So all three are monitorable only by bespoke polling — a uniform GAP, equally applicable to all,
  and one the platform must decide on rather than discover.

**M1 emits no request id in any log line.** Verified across `main.go` and `api.go`: request ids exist in the
success response `meta` and on the stored verification row, but are never logged. Combined with §3.2's
finding that error bodies carry no request id either, **an M1 error reported by a user is correlatable to
neither a log line nor a response identifier.** Both senders log `rid=` on every line and echo the same id
in every error body.

**M3 notifications are write-only from a client's perspective.** The `notifications` table (migrations 0033,
0047) has two INSERT sites and is read only by `dispatch/notify.go` for webhook egress. No platform route
and no admin route reads it. So the module records user-actionable events that a UI **cannot fetch** — the
data exists and is unreachable. Given M3's stated principle that users are "notified about everything
actionable," this is a GAP against the module's own design intent, and a hard blocker for building the
notification surface of a UI.

### 3.10 DB boundary

| | M1 | M2 | M3 |
|---|---|---|---|
| Dedicated schema | **no** (bare `public`) | `sender` | `byosmtp` |
| Owner role | `cp_admin` (NOLOGIN) | `sender_owner` (NOLOGIN, owns schema) | `byosmtp_owner` (NOLOGIN, owns schema) |
| Runtime role | `cp_runtime` (NOLOGIN; Supabase service_role in practice) | `sender_module` (LOGIN) | `byosmtp_module` (LOGIN) |
| Grants | per-table, enumerated | per-table, enumerated | per-table, enumerated |
| Append-only | by grant (`REVOKE UPDATE,DELETE` on `verifications`) | by grant (`job_events`: SELECT+INSERT) | same as M2 |
| RLS | **none** | 8 tables + FORCE on `accounts` | 13 tables + FORCE on `accounts` |
| Worker DB access | none (Ruby node ships no DB driver) | none (box `dependencies:{}`) | none (box ships no DB driver) |
| Platform tables | **references none** | SELECT-only on `public.users`, `public.job_items` | same as M2 |

**Strong three-way agreement (M1↔M2 independent) on two properties**, both of which should be locked into the
canonical contract:

1. **Per-table enumerated grants, never blanket default privileges.** All three do this. The senders document
   why (ADR-0008's trap: default privileges key on the creator and do not fire for a table an admin created
   then reowned).
2. **The worker holds no DB credential, structurally.** All three enforce this by shipping no driver in the
   worker rather than by configuration. Independently arrived at by M1 and M2 — the strongest architectural
   agreement in the audit.

**M1's lack of a dedicated schema is a real divergence.** Its tables are created unqualified
(`CREATE TABLE IF NOT EXISTS verifications`), landing in whatever `search_path` yields — `public` in
practice. Both senders own a named schema with `REVOKE ALL ON SCHEMA … FROM PUBLIC`. Since the senders'
own FKs point into `public`, M1's unqualified tables share the namespace the platform owns and the
senders read. Verified M1 table list — **21 base tables**, recounted 2026-08-07: `api_callers, cache,
calibration, catchall_domains, disposable_domains, external_calls, jobs, node_audits, node_warmup, nodes,
outcome_ingest_cursor, policies, rate_budgets, recalibration_runs, send_outcomes, spam_trap_domains,
verification_outcomes, verification_raw, verifications, work_item_attempts, work_items`, **plus 3 monthly
partitions** of `verification_raw` (`verification_raw_2026_07|_08|_09`, `0001_verifications.sql:124-130`),
so 24 relations in `public` in total.

**The "22" this paragraph used to claim was never right, and it contradicted the list printed immediately
below it** — which has always had 21 names. The likely origin is a `grep -c 'CREATE TABLE'` that counted
the truncated partition prefix `verification_raw_` as a 22nd distinct table. Recorded rather than silently
edited: a count stated next to the enumeration that disproves it is a small instance of the shape this
audit exists to find, and it survived because nobody counted the list. **`public.jobs` and `public.users` are exactly the kind of names a platform
is likely to want** — a live collision risk in a shared database, and the reason this is classified
structural rather than cosmetic.

**The unverified platform coupling (both senders, identical):**

```sql
-- byosmtp_0000_bootstrap.up.sql:49-50  (sender_0000 identical)
GRANT SELECT ON public.users     TO byosmtp_module;
GRANT SELECT ON public.job_items TO byosmtp_module;
```

No migration in either sender creates these tables — they are assumed to pre-exist, with a preflight script
checking **existence only, deliberately not shape** (`db/preflight/check_platform_tables.sql`). The assumed
shapes are recorded in that script's comments as needing confirmation:

```
public.users(id uuid pk, email, is_active, created_at)
public.job_items(id uuid pk, campaign_id, user_id, recipient_email,
                 rendered_subject, rendered_body, reply_to, status, created_at)
```

The senders' actual code depends on more than existence — M3 reads
`ji.id, ji.campaign_id, ji.user_id, ji.recipient_email, ji.rendered_subject, ji.rendered_body, ji.reply_to`
(`platformapi/store.go:145-152`), i.e. **seven columns of `job_items` by name**. And the coupling is harder
than a read: verified **seven** hard FKs to `public.users(id)` across migrations 0004/0005/0006/0008/0009/
0014/0027 (six `ON DELETE CASCADE`, one `ON DELETE RESTRICT` at 0027) plus one to `public.job_items(id)`
(0007:45). Neither module writes to either table (verified: only test fixtures do).

**Two consequences the platform must own, both registered in the assumptions register:**
- A `public.users` row deletion **cascades into module data** through six FKs — and is *refused outright* by
  the one `RESTRICT` FK on `accounts`. So platform user-deletion semantics are partly dictated by module
  migrations, inconsistently between them.
- A platform rename or type change on any of those eight referenced columns **breaks both senders'
  migrations and runtime**, and the preflight will not catch it because it checks existence only.

M1 references neither table and has no `users` table of its own. **So the three modules do not share a
tenant identity**: the senders key on the platform's `users.id` UUID, M1 on its own `api_callers.caller_id`
text. Any platform-level per-user view spanning all three must map between two identity spaces.

### 3.11 Config approach — **STRUCTURAL divergence**

| | M1 | M2 | M3 |
|---|---|---|---|
| Mechanism | **env vars only** (`TM_*`) | DB rows, `sender.config` | DB rows, `byosmtp.config` |
| Encoding | strings parsed at load | JSONB `{"v":…}` | JSONB `{"v":…}` |
| Count | ~50 dials + switch structs (60+ settings) | **53** keys | **87** keys |
| Hot-reload | **none** — restart required | yes, read fresh per tick/request | yes, read fresh per tick/request |
| Drift gate | none | `config_drift` (blocking) | `config_drift` (blocking) |
| Consumer gate | none | `config_consumption` (blocking) | `config_consumption` (blocking) |

M1's `Dial{Value int; Set bool}` wrapper (`config.go:106-116`) is a genuinely good pattern the senders lack:
it distinguishes "unset" from "explicitly zero", and consuming code returns a do-nothing zero rather than
inventing a value. Worth adopting.

The operational difference is what matters to the platform: **retuning M1 requires a restart; retuning either
sender does not.** Under load, that is the difference between a config change and a rolling deployment.

**Hardcoded tuning literals found in all three** (a self-declared rule violation in the senders' own terms;
recorded, not fixed):

- **M1** — `maxBulkAddresses = 100_000` (`api.go:200`, fallback when the dial is unset); body caps `1<<16`
  (`api.go:149`) and `16<<20` (`api.go:233`); results pagination default `100` / clamp `1000`
  (`api.go:330-335`); `15 * time.Second` node client deadline (`node/http_client.go:32`);
  `ReadHeaderTimeout: 5 * time.Second` (`main.go:73`); `maxSMTP = 60 * time.Second` (`main.go:214`);
  several `io.LimitReader` caps in `node/*.go`. None has a `TM_*` var or `Dials` field.
- **M2** — `defaultQuoteUnitCredits=1` and `defaultQuoteTier="shared"` (fallbacks); and genuinely
  behaviour-governing: `defaultPageSize=100` / `maxPageSize=200` in `parseReportPageSize`
  (`campaign_report.go:322-323`), which are **not** backed by a seeded row.
- **M3** — `dailyLimitCeiling = 100_000` (flagged in-code as a sanity bound, not a dial); several
  fallbacks mirroring seeded defaults; and a bare `10*time.Second` DB-operation timeout
  (`server.go:156-158`, `adminapi/server.go:170-172`) with no config key — the clearest genuine violation
  of the three senders' own rule, since every comparable SMTP/dispatch timeout *is* a key.

Note the pagination literals matter to the platform directly: `page_size` behaviour on `/v1/report` is
governed by non-configurable constants that differ from anything documented as a dial.

### 3.12 Migration conventions — **STRUCTURAL divergence**

| | M1 | M2 | M3 |
|---|---|---|---|
| Naming | `NNNN_desc.sql` | `sender_NNNN_desc.{up,down}.sql` | `byosmtp_NNNN_desc.{up,down}.sql` |
| Down migrations | **none — forward-only** | required, paired | required, paired |
| Count | 12 files | 50 files (25 pairs) | 98 files (49 pairs) |
| Runner | `sort` + `psql -f` shell loop | custom Go binary | custom Go binary |
| Tracking table | **none** | `sender.schema_migrations` (self-inserting) | `byosmtp.schema_migrations` (self-inserting) |
| Lint gate | **none** | `migration_lint` (blocking) | `migration_lint` (blocking) |

M1's runner is literally `for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql | sort); do psql … -f "$f"; done`
(`.github/workflows/ci.yml:44-51`) with `ON_ERROR_STOP=1`. **With no tracking table, re-application safety
rests entirely on each file's own `IF NOT EXISTS` / idempotent `DO $$` guards.** Verified there is no
`schema_migrations`-equivalent in any M1 migration. Forward-only is a legitimate choice; *unledgered* is a
real operational risk, because nothing but convention prevents partial or repeated application, and nothing
records what has run.

The senders' `migration_lint` enforces filename pattern, up/down pairing, chain contiguity, that each file
self-records its own version, that each down either deletes that row or documents a forward-only no-op, and
that the newest version appears in a test's expected-chain literal. M1 has no equivalent check of any kind.

### 3.13 Kill switches and shadow modes — **M1 is materially ahead**

**M1 — a genuine graduated ladder** (`config/config.go:32-76`), four rungs, all config-driven and validated
at load:

| Rung | Field | Values | Default |
|---|---|---|---|
| 1 (SMTP) | `KillSwitch.SMTP` | `enabled` / `disabled` | **disabled** |
| 2 (fallback) | `KillSwitch.Fallback` | `enabled` / `off` | **off** |
| 3 (cheap-layers-only) | `CheapLayersOnly()` — **derived** | SMTP off ∧ fallback off | **the steady state** |
| 4 (front door) | `KillSwitch.FrontDoor` | `serving` / `accept_and_defer` / `reject` | serving |

Plus four per-mutator shadow dials (`shadow` / `recommend_only` / `auto`), **all defaulting to
`recommend_only`**, and three separate opt-in flags each requiring the literal string `"true"`
(`FlywheelGateEnabled`, `FlywheelCalibrationEnabled`, `ExternalDPAConfirmed`). The external fallback layer
requires **both** the rung-2 switch **and** the DPA flag **and** a vendor key before it can go live — a
genuine "enabling is hard" design. `Validate()` rejects illegal mode strings and unsafe numeric combinations
(e.g. `FleetWidePct` must be 1..99 or the fleet-wide brake becomes mathematically unable to fire).

**The senders** have five independent booleans plus one three-value dial:
`dispatch_enabled` (true), `autoquarantine_mode` (`off|shadow|enforce`, default **shadow**, with an
unrecognised value defensively falling back to `shadow` and never `enforce`), `autopause_enabled` (true),
`variation_enabled` (true), `alert_webhook_url` (`""` = off). M3 adds `notifications_enabled`,
`from_gate_mode` (fail-closed to `enforce` on read error), `quote_capacity_projection_enabled`, and
`brain_singleton_reacquire_interval_sec`.

**Assessment the brief asked for — should the senders adopt M1's ladder?**

**Partly, and the distinction matters.** The senders' single `dispatch_enabled` is binary: dispatch runs or
it stops. There is no intermediate rung — no "finish in-flight but accept nothing new", no "serve reads but
refuse new work", no "accept and defer". Under a partial incident (one provider degrading, or a bad config
suspected) the only tool is a full stop. That is a real operational gap and the ladder shape is the right
answer to it.

Two qualifications, both from the code:

1. The senders are **not** starting from nothing. `autoquarantine_mode`'s `off|shadow|enforce` is a genuine
   three-rung ladder, shadow-by-default, with a fail-safe on unrecognised values. The gap is that the ladder
   discipline was applied to the *autonomous account-scoring* subsystem and not to *dispatch* itself.
2. M1's ladder is **not fully wired**, and this is a real defect I confirmed independently:

> **M1 defect — `accept_and_defer` validates but has no consumer.** `FrontDoorDefer FrontDoorMode =
> "accept_and_defer"` (`config.go:59`) passes `Validate()` (`config.go:750-753`). Exhaustive grep for
> `KillSwitch.FrontDoor` reads: `api.go:144` (sync verify) and `api.go:223` (bulk submit) — **both compare
> only to `FrontDoorReject`** — plus the config package's own set/validate/summary sites. So setting
> `TM_FRONTDOOR_MODE=accept_and_defer` **validates successfully and then behaves exactly like `serving`.**
> An operator reaching for the middle rung during an incident gets full serving with no error and no warning.
> A silently-inert safety control is worse than an absent one, because it will be trusted.

So the recommendation is: the senders should adopt the *ladder shape* for `dispatch_enabled` (graduated
degradation rather than binary halt), and M1 should either wire `accept_and_defer` or reject it at load.
Neither module currently has a complete ladder — M1 has the design without the wiring, the senders have the
wiring without the design.

M1's **shadow-mode-by-default from day one** is unambiguously ahead and worth adopting wholesale: four
mutators all defaulting to `recommend_only`, with the recommend-only path proven to produce
`Action{Apply:false, Recommend:…}` and zero live mutation (`health/decide.go:241-248`). The senders
retrofitted one shadow dial late.

### 3.14 Logging discipline

All three log via stdlib `log` in plain text — no JSON, no structured logger (verified: no zap/zerolog/logrus
in any `go.mod`). **Strong three-way agreement** — and arguably all three are equally wrong for a platform
that will want to aggregate logs across modules.

Redaction discipline is real in all three. M1's `Secrets.String()` (`secrets.go:193-199`) renders only
`<set>`/`<unset>` and counts (never values); every reviewed call site logs presence or an env var *name*,
never a value. The senders state the rule in code and enforce it in CI.

**CI enforcement differs sharply — this is a GAP, not a difference:**

- **M1** — ~~no automated log-scanning gate of any kind exists~~ **BOTH HALVES OF THIS ARE NOW OUT OF DATE
  (2026-08-07), and the residual risk resolved the WRONG WAY from what this paragraph guessed.**
  `scripts/check-no-secret-leak.sh` now exists and is wired at `ci.yml:145`: it boots the real binary with
  canary secrets and scans the process log, rather than grepping source — because the leak had no
  credential-looking token in the source at all. **The "unverified residual risk" was a REAL LEAK.** pgx
  v5.10.0 embeds the whole connection string, password included, in its parse error, so
  `log.Printf("%v", err)` on the store-open path printed it. Fixed at the source in `29e6204`
  (`store.OpenPG` wraps both returns in `scrubDSNError`). **The hedge "pgx v5 typically does not embed the
  password" was wrong, and it was wrong in the direction that reads as reassurance** — worth recording,
  because that sentence is why the item sat at LOW/unverified for a whole audit. **The same defect is
  UNFIXED in both senders at pgx v5.7.4 (fix-plan N1).**
- **M2** — `no_secret_leak.sh` (blocking): greps running service logs for the dev box token plaintext and its
  bcrypt hash. Explicitly scoped to the box token; the SMTP-credential scan lives separately in the send
  harness.
- **M3** — the same gate **plus** `user_output_scrub.sh` (blocking), a static source-grep with four
  independent checks and an anti-vacuity companion: every read of `jobs.last_smtp_response` must go through
  its single named accessor; `jobs.last_error_message` (the internal-slug-carrying column) must have **zero**
  readers in `platformapi/`; the notification render path must draw from the accessor; and no unledgered
  consumer of `boxproto.Report.SMTPResponse` may exist outside a ledger file. **This is the strongest
  two-vocabulary enforcement in the three repos** and the pattern the canonical contract should require —
  it is the only gate that mechanically prevents an internal reason string from reaching a client, rather
  than trusting handlers to route through the right helper.

### 3.15 Tests

| | M1 | M2 | M3 |
|---|---|---|---|
| Count | **339** `func Test`, 15 `t.Skip` | real-infra harness | real-infra harness |
| Platform-boundary style | in-process `httptest` + interface fakes | **real** cloned PG + real HTTP | **real** cloned PG + real HTTP |
| Store-layer style | **real** PG, gated on `TM_TEST_DATABASE_URL` | real PG | real PG |
| CI infra | real `postgres:16-alpine` service container | docker-compose harness | docker-compose harness |
| Blocking gates | build/vet/test, door5-gem-unmodified, RSpec, docs-discipline | **6** | **7** |

**M1's suite is substantial and honest, and must not be mischaracterised.** 339 real tests; only 15 skips,
of which 5 are GAP scaffolds in `control-plane/test/gaps/`. Its real-Postgres tests skip explicitly rather
than fake-pass — `t.Skip("PENDING: set TM_TEST_DATABASE_URL to a migrated Postgres to exercise the real
store (never faked)")` — **and CI actually sets that variable**, spinning a real Postgres, applying every
migration via psql, then running `go test ./...`. So the PG-backed tests genuinely run in CI. Any
characterisation of M1 as having a hollow or mock-based suite would be wrong.

M1's paired-control discipline is in one respect the **best of the three**: `auth_test.go:50-54` carries an
explicit `FAIL-VERIFY` comment naming the exact mutation that breaks the property — *"remove the now in
[not_before, not_after) window from LookupCaller → the rotated-out case starts authenticating → this test
fails"* and *"make the bulk path call authenticate(r, false) instead of true → the static-token-rejected-on-bulk
case fails"* — then asserts positive and negative cases in the same function.
`TestVerifyInvalidNeverSaysSend` similarly asserts both the classification and the forbidden combination.

The real difference is **where** the boundary is exercised. M1's *route-level* tests inject `Verifier`,
`CallerAuth`, `BulkSubmitter`, `JobReader` interfaces and use an in-memory store (`store.NewMem`), so the
platform-boundary contract is validated against fakes while the store is validated separately against real
PG. Both senders drive the **actual production `Server`** over real HTTP against a **freshly cloned migrated
database, connected as the real least-privileged runtime role** — so their platform-boundary tests would
catch a grant error, an RLS misconfiguration, or a query-level tenancy bug. M1's route tests would not.

That difference is exactly why M1's cross-tenant gap (§2) is untested rather than caught: with a fake store,
there is no query to be missing a `caller_id` predicate from.

Both senders prove tenancy behaviourally with the anti-vacuity control that makes it meaningful — the
cross-tenant 404's `error.code` is asserted **byte-identical** to a genuinely-nonexistent id's 404, and the
cross-tenant write test queries the DB directly to assert **zero** rows were created. One M3 uncertainty is
carried forward honestly: the inventory agent confirmed `account_cap_race_test.go` and
`account_unquarantine_race_test.go` exist and are wired into the harness, but **did not read their bodies**,
so whether they satisfy the repo's own scoped-`pg_stat_activity` contention proof is **not verified here**.

---

## Section 4 — Category-by-category convergence summary

Classification of every category, with the evidence weight stated. Full per-difference classification
(IDENTICAL / DELIBERATE / COSMETIC / STRUCTURAL / GAP) and the one-integration verdict are C2.

| # | Category | M1 vs M2/M3 | Evidence weight |
|---|---|---|---|
| 1 | Platform routes | **STRUCTURAL** — disjoint verb sets | M1↔M2 disagree: strong signal |
| 2 | Error envelope | **STRUCTURAL** — no codes, no request_id, 2 shapes | strong |
| 3 | Auth | **COSMETIC + GAP** — same header; M1 laxer scheme, richer identity | mixed |
| 4 | Versioning | **IDENTICAL** (`/v1/`, no header) | **STRONG — M1↔M2 independent** |
| 5 | Tenant scoping | **STRUCTURAL + SECURITY GAP** | strong |
| 6 | Status vocabulary | **DELIBERATE** (domains differ) + GAP (none discoverable) | n/a |
| 7 | Cost / feasibility | **STRUCTURAL ×2** — M1 absent; M2↔M3 irreconcilable bodies | strong |
| 8 | Admin surface | **STRUCTURAL** — M1 has none | strong |
| 9 | Health / observability | **STRUCTURAL** — no two agree on `/readyz` | strong |
| 10 | DB boundary | **IDENTICAL** on grants + worker-no-DB; **STRUCTURAL** on schema | **STRONG on the agreements** |
| 11 | Config | **STRUCTURAL** — env-restart vs DB-hot | strong |
| 12 | Migrations | **STRUCTURAL** — unledgered forward-only vs gated pairs | strong |
| 13 | Kill switches | **M1 AHEAD** (ladder + shadow-default), one rung inert | strong |
| 14 | Logging | **IDENTICAL** in style; **GAP** in M1 CI enforcement | strong |
| 15 | Tests | **DELIBERATE + GAP** — M1 fakes the boundary, real-PGs the store | strong |

---

## Section 5 — Defects found in code during this audit

Reported as findings, **not fixed**. Each was verified first-hand in the tree.

| # | Module | Severity | Finding | Evidence |
|---|---|---|---|---|
| D1 | M1 | **MEDIUM as shipped / HIGH on multi-tenant key issuance** (revised §0b.3; was HIGH) | Cross-tenant read: any authenticated caller reads any job's status and verdicts. **Not** email addresses (`api.go:444` blanks them) and **not** enumerable (74 `crypto/rand` bits). No second untrusted key exists yet — no issuance path — so it is a **prerequisite gate**, not a live breach | `api.go:251,273,313,444`, `postgres.go:805,828,857`, `idgen.go:35`, `0005_grants.sql:112`, `0010_api_callers.sql:43` |
| D2 | M1 | **MEDIUM (safety)** | `accept_and_defer` kill-switch rung validates but has no consumer — behaves as `serving` | `config.go:59,750-753` vs `api.go:144,223` |
| D3 | M1 | **MEDIUM (operability)** | No `request_id` in any error body and none in any log line — errors uncorrelatable | every `writeJSON` site in `api.go` |
| D4 | M2+M3 | **MEDIUM (integration)** | `/v1/quote` request bodies mutually rejecting under `DisallowUnknownFields` | M2 `handlers.go:49-51,302`; M3 `handlers.go:52-55,374` |
| D5 | M3 | **MEDIUM (design intent)** | `notifications` is write-only — no platform or admin route reads it | migrations 0033/0047; only reader is `dispatch/notify.go` |
| D6 | M1 | **LOW-MED (operability)** | Migrations have no tracking table; re-apply safety rests on per-file idempotence guards | `ci.yml:44-51`; no `schema_migrations` in any migration |
| D7 | M1 | **LOW-MED (isolation)** | No dedicated schema; **21 base tables + 3 partitions** in `public`, including `jobs` — collision risk with platform (count corrected 2026-08-07) | verified table list |
| D8 | M1 | **LOW (auth)** | Per-caller key miss falls through to legacy static token on the sync path | `api.go:399-407` |
| D9 | M2 | **LOW (correctness)** | `parseReportPageSize` default/max (100/200) are hardcoded, not seeded config | `campaign_report.go:322-323` |
| D10 | M3 | **LOW (rule violation)** | Bare `10*time.Second` DB timeout with no config key, unlike every comparable timeout | `server.go:156-158`, `adminapi/server.go:170-172` |
| D11 | M2 | ~~COSMETIC~~ **FIXED** | `contract_drift.sh` logged "22 routes gated"; registration and probe map are both 21. Corrected in `coldmail-send-managed@80f3a69` | `contract_drift.sh:69` vs `server.go:43-74` |
| D12 | M1 | ~~UNVERIFIED RISK~~ **CONFIRMED LEAK, FIXED** | pgx v5.10.0 embeds the whole connection string (password included) in its parse error, and `main.go` logged it verbatim. Fixed at the source in `29e6204`: `store.OpenPG` wraps both returns in `scrubDSNError` (`postgres.go:52,56`). `main.go:113` is now a comment; the live log site is `:115`. **The same defect is UNFIXED in both senders at pgx v5.7.4 — see fix-plan item N1** | verified by measurement, not flagged |

D1 and D2 are the two that would change a deployment decision. D4 is the one that blocks a shared platform
client for the two senders today.

---

## Section 6 — Deliberate-by-design differences (NOT defects)

Per the brief: classifying a deliberate design difference as a defect is as wrong as missing a real one.
The following are correct as they stand and should not be "aligned."

1. **M3's 17 `/v1/byosmtp/*` extension routes.** Users manage their own SMTP credentials; M2's users do not
   own accounts at all. The extension mechanism is the right shape, and M3's implementation — one registry
   slice that is simultaneously the mount source and the `/v1/describe` content — is the best pattern in the
   three repos. It should become the canonical extension mechanism.
2. **M3's quote as feasibility rather than cost.** M3 bills a flat subscription; per-recipient cost is
   meaningless for it. Retaining inert cost fields for contract compatibility while adding real feasibility
   is the right call. (The *request body* incompatibility, D4, is a separate and genuine defect.)
3. **M1's nodes carrying IP reputation, warmup, quarantine, and audit state.** M1 probes MX servers directly,
   so its nodes' IP reputation is load-bearing; M3's boxes are pure relays using the user's own SMTP
   credentials. Different node models are correct.
4. **Different status vocabularies.** `valid|invalid|risky|unknown` and
   `queued|assigned|sending|sent|failed|bounced` describe different domains. Forcing a shared vocabulary
   would destroy information.
5. **M1's `caller_ref` as an opaque echo.** Deliberate and well-documented: the platform owns the
   user-facing job; the module holds only work state. The design is sound — the defect (D1) is that the
   *authenticated* `caller_id` is also unused, which is a different thing from `caller_ref` being opaque.
6. **M3's admin-only quarantine verbs.** Absent from the platform surface and 404 (not 401) there, asserted
   by CI. Correct separation of operator and user authority.
7. **M3's split settings tables.** User-writable `user_settings` vs admin-only `user_config_overrides` are
   two different tables rather than one table with a permission check — structural enforcement, the stronger
   choice.
8. **Campaign-scoped reports returning empty rather than 404.** `campaign_id` is platform-minted, not
   module-allocated, so "no rows" is the honest answer. Consistent in both senders.
9. **M1's forward-only migrations.** A legitimate convention. The *absence of a ledger* (D6) is the finding,
   not the forward-only choice.
10. **M1's bulk results omitting `email`.** Plaintext is purged on settle by design (data retention). Not an
    inconsistency with the sync shape but a deliberate privacy property.

---

## Section 7 — C2: the three-way diff, every difference classified

### 7.0 Classification scheme

Each difference below is assigned **exactly one** label:

| Label | Meaning |
|---|---|
| **IDENTICAL** | Same across all three (or across all where the category applies). Nothing to do. |
| **DELIBERATE** | Differs because the domain genuinely differs. Correct as-is; aligning it would destroy information. |
| **COSMETIC** | Differs in name/shape only, with no behavioural consequence beyond needing a mapping table. Cheap. |
| **STRUCTURAL** | Differs in a way that forces the platform to hold per-module code paths, not just per-module values. |
| **GAP** | One module lacks something another has. Each is marked **REQUIRED** (every module must have it) or **OPTIONAL**. |

Two scoping notes that make the table readable:

- Rows are marked **[1v2]** where M1 and M2 disagree independently (**strong evidence** that the difference
  is real), **[2v3]** where the disagreement is between the fork parent and child (**weak evidence** — but a
  *divergence* between M2 and M3 is still real, because two deployed modules genuinely differ; it is
  *agreement* between them that proves nothing), or **[3-way]**.
- A row is judged against **what the platform must write**, not against which module is "better."

### 7.1 New evidence gathered for this section

Before classifying, I diffed the senders' three remaining core routes line by line (the `quote`
incompatibility was already established in §3.7):

- **`POST /v1/run` — CHARACTER-IDENTICAL between M2 and M3.** `runReq{JobItemID, UserID}`, the same
  `uuidRe` validation, the same required/malformed distinction, the same `createJob` → 202
  `{job_id, state:"queued"}` / duplicate → 200 `{job_id, state:<current>}`. The only textual difference in
  the surrounding code is the schema name in a comment (`sender.jobs` vs `byosmtp.jobs`).
- **`GET /v1/report` request parsing — CHARACTER-IDENTICAL between M2 and M3.** Same three selectors, same
  required `user_id`, same precedence (`campaign_id` wins), same `job_not_found` with the caller's own
  selector echoed, same no-existence-disclosure comment.
- **`GET /v1/report` response — M3 is a strict superset.** M2 returns `job_id, state, attempt_count` plus
  optional `provider_message_id, completed_at`. M3 returns those five plus optional `account_id`,
  `account_username`, `smtp_response`, `error_class`, `failed_at`. Every addition is optional and omitted
  when absent, so an M2-shaped client parses an M3 response correctly.
- **Campaign report — M3 is a superset with one internal inconsistency.** M3 adds `totals_by_account[]` and
  the same five per-item fields. **Note:** in the campaign shape those five serialize as explicit `null`,
  while in the single-job shape the keys are **omitted**. M3's own code documents this as intentional —
  *"a SEPARATE, deliberately inconsistent contract (M3-P7-OD-4), not something this file should silently
  fix"* (`campaign_report.go:38-49`). It is a documented decision, but it still means a UI needs two
  null-handling paths for the same five logical fields within one module.
- **`GET /v1/describe` — differs in two values.** `module` is `"sender"` vs `"byosmtp"` (correct and
  necessary). `pricing.model` is `"flat_per_recipient"` vs `"flat_subscription"` — a genuine semantic
  divergence in a field with **no declared value set**. M3 additionally returns `extension_routes`,
  `schemas`, and `bulk_add` (additive).

**Consequence for the verdict:** ~~the sender incompatibility is not diffuse. Of the four core routes, **three
are already uniform** (two character-identical, one strictly additive). Exactly **one route** — `quote` —
is genuinely irreconcilable.~~

> ## RETRACTED (2026-08-04, §0b). "Exactly one route" was wrong.
>
> An adversarial refutation pass (13 claims, 20 agents, 0 failures) found this understates the surface, and
> **the two worst breakages are silent rather than rejecting**. Verified first-hand:
>
> 1. **`quote` blocks by rejection** — the mutually-exclusive bodies (D4). CONFIRMED, unchanged.
> 2. **`describe` blocks by changed VALUE, not changed shape.** No field is renamed, removed, or retyped —
>    so it is a *field-name* superset — but two keys M2 already emits carry different values: `module`
>    `"sender"`→`"byosmtp"` (`Codmailling 2/.../handlers.go:257` vs `Codmailling 3/.../handlers.go:309`) and
>    `pricing.model` `"flat_per_recipient"`→`"flat_subscription"` (`:269` vs `:321`). A client keyed on
>    either string breaks. **"Strict superset" implies value-compatibility and should not have been used.**
> 3. **`quote`'s `cost` is always 0 in M3 — a wrong number, not an error.** `byosmtp_0048_unit_price_zero
>    .up.sql:23` reseeds **both** `value` and `default_value` to `{"v":0}`, and `defaultQuoteUnitCredits = 0`
>    (`handlers.go:25`). Verified: M2's is seeded `{"v":1}` and admin-retunable. So a cost-preview client
>    calling M3 silently receives 0 with a 200. **This is worse than D4** — a rejection is detectable; a
>    plausible wrong number is not.
> 4. **M3's campaign report can 500 where M2's returned data.** `readConfigInt` returns `pgx.ErrNoRows` in
>    the *err* branch, so the `!ok` fallback at `campaign_report.go:160-162` is **unreachable for a missing
>    row** — an absent `daily_reset_timezone` / `throttle_pause_seconds` / `daily_reset_hour` row yields 500
>    `campaign_report_totals_failed` on the shared route. A latent-dead fallback.
>
> **Corrected claim:** `run` and single-job `report` are genuinely uniform (byte-diff of `runReq`,
> `handleRun`, and `handleReport`'s parsing region differs only in one comment each, `sender.jobs` →
> `byosmtp.jobs`). **But at least four surfaces block or silently break, not one.** The direction of the
> original finding holds — the senders are far closer to each other than either is to M1 — but "one cheap
> route-fix away from a shared client" is retracted as too optimistic.

### 7.2 The full classification table

**Category 1 — Platform-facing routes**

| # | Difference | Class | Notes |
|---|---|---|---|
| 1.1 | M1's verb set (`verify`, `verify/bulk`, `jobs/{id}`, `jobs/{id}/results`) vs the senders' (`quote`, `run`, `report`, `describe`) | **STRUCTURAL** [1v2] | Disjoint. No route, method, or path is shared. The single largest finding. |
| 1.2 | `/v1/` prefix on every route in all three | **IDENTICAL** [3-way] | Strong (M1↔M2 independent). |
| 1.3 | Go 1.22 `ServeMux` method+path patterns in all three | **IDENTICAL** [3-way] | Strong. |
| 1.4 | M3's 17 `/v1/byosmtp/*` extension routes | **DELIBERATE** [2v3] | M3's users own SMTP accounts; M2's do not. Correct. |
| 1.5 | Route registration: M1 inline+auth-inside-handler, M2 inline+decorator, M3 table-driven | **STRUCTURAL (internal)** [3-way] | Invisible to the platform, but M1's is the only one where forgetting auth on a new route is silent. Affects trust in the surface, not its shape. |
| 1.6 | `DisallowUnknownFields()`: senders yes, M1 no | **GAP — REQUIRED** [1v2] | Strict decode is what makes an additive contract *enforceable*. M1 silently ignores unknown fields, so a client typo is accepted and dropped. |
| 1.7 | M1 has no `describe`-equivalent | **GAP — REQUIRED** [1v2] | Without it, capabilities must be hardcoded per module in the platform, which is exactly the per-module special-casing this audit tests for. |

**Category 2 — Error envelope**

| # | Difference | Class | Notes |
|---|---|---|---|
| 2.1 | M1 `{"error":"<free text>"}` vs senders `{"error":{code,message,details?,request_id}}` | **STRUCTURAL** [1v2] | Two incompatible envelopes. A platform error handler cannot be written once. |
| 2.2 | M1 has no machine-readable error codes | **GAP — REQUIRED** [1v2] | Clients must string-match English. Any prose edit in M1 is a silent breaking change. |
| 2.3 | M1 emits no `request_id` in any error body | **GAP — REQUIRED** [1v2] | A user-reported M1 error is uncorrelatable to any server-side record (§3.9: not logged either). |
| 2.4 | M1's 413 returns a second shape (`{error, max, got}`) | **COSMETIC** [M1-internal] | Still parses as `{error:string}`; the extra keys are additive. Signals the absence of a choke point. |
| 2.5 | M2 has 5 error codes, M3 has 7 (`unprocessable`, `rate_limited`) | **STRUCTURAL** [2v3] | Additive *within* M3, divergent *across* modules at the same `/v1`. A client with an exhaustive switch on M2's set breaks on M3. Fix is in the contract (clients MUST tolerate unknown codes), not in either module. |
| 2.6 | Issue slugs (`required`, `malformed`, `out_of_range`, `not_found`) identical in both senders | **IDENTICAL** [2v3 — weak] | Inheritance. Should be *locked* by the contract, not assumed stable. |
| 2.7 | Senders route every error through one `writeError`; M1 hand-rolls at ~25 sites | **STRUCTURAL (internal)** [1v2] | Determines whether uniformity can be *maintained*, not just achieved once. |

**Category 3 — Auth**

| # | Difference | Class | Notes |
|---|---|---|---|
| 3.1 | `Authorization` header in all three | **IDENTICAL** [3-way] | Strong. |
| 3.2 | ~~M1 accepts a bare token without the `Bearer` scheme~~ **FIXED `d481c97`** | ~~COSMETIC~~ **[now 3-way IDENTICAL]** | The COSMETIC rating missed that the divergence was two-way: M1 was also the only module REJECTING lower-case `bearer`, which RFC 9110 §11.1 makes conforming. Both closed. |
| 3.3 | Credential storage: M1 sha256-in-DB + env static token; senders bcrypt-in-env | **DELIBERATE** [1v2] | Both are defensible. sha256 is correct for a high-rate lookup by index; bcrypt is correct for a small compared set. Neither is wrong. |
| 3.4 | M1 has per-caller identity (`api_callers`); senders have a flat token set with none | **STRUCTURAL** [1v2] | M1's model is *better* and is the one the contract should require — but see 5.2: M1 is also the one that fails to use it. |
| 3.5 | Senders separate platform-token and admin-key credentials structurally | **IDENTICAL** [2v3 — weak] | M1 has no admin surface, so the property is vacuous there rather than violated. |
| 3.6 | M1 falls through from a per-caller key miss to the static token on the sync path | **GAP — REQUIRED** [M1] | Revoking a per-caller key does not fully revoke sync access while any static token exists. |
| 3.7 | Unconfigured behaviour: M1 dev-open (dev env only); senders fail-closed, M3 unmounts the surface | **STRUCTURAL** [1v2] | M1's is env-gated, so not a production hole — but "misconfigured" means *serving* in one module and *refusing* in the others. |

**Category 4 — Versioning**

| # | Difference | Class | Notes |
|---|---|---|---|
| 4.1 | `/v1/` URL prefix, no version header, in all three | **IDENTICAL** [3-way] | **The single strongest three-way agreement in the audit.** Lock it as-is. |
| 4.2 | M1 asserts its response version in code (`Valid()`); senders do not | **GAP — REQUIRED of senders** [1v2] | M1's pattern is better and should be adopted. |
| 4.3 | Senders gate served-set == documented-set in CI; M1 has no route-drift gate | **GAP — REQUIRED of M1** [1v2] | Each module has half the mechanism. The contract should require both halves. |
| 4.4 | M1 mirrors its vocabularies into DB `CHECK` constraints and widens them as explicit supersets | **DELIBERATE (and exemplary)** [1v2] | Not a divergence to fix — a pattern to copy. |

**Category 5 — Tenant scoping**

| # | Difference | Class | Notes |
|---|---|---|---|
| 5.1 | Tenant identity space: senders use platform `public.users.id` (UUID); M1 uses its own `api_callers.caller_id` (text) | **STRUCTURAL** [1v2] | No shared tenant identity across the three. Any cross-module per-user view needs a mapping layer. |
| 5.2 | M1 does not scope reads by the authenticated caller at all | **GAP — REQUIRED (SECURITY)** [1v2] | Defect D1. The highest-severity finding in the audit. |
| 5.3 | Senders' explicit `user_id` selector on every query | **IDENTICAL** [2v3 — weak inheritance, but independently proven by behavioural tests in each] | The pattern the contract should require. |
| 5.4 | 404-not-403 with byte-identical bodies for "not yours" and "nonexistent" | **IDENTICAL** [2v3 — weak] | Correct posture. M1 cannot be said to violate it because it has no cross-tenant check at all. |
| 5.5 | `campaign_id` returning an empty aggregate rather than 404 | **DELIBERATE** [2v3] | `campaign_id` is platform-minted; "no rows" is the honest answer. |
| 5.6 | RLS: M1 none, M2 8 tables, M3 13 | **GAP — OPTIONAL** [1v2] | Genuinely optional: in the senders RLS is an explicit `USING(true)` backstop, not the isolation mechanism. Requiring it of M1 would not have prevented D1. Recommend it as defence-in-depth; do not make it a contract condition. |
| 5.7 | M3's `accountUpdateReq` structurally omits `user_id` so ownership cannot be reassigned | **DELIBERATE (exemplary)** [2v3] | Structural prevention over a runtime check. |

**Category 6 — Status / state vocabulary**

| # | Difference | Class | Notes |
|---|---|---|---|
| 6.1 | `valid/invalid/risky/unknown` vs `queued/assigned/sending/sent/failed/bounced` | **DELIBERATE** [1v2] | Different domains. Forcing a shared vocabulary destroys information. |
| 6.2 | No module exposes its vocabulary programmatically | **GAP — REQUIRED** [3-way] | Applies equally to all three. A UI must hardcode three enum sets from docs. This is the most broadly-applicable gap found. |
| 6.3 | M1 enforces a composability invariant in code (`invalid ⇒ skip` only) | **DELIBERATE (exemplary)** [M1] | Nothing comparable in the senders; worth adopting where an equivalent invariant exists. |
| 6.4 | M1 pre-reserves later phases' vocabulary values from Phase 1 | **DELIBERATE (exemplary)** [M1] | Makes additive-only true by construction rather than by discipline. |
| 6.5 | `waiting_for_capacity` as an aggregate-only value, never a per-job state | **IDENTICAL** [2v3 — weak] | Correct and worth locking. |
| 6.6 | M3 adds 7 domain enums M2 lacks | **DELIBERATE** [2v3] | Its domain has states M2's does not. |

**Category 7 — Cost / feasibility**

| # | Difference | Class | Notes |
|---|---|---|---|
| 7.1 | M1 has no cost or feasibility verb, field, or dry-run of any kind | **GAP — REQUIRED** [1v2] | Cost is discoverable only by submitting real work. The platform cannot show a user a price before committing. |
| 7.2 | `quote` request bodies mutually rejecting under strict decode | **STRUCTURAL** [2v3] | Defect D4. **The one thing blocking a single client for the two senders.** Cheap to fix. |
| 7.3 | `quote` semantics: cost multiplier (M2) vs feasibility check (M3) | **DELIBERATE** [2v3] | M3 bills a flat subscription; per-recipient cost is meaningless for it. Retaining inert cost fields for compatibility is the right call. |
| 7.4 | `pricing.model`: `flat_per_recipient` vs `flat_subscription`, with no declared value set | **STRUCTURAL** [2v3] | A client switching on `pricing.model` must already know both values, and nothing tells it what values exist. Needs a declared enum in the contract. |
| 7.5 | `currency` is the hardcoded literal `"credits"` in both senders; no ISO-4217, no decimals | **DELIBERATE for now / future STRUCTURAL** [2v3] | Correct while credits are abstract integers. If real money ever crosses this boundary it is a contract change in both. Register it as an assumption. |
| 7.6 | M3's `feasible`/`reasons[]`/`caps{}`/`capacity{}`/`warnings[]` block | **DELIBERATE** [2v3] | Genuinely useful and domain-correct. Should become the *optional* feasibility half of the canonical `quote`. |

**Category 8 — Admin surface**

| # | Difference | Class | Notes |
|---|---|---|---|
| 8.1 | M1 has zero HTTP admin routes; senders have 21 each | **GAP — OPTIONAL for the platform contract, REQUIRED for operability** [1v2] | The platform does not call admin routes, so this blocks no integration. But issuing or rotating an M1 platform key requires hand-run SQL in production with no audit trail — a real operational burden and the weaker audit posture. |
| 8.2 | Admin route composition: M2 21 (incl. 5 pools), M3 21 (pools dropped, 6 user/config added) | **DELIBERATE** [2v3] | Same total, different composition, both matching their domains. |
| 8.3 | Admin registration: M2 per-route decorator, M3 table-driven | **COSMETIC** [2v3] | M3's is structurally safer; neither is visible to the platform. |
| 8.4 | The admin surface is a parallel write path over the same rows the platform reads, with no notification to the platform | **STRUCTURAL** [2v3, applies to both] | An operator pause changes state a platform client is polling, and the platform learns only by re-polling. Needs stating in the contract, not fixing in the modules. |

**Category 9 — Health / observability**

| # | Difference | Class | Notes |
|---|---|---|---|
| 9.1 | `/healthz` and `/readyz` exist, unauthenticated, in all three | **IDENTICAL** [3-way] | Strong. |
| 9.2 | `/readyz` body: M1 `{ready, config, pepper, supabaseKey}` vs senders `{status, db, singleton_lock, at}` — **not one shared field name** | **STRUCTURAL** [1v2] | Cheap to fix, but until fixed a platform health dashboard needs three parsers. |
| 9.3 | M1's readiness checks only that a secret loaded — not the DB | **GAP — REQUIRED** [1v2] | M1 can report `ready:true` and still 503 every request. An orchestrator's readiness gate does not reflect serving capability. |
| 9.4 | `/healthz` body: M1 omits `at` | **COSMETIC** [1v2] | |
| 9.5 | No module exposes `/metrics` | **GAP — REQUIRED (uniformly)** [3-way] | All three are monitorable only by bespoke polling. M1 has a `metrics` package computing rates and percentiles with **no HTTP exposure and no caller**. A platform decision, not a per-module defect. |
| 9.6 | Operator health JSON: senders have a rich `/v1/admin/health`; M1 has nothing | **GAP — OPTIONAL** [1v2] | Operator-facing, not platform-facing. |
| 9.7 | M3's `notifications` table is write-only — no route reads it | **GAP — REQUIRED** [2v3] | Defect D5. The module records user-actionable events a UI cannot fetch, contradicting M3's own stated principle that users are notified about everything actionable. |
| 9.8 | M3's five widened report fields are `null` in the campaign shape but omitted in the single-job shape | **COSMETIC (documented)** [M3-internal] | M3's code explicitly declares this deliberate (M3-P7-OD-4). Still costs a UI two null-handling paths. |

**Category 10 — DB boundary**

| # | Difference | Class | Notes |
|---|---|---|---|
| 10.1 | Per-table enumerated grants, never blanket defaults, in all three | **IDENTICAL** [3-way] | **Strong (M1↔M2 independent).** Lock it. |
| 10.2 | The worker holds no DB credential — enforced structurally by shipping no driver | **IDENTICAL** [3-way] | **The strongest architectural agreement in the audit**, independently arrived at. Lock it. |
| 10.3 | Append-only enforced by grant revocation (M1 `verifications`, senders `job_events`) | **IDENTICAL** [3-way] | Strong. |
| 10.4 | Non-login owner role distinct from the login runtime role, in all three | **IDENTICAL** [3-way] | Strong. |
| 10.5 | M1 has no dedicated schema — **21 base tables + 3 partitions** in `public`, including `jobs` (count corrected 2026-08-07) | **STRUCTURAL** [1v2] | `public.jobs` and `public.users` are names a platform is likely to want, and the senders already FK into `public`. Live collision risk in a shared database. |
| 10.6 | Senders hard-FK into `public.users` (7 FKs) and `public.job_items` (1); M1 references neither | **STRUCTURAL** [1v2] | The senders cannot start without the platform schema; M1 is fully standalone. Two different deployment couplings. |
| 10.7 | Platform-table shape is assumed, never verified — preflight checks existence only, by design | **GAP — REQUIRED (of the platform, not the modules)** [2v3] | The senders read 7 named `job_items` columns. A platform rename breaks both at runtime and the preflight will not catch it. Belongs in the assumptions register. |
| 10.8 | User deletion: 6 FKs `CASCADE` into module data, 1 `RESTRICT`s the delete outright | **STRUCTURAL** [2v3] | Platform user-deletion semantics are partly dictated by module migrations, inconsistently between the two. |

**Category 11 — Config**

| # | Difference | Class | Notes |
|---|---|---|---|
| 11.1 | M1 env vars (restart to retune) vs senders DB rows (hot) | **STRUCTURAL** [1v2] | Under load this is the difference between a config change and a rolling deployment. Not platform-contract-blocking; operationally significant. |
| 11.2 | Senders gate config drift and config consumption in CI; M1 has neither | **GAP — OPTIONAL** [1v2] | Internal hygiene, invisible to the platform. It is what caught M1's own D2 by hand rather than automatically. |
| 11.3 | Hardcoded tuning literals present in all three | **GAP — OPTIONAL** [3-way] | Both senders' own rules forbid these, so they are self-declared violations (D9, D10). M2's `page_size` 100/200 (D9) is the only one that governs a **platform-visible** response, which raises it above internal hygiene. |
| 11.4 | M1's `Dial{Value, Set}` distinguishing unset from explicit zero | **DELIBERATE (exemplary)** [M1] | The senders lack it; worth adopting. |

**Category 12 — Migrations**

| # | Difference | Class | Notes |
|---|---|---|---|
| 12.1 | M1 forward-only; senders paired up/down | **DELIBERATE** [1v2] | Forward-only is a legitimate convention. |
| 12.2 | M1 has no migration tracking table | **GAP — REQUIRED** [1v2] | Defect D6. Re-application safety rests entirely on each file's own idempotence guards; nothing records what ran. |
| 12.3 | M1 runs migrations via a `ls | sort | psql -f` shell loop; senders use a custom Go runner | **COSMETIC** [1v2] | Consequence of 12.2, not independently harmful. |
| 12.4 | Senders enforce a blocking `migration_lint`; M1 has no equivalent | **GAP — OPTIONAL** [1v2] | Internal hygiene. |
| 12.5 | Naming: `NNNN_x.sql` vs `{sender,byosmtp}_NNNN_x.{up,down}.sql` | **COSMETIC** [1v2] | The prefix is actually load-bearing for the senders (they share a database). |

**Category 13 — Kill switches**

| # | Difference | Class | Notes |
|---|---|---|---|
| 13.1 | M1 has a 4-rung graduated ladder; senders have a binary `dispatch_enabled` | **GAP — REQUIRED of the senders** [1v2] | Under a partial incident the senders' only tool is a full stop. The ladder shape is the right answer; see §7.4. |
| 13.2 | M1's `accept_and_defer` rung validates but has no consumer | **GAP — REQUIRED (SAFETY)** [M1] | Defect D2. A silently-inert safety control is worse than an absent one, because it will be trusted. |
| 13.3 | M1's four mutators all default to `recommend_only`; senders retrofitted one shadow dial | **GAP — REQUIRED of the senders** [1v2] | Shadow-by-default-from-day-one is unambiguously ahead. |
| 13.4 | `autoquarantine_mode` is a real 3-rung ladder, shadow-default, fail-safe on unknown values | **DELIBERATE (and the counter-example to 13.1)** [2v3] | The senders *do* have ladder discipline — applied to autonomous scoring, not to dispatch. |
| 13.5 | M1 requires the literal `"true"` plus a separate DPA flag plus a vendor key before external egress | **DELIBERATE (exemplary)** [M1] | "Enabling is hard" by design. |
| 13.6 | Kill switches are config-driven and load-validated in all three | **IDENTICAL** [3-way] | Strong. |

**Category 14 — Logging**

| # | Difference | Class | Notes |
|---|---|---|---|
| 14.1 | stdlib `log`, plain text, no structured logger, in all three | **IDENTICAL** [3-way] | Strong — and arguably all three are equally wrong for cross-module aggregation. |
| 14.2 | M1 logs no request id on any line; senders log `rid=` on every line | **GAP — REQUIRED** [1v2] | Compounds D3: an M1 error is correlatable to neither a log line nor a response field. |
| 14.3 | Redaction discipline real in all three (M1's `Secrets.String()` renders `<set>`/`<unset>`) | **IDENTICAL** [3-way] | Strong. |
| 14.4 | CI leak gates: M1 none, M2 `no_secret_leak`, M3 that plus `user_output_scrub` | **GAP — REQUIRED** [1v2] | M3's `user_output_scrub` is the only mechanism in any repo that *structurally prevents* an internal reason string reaching a client. The pattern the contract should require. |

**Category 15 — Tests**

| # | Difference | Class | Notes |
|---|---|---|---|
| 15.1 | M1 tests the platform boundary against in-memory fakes; senders drive the real `Server` over real HTTP against a cloned migrated DB as the least-privileged role | **STRUCTURAL** [1v2] | This is *why* D1 is untested rather than caught: with a fake store there is no query for a `caller_id` predicate to be missing from. |
| 15.2 | M1's store-layer tests use real Postgres and CI genuinely provisions it | **IDENTICAL** [1v2] | Must be stated: M1's 339 tests are real and its PG-backed tests actually run. Any claim that M1 has a hollow suite would be wrong. |
| 15.3 | M1's `FAIL-VERIFY` comments naming the exact mutation that breaks each property | **DELIBERATE (exemplary)** [M1] | The best paired-control documentation in the three repos. |
| 15.4 | Senders assert the cross-tenant 404 is byte-identical to a nonexistent-id 404, and assert zero rows created | **DELIBERATE (exemplary)** [2v3] | The anti-vacuity control that makes the tenancy test meaningful. |
| 15.5 | CI gate count: M1 4, M2 6, M3 7 | **GAP — OPTIONAL** [1v2] | Internal hygiene, but it is the mechanism difference behind several rows above. |

### 7.3 Counts

| Class | Rows | Of which block a single platform integration |
|---|---|---|
| IDENTICAL | 20 | — |
| DELIBERATE | 16 | — |
| COSMETIC | 7 | 0 (but 9.2 costs three health parsers until aligned) |
| STRUCTURAL | 17 | **9** |
| GAP — REQUIRED | 17 | **6** |
| GAP — OPTIONAL | 7 | 0 |

**20 IDENTICAL rows is the substantive good news of this audit**, and four of them are strong (M1↔M2
independent) architectural agreements worth locking permanently: the `/v1/` prefix with no version header,
per-table enumerated grants, append-only-by-grant-revocation, and the worker holding no DB credential by
shipping no driver.

### 7.4 The senders' kill-switch ladder — the assessment the brief asked for

**Should the senders adopt M1's ladder? Yes for dispatch, and the reason is specific.**

The senders' `dispatch_enabled` is binary: dispatch runs, or it stops entirely. There is no rung for "drain
in-flight but accept nothing new", no "serve reads, refuse new work", no "accept and defer". During a partial
incident — one provider degrading, a suspected bad config, a single user's accounts misbehaving — the only
available action is a full stop that halts every tenant. That is a real operational gap and the ladder is
the right shape for it.

Two qualifications, both drawn from the code rather than asserted:

1. **The senders are not starting from zero.** `autoquarantine_mode`'s `off|shadow|enforce` is a genuine
   three-rung ladder with shadow as the default and a fail-safe that falls back to `shadow` — never
   `enforce` — on an unrecognised value. The discipline exists; it was applied to the autonomous
   account-scoring subsystem and not to dispatch itself. So this is a gap in *coverage*, not in
   *understanding*, which makes it cheaper to close than a from-scratch design.
2. **M1's ladder is not fully wired, so "adopt M1's ladder" cannot mean "copy M1."** Its `accept_and_defer`
   rung (D2) validates at load and then behaves as `serving`, because no consumer reads it. M1 has the
   design without the wiring; the senders have wiring without the design. The recommendation is therefore
   symmetric: the senders should adopt the ladder *shape* for dispatch, and M1 should either wire
   `accept_and_defer` or reject it at load rather than accept a control it will not honour.

**Shadow-mode-by-default should be adopted wholesale by the senders**, separately from the ladder. M1 has
four independent mutators all defaulting to `recommend_only`, with the recommend-only path proven to produce
`Action{Apply:false, Recommend:…}` and zero live mutation. The senders retrofitted one shadow dial late.

### 7.5 THE VERDICT

> **COULD ONE PLATFORM INTEGRATION DRIVE ALL THREE MODULES WITHOUT PER-MODULE SPECIAL-CASING?**
>
> ## NO.

Not "not yet" and not "with some polish". The three modules are **structurally incompatible at the
platform boundary today**, and the incompatibility is not symmetric — it is almost entirely M1 versus the
two senders. Stating it plainly, as the brief asked:

**M2 and M3 are much closer to each other than either is to M1 — but not "one route" close.** `run` and
single-job `report` are genuinely character-identical (byte-diff: one comment each, `sender.jobs` →
`byosmtp.jobs`). **REVISED 2026-08-04 (§7.1, §0b):** the original claim that *exactly one* route blocks a
shared client is **retracted**. At least **four** surfaces block or silently break, and **the two worst are
silent, not rejecting**:

| Surface | How it breaks | Detectable by the client? |
|---|---|---|
| `quote` request | Mutually rejecting bodies via `DisallowUnknownFields` (D4) | **Yes** — 400 |
| `describe` values | `module` and `pricing.model` carry different values for the same keys | **No** — a keyed client silently mis-routes |
| `quote` `cost` | M3 returns **always 0** (`0048` reseeds `value` **and** `default_value`; D22) | **No** — 200 with a wrong number |
| campaign `report` | M3 500s on a missing config row where M2 returned data (D21) | Yes, but only in production |

Fixing `quote`'s body remains the cheapest single unblock and is still worth doing first. But **"a small,
well-defined piece of work with a clear finish line" was too optimistic**, and the honest version is: one
cheap fix, one value-vocabulary decision (`pricing.model`, `module`), and one pricing question M3's own
migration raises — whether `unit_price_credits` should exist at all under flat subscription.

**M1 cannot be driven by that integration at all, and no amount of alignment changes it — because what is
missing is not agreement but implementation.** Six independent things must be *built*, not reconciled:

| # | What M1 lacks | Why the platform cannot work around it |
|---|---|---|
| 1 | The `quote`/`run`/`report`/`describe` verb set (1.1) | Zero shared routes. Not a rename — a different API. |
| 2 | A coded error envelope with `request_id` (2.1-2.3) | The platform's error handler cannot be written once; M1 failures are uncorrelatable to any log. |
| 3 | Tenant scoping on reads (5.2) | **Wiring M1 into a multi-tenant platform today exposes every caller's job results to every other caller.** This is a blocker on security grounds independent of contract shape. |
| 4 | Any cost or feasibility verb (7.1) | The platform cannot price or pre-check M1 work before committing it. |
| 5 | A `describe`-equivalent (1.7) | Capabilities must be hardcoded per module — the definition of per-module special-casing. |
| 6 | A shared tenant identity (5.1) | Senders key on platform `users.id`; M1 on its own `caller_id`. No cross-module per-user view without a mapping layer. |

**So the honest shape of the answer is:** the platform needs **two** integrations today — one shared by the
two senders (after one fix), and a bespoke one for M1. Whether that is acceptable is a product decision, not
an engineering finding, and the audit does not presume it. What the audit does establish is the cost of the
alternative: making one integration drive all three means giving M1 a second, parallel platform surface
implementing the uniform contract — which is a phase of work, not a fix pass. C4's ordered fix plan sizes
both paths.

**The evidence-weight caveat, stated where it bites hardest:** the uniform contract that M1 would be asked
to adopt has been validated by exactly **one** independent implementation. M2 and M3 both implement it, but
M3 inherited it from M2 by fork — their `platformapi/auth.go` is byte-identical, and `run` and `report` are
character-identical. That agreement proves the copy was faithful; it does not prove the contract is
well-designed. **Before M1 is asked to implement quote/run/report/describe, the contract should be reviewed
on its own merits**, because M3's adoption of it is not independent corroboration — and the one place M3
genuinely had to *think* rather than copy (`quote`, where its billing model differs) is precisely the one
place the contract broke (D4). That is a signal about the contract, not just about M3.

---

## Section 8 — C3: quality and operational parity

A different axis from C2. C2 asked *can one client drive all three*. C3 asks *are all three equally safe to
operate*, independent of whether their contracts match. A module can have a perfect contract and be
operationally unsafe, or a divergent contract and be rock-solid. Grades below are per-module, evidence-first.

### 8.1 Two-vocabulary errors (internal reason never crosses a client boundary)

| | Grade | Evidence |
|---|---|---|
| M1 | **N/A — no vocabulary exists** | No codes at all (§3.2), so there is no internal/external split to violate or honour. The free-text strings are *incidentally* clean (no SQLSTATE, no SQL, no stack traces found at any `writeJSON` site), but by absence of a mechanism rather than by design. |
| M2 | **STRONG** | One `writeError` serializer, one `s.fail` choke point, ~26 internal reason slugs that never appear on the wire. Enforced structurally: there is exactly one writer. |
| M3 | **STRONGEST of the three** | M2's structure **plus** `user_output_scrub.sh`, a blocking gate with four independent static checks — every read of `jobs.last_smtp_response` must go through its single named accessor; `jobs.last_error_message` must have **zero** readers in `platformapi/`; the notification render path must draw from the accessor; no unledgered consumer of `boxproto.Report.SMTPResponse` may exist. |

**The finding:** M3's gate is the only mechanism in any of the three repos that *mechanically prevents* an
internal reason string reaching a client, rather than trusting every handler to route through the right
helper. M2 relies on there being one writer — true today, but a discipline, not a barrier. This is the
clearest case in the audit of the fork *improving* on its parent, and it is the pattern the canonical
contract should require.

### 8.2 Config-not-code

| | Grade | Evidence |
|---|---|---|
| M1 | **MEDIUM** | ~60 settings, all env vars. Hot-retuning impossible — every change is a restart. `Dial{Value,Set}` is a better primitive than anything the senders have. No drift or consumption gate; several genuine literals (D-list). |
| M2 | **STRONG** | **54** DB rows (corrected, §0a.2 — the 53 figure was the manifest's stale header comment, D18), JSONB `{"v":…}`, re-read every dispatch tick and per request. Two blocking gates (`config_drift`, `config_consumption`). Platform-visible violations: `page_size` 100/200 hardcoded **and silently clamped** (D9, D20); one loaded-but-never-read key (D19). |
| M3 | **STRONG** | 87 keys, same mechanism and gates. One genuine violation: a bare `10*time.Second` DB timeout with no key (D10), notable because every comparable SMTP/dispatch timeout *is* a key. |

**Operational consequence, stated plainly:** during an incident, the senders can be retuned live; M1 requires
a rolling deployment. That is the practical difference, not the aesthetic one.

### 8.3 Real behavioural tests with paired controls — **the sharpest parity finding in C3**

This is where the gap between the three is widest, and it is not the gap the test *counts* suggest.

**M1's suite is real** (339 tests, 15 skips, CI provisions a real Postgres and applies every migration).
**But two specific properties are scaffolded, not proven, and one is worse than scaffolded:**

1. **Exactly-once settlement is a `t.Skip` scaffold.** `test/gaps/gaps_test.go:15-27` —
   `TestGAP37_ExactlyOnceSettlementUnderReclaim` enumerates five scenarios including *"slow (not dead)
   holder + TTL reclaim: second attempt runs, but only the CAS-on-lease-holder commits"* and *"crash after
   SMTP-success, before settle: reclaim re-attempts; settles exactly once"* — then
   `t.Skip("PENDING P5: needs the work_items store + drainer + CAS")`. The scaffold is **honest** (its own
   header says *"these appear as SKIP (not PASS), so CI honestly shows the behavioral layer is
   scaffolded-not-proven"*, and the CI comment repeats it), and the design is present — `DequeueReady` uses
   `FOR UPDATE SKIP LOCKED` (`postgres.go:599-613`), and `0009_work_items_jobs.sql:29` documents
   compare-and-set settlement. **But the property is unproven by test.** Both senders prove their
   equivalent behaviourally against real infrastructure. This is the single largest quality-parity gap.

2. **M1's grant-enforcement tests can never assert in CI — and the CI comment claims the opposite.**
   `TestPGSendOutcome_RuntimeCannotDelete` (`postgres_flywheel_test.go:138-143`) and
   `TestPGRateBudget_DeleteDeniedForRuntime` (`postgres_test.go:391-398`) both attempt a `DELETE` and then:
   ```go
   if err == nil {
       t.Skip("connected as a privileged role (DELETE allowed) — grant enforcement is proven only as cp_runtime")
   }
   ```
   CI connects as **`postgres://postgres:ci@localhost:5432/cptest`** — the superuser
   (`ci.yml:55-58`). `cp_runtime` is `NOLOGIN` (`0005_grants.sql:15`) and **is never referenced anywhere in
   `ci.yml`** (verified: zero hits). So the `DELETE` always succeeds, and both tests **always skip in CI**.
   Meanwhile `ci.yml:23-25` states: *"A real Postgres so the store + Door-2 append-only integration tests
   actually RUN in CI (they SKIP without `TM_TEST_DATABASE_URL` — proving append-only on every push, not
   just locally)."* **That claim is false as configured** — the tests skip for a *different* reason than the
   one the comment anticipates. The skip is honest in isolation; the CI comment asserting the property is
   proven on every push is not. Recorded as **D13**.

   **The senders do exactly what M1 intends but does not do.** M3's harness connects as the least-privileged
   role: `moduleDSN := withUserPass(cloneDSN, "byosmtp_module", modulePassword())`
   (`platformapi/harness_test.go:49`), and `tests/harness/run.sh:77-85` sets that role's password
   out-of-band specifically *"so the Phase 1 role-isolation tests (T5/T6/T3e) can connect as
   `byosmtp_module` against clones."* Docker Compose gives the Brain
   `DATABASE_URL: postgres://byosmtp_module:…` while migrations run as a separate superuser DSN
   (`docker-compose.yml:39,56`). So the senders' grant posture is proven under the real role; M1's is
   asserted in a comment.

3. **Where M1 is genuinely ahead:** its `FAIL-VERIFY` comments name the exact mutation that breaks each
   property (`auth_test.go:50-54`), which is the best paired-control *documentation* in the three repos. The
   senders' equivalent strength is the anti-vacuity control — asserting the cross-tenant 404 is
   byte-identical to a nonexistent-id 404, and querying the DB directly to assert zero rows created.

**Grades:** M1 **MEDIUM** (real and honest, but the two load-bearing safety properties — exactly-once and
grant enforcement — are unproven, and one is mis-claimed as proven). M2 / M3 **STRONG** (platform boundary
driven over real HTTP against a cloned migrated DB as the least-privileged role, so a grant error, RLS
misconfiguration, or query-level tenancy bug *would* be caught).

**And this is the mechanism behind D1.** M1's route-level tests inject `Verifier`, `CallerAuth`,
`BulkSubmitter`, `JobReader` interfaces over an in-memory store. With a fake store there is no SQL for a
`caller_id` predicate to be missing from — so the cross-tenant read gap is structurally invisible to M1's
own test suite. The senders' identical property is caught by construction.

### 8.4 Credential handling

| | Grade | Evidence |
|---|---|---|
| M1 | **STRONG (different threat model)** | Pepper is `TM_PEPPER`-only, never leaves `internal/secrets`, refuses to start without it (`secrets.go:94-97`), `Secrets.String()` renders only `<set>`/`<unset>` and counts. **No SMTP credentials to hold** — it probes MX servers, it does not authenticate to them. No zeroize path exists because there is nothing to zeroize. |
| M2 | **STRONG** | AES-256-GCM, AAD-bound, Brain-sole-decryptor; box holds credentials in memory only and zeroes on every exit path; box ships no DB driver and no KMS SDK. |
| M3 | **STRONG** | Same, plus per-user credential verification state and the `bulk_add` plaintext-file warning contract in `/v1/describe`. |

**Not a defect in M1:** the absence of zeroize/KMS is domain-correct, not a gap. Its custody design is
deliberate and well-documented — the encryption key is *separate* from the pepper precisely so that losing
the key is recoverable while losing the pepper de-joins history forever (`secrets.go:43-46`). That is
better reasoning than most modules apply to key custody.

~~**One unverified residual risk carried forward honestly:**~~ **RESOLVED 2026-08-07 — it was a CONFIRMED
LEAK, and both hedges in the original sentence were wrong.** `main.go` logged the store-open error via `%v`;
pgx v5.10.0 embeds the whole connection string, password included, in its parse error. Fixed at the source
in `29e6204` (`store.OpenPG` → `scrubDSNError`, `postgres.go:52,56`; the live log site is now `main.go:115`),
and M1 now DOES have a CI leak gate (`scripts/check-no-secret-leak.sh`, wired at `ci.yml:145`) which boots
the binary and scans its log rather than grepping source. **The same defect is unfixed in both senders at
pgx v5.7.4 — fix-plan N1.**

### 8.5 Append-only audit

| | Grade | Evidence |
|---|---|---|
| M1 | **DESIGNED, UNPROVEN** | `GRANT SELECT, INSERT ON verifications TO cp_runtime; REVOKE UPDATE, DELETE` (`0005_grants.sql:38-39`), with the table comment *"THE LOG: append-only enforcement via grants… Corrections are new rows, never mutations."* Extended to `verification_raw` and its partitions. **Design is correct; §8.3's finding is that CI never exercises it.** |
| M2 | **PROVEN** | `job_events` SELECT+INSERT only, exercised as `sender_module`. |
| M3 | **PROVEN** | Same, plus `account_events` with a widened action CHECK. |

**Strong three-way agreement on the *mechanism*** (grant revocation, not triggers, not application
discipline) — one of the audit's genuinely reassuring convergences, and M1↔M2 independent. The divergence is
only in whether it is tested.

### 8.6 Tenant isolation

| | Grade | Evidence |
|---|---|---|
| M1 | **FAIL** | D1. No predicate on any read path; no RLS backstop; auth establishes an identity that is never used as a filter. |
| M2 | **STRONG** | Explicit `user_id` on every query, 404-not-403, byte-identical bodies, proven behaviourally with an anti-vacuity control. |
| M3 | **STRONG** | Same, plus RLS on 13 tables and `accountUpdateReq` structurally omitting `user_id` so ownership cannot be reassigned. |

The only **FAIL** grade in C3. It is a security failure, not a parity gap, and it is why M1 cannot be wired
into a multi-tenant platform as-is regardless of contract alignment.

### 8.7 Concurrency safety

| | Grade | Evidence |
|---|---|---|
| M1 | **DESIGNED, UNPROVEN** | Real primitives: `FOR UPDATE SKIP LOCKED` for work-item leasing (`postgres.go:599-613`), `FOR UPDATE` for rate-budget buckets with a documented cold-start fix (*"`SELECT … FOR UPDATE` cannot lock a NONEXISTENT row"*, `postgres.go:391-395`), CAS-on-lease-holder settlement. **No `pg_advisory_lock`, no singleton guard, no leader election anywhere** (verified: zero hits). The exactly-once property is the skipped GAP-37 scaffold. |
| M2 | **STRONG** | Postgres advisory-lock singleton with bounded acquire; documented account→job lock ordering; two-connection race tests. |
| M3 | **STRONG** | Same, plus a codified lock-ordering rule (its own Rule 9), an anti-vacuity requirement that race tests *prove* contention via scoped `pg_stat_activity`, and recorded Postgres semantics for when the rule does not apply. |

**M1 has no singleton guard at all** — the senders acquire a session-level advisory lock at startup and fail
if they cannot. M1's drainer is documented as *"Single-brain (FOR UPDATE SKIP LOCKED, no broker)"*
(`queue/drainer.go:76`), so its safety under two accidentally-running instances rests entirely on
`SKIP LOCKED` and CAS rather than on preventing the second instance. That is a defensible design — arguably
more robust than a lock, since it tolerates the condition instead of forbidding it — **but it is the exact
property GAP-37 was written to prove and does not.** Recorded as **D14** (design-vs-proof gap, not a
confirmed bug).

**One M3 uncertainty carried forward honestly:** `account_cap_race_test.go` and
`account_unquarantine_race_test.go` exist and are wired into the harness, but **their bodies were not read**,
so whether they satisfy M3's own scoped-`pg_stat_activity` contention requirement is **not verified here**.

### 8.8 Kill switches and shadow mode

Delivered in full at **§7.4**. Summary: M1 **STRONG design / MEDIUM wiring** (4-rung ladder,
shadow-by-default on four mutators, but `accept_and_defer` is inert — D2). Senders **MEDIUM** (binary
dispatch halt with no intermediate rung; genuine 3-rung ladder discipline exists but is applied to
autonomous scoring rather than dispatch). Recommendation: senders adopt the ladder shape for dispatch and
shadow-by-default wholesale; M1 either wires `accept_and_defer` or rejects it at load.

### 8.9 Health and metrics

| | Grade | Evidence |
|---|---|---|
| M1 | **WEAK** | Readiness checks only that a secret loaded — not the DB — so `ready:true` can coexist with every request 503ing (§3.9). No request id in any log line, so an error is uncorrelatable to any record (D3 + 14.2 compounding). |
| M2 | **MEDIUM** | Readiness checks DB ping and genuine singleton-lock holding. `rid=` on every log line. Rich `/v1/admin/health`. |
| M3 | **MEDIUM** | Same, plus per-user headroom and destination throttles. |

**No module exposes `/metrics`** — a uniform gap. M1's `internal/metrics` package computes block rate,
unknown rate, cache hit rate, and latency p50/95/99 with **no HTTP exposure and no caller** — the
computation exists and is unreachable, structurally the same shape as M3's write-only `notifications` (D5).
Two modules independently built observability data a consumer cannot reach.

### 8.10 Deployment story

| | Grade | Evidence |
|---|---|---|
| M1 | **INCOMPLETE for the control-plane** | Verified by `find`: the **only** Docker artifacts in the repo are `verifier-node/docker-compose.yml`, `verifier-node/truemail-rack/Dockerfile`, and a test Dockerfile. **There is no Dockerfile and no compose file for the Go control-plane** — the deployable half of the module. No Terraform, no Helm, no manifest of any kind. CI builds it with `go build ./...` and never packages it. |
| M2 | **COMPLETE** | `docker-compose.yml` stands up Postgres + Brain + boxes; dev==CI loop. |
| M3 | **COMPLETE** | Same, with the migration/runtime role split visible in the compose env: migrations use a superuser DSN, the Brain uses `byosmtp_module`, and boxes carry *"no DATABASE_URL, no KMS key — box has neither by construction"* (`docker-compose.yml:39,56,123,166`). |

**This is a real independent-deployability finding** and the one place the brief's "is each module
independently deployable and operable" question returns a negative. M1's verifier-node is containerised; its
control-plane is not. Every other module ships a working local stack that is the same stack CI runs.

### 8.11 CI gates

| | Count | Gates |
|---|---|---|
| M1 | 4 | Go build/vet/test w/ real PG; `door5-gem-unmodified`; RSpec; docs-updated + config-reference freshness. Plus a `code-reviewer` job. |
| M2 | 6 | `config_drift`, `contract_drift`, `migration_lint`, `config_consumption`, `drain_grace`, `no_secret_leak` |
| M3 | 7 | those six plus `user_output_scrub` |

M1's gates are real and two are distinctive — `assert-gem-unmodified.sh` pins the third-party verification
engine byte-for-byte (a supply-chain control neither sender has an equivalent of), and a docs-staleness gate
that both senders also have in spirit. What M1 lacks is any **drift** gate: nothing checks that its served
route set matches its documented one, that its config keys have consumers, or that no secret appears in
logs. Those three absences map directly to three findings in this audit (D2's unconsumed dial, D12's
unverified log risk, and the absence of a route-surface freeze).

### 8.12 Parity scorecard

| Axis | M1 | M2 | M3 |
|---|---|---|---|
| Two-vocabulary errors | N/A (no vocabulary) | STRONG | **STRONGEST** |
| Config-not-code | MEDIUM | STRONG | STRONG |
| Behavioural tests + controls | **MEDIUM** | STRONG | STRONG |
| Credential handling | STRONG | STRONG | STRONG |
| Append-only audit | DESIGNED, UNPROVEN | PROVEN | PROVEN |
| Tenant isolation | **FAIL** | STRONG | STRONG |
| Concurrency safety | DESIGNED, UNPROVEN | STRONG | STRONG |
| Kill switches / shadow | STRONG design / MEDIUM wiring | MEDIUM | MEDIUM |
| Health / metrics | **WEAK** | MEDIUM | MEDIUM |
| Deployment story | **INCOMPLETE** | COMPLETE | COMPLETE |
| CI gates | 4 (no drift gates) | 6 | 7 |

**The honest summary:** the three modules are **not** at equivalent operational safety. M2 and M3 are close
peers, with M3 ahead on output scrubbing and lock-ordering rigour — though their agreement is weak evidence
by inheritance, so their shared strengths are better read as *one* validated design than two. M1 is a
different profile entirely: strong on secret custody, supply-chain pinning, vocabulary design, and
shadow-mode defaults, and materially behind on tenant isolation (a failure, not a gap), proof of its
concurrency and grant properties, observability, and packaging.

**The pattern worth naming:** M1's weaknesses cluster in *proof and packaging*, not in design. GAP-37 names
the right five scenarios; the grant tests attempt the right `DELETE`; the ladder declares the right four
rungs; `internal/metrics` computes the right percentiles. In each case the design is sound and the last
step — assert it, wire it, expose it, package it — is missing. That is a cheaper class of problem to fix
than a wrong design, and it should be read that way when sizing the work. **The exception is D1**, which is
not an unproven design but an absent one, and is the only item on this page that is a security failure
rather than a maturity gap.

### 8.13 Additional defects found during C3

| # | Module | Severity | Finding | Evidence |
|---|---|---|---|---|
| D13 | M1 | **MEDIUM (false assurance)** | Grant-enforcement tests always skip in CI because CI connects as superuser, while `ci.yml`'s own comment claims append-only is "prov[en] on every push" | `postgres_flywheel_test.go:143`, `postgres_test.go:398`, `ci.yml:23-25,55-58`; `cp_runtime` is NOLOGIN and unreferenced in CI |
| D14 | M1 | **MEDIUM (unproven safety)** | Exactly-once settlement under a slow-holder + TTL reclaim is a `t.Skip` scaffold; no singleton guard exists, so the property rests entirely on `SKIP LOCKED` + CAS | `test/gaps/gaps_test.go:15-27`; zero `pg_advisory`/singleton hits |
| D15 | M1 | **MEDIUM (deployability)** | No Dockerfile or compose file exists for the Go control-plane — only the Ruby verifier-node is containerised | `find` over the repo: 3 Docker artifacts, all under `verifier-node/` |
| D16 | M1 | **LOW-MED (observability)** | `internal/metrics` computes block/unknown/cache rates and latency percentiles with no HTTP exposure and no caller | mirrors D5's shape in M3 |
| **D17** | **ALL THREE** | **MEDIUM (contract)** | **A method mismatch or unmatched `/v1/*` path escapes the error envelope entirely.** Go 1.22 method+path registration means `ServeMux` answers first — `405 text/plain` `Method Not Allowed\n` / `404 page not found\n`, no code, no `request_id`, **before auth**. A client assuming `error.code` on every non-2xx nil-derefs. No gate probes it. | `Codmailling 2/.../platformapi/server.go:56-59`; `Codmailling 3/.../server.go:140-143`; `Codmailling/control-plane/internal/api/api.go:116-123`; mount at `httpapi/health.go:43-46`; `contract_drift.sh:79-88` (method-appropriate probes only) |
| **D18** | **M2** | **LOW (stale doc)** | `Docs/config-manifest.txt`'s header comment claims a 53-key total; the file's own counted content is **54** (`config-reference.md` agrees at 54). Written when `sender_0023` was newest and never updated. **This audit initially repeated the comment instead of counting** — see §0a.2. | `Docs/config-manifest.txt:5-6` vs counted content; `sender_0023_singleton_reacquire_config.up.sql` |
| **D19** | **M2** | **LOW-MED (dead config)** | `alert_webhook_url` is loaded into `Config.AlertWebhookURL` every dispatch tick and **never read** — the field has zero readers. The live consumer re-reads the key inside the admin health handler, so **alerting fires only when an operator polls `/v1/admin/health`**, never autonomously. | `dispatch/config.go:101,331` (declare + assign, no reader); `adminapi/health_handler.go:123,128` |
| **D20** | **M2** | **LOW (silent clamp)** | `/v1/report`'s `page_size` **silently falls back to 100** on a non-numeric or ≤0 value rather than returning `validation_failed` — a platform-visible parameter failing quietly. Strengthens D9. | `campaign_report.go:324` |

D13 is the one to weigh most heavily of these four: a test that always skips while CI documentation claims
it proves the property is worse than no test, for the same reason D2 is worse than no kill switch — it will
be trusted.

---

## Section 9 — What C4 will resolve

- **C4** — `canonical-module-contract.md` (the contract + the platform assumptions register + the
  UI-readiness assessment) and `alignment-fix-plan.md` (the ordered fix plan, each item marked
  BLOCKS-PLATFORM-INTEGRATION or not). **Recommendations only; none performed.**
