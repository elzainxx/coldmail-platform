#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/m2worker/stage.sh go RED, for the reason they name, and stay green on a good input. No Docker, no Go: the functions are sourced and run against stubs (a
# fake provision-worker, a fake `docker compose logs`, fake test logs). What it covers:
#   m2w_mint          writes exactly four lines to the env file (the operator key, its bcrypt hash with every `$` doubled, the worker's id and token); masks each credential BEFORE it is written to the file; refuses
#                     a tool that printed nothing and refuses two equal credentials
#   m2w_require_pass  green when every TestM2Worker_* test has a column-0 `--- PASS` line; red, naming the test, when one is missing, FAILED or only mentioned
#   m2w_secret_leak   green on clean logs; red, naming the SERVICE and never printing the log or the credential, when a log holds the operator key, the worker token, the service token or the sink password
#   m2w_outage*       (B12) the outage sub-stage under stand-ins for docker compose and docker: m2w_outage_stop (container exited AND a request over the private network refused; a stop that fails, one that does not stop, one
#                     that still answers, a probe that did not run, a worker taken down), m2w_outage_platform_ready, m2w_outage_start (never healthy, never ready over the private network: the module's logs are printed),
#                     m2w_outage_worker (still running / exited and started again / never heartbeats), m2w_outage_require_phase (a PASS line AND the live line, from the phase's own log: a control-only PASS, a FAILED, an
#                     indented, another phase's), the whole m2w_outage in order and stopping at the first red phase, m2w_evidence on the outage files, and the wiring of m2w_scope_run (-test.skip, m2w_outage, the PASS lines)
# NOT covered here (they need a Linux host with Docker): m2w_preflight (file owner and mode, certificate names), m2w_stage_5c, m2w_scope_run itself (only its text is read), the real docker compose and wget.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=63; cases=0; ok=0; bad=0
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

# ── the outage sub-stage (B12): m2w_outage and its parts, under stand-ins for docker compose and docker. Every scenario is a set of SC_* variables; DC and docker answer from them. ──
cat > "$T/outage-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
M2_WORKER_ID="11111111-2222-3333-4444-555555555555"
DC() {
  local last="${!#}" n nm
  case "$1" in
    stop) [ "${SC_STOP_FAIL:-0}" = 1 ] && return 1; return 0;;
    ps)
      case "$last" in
        m2) [ "${SC_M2_NOCID:-0}" = 1 ] || echo cid-m2;;
        m2-worker) echo cid-worker;;
        postgres) echo cid-pg;;
        smtp-ctl) echo cid-sink;;
        platform) [ -n "${SC_PLATFORM:-}" ] && echo cid-platform;;
      esac
      return 0;;
    run)
      case "$*" in
        *"--entrypoint wget"*)
          n="$(cat "$T/sc.probe" 2>/dev/null || echo 0)"; n=$((n+1)); echo "$n" > "$T/sc.probe"
          case "${SC_PROBE:-refused}" in
            refused) echo "wget: can't connect to remote host (172.19.0.5): Connection refused"; return 1;;
            nxdomain) echo "wget: bad address 'm2'"; return 1;;
            ok) return 0;;
            http503) echo "wget: server returned error: HTTP/1.1 503 Service Unavailable"; return 1;;
            noprobe) echo "docker: unknown flag: --entrypoint"; return 125;;
            after3) if [ "$n" -le 3 ]; then echo "wget: can't connect to remote host (172.19.0.5): Connection refused"; return 1; fi; return 0;;
          esac;;
        *"HARNESS_M2_OUTAGE=1"*)
          nm="$(printf '%s' "$*" | sed -n 's/.*-test.run \^\([A-Za-z0-9_]*\)\$.*/\1/p')"
          [ -f "$SC_PHASES/$nm.log" ] || return 1
          cat "$SC_PHASES/$nm.log"; return "$(cat "$SC_PHASES/$nm.rc" 2>/dev/null || echo 0)";;
      esac;;
    up)
      case "$*" in
        *m2-worker*) : > "$T/sc.worker-started"; return 0;;
        *) [ "${SC_UP_FAIL:-0}" = 1 ] && return 1; return 0;;
      esac;;
    logs) echo "M2-LOG-MARKER the logs of $last"; return 0;;
    exec) echo "  HTTP/1.1 ${SC_PLATFORM:-200} OK"; return 0;;
  esac
  return 0
}
docker() {
  local fmt="$3" cid="$4"
  case "$cid:$fmt" in
    cid-m2:*) echo "${SC_M2_STATUS:-exited 0}";;
    cid-worker:*"State.ExitCode"*) echo 1;;
    cid-worker:*) if [ -f "$T/sc.worker-started" ]; then echo running; else echo "${SC_WORKER:-running}"; fi;;
    *) echo running;;
  esac
}
EOF
oc() { # run one function of stage.sh under the stand-ins; the caller sets SC_* with a prefix:  SC_X=1 oc function args
  ( set -u; WORK="$T/ow"; export WORK; rm -rf "$WORK"; rm -f "$T"/sc.*; mkdir -p "$WORK/out"; [ "${SC_NOBIN:-0}" = 1 ] || : > "$WORK/out/harness.test"
    export M2W_OUTAGE_SLEEP=0 M2W_OUTAGE_STOP_TRIES=3 M2W_OUTAGE_READY_TRIES="${M2W_OUTAGE_READY_TRIES:-3}" M2W_OUTAGE_HB_TRIES=3
    . "$T/env.sh"; . "$T/outage-env.sh"; . "$HERE/stage.sh"
    m2w_psql_su() {
      case "$1" in
        *clock_timestamp*) echo "2026-11-18 10:00:00+00";;
        *last_heartbeat_at*) case "${SC_HB:-yes}" in yes) echo 1;; no) echo 0;; afterstart) if [ -f "$T/sc.worker-started" ]; then echo 1; else echo 0; fi;; esac;;
      esac
    }
    "$@" ) 2>&1
}
out="$(oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: the container is exited and a request over the private network is refused" 0 "module 2 is gone: container m2 is 'exited 0' (status, exit code) and a request from a container on the private network gets: wget: can't connect" "$out" "$rc"
verdict "m2w_outage_stop: Postgres, the sink and the worker keep running" 0 "postgres, smtp-ctl and m2-worker are still running" "$out" "$rc"
out="$(SC_PROBE=nxdomain oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: an unresolvable name counts as gone too" 0 "gets: wget: bad address 'm2'" "$out" "$rc"
out="$(SC_STOP_FAIL=1 oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: a stop that fails" 1 "module 2 could not be stopped (docker compose stop m2 failed)" "$out" "$rc"
out="$(SC_M2_STATUS='running 0' oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: a stop that did not stop" 1 "module 2 did not stop: the container m2 is 'running 0'" "$out" "$rc"
out="$(SC_PROBE=ok oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: the container is exited but a request still succeeds" 1 "module 2 still answers: a request from a container on the private network" "$out" "$rc" "module 2 is gone"
out="$(SC_PROBE=http503 oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: something at the name m2 still answers with an HTTP error" 1 "module 2 still answers over the private network" "$out" "$rc" "module 2 is gone"
out="$(SC_PROBE=noprobe oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: a probe that did not run proves nothing" 1 "the probe from the private network did not run, so 'gone' was not proven" "$out" "$rc" "module 2 is gone"
out="$(SC_WORKER=exited oc m2w_outage_stop)"; rc=$?
verdict "m2w_outage_stop: the stop must not take the worker down" 1 "stopping module 2 took m2-worker down with it" "$out" "$rc"

out="$(oc m2w_outage_platform_ready)"; rc=$?
verdict "m2w_outage_platform_ready: no platform service in the scope" 0 "the platform service is not part of this run (scope m2worker)" "$out" "$rc"
out="$(SC_PLATFORM=200 oc m2w_outage_platform_ready)"; rc=$?
verdict "m2w_outage_platform_ready: a running platform answers 200" 0 "answers /readyz 200 while module 2 is stopped" "$out" "$rc"
out="$(SC_PLATFORM=503 oc m2w_outage_platform_ready)"; rc=$?
verdict "m2w_outage_platform_ready: a platform that is not ready while a module is down" 1 "the platform's /readyz answered '503' while module 2 is stopped, want 200" "$out" "$rc"

out="$(M2W_OUTAGE_READY_TRIES=6 SC_PROBE=after3 oc m2w_outage_start)"; rc=$?
verdict "m2w_outage_start: ready after three refused probes" 0 "module 2 is back: healthy by its own check, and a request from a container on the private network gets HTTP 200 on /readyz (after 3 failed probe(s))" "$out" "$rc"
out="$(SC_UP_FAIL=1 oc m2w_outage_start)"; rc=$?
verdict "m2w_outage_start: compose's own health wait fails" 1 "module 2 did not become healthy again after the start" "$out" "$rc"
verdict "m2w_outage_start: ... and the module's logs are printed before the stage dies" 1 "M2-LOG-MARKER the logs of m2" "$out" "$rc"
out="$(SC_PROBE=refused oc m2w_outage_start)"; rc=$?
verdict "m2w_outage_start: a start that never becomes ready over the private network" 1 "module 2 never became ready over the private network: 3 requests" "$out" "$rc" "module 2 is back"
verdict "m2w_outage_start: ... and the module's logs are printed before the stage dies (never ready)" 1 "M2-LOG-MARKER the logs of m2" "$out" "$rc"

out="$(oc m2w_outage_worker)"; rc=$?
verdict "m2w_outage_worker: a worker that never stopped has reconnected by itself" 0 "heartbeating again: it had never stopped and reconnected by itself" "$out" "$rc"
out="$(SC_WORKER=exited SC_HB=afterstart oc m2w_outage_worker)"; rc=$?
verdict "m2w_outage_worker: a worker that exited is started again by the stage" 0 "it had exited ('exited', exit code 1) after its reconnect budget ran out, as module 2's worker is designed to, and this stage started it again" "$out" "$rc"
out="$(SC_HB=no oc m2w_outage_worker)"; rc=$?
verdict "m2w_outage_worker: a worker that never heartbeats" 1 "module 2's worker never heartbeated after 2026-11-18 10:00:00+00 following the module's return" "$out" "$rc" "heartbeating again"
verdict "m2w_outage_worker: ... and the worker's logs are printed before the stage dies" 1 "M2-LOG-MARKER the logs of m2-worker" "$out" "$rc"

# a phase counts only on its own log: the column-0 PASS line AND the line only a live run writes
ph="$T/ph"; mkdir -p "$ph"
req() { # <test> <phase> <log file>
  ( set -u; WORK="$T"; . "$T/env.sh"; . "$T/outage-env.sh"; . "$HERE/stage.sh"; m2w_outage_require_phase "$1" "$2" "$3" && echo "REQUIRE-OK" ) 2>&1
}
printf '    m2_worker_outage_test.go:200: NOTE outage phase1 live: a job is in flight\n--- PASS: TestM2Worker_OutagePhase1Start (31.20s)\n' > "$ph/good.log"
out="$(req TestM2Worker_OutagePhase1Start 1 "$ph/good.log")"; rc=$?
verdict "m2w_outage_require_phase: a PASS line and the live line" 0 "REQUIRE-OK" "$out" "$rc"
printf '    m2_worker_outage_test.go:51: NOTE outage phase1 control-only: HARNESS_M2_OUTAGE=1 is set only by the outage sub-stage\n--- PASS: TestM2Worker_OutagePhase1Start (0.00s)\n' > "$ph/controlonly.log"
out="$(req TestM2Worker_OutagePhase1Start 1 "$ph/controlonly.log")"; rc=$?
verdict "m2w_outage_require_phase: a control-only PASS is not the phase" 1 "wrote no 'NOTE outage phase1 live' line: it ran control-only" "$out" "$rc" "REQUIRE-OK"
printf '    m2_worker_outage_test.go:200: NOTE outage phase1 live: x\n--- FAIL: TestM2Worker_OutagePhase1Start (31.20s)\n' > "$ph/failed.log"
out="$(req TestM2Worker_OutagePhase1Start 1 "$ph/failed.log")"; rc=$?
verdict "m2w_outage_require_phase: a FAILED phase" 1 "TestM2Worker_OutagePhase1Start has no '--- PASS' line in its own log" "$out" "$rc" "REQUIRE-OK"
printf '    m2_worker_outage_test.go:200: NOTE outage phase1 live: x\n    --- PASS: TestM2Worker_OutagePhase1Start (31.20s)\n' > "$ph/indented.log"
out="$(req TestM2Worker_OutagePhase1Start 1 "$ph/indented.log")"; rc=$?
verdict "m2w_outage_require_phase: an indented PASS line (a mention) is not a PASS" 1 "has no '--- PASS' line in its own log" "$out" "$rc" "REQUIRE-OK"
out="$(req TestM2Worker_OutagePhase2Down 2 "$ph/good.log")"; rc=$?
verdict "m2w_outage_require_phase: the log of another phase is not this phase's" 1 "TestM2Worker_OutagePhase2Down has no '--- PASS' line in its own log" "$out" "$rc" "REQUIRE-OK"
printf '    m2_worker_outage_test.go:200: NOTE outage phase1 live: x\n--- PASS: TestM2Worker_OutagePhase2Down (3.00s)\n' > "$ph/wronglive.log"
out="$(req TestM2Worker_OutagePhase2Down 2 "$ph/wronglive.log")"; rc=$?
verdict "m2w_outage_require_phase: another phase's live line does not count" 1 "wrote no 'NOTE outage phase2 live' line" "$out" "$rc" "REQUIRE-OK"
out="$(req TestM2Worker_OutagePhase1Start 1 "$ph/does-not-exist.log")"; rc=$?
verdict "m2w_outage_require_phase: no log at all" 1 "has no '--- PASS' line in its own log" "$out" "$rc" "REQUIRE-OK"

# the whole sub-stage, in order, and a flow that stops at the first red phase (the module is never started after a phase 2 that failed, never stopped after a phase 1 that did not run live)
sp="$T/phases"; mkdir -p "$sp"; rm -f "$sp"/*
printf '    m2_worker_outage_test.go:1: NOTE outage phase1 live: x\n--- PASS: TestM2Worker_OutagePhase1Start (31.20s)\n' > "$sp/TestM2Worker_OutagePhase1Start.log"
printf '    m2_worker_outage_test.go:1: NOTE outage phase2 live: x\n--- PASS: TestM2Worker_OutagePhase2Down (14.10s)\n' > "$sp/TestM2Worker_OutagePhase2Down.log"
printf '    m2_worker_outage_test.go:1: NOTE outage phase3 live: x\n--- PASS: TestM2Worker_OutagePhase3Resume (22.00s)\n' > "$sp/TestM2Worker_OutagePhase3Resume.log"
out="$(SC_PHASES="$sp" SC_PROBE=after3 M2W_OUTAGE_READY_TRIES=6 oc m2w_outage)"; rc=$?
verdict "m2w_outage: three live phases around a stop and a start" 0 "all three phases ran live and passed" "$out" "$rc"
ord="$(printf '%s\n' "$out" | grep -n -E 'phase 1: TestM2Worker|stopping module 2:|phase 2: TestM2Worker|starting module 2 again|phase 3: TestM2Worker' | cut -d: -f1 | tr '\n' ' ')"
sorted="$(printf '%s\n' $ord | sort -n | tr '\n' ' ')"
verdict "m2w_outage: phase 1, the stop, phase 2, the start, phase 3: in this order" 0 "in order" "$([ -n "$ord" ] && [ "$ord" = "$sorted" ] && [ "$(printf '%s\n' $ord | wc -l)" = 5 ] && echo "in order" || echo "out of order ($ord)")" 0
rm -f "$sp/TestM2Worker_OutagePhase2Down.log"
out="$(SC_PHASES="$sp" oc m2w_outage)"; rc=$?
verdict "m2w_outage: a phase 2 that left no log is red" 1 "phase 2 of module 2's outage proof" "$out" "$rc" "starting module 2 again"
printf '    m2_worker_outage_test.go:1: NOTE outage phase2 live: x\n--- PASS: TestM2Worker_OutagePhase2Down (14.10s)\n' > "$sp/TestM2Worker_OutagePhase2Down.log"; printf '1\n' > "$sp/TestM2Worker_OutagePhase2Down.rc"
out="$(SC_PHASES="$sp" oc m2w_outage)"; rc=$?
verdict "m2w_outage: a phase 2 that exits non-zero is red, and the module is not started" 1 "TestM2Worker_OutagePhase2Down failed (exit 1): phase 2 of module 2's outage proof" "$out" "$rc" "starting module 2 again"
rm -f "$sp/TestM2Worker_OutagePhase2Down.rc"
printf '    m2_worker_outage_test.go:1: NOTE outage phase1 control-only: x\n--- PASS: TestM2Worker_OutagePhase1Start (0.00s)\n' > "$sp/TestM2Worker_OutagePhase1Start.log"
out="$(SC_PHASES="$sp" oc m2w_outage)"; rc=$?
verdict "m2w_outage: a control-only phase 1 is red and the module is never stopped" 1 "wrote no 'NOTE outage phase1 live' line" "$out" "$rc" "stopping module 2:"
out="$(SC_NOBIN=1 SC_PHASES="$sp" oc m2w_outage)"; rc=$?
verdict "m2w_outage: no compiled binary" 1 "the compiled harness test binary is missing" "$out" "$rc"

# the evidence filter shows the outage tests' own lines (the test file, its helper file) and nothing else
cat > "$ph/evidence.log" <<'EOF'
    m2_worker_outage_test.go:120: NOTE outage phase1 live: a job is in flight
    m2_worker_outage_helpers_test.go:300: NOTE outage control phase 2 (one call against a stopped module): the assertion accepts a good input
    some_other_test.go:5: NOTE not mine
--- PASS: TestM2Worker_OutagePhase1Start (31.20s)
EOF
out="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; m2w_evidence "$ph/evidence.log" )"; rc=$?
verdict "m2w_evidence: the outage test's own lines are shown" 0 "m2_worker_outage_test.go:120: NOTE outage phase1 live" "$out" "$rc" "NOTE not mine"
verdict "m2w_evidence: ... so are its helper file's lines and the verdict line" 0 "m2_worker_outage_helpers_test.go:300: NOTE outage control phase 2" "$out" "$rc" "NOTE not mine"

# the contract edge-case test (B6c): its own lines are shown too, and it is one of the tests whose PASS line the stage requires
cat > "$ph/edge.log" <<'EOF'
    m2_worker_contract_edge_test.go:210: NOTE edge run conflict: Run of item B answered kind refused, HTTP 409, code conflict
    m2_worker_contract_edge_test.go:300: NOTE edge describe: GET /v1/describe answered 200
    some_other_test.go:5: NOTE not mine
--- PASS: TestM2Worker_ContractEdgeCases (21.40s)
EOF
out="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; m2w_evidence "$ph/edge.log" )"; rc=$?
verdict "m2w_evidence: the contract edge test's own lines are shown" 0 "m2_worker_contract_edge_test.go:210: NOTE edge run conflict" "$out" "$rc" "NOTE not mine"
verdict "m2w_evidence: ... and its verdict line" 0 "--- PASS: TestM2Worker_ContractEdgeCases" "$out" "$rc" "NOTE not mine"
printf '%s\n' "$all" | grep -v ContractEdgeCases > "$T/pass-no-edge.log"
out="$(pass_run "$T/pass-no-edge.log")"; rc=$?
verdict "m2w_require_pass: the contract edge test has no PASS line" 1 "TestM2Worker_ContractEdgeCases has no '--- PASS' line" "$out" "$rc"

# the lists and the wiring: three outage tests, all in the list the PASS lines and the floor are built from, and the scope run leaves them out of its ordinary run and runs the sub-stage
n3="$( . "$HERE/stage.sh"; k=0; for t in "${M2W_OUTAGE_TESTS[@]}"; do for u in "${M2W_TESTS[@]}"; do [ "$t" = "$u" ] && k=$((k+1)); done; done; echo "${#M2W_OUTAGE_TESTS[@]} outage tests, $k of them in M2W_TESTS" )"
verdict "M2W_OUTAGE_TESTS: three tests, every one in M2W_TESTS" 0 "3 outage tests, 3 of them in M2W_TESTS" "$n3" 0
wire="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; declare -f m2w_scope_run )"
verdict "m2w_scope_run: the ordinary run leaves the outage phases out" 0 '-test.skip "$skip"' "$wire" 0
verdict "m2w_scope_run: the sub-stage runs after the ordinary tests passed and its PASS lines join the run log" 0 "m2w_outage" "$wire" 0
verdict "m2w_scope_run: the outage PASS lines are appended to the log the floor reads" 0 "OutagePhase" "$wire" 0
# the names of the list are test functions of the harness package, and the scope's own -test.run pattern runs every one of them (a name the pattern misses would die in the scope after a build: "is not among the declared tests"; a name
# that is no function, the same). The three TestJobsLive_*M2* tests live in m2_worker_jobs_test.go and are not named TestM2Worker_*, so the pattern has to name them.
HARNESS_DIR="$(cd "$HERE/.." && pwd)"
declared_in() { # name... -> the names that are no `func Name(` of a harness test file
  local n out=""; for n in "$@"; do grep -qE "^func $n\(" "$HARNESS_DIR"/*_test.go || out="$out $n"; done; [ -z "$out" ] && echo all-declared || echo "NOT-DECLARED:$out"
}
listed_names="$( . "$HERE/stage.sh"; printf '%s\n' "${M2W_TESTS[@]}" )"
verdict "consistency: every name in M2W_TESTS is a test function of the harness package" 0 "all-declared" "$(declared_in $listed_names)" 0
verdict "consistency: ... and the check can fail (a name that is no test function is reported by name)" 0 "NOT-DECLARED: TestM2Worker_NoSuchTest" "$(declared_in $listed_names TestM2Worker_NoSuchTest)" 0
re="$(printf '%s\n' "$wire" | sed -n "s/.*local re='\([^']*\)'.*/\1/p" | head -1)"
unmatched=""; for n in $listed_names; do printf '%s\n' "$n" | grep -qE "$re" || unmatched="$unmatched $n"; done
verdict "consistency: the scope's -test.run pattern matches every name in M2W_TESTS" 0 "all-matched" "$([ -n "$re" ] && [ -z "$unmatched" ] && echo all-matched || echo "NOT-MATCHED:$unmatched (pattern '$re')")" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
