#!/usr/bin/env bash
# stage.sh — the browser end-to-end scope (Phase 6, chunk A9; Docs/phase-6-spec.md decisions 1, 2 and 9, chunks A10 and A11). SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself)
# AFTER tests/harness/gotrue/stage.sh, tests/harness/jobs/stage.sh and tests/harness/m2worker/stage.sh, whose helpers it uses (gotrue_stage, jobs_ip, jobs_cid, m2w_stage_5c), and it uses what run.sh defines:
# DC, die, banner, mask, STAGE, WORK, HERE, PLATFORM_DIR, PROJECT_NET, psql_su, m3w_stage_5b, and the credentials of stage 2 (M2_TOKEN, M3_TOKEN, M3_ADMIN_KEY, M2_ADMIN_KEY, M2_WORKER_TOKEN, M3_WORKER*_TOKEN,
# SINK_PASS, PG_PASS, PL_PASS, GT_JWT_SECRET, GT_DB_PASS, JOBS_M1K, JOBS_VENDOR).
#
# WHAT THE SCOPE IS. A real browser (Playwright's Chromium, from a digest-pinned image) INSIDE the harness's private network, driving the built web app against the real stack: Postgres, a real GoTrue, module 1
# (with its fixture DNS and its vendor stand-in), modules 2 and 3 with their workers, the controllable sink and the From-gate's fixture DNS, and ONE platform instance with the job engine on that serves the app and
# the API from one origin. Nothing is published to the host. The screens' specs (web/e2e/*.spec.ts) run in the browser against this stack; the stage makes everything they rely on and checks that it is what it says.
#
# DORMANT BY DEFAULT. run.sh calls these functions only when E2E_ON=1: under H_SCOPE=e2e and nowhere else. It is NOT nameable in FULL_INCLUDES: the scope runs its own stack (module 1 as m1j, one platform instance
# with a different configuration) and ends with its own `exit`. With E2E_ON=0 nothing here runs, docker-compose.e2e.yml is not merged, no secret is minted and no container exists; the scope adds no test to
# tests/floors-harness.txt (its floor is E2E_MIN_TESTS below, asserted by this stage on the report the run itself wrote).
#
# What is here, in the order run.sh calls it:
#   e2e_mint          stage 2: the unsubscribe secret, the anon key and the service-role key (HS256, signed with GoTrue's secret by tests/harness/e2e/jwt.mjs) and the test users' passwords, minted, masked BEFORE anything
#                     uses them; two lines appended mode 600 to the env file (H_E2E_UNSUBSCRIBE_SECRET, H_E2E_ANON_KEY); the rest stays in shell variables; none printed
#   e2e_pull          stage 3: the two digest-pinned images (nginx, Playwright) are pulled, so a runner that cannot pull THAT digest is red before the migrations spend minutes
#   e2e_scope_run     H_SCOPE=e2e, after stage 4b: E1 the web app built, E2 GoTrue, E3 module 1, E4 module 2, E5 module 3, E6 the platform and the TLS edge, E7 the users, E8 the browser, E9 the evidence; ends the run
# Nothing here prints a credential: no `set -x`, no environment dump; a failing check names a file, a status and a number, never a value; the minted values are never on a command line but a container's own.

E2E_STAGE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_COMPOSE="$E2E_STAGE_DIR/../docker-compose.e2e.yml"
E2E_APP_HOST="app.harness.test"; E2E_AUTH_HOST="auth.harness.test"
E2E_VERIFY_PATH="/auth/v1/verify"   # the mailer's URL path: GoTrue resolves it against its external URL and an absolute path REPLACES that URL's own, so the /auth/v1 is written here (docker-compose.e2e.yml says why)
E2E_APP_URL="https://$E2E_APP_HOST"; E2E_AUTH_ORIGIN="https://$E2E_AUTH_HOST"; E2E_AUTH_URL="$E2E_AUTH_ORIGIN/auth/v1"; E2E_ISSUER="$E2E_AUTH_URL"   # the public Supabase URL is the whole address the browser calls GoTrue at (web/src/auth/gotrue.ts uses it as given), and it is the issuer GoTrue stamps
E2E_SINK_SMTP_PORT=1025
E2E_M3_SINK_HOST="10.smtp-ctl.example.test"      # the sink's alias module 3's accounts name (docker-compose.yml): private-looking, below the From domain example.test of tests/harness/m3adapter/m3dns.json
E2E_USER_TAGS=(a b c s)                          # a, b: signed-in users (b is the other user of the cross-user specs); c: the invitee of the browser's invite flow; s: a user with no credits, no entitlement, no sending account
E2E_FUNDED_TAGS="a b c"                          # the users who hold credits at modules 1 and 2, an entitlement at module 3 and a sending account at module 2
E2E_CREDITS=100000                               # per metered module and funded user, through the ledger
E2E_MIN_TESTS="${E2E_MIN_TESTS:-38}"             # the floor of this scope: the lead raises it as the screens' specs land (web/e2e/README.md); the smoke spec holds ten tests
E2E_MIN_SHOTS="${E2E_MIN_SHOTS:-100}"              # pictures the run must have left in E2E_SHOTS_DIR (the smoke spec takes one: the helper is proven; the screens' specs take one per main screen)
E2E_UNSUB=""; E2E_ANON=""; E2E_SERVICE=""        # set by e2e_mint (only the shell's copies: the leak scans look for them)
# the global dials of module 2 for the scope (tests/harness/jobscenario/send.go m2Dials, and why): two failed attempts end a job, a one-second retry ladder, no pause between sends, one lane per user, a 250 ms tick
E2E_M2_DIALS=(
  'job_max_failed_attempts|2'
  'retry_backoff_schedule|{"ladder_seconds":[1,1,1],"max_seconds":1}'
  'send_delay_min_sec|0'
  'send_delay_max_sec|0'
  'concurrency_per_user_default|1'
  'dispatch_tick_interval_ms|250'
)
# the dials of module 3 for the scope, written to send_byo.config like the From-gate's (tests/harness/jobscenario/send.go cmdSendPrepare): the gate in ENFORCE against the fixture DNS, a user's sends at most a second
# apart on one lane (module 3's defaults are three to ten seconds and three lanes). The minimum is lowered before the maximum so that min <= max holds at every step.
E2E_M3_DIALS=(
  'from_gate_mode|"enforce"'
  'from_dns_resolver|"m3dns:53"'
  'send_delay_min_sec|0'
  'send_delay_max_sec|1'
  'concurrency_per_user_default|1'
)
# a compact JWT (both of the first two segments are JSON objects, so both start with eyJ): the service logs and the runner's own log hold none
E2E_JWT_SHAPE='eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}'
# the services of the scope, for the exposure check and the leak scan (every one that has a log worth reading; e2e-runner is a one-off container and is removed when it ends)
E2E_SERVICES=(postgres gotrue m1j jobsdns zbstub m2 m2-worker m3 m3-worker m3-worker-2 m3-worker-3 smtp-ctl smtp-sink m3dns e2e-platform e2e-edge)

e2e_enforces_modes() { [ "$(uname -s)" = Linux ]; }       # a Windows or macOS bind mount does not keep file modes: only Linux is asked
e2e_mode_of() { stat -c '%a' "$1" 2>/dev/null; }

# e2e_fail <service> <message>: the log of the service that failed, then the run's own RED line (which names the stage and prints the tail of every service). The message names no value.
e2e_fail() {
  local svc="$1"; shift
  echo "  ---- the log of $svc (the last 80 lines; the stage is [$STAGE]) ----"
  DC logs --no-color --tail=80 "$svc" 2>&1 | sed 's/^/     | /'
  die "$*"
}

# e2e_json_get <member>: stdin is a JSON document, prints the member (a dotted path reaches into objects: user.id) when it is a string and nothing otherwise (the document is never echoed: it may hold a token)
e2e_json_get() {
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{const v=process.argv[1].split(".").reduce((o,k)=>o[k],JSON.parse(s));process.stdout.write(typeof v==="string"?v:"")}catch{}})' "$1"
}

# ── stage 2: secrets ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Called right AFTER gotrue_mint (the anon and service-role keys are signed with GoTrue's secret): it mints, masks, then appends two lines to the env file (written under umask 077, so mode 600, checked where the
# host keeps modes). The anon key reaches the platform (it hands it to every visitor in /config.json: public by design) and the runner; the service-role key stays in the stage (it calls GoTrue's admin routes
# with it); the passwords stay in the stage and in the fixtures file (mode 600).
e2e_mint() {
  local u m other tag p anon svc seen=""
  [ -n "${GT_JWT_SECRET:-}" ] || die "GoTrue's JWT secret was not minted: e2e_mint runs after gotrue_mint (the anon key and the service-role key are signed with it)"
  u="$(openssl rand -hex 32)"
  [ "${#u}" = 64 ] || die "could not mint the e2e scope's unsubscribe secret (openssl rand printed ${#u} characters, want 64)"
  for other in "$GT_JWT_SECRET" "${GT_DB_PASS:-}" "${JOBS_JWT:-}" "${JOBS_M1K:-}" "${JOBS_VENDOR:-}"; do
    [ "$u" != "$other" ] || die "the e2e scope's unsubscribe secret equals another secret of this run: they must be different credentials"
  done
  mask "$u"                                                              # masked BEFORE it is written anywhere or handed to any command
  anon="$(E2E_JWT_SECRET="$GT_JWT_SECRET" node "$E2E_STAGE_DIR/jwt.mjs" anon 31536000)" || die "could not mint the anon key (tests/harness/e2e/jwt.mjs failed)"
  svc="$(E2E_JWT_SECRET="$GT_JWT_SECRET" node "$E2E_STAGE_DIR/jwt.mjs" service_role 14400)" || die "could not mint the service-role key (tests/harness/e2e/jwt.mjs failed)"
  mask "$anon"; mask "$svc"
  printf '%s\n' "$anon" | grep -qE "^$E2E_JWT_SHAPE\$" || die "the anon key is not a compact token of three segments"
  printf '%s\n' "$svc" | grep -qE "^$E2E_JWT_SHAPE\$" || die "the service-role key is not a compact token of three segments"
  [ "$anon" != "$svc" ] || die "the anon key and the service-role key are the same token"
  for tag in "${E2E_USER_TAGS[@]}"; do
    p="$(openssl rand -hex 12)"
    [ "${#p}" = 24 ] || die "could not mint the password of the e2e user $tag (openssl rand printed ${#p} characters, want 24)"
    case " $seen " in *" $p "*) die "two of the e2e users were given the same password";; esac
    seen="$seen $p"
    mask "$p"
    printf -v "E2E_PW_$tag" '%s' "$p"
  done
  [ -f "$WORK/harness.env" ] || die "the env file is missing: e2e_mint runs after run.sh wrote it"
  if e2e_enforces_modes; then
    m="$(e2e_mode_of "$WORK/harness.env")"
    [ "$m" = 600 ] || die "the env file $WORK/harness.env is mode ${m:-unknown}, expected 600: it holds this run's secrets"
  fi
  ( umask 077; { echo "H_E2E_UNSUBSCRIBE_SECRET=$u"; echo "H_E2E_ANON_KEY=$anon"; } >> "$WORK/harness.env" ) || die "could not append the e2e scope's secrets to the env file"
  E2E_UNSUB="$u"; E2E_ANON="$anon"; E2E_SERVICE="$svc"
  echo "  minted for the e2e scope: 1 unsubscribe secret (32 random bytes in hex), the anon key and the service-role key (HS256, signed with GoTrue's secret), ${#E2E_USER_TAGS[@]} test-user passwords — masked, none printed"
}

# ── stage 3: the images ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
e2e_pull() {
  STAGE=build
  DC pull e2e-edge e2e-runner > "$WORK/e2e-pull.log" 2>&1 || { tail -15 "$WORK/e2e-pull.log" | sed 's/^/     | /'; die "the runner could not pull a digest-pinned image of docker-compose.e2e.yml (the nginx edge or the Playwright image): the pin is NOT changed from the runner's side, report it"; }
  echo "  the digest-pinned images are pulled: $(DC config --images 2>/dev/null | grep -E '^(nginx|mcr\.microsoft\.com/playwright)' | tr '\n' ' ')"
}

# ── E1: the web app, built on the runner ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The built app (web/dist) is what the platform serves (PLATFORM_WEB_DIR): `npm ci` from the lockfile, then the same build the `web` workflow runs. `npm ci` downloads no browser (@playwright/test has no install
# script; PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD is set anyway). The Playwright the specs run with (node_modules/@playwright/test) must be the version of the image that carries the browsers, or Chromium would not start.
e2e_image_version() { sed -n 's#^[[:space:]]*image: mcr\.microsoft\.com/playwright:v\([0-9][0-9.]*\)-.*#\1#p' "$E2E_COMPOSE" | head -1; }

e2e_web() {
  STAGE=e2e-web; banner E1 "the web app: npm ci and npm run build on the runner, the Playwright package equal to the image's"
  local web="$PLATFORM_DIR/web" major want got n
  command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1 || die "node and npm are not on the runner's PATH: the web app cannot be built (the harness workflow sets Node up for this scope)"
  major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null)"
  case "$major" in ''|*[!0-9]*) die "could not read the node version";; esac
  [ "$major" -ge 24 ] || die "node $major is too old: web/package.json asks for node 24 or later (engines)"
  echo "  node $(node --version), npm $(npm --version)"
  ( cd "$web" && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --no-audit --no-fund ) > "$WORK/e2e-npm-ci.log" 2>&1 || { tail -25 "$WORK/e2e-npm-ci.log" | sed 's/^/     | /'; die "npm ci failed in web/ (the lockfile and package.json disagree, or the registry did not answer)"; }
  ( cd "$web" && npm run build ) > "$WORK/e2e-npm-build.log" 2>&1 || { tail -40 "$WORK/e2e-npm-build.log" | sed 's/^/     | /'; die "npm run build failed in web/"; }
  [ -s "$web/dist/index.html" ] || die "web/dist/index.html is missing or empty after the build: the platform has nothing to serve"
  n="$(find "$web/dist" -type f | wc -l | tr -d ' ')"
  [ "$n" -ge 2 ] || die "web/dist holds $n file(s) after the build: an index and its assets are expected (the instrument matched nothing)"
  want="$(e2e_image_version)"; [ -n "$want" ] || die "could not read the Playwright version from the image line of docker-compose.e2e.yml"
  got="$(cd "$web" && node -p "require('./node_modules/@playwright/test/package.json').version" 2>/dev/null)"
  [ "$got" = "$want" ] || die "web/node_modules/@playwright/test is version '${got:-none}' but the digest-pinned image carries the browsers of $want: a runner and its image must be the same version (bump web/package.json, the lockfile and the image line together)"
  echo "  web/dist built: $n file(s); @playwright/test $got is the version of the pinned image; no browser was downloaded (npm ci runs no install script of Playwright)"
}

# ── E6a: the certificate ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# A self-signed certificate for the run, ECDSA P-256, valid two days, with the two names of the scope as subject alternative names (Chromium ignores the common name). The key is mode 600 in a directory
# of mode 700 owned by the run's user, which is the user the nginx container runs as. Both are thrown away with the run's temporary directory. The TLS choice and its reasons are in tests/harness/e2e/nginx.conf.
e2e_tls() {
  local d="$WORK/e2e/tls" text pub1 pub2 m
  ( umask 077; mkdir -p "$d" ) || die "could not make the certificate directory $d"
  # relative file names from inside the directory, and no path conversion for the one argument that starts with a slash: Git Bash on a Windows machine (a rehearsal) rewrites /CN=... as a drive path, a Linux runner never does
  ( umask 077; cd "$d" && export MSYS_NO_PATHCONV=1 && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -keyout tls.key -out tls.crt -subj "/CN=$E2E_APP_HOST" \
    -addext "subjectAltName=DNS:$E2E_APP_HOST,DNS:$E2E_AUTH_HOST" -addext "basicConstraints=critical,CA:FALSE" -addext "extendedKeyUsage=serverAuth" ) > "$WORK/e2e-tls.log" 2>&1 \
    || { tail -5 "$WORK/e2e-tls.log" | sed 's/^/     | /'; die "openssl could not make the run's self-signed certificate"; }
  chmod 600 "$d/tls.key"; chmod 644 "$d/tls.crt"
  [ -s "$d/tls.key" ] && [ -s "$d/tls.crt" ] || die "the certificate or its key is empty"
  text="$(openssl x509 -in "$d/tls.crt" -noout -text 2>/dev/null)"
  printf '%s\n' "$text" | grep -A1 'Subject Alternative Name' | grep -q "DNS:$E2E_APP_HOST" || die "the certificate does not carry the name $E2E_APP_HOST as a subject alternative name: Chromium would refuse it for that name"
  printf '%s\n' "$text" | grep -A1 'Subject Alternative Name' | grep -q "DNS:$E2E_AUTH_HOST" || die "the certificate does not carry the name $E2E_AUTH_HOST as a subject alternative name: Chromium would refuse it for that name"
  openssl x509 -in "$d/tls.crt" -noout -checkend 3600 >/dev/null 2>&1 || die "the certificate expires within the hour"
  pub1="$(openssl x509 -in "$d/tls.crt" -noout -pubkey 2>/dev/null | openssl sha256 2>/dev/null)"; pub2="$(openssl pkey -in "$d/tls.key" -pubout 2>/dev/null | openssl sha256 2>/dev/null)"
  [ -n "$pub1" ] && [ "$pub1" = "$pub2" ] || die "the certificate and its key are not a pair"
  if e2e_enforces_modes; then
    m="$(e2e_mode_of "$d/tls.key")"; [ "$m" = 600 ] || die "the certificate's key is mode ${m:-unknown}, expected 600"
  fi
  echo "  TLS: a per-run self-signed certificate (ECDSA P-256, 2 days) for $E2E_APP_HOST and $E2E_AUTH_HOST, its key mode 600; the browser is told to ignore the self-signed chain (ignoreHTTPSErrors), nothing else trusts it"
}

# ── E3: module 1 ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
e2e_m1() {
  STAGE=e2e-module1; banner E3 "module 1 (m1j) asking the fixture DNS, its vendor fallback ON and wired to its own stand-in (zbstub)"
  local ip got env_out
  mkdir -p "$WORK/jobs"
  DC up -d --no-deps --wait --wait-timeout 180 jobsdns > "$WORK/e2e-up-jobsdns.log" 2>&1 || e2e_fail jobsdns "the fixture DNS (service jobsdns: tests/harness/fx, tests/harness/jobs/dns.json) did not become healthy"
  ip="$(jobs_ip jobsdns)"
  case "$ip" in [0-9]*.[0-9]*.[0-9]*.[0-9]*) ;; *) die "could not read the address of jobsdns on $PROJECT_NET (got '$ip')";; esac
  printf 'nameserver %s\noptions timeout:2 attempts:1\n' "$ip" > "$WORK/jobs/resolv.conf"; chmod 644 "$WORK/jobs/resolv.conf"
  DC up -d --no-deps --wait --wait-timeout 180 zbstub > "$WORK/e2e-up-zbstub.log" 2>&1 || e2e_fail zbstub "module 1's own ZeroBounce stand-in (service zbstub) did not become healthy"
  DC up -d --no-deps --wait --wait-timeout 240 m1j > "$WORK/e2e-up-m1j.log" 2>&1 || e2e_fail m1j "module 1 (service m1j) did not become healthy asking the fixture DNS"
  got="$(DC exec -T m1j cat /etc/resolv.conf 2>&1)"
  printf '%s\n' "$got" | grep -qx "nameserver $ip" || die "module 1's resolv.conf does not name the fixture ($ip) as its nameserver: its answers would be the runner's"
  env_out="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(jobs_cid m1j)" 2>/dev/null)"
  printf '%s\n' "$env_out" | grep -qx 'TM_FALLBACK_MODE=enabled' || die "module 1's container does not have TM_FALLBACK_MODE=enabled: docker-compose.e2e.yml's override did not reach it"
  printf '%s\n' "$env_out" | grep -qx 'TM_EXTERNAL_ZEROBOUNCE_BASE_URL=http://zbstub:8080' || die "module 1's container does not have the stand-in as its vendor (TM_EXTERNAL_ZEROBOUNCE_BASE_URL): its vendor would be the real one, which the network cannot reach"
  echo "  fixture DNS $ip answers; module 1 (m1j) is healthy, its resolv.conf is the fixture's, its vendor fallback is ON and its vendor is the stand-in (the key is present, not shown)"
}

# ── E4: module 2 ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# e2e_describe_body: stdin is an answer's body; prints what SHAPE it has and nothing it SAYS: its size in bytes and, for a JSON object, the NAMES of its top-level keys (never a value: a body may hold a token, a
# link or a credential), or that it is empty, a JSON array or scalar, or not JSON at all (with the kind of character it starts with). This is what a red run prints when it could not read an answer.
e2e_describe_body() {
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const n=Buffer.byteLength(s);if(n===0){process.stdout.write("0 bytes, empty");return}try{const j=JSON.parse(s);if(j!==null&&typeof j==="object"&&!Array.isArray(j)){const k=Object.keys(j).sort();process.stdout.write(n+" bytes, a JSON object with "+(k.length?"the keys: "+k.join(", "):"no keys"))}else{process.stdout.write(n+" bytes, a JSON "+(Array.isArray(j)?"array":j===null?"null":typeof j))}}catch{const c=s.trimStart().charAt(0);process.stdout.write(n+" bytes, NOT JSON (it starts with "+("{[<\"".includes(c)&&c!==""?"a "+c+" character":"another character")+")")}})'
}

# e2e_http_what: one phrase for the last answer of e2e_m2_call, for a message: the HTTP status, and the shape of the body (e2e_describe_body); when no status came (curl failed) what curl said.
e2e_http_what() {
  local w="HTTP $E2E_HTTP_CODE, $(printf '%s' "$E2E_HTTP_BODY" | e2e_describe_body)"
  [ "$E2E_HTTP_CODE" != 000 ] || w="$w; curl said: ${E2E_HTTP_ERR:-nothing}"
  printf '%s' "$w"
}

# e2e_m2_call <method> <path> <json body>: one request to module 2's operator surface from INSIDE module 2's container (curl, as its own health check uses). Sets E2E_HTTP_CODE (three digits, 000 when none came),
# E2E_HTTP_BODY (the WHOLE body, with every credential of the request taken out of it: it is parsed, so it is never cut) and E2E_HTTP_ERR (what curl said on its stderr, cut to 200 characters). curl's stderr is kept
# apart from the body (a warning must not become part of it) and its status is the last line, after a newline of its own (-w): the body is everything before that line, however many newlines it has. The operator
# key and the request body are on the container's command line only, never on the runner's output; a message about the answer carries its SHAPE (e2e_http_what), never its text.
e2e_m2_call() {
  local out rc
  out="$(DC exec -T m2 curl -sS -m 30 -X "$1" -H "Authorization: Bearer $M2_ADMIN_KEY" -H 'Content-Type: application/json' -d "$3" -w '\n%{http_code}' "http://127.0.0.1:8080$2" 2> "$WORK/e2e-curl.err")"; rc=$?
  E2E_HTTP_ERR="$(head -c 200 "$WORK/e2e-curl.err" 2>/dev/null | tr '\r\n' '  ')"
  E2E_HTTP_CODE="${out##*$'\n'}"
  case "$out" in *$'\n'*) E2E_HTTP_BODY="${out%$'\n'*}";; *) E2E_HTTP_BODY="";; esac
  [ -z "${SINK_PASS:-}" ] || E2E_HTTP_BODY="${E2E_HTTP_BODY//$SINK_PASS/[redacted]}"
  [ -z "${M2_ADMIN_KEY:-}" ] || E2E_HTTP_BODY="${E2E_HTTP_BODY//$M2_ADMIN_KEY/[redacted]}"
  case "$E2E_HTTP_CODE" in [0-9][0-9][0-9]) ;; *) E2E_HTTP_CODE="000";; esac
  return "$rc"
}

e2e_m2() {
  STAGE=e2e-module2; banner E4 "module 2 with its real worker and the controllable sink; its global dials set for a run of seconds"
  local d key val
  DC up -d --wait --wait-timeout 240 m2 > "$WORK/e2e-up-m2.log" 2>&1 || e2e_fail m2 "module 2 did not become healthy"
  m2w_stage_5c
  STAGE=e2e-module2
  for d in "${E2E_M2_DIALS[@]}"; do
    key="${d%%|*}"; val="${d#*|}"
    e2e_m2_call PUT "/v1/admin/config/$key" "{\"value\":$val}" || true
    [ "$E2E_HTTP_CODE" = 200 ] || e2e_fail m2 "module 2 answered $E2E_HTTP_CODE to the dial $key (want 200): $(e2e_http_what)"
  done
  echo "  module 2: ${#E2E_M2_DIALS[@]} global dials set (two failed attempts end a job, a one-second retry ladder, no pause between sends, one lane per user, a 250 ms tick)"
}

# ── E5: module 3 ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Stage 5b of the full run, the same function (module 3's three workers, the sink, Mailpit and the From-gate's fixture DNS m3dns; its probes are made from the container GoTrue runs in). Stage 5b switches the
# From-gate OFF for the raw module 3 tests: this scope puts it back to ENFORCE against m3dns, which is what the screens' From-gate result is made of (tests/harness/m3adapter/m3dns.json answers example.test).
e2e_dial_row() {   # <config key> <jsonb value text>: the row is updated and exactly one row was
  local n
  n="$(psql_su "WITH u AS (UPDATE send_byo.config SET value = '{\"v\":$2}' WHERE key = '$1' RETURNING 1) SELECT count(*) FROM u")" || die "could not write module 3's dial $1"
  [ "$n" = 1 ] || die "module 3 has no dial $1 in send_byo.config ($n row(s) updated): the pinned module is not the one these dials were written against"
}
e2e_m3() {
  STAGE=e2e-module3; banner E5 "module 3 with its three workers and both sinks; the From-gate in ENFORCE against the fixture DNS; one lane per user, at most a second between two sends"
  local d
  DC up -d --wait --wait-timeout 240 m3 > "$WORK/e2e-up-m3.log" 2>&1 || e2e_fail m3 "module 3 did not become healthy"
  M3W_PROBE_SVC=gotrue m3w_stage_5b
  STAGE=e2e-module3
  for d in "${E2E_M3_DIALS[@]}"; do e2e_dial_row "${d%%|*}" "${d#*|}"; done
  echo "  module 3: ${#E2E_M3_DIALS[@]} dials written to send_byo.config (From-gate enforce, resolver m3dns:53, a user's sends at most a second apart on one lane)"
}

# ── E6: the platform and the TLS edge ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The preflight, run from the Playwright image on the private network (node's fetch, the self-signed chain ignored): the platform answers through the edge, /config.json carries exactly the public address
# and the anon key this run was given, GoTrue answers at its hosted path, and the app's own answer for / carries the strict policy. A red here names which hop is wrong before a spec has to.
E2E_PREFLIGHT='
const base = process.env.E2E_BASE_URL, auth = process.env.E2E_GOTRUE_URL, anon = process.env.E2E_ANON_KEY
const fail = (m) => { console.error("preflight: " + m); process.exit(1) }
const get = async (u) => { try { return await fetch(u, { signal: AbortSignal.timeout(15000) }) } catch (e) { return fail(u + " did not answer: " + (e.cause?.code ?? e.message)) } }
let r = await get(base + "/readyz"); if (r.status !== 200) fail(base + "/readyz answered " + r.status + " (want 200)")
r = await get(base + "/config.json"); if (r.status !== 200) fail(base + "/config.json answered " + r.status + " (want 200): PLATFORM_WEB_DIR is not served")
const c = await r.json().catch(() => null)
if (!c || c.supabase_url !== auth || c.anon_key !== anon) fail("/config.json does not hold exactly the public GoTrue address and the anon key of this run")
r = await get(auth + "/health"); if (r.status !== 200) fail(auth + "/health answered " + r.status + " (want 200): the edge does not hand /auth/v1/ to GoTrue")
r = await get(base + "/"); const csp = r.headers.get("content-security-policy") ?? ""
if (r.status !== 200) fail(base + "/ answered " + r.status + " (want 200)")
if (!csp.includes("default-src \x27none\x27") || !csp.includes("connect-src \x27self\x27 " + new URL(auth).origin)) fail("the policy of / does not carry default-src none and connect-src self plus the GoTrue origin")
if (!(r.headers.get("cache-control") ?? "").includes("no-store")) fail("/ is not Cache-Control no-store")
console.log("preflight ok: " + base + " (readyz 200, config.json equal to this run, / with its policy) and " + auth + " (health 200), reached through the TLS edge from the browser image")
'

e2e_platform() {
  STAGE=e2e-platform; banner E6 "the platform (the engine on, the app and the API from one origin) behind the TLS edge, and the hops proved from the browser's image"
  local env_out want
  e2e_tls
  DC up -d --no-deps --wait --wait-timeout 240 e2e-platform > "$WORK/e2e-up-platform.log" 2>&1 || e2e_fail e2e-platform "the platform instance did not become healthy (a dial it refuses, PLATFORM_WEB_DIR without an index.html, or a refused connection to its database; its log is above)"
  DC logs --no-color e2e-platform 2>&1 | grep -q 'unsubscribe link configured' || e2e_fail e2e-platform "the platform did not log that its unsubscribe link is configured: PLATFORM_UNSUBSCRIBE_SECRET and PLATFORM_PUBLIC_BASE_URL did not both reach it, and a send job could not be confirmed"
  DC up -d --no-deps --wait --wait-timeout 120 e2e-edge > "$WORK/e2e-up-edge.log" 2>&1 || e2e_fail e2e-edge "the TLS edge (nginx) did not become healthy (its configuration, its certificate or an upstream name that does not resolve; its log is above)"
  env_out="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(jobs_cid gotrue)" 2>/dev/null)"
  for want in "API_EXTERNAL_URL=$E2E_ISSUER" "GOTRUE_API_EXTERNAL_URL=$E2E_ISSUER" "GOTRUE_SITE_URL=$E2E_APP_URL" "GOTRUE_JWT_ISSUER=$E2E_ISSUER" "GOTRUE_MAILER_URLPATHS_INVITE=$E2E_VERIFY_PATH" "GOTRUE_MAILER_URLPATHS_RECOVERY=$E2E_VERIFY_PATH"; do
    printf '%s\n' "$env_out" | grep -qx "$want" || die "GoTrue's container does not have ${want%%=*}=${want#*=}: docker-compose.e2e.yml's override did not reach it, so the invite links and the tokens' issuer would be the harness's plain-http names"
  done
  mkdir -p "$WORK/e2e-artifacts"
  DC run --rm -T --no-deps -e NODE_TLS_REJECT_UNAUTHORIZED=0 --entrypoint node e2e-runner --input-type=module -e "$E2E_PREFLIGHT" > "$WORK/e2e-preflight.log" 2>&1 \
    || { sed 's/^/     | /' "$WORK/e2e-preflight.log" | tail -12; e2e_fail e2e-edge "the preflight from the Playwright image failed (what it says is above; the log of the edge follows)"; }
  sed 's/^/  | /' "$WORK/e2e-preflight.log" | tail -3
}

# ── E7: the users ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# One user = an invite minted by GoTrue's admin route (POST /admin/generate_link, the per-run service-role key; the returned action_link is what a spec opens in the browser) + the matching public.users row
# (as the harness's superuser, like tests/harness/jobscenario/db.go newUser) + for a funded user: credits at modules 1 and 2 through the LEDGER (kind grant, naming the first admin, the way jobscenario grants),
# an m3_enable ledger row that the platform's own reconcile loop delivers to module 3 (waited for: converged and observed active), and a dedicated pool and one account at module 2 naming the controllable
# sink's private address (module 2's operator surface; the credential is write-only). The module 3 account is NOT made: the specs add it through the screens. Nothing here prints a link, a token or a password.
e2e_user() {   # <tag> <funded: 1|0>
  local tag="$1" funded="$2" email id link body shape n m pool acct sink_ip
  email="e2e-$tag@example.test"
  body="$(DC exec -T gotrue wget -q -O - --header "Authorization: Bearer $E2E_SERVICE" --header 'Content-Type: application/json' --post-data "{\"type\":\"invite\",\"email\":\"$email\"}" http://127.0.0.1:9999/admin/generate_link 2>/dev/null)" \
    || e2e_fail gotrue "GoTrue refused the invite of the user $tag at POST /admin/generate_link (the service-role key is not accepted, or the route answered an error)"
  id="$(printf '%s' "$body" | e2e_json_get id)"; [ -n "$id" ] || id="$(printf '%s' "$body" | e2e_json_get user.id)"
  link="$(printf '%s' "$body" | e2e_json_get action_link)"; [ -n "$link" ] || link="$(printf '%s' "$body" | e2e_json_get properties.action_link)"
  shape="HTTP 200 (wget exits non-zero on an HTTP error), $(printf '%s' "$body" | e2e_describe_body)"; body=""
  case "$id" in ????????-????-????-????-????????????) ;; *) die "GoTrue's answer to the invite of the user $tag has no user id (the stage reads a top-level id, or user.id): $shape";; esac
  case "$link" in
    "$E2E_ISSUER/verify?"*) ;;
    *) die "the invite link of the user $tag does not start with $E2E_ISSUER/verify? (it starts with '${link%%\?*}'): GoTrue built the link from its external URL and the mailer's URL path (GOTRUE_MAILER_URLPATHS_INVITE=$E2E_VERIFY_PATH: an absolute path replaces the path of the external URL, so /auth/v1 must be IN it), and the browser could not open this one; the answer was: $shape";;
  esac
  case "$link" in *"type=invite"*) ;; *) die "the link GoTrue made for the user $tag is not an invite link";; esac
  # GoTrue writes redirect_to as it holds it: unencoded when it has no & = or # (internal/mailer encodeRedirectURL), so both spellings are the app's own address
  case "$link" in *"redirect_to=https%3A%2F%2F$E2E_APP_HOST"*|*"redirect_to=https://$E2E_APP_HOST"*) ;; *) die "the invite link of the user $tag does not send the browser to $E2E_APP_URL after the verification (GOTRUE_SITE_URL did not reach GoTrue)";; esac
  mask "$link"
  printf -v "E2E_ID_$tag" '%s' "$id"; printf -v "E2E_EMAIL_$tag" '%s' "$email"; printf -v "E2E_LINK_$tag" '%s' "$link"; printf -v "E2E_FUNDED_$tag" '%s' "$funded"; printf -v "E2E_M2ACCOUNT_$tag" '%s' ''
  psql_su "INSERT INTO public.users (id, auth_user_id, email) VALUES ('$id', '$id', '$email')" >/dev/null || die "could not create the platform user of $tag in public.users"
  [ "$funded" = 1 ] || { echo "  user $tag: invited, a platform user, no credits, no entitlement, no sending account"; return 0; }
  for m in m1 m2; do
    n="$(psql_su "WITH i AS (INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key) SELECT '$id'::uuid, '$m', 'grant', $E2E_CREDITS, a.id, 'the e2e scope funds its user', 'e2e-grant-$m-$id' FROM platform.admins a WHERE a.disabled_at IS NULL ORDER BY a.created_at, a.id LIMIT 1 RETURNING 1) SELECT count(*) FROM i")" || die "could not grant credits at $m to the user $tag through the ledger"
    [ "$n" = 1 ] || die "the grant at $m to the user $tag wrote $n ledger row(s), want 1 (platform.admins holds no active admin: stage 4b did not run)"
  done
  n="$(psql_su "WITH i AS (INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key) SELECT '$id'::uuid, 'm3', 'm3_enable', a.id, 'the e2e scope entitles its user', 'e2e-enable-m3-$id' FROM platform.admins a WHERE a.disabled_at IS NULL ORDER BY a.created_at, a.id LIMIT 1 RETURNING 1) SELECT count(*) FROM i")" || die "could not enable module 3 for the user $tag through the ledger"
  [ "$n" = 1 ] || die "the module 3 entitlement of the user $tag wrote $n ledger row(s), want 1"
  sink_ip="$(jobs_ip smtp-ctl)"
  case "$sink_ip" in [0-9]*.[0-9]*.[0-9]*.[0-9]*) ;; *) die "could not read the address of smtp-ctl on $PROJECT_NET (got '$sink_ip'): module 2's account names the sink's private address";; esac
  e2e_m2_call POST /v1/admin/pools "{\"kind\":\"dedicated\",\"owner_user_id\":\"$id\",\"name\":\"e2e $tag\"}" || true
  [ "$E2E_HTTP_CODE" = 201 ] || e2e_fail m2 "module 2 answered $E2E_HTTP_CODE to the pool of the user $tag (want 201): $(e2e_http_what)"
  pool="$(printf '%s' "$E2E_HTTP_BODY" | e2e_json_get id)"
  case "$pool" in ????????-????-????-????-????????????) ;; *) e2e_fail m2 "module 2's answer to the pool of the user $tag has no pool id (the stage reads a top-level id, as the scenario program does): $(e2e_http_what)";; esac
  e2e_m2_call POST /v1/admin/accounts "{\"pool_id\":\"$pool\",\"smtp_host\":\"$sink_ip\",\"smtp_port\":$E2E_SINK_SMTP_PORT,\"smtp_username\":\"e2e-$tag\",\"smtp_secret\":\"$SINK_PASS\",\"from_address\":\"sender-e2e-$tag@example.test\",\"from_name\":\"Harness\",\"daily_limit\":1000}" || true
  [ "$E2E_HTTP_CODE" = 201 ] || e2e_fail m2 "module 2 answered $E2E_HTTP_CODE to the sending account of the user $tag (want 201): $(e2e_http_what)"
  acct="$(printf '%s' "$E2E_HTTP_BODY" | e2e_json_get id)"
  [ -n "$acct" ] || e2e_fail m2 "module 2's answer to the sending account of the user $tag has no account id (the stage reads a top-level id): $(e2e_http_what)"
  printf -v "E2E_M2ACCOUNT_$tag" '%s' "$acct"
  echo "  user $tag: invited, a platform user, $E2E_CREDITS credits at module 1 and at module 2 (ledger), module 3 enabled (ledger row written, delivery awaited below), module 2 pool and sending account at the sink"
}

e2e_users() {
  STAGE=e2e-users; banner E7 "the users of the specs: an invite each (GoTrue's admin route), a platform user, credits at modules 1 and 2, module 3 enabled, a sending account at module 2"
  local tag i ok id k n
  [ -n "$E2E_SERVICE" ] || die "the service-role key was not minted: e2e_mint did not run (stage 2)"
  for tag in "${E2E_USER_TAGS[@]}"; do
    case " $E2E_FUNDED_TAGS " in *" $tag "*) e2e_user "$tag" 1;; *) e2e_user "$tag" 0;; esac
  done
  # module 3: the platform's reconcile loop (the leading instance) delivers each entitlement and reads it back from module 3's suspension route (rule 21): converged at the wanted version, observed active
  for tag in $E2E_FUNDED_TAGS; do
    id="E2E_ID_$tag"; id="${!id}"; i=0; ok=""
    while [ "$i" -lt 120 ]; do
      ok="$(psql_su "select converged_version = want_version and observed = 'active' and not want_suspended from platform.m3_sync where user_id = '$id'")" || true
      [ "$ok" = t ] && break
      i=$((i+1)); sleep 1
    done
    [ "$ok" = t ] || e2e_fail e2e-platform "the platform did not deliver module 3's entitlement of the user $tag within 120 s (platform.m3_sync is not converged and observed active): the leading instance's reconcile loop did not converge"
  done
  echo "  module 3's entitlement is delivered and observed active for every funded user"
  mkdir -p "$WORK/e2e-fixtures"; chmod 700 "$WORK/e2e-fixtures"
  # the values reach fixtures.mjs through the ENVIRONMENT of that one process (a subshell exports them; an argument would be visible to every process of the host)
  ( umask 077
    export E2E_TAGS="${E2E_USER_TAGS[*]}" E2E_APP_URL E2E_AUTH_URL E2E_ISSUER E2E_M3_SINK_HOST E2E_SINK_SMTP_PORT E2E_SINK_URL="http://smtp-ctl:8025"
    for tag in "${E2E_USER_TAGS[@]}"; do for k in ID EMAIL PW LINK FUNDED M2ACCOUNT; do n="E2E_${k}_$tag"; export "$n=${!n-}"; done; done
    node "$E2E_STAGE_DIR/fixtures.mjs" > "$WORK/e2e-fixtures/fixtures.json" 2> "$WORK/e2e-fixtures.err" ) \
    || { sed 's/^/     | /' "$WORK/e2e-fixtures.err" | head -12; die "the fixtures file could not be written (tests/harness/e2e/fixtures.mjs refused what the stage made; it names the variable, never a value)"; }
  [ -s "$WORK/e2e-fixtures/fixtures.json" ] || die "the fixtures file is empty"
  e2e_enforces_modes && { [ "$(e2e_mode_of "$WORK/e2e-fixtures/fixtures.json")" = 600 ] || die "the fixtures file is not mode 600: it holds the users' passwords and invite links"; }
  echo "  fixtures written: ${#E2E_USER_TAGS[@]} users (${E2E_USER_TAGS[*]}), mode 600, mounted read-only at /fixtures in the browser's container"
}

# ── E8: the browser ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# The secrets of this run in the services' logs and in the runner's own log: the infrastructure's (minted per run: GoTrue's secret and database password, the unsubscribe secret, the service-role key, the modules'
# credentials, the database passwords) never; the users' passwords and invite links never in a LOG (they are in the traces of the artifacts, of a stack that is torn down: web/e2e/README.md says so). One value per
# line, empty ones left out (an empty pattern matches every line).
e2e_infra_secrets() {
  local v
  for v in "${GT_JWT_SECRET:-}" "${GT_DB_PASS:-}" "${E2E_UNSUB:-}" "${E2E_SERVICE:-}" "${M2_TOKEN:-}" "${M3_TOKEN:-}" "${M3_ADMIN_KEY:-}" "${M2_ADMIN_KEY:-}" "${M2_WORKER_TOKEN:-}" "${M3_WORKER_TOKEN:-}" "${M3_WORKER2_TOKEN:-}" "${M3_WORKER3_TOKEN:-}" "${SINK_PASS:-}" "${JOBS_M1K:-}" "${JOBS_VENDOR:-}" "${PG_PASS:-}" "${PL_PASS:-}"; do
    if [ -n "$v" ]; then printf '%s\n' "$v"; fi
  done
}
e2e_user_secrets() {
  local tag n v
  for tag in "${E2E_USER_TAGS[@]}"; do
    for n in "E2E_PW_$tag" "E2E_LINK_$tag"; do v="${!n:-}"; if [ -n "$v" ]; then printf '%s\n' "$v"; fi; done
  done
}

# e2e_scan_log <file> <label>: red, naming the label and never showing the file, when one of this run's secrets (infrastructure or users') or anything shaped like a JWT is in it
e2e_scan_log() {
  local f="$1" label="$2" list="$WORK/e2e-scan-list.txt"
  ( umask 077; { e2e_infra_secrets; e2e_user_secrets; } > "$list" )
  [ "$(wc -l < "$list" | tr -d ' ')" -ge 10 ] || { rm -f "$list"; die "only $(wc -l < "$list" | tr -d ' ') secret(s) of this run are known to the scan of $label: it would look for almost nothing"; }
  # a hit sets E2E_NO_DUMP: run.sh's die then does not dump the services' logs (the line that holds the secret may be in one of them, and a workflow log is public)
  if grep -qFf "$list" "$f"; then rm -f "$list"; E2E_NO_DUMP=1; die "a secret of this run appears in $label (the text is not shown: it is public)"; fi
  rm -f "$list"
  if grep -qE "$E2E_JWT_SHAPE" "$f"; then E2E_NO_DUMP=1; die "something shaped like a JWT (an access token, a key) appears in $label (the text is not shown: it is public)"; fi
}

e2e_run_tests() {
  STAGE=e2e-browser; banner E8 "the specs, in Playwright's Chromium inside the private network (one worker, no retry; screenshots, traces and the reports under \$H_WORK/e2e-artifacts)"
  local listed rc keep
  mkdir -p "$WORK/e2e-artifacts"
  DC run --rm -T --no-deps e2e-runner node_modules/.bin/playwright test --list > "$WORK/e2e-list.log" 2>&1 || { sed 's/^/     | /' "$WORK/e2e-list.log" | tail -30; die "the specs could not be listed inside the Playwright image (web/playwright.config.ts or a spec does not load)"; }
  listed="$(sed -n 's/^Total: \([0-9][0-9]*\) test.*/\1/p' "$WORK/e2e-list.log" | tail -1)"
  case "$listed" in ''|*[!0-9]*) sed 's/^/     | /' "$WORK/e2e-list.log" | tail -10; die "the list of the specs has no 'Total: N tests' line: the instrument matched nothing";; esac
  [ "$listed" -ge "$E2E_MIN_TESTS" ] || die "$listed test(s) are listed, the floor of this scope is $E2E_MIN_TESTS (E2E_MIN_TESTS in tests/harness/e2e/stage.sh): a spec was lost, or the floor was raised before its specs landed"
  echo "  $listed test(s) are listed (floor $E2E_MIN_TESTS)"
  DC run --rm -T --no-deps e2e-runner node_modules/.bin/playwright test ${E2E_RUN_ARGS:-} > "$WORK/e2e-run.log" 2>&1; rc=$?
  # the runner's own text is read for secrets BEFORE it is printed (a workflow log is public), and so are the artifacts BEFORE the workflow uploads them (a hit empties the directory)
  e2e_scan_log "$WORK/e2e-run.log" "the browser run's own output"
  if [ "$rc" = 0 ]; then keep=60; else keep=300; fi
  sed 's/^/  | /' "$WORK/e2e-run.log" | tail -"$keep"
  [ "$rc" = 0 ] || [ -n "$(find "$WORK/e2e-artifacts" -type f 2>/dev/null | head -1)" ] || die "the browser run failed (exit $rc) before it wrote a single artifact (the output above says why: the browser did not start, or the config did not load)"
  e2e_scan_artifacts
  [ "$rc" = 0 ] || e2e_fail e2e-platform "the browser specs failed (exit $rc): what failed is above; the platform's log follows, the traces and screenshots are in the e2e-artifacts artifact"
  node "$E2E_STAGE_DIR/check-report.mjs" "$WORK/e2e-artifacts/results.json" "$E2E_MIN_TESTS" "$listed" || die "the browser run's own report does not prove the run (it is not the counts the list promised, or a test did not run)"
  e2e_require_shots
}

# e2e_scan_artifacts: the infrastructure's secrets are in no file of the artifacts directory, the unpacked traces and report data included; on a hit the directory is EMPTIED (the workflow uploads it whatever happens)
e2e_scan_artifacts() {
  local d="$WORK/e2e-artifacts" list="$WORK/e2e-artifacts-secrets.txt" n z=0 f hit shots
  [ -d "$d" ] || die "the artifacts directory $d does not exist"
  n="$(find "$d" -type f | wc -l | tr -d ' ')"
  [ "$n" -ge 1 ] || die "the artifacts directory is empty after the run: neither a report, a screenshot nor a trace was written (the instrument matched nothing)"
  ( umask 077; e2e_infra_secrets > "$list" )
  [ "$(wc -l < "$list" | tr -d ' ')" -ge 10 ] || { rm -f "$list"; die "only $(wc -l < "$list" | tr -d ' ') infrastructure secret(s) are known to the artifact scan: it would look for almost nothing"; }
  hit="$(grep -rIlFf "$list" "$d" 2>/dev/null | head -1)"
  if [ -n "$hit" ]; then rm -f "$list"; find "$d" -mindepth 1 -delete 2>/dev/null; die "a secret of this run's infrastructure appears in the artifact ${hit#"$d"/} (not shown; the artifacts directory was emptied so that nothing is uploaded)"; fi
  while IFS= read -r f; do
    z=$((z+1))
    command -v unzip >/dev/null 2>&1 || { rm -f "$list"; die "unzip is not installed: the traces (zip files) cannot be scanned for secrets, and an unscanned trace is not uploaded"; }
    if unzip -p "$f" 2>/dev/null | grep -qFf "$list"; then rm -f "$list"; find "$d" -mindepth 1 -delete 2>/dev/null; die "a secret of this run's infrastructure appears inside the zip ${f#"$d"/} (not shown; the artifacts directory was emptied so that nothing is uploaded)"; fi
  done < <(find "$d" -type f -name '*.zip')
  rm -f "$list"
  shots="$(find "$d/shots" -type f -name '*.png' 2>/dev/null | wc -l | tr -d ' ')"
  echo "  artifacts: $n file(s), $z zip(s) unpacked and read, $shots screenshot(s); no secret of this run's infrastructure is in any of them (the test users' passwords and tokens are, by design, in a trace of a stack that is torn down)"
}

# e2e_require_shots: the pictures the specs took (web/e2e/support/shots.ts writes one PNG per main screen into E2E_SHOTS_DIR) are at least the floor: a helper that wrote nothing is red, a gallery of zero is not a gallery
e2e_require_shots() {
  local shots
  shots="$(find "$WORK/e2e-artifacts/shots" -type f -name '*.png' 2>/dev/null | wc -l | tr -d ' ')"
  [ "$shots" -ge "$E2E_MIN_SHOTS" ] || die "$shots screenshot(s) in $WORK/e2e-artifacts/shots, the floor is $E2E_MIN_SHOTS: the specs took no picture (web/e2e/support/shots.ts), or E2E_SHOTS_DIR is not where the stage reads"
  echo "  $shots screenshot(s) in the gallery (floor $E2E_MIN_SHOTS)"
}

# ── E9: the evidence ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# the logs of every service of the scope (the containers that are running now), read for this run's secrets and for anything shaped like a JWT; the run's own output was read at E8
e2e_secret_leak() {
  STAGE=e2e-leak
  local svc f=0
  [ -n "$E2E_UNSUB" ] && [ -n "$GT_JWT_SECRET" ] || die "the secrets of this run are empty: the leak scan would look for nothing"
  for svc in "${E2E_SERVICES[@]}"; do
    [ "$svc" = postgres ] && continue
    DC logs --no-color "$svc" > "$WORK/e2e-leak-$svc.log" 2>&1 || die "could not read the log of $svc"
    case "$svc" in gotrue|e2e-platform|m1j|m2|m3) [ -s "$WORK/e2e-leak-$svc.log" ] || die "the log of $svc, which the leak check must read, is empty: the instrument matched nothing";; esac
    e2e_scan_log "$WORK/e2e-leak-$svc.log" "the log of $svc"
    f=$((f+1)); rm -f "$WORK/e2e-leak-$svc.log"
  done
  echo "  none of this run's secrets (the infrastructure's and the users'), nor anything shaped like a JWT, appears in the logs of $f services or in the browser run's own output"
}

e2e_exposure() {
  STAGE=exposure; banner 8 "scope e2e: no service publishes a port; the network is internal"
  local ids id pb cnt=0
  DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps.json" "${E2E_SERVICES[@]}" || die "a service is reachable from outside the network"
  [ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
  ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
  for id in $ids; do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"
}

# ── the scope ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# H_SCOPE=e2e: Postgres, the four migration chains, the install step after module 1's and the first admin (stages 1-4b ran), then the stages above and nothing else. It never returns (it exits with the run's own
# status; the EXIT trap tears the stack down).
e2e_scope_run() {
  STAGE=services; banner E0 "scope e2e: a real browser, a real GoTrue, modules 1, 2 and 3 with their workers, the sink and ONE platform instance serving the app and the API from one origin (nothing is published)"
  # the host directories the compose file bind-mounts are made HERE, by the run's user, before any container is started: a bind mount of a path that does not exist is created by Docker as a directory of root,
  # which the run's user could neither fill (the fixtures) nor chmod (the first runner container is the preflight, long before the users exist)
  mkdir -p "$WORK/e2e-artifacts" "$WORK/e2e-fixtures"; chmod 700 "$WORK/e2e-fixtures"
  e2e_web
  gotrue_stage
  e2e_m1
  e2e_m2
  e2e_m3
  e2e_platform
  e2e_users
  e2e_run_tests
  e2e_secret_leak
  e2e_exposure
  STAGE=done
  echo
  echo "================ NOT CHECKED (scope e2e: what this run did not and could not vouch for)"
  e2e_not_checked
  echo
  echo "HARNESS RESULT: GREEN (scope e2e: $(sed -n 's/^Total: \([0-9][0-9]*\) test.*/\1/p' "$WORK/e2e-list.log" | tail -1) browser tests passed in Chromium against the real stack; $(find "$WORK/e2e-artifacts/shots" -type f -name '*.png' 2>/dev/null | wc -l | tr -d ' ') screenshot(s) in the e2e-artifacts artifact)"
  exit 0
}

e2e_not_checked() {
  cat <<'EOF'
  - ONE browser engine: Chromium, the build the pinned Playwright image carries. Firefox, WebKit, Safari, mobile browsers and any other Chromium version are not exercised, and neither is a viewport other than Playwright's desktop default.
  - TLS is a per-run SELF-SIGNED certificate on an nginx sidecar, and the browser is told to ignore the chain (ignoreHTTPSErrors): the public edge, a real certificate chain, HSTS, HTTP/2 and the client address behind a real proxy are Phase 8's.
    The platform's own code serves plain http behind it (it has no TLS of its own), as it will in production behind the edge.
  - GoTrue sends NO mail here (GOTRUE_SMTP_HOST is empty): the invite is minted by the admin route and its link opened by the spec, which is how Phase 7's invite sender will hand it over; the recovery ("forgot password") mail and every other
    GoTrue mail are never sent, so a link in one is never followed. GoTrue's asymmetric keys and JWKS endpoint, MFA, SSO and social logins are not exercised (tests/harness/gotrue/stage.sh says the rest).
  - the fixtures are a handful of users (four) with large balances: a user with a low balance is made by a spec, not by the stage; a real customer's volume (lists near the engine's limit, hundreds of jobs, hours-long runs) is not exercised.
  - module 1 answers from a FIXTURE DNS and an unwired SMTP stage (names under .test: NXDOMAIN is invalid/no_mx, free.example.test has an MX and is risky/plausible, provider.example.test is routed to the vendor stand-in): a valid, a catch-all,
    a greylisted or a cached answer never occurs. The vendor is module 1's own ZeroBounce STAND-IN, never the real service.
  - modules 2 and 3 send to the CONTROLLABLE SINK (and Mailpit): never a real mailbox provider, never a bounce that comes back later, a spam folder, a reply or an open; the From-gate answers from a fixture DNS (tests/harness/m3adapter/m3dns.json:
    example.test is aligned, a name outside the fixture is not).
  - the unsubscribe link is opened by the browser at the platform's https name behind the edge: how a recipient's mail client, a provider's one-click POST or a link scanner reaches it is not exercised.
  - the traces of a FAILED test hold the test users' passwords and tokens (Playwright records every request and every typed value): they belong to a stack that is torn down when the run ends, and the stage reads every artifact for the
    infrastructure's secrets (the JWT secret, the service-role key, the unsubscribe secret, the modules' credentials, the database passwords) and empties the directory on a hit; it cannot read a PNG's pixels.
  - the specs run with ONE worker and no retry: a race that only two browsers at once would show is not exercised; accessibility is checked by axe-core (WCAG 2 A and AA rules, contrast included) on the screens the specs open, which is an automated
    subset of what a person with assistive technology would find.
EOF
  echo "NOT CHECKED: any browser but Chromium, a real certificate and the public edge, GoTrue's mail flow, a real mailbox provider, real DNS, a large account, a second worker, what automated accessibility rules cannot see"
}
