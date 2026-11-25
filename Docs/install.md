# Installing the platform on Supabase

This is the install procedure, in the order it must be run. Every command below is the command CI runs against the digest-pinned `supabase/postgres` image (the `harness` workflow runs steps 2–4 as a real install does; `scripts/check-install-doc.sh` fails when this document names a file, command or setting that no longer exists). What CI does **not** run is stated at the end ("Not checked").

The platform and the three modules share one Supabase Postgres database, each in its own schema: `platform` (the platform), `verify` (module 1), `send_managed` (module 2), `send_byo` (module 3), plus the platform's two tables in `public` (`users`, `job_items`) that modules 2 and 3 read.

## Step 1 — the Supabase project

Create the project and note the **database** connection string of the migrating role `postgres` (not a superuser on Supabase: that is why the steps below exist). Keep Supabase Auth's email + password sign-in on and **public sign-up off** (locked decision: users are created by an admin). The `auth.users` table must exist: `platform_0001` refuses to run without it.

## Step 2 — required role provisioning (`db/install/preprovision.sql`)

Run once, as `postgres`, **before any module migrates**:

```
psql "$MIGRATE_DATABASE_URL" -v ON_ERROR_STOP=1 -f db/install/preprovision.sql
```

Why: on PostgreSQL 16 and later the creator of a role holds only `ADMIN OPTION` on it, with neither `SET` nor `INHERIT`, so `postgres` cannot create objects as the owner roles of modules 2 and 3 and their own migrations stop with `42501` (measured on every run by the `supabase` workflow's BASELINE leg: exactly eight statements fail without this step, and none with it). The file creates `send_managed_owner` and `send_byo_owner` if missing and grants both to `postgres` `WITH INHERIT TRUE, SET TRUE`. It is idempotent and changes no module.

## Step 3 — migrations, in this order, all as `postgres`

1. **Platform**: `platform-migrate up-and-set-password` (`PLATFORM_MIGRATE_DATABASE_URL`, `PLATFORM_MIGRATIONS_DIR=db/migrations`, `PLATFORM_APP_PASSWORD`); see [`migrator-reference.md`](migrator-reference.md). It creates `public.users` and `public.job_items` before any module needs them.
2. **Module 1**: its migrations, then the runtime password out of band (`ALTER ROLE verify_app PASSWORD …`), per the module's own install notes.
3. **Module 2** and **module 3**: each module's own migrator (`up-and-set-password`), with its own `MIGRATE_DATABASE_URL`. Their pre-flight (`check_platform_tables.sql`) passes only after step 3.1.

## Step 4 — the first admin

An admin is a Supabase login with a row in `platform.admins`. Create the login first (Supabase dashboard → Authentication → Users), then:

```
platform-migrate create-first-admin --auth-user-id <the login's id> --display-name "<name>"
```

It creates the `public.users` row and the `platform.admins` row in one transaction, as the privileged migrating identity, and **refuses when any admin already exists** (admin management is built in Phase 7; until then more admins are added out of band by the owner). It refuses an unknown login, a blank name and the all-zero system user, and it refuses a login whose platform user already exists and is **inactive** (reactivate that user first, or use a different login; the command never reactivates anyone).

## Step 5 — remove the platform's schemas from the exposed API (owner action, Supabase dashboard)

In Supabase → Settings → API → *Exposed schemas*, **remove `public` (and never add `platform`, `verify`, `send_managed` or `send_byo`)**. The platform's tables are reached only by the Go service over the database connection, never through PostgREST. `platform_0001` already revokes `anon`, `authenticated` and `service_role` on `public.users` and `public.job_items`; **no row-level security is enabled** (ruling: the exposed-schema setting and the revokes are the control, and a second mechanism would be a second place to be wrong). The same line is in the parent repository's `OWNER-ACTIONS.md`.

## Settings an admin owns

- **Unit costs** (`platform.unit_costs`): five units, integer credits ≥ 1. The seed is `m1_own` 1, `m1_cache` 1, `m1_vendor` **2**, `m2_accepted` 1, `m2_bounced` 1. The vendor-fallback price (`m1_vendor`) is an **admin setting** and must stay above 1; the seed of 2 is a placeholder the owner replaces before launch. Changing a price needs a named admin and a reason and is recorded in `platform.unit_cost_history`; a job freezes its prices at confirm, so a change never affects a job already confirmed.
- **Module 3** is not metered: an admin enables it per user, optionally until a date.

## Module 1's vendor fallback (ZeroBounce) — ships disabled

The platform charges a vendor answer as `m1_vendor` (the unit price above), but the vendor itself is **module 1's** and is switched on in module 1's environment; the platform is only TOLD whether it is on (`PLATFORM_M1_VENDOR_REACHABLE`, below). Everything in the table is module 1's own configuration (`coldmail-verify/Docs/vendor-zerobounce.md` is the reference; the owner steps, in order, are section 3 of the parent repository's `OWNER-ACTIONS.md`). Nothing is sent to the vendor until **all** of the switches are on:

| Dial (module 1) | Default | What it does |
|---|---|---|
| `TM_FALLBACK_MODE` | off | `enabled` allows the external-fallback stage (the global kill switch can still force it off) |
| `TM_EXTERNAL_DPA_CONFIRMED` | false | `true` only after the vendor's data-processing agreement is signed |
| `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` | `ZEROBOUNCE_API_KEY` | the **name** of the environment variable that holds the API key (the key itself is never in a file or a repository) |
| `TM_EXTERNAL_DAILY_SPEND_CAP` | 0 | credits per UTC day module 1 may spend at the vendor; 0 sends nothing |
| `TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS` | 8000 | per-call timeout, 3000–60000 |
| `TM_EXTERNAL_ZEROBOUNCE_BASE_URL` | `https://api.zerobounce.net` | the vendor endpoint (a regional host if the owner chooses one) |

**The platform's side of the switch: `PLATFORM_M1_VENDOR_REACHABLE`** (`true` or `false`, default `false`; [`config-reference.md`](config-reference.md)). The platform cannot read module 1's switches, so the owner switches both together: module 1's `TM_EXTERNAL_*` dials (above) and this variable, in the same change (the pair is in `OWNER-ACTIONS.md` of the parent repository). It decides what a **module 1 job reserves**, because a reserve holds credits at the dearest unit a job of the module can be charged:

- **off (default):** `m1_vendor` is not reachable, a module 1 job reserves **1 credit per row**, and a vendor answer that arrives all the same is never charged: the settlement returns it as unmapped, with the charge rule that decided it, and its credits stay in the reserve until the release.
- **on:** a **new** module 1 job reserves **2 credits per row**, and a vendor answer is charged `m1_vendor`.
- **A job already confirmed never changes.** Its prices were frozen when it reserved, in the other world or not: switching the variable moves no existing job, in either direction (a job reserved with the vendor on can still be charged for a vendor answer after it is switched off).
- **Every instance writes it.** Each platform instance, leader or standby, writes the value into `platform.unit_costs.reachable` when it starts and again every 60 seconds (every 5 seconds until the first write succeeds; it needs no database to start, so a database that comes up late is found within seconds). The write is idempotent: it changes nothing, and writes no history row, when the flag already holds. **All instances must be given the same value**: instances with different values would undo each other's change every minute (each such change is logged at warn as `unit reachability was changed by someone else`, and `platform.unit_reachability_history` records every change with its source). A change of the variable takes effect when the instances are restarted with it: each writes it at start-up.
- The platform side is covered by tests against a real Postgres (`TestMoneyReachable_*`) and by the unit tests of the loop; **not checked** here is whether module 1's own switches agree with this variable: the two are switched by the owner by hand.

A vendor *answer* reaches the platform as `risky` or `invalid` with reason `external_verified` and is charged `m1_vendor`; a vendor *non-answer* (an error, a timeout, an `unknown`) leaves module 1's own result standing and is charged by the ordinary rows, never as a vendor call. Not checked: any real vendor call, and the vendor's real status vocabulary (the mapping was built from its official SDKs; the owner verifies it against the live documentation first).

## Behaviours that are rulings, not accidents

- **Module 1's readiness is all-or-nothing on `TM_QUEUE_ADDR_KEY`.** Without the key `/readyz` reports not-ready and names the key. The platform always submits bulk jobs, so a deployment of module 1 without the queue can never serve the platform; a verify-only mode is deliberately not offered.
- **One address rule, in Go, for verification and suppression: `norm.Validate`** (`internal/norm`): module 1's frozen `norm/v1` normalisation, then module 1's own syntax layer exactly as module 1 implements it (after normalising, the local part is at most 64 bytes, the domain at most 255 bytes, and the domain holds a dot). An address that fails either half is never sent to a module, is counted and skipped, and is free (charge row P-08); module 1's own `invalid_syntax` answer (row M1-10) is therefore unreachable from the platform. The suppressions table stores what `Validate` returns and its CHECK (`suppressions_email_norm_v1`, `platform_0009`) is **the same rule where SQL can express it, and stricter or wider only where listed**: stricter on a NUL, white space in the local part, an empty label or a leading dot in the domain and an address over 254 bytes (an address of that kind is refused at the table although the validator accepts it; the writer treats SQLSTATE 23514 as "cannot be stored"), wider only on a punycode label that is not valid punycode (SQL cannot decode it; the writer validates first). **For those two shapes (`xn--a`, `xn--zzzzzzzz`) the Go validator is the ONLY wall**: the table would store them if they reached it, and the owner accepted that limit (Phase 4, Part A item A2) on one condition, that it is a gate. So **`internal/suppress` `Add` is the one place that writes `platform.suppressions`**: it calls `norm.Validate` first, stores the normalised form `Validate` returns (never its argument), reports a duplicate as "not created", and returns the table's own refusals as typed errors (SQLSTATE 23514 on `suppressions_email_norm_v1`, and 22021 for a NUL, are "cannot be stored"; the error never carries the address). `scripts/check-suppression-writers.sh` fails when any other non-test Go file under `internal/` or `cmd/`, or any SQL under `db/` outside `db/migrations/`, writes the table (INSERT, UPDATE, DELETE, TRUNCATE, COPY, MERGE), when a migration writes a row without a reasoned entry in `tests/suppression-writer-exceptions.txt`, and when a write in `internal/suppress/store.go` is not preceded by a `norm.Validate(` call in the same function (its fault proofs are in `ci/faults/p4a/`; the gate reads source, so SQL assembled by string concatenation, and everything under `tests/`, are not checked). `TestNormAgreement_Database` pins every difference by vector name against the real table, and the shared vector file runs through both implementations. **This supersedes the Phase 2 ruling OD-11** (single-label domains such as `a@b` were accepted): `a@b` fails module 1's layer, so it is neither sent nor stored. An earlier version of this paragraph said the two rules were identical; they were not (22 vectors differed).
- **The platform's migrations run before the modules'.** Since the Phase 3 close-out the shared table `public.job_items` carries one more nullable column, `rendered_text` (the optional plain-text part, `platform_0010`); modules 2 and 3 read it, and each refuses to migrate against a database that lacks it (its own pre-flight), so a module whose migrations ran before the platform's would stop with a message that names the column. The order of this document (platform chain first, then the three modules) is therefore a requirement, not a habit.
- **Module 1's node promotes itself.** A verifier node is `provisioning` with `port25_open=false` at every control-plane boot and becomes `active` only because a green audit recorded after that boot says so (module 1's own watcher; `port25_open` is taken from that audit). Nobody runs SQL to promote it, and a restart re-audits and re-promotes: it never trusts an `active` left behind by an earlier boot. What a green audit needs from its environment is in module 1's `operator-runbook` section 5.1. Module 1's audit does not itself open port 25 to a real mail host: the owner's egress smoke test (OWNER-ACTIONS, G1.3) stays the gate before real verification traffic.
- **Module 3's SMTP host allowlist stays empty in production.** Module 3 refuses an SMTP host that is, or resolves to, a loopback, private, link-local or cloud-metadata address (and the other special-use ranges) at add time and at connect time. The one dial that lets a private network through (`smtp_host_allow_cidrs`) exists for the harness's compose network; the module logs a loud warning at every start while it is not empty. Module 3's From-gate (`from_gate_mode`) stays `enforce`, its seeded value.
- **A scheduled job holds the one-active-job slot** for its module and user.
- **A disabled admin can no longer act**: the database refuses one in the ledger, the audit log, M3 entitlements and unit costs.

## Running more than one instance

The platform may run as several instances against one database (a rolling restart, a second node for availability). **Every instance serves HTTP; the two background loops run in exactly one of them.** The loops are the **expiry** of module 3 entitlements and the **reconcile** with module 3's suspend route. The instance that runs them is the **leader**; the others are **standbys**: they idle both loops (no pass, no call to module 3, no write), ask again at every tick, and take over when the leader is gone.

- **How the leader is chosen.** The leader is the instance that holds one PostgreSQL *session-level advisory lock*, key `7200000002` (in the platform's `7_xxx_xxx_xxx` band; the migrator holds `7100000001` while it runs). It is taken with `pg_try_advisory_lock`, which never waits, on **one dedicated connection** that the leader keeps for as long as it leads, so **one connection of the pool is checked out for that** (the pool's default is at least four). PostgreSQL frees a session lock the moment its session ends: a leader that crashes, is killed, loses its connection or is stopped hands the lock on without any lease, heartbeat or clock. A planned stop (SIGTERM) unlocks explicitly and returns the connection before the pool closes, so a standby takes over at its next tick instead of when the server notices a closed socket.
- **How long a takeover takes.** One tick of the standby's loops: the shorter of `PLATFORM_ENTITLEMENT_EXPIRE_INTERVAL_SEC` (default 30) and `PLATFORM_M3_RECONCILE_INTERVAL_SEC` (default 5), so about 5 s by default. The new leader's first pass of each loop is at that loop's own next tick.
- **One loop runs in every instance, not only in the leader's:** the reachability sync writes `PLATFORM_M1_VENDOR_REACHABLE` into the database (see "Module 1's vendor fallback" above). It is idempotent, so any number of instances do no harm, **provided they all have the same value**.
- **Name each instance.** Set `PLATFORM_INSTANCE_ID` to a different value on every instance (1 to 63 printable ASCII characters; the default is the host name, which is already distinct per container). The leader's lock session carries it as its `application_name`, and the election's log lines carry it as `instance`.
- **How to see the leader.** In SQL, as a role that can read `pg_stat_activity` (an admin of the database; exactly one row means one leader):

```
select a.application_name as leader, a.pid, a.backend_start
from pg_locks l join pg_stat_activity a on a.pid = l.pid
where l.locktype = 'advisory' and l.granted and l.objsubid = 1
  and ((l.classid::bigint << 32) | l.objid::bigint) = 7200000002;
```

  No row means no instance can reach the database (or all are starting). In the logs: `loops: this instance is now the leader`, `loops: another instance holds the leader lock; ...` (the standby, once per stretch) and `loops: this instance lost the leader lock; ...` (a leader whose session failed: logged once, at warn; it tries again at the next tick, like a standby).
- **The connection string must keep one backend for the whole session.** Use the direct connection or a **session-mode** pooler for `PLATFORM_DATABASE_URL`, not a transaction-mode pooler (the same reason the migrator needs the direct connection: [`config-reference.md`](config-reference.md)). Behind a transaction pooler the lock would be taken on a backend the connection does not keep; the leader checks every tick that its own backend holds the lock and gives the leadership up when it does not, but the lock could stay held on the pooler's server connection and no instance could lead until that connection closes.
- **What the lock does not promise.** A pass that is already running when its instance loses the lock runs to its end (bounded by its own timeout) while the next leader may have started; the ledger's unique keys (an expiry is one `m3_expire` row per user) and the reconciler's compare-and-set on the wanted version, not the lock, are what keep that safe. A leader that is cut off from the database but not killed keeps its server-side session, and so the lock, until the server decides the peer is gone (TCP keepalive): nothing runs twice, a takeover is late.
- **No collision of keys.** The money store and the entitlement writers take a transaction-level lock per (user, module) hashed into the same band; the hash steps over the platform's own constant keys (this lock and the migrator's), so a user's key never equals the leader's and the leader's session lock can never make a user's writes wait (a unit test in `internal/money` and one in `internal/entitlements` pin it).
- **What CI proves.** `TestLeader_*` (database tests, two real instances over one database: exactly one runs passes, the server shows exactly one granted lock held by the leader's `application_name`, a terminated leader session is replaced within ten seconds, twenty startup races never give two leaders, a stopped leader returns its connection) and the harness stage 9c in `tests/harness/run.sh` (two platform containers, the leader killed, the killed one restarted as a standby); the fault proof `loops-leader` shows the first of these red when the election is removed.

## Authentication of the job API

The job API accepts the access token Supabase gives a signed-in user (`Authorization: Bearer <token>`) and nothing else. What to set on the platform service, from the Supabase project's own settings (the full table is [`config-reference.md`](config-reference.md)):

- **A project with asymmetric signing keys (new projects):** `PLATFORM_JWKS_URL=https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json`. The service fetches the public keys from there (https only, at most every 30 seconds) and verifies ES256 and RS256 tokens with them.
- **A project on the legacy shared secret:** `PLATFORM_JWT_SECRET` is the project's *legacy JWT secret* (Supabase dashboard, project settings, JWT keys), used exactly as shown (at least 32 bytes, printable ASCII, no spaces). It verifies HS256 tokens. **Never put the secret in a file that is committed**: give it to the service through the environment or the host's secret store, like `PLATFORM_DATABASE_URL`.
- **Both may be set** while a project moves from one to the other; each token is judged by the source of its own algorithm.
- **`PLATFORM_JWT_ISSUER=https://<project-ref>.supabase.co/auth/v1`** pins the issuer. It is optional only so that the service starts without it: set it.
- `PLATFORM_JWT_AUDIENCE` (default `authenticated`, what a signed-in user's token carries) and `PLATFORM_JWT_LEEWAY_SEC` (default 30) normally stay as they are.
- **Nothing set** means the service starts and the job API is not served; the start-up log says `job API authentication is not configured`. An invalid value (a secret under 32 bytes, an `http://` JWKS URL, a leeway over 300) stops the process with exit status 2 and a message that names the variable and never its value.

The token proves who the user is and nothing more: the API resolves its `sub` to a platform user, and an inactive or unknown login is refused. Not checked here: a real Supabase token (the harness mints its own with a per-run key until GoTrue joins it, Phase 4 chunk B15), the project's key rotation (a rotated-in key verifies from the first token that names it at least 30 seconds after the previous fetch of the key set (a token that names it sooner is refused, and the user signs in again or retries); a withdrawn key stops verifying within an hour), and a signed-out session (an access token stays valid until its `exp`, normally an hour).

## Not checked

CI does not run GoTrue (no login is ever performed), the pooler (Supavisor), PostgREST or a hosted project's own grants and extensions, so step 1 and step 5 are verified by the owner, not by a runner; the `supabase` workflow measures migrations only, statement by statement, and the `harness` workflow measures the running stack on the same image. The leader election is exercised by two platform instances and by killing one, not under a network partition and not behind a transaction-mode pooler.
