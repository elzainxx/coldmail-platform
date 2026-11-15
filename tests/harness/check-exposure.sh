#!/usr/bin/env bash
# check-exposure.sh <compose-ps-json-file> <expected-service>... — the modules (and everything else in the harness) must not be reachable from outside the
# Docker network. Reads `docker compose ps --all --format json` (one JSON object per line, or one array) and asserts:
#   1. EVERY expected service is present in the output (a service that vanished from the listing cannot be vouched for),
#   2. NO service anywhere in the stack publishes a port to the host (PublishedPort > 0).
# Subject floor: it must have examined at least as many services as were expected, and at least 1.
#
# BLIND SPOT, MEASURED (fault/p0-exposure, run 43729261466): a `ports:` entry on a service that sits only on an `internal` network is NOT active, so it never appears in
# `docker compose ps` Publishers — this check said "0 publish a port" for a stack that had one configured. run.sh therefore ALSO reads each container's configured
# HostConfig.PortBindings from `docker inspect`, and that second instrument is what went red. Neither is sufficient alone; both must pass.
#
# NOT CHECKED (printed on every run): reachability from ANOTHER HOST or through a firewall or cloud security group (the runner has none to test);
# traffic on the private network itself (the modules and the platform are meant to talk there); anything the containers do on their own outbound side.
# That the Docker network is `internal` is asserted separately by run.sh, from `docker network inspect`.
set -u
F="${1:?usage: check-exposure.sh <compose-ps-json-file> <expected-service>...}"; shift
[ $# -ge 1 ] || { echo "check-exposure: RED — no expected services given (the check would vouch for nothing)"; exit 1; }
[ -s "$F" ] || { echo "check-exposure: RED — $F is empty: the instrument saw no services"; exit 1; }
out="$(EXPECTED="$*" node -e '
const fs=require("fs");
const raw=fs.readFileSync(process.argv[1],"utf8").replace(/\r/g,"").trim();
let items=[];
try { const j=JSON.parse(raw); items=Array.isArray(j)?j:[j]; }
catch(e){ for(const line of raw.split("\n")){ if(!line.trim()) continue; try{ items.push(JSON.parse(line)); }catch(e2){ console.log("UNPARSEABLE "+line.slice(0,80)); } } }
const want=process.env.EXPECTED.split(/\s+/).filter(Boolean);
const seen=new Set(); const bad=[];
for(const it of items){
  const name=it.Service||it.Name||"?"; seen.add(name);
  for(const p of (it.Publishers||[])){ if((p.PublishedPort||0)>0) bad.push(name+" publishes "+p.PublishedPort+"->"+p.TargetPort+"/"+(p.Protocol||"tcp")+" on "+(p.URL||"?")); }
}
console.log("EXAMINED "+seen.size);
for(const w of want){ if(!seen.has(w)) console.log("MISSING "+w); }
for(const b of bad) console.log("PUBLISHED "+b);
' "$F" 2>&1)"
examined=$(printf '%s\n' "$out" | sed -n 's/^EXAMINED //p' | head -1); examined=${examined:-0}
fail=0
if [ "$examined" -lt 1 ] || [ "$examined" -lt "$#" ]; then echo "  FAIL: examined $examined services, expected at least $# — the instrument saw fewer services than it was told to vouch for"; fail=$((fail+1)); fi
for m in $(printf '%s\n' "$out" | sed -n 's/^MISSING //p'); do echo "  FAIL: expected service '$m' is not in the listing"; fail=$((fail+1)); done
printf '%s\n' "$out" | sed -n 's/^PUBLISHED /  FAIL: /p' | while read -r l; do echo "$l"; done
p=$(printf '%s\n' "$out" | grep -c '^PUBLISHED ' || true); fail=$((fail+p))
u=$(printf '%s\n' "$out" | grep -c '^UNPARSEABLE ' || true); [ "$u" -eq 0 ] || { echo "  FAIL: $u line(s) of the listing could not be parsed"; fail=$((fail+u)); }
echo "check-exposure: $examined services examined, $p publish a port to the host, failures: $fail"
echo "NOT CHECKED: a publish that is configured but inactive (a service on an internal network) — run.sh reads docker inspect PortBindings for that; reachability from another host, a firewall or a security group; the private network's own traffic; the containers' outbound side"
[ "$fail" -eq 0 ] || { echo "check-exposure: RED"; exit 1; }
echo "check-exposure: OK"
