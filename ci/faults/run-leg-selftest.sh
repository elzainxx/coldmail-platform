#!/usr/bin/env bash
# run-leg-selftest.sh — proves ci/faults/run-leg.sh passes ONLY a good fault and fails, FOR THE REASON IT NAMES, on every way a leg can be wrong.
# Every case asserts the exact FAULT-PROOF line (or the named substring), not only the exit status (class 10). It needs no Postgres and no deploy keys:
#   * the fixture is a small git repository built in a temp directory and run through FAULT_REPO_ROOT; its gate (gate.sh) exits 1 with a message when
#     app/data.txt contains the word BROKEN (or ODD);
#   * the three modules come from FAULT_MODULES_DIR (one is a git repository, two are plain directories, so both copy paths run);
#   * `psql` is a STUB placed first on PATH: it records every call in $PSQL_LOG and fails any `-f <file>` whose file contains BROKEN, as a broken migration
#     would. So the database NAMES, the FAULT_DSN derivation, the creation and the drop are proven here; real SQL is not.
# NOT CHECKED here: the real psql, the real `go build ./cmd/platform-migrate` and `up` (setup token platform-up), the real fetch of the private modules
# (tools/fetch-modules.sh) — those run only in the fault-proofs workflow, on the runner.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; RUNLEG="$HERE/run-leg.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=29; cases=0; ok=0; bad=0; EXTRA_BAD=0
OUT=""; RC=0

# ---- the harness ------------------------------------------------------------------------------------------------------------------------
also() {   # also <description> <command...>: an extra assertion about the last run; the case is a mismatch when the command fails
  local d="$1"; shift
  if ! "$@"; then EXTRA_BAD=$((EXTRA_BAD + 1)); echo "      extra assertion failed: $d"; fi
}
check() {  # check <label> <want-rc> <item>...   item: =exact whole line | ~substring | ?extended regex matching a line
  local label="$1" want="$2" item kind txt miss=0
  shift 2; cases=$((cases + 1))
  [ "$RC" -eq "$want" ] || { miss=$((miss + 1)); echo "      exit status is $RC, want $want"; }
  for item in "$@"; do
    kind="${item:0:1}"; txt="${item:1}"
    case "$kind" in
      "=") printf '%s\n' "$OUT" | grep -q -F -x -e "$txt" || { miss=$((miss + 1)); echo "      missing line:      $txt"; };;
      "~") printf '%s\n' "$OUT" | grep -q -F -e "$txt"    || { miss=$((miss + 1)); echo "      missing substring: $txt"; };;
      "?") printf '%s\n' "$OUT" | grep -q -E -e "$txt"    || { miss=$((miss + 1)); echo "      no line matches:   $txt"; };;
      *)   miss=$((miss + 1)); echo "      selftest bug: item '$item' has no =, ~ or ? prefix";;
    esac
  done
  miss=$((miss + EXTRA_BAD)); EXTRA_BAD=0
  if [ "$miss" -eq 0 ]; then ok=$((ok + 1)); echo "  ok [$label] rc=$RC"
  else bad=$((bad + 1)); echo "  MISMATCH [$label]"; printf '%s\n' "$OUT" | sed 's/^/      | /'; fi
}
run() {    # run <repo> <gate> [env-args...]: one leg of phase fx against a fixture repository; sets OUT and RC
  local repo="$1" gate="$2"
  shift 2
  rm -f "$PSQL_LOG"
  OUT="$(env "$@" FAULT_DSN=poison-dsn FAULT_MODULES=poison-modules FAULT_REPO_ROOT="$repo" bash "$RUNLEG" fx "$gate" 2>&1)"; RC=$?
}
sh_run() { OUT="$("$@" 2>&1)"; RC=$?; }
no_psql_call() { [ ! -e "$PSQL_LOG" ]; }
no_apply_step() { ! printf '%s\n' "$OUT" | grep -q 'apply ci/faults'; }
has_text() { grep -q -F -e "$2" -- "$1"; }
chain_log() { grep -o '/[0-9][0-9][0-9][0-9]_[a-z]*\.sql' "$PSQL_LOG" | tr -d '/' | tr '\n' ' '; }
chain_is() { [ "$(chain_log)" = "$1" ]; }

# ---- fixtures ---------------------------------------------------------------------------------------------------------------------------
gitc() { git -c user.name=fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false -c core.autocrlf=false -c init.defaultBranch=main "$@"; }
mkrepo() { gitc -C "$1" init -q && gitc -C "$1" add -A && gitc -C "$1" commit -q -m fixture; }
mkpatch() {   # mkpatch <out> <path> <old line> <new line>: a one-line change to a one-line file
  printf 'diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-%s\n+%s\n' "$2" "$2" "$2" "$2" "$3" "$4" > "$1"
}
mkexpect() {  # mkexpect <gate> <target> <setup> <cmd> <message>...: writes $FXP/<gate>.expect
  local g="$1" target="$2" setup="$3" cmd="$4" m
  shift 4
  { printf 'GATE=%s\nTARGET=%s\nSETUP=%s\nSTEP=data check\nCMD=%s\n' "$g" "$target" "$setup" "$cmd"
    for m in "$@"; do printf 'MESSAGE=%s\n' "$m"; done; } > "$FXP/$g.expect"
}
write_gate() {  # the fixture gate: exits 1 with messages when app/data.txt holds BROKEN (or ODD); also reports what the runner exported
  cat > "$1/gate.sh" <<'EOF'
#!/usr/bin/env bash
set -u
echo "gate: root=$(basename "$FAULT_ROOT") dsn=${FAULT_DSN:-<unset>} modules=${FAULT_MODULES:+set}"
[ "$(cd "$FAULT_ROOT" && pwd -P)" = "$(pwd -P)" ] || { echo "gate: the working directory is not FAULT_ROOT"; exit 3; }
if grep -q BROKEN app/data.txt; then echo "gate-fail: BROKEN found"; echo "gate-fail: second line"; exit 1; fi
if grep -q ODD app/data.txt; then echo "gate-fail: something odd"; exit 1; fi
echo "gate: ok"
EOF
}

mkdir -p "$T/bin"
cat > "$T/bin/psql" <<'EOF'
#!/usr/bin/env bash
# selftest stand-in for psql: records the call, fails a -f file that contains BROKEN
log="${PSQL_LOG:-/dev/null}"
args=("$@")
printf 'psql' >> "$log"; for a in "$@"; do printf ' [%s]' "$a" >> "$log"; done; printf '\n' >> "$log"
file=""
for ((i = 0; i < ${#args[@]}; i++)); do if [ "${args[$i]}" = "-f" ]; then file="${args[$((i + 1))]}"; fi; done
if [ -n "$file" ] && grep -q BROKEN "$file"; then echo "psql: $file: ERROR: syntax error at or near BROKEN" >&2; exit 3; fi
exit 0
EOF
chmod +x "$T/bin/psql"
export PATH="$T/bin:$PATH" PSQL_LOG="$T/psql.log" FAULT_MODULES_DIR="$T/mods"
export FAULT_PG_ADMIN_DSN="postgres://fpuser:fppass@dbhost:6543/postgres?sslmode=disable"

# the modules: coldmail-verify is a git repository with a migration chain, the other two are plain directories
MODS="$T/mods"; mkdir -p "$MODS/coldmail-verify/control-plane/migrations" "$MODS/coldmail-verify/lib" "$MODS/coldmail-send-managed" "$MODS/coldmail-send-byo"
printf 'select 1;\n' > "$MODS/coldmail-verify/control-plane/migrations/0001_a.sql"
printf 'select 2;\n' > "$MODS/coldmail-verify/control-plane/migrations/0002_b.sql"
printf 'not a migration\n' > "$MODS/coldmail-verify/control-plane/migrations/notes.sql"
printf 'fine\n' > "$MODS/coldmail-verify/lib/x.txt"
cat > "$MODS/coldmail-verify/modgate.sh" <<'EOF'
#!/usr/bin/env bash
if grep -q BROKEN lib/x.txt; then echo "mod-gate: BROKEN lib"; exit 1; fi
echo "mod-gate: root=$(basename "$FAULT_ROOT") modules=$(ls "$FAULT_MODULES" | tr '\n' ' ')"
EOF
printf 'plain\n' > "$MODS/coldmail-send-managed/README"; printf 'plain\n' > "$MODS/coldmail-send-byo/README"
mkrepo "$MODS/coldmail-verify" || { echo "selftest: cannot build the module fixture"; exit 2; }
# a second module set whose coldmail-verify has no migration files at all
cp -a "$MODS" "$T/mods-nomig"; rm -rf "$T/mods-nomig/coldmail-verify/.git" "$T/mods-nomig/coldmail-verify/control-plane/migrations"; mkdir -p "$T/mods-nomig/coldmail-verify/control-plane/migrations"

# the platform fixture: one gate, and one fault per way a leg can be right or wrong
PLAT="$T/plat"; FXP="$PLAT/ci/faults/fx"; mkdir -p "$PLAT/app" "$FXP"
printf 'all good\n' > "$PLAT/app/data.txt"; write_gate "$PLAT"
GOODCMD='bash gate.sh'; MSG1='gate-fail: BROKEN found'
mkpatch "$FXP/good.patch"    app/data.txt 'all good' 'BROKEN'
mkpatch "$FXP/noop.patch"    app/data.txt 'all good' 'still fine'
mkpatch "$FXP/diffmsg.patch" app/data.txt 'all good' 'ODD'
mkpatch "$FXP/noapply.patch" app/data.txt 'a line that is not there' 'BROKEN'
mkexpect good      platform '' "$GOODCMD" "$MSG1"
mkexpect noop      platform '' "$GOODCMD" "$MSG1"
mkexpect diffmsg  platform '' "$GOODCMD" "$MSG1"
mkexpect noapply   platform '' "$GOODCMD" "$MSG1"
for g in twomsg twook unktoken unkkey nomsg badgate emptymsg nopatch crlf dbleg platm1; do cp "$FXP/good.patch" "$FXP/$g.patch"; done
rm -f "$FXP/nopatch.patch"
mkexpect twomsg    platform '' "$GOODCMD" "$MSG1" 'gate-fail: no such line'
mkexpect twook     platform '' "$GOODCMD" "$MSG1" 'gate-fail: second line'
mkexpect unktoken  platform 'frobnicate' "$GOODCMD" "$MSG1"
mkexpect unkkey    platform '' "$GOODCMD" "$MSG1";                                 printf 'FROBNICATE=1\n' >> "$FXP/unkkey.expect"
mkexpect nomsg     platform '' "$GOODCMD"
mkexpect badgate   platform '' "$GOODCMD" "$MSG1";                                 sed -i 's/^GATE=badgate$/GATE=other/' "$FXP/badgate.expect"
mkexpect emptymsg  platform '' "$GOODCMD" "$MSG1";                                 printf 'MESSAGE=\n' >> "$FXP/emptymsg.expect"
mkexpect nopatch   platform '' "$GOODCMD" "$MSG1"
mkexpect crlf      platform '' "$GOODCMD" "$MSG1";                                 sed -i 's/$/\r/' "$FXP/crlf.expect"
mkexpect dbleg     platform 'db' "$GOODCMD" "$MSG1"
mkexpect platm1    platform 'm1-chain' "$GOODCMD" "$MSG1"
# module legs: the patch changes a file of coldmail-verify (a path relative to that module's root)
mkpatch "$FXP/mod-ok.patch" lib/x.txt 'fine' 'BROKEN'
mkpatch "$FXP/mod-wr.patch" control-plane/migrations/0002_b.sql 'select 2;' 'BROKEN'
mkexpect mod-ok module:coldmail-verify 'm1-chain' 'bash modgate.sh' 'mod-gate: BROKEN lib'
mkexpect mod-wr module:coldmail-verify 'm1-chain' 'bash modgate.sh' 'mod-gate: BROKEN lib'
mkrepo "$PLAT" || { echo "selftest: cannot build the platform fixture"; exit 2; }
[ "$(tr -cd '\r' < "$FXP/crlf.expect" | wc -c)" -gt 0 ] || { echo "selftest: the CRLF fixture holds no CR bytes, so it would prove nothing"; exit 2; }

# the control-red fixture: the gate is already red on the unpatched tree
RED="$T/red"; FXP="$RED/ci/faults/fx"; mkdir -p "$RED/app" "$FXP"
printf 'BROKEN\n' > "$RED/app/data.txt"; write_gate "$RED"
mkpatch "$FXP/ctlred.patch" app/data.txt 'BROKEN' 'BROKEN BROKEN'
mkexpect ctlred platform '' "$GOODCMD" "$MSG1"
mkrepo "$RED" || { echo "selftest: cannot build the control-red fixture"; exit 2; }
FXP="$PLAT/ci/faults/fx"

echo "selftest: fixtures built in $T"
# ---- the cases --------------------------------------------------------------------------------------------------------------------------
run "$PLAT" good
also "no database is created for a leg without a db token" no_psql_call
check "good fault: control green, patch applies, gate red with the message" 0 \
  "=FAULT-PROOF PASS gate=good step=data check exit=1 messages=1" \
  "=gate: ok" "=gate: root=ctl dsn=<unset> modules=" "=gate: root=flt dsn=<unset> modules=" "=gate-fail: BROKEN found"

run "$PLAT" twook
check "two MESSAGE lines, both present, pass" 0 "=FAULT-PROOF PASS gate=twook step=data check exit=1 messages=2"

run "$PLAT" crlf
check "an .expect file with CRLF line endings is read with the CR stripped" 0 "=FAULT-PROOF PASS gate=crlf step=data check exit=1 messages=1"

run "$PLAT" noop
check "patch applies but the gate still passes (no red)" 1 \
  "=FAULT-PROOF FAIL gate=noop: (4) the gate command did not fail on the patched copy: it exited 0, so this gate does not catch the fault"

run "$PLAT" diffmsg
check "gate fails but with a different message" 1 \
  "=FAULT-PROOF FAIL gate=diffmsg: (4) the gate command exited 1 on the patched copy but its output is missing 1 of 1 expected message(s); first missing: 'gate-fail: BROKEN found'" \
  "=  message MISSING: gate-fail: BROKEN found"

run "$PLAT" twomsg
check "two MESSAGE lines, only one matches" 1 \
  "=FAULT-PROOF FAIL gate=twomsg: (4) the gate command exited 1 on the patched copy but its output is missing 1 of 2 expected message(s); first missing: 'gate-fail: no such line'" \
  "=  message found:   gate-fail: BROKEN found"

run "$RED" ctlred
also "the leg stops before the patch is applied" no_apply_step
check "control red: the gate fails on the unpatched copy" 1 \
  "=FAULT-PROOF FAIL gate=ctlred: (1) the paired control is red: the gate command exited 1 on the unpatched copy (it must exit 0)"

run "$PLAT" noapply
check "patch does not apply to a pristine copy" 1 \
  "=FAULT-PROOF FAIL gate=noapply: (2) the patch does not apply to a pristine copy of the platform"

run "$PLAT" mod-wr
check "a setup step that fails on the patched side only: wrong reason" 1 \
  "=FAULT-PROOF FAIL gate=mod-wr: (3) the fault broke the setup: setup step 'm1-chain' failed on the patched copy but passed on the unpatched copy, so a red from the gate would be for the wrong reason" \
  "~syntax error at or near BROKEN"

run "$PLAT" mod-ok
also "m1-chain applied the two migrations in order on each side and skipped notes.sql" chain_is "0001_a.sql 0002_b.sql 0001_a.sql 0002_b.sql "
check "module target: patch applied in the module copy, FAULT_ROOT is the module, FAULT_MODULES holds all three, m1-chain runs" 0 \
  "=FAULT-PROOF PASS gate=mod-ok step=data check exit=1 messages=1" \
  "~mod-gate: root=coldmail-verify modules=coldmail-send-byo coldmail-send-managed coldmail-verify"

run "$PLAT" platm1
also "m1-chain ran on both sides of a platform-target leg" chain_is "0001_a.sql 0002_b.sql 0001_a.sql 0002_b.sql "
check "platform target with m1-chain: FAULT_MODULES is provided" 0 \
  "=FAULT-PROOF PASS gate=platm1 step=data check exit=1 messages=1" \
  "?^gate: root=flt dsn=postgres://fpuser:fppass@dbhost:6543/fp_platm1_flt_[0-9a-f]{8}\\?sslmode=disable modules=set\$"

run "$PLAT" mod-ok FAULT_MODULES_DIR="$T/mods-nomig"
check "m1-chain finds zero migration files: the control cannot be set up" 1 \
  "=FAULT-PROOF FAIL gate=mod-ok: (1) the control could not be set up: setup step 'm1-chain' failed on the unpatched copy, so nothing can be concluded from the patched copy" \
  "~m1-chain: found 0 migration files"

run "$PLAT" mod-ok -u FAULT_MODULES_DIR -u DEPLOY_KEY_COLDMAIL_VERIFY -u DEPLOY_KEY_COLDMAIL_SEND_MANAGED -u DEPLOY_KEY_COLDMAIL_SEND_BYO
check "modules needed, no FAULT_MODULES_DIR and no deploy keys" 1 \
  "=FAULT-PROOF FAIL gate=mod-ok: the modules are needed but FAULT_MODULES_DIR is not set and these deploy keys are missing: DEPLOY_KEY_COLDMAIL_VERIFY DEPLOY_KEY_COLDMAIL_SEND_MANAGED DEPLOY_KEY_COLDMAIL_SEND_BYO (set FAULT_MODULES_DIR to a directory holding the three modules, or export the keys)"

run "$PLAT" mod-ok -u FAULT_MODULES_DIR -u DEPLOY_KEY_COLDMAIL_SEND_BYO DEPLOY_KEY_COLDMAIL_VERIFY=x DEPLOY_KEY_COLDMAIL_SEND_MANAGED=x
check "modules needed, one deploy key missing: only that one is named" 1 \
  "=FAULT-PROOF FAIL gate=mod-ok: the modules are needed but FAULT_MODULES_DIR is not set and these deploy keys are missing: DEPLOY_KEY_COLDMAIL_SEND_BYO (set FAULT_MODULES_DIR to a directory holding the three modules, or export the keys)"

run "$PLAT" dbleg
created="$(grep -o 'CREATE DATABASE "[^"]*"' "$PSQL_LOG" 2>/dev/null | sed 's/CREATE DATABASE "\(.*\)"/\1/' | tr '\n' ' ')"
db_ctl="$(printf '%s\n' "$created" | tr ' ' '\n' | grep '^fp_dbleg_ctl_[0-9a-f]\{8\}$')"; db_flt="$(printf '%s\n' "$created" | tr ' ' '\n' | grep '^fp_dbleg_flt_[0-9a-f]\{8\}$')"
dropped_ok() { has_text "$PSQL_LOG" "DROP DATABASE IF EXISTS \"$1\" WITH (FORCE)"; }
n_created="$(printf '%s\n' "$created" | wc -w)"
also "two databases were created" test "$n_created" -eq 2
also "one is named fp_<gate>_ctl_<random> and one fp_<gate>_flt_<random>" test -n "$db_ctl" -a -n "$db_flt"
also "the control database was dropped" dropped_ok "$db_ctl"
also "the fault database was dropped" dropped_ok "$db_flt"
also "the create call used the admin DSN" has_text "$PSQL_LOG" "[postgres://fpuser:fppass@dbhost:6543/postgres?sslmode=disable]"
check "a db token: a fresh database per side, FAULT_DSN is the admin DSN with the database replaced" 0 \
  "=FAULT-PROOF PASS gate=dbleg step=data check exit=1 messages=1" \
  "?^gate: root=ctl dsn=postgres://fpuser:fppass@dbhost:6543/fp_dbleg_ctl_[0-9a-f]{8}\\?sslmode=disable modules=\$" \
  "?^gate: root=flt dsn=postgres://fpuser:fppass@dbhost:6543/fp_dbleg_flt_[0-9a-f]{8}\\?sslmode=disable modules=\$" \
  "?^##\\[step\\] dropped database fp_dbleg_ctl_[0-9a-f]{8}\$"

run "$PLAT" unktoken
also "an unknown token never reaches a database" no_psql_call
check "unknown SETUP token" 1 \
  "=FAULT-PROOF FAIL gate=unktoken: the .expect file is invalid: unknown SETUP token 'frobnicate' (known: platform-up m1-chain db modules)"

run "$PLAT" unkkey
check "unknown key in the .expect file" 1 \
  "=FAULT-PROOF FAIL gate=unkkey: the .expect file is invalid: line 7: unknown key 'FROBNICATE' (allowed: GATE TARGET SETUP STEP CMD MESSAGE)"

run "$PLAT" nomsg
check "no MESSAGE line" 1 \
  "=FAULT-PROOF FAIL gate=nomsg: the .expect file is invalid: no MESSAGE line: at least one MESSAGE is required (without one a red for ANY reason would pass)"

run "$PLAT" emptymsg
check "an empty MESSAGE line" 1 \
  "=FAULT-PROOF FAIL gate=emptymsg: the .expect file is invalid: an empty MESSAGE would match any output"

run "$PLAT" badgate
check "GATE is not the file basename" 1 \
  "=FAULT-PROOF FAIL gate=badgate: the .expect file is invalid: GATE is 'other' but the file name says 'badgate' (GATE must equal the file basename)"

run "$PLAT" nopatch
check "an .expect file without its .patch" 1 "=FAULT-PROOF FAIL gate=nopatch: no such file: ci/faults/fx/nopatch.patch"

run "$PLAT" absent
check "no .expect file for the leg" 1 "=FAULT-PROOF FAIL gate=absent: no such file: ci/faults/fx/absent.expect"

sh_run bash "$RUNLEG"
check "usage: no arguments" 2 "~usage: run-leg.sh <phase> <gate>"
sh_run bash "$RUNLEG" fx 'Bad Gate'
check "usage: a gate name that is not [a-z0-9_-]" 2 "~phase and gate must match"
sh_run bash "$RUNLEG" --bogus
check "usage: an unknown option" 2 "~run-leg: unknown option: --bogus"

sh_run bash "$RUNLEG" --check-expect "$PLAT/ci/faults/fx/good.expect"
check "--check-expect on a good file (the mode check-fault-patches.sh uses)" 0 "=ok TARGET=platform MESSAGES=1 SETUP="
sh_run bash "$RUNLEG" --check-expect "$PLAT/ci/faults/fx/unkkey.expect"
check "--check-expect on a bad file names the file, the line and the key" 1 "~unkkey.expect: line 7: unknown key 'FROBNICATE'"
sh_run bash "$RUNLEG" --check-expect "$PLAT/ci/faults/fx/mod-ok.expect"
check "--check-expect reports a module target and its setup" 0 "=ok TARGET=module:coldmail-verify MESSAGES=1 SETUP=m1-chain"

# the selftest's own control: a deliberately WRONG expectation must be reported as a mismatch (a harness that cannot disagree is not testing)
cases_before=$cases; ok_before=$ok; bad_before=$bad
run "$PLAT" good
check "CONTROL (expected to mismatch): a passing leg wrongly expected to fail" 1 "=FAULT-PROOF FAIL gate=good: (4) the gate command did not fail on the patched copy" >/dev/null 2>&1
if [ "$bad" -eq $((bad_before + 1)) ]; then
  bad=$bad_before; cases=$cases_before; ok=$ok_before
  cases=$((cases + 1)); ok=$((ok + 1)); echo "  ok [selftest control] a wrong expectation was detected as a mismatch"
else
  echo "  MISMATCH [selftest control]: a wrong expectation was NOT detected — this selftest cannot fail"; cases=$((cases_before + 1)); bad=$((bad_before + 1)); ok=$ok_before
fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — only $cases cases ran, the floor is $MIN_CASES (cases were deleted or skipped)"; exit 1; }
[ "$bad" -eq 0 ] && [ "$ok" -eq "$cases" ] || exit 1
