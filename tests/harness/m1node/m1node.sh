#!/usr/bin/env bash
# m1node.sh — the module-1 node stages of the harness (Phase 3, chunk B7). SOURCED by tests/harness/run.sh (it uses run.sh's DC, die, banner, mask, WORK, HERE, PLATFORM_DIR and
# STAGE); it runs nothing by itself. What it adds, in the order the run needs it:
#
#   m1n_mint      (run.sh stage 2)  the node's token, the per-run CA and the api.ipify.org certificate (keys mode 600, never printed), the two resolv.conf files, the address prefix
#   m1n_run       the four stages below, each with its own banner and its own printed evidence, so ONE run says exactly which one broke:
#     M1N-1  m1n_stage1   the fixtures alone: DNS, the ipify HTTPS endpoint and the SMTP fixture on the second internal network, checked from a SEPARATE container on that network
#     M1N-2  m1n_stage2   the node alone: its resolver and hostname, its audit (each of the three steps, then the JSON), then POST /v1/verify DIRECTLY on the node for every recipe
#     M1N-3  m1n_stage3   module 1's control plane wired to the node: boot log, the first audit row, the promotion (SQL), a synchronous verify that shows a node verdict
#     M1N-4  m1n_stage4   the integration tests (tests/harness/m1_node_test.go, TestM1Node_*): one bulk job holding one address per recipe, the cache, a control per recipe family
#     M1N-4b m1n_outage   the OUTAGE proof (Phase 3, B12; tests/harness/m1_node_outage_test.go): a job in flight, the control plane stopped, the platform's calls and books while it is down, the
#                         control plane started again, the job completed exactly once; three test phases in three processes with the stop and the start between them, then m1n_floor counts all of them
#   m1n_floor     every TestM1Node test (the ordinary run and the three outage phases) ran, counted against the floor of this scope
#   m1n_exposure  no service of this stack publishes a port; the fixture network is internal
#
# DORMANT until proven: run.sh sets M1N_ON=1, merges docker-compose.m1node.yml, calls m1n_mint and runs these stages ONLY under H_SCOPE=m1node, or in the full run once `m1node` is named in
# FULL_INCLUDES (the lead adds it after a runner run of scope m1node is green). Otherwise nothing here runs, no container of this stack exists, and the tests (tests/harness/m1_node_test.go,
# build tag m1node) are not compiled into the harness binary. H_SCOPE=m1node runs only what this stack needs (postgres, the migrations, this stack) and exits.
# What it does NOT check is printed by m1n_not_checked. Read tests/harness/m1node/FIRST-RUN.md for what each printed line means.

M1N_DIR="$HERE/m1node"
M1N_NODE_ID="harness-node-1"
M1N_NET="coldmail-platform-harness_fixtures"
M1N_PREFIX=""        # the three-octet prefix of the fixture network, set by m1n_mint
# the tests of this stack, by name: stage M1N-4 requires each to be declared by the binary and to have a `--- PASS` line, and the floor of this scope is their number (like m2worker, M2W_TESTS)
M1N_TESTS=(
  TestM1Node_Preconditions
  TestM1Node_BulkRecipeTable
  TestM1Node_CacheHit
  TestM1Node_ControlRCPTFamily
  TestM1Node_ControlCatchAllFamily
  TestM1Node_ControlTransportFamily
  TestM1Node_ControlDNSFamily
  TestM1Node_ControlListsFamily
  TestM1Node_ExpectationsBite
  TestM1Node_E2EBulkSettleThroughAdapter
  TestM1Node_E2EPaginationBoundaries
  TestM1Node_E2ECrossTenant
  TestM1Node_E2EChecksBite
  TestM1Node_OutageChecksBite
  TestM1Node_ZContractDrift
)
# the three phases of the outage proof: each runs in a process of its own, in this order, between the stop and the start of the control plane (m1n_outage). The ordinary run of stage M1N-4
# excludes them by name (M1N_OUTAGE_SKIP, -test.skip): a phase needs the state of the one before and a module that is up or down at the right moment. Their PASS lines are required by name
# and they count towards the floor (m1n_floor).
M1N_OUTAGE_TESTS=(
  TestM1Node_OutagePhase1Start
  TestM1Node_OutagePhase2Down
  TestM1Node_OutagePhase3Resume
)
M1N_OUTAGE_SKIP='^TestM1Node_OutagePhase'
M1N_READY_SECS=90        # how long m1n_outage waits for the restarted control plane to answer /readyz with ready=true over the private network
M1N_NODE_TOKEN=""
M1N_T0=$SECONDS

m1n_ip() { printf '%s.%s' "$M1N_PREFIX" "$1"; }                 # an address of the fixture network: 2 = the fixture, 3 = the node
m1n_psql() { DC exec -T postgres psql -X -v ON_ERROR_STOP=1 -U supabase_admin -d postgres -tAc "$1"; }
m1n_fx() { DC exec -T fx wget -q -O - "http://127.0.0.1:8025$1"; }   # a GET on the fixture's control API from inside the fixture (busybox wget)
m1n_show() { sed 's/^/     | /'; }                              # indent what a step printed
m1n_step() { echo "  [$1] $2"; }
m1n_nodeprobe() { DC exec -T verifier-node ruby - "$@" < "$M1N_DIR/node-probe.rb"; }

# ── run.sh stage 2: the secrets and files of this stack ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The node's token (masked at once), a CA made for this run and one certificate for api.ipify.org signed by it (the CA key is deleted after signing; the certificate key is mode
# 600 and owned by the harness user, which the fixture container runs as), the resolv.conf of the node and of module 1's second control plane, and the variables compose reads.
m1n_mint() {
  M1N_PREFIX="${H_FX_PREFIX:-100.128.0}"
  [[ "$M1N_PREFIX" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || die "H_FX_PREFIX='$M1N_PREFIX' is not a three-octet prefix (the fixture network is <prefix>.0/24)"
  local t="$WORK/m1fx" tok
  tok="$(openssl rand -hex 24)"; [ -n "$tok" ] || die "could not mint the verifier node's token"
  mask "$tok"; M1N_NODE_TOKEN="$tok"
  rm -rf "$t"; mkdir -p "$t/tls"
  cat > "$t/openssl.cnf" <<'EOF'
[req]
distinguished_name = dn
prompt = no
[dn]
CN = harness fixture CA
[ca_ext]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
[leaf_ext]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:api.ipify.org
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
EOF
  (
    umask 077
    cd "$t" || exit 1
    export MSYS_NO_PATHCONV=1      # a no-op on the runner; keeps Git Bash for Windows from turning "/CN=..." into a path when this is rehearsed on a workstation (relative paths below, for the same reason)
    openssl ecparam -name prime256v1 -genkey -noout -out ca.key 2>/dev/null &&
    openssl req -new -x509 -key ca.key -sha256 -days 2 -config openssl.cnf -extensions ca_ext -subj "/CN=harness fixture CA" -out ca.pem 2>/dev/null &&
    openssl ecparam -name prime256v1 -genkey -noout -out tls/ipify.key 2>/dev/null &&
    openssl req -new -key tls/ipify.key -sha256 -subj "/CN=api.ipify.org" -out leaf.csr 2>/dev/null &&
    openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -sha256 -days 2 -extfile openssl.cnf -extensions leaf_ext -out tls/ipify.crt 2>/dev/null
  ) || die "could not make the run's CA and the api.ipify.org certificate with openssl"
  rm -f "$t/ca.key" "$t/leaf.csr" "$t/ca.srl" "$t/openssl.cnf"      # the CA key never outlives the signing
  chmod 644 "$t/ca.pem" "$t/tls/ipify.crt"; chmod 600 "$t/tls/ipify.key"; chmod 755 "$t/tls"
  openssl verify -CAfile "$t/ca.pem" -verify_hostname api.ipify.org "$t/tls/ipify.crt" >/dev/null 2>&1 || die "the freshly made api.ipify.org certificate does not verify against the freshly made CA"
  # glibc (the audit's IP lookup) and Ruby's resolver read this; module 1's pure-Go resolver reads the other. No search line, no domain line, no ndots: the node's own hygiene check
  # (resolver_hygiene.rb) goes red on any of them. timeout:2 attempts:1 makes a silent zone settle in 2 s instead of 10.
  printf 'nameserver %s.2\noptions timeout:2 attempts:1\n' "$M1N_PREFIX" > "$t/node-resolv.conf"
  cp "$t/node-resolv.conf" "$t/cp-resolv.conf"; chmod 644 "$t/node-resolv.conf" "$t/cp-resolv.conf"
  ( umask 077; printf 'H_M1_NODE_TOKEN=%s\nH_FX_PREFIX=%s\n' "$tok" "$M1N_PREFIX" >> "$WORK/harness.env" )
  echo "  minted for the module-1 node stack: 1 node token, a CA and an api.ipify.org certificate valid 2 days (CA key deleted; certificate key mode 600), 2 resolv.conf files; fixture network prefix $M1N_PREFIX — none of the secrets printed"
}

# ── M1N-1: the fixtures alone ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
m1n_stage1() {
  STAGE=m1n-fixtures; banner "M1N-1" "the fixtures alone: DNS, the ipify HTTPS endpoint, the SMTP fixture, on the second internal network ($M1N_PREFIX.0/24, public to the node's destination guard)"
  local t0=$SECONDS
  DC up -d --wait --wait-timeout 120 fx || die "the fixture container (fx) did not become healthy"
  m1n_step M1N-1.1 "the fixture's own startup line (it must say uid and the ports it bound: a non-root user binding 53 and 443 is unproven until this line, [U8])"
  DC logs --no-color fx 2>&1 | grep -E 'fx: ' | head -6 | m1n_show
  DC logs --no-color fx 2>&1 | grep -q 'fx: ready: roles=dns,https,smtp,api ' || die "the fixture never printed its ready line"
  m1n_step M1N-1.2 "the fixture network as Docker made it ([U4]: the /24 is accepted, the fixture sits on it)"
  docker network inspect "$M1N_NET" -f 'internal={{.Internal}} subnet={{range .IPAM.Config}}{{.Subnet}} ip_range={{.IPRange}}{{end}}' | m1n_show
  [ "$(docker network inspect "$M1N_NET" -f '{{.Internal}}')" = true ] || die "$M1N_NET is not an internal network"
  docker network inspect "$M1N_NET" -f '{{range .Containers}}{{.Name}} {{.IPv4Address}}{{"\n"}}{{end}}' | m1n_show
  docker network inspect "$M1N_NET" -f '{{range .Containers}}{{.IPv4Address}}{{"\n"}}{{end}}' | grep -q "^$(m1n_ip 2)/24\$" || die "the fixture is not at $(m1n_ip 2) on $M1N_NET"
  m1n_step M1N-1.3 "fx selfcheck, run from a SEPARATE container on the fixture network: every record, mode, negative answer, the dotless forwarder, the HTTPS chain and every SMTP rule (one line each)"
  local img rc
  img="$(docker inspect -f '{{.Config.Image}}' "$(DC ps -q fx)")" && [ -n "$img" ] || die "could not read the fixture's image name"
  docker run --rm --network "$M1N_NET" \
    -e FX_CONFIG=/fx/fixture.json -e FX_SELF_IP="$(m1n_ip 2)" -e FX_NODE_IP="$(m1n_ip 3)" -e FX_CHECK_CA=/fx/ca.pem -e FX_CHECK_FORWARD=1 \
    -v "$M1N_DIR/fixture.json:/fx/fixture.json:ro" -v "$WORK/m1fx/ca.pem:/fx/ca.pem:ro" \
    "$img" selfcheck > "$WORK/m1n-selfcheck.log" 2>&1; rc=$?
  m1n_show < "$WORK/m1n-selfcheck.log"
  [ "$rc" = 0 ] || die "fx selfcheck failed ($(grep -c '\[FAIL\]' "$WORK/m1n-selfcheck.log") failing check(s), listed above): the fixtures are wrong before the node or module 1 is involved"
  m1n_step M1N-1.4 "what the fixture says it served so far (control API /api/state)"
  m1n_fx /api/state | m1n_show
  echo "  M1N-1 OK in $((SECONDS - t0)) s: the fixtures answer exactly what fixture.json says, over the network, from another container"
}

# ── M1N-2: the node alone ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The recipes that reach the node: name, address, and the substrings its SUMMARY line must contain. (The cheap recipes never reach it: no MX, null MX, the lists, a provider MX.)
M1N_DIRECT=(
  "valid|probe@valid.fixture.test|success=true;rcpt=\"250;catchall=not_catchall"
  "mailbox_full|probe@full.fixture.test|success=false;rcpt=\"552;catchall=not_catchall"
  "mailbox_not_found|probe@gone.fixture.test|success=false;rcpt=\"550;catchall=not_catchall"
  "catch_all|probe@catchall.fixture.test|success=true;catchall=catchall"
  "greylisted|probe@grey.fixture.test|success=false;rcpt=\"450;catchall=inconclusive"
  "smtp_blocked|probe@blocked.fixture.test|success=false;rcpt=\"554;catchall=not_catchall"
  "smtp_error_remote|probe@ambiguous.fixture.test|success=false;rcpt=\"550 5.7.0;catchall=not_catchall"
  "smtp_timeout|probe@stall.fixture.test|success=false;rcpt=\"server response timeout;catchall=not_catchall"
  "smtp_error_dropped|probe@dropped.fixture.test|success=false;rcpt=\"server dropped connection after response;catchall=not_catchall"
  "connection_failed|probe@noconn.fixture.test|success=false;port_opened=false;catchall=inconclusive"
  "non_public_mx|probe@nonpublic.fixture.test|probe_refused=true;rcpt=\"not-attempted\""
)

m1n_stage2() {
  STAGE=m1n-node; banner "M1N-2" "the node alone: module 1's real verifier node (truemail 3.3.1 + the fork's wrapper) against the fixtures, no control plane yet"
  local t0=$SECONDS out
  DC up -d --wait --wait-timeout 240 verifier-node || die "the verifier node did not become healthy (its image is built from module 1's own Dockerfile; its log is below)"
  m1n_step M1N-2.1 "resolver and hostname as the node sees them ([U2]: the bind-mounted resolv.conf is honoured; hygiene: no search/domain line, ndots <= 1, hostname without a dot)"
  out="$(m1n_nodeprobe hygiene)" || die "the node probe could not run inside the node container"
  printf '%s\n' "$out" | m1n_show
  printf '%s\n' "$out" | grep -qE '^HYGIENE   \| *(search|domain)[[:space:]]' && die "the node's resolv.conf has a search or domain line: the audit will be red (docker rewrote the mount, or the mount did not take)"
  printf '%s\n' "$out" | grep -q '^HYGIENE   | nameserver '"$(m1n_ip 2)"'$' || die "the node's resolv.conf does not name the fixture ($(m1n_ip 2)) as its nameserver: the bind mount over /etc/resolv.conf did not take"
  printf '%s\n' "$out" | grep -q 'hostname="[^".]*"' || die "the node's hostname contains a dot: the audit's resolver hygiene check goes red"
  printf '%s\n' "$out" | grep -q 'PROBE_ALLOW_NON_PUBLIC=nil' || die "PROBE_ALLOW_NON_PUBLIC is set in the node: the destination guard would be off and the audit red"
  m1n_step M1N-2.2 "the three steps of the gem's host audit, one by one ([U3]: SSL_CERT_FILE honoured by Ruby, the OS resolver uses the mounted resolv.conf)"
  out="$(m1n_nodeprobe steps)" || die "the node probe could not run its steps"
  printf '%s\n' "$out" | m1n_show
  printf '%s\n' "$out" | grep -q '^STEP ipify ok=true body="'"$(m1n_ip 3)"'" ' || die "step 1 (the HTTPS 'what is my IP' answer) did not give the node's own address $(m1n_ip 3): TLS trust (SSL_CERT_FILE), the OS resolver, or the fixture's HTTPS role"
  printf '%s\n' "$out" | grep -q '^STEP a_record ok=true ' || die "step 2 (the A record of the verifier domain must equal the address of step 1) failed"
  printf '%s\n' "$out" | grep -q '^STEP ptr ok=true ' || die "step 3 (the PTR of that address must name the verifier domain) failed"
  m1n_step M1N-2.3 "GET /v1/audit on the node: the audit JSON (green means warnings is empty)"
  out="$(m1n_nodeprobe audit)" || die "the node probe could not run the audit"
  printf '%s\n' "$out" | m1n_show
  printf '%s\n' "$out" | grep -q '^AUDIT http=200' || die "the node's /v1/audit did not answer 200"
  printf '%s\n' "$out" | grep -q '"warnings":{}' || die "the node's audit has warnings (printed above): the keys name the failing step (ip, dns, ptr, resolver, probe_guard)"
  printf '%s\n' "$out" | grep -q '"audit_status":"green"' || die "the node's audit is not green"
  printf '%s\n' "$out" | grep -q '"current_host_ip":"'"$(m1n_ip 3)"'"' || die "the audit's current_host_ip is not $(m1n_ip 3): module 1 requires it to equal the egress IP it is configured with"
  m1n_step M1N-2.4 "POST /v1/verify DIRECTLY on the node for every recipe that reaches it: the node's own answer, then what it must contain"
  local bad=0 row name addr want sum http w rest
  for row in "${M1N_DIRECT[@]}"; do
    name="${row%%|*}"; rest="${row#*|}"; addr="${rest%%|*}"; want="${rest#*|}"
    out="$(m1n_nodeprobe verify "$addr" 2>&1)" || { echo "     ! $name: the probe call itself failed: $out"; bad=$((bad+1)); continue; }
    http="$(printf '%s\n' "$out" | sed -n 's/^VERIFY http=\([0-9]*\) .*/\1/p')"; sum="$(printf '%s\n' "$out" | sed -n 's/^SUMMARY //p')"
    echo "     == $name ($addr): $(printf '%s\n' "$out" | grep '^VERIFY ')"
    echo "        SUMMARY $sum"
    printf '%s\n' "$out" | grep '^RAW ' | cut -c1-1800 | sed 's/^/        /'
    local missing=""
    IFS=';' read -ra ws <<< "$want"
    for w in "${ws[@]}"; do printf '%s' "$sum" | grep -qF -- "$w" || missing="$missing [$w]"; done
    [ "$http" = 200 ] || missing="$missing [HTTP 200]"
    if [ -n "$missing" ]; then echo "        MISMATCH: the summary lacks$missing"; bad=$((bad+1)); fi
  done
  [ "$bad" = 0 ] || die "$bad recipe(s) of the node alone answered other than expected (every answer is printed above): the fixture, the gem's conversation or an expectation of this table is wrong"
  m1n_step M1N-2.5 "what the SMTP fixture saw from the node (session log: the source must be the node, and each recipe makes two RCPTs: the target and the nx- probe)"
  m1n_fx '/api/smtp/sessions' | tr '}' '\n' | grep -oE '"remote":"[^"]*"|"rcpt":"[^"]*"|"rule":"[^"]*"|"action":"[^"]*"' | paste - - - - | sed 's/\t/  /g' | sed -n '1,40p' | m1n_show
  m1n_fx '/api/smtp/sessions' | grep -q '"remote":"'"$(m1n_ip 3)"'"' || die "the SMTP fixture saw no session from the node ($(m1n_ip 3)): the sessions did not take the path the guard allows"
  echo "  M1N-2 OK in $((SECONDS - t0)) s: the node's audit is green and it answers every recipe as the table says"
}

# ── M1N-3: module 1's control plane wired ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
m1n_stage3() {
  STAGE=m1n-wired; banner "M1N-3" "module 1's control plane wired to the node: boot, first audit row, promotion, a synchronous verify with a node verdict"
  local t0=$SECONDS out i a
  m1n_step M1N-3.1 "the lists the cheap recipes need (module 1 reads them per request), and exactly ONE control plane draining the queue"
  m1n_psql "INSERT INTO verify.disposable_domains (domain, source, added_at) VALUES ('disposable.fixture.test', 'harness m1node', now()) ON CONFLICT DO NOTHING" >/dev/null || die "could not seed verify.disposable_domains"
  m1n_psql "INSERT INTO verify.spam_trap_domains (domain, source, added_at) VALUES ('trap.fixture.test', 'harness m1node', now()) ON CONFLICT DO NOTHING" >/dev/null || die "could not seed verify.spam_trap_domains"
  DC stop m1 >/dev/null 2>&1 || true
  m1n_step M1N-3.2 "start module 1's wired control plane (it fetches the node's /v1/config ONCE at boot: the node was healthy first)"
  DC up -d --wait --wait-timeout 240 m1n || die "the wired control plane (m1n) did not become healthy"
  DC logs --no-color m1n 2>&1 | grep -E 'verify: (SMTP node=|orchestrator wired|WARNING|/v1/config|provider-routing|no )' | cut -c1-260 | m1n_show
  DC logs --no-color m1n 2>&1 | grep -q 'verify: SMTP node='"$M1N_NODE_ID"' config_fingerprint=' || die "the control plane did not fetch the node's /v1/config at boot (it stays unwired for its whole life: see its log above)"
  DC logs --no-color m1n 2>&1 | grep -q 'verify: orchestrator wired (SMTP-enabled' || die "the control plane's orchestrator is not SMTP-enabled"
  m1n_step M1N-3.3 "the first audit row (the boot tick writes it; with TM_WATCHER_CADENCE_SECONDS=0 there is no other)"
  i=0; a=""
  while [ $i -lt 60 ]; do a="$(m1n_psql "select coalesce(audit_status, '') from verify.nodes where id = '$M1N_NODE_ID'")"; [ -n "$a" ] && break; i=$((i+1)); sleep 1; done
  m1n_psql "select 'node row: id=' || id || ' status=' || status || ' port25_open=' || coalesce(port25_open::text, 'null') || ' audit_status=' || coalesce(audit_status, '') || ' base_url_set=' || (base_url <> '')::text from verify.nodes where id = '$M1N_NODE_ID'" | m1n_show
  m1n_psql "select 'latest audit: ' || audit_status || ' current_host_ip=' || coalesce(host(current_host_ip), '') || ' warnings=' || warnings::text from verify.node_audits where node_id = '$M1N_NODE_ID' order by audited_at desc limit 1" | m1n_show
  [ "$a" = green ] || die "the node's first audit row is '${a:-missing}', not green (a red boot audit quarantines the node for good with the watcher loop off): the audit row and the node's own audit are printed above and in M1N-2"
  m1n_step M1N-3.4 "the promotion: nothing in module 1's code moves a node from provisioning to active or sets port25_open ([U6]); the harness does it in SQL, after the final start"
  [ "$(m1n_psql "select status from verify.nodes where id = '$M1N_NODE_ID'")" = provisioning ] || echo "     NOTE: the node was not 'provisioning' after boot ([U6] differs from the research): $(m1n_psql "select status from verify.nodes where id = '$M1N_NODE_ID'")"
  [ "$(m1n_psql "with u as (update verify.nodes set status = 'active', port25_open = true where id = '$M1N_NODE_ID' and audit_status = 'green' returning 1) select count(*) from u")" = 1 ] || die "the promotion UPDATE changed no row"
  m1n_psql "select 'node row now: status=' || status || ' port25_open=' || port25_open::text from verify.nodes where id = '$M1N_NODE_ID'" | m1n_show
  m1n_step M1N-3.5 "a synchronous POST /v1/verify through module 1 for an address nobody else uses: it must carry a node verdict (meta.node_id, meta.verifier_ip), not smtp_unwired / no_eligible_node"
  out="$(printf '{"email":"gate-sync@valid.fixture.test"}' | DC exec -T -e K="$M1_KEY" m1n sh -c 'curl -sS -m 40 -X POST -H "Authorization: Bearer $K" -H "Content-Type: application/json" --data-binary @- http://localhost:8080/v1/verify')" || die "the synchronous verify call failed"
  printf '%s\n' "$out" | cut -c1-1500 | m1n_show
  printf '%s' "$out" | grep -q '"status":"valid"' && printf '%s' "$out" | grep -q '"reason":"accepted"' || die "the synchronous verdict is not valid/accepted (smtp_skipped_reason in the signals above says why the node was not used)"
  printf '%s' "$out" | grep -q '"node_id":"'"$M1N_NODE_ID"'"' || die "the verdict carries no meta.node_id=$M1N_NODE_ID: the node did not serve it"
  printf '%s' "$out" | grep -q '"verifier_ip":"'"$(m1n_ip 3)"'"' || die "the verdict's meta.verifier_ip is not the node's address $(m1n_ip 3)"
  m1n_step M1N-3.6 "the SMTP fixture's view of that one verification (the target and the nx- probe, both from the node)"
  m1n_fx '/api/smtp/sessions?domain=valid.fixture.test' | tr '}' '\n' | grep gate-sync | m1n_show
  [ "$(m1n_fx '/api/smtp/sessions?domain=valid.fixture.test' | grep -o 'gate-sync@valid.fixture.test' | wc -l | tr -d ' ')" = 1 ] || die "the SMTP fixture did not see exactly one RCPT for gate-sync@valid.fixture.test"
  echo "  M1N-3 OK in $((SECONDS - t0)) s: module 1 is wired, the node is active, and a synchronous verify returns a node verdict"
}

# ── M1N-4: the tests ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The tests of this stack are compiled behind the build tag m1node into a binary of their own (harness-m1node.test): the harness binary of the other stages never contains them, and this
# stage never needs the full harness environment (the m1node-tests service gets its own variables).
m1n_stage4() {
  STAGE=m1n-tests; banner "M1N-4" "the integration tests of this stack (TestM1Node_*): the recipe table in one bulk job, the cache, a control per recipe family"
  local t0=$SECONDS trc lrc t
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -tags "harness m1node" -c -o "$WORK/out/harness-m1node.test" ./tests/harness ) || die "the harness test binary (tags harness m1node) did not compile"
  DC run --rm -T m1node-tests -test.list '^TestM1Node_' > "$WORK/m1n-list.log" 2>&1; lrc=$?
  [ "$lrc" = 0 ] || { cat "$WORK/m1n-list.log"; die "listing the TestM1Node tests failed ($lrc)"; }
  for t in "${M1N_TESTS[@]}" "${M1N_OUTAGE_TESTS[@]}"; do grep -qx "$t" "$WORK/m1n-list.log" || die "$t is not among the declared tests of the binary"; done
  [ "$(grep -c '^TestM1Node_' "$WORK/m1n-list.log")" = "$(( ${#M1N_TESTS[@]} + ${#M1N_OUTAGE_TESTS[@]} ))" ] || die "the binary declares $(grep -c '^TestM1Node_' "$WORK/m1n-list.log") TestM1Node tests, m1node.sh names $(( ${#M1N_TESTS[@]} + ${#M1N_OUTAGE_TESTS[@]} )) (M1N_TESTS and M1N_OUTAGE_TESTS): a test was added or removed without the list"
  # the outage phases are NOT part of this run: each needs the state of the one before and the control plane up or down at the right moment (m1n_outage runs them, by name, and m1n_floor counts them)
  DC run --rm -T m1node-tests -test.run '^TestM1Node_' -test.skip "$M1N_OUTAGE_SKIP" -test.v -test.count=1 -test.timeout=20m > "$WORK/m1n-run.log" 2>&1; trc=$?
  echo "  ---- the tests' own log lines (the recipe table as module 1 reported it, the sessions the fixture saw, the controls) ----"
  grep -E " m1_node(_[a-z0-9]+)*_test.go:[0-9]+: " "$WORK/m1n-run.log" | cut -c1-420 | m1n_show | head -500
  echo "  ---- go test verdict lines ----"
  grep -E "^(--- (PASS|FAIL|SKIP)|PASS|FAIL|ok )" "$WORK/m1n-run.log" | m1n_show
  [ "$trc" = 0 ] || { echo "  ---- failing tests (with context) ----"; grep -n -B1 -A30 -e "--- FAIL" "$WORK/m1n-run.log" | cut -c1-700 | sed "s/^/  | /" | head -320; }
  [ "$trc" = 0 ] || { printf 'FAIL\t%s\t0.000s\n' "$GO_PKG" >> "$WORK/m1n-run.log"; die "the TestM1Node tests failed (exit $trc)"; }
  for t in "${M1N_TESTS[@]}"; do grep -q "^--- PASS: $t " "$WORK/m1n-run.log" || die "$t has no '--- PASS' line in the run log: it did not run, or did not pass"; done
  # the test-count floor of this scope (M1N_TESTS plus the outage phases) is checked by m1n_floor, after the outage sub-stage has added its three PASS lines to this log
  m1n_step M1N-4.9 "the fixture's counters after the whole run"
  m1n_fx /api/state | m1n_show
  echo "  M1N-4 OK in $((SECONDS - t0)) s"
}

# ── M1N-4b: the outage proof ────────────────────────────────────────────────────────────────────────────────────────────────
# Phase 3, chunk B12. Module 1's control plane (m1n) is stopped in the MIDDLE of a job and started again; the node, the fixtures and Postgres keep running. Three test phases
# (tests/harness/m1_node_outage_test.go), each a `go test` of its own in a fresh test container; the state between them is the scratch table harness_outage.m1 in the stack's database:
#   phase 1  a real tenant, a reserve, ONE job that cannot be finished a few seconds later, progress and "not complete" proved by the adapter's own Status
#   (stage)  stop m1n; prove it is stopped: its container is not running AND its health endpoint no longer answers on the private network (with a control: the same probe reaches the fixture)
#   phase 2  every call the platform polls or writes with fails typed and returns nothing, polling halts by its context, and neither the platform's books nor module 1's tables moved
#   (stage)  start m1n; wait for /readyz ready=true over the private network; wait for its audit at boot; promote the node again (every boot registers it as provisioning again)
#   phase 3  the job completes exactly once, every charged item is charged exactly once, the books close, a replay writes nothing
# The same flow with the module up is TestM1Node_E2EBulkSettleThroughAdapter (stage M1N-4): phase 3 runs the same comparisons.

# one phase: a go test of its own in a fresh test container, by name, with the variable that only this sub-stage sets (the phases fail without it)
m1n_outage_phase() { # number name
  local n="$1" name="$2" rc log="$WORK/m1n-outage-$1.log"
  DC run --rm -T -e HARNESS_M1_OUTAGE=1 m1node-tests -test.run "^${name}\$" -test.v -test.count=1 -test.timeout=10m > "$log" 2>&1; rc=$?
  grep -E " m1_node(_[a-z0-9]+)*_test.go:[0-9]+: " "$log" | cut -c1-420 | m1n_show | head -200
  grep -E "^(--- (PASS|FAIL|SKIP)|PASS|FAIL|ok )" "$log" | m1n_show
  if [ "$rc" != 0 ]; then
    echo "  ---- the failing phase (with context) ----"
    grep -n -B1 -A30 -e "--- FAIL" "$log" | cut -c1-700 | sed 's/^/  | /' | head -200
    die "outage phase $n ($name) failed (exit $rc): its log is above"
  fi
  grep -q "^--- PASS: $name " "$log" || die "outage phase $n ($name) has no '--- PASS' line in its log: it did not run, or it was skipped"
  echo "  outage phase $n ($name): PASS"
}

# a GET from a throwaway container on the private network (busybox wget): succeeds, printing the body, when the URL answers
m1n_outage_probe() { DC run --rm -T --entrypoint wget m1node-tests -q -T 3 -O - "$1" 2>/dev/null; }

m1n_outage_stop() {
  local id st
  DC stop m1n >/dev/null 2>&1 || die "docker compose could not stop m1n: the outage cannot start"
  id="$(DC ps -aq m1n)"
  [ -n "$id" ] || die "m1n has no container after the stop: the stack is not what the outage sub-stage expects"
  st="$(docker inspect -f '{{.State.Running}}' "$id")"
  [ "$st" = false ] || die "m1n is still running after docker compose stop (State.Running=$st): an outage proof against a running module would prove nothing"
  m1n_outage_probe http://fx:8025/api/state >/dev/null || die "the probe from the private network cannot reach the fixture (fx) either: a refused connection to m1n would prove nothing, the probe itself does not work"
  if m1n_outage_probe http://m1n:8080/healthz >/dev/null; then die "m1n's health endpoint still answers on the private network after the stop: the module is not gone"; fi
  echo "  m1n is stopped: its container is not running, its health endpoint no longer answers on the private network, and the same probe does reach the fixture (the control)"
}

m1n_outage_start() {
  local n0 n1 i body a
  n0="$(m1n_psql "select count(*) from verify.node_audits where node_id = '$M1N_NODE_ID'")"
  DC up -d --wait --wait-timeout 240 m1n >/dev/null 2>&1 || { DC logs --no-color --tail 80 m1n 2>&1 | m1n_show; die "m1n did not become healthy again after the restart (its log tail is above)"; }
  i=0; body=""
  while [ "$i" -lt "$M1N_READY_SECS" ]; do
    body="$(m1n_outage_probe http://m1n:8080/readyz)" && printf '%s' "$body" | grep -Eq '"ready": *true' && break
    body=""; i=$((i+1)); sleep 1
  done
  [ -n "$body" ] || { DC logs --no-color --tail 80 m1n 2>&1 | m1n_show; die "m1n did not answer /readyz with ready=true within ${M1N_READY_SECS} s of the restart, over the private network (its log tail is above)"; }
  echo "  m1n is back: /readyz answers ready=true over the private network after $i s of waiting"
  DC logs --no-color m1n 2>&1 | grep -E 'verify: (SMTP node=|orchestrator wired)|queue: bulk endpoint wired' | tail -3 | cut -c1-260 | m1n_show
  i=0; n1="$n0"
  while [ "$i" -lt 30 ]; do n1="$(m1n_psql "select count(*) from verify.node_audits where node_id = '$M1N_NODE_ID'")"; [ "$n1" -gt "$n0" ] && break; i=$((i+1)); sleep 1; done
  if [ "$n1" -gt "$n0" ]; then echo "  the restarted control plane audited the node again at boot ($n0 -> $n1 audit rows)"
  else echo "     NOTE: no new audit row within 30 s of the restart ($n0 rows): the node row keeps its last green audit and is promoted from that"; fi
  a="$(m1n_psql "select coalesce(audit_status, '') from verify.nodes where id = '$M1N_NODE_ID'")"
  [ "$a" = green ] || die "after the restart the node's audit status is '${a:-missing}', not green: the promotion that puts the node back in service needs it"
  [ "$(m1n_psql "with u as (update verify.nodes set status = 'active', port25_open = true where id = '$M1N_NODE_ID' and audit_status = 'green' returning 1) select count(*) from u")" = 1 ] || die "the promotion UPDATE after the restart changed no row: the node row is missing"
  m1n_psql "select 'node row after the restart and the promotion: status=' || status || ' port25_open=' || port25_open::text from verify.nodes where id = '$M1N_NODE_ID'" | m1n_show
}

m1n_outage() {
  STAGE=m1n-outage; banner "M1N-4b" "module 1's control plane is stopped in the middle of a job: the platform's calls fail typed and nothing is settled or invented; started again, the job completes exactly once"
  local t0=$SECONDS
  m1n_step M1N-4b.1 "phase 1: a real tenant, a reserve, one job in flight (the adapter's Status shows progress and not complete)"
  m1n_outage_phase 1 "${M1N_OUTAGE_TESTS[0]}"
  m1n_step M1N-4b.2 "stop m1n (the node, the fixtures and Postgres keep running) and prove it is gone"
  m1n_outage_stop
  m1n_step M1N-4b.3 "phase 2: the module is down: typed errors and no value from every call, polling halts, nothing settled or invented"
  m1n_outage_phase 2 "${M1N_OUTAGE_TESTS[1]}"
  m1n_step M1N-4b.4 "start m1n again and wait for its readiness over the private network"
  m1n_outage_start
  m1n_step M1N-4b.5 "phase 3: the job completes exactly once, charged exactly once, the books close, a replay writes nothing"
  m1n_outage_phase 3 "${M1N_OUTAGE_TESTS[2]}"
  echo "  M1N-4b OK in $((SECONDS - t0)) s"
}

# the test-count floor of this scope: the tests of stage M1N-4 and the three outage phases, each proven to have run by its own PASS line (the phases were run by name, outside the ordinary run)
m1n_floor() {
  STAGE=m1n-floor; banner "M1N-F" "every TestM1Node test ran: the ordinary run and the three outage phases, counted against the floor"
  local i t n=$(( ${#M1N_TESTS[@]} + ${#M1N_OUTAGE_TESTS[@]} ))
  for i in 1 2 3; do
    [ -s "$WORK/m1n-outage-$i.log" ] || die "the log of outage phase $i is missing: the outage sub-stage did not run it"
    cat "$WORK/m1n-outage-$i.log" >> "$WORK/m1n-run.log"
  done
  for t in "${M1N_OUTAGE_TESTS[@]}"; do grep -q "^--- PASS: $t " "$WORK/m1n-run.log" || die "$t has no '--- PASS' line in the run log: it did not run, or did not pass"; done
  printf 'ok  \t%s\t0.000s\n' "$GO_PKG" >> "$WORK/m1n-run.log"
  printf 'ok  \t%s\t0.000s\n' "$GO_PKG" >> "$WORK/m1n-list.log"
  # the floor of THIS scope is the number of tests named in M1N_TESTS and M1N_OUTAGE_TESTS (the full floor, tests/floors-harness.txt, never counts them: they are behind a build tag)
  printf '# the TestM1Node tests of scope m1node\ngo|%s|%s\n' "$GO_PKG" "$n" > "$WORK/floors-m1node.txt"
  bash "$PLATFORM_DIR/scripts/check-test-floors.sh" --floors "$WORK/floors-m1node.txt" --go-run-log "$WORK/m1n-run.log" --go-list-log "$WORK/m1n-list.log" || die "the TestM1Node test-count floor ($n)"
  echo "  floor: $n TestM1Node tests declared, run and passed (${#M1N_TESTS[@]} in the ordinary run, ${#M1N_OUTAGE_TESTS[@]} outage phases)"
}

# None of this run's credentials may appear in what the stack logged or in what this script captured (a workflow log is public). Names a file and a count, never a value.
m1n_secret_leak() {
  STAGE=m1n-secret-leak; banner "M1N-6" "none of this run's credentials is in the logs of the module-1 node stack"
  local f v n bad=0 name
  for f in fx verifier-node m1n; do DC logs --no-color "$f" > "$WORK/m1n-svc-$f.log" 2>&1 || die "could not read the logs of $f"; done
  for name in M1N_NODE_TOKEN M1_KEY M1_PEPPER M1_QUEUE_KEY M1_APP_PASS PG_PASS; do
    v="${!name:-}"; [ -n "$v" ] || die "$name is empty: the leak scan would vouch for nothing"
    for f in "$WORK"/m1n-svc-*.log "$WORK"/m1n-selfcheck.log "$WORK"/m1n-list.log "$WORK"/m1n-run.log; do
      n="$(grep -cF -- "$v" "$f" || true)"
      [ "$n" = 0 ] || { echo "  LEAK: the value of $name appears $n time(s) in $(basename "$f")"; bad=$((bad+1)); }
    done
  done
  [ "$bad" = 0 ] || die "$bad credential(s) of this run appear in a log of the module-1 node stack (the files are named above; the values are not printed)"
  echo "  scanned $(ls "$WORK"/m1n-svc-*.log "$WORK"/m1n-selfcheck.log "$WORK"/m1n-list.log "$WORK"/m1n-run.log | wc -l | tr -d ' ') logs for 6 credentials: none present"
}

m1n_exposure() {
  STAGE=m1n-exposure; banner "M1N-5" "nothing of the module-1 node stack publishes a port; the fixture network is internal"
  DC ps --all --format json > "$WORK/ps-m1n.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps-m1n.json" fx verifier-node m1n || die "a service of the module-1 node stack is reachable from outside the network"
  [ "$(docker network inspect "$M1N_NET" -f '{{.Internal}}')" = true ] || die "$M1N_NET is not an internal network"
  local id n=0 pb
  for id in $(DC ps -q fx verifier-node m1n); do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; n=$((n+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  [ "$n" = 3 ] || die "expected to inspect the 3 containers of the module-1 node stack, found $n"
  echo "  second instrument: $n containers inspected, none has a host port binding; network $M1N_NET internal=true"
}

# The whole stack, in order. Used by both scopes.
m1n_run() {
  m1n_stage1
  m1n_stage2
  m1n_stage3
  m1n_stage4
  m1n_outage      # stops and starts m1n: after every stage that needs it up, before the two checks that read its logs
  m1n_floor
  m1n_exposure
  m1n_secret_leak
}

# H_SCOPE=m1node: the stack on its own, then the end of the run (the cleanup trap removes the containers).
m1n_run_scope() {
  m1n_run
  STAGE=done
  echo
  echo "================ NOT CHECKED (scope m1node: what this run did not and could not vouch for)"
  m1n_not_checked
  echo
  echo "HARNESS RESULT: GREEN (scope m1node, $((SECONDS - M1N_T0)) s) — module 1 built from $(lockpin coldmail-verify | cut -c1-7); the real node, the fixtures and a wired control plane; the TestM1Node tests passed"
  exit 0
}

m1n_not_checked() {
  cat <<'EOF'
  - the module-1 node stack (stages M1N-1 to M1N-6) supersedes any statement above that module 1 has no SMTP node or no DNS answers of its own: module 1 had its REAL verifier node (module 1's own image,
    the guard ON, a green audit) and a second control plane wired to it, against fixtures on a second internal network that the node's destination guard calls public. Not exercised: the node's egress firewall
    (verifier_egress.nft needs the node's network namespace and NET_ADMIN), a real MX, a real provider's behaviour (greylisting windows, throttling, tarpits), the default retry ladder (the profile settles a
    transient at its first attempt), the audit going red later and the quarantine that follows (the watcher loop is off), a node on the public internet with a real PTR, a second node, the rate-limit edge (30
    session starts per 60 s per domain), the vendor path (ZeroBounce stays disabled), the warm-up cap (set to 0), and the recipes the shipped wiring cannot produce (smtp_unwired, warmup_cap, M1-16 no-token,
    unknown_error). The wired control plane is a second service (m1n) on the same database, not the m1 of the other stages.
  - a verdict module 1 derived from a transcript is only as real as the fixture's conversation: the fixture is hand-written (tests/harness/fx), the node is not.
EOF
}
