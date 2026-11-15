#!/usr/bin/env bash
# run-leg.sh — run ONE fault-proof leg: show that a gate goes red, FOR THE NAMED REASON, when ci/faults/<phase>/<gate>.patch is applied.
#
#   run-leg.sh <phase> <gate>                 run the leg (what the fault-proofs workflow runs, one matrix leg per .expect)
#   run-leg.sh --check-expect <file.expect>   only parse and validate one .expect file (scripts/check-fault-patches.sh calls this, so there is ONE parser)
#
# WHY: a gate that has never been seen red is not known to work, and a red for the wrong reason (the fault broke the setup, the command is mistyped, the
# control was already red) proves nothing (false-green class 10). A leg applies one fault to a scratch copy and passes ONLY IF all four hold:
#   (1) the gate command exits 0 on an UNPATCHED pristine copy (the paired control);
#   (2) the patch applies to a pristine copy;
#   (3) the SETUP steps still succeed on the patched copy (otherwise the red would be for the wrong reason);
#   (4) the gate command exits NON-ZERO on the patched copy and its output contains every MESSAGE line (fixed-string match).
# The final line is `FAULT-PROOF PASS gate=<g> step=<s> exit=<rc> messages=<n>` or `FAULT-PROOF FAIL gate=<g>: <the condition that failed>`.
# Exit status: 0 pass, 1 fail, 2 usage error. The .expect format, the exported variables and the setup tokens are in ci/faults/README.md.
#
# Environment: FAULT_REPO_ROOT (the repository to test; default: the one this script lives in), FAULT_MODULES_DIR (a directory holding the three module
# checkouts; otherwise they are fetched with tools/fetch-modules.sh and the DEPLOY_KEY_COLDMAIL_* variables), FAULT_PG_ADMIN_DSN (default
# postgres://postgres:ci@localhost:5432/postgres; psql creates one fresh database per side from it, and ONLY when a SETUP token needs one).
#
# The gate step is captured with `tee` under pipefail and the MESSAGE lines are matched against that captured log, never against a summary (Phase 0 rule 4).
# The .expect file is read line by line: it is never sourced and never eval'ed.
#
# NOT CHECKED: that the copies hold anything newer than the last commit (copies are `git archive HEAD`; the patch file itself is read from the working tree);
# which line of the output a MESSAGE came from (it may match any line of the gate step); the commits of modules given by FAULT_MODULES_DIR (used as given);
# that the SETUP of a Windows rehearsal behaves like the runner's (only the ubuntu-latest run counts).
set -u -o pipefail

MODULE_NAMES=(coldmail-verify coldmail-send-managed coldmail-send-byo)
KNOWN_TOKENS="platform-up m1-chain db modules"
DEFAULT_ADMIN_DSN="postgres://postgres:ci@localhost:5432/postgres"

usage() {
  echo "usage: run-leg.sh <phase> <gate>" >&2
  echo "       run-leg.sh --check-expect <file.expect>" >&2
  exit 2
}
name_ok() { [[ "$1" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; }

MODE=run; PHASE=""; GATE=""; EXPECT_ARG=""
case "${1:-}" in
  --check-expect) [ $# -eq 2 ] || usage; MODE=check; EXPECT_ARG="$2";;
  -h|--help)      usage;;
  -*)             echo "run-leg: unknown option: $1" >&2; usage;;
  *)              [ $# -eq 2 ] || usage
                  PHASE="$1"; GATE="$2"
                  if ! name_ok "$PHASE" || ! name_ok "$GATE"; then
                    echo "run-leg: phase and gate must match ^[a-z0-9][a-z0-9_-]*\$ (got phase '$PHASE', gate '$GATE')" >&2; exit 2
                  fi;;
esac

# ---- the .expect parser (no sourcing, no eval) -------------------------------------------------------------------------------------------
# parse_expect <file> <the gate name the file name implies>: sets E_* and PROBLEMS; returns 0 only when PROBLEMS is empty.
known_token() { case " $KNOWN_TOKENS " in *" $1 "*) return 0;; esac; return 1; }
module_known() { local m; for m in "${MODULE_NAMES[@]}"; do [ "$m" = "$1" ] && return 0; done; return 1; }
PROBLEMS=()
problem() { PROBLEMS+=("$*"); }
parse_expect() {
  local f="$1" want_gate="$2" line key val n=0 t setup_raw=""
  local have_gate=0 have_target=0 have_setup=0 have_step=0 have_cmd=0
  PROBLEMS=(); E_GATE=""; E_TARGET=""; E_STEP=""; E_CMD=""; E_SETUP=(); E_MSGS=()
  E_TARGET_KIND=""; E_TARGET_MODULE=""; E_NEEDS_DB=0; E_NEEDS_MODULES=0
  if [ ! -f "$f" ]; then problem "no such file"; return 1; fi
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1)); line="${line//$'\r'/}"
    if [[ "$line" =~ ^[[:space:]]*(#.*)?$ ]]; then continue; fi
    case "$line" in
      *=*) ;;
      *) problem "line $n: not KEY=value: '$line'"; continue;;
    esac
    key="${line%%=*}"; val="${line#*=}"
    if ! [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]]; then
      problem "line $n: bad key '$key' (keys are UPPERCASE letters, digits and _, with nothing around the =)"; continue
    fi
    case "$key" in
      GATE)    [ "$have_gate" -eq 0 ]   || problem "line $n: duplicate key GATE";   have_gate=1;   E_GATE="$val";;
      TARGET)  [ "$have_target" -eq 0 ] || problem "line $n: duplicate key TARGET"; have_target=1; E_TARGET="$val";;
      SETUP)   [ "$have_setup" -eq 0 ]  || problem "line $n: duplicate key SETUP";  have_setup=1;  setup_raw="$val";;
      STEP)    [ "$have_step" -eq 0 ]   || problem "line $n: duplicate key STEP";   have_step=1;   E_STEP="$val";;
      CMD)     [ "$have_cmd" -eq 0 ]    || problem "line $n: duplicate key CMD";    have_cmd=1;    E_CMD="$val";;
      MESSAGE) E_MSGS+=("$val");;
      *)       problem "line $n: unknown key '$key' (allowed: GATE TARGET SETUP STEP CMD MESSAGE)";;
    esac
  done < "$f"
  if ! name_ok "$want_gate"; then problem "the file name '$want_gate.expect' is not usable: the gate name must match ^[a-z0-9][a-z0-9_-]*\$ (the runner and the workflow refuse other names)"; fi
  [ "$have_gate" -eq 1 ] || problem "GATE is missing"
  if [ "$have_gate" -eq 1 ] && [ "$E_GATE" != "$want_gate" ]; then
    problem "GATE is '$E_GATE' but the file name says '$want_gate' (GATE must equal the file basename)"
  fi
  if [ "$have_target" -eq 0 ]; then problem "TARGET is missing"
  elif [ "$E_TARGET" = platform ]; then E_TARGET_KIND=platform
  elif [ "${E_TARGET#module:}" != "$E_TARGET" ] && module_known "${E_TARGET#module:}"; then E_TARGET_KIND=module; E_TARGET_MODULE="${E_TARGET#module:}"
  else problem "TARGET is '$E_TARGET' (want platform, or module:<name> where name is one of: ${MODULE_NAMES[*]})"; fi
  if [ "$have_step" -eq 0 ] || [ -z "$E_STEP" ]; then problem "STEP is missing or empty"; fi
  if [ "$have_cmd" -eq 0 ] || [ -z "$E_CMD" ]; then problem "CMD is missing or empty"; fi
  if [ "${#E_MSGS[@]}" -lt 1 ]; then problem "no MESSAGE line: at least one MESSAGE is required (without one a red for ANY reason would pass)"; fi
  for t in ${E_MSGS[@]+"${E_MSGS[@]}"}; do
    [ -n "$t" ] || problem "an empty MESSAGE would match any output"
  done
  read -r -a E_SETUP <<< "$setup_raw"
  for t in ${E_SETUP[@]+"${E_SETUP[@]}"}; do
    if ! known_token "$t"; then problem "unknown SETUP token '$t' (known: $KNOWN_TOKENS)"; continue; fi
    case "$t" in
      platform-up|db) E_NEEDS_DB=1;;
      m1-chain)       E_NEEDS_DB=1; E_NEEDS_MODULES=1;;
      modules)        E_NEEDS_MODULES=1;;
    esac
  done
  [ "$E_TARGET_KIND" != module ] || E_NEEDS_MODULES=1
  [ "${#PROBLEMS[@]}" -eq 0 ]
}

if [ "$MODE" = check ]; then
  base="${EXPECT_ARG##*/}"
  case "$base" in
    *.expect) ;;
    *) echo "$EXPECT_ARG: the file name must end in .expect"; exit 1;;
  esac
  if ! parse_expect "$EXPECT_ARG" "${base%.expect}"; then
    for t in "${PROBLEMS[@]}"; do echo "$EXPECT_ARG: $t"; done
    exit 1
  fi
  echo "ok TARGET=$E_TARGET MESSAGES=${#E_MSGS[@]} SETUP=${E_SETUP[*]-}"
  exit 0
fi

# ---- run mode ----------------------------------------------------------------------------------------------------------------------------
WORK=""; DBS=()
FAULT_ADMIN_DSN="${FAULT_PG_ADMIN_DSN:-$DEFAULT_ADMIN_DSN}"
VERDICT=""   # printed by cleanup as the LAST line of the run, after the databases are dropped; a run that dies without one still ends in a FAIL line
fail() { VERDICT="FAULT-PROOF FAIL gate=$GATE: $*"; exit 1; }
cleanup() {
  local rc=$? d
  trap - EXIT
  for d in ${DBS[@]+"${DBS[@]}"}; do
    if psql "$FAULT_ADMIN_DSN" -v ON_ERROR_STOP=1 -q -c "DROP DATABASE IF EXISTS \"$d\" WITH (FORCE)"; then echo "##[step] dropped database $d"
    else echo "WARNING: could not drop database $d" >&2; fi
  done
  if [ -n "$WORK" ]; then rm -rf "$WORK"; fi
  if [ -z "$VERDICT" ]; then
    VERDICT="FAULT-PROOF FAIL gate=$GATE: the runner stopped (exit status $rc) before reaching a verdict"
    [ "$rc" -ne 0 ] || rc=1
  fi
  echo "$VERDICT"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

ROOT_IN="${FAULT_REPO_ROOT:-$(dirname "${BASH_SOURCE[0]}")/../..}"
ROOT="$(cd "$ROOT_IN" 2>/dev/null && pwd)" || fail "the repository root '$ROOT_IN' is not a directory"
EXP="$ROOT/ci/faults/$PHASE/$GATE.expect"
PATCH="$ROOT/ci/faults/$PHASE/$GATE.patch"
[ -f "$EXP" ] || fail "no such file: ci/faults/$PHASE/$GATE.expect"
if ! parse_expect "$EXP" "$GATE"; then
  for t in "${PROBLEMS[@]}"; do echo "  problem: $t"; done
  fail "the .expect file is invalid: ${PROBLEMS[0]}"
fi
[ -f "$PATCH" ] || fail "no such file: ci/faults/$PHASE/$GATE.patch"
[ -s "$PATCH" ] || fail "(2) the patch file ci/faults/$PHASE/$GATE.patch is empty, so it cannot apply"
for t in git tar; do command -v "$t" >/dev/null 2>&1 || fail "the tool '$t' is not on PATH"; done
if [ "$E_NEEDS_DB" -eq 1 ]; then command -v psql >/dev/null 2>&1 || fail "psql is not on PATH, and SETUP '${E_SETUP[*]}' needs a database"; fi
git -C "$ROOT" rev-parse --verify -q HEAD >/dev/null 2>&1 || fail "$ROOT is not a git repository with a commit (the copies are made with git archive HEAD)"

WORK="$(mktemp -d)" || fail "cannot create a temporary directory"
echo "##[step] leg $PHASE/$GATE: target=$E_TARGET setup='${E_SETUP[*]-}' step='$E_STEP' messages=${#E_MSGS[@]} work=$WORK"

archive_to() { mkdir -p "$2" && git -C "$1" archive HEAD | tar -x -C "$2"; }
copy_module() {   # a module checkout (it has a .git) is copied as `git archive HEAD`; a plain directory is copied as it is
  mkdir -p "$2" || return 1
  if [ -e "$1/.git" ]; then git -C "$1" archive HEAD | tar -x -C "$2"; else cp -a "$1/." "$2/"; fi
}
echo "##[step] pristine copies of the platform (git archive HEAD): control and fault"
archive_to "$ROOT" "$WORK/ctl" || fail "cannot make the pristine control copy"
archive_to "$ROOT" "$WORK/flt" || fail "cannot make the pristine fault copy"

if [ "$E_NEEDS_MODULES" -eq 1 ]; then
  if [ -n "${FAULT_MODULES_DIR:-}" ]; then
    MODS_SRC="$FAULT_MODULES_DIR"
    [ -d "$MODS_SRC" ] || fail "FAULT_MODULES_DIR is not a directory: $MODS_SRC"
    echo "##[step] modules from FAULT_MODULES_DIR (used as they are: their commits are not compared with tools/modules.lock)"
  else
    missing=""
    for t in DEPLOY_KEY_COLDMAIL_VERIFY DEPLOY_KEY_COLDMAIL_SEND_MANAGED DEPLOY_KEY_COLDMAIL_SEND_BYO; do
      [ -n "${!t:-}" ] || missing="$missing${missing:+ }$t"
    done
    [ -z "$missing" ] || fail "the modules are needed but FAULT_MODULES_DIR is not set and these deploy keys are missing: $missing (set FAULT_MODULES_DIR to a directory holding the three modules, or export the keys)"
    echo "##[step] fetch the modules at the commits tools/modules.lock pins"
    ( cd "$WORK/ctl" && bash tools/fetch-modules.sh --lock tools/modules.lock "$WORK/mods" ) || fail "the modules could not be fetched at the commits tools/modules.lock pins"
    MODS_SRC="$WORK/mods"
    unset DEPLOY_KEY_COLDMAIL_VERIFY DEPLOY_KEY_COLDMAIL_SEND_MANAGED DEPLOY_KEY_COLDMAIL_SEND_BYO
  fi
  for t in "${MODULE_NAMES[@]}"; do
    [ -d "$MODS_SRC/$t" ] || fail "the module directory $MODS_SRC/$t does not exist"
    for side in ctl flt; do copy_module "$MODS_SRC/$t" "$WORK/$side-mods/$t" || fail "cannot copy the module $t for the $side side"; done
  done
fi

# ---- the setup tokens: one function each; the token list was validated by parse_expect, so nothing unknown reaches this point -------------
tok_platform_up() {
  local ext="" bin
  command -v go >/dev/null 2>&1 || { echo "platform-up: go is not on PATH"; return 1; }
  case "$(go env GOOS)" in windows) ext=".exe";; esac
  bin="$WORK/platform-migrate-$SIDE$ext"
  ( cd "$FAULT_PLATFORM" && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$bin" ./cmd/platform-migrate ) || { echo "platform-up: go build ./cmd/platform-migrate failed"; return 1; }
  # platform_0001 needs auth.users (Supabase Auth); a fresh database has none, so apply the same minimal fixture the harness uses (a no-op when a leg's own SETUP already made it)
  psql "$FAULT_DSN" -X -q -v ON_ERROR_STOP=1 -f "$FAULT_PLATFORM/tests/harness/fixtures/auth-users.sql" || { echo "platform-up: could not apply tests/harness/fixtures/auth-users.sql"; return 1; }
  PLATFORM_MIGRATE_DATABASE_URL="$FAULT_DSN" PLATFORM_MIGRATIONS_DIR="$FAULT_PLATFORM/db/migrations" "$bin" up || { echo "platform-up: platform-migrate up failed"; return 1; }
}
tok_m1_chain() {
  local dir="$FAULT_MODULES/coldmail-verify/control-plane/migrations" f n=0
  [ -d "$dir" ] || { echo "m1-chain: $dir does not exist"; return 1; }
  while IFS= read -r f; do
    n=$((n + 1)); echo "  applying ${f##*/}"
    psql "$FAULT_DSN" -v ON_ERROR_STOP=1 -q -f "$f" || { echo "m1-chain: ${f##*/} failed"; return 1; }
  done < <(find "$dir" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.sql' | LC_ALL=C sort)
  [ "$n" -gt 0 ] || { echo "m1-chain: found 0 migration files (NNNN_*.sql) in $dir — the instrument matched nothing"; return 1; }
}
run_token() {
  case "$1" in
    platform-up) tok_platform_up;;
    m1-chain)    tok_m1_chain;;
    db)          echo "  (a fresh database was requested: FAULT_DSN is set)";;
    modules)     echo "  (the modules were requested: FAULT_MODULES is set)";;
    *)           echo "run-leg: no function for setup token '$1'"; return 1;;
  esac
}

# ---- one database per side, only when a token asked for one -------------------------------------------------------------------------------
dsn_with_db() {   # dsn_with_db <postgres:// URL> <database name>: the URL with its database name replaced (a query string is kept)
  local dsn="$1" db="$2" query="" rest base
  case "$dsn" in *\?*) query="?${dsn#*\?}"; dsn="${dsn%%\?*}";; esac
  case "$dsn" in postgres://?*|postgresql://?*) ;; *) return 1;; esac
  rest="${dsn#*://}"
  case "$rest" in */*) base="${dsn%/*}";; *) base="$dsn";; esac
  printf '%s/%s%s' "$base" "$db" "$query"
}
create_db() {   # create_db <side>: sets FAULT_DSN to a fresh database fp_<gate>_<side>_<random>
  local slug name dsn rnd
  slug="$(printf '%s' "$GATE" | tr -c 'a-z0-9' '_')"; slug="${slug:0:30}"
  printf -v rnd '%04x%04x' "$RANDOM" "$RANDOM"
  name="fp_${slug}_$1_$rnd"
  dsn="$(dsn_with_db "$FAULT_ADMIN_DSN" "$name")" || fail "FAULT_PG_ADMIN_DSN must be a postgres:// or postgresql:// URL (its database name is replaced to build FAULT_DSN)"
  echo "##[step] create database $name ($1)"
  psql "$FAULT_ADMIN_DSN" -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$name\"" || fail "cannot create the database $name (is Postgres up at FAULT_PG_ADMIN_DSN, and may that role CREATE DATABASE?)"
  DBS+=("$name")
  FAULT_DSN="$dsn"; export FAULT_DSN
}

# ---- one side: set the variables, run SETUP in order, run the gate step with its output captured ------------------------------------------
SIDE=""; SETUP_FAILED=""; GATE_RC=0
run_side() {   # run_side ctl|flt: sets SETUP_FAILED (the token that failed, or "") and GATE_RC (the gate step's exit status)
  local tok st label
  SIDE="$1"; SETUP_FAILED=""; GATE_RC=0
  if [ "$SIDE" = ctl ]; then label="control, unpatched"; else label="fault, patched"; fi
  FAULT_PLATFORM="$WORK/$SIDE"
  if [ "$E_TARGET_KIND" = module ]; then FAULT_ROOT="$WORK/$SIDE-mods/$E_TARGET_MODULE"; else FAULT_ROOT="$FAULT_PLATFORM"; fi
  export FAULT_ROOT FAULT_PLATFORM FAULT_ADMIN_DSN
  if [ "$E_NEEDS_MODULES" -eq 1 ]; then FAULT_MODULES="$WORK/$SIDE-mods"; export FAULT_MODULES; else unset FAULT_MODULES; fi
  if [ "$E_NEEDS_DB" -eq 1 ]; then create_db "$SIDE"; else unset FAULT_DSN; fi
  for tok in ${E_SETUP[@]+"${E_SETUP[@]}"}; do
    echo "##[step] setup $tok [$label]"
    if ! run_token "$tok" </dev/null; then SETUP_FAILED="$tok"; return 0; fi
  done
  echo "##[step] $E_STEP [$label]"
  ( cd "$FAULT_ROOT" && exec bash -c "$E_CMD" </dev/null ) 2>&1 | tee "$WORK/$SIDE.log"
  st=("${PIPESTATUS[@]}")
  [ "${st[1]}" -eq 0 ] || fail "the output of the gate step could not be captured (tee exited ${st[1]})"
  GATE_RC="${st[0]}"
  echo "##[step] $E_STEP [$label]: exit status $GATE_RC"
}

# (1) the paired control: the gate command, unpatched, must exit 0
run_side ctl
[ -z "$SETUP_FAILED" ] || fail "(1) the control could not be set up: setup step '$SETUP_FAILED' failed on the unpatched copy, so nothing can be concluded from the patched copy"
[ "$GATE_RC" -eq 0 ] || fail "(1) the paired control is red: the gate command exited $GATE_RC on the unpatched copy (it must exit 0)"

# (2) the patch applies to a pristine copy (checked on the control copy, then applied to the fault copy)
if [ "$E_TARGET_KIND" = module ]; then
  CTL_BASE="$WORK/ctl-mods/$E_TARGET_MODULE"; FLT_BASE="$WORK/flt-mods/$E_TARGET_MODULE"; WHAT="the module $E_TARGET_MODULE"
else
  CTL_BASE="$WORK/ctl"; FLT_BASE="$WORK/flt"; WHAT="the platform"
fi
echo "##[step] apply ci/faults/$PHASE/$GATE.patch to a pristine copy of $WHAT"
( cd "$CTL_BASE" && git apply --check --whitespace=nowarn -- "$PATCH" ) || fail "(2) the patch does not apply to a pristine copy of $WHAT"
( cd "$FLT_BASE" && git apply --whitespace=nowarn -- "$PATCH" ) || fail "(2) the patch does not apply to a pristine copy of $WHAT"

# (3) the setup still works on the patched copy, (4) the gate fails there, with every message
run_side flt
[ -z "$SETUP_FAILED" ] || fail "(3) the fault broke the setup: setup step '$SETUP_FAILED' failed on the patched copy but passed on the unpatched copy, so a red from the gate would be for the wrong reason"
[ "$GATE_RC" -ne 0 ] || fail "(4) the gate command did not fail on the patched copy: it exited 0, so this gate does not catch the fault"
tr -d '\r' < "$WORK/flt.log" > "$WORK/flt.lf" || fail "cannot read the captured log of the fault side"
missing=0; first_missing=""
for t in "${E_MSGS[@]}"; do
  if grep -q -F -e "$t" -- "$WORK/flt.lf"; then echo "  message found:   $t"
  else echo "  message MISSING: $t"; missing=$((missing + 1)); [ -n "$first_missing" ] || first_missing="$t"; fi
done
[ "$missing" -eq 0 ] || fail "(4) the gate command exited $GATE_RC on the patched copy but its output is missing $missing of ${#E_MSGS[@]} expected message(s); first missing: '$first_missing'"
VERDICT="FAULT-PROOF PASS gate=$GATE step=$E_STEP exit=$GATE_RC messages=${#E_MSGS[@]}"
exit 0
