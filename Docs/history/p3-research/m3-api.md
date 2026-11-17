# Phase 3 research: module 3 (send-byo): the platform-facing API, what the harness already runs, what an adapter test still needs

Read-only research for the Phase 3 spec (Part B adapters). Written from module CODE; docs were read only to find where to look and are marked where they disagree with the code.

**Pins and abbreviations.** M3 = coldmail-send-byo at c70f4b6 (the pin in `tools/modules.lock`); M2 = coldmail-send-managed at ff8c302; platform = coldmail-platform. Paths: `M3C/` = `coldmail-send-byo/services/control/internal/`; `M3W/` = `coldmail-send-byo/services/worker/src/`; `M3D/` = `coldmail-send-byo/db/migrations/`; `M2C/` = `coldmail-send-managed/services/control/internal/`; `PH/` = `coldmail-platform/tests/harness/`. Every claim has file:line.

**What is proven and what is not.** Nothing was run here (no Docker, no Postgres). JSON bodies below are **constructed from the handler code and the handler tests** unless marked "from the run log": they are not captured responses, so Phase 3 must pin fixtures from a runner capture. The only measured numbers come from the log of one hosted harness run (run 44144622152, platform@4ecc704, the E2 close-out run, read with `gh run view --log`); they are labelled as such. Anything I could only read is labelled "read, not run".

---

## 0. The five things the lead must know before writing the spec

1. **M3 has no cancel.** M2 has `POST /v1/cancel` (`M2C/platformapi/method_dispatch.go:169-177`, `cancel.go:87`) and a `cancelled` state; M3 has neither (M3's route table is quote/run/report/describe plus the `/v1/send-byo/*` extensions, `M3C/platformapi/method_dispatch.go:155-166`; no `cancel` string anywhere in its Go source). The locked job flow says "pause, cancel, resume". For M3 the only levers are per-user suspend (no new claims; queued items stay queued, and `campaign_expiry_mode` is seeded `shadow` so they never expire, `M3D/send_byo_0053...:177`) and account disable/pause (which makes READY queued items terminal `failed`, section 5). That is an owner decision (additive module route vs platform-side semantics), not an adapter detail.
2. **`run`'s 409 is one opaque answer for four different causes** (suspended, another campaign active, campaign cap, queue cap: `M3C/platformapi/store.go:75-77, 101-103, 117-119, 126-128`, all `conflict` with the generic message and no `details[]`), and **quote does not predict two of them** (it never reads suspension and it computes `max_campaign_size` but never uses it, `quote_feasibility.go:93-96, 118, 143-157`). The adapter cannot classify a 409; the spec must make the platform pre-check from its own state (entitlement, its own one-active-job rule, the caps in `quote.caps`).
3. **The account surface is not a clean state machine for a user app.** Create is not idempotent (a retry is 409); verify-on-add runs inside a 10 s request budget and can leave `unverified`, and, read not run, may even return a 500 with the row already created (section 12.7); a PATCH of the credentials neither resets nor re-runs verification; the test button's limit (5 per account, 20 per user, rolling 24 h) counts **every** verification attempt including create and bulk rows; bulk-add is ONE synchronous request that sleeps 2 s per row (500 rows is over 17 minutes). The adapter needs per-route timeouts and a reconcile-by-list path.
4. **The tenancy contract is explicit and uniform (`user_id` in the body for quote/run/create/bulk, in the query for everything else), but the cross-tenant ANSWER is not uniform:** job/account reads are 404 `not_found` (same as nonexistent), the campaign report and account events are **200 empty**, quote answers for the caller's own (possibly empty) account set, and an unknown `user_id` on account create is 400 with `user_id: not_found`. A cross-tenant test needs one expected answer per route (table in section 3.4).
5. **The harness already runs M3 for real** (Control, three workers, Mailpit and a controllable sink, From-gate OFF, service token and admin key both available to the test container, `PH/docker-compose.yml:174-318`, `PH/run.sh:199-221`), so the Phase 3 M3 test can use real routes with SQL-seeded users and job items, like `PH/m3_live_test.go`. Two limits: the platform SERVICE container has only module 3's ADMIN token (`docker-compose.yml:325-326`, `internal/config/config.go:41-47`), so an adapter that runs inside the service needs a new Domain-A token variable; and a Go test inside the harness container cannot stop containers, so "outage mid-job" needs a `run.sh` stage (like stage 9b, `run.sh:283-304`) or a fault proxy.

Also keep in mind (not in the five): M3 serves ANY user that exists in `public.users` until the platform suspends them (row absent means not suspended, `M3D/send_byo_0031...`), so entitlement enforcement for a never-enabled user is entirely the platform's job.

---

## 1. Surfaces, credentials, configuration

One Go process serves everything on **one port** (`CONTROL_HEALTH_ADDR`, default `:8080`, `M3C/config/config.go:54`; `httpapi/health.go:36-76`):

| Surface | Paths | Auth | Mounted when |
|---|---|---|---|
| Probes | `GET /healthz`, `GET /readyz` (any verb) | none | always (`health.go:39-40`) |
| Domain A (platform, service token) | `POST /v1/quote`, `POST /v1/run`, `GET /v1/report`, `GET /v1/describe`, and 18 extension routes under `/v1/send-byo/` | `Authorization: Bearer <service token>` | `PLATFORM_TOKEN_HASHES` non-empty (`config.go:99-100`, `cmd/control/main.go:272`) |
| Domain B (operator, admin key) | `/v1/admin/*` (accounts, config, health, users/{id}/suspend, unsuspend, suspension, config) | `Authorization: Bearer <admin key>` | `ADMIN_API_KEY_HASHES` non-empty (`config.go:93-95`, `main.go:162`) |

- **How each credential is configured.** Both are **comma-separated bcrypt hashes in an env var**; the module never sees the plaintext at rest; the caller sends the plaintext as a Bearer token. The two domains never share a credential (the harness proves both directions: `PH/m3_live_test.go:111-122`). With no hashes the surface is **not mounted at all** (a mounted-surface 404 and an unmounted-surface 404 are the same `not_found` envelope, so a missing admin mount reads as "unknown user" to `m3admin`; `m3admin.go:50-55`). The harness mints both with module 2's `provision-worker admin-key` (`PH/run.sh:62-80`) and passes `PLATFORM_TOKEN_HASHES` / `ADMIN_API_KEY_HASHES` to `m3` (`docker-compose.yml:187-188`).
- **Auth is a bcrypt compare on every request** against every configured hash, no early return, no cache (`M3C/platformapi/auth.go:37-49`; cost is bcrypt DefaultCost from `provision-worker`, `coldmail-send-managed/tools/provision-worker/main.go:105`). Measured in the cited run: 36 `run` calls took 1.717 s and 24+20 took 2.082 s, about 47 ms per call (from the run log). Poll cadence times users times hashes is CPU in the module.
- **Error to expect from the wrong domain:** the service token on an admin route is 401 `unauthenticated` with the admin message "Missing or invalid admin API key." (`M3C/adminapi/errors.go:50`); the reverse says "Missing or invalid platform service token." (`platformapi/errors.go:59`).
- **Health.** `GET /healthz` is `{"status":"ok","at":"<RFC 3339>"}`. `GET /readyz` is 200/503 with `{"status":"ready"|"not_ready","checks":{"db":bool,"singleton_lock":bool},"db":bool,"singleton_lock":bool,"at":...}` (`health.go:151-174`); it does **not** look at workers: a stack with no worker is "ready" and its verifications fail (section 8.1). The platform probe already asserts both for M3 (`internal/probe/checks.go:149-166`). Worker liveness is only on `GET /v1/admin/health` (`workers.active`, admin key).
- **`describe`** (Domain A, token required): `module:"send_byo"`, `api_version:"v1"` (derived from the route table, `method_dispatch.go:103-120`), `capabilities`, `pricing` (`model:"flat_subscription"`, `models`, `unit_price_credits:0`, `tiers`, `cost_fields_inert:true`), `extension_routes[]` (18), `schemas`, `vocabularies{}` (live `pg_enum`), `error_codes[]`, `bulk_add{warning_code,warning_text}` (`handlers.go:327-418`). The version marker is `api_version`; the suspension read's marker is `version` (section 9).

## 2. Conventions every route shares

**Error envelope** (`platformapi/errors.go:113-121`): `{"error":{"code":"<code>","message":"<fixed sentence>","details":[{"field":"<f>","issue":"<i>"}],"request_id":"req_<32 hex>"}}`. `details` is omitted when empty. `request_id` is minted by the module (`errors.go:124-130`); it is on every error, and in the bulk-add success body; **there is no request_id on other successes and M3 ignores any inbound correlation header** (no such code), so the platform logs the module's request_id from errors only.

| `code` | HTTP | Domain A emits it | Notes |
|---|---|---|---|
| `validation_failed` | 400 | yes | `details[]` usually names the field |
| `unauthenticated` | 401 | yes | |
| `not_found` | 404 | yes | also an unknown path under `/v1/send-byo/` and any malformed `{id}` |
| `method_not_allowed` | 405 | yes | with an `Allow` header (`method_dispatch.go:216-243`) |
| `conflict` | 409 | yes | many causes, mostly opaque |
| `unprocessable` | 422 | yes | lifecycle preconditions, batch too large, From-gate refusals |
| `rate_limited` | 429 | yes | test button only; no `Retry-After` |
| `internal_error` | 500 | yes | |
| `paused` | 503 | yes (`run` only) | dispatch rung `refuse_new`/`stopped` (`handlers.go:177-180`) |
| `forbidden` 403, `config_ordering_violation` 400 | | admin only | `adminapi/errors.go:22-33` |

Real shapes (constructed from the fixed messages and the handlers; `request_id` is `req_` plus 32 hex):
```json
{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"user_id","issue":"required"}],"request_id":"req_0123456789abcdef0123456789abcdef"}}
{"error":{"code":"not_found","message":"The requested resource was not found.","details":[{"field":"job_id","issue":"not_found"}],"request_id":"req_..."}}
{"error":{"code":"conflict","message":"The request conflicts with the current state.","request_id":"req_..."}}
{"error":{"code":"conflict","message":"This action is not available for your account right now.","request_id":"req_..."}}
{"error":{"code":"unprocessable","message":"This account is already paused.","details":[{"field":"paused","issue":"conflict"}],"request_id":"req_..."}}
{"error":{"code":"paused","message":"Dispatch is paused by an operator; retry later.","request_id":"req_..."}}
```
(second line: a cross-tenant or unknown report; third: any of `run`'s four 409 causes; fourth: the suspended-user sentence on create, bulk, test and test-send.)

`describe.error_codes[]` publishes exactly the nine Domain A codes (`describe_vocabularies.go:81-92`). `details[].issue` vocabulary: `required, malformed, out_of_range, not_found, conflict` (Domain B adds `forbidden_value`). **The message is a fixed sentence per code** (`errors.go:57-67`) except a few curated overrides (lifecycle refusals, the suspension sentence, `from_dns_unavailable`); branch on `code` plus `details`, never on the message. Every non-2xx body, including mux 404/405 and the root-mux 404, is this envelope (`method_dispatch.go:205-243`, `health.go:86-95`).

**Request decoding.** Two decoders, two 400 shapes: `run`/`quote` use `decodeStrict` (unknown field or wrong type gives `validation_failed` with NO details, except a wrong-TYPE field which names it as `malformed`; an empty body falls through to the required-field checks; trailing data after the first JSON value is silently ignored; no explicit size cap) (`handlers.go:445-472`); every `/v1/send-byo/*` route uses `decodeStrictPresence` (1 MiB cap; a body that is not a JSON object, an unknown field, or a wrong type all give 400 `validation_failed` with `details:[{body,malformed}]`: the field is NOT named) (`decode.go:13-41`). UUIDs are matched case-insensitively and echoed as stored.

**Time formats.** Single-job report `completed_at`/`failed_at`: RFC 3339 with fractional seconds (`handlers.go:294-296`, a Go `time.Time`); campaign items, account objects, events, notifications: `YYYY-MM-DDTHH:MM:SSZ` (second precision, `campaign_report.go:338`, `account_store.go:78-91`).

## 3. Tenancy

### 3.1 How a user is identified
There is no header and no token claim. The platform passes **`user_id` (UUID) explicitly**: in the **body** for `quote`, `run`, `POST /v1/send-byo/accounts`, `POST /v1/send-byo/accounts/bulk`; in the **query** (`?user_id=`) for `report`, every other `/v1/send-byo/*` route (including `DELETE` and `POST` action routes, which carry no body) (`requireTenantSelector`, `account_read.go:107-117`). It is **required**: empty gives 400 `validation_failed` with `details:[{user_id,required}]`, non-UUID gives `details:[{user_id,malformed}]`. Every module query is `WHERE user_id = $tenant`. The module trusts the platform to pass the right id (defence in depth, `server.go` header, OD-25).

### 3.2 How `public.users` and `public.job_items` are read
- `public.job_items` (SELECT, plus test-send's INSERT): `run` reads `campaign_id` (`store.go:84-86`) then copies `recipient_email, rendered_subject, rendered_body, reply_to` into `send_byo.jobs` with `WHERE ji.id = $1 AND ji.user_id = $2` (`store.go:146-153`). The copy is **frozen at `run`**: later edits to the job item are not seen. `status` is never read. `jobs.job_item_id` is UNIQUE and cascades from the job item.
- `public.users`: only `SELECT EXISTS(... WHERE id=$1)` (`account_store.go:216-220`): account create/bulk (unknown gives 400), admin suspend/read/config (unknown gives 404). `run`, `report`, `quote` do not check it. M3 tables have FKs to `public.users(id)`.
- **test-send writes to the platform table**: `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1,$1,...,'pending')` with `campaign_id = user_id` (`M3C/dispatch/send_test_action.go:107-109`), recipient = the account's own from address. The platform gets a job item it did not create (`db/module-reads.txt` J4 already declares this).

### 3.3 One active campaign per user
`run` refuses (409) when the user has a non-terminal job of a **different** `campaign_id` (`store.go:94-103`). A test-send job has `campaign_id = user_id`, so a test-send in flight blocks a real `run` for that user for the seconds it is non-terminal. The platform's own "one active job per module per user" lock should not be relied on to prevent this.

### 3.4 The cross-tenant matrix (tenant B asks for tenant A's resource); all from code
| Call | Answer |
|---|---|
| `POST /v1/run` A's `job_item_id` with B's `user_id` | 404 `not_found`, `details:[{job_item_id,not_found}]` (`store.go:88-90, 169-171`), same as a nonexistent id |
| `GET /v1/report?job_id=<A's>&user_id=B` | 404 `not_found`, `details:[{job_id,not_found}]` (`handlers.go:270-276`); `?job_item_id=` names `job_item_id`; identical to nonexistent except `request_id` |
| `GET /v1/report?campaign_id=<A's>&user_id=B` | **200**, `totals` all 0, `totals_by_account:[]`, `items.results:[]`, `next_cursor:null`, `progress.is_complete:false` (`campaign_report.go:73-80`) |
| `GET/PATCH/DELETE /v1/send-byo/accounts/{A's}?user_id=B`, and every `POST .../{id}/...` action | 404 `not_found`, no `details` (`account_store.go:181-190`, `getAccount` / `readFlags` scoped by user) |
| `GET /v1/send-byo/accounts/{A's}/events?user_id=B` | **200** `{"items":[],"next_cursor":null}` (`account_events.go:46-47`, no existence check) |
| `GET /v1/send-byo/accounts?user_id=B` | B's own accounts only |
| `POST .../test-send` or `/test` for A's account as B | the suspension pre-check is on B (`account_test_send.go:31`), then 404 |
| `POST /v1/quote` for B | B's own feasibility; an unknown user is zero accounts, so `feasible:false`, `no_eligible_accounts` (200) |
| unknown `user_id` on account create / bulk | 400 `validation_failed`, `details:[{user_id,not_found}]` (`account_create.go:83-87`, `account_bulk_add.go:109-113`): existence is disclosed to the platform |
| unknown `user_id` on `run` | 404 `job_item_not_found` (the scoped read finds no row) |

---

## 4. The routes

### 4.0 All 22 Domain A routes (Bearer service token; `M3C/platformapi/method_dispatch.go:155-166`, `describe_routes.go:40-123`)
| describe id | Method and path | Tenant | Does |
|---|---|---|---|
| (core) | `POST /v1/quote` | body | feasibility of N recipients (4.1) |
| (core) | `POST /v1/run` | body | accept one job item (4.2) |
| (core) | `GET /v1/report` | query | one job, one job item, or a whole campaign (4.3, 4.4) |
| (core) | `GET /v1/describe` | none | self-description (section 1) |
| accounts.create | `POST /v1/send-byo/accounts` | body | create plus verify-on-add (8.1) |
| accounts.bulk_add | `POST /v1/send-byo/accounts/bulk` | body | N accounts, paced (8.3) |
| accounts.list | `GET /v1/send-byo/accounts` | query | the user's accounts, paged |
| accounts.get / update / delete | `GET`, `PATCH`, `DELETE /v1/send-byo/accounts/{id}` | query | read, change, soft delete (204) |
| accounts.events | `GET /v1/send-byo/accounts/{id}/events` | query | the account's audit trail, paged |
| accounts.disable / enable / pause / unpause | `POST /v1/send-byo/accounts/{id}/<verb>` | query | manual flags, optional `{reason}` body |
| accounts.unquarantine | `POST /v1/send-byo/accounts/{id}/unquarantine` | query | self-serve, rate limited |
| accounts.test | `POST /v1/send-byo/accounts/{id}/test` | query | credential check plus From re-check |
| accounts.test_send | `POST /v1/send-byo/accounts/{id}/test-send` | query | real send to the account's own address |
| settings.get / put | `GET`, `PUT /v1/send-byo/settings` | query | `routing_strategy` |
| usage.get | `GET /v1/send-byo/usage` | query | per UTC day counters |
| notifications.list | `GET /v1/send-byo/notifications` | query | read-only list |

### 4.1 `POST /v1/quote` (`handlers.go:52-122`, `quote_feasibility.go`)
Request `{"recipient_count": <int >= 0>, "user_id": "<uuid>"}`; both required (`recipient_count` null/absent is `required`, negative is `out_of_range`, a wrong type is `malformed` naming the field); unknown fields are 400. **The same body works on M2** (M2 tolerates and ignores `user_id`, `M2C/platformapi/handlers.go:49-65`). Pure: reserves nothing, creates nothing.

Response 200 (constructed; Go map keys serialize alphabetically): 
```json
{"cost":0,"currency":"credits","unit_price_credits":0,"tier":"shared","recipient_count":50,
 "feasible":true,"reasons":[],
 "caps":{"queue_depth_cap":100000,"daily_send_cap":500,"account_count_cap":20},
 "capacity":{"eligible_accounts":1,"daily_capacity_remaining":500,"effective_concurrency":3,
             "estimated_duration_sec":85,"estimate_basis":"current_eligible_capacity"},
 "warnings":[]}
```
Infeasible: `"feasible":false,"reasons":[{"code":"exceeds_queue_cap","limit":100000,"requested":150000,"cap_name":"queue_depth_cap"}]` and `capacity` without the two estimate keys (`quote_feasibility.go:303-350`). `warnings[]` holds `{"code":"long_duration","estimated_duration_sec":N,"threshold_sec":M}` when the projection crosses `quote_long_duration_warn_sec`.

**Semantics.** `feasible` means "this request can be accepted as submitted", never "it will be fast": exceeding today's daily capacity is feasible with a long projection. The FIRST binding reason wins, in this order (`quote_feasibility.go:143-157`): `dispatch_disabled` (only `dispatch_enabled=false`; the `dispatch_mode` rung is NOT read), `exceeds_queue_cap` (`recipient_count > max_queued_jobs`, resolved per user override then global), `no_eligible_accounts` (the user has zero non-deleted accounts), `all_accounts_unverified` (none has `credential_verification_state='verified'`), `from_gate_blocked` (every non-deleted account is From-`blocked`), else `no_eligible_accounts` (none eligible now). Eligible means (`campaignEligibleAccountsSubqueryDirect`, `:278-299`): not quarantined, not disabled, not deleted, credential `verified`, **From-gate state `aligned`, or `unaligned_warned` with the acknowledgment recorded for the current verdict fingerprint**, and not paused (or paused only by `daily_limit` after the reset, or by an expired `throttle`) with `daily_sent_count < daily_limit`. Quota: the per-account cap is 20 (`account_count_cap_per_user`), `daily_limit` default 500 (max 100000, `account_validate.go:122`); `capacity.daily_capacity_remaining` is informational only and never makes a quote infeasible. `estimated_duration_sec = ceil(n / lanes) * midpoint(send_delay_min, send_delay_max)` (`:201-211`; the default is 3-7 s, so 5 s per send per lane). `cost` is always `0` at the seeded `unit_price_credits=0` and `describe.pricing.cost_fields_inert` says so.

### 4.2 `POST /v1/run` (`handlers.go:126-205`, `store.go:42-178`)
Request `{"job_item_id":"<uuid>","user_id":"<uuid>"}` (both required). Order: validate, **rung gate** (503 `paused` at `refuse_new`/`stopped`, fails OPEN on an unreadable rung), then `createJob`:
1. existing job for (job_item, user)? return it (`store.go:45-47`) **before any gate**;
2. under a per-user advisory lock: suspended gives 409; job item not found for this user gives 404; a different campaign has a non-terminal job gives 409; `campaign` already holds `max_campaign_size` (default 50000) jobs gives 409; the user's live queue holds `max_queued_jobs` (default 100000) gives 409; upsert the campaign row (freezing `campaign_expiry_sec`); insert the job.

Responses: **202** `{"job_id":"<uuid>","state":"queued"}` (new); **200** `{"job_id":"<uuid>","state":"<current state>"}` (this job item already has a job: idempotent); 400 (`job_item_id_required` with `details:[{job_item_id,required}]`, `user_id` required/malformed); 404 `not_found` with `details:[{job_item_id,not_found}]`; 409 `conflict` (generic message "The request conflicts with the current state.", no details, **four indistinguishable causes**); 503 `paused`. `run` kicks the dispatcher (`s.kick()`), never waits for the send. One HTTP call per item: there is no bulk run.

### 4.3 `GET /v1/report`, single job (`handlers.go:209-315`, `store.go:263-295`)
Query `?user_id=<uuid>` plus one of `job_id`, `job_item_id` (`job_id` wins if both) or `campaign_id` (wins over both). None of the three gives 400 `job_id_or_job_item_id_or_campaign_id_required`. Malformed ids give 400 `malformed`. Response 200: `job_id`, `state`, `attempt_count` are always there; every other key is **omitted until set** (not null):
```json
{"job_id":"<uuid>","state":"sent","attempt_count":1,
 "provider_message_id":"<Message-ID>","completed_at":"2026-11-17T12:00:01.123456Z",
 "account_id":"<uuid>","account_username":"sender-1@example.test"}
```
plus `smtp_response` (the provider's scrubbed text), `error_class` (one of the six), `failed_at` (only for terminal `failed`/`bounced`). **For a `sent` job `smtp_response`, `error_class`, `failed_at` are always omitted** (the underlying columns are sticky across retries, `provider_text.go:61-72`); for `queued`/`assigned`/`sending` they are present when the last attempt failed. `account_id`/`account_username` are omitted until a job is dispatched (and again after a requeue clears it). There is **no `reason` key** (M2 has one).

### 4.4 `GET /v1/report?campaign_id=<uuid>&user_id=<uuid>[&cursor=][&page_size=]` (`campaign_report.go:81-140`)
```json
{"campaign_id":"<uuid>","dispatch_paused":false,
 "totals":{"total":36,"sent":10,"bounced":0,"failed":0,"in_flight":3,"pending":23,"waiting_for_capacity":0},
 "totals_by_account":[{"account_id":"<uuid>","account_username":"sender-1@example.test","sent":10,"bounced":0,"failed":0,"in_flight":3}],
 "progress":{"terminal":10,"fraction_complete":0.277,"is_complete":false},
 "items":{"results":[{"job_item_id":"<uuid>","state":"sent","provider_message_id":"<id>","completed_at":"2026-11-17T12:00:01Z",
                      "attempt_count":1,"account_id":"<uuid>","account_username":"...","smtp_response":null,"error_class":null,"failed_at":null}],
          "next_cursor":"<opaque>|null","page_size":100}}
```
Item keys are **always present, `null` when unset** (the opposite of 4.3, enforced by `report_null_convention_test.go`). `terminal = sent+bounced+failed`; `is_complete = total>0 && terminal==total`; `in_flight = assigned+sending`; `pending = queued - waiting_for_capacity`; `waiting_for_capacity` counts queued items when the user has **no currently eligible account** (the quote predicate, `:155-206`). Items are ordered `(created_at, id)` ascending = the order `run` accepted them.
- `totals.total` counts **only the jobs `run` has accepted so far**: while the platform is still submitting, `is_complete` can be true at a partial total. Compare `total` with the number of items the platform submitted.
- `dispatch_paused` is the **global** rung only (`dispatch_enabled=false` or `dispatch_mode != serving`, `:383-409`); a per-user suspension is NOT visible here (suspended items just stay `pending`).
- The four reads (totals, per-account totals, items page, paused flag) are **separate statements, not one snapshot** (`:97-116`): an item can move between the totals query and the items query. Settle on a final stable pass, not on one poll.

### 4.5 `GET /v1/describe` and health: section 1.

---

## 5. Item states, reasons, totals

Per-item `state` is the `send_byo.job_state` enum, additive-only: `queued, assigned, sending, sent, failed, bounced` (`M3D/send_byo_0003...:7-14`). **No `cancelled`.** `error_class`: `bounce_permanent, transport_4xx, auth_failure, throttle, timeout, unknown` (`:16-23`). Both are published live in `describe.vocabularies`.

| State | Terminal | What it means (code) |
|---|---|---|
| `queued` | no | not claimed yet, OR requeued after a transient failure (4xx, timeout, throttle, no worker, reclaim). After a failed attempt `error_class`/`smtp_response` are set and `attempt_count` counts it; it **saturates at `max_attempts=3`** (`M3D/0007:25`, `dispatch.go:542-547`), and the job is retried **forever** on the ladder 30 s, 120 s, 600 s then 600 s (`dispatch/config.go:195, 215`). An item never fails by attempt count. |
| `assigned` / `sending` | no | claimed with a lease / handed to a worker. Lease `job_max_processing_time_sec` 120 s; a dead worker's jobs are reclaimed to `queued`. |
| `sent` | yes | SMTP accepted; `provider_message_id` (the Message-ID) and `completed_at` set. Exactly-once under normal retry and replay; a crash between SMTP acceptance and a persisted report can duplicate (the same crash-window class as M2). |
| `bounced` | yes | permanent recipient rejection (`bounce_permanent`, 550); never retried (the hostile test asserts one RCPT each, `PH/m3_hostile_test.go:429`). |
| `failed` | yes | (a) **account exhaustion**: the user has at least one account and **none** is eligible or self-recovering, so all READY queued items fail on the next tick (`dispatch/exhaustion.go:37-61, 139-152`): accounts that are quarantined, disabled, deleted, manually paused, **credential not `verified`**, or From-gate not aligned (`eligibility.go:56-61`) are terminal-blocked; (b) campaign expiry, only when `campaign_expiry_mode=enforce` (seeded `shadow`). The report carries no reason. |

`error_class` (what the module did with the attempt, from the enum comments `M3D/0003:16-23` and `dispatch/classify.go`): `bounce_permanent` item becomes `bounced`; `transport_4xx` requeue with backoff; `auth_failure` requeue, and the account is quarantined for `auth_failure` **only when `autoquarantine_mode=enforce`** (seeded `shadow`: it only records a would-quarantine row, `dispatch/account_effect.go:247-265, 351`); `throttle` requeue, and the account is paused for `throttle` only when `autopause_mode=enforce` (seeded `shadow`, same file `:118-141`); `timeout` lease expiry, worker heartbeat miss or connectivity, requeue (a Control-declared reclaim never scores the account); `unknown` catch-all, requeue. It is present on a `queued`/`assigned`/`sending` item after a failed attempt (sticky) and absent on `sent`.

What does NOT fail an item: no accounts at all (waits forever); every account throttled or at its daily limit (waits for the reset); no worker (requeues). `waiting_for_capacity` is an **aggregate** distinction only; a single item reads plain `queued` and the single-job report has no way to say why (M2's does).

**Observed live** (the green E2 test, `PH/m3_hostile_test.go:378-476`): a recipient answered 550 at RCPT is `bounced` / `bounce_permanent` with `smtp_response` containing 550; one answered 451 stays `queued` with `error_class:"transport_4xx"`, `smtp_response` containing 451, and `attempt_count` rising 1, 2, 3 across the 30 s then 120 s backoff, and is sent once the sink relents.

Charge mapping already exists: every M3 state is Free by rule M3-01 (`internal/charge`, asserted in `PH/m3_hostile_test.go:797-870`).

## 6. Pagination

| Listing | Param names | Default / max | Envelope | Order |
|---|---|---|---|---|
| campaign report items | `cursor`, `page_size` | 100 / 200 (silently clamped; zero, negative or non-numeric falls back to 100, never a 400) | `items.{results,next_cursor,page_size}` | `(created_at, id)` asc |
| `accounts.list`, `accounts.events`, `notifications.list` | `cursor`, `limit` | 50 / 200 (same silent clamp, `pagination.go:49-62`) | `{"items":[...],"next_cursor":"<opaque>"|null}` | `(created_at, id)` asc (notifications: `first_seen_at, id`) |

- The cursor is opaque base64url of `<created_at>|<id>`; pass back exactly what was returned. Non-base64 or no `|` gives 400 `cursor_malformed` (`details:[{cursor,malformed}]`).
- **Exact-once after the Phase 2 fix.** The campaign cursor anchors on the last row KEPT (`keys[pageSize-1]`, `campaign_report.go:323-361`, no longer the lookahead row), and the list cursors carry a microsecond key (`pagination.go:22-35`). `pagination_exact_test.go` walks every size and requires exact set equality with paired controls. The last page has `next_cursor:null`. New jobs appended during a walk appear at the end (later `created_at`).
- **Read, not run:** the cursor's two halves are cast in SQL (`::timestamptz`, `::uuid`) with no validation beyond base64 and the `|`, so a hand-built cursor that decodes but is junk reaches the cast and is a 500 `internal_error`, not a 400 (`pagination.go:69-85`, `campaign_report.go:291, 418-433`). The notifications test only covers the non-base64 case. Treat the cursor as opaque.
- `describe.schemas` is wrong in two places (FW-M3-16 records the first): the list fragments name `results` where the handlers return `items`; and `account_object` is declared flat (`disabled`, `paused`, `quarantined` at top level, `describe_routes.go:213-220`) while the real object nests `status{}`, `verification{}`, `from_alignment{}` (`account_store.go:50-65`). Build the adapter's types from the handlers, not from `schemas`.

## 7. Idempotency and resume

| Operation | Idempotent? | Retry/resume behaviour |
|---|---|---|
| `run` | **yes**, by `job_item_id` (UNIQUE) per tenant | repeat gives 200 with the current state, even for a suspended user or a full cap (the lookup precedes the gates, `store.go:45-47`); safe to re-submit every item after a platform crash |
| `report`, `quote`, `describe`, lists | read-only | |
| account create | **no** | a retry after a lost response is 409 `conflict` with `details:[{smtp_username,conflict}]` (dup of `(user, host, username)`); the first call DID create the row. Recover by list. |
| bulk-add | per row, no | a repeated row is `rejected` / `duplicate_account_for_user` |
| `DELETE account` | effectively | second delete is 404 (same as never existed) |
| lifecycle POSTs | state-guarded | already in the target state is 422 (`already_in_state:...`), with `details:[{<flag>,conflict}]` |
| admin suspend / unsuspend | **status codes are not idempotent, effects are** | already suspended gives 422 `details:[{suspended,conflict}]` (`user_suspend.go:121-126`); the read route is the confirmation (section 9) |
| a `failed` or `bounced` item | **cannot be re-run** under the same `job_item_id` | `run` returns the existing job (200); a retry needs a new job item |

No `Idempotency-Key` header exists anywhere on M3.

---

## 8. The account surface (Domain A, `/v1/send-byo/*`)

All need `?user_id=` except create/bulk (body). Registry: `M3C/platformapi/describe_routes.go:40-123`. Recommended grouping for a thin Phase 3 adapter (a recommendation, not a fact):

- **Tier 1 (a user app needs them now):** list, create, bulk-add, test, get, PATCH (change password/host/limit), delete.
- **Tier 2 (thin pass-through, test one):** disable, enable, pause, unpause, unquarantine, events, test-send, notifications.list, settings get/put, usage.
- **No route exists to clear or acknowledge a notification** (only `GET /v1/send-byo/notifications`, oldest first, `{"items":[...],"next_cursor":...}`); `cleared_at` is stamped by the module when the condition resolves (self-serve unquarantine, credential auto-clear). `condition` is `account_quarantined | campaign_expired`; `state` is `pending | delivered | suppressed | cleared` (`describe_routes.go:255-256`).

**Account object** (create 201, get, PATCH, lifecycle 200; constructed from `accountResource`, `account_store.go:50-65`; `omitempty` keys are absent when unset):
```json
{"id":"<uuid>","user_id":"<uuid>","smtp_host":"smtp.example.test","smtp_port":587,"smtp_username":"sender-1@example.test",
 "from_address":"sender-1@example.test","from_name":"Harness","has_credential":true,
 "status":{"paused":false,"pause_reason":null,"quarantined":false,"disabled":false},
 "verification":{"state":"verified","verified_at":"...","verify_at":"...","smtp_response":"235 ..."},
 "from_alignment":{"state":"aligned","checked_at":"...","message":"..."},
 "daily_limit":500,"created_at":"...","updated_at":"..."}
```
`status.quarantine_reason`/`quarantined_at` appear only when quarantined; `verification.code` only when `failed`. The secret, ciphertext, nonce and key id are never returned.

### 8.1 Create (`account_create.go:33-122`)
Body `{user_id, smtp_host, smtp_port?(default 587), smtp_username, smtp_secret, from_address, from_name?, daily_limit?(default `daily_limit_default`), acknowledge_alignment_warning?}`. Limits (`account_validate.go`): host IP or hostname up to 255; port 1-65535; username 1-320; from_address regex up to 320; from_name up to 255; `daily_limit` 1-100000; `smtp_secret` non-empty (a missing/empty secret is reported as `malformed`, not `required`). Order: validate, user exists (400), **suspension refusal (409)**, From-gate (DNS) then the create transaction (cap 20 per user: 409 `account_cap_reached`; duplicate: 409), THEN verify-on-add (synchronous, through a worker), then a re-read. **201 always reflects the stored row, verified or not.**
- Port 465 is implicit TLS, anything else STARTTLS-before-AUTH (`M3C/dispatch/dispatch.go:599-606`); the worker skips certificate validation for `localhost`, `smtp-sink` and private/loopback/link-local IPs (`M3W/smtp.js:156-165, 202`). **The module accepts any IP as `smtp_host`** (`account_validate.go:83-85`): the platform must decide whether a production user may name a private address.
- `credential_verification_state` (`M3D/0035:32-36`): `unverified` (never checked or the attempt did not complete), `verified`, `failed` with `verification.code` from the closed set `verify_connect_failed, verify_timeout, verify_tls_unavailable, verify_tls_failed, verify_auth_failed, verify_protocol_error, verify_connection_lost` (`dispatch/verify.go:297-312`). **No worker connected is recorded as `failed` / `verify_connection_lost`** (`verify.go:130-136`), not `verify_connect_failed` as `Docs/api-contract.md` says (the docs are stale on this).
- Only `verified` accounts are routable; so are only From-`aligned` (or acknowledged `unaligned_warned`) ones. An unverified or failed account never sends and never errors; its user's READY jobs fail (section 5).

### 8.2 Test button `POST /v1/send-byo/accounts/{id}/test` (`account_test_button.go:42-160`)
Body `{}` or `{"acknowledge_alignment_warning":true}`. Re-runs the credential check through a worker AND the From-gate against the stored address. 200:
```json
{"verification":{"state":"verified","smtp_response":"235 ..."},"from_alignment":{"state":"aligned","message":"..."}}
```
A failed credential is still 200: `{"verification":{"state":"failed","smtp_response":"...","code":"verify_auth_failed"},...}`; an unavailable check is `{"state":"unavailable","code":"verify_error"|"verification_unavailable"}`; an indeterminate From check is `{"state":"indeterminate","code":"from_dns_unavailable"}`. The user-safe sentence per code is NOT returned here (only the code; the copy ledger is `coldmail-send-byo/Docs/user-copy-strings.txt`). **Rate limits** (`:85-118`): refused 429 `rate_limited` (message "Too many requests.", no details, no `Retry-After`) when the account has `>= account_test_rate_limit_per_day` (5) or the user has `>= test_button_rate_limit_per_user_per_day` (20) `verified` audit rows in the trailing 24 h. **Those rows are written by every verification attempt** (create, each bulk row, each test click, success or failure: `M3C/dispatch/verify.go:213-285`), so a create costs one of the account's five, and bulk-adding 21 accounts uses the user's whole day of test clicks. Available while quarantined/disabled/paused; refused 409 (fixed sentence) for a suspended user.

### 8.3 Bulk-add `POST /v1/send-byo/accounts/bulk` (`account_bulk_add.go:71-239`)
Body `{user_id, accounts:[{<create fields>, acknowledge_alignment_warning?}]}`; max `bulk_add_max_rows` (500) else 422 `batch_too_large` (`details:[{accounts,out_of_range}]`); empty list 400. Suspended: **whole request** 409. 200 `{"request_id":"req_...","results":[{...}]}`; `row` is 1-based:
- created: `{"row":1,"status":"created","account_id":"<uuid>","verification":"verified"|"failed"|"unverified","code":"verify_auth_failed"?,"smtp_response":"..."?,"from_alignment_state":"aligned","from_alignment_message":"..."?}` (`verification` is a **string** here, an object on the account);
- rejected: `{"row":3,"status":"rejected","code":"<slug>","details":[...]?}` with `code` one of `validation_failed` (with details), `account_cap_reached`, `duplicate_account_for_user`, `from_alignment_warning_unacknowledged`, **or the raw internal reason for an infrastructure failure** (`account_bulk_add.go:203`: e.g. `account_write_error: <error text>`): a two-vocabulary leak. Treat unknown `code` values as opaque and never display them.
- **Synchronous and paced**: one row at a time, `bulk_add_verify_interval_ms` (2000) between rows, each row's create+verify under a 10 s budget (`:143-151`, `:186`). 500 rows exceed 17 minutes in one HTTP request (the server has only a 5 s header timeout, `health.go:74`). A client disconnect stops pacing, leaves later rows unprocessed and earlier rows created (`:144-150`); there is no way to learn per-row results afterwards except listing accounts.

### 8.4 Other account routes
- **PATCH** (`account_update.go:33-...`): omit means unchanged; `user_id` is not a field (400). Changing `from_address` re-runs the From-gate (an indeterminate answer is 422 `from_dns_unavailable`, an unacknowledged WARN is 422 `from_alignment_warning_unacknowledged` with `details:[{acknowledge_alignment_warning,required}]`). **Changing `smtp_secret`/host/port/username does not touch `credential_verification_state` and does not verify** (the file never references it): the account keeps a stale `verified`, or a stale `failed`; the adapter must follow with `POST .../test`. A new secret auto-clears an `auth_failure` quarantine only.
- **DELETE**: soft delete, 204, retained `account_retention_window_sec` (30 days); the purge sweep is seeded `shadow`, so credentials are not actually purged until the owner promotes it (`M3 CLAUDE.md` rule 7; go-live checklist section 14).
- **Lifecycle** `disable, enable, pause, unpause` (`account_lifecycle.go:31-165`): body optional `{reason<=2000}`; refusals are 422 with curated messages and `details:[{<flag>,conflict}]`; `unpause` refuses a non-manual pause. **`unquarantine`** (`account_unquarantine.go:32-200`): 422 `not_in_state:quarantined`, or 422 `unquarantine_rate_limited` after `unquarantine_max_per_day` (3) unless a successful test postdates the quarantine within `unquarantine_evidence_ttl_sec`. There is no user route to quarantine.
- **test-send** (`account_test_send.go:13-73`): a real send to the account's own address; 202 `{"job_id":"<uuid>","state":"sending"}`; 409 for an ineligible account or no connected worker (both plain `conflict`, generic message); consumes one daily send; poll `GET /v1/report?job_id=`. Gated by suspension.
- **settings** `GET/PUT /v1/send-byo/settings?user_id=` `{"user_id","routing_strategy":"random|round_robin|one_by_one"}`; **usage** `GET .../usage?user_id=&from=&to=` (per UTC day, last 30 days default; `from`/`to` `YYYY-MM-DD`).

### 8.5 Suspension gating (the P2 close-out)
While suspended, **four routes refuse before any other work**: account create, bulk-add (whole), test button, test-send: 409 `conflict`, message exactly `This action is not available for your account right now.`, no `details[]`, never the word suspend (`suspension_gate.go:51, 68-83`; the same predicate is enforced in dispatch at `Verifier.Verify` and `Dispatcher.SendTest`). **`run` is also 409 but with the generic message** "The request conflicts with the current state." (`store.go:75-77`; the test's own control list rejects the generic message for the four routes, `suspension_gate_test.go:186`). Not gated: list, get, PATCH, lifecycle, delete, settings, usage, notifications, events, quote, report, describe.

---

## 9. Domain B routes the platform touches, and what `m3admin` covers

| Route | Request | Response |
|---|---|---|
| `POST /v1/admin/users/{id}/suspend` | optional `{"reason":"<=2000 bytes"}` | 200 `{"user_id","suspended":true,"reason":"operator"}`; 422 `details:[{suspended,conflict}]` if already; 404 unknown user |
| `POST /v1/admin/users/{id}/unsuspend` | same | 200 `{"user_id","suspended":false}`; 422 `details:[{suspended,conflict}]` if not suspended **and also for an unknown user** (no existence check, `user_suspend.go:158-195`) |
| `GET /v1/admin/users/{id}/suspension` | none | 200 `{"user_id","suspended":bool,"suspended_at":RFC3339Nano\|null,"reinstated_at":RFC3339Nano\|null,"version":int}`; a known never-suspended user is `suspended:false`, both null, `version:0`; unknown or malformed id is 404 `not_found` (`user_suspension_read.go:49-94`) |
| `GET/PUT/DELETE /v1/admin/users/{id}/config[/{dial}]` | PUT `{"concurrency_per_user":>0,"send_delay_min_sec":>=0,"send_delay_max_sec":>=0,"max_campaign_size","max_queued_jobs","campaign_expiry_sec"}` | per-user override row (`user_config_override.go:60-110`) |
| `GET/PUT /v1/admin/config[/{key}]` | PUT `{"value":...}` | global dial (`config_handlers.go`) |
| `GET /v1/admin/health` | none | queue counts, `workers.active`, rung, accounts |

`version` is the suspension marker: a monotonic counter of state transitions (first suspend 1, unsuspend 2, ...), bumped in the SAME statement that flips the flag (`user_suspend.go:43-56`), 0 for no row. Suspend means **no new claims**; up to `concurrency_per_user` in-flight sends finish after the call returns; resume latency is up to one dispatch tick (unsuspend does not kick, `user_suspend.go:19-22`). Live numbers from the cited run: module 3 recorded a suspension 687 ms after the platform's expiry write, held 3 in-flight sends to completion, claimed nothing after the call returned.

**`internal/m3admin` (platform, `m3admin.go`) covers exactly Domain B suspension:** `SetSuspended` (200 confirmed only when the body agrees; 422 only with the `suspended:conflict` detail; the unsuspend 422 is confirmed by an existence probe `GET .../config`, `:280-319`), `Suspension` (strict five-key read, `:353-409`), a hardened transport (no redirects, no proxy, 1 MiB body cap, secret never in an error, `:173-217, 452-483`), retryable-versus-definitive classification, and a closed-code allowlist. **It covers nothing of Domain A**: no quote, run, report, describe, accounts, health. **What an adapter adds:** a second client with the service token and the same transport discipline; per-route timeouts (section 9.1); envelope parsing for the Domain A vocabulary (note `m3admin.knownCodes` at `:524-529` omits `paused`, which `run` emits); a retryable class for 5xx/network and a definitive class for 4xx; request builders with `user_id` in body or query; response decoders that tolerate omitted-versus-null keys; the pagination walker; and a config variable for the Domain A token (none exists, section 0.5).

---

### 9.1 Adapter sketch and client timeouts (a recommendation derived from the code, not a measurement)
One method per route, in two groups. Core (shared shape with M2): `Quote(user, n)`, `Run(user, jobItem)` (200 and 202 both mean accepted), `ReportJob(user, jobID|jobItemID)`, `ReportCampaign(user, campaign)` that **walks every page and returns the union** (stop on `next_cursor:null`), `Describe`, `Healthz`, `Readyz`. M3-only: `Accounts{List, Create, Test, BulkAdd, Get, Update, Delete}` first, the Tier 2 verbs later, and the existing `m3admin` for suspension. Decoders must accept omitted and null for every optional key and must not require keys the other module lacks (`reason`, `smtp_response`).

Timeouts the module's own bounds imply: every DB step is capped at 10 s server side (`dbCtx`, `server.go:182-184`), so 15 s (the harness client's value, `PH/m3_live_helpers_test.go:75`) is right for quote, run, report, lists, get, delete, lifecycle; account create and the test button run the From-gate (its own budget up to 15 s with the gate on) and a worker verify (`smtp_operation_timeout_sec` 30 s, capped at 10 s by the request budget, section 12.7), so allow about 40 s; bulk-add is N rows times (2 s pause plus up to that again) so size the timeout as N times 30 s and treat a timeout as "outcome per row unknown, reconcile by listing accounts". A retried `run` is always safe; a retried create/bulk is not (it is a 409), so reconcile first.

## 10. Oddities an adapter must special-case (consolidated, each with its source)

1. No cancel route; no `cancelled` state (section 0.1).
2. `run` 409 has four indistinguishable causes; `run`'s suspended message differs from the four account routes' fixed sentence (`store.go:75`, `suspension_gate.go:81-83`).
3. Quote ignores suspension, the `dispatch_mode` rung (only `dispatch_enabled`), and `max_campaign_size` (computed at `quote_feasibility.go:93-96, 118`, never used): a 60,000-recipient quote is `feasible:true` and the 50,001st `run` is 409 `max_campaign_size_reached`.
4. `run` reports a malformed `job_item_id` as `required` (`handlers.go:137-141`); every other UUID reports `malformed`.
5. Single-job report omits unset keys; campaign items carry explicit `null`; timestamp precision differs between them (section 2).
6. Campaign report: foreign/unknown campaign is 200 empty; `total` only counts accepted runs; four non-snapshot reads; suspension invisible.
7. Two pagination dialects (`page_size` / `limit`, `items.results` / `items`); junk cursor may 500; `describe.schemas` inaccurate (section 6).
8. Account create is not idempotent and may return `verification.state:"unverified"` (and, read not run, a 500 with the row created: section 12.7).
9. PATCH of credentials leaves verification stale (section 8.4).
10. Test-button counters are shared with create/bulk; 429 has no `Retry-After`.
11. Bulk-add: synchronous, 2 s per row, raw internal reasons possible in `code`, `verification` is a string, a disconnect leaves a half-done batch.
12. `verify_connection_lost` is what "no worker" looks like (docs say otherwise).
13. Notifications are read-only; no clear/ack route.
14. test-send writes a platform `public.job_items` row with `campaign_id = user_id`, consumes a daily send, and blocks `run` for that user while in flight (section 3.3).
15. A failed/bounced item cannot be re-run under its `job_item_id`; a user whose only accounts are unverified/unaligned/quarantined/disabled sees READY items turn `failed`, a user with no accounts waits forever, and expiry is `shadow` (nothing times out until the owner promotes it). At the seeded defaults `autoquarantine_mode`, `autopause_mode` and `campaign_expiry_mode` are all `shadow` (`M3D/0053`), so an account whose password stops working keeps requeueing its items as `queued` with `error_class:"auth_failure"` indefinitely instead of being quarantined: the platform sees "not finished", never "failed", until the owner promotes the modes (M3's go-live checklist section 14).
16. Unsuspend of an unknown user is the same 422 as a not-suspended user (handled by `m3admin`'s probe); suspend of an unknown user is 404.
17. No correlation id is accepted; `request_id` exists on errors only.
18. M3 accepts any IP as `smtp_host`; the worker then skips certificate validation for private ranges (`M3W/smtp.js:156-165`). The harness depends on this (sink by private IP).
19. M3 sends `rendered_body` as a single `text/html` part (`M3W/assemble.js:36`); there is no `text/plain` alternative to carry the platform's planned auto-generated plain-text part (a Phase 5 matter, noted here because the job item has no field for it).
20. `readyz` is green with no worker; `describe` needs the token while `healthz`/`readyz` do not.
21. Entitlement is not checked by M3: a user that exists and was never suspended can send (section 0, last paragraph).
22. Auth cost: bcrypt per request per hash; 50,000 items is 50,000 sequential calls (about 40 min at the measured 47 ms) plus polling.

## 11. Module 2 versus module 3, for "one adapter body"

### 11.1 Identical (one decoder/transport serves both)
Bearer service token (env `PLATFORM_TOKEN_HASHES`, bcrypt, `M2C/config/config.go:97-99`); the envelope and `request_id` shape (M2 emits 7 codes, M3 9: M2 lacks `unprocessable`, `rate_limited`); `GET /healthz`, `GET /readyz` (`checks.db`, `checks.singleton_lock`); `GET /v1/describe` core keys (`module`, `api_version:"v1"`, `capabilities`, `pricing`, `vocabularies`, `error_codes`); `POST /v1/quote` accepting `{recipient_count, user_id}`; `POST /v1/run` body, 202/200 bodies, 404 with `details`, 409 `user_campaign_active`, 503 `paused`; `GET /v1/report` selectors (`job_id`, `job_item_id`, `campaign_id`, `user_id` required), the campaign totals keys (`total, sent, bounced, failed, in_flight, pending, waiting_for_capacity`), `progress`, `items.{results,next_cursor,page_size}`, `page_size` 100/200, cursor `base64(created_at|id)`, the fixed exact-once anchor, and the empty-report answer for a foreign campaign; the same job state spellings for the six shared states; cross-tenant 404 `job_not_found`; `dispatch_paused`; `run` idempotent by `job_item_id`.

### 11.2 Different
| | M2 | M3 |
|---|---|---|
| Routes | quote, run, report, describe, **cancel** | quote, run, report, describe, **18 `/v1/send-byo/*` extensions**, no cancel |
| States | adds `cancelled` (terminal, in `progress.terminal`) | six states only |
| Single-job report | `reason` and `error_class` keys **always present**, null when none (`M2C/platformapi/handlers.go:306-313`); reason vocabulary in `item_reason.go` (pool_exhausted, attempts_exhausted, bounce_permanent, cancelled, dispatch_paused, waiting_for_capacity, no_eligible_account, class names) | **no `reason`**; omit-until-set `smtp_response`, `error_class`, `failed_at`, `account_id`, `account_username` |
| Campaign items | `reason`, `error_class` | `smtp_response`, `error_class`, `failed_at`, `account_id`, `account_username`; plus `totals_by_account[]` |
| Totals | adds `cancelled` | none |
| Quote | `cost = n * unit_price` (+ `tier`, `unit_price_credits`); `user_id` tolerated | feasibility block + inert cost fields; `user_id` **required** |
| `run` 409 causes | `user_campaign_active` (M2 `store.go:80`) | + suspended, `max_campaign_size_reached`, `max_queued_jobs_reached` |
| `failed` meaning | pool exhausted or attempts exhausted (`terminal_reason`) | account exhaustion or enforced expiry |
| Sending identity | operator-owned pools, no user accounts | user-owned SMTP accounts; From-gate; verification |
| Metering | per recipient (charge table m2_accepted) | none (M3-01), entitlement gated |

So the **core four routes can share one client body** (decode the union of keys, treat M2's `reason` and M3's `smtp_response` as optional), while accounts, suspension and the 409 interpretation are M3-only, and `cancel` is M2-only.

---

## 12. What the harness already provides for M3, and what an adapter test still needs

### 12.1 Exists (all `PH/`)
- Stack (`docker-compose.yml`): `migrate-m3` (113), `m3` Control (174-203; `PLATFORM_TOKEN_HASHES`, `ADMIN_API_KEY_HASHES`), `m3-worker` and `-2`, `-3` (206-284; **`restart: "no"`**), `smtp-sink` = Mailpit v1.33.1 (228-240; accepts any credential), `smtp-ctl` = our controllable sink (293-318; `tests/harness/sink`), `platform` and `platform-2` (320+; `PLATFORM_M3_URL`, `PLATFORM_M3_ADMIN_TOKEN`, loops at 2 s). The test container gets `HARNESS_M3_URL, _M3_TOKEN, _M3_ADMIN_KEY, _SINK_URL, _CTL_URL, _M3_WORKER_IDS, _SINK_PASSWORD` and a superuser database URL (`docker-compose.yml:386-400`).
- `run.sh` stage 5b (199-221): switches the From-gate off, seeds three `send_byo.boxes` rows with bcrypt hashes BEFORE the workers start (a rejected worker exits), waits for three heartbeats (database clock), waits for both sinks. Stage 9b (283-304): stops `m3-worker` and `m3`, lets the platform retry, restarts both.
- Helpers (package `harness`, tag `harness`, names `m3l*`/`m3h*`): `m3lEnv.do/expect/snip` (credentials scrubbed, no proxy, 3 tries for GET), `m3lWait`, `m3lTenant`, `m3lRun.report/parseReport`, `sinkView` (exact-address counting, duplicate detection), the sink control calls (`setMode`, `inflight`, `rcptCounts`), `requireColumns`.
- Two live tests: `TestM3Live_ExpiredEntitlementHaltsSendingAndReenableResumes` (1 lane, 2 s delay, 24+20 items, the platform's real loops) and `TestM3Live_SeveralLanesAndAHostileSink` (3 workers, 3 lanes, 550/451/delayed sink, halt and resume, charge decisions). Both drive raw HTTP, not an adapter, and compare against the database and the sink.

### 12.2 From-gate OFF: what it means
`run.sh:201` writes `{"v":"off"}` to `send_byo.config` `from_gate_mode` as superuser before any account exists. `Evaluate` then skips DNS and returns allow/`aligned` with `detail.from_gate_mode:"off"` (`M3C/fromgate/evaluator.go:90-95`), so **any** `from_address` (`sender-u1-<run>@example.test`) creates an `aligned` account with no lookups and no DNS in the network. Consequences: (1) the WARN/BLOCK/indeterminate paths (422 `from_alignment_warning_unacknowledged`, `from_dns_unavailable`, `from_alignment.state` `unaligned_warned`/`blocked`, quote `from_gate_blocked`) are **unreachable** in the harness (M3's own harness has a DNS fixture; the platform's has none), so an adapter test cannot prove those mappings without either a unit test over recorded envelopes or direct SQL (`UPDATE send_byo.accounts SET from_verification_state = ...` as superuser to stage `blocked`); (2) the 10 s create budget has no DNS in it, so the slow-create risk (below) is not exercised; (3) the key is read fresh per create (`platformapi/from_gate.go:53-57`), but the cache TTLs are read once at start.

### 12.3 Creating a user, an account and a job through the real routes
1. **User: no module route.** Superuser SQL, as both live tests do: `INSERT INTO auth.users (id,email)` then `INSERT INTO public.users (id, auth_user_id, email)` (`PH/m3_live_test.go:150-153`). Use `<name>-<runid>@example.test`.
2. **Pacing (admin key):** `PUT /v1/admin/users/{id}/config` `{"concurrency_per_user":N,"send_delay_min_sec":0,"send_delay_max_sec":0}` (200; the live tests verify it in `send_byo.user_config_overrides`).
3. **Account (service token):** `POST /v1/send-byo/accounts` `{"user_id","smtp_host":"smtp-sink","smtp_port":1025,"smtp_username":<from>,"smtp_secret":<HARNESS_SINK_PASSWORD>,"from_address":<from>,"from_name":"Harness"}` returns 201; assert `verification.state=="verified"` and `from_alignment.state=="aligned"` (an unverified account never errors, it just never sends). For the controllable sink use its **private IP** as `smtp_host` (resolved from `smtp-ctl`, asserted private): the worker validates the certificate for any other name (`PH/m3_hostile_test.go:141-153`).
4. **Job items: no module route.** `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)` as superuser, one campaign per user, **distinct recipients carrying the run id** (the Mailpit sink is cumulative and its search is fuzzy: match exact addresses).
5. **Run/poll:** `POST /v1/run` per item, then `GET /v1/report?campaign_id=&user_id=&page_size=200` until `progress.is_complete`, then walk every page.

### 12.4 How long outcomes take, and the dials
**Measured, one hosted run (cited at the top; not a benchmark):** `run` about 47 ms per item; 1 lane and a 2 s delay gave about one send per 2-3 s (24 items about a minute, test comment `m3_live_test.go:184`); 3 lanes with 0-1 s delay and a 3 s hold per send: 36 items completed within the 240 s wait and every lane/worker took 12; a suspension was recorded 1.5 ms before its call returned, with 3 held sends finishing and 0 claims after it; after the unsuspend all 27 remaining items were claimed. The retry ladder is the long pole: a 451 item needs **at least 30 s then 120 s** (a third attempt about 150 s after the first) at the seeded ladder. **Estimates, not measured:** a single `run` to `sent` against Mailpit with default dials is about 1-2 s (kick, one tick, a worker, under 100 ms of SMTP); defaults are 3 lanes, 3-7 s between a lane's sends, so a 100-item campaign is about 3 minutes.

Dials, all read live except noted (`PUT /v1/admin/config/{key}` `{"value":...}`, or per-user via section 9): `dispatch_tick_interval_ms` 1000; `send_delay_min/max_sec` 3/7 and `concurrency_per_user_default` 3 (per-user override preferred); `retry_backoff_schedule` `{"ladder_seconds":[30,120,600],"max_seconds":600}` (positive ints, so 1 s is the floor); `bulk_add_verify_interval_ms` 2000 (0 disables pacing, `account_bulk_add.go:143`); `account_test_rate_limit_per_day` 5 and `test_button_rate_limit_per_user_per_day` 20; `account_count_cap_per_user` 20; `max_campaign_size` 50000, `max_queued_jobs` 100000, `campaign_expiry_sec` 604800 with `campaign_expiry_mode` `shadow`; `dispatch_mode` `serving|accept_and_defer|refuse_new|stopped`; `smtp_operation_timeout_sec` 30, `job_max_processing_time_sec` 120, `reclaim_tick_interval_ms` 15000, `startup_reconnect_window_sec` 120, `box_outcome_buffer_ttl_sec` 900. Global writes are shared by every test in the run: restore in cleanup. The pacer's lane timers are in memory: do not restart `m3` mid-test expecting pacing continuity.

### 12.5 What an adapter test still needs
| Scenario | What it needs beyond today |
|---|---|
| run, poll, report every page, settle | the adapter itself (a client package) driven by a test that compares its decisions with `send_byo.jobs` and the sink (ground truth), a page size small enough to force several pages (for example 7 over 36 items), `item set == inserted set` and each recipient exactly once; a paired control that breaks the pager (as `pagination_exact_test.go` does) |
| single-job and by-`job_item_id` report | one real item through both selectors; the omit-versus-null shapes decoded both ways |
| cross-tenant refusal | users A and B, A's job item and campaign and account, every row of the 3.4 table, each with a paired owner-succeeds control; expected statuses differ per route (404 versus 200 empty) |
| outage mid-job | **cannot be done from a Go test in the container** (no docker control; compose must not mount the socket). Options: a `run.sh` stage like 9b that stops `m3` after N sends, runs the adapter test binary in two phases, restarts (workers are `restart: "no"` and exit after about 30 s without Control unless they hold unacked outcomes, `M3W/index.js:66-104`, so restart `m3-worker*` too); or a small fault proxy service the test toggles (tests the adapter only, M3 keeps sending). Assert: errors are retryable, no state is invented, after recovery the same items finish, sink exactly once |
| resume after adapter crash | in-process: build a second adapter instance mid-campaign, re-run every item (all 200), continue polling; cheap and needs no docker |
| `paused`, 409 families | `PUT /v1/admin/config/dispatch_mode` to `refuse_new` (503 `paused`), suspend through the admin route (the generic-message 409 on `run`, the fixed sentence on create/test/bulk/test-send), per-cause 409s via small per-user caps (`max_campaign_size`=2 etc.) |
| account lifecycle | create, list, test (and the 5/20 rate limit), bulk-add with a mixed batch and `bulk_add_verify_interval_ms` set low, PATCH then test, delete (204 then 404) |

### 12.6 Risks
- **One active campaign per user** (409) and **test-send blocks `run`**: one user per scenario, no test-send in a campaign scenario.
- **Entitlement loops**: the service's reconcile loop re-sends the wanted state of every user it knows (`m3_hostile_test.go:34-36`): never enable an entitlement for an adapter-test user unless that is the test.
- **Shared, cumulative sink; shared controllable-sink mode.** Reset and restore it; Go tests run sequentially but the sink mode is global. Recipients are prefixed per run.
- **Workers `restart: "no"`** and **one job per worker at a time**: with `concurrency_per_user` above the worker count, extra claims requeue with churn; the stack has three.
- **Test-button budget**: bulk and create burn the 20 per day per user; use a fresh user per scenario.
- **Time**: M3 tests are minutes, not seconds; the 451 path alone is at least 150 s unless the ladder is shortened. The harness job already takes about 11 minutes end to end (run log).
- **CPU**: bcrypt per call; keep the poll at 1-2 s per campaign, not per item.
- **Slow create** (section 12.7, item 1).

### 12.7 Unproven (a runner run would settle each)
1. **Create under a slow verification.** `handleAccountCreate` creates `ctx` with a 10 s `dbCtx` first (`account_create.go:69`), then spends time in the From-gate (its own context, up to 15 s with the gate on) and the create transaction, and only then calls the worker verify with the same, partly spent `ctx` (`:114`; `M3C/dispatch/verify.go` returns on `ctx.Done()` while waiting for the worker, about `:192-203`), and re-reads the account with it (`:116`). If the total passes 10 s the verify is abandoned, the state stays `unverified`, and the re-read can fail with a 500 `account_reread_error` although the row exists. The test button and each bulk row have the same shape (`account_test_button.go:42`, `account_bulk_add.go:186`). Read, not run; the current sink has no mode that delays AUTH (only DATA), so proving it needs a new sink control.
2. Junk-but-base64 cursor gives a 500 (section 6).
3. Whether `bulk_add_verify_interval_ms=0` is accepted by the config PUT (no lower bound was found in `config_validate.go`; the handler treats 0 as unpaced).
4. The constructed JSON samples above versus a real capture.
5. End-to-end timings of the Phase 3 scenarios.

---

## 13. Decisions and notices for the lead (not mine to settle)
- **Cancel for M3** (section 0.1): additive module route (a mirror of M2's `/v1/cancel`) versus a documented platform-side rule; today the only terminal stop is making READY items `failed` by disabling accounts, which is a side effect on the user's accounts.
- **Entitlement gate on the platform side**: M3 allows a never-suspended user; the adapter or job engine must refuse non-entitled users, or the platform must suspend new users by default.
- **Which 409 the adapter tells the user**: pre-check from platform state, since the module will not say.
- **Production `smtp_host` policy** (private addresses) and **the `max_campaign_size` quote gap**: record as findings; M3 is not modified from here.
- Noticed, not fixed: the harness comments cite `smtp.js:136-145` for the private-host rule; at c70f4b6 it is `M3W/smtp.js:156-165, 202`.
