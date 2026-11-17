#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/m1node/m1node.sh go RED, for the reason they name, and stay green on a good input; and that the files of the module-1 node stack agree with
# each other (the list of tests, the recipe domains, the compose services). No Docker, no Go build, no Ruby needed: the functions are sourced and run against stubs. What it covers:
#   m1n_mint         makes the CA and the api.ipify.org certificate with openssl (the leaf verifies against the CA for that host name; the CA key is gone afterwards), writes exactly two lines to the env file,
#                    masks the node token BEFORE it is written, never prints it, writes two resolv.conf files with a nameserver and nothing that turns the node's audit red; refuses a prefix that is not three octets
#   m1n_secret_leak  green on clean logs; red, naming the CREDENTIAL (never printing its value), when any of the six credentials is in a log of the stack; red when a credential is empty (a scan with nothing to look for)
#   consistency      M1N_TESTS is exactly the set of TestM1Node_ functions in the m1_node*_test.go files (a test added without the list, or a listed test that does not exist, is red); every recipe domain the Go tests and
#                    the direct-node table use is in fixture.json; the exposure check names the stack's three services; the compose file defines exactly the four services the scripts start
# NOT covered here (they need a Linux host with Docker): m1n_stage1..4, m1n_exposure, node-probe.rb on a node (its syntax is checked with `ruby -c` when this host has Ruby, and said so when it has not).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=24; cases=0; ok=0; bad=0
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
srcs=("$HARNESS"/m1_node*_test.go)   # every file of the module-1 node tests: the tests, their helpers and the end-to-end tests of B9-B11
in_go="$(grep -ho '^func TestM1Node_[A-Za-z0-9_]*' "${srcs[@]}" | sed 's/^func //' | sort | tr '\n' ' ')"
in_sh="$( ( WORK=/x; . "$T/env.sh"; HERE="$HARNESS"; PLATFORM_DIR=/x; . "$HERE/m1node/m1node.sh"; printf '%s\n' "${M1N_TESTS[@]}" ) | sort | tr '\n' ' ')"
verdict "M1N_TESTS is exactly the set of TestM1Node_ functions of the m1_node*_test.go files" 0 "same" "$([ -n "$in_go" ] && [ "$in_go" = "$in_sh" ] && echo same || printf 'go: %s\nsh: %s' "$in_go" "$in_sh")" 0
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

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
