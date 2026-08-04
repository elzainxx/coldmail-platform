# Module 2 — sender (shared SMTP pool)  
repo: D:/Mnfi/Codmailling 2

## 1. PLATFORM-FACING ROUTES
All four platform routes are registered in `services/brain/internal/platformapi/server.go:56-60` on a dedicated `http.ServeMux`, each wrapped in `s.requireAuth(...)`:

```go
s.mux.HandleFunc("POST /v1/quote", s.requireAuth(s.handleQuote))
s.mux.HandleFunc("POST /v1/run", s.requireAuth(s.handleRun))
s.mux.HandleFunc("GET /v1/report", s.requireAuth(s.handleReport))
s.mux.HandleFunc("GET /v1/describe", s.requireAuth(s.handleDescribe))
```
Mounted explicitly (not path-prefix) onto the Brain's health-server mux in `services/brain/internal/httpapi/health.go:40-47`.

### POST /v1/quote — `handleQuote`, `handlers.go:53-93`
```go
type quoteReq struct {
    RecipientCount *int `json:"recipient_count"`
}
```
`recipient_count`: `*int`, REQUIRED (nil → `validation_failed`/`required`), must be `>=0` (negative → `validation_failed`/`out_of_range`), `0` valid. Response: `recipient_count`(int),`cost`(int),`currency`(literal `"credits"`),`unit_price_credits`(int, live),`tier`(string, always `"shared"`).

### POST /v1/run — `handleRun`, `handlers.go:102-155`
```go
type runReq struct {
    JobItemID string `json:"job_item_id"`
    UserID    string `json:"user_id"`
}
```
Both REQUIRED UUIDs (regex `uuidRe`, `handlers.go:45`). Response `202`: `{"job_id":uuid,"state":"queued"}`; duplicate → `200`: `{"job_id":uuid,"state":<current>}`.

### GET /v1/report — `handleReport`, `handlers.go:159-242` + `campaign_report.go`
Query: `job_id`/`job_item_id`/`campaign_id` (one required) + `user_id` (REQUIRED). Single-job response: `job_id`,`state`,`attempt_count` always; `provider_message_id`/`completed_at` only when non-null. Campaign response (`campaign_report.go:93-107`): `campaign_id`,`dispatch_paused`,`totals{total,sent,bounced,failed,in_flight,pending,waiting_for_capacity}`,`progress{terminal,fraction_complete,is_complete}`,`items{results[],next_cursor,page_size}`.

### GET /v1/describe — `handleDescribe`, `handlers.go:252-274`
No params. Response: `module`,`api_version`,`capabilities{...}`,`pricing{currency,model,unit_price_credits,tiers[]}`.

All bodies decoded via `decodeStrict` (`handlers.go:300-327`) with `DisallowUnknownFields()`.

## 2. ERROR ENVELOPE
Single uniform shape, `errors.go`:
```go
type errorEnvelope struct { Error errorBody `json:"error"` }
type errorBody struct {
    Code      string        `json:"code"`
    Message   string        `json:"message"`
    Details   []fieldDetail `json:"details,omitempty"`
    RequestID string        `json:"request_id"`
}
```
Choke point: `newErr` builds it, `writeError` (`errors.go:95-110`) is the ONLY serializer, invoked via every handler's `s.fail` (`auth.go:87-91`) — uniform across ALL routes, no hand-rolled shape found.

Codes/status (`errors.go:11-17,36-42`): `validation_failed`→400, `unauthenticated`→401, `not_found`→404, `conflict`→409, `internal_error`→500. Issue slugs (`errors.go:21-26`): `required`,`malformed`,`out_of_range`,`not_found`.

Internal-reason literals (never serialized): `recipient_count_required`,`recipient_count_negative`,`job_item_id_required`,`user_id_required`,`user_id_malformed`,`job_id_or_job_item_id_or_campaign_id_required`,`job_id_malformed`,`job_item_id_malformed`,`job_not_found`,`report_query_failed`,`campaign_id_malformed`,`cursor_malformed`,`job_item_not_found`,`user_campaign_active`,`job_item_invalid`,`job_item_already_accepted`,`create_job_failed`,`begin_tx_failed`,`set_actor_failed`,`advisory_lock_failed`,`read_job_item_failed`,`campaign_check_failed`,`commit_failed`,`service_token_invalid_or_missing`,`panic_recovered`,`malformed_json:...`.

Admin surface (`internal/adminapi/errors.go`) has a byte-identical envelope but a superset of codes (`config_ordering_violation`,`forbidden`,`unprocessable`,`rate_limited`) never used by platform.

## 3. AUTH MECHANISM
Header `Authorization`, scheme `Bearer <token>` (case-insensitive), extracted by `bearerToken` (`auth.go:50-61`).

Storage: bcrypt hashes only (`Authenticator.keyHashes`), sourced from env `PLATFORM_TOKEN_HASHES` (comma-separated, `internal/config/config.go:99-100`). No DB table.

Comparison: `bcrypt.CompareHashAndPassword` looped over all hashes with no early return so timing doesn't reveal the matching index (`auth.go:37-48`). Fail-closed on empty hash set or empty bearer.

Middleware `requireAuth` (`auth.go:65-84`): failure → `codeUnauthenticated`/`service_token_invalid_or_missing`→401; installs `defer recover()` for clean 500 on panic.

Exactly one credential type (bearer service token). Distinct from operator admin key (`internal/adminapi/auth.go`) — separate `Authenticator` type/instance + env var, never shared (Rule 6, `server.go:1-9`).

## 4. VERSIONING SCHEME
URL prefix `/v1/` for all four routes (`server.go:56-59`). No version header used. `handleDescribe` echoes `"api_version":"v1"` as a response field (`handlers.go:258`), informational only, not negotiated.

Additive-only discipline enforced in code by `tests/harness/checks/contract_drift.sh` (BLOCKING, `.github/workflows/real-infra.yml:40-42`): probes unauthenticated, asserts each of the 4 routes returns `401` (proving mounted, `contract_drift.sh:74-92`), an undocumented path returns `404` (`contract_drift.sh:94-96`), and each served path literally appears in `Docs/api-contract.md` (`contract_drift.sh:98-105`). No Go-level route-registry-freeze test exists inside `platformapi` — enforcement is this CI probe only.

## 5. TENANT SCOPING
Field `user_id`, string UUID. Body for `run` (`runReq.UserID`), query string for `report` (`handlers.go:163`). Never derived from the auth credential — caller-supplied (OD-25, `server.go:11-17`).

SQL WHERE-clause scoping (exact text):
- `store.go:63`: `SELECT campaign_id FROM public.job_items WHERE id = $1 AND user_id = $2`
- `store.go:73-76`: `SELECT EXISTS (SELECT 1 FROM sender.jobs WHERE user_id = $1 AND campaign_id <> $2 AND state IN ('queued','assigned','sending'))`
- `store.go:88-94`: `... FROM public.job_items ji WHERE ji.id = $1 AND ji.user_id = $2 RETURNING id`
- `store.go:124-125`: `SELECT id FROM sender.jobs WHERE job_item_id = $1 AND user_id = $2`
- `store.go:163-171`: single-job report lookups by `id`/`job_item_id` AND `user_id`
- `campaign_report.go:135`,`199`: campaign items/totals scoped by `campaign_id`/`user_id`

Cross-tenant: single-job — zero rows → identical 404 `not_found` as a nonexistent id, never 403 (`handlers.go:203-227`, `store.go:107-112`). Campaign-scoped — clean empty aggregate (`total:0`), not 404 (`campaign_report.go:47-54`). Proven by `tenancy_test.go` and `campaign_report_test.go`'s `TestCampaignReport_CrossTenantIsEmptyNotLeaked`.

## 6. STATUS / STATE VOCABULARY
Primary state machine — Postgres enum `sender.job_state` (`db/migrations/sender_0003_create_enums.up.sql:7-14`): `'queued','assigned','sending','sent','failed','bounced'`. These literal strings appear via `state::text` on `/v1/report` and campaign items.

Aggregate-only distinction in campaign report only, NOT a new per-job enum value (`campaign_report.go:9-14`): `waiting_for_capacity`,`pending`,`in_flight`,`sent`,`bounced`,`failed`,`total` — SQL `COUNT(*) FILTER(...)` (`campaign_report.go:143-153`).

Other enums exist but never cross the platform boundary: `sender.error_class` (`bounce_permanent,transport_4xx,auth_failure,throttle,timeout,unknown`, `sender_0003...up.sql:16-23`) feeds only the admin `errors_last_window`. `sender.pool_kind` (`shared,dedicated`) surfaces only as the `tier` string. `routing_strategy`,`box_status`,`event_actor`,`pause_reason` are internal/admin-only.

A job WAITING for capacity reads `state:"queued"` (never `failed`), per `campaign_report.go:9-14` and `Docs/api-contract.md:289`.

## 7. COST / FEASIBILITY EXPRESSION
Pre-flight verb exists: `POST /v1/quote` (`handlers.go:53-93`) — pure, side-effect-free, idempotent, proven zero-job-created by `quote_test.go:36-42`.

Fields: `recipient_count`(int),`cost`(int = recipient_count×unit),`currency`(hardcoded literal `"credits"`, `handlers.go:83`),`unit_price_credits`(int, live-read from `sender.config` via `readUnitPriceCredits`, `handlers.go:282-294`, fallback `defaultQuoteUnitCredits=1` at `handlers.go:21`),`tier`(string, always `"shared"`, `handlers.go:31`).

No feasibility/capacity check from `quote` itself — no tenant/pool context in request (`handlers.go:23-30`). Capacity signal appears only post-hoc via the campaign report's `waiting_for_capacity`/`dispatch_paused` (`campaign_report.go:93-107`).

Units: integer credits only, no float, no ISO-4217 code, flat per-recipient pricing (`handlers.go:267-273`).

## 8. ADMIN SURFACE + ITS AUTH
21 operator routes registered `internal/adminapi/server.go:45-73`, all behind `s.requireAuth(...)` (separate `Authenticator`): accounts CRUD+events (6), account lifecycle transitions quarantine/unquarantine/disable/enable/pause/unpause (6), pools CRUD (5), config list/get/put (3), health (1).

(`contract_drift.sh:69` echoes "22 routes gated" but its own `ADMIN_ROUTES` map and `server.go` both enumerate exactly 21 — minor internal inconsistency, flagged not silently fixed.)

Auth: operator bearer key, bcrypt-hashed, env `ADMIN_API_KEY_HASHES` — distinct `Authenticator` type/instance from platform's, never shared.

Parallel write path: admin accounts routes are the ONLY writer of `sender.accounts.credential_ciphertext` (Encrypt-only, `internal/adminapi/crypto.go`); platform has no credential write path. Admin's pause/unpause/quarantine/disable mutate columns the platform's `campaignEligibleAccountsSubquery` (`campaign_report.go:174-192`) reads for `waiting_for_capacity`. `PUT /v1/admin/config/{key}` is the only writer of `sender.config`, read live by both surfaces.

## 9. HEALTH / OBSERVABILITY
Liveness `GET /healthz` (`internal/httpapi/health.go:56-58`): unauthenticated, always 200, `{"status":"ok","at":<timestamp>}`.

Readiness `GET /readyz` (`health.go:62-79`): unauthenticated, checks `db.Pool.Ping` AND `guard.Healthy`; `{"status":"ready"|"not_ready","db":bool,"singleton_lock":bool,"at":<timestamp>}`; 200 if both true else 503.

Operator-only `GET /v1/admin/health` (`internal/adminapi/health_handler.go:13-60`), key-gated: `generated_at`,`window_sec`,`dispatch{...}`,`queue{queued,assigned,sending,sent,failed,bounced,waiting_for_capacity}`,`boxes{active,stale,disabled,oldest_active_heartbeat_age_sec}`,`accounts{healthy,paused,quarantined,disabled,at_daily_limit}`,`pools[]`,`errors_last_window{class:count}`,`shadow{...}`,`active_conditions[]`.

No `/metrics`, no Prometheus (grep over Go tree returned zero hits).

Logging: stdlib `log.Printf`, not structured JSON (`internal/adminapi/logger.go`, `internal/platformapi/auth.go:95-101`). `rid`/`request_id` on every log line, minted by `newRequestID()` (`errors.go:81-87`), echoed to caller.

## 10. DB BOUNDARY
Schema `sender` (`db/migrations/sender_0000_bootstrap.up.sql:26`). Owning role `sender_owner` — NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB. App/login role `sender_module` — LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB, password out-of-band.

RLS enabled on ALL `sender.*` tables, `FORCE`d additionally on `sender.accounts` only (`sender_0012_grants_and_rls.up.sql:38-45`). `sender_module` gets one permissive `USING(true) WITH CHECK(true)` policy per table — backstop, not the tenant-scoping mechanism.

Grants per-table ENUMERATED explicitly: full DML on `pools,accounts,boxes,jobs,config`; `job_events` SELECT+INSERT only; `send_logs` SELECT+INSERT+DELETE (`sender_0012_grants_and_rls.up.sql:18-26`). Bootstrap's default privileges key on CREATOR and do NOT fire for admin-created-then-reowned tables — documented ADR-0008 trap (`sender_0012...up.sql:3-11`).

Platform-table access: `sender_module` gets `GRANT SELECT` ONLY on `public.users`/`public.job_items` (`sender_0000_bootstrap.up.sql:49-50`).

Only the Brain connects to Postgres; the box ships NO DB driver/KMS SDK (`services/box/package.json` `dependencies:{}` with explicit forbidding comment).

## 11. CONFIG APPROACH
DB rows in `sender.config` (`db/migrations/sender_0010_create_config_and_seed.up.sql:8-41`), JSONB-wrapped `{"v":...}`, `default_value` never mutated post-seed. Total 53 keys per `Docs/config-manifest.txt` (14+3+2+6+17+11 across Phase1/5/6/7/8/9.5 migrations).

Load path: `dispatch.LoadConfig` (`services/brain/internal/dispatch/config.go:189-362`) re-reads ALL keys every dispatch tick; `platformapi` reads fresh per-request (`readUnitPriceCredits` `handlers.go:282-294`, `readDispatchPaused` `campaign_report.go:250-262`) — hot-reloadable, no caching.

Env-var exception: `internal/config/config.go:1-8` — Phase-0 process-wiring only (`DATABASE_URL`,`PLATFORM_TOKEN_HASHES`,`ADMIN_API_KEY_HASHES`), never behavioral dials.

CI gates: `config_drift.sh` (BLOCKING) — live==manifest==config-reference.md both directions. `config_consumption.sh` (BLOCKING) — every key needs a real grep-verifiable consumer or explicit RESERVED marker.

Hardcoded tuning literals found: `handlers.go:21` `defaultQuoteUnitCredits=1` (fallback only); `handlers.go:31` `defaultQuoteTier="shared"`; and `campaign_report.go:322-323` `defaultPageSize=100`/`maxPageSize=200` in `parseReportPageSize` — genuine runtime-behavior-governing literals NOT backed by a seeded config row, a drift candidate against Rule 1.

## 12. MIGRATION CONVENTIONS
Naming: `sender_NNNN_<desc>.up.sql`/`.down.sql`, regex-enforced `^sender_([0-9]{4})_(.+)\.up\.sql$` (`tests/harness/checks/migration_lint.sh:22`). Chain `sender_0000_bootstrap`→`sender_0024_job_events_fk_carveout` (50 files, 25 pairs).

Up/down pairing required (`migration_lint.sh:29`).

Runner: custom Go binary `tools/sender-migrate`. Tracking table `sender.schema_migrations(version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ)` (`sender_0000_bootstrap.up.sql:31-37`) — each migration file self-inserts its version; each down self-deletes.

Contiguity enforced (`migration_lint.sh:33-38`). BLOCKING CI gate (`real-infra.yml:44-46`), also cross-checks newest version literal appears in `tools/sender-migrate/runner_integration_test.go`'s `wantChain` (`migration_lint.sh:54-81`).

## 13. KILL SWITCHES AND SHADOW MODES
Five kill switches, all Phase-9.5/`sender_0021`-seeded, read every tick via `dispatch.LoadConfig`:
- `dispatch_enabled` bool, default `true` (`dispatch/config.go:319`; consumed `dispatch/dispatch.go:248`, `dispatch/loop.go:67`) — halts dispatch cleanly.
- `autoquarantine_mode` string enum `off|shadow|enforce`, default `shadow` (`dispatch/config.go:322`; consumed `dispatch/account_effect.go:70`, unrecognized defaults to shadow at `account_effect.go:75`) — the one graduated 3-level ladder, shadow-by-default.
- `autopause_enabled` bool, default `true` (`dispatch/config.go:325`; consumed `dispatch/account_effect.go:99`).
- `variation_enabled` bool, default `true` (`dispatch/config.go:328`; consumed `dispatch/variation.go:22`, bypasses entire variation layer).
- `alert_webhook_url` string URL or `""`, default `""` (`dispatch/config.go:331`; consumed `adminapi/health_handler.go:123,128`).

Structural (non-config-table) switch: `brain_singleton_acquire_timeout_sec` (also sender_0021) bounds startup wait for the Postgres advisory lock (`internal/singleton/singleton.go`, read `cmd/brain/main.go:66,303-328`); SIGKILL auto-releases.

## 14. LOGGING DISCIPLINE
Rule stated in code: `internal/adminapi/logger.go:8-13` — admin service "NEVER logs request bodies... the smtp_secret and the ciphertext bytes structurally cannot appear in a log line (tests T1/T3 assert zero occurrences...)". Matching comment `internal/platformapi/auth.go:63-64` ("The token is NEVER logged").

CI gate: `tests/harness/checks/no_secret_leak.sh` (BLOCKING, `real-infra.yml:56-58`) greps ALL service docker-compose logs for the dev box token plaintext + bcrypt hash (and a second box's if provisioned), fails on any match (`no_secret_leak.sh:28-38`). Its header notes scope: covers box-registration token only; SMTP-credential leak scan lives separately in `tests/harness/phase3_send.sh`.

Direct inspection: every log call site read (`platformapi/auth.go:95-101`, `adminapi/logger.go:16-44`) logs only `rid`, method, sanitized path, `code`, curated `internalReason`, log level — no body/token/credential/ciphertext/raw pg error found. Not a 100%-exhaustive scan of every log line in the repo.

## 15. TESTS
Real Postgres + real HTTP, not mocks — stated explicitly `platformapi/quote_test.go:5-6` ("Real Postgres, real HTTP against the actual production Server (CLAUDE.md Rule 3) — no mocks"). `newTestEnv` (`harness_test.go`) stands up the real `platformapi.Server` with a live `pgxpool.Pool` (`e.admin`), used directly in assertions (`quote_test.go:37`, `tenancy_test.go:171`).

Test files: `quote_test.go`,`quote_validation_test.go`,`campaign_test.go`,`campaign_report_test.go`,`unitprice_test.go`,`tenancy_test.go`,`report_notfound_selector_test.go`,`harness_test.go`.

Paired positive/negative control: `tenancy_test.go:112-153` (`TestTenancy_CrossTenantReportIsNotFound`) — positive: A reads A's job → 200; negative: B reads A's job → 404, plus the cross-tenant 404 is asserted byte-identical in code to a genuinely-nonexistent-id's 404. If tenant scoping were removed, `requireStatus(t,404)` at lines 136/141 would fail immediately.

Another paired control: `quote_test.go:19-43` asserts `cost==250` (positive) AND `jobCount==0` after quote (negative, side-effect-free).

Auth/tenancy separation control: `tenancy_test.go:213-235` (`TestTenancy_Unauthenticated`) omits Authorization header, asserts 401 distinct from the 404 tenancy tests.

## UNCERTAINTIES

[
 "contract_drift.sh's echo string at line 69 says \"22 routes gated\" for the admin surface, but counting the ADMIN_ROUTES map entries in the same script (and the routes() registration in adminapi/server.go) yields 21 distinct method+path entries \u2014 a minor internal inconsistency in the gate's own log message, flagged rather than silently corrected.",
 "The 'currency' field on /v1/quote is a hardcoded string literal 'credits' (handlers.go:83) rather than a config-driven or ISO-4217 value \u2014 this module's cost model is an abstract integer-credit unit, not real-world money, which could matter for a cross-module compatibility audit expecting a currency code.",
 "I did not open Docs/config-consumers.txt to quote unit_price_credits' exact consumer-pattern regex; the reader call sites in handlers.go/campaign_report.go were verified directly in code instead.",
 "I did not exhaustively grep every single log call site in the entire services/brain tree (e.g. every dispatch/*.go file, boxlink package) for a literal credential/body leak \u2014 spot-checked platformapi/adminapi loggers plus a few dispatch config log lines only.",
 "The single-job report's completed_at (rep.completedAt.UTC(), Go default time.Time JSON marshaling) and the campaign-scoped report's CompletedAt (*string, explicitly formatted '2006-01-02T15:04:05Z') appear to use different timestamp serialization code paths \u2014 noticed but not confirmed whether the two ultimately produce identical wire output or a format inconsistency between the two report shapes.",
 "quote's response is built via a Go map[string]any (handlers.go:80), so JSON key order on the wire relies on encoding/json's alphabetical map-key sort rather than an explicit struct field order \u2014 not a correctness issue but worth noting if a consumer audit checks for stable key ordering."
]