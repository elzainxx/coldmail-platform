#!/usr/bin/env bash
# Rebuild M2's test databases using the SAME steps tests/harness/run.sh uses:
# DROP/CREATE, seed public.users + public.job_items, then the real migrate binary
# (up-and-set-password) for the three templates. send_managed_migtest gets no migration
# (the migration-lifecycle suite applies the chain itself).
set -uo pipefail
cd "<repository root>/coldmail-send-managed"

DC="docker compose"
SUPER="${POSTGRES_SUPERPASS:-devsuperpass}"
export MIGRATIONS_DIR="<repository root>/coldmail-send-managed/db/migrations"
export PREFLIGHT_SQL="<repository root>/coldmail-send-managed/db/preflight/check_platform_tables.sql"
export SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}"

SEED_USERS="CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())"
SEED_JOBS="CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())"

make_db() { # $1=dbname  $2=migrate(yes|no)
  local db="$1" mig="$2"
  echo "== $db =="
  $DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
    -c "DROP DATABASE IF EXISTS $db" -c "CREATE DATABASE $db" || { echo "  CREATE FAILED"; return 1; }
  $DC exec -T postgres psql -U postgres -d "$db" -v ON_ERROR_STOP=1 -q \
    -c "$SEED_USERS" -c "$SEED_JOBS" || { echo "  SEED FAILED"; return 1; }
  if [[ "$mig" == "yes" ]]; then
    if MIGRATE_DATABASE_URL="postgres://postgres:${SUPER}@localhost:5432/$db" \
         tools/send-managed-migrate/bin/send-managed-migrate up-and-set-password; then
      echo "  migrated + app password set"
    else
      echo "  MIGRATE FAILED"; return 1
    fi
  else
    echo "  seeded (no migration by design)"
  fi
}

rc=0
make_db send_managed_migtest     no  || rc=1
make_db send_managed_template    yes || rc=1
make_db send_managed_template_p2 yes || rc=1
make_db send_managed_template_p3 yes || rc=1

echo "=== verification ==="
$DC exec -T postgres psql -U postgres -d postgres -At -c \
  "SELECT datname FROM pg_database WHERE datname LIKE 'send_managed%' ORDER BY 1;"
for db in send_managed_template send_managed_template_p2 send_managed_template_p3; do
  n="$($DC exec -T postgres psql -U postgres -d "$db" -At -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='send_managed';" | tr -d '\r')"
  e="$($DC exec -T postgres psql -U postgres -d "$db" -At -c "SELECT count(DISTINCT t.typname) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='send_managed';" | tr -d '\r')"
  echo "  $db: $n send_managed tables, $e enum types"
done
exit $rc
