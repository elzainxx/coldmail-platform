#!/usr/bin/env bash
# stage.sh — the crash, handover and outage proofs of the job engine, against the REAL service process, a REAL module 1 and REAL kills (Phase 4, chunk B17a; Docs/phase-4-spec.md "The loops" and the
# first three exit criteria). SOURCED by tests/harness/run.sh (it only defines functions; it runs nothing by itself) and it uses what run.sh defines: DC, die, banner, mask, STAGE, WORK, HERE, PLATFORM_DIR,
# PROJECT_NET, SCOPE.
#
# DORMANT BY DEFAULT. run.sh calls these functions only when JOBS_ON=1: under H_SCOPE=jobs, or in the full run once `jobs` is named in FULL_INCLUDES (run.sh, one place; the lead adds the name after a runner run
# of the scope is green). With JOBS_ON=0 nothing here runs, the compose override docker-compose.jobs.yml is not merged, no secret is minted and no container exists.
#
# WHAT IS PROVEN, and how each proof is made unable to pass on a run that did not do the thing (every stage prints evidence lines; the run is RED at the first failed assertion):
#   J1 baseline   two instances, engine on, NO fault armed: a job of 60 lines (four malformed, three repeats) runs to `succeeded` and `verify` holds (the control of every later stage). The scenario program's
#                 own controls: `verify` on a job that does not exist and on a job that was never confirmed must FAIL.
#   J2 kills      for each of the five fault points the platform's OWN process is killed by itself at that point (exit status 137, OOMKilled false, the log line of the armed build), the database is read AT THAT
#                 MOMENT and must be the mid-state the point names (a kill that landed at the end would prove nothing), the instance is re-created without the fault, the confirm is replayed, the job runs to the
#                 end and `verify` holds: no row lost, none duplicated, none charged twice, the three drift views empty, module 1 holds the job once, every transition once.
#   J3 two        both instances up: exactly one holds the leader lock and ONLY it logs engine activity for the job (the standby's API serves the request); the leader is SIGKILLed while the job runs, the other takes
#                 the lock within 60 s and finishes it exactly once; the killed instance comes back as a standby, and a second job runs on the survivor with no engine line in the standby's log.
#   J4 outage     module 1 is STOPPED while the job runs: for 25 s the job stays running, nothing is recorded, nothing is charged, nothing is invented; after the staleness limit a `stale` event exists and the job is
#                 still running; module 1 is started again and the job completes exactly once.
#   J6 vendor     module 1's vendor fallback is switched OFF, ON and OFF again by RE-CREATING its container with the four dials and the key of the compose overlay set (jobs_m1j_vendor; the key is a value minted per
#                 run, never printed, and the harness network is internal), and each time what the platform does is read from four places that cannot agree by accident: module 1's own describe, the platform's stored
#                 flag and its history, the preview the API answers and the ledger and price snapshot of a SCHEDULED confirm (an hour ahead, so the engine never starts it) and of its cancel. OFF: a preview shows 1
#                 credit per pending row, the confirm reserves 1 per row and the cancel releases it; ON: the next preview shows 2 per row (and a preview made under OFF is refused 409 preview_stale at the confirm),
#                 the confirm reserves 2 per row, frozen into the job's price snapshot with the vendor unit; OFF again: 1 per row, and the job confirmed under ON keeps its reserve and its snapshot. Last, module 1 is
#                 STOPPED: a preview says it was not read (vendor_reachable_known false) at the rate of the stored flag, and says it is read again once module 1 is back. Every assertion prints `J6 OK <name>`
#                 (JOBS_J6_OKS), each exactly once, and the summary is red without them.
#   evidence      every `verify` prints one `OK <name>` line per invariant (JOBS_VERIFY_OKS); a run that prints fewer, more or other lines is RED, and so is a run with fewer `verify` runs than the stages make; J6's
#                 `J6 OK` lines are counted the same way.
#
# What is here, in the order run.sh calls it:
#   jobs_mint            stage 2: the JWT secret the instances verify access tokens with, the secret module 1's per-user keys are derived from and the stand-in vendor key of stage J6, minted with openssl, masked, appended mode 600 to the env file
#   jobs_scope_run       H_SCOPE=jobs: Postgres + migrations + the first admin (stages 1-4b ran), then J0-J4, J6 and the checks below, then the end of the run: it never returns
#   jobs_run             the stages J0-J4 and J6 and the summary (what a full run calls once `jobs` is in FULL_INCLUDES)
# Nothing here prints a credential: no `set -x`, no environment dump; the scenario program prints neither a secret nor a token.

JOBS_JWT=""; JOBS_M1K=""; JOBS_VENDOR=""     # set by jobs_mint (only the shell's copies: the leak scan looks for them)
JOBS_API_A="http://jobs-a:8080"; JOBS_API_B="http://jobs-b:8080"
JOBS_POINTS=(confirm.before_commit submit.after_module start.before_running settle.after_commit release.mid_tx)
# the lines `verify` must print, one each (tests/harness/jobscenario/verify.go, verifyOKs: the selftest holds the two lists equal)
JOBS_VERIFY_OKS=(
  rows_equal_results
  every_pending_row_has_one_result
  results_name_their_rows
  job_succeeded
  one_charge_per_item
  charges_equal_results_with_a_unit
  settlement_drift_empty
  balance_drift_empty
  reserve_closed
  one_release_per_reserve
  module_report_covers_the_pending_rows
  module_report_equals_results
  module_report_equals_ledger
  module_holds_the_job_once
  module_item_count_equals_pending
  events_legal_and_once_each
  proof_not_vacuous
  control_checks_detect_corruption
)
JOBS_VERIFY_RUNS_WANTED=9                    # J1 one, J2 five (one per point), J3 two, J4 one (J6 verifies no job: nothing it confirms is ever started)
JOBS_VERIFIED=0
# the lines stage J6 must print, one each, as `J6 OK <name>` (jobs_j6_ok; the selftest holds the list equal to the calls of jobs_j6)
JOBS_J6_OKS=(
  platform_registered_its_service_caller
  module_says_off_at_the_start
  off_preview_is_1_per_row_and_read_from_module_1
  off_confirm_reserves_1_per_row_without_the_vendor_unit
  off_cancel_releases_the_whole_reserve
  module_says_on_after_the_flip
  a_preview_made_under_off_is_stale_at_the_confirm
  the_flag_follows_module_1_and_says_whose_word_it_is
  on_preview_is_2_per_row_at_the_vendor_unit
  on_confirm_reserves_2_per_row_with_the_vendor_unit_frozen
  module_says_off_again
  off_again_preview_is_1_per_row
  off_again_confirm_reserves_1_per_row
  a_job_confirmed_under_on_is_not_changed_by_the_later_off
  a_module_that_cannot_be_read_is_said_not_to_be_read
  a_module_that_is_back_is_read_again
)
JOBS_J6_ROWS="${JOBS_J6_ROWS:-40}"           # J6: a list of 40 lines, 2 malformed and 2 repeats: 36 pending rows, so 1 credit a row is 36 and 2 a row is 72
JOBS_VENDOR_MEMORY_SEC=5                     # how long the platform keeps module 1's answer (internal/jobs DefaultVendorReadTTL: the selftest holds the two equal); J6 waits longer than this after every flip
JOBS_OUT=""; JOBS_ERR=""; JOBS_RC=0
# the size of the lists (a job of N lines has N-bad-dup addresses module 1 is asked about): small for the points whose kill needs no timing, large where the proof needs module 1 to still be working
JOBS_SMALL="${JOBS_SMALL:-60}"               # J1 and every point but settle.after_commit
JOBS_SETTLE_ROWS="${JOBS_SETTLE_ROWS:-1500}" # settle.after_commit: more rows than module 1 settles between two passes of the engine
JOBS_SETTLE_NTH="${JOBS_SETTLE_NTH:-2}"      # the kill lands in the SECOND recording of results
JOBS_J3_ROWS="${JOBS_J3_ROWS:-1500}"
JOBS_OUTAGE_ROWS="${JOBS_OUTAGE_ROWS:-1500}"
JOBS_LOCK_KEY=7200000002                     # the engine's leader lock (the entitlement loops' too): the one stage 9c reads
JOBS_LOCKWHERE="l.locktype = 'advisory' and l.granted and l.objsubid = 1 and l.database = (select d.oid from pg_database d where d.datname = current_database()) and ((l.classid::bigint << 32) | l.objid::bigint) = $JOBS_LOCK_KEY"

jobs_psql_su() { DC exec -T postgres psql -X -U supabase_admin -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }
jobs_holders_sql() { jobs_psql_su "select coalesce(string_agg(a.application_name, ',' order by a.application_name), '') from pg_locks l join pg_stat_activity a on a.pid = l.pid where $JOBS_LOCKWHERE"; }
jobs_nlocks_sql() { jobs_psql_su "select count(*) from pg_locks l where $JOBS_LOCKWHERE"; }
jobs_cid() { DC ps -a -q "$1" 2>/dev/null | head -1; }
jobs_sleep() { sleep "${JOBS_SLEEP:-1}"; }

# ── stage 2: secrets ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Called right AFTER run.sh wrote $WORK/harness.env: it mints, masks, then appends its three lines to the same file (written under umask 077, so mode 600, checked where the host keeps modes). Each is 32 random
# bytes in hex (64 characters: the platform asks for at least 32 bytes of printable ASCII for the first and at least 32 bytes for the second; the third is the stand-in API key of module 1's vendor fallback,
# which module 1 only tests for presence and which the stage hands to the compose file for the one command that switches the vendor ON). None is ever printed, and the leak scan looks for all three.
jobs_mint() {
  local a b c m
  a="$(openssl rand -hex 32)"; b="$(openssl rand -hex 32)"; c="$(openssl rand -hex 32)"
  [ "${#a}" = 64 ] && [ "${#b}" = 64 ] && [ "${#c}" = 64 ] || die "could not mint the jobs scope's three secrets (openssl rand printed ${#a}, ${#b} and ${#c} characters, want 64, 64 and 64)"
  [ "$a" != "$b" ] || die "the jobs scope's two secrets are the same value: the JWT secret and the key-derivation secret must be different credentials"
  [ "$a" != "$c" ] && [ "$b" != "$c" ] || die "the jobs scope's vendor key equals another of its secrets: the three must be different credentials"
  mask "$a"; mask "$b"; mask "$c"                                       # masked BEFORE they are written anywhere or handed to any command
  [ -f "$WORK/harness.env" ] || die "the env file is missing: jobs_mint runs after run.sh wrote it"
  if [ "$(uname -s)" = Linux ]; then
    m="$(stat -c '%a' "$WORK/harness.env" 2>/dev/null)"
    [ "$m" = 600 ] || die "the env file $WORK/harness.env is mode ${m:-unknown}, expected 600: it holds this run's secrets"
  fi
  ( umask 077; { echo "H_JOBS_JWT_SECRET=$a"; echo "H_JOBS_M1_KEY_SECRET=$b"; echo "H_JOBS_VENDOR_KEY=$c"; } >> "$WORK/harness.env" ) || die "could not append the jobs scope's secrets to the env file"
  JOBS_JWT="$a"; JOBS_M1K="$b"; JOBS_VENDOR="$c"
  echo "  minted for the jobs scope: 1 JWT secret (HS256, 32 random bytes in hex), 1 module-1 key-derivation secret and 1 stand-in vendor key (32 random bytes in hex each) — masked, none printed"
}

# ── the scenario program ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# jobs_scn <args...>: runs the scenario program (a one-off container of the compose service jobscenario, on the private network) and sets JOBS_OUT (its stdout: the facts and the OK lines), JOBS_ERR (its stderr: the
# reason of a failure) and JOBS_RC. Both are printed, indented, unless JOBS_QUIET=1 (a polling loop); everything printed is also kept in $WORK/jobs-run.log. It returns the program's exit status.
jobs_scn() {
  local errf="$WORK/jobs-scn-err.$$"
  JOBS_OUT="$(DC run --rm -T --no-deps jobscenario "$@" 2>"$errf")"; JOBS_RC=$?
  JOBS_ERR="$(cat "$errf" 2>/dev/null)"; rm -f "$errf"
  { echo "  \$ jobscenario $*"; [ -z "$JOBS_OUT" ] || printf '%s\n' "$JOBS_OUT" | sed 's/^/     | /'; [ -z "$JOBS_ERR" ] || printf '%s\n' "$JOBS_ERR" | sed 's/^/     ! /'; echo "     (exit $JOBS_RC)"; } > "$WORK/jobs-scn-last.txt"
  cat "$WORK/jobs-scn-last.txt" >> "$WORK/jobs-run.log"
  [ "${JOBS_QUIET:-0}" = 1 ] || cat "$WORK/jobs-scn-last.txt"
  return "$JOBS_RC"
}

# jobs_fact <name>: the first fact line of that name in JOBS_OUT. jobs_get <line> <member>: one member of a fact line, string or number, without its quotes.
jobs_fact() { printf '%s\n' "$JOBS_OUT" | grep -m1 "^{\"fact\":\"$1\"" || true; }
jobs_get() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\{0,1\}\([^\",}]*\).*/\1/p" | head -1; }

# jobs_need <what>: the last jobs_scn must have succeeded, else the run is red naming what it was doing and the program's own reason
jobs_need() { [ "$JOBS_RC" = 0 ] || die "$1: the scenario program exited $JOBS_RC: $(printf '%s' "$JOBS_ERR" | tail -3 | tr '\n' ' ')"; }

# jobs_expect_fail <label> <substring of its stderr> <args...>: the program MUST fail, and for the named reason (a control that failed for another reason proves nothing)
jobs_expect_fail() {
  local label="$1" want="$2"; shift 2
  jobs_scn "$@"
  [ "$JOBS_RC" != 0 ] || die "$label: the scenario program exited 0 where it must fail ($*): a verifier that cannot go red proves nothing"
  printf '%s\n' "$JOBS_ERR" | grep -qF -- "$want" || die "$label: the scenario program failed (exit $JOBS_RC) but not for the reason named ('$want'): $(printf '%s' "$JOBS_ERR" | tail -3 | tr '\n' ' ')"
  echo "  control holds: $label -> exit $JOBS_RC, '$want'"
}

# jobs_expect_oks <label>: JOBS_OUT (the output of a `verify`) holds each `OK <name>` line of JOBS_VERIFY_OKS exactly once and no other OK line. A program that printed nothing is red.
jobs_expect_oks() {
  local label="$1" name c total
  for name in "${JOBS_VERIFY_OKS[@]}"; do
    c="$(printf '%s\n' "$JOBS_OUT" | grep -cx "OK $name" || true)"
    [ "$c" = 1 ] || die "$label: the scenario program printed 'OK $name' $c time(s), want exactly once: the check did not run (or ran twice)"
  done
  total="$(printf '%s\n' "$JOBS_OUT" | grep -c '^OK ' || true)"
  [ "$total" = "${#JOBS_VERIFY_OKS[@]}" ] || die "$label: the scenario program printed $total OK line(s), want exactly ${#JOBS_VERIFY_OKS[@]}: it printed a line this stage does not know"
  printf '%s\n' "$JOBS_OUT" | grep '^OK ' | sed "s/^/$label /" >> "$WORK/jobs-ok.log"
}

# jobs_verify <label> <job>: the invariants of a finished job, every one printed and every one held
jobs_verify() {
  local label="$1" job="$2" f
  jobs_scn verify --job "$job"
  jobs_need "$label: verify of job $job"
  jobs_expect_oks "$label"
  JOBS_VERIFIED=$((JOBS_VERIFIED+1))
  f="$(jobs_fact verify)"
  echo "  $label: job $job holds ${#JOBS_VERIFY_OKS[@]} of ${#JOBS_VERIFY_OKS[@]} invariants: rows $(jobs_get "$f" rows), pending $(jobs_get "$f" pending), charged $(jobs_get "$f" charged), free by module $(jobs_get "$f" free_module), unmapped $(jobs_get "$f" unmapped), charge rows $(jobs_get "$f" charge_rows), events $(jobs_get "$f" events)"
  echo "     module 1 answered: $(jobs_get "$f" answers)"
}

# jobs_list <api> <rows> <bad> <dup>: creates a user and a draft, previews it (no confirm); sets JOB, PENDING from the fact
jobs_create() {
  local api="$1" rows="$2" bad="$3" dup="$4" f
  jobs_scn create --api "$api" --rows "$rows" --bad "$bad" --dup "$dup"
  jobs_need "creating a job of $rows lines"
  f="$(jobs_fact created)"; JOB="$(jobs_get "$f" job)"; PENDING="$(jobs_get "$f" pending)"
  [ -n "$JOB" ] && [ -n "$PENDING" ] || die "the scenario program's 'created' fact has no job or no pending count: $f"
}

# jobs_wait_terminal <job> <timeout seconds>: the job reaches a terminal status (the verdict on which one is verify's)
jobs_wait_terminal() {
  JOBS_QUIET=1 jobs_scn wait --job "$1" --until terminal --timeout "$2"
  if [ "$JOBS_RC" != 0 ]; then
    cat "$WORK/jobs-scn-last.txt"
    JOBS_QUIET=0 jobs_scn state --job "$1" || true
    die "job $1 did not reach a terminal status within ${2} s: $(printf '%s' "$JOBS_ERR" | tail -2 | tr '\n' ' ') (the engine did not run it to its end: see the instances' logs below)"
  fi
  echo "  job $1 is $(jobs_get "$(jobs_fact waited)" status) after $(jobs_get "$(jobs_fact waited)" seconds) s"
}

# ── the instances ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# jobs_up <jobs-a|jobs-b> [fault]: (re-)creates the instance with the fault armed (a point name, or name:n; empty for none) and waits until it is healthy. The variable is set for this one command only, so a
# later `up` without it creates the instance unarmed. --no-deps: the other services are the stage's to start and stop.
jobs_up() {
  local svc="$1" fault="${2:-}" var
  case "$svc" in jobs-a) var=H_JOBS_A_FAULT;; jobs-b) var=H_JOBS_B_FAULT;; *) die "jobs_up: $svc is not a platform instance of this scope";; esac
  ( export "$var=$fault"; DC up -d --no-deps --force-recreate --wait --wait-timeout 180 "$svc" ) > "$WORK/jobs-up-$svc.log" 2>&1 || { sed 's/^/     | /' "$WORK/jobs-up-$svc.log" | tail -20; die "$svc did not become healthy${fault:+ with the fault $fault armed}"; }
  if [ -n "$fault" ]; then echo "  $svc is up and healthy, the fault $fault armed"; else echo "  $svc is up and healthy, no fault armed"; fi
}

# jobs_wait_exited <service> <seconds>: the container leaves the running state (a kill: the process did it to itself)
jobs_wait_exited() {
  local i=0 n=$(( $2 * 2 )) st
  while [ "$i" -lt "$n" ]; do
    st="$(docker inspect -f '{{.State.Status}}' "$(jobs_cid "$1")" 2>/dev/null)"
    [ "$st" = exited ] && return 0
    i=$((i+1)); sleep 0.5
  done
  return 1
}

# jobs_prove_killed <service> <point> <nth>: the process was killed BY THE FAULT POINT and by nothing else: the container exited, with status 137 (128 + SIGKILL), the kernel's out-of-memory killer did not do it, and the
# armed build's own log says it killed itself at that point, exactly once. The log is kept in $WORK before anything re-creates the container.
jobs_prove_killed() {
  local svc="$1" point="$2" nth="$3" cid st n
  cid="$(jobs_cid "$svc")"; [ -n "$cid" ] || die "$svc has no container: nothing to read the exit status of"
  st="$(docker inspect -f '{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}' "$cid" 2>/dev/null)"
  DC logs --no-color "$svc" > "$WORK/jobs-kill-$svc-$point.log" 2>&1 || die "could not read the log of $svc"
  case "$st" in
    "exited 137 false") ;;
    "exited 137 true")  die "$svc exited 137 but the kernel's OOM killer did it (OOMKilled true): this is not the fault point's kill";;
    *)                  die "$svc is '$st' (status, exit code, OOMKilled), want 'exited 137 false': the process was not SIGKILLed by $point";;
  esac
  grep -qF "faultpoint: \"$point\" hit $nth: killing the process" "$WORK/jobs-kill-$svc-$point.log" || die "the log of $svc has no 'faultpoint: \"$point\" hit $nth: killing the process' line: it was killed by something else, or by another point ($(grep -c 'killing the process' "$WORK/jobs-kill-$svc-$point.log" || true) such line(s))"
  n="$(grep -c 'killing the process' "$WORK/jobs-kill-$svc-$point.log" || true)"
  [ "$n" = 1 ] || die "the log of $svc says it killed itself $n times, want exactly once"
  echo "  killed by the fault point: $svc is '$st' (status, exit code, OOMKilled) and its log says: $(grep -F 'killing the process' "$WORK/jobs-kill-$svc-$point.log" | head -1 | sed 's/^.*| //' | cut -c1-120)"
}

# jobs_lines <service> <job>: how many lines of the service's log say the ENGINE did something for the job (a move it made, the results it recorded, a step the lifecycle refused it). The API's own lines (a draft,
# a confirm) are not engine activity.
jobs_lines() {
  DC logs --no-color "$1" 2>&1 | grep -F "\"job\":\"$2\"" | grep -cE '"msg":"(job moved by the engine|results recorded|engine step refused)"' || true
}

# ── J0: module 1 and the fixture DNS ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_ip() {   # <service>: its address on the harness network
  docker inspect -f "{{(index .NetworkSettings.Networks \"$PROJECT_NET\").IPAddress}}" "$(jobs_cid "$1")" 2>/dev/null
}

jobs_infra() {
  STAGE=jobs-infra; banner J0 "the fixture DNS, module 1 asking it (its resolv.conf written from the fixture's address), the scenario program compiled"
  local ip got
  mkdir -p "$WORK/jobs"
  DC pull jobscenario > "$WORK/jobs-pull.log" 2>&1 || { tail -10 "$WORK/jobs-pull.log" | sed 's/^/     | /'; die "the runner could not pull the digest-pinned image of the scenario program's container (service jobscenario, docker-compose.jobs.yml): the pin is not changed from the runner's side, report it"; }
  DC up -d --no-deps --wait --wait-timeout 180 jobsdns || die "the fixture DNS (service jobsdns: tests/harness/fx with its dns and api roles, tests/harness/jobs/dns.json) did not become healthy"
  ip="$(jobs_ip jobsdns)"
  case "$ip" in [0-9]*.[0-9]*.[0-9]*.[0-9]*) ;; *) die "could not read the address of jobsdns on $PROJECT_NET (got '$ip')";; esac
  printf 'nameserver %s\noptions timeout:2 attempts:1\n' "$ip" > "$WORK/jobs/resolv.conf"; chmod 644 "$WORK/jobs/resolv.conf"
  DC up -d --no-deps --wait --wait-timeout 240 m1j || die "module 1 (service m1j) did not become healthy asking the fixture DNS (it reaches its database through the fixture's forwarder)"
  got="$(DC exec -T m1j cat /etc/resolv.conf 2>&1)"
  printf '%s\n' "$got" | grep -qx "nameserver $ip" || die "module 1's resolv.conf does not name the fixture ($ip) as its nameserver: the bind mount over /etc/resolv.conf did not take, and its answers would be the runner's (resolv.conf: $(printf '%s' "$got" | tr '\n' ' '))"
  ( cd "$PLATFORM_DIR" && CGO_ENABLED=0 GOTOOLCHAIN=local go build -tags harness -o "$WORK/out/jobscenario" ./tests/harness/jobscenario ) || die "the scenario program (tests/harness/jobscenario) did not compile"
  [ -x "$WORK/out/jobscenario" ] || die "the scenario program was built but is not executable"
  echo "  fixture DNS $ip answers; module 1 (m1j) is healthy and its resolv.conf is the fixture's; the scenario program is compiled (tag harness)"
}

# ── J1: the baseline and the program's own controls ─────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_j1() {
  STAGE=jobs-baseline; banner J1 "two instances, the engine on, no fault: a job of $JOBS_SMALL lines runs to the end and every invariant holds; the verifier fails where it must"
  local f
  jobs_up jobs-a ""; jobs_up jobs-b ""
  jobs_scn setup --api "$JOBS_API_A" --rows "$JOBS_SMALL" --bad 4 --dup 3
  jobs_need "J1 setup"
  f="$(jobs_fact created)"; JOB="$(jobs_get "$f" job)"; PENDING="$(jobs_get "$f" pending)"
  [ -n "$JOB" ] && [ "$PENDING" = "$((JOBS_SMALL - 7))" ] || die "J1: the job has $PENDING pending rows, want $((JOBS_SMALL - 7))"
  f="$(jobs_fact confirm)"; [ "$(jobs_get "$f" outcome)" = ok ] && [ "$(jobs_get "$f" replayed)" = false ] || die "J1: the confirm was not a first, accepted confirm: $f"
  jobs_wait_terminal "$JOB" 300
  jobs_verify "J1" "$JOB"
  # the controls of the program itself: it fails on a job that does not exist and on one that was never confirmed (it holds no results, no module job, no succeeded status)
  jobs_expect_fail "J1 control: verify of a job that does not exist" "does not exist in platform.jobs" verify --job "00000000-0000-4000-8000-00000000dead"
  jobs_create "$JOBS_API_A" 5 0 0
  jobs_expect_fail "J1 control: verify of a job that was previewed and never confirmed" "FAIL job_succeeded" verify --job "$JOB"
  jobs_expect_fail "J1 control: ... and it names the module's missing job" "FAIL module_holds_the_job_once" verify --job "$JOB"
}

# ── J2: kills at the five points ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# jobs_kill_point <point> <nth or ""> <rows> <bad> <dup> <what the database must hold at the kill>
jobs_kill_point() {
  local point="$1" nth="$2" rows="$3" bad="$4" dup="$5" mid="$6" spec f replay reserved
  spec="$point"; [ -z "$nth" ] || spec="$point:$nth"
  STAGE="jobs-kill-$point"; banner "J2" "kill the platform at $spec: the process kills ITSELF there; the database is read at that moment; the instance is started without the fault and the job must end exactly as if nothing had happened"
  DC stop -t 10 jobs-b >/dev/null 2>&1 || true                            # ONLY jobs-a: the survivor of a kill must be the restarted instance, not a second one that was already there
  jobs_up jobs-a "$spec"
  if [ "$point" = confirm.before_commit ]; then
    jobs_create "$JOBS_API_A" "$rows" "$bad" "$dup"
    echo "  the draft is previewed (job $JOB, $PENDING pending); the confirm is sent and the process dies INSIDE the request, with the transaction open"
    jobs_scn confirm --api "$JOBS_API_A" --job "$JOB" --expect no-response
    jobs_need "J2 $point: the confirm was meant to get no answer"
  else
    jobs_scn setup --api "$JOBS_API_A" --rows "$rows" --bad "$bad" --dup "$dup"
    jobs_need "J2 $point: setup"
    f="$(jobs_fact created)"; JOB="$(jobs_get "$f" job)"; PENDING="$(jobs_get "$f" pending)"
    [ -n "$JOB" ] || die "J2 $point: no job in the 'created' fact: $f"
  fi
  jobs_wait_exited jobs-a 150 || { JOBS_QUIET=0 jobs_scn state --job "$JOB" || true; die "J2 $point: jobs-a is still running 150 s after the job was confirmed: the fault point $spec was never hit (the job is above), or the armed variable did not reach the process"; }
  jobs_prove_killed jobs-a "$point" "${nth:-1}"
  # the evidence that the kill landed MID-job: the database as the dead process left it (nothing writes it now; module 1 works on, which changes none of these numbers)
  jobs_scn state --job "$JOB" --expect "$mid"
  jobs_need "J2 $point: the database is not in the mid-state this point names ($mid)"
  echo "  mid-state holds at $spec: $mid"
  # the instance comes back with no fault and the confirm is sent again: a confirm that never committed is made now, once; one that had committed is a replay that writes nothing
  jobs_up jobs-a ""
  jobs_scn confirm-retry --api "$JOBS_API_A" --job "$JOB"
  jobs_need "J2 $point: confirm-retry"
  f="$(jobs_fact confirm)"; replay="$(jobs_get "$f" replayed)"; reserved="$(jobs_get "$f" reserved)"
  if [ "$point" = confirm.before_commit ]; then
    [ "$replay" = false ] && [ "$reserved" = "$PENDING" ] || die "J2 $point: the retried confirm should be the FIRST confirm (replayed false, reserved $PENDING credits) because the transaction rolled back, got: $f"
    echo "  the retried confirm is the first confirm of the job: it held $reserved credits, once"
  else
    [ "$replay" = true ] && [ "$reserved" = "$PENDING" ] || die "J2 $point: the retried confirm should be a REPLAY of the one that committed (replayed true, the same $PENDING credits), got: $f"
    echo "  the retried confirm is a replay (the same key): it wrote nothing and reports the $reserved credits the first confirm held"
  fi
  jobs_wait_terminal "$JOB" 600
  jobs_verify "J2 $point" "$JOB"
}

jobs_j2() {
  jobs_kill_point confirm.before_commit ""                 "$JOBS_SMALL" 4 3 "status=previewed,confirmed=0,confirm_key_set=0,reserves=0,charges=0,results=0,balance_reserved=0,module_jobs=0,rows>0"
  jobs_kill_point submit.after_module   ""                 "$JOBS_SMALL" 4 3 "status=confirmed,started=0,module_ref=,stamped=0,events_running=0,reserves=1,results_module=0,releases=0,finished=0,module_jobs=1"
  jobs_kill_point start.before_running  ""                 "$JOBS_SMALL" 4 3 "status=confirmed,started=0,module_ref=,stamped=0,events_running=0,reserves=1,results_module=0,releases=0,finished=0,module_jobs=1"
  jobs_kill_point settle.after_commit   "$JOBS_SETTLE_NTH" "$JOBS_SETTLE_ROWS" 10 10 "status=running,started=1,finished=0,events_running=1,stamped=@pending,results_module>0,results_module<@pending,releases=0,reserve_released=0,reserves=1,module_jobs=1"
  jobs_kill_point release.mid_tx        ""                 "$JOBS_SMALL" 4 3 "status=completing,started=1,finished=0,events_running=1,results=@rows,results_module=@pending,releases=0,reserve_released=0,reserves=1,module_jobs=1"
}

# ── J3: two instances ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_j3() {
  STAGE=jobs-two; banner J3 "two instances: only the leader runs the engine; the leader is SIGKILLed while a job runs; the other takes over and finishes it once; the killed one returns as a standby"
  local i lead other lsvc job1 job2 l0 o0 lk l1 o1 t0 f
  jobs_up jobs-a ""; jobs_up jobs-b ""
  # (a) exactly one lock, held by a session named after one of the instances
  i=0; lead=""
  while [ "$i" -lt 90 ]; do
    if [ "$(jobs_nlocks_sql)" = 1 ]; then lead="$(jobs_holders_sql)"; case "$lead" in jobs-a|jobs-b) break;; esac; lead=""; fi
    i=$((i+1)); sleep 1
  done
  [ -n "$lead" ] || die "J3: after 90 s there is not exactly one granted lock on the leader key held by a session named jobs-a or jobs-b: locks=$(jobs_nlocks_sql) holders='$(jobs_holders_sql)'"
  JOBS_QUIET=0 jobs_scn leader; jobs_need "J3 leader"
  [ "$(jobs_get "$(jobs_fact leader)" holders)" = "$lead" ] && [ "$(jobs_get "$(jobs_fact leader)" locks)" = 1 ] || die "J3: the scenario program's reading of the leader lock ($(jobs_fact leader)) is not the server's ('$lead', 1 lock)"
  case "$lead" in jobs-a) other=jobs-b;; *) other=jobs-a;; esac
  echo "  a. exactly one granted lock on key $JOBS_LOCK_KEY, held by a session named '$lead'; the other instance is '$other'"
  # (b) a job through the STANDBY's API: the engine of the leader runs it, the standby's engine does not
  jobs_scn setup --api "http://$other:8080" --rows "$JOBS_J3_ROWS" --bad 8 --dup 8
  jobs_need "J3 setup through the standby's API"
  f="$(jobs_fact created)"; job1="$(jobs_get "$f" job)"; [ -n "$job1" ] || die "J3: no job in the 'created' fact: $f"
  JOBS_QUIET=1 jobs_scn wait --job "$job1" --until running --timeout 120
  [ "$JOBS_RC" = 0 ] || { cat "$WORK/jobs-scn-last.txt"; die "J3: the job did not reach running: $(printf '%s' "$JOBS_ERR" | tail -2 | tr '\n' ' ')"; }
  l0="$(jobs_lines "$lead" "$job1")"; o0="$(jobs_lines "$other" "$job1")"
  [ "$l0" -ge 1 ] || die "J3: the leader $lead logged no engine line for the running job (the engine's moves are 'job moved by the engine' and 'results recorded')"
  [ "$o0" = 0 ] || die "J3: the standby $other logged $o0 engine line(s) for the job while $lead leads: two engines"
  echo "  b. job $job1 was confirmed through $other's API and is running: engine lines in the log, $lead (leader) $l0, $other (standby) $o0"
  # (c) the leader is killed while the job runs
  lsvc="$lead"
  docker kill -s KILL "$(jobs_cid "$lsvc")" >/dev/null 2>&1 || die "J3: could not SIGKILL the leader $lsvc"
  t0=$SECONDS
  lk="$(jobs_lines "$lsvc" "$job1")"   # the dead instance writes no more: this is how much of the job's engine activity it logged, once and for all
  JOBS_QUIET=1 jobs_scn state --job "$job1"; echo "  c. $lsvc was SIGKILLed with the job at: $(jobs_fact state | cut -c1-260)"
  i=0
  while [ "$i" -lt 60 ]; do
    [ "$(jobs_nlocks_sql)" = 1 ] && [ "$(jobs_holders_sql)" = "$other" ] && break
    i=$((i+1)); sleep 1
  done
  [ "$i" -lt 60 ] || die "J3: $other did not take the leader lock within 60 s of the kill of $lsvc: locks=$(jobs_nlocks_sql) holders='$(jobs_holders_sql)'"
  echo "     $other holds the lock $((SECONDS - t0)) s after the kill (exactly one granted lock)"
  DC logs --no-color "$other" 2>&1 | grep -qF 'loops: this instance is now the leader' || die "J3: $other holds the lock but never logged that it became the leader"
  jobs_wait_terminal "$job1" 600
  jobs_verify "J3 job 1 (leader killed)" "$job1"
  l1="$(jobs_lines "$lsvc" "$job1")"; o1="$(jobs_lines "$other" "$job1")"
  [ "$l1" = "$lk" ] || die "J3: the killed instance $lsvc logged $((l1 - lk)) engine line(s) for the job after it was killed"
  [ "$o1" -ge 1 ] || die "J3: the survivor $other logged no engine line for the job it finished"
  echo "     the job ended on the survivor: engine lines for it, $lsvc $l1 (all before the kill: $l0 when the job was seen running, $lk at the kill), $other $o1; module 1 holds it once and it started once (verify)"
  # (d) the killed instance comes back: a standby
  DC up -d --no-deps --wait --wait-timeout 180 "$lsvc" >/dev/null 2>&1 || die "J3: could not start the killed instance $lsvc again"
  i=0; while [ "$i" -lt 12 ]; do
    [ "$(jobs_nlocks_sql)" = 1 ] && [ "$(jobs_holders_sql)" = "$other" ] || die "J3: after $lsvc came back the leader lock is held by '$(jobs_holders_sql)' ($(jobs_nlocks_sql) lock(s)); it must stay with $other"
    i=$((i+1)); sleep 1
  done
  DC logs --no-color "$lsvc" 2>&1 | grep -qF 'loops: another instance holds the leader lock' || die "J3: the restarted $lsvc never logged that another instance holds the lock (it did not run its election)"
  echo "  d. $lsvc is back and healthy; for 12 s the lock stayed with $other (it is the standby)"
  jobs_scn setup --api "http://$lsvc:8080" --rows 400 --bad 4 --dup 4
  jobs_need "J3 second job through the restarted standby's API"
  f="$(jobs_fact created)"; job2="$(jobs_get "$f" job)"
  jobs_wait_terminal "$job2" 600
  jobs_verify "J3 job 2 (standby back)" "$job2"
  [ "$(jobs_lines "$lsvc" "$job2")" = 0 ] || die "J3: the standby $lsvc logged engine lines for the second job: it ran the engine while $other leads"
  [ "$(jobs_lines "$other" "$job2")" -ge 1 ] || die "J3: the leader $other logged no engine line for the second job"
  [ "$(jobs_holders_sql)" = "$other" ] || die "J3: the lock moved to '$(jobs_holders_sql)' while the second job ran"
  echo "     the second job (through $lsvc's API) was run by $other alone: engine lines for it, $lsvc 0, $other $(jobs_lines "$other" "$job2"); the lock did not move"
}

# ── J4: module 1 is stopped in the middle of a job ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_probe_m1j() { DC run --rm -T --no-deps --entrypoint wget jobscenario -q -T 3 -O /dev/null http://m1j:8080/readyz 2>&1; }

jobs_j4() {
  STAGE=jobs-outage; banner J4 "module 1 is stopped while a job runs: the engine halts cleanly, invents nothing, flags the job stale and still waits; module 1 returns and the job completes exactly once"
  local f st r0 c0 i out rc job keep
  jobs_up jobs-a ""
  DC stop -t 10 jobs-b >/dev/null 2>&1 || true
  jobs_scn setup --api "$JOBS_API_A" --rows "$JOBS_OUTAGE_ROWS" --bad 6 --dup 6
  jobs_need "J4 setup"
  f="$(jobs_fact created)"; job="$(jobs_get "$f" job)"; [ -n "$job" ] || die "J4: no job in the 'created' fact: $f"
  # the stop must land in the middle: the job is running and module 1 has settled SOME of it and not all
  JOBS_QUIET=1 jobs_scn wait --job "$job" --until running --min-results 1 --timeout 180 --poll-ms 50
  [ "$JOBS_RC" = 0 ] || { cat "$WORK/jobs-scn-last.txt"; die "J4: the job did not reach running with a first result recorded: $(printf '%s' "$JOBS_ERR" | tail -2 | tr '\n' ' ')"; }
  echo "  the job is running and the engine has recorded its first results; module 1 is stopped now (docker compose stop: SIGTERM, a graceful stop)"
  DC stop -t 30 m1j >/dev/null 2>&1 || die "J4: could not stop module 1 (service m1j)"
  i=0; while [ "$i" -lt 40 ]; do st="$(docker inspect -f '{{.State.Status}}' "$(jobs_cid m1j)" 2>/dev/null)"; [ "$st" = exited ] && break; i=$((i+1)); sleep 1; done
  [ "$st" = exited ] || die "J4: module 1 is '$st' 40 s after the stop"
  out="$(jobs_probe_m1j)"; rc=$?
  [ "$rc" != 0 ] || die "J4: module 1 still answers: a request from a container on the private network to http://m1j:8080/readyz succeeded although the container is $st"
  case "$out" in
    *"server returned error"*) die "J4: module 1 still answers over the private network (an HTTP error, so something listens at the name m1j): $(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
    *"Connection refused"*|*"bad address"*|*"can't connect"*|*"timed out"*|*"nreachable"*) ;;
    *) die "J4: the probe from the private network did not run, so 'gone' was not proven (wget printed: $(printf '%s' "$out" | head -3 | tr '\n' ' '))";;
  esac
  echo "  module 1 is gone: the container is '$st' and a request from a container on the private network gets: $(printf '%s\n' "$out" | grep -m1 'wget:' || printf '%s\n' "$out" | head -1)"
  JOBS_QUIET=1 jobs_scn state --job "$job"; jobs_need "J4 state at the stop"
  f="$(jobs_fact state)"; r0="$(jobs_get "$f" results_module)"; c0="$(jobs_get "$f" charges)"; keep="$(jobs_get "$f" pending)"
  echo "  at the stop: $(printf '%s' "$f" | cut -c1-300)"
  [ "$r0" -ge 1 ] && [ "$r0" -lt "$keep" ] || die "J4: at the stop the engine had recorded $r0 of $keep results: the stop did not land in the middle of the job (module 1 finished first: use a longer list, JOBS_OUTAGE_ROWS)"
  # (i) the control: a verify of a job that is still running must fail, for that reason
  jobs_expect_fail "J4 control: verify of a job that is still running" "FAIL job_succeeded" verify --job "$job"
  # (ii) 25 s of outage: the job stays running and nothing moves
  i=0; while [ "$i" -lt 5 ]; do
    sleep 5
    JOBS_QUIET=1 jobs_scn state --job "$job" --expect "status=running,results_module=$r0,charges=$c0,results_invented=0,events_running=1,releases=0,finished=0,reserves=1"
    jobs_need "J4: during the outage (module 1 down, $(( (i+1) * 5 )) s)"
    i=$((i+1))
  done
  echo "  for 25 s with module 1 down: the job stayed running, results recorded stayed $r0, charge rows stayed $c0, no result was invented, nothing was released"
  # (iii) after the staleness limit (one minute without progress) a stale event exists and the job is STILL running
  i=0; f=""
  while [ "$i" -lt 18 ]; do
    JOBS_QUIET=1 jobs_scn state --job "$job"; jobs_need "J4 state while waiting for the stale event"
    f="$(jobs_fact state)"
    [ "$(jobs_get "$f" events_stale)" -ge 1 ] && break
    i=$((i+1)); sleep 10
  done
  [ "$(jobs_get "$f" events_stale)" -ge 1 ] || die "J4: no stale event after $((i * 10 + 25)) s without progress (the limit is one minute): $f"
  JOBS_QUIET=1 jobs_scn state --job "$job" --expect "status=running,results_module=$r0,charges=$c0,results_invented=0,events_running=1,releases=0,finished=0,events_stale>0"
  jobs_need "J4: the stale job"
  echo "  the engine raised a stale event (events_stale $(jobs_get "$f" events_stale)) and the job is STILL running, with the same $r0 results and $c0 charge rows: it was flagged, not failed, expired or finished"
  # (iv) module 1 comes back: the job completes once
  DC up -d --no-deps --wait --wait-timeout 240 m1j >/dev/null 2>&1 || die "J4: module 1 did not become healthy again"
  out="$(jobs_probe_m1j)" || die "J4: module 1 is healthy by its own check but a request from the private network failed: $(printf '%s' "$out" | head -2 | tr '\n' ' ')"
  echo "  module 1 is back: healthy by its own check and /readyz answers from a container on the private network"
  jobs_wait_terminal "$job" 900
  jobs_verify "J4" "$job"
  JOBS_QUIET=1 jobs_scn state --job "$job" --expect "status=succeeded,finished=1,events_stale>0,reserves=1,results_invented=0,events_running=1"
  jobs_need "J4: the finished job"
  echo "  the job completed exactly once after the outage: succeeded, one reserve, started once, results $(jobs_get "$(jobs_fact state)" results) of $(jobs_get "$(jobs_fact state)" rows) rows (verify: one release, one finished transition)"
}

# ── J6: module 1's vendor fallback, switched by re-creating module 1 ──────────────────────────────────────────────────────────────────────────────────────────────
# jobs_j6_ok <name>: one assertion of J6 held: its line is printed and kept (jobs_summary counts them).
jobs_j6_ok() {
  echo "  J6 OK $1"
  echo "J6 OK $1" >> "$WORK/jobs-j6.log"
}

# jobs_m1j_vendor <on|off>: (re-)creates module 1 (service m1j) with the dials of its vendor fallback ON (TM_FALLBACK_MODE=enabled, TM_EXTERNAL_DPA_CONFIRMED=true, a daily spend cap of 100, the key variable
# holding this run's stand-in key) or OFF (off, false, 0, the key variable empty), waits until it is healthy, and PROVES the container got what was said from `docker inspect` (the names and the dials printed,
# the key only as present or absent and equal to the minted value, never printed). The values reach the compose file through variables set for this one command (docker-compose.jobs.yml, the m1j environment),
# like the fault points of the instances. The log of the container that is about to go is kept first (a re-created container's log is gone), so the leak scan still reads it.
jobs_m1j_vendor() {
  local mode="$1" want_mode want_dpa want_cap key="" cid env_out got_key
  case "$mode" in
    on)  want_mode=enabled; want_dpa=true;  want_cap=100; key="$JOBS_VENDOR";;
    off) want_mode=off;     want_dpa=false; want_cap=0;;
    *)   die "jobs_m1j_vendor: $mode is neither on nor off";;
  esac
  [ "$mode" = off ] || [ -n "$key" ] || die "jobs_m1j_vendor on: the stand-in vendor key was not minted (jobs_mint): module 1 would see no key and the vendor would stay unreachable"
  JOBS_M1J_GEN=$(( ${JOBS_M1J_GEN:-0} + 1 ))
  if [ -n "$(jobs_cid m1j)" ]; then
    DC logs --no-color m1j > "$WORK/jobs-m1j-before-switch-$JOBS_M1J_GEN.log" 2>&1 || die "could not keep the log of module 1 before it is re-created"
  fi
  ( export H_M1J_FALLBACK_MODE="$want_mode" H_M1J_DPA_CONFIRMED="$want_dpa" H_M1J_DAILY_SPEND_CAP="$want_cap" H_M1J_VENDOR_KEY="$key"
    DC up -d --no-deps --force-recreate --wait --wait-timeout 240 m1j ) > "$WORK/jobs-up-m1j.log" 2>&1 || { sed 's/^/     | /' "$WORK/jobs-up-m1j.log" | tail -20; die "module 1 (m1j) did not become healthy with its vendor fallback $mode"; }
  cid="$(jobs_cid m1j)"; [ -n "$cid" ] || die "module 1 (m1j) has no container after it was re-created"
  env_out="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$cid" 2>/dev/null)"
  [ -n "$env_out" ] || die "could not read the environment of module 1's container"
  printf '%s\n' "$env_out" | grep -qx "TM_FALLBACK_MODE=$want_mode" || die "module 1's container has TM_FALLBACK_MODE=$(printf '%s\n' "$env_out" | sed -n 's/^TM_FALLBACK_MODE=//p'), want $want_mode"
  printf '%s\n' "$env_out" | grep -qx "TM_EXTERNAL_DPA_CONFIRMED=$want_dpa" || die "module 1's container has TM_EXTERNAL_DPA_CONFIRMED=$(printf '%s\n' "$env_out" | sed -n 's/^TM_EXTERNAL_DPA_CONFIRMED=//p'), want $want_dpa"
  printf '%s\n' "$env_out" | grep -qx "TM_EXTERNAL_DAILY_SPEND_CAP=$want_cap" || die "module 1's container has TM_EXTERNAL_DAILY_SPEND_CAP=$(printf '%s\n' "$env_out" | sed -n 's/^TM_EXTERNAL_DAILY_SPEND_CAP=//p'), want $want_cap"
  printf '%s\n' "$env_out" | grep -qx "TM_EXTERNAL_ZEROBOUNCE_KEY_ENV=M1J_VENDOR_KEY" || die "module 1's container does not name M1J_VENDOR_KEY as the variable that holds the vendor key"
  got_key="$(printf '%s\n' "$env_out" | sed -n 's/^M1J_VENDOR_KEY=//p')"
  if [ "$mode" = on ]; then
    [ -n "$got_key" ] && [ "$got_key" = "$JOBS_VENDOR" ] || die "module 1's key variable M1J_VENDOR_KEY is not the minted stand-in key (it is ${#got_key} characters long): the switch did not reach the container"
  else
    [ -z "$got_key" ] || die "module 1's key variable M1J_VENDOR_KEY holds a value (${#got_key} characters) with the vendor switched off"
  fi
  got_key=""
  echo "  module 1 (m1j) was re-created with its vendor fallback $mode: TM_FALLBACK_MODE=$want_mode, TM_EXTERNAL_DPA_CONFIRMED=$want_dpa, TM_EXTERNAL_DAILY_SPEND_CAP=$want_cap, the key variable M1J_VENDOR_KEY $([ "$mode" = on ] && echo 'holds the minted stand-in key (not shown)' || echo 'is empty')"
}

# jobs_remember_wait: waits longer than the platform keeps module 1's last answer, so that the next preview or confirm asks module 1 again
jobs_remember_wait() { sleep "$((JOBS_VENDOR_MEMORY_SEC + 2))"; }

# jobs_vendor_says <true|false> <label>: module 1 ITSELF (its describe, read with the platform's own service caller's key) says vendor_fallback.reachable <the value>
jobs_vendor_says() {
  jobs_scn vendor --expect "known=1,reachable=$1"
  jobs_need "J6 $2: module 1's own describe"
  echo "  module 1 itself says vendor_fallback.reachable = $1 ($2)"
}

# jobs_j6_job <label>: a fresh user with a draft of the J6 list, previewed (sets JOB, PENDING, FP: the fingerprint of that preview)
jobs_j6_job() {
  local f
  jobs_create "$JOBS_API_A" "$JOBS_J6_ROWS" 2 2
  f="$(jobs_fact created)"; FP="$(jobs_get "$f" fingerprint)"
  [ "${#FP}" = 64 ] || die "J6 $1: the preview of the new job has no fingerprint: $f"
  [ "$PENDING" = "$((JOBS_J6_ROWS - 4))" ] || die "J6 $1: the job has $PENDING pending rows, want $((JOBS_J6_ROWS - 4))"
}

# jobs_j6_preview <job> <rate> <vendor on|off> <label>: the preview of the job, again, is at <rate> credits per pending row, module 1 was read for it, and the rate unit is (or is not) the vendor unit
jobs_j6_preview() {
  local job="$1" rate="$2" vendor="$3" label="$4" unit
  if [ "$vendor" = on ]; then unit="rate_unit=m1_vendor"; else unit="rate_unit!=m1_vendor"; fi
  jobs_scn preview --api "$JOBS_API_A" --job "$job" --expect "unit_cost=$rate,credits=$((PENDING * rate)),units=$PENDING,known=1,$unit"
  jobs_need "J6 $label: the preview of job $job"
}

# jobs_j6_reserve <job> <rate> <vendor on|off> <label>: the job holds <PENDING x rate> credits, frozen at <rate>, in a snapshot that holds the vendor unit exactly when the vendor was on at its confirm
jobs_j6_reserve() {
  local job="$1" rate="$2" vendor="$3" label="$4" snap
  if [ "$vendor" = on ]; then snap="snapshot_units=3,snapshot_vendor=2,rate_unit=m1_vendor"; else snap="snapshot_units=2,snapshot_vendor=0,rate_unit!=m1_vendor"; fi
  jobs_scn reserve --job "$job" --expect "reserve_rows=1,unit_cost=$rate,units=$PENDING,reserved_credits=$((PENDING * rate)),reserved=$((PENDING * rate)),charged=0,$snap"
  jobs_need "J6 $label: the reserve of job $job"
}

jobs_j6() {
  STAGE=jobs-vendor; banner J6 "module 1's vendor fallback is switched OFF, ON and OFF again by re-creating module 1: the platform reads it at the preview and the confirm, and the reserve is 1 credit a row, then 2, then 1"
  local f ja js jc jd fp0 pend2
  : > "$WORK/jobs-j6.log"
  DC stop -t 10 jobs-b >/dev/null 2>&1 || true                          # ONE instance: the one that serves every request below also runs the engine, which never starts a job scheduled an hour ahead
  jobs_up jobs-a ""

  # (a) OFF at the start: forced, through the very function that flips it later, so the stage does not depend on what an earlier stage left
  jobs_m1j_vendor off
  jobs_scn vendor --wait-registered 120 --expect "known=1,reachable=false"
  jobs_need "J6 the platform's service caller and module 1's answer at the start"
  jobs_j6_ok platform_registered_its_service_caller
  echo "  module 1 itself says vendor_fallback.reachable = false (the platform's own service caller is registered at module 1: it registered itself)"
  jobs_j6_ok module_says_off_at_the_start
  jobs_remember_wait

  # (b) OFF: a job is previewed at 1 per row, confirmed SCHEDULED (an hour ahead: the engine does not start it) and the reserve is 1 per row; its cancel releases all of it
  jobs_j6_job "off"; ja="$JOB"
  jobs_j6_preview "$ja" 1 off "off"
  jobs_scn flag --expect "reachable=false"; jobs_need "J6 off: the platform's flag"
  jobs_j6_ok off_preview_is_1_per_row_and_read_from_module_1
  jobs_scn confirm --api "$JOBS_API_A" --job "$ja" --scheduled-in 3600
  jobs_need "J6 off: the scheduled confirm"
  f="$(jobs_fact confirm)"
  [ "$(jobs_get "$f" status)" = scheduled ] && [ "$(jobs_get "$f" reserved)" = "$PENDING" ] || die "J6 off: the scheduled confirm did not hold $PENDING credits as a scheduled job: $f"
  jobs_j6_reserve "$ja" 1 off "off"
  jobs_j6_ok off_confirm_reserves_1_per_row_without_the_vendor_unit
  jobs_scn cancel --api "$JOBS_API_A" --job "$ja"; jobs_need "J6 off: the cancel of the scheduled job"
  jobs_scn reserve --job "$ja" --expect "reserve_rows=1,reserved=$PENDING,released=$PENDING,remainder=0,release_rows=1,balance_reserved=0"
  jobs_need "J6 off: the release after the cancel"
  jobs_j6_ok off_cancel_releases_the_whole_reserve

  # (c) a job is PREVIEWED under off and left there; then the vendor is switched ON
  jobs_j6_job "the job previewed under off"; js="$JOB"; fp0="$FP"
  jobs_j6_preview "$js" 1 off "the job previewed under off"
  fp0="$(jobs_get "$(jobs_fact preview)" fingerprint)"
  jobs_m1j_vendor on
  jobs_vendor_says true "after the flip"
  jobs_j6_ok module_says_on_after_the_flip
  jobs_remember_wait

  # (d) the confirm of the preview made under off: the platform reads module 1 (on), the flag and the prices follow, the fingerprint is not the one the user saw: refused, nothing held
  jobs_scn confirm --api "$JOBS_API_A" --job "$js" --scheduled-in 3600 --expect refused:preview_stale
  jobs_need "J6 on: the confirm of the preview made under off"
  jobs_scn reserve --job "$js" --expect "reserve_rows=0,reserved=0,snapshot_units=0,balance_reserved=0"
  jobs_need "J6 on: the refused confirm held something"
  jobs_j6_ok a_preview_made_under_off_is_stale_at_the_confirm
  jobs_scn flag --expect "reachable=true,source=m1_describe,from_module1>=1,last_reachable=true,last_source=m1_describe"
  jobs_need "J6 on: the platform's flag"
  jobs_j6_ok the_flag_follows_module_1_and_says_whose_word_it_is

  # (e) previewed again: 2 per row at the vendor unit, a new fingerprint; the scheduled confirm reserves 2 per row and freezes the vendor unit into the job's snapshot
  jobs_j6_preview "$js" 2 on "on"
  [ "$(jobs_get "$(jobs_fact preview)" fingerprint)" != "$fp0" ] || die "J6 on: the preview under on has the fingerprint of the preview under off ($fp0): the price of a reachable unit is not part of it"
  jobs_j6_ok on_preview_is_2_per_row_at_the_vendor_unit
  jobs_scn confirm --api "$JOBS_API_A" --job "$js" --scheduled-in 3600
  jobs_need "J6 on: the scheduled confirm"
  f="$(jobs_fact confirm)"; pend2=$((PENDING * 2))
  [ "$(jobs_get "$f" status)" = scheduled ] && [ "$(jobs_get "$f" reserved)" = "$pend2" ] || die "J6 on: the scheduled confirm did not hold $pend2 credits as a scheduled job: $f"
  jobs_j6_reserve "$js" 2 on "on"
  jobs_j6_ok on_confirm_reserves_2_per_row_with_the_vendor_unit_frozen

  # (f) OFF again: module 1 says so, a new job is previewed at 1 per row and reserves 1 per row; the job confirmed under ON is NOT changed
  jobs_m1j_vendor off
  jobs_vendor_says false "after the second flip"
  jobs_j6_ok module_says_off_again
  jobs_remember_wait
  jobs_j6_job "off again"; jc="$JOB"
  jobs_j6_preview "$jc" 1 off "off again"
  jobs_scn flag --expect "reachable=false,source=m1_describe,from_module1>=2,last_reachable=false,last_source=m1_describe"
  jobs_need "J6 off again: the platform's flag"
  jobs_j6_ok off_again_preview_is_1_per_row
  jobs_scn confirm --api "$JOBS_API_A" --job "$jc" --scheduled-in 3600
  jobs_need "J6 off again: the scheduled confirm"
  f="$(jobs_fact confirm)"
  [ "$(jobs_get "$f" status)" = scheduled ] && [ "$(jobs_get "$f" reserved)" = "$PENDING" ] || die "J6 off again: the scheduled confirm did not hold $PENDING credits as a scheduled job: $f"
  jobs_j6_reserve "$jc" 1 off "off again"
  jobs_j6_ok off_again_confirm_reserves_1_per_row
  jobs_j6_reserve "$js" 2 on "the job confirmed under on, after the flip back"
  jobs_scn state --job "$js" --expect "status=scheduled,reserves=1,releases=0"
  jobs_need "J6 off again: the job confirmed under on"
  jobs_j6_ok a_job_confirmed_under_on_is_not_changed_by_the_later_off
  jobs_scn cancel --api "$JOBS_API_A" --job "$jc"; jobs_need "J6 off again: the cancel"
  jobs_scn reserve --job "$jc" --expect "reserved=$PENDING,released=$PENDING,remainder=0,release_rows=1"
  jobs_need "J6 off again: the release after the cancel"
  jobs_scn cancel --api "$JOBS_API_A" --job "$js"; jobs_need "J6 on: the cancel of the job confirmed under on"
  jobs_scn reserve --job "$js" --expect "reserved=$pend2,released=$pend2,remainder=0,release_rows=1,balance_reserved=0"
  jobs_need "J6 on: the release after the cancel"
  echo "  both scheduled jobs were cancelled and every credit they held is back (the job confirmed under on released its $pend2, the other its $PENDING)"

  # (g) module 1 is STOPPED: a preview says it was not read, at the rate of the flag it last stored (off, set by module 1); module 1 comes back: read again
  jobs_j6_job "module 1 stopped"; jd="$JOB"
  DC stop -t 30 m1j >/dev/null 2>&1 || die "J6: could not stop module 1 (service m1j)"
  f="$(jobs_probe_m1j)" && die "J6: module 1 still answers after it was stopped: $f"
  jobs_remember_wait
  jobs_scn preview --api "$JOBS_API_A" --job "$jd" --expect "known=0,unit_cost=1,credits=$PENDING,rate_unit!=m1_vendor"
  jobs_need "J6 stopped: the preview"
  jobs_scn flag --expect "reachable=false,source=m1_describe"; jobs_need "J6 stopped: the flag"
  jobs_j6_ok a_module_that_cannot_be_read_is_said_not_to_be_read
  echo "  module 1 is stopped: the preview says vendor_reachable_known = false, at 1 per row (the flag module 1 last set), and the flag is untouched"
  DC up -d --no-deps --wait --wait-timeout 240 m1j >/dev/null 2>&1 || die "J6: module 1 did not become healthy again"
  jobs_remember_wait
  jobs_scn preview --api "$JOBS_API_A" --job "$jd" --expect "known=1,unit_cost=1,credits=$PENDING"
  jobs_need "J6 back: the preview"
  jobs_j6_ok a_module_that_is_back_is_read_again
  echo "  module 1 is back: the next preview says vendor_reachable_known = true"
}

# ── the end: evidence, leak scan, exposure ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_summary() {
  local want n
  STAGE=jobs-summary; banner "J5" "what the scenario program verified (and the assertions of J6)"
  [ "$JOBS_VERIFIED" = "$JOBS_VERIFY_RUNS_WANTED" ] || die "the stages ran $JOBS_VERIFIED verify run(s), want $JOBS_VERIFY_RUNS_WANTED (J1, five kills, two jobs of J3, J4): a stage did not reach its verify"
  want=$(( JOBS_VERIFY_RUNS_WANTED * ${#JOBS_VERIFY_OKS[@]} ))
  [ -s "$WORK/jobs-ok.log" ] || die "no OK line was kept: the scenario program verified nothing"
  n="$(grep -c ' OK ' "$WORK/jobs-ok.log" || true)"
  [ "$n" = "$want" ] || die "$n OK line(s) were printed by the $JOBS_VERIFIED verify runs, want $want ($JOBS_VERIFY_RUNS_WANTED runs x ${#JOBS_VERIFY_OKS[@]} invariants)"
  echo "  $n OK lines from $JOBS_VERIFIED verify runs ($JOBS_VERIFY_RUNS_WANTED x ${#JOBS_VERIFY_OKS[@]} invariants), by run:"
  sed 's/ OK .*//' "$WORK/jobs-ok.log" | sort | uniq -c | sed 's/^/     /'
  echo "  by invariant (each must hold in every run):"
  sed 's/^.* OK //' "$WORK/jobs-ok.log" | sort | uniq -c | sed 's/^/     /'
  jobs_j6_summary
}

# jobs_j6_summary: the J6 evidence: every name of JOBS_J6_OKS printed exactly once and nothing else, by the lines jobs_j6_ok kept
jobs_j6_summary() {
  local name c total
  [ -s "$WORK/jobs-j6.log" ] || die "no J6 line was kept: stage J6 proved nothing (it did not run, or it printed no assertion)"
  for name in "${JOBS_J6_OKS[@]}"; do
    c="$(grep -cx "J6 OK $name" "$WORK/jobs-j6.log" || true)"
    [ "$c" = 1 ] || die "J6 printed 'J6 OK $name' $c time(s), want exactly once: the assertion did not run (or ran twice)"
  done
  total="$(grep -c '^J6 OK ' "$WORK/jobs-j6.log" || true)"
  [ "$total" = "${#JOBS_J6_OKS[@]}" ] || die "J6 printed $total 'J6 OK' line(s), want exactly ${#JOBS_J6_OKS[@]}: it printed an assertion this stage does not know"
  echo "  J6: ${#JOBS_J6_OKS[@]} of ${#JOBS_J6_OKS[@]} assertions held (module 1's vendor fallback off, on, off again, and unreadable)"
}

# the secrets of this run, and anything shaped like a JWT, in no log that is printed or kept: the logs of the instances, of module 1 (the container that runs now AND every one re-created away by J6, whose log
# jobs_m1j_vendor kept) and of the fixture, and the scenario program's own output. The three secrets are the JWT secret, the key-derivation secret and the stand-in vendor key. grep reads the patterns from a
# file descriptor, so no secret is on a command line; nothing is printed but a name.
jobs_secret_leak() {
  local svc f kept
  [ -n "$JOBS_JWT" ] && [ -n "$JOBS_M1K" ] && [ -n "$JOBS_VENDOR" ] || die "the secrets of this run are empty: the leak scan would look for nothing"
  for svc in jobs-a jobs-b m1j jobsdns; do
    DC logs --no-color "$svc" > "$WORK/leak-$svc.log" 2>&1 || die "could not read the logs of $svc"
    [ -s "$WORK/leak-$svc.log" ] || { rm -f "$WORK/leak-$svc.log"; die "the log of $svc, which the leak check must read, is empty: the instrument matched nothing"; }
  done
  kept="$(ls "$WORK"/jobs-m1j-before-switch-*.log 2>/dev/null | tr '\n' ' ')"
  for f in "$WORK/leak-jobs-a.log" "$WORK/leak-jobs-b.log" "$WORK/leak-m1j.log" "$WORK/leak-jobsdns.log" "$WORK/jobs-run.log" $kept; do
    [ -s "$f" ] || die "the log $(basename "$f"), which the leak check must read, is missing or empty"
    if grep -qFf <(printf '%s\n' "$JOBS_JWT" "$JOBS_M1K" "$JOBS_VENDOR") "$f"; then rm -f "$WORK"/leak-*.log; die "a secret of this run (the JWT secret, the key-derivation secret or the stand-in vendor key) appears in $(basename "$f") (the log is not shown: it is public)"; fi
    if grep -qE 'eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}' "$f"; then rm -f "$WORK"/leak-*.log; die "something shaped like a JWT (an access token) appears in $(basename "$f") (the log is not shown: it is public)"; fi
  done
  rm -f "$WORK"/leak-*.log
  echo "  none of the three secrets of this run (the JWT secret, the key-derivation secret, the stand-in vendor key), nor anything shaped like a JWT, appears in the logs of jobs-a, jobs-b, m1j and jobsdns (module 1: the present container and $(printf '%s' "$kept" | wc -w | tr -d ' ') kept before a switch) or in the scenario program's own output"
}

jobs_exposure() {
  STAGE=exposure; banner 8 "scope jobs: no service publishes a port; the network is internal"
  local ids id pb cnt=0
  DC ps --all --format json > "$WORK/ps.json" || die "docker compose ps failed"
  bash "$HERE/check-exposure.sh" "$WORK/ps.json" postgres m1j jobsdns jobs-a jobs-b || die "a service is reachable from outside the network"
  [ "$(docker network inspect "$PROJECT_NET" -f '{{.Internal}}')" = true ] || die "$PROJECT_NET is not an internal network"
  ids="$(DC ps -q)"; [ -n "$ids" ] || die "no running containers to inspect"
  for id in $ids; do
    pb="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$id")"; cnt=$((cnt+1))
    case "$pb" in '{}'|'null') ;; *) die "container $(docker inspect -f '{{.Name}}' "$id") has host port bindings: $pb";; esac
  done
  echo "  second instrument: $cnt containers inspected, none has a host port binding; network $PROJECT_NET internal=true"
}

# ── the whole stage, and the scope ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
jobs_run() {
  : > "$WORK/jobs-run.log"; : > "$WORK/jobs-ok.log"; : > "$WORK/jobs-j6.log"
  rm -f "$WORK"/jobs-m1j-before-switch-*.log
  # in a full run the legacy platform containers (engine OFF) would win the leader lock and hold every job where it is, and the legacy module 1 would drain the same queue: they stop here
  if [ "$SCOPE" = full ]; then DC stop -t 10 platform platform-2 m1 >/dev/null 2>&1 || true; fi
  jobs_infra
  jobs_j1
  jobs_j2
  jobs_j3
  jobs_j4
  jobs_j6
  jobs_summary
  jobs_secret_leak
  jobs_exposure
}

# H_SCOPE=jobs: Postgres, the four migration chains, the install step after module 1's and the first admin (stages 1-4b ran), then the stages above (J0-J4 and J6) and nothing else (no module 2 or 3, no probe, no outage or
# leader stage of the full run). Called by run.sh after stage 4b; it never returns (it exits with the run's own status, the EXIT trap tears the stack down).
jobs_scope_run() {
  STAGE=services; banner 5 "scope jobs: module 1 (m1j), its fixture DNS and two platform instances with the job engine on (modules 2 and 3 are not started)"
  jobs_run
  STAGE=done
  echo
  echo "================ NOT CHECKED (scope jobs: what this run did not and could not vouch for)"
  jobs_not_checked
  echo "  - everything outside the job engine: module 2, module 3, the probe, the outage and leader stages of the full run and the ledger check are NOT run in this scope (H_SCOPE=full runs them)"
  echo
  echo "HARNESS RESULT: GREEN (scope jobs: $JOBS_VERIFIED verified jobs, $(grep -c ' OK ' "$WORK/jobs-ok.log" || true) invariant lines, ${#JOBS_J6_OKS[@]} J6 assertions) — the platform's own process was killed at five points, a leader was killed, module 1 was stopped: every job ended with the books closed; module 1's vendor fallback was switched off, on and off again and the reserve followed it (1, 2, 1 credits a row)"
  exit 0
}

# what the jobs stage does not vouch for (printed by the scoped run and, in the full run, run.sh's own list refers to it)
jobs_not_checked() {
  cat <<'EOF'
  - module 1 ONLY: modules 2 and 3 (their campaigns, workers, sinks and cancels) follow in a later chunk; their rows in the five kills, the two instances and the outage are not exercised here.
  - the kill points are the five of internal/faultpoint/names.txt, each hit by the real process killing ITSELF (SIGKILL, exit status 137): NOT exercised are a kill at any other instant (between two statements of one transaction other than
    the named ones, mid-HTTP-response, mid-write of a log line), an out-of-memory kill, a kill of the database or of module 1 together with the platform, and two kills in a row in one job. Which instant of the job each point lands at is asserted
    from the database at the moment of the kill (the state conditions in stage.sh); that the kill of `settle.after_commit` lands with SOME results recorded and not all depends on module 1 settling the list over more than one pass of the
    engine, which a list of JOBS_SETTLE_ROWS lines makes certain on the runners measured and nothing makes certain on every machine (the stage is red, naming it, when the kill lands with every result recorded).
  - the outage stops module 1's container gracefully (SIGTERM): a SIGKILL of module 1, a module that answers 500 or garbage, a slow module and a network partition are not exercised; the leases module 1's drainer holds on a stop are
    shortened (TM_QUEUE_LEASE_MARGIN_SECONDS 5) for the run, not module 1's default.
  - module 1's answers come from a fixture DNS (names under .test: NXDOMAIN, so invalid/no_mx and a charge; one name with an MX, so risky/plausible with smtp_unwired and no charge) and an unwired SMTP stage: a valid, a catch-all, a greylisted, a
    vendor (m1_vendor) or a cached answer never occurs, and neither does an unmapped one. What is proven is the platform's handling of module 1's answers and its books, not module 1's verdicts.
  - J6 switches module 1's vendor fallback by re-creating its container (module 1 has no hot reload: a change of a dial is a restart) and reads the PREVIEW and the RESERVE of a scheduled job and its cancel; NOT exercised: a vendor
    answer (module 1 is given a stand-in key and no verification is ever sent to the vendor: the network is internal and J6 starts no job), the charge of a vendor answer (the platform's settlement of m1_vendor is the db-tests'), a
    confirm that module 1's unreadable describe refuses (it needs the seeded flag that module 1 never set, which J1-J4 have already made module 1's word: the db-tests prove it, TestJobsStore_VendorReachabilityFromTheModule*),
    an IMMEDIATE confirm under any of the three states, and more than one platform instance reading module 1 at once.
  - the two instances share one database and one network: a partition between the leader and the database (a leader that is cut off and not killed keeps its server-side session and so the lock), more than two instances and a pass that is
    already running when its instance loses the lock are not exercised; the leader is killed with SIGKILL, so the server sees the closed socket at once.
  - the scenario program mints its users' tokens itself (HS256, the secret both instances were given): GoTrue's own tokens are the gotrue scope's; the program is a harness caller, not a browser.
  - a job's size: 60 lines for four kills and the baseline, JOBS_SETTLE_ROWS (1500) for the settle kill, JOBS_J3_ROWS (1500) and JOBS_OUTAGE_ROWS (1500) for the others; a list near the engine's limit of 20000 rows, two jobs of one customer at
    once and a job that runs for hours are not exercised.
EOF
  echo "NOT CHECKED: modules 2 and 3, a kill at any instant but the five points, a SIGKILL or a slow module 1, a verdict module 1 does not give on the fixture DNS, a partition, more than two instances, a job near 20000 rows"
}
