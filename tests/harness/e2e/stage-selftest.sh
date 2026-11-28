#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/e2e/stage.sh (the browser end-to-end scope) go RED, for the reason they name, and stay green on a good input; and that the files of the scope agree with each
# other (the compose file, the TLS edge's configuration, the names written in four places, the dials of the platform against the Go code that reads them, the pinned versions of Playwright, the wiring in run.sh and the
# workflows, the specs against the floor). No Docker, no browser, no network: the functions are sourced and run against stand-ins for docker compose, psql and the services; the small node programs (jwt.mjs,
# check-report.mjs, fixtures.mjs) and openssl run for real. What it covers:
#   jwt.mjs             a token that verifies under HS256 with the secret and under no other, with the claims GoTrue's admin routes and the anon key need; refuses a missing or short secret, an unknown role and a bad ttl
#   e2e_mint            two lines (a 64-character secret and a three-segment anon key) appended to the env file, every secret masked BEFORE it is written or used, none printed, four distinct passwords; red for a tool that
#                       printed nothing, a secret of the wrong length or equal to another, no GoTrue secret, no env file, and (where modes are kept) an env file that is not mode 600
#   e2e_tls             the certificate carries both names, is not about to expire and pairs with its key; red for a certificate that lacks either name, one that expires within the hour, a key that is not its key, a tool that fails
#   check-report.mjs    green on a run that did what it listed; red, for its reason, on a failed, flaky or skipped test, a floor not met, a count that is not the list's, an empty, missing or foreign file
#   fixtures.mjs        writes the document the specs read; red, naming the variable and never a value, for every rule it holds
#   e2e_scan_log        every secret of the run (infrastructure and users), one case each, and anything shaped like a JWT, is red and never shown; near misses are green; too few secrets known is red
#   e2e_scan_artifacts  clean artifacts are green; a secret in a text file or inside a zip is red, never shown, and EMPTIES the directory; an unreadable zip, an empty directory and too few secrets are red
#   e2e_require_shots, e2e_secret_leak, e2e_run_tests, e2e_user, e2e_users, e2e_m2, e2e_m3, e2e_web, e2e_platform  against stand-ins: every sentence they die with is reached
#   consistency         the compose file (digest-pinned images, no port, the profile, hardening, the runner holds no secret), the names in nginx.conf, the compose aliases and the stage, the dials of e2e-platform
#                       against internal/config, the Playwright image tag against web/package.json and the lockfile, the specs against the floor, run.sh (dormant in `full`, E2E_ON evaluated for six settings), the workflows
# NOT covered here (they need a Linux host with Docker): the real docker compose, nginx, GoTrue, the modules, the platform image and Chromium itself; e2e_scope_run, e2e_m1 and e2e_exposure run only there.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"; ROOT="$(cd "$HARNESS/../.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=300; cases=0; ok=0; bad=0
verdict() { # label want-rc want-substring out rc [forbidden-substring]
  local label="$1" want="$2" sub="$3" out="$4" rc="$5" forbid="${6:-}" n f=0
  cases=$((cases+1))
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ -n "$forbid" ] && printf '%s\n' "$out" | grep -q -F -- "$forbid"; then f=1; fi
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ] && [ "$f" -eq 0 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), substring '$sub' found $n times${forbid:+, forbidden text present: $f}"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
rnd() { command openssl rand -hex "$1"; }   # `command`: the stand-ins below replace the function openssl, and this must stay the real tool
b64u() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '+/' '-_'; }
FAKE_JWT="$(b64u '{"alg":"HS256","typ":"JWT"}').$(b64u '{"sub":"stand-in","role":"authenticated"}').$(b64u 'a-signature-of-some-length')"   # built here, never stored: a token-shaped string must not sit in a committed file
# hs256_ok <token> <secret>: ok when the token's signature is the HMAC-SHA256 of its first two segments under the secret (an independent computation: openssl, not the program under test)
hs256_ok() { local h p s sig; IFS=. read -r h p s <<< "$1"; sig="$(printf '%s' "$h.$p" | openssl dgst -sha256 -hmac "$2" -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"; [ -n "$s" ] && [ "$sig" = "$s" ] && echo ok || echo bad; }
claim() { # <token> <member>: one member of the payload
  node -e 'const p=JSON.parse(Buffer.from(process.argv[1].split(".")[1],"base64url").toString());process.stdout.write(String(p[process.argv[2]]))' "$1" "$2"
}

# ── jwt.mjs ──
JWT="$HERE/jwt.mjs"
jwt_run() { ( E2E_JWT_SECRET="$1" node "$JWT" "${@:2}" ) 2>&1; }
SEC="$(rnd 48)"
tok="$(jwt_run "$SEC" anon 3600)"; rc=$?
verdict "jwt.mjs: a token for the role anon" 0 "three-segments" "$([ "$(printf '%s' "$tok" | tr -cd . | wc -c | tr -d ' ')" = 2 ] && echo three-segments || echo "$tok")" "$rc"
verdict "jwt.mjs: its signature is the HMAC-SHA256 of its segments under the secret (computed by openssl)" 0 "ok" "$(hs256_ok "$tok" "$SEC")" 0
verdict "jwt.mjs: CONTROL: the same token does not verify under another secret" 0 "bad" "$(hs256_ok "$tok" "$(rnd 48)")" 0
verdict "jwt.mjs: the header says HS256" 0 "HS256" "$(node -e 'process.stdout.write(Buffer.from(process.argv[1].split(".")[0],"base64url").toString())' "$tok")" 0
verdict "jwt.mjs: the role claim is anon" 0 "anon" "$(claim "$tok" role)" 0
verdict "jwt.mjs: the issuer claim is the one GoTrue accepted for its admin routes" 0 "supabase" "$(claim "$tok" iss)" 0
verdict "jwt.mjs: the lifetime is the ttl asked for" 0 "3600" "$(node -e 'const p=JSON.parse(Buffer.from(process.argv[1].split(".")[1],"base64url").toString());process.stdout.write(String(p.exp-p.iat))' "$tok")" 0
svc="$(jwt_run "$SEC" service_role 14400)"
verdict "jwt.mjs: a token for the role service_role" 0 "service_role" "$(claim "$svc" role)" 0
out="$(jwt_run "" anon 3600)"; rc=$?
verdict "jwt.mjs: no secret is refused" 2 "E2E_JWT_SECRET is missing or shorter than 32 characters" "$out" "$rc"
out="$(jwt_run "short" anon 3600)"; rc=$?
verdict "jwt.mjs: a short secret is refused" 2 "shorter than 32 characters" "$out" "$rc"
out="$(jwt_run "$SEC" admin 3600)"; rc=$?
verdict "jwt.mjs: a role that is neither anon nor service_role is refused" 2 "the role must be one of anon, service_role (got 'admin')" "$out" "$rc"
out="$(jwt_run "$SEC" anon 5)"; rc=$?
verdict "jwt.mjs: a ttl under a minute is refused" 2 "the ttl must be a whole number of seconds" "$out" "$rc"
out="$(jwt_run "$SEC" anon soon)"; rc=$?
verdict "jwt.mjs: a ttl that is not a number is refused" 2 "the ttl must be a whole number of seconds" "$out" "$rc"
case "$out" in *"$SEC"*) leaked=yes;; *) leaked=no;; esac
verdict "jwt.mjs: a refusal never shows the secret" 0 "no" "$leaked" 0

# ── the stand-ins of the stage ──
cat > "$T/env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
MASKLOG="$WORK/mask.log"
mask() { if [ -f "$WORK/harness.env" ] && grep -qF -- "$1" "$WORK/harness.env"; then echo "AFTER_WRITE" >> "$MASKLOG"; else echo "BEFORE_WRITE" >> "$MASKLOG"; fi; }
DC() { echo "DC called: $*" >&2; return 1; }
sleep() { :; }
STAGE=selftest
EOF

# ── e2e_mint ──
mint_run() { # mode: good | empty | short | nogotrue | noenv | mode644 | mode600 | samejwt
  W="$T/w-$1"; rm -rf "$W"; mkdir -p "$W"; ( umask 077; : > "$W/harness.env" ); [ "$1" != noenv ] || rm -f "$W/harness.env"
  ( set -u; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    GT_JWT_SECRET="$SEC"; GT_DB_PASS="$(rnd 16)"; JOBS_JWT="$(rnd 32)"; JOBS_M1K="$(rnd 32)"; JOBS_VENDOR="$(rnd 32)"
    [ "$1" != nogotrue ] || GT_JWT_SECRET=""
    case "$1" in
      empty)   openssl() { :; };;
      short)   openssl() { rnd 5; };;
      samejwt) openssl() { printf '%s\n' "$JOBS_JWT"; };;
      *)       openssl() { rnd "$3"; };;
    esac
    case "$1" in
      mode644) e2e_enforces_modes() { return 0; }; e2e_mode_of() { echo 644; };;
      mode600) e2e_enforces_modes() { return 0; }; e2e_mode_of() { echo 600; };;
      *)       e2e_enforces_modes() { return 1; };;
    esac
    e2e_mint; for t in a b c s; do n="E2E_PW_$t"; printf '%s\n' "${!n}" >> "$WORK/pw.txt"; done; echo "KEYS: ${#E2E_UNSUB} ${#E2E_ANON} ${#E2E_SERVICE}" ) 2>&1
}
out="$(mint_run good)"; rc=$?
verdict "e2e_mint: a good mint" 0 "masked, none printed" "$out" "$rc"
env="$T/w-good/harness.env"
verdict "e2e_mint: exactly two H_ lines in the env file, one of each name" 0 "two" "$([ "$(grep -c '^H_' "$env")" = 2 ] && [ "$(grep -c '^H_E2E_UNSUBSCRIBE_SECRET=' "$env")" = 1 ] && [ "$(grep -c '^H_E2E_ANON_KEY=' "$env")" = 1 ] && echo two || echo "$(grep -c '^H_' "$env") lines")" 0
usec="$(sed -n 's/^H_E2E_UNSUBSCRIBE_SECRET=//p' "$env")"; anon="$(sed -n 's/^H_E2E_ANON_KEY=//p' "$env")"
verdict "e2e_mint: the unsubscribe secret is 64 hexadecimal characters" 0 "hex64" "$(printf '%s\n' "$usec" | grep -qE '^[0-9a-f]{64}$' && echo hex64 || echo "$usec")" 0
verdict "e2e_mint: the unsubscribe secret is not GoTrue's" 0 "different" "$([ "$usec" != "$SEC" ] && echo different || echo same)" 0
verdict "e2e_mint: the anon key verifies under GoTrue's secret (computed by openssl)" 0 "ok" "$(hs256_ok "$anon" "$SEC")" 0
verdict "e2e_mint: the anon key has the role anon" 0 "anon" "$(claim "$anon" role)" 0
verdict "e2e_mint: all seven secrets were masked BEFORE they were written or used" 0 "7 before, 0 after" "$(printf '%s before, %s after' "$(grep -c BEFORE_WRITE "$T/w-good/mask.log")" "$(grep -c AFTER_WRITE "$T/w-good/mask.log" || true)")" 0
verdict "e2e_mint: the two keys and the unsubscribe secret are kept for the leak scans" 0 "KEYS: 64" "$out" "$rc"
verdict "e2e_mint: four distinct 24-character passwords" 0 "four-distinct" "$([ "$(sort -u "$T/w-good/pw.txt" | wc -l | tr -d ' ')" = 4 ] && [ "$(awk '{ if (length($0) != 24) bad=1 } END { print bad+0 }' "$T/w-good/pw.txt")" = 0 ] && echo four-distinct || echo wrong)" 0
leaked=no; for v in "$usec" "$anon" $(cat "$T/w-good/pw.txt"); do [ -z "$v" ] || case "$out" in *"$v"*) leaked=yes;; esac; done
verdict "e2e_mint: the output never shows a secret, a key or a password" 0 "no" "$leaked" 0
out="$(mint_run empty)"; rc=$?
verdict "e2e_mint: a tool that printed nothing is refused" 1 "could not mint the e2e scope's unsubscribe secret" "$out" "$rc"
out="$(mint_run short)"; rc=$?
verdict "e2e_mint: a secret of the wrong length is refused" 1 "openssl rand printed 10 characters, want 64" "$out" "$rc"
out="$(mint_run samejwt)"; rc=$?
verdict "e2e_mint: a secret equal to another credential of the run is refused" 1 "equals another secret of this run" "$out" "$rc"
out="$(mint_run nogotrue)"; rc=$?
verdict "e2e_mint: without GoTrue's secret nothing is signed" 1 "GoTrue's JWT secret was not minted" "$out" "$rc"
out="$(mint_run noenv)"; rc=$?
verdict "e2e_mint: no env file" 1 "the env file is missing" "$out" "$rc"
out="$(mint_run mode644)"; rc=$?
verdict "e2e_mint: an env file that is mode 644 (where modes are kept)" 1 "is mode 644, expected 600" "$out" "$rc"
out="$(mint_run mode600)"; rc=$?
verdict "e2e_mint: the same host with a mode-600 env file is green (the control of the case above)" 0 "masked, none printed" "$out" "$rc"

# ── e2e_tls (openssl runs for real) ──
mkcert() { ( cd "$1" && MSYS_NO_PATHCONV=1 openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -keyout key -out crt -subj "/CN=$2" -addext "subjectAltName=$3" >/dev/null 2>&1 ); }   # <dir> <common name> <alt names>: relative names, no path rewriting of /CN= (Git Bash on Windows)
tls_run() { # mode: good | fail | noauth | noapp | expired | mismatch
  W="$T/tls-$1"; rm -rf "$W"; mkdir -p "$W"
  P="$T/prep-$1"; rm -rf "$P"; mkdir -p "$P"
  case "$1" in
    noauth)   mkcert "$P" app.harness.test "DNS:app.harness.test";;
    noapp)    mkcert "$P" auth.harness.test "DNS:auth.harness.test";;
    expired)  mkcert "$P" app.harness.test "DNS:app.harness.test,DNS:auth.harness.test";;   # a good certificate; the stand-in below answers the expiry check as an expiring one would
    mismatch) mkcert "$P" app.harness.test "DNS:app.harness.test,DNS:auth.harness.test"; ( cd "$P" && openssl ecparam -name prime256v1 -genkey -noout -out key >/dev/null 2>&1 );;
  esac
  ( set -u; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    e2e_enforces_modes() { return 1; }
    case "$1" in
      fail) openssl() { echo "openssl: boom"; return 1; };;
      good) ;;
      *)    MODE="$1"; openssl() { if [ "$1" = req ]; then while [ $# -gt 0 ]; do case "$1" in -keyout) cp "$P/key" "$2";; -out) cp "$P/crt" "$2";; esac; shift; done; return 0; fi
              if [ "$MODE" = expired ]; then case " $* " in *" -checkend "*) return 1;; esac; fi
              command openssl "$@"; };;
    esac
    e2e_tls; echo "RC: $?"; ls "$WORK/e2e/tls" 2>/dev/null | tr '\n' ' ' ) 2>&1
}
out="$(tls_run good)"; rc=$?
verdict "e2e_tls: a good certificate" 0 "TLS: a per-run self-signed certificate (ECDSA P-256, 2 days) for app.harness.test and auth.harness.test" "$out" "$rc"
verdict "e2e_tls: ... the directory holds the key and the certificate" 0 "tls.crt tls.key" "$out" "$rc"
verdict "e2e_tls: ... and the certificate carries both names as subject alternative names (read back by openssl)" 0 "both-names" "$(t="$(openssl x509 -in "$T/tls-good/e2e/tls/tls.crt" -noout -text | grep -A1 'Subject Alternative Name')"; printf '%s' "$t" | grep -q 'DNS:app.harness.test' && printf '%s' "$t" | grep -q 'DNS:auth.harness.test' && echo both-names || echo "$t")" 0
verdict "e2e_tls: ... and it is not a certificate authority (CA:FALSE)" 0 "leaf" "$(openssl x509 -in "$T/tls-good/e2e/tls/tls.crt" -noout -text | grep -q 'CA:FALSE' && echo leaf || echo other)" 0
out="$(tls_run fail)"; rc=$?
verdict "e2e_tls: a tool that fails" 1 "openssl could not make the run's self-signed certificate" "$out" "$rc"
out="$(tls_run noauth)"; rc=$?
verdict "e2e_tls: a certificate without the GoTrue name" 1 "does not carry the name auth.harness.test" "$out" "$rc"
out="$(tls_run noapp)"; rc=$?
verdict "e2e_tls: a certificate without the platform's name" 1 "does not carry the name app.harness.test" "$out" "$rc"
out="$(tls_run expired)"; rc=$?
verdict "e2e_tls: a certificate that expires within the hour" 1 "the certificate expires within the hour" "$out" "$rc"
out="$(tls_run mismatch)"; rc=$?
verdict "e2e_tls: a key that is not the certificate's" 1 "the certificate and its key are not a pair" "$out" "$rc"

# ── check-report.mjs ──
CR="$HERE/check-report.mjs"
report() { # <expected> <unexpected> <flaky> <skipped> -> a Playwright-shaped report on stdout
  node -e 'const [e,u,f,s]=process.argv.slice(1).map(Number);const specs=Array.from({length:e+u+f+s},(_,i)=>({title:"t"+(i+1),tests:[{status:i<e?"expected":i<e+u?"unexpected":i<e+u+f?"flaky":"skipped"}]}));console.log(JSON.stringify({stats:{expected:e,unexpected:u,flaky:f,skipped:s},suites:[{title:"smoke.spec.ts",specs}]}))' "$@"
}
cr() { node "$CR" "$@" 2>&1; }
report 10 0 0 0 > "$T/r-good.json"
out="$(cr "$T/r-good.json" 10 10)"; rc=$?
verdict "check-report: ten listed, ten ran, ten passed" 0 "check-report: OK" "$out" "$rc"
verdict "check-report: ... the counts and the titles are printed" 0 "10 passed, 0 failed, 0 flaky, 0 skipped, 10 test(s) named in the report" "$out" "$rc"
report 9 1 0 0 > "$T/r-fail.json"; out="$(cr "$T/r-fail.json" 10 10)"; rc=$?
verdict "check-report: a failed test" 1 "RED - 1 test(s) failed" "$out" "$rc"
report 9 0 1 0 > "$T/r-flaky.json"; out="$(cr "$T/r-flaky.json" 10 10)"; rc=$?
verdict "check-report: a test that passed only on a retry" 1 "passed only on a retry (flaky)" "$out" "$rc"
report 9 0 0 1 > "$T/r-skip.json"; out="$(cr "$T/r-skip.json" 9 10)"; rc=$?
verdict "check-report: a skipped test" 1 "were skipped: a test that cannot run FAILS, it never skips" "$out" "$rc"
report 4 0 0 0 > "$T/r-few.json"; out="$(cr "$T/r-few.json" 10)"; rc=$?
verdict "check-report: fewer tests than the floor" 1 "only 4 test(s) ran, the floor is 10" "$out" "$rc"
out="$(cr "$T/r-good.json" 10 12)"; rc=$?
verdict "check-report: a listed spec that did not run" 1 "12 test(s) were listed and 10 ran" "$out" "$rc"
: > "$T/r-empty.json"; out="$(cr "$T/r-empty.json" 10)"; rc=$?
verdict "check-report: an empty file" 1 "is empty: the run wrote no report" "$out" "$rc"
out="$(cr "$T/r-missing.json" 10)"; rc=$?
verdict "check-report: a missing file" 1 "cannot be read as a report" "$out" "$rc"
printf '{"hello":"world"}' > "$T/r-foreign.json"; out="$(cr "$T/r-foreign.json" 10)"; rc=$?
verdict "check-report: a JSON file that is not a report" 1 "it is not a Playwright JSON report" "$out" "$rc"
printf 'not json' > "$T/r-garbage.json"; out="$(cr "$T/r-garbage.json" 10)"; rc=$?
verdict "check-report: a file that is not JSON" 1 "cannot be read as a report" "$out" "$rc"
out="$(cr "$T/r-good.json" 0)"; rc=$?
verdict "check-report: a floor of zero is refused" 2 "usage: check-report.mjs" "$out" "$rc"

# ── fixtures.mjs ──
FX="$HERE/fixtures.mjs"
UUID_A="11111111-1111-4111-8111-111111111111"; UUID_B="22222222-2222-4222-8222-222222222222"; UUID_S="33333333-3333-4333-8333-333333333333"
LINK="https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https%3A%2F%2Fapp.harness.test"
fx_env() { # prints the variables of a good fixture set, one per line, for a subshell to export
  cat <<EOF
E2E_TAGS=a b s
E2E_APP_URL=https://app.harness.test
E2E_AUTH_URL=https://auth.harness.test/auth/v1
E2E_ISSUER=https://auth.harness.test/auth/v1
E2E_SINK_URL=http://smtp-ctl:8025
E2E_M3_SINK_HOST=10.smtp-ctl.example.test
E2E_SINK_SMTP_PORT=1025
E2E_ID_a=$UUID_A
E2E_EMAIL_a=e2e-a@example.test
E2E_PW_a=aaaaaaaaaaaaaaaaaaaaaaaa
E2E_LINK_a=$LINK
E2E_FUNDED_a=1
E2E_M2ACCOUNT_a=acct-a
E2E_ID_b=$UUID_B
E2E_EMAIL_b=e2e-b@example.test
E2E_PW_b=bbbbbbbbbbbbbbbbbbbbbbbb
E2E_LINK_b=$LINK
E2E_FUNDED_b=1
E2E_M2ACCOUNT_b=acct-b
E2E_ID_s=$UUID_S
E2E_EMAIL_s=e2e-s@example.test
E2E_PW_s=ssssssssssssssssssssssss
E2E_LINK_s=$LINK
E2E_FUNDED_s=0
E2E_M2ACCOUNT_s=
EOF
}
fx_run() { # <sed expression applied to the variable lines, or empty> -> output and status of fixtures.mjs
  ( set -u; while IFS= read -r l; do export "${l%%=*}=${l#*=}"; done < <(fx_env | sed "${1:-s/^X//}"); node "$FX" 2>&1 )
}
out="$(fx_run)"; rc=$?
verdict "fixtures.mjs: a good set writes the document" 0 '"version": 1' "$out" "$rc"
verdict "fixtures.mjs: ... valid JSON with the three users, the urls and no stray member" 0 "shape-ok" "$(printf '%s' "$out" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const j=JSON.parse(s);const u=Object.keys(j.users).join(",");console.log(Object.keys(j).join(",")==="version,urls,users"&&u==="a,b,s"&&j.users.a.funded===true&&j.users.s.funded===false&&j.users.s.m2_account_id===null&&j.users.a.m2_account_id==="acct-a"&&j.urls.sink_smtp_port===1025?"shape-ok":s)})')" 0
out="$(fx_run 's/^E2E_ID_a=.*/E2E_ID_a=not-a-uuid/')"; rc=$?
verdict "fixtures.mjs: an id that is not a uuid" 1 "E2E_ID_a: must be a lower-case uuid" "$out" "$rc"
out="$(fx_run '/^E2E_PW_b=/d')"; rc=$?
verdict "fixtures.mjs: a missing password names the variable" 1 "E2E_PW_b: is not set" "$out" "$rc"
out="$(fx_run 's/^E2E_PW_a=.*/E2E_PW_a=short/')"; rc=$?
verdict "fixtures.mjs: a password under 16 characters" 1 "E2E_PW_a: must be at least 16 characters" "$out" "$rc"
case "$out" in *short*) leaked=yes;; *) leaked=no;; esac
verdict "fixtures.mjs: ... and a refusal never shows the value" 0 "no" "$leaked" 0
out="$(fx_run 's#^E2E_LINK_a=.*#E2E_LINK_a=http://gotrue:9999/verify?token=a1\&type=invite#')"; rc=$?
verdict "fixtures.mjs: an invite link that is not under the public GoTrue address" 1 "E2E_LINK_a: must be an invite link under https://auth.harness.test/auth/v1/verify" "$out" "$rc"
out="$(fx_run 's/type=invite/type=recovery/')"; rc=$?
verdict "fixtures.mjs: a link that is not an invite" 1 "must be an invite link" "$out" "$rc"
out="$(fx_run "s/^E2E_ID_b=.*/E2E_ID_b=$UUID_A/")"; rc=$?
verdict "fixtures.mjs: two users with one id" 1 "equals another user's id" "$out" "$rc"
out="$(fx_run 's/^E2E_PW_b=.*/E2E_PW_b=aaaaaaaaaaaaaaaaaaaaaaaa/')"; rc=$?
verdict "fixtures.mjs: two users with one password" 1 "equals another user's password" "$out" "$rc"
out="$(fx_run 's/^E2E_M2ACCOUNT_a=.*/E2E_M2ACCOUNT_a=/')"; rc=$?
verdict "fixtures.mjs: a funded user with no sending account at module 2" 1 "E2E_M2ACCOUNT_a: is empty for a funded user" "$out" "$rc"
out="$(fx_run 's/^E2E_M2ACCOUNT_s=.*/E2E_M2ACCOUNT_s=acct-s/')"; rc=$?
verdict "fixtures.mjs: an unfunded user with a sending account" 1 "E2E_M2ACCOUNT_s: is set for a user who is not funded" "$out" "$rc"
out="$(fx_run 's/^E2E_FUNDED_s=.*/E2E_FUNDED_s=1/; s/^E2E_M2ACCOUNT_s=.*/E2E_M2ACCOUNT_s=acct-s/')"; rc=$?
verdict "fixtures.mjs: no user without credits (the error-state specs would have nobody to sign in as)" 1 "at least one funded user and one user without credits" "$out" "$rc"
out="$(fx_run 's/^E2E_FUNDED_a=.*/E2E_FUNDED_a=yes/')"; rc=$?
verdict "fixtures.mjs: a funded flag that is not 1 or 0" 1 "E2E_FUNDED_a: must be 1 or 0" "$out" "$rc"
out="$(fx_run 's#^E2E_AUTH_URL=.*#E2E_AUTH_URL=http://auth.harness.test/auth/v1#')"; rc=$?
verdict "fixtures.mjs: a plain-http GoTrue address" 1 "E2E_AUTH_URL: must be a https address" "$out" "$rc"
out="$(fx_run 's/^E2E_TAGS=.*/E2E_TAGS=/')"; rc=$?
verdict "fixtures.mjs: no user at all" 2 "E2E_TAGS names no user" "$out" "$rc"
out="$(fx_run 's/^E2E_TAGS=.*/E2E_TAGS=a B s/')"; rc=$?
verdict "fixtures.mjs: a tag that is not lower case" 1 "the tag 'B' must be lower case letters and digits" "$out" "$rc"

# ── the stand-ins of the secrets of a run, and of the services ──
SECRETVARS="GT_JWT_SECRET GT_DB_PASS E2E_UNSUB E2E_SERVICE M2_TOKEN M3_TOKEN M3_ADMIN_KEY M2_ADMIN_KEY M2_WORKER_TOKEN M3_WORKER_TOKEN M3_WORKER2_TOKEN M3_WORKER3_TOKEN SINK_PASS JOBS_M1K JOBS_VENDOR PG_PASS PL_PASS"
USERVARS="E2E_PW_a E2E_PW_b E2E_PW_c E2E_PW_s E2E_LINK_a E2E_LINK_b E2E_LINK_c E2E_LINK_s"
setsecrets() { local v; for v in $SECRETVARS; do printf -v "$v" '%s' "$(rnd 20)"; done; for v in $USERVARS; do printf -v "$v" '%s' "$(rnd 16)"; done; }

# ── e2e_scan_log: one case per secret ──
cat > "$T/scan-env.sh" <<'EOF'
die() { echo "DIE: $*"; echo "NO_DUMP=${E2E_NO_DUMP:-0}"; exit 1; }
STAGE=selftest
EOF
scan_run() { # <text of the log> [variable to blank out, or "all"] [secret variable whose value is appended to the text]
  W="$T/sl"; rm -rf "$W"; mkdir -p "$W"; : > "$W/used.txt"
  ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets
    text="$1"; [ -z "${3:-}" ] || { text="$text ${!3}"; printf '%s' "${!3}" > "$W/used.txt"; }
    printf '%s\n' "$text" > "$W/log.txt"
    case "${2:-}" in ''|none) ;; all) for v in $SECRETVARS $USERVARS; do printf -v "$v" '%s' ""; done;; *) for v in $2; do printf -v "$v" '%s' ""; done;; esac
    e2e_scan_log "$W/log.txt" "the log of x" ) 2>&1
}
out="$(scan_run 'engine started; request served in 3 ms')"; rc=$?
verdict "e2e_scan_log: a clean log" 0 "" "$out" "$rc"
for v in $SECRETVARS $USERVARS; do
  out="$(scan_run 'engine started' none "$v")"; rc=$?
  want="a secret of this run appears in the log of x (the text is not shown: it is public)"
  verdict "e2e_scan_log: $v in a log is red, names the log and never shows the value" 1 "$want" "$out" "$rc" "$(cat "$T/sl/used.txt")"
done
out="$(scan_run "request Authorization: Bearer $FAKE_JWT")"; rc=$?
verdict "e2e_scan_log: a JWT-shaped string is red" 1 "something shaped like a JWT" "$out" "$rc" "$FAKE_JWT"
out="$(scan_run 'a header eyJhbGci on its own, and a.b.c, and eyJ.x.y')"; rc=$?
verdict "e2e_scan_log: strings that only start like a token are not a token (the pattern is not absurdly broad)" 0 "" "$out" "$rc"
out="$(scan_run 'fine' all)"; rc=$?
verdict "e2e_scan_log: with no secret known the scan refuses to run (it would look for nothing)" 1 "of this run are known to the scan of the log of x: it would look for almost nothing" "$out" "$rc"
out="$(scan_run 'fine' "$SECRETVARS")"; rc=$?
verdict "e2e_scan_log: with only the users' secrets known it still refuses (eight is under the floor of ten)" 1 "are known to the scan" "$out" "$rc"

# the values themselves never appear in the output of a red scan (checked for a real value, not a marker)
leak_val() { ( set -u; W="$T/sl2"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets; printf '%s' "$GT_JWT_SECRET" > "$W/secret.txt"; printf 'x %s y\n' "$GT_JWT_SECRET" > "$W/log.txt"; e2e_scan_log "$W/log.txt" "the log of x" ) 2>&1; }
body="$(leak_val)"; sv="$(cat "$T/sl2/secret.txt")"
verdict "e2e_scan_log: the red message is the one for a secret (the control of the next case: the scan did find it)" 1 "a secret of this run appears in the log of x" "$body" 1
case "$body" in *"$sv"*) leaked=yes;; *) leaked=no;; esac
verdict "e2e_scan_log: the red message never contains the secret that was found" 0 "no" "$leaked" 0

# ── e2e_scan_artifacts and e2e_require_shots ──
art_run() { # mode: clean | secret-html | secret-zip | empty | nodir | fewsecrets | nounzip | clean-nounzip-nozip
  W="$T/art-$1"; rm -rf "$W"; mkdir -p "$W/e2e-artifacts/shots" "$W/e2e-artifacts/report" "$W/e2e-artifacts/test-results/t1"
  ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets
    unzip() { cat "$2"; }
    printf '<html>report</html>' > "$W/e2e-artifacts/report/index.html"; printf '{"stats":{}}' > "$W/e2e-artifacts/results.json"
    head -c 800 /dev/zero | tr '\0' 'x' > "$W/e2e-artifacts/shots/smoke-root.png"
    printf 'PK\000trace-of-a-failed-test' > "$W/e2e-artifacts/test-results/t1/trace.zip"   # a NUL byte makes it a binary file, which the text scan (grep -I) skips: only the unzip pass can read it, as with a real trace
    case "$1" in
      secret-html) printf '<html>%s</html>' "$M2_TOKEN" > "$W/e2e-artifacts/report/index.html";;
      secret-zip)  printf 'PK\000trace %s end' "$E2E_SERVICE" > "$W/e2e-artifacts/test-results/t1/trace.zip";;
      empty)       rm -rf "$W/e2e-artifacts"; mkdir -p "$W/e2e-artifacts";;
      nodir)       rm -rf "$W/e2e-artifacts";;
      fewsecrets)  for v in $SECRETVARS; do case "$v" in GT_JWT_SECRET|E2E_UNSUB|M2_TOKEN) ;; *) printf -v "$v" '%s' "";; esac; done;;
      nounzip)     command() { if [ "$1" = -v ] && [ "$2" = unzip ]; then return 1; fi; builtin command "$@"; };;
    esac
    e2e_scan_artifacts; echo "RC: $?"; echo "FILES-LEFT: $(find "$W/e2e-artifacts" -type f 2>/dev/null | wc -l | tr -d ' ')" ) 2>&1
}
out="$(art_run clean)"; rc=$?
verdict "e2e_scan_artifacts: clean artifacts" 0 "no secret of this run's infrastructure is in any of them" "$out" "$rc"
verdict "e2e_scan_artifacts: ... the files, the zips and the pictures are counted" 0 "artifacts: 4 file(s), 1 zip(s) unpacked and read, 1 screenshot(s)" "$out" "$rc"
out="$(art_run secret-html)"; rc=$?
verdict "e2e_scan_artifacts: a secret in the HTML report" 1 "appears in the artifact report/index.html (not shown; the artifacts directory was emptied" "$out" "$rc"
out="$(art_run secret-zip)"; rc=$?
verdict "e2e_scan_artifacts: a secret inside a trace (a zip)" 1 "appears inside the zip test-results/t1/trace.zip (not shown; the artifacts directory was emptied" "$out" "$rc"
verdict "e2e_scan_artifacts: ... and nothing is left to upload (the directory is emptied)" 0 "FILES-LEFT: 0" "$( cd "$T/art-secret-zip" && echo "FILES-LEFT: $(find e2e-artifacts -type f | wc -l | tr -d ' ')" )" 0
verdict "e2e_scan_artifacts: ... for the text file too" 0 "FILES-LEFT: 0" "$( cd "$T/art-secret-html" && echo "FILES-LEFT: $(find e2e-artifacts -type f | wc -l | tr -d ' ')" )" 0
out="$(art_run empty)"; rc=$?
verdict "e2e_scan_artifacts: an empty directory (the run wrote nothing)" 1 "the artifacts directory is empty after the run" "$out" "$rc"
out="$(art_run nodir)"; rc=$?
verdict "e2e_scan_artifacts: no directory" 1 "the artifacts directory $T/art-nodir/e2e-artifacts does not exist" "$out" "$rc"
out="$(art_run fewsecrets)"; rc=$?
verdict "e2e_scan_artifacts: too few secrets known to the scan" 1 "infrastructure secret(s) are known to the artifact scan: it would look for almost nothing" "$out" "$rc"
out="$(art_run nounzip)"; rc=$?
verdict "e2e_scan_artifacts: a zip and no unzip: an unscanned trace is not uploaded" 1 "unzip is not installed" "$out" "$rc"
shots_run() { # <number of pngs> [floor]
  W="$T/sh"; rm -rf "$W"; mkdir -p "$W/e2e-artifacts/shots"; local i=0; while [ "$i" -lt "$1" ]; do head -c 600 /dev/zero > "$W/e2e-artifacts/shots/s$i.png"; i=$((i+1)); done
  ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; [ -z "${2:-}" ] || E2E_MIN_SHOTS="$2"; e2e_require_shots ) 2>&1
}
out="$(shots_run 1 1)"; rc=$?
verdict "e2e_require_shots: one picture, a floor of one" 0 "1 screenshot(s) in the gallery (floor 1)" "$out" "$rc"
out="$(shots_run 0)"; rc=$?
verdict "e2e_require_shots: no picture" 1 "0 screenshot(s)" "$out" "$rc"
verdict "e2e_require_shots: ... and the sentence says where to look" 1 "the specs took no picture (web/e2e/support/shots.ts), or E2E_SHOTS_DIR is not where the stage reads" "$out" "$rc"
out="$(shots_run 2 5)"; rc=$?
verdict "e2e_require_shots: fewer pictures than a raised floor" 1 "2 screenshot(s)" "$out" "$rc"
out="$(shots_run 5 5)"; rc=$?
verdict "e2e_require_shots: as many as the raised floor is green (the control of the case above)" 0 "5 screenshot(s) in the gallery (floor 5)" "$out" "$rc"

# ── e2e_secret_leak ──
leak_run() { # <service whose log holds the text> <text> [empty: service whose log is empty]
  W="$T/lk"; rm -rf "$W"; mkdir -p "$W"
  ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets
    LK_SVC="$1"; LK_TEXT="$2"
    DC() { if [ "$1" = logs ]; then local svc="${@: -1}"; echo "ordinary log line of $svc"; if [ "$svc" = "$LK_SVC" ]; then echo "$LK_TEXT"; fi; else return 1; fi; }
    e2e_secret_leak ) 2>&1
}
out="$(leak_run none 'nothing')"; rc=$?
verdict "e2e_secret_leak: clean logs of fifteen services" 0 "appears in the logs of 15 services or in the browser run's own output" "$out" "$rc"
out="$(leak_run e2e-platform "dsn password $(rnd 20)")"; rc=$?
verdict "e2e_secret_leak: a harmless line is not a leak" 0 "none of this run's secrets" "$out" "$rc"
out="$(leak_run e2e-platform "token $FAKE_JWT")"; rc=$?
verdict "e2e_secret_leak: a JWT-shaped string in the platform's log" 1 "something shaped like a JWT (an access token, a key) appears in the log of e2e-platform" "$out" "$rc" "$FAKE_JWT"
out="$( W="$T/lk2"; rm -rf "$W"; mkdir -p "$W"; ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets; DC() { if [ "$1" = logs ]; then [ "${@: -1}" = gotrue ] || echo "line"; else return 1; fi; }; e2e_secret_leak ) 2>&1 )"; rc=$?
verdict "e2e_secret_leak: an empty log of GoTrue is a scan of nothing" 1 "the log of gotrue, which the leak check must read, is empty" "$out" "$rc"
out="$( W="$T/lk3"; rm -rf "$W"; mkdir -p "$W"; ( set -u; WORK="$W"; export WORK; . "$T/scan-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_UNSUB=""; DC() { echo line; }; e2e_secret_leak ) 2>&1 )"; rc=$?
verdict "e2e_secret_leak: no secret of the run known to the shell" 1 "the secrets of this run are empty: the leak scan would look for nothing" "$out" "$rc"


# ── the stand-ins of the services (module 2's operator surface, GoTrue's admin route, psql, the compose CLI) ──
INVITE_ID_A="11111111-1111-4111-8111-111111111111"
LINK_OK_A="https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https://app.harness.test"   # GoTrue writes redirect_to as it holds it (internal/mailer encodeRedirectURL): unencoded here
cat > "$T/user-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
mask() { printf '%s\n' "$1" >> "$WORK/masked.log"; }
sleep() { :; }
STAGE=selftest; PROJECT_NET=n
jobs_ip() { echo "${SC_SINK_IP-10.0.0.9}"; }
jobs_cid() { echo "cid-$1"; }
m2w_stage_5c() { echo "5c-called"; }
m3w_stage_5b() { echo "5b-called probe=${M3W_PROBE_SVC:-}"; }
psql_su() {
  printf '%s\n' "$1" >> "$WORK/sql.log"
  case "$1" in
    *"select converged_version"*) echo "${SC_M3:-t}";;
    *"WITH i AS"*) echo "${SC_LEDGER:-1}";;
    *"WITH u AS"*) echo "${SC_DIAL:-1}";;
  esac
  return 0
}
POOL_DEFAULT='{"id":"55555555-5555-4555-8555-555555555555"}'; ACCT_DEFAULT='{"id":"acct-1"}'
uuid_of() { case "$1" in *e2e-a@*) echo 11111111-1111-4111-8111-111111111111;; *e2e-b@*) echo 22222222-2222-4222-8222-222222222222;; *e2e-c@*) echo 33333333-3333-4333-8333-333333333333;; *e2e-s@*) echo 44444444-4444-4444-8444-444444444444;; esac; }
DC() {
  printf '%s\n' "$*" >> "$WORK/dc.log"
  case "$*" in
    "exec -T gotrue wget"*)
      [ "${SC_WGET_FAIL:-0}" = 1 ] && return 1
      id="$(uuid_of "$*")"; email="$(printf '%s' "$*" | grep -o 'e2e-[a-z]@example.test' | head -1)"
      case "${SC_INVITE:-good}" in
        good)     printf '{"id":"%s","action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https://app.harness.test","email":"%s"}' "$id" "$email";;
        encoded)  printf '{"id":"%s","action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https%%3A%%2F%%2Fapp.harness.test","email":"%s"}' "$id" "$email";;
        defaultpath) printf '{"id":"%s","action_link":"https://auth.harness.test/verify?token=a1b2c3&type=invite&redirect_to=https://app.harness.test","email":"%s"}' "$id" "$email";;   # what run 46085469192 got: the host is ours, the /auth/v1 is gone
        nested)   printf '{"user":{"id":"%s"},"properties":{"action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https%%3A%%2F%%2Fapp.harness.test"}}' "$id";;
        noid)     printf '{"action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=https%%3A%%2F%%2Fapp.harness.test"}';;
        plainhttp) printf '{"id":"%s","action_link":"http://gotrue:9999/verify?token=a1b2c3&type=invite&redirect_to=https%%3A%%2F%%2Fapp.harness.test"}' "$id";;
        recovery) printf '{"id":"%s","action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=recovery&redirect_to=https%%3A%%2F%%2Fapp.harness.test"}' "$id";;
        badredirect) printf '{"id":"%s","action_link":"https://auth.harness.test/auth/v1/verify?token=a1b2c3&type=invite&redirect_to=http%%3A%%2F%%2Flocalhost"}' "$id";;
      esac
      return 0;;
    "exec -T m2 curl"*)
      if [ "${SC_CURL_FAIL:-0}" = 1 ]; then echo "curl: (7) Failed to connect to localhost port 8080" >&2; return 7; fi
      case "$*" in
        *"/v1/admin/pools"*)    printf '%s\n%s' "${SC_POOL_BODY-$POOL_DEFAULT}" "${SC_POOL_CODE:-201}";;
        *"/v1/admin/accounts"*) if [ "${SC_ACCT_ECHO:-0}" = 1 ]; then printf '%s\n%s' "{\"error\":\"bad smtp_secret $SINK_PASS\"}" 400; else printf '%s\n%s' "${SC_ACCT_BODY-$ACCT_DEFAULT}" "${SC_ACCT_CODE:-201}"; fi;;
        *"/v1/admin/config/"*)  if [ "${SC_DIAL_ECHO:-0}" = 1 ]; then printf '%s\n%s' "{\"error\":\"bad key $M2_ADMIN_KEY\"}" 500; else printf '%s\n%s' '{"value":1}' "${SC_DIAL_CODE:-200}"; fi;;
      esac
      return 0;;
    "logs "*) echo "LOG-MARKER of ${@: -1}"; return 0;;
    "up "*) [ "${SC_UP_FAIL:-}" = "${@: -1}" ] && return 1; return 0;;
  esac
  return 0
}
EOF
us_run() { # <the function and its arguments>; the SC_* variables are the caller's
  W="$T/us"; rm -rf "$W"; mkdir -p "$W"
  ( set -u; WORK="$W"; export WORK; . "$T/user-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_SERVICE="$(rnd 30)"
    e2e_enforces_modes() { return 1; }
    "$@"; echo "RC: $?" ) 2>&1
}

# ── e2e_m2_call, e2e_m2 ──
out="$(us_run e2e_m2)"; rc=$?
verdict "e2e_m2: six dials set, after module 2's worker stage" 0 "module 2: 6 global dials set" "$out" "$rc"
verdict "e2e_m2: ... the worker stage ran (5c)" 0 "5c-called" "$out" "$rc"
verdict "e2e_m2: ... each dial is a PUT to the operator surface with {\"value\": ...}" 0 "put-ok" "$(n=$(grep -c 'curl .* -X PUT .*-d {"value":.*/v1/admin/config/' "$T/us/dc.log"); [ "$n" = 6 ] && echo put-ok || echo "$n")" 0
verdict "e2e_m2: ... the retry ladder is the shortened one" 0 "ladder" "$(grep -q '/v1/admin/config/retry_backoff_schedule' "$T/us/dc.log" && grep -q '{"value":{"ladder_seconds":\[1,1,1\],"max_seconds":1}}' "$T/us/dc.log" && echo ladder || echo missing)" 0
verdict "e2e_m2: ... and the dials are the six of the scenario program (job_max_failed_attempts, retry_backoff_schedule, send_delay_min_sec, send_delay_max_sec, concurrency_per_user_default, dispatch_tick_interval_ms)" 0 "six" "$(n=0; for k in job_max_failed_attempts retry_backoff_schedule send_delay_min_sec send_delay_max_sec concurrency_per_user_default dispatch_tick_interval_ms; do grep -q "/v1/admin/config/$k" "$T/us/dc.log" && n=$((n+1)); done; [ "$n" = 6 ] && echo six || echo "$n")" 0
out="$(SC_DIAL_CODE=500 us_run e2e_m2)"; rc=$?
verdict "e2e_m2: a dial that module 2 refuses" 1 "module 2 answered 500 to the dial job_max_failed_attempts (want 200)" "$out" "$rc"
verdict "e2e_m2: ... the log of module 2 is printed first" 1 "LOG-MARKER of m2" "$out" "$rc"
out="$(SC_UP_FAIL=m2 us_run e2e_m2)"; rc=$?
verdict "e2e_m2: module 2 that does not become healthy" 1 "module 2 did not become healthy" "$out" "$rc"
res="$(SC_DIAL_ECHO=1 us_run e2e_m2)"; rc=$?
verdict "e2e_m2: a refusal says its status and the SHAPE of its body, not its text" 1 "answered 500 to the dial job_max_failed_attempts (want 200): HTTP 500, " "$res" "$rc"
verdict "e2e_m2: ... the key names of that body (the only thing it says of it besides its size)" 1 "a JSON object with the keys: error" "$res" "$rc"
adm="$(grep -o 'Bearer [0-9a-f]*' "$T/us/dc.log" | head -1 | sed 's/^Bearer //')"
case "$res" in *"$adm"*) leaked=yes;; *) leaked=no;; esac
verdict "e2e_m2: ... the key is nowhere in the output" 0 "no" "$leaked" 0

# ── e2e_dial_row, e2e_m3 ──
out="$(us_run e2e_m3)"; rc=$?
verdict "e2e_m3: module 3 up, stage 5b with GoTrue's container as the prober, five dials" 0 "5b-called probe=gotrue" "$out" "$rc"
verdict "e2e_m3: ... five dials written to send_byo.config" 0 "module 3: 5 dials written to send_byo.config" "$out" "$rc"
verdict "e2e_m3: ... the From-gate in enforce against the fixture DNS" 0 "gate" "$(grep -q "{\"v\":\"enforce\"}' WHERE key = 'from_gate_mode'" "$T/us/sql.log" && grep -q "{\"v\":\"m3dns:53\"}' WHERE key = 'from_dns_resolver'" "$T/us/sql.log" && echo gate || echo missing)" 0
verdict "e2e_m3: ... the minimum delay is lowered BEFORE the maximum (min <= max at every step)" 0 "ordered" "$(a="$(grep -n "key = 'send_delay_min_sec'" "$T/us/sql.log" | cut -d: -f1)"; b="$(grep -n "key = 'send_delay_max_sec'" "$T/us/sql.log" | cut -d: -f1)"; [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] && echo ordered || echo "min $a max $b")" 0
verdict "e2e_m3: ... one lane per user" 0 "lane" "$(grep -q "{\"v\":1}' WHERE key = 'concurrency_per_user_default'" "$T/us/sql.log" && echo lane || echo missing)" 0
out="$(SC_DIAL=0 us_run e2e_m3)"; rc=$?
verdict "e2e_m3: a dial that module 3 does not have" 1 "module 3 has no dial from_gate_mode in send_byo.config (0 row(s) updated)" "$out" "$rc"
out="$(SC_UP_FAIL=m3 us_run e2e_m3)"; rc=$?
verdict "e2e_m3: module 3 that does not become healthy" 1 "module 3 did not become healthy" "$out" "$rc"

# ── e2e_user ──
out="$(us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a funded user" 0 "user a: invited, a platform user, 100000 credits at module 1 and at module 2 (ledger), module 3 enabled" "$out" "$rc"
verdict "e2e_user: ... the invite is asked of GoTrue's admin route with the type invite and the user's address" 0 "invite-asked" "$(grep -q 'exec -T gotrue wget .*--post-data {"type":"invite","email":"e2e-a@example.test"} http://127.0.0.1:9999/admin/generate_link' "$T/us/dc.log" && echo invite-asked || echo missing)" 0
verdict "e2e_user: ... the platform user has the id GoTrue gave, as id and as auth_user_id" 0 "row" "$(grep -q "INSERT INTO public.users (id, auth_user_id, email) VALUES ('$INVITE_ID_A', '$INVITE_ID_A', 'e2e-a@example.test')" "$T/us/sql.log" && echo row || echo missing)" 0
verdict "e2e_user: ... credits at modules 1 and 2 through the ledger, as grants naming the first admin, keyed per module and user" 0 "grants" "$(n=$(grep -c "'grant', 100000, a.id" "$T/us/sql.log"); [ "$n" = 2 ] && grep -q "'m1', 'grant'" "$T/us/sql.log" && grep -q "'m2', 'grant'" "$T/us/sql.log" && grep -q "e2e-grant-m1-$INVITE_ID_A" "$T/us/sql.log" && grep -q "e2e-grant-m2-$INVITE_ID_A" "$T/us/sql.log" && grep -q 'FROM platform.admins a WHERE a.disabled_at IS NULL' "$T/us/sql.log" && echo grants || echo "$n")" 0
verdict "e2e_user: ... module 3 enabled through the ledger" 0 "m3" "$(grep -q "'m3', 'm3_enable', a.id" "$T/us/sql.log" && echo m3 || echo missing)" 0
verdict "e2e_user: ... a dedicated pool of the user at module 2, then an account naming the sink's private address and port" 0 "pool-account" "$(grep -q '"kind":"dedicated","owner_user_id":"'"$INVITE_ID_A"'"' "$T/us/dc.log" && grep -q '"pool_id":"55555555-5555-4555-8555-555555555555","smtp_host":"10.0.0.9","smtp_port":1025' "$T/us/dc.log" && echo pool-account || echo missing)" 0
verdict "e2e_user: ... the invite link was masked and is never printed" 0 "masked-not-printed" "$(grep -qF "$LINK_OK_A" "$T/us/masked.log" && ! printf '%s' "$out" | grep -qF 'token=a1b2c3' && echo masked-not-printed || echo wrong)" 0
verdict "e2e_user: ... and the module 2 account id is kept for the fixtures" 0 "kept" "$( ( set -u; WORK="$T/us"; . "$T/user-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_SERVICE=x; e2e_user a 1 >/dev/null 2>&1; echo "${E2E_M2ACCOUNT_a}|${E2E_ID_a}|${E2E_FUNDED_a}" ) | grep -q '^acct-1|'"$INVITE_ID_A"'|1$' && echo kept || echo missing)" 0
out="$(us_run e2e_user s 0)"; rc=$?
verdict "e2e_user: a user without credits" 0 "user s: invited, a platform user, no credits, no entitlement, no sending account" "$out" "$rc"
verdict "e2e_user: ... writes no ledger row and calls module 2 not at all" 0 "none" "$(! grep -q 'platform.ledger' "$T/us/sql.log" && ! grep -q 'exec -T m2' "$T/us/dc.log" && echo none || echo some)" 0
out="$(SC_INVITE=nested us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: an id and a link nested the way a newer GoTrue may answer them are read too" 0 "user a: invited" "$out" "$rc"
out="$(SC_WGET_FAIL=1 us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: GoTrue that refuses the invite" 1 "GoTrue refused the invite of the user a at POST /admin/generate_link" "$out" "$rc"
verdict "e2e_user: ... the log of GoTrue is printed first" 1 "LOG-MARKER of gotrue" "$out" "$rc"
out="$(SC_INVITE=noid us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: an answer with no user id" 1 "has no user id" "$out" "$rc"
verdict "e2e_user: ... says what it did get: its size and the key names, never a value" 1 "HTTP 200 (wget exits non-zero on an HTTP error), " "$out" "$rc" "a1b2c3"
verdict "e2e_user: ... the key names of that body" 1 "a JSON object with the keys: action_link" "$out" "$rc"
out="$(SC_INVITE=defaultpath us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: the link of the first runner run (our host, no /auth/v1: the mailer's URL path was the default /verify)" 1 "does not start with https://auth.harness.test/auth/v1/verify? (it starts with 'https://auth.harness.test/verify')" "$out" "$rc"
verdict "e2e_user: ... and the sentence names the variable that is wrong, without the token" 1 "GOTRUE_MAILER_URLPATHS_INVITE=/auth/v1/verify" "$out" "$rc" "a1b2c3"
out="$(SC_INVITE=encoded us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a redirect_to that GoTrue URL-encoded (it does so when the address holds & = or #) is the same address" 0 "user a: invited" "$out" "$rc"
out="$(SC_INVITE=plainhttp us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a link under the plain-http name (API_EXTERNAL_URL did not reach GoTrue)" 1 "does not start with https://auth.harness.test/auth/v1/verify? (it starts with 'http://gotrue:9999/verify')" "$out" "$rc"
verdict "e2e_user: ... the sentence shows no token" 1 "GOTRUE_MAILER_URLPATHS_INVITE" "$out" "$rc" "a1b2c3"
out="$(SC_INVITE=recovery us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a link that is not an invite" 1 "is not an invite link" "$out" "$rc"
out="$(SC_INVITE=badredirect us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a link that does not send the browser to the app" 1 "does not send the browser to https://app.harness.test after the verification" "$out" "$rc"
out="$(SC_LEDGER=0 us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a grant that wrote no row (no active admin)" 1 "the grant at m1 to the user a wrote 0 ledger row(s), want 1" "$out" "$rc"
out="$(SC_SINK_IP= us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: the sink's address that cannot be read" 1 "could not read the address of smtp-ctl on n (got '')" "$out" "$rc"
out="$(SC_POOL_CODE=500 us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a pool that module 2 refuses" 1 "module 2 answered 500 to the pool of the user a (want 201)" "$out" "$rc"
out="$(SC_POOL_BODY='{}' us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a pool answer with no id" 1 "has no pool id" "$out" "$rc"
out="$(SC_ACCT_CODE=409 us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a sending account that module 2 refuses" 1 "module 2 answered 409 to the sending account of the user a (want 201)" "$out" "$rc"
out="$(SC_ACCT_BODY='{}' us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: an account answer with no id" 1 "has no account id" "$out" "$rc"
res="$(SC_ACCT_ECHO=1 us_run e2e_user a 1)"; rc=$?
verdict "e2e_user: a refusal that echoes the SMTP secret says its status and the key names of its body, never its text" 1 "answered 400 to the sending account of the user a (want 201): HTTP 400, " "$res" "$rc"
verdict "e2e_user: ... and the key names are all it says of the body" 1 "a JSON object with the keys: error" "$res" "$rc"
sp="$(grep -o '"smtp_secret":"[0-9a-f]*"' "$T/us/dc.log" | head -1 | sed 's/.*:"\(.*\)"/\1/')"
case "$res" in *"$sp"*) leaked=yes;; *) leaked=no;; esac
verdict "e2e_user: ... the secret is nowhere in the output" 0 "no" "$leaked" 0

# ── how the stage reads an answer: the shapes that go wrong (run 46092116199: a body of more than 300 bytes was cut BEFORE it was parsed, and the pool had "no id") ──
POOL_ID="55555555-5555-4555-8555-555555555555"
LONG_POOL="$(node -e 'process.stdout.write(JSON.stringify({id:process.argv[1],kind:"dedicated",owner_user_id:"11111111-1111-4111-8111-111111111111",name:"e2e a",created_at:"2026-11-28T10:00:00.000Z",updated_at:"2026-11-28T10:00:00.000Z",notes:"x".repeat(400)}))' "$POOL_ID")"
LONG_ACCT="$(node -e 'process.stdout.write(JSON.stringify({id:"acct-long",pool_id:process.argv[1],smtp_host:"10.0.0.9",from_address:"a@example.test",notes:"y".repeat(500)}))' "$POOL_ID")"
m2probe() { e2e_m2_call POST /v1/admin/pools '{}'; echo "CODE=$E2E_HTTP_CODE LEN=${#E2E_HTTP_BODY}"; echo "ID=$(printf '%s' "$E2E_HTTP_BODY" | e2e_json_get id)"; echo "WHAT=$(e2e_http_what)"; }
desc() { printf '%s' "$1" | ( set -u; . "$HERE/stage.sh"; e2e_describe_body ); }
verdict "answer parse: CONTROL: the body of the next cases is longer than the 300 bytes the first runner pass cut it to" 0 "longer" "$([ "${#LONG_POOL}" -gt 300 ] && [ "${#LONG_ACCT}" -gt 300 ] && echo longer || echo "${#LONG_POOL}/${#LONG_ACCT}")" 0
out="$(SC_POOL_BODY="$LONG_POOL" us_run m2probe)"; rc=$?
verdict "answer parse: a body of ${#LONG_POOL} bytes is kept WHOLE" 0 "CODE=201 LEN=${#LONG_POOL}" "$out" "$rc"
verdict "answer parse: ... and its id is read" 0 "ID=$POOL_ID" "$out" "$rc"
out="$(SC_POOL_BODY="$LONG_POOL" SC_ACCT_BODY="$LONG_ACCT" us_run e2e_user a 1)"; rc=$?
verdict "answer parse: a pool and an account answer of more than 300 bytes each pass e2e_user (the fault of run 46092116199)" 0 "user a: invited" "$out" "$rc"
out="$(SC_POOL_BODY="{\"id\":\"$POOL_ID\"}"$'\n' us_run m2probe)"; rc=$?
verdict "answer parse: a body with a trailing newline" 0 "ID=$POOL_ID" "$out" "$rc"
verdict "answer parse: ... the newline is part of the body, the status is still the last line" 0 "CODE=201 LEN=$((${#POOL_ID} + 10))" "$out" "$rc"
out="$(SC_POOL_BODY=$'{\n  "id": "'"$POOL_ID"$'",\n  "kind": "dedicated"\n}\n' us_run m2probe)"; rc=$?
verdict "answer parse: a body printed over several lines" 0 "ID=$POOL_ID" "$out" "$rc"
verdict "answer parse: ... with the status of its own after it" 0 "CODE=201" "$out" "$rc"
out="$(SC_POOL_BODY=$'HTTP/1.1 201 Created\r\n\r\n'"{\"id\":\"$POOL_ID\"}" us_run m2probe)"; rc=$?
verdict "answer parse: a body mixed with a status line is not read as JSON (the id is empty)" 0 "ID=" "$out" "$rc" "ID=$POOL_ID"
verdict "answer parse: ... and the description says so, with its size and no text" 0 "HTTP 201, " "$out" "$rc" "Created"
verdict "answer parse: ... NOT JSON, and what it starts with by kind" 0 "NOT JSON (it starts with another character)" "$out" "$rc"
out="$(SC_POOL_BODY=$'HTTP/1.1 201 Created\r\n\r\n'"{\"id\":\"$POOL_ID\"}" us_run e2e_user a 1)"; rc=$?
verdict "answer parse: e2e_user on that body dies naming the pool and what it received" 1 "module 2's answer to the pool of the user a has no pool id (the stage reads a top-level id, as the scenario program does): HTTP 201, " "$out" "$rc"
verdict "answer parse: ... and the log of module 2 is printed first" 1 "LOG-MARKER of m2" "$out" "$rc"
out="$(SC_POOL_BODY= us_run e2e_user a 1)"; rc=$?
verdict "answer parse: an empty body with a 201" 1 "HTTP 201, 0 bytes, empty" "$out" "$rc"
out="$(SC_ACCT_BODY='[]' us_run e2e_user a 1)"; rc=$?
verdict "answer parse: an account answer that is a JSON array" 1 "has no account id (the stage reads a top-level id): HTTP 201, 2 bytes, a JSON array" "$out" "$rc"
out="$(SC_CURL_FAIL=1 us_run m2probe)"; rc=$?
verdict "answer parse: curl that fails: no status, no body" 0 "CODE=000 LEN=0" "$out" "$rc"
verdict "answer parse: ... what curl said (its stderr, kept apart from the body) is in the description" 0 "WHAT=HTTP 000, 0 bytes, empty; curl said: curl: (7) Failed to connect to localhost port 8080" "$out" "$rc"
verdict "e2e_describe_body: a JSON object: its size and its key names, sorted" 0 "$(printf '%s' '{"smtp_secret":"VERYSECRETVALUE","id":"i"}' | wc -c | tr -d ' ') bytes, a JSON object with the keys: id, smtp_secret" "$(desc '{"smtp_secret":"VERYSECRETVALUE","id":"i"}')" 0
verdict "e2e_describe_body: ... and never a value" 0 "no-value" "$(d="$(desc '{"smtp_secret":"VERYSECRETVALUE","id":"i"}')"; case "$d" in *VERYSECRETVALUE*) echo LEAKED;; *) echo no-value;; esac)" 0
verdict "e2e_describe_body: an empty object" 0 "2 bytes, a JSON object with no keys" "$(desc '{}')" 0
verdict "e2e_describe_body: an array" 0 "3 bytes, a JSON array" "$(desc '[1]')" 0
verdict "e2e_describe_body: a scalar" 0 "2 bytes, a JSON number" "$(desc '42')" 0
verdict "e2e_describe_body: null" 0 "4 bytes, a JSON null" "$(desc 'null')" 0
verdict "e2e_describe_body: nothing" 0 "0 bytes, empty" "$(desc '')" 0
verdict "e2e_describe_body: an HTML page" 0 "NOT JSON (it starts with a < character)" "$(desc '<html>nope</html>')" 0
verdict "e2e_describe_body: a truncated JSON document (what a cut body looks like)" 0 "NOT JSON (it starts with a { character)" "$(desc '{"id":"abc","kind":"dedi')" 0

# ── e2e_users: the four users and the fixtures file ──
out="$(us_run e2e_users)"; rc=$?
verdict "e2e_users: four users, the entitlements delivered, the fixtures written" 0 "fixtures written: 4 users (a b c s), mode 600" "$out" "$rc"
verdict "e2e_users: ... module 3's entitlement is awaited for the three funded users" 0 "module 3's entitlement is delivered and observed active for every funded user" "$out" "$rc"
verdict "e2e_users: ... the ledger holds three module 3 enables and none for the user without credits" 0 "three" "$([ "$(grep -c "'m3', 'm3_enable'" "$T/us/sql.log")" = 3 ] && ! grep -q "'m3_enable'.*44444444-4444-4444-8444-444444444444" "$T/us/sql.log" && echo three || echo wrong)" 0
fxj="$T/us/e2e-fixtures/fixtures.json"
verdict "e2e_users: ... the document has four users, three funded with a sending account, one without" 0 "doc-ok" "$(node -e 'const j=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));const u=j.users;console.log(Object.keys(u).join()==="a,b,c,s"&&u.a.funded&&u.b.funded&&u.c.funded&&!u.s.funded&&u.s.m2_account_id===null&&u.a.m2_account_id==="acct-1"&&j.urls.app==="https://app.harness.test"&&j.urls.auth==="https://auth.harness.test/auth/v1"&&j.urls.sink==="http://smtp-ctl:8025"&&j.urls.m3_sink_host==="10.smtp-ctl.example.test"?"doc-ok":"wrong")' "$fxj")" 0
verdict "e2e_users: ... every user has its own id, its own password and the invite link GoTrue gave" 0 "distinct" "$(node -e 'const u=Object.values(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).users);const d=(k)=>new Set(u.map(x=>x[k])).size===4;console.log(d("id")&&d("password")&&d("email")&&u.every(x=>x.invite_link.startsWith("https://auth.harness.test/auth/v1/verify?"))?"distinct":"wrong")' "$fxj")" 0
out="$(SC_M3=f us_run e2e_users)"; rc=$?
verdict "e2e_users: an entitlement the platform never delivers" 1 "the platform did not deliver module 3's entitlement of the user a within 120 s" "$out" "$rc"
verdict "e2e_users: ... the log of the platform is printed first" 1 "LOG-MARKER of e2e-platform" "$out" "$rc"
out="$( W="$T/us"; rm -rf "$W"; mkdir -p "$W"; ( set -u; WORK="$W"; export WORK; . "$T/user-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_SERVICE=""; e2e_users ) 2>&1 )"; rc=$?
verdict "e2e_users: no service-role key (e2e_mint did not run)" 1 "the service-role key was not minted" "$out" "$rc"
out="$( W="$T/us"; rm -rf "$W"; mkdir -p "$W"; ( set -u; WORK="$W"; export WORK; . "$T/user-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_SERVICE="$(rnd 30)"; e2e_enforces_modes() { return 1; }; E2E_PW_a=pw-too-brief; e2e_users ) 2>&1 )"; rc=$?
verdict "e2e_users: a fixtures file that the writer refuses (a password under 16 characters)" 1 "the fixtures file could not be written" "$out" "$rc"
verdict "e2e_users: ... and the writer's own reason names the variable, not the value" 1 "E2E_PW_a: must be at least 16 characters" "$out" "$rc" "pw-too-brief"

# ── e2e_run_tests ──
cat > "$T/run-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
STAGE=selftest
DC() {
  printf '%s\n' "$*" >> "$WORK/dc.log"
  case "$*" in
    *"playwright test --list"*)
      [ "${SC_LIST_FAIL:-0}" = 1 ] && { echo "Error: cannot load the config"; return 1; }
      echo "${SC_LISTTEXT-Listing tests:}"; [ -n "${SC_NOTOTAL:-}" ] || echo "Total: ${SC_TOTAL:-10} tests in 1 file"; return 0;;
    *"playwright test"*)
      if [ "${SC_NOARTIFACTS:-0}" != 1 ]; then
        mkdir -p "$WORK/e2e-artifacts/shots" "$WORK/e2e-artifacts/report"
        report ${SC_REPORT:-10 0 0 0} > "$WORK/e2e-artifacts/results.json"
        [ "${SC_NOSHOTS:-0}" = 1 ] || head -c 700 /dev/zero > "$WORK/e2e-artifacts/shots/smoke-root.png"
        [ -z "${SC_ARTIFACT_LEAK:-}" ] || printf '%s' "${!SC_ARTIFACT_LEAK}" > "$WORK/e2e-artifacts/report/index.html"
      fi
      echo "the run printed a line"; [ -z "${SC_RUN_LEAK:-}" ] || echo "and then ${!SC_RUN_LEAK}"
      return "${SC_RUN_RC:-0}";;
    "logs "*) echo "LOG-MARKER of ${@: -1}"; return 0;;
  esac
  return 0
}
EOF
rt_run() { # runs e2e_run_tests with the caller's SC_* variables
  W="$T/rt"; rm -rf "$W"; mkdir -p "$W"
  ( set -u; WORK="$W"; export WORK; . "$T/run-env.sh"; . "$HERE/stage.sh"; setsecrets; E2E_MIN_TESTS=10; E2E_MIN_SHOTS=1
    e2e_run_tests; echo "RC: $?" ) 2>&1
}
out="$(rt_run)"; rc=$?
verdict "e2e_run_tests: ten listed, ten ran, a picture taken" 0 "check-report: OK" "$out" "$rc"
verdict "e2e_run_tests: ... the list is read first and its count is reported" 0 "10 test(s) are listed (floor 10)" "$out" "$rc"
verdict "e2e_run_tests: ... the artifacts were read before the verdict" 0 "no secret of this run's infrastructure is in any of them" "$out" "$rc"
verdict "e2e_run_tests: ... the pictures are counted at the end" 0 "1 screenshot(s) in the gallery (floor 1)" "$out" "$rc"
out="$(E2E_RUN_ARGS='--grep smoke' rt_run)"; rc=$?
verdict "e2e_run_tests: extra arguments reach Playwright (a dispatch can narrow a run)" 0 "args" "$(grep -q 'playwright test --grep smoke' "$T/rt/dc.log" && echo args || echo missing)" 0
out="$(SC_TOTAL=8 rt_run)"; rc=$?
verdict "e2e_run_tests: fewer specs listed than the floor" 1 "8 test(s) are listed, the floor of this scope is 10" "$out" "$rc"
out="$(SC_NOTOTAL=1 rt_run)"; rc=$?
verdict "e2e_run_tests: a list with no 'Total: N tests' line" 1 "the list of the specs has no 'Total: N tests' line" "$out" "$rc"
out="$(SC_LIST_FAIL=1 rt_run)"; rc=$?
verdict "e2e_run_tests: a config that does not load" 1 "the specs could not be listed inside the Playwright image" "$out" "$rc"
verdict "e2e_run_tests: ... what the list said is printed" 1 "Error: cannot load the config" "$out" "$rc"
out="$(SC_RUN_RC=1 SC_REPORT='9 1 0 0' rt_run)"; rc=$?
verdict "e2e_run_tests: a failed spec" 1 "the browser specs failed (exit 1)" "$out" "$rc"
verdict "e2e_run_tests: ... the run's own text is printed and the platform's log follows" 1 "LOG-MARKER of e2e-platform" "$out" "$rc"
out="$(SC_RUN_RC=1 SC_NOARTIFACTS=1 rt_run)"; rc=$?
verdict "e2e_run_tests: a run that failed before it wrote anything" 1 "failed (exit 1) before it wrote a single artifact" "$out" "$rc"
out="$(SC_RUN_LEAK=M2_TOKEN rt_run)"; rc=$?
verdict "e2e_run_tests: a secret in the run's own output is red and the output is not printed" 1 "a secret of this run appears in the browser run's own output (the text is not shown: it is public)" "$out" "$rc" "the run printed a line"
out="$(SC_ARTIFACT_LEAK=E2E_SERVICE rt_run)"; rc=$?
verdict "e2e_run_tests: a secret in an artifact is red even when the run passed" 1 "appears in the artifact report/index.html" "$out" "$rc"
out="$(SC_REPORT='9 0 0 1' rt_run)"; rc=$?
verdict "e2e_run_tests: a skipped test in the report is red although the exit status was 0" 1 "were skipped: a test that cannot run FAILS, it never skips" "$out" "$rc"
out="$(SC_TOTAL=12 rt_run)"; rc=$?
verdict "e2e_run_tests: twelve listed and ten ran" 1 "12 test(s) were listed and 10 ran" "$out" "$rc"
verdict "e2e_run_tests: ... and the stage says the report does not prove the run" 1 "the browser run's own report does not prove the run" "$out" "$rc"
out="$(SC_NOSHOTS=1 rt_run)"; rc=$?
verdict "e2e_run_tests: no picture" 1 "0 screenshot(s)" "$out" "$rc"

# ── e2e_web ──
cat > "$T/web-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
STAGE=selftest
node() { case "$*" in *process.versions*) echo "${SC_NODE_MAJOR-24}";; "--version") echo v24.0.0;; *"@playwright/test/package.json"*) echo "${SC_PW-$(e2e_image_version)}";; *) command node "$@";; esac; }
npm() {
  case "$1" in
    --version) echo 11.0.0;;
    ci) [ "${SC_CI_FAIL:-0}" = 1 ] && { echo "npm error code EUSAGE"; return 1; }; echo "added packages";;
    run) [ "${SC_BUILD_FAIL:-0}" = 1 ] && { echo "error TS2322: type error"; return 1; }
         mkdir -p dist/assets; [ "${SC_NOINDEX:-0}" = 1 ] || echo '<!doctype html>' > dist/index.html; [ "${SC_ONEFILE:-0}" = 1 ] || echo 'x' > dist/assets/index.js; echo built;;
  esac
}
EOF
web_run() {
  W="$T/web"; rm -rf "$W" "$T/pd"; mkdir -p "$W" "$T/pd/web"
  ( set -u; WORK="$W"; PLATFORM_DIR="$T/pd"; export WORK PLATFORM_DIR; . "$T/web-env.sh"; . "$HERE/stage.sh"; e2e_web ) 2>&1
}
want_pw="$( . "$HERE/stage.sh"; e2e_image_version )"
out="$(web_run)"; rc=$?
verdict "e2e_web: the app is built and the Playwright package is the image's version" 0 "@playwright/test $want_pw is the version of the pinned image; no browser was downloaded" "$out" "$rc"
verdict "e2e_web: ... the pinned version was read from the image line (a three-part version)" 0 "versioned" "$(printf '%s' "$want_pw" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' && echo versioned || echo "$want_pw")" 0
out="$(SC_NODE_MAJOR=22 web_run)"; rc=$?
verdict "e2e_web: a node older than web/package.json asks for" 1 "node 22 is too old: web/package.json asks for node 24 or later" "$out" "$rc"
out="$(SC_NODE_MAJOR= web_run)"; rc=$?
verdict "e2e_web: a node version that cannot be read" 1 "could not read the node version" "$out" "$rc"
out="$(SC_CI_FAIL=1 web_run)"; rc=$?
verdict "e2e_web: npm ci that fails" 1 "npm ci failed in web/" "$out" "$rc"
verdict "e2e_web: ... npm's own text is printed" 1 "npm error code EUSAGE" "$out" "$rc"
out="$(SC_BUILD_FAIL=1 web_run)"; rc=$?
verdict "e2e_web: a build that fails" 1 "npm run build failed in web/" "$out" "$rc"
out="$(SC_NOINDEX=1 web_run)"; rc=$?
verdict "e2e_web: a build that leaves no index.html" 1 "web/dist/index.html is missing or empty after the build" "$out" "$rc"
out="$(SC_ONEFILE=1 web_run)"; rc=$?
verdict "e2e_web: a dist of one file" 1 "web/dist holds 1 file(s) after the build" "$out" "$rc"
out="$(SC_PW=1.0.0 web_run)"; rc=$?
verdict "e2e_web: a Playwright package that is not the image's version" 1 "web/node_modules/@playwright/test is version '1.0.0' but the digest-pinned image carries the browsers of $want_pw" "$out" "$rc"

# ── e2e_platform ──
cat > "$T/plat-env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
STAGE=selftest
jobs_cid() { echo "cid-$1"; }
e2e_tls() { echo "tls-made"; }
docker() { if [ "${SC_NOGOTRUE_ENV:-0}" = 1 ]; then printf 'API_EXTERNAL_URL=http://gotrue:9999\nGOTRUE_SITE_URL=http://gotrue:9999\nGOTRUE_JWT_ISSUER=http://gotrue:9999/auth/v1\n'; elif [ "${SC_NOURLPATH:-0}" = 1 ]; then printf 'PATH=/bin\nAPI_EXTERNAL_URL=https://auth.harness.test/auth/v1\nGOTRUE_API_EXTERNAL_URL=https://auth.harness.test/auth/v1\nGOTRUE_SITE_URL=https://app.harness.test\nGOTRUE_JWT_ISSUER=https://auth.harness.test/auth/v1\n'; else printf 'PATH=/bin\nAPI_EXTERNAL_URL=https://auth.harness.test/auth/v1\nGOTRUE_API_EXTERNAL_URL=https://auth.harness.test/auth/v1\nGOTRUE_SITE_URL=https://app.harness.test\nGOTRUE_JWT_ISSUER=https://auth.harness.test/auth/v1\nGOTRUE_MAILER_URLPATHS_INVITE=/auth/v1/verify\nGOTRUE_MAILER_URLPATHS_RECOVERY=/auth/v1/verify\n'; fi; }
DC() {
  printf '%s\n' "$*" >> "$WORK/dc.log"
  case "$*" in
    "up -d --no-deps --wait --wait-timeout 240 e2e-platform") [ "${SC_PLAT_FAIL:-0}" = 1 ] && return 1; return 0;;
    "up -d --no-deps --wait --wait-timeout 120 e2e-edge") [ "${SC_EDGE_FAIL:-0}" = 1 ] && return 1; return 0;;
    "logs --no-color e2e-platform") [ "${SC_NOUNSUB:-0}" = 1 ] || echo '{"msg":"unsubscribe link configured","enabled":true}'; return 0;;
    "logs "*) echo "LOG-MARKER of ${@: -1}"; return 0;;
    "run --rm -T --no-deps -e NODE_TLS_REJECT_UNAUTHORIZED=0 --entrypoint node e2e-runner --input-type=module -e "*) [ "${SC_PREFLIGHT_RC:-0}" = 0 ] && { echo "preflight ok: stand-in"; return 0; }; echo "preflight: https://app.harness.test/config.json answered 404 (want 200): PLATFORM_WEB_DIR is not served"; return 1;;
  esac
  return 0
}
EOF
pl_run() {
  W="$T/pl"; rm -rf "$W"; mkdir -p "$W"
  ( set -u; WORK="$W"; export WORK; . "$HERE/stage.sh"; . "$T/plat-env.sh"; e2e_platform; echo "RC: $?" ) 2>&1   # the stand-ins AFTER the stage: e2e_tls here is the stand-in's, not the real one
}
out="$(pl_run)"; rc=$?
verdict "e2e_platform: the certificate, the platform, the edge and the preflight" 0 "preflight ok: stand-in" "$out" "$rc"
verdict "e2e_platform: ... the certificate is made first" 0 "tls-made" "$out" "$rc"
verdict "e2e_platform: ... the preflight runs in the Playwright image with the self-signed chain ignored" 0 "-e NODE_TLS_REJECT_UNAUTHORIZED=0 --entrypoint node e2e-runner --input-type=module" "$(cat "$T/pl/dc.log")" 0
out="$(SC_PLAT_FAIL=1 pl_run)"; rc=$?
verdict "e2e_platform: a platform that does not become healthy" 1 "the platform instance did not become healthy" "$out" "$rc"
verdict "e2e_platform: ... its log is printed first" 1 "LOG-MARKER of e2e-platform" "$out" "$rc"
out="$(SC_NOUNSUB=1 pl_run)"; rc=$?
verdict "e2e_platform: a platform whose unsubscribe link is not configured" 1 "the platform did not log that its unsubscribe link is configured" "$out" "$rc"
out="$(SC_EDGE_FAIL=1 pl_run)"; rc=$?
verdict "e2e_platform: an edge that does not become healthy" 1 "the TLS edge (nginx) did not become healthy" "$out" "$rc"
verdict "e2e_platform: ... the edge's log is printed first" 1 "LOG-MARKER of e2e-edge" "$out" "$rc"
out="$(SC_NOGOTRUE_ENV=1 pl_run)"; rc=$?
verdict "e2e_platform: a GoTrue that kept the plain-http names" 1 "GoTrue's container does not have API_EXTERNAL_URL=https://auth.harness.test/auth/v1" "$out" "$rc"
out="$(SC_NOURLPATH=1 pl_run)"; rc=$?
verdict "e2e_platform: a GoTrue with the right external URL and no mailer URL path (the fault of the first runner run)" 1 "GoTrue's container does not have GOTRUE_MAILER_URLPATHS_INVITE=/auth/v1/verify" "$out" "$rc"
out="$(SC_PREFLIGHT_RC=1 pl_run)"; rc=$?
verdict "e2e_platform: a preflight that fails says which hop" 1 "preflight: https://app.harness.test/config.json answered 404 (want 200): PLATFORM_WEB_DIR is not served" "$out" "$rc"
verdict "e2e_platform: ... and the edge's log follows" 1 "the preflight from the Playwright image failed" "$out" "$rc"
verdict "e2e_platform: the preflight script is a valid ES module (top-level await, parsed by node)" 0 "parses" "$( . "$HERE/stage.sh"; printf '%s\n' "$E2E_PREFLIGHT" > "$T/preflight.mjs"; node --check "$T/preflight.mjs" 2>&1 && echo parses )" 0

# ── e2e_fail ──
out="$( ( set -u; WORK="$T"; . "$T/user-env.sh"; . "$HERE/stage.sh"; STAGE=e2e-example; e2e_fail m2 "module 2 broke" ) 2>&1 )"; rc=$?
verdict "e2e_fail: names the stage, prints the failing service's log, then dies with the message" 1 "the log of m2 (the last 80 lines; the stage is [e2e-example])" "$out" "$rc"
verdict "e2e_fail: ... the log itself" 1 "LOG-MARKER of m2" "$out" "$rc"
verdict "e2e_fail: ... and the message" 1 "DIE: module 2 broke" "$out" "$rc"

# ── e2e_not_checked ──
out="$( ( set -u; . "$HERE/stage.sh"; e2e_not_checked ) 2>&1 )"; rc=$?
verdict "e2e_not_checked: says it is one browser engine" 0 "ONE browser engine: Chromium" "$out" "$rc"
verdict "e2e_not_checked: ... a self-signed certificate and no real edge" 0 "SELF-SIGNED certificate" "$out" "$rc"
verdict "e2e_not_checked: ... no mail from GoTrue" 0 "GoTrue sends NO mail here" "$out" "$rc"
verdict "e2e_not_checked: ... what the traces hold" 0 "the traces of a FAILED test hold the test users' passwords and tokens" "$out" "$rc"
verdict "e2e_not_checked: ends with one grep-able line" 0 "line-ok" "$(printf '%s\n' "$out" | tail -1 | grep -q '^NOT CHECKED: any browser but Chromium' && echo line-ok || echo LINE-MISSING)" 0

# ── consistency: the compose file ──
COMPOSE="$HARNESS/docker-compose.e2e.yml"; RUNSH="$HARNESS/run.sh"; NGINX="$HERE/nginx.conf"; GOTRUE_COMPOSE="$HARNESS/docker-compose.gotrue.yml"; STG="$HERE/stage.sh"
blk() { awk -v s="  $1:" '$0==s {p=1; next} p && /^  [a-z0-9]/ {p=0} p' "$COMPOSE"; }
nginx_img="$(sed -n 's/^    image: \(nginx:[A-Za-z0-9._-]*@sha256:[0-9a-f]\{64\}\)$/\1/p' "$COMPOSE")"
pw_img="$(sed -n 's#^    image: \(mcr.microsoft.com/playwright:v[0-9.]*-[a-z]*@sha256:[0-9a-f]\{64\}\)$#\1#p' "$COMPOSE")"
verdict "consistency: the nginx image is pinned name:tag@sha256:<64 hex>" 0 "pinned" "$([ -n "$nginx_img" ] && [ "$(printf '%s\n' "$nginx_img" | wc -l | tr -d ' ')" = 1 ] && echo pinned || echo "none or many")" 0
verdict "consistency: the Playwright image is pinned name:tag@sha256:<64 hex>" 0 "pinned" "$([ -n "$pw_img" ] && [ "$(printf '%s\n' "$pw_img" | wc -l | tr -d ' ')" = 1 ] && echo pinned || echo "none or many")" 0
verdict "consistency: those are the only two images the file names" 0 "two" "$([ "$(grep -cE '^[[:space:]]*image:' "$COMPOSE")" = 2 ] && echo two || echo "$(grep -cE '^[[:space:]]*image:' "$COMPOSE")")" 0
pkg_pw="$(node -e 'const p=require(process.argv[1]);process.stdout.write(p.devDependencies["@playwright/test"]||"")' "$ROOT/web/package.json")"
lock_pw="$(node -e 'const l=require(process.argv[1]);process.stdout.write((l.packages["node_modules/@playwright/test"]||{}).version||"")' "$ROOT/web/package-lock.json")"
lock_core="$(node -e 'const l=require(process.argv[1]);process.stdout.write((l.packages["node_modules/playwright-core"]||{}).version||"")' "$ROOT/web/package-lock.json")"
tag_pw="$(printf '%s' "$pw_img" | sed -n 's#^mcr.microsoft.com/playwright:v\([0-9.]*\)-.*#\1#p')"
verdict "consistency: web/package.json pins @playwright/test to an EXACT version (no range)" 0 "exact" "$(printf '%s\n' "$pkg_pw" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' && echo exact || echo "'$pkg_pw'")" 0
verdict "consistency: the image tag is the version of @playwright/test in web/package.json" 0 "same" "$([ -n "$tag_pw" ] && [ "$tag_pw" = "$pkg_pw" ] && echo same || echo "image $tag_pw, package $pkg_pw")" 0
verdict "consistency: the lockfile holds that very version of @playwright/test and of playwright-core" 0 "same" "$([ "$lock_pw" = "$pkg_pw" ] && [ "$lock_core" = "$pkg_pw" ] && echo same || echo "lock $lock_pw core $lock_core package $pkg_pw")" 0
verdict "consistency: @axe-core/playwright is pinned to an exact version too" 0 "exact" "$(node -e 'const p=require(process.argv[1]);process.stdout.write(/^[0-9]+\.[0-9]+\.[0-9]+$/.test(p.devDependencies["@axe-core/playwright"]||"")?"exact":"not exact")' "$ROOT/web/package.json")" 0
verdict "consistency: the Playwright version is not in dependencies (a test tool is a devDependency)" 0 "dev-only" "$(node -e 'const p=require(process.argv[1]);process.stdout.write(Object.keys(p.dependencies||{}).some(k=>/playwright|axe/.test(k))?"in dependencies":"dev-only")' "$ROOT/web/package.json")" 0
verdict "consistency: the compose file publishes no port" 0 "none" "$(grep -qE '^[[:space:]]*ports:' "$COMPOSE" && echo "a ports: key" || echo none)" 0
svcs="$(grep -E '^  [a-z0-9-]+:$' "$COMPOSE" | tr -d ' :' | tr '\n' ' ')"
verdict "consistency: the file touches exactly gotrue, smtp-sink, m1j, e2e-platform, e2e-edge and e2e-runner" 0 "six" "$([ "$svcs" = "gotrue smtp-sink m1j e2e-platform e2e-edge e2e-runner " ] && echo six || echo "$svcs")" 0
verdict "consistency: the three services it ADDS are in the profile e2e (they cannot start by accident)" 0 "profiled" "$(n=0; for s in e2e-platform e2e-edge e2e-runner; do blk "$s" | grep -q '^    profiles: \[e2e\]$' && n=$((n+1)); done; [ "$n" = 3 ] && echo profiled || echo "$n of 3")" 0
verdict "consistency: the three added services are no-new-privileges, drop every capability and sit on the private network only" 0 "hardened" "$(n=0; for s in e2e-platform e2e-edge e2e-runner; do b="$(blk "$s")"; printf '%s\n' "$b" | grep -q 'no-new-privileges:true' && printf '%s\n' "$b" | grep -q 'cap_drop: \[ALL\]' && printf '%s\n' "$b" | grep -qE 'networks: \[private\]|^      private:' && n=$((n+1)); done; [ "$n" = 3 ] && echo hardened || echo "$n of 3")" 0
verdict "consistency: the platform and the edge have a read-only file system" 0 "read-only" "$(n=0; for s in e2e-platform e2e-edge; do blk "$s" | grep -q '^    read_only: true$' && n=$((n+1)); done; [ "$n" = 2 ] && echo read-only || echo "$n of 2")" 0
verdict "consistency: no service of the file sets a fault point or the build tag that arms it (the platform here is the production build)" 0 "none" "$(grep -vE '^[[:space:]]*#' "$COMPOSE" | grep -qiE 'faultpoint|GO_TAGS' && echo present || echo none)" 0
rv="$(blk e2e-runner | grep -oE '\$\{H_[A-Z0-9_]+' | sed 's/^\${//' | LC_ALL=C sort -u | tr '\n' ' ')"
verdict "consistency: the browser's container is given only the anon key and the run's own paths and ids (no JWT secret, service key, database or module credential)" 0 "clean" "$([ "$rv" = "H_E2E_ANON_KEY H_HOST_GID H_HOST_UID H_PLATFORM_DIR H_WORK " ] && echo clean || echo "$rv")" 0
verdict "consistency: the browser's container mounts the specs and the fixtures READ-ONLY and the artifacts read-write" 0 "mounts" "$(b="$(blk e2e-runner)"; printf '%s\n' "$b" | grep -q ':/work/web:ro$' && printf '%s\n' "$b" | grep -q ':/fixtures:ro$' && printf '%s\n' "$b" | grep -qE ':/artifacts$' && echo mounts || echo wrong)" 0
verdict "consistency: the platform mounts the built app read-only at the path PLATFORM_WEB_DIR names" 0 "web" "$(b="$(blk e2e-platform)"; printf '%s\n' "$b" | grep -q '^      PLATFORM_WEB_DIR: /web$' && printf '%s\n' "$b" | grep -q '/web/dist:/web:ro$' && echo web || echo wrong)" 0
verdict "consistency: the browser's container runs as the run's user and holds HOME in a tmpfs" 0 "user" "$(b="$(blk e2e-runner)"; printf '%s\n' "$b" | grep -q 'user: "${H_HOST_UID:?}:${H_HOST_GID:?}"' && printf '%s\n' "$b" | grep -q 'tmpfs: \[/tmp\]' && printf '%s\n' "$b" | grep -q '^      HOME: /tmp$' && echo user || echo wrong)" 0
verdict "consistency: Playwright's container is started with an init process and a shared memory of its own" 0 "init-shm" "$(b="$(blk e2e-runner)"; printf '%s\n' "$b" | grep -q '^    init: true$' && printf '%s\n' "$b" | grep -q '^    shm_size: 1gb$' && echo init-shm || echo wrong)" 0

# ── the names, written in four places ──
APP="https://app.harness.test"; AUTHB="https://auth.harness.test/auth/v1"
verdict "names: the stage's constants are the two hosts, the app origin and the GoTrue address" 0 "constants" "$( . "$STG"; [ "$E2E_APP_HOST" = app.harness.test ] && [ "$E2E_AUTH_HOST" = auth.harness.test ] && [ "$E2E_APP_URL" = "$APP" ] && [ "$E2E_AUTH_URL" = "$AUTHB" ] && [ "$E2E_ISSUER" = "$AUTHB" ] && echo constants || echo wrong)" 0
verdict "names: the compose file's two anchors are those addresses" 0 "anchors" "$([ "$(grep -c "^x-e2e-app: &e2e-app \"$APP\"\$" "$COMPOSE")" = 1 ] && [ "$(grep -c "^x-e2e-issuer: &e2e-issuer \"$AUTHB\"\$" "$COMPOSE")" = 1 ] && echo anchors || echo wrong)" 0
verdict "names: the edge answers to both names (aliases), and nginx.conf has a server for each" 0 "aliases" "$(blk e2e-edge | grep -q 'aliases: \[app.harness.test, auth.harness.test\]' && grep -q 'server_name app.harness.test;' "$NGINX" && grep -q 'server_name auth.harness.test;' "$NGINX" && echo aliases || echo wrong)" 0
verdict "names: nginx.conf hands the app name to the platform's service and port, and /auth/v1/ to GoTrue's (the port GoTrue's own compose file listens on)" 0 "upstreams" "$(gp="$(sed -n 's/^      GOTRUE_API_PORT: "\([0-9]*\)"$/\1/p' "$GOTRUE_COMPOSE")"; pp="$(blk e2e-platform | sed -n 's/^      PLATFORM_HTTP_ADDR: ":\([0-9]*\)"$/\1/p')"; grep -q "proxy_pass http://e2e-platform:$pp;" "$NGINX" && grep -q "location /auth/v1/ { proxy_pass http://gotrue:$gp/; }" "$NGINX" && echo upstreams || echo "gotrue $gp platform $pp")" 0
verdict "names: GoTrue is told its public address, its site and its issuer (the platform's names, by the anchors)" 0 "gotrue-names" "$(b="$(blk gotrue)"; printf '%s\n' "$b" | grep -q '^      API_EXTERNAL_URL: \*e2e-issuer$' && printf '%s\n' "$b" | grep -q '^      GOTRUE_SITE_URL: \*e2e-app$' && printf '%s\n' "$b" | grep -q '^      GOTRUE_JWT_ISSUER: \*e2e-issuer$' && echo gotrue-names || echo wrong)" 0
verdict "names: the platform is told the same issuer, the app's own address as its public address and GoTrue's address as its public Supabase URL" 0 "platform-names" "$(b="$(blk e2e-platform)"; printf '%s\n' "$b" | grep -q '^      PLATFORM_JWT_ISSUER: \*e2e-issuer$' && printf '%s\n' "$b" | grep -q '^      PLATFORM_PUBLIC_BASE_URL: \*e2e-app$' && printf '%s\n' "$b" | grep -q '^      PLATFORM_PUBLIC_SUPABASE_URL: \*e2e-issuer$' && echo platform-names || echo wrong)" 0
verdict "names: the browser's container is given the same two addresses" 0 "runner-names" "$(b="$(blk e2e-runner)"; printf '%s\n' "$b" | grep -q '^      E2E_BASE_URL: \*e2e-app$' && printf '%s\n' "$b" | grep -q '^      E2E_GOTRUE_URL: \*e2e-issuer$' && echo runner-names || echo wrong)" 0
verdict "names: the platform's public address is https (a dial the platform refuses otherwise) and the stage's certificate names both hosts" 0 "https-sans" "$(grep -q '^x-e2e-app: &e2e-app "https://' "$COMPOSE" && grep -q 'subjectAltName=DNS:$E2E_APP_HOST,DNS:$E2E_AUTH_HOST' "$STG" && echo https-sans || echo wrong)" 0
verdict "names: the platform's JWT secret and audience are GoTrue's (one variable, one value; the audience GoTrue stamps)" 0 "same-secret" "$(ga="$(sed -n 's/^      GOTRUE_JWT_AUD: //p' "$GOTRUE_COMPOSE")"; grep -q '^      PLATFORM_JWT_SECRET: ${H_JWT_SECRET:?}$' "$COMPOSE" && grep -q '^      GOTRUE_JWT_SECRET: ${H_JWT_SECRET:?}$' "$GOTRUE_COMPOSE" && grep -q "^      PLATFORM_JWT_AUDIENCE: $ga\$" "$COMPOSE" && echo same-secret || echo "audience $ga")" 0
verdict "names: the unsubscribe secret is a variable of its own, never GoTrue's or module 1's" 0 "own" "$(grep -q '^      PLATFORM_UNSUBSCRIBE_SECRET: ${H_E2E_UNSUBSCRIBE_SECRET:?}$' "$COMPOSE" && ! grep -q 'PLATFORM_UNSUBSCRIBE_SECRET: ${H_JWT_SECRET\|PLATFORM_UNSUBSCRIBE_SECRET: ${H_JOBS' "$COMPOSE" && echo own || echo wrong)" 0
verdict "names: the sink alias module 3's accounts name is the one the compose file of the harness gives the sink, and the stage and the runner agree" 0 "alias" "$(grep -q 'aliases: \[smtp.gmail.com, 10.smtp-ctl.example.test\]' "$HARNESS/docker-compose.yml" && . "$STG" && [ "$E2E_M3_SINK_HOST" = 10.smtp-ctl.example.test ] && blk e2e-runner | grep -q '^      E2E_M3_SINK_HOST: 10.smtp-ctl.example.test$' && echo alias || echo wrong)" 0
verdict "names: the sink's control address is the one the stage writes into the fixtures and the runner is given, on the port the sink's compose file serves it on" 0 "sink" "$(blk e2e-runner | grep -q '^      E2E_SINK_URL: http://smtp-ctl:8025$' && grep -q 'E2E_SINK_URL="http://smtp-ctl:8025"' "$STG" && grep -q 'SINK_HTTP_ADDR: ":8025"' "$HARNESS/docker-compose.yml" && echo sink || echo wrong)" 0

# ── GoTrue's variables: the names its image reads, and the link they make (the fault of the first runner run) ──
gblk="$(blk gotrue)"
verdict "gotrue overlay: every variable set on GoTrue is GOTRUE_-prefixed or the one bare name GoTrue's own source names as an alternative (API_EXTERNAL_URL)" 0 "names-ok" "$(bad=""; got=0; for n in $(printf '%s\n' "$gblk" | grep -oE '^      [A-Z0-9_]+' | tr -d ' '); do got=$((got+1)); case "$n" in GOTRUE_*|API_EXTERNAL_URL) ;; *) bad="$bad $n";; esac; done; [ "$got" -ge 8 ] && [ -z "$bad" ] && echo names-ok || echo "read $got, not read by GoTrue:$bad")" 0
verdict "gotrue overlay: CONTROL: a bare name that GoTrue does not read would be named (the check can fail)" 0 "caught" "$(bad=""; for n in SITE_URL API_EXTERNAL_URL MAILER_URLPATHS_INVITE; do case "$n" in GOTRUE_*|API_EXTERNAL_URL) ;; *) bad="$bad $n";; esac; done; [ "$bad" = " SITE_URL MAILER_URLPATHS_INVITE" ] && echo caught || echo "$bad")" 0
verdict "gotrue overlay: the external URL is set under both of its names, to the issuer anchor" 0 "both" "$(printf '%s\n' "$gblk" | grep -q '^      API_EXTERNAL_URL: \*e2e-issuer$' && printf '%s\n' "$gblk" | grep -q '^      GOTRUE_API_EXTERNAL_URL: \*e2e-issuer$' && echo both || echo wrong)" 0
verdict "gotrue overlay: the four mailer URL paths are the one verify-path anchor" 0 "four" "$(n=0; for k in INVITE CONFIRMATION RECOVERY EMAIL_CHANGE; do printf '%s\n' "$gblk" | grep -q "^      GOTRUE_MAILER_URLPATHS_$k: \*e2e-verify\$" && n=$((n+1)); done; [ "$n" = 4 ] && echo four || echo "$n of 4")" 0
resolve() { MSYS_NO_PATHCONV=1 node -e 'process.stdout.write(new URL(process.argv[1], process.argv[2]).href)' "$1" "$2"; }   # RFC 3986 resolution: what Go's URL.ResolveReference does in GoTrue's templatemailer
ext="$(sed -n 's/^x-e2e-issuer: &e2e-issuer "\(.*\)"$/\1/p' "$COMPOSE")"
vp="$(sed -n 's/^x-e2e-verify: &e2e-verify "\([^"]*\)".*$/\1/p' "$COMPOSE")"
verdict "gotrue overlay: the link's prefix (the mailer path resolved against the external URL, as GoTrue does) is the issuer plus /verify" 0 "ok" "$([ -n "$ext" ] && [ -n "$vp" ] && [ "$(resolve "$vp" "$ext")" = "$ext/verify" ] && echo ok || echo "ext [$ext] path [$vp] gives [$(resolve "$vp" "$ext")]")" 0
verdict "gotrue overlay: CONTROL: GoTrue's default mailer path /verify, resolved the same way, LOSES the /auth/v1 (the link of the first runner run), so this check can fail" 0 "lost" "$([ "$(resolve /verify "$ext")" = "https://auth.harness.test/verify" ] && [ "$(resolve /verify "$ext")" != "$ext/verify" ] && echo lost || echo "$(resolve /verify "$ext")")" 0
verdict "gotrue overlay: the stage's verify path is the compose file's, and the stage checks the running container for the variables that make the link" 0 "checked" "$( . "$STG"; [ "$E2E_VERIFY_PATH" = "$vp" ] && b="$(sed -n '/^e2e_platform() {/,/^}/p' "$STG")" && printf '%s\n' "$b" | grep -q 'GOTRUE_MAILER_URLPATHS_INVITE=\$E2E_VERIFY_PATH' && printf '%s\n' "$b" | grep -q 'GOTRUE_API_EXTERNAL_URL=\$E2E_ISSUER' && echo checked || echo wrong)" 0
verdict "gotrue overlay: the app origin, and what is under it, is on the redirect allow list besides GoTrue's own site URL" 0 "allowed" "$(printf '%s\n' "$gblk" | grep -qF '      GOTRUE_URI_ALLOW_LIST: "https://app.harness.test,https://app.harness.test/**"' && echo allowed || echo wrong)" 0
verdict "smtp-sink overlay: Mailpit is told not to look for a newer version (the name v1.33.1 reads), so nothing in the logs depends on api.github.com" 0 "quiet" "$(blk smtp-sink | grep -q '^      MP_DISABLE_VERSION_CHECK: "true"$' && echo quiet || echo wrong)" 0
verdict "nginx.conf: /auth/v1/ is taken off before GoTrue sees the request, and the query (the invite token) is kept: the trailing slash of proxy_pass, no rewrite that drops it" 0 "strip" "$(grep -q 'location /auth/v1/ { proxy_pass http://gotrue:9999/; }' "$NGINX" && ! grep -qE 'rewrite |proxy_pass .*\$' "$NGINX" && echo strip || echo wrong)" 0

# ── the platform's dials against the Go code that reads them ──
gonames="$(for f in "$ROOT"/internal/config/*.go; do case "$f" in *_test.go) ;; *) cat "$f";; esac; done | grep -oE '"PLATFORM_[A-Z0-9_]+"' | tr -d '"' | sort -u)"
mynames="$(blk e2e-platform | grep -oE '^      PLATFORM_[A-Z0-9_]+' | tr -d ' ' | sort -u)"
verdict "dials: the Go code names at least thirty PLATFORM_ dials (the instrument matched something)" 0 "found" "$([ "$(printf '%s\n' "$gonames" | wc -l | tr -d ' ')" -ge 30 ] && echo found || echo "$(printf '%s\n' "$gonames" | wc -l | tr -d ' ')")" 0
verdict "dials: every PLATFORM_ variable of e2e-platform is a dial internal/config reads (a renamed or misspelt dial is red)" 0 "all-known" "$(miss=""; for n in $mynames; do printf '%s\n' "$gonames" | grep -qx "$n" || miss="$miss $n"; done; [ -z "$miss" ] && echo all-known || echo "unknown to the Go code:$miss")" 0
verdict "dials: the three dials of the web app (chunk A2) are among them" 0 "web-dials" "$(for n in PLATFORM_WEB_DIR PLATFORM_PUBLIC_SUPABASE_URL PLATFORM_PUBLIC_SUPABASE_ANON_KEY; do printf '%s\n' "$mynames" | grep -qx "$n" && printf '%s\n' "$gonames" | grep -qx "$n" || { echo "missing $n"; exit 0; }; done; echo web-dials)" 0
verdict "dials: the dials that make a send job confirmable and the engine run are set" 0 "needed" "$(for n in PLATFORM_JOB_ENGINE_ENABLED PLATFORM_UNSUBSCRIBE_SECRET PLATFORM_PUBLIC_BASE_URL PLATFORM_JWT_SECRET PLATFORM_JWT_ISSUER PLATFORM_JWT_AUDIENCE PLATFORM_M1_URL PLATFORM_M1_KEY_SECRET PLATFORM_M2_URL PLATFORM_M2_SERVICE_TOKEN PLATFORM_M3_URL PLATFORM_M3_SERVICE_TOKEN PLATFORM_M3_ADMIN_TOKEN PLATFORM_DATABASE_URL; do printf '%s\n' "$mynames" | grep -qx "$n" || { echo "missing $n"; exit 0; }; done; echo needed)" 0
verdict "dials: the engine is ON and no fault point is armed" 0 "engine-on" "$(blk e2e-platform | grep -q '^      PLATFORM_JOB_ENGINE_ENABLED: "true"$' && ! blk e2e-platform | grep -q 'FAULTPOINT' && echo engine-on || echo wrong)" 0
missing=""
for v in $(grep -o '\${H_[A-Z0-9_]*' "$COMPOSE" | sed 's/^\${//' | sort -u); do
  grep -q "^$v=" "$RUNSH" || grep -q "echo \"$v=" "$STG" "$HARNESS/gotrue/stage.sh" "$HARNESS/jobs/stage.sh" "$HARNESS/m2worker/stage.sh" || missing="$missing $v"
done
verdict "dials: every H_ variable the compose file needs is written by run.sh or by a stage's mint (H_E2E_UNSUBSCRIBE_SECRET and H_E2E_ANON_KEY by e2e_mint)" 0 "written" "$([ -z "$missing" ] && echo written || echo "nobody writes:$missing")" 0
verdict "dials: e2e_mint writes exactly the two e2e variables" 0 "two" "$([ "$(sed -n '/^e2e_mint() {/,/^}/p' "$STG" | grep -o 'echo "H_E2E_[A-Z_]*=' | wc -l | tr -d ' ')" = 2 ] && echo two || echo wrong)" 0

# ── the TLS edge's configuration ──
verdict "nginx.conf: no access log (a request line holds an invite or an unsubscribe token), logs to stderr, every write path under /tmp" 0 "quiet" "$(grep -q '^  access_log off;$' "$NGINX" && grep -q '^error_log /dev/stderr' "$NGINX" && [ "$(grep -cE '^  (client_body|proxy|fastcgi|uwsgi|scgi)_temp_path /tmp/' "$NGINX")" = 5 ] && grep -q '^pid /tmp/nginx.pid;$' "$NGINX" && echo quiet || echo wrong)" 0
verdict "nginx.conf: TLS 1.2 and 1.3 on 443 with the certificate and the key the stage writes" 0 "tls" "$(grep -q 'ssl_certificate /tls/tls.crt;' "$NGINX" && grep -q 'ssl_certificate_key /tls/tls.key;' "$NGINX" && grep -q 'ssl_protocols TLSv1.2 TLSv1.3;' "$NGINX" && [ "$(grep -c 'listen 443 ssl;' "$NGINX")" = 2 ] && grep -q 'tls.key' "$STG" && grep -q 'tls.crt' "$STG" && echo tls || echo wrong)" 0
verdict "nginx.conf: no user directive (it runs as the run's user), and a plain-http health port with no upstream" 0 "unprivileged" "$(! grep -qE '^user ' "$NGINX" && grep -q 'listen 8081;' "$NGINX" && grep -q 'location = /healthz' "$NGINX" && blk e2e-edge | grep -q '8081/healthz' && echo unprivileged || echo wrong)" 0
verdict "nginx.conf: the Host and the forwarded protocol reach the platform" 0 "headers" "$(grep -q 'proxy_set_header Host $http_host;' "$NGINX" && grep -q 'proxy_set_header X-Forwarded-Proto https;' "$NGINX" && echo headers || echo wrong)" 0
verdict "nginx.conf: a CSV upload of tens of megabytes passes (client_max_body_size)" 0 "uploads" "$(grep -q 'client_max_body_size 64m;' "$NGINX" && echo uploads || echo wrong)" 0
verdict "nginx.conf: the edge binds 443 as a non-root user through the namespaced sysctl, as the fixture DNS binds 53" 0 "sysctl" "$(blk e2e-edge | grep -q 'net.ipv4.ip_unprivileged_port_start: "0"' && echo sysctl || echo wrong)" 0

# ── the specs, the config and the floor ──
SPECS="$ROOT/web/e2e"; ntests="$(cat "$SPECS"/*.spec.ts | grep -cE '^[[:space:]]+test\(')"
verdict "specs: the floor of the stage is not above the number of tests the specs declare" 0 "floor-ok" "$(fl="$( . "$STG"; echo "$E2E_MIN_TESTS" )"; [ "$ntests" -ge "$fl" ] && echo floor-ok || echo "$ntests declared, floor $fl")" 0
verdict "specs: the floor is at least the ten tests of the smoke spec" 0 "ten" "$([ "$( . "$STG"; echo "$E2E_MIN_TESTS" )" -ge 10 ] && echo ten || echo lower)" 0
verdict "specs: no spec skips, fixes, restricts or expects to fail a test (a test that cannot run FAILS)" 0 "none" "$(grep -nE 'test\.(skip|fixme|only|fail)\(|describe\.(skip|only|fixme)\(|\.skip\(' "$SPECS"/*.spec.ts "$SPECS"/support/*.ts | head -3 | grep . || echo none)" 0
verdict "specs: every check helper of the support directory has a paired control in the smoke spec (axe and the copy deny-list)" 0 "controls" "$(grep -q 'axe CONTROL' "$SPECS/smoke.spec.ts" && grep -q 'copy CONTROL' "$SPECS/smoke.spec.ts" && echo controls || echo missing)" 0
verdict "specs: the support files and the README exist" 0 "present" "$(for f in support/env.ts support/shots.ts support/axe.ts support/copy.ts support/users.ts support/sink.ts support/data.ts support/csp.ts README.md fixtures/verify-list.csv fixtures/send-list.csv fixtures/suppression-import.csv; do [ -s "$SPECS/$f" ] || { echo "missing $f"; exit 0; }; done; echo present)" 0
verdict "specs: the CSV fixtures start with their header rows" 0 "headers" "$([ "$(head -1 "$SPECS/fixtures/verify-list.csv")" = "email,first_name,company" ] && [ "$(head -1 "$SPECS/fixtures/send-list.csv")" = "email,first_name,topic" ] && [ "$(head -1 "$SPECS/fixtures/suppression-import.csv")" = "email" ] && echo headers || echo wrong)" 0
verdict "specs: the deny-list the copy helper reads exists" 0 "list" "$([ -s "$ROOT/web/src/copy/internal-words.json" ] && node -e 'const j=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));const w=Array.isArray(j)?j:j.words;process.stdout.write(Array.isArray(w)&&w.length>0&&w.every(x=>typeof x==="string")?"list":"bad")' "$ROOT/web/src/copy/internal-words.json" || echo missing)" 0
cfg="$ROOT/web/playwright.config.ts"
verdict "config: Chromium only, one worker, no retry, a test.only refused, trace kept on failure, screenshots on failure, the self-signed chain ignored" 0 "config" "$(grep -q "workers: 1," "$cfg" && grep -q "retries: 0," "$cfg" && grep -q "forbidOnly: true," "$cfg" && grep -q "trace: 'retain-on-failure'" "$cfg" && grep -q "screenshot: 'only-on-failure'" "$cfg" && grep -q "ignoreHTTPSErrors: true" "$cfg" && [ "$(grep -c "name: '" "$cfg")" = 1 ] && grep -q "name: 'chromium'" "$cfg" && echo config || echo wrong)" 0
verdict "config: the base URL comes from E2E_BASE_URL, the artifacts from E2E_ARTIFACTS_DIR, and the JSON report the stage reads is written to results.json" 0 "env" "$(grep -q "process.env\['E2E_BASE_URL'\]" "$cfg" && grep -q "process.env\['E2E_ARTIFACTS_DIR'\]" "$cfg" && grep -q "outputFile: join(artifacts, 'results.json')" "$cfg" && echo env || echo wrong)" 0
verdict "config: the report folder is not inside the run's own output directory (Playwright empties that one)" 0 "separate" "$(grep -q "outputDir: join(artifacts, 'test-results')" "$cfg" && grep -q "outputFolder: join(artifacts, 'report')" "$cfg" && echo separate || echo wrong)" 0
verdict "config: the stage reads the artifacts where the config and the compose file write them" 0 "paths" "$(blk e2e-runner | grep -q '^      E2E_ARTIFACTS_DIR: /artifacts$' && blk e2e-runner | grep -q '^      E2E_SHOTS_DIR: /artifacts/shots$' && grep -q 'e2e-artifacts/shots' "$STG" && grep -q 'e2e-artifacts/results.json' "$STG" && grep -q ':/artifacts$' "$COMPOSE" && echo paths || echo wrong)" 0
verdict "web: the unit tests (vitest) include only src/**/*.test, never the browser specs" 0 "separate" "$(grep -q "include: \['src/\*\*/\*.test.{ts,tsx}'\]" "$ROOT/web/vite.config.ts" && echo separate || echo wrong)" 0
verdict "web: npm run typecheck includes the e2e project, and the e2e tsconfig covers the config and the specs" 0 "typechecked" "$(node -e 'const p=require(process.argv[1]);process.stdout.write(p.scripts.typecheck.includes("tsconfig.e2e.json")&&p.scripts["e2e:list"]==="playwright test --list"&&p.scripts.e2e==="playwright test"?"typechecked":"wrong")' "$ROOT/web/package.json")" 0
verdict "web: the e2e tsconfig includes playwright.config.ts and the e2e directory" 0 "include" "$(grep -q '"include": \["playwright.config.ts", "e2e"\]' "$ROOT/web/tsconfig.e2e.json" && echo include || echo wrong)" 0
verdict "web: the artifact directories are git-ignored" 0 "ignored" "$(grep -qx 'e2e-artifacts' "$ROOT/web/.gitignore" && grep -qx 'playwright-report' "$ROOT/web/.gitignore" && echo ignored || echo wrong)" 0

# ── run.sh ──
verdict "run.sh: allows the scope e2e without touching the line the other selftests read" 0 "allowed" "$(grep -q 'case "\$SCOPE" in full|m1node|m2worker|gotrue|jobs) ;; sendjobs|sendjobs-m2|sendjobs-m3) ;; e2e) ;; \*) echo "H_SCOPE must be' "$RUNSH" && echo allowed || echo NOT-ALLOWED)" 0
verdict "run.sh: the scope builds what it starts that has a recipe, and nothing it only pulls" 0 "scoped" "$(want='migrate-platform migrate-m1 migrate-m2 migrate-m3 m1j jobsdns zbstub m2 m2-worker m3 m3-worker m3-worker-2 m3-worker-3 smtp-ctl m3dns e2e-platform'; grep -qE "^  e2e\) +BUILD_ONLY=\"$want\";;" "$RUNSH" && echo scoped || echo missing)" 0
verdict "run.sh: every service the scope builds has a build recipe in one of the compose files" 0 "recipes" "$(list="$(sed -n 's/^  e2e) *BUILD_ONLY="\([^"]*\)";;.*/\1/p' "$RUNSH")"; miss=""; for s in $list; do found=0; for f in "$HARNESS"/docker-compose*.yml; do awk -v s="  $s:" '$0==s {p=1; next} p && /^  [a-z0-9]/ {p=0} p && /^    build:/ {f=1} END {exit !f}' "$f" && found=1; done; [ "$found" = 1 ] || miss="$miss $s"; done; [ -n "$list" ] && [ -z "$miss" ] && echo recipes || echo "no recipe:$miss")" 0
verdict "run.sh: every service the scope builds exists in a file the scope merges (a name in BUILD_ONLY that no merged file defines would fail the build)" 0 "merged" "$(list="$(sed -n 's/^  e2e) *BUILD_ONLY="\([^"]*\)";;.*/\1/p' "$RUNSH")"; miss=""; for s in $list; do n=0; for f in docker-compose.yml docker-compose.m2worker.yml docker-compose.gotrue.yml docker-compose.jobs.yml docker-compose.e2e.yml; do grep -qE "^  $s:\$" "$HARNESS/$f" && n=1; done; [ "$n" = 1 ] || miss="$miss $s"; done; [ -z "$miss" ] && echo merged || echo "in no merged file:$miss")" 0
verdict "run.sh: sources the stage after the three whose helpers it uses" 0 "sourced" "$(e="$(grep -n '^\. "\$HERE/e2e/stage.sh"' "$RUNSH" | cut -d: -f1)"; g="$(grep -n '^\. "\$HERE/gotrue/stage.sh"' "$RUNSH" | cut -d: -f1)"; j="$(grep -n '^\. "\$HERE/jobs/stage.sh"' "$RUNSH" | cut -d: -f1)"; m="$(grep -n '^\. "\$HERE/m2worker/stage.sh"' "$RUNSH" | cut -d: -f1)"; [ -n "$e" ] && [ "$e" -gt "$g" ] && [ "$e" -gt "$j" ] && [ "$e" -gt "$m" ] && echo sourced || echo "e2e $e gotrue $g jobs $j m2worker $m")" 0
unguarded="$(grep -nE '(^|[^_a-z])e2e_(mint|pull|scope_run)( |$)' "$RUNSH" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE '\[ "\$E2E_ON" = 1 \] &&|\[ "\$SCOPE" = e2e \] &&' | head -5)"
verdict "run.sh: every call of a stage function is under the E2E_ON or the scope guard" 0 "all-guarded" "$([ -z "$unguarded" ] && echo all-guarded || echo "UNGUARDED: $unguarded")" 0
verdict "run.sh: e2e_mint is called once, after gotrue_mint and jobs_mint and before the build" 0 "minted" "$(m="$(grep -nE '^\[ "\$E2E_ON" = 1 \] && e2e_mint( |$)' "$RUNSH" | cut -d: -f1)"; g="$(grep -nE '^\[ "\$GT_ON" = 1 \] && gotrue_mint( |$)' "$RUNSH" | cut -d: -f1)"; j="$(grep -nE '^\[ "\$JOBS_ON" = 1 \] && jobs_mint( |$)' "$RUNSH" | cut -d: -f1)"; b="$(grep -n '^STAGE=build; banner 3' "$RUNSH" | cut -d: -f1)"; [ "$(printf '%s\n' "$m" | grep -c .)" = 1 ] && [ "$m" -gt "$g" ] && [ "$m" -gt "$j" ] && [ "$m" -lt "$b" ] && echo minted || echo "e2e $m gotrue $g jobs $j build $b")" 0
verdict "run.sh: e2e_pull is called once, in the build stage after the images are built" 0 "pulled" "$(p="$(grep -nE '^\[ "\$E2E_ON" = 1 \] && e2e_pull( |$)' "$RUNSH" | cut -d: -f1)"; b="$(grep -n '^DC build --pull --no-cache' "$RUNSH" | cut -d: -f1)"; d="$(grep -n '^STAGE=database; banner 4' "$RUNSH" | cut -d: -f1)"; [ "$(printf '%s\n' "$p" | grep -c .)" = 1 ] && [ "$p" -gt "$b" ] && [ "$p" -lt "$d" ] && echo pulled || echo "pull $p build $b database $d")" 0
verdict "run.sh: the scope run is called once, after stage 4b (the first admin) and before stage 5" 0 "after-4b" "$(c="$(grep -nE '^\[ "\$SCOPE" = e2e \] && e2e_scope_run' "$RUNSH" | cut -d: -f1)"; a="$(grep -n '^STAGE=first-admin; banner "4b"' "$RUNSH" | cut -d: -f1)"; s="$(grep -n '^STAGE=services; banner 5 ' "$RUNSH" | cut -d: -f1)"; [ "$(printf '%s\n' "$c" | grep -c .)" = 1 ] && [ "$c" -gt "$a" ] && [ "$c" -lt "$s" ] && echo after-4b || echo "call $c 4b $a stage5 $s")" 0
verdict "run.sh: the compose function merges the e2e overlay LAST (after the gotrue, jobs and m2worker files whose services it overrides)" 0 "last" "$(l="$(grep -n '^DC() {' "$RUNSH" | head -1 | cut -d: -f1)"; line="$(sed -n "${l}p" "$RUNSH")"; pos() { printf '%s' "$line" | grep -bo -e "$1" | head -1 | cut -d: -f1; }; e="$(pos 'E2E_DC\[@\]')"; g="$(pos 'GT_DC\[@\]')"; j="$(pos 'JOBS_DC\[@\]')"; m="$(pos 'M2W_DC\[@\]')"; en="$(pos '--env-file')"; [ -n "$e" ] && [ "$e" -gt "$g" ] && [ "$e" -gt "$j" ] && [ "$e" -gt "$m" ] && [ "$e" -lt "$en" ] && echo last || echo "e2e $e gt $g jobs $j m2w $m env $en")" 0
verdict "run.sh: the overlay is merged only through E2E_DC" 0 "merged" "$(grep -q 'E2E_DC=(-f "\$HERE/docker-compose.e2e.yml" --profile e2e)' "$RUNSH" && [ "$(grep -c 'docker-compose.e2e.yml' "$RUNSH" | tr -d ' ')" -ge 1 ] && [ "$(grep -vE '^[[:space:]]*#' "$RUNSH" | grep -c -- '-f "$HERE/docker-compose.e2e.yml"')" = 1 ] && echo merged || echo wrong)" 0
verdict "run.sh: the scope is in the header's stage list" 0 "listed" "$(grep -q '^#   5g e2e ' "$RUNSH" && echo listed || echo missing)" 0

# DORMANCY and E2E_ON, executed on the real text of run.sh
block="$(sed -n '/^E2E_ON=0; E2E_DC=()$/,/^fi$/p' "$RUNSH")"
verdict "dormancy: the guard block of run.sh was found (its subject is not empty)" 0 "found" "$([ "$(printf '%s\n' "$block" | wc -l | tr -d ' ')" = 8 ] && echo found || echo "$(printf '%s\n' "$block" | wc -l) lines")" 0
e2e_state() { # scope full-includes
  ( set -u; HERE=/h; SCOPE="$1"; FULL_INCLUDES="$2"; M2W_ON=0; M2W_DC=(); M2W_EXPOSE=""; GT_ON=0; GT_DC=(); GT_EXPOSE=""; JOBS_ON=0; JOBS_DC=(); eval "$block"
    printf 'E2E_ON=%s e2e=%s m2w=%s/%s gt=%s/%s jobs=%s/%s' "$E2E_ON" "${#E2E_DC[@]}" "$M2W_ON" "${#M2W_DC[@]}" "$GT_ON" "${#GT_DC[@]}" "$JOBS_ON" "${#JOBS_DC[@]}" )
}
verdict "dormancy: the full run is untouched (nothing on, no file merged)" 0 "E2E_ON=0 e2e=0 m2w=0/0 gt=0/0 jobs=0/0" "$(e2e_state full '')" 0
verdict "dormancy: the full run with FULL_INCLUDES as it stands in run.sh" 0 "E2E_ON=0 e2e=0 m2w=0/0 gt=0/0 jobs=0/0" "$(e2e_state full "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1)")" 0
verdict "dormancy: the full run with every stage named in FULL_INCLUDES, e2e among them: still off (the scope is not nameable there)" 0 "E2E_ON=0 e2e=0 m2w=0/0 gt=0/0 jobs=0/0" "$(e2e_state full 'm2worker m1node gotrue jobs sendjobs e2e')" 0
verdict "dormancy: the scope gotrue does not switch it on" 0 "E2E_ON=0 e2e=0 m2w=0/0 gt=0/0 jobs=0/0" "$(e2e_state gotrue 'e2e')" 0
verdict "dormancy: the scope sendjobs does not switch it on" 0 "E2E_ON=0 e2e=0 m2w=0/0 gt=0/0 jobs=0/0" "$(e2e_state sendjobs 'e2e')" 0
verdict "dormancy: a scope whose name merely contains e2e does not switch it on" 0 "E2E_ON=0 e2e=0" "$(e2e_state e2e2 '')" 0
verdict "dormancy: the scope e2e switches on itself and the three stages it needs, each with its compose file and profile" 0 "E2E_ON=1 e2e=4 m2w=1/4 gt=1/4 jobs=1/4" "$(e2e_state e2e '')" 0

# ── the workflows ──
HW="$ROOT/.github/workflows/harness.yml"
verdict "workflows: the harness workflow offers the scope e2e" 0 "offered" "$(grep -qE '^          - e2e$' "$HW" && echo offered || echo missing)" 0
up="$(sed -n '/- name: The browser run.s artifacts/,/if-no-files-found/p' "$HW")"
want_if="        if: \${{ always() && inputs.scope == 'e2e' }}"
verdict "workflows: the artifact step runs whatever the harness step ended with, for the scope e2e only" 0 "always" "$(printf '%s\n' "$up" | grep -qxF "$want_if" && echo always || echo wrong)" 0
verdict "workflows: ... it uploads the directory the stage writes, under the name e2e-artifacts, for 14 days" 0 "upload" "$(printf '%s\n' "$up" | grep -q '^          name: e2e-artifacts$' && printf '%s\n' "$up" | grep -q '^          path: ${{ runner.temp }}/harness/e2e-artifacts$' && printf '%s\n' "$up" | grep -q '^          retention-days: 14$' && echo upload || echo wrong)" 0
verdict "workflows: ... and is pinned by a full commit SHA with its tag in a trailing comment, like every other action" 0 "pinned" "$(printf '%s\n' "$up" | grep -qE '^        uses: actions/upload-artifact@[0-9a-f]{40} # v[0-9]+' && echo pinned || echo wrong)" 0
verdict "workflows: every uses: of the harness workflow is pinned by a full commit SHA" 0 "all-pinned" "$([ -z "$(grep -E '^\s*(- )?uses:' "$HW" | grep -vE '@[0-9a-f]{40} # v' )" ] && echo all-pinned || echo unpinned)" 0
verdict "workflows: the harness workflow still asks for no permission beyond contents: read" 0 "read-only" "$([ "$(sed -n '/^permissions:/,/^[a-z]/p' "$HW" | grep -cE '^  [a-z-]+:')" = 1 ] && grep -q '^  contents: read$' "$HW" && echo read-only || echo wider)" 0
verdict "workflows: Node (the version web/ asks for) is set up for the scope e2e only, before the harness step" 0 "node" "$(a="$(grep -n 'actions/setup-node@' "$HW" | cut -d: -f1)"; h="$(grep -n '^      - name: The harness$' "$HW" | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$h" ] && sed -n "$((a+1)),$((a+4))p" "$HW" | grep -q "if: inputs.scope == 'e2e'" && sed -n "$((a+1)),$((a+4))p" "$HW" | grep -q 'node-version-file: coldmail-platform/web/.node-version' && echo node || echo wrong)" 0
verdict "workflows: the gates workflow runs this selftest" 0 "wired" "$(grep -q 'tests/harness/e2e/stage-selftest.sh' "$ROOT/.github/workflows/gates.yml" && echo wired || echo missing)" 0
verdict "workflows: the web workflow lists the specs (a list of zero is red) and sets PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD" 0 "web-wired" "$(w="$ROOT/.github/workflows/web.yml"; grep -q 'npm run e2e:list' "$w" && grep -q "grep -qE '^Total: \[1-9\]" "$w" && grep -q 'PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD: "1"' "$w" && echo web-wired || echo wrong)" 0

# ── a leak must not be dumped by the run's own failure path ──
nd="$(scan_run 'engine started' none M2_TOKEN)"; rc=$?
verdict "e2e_scan_log: a secret in a log tells run.sh not to dump the services' logs (E2E_NO_DUMP)" 1 "NO_DUMP=1" "$nd" "$rc"
nd="$(scan_run "Bearer $FAKE_JWT")"; rc=$?
verdict "e2e_scan_log: a JWT-shaped string does too" 1 "NO_DUMP=1" "$nd" "$rc"
nd="$(scan_run 'engine started')"; rc=$?
verdict "e2e_scan_log: a clean log dies with nothing and so tells run.sh nothing (the control of the two cases above)" 0 "clean" "$([ -z "$nd" ] && echo clean || echo "$nd")" "$rc" "NO_DUMP=1"
dl="$(grep -m1 '^dump_logs() {' "$RUNSH")"
dump_state() { ( set -u; DC() { echo "DC $*"; }; eval "$dl"; E2E_NO_DUMP="$1"; dump_logs ) 2>&1; }
verdict "run.sh: dump_logs dumps the services' logs as it always did" 0 "DC logs --no-color --tail=120" "$(dump_state 0)" 0
verdict "run.sh: ... and when the variable is empty" 0 "DC logs --no-color --tail=120" "$(dump_state '')" 0
verdict "run.sh: dump_logs does NOT dump them when a stage found a secret in a log" 0 "service logs NOT dumped" "$(dump_state 1)" 0 "DC logs"

# ── the host directories the compose file bind-mounts ──
verdict "consistency: every host directory the compose file bind-mounts under H_WORK is made by the stage as the run's user before a container needs it (Docker would make it as root)" 0 "made" "$(miss=""; mounted="$(grep -oE '\$\{H_WORK:\?\}/[A-Za-z0-9/_-]+' "$COMPOSE" | sed 's#^\${H_WORK:?}/##' | sort -u | tr '\n' ' ')"; for d in e2e-fixtures e2e-artifacts; do grep -q "mkdir -p .*\"\$WORK/$d\"" "$STG" || miss="$miss $d"; done; grep -q 'local d="$WORK/e2e/tls"' "$STG" && grep -q 'mkdir -p "$d"' "$STG" || miss="$miss e2e/tls"; [ "$mounted" = "e2e-artifacts e2e-fixtures e2e/tls " ] && [ -z "$miss" ] && echo made || echo "mounted [$mounted] not made [$miss]")" 0
verdict "consistency: the scope's first lines make those directories before the web app, GoTrue and every container" 0 "first" "$(body="$(sed -n '/^e2e_scope_run() {/,/^}/p' "$STG")"; m="$(printf '%s\n' "$body" | grep -n 'mkdir -p "$WORK/e2e-artifacts" "$WORK/e2e-fixtures"' | head -1 | cut -d: -f1)"; w="$(printf '%s\n' "$body" | grep -n '^  e2e_web$' | head -1 | cut -d: -f1)"; [ -n "$m" ] && [ -n "$w" ] && [ "$m" -lt "$w" ] && echo first || echo "mkdir $m web $w")" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; bad=$((bad+1)); fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
