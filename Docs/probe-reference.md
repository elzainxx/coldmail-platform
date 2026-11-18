# platform-probe reference

`platform-probe` (`cmd/platform-probe`, logic in `internal/probe`) is the platform's role as the **first caller of the three modules**. The harness runs it inside the private Docker network. For each service it checks health and readiness and, for each of the three modules, `describe` with the module's credential (the service token of modules 2 and 3, the per-caller key of module 1). It is written so that every check can fail and so that the run is red when a check did not run. Phase context: [`phase-0-spec.md`](phase-0-spec.md), chunk 6.

The tables below are checked against the code by tests in `internal/probe` (`TestReference_MatchesVars`, `TestReference_ChecksTableMatchesAHealthyRun`): each fails when the document and the code disagree in either direction, and each fails if it finds no rows, so a table that stopped matching anything cannot pass quietly.

## Configuration

An unset or blank variable takes its default. A missing required variable, or a value that is not valid, stops the process with exit status 2; the message names every bad variable and the reason, never a value.

| Variable | Default | Required | Secret | Meaning |
|---|---|---|---|---|
| `PROBE_PLATFORM_URL` | (none) | yes | no | Base URL of the platform service, as http://host:port. No credentials, query or fragment. |
| `PROBE_M1_URL` | (none) | yes | no | Base URL of module 1 (verify). Same rules as the platform URL. |
| `PROBE_M1_KEY` | (none) | yes | yes | Raw per-caller bearer key for module 1, sent on the job lookups and on describe. Never printed. |
| `PROBE_M2_URL` | (none) | yes | no | Base URL of module 2 (send_managed). Same rules as the platform URL. |
| `PROBE_M2_TOKEN` | (none) | yes | yes | Service token for module 2, sent as a bearer credential to describe. Never printed. |
| `PROBE_M3_URL` | (none) | yes | no | Base URL of module 3 (send_byo). Same rules as the platform URL. |
| `PROBE_M3_TOKEN` | (none) | yes | yes | Service token for module 3, sent as a bearer credential to describe. Never printed. |
| `PROBE_TIMEOUT_SEC` | `10` | no | no | Seconds one request may take, from connect to the last body byte, before its checks fail with timeout. Integer, 1 to 600. |
| `PROBE_MIN_CHECKS` | `25` | no | no | The run is red when fewer checks than this were executed. Integer, 1 to 1000. |

Column notes. Default `(none)` means the variable has no default. A URL with credentials in it is refused at start-up (exit 2), because the credentials would otherwise be sent as Basic authentication and turn a "no credential" request into an authenticated one. A secret may not contain whitespace or control characters.

The default floor of 25 is deliberately looser than the 52 checks a healthy run executes. A run that lost one whole module's checks (16 for a sender, 18 for module 1) still reaches at least 34, so the floor alone does not catch a dropped module; a caller that wants it caught sets `PROBE_MIN_CHECKS=52` (the harness does). (Every check that cannot run is a failure rather than a skip, which is what keeps the count stable when a service is down.)

## Output and exit status

Standard output holds exactly three kinds of line, one per check, in this order: platform, module 1, module 2, module 3.

```
CHECK <id> PASS
CHECK <id> FAIL <reason>
NOT-APPLICABLE <id> <why>
PROBE SUMMARY checks=<N> pass=<P> fail=<F> not_applicable=<A>
```

The summary is always the last line. `N` is passes plus failures; a `NOT-APPLICABLE` line is not a check and not a pass and is counted apart (no check is recorded that way today, since module 1 gained a describe; the line kind and the counter stay for a service that lacks an endpoint, and a unit test exercises them directly). Standard error holds one `REQUEST GET <url without credentials> auth=<none|configured|wrong>` line per request, a `PROBE RED:` line saying why the run is red, and, on every run, a `PROBE NOT CHECKED:` line.

| Exit | Meaning |
|---|---|
| 0 | No check failed and at least `PROBE_MIN_CHECKS` checks were executed. |
| 1 | A check failed, or fewer than `PROBE_MIN_CHECKS` checks were executed (the message names the floor). |
| 2 | Configuration error. Nothing was called. |

A failure reason says what was expected and what was got (the status code and the field concerned) and never dumps a body. A check that cannot run is a failure and not a skip: a network error, a timeout, a redirect or a body over 1 MiB fails every check of that request, so `N` does not shrink when a service is down. Network failures are reported by class (`connection refused`, `timeout`, `dns lookup failed`, `connection reset`, ...) and never with the URL.

One `http.Client` makes every request: the per-request timeout, **no redirect followed** (a 3xx is a failure), no proxy, no connection reuse, and at most 1 MiB of any response read.

## Secrets

The configured key and tokens are sent only in the `Authorization: Bearer` header. Every line the probe writes, to either stream, is scrubbed of the exact configured values before it is written (a module that echoes a credential into a field the probe quotes shows `[redacted]`), the request lines carry a label and not a credential, and a transport error is never printed as text. The scrub removes the exact value; a transformed copy of a secret (encoded, hashed, truncated by the module) is not recognised.

## The checks

A healthy run executes 52 checks (36 positive, 16 negative) and prints no NOT-APPLICABLE line. A **positive** check asserts that a good call is answered well. A **negative** check asserts that a call with no credential, or a deliberately wrong one, is refused with the module's error envelope; each negative call is the control that proves the positive call can fail. The positive call is always made first.

| Check | Kind | Asserts |
|---|---|---|
| `platform.healthz.ok` | positive | `GET /healthz`: 200, `status` is `ok`, `at` is a non-empty string. |
| `platform.readyz.ok` | positive | `GET /readyz`: 200, `status` is `ready`, `at` present, `checks.db` and `db` are true. |
| `m1.healthz.ok` | positive | As the platform's health check. |
| `m1.readyz.ok` | positive | 200, `status` is `ready`, `at` present, `checks.pepper`, `checks.db` and `ready` are true. |
| `m1.jobs.right_key.accepted` | positive | `GET /v1/jobs/<well-formed uuid>` with the caller key: 404 and `error.code` is `not_found` (authenticated, job absent). A 401 means the key was rejected. |
| `m1.jobs.right_key.request_id` | positive | The 404 envelope carries a non-empty `error.request_id`. |
| `m1.jobs.no_key.rejected` | negative | The same call with no Authorization header: 401 and `error.code` is `unauthenticated`. |
| `m1.jobs.no_key.request_id` | negative | That envelope carries a non-empty `error.request_id`. |
| `m1.jobs.wrong_key.rejected` | negative | The same call with a wrong bearer value: 401 and `unauthenticated`. |
| `m1.jobs.wrong_key.request_id` | negative | That envelope carries a non-empty `error.request_id`. |
| `m1.describe.ok` | positive | `GET /v1/describe` with the caller key (the one the job lookup sends): 200 and a JSON object. |
| `m1.describe.module` | positive | `module` is `verify`. |
| `m1.describe.api_version` | positive | `api_version` is `v1`. |
| `m1.describe.capabilities` | positive | `capabilities` is an object, and `capabilities.idempotent_bulk_create` and `capabilities.job_totals` are true. |
| `m1.describe.vocabularies` | positive | `vocabularies.status.values` is a non-empty array (the module serves its own constants). |
| `m1.describe.error_codes` | positive | `error_codes` is a non-empty array of `{code, http_status}`. |
| `m1.describe.no_key.rejected` | negative | The same call with no Authorization header: 401 and `error.code` is `unauthenticated`. |
| `m1.describe.no_key.request_id` | negative | That envelope carries a non-empty `error.request_id`. |
| `m1.describe.wrong_key.rejected` | negative | The same call with a wrong bearer value: 401 and `unauthenticated`. |
| `m1.describe.wrong_key.request_id` | negative | That envelope carries a non-empty `error.request_id`. |
| `m2.healthz.ok` | positive | As the platform's health check. |
| `m2.readyz.ok` | positive | 200, `status` is `ready`, `at` present, `checks.db`, `checks.singleton_lock`, `db` and `singleton_lock` are true. |
| `m2.describe.ok` | positive | `GET /v1/describe` with the service token: 200 and a JSON object. |
| `m2.describe.module` | positive | `module` is `send_managed`. |
| `m2.describe.api_version` | positive | `api_version` is `v1`. |
| `m2.describe.capabilities` | positive | `capabilities` is an object. |
| `m2.describe.pricing` | positive | `pricing` is an object and `pricing.currency` is `credits`. |
| `m2.describe.vocabularies` | positive | `vocabularies.job_state.values` is a non-empty array (the module read its database enums). |
| `m2.describe.error_codes` | positive | `error_codes` is a non-empty array of `{code, http_status}`. |
| `m2.describe.absent.extension_routes` | positive | Module 2 has no `extension_routes` key (a recorded shape difference from module 3). |
| `m2.describe.absent.schemas` | positive | Module 2 has no `schemas` key. |
| `m2.describe.absent.bulk_add` | positive | Module 2 has no `bulk_add` key. |
| `m2.describe.no_token.rejected` | negative | The same call with no Authorization header: 401 and `unauthenticated`. |
| `m2.describe.no_token.request_id` | negative | That envelope's `error.request_id` matches `^req_[0-9a-f]{32}$`. |
| `m2.describe.wrong_token.rejected` | negative | The same call with a wrong bearer value: 401 and `unauthenticated`. |
| `m2.describe.wrong_token.request_id` | negative | That envelope's `error.request_id` matches `^req_[0-9a-f]{32}$`. |
| `m3.healthz.ok` | positive | As the platform's health check. |
| `m3.readyz.ok` | positive | As module 2's readiness check. |
| `m3.describe.ok` | positive | `GET /v1/describe` with the service token: 200 and a JSON object. |
| `m3.describe.module` | positive | `module` is `send_byo`. |
| `m3.describe.api_version` | positive | `api_version` is `v1`. |
| `m3.describe.capabilities` | positive | `capabilities` is an object. |
| `m3.describe.pricing` | positive | `pricing` is an object and `pricing.currency` is `credits`. |
| `m3.describe.vocabularies` | positive | `vocabularies.job_state.values` is a non-empty array. |
| `m3.describe.error_codes` | positive | `error_codes` is a non-empty array of `{code, http_status}`. |
| `m3.describe.present.extension_routes` | positive | `extension_routes` is a non-empty array (module 3 only). |
| `m3.describe.present.schemas` | positive | `schemas` is present and not null (module 3 only). |
| `m3.describe.present.bulk_add` | positive | `bulk_add` is present and not null (module 3 only). |
| `m3.describe.no_token.rejected` | negative | The same call with no Authorization header: 401 and `unauthenticated`. |
| `m3.describe.no_token.request_id` | negative | That envelope's `error.request_id` matches `^req_[0-9a-f]{32}$`. |
| `m3.describe.wrong_token.rejected` | negative | The same call with a wrong bearer value: 401 and `unauthenticated`. |
| `m3.describe.wrong_token.request_id` | negative | That envelope's `error.request_id` matches `^req_[0-9a-f]{32}$`. |

When the describe call itself fails (not 200, not JSON, no answer), every `describe.*` field check of that module fails with that reason: an absent key cannot be proved absent in an error body, so nothing passes by omission.

Module 1 asserts no `pricing`: its describe has none (module 1 has nothing to quote), and the senders' `pricing` checks are not applied to it. Module 1's `vocabularies` check reads `vocabularies.status` (module 1's own constants), where the senders' reads `vocabularies.job_state` (their database enums).

## The contract the checks encode

Read from the module code, not from the module documentation. Authentication is `Authorization: Bearer <token>`.

- Platform: `GET /healthz` 200 `{"status":"ok","at":...}`; `GET /readyz` 200 `{"status":"ready","checks":{"db":true},"db":true,"at":...}`.
- Modules 2 and 3: `/healthz` and `/readyz` need no token; `/readyz` carries `checks.db` and `checks.singleton_lock` and the two top-level copies. `GET /v1/describe` needs the service token and answers 401 with `{"error":{"code":"unauthenticated","message":...,"request_id":"req_<32 hex>"}}` otherwise. Module 3 additionally has `extension_routes`, `schemas` and `bulk_add`; module 2 has none of the three.
- Module 1: `/healthz` and `/readyz` need no token (`checks.pepper`, `checks.db`, `ready`). `GET /v1/jobs/<uuid>` answers 401 `unauthenticated` without a valid per-caller key and 404 `not_found` with one when the job does not exist. `GET /v1/describe` takes the same per-caller key and answers 401 `unauthenticated` with none or a wrong one; its body carries `module` (`verify`), `api_version` (`v1`), `capabilities` (booleans; `idempotent_bulk_create` and `job_totals` are true), `routes`, `schemas`, `vocabularies` (`{name: {source: module_constant, values, fields}}`, among them `status`), `error_codes` (`[{code, http_status}]`) and `idempotency`, and has no `pricing`. Its request ids are UUIDs, not `req_<hex>`, which is why its rule is only "non-empty".

## What the probe does not check

It makes no verify, send or bulk call and so says nothing about module behaviour beyond health, readiness, describe and credential refusal. It does not check TLS, reachability from outside the private network (the harness's exposure check does), or the contents of describe beyond the fields in the table above. It does not detect a module that accepts only a prefix of a token, or one that rate-limits repeated refusals. Whole services being down is detected; one service being slow is detected only when it is slower than `PROBE_TIMEOUT_SEC`. The worst-case run time with a service that never answers is the number of requests to it times `PROBE_TIMEOUT_SEC`. The same list is printed on every run as the `PROBE NOT CHECKED:` line.
