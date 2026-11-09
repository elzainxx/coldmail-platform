# Module 3 — BYO-SMTP sender (Go "Brain" orchestrator, fork of module 2)  
repo: D:/Mnfi/Codmailling 3

## 1. Platform-facing routes
Two Go packages expose HTTP: `internal/platformapi` (Domain A, platform-facing — primary focus) and `internal/adminapi` (Domain B, operator — see category 8). Mounting is in `services/brain/internal/httpapi/health.go:32-54`: `/v1/quote`, `/v1/run`, `/v1/report`, `/v1/describe` are registered as literal paths plus one prefix `/v1/byosmtp/` — all delegate to `platformapi.Server.Handler()`.

### Core four (registered in `platformapi/server.go:140-152`)

**POST /v1/quote** — `handleQuote` (`handlers.go:57`). Request `quoteReq` (`handlers.go:52-55`): `recipient_count *int` (required, must be >=0), `user_id string` (required UUID). Response (map, `handlers.go:106-121` + `quoteFeasibilityJSON`): `recipient_count int`, `cost int` (=unit*count, always 0 in V1), `currency:"credits"`, `unit_price_credits int` (0 by default, byosmtp_0048), `tier:"shared"`, plus feasibility fields spliced in: `feasible bool`, `reasons[]{code,limit?,requested?,cap_name?}`, `caps{queue_depth_cap,daily_send_cap,account_count_cap}`, `capacity{eligible_accounts,daily_capacity_remaining,effective_concurrency,estimated_duration_sec?,estimate_basis?}`, `warnings[]{code,estimated_duration_sec,threshold_sec}`.

**POST /v1/run** — `handleRun` (`handlers.go:131`). Request `runReq`: `job_item_id string` (required UUID), `user_id string` (required UUID, tenant selector). Response: 200 `{"job_id":str,"state":str}` if job_item already had a job (idempotent replay); 202 `{"job_id":str,"state":"queued"}` on fresh accept.

**GET /v1/report** — `handleReport` (`handlers.go:188`). Query params: `job_id`, `job_item_id`, `campaign_id` (at least one of the first three required), `user_id` (required UUID). If `campaign_id` present, delegates to `handleCampaignReport` (see below). Otherwise response (map, `handlers.go:265-291`): `job_id`, `state`, `attempt_count int`, then conditionally present (never null-valued, simply omitted) keys: `provider_message_id`, `completed_at` (UTC time), `account_id`, `account_username`, `smtp_response`, `error_class`, `failed_at`.

**GET /v1/report?campaign_id=** — `handleCampaignReport` (`campaign_report.go:81`). Extra query: `cursor`, `page_size`. Response: `campaign_id`, `dispatch_paused bool`, `totals{total,sent,bounced,failed,in_flight,pending,waiting_for_capacity}`, `totals_by_account[]{account_id,account_username,sent,bounced,failed,in_flight}`, `progress{terminal,fraction_complete,is_complete}`, `items{results[]{job_item_id,state,provider_message_id,completed_at,attempt_count,account_id,account_username,smtp_response,error_class,failed_at},next_cursor,page_size}`. Note: campaignItem's five widened fields are explicit `nil` (not omitted keys) — a deliberately inconsistent shape from the single-job report's map-omission (flagged in code as M3-P7-OD-4).

**GET /v1/describe** — `handleDescribe` (`handlers.go:304`). No request body. Response: `module:"byosmtp"`, `api_version:"v1"`, `capabilities{single_send:true,sequences:false,reply_to_per_campaign:true,open_tracking:false,click_tracking:false,content_variation:true}`, `pricing{currency:"credits",model:"flat_subscription",unit_price_credits,tiers[{id,unit_price_credits,description}]}`, `extension_routes[]` (generated from the registry, `describe_routes.go`), `schemas{}` (JSON-Schema-ish), `bulk_add{warning_code:"credentials_in_plaintext_file",warning_text:str}`.

### Extension routes (Module 3's own; all under `/v1/byosmtp/...`, declared once in `describe_routes.go:41-114` and mounted from the same slice)

- **POST /v1/byosmtp/accounts** — `handleAccountCreate` (`account_create.go:33`). Req `accountCreateReq`: `user_id string`(req,UUID), `smtp_host string`(req), `smtp_port *int`(opt,default 587), `smtp_username string`(req), `smtp_secret string`(req, never stored/echoed), `from_address string`(req, email regex), `from_name *string`(opt), `daily_limit *int`(opt, default from config), `acknowledge_alignment_warning bool`(opt). Resp 201 `accountResource` (see below).
- **POST /v1/byosmtp/accounts/bulk** — `handleAccountBulkAdd` (`account_bulk_add.go:62`). Req `bulkAddReq{user_id string, accounts []bulkAddRow}` where each row mirrors create's fields minus user_id. Resp 200 `{"request_id":str,"results":[]bulkAddRowResult{row int,status str("created"|"rejected"),account_id?,verification?,code?,smtp_response?,from_alignment_state?,from_alignment_message?,details?[]}}`.
- **GET /v1/byosmtp/accounts** — `handleAccountList` (`account_read.go:40`). Query: `user_id`(req), `limit`, `cursor`. Resp `{"items":[]accountResource,"next_cursor":str|null}`.
- **GET /v1/byosmtp/accounts/{id}** — `handleAccountGet` (`account_read.go:12`). Query `user_id`(req). Resp `accountResource`.
- **PATCH /v1/byosmtp/accounts/{id}** — `handleAccountUpdate` (`account_update.go:33`). Query `user_id`(req). Req `accountUpdateReq` (all `*string`/`*int` optional, omit=unchanged; deliberately NO `user_id` field — accounts never re-tenant): `smtp_host,smtp_port,smtp_username,smtp_secret,from_address,from_name,daily_limit` + `acknowledge_alignment_warning bool`. Resp `accountResource`.
- **DELETE /v1/byosmtp/accounts/{id}** — `handleAccountDelete` (`account_delete.go:18`). Query `user_id`(req). Soft delete; resp 204 No Content.
- **GET /v1/byosmtp/accounts/{id}/events** — `handleAccountEvents` (`account_events.go:28`). Query `user_id`(req), `limit`, `cursor`. Resp `{"items":[]accountEvent{id,account_id,user_id,actor,action,from_flags,to_flags,reason,request_id,created_at},"next_cursor"}`.
- **POST /v1/byosmtp/accounts/{id}/disable|enable|pause|unpause** — `doLifecycleTransition` (`account_lifecycle.go:94`), dispatched via 4 thin wrappers. Query `user_id`(req). Req `lifecycleReq{reason *string}` (max 2000 chars). Resp `accountResource`. quarantine/unquarantine (admin verbs) are deliberately ABSENT from this list — 404, never 401, on this surface (asserted by `contract_drift.sh`).
- **POST /v1/byosmtp/accounts/{id}/unquarantine** — `handleAccountUnquarantine` (`account_unquarantine.go:32`). Query `user_id`(req). Req `unquarantineReq{}` (no fields, strict-decode only). Resp `accountResource` or 422 refusal.
- **POST /v1/byosmtp/accounts/{id}/test** — `handleAccountTest` (`account_test_button.go:41`). Query `user_id`(req). Req `accountTestReq{acknowledge_alignment_warning bool}`. Resp `{"verification":{state,smtp_response?,code?}|{state:"unavailable",code},"from_alignment":{state,message?,error?}}`.
- **POST /v1/byosmtp/accounts/{id}/test-send** — `handleAccountTestSend` (`account_test_send.go:13`). Query `user_id`(req). No body fields used. Forces the clicked account and its own from_address (never an arbitrary recipient — M3-P3-OD-5). Resp 202 `{"job_id":str,"state":"sending"}`.
- **GET/PUT /v1/byosmtp/settings** — `handleUserSettingsGet`/`Put` (`user_settings.go:33,81`). Query `user_id`(req). PUT req `{"routing_strategy":*string}` validated against `{random,round_robin,one_by_one}`. Resp `{"user_id":str,"routing_strategy":str}`.
- **GET /v1/byosmtp/usage** — `handleUsage` (`usage.go:40`). Query `user_id`(req), `from`,`to` (YYYY-MM-DD, optional, default last 30 UTC days). Resp `{"user_id","from","to","accounts_configured","accounts_eligible","days":[]usageDay{date,messages_attempted,messages_sent,messages_bounced,messages_failed,campaigns_run,peak_concurrency_used,distinct_destination_domains}}`.

### accountResource shape (`account_store.go:50-65`, returned by create/get/list/update/lifecycle/unquarantine)
`id,user_id,smtp_host,smtp_port,smtp_username,from_address,from_name,has_credential(bool, never the secret),status{paused,pause_reason,quarantined,quarantine_reason?,quarantined_at?,disabled},verification{state,verified_at?,verify_at?,code?,smtp_response?},from_alignment{state,dmarc_policy?,checked_at?,last_attempt_at?,last_attempt_error?,message?,acknowledged_at?},daily_limit,created_at,updated_at`. Never includes `credential_ciphertext`/`nonce`/`key_id`.

## 2. Error envelope
Single uniform shape across the ENTIRE platform surface (and identically shaped, independently duplicated, on adminapi). Defined `platformapi/errors.go:86-94`:
```go
type errorEnvelope struct { Error errorBody `json:"error"` }
type errorBody struct {
  Code      string        `json:"code"`
  Message   string        `json:"message"`
  Details   []fieldDetail `json:"details,omitempty"`
  RequestID string        `json:"request_id"`
}
```
Constructed by `newErr(code, internalReason)` (`errors.go:71`) → `.withDetails(...)` / `.withMessage(...)` → rendered by `writeError` (`errors.go:111-126`) and the single choke point `Server.fail` (`auth.go:87-91`). Every handler in the package ends its error path via `s.fail(w, r, e)` — no route hand-rolls a different shape.

`fieldDetail{Field string, Issue string}` (`errors.go:55-58`) is the field-level vocabulary. `Issue` values used: `required`, `malformed`, `out_of_range`, `not_found`, `conflict` (`errors.go:26-32`).

**External code -> HTTP status map** (`errors.go:14-52`):
| code | status |
|---|---|
| `validation_failed` | 400 |
| `unauthenticated` | 401 |
| `not_found` | 404 |
| `conflict` | 409 |
| `unprocessable` | 422 |
| `rate_limited` | 429 |
| `internal_error` | 500 |

adminapi's mirror (`adminapi/errors.go:22-32`) adds two more codes not present on platformapi: `config_ordering_violation` (400) and `forbidden` (403, reserved/unused in V1).

Internal reason strings (logged, never serialized) observed as literal values across handlers include: `account_create_validation`, `user_not_found`, `duplicate_account_for_user`, `account_cap_reached`, `daily_limit_below_sent_count`, `from_alignment_warning_unacknowledged`, `from_dns_unavailable`, `account_not_found:<id>`, `test_button_rate_limited`, `unquarantine_rate_limited`, `not_in_state:quarantined`, `account_not_eligible_for_test_send`, `no_box_available`, `job_item_not_found`, `job_item_already_accepted`, `user_campaign_active`, `user_suspended`, `max_campaign_size_reached`, `max_queued_jobs_reached`, `dispatch_disabled` (as a quote reason code, distinct vocabulary), `service_token_invalid_or_missing`, `panic_recovered`.

The error envelope is uniform; the only structural inconsistency found is WITHIN success payloads, not the error shape: `campaignItem`'s widened fields render explicit JSON `null` while the single-job `/v1/report`'s equivalent fields are simply omitted keys — an acknowledged (M3-P7-OD-4), deliberate inconsistency in the success contract, not the error contract.

## 3. Auth mechanism
**Header/scheme:** `Authorization: Bearer <token>`, case-insensitive scheme match. Extraction: `bearerToken(r *http.Request)` (`platformapi/auth.go:51-61`); identical logic duplicated in `adminapi/auth.go:53-63`.

**Credential type:** a shared PLATFORM SERVICE TOKEN (Domain A) — one flat set of tokens, no per-caller identity in the token itself (identity/tenancy is carried in the request body/query as `user_id`, not derived from the credential).

**Storage:** bcrypt hashes only. `Authenticator{keyHashes []string}` (`auth.go:17-30`), constructed from `cfg.PlatformTokenHashes` which comes from env var `PLATFORM_TOKEN_HASHES` (comma-separated, `config/config.go:99-100`). The plaintext token never appears in config/DB.

**Comparison:** `authenticate()` (`auth.go:37-48`) iterates ALL configured hashes with `bcrypt.CompareHashAndPassword`, never early-returning on a match — explicit anti-timing-leak comment ("Checks all hashes (no early return) so timing does not reveal which index matched"). Bcrypt itself is constant-time vs. its own stored hash.

**Empty-set posture:** `Enabled() bool` reports whether any hash configured; `main.go:272` only mounts `platformHandler` when `cfg.PlatformEnabled` (== `len(PlatformTokenHashes) > 0`) — fail-closed: unset env var means the entire Domain-A surface is unmounted, not merely unauthenticated.

**Failure response:** 401 `unauthenticated` / internal reason `service_token_invalid_or_missing` (`auth.go:77-81`).

**Middleware:** `requireAuth(next http.HandlerFunc) http.HandlerFunc` (`auth.go:65-84`) wraps every route registered in `routes()` (`server.go:140-152`) — mints a request id, installs a `recover()` that degrades a panic to a clean 500 envelope (never a bare connection reset), then gates on `s.auth.authenticate(bearerToken(r))` before calling `next`.

**One credential type only** on this surface — no API-key-in-query, no mTLS client-cert auth, no second scheme. Domain B (adminapi) uses a STRUCTURALLY SEPARATE `Authenticator`/hash-set (`ADMIN_API_KEY_HASHES`) — Rule 6 explicitly forbids the two domains sharing a credential, and the code enforces this by using two distinct `Authenticator` types/instances (`adminapi.Authenticator` vs `platformapi.Authenticator`) built from two distinct env vars, wired by two distinct constructor calls in `main.go:171,281`.

## 4. Versioning scheme
**URL prefix:** `/v1/` for everything — no `/v2` exists anywhere in the tree. `handleDescribe` reports `"api_version": "v1"` literally (`handlers.go:310`).

**No version header** of any kind is read or written on this surface (no `Accept-Version`, no `X-API-Version` handling found in platformapi or adminapi).

**Additive-only discipline enforced IN CODE, not merely in docs, via three separate mechanisms:**
1. `describe_routes.go`'s `extensionRoutes()` slice is the SINGLE registry both `server.go:150-152`'s mux-mounting loop and `handleDescribe`'s `extension_routes` JSON read — a route cannot exist on the mux without a description and vice versa (structural, not test-time).
2. The CI gate `tests/harness/checks/contract_drift.sh` probes every documented route (unauthenticated, expecting 401 = "exists and gated", 404 = "does not exist") and asserts an UNDOCUMENTED path 404s (`contract_drift.sh:119-120,136-137` — cross-checks `Docs/api-contract.md` literally contains each route string). Runs on every push/PR (`.github/workflows/real-infra.yml:45`).
3. Test-level: `describe_routes_test.go` exists in the package (a T6-style dispositive test — "a client calling only what describe declares" per the file's own header comment) proving the registry and the mux cannot silently diverge.

There is no code-enforced SemVer or deprecation-header mechanism — additive-only is enforced by "the served route set must equal the documented contract", not by a version negotiation protocol.

## 5. Tenant scoping
**Tenant field:** `user_id` (type: canonical UUID string, validated against `uuidRe` = `^[0-9a-fA-F]{8}-...-[0-9a-fA-F]{12}$`, `handlers.go:48`). This IS the tenant/user identifier and it is EXPLICIT on every request — never implicitly derived from the auth credential (the platform service token is shared/flat, per category 3), and never optional.

**How passed:**
- Body field `user_id` on POST/PUT with a JSON body: `/v1/run` (`runReq.UserID`), `/v1/quote` (`quoteReq.UserID`), `/v1/byosmtp/accounts` create (`accountCreateReq.UserID`), `/v1/byosmtp/accounts/bulk` (`bulkAddReq.UserID`).
- Query param `user_id` everywhere else: `/v1/report`, and EVERY `/v1/byosmtp/accounts/{id}...` verb, `/v1/byosmtp/settings`, `/v1/byosmtp/usage` — all funnel through the shared helper `requireTenantSelector(userID string) *apiError` (`account_read.go:103-113`), which returns `validation_failed`/`user_id_required` or `user_id_malformed` distinctly.
- Deliberately ABSENT from `accountUpdateReq` (PATCH) — code comment (`account_update.go:12-16`) explicitly states this closes a tenancy hole module 2 had (a patchable `pool_id` that re-derived `user_id`); the module enforces "an account never changes owner" by the field's structural absence, not a runtime check.

**SQL WHERE-clause scoping (representative, file:line):**
- `getAccount`: `"SELECT "+accountSelectCols+" FROM byosmtp.accounts a WHERE a.id=$1 AND a.user_id=$2 AND a.deleted_at IS NULL"` (`account_store.go:182-184`).
- Account list: `conds := []string{"a.user_id = $1", "a.deleted_at IS NULL"}` (`account_read.go:53`).
- Account events: `conds := []string{"e.account_id = $1", "e.user_id = $2"}` (`account_events.go:46`) — scoped by the event row's OWN stamped `user_id`, not merely by joining through the account, so even a purged/nulled account_id cannot leak cross-tenant.
- Job report: `"... WHERE j.id = $1 AND j.user_id = $2"` / `"... WHERE j.job_item_id = $1 AND j.user_id = $2"` (`store.go:273-286`).
- Job creation (`createJob`, `store.go:42-178`): the job_item lookup is `SELECT campaign_id FROM public.job_items WHERE id = $1 AND user_id = $2` (`store.go:84-86`) and the final INSERT does `SELECT ... FROM public.job_items ji WHERE ji.id = $1 AND ji.user_id = $2` (`store.go:146-153`) — a cross-tenant job_item_id matches 0 rows.
- Usage aggregation: `WHERE user_id = $1 AND created_at >= $2 AND created_at < $3` (`usage.go:117`).
- Campaign report totals/items: `WHERE j.campaign_id = $1 AND j.user_id = $2` (`campaign_report.go:182,288`).

**Cross-tenant behavior:** documented and tested as NO EXISTENCE DISCLOSURE — the SAME 404 `not_found` (never 403) is returned for "belongs to another tenant" as for "genuinely does not exist" (design line: OD-25, `server.go:11-17`). Verified behaviorally in `tenancy_test.go` (`TestTenancy_CrossTenantReportIsNotFound`, `TestTenancy_CrossTenantRunIsNotFound`) which explicitly asserts the cross-tenant 404's `error.code` is byte-identical to a genuinely-nonexistent id's 404 (`tenancy_test.go:155-162`), and that a cross-tenant `run` creates ZERO job rows (`tenancy_test.go:180-186`).

The campaign_id case is a partial exception by design: a campaign_id that is foreign OR simply not-yet-run both yield a clean EMPTY aggregate (`total:0`) rather than 404, because campaign_id is platform-minted, not a module-allocated resource identity (`campaign_report.go:73-80`).

## 6. Status / state vocabulary
All native Postgres ENUM types, additive-only forever from module 3's baseline (CLAUDE.md Rule 5), defined in `db/migrations/byosmtp_0003_create_enums.up.sql` (inherited from module 2) and `byosmtp_0025_new_enums.up.sql` / `byosmtp_0035_account_verification_state.up.sql` (module 3 additions).

**`byosmtp.job_state`** (`0003:7-14`): `queued`, `assigned`, `sending`, `sent`, `failed`, `bounced`. Crosses the boundary as `state` on `/v1/run` and `/v1/report` and campaign items.

**`byosmtp.error_class`** (`0003:16-23`): `bounce_permanent`, `transport_4xx`, `auth_failure`, `throttle`, `timeout`, `unknown`. Crosses the boundary as `error_class` on `/v1/report` (via `providerFailureDetail`, `provider_text.go:36-40`) — only on a non-`sent` state.

**`byosmtp.pool_kind`** (`0003:25-28`): `shared`, `dedicated` — inherited but the underlying pools table/concept was DROPPED at G2; the type itself is not dropped (enum values are additive-only-forever, cannot be removed structurally) but has no live consumer on this boundary except `defaultQuoteTier`/`describeTiers`' string literals `"shared"`/`"dedicated"` in `quote`/`describe`'s `tier`/`tiers[].id` fields, which are plain strings, not bound to the SQL enum.

**`byosmtp.routing_strategy`** (`0003:30-34`): `random`, `round_robin`, `one_by_one` — crosses the boundary as `routing_strategy` on `GET/PUT /v1/byosmtp/settings`, validated app-side against `validRoutingStrategies` map (`user_settings.go:24-28`).

**`byosmtp.event_actor`** (`0003:42-48`, plus additive `'user'` value added `0025:53`): `dispatcher`, `box`, `reclaim_loop`, `admin`, `system`, `user`. Crosses as `actor` on `GET /v1/byosmtp/accounts/{id}/events`. platformapi's own writer always stamps `'user'` (`audit.go:49`).

**`byosmtp.pause_reason`** (`0003:50-54`): `daily_limit`, `throttle`, `manual`. Crosses as `status.pause_reason` on `accountResource`.

**`byosmtp.quarantine_reason`** (`0025:14-19`): `auth_failure`, `consecutive_failures`, `failure_rate`, `manual`. Crosses as `status.quarantine_reason`.

**`byosmtp.from_verification_state`** (`0025:21-27`): `unverified`, `aligned`, `unaligned_warned`, `blocked`. Crosses as `from_alignment.state`.

**`byosmtp.dmarc_policy`** (`0025:29`): `none`, `quarantine`, `reject` (NULL = no DMARC record). Crosses as `from_alignment.dmarc_policy`.

**`byosmtp.suspend_reason`** (`0025:31-34`): `operator`, `subscription_lapsed` — admin-only surface, not directly returned on the platform boundary's accountResource, but its effect (`user_suspended` conflict on `/v1/run`) is visible.

**`byosmtp.notification_severity`** (`0025:36-40`): `critical`, `warning`, `info` — not directly observed rendered on the platform boundary in the files read (notifications delivery is a separate loop; no platformapi GET /notifications route exists).

**`byosmtp.notification_state`** (`0025:42-46`): `pending`, `delivered`, `cleared` (a 4th value `suppressed` added additively later per `byosmtp_0047_notification_state_suppressed.up.sql`).

**`byosmtp.credential_verification_state`** (`0035:32-36`): `unverified`, `verified`, `failed`. Crosses as `verification.state` on accountResource and the literal `state` key in the test-button response.

**Additional string vocabularies not backed by a SQL enum** (module-invented, app-level, additive by convention but not DB-enforced): campaign-report's aggregate distinction `waiting_for_capacity` (never a per-job DB state, purely a query-derived count, `campaign_report.go:9-14`); quote's `reasons[].code` vocabulary (`dispatch_disabled`, `exceeds_queue_cap`, `no_eligible_accounts`, `all_accounts_unverified`, `from_gate_blocked`, `quote_feasibility.go:125-139`); `verification.state` values surfaced by the test button (`unavailable` — an app-level state not in the DB enum, used when no verifier is wired, `account_test_button.go:121`).

## 7. Cost / feasibility expression
**Pre-flight verb: yes — `POST /v1/quote`.** Its purpose was rewritten in Phase 7 (M3-P7-OD-11) from a per-recipient cost lookup into a pure feasibility check, because module 3's actual billing is a FLAT MONTHLY SUBSCRIPTION with no per-send/per-recipient charge (`quote_feasibility.go:1-10`, `module-3-build-plan.md §1` as cited in comments).

**Cost fields (retained for Rule-5 contract compatibility, but INERT):** `cost int` (=`unit_price_credits * recipient_count`, always 0 at default), `currency:"credits"` (literal string), `unit_price_credits int` (config key `unit_price_credits`, seeded `0` by `byosmtp_0048_unit_price_zero.up.sql` — a DELETE+re-INSERT from the module-2-inherited default of `1`, because the live value was contradicting the module's own documented flat-subscription pricing until this migration), `tier string` (`"shared"` — the only tier `quote` can report since it has no campaign context to resolve).

**Feasibility fields — the real substance of `quote`'s answer (`quote_feasibility.go:42-54`, JSON via `quoteFeasibilityJSON`, `quote_feasibility.go:285-333`):**
- `feasible bool` — "CAN this be accepted as submitted," never conflated with "will this be slow" (explicit design comment: a campaign exceeding today's daily capacity is NOT infeasible, since it drains over days).
- `reasons[]{code,limit?,requested?,cap_name?}` — only the FIRST binding reason is reported (ordered priority in `computeQuoteFeasibility`'s switch, `quote_feasibility.go:125-139`): `dispatch_disabled`, `exceeds_queue_cap`, `no_eligible_accounts`, `all_accounts_unverified`, `from_gate_blocked`.
- `caps{queue_depth_cap,daily_send_cap,account_count_cap}` (all `int`).
- `capacity{eligible_accounts,daily_capacity_remaining,effective_concurrency,estimated_duration_sec?,estimate_basis?}` — `estimated_duration_sec` (int, SECONDS) and `estimate_basis` (string, literal `"current_eligible_capacity"`) are REQUIRED/non-null whenever `feasible:true` (never omitted, even for a tiny campaign) — computed by `estimateDurationSec` (`quote_feasibility.go:183-193`), a rough arithmetic projection over `(sendDelayMin+sendDelayMax)/2` per lane, explicitly documented as "a projection, not a promise."
- `warnings[]{code:"long_duration",estimated_duration_sec,threshold_sec}` — populated when the estimate crosses the config-driven `quote_long_duration_warn_sec` threshold; empty array (never omitted) otherwise.

**Units/precision:** all durations are plain integer SECONDS (no currency/decimal precision anywhere — `cost`/`unit_price_credits` are plain integers, always 0 under this module's pricing model). No currency-precision (e.g. cents) handling exists because there is no real per-unit charge.

**No reservation/side effect:** `quote` is explicitly pure/side-effect-free — "computed fresh on every request (no caching, no reservation)" (`quote_feasibility.go:37-41`).

## 8. Admin surface + its auth
**Package:** `internal/adminapi`, mounted under `/v1/admin/*` (`httpapi/health.go:37-39`).

**Auth:** a SEPARATE `Authenticator` (`adminapi/auth.go:15-31`) — bcrypt-hashed operator bearer keys from env `ADMIN_API_KEY_HASHES` (`config/config.go:93-95`), constant-time comparison, same fail-closed-when-empty posture, same `Authorization: Bearer` scheme, same panic-recovering `requireAuth` wrapper (`adminapi/auth.go:72-95`). Structurally a DIFFERENT credential from the platform service token per Rule 6 — two distinct `Authenticator` types/instances, two distinct env vars, two distinct constructor calls in `main.go:171` (admin) vs `main.go:281` (platform); nothing shares code or state between them (OD-7 package-isolation: adminapi does not import platformapi or vice versa).

**Route count:** exactly 19, enumerated once in `adminapi/server.go:105-149`'s `adminRoutes()` slice (the single source `routes()` mounts from AND the test-only route-enumeration test iterates for the '19th route is safe' guarantee):
```
POST   /v1/admin/accounts
GET    /v1/admin/accounts
GET    /v1/admin/accounts/{id}
PATCH  /v1/admin/accounts/{id}
DELETE /v1/admin/accounts/{id}
GET    /v1/admin/accounts/{id}/events
POST   /v1/admin/accounts/{id}/quarantine
POST   /v1/admin/accounts/{id}/unquarantine
POST   /v1/admin/accounts/{id}/disable
POST   /v1/admin/accounts/{id}/enable
POST   /v1/admin/accounts/{id}/pause
POST   /v1/admin/accounts/{id}/unpause
GET    /v1/admin/config
GET    /v1/admin/config/{key}
PUT    /v1/admin/config/{key}
GET    /v1/admin/health
POST   /v1/admin/users/{id}/suspend
POST   /v1/admin/users/{id}/unsuspend
GET    /v1/admin/users/{id}/config
PUT    /v1/admin/users/{id}/config
DELETE /v1/admin/users/{id}/config/{dial}
```
(Note: `contract_drift.sh`'s own comment says "21 routes gated" — this counts distinct method+path probes in its own fixed test map, which appears to enumerate slightly differently than the canonical `adminRoutes()` slice; the registered-route count per the Go source is 19.)

**PARALLEL WRITE PATHS to the same resources the platform surface also writes ("the admin bypass surface" — a standing CLAUDE.md concern):**
- `POST/PATCH/DELETE /v1/admin/accounts...` writes the SAME `byosmtp.accounts` rows `/v1/byosmtp/accounts...` writes — both paths share the credential Encryptor pattern (separate `Encryptor` instances, same KMS key material) and both run verify-on-add through the SAME `*dispatch.Verifier`, wired via two structurally distinct adapter types (`verifierAdapter` for platform, `adminVerifierAdapter` for admin — `main.go:409-452`).
- `POST /v1/admin/accounts/{id}/disable|enable|pause|unpause` duplicates the exact same four lifecycle transitions the platform surface exposes at `/v1/byosmtp/accounts/{id}/...` — two independent handler implementations (not shared code, OD-7) that must both honor the SAME precondition.
- `POST /v1/admin/accounts/{id}/quarantine` and `.../unquarantine` are ADMIN-ONLY — deliberately absent from the platform surface except platform's own separate, rate-limited self-serve `/v1/byosmtp/accounts/{id}/unquarantine`, a DIFFERENT code path with its own atomic rate-limit statement, not a re-scope of admin's unquarantine.
- `GET/PUT /v1/admin/users/{id}/config` (admin-only `user_config_overrides` table) is a STRUCTURALLY DISTINCT table from the user-writable `GET/PUT /v1/byosmtp/settings` (`user_settings` table) — enforced by being two different tables rather than a permission check on one.
- `GET /v1/admin/health` has no platform-surface equivalent.

## 9. Health / observability
**Liveness/readiness (`internal/httpapi/health.go`), UNAUTHENTICATED:**
- `GET /healthz` (`health.go:60-62`): 200 always, body `{"status":"ok","at":<RFC3339 time>}`.
- `GET /readyz` (`health.go:66-83`): checks `db.Pool.Ping(ctx)` AND `guard.Healthy(ctx)` (singleton lock genuinely held). 200 when both true, else 503. Body `{"status":"ready"|"not_ready","db":bool,"singleton_lock":bool,"at":time}`.

**Operator health (`GET /v1/admin/health`, operator-key auth, `adminapi/health_handler.go`):** response struct `healthResponse` (`health_handler.go:13-62`):
- `generated_at string`, `window_sec int`.
- `dispatch{dispatch_paused bool, ticks_per_min float64, sends_last_window int, send_rate_per_min float64}`.
- `queue{queued,assigned,sending,sent,failed,bounced,waiting_for_capacity}` (all int).
- `boxes{active,stale,disabled int, oldest_active_heartbeat_age_sec *int (omitempty)}`.
- `accounts{healthy,paused,quarantined,disabled,at_daily_limit}` (all int).
- `users []{user_id,eligible_accounts,headroom}` — per-user headroom rows (renamed from module 2's per-pool shape).
- `errors_last_window map[string]int` — ALWAYS includes all 6 fixed keys (`bounce_permanent,transport_4xx,auth_failure,throttle,timeout,unknown`) even at 0 (`errorsLastWindowClasses`, `health_handler.go:101-103`).
- `shadow{autoquarantine_mode string, would_quarantine_count int, recent[]{account_id,trigger,at}}`.
- `active_conditions[]{condition,severity,detail map[string]any,since}` — built by `health_alerts.go`'s `buildActiveConditions`.
- `destination_throttles[]{destination_domain,window_sec,throttle_events,distinct_users_affected,distinct_accounts_affected,first_seen_at,last_seen_at}` — deliberately has NO response-text field.

**Alert webhook (untrusted outbound egress):** `webhookPayload` (`health_alerts.go:184-191`): `condition,severity,subject{type,id},detail map[string]any,fired_at,module` — explicitly two-vocabulary-clean. Fired via `fireWebhooks` in its own goroutine per NEW rising edge, deduped in-process via `alertDedup` (`health_alerts.go:198-236`), controlled by config key `alert_webhook_url` (empty string = disabled).

**Metrics endpoint:** no `/metrics` / Prometheus endpoint exists anywhere in the codebase — observability is entirely the custom `/v1/admin/health` JSON snapshot plus structured stdout logging; no metrics library import found in `go.mod`.

**Structured logging:** plain `log.Printf` (standard library `log` package), NOT JSON. `platformapi/auth.go:95-101` and `adminapi/logger.go` mirror each other: `"platformapi <reject|error> rid=%s %s %s: ..."` / `"admin request_id=%s method=%s path=%s outcome=error code=%s reason=%s level=%s"`. `request_id` (a `req_<32-hex>` opaque token minted per-request, `errors.go:97-103`) is emitted on every log line and echoed in the client error envelope, bridging the two vocabularies. `main.go:36-37` sets `log.SetFlags(log.LstdFlags | log.LUTC)` and `log.SetPrefix("brain ")` globally — one process-wide logger, no per-package logger instance, no JSON formatter, no correlation-id header accepted from the client.

## 10. DB boundary
**Schema:** `byosmtp` (created `byosmtp_0000_bootstrap`/`0001_create_schema`, `AUTHORIZATION byosmtp_owner`; `REVOKE ALL ON SCHEMA byosmtp FROM PUBLIC` — `0001_create_schema.up.sql:14-15`).

**Owning role:** `byosmtp_owner` — explicitly NON-LOGIN, NON-SUPERUSER (CLAUDE.md Rule 6; every enum type and table is `ALTER ... OWNER TO byosmtp_owner`, e.g. `0003_create_enums.up.sql:57-63`, `0010_create_config_and_seed.up.sql:25`).

**App/login role:** `byosmtp_module` — the only role Brain connects as at runtime (`db.Connect(ctx, cfg.DatabaseURL)`, `main.go:48`, explicit comment "never a superuser at runtime").

**Grants — explicitly per-table, never relying on default privileges** (ADR-0008's documented trap): `byosmtp_0012_grants_and_rls.up.sql:18-29` grants full DML (`SELECT,INSERT,UPDATE,DELETE`) on `pools,accounts,boxes,jobs,config`, restricted DML on append-only tables (`job_events`: `SELECT,INSERT` only; `send_logs`: `SELECT,INSERT,DELETE`, no UPDATE), plus `GRANT USAGE,SELECT ON ALL SEQUENCES`. Every Module-3-added table repeats this discipline explicitly at its own creation migration: `user_settings` (`0029:22`), `user_config_overrides` (`0030:56`), `user_suspensions` (`0031:37`), `campaigns` (`0032:56`), `notifications` (`0033:54`) — all `GRANT SELECT, INSERT, UPDATE, DELETE ... TO byosmtp_module`, each in the SAME migration file that creates the table.

**RLS:** ENABLEd on all `byosmtp.*` tables (`0012:38-45`); `FORCE ROW LEVEL SECURITY` specifically on `byosmtp.accounts` only ("crown jewel", `0012:40`) — binds even the table owner `byosmtp_owner`. `byosmtp_module` gets a permissive `USING(true) WITH CHECK(true)` policy per table (`sender_module_full_access`, `0012:47-60`) — RLS is a defense-in-depth backstop against a future misgrant, NOT the tenant-isolation mechanism; tenant scoping is explicitly a module-QUERY responsibility (every WHERE clause carries `user_id = $N`), not RLS-enforced per-row.

**Cross-schema access:** SELECT-only grants on `public.users` and `public.job_items` (platform-owned tables), issued by the bootstrap migration and deliberately never re-issued/revoked by later migrations (`0012.up.sql:13-15`) — the module can read but never write platform-owned identity/job-item data.

**Which services hold DB credentials:** the Brain connects to Postgres as `byosmtp_module`; the box (Node.js sender) has NO DB driver shipped and NO DB credential mounted per CLAUDE.md Rule 6 — structurally cannot reach Postgres at all. The migration runner (`tools/byosmtp-migrate`) runs as a separate, more-privileged admin/owner-capable role, distinct from the runtime `byosmtp_module` role.

## 11. Config approach
**Mechanism:** exclusively `byosmtp.config` DB table (`key TEXT PRIMARY KEY, value JSONB, default_value JSONB, description TEXT, value_type TEXT, updated_at, updated_by` — `byosmtp_0010_create_config_and_seed.up.sql:8-23`). Values are JSONB-wrapped `{"v": ...}` and read fresh per request/tick (no long-lived cache except the deliberately-scoped `fromgate.Cache` for DNS TTLs, constructed once at server startup — `platformapi/server.go:111-127`, `adminapi/server.go:71-85`).

**Count:** 87 keys currently seeded, per `Docs/config-manifest.txt` (`grep -c '^[a-z]'` = 87), tracked phase-by-phase in that file's own running commentary and enforced live-vs-doc-vs-manifest equality by the `config_drift` CI gate.

**Hot-reloadable:** yes for nearly every key — read fresh via small scoped readers (`readConfigInt`/`readConfigString`/`readConfigBool`, `platformapi/config_read.go:11-56`, independently duplicated in adminapi per OD-7) on every request/tick; no redeploy needed. Explicit exceptions documented in code: the two singleton-guard timeouts read once pre-lock-acquire with an env fallback ONLY for the pre-migration bootstrap window (`main.go:362-407`), and the `fromGateCache` TTLs (restart-only refresh, by design).

**Env vars used (process-wiring only, not tuning dials, per Rule 1's bootstrap exception):** `DATABASE_URL`, `BRAIN_HEALTH_ADDR`, `BRAIN_BOXLINK_ADDR`, `BRAIN_BOXLINK_STREAM_ADDR`, `KMS_DEV_KEY_FILE`, `BRAIN_SINGLETON_ACQUIRE_TIMEOUT_SEC` (bootstrap-only fallback), mTLS cert/key/CA paths, `DEV_BOX_ID`/`DEV_BOX_TOKEN_HASH`, `ADMIN_API_KEY_HASHES`, `CREDENTIAL_KEY_ID`, `PLATFORM_TOKEN_HASHES` (`config/config.go:51-103`).

**CI gate against hardcoded constants:** `config_consumption.sh` is a SEPARATE BLOCKING gate from `config_drift` — it asserts every seeded/documented key has a REAL CONSUMER (a grep-verified read site outside the loader/validator/tests) or an explicit, expiring `RESERVED` marker in a ledger; a `RESERVED` marker past its stated phase-expiry FAILS the gate. There is no separate "no hardcoded literal" static-analysis gate — the discipline is enforced by requiring every behavioral literal to be a config key with a consumer, backstopped by code comments (e.g. `account_validate.go`'s `dailyLimitCeiling = 100_000` is explicitly called out as "a hardcoded sanity BOUND, not a tuning dial" — `account_validate.go:57-59`).

**Hardcoded literals actually found in runtime code (flagged in-code as deliberate, non-tuning):** `dailyLimitCeiling = 100_000` (`platformapi/account_validate.go:59`); `defaultQuoteUnitCredits = 0` and `defaultQuoteTier = "shared"` (`handlers.go:25,32` — documented unreachable fallbacks); `defaultAccountCountCap=20`, `defaultBulkAddMaxRows=500`, `defaultBulkAddVerifyIntervalMS=2000`, `defaultAccountTestRateLimit=5`, `defaultTestButtonRateLimitPerUser=20` — all mirror the migration's own seeded `default_value` as fallback-only. `dbCtx` uses a bare `10*time.Second` DB-operation timeout (`server.go:156-158`, `adminapi/server.go:170-172`) — NOT config-driven, a candidate genuine hardcoded runtime literal not tied to a config key (unlike the smtp/dispatch timeouts, which are all config keys).

## 12. Migration conventions
**Naming pattern:** `byosmtp_NNNN_description.{up,down}.sql`, 4-digit zero-padded, strictly contiguous, enforced by `migration_lint.sh`. 98 `.sql` files total in `db/migrations/` (49 up/down pairs).

**Up/down presence:** mandatory pairing — `migration_lint.sh` fails if any `.up.sql` lacks a matching `.down.sql`. Down files may be a documented forward-only no-op (e.g. `0025`'s down cannot literally reverse `ALTER TYPE ... ADD VALUE 'user'` since Postgres has no `DROP VALUE` — documented rather than faked, per M3-P1-OD-7).

**Runner:** custom Go tool `tools/byosmtp-migrate` (`main.go`, `runner.go`), NOT a third-party migration framework. Has its own `contiguity_test.go` and `runner_integration_test.go`.

**Applied-migration tracking:** self-tracking — each `.up.sql` file itself contains the final statement `INSERT INTO byosmtp.schema_migrations (version) VALUES ('NNNN')` (e.g. `0012_grants_and_rls.up.sql:62`) rather than the runner writing that row externally; `byosmtp.schema_migrations` is created by the Phase-0 bootstrap and re-asserted idempotently by `0001` (`0001_create_schema.up.sql:19-25`).

**Migration lint gate:** `tests/harness/checks/migration_lint.sh` — BLOCKING CI gate checking filename pattern, up/down pairing, chain contiguity from 0000/0001, each file self-records its OWN version, each down-file either deletes that version row or documents a forward-only no-op, and that the newest migration is present in a `wantChain` literal inside a lifecycle test file.

**Bootstrap exception:** `byosmtp_0000_bootstrap` predates `byosmtp.config`'s existence, so the migration RUNNER's OWN `lock_timeout`/`statement_timeout` are CLI-flag defaults rather than config-table-driven — the ONE documented exception to config-not-code (CLAUDE.md §1).

## 13. Kill switches and shadow modes
All are `byosmtp.config` rows (config-not-code), read fresh per tick/request, no code redeploy needed.

| Key | Type/values | Default | Read at (file:line) |
|---|---|---|---|
| `dispatch_enabled` | bool | `true` | `dispatch/dispatch.go:164` ("dispatch_paused_by_operator" log line when false); also `platformapi/quote_feasibility.go:63` and `platformapi/campaign_report.go:357-368` (`readDispatchPaused`, negation reported as `dispatch_paused`) |
| `autoquarantine_mode` | string enum `off\|shadow\|enforce` | `shadow` | `dispatch/account_effect.go:75-116` — a THREE-RUNG LADDER, not a binary switch: `off` = decision never even evaluated (zero extra DNS/DB query); `shadow` = decision computed and logged as a separate would-quarantine observation row, flag NOT set; `enforce` = flag actually set. Unrecognized value defensively falls back to `shadow`, NEVER `enforce` (`account_effect.go:79-81`) |
| `autopause_enabled` | bool | `true` | `dispatch/account_effect.go` (Chunk 4 seam #2, ~line 117-120) — simple on/off (no ladder — "auto-pause is self-reversing") |
| `variation_enabled` | bool | `true` | `dispatch/variation.go` (content-variation 12-toggle system; all-off = byte-identical output) |
| `alert_webhook_url` | string (URL or `""`) | `""` (off) | `adminapi/health_handler.go:150-155` — read fresh per `/v1/admin/health` poll, fired in a background goroutine via `fireWebhooks` |
| `notifications_enabled` | bool | (seeded, Phase 7) | `main.go:254` wires `NotificationRunner` — explicitly documented as NOT coupled to `dispatch_enabled` |
| `from_gate_mode` | string enum | `enforce` | `platformapi/from_gate.go:54-57`, `adminapi/from_gate.go` — gates the From-address DNS/SPF/DMARC alignment check; fail-closed on read error (falls back to `enforce`, not `off`) |

**Additional per-subsystem gates:** `quote_capacity_projection_enabled` (bool, default true) — turns off the `estimated_duration_sec`/`estimate_basis`/`warnings` half of `/v1/quote`'s response while feasibility itself still works (`quote_feasibility.go:151-162`); `brain_singleton_reacquire_interval_sec` (int, default-fallback 0=disabled) — controls whether a Brain that LOSES its singleton lock mid-life ever re-attempts acquisition; `0` = "never re-acquire... paused-and-honest" (`main.go:384-407`).

**Shadow-mode-by-default confirmed:** `autoquarantine_mode` defaults to `shadow` ("the documented launch-safety default, never to 'enforce'") — the one subsystem explicitly shadow-by-default per CLAUDE.md Rule 7.

**Ladder vs binary summary:** `autoquarantine_mode` is the only true 3-rung graduated ladder (off/shadow/enforce) observed in the files read; every other kill switch found is a plain boolean on/off. No 4+-level ladder was found.

## 14. Logging discipline
**Rule/gate forbidding secret logging:** CLAUDE.md Rule 6 plus explicit package-level comments on `adminapi/logger.go:8-13` and `platformapi/auth.go` reiterating the two-vocabulary rule specifically for logging: "the admin service NEVER logs request bodies... the smtp_secret and the ciphertext bytes structurally cannot appear in a log line."

**Automated CI enforcement — TWO separate gates:**
1. `tests/harness/checks/no_secret_leak.sh` — BLOCKING CI gate that greps the RUNNING service logs (`docker compose logs`, including the `multibox` profile) for the literal dev box registration token and its bcrypt hash (both box 1 and box 2's), failing if either string is found anywhere in service output. Explicitly scoped: box token only — "the SMTP credential is a different credential on a different path... its leak scan lives with the send harness — `tests/harness/phase3_send.sh` S1, which greps plaintext AND base64 in logs and in the DB."
2. `tests/harness/checks/user_output_scrub.sh` — BLOCKING CI gate, a STATIC source-grep with four independent checks: (1) every read of `jobs.last_smtp_response` must be inside its single named accessor `platformapi/provider_text.go` — any other reader is flagged as a bypass; (2) `jobs.last_error_message` (the internal-slug-carrying column) must have ZERO readers anywhere in `platformapi/`; (3) the notifications-payload render path (once built) must draw from the accessor, not a raw column read — vacuously green until that code exists; (4) no unledgered consumer of `boxproto.Report.SMTPResponse` exists outside `Docs/user-output-readers.txt`'s ledger, with an anti-vacuity companion check that every ledgered file is still a genuine consumer.

**Actual log line format observed** (`platformapi/auth.go:95-101`, `adminapi/logger.go:16-44`): plain `log.Printf` lines like `"platformapi error rid=%s %s %s: code=%s reason=%s level=%s"` and `"admin request_id=%s method=%s path=%s outcome=error code=%s reason=%s level=%s"` — NEVER interpolate the request body or a raw field value directly; internal-reason strings occasionally concatenate `err.Error()` (e.g. `account_create.go:315` `newErr(codeInternalError, "account_write_error: "+err.Error())`), but this is logged only via `internalReason`, never serialized to the client (two-vocabulary rule), and most pg errors are pre-translated by `translateAccountWrite`/`pgCode`/`pgConstraint` before reaching this fallback.

**No direct evidence found of an actual violation** in the files read — no `log.Printf` call was found interpolating `smtp_secret`, ciphertext, or a bearer token literal. The credential value is `zero()`-wiped (`crypto.go:82-86`) immediately after use in every encrypt path, reducing the window such a bug could even occur in.

## 15. Tests
**Real Postgres / real HTTP, never mocks** — CLAUDE.md Rule 3 is followed structurally: every platform-boundary test clones a REAL migrated Postgres template DB per test (`newTestEnv`, `platformapi/harness_test.go:40-90`: `cloneDB` does `CREATE DATABASE ... TEMPLATE ...`), connects as the REAL `byosmtp_module` role (`withUserPass(cloneDSN, "byosmtp_module", modulePassword())`), constructs the ACTUAL production `platformapi.Server` (not a stub), and drives it over REAL HTTP via `httptest.NewServer(srv.Handler())`.

**Test files covering the platform boundary** (in `services/brain/internal/platformapi/`): `tenancy_test.go`, `account_tenancy_test.go`, `account_crud_test.go`, `account_bulk_add_test.go`, `account_cap_test.go`, `account_cap_race_test.go`, `account_delete_test.go`, `account_autoclear_test.go`, `account_test_button_test.go`, `account_test_button_per_user_test.go`, `account_test_send_test.go`, `account_unquarantine_test.go`, `account_unquarantine_race_test.go`, `account_uniqueness_test.go`, `attribution_test.go`, `campaign_report_test.go`, `campaign_test.go`, `quote_test.go`, `quote_validation_test.go`, `unitprice_test.go`, `usage_test.go`, `user_settings_test.go`, `notification_clear_test.go`, `report_notfound_selector_test.go`, `report_widening_test.go`, `resource_caps_test.go`, `describe_routes_test.go`, `from_gate_test.go`. Tests SKIP (not silently pass) when `TEST_ADMIN_DATABASE_URL`/`TEST_TEMPLATE_DB` env vars are unset (`requireAdminDSN`/`requireTemplate`, `harness_test.go:151-167`).

**Paired positive/negative control convention — concretely demonstrated:** `TestTenancy_CrossTenantReportIsNotFound` (`tenancy_test.go:122-163`): (a) POSITIVE control — tenant A reads A's own job by `job_id` → 200 with the correct `job_id` echoed back (`140-143`); (b) NEGATIVE — tenant B reads A's job by `job_id` → 404 `not_found` (`146-149`); (c) a SECOND negative via `job_item_id` → also 404 (`151-154`); (d) an explicit ANTI-VACUITY assertion that the cross-tenant 404's `error.code` is byte-identical to a genuinely-nonexistent ghost-id's 404 (`157-162`) — the test that would FAIL if tenant scoping were broken (a dropped `user_id = $2` predicate would return 200 in step b).

**Companion cross-tenant WRITE test:** `TestTenancy_CrossTenantRunIsNotFound` (`tenancy_test.go:168-194`) additionally queries the DB DIRECTLY (`SELECT count(*) FROM byosmtp.jobs WHERE job_item_id=$1`) to assert ZERO rows were created by the rejected cross-tenant `run` attempt (`180-186`), paired with a positive control that A's own run creates exactly one row frozen to A (`188-193`).

**Auth-vs-tenancy separation control:** `TestTenancy_Unauthenticated` (`tenancy_test.go:223-245`) proves the two gates are independent — no Authorization header gets 401 `unauthenticated` (a different code/status than the tenancy 404).

**Concurrency tests present:** `account_cap_race_test.go`, `account_unquarantine_race_test.go` — named per CLAUDE.md Rule 10's anti-vacuity discipline, wired into the harness's `go_suite services/brain/internal/platformapi` step (`tests/harness/run.sh:290`, labeled "Platform API tenancy (OD-25) + one-campaign-per-user (Phase 4 P4-7, incl. concurrent-attempt serialization)") — their exact contention-proof mechanism was not independently re-verified in this pass beyond confirming the files exist and are wired in.

## UNCERTAINTIES

[
 "contract_drift.sh's own log line says \"21 routes gated\" for the admin surface and \"20 routes gated\" for the platform surface, but the canonical adminRoutes()/extensionRoutes() slices I counted yield 19 admin routes and 17 platform extension routes (+4 core = 21 total platform-surface routes including quote/run/report/describe). I did not fully reconcile the exact discrepancy between the gate's own comment-count and a literal Go-slice count \u2014 likely the gate's fixed ADMIN_ROUTES/PLATFORM_ROUTES bash associative arrays enumerate some method+path pairs slightly differently (e.g. counting GET+PUT on the same path as 2) than the Go route-registration slices do; this is a minor documentation-vs-shell-script discrepancy, not a functional ambiguity.",
 "I did not read internal/fromgate's full evaluator.go, internal/dispatch's full config.go/outcome.go, or internal/adminapi's config_validate.go/config_ordering_write.go in depth \u2014 my characterization of the from_gate_mode kill switch's exact allowed string values (off/warn_only/enforce) is inferred from comments and default-fallback code rather than the enum/CHECK constraint definition itself, which I did not directly open.",
 "I did not verify whether a genuine pg_stat_activity-scoped contention proof (per CLAUDE.md Rule 10) is actually present inside account_cap_race_test.go / account_unquarantine_race_test.go \u2014 I confirmed the files exist and are wired into the test harness but did not read their bodies, so I cannot personally confirm they satisfy the anti-vacuity requirement rather than merely asserting 'no 40P01 occurred'.",
 "The exact behavior of GET /v1/report when BOTH job_id/job_item_id AND campaign_id are supplied is documented in a comment as 'campaign_id wins' but I did not find a dedicated test asserting this precedence.",
 "Docs/api-contract.md itself was not read in this pass (per task instructions, docs are not evidence) \u2014 my route inventory is derived entirely from describe_routes.go, server.go, adminapi/server.go's Go source, plus the contract_drift.sh gate's own probe list (code/test evidence, not doc evidence).",
 "I did not inspect internal/dispatch/notify.go or the notifications table's read path in depth, so I cannot state definitively whether there is (or is not) a platform-facing GET /v1/byosmtp/notifications-style route \u2014 none was found in describe_routes.go's extensionRoutes() slice, so I report its absence, but this is drawn from one file rather than an exhaustive route-mount search across every package."
]