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
#   m2w_scope_run       H_SCOPE=m2worker: Postgres + migrations + module 2 + its worker + the sink + only the TestM2Worker tests, then the exposure check; ends the run
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
  grep -E " m2_worker(_e2e)?(_helpers)?_test.go:[0-9]+: |^--- (PASS|FAIL|SKIP): TestM2Worker_" "$1" | sed 's/^/  | /' | head -700
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

# ── H_SCOPE=m2worker ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Postgres, the four migration chains (stage 4 ran), module 2 and its worker, the sink, and only the TestM2Worker tests; no module 1, no module 3, no platform service, no probe, no outage stages.
# Called by run.sh after stage 4b; it never returns (it exits with the run's own status, the EXIT trap tears the stack down).
m2w_scope_run() {
  STAGE=services; banner 5 "scope m2worker: module 2's Control only (module 1, module 3 and the platform service are not started)"
  DC up -d --wait --wait-timeout 240 m2 || die "module 2 did not become healthy"
  DC ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}' | sed 's/^/  | /'
  m2w_stage_5c

  STAGE=tests; banner 6 "scope m2worker: the TestM2Worker_* tests, compiled on the runner and run inside the network"
  local re='^TestM2Worker_' trc lrc t
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags "$TEST_TAGS" -c -o "$WORK/out/harness.test" ./tests/harness ) || die "the harness test binary (tags: $TEST_TAGS) did not compile"
  DC run --rm -T harness-tests -test.list "$re" > "$WORK/h-list.log" 2>&1; lrc=$?
  [ "$lrc" = 0 ] || { cat "$WORK/h-list.log"; die "listing the TestM2Worker tests failed ($lrc)"; }
  for t in "${M2W_TESTS[@]}"; do grep -qx "$t" "$WORK/h-list.log" || die "$t is not among the declared tests of the harness binary"; done
  DC run --rm -T harness-tests -test.v -test.count=1 -test.timeout=30m -test.run "$re" > "$WORK/h-run.log" 2>&1; trc=$?
  sed 's/^/  | /' "$WORK/h-run.log" | tail -60
  m2w_evidence "$WORK/h-run.log"
  [ "$trc" = 0 ] || { echo "  ---- failing tests (with context) ----"; grep -n -B1 -A14 -e '--- FAIL' "$WORK/h-run.log" | sed 's/^/  | /' | head -240; }
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
    each observed through module 2's own report and the sink's independent counters. NOT exercised: a second worker, a worker that dies mid-send or loses its stream (reclaim), a Control restart, an account that fails AUTH (535: the
    quarantine path) or a pool with several accounts (routing, fairness, daily limits), the throttle class (a rate-limit text in a 4xx), a reply that never comes (the 30 s operation timeout), the real public internet (the sink's address is
    private: the worker skips certificate validation for private ranges only, and module 2's production rule is the opposite), variation of the message body (switched on by default; the sink is read for recipients and ids, not for body bytes),
    a campaign of more than a handful of items, and the platform's job engine (the test calls module 2's routes itself).
  - the dials of module 2 that make the run take seconds (job_max_failed_attempts 2, retry_backoff_schedule [1,1,1] with a ceiling of 1 s, send_delay_min_sec / send_delay_max_sec 0, concurrency_per_user_default 1,
    dispatch_tick_interval_ms 250) are set through module 2's admin API for the run and restored afterwards: the module's DEFAULT cadence (a retry ladder of 30 s, 120 s and 600 s, six attempts) is not what ran.
EOF
}
