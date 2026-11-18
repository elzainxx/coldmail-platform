#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/m3adapter/outage.sh go RED, for the reason they name, and stay green on a good input. No Docker, no Go: the functions are sourced and run against stubs
# (a fake `docker compose`, a fake probe, a fake psql, fake test logs); the floor check runs the REAL scripts/check-test-floors.sh on fake logs. What it covers:
#   m3a_require_phase  green when the log has the column-0 `--- PASS` line AND the `NOTE m3a outage phaseN live:` line; red, naming the phase, when the PASS line is missing, FAILED or only indented (a subtest or a
#                      mention), and red as "control-only" when the live line is missing or belongs to another phase (the tests ran without HARNESS_M3_OUTAGE=1: a PASS proves nothing about the outage)
#   m3a_stop           green when the container is stopped and its health endpoint no longer answers while the control probe of another service does; red for a stop that failed, a container that is gone from
#                      `ps`, one that is still running, a probe that cannot reach the control service either (so a refusal would prove nothing), and a module that still answers after the stop
#   m3a_start          green when the Control becomes healthy and answers /readyz with status ready; red for a start that never became healthy and for a /readyz that never says ready (a `not_ready` body is not ready)
#   m3a_workers        green when each of the three workers heartbeated after the start; red, naming the worker, when one never did; red when the workers did not start
#   m3a_floor          the three phases' live PASS lines counted by the real floor gate: green with all three; red for a missing phase log, a phase log without its PASS line (the gate's own `declared != ran`),
#                      a binary that declares a fourth outage test or lacks one of the three
#   structure          the list of tests in outage.sh is the list of the Go test functions of m3_adapter_e2e_outage_test.go (and a renamed function is caught); run.sh sources outage.sh and calls m3a_outage
#                      after stage 9b and before stage 9c; the selftest is run by the `gates` workflow
# NOT covered here (they need a Linux host with Docker): m3a_phase (a docker compose run), m3a_outage as a whole.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=30; cases=0; ok=0; bad=0
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
banner() { echo "BANNER: $*"; }
sleep() { :; }
EOF

# ── m3a_require_phase ──
N1=TestM3Adapter_OutagePhase1Start; N2=TestM3Adapter_OutagePhase2Down; N3=TestM3Adapter_OutagePhase3Resume
phase_run() { # log number name
  ( set -u; WORK="$T"; . "$T/env.sh"; . "$HERE/outage.sh"; m3a_require_phase "$1" "$2" "$3" && echo accepted ) 2>&1
}
live="    m3_adapter_e2e_outage_test.go:512: NOTE m3a outage phase2 live: the platform's books are unchanged"
printf '=== RUN   %s\n%s\n--- PASS: %s (3.21s)\nPASS\n' "$N2" "$live" "$N2" > "$T/phase-good.log"
out="$(phase_run "$T/phase-good.log" 2 "$N2")"; rc=$?
verdict "m3a_require_phase: a live PASS" 0 "accepted" "$out" "$rc"
printf '=== RUN   %s\n%s\nPASS\n' "$N2" "$live" > "$T/phase-nopass.log"
out="$(phase_run "$T/phase-nopass.log" 2 "$N2")"; rc=$?
verdict "m3a_require_phase: no PASS line" 1 "outage phase 2 ($N2) has no '--- PASS' line" "$out" "$rc"
printf '=== RUN   %s\n%s\n--- FAIL: %s (3.21s)\nFAIL\n' "$N2" "$live" "$N2" > "$T/phase-fail.log"
out="$(phase_run "$T/phase-fail.log" 2 "$N2")"; rc=$?
verdict "m3a_require_phase: a FAILED test is not a PASS" 1 "has no '--- PASS' line" "$out" "$rc"
printf '=== RUN   %s\n%s\n    --- PASS: %s (3.21s)\nPASS\n' "$N2" "$live" "$N2" > "$T/phase-indented.log"
out="$(phase_run "$T/phase-indented.log" 2 "$N2")"; rc=$?
verdict "m3a_require_phase: an indented line (a subtest, a mention) is not a top-level PASS" 1 "has no '--- PASS' line" "$out" "$rc"
printf '=== RUN   %s\n    m3_adapter_e2e_outage_test.go:90: NOTE m3a outage phase2 control-only: HARNESS_M3_OUTAGE=1 is set only by the outage sub-stage\n--- PASS: %s (0.60s)\nPASS\n' "$N2" "$N2" > "$T/phase-control.log"
out="$(phase_run "$T/phase-control.log" 2 "$N2")"; rc=$?
verdict "m3a_require_phase: a control-only PASS stands in for nothing" 1 "it ran as control-only (HARNESS_M3_OUTAGE=1 did not reach the test container)" "$out" "$rc"
out="$(phase_run "$T/phase-good.log" 3 "$N2")"; rc=$?
verdict "m3a_require_phase: the live line of ANOTHER phase is not this phase's" 1 "no 'NOTE m3a outage phase3 live:' line" "$out" "$rc"
out="$(phase_run "$T/phase-good.log" 1 "$N1")"; rc=$?
verdict "m3a_require_phase: the PASS line of another test is not this test's" 1 "outage phase 1 ($N1) has no '--- PASS' line" "$out" "$rc"

# ── m3a_evidence ──
printf '%s\n' "    m3_adapter_e2e_test.go:77: NOTE m3a quote: 12 recipients" "    helpers_test.go:5: NOTE someone else" "    m3_adapter_e2e_helpers_test.go:9: NOTE m3a dial x" "--- PASS: x" > "$T/ev.log"
out="$( set -u; . "$T/env.sh"; . "$HERE/outage.sh"; m3a_evidence "$T/ev.log" 2>&1 )"; rc=$?
verdict "m3a_evidence: prints the module 3 adapter's NOTE lines" 0 "NOTE m3a quote: 12 recipients" "$out" "$rc" "someone else"

# ── m3a_stop ──
stop_run() { # mode
  ( set -u; . "$T/env.sh"; STUB_MODE="$1"; . "$HERE/outage.sh"
    DC() { case "$1" in stop) [ "$STUB_MODE" = stopfail ] && return 1; return 0;; ps) [ "$STUB_MODE" = noid ] && return 0; echo abc123;; esac; }
    docker() { if [ "$STUB_MODE" = running ]; then echo true; else echo false; fi; }
    m3a_probe() { case "$1" in
      *smtp-ctl*) [ "$STUB_MODE" = probe-broken ] && return 1; echo ok; return 0;;
      *m3:*) [ "$STUB_MODE" = still-answers ] && { echo ok; return 0; }; return 1;;
    esac; }
    m3a_stop ) 2>&1
}
out="$(stop_run ok)"; rc=$?
verdict "m3a_stop: a stopped module" 0 "m3 is stopped: its container is not running, its health endpoint no longer answers" "$out" "$rc"
out="$(stop_run stopfail)"; rc=$?
verdict "m3a_stop: docker compose could not stop it" 1 "could not stop m3" "$out" "$rc"
out="$(stop_run noid)"; rc=$?
verdict "m3a_stop: no container left to inspect" 1 "m3 has no container after the stop" "$out" "$rc"
out="$(stop_run running)"; rc=$?
verdict "m3a_stop: still running after the stop" 1 "m3 is still running after docker compose stop" "$out" "$rc"
out="$(stop_run probe-broken)"; rc=$?
verdict "m3a_stop: a probe that cannot reach the control service proves nothing" 1 "the probe itself does not work" "$out" "$rc"
out="$(stop_run still-answers)"; rc=$?
verdict "m3a_stop: the module still answers on the private network" 1 "health endpoint still answers" "$out" "$rc"

# ── m3a_start ──
start_run() { # mode
  ( set -u; . "$T/env.sh"; STUB_MODE="$1"; . "$HERE/outage.sh"; M3A_READY_SECS=3; : > "$T/probe.count"
    DC() { case "$1" in up) [ "$STUB_MODE" = up-fail ] && return 1; return 0;; logs) echo "m3 log tail";; esac; }
    m3a_probe() { echo x >> "$T/probe.count"; local n; n="$(wc -l < "$T/probe.count" | tr -d ' ')"
      case "$STUB_MODE" in
        ready-late) [ "$n" -ge 2 ] && { echo '{"status":"ready","checks":{"db":true}}'; return 0; }; return 1;;
        not-ready) echo '{"status":"not_ready","checks":{"db":false}}'; return 0;;
        *) return 1;;
      esac; }
    m3a_start ) 2>&1
}
out="$(start_run ready-late)"; rc=$?
verdict "m3a_start: the Control becomes ready" 0 "m3 is back: /readyz answers ready over the private network" "$out" "$rc"
out="$(start_run up-fail)"; rc=$?
verdict "m3a_start: the Control never became healthy" 1 "m3 did not become healthy again after the restart" "$out" "$rc"
out="$(start_run never)"; rc=$?
verdict "m3a_start: /readyz never answers" 1 "m3 did not answer /readyz with status ready within 3 s" "$out" "$rc"
out="$(start_run not-ready)"; rc=$?
verdict "m3a_start: a not_ready body is not ready" 1 "m3 did not answer /readyz with status ready within 3 s" "$out" "$rc"

# ── m3a_workers ──
workers_run() { # mode
  ( set -u; . "$T/env.sh"; STUB_MODE="$1"; . "$HERE/outage.sh"; M3A_BEAT_TRIES=2; M3_WORKER_ID=w1; M3_WORKER2_ID=w2; M3_WORKER3_ID=w3
    psql_su() { case "$1" in
      *clock_timestamp*) echo "2026-01-01 00:00:00+00";;
      *"id = 'w3'"*) [ "$STUB_MODE" = w3-silent ] && echo 0 || echo 1;;
      *) echo 1;;
    esac; }
    DC() { [ "$STUB_MODE" = up-fail ] && return 1; return 0; }
    m3a_workers ) 2>&1
}
out="$(workers_run ok)"; rc=$?
verdict "m3a_workers: three heartbeating workers" 0 "module 3's three workers are running and each heartbeated after" "$out" "$rc"
out="$(workers_run w3-silent)"; rc=$?
verdict "m3a_workers: one worker never heartbeats, and the sentence names it" 1 "module 3's worker w3 has no heartbeat after" "$out" "$rc" "w1 has no heartbeat"
out="$(workers_run up-fail)"; rc=$?
verdict "m3a_workers: the workers did not start" 1 "module 3's workers did not start" "$out" "$rc"

# ── m3a_floor (the REAL floor gate on fake logs) ──
floor_run() { # mode
  local w="$T/fl-$1"; mkdir -p "$w"; rm -f "$w"/*
  ( set -u; . "$T/env.sh"; WORK="$w"; PLATFORM_DIR="$ROOT"; GO_PKG="example.test/harness"; STUB_MODE="$1"; . "$HERE/outage.sh"
    for i in 1 2 3; do
      case "$1:$i" in
        missing-log:2) continue;;
        nopass:2) printf '=== RUN   x\nPASS\n' > "$w/m3a-outage-$i.log"; continue;;
      esac
      eval "name=\${M3A_OUTAGE_TESTS[$((i-1))]}"
      printf '=== RUN   %s\n--- PASS: %s (1.00s)\nPASS\n' "$name" "$name" > "$w/m3a-outage-$i.log"
    done
    DC() { case "$STUB_MODE" in
        four) printf '%s\n' "${M3A_OUTAGE_TESTS[@]}" TestM3Adapter_OutagePhase4Extra;;
        lacks) printf '%s\n' "${M3A_OUTAGE_TESTS[0]}" "${M3A_OUTAGE_TESTS[1]}";;
        *) printf '%s\n' "${M3A_OUTAGE_TESTS[@]}";;
      esac; }
    m3a_floor ) 2>&1
}
out="$(floor_run good)"; rc=$?
verdict "m3a_floor: three live phases against the real floor gate" 0 "floor: 3 TestM3Adapter_Outage tests declared, each run live and passed" "$out" "$rc"
out="$(floor_run missing-log)"; rc=$?
verdict "m3a_floor: a phase that left no log" 1 "the log of outage phase 2 is missing" "$out" "$rc"
out="$(floor_run nopass)"; rc=$?
verdict "m3a_floor: a phase log without its PASS line is the gate's declared != ran" 1 "RED: declared != ran" "$out" "$rc"
out="$(floor_run four)"; rc=$?
verdict "m3a_floor: a binary that declares a fourth outage test" 1 "the binary declares 4 TestM3Adapter_Outage tests, outage.sh names 3" "$out" "$rc"
out="$(floor_run lacks)"; rc=$?
verdict "m3a_floor: a binary that lacks one of the three" 1 "is not among the declared tests of the harness binary" "$out" "$rc"

# ── structure: the lists agree, the stage is called, the selftest is run ──
gofile="$ROOT/tests/harness/m3_adapter_e2e_outage_test.go"
inscript="$( . "$HERE/outage.sh"; printf '%s\n' "${M3A_OUTAGE_TESTS[@]}" | sort )"
ingo="$( grep -oE '^func (TestM3Adapter_Outage[A-Za-z0-9_]*)' "$gofile" | sed 's/^func //' | sort )"
verdict "structure: outage.sh names exactly the Go test functions of m3_adapter_e2e_outage_test.go" 0 "same" "$([ -n "$ingo" ] && [ "$inscript" = "$ingo" ] && echo same || printf 'script: %s | go: %s' "$inscript" "$ingo")" 0
sed 's/^func TestM3Adapter_OutagePhase2Down/func TestM3Adapter_OutagePhase2Renamed/' "$gofile" > "$T/renamed.go"
renamed="$( grep -oE '^func (TestM3Adapter_Outage[A-Za-z0-9_]*)' "$T/renamed.go" | sed 's/^func //' | sort )"
verdict "structure (control): a renamed Go test function is not the script's list" 0 "differ" "$([ "$inscript" != "$renamed" ] && echo differ || echo same)" 0
nest="$( awk '/^STAGE=m3-outage; banner "9b"/{a=NR} /m3a_outage[ \t]*(#.*)?$/ && !/^m3a_outage\(\)/ && !/^#/{c=NR} /^STAGE=leader; banner "9c"/{d=NR} END{ if (a && c && d && a < c && c < d) print "between"; else print "a=" a " call=" c " 9c=" d }' "$ROOT/tests/harness/run.sh" )"
verdict "structure: run.sh calls m3a_outage between stage 9b and stage 9c" 0 "between" "$nest" 0
verdict "structure: run.sh sources outage.sh" 0 "sourced" "$(grep -q '^\. "\$HERE/m3adapter/outage.sh"' "$ROOT/tests/harness/run.sh" && echo sourced || echo missing)" 0
verdict "structure: the gates workflow runs this selftest" 0 "wired" "$(grep -q 'tests/harness/m3adapter/stage-selftest.sh' "$ROOT/.github/workflows/gates.yml" && echo wired || echo missing)" 0
verdict "structure: the tests are guarded by HARNESS_M3_OUTAGE and the stage sets it" 0 "guarded" "$(grep -q 'HARNESS_M3_OUTAGE' "$gofile" && grep -q 'HARNESS_M3_OUTAGE=1' "$HERE/outage.sh" && echo guarded || echo missing)" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
