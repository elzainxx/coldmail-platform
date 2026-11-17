# Phase 3 research: module 2 (send-managed): the platform-facing API, and a real worker plus SMTP sink in the platform harness

Subject: `coldmail-send-managed` (M2) at the pinned commit `ff8c302` (tools/modules.lock; the working tree is that commit, clean). Everything below is read from M2's CODE, not its docs. Citations are
`M2:<path>:<line>` (paths inside the M2 repository) and `P:<path>:<line>` (inside the platform repository). Docs were used only to cross-check, and where a doc and the code differ the code is what is
written here (section 6, items 14 and 15). Nothing was run: Docker, Postgres and the harness cannot be run in this environment, so every statement about behaviour at run time is a reading of code and is
marked UNPROVEN where it matters (section 11 says how one runner run settles each). JSON shown is assembled from the handlers' struct tags and map literals, not captured from a live module.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 1. The surfaces, the credentials, the envelope

One Go process (the Control) serves everything on ONE listener, `CONTROL_HEALTH_ADDR` (`:8080` in the platform compose). Two more mTLS listeners serve workers (`:8443` HTTP register/heartbeat, `:8444`
the persistent send stream); the Node worker uses only `:8444` (M2:services/worker/src/index.js:34-52, the `port` is `CONTROL_STREAM_PORT`).

| Surface | Routes | Credential | Switch |
|---|---|---|---|
| probes (no auth) | `/healthz`, `/readyz` (registered WITHOUT a method: any verb answers) | none | always on (M2:services/control/internal/httpapi/health.go:38-39) |
| Domain A, the platform's | `POST /v1/quote`, `POST /v1/run`, `GET /v1/report`, `GET /v1/describe`, `POST /v1/cancel` | service token, `Authorization: Bearer <token>` | `PLATFORM_TOKEN_HASHES` (comma list of bcrypt hashes); unset = surface not mounted, the five paths answer the root mux's coded 404 (M2:services/control/internal/config/config.go:97-100; cmd/control/main.go:220-228; httpapi/health.go:44-52,66) |
| Domain B, the operator's | `/v1/admin/*` (section 7) | operator key, `Authorization: Bearer <key>` | `ADMIN_API_KEY_HASHES` (comma list of bcrypt hashes); unset = not mounted (config.go:93-95; main.go:107-123) |

- The two credentials never cross: the platform token on `/v1/admin/*` is 401 `unauthenticated` and so is the admin key on `/v1/quote` (M2:services/control/internal/adminapi/auth.go:88-91; platformapi/auth.go:77-80).
- Verification is `bcrypt.CompareHashAndPassword` against EVERY configured hash on EVERY request, no early return, no cache (platformapi/auth.go:37-48). Each call therefore costs one bcrypt verify per
  configured hash (the hashes `provision-worker admin-key` mints use `bcrypt.DefaultCost`, M2:tools/provision-worker/main.go:106-112): poll at seconds, not milliseconds.
- Order of events for a request: routing first (an unknown path is 404, a wrong verb 405 with `Allow`, BOTH answered before the credential is looked at, platformapi/method_dispatch.go:221-248), then the
  token (401), then the handler's own validation. A valid token on an unknown path is 404, never 401.
- Token and key are minted for the harness with M2's own tool: `provision-worker admin-key` prints `ADMIN_API_KEY=<plaintext>` and `ADMIN_API_KEY_HASH=<bcrypt>` (P:tests/harness/run.sh:65-70 already does this
  for the service token).

### The error envelope (both domains)

```json
{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"user_id","issue":"required"}],"request_id":"req_<32 hex>"}}
```

- `message` is a FIXED sentence per code (platformapi/errors.go:52-60); `details` is optional and omitted when empty (errors.go:104); `request_id` is `req_` + 32 hex, minted per request (errors.go:109-115),
  present on every error including routing errors and the root mux's (httpapi/health.go:79-98). It is NOT returned as a response header on a success.
- Domain A codes (the closed set, published live as `describe.error_codes`, derived from the same map): 400 `validation_failed`, 401 `unauthenticated`, 404 `not_found`, 405 `method_not_allowed`,
  409 `conflict`, 500 `internal_error`, 503 `paused` (errors.go:11-19, 62-70; describe_vocabularies.go:89-100). Domain B adds 400 `config_ordering_violation`, 403 `forbidden` (never emitted),
  422 `unprocessable`, 429 `rate_limited` (declared, never emitted: no code path writes it) (adminapi/errors.go:16-67).
- `details[].issue` on Domain A is exactly `required | malformed | out_of_range | not_found` (errors.go:45-50; published as `vocabularies.detail_issue`). Domain B adds `conflict`, `forbidden_value`.
- Internal reasons are logged against the request id and never serialized, so the adapter cannot tell two 409s or two 500s apart from the body (oddities 5, 6).

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 2. The routes the adapter uses

Body decoding on Domain A: `json.Decoder` with `DisallowUnknownFields`; an EMPTY body is allowed and falls through to the required-field checks; trailing bytes after the first JSON value are NOT rejected;
there is NO body size cap and no Content-Type check (platformapi/handlers.go:433-460). A UUID is any `8-4-4-4-12` hex string, either case, version not checked (handlers.go:45).

### 2.1 `POST /v1/quote` (handlers.go:47-119)

Request `{"recipient_count": 3}`; optional `"user_id": "<uuid>"` (accepted, validated if present, otherwise IGNORED: M2's price has no tenant term, handlers.go:51-63, 90-102).

Response 200:
```json
{"recipient_count":3,"cost":3,"currency":"credits","unit_price_credits":1,"tier":"shared"}
```
- `cost = recipient_count * unit_price_credits`, the unit read live from `send_managed.config.unit_price_credits` on every call (seed 1; the PUT validator allows 0..1000, adminapi/config_validate.go:242-246);
  an unreadable config row falls back to 1 silently (handlers.go:415-427). `tier` is the constant `"shared"` (handlers.go:31).
- Errors: `recipient_count` absent or null -> 400 `[recipient_count:required]`; negative -> 400 `[recipient_count:out_of_range]`; a string -> 400 `[recipient_count:malformed]`; `user_id` present and not a UUID ->
  400 `[user_id:malformed]`; empty-string `user_id` is treated as absent.
- WHAT QUOTE VALIDATES: nothing about eligibility, capacity, pools, accounts, workers or the dispatch rung. It is arithmetic. It is never gated by the rung. There is no upper bound on `recipient_count`
  (a 64-bit multiply; see oddity 9). Whether the tenant can actually be served is visible only AFTER `run`, in the campaign report (`waiting_for_capacity`, section 4.4).

### 2.2 `POST /v1/run` (handlers.go:121-202; store.go:15-119)

Request: `{"job_item_id":"<uuid>","user_id":"<uuid>"}`, both required. The module does NOT take a campaign id here: it reads it, with the recipient, subject, body and reply-to, from the platform's
`public.job_items` row (store.go:61-65, 87-94). So the platform must have COMMITTED the `job_items` row (`id`, `campaign_id`, `user_id`, `recipient_email`, `rendered_subject`, `rendered_body`, `reply_to`) before it calls run.

Check order: token -> decode -> `job_item_id` -> `user_id` -> rung -> `createJob`.
- 202 `{"job_id":"<uuid>","state":"queued"}`: a NEW job was created (state `queued`, `next_attempt_at = now`) and the dispatcher was kicked asynchronously (handlers.go:197-201).
- 200 `{"job_id":"<uuid>","state":"<current state>"}`: a job for that `job_item_id` and that tenant already exists. The state is whatever it is NOW (it can be `sent`, `bounced`, `failed`, `cancelled`).
  This is the idempotency (section 4.1). No second job, no second send, no 23505 ever surfaces.
- 400 `validation_failed`: `job_item_id` missing OR malformed -> `[job_item_id:required]` (oddity 2); `user_id` missing -> `[user_id:required]`; not a UUID -> `[user_id:malformed]`.
- 404 `not_found` `[job_item_id:not_found]`: no such job_item FOR THIS TENANT (unknown, or another tenant's: identical).
- 409 `conflict` (no details): the user already has a DIFFERENT campaign with a job in `queued`/`assigned`/`sending` (store.go:57-81), or, in theory, a lost unique-violation race (store.go:99-103).
- 503 `paused`: the dispatch rung is `refuse_new` or `stopped`. A config read failure FAILS OPEN (the job is accepted) (handlers.go:158-177).
- 500 `internal_error`: begin/lock/read/insert/commit failure; retrying the identical request is safe.

### 2.3 `GET /v1/report`, single job (handlers.go:204-322; store.go:143-189)

Query: `user_id` (required UUID) plus ONE of `job_id`, `job_item_id`, `campaign_id` (campaign wins if given with the others; `job_id` wins over `job_item_id`). Neither selector present ->
400 `[job_id:required]` (this check comes BEFORE the `user_id` check).

Response 200:
```json
{"job_id":"<uuid>","state":"sent","attempt_count":0,"reason":null,"error_class":null,"provider_message_id":"<id>","completed_at":"2026-11-17T10:00:00.123456Z"}
```
- `reason` and `error_class` are ALWAYS present (null when there is nothing to say); `provider_message_id` and `completed_at` are OMITTED until set (handlers.go:308-320). The single report does not
  carry `job_item_id` back.
- 404 `not_found` `[job_id:not_found]` or `[job_item_id:not_found]` (the selector the caller used) for: no such job, a job of another tenant, and a job_item that was never run: all three identical
  (handlers.go:258-273).
- `attempt_count` is NOT the number of SMTP attempts (oddity 11).
- For a `queued` job the handler makes two extra reads (dispatch paused? tenant has eligible capacity?) to word `reason`; a failure of either is a 500 (handlers.go:285-300).

### 2.4 `GET /v1/report?campaign_id=...&user_id=...[&cursor=...][&page_size=...]` (campaign_report.go)

```json
{"campaign_id":"<uuid>","dispatch_paused":false,
 "totals":{"total":3,"sent":1,"bounced":1,"failed":0,"in_flight":0,"pending":1,"waiting_for_capacity":0,"cancelled":0},
 "progress":{"terminal":2,"fraction_complete":0.6666666666666666,"is_complete":false},
 "items":{"results":[
   {"job_item_id":"<uuid>","state":"sent","provider_message_id":"<id>","completed_at":"2026-11-17T10:00:00Z","attempt_count":0,"reason":null,"error_class":null},
   {"job_item_id":"<uuid>","state":"bounced","provider_message_id":null,"completed_at":"2026-11-17T10:00:03Z","attempt_count":0,"reason":"bounce_permanent","error_class":"bounce_permanent"}],
  "next_cursor":null,"page_size":100}}
```
- Shape: campaign_report.go:117-131; item shape 47-59; totals shape 31-43. In items every key is ALWAYS present (null when unset), unlike the single report; `completed_at` is second-precision
  (`2006-01-02T15:04:05Z`, campaign_report.go:321-324) whereas the single report's is a full `time.Time` (oddity 8).
- A campaign that is unknown or another tenant's is NOT a 404: it is 200 with `total:0`, empty `results`, `is_complete:false` (campaign_report.go:61-68).
- Pagination: section 4.2.

### 2.5 `POST /v1/cancel` (cancel.go)

Request `{"campaign_id":"<uuid>","user_id":"<uuid>"}`, both required (400 with `required`/`malformed` details, in that order: campaign first). Response 200:
```json
{"campaign_id":"<uuid>","cancelled":2,"in_flight":1,"already_terminal":0}
```
Semantics in section 4.5. A foreign or unknown campaign answers 200 `{0,0,0}` (cancel.go:15-19). Not gated by the rung (cancel.go:36-38). 500 `internal_error` `cancel_failed` after 3 attempts at a lock cycle.

### 2.6 `GET /v1/describe` (handlers.go:324-407)

```json
{"module":"send_managed","api_version":"v1",
 "capabilities":{"single_send":true,"sequences":false,"reply_to_per_campaign":true,"open_tracking":false,"click_tracking":false,"content_variation":true},
 "pricing":{"currency":"credits","model":"flat_per_recipient","models":["flat_per_recipient","flat_subscription","free"],"unit_price_credits":1,
            "tiers":[{"id":"shared","unit_price_credits":1,"description":"Global shared SMTP pool."},{"id":"dedicated","unit_price_credits":1,"description":"User-dedicated pool with shared fallback."}],
            "cost_fields_inert":false},
 "vocabularies":{"job_state":{"source":"db_enum","values":["queued","assigned","sending","sent","failed","bounced","cancelled"],"fields":[...]},
                 "pool_kind":{...},"error_class":{"values":["bounce_permanent","transport_4xx","auth_failure","throttle","timeout","unknown"],...},
                 "detail_issue":{"source":"module_constant",...},"pricing_model":{...},"item_reason":{"source":"module_constant","values":[12 names, section 4.3],...}},
 "error_codes":[{"code":"validation_failed","http_status":400},{"code":"unauthenticated","http_status":401},{"code":"not_found","http_status":404},{"code":"method_not_allowed","http_status":405},{"code":"conflict","http_status":409},{"code":"internal_error","http_status":500},{"code":"paused","http_status":503}]}
```
`job_state`, `pool_kind`, `error_class` are read live from `pg_enum` per request (describe_vocabularies.go:216-268); a DB error fails the whole describe with 500 (no fallback). `api_version` is derived from the
route table, never a literal (handlers.go:345-357). The platform probe already checks module, api_version, capabilities, pricing.currency, `vocabularies.job_state.values`, `error_codes`, and that
`extension_routes`/`schemas`/`bulk_add` are absent (P:internal/probe/checks.go:153-249).

### 2.7 Health

`GET /healthz` 200 `{"status":"ok","at":"<rfc3339>"}`. `GET /readyz` 200 or 503 `{"status":"ready"|"not_ready","checks":{"db":bool,"singleton_lock":bool},"db":bool,"singleton_lock":bool,"at":...}`: DB ping
plus "this Control holds the singleton advisory lock" (httpapi/health.go:143-166). Readiness says NOTHING about a worker: a Control with no worker is ready and accepts jobs it cannot send
(this is the platform harness today). Worker presence is visible only on `GET /v1/admin/health` (`boxes.active`) or on the `send_managed.boxes` row.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 3. Tenancy

- The tenant is `user_id`, supplied by the caller on EVERY Domain A call: in the body for `run`/`cancel`, in the query for `report` (and optional on `quote`). It is never inferred from the token (one token serves
  all tenants). The module does not check it against any user list on these routes; it scopes every statement `WHERE user_id = $tenant` (store.go:90-94, 125, 136, 169-178; cancel.go:132-164; campaign_report.go:156-160, 289).
- Reads of the platform's tables: the module's role `send_managed_app` has SELECT on `public.users` and `public.job_items` only. `run` runs `SELECT campaign_id FROM public.job_items WHERE id=$1 AND user_id=$2`
  and `INSERT ... SELECT ... FROM public.job_items ji WHERE ji.id=$1 AND ji.user_id=$2` (store.go:61-63, 87-94). `public.users` is read by the admin routes (`SELECT EXISTS ... WHERE id=$1`, adminapi/pool_store.go:52-66)
  and enforced by foreign keys (a job's genesis event needs the user; `send_managed.boxes.user_id` references it). The all-zero system user must exist for shared pools and for worker rows: the platform's
  migration `platform_0001` already inserts it (P:db/migrations/platform_0001_public_tables.up.sql:56-58).
- Cross-tenant answers: single report and `run` -> the same 404 as a nonexistent id (no 403 anywhere); campaign report -> 200 empty; cancel -> 200 `{0,0,0}`. So a tenant mix-up in the adapter does not surface
  as an error, it surfaces as "nothing there": the adapter test needs a positive control (the right tenant sees the item) beside every cross-tenant probe.
- One active campaign per user: `run` takes a per-user transaction advisory lock (`hashtext('send_managed.campaign_start:'||user_id)`) and refuses a job of a DIFFERENT campaign while the user has any job of
  another campaign in `queued`/`assigned`/`sending` (store.go:45-81). A repeat `run` of an existing job_item never reaches that gate.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 4. Semantics

### 4.1 Idempotency
`jobs.job_item_id` is UNIQUE (constraint `jobs_job_item_id_unique`, M2:db/migrations/send_managed_0007_create_jobs.up.sql), so one job per platform job_item, forever. Run twice = same job, 202 then 200. A run lost in transit can be retried
verbatim (500, timeout, connection reset): there is no way to create two jobs, and the answer tells the current state. A cancelled or terminal job is never re-run under the same job_item_id (a repeat `run` answers the
existing job, 200).

### 4.2 Pagination (campaign report only)
- Order `ORDER BY j.created_at ASC, j.id ASC`, keyset `(created_at, id) > (cursor)`, `LIMIT page_size+1` (campaign_report.go:289-301). `created_at` is the job's creation instant = the order of the `run` calls.
- `page_size`: default 100, MAX 200; missing, zero, negative or non-numeric silently becomes 100; above 200 silently becomes 200; the effective value is echoed as `items.page_size` (campaign_report.go:455-470).
  The adapter must read the echoed value, not assume what it sent.
- `next_cursor`: opaque, `base64url-raw("<RFC3339Nano created_at>|<id>")` of the LAST RETURNED row (the M2-G5 fix: `keys[pageSize-1]`, campaign_report.go:340-347; the commit is in the pin). `null` on the last page.
  A walk to `null` returns each item of a quiescent campaign exactly once, in order.
- Not a snapshot: `totals` are one query, `items` another, `dispatch_paused` a third (campaign_report.go:85-103). Items already read can change state before a later page is read. A walk is exact only for a
  campaign that no longer receives `run` calls: a job whose `created_at` (transaction start) is earlier than a row already paged but which commits later is missed by that walk and found by the next. INFERENCE from
  the query shape, not observed. Settle idempotently per `job_item_id` and re-walk until every submitted item is terminal (the m1results pattern, P:internal/m1results).
- A syntactically valid cursor whose body is not `<timestamp>|<uuid>` is not caught: only non-base64 or a missing `|` is a 400 `[cursor:malformed]` (campaign_report.go:437-453); a bad timestamp/uuid reaches
  the `::timestamptz`/`::uuid` casts and is a 500 `internal_error` (oddity 13).

### 4.3 Item states, reasons, error classes (the vocabulary)
States (`send_managed.job_state`, in enum order): `queued, assigned, sending, sent, failed, bounced, cancelled`. Terminal: `sent, failed, bounced, cancelled`. The platform's charge table already maps them
(P:internal/charge/rules.psv M2-01..M2-08: sent and bounced charge; failed in every reason and cancelled are free; queued, assigned and sending are `notfinal`).

`reason` by state (M2:services/control/internal/platformapi/item_reason.go:95-150): `sent` null; `bounced` `bounce_permanent`; `failed` `pool_exhausted` | `attempts_exhausted` (a failed row with no recorded
reason is `pool_exhausted`; an unknown reason is null); `cancelled` `cancelled`; `assigned`/`sending` null; `queued`, most immediate cause first: `dispatch_paused` (effective rung not `serving`), `waiting_for_capacity` (tenant
has no eligible account now, OR the last handoff found no worker / failed to push), `no_eligible_account` (dispatcher's last attempt found none), else the last failed SMTP attempt's class, else null (not tried).
The closed list, in contract order (item_reason.go:65-78, published as `vocabularies.item_reason`): `pool_exhausted, attempts_exhausted, bounce_permanent, cancelled, transport_4xx, auth_failure, throttle, timeout,
unknown, no_eligible_account, waiting_for_capacity, dispatch_paused`. Additive-only; the adapter must treat an unknown reason as opaque, not an error.
`error_class` = the job's last classified failure (`bounce_permanent|transport_4xx|auth_failure|throttle|timeout|unknown`), null if none, ALWAYS null for `sent`; it also persists on a `queued` retry and on
`assigned`/`sending` after an earlier failure.

How a class is produced (so the harness can force it): the worker reports `accepted | bounced | error`; the Control, and only the Control, classifies (dispatch/classify.go:67-143): a worker `bounced`
(a 550/551/553 at RCPT, smtp.js:230-232) is `bounce_permanent`; an `error` is classified by reply code, enhanced code and keywords: a 535/534/530/538 or `auth_failed` -> `auth_failure`; a 4xx with a rate
keyword or 4.2.1 -> `throttle`; any other 4xx -> `transport_4xx`; connect/timeout slugs -> `timeout`; other 5xx -> `unknown`. The platform sink's deferral answers `451 4.7.1 try later`: no rate keyword
(`rate`, `too many`, `throttl`, `try again later`, `quota`, ...: classify.go:57-60), so it is `transport_4xx`: requeue with backoff, no pause and no quarantine (the account's score is still recomputed
and its consecutive-failure counter rises, outcome.go:408-412; quarantine acts only at `autoquarantine_mode=enforce`, seeded `shadow`). Its rejection answers `550 5.1.1 no such user`; a bounce touches the account not at all
(outcome.go:275-278).

### 4.4 Campaign totals
`total` = all jobs of (campaign, tenant). `sent`, `bounced`, `failed` (both reasons), `cancelled` = jobs in that state. `in_flight` = `assigned` + `sending`. `waiting_for_capacity` = `queued` jobs while the TENANT
has no eligible account in any enabled pool it can reach (own dedicated pools plus enabled shared pools); `pending` = the other `queued` jobs (`pending_total - waiting`). The seven numbers partition `total`
(campaign_report.go:155-190). Eligible = not quarantined, not disabled, daily counter below the limit or reset, paused only for an expired throttle or `daily_limit` (campaign_report.go:199-217).
`progress.terminal = sent+bounced+failed+cancelled`; `is_complete = total>0 && terminal==total`; `fraction_complete = terminal/total` (0 when total is 0). A queued job waiting for its retry backoff is `pending`, not waiting.
No worker is not "waiting for capacity" in the totals (only in the item `reason`, via the `no_worker_available` requeue).

### 4.5 Cancel
Two committed transactions (cancel.go:127-208): (1) flag every non-terminal job of the campaign with `cancel_requested_at` (no state change); (2) move every `queued` job to terminal `cancelled`, with an event
row and `completed_at`. Jobs already `assigned`/`sending` finish and report truthfully exactly once; if their attempt FAILS they end `cancelled` instead of returning to the queue (a database trigger, send_managed_0028).
The response is the position AFTER the call, not a delta: `cancelled` = ALL jobs of the campaign now in `cancelled` (including earlier ones), `in_flight` = `assigned+sending`, `already_terminal` = `sent+failed+bounced`.
A second call returns the same numbers. A `run` for a NEW job_item of that campaign afterwards is accepted and is not flagged: the platform must stop submitting.
Numbers while a send is in flight are not deterministic: items the dispatcher has claimed but could not push (no idle worker) are `assigned` at that instant and end `cancelled` when requeued
(M2's own cancel suite asserts the partition and the end state, never the split, M2:tests/harness/phase11_cancel.sh:6-37).

### 4.6 Poll cadence, limits, rate limits
- Dispatch is a polling loop: `dispatch_tick_interval_ms` (seed 1000) re-read each tick, plus an immediate kick on every `run` and on every worker registration (dispatch/runner.go:49-67; main.go:191-193).
  Reclaim ticks every 15 s (`reclaim_tick_interval_ms`). A worker's outcome is persisted the moment it reports.
- No rate limit on Domain A (nothing writes 429); no cache; each request costs a bcrypt verify and a DB round trip with a 10 s bound (platformapi/server.go:98-101). The server sets only `ReadHeaderTimeout: 5s`
  (httpapi/health.go:67), no body limit. The campaign report runs the totals query (with the eligibility subquery) and reads three config rows on every call. Reasonable adapter cadence: 2-5 s per campaign,
  `page_size=200`, stop once `is_complete` and every submitted item is settled.
- Throughput of the sender itself: one job in flight per worker (stream.go:265-268, 295-304), per-user lane ceiling `concurrency_per_user_default` (3), and a per-lane random cool-down of
  `send_delay_min_sec`..`send_delay_max_sec` (seed 3..7 s) after each send (dispatch/pacing.go; loop.go:129,189).

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 5. What the thin adapter implements (one method per route, and how each answer is read)

Style to copy: P:internal/m3admin and P:internal/m1results (a `config.Secret` token revealed at one place only, no redirects followed, no proxy, bodies read through a hard cap, errors carrying a status and a fixed
reason, never a body or a token, a `Retryable` test). The adapter takes the tenant as an explicit argument on every call; it never defaults it.

| Method | Route | Success | Retry (nothing was confirmed) | Do not retry | Special reading |
|---|---|---|---|---|---|
| Quote(count) | `POST /v1/quote` | 200 `cost`, `unit_price_credits` | network, 5xx | 400 (bug), 401 (configuration) | bound `count` first (oddity 9) |
| Run(itemID, tenant) | `POST /v1/run` | 202 created, 200 existing (read its `state`) | network, 500, **409** and **503** (later, unchanged: backpressure) | 400 (bug), 401 (configuration) | 404 = the job_item is not committed or not that tenant's: a platform bug, surfaced, not retried in a loop |
| Report(itemID, tenant) | `GET /v1/report?job_item_id=` | 200 `state`, `reason`, `error_class` | network, 5xx | 400, 401 | 404 = "not run yet" OR foreign OR typo: an unsettled item, never a failure |
| ReportCampaign(campaign, tenant) | `GET /v1/report?campaign_id=` + `next_cursor` | 200, walk to `next_cursor` null | network, 5xx, restart the walk | 400, 401 | `total` 0 = nothing known; completeness = the platform's own submitted count, not `is_complete` alone |
| Cancel(campaign, tenant) | `POST /v1/cancel` | 200 position | network, 5xx (idempotent) | 400, 401 | the split is advisory; the platform keeps submitting nothing further |
| Describe | `GET /v1/describe` | 200 | network, 5xx | 401 | unknown `error_codes`/`reason` values are tolerated (additive-only) |

The report the adapter hands up is the platform's own type: `{State, Reason *string, ErrorClass *string, ProviderMessageID *string, CompletedAt *time.Time, Terminal bool}`, with `Terminal` = state in
`sent|failed|bounced|cancelled`. A state outside the seven is an `unmapped` outcome (rules.psv M2-99): never charged, reported to the caller (CLAUDE.md rule 20).

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 6. Oddities the adapter must special-case

1. **Two success codes on `run`.** 202 = created, 200 = already existed with its CURRENT state. Never assume the 200 body says `queued`; map it like a report state.
2. **A malformed `job_item_id` is reported as `required`** (handlers.go:134-138: the check is `!uuidRe.Match`, the detail is `issueRequired`). A malformed `user_id` is `malformed`. The adapter validates UUIDs
   itself before sending.
3. **Cross-tenant and nonexistent are the same**: 404 for single report and `run`, 200-empty for campaign report, 200-`{0,0,0}` for cancel. A typo in a campaign id cannot be told from "nothing yet".
   `is_complete` is false when `total` is 0; compare `totals.total` with the number of items the platform submitted before believing completion.
4. **`409 conflict` on `run` is backpressure, not failure.** It means "this user has another campaign in flight" (store.go:79-81). It also stays true while a CANCELLED campaign's in-flight items are still
   `assigned`/`sending`, and for any queued item waiting for capacity forever. The body cannot distinguish it from the race 409. Retry later; the platform's job engine must serialise a user's campaigns on M2.
5. **Two 500s and two 409s look alike** (fixed message, no details): never parse the message; branch on status and code only.
6. **`503 paused` is the whole module's condition**, retry unchanged later; `report`, `quote` and `cancel` still work at every rung. A report of a queued item then says `dispatch_paused`.
7. **Single report omits `provider_message_id`/`completed_at` until set; campaign items carry them as null.** `reason`/`error_class` are present as keys in both. Decode with pointer fields.
8. **`completed_at` has two formats**: fractional seconds in the single report, whole seconds in the campaign items. Do not compare across the two.
9. **`quote` has no upper bound and ignores capacity**: a huge `recipient_count` overflows the 64-bit product silently (unit is up to 1000). The adapter bounds the count itself; the money core must not trust `cost` for a
   count it did not bound. `tier` is always `shared`; `user_id` is ignored.
10. **`report?job_id=` and `?job_item_id=` both exist**; use `job_item_id` (the platform's own key); it returns the job's id too. With several selectors the order is campaign > job_id > job_item_id.
11. **`attempt_count` is not an attempt count.** It is `SMALLINT`, saturates at `max_attempts` (3), and is also bumped by dispatcher-side requeues that never reached SMTP (`no_eligible_account`,
    `no_worker_available`, `push_failed`, `header_injection`) (dispatch.go:549-586; outcome.go:383). A first-try send reports `attempt_count: 0`. Never bill or reason on it; the real counter
    (`failed_attempt_count`) is not exposed.
12. **`failed` has two reasons and both are free for the platform** (rules.psv M2-03/M2-04): `pool_exhausted` (every account of a non-empty chain is terminal-blocked) and `attempts_exhausted` (the Nth real failed
    SMTP attempt, or the age cap). An unknown future reason is null: treat `state=failed` as the fact, `reason` as colour.
13. **A syntactically valid but wrong `cursor` is a 500, not a 400** (the doc says 400: it is wrong for a base64 value that is not `<timestamp>|<uuid>`). The adapter only ever passes back `next_cursor` verbatim.
14. **`page_size` is clamped silently** (0, negative, text -> 100; over 200 -> 200). Read `items.page_size`.
15. **Doc and code differ on the cursor**: M2's api-contract.md says a malformed cursor is 400; the code answers 400 only for a non-base64 value or one without `|` (13). Code wins; a first-try send reports
    `attempt_count: 0` (11), whatever an example in the doc shows.
16. **`cancel` answers a position, not a delta**, and its split between `cancelled` and `in_flight` is timing dependent while anything is claimed. Assert the partition (`cancelled + in_flight + already_terminal
    = total`) and the end state, not the split. `cancelled` in the response includes items cancelled by an earlier call.
17. **Cancel does not freeze a campaign**: a later `run` of the same campaign is accepted and sent.
18. **No worker is invisible to `readyz` and to quote.** Jobs are accepted and sit `queued` with reason `waiting_for_capacity` (no worker) or, when the tenant has no eligible account, `waiting_for_capacity`
    by the live predicate. A tenant with no pool/account waits for ever: it never becomes `failed` (an empty chain is a WAIT, dispatch/exhaustion.go:48-56).
19. **Totals and items are separate queries**, not one snapshot (4.2).
20. **A "queued" item can be mid-retry.** After a 4xx the job is `queued` with `error_class: transport_4xx`, `reason: transport_4xx`, `next_attempt_at` in the future; it is `pending` in totals.
21. **`unit_price_credits` is a live dial** (0..1000). `describe.pricing.cost_fields_inert` is true at 0; the adapter should read the unit from `quote`/`describe` per use, never cache it.
22. **Every request costs a bcrypt verify** (1); the HTTP server has no write/read timeouts and no body cap (oddity-level, record only).
23. **`GET /readyz` accepts any verb** (registered without a method) and answers 200/503 with `status` `ready|not_ready`.
24. **Domain A responses use `application/json; charset=utf-8`; the probes use `application/json`.** Cosmetic.
25. **Admin routes give different 404/422 vocabulary** (`unprocessable` 422 exists only there); the adapter's admin helper (harness only) must not share the Domain A error mapping.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 7. The admin routes needed to prepare a campaign (Domain B, harness and operator use only)

Operator key; body cap 1 MiB; strict decode, unknown field = 400 (adminapi/helpers.go:14-44). The platform's contract test needs only these:

| Call | Body (fields and limits) | Response |
|---|---|---|
| `POST /v1/admin/pools` | `kind` `shared`\|`dedicated` (required); `name` (required); `owner_user_id` (required for dedicated, must be a `public.users` id; forbidden for shared); optional `routing_strategy` `random`\|`round_robin`\|`one_by_one` (default `round_robin`), `fallback_pool_id`, `enabled` (default true) (adminapi/pool_handlers.go:14-142) | 201 pool `{"id","user_id","kind","owner_user_id","name","routing_strategy","fallback_pool_id","enabled","account_count","created_at","updated_at"}`. Shared needs the system user, else 409 `system_user_not_provisioned`; a dedicated pool whose owner is not in `public.users` is 400 `validation_failed` `[owner_user_id:not_found]` |
| `POST /v1/admin/accounts` | `pool_id` (uuid), `smtp_host` (IP literal or hostname, no scheme, port or path, 1..255), `smtp_port` (default 587; 1..65535), `smtp_username` (1..320), `smtp_secret` (non-empty, write-only), `from_address` (regex `^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`, ≤320), optional `from_name` (≤255), `daily_limit` (1..100000; default config `daily_limit_default` = 500) (adminapi/account_create.go:9-170; account_validate.go:22-92) | 201 `{"id","user_id","pool_id","smtp_host","smtp_port","smtp_username","from_address","from_name","has_credential":true,"status":{"paused","pause_reason","quarantined","disabled"},"daily_limit","daily_sent_count",...}`; duplicate `(pool, host, username)` 409; the secret is AES-GCM encrypted with the dev KMS key, AAD = account id + key id |
| `PUT /v1/admin/config/{key}` | `{"value": <inner>}` (the module wraps it in `{"v":..}`), type-checked per key; unknown key 404; an ordering-breaking pair 400 `config_ordering_violation` (adminapi/config_handlers.go:51-146) | 200 the config row |
| `GET /v1/admin/health` | none | 200 `{"generated_at","window_sec","dispatch":{"dispatch_paused","dispatch_rung",...},"queue":{"queued","assigned","sending","sent","failed","bounced","cancelled","waiting_for_capacity"},"boxes":{"active","stale","disabled"},"accounts":{...},"pools":[...],"errors_last_window":{...},"shadow":{...},"active_conditions":[...]}` (adminapi/health_handler.go:15-77): `boxes.active >= 1` is how a test sees the worker |
| `GET /v1/admin/accounts`, `/pools`, `.../{id}` | `?limit` (default 50, max 200), `?cursor`, filters | lists `{"items":[...],"next_cursor":...}` |

Routing: a tenant's job goes to the tenant's own ENABLED dedicated pools first, then at most one shared pool (the first dedicated pool's `fallback_pool_id` if it is an enabled shared pool, else the config
`default_shared_pool_id`) (dispatch/pool.go:35-126). A tenant with ONLY a dedicated pool and one account in it is fully served with no global config: the proposal uses that (per-tenant pool, no shared state).
Workers are not registered through the API: a worker is a row in `send_managed.boxes (id uuid, user_id, token_hash, status)` that the operator inserts (M2:db/migrations/send_managed_0006_create_boxes.up.sql:7-19;
M2:scripts/dev.sh:105-125), and the worker proves itself with id + token (workerlink/stream.go:328-349).

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 8. How M2's OWN harness sets up the worker, the sink and the outcomes

M2's harness is `scripts/dev.sh` + `docker-compose.yml` + `tests/harness/run.sh` and the sourced `phase*.sh` suites (a real stack, nothing mocked).

- **Secrets** (`dev.sh certs`, M2:scripts/dev.sh:36-102): `tools/gen-certs.sh` makes a dev CA and leaves for the Control (`DNS:control,DNS:localhost,IP:127.0.0.1`, gen-certs.sh:80), the worker
  (`DNS:worker,DNS:localhost`, :82, CN carries an id the Control never checks), the sink (`DNS:smtp-sink,DNS:localhost`, :84) and a host test identity (:89); `tools/gen-secrets.sh` writes the 32-byte dev KMS master key;
  `provision-worker new/admin-key` mint the worker token + bcrypt hash, the operator key + hash and the platform token + hash into a git-ignored `.env`; `tools/own-secrets.sh` keeps every key mode 600 and chowns
  `control.key` and `dev-master.key` to uid 10001 and `worker.key` to uid 10002 (the uids are read from the Dockerfiles, own-secrets.sh:33, 58), then PROVES each can read its own and no other uid can.
  The worker mounts only `worker.crt`, `worker.key`, `ca.crt`; the Control only the KMS key, its own pair and the CA (M2:docker-compose.yml, worker block from :98).
- **The worker row** (`dev.sh seed`, dev.sh:105-125; phase3_send.sh:36-37): `INSERT INTO send_managed.boxes (id,user_id,token_hash,status) VALUES (<uuid>, '00000000-...-0000', <bcrypt of the worker token>, 'active')`,
  and the system user into `public.users`. BEFORE the worker starts: a rejected worker exits after ~30 one-second attempts (worker index.js:71-96). Readiness is a heartbeat newer than the test's start
  (phase3_send.sh:56-63), because registration sets `last_heartbeat_at = NOW()` (stream.go:353-364).
- **Pool and account** (phase3_send.sh:66-75): a SHARED pool and one account through the admin API, `smtp_host: "smtp-sink"`, `smtp_port: 1025`, the account's secret a canary string; then
  `PUT /v1/admin/config/default_shared_pool_id`. The host name `smtp-sink` is special-cased by the worker next to `localhost` and private ranges (smtp.js:155-164), which is why M2's TLS to Mailpit works.
- **Sink**: Mailpit, STARTTLS required, `MP_SMTP_AUTH_ACCEPT_ANY=true` (docker-compose.yml:166-187). M2's compose states its own limit: Mailpit cannot emit 535/550/4xx, "this stack proves the success path only".
- **DNS**: none. M2 has no CoreDNS and no From-gate; the worker resolves only compose service names.
- **How outcomes are forced there**: `sent` live through Mailpit. `bounced` and every 4xx/auth/throttle class are driven by Go tests that hand the outcome handler a synthetic report
  (dispatch/classify_test.go:23, dispatch_test.go:285, cancel_test.go:219), not by a real worker against a hostile server; the Node worker's test sink answers `250` to every RCPT
  (M2:services/worker/test/sink.mjs:103-104), so **no M2 test drives the worker's RCPT-550 -> `bounced` path or RCPT-451 -> `rcpt_rejected` path against a real server** (UNPROVEN end to end). `cancelled`
  mid-campaign: `phase11_cancel.sh` freezes the worker with `docker pause`, runs a campaign of 10, waits for one item to be `sending`, cancels, unpauses, and asserts the partition, the end state and the sink's
  per-recipient count (phase11_cancel.sh:6-37, 75-90). `docker pause` needs the host's docker; the platform's tests run inside the private network and cannot do it (hence the sink's delay, below).
- Retry dials in M2's own tests: none shortened live; the cap and backoff are covered by Go tests against Postgres.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 9. Proposed additions to the PLATFORM harness (no module source touched)

Principle: reuse what exists. The sink `smtp-ctl` (P:tests/harness/docker-compose.yml:293-315, source tests/harness/sink) is used UNCHANGED as M2's SMTP target. M2's Control (`m2`, compose :146-175) stays as is except for one
env line. One new service, one new stage, a few env lines, one new test file.

### 9.1 The sink's API (P:tests/harness/sink/api.go:11-17, store.go:34-52) and why it fits M2
`POST /api/mode` REPLACES the whole mode: `{"reject_rcpt":[addr],"reject_code":550,"defer_rcpt":[addr],"data_delay_ms":N,"delay_recipients":[addr],"fail_auth":false}` (unknown field = 400); `GET /api/messages`
(`[{seq,at,to[],from,envelope_from,subject,message_id,size,held_ms}]`), `GET /api/rcpt` (`{addr:{accepted,rejected,deferred}}`), `GET /api/inflight` (`{inflight,peak,held_total,held[]}`), `POST /api/reset`, `GET /readyz`.
Read against M2's worker (smtp.js:186-264) the dialogue matches: port 1025 means `securityForPort` = STARTTLS (dispatch.go:623-628), the sink offers STARTTLS and, once secured, `AUTH PLAIN LOGIN`; the worker's
`AUTH LOGIN` runs the 334/334/235 sequence the sink implements (sink smtp.go:356-382); `RCPT` 550 -> worker `bounced`; `RCPT` 451 -> `rcpt_rejected` with the reply text -> `transport_4xx`; a held final reply keeps the job
`sending` (the worker's per-operation timeout is `smtp_operation_timeout_sec` = 30 s, so a hold must stay under it); `queued as <id>` is parsed as the provider message id (stream.js:457-466), unique across resets (store.go idSeq).

### 9.2 Compose (`tests/harness/docker-compose.yml`)
```yaml
  m2:   # one added line in the existing environment block
      ADMIN_API_KEY_HASHES: ${H_M2_ADMIN_KEY_HASH:?}      # the operator surface (pools, accounts, config); a credential DISTINCT from the service token

  m2-worker:
    build: { context: "${H_MODULES_ROOT:?}/coldmail-send-managed", dockerfile: services/worker/Dockerfile }
    environment:
      CONTROL_HOST: m2
      CONTROL_STREAM_PORT: "8444"
      CONTROL_SERVER_NAME: control          # the Control certificate's SAN (DNS:control); the dial uses host m2 and this name separately
      WORKER_ID: ${H_M2_WORKER_ID:?}        # a uuid: send_managed.boxes.id
      WORKER_TOKEN: ${H_M2_WORKER_TOKEN:?}
      WORKER_HEARTBEAT_INTERVAL_SEC: "10"
      WORKER_SMTP_TIMEOUT_MS: "30000"
      MTLS_CLIENT_CERT: /secrets/mtls/worker.crt
      MTLS_CLIENT_KEY: /secrets/mtls/worker.key
      MTLS_CA_CERT: /secrets/mtls/ca.crt
    volumes:      # M2's own secrets, minted by its own tools in stage 2: worker.crt, worker.key (uid 10002), ca.crt
      - ${H_MODULES_ROOT:?}/coldmail-send-managed/secrets/dev/certs/worker.crt:/secrets/mtls/worker.crt:ro
      - ${H_MODULES_ROOT:?}/coldmail-send-managed/secrets/dev/certs/worker.key:/secrets/mtls/worker.key:ro
      - ${H_MODULES_ROOT:?}/coldmail-send-managed/secrets/dev/certs/ca.crt:/secrets/mtls/ca.crt:ro
    depends_on: { m2: { condition: service_healthy } }
    networks: [private]
    restart: "no"

  harness-tests:  # added to environment
      HARNESS_M2_URL: http://m2:8080
      HARNESS_M2_TOKEN: ${H_M2_TOKEN:?}
      HARNESS_M2_ADMIN_KEY: ${H_M2_ADMIN_KEY:?}
      HARNESS_M2_WORKER_ID: ${H_M2_WORKER_ID:?}
      HARNESS_SINK_PASSWORD   # already there: reuse it as the account's SMTP secret (the sink accepts any credential; nothing new to mask)
```
No `image:` is added (the digest-pin gate reads `image:`); the Dockerfile's own base is digest-pinned. No port is published. Update the header comment of the compose file, which still says the workers and the sink
are "not here until Phase 2 chunk B8", and add `m2-worker` to the exposure list (run.sh:259).

### 9.3 `tests/harness/run.sh`
- Stage 2 (after run.sh:75): mint with the existing `mint` (provision-worker): `M2_ADMIN_KEY`/hash, `M2_WORKER_TOKEN`/hash, `M2_WORKER_ID=$(cat /proc/sys/kernel/random/uuid)`; assert all non-empty and that the admin key differs from
  the service token and from the worker token; `mask` the two tokens and the key (run.sh:95) and the two hashes (run.sh:99) BEFORE they reach the env file; write `H_M2_ADMIN_KEY`, `H_M2_ADMIN_KEY_HASH=$(esc ...)`,
  `H_M2_WORKER_ID`, `H_M2_WORKER_TOKEN` (run.sh:101-128). Keys are already generated and chowned for M2 (run.sh:59-63): nothing new there.
- Stage 5 unchanged (`m2` now mounts the admin surface).
- New stage 5c, after 5b (the SQL helper `psql_su` is defined at run.sh:200): `INSERT INTO send_managed.boxes (id, user_id, token_hash, status) VALUES ('$M2_WORKER_ID','00000000-0000-0000-0000-000000000000','$M2_WORKER_HASH','active')`
  as `supabase_admin`, THEN `DC up -d m2-worker`, then wait (60 x 2 s) for `status='active' AND last_heartbeat_at > <db clock taken before the insert>`; die with the service logs otherwise (the 5b pattern, run.sh:202-220).
  Also wait for `smtp-ctl` (already done in 5b).
- Stage 6: after the binary runs, require `--- PASS:` lines for each new test by name (the M1 line at run.sh:246 is the model); raise `tests/floors-harness.txt` by exactly the number of new top-level tests.
- The NOT-CHECKED block (run.sh:425-428) says "Module 2 has no worker": rewrite it, or the closing report contradicts the run.

### 9.4 What the Go test creates through M2's admin API (per run, per scenario tenant)
1. A tenant: `auth.users` + `public.users` rows (the 9b/9c pattern, run.sh:285-286), and `public.job_items` rows (`campaign_id`, `user_id`, unique `recipient_email` at an `example.test` address, subject, body), as `supabase_admin`.
2. `POST /v1/admin/pools {"kind":"dedicated","owner_user_id":<tenant>,"name":...}`; `POST /v1/admin/accounts {pool_id, smtp_host:<sink private IP>, smtp_port:1025, smtp_username, smtp_secret:<H_SINK_PASSWORD>, from_address:"sender@example.test", daily_limit:1000}`.
   The IP is resolved in the test from `smtp-ctl` and asserted to lie in 10/8, 172.16/12 or 192.168/16 (the M3 test does exactly this: m3_hostile_test.go:141-155); with the NAME `smtp-ctl` the worker validates the
   self-signed certificate and every send fails `starttls_failed` -> class `timeout` (smtp.js:142-149; classify.go:126-130).
3. Config dials (global, one `PUT` each, `GET` back to assert): `retry_backoff_schedule {"ladder_seconds":[1,1,1],"max_seconds":1}`, `job_max_failed_attempts` 2, `send_delay_min_sec` 0 and `send_delay_max_sec` 0.
   (No semantic validator exists for the two delays; the pacing draw is degenerate-safe, pacing.go:64-70.) Optional: `dispatch_tick_interval_ms` 250.
4. The sink mode, set ONCE before any scenario (distinct recipients make the modes disjoint): `reject_rcpt:[bounce-addr]`, `defer_rcpt:[defer-addr]`, `data_delay_ms:10000`, `delay_recipients:[the cancel campaign's addresses]`.
   Reset to default (`POST /api/mode {}`; not `/api/reset` unless the M3 tests are known to be finished) in a `t.Cleanup`.
5. The worker is the one registered in 5c; the test only asserts `GET /v1/admin/health` `boxes.active >= 1` as its precondition (a failure, never a skip).

### 9.5 Scenarios, assertions and the controls that make them fail when the property breaks
All through the ADAPTER (quote / run / report single and campaign / cancel), with the sink's own API as the independent witness.
- `sent`: run -> report `state:"sent"`, `provider_message_id` set, `reason:null`, `error_class:null`; sink `/api/messages` holds exactly one message to that recipient, whose `message_id` CONTAINS the job id (the Control builds `<job_id@sending-domain>`, headers.go:53-55; assert containment, not the exact string,
  because the worker's assembly step is unproven here); re-run answers 200/`sent` and the sink count stays 1. Control: a recipient that was never run has zero.
- `bounced`: recipient in `reject_rcpt` -> `state:"bounced"`, `reason:"bounce_permanent"`, `error_class:"bounce_permanent"`; sink `/api/rcpt[addr].rejected == 1` and NO message; the account is not penalised
  (admin account `status` unchanged, `daily_sent_count` still counts the consumed slot).
- `failed/attempts_exhausted`: recipient in `defer_rcpt`, cap 2 -> `state:"failed"`, `reason:"attempts_exhausted"`, `error_class:"transport_4xx"`; sink `/api/rcpt[addr].deferred == 2` EXACTLY and no message. Paired control: with
  the cap raised the item stays `queued` after one attempt with `reason:"transport_4xx"` (`deferred == 1`).
- `cancelled`: a campaign of 3 for a fresh tenant, all recipients delayed; `run` the three; wait until `/api/inflight.inflight == 1` (the first item is `sending` on the sink); `cancel`; assert the partition sums to 3
  and `in_flight >= 1` (the split is not asserted), `queued == 0` in a report; after the hold: exactly one `sent` (the held one) and two `cancelled` with `reason:"cancelled"`, the sink holds exactly one message of the campaign,
  `is_complete:true`; a second `cancel` returns the same numbers; a cancel by ANOTHER tenant answers `{0,0,0}` and changes nothing. Control: the same campaign without a cancel ends 3 `sent`.
- Contract and tenancy (no SMTP needed): `quote` (200 shape; negative -> 400 `out_of_range`; `user_id` garbage -> 400), the three 401 cases (no token, wrong token, the admin key), a foreign-tenant report/`run` -> 404, an
  unknown path 404 and a wrong verb 405 with `Allow`, `describe` shape and `error_codes`, the 409 `user_campaign_active` (second campaign for a tenant with a held first), the 503 `paused` after `PUT dispatch_mode refuse_new`
  (restore to `serving` in cleanup) with `quote` and `cancel` still 200, campaign pagination over more than one page (`page_size` 2 over 5 items: every item exactly once, in creation order, `next_cursor` null on the last page; paired control: the same campaign read with
  `page_size` 200 returns the same set, and a deliberately skipped page makes the checker report the missing item). A forged cursor `base64url("x|y")` is expected to be a 500 (oddity 13): record it, do not assert it as contract.
Per-tenant data keeps the scenarios independent, but ONE worker serves them all, one job at a time: the cancel scenario's 10 s hold stalls everything queued behind it. Run the cancel scenario LAST, or register a
second worker (a copy of the block above with its own id, token and box row) so one held send does not starve the others.

### 9.6 Time each outcome takes (estimates from the code, UNPROVEN; the dials above)
- `sent`, `bounced`: up to one tick (a `run` kicks the loop at once) + the SMTP dialogue (about 10 replies at the sink's 8 ms reply delay) = about 1-3 s.
- `failed/attempts_exhausted` with cap 2 and ladder `[1,1,1]`: attempt 1, requeue `next_attempt_at = now + 1 s`, attempt 2 on the next tick after that: about 3-5 s. With M2's defaults it would be 30 s + 120 s + 600 s ... = about 32 minutes
  for the default cap of 6 (ladder rung per attempt, capped at `max_seconds` 600). `job_failed_max_age_sec` cannot help: its floor is 60 s (config_validate.go:142-146), and it only fires on a further failed attempt.
- `cancelled`: the hold (10 s; under the 30 s operation timeout and the 120 s lease) + 1-2 s; the cancel itself returns in milliseconds. The test's bound on every wait should be 60 s, with the elapsed time logged.
- Whole M2 test: well under 2 minutes of wall time on top of the stage's 5 s worker wait.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 10. Risks: what could make the worker, or the test, unusable in compose

1. **Box row not seeded before the worker starts**: the worker is rejected and exits (`restart: "no"`). Seed first, then `up` (the 5b order). A heartbeat newer than the pre-insert DB clock is the readiness proof.
2. **Wrong dial target**: the worker defaults to host `control`, which does not resolve in this compose; `CONTROL_HOST: m2` is required, with `CONTROL_SERVER_NAME: control` because the certificate's names are
   `control`, `localhost` and `127.0.0.1` (gen-certs.sh:80), not `m2`. The same split already works for M3.
3. **Private-range guard on `smtp_host`**: certificate verification is skipped ONLY for `localhost`, `smtp-sink`, loopback, 10/8, 172.16/12, 192.168/16, 169.254/16 (smtp.js:155-164). The account must hold the sink's IP
   literal, resolved at run time, asserted in range (a runner whose Docker network is outside those ranges fails loudly instead of silently timing out). This is the opposite of M2's production rule ("no account's
   `smtp_host` is private-range": M2:Docs/go-live-checklist.md:256-258, security-posture.md §3.13): fine for a throwaway harness, and the platform's own go-live check must stay a separate thing. Do NOT point an account at
   Mailpit's name `smtp-sink` for these scenarios (it cannot reject or defer).
4. **Key file ownership**: `worker.key` must be readable by uid 10002, `control.key` and `dev-master.key` by 10001, all mode 600 (own-secrets.sh:58). Stage 2 already runs `own-secrets.sh` for M2 (run.sh:59-63);
   if it falls back to a root container for the chown the mounts still work. A regenerated certificate set is a different CA: never reuse a stale `secrets/dev` from a previous run (the runner is fresh each time).
5. **`$` in bcrypt hashes**: `ADMIN_API_KEY_HASHES` in the env file must pass through `esc` (`$` -> `$$`); in the SQL literal the hash is used raw. A wrong escape reads as "401 for a valid key" or "register rejected".
6. **One job in flight per worker** (stream.go:265-268): with one worker, every scenario shares one lane; a held send stalls the rest, and the dispatcher churns claimed-but-unpushable jobs
   `assigned -> queued` (reason `waiting_for_capacity`) once per tick (loop.go:172-182, dispatch.go:549-586), which also saturates `attempt_count` at 3 (oddity 11). Assert end states, not transient ones.
7. **Config is global** to the one M2 instance: the dial changes (and `dispatch_mode refuse_new`) affect every tenant of every test in the run. Restore in cleanup, keep the mode scenario last, and put the dials in ONE helper
   so a later test cannot silently inherit a changed value.
8. **The sink's mode is global and `POST /api/mode` replaces it**: the M3 tests also use `smtp-ctl`; they run before or after in one sequential binary and both reset at the end; do not run an M2 test in parallel with them. Count by
   this run's own recipient addresses, never by the sink's total.
9. **Daily headroom is consumed before the send** (eligibility.go header): every attempt, failed or not, costs a slot; `daily_limit` 1000 is far above the few sends here.
10. **Singleton lock**: after the stage-9 outage (Postgres stopped) the M2 Control loses and re-acquires its lock on `brain_singleton_reacquire_interval_sec`; nothing of M2 runs after stage 9, so there is no interplay as long as
    the M2 test stays in stage 6. Do not move M2 sends after the outage stages.
11. **Image build**: one more `docker compose build --no-cache` service (Node, `apk add curl` needs the build network, which is not the internal one): fine, as for the other images.
12. **Secrets in logs**: the worker and Control never log the SMTP secret (M2 CLAUDE.md rule 6; the module's own gate `no_secret_leak`); the new tokens and hashes are masked by `mask` before first use; the account secret reuses
    `H_SINK_PASSWORD`, already masked.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 11. What I am unsure of, and how one runner run settles it

| Unsure | Why | What settles it in one run |
|---|---|---|
| The worker registers and heartbeats in this compose | names, uid, certificate chain are read from files, not run | stage 5c's heartbeat wait; on failure the m2 and m2-worker logs (`worker registered id=` / `register REJECTED` / `connect attempt N not ready`) |
| STARTTLS + AUTH LOGIN against `smtp-ctl` completes with M2's worker | I traced the dialogue by reading both ends (sink smtp.go:270-392 vs smtp.js:186-318); Node warns on an IP `servername` (non-fatal for M3, same pattern) | the `sent` scenario; sink log lines `smtp: accepted message` |
| RCPT 550 -> `bounced` and RCPT 451 -> `transport_4xx` really reach `failed/attempts_exhausted` end to end | no M2 test has ever driven these through a real worker (section 8) | the `bounced` and `failed` scenarios; the sink's per-recipient counters are the independent witness |
| Timing (1-3 s, 3-5 s, hold + 2 s) | estimates from the loop and the dials | log the elapsed time of every wait in the test; the first run's numbers set the bounds |
| The cancel split | timing dependent by design | only the partition and the end state are asserted, never the split |
| The runner's Docker network lies in the private ranges | Docker picks the pool; the M3 test already asserts it | the IP assertion in the test (first line of the M2 test's preconditions) |
| `send_delay_*` = 0 accepted | I found no validator for those keys beyond "integer" (config_validate.go:105-274 has no case) | the `PUT` answering 200 and the `GET` echoing 0 |
| Account effect of repeated `transport_4xx` | the score recompute runs; quarantine acts only at `autoquarantine_mode=enforce` (seed `shadow`) | the account's `status` read through the admin API after the failed scenario (expect unchanged flags) |
| `docker compose` interpolation of `H_M2_*` | same mechanism as the M3 variables | stage 3 `build` / stage 5 `up` |

Not read (not needed for this question): account PATCH and lifecycle routes, `reclaim.go`, `retention.go`, the variation pipeline (`variation_enabled` defaults on and is applied to every message body: the sink stores the
body, nothing here depends on it).

Staging advice: the first runner run should carry the whole design but only ONE hard assertion beyond the preconditions per scenario (the end state), plus the log lines; tighten after it is green once. A red first run costs
a CI cycle, not a redesign: every unsure row above names the log that settles it.

---------------------------------------------------------------------------------------------------------------------------------------------------------------------

## 12. The five things the lead must know before writing the Phase 3 spec

1. **Quote is pure arithmetic and `run` accepts what it cannot send.** Quote validates no eligibility, capacity or account (`recipient_count * unit`, `tier` always `shared`); `run` queues a job for a tenant with no pool, no account or no worker, and
   nothing in `readyz` or `quote` says so. Capacity is only visible afterwards (`reason: waiting_for_capacity`, `totals.waiting_for_capacity`). The spec must say where the platform checks capacity and what it does with a campaign that never starts.
2. **Idempotency and tenancy are strong, but they answer "nothing" rather than "no".** `run` is idempotent on `job_item_id` (202 created, 200 existing with the CURRENT state), and every cross-tenant or unknown probe is 404 / empty 200 / `{0,0,0}`.
   Plus M2 allows ONE active campaign per user: `run` answers 409 `conflict` for a second campaign while the first has anything in flight (a cancelled campaign included until its in-flight items finish). Phase 4's job engine
   needs a per-user serialisation rule and must treat that 409 as backpressure.
3. **The report vocabulary is closed and already mapped.** States `queued assigned sending sent failed bounced cancelled`; `reason` (12 names) and `error_class` (6) always present as keys; `failed` has `pool_exhausted` and `attempts_exhausted`
   (both free in rules.psv); campaign totals partition `total`; pagination is exact-once for a quiescent campaign (`page_size` 100 default, 200 max, silently clamped; read the echoed `page_size`; `next_cursor` verbatim;
   a forged cursor is a 500); totals and items are separate queries; `attempt_count` is NOT an attempt count; `cancel` returns a position, not a delta.
4. **A real worker + the existing `smtp-ctl` sink needs exactly one new service (`m2-worker`), one env line on `m2` (admin key hashes, otherwise the admin API is not mounted and no pool/account can be made), one 5c stage that seeds the
   `send_managed.boxes` row before the worker starts, and one new test.** The account must name the sink's PRIVATE IP (not the name `smtp-ctl`) and the test asserts the range. One worker = one job at a time. The sink is driven by one
   mode set up front with disjoint recipient lists (reject, defer, hold). No module source is modified; secrets come from M2's own tools and are masked.
5. **Defaults make `failed/attempts_exhausted` take about 32 minutes; the dials make it seconds, and the end-to-end path has never run.** Set `job_max_failed_attempts=2` and `retry_backoff_schedule {[1,1,1],1}` (the age cap's 60 s floor cannot
   shorten it), `send_delay_min/max_sec=0`. No M2 test has driven the worker's RCPT-550 or RCPT-451 path against a real server, so the first runner run is the proof and may expose a real module defect: flag it in the phase spec,
   do not patch M2.
