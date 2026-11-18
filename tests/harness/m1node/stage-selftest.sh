#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/m1node/m1node.sh go RED, for the reason they name, and stay green on a good input; and that the files of the module-1 node stack agree with
# each other (the list of tests, the recipe domains, the compose services). No Docker, no Go build, no Ruby needed: the functions are sourced and run against stubs. What it covers:
#   m1n_mint         makes the CA and the api.ipify.org certificate with openssl (the leaf verifies against the CA for that host name; the CA key is gone afterwards), writes exactly two lines to the env file,
#                    masks the node token BEFORE it is written, never prints it, writes two resolv.conf files with a nameserver and nothing that turns the node's audit red; refuses a prefix that is not three octets
#   m1n_secret_leak  green on clean logs; red, naming the CREDENTIAL (never printing its value), when any of the six credentials is in a log of the stack; red when a credential is empty (a scan with nothing to look for)
#   outage sub-stage m1n_outage_phase, m1n_outage_stop, m1n_outage_start, m1n_outage and m1n_floor run against stubs of docker compose: each failure sentence (a phase that fails or has no PASS line, a stop that did
#                    not stop, a probe that proves nothing, a start that never becomes healthy or ready, a node that is not green, a promotion that changes no row, a missing phase log, the floor) has a case that
#                    asserts it, and the order phase 1, stop, phase 2, start, phase 3 is asserted on the output
#   consistency      M1N_TESTS and M1N_OUTAGE_TESTS together are exactly the set of TestM1Node_ functions in the m1_node*_test.go files (a test added without the list, or a listed test that does not exist, is red); every recipe domain the Go tests and
#                    the direct-node table use is in fixture.json; the exposure check names the stack's three services; the compose file defines exactly the four services the scripts start
# NOT covered here (they need a Linux host with Docker): m1n_stage1..4, m1n_exposure, node-probe.rb on a node (its syntax is checked with `ruby -c` when this host has Ruby, and said so when it has not).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=60; cases=0; ok=0; bad=0
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
banner() { echo "== [$1] $2"; }
MASKLOG="$WORK/mask.log"
mask() { if [ -f "$WORK/harness.env" ] && grep -qF -- "$1" "$WORK/harness.env"; then echo "AFTER_WRITE" >> "$MASKLOG"; else echo "BEFORE_WRITE" >> "$MASKLOG"; fi; }
lockpin() { echo 0000000; }
GO_PKG=github.com/coldmail-platform/platform/tests/harness
EOF

# ── m1n_mint ──
mint_run() { # label [prefix]
  W="$T/w-$1"; mkdir -p "$W"; : > "$W/harness.env"
  ( set -u; WORK="$W"; export WORK; PLATFORM_DIR="$HARNESS/../.."; H_FX_PREFIX="${2:-}"; [ -n "$H_FX_PREFIX" ] || unset H_FX_PREFIX
    . "$T/env.sh"; . "$HERE/m1node.sh"; m1n_mint ) 2>&1
}
if command -v openssl >/dev/null 2>&1; then
  out="$(mint_run good)"; rc=$?
  verdict "m1n_mint: a good mint" 0 "none of the secrets printed" "$out" "$rc"
  W="$T/w-good"; env="$W/harness.env"; tok="$(sed -n 's/^H_M1_NODE_TOKEN=//p' "$env")"
  verdict "m1n_mint: exactly two lines in the env file, the token and the prefix" 0 "two" "$([ "$(grep -c '' "$env")" = 2 ] && grep -q '^H_M1_NODE_TOKEN=[0-9a-f]\{48\}$' "$env" && grep -q '^H_FX_PREFIX=100.128.0$' "$env" && echo two || echo "$(grep -c '' "$env") lines")" 0
  verdict "m1n_mint: the token was masked BEFORE it was written to the env file" 0 "1 before, 0 after" "$(printf '%s before, %s after' "$(grep -c BEFORE_WRITE "$W/mask.log")" "$(grep -c AFTER_WRITE "$W/mask.log" || true)")" 0
  case "$out" in *"$tok"*) leaked=yes;; *) leaked=no;; esac
  verdict "m1n_mint: the output never shows the token" 0 "no" "$leaked" 0
  verdict "m1n_mint: the CA key does not outlive the signing" 0 "gone" "$([ ! -e "$W/m1fx/ca.key" ] && [ ! -e "$W/m1fx/ca.srl" ] && echo gone || echo "still there")" 0
  c="$W/m1fx"
  verdict "m1n_mint: the leaf verifies against the CA for api.ipify.org" 0 "OK" "$(openssl verify -CAfile "$c/ca.pem" -verify_hostname api.ipify.org "$c/tls/ipify.crt" 2>&1)" 0
  # the control: the same leaf does NOT verify for another host name, so the line above is a real check
  out2="$(openssl verify -CAfile "$c/ca.pem" -verify_hostname api.example.org "$c/tls/ipify.crt" 2>&1)"; rc2=$?; [ "$rc2" -ne 0 ] && rc2=1   # the exit status of a refused verify differs between OpenSSL versions: any non-zero is a refusal
  verdict "m1n_mint: the leaf does not verify for another host name (the control)" 1 "" "$out2" "$rc2"
  verdict "m1n_mint: the resolv.conf files hold a nameserver and nothing that reddens the audit" 0 "clean" "$(for f in node-resolv.conf cp-resolv.conf; do grep -qE '^nameserver 100\.128\.0\.2$' "$c/$f" && ! grep -qE '^(search|domain|options .*ndots)' "$c/$f" || echo "bad $f"; done | grep . || echo clean)" 0
  out="$(mint_run bad 10.0)"; rc=$?
  verdict "m1n_mint: a two-octet prefix is refused" 1 "is not a three-octet prefix" "$out" "$rc"
  out="$(mint_run bad4 203.0.113.4)"; rc=$?
  verdict "m1n_mint: a four-octet address is refused" 1 "is not a three-octet prefix" "$out" "$rc"
else
  echo "  NOT CHECKED: m1n_mint (no openssl on this host)"; bad=$((bad+1)); cases=$((cases+1))
fi

# ── m1n_secret_leak ──
leak_run() { # label  secret-name-or-empty  [empty-credential-name]
  W="$T/l-$1"; mkdir -p "$W"
  for s in fx verifier-node m1n; do echo "a clean log line of $s" > "$W/svc-$s.txt"; done
  : > "$W/m1n-selfcheck.log"; : > "$W/m1n-list.log"; : > "$W/m1n-run.log"
  ( set -u; WORK="$W"; PLATFORM_DIR=/x; . "$T/env.sh"; . "$HERE/m1node.sh"       # sourced FIRST: m1node.sh starts M1N_NODE_TOKEN empty, like run.sh before m1n_mint
    M1N_NODE_TOKEN=tokenvalue111; M1_KEY=callerkeyvalue222; M1_PEPPER=peppervalue333; M1_QUEUE_KEY=queuekeyvalue444; M1_APP_PASS=apppassvalue555; PG_PASS=pgpassvalue666
    [ -z "${3:-}" ] || eval "$3=''"
    [ -z "${2:-}" ] || { v="$(eval echo "\$$2")"; echo "oops: $v appeared" >> "$W/svc-m1n.txt"; }
    DC() { if [ "$1" = logs ]; then cat "$W/svc-${3}.txt"; else return 1; fi; }
    m1n_secret_leak ) 2>&1
}
out="$(leak_run clean)"; rc=$?
verdict "m1n_secret_leak: clean logs are green" 0 "none present" "$out" "$rc"
for name in M1N_NODE_TOKEN M1_KEY M1_PEPPER M1_QUEUE_KEY M1_APP_PASS PG_PASS; do
  out="$(leak_run "L$name" "$name")"; rc=$?
  case "$name" in M1N_NODE_TOKEN) val=tokenvalue111;; M1_KEY) val=callerkeyvalue222;; M1_PEPPER) val=peppervalue333;; M1_QUEUE_KEY) val=queuekeyvalue444;; M1_APP_PASS) val=apppassvalue555;; PG_PASS) val=pgpassvalue666;; esac
  verdict "m1n_secret_leak: $name in a log is red and the value is not printed" 1 "the value of $name appears 1 time(s)" "$out" "$rc" "$val"
done
out="$(leak_run empty "" M1_PEPPER)"; rc=$?
verdict "m1n_secret_leak: an empty credential is refused (a scan with nothing to look for)" 1 "M1_PEPPER is empty" "$out" "$rc"

# ── consistency of the files ──
srcs=("$HARNESS"/m1_node*_test.go "$HARNESS"/zz_m1_node*_test.go)   # every file of the module-1 node tests: the tests, their helpers, the end-to-end tests of B9-B11 and the contract drift gate (zz_: the last test of the binary)
in_go="$(grep -ho '^func TestM1Node_[A-Za-z0-9_]*' "${srcs[@]}" | sed 's/^func //' | sort | tr '\n' ' ')"
in_sh="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s\n' "${M1N_TESTS[@]}" "${M1N_OUTAGE_TESTS[@]}" ) | sort | tr '\n' ' ')"
verdict "M1N_TESTS and M1N_OUTAGE_TESTS together are exactly the set of TestM1Node_ functions of the m1_node*_test.go files" 0 "same" "$([ -n "$in_go" ] && [ "$in_go" = "$in_sh" ] && echo same || printf 'go: %s\nsh: %s' "$in_go" "$in_sh")" 0
# the control: a list with one more name, or one fewer, is not the same set
verdict "a list with an extra name is not the same set (the control)" 1 "differ" "$([ "$in_go" = "$in_sh TestM1Node_Extra " ] && echo same || echo differ)" 1
fx="$HARNESS/m1node/fixture.json"
miss=""
for d in $(grep -ohE '[a-z0-9-]+\.fixture\.test' "${srcs[@]}" | sort -u); do [ "$d" = nomx.fixture.test ] && continue; grep -qF "\"$d\"" "$fx" || miss="$miss $d"; done
verdict "every fixture domain the Go tests name is in fixture.json (nomx excepted, below)" 0 "none missing" "$([ -z "$miss" ] && echo "none missing" || echo "missing:$miss")" 0
verdict "nomx.fixture.test is NOT in fixture.json: it is the NXDOMAIN recipe, and a name that exists would be NODATA or an answer" 0 "absent" "$(grep -qF nomx "$fx" && echo present || echo absent)" 0
miss=""
for row in $( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s\n' "${M1N_DIRECT[@]}" | cut -d'|' -f2 | sed 's/.*@//' ) ); do grep -qF "\"$row\"" "$fx" || miss="$miss $row"; done
verdict "every domain of the direct-node table is an SMTP domain of fixture.json" 0 "none missing" "$([ -z "$miss" ] && echo "none missing" || echo "missing:$miss")" 0
verdict "the exposure check names the stack's three services" 0 "fx verifier-node m1n" "$(grep -o 'check-exposure.sh" "\$WORK/ps-m1n.json" fx verifier-node m1n' "$HERE/m1node.sh" | sed 's/.*json" //')" 0
verdict "the compose file defines exactly the four services the scripts start" 0 "fx m1n m1node-tests verifier-node" "$(awk '/^services:/{s=1;next} s && /^  [a-z0-9-]+:/{gsub(/[ :]/,""); print}' "$HARNESS/docker-compose.m1node.yml" | sort | tr '\n' ' ' | sed 's/ $//')" 0
verdict "the fixture's compose service publishes no port (no ports: key in the file)" 0 "no ports" "$(grep -qE '^[[:space:]]+ports:' "$HARNESS/docker-compose.m1node.yml" && echo "has ports" || echo "no ports")" 0
if command -v ruby >/dev/null 2>&1; then
  verdict "node-probe.rb is valid Ruby" 0 "Syntax OK" "$(ruby -c "$HERE/node-probe.rb" 2>&1)" 0
else
  echo "  NOT CHECKED: the Ruby syntax of node-probe.rb (no ruby on this host)"
fi

# ── the outage sub-stage (M1N-4b) and the floor, against stubs ──
# What is stubbed: docker compose (DC), docker, sleep. Every scenario is a few assignments that the stub reads: PH_RCn / PH_PASSn (the exit status of phase n and whether its log has a PASS line),
# STOP_RC, NO_CONTAINER, RUNNING (State.Running after the stop), M1_GONE (1: m1n's health endpoint no longer answers), FX_UP (the control: the fixture answers the same probe), UP_RC, READY,
# NOAUDIT, AUDIT, PROMO (1: the node promotes itself; 0: it stays provisioning), STALE (1: the promoting audit row is older than the registration), NOID (1: the node names no promoting audit row). Each reject case asserts the sentence the stage dies with.
outage_run() { # label function scenario
  W="$T/o-$1"; mkdir -p "$W"
  ( set -u; WORK="$W"; PLATFORM_DIR=/x; . "$T/env.sh"; . "$HERE/m1node.sh"
    M1N_NODE_ID=harness-node-1; M1N_READY_SECS=3
    PH_RC1=0; PH_RC2=0; PH_RC3=0; PH_PASS1=1; PH_PASS2=1; PH_PASS3=1; STOP_RC=0; NO_CONTAINER=0; RUNNING=false; M1_GONE=1; FX_UP=1; UP_RC=0; READY=1; NOAUDIT=0; AUDIT=green; PROMO=1; STALE=0; NOID=0
    eval "${3:-}"
    DC() {
      case "$1" in
        run)
          local n=0 url name rc pass
          case "$*" in *OutagePhase1Start*) n=1;; *OutagePhase2Down*) n=2;; *OutagePhase3Resume*) n=3;; esac
          if [ "$n" != 0 ]; then
            case "$*" in *"-e HARNESS_M1_OUTAGE=1 "*) ;; *) echo "stub: the guard variable HARNESS_M1_OUTAGE=1 was not passed"; return 9;; esac
            case "$*" in *"-test.run ^TestM1Node_OutagePhase${n}"*'$ '*) ;; *) echo "stub: the phase was not selected by an anchored name"; return 9;; esac
            eval "rc=\$PH_RC$n; pass=\$PH_PASS$n"
            name="$(printf '%s\n' "${M1N_OUTAGE_TESTS[@]}" | sed -n "${n}p")"
            echo "    m1_node_outage_test.go:1: a log line of phase $n"
            [ "$pass" = 1 ] && [ "$rc" = 0 ] && echo "--- PASS: $name (0.01s)"
            [ "$rc" != 0 ] && echo "--- FAIL: $name (0.01s)"
            return "$rc"
          fi
          url="${@: -1}"
          case "$url" in
            http://fx:8025/*) [ "$FX_UP" = 1 ] && { echo '{"seq":1}'; return 0; }; return 1;;
            http://m1n:8080/healthz) [ "$M1_GONE" = 1 ] && return 1; echo '{"status":"ok"}'; return 0;;
            http://m1n:8080/readyz) [ "$READY" = 1 ] && echo '{"ready":true,"status":"ok"}' || echo '{"ready":false}'; return 0;;
          esac
          return 1;;
        stop) return "$STOP_RC";;
        ps) [ "$NO_CONTAINER" = 1 ] || echo cid123;;
        up) return "$UP_RC";;
        logs) echo "a log line of m1n";;
        exec)
          local sql="${@: -1}"
          case "$sql" in
            *"select status || '|' || coalesce(port25_open"*) [ "$PROMO" = 1 ] && echo "active|true" || echo "provisioning|false";;
            *"select 'node row: status="*) [ "$PROMO" = 1 ] && echo "node row: status=active port25_open=true audit_status=green promoted_by_audit_id=audit-9 registered_at=x" || echo "node row: status=provisioning port25_open=false audit_status=green promoted_by_audit_id=null registered_at=x";;
            *"a.audited_at >= n.registered_at"*) [ "$STALE" = 1 ] && echo false || echo true;;
            *"select audit_status from verify.node_audits where id"*) echo green;;
            *"select coalesce(promoted_by_audit_id"*) [ "$NOID" = 1 ] && echo "" || echo audit-9;;
            *"select id::text from verify.node_audits"*) echo audit-9;;
            *"from verify.node_audits"*)
              if [ ! -f "$W/audits" ]; then echo 3 > "$W/audits"; echo 3
              elif [ "$NOAUDIT" = 1 ]; then echo 3; else echo 4; fi;;
            *"coalesce(audit_status"*) echo "$AUDIT";;
          esac;;
      esac
    }
    docker() { [ "$1" = inspect ] && echo "$RUNNING"; }
    sleep() { :; }
    eval "$2" ) 2>&1
}
out="$(outage_run p1ok 'm1n_outage_phase 1 "${M1N_OUTAGE_TESTS[0]}"')"; rc=$?
verdict "m1n_outage_phase: a phase that passes (the guard variable is passed and the name is anchored)" 0 "outage phase 1 (TestM1Node_OutagePhase1Start): PASS" "$out" "$rc"
out="$(outage_run p2fail 'm1n_outage_phase 2 "${M1N_OUTAGE_TESTS[1]}"' 'PH_RC2=1')"; rc=$?
verdict "m1n_outage_phase: a phase that fails is red, naming the phase" 1 "outage phase 2 (TestM1Node_OutagePhase2Down) failed (exit 1)" "$out" "$rc"
out="$(outage_run p3skip 'm1n_outage_phase 3 "${M1N_OUTAGE_TESTS[2]}"' 'PH_PASS3=0')"; rc=$?
verdict "m1n_outage_phase: a phase that exits 0 without its PASS line (skipped, or it did not run) is red" 1 "outage phase 3 (TestM1Node_OutagePhase3Resume) has no '--- PASS' line in its log" "$out" "$rc"

out="$(outage_run stopok m1n_outage_stop)"; rc=$?
verdict "m1n_outage_stop: a module that is really gone" 0 "m1n is stopped: its container is not running, its health endpoint no longer answers" "$out" "$rc"
out="$(outage_run stopfail m1n_outage_stop 'STOP_RC=1')"; rc=$?
verdict "m1n_outage_stop: docker compose cannot stop it" 1 "docker compose could not stop m1n" "$out" "$rc"
out="$(outage_run nocont m1n_outage_stop 'NO_CONTAINER=1')"; rc=$?
verdict "m1n_outage_stop: no container to look at" 1 "m1n has no container after the stop" "$out" "$rc"
out="$(outage_run running m1n_outage_stop 'RUNNING=true')"; rc=$?
verdict "m1n_outage_stop: a stop that did not stop (the container is still running)" 1 "m1n is still running after docker compose stop (State.Running=true)" "$out" "$rc"
out="$(outage_run answers m1n_outage_stop 'M1_GONE=0')"; rc=$?
verdict "m1n_outage_stop: a stopped container whose health endpoint still answers" 1 "m1n's health endpoint still answers on the private network after the stop" "$out" "$rc"
out="$(outage_run fxdown m1n_outage_stop 'FX_UP=0')"; rc=$?
verdict "m1n_outage_stop: the control: a probe that cannot reach the fixture proves nothing about m1n" 1 "the probe itself does not work" "$out" "$rc"

out="$(outage_run startok m1n_outage_start)"; rc=$?
verdict "m1n_outage_start: a module that comes back, is ready, re-audits and is promoted" 0 "the restarted control plane audited the node again at boot (3 -> 4 audit rows)" "$out" "$rc"
verdict "m1n_outage_start: ... and says it is ready over the private network" 0 "m1n is back: /readyz answers ready=true over the private network" "$out" "$rc"
verdict "m1n_outage_start: ... and shows the node promoted by itself, naming the audit row" 0 "promoted by the green audit row audit-9" "$out" "$rc"
verdict "m1n_outage_start: ... and the harness ran no UPDATE (the stub answers no such statement)" 0 "no UPDATE was run by the harness" "$out" "$rc"
out="$(outage_run upfail m1n_outage_start 'UP_RC=1')"; rc=$?
verdict "m1n_outage_start: a start that never becomes healthy dies with the module's log" 1 "m1n did not become healthy again after the restart (its log tail is above)" "$out" "$rc"
verdict "m1n_outage_start: ... and prints the log tail before it dies" 1 "a log line of m1n" "$out" "$rc"
out="$(outage_run notready m1n_outage_start 'READY=0')"; rc=$?
verdict "m1n_outage_start: healthy but never ready (ready=false) is red after the bounded wait" 1 "m1n did not answer /readyz with ready=true within 3 s of the restart" "$out" "$rc"
out="$(outage_run noaudit m1n_outage_start 'NOAUDIT=1')"; rc=$?
verdict "m1n_outage_start: no new audit row is a NOTE, not a failure (the node keeps its last green audit)" 0 "NOTE: no new audit row within 30 s of the restart (3 rows)" "$out" "$rc"
out="$(outage_run red m1n_outage_start 'AUDIT=red')"; rc=$?
verdict "m1n_outage_start: a node whose audit is not green is not put back in service" 1 "the node's audit status is 'red', not green" "$out" "$rc"
out="$(outage_run nopromo m1n_outage_start 'PROMO=0')"; rc=$?
verdict "m1n_outage_start: a node that never promotes itself is red (the harness does not do it for it)" 1 "the node did not promote itself to active with port25_open=true within 90 s after the restart" "$out" "$rc"
out="$(outage_run stale m1n_outage_start 'STALE=1')"; rc=$?
verdict "m1n_outage_start: a node promoted by an audit row older than its registration is red (a stale active is never trusted)" 1 "is older than the node's registration after the restart" "$out" "$rc"
out="$(outage_run noid m1n_outage_start 'NOID=1')"; rc=$?
verdict "m1n_outage_start: a node that is active but names no promoting audit row is red" 1 "names no audit row" "$out" "$rc"

out="$(outage_run whole m1n_outage)"; rc=$?
verdict "m1n_outage: the whole sub-stage on a good run" 0 "M1N-4b OK in" "$out" "$rc"
order="$(printf '%s\n' "$out" | grep -oE 'outage phase [123] \(|m1n is stopped|m1n is back' | sed 's/ (//' | tr '\n' ',')"
verdict "m1n_outage: the order is phase 1, the stop, phase 2, the start, phase 3" 0 "outage phase 1,m1n is stopped,outage phase 2,m1n is back,outage phase 3," "$order" 0
out="$(outage_run whole1 m1n_outage 'PH_RC1=1')"; rc=$?
verdict "m1n_outage: when phase 1 fails the module is never stopped" 1 "outage phase 1 (TestM1Node_OutagePhase1Start) failed" "$out" "$rc" "m1n is stopped"
out="$(outage_run whole2 m1n_outage 'PH_RC2=1')"; rc=$?
verdict "m1n_outage: when phase 2 fails the module is never started again and phase 3 never runs" 1 "outage phase 2 (TestM1Node_OutagePhase2Down) failed" "$out" "$rc" "m1n is back"
out="$(outage_run whole3 m1n_outage 'RUNNING=true')"; rc=$?
verdict "m1n_outage: when the stop does not stop, phase 2 never runs" 1 "m1n is still running after docker compose stop" "$out" "$rc" "outage phase 2 ("

floor_run() { # label scenario
  W="$T/f-$1"; mkdir -p "$W"
  ( set -u; WORK="$W"; PLATFORM_DIR="$HARNESS/../.."; . "$T/env.sh"; . "$HERE/m1node.sh"
    DROP_ORD=0; MISSING_LOG=0; NOPASS_LOG=0; eval "${2:-}"
    : > "$W/m1n-list.log"; : > "$W/m1n-run.log"
    for t in "${M1N_TESTS[@]}" "${M1N_OUTAGE_TESTS[@]}"; do echo "$t" >> "$W/m1n-list.log"; done
    n=0; for t in "${M1N_TESTS[@]}"; do n=$((n+1)); [ "$DROP_ORD" = 1 ] && [ "$n" = 1 ] && continue; echo "--- PASS: $t (0.00s)" >> "$W/m1n-run.log"; done
    i=0; for t in "${M1N_OUTAGE_TESTS[@]}"; do
      i=$((i+1)); [ "$MISSING_LOG" = "$i" ] && continue
      if [ "$NOPASS_LOG" = "$i" ]; then echo "ran, printed no verdict" > "$W/m1n-outage-$i.log"; else printf -- '--- PASS: %s (0.00s)\nPASS\n' "$t" > "$W/m1n-outage-$i.log"; fi
    done
    m1n_floor ) 2>&1
}
want_n="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; echo $(( ${#M1N_TESTS[@]} + ${#M1N_OUTAGE_TESTS[@]} )) ) )"
out="$(floor_run good)"; rc=$?
verdict "m1n_floor: the ordinary tests and the three outage phases, all run, all counted" 0 "floor: $want_n TestM1Node tests declared, run and passed" "$out" "$rc"
out="$(floor_run nolog 'MISSING_LOG=2')"; rc=$?
verdict "m1n_floor: an outage phase whose log is missing is red" 1 "the log of outage phase 2 is missing" "$out" "$rc"
out="$(floor_run nopass 'NOPASS_LOG=3')"; rc=$?
verdict "m1n_floor: an outage phase with no PASS line is red" 1 "TestM1Node_OutagePhase3Resume has no '--- PASS' line in the run log" "$out" "$rc"
out="$(floor_run dropped 'DROP_ORD=1')"; rc=$?
verdict "m1n_floor: the floor bites: one ordinary test that did not run is red, declared against ran" 1 "declared=$want_n ran=$((want_n - 1))" "$out" "$rc"
verdict "m1n_floor: ... and the stage says which floor" 1 "the TestM1Node test-count floor ($want_n)" "$out" "$rc"

# the names, the exclusion and the guard agree between the Go files, the lists and the stage
phase_names="$(grep -ho '^func TestM1Node_OutagePhase[A-Za-z0-9_]*' "${srcs[@]}" | sed 's/^func //' | tr '\n' ' ')"
verdict "the outage phases carry their order in their names and are declared in that order" 0 "TestM1Node_OutagePhase1Start TestM1Node_OutagePhase2Down TestM1Node_OutagePhase3Resume" "$(printf '%s' "$phase_names" | sed 's/ $//')" 0
skip_re="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s' "$M1N_OUTAGE_SKIP" ) )"
ord_match="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s\n' "${M1N_TESTS[@]}" ) | grep -cE "$skip_re" || true)"
out_match="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s\n' "${M1N_OUTAGE_TESTS[@]}" ) | grep -cE "$skip_re" || true)"
verdict "the -test.skip pattern excludes all three outage phases from the ordinary run" 0 "3" "$out_match" 0
verdict "the -test.skip pattern excludes none of the ordinary tests (the pure outage test stays in the ordinary run)" 0 "0" "$ord_match" 0
verdict "stage M1N-4 passes the skip pattern to the ordinary run" 0 "-test.skip \"\$M1N_OUTAGE_SKIP\"" "$(grep -o -- '-test.skip "\$M1N_OUTAGE_SKIP"' "$HERE/m1node.sh" | head -1)" 0
verdict "the outage phases are run by an anchored name with the guard variable the Go tests require" 0 "HARNESS_M1_OUTAGE" "$(grep -q 'HARNESS_M1_OUTAGE=1 m1node-tests -test.run "^${name}' "$HERE/m1node.sh" && grep -q 'm1oEnvStage *= "HARNESS_M1_OUTAGE"' "$HARNESS/m1_node_outage_helpers_test.go" && echo HARNESS_M1_OUTAGE || echo "the stage and the Go tests disagree on the guard variable")" 0
verdict "m1n_run runs the stages in the order: tests, outage, floor, exposure, leak scan" 0 "m1n_stage4 m1n_outage m1n_floor m1n_exposure m1n_secret_leak" "$(sed -n '/^m1n_run() {/,/^}/p' "$HERE/m1node.sh" | grep -oE '^  m1n_[a-z0-9_]+' | tr -d ' ' | tr '\n' ' ' | grep -o 'm1n_stage4 m1n_outage m1n_floor m1n_exposure m1n_secret_leak')" 0
margin="$(sed -n 's/^ *TM_QUEUE_LEASE_MARGIN_SECONDS: "\([0-9]*\)".*/\1/p' "$HARNESS/docker-compose.m1node.yml")"
verdict "the control plane's lease margin is set low enough for the outage proof and above zero (the module refuses 0)" 0 "ok" "$([ -n "$margin" ] && [ "$margin" -gt 0 ] && [ "$margin" -le 30 ] && echo ok || echo "margin '${margin:-unset}'")" 0
cap_cp="$(sed -n 's/^ *TM_QUEUE_BULK_MAX_ADDRESSES: "\${H_M1N_BULK_CAP:-\([0-9]*\)}".*/\1/p' "$HARNESS/docker-compose.m1node.yml")"
cap_test="$(sed -n 's/^ *HARNESS_M1NODE_BULK_CAP: "\${H_M1N_BULK_CAP:-\([0-9]*\)}".*/\1/p' "$HARNESS/docker-compose.m1node.yml")"
cap_ok() { [ -n "$1" ] && [ "$1" = "$2" ] && [ "$1" -ge 300 ] && [ "$1" -le 5000 ]; }
verdict "the control plane's cap of rows per submission and the edge test's copy of it are one value, above the 220 rows of the pagination test and low enough to cross in one request" 0 "ok" "$(cap_ok "$cap_cp" "$cap_test" && echo ok || echo "control plane '${cap_cp:-unset}', test '${cap_test:-unset}'")" 0
verdict "the cap check bites: a cap below the pagination test's 220 rows, a cap the test does not share and an empty one are each refused (the control)" 0 "all three refused" "$(! cap_ok 100 100 && ! cap_ok 500 600 && ! cap_ok "" "" && echo "all three refused" || echo "one of them was accepted")" 0
verdict "the edge test reads the cap from the variable the compose file sets (and the compose file sets it for the test container)" 0 "HARNESS_M1NODE_BULK_CAP" "$(grep -q 'm1xEnvCap *= "HARNESS_M1NODE_BULK_CAP"' "$HARNESS/m1_node_contract_edge_test.go" && grep -q 'HARNESS_M1NODE_BULK_CAP:' "$HARNESS/docker-compose.m1node.yml" && echo HARNESS_M1NODE_BULK_CAP || echo "the test and the compose file disagree on the cap variable")" 0
first_zz="$(ls "$HARNESS"/*_test.go | xargs -n1 basename | grep -E '^(m1_node|zz_m1_node)' | sort | tail -1)"
verdict "the contract drift test is the last test file of module 1's tests (the edge test and every other m1_node test file sort before it)" 0 "zz_m1_node_contract_drift_test.go" "$first_zz" 0

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
