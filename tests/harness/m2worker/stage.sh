#!/usr/bin/env bash
# stage.sh — module 2's REAL WORKER in the platform harness (Phase 3, chunk B8). SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself) and it uses what run.sh defines:
# DC, die, banner, mask, mint, field, esc, STAGE, WORK, HERE, PLATFORM_DIR, GO_PKG, PROJECT_NET, M2 (the module-2 checkout), M2_TOKEN, SINK_PASS, TEST_TAGS.
#
# DORMANT BY DEFAULT. run.sh calls these functions only when M2W_ON=1: under H_SCOPE=m2worker, or in the full run once `m2worker` is named in FULL_INCLUDES (run.sh, one place). With M2W_ON=0 nothing here runs, the
# compose override docker-compose.m2worker.yml is not merged, and the tests (build tag m2worker) are not compiled: the full harness is what it was before this stage existed.
#
# What is here, in the order run.sh calls it:
#   m2w_mint            stage 2: the operator key, the worker's id and token (+ bcrypt hashes) minted with module 2's OWN provision-worker, masked before anything uses them, written mode 600 to the env file; never printed
#   m2w_stage_5c        stage 5c: the key files' owner and mode (per container uid), the certificates' names, the send_managed.boxes row BEFORE the worker starts, the worker, a heartbeat newer than the insert, the sink
#   m2w_evidence        stage 6: prints the test's own log lines (the sink's per-recipient counts, the timings, the report snapshots): the first-run review reads them
#   m2w_require_pass    stage 6: every TestM2Worker_* test has a `--- PASS` line in the log the run itself produced (a binary built without one, or a -run that skipped it, would leave the rest green)
#   m2w_secret_leak     after the tests: none of this run's credentials appears in the logs of m2, m2-worker or smtp-ctl (a workflow log is public)
#   m2w_outage          the OUTAGE sub-stage (Phase 3, chunk B12): module 2's platform API container is stopped in the middle of a campaign (phase 1 -> stop -> phase 2 -> start -> phase 3, three `go test` invocations of the
#                       same binary), and nothing is invented while it is down or lost or done twice when it is back. m2w_scope_run calls it after the ordinary tests; the FULL run calls it only where run.sh does (one line
#                       after m2w_require_pass: `[ "$M2W_ON" = 1 ] && m2w_outage`, which the lead adds)
#   m2w_scope_run       H_SCOPE=m2worker: Postgres + migrations + module 2 + its worker + the sink + only the TestM2Worker tests (the outage phases run in m2w_outage), then the exposure check; ends the run
# Nothing here prints a credential: no `set -x`, no environment dump, and a failing check names a file and a number, never a value.

M2W_SYSTEM_USER="00000000-0000-0000-0000-000000000000"      # the reserved user module 2 requires in public.users (platform_0001 inserts it); send_managed.boxes.user_id references it
M2W_TESTS=(
  TestM2Worker_Preconditions
  TestM2Worker_ControlAllSent
  TestM2Worker_SentBouncedAndAttemptsExhausted
  TestM2Worker_ControlHeldCampaignAllSent
  TestM2Worker_CancelStopsSending
  TestM2Worker_AdapterEndToEndSettlesItemByItem
  TestM2Worker_AdapterPaginationAcrossPageBoundaries
  TestM2Worker_AdapterCrossTenantReadsAndCancelAreRefused
  TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve
  TestM2Worker_PlainTextPartPassesThroughTheSharedTable
  TestM2Worker_ContractEdgeCases
  TestM2Worker_ZContractDrift
  TestM2Worker_OutagePhase1Start
  TestM2Worker_OutagePhase2Down
  TestM2Worker_OutagePhase3Resume
  TestJobsLive_M2_SendJobSettlesItemByItem
  TestJobsLive_M2_CancelRunningJob
  TestJobsLive_M2_PauseAndResumeARunningJob
  TestJobsLive_M2_CancelAPausedRunningJob
  TestJobsLive_Outage_M2_HaltsCleanlyAndCompletesOnce
)
# The three phases of the outage proof (B12; tests/harness/m2_worker_outage_test.go). They are in M2W_TESTS (the PASS-line list and the stage's floor count them) but they are NOT part of the ordinary run of
# m2w_scope_run: each is its own `go test -test.run '^Name$'` of the same binary, with module 2's container stopped and started between them (m2w_outage). The order here IS the order they run in.
M2W_OUTAGE_TESTS=(
  TestM2Worker_OutagePhase1Start
  TestM2Worker_OutagePhase2Down
  TestM2Worker_OutagePhase3Resume
)

m2w_psql_su() { DC exec -T postgres psql -U supabase_admin -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }

# ── stage 2: credentials ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Called right AFTER run.sh wrote $WORK/harness.env: it mints, masks, then appends its four lines to the same file. The key and the token are 24 random bytes in hex, minted by module 2's provision-worker
# (the same tool that minted the service token and module 3's worker token); the hashes are bcrypt. The plaintexts exist only in shell variables and in the mode-600 env file.
m2w_mint() {
  local a w
  a="$(mint)"; w="$(mint)"
  M2_ADMIN_KEY="$(printf '%s\n' "$a" | field ADMIN_API_KEY)"; M2_ADMIN_HASH="$(printf '%s\n' "$a" | field ADMIN_API_KEY_HASH)"
  M2_WORKER_TOKEN="$(printf '%s\n' "$w" | field ADMIN_API_KEY)"; M2_WORKER_HASH="$(printf '%s\n' "$w" | field ADMIN_API_KEY_HASH)"
  M2_WORKER_ID="$(cat /proc/sys/kernel/random/uuid)"
  a=""; w=""
  [ -n "$M2_ADMIN_KEY" ] && [ -n "$M2_ADMIN_HASH" ] && [ -n "$M2_WORKER_TOKEN" ] && [ -n "$M2_WORKER_HASH" ] && [ -n "$M2_WORKER_ID" ] || die "could not mint module 2's operator key and worker credentials (provision-worker admin-key printed no key or hash)"
  for v in "$M2_ADMIN_KEY" "$M2_WORKER_TOKEN" "$M2_ADMIN_HASH" "$M2_WORKER_HASH"; do mask "$v"; done      # masked BEFORE they are written anywhere or handed to any command
  [ "$M2_ADMIN_KEY" != "$M2_TOKEN" ] && [ "$M2_ADMIN_KEY" != "$M2_WORKER_TOKEN" ] && [ "$M2_TOKEN" != "$M2_WORKER_TOKEN" ] || die "module 2's operator key, service token and worker token must be three different credentials (the surfaces must not share one)"
  case "$M2_WORKER_ID" in ????????-????-????-????-????????????) ;; *) die "the worker id is not a uuid (send_managed.boxes.id is one)";; esac
  [ -f "$WORK/harness.env" ] || die "the env file is missing: m2w_mint runs after run.sh wrote it"
  ( umask 077; {
      echo "H_M2_ADMIN_KEY=$M2_ADMIN_KEY"
      echo "H_M2_ADMIN_KEY_HASH=$(esc "$M2_ADMIN_HASH")"
      echo "H_M2_WORKER_ID=$M2_WORKER_ID"
      echo "H_M2_WORKER_TOKEN=$M2_WORKER_TOKEN"
    } >> "$WORK/harness.env" ) || die "could not append module 2's operator key and worker credentials to the env file"
  echo "  minted for module 2's real worker: 1 operator key (+ bcrypt hash), 1 worker id, 1 worker token (+ bcrypt hash) — masked, none printed"
}

# ── stage 5c: the worker ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Reads, never prints: owner uid and mode of each key file, and the certificates' names. tools/own-secrets.sh (stage 2) chowned control.key and dev-master.key to the Control's uid (10001) and worker.key to the
# worker's (10002), kept them mode 600 and proved each is readable by its own uid and by no other; this re-reads the result on the host just before the worker starts, so that a 'permission denied' inside the
# container is never the first anybody hears of it. On a host that does not enforce ownership (a Windows or macOS bind mount) only the existence is checked.
m2w_preflight() {
  local certs="$M2/secrets/dev/certs" kms="$M2/secrets/dev/kms" spec f want got
  for spec in "$certs/worker.key|600|10002" "$certs/control.key|600|10001" "$kms/dev-master.key|600|10001"; do
    f="${spec%%|*}"; want="${spec#*|}"; want="${want/|/ }"
    [ -f "$f" ] || die "module 2's $(basename "$f") does not exist after stage 2 (gen-certs.sh / gen-secrets.sh)"
    if [ "$(uname -s)" = Linux ]; then
      got="$(stat -c '%a %u' "$f")"
      [ "$got" = "$want" ] || die "module 2's $(basename "$f") has (mode, uid) = ($got), expected ($want): the container's user cannot read a key that is not owned by its uid (tools/own-secrets.sh gives each key to the one uid that reads it, mode 600)"
    fi
  done
  for f in worker.crt control.crt ca.crt; do [ -s "$certs/$f" ] || die "module 2's $f is missing or empty after stage 2"; done
  openssl verify -CAfile "$certs/ca.crt" -purpose sslclient "$certs/worker.crt" >/dev/null 2>&1 || die "module 2's worker certificate does not verify against the dev CA as a TLS client certificate: the Control would refuse the worker at the handshake"
  openssl verify -CAfile "$certs/ca.crt" -purpose sslserver "$certs/control.crt" >/dev/null 2>&1 || die "module 2's Control certificate does not verify against the dev CA as a TLS server certificate: the worker would refuse the Control"
  # the worker dials the host m2 and verifies the name `control` (CONTROL_SERVER_NAME): the Control certificate must carry that name
  openssl x509 -in "$certs/control.crt" -noout -text 2>/dev/null | grep -A1 'Subject Alternative Name' | grep -q 'DNS:control' || die "module 2's Control certificate has no subject alternative name DNS:control: the worker verifies that name (CONTROL_SERVER_NAME), not the host it dials"
  echo "  key files: worker.key is mode 600 owned by uid 10002, control.key and dev-master.key mode 600 owned by uid 10001 ($(uname -s) host); worker.crt verifies as a client certificate, control.crt as a server certificate and carries DNS:control"
}

m2w_stage_5c() {
  STAGE=m2-worker; banner "5c" "module 2's real worker (its box row seeded BEFORE it starts, a heartbeat newer than the insert) and the controllable sink as its SMTP target"
  m2w_preflight
  DC up -d --wait --wait-timeout 180 smtp-ctl || die "the controllable SMTP sink (smtp-ctl) did not become healthy (it exits at start when it cannot read module 3's dev sink certificate and key: its log is below)"
  local t0 i
  t0="$(m2w_psql_su "select clock_timestamp()")"; [ -n "$t0" ] || die "could not read the database clock"
  [ "$(m2w_psql_su "select count(*) from public.users where id = '$M2W_SYSTEM_USER'")" = 1 ] || die "the reserved system user is not in public.users: platform_0001 inserts it, and module 2's boxes row references it"
  m2w_psql_su "INSERT INTO send_managed.boxes (id, user_id, token_hash, status) VALUES ('$M2_WORKER_ID', '$M2W_SYSTEM_USER', '$M2_WORKER_HASH', 'active')" >/dev/null || die "could not seed module 2's worker box row"
  DC up -d m2-worker || die "module 2's worker did not start"
  i=0; while [ $i -lt 60 ]; do
    [ "$(m2w_psql_su "select count(*) from send_managed.boxes where id = '$M2_WORKER_ID' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
    i=$((i+1)); sleep 2
  done
  [ "$i" -lt 60 ] || die "module 2's worker never heartbeated after $t0 (it registers over mTLS on the Control's stream port with its token; the logs below say 'register REJECTED', 'connect attempt N not ready' or nothing: see tests/harness/m2worker/FIRST-RUN.md)"
  echo "  module 2's worker registered and heartbeated after $((i*2)) s (send_managed.boxes, database clock)"
  DC logs --no-color m2 m2-worker 2>&1 | grep -i -E 'worker .*regist|registered|register' | head -6 | sed 's/^/     | /' || true
  echo "  the worker is registered and heartbeating; the sink answers; module 2's account will name the sink's PRIVATE address (the test asserts the range)"
}

# ── stage 6 helpers ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
m2w_evidence() {
  echo "  ---- module 2's real worker: the test's own log lines (the sink's per-recipient counts, timings, snapshots) ----"
  grep -E " m2_worker(_e2e|_outage|_contract_edge|_jobs)?(_helpers)?_test.go:[0-9]+: |^--- (PASS|FAIL|SKIP): Test(M2Worker_|JobsLive_M2_|JobsLive_Outage_M2_)" "$1" | sed 's/^/  | /' | head -700
}

m2w_require_pass() {
  local t
  for t in "${M2W_TESTS[@]}"; do
    grep -q "^--- PASS: $t " "$1" || die "$t has no '--- PASS' line in the run log: it did not run, or did not pass (module 2's real worker sent, bounced, exhausted or cancelled nothing it was not seen to)"
  done
  echo "  module 2's real worker: ${#M2W_TESTS[@]} TestM2Worker_* tests have a '--- PASS' line"
}

# the credentials of this run, in no log of the services that handled them. grep reads the patterns from a file descriptor, so no credential is on a command line; nothing is printed but a name.
m2w_secret_leak() {
  local svc
  for svc in m2 m2-worker smtp-ctl; do
    DC logs --no-color "$svc" > "$WORK/leak-$svc.log" 2>&1 || die "could not read the logs of $svc"
    if grep -qFf <(printf '%s\n' "$M2_ADMIN_KEY" "$M2_WORKER_TOKEN" "$M2_TOKEN" "$SINK_PASS") "$WORK/leak-$svc.log"; then
      rm -f "$WORK/leak-$svc.log"; die "a credential of this run (module 2's operator key, worker token or service token, or the sink password) appears in the log of $svc (the log is not shown: it is public)"
    fi
    rm -f "$WORK/leak-$svc.log"
  done
  echo "  no credential of this run (operator key, worker token, service token, SMTP password) appears in the logs of m2, m2-worker or smtp-ctl"
}

# ── the outage sub-stage (Phase 3, chunk B12) ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Module 2's platform API container `m2` is STOPPED in the middle of a campaign and STARTED again; between the stop and the start the platform's adapter is asked everything it polls with, and after the start the
# campaign completes exactly once. Three phases, three `go test` invocations of the SAME compiled binary (each a fresh container; HARNESS_M2_OUTAGE=1 makes the tests do their real work, without it they run only
# their paired controls), the state between them in harness_outage.m2 (tests/harness/m2_worker_outage_helpers_test.go):
#     phase 1  TestM2Worker_OutagePhase1Start   a job in flight; what is final is settled; the books, module 2's job rows and the sink are recorded
#     STOP     m2w_outage_stop                  docker compose stop m2 (graceful: the send in flight reports, the Control exits), then PROOF that it is gone: the container is exited AND a request from a container
#                                               on the private network gets no HTTP answer; the sink, the worker and Postgres keep running
#     phase 2  TestM2Worker_OutagePhase2Down    every read and write against the stopped module fails typed with no value; nothing is settled or invented; the platform's /readyz handler answers 200
#     START    m2w_outage_start / _worker       docker compose up --wait m2, then a request over the private network until /readyz answers 200 (bounded: the stage dies with the module's logs if it never does); the worker
#                                               is waited for (a heartbeat newer than the start) and started again if it had exited (its own reconnect budget is about 30 s: it exits "for the orchestrator", which here is this stage)
#     phase 3  TestM2Worker_OutagePhase3Resume  poll to completion, every item exactly once, one message per sent item, one ledger entry per charged item, books closed, replays write and send nothing
# Failure sentences (each has a case in stage-selftest.sh): "module 2 could not be stopped", "module 2 did not stop", "module 2 still answers", "the probe from the private network did not run",
# "module 2 did not become healthy again", "module 2 never became ready over the private network", "module 2's worker never heartbeated", "has no '--- PASS' line in its own log",
# "wrote no 'NOTE outage phaseN live' line", "the platform's /readyz answered".
# The sub-stage's evidence is its own logs ($WORK/outage-p1.log .. p3.log), printed per phase; the floor counts the three PASS lines (m2w_scope_run appends them to the run log).

# one request from a container on the private network (the test container's image, busybox wget; no credential is sent) to module 2's readiness route: prints wget's own text, returns wget's own status
m2w_outage_probe() { DC run --rm -T --no-deps --entrypoint wget harness-tests -q -T 3 -O /dev/null http://m2:8080/readyz 2>&1; }

# the state of a compose service's container ("running", "exited", ...); empty when there is none
m2w_outage_state() { local cid; cid="$(DC ps -a -q "$1" 2>/dev/null | head -1)"; [ -n "$cid" ] || return 0; docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null; }

# m2w_outage_gone: the container m2 is exited AND a request over the private network gets no HTTP answer (a refusal or an unresolvable name: both are "gone"). A probe that did not run at all proves nothing and is red.
m2w_outage_gone() {
  local cid st out rc i=0 tries="${M2W_OUTAGE_STOP_TRIES:-15}"
  while :; do
    cid="$(DC ps -a -q m2 2>/dev/null | head -1)"
    st=""; [ -z "$cid" ] || st="$(docker inspect -f '{{.State.Status}} {{.State.ExitCode}}' "$cid" 2>/dev/null)"
    case "$st" in "exited "*|"dead "*) break;; esac
    i=$((i+1)); [ "$i" -lt "$tries" ] || die "module 2 did not stop: the container m2 is '${st:-not found}' after $tries checks following docker compose stop (a graceful stop waits for the send in flight to report, then the Control exits; its logs are below)"
    sleep "${M2W_OUTAGE_SLEEP:-2}"
  done
  out="$(m2w_outage_probe)"; rc=$?
  [ "$rc" != 0 ] || die "module 2 still answers: a request from a container on the private network to http://m2:8080/readyz succeeded although the container m2 is '$st'"
  case "$out" in
    *"server returned error"*) die "module 2 still answers over the private network (an HTTP error, so something listens at the name m2): $(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
    *"Connection refused"*|*"bad address"*|*"can't connect"*|*"timed out"*|*"nreachable"*) ;;
    *) die "the probe from the private network did not run, so 'gone' was not proven (wget printed: $(printf '%s' "$out" | head -3 | tr '\n' ' '))";;
  esac
  echo "  module 2 is gone: container m2 is '$st' (status, exit code) and a request from a container on the private network gets: $(printf '%s\n' "$out" | grep -m1 'wget:' || printf '%s\n' "$out" | head -1)"
}

m2w_outage_stop() {
  local s st
  echo "  $(date -u +%T) stopping module 2: docker compose stop m2 (SIGTERM: the Control stops claiming, the send in flight reports, then it exits; grace period 90 s)"
  DC stop m2 >/dev/null 2>&1 || die "module 2 could not be stopped (docker compose stop m2 failed)"
  m2w_outage_gone
  for s in postgres smtp-ctl m2-worker; do
    st="$(m2w_outage_state "$s")"
    [ "$st" = running ] || die "stopping module 2 took $s down with it (it is '${st:-not found}'): the outage proof leaves Postgres, the sink and the worker running"
  done
  echo "  postgres, smtp-ctl and m2-worker are still running: only module 2's Control is down"
}

# the running platform service (a full run only) answers /readyz 200 while module 2 is stopped; in the scope there is no platform service and phase 2 asks the platform's readiness handler in process
m2w_outage_platform_ready() {
  local code
  if [ -z "$(DC ps -q platform 2>/dev/null)" ]; then
    echo "  the platform service is not part of this run (scope m2worker): phase 2 asks the platform's own /readyz handler (internal/httpapi) in process"
    return 0
  fi
  code="$(DC exec -T platform wget -q -S -O /dev/null http://127.0.0.1:8080/readyz 2>&1 | awk '/HTTP\//{print $2; exit}')"
  [ "$code" = 200 ] || die "the platform's /readyz answered '${code:-nothing}' while module 2 is stopped, want 200: the platform's readiness follows its database, never a module"
  echo "  the running platform service answers /readyz 200 while module 2 is stopped"
}

# start module 2 and wait for it over the private network, the way stage 9b waits for module 3 (compose's own health wait, then a bounded loop); dies with the module's logs when it never comes back
m2w_outage_start() {
  local out i=0 tries="${M2W_OUTAGE_READY_TRIES:-60}"
  echo "  $(date -u +%T) starting module 2 again: docker compose up -d --wait m2, then /readyz over the private network"
  DC up -d --wait --wait-timeout 240 m2 >/dev/null 2>&1 || { DC logs --no-color --tail=80 m2 2>&1 | sed 's/^/     | m2: /'; die "module 2 did not become healthy again after the start (its logs are above)"; }
  while :; do
    out="$(m2w_outage_probe)" && break
    i=$((i+1))
    if [ "$i" -ge "$tries" ]; then
      DC logs --no-color --tail=80 m2 2>&1 | sed 's/^/     | m2: /'
      die "module 2 never became ready over the private network: $tries requests from a container on the network to http://m2:8080/readyz failed, the last said: $(printf '%s' "$out" | head -2 | tr '\n' ' ') (its logs are above)"
    fi
    sleep "${M2W_OUTAGE_SLEEP:-2}"
  done
  echo "  module 2 is back: healthy by its own check, and a request from a container on the private network gets HTTP 200 on /readyz (after $i failed probe(s))"
}

# the worker after the module is back: a heartbeat newer than now (the Control must see it register again). A worker that exited while the Control was away (its own reconnect budget is about 30 attempts: module 2's design is
# "exit non-zero for the orchestrator to restart us", and the orchestrator here is this stage) is started again; one that is still running has reconnected by itself.
m2w_outage_worker() {
  local t0 st i=0 tries="${M2W_OUTAGE_HB_TRIES:-45}" how="it had never stopped and reconnected by itself" code
  t0="$(m2w_psql_su "select clock_timestamp()")"; [ -n "$t0" ] || die "could not read the database clock"
  while :; do
    st="$(m2w_outage_state m2-worker)"
    if [ "$st" != running ]; then
      code="$(docker inspect -f '{{.State.ExitCode}}' "$(DC ps -a -q m2-worker 2>/dev/null | head -1)" 2>/dev/null)"
      how="it had exited ('${st:-not found}', exit code ${code:-?}) after its reconnect budget ran out, as module 2's worker is designed to, and this stage started it again"
      DC up -d m2-worker >/dev/null 2>&1 || die "module 2's worker could not be started again"
    fi
    [ "$(m2w_psql_su "select count(*) from send_managed.boxes where id = '$M2_WORKER_ID' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
    i=$((i+1))
    if [ "$i" -ge "$tries" ]; then
      DC logs --no-color --tail=60 m2-worker 2>&1 | sed 's/^/     | m2-worker: /'
      die "module 2's worker never heartbeated after $t0 following the module's return ($tries checks; its logs are above): the Control does not accept it again"
    fi
    sleep "${M2W_OUTAGE_SLEEP:-2}"
  done
  echo "  module 2's worker is registered and heartbeating again: $how"
}

# the log of one phase: the test's own evidence lines and its verdict line
m2w_outage_evidence() {
  grep -E " m2_worker_outage(_helpers)?_test.go:[0-9]+: |^--- (PASS|FAIL|SKIP): TestM2Worker_" "$1" | sed 's/^/  | /' | head -200
}

# a phase counts only when ITS OWN log has the column-0 PASS line and the line only a live run writes: without HARNESS_M2_OUTAGE=1 the test passes on its controls alone, and that PASS must never stand in for the phase
m2w_outage_require_phase() {   # <test name> <phase number> <log>
  grep -q "^--- PASS: $1 " "$3" || die "$1 has no '--- PASS' line in its own log: phase $2 of module 2's outage proof did not run, or did not pass"
  grep -q "NOTE outage phase$2 live: " "$3" || die "$1 passed but wrote no 'NOTE outage phase$2 live' line: it ran control-only (HARNESS_M2_OUTAGE=1 did not reach the test container), so phase $2 was NOT run against the stack"
}

# one phase: a fresh test container running ONE test of the compiled binary with the outage variable set
m2w_outage_phase() {   # <test name> <phase number>
  local log="$WORK/outage-p$2.log" rc
  echo "  $(date -u +%T) phase $2: $1 (a fresh test container, HARNESS_M2_OUTAGE=1)"
  DC run --rm -T -e HARNESS_M2_OUTAGE=1 harness-tests -test.v -test.count=1 -test.timeout=10m -test.run "^$1\$" > "$log" 2>&1; rc=$?
  m2w_outage_evidence "$log"
  if [ "$rc" != 0 ]; then
    echo "  ---- $1 failed (exit $rc): the end of its log ----"; tail -120 "$log" | sed 's/^/  | /'
    die "$1 failed (exit $rc): phase $2 of module 2's outage proof"
  fi
  m2w_outage_require_phase "$1" "$2" "$log"
}

m2w_outage() {
  local t0=$SECONDS
  STAGE=m2-outage; banner "6b-m2" "module 2 is stopped in the middle of a campaign and started again: the platform neither invents nor loses anything (three phases, one stage)"
  [ -f "$WORK/out/harness.test" ] || die "the compiled harness test binary is missing ($WORK/out/harness.test): the outage proof runs its three phases from it"
  echo "  phases (one go test -test.run per phase, a fresh container each, state in harness_outage.m2): ${M2W_OUTAGE_TESTS[*]}"
  m2w_outage_phase "${M2W_OUTAGE_TESTS[0]}" 1
  m2w_outage_stop
  m2w_outage_platform_ready
  m2w_outage_phase "${M2W_OUTAGE_TESTS[1]}" 2
  m2w_outage_start
  m2w_outage_worker
  m2w_outage_phase "${M2W_OUTAGE_TESTS[2]}" 3
  echo "  module 2's outage proof: all three phases ran live and passed in $((SECONDS - t0)) s (stop -> gone -> phase 2 -> start -> ready -> phase 3); the same flow with the module up is TestM2Worker_AdapterEndToEndSettlesItemByItem"
}

# ── H_SCOPE=m2worker ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Postgres, the four migration chains (stage 4 ran), module 2 and its worker, the sink, and only the TestM2Worker tests; no module 1, no module 3, no platform service, no probe, no outage stages.
# Called by run.sh after stage 4b; it never returns (it exits with the run's own status, the EXIT trap tears the stack down).
m2w_scope_run() {
  STAGE=services; banner 5 "scope m2worker: module 2's Control only (module 1, module 3 and the platform service are not started)"
  DC up -d --wait --wait-timeout 240 m2 || die "module 2 did not become healthy"
  DC ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}' | sed 's/^/  | /'
  m2w_stage_5c

  STAGE=tests; banner 6 "scope m2worker: the TestM2Worker_* tests, compiled on the runner and run inside the network"
  local re='^(TestM2Worker_|TestJobsLive_M2_|TestJobsLive_Outage_M2_)' skip='^TestM2Worker_OutagePhase' trc lrc t
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags "$TEST_TAGS" -c -o "$WORK/out/harness.test" ./tests/harness ) || die "the harness test binary (tags: $TEST_TAGS) did not compile"
  DC run --rm -T harness-tests -test.list "$re" > "$WORK/h-list.log" 2>&1; lrc=$?
  [ "$lrc" = 0 ] || { cat "$WORK/h-list.log"; die "listing the TestM2Worker tests failed ($lrc)"; }
  for t in "${M2W_TESTS[@]}"; do grep -qx "$t" "$WORK/h-list.log" || die "$t is not among the declared tests of the harness binary"; done
  # the ordinary run leaves the three outage phases out (-test.skip): each is its own invocation between a stop and a start of module 2 (m2w_outage); their PASS lines join this log before the floor reads it
  DC run --rm -T harness-tests -test.v -test.count=1 -test.timeout=30m -test.run "$re" -test.skip "$skip" > "$WORK/h-run.log" 2>&1; trc=$?
  sed 's/^/  | /' "$WORK/h-run.log" | tail -60
  m2w_evidence "$WORK/h-run.log"
  drift_evidence "$WORK/h-run.log"   # the contract drift session of module 2 (and, in record mode, its recorded fixtures): Phase 3, chunk B6
  [ "$trc" = 0 ] || { echo "  ---- failing tests (with context) ----"; grep -n -B1 -A14 -e '--- FAIL' "$WORK/h-run.log" | sed 's/^/  | /' | head -240; }
  if [ "$trc" = 0 ]; then
    m2w_outage
    grep -h '^--- PASS: TestM2Worker_OutagePhase' "$WORK/outage-p1.log" "$WORK/outage-p2.log" "$WORK/outage-p3.log" >> "$WORK/h-run.log" || die "the outage phases left no PASS line to count"
  fi
  if [ "$trc" = 0 ]; then t="ok  \t$GO_PKG\t0.000s"; else t="FAIL\t$GO_PKG\t0.000s"; fi
  printf "$t\n" >> "$WORK/h-run.log"; printf "ok  \t$GO_PKG\t0.000s\n" >> "$WORK/h-list.log"
  [ "$trc" = 0 ] || die "the TestM2Worker tests failed (exit $trc)"
  m2w_require_pass "$WORK/h-run.log"
  # the floor of THIS scope is the number of tests the scope names (the full floor, tests/floors-harness.txt, is asserted only by the full run)
  printf '# the TestM2Worker tests of scope m2worker\ngo|%s|%s\n' "$GO_PKG" "${#M2W_TESTS[@]}" > "$WORK/floors-m2worker.txt"
  bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$WORK/floors-m2worker.txt" --go-run-log "$WORK/h-run.log" --go-list-log "$WORK/h-list.log" || die "the TestM2Worker test-count floor"
  m2w_secret_leak

  STAGE=exposure; banner 8 "scope m2worker: no service publishes a port; the network is internal"
  local ids id pb cnt=0
  DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres m2 m2-worker smtp-ctl || die "a service is reachable from outside the network"
  [ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
  ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
  for id in $ids; do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"

  STAGE=done
  echo
  echo "================ NOT CHECKED (scope m2worker: what this run did not and could not vouch for)"
  m2w_not_checked
  echo "  - everything outside module 2 and its worker: module 1, module 3, the platform service, the probe, the outage and leader stages and the ledger check are NOT run in this scope (H_SCOPE=full runs them)"
  echo
  echo "HARNESS RESULT: GREEN (scope m2worker: ${#M2W_TESTS[@]} TestM2Worker tests) — module 2 built from $(lockpin coldmail-send-managed | cut -c1-7)"
  exit 0
}

# what the module-2 worker stage does not vouch for (printed by the scoped run and, in the full run, run.sh's own list refers to it)
m2w_not_checked() {   # `m2w_not_checked correction` (the full run) first says that the first bullet of run.sh's own list no longer holds for module 2
  [ "${1:-}" = correction ] && echo '  - CORRECTION to the first bullet above, for a run in which module 2'"'"'s real worker stage ran: module 2 DOES have a worker here (the bullet below), so "Module 2 has no worker" no longer holds; module 1 still has no SMTP node.'
  cat <<'EOF'
  - Module 2 REALLY sends here: ONE worker (so one job at a time: a held send stalls everything behind it) to the controllable sink (tests/harness/sink), which accepts, rejects (550) or defers (451) named recipients at RCPT TO
    and holds the final reply. Proven end to end: sent, bounced (a 550 at RCPT TO), failed/attempts_exhausted (two 451 deferrals with the attempt cap and the backoff ladder shortened) and cancelled (a held campaign cancelled mid-flight),
    each observed through module 2's own report and the sink's independent counters. NOT exercised: a second worker, a worker that dies mid-send or loses its stream (reclaim), a Control CRASH (the outage proof below stops it gracefully), an account that fails AUTH (535: the
    quarantine path) or a pool with several accounts (routing, fairness, daily limits), the throttle class (a rate-limit text in a 4xx), a reply that never comes (the 30 s operation timeout), the real public internet (the sink's address is
    private: the worker skips certificate validation for private ranges only, and module 2's production rule is the opposite), variation of the message body (switched on by default; the sink is read for recipients and ids, not for body bytes),
    a campaign of more than a handful of items, and the platform's job engine (the test calls module 2's routes itself).
  - The OUTAGE proof (m2w_outage) stops module 2's Control with `docker compose stop` (SIGTERM, a graceful drain: the send in flight reports, nothing is left in doubt) once, while ONE campaign of ten items is in flight, and starts it again.
    NOT exercised: a SIGKILL of the Control in the middle of a send (the worker's unacknowledged outcome buffer and the Control's startup rescan are what keep that exactly-once; here the worker has nothing buffered and, after about 30 s without
    a Control, exits and is started again by the stage), an outage longer than the worker's buffer TTL, a stop that outlives the drain window, two outages in a row, and the platform's job engine (the test plays its poll-and-settle step).
  - the dials of module 2 that make the run take seconds (job_max_failed_attempts 2, retry_backoff_schedule [1,1,1] with a ceiling of 1 s, send_delay_min_sec / send_delay_max_sec 0, concurrency_per_user_default 1,
    dispatch_tick_interval_ms 250) are set through module 2's admin API for the run and restored afterwards: the module's DEFAULT cadence (a retry ladder of 30 s, 120 s and 600 s, six attempts) is not what ran.
EOF
}
