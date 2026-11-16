#!/usr/bin/env bash
# run.sh — runs EVERY migration of the platform and of the three modules as the NON-superuser `postgres` against a digest-pinned supabase/postgres image, and REPORTS every
# statement that fails (file, line, SQLSTATE, the exact statement). It then passes only if the set of real BLOCKERS equals the committed list tests/supabase/known-blockers.txt:
# a new blocker, or a listed one that no longer fails, is red. Run by .github/workflows/supabase.yml on a GitHub runner; a run anywhere else is rehearsal, not evidence.
#
#   run.sh --dsn DSN --platform-dir DIR --modules-root DIR [--known FILE] [--preprovision FILE] [--label NAME]
#     DSN             the NON-superuser `postgres` role of the Supabase image, e.g. postgres://postgres:ci@localhost:5432/postgres (the database in it is the one migrated)
#     --known         the committed list of expected blockers for THIS pass; default: known-blockers.txt beside this script
#     --preprovision  a SQL file applied as `postgres` right after stage a and BEFORE every chain, with ON_ERROR_STOP=1 (db/install/preprovision.sql is the proposed no-module-change
#                     fix). If it fails the pass is RED and the chains are not run: a report about a provisioning that did not happen would be a report about nothing.
#                     Without it the pass shows what the migrations do with NO provisioning at all (the baseline).
#     --label         a name for the pass (letters, digits, . _ -), printed in every banner and in the result line; the workflow runs two passes, BASELINE and FIXED
#   env: S_WORK = the work directory (default: a new temp directory; raw psql output, per-file records and last-report.tsv are written there, never into the repository)
#        S_WAIT_SECONDS = how long to wait for the database to accept connections (default 180)
#
# WHY. The platform will run on Supabase, where the migrating role `postgres` is NOT a superuser (the real superuser is `supabase_admin`). The main harness runs stock Postgres
# with a superuser, so every migration that needs one (CREATE ROLE with attributes, ALTER ... OWNER TO a role the migrator cannot SET ROLE to, CREATE SCHEMA ... AUTHORIZATION,
# extensions, ALTER DEFAULT PRIVILEGES FOR ROLE, GRANT ... WITH ADMIN) has never been exercised. This job is a REPORTING instrument first: it does not fix anything.
#
# STAGES (each prints a banner; each ends in a hard assertion; a failed assertion is remembered and the run goes on where that still produces evidence, and the verdict is RED):
#   0  inputs      the platform checkout, the three module checkouts and psql exist
#   a  instrument  the migrating role is `f|t` for (rolsuper, rolcreaterole) — a superuser is RED: "not Supabase-shaped"; auth.users must exist in the pass's database (RED: "it was not
#                  cloned from the pristine image database"); facts about the image are printed; a CONTROL file of three statements whose outcome is known (one refused, one failing on a
#                  missing table, one fine) is run through the REAL psql and the classifier must read exactly that
#   p  provisioning  --preprovision FILE applied with ON_ERROR_STOP=1 (or "none"); the migrator's memberships of the two owner roles are printed afterwards
#   b  chains      the PLATFORM chain first (platform_0001 creates public.users and public.job_items: there is no module fixture), then those two tables are asserted to exist and be owned by
#                  platform_owner, then module 2's and module 3's db/preflight/check_platform_tables.sql (a failing one is red with its output), then module 1, module 2, module 3 — each file
#                  on its own, `psql -X -b -v ON_ERROR_STOP=0 -v VERBOSITY=verbose -f FILE` (no -1: every statement autocommits, so one failure does not poison the rest). Each chain must
#                  contain at least one file. Every failure is classified (BLOCKER | CASCADE; see classify.sh) and tabulated
#   c  compare     the BLOCKER set against known-blockers.txt: NEW BLOCKER / STALE ENTRY / malformed / duplicate is red; exit 0 only when none
# and finally prints what it did NOT check.
#
# INSTRUMENT GUARDS (a classifier that silently reads nothing would be a green with no subject): psql must exit 0 on every file (a failed statement is not a failed psql); the number of
# records must equal an independent grep of psql's `psql:FILE:LINE: ERROR` lines; no error line may be left unattributed, no STATEMENT echo orphaned, no `psql: error:` line present.
set -u -o pipefail
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLASSIFY="$HERE/classify.sh"
DSN=""; PLATFORM_DIR=""; MODULES_ROOT=""; KNOWN="$HERE/known-blockers.txt"; PREPROV=""; LABEL=""
usage() { echo "usage: run.sh --dsn DSN --platform-dir DIR --modules-root DIR [--known FILE] [--preprovision FILE] [--label NAME]" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --dsn)           [ $# -ge 2 ] || usage; DSN="$2"; shift 2;;
    --platform-dir)  [ $# -ge 2 ] || usage; PLATFORM_DIR="$2"; shift 2;;
    --modules-root)  [ $# -ge 2 ] || usage; MODULES_ROOT="$2"; shift 2;;
    --known)         [ $# -ge 2 ] || usage; KNOWN="$2"; shift 2;;
    --preprovision)  [ $# -ge 2 ] || usage; PREPROV="$2"; shift 2;;
    --label)         [ $# -ge 2 ] || usage; LABEL="$2"; shift 2;;
    -h|--help)       usage;;
    *)               echo "run.sh: unknown argument: $1" >&2; usage;;
  esac
done
[ -n "$DSN" ] && [ -n "$PLATFORM_DIR" ] && [ -n "$MODULES_ROOT" ] || usage
case "$LABEL" in
  *[!A-Za-z0-9._-]*) echo "run.sh: invalid --label '$LABEL': letters, digits, . _ - only" >&2; exit 2;;
esac

WAIT="${S_WAIT_SECONDS:-180}"
WORK="${S_WORK:-$(mktemp -d)}"; mkdir -p "$WORK/raw" "$WORK/control" || { echo "run.sh: cannot create the work directory $WORK" >&2; exit 2; }
REPORT="$WORK/last-report.tsv"
rm -f "$WORK"/rec.*.tsv "$WORK"/raw/* "$REPORT"       # a reused work directory must not carry the records of an earlier run into this one
CHAINS_JUDGED="platform,m1,m2,m3"

banner() { echo; echo "================ ${LABEL:+$LABEL }[$1] $2"; }
scrub() { printf '%s' "$1" | sed 's#\(://[^:/@]*\):[^@]*@#\1:***@#'; }
REDS=()
red() { REDS+=("$1"); [ "${#REDS[@]}" -gt 30 ] || echo "  RED: $1"; }          # after 30 the lines are kept for the final list but no longer echoed (a broken instrument would otherwise print one per file)
STAGE=start
die() { echo; echo "SUPABASE-MIGRATIONS RED${LABEL:+ [$LABEL]} at stage [$STAGE]: $*"; exit 1; }
q() { psql -X -w -At -F '|' -v ON_ERROR_STOP=1 -c "$1" "$DSN" < /dev/null; }                 # one statement, tuples only, `|`-separated
fact() { psql -X -w -v ON_ERROR_STOP=1 -c "$1" "$DSN" < /dev/null 2>&1 | sed 's/^/  | /' || true; }   # a fact for the report: never an assertion
statv() { sed -n "s/^$2=//p" "$1" 2> /dev/null | head -1; }
RAW_DUMPS=0
raw_dump() {
  RAW_DUMPS=$((RAW_DUMPS + 1)); [ "$RAW_DUMPS" -le 6 ] || { [ "$RAW_DUMPS" -gt 7 ] || echo "    (further raw dumps suppressed: every file's raw psql output is in $WORK/raw/)"; return 0; }
  echo "    ---- raw psql stderr (first 60 lines) of $(basename "$1") ----"; sed -n '1,60p' "$1" | sed 's/^/    | /'
}

# ── 0. inputs ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=inputs; banner 0 "inputs"
command -v psql > /dev/null 2>&1 || die "psql is not installed on this machine: the job cannot run, and a job that cannot run is red"
[ -f "$CLASSIFY" ] || die "classify.sh is missing beside run.sh ($CLASSIFY)"
[ -d "$PLATFORM_DIR/db/migrations" ] || die "--platform-dir $PLATFORM_DIR has no db/migrations"
PLATFORM_DIR="$(cd "$PLATFORM_DIR" && pwd)"
[ -d "$MODULES_ROOT" ] || die "--modules-root $MODULES_ROOT is not a directory"
MODULES_ROOT="$(cd "$MODULES_ROOT" && pwd)"
M1="$MODULES_ROOT/coldmail-verify"; M2="$MODULES_ROOT/coldmail-send-managed"; M3="$MODULES_ROOT/coldmail-send-byo"
for m in "$M1" "$M2" "$M3"; do [ -d "$m" ] || die "module checkout missing: $m (tools/fetch-modules.sh --lock puts the three beside the platform)"; done
echo "  psql:           $(psql --version)"
echo "  target:         $(scrub "$DSN")"
echo "  platform:       $PLATFORM_DIR"
for m in "$M1" "$M2" "$M3"; do echo "  $(basename "$m"):  $(git -C "$m" rev-parse --short HEAD 2> /dev/null || echo 'not a git checkout')"; done
echo "  pass label:     ${LABEL:-(none)}"
echo "  known blockers: $KNOWN"
if [ -n "$PREPROV" ]; then
  [ -f "$PREPROV" ] || die "--preprovision $PREPROV is not a file: a provisioning step that is named and missing is red, never skipped"
  PREPROV="$(cd "$(dirname "$PREPROV")" && pwd)/$(basename "$PREPROV")"
  echo "  preprovision:   $PREPROV"
else
  echo "  preprovision:   none (this pass shows what the migrations do with no provisioning at all)"
fi
echo "  work directory: $WORK"

# ── a. instrument ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=instrument; banner a "INSTRUMENT VALID — the migrating role must be Supabase-shaped (not a superuser)"
t0="$(date +%s)"; out=""
while :; do
  if out="$(PGCONNECT_TIMEOUT=5 psql -X -w -At -c 'select 1' "$DSN" 2>&1 < /dev/null)"; then break; fi
  [ $(( $(date +%s) - t0 )) -lt "$WAIT" ] || die "the database did not accept a connection within ${WAIT}s: $out"
  sleep 3
done
who="$(q "select current_user")" || die "could not ask the database who is connected"
[ "$who" = postgres ] || die "the DSN connects as '$who', not as the Supabase migrating role 'postgres'"
shape="$(q "select rolsuper, rolcreaterole from pg_roles where rolname = current_user")" || die "could not read pg_roles for the current role"
case "$shape" in
  "f|t") echo "  ok: current_user = postgres, (rolsuper, rolcreaterole) = (f, t): not a superuser, may create roles — the shape of Supabase's migrating role";;
  t\|*)  die "not Supabase-shaped: the migrating role is a superuser ((rolsuper, rolcreaterole) = ($shape)). Every privilege failure this job exists to find would be invisible.";;
  *)     die "not Supabase-shaped: (rolsuper, rolcreaterole) of the migrating role is ($shape), expected (f|t)";;
esac
au="$(q "select to_regclass('auth.users') is not null")" || die "could not ask the database whether auth.users exists"
if [ "$au" = t ]; then echo "  ok: auth.users exists in this pass's database (the pass database is a clone of the pristine image database; platform_0001 needs it)"
else die "the pass database has no auth.users: it was not cloned from the pristine image database"; fi
echo "  facts (what the image is; none of these is an assertion):"
echo "  version:"; fact "select version()"
echo "  auth schema / auth.users:"; fact "select to_regnamespace('auth') is not null as auth_schema_exists, to_regclass('auth.users') is not null as auth_users_exists"
echo "  roles named supabase* / anon / authenticated / service_role:"
fact "select rolname, rolsuper, rolcreaterole, rolcreatedb, rolcanlogin, rolbypassrls from pg_roles where rolname ~ '^(supabase|anon|authenticated|service_role)' order by rolname"
echo "  role memberships of postgres (the SET option decides ALTER ... OWNER TO):"
fact "select roleid::regrole as member_of, admin_option, inherit_option, set_option from pg_auth_members where member = (select oid from pg_roles where rolname = current_user) order by 1"
echo "  schemas public / auth (owner, privileges):"
fact "select nspname, pg_get_userbyid(nspowner) as owner, nspacl from pg_namespace where nspname in ('public', 'auth') order by 1"
echo "  CREATE on the database for postgres:"; fact "select has_database_privilege(current_user, current_database(), 'CREATE') as can_create_in_database"
# Phase 2 (moving the main harness onto this image): the integration tests need a SUPERUSER connection over TCP (to create scratch databases, SET session_replication_role, read pg_hba) while the
# migrators run as `postgres`. These are facts, printed so the harness wiring is chosen from measurements and not from memory of the image.
SUPER_DSN="${DSN/\/\/postgres:/\/\/supabase_admin:}"
echo "  the image's superuser over TCP with the same password (the harness's test-admin identity):"
psql -X -w -At -F '|' -v ON_ERROR_STOP=1 -c "select current_user, rolsuper, rolcreatedb from pg_roles where rolname = current_user" "$SUPER_DSN" < /dev/null 2>&1 | sed 's/^/  | /' || true
echo "  pg_hba rules (superuser only):"
psql -X -w -At -F '|' -c "select line_number, type, database, user_name, address, auth_method from pg_hba_file_rules order by line_number" "$SUPER_DSN" < /dev/null 2>&1 | sed 's/^/  | /' || true
echo "  can postgres create and force-drop a scratch database (the scratch-database tests do):"
{ psql -X -w -At -v ON_ERROR_STOP=1 -c "create database zz_probe_scratch" "$DSN" && psql -X -w -At -v ON_ERROR_STOP=1 -c "drop database zz_probe_scratch with (force)" "$DSN"; } < /dev/null 2>&1 | sed 's/^/  | /' || true
echo "  does the database already hold roles platform_owner / platform_app / verify_* (a clean image should not):"; fact "select rolname from pg_roles where rolname ~ '^(platform|verify|send_)' order by 1"

# run_file CHAIN FILE — run one SQL file through psql from its own directory (so psql prints the bare file name), classify its stderr, append the records to rec.CHAIN.tsv.
# Sets RF_FAILS RF_BLK RF_CAS RF_RC RF_PROBLEM. stdout goes to a sidecar file so a result row can never be read as the tail of a statement.
RF_FAILS=0; RF_BLK=0; RF_CAS=0; RF_RC=0; RF_PROBLEM=0
run_file() {
  local chain="$1" f="$2" dir base tag err out stats tsv heads n
  dir="$(cd "$(dirname "$f")" && pwd)"; base="$(basename "$f")"
  tag="$chain.$base"; err="$WORK/raw/$tag.stderr"; out="$WORK/raw/$tag.stdout"; stats="$WORK/raw/$tag.stats"; tsv="$WORK/raw/$tag.tsv"
  RF_PROBLEM=0; [ -f "$WORK/rec.$chain.tsv" ] || : > "$WORK/rec.$chain.tsv"
  ( cd "$dir" && PGCONNECT_TIMEOUT=15 psql -X -w -b -v ON_ERROR_STOP=0 -v VERBOSITY=verbose -f "$base" "$DSN" > "$out" 2> "$err" < /dev/null )
  RF_RC=$?
  if ! bash "$CLASSIFY" --chain "$chain" --stats "$stats" "$err" > "$tsv"; then
    red "$chain/$base: classify.sh failed on psql's output"; RF_PROBLEM=1; : > "$tsv"
  fi
  RF_FAILS="$(statv "$stats" records)"; RF_BLK="$(statv "$stats" blockers)"; RF_CAS="$(statv "$stats" cascades)"
  RF_FAILS="${RF_FAILS:-0}"; RF_BLK="${RF_BLK:-0}"; RF_CAS="${RF_CAS:-0}"
  if [ "$RF_RC" -ne 0 ]; then
    red "$chain/$base: psql exited $RF_RC (a failed statement still exits 0 with ON_ERROR_STOP=0; 1 = psql itself failed to start or connect, 2 = the connection was lost)"; RF_PROBLEM=1
  fi
  n="$(statv "$stats" psql_errors)"; if [ "${n:-0}" -gt 0 ]; then red "$chain/$base: psql printed $n 'psql: ...' error line(s) of its own"; RF_PROBLEM=1; fi
  n="$(statv "$stats" unattributed_error_lines)"; if [ "${n:-0}" -gt 0 ]; then red "$chain/$base: $n error line(s) carry no psql:FILE:LINE: prefix, so the classifier cannot attribute them"; RF_PROBLEM=1; fi
  n="$(statv "$stats" orphan_statements)"; if [ "${n:-0}" -gt 0 ]; then red "$chain/$base: $n STATEMENT echo(s) do not belong to the error before them"; RF_PROBLEM=1; fi
  heads="$(grep -c -E '^psql:[^ ]+: (ERROR|FATAL|PANIC):' "$err" || true)"
  if [ "${heads:-0}" != "$RF_FAILS" ]; then red "$chain/$base: an independent grep finds ${heads:-0} error line(s) in psql's output but the classifier made $RF_FAILS record(s)"; RF_PROBLEM=1; fi
  [ "$RF_PROBLEM" -eq 0 ] || raw_dump "$err"
  cat "$tsv" >> "$WORK/rec.$chain.tsv"
}

# the control: three statements whose outcome is known. The first needs a superuser, the second names a table that cannot exist and spans three lines, the third is fine.
cat > "$WORK/control/zz_instrument_control.sql" <<'SQL'
-- instrument control: statement 1 MUST be refused (the migrating role is not a superuser), statement 2 MUST fail on a missing table, statement 3 MUST succeed.
CREATE ROLE zz_instrument_probe SUPERUSER;
SELECT 1
  FROM zz_instrument_probe_missing_table
 WHERE true;
SELECT 1;
SQL
echo "  control: a file of three statements with a known outcome, through the real psql and the classifier"
run_file control "$WORK/control/zz_instrument_control.sql"
ctl="$WORK/rec.control.tsv"; CTL_WHY=""
cchk() { [ "$2" = "$3" ] || CTL_WHY="$CTL_WHY
      - $1: got [$2] want [$3]"; }
cchk "number of failed statements" "$(wc -l < "$ctl" | tr -d ' ')" 2
cchk "file name of record 1" "$(awk -F'\t' 'NR==1 {print $2}' "$ctl")" zz_instrument_control.sql
cchk "SQLSTATE of record 1 (CREATE ROLE ... SUPERUSER by a non-superuser)" "$(awk -F'\t' 'NR==1 {print $4}' "$ctl")" 42501
cchk "class of record 1" "$(awk -F'\t' 'NR==1 {print $5}' "$ctl")" BLOCKER
cchk "SQLSTATE of record 2 (undefined_table)" "$(awk -F'\t' 'NR==2 {print $4}' "$ctl")" 42P01
cchk "class of record 2" "$(awk -F'\t' 'NR==2 {print $5}' "$ctl")" CASCADE
s1="$(awk -F'\t' 'NR==1 {print $7}' "$ctl" | bash "$CLASSIFY" --decode)"; s2="$(awk -F'\t' 'NR==2 {print $7}' "$ctl" | bash "$CLASSIFY" --decode)"
case "$s1" in *zz_instrument_probe*) ;; *) CTL_WHY="$CTL_WHY
      - the statement echoed for record 1 does not contain the statement that failed: [$s1]";; esac
case "$s2" in *zz_instrument_probe_missing_table*) ;; *) CTL_WHY="$CTL_WHY
      - the statement echoed for record 2 does not contain its table name: [$s2]";; esac
[ "$(printf '%s\n' "$s2" | wc -l | tr -d ' ')" -ge 3 ] || CTL_WHY="$CTL_WHY
      - the three-line statement of record 2 came back as [$s2]: continuation lines of a statement are not being read"
if [ -z "$CTL_WHY" ] && [ "$RF_PROBLEM" -eq 0 ]; then
  echo "  ok: the classifier read the real psql output — [42501 BLOCKER] for the refused CREATE ROLE, [42P01 CASCADE] for the three-line statement, nothing for the clean one; raw output kept in $WORK/raw/"
else
  red "INSTRUMENT INVALID: the classifier did not read psql's real output the way classify.sh assumes:${CTL_WHY:- (see the lines above)}"
  echo "  The chains below still run so that their raw output is on record, but nothing they say is trusted."
  raw_dump "$WORK/raw/control.zz_instrument_control.sql.stderr"
fi

# ── p. provisioning ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=provision
if [ -n "$PREPROV" ]; then banner p "PROVISIONING — $(basename "$PREPROV") applied as postgres with ON_ERROR_STOP=1, before the chains"
else banner p "PROVISIONING — none: the chains run on whatever the cluster already holds"; fi
if [ -n "$PREPROV" ]; then
  pdir="$(dirname "$PREPROV")"; pbase="$(basename "$PREPROV")"
  ( cd "$pdir" && PGCONNECT_TIMEOUT=15 psql -X -w -v ON_ERROR_STOP=1 -f "$pbase" "$DSN" > "$WORK/raw/preprovision.stdout" 2> "$WORK/raw/preprovision.stderr" < /dev/null )
  prc=$?
  if [ "$prc" -ne 0 ]; then
    echo "  psql exited $prc applying $pbase; its output:"; sed 's/^/    | /' "$WORK/raw/preprovision.stderr" "$WORK/raw/preprovision.stdout" | head -60
    die "PROVISIONING FAILED: $pbase did not apply cleanly (psql exit $prc). The chains were NOT run: a report on a provisioning that did not happen would be a report on nothing."
  fi
  echo "  ok: $pbase applied (psql exit 0, ON_ERROR_STOP=1); the migrator's memberships of the two owner roles now:"
  fact "select m.roleid::regrole as member_of, m.admin_option, m.inherit_option, m.set_option from pg_auth_members m where m.member = (select oid from pg_roles where rolname = current_user) and m.roleid::regrole::text in ('send_managed_owner', 'send_byo_owner') order by 1"
else
  echo "  none: nothing is applied before the chains (roles are cluster-wide: a role an earlier pass created in this cluster is already here)"
fi

# ── b. chains ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# Order: the PLATFORM chain first (platform_0001 creates public.users and public.job_items itself: there is no module fixture any more), then the two tables are asserted, then module 2's and
# module 3's pre-flight (a failing one is red with its output, not a cascade), then module 1, module 2, module 3.
STAGE=chains; banner b "CHAINS — platform, then the module pre-flights, then module 1, 2, 3; one file at a time, every statement autocommitted, every failure recorded"
chain_files() {
  case "$1" in
    platform) find "$PLATFORM_DIR/db/migrations" -maxdepth 1 -type f -name 'platform_*.up.sql';;
    m1)       find "$M1/control-plane/migrations" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.sql';;
    m2)       find "$M2/db/migrations" -maxdepth 1 -type f -name '*.up.sql';;
    m3)       find "$M3/db/migrations" -maxdepth 1 -type f -name '*.up.sql';;
  esac 2> /dev/null | LC_ALL=C sort
}
declare -A NFILES=()
run_chain() {   # run_chain NAME — every file of the chain in order; the count is printed and kept in NFILES; a chain that matched nothing is red
  local c="$1" list n f
  list="$(chain_files "$c")"; n=0; [ -z "$list" ] || n="$(printf '%s\n' "$list" | wc -l | tr -d ' ')"
  NFILES[$c]="$n"
  if [ "$n" -lt 1 ]; then red "chain $c matched 0 migration files: the instrument matched nothing, so it would have passed vacuously"; return 0; fi
  echo "  chain $c: $n file(s), $(basename "$(printf '%s\n' "$list" | head -1)") .. $(basename "$(printf '%s\n' "$list" | tail -1)")"
  : > "$WORK/rec.$c.tsv"
  while IFS= read -r f; do
    run_file "$c" "$f"
    if [ "$RF_FAILS" -eq 0 ]; then echo "    [$c] $(basename "$f"): clean"
    else echo "    [$c] $(basename "$f"): $RF_FAILS failure(s) ($RF_BLK blocker, $RF_CAS cascade)"; fi
  done <<< "$list"
}
run_chain platform

echo "  the platform's own tables (platform_0001), asserted before any module runs:"
tabs="$(q "select coalesce((select pg_get_userbyid(relowner)::text from pg_class where oid = to_regclass('public.users')), '-') || '|' || coalesce((select pg_get_userbyid(relowner)::text from pg_class where oid = to_regclass('public.job_items')), '-')")" || tabs="query failed"
if [ "$tabs" = "platform_owner|platform_owner" ]; then echo "  ok: public.users and public.job_items exist and are owned by platform_owner (created by the platform's own migrations)"
else red "platform_0001 did not create the public tables: (owner of public.users | owner of public.job_items) = ($tabs), want (platform_owner|platform_owner); '-' means the table does not exist. The module pre-flights and chains below depend on them."; fi

echo "  module pre-flights (each module's db/preflight/check_platform_tables.sql, run as postgres; module 1 ships none: its chain has no foreign key into a platform table):"
for pair in "m2:$M2" "m3:$M3"; do
  mod="${pair%%:*}"; dir="${pair#*:}"; pf="$dir/db/preflight/check_platform_tables.sql"
  if [ ! -f "$pf" ]; then red "PRE-FLIGHT: $(basename "$dir") ships no db/preflight/check_platform_tables.sql (its migrator runs it before the chain)"; continue; fi
  run_file "preflight-$mod" "$pf"
  if [ "$RF_FAILS" -eq 0 ] && [ "$RF_PROBLEM" -eq 0 ]; then echo "  pre-flight $mod: PASS"
  else
    red "PRE-FLIGHT $mod: check_platform_tables.sql FAILED ($RF_FAILS failure(s)): $(awk -F'\t' 'NR==1 {print $6}' "$WORK/rec.preflight-$mod.tsv" | bash "$CLASSIFY" --decode | head -1)"
    echo "    its output (the statement that failed and its message):"; bash "$CLASSIFY" --table "$WORK/rec.preflight-$mod.tsv" | sed 's/^/  /'
  fi
done
run_chain m1
run_chain m2
run_chain m3

{ printf '#chain\tfile\tline\tsqlstate\tclass\tmessage\tstatement\tdetail\n'
  for c in control platform preflight-m2 preflight-m3 m1 m2 m3; do [ ! -f "$WORK/rec.$c.tsv" ] || cat "$WORK/rec.$c.tsv"; done; } > "$REPORT"

echo
echo "  EVERY FAILED STATEMENT (chain, file:line, SQLSTATE, class, first 160 characters of the statement, then the message):"
bash "$CLASSIFY" --table --chains "$CHAINS_JUDGED" "$REPORT"
echo
echo "  PER CHAIN (files run; failures):"
for c in platform m1 m2 m3; do
  s="$(bash "$CLASSIFY" --summary --chains "$c" "$REPORT" | tr -s ' ' | sed 's/^ //; s/^[^ ]* //')"
  echo "    $c: ${NFILES[$c]:-0} file(s) run; $s"
done
echo "  machine-readable report: $REPORT"

# ── c. compare ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=compare; banner c "COMPARE — the observed BLOCKER set against $(basename "$KNOWN")"
cmpout="$(bash "$CLASSIFY" --compare "$KNOWN" --chains "$CHAINS_JUDGED" "$REPORT" 2>&1)"; cmprc=$?
printf '%s\n' "$cmpout" | sed 's/^/  /'
[ "$cmprc" -eq 0 ] || red "the observed blocker set differs from $(basename "$KNOWN") (see NEW BLOCKER / STALE ENTRY / MALFORMED / DUPLICATE above)"
sug="$(bash "$CLASSIFY" --suggest --chains "$CHAINS_JUDGED" "$REPORT")"
if [ -n "$sug" ]; then
  echo
  echo "  OBSERVED BLOCKERS as known-blockers.txt lines (copy ONLY with the owner's ruling for module migrations; an entry is FILE|SQLSTATE|MESSAGE-SUBSTRING):"
  printf '%s\n' "$sug" | sed 's/^/    /'
fi

# ── result ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
STAGE=done
echo
echo "================ NOT CHECKED (what this run did not and could not vouch for)"
cat <<'EOF'
  - the pooler (Supavisor): this run connects straight to Postgres on 5432. A real project is reached through the direct connection or through the pooler in session or
    transaction mode; neither pooler mode was exercised.
  - Supabase Auth / GoTrue: the image carries the auth schema and its roles from its init scripts; no GoTrue process ran, so nothing here shows how auth.users is populated.
  - PostgREST, the exposed schemas and the RLS defaults of a real project: no API layer ran, and a hosted project's default exposure and RLS-on-new-tables behaviour were not exercised.
  - extensions available on the hosted service: the image's extension set is what was available here; the hosted service's list and versions can differ.
  - the real project's role grants: this is the image's `postgres` role with the image's default memberships. supabase_admin vs postgres defaults may differ by project age.
  - runtime behaviour of the modules: migrations only. No service, worker or integration test ran on this image.
  - transaction shape: psql ran each statement in autocommit, one by one. The modules' migrators run each file in ONE transaction, so there a failing statement aborts the rest of the
    file (25P02 for everything after it) and keeps nothing of it; here the later statements run, so some CASCADE rows above would not occur there and others would read 25P02.
  - the database is the one in the DSN: roles are CLUSTER-wide, so a role that an earlier pass of the same job created in this cluster is already here (and is found by the modules' IF NOT EXISTS guards); tables and schemas are per database (each pass runs in its own clone of the pristine image database, so the platform's public.users and public.job_items are created by the platform's own migrations in each).
  - which failures are blockers is a classification of SQLSTATE and message text (see classify.sh); a non-privilege failure that is a real incompatibility (a missing extension, say)
    is listed as a CASCADE and is not judged, so the CASCADE rows are read by a person.
EOF
echo
if [ "${#REDS[@]}" -eq 0 ]; then
  echo "SUPABASE-MIGRATIONS RESULT${LABEL:+ [$LABEL]}: GREEN — the migrating role is not a superuser; ${NFILES[platform]} + ${NFILES[m1]} + ${NFILES[m2]} + ${NFILES[m3]} migration files ran (platform + module 1 + module 2 + module 3); the observed blocker set equals $(basename "$KNOWN")"
  exit 0
fi
echo "SUPABASE-MIGRATIONS RESULT${LABEL:+ [$LABEL]}: RED — ${#REDS[@]} reason(s):"
i=0; for r in "${REDS[@]}"; do i=$((i + 1)); [ "$i" -le 30 ] || continue; echo "  - $r"; done
[ "${#REDS[@]}" -le 30 ] || echo "  - ... and $(( ${#REDS[@]} - 30 )) more (the first reason is usually the cause of the rest)"
exit 1
