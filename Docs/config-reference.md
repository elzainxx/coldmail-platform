# Configuration reference

Every environment variable the platform service reads. The table below is checked against the code by a
test (`internal/config`, `TestConfigReference_MatchesVars`): it fails when a variable is added, removed or
changed in one place and not the other, and it fails if it finds no rows at all, so a table that stopped
matching anything cannot pass quietly.

An unset or blank variable takes its default. A required variable with no value stops the process at
start-up with exit status 2, and the message names the variable and the reason, never a value.

| Variable | Default | Required | Secret | Meaning |
|---|---|---|---|---|
| `PLATFORM_HTTP_ADDR` | `:8080` | no | no | TCP listen address of the HTTP server, as host:port. Port 0 picks a free port. |
| `PLATFORM_DATABASE_URL` | (none) | yes | yes | PostgreSQL connection string of the least-privileged `platform_app` login. Never printed or logged. |
| `PLATFORM_LOG_LEVEL` | `info` | no | no | Minimum log level: one of debug, info, warn, error. |
| `PLATFORM_ENV` | `development` | no | no | Free-form label for the deployment (for example development or production), reported in the startup log line. |
| `PLATFORM_SHUTDOWN_TIMEOUT_SEC` | `30` | no | no | Seconds in-flight requests get to finish after SIGINT or SIGTERM before the remaining connections are cut. Integer, 1 to 3600. |
| `PLATFORM_READYZ_TIMEOUT_MS` | `2000` | no | no | Milliseconds the readiness probe waits for the database ping before answering not ready. Integer, 1 to 60000. |
| `PLATFORM_READ_HEADER_TIMEOUT_SEC` | `10` | no | no | Seconds a client has to send its request headers before the connection is closed. Integer, 1 to 3600. |
| `PLATFORM_M3_URL` | (none) | yes | no | Base URL of module 3's control service (http or https, with a host and no credentials), for example http://m3:8080. The platform suspends and resumes users through its admin route. |
| `PLATFORM_M3_ADMIN_TOKEN` | (none) | yes | yes | Module 3's operator (admin) key, sent as a bearer token on its admin route. A different credential from the service token. Never printed or logged. |
| `PLATFORM_ENTITLEMENT_EXPIRE_INTERVAL_SEC` | `30` | no | no | Seconds between passes of the expiry loop, which ends every M3 entitlement whose enabled-until date has passed by the database's clock. Integer, 1 to 3600. |
| `PLATFORM_M3_RECONCILE_INTERVAL_SEC` | `5` | no | no | Seconds between passes of the reconcile loop, which delivers each wanted suspend or resume to module 3 and records what module 3 answered. Integer, 1 to 3600. |
| `PLATFORM_M3_REVERIFY_INTERVAL_SEC` | `300` | no | no | Seconds after which a user already in the wanted state is checked against module 3 again (the same suspend or resume is sent once more; a changed answer means module 3 had drifted). Integer, 10 to 86400. |
| `PLATFORM_M3_REQUEST_TIMEOUT_SEC` | `10` | no | no | Seconds one request to module 3's admin route may take before it counts as failed and is retried later. Integer, 1 to 120. |
| `PLATFORM_MONEY_LOCK_TIMEOUT_MS` | `10000` | no | no | Milliseconds a money operation (grant, adjustment, reserve, settle, release) waits for another operation on the same user's credits of the same module before it gives up with a lock-timeout error and writes nothing. Integer, 100 to 600000. |
| `PLATFORM_MODULE_TIMEOUT_MS` | `15000` | no | no | Milliseconds one request to a module (module 1, 2 or 3) may take, counted per attempt and including the time to read the response body, before it counts as a timeout. A request that sets its own timeout (module 3's account creation and bulk add run for tens of seconds) overrides it. Integer, 500 to 300000. |
| `PLATFORM_MODULE_RETRIES` | `2` | no | no | Extra attempts a request to a module gets after a failure, 0 for none. Only a request marked idempotent (a read, or a write the module deduplicates by the platform's own key) is ever retried, and only after the module was unreachable, timed out or answered 429; a refusal, a not found, a module fault (500) or an answer that breaks the contract is never retried, and neither is a write that is not idempotent. Integer, 0 to 10. |
| `PLATFORM_MODULE_RETRY_BACKOFF_MS` | `250` | no | no | Milliseconds before the first retry of a request to a module; each further retry waits twice as long as the one before, up to 30 seconds, with no random jitter. A 429 that names a Retry-After waits the larger of the two (never more than the request timeout). 0 retries at once. Integer, 0 to 30000. |
| `PLATFORM_M1_KEY_SECRET` | `(unset)` | no | yes | The secret each user's module 1 caller key is derived from: the key is the unpadded base64url of the HMAC-SHA256, under this secret, of the text m1-caller, v and the key version (PLATFORM_M1_KEY_VERSION) and the user's id, each part set off by a vertical bar; module 1 keeps only the SHA-256 of each key. At least 32 bytes (64 random hexadecimal characters are plenty). The default (unset) means no module 1 key can be derived and no tenant scope built; the service still starts. Changing it changes every user's key: rotate it with a new key version (see PLATFORM_M1_KEY_VERSION). Never printed or logged. |
| `PLATFORM_M1_KEY_VERSION` | `1` | no | no | The version of the module 1 caller keys the platform derives and presents: the key of a user is the HMAC-SHA256, under PLATFORM_M1_KEY_SECRET, of the text m1-caller, v and this number, and the user's id, each part set off by a vertical bar. Integer, 1 to 999. Raise it (with a new PLATFORM_M1_KEY_SECRET) to rotate: provision the new version's rows first (the old keys keep working beside them), switch this dial and the secret, then retire the old version; the steps are in Docs/config-reference.md. Changing it without provisioning the new rows makes every module 1 call answer 401. |
| `PLATFORM_M1_URL` | `(unset)` | no | no | Base URL of module 1's control plane (http or https, with a host and no credentials, query or fragment), for example http://m1:8080. Module 1 is called with each user's derived caller key (see PLATFORM_M1_KEY_SECRET), not with a service token. The default (unset) means no module 1 call can be made and its adapter refuses with a typed error; the service still starts. With it set (and `PLATFORM_M1_KEY_SECRET`), the platform also reads module 1's `GET /v1/describe` for `vendor_fallback.reachable` at every module 1 preview and confirm and every 60 seconds in each instance, with the key of a service caller of its own, derived from the same secret (see "Module 1's vendor fallback" below). Consumed by `internal/adapters/m1.FromConfig` and `internal/app`. |
| `PLATFORM_M2_URL` | `(unset)` | no | no | Base URL of module 2's platform API (http or https, with a host and no credentials, query or fragment), for example http://m2:8080. The default (unset) means no module 2 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m2.FromConfig`. |
| `PLATFORM_M2_SERVICE_TOKEN` | `(unset)` | no | yes | Module 2's service token (its Domain A bearer), sent on every platform call to module 2. Printable ASCII with no spaces. The default (unset) means no module 2 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m2.FromConfig`. Never printed or logged. |
| `PLATFORM_M3_SERVICE_TOKEN` | `(unset)` | no | yes | Module 3's Domain A service token, sent as the bearer on the platform's user-facing calls to module 3. NOT the operator key PLATFORM_M3_ADMIN_TOKEN, which stays a different credential. Printable ASCII with no spaces. The default (unset) means no user-facing module 3 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m3.FromConfig`. Never printed or logged. |
| `PLATFORM_JWT_SECRET` | `(unset)` | no | yes | The HS256 secret of the Supabase project (its "legacy JWT secret"), used as the raw bytes of the string, with which the job API verifies the access tokens users present. At least 32 bytes of printable ASCII with no spaces. The default (unset) means no HS256 token is accepted; the service still starts, and the job API is not served until this or PLATFORM_JWKS_URL is set. Consumed by `internal/auth.FromConfig`. Never printed or logged. |
| `PLATFORM_JWKS_URL` | `(unset)` | no | no | Where the Supabase project publishes the public keys its access tokens are signed with (a project with asymmetric signing keys): `https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json`. An http or https URL with a host and no credentials, query or fragment; `internal/auth` fetches it only over https, at most every 30 seconds, with a 10 second timeout, no proxy and no redirect, and refuses it at start-up when it is http. Keys are used for ES256 (P-256) and RS256 (at least 2048 bits) only. The default (unset) means no asymmetric token is accepted; the service still starts, and the job API is not served until this or PLATFORM_JWT_SECRET is set. Consumed by `internal/auth.FromConfig`. |
| `PLATFORM_JWT_ISSUER` | `(unset)` | no | no | The issuer (iss) every access token must carry, exactly, for example `https://<project-ref>.supabase.co/auth/v1`. Printable ASCII with no spaces, at most 512 bytes. The default (unset) means the issuer is not checked: a token is judged by its signature, audience and times alone. Set it in production. Consumed by `internal/auth.FromConfig`. |
| `PLATFORM_JWT_AUDIENCE` | `authenticated` | no | no | The audience (aud) every access token must include; Supabase gives a signed-in user's token authenticated. Printable ASCII with no spaces, at most 512 bytes. Consumed by `internal/auth.FromConfig`. |
| `PLATFORM_JWT_LEEWAY_SEC` | `30` | no | no | Seconds of clock difference between the token's issuer and this process that exp, nbf and iat are judged with: a token is expired at exp plus this, and not yet valid before nbf minus this. Integer, 0 to 300. Consumed by `internal/auth.FromConfig`. |
| `PLATFORM_JOB_MAX_ROWS` | `100000` | no | no | The most rows one job may hold. A longer list is refused when the job is created, with the limit named: the job API answers 413 `too_many_rows`, and a CSV body is cut off at the limit while it is read. Integer, 1 to 100000 (100000 is module 1's own limit for one job). Consumed by `internal/app` (`jobs.WithMaxRows`, on the one job store) and, through the store's `MaxRows`, by `internal/api`. |
| `PLATFORM_JOB_MAX_START_DELAY_MIN` | `30` | no | no | Minutes after its start that a confirmed or scheduled job may wait for the engine to start it before it expires and its credits are released. The start deadline is the scheduled start (the moment of the confirm for a job with none) plus this, added by the database's clock when the job is confirmed; a job confirmed before the dial was changed keeps the deadline it was given. Integer, 1 to 1440. Consumed by `internal/app` (`jobs.WithMaxStartDelay`, on the one job store). |
| `PLATFORM_RESULTS_PAGE_MAX` | `1000` | no | no | The most results one page of the job API's `GET /v1/jobs/{id}/results` may hold: the largest `limit` it accepts (a larger one is refused with 400 `validation_failed`); without a `limit` a page is 100, or this when it is smaller. The CSV download is not paged by it: it streams every result, 1000 at a time. Integer, 1 to 1000. Consumed by `internal/app` (`api.New`, `Deps.ResultsPageMax`). |
| `PLATFORM_JOB_ENGINE_ENABLED` | `true` | no | no | Whether this instance runs the job engine: `true` or `false` and nothing else (any other value stops the process at start-up with exit status 2). With `true` (the default) the engine runs in the instance that holds the leader lock, beside the entitlement loops and under the same lock: it starts confirmed and due jobs in their module, reads what the module reports, records and charges every finished item, completes jobs and gives back the credits a job did not use. With `false` the engine does not start in this instance and the start-up log says so once; a job then stays where it is. Give every instance the same value: a job waits for the engine of whichever instance leads, so one instance with the engine off can win the lock and hold every job. The engine needs the module URLs and tokens (`PLATFORM_M1_URL` with `PLATFORM_M1_KEY_SECRET`, `PLATFORM_M2_URL` with `PLATFORM_M2_SERVICE_TOKEN`, `PLATFORM_M3_URL` with `PLATFORM_M3_SERVICE_TOKEN`): a job of a module that is not configured waits and goes stale, it is never failed for it. Consumed by `internal/app` (it builds and runs `internal/jobengine`, or does not). |
| `PLATFORM_JOB_TICK_MS` | `1500` | no | no | Milliseconds between two passes of the job engine, each of which looks at every job that has something to do. Integer, 100 to 60000. Consumed by `internal/app` (`jobengine.Config.Tick`). |
| `PLATFORM_JOB_SETTLE_INTERVAL_SEC` | `10` | no | no | Seconds the engine leaves a running job alone between two observations of its module, unless the last observation showed finished items it has not recorded yet or said the job was complete (the next pass then looks again). Integer, 1 to 3600. Consumed by `internal/app` (`jobengine.Config.SettleEvery`). |
| `PLATFORM_JOB_STALE_AFTER_MIN_M1` | `30` | no | no | Minutes a running or cancelling module 1 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to 10080 (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 1). |
| `PLATFORM_JOB_STALE_AFTER_MIN_M2` | `15` | no | no | Minutes a running or cancelling module 2 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to 10080 (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 2). |
| `PLATFORM_JOB_STALE_AFTER_MIN_M3` | `15` | no | no | Minutes a running or cancelling module 3 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to 10080 (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 3). |
| `PLATFORM_JOB_SUBMIT_BATCH` | `500` | no | no | How many rows one submit call of the engine offers module 2 or module 3 (one run call each; a module that takes fewer is asked for the rest at the next pass). Module 1 takes a job's whole list in one request and ignores this dial. Integer, 1 to 5000. Consumed by `internal/app` (`jobengine.Config.SubmitBatch`). |
| `PLATFORM_JOB_SUBMIT_CONCURRENCY` | `8` | no | no | How many run calls to module 2 or module 3 one submit batch has in flight at once; the first row of a batch always goes alone, and the batch stops at the first answer that says the module is busy or cannot answer. Integer, 1 to 64. Consumed by `internal/app` (`jobengine.RegistryDeps.SubmitConcurrency`). |
| `PLATFORM_JOB_STEP_TIMEOUT_SEC` | `120` | no | no | The most seconds the engine spends on ONE job in one pass: a module that hangs costs a job this much, then the engine goes on to the next job and tries this one again at the next pass. Keep it above `PLATFORM_MODULE_TIMEOUT_MS` (module 1's bulk submit is given at least two minutes). Integer, 5 to 3600. Consumed by `internal/app` (`jobengine.Config.StepTimeout`). |
| `PLATFORM_JOB_BACKOFF_CAP_SEC` | `60` | no | no | The longest the engine leaves a job alone after a pass that made no progress on it (its module is down or full: nothing handed over, nothing recorded). The wait starts at one pass (`PLATFORM_JOB_TICK_MS`) and doubles with every such pass in a row up to this, so a hundred jobs whose module is down cannot keep the engine from newer jobs, and a module that recovers is polled again within this many seconds; a job that is moved by someone (a cancel) or makes progress is looked at at the next pass. The engine never waits longer than a quarter of the module's staleness limit (`PLATFORM_JOB_STALE_AFTER_MIN_M1`, `_M2`, `_M3`), so a stale job is still noticed on time, and the database refuses a wait of more than 600 seconds whatever this says. Integer, 1 to 600. Consumed by `internal/app` (`jobengine.Config.BackoffCap`). |
| `PLATFORM_JOB_PAUSE_MAX_DAYS` | `7` | no | no | Days a job may stay paused before the engine cancels it, counted by the database's clock from the moment it was paused (a job that is resumed and paused again starts a new count). A job paused while it was running is cancelled through `cancelling`: its module cancels the items it has not claimed, every item already sent stays sent and charged, and the job ends `cancelled`; a job paused before its start is cancelled at once. Either way the items not sent are cancelled and free, the credits not consumed are released, the user's one-active-job slot is freed, and an event whose reason begins `pause_limit` says why, with the limit in its detail. A user who resumes at the very moment the engine cancels meets exactly one of the two outcomes: the resume wins, or it is refused as a resume of a job that is not paused. Integer, 1 to 90. Consumed by `internal/app` (`jobengine.Config.PauseMax`, in the engine of the instance that leads). |
| `PLATFORM_CONTENT_RETENTION_DAYS` | `7` | no | no | Days after a send job ended (succeeded, failed, cancelled or expired), counted from its `finished_at` by the database's clock, before the engine clears the message the platform kept for it in `public.job_items`: `rendered_subject` and `rendered_body` become `[removed]`, `rendered_text` and `unsubscribe_url` become empty (`reply_to` and the recipient stay). The job's outcomes (its rows, results, events and charges) are kept for ever, and so are the items of a job that has not ended and the rows that belong to no job. A job of 100000 rows is cleared a few thousand rows at a time over several passes, and the job is marked once the last page is done. Integer, 1 to 365. Consumed by `internal/app` (`jobengine.Config.RetentionAfter`, in the engine of the instance that leads). |
| `PLATFORM_UNSUBSCRIBE_SECRET` | `(unset)` | no | yes | The secret the unsubscribe links of sent messages are signed with: a link's token names one item of a send job and carries the first 24 bytes of the HMAC-SHA256, under this secret, of the text unsub, v1 and the item's id, each part set off by a vertical bar (the format is in Docs/unsubscribe.md). At least 32 bytes of printable ASCII with no spaces (64 random hexadecimal characters are plenty). The default (unset) means no link can be made: the service still starts, but a send job (module 2 or 3) cannot be confirmed (409 `not_allowed`, reason `unsubscribe_not_configured`) and the `/u/{token}` routes are not served. It works only together with PLATFORM_PUBLIC_BASE_URL. There is no rotation: changing it makes every link already sent answer the not-found page, so keep it. Consumed by `internal/app` (`unsubscribe.NewSigner`, `jobs.WithUnsubscribe` and the public endpoint). Never printed or logged. |
| `PLATFORM_PUBLIC_BASE_URL` | `(unset)` | no | no | The public address of this platform as the recipients of its messages reach it, for example `https://app.example.com`: the unsubscribe link of every message is this, `/u/` and the item's token. `https://` in lower case, then a host name of letters, digits, dots and hyphens, an optional port and an optional path of letters, digits and `._~-/`; no credentials, query or fragment, at most 500 characters; trailing slashes are dropped. The default (unset) means no link can be made (see PLATFORM_UNSUBSCRIBE_SECRET). Whatever stands at this address must pass `/u/` on to this service unchanged. Consumed by `internal/app` (`unsubscribe.Link`, through `jobs.WithUnsubscribe`). |
| `PLATFORM_UNSUBSCRIBE_RATE_PER_MIN` | `30` | no | no | How many requests one client address may make to the public `/u/{token}` routes in a minute before they answer 429 with a `Retry-After`: a token bucket per address (an IPv6 address counts as its /64) that holds this many requests at most and refills evenly; at most 10000 addresses are remembered. Behind a reverse proxy the address is the proxy's until a trusted forwarded-for header is configured (a later phase), so every recipient shares one bucket there. Integer, 1 to 600. Consumed by `internal/app` (`endpoint.Config.RatePerMin`). |
| `PLATFORM_INSTANCE_ID` | `(hostname)` | no | no | Name of this platform instance, 1 to 63 printable ASCII characters. With more than one instance running, only the one holding the leader lock runs the expiry and reconcile loops; the name is the application_name of the database session that holds it (pg_stat_activity) and appears in the loop log lines. Each instance needs its own. The default (hostname) means this machine's host name. |

Column notes. Default `(none)` means the variable has no default. Secret means the value is never written
to a log, an error message or the startup line; the service prints the variable name only.

## Which database connection string goes where

There are two connection strings in play and they are not interchangeable.

**`PLATFORM_DATABASE_URL` is the service's runtime login and must be the least-privileged `platform_app`
login.** It is a login role that does not own the schema, is not a superuser, and holds only the grants
the service needs. Do not point it at the owner role, at `postgres`, or at any role that can create or
alter objects: the grant proofs in the later database phase assume the runtime login cannot.

**The migrator must be given the DIRECT (session-mode) Supabase connection string, not the transaction
pooler.** The migration runner serialises itself with a session-level PostgreSQL advisory lock and holds
that lock for the whole run. A transaction-mode pooler hands the client a different backend connection
for each transaction, so the lock is taken on one backend and released, or silently lost, on another, and
two migrators could then run at once. Session-level advisory locks only mean what they say on a
connection that stays attached to one backend for the whole session, which is what the direct
(session-mode) connection provides. `PLATFORM_DATABASE_URL` is never the migrator's connection string:
the runtime login must not be able to run migrations.

## Module 3's entitlement flow

The six `PLATFORM_M3_*` and `PLATFORM_ENTITLEMENT_*` variables above wire the two loops the service runs
beside its HTTP server (`internal/entitlements`). **Expiry** ends every M3 entitlement whose enabled-until
date has passed, on the database's clock, by writing the `m3_expire` ledger row; **reconcile** delivers each
wanted suspend or resume to module 3 through its admin route and records what module 3 itself answered.
Both loops start once the HTTP server is listening and stop, before the connection pool closes, on SIGINT or
SIGTERM. A failed pass never stops a loop.

**`PLATFORM_M3_ADMIN_TOKEN` is module 3's operator key, not the service token.** Module 3 keeps the two
credentials apart (`ADMIN_API_KEY_HASHES` against `PLATFORM_TOKEN_HASHES`), and the platform sends this one
only to the admin route. A wrong key makes module 3 answer 401: the reconcile loop reports a configuration
fault and slows down, and every user's state stays "not converged" until the key is fixed. Nothing in this
file or in a log carries the token.

**`PLATFORM_M3_SERVICE_TOKEN` is the other credential, module 3's Domain A service token, and the two stay different on purpose:** module 3 answers 401 to each of them on the other's routes, and the
token that carries the platform's user-facing calls must never be able to suspend or resume a user, which is what the operator key can do. The service token is optional (unset means no user-facing
module 3 call can be made and `internal/adapters/m3.FromConfig` refuses with a typed error); the operator key is required.

## How long a money operation waits

Every operation on the money of one user and module (grant, adjustment, reserve, settle, release) takes one database advisory lock first, so two of them on the same
credits run one after the other. Before the close-out of Phase 2 the second one waited for the first for as long as it took, so a transaction that got stuck holding
the lock held every other operation on that balance for ever. **`PLATFORM_MONEY_LOCK_TIMEOUT_MS`** bounds that wait: every money transaction begins with
`SET LOCAL lock_timeout` set to this many milliseconds, and an operation that waits longer fails with the store's `ErrLockTimeout` (which names the user, the module
and the wait, and no SQL) and writes nothing. The default is 10 seconds; the accepted range is 100 ms to 10 minutes, and 0 is refused at start-up (PostgreSQL reads a
timeout of 0 as "wait for ever"). An operation that gives up is safe to retry: every money write is idempotent per business key. The value is validated at start-up like
every other variable (an invalid one stops the process with exit status 2) and reaches the store as `money.WithLockTimeout(cfg.MoneyLockTimeout)`; the service builds no
money store before the first phase that serves money requests, and a store built without the option, like the zero value of the store, waits 10 seconds and never without a bound.

## Which instance runs the loops

With more than one platform instance on one database, **`PLATFORM_INSTANCE_ID`** names each of them (1 to 63 printable ASCII characters; unset, or `(hostname)`, means the machine's host name, which
is already distinct per container). Only the instance that holds the leader advisory lock runs the two entitlement loops; the others serve HTTP and idle the loops until the leader's database session
ends. The leader's lock session carries the name as its `application_name`, so `pg_stat_activity` shows which instance leads, and the election's log lines carry it as `instance`. The name is validated at
start-up like every other variable (a longer or non-ASCII one stops the process with exit status 2, naming the variable and not echoing the value), and an instance started without a name takes the
host name. How the election works, how to see the leader and what it does not promise: [`install.md`](install.md), "Running more than one instance". The lock needs a connection string that keeps one
backend for the whole session (direct or session-mode, never a transaction-mode pooler), the same condition as the migrator's.

## How a call to a module is bounded and retried

Every request the platform makes to module 1, 2 or 3 goes through one client (`internal/modclient`), and three variables bound it: **`PLATFORM_MODULE_TIMEOUT_MS`** is how long one
attempt may take, from the first byte sent to the last byte of the response body read (a module that sends its headers and then stalls is a timeout too); **`PLATFORM_MODULE_RETRIES`** is the number of
extra attempts and **`PLATFORM_MODULE_RETRY_BACKOFF_MS`** the first wait between attempts (each wait is twice the one before, capped at 30 seconds, with no random jitter, so a run can be
reproduced). They reach the client as `modclient.FromConfig(cfg)`.

A retry is rare on purpose. It happens only when the request was marked idempotent by the code that built it (a read, or a write the module deduplicates by the platform's own key, such as module 2
and 3's `run`) **and** the failure was one of three kinds: the module could not be reached (connection refused, DNS failure, a reset connection, a 502 or 504, a 503 with the code `unavailable`), the attempt
timed out, or the module answered 429. A 429 that carries a `Retry-After` waits the larger of that and the backoff, never more than the request timeout. Everything else comes back at once, whatever the
dials say: a refusal (400, 401, 403, 409, 410, 413, 422), a not found, `paused` (a 503 that means an operator stopped the module: wait, do not page and do not hammer), a 500 (a fault of the module, not of the
connection), an answer that breaks the contract (not JSON, a missing key, a status the route does not use, a body over the size limit), and every write that is not idempotent (module 1's synchronous verify is the
one that matters: a second attempt repeats billed work; its bulk submission is idempotent since the Phase 3 close-out, on the Idempotency-Key every submission carries, and is retried with the same key). The caller's context ends the whole call, including a wait between attempts. A call that fails returns an error and never a value: no default,
no zero verdict and no "probably fine" is ever produced for a module that did not answer.

The three values are validated at start-up like every other variable. A request may carry its own timeout instead of the dial (module 3's account creation and its bulk add run for tens of seconds
and the adapter says so per route); the retry count and the backoff are always the dials'.

## How a user's module 1 key is made

Modules 2 and 3 take the platform user's id as `user_id` on every call. Module 1 has no such parameter: its tenant is the owner recorded on the **caller key** that submits a job, and every job read
is limited to that owner. So the platform gives each user one module 1 key and writes the one row that lets module 1 recognise it (`internal/tenant`):

```
key    = base64url-without-padding( HMAC-SHA256( PLATFORM_M1_KEY_SECRET, "m1-caller|v" + PLATFORM_M1_KEY_VERSION + "|" + user id ) )     43 characters
digest = SHA-256( the key as text )                                                                     what module 1 stores
```

The user id is the platform user's id in lower case; the digest is of the key's text, which is what module 1's front door computes from the bearer it is shown. Nothing is stored on the platform's
side: the key is recomputed on every call. The row (`caller_id`, the digest, `not_before`, `user_id`) is registered AUTOMATICALLY, at the user's first module 1 call, by the install function `platform.provision_m1_caller` (`db/install/postmodule1.sql`, Docs/install.md step 3b: SECURITY DEFINER, owned by module 1's `verify_admin`, executable by `platform_app` only), which the service calls through `tenant.M1CallerEnsurer`; no operator step per user and no module 1 admin credential in the service (module 1's runtime login, like the platform's `platform_app`, can only read that table). The operator tools' privileged writer `ProvisionM1Caller` (`internal/tenant/m1key.go`, used by the rotation below and the tests) stays. The write is idempotent, and it refuses a user id that is empty or malformed because a key with no owner
can submit jobs that nobody can read. **`PLATFORM_M1_KEY_SECRET`** is optional so that a deployment that makes no module 1 call starts without it; without it no key is derived and no tenant scope
is built (the resolver answers `ErrNoM1KeySecret`).

### Rotating the secret (owner ruling OD-2: one derived key per user, with a key version)

The version is in the text the HMAC is taken over, so a user's version-2 key is a different key from the version-1 key even under one secret, and module 1 can hold both at once: its key table was
designed for rotation (two rows for one `caller_id` with overlapping windows, migration 0010). The steps, none of which interrupts a user:

1. Choose the new secret (at least 32 bytes) and the next version, N = the current version + 1.
2. **Provision** every ACTIVE user's version-N row beside the old one (the automatic provisioning only registers the version the service presents, at a user's next call: do this before the switch so that no user's next job meets a key module 1 has not seen): build a resolver with the NEW secret and `tenant.WithM1KeyVersion(N)` and call `ProvisionM1Caller` (or `ProvisionM1CallerVersion(…, N)`)
   for each user, as the privileged identity of module 1's database. It is idempotent. Both keys now authenticate as the same user (same `caller_id`, same owner).
3. **Switch** the platform: set `PLATFORM_M1_KEY_SECRET` to the new secret and `PLATFORM_M1_KEY_VERSION` to N, and restart. From then on the platform presents only version N.
4. **Retire** the old rows: build a resolver with the OLD secret and call `RetireM1CallerVersion(…, N-1, grace)` for each user (the grace is how long an old key still works; `0` ends it at once). The row
   gets an end (`not_after`) and the status `rotating_out`; nothing is deleted.
5. Forget the old secret.

Switching before step 2 is finished makes every call of a user without a version-N row answer 401 (module 1 has never heard of the key); step 4 before step 3 locks everyone out. The tests that prove the
steps: `internal/tenant` (the pinned vectors of versions 1, 2 and 12, the distinctness of versions, the statements of provisioning and retirement) and, against module 1's real front-door lookup,
`TestM1LiveTenant_RotationBothVersionsAuthenticateAsTheSameUserThenTheOldOneEnds` in the harness.

## Where the modules are and which credential each call carries

Four optional variables tell the module adapters where their module is and what the platform sends it. **`PLATFORM_M1_URL`** and **`PLATFORM_M2_URL`** are base URLs with the same rule as
`PLATFORM_M3_URL`: an absolute http or https URL with a host, and no credentials, no query and no fragment (a URL with a user name or a query is refused at start-up, and the message names the
variable and never the value). **`PLATFORM_M2_SERVICE_TOKEN`** and **`PLATFORM_M3_SERVICE_TOKEN`** are the Domain A bearer credentials of modules 2 and 3: printable ASCII with no spaces (a stray
newline or a pasted non-ASCII character stops the process at start-up, not at the first request), kept in the `Secret` type, and shown in the printed configuration only as `[redacted]`
(or `(unset)` when not set). Module 1 has no service token: it is called with each user's own derived caller key (`PLATFORM_M1_KEY_SECRET`, above), so it has a URL and no token.

All four are optional, so that a deployment that makes no call to a module starts without them. A module whose URL or token is not set is not reached by default: its adapter answers with a typed
error, never a made-up verdict. The consumers are `internal/adapters/m1.FromConfig` (`PLATFORM_M1_URL`), `internal/adapters/m2.FromConfig` (`PLATFORM_M2_URL` and `PLATFORM_M2_SERVICE_TOKEN`) and
`internal/adapters/m3.FromConfig` (`PLATFORM_M3_SERVICE_TOKEN`, with `PLATFORM_M3_URL`). The M3 service token is a different credential from `PLATFORM_M3_ADMIN_TOKEN` (see above).

## Module 1's vendor fallback (nothing is configured here)

Whether a module 1 job reserves 1 credit per row or 2 depends on whether module 1's paid vendor fallback (ZeroBounce) can answer at all. **The platform has no setting for it**: module 1 says so
itself, in `GET /v1/describe` (`vendor_fallback.reachable`, computed per request from the very switches its external stage uses: `TM_FALLBACK_MODE=enabled`, `TM_EXTERNAL_DPA_CONFIRMED=true`, the vendor
key present in the variable `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` names, `TM_EXTERNAL_DAILY_SPEND_CAP` above 0, the policy opting in and the kill switch not pulled). The platform reads it before the
transaction of every module 1 **preview** and **confirm** (the answer is kept for 5 seconds) and, in every instance, every 60 seconds (every 5 seconds until the first success), and writes what it read into
`platform.unit_costs.reachable` for `m1_vendor` through the audited function `platform.set_unit_reachable` (source `m1_describe`; nothing is written when nothing changed). The owner therefore switches
**module 1's own `TM_EXTERNAL_*` dials and nothing on the platform**. A job's prices are frozen at its confirm: a later change never touches a job already confirmed, and a preview made under the other
setting is refused as stale at confirm (`preview_stale`). When module 1 cannot be read (it is down, or an older module 1 whose describe has no `vendor_fallback`), the flag keeps its last value, the
preview says so (`vendor_reachable_known: false`) and an immediate confirm is refused (`module_unavailable`) only while the flag was never set from module 1's own answer; `Docs/install.md` ("Module 1's
vendor fallback") has the exact rule. The describe call needs a module 1 caller key, and module 1 has no service credential: the platform uses the key of a **service caller** of its own (a reserved id, never
a customer), derived from `PLATFORM_M1_KEY_SECRET` and `PLATFORM_M1_KEY_VERSION` exactly like a customer's and registered through the same function at the first read.

## How the job API verifies a login

The job API (Phase 4) authenticates the Supabase access token each request carries as `Authorization: Bearer <token>`. Five variables say how, and **all five are optional**: with neither `PLATFORM_JWT_SECRET`
nor `PLATFORM_JWKS_URL` set the service starts as before and the job API is simply not served. They reach `internal/auth` as `auth.FromConfig(cfg)`, which `internal/app` calls at start-up to validate them
(one log line, `job API authentication configured`, says which sources are on and never a value; `job API authentication is not configured` says there are none).

- **`PLATFORM_JWT_SECRET`** is the project's legacy JWT secret, the key of **HS256** tokens, used as the raw bytes of the string (it is not base64-decoded). At least 32 bytes; a shorter one stops the process with exit status 2.
- **`PLATFORM_JWKS_URL`** is where the project publishes the public keys of its **asymmetric** signing keys (ES256 on P-256, RS256 with a modulus of at least 2048 bits). It is fetched only over https (an http URL is a
  start-up error naming the variable, exit status 2), at most once every 30 seconds, with a 10 second timeout, no proxy, no redirect and a 1 MiB cap on the answer; a fetched key set is trusted for an hour, an unknown `kid`
  makes one refresh, and while the issuer cannot be reached the keys already held keep serving.
- **The two sources may both be set** (a project that is moving from the legacy secret to asymmetric keys): an HS256 token is judged by the secret alone and an ES256 or RS256 token by the key set alone. No other
  algorithm is accepted, `none` least of all, and a published public key is never used as an HMAC key.
- **`PLATFORM_JWT_ISSUER`** pins `iss` exactly; unset means it is not checked, so set it in production. **`PLATFORM_JWT_AUDIENCE`** (default `authenticated`) must be one of the token's `aud` values.
  **`PLATFORM_JWT_LEEWAY_SEC`** (default 30, 0 to 300) is the clock difference tolerated on `exp`, `nbf` and `iat`: a token is expired when now is at or after `exp` plus the leeway.
- A verified token proves only who the user is (`sub`, a canonical lower-case uuid). The `role` and `email` claims are carried and never trusted for authorisation: the API resolves `sub` to a platform user and an
  inactive or unknown login is refused.

## The limits of a job and of the job API

Three variables bound what a job and its API serve; each has a default that needs no change, and an out-of-range value stops the process at start-up with exit status 2 (the message names the variable, never the value). The API they bound is described in [`jobs-api.md`](jobs-api.md).

- **`PLATFORM_JOB_MAX_ROWS`** (default 100000, 1 to 100000) is the longest list one job holds: `internal/app` gives it to the one job store (`jobs.WithMaxRows`), and the API reads the store's limit while it reads a CSV, so a list far over the limit is refused (413 `too_many_rows`, with the limit in `details`) without being read to its end. 100000 is module 1's own limit for one job.
- **`PLATFORM_JOB_MAX_START_DELAY_MIN`** (default 30, 1 to 1440) is how long, in minutes, a confirmed or scheduled job may wait for the engine to start it before it expires and its reserve is released: the database adds it to the scheduled start (or to the moment of the confirm) when the job is confirmed, so changing the dial moves no job already confirmed.
- **`PLATFORM_RESULTS_PAGE_MAX`** (default 1000, 1 to 1000) is the largest `limit` of `GET /v1/jobs/{id}/results`. It does not page the CSV download, which streams every result a page of 1000 at a time, so the memory of a download does not depend on the size of the list.

## The job engine's dials

Ten variables tune the job engine (the leader-only loops that carry a confirmed job to its end: [`install.md`](install.md), "Running the job engine"). Each has a default that needs no change, and an out-of-range value stops the process at start-up with exit status 2 (the message names the variable, never the value).

- **`PLATFORM_JOB_ENGINE_ENABLED`** (`true` or `false`, default `true`) is the switch. With `false` the engine does not start in that instance and the start-up log says so once (`job engine is switched off`); jobs stay where they are. Give every instance the same value. The harness sets it to `false` on the platform containers whose tests write jobs by hand.
- **`PLATFORM_JOB_TICK_MS`** (default 1500, 100 to 60000) and **`PLATFORM_JOB_SETTLE_INTERVAL_SEC`** (default 10, 1 to 3600) set how often the engine looks at a job and how long it leaves a running job alone between two observations of its module.
- **`PLATFORM_JOB_STALE_AFTER_MIN_M1`**, **`_M2`** and **`_M3`** (defaults 30, 15, 15; 1 to 10080) are how long a running job may make no progress before its `stale` event. The job is never touched: an administrator decides.
- **`PLATFORM_JOB_SUBMIT_BATCH`** (default 500, 1 to 5000) and **`PLATFORM_JOB_SUBMIT_CONCURRENCY`** (default 8, 1 to 64) set how many rows one submit call offers module 2 or 3 and how many run calls of it are in flight at once. Module 1 takes a job's whole list in one request.
- **`PLATFORM_JOB_STEP_TIMEOUT_SEC`** (default 120, 5 to 3600) bounds one job's step in one pass; keep it above `PLATFORM_MODULE_TIMEOUT_MS`.
- **`PLATFORM_JOB_BACKOFF_CAP_SEC`** (default 60, 1 to 600) is the longest the engine leaves a job alone after a pass that made no progress on it. The engine looks at jobs oldest-due first, at most a hundred per pass; a job whose module does not answer is looked at again after one pass, then two, four, ... up to this, so jobs that are stuck cannot use every pass and keep a newer job waiting, and a module that comes back is polled again within this many seconds. A cancel, a pause or any other move of the job wakes it at once; a start deadline and the release of an ended job's reserve do not wait for it. It is never more than a quarter of the module's staleness limit.

What the engine needs besides these is already in the table: the module URLs and tokens (`PLATFORM_M1_URL`, `PLATFORM_M1_KEY_SECRET`, `PLATFORM_M2_URL`, `PLATFORM_M2_SERVICE_TOKEN`, `PLATFORM_M3_URL`, `PLATFORM_M3_SERVICE_TOKEN`) and the module call dials (`PLATFORM_MODULE_TIMEOUT_MS`, `PLATFORM_MODULE_RETRIES`, `PLATFORM_MODULE_RETRY_BACKOFF_MS`). A job of a module that is not configured waits (and goes stale); it is never failed for it.

## The pause limit and the content retention

Two variables set the engine's housekeeping (Phase 5). Both are read by the engine of the instance that leads, a few times a minute, after it has looked at the jobs that are due; give every instance the same values. An out-of-range value stops the process at start-up with exit status 2 (the message names the variable, never the value). Every comparison of time is made by the database's clock.

- **`PLATFORM_JOB_PAUSE_MAX_DAYS`** (default 7, 1 to 90) is how long a job may stay paused. After that the engine cancels it: a job that was paused while it was running goes through `cancelling` (its module cancels the items it has not claimed, the ones already sent stay sent and charged, and the job ends `cancelled`), and a job that was paused before it started is cancelled at once. In both the items not sent are cancelled and free, the credits not consumed go back to the user, the user's slot for that module is free again, and the job's history says why (an event whose reason begins `pause_limit`, with the limit in the detail of the note beside it). A job that is resumed and paused again starts a new count. Nothing already sent is refunded. See [`jobs-api.md`](jobs-api.md), "Pause, resume and cancel of a running job".
- **`PLATFORM_CONTENT_RETENTION_DAYS`** (default 7, 1 to 365) is how long after a send job ended the platform keeps the message it rendered for each recipient in `public.job_items`, the table modules 2 and 3 read. After that the subject and the body there read `[removed]` and the plain-text part and the stored unsubscribe link are empty; the recipient and `reply_to` stay. The job's outcomes (its rows, results, events and charges) are never touched, and neither is a job that has not ended. The columns the modules read are NOT NULL, which is why the placeholder is a string and not a NULL; the unsubscribe link in a message already sent keeps working. What else holds the campaign's text, and what the modules themselves keep, is in [`install.md`](install.md), "Content retention".

## The unsubscribe link

Three variables make the per-recipient unsubscribe link of every message of a send job and the public endpoint that answers it (`GET` and `POST /u/{token}`: [`unsubscribe.md`](unsubscribe.md)).

- **`PLATFORM_UNSUBSCRIBE_SECRET`** and **`PLATFORM_PUBLIC_BASE_URL`** work only together. With either unset the service still starts, logs once which one is missing (`unsubscribe link is not configured`, or `is only half configured` when exactly one is set; never a value), does not mount the `/u/` routes (a request for one is the 404 envelope of any unknown path) and **refuses to confirm a send job** (`409 not_allowed`, `details.reason` `unsubscribe_not_configured`, [`jobs-api.md`](jobs-api.md)). A verify job is unaffected.
- The secret signs a 54-character token per item (`unsub|v1|` and the item's id under HMAC-SHA256); there is no rotation, so a changed secret makes every link already sent answer the not-found page. It is at least 32 bytes of printable ASCII with no spaces, held as a `Secret` (never printed or logged), and the base URL is `https://` in lower case with a host name, an optional port and an optional path of letters, digits and `._~-/`, at most 500 characters, trailing slashes dropped. The narrow alphabet is deliberate: the link is written into an HTML attribute, a mail header and a column whose CHECK refuses white space and angle brackets.
- **`PLATFORM_UNSUBSCRIBE_RATE_PER_MIN`** (default 30, 1 to 600) is a token bucket per client address, applied before any other work. Behind a reverse proxy the address is the proxy's until a trusted forwarded-for header is configured (the deployment phase): then everyone shares one bucket.

## What the service does with the database at start-up

Nothing at start-up itself. The connection pool is created lazily and the process starts and serves
`/healthz` even when the database is unreachable (the two entitlement loops make their first pass once the
server is listening, log a failed pass, and try again at their next interval; the reachability sync, which reads module 1's describe and writes the vendor unit's flag, tries from the start, logs a failure once per class and retries every 5 seconds until its first success). Readiness reports the database truthfully: `/readyz` answers 200 only when a
ping succeeds within `PLATFORM_READYZ_TIMEOUT_MS` and the service is not shutting down, and 503
otherwise. A process that is alive but not ready is the expected state during a database outage.

## Removed variables

A variable that an earlier version read and no version reads any more is **ignored**: setting it changes nothing and cannot stop the process (a value that was once invalid is no error now), and the
process says so once at start-up, in a warning that names the variable and what replaced it and never shows its value. The list is `config.RemovedVars` and a test holds this section equal to it.

- `PLATFORM_M1_VENDOR_REACHABLE` (removed in Phase 4, owner ruling OD-7): module 1 says whether its vendor fallback is reachable (`GET /v1/describe`, `vendor_fallback.reachable`) and the platform reads it
  at every module 1 preview and confirm. Switch module 1's own `TM_EXTERNAL_*` dials; delete this variable from the deployment.
