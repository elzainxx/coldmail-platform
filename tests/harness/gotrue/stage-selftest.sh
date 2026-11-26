#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/gotrue/stage.sh go RED, for the reason they name, and stay green on a good input; and that the files of the GoTrue stage agree with each other (the list of
# tests, the compose file, the environment the test container is given, the wiring in run.sh and the workflows). No Docker, no Go build: the functions are sourced and run against stand-ins for docker, docker compose,
# openssl and psql. What it covers:
#   gotrue_mint          writes exactly two lines to the env file (a 96-character secret and a 32-character password), masks each BEFORE it is written, never prints either, refuses a tool that printed nothing or the wrong
#                        length, a missing env file and (where the host keeps file modes) an env file that is not mode 600
#   gotrue_pull          red, naming the pin, when the runner cannot pull the digest; green and says so when it can
#   gotrue_stage         the role is checked (missing, not a login, a superuser), its password is set without the password ever reaching the SQL text or a failure message, a start that fails, a container that exits
#                        (its log is printed), one that never gets healthy or says unhealthy, a probe from the private network that is refused or answered by something that is not GoTrue, an empty
#                        auth.schema_migrations; each case asserts the sentence it fails for
#   gotrue_require_pass  green when every TestGoTrue_* test has a column-0 `--- PASS` line; red, naming the test, when ANY ONE of them is missing (one case per listed test), FAILED or only indented
#   gotrue_secret_leak   green on clean logs and on a string that merely starts like a token; red, naming the log and never printing it, when the secret, the password or anything shaped like a JWT is in the
#                        log of GoTrue or in the run's own log; red when a log is missing or empty or when the secrets are empty (a scan with nothing to look for)
#   consistency          GT_TESTS is exactly the set of TestGoTrue_ functions of tests/harness/gotrue_test.go and gotrue_api_test.go and every one of them names a control; the compose file pins the image by digest, publishes no port and
#                        needs only variables somebody writes; the test container gets every variable the Go file reads; run.sh allows the scope, sources the stage and calls every gotrue_ function only under its guard;
#                        the stage is DORMANT in `full` until FULL_INCLUDES names it (the guard block of run.sh is evaluated for four settings); the workflows offer the scope and run this selftest
# NOT covered here (they need a Linux host with Docker): the real docker compose, wget, psql and the GoTrue image itself.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"; ROOT="$(cd "$HARNESS/../.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=100; cases=0; ok=0; bad=0
verdict() { # label want-rc want-substring out rc [forbidden-substring]
  local label="$1" want="$2" sub="$3" out="$4" rc="$5" forbid="${6:-}" n f=0
  cases=$((cases+1))
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ -n "$forbid" ] && printf '%s\n' "$out" | grep -q -F -- "$forbid"; then f=1; fi
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ] && [ "$f" -eq 0 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), substring '$sub' found $n times${forbid:+, forbidden text present: $f}"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}

# the stand-ins for what run.sh defines
cat > "$T/env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
MASKLOG="$WORK/mask.log"
mask() { if [ -f "$WORK/harness.env" ] && grep -qF -- "$1" "$WORK/harness.env"; then echo "AFTER_WRITE" >> "$MASKLOG"; else echo "BEFORE_WRITE" >> "$MASKLOG"; fi; }
DC() { echo "DC called: $*" >&2; return 1; }
EOF

# ── gotrue_mint ──
rnd() { local n="$1" i=0 s=""; while [ "$i" -lt "$n" ]; do s="$s$(printf '%02x' $((RANDOM % 256)))"; i=$((i+1)); done; printf '%s\n' "$s"; }
mint_run() { # mode: good | empty | short | noenv | mode644 | mode600
  W="$T/w-$1"; rm -rf "$W"; mkdir -p "$W"; ( umask 077; : > "$W/harness.env" ); [ "$1" != noenv ] || rm -f "$W/harness.env"
  ( set -u; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    case "$1" in
      empty) openssl() { :; };;
      short) openssl() { rnd 5; };;
      *)     openssl() { rnd "$3"; };;
    esac
    case "$1" in
      mode644) gt_enforces_modes() { return 0; }; gt_mode_of() { echo 644; };;
      mode600) gt_enforces_modes() { return 0; }; gt_mode_of() { echo 600; };;
      *)       gt_enforces_modes() { return 1; };;
    esac
    gotrue_mint ) 2>&1
}
out="$(mint_run good)"; rc=$?
verdict "gotrue_mint: a good mint" 0 "masked, none printed" "$out" "$rc"
env="$T/w-good/harness.env"
verdict "gotrue_mint: exactly two H_ lines in the env file, one of each name" 0 "two" "$([ "$(grep -c '^H_' "$env")" = 2 ] && [ "$(grep -c '^H_JWT_SECRET=' "$env")" = 1 ] && [ "$(grep -c '^H_GOTRUE_DB_PASSWORD=' "$env")" = 1 ] && echo two || echo "$(grep -c '^H_' "$env") lines")" 0
verdict "gotrue_mint: the JWT secret is 96 hex characters (48 bytes) and the password 32" 0 "96 and 32" "$(printf '%s and %s' "$(sed -n 's/^H_JWT_SECRET=//p' "$env" | tr -d '\n' | wc -c | tr -d ' ')" "$(sed -n 's/^H_GOTRUE_DB_PASSWORD=//p' "$env" | tr -d '\n' | wc -c | tr -d ' ')")" 0
verdict "gotrue_mint: both are hexadecimal (nothing a URL, a compose file or a shell would change)" 0 "all-hex-ok" "$(grep -E '^H_(JWT_SECRET|GOTRUE_DB_PASSWORD)=[0-9a-f]+$' "$env" | wc -l | tr -d ' ' | grep -qx 2 && echo all-hex-ok || echo "NOT-HEX-FAIL")" 0
verdict "gotrue_mint: both were masked BEFORE they were written to the env file" 0 "2 before, 0 after" "$(printf '%s before, %s after' "$(grep -c BEFORE_WRITE "$T/w-good/mask.log")" "$(grep -c AFTER_WRITE "$T/w-good/mask.log" || true)")" 0
sec="$(sed -n 's/^H_JWT_SECRET=//p' "$env")"; pw="$(sed -n 's/^H_GOTRUE_DB_PASSWORD=//p' "$env")"
case "$out" in *"$sec"*|*"$pw"*) leaked=yes;; *) leaked=no;; esac
verdict "gotrue_mint: the output never shows a secret" 0 "no" "$leaked" 0
out="$(mint_run empty)"; rc=$?
verdict "gotrue_mint: a tool that printed nothing is refused" 1 "could not mint GoTrue's JWT secret and database password" "$out" "$rc"
out="$(mint_run short)"; rc=$?
verdict "gotrue_mint: a secret of the wrong length is refused" 1 "GoTrue's JWT secret is 10 characters long, expected 96" "$out" "$rc"
out="$(mint_run noenv)"; rc=$?
verdict "gotrue_mint: no env file" 1 "the env file is missing" "$out" "$rc"
out="$(mint_run mode644)"; rc=$?
verdict "gotrue_mint: an env file that is mode 644 (where modes are kept)" 1 "is mode 644, expected 600" "$out" "$rc"
out="$(mint_run mode600)"; rc=$?
verdict "gotrue_mint: the same host with a mode-600 env file is green (the control of the case above)" 0 "masked, none printed" "$out" "$rc"

# ── the stand-ins of the stage: docker compose, docker and psql, driven by SC_* variables ──
cat > "$T/stage-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
WORK="$T/sw"
bump() { local n; n="$(cat "$T/sc.$1" 2>/dev/null || echo 0)"; n=$((n+1)); echo "$n" > "$T/sc.$1"; echo "$n"; }
DC() {
  case "$1" in
    exec)  printf '%s\n' "$*" > "$T/sc.alter-args"; cat > "$T/sc.alter-stdin"
           if [ "${SC_ALTER_FAIL:-0}" = 1 ]; then echo "ERROR:  syntax error at or near \"x\"" >&2; echo "LINE 1: ALTER ROLE supabase_auth_admin WITH PASSWORD '$GT_DB_PASS';" >&2; return 1; fi
           return 0;;
    up)    [ "${SC_UP_FAIL:-0}" = 1 ] && { echo "Error response from daemon: driver failed"; return 1; }; return 0;;
    ps)    [ "${SC_NOCID:-0}" = 1 ] || echo cid-gotrue; return 0;;
    logs)  echo "GOTRUE-LOG-MARKER the logs of gotrue"; return 0;;
    pull)  [ "${SC_PULL_FAIL:-0}" = 1 ] && { echo "manifest for the digest not found"; return 1; }; return 0;;
    config) echo "supabase/gotrue:v0.0.0@sha256:0000000000000000000000000000000000000000000000000000000000000000"; return 0;;
    run)   case "${SC_PROBE:-ok}" in
             ok)      echo '{"version":"v0.0.0-test","name":"GoTrue","description":"stand-in"}'; return 0;;
             refused) echo "wget: can't connect to remote host (172.19.0.5): Connection refused"; return 1;;
             notjson) echo "<html>another service</html>"; return 0;;
           esac;;
  esac
  return 0
}
docker() {   # docker inspect -f <format> <container>
  local fmt="$3" n
  case "$fmt" in
    *Health.Status*) n="$(bump health)"; if [ "$n" -le "${SC_HEALTH_AFTER:-0}" ]; then echo starting; else echo "${SC_HEALTH:-healthy}"; fi;;
    *State.Status*)  echo "${SC_STATE:-running}";;
    *ExitCode*)      echo "${SC_EXIT:-1}";;
  esac
}
gt_psql_su() {
  local n
  case "$1" in
    *rolcanlogin*) case "${SC_ROLE:-ok}" in ok) echo "true|false|true";; none) echo "";; super) echo "true|true|true";; nologin) echo "false|false|true";; esac;;
    *"from auth.schema_migrations"*) n="$(bump mig)"; if [ "$n" = 1 ]; then echo "${SC_MIG_BEFORE:-12}"; else echo "${SC_MIG_AFTER:-87}"; fi;;
    *pg_stat_activity*) echo 2;;
    *tableowner*) echo "auth.users is owned by supabase_auth_admin, the schema auth by supabase_auth_admin";;
  esac
}
EOF
sg() { # run one function of stage.sh under the stand-ins; the caller sets SC_* with a prefix:  SC_X=1 sg function args
  ( set -u; rm -rf "$T/sw"; rm -f "$T"/sc.*; mkdir -p "$T/sw"
    export GT_SLEEP=0 GT_WAIT_TRIES="${GT_WAIT_TRIES:-4}"
    . "$HERE/stage.sh"; . "$T/stage-env.sh"
    GT_JWT_SECRET="${SC_JWT:-$(rnd 48)}"; GT_DB_PASS="${SC_PASS:-$(rnd 16)}"
    "$@" ) 2>&1
}

# ── gotrue_pull ──
out="$(sg gotrue_pull)"; rc=$?
verdict "gotrue_pull: the runner can pull the pinned digest" 0 "the digest-pinned GoTrue image is pulled: supabase/gotrue:v0.0.0@sha256:" "$out" "$rc"
out="$(SC_PULL_FAIL=1 sg gotrue_pull)"; rc=$?
verdict "gotrue_pull: a runner that cannot pull the digest is red and the pin is not touched" 1 "the runner could not pull the digest-pinned GoTrue image of docker-compose.gotrue.yml: the pin is NOT changed from the runner's side" "$out" "$rc"
verdict "gotrue_pull: ... and the pull's own error is printed" 1 "manifest for the digest not found" "$out" "$rc"

# ── gotrue_stage ──
out="$(sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a good start" 0 "GoTrue answers /health from a container on the private network, by name:" "$out" "$rc"
verdict "gotrue_stage: ... the role is read and printed" 0 "the role supabase_auth_admin: (login, superuser, createrole) = (true|false|true)" "$out" "$rc"
verdict "gotrue_stage: ... the migrations are read back, before and after" 0 "GoTrue's own migrations: auth.schema_migrations held 12 row(s) before GoTrue started and holds 87 now" "$out" "$rc"
out="$( ( rm -rf "$T/sw"; rm -f "$T"/sc.*; mkdir -p "$T/sw"; SC_PASS=deadbeefdeadbeefdeadbeefdeadbeef; export SC_PASS; sg gotrue_stage >/dev/null; cat "$T/sc.alter-stdin" ) 2>&1 )"
verdict "gotrue_stage: the SQL on stdin sets the role's password from a psql variable and never carries it" 0 "ALTER ROLE supabase_auth_admin WITH PASSWORD :'pw';" "$out" 0 "deadbeefdeadbeef"
args="$( ( rm -rf "$T/sw"; rm -f "$T"/sc.*; mkdir -p "$T/sw"; SC_PASS=deadbeefdeadbeefdeadbeefdeadbeef; export SC_PASS; sg gotrue_stage >/dev/null; cat "$T/sc.alter-args" ) 2>&1 )"
verdict "gotrue_stage: ... the password reaches psql as -v pw=, as the superuser supabase_admin" 0 "-U supabase_admin" "$args" 0
verdict "gotrue_stage: ... and as the variable pw" 0 "-v pw=deadbeefdeadbeef" "$args" 0
out="$(SC_ROLE=none sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: the image has no such role" 1 "the database has no role supabase_auth_admin" "$out" "$rc"
out="$(SC_ROLE=super sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a superuser role is refused" 1 "is (login, superuser, createrole) = (true|true|true), expected a login role that is NOT a superuser" "$out" "$rc"
out="$(SC_ROLE=nologin sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a role that cannot log in is refused" 1 "is (login, superuser, createrole) = (false|false|true), expected a login role" "$out" "$rc"
out="$( SC_ALTER_FAIL=1 SC_PASS=cafebabecafebabecafebabecafebabe sg gotrue_stage )"; rc=$?
verdict "gotrue_stage: a password that cannot be set" 1 "could not set the password of supabase_auth_admin as the image's superuser supabase_admin" "$out" "$rc" "cafebabecafebabe"
out="$(SC_UP_FAIL=1 sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: GoTrue does not start" 1 "GoTrue did not start (docker compose up -d gotrue failed)" "$out" "$rc"
out="$(SC_STATE=exited SC_HEALTH=starting SC_EXIT=3 sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a container that exits" 1 "GoTrue's container is 'exited' (exit code 3)" "$out" "$rc" "answers /health"
verdict "gotrue_stage: ... and its log is printed before the stage dies" 1 "GOTRUE-LOG-MARKER the logs of gotrue" "$out" "$rc"
verdict "gotrue_stage: ... and the message says where to look" 1 "a 'permission denied' or 'must be owner' there is about the facts printed before it" "$out" "$rc"
out="$(SC_HEALTH=starting sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a container that never gets healthy" 1 "GoTrue was not healthy after" "$out" "$rc" "answers /health"
verdict "gotrue_stage: ... and its log is printed (never healthy)" 1 "GOTRUE-LOG-MARKER the logs of gotrue" "$out" "$rc"
out="$(SC_HEALTH=unhealthy sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: a container that says unhealthy" 1 "GoTrue's own health check says unhealthy" "$out" "$rc" "answers /health"
out="$(SC_HEALTH_AFTER=2 GT_WAIT_TRIES=6 sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: healthy after two polls that said starting (the bounded loop waits)" 0 "GoTrue is healthy by its own check after" "$out" "$rc"
out="$(SC_PROBE=refused sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: healthy by its own check, but the name does not answer from the private network" 1 "healthy by its own check but a request from a container on the private network to http://gotrue:9999/health failed" "$out" "$rc"
out="$(SC_PROBE=notjson sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: something that is not GoTrue answers at the name" 1 "answered, but not with JSON" "$out" "$rc"
out="$(SC_MIG_AFTER=0 sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: auth.schema_migrations empty after GoTrue is healthy" 1 "is EMPTY after GoTrue reported healthy" "$out" "$rc"
out="$(SC_MIG_AFTER=x sg gotrue_stage)"; rc=$?
verdict "gotrue_stage: auth.schema_migrations answers something that is not a number" 1 "did not answer a number" "$out" "$rc"
out2="$( ( set -u; rm -rf "$T/sw"; mkdir -p "$T/sw"; . "$HERE/stage.sh"; . "$T/stage-env.sh"; gotrue_stage ) 2>&1 )"; rc2=$?
verdict "gotrue_stage: the secrets were never minted" 1 "GoTrue's secrets were not minted" "$out2" "$rc2"

# ── gotrue_require_pass ──
pass_run() { # log
  ( set -u; WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; gotrue_require_pass "$1" ) 2>&1
}
all="$( . "$HERE/stage.sh"; for name in "${GT_TESTS[@]}"; do printf -- '--- PASS: %s (1.00s)\n' "$name"; done )"
ntests="$( . "$HERE/stage.sh"; echo "${#GT_TESTS[@]}" )"
[ "$ntests" -ge 5 ] || { echo "selftest: RED — stage.sh lists only $ntests test(s): the brief names five at least"; exit 1; }
printf '%s\n' "$all" > "$T/pass-all.log"
out="$(pass_run "$T/pass-all.log")"; rc=$?
verdict "gotrue_require_pass: every listed test passed" 0 "$ntests TestGoTrue_* tests have a '--- PASS' line" "$out" "$rc"
for name in $( . "$HERE/stage.sh"; printf '%s\n' "${GT_TESTS[@]}" ); do
  printf '%s\n' "$all" | grep -v "$name " > "$T/pass-missing.log"
  out="$(pass_run "$T/pass-missing.log")"; rc=$?
  verdict "gotrue_require_pass: $name has no PASS line" 1 "$name has no '--- PASS' line" "$out" "$rc"
done
first="$( . "$HERE/stage.sh"; echo "${GT_TESTS[0]}" )"
printf '%s\n' "$all" | sed "s/^--- PASS: $first/--- FAIL: $first/" > "$T/pass-failed.log"
out="$(pass_run "$T/pass-failed.log")"; rc=$?
verdict "gotrue_require_pass: a FAILED test is not a PASS" 1 "$first has no '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | sed "s/^--- PASS: $first/    --- PASS: $first/" > "$T/pass-indented.log"
out="$(pass_run "$T/pass-indented.log")"; rc=$?
verdict "gotrue_require_pass: an indented line (a subtest, or a mention) is not a top-level PASS" 1 "$first has no '--- PASS' line" "$out" "$rc"
: > "$T/pass-empty.log"
out="$(pass_run "$T/pass-empty.log")"; rc=$?
verdict "gotrue_require_pass: an empty log passes nothing" 1 "$first has no '--- PASS' line" "$out" "$rc"

# ── gotrue_secret_leak ──
b64u() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '+/' '-_'; }
FAKE_JWT="$(b64u '{"alg":"HS256","typ":"JWT"}').$(b64u '{"sub":"stand-in","role":"authenticated"}').$(b64u 'a-signature-of-some-length')"   # built here, never stored: a token-shaped string must not sit in a committed file
leak_run() { # <text of the log of gotrue> <text of the run's own log> [secrets]
  rm -rf "$T/lk"; mkdir -p "$T/lk"; printf '%s' "$1" > "$T/lk/gotrue.txt"; [ -z "${2+x}" ] || printf '%s\n' "$2" > "$T/lk/h-run.log"
  ( set -u; WORK="$T/lk"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    DC() { if [ "$1" = logs ]; then cat "$T/lk/gotrue.txt"; else return 1; fi; }
    GT_JWT_SECRET="${LK_JWT-jwtsecretjwtsecretjwtsecretjwtsecretjwtsecretjwtsecret}"; GT_DB_PASS="${LK_PASS-dbpassdbpassdbpassdbpass}"
    gotrue_secret_leak ) 2>&1
}
out="$(leak_run 'GoTrue API started on 0.0.0.0:9999; migrations applied' 'signed up user; token verified')"; rc=$?
verdict "gotrue_secret_leak: clean logs" 0 "appears in the log of gotrue or in this run's own test log" "$out" "$rc"
out="$(leak_run 'config jwt secret jwtsecretjwtsecretjwtsecretjwtsecretjwtsecretjwtsecret loaded' 'fine')"; rc=$?
verdict "gotrue_secret_leak: the JWT secret in GoTrue's log is red, names the log, shows nothing" 1 "appears in leak-gotrue.log (the log is not shown" "$out" "$rc" "jwtsecretjwtsecret"
out="$(leak_run 'fine' 'dsn password dbpassdbpassdbpassdbpass')"; rc=$?
verdict "gotrue_secret_leak: the database password in the run's own log is red" 1 "appears in h-run.log (the log is not shown" "$out" "$rc" "dbpassdbpass"
out="$(leak_run 'fine' "request Authorization: Bearer $FAKE_JWT")"; rc=$?
verdict "gotrue_secret_leak: a JWT-shaped string in the run's own log is red (a token a test printed)" 1 "something shaped like a JWT" "$out" "$rc" "$FAKE_JWT"
verdict "gotrue_secret_leak: ... naming the log it was in" 1 "appears in h-run.log" "$out" "$rc"
out="$(leak_run "access_token=$FAKE_JWT" 'fine')"; rc=$?
verdict "gotrue_secret_leak: a JWT-shaped string in GoTrue's log is red" 1 "something shaped like a JWT" "$out" "$rc" "$FAKE_JWT"
out="$(leak_run 'a header eyJhbGci on its own, and a.b.c, and eyJ.x.y' 'fine')"; rc=$?
verdict "gotrue_secret_leak: strings that only start like a token are not a token (the pattern is not absurdly broad)" 0 "appears in the log of gotrue" "$out" "$rc"
out="$(leak_run '' 'fine')"; rc=$?
verdict "gotrue_secret_leak: an empty log of GoTrue is a scan of nothing" 1 "the log leak-gotrue.log, which the leak check must read, is missing or empty" "$out" "$rc"
out="$(leak_run 'fine')"; rc=$?
verdict "gotrue_secret_leak: no run log at all" 1 "the log h-run.log, which the leak check must read, is missing or empty" "$out" "$rc"
out="$(LK_JWT= leak_run 'fine' 'fine')"; rc=$?
verdict "gotrue_secret_leak: an empty secret would match every line: refused" 1 "the secrets of this run are empty" "$out" "$rc"

# ── gotrue_not_checked ──
out="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; gotrue_not_checked )"; rc=$?
verdict "gotrue_not_checked: names the asymmetric keys and the JWKS endpoint" 0 "asymmetric signing keys and its JWKS endpoint" "$out" "$rc" "CORRECTION"
verdict "gotrue_not_checked: ... a hosted project's real configuration" 0 "a Supabase hosted project's real configuration" "$out" "$rc"
verdict "gotrue_not_checked: ... token refresh, MFA and the mail flow" 0 "token refresh and sign-out" "$out" "$rc"
verdict "gotrue_not_checked: ... the mail flow" 0 "the mail flow" "$out" "$rc"
verdict "gotrue_not_checked: ends with one grep-able line that starts with NOT CHECKED:" 0 "line-ok" "$(printf '%s\n' "$out" | grep -q "^NOT CHECKED: GoTrue's asymmetric signing keys and JWKS endpoint" && echo line-ok || echo LINE-MISSING)" 0
out="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; gotrue_not_checked correction )"; rc=$?
verdict "gotrue_not_checked correction: says the older bullet no longer holds" 0 "CORRECTION to the \"real Supabase beyond the database image\" bullet above" "$out" "$rc"

# ── the wiring of the scope run: its text is read (it needs Docker to run) ──
wire="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; declare -f gotrue_scope_run )"
verdict "gotrue_scope_run: starts GoTrue through gotrue_stage" 0 "gotrue_stage" "$wire" 0
verdict "gotrue_scope_run: compiles the binary with the tags the run computed" 0 'go test -tags "$TEST_TAGS" -c' "$wire" 0
verdict "gotrue_scope_run: runs only the TestGoTrue tests" 0 '-test.run "$re"' "$wire" 0
verdict "gotrue_scope_run: requires every PASS line, the scope's floor, the leak check and the exposure check" 0 "gotrue_require_pass" "$wire" 0
verdict "gotrue_scope_run: ... the floor of the scope is the number of tests the scope names" 0 'go|%s|%s' "$wire" 0
verdict "gotrue_scope_run: ... the leak check" 0 "gotrue_secret_leak" "$wire" 0
verdict "gotrue_scope_run: ... the exposure check lists postgres and gotrue" 0 "check-exposure.sh" "$wire" 0
verdict "gotrue_scope_run: ends the run" 0 "exit 0" "$wire" 0

# ── consistency of the files of the stage ──
GOF="$HARNESS/gotrue_test.go"; GOF2="$HARNESS/gotrue_api_test.go"; COMPOSE="$HARNESS/docker-compose.gotrue.yml"; RUNSH="$HARNESS/run.sh"
listed="$( . "$HERE/stage.sh"; printf '%s\n' "${GT_TESTS[@]}" | sort )"
declared="$(grep -ho '^func TestGoTrue_[A-Za-z0-9_]*(' "$GOF" "$GOF2" | sed 's/^func //; s/($//' | sort)"
verdict "consistency: GT_TESTS is exactly the set of TestGoTrue_ functions of gotrue_test.go and gotrue_api_test.go (a test added without the list, or a listed test that does not exist, is red)" 0 "same" "$([ -n "$declared" ] && [ "$listed" = "$declared" ] && echo same || printf 'listed: %s\ndeclared: %s' "$(printf '%s' "$listed" | tr '\n' ' ')" "$(printf '%s' "$declared" | tr '\n' ' ')")" 0
verdict "consistency: GT_TESTS has no name twice" 0 "unique" "$([ -z "$(printf '%s\n' "$listed" | uniq -d)" ] && echo unique || echo duplicated)" 0
verdict "consistency: the Go file carries the build tag harness && gotrue on its first line" 0 "tagged" "$([ "$(head -1 "$GOF")" = '//go:build harness && gotrue' ] && [ "$(head -1 "$GOF2")" = '//go:build harness && gotrue' ] && echo tagged || echo "first lines: $(head -1 "$GOF") | $(head -1 "$GOF2")")" 0
nocontrol=""
for name in $declared; do
  body="$(awk -v n="func $name(" 'index($0, n)==1 {p=1} p{print} p && $0=="}" {p=0}' "$GOF" "$GOF2")"
  printf '%s\n' "$body" | grep -qi 'control' || nocontrol="$nocontrol $name"
done
verdict "consistency: every TestGoTrue_ test names a control in its body (a paired control is a rule, not a hope)" 0 "controls-ok" "$([ -z "$nocontrol" ] && echo controls-ok || echo "NO-CONTROL-FAIL in:$nocontrol")" 0
verdict "consistency: the test file reads no secret from the source: no JWT-shaped literal" 0 "none" "$(cat "$GOF" "$GOF2" | grep -cE 'eyJ[A-Za-z0-9_-]{5,}\.eyJ' | grep -qx 0 && echo none || echo present)" 0
verdict "consistency: the compose image is pinned name:tag@sha256:<64 hex>" 0 "pin-ok" "$(grep -qE '^    image: supabase/gotrue:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$' "$COMPOSE" && echo pin-ok || echo "PIN-FAIL")" 0
verdict "consistency: the compose file publishes no port" 0 "none" "$(grep -qE '^[[:space:]]*ports:' "$COMPOSE" && echo "a ports: key" || echo none)" 0
verdict "consistency: the service is no-new-privileges, read-only and on the private network" 0 "hardened" "$(grep -q 'no-new-privileges:true' "$COMPOSE" && grep -q 'read_only: true' "$COMPOSE" && grep -q 'networks: \[private\]' "$COMPOSE" && echo hardened || echo missing)" 0
verdict "consistency: the service is in the profile gotrue (it cannot start by accident)" 0 "profiled" "$(grep -q 'profiles: \[gotrue\]' "$COMPOSE" && echo profiled || echo missing)" 0
verdict "consistency: GoTrue is configured for autoconfirm, no mail host and no published issuer drift (one anchor, used by GoTrue and by the test container)" 0 "configured" "$( [ "$(grep -c 'GOTRUE_MAILER_AUTOCONFIRM: "true"' "$COMPOSE")" = 1 ] && [ "$(grep -c 'GOTRUE_SMTP_HOST: ""' "$COMPOSE")" = 1 ] && [ "$(grep -c '\*gotrue-issuer' "$COMPOSE")" = 2 ] && [ "$(grep -c '^x-gotrue-issuer: &gotrue-issuer ' "$COMPOSE")" = 1 ] && echo configured || echo wrong)" 0
verdict "consistency: GoTrue has a health check on /health" 0 "checked" "$(grep -q 'http://127.0.0.1:9999/health' "$COMPOSE" && echo checked || echo missing)" 0
missing=""
for v in $(grep -o '\${H_[A-Z0-9_]*' "$COMPOSE" | sed 's/^\${//' | sort -u); do
  grep -q "^$v=" "$RUNSH" || grep -q "echo \"$v=" "$HERE/stage.sh" || missing="$missing $v"
done
verdict "consistency: every H_ variable the compose file needs is written by run.sh or by gotrue_mint" 0 "written" "$([ -z "$missing" ] && echo written || echo "nobody writes:$missing")" 0
missing=""
for v in $(grep -o '"HARNESS_GOTRUE_[A-Z_]*"' "$GOF" | tr -d '"' | sort -u); do grep -q "^      $v:" "$COMPOSE" || missing="$missing $v"; done
nvars="$(grep -o '"HARNESS_GOTRUE_[A-Z_]*"' "$GOF" | sort -u | wc -l | tr -d ' ')"
verdict "consistency: the test container gets every HARNESS_GOTRUE_ variable the Go file reads (and the Go file reads three)" 0 "given" "$([ -z "$missing" ] && [ "$nvars" = 3 ] && echo given || echo "missing:$missing, the Go file names $nvars")" 0
verdict "consistency: run.sh allows the scope gotrue" 0 "scope-allowed-ok" "$(grep -q 'case "\$SCOPE" in full|m1node|m2worker|gotrue|jobs) ;;' "$RUNSH" && echo scope-allowed-ok || echo "SCOPE-NOT-ALLOWED-FAIL")" 0
verdict "consistency: run.sh sources the stage and ends the scope through gotrue_scope_run" 0 "wired" "$(grep -q '^\. "\$HERE/gotrue/stage.sh"' "$RUNSH" && grep -q '^\[ "\$SCOPE" = gotrue \] && gotrue_scope_run' "$RUNSH" && echo wired || echo missing)" 0
verdict "consistency: the scope builds only the four migration images (GoTrue is pulled by digest, never built)" 0 "scoped" "$(grep -q 'gotrue)   BUILD_ONLY="migrate-platform migrate-m1 migrate-m2 migrate-m3";;' "$RUNSH" && echo scoped || echo missing)" 0
# every call of a gotrue_ function in run.sh sits under its guard: GT_ON is 1, or the scope is gotrue (the line that sources the stage is the one exception)
unguarded="$(grep -nE '(^|[^_a-z])gotrue_(mint|pull|stage|evidence|require_pass|secret_leak|scope_run|not_checked)( |$)' "$RUNSH" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE '\[ "\$GT_ON" = 1 \] &&|\[ "\$SCOPE" = gotrue \] &&' | head -5)"
verdict "consistency: every call of a gotrue_ function in run.sh is under the GT_ON or the scope guard (the stage is dormant otherwise)" 0 "all-guarded-ok" "$([ -z "$unguarded" ] && echo all-guarded-ok || echo "UNGUARDED-FAIL: $unguarded")" 0
ncalls="$(grep -cE '\[ "\$GT_ON" = 1 \] && gotrue_(mint|pull|stage|evidence|require_pass|secret_leak|not_checked)' "$RUNSH")"
verdict "consistency: run.sh calls the seven stage functions of the full run (mint, pull, stage, evidence, require_pass, secret_leak, not_checked)" 0 "seven" "$([ "$ncalls" = 7 ] && echo seven || echo "$ncalls")" 0
verdict "consistency: the exposure check of the full run lists gotrue when it is on" 0 "listed" "$(grep -q 'm3-worker-3 \$M2W_EXPOSE \$GT_EXPOSE' "$RUNSH" && grep -q 'GT_EXPOSE="gotrue"' "$RUNSH" && echo listed || echo missing)" 0

# DORMANCY, executed on the real text of run.sh: the guard block is cut out of it and evaluated for four settings
block="$(sed -n '/^GT_ON=0; GT_DC=(); GT_EXPOSE=""$/,/^\[ "\$GT_ON" = 1 \] && { GT_DC=/p' "$RUNSH")"
verdict "dormancy: the guard block of run.sh was found (its subject is not empty)" 0 "found" "$([ "$(printf '%s\n' "$block" | wc -l | tr -d ' ')" = 4 ] && echo found || echo "$(printf '%s\n' "$block" | wc -l) lines")" 0
gt_state() { # scope full-includes
  ( set -u; HERE=/h; TEST_TAGS=harness; SCOPE="$1"; FULL_INCLUDES="$2"; eval "$block"; printf 'GT_ON=%s tags=[%s] files=%s expose=[%s]' "$GT_ON" "$TEST_TAGS" "${#GT_DC[@]}" "$GT_EXPOSE" )
}
verdict "dormancy: the full run with FULL_INCLUDES as it stands in run.sh is exactly what it names" 0 "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1 | grep -qw gotrue && echo 'GT_ON=1 tags=[harness gotrue] files=4' || echo 'GT_ON=0 tags=[harness] files=0 expose=[]')" "$(gt_state full "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1)")" 0
verdict "dormancy: the full run with FULL_INCLUDES = m2worker m1node: no compose file, no tag, nothing exposed" 0 "GT_ON=0 tags=[harness] files=0 expose=[]" "$(gt_state full 'm2worker m1node')" 0
verdict "dormancy: the full run with FULL_INCLUDES empty" 0 "GT_ON=0 tags=[harness] files=0 expose=[]" "$(gt_state full '')" 0
verdict "dormancy: a name that merely contains gotrue does not switch it on" 0 "GT_ON=0 tags=[harness] files=0 expose=[]" "$(gt_state full 'notgotrue gotrue2')" 0
verdict "dormancy: the scope m2worker (FULL_INCLUDES names gotrue) does not switch it on" 0 "GT_ON=0 tags=[harness] files=0 expose=[]" "$(gt_state m2worker 'gotrue')" 0
verdict "dormancy: the scope gotrue switches it on: the compose file and profile, the tag, the exposure" 0 "GT_ON=1 tags=[harness gotrue] files=4 expose=[gotrue]" "$(gt_state gotrue '')" 0
verdict "dormancy: the full run with FULL_INCLUDES naming gotrue switches it on (the lead's one-word change)" 0 "GT_ON=1 tags=[harness gotrue] files=4 expose=[gotrue]" "$(gt_state full 'm2worker m1node gotrue')" 0

# the workflows
verdict "workflows: the harness workflow offers the scope gotrue" 0 "offered" "$(grep -qE '^          - gotrue$' "$ROOT/.github/workflows/harness.yml" && echo offered || echo missing)" 0
verdict "workflows: the gates workflow runs this selftest" 0 "wired" "$(grep -q 'tests/harness/gotrue/stage-selftest.sh' "$ROOT/.github/workflows/gates.yml" && echo wired || echo missing)" 0
verdict "workflows: the image-pin gate's floor was raised by one (the GoTrue image is one more reference)" 0 "raised" "$(grep -q 'check-image-pins.sh --min-refs 6 \.' "$ROOT/.github/workflows/gates.yml" && echo raised || echo missing)" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; bad=$((bad+1)); fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
