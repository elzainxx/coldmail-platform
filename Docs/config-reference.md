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

## What the service does with the database at start-up

Nothing at start-up itself. The connection pool is created lazily and the process starts and serves
`/healthz` even when the database is unreachable (the two entitlement loops make their first pass once the
server is listening, log a failed pass, and try again at their next interval). Readiness reports the database truthfully: `/readyz` answers 200 only when a
ping succeeds within `PLATFORM_READYZ_TIMEOUT_MS` and the service is not shutting down, and 503
otherwise. A process that is alive but not ready is the expected state during a database outage.
