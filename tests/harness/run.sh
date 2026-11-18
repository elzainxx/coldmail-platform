#!/usr/bin/env bash
# run.sh — the platform's real-infrastructure harness. Run by .github/workflows/harness.yml on a GitHub runner; a run on any other machine is rehearsal, not evidence.
#
# What it does, in order (each stage prints a banner and the run is RED at the first failed assertion, with the service logs dumped):
#   1  pins      each module checkout is EXACTLY the commit tools/modules.lock pins, shallow-free, with a clean tree
#   2  secrets   per-run secrets, minted with each module's OWN tools (gen-certs / gen-secrets / own-secrets / provision-worker); never printed, never committed
#   3  build     every image built from source with --pull --no-cache (no layer from a previous run can earn a green)
#   4  database  ONE real Postgres (the digest-pinned supabase/postgres: real auth schema, non-superuser migrator `postgres`); the required provisioning step (db/install/preprovision.sql), then the platform's migrations (which create public.users and public.job_items), module 1's, module 2's and module 3's own, in that order, all as `postgres`
#   5  services  the three module Controls and the platform, started and waited healthy on a network that is `internal` and publishes nothing
#   6  tests     the integration tests (tests/harness, build tag `harness`) compiled on the runner, run INSIDE the network, then the per-package floor gate
#   7  probe     platform-probe calls every module over the private network with the service token (health, readyz, describe) — the Phase 0 end-to-end call
#   8  exposure  no service publishes a port; the network is internal (two independent instruments)
#   9  outage    the platform's /readyz follows the database: 200, then 503 with Postgres stopped, then 200 again
#   5b worker    module 3's three workers (each with its own token and box row), the SMTP sink (Mailpit) and the controllable sink (smtp-ctl: it rejects, defers and delays on command): module 3 really sends, on several lanes, to a sink that can be hostile
#   5c m2-worker module 2's REAL worker (Phase 3, B8; part of `full` since FULL_INCLUDES names it, and the only stage of its scope otherwise): its credentials minted in stage 2 with module 2's own tools, its box row seeded before it starts, a heartbeat awaited; module 2 really sends (sent, bounced, failed/attempts_exhausted, cancelled) to the controllable sink (tests/harness/m2worker/stage.sh)
#   scope        H_SCOPE=m2worker runs stages 1-4b, then only module 2's Control, its worker, the sink, the TestM2Worker tests and the exposure check (m2w_scope_run); H_SCOPE=full (the default) runs everything except the dormant stages
#   9b m3-outage module 3 stopped while an entitlement expires: the platform retries, claims no convergence it did not see, converges when module 3 is back
#   9b-adapter   module 3's Domain A API stopped in the middle of a campaign of the adapter's (Phase 3, B12; tests/harness/m3adapter/outage.sh): three test phases with the stop and the start between them, nothing settled or invented, the campaign completes exactly once
#   9c leader    TWO platform instances on one database: exactly one holds the leader lock and runs the loops (the other runs none); dated entitlements expire exactly once; the leader is killed, the other takes over; the killed one comes back as the standby
#   4b first admin   the documented platform-migrate create-first-admin, as postgres, on the real auth schema; a second run refused
#  10  ledger    the balance invariant: the live ledger trigger is intact (a rolled-back exercise through the ledger leaves no drift and no overdrawn reserve)
# and finally prints what it did NOT check. Nothing here is mocked: every module is the real, unmodified pinned source running as a real service.
set -u
PINS_ONLY=0; [ "${1:-}" = "--pins-only" ] && PINS_ONLY=1   # stage 1 only: used by the workflow to prove the pin check goes red on a wrong checkout
# H_SCOPE (the workflow's `scope` input): full = every stage; m2worker = Postgres + the migrations + module 2 + its real worker + the controllable sink + only the TestM2Worker tests (tests/harness/m2worker/stage.sh, m2w_scope_run);
# m1node = module 1's real node (its own stage file). A scope builds and starts only what it needs: BUILD_ONLY is the list stage 3 builds (empty = every service).
SCOPE="${H_SCOPE:-full}"
case "$SCOPE" in full|m1node|m2worker) ;; *) echo "H_SCOPE must be full, m1node or m2worker (got '$SCOPE')"; exit 2;; esac
BUILD_ONLY=""
case "$SCOPE" in
  m2worker) BUILD_ONLY="migrate-platform migrate-m1 migrate-m2 migrate-m3 m2 m2-worker smtp-ctl";;
  m1node) BUILD_ONLY="migrate-platform migrate-m1 migrate-m2 migrate-m3 m1n fx verifier-node";;   # stage 4 runs every migration chain; module 1's own control plane is `m1n` in this scope (docker-compose.m1node.yml)
esac
# FULL_INCLUDES: the optional stages the `full` run ALSO runs, once a runner has proven each in its own scope. It was EMPTY until the first scoped runs were green: a stage of a scope is DORMANT in `full` (it does not compile into the test binary, add a compose
# file, start a container, mint a credential or print a line), so the full harness is exactly what it was before the stage existed. (Both stages have since been proven and included, see the value below.) THE ONE PLACE TO FLIP: name the stage here (FULL_INCLUDES="m2worker m1node"); a stage
# that adds tests also needs tests/floors-harness.txt raised by its test count at the same time (they then compile into the full binary). Known names: m2worker (tests/harness/m2worker/stage.sh), m1node (tests/harness/m1node).
FULL_INCLUDES="m2worker m1node"   # both proven in their own scope on the runner at aeb29ed: m1node run 44183527048 (9 TestM1Node tests), m2worker run 44183533432 (5 TestM2Worker tests)
HERE="$(cd "$(dirname "$0")" && pwd)"
PLATFORM_DIR="$(cd "$HERE/../.." && pwd)"
MODULES_ROOT="${H_MODULES_ROOT:-$(cd "$PLATFORM_DIR/.." && pwd)}"
LOCK="$PLATFORM_DIR/tools/modules.lock"
WORK="${H_WORK:-$(mktemp -d)}"; mkdir -p "$WORK/out"
PROJECT_NET="coldmail-platform-harness_private"
M1="$MODULES_ROOT/coldmail-verify"; M2="$MODULES_ROOT/coldmail-send-managed"; M3="$MODULES_ROOT/coldmail-send-byo"
GO_PKG="github.com/coldmail-platform/platform/tests/harness"
# The module-1 node stack (tests/harness/m1node, docker-compose.m1node.yml, tests/harness/m1_node_test.go behind the build tag m1node) is DORMANT: its compose file is merged, its secrets are minted, its stages run and its tests are compiled
# ONLY under H_SCOPE=m1node, or in the full run once `m1node` is named in FULL_INCLUDES (the lead adds it there after a runner run of scope m1node is green). M1N_ON is 1 exactly then.
M1N_ON=0; M1N_FILES=()
case "$SCOPE" in m1node) M1N_ON=1;; esac
case " ${FULL_INCLUDES:-} " in *" m1node "*) [ "$SCOPE" = full ] && M1N_ON=1;; esac
[ "$M1N_ON" = 1 ] && M1N_FILES=(-f "$HERE/docker-compose.m1node.yml")
# Module 2's real worker (tests/harness/m2worker, docker-compose.m2worker.yml, tests/harness/m2_worker_test.go behind the build tag m2worker) is DORMANT in the same way: its compose file (the operator surface of module 2 and the
# m2-worker service, profile m2worker), its credentials, stage 5c, the evidence and PASS-line checks, the exposure of m2-worker and the build tag m2worker of the test binary exist ONLY under H_SCOPE=m2worker, or in the full run once
# `m2worker` is named in FULL_INCLUDES. M2W_ON is 1 exactly then. TEST_TAGS is the build tags of the test binary: `harness` alone in `full`; a scope adds its own tag (m2worker here; the m1node stage compiles its own).
M2W_ON=0; M2W_DC=(); M2W_EXPOSE=""; TEST_TAGS="harness"
case "$SCOPE" in m2worker) M2W_ON=1;; esac
case " ${FULL_INCLUDES:-} " in *" m2worker "*) [ "$SCOPE" = full ] && M2W_ON=1;; esac
[ "$M2W_ON" = 1 ] && { M2W_DC=(-f "$HERE/docker-compose.m2worker.yml" --profile m2worker); M2W_EXPOSE="m2-worker"; TEST_TAGS="$TEST_TAGS m2worker"; }
DC() { docker compose --project-directory "$HERE" -f "$HERE/docker-compose.yml" ${M2W_DC[@]+"${M2W_DC[@]}"} ${M1N_FILES[@]+"${M1N_FILES[@]}"} --env-file "$WORK/harness.env" --profile probe --profile tests "$@"; }
banner() { echo; echo "================ [$1] $2"; }
# drift_evidence LOG: the contract drift gate's own output (internal/contractcheck Session, tests TestM*_ZContractDrift): everything the session printed, whole (the differences, the undeclared calls, the contracts no response reached with the reason
# of each allowance, the coverage and what it cannot see). In record mode (CONTRACT_RECORD=1, the dispatch input contract_record) the session prints each CONTRACT-FIXTURE block as test log lines: those are printed RAW (no prefix) so that
# cmd/contract-fixtures can read them back out of the downloaded workflow log. Always returns 0: the verdict is the test's.
drift_evidence() {
  echo "  ---- the contract drift gate (internal/contractcheck): what each session compared and what it did not reach ----"
  awk '/^=== RUN +Test[A-Za-z0-9]+_ZContractDrift/{r=1} r && /CONTRACT-FIXTURE BEGIN/{b=1} b{print; if (/CONTRACT-FIXTURE END/) b=0; next} r{print "  | " substr($0,1,600)} /^--- (PASS|FAIL|SKIP): Test[A-Za-z0-9]+_ZContractDrift/{r=0}' "$1"
  return 0
}
dump_logs() { echo "---- service logs (tail) ----"; DC logs --no-color --tail=120 2>&1 | sed 's/^/  | /' || true; DC ps --all 2>&1 | sed 's/^/  | /' || true; }
STAGE="start"
die() { echo; echo "HARNESS RED at stage [$STAGE]: $*"; dump_logs; exit 1; }
cleanup() { rc=$?; DC down -v --remove-orphans -t 10 >/dev/null 2>&1 || true; exit $rc; }
trap cleanup EXIT
mask() { [ "${GITHUB_ACTIONS:-}" = true ] && echo "::add-mask::$1"; return 0; }
lockpin() { tr -d '\r' < "$LOCK" | awk -v r="$1" '/^[[:space:]]*#/ || NF==0 {next} $1==r {print $2}'; }
. "$HERE/m2worker/stage.sh"   # module 2's real worker (Phase 3, chunk B8): functions only (m2w_mint, m2w_stage_5c, m2w_evidence, m2w_require_pass, m2w_secret_leak, m2w_scope_run, m2w_not_checked)
. "$HERE/m1node/m1node.sh"   # module 1's real verifier node, its fixtures and a wired control plane (Phase 3, chunk B7): functions only (m1n_mint, m1n_stage1..4, m1n_exposure, m1n_run, m1n_run_scope, m1n_not_checked)
. "$HERE/m3adapter/outage.sh"   # module 3's adapter outage proof (Phase 3, chunk B12): functions only (m3a_evidence, m3a_outage and the steps it calls); called once, after stage 9b

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
# module 3's two further credentials (Phase 2, chunk B8): the WORKER's token (module 3's worker registers with it; its bcrypt hash is seeded into send_byo.boxes) and module 3's ADMIN key (the per-user suspend route is
# Domain B, a DIFFERENT credential from the service token above): both minted with module 2's provision-worker like the tokens, never printed
t3w="$(mint)"; t3a="$(mint)"
M3_WORKER_TOKEN="$(printf '%s\n' "$t3w" | field ADMIN_API_KEY)"; M3_WORKER_HASH="$(printf '%s\n' "$t3w" | field ADMIN_API_KEY_HASH)"
M3_ADMIN_KEY="$(printf '%s\n' "$t3a" | field ADMIN_API_KEY)"; M3_ADMIN_HASH="$(printf '%s\n' "$t3a" | field ADMIN_API_KEY_HASH)"
M3_WORKER_ID="$(cat /proc/sys/kernel/random/uuid)"
SINK_PASS="$(openssl rand -hex 12)"      # the placeholder SMTP-sink password of the live module-3 test: random per run, never a literal in the tree (the sink accepts any credential)
[ -n "$M3_WORKER_TOKEN" ] && [ -n "$M3_WORKER_HASH" ] && [ -n "$M3_ADMIN_KEY" ] && [ -n "$M3_ADMIN_HASH" ] && [ -n "$M3_WORKER_ID" ] || die "could not mint module 3's worker token and admin key"
[ "$M3_ADMIN_KEY" != "$M3_TOKEN" ] && [ "$M3_ADMIN_KEY" != "$M3_WORKER_TOKEN" ] || die "module 3's admin key must differ from its service token and its worker token"
# two more module-3 workers (Phase 2 close-out, E2: a user with three lanes needs three workers, because module 3 pushes one job per worker at a time): each has an id, a token and (stage 5b) a box row of its own,
# minted exactly like the first worker's; the tokens are masked at once and never printed. The sink runs as the user that owns module 3's dev sink key (compose `user:`), so the harness user's ids go in the env file.
t3w2="$(mint)"; t3w3="$(mint)"
M3_WORKER2_TOKEN="$(printf '%s\n' "$t3w2" | field ADMIN_API_KEY)"; M3_WORKER2_HASH="$(printf '%s\n' "$t3w2" | field ADMIN_API_KEY_HASH)"
M3_WORKER3_TOKEN="$(printf '%s\n' "$t3w3" | field ADMIN_API_KEY)"; M3_WORKER3_HASH="$(printf '%s\n' "$t3w3" | field ADMIN_API_KEY_HASH)"
M3_WORKER2_ID="$(cat /proc/sys/kernel/random/uuid)"; M3_WORKER3_ID="$(cat /proc/sys/kernel/random/uuid)"
HOST_UID="$(id -u)"; HOST_GID="$(id -g)"
[ -n "$M3_WORKER2_TOKEN" ] && [ -n "$M3_WORKER2_HASH" ] && [ -n "$M3_WORKER3_TOKEN" ] && [ -n "$M3_WORKER3_HASH" ] && [ -n "$M3_WORKER2_ID" ] && [ -n "$M3_WORKER3_ID" ] || die "could not mint the credentials of module 3's second and third workers"
for v in "$M3_WORKER2_TOKEN" "$M3_WORKER3_TOKEN"; do mask "$v"; done
[ -n "$HOST_UID" ] && [ -n "$HOST_GID" ] || die "could not read the harness user's uid and gid (the controllable sink runs as the user that owns module 3's dev sink key)"
[ "$M3_WORKER2_TOKEN" != "$M3_WORKER_TOKEN" ] && [ "$M3_WORKER3_TOKEN" != "$M3_WORKER_TOKEN" ] && [ "$M3_WORKER2_TOKEN" != "$M3_WORKER3_TOKEN" ] || die "module 3's three workers must have three different tokens"
[ "$M3_WORKER2_ID" != "$M3_WORKER_ID" ] && [ "$M3_WORKER3_ID" != "$M3_WORKER_ID" ] && [ "$M3_WORKER2_ID" != "$M3_WORKER3_ID" ] || die "module 3's three workers must have three different ids"
[ "$M3_ADMIN_KEY" != "$M3_WORKER2_TOKEN" ] && [ "$M3_ADMIN_KEY" != "$M3_WORKER3_TOKEN" ] && [ "$M3_TOKEN" != "$M3_WORKER2_TOKEN" ] && [ "$M3_TOKEN" != "$M3_WORKER3_TOKEN" ] || die "a module 3 worker token must differ from its admin key and its service token"
M1_KEY="$(openssl rand -hex 32)"; M1_KEY_SHA="$(printf '%s' "$M1_KEY" | sha256sum | cut -d' ' -f1)"
for v in "$M2_TOKEN" "$M3_TOKEN" "$M1_KEY" "$M3_WORKER_TOKEN" "$M3_ADMIN_KEY" "$SINK_PASS"; do mask "$v"; done
PG_PASS="$(openssl rand -hex 16)"; PL_PASS="$(openssl rand -hex 16)"; M2_PASS="$(openssl rand -hex 16)"; M3_PASS="$(openssl rand -hex 16)"; M1_PEPPER="$(openssl rand -hex 32)"; M1_QUEUE_KEY="$(openssl rand -hex 32)"; M1_APP_PASS="$(openssl rand -hex 16)"
for v in "$PG_PASS" "$PL_PASS" "$M2_PASS" "$M3_PASS" "$M1_PEPPER" "$M1_QUEUE_KEY" "$M1_APP_PASS"; do mask "$v"; done
# the bcrypt hashes of the tokens above are not credentials (a hash of 32 random bytes), but a failed INSERT or a service log could print one into a public log: mask them too
for v in "$M2_HASH" "$M3_HASH" "$M3_WORKER_HASH" "$M3_ADMIN_HASH" "$M3_WORKER2_HASH" "$M3_WORKER3_HASH"; do mask "$v"; done
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
H_M3_WORKER_ID=$M3_WORKER_ID
H_M3_WORKER_TOKEN=$M3_WORKER_TOKEN
H_M3_WORKER2_ID=$M3_WORKER2_ID
H_M3_WORKER2_TOKEN=$M3_WORKER2_TOKEN
H_M3_WORKER3_ID=$M3_WORKER3_ID
H_M3_WORKER3_TOKEN=$M3_WORKER3_TOKEN
H_HOST_UID=$HOST_UID
H_HOST_GID=$HOST_GID
H_M3_ADMIN_KEY=$M3_ADMIN_KEY
H_SINK_PASSWORD=$SINK_PASS
H_M3_ADMIN_KEY_HASH=$(esc "$M3_ADMIN_HASH")
EOF
)
echo "  minted: 5 database/app passwords, 1 pepper, 1 module-1 queue key, 1 module-1 caller key, 2 module service tokens (+ bcrypt hashes), module 3's admin key and its three workers' tokens (+ hashes, ids), KMS keys and mTLS material for modules 2 and 3 — none printed"
[ "$M2W_ON" = 1 ] && m2w_mint   # module 2's real worker (only when it is on): the operator key and the worker's id and token (+ hashes), minted with module 2's own provision-worker, masked, appended to the env file (tests/harness/m2worker/stage.sh)
[ "$M1N_ON" = 1 ] && m1n_mint   # module 1's node stack (only when it is on): the node's token (masked), a per-run CA and the api.ipify.org certificate (keys mode 600, never printed), two resolv.conf files, the fixture network prefix; appended to the env file (tests/harness/m1node/m1node.sh)

# ── 3. build ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=build; banner 3 "build every image from source, no cache"
DC build --pull --no-cache $BUILD_ONLY || die "an image failed to build"
echo "  built from the pinned sources${BUILD_ONLY:+ (scope $SCOPE: only $BUILD_ONLY)}; base images are the digests pinned in each Dockerfile"

# ── 4. database ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=database; banner 4 "one real Postgres (supabase/postgres), the required provisioning step, four migration chains — all as the non-superuser postgres"
DC up -d --wait --wait-timeout 300 postgres || die "postgres did not become healthy"
# the instruments, BEFORE anything runs on this database: it is the image a real install runs on, the migrating role is not a superuser (so every privilege failure is visible), the auth schema
# is the image's REAL one (not a fixture), and the integration tests' superuser identity works over TCP with the same password (it is how the tests prove a refusal bites)
shape="$(DC exec -T postgres psql -X -At -F '|' -U postgres -d postgres -c "select rolsuper, rolcreaterole from pg_roles where rolname = current_user")" || die "could not read the migrating role"
[ "$shape" = "f|t" ] || die "the migrating role postgres has (rolsuper, rolcreaterole) = ($shape), expected (f|t): this is not the Supabase shape, and a superuser would hide every privilege failure"
[ "$(DC exec -T postgres psql -X -At -U postgres -d postgres -c "select count(*) from information_schema.columns where table_schema='auth' and table_name='users' and column_name='encrypted_password'")" = 1 ] || die "auth.users is not Supabase's own table (no encrypted_password column): the harness must run on the real auth schema, not a fixture"
sa="$(DC exec -T postgres psql -X -At -U supabase_admin -d postgres -c "select rolsuper from pg_roles where rolname = current_user")" || die "the image's superuser supabase_admin cannot connect"
[ "$sa" = t ] || die "supabase_admin is not a superuser ($sa): the integration tests need one"
echo "  instruments: migrator postgres is (rolsuper, rolcreaterole) = (f, t); auth.users is the image's own table; supabase_admin is a superuser"
echo "  -- preprovision (db/install/preprovision.sql, the required install step)"; DC run --rm -T preprovision || die "the required provisioning step (db/install/preprovision.sql) failed"
psql_() { DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }
for s in migrate-platform migrate-m1 migrate-m1-password migrate-m2 migrate-m3; do
  echo "  -- $s"; DC run --rm -T "$s" > "$WORK/$s.log" 2>&1; rc=$?; sed 's/^/     | /' "$WORK/$s.log" | tail -25
  [ "$rc" = 0 ] || die "$s exited $rc"
done
m1_want="$(ls "$M1"/control-plane/migrations/[0-9][0-9][0-9][0-9]_*.sql 2>/dev/null | wc -l | tr -d ' ')"
[ "${m1_want:-0}" -ge 1 ] || die "module 1's migration directory has no numbered files — the runner would have applied nothing"
# module 1's own runner (cmd/migrate): the first run applied every file; a SECOND run of the full chain must be a no-op (the owner's ruling), and say so
m1_line="$(grep -E '^migrate: applied [0-9]+, adopted [0-9]+, already applied [0-9]+, total [0-9]+' "$WORK/migrate-m1.log" | tail -1)"
[ -n "$m1_line" ] || die "module 1's runner printed no 'migrate: applied N, adopted M, already applied K, total T' summary line"
m1_got="$(printf '%s\n' "$m1_line" | sed -n 's/^migrate: applied \([0-9]*\), adopted.*/\1/p')"
[ "$m1_got" = "$m1_want" ] || die "module 1: the checkout has $m1_want migration files but the runner applied '${m1_got:-none}' ($m1_line)"
echo "  -- migrate-m1 again (the full chain twice must be a no-op)"; DC run --rm -T migrate-m1 > "$WORK/migrate-m1-2.log" 2>&1 || { cat "$WORK/migrate-m1-2.log"; die "the second run of module 1's runner failed"; }
grep -qE "^migrate: applied 0, adopted 0, already applied $m1_want, total $m1_want" "$WORK/migrate-m1-2.log" || die "the second run of module 1's runner was not a no-op: $(grep '^migrate:' "$WORK/migrate-m1-2.log" | tail -1)"
[ "$(psql_ "select count(*) from verify.schema_migrations")" = "$m1_want" ] || die "verify.schema_migrations does not hold exactly $m1_want rows after the two runs"
[ "$(psql_ "select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname = 'public' and c.relname in ('verifications', 'cache', 'jobs')")" = 0 ] || die "module 1's tables are back in public after the second run: the runner replayed 0001-0015"
n="$(psql_ "select count(*) from pg_namespace where nspname in ('platform','send_managed','send_byo','verify')")"; [ "$n" = 4 ] || die "expected the platform, send_managed, send_byo and verify schemas, found $n of them"
[ "$(psql_ "select to_regclass('verify.api_callers') is not null")" = t ] || die "module 1's verify.api_callers does not exist after its migrations"
psql_ "INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ('harness-caller', decode('$M1_KEY_SHA','hex'), now() - interval '1 hour', '00000000-0000-0000-0000-0000000000a1')" >/dev/null || die "could not seed module 1's per-caller key"
echo "  schemas: platform, send_managed, send_byo, verify present; module 1 applied $m1_got of $m1_want files and a second run was a no-op; one module-1 caller key seeded"
# schema public may hold only the platform's own tables (tests/public-allowlist.txt): module 1 must have left it, and no module may have put anything there
DC exec -T postgres psql -X -At -F '|' -v ON_ERROR_STOP=1 -U postgres -d postgres < "$PLATFORM_DIR/scripts/public-schema-listing.sql" > "$WORK/public-listing.txt" || die "the public-schema listing query failed"
bash "$PLATFORM_DIR/scripts/check-public-schema.sh" --allowlist "$PLATFORM_DIR/tests/public-allowlist.txt" --listing "$WORK/public-listing.txt" || die "schema public holds something that is not allowlisted"

# ── 4b. first admin ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The documented install step (Docs/install.md step 4), run for real: as the non-superuser `postgres`, against the image's REAL auth.users, with a login that exists only in auth.users.
STAGE=first-admin; banner "4b" "platform-migrate create-first-admin, as documented, on the live database"
FA_ID="$(cat /proc/sys/kernel/random/uuid)"
DC exec -T postgres psql -X -v ON_ERROR_STOP=1 -U supabase_admin -d postgres -c "INSERT INTO auth.users (id, email) VALUES ('$FA_ID', 'Harness.Admin@example.test')" >/dev/null || die "could not create the Supabase login the first admin needs (a row in the image's auth.users)"
DC run --rm -T migrate-platform create-first-admin --auth-user-id "$FA_ID" --display-name "Harness Admin" > "$WORK/first-admin.log" 2>&1; rc=$?; sed 's/^/     | /' "$WORK/first-admin.log" | tail -5
[ "$rc" = 0 ] || die "create-first-admin exited $rc on the live database"
grep -q "created admin $FA_ID (Harness Admin)" "$WORK/first-admin.log" || die "create-first-admin did not print its success line"
[ "$(psql_ "select count(*) from platform.admins where id = '$FA_ID' and display_name = 'Harness Admin'")" = 1 ] || die "the admin row was not created"
[ "$(psql_ "select email from public.users where id = '$FA_ID' and auth_user_id = '$FA_ID'")" = "harness.admin@example.test" ] || die "the public.users row is missing or its email was not normalised"
if DC run --rm -T migrate-platform create-first-admin --auth-user-id "$FA_ID" --display-name "Harness Admin" > "$WORK/first-admin-2.log" 2>&1; then die "a SECOND create-first-admin succeeded: the command must refuse when an admin exists"; fi
grep -qi "admin already exists\|already exists" "$WORK/first-admin-2.log" || die "the second create-first-admin failed, but not for the reason it must name (an admin already exists): $(tail -2 "$WORK/first-admin-2.log")"
[ "$(psql_ "select count(*) from platform.admins")" = 1 ] || die "the refused second run changed platform.admins"
echo "  first admin created as postgres on the real auth schema; a second run refused; platform.admins holds exactly one row"
[ "$SCOPE" = m2worker ] && m2w_scope_run   # H_SCOPE=m2worker: module 2's Control, its worker (5c), the TestM2Worker tests and the exposure check, then the end of the run: it never returns
[ "$SCOPE" = m1node ] && m1n_run_scope     # H_SCOPE=m1node (M1N_ON is 1): the fixtures, module 1's real node, a wired control plane and the TestM1Node tests (stages M1N-1 to M1N-5), then the end of the run: it never returns

# ── 5. services ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=services; banner 5 "the three Controls and the platform (two instances of it), healthy on the private network"
DC up -d --wait --wait-timeout 240 m1 m2 m3 platform platform-2 || die "a service did not become healthy"
DC ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}' | sed 's/^/  | /'
# the instrument for "module 1 runs as the least-privilege login": a healthy module 1 proves nothing about WHICH role it connected as, so read it from the server
n="$(psql_ "select count(*) from pg_stat_activity where usename = 'verify_app'")"; [ "${n:-0}" -ge 1 ] || die "module 1 holds no connection as verify_app: it is not running as the least-privilege login"
echo "  module 1 holds $n connection(s) as verify_app (pg_stat_activity)"

# ── 5b. module 3's worker and an SMTP sink ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Phase 2 (chunks B8, B9): module 3 must REALLY send for the entitlement proofs to mean anything. Its worker registers with a token whose bcrypt hash is seeded into send_byo.boxes BEFORE the worker starts
# (a worker that is rejected exits), the From-gate is switched off (module 3's own harness does the same: no DNS needed to send), and the sink is Mailpit, which accepts any credential and keeps what it received.
STAGE=m3-worker; banner "5b" "module 3's three workers (own token, own box row each), the SMTP sink and the controllable sink"
psql_su() { DC exec -T postgres psql -U supabase_admin -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }
psql_su "UPDATE send_byo.config SET value = '{\"v\":\"off\"}' WHERE key = 'from_gate_mode'" >/dev/null || die "could not switch module 3's From-gate off"
t0="$(psql_su "select clock_timestamp()")"; [ -n "$t0" ] || die "could not read the database clock"
psql_su "INSERT INTO send_byo.boxes (id, user_id, token_hash, status) VALUES ('$M3_WORKER_ID', '00000000-0000-0000-0000-000000000000', '$M3_WORKER_HASH', 'active')" >/dev/null || die "could not seed module 3's worker box row"
# the second and third workers (E2: several lanes): their box rows are seeded exactly like the first's, before they start
psql_su "INSERT INTO send_byo.boxes (id, user_id, token_hash, status) VALUES ('$M3_WORKER2_ID', '00000000-0000-0000-0000-000000000000', '$M3_WORKER2_HASH', 'active')" >/dev/null || die "could not seed module 3's second worker box row"
psql_su "INSERT INTO send_byo.boxes (id, user_id, token_hash, status) VALUES ('$M3_WORKER3_ID', '00000000-0000-0000-0000-000000000000', '$M3_WORKER3_HASH', 'active')" >/dev/null || die "could not seed module 3's third worker box row"
DC up -d m3-worker m3-worker-2 m3-worker-3 smtp-sink smtp-ctl || die "module 3's workers, the SMTP sink or the controllable sink did not start"
i=0; while [ $i -lt 60 ]; do
  [ "$(psql_su "select count(*) from send_byo.boxes where id = '$M3_WORKER_ID' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
  i=$((i+1)); sleep 2
done
[ "$i" -lt 60 ] || die "module 3's worker never heartbeated after $t0 (it registers over mTLS with its token; its logs are below)"
for wid in "$M3_WORKER2_ID" "$M3_WORKER3_ID"; do
  i=0; while [ $i -lt 60 ]; do
    [ "$(psql_su "select count(*) from send_byo.boxes where id = '$wid' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
    i=$((i+1)); sleep 2
  done
  [ "$i" -lt 60 ] || die "module 3's worker $wid never heartbeated after $t0 (it registers over mTLS with its token; its logs are below)"
done
[ "$(psql_su "select count(*) from send_byo.boxes where id in ('$M3_WORKER_ID', '$M3_WORKER2_ID', '$M3_WORKER3_ID') and status = 'active' and last_heartbeat_at > '$t0'")" = 3 ] || die "not all three of module 3's workers are registered and heartbeating"
i=0; while [ $i -lt 30 ]; do DC exec -T platform wget -q -O /dev/null http://smtp-sink:8025/api/v1/info 2>/dev/null && break; i=$((i+1)); sleep 2; done
[ "$i" -lt 30 ] || die "the SMTP sink's API did not answer"
# the controllable sink: ready means its SMTP listener accepts connections (it exits at start when it cannot read module 3's dev sink certificate and key: its log is below)
i=0; while [ $i -lt 30 ]; do DC exec -T platform wget -q -O /dev/null http://smtp-ctl:8025/readyz 2>/dev/null && break; i=$((i+1)); sleep 2; done
[ "$i" -lt 30 ] || die "the controllable SMTP sink (smtp-ctl) did not answer /readyz"
echo "  module 3's three workers are registered and heartbeating; the SMTP sink and the controllable sink answer; From-gate off"
[ "$M2W_ON" = 1 ] && m2w_stage_5c   # module 2's real worker (only when it is on): its box row seeded, then the worker started and a heartbeat awaited (tests/harness/m2worker/stage.sh)

# ── 6. tests ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=tests; banner 6 "integration tests, compiled on the runner and run inside the network"
( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags "$TEST_TAGS" -c -o "$WORK/out/harness.test" ./tests/harness && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$WORK/out/platform-migrate" ./cmd/platform-migrate ) || die "the harness test binary (or platform-migrate) did not compile"
DC run --rm -T harness-tests -test.list '.*' > "$WORK/h-list.log" 2>&1; lrc=$?
[ "$lrc" = 0 ] || { cat "$WORK/h-list.log"; die "listing the declared integration tests failed ($lrc)"; }
DC run --rm -T harness-tests -test.v -test.count=1 -test.timeout=40m > "$WORK/h-run.log" 2>&1; trc=$?
sed 's/^/  | /' "$WORK/h-run.log" | tail -60
# the live module-3 proof logs its numbers (sink counts, bounds, timestamps): print them whole, they are the evidence the tail above can cut off
echo "  ---- the live module-3 proof: the test's own log lines ----"; grep -E " m3_(live|hostile)(_helpers)?_test.go:[0-9]+: " "$WORK/h-run.log" | sed 's/^/  | /' | head -160
# the live module-1 proof (close-out D2) logs what its job looked like (the module's own report, the oracle's decisions, the distribution, how many pages were settled): print it whole
echo "  ---- the live module-1 proof: the test's own log lines ----"; grep -E " m1_bulk_settle_test.go:[0-9]+: " "$WORK/h-run.log" | sed 's/^/  | /' | head -80
# module 2's real worker (Phase 3, B8): the test's own log lines, among them the sink's per-recipient counts, the timings and the job rows
[ "$M2W_ON" = 1 ] && m2w_evidence "$WORK/h-run.log"
drift_evidence "$WORK/h-run.log"   # the contract drift sessions of module 2 and module 3 (and, in record mode, the recorded fixtures): Phase 3, chunk B6
# module 3's adapter (Phase 3, B9 to B11 and the control-only runs of the three B12 phases): the tests' own NOTE lines (counts, ledger rows, sink counters, positions), tests/harness/m3adapter/FIRST-RUN.md says what to look for
echo "  ---- module 3's adapter: the test's own NOTE lines ----"; m3a_evidence "$WORK/h-run.log" 400
# a failure must name itself: the tail above is only the end of the run, so print every failing test with its context
[ "$trc" = 0 ] || { echo "  ---- failing tests (with context) ----"; grep -n -B1 -A12 -e '--- FAIL' "$WORK/h-run.log" | sed 's/^/  | /' | head -200; }
# a compiled test binary prints no `ok <pkg>` line (go test does); the floor gate reads one, so write it from the binary's own exit status
if [ "$trc" = 0 ]; then t="ok  \t$GO_PKG\t0.000s"; else t="FAIL\t$GO_PKG\t0.000s"; fi
printf "$t\n" >> "$WORK/h-run.log"; printf "ok  \t$GO_PKG\t0.000s\n" >> "$WORK/h-list.log"
[ "$trc" = 0 ] || die "the integration tests failed (exit $trc)"
# the live module-1 test must be among the tests that PASSED in the log the run itself produced (a binary built without it, or a -run that skipped it, would leave the rest green)
grep -q '^--- PASS: TestM1Live_BulkJobSettlesItemByItem ' "$WORK/h-run.log" || die "TestM1Live_BulkJobSettlesItemByItem (a real module-1 bulk job settled item by item, compared with the module's own report) has no '--- PASS' line in the run log: it did not run, or did not pass"
[ "$M2W_ON" = 1 ] && m2w_outage   # (only when module 2's real worker is on) B12: module 2's API stopped in the middle of a campaign and started again, three phases (tests/harness/m2worker/stage.sh; proven in scope m2worker, run 44206820889); it puts module 2 and its worker back before the stages that follow
[ "$M2W_ON" = 1 ] && m2w_require_pass "$WORK/h-run.log"   # (only when module 2's real worker is on) every TestM2Worker_* test has a '--- PASS' line (module 2 really sent, bounced, exhausted and cancelled)
bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$PLATFORM_DIR/tests/floors-harness.txt" --go-run-log "$WORK/h-run.log" --go-list-log "$WORK/h-list.log" || die "the harness test-count floor"
[ "$M2W_ON" = 1 ] && m2w_secret_leak   # (only when module 2's real worker is on) none of module 2's credentials of this run, nor the sink password, is in the logs of m2, m2-worker or smtp-ctl

# ── 7. probe ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=probe; banner 7 "platform-probe -> each module, over the private network, with the service token"
DC run --rm -T probe > "$WORK/probe.log" 2>&1; prc=$?
sed 's/^/  | /' "$WORK/probe.log"
[ "$prc" = 0 ] || die "platform-probe exited $prc"
sline="$(grep '^PROBE SUMMARY' "$WORK/probe.log" | tail -1)"; [ -n "$sline" ] || die "platform-probe printed no 'PROBE SUMMARY' line — it may not have run its checks"

# ── 8. exposure ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=exposure; banner 8 "no service publishes a port; the network is internal"
DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres m1 m2 m3 platform platform-2 smtp-ctl m3-worker-2 m3-worker-3 $M2W_EXPOSE || die "a service is reachable from outside the network"
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
wait_for() { want="$1"; i=0; while [ $i -lt 120 ]; do [ "$(rz)" = "$want" ] && return 0; i=$((i+1)); sleep 1; done; return 1; }
wait_for 200 || die "readyz was not 200 with a healthy database"
DC stop postgres >/dev/null 2>&1 || die "could not stop postgres"
wait_for 503 || die "readyz stayed $(rz) with the database stopped — readiness does not follow the database"
DC start postgres >/dev/null 2>&1 || die "could not restart postgres"
wait_for 200 || die "readyz did not return to 200 after the database came back"
echo "  readyz: 200 -> 503 (postgres stopped) -> 200 (postgres back)"

# ── 9b. module 3 down at expiry ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Phase 2, chunk B9. The running PLATFORM SERVICE (its expiry and reconcile loops, the real module-3 client) with the REAL module 3 container stopped while an entitlement expires. The assertions are
# the three the ruling asks for: (1) while module 3 is down the platform keeps retrying (attempts rise); (2) it NEVER records a convergence it did not see (converged_version stays behind and
# `observed` keeps the last state module 3 really answered with); (3) once module 3 is back the platform converges, and module 3 itself says the user is suspended (a repeated suspend answers 422).
STAGE=m3-outage; banner "9b" "module 3 is stopped while an entitlement expires; the platform retries, never reports success it did not see, and converges when module 3 is back"
OU="$(cat /proc/sys/kernel/random/uuid)"
psql_su "INSERT INTO auth.users (id, email) VALUES ('$OU', 'outage@example.test')" >/dev/null || die "could not create the outage user's login"
psql_su "INSERT INTO public.users (id, auth_user_id, email) VALUES ('$OU', '$OU', 'outage@example.test')" >/dev/null || die "could not create the outage user"
syncrow() { psql_su "select row_to_json(s) from platform.m3_sync s where user_id = '$OU'"; }
waitfor() { desc="$1"; secs="$2"; q="$3"; i=0; while [ $i -lt "$secs" ]; do [ "$(psql_su "$q")" = t ] && return 0; i=$((i+1)); sleep 1; done; die "timed out after ${secs}s waiting for: $desc — m3_sync: $(syncrow)"; }
psql_su "INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key, until_at) VALUES ('$OU', 'm3', 'm3_enable', '$FA_ID', 'harness outage proof', 'outage-enable', now() + interval '25 seconds')" >/dev/null || die "could not enable module 3 for the outage user"
waitfor "the platform's reconcile loop to deliver the enable to the REAL module 3 (converged, observed active)" 60 "select converged_version = want_version and observed = 'active' from platform.m3_sync where user_id = '$OU'"
echo "  1. the enable was delivered: $(syncrow)"
DC stop m3-worker m3 >/dev/null 2>&1 || die "could not stop module 3"
waitfor "the platform's expiry loop to expire the entitlement (database clock)" 90 "select not enabled from platform.m3_entitlements where user_id = '$OU'"
waitfor "the reconcile loop to RETRY at least 3 times against the stopped module 3" 120 "select attempts >= 3 and last_error is not null from platform.m3_sync where user_id = '$OU'"
down="$(psql_su "select converged_version < want_version and want_suspended and observed <> 'suspended' from platform.m3_sync where user_id = '$OU'")"
[ "$down" = t ] || die "while module 3 was down the platform recorded a convergence it did not see: $(syncrow)"
echo "  2. module 3 down, the entitlement expired: the platform has retried and has NOT claimed convergence: $(syncrow)"
DC up -d --wait --wait-timeout 240 m3 >/dev/null 2>&1 || die "module 3 did not come back healthy"
DC up -d m3-worker >/dev/null 2>&1 || die "module 3's worker did not restart"
waitfor "the platform to converge once module 3 is back (observed suspended, converged_version = want_version)" 240 "select converged_version = want_version and observed = 'suspended' and attempts = 0 from platform.m3_sync where user_id = '$OU'"
echo "  3. module 3 is back and the platform converged: $(syncrow)"
code="$(DC exec -T platform wget -S -q -O /dev/null --header "Authorization: Bearer $M3_ADMIN_KEY" --post-data '{}' "http://m3:8080/v1/admin/users/$OU/suspend" 2>&1 | awk '/HTTP\//{print $2; exit}')"
[ "$code" = 422 ] || die "module 3 itself does not hold the user as suspended: a repeated suspend answered '$code', want 422 (already suspended)"
echo "  module 3 itself confirms it: a repeated suspend answers 422 (already suspended)"
m3a_outage

# ── 9c. one loop runner ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Phase 2 close-out D9 (finding F36): TWO platform instances on one database (the service `platform` is the instance `platform-1`, the service `platform-2` the instance `platform-2`), both serving HTTP. The expiry and reconcile loops
# run only in the one that holds the leader advisory lock (session level, key 7200000002, taken on one dedicated connection whose application_name is the instance's name, so pg_stat_activity names the leader). Four things are read:
#   (a) exactly one granted lock on the key, held by a session named after one of the two instances; while it leads, the OTHER instance runs no pass at all (the debug line every pass writes is counted in each instance's log);
#   (b) five dated entitlements are expired exactly once each (one m3_expire ledger row per user, none skipped) and all five converge at the REAL module 3 (observed suspended); the instance that did it is the leader;
#   (c) the leader is KILLED (SIGKILL): the other instance takes the lock within 60 s, and three more dated entitlements are expired and delivered by the survivor;
#   (d) the killed instance is started again: it serves HTTP, holds no lock and runs no pass (it is the standby), and the lock holder does not change.
# The ledger alone cannot show "exactly one acts": an expiry is idempotent per user by a unique key, so one m3_expire row per user would also hold with two runners. That is why the evidence is the lock itself and each instance's own log
# (a debug line per pass, an info line per expiry), counted before and after, never the ledger only.
STAGE=leader; banner "9c" "two platform instances: exactly one runs the loops; the leader is killed and the other takes over; the killed one comes back as the standby"
LEADER_KEY=7200000002
LOCKWHERE="l.locktype = 'advisory' and l.granted and l.objsubid = 1 and l.database = (select d.oid from pg_database d where d.datname = current_database()) and ((l.classid::bigint << 32) | l.objid::bigint) = $LEADER_KEY"
nlocks() { psql_su "select count(*) from pg_locks l where $LOCKWHERE"; }
holders() { psql_su "select coalesce(string_agg(a.application_name, ',' order by a.application_name), '') from pg_locks l join pg_stat_activity a on a.pid = l.pid where $LOCKWHERE"; }
svc_of() { case "$1" in platform-1) echo platform;; platform-2) echo platform-2;; esac; }
other_of() { case "$1" in platform-1) echo platform-2;; platform-2) echo platform-1;; esac; }
http_code() { DC exec -T "$1" wget -q -S -O /dev/null "http://127.0.0.1:8080/$2" 2>&1 | awk '/HTTP\//{print $2; exit}'; }
svclog() { DC logs --no-color "$1" > "$WORK/leader-$1.log" 2>&1 || die "could not read the logs of the service $1"; }
nlog() { grep -cF -- "$2" "$WORK/leader-$1.log" || true; }
# snap <service>: reads the service's log once and sets PE and PR (the passes of its expiry and its reconcile loop written so far), PL (how often it logged that it became the leader) and PS (how often it logged that it is a standby)
snap() { svclog "$1"; PE="$(nlog "$1" '"msg":"entitlement expiry pass"')"; PR="$(nlog "$1" '"msg":"entitlement reconcile pass"')"; PL="$(nlog "$1" 'loops: this instance is now the leader')"; PS="$(nlog "$1" 'loops: another instance holds the leader lock')"; }
# lwait <what> <seconds> <sql that returns t>
lwait() { local i=0; while [ $i -lt "$2" ]; do [ "$(psql_su "$3")" = t ] && return 0; i=$((i+1)); sleep 1; done; die "timed out after ${2}s waiting for: $1 (the leader lock is held by '$(holders)')"; }
# mkusers <tag> <n>: creates n users (a login and a platform user each); sets NEWU (their ids) and NEWIN (them as a quoted SQL list)
mkusers() {
  NEWU=(); NEWIN=""; local au="" pu="" u k
  for k in $(seq 1 "$2"); do
    u="$(cat /proc/sys/kernel/random/uuid)"; NEWU+=("$u"); NEWIN="$NEWIN${NEWIN:+,}'$u'"
    au="$au${au:+,}('$u', 'leader-$1-$k@example.test')"; pu="$pu${pu:+,}('$u', '$u', 'leader-$1-$k@example.test')"
  done
  psql_su "INSERT INTO auth.users (id, email) VALUES $au" >/dev/null || die "could not create the logins of the $1 users"
  psql_su "INSERT INTO public.users (id, auth_user_id, email) VALUES $pu" >/dev/null || die "could not create the platform users of the $1 users"
}
# dated <tag> <seconds>: enables module 3 for every user of NEWU through the ledger (the acting admin is the first admin of stage 4b), each until <seconds> from now on the DATABASE's clock
dated() {
  local vals="" u
  for u in "${NEWU[@]}"; do vals="$vals${vals:+,}('$u', 'm3', 'm3_enable', '$FA_ID', 'harness leader proof', 'leader-$1-$u', now() + interval '$2 seconds')"; done
  psql_su "INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key, until_at) VALUES $vals" >/dev/null || die "could not enable module 3 for the $1 users through the ledger"
}
# batch <tag> <n> <the service that must act> <the service that must idle>: n dated entitlements that all expire 8 s from now; read what each instance did
batch() {
  local tag="$1" n="$2" act="$3" idle="$4" u a b exp ae0 ar0 ie0 ir0 ae1 ar1 ie1 ir1
  snap "$act"; ae0="$PE"; ar0="$PR"; snap "$idle"; ie0="$PE"; ir0="$PR"
  mkusers "$tag" "$n"; dated "$tag" 8
  lwait "all $n $tag users to be expired by an m3_expire ledger row" 90 "select count(distinct user_id) = $n from platform.ledger where kind = 'm3_expire' and user_id in ($NEWIN)"
  sleep 6   # three more ticks of each loop: a second runner, or an expiry done twice, would show itself in this time
  exp="$(psql_su "select count(*) || '/' || count(distinct user_id) from platform.ledger where kind = 'm3_expire' and user_id in ($NEWIN)")"
  [ "$exp" = "$n/$n" ] || die "the $n $tag users have $exp m3_expire rows / distinct users, want exactly one row each ($n/$n): an expiry was done twice or skipped"
  lwait "all $n $tag users to be delivered to the REAL module 3 (converged_version = want_version, observed suspended)" 120 "select count(*) = $n from platform.m3_sync where user_id in ($NEWIN) and want_suspended and converged_version = want_version and observed = 'suspended'"
  snap "$act"; ae1="$PE"; ar1="$PR"; snap "$idle"; ie1="$PE"; ir1="$PR"
  [ $((ae1-ae0)) -ge 4 ] && [ $((ar1-ar0)) -ge 4 ] || die "$act ran only +$((ae1-ae0)) expiry and +$((ar1-ar0)) reconcile passes while the $tag users expired and converged (2 s loops): it is not the runner"
  [ "$ie1" = "$ie0" ] && [ "$ir1" = "$ir0" ] || die "$idle ran passes while $act acted (expiry $ie0 -> $ie1, reconcile $ir0 -> $ir1): two runners"
  for u in "${NEWU[@]}"; do
    a="$(grep -F '"msg":"entitlement expired"' "$WORK/leader-$act.log" | grep -cF "$u" || true)"
    b="$(grep -F '"msg":"entitlement expired"' "$WORK/leader-$idle.log" | grep -cF "$u" || true)"
    [ "$a" = 1 ] || die "user $u of the $tag batch was logged as expired $a times by $act, want exactly once"
    [ "$b" = 0 ] || die "user $u of the $tag batch was logged as expired $b times by $idle, which must not act"
  done
  echo "     the $n $tag entitlements: $exp m3_expire rows/users, all converged at module 3 (observed suspended); each logged as expired exactly once by $act and never by $idle; passes while it ran: $act +$((ae1-ae0)) expiry +$((ar1-ar0)) reconcile, $idle +0 +0"
}

# (a) the lock, and who holds it
i=0; LEAD=""
while [ $i -lt 60 ]; do
  h="$(holders)"
  if [ "$(nlocks)" = 1 ] && { [ "$h" = platform-1 ] || [ "$h" = platform-2 ]; }; then LEAD="$h"; break; fi
  i=$((i+1)); sleep 1
done
[ -n "$LEAD" ] || die "after 60 s there is not exactly one granted lock on the leader key held by a session named platform-1 or platform-2: locks=$(nlocks) holders='$(holders)' (pg_stat_activity application_name)"
LSVC="$(svc_of "$LEAD")"; OTHER="$(other_of "$LEAD")"; OSVC="$(svc_of "$OTHER")"
echo "  a. exactly one granted lock on key $LEADER_KEY, held by a session named '$LEAD' (service $LSVC); the other instance is '$OTHER' (service $OSVC)"
for s in "$LSVC" "$OSVC"; do [ "$(http_code "$s" readyz)" = 200 ] || die "$s does not answer /readyz with 200: an instance that does not lead still serves HTTP"; done
echo "     both instances answer /readyz 200 (the standby serves HTTP like the leader)"
snap "$LSVC"; le0="$PE"; lr0="$PR"; snap "$OSVC"; oe0="$PE"; or0="$PR"
sleep 10
snap "$LSVC"; le1="$PE"; lr1="$PR"; snap "$OSVC"; oe1="$PE"; or1="$PR"
[ $((le1-le0)) -ge 3 ] && [ $((lr1-lr0)) -ge 3 ] || die "the leader $LEAD ran only +$((le1-le0)) expiry and +$((lr1-lr0)) reconcile passes in 10 s (2 s loops): it is not running the loops"
[ "$oe1" = "$oe0" ] && [ "$or1" = "$or0" ] || die "the other instance $OTHER ran passes while $LEAD held the lock: expiry $oe0 -> $oe1, reconcile $or0 -> $or1 (two runners)"
[ "$(nlocks)" = 1 ] && [ "$(holders)" = "$LEAD" ] || die "the leader lock moved while nothing was wrong: now '$(holders)'"
echo "     in 10 s the leader $LEAD ran +$((le1-le0)) expiry and +$((lr1-lr0)) reconcile passes; the other instance $OTHER ran +0 and +0 (it idles); the lock did not move"

# (b) five dated entitlements, expired and delivered once each by the one that leads
echo "  b. five entitlements dated 8 s ahead, expired by the leader $LEAD"
batch first 5 "$LSVC" "$OSVC"

# (c) the leader is killed
snap "$OSVC"; ol0="$PL"
DC kill "$LSVC" >/dev/null 2>&1 || die "could not kill the leader's service $LSVC"
i=0; while [ $i -lt 60 ]; do [ "$(nlocks)" = 1 ] && [ "$(holders)" = "$OTHER" ] && break; i=$((i+1)); sleep 1; done
[ $i -lt 60 ] || die "$OTHER did not take the leader lock within 60 s of the kill of $LEAD: locks=$(nlocks) holders='$(holders)'"
snap "$OSVC"; [ "$PL" -gt "$ol0" ] || die "$OTHER holds the lock but never logged that it became the leader"
echo "  c. $LEAD ($LSVC) was killed (SIGKILL); $OTHER holds the lock about ${i} s later (exactly one granted lock) and logged that it became the leader; three more entitlements, expired by the survivor"
batch second 3 "$OSVC" "$LSVC"

# (d) the killed instance comes back as the standby
snap "$LSVC"; kpe="$PE"; kpr="$PR"; kl0="$PL"; ks0="$PS"; snap "$OSVC"; se0="$PE"; sr0="$PR"
DC start "$LSVC" >/dev/null 2>&1 || die "could not start the killed instance $LSVC again"
i=0; while [ $i -lt 90 ]; do [ "$(http_code "$LSVC" readyz)" = 200 ] && break; i=$((i+1)); sleep 1; done
[ $i -lt 90 ] || die "the restarted instance $LSVC did not answer /readyz with 200 within 90 s"
j=0; while [ $j -lt 12 ]; do   # six ticks of its loops at 2 s, in which it would have taken the lock if it could
  [ "$(nlocks)" = 1 ] && [ "$(holders)" = "$OTHER" ] || die "after $LEAD came back the leader lock is held by '$(holders)' ($(nlocks) lock(s)); it must stay with $OTHER"
  j=$((j+1)); sleep 1
done
snap "$LSVC"; ke="$PE"; kr="$PR"
[ "$ke" = "$kpe" ] && [ "$kr" = "$kpr" ] || die "the restarted instance $LEAD ran passes (expiry $kpe -> $ke, reconcile $kpr -> $kr) although $OTHER holds the lock"
[ "$PL" = "$kl0" ] || die "the restarted instance $LEAD logged that it became the leader although $OTHER holds the lock"
[ "$PS" -gt "$ks0" ] || die "the restarted instance $LEAD never logged that another instance holds the lock (it did not run its election)"
snap "$OSVC"; [ $((PE-se0)) -ge 3 ] && [ $((PR-sr0)) -ge 3 ] || die "the leader $OTHER stopped running passes after $LEAD came back (expiry +$((PE-se0)), reconcile +$((PR-sr0)))"
echo "  d. $LEAD ($LSVC) is back: /readyz 200, no pass run, no lock held; for 12 s the lock stayed with $OTHER (exactly one granted lock); $OTHER went on running the loops (+$((PE-se0)) expiry, +$((PR-sr0)) reconcile)"

# ── 10. ledger ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=ledger; banner 10 "the balance invariant holds on the live database (rolled-back exercise through the ledger)"
bash "$PLATFORM_DIR/scripts/check-balance-invariant.sh" --psql-cmd "docker compose --project-directory $HERE -f $HERE/docker-compose.yml --env-file $WORK/harness.env --profile probe --profile tests exec -T postgres psql -U supabase_admin -d postgres" --exercise || die "the balance invariant gate is red on the live database"

[ "$M1N_ON" = 1 ] && [ "$SCOPE" = full ] && m1n_run   # the module-1 node stack, only when `m1node` is named in FULL_INCLUDES (stages M1N-1 to M1N-5, tests/harness/m1node/m1node.sh); dormant otherwise

# ── result ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=done
echo
echo "================ NOT CHECKED (what this run did not and could not vouch for)"
cat <<'EOF'
  - CoreDNS is not started and module 3's From-gate is OFF (module 3's own harness does the same): nothing that depends on DNS or on the From alignment is exercised. Module 3 REALLY sends here: three workers, so three lanes of one user, to Mailpit (which
    accepts everything) and to a controllable sink (tests/harness/sink) that rejects (550) or defers (451) named recipients at RCPT TO and holds the final reply, so that sends are in flight on demand. Still not exercised: a sink that drops a connection, answers
    slowly before DATA, throttles or greylists the way a real provider does (the two RCPT answers, a held final reply and a refused AUTH are all it simulates), more than three lanes or workers, and a provider's own limits. Module 2 has no worker and module 1 no
    SMTP node, so those two never send or probe.
  - real Supabase beyond the database image: this is the digest-pinned supabase/postgres (real auth schema, the non-superuser migrating role `postgres`, the required provisioning step). Not here: GoTrue (so
    no login is ever performed), the pooler (Supavisor), PostgREST and the exposed-schema setting (an owner action: Docs/install.md step 5), a hosted project's role grants and extensions.
  - the scratch databases of the integration tests (cloned from nothing: a fresh database, the auth.users FIXTURE, the platform migrated by the superuser) differ from the live database in exactly
    those two ways; what runs as the non-superuser `postgres` on the real auth schema is the live database and the `supabase` workflow.
  - module BEHAVIOUR on the platform's tables beyond the statements the modules issue against public.users / public.job_items (db/module-statements.txt, executed as each module's role in a rolled-back transaction): no job was run end to end by the platform's job engine (Phase 3/4). The one exception is module 1's bulk job below, which the test itself
    submits, drains and settles.
  - module 1's bulk results settled item by item (close-out D2, TestM1Live_BulkJobSettlesItemByItem): ONE job of 12 rows (three malformed, one address in three spellings, six addresses at domains of ours) read in pages of 5, on a module 1 whose SMTP
    stage is not wired and whose DNS has no route out. What that proves is the platform's mapping, its keying by the module's own item_id, its idempotence and its per-page settlement against the module's OWN report, with a hand-written oracle. It
    does NOT prove module 1's verdicts: no row reaches an SMTP answer (valid, mailbox_full, catch_all, greylisted, a 550), the vendor unit (m1_vendor) and a cached answer (m1_cache) never occur, and the failure origin of an unknown is whatever this
    runner's failed lookups give (the oracle follows it, so a different origin changes the numbers and not the verdict). Not exercised either: a job of more than one page of 1000 results, a job that runs for hours, a module 1 restarted in the middle
    of a drain, two jobs of one customer at once, and the platform's job engine (the test calls Settle itself, page by page, as the engine will).
  - module behaviour beyond health, readiness and describe (Phase 3); module 1 has NO describe at all (Docs/phase-0-spec.md F1) and runs here as the least-privilege login verify_app (since Phase 1, A2).
  - reachability from another host, a firewall or a cloud security group; TLS termination; the mTLS worker links of modules 1 and 2 (module 3's is exercised by its worker).
  - the loops are exercised by TWO platform instances on one database, and by killing the leader (SIGKILL: the server sees the closed socket at once and frees the session lock), but NOT under a network partition: a leader that is cut off from the
    database and not killed keeps its server-side session, and so the lock, until the server decides the peer is gone (TCP keepalive), so nothing runs twice but a takeover is late. Not exercised either: more than two instances, a database failover under load,
    and a pass already running when its instance loses the lock (it runs to its end, bounded by its timeout, while the next leader may have started: the ledger's unique keys and the reconciler's compare-and-set keep that safe, the lock does not).
  - that every module commit other than the three pins in tools/modules.lock still works with the platform.
EOF
[ "$M2W_ON" = 1 ] && m2w_not_checked correction   # what module 2's real worker stage does not vouch for (tests/harness/m2worker/stage.sh); printed only when that stage ran, and it says that the first bullet's "Module 2 has no worker" no longer holds
[ "$M1N_ON" = 1 ] && m1n_not_checked   # what the module-1 node stack does not vouch for (tests/harness/m1node/m1node.sh); printed only when that stack ran
echo
echo "HARNESS RESULT: GREEN — modules built from $(lockpin coldmail-verify | cut -c1-7) / $(lockpin coldmail-send-managed | cut -c1-7) / $(lockpin coldmail-send-byo | cut -c1-7); $sline"
