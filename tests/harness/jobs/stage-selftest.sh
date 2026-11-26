#!/usr/bin/env bash
# stage-selftest.sh — proves the checks of tests/harness/jobs/stage.sh go RED, for the reason they name, and stay green on a good input; and that the files of the jobs scope agree with each other (the list of fault
# points and the registry of internal/faultpoint, the list of invariants of the scenario program and of the stage, the compose file, the environment the scenario program reads, the wiring in run.sh and the workflows).
# No Docker, no Go build: the functions are sourced and run against stand-ins for docker, docker compose, openssl and psql. What it covers:
#   jobs_mint          writes exactly two lines to the env file (two different 64-character secrets), masks each BEFORE it is written, never prints either, refuses a tool that printed nothing, the wrong length or one
#                      value twice, a missing env file and (where the host keeps file modes) one that is not mode 600
#   jobs_get, jobs_fact the members of a fact line are read exactly (a name that is the suffix of another is not confused with it)
#   jobs_expect_oks    green when every OK line of JOBS_VERIFY_OKS is printed once; red, naming the line, when ANY ONE is missing (one case per name), printed twice or joined by a line the stage does not know; red on no output
#   jobs_expect_fail   the program's own control: green when it fails for the named reason; red when it exits 0, and red when it fails for another reason
#   jobs_prove_killed  green on exit 137, OOMKilled false and the armed build's log line; red on any other exit status, on the OOM killer, on a container that is still running, on a missing or doubled log line
#   jobs_summary       red when the stages ran fewer verify runs than they make, and when the OK lines are not runs x invariants; green otherwise
#   jobs_secret_leak   green on clean logs; red, naming the log and never printing it, when either secret or anything shaped like a JWT is in a log; red on an empty log or empty secrets
#   consistency        the kill points are exactly the registry's names; the stage's list of invariants is the scenario program's, in order, and each is checked once in verify.go; the compose file publishes no port, names every
#                      service of the scope in the profile jobs, arms the fault points from the two variables, keeps the engine on in the instances and off in the legacy ones, and gives the scenario program every variable it
#                      reads; every H_ variable it needs is written by run.sh or jobs_mint; run.sh allows the scope, sources the stage, builds only what it needs and calls every jobs_ function only under its guard; the stage is
#                      DORMANT in `full` until FULL_INCLUDES names it (the guard block of run.sh is evaluated for settings); the workflows offer the scope, vet the scenario program and run this selftest
# NOT covered here (they need a Linux host with Docker): the real docker compose, wget, psql, module 1, the platform image and the kills themselves.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"; ROOT="$(cd "$HARNESS/../.." && pwd)"
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
banner() { echo "BANNER [$1] $2"; }
MASKLOG="$WORK/mask.log"
mask() { if [ -f "$WORK/harness.env" ] && grep -qF -- "$1" "$WORK/harness.env"; then echo "AFTER_WRITE" >> "$MASKLOG"; else echo "BEFORE_WRITE" >> "$MASKLOG"; fi; }
DC() { echo "DC called: $*" >&2; return 1; }
EOF

# ── jobs_mint ──
bumpf() { local n; n="$(cat "$T/ctr.$1" 2>/dev/null || echo 0)"; n=$((n+1)); echo "$n" > "$T/ctr.$1"; echo "$n"; }
mint_run() { # mode: good | empty | short | same | noenv | mode644 | mode600
  W="$T/w-$1"; rm -rf "$W"; mkdir -p "$W"; rm -f "$T"/ctr.*; ( umask 077; : > "$W/harness.env" ); [ "$1" != noenv ] || rm -f "$W/harness.env"
  ( set -u; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    case "$1" in
      empty) openssl() { :; };;
      short) openssl() { printf 'abcd\n'; };;
      same)  openssl() { printf '%064x\n' 7; };;
      *)     openssl() { printf '%064x\n' "$(( $(bumpf ossl) * 4097 + 31 ))"; };;
    esac
    case "$1" in
      mode644) uname() { echo Linux; }; stat() { echo 644; };;
      mode600) uname() { echo Linux; }; stat() { echo 600; };;
      *)       uname() { echo Other; };;
    esac
    jobs_mint ) 2>&1
}
out="$(mint_run good)"; rc=$?
verdict "jobs_mint: a good mint" 0 "masked, none printed" "$out" "$rc"
env="$T/w-good/harness.env"
verdict "jobs_mint: exactly two H_ lines in the env file, one of each name" 0 "two" "$([ "$(grep -c '^H_' "$env")" = 2 ] && [ "$(grep -c '^H_JOBS_JWT_SECRET=' "$env")" = 1 ] && [ "$(grep -c '^H_JOBS_M1_KEY_SECRET=' "$env")" = 1 ] && echo two || echo "$(grep -c '^H_' "$env") lines")" 0
verdict "jobs_mint: both are 64 hexadecimal characters and differ" 0 "hex64-different" "$(a="$(sed -n 's/^H_JOBS_JWT_SECRET=//p' "$env")"; b="$(sed -n 's/^H_JOBS_M1_KEY_SECRET=//p' "$env")"; printf '%s\n%s\n' "$a" "$b" | grep -qE '^[0-9a-f]{64}$' && [ "$(printf '%s\n%s\n' "$a" "$b" | grep -cE '^[0-9a-f]{64}$')" = 2 ] && [ "$a" != "$b" ] && echo hex64-different || echo WRONG)" 0
verdict "jobs_mint: both were masked BEFORE they were written to the env file" 0 "2 before, 0 after" "$(printf '%s before, %s after' "$(grep -c BEFORE_WRITE "$T/w-good/mask.log")" "$(grep -c AFTER_WRITE "$T/w-good/mask.log" || true)")" 0
sa="$(sed -n 's/^H_JOBS_JWT_SECRET=//p' "$env")"; sb="$(sed -n 's/^H_JOBS_M1_KEY_SECRET=//p' "$env")"
case "$out" in *"$sa"*|*"$sb"*) leaked=yes;; *) leaked=no;; esac
verdict "jobs_mint: the output never shows a secret" 0 "no" "$leaked" 0
out="$(mint_run empty)"; rc=$?
verdict "jobs_mint: a tool that printed nothing is refused" 1 "could not mint the jobs scope's two secrets" "$out" "$rc"
out="$(mint_run short)"; rc=$?
verdict "jobs_mint: secrets of the wrong length are refused" 1 "openssl rand printed 4 and 4 characters, want 64 and 64" "$out" "$rc"
out="$(mint_run same)"; rc=$?
verdict "jobs_mint: one value twice is refused (two credentials must differ)" 1 "the JWT secret and the key-derivation secret must be different credentials" "$out" "$rc"
out="$(mint_run noenv)"; rc=$?
verdict "jobs_mint: no env file" 1 "the env file is missing" "$out" "$rc"
out="$(mint_run mode644)"; rc=$?
verdict "jobs_mint: an env file that is mode 644 (where modes are kept)" 1 "is mode 644, expected 600" "$out" "$rc"
out="$(mint_run mode600)"; rc=$?
verdict "jobs_mint: the same host with a mode-600 env file is green (the control of the case above)" 0 "masked, none printed" "$out" "$rc"

# ── jobs_get and jobs_fact ──
line='{"fact":"state","job":"j-1","status":"running","module_ref":"","results":12,"results_module":7,"finished":0,"replayed":false}'
getf() { ( set -u; WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; jobs_get "$line" "$1" ); }
verdict "jobs_get: a string member" 0 "running" "$(getf status)" 0
verdict "jobs_get: a number is read whole, and 'results' is not 'results_module'" 0 "12" "$(getf results)" 0 "7"
verdict "jobs_get: the member whose name is the longer one" 0 "7" "$(getf results_module)" 0 "12"
verdict "jobs_get: an empty string member reads as empty" 0 "[]" "$(printf '[%s]' "$(getf module_ref)")" 0
verdict "jobs_get: a boolean" 0 "false" "$(getf replayed)" 0
verdict "jobs_get: a member that is not there reads as empty" 0 "[]" "$(printf '[%s]' "$(getf nope)")" 0
fact_of() { ( set -u; WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; JOBS_OUT="$(printf '%s\n%s\n' '{"fact":"created","job":"a"}' "$line")"; jobs_fact "$1" ); }
verdict "jobs_fact: the first line of the name" 0 '{"fact":"state","job":"j-1"' "$(fact_of state)" 0
verdict "jobs_fact: a name that is not there is empty" 0 "[]" "$(printf '[%s]' "$(fact_of verify)")" 0

# ── jobs_expect_oks ──
oks() { ( . "$HERE/stage.sh" >/dev/null 2>&1; printf '%s\n' "${JOBS_VERIFY_OKS[@]}" ); }
nok="$(oks | wc -l | tr -d ' ')"
[ "$nok" -ge 15 ] || { echo "selftest: RED — stage.sh lists only $nok invariants: the brief names more than fifteen"; exit 1; }
good="$(oks | sed 's/^/OK /')"
expect_run() { # <the program's output> -> the verdict of jobs_expect_oks
  ( set -u; W="$T/eo"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"; JOBS_OUT="$1"; jobs_expect_oks "label" && echo "ALL-FOUND $(wc -l < "$W/jobs-ok.log" | tr -d ' ') kept" ) 2>&1
}
out="$(expect_run "$good")"; rc=$?
verdict "jobs_expect_oks: every OK line, once" 0 "ALL-FOUND $nok kept" "$out" "$rc"
for name in $(oks); do
  out="$(expect_run "$(printf '%s\n' "$good" | grep -vx "OK $name")")"; rc=$?
  verdict "jobs_expect_oks: 'OK $name' is missing" 1 "printed 'OK $name' 0 time(s)" "$out" "$rc"
done
out="$(expect_run "$(printf '%s\nOK %s\n' "$good" "$(oks | head -1)")")"; rc=$?
verdict "jobs_expect_oks: a line printed twice" 1 "printed 'OK $(oks | head -1)' 2 time(s)" "$out" "$rc"
out="$(expect_run "$(printf '%s\nOK a_check_nobody_listed\n' "$good")")"; rc=$?
verdict "jobs_expect_oks: a line the stage does not know" 1 "printed $((nok+1)) OK line(s), want exactly $nok" "$out" "$rc"
out="$(expect_run "")"; rc=$?
verdict "jobs_expect_oks: a program that printed nothing" 1 "the check did not run" "$out" "$rc"
out="$(expect_run "$(printf '%s\n' "$good" | sed 's/^OK /ok /')")"; rc=$?
verdict "jobs_expect_oks: a lower-case 'ok' is not an OK line" 1 "the check did not run" "$out" "$rc"

# ── jobs_expect_fail (the program's own control) ──
cat > "$T/scn-env.sh" <<'EOF'
DC() {   # the scenario program, as a stand-in: SC_RC and SC_ERR say what it did
  case "$1" in
    run) [ -z "${SC_OUT:-}" ] || printf '%s\n' "$SC_OUT"; [ -z "${SC_ERR:-}" ] || printf '%s\n' "$SC_ERR" >&2; return "${SC_RC:-0}";;
  esac
  return 0
}
EOF
efail() { ( set -u; W="$T/ef"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"; . "$T/scn-env.sh"; JOBS_QUIET=1; jobs_expect_fail "control" "FAIL job_succeeded" verify --job x ) 2>&1; }
out="$(SC_RC=1 SC_ERR='FAIL job_succeeded: the job is previewed, want succeeded' efail)"; rc=$?
verdict "jobs_expect_fail: the program fails for the reason named" 0 "control holds: control -> exit 1, 'FAIL job_succeeded'" "$out" "$rc"
out="$(SC_RC=0 efail)"; rc=$?
verdict "jobs_expect_fail: a program that exits 0 where it must fail is red" 1 "exited 0 where it must fail" "$out" "$rc"
out="$(SC_RC=1 SC_ERR='the environment variable X is not set' efail)"; rc=$?
verdict "jobs_expect_fail: a program that fails for ANOTHER reason is red" 1 "failed (exit 1) but not for the reason named" "$out" "$rc"
out="$(SC_RC=2 SC_ERR='usage: jobscenario' efail)"; rc=$?
verdict "jobs_expect_fail: a usage failure is not the named reason" 1 "not for the reason named" "$out" "$rc"

# ── jobs_prove_killed ──
cat > "$T/kill-env.sh" <<'EOF'
DC() {
  case "$1" in
    ps)   [ "${SC_NOCID:-0}" = 1 ] || echo cid-jobs-a; return 0;;
    logs) printf '%s\n' "${SC_LOG-jobs-a  | faultpoint: \"submit.after_module\" hit 1: killing the process}"; return 0;;
  esac
  return 0
}
docker() { echo "${SC_STATE:-exited 137 false}"; }
EOF
killrun() { ( set -u; W="$T/kr"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"; . "$T/kill-env.sh"; jobs_prove_killed jobs-a submit.after_module 1 ) 2>&1; }
out="$(killrun)"; rc=$?
verdict "jobs_prove_killed: SIGKILL (137), no OOM kill, the armed build's line" 0 "killed by the fault point: jobs-a is 'exited 137 false'" "$out" "$rc"
out="$(SC_STATE='exited 1 false' killrun)"; rc=$?
verdict "jobs_prove_killed: another exit status is not a kill" 1 "is 'exited 1 false' (status, exit code, OOMKilled), want 'exited 137 false'" "$out" "$rc"
out="$(SC_STATE='exited 137 true' killrun)"; rc=$?
verdict "jobs_prove_killed: the OOM killer is not the fault point" 1 "the kernel's OOM killer did it" "$out" "$rc"
out="$(SC_STATE='running 0 false' killrun)"; rc=$?
verdict "jobs_prove_killed: a container that is still running" 1 "want 'exited 137 false'" "$out" "$rc"
out="$(SC_LOG='jobs-a  | a perfectly ordinary line' killrun)"; rc=$?
verdict "jobs_prove_killed: no log line of the armed build" 1 "has no 'faultpoint: \"submit.after_module\" hit 1: killing the process' line" "$out" "$rc"
out="$(SC_LOG='jobs-a  | faultpoint: "confirm.before_commit" hit 1: killing the process' killrun)"; rc=$?
verdict "jobs_prove_killed: the line of ANOTHER point" 1 "has no 'faultpoint: \"submit.after_module\" hit 1" "$out" "$rc"
out="$(SC_LOG='jobs-a  | faultpoint: "submit.after_module" hit 2: killing the process' killrun)"; rc=$?
verdict "jobs_prove_killed: the right point at another occurrence" 1 "hit 1: killing the process' line" "$out" "$rc"
out="$(SC_LOG="$(printf 'faultpoint: "submit.after_module" hit 1: killing the process\nfaultpoint: "submit.after_module" hit 1: killing the process')" killrun)"; rc=$?
verdict "jobs_prove_killed: a process that killed itself twice" 1 "killed itself 2 times, want exactly once" "$out" "$rc"
out="$(SC_NOCID=1 killrun)"; rc=$?
verdict "jobs_prove_killed: no container at all" 1 "has no container" "$out" "$rc"

# ── jobs_summary ──
sumrun() { # <verified> <number of OK lines kept, as <label> OK <name> lines of one label per run>
  ( set -u; W="$T/su"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    : > "$W/jobs-ok.log"; local r name
    for r in $(seq 1 "$2"); do for name in "${JOBS_VERIFY_OKS[@]}"; do echo "run$r OK $name" >> "$W/jobs-ok.log"; done; done
    JOBS_VERIFIED="$1"; jobs_summary ) 2>&1
}
out="$(sumrun 9 9)"; rc=$?
verdict "jobs_summary: nine verify runs of every invariant" 0 "$((9 * nok)) OK lines from 9 verify runs" "$out" "$rc"
out="$(sumrun 8 8)"; rc=$?
verdict "jobs_summary: eight runs where the stages make nine" 1 "the stages ran 8 verify run(s), want 9" "$out" "$rc"
out="$(sumrun 9 8)"; rc=$?
verdict "jobs_summary: nine runs counted, eight runs' lines kept" 1 "$((8 * nok)) OK line(s) were printed by the 9 verify runs, want $((9 * nok))" "$out" "$rc"
out="$(sumrun 9 0)"; rc=$?
verdict "jobs_summary: nothing was kept" 1 "the scenario program verified nothing" "$out" "$rc"
out="$(sumrun 0 0)"; rc=$?
verdict "jobs_summary: no run at all" 1 "the stages ran 0 verify run(s)" "$out" "$rc"

# ── jobs_secret_leak ──
rnd() { local n="$1" i=0 s=""; while [ "$i" -lt "$n" ]; do s="$s$(printf '%02x' $((RANDOM % 256)))"; i=$((i+1)); done; printf '%s\n' "$s"; }
b64u() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '+/' '-_'; }
FAKE_JWT="$(b64u '{"alg":"HS256","typ":"JWT"}').$(b64u '{"sub":"stand-in","role":"authenticated"}').$(b64u 'a-signature-of-some-length')"   # built here, never stored: a token-shaped string must not sit in a committed file
SA="$(rnd 32)"; SB="$(rnd 32)"
leak_run() { # <jobs-a log> <jobs-b log> <m1j log> <jobsdns log> <the scenario program's own log> [empty-secrets]
  rm -rf "$T/lk"; mkdir -p "$T/lk"; printf '%s' "$1" > "$T/lk/jobs-a.txt"; printf '%s' "$2" > "$T/lk/jobs-b.txt"; printf '%s' "$3" > "$T/lk/m1j.txt"; printf '%s' "$4" > "$T/lk/jobsdns.txt"; printf '%s\n' "$5" > "$T/lk/jobs-run.log"
  ( set -u; WORK="$T/lk"; export WORK; . "$T/env.sh"; . "$HERE/stage.sh"
    DC() { if [ "$1" = logs ]; then cat "$T/lk/${@: -1}.txt"; else return 1; fi; }
    if [ -n "${6:-}" ]; then JOBS_JWT=""; JOBS_M1K=""; else JOBS_JWT="$SA"; JOBS_M1K="$SB"; fi
    jobs_secret_leak ) 2>&1
}
out="$(leak_run 'engine started' 'standby' 'drainer starting' 'dns up' 'verify ok')"; rc=$?
verdict "jobs_secret_leak: clean logs" 0 "appears in the logs of jobs-a, jobs-b, m1j and jobsdns" "$out" "$rc"
out="$(leak_run "config $SA loaded" 'standby' 'drainer' 'dns' 'fine')"; rc=$?
verdict "jobs_secret_leak: the JWT secret in an instance's log is red, names the log, shows nothing" 1 "appears in leak-jobs-a.log (the log is not shown" "$out" "$rc" "$SA"
out="$(leak_run 'engine' 'standby' "derived from $SB" 'dns' 'fine')"; rc=$?
verdict "jobs_secret_leak: the key-derivation secret in module 1's log is red" 1 "appears in leak-m1j.log (the log is not shown" "$out" "$rc" "$SB"
out="$(leak_run 'engine' 'standby' 'drainer' 'dns' "Authorization: Bearer $FAKE_JWT")"; rc=$?
verdict "jobs_secret_leak: a JWT-shaped string in the scenario program's own log is red" 1 "something shaped like a JWT" "$out" "$rc" "$FAKE_JWT"
verdict "jobs_secret_leak: ... naming the log it was in" 1 "appears in jobs-run.log" "$out" "$rc"
out="$(leak_run 'a header eyJhbGci on its own, and a.b.c, and eyJ.x.y' 'standby' 'drainer' 'dns' 'fine')"; rc=$?
verdict "jobs_secret_leak: strings that only start like a token are not a token" 0 "appears in the logs of jobs-a" "$out" "$rc"
out="$(leak_run '' 'standby' 'drainer' 'dns' 'fine')"; rc=$?
verdict "jobs_secret_leak: an empty log is a scan of nothing" 1 "the log of jobs-a, which the leak check must read, is empty" "$out" "$rc"
out="$(leak_run 'engine' 'standby' 'drainer' 'dns' 'fine' empty)"; rc=$?
verdict "jobs_secret_leak: empty secrets would match every line: refused" 1 "the secrets of this run are empty" "$out" "$rc"

# ── jobs_not_checked ──
out="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; jobs_not_checked )"; rc=$?
verdict "jobs_not_checked: says it is module 1 only" 0 "module 1 ONLY" "$out" "$rc"
verdict "jobs_not_checked: names the kills it does not make" 0 "a kill at any other instant" "$out" "$rc"
verdict "jobs_not_checked: names the verdicts the fixture DNS cannot give" 0 "a valid, a catch-all, a greylisted" "$out" "$rc"
verdict "jobs_not_checked: ends with one grep-able line that starts with NOT CHECKED:" 0 "line-ok" "$(printf '%s\n' "$out" | grep -q '^NOT CHECKED: modules 2 and 3' && echo line-ok || echo LINE-MISSING)" 0

# ── the wiring of the scope run: its text is read (it needs Docker to run) ──
wire="$( WORK="$T"; . "$T/env.sh"; . "$HERE/stage.sh"; declare -f jobs_run jobs_scope_run jobs_j2 jobs_kill_point )"
verdict "jobs_run: runs the infrastructure, the four stages, the summary, the leak scan and the exposure check, in that order" 0 "line-ok" "$(printf '%s\n' "$wire" | grep -E '^    (jobs_infra|jobs_j1|jobs_j2|jobs_j3|jobs_j4|jobs_summary|jobs_secret_leak|jobs_exposure);?$' | tr -d ' ;' | tr '\n' ' ' | grep -qx 'jobs_infra jobs_j1 jobs_j2 jobs_j3 jobs_j4 jobs_summary jobs_secret_leak jobs_exposure ' && echo line-ok || echo WRONG-ORDER)" 0
verdict "jobs_scope_run: calls jobs_run and ends the run" 0 "exit 0" "$wire" 0
verdict "jobs_kill_point: the kill is proven, the mid-state is asserted and the confirm is replayed" 0 "jobs_prove_killed jobs-a" "$wire" 0
verdict "jobs_kill_point: ... the state of the database is asserted at the kill" 0 'jobs_scn state --job "$JOB" --expect "$mid"' "$wire" 0
verdict "jobs_kill_point: ... the confirm is sent again" 0 "confirm-retry" "$wire" 0
verdict "jobs_kill_point: ... and the job is verified at the end" 0 'jobs_verify "J2 $point"' "$wire" 0

# ── consistency of the files of the scope ──
GOV="$HERE/../jobscenario/verify.go"; COMPOSE="$HARNESS/docker-compose.jobs.yml"; RUNSH="$HARNESS/run.sh"; REG="$ROOT/internal/faultpoint/names.txt"
goloc="$(sed -n '/^var verifyOKs = \[\]string{/,/^}/p' "$GOV" | sed -n 's/^[[:space:]]*"\([a-z0-9_]*\)",$/\1/p')"
verdict "consistency: the stage's invariants are the scenario program's, in the same order (a name added to one and not the other is red)" 0 "same" "$([ -n "$goloc" ] && [ "$goloc" = "$(oks)" ] && echo same || printf 'go: %s\nstage: %s' "$(printf '%s' "$goloc" | tr '\n' ' ')" "$(oks | tr '\n' ' ')")" 0
bad_names=""
for name in $goloc; do [ "$(grep -c "v.check(\"$name\"" "$GOV")" = 1 ] || bad_names="$bad_names $name"; done
verdict "consistency: every invariant is checked, exactly once, by a v.check call of verify.go" 0 "once-each" "$([ -z "$bad_names" ] && echo once-each || echo "not exactly once:$bad_names")" 0
verdict "consistency: no invariant name appears twice in the stage's list" 0 "unique" "$([ -z "$(oks | sort | uniq -d)" ] && echo unique || echo duplicated)" 0
points="$( . "$HERE/stage.sh" >/dev/null 2>&1; printf '%s\n' "${JOBS_POINTS[@]}" | sort )"
registry="$(sed -n 's/^\([a-z0-9_]*\.[a-z0-9_]*\)[[:space:]]*#.*/\1/p' "$REG" | sort)"
verdict "consistency: the kill points are exactly the names of internal/faultpoint/names.txt (a point added to the registry and not killed here is red)" 0 "same" "$([ -n "$registry" ] && [ "$points" = "$registry" ] && echo same || printf 'stage: %s\nregistry: %s' "$(printf '%s' "$points" | tr '\n' ' ')" "$(printf '%s' "$registry" | tr '\n' ' ')")" 0
verdict "consistency: every point has a kill stage (jobs_j2 names each, with the state it must leave)" 0 "all-killed" "$(miss=""; for p in $points; do grep -q "^  jobs_kill_point $p " "$HERE/stage.sh" || miss="$miss $p"; done; [ -z "$miss" ] && echo all-killed || echo "no kill stage for:$miss")" 0
verdict "consistency: the compose file publishes no port" 0 "none" "$(grep -qE '^[[:space:]]*ports:' "$COMPOSE" && echo "a ports: key" || echo none)" 0
verdict "consistency: the compose file names jobs-a, jobs-b, m1j, jobsdns and jobscenario, each in the profile jobs" 0 "all-profiled" "$(c=0; for s in jobs-a jobs-b m1j jobsdns jobscenario; do awk -v s="  $s:" '$0==s {p=1; next} p && /^  [a-z0-9]/ {p=0} p && /profiles: \[jobs\]/ {f=1} END {exit f?0:1}' "$COMPOSE" && c=$((c+1)); done; [ "$c" = 5 ] && echo all-profiled || echo "$c of 5")" 0
verdict "consistency: the two instances arm the fault point from their own variable and nothing else sets it" 0 "armed" "$(grep -c 'PLATFORM_FAULTPOINT: ${H_JOBS_A_FAULT:-}' "$COMPOSE" | grep -qx 1 && grep -c 'PLATFORM_FAULTPOINT: ${H_JOBS_B_FAULT:-}' "$COMPOSE" | grep -qx 1 && [ "$(grep -c 'PLATFORM_FAULTPOINT' "$COMPOSE" | tr -d ' ')" = 2 ] && echo armed || echo WRONG)" 0
verdict "consistency: the instances are built with the fault-point tag (the one build that has one)" 0 "tagged" "$([ "$(grep -c 'args: { GO_TAGS: faultpoints }' "$COMPOSE")" = 2 ] && echo tagged || echo WRONG)" 0
verdict "consistency: the engine is ON in both instances and OFF in the legacy ones (docker-compose.yml)" 0 "engine-split" "$([ "$(grep -c 'PLATFORM_JOB_ENGINE_ENABLED: "true"' "$COMPOSE")" = 2 ] && [ "$(grep -c 'PLATFORM_JOB_ENGINE_ENABLED: "false"' "$HARNESS/docker-compose.yml")" = 2 ] && echo engine-split || echo WRONG)" 0
verdict "consistency: the staleness limit of module 1 is at its minimum (one minute) and the tick and settle interval are short, in both instances" 0 "dials" "$([ "$(grep -c 'PLATFORM_JOB_STALE_AFTER_MIN_M1: "1"' "$COMPOSE")" = 2 ] && [ "$(grep -c 'PLATFORM_JOB_TICK_MS: "200"' "$COMPOSE")" = 2 ] && [ "$(grep -c 'PLATFORM_JOB_SETTLE_INTERVAL_SEC: "1"' "$COMPOSE")" = 2 ] && echo dials || echo WRONG)" 0
verdict "consistency: the instances are named jobs-a and jobs-b (the application_name of the leader's session)" 0 "named" "$(grep -q 'PLATFORM_INSTANCE_ID: jobs-a' "$COMPOSE" && grep -q 'PLATFORM_INSTANCE_ID: jobs-b' "$COMPOSE" && echo named || echo WRONG)" 0
verdict "consistency: a killed instance stays dead until the stage starts it (restart: \"no\")" 0 "no-restart" "$([ "$(grep -c 'restart: "no"' "$COMPOSE")" -ge 3 ] && echo no-restart || echo WRONG)" 0
verdict "consistency: the scope's image of the scenario program is pinned name:tag@sha256:<64 hex>" 0 "pin-ok" "$(grep -qE '^    image: alpine:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$' "$COMPOSE" && echo pin-ok || echo PIN-FAIL)" 0
missing=""
for v in $(grep -o '\${H_[A-Z0-9_]*' "$COMPOSE" | sed 's/^\${//' | sort -u); do
  case "$v" in H_JOBS_A_FAULT|H_JOBS_B_FAULT) continue;; esac       # set for one command by jobs_up, defaulted to empty everywhere else
  grep -q "^$v=" "$RUNSH" || grep -q "echo \"$v=" "$HERE/stage.sh" || missing="$missing $v"
done
verdict "consistency: every H_ variable the compose file needs is written by run.sh or by jobs_mint" 0 "written" "$([ -z "$missing" ] && echo written || echo "nobody writes:$missing")" 0
verdict "consistency: jobs_up sets exactly the two fault variables" 0 "set" "$(grep -q 'jobs-a) var=H_JOBS_A_FAULT' "$HERE/stage.sh" && grep -q 'jobs-b) var=H_JOBS_B_FAULT' "$HERE/stage.sh" && echo set || echo WRONG)" 0
missing=""; nvars=0
for v in $(grep -ohE '(env|envOr)\("JOBS_[A-Z0-9_]+"' "$HERE"/../jobscenario/*.go | sed 's/.*"\(JOBS_[A-Z0-9_]*\)"/\1/' | sort -u); do
  nvars=$((nvars+1)); grep -q "^      $v:" "$COMPOSE" || missing="$missing $v"
done
verdict "consistency: the scenario program's container is given every JOBS_ variable the program reads (and it reads at least five)" 0 "given" "$([ -z "$missing" ] && [ "$nvars" -ge 5 ] && echo given || echo "missing:$missing, the program reads $nvars")" 0
verdict "consistency: the secrets of the instances and of the scenario program are the same two variables" 0 "same-secrets" "$([ "$(grep -c '\${H_JOBS_JWT_SECRET:?}' "$COMPOSE")" = 3 ] && [ "$(grep -c '\${H_JOBS_M1_KEY_SECRET:?}' "$COMPOSE")" = 3 ] && echo same-secrets || echo WRONG)" 0
verdict "consistency: the scenario program names no fault-point variable (a Go file under tests/ that does must carry the armed build tag)" 0 "none" "$(grep -il 'platform_faultpoint' "$HERE"/../jobscenario/*.go | head -1 | grep -q . && echo "names it" || echo none)" 0
verdict "consistency: every Go file of the scenario program carries the build tag harness on its first line" 0 "tagged" "$(bad=""; for f in "$HERE"/../jobscenario/*.go; do [ "$(head -1 "$f")" = '//go:build harness' ] || bad="$bad $(basename "$f")"; done; [ -z "$bad" ] && echo tagged || echo "not tagged:$bad")" 0
verdict "consistency: the scenario program has no test file (it is not a test: it adds nothing to the floor of tests/floors-harness.txt)" 0 "none" "$(ls "$HERE"/../jobscenario/*_test.go >/dev/null 2>&1 && echo "a test file" || echo none)" 0
verdict "consistency: run.sh allows the scope jobs" 0 "scope-allowed-ok" "$(grep -q 'case "\$SCOPE" in full|m1node|m2worker|gotrue|jobs) ;;' "$RUNSH" && echo scope-allowed-ok || echo "SCOPE-NOT-ALLOWED-FAIL")" 0
verdict "consistency: run.sh sources the stage and ends the scope through jobs_scope_run" 0 "wired" "$(grep -q '^\. "\$HERE/jobs/stage.sh"' "$RUNSH" && grep -q '^\[ "\$SCOPE" = jobs \] && jobs_scope_run' "$RUNSH" && echo wired || echo missing)" 0
verdict "consistency: the scope builds only the four migration images, module 1, the fixture DNS and the two instances" 0 "scoped" "$(grep -q 'jobs)     BUILD_ONLY="migrate-platform migrate-m1 migrate-m2 migrate-m3 m1j jobsdns jobs-a jobs-b";;' "$RUNSH" && echo scoped || echo missing)" 0
unguarded="$(grep -nE '(^|[^_a-z])jobs_(mint|run|scope_run|not_checked)( |$)' "$RUNSH" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE '\[ "\$JOBS_ON" = 1 \] &&|\[ "\$SCOPE" = jobs \] &&' | head -5)"
verdict "consistency: every call of a jobs_ function in run.sh is under the JOBS_ON or the scope guard (the stage is dormant otherwise)" 0 "all-guarded-ok" "$([ -z "$unguarded" ] && echo all-guarded-ok || echo "UNGUARDED-FAIL: $unguarded")" 0
ncalls="$(grep -cE '\[ "\$JOBS_ON" = 1 \] && (\[ "\$SCOPE" = full \] && )?jobs_(mint|run|not_checked)' "$RUNSH")"
verdict "consistency: run.sh calls the three stage functions of the full run (mint, run, not_checked) under the JOBS_ON guard" 0 "three" "$([ "$ncalls" = 3 ] && echo three || echo "$ncalls")" 0
verdict "consistency: the docker compose function of run.sh merges the scope's compose file only through JOBS_DC" 0 "merged" "$(grep -q '\${JOBS_DC\[@\]+"\${JOBS_DC\[@\]}"}' "$RUNSH" && grep -q 'JOBS_DC=(-f "\$HERE/docker-compose.jobs.yml" --profile jobs)' "$RUNSH" && echo merged || echo missing)" 0

# DORMANCY, executed on the real text of run.sh: the guard block is cut out of it and evaluated for settings
block="$(sed -n '/^JOBS_ON=0; JOBS_DC=()$/,/^\[ "\$JOBS_ON" = 1 \] && JOBS_DC=/p' "$RUNSH")"
verdict "dormancy: the guard block of run.sh was found (its subject is not empty)" 0 "found" "$([ "$(printf '%s\n' "$block" | wc -l | tr -d ' ')" = 4 ] && echo found || echo "$(printf '%s\n' "$block" | wc -l) lines")" 0
jobs_state() { # scope full-includes
  ( set -u; HERE=/h; SCOPE="$1"; FULL_INCLUDES="$2"; eval "$block"; printf 'JOBS_ON=%s files=%s' "$JOBS_ON" "${#JOBS_DC[@]}" )
}
verdict "dormancy: the full run with FULL_INCLUDES as it stands in run.sh is exactly what it names" 0 "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1 | grep -qw jobs && echo 'JOBS_ON=1 files=4' || echo 'JOBS_ON=0 files=0')" "$(jobs_state full "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1)")" 0
verdict "dormancy: the full run with FULL_INCLUDES = m2worker m1node: no compose file, nothing on" 0 "JOBS_ON=0 files=0" "$(jobs_state full 'm2worker m1node')" 0
verdict "dormancy: the full run with FULL_INCLUDES empty" 0 "JOBS_ON=0 files=0" "$(jobs_state full '')" 0
verdict "dormancy: a name that merely contains jobs does not switch it on" 0 "JOBS_ON=0 files=0" "$(jobs_state full 'notjobs jobs2')" 0
verdict "dormancy: the scope gotrue (FULL_INCLUDES names jobs) does not switch it on" 0 "JOBS_ON=0 files=0" "$(jobs_state gotrue 'jobs')" 0
verdict "dormancy: the scope jobs switches it on: the compose file and the profile" 0 "JOBS_ON=1 files=4" "$(jobs_state jobs '')" 0
verdict "dormancy: the full run with FULL_INCLUDES naming jobs switches it on (the lead's one-word change)" 0 "JOBS_ON=1 files=4" "$(jobs_state full 'm2worker m1node jobs')" 0

# the workflows
verdict "workflows: the harness workflow offers the scope jobs" 0 "offered" "$(grep -qE '^          - jobs$' "$ROOT/.github/workflows/harness.yml" && echo offered || echo missing)" 0
verdict "workflows: the go workflow vets every program under tests/harness with the scoped tags" 0 "vetted" "$(grep -q 'go vet -tags "harness m1node m2worker gotrue" ./tests/harness/\.\.\.' "$ROOT/.github/workflows/go.yml" && echo vetted || echo missing)" 0
verdict "workflows: the go workflow builds the scenario program" 0 "built" "$(grep -q 'go build -tags harness -o /dev/null ./tests/harness/jobscenario' "$ROOT/.github/workflows/go.yml" && echo built || echo missing)" 0
verdict "workflows: the gates workflow runs this selftest" 0 "wired" "$(grep -q 'tests/harness/jobs/stage-selftest.sh' "$ROOT/.github/workflows/gates.yml" && echo wired || echo missing)" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; bad=$((bad+1)); fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
