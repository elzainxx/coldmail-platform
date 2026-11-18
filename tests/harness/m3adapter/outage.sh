#!/usr/bin/env bash
# outage.sh — the OUTAGE proof of module 3's adapter (Phase 3, chunk B12). SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself) and called once, by m3a_outage, right after
# stage 9b. It uses what run.sh defines: DC, die, banner, STAGE, WORK, GO_PKG, PLATFORM_DIR, psql_su (stage 5b) and the three worker ids M3_WORKER_ID, M3_WORKER2_ID, M3_WORKER3_ID (stage 2).
#
# THE SHAPE (the tests are tests/harness/m3_adapter_e2e_outage_test.go; the stack is the ordinary full harness, module 3 included):
#   phase 1   TestM3Adapter_OutagePhase1Start   a tenant, an entitlement, a verified account and a campaign of 24 whose every send the controllable sink holds 6 s: still running, proved by the adapter and by the sink
#   (stage)   stop m3                           the container that serves module 3's Domain A API (the Control); its three workers, Postgres and the sinks keep running; prove it is gone
#   phase 2   TestM3Adapter_OutagePhase2Down    every read and write fails typed and returns nothing, polling halts, the platform's books and module 3's tables are what they were
#   (stage)   start m3, then the workers        wait for /readyz over the private network; start every worker that left (a worker with nothing buffered exits after ~30 s without a Control: restart "no")
#   phase 3   TestM3Adapter_OutagePhase3Resume  the campaign completes exactly once: one job, one message, one send-log row per item; nothing charged; a repeat Run is the module's replay
#
# EACH PHASE is a `go test` of its own in a fresh test container, run by name with HARNESS_M3_OUTAGE=1 (the only place it is set). Without it the three tests run only their paired controls, so the ordinary run of stage 6,
# which lists and runs every test of the binary, counts them toward tests/floors-harness.txt harmlessly; THIS sub-stage requires, in each invocation's log, the `--- PASS` line AND the `NOTE m3a outage phaseN live:` line only a
# live run writes (m3a_require_phase), then counts the three live PASS lines against a floor of their own (m3a_floor). There is no skip path: a missing line stops the run with a sentence.
#
# Nothing here prints a credential: no `set -x`, no environment dump, and a failing check names a file and a number, never a value.

M3A_OUTAGE_TESTS=(
  TestM3Adapter_OutagePhase1Start
  TestM3Adapter_OutagePhase2Down
  TestM3Adapter_OutagePhase3Resume
)
M3A_READY_SECS=120     # how long m3a_start waits for the restarted Control to answer /readyz over the private network
M3A_BEAT_TRIES=45      # how many 2 s waits a worker gets to heartbeat after it was started

# the test's own evidence lines (the NOTE lines of the three test files), shortened and indented: `m3a_evidence <log> [max lines]`
m3a_evidence() {
  grep -E " m3_adapter_e2e(_[a-z0-9]+)*_test.go:[0-9]+: " "$1" | cut -c1-900 | sed 's/^/  | /' | head -"${2:-400}"
}

# the judge of one phase's log: the PASS line of the test AND the line only a live run writes
m3a_require_phase() { # log number name
  local log="$1" n="$2" name="$3"
  grep -q "^--- PASS: $name " "$log" || die "outage phase $n ($name) has no '--- PASS' line in its log: it did not run, or it did not pass"
  grep -q "NOTE m3a outage phase$n live:" "$log" || die "outage phase $n ($name) has a '--- PASS' line but no 'NOTE m3a outage phase$n live:' line: it ran as control-only (HARNESS_M3_OUTAGE=1 did not reach the test container), so it proved nothing about the outage"
}

# one phase: a go test of its own in a fresh test container, by name, with the variable that only this sub-stage sets
m3a_phase() { # number name
  local n="$1" name="$2" rc log="$WORK/m3a-outage-$1.log"
  DC run --rm -T -e HARNESS_M3_OUTAGE=1 harness-tests -test.run "^${name}\$" -test.v -test.count=1 -test.timeout=10m > "$log" 2>&1; rc=$?
  m3a_evidence "$log" 200
  grep -E "^(--- (PASS|FAIL|SKIP)|PASS|FAIL|ok )" "$log" | sed 's/^/  | /'
  if [ "$rc" != 0 ]; then
    echo "  ---- the failing phase (with context) ----"
    grep -n -B1 -A30 -e '--- FAIL' "$log" | cut -c1-700 | sed 's/^/  | /' | head -200
    die "outage phase $n ($name) failed (exit $rc): its log is above"
  fi
  m3a_require_phase "$log" "$n" "$name"
  echo "  outage phase $n ($name): PASS (live)"
}

# a GET from inside the stack (the platform container, busybox wget): succeeds, printing the body, when the URL answers with a 2xx
m3a_probe() { DC exec -T platform wget -q -T 3 -O - "$1" 2>/dev/null; }

# stop the Control and prove it is gone: its container is not running AND its health endpoint no longer answers on the private network, with a control that the same probe does reach another service
m3a_stop() {
  local id st
  DC stop m3 >/dev/null 2>&1 || die "docker compose could not stop m3: the outage cannot start"
  id="$(DC ps -aq m3)"
  [ -n "$id" ] || die "m3 has no container after the stop: the stack is not what the outage sub-stage expects"
  st="$(docker inspect -f '{{.State.Running}}' "$id")"
  [ "$st" = false ] || die "m3 is still running after docker compose stop (State.Running=$st): an outage proof against a running module would prove nothing"
  m3a_probe http://smtp-ctl:8025/readyz >/dev/null || die "the probe from the platform container cannot reach the controllable sink (smtp-ctl) either: a refused connection to m3 would prove nothing, the probe itself does not work"
  if m3a_probe http://m3:8080/healthz >/dev/null; then die "m3's health endpoint still answers on the private network after the stop: the module is not gone"; fi
  echo "  m3 is stopped: its container is not running, its health endpoint no longer answers on the private network, and the same probe does reach the controllable sink (the control)"
}

# start the Control again and wait for its readiness over the private network (bounded; the run dies with the Control's log when it never comes back)
m3a_start() {
  local i body=""
  DC up -d --wait --wait-timeout 240 m3 >/dev/null 2>&1 || { DC logs --no-color --tail 80 m3 2>&1 | sed 's/^/  | /'; die "m3 did not become healthy again after the restart (its log tail is above)"; }
  i=0
  while [ "$i" -lt "$M3A_READY_SECS" ]; do
    body="$(m3a_probe http://m3:8080/readyz)" && printf '%s' "$body" | grep -Eq '"status": *"ready"' && break
    body=""; i=$((i+1)); sleep 1
  done
  [ -n "$body" ] || { DC logs --no-color --tail 80 m3 2>&1 | sed 's/^/  | /'; die "m3 did not answer /readyz with status ready within ${M3A_READY_SECS} s of the restart, over the private network (its log tail is above)"; }
  echo "  m3 is back: /readyz answers ready over the private network after $i s of waiting"
}

# every module 3 worker running again, each with a heartbeat newer than this moment (the Control's boot rescan refreshes every heartbeat, so only a heartbeat taken AFTER the start proves a live worker)
m3a_workers() {
  local t0 wid i
  t0="$(psql_su "select clock_timestamp()")"; [ -n "$t0" ] || die "could not read the database clock"
  DC up -d m3-worker m3-worker-2 m3-worker-3 >/dev/null 2>&1 || die "module 3's workers did not start"
  for wid in "$M3_WORKER_ID" "$M3_WORKER2_ID" "$M3_WORKER3_ID"; do
    i=0
    while [ "$i" -lt "$M3A_BEAT_TRIES" ]; do
      [ "$(psql_su "select count(*) from send_byo.boxes where id = '$wid' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
      i=$((i+1)); sleep 2
    done
    [ "$i" -lt "$M3A_BEAT_TRIES" ] || die "module 3's worker $wid has no heartbeat after $t0: it did not register with the Control (it registers over mTLS with its token; the logs of m3 and the worker are in the dump below)"
  done
  echo "  module 3's three workers are running and each heartbeated after $t0"
}

# the floor of the sub-stage: the three tests were declared by the binary and each has a live PASS line in a log this run produced (the full floor, tests/floors-harness.txt, counts the control-only runs of stage 6)
m3a_floor() {
  local i t n=${#M3A_OUTAGE_TESTS[@]} got
  DC run --rm -T harness-tests -test.list '^TestM3Adapter_Outage' > "$WORK/m3a-list.log" 2>&1 || { cat "$WORK/m3a-list.log"; die "listing the TestM3Adapter_Outage tests failed"; }
  for t in "${M3A_OUTAGE_TESTS[@]}"; do grep -qx "$t" "$WORK/m3a-list.log" || die "$t is not among the declared tests of the harness binary"; done
  got="$(grep -c '^TestM3Adapter_Outage' "$WORK/m3a-list.log")"
  [ "$got" = "$n" ] || die "the binary declares $got TestM3Adapter_Outage tests, outage.sh names $n (M3A_OUTAGE_TESTS): a test was added or removed without the list"
  : > "$WORK/m3a-run.log"
  for i in 1 2 3; do
    [ -s "$WORK/m3a-outage-$i.log" ] || die "the log of outage phase $i is missing: the outage sub-stage did not run it"
    cat "$WORK/m3a-outage-$i.log" >> "$WORK/m3a-run.log"
  done
  printf 'ok  \t%s\t0.000s\n' "$GO_PKG" >> "$WORK/m3a-run.log"
  printf 'ok  \t%s\t0.000s\n' "$GO_PKG" >> "$WORK/m3a-list.log"
  printf '# the three live outage phases of module 3 adapter\ngo|%s|%s\n' "$GO_PKG" "$n" > "$WORK/floors-m3a.txt"
  bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$WORK/floors-m3a.txt" --go-run-log "$WORK/m3a-run.log" --go-list-log "$WORK/m3a-list.log" || die "the module 3 outage test-count floor ($n)"
  echo "  floor: $n TestM3Adapter_Outage tests declared, each run live and passed"
}

# the whole sub-stage; run.sh calls it right after stage 9b (module 3 was stopped and started there, so the second and third workers may have left)
m3a_outage() {
  STAGE=m3-adapter-outage; banner "9b-adapter" "module 3's Domain A API is stopped in the middle of a campaign: the adapter's calls fail typed and nothing is settled or invented; started again, the campaign completes exactly once"
  local t0=$SECONDS
  echo "  1. module 3's three workers running, each with a fresh heartbeat (stage 9b left only the first one started)"
  m3a_workers
  echo "  2. phase 1: a tenant, an entitlement, a verified account and a campaign of 24 whose sends the sink holds 6 s each: proved in flight by the adapter and the sink"
  m3a_phase 1 "${M3A_OUTAGE_TESTS[0]}"
  echo "  3. stop m3 (its workers, Postgres and the sinks keep running) and prove it is gone"
  m3a_stop
  echo "  4. phase 2: the module is down: typed errors and no value from every call, polling halts, nothing settled or invented, nothing created"
  m3a_phase 2 "${M3A_OUTAGE_TESTS[1]}"
  echo "  5. start m3 again and wait for its readiness over the private network"
  m3a_start
  echo "  6. module 3's workers: start the ones that left, wait for a heartbeat newer than the restart"
  m3a_workers
  echo "  7. phase 3: the campaign completes exactly once, nothing charged, a repeat Run is the replay"
  m3a_phase 3 "${M3A_OUTAGE_TESTS[2]}"
  m3a_floor
  echo "  M3A OK in $((SECONDS - t0)) s"
}
