#!/usr/bin/env bash
# send-stage.sh — the SENDERS' crash matrix and real outages (Phase 4 close-out D5 and D6), against the REAL platform process, the REAL modules 2 and 3 with their workers, and the controllable SMTP sink
# that counts every message per recipient. SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself), AFTER tests/harness/jobs/stage.sh, whose helpers it uses (jobs_scn,
# jobs_fact, jobs_get, jobs_need, jobs_expect_fail, jobs_up, jobs_wait_exited, jobs_prove_killed, jobs_wait_terminal, jobs_cid), and AFTER tests/harness/m2worker/stage.sh (m2w_stage_5c,
# m2w_outage_worker). From run.sh it uses DC, die, banner, STAGE, WORK, HERE, PLATFORM_DIR, PROJECT_NET, SCOPE, psql_su, m3w_stage_5b, the module credentials of stage 2 (M2_TOKEN, M3_TOKEN,
# M3_ADMIN_KEY, M3_WORKER_TOKEN, M3_WORKER2_TOKEN, M3_WORKER3_TOKEN, SINK_PASS, M2_ADMIN_KEY, M2_WORKER_TOKEN, JOBS_JWT) and the worker ids.
#
# DORMANT BY DEFAULT. run.sh calls these functions only when SJ_ON=1: under H_SCOPE=sendjobs (both senders), sendjobs-m2 or sendjobs-m3 (one each), or in the full run once `sendjobs` AND `jobs` are named in
# FULL_INCLUDES. With SJ_ON=0 nothing here runs and docker-compose.sendjobs.yml is not merged.
#
# THE SCOPES. One stage, a list of senders: `sendjobs` runs module 2 then module 3 (every image of both is built), `sendjobs-m2` and `sendjobs-m3` one of them each (only its images are built). The split
# exists because one sender's matrix is about twenty minutes on a runner and both with every image built near fifty: a red module 2 run must not hide what module 3 does, and the lead can dispatch the two
# halves side by side.
#
# WHAT IS PROVEN, per sender (SJ_MODS), and how each proof is made unable to pass on a run that did not do the thing (the run is RED at the first failed assertion, naming the stage, the point, the database and
# the sink):
#   SJ-C  control   one instance, no fault: a send job of SJ_ROWS lines (SJ_BAD malformed, SJ_DUP repeats, SJ_BOUNCE recipients the sink rejects with 550, every message held SJ_HOLD_MS by the sink) runs to
#                   `succeeded` and `verify` holds; the program's own control: a verify of a send job that was previewed and never confirmed fails for its reason. It is also the paired control of the outage
#                   (the same shape, no stop: no `stale` event).
#   SJ-K  kills     for each of the seven points of SJ_POINTS the platform's OWN process is killed by itself at that point (exit 137, OOMKilled false, the armed build's line), the database, the module's own
#                   tables, its report (pause, cancel) and the SINK are read AT THAT MOMENT and must be the mid-state of the point (sj_mid: a kill that landed at the end proves nothing), the instance is
#                   re-created without the fault, the confirm is replayed, the job is resumed where the point needs it (pause), runs to its end and `verify` holds: no row lost, none sent twice (counted at the
#                   sink per recipient), none charged twice, the drift views empty, module and platform equal item by item.
#   SJ-O  outage    the module's control container is STOPPED in the middle of a running job (module 3: its control and one of its workers), proven gone from the private network; for 25 s nothing is
#                   recorded, charged, invented or sent; after the staleness limit (one minute) a `stale` event exists and the job is still running; the module is started again (and its workers) and the job
#                   completes EXACTLY once (one completing, one succeeded, one release, the sink once per sent item: verify).
#   evidence        every verify prints one `OK <name>` per invariant (SJ_VERIFY_OKS, the program's sendVerifyOKs), every kill keeps one `SJ KILL <module> <point>` line, every outage step one
#                   `SJ D6 OK <module> <name>` line (SJ_D6_OKS); the summary is RED on one missing, doubled or unknown.
# NOT required here, and said so in the NOT CHECKED lines: the two-instance leader kill (proved for module 1 by the scope jobs, J3).
# Nothing here prints a credential: no `set -x`, no environment dump; the scenario program prints neither a secret nor a token.

SJ_POINTS=(confirm.before_commit submit.before_module submit.after_module settle.after_commit release.mid_tx pause.after_module cancel.after_module)
# the lines a send job's `verify` must print, one each (tests/harness/jobscenario/send_verify.go, sendVerifyOKs: the selftest holds the two lists equal)
SJ_VERIFY_OKS=(
  rows_equal_results
  every_pending_row_has_one_result
  results_name_their_rows
  job_ended_as_its_path_says
  one_charge_per_item
  charges_equal_results_with_a_unit
  settlement_drift_empty
  balance_drift_empty
  reserve_closed
  one_release_per_reserve
  module_report_covers_the_submitted_rows
  module_report_equals_results
  module_report_equals_ledger
  module_holds_each_item_once
  sink_at_most_one_message_per_recipient
  sink_one_message_per_sent_item_none_otherwise
  sink_total_equals_sent
  events_follow_the_path_once_each
  nothing_invented
  proof_not_vacuous
  control_checks_detect_corruption
)
# the steps of an outage, each printed once per sender as `SJ D6 OK <module> <name>` (sj_d6_ok; the selftest holds the list equal to the calls of sj_outage)
SJ_D6_OKS=(
  module_gone_from_the_network
  stopped_in_the_middle_of_the_job
  nothing_recorded_charged_or_invented_while_down
  sink_quiet_while_down
  stale_raised_and_the_job_still_running
  module_and_workers_back
  completed_exactly_once
  control_without_a_stop_raised_no_stale
)
SJ_RUNS_PER_MODULE=9                         # the control, seven kills, the outage
SJ_MODS="${SJ_MODS:-m2 m3}"                  # set by run.sh from the scope
SJ_ROWS="${SJ_ROWS:-12}"; SJ_BAD="${SJ_BAD:-2}"; SJ_DUP="${SJ_DUP:-1}"; SJ_BOUNCE="${SJ_BOUNCE:-2}"   # 12 lines: 9 pending, of which 2 bounced and 7 sent
SJ_HOLD_MS="${SJ_HOLD_MS:-2000}"             # the sink holds every message 2 s: one lane sends 7 messages in about 15 s, so a kill, a pause and a cancel land with items still queued
SJ_OUTAGE_HOLD_MS="${SJ_OUTAGE_HOLD_MS:-3000}"   # the outage's job (and its control) is longer: about 25 s of sending, the stop lands after the first result
SJ_SETTLE_NTH="${SJ_SETTLE_NTH:-2}"          # settle.after_commit: the kill lands in the SECOND recording of results
SJ_STALE_WAIT="${SJ_STALE_WAIT:-180}"        # seconds to wait for the stale event (the senders' staleness limit is one minute in the instances: docker-compose.sendjobs.yml)
SJ_VERIFIED=0
SJ_CONTROL_JOB=""

sj_res_of() { case "$1" in m2) echo 1;; *) echo 0;; esac; }   # how many reserves a job of the module holds (module 3 is not metered)
sj_d6_ok() { echo "  SJ D6 OK $1 $2"; echo "SJ D6 OK $1 $2" >> "$WORK/sj-d6.log"; }

# sj_mid <module> <point>: the state the database, the module's own tables, its report and the sink must be in at the moment of the kill (the conditions of `jobscenario state`; @x is another field).
sj_mid() {
  local r; r="$(sj_res_of "$1")"
  case "$2" in
    confirm.before_commit) echo "status=previewed,confirmed=0,confirm_key_set=0,reserves=0,charges=0,results=0,balance_reserved=0,items_ensured=0,job_items=0,module_jobs=0,sink_msgs=0,rows>0";;
    submit.before_module)  echo "status=confirmed,started=0,stamped=0,events_running=0,items_ensured=1,job_items=@pending,module_jobs=0,results_module=0,sink_msgs=0,reserves=$r,releases=0";;
    submit.after_module)   echo "status=confirmed,started=0,stamped=0,events_running=0,items_ensured=1,job_items=@pending,module_jobs>0,module_jobs<=@pending,results_module=0,reserves=$r,releases=0,finished=0";;
    settle.after_commit)   echo "status=running,started=1,finished=0,events_running=1,stamped=@pending,module_jobs=@pending,results_module>0,results_module<@pending,releases=0,reserve_released=0,reserves=$r";;
    release.mid_tx)        echo "status=completing,started=1,finished=0,events_running=1,events_completing=1,results=@rows,results_module=@pending,module_terminal=@pending,releases=0,reserve_released=0,reserves=$r";;
    pause.after_module)    echo "status=paused,events_paused=1,events_module_paused=0,module_report_paused=1,module_report_complete=0,finished=0,releases=0,reserves=$r";;
    cancel.after_module)   echo "status=cancelling,cancel_requested=1,events_cancelling=1,events_completing=0,results_module_cancelled=0,module_cancelled>0,module_report_cancelled>0,finished=0,releases=0,reserves=$r";;
    *) die "sj_mid: $2 is not a point of the senders' matrix";;
  esac
}

# sj_up <service> [fault]: keeps the log of the container about to be re-created (a re-created container's log is gone, and the leak scan reads every one), then jobs_up.
sj_up() {
  SJ_GEN=$(( ${SJ_GEN:-0} + 1 ))
  if [ -n "$(jobs_cid "$1")" ]; then DC logs --no-color "$1" > "$WORK/sj-kept-$1-$SJ_GEN.log" 2>&1 || die "could not keep the log of $1 before it is re-created"; fi
  jobs_up "$@"
}

# sj_setup / sj_create <module> <tag> <hold ms>: a user ready to send at the module and a draft of the scope's list (sj_setup also confirms it); sets JOB, PENDING, CREDITS
sj_args() { echo "--api $JOBS_API_A --module $1 --rows $SJ_ROWS --bad $SJ_BAD --dup $SJ_DUP --bounce $SJ_BOUNCE --hold-ms $3 --tag $2"; }
sj_created() {
  local f; f="$(jobs_fact created)"; JOB="$(jobs_get "$f" job)"; PENDING="$(jobs_get "$f" pending)"; CREDITS="$(jobs_get "$f" credits)"
  [ -n "$JOB" ] && [ "$PENDING" = "$((SJ_ROWS - SJ_BAD - SJ_DUP))" ] || die "$1: the 'created' fact has no job or $PENDING pending rows, want $((SJ_ROWS - SJ_BAD - SJ_DUP)): $f"
}
sj_create() { jobs_scn create $(sj_args "$@"); jobs_need "creating a $1 send job ($2)"; sj_created "$1 $2"; }
sj_setup() {
  jobs_scn setup $(sj_args "$@"); jobs_need "creating and confirming a $1 send job ($2)"; sj_created "$1 $2"
  [ "$(jobs_get "$(jobs_fact confirm)" outcome)" = ok ] || die "$1 $2: the confirm was not accepted: $(jobs_fact confirm)"
}

# sj_expect_oks <label>: JOBS_OUT holds each `OK <name>` of SJ_VERIFY_OKS exactly once and no other OK line
sj_expect_oks() {
  local label="$1" name c total
  for name in "${SJ_VERIFY_OKS[@]}"; do
    c="$(printf '%s\n' "$JOBS_OUT" | grep -cx "OK $name" || true)"
    [ "$c" = 1 ] || die "$label: the scenario program printed 'OK $name' $c time(s), want exactly once: the check did not run (or ran twice)"
  done
  total="$(printf '%s\n' "$JOBS_OUT" | grep -c '^OK ' || true)"
  [ "$total" = "${#SJ_VERIFY_OKS[@]}" ] || die "$label: the scenario program printed $total OK line(s), want exactly ${#SJ_VERIFY_OKS[@]}: it printed a line this stage does not know"
  printf '%s\n' "$JOBS_OUT" | grep '^OK ' | sed "s/^/$label /" >> "$WORK/sj-ok.log"
}

# sj_verify <label> <job> <path>: a finished send job's invariants, every one printed and held, and the sink's per-recipient evidence
sj_verify() {
  local label="$1" job="$2" path="$3" f
  jobs_scn verify --job "$job" --path "$path"
  jobs_need "$label: verify of send job $job (path $path)"
  sj_expect_oks "$label"
  SJ_VERIFIED=$((SJ_VERIFIED+1))
  f="$(jobs_fact verify)"
  echo "  $label: job $job holds ${#SJ_VERIFY_OKS[@]} of ${#SJ_VERIFY_OKS[@]} invariants: $(jobs_get "$f" status) by the path $path; pending $(jobs_get "$f" pending), sent $(jobs_get "$f" sent), bounced $(jobs_get "$f" bounced), cancelled $(jobs_get "$f" cancelled), failed $(jobs_get "$f" failed_items); charge rows $(jobs_get "$f" charge_rows); the sink holds $(jobs_get "$f" sink_messages) message(s) for the job's recipients"
  echo "     the sink, per recipient (messages / RCPT accepted, rejected, deferred / the module's state):"
  printf '%s\n' "$JOBS_OUT" | grep '^{"fact":"sink"' | while IFS= read -r l; do
    echo "       $(jobs_get "$l" rcpt): $(jobs_get "$l" messages) / $(jobs_get "$l" rcpt_accepted), $(jobs_get "$l" rcpt_rejected), $(jobs_get "$l" rcpt_deferred) / $(jobs_get "$l" module_state)"
  done
}

# sj_state <label> <job> <conditions> [--report]: the state must hold the conditions; red names the stage, what was read and what did not hold
sj_state() {
  local label="$1" job="$2" cond="$3"; shift 3
  jobs_scn state --job "$job" --expect "$cond" "$@"
  jobs_need "$label: the database, the module's tables and the sink are not as they must be ($cond)"
}

sj_stop_others() { DC stop -t 10 jobs-b >/dev/null 2>&1 || true; }   # ONE instance: the survivor of a kill is the restarted instance, not a second one that was already there

# ── SJ-C: the control ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
sj_control() {
  local m="$1"
  STAGE="sendjobs-$m-control"; banner "SJ-C $m" "no fault: a send job of $SJ_ROWS lines at module ${m#m} runs to the end, every invariant holds, the sink holds each sent recipient once; the verifier fails where it must"
  sj_stop_others; sj_up jobs-a ""
  sj_setup "$m" control "$SJ_OUTAGE_HOLD_MS"
  SJ_CONTROL_JOB="$JOB"
  jobs_wait_terminal "$JOB" 600
  sj_verify "SJ-C $m" "$JOB" normal
  sj_state "SJ-C $m" "$JOB" "status=succeeded,events_stale=0,events_completing=1,events_running=1,sink_max=1,sink_foreign=0"
  sj_program_controls "$m"
}

# the program's own control: a send job that was previewed and never confirmed is not a finished job
sj_program_controls() {
  sj_create "$1" never-confirmed 0
  jobs_expect_fail "SJ-C $1 control: verify of a send job that was previewed and never confirmed" "FAIL job_ended_as_its_path_says" verify --job "$JOB"
  jobs_expect_fail "SJ-C $1 control: ... and it names the module's report that holds nothing" "FAIL proof_not_vacuous" verify --job "$JOB"
}

# ── SJ-K: kills at the seven points ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# sj_kill_point <module> <point> <nth or "">
sj_kill_point() {
  local m="$1" point="$2" nth="$3" spec f path=normal mid i s1 m1
  spec="$point"; [ -z "$nth" ] || spec="$point:$nth"
  STAGE="sendjobs-$m-kill-$point"; banner "SJ-K $m" "kill the platform at $spec during a module ${m#m} send job: the process kills ITSELF there; the database, the module and the sink are read at that moment; restarted without the fault, the job must end as if nothing had happened, nothing sent twice"
  sj_stop_others; sj_up jobs-a "$spec"
  case "$point" in
    confirm.before_commit)
      sj_create "$m" "kill-$point" "$SJ_HOLD_MS"
      echo "  the draft is previewed (job $JOB, $PENDING pending); the confirm is sent and the process dies INSIDE the request, with the transaction open"
      jobs_scn confirm --api "$JOBS_API_A" --job "$JOB" --expect no-response
      jobs_need "SJ-K $m $point: the confirm was meant to get no answer";;
    pause.after_module|cancel.after_module)
      sj_setup "$m" "kill-$point" "$SJ_HOLD_MS"
      JOBS_QUIET=1 jobs_scn wait --job "$JOB" --until running --min-results 1 --timeout 180 --poll-ms 50
      [ "$JOBS_RC" = 0 ] || { cat "$WORK/jobs-scn-last.txt"; die "SJ-K $m $point: the job did not reach running with a first result recorded: $(printf '%s' "$JOBS_ERR" | tail -2 | tr '\n' ' ')"; }
      if [ "$point" = pause.after_module ]; then
        path=pause; jobs_scn move --api "$JOBS_API_A" --job "$JOB" --op pause --expect-status paused; jobs_need "SJ-K $m $point: the user's pause of the running job"
      else
        path=cancel; jobs_scn move --api "$JOBS_API_A" --job "$JOB" --op cancel --expect-status cancelling; jobs_need "SJ-K $m $point: the user's cancel of the running job"
      fi
      echo "  the user's ${point%%.*} of the RUNNING job was answered; the engine tells the module at its next look and the process dies right after the module's answer";;
    *)
      sj_setup "$m" "kill-$point" "$SJ_HOLD_MS";;
  esac
  jobs_wait_exited jobs-a 150 || { JOBS_QUIET=0 jobs_scn state --job "$JOB" || true; die "SJ-K $m $point: jobs-a is still running 150 s after the job was driven: the fault point $spec was never hit (the job's state is above), or the armed variable did not reach the process"; }
  jobs_prove_killed jobs-a "$point" "${nth:-1}"
  # the mid-state: nothing of the platform writes now (module 2 or 3 works on, which can change only the module's columns and the sink, and the conditions name only what the dead process left)
  mid="$(sj_mid "$m" "$point")"
  case "$point" in pause.after_module|cancel.after_module) sj_state "SJ-K $m $point" "$JOB" "$mid" --report;; *) sj_state "SJ-K $m $point" "$JOB" "$mid";; esac
  echo "  mid-state holds at $spec: $mid"
  echo "SJ KILL $m $point" >> "$WORK/sj-kill.log"
  sj_up jobs-a ""
  jobs_scn confirm-retry --api "$JOBS_API_A" --job "$JOB"
  jobs_need "SJ-K $m $point: confirm-retry"
  f="$(jobs_fact confirm)"
  if [ "$point" = confirm.before_commit ]; then
    [ "$(jobs_get "$f" replayed)" = false ] && [ "$(jobs_get "$f" reserved)" = "$CREDITS" ] || die "SJ-K $m $point: the retried confirm should be the FIRST confirm (replayed false, reserved $CREDITS credits) because the transaction rolled back, got: $f"
    echo "  the retried confirm is the first confirm of the job: it held $(jobs_get "$f" reserved) credits, once"
  else
    [ "$(jobs_get "$f" replayed)" = true ] && [ "$(jobs_get "$f" reserved)" = "$CREDITS" ] || die "SJ-K $m $point: the retried confirm should be a REPLAY (replayed true, the same $CREDITS credits), got: $f"
    echo "  the retried confirm is a replay (the same key): it wrote nothing and reports the $(jobs_get "$f" reserved) credits the first confirm held"
  fi
  if [ "$point" = pause.after_module ]; then
    # the restarted engine reads the module's report, sees the campaign held and writes the module_paused note; nothing new reaches the sink while it is held; the user resumes
    i=0; while [ "$i" -lt 60 ]; do
      JOBS_QUIET=1 jobs_scn state --job "$JOB"; jobs_need "SJ-K $m $point: the state while waiting for the module_paused note"
      [ "$(jobs_get "$(jobs_fact state)" events_module_paused)" = 1 ] && break
      i=$((i+1)); sleep 1
    done
    [ "$i" -lt 60 ] || die "SJ-K $m $point: the restarted engine wrote no module_paused note in 60 s: $(jobs_fact state | cut -c1-400)"
    sleep 4   # an item that was in flight when the pause came finishes (the sink holds a message $SJ_HOLD_MS ms)
    JOBS_QUIET=1 jobs_scn state --job "$JOB"; jobs_need "SJ-K $m $point: the state while paused"
    s1="$(jobs_get "$(jobs_fact state)" sink_msgs)"; m1="$(jobs_get "$(jobs_fact state)" module_terminal)"
    sleep 8
    sj_state "SJ-K $m $point (held)" "$JOB" "status=paused,events_module_paused=1,sink_msgs=$s1,module_terminal=$m1,module_report_paused=1" --report
    echo "  the module holds the campaign (its report says paused, the note is written once): for 8 s nothing new reached the sink ($s1 message(s)) and no item ended ($m1); the user resumes"
    jobs_scn move --api "$JOBS_API_A" --job "$JOB" --op resume --expect-status running; jobs_need "SJ-K $m $point: the user's resume"
  fi
  jobs_wait_terminal "$JOB" 600
  sj_verify "SJ-K $m $point" "$JOB" "$path"
}

sj_kills() {
  local m="$1" p
  for p in "${SJ_POINTS[@]}"; do
    case "$p" in settle.after_commit) sj_kill_point "$m" "$p" "$SJ_SETTLE_NTH";; *) sj_kill_point "$m" "$p" "";; esac
  done
}

# ── SJ-O: the module is stopped in the middle of a job ────────────────────────────────────────────────────────────────────────────────────────────────────────────
sj_probe() { DC run --rm -T --no-deps --entrypoint wget jobscenario -q -T 3 -O /dev/null "$1" 2>&1; }

# sj_gone <module service>: the container is exited AND a request from a container on the private network gets no HTTP answer
sj_gone() {
  local svc="$1" st i=0 out rc
  while [ "$i" -lt 60 ]; do st="$(docker inspect -f '{{.State.Status}}' "$(jobs_cid "$svc")" 2>/dev/null)"; [ "$st" = exited ] && break; i=$((i+1)); sleep 2; done
  [ "$st" = exited ] || die "SJ-O: $svc is '$st' 120 s after the stop"
  out="$(sj_probe "http://$svc:8080/readyz")"; rc=$?
  [ "$rc" != 0 ] || die "SJ-O: $svc still answers: a request from a container on the private network to http://$svc:8080/readyz succeeded although the container is $st"
  case "$out" in
    *"server returned error"*) die "SJ-O: $svc still answers over the private network (an HTTP error, so something listens at the name): $(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
    *"Connection refused"*|*"bad address"*|*"can't connect"*|*"timed out"*|*"nreachable"*) ;;
    *) die "SJ-O: the probe from the private network did not run, so 'gone' was not proven (wget printed: $(printf '%s' "$out" | head -3 | tr '\n' ' '))";;
  esac
  echo "  $svc is gone: the container is '$st' and a request from a container on the private network gets: $(printf '%s\n' "$out" | grep -m1 'wget:' || printf '%s\n' "$out" | head -1)"
}

# sj_m3_workers: module 3's three workers running, each with a heartbeat newer than now
sj_m3_workers() {
  local t0 wid i
  t0="$(psql_su "select clock_timestamp()")"; [ -n "$t0" ] || die "could not read the database clock"
  DC up -d --no-deps m3-worker m3-worker-2 m3-worker-3 >/dev/null 2>&1 || die "module 3's workers did not start again"
  for wid in "$M3_WORKER_ID" "$M3_WORKER2_ID" "$M3_WORKER3_ID"; do
    i=0; while [ "$i" -lt 60 ]; do
      [ "$(psql_su "select count(*) from send_byo.boxes where id = '$wid' and status = 'active' and last_heartbeat_at > '$t0'")" = 1 ] && break
      i=$((i+1)); sleep 2
    done
    [ "$i" -lt 60 ] || die "module 3's worker $wid has no heartbeat after $t0: it did not register with the Control again"
  done
  echo "  module 3's three workers are running and each heartbeated after $t0"
}

sj_outage() {
  local m="$1" svc f r0 c0 s0 keep i
  STAGE="sendjobs-$m-outage"; banner "SJ-O $m" "module ${m#m}'s control is STOPPED while a send job runs: the engine records, charges, invents and sends nothing, raises a stale event and waits; the module returns and the job completes exactly once"
  case "$m" in m2) svc=m2;; *) svc=m3;; esac
  sj_stop_others; sj_up jobs-a ""
  sj_setup "$m" outage "$SJ_OUTAGE_HOLD_MS"
  JOBS_QUIET=1 jobs_scn wait --job "$JOB" --until running --min-results 1 --timeout 180 --poll-ms 50
  [ "$JOBS_RC" = 0 ] || { cat "$WORK/jobs-scn-last.txt"; die "SJ-O $m: the job did not reach running with a first result recorded: $(printf '%s' "$JOBS_ERR" | tail -2 | tr '\n' ' ')"; }
  if [ "$m" = m2 ]; then
    echo "  the job is running with its first results recorded; module 2's control is stopped now (docker compose stop: SIGTERM, a graceful drain; its grace period is 90 s)"
    DC stop m2 >/dev/null 2>&1 || die "SJ-O $m: could not stop module 2"
  else
    # one of module 3's three workers goes down with the control: one that holds NO send of this job in flight (send_byo.job_events names the worker of every assigned -> sending move), so that what is
    # proven is the platform's handling of a module and a worker that are gone, not module 3's at-least-once window of a worker killed in the middle of a send (NOT CHECKED says so)
    local busy wsvc="" pair
    busy="$(psql_su "select coalesce(string_agg(distinct e.box_id::text, ','), '') from send_byo.job_events e join send_byo.jobs j on j.id = e.job_id where j.campaign_id = '$JOB' and j.state::text in ('assigned', 'sending') and e.box_id is not null")" || die "SJ-O $m: could not read which of module 3's workers holds a send of the job"
    for pair in "m3-worker:$M3_WORKER_ID" "m3-worker-2:$M3_WORKER2_ID" "m3-worker-3:$M3_WORKER3_ID"; do
      case ",$busy," in *",${pair#*:},"*) ;; *) wsvc="${pair%%:*}"; break;; esac
    done
    [ -n "$wsvc" ] || die "SJ-O $m: every one of module 3's three workers holds a send of the job in flight ($busy): one lane per user cannot do that"
    echo "  the job is running with its first results recorded; module 3's control and its worker $wsvc (no send of the job in flight on it) are stopped now (docker compose stop: SIGTERM, a graceful drain)"
    DC stop m3 "$wsvc" >/dev/null 2>&1 || die "SJ-O $m: could not stop module 3 and its worker $wsvc"
  fi
  sj_gone "$svc"
  sj_d6_ok "$m" module_gone_from_the_network
  JOBS_QUIET=1 jobs_scn state --job "$JOB"; jobs_need "SJ-O $m: the state at the stop"
  f="$(jobs_fact state)"; r0="$(jobs_get "$f" results_module)"; c0="$(jobs_get "$f" charges)"; keep="$(jobs_get "$f" pending)"
  echo "  at the stop: $(printf '%s' "$f" | cut -c1-400)"
  [ "$r0" -ge 1 ] && [ "$r0" -lt "$keep" ] || die "SJ-O $m: at the stop the engine had recorded $r0 of $keep results: the stop did not land in the middle of the job (raise SJ_OUTAGE_HOLD_MS)"
  sj_d6_ok "$m" stopped_in_the_middle_of_the_job
  jobs_expect_fail "SJ-O $m control: verify of a send job that is still running" "FAIL job_ended_as_its_path_says" verify --job "$JOB"
  sleep 6   # a send that was in flight at the stop has had longer than the sink's hold to report
  JOBS_QUIET=1 jobs_scn state --job "$JOB"; jobs_need "SJ-O $m: the state after the drain"
  f="$(jobs_fact state)"; r0="$(jobs_get "$f" results_module)"; c0="$(jobs_get "$f" charges)"; s0="$(jobs_get "$f" sink_msgs)"
  i=0; while [ "$i" -lt 5 ]; do
    sleep 5
    JOBS_QUIET=1 jobs_scn state --job "$JOB" --expect "status=running,results_module=$r0,charges=$c0,results_invented=0,releases=0,finished=0,events_running=1,sink_msgs=$s0,sink_max<=1"
    jobs_need "SJ-O $m: during the outage (module ${m#m} down, $(( (i+1) * 5 )) s after the drain)"
    i=$((i+1))
  done
  echo "  for 25 s with module ${m#m} down: the job stayed running, results stayed $r0, charge rows $c0, nothing invented or released, the sink held $s0 message(s) for the job and no more"
  sj_d6_ok "$m" nothing_recorded_charged_or_invented_while_down
  sj_d6_ok "$m" sink_quiet_while_down
  i=0; f=""
  while [ "$i" -lt "$((SJ_STALE_WAIT / 10))" ]; do
    JOBS_QUIET=1 jobs_scn state --job "$JOB"; jobs_need "SJ-O $m: the state while waiting for the stale event"
    f="$(jobs_fact state)"
    [ "$(jobs_get "$f" events_stale)" -ge 1 ] && break
    i=$((i+1)); sleep 10
  done
  [ "$(jobs_get "$f" events_stale)" -ge 1 ] || die "SJ-O $m: no stale event $SJ_STALE_WAIT s into the outage (the senders' limit is one minute in the instances): $(printf '%s' "$f" | cut -c1-400)"
  sj_state "SJ-O $m (stale)" "$JOB" "status=running,results_module=$r0,charges=$c0,results_invented=0,events_stale>0,releases=0,finished=0,sink_msgs=$s0"
  echo "  the engine raised a stale event and the job is STILL running with the same $r0 results: flagged, not failed, expired or finished"
  sj_d6_ok "$m" stale_raised_and_the_job_still_running
  if [ "$m" = m2 ]; then
    DC up -d --no-deps --wait --wait-timeout 240 m2 >/dev/null 2>&1 || die "SJ-O $m: module 2 did not become healthy again"
    m2w_outage_worker
  else
    DC up -d --no-deps --wait --wait-timeout 240 m3 >/dev/null 2>&1 || die "SJ-O $m: module 3 did not become healthy again"
    sj_m3_workers
  fi
  sj_probe "http://$svc:8080/readyz" >/dev/null || die "SJ-O $m: module ${m#m} is healthy by its own check but /readyz does not answer from the private network"
  echo "  module ${m#m} is back: healthy by its own check and /readyz answers from a container on the private network; its workers heartbeat"
  sj_d6_ok "$m" module_and_workers_back
  jobs_wait_terminal "$JOB" 900
  sj_verify "SJ-O $m" "$JOB" normal
  sj_state "SJ-O $m (end)" "$JOB" "status=succeeded,finished=1,events_stale>0,events_running=1,events_completing=1,reserves=$(sj_res_of "$m"),results_invented=0,sink_max=1,sink_foreign=0"
  echo "  the job completed exactly once after the outage: one move to completing, one to succeeded, the reserve released once when it had a remainder (verify), every sent recipient once at the sink"
  sj_d6_ok "$m" completed_exactly_once
  [ -n "$SJ_CONTROL_JOB" ] || die "SJ-O $m: the control job of the same shape (SJ-C) is missing"
  sj_state "SJ-O $m (the control)" "$SJ_CONTROL_JOB" "status=succeeded,events_stale=0,finished=1"
  echo "  the paired control (the same job, no stop: $SJ_CONTROL_JOB) succeeded with no stale event"
  sj_d6_ok "$m" control_without_a_stop_raised_no_stale
}

# ── the infrastructure ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
sj_has() { case " $SJ_MODS " in *" $1 "*) return 0;; esac; return 1; }

sj_infra() {
  STAGE=sendjobs-infra; banner "SJ-0" "the senders of the scope ($SJ_MODS), their workers, the controllable sink, one platform instance with the engine on, the scenario program compiled"
  DC pull jobscenario > "$WORK/sj-pull.log" 2>&1 || { tail -10 "$WORK/sj-pull.log" | sed 's/^/     | /'; die "the runner could not pull the digest-pinned image of the scenario program's container (service jobscenario)"; }
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go build -tags harness -o "$WORK/out/jobscenario" ./tests/harness/jobscenario ) || die "the scenario program (tests/harness/jobscenario) did not compile"
  sj_up jobs-a ""
  if sj_has m2; then
    if [ "$SCOPE" != full ]; then DC up -d --wait --wait-timeout 240 m2 || die "module 2 did not become healthy"; m2w_stage_5c; fi
  fi
  if sj_has m3; then
    if [ "$SCOPE" != full ]; then
      DC up -d --wait --wait-timeout 240 m3 || die "module 3 did not become healthy"
      STAGE=m3-worker; banner "5b" "module 3's three workers (own token, own box row each), the SMTP sink and the controllable sink (stage 5b of the full run, the same function; the probes from jobs-a)"
      M3W_PROBE_SVC=jobs-a m3w_stage_5b
      STAGE=sendjobs-infra
    else
      sj_m3_workers
    fi
  fi
  DC up -d --wait --wait-timeout 180 smtp-ctl >/dev/null 2>&1 || die "the controllable sink (smtp-ctl) is not healthy"
  local mods; mods="$(printf '%s' "$SJ_MODS" | tr ' ' ',')"
  jobs_scn send-prepare --modules "$mods"
  jobs_need "preparing the senders (module 2's dials; module 3's From-gate in enforce against the fixture DNS m3dns; the sink reset)"
  echo "  the senders $SJ_MODS are up with their workers; the sink is reset; the scenario program is compiled; one platform instance (jobs-a) leads"
}

# ── the end: evidence, leak scan, exposure ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
sj_summary() {
  local nm=0 m p name want n c
  STAGE=sendjobs-summary; banner "SJ-S" "what the scenario program verified, the kills that landed and the outage steps"
  for m in $SJ_MODS; do nm=$((nm+1)); done
  [ "$nm" -ge 1 ] || die "the scope names no sender (SJ_MODS is empty): the stage proved nothing"
  want=$(( nm * SJ_RUNS_PER_MODULE ))
  [ "$SJ_VERIFIED" = "$want" ] || die "the stages ran $SJ_VERIFIED verify run(s), want $want ($nm sender(s) x the control, seven kills and the outage): a stage did not reach its verify"
  [ -s "$WORK/sj-ok.log" ] || die "no OK line was kept: the scenario program verified nothing"
  n="$(grep -c ' OK ' "$WORK/sj-ok.log" || true)"
  [ "$n" = "$(( want * ${#SJ_VERIFY_OKS[@]} ))" ] || die "$n OK line(s) were printed by the $SJ_VERIFIED verify runs, want $(( want * ${#SJ_VERIFY_OKS[@]} )) ($want runs x ${#SJ_VERIFY_OKS[@]} invariants)"
  [ -s "$WORK/sj-kill.log" ] || die "no kill was kept: stage SJ-K proved nothing"
  for m in $SJ_MODS; do
    for p in "${SJ_POINTS[@]}"; do
      c="$(grep -cx "SJ KILL $m $p" "$WORK/sj-kill.log" || true)"
      [ "$c" = 1 ] || die "the kill at $p of module ${m#m} is kept $c time(s), want exactly once: the point was not killed (or twice)"
    done
    for name in "${SJ_D6_OKS[@]}"; do
      c="$(grep -cx "SJ D6 OK $m $name" "$WORK/sj-d6.log" 2>/dev/null || true)"
      [ "$c" = 1 ] || die "the outage of module ${m#m} printed 'SJ D6 OK $m $name' $c time(s), want exactly once: the step did not run (or ran twice)"
    done
  done
  [ "$(grep -c '^SJ KILL ' "$WORK/sj-kill.log")" = "$(( nm * ${#SJ_POINTS[@]} ))" ] || die "the kill log holds $(grep -c '^SJ KILL ' "$WORK/sj-kill.log") line(s), want $(( nm * ${#SJ_POINTS[@]} )): a kill this stage does not know"
  [ "$(grep -c '^SJ D6 OK ' "$WORK/sj-d6.log")" = "$(( nm * ${#SJ_D6_OKS[@]} ))" ] || die "the outage log holds $(grep -c '^SJ D6 OK ' "$WORK/sj-d6.log") line(s), want $(( nm * ${#SJ_D6_OKS[@]} )): a step this stage does not know"
  echo "  $n OK lines from $SJ_VERIFIED verify runs ($want x ${#SJ_VERIFY_OKS[@]} invariants), by run:"
  sed 's/ OK .*//' "$WORK/sj-ok.log" | sort | uniq -c | sed 's/^/     /'
  echo "  kills: $(grep -c '^SJ KILL ' "$WORK/sj-kill.log") ($nm sender(s) x ${#SJ_POINTS[@]} points); outage steps: $(grep -c '^SJ D6 OK ' "$WORK/sj-d6.log") ($nm x ${#SJ_D6_OKS[@]})"
}

# the credentials of this run in no log that is kept or printed: the instances (every container, the ones re-created away included), the modules, their workers, the sink and the scenario program's own output
sj_secret_leak() {
  local svc f logs=() secrets=()
  for f in "$JOBS_JWT" "$M2_TOKEN" "$M3_TOKEN" "$M3_ADMIN_KEY" "$SINK_PASS" "${M2_ADMIN_KEY:-}" "${M2_WORKER_TOKEN:-}" "$M3_WORKER_TOKEN" "$M3_WORKER2_TOKEN" "$M3_WORKER3_TOKEN"; do
    [ -n "$f" ] && secrets+=("$f")
  done
  [ "${#secrets[@]}" -ge 6 ] || die "only ${#secrets[@]} credential(s) of this run are known to the leak scan: it would look for almost nothing"
  for svc in jobs-a smtp-ctl $(sj_has m2 && echo m2 m2-worker) $(sj_has m3 && echo m3 m3-worker m3-worker-2 m3-worker-3); do
    DC logs --no-color "$svc" > "$WORK/sjleak-$svc.log" 2>&1 || die "could not read the logs of $svc"
    case "$svc" in jobs-a|m2|m3) [ -s "$WORK/sjleak-$svc.log" ] || die "the log of $svc, which the leak check must read, is empty: the instrument matched nothing";; esac
    logs+=("$WORK/sjleak-$svc.log")
  done
  for f in "$WORK"/sj-kept-*.log "$WORK"/jobs-kill-*.log "$WORK/jobs-run.log"; do [ -s "$f" ] && logs+=("$f"); done
  for f in "${logs[@]}"; do
    if grep -qFf <(printf '%s\n' "${secrets[@]}") "$f"; then rm -f "$WORK"/sjleak-*.log; die "a credential of this run appears in $(basename "$f") (the log is not shown: it is public)"; fi
    if grep -qE 'eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}' "$f"; then rm -f "$WORK"/sjleak-*.log; die "something shaped like a JWT (an access token) appears in $(basename "$f") (the log is not shown: it is public)"; fi
  done
  rm -f "$WORK"/sjleak-*.log
  echo "  none of the ${#secrets[@]} credentials of this run, nor anything shaped like a JWT, appears in ${#logs[@]} log(s): the instance (now and every container re-created away), the modules, their workers, the sink and the scenario program's output"
}

sj_exposure() {
  STAGE=exposure; banner 8 "scope $SCOPE: no service publishes a port; the network is internal"
  local ids id pb cnt=0
  DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres jobs-a smtp-ctl $(sj_has m2 && echo m2 m2-worker) $(sj_has m3 && echo m3 m3-worker m3-worker-2 m3-worker-3 smtp-sink m3dns) || die "a service is reachable from outside the network"
  [ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
  ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
  for id in $ids; do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"
}

# ── the whole stage, and the scope ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
sj_run() {
  local m
  : > "$WORK/sj-ok.log"; : > "$WORK/sj-kill.log"; : > "$WORK/sj-d6.log"; [ -f "$WORK/jobs-run.log" ] || : > "$WORK/jobs-run.log"
  # in a full run the legacy platform containers (engine OFF) would win the leader lock and hold every job where it is: they stop here
  if [ "$SCOPE" = full ]; then DC stop -t 10 platform platform-2 >/dev/null 2>&1 || true; fi
  sj_infra
  for m in $SJ_MODS; do
    sj_control "$m"
    sj_kills "$m"
    sj_outage "$m"
  done
  sj_summary
  sj_secret_leak
  sj_exposure
}

# H_SCOPE=sendjobs | sendjobs-m2 | sendjobs-m3: Postgres, the four migration chains, the install step after module 1's and the first admin (stages 1-4b ran), then the stage and nothing else. It never returns.
sj_scope_run() {
  STAGE=services; banner 5 "scope $SCOPE: the senders $SJ_MODS with their workers, the controllable sink and the platform instance jobs-a with the job engine on (module 1 is not started)"
  sj_run
  STAGE=done
  echo
  echo "================ NOT CHECKED (scope $SCOPE: what this run did not and could not vouch for)"
  sj_not_checked
  echo "  - everything outside the senders' job engine: module 1, the probe, the outage and leader stages of the full run and the ledger check are NOT run in this scope (H_SCOPE=full runs them)"
  echo
  echo "HARNESS RESULT: GREEN (scope $SCOPE: $SJ_VERIFIED verified send jobs, $(grep -c ' OK ' "$WORK/sj-ok.log" || true) invariant lines, $(grep -c '^SJ KILL ' "$WORK/sj-kill.log") kills, $(grep -c '^SJ D6 OK ' "$WORK/sj-d6.log") outage steps) — the platform's own process was killed at seven points of a send job and the senders were stopped mid-job: nothing lost, nothing sent twice at the sink, nothing charged twice"
  exit 0
}

sj_not_checked() {
  cat <<'EOF'
  - the TWO-INSTANCE leader kill is NOT repeated for the senders (it is proved for module 1 by the scope jobs, J3); every kill here runs ONE instance (jobs-b is stopped), so a kill hands over to a restart, never to a standby.
  - the kill points are the seven of SJ_POINTS (send-stage.sh), each hit by the real process killing ITSELF; start.before_running is killed for module 1 only (scope jobs). A kill at any other instant, a kill of a module
    together with the platform, two kills in a row in one job and an out-of-memory kill are not exercised. cancel.after_module is hit after every return of the module's cancel (success or not): the mid-state asserts
    the module's items cancelled, so a first cancel that failed is red, never a vacuous green.
  - the sink is the controllable sink (tests/harness/sink) for both senders: one job's recipients, held 2 s (3 s for the outage) each, two of them rejected 550 at RCPT TO. Not exercised: deferrals (4xx) and the
    attempt cap, a sink that drops the connection, more than one lane of a user, a real provider. Mailpit (smtp-sink) runs for module 3's stage 5b but no account of this stage names it.
  - the outages stop the module's control with `docker compose stop` (SIGTERM, a graceful drain); module 3 also loses one of its three workers, one that holds no send of the job in flight (a worker killed in the
    middle of a send, module 3's at-least-once window, is not exercised). A SIGKILL of a module, a module that answers 500 or garbage, a slow module
    and a network partition are not exercised; module 2's worker exits after its own reconnect budget and the stage starts it again (m2w_outage_worker), as module 2's design says the orchestrator does.
  - the modules' dials are the scope's: module 2's retry ladder, attempt cap, pacing and dispatch tick are shortened (send-prepare), module 3 sends on one lane with at most a second between sends, the senders'
    staleness limit is one minute in the instances. A list of more than twelve lines and a job that runs for hours are not exercised.
EOF
  echo "NOT CHECKED: the two-instance leader kill for the senders (module 1 only), a kill at any instant but the seven points, a SIGKILL or a slow module, deferrals at the sink, more than one lane, a list of more than twelve lines"
}
