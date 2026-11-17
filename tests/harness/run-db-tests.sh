#!/usr/bin/env bash
# run-db-tests.sh [go-test-run-regexp] — the harness's DATABASE-ONLY integration tests against a plain Postgres, with no Docker Compose and no module images.
#
# WHAT IT IS FOR. tests/harness/run.sh builds the three modules and runs everything inside a private Docker network (~20 minutes). Most of Phase 1's proofs (grants, the ledger, constraints, the
# balance invariant, the module shapes) need only a real Postgres, the platform-migrate binary and the repository. This script gives exactly that, in minutes:
#   - the `db-tests` workflow runs it on every push (fast feedback, itself admissible evidence: a real Postgres on a GitHub runner);
#   - the `fault-proofs` legs run it with a narrow -run regexp to prove a database gate goes red on a fault.
# It is NOT a substitute for the full harness: tests that need the modules (TestLive_StackCoexists, TestModulesLive_*, TestM3Live_*, TestM1Live_*) are skipped HERE (H_SKIP) and run there. Per-package test-count floors are
# enforced only by the full harness (it runs every test); this script only refuses a run in which NO test ran (a -run regexp that matches nothing is a false green).
#
# Environment: H_PG_ADMIN_DSN (the SUPERUSER DSN on database `postgres`: on the Supabase image that is `supabase_admin`; default postgres://supabase_admin:ci@localhost:5432/postgres) and H_PG_MIGRATOR_DSN (the NON-superuser migrating role `postgres` that an install uses; default postgres://postgres:ci@localhost:5432/postgres), H_SKIP (a go test -skip regexp; default skips the module-dependent tests).
# Needs psql, go, openssl. It creates a database `platform_live_<random>` (the "live" database the tests expect, migrated by the real binary, with a platform_app password) and drops it at the end.
set -u -o pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../.." && pwd)"
RUN="${1:-.}"; ADMIN="${H_PG_ADMIN_DSN:-postgres://supabase_admin:ci@localhost:5432/postgres}"; MIG="${H_PG_MIGRATOR_DSN:-postgres://postgres:ci@localhost:5432/postgres}"; SKIP="${H_SKIP-TestLive_StackCoexists|TestModulesLive|TestM3Live|TestM1Live}"
WORK="$(mktemp -d)"; DB="platform_live_$(openssl rand -hex 4)"
case "$ADMIN" in postgres://*/postgres|postgresql://*/postgres) ;; *) echo "run-db-tests: H_PG_ADMIN_DSN must be a postgres:// URL ending in /postgres (got a different shape)"; exit 2;; esac
case "$MIG" in postgres://*/postgres|postgresql://*/postgres) ;; *) echo "run-db-tests: H_PG_MIGRATOR_DSN must be a postgres:// URL ending in /postgres (got a different shape)"; exit 2;; esac
LIVE="${ADMIN%/postgres}/$DB"; LIVEMIG="${MIG%/postgres}/$DB"; HOSTURL="$ADMIN"
APP_PW="$(openssl rand -hex 16)"
# the password minted above is masked in the (public) Actions log BEFORE anything uses it: a failing test can print a DSN that carries it (pgx's malformed-DSN error embeds the password)
[ "${GITHUB_ACTIONS:-}" = true ] && echo "::add-mask::$APP_PW"
cleanup() { rc=$?; psql "$ADMIN" -X -q -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1; rm -rf "$WORK"; exit $rc; }
trap cleanup EXIT
step() { echo; echo "##[step] $*"; }

step "create the live-like database $DB and apply the auth.users fixture"
psql "$MIG" -X -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE $DB" || { echo "run-db-tests: RED — the migrating role could not create the database"; exit 1; }
# the required install step (db/install/preprovision.sql) as the migrating role, exactly as Docs/install.md says, before any chain
psql "$MIG" -X -q -v ON_ERROR_STOP=1 -f "$ROOT/db/install/preprovision.sql" || { echo "run-db-tests: RED — the required provisioning step failed"; exit 1; }
psql "$LIVEMIG" -X -q -v ON_ERROR_STOP=1 -f "$ROOT/tests/harness/fixtures/auth-users.sql" || { echo "run-db-tests: RED — the auth.users fixture failed"; exit 1; }

step "build platform-migrate and migrate the live database (up-and-set-password)"
EXT=""; case "$(go env GOOS)" in windows) EXT=".exe";; esac
( cd "$ROOT" && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$WORK/platform-migrate$EXT" ./cmd/platform-migrate ) || { echo "run-db-tests: RED — go build ./cmd/platform-migrate failed"; exit 1; }
PLATFORM_MIGRATE_DATABASE_URL="$LIVEMIG" PLATFORM_MIGRATIONS_DIR="$ROOT/db/migrations" PLATFORM_APP_PASSWORD="$APP_PW" "$WORK/platform-migrate$EXT" up-and-set-password || { echo "run-db-tests: RED — platform-migrate up-and-set-password failed"; exit 1; }

step "go test -tags harness -run '$RUN' (skip: ${SKIP:-nothing})"
export HARNESS_ADMIN_DATABASE_URL="$LIVE" HARNESS_ADMIN_DATABASE_HOST_URL="$HOSTURL" HARNESS_PLATFORM_APP_PASSWORD="$APP_PW" HARNESS_MIGRATE_BIN="$WORK/platform-migrate$EXT" HARNESS_REPO_ROOT="$ROOT"
args=(test -tags harness -count=1 -v -timeout 25m -run "$RUN")
[ -z "$SKIP" ] || args+=(-skip "$SKIP")
( cd "$ROOT" && CGO_ENABLED=0 GOTOOLCHAIN=local go "${args[@]}" ./tests/harness ) 2>&1 | tee "$WORK/test.log"
rc=${PIPESTATUS[0]}
ran=$(grep -c '^=== RUN' "$WORK/test.log" || true); passed=$(grep -c -E '^--- PASS' "$WORK/test.log" || true); failed=$(grep -c -E '^--- FAIL' "$WORK/test.log" || true); skipped=$(grep -c -E '^--- SKIP' "$WORK/test.log" || true)
echo; echo "run-db-tests: $ran test(s) started, $passed passed, $failed failed, $skipped skipped (top-level and subtests counted by their --- lines)"
[ "$ran" -ge 1 ] || { echo "run-db-tests: RED — found 0 tests: -run '$RUN' matched nothing, so a green would prove nothing"; exit 1; }
[ "$skipped" -eq 0 ] || { echo "run-db-tests: RED — $skipped test(s) SKIPPED: a test that cannot run FAILS (CLAUDE.md, Phase 0 rule 1)"; exit 1; }
[ "$rc" -eq 0 ] || { echo "run-db-tests: RED — go test exited $rc"; exit 1; }
echo "NOT CHECKED: the module-dependent tests (skipped here, run by the full harness); per-package test-count floors (the full harness enforces them)"
echo "run-db-tests: OK"
