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
| `PLATFORM_M1_KEY_SECRET` | `(unset)` | no | yes | The secret each user's module 1 caller key is derived from: the key is the unpadded base64url of the HMAC-SHA256, under this secret, of the fixed text m1-caller, a vertical bar and the user's id; module 1 keeps only the SHA-256 of each key. At least 32 bytes (64 random hexadecimal characters are plenty). The default (unset) means no module 1 key can be derived and no tenant scope built; the service still starts. Changing it changes every user's key. Never printed or logged. |
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
connection), an answer that breaks the contract (not JSON, a missing key, a status the route does not use, a body over the size limit), and every write that is not idempotent (module 1's bulk submission is the
one that matters: a second attempt would create a second job). The caller's context ends the whole call, including a wait between attempts. A call that fails returns an error and never a value: no default,
no zero verdict and no "probably fine" is ever produced for a module that did not answer.

The three values are validated at start-up like every other variable. A request may carry its own timeout instead of the dial (module 3's account creation and its bulk add run for tens of seconds
and the adapter says so per route); the retry count and the backoff are always the dials'.

## How a user's module 1 key is made

Modules 2 and 3 take the platform user's id as `user_id` on every call. Module 1 has no such parameter: its tenant is the owner recorded on the **caller key** that submits a job, and every job read
is limited to that owner. So the platform gives each user one module 1 key and writes the one row that lets module 1 recognise it (`internal/tenant`):

```
key    = base64url-without-padding( HMAC-SHA256( PLATFORM_M1_KEY_SECRET, "m1-caller|" + user id ) )     43 characters
digest = SHA-256( the key as text )                                                                     what module 1 stores
```

The user id is the platform user's id in lower case; the digest is of the key's text, which is what module 1's front door computes from the bearer it is shown. Nothing is stored on the platform's
side: the key is recomputed on every call. The row (`caller_id`, the digest, `not_before`, `user_id`) is written by `ProvisionM1Caller`, once per user, as the **privileged** identity of module 1's
database (module 1's runtime login, like the platform's `platform_app`, can only read that table). It is idempotent, and it refuses a user id that is empty or malformed because a key with no owner
can submit jobs that nobody can read. **`PLATFORM_M1_KEY_SECRET`** is optional so that a deployment that makes no module 1 call starts without it; without it no key is derived and no tenant scope
is built (the resolver answers `ErrNoM1KeySecret`). Changing the secret changes every user's key and leaves the old rows valid: provision the new rows first, and expire the old ones in module 1's
database once nothing uses them.

## What the service does with the database at start-up

Nothing at start-up itself. The connection pool is created lazily and the process starts and serves
`/healthz` even when the database is unreachable (the two entitlement loops make their first pass once the
server is listening, log a failed pass, and try again at their next interval). Readiness reports the database truthfully: `/readyz` answers 200 only when a
ping succeeds within `PLATFORM_READYZ_TIMEOUT_MS` and the service is not shutting down, and 503
otherwise. A process that is alive but not ready is the expected state during a database outage.
