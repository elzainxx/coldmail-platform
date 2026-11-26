#!/usr/bin/env bash
# stage.sh — a REAL GoTrue in the platform harness (Phase 4, chunk B15; Docs/phase-4-spec.md "Locked here" 29). SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself) and it uses what
# run.sh defines: DC, die, banner, mask, STAGE, WORK, HERE, PLATFORM_DIR, GO_PKG, PROJECT_NET, TEST_TAGS.
#
# DORMANT BY DEFAULT. run.sh calls these functions only when GT_ON=1: under H_SCOPE=gotrue, or in the full run once `gotrue` is named in FULL_INCLUDES (run.sh, one place; the lead adds the name after a scoped run is
# green, and raises tests/floors-harness.txt by the number of tests in GT_TESTS at the same time). With GT_ON=0 nothing here runs, the compose override docker-compose.gotrue.yml is not merged, no secret is minted, no
# container exists and the tests (build tag gotrue) are not compiled: the full harness is what it was before this stage existed.
#
# What is here, in the order run.sh calls it:
#   gotrue_mint         stage 2: GoTrue's JWT secret (48 random bytes in hex: the HS256 key GoTrue signs with and the tests verify with) and the password of the database role GoTrue logs in as, minted with
#                       openssl, masked BEFORE anything uses them, appended mode 600 to the env file (H_JWT_SECRET, H_GOTRUE_DB_PASSWORD); never printed
#   gotrue_pull         stage 3: the digest-pinned image is pulled (a runner that cannot pull THAT digest is red here, before the migrations spend minutes, and the pin is not changed from the runner's side)
#   gotrue_stage        stage 5d: the role supabase_auth_admin is checked (it exists, it can log in, it is NOT a superuser) and given the minted password, GoTrue is started, its health is waited for in a bounded loop
#                       (the container's log tail is printed when it exits or never gets healthy), asked from a container on the private network by name, and its own migrations are read back from the database
#   gotrue_evidence     stage 6: prints the tests' own log lines (the NOTE lines and the verdict lines)
#   gotrue_require_pass stage 6: every TestGoTrue_* test has a `--- PASS` line in the log the run itself produced (a binary built without one, or a -run that skipped it, would leave the rest green)
#   gotrue_secret_leak  after the tests: the secret, the database password and anything shaped like a JWT (every token the tests were given, and the service-role key they minted) are in neither the log of
#                       GoTrue nor this run's own test log (a workflow log is public)
#   gotrue_scope_run    H_SCOPE=gotrue: Postgres + migrations + GoTrue + only the TestGoTrue tests, then the exposure check; ends the run
#   gotrue_not_checked  what this stage does not vouch for
# Nothing here prints a credential: no `set -x`, no environment dump, and a failing check names a file and a number, never a value.

GT_ROLE="supabase_auth_admin"                 # the role the Supabase image creates for GoTrue; it owns auth.users and the other objects of the auth schema that GoTrue's migrations alter
GT_JWT_SECRET=""; GT_DB_PASS=""               # set by gotrue_mint
# a compact JWT: both of the first two segments are JSON objects, so both start with `eyJ`
GT_JWT_SHAPE='eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}'
# the tests of this stage, by name: the scope requires each to be declared by the binary and to have a `--- PASS` line, and the floor of this scope is their number (tests/harness/gotrue_test.go; the selftest
# proves this list is exactly the set of TestGoTrue_ functions of that file)
GT_TESTS=(
  TestGoTrue_Preconditions
  TestGoTrue_SignupAndPasswordGrantTokensVerify
  TestGoTrue_TamperedAndForeignTokensAreRefused
  TestGoTrue_ThePlatformIdentityChain
  TestGoTrue_InactiveUserIsRefusedByTheResolverNotByTheToken
  TestGoTrue_DeletedLoginIsRefusedByGoTrueButItsOldTokenStillVerifies
  TestGoTrue_API_ARealTokenIsAcceptedATamperedOneRefusedAndUsersAreIsolated
)

gt_psql_su() { DC exec -T postgres psql -X -U supabase_admin -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }
gt_logs_tail() { DC logs --no-color --tail="${1:-60}" gotrue 2>&1 | sed 's/^/     | gotrue: /'; }
gt_enforces_modes() { [ "$(uname -s)" = Linux ]; }       # a Windows or macOS bind mount does not keep file modes: only Linux is asked
gt_mode_of() { stat -c '%a' "$1" 2>/dev/null; }

# ── stage 2: secrets ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Called right AFTER run.sh wrote $WORK/harness.env: it mints, masks, then appends its two lines to the same file (written under umask 077, so mode 600, which is checked where the host keeps modes).
gotrue_mint() {
  local a b m
  a="$(openssl rand -hex 48)"; b="$(openssl rand -hex 16)"
  [ -n "$a" ] && [ -n "$b" ] || die "could not mint GoTrue's JWT secret and database password (openssl rand printed nothing)"
  [ "${#a}" = 96 ] || die "GoTrue's JWT secret is ${#a} characters long, expected 96 (48 random bytes in hex)"
  [ "${#b}" = 32 ] || die "GoTrue's database password is ${#b} characters long, expected 32 (16 random bytes in hex)"
  mask "$a"; mask "$b"                                                  # masked BEFORE they are written anywhere or handed to any command
  [ -f "$WORK/harness.env" ] || die "the env file is missing: gotrue_mint runs after run.sh wrote it"
  if gt_enforces_modes; then
    m="$(gt_mode_of "$WORK/harness.env")"
    [ "$m" = 600 ] || die "the env file $WORK/harness.env is mode ${m:-unknown}, expected 600: it holds this run's secrets"
  fi
  ( umask 077; { echo "H_JWT_SECRET=$a"; echo "H_GOTRUE_DB_PASSWORD=$b"; } >> "$WORK/harness.env" ) || die "could not append GoTrue's secrets to the env file"
  GT_JWT_SECRET="$a"; GT_DB_PASS="$b"
  echo "  minted for GoTrue: 1 JWT secret (48 random bytes in hex), 1 database password for the role $GT_ROLE — masked, none printed"
}

# ── stage 3: the image ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# `docker compose pull gotrue`: the image of docker-compose.gotrue.yml is name:tag@sha256:<digest>. If the runner cannot pull THAT digest the run is red here and the pin is not changed from this side: it is reported.
gotrue_pull() {
  STAGE=build
  DC pull gotrue > "$WORK/gotrue-pull.log" 2>&1 || { tail -15 "$WORK/gotrue-pull.log" | sed 's/^/     | /'; die "the runner could not pull the digest-pinned GoTrue image of docker-compose.gotrue.yml: the pin is NOT changed from the runner's side, report it"; }
  echo "  the digest-pinned GoTrue image is pulled: $(DC config --images 2>/dev/null | grep -F supabase/gotrue | head -1)"
}

# ── stage 5d: GoTrue ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# One request from a container on the private network (the test container's image, busybox wget; no credential is sent): GoTrue's /health by NAME, which is how the tests reach it. Prints wget's own text, returns its status.
gotrue_probe() { DC run --rm -T --no-deps --entrypoint wget harness-tests -q -T 3 -O - http://gotrue:9999/health 2>&1; }

gotrue_stage() {
  STAGE=gotrue; banner "5d" "a REAL GoTrue (digest-pinned) on the real auth schema: the role's password set, GoTrue started, its own migrations run, its health read from inside the private network"
  local out i tries st hs cid before after n rc
  tries="${GT_WAIT_TRIES:-120}"
  [ -n "$GT_JWT_SECRET" ] && [ -n "$GT_DB_PASS" ] || die "GoTrue's secrets were not minted: gotrue_mint did not run (stage 2)"
  # the role the image made for GoTrue: it must exist, log in, and NOT be a superuser (GoTrue's migrations must pass on ownership and grants, never on a superuser)
  out="$(gt_psql_su "select rolcanlogin::text || '|' || rolsuper::text || '|' || rolcreaterole::text from pg_roles where rolname = '$GT_ROLE'" 2>&1)" || die "could not read pg_roles: $out"
  [ -n "$out" ] || die "the database has no role $GT_ROLE: the Supabase image's init scripts create it for GoTrue (it owns the auth schema's tables), and GoTrue's migrations cannot run without it"
  case "$out" in
    "true|false|"*) ;;
    *) die "the role $GT_ROLE is (login, superuser, createrole) = ($out), expected a login role that is NOT a superuser: GoTrue's migrations must pass on ownership, never on a superuser";;
  esac
  echo "  the role $GT_ROLE: (login, superuser, createrole) = ($out)"
  echo "  facts the image left: $(gt_psql_su "select 'auth.users is owned by ' || coalesce((select tableowner from pg_tables where schemaname = 'auth' and tablename = 'users'), '(no such table)') || ', the schema auth by ' || coalesce((select pg_get_userbyid(nspowner) from pg_namespace where nspname = 'auth'), '(no such schema)')" 2>&1 | head -1)"
  # the password: the image sets none for this role (the Supabase CLI and the self-hosted stack set it the same way, ALTER USER ... WITH PASSWORD); it is a per-run secret of its own, so GoTrue never holds the password of the superuser supabase_admin that the harness shares
  out="$(printf "ALTER ROLE %s WITH PASSWORD :'pw';\n" "$GT_ROLE" | DC exec -T postgres psql -X -q -v ON_ERROR_STOP=1 -v "pw=$GT_DB_PASS" -U supabase_admin -d postgres 2>&1)" \
    || die "could not set the password of $GT_ROLE as the image's superuser supabase_admin: $(printf '%s' "$out" | sed "s/$GT_DB_PASS/***/g" | head -3 | tr '\n' ' ')"
  before="$(gt_psql_su "select count(*) from auth.schema_migrations" 2>/dev/null)" || before="unreadable"
  DC up -d gotrue > "$WORK/gotrue-up.log" 2>&1 || { sed 's/^/     | /' "$WORK/gotrue-up.log" | tail -20; die "GoTrue did not start (docker compose up -d gotrue failed)"; }
  i=0
  while [ "$i" -lt "$tries" ]; do
    cid="$(DC ps -a -q gotrue 2>/dev/null | head -1)"; st=""; hs=""
    if [ -n "$cid" ]; then
      st="$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null)"
      hs="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null)"
    fi
    [ "$hs" = healthy ] && break
    case "$st" in
      exited|dead) gt_logs_tail 80; die "GoTrue's container is '$st' (exit code $(docker inspect -f '{{.State.ExitCode}}' "$cid" 2>/dev/null)) after $((i * ${GT_SLEEP:-2})) s: its log is above. Its migrations run as $GT_ROLE at its start: a 'permission denied' or 'must be owner' there is about the facts printed before it; a 'read-only file system' is docker-compose.gotrue.yml's read_only; a 'connection' or 'password authentication' error is the role's password.";;
    esac
    [ "$hs" != unhealthy ] || { gt_logs_tail 80; die "GoTrue's own health check says unhealthy after $((i * ${GT_SLEEP:-2})) s (container '$st'): its log is above"; }
    i=$((i+1)); sleep "${GT_SLEEP:-2}"
  done
  [ "$i" -lt "$tries" ] || { gt_logs_tail 80; die "GoTrue was not healthy after $((i * ${GT_SLEEP:-2})) s (container '${st:-none}', health '${hs:-none}'): its log is above"; }
  echo "  GoTrue is healthy by its own check after $((i * ${GT_SLEEP:-2})) s"
  out="$(gotrue_probe)"; rc=$?
  [ "$rc" = 0 ] || die "GoTrue is healthy by its own check but a request from a container on the private network to http://gotrue:9999/health failed (wget printed: $(printf '%s' "$out" | head -3 | tr '\n' ' ')): the tests reach it by that name"
  case "$out" in *'{'*'}'*) ;; *) die "http://gotrue:9999/health answered, but not with JSON (wget printed: $(printf '%s' "$out" | head -c 200 | tr '\n' ' ')): something else listens at the name gotrue";; esac
  echo "  GoTrue answers /health from a container on the private network, by name: $(printf '%s' "$out" | head -c 300)"
  # what GoTrue's own migrations did to the database, read back (the image's auth.schema_migrations is GoTrue's own table)
  after="$(gt_psql_su "select count(*) from auth.schema_migrations" 2>&1)" || die "could not count auth.schema_migrations after GoTrue started: $after"
  case "$after" in ''|*[!0-9]*) die "auth.schema_migrations did not answer a number after GoTrue started: '$after'";; esac
  [ "$after" -ge 1 ] || die "auth.schema_migrations is EMPTY after GoTrue reported healthy: GoTrue did not migrate this database (the instrument matched nothing)"
  n="$(gt_psql_su "select count(*) from pg_stat_activity where usename = '$GT_ROLE'" 2>/dev/null)" || n="unreadable"
  echo "  GoTrue's own migrations: auth.schema_migrations held $before row(s) before GoTrue started and holds $after now; $n connection(s) are held as $GT_ROLE (pg_stat_activity)"
  DC logs --no-color gotrue 2>&1 | grep -i -E 'migrat|listening|started|version|error|warn|mail' | head -8 | sed 's/^/     | gotrue: /'
  return 0
}

# ── stage 6 helpers ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
gotrue_evidence() {
  echo "  ---- GoTrue: the tests' own log lines (every NOTE says what a test saw; no token, password or secret is ever printed) ----"
  grep -E " gotrue(_api)?_test.go:[0-9]+: |^--- (PASS|FAIL|SKIP): TestGoTrue_" "$1" | sed 's/^/  | /' | head -200
}

gotrue_require_pass() {
  local t
  for t in "${GT_TESTS[@]}"; do
    grep -q "^--- PASS: $t " "$1" || die "$t has no '--- PASS' line in the run log: it did not run, or did not pass (the platform's token verification was not shown against a token the real GoTrue issued)"
  done
  echo "  GoTrue: ${#GT_TESTS[@]} TestGoTrue_* tests have a '--- PASS' line"
}

# the secrets of this run, and anything shaped like a JWT, in no log that is printed or kept: the log of GoTrue and this run's own test log. grep reads the patterns from a file descriptor, so no secret is on a command
# line; nothing is printed but a name. (The tokens the tests were given and the service-role key they minted are not known to the shell: their SHAPE is what is searched for.)
gotrue_secret_leak() {
  local name f
  [ -n "$GT_JWT_SECRET" ] && [ -n "$GT_DB_PASS" ] || die "the secrets of this run are empty: the leak scan would look for nothing"
  DC logs --no-color gotrue > "$WORK/leak-gotrue.log" 2>&1 || die "could not read the logs of gotrue"
  for name in leak-gotrue.log h-run.log; do
    f="$WORK/$name"
    [ -s "$f" ] || { rm -f "$WORK/leak-gotrue.log"; die "the log $name, which the leak check must read, is missing or empty: the instrument matched nothing"; }
    if grep -qFf <(printf '%s\n' "$GT_JWT_SECRET" "$GT_DB_PASS") "$f"; then
      rm -f "$WORK/leak-gotrue.log"; die "a secret of this run (GoTrue's JWT secret or its database password) appears in $name (the log is not shown: it is public)"
    fi
    if grep -qE "$GT_JWT_SHAPE" "$f"; then
      rm -f "$WORK/leak-gotrue.log"; die "something shaped like a JWT (an access token or a service-role key) appears in $name (the log is not shown: it is public)"
    fi
  done
  rm -f "$WORK/leak-gotrue.log"
  echo "  neither the JWT secret nor the database password of this run, nor anything shaped like a JWT, appears in the log of gotrue or in this run's own test log"
}

# ── H_SCOPE=gotrue ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Postgres, the four migration chains and the first admin (stages 1-4b ran), then GoTrue and only the TestGoTrue tests; no module, no platform service, no probe, no outage stages.
# Called by run.sh after stage 4b; it never returns (it exits with the run's own status, the EXIT trap tears the stack down).
gotrue_scope_run() {
  STAGE=services; banner 5 "scope gotrue: GoTrue only (module 1, module 2, module 3 and the platform service are not started)"
  gotrue_stage

  STAGE=tests; banner 6 "scope gotrue: the TestGoTrue_* tests, compiled on the runner and run inside the network"
  local re='^TestGoTrue_' trc lrc t
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags "$TEST_TAGS" -c -o "$WORK/out/harness.test" ./tests/harness ) || die "the harness test binary (tags: $TEST_TAGS) did not compile"
  DC run --rm -T harness-tests -test.list "$re" > "$WORK/h-list.log" 2>&1; lrc=$?
  [ "$lrc" = 0 ] || { cat "$WORK/h-list.log"; die "listing the TestGoTrue tests failed ($lrc)"; }
  for t in "${GT_TESTS[@]}"; do grep -qx "$t" "$WORK/h-list.log" || die "$t is not among the declared tests of the harness binary"; done
  DC run --rm -T harness-tests -test.v -test.count=1 -test.timeout=15m -test.run "$re" > "$WORK/h-run.log" 2>&1; trc=$?
  sed 's/^/  | /' "$WORK/h-run.log" | tail -60
  gotrue_evidence "$WORK/h-run.log"
  [ "$trc" = 0 ] || { echo "  ---- failing tests (with context) ----"; grep -n -B1 -A14 -e '--- FAIL' "$WORK/h-run.log" | sed 's/^/  | /' | head -240; echo "  ---- GoTrue's log (tail) ----"; gt_logs_tail 60; }
  if [ "$trc" = 0 ]; then t="ok  \t$GO_PKG\t0.000s"; else t="FAIL\t$GO_PKG\t0.000s"; fi
  printf "$t\n" >> "$WORK/h-run.log"; printf "ok  \t$GO_PKG\t0.000s\n" >> "$WORK/h-list.log"
  [ "$trc" = 0 ] || die "the TestGoTrue tests failed (exit $trc)"
  gotrue_require_pass "$WORK/h-run.log"
  # the floor of THIS scope is the number of tests the scope names (the full floor, tests/floors-harness.txt, is asserted only by the full run)
  printf '# the TestGoTrue tests of scope gotrue\ngo|%s|%s\n' "$GO_PKG" "${#GT_TESTS[@]}" > "$WORK/floors-gotrue.txt"
  bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$WORK/floors-gotrue.txt" --go-run-log "$WORK/h-run.log" --go-list-log "$WORK/h-list.log" || die "the TestGoTrue test-count floor"
  gotrue_secret_leak

  STAGE=exposure; banner 8 "scope gotrue: no service publishes a port; the network is internal"
  local ids id pb cnt=0
  DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres gotrue || die "a service is reachable from outside the network"
  [ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
  ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
  for id in $ids; do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"

  STAGE=done
  echo
  echo "================ NOT CHECKED (scope gotrue: what this run did not and could not vouch for)"
  gotrue_not_checked
  echo "  - everything outside GoTrue: module 1, module 2, module 3, the platform service, the probe, the outage and leader stages and the ledger check are NOT run in this scope (H_SCOPE=full runs them)"
  echo
  echo "HARNESS RESULT: GREEN (scope gotrue: ${#GT_TESTS[@]} TestGoTrue tests) — the digest-pinned GoTrue image issued the tokens; the platform's internal/auth and internal/tenant took them"
  exit 0
}

# what the GoTrue stage does not vouch for (printed by the scoped run and, in the full run, run.sh's own list refers to it)
gotrue_not_checked() {   # `gotrue_not_checked correction` (the full run) first says that the "Not here: GoTrue" of run.sh's own list no longer holds
  [ "${1:-}" = correction ] && echo '  - CORRECTION to the "real Supabase beyond the database image" bullet above, for a run in which the GoTrue stage ran: GoTrue IS here (the bullet below), so "Not here: GoTrue (so no login is ever performed)" no longer holds; the pooler, PostgREST, the exposed-schema setting and a hosted project'"'"'s grants are still not here.'
  cat <<'EOF'
  - GoTrue's tokens are checked against the LEGACY symmetric configuration only: GOTRUE_JWT_SECRET, HS256. NOT exercised: GoTrue's asymmetric signing keys and its JWKS endpoint (internal/auth's ES256 / RS256 and key-set paths have only
    their own unit tests, against a fake issuer, and nothing here proves that a real project's published key set parses and verifies a real token), key rotation, and a project that has moved off the legacy secret.
  - a Supabase hosted project's real configuration: its issuer (https://<ref>.supabase.co/auth/v1), its gateway in front of GoTrue, its JWT expiry, its password and e-mail policies, its rate limits and its extensions. Here GoTrue serves its API at the root
    of a private name, the issuer is a string this harness chose and configured on both sides, and the rate limits are raised.
  - token refresh and sign-out (the refresh token is never used and a refreshed access token is never verified), MFA (the aal and amr claims are not read), a banned user (only a deleted login and the platform's own is_active are exercised), SSO, phone, anonymous
    and every social provider (all switched off, and the settings read back to say so).
  - the mail flow: GOTRUE_SMTP_HOST is empty and sign-ups are confirmed at once, so no confirmation, recovery, magic-link, invite or change-of-address mail is ever sent, and no link in one is ever followed.
  - the order of a real install: GoTrue's migrations ran here on a database that already held the platform's tables and the first admin's login (a real install deploys GoTrue first and the platform second).
  - the platform's HTTP API: this stage drives internal/auth and internal/tenant directly with the tokens GoTrue issued; the API's own middleware (Bearer parsing, status codes, cross-user refusals) is chunk B14's tests. Revocation is not provided by the token: the old
    token of a deleted login still verifies, and the platform's public.users.is_active is the control (said in the tests' NOTE lines).
  - an upgrade of GoTrue: one version, pinned by digest, is exercised, and what a newer one does to the token's claims is not.
EOF
  echo "NOT CHECKED: GoTrue's asymmetric signing keys and JWKS endpoint, a Supabase hosted project's real configuration, token refresh and sign-out, MFA, the mail flow, banned users, the order of a real install (GoTrue before the platform), the platform's HTTP API (chunk B14), an upgrade of GoTrue"
}
