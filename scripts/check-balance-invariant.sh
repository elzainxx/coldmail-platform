#!/usr/bin/env bash
# check-balance-invariant.sh — the recompute gate for the credit ledger (Docs/phase-1-spec.md, locked decision 5; chunk B7).
#
#   check-balance-invariant.sh --dsn DSN [--exercise]
#   check-balance-invariant.sh --psql-cmd "CMD PREFIX" [--exercise]
#   check-balance-invariant.sh --drift FILE --overdrawn FILE
#   check-balance-invariant.sh --print-exercise-sql
#
# WHY IT EXISTS. platform.balances has exactly one writer, an AFTER INSERT trigger on platform.ledger, so a stored balance changes only in the transaction of the ledger row that explains it.
# That makes drift impossible for every writer that goes through the ledger and says NOTHING about the day someone disables the trigger, drops it, or ships a migration that breaks it. This
# gate is the alarm that notices: it reads the two views the migration defines and is RED on any row.
#   platform.balance_drift     every (user, module) whose stored balance differs from SUM(ledger deltas), or that has a ledger sum and no balance row (or a balance row and no ledger).
#   platform.reserve_overdrawn every reserve whose charges plus releases exceed what it reserved (the per-reserve half, which the per-balance CHECK cannot see).
#
# INPUT, three ways (exactly one):
#   --dsn DSN          psql connects with that connection string.
#   --psql-cmd "CMD"   a command prefix that runs psql, for a database that is not reachable from the host (the harness network publishes no port:
#                      --psql-cmd 'docker compose exec -T postgres psql -U postgres -d platform'). The script appends `-X -At -F '|' -v ON_ERROR_STOP=1` and feeds ALL its SQL on STDIN.
#   --drift FILE --overdrawn FILE   offline: the rows of the two views, one per line, pipe-separated (user_id|module_id|stored_available|ledger_available|stored_reserved|ledger_reserved
#                      and reserve_id|user_id|module_id|job_id|reserved_amount|settled_amount); an empty file means the view is empty. This is how the selftest feeds it.
# --dsn and --psql-cmd are mutually exclusive, and so are both of them with the offline files (usage error, exit 2).
#
# LIVE MODE reads the real database: it is RED if psql fails, if either view does not exist ("found no platform.balance_drift view: the instrument matched nothing"), or if any view has a row.
# --exercise (the proof that the gate can see the fault it exists for): in ONE transaction that is ALWAYS rolled back (there is no COMMIT in it: --print-exercise-sql shows it) the script
#   creates its own fixtures (an auth.users row, a public.users row, an admin, a job and its price snapshot), writes a grant, a reserve, two partial charges and a release THROUGH THE LEDGER, and
#   THEN reads the two views, restricted to its own fixture user, in the same transaction. A missing or disabled balance trigger leaves the stored balance behind the ledger, the views are
#   not empty and the gate is RED: that is the fault it is proven against (scripts/check-balance-invariant-selftest.sh proves the parsing; the fault-proofs leg proves it against a real
#   database whose trigger was disabled). It prints how many ledger rows it exercised and is RED below 5: an exercise that exercised nothing proves nothing. It needs a role that may write
#   the fixtures (the migrator), and leaves nothing behind.
#
# EXIT: 0 green; 1 RED (the reason is printed; a row of a view starts BALANCE DRIFT or RESERVE OVERDRAWN and names user, module, stored and ledger values); 2 usage error.
set -u

DSN=""; PSQLCMD=""; EXERCISE=0; DRIFT=""; OVERDRAWN=""; PRINT_SQL=0; GOT_DSN=0; GOT_CMD=0
die_usage() { echo "check-balance-invariant: $*" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --dsn)       [ $# -ge 2 ] && [ -n "$2" ] || die_usage "--dsn needs a connection string"; DSN="$2"; GOT_DSN=1; shift 2;;
    --psql-cmd)  [ $# -ge 2 ] && [ -n "$2" ] || die_usage "--psql-cmd needs a command prefix"; PSQLCMD="$2"; GOT_CMD=1; shift 2;;
    --drift)     [ $# -ge 2 ] && [ -n "$2" ] || die_usage "--drift needs a file"; DRIFT="$2"; shift 2;;
    --overdrawn) [ $# -ge 2 ] && [ -n "$2" ] || die_usage "--overdrawn needs a file"; OVERDRAWN="$2"; shift 2;;
    --exercise)  EXERCISE=1; shift;;
    --print-exercise-sql) PRINT_SQL=1; shift;;
    *) die_usage "unknown argument '$1'";;
  esac
done

# The whole exercise as ONE stdin script. Pure SQL (no psql meta-commands) so it can be fed through any psql, including `docker compose exec -T`. Every row it prints starts with a marker; the
# rest (command tags, notices) is ignored by the parser. It is ROLLED BACK; if any statement fails psql stops (ON_ERROR_STOP) and the open transaction dies with the connection.
exercise_sql() {
  cat <<'SQL'
BEGIN;
SELECT 'FIXTURE', set_config('bi.u', gen_random_uuid()::text, true), set_config('bi.a', gen_random_uuid()::text, true), set_config('bi.j', gen_random_uuid()::text, true);
INSERT INTO auth.users (id, email)
SELECT x, 'bi-' || x::text || '@exercise.invalid' FROM (VALUES (current_setting('bi.u')::uuid), (current_setting('bi.a')::uuid)) AS v(x);
INSERT INTO public.users (id, auth_user_id, email)
SELECT id, id, email FROM auth.users WHERE id IN (current_setting('bi.u')::uuid, current_setting('bi.a')::uuid);
INSERT INTO platform.admins (id, display_name) VALUES (current_setting('bi.a')::uuid, 'balance-invariant exercise (rolled back)');
INSERT INTO platform.jobs (id, user_id, module_id) VALUES (current_setting('bi.j')::uuid, current_setting('bi.u')::uuid, 'm1');
INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost)
SELECT current_setting('bi.j')::uuid, module_id, unit, cost FROM platform.unit_costs WHERE module_id = 'm1';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'grant', 100 * p.cost, current_setting('bi.a')::uuid, 'balance-invariant exercise (rolled back)', 'bi-grant-1'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'grant', 50 * p.cost, current_setting('bi.a')::uuid, 'balance-invariant exercise (rolled back)', 'bi-grant-2'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, unit, units, unit_cost, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'reserve', -(10 * p.cost), 10 * p.cost, p.job_id, p.unit, 10, p.cost, 'bi-reserve'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'charge', 0, -(4 * p.cost), p.job_id,
       (SELECT id FROM platform.ledger WHERE user_id = current_setting('bi.u')::uuid AND module_id = 'm1' AND idempotency_key = 'bi-reserve'), p.unit, 4, p.cost, 'bi-charge-1'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'charge', 0, -(2 * p.cost), p.job_id,
       (SELECT id FROM platform.ledger WHERE user_id = current_setting('bi.u')::uuid AND module_id = 'm1' AND idempotency_key = 'bi-reserve'), p.unit, 2, p.cost, 'bi-charge-2'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, idempotency_key)
SELECT current_setting('bi.u')::uuid, 'm1', 'release', 4 * p.cost, -(4 * p.cost), p.job_id,
       (SELECT id FROM platform.ledger WHERE user_id = current_setting('bi.u')::uuid AND module_id = 'm1' AND idempotency_key = 'bi-reserve'), 'bi-release'
FROM platform.job_unit_prices p WHERE p.job_id = current_setting('bi.j')::uuid AND p.unit = 'm1_own';
SELECT 'EXERCISED', count(*) FROM platform.ledger WHERE user_id = current_setting('bi.u')::uuid;
SELECT 'BALANCE', COALESCE(b.available, -1), COALESCE(b.reserved, -1)
FROM (SELECT 1) AS one LEFT JOIN platform.balances b ON b.user_id = current_setting('bi.u')::uuid AND b.module_id = 'm1';
SELECT 'DRIFT', user_id, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved
FROM platform.balance_drift WHERE user_id = current_setting('bi.u')::uuid ORDER BY module_id;
SELECT 'OVERDRAWN', reserve_id, user_id, module_id, job_id, reserved_amount, settled_amount
FROM platform.reserve_overdrawn WHERE user_id = current_setting('bi.u')::uuid ORDER BY reserve_id;
ROLLBACK;
SELECT 'ROLLED_BACK';
SQL
}

# The existence check is its own query so that a missing view is reported as "the instrument matched nothing" and not as an opaque psql error.
VIEWS_SQL="SELECT 'VIEW', v.name, count(c.oid) FROM (VALUES ('balance_drift'), ('reserve_overdrawn')) AS v(name) LEFT JOIN pg_catalog.pg_class c ON c.relname = v.name AND c.relnamespace = to_regnamespace('platform') AND c.relkind IN ('v', 'm') GROUP BY v.name ORDER BY v.name;"
DATA_SQL="SELECT 'COUNTS', (SELECT count(*) FROM platform.ledger), (SELECT count(*) FROM platform.balances);
SELECT 'DRIFT', user_id, module_id, stored_available, ledger_available, stored_reserved, ledger_reserved FROM platform.balance_drift ORDER BY user_id, module_id;
SELECT 'OVERDRAWN', reserve_id, user_id, module_id, job_id, reserved_amount, settled_amount FROM platform.reserve_overdrawn ORDER BY reserve_id;"

if [ "$PRINT_SQL" -eq 1 ]; then exercise_sql; exit 0; fi

OFFLINE=0; { [ -n "$DRIFT" ] || [ -n "$OVERDRAWN" ]; } && OFFLINE=1
if [ "$OFFLINE" -eq 1 ]; then
  { [ -n "$DRIFT" ] && [ -n "$OVERDRAWN" ]; } || die_usage "offline mode needs BOTH --drift FILE and --overdrawn FILE"
  { [ "$GOT_DSN" -eq 0 ] && [ "$GOT_CMD" -eq 0 ] && [ "$EXERCISE" -eq 0 ]; } || die_usage "--drift/--overdrawn (offline) cannot be combined with --dsn, --psql-cmd or --exercise"
else
  { [ "$GOT_DSN" -eq 1 ] && [ "$GOT_CMD" -eq 1 ]; } && die_usage "--dsn and --psql-cmd are mutually exclusive"
  { [ "$GOT_DSN" -eq 1 ] || [ "$GOT_CMD" -eq 1 ]; } || die_usage "give --dsn DSN, --psql-cmd CMD, or --drift FILE --overdrawn FILE (and --exercise only with --dsn or --psql-cmd)"
fi

# ---- from here on every exit path prints the NOT CHECKED line last ----
if [ "$OFFLINE" -eq 1 ]; then MODE="offline files"; elif [ "$EXERCISE" -eq 1 ]; then MODE="live exercise"; else MODE="live"; fi
finish() { # $1 = exit code
  if [ "$EXERCISE" -eq 1 ]; then first="the live data of real users (it is only read when run WITHOUT --exercise)"
  elif [ "$OFFLINE" -eq 1 ]; then first="where the files came from (a stale or hand-made file proves nothing about a database)"
  else first="the exercise path (only run with --exercise: on an idle database a missing trigger shows no drift, because there is nothing for it to have missed)"; fi
  echo "NOT CHECKED: $first; row-level tampering that preserves the sums (an UPDATE of a ledger row that keeps every delta, or an edit of reason, unit or job); a ledger row pair that cancels (an offsetting insert hides both); anything the two views do not select"
  exit "$1"
}
red() { echo "check-balance-invariant: RED — $*"; finish 1; }

T=""
cleanup() { [ -z "$T" ] || rm -rf "$T"; }
trap cleanup EXIT

if [ "$OFFLINE" -eq 0 ]; then
  if [ "$GOT_CMD" -eq 1 ]; then
    read -r -a PSQL_ARGV <<< "$PSQLCMD"
    [ "${#PSQL_ARGV[@]}" -ge 1 ] || die_usage "--psql-cmd is empty"
    command -v "${PSQL_ARGV[0]}" > /dev/null 2>&1 || red "--psql-cmd starts with '${PSQL_ARGV[0]}', which is not on PATH"
  else
    command -v psql > /dev/null 2>&1 || red "psql is not on PATH (the live mode needs the client; the offline mode --drift/--overdrawn does not)"
  fi
fi
T="$(mktemp -d)" || { echo "check-balance-invariant: cannot create a temporary directory" >&2; exit 2; }

psql_exec() { # SQL on stdin; rows on stdout
  if [ "$GOT_DSN" -eq 1 ]; then psql -X -At -F '|' -v ON_ERROR_STOP=1 "$DSN"
  else "${PSQL_ARGV[@]}" -X -At -F '|' -v ON_ERROR_STOP=1; fi
}
run_sql() { # $1 = SQL; output with CRs removed in $T/out; sets RC
  printf '%s\n' "$1" | psql_exec > "$T/out.raw" 2> "$T/err.raw"; RC=$?
  tr -d '\r' < "$T/out.raw" > "$T/out"; tr -d '\r' < "$T/err.raw" > "$T/err"
}
psql_failed() { # $1 = what was being run
  red "psql failed ($1, exit $RC): $(head -3 "$T/err" | tr '\n' ' ')"
}
rows_of() { awk -F'|' -v m="$1" '$1 == m { sub(/^[^|]*\|?/, ""); print }' "$2"; }   # the rows that start with marker $1, marker removed

if [ "$OFFLINE" -eq 1 ]; then
  for f in "$DRIFT" "$OVERDRAWN"; do [ -f "$f" ] || red "the input file $f does not exist"; done
  tr -d '\r' < "$DRIFT" > "$T/drift"; tr -d '\r' < "$OVERDRAWN" > "$T/overdrawn"
else
  # 1. the instrument must exist
  run_sql "$VIEWS_SQL"; [ "$RC" -eq 0 ] || psql_failed "looking for the two views"
  missing=0
  for v in balance_drift reserve_overdrawn; do
    n="$(awk -F'|' -v v="$v" '$1 == "VIEW" && $2 == v { print $3 }' "$T/out" | head -1)"
    if [ "${n:-0}" != "1" ]; then echo "check-balance-invariant: found no platform.$v view: the instrument matched nothing"; missing=$((missing+1)); fi
  done
  [ "$missing" -eq 0 ] || red "$missing of the 2 views the gate reads do not exist"
  # 2. read them (live), or exercise the ledger and read them (exercise)
  if [ "$EXERCISE" -eq 1 ]; then
    exercise_sql > "$T/exercise.sql"
    run_sql "$(cat "$T/exercise.sql")"; [ "$RC" -eq 0 ] || psql_failed "the exercise, which was rolled back with its connection"
    grep -q '^ROLLED_BACK' "$T/out" || red "the exercise never printed ROLLED_BACK: its final ROLLBACK did not run, so what it wrote cannot be assumed gone"
    ex="$(rows_of EXERCISED "$T/out" | head -1)"
    case "$ex" in ''|*[!0-9]*) red "the exercise printed no ledger-row count (got '${ex:-nothing}'): the instrument matched nothing";; esac
    [ "$ex" -ge 5 ] || red "the exercise wrote only $ex ledger row(s) (floor 5): an exercise that exercised nothing proves nothing"
    bal="$(rows_of BALANCE "$T/out" | head -1)"
    echo "check-balance-invariant: exercised $ex ledger row(s) (grant, grant, reserve, charge, charge, release) in one transaction that was rolled back; the stored balance after them was available|reserved = ${bal:-unknown}"
  else
    run_sql "$DATA_SQL"; [ "$RC" -eq 0 ] || psql_failed "reading the views"
    cnt="$(rows_of COUNTS "$T/out" | head -1)"
    case "$cnt" in [0-9]*"|"[0-9]*) ;; *) red "the read printed no COUNTS row (got '${cnt:-nothing}'): the instrument matched nothing";; esac
    echo "check-balance-invariant: live database: ${cnt%%|*} ledger row(s) and ${cnt#*|} balance row(s) were compared"
    [ "${cnt%%|*}" != "0" ] || echo "check-balance-invariant: WARNING — the ledger is empty, so this live check is vacuous; --exercise proves the gate on fixtures"
  fi
  rows_of DRIFT "$T/out" > "$T/drift"; rows_of OVERDRAWN "$T/out" > "$T/overdrawn"
fi

# ---- judge the two row sets (the same code for offline files, live rows and the exercise) ----
awk 'BEGIN { FS = "|" }
  function isint(s) { return s ~ /^-?[0-9]+$/ }
  $0 ~ /^[ \t]*$/ { next }
  { if (NF != 6 || $1 == "" || $2 == "" || !isint($3) || !isint($4) || !isint($5) || !isint($6)) {
      printf "MALFORMED balance_drift line %d (want user_id|module_id|stored_available|ledger_available|stored_reserved|ledger_reserved, four integers): %s\n", NR, $0; bad++; next }
    printf "BALANCE DRIFT: user %s module %s: stored available %s vs ledger %s, stored reserved %s vs ledger %s\n", $1, $2, $3, $4, $5, $6; n++ }
  END { printf "COUNT|%d|%d\n", n + 0, bad + 0 }' "$T/drift" > "$T/drift.j"
awk 'BEGIN { FS = "|" }
  function isint(s) { return s ~ /^-?[0-9]+$/ }
  $0 ~ /^[ \t]*$/ { next }
  { if (NF != 6 || !isint($1) || $2 == "" || $3 == "" || $4 == "" || !isint($5) || !isint($6)) {
      printf "MALFORMED reserve_overdrawn line %d (want reserve_id|user_id|module_id|job_id|reserved_amount|settled_amount, integers in columns 1, 5 and 6): %s\n", NR, $0; bad++; next }
    printf "RESERVE OVERDRAWN: reserve %s (user %s module %s job %s): reserved %s but charges and releases settled %s\n", $1, $2, $3, $4, $5, $6; n++ }
  END { printf "COUNT|%d|%d\n", n + 0, bad + 0 }' "$T/overdrawn" > "$T/overdrawn.j"
grep -v '^COUNT|' "$T/drift.j"; grep -v '^COUNT|' "$T/overdrawn.j"
nd="$(awk -F'|' '$1 == "COUNT" { print $2 }' "$T/drift.j")"; bd="$(awk -F'|' '$1 == "COUNT" { print $3 }' "$T/drift.j")"
no="$(awk -F'|' '$1 == "COUNT" { print $2 }' "$T/overdrawn.j")"; bo="$(awk -F'|' '$1 == "COUNT" { print $3 }' "$T/overdrawn.j")"
echo "check-balance-invariant: [$MODE] $nd BALANCE DRIFT row(s), $no RESERVE OVERDRAWN row(s), $((bd + bo)) malformed line(s)"
if [ $((nd + no + bd + bo)) -ne 0 ]; then
  red "$nd BALANCE DRIFT row(s), $no RESERVE OVERDRAWN row(s), $((bd + bo)) malformed line(s): the stored balances and the ledger disagree, or the input is not what the gate reads"
fi
echo "check-balance-invariant: OK"
finish 0
