# Migration runner reference

`platform-migrate` applies the files in `db/migrations/` to the platform's PostgreSQL database. This page is the whole of its
configuration and its contract. The variable table below is checked against the code by a test (`internal/migrate`,
`TestMigratorReference_MatchesVars`): it fails when a variable is added, removed or changed in one place and not the other,
and it fails if it finds no rows at all, so a table that stopped matching anything cannot pass quietly.

An unset or blank variable takes its default. A configuration problem stops the command before it connects, with exit status 2,
and the message names the variable and the reason, never a value.

| Variable | Default | Required | Secret | Meaning |
|---|---|---|---|---|
| `PLATFORM_MIGRATE_DATABASE_URL` | (none) | yes | yes | PostgreSQL connection string the migrator runs as: a role that may create roles and re-own objects. DIRECT (session-mode) connection only, never a transaction pooler. |
| `PLATFORM_MIGRATIONS_DIR` | `db/migrations` | no | no | Directory holding the .up.sql and .down.sql migration files, named platform_NNNN_ then a description. |
| `PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC` | `10` | no | no | Seconds to wait for the migration advisory lock, and the SET LOCAL lock_timeout of every migration file. Integer, 1 to 3600. |
| `PLATFORM_MIGRATE_STATEMENT_TIMEOUT_SEC` | `300` | no | no | The SET LOCAL statement_timeout of every migration file, in seconds. Integer, 1 to 86400. |
| `PLATFORM_MIGRATE_ALLOW_DOWN` | `false` | no | no | true permits the down command; anything else refuses it. Forward-only unless set. Must be true or false. |
| `PLATFORM_APP_PASSWORD` | (none) | password commands | yes | The platform_app login password, set by set-app-password and up-and-set-password. Never logged, never in a migration file. |

Column notes. Default `(none)` means the variable has no default. Required `password commands` means required by `set-app-password`
and `up-and-set-password` and not read by the others. Secret means the value is never written to a log, an error message or the
usage text. A timeout of 0 is rejected, because to PostgreSQL 0 means no timeout.

## The connection string must be a direct, session-mode connection

**`PLATFORM_MIGRATE_DATABASE_URL` must be the DIRECT (session-mode) connection string, not a transaction pooler.** The runner
serialises itself with a session-level advisory lock (`pg_try_advisory_lock`) and holds it for the whole of `up` or `down`. A
transaction-mode pooler hands the client a different backend for each transaction, so the lock is taken on one backend and
released, or silently lost, on another, and two migrators could then run at once. Nothing in the runner can detect that from
the inside; the guarantee holds only on a connection that stays on one backend. On Supabase that is the direct connection on
port 5432, not the pooler.

It is also never the service's connection string. `PLATFORM_DATABASE_URL` is the least-privileged `platform_app` login and must
not be able to run migrations (see `config-reference.md`). The migrator's login is a different, privileged role: the bootstrap
creates roles and re-owns objects, which only a superuser or a role with `CREATEROLE` can do.

`connect_timeout` in the connection string bounds the connection attempt. Without one the runner bounds it at 15 seconds, so an
unreachable database fails fast; the connection string's own value replaces it. Two other values are fixed and are not tuning
dials: the lock retry loop polls every 250 milliseconds (how long it may run is `PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC`), and the
unlock, rollback and close that run after the work are bounded at 5 seconds.

## Commands and exit status

| Command | What it does |
|---|---|
| `up` | Applies every pending `platform_NNNN_*.up.sql` in ascending order, one transaction per file. |
| `down <NNNN>` | Rolls back exactly one version. Needs `PLATFORM_MIGRATE_ALLOW_DOWN=true`. |
| `status` | Prints each version as applied or PENDING and a summary. Takes no lock. |
| `set-app-password` | Sets the `platform_app` login password from `PLATFORM_APP_PASSWORD`. |
| `up-and-set-password` | `up`, then `set-app-password`. The password is checked first, so a missing one applies nothing. |
| `help` | Prints the usage and the variable list. |

Exit status: **0** success. **1** runtime error: the database could not be reached, a migration failed or did not record itself,
the lock timed out, the directory is unreadable or holds no migrations, a gap in the chain. **2** usage or configuration error:
no command, an unknown command, `down` without exactly one version, a missing or invalid variable, a connection string the
driver cannot parse, `down` while `PLATFORM_MIGRATE_ALLOW_DOWN` is not true.

## Files

A migration is a pair `platform_NNNN_<desc>.up.sql` and `platform_NNNN_<desc>.down.sql` (`NNNN` four digits, starting at `0000`,
no gaps). The convention is self-tracking: **the up file INSERTs its own row into `platform.schema_migrations` and the down file
DELETEs it.** The runner never writes that row; it checks that the file did. A file that begins with `platform_` and ends in
`.sql` but does not match the pattern is an error, not something silently skipped, because a migration that never applies is the
same failure as a gap. Other files in the directory are ignored. A directory with no migration files is an error.

Each file runs in one transaction, so it must not contain `BEGIN` or `COMMIT`, and cannot contain `CREATE INDEX CONCURRENTLY`.

## What the runner guarantees

* **One migrator at a time.** The session-level advisory lock, key `7_100_000_001` (the platform's `7_xxx_xxx_xxx` band), is taken
  by a retry loop around `pg_try_advisory_lock` that gives up after `PLATFORM_MIGRATE_LOCK_TIMEOUT_SEC`, is held for the whole of
  `up` or `down`, and is released at the end. The key is per database. `status` takes no lock.
* **No gap.** Before the first file is applied, every version above `0000` must have its predecessor applied or on disk. A gap is
  an error and nothing is applied.
* **One transaction per file**, which begins with `SET LOCAL lock_timeout` and `SET LOCAL statement_timeout` from the two
  variables above. A file that fails is rolled back whole; the files before it stay applied.
* **The file records itself.** After running an up file, before committing, the runner checks that the file inserted its own version
  row. If not, the transaction is rolled back and the error says `migration X did not record itself`.
* **Down is deliberate.** It needs `PLATFORM_MIGRATE_ALLOW_DOWN=true`, refuses a version that is not applied, refuses while a
  higher version is applied (the bootstrap's `DROP SCHEMA ... CASCADE` would destroy the higher objects without their own down
  files running), and checks before committing that the row is gone.
* **No secret in any message.** Driver errors that quote the connection string are replaced by a fixed message
  (`internal/dsnscrub`), `Config` prints both secrets as set or unset, and every error line is passed through a final redaction of
  the connection string, its password and the app password.

## What it does not cover

* The server's own statement log. `set-app-password` sends `ALTER ROLE platform_app LOGIN PASSWORD '...'`, built on the server
  with `format('%L')`; a server that logs statements (`log_statement` set to `ddl` or `all`) records it. The runner cannot change that.
* Whether a migration's SQL is correct. The runner applies what it is given; the database tests in `tests/harness` prove the
  bootstrap and the grants against a real PostgreSQL.
* A transaction pooler. See above: it is documented as unsupported and cannot be detected.
* Pre-flight checks. The platform owns `public.users` and `public.job_items`; the runner does not look for them.
