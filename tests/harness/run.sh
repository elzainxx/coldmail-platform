#!/usr/bin/env bash
# run.sh — the platform's real-infrastructure harness. Run by .github/workflows/harness.yml on a GitHub runner; a run on any other machine is rehearsal, not evidence.
#
# What it does, in order (each stage prints a banner and the run is RED at the first failed assertion, with the service logs dumped):
#   1  pins      each module checkout is EXACTLY the commit tools/modules.lock pins, shallow-free, with a clean tree
#   2  secrets   per-run secrets, minted with each module's OWN tools (gen-certs / gen-secrets / own-secrets / provision-worker); never printed, never committed
#   3  build     every image built from source with --pull --no-cache (no layer from a previous run can earn a green)
#   4  database  ONE real Postgres; the auth.users fixture, then the platform's (which creates public.users and public.job_items), module 1's, module 2's and module 3's own migrations, in that order
#   5  services  the three module Controls and the platform, started and waited healthy on a network that is `internal` and publishes nothing
#   6  tests     the integration tests (tests/harness, build tag `harness`) compiled on the runner, run INSIDE the network, then the per-package floor gate
#   7  probe     platform-probe calls every module over the private network with the service token (health, readyz, describe) — the Phase 0 end-to-end call
#   8  exposure  no service publishes a port; the network is internal (two independent instruments)
#   9  outage    the platform's /readyz follows the database: 200, then 503 with Postgres stopped, then 200 again
#  10  ledger    the balance invariant: the live ledger trigger is intact (a rolled-back exercise through the ledger leaves no drift and no overdrawn reserve)
# and finally prints what it did NOT check. Nothing here is mocked: every module is the real, unmodified pinned source running as a real service.
set -u
PINS_ONLY=0; [ "${1:-}" = "--pins-only" ] && PINS_ONLY=1   # stage 1 only: used by the workflow to prove the pin check goes red on a wrong checkout
HERE="$(cd "$(dirname "$0")" && pwd)"
PLATFORM_DIR="$(cd "$HERE/../.." && pwd)"
MODULES_ROOT="${H_MODULES_ROOT:-$(cd "$PLATFORM_DIR/.." && pwd)}"
LOCK="$PLATFORM_DIR/tools/modules.lock"
WORK="${H_WORK:-$(mktemp -d)}"; mkdir -p "$WORK/out"
PROJECT_NET="coldmail-platform-harness_private"
M1="$MODULES_ROOT/coldmail-verify"; M2="$MODULES_ROOT/coldmail-send-managed"; M3="$MODULES_ROOT/coldmail-send-byo"
GO_PKG="github.com/coldmail-platform/platform/tests/harness"
DC() { docker compose --project-directory "$HERE" -f "$HERE/docker-compose.yml" --env-file "$WORK/harness.env" --profile probe --profile tests "$@"; }
banner() { echo; echo "================ [$1] $2"; }
dump_logs() { echo "---- service logs (tail) ----"; DC logs --no-color --tail=120 2>&1 | sed 's/^/  | /' || true; DC ps --all 2>&1 | sed 's/^/  | /' || true; }
STAGE="start"
die() { echo; echo "HARNESS RED at stage [$STAGE]: $*"; dump_logs; exit 1; }
cleanup() { rc=$?; DC down -v --remove-orphans -t 10 >/dev/null 2>&1 || true; exit $rc; }
trap cleanup EXIT
mask() { [ "${GITHUB_ACTIONS:-}" = true ] && echo "::add-mask::$1"; return 0; }
lockpin() { tr -d '\r' < "$LOCK" | awk -v r="$1" '/^[[:space:]]*#/ || NF==0 {next} $1==r {print $2}'; }

# ── 1. pins ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=pins; banner 1 "the modules are exactly the pinned commits"
for m in coldmail-verify coldmail-send-managed coldmail-send-byo; do
  d="$MODULES_ROOT/$m"; [ -d "$d/.git" ] || die "$m is not a git checkout at $d"
  want="$(lockpin "$m")"; got="$(git -C "$d" rev-parse HEAD)"
  [ -n "$want" ] || die "$m has no entry in $LOCK"
  [ "$got" = "$want" ] || die "$m is at $got but tools/modules.lock pins $want"
  [ "$(git -C "$d" rev-parse --is-shallow-repository)" = false ] || die "$m is a shallow clone"
  dirty="$(git -C "$d" status --porcelain | wc -l | tr -d ' ')"; [ "$dirty" = 0 ] || die "$m has $dirty modified or untracked paths before the run — it is not the pinned source"
  echo "  $m @ $got  (tree clean, not shallow)"
done

if [ "$PINS_ONLY" = 1 ]; then echo "pins-only: the three modules are exactly the pinned commits"; trap - EXIT; exit 0; fi

# the declared module statements (db/module-statements.txt) still appear in the pinned module sources, and no undeclared file references the platform tables
bash "$PLATFORM_DIR/scripts/check-module-reads.sh" --modules "$MODULES_ROOT" --statements "$PLATFORM_DIR/db/module-statements.txt" || die "the module-reads drift gate is red: a pinned module references public.users / public.job_items in a way db/module-statements.txt does not declare"

# ── 2. secrets ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=secrets; banner 2 "per-run secrets, minted with the modules' own tools"
for m in "$M2" "$M3"; do
  ( cd "$m" && bash tools/gen-certs.sh >/dev/null && bash tools/gen-secrets.sh >/dev/null && bash tools/own-secrets.sh >/dev/null ) || die "the secret tools of $(basename "$m") failed (certs / KMS key / ownership)"
  [ -s "$m/secrets/dev/kms/dev-master.key" ] && [ -s "$m/secrets/dev/certs/control.key" ] || die "$(basename "$m"): the key files are missing after the secret tools ran"
  d="$(git -C "$m" status --porcelain | wc -l | tr -d ' ')"; [ "$d" = 0 ] || die "$(basename "$m"): generating secrets changed $d tracked/untracked paths — they must all be git-ignored"
done
( cd "$M2/tools/provision-worker" && GOTOOLCHAIN=local go build -o "$WORK/provision-worker" . ) || die "could not build module 2's provision-worker (used to mint bcrypt-hashed service tokens)"
mint() { "$WORK/provision-worker" admin-key; }
field() { grep "^$1=" | head -1 | sed "s/^$1=//; s/ .*//"; }
t2="$(mint)"; t3="$(mint)"
M2_TOKEN="$(printf '%s\n' "$t2" | field ADMIN_API_KEY)"; M2_HASH="$(printf '%s\n' "$t2" | field ADMIN_API_KEY_HASH)"
M3_TOKEN="$(printf '%s\n' "$t3" | field ADMIN_API_KEY)"; M3_HASH="$(printf '%s\n' "$t3" | field ADMIN_API_KEY_HASH)"
[ -n "$M2_TOKEN" ] && [ -n "$M2_HASH" ] && [ -n "$M3_TOKEN" ] && [ -n "$M3_HASH" ] || die "provision-worker admin-key printed no token/hash"
[ "$M2_TOKEN" != "$M3_TOKEN" ] || die "the two module service tokens are identical — each module must have its own credential"
M1_KEY="$(openssl rand -hex 32)"; M1_KEY_SHA="$(printf '%s' "$M1_KEY" | sha256sum | cut -d' ' -f1)"
for v in "$M2_TOKEN" "$M3_TOKEN" "$M1_KEY"; do mask "$v"; done
PG_PASS="$(openssl rand -hex 16)"; PL_PASS="$(openssl rand -hex 16)"; M2_PASS="$(openssl rand -hex 16)"; M3_PASS="$(openssl rand -hex 16)"; M1_PEPPER="$(openssl rand -hex 32)"; M1_QUEUE_KEY="$(openssl rand -hex 32)"; M1_APP_PASS="$(openssl rand -hex 16)"
for v in "$PG_PASS" "$PL_PASS" "$M2_PASS" "$M3_PASS" "$M1_PEPPER" "$M1_QUEUE_KEY" "$M1_APP_PASS"; do mask "$v"; done
esc() { printf '%s' "$1" | sed 's/\$/$$/g'; }       # bcrypt hashes contain '$', which compose would interpolate
( umask 077; cat > "$WORK/harness.env" <<EOF
H_PLATFORM_DIR=$PLATFORM_DIR
H_MODULES_ROOT=$MODULES_ROOT
H_WORK=$WORK
H_PG_SUPERPASS=$PG_PASS
H_PLATFORM_APP_PASSWORD=$PL_PASS
H_M1_PEPPER=$M1_PEPPER
H_M1_QUEUE_KEY=$M1_QUEUE_KEY
H_M1_APP_PASSWORD=$M1_APP_PASS
H_M1_CALLER_KEY=$M1_KEY
H_M2_APP_PASSWORD=$M2_PASS
H_M3_APP_PASSWORD=$M3_PASS
H_M2_TOKEN=$M2_TOKEN
H_M3_TOKEN=$M3_TOKEN
H_M2_TOKEN_HASH=$(esc "$M2_HASH")
H_M3_TOKEN_HASH=$(esc "$M3_HASH")
EOF
)
echo "  minted: 5 database/app passwords, 1 pepper, 1 module-1 queue key, 1 module-1 caller key, 2 module service tokens (+ bcrypt hashes), KMS keys and mTLS material for modules 2 and 3 — none printed"

# ── 3. build ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=build; banner 3 "build every image from source, no cache"
DC build --pull --no-cache || die "an image failed to build"
echo "  built from the pinned sources; base images are the digests pinned in each Dockerfile"

# ── 4. database ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=database; banner 4 "one real Postgres, four migration chains"
DC up -d --wait --wait-timeout 120 postgres || die "postgres did not become healthy"
DC run --rm -T fixtures || die "the auth.users fixture could not be created"
for s in migrate-platform migrate-m1 migrate-m2 migrate-m3; do
  echo "  -- $s"; DC run --rm -T "$s" > "$WORK/$s.log" 2>&1; rc=$?; sed 's/^/     | /' "$WORK/$s.log" | tail -25
  [ "$rc" = 0 ] || die "$s exited $rc"
done
m1_want="$(ls "$M1"/control-plane/migrations/[0-9][0-9][0-9][0-9]_*.sql 2>/dev/null | wc -l | tr -d ' ')"
m1_got="$(sed -n 's/.*migrate-m1: applied \([0-9]*\) files.*/\1/p' "$WORK/migrate-m1.log" | tail -1)"
[ "${m1_want:-0}" -ge 1 ] || die "module 1's migration directory has no numbered files — the loop would have applied nothing"
[ "$m1_got" = "$m1_want" ] || die "module 1: the checkout has $m1_want migration files but the loop applied '${m1_got:-none}'"
psql_() { DC exec -T postgres psql -U postgres -d platform -v ON_ERROR_STOP=1 -tAc "$1"; }
n="$(psql_ "select count(*) from pg_namespace where nspname in ('platform','send_managed','send_byo','verify')")"; [ "$n" = 4 ] || die "expected the platform, send_managed, send_byo and verify schemas, found $n of them"
[ "$(psql_ "select to_regclass('verify.api_callers') is not null")" = t ] || die "module 1's verify.api_callers does not exist after its migrations"
psql_ "INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ('harness-caller', decode('$M1_KEY_SHA','hex'), now() - interval '1 hour', '00000000-0000-0000-0000-0000000000a1')" >/dev/null || die "could not seed module 1's per-caller key"
echo "  schemas: platform, send_managed, send_byo, verify present; module 1 applied $m1_got of $m1_want files; one module-1 caller key seeded"
# schema public may hold only the platform's own tables (tests/public-allowlist.txt): module 1 must have left it, and no module may have put anything there
DC exec -T postgres psql -X -At -F '|' -v ON_ERROR_STOP=1 -U postgres -d platform < "$PLATFORM_DIR/scripts/public-schema-listing.sql" > "$WORK/public-listing.txt" || die "the public-schema listing query failed"
bash "$PLATFORM_DIR/scripts/check-public-schema.sh" --allowlist "$PLATFORM_DIR/tests/public-allowlist.txt" --listing "$WORK/public-listing.txt" || die "schema public holds something that is not allowlisted"

# ── 5. services ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=services; banner 5 "the three Controls and the platform, healthy on the private network"
DC up -d --wait --wait-timeout 240 m1 m2 m3 platform || die "a service did not become healthy"
DC ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}' | sed 's/^/  | /'
# the instrument for "module 1 runs as the least-privilege login": a healthy module 1 proves nothing about WHICH role it connected as, so read it from the server
n="$(psql_ "select count(*) from pg_stat_activity where usename = 'verify_app'")"; [ "${n:-0}" -ge 1 ] || die "module 1 holds no connection as verify_app: it is not running as the least-privilege login"
echo "  module 1 holds $n connection(s) as verify_app (pg_stat_activity)"

# ── 6. tests ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=tests; banner 6 "integration tests, compiled on the runner and run inside the network"
( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags harness -c -o "$WORK/out/harness.test" ./tests/harness && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$WORK/out/platform-migrate" ./cmd/platform-migrate ) || die "the harness test binary (or platform-migrate) did not compile"
DC run --rm -T harness-tests -test.list '.*' > "$WORK/h-list.log" 2>&1; lrc=$?
[ "$lrc" = 0 ] || { cat "$WORK/h-list.log"; die "listing the declared integration tests failed ($lrc)"; }
DC run --rm -T harness-tests -test.v -test.count=1 > "$WORK/h-run.log" 2>&1; trc=$?
sed 's/^/  | /' "$WORK/h-run.log" | tail -60
# a compiled test binary prints no `ok <pkg>` line (go test does); the floor gate reads one, so write it from the binary's own exit status
if [ "$trc" = 0 ]; then t="ok  \t$GO_PKG\t0.000s"; else t="FAIL\t$GO_PKG\t0.000s"; fi
printf "$t\n" >> "$WORK/h-run.log"; printf "ok  \t$GO_PKG\t0.000s\n" >> "$WORK/h-list.log"
[ "$trc" = 0 ] || die "the integration tests failed (exit $trc)"
bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$PLATFORM_DIR/tests/floors-harness.txt" --go-run-log "$WORK/h-run.log" --go-list-log "$WORK/h-list.log" || die "the harness test-count floor"

# ── 7. probe ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=probe; banner 7 "platform-probe -> each module, over the private network, with the service token"
DC run --rm -T probe > "$WORK/probe.log" 2>&1; prc=$?
sed 's/^/  | /' "$WORK/probe.log"
[ "$prc" = 0 ] || die "platform-probe exited $prc"
sline="$(grep '^PROBE SUMMARY' "$WORK/probe.log" | tail -1)"; [ -n "$sline" ] || die "platform-probe printed no 'PROBE SUMMARY' line — it may not have run its checks"

# ── 8. exposure ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=exposure; banner 8 "no service publishes a port; the network is internal"
DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres m1 m2 m3 platform || die "a service is reachable from outside the network"
[ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
cnt=0; for id in $ids; do
  pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
  case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
done
echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"

# ── 9. outage ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=outage; banner 9 "the platform's /readyz follows the database"
rz() { DC exec -T platform wget -q -S -O /dev/null http://127.0.0.1:8080/readyz 2>&1 | awk '/HTTP\//{print $2; exit}'; }
wait_for() { want="$1"; i=0; while [ $i -lt 40 ]; do [ "$(rz)" = "$want" ] && return 0; i=$((i+1)); sleep 1; done; return 1; }
wait_for 200 || die "readyz was not 200 with a healthy database"
DC stop postgres >/dev/null 2>&1 || die "could not stop postgres"
wait_for 503 || die "readyz stayed $(rz) with the database stopped — readiness does not follow the database"
DC start postgres >/dev/null 2>&1 || die "could not restart postgres"
wait_for 200 || die "readyz did not return to 200 after the database came back"
echo "  readyz: 200 -> 503 (postgres stopped) -> 200 (postgres back)"

# ── 10. ledger ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=ledger; banner 10 "the balance invariant holds on the live database (rolled-back exercise through the ledger)"
bash "$PLATFORM_DIR/scripts/check-balance-invariant.sh" --psql-cmd "docker compose --project-directory $HERE -f $HERE/docker-compose.yml --env-file $WORK/harness.env --profile probe --profile tests exec -T postgres psql -U postgres -d platform" --exercise || die "the balance invariant gate is red on the live database"

# ── result ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=done
echo
echo "================ NOT CHECKED (what this run did not and could not vouch for)"
cat <<'EOF'
  - workers, the SMTP sink and CoreDNS: not started. No module sent, verified or billed anything; only health, readiness and describe were called.
  - real Supabase: this is stock Postgres 16 with a superuser `postgres`. Supabase's `postgres` role is not a superuser, so the module migrations (CREATE ROLE, OWNER TO)
    are untested against the real project; neither are its pooler, its `auth` schema or its roles.
  - Supabase Auth: auth.users is a minimal FIXTURE (id, email); the real table, GoTrue and the exposed schemas are not here (the supabase workflow has the real auth schema, migrations only).
  - module BEHAVIOUR on the platform's tables beyond the statements the modules issue against public.users / public.job_items (db/module-statements.txt, executed as each module's role in a rolled-back transaction): no job was run end to end (Phase 3).
  - module behaviour beyond health, readiness and describe (Phase 3); module 1 has NO describe at all (Docs/phase-0-spec.md F1) and runs here as the least-privilege login verify_app (since Phase 1, A2).
  - reachability from another host, a firewall or a cloud security group; TLS termination; the modules' mTLS worker ports (they are up, unexercised).
  - that every module commit other than the three pins in tools/modules.lock still works with the platform.
EOF
echo
echo "HARNESS RESULT: GREEN — modules built from $(lockpin coldmail-verify | cut -c1-7) / $(lockpin coldmail-send-managed | cut -c1-7) / $(lockpin coldmail-send-byo | cut -c1-7); $sline"
