# Phase 3 research: module 1 (verify): the API an adapter calls, and what it takes to run M1's REAL verifier node in the harness

Read-only research for the Phase 3 spec (Part B: the M1 adapter and the harness additions). Written from module CODE; docs were read afterwards only to cross-check and are named where they disagree with the code.

**Pins and abbreviations.** M1 = coldmail-verify at 4adc316 (the pin in `tools/modules.lock`). `CP/` = `coldmail-verify/control-plane/internal/`; `MAIN` = `coldmail-verify/control-plane/cmd/control-plane/main.go`; `NODE/` = `coldmail-verify/verifier-node/truemail-rack/`; `GEM` = the truemail 3.3.1 sources (not vendored in M1: read from the gem's public tag, file names given, no line numbers); `PH/` = `tests/harness/` of this repository.

**What is proven and what is not.** Nothing was run here (no Docker, no Postgres). JSON bodies are CONSTRUCTED from the handler and serializer code and the module's own docs; they are not captured responses. Every statement tagged [U#] is an unproven assumption, collected with a cheap way for a runner run to settle it in section 6. A claim about "what the code does" cites file:line; a claim about Docker, Ruby or the runner that I could only reason about is tagged [U#].

---

## 0. The five things the lead must know before writing the Phase 3 spec

1. **Today no M1 verdict that needs a node can occur in the harness, and a naive node would still produce none: the destination guard (ADR-0021) refuses every private address, and a Docker bridge hands out private addresses.** Every fixture domain would answer `risky/non_public_mx`. The only test-only override (`PROBE_ALLOW_NON_PUBLIC=1`, `NODE/app/truemail_server/wrapper/probe_guard.rb:86-88`) turns the node's audit RED by design (`wrapper/app.rb:128-130`), and a red node is never routed (`CP/audit/audit.go:130-132`). So the guard must stay ON and the fixtures must sit on addresses the guard calls public: a SECOND internal network with a public-looking /24 (section 3.1). M1's own CI never runs control plane, node and an SMTP peer together (section 2.1): this would be the first end-to-end run, so expect surprises and stage the work.
2. **The audit is the gate, and the audit needs three fixtures that have nothing to do with mail: an HTTPS "what is my IP" answer, an A record and a PTR record.** The gem's `host_audit` asks `https://api.ipify.org` for the node's own address (GEM `audit/ip.rb`), then requires the verifier domain's A record to equal it (GEM `audit/dns.rb`) and a PTR for it to name the verifier domain (GEM `audit/ptr.rb`); with no internet the IP step fails and the audit is red for ever. The fixture must answer the lookup over TLS with a certificate the node trusts (`SSL_CERT_FILE`) and the node must resolve it through a clean resolv.conf (section 2.4, 2.5).
3. **Nothing in M1's running code promotes a node `provisioning` to `active`, and nothing sets `port25_open`: both are SQL the harness must run, and a control-plane restart undoes them.** The watcher's decision function has no rule for a provisioning node (`CP/health/decide.go:204-281`); the P3 promotion (`audit.NextState`) is only called from tests (`CP/audit/scheduler.go:90`, `AuditAll` has no non-test caller); `UpsertNodeState` resets `status='provisioning', port25_open=false` at every boot (`CP/store/postgres.go:520-532`, `MAIN:501-508`). Module 1's docs say the node "promotes" (`Docs/install-and-deploy.md:249`, `Docs/go-live-checklist.md` G1.9): that is stale against the code [U6]. The harness step is `UPDATE verify.nodes SET status='active', port25_open=true WHERE id=...` as the superuser, after the first green audit row exists.
4. **A node that is wired but not eligible makes a bulk job HANG, not settle.** `no_eligible_node`, `audit_red`, `warmup_cap` and `rate_limited` are requeued as capacity waits (30 s each) until the item is 6 hours old (`CP/queue/drainer.go:301-316`, defaults `config.go:636-646`). Set `TM_QUEUE_MAX_RETRY_AGE_SECONDS` small in the harness, and read the SMTP-wired boot (`/v1/config` fetched once at boot, `MAIN:471-477`) as a one-shot: a node that is down at that moment leaves the process in `smtp_unwired` for its whole life.
5. **The API is small and its sharp edges are known**: six routes, no describe/admin/config surface, caller keys exist only as SQL rows, a job is owned by the `user_id` of the key that submitted it (NULL = nobody can read it), a non-UUID job id is a 500 not a 404, `POST` bulk is not idempotent, and results of a running job are a moving target (section 1.8). The platform already has the right client for the three bulk routes (`internal/m1results`); the adapter is thin and its open decisions are about keys (one service key or one per user), health (readyz says nothing about SMTP wiring) and charging of fixture-only kinds.

---

# PART 1. The API an adapter must call

## 1.1 The surface (code is the source: `CP/api/api.go:226-237`)

Exactly six routes. There is **no describe, no vocabulary, no policies, no admin, no config, no metrics route** (api-contract "Not served", and `Routes()` is what `Handler()` serves). The platform's describe check cannot exist for M1; vocabulary, policy and dials are code or environment, read at boot only (no reload).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/verify` | per-caller key, or the legacy static token (`TM_FRONTDOOR_TOKEN`) | one address, synchronous |
| POST | `/v1/verify/bulk` | per-caller key ONLY (the static token is 401 here) | submit a job, 202 |
| GET | `/v1/jobs/{id}` | per-caller key only | counts and status |
| GET | `/v1/jobs/{id}/results` | per-caller key only | one result per input row, cursor-paged |
| GET | `/healthz` | none | liveness (`{"status":"ok","at":"<rfc3339>"}`, 200 even with every dependency down) |
| GET | `/readyz` | none | readiness, 200 or 503 |

Wrong verb on a real path is `405 method_not_allowed` with `Allow` (GET implies HEAD); an unknown path is `404 not_found`, both in the envelope (`api.go:256-295`). `OPTIONS` is 405.

## 1.2 Auth, callers, tenancy

- `Authorization: Bearer <key>` is mandatory; the scheme is case-insensitive; a bare token is 401 (`CP/api/helpers.go:34-41`). The server hashes the presented key with sha256 and looks it up in `verify.api_callers` by that digest, inside its validity window (`CP/store/postgres.go:1345-1363`, `api.go:810-845`).
- **Callers are created only by SQL.** `api_callers` is SELECT-only for the runtime role and INSERT/UPDATE for the admin role (migration 0010); there is no issuance route anywhere. The harness seeds one with `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id)` (`PH/run.sh:167`). Columns: `caller_id` (opaque principal), `key_sha256` (bytea, primary key), `status`, `not_before`, `not_after` (NULL = no expiry; rotation = two rows, overlapping windows), `label`, `user_id` (uuid, added by 0013).
- **"Tenant" in M1 = `api_callers.user_id`**, a bare uuid with no foreign key (M1 has its own database; `migrations/0013_job_owner.sql`). At submit the job is stamped `owner_user_id = user_id of the AUTHENTICATING KEY` (`api.go:477-488`); the body can carry no owner (unknown body fields are silently ignored: a `user_id` in the body does nothing). Every job read predicates on it in SQL (`postgres.go:1053-1085`, `1089-1140`, `1153-1180`).
- **A key whose `user_id` is NULL can submit but its jobs are unreadable by anyone** (NULL never equals anything); the 202 still comes back with a job id. A silent trap for any seeding script.
- `caller_ref` (body, optional, opaque) is echoed on the status route only; it is NOT an owner and is not returned in results. There is no way to list jobs or find one by `caller_ref`.
- **Cross-tenant read** = absent job: `404`, body `{"error":{"code":"not_found","message":"The requested resource was not found.","request_id":"<uuidv7>"}}`, identical except `request_id` (`api.go:545-552`, `607-612`; proven by `CP/api/pg_tenancy_test.go`). Tenancy is checked BEFORE the cursor, so a stranger never learns the cursor was bad.
- Open decision for the spec: ONE service key with a platform service-principal `user_id` (M1 then isolates nothing between platform users; the platform does) versus one key per platform user (SQL on every user creation, no API to do it). The existing harness and `m1results` use one key.

## 1.3 `POST /v1/verify` (sync) and `POST /v1/verify/bulk` (bulk)

Sync: body `{"email": "<string, required>", "policy": "<optional>"}`, limit 64 KiB (`api.go:333`); an undecodable body, an empty email or a body over the limit are all `400 validation_failed` with `details:[{"field":"email","issue":"required"}]` (misleading for the oversize case). Rung 4 (either sub-mode) answers `503 paused`. No orchestrator wired: `503 unavailable`. The call is synchronous through the whole lifecycle (cache, cheap layers, SMTP stage): with a node it holds the HTTP request for the DNS lookups plus the node call (the control plane's node deadline is 15 s unless `TM_SMTP_CLIENT_DEADLINE_MS` is set, `node/http_client.go:31-35`); the platform's HTTP client needs a timeout above that (m1results uses 30 s).

Bulk body (`api.go:385-390`, `bulk_rows.go`): EXACTLY ONE of `addresses` (array of strings) or `items` (array of `{"item_id","address"}`), plus optional `policy`, `caller_ref`.

| Field / limit | Rule | Failure |
|---|---|---|
| both `addresses` and `items` given | refused | 400, details `addresses: conflict`, `items: conflict` |
| neither, or an empty array | refused | 400, `required` on the field(s) |
| rows | at most 100,000 by default (`TM_QUEUE_BULK_MAX_ADDRESSES` dial) | 413 `payload_too_large`, details `[{"field":"<addresses or items>","issue":"out_of_range"}]` |
| body | 64 MiB (`maxBulkBodyBytes`) | an oversize body is a decode error: 400 `validation_failed`, details `body: malformed` (NOT 413) |
| non-JSON, wrong element types | refused | 400, details `body: malformed` |
| `policy` | "" or `cold-email` only (the one shipped policy; no version selector) | 400 `validation_failed`, details `policy: not_found` (a 400, not a 404) |
| `items[i].item_id` | mandatory in `items`; non-blank; at most 200 BYTES; no control characters; unique, case-sensitive | 400, details `items[N].item_id` with `required`/`out_of_range`/`malformed`/`duplicate`, lowest ordinal first, at most 100 entries |

A bad ADDRESS is never a bad request: it becomes a malformed row (`hash.Normalize` refuses it: not exactly one `@`, blank local part or domain, a domain IDNA cannot encode; `CP/hash/hash.go:75-110`). Not-malformed but still syntactically bad addresses (no dot in the domain, local part over 64, domain over 255) are accepted as work items and settle as `invalid/invalid_syntax` from the syntax layer (`CP/layers/cheap_layers.go:22-33`): the existing harness fixture relies on this (`dup@nodot`).

**202 body** (`api.go:512-521`):
```json
{"job_id":"<uuidv7>","status":"accepted",
 "counts":{"total":4,"enqueued":2,"duplicate":1,"malformed":1},
 "malformed_rows":[{"ordinal":3,"item_id":"row-3"}],"malformed_rows_truncated":false,"processing_paused":false}
```
`total = enqueued + duplicate + malformed` (rows, not addresses). `malformed_rows` lists at most 1000 (`item_id` omitted for the `addresses` shape), always present (`[]`); `counts.malformed` is exact. Keep this response: `duplicate` and the malformed list appear nowhere else. Rung 4 accept-and-defer still answers 202 with `processing_paused:true` and nothing drains; reject answers `503 paused`.

**503 `unavailable` when `TM_QUEUE_ADDR_KEY` is missing**: the queue refuses to start, `WithBulk` is never called, the bulk and job routes answer 503 `unavailable` to an AUTHENTICATED caller (auth first: a bad key is still 401) while `/v1/verify` keeps working (`MAIN:213-218`, `api.go:429-432`, `535-538`). The same condition makes `/readyz` 503 (below).

## 1.4 Status and results

`GET /v1/jobs/{id}` (`api.go:575-584`):
```json
{"job_id":"<uuidv7>","status":"complete","complete":true,"caller_ref":"platform-job-42",
 "counts":{"total":4,"settled":4,"pending":0},"processing_paused":false}
```
- `complete` is decided from the work items (NOT EXISTS a non-terminal one), never from the counts. `settled` counts ROWS whose work item is `settled`/`dead` plus all malformed rows; duplicates settle with their first row; `pending = total - settled`.
- `status` is `running` until `complete`, then `complete`. The database vocabulary also names `submitted` and `complete_with_unknowns`, but the code only ever writes `running` (`CP/queue/enqueue.go:240`) and derives `complete` (`api.go:558-561`): branch on the boolean `complete`, not on `status`. A job of only malformed rows is complete at once.
- Jobs submitted before migration 0018 keep a `total` that can exceed their rows; not a harness concern.

`GET /v1/jobs/{id}/results?cursor=&limit=` (`api.go:590-677`, `postgres.go:1153-1215`):
```json
{"job_id":"<uuidv7>","results":[ <element>, ... ],"next_cursor":"4"}
```
- Ordered by `ordinal` (the 0-based position in the submitted list, malformed rows counted). One element per input row THAT HAS AN OUTCOME: a settled/dead work item (duplicates share it) or a malformed row (served from its own LOG row, available from the moment of the 202).
- `limit`: 1..1000, default 100; anything else (0, negative, non-numeric, over 1000) silently becomes 100. No error.
- `cursor`: the decimal ordinal of the LAST row returned (`store.go:324-338`); "" = first page; `next_cursor` is "" when nothing follows AS THE LISTING STANDS NOW. A cursor that is not a canonical non-negative decimal is `400 validation_failed`, details `[{"field":"cursor","issue":"malformed"}]`, checked after the tenancy 404. A well-formed cursor of another job simply means "after that ordinal" (it is not job-bound).
- **While the job runs** the listing is a moving target: a row that settles later at an ordinal below an already-issued cursor is never revisited and `next_cursor:""` does not mean done. Walk only after `complete:true` (`m1results.Drain` polls and re-reads for this reason).
- An element whose stored row fails `Valid()` fails the WHOLE page: `500 contract_violation` (no partial page), after tenancy.

**Element** = the verdict object below, key for key, plus `ordinal` (always, 0 is real), `item_id` (only when the row had one) and `duplicate_of` (only on a row repeating an earlier address: the ordinal of the FIRST such row, 0 is real; absent on first rows and on malformed rows).

## 1.5 The verdict object (`CP/verdict/verdict.go:17-43`, `assemble.go:66-107`)

Sync example for a probed, accepted address (constructed; values illustrative):
```json
{"api_version":"v1","status":"valid","reason":"accepted","confidence":90,"recommendation":"send",
 "email":"Ada@acme.fixture.test","domain":"acme.fixture.test",
 "flags":{"from_cache":false,"has_typo":false,"is_disposable":false,"is_free_provider":false,"is_role":false,"is_spam_trap":false},
 "signals":{"brain_mx_provider":"self","catchall_conclusion":"not_catchall","engine_mx_hosts":["<fixture smtp ip>"],"mx_found":true,"mx_provider":"self","null_mx":false,"syntax_valid":true},
 "meta":{"request_id":"<uuidv7>","checked_at":"<rfc3339 utc>","latency_ms":412,"policy":"cold-email","policy_version":1,
         "verifier_ip":"<node egress ip>","node_id":"<TM_NODE_ID>","engine_version":null,"config_fingerprint":"cfg_sha256:<16 hex>","from_cache":false},
 "raw":null}
```
The same row as a bulk element (`verdictFromRow`, `api.go:865-888`; `bulk-item-identity.md` section 5):
```json
{"api_version":"v1","status":"valid","reason":"accepted","confidence":90,"recommendation":"send","email":"","domain":"acme.fixture.test",
 "flags":{"from_cache":false,"...":false},"signals":{"...":"as stored"},
 "meta":{"request_id":"","checked_at":"","latency_ms":0,"policy":"cold-email","policy_version":1,"verifier_ip":null,"node_id":null,"engine_version":null,"config_fingerprint":null,"from_cache":false},
 "raw":null,"item_id":"row-2","ordinal":2}
```
Field rules: `status` valid|invalid|risky|unknown; `reason` from the frozen enum (`CP/vocab/vocab.go:43-81`, 27 values, additive-only); `confidence` int 0..100 from policy baselines (`CP/policy/coldemail.go:78-100`); `recommendation` send|review|skip with `invalid=>skip`, `valid=>send|review`, `risky|unknown=>review|skip` (`vocab.go:114-125`); `email` echoes the RAW input on sync and is ALWAYS "" in bulk (Door 3); `domain` normalized; `raw` is always null; `meta.engine_version` is always null (the orchestrator never fills it, `orchestrator.go:365-374`); in a bulk element `meta` carries only policy, policy_version and a `from_cache` that is ALWAYS false: use **`flags.from_cache`**, and read node/verifier provenance from the log, not from the result.

`flags` (open bag, bool): `from_cache` always; `is_role`, `is_disposable`, `is_free_provider`, `is_spam_trap`, `has_typo` for the layers that ran before a stopping gate (so a verdict stopped at `no_mx` carries fewer).

`signals` (open bag; consumers must ignore unknown keys), keys the platform acts on or a test can assert:

| Key | When | Meaning |
|---|---|---|
| `failure_origin` | EVERY `unknown`; also a `risky` that is OUR degraded fallback (skip token below) | `own` / `remote` / `undetermined`; the platform bills `remote` only (`CP/failureorigin/failureorigin.go:90-168`; charge rows M1-18..M1-23) |
| `smtp_skipped_reason` | `risky/plausible` where SMTP did not run; and `provider_mx_routed` on `risky/provider_no_smtp` | `no_eligible_node`, `audit_red`, `warmup_cap`, `rate_limited`, `smtp_unwired` (all OUR failure, free, never cached) or `provider_mx_routed` (settled, cacheable) (`CP/vocab/signals.go:47-56`) |
| `error_capped` | `unknown/unknown_error` | the item failed to compute 5 times (`TM_QUEUE_ERROR_MAX_ATTEMPTS`); origin `own` |
| `smtp_code`, `smtp_enhanced_code` | SMTP ran and a code was parsed | absent on a clean 250 (the node reports `smtp_debug:null`, `CP/smtp/transcript.go:121-129`) |
| `catchall_conclusion` | accepted target | `catchall`, `not_catchall`, `inconclusive`, `not_probed` |
| `greylisted`, `retry_eligible`, `retry_after_hint_s` (300) | greylist/transient unknowns | requeue seeds |
| `probe_refused`, `probe_refused_ranges`, `probe_refused_count` | `risky/non_public_mx` | the guard's facts |
| `mx_found`, `null_mx`, `syntax_valid`, `mx_provider`, `brain_mx_provider`, `engine_mx_hosts`, `dns_divergence`, `suggestion` | cheap layers / SMTP | evidence |

The charge table's inputs are exactly status, reason, `flags.from_cache`, `signals.smtp_skipped_reason` and `signals.failure_origin` (`internal/m1results/row.go:59-104`).

## 1.6 Error envelope (`CP/api/errors.go`)

`{"error":{"code":"<code>","message":"<fixed sentence>","details":[{"field","issue"}],"request_id":"<uuidv7>"}}`; `details` omitted when empty; `message` is a curated constant per code, never interpolated (do not parse it); `request_id` is a fresh UUIDv7 per error (the same id is in the server log line); a contract-violation error reuses the verdict's own request id.

| code | status | when |
|---|---|---|
| `unauthenticated` | 401 | no/invalid key, scheme-less token, expired key window, legacy token on a bulk/job route |
| `validation_failed` | 400 | the cases of 1.3 and a bad cursor; `issue` in `required`, `malformed`, `out_of_range`, `not_found`, `conflict`, `duplicate` |
| `not_found` | 404 | unknown path, absent job, another owner's job |
| `method_not_allowed` | 405 | wrong verb |
| `payload_too_large` | 413 | row cap |
| `unavailable` | 503 | not wired: no DB, no pepper, or no queue key (bulk/job) |
| `paused` | 503 | Rung 4 pulled (`TM_FRONTDOOR_MODE`): retry later unchanged |
| `internal_error` | 500 | a store error, including a NON-UUID job id on a job route |
| `contract_violation` | 500 | a stored/produced verdict fails `Valid()` |

No `rate_limited`, `conflict` or `unprocessable` code exists (M1 has no per-caller rate limiting and no HTTP 429 anywhere); the same five codes as the senders are spelled byte-identically.

## 1.7 Readiness (`api.go:721-774`)

```json
{"at":"<rfc3339>","checks":{"db":true,"pepper":true,"queue":true},"config":"env=development listen=:8080 ladder=cheap-layers-only(Rung3) smtp=disabled fallback=off frontdoor=serving nodes=0 mutators=recommend-only","pepper":"set","ready":true,"status":"ready","supabaseKey":"unset"}
```
200 only if pepper AND a live DB ping (3 s bound, per request) AND (once declared) the queue are all true; else 503 `status:"not_ready"`, plus `"reasons":["queue: TM_QUEUE_ADDR_KEY is not set: the bulk and job routes answer 503 until it is"]` (other reasons: `DR-4 cipher init failed: ...`, `drainer init failed: ...`). `checks.queue` exists only on a process whose boot reached the queue wiring. **Readiness does NOT cover the node or the SMTP stage**: the `config` string prints `smtp=enabled nodes=1` from configuration, whether or not the stage wired. The only observable signals of wiring are the boot log (`verify: orchestrator wired (SMTP-enabled (against node list) ...`, `MAIN:169-170`) and the verdicts themselves.

## 1.8 Oddities an adapter must special-case (numbered; each is one test or one `if`)

1. No describe/policies/vocabulary/admin/config route (1.1). Synthesize M1's "describe" from constants.
2. Keys are SQL-only; NULL `user_id` makes jobs unreadable (1.2). A garbled `Authorization` is 401, never 400.
3. A non-UUID job id on `GET /v1/jobs/{id}` or `/results` is a store error: `500 internal_error`, not 404 (`jobs.id` is uuid, `CP/api/pg_tenancy_test.go:195`). `m1results.checkJobID` does not check shape (`client.go:267-272`): validate UUID shape before calling, or map this 500.
4. `POST /v1/verify/bulk` is not idempotent (no key, FW-10): a retry after a lost response creates a second full job; there is no lookup by `caller_ref`. Record intent before the call and treat a timeout as "orphan job possible".
5. Job `status` is only ever `running`/`complete`; use `complete` (1.4).
6. `counts.total` counts every row; `enqueued` counts distinct addresses; malformed rows are already `settled`; the status route does not repeat `duplicate` or the malformed list.
7. Malformed (normalization fails, listed in `malformed_rows`) is not the same as `invalid_syntax` from the syntax layer (a settled work item); both read `invalid/invalid_syntax`. A repeated malformed address is not a duplicate; a repeated good address is `duplicate_of` the first.
8. Result `email` is "" and bulk `meta` is skeletal (1.5); `flags.from_cache` is the cache signal.
9. A moving listing while running (1.4); `limit` silently falls back to 100; cursors are not job-bound.
10. 503 has two codes with opposite retry semantics: `unavailable` (page someone) and `paused` (wait); both share the status.
11. `/v1/verify` works without `TM_QUEUE_ADDR_KEY`; `/readyz` does not (503). Use `checks` and `reasons`, not the status alone.
12. A wired-but-ineligible node = jobs that wait up to 6 h (section 0.4); an UNWIRED stage settles at once with `smtp_unwired`. Same token family, opposite timing.
13. Per (node x provider bucket) rate limit from the policy: 30 session starts per 60 s and 2 in flight, bucket `other:<domain>` per corporate domain, `google`/`o365`/`yahoo` as classes; over it, rows requeue as `rate_limited` waits (`CP/ratelimit/ratelimit.go:105-142`, `285-297`; policy `coldemail.go:66-68`). 31 rows at ONE domain inside a minute meets it.
14. Caching only if `TM_CACHE_TTL_SECONDS` is set (default unset: no caching, `config.go:510-518`); keyed on (email hash, config fingerprint); only settled verdicts (valid, invalid, risky except `non_public_mx`; never an `unknown`, never a provisional fallback); a hit keeps the ORIGINAL stored signals with `flags.from_cache:true`; nothing is deduplicated across jobs or tenants.
15. The SMTP stage reads node health from the database on every request: `UPDATE verify.nodes` changes eligibility at once, no restart.
16. Unknown body keys are ignored; policy other than `cold-email` is a 400 with `not_found`.
17. A single drainer (`drainer-1`), batch of 10, items worked serially: about one SMTP item per 0.3-1 s (jitter 50-500 ms before each session start, `CP/ratelimit/ratelimit.go:209-220`); cheap items are limited by the 2 s idle poll (`TM_QUEUE_DRAIN_IDLE_POLL_SECONDS`).
18. No timeouts beyond `ReadHeaderTimeout` 5 s on the server (`MAIN:71-75`); a request is as slow as its dependencies.

## 1.9 What `internal/m1results` already covers

Submit (items shape only), Job, Results, Drain (polls to `complete`, re-reads behind the cursor, refuses an incomplete listing), `ToSettleItems` (item_id as `item_ref`, `duplicate_of` becomes platform/duplicate). It does not cover: sync `/v1/verify`, `/healthz`/`/readyz`, job-id shape validation, the status values, `malformed_rows`, or a non-202 `payload_too_large` mapping beyond `StatusError.Code`. The thin adapter is therefore: constants for describe, a readiness mapping that reads `checks`/`reasons`, and the two decisions of 1.2 and 1.8(4).

---

# PART 2. Running M1's REAL verifier node inside the harness

## 2.1 What exists today (code and CI)

- The harness runs M1's CONTROL PLANE only (`PH/docker-compose.yml:128-144`, built by `PH/m1/Dockerfile`, a static `CGO_ENABLED=0` Go binary on alpine), with no `TM_SMTP_MODE`, no node variables, no cache TTL: every residue is `risky/plausible` + `smtp_unwired` (`CP/orchestrator/smtp_stage.go:96-102`). Its DNS is the container's resolv.conf pointing at Docker's embedded resolver on an internal network (`PH/m1_bulk_settle_test.go` header).
- M1 ships NO compose stack for control plane + node. `verifier-node/docker-compose.yml` is the Phase 0 stock node only. Its CI proves the node in pieces: the image builds from its own Dockerfile and resolves truemail 3.3.1 (`.github/workflows/ci.yml:257-262`); the fork's RSpec runs inside that image with the gem stubbed (`ci.yml:360-364`); the resolver-hygiene gate runs the real fork file under the image's Ruby with a bind-mounted resolv.conf and hostname (`ci.yml:325-333`, `scripts/check-resolver-hygiene.sh`); the probe-guard gate checks the guard against an independent oracle and the egress ruleset (`ci.yml:339-347`); the wire proof against `smtp_mock`/`dns_mock` on loopback (`NODE/test/p2/socket_proof.rb`, README) is "out of band", NOT in CI, and runs with the guard overridden for its first eight rows. **No M1 job has ever run `GET /v1/audit` against the real gem, nor the control plane against a real node.** Treat the first harness run as exploratory.
- The control plane never takes a resolver: `orchestrator.New` defaults to `layers.NewNetResolver()` (`CP/orchestrator/orchestrator.go:88-92`; `MAIN:156-164` passes none). That is `net.DefaultResolver` of a pure-Go static binary: it reads `/etc/resolv.conf` and `/etc/hosts` and nothing else. M1's `internal/dnsfixture` (`CP/dnsfixture/dnsfixture.go`) is a test-only authoritative server on loopback, inside `internal/` (not importable from this module) and guarded against non-test imports: it is the MODEL for the zone shapes, not a component the platform can use.

## 2.2 Processes and containers

| Piece | What | Notes |
|---|---|---|
| `m1` | the control plane: front door, drainer (started only when `TM_QUEUE_ADDR_KEY` is set), health watcher (only when SMTP is wired and an enabled node exists), raw-partition loop | existing service; gains env, a second network and a resolv.conf mount |
| `verifier-node` | the forked truemail-rack: `thin` on 3000, Ruby 3.4.12 on Debian 12 | built from M1's OWN Dockerfile, unmodified (context `coldmail-verify/verifier-node/truemail-rack`); no platform Dockerfile needed |
| `m1-dns` | platform fixture: authoritative DNS (UDP+TCP 53), HTTPS 443 for the audit's IP lookup, forwarder for dotless names, control API | new, platform-owned Go (stdlib), like `PH/sink` |
| `m1-smtp` | platform fixture: the recipient MX; behaviour chosen by recipient domain and local part | new, same image as `m1-dns`, other role |
| (optional) `zbstub` | M1's own ZeroBounce stand-in (`control-plane/test/zbstub/Dockerfile`), accepted as an http host by name (`CP/external/zerobounce.go:200-208`) | only for `m1_vendor`, section 2.9 |

## 2.3 Control-plane environment

All names are read in `CP/config/config.go:690-921`; secrets in `CP/secrets/secrets.go:68-80`.

| Variable | Value / why |
|---|---|
| `TM_SMTP_MODE` | `enabled` (default `disabled`, `config.go:594`, read at `719`) |
| `TM_NODE_ID`, `TM_NODE_BASE_URL`, `TM_NODE_ENABLED`, `TM_NODE_PUBLIC_EGRESS_IP` | the first node uses the bare keys (`734-745`); `ENABLED` must be exactly `true`; the egress IP must EQUAL the audit's `current_host_ip` (GAP-24) or the node is never eligible. Use the node's own static address as both base-URL host and egress IP |
| `TM_NODE_TOKENS` | `<id>=<token>` (`secrets.go:72, 137-149`); the raw `Authorization` value sent to the node (NOT Bearer, `node/http_client.go:67`) |
| `TM_WARMUP_DAILY_CAP` | `0` (default 50 would starve a job; `<=0` means not enforced, `smtp_stage.go:253-255`) |
| `TM_WATCHER_CADENCE_SECONDS` | `0`: one audit at boot, no loop (`health/watcher.go:272-277`); see 2.4 |
| `TM_AUDIT_MAX_AGE_SECONDS` | `86400` (default 7200: the one boot audit would go stale, `audit.go:150`) |
| `TM_SMTP_CLIENT_DEADLINE_MS` | `30000` (default 15 s; lease = deadline + 120 s, `MAIN:243-247`) |
| `TM_QUEUE_MAX_ATTEMPTS` | `1` in the CI profile: a transient (greylist, timeout, connection_failed) settles at its first attempt instead of waiting (2.8) |
| `TM_QUEUE_MAX_RETRY_AGE_SECONDS`, `TM_QUEUE_RATE_WAIT_SECONDS` | `90`, `5`: bounds the capacity waits of section 0.4 |
| `TM_QUEUE_DRAIN_IDLE_POLL_SECONDS` | `1` (default 2) |
| `TM_CACHE_TTL_SECONDS` | `3600`: makes `m1_cache` reachable |
| `TM_ENV` | `development` (already); `TM_FALLBACK_MODE` stays `off` |
| unchanged | `TM_DATABASE_URL`, `TM_PEPPER`, `TM_QUEUE_ADDR_KEY`, `TM_LISTEN_ADDR` |

## 2.4 Provisioning and the audit gate (the part that is easy to get wrong)

Boot sequence in `buildSMTP` (`MAIN:419-536`), in this order: (1) `GET /v1/config` of the first enabled node, 10 s, once; failure leaves SMTP unwired for the process lifetime (`MAIN:471-477`); (2) the provider-routing drift check: a mismatch between the policy and the node's `validation_type_for` is a LOGGED warning only (`MAIN:485-492`); set the node's `VALIDATION_TYPE_FOR` to `gmail.com:mx,googlemail.com:mx,yahoo.com:mx,...` if the log should be quiet (`CP/provrouting/provrouting.go:30-45`); (3) `UpsertNodeState(id, url, "provisioning", false)` for every enabled node (`MAIN:501-508`); (4) the watcher goroutine starts and ticks AT ONCE (`watcher.go:272-274`): fetch `/v1/audit`, derive green/red, apply the GAP-24 cross-check, append `verify.node_audits` and update `nodes.audit_status/last_audit_at` (`audit/scheduler.go:171-198`, `store/postgres.go:461-489`).

**Eligibility** (`CP/audit/audit.go:117-154`, applied per request by `router.SMTPRoute`, `router/router.go:163-254`): `status=active` (or `recovering`, as a metered canary) AND `last_audit_at` known AND audit green AND `port25_open` AND `ExpectedEgressIP != "" AND last audit host ip == ExpectedEgressIP` AND last audit younger than `TM_AUDIT_MAX_AGE_SECONDS`. Failing conjuncts map to tokens: not active or never audited -> `no_eligible_node`; red, port25 closed, stale, egress mismatch -> `audit_red`.

**Who writes what.** `status`: only the watcher, on transitions its pure `Evaluate` decides (`health/decide.go:146-293`): audit red on any non-quarantined node -> `quarantined`; liveness; block rate; recovery. There is no `provisioning -> active` case. `port25_open`: written only by `UpsertNodeState`, which boot calls with `false`; the docs admit no operator command exists (`Docs/go-live-checklist.md` G1.2, `Docs/install-and-deploy.md:240-242`). Therefore the harness does, as the superuser, after the first green audit row exists:
```sql
UPDATE verify.nodes SET status='active', port25_open=true WHERE id='<TM_NODE_ID>';
```
and must repeat it after any control-plane restart. Wait first for `audit_status='green'` in `verify.nodes` (written by the boot tick): if that tick was RED the node is already `quarantined` (and with cadence 0 never recovers): fail the stage with the node's logs, do not retry silently. [U6]

**Why cadence 0.** Any later fetch error after a first known audit reads as "not green" and quarantines the node at once (`decide.go:210-213`, `watcher.go:197-201`). The node is a single-threaded Thin/EventMachine server (`NODE/Dockerfile:71`); an SMTP check that stalls for several seconds delays `/v1/audit` and `/healthcheck` behind it. One boot audit removes that flake; the cost is that "audit goes red -> quarantine" is not exercised in this stack [U5].

**What the node's audit needs** (`NODE/app/truemail_server/wrapper/app.rb:122-137`, GEM `auditor.rb`, `audit/ip.rb`, `audit/dns.rb`, `audit/ptr.rb`): (a) `Net::HTTP.get(URI('https://api.ipify.org'))` through the OS resolver (getaddrinfo, resolv.conf), TLS verified against Ruby's default store, body = the node's address, no trailing newline; (b) the A record of `VERIFIER_DOMAIN` through truemail's resolver (the `DNS` setting) must string-equal (a); (c) a PTR for the reverse name of (a) must name `VERIFIER_DOMAIN` exactly (`ptr_record.name.to_s`); (d) the wrapper adds `warnings.resolver` if the resolver is unclean (2.5) and `warnings.probe_guard` if the guard is overridden. Green iff `warnings == {}` and the IP is non-empty (`audit.go:60-68`). Any of (a)-(d) failing is RED.

## 2.5 DNS: who resolves what, and what "clean resolver" means in a compose service

- **Control plane** (layers): MX then, on not-found only, A and AAAA as two lookups (implicit MX, ADR-0020), every name rooted (`CP/layers/resolver.go:26-31, 123-156`, `layers.go:145-200`). Verdict mapping: MX answer -> found; NXDOMAIN/NODATA on MX and on both address families -> `invalid/no_mx`; MX `0 .` alone -> `invalid/null_mx`; SERVFAIL -> `unknown/dns_error`, origin `remote` (`dns_cause.go:67-89`); a timeout or an unreachable resolver -> `dns_error`, origin `own`; REFUSED/malformed -> `undetermined`.
- **Node** (truemail): `Truemail::Dns::Worker < Resolv::DNS`; the `DNS` environment variable (comma list of `ip` or `ip:port`) makes it use exactly those servers; unset, it reads resolv.conf (GEM `dns/worker.rb`; `NODE/config/system/command_line_params.rb:22`). MX targets are resolved with `getaddresses` (A and AAAA), and the node connects to the resulting ADDRESSES (GEM `validate/mx.rb`). The SAME fixture must therefore answer both, consistently.
- **Resolver hygiene** (ADR-0019 addendum, `NODE/.../resolver_hygiene.rb:43-87`): the audit is red if resolv.conf has a `search` or `domain` line, `options ndots:N` with N>1, or the hostname contains a dot. In a compose service: the default hostname is the 12-character container id (no dot): never set `hostname:` or `domainname:` to a dotted value. Docker's generated resolv.conf for a user network is `nameserver 127.0.0.11` + `options ndots:0` (fine) + the HOST's `search` line, and a GitHub runner HAS one: M1's own gate says so ("a GitHub runner's container has a `search` line", `scripts/check-resolver-hygiene.sh` header) and the platform's class 20(h) records it. Unfixed, the node's audit is red on the runner and green on a developer machine. The fix M1's CI itself uses is a read-only bind mount over `/etc/resolv.conf` (`scripts/check-dns-search-domain.sh:58`); compose can do the same. Alternative `dns_search: ["."]` [U2].
- **Fixture DNS requirements**: authoritative for the fixture zones and for the verifier A, the reverse zone and `api.ipify.org`; positive answers; NXDOMAIN and NODATA both with an SOA in authority (the model is `dnsfixture.go:226-275`); SERVFAIL and REFUSED per zone entry; silence per zone entry; AAAA answered as NODATA (a dropped AAAA stalls Go and Ruby); NXDOMAIN for every other dotted name; dotless names (`postgres`) forwarded to Docker's resolver at 127.0.0.11 (the fixture container shares the `private` network) so that mounting the fixture as the control plane's only nameserver does not break its database host name. A small stdlib Go server (the pattern of `PH/sink`, and of M1's own `dnsfixture`) is proposed over CoreDNS or dnsmasq because the recipes need per-name SERVFAIL, REFUSED and silence, SOA shapes and the dotless forwarder in one place; CoreDNS (`file`, `template` for rcodes, `forward`) can answer the positive and negative zones but silence per name is awkward [U10].
- **Pointing the services at it.** Node: `DNS=<fixture dns ip>` AND a bind-mounted resolv.conf containing only `nameserver <fixture dns ip>` (hygiene clean; glibc for the audit's IP lookup). Control plane: a bind-mounted resolv.conf `nameserver <fixture dns ip>` plus `options timeout:2 attempts:1` (a silent zone then settles in about 2 s instead of 10 s; Go's defaults are 5 s and 2 attempts). Option A for the control plane is compose `dns: [<fixture ip>]`, relying on the embedded resolver forwarding to a user-given server from a container on internal networks only [U1]: not recommended as primary.

## 2.6 How the probe reaches an SMTP fixture: the destination guard

- The node's guard refuses (`probe_guard.rb:45-78`): every IANA special-purpose IPv4 block (0/8, 10/8, 100.64/10, 127/8, 169.254/16, 172.16/12, 192.0.0/24, TEST-NET-1/2/3, 192.88.99/24, 192.168/16, 198.18/15, 224/4, 240/4) and every IPv6 address outside 2000::/3 (plus 2001::/23, 2001:db8::/32, 2002::/16, 3fff::/20). It checks BEFORE the gem connects (preflight, the gem's own MX step, ANY non-public address refuses the whole probe) and AFTER (the transcript is discarded), for the target and for the catch-all probe (`wrapper/app.rb:74-87`, `catchall_probe.rb:36-43`). A refusal is a FACT, `probe_refused`, which the control plane maps to `risky/non_public_mx`, confidence 20, `review`, not cached (`CP/smtp/probe_refused.go:11-32`, `smtp_stage.go:213-217`).
- **There is no allow-list dial.** The only escape is `PROBE_ALLOW_NON_PUBLIC=1` (exact string), documented "for the socket-level proof harness only", and it adds `warnings.probe_guard` to the audit, so the control plane treats the node as red and never routes to it (`app.rb:128-130`, `audit.go:60-68`, ADR-0021 decision 3). It cannot be used with the real control plane.
- **Therefore the fixtures live on public-looking addresses**: a second internal Docker network whose /24 lies outside every block above. The guard's own spec lists the first address after the CGNAT block as a public control (`ADR-0021`, "Every range, with its neighbours"), so call the subnet `S` = the /24 `100.128.0/24` (three octets written on purpose; a design value, not a host). The network is `internal: true`: nothing routes out, nothing is published, and the guard stays fully ON, which the `non_public_mx` recipe then proves in the same stack. The existing `private` network is NOT changed (module 3's worker skips TLS validation only for private addresses, `PH/docker-compose.yml:204-205, 290-292`; moving that network would break the hostile-sink test).
- The node's egress firewall (`verifier-node/egress/verifier_egress.nft`, layer 4) is not installed in the harness (it needs the node's network namespace and NET_ADMIN): say so in the NOT CHECKED list.
- The SMTP port is the node's `SMTP_PORT` (default 25, a single global setting, `NODE/config/system/configuration.rb:8-31`): run the fixture on 2525 and set `SMTP_PORT=2525` rather than granting a low-port capability. It is part of the engine fingerprint, harmless here.

## 2.7 What the SMTP fixture must say (the conversation the gem actually holds)

From GEM `validate/smtp.rb`, `smtp/request.rb`, and `NODE/.../app.rb:61-116`, `catchall_probe.rb:32-90`. For ONE verification the node makes TWO validations (the target, then the catch-all probe at `nx-<24 hex>@<same domain>`), and each validation is: a plain TCP connect-and-close ("port check", `Socket.tcp`), then a Net::SMTP 0.5.1 session: banner, `EHLO <verifier domain>` (HELO if refused), `MAIL FROM:<verifier email>`, `RCPT TO:<target>`, `QUIT`. Defaults: connection timeout 2 s, response timeout 2 s, connection attempts 2, `smtp_fail_fast` false (every MX address is tried in order until one gives an RCPT answer), TLS verification off, STARTTLS used only if advertised (do not advertise it).

Consequences for the fixture: tolerate a connection that opens and closes with no bytes; answer EHLO with a plain `250` (no STARTTLS, no AUTH); reply to RSET/NOOP; log each session's source address, MAIL FROM and RCPT (a control endpoint exposes the log: the second instrument that proves SMTP really happened). A clean RCPT 250 comes back to the control plane as `smtp_debug:null, success:true` (no code in the verdict). A 5xx/4xx at RCPT arrives as the raw reply string in `errors.rcptto` and the control plane parses the code from it.

The control plane's classifier (`CP/smtp/classify.go:39-120`) decides on code and body; the fixture wording matters:
- address fault (`invalid/mailbox_not_found`, conf 95): code 550/554 with a body matching `addressFaultBodyRe` (a noun user|account|customer|mailbox|recipient|address followed by unknown|not found|no such|does not exist|rejected|invalid|no longer|disabled|unavailable, or the reverse "unknown user"), or ANY 551/553. Example: `550 5.1.1 <addr> User unknown`. **Beware the false positive: "550 5.7.1 Recipient address rejected: Access denied" matches the address-fault pattern first and is `invalid`**, not blocked (`classify.go:28, 76-82`).
- blocked (`unknown/smtp_blocked`, always origin `own`, terminal, no retry): ANY 554 whose body is not address-fault (`classify.go:93-99`), or a 550 whose body matches `blocked|blacklist|denied|spam|policy|reputation|rbl|dnsbl|not allowed|refused|access denied|prohibited` and not the address-fault pattern (`classify.go:83-85, 124`): `554 5.7.1 Client host blocked by policy`.
- ambiguous (`unknown/smtp_error`): 550 with neither pattern (`550 5.7.0 Message rejected`), or any other 5xx; origin `remote` (an RCPT-stage reply).
- greylist (`unknown/greylisted`): `450`, any body containing "greylist", and ANY other 4xx including 421 (all map to greylisted, `classify.go:101-111`) except 452; origin `remote`.
- mailbox full (`valid/mailbox_full`, conf 90, `review`): `552` or `452`.
- `valid/accepted` (conf 90, `send`) requires target 250 AND the probe REJECTED with a 5xx (a domain that tells real from fake); 250 for both = `risky/catch_all` (conf 40); a probe that is 4xx/stalls/refused = `inconclusive` = `risky/catch_all` too (`smtp/score.go:104-137`, `catchall_probe.rb:82-90`).

## 2.8 Recipes: what makes M1 emit each verdict kind

`S` = the public-looking /24; `DNS` = the fixture DNS; `SMTP` = the fixture SMTP at S; "node" = needs the real node. Times are DERIVED from the code and defaults, not measured; "item" = per item inside a job whose drainer is idle-polling at 1 s. Domains below are `.test` fixture names. DB-list rows are inserted by the superuser into `verify.disposable_domains` / `verify.spam_trap_domains` (`domain`, `source`, `added_at`; the runtime role can only read them, `CP/store/postgres.go:342-360`).

| Verdict kind | DNS for the domain | SMTP script / other input | Node | Settles in; dials | Evidence in the result |
|---|---|---|---|---|---|
| valid / accepted (90, send) | MX 10 -> `mx.<d>` -> A in S (the SMTP fixture) | RCPT target 250; RCPT `nx-*@<d>` -> `550 5.1.1 <addr> User unknown` | yes | about 1 s/item | `catchall_conclusion:not_catchall`; sync meta has `node_id`, `verifier_ip` = the node's egress |
| valid / mailbox_full (90, review) | as above | RCPT target `552 5.2.2 mailbox full` (or 452) | yes | about 1 s | `smtp_code:552` |
| invalid / no_mx (96, skip) | no MX, no A, no AAAA (NXDOMAIN, or the name present with TXT only) | none | no | one poll | |
| invalid / null_mx (97) | `MX 0 .` (an A record beside it must not rescue it) | none | no | one poll | |
| invalid / mailbox_not_found (95) | as accepted | RCPT target `550 5.1.1 <addr> User unknown` | yes | about 1 s | `smtp_code:550`, `smtp_enhanced_code:5.1.1` |
| invalid / spam_trap (95) | MX present (the `mx` gate runs before the list gates: without MX this reads `no_mx`) | row in `verify.spam_trap_domains` | no | one poll | |
| risky / catch_all (40, review) | as accepted | every RCPT, incl. `nx-*`, answers 250 | yes | about 1 s | `catchall_conclusion:catchall` |
| risky / disposable (40, skip) | MX present | row in `verify.disposable_domains` | no | one poll | `flags.is_disposable:true` |
| risky / provider_no_smtp (35) | MX host under `google.com`, `googlemail.com`, `outlook.com`, `protection.outlook.com`, `office365.com`, `yahoodns.net` or `yahoo.com` (`CP/layers/dns_layers.go:56-68`); no A needed | none (the node is never called) | no | one poll | `smtp_skipped_reason:provider_mx_routed`; settled, cacheable |
| risky / non_public_mx (20, review) | MX host with an A in any guard-refused block (an RFC 1918 address is enough) | none (the node's pre-flight refuses) | yes | about 1 s | `probe_refused:true`, `probe_refused_ranges`; never cached |
| risky / plausible + `no_eligible_node` | as accepted | `UPDATE verify.nodes SET status='provisioning'` | wired | sync: at once; bulk: waits `TM_QUEUE_MAX_RETRY_AGE_SECONDS`, then settles | `failure_origin:own`; free; not cached |
| risky / plausible + `audit_red` | as accepted | `SET port25_open=false` (or a red/stale audit, or an egress mismatch) | wired | same | same |
| risky / plausible + `warmup_cap` | as accepted | a second M1 profile with `TM_WARMUP_DAILY_CAP=1` | wired | same | needs another env: not cheap |
| risky / plausible + `rate_limited` | one domain | 31 checks inside 60 s at one domain (`ratelimit.go:285-297`) | yes | sync: the 31st; bulk: 5 s waits | [U9] token-bucket edge not read to the last line |
| risky / plausible + `smtp_unwired` | any | `TM_SMTP_MODE=disabled`, no node list, or the boot `/v1/config` fetch failed | no | at once | needs a restart or a second M1: not in the wired stack; keep the charge rows M1-15/M1-17 proven by the existing oracle |
| risky / plausible, no token (M1-16) | any | not producible with the shipped wiring: the stage either runs or carries a token; the `not_probed` branch needs a node reply without a `catchall` block (`score.go:134`, `app.rb:108-113` always adds one) | | | row exists in the charge table; do not plan a recipe for it |
| unknown / greylisted, origin remote | as accepted | RCPT `450 4.2.0 greylisted, try later` | yes | CI profile (`MAX_ATTEMPTS=1`): about 1 s. Default dials: 300+600+1200 s = 35 min and 4 sessions, and the FIRST wait is at least 300 s whatever the base: the 300 s hint is hard-coded (`score.go:180`, `backoff.go:27-46`) | `greylisted:true` |
| unknown / dns_error, origin remote | `SERVFAIL` for the MX (and every type) | none | no | immediate | `failure_origin:remote` (charged) |
| unknown / dns_error, origin own | the zone name is SILENT (no reply) | none | no | `timeout x attempts` of the control plane's resolv.conf (2 s with the proposed options) | `failure_origin:own` |
| unknown / dns_error, undetermined | REFUSED for the MX | none | no | immediate | `failure_origin:undetermined` |
| unknown / smtp_timeout, origin remote | as accepted | RCPT of the target stalls longer than 2 s, the probe answers normally | yes | the gem retries the session once (attempts 2): about 4-5 s; with `MAX_ATTEMPTS=1` settles then | `failure_origin:remote` |
| unknown / smtp_timeout, origin own | any SMTP-eligible address | stop the node container mid-job (transport error, `smtp_stage.go:297-298`) | yes | the client deadline | `failure_origin:own`; needs `run.sh`-level control |
| unknown / smtp_error, origin remote | as accepted | RCPT `550 5.7.0 Message rejected` (neither pattern) | yes | about 1 s | `smtp_code:550` |
| unknown / smtp_error, undetermined | as accepted | the server closes the connection after RCPT (the gem reports "server dropped connection after response": no code, no timeout) | yes | about 1 s | |
| unknown / connection_failed, undetermined | MX host with an A in S where NOTHING listens on the SMTP port (the DNS container, which listens on 53 and 443 only): refused at once | none | yes | refused: at once; with default dials it is retried (a transient class), `MAX_ATTEMPTS=1` settles it | |
| unknown / smtp_blocked, origin own | as accepted | RCPT `554 5.7.1 Client host blocked by policy` | yes | about 1 s; terminal | |
| cache hit | any cacheable settled address | verify it twice (sync, or two jobs) with `TM_CACHE_TTL_SECONDS` set | not for the second | instant | `flags.from_cache:true`; the second job's row is charged `m1_cache` |
| external_verified (`m1_vendor`) | any catch_all or provider_no_smtp address | an optional second profile: `TM_FALLBACK_MODE=enabled`, `TM_EXTERNAL_DPA_CONFIRMED=true`, a throwaway key in `ZEROBOUNCE_API_KEY`, `TM_EXTERNAL_DAILY_SPEND_CAP>0`, `TM_EXTERNAL_ZEROBOUNCE_BASE_URL=http://zbstub:8080` and M1's own `zbstub` image | no | | this CHANGES the catch_all and provider rows of the main profile, so it cannot share a stack with them |
| unknown / unknown_error (error cap) | | a decrypt/policy/compute failure repeated 5 times | | | not reachable from fixtures; keep proven elsewhere |

## 2.9 Proposed compose and run.sh additions (minimal; nothing in a module is edited)

Networks: keep `private` as is; add `fixtures` (`internal: true`, one /24 `S` in `ipam`). Static addresses on `fixtures` only for the services the zone names: `m1-dns` S.2, `verifier-node` S.3, `m1-smtp` S.10.

```yaml
networks:
  fixtures:
    internal: true
    ipam: { config: [ { subnet: "${H_FX_SUBNET:?}" } ] }   # S, written into harness.env by run.sh; the guard calls it public
services:
  m1-dns:     # platform Go fixture, role dns+https+forwarder; on BOTH networks (control API reachable from private, forwards dotless names)
    build: { context: "${H_PLATFORM_DIR}/tests/harness/m1fixture" }
    user: "${H_HOST_UID}:${H_HOST_GID}"           # reads the per-run TLS key (mode 600), as smtp-ctl does
    environment: { FX_ROLE: "dns,https", FX_ZONE: "/fx/zone.json", FX_TLS_CERT: "/fx/ipify.crt", FX_TLS_KEY: "/fx/ipify.key" }
    volumes: [ "${H_WORK}/m1fx:/fx:ro" ]
    healthcheck: { test: ["CMD-SHELL","wget -q -O /dev/null http://127.0.0.1:8025/readyz || exit 1"], interval: 3s, timeout: 3s, retries: 30 }
    networks: { private: {}, fixtures: { ipv4_address: "${H_FX_DNS_IP:?}" } }
  m1-smtp:    # the same image, role smtp on 2525, the same healthcheck
    build: { context: "${H_PLATFORM_DIR}/tests/harness/m1fixture" }
    environment: { FX_ROLE: "smtp", FX_SMTP_ADDR: ":2525" }
    networks: { private: {}, fixtures: { ipv4_address: "${H_FX_SMTP_IP:?}" } }
  verifier-node:   # M1's own Dockerfile, unmodified
    build: { context: "${H_MODULES_ROOT}/coldmail-verify/verifier-node/truemail-rack", dockerfile: Dockerfile }
    environment:
      VERIFIER_EMAIL: "verifier@verifier.fixture.test"
      VERIFIER_DOMAIN: "verifier.fixture.test"
      ACCESS_TOKENS: "${H_M1_NODE_TOKEN:?}"
      DNS: "${H_FX_DNS_IP:?}"
      SMTP_PORT: "2525"
      SSL_CERT_FILE: "/fx/ca.pem"                  # the fixture CA, for the audit's HTTPS lookup [U3]
      RACK_ENV: "production"                       # PROBE_ALLOW_NON_PUBLIC is NEVER set
    volumes: [ "${H_WORK}/m1fx/node-resolv.conf:/etc/resolv.conf:ro", "${H_WORK}/m1fx/ca.pem:/fx/ca.pem:ro" ]
    healthcheck: { test: ["CMD","ruby","-rnet/http","-e","exit(Net::HTTP.get_response(URI('http://127.0.0.1:3000/healthcheck')).is_a?(Net::HTTPSuccess) ? 0 : 1)"], interval: 3s, timeout: 5s, retries: 40 }
    networks: { fixtures: { ipv4_address: "${H_FX_NODE_IP:?}" } }
  m1:               # the existing service, plus:
    networks: [ private, fixtures ]
    volumes: [ "${H_WORK}/m1fx/cp-resolv.conf:/etc/resolv.conf:ro" ]   # "nameserver <fixture dns>" and "options timeout:2 attempts:1"
    environment: { TM_SMTP_MODE: "enabled", TM_NODE_ID: "harness-node-1", TM_NODE_BASE_URL: "http://${H_FX_NODE_IP}:3000", TM_NODE_ENABLED: "true",
                   TM_NODE_PUBLIC_EGRESS_IP: "${H_FX_NODE_IP}", TM_NODE_TOKENS: "harness-node-1=${H_M1_NODE_TOKEN}", ... }   # + the dials of 2.3
    depends_on: { postgres: {condition: service_healthy}, m1-dns: {condition: service_healthy}, m1-smtp: {condition: service_healthy}, verifier-node: {condition: service_healthy} }
```
All four addresses are variables run.sh derives from `S` (for example S.2 for the DNS fixture, S.3 for the node, S.10 for the SMTP fixture): no literal public-looking address has to be committed, which also keeps the personal-details gate quiet.

`run.sh` additions, in order: (2 secrets) mint `H_M1_NODE_TOKEN` (masked), derive and write `H_FX_SUBNET`, `H_FX_DNS_IP`, `H_FX_NODE_IP`, `H_FX_SMTP_IP` into harness.env, mint a per-run CA and an `api.ipify.org` certificate with `openssl` (subject alternative name the host name, the CA key discarded after signing), write the zone and the two resolv.conf files into `$WORK/m1fx` (certificate key mode 600, the rest 644); (3 build) nothing new, `DC build` covers the three new images (the node image compiles native gems and needs the runner's network, as every build here does); (5, before `m1`) `up -d --wait m1-dns m1-smtp verifier-node`, then the PREFLIGHT, from inside the node container: `GET /v1/audit` with the raw token must say `audit_status:green` with `current_host_ip` equal to the egress IP, and the ipify lookup must print that IP [U3]; then `up -d --wait m1`; (5c) wait for `verify.nodes.audit_status='green'`, run the promotion `UPDATE`, seed the two list tables, then GATE with one sync verify of a known-accepted address expecting `valid/accepted` and `meta.verifier_ip` = the egress IP, plus the fixture's session log showing the target and an `nx-` probe; (8 exposure) add the new services to the `check-exposure.sh` list; (NOT CHECKED text) replace "module 1 no SMTP node" and "DNS has no route out" with what is now and is not exercised: the egress firewall, the default retry ladder, the quarantine on a red audit, a real MX behaviour, a node on the public internet.

Ordering: fixtures and node healthy -> preflight -> `m1` (it fetches `/v1/config` once at boot) -> promotion -> gate -> tests. The existing live test `TestM1Live_BulkJobSettlesItemByItem` keeps working (its oracle follows the module's report) but its `*.example.test` domains become NXDOMAIN, so they settle `invalid/no_mx` (charged) instead of `unknown/dns_error`: update its header comment and the NOT CHECKED text, and expect different ledger numbers.

---

## 3. Docs against code (disagreements found)

| Where | The doc says | The code does |
|---|---|---|
| `Docs/install-and-deploy.md:249`, `Docs/go-live-checklist.md` G1.9 | the first green audit promotes `provisioning -> active` | no running code does (section 0.3) [U6] |
| `Docs/history/phase-3-first-real-box-spec.md` / ADR-0009 | `audit.NextState` is the lifecycle | superseded by the P4 watcher (`MAIN:510-517`): `Evaluate` has no provisioning rule |
| `CP/store/store.go:235`, migration 0009 | job status is one of `submitted`, `running`, `complete`, `complete_with_unknowns` | only `running` is written; `complete` is derived |
| `/readyz` `config` string | `smtp=enabled nodes=1` reads as "SMTP on" | printed from configuration, not from the wired state |
| `PH/m1_bulk_settle_test.go` header, `PH/run.sh:425-428` | "module 1 no SMTP node", "DNS has no route out" | true today; false after this change |

## 4. Risks (what could make the real node unusable in compose)

| Risk | Why | Mitigation |
|---|---|---|
| Every fixture domain is `non_public_mx` | Docker private subnets are inside the guard's deny list | the separate public-looking /24 (2.6); keep a `non_public_mx` recipe as the positive control that the guard is ON |
| Audit red on the runner only | host search domain copied into the container's resolv.conf | bind-mount a clean resolv.conf; assert `warnings == {}` in the preflight |
| Audit red for no mail reason | ipify TLS (CA trust), A and PTR missing, trailing newline in the IP body | the preflight prints each of the three steps separately; serve the IP with no newline |
| Node stays `provisioning`, jobs hang 6 h | no promotion code; capacity waits | the promotion `UPDATE`, a hard gate, `TM_QUEUE_MAX_RETRY_AGE_SECONDS=90` |
| Wiring lost after a restart of `m1` | boot upsert resets the node; the `/v1/config` fetch is one-shot | never restart `m1` mid-run without repeating 5/5c; stage 9 stops only Postgres [U7] |
| Quarantine from a flaky audit | single-threaded Thin; any fetch error after a known audit quarantines | cadence 0; keep stalls short (target-only, 2 s) |
| Ports, capabilities | none needed: SMTP on 2525, DNS on 53 and 443 only inside the fixture container (running as the host uid needs `net.ipv4.ip_unprivileged_port_start=0` via compose `sysctls`, or run the DNS/HTTPS role as root in the container) | choose at spec time [U8] |
| The hand-written SMTP fixture is not what Net::SMTP expects | never exercised by M1's CI | drive the node's `POST /v1/verify` directly (raw token) for every recipe BEFORE involving the control plane; give the fixture its own unit test |
| Compose `up --wait` ordering | `m1` must start after the node is healthy | `depends_on` plus the explicit preflight stage |
| Image build cost | `--no-cache` Ruby image with native gems, minutes | accepted; the harness already builds this way |

## 5. Unsure parts, and how one runner run settles each cheaply

A diagnostic stage placed before any test, printing (and failing on) these facts:

- [U1] Docker's embedded resolver forwarding to a user-given `dns:` server from an internal-only container: avoided by the bind-mount design; settle only if Option A is wanted: `getent hosts <fixture name>` inside `m1`.
- [U2] A bind mount over `/etc/resolv.conf` is honoured and the node's audit has no `resolver` warning: print the node's `/v1/audit` JSON.
- [U3] `SSL_CERT_FILE` is honoured by Ruby's `Net::HTTP` in the node image, and the glibc lookup uses the mounted resolv.conf: one `ruby -rnet/http -e 'puts Net::HTTP.get(URI("https://api.ipify.org"))'` inside the node container must print the node's address.
- [U4] Docker accepts the /24 on an internal network and containers on it reach each other: `docker network inspect` plus the fixture's session log.
- [U5] Single-threaded Thin starves `/v1/audit` behind a stalled check: avoided by cadence 0; settle by timing an audit while a stall recipe runs.
- [U6] No promotion: after boot with a green audit and `port25_open=true` the row stays `provisioning` (`SELECT status`), and the verdict carries `no_eligible_node`.
- [U7] Restart behaviour of the stack through stage 9 (Postgres stopped): read `nodes.status` and a sync verify after it.
- [U8] Unprivileged low ports in the fixture container: print the fixture's own startup line.
- [U9] The exact edge of the 30/60 s rate bucket (`ReserveRateToken`, not read to the last line): count `rate_limited` in a 35-row single-domain sync burst.
- [U10] What CoreDNS plugins can do (rcode templates, dropping a query per name) is from memory and unchecked; only relevant if CoreDNS is preferred to the Go fixture.
- Direct node probe: for every recipe, one `POST /v1/verify` to the node itself, comparing its `smtp_debug` with the expected code, before the control plane is in the loop.

## 6. Verification and what this document does not vouch for

Read from code: sections 1 and 2.3-2.8 (file:line given). Read from the gem source at tag v3.3.1 (a public repository, not vendored): the audit's three steps, the SMTP request sequence, the default timeouts, the MX resolution. Not run: anything about Docker networking, Ruby's TLS store, the runner's resolv.conf, Thin's concurrency, the fixture programs (they do not exist yet), and every figure in the "settles in" column. Not covered by the fixtures even when built: a real provider's behaviour, the default retry ladder, quarantine on a red audit, the egress firewall, a node on the public internet, the vendor path unless the optional profile is built.
