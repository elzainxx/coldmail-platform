#!/usr/bin/env bash
# send-stage-selftest.sh — proves the checks of tests/harness/jobs/send-stage.sh (the senders' crash matrix and outages, scope sendjobs) go RED, for the reason they name, and stay green on a good input; and
# that the files of the scope agree with each other (the kill points and the registry of internal/faultpoint, the invariants of the stage and of the scenario program, the overlay docker-compose.sendjobs.yml,
# the environment the scenario program reads, the wiring in run.sh, the refactored stage 5b and the workflow). No Docker, no Go build: the functions are sourced and run against stand-ins. It is run by
# tests/harness/jobs/stage-selftest.sh (which the gates workflow runs), and can be run alone. What it covers:
#   sj_expect_oks     green when every OK line of SJ_VERIFY_OKS is printed once; red, naming the line, when ANY ONE is missing (one case per name), printed twice or joined by a line the stage does not know; red on no output
#   sj_summary        green for one and for two senders with every verify run, every kill and every outage step; red when a verify run is missing, when the OK lines are not runs x invariants, when ANY ONE kill of
#                     either sender is missing (one case per point) or kept twice, when ANY ONE outage step is missing (one case per name), and on a line the stage does not know
#   sj_mid            the mid-state of each point names what that point must leave (a kill that landed at the end is red), module 3 holds no reserve, module 2 one; an unknown point is refused
#   sj_kill_point     run against stand-ins for every point: the instance armed with the point (settle.after_commit at its count), the confirm that dies inside the request, the user's pause and cancel of the RUNNING
#                     job, the kill proven, the mid-state asserted (with the module's report for pause and cancel), the confirm replayed (the first confirm only for confirm.before_commit), the module_paused note
#                     awaited and the resume, the verify of the path; red when the state does not hold, when the retried confirm is not what the point says
#   sj_secret_leak    green on clean logs; red, naming the log and never printing it, for a credential in an instance's log (now or kept before a re-create), a module's log, the scenario program's own output, and for
#                     anything shaped like a JWT; red on an empty instance log and on too few credentials
#   consistency       the stage's invariants are the scenario program's (sendVerifyOKs), in order, each checked once in send_verify.go; the kill points with the module 1 scope's are the registry; every point has a
#                     mid-state; the outage steps are the calls of sj_outage in order; the overlay publishes no port, builds nothing, gives both instances the same senders' lines and the scenario program every
#                     SENDJOBS_ variable the program reads, and needs only H_ variables somebody writes; run.sh allows the three scopes without touching the line the other selftests read, sources the stage,
#                     builds only what each scope needs, calls the stage only under its guards, merges the overlay only through SJ_DC; the stage is DORMANT in `full` unless FULL_INCLUDES names sendjobs AND
#                     jobs (the guard block is evaluated); stage 5b is ONE function the full run and the scope call, its probes made through m3w_probe (the platform service unless the scope says jobs-a); the
#                     harness workflow offers the three scopes; every command the stage gives the scenario program is a command of it
# NOT covered here (they need a Linux host with Docker): the real docker compose, wget, psql, the modules, their workers, the sink, the platform image and the kills themselves; sj_outage and sj_infra run only there.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; HARNESS="$(cd "$HERE/.." && pwd)"; ROOT="$(cd "$HARNESS/../.." && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=80; cases=0; ok=0; bad=0
verdict() { # label want-rc want-substring out rc [forbidden-substring]
  local label="$1" want="$2" sub="$3" out="$4" rc="$5" forbid="${6:-}" n f=0
  cases=$((cases+1))
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ -n "$forbid" ] && printf '%s\n' "$out" | grep -q -F -- "$forbid"; then f=1; fi
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ] && [ "$f" -eq 0 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), substring '$sub' found $n times${forbid:+, forbidden text present: $f}"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}

cat > "$T/env.sh" <<'EOF'
die() { echo "DIE: $*"; exit 1; }
banner() { echo "BANNER [$1] $2"; }
mask() { :; }
DC() { echo "DC called: $*" >&2; return 1; }
EOF
src() { . "$T/env.sh"; . "$HERE/stage.sh"; . "$HERE/send-stage.sh"; }   # the order run.sh sources them in

# ── sj_expect_oks ──
oks() { ( . "$HERE/send-stage.sh" >/dev/null 2>&1; printf '%s\n' "${SJ_VERIFY_OKS[@]}" ); }
nok="$(oks | wc -l | tr -d ' ')"
[ "$nok" -ge 18 ] || { echo "selftest: RED — send-stage.sh lists only $nok invariants: a send job's verify states at least eighteen"; exit 1; }
good="$(oks | sed 's/^/OK /')"
expect_run() { ( set -u; W="$T/eo"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; src; JOBS_OUT="$1"; sj_expect_oks "label" && echo "ALL-FOUND $(wc -l < "$W/sj-ok.log" | tr -d ' ') kept" ) 2>&1; }
out="$(expect_run "$good")"; rc=$?
verdict "sj_expect_oks: every OK line, once" 0 "ALL-FOUND $nok kept" "$out" "$rc"
for name in $(oks); do
  out="$(expect_run "$(printf '%s\n' "$good" | grep -vx "OK $name")")"; rc=$?
  verdict "sj_expect_oks: 'OK $name' is missing" 1 "printed 'OK $name' 0 time(s)" "$out" "$rc"
done
out="$(expect_run "$(printf '%s\nOK %s\n' "$good" "$(oks | tail -1)")")"; rc=$?
verdict "sj_expect_oks: a line printed twice" 1 "printed 'OK $(oks | tail -1)' 2 time(s)" "$out" "$rc"
out="$(expect_run "$(printf '%s\nOK a_check_nobody_listed\n' "$good")")"; rc=$?
verdict "sj_expect_oks: a line the stage does not know" 1 "printed $((nok+1)) OK line(s), want exactly $nok" "$out" "$rc"
out="$(expect_run "")"; rc=$?
verdict "sj_expect_oks: a program that printed nothing" 1 "the check did not run" "$out" "$rc"
out="$(expect_run "$(printf '%s\n' "$good" | sed 's/^OK /OK: /')")"; rc=$?
verdict "sj_expect_oks: 'OK:' is not an OK line" 1 "the check did not run" "$out" "$rc"

# ── sj_summary ──
pts() { ( . "$HERE/send-stage.sh" >/dev/null 2>&1; printf '%s\n' "${SJ_POINTS[@]}" ); }
d6s() { ( . "$HERE/send-stage.sh" >/dev/null 2>&1; printf '%s\n' "${SJ_D6_OKS[@]}" ); }
runs_per="$( . "$HERE/send-stage.sh" >/dev/null 2>&1; echo "$SJ_RUNS_PER_MODULE" )"
[ "$runs_per" = $(( 2 + $(pts | wc -l) )) ] || { echo "selftest: RED — SJ_RUNS_PER_MODULE is $runs_per, the stage makes the control, one run per kill point and the outage ($(( 2 + $(pts | wc -l) )))"; exit 1; }
sumrun() { # <mods> <verified> <runs of OK lines kept> [kill mode: all | none | missing:<m>:<point> | twice:<m>:<point> | extra] [d6 mode: all | missing:<m>:<name> | extra]
  ( set -u; W="$T/su"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; src
    SJ_MODS="$1"; : > "$W/sj-ok.log"; : > "$W/sj-kill.log"; : > "$W/sj-d6.log"; local r name m p km="${4:-all}" dm="${5:-all}"
    for r in $(seq 1 "$3"); do for name in "${SJ_VERIFY_OKS[@]}"; do echo "run$r OK $name" >> "$W/sj-ok.log"; done; done
    for m in $SJ_MODS; do
      for p in "${SJ_POINTS[@]}"; do
        [ "$km" = none ] && continue
        [ "$km" = "missing:$m:$p" ] && continue
        echo "SJ KILL $m $p" >> "$W/sj-kill.log"
        [ "$km" = "twice:$m:$p" ] && echo "SJ KILL $m $p" >> "$W/sj-kill.log"
      done
      for name in "${SJ_D6_OKS[@]}"; do [ "$dm" = "missing:$m:$name" ] || echo "SJ D6 OK $m $name" >> "$W/sj-d6.log"; done
    done
    [ "$km" != extra ] || echo "SJ KILL m2 a.point_nobody_listed" >> "$W/sj-kill.log"
    [ "$dm" != extra ] || echo "SJ D6 OK m2 a_step_nobody_listed" >> "$W/sj-d6.log"
    SJ_VERIFIED="$2"; sj_summary ) 2>&1
}
out="$(sumrun "m2 m3" $((2 * runs_per)) $((2 * runs_per)))"; rc=$?
verdict "sj_summary: two senders, every run, kill and step" 0 "$((2 * runs_per * nok)) OK lines from $((2 * runs_per)) verify runs" "$out" "$rc"
out="$(sumrun "m3" "$runs_per" "$runs_per")"; rc=$?
verdict "sj_summary: one sender (sendjobs-m3)" 0 "kills: $(pts | wc -l | tr -d ' ') (1 sender(s) x" "$out" "$rc"
out="$(sumrun "m2 m3" $((2 * runs_per - 1)) $((2 * runs_per - 1)))"; rc=$?
verdict "sj_summary: a verify run missing" 1 "the stages ran $((2 * runs_per - 1)) verify run(s), want $((2 * runs_per))" "$out" "$rc"
out="$(sumrun "m2 m3" $((2 * runs_per)) $((2 * runs_per - 1)))"; rc=$?
verdict "sj_summary: the runs counted, fewer runs' lines kept" 1 "OK line(s) were printed by the $((2 * runs_per)) verify runs" "$out" "$rc"
out="$(sumrun "" 0 0)"; rc=$?
verdict "sj_summary: no sender at all" 1 "the scope names no sender" "$out" "$rc"
out="$(sumrun "m2" "$runs_per" "$runs_per" none)"; rc=$?
verdict "sj_summary: no kill kept" 1 "no kill was kept" "$out" "$rc"
for p in $(pts); do
  out="$(sumrun "m2 m3" $((2 * runs_per)) $((2 * runs_per)) "missing:m3:$p")"; rc=$?
  verdict "sj_summary: the kill at $p of module 3 is missing" 1 "the kill at $p of module 3 is kept 0 time(s)" "$out" "$rc"
done
out="$(sumrun "m2 m3" $((2 * runs_per)) $((2 * runs_per)) "twice:m2:$(pts | head -1)")"; rc=$?
verdict "sj_summary: a kill kept twice" 1 "is kept 2 time(s)" "$out" "$rc"
out="$(sumrun "m2" "$runs_per" "$runs_per" extra)"; rc=$?
verdict "sj_summary: a kill the stage does not know" 1 "a kill this stage does not know" "$out" "$rc"
for name in $(d6s); do
  out="$(sumrun "m2 m3" $((2 * runs_per)) $((2 * runs_per)) all "missing:m2:$name")"; rc=$?
  verdict "sj_summary: the outage step $name of module 2 is missing" 1 "printed 'SJ D6 OK m2 $name' 0 time(s)" "$out" "$rc"
done
out="$(sumrun "m2" "$runs_per" "$runs_per" all extra)"; rc=$?
verdict "sj_summary: an outage step the stage does not know" 1 "a step this stage does not know" "$out" "$rc"

# ── sj_mid ──
mid() { ( set -u; WORK="$T"; src; sj_mid "$1" "$2" ) 2>&1; }
verdict "sj_mid: confirm.before_commit leaves nothing confirmed, held, sent or at the module" 0 "status=previewed,confirmed=0,confirm_key_set=0,reserves=0" "$(mid m2 confirm.before_commit)" 0
verdict "sj_mid: ... and nothing at the sink" 0 "module_jobs=0,sink_msgs=0" "$(mid m2 confirm.before_commit)" 0
verdict "sj_mid: submit.before_module: the rows are ensured, the module holds none, nothing sent" 0 "items_ensured=1,job_items=@pending,module_jobs=0,results_module=0,sink_msgs=0" "$(mid m3 submit.before_module)" 0
verdict "sj_mid: submit.after_module: the module holds rows, the platform stamped none and did not start" 0 "status=confirmed,started=0,stamped=0,events_running=0,items_ensured=1,job_items=@pending,module_jobs>0" "$(mid m2 submit.after_module)" 0
verdict "sj_mid: settle.after_commit: SOME results and not all" 0 "results_module>0,results_module<@pending" "$(mid m2 settle.after_commit)" 0
verdict "sj_mid: release.mid_tx: completing, every result, nothing released" 0 "status=completing,started=1,finished=0,events_running=1,events_completing=1,results=@rows,results_module=@pending,module_terminal=@pending,releases=0,reserve_released=0" "$(mid m2 release.mid_tx)" 0
verdict "sj_mid: pause.after_module: paused, the module holds it by its report, no module_paused note yet" 0 "status=paused,events_paused=1,events_module_paused=0,module_report_paused=1" "$(mid m3 pause.after_module)" 0
verdict "sj_mid: cancel.after_module: cancelling, the module's items cancelled, nothing recorded or completed" 0 "status=cancelling,cancel_requested=1,events_cancelling=1,events_completing=0,results_module_cancelled=0,module_cancelled>0,module_report_cancelled>0" "$(mid m2 cancel.after_module)" 0
verdict "sj_mid: module 2 holds one reserve" 0 "reserves=1" "$(mid m2 submit.before_module)" 0
verdict "sj_mid: module 3 holds none (it is not metered)" 0 "reserves=0" "$(mid m3 submit.before_module)" 0 "reserves=1"
out="$(mid m2 start.before_running)"; rc=$?
verdict "sj_mid: a point that is not the senders' is refused" 1 "start.before_running is not a point of the senders' matrix" "$out" "$rc"
for p in $(pts); do
  verdict "sj_mid: $p has a mid-state of at least four conditions" 0 "four" "$([ "$(mid m2 "$p" | tr ',' '\n' | grep -c .)" -ge 4 ] && echo four || mid m2 "$p")" 0
done

# ── sj_kill_point, against stand-ins ──
cat > "$T/kp-env.sh" <<'EOF'
JOBS_API_A="http://jobs-a:8080"
: > "$WORK/kp-trace.log"
jobs_cid() { :; }
jobs_up() { echo "up $1 [${2:-}]" >> "$WORK/kp-trace.log"; }
jobs_wait_exited() { echo "exited? $1" >> "$WORK/kp-trace.log"; return "${KP_NOEXIT:-0}"; }
jobs_prove_killed() { echo "prove $1 $2 $3" >> "$WORK/kp-trace.log"; }
jobs_wait_terminal() { echo "terminal $1" >> "$WORK/kp-trace.log"; }
sleep() { :; }
DC() { echo "DC $*" >> "$WORK/kp-trace.log"; return 0; }
jobs_scn() {
  echo "SCN $*" >> "$WORK/kp-trace.log"
  JOBS_RC=0; JOBS_ERR=""; JOBS_OUT=""
  case "$*" in *"${KP_FAIL_AT:-@@never@@}"*) JOBS_RC=1; JOBS_ERR="stand-in failure: the condition does not hold"; return 1;; esac
  case "$1" in
    create|setup) JOBS_OUT="$(printf '%s\n%s' '{"fact":"created","job":"00000000-0000-4000-8000-0000000000aa","pending":9,"credits":27}' '{"fact":"confirm","outcome":"ok","replayed":false,"reserved":27}')";;
    confirm-retry) JOBS_OUT="{\"fact\":\"confirm\",\"outcome\":\"ok\",\"replayed\":${KP_REPLAYED:-true},\"reserved\":${KP_RESERVED:-27}}";;
    state) JOBS_OUT='{"fact":"state","job":"00000000-0000-4000-8000-0000000000aa","events_module_paused":1,"sink_msgs":3,"module_terminal":4}';;
    verify) JOBS_OUT="$(for n in "${SJ_VERIFY_OKS[@]}"; do echo "OK $n"; done; echo '{"fact":"verify","status":"succeeded","pending":9}')";;
  esac
  return 0
}
EOF
kprun() { # <module> <point> <nth> -> the output, then the trace
  ( set -u; W="$T/kp"; rm -rf "$W"; mkdir -p "$W"; WORK="$W"; export WORK; src; . "$T/kp-env.sh"; sj_kill_point "$1" "$2" "$3"; echo "RC-OF-KP: $?"; echo "TRACE:"; cat "$W/kp-trace.log"; echo "KILLS: $(cat "$W/sj-kill.log" 2>/dev/null | tr '\n' '|')" ) 2>&1
}
out="$(kprun m2 submit.after_module "")"; rc=$?
verdict "sj_kill_point submit.after_module: green with everything answering" 0 "RC-OF-KP: 0" "$out" "$rc"
verdict "sj_kill_point: ... the instance is armed with the point, then re-created without it" 0 "up jobs-a [submit.after_module]" "$out" "$rc"
verdict "sj_kill_point: ... the job is set up as a send job of the module, with its hold and its bounces" 0 "SCN setup --api http://jobs-a:8080 --module m2 --rows 12 --bad 2 --dup 1 --bounce 2 --hold-ms 2000 --tag kill-submit.after_module" "$out" "$rc"
verdict "sj_kill_point: ... the kill is proven for the point at its first hit" 0 "prove jobs-a submit.after_module 1" "$out" "$rc"
verdict "sj_kill_point: ... the mid-state is asserted" 0 "SCN state --job 00000000-0000-4000-8000-0000000000aa --expect $(mid m2 submit.after_module)" "$out" "$rc"
verdict "sj_kill_point: ... and the kill is kept for the summary" 0 "KILLS: SJ KILL m2 submit.after_module|" "$out" "$rc"
verdict "sj_kill_point: ... the confirm is replayed, the job verified by the normal path" 0 "SCN verify --job 00000000-0000-4000-8000-0000000000aa --path normal" "$out" "$rc"
verdict "sj_kill_point: ... and the restart has no fault" 0 "up jobs-a []" "$out" "$rc"
out="$(kprun m3 settle.after_commit 2)"; rc=$?
verdict "sj_kill_point settle.after_commit: armed at its count" 0 "up jobs-a [settle.after_commit:2]" "$out" "$rc"
verdict "sj_kill_point settle.after_commit: proven at hit 2" 0 "prove jobs-a settle.after_commit 2" "$out" "$rc"
out="$(KP_REPLAYED=false kprun m3 confirm.before_commit "")"; rc=$?
verdict "sj_kill_point confirm.before_commit: green when the retried confirm is the FIRST" 0 "RC-OF-KP: 0" "$out" "$rc"
verdict "sj_kill_point confirm.before_commit: the draft is created, not confirmed, then the confirm dies inside the request" 0 "SCN confirm --api http://jobs-a:8080 --job 00000000-0000-4000-8000-0000000000aa --expect no-response" "$out" "$rc"
verdict "sj_kill_point confirm.before_commit: ... created by create (no setup)" 0 "SCN create --api" "$out" "$rc" "SCN setup"
out="$(kprun m3 confirm.before_commit "")"; rc=$?
verdict "sj_kill_point confirm.before_commit: a retried confirm that is a replay is red" 1 "should be the FIRST confirm" "$out" "$rc"
out="$(KP_REPLAYED=false kprun m2 release.mid_tx "")"; rc=$?
verdict "sj_kill_point release.mid_tx: a retried confirm that is not a replay is red" 1 "should be a REPLAY" "$out" "$rc"
out="$(KP_RESERVED=26 kprun m2 release.mid_tx "")"; rc=$?
verdict "sj_kill_point: a replay that reports other credits than the preview is red" 1 "the same 27 credits" "$out" "$rc"
out="$(kprun m2 pause.after_module "")"; rc=$?
verdict "sj_kill_point pause.after_module: green" 0 "RC-OF-KP: 0" "$out" "$rc"
verdict "sj_kill_point pause.after_module: the job is seen running with a first result before the pause" 0 "SCN wait --job 00000000-0000-4000-8000-0000000000aa --until running --min-results 1" "$out" "$rc"
verdict "sj_kill_point pause.after_module: the user pauses the RUNNING job through the API" 0 "SCN move --api http://jobs-a:8080 --job 00000000-0000-4000-8000-0000000000aa --op pause --expect-status paused" "$out" "$rc"
verdict "sj_kill_point pause.after_module: the mid-state is read with the module's report" 0 "--expect $(mid m2 pause.after_module) --report" "$out" "$rc"
verdict "sj_kill_point pause.after_module: nothing new at the sink while held, by the module's report too" 0 "--expect status=paused,events_module_paused=1,sink_msgs=3,module_terminal=4,module_report_paused=1 --report" "$out" "$rc"
verdict "sj_kill_point pause.after_module: the user resumes" 0 "--op resume --expect-status running" "$out" "$rc"
verdict "sj_kill_point pause.after_module: verified by the pause path" 0 "SCN verify --job 00000000-0000-4000-8000-0000000000aa --path pause" "$out" "$rc"
out="$(kprun m3 cancel.after_module "")"; rc=$?
verdict "sj_kill_point cancel.after_module: the user cancels the RUNNING job" 0 "--op cancel --expect-status cancelling" "$out" "$rc"
verdict "sj_kill_point cancel.after_module: verified by the cancel path" 0 "SCN verify --job 00000000-0000-4000-8000-0000000000aa --path cancel" "$out" "$rc"
verdict "sj_kill_point cancel.after_module: no resume" 0 "RC-OF-KP: 0" "$out" "$rc" "--op resume"
out="$(KP_FAIL_AT="--expect $(mid m2 submit.before_module)" kprun m2 submit.before_module "")"; rc=$?
verdict "sj_kill_point: a mid-state that does not hold is red, naming the stage and the point" 1 "SJ-K m2 submit.before_module: the database, the module's tables and the sink are not as they must be" "$out" "$rc" "RC-OF-KP: 0"
verdict "sj_kill_point: ... and nothing is verified after it" 1 "stand-in failure" "$out" "$rc" "SCN verify"
out="$(KP_NOEXIT=1 kprun m2 submit.before_module "")"; rc=$?
verdict "sj_kill_point: a process that never died is red, naming the point" 1 "the fault point submit.before_module was never hit" "$out" "$rc"

# ── sj_secret_leak ──
rnd() { local n="$1" i=0 s=""; while [ "$i" -lt "$n" ]; do s="$s$(printf '%02x' $((RANDOM % 256)))"; i=$((i+1)); done; printf '%s\n' "$s"; }
b64u() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '+/' '-_'; }
FAKE_JWT="$(b64u '{"alg":"HS256","typ":"JWT"}').$(b64u '{"sub":"stand-in","role":"authenticated"}').$(b64u 'a-signature-of-some-length')"   # built here, never stored
S1="$(rnd 24)"; S2="$(rnd 24)"; S3="$(rnd 24)"
leak_run() { # <jobs-a log> <m2 log> <program output> [kept instance log] [few]
  rm -rf "$T/lk"; mkdir -p "$T/lk"; printf '%s' "$1" > "$T/lk/jobs-a.txt"; printf '%s' "$2" > "$T/lk/m2.txt"; printf 'worker up' > "$T/lk/m2-worker.txt"; : > "$T/lk/smtp-ctl.txt"; printf '%s\n' "$3" > "$T/lk/jobs-run.log"
  [ -z "${4:-}" ] || printf '%s\n' "$4" > "$T/lk/sj-kept-jobs-a-1.log"
  ( set -u; WORK="$T/lk"; export WORK; src; SJ_MODS="m2"
    DC() { if [ "$1" = logs ]; then cat "$T/lk/${@: -1}.txt"; else return 1; fi; }
    JOBS_JWT="$S1"; M2_TOKEN="$S2"; M3_TOKEN="$S3"; M3_ADMIN_KEY="$(rnd 24)"; SINK_PASS="$(rnd 12)"; M2_ADMIN_KEY="$(rnd 24)"; M2_WORKER_TOKEN="$(rnd 24)"; M3_WORKER_TOKEN="$(rnd 24)"; M3_WORKER2_TOKEN="$(rnd 24)"; M3_WORKER3_TOKEN="$(rnd 24)"
    if [ -n "${5:-}" ]; then M3_TOKEN=""; M3_ADMIN_KEY=""; SINK_PASS=""; M2_ADMIN_KEY=""; M2_WORKER_TOKEN=""; M3_WORKER_TOKEN=""; M3_WORKER2_TOKEN=""; M3_WORKER3_TOKEN=""; fi
    sj_secret_leak ) 2>&1
}
out="$(leak_run 'engine started' 'control up' 'verify ok')"; rc=$?
verdict "sj_secret_leak: clean logs" 0 "none of the 10 credentials of this run" "$out" "$rc"
out="$(leak_run "jwt $S1 here" 'control up' 'verify ok')"; rc=$?
verdict "sj_secret_leak: the JWT secret in the instance's log is red, names the log, shows nothing" 1 "appears in sjleak-jobs-a.log (the log is not shown" "$out" "$rc" "$S1"
out="$(leak_run 'engine' "token $S2" 'verify ok')"; rc=$?
verdict "sj_secret_leak: module 2's service token in module 2's log is red" 1 "appears in sjleak-m2.log" "$out" "$rc" "$S2"
out="$(leak_run 'engine' 'control up' 'verify ok' "an old container said $S3")"; rc=$?
verdict "sj_secret_leak: a credential in the log of an instance re-created away is red" 1 "appears in sj-kept-jobs-a-1.log" "$out" "$rc" "$S3"
out="$(leak_run 'engine' 'control up' "Authorization: Bearer $FAKE_JWT")"; rc=$?
verdict "sj_secret_leak: a JWT-shaped string in the scenario program's output is red" 1 "something shaped like a JWT" "$out" "$rc" "$FAKE_JWT"
out="$(leak_run '' 'control up' 'verify ok')"; rc=$?
verdict "sj_secret_leak: an empty instance log is a scan of nothing" 1 "the log of jobs-a, which the leak check must read, is empty" "$out" "$rc"
out="$(leak_run 'engine' 'control up' 'verify ok' "" few)"; rc=$?
verdict "sj_secret_leak: too few credentials known" 1 "credential(s) of this run are known to the leak scan" "$out" "$rc"

# ── sj_not_checked ──
out="$( WORK="$T"; src; sj_not_checked )"; rc=$?
verdict "sj_not_checked: says the leader kill is not repeated for the senders" 0 "the TWO-INSTANCE leader kill is NOT repeated for the senders" "$out" "$rc"
verdict "sj_not_checked: ends with one grep-able line" 0 "line-ok" "$(printf '%s\n' "$out" | tail -1 | grep -q '^NOT CHECKED: the two-instance leader kill for the senders' && echo line-ok || echo LINE-MISSING)" 0

# ── the wiring of the stage: its text is read (it needs Docker to run) ──
wire="$( WORK="$T"; src; declare -f sj_run sj_scope_run sj_outage sj_control )"
verdict "sj_run: per sender the control, the kills and the outage, then the summary, the leak scan and the exposure check" 0 "line-ok" "$(printf '%s\n' "$wire" | sed -n '/^sj_run ()/,/^}/p' | grep -oE '^ +(sj_[a-z_0-9]+)' | tr -d ' ' | tr '\n' ' ' | grep -q 'sj_infra sj_control sj_kills sj_outage sj_summary sj_secret_leak sj_exposure' && echo line-ok || echo WRONG-ORDER)" 0
verdict "sj_scope_run: calls sj_run and ends the run" 0 "exit 0" "$(printf '%s\n' "$wire" | sed -n '/^sj_scope_run ()/,/^}/p')" 0
verdict "sj_outage: the module is proven gone, a stale event awaited, the module started, the job verified, the control read" 0 "line-ok" "$(o="$(printf '%s\n' "$wire" | sed -n '/^sj_outage ()/,/^}/p')"; for w in 'sj_gone' 'events_stale' 'm2w_outage_worker' 'sj_m3_workers' 'sj_verify "SJ-O $m" "$JOB" normal' 'SJ_CONTROL_JOB'; do printf '%s' "$o" | grep -qF -- "$w" || { echo "missing $w"; exit 0; }; done; echo line-ok)" 0
verdict "sj_control: the control is verified and its job kept for the outage's pairing" 0 'SJ_CONTROL_JOB="$JOB"' "$wire" 0

# ── consistency of the files of the scope ──
GOV="$HERE/../jobscenario/send_verify.go"; COMPOSE="$HARNESS/docker-compose.sendjobs.yml"; RUNSH="$HARNESS/run.sh"; REG="$ROOT/internal/faultpoint/names.txt"; STG="$HERE/send-stage.sh"
goloc="$(sed -n '/^var sendVerifyOKs = \[\]string{/,/^}/p' "$GOV" | sed -n 's/^[[:space:]]*"\([a-z0-9_]*\)",$/\1/p')"
verdict "consistency: the stage's invariants are the scenario program's sendVerifyOKs, in the same order" 0 "same" "$([ -n "$goloc" ] && [ "$goloc" = "$(oks)" ] && echo same || printf 'go: %s\nstage: %s' "$(printf '%s' "$goloc" | tr '\n' ' ')" "$(oks | tr '\n' ' ')")" 0
bad_names=""
for name in $goloc; do [ "$(grep -cE "(v\.check|needReport)\(\"$name\"" "$GOV")" = 1 ] || bad_names="$bad_names $name"; done
verdict "consistency: every invariant is checked exactly once in send_verify.go (v.check or needReport)" 0 "once-each" "$([ -z "$bad_names" ] && echo once-each || echo "not exactly once:$bad_names")" 0
verdict "consistency: no invariant name appears twice in the stage's list" 0 "unique" "$([ -z "$(oks | sort | uniq -d)" ] && echo unique || echo duplicated)" 0
registry="$(sed -n 's/^\([a-z0-9_]*\.[a-z0-9_]*\)[[:space:]]*#.*/\1/p' "$REG" | sort)"
jpoints="$( . "$HERE/stage.sh" >/dev/null 2>&1; printf '%s\n' "${JOBS_POINTS[@]}" )"
verdict "consistency: every kill point of the senders is a name of the registry" 0 "listed" "$(miss=""; for p in $(pts); do printf '%s\n' "$registry" | grep -qx "$p" || miss="$miss $p"; done; [ -z "$miss" ] && echo listed || echo "not in the registry:$miss")" 0
verdict "consistency: the senders' points and module 1's are the registry, together" 0 "same" "$([ "$(printf '%s\n%s\n' "$(pts)" "$jpoints" | sort -u)" = "$registry" ] && echo same || echo "stages: $(printf '%s\n%s\n' "$(pts)" "$jpoints" | sort -u | tr '\n' ' ') registry: $(printf '%s' "$registry" | tr '\n' ' ')")" 0
verdict "consistency: the three points added for the senders are among the senders' points" 0 "three" "$(n=0; for p in submit.before_module pause.after_module cancel.after_module; do pts | grep -qx "$p" && n=$((n+1)); done; [ "$n" = 3 ] && echo three || echo "$n of 3")" 0
verdict "consistency: every point has a case in sj_mid" 0 "all" "$(miss=""; for p in $(pts); do grep -q "^    $p)" "$STG" || miss="$miss $p"; done; [ -z "$miss" ] && echo all || echo "no mid-state for:$miss")" 0
verdict "consistency: the outage steps are exactly the calls of sj_d6_ok in sj_outage, in order" 0 "same" "$(called="$(sed -n '/^sj_outage() {/,/^}/p' "$STG" | grep -o 'sj_d6_ok "$m" [a-z0-9_]*' | awk '{print $3}')"; [ -n "$called" ] && [ "$called" = "$(d6s)" ] && echo same || printf 'called: %s\nlisted: %s' "$(printf '%s' "$called" | tr '\n' ' ')" "$(d6s | tr '\n' ' ')")" 0
verdict "consistency: the overlay publishes no port" 0 "none" "$(grep -qE '^[[:space:]]*ports:' "$COMPOSE" && echo "a ports: key" || echo none)" 0
verdict "consistency: the overlay builds nothing and pulls nothing (it only adds environment to services of the other files)" 0 "none" "$(grep -qE '^[[:space:]]*(image|build):' "$COMPOSE" && echo "an image or a build" || echo none)" 0
verdict "consistency: the overlay touches exactly jobs-a, jobs-b and jobscenario" 0 "three" "$([ "$(grep -E '^  [a-z0-9-]+:$' "$COMPOSE" | tr -d ' :' | tr '\n' ' ')" = "jobs-a jobs-b jobscenario " ] && echo three || grep -E '^  [a-z0-9-]+:$' "$COMPOSE" | tr '\n' ' ')" 0
blk() { awk -v s="  $1:" '$0==s {p=1; next} p && /^  [a-z0-9]/ {p=0} p' "$COMPOSE" | grep -E '^      [A-Z]'; }
verdict "consistency: both instances get the same senders' lines (module 2's URL and token, module 3's token, the two staleness limits at one minute)" 0 "same" "$([ -n "$(blk jobs-a)" ] && [ "$(blk jobs-a)" = "$(blk jobs-b)" ] && [ "$(blk jobs-a | wc -l | tr -d ' ')" = 5 ] && blk jobs-a | grep -q 'PLATFORM_JOB_STALE_AFTER_MIN_M2: "1"' && blk jobs-a | grep -q 'PLATFORM_JOB_STALE_AFTER_MIN_M3: "1"' && blk jobs-a | grep -q 'PLATFORM_M2_SERVICE_TOKEN: ${H_M2_TOKEN:?}' && echo same || echo DIFFERENT)" 0
missing=""; nvars=0
for v in $(grep -ohE '"SENDJOBS_[A-Z0-9_]+"' "$HERE"/../jobscenario/*.go | tr -d '"' | sort -u); do
  nvars=$((nvars+1)); blk jobscenario | grep -q "^      $v:" || missing="$missing $v"
done
verdict "consistency: the scenario program's container is given every SENDJOBS_ variable the program names (and it names at least eight)" 0 "given" "$([ -z "$missing" ] && [ "$nvars" -ge 8 ] && echo given || echo "missing:$missing, the program names $nvars")" 0
missing=""
for v in $(grep -o '\${H_[A-Z0-9_]*' "$COMPOSE" | sed 's/^\${//' | sort -u); do
  grep -q "^$v=" "$RUNSH" || grep -q "echo \"$v=" "$HARNESS/m2worker/stage.sh" || grep -q "echo \"$v=" "$HERE/stage.sh" || missing="$missing $v"
done
verdict "consistency: every H_ variable the overlay needs is written by run.sh, m2w_mint or jobs_mint" 0 "written" "$([ -z "$missing" ] && echo written || echo "nobody writes:$missing")" 0
verdict "consistency: the overlay names no fault point and no build tag (the instances are armed by the jobs overlay alone)" 0 "none" "$(grep -qiE 'faultpoint' "$COMPOSE" && echo "names it" || echo none)" 0
cmds="$(grep -vE '^[[:space:]]*#' "$STG" | grep -oE 'jobs_scn [a-z][a-z-]*' | awk '{print $2}' | sort -u)"
miss=""; for c in $cmds; do grep -q "case \"$c\":" "$HERE/../jobscenario/main.go" || miss="$miss $c"; done
verdict "consistency: every command the stage gives the scenario program is a command of it (at least seven kinds)" 0 "all-commands" "$([ -z "$miss" ] && [ "$(printf '%s\n' "$cmds" | wc -l | tr -d ' ')" -ge 7 ] && echo all-commands || echo "no such command:$miss")" 0
verdict "consistency: every Go file of the scenario program carries the build tag harness on its first line" 0 "tagged" "$(b=""; for f in "$HERE"/../jobscenario/*.go; do [ "$(head -1 "$f")" = '//go:build harness' ] || b="$b $(basename "$f")"; done; [ -z "$b" ] && echo tagged || echo "not tagged:$b")" 0
# run.sh
verdict "consistency: run.sh allows the three scopes and keeps the line the other stages' selftests read" 0 "allowed" "$(grep -q 'case "\$SCOPE" in full|m1node|m2worker|gotrue|jobs) ;; sendjobs|sendjobs-m2|sendjobs-m3) ;;' "$RUNSH" && echo allowed || echo NOT-ALLOWED)" 0
verdict "consistency: run.sh sources the stage after the module 1 stage it builds on" 0 "sourced" "$(a="$(grep -n '^\. "\$HERE/jobs/stage.sh"' "$RUNSH" | cut -d: -f1)"; b="$(grep -n '^\. "\$HERE/jobs/send-stage.sh"' "$RUNSH" | cut -d: -f1)"; c="$(grep -n '^\. "\$HERE/m2worker/stage.sh"' "$RUNSH" | cut -d: -f1)"; [ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ] && [ "$b" -gt "$a" ] && [ "$b" -gt "$c" ] && echo sourced || echo "jobs $a send $b m2worker $c")" 0
for sc in "sendjobs|m2 m2-worker m3 m3-worker m3-worker-2 m3-worker-3 smtp-ctl m3dns jobs-a jobs-b" "sendjobs-m2|m2 m2-worker smtp-ctl jobs-a jobs-b" "sendjobs-m3|m3 m3-worker m3-worker-2 m3-worker-3 smtp-ctl m3dns jobs-a jobs-b"; do
  name="${sc%%|*}"; want="migrate-platform migrate-m1 migrate-m2 migrate-m3 ${sc#*|}"
  verdict "consistency: the scope $name builds only what it needs" 0 "scoped" "$(grep -qE "^  $name\) +BUILD_ONLY=\"$want\";;" "$RUNSH" && echo scoped || echo missing)" 0
done
unguarded="$(grep -nE '(^|[^_a-z])sj_(run|scope_run|not_checked)( |$)' "$RUNSH" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE '\[ "\$SJ_ON" = 1 \] &&' | head -5)"
verdict "consistency: every call of a stage function in run.sh is under the SJ_ON guard" 0 "all-guarded" "$([ -z "$unguarded" ] && echo all-guarded || echo "UNGUARDED: $unguarded")" 0
verdict "consistency: run.sh calls the scope run, the full run's stage and its NOT CHECKED lines, once each" 0 "three" "$([ "$(grep -cE '^\[ "\$SJ_ON" = 1 \] && (\[ "\$SCOPE" (!=|=) full \] && )?sj_(scope_run|run|not_checked)' "$RUNSH")" = 3 ] && echo three || echo WRONG)" 0
verdict "consistency: the docker compose function merges the overlay only through SJ_DC" 0 "merged" "$(grep -q '\${SJ_DC\[@\]+"\${SJ_DC\[@\]}"}' "$RUNSH" && grep -q 'SJ_DC=(-f "\$HERE/docker-compose.sendjobs.yml")' "$RUNSH" && echo merged || echo missing)" 0
verdict "consistency: the senders' scopes switch on the instances' overlay (JOBS_ON) and module 2's credentials (M2W_ON)" 0 "on" "$(grep -q 'case "\$SCOPE" in jobs|sendjobs|sendjobs-m2|sendjobs-m3) JOBS_ON=1;; esac' "$RUNSH" && grep -q 'case "\$SCOPE" in m2worker|sendjobs|sendjobs-m2|sendjobs-m3) M2W_ON=1;; esac' "$RUNSH" && echo on || echo missing)" 0
# DORMANCY, executed on the real text of run.sh
block="$(sed -n '/^SJ_ON=0; SJ_DC=(); SJ_MODS=""$/,/^\[ "\$SJ_ON" = 1 \] && SJ_DC=/p' "$RUNSH")"
verdict "dormancy: the guard block of run.sh was found (its subject is not empty)" 0 "found" "$([ "$(printf '%s\n' "$block" | wc -l | tr -d ' ')" = 4 ] && echo found || echo "$(printf '%s\n' "$block" | wc -l) lines")" 0
sj_guard_state() { ( set -u; HERE=/h; SCOPE="$1"; FULL_INCLUDES="$2"; eval "$block"; printf 'SJ_ON=%s files=%s mods=[%s]' "$SJ_ON" "${#SJ_DC[@]}" "$SJ_MODS" ); }
verdict "dormancy: the full run with FULL_INCLUDES as it stands in run.sh" 0 "$(f="$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1)"; printf ' %s ' "$f" | grep -q ' sendjobs ' && printf ' %s ' "$f" | grep -q ' jobs ' && echo 'SJ_ON=1 files=2' || echo 'SJ_ON=0 files=0')" "$(sj_guard_state full "$(sed -n 's/^FULL_INCLUDES="\([^"]*\)".*/\1/p' "$RUNSH" | head -1)")" 0
verdict "dormancy: the full run with FULL_INCLUDES = m2worker m1node: nothing on" 0 "SJ_ON=0 files=0 mods=[]" "$(sj_guard_state full 'm2worker m1node')" 0
verdict "dormancy: the full run naming sendjobs WITHOUT jobs stays off (the instances are the jobs overlay's)" 0 "SJ_ON=0 files=0 mods=[]" "$(sj_guard_state full 'm2worker m1node sendjobs')" 0
verdict "dormancy: the full run naming sendjobs and jobs switches it on for both senders" 0 "SJ_ON=1 files=2 mods=[m2 m3]" "$(sj_guard_state full 'm2worker m1node jobs sendjobs')" 0
verdict "dormancy: a name that merely contains sendjobs does not switch it on" 0 "SJ_ON=0 files=0 mods=[]" "$(sj_guard_state full 'jobs sendjobs2 notsendjobs')" 0
verdict "dormancy: the scope jobs does not switch it on" 0 "SJ_ON=0 files=0 mods=[]" "$(sj_guard_state jobs 'jobs sendjobs')" 0
verdict "dormancy: the scope sendjobs: both senders" 0 "SJ_ON=1 files=2 mods=[m2 m3]" "$(sj_guard_state sendjobs '')" 0
verdict "dormancy: the scope sendjobs-m2: module 2 only" 0 "SJ_ON=1 files=2 mods=[m2]" "$(sj_guard_state sendjobs-m2 '')" 0
verdict "dormancy: the scope sendjobs-m3: module 3 only" 0 "SJ_ON=1 files=2 mods=[m3]" "$(sj_guard_state sendjobs-m3 '')" 0
# stage 5b, as one function
verdict "stage 5b: run.sh defines m3w_stage_5b once" 0 "once" "$([ "$(grep -c '^m3w_stage_5b() {' "$RUNSH")" = 1 ] && echo once || echo WRONG)" 0
verdict "stage 5b: the full run calls it once, right after the 5b banner" 0 "called" "$(awk '/^STAGE=m3-worker; banner "5b"/{b=NR} /^m3w_stage_5b$/{c=NR; n++} END{exit !(b && c==b+1 && n==1)}' "$RUNSH" && echo called || echo WRONG)" 0
verdict "stage 5b: its probes are made through m3w_probe, which asks the platform service unless told otherwise" 0 "probes" "$(body="$(sed -n '/^m3w_stage_5b() {/,/^}/p' "$RUNSH")"; ! printf '%s' "$body" | grep -q 'DC exec -T platform' && [ "$(printf '%s' "$body" | grep -c 'm3w_probe http://')" = 3 ] && grep -q '^m3w_probe() { DC exec -T "\${M3W_PROBE_SVC:-platform}" wget -q -O /dev/null "\$1"; }' "$RUNSH" && echo probes || echo WRONG)" 0
verdict "stage 5b: the senders' scope calls the same function with the instance as the prober" 0 "same" "$(grep -q 'M3W_PROBE_SVC=jobs-a m3w_stage_5b' "$STG" && echo same || echo missing)" 0
verdict "stage 5b: psql_su is defined once, at the top level of run.sh (module 3's outage proof uses it)" 0 "once" "$([ "$(grep -c '^psql_su() {' "$RUNSH")" = 1 ] && echo once || echo WRONG)" 0
# the workflows
verdict "workflows: the harness workflow offers the three scopes" 0 "offered" "$(n=0; for s in sendjobs sendjobs-m2 sendjobs-m3; do grep -qE "^          - $s\$" "$ROOT/.github/workflows/harness.yml" && n=$((n+1)); done; [ "$n" = 3 ] && echo offered || echo "$n of 3")" 0
verdict "workflows: the module 1 scope's selftest (run by the gates workflow) runs this selftest" 0 "wired" "$(grep -q 'send-stage-selftest.sh' "$HERE/stage-selftest.sh" && grep -q 'tests/harness/jobs/stage-selftest.sh' "$ROOT/.github/workflows/gates.yml" && echo wired || echo missing)" 0

# ── the control: the verdict function itself fails when the substring is wrong (a selftest that cannot fail proves nothing) ──
before_bad=$bad; before_ok=$ok; before_cases=$cases
verdict "CONTROL (expected to MISMATCH): a wrong expectation" 0 "this text is not in the output" "some other output" 0 >/dev/null
if [ "$bad" -eq $((before_bad+1)) ] && [ "$ok" -eq "$before_ok" ]; then bad=$before_bad; cases=$((before_cases+1)); ok=$((before_ok+1)); echo "  ok [control: the verdict function reports a wrong expectation as a mismatch]"
else echo "  MISMATCH [control]: the verdict function did not fail on a wrong expectation"; bad=$((bad+1)); fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
