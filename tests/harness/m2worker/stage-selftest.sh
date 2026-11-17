#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/m2worker/stage.sh go RED, for the reason they name, and stay green on a good input. No Docker, no Go: the functions are sourced and run against stubs (a
# fake provision-worker, a fake `docker compose logs`, fake test logs). What it covers:
#   m2w_mint          writes exactly four lines to the env file (the operator key, its bcrypt hash with every `$` doubled, the worker's id and token); masks each credential BEFORE it is written to the file; refuses
#                     a tool that printed nothing and refuses two equal credentials
#   m2w_require_pass  green when every TestM2Worker_* test has a column-0 `--- PASS` line; red, naming the test, when one is missing, FAILED or only mentioned
#   m2w_secret_leak   green on clean logs; red, naming the SERVICE and never printing the log or the credential, when a log holds the operator key, the worker token, the service token or the sink password
# NOT covered here (they need a Linux host with Docker): m2w_preflight (file owner and mode, certificate names), m2w_stage_5c, m2w_scope_run.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=13; cases=0; ok=0; bad=0
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
MASKLOG="$WORK/mask.log"
mask() { if [ -f "$WORK/harness.env" ] && grep -qF -- "$1" "$WORK/harness.env"; then echo "AFTER_WRITE" >> "$MASKLOG"; else echo "BEFORE_WRITE" >> "$MASKLOG"; fi; }
field() { grep "^$1=" | head -1 | sed "s/^$1=//; s/ .*//"; }
esc() { printf '%s' "$1" | sed 's/\$/$$/g'; }
DC() { echo "DC called: $*" >&2; return 1; }
EOF

# ── m2w_mint ──
mint_run() { # mode: good | same | empty
  W="$T/w-$1"; mkdir -p "$W"; : > "$W/harness.env"
  ( set -u; WORK="$W"; export WORK; . "$T/env.sh"; M2_TOKEN="svc-token-1"
    case "$1" in
      good)  mint() { local n; n="$RANDOM$RANDOM"; printf '# operator admin key\nADMIN_API_KEY=key%sabcdef   # SHOWN ONCE\nADMIN_API_KEY_HASH=$2a$10$hash%sxyz   # configure\n' "$n" "$n"; };;
      same)  mint() { printf 'ADMIN_API_KEY=samekey   # x\nADMIN_API_KEY_HASH=$2a$10$samehash   # x\n'; };;
      empty) mint() { echo "# nothing"; };;
    esac
    cat() { if [ "${1:-}" = /proc/sys/kernel/random/uuid ]; then echo 11111111-2222-3333-4444-555555555555; else command cat "$@"; fi; }
    . "$HERE/stage.sh"; m2w_mint ) 2>&1
}
out="$(mint_run good)"; rc=$?
verdict "m2w_mint: a good mint" 0 "masked, none printed" "$out" "$rc"
env="$T/w-good/harness.env"
lines="$(grep -c '^H_M2_' "$env")"
verdict "m2w_mint: exactly four H_M2_ lines in the env file" 0 "four" "$([ "$lines" = 4 ] && echo four || echo "$lines")" 0
verdict "m2w_mint: the bcrypt hash is written with every dollar doubled" 0 "H_M2_ADMIN_KEY_HASH=\$\$2a\$\$10\$\$hash" "$(cat "$env")" 0
verdict "m2w_mint: every credential was masked BEFORE it was written to the env file" 0 "4 before, 0 after" "$(printf '%s before, %s after' "$(grep -c BEFORE_WRITE "$T/w-good/mask.log")" "$(grep -c AFTER_WRITE "$T/w-good/mask.log" || true)")" 0
case "$out" in *"key"*"abcdef"*) leaked=yes;; *) leaked=no;; esac
verdict "m2w_mint: the output never shows a credential" 0 "no" "$leaked" 0
out="$(mint_run same)"; rc=$?
verdict "m2w_mint: equal credentials are refused" 1 "must be three different credentials" "$out" "$rc"
out="$(mint_run empty)"; rc=$?
verdict "m2w_mint: a tool that printed nothing is refused" 1 "could not mint module 2's operator key" "$out" "$rc"

# ── m2w_require_pass ──
pass_run() { # log
  ( set -u; WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; m2w_require_pass "$1" ) 2>&1
}
# the PASS lines of EVERY test stage.sh lists (M2W_TESTS is the one list: the five tests of m2_worker_test.go and the four adapter tests of m2_worker_e2e_test.go), so that a test added to the stage cannot be forgotten here
all="$( . "$HERE/stage.sh"; for name in "${M2W_TESTS[@]}"; do printf -- '--- PASS: %s (1.00s)\n' "$name"; done )"
ntests="$( . "$HERE/stage.sh"; echo "${#M2W_TESTS[@]}" )"
[ "$ntests" -ge 9 ] || { echo "selftest: RED — stage.sh lists only $ntests test(s): the adapter tests (B9, B10, B11, B13) are not all in M2W_TESTS"; exit 1; }
printf '%s\n' "$all" > "$T/pass-all.log"
out="$(pass_run "$T/pass-all.log")"; rc=$?
verdict "m2w_require_pass: every listed test passed" 0 "$ntests TestM2Worker_* tests have a '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | grep -v AdapterEndToEndSettlesItemByItem > "$T/pass-no-e2e.log"
out="$(pass_run "$T/pass-no-e2e.log")"; rc=$?
verdict "m2w_require_pass: the adapter end-to-end test has no PASS line" 1 "TestM2Worker_AdapterEndToEndSettlesItemByItem has no '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | grep -v AdapterCancelSettlesFree > "$T/pass-no-adcancel.log"
out="$(pass_run "$T/pass-no-adcancel.log")"; rc=$?
verdict "m2w_require_pass: the adapter cancel test has no PASS line" 1 "TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve has no '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | grep -v CancelStopsSending > "$T/pass-missing.log"
out="$(pass_run "$T/pass-missing.log")"; rc=$?
verdict "m2w_require_pass: the cancel test has no PASS line" 1 "TestM2Worker_CancelStopsSending has no '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | sed 's/^--- PASS: TestM2Worker_ControlAllSent/--- FAIL: TestM2Worker_ControlAllSent/' > "$T/pass-failed.log"
out="$(pass_run "$T/pass-failed.log")"; rc=$?
verdict "m2w_require_pass: a FAILED test is not a PASS" 1 "TestM2Worker_ControlAllSent has no '--- PASS' line" "$out" "$rc"
printf '%s\n' "$all" | sed 's/^--- PASS: TestM2Worker_Preconditions/    --- PASS: TestM2Worker_Preconditions/' > "$T/pass-indented.log"
out="$(pass_run "$T/pass-indented.log")"; rc=$?
verdict "m2w_require_pass: an indented line (a subtest, or a mention) is not a top-level PASS" 1 "TestM2Worker_Preconditions has no '--- PASS' line" "$out" "$rc"

# ── m2w_secret_leak ──
leak_run() { # the text of the log every service answers with
  mkdir -p "$T/lk"; printf '%s\n' "$1" > "$T/lk/log.txt"
  ( set -u; WORK="$T/lk"; export WORK; . "$T/env.sh"
    DC() { if [ "$1" = logs ]; then cat "$T/lk/log.txt"; else return 1; fi; }
    M2_ADMIN_KEY="opkeyopkeyopkeyopkey"; M2_WORKER_TOKEN="wtokenwtokenwtokenwt"; M2_TOKEN="svctokensvctokensvct"; SINK_PASS="sinkpasssinkpasssink"
    . "$HERE/stage.sh"; m2w_secret_leak ) 2>&1
}
out="$(leak_run 'worker registered id=abc; send accepted; nothing secret here')"; rc=$?
verdict "m2w_secret_leak: clean logs" 0 "appears in the logs of m2, m2-worker or smtp-ctl" "$out" "$rc"
out="$(leak_run 'auth ok for wtokenwtokenwtokenwt from worker')"; rc=$?
verdict "m2w_secret_leak: the worker token in a log is red, names the service, shows nothing" 1 "appears in the log of m2 (the log is not shown" "$out" "$rc" "wtokenwtokenwtokenwt"
out="$(leak_run 'password sinkpasssinkpasssink was sent')"; rc=$?
verdict "m2w_secret_leak: the sink password in a log is red" 1 "appears in the log of m2 " "$out" "$rc" "sinkpasssinkpasssink"

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
