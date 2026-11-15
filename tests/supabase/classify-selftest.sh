#!/usr/bin/env bash
# classify-selftest.sh — the PAIRED CONTROL for tests/supabase/classify.sh. No database, no psql: it feeds the classifier recorded-shape psql output (written inline below, in the
# format classify.sh documents) and asserts, for every case, the EXACT chain, file, line, SQLSTATE, class, message and statement text of each record; then it feeds the
# comparison against known-blockers.txt every way it can be wrong. A second half drives run.sh itself with a FAKE psql (a script that answers the scalar queries, logs each `-f FILE`
# call with its options and DSN, and prints canned stderr) over a miniature module tree: --label, --preprovision (order, ON_ERROR_STOP=1, the DSN's database, and the failure and
# missing-file paths, which must be RED and must not run the chains), --known per pass, and the guards. It also reads supabase.yml and preprovision.sql for the two-pass design.
#
# Every REJECT case (a compare that must go red, a usage error) asserts three things, not only a non-zero exit (false-green class 10): the exit status, the REASON text, and
# the exact NUMBER of each kind of finding (so a fixture that trips a second rule is caught here and not read as the first).
# Two guards on THIS file: a hard-coded MIN_CASES floor (deleting cases is red, not a quiet shrinkage), and a final CONTROL that feeds the harness two deliberately wrong
# expectations in a subshell and requires BOTH to be reported as mismatches (proof that the assertions here can fail at all).
# CRLF: the CRLF cases write real CR bytes and ASSERT that they exist (on this author's host awk and sed hide a CR, so a fixture that lost its CRs would prove nothing).
#
# NOT covered here, and only the first runner run can show: that a real psql prints exactly these shapes. The fixtures are what the PostgreSQL sources and its documented
# behaviour say psql prints (see the assumption list in classify.sh); run.sh therefore also runs a control file through the REAL psql and requires the classifier to read
# its output, and it cross-checks the number of records against an independent grep of the raw output.
set -u -o pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLS="$HERE/classify.sh"
MIN_CASES=98
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
FX="$TMP/fx"; mkdir -p "$FX"

cases=0; okc=0; bad=0
CUR_LABEL=""; CUR_BAD=0; CUR_MSG=""
begin()  { CUR_LABEL="$1"; CUR_BAD=0; CUR_MSG=""; }
fail_()  { CUR_BAD=$((CUR_BAD + 1)); CUR_MSG="$CUR_MSG
        - $1"; }
eq()     { [ "$2" = "$3" ] || fail_ "$1: got [$2] want [$3]"; }                        # eq NAME GOT WANT
has()    { case "$2" in *"$3"*) ;; *) fail_ "$1: [$2] does not contain [$3]";; esac; }   # has NAME TEXT NEEDLE
hasnot() { case "$2" in *"$3"*) fail_ "$1: [$2] unexpectedly contains [$3]";; *) ;; esac; }
finish() {
  cases=$((cases + 1))
  if [ "$CUR_BAD" -eq 0 ]; then okc=$((okc + 1)); echo "  ok   $CUR_LABEL"
  else bad=$((bad + 1)); echo "  BAD  $CUR_LABEL$CUR_MSG"; fi
}

# ---- fixtures: what psql -X -b -v VERBOSITY=verbose -f FILE writes to STDERR --------------------------------------------------------------------------------------------
cat > "$FX/create_role" <<'EOF'
psql:send_managed_0000_bootstrap.up.sql:20: ERROR:  42501: permission denied to create role
DETAIL:  Only roles with the CREATEROLE attribute may create roles.
CONTEXT:  SQL statement "CREATE ROLE send_managed_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB"
PL/pgSQL function inline_code_block line 9 at SQL statement
LOCATION:  CreateRole, user.c:318
psql:send_managed_0000_bootstrap.up.sql:20: STATEMENT:  DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'send_managed_owner') THEN
        CREATE ROLE send_managed_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
END $$
EOF
cat > "$FX/set_role" <<'EOF'
psql:platform_0000_bootstrap.up.sql:44: ERROR:  42501: must be able to SET ROLE "platform_owner"
LOCATION:  ATExecChangeOwner, tablecmds.c:15021
psql:platform_0000_bootstrap.up.sql:44: STATEMENT:  ALTER TABLE platform.schema_migrations OWNER TO platform_owner;
EOF
cat > "$FX/default_priv" <<'EOF'
psql:send_managed_0000_bootstrap.up.sql:47: ERROR:  42501: must be member of role "send_managed_owner"
LOCATION:  check_is_member_of_role, acl.c:5113
psql:send_managed_0000_bootstrap.up.sql:47: STATEMENT:  ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO send_managed_app;
EOF
cat > "$FX/cascade" <<'EOF'
psql:send_managed_0000_bootstrap.up.sql:53: ERROR:  42P01: relation "send_managed.schema_migrations" does not exist
LINE 1: INSERT INTO send_managed.schema_migrations (version) VALUES ...
                    ^
LOCATION:  parserOpenTable, parse_relation.c:1449
psql:send_managed_0000_bootstrap.up.sql:53: STATEMENT:  INSERT INTO send_managed.schema_migrations (version) VALUES ('0000');
EOF
cat > "$FX/schema_missing" <<'EOF'
psql:send_byo_0004_create_pools.up.sql:9: ERROR:  3F000: schema "send_byo" does not exist
LINE 1: CREATE TABLE send_byo.pools (
                     ^
LOCATION:  LookupCreationNamespace, namespace.c:3150
psql:send_byo_0004_create_pools.up.sql:9: STATEMENT:  CREATE TABLE send_byo.pools (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL
)
EOF
cat > "$FX/dup" <<'EOF'
psql:send_managed_0003_create_enums.up.sql:12: ERROR:  42710: type "job_state" already exists
LOCATION:  TypeCreate, pg_type.c:1011
psql:send_managed_0003_create_enums.up.sql:12: STATEMENT:  CREATE TYPE send_managed.job_state AS ENUM (
    'queued',

    'running'
);

EOF
cat > "$FX/clean" <<'EOF'
psql:send_byo_0001_create_schema.up.sql:14: NOTICE:  42P06: schema "send_byo" already exists, skipping
LOCATION:  CreateSchemaCommand, schemacmds.c:118
psql:send_byo_0001_create_schema.up.sql:19: NOTICE:  42P07: relation "schema_migrations" already exists, skipping
LOCATION:  transformCreateStmt, parse_utilcmd.c:207
psql:send_byo_0001_create_schema.up.sql:30: WARNING:  01007: no privileges were granted for "send_byo"
LOCATION:  ExecGrant_Relation, aclchk.c:1990
EOF
: > "$FX/empty"
cat > "$FX/multi" <<'EOF'
psql:m.sql:5: NOTICE:  42P06: schema "x" already exists, skipping
LOCATION:  CreateSchemaCommand, schemacmds.c:118
psql:m.sql:9: ERROR:  42501: permission denied to create role
LOCATION:  CreateRole, user.c:318
psql:m.sql:9: STATEMENT:  CREATE ROLE a LOGIN;
psql:m.sql:12: ERROR:  42704: role "a" does not exist
LOCATION:  get_role_oid, acl.c:5315
psql:m.sql:12: STATEMENT:  GRANT a TO b;
psql:m.sql:15: NOTICE:  00000: noise between two failures
psql:m.sql:20: ERROR:  42501: must be able to SET ROLE "a"
LOCATION:  ATExecChangeOwner, tablecmds.c:15021
psql:m.sql:20: STATEMENT:  ALTER TABLE t OWNER TO a;
EOF
cat > "$FX/nostatement" <<'EOF'
psql:n.sql:8: ERROR:  42501: permission denied to create role
LOCATION:  CreateRole, user.c:318
psql:n.sql:11: NOTICE:  00000: done
EOF
cat > "$FX/nosqlstate" <<'EOF'
psql:x.sql:3: ERROR:  permission denied to create extension "pgcrypto"
HINT:  Must have CREATE privilege on current database to create this extension.
psql:x.sql:3: STATEMENT:  CREATE EXTENSION IF NOT EXISTS pgcrypto;
psql:x.sql:7: ERROR:  relation "t" does not exist
psql:x.sql:7: STATEMENT:  SELECT 1 FROM t;
EOF
cat > "$FX/detail_only" <<'EOF'
psql:d.sql:4: ERROR:  XX000: cannot grant role "x"
DETAIL:  Only roles with the ADMIN option on role "x" may grant this role.
psql:d.sql:4: STATEMENT:  GRANT x TO y;
EOF
cat > "$FX/paths" <<'EOF'
psql:coldmail-platform/db/migrations/platform_0000_bootstrap.up.sql:44: ERROR:  42501: must be able to SET ROLE "platform_owner"
psql:coldmail-platform/db/migrations/platform_0000_bootstrap.up.sql:44: STATEMENT:  ALTER TABLE a OWNER TO b;
psql:D:/a/work/send_byo_0012_grants_and_rls.up.sql:31: ERROR:  42501: permission denied for schema send_byo
psql:D:/a/work/send_byo_0012_grants_and_rls.up.sql:31: STATEMENT:  GRANT USAGE ON SCHEMA send_byo TO x;
psql:D:\a\work\0016_verify_schema.sql:2: ERROR:  42501: must be owner of schema verify
psql:D:\a\work\0016_verify_schema.sql:2: STATEMENT:  ALTER SCHEMA verify OWNER TO verify_owner;
EOF
cat > "$FX/orphan" <<'EOF'
psql:o.sql:9: STATEMENT:  select 1
  from t;
EOF
cat > "$FX/psqlerr" <<'EOF'
psql: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: Connection refused
	Is the server running on that host and accepting TCP/IP connections?
EOF
cat > "$FX/stray" <<'EOF'
ERROR:  42501: permission denied to create role
psql:y.sql:5: ERROR:  P0001: boom
CONTEXT:  PL/pgSQL function inline_code_block line 3 at RAISE
psql:y.sql:5: STATEMENT:  DO $$ BEGIN RAISE EXCEPTION 'ERROR:  boom'; END $$
EOF
cat > "$FX/fatal" <<'EOF'
psql:f.sql:3: FATAL:  57P01: terminating connection due to administrator command
psql:f.sql:3: STATEMENT:  select pg_sleep(10);
psql: error: connection to server was lost
EOF
cat > "$FX/unprefixed_stmt" <<'EOF'
psql:u.sql:7: ERROR:  42501: permission denied to create role
LOCATION:  CreateRole, user.c:318
STATEMENT:  CREATE ROLE a
    LOGIN;
EOF
cat > "$FX/mismatch" <<'EOF'
psql:q.sql:5: ERROR:  42501: permission denied to create role
psql:q.sql:6: STATEMENT:  CREATE ROLE a;
EOF
S_TAB=$'SELECT 1/0, \'a\tb\', \'%0A 100%\';'
printf '%s\n' "psql:t.sql:2: ERROR:  22012: division by zero" "psql:t.sql:2: STATEMENT:  $S_TAB" > "$FX/tabs"
LONG="SELECT '$(printf 'x%.0s' $(seq 1 300))';"
printf '%s\n' "psql:l.sql:1: ERROR:  42501: permission denied to create role" "psql:l.sql:1: STATEMENT:  $LONG" > "$FX/long"
{ cat "$FX/create_role"; cat "$FX/cascade"; } > "$FX/crlf"
awk '{ printf "%s\r\n", $0 }' "$FX/crlf" > "$FX/crlf.tmp" && mv "$FX/crlf.tmp" "$FX/crlf"
CRS="$(tr -cd '\r' < "$FX/crlf" | wc -c | tr -d ' ')"

# ---- helpers ------------------------------------------------------------------------------------------------------------------------------------------------------------
PRC=0
parse()  { bash "$CLS" --chain "$2" --stats "$TMP/stats" "$FX/$1" > "$TMP/out.tsv" 2> "$TMP/err"; PRC=$?; }        # parse FIXTURE CHAIN
nrows()  { wc -l < "$TMP/out.tsv" | tr -d ' '; }
col()    { awk -F'\t' -v r="$1" -v c="$2" 'NR == r { print $c }' "$TMP/out.tsv"; }                                   # raw column (no escapes in 1-5)
dcol()   { awk -F'\t' -v r="$1" -v c="$2" 'NR == r { print $c }' "$TMP/out.tsv" | bash "$CLS" --decode; }            # decoded column (6 message, 7 statement, 8 detail)
stat()   { sed -n "s/^$1=//p" "$TMP/stats" | head -1; }
row()    { # row N CHAIN FILE LINE SQLSTATE CLASS
  eq "row $1 chain" "$(col "$1" 1)" "$2"; eq "row $1 file" "$(col "$1" 2)" "$3"; eq "row $1 line" "$(col "$1" 3)" "$4"
  eq "row $1 sqlstate" "$(col "$1" 4)" "$5"; eq "row $1 class" "$(col "$1" 5)" "$6"
}
allcols8() { awk -F'\t' '{ if (NF != 8) bad++ } END { print bad + 0 }' "$TMP/out.tsv"; }

echo "classify-selftest: PARSE — the shapes psql prints for failed statements"
begin "permission denied to create role: a BLOCKER by SQLSTATE 42501, DO block statement kept exactly, DETAIL/CONTEXT/LOCATION in the detail column"
parse create_role m2; eq rc "$PRC" 0; eq rows "$(nrows)" 1; row 1 m2 send_managed_0000_bootstrap.up.sql 20 42501 BLOCKER
eq message "$(dcol 1 6)" 'permission denied to create role'
eq statement "$(dcol 1 7)" $'DO $$\nBEGIN\n    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = \'send_managed_owner\') THEN\n        CREATE ROLE send_managed_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;\n    END IF;\nEND $$'
has detail "$(dcol 1 8)" 'DETAIL:  Only roles with the CREATEROLE attribute may create roles.'; has detail-location "$(dcol 1 8)" 'LOCATION:  CreateRole, user.c:318'
eq stats-records "$(stat records)" 1; eq stats-blockers "$(stat blockers)" 1; eq stats-unattributed "$(stat unattributed_error_lines)" 0; eq stats-orphans "$(stat orphan_statements)" 0
eq columns "$(allcols8)" 0; finish

begin "must be able to SET ROLE \"x\" (ALTER ... OWNER TO a role the migrator cannot SET ROLE to): BLOCKER"
parse set_role platform; row 1 platform platform_0000_bootstrap.up.sql 44 42501 BLOCKER
eq message "$(dcol 1 6)" 'must be able to SET ROLE "platform_owner"'; eq statement "$(dcol 1 7)" 'ALTER TABLE platform.schema_migrations OWNER TO platform_owner;'; finish

begin "ALTER DEFAULT PRIVILEGES ... must be member of role: BLOCKER, the two-line statement keeps its indentation"
parse default_priv m2; row 1 m2 send_managed_0000_bootstrap.up.sql 47 42501 BLOCKER
eq message "$(dcol 1 6)" 'must be member of role "send_managed_owner"'
eq statement "$(dcol 1 7)" $'ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed\n    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO send_managed_app;'; finish

begin "relation does not exist (42P01), with LINE/caret detail lines: a CASCADE, never judged"
parse cascade m2; row 1 m2 send_managed_0000_bootstrap.up.sql 53 42P01 CASCADE
eq message "$(dcol 1 6)" 'relation "send_managed.schema_migrations" does not exist'
eq statement "$(dcol 1 7)" "INSERT INTO send_managed.schema_migrations (version) VALUES ('0000');"
has detail-caret "$(dcol 1 8)" '^'; has detail-line "$(dcol 1 8)" 'LINE 1: INSERT INTO'; eq stats-cascades "$(stat cascades)" 1; finish

begin "schema does not exist (3F000): a CASCADE with a multi-line CREATE TABLE statement"
parse schema_missing m3; row 1 m3 send_byo_0004_create_pools.up.sql 9 3F000 CASCADE
eq statement "$(dcol 1 7)" $'CREATE TABLE send_byo.pools (\n    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),\n    name text NOT NULL\n)'; finish

begin "duplicate_object (42710) is a CASCADE; a blank line INSIDE the statement is kept, the blank lines after it are not"
parse dup m2; row 1 m2 send_managed_0003_create_enums.up.sql 12 42710 CASCADE
eq message "$(dcol 1 6)" 'type "job_state" already exists'
eq statement "$(dcol 1 7)" $'CREATE TYPE send_managed.job_state AS ENUM (\n    \'queued\',\n\n    \'running\'\n);'; finish

begin "a clean run (only NOTICE/WARNING with LOCATION lines): zero records, nothing unattributed"
parse clean m3; eq rc "$PRC" 0; eq rows "$(nrows)" 0; eq records "$(stat records)" 0; eq prefixed "$(stat prefixed_lines)" 3
eq unattributed "$(stat unattributed_error_lines)" 0; eq psql_errors "$(stat psql_errors)" 0; finish

begin "an EMPTY input (psql wrote nothing) is a clean run, not an error"
parse empty m3; eq rc "$PRC" 0; eq rows "$(nrows)" 0; eq lines "$(stat lines)" 0; finish

begin "CRLF line endings: the same two records, and no CR byte survives in the records"
eq fixture-has-CR "$([ "$CRS" -gt 0 ] && echo yes || echo no)" yes
parse crlf m2; eq rows "$(nrows)" 2; row 1 m2 send_managed_0000_bootstrap.up.sql 20 42501 BLOCKER; row 2 m2 send_managed_0000_bootstrap.up.sql 53 42P01 CASCADE
eq statement1 "$(dcol 1 7)" $'DO $$\nBEGIN\n    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = \'send_managed_owner\') THEN\n        CREATE ROLE send_managed_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;\n    END IF;\nEND $$'
eq statement2 "$(dcol 2 7)" "INSERT INTO send_managed.schema_migrations (version) VALUES ('0000');"
eq cr-in-records "$(tr -cd '\r' < "$TMP/out.tsv" | wc -c | tr -d ' ')" 0; finish

begin "several failures with NOTICEs between them: records do not merge and a NOTICE is not appended to the statement before it"
parse multi m2; eq rows "$(nrows)" 3
row 1 m2 m.sql 9 42501 BLOCKER; row 2 m2 m.sql 12 42704 CASCADE; row 3 m2 m.sql 20 42501 BLOCKER
eq stmt1 "$(dcol 1 7)" 'CREATE ROLE a LOGIN;'; eq stmt2 "$(dcol 2 7)" 'GRANT a TO b;'; eq stmt3 "$(dcol 3 7)" 'ALTER TABLE t OWNER TO a;'
eq detail1 "$(dcol 1 8)" 'LOCATION:  CreateRole, user.c:318'; eq blockers "$(stat blockers)" 2; eq cascades "$(stat cascades)" 1; finish

begin "an error with NO STATEMENT echo (psql run without -b): a record with an empty statement, still classified"
parse nostatement m2; eq rows "$(nrows)" 1; row 1 m2 n.sql 8 42501 BLOCKER; eq statement "$(dcol 1 7)" ''; eq orphans "$(stat orphan_statements)" 0; finish

begin "no SQLSTATE on the line (VERBOSITY not honoured): sqlstate ----- and the class comes from the message alone"
parse nosqlstate m2; eq rows "$(nrows)" 2; row 1 m2 x.sql 3 ----- BLOCKER; row 2 m2 x.sql 7 ----- CASCADE
eq message1 "$(dcol 1 6)" 'permission denied to create extension "pgcrypto"'; finish

begin "the DETAIL line decides when the message alone would not (SQLSTATE XX000, 'Only roles with the ADMIN option')"
parse detail_only m2; row 1 m2 d.sql 4 XX000 BLOCKER; finish

begin "file names: a relative path, a Windows D:/ path (with a colon) and a backslash path all reduce to the BASENAME"
parse paths m1; eq rows "$(nrows)" 3
row 1 m1 platform_0000_bootstrap.up.sql 44 42501 BLOCKER; row 2 m1 send_byo_0012_grants_and_rls.up.sql 31 42501 BLOCKER; row 3 m1 0016_verify_schema.sql 2 42501 BLOCKER; finish

begin "a TAB and a % inside the statement round-trip exactly, and the record stays 8 columns"
parse tabs m2; eq rows "$(nrows)" 1; eq statement "$(dcol 1 7)" "$S_TAB"; eq columns "$(allcols8)" 0; row 1 m2 t.sql 2 22012 CASCADE; finish

begin "an echoed STATEMENT with no error before it is an ORPHAN: no record, counted, and its continuation line is not read as an error"
parse orphan m2; eq rows "$(nrows)" 0; eq orphans "$(stat orphan_statements)" 1; eq unattributed "$(stat unattributed_error_lines)" 0; has orphan-line "$(cat "$TMP/stats")" 'orphan_statement: psql:o.sql:9: STATEMENT:'; finish

begin "a STATEMENT echo WITHOUT the psql:FILE:LINE: prefix, right after the error's own lines, is accepted (tolerated second shape); its continuation line is kept"
parse unprefixed_stmt m2; eq rows "$(nrows)" 1; row 1 m2 u.sql 7 42501 BLOCKER; eq statement "$(dcol 1 7)" $'CREATE ROLE a\n    LOGIN;'
eq detail "$(dcol 1 8)" 'LOCATION:  CreateRole, user.c:318'; eq orphans "$(stat orphan_statements)" 0; eq unattributed "$(stat unattributed_error_lines)" 0; finish

begin "a STATEMENT echo whose FILE:LINE differs from the error before it is an ORPHAN, and the error stays a record with no statement"
parse mismatch m2; eq rows "$(nrows)" 1; eq statement "$(dcol 1 7)" ''; eq orphans "$(stat orphan_statements)" 1; finish

begin "psql's own failure (cannot connect) is counted as psql_errors and is not a record"
parse psqlerr m2; eq rows "$(nrows)" 0; eq psql_errors "$(stat psql_errors)" 1; eq unattributed "$(stat unattributed_error_lines)" 0; finish

begin "an ERROR line with no psql: prefix is UNATTRIBUTED (counted), while 'ERROR:  ' inside a statement text is not"
parse stray m2; eq rows "$(nrows)" 1; eq unattributed "$(stat unattributed_error_lines)" 1; row 1 m2 y.sql 5 P0001 CASCADE
eq statement "$(dcol 1 7)" "DO \$\$ BEGIN RAISE EXCEPTION 'ERROR:  boom'; END \$\$"; finish

begin "a FATAL line (connection lost mid-file) is a record, and psql's following 'psql: error:' line is counted separately"
parse fatal m2; eq rows "$(nrows)" 1; row 1 m2 f.sql 3 57P01 CASCADE; eq psql_errors "$(stat psql_errors)" 1; finish

begin "stdin: the same records as reading the file (no argument, and the explicit -)"
a="$(bash "$CLS" --chain m2 < "$FX/multi")"; b="$(bash "$CLS" --chain m2 - < "$FX/multi")"; c="$(bash "$CLS" --chain m2 "$FX/multi")"
eq stdin-vs-file "$a" "$c"; eq dash-vs-file "$b" "$c"; has non-empty "$a" 'm.sql'; finish

echo "classify-selftest: CLASSIFICATION — the message regex alone decides when the SQLSTATE is not 42501 (synthetic SQLSTATE XX000)"
msgcase() { # <BLOCKER|CASCADE> <message>
  printf '%s\n' "psql:r.sql:1: ERROR:  XX000: $2" "psql:r.sql:1: STATEMENT:  select 1;" > "$FX/msg"
  begin "XX000 '$2' -> $1"; parse msg m2; eq rows "$(nrows)" 1; row 1 m2 r.sql 1 XX000 "$1"; finish
}
msgcase BLOCKER 'must be superuser to create this extension'
msgcase BLOCKER 'permission denied to create extension "pgcrypto"'
msgcase BLOCKER 'must have CREATEROLE privilege to create roles'
msgcase BLOCKER 'must have admin option on role "x"'
msgcase BLOCKER 'must have the SET option on role "x"'
msgcase BLOCKER 'must be owner of table platform.schema_migrations'
msgcase BLOCKER 'must be able to SET ROLE "platform_owner"'
msgcase BLOCKER '"supabase_admin" is a reserved role, only superusers can modify it'
msgcase BLOCKER 'permission denied for schema public'
msgcase BLOCKER 'permission denied for database postgres'
msgcase BLOCKER 'permission denied for function pg_ls_dir'
msgcase BLOCKER 'permission denied for language c'
msgcase BLOCKER 'permission denied for tablespace pg_default'
msgcase CASCADE 'relation "x" does not exist'
msgcase CASCADE 'role "x" does not exist'
msgcase CASCADE 'extension "pg_cron" is not available'
msgcase CASCADE 'type "t" already exists'
msgcase CASCADE 'permission denied for table users'      # a real one carries 42501; the regex deliberately lists no table, so the SQLSTATE rule is what catches it

echo "classify-selftest: COMPARE — the BLOCKER set must equal the committed list"
recs="$TMP/recs.tsv"
{ bash "$CLS" --chain platform "$FX/set_role"; bash "$CLS" --chain m2 "$FX/create_role"; bash "$CLS" --chain m2 "$FX/default_priv"; bash "$CLS" --chain m2 "$FX/cascade"; } > "$recs"
EXACT=$'platform_0000_bootstrap.up.sql|42501|must be able to SET ROLE\nsend_managed_0000_bootstrap.up.sql|42501|permission denied to create role\nsend_managed_0000_bootstrap.up.sql|42501|must be member of role\n'
K="$TMP/known-blockers.txt"
COUT=""; CRC=0
cmp_() { COUT="$(bash "$CLS" --compare "$1" --chains "${3:-platform,m1,m2,m3}" "$2" 2>&1)"; CRC=$?; }       # cmp_ KNOWN RECORDS [CHAINS]
verdict() { # NEW STALE MALFORMED DUPLICATE
  has "verdict line" "$COUT" "compare: RED - NEW=$1 STALE=$2 MALFORMED=$3 DUPLICATE=$4 BADROWS=0"
  eq "NEW BLOCKER lines" "$(printf '%s\n' "$COUT" | grep -c '^NEW BLOCKER ' || true)" "$1"
  eq "STALE ENTRY lines" "$(printf '%s\n' "$COUT" | grep -c '^STALE ENTRY ' || true)" "$2"
}

begin "EXACT MATCH: three blockers listed, three observed (one cascade present and not judged) -> OK, rc 0"
printf '%s' "$EXACT" > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 0; has verdict "$COUT" 'compare: OK - the 3 observed blocker(s) (3 distinct (file, SQLSTATE, message)) are exactly the 3 listed entries; 1 cascade(s)'
hasnot nothing-red "$COUT" 'NEW BLOCKER'; hasnot nothing-stale "$COUT" 'STALE ENTRY'; finish

begin "the known file may carry comments, blank lines, indented comments, padding around the bars and CRLF endings"
{ printf '# header\r\n\r\n   # indented comment\r\n'; printf '  platform_0000_bootstrap.up.sql | 42501 | must be able to SET ROLE  \r\n'; printf '\t\r\n'
  printf 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role\r\nsend_managed_0000_bootstrap.up.sql|42501|must be member of role\r\n'; } > "$K"
eq known-has-CR "$([ "$(tr -cd '\r' < "$K" | wc -c | tr -d ' ')" -gt 0 ] && echo yes || echo no)" yes
cmp_ "$K" "$recs"; eq rc "$CRC" 0; has verdict "$COUT" 'compare: OK - the 3 observed blocker(s)'; finish

begin "NEW BLOCKER: an observed blocker that is not listed is red, naming file, SQLSTATE and the start of the statement"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42501|must be able to SET ROLE' 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role' > "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1
has file "$COUT" 'NEW BLOCKER file=send_managed_0000_bootstrap.up.sql line=47 sqlstate=42501 chain=m2'
has message "$COUT" 'message="must be member of role "send_managed_owner""'
has statement "$COUT" 'statement: ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed GRANT SELECT'
verdict 1 0 0 0; finish

begin "STALE ENTRY: a listed blocker that is no longer observed is red, naming its line in the known file"
printf '%s' "$EXACT" > "$K"; printf '%s\n' 'send_byo_0012_grants_and_rls.up.sql|42501|permission denied for schema' >> "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; has stale "$COUT" 'STALE ENTRY known-blockers.txt line 4: send_byo_0012_grants_and_rls.up.sql|42501|permission denied for schema - no observed blocker'
has reason "$COUT" 'it no longer fails, or its message changed'; verdict 0 1 0 0; finish

begin "a listed line whose SQLSTATE is wrong is BOTH a stale entry and leaves the real blocker new"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42P01|must be able to SET ROLE' 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role' 'send_managed_0000_bootstrap.up.sql|42501|must be member of role' > "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; has new "$COUT" 'NEW BLOCKER file=platform_0000_bootstrap.up.sql line=44 sqlstate=42501'; has stale "$COUT" 'STALE ENTRY known-blockers.txt line 1: platform_0000_bootstrap.up.sql|42P01|'
verdict 1 1 0 0; finish

begin "a listed line whose message substring no longer occurs (the message changed) is NEW + STALE"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42501|must be able to SET ROLE' 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role' 'send_managed_0000_bootstrap.up.sql|42501|must be a member of the role' > "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; verdict 1 1 0 0; has stale-line "$COUT" 'line 3: send_managed_0000_bootstrap.up.sql|42501|must be a member of the role'; finish

begin "a listed line for a file name that differs only by a suffix does not cover the observed blocker"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42501|must be able to SET ROLE' 'send_managed_0000_bootstrap.down.sql|42501|permission denied to create role' 'send_managed_0000_bootstrap.up.sql|42501|must be member of role' > "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; verdict 1 1 0 0; has new "$COUT" 'line=20 sqlstate=42501'; finish

begin "a CASCADE is listed but not judged: listing one in the known file is a STALE ENTRY (only blockers are listed)"
printf '%s' "$EXACT" > "$K"; printf '%s\n' 'send_managed_0000_bootstrap.up.sql|42P01|does not exist' >> "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; verdict 0 1 0 0; finish

begin "one listed line covers every observed blocker with the same file, SQLSTATE and message (the sets are compared, not the counts)"
{ bash "$CLS" --chain m2 "$FX/create_role"; bash "$CLS" --chain m2 "$FX/create_role"; } > "$TMP/twice.tsv"
printf '%s\n' 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role' > "$K"
cmp_ "$K" "$TMP/twice.tsv"; eq rc "$CRC" 0; has verdict "$COUT" 'compare: OK - the 2 observed blocker(s) (1 distinct (file, SQLSTATE, message)) are exactly the 1 listed entry'; finish

begin "EMPTY list + blockers observed: every blocker is NEW (the first run of the job)"
printf '# no entries yet\n\n' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; verdict 3 0 0 0; finish

begin "EMPTY list + a zero-byte records file: nothing blocks, nothing is listed -> OK (the goal state)"
: > "$K"; : > "$TMP/none.tsv"; cmp_ "$K" "$TMP/none.tsv"; eq rc "$CRC" 0; has verdict "$COUT" 'compare: OK - the 0 observed blocker(s) (0 distinct (file, SQLSTATE, message)) are exactly the 0 listed entries'; finish

begin "EMPTY list + records that are all cascades -> OK"
: > "$K"; bash "$CLS" --chain m2 "$FX/cascade" > "$TMP/casc.tsv"; cmp_ "$K" "$TMP/casc.tsv"; eq rc "$CRC" 0; has verdict "$COUT" '1 cascade(s)'; finish

begin "chain filter: a blocker of the 'fixture' chain is not judged by default, and is judged when asked for"
: > "$K"; bash "$CLS" --chain fixture "$FX/create_role" > "$TMP/fixture.tsv"
cmp_ "$K" "$TMP/fixture.tsv"; eq rc-default "$CRC" 0; cmp_ "$K" "$TMP/fixture.tsv" fixture; eq rc-asked "$CRC" 1; verdict 1 0 0 0; finish

begin "MALFORMED: a line with no bar"
printf '%s\n' 'platform_0000_bootstrap.up.sql 42501 must be able' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1
has reason "$COUT" 'MALFORMED KNOWN-BLOCKER LINE 1 (known-blockers.txt): expected FILE|SQLSTATE|MESSAGE-SUBSTRING, found no bar'; has verdict "$COUT" 'MALFORMED=1'; finish
begin "MALFORMED: a line with only two fields"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42501' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'found only two fields'; has verdict "$COUT" 'MALFORMED=1'; finish
begin "MALFORMED: FILE given as a path instead of a basename"
printf '%s\n' 'db/migrations/platform_0000_bootstrap.up.sql|42501|SET ROLE' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'FILE must be a basename, got db/migrations/platform_0000_bootstrap.up.sql'; finish
begin "MALFORMED: a SQLSTATE that is not five characters of 0-9 A-Z"
printf '%s\n' 'platform_0000_bootstrap.up.sql|4250|SET ROLE' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'SQLSTATE must be five characters from 0-9 A-Z, got 4250'; finish
begin "MALFORMED: a lower-case SQLSTATE is rejected (the classifier writes them upper-case)"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42p01|SET ROLE' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'got 42p01'; finish
begin "MALFORMED: an empty MESSAGE-SUBSTRING would cover everything and is rejected"
printf '%s\n' 'platform_0000_bootstrap.up.sql|42501|   ' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'empty MESSAGE-SUBSTRING: an entry must say what fails'; finish
begin "MALFORMED: an empty FILE"
printf '%s\n' '|42501|SET ROLE' > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'MALFORMED KNOWN-BLOCKER LINE 1 (known-blockers.txt): empty FILE'; finish
begin "DUPLICATE: the same entry twice is red, naming both lines"
printf '%s' "$EXACT" > "$K"; printf '%s\n' 'send_managed_0000_bootstrap.up.sql|42501|permission denied to create role' >> "$K"
cmp_ "$K" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'DUPLICATE ENTRY line 4 of known-blockers.txt repeats line 2: send_managed_0000_bootstrap.up.sql|42501|permission denied to create role'; has verdict "$COUT" 'DUPLICATE=1'; finish
begin "the substring may itself contain a bar (everything after the second bar is the substring)"
printf 'psql:a.sql:3: ERROR:  42501: role a | b denied\n' > "$FX/bar"; bash "$CLS" --chain m2 "$FX/bar" > "$TMP/bar.tsv"
printf '%s\n' 'a.sql|42501|a | b denied' > "$K"; cmp_ "$K" "$TMP/bar.tsv"; eq rc "$CRC" 0; finish
begin "a records file with a short row is red: BADROWS, never silently skipped"
printf 'm2\tonly\tthree\n' > "$TMP/short.tsv"; : > "$K"; cmp_ "$K" "$TMP/short.tsv"; eq rc "$CRC" 1; has reason "$COUT" 'MALFORMED RECORD ROW 1 of the records file: 3 columns, want 8'; has verdict "$COUT" 'BADROWS=1'; finish
begin "the known file is MISSING: red, never read as an empty list"
cmp_ "$TMP/does-not-exist.txt" "$recs"; eq rc "$CRC" 1; has reason "$COUT" 'compare: RED - the known-blockers file is missing'; has reason2 "$COUT" 'never read as an empty one'; finish

echo "classify-selftest: OUTPUT MODES"
begin "--summary prints every named chain, a chain with no failure as 0"
S="$(bash "$CLS" --summary --chains platform,m1,m2,m3 "$recs" | tr -s ' ')"      # column padding squeezed so the checks do not depend on the widths
eq lines "$(printf '%s\n' "$S" | wc -l | tr -d ' ')" 4
has platform "$S" ' platform failures=1 blockers=1 cascades=0'; has m1 "$S" ' m1 failures=0 blockers=0 cascades=0'
has m2 "$S" ' m2 failures=3 blockers=2 cascades=1'; has m3 "$S" ' m3 failures=0 blockers=0 cascades=0'; finish

begin "--suggest prints the distinct blockers as known-blockers.txt lines (the full message), ready to paste, and they pass --compare"
S="$(bash "$CLS" --suggest --chains platform,m1,m2,m3 "$recs")"
eq suggested "$S" $'platform_0000_bootstrap.up.sql|42501|must be able to SET ROLE "platform_owner"\nsend_managed_0000_bootstrap.up.sql|42501|permission denied to create role\nsend_managed_0000_bootstrap.up.sql|42501|must be member of role "send_managed_owner"'
printf '%s\n' "$S" > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 0; finish

begin "--table lists every failure with its class, file:line, SQLSTATE and message; a failure-free selection prints '(no failures)'"
T="$(bash "$CLS" --table --chains platform,m1,m2,m3 "$recs" | tr -s ' ')"
has row1 "$T" ' [platform] platform_0000_bootstrap.up.sql:44 42501 BLOCKER ALTER TABLE platform.schema_migrations OWNER TO platform_owner;'
has row-cascade "$T" ' [m2] send_managed_0000_bootstrap.up.sql:53 42P01 CASCADE INSERT INTO send_managed.schema_migrations'
has message "$T" 'message: must be member of role "send_managed_owner"'
has multi-line-collapsed "$T" 'ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO send_managed_app;'
eq rows "$(printf '%s\n' "$T" | grep -c 'message: ')" 4
eq empty "$(bash "$CLS" --table --chains m3 "$recs")" '  (no failures)'; finish

begin "--table shows the first 160 characters of a long statement, then '...'"
parse long m2; bash "$CLS" --table "$TMP/out.tsv" > "$TMP/table.txt"
has first160 "$(cat "$TMP/table.txt")" "SELECT '$(printf 'x%.0s' $(seq 1 152))..."
eq clipped-length "$(grep -o "SELECT 'x*\.\.\." "$TMP/table.txt" | tr -d '\n' | wc -c | tr -d ' ')" 163; finish

begin "--decode undoes the escaping of all four characters, in one pass, and leaves a literal %0A that was escaped as %250A alone"
eq decoded "$(printf '%s\n' 'a%0Ab%09c%0Dd%25e%250Af' | bash "$CLS" --decode | tr '\r' '~' | tr '\t' '^' | tr '\n' '|')" 'a|b^c~d%e%0Af|'; finish

echo "classify-selftest: USAGE ERRORS (exit 2, with the reason)"
usage_() { # label reason args...
  local label="$1" want="$2" out rc; shift 2
  out="$(bash "$CLS" "$@" 2>&1 < /dev/null)"; rc=$?
  begin "usage: $label"; eq rc "$rc" 2; has reason "$out" "$want"; finish
}
usage_ "an unknown flag"                          'classify: unknown argument: --bogus'           --bogus
usage_ "parse of a file that does not exist"      'classify: input file missing: '                "$TMP/no-such-file"
usage_ "two inputs"                               'classify: more than one input given'           "$FX/multi" "$FX/clean"
usage_ "--compare without the known file"         'classify: --compare needs the known-blockers file' --compare
usage_ "--compare without a records file"         'classify: this mode needs a records file argument' --compare "$K"
usage_ "--compare with a records file that is gone" 'classify: records file missing: '            --compare "$K" "$TMP/no-such-records"
usage_ "--summary without --chains"               'classify: --summary needs --chains'            --summary "$recs"
usage_ "--chains without a value"                 'classify: --chains needs a comma-separated list' --chains
usage_ "--stats without a value"                  'classify: --stats needs a file'                --stats

echo "classify-selftest: RUN.SH — options and control flow, driven with a FAKE psql over a miniature module tree (no database)"
RUN="$HERE/run.sh"
FAKEBIN="$TMP/fakebin"; FAKE="$TMP/fake"; TREE="$TMP/tree"; EMPTYK="$TMP/empty-known.txt"; PREP="$TMP/preprovision.sql"
mkdir -p "$FAKEBIN"; : > "$EMPTYK"; echo 'select 1;' > "$PREP"
# the fake: answers the scalar queries run.sh asks, logs every `-f FILE` call as  FILE|-v options|flags|DSN, prints FILE.err to stderr and exits with FILE.rc if they exist in $FAKE_DIR
cat > "$FAKEBIN/psql" <<'FAKEPSQL'
#!/usr/bin/env bash
D="${FAKE_DIR:?}"; cmd=""; file=""; vs=""; fl=""; dsn=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c) cmd="$2"; shift 2;;
    -f) file="$2"; shift 2;;
    -v) vs="$vs $2"; shift 2;;
    -F) shift 2;;
    --version) echo "psql (PostgreSQL) 17.0 (fake)"; exit 0;;
    -*) fl="$fl $1"; shift;;
    *) dsn="$1"; shift;;
  esac
done
if [ -n "$cmd" ]; then
  case "$cmd" in
    "select 1") echo 1;;
    "select current_user") echo postgres;;
    "select rolsuper, rolcreaterole from pg_roles where rolname = current_user") if [ -f "$D/shape" ]; then cat "$D/shape"; else echo "f|t"; fi;;
    "select to_regclass('public.users') is not null, to_regclass('public.job_items') is not null") echo "t|t";;
    *) echo " (a fact the fake does not model)";;
  esac
  exit 0
fi
b="$(basename "$file")"
echo "$b|$vs|$fl|$dsn" >> "$D/calls.log"
if [ -f "$D/$b.err" ]; then cat "$D/$b.err" >&2; fi
if [ -f "$D/$b.rc" ]; then exit "$(cat "$D/$b.rc")"; fi
exit 0
FAKEPSQL
chmod +x "$FAKEBIN/psql"
fake_reset() {   # a pristine fake: the control file's real-shaped output, and the miniature tree of one file per chain
  rm -rf "$FAKE" "$TREE"; mkdir -p "$FAKE" "$TREE/coldmail-platform/db/migrations" "$TREE/coldmail-verify/control-plane/migrations" \
    "$TREE/coldmail-send-managed/db/migrations" "$TREE/coldmail-send-managed/db/preflight" "$TREE/coldmail-send-managed/deploy/pg-init" \
    "$TREE/coldmail-send-byo/db/migrations" "$TREE/coldmail-send-byo/db/preflight"
  for f in coldmail-platform/db/migrations/platform_0000_bootstrap.up.sql coldmail-verify/control-plane/migrations/0001_init.sql \
           coldmail-send-managed/db/migrations/send_managed_0000_bootstrap.up.sql coldmail-send-byo/db/migrations/send_byo_0000_bootstrap.up.sql \
           coldmail-send-managed/db/preflight/check_platform_tables.sql coldmail-send-byo/db/preflight/check_platform_tables.sql \
           coldmail-send-managed/deploy/pg-init/00-platform-fixtures.sql; do echo 'select 1;' > "$TREE/$f"; done
  cat > "$FAKE/zz_instrument_control.sql.err" <<'EOF'
psql:zz_instrument_control.sql:2: ERROR:  42501: permission denied to create role
LOCATION:  CreateRole, user.c:318
psql:zz_instrument_control.sql:2: STATEMENT:  CREATE ROLE zz_instrument_probe SUPERUSER;
psql:zz_instrument_control.sql:5: ERROR:  42P01: relation "zz_instrument_probe_missing_table" does not exist
LINE 2:   FROM zz_instrument_probe_missing_table
               ^
LOCATION:  parserOpenTable, parse_relation.c:1449
psql:zz_instrument_control.sql:5: STATEMENT:  SELECT 1
  FROM zz_instrument_probe_missing_table
 WHERE true;
EOF
}
RN=0; ROUT=""; RRC=0
runsh() {        # runsh [extra run.sh args]  -> ROUT (stdout+stderr), RRC
  RN=$((RN + 1))
  ROUT="$(PATH="$FAKEBIN:$PATH" FAKE_DIR="$FAKE" S_WORK="$TMP/w$RN" S_WAIT_SECONDS=5 bash "$RUN" --dsn postgres://postgres:ci@localhost:5432/postgres \
          --platform-dir "$TREE/coldmail-platform" --modules-root "$TREE" "$@" 2>&1 < /dev/null)"; RRC=$?
}
ncalls() { if [ -f "$FAKE/calls.log" ]; then grep -c -- "$1" "$FAKE/calls.log" || true; else echo 0; fi; }   # how many logged psql -f calls match
callno() { if [ -f "$FAKE/calls.log" ]; then grep -n -- "$1" "$FAKE/calls.log" | head -1 | cut -d: -f1; fi; }   # line number (order) of the first match

begin "run.sh --label: every banner and the result line carry the label; no provisioning is reported as 'none'; a clean tree is GREEN, rc 0"
fake_reset; runsh --label FIXED --known "$EMPTYK"; eq rc "$RRC" 0
has banner-a "$ROUT" 'FIXED [a] INSTRUMENT VALID'; has banner-b "$ROUT" 'FIXED [b] PRE-FLIGHT'; has banner-p "$ROUT" 'FIXED [p] PROVISIONING'; has banner-c "$ROUT" 'FIXED [c] CHAINS'; has banner-d "$ROUT" 'FIXED [d] COMPARE'
has result "$ROUT" 'SUPABASE-MIGRATIONS RESULT [FIXED]: GREEN'; has label-line "$ROUT" 'pass label:     FIXED'; has none "$ROUT" 'preprovision:   none'
hasnot unlabelled-result "$ROUT" 'RESULT: GREEN'; eq provision-calls "$(ncalls '^preprovision')" 0; finish

begin "run.sh without --label: banners and result line are exactly the unlabelled ones"
fake_reset; runsh --known "$EMPTYK"; eq rc "$RRC" 0
has banner "$ROUT" '================ [a] INSTRUMENT VALID'; has result "$ROUT" 'SUPABASE-MIGRATIONS RESULT: GREEN'; has label-line "$ROUT" 'pass label:     (none)'; finish

begin "run.sh --preprovision FILE: applied once, with ON_ERROR_STOP=1, AFTER the fixture and BEFORE the first chain file; chain files keep ON_ERROR_STOP=0 and -b; the DSN's database is the one used"
fake_reset; runsh --label FIXED --preprovision "$PREP" --known "$EMPTYK" --dsn postgres://postgres:ci@localhost:5432/fixed; eq rc "$RRC" 0
eq provision-calls "$(ncalls '^preprovision.sql|')" 1; has on-error-stop "$(grep '^preprovision.sql|' "$FAKE/calls.log")" ' ON_ERROR_STOP=1|'
fx="$(callno '^00-platform-fixtures.sql|')"; pv="$(callno '^preprovision.sql|')"; pf="$(callno '^check_platform_tables.sql|')"; ch="$(callno '^platform_0000_bootstrap.up.sql|')"
eq order-fixture-before-provision "$([ "$fx" -lt "$pv" ] && echo yes || echo no)" yes; eq order-preflight-before-provision "$([ "$pf" -lt "$pv" ] && echo yes || echo no)" yes
eq order-provision-before-chain "$([ "$pv" -lt "$ch" ] && echo yes || echo no)" yes
has chain-flags "$(grep '^platform_0000_bootstrap.up.sql|' "$FAKE/calls.log")" 'ON_ERROR_STOP=0 VERBOSITY=verbose| -X -w -b|'
eq all-calls-fixed-db "$(grep -c '/fixed$' "$FAKE/calls.log")" "$(grep -c . "$FAKE/calls.log")"; eq no-call-in-postgres-db "$(grep -c '/postgres$' "$FAKE/calls.log" || true)" 0
has banner "$ROUT" 'FIXED [p] PROVISIONING'; has applied "$ROUT" 'preprovision.sql applied (psql exit 0, ON_ERROR_STOP=1)'; has result "$ROUT" 'RESULT [FIXED]: GREEN'; finish

begin "run.sh --preprovision that FAILS (psql exit 3): RED at stage [provision], reason PROVISIONING FAILED, its error shown, and NO chain file is run"
fake_reset; echo 3 > "$FAKE/preprovision.sql.rc"
printf '%s\n' 'psql:preprovision.sql:12: ERROR:  42501: permission denied to grant role "send_byo_owner"' > "$FAKE/preprovision.sql.err"
runsh --label FIXED --preprovision "$PREP" --known "$EMPTYK"; eq rc "$RRC" 1
has reason "$ROUT" 'PROVISIONING FAILED: preprovision.sql did not apply cleanly (psql exit 3)'; has stage "$ROUT" 'SUPABASE-MIGRATIONS RED [FIXED] at stage [provision]'; has shown "$ROUT" 'permission denied to grant role "send_byo_owner"'
eq no-platform-chain "$(ncalls '^platform_0000')" 0; eq no-m2-chain "$(ncalls '^send_managed_0000')" 0; eq no-m3-chain "$(ncalls '^send_byo_0000')" 0; hasnot no-result-green "$ROUT" ': GREEN'; finish

begin "run.sh --preprovision naming a file that does not exist: RED at stage [inputs], saying a named and missing provisioning step is never skipped; nothing is run"
fake_reset; runsh --label FIXED --preprovision "$TMP/no-such-provision.sql" --known "$EMPTYK"; eq rc "$RRC" 1
has reason "$ROUT" 'is not a file: a provisioning step that is named and missing is red, never skipped'; has stage "$ROUT" 'RED [FIXED] at stage [inputs]'; eq no-calls "$(ncalls .)" 0; finish

begin "run.sh --label with a character outside letters, digits, . _ -: usage error, exit 2, with the reason"
fake_reset; runsh --label 'a b' --known "$EMPTYK"; eq rc "$RRC" 2; has reason "$ROUT" "run.sh: invalid --label 'a b': letters, digits, . _ - only"; eq no-calls "$(ncalls .)" 0; finish

begin "run.sh --preprovision / --label given without a value: usage error, exit 2"
fake_reset; runsh --label; eq rc-label "$RRC" 2; has usage-label "$ROUT" 'usage: run.sh --dsn DSN --platform-dir DIR --modules-root DIR [--known FILE] [--preprovision FILE] [--label NAME]'
runsh --preprovision; eq rc-prov "$RRC" 2; has usage-prov "$ROUT" 'usage: run.sh'; finish

begin "run.sh: a blocker in a chain file is NEW against an empty list: RED, rc 1, NEW BLOCKER naming file and SQLSTATE, the result line carries the label, the paste-ready line is printed"
fake_reset
cat > "$FAKE/send_managed_0000_bootstrap.up.sql.err" <<'EOF'
psql:send_managed_0000_bootstrap.up.sql:26: ERROR:  42501: must be able to SET ROLE "send_managed_owner"
LOCATION:  check_can_set_role, acl.c:5237
psql:send_managed_0000_bootstrap.up.sql:26: STATEMENT:  CREATE SCHEMA IF NOT EXISTS send_managed AUTHORIZATION send_managed_owner;
EOF
runsh --label BASELINE --known "$EMPTYK"; eq rc "$RRC" 1
has new "$ROUT" 'NEW BLOCKER file=send_managed_0000_bootstrap.up.sql line=26 sqlstate=42501 chain=m2'; has result "$ROUT" 'SUPABASE-MIGRATIONS RESULT [BASELINE]: RED'
has verdict "$ROUT" 'compare: RED - NEW=1 STALE=0'; has paste "$ROUT" 'send_managed_0000_bootstrap.up.sql|42501|must be able to SET ROLE "send_managed_owner"'; finish

begin "run.sh --known selects the list of THIS pass: the same blocker is covered by a list that names it (GREEN, rc 0) and is NEW against an empty one (the two passes of the workflow)"
printf '%s\n' '# baseline list' 'send_managed_0000_bootstrap.up.sql|42501|must be able to SET ROLE' > "$TMP/baseline-known.txt"
runsh --label BASELINE --known "$TMP/baseline-known.txt"; eq rc "$RRC" 0; has ok "$ROUT" 'compare: OK - the 1 observed blocker(s)'; has result "$ROUT" 'RESULT [BASELINE]: GREEN'; finish

begin "run.sh: a STALE entry in the pass's list (the listed blocker is not observed): RED, rc 1, STALE ENTRY"
fake_reset; printf '%s\n' 'send_managed_0000_bootstrap.up.sql|42501|must be able to SET ROLE' > "$TMP/stale-known.txt"
runsh --label FIXED --known "$TMP/stale-known.txt"; eq rc "$RRC" 1; has stale "$ROUT" 'STALE ENTRY stale-known.txt line 1: send_managed_0000_bootstrap.up.sql|42501|must be able to SET ROLE'; has verdict "$ROUT" 'compare: RED - NEW=0 STALE=1'; finish

begin "run.sh: psql exiting 1 on a chain file is RED (a failed statement exits 0; a failed psql does not)"
fake_reset; echo 1 > "$FAKE/platform_0000_bootstrap.up.sql.rc"; runsh --known "$EMPTYK"; eq rc "$RRC" 1; has reason "$ROUT" 'platform/platform_0000_bootstrap.up.sql: psql exited 1 (a failed statement still exits 0 with ON_ERROR_STOP=0'; finish

begin "run.sh: a superuser migrating role is RED at stage [instrument]: not Supabase-shaped, and nothing is run"
fake_reset; echo "t|t" > "$FAKE/shape"; runsh --label BASELINE --known "$EMPTYK"; eq rc "$RRC" 1
has reason "$ROUT" 'not Supabase-shaped: the migrating role is a superuser'; has stage "$ROUT" 'RED [BASELINE] at stage [instrument]'; eq no-calls "$(ncalls .)" 0; finish

begin "run.sh: a control file whose psql output the classifier cannot read (unprefixed error) is RED: INSTRUMENT INVALID"
fake_reset; printf '%s\n' 'ERROR:  42501: permission denied to create role' > "$FAKE/zz_instrument_control.sql.err"; runsh --known "$EMPTYK"; eq rc "$RRC" 1
has reason "$ROUT" 'INSTRUMENT INVALID: the classifier did not read psql'; has why "$ROUT" 'carry no psql:FILE:LINE: prefix'; finish

begin "run.sh: a failing fixture is REPORTED and never counted as a blocker (the pass stays GREEN when the chains are clean)"
fake_reset
cat > "$FAKE/00-platform-fixtures.sql.err" <<'EOF'
psql:00-platform-fixtures.sql:12: ERROR:  42501: permission denied for schema public
psql:00-platform-fixtures.sql:12: STATEMENT:  CREATE TABLE IF NOT EXISTS public.users (id uuid PRIMARY KEY);
EOF
runsh --label FIXED --known "$EMPTYK"; eq rc "$RRC" 0; has reported "$ROUT" 'fixture: 1 statement(s) FAILED (reported, not counted as blockers)'; has ok "$ROUT" 'compare: OK - the 0 observed blocker(s)'; finish

begin "run.sh: a chain that matches no migration file is RED (the instrument matched nothing)"
fake_reset; rm -f "$TREE"/coldmail-send-byo/db/migrations/*.up.sql; runsh --known "$EMPTYK"; eq rc "$RRC" 1; has reason "$ROUT" 'chain m3 matched 0 migration files: the instrument matched nothing'; finish

echo "classify-selftest: the workflow and preprovision.sql are the two-pass design they claim to be"
WF="$HERE/../../.github/workflows/supabase.yml"; PV="$HERE/preprovision.sql"
step_block() { awk -v start="$1" 'index($0, "- name: " start) { on = 1; print; next } on && /^      - / { exit } on { print }' "$WF"; }   # one step of the workflow, from its `- name:` line to the next step
BASE_BLOCK="$(step_block 'Pass BASELINE')"; FIXED_BLOCK="$(step_block 'Pass FIXED')"
begin "supabase.yml: exactly ONE step has an if:, it is 'if: always()', and it is the FIXED pass (the baseline pass has none; the job fails if either failed)"
eq workflow-exists "$([ -f "$WF" ] && echo yes || echo no)" yes
eq if-count "$(grep -c '^ *if:' "$WF" || true)" 1; eq if-always "$(grep '^ *if:' "$WF" | sed 's/^ *//; s/ *$//')" 'if: always()'
has if-in-fixed "$FIXED_BLOCK" 'if: always()'; hasnot if-in-baseline "$BASE_BLOCK" 'if:'; has baseline-found "$BASE_BLOCK" 'Pass BASELINE'; finish

begin "supabase.yml: two distinct step names; BASELINE = database postgres, baseline-blockers.txt, NO --preprovision; FIXED = database fixed, --preprovision preprovision.sql, known-blockers.txt"
eq distinct-names "$(grep -c '^      - name: Pass ' "$WF")" 2
has b-label "$BASE_BLOCK" '--label BASELINE'; has b-db "$BASE_BLOCK" 'localhost:5432/postgres'; has b-known "$BASE_BLOCK" '--known coldmail-platform/tests/supabase/baseline-blockers.txt'; hasnot b-no-prov "$BASE_BLOCK" '--preprovision'
has f-label "$FIXED_BLOCK" '--label FIXED'; has f-db "$FIXED_BLOCK" 'localhost:5432/fixed'; has f-prov "$FIXED_BLOCK" '--preprovision coldmail-platform/tests/supabase/preprovision.sql'; has f-known "$FIXED_BLOCK" '--known coldmail-platform/tests/supabase/known-blockers.txt'
hasnot f-not-baseline-list "$FIXED_BLOCK" 'baseline-blockers.txt'; finish

begin "supabase.yml: pass FIXED asserts rolcreatedb BEFORE it creates the database fixed (and creates it itself, so the step also works when BASELINE failed)"
has asserts "$FIXED_BLOCK" 'select rolcreatedb'; has creates "$FIXED_BLOCK" 'CREATE DATABASE fixed'
na="$(printf '%s\n' "$FIXED_BLOCK" | grep -n 'select rolcreatedb' | head -1 | cut -d: -f1)"; nc="$(printf '%s\n' "$FIXED_BLOCK" | grep -n 'CREATE DATABASE fixed' | head -1 | cut -d: -f1)"
eq assert-before-create "$([ -n "$na" ] && [ -n "$nc" ] && [ "$na" -lt "$nc" ] && echo yes || echo no)" yes; finish

begin "preprovision.sql: both owner roles, the WITH INHERIT TRUE, SET TRUE grant, and the 'SET' privilege name only after a >= 160000 test (never evaluated on PostgreSQL < 16)"
has managed "$(cat "$PV")" 'send_managed_owner'; has byo "$(cat "$PV")" 'send_byo_owner'; has grant "$(cat "$PV")" 'GRANT %I TO %I WITH INHERIT TRUE, SET TRUE'; has create "$(cat "$PV")" 'CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB'
eq plain-grant "$(grep -c "EXECUTE format('GRANT %I TO %I', r, current_user)" "$PV")" 1
eq set-guarded "$(awk -v q="'" '/^--/ { next } /server_version_num/ { last = $0 } index($0, q "SET" q) { n++; if (last !~ />= 160000/) bad++ } END { print (n > 0 ? bad + 0 : "none") }' "$PV")" 0; finish

# ---- the control for THIS file: prove the assertions can say no -------------------------------------------------------------------------------------------------------------
# Inside a subshell (its counters cannot leak into ours) feed two WRONG expectations: a BLOCKER claimed to be a CASCADE, and an exact-match compare claimed to be red.
# Both must be reported as mismatches. If either were reported as a pass, every `ok` above would mean nothing.
echo "classify-selftest: CONTROL"
ctl="$( (
  cases=0; okc=0; bad=0
  { begin "wrong class";   parse create_role m2; row 1 m2 send_managed_0000_bootstrap.up.sql 20 42501 CASCADE; finish; } > /dev/null
  { begin "wrong verdict"; printf '%s' "$EXACT" > "$K"; cmp_ "$K" "$recs"; eq rc "$CRC" 1; finish; } > /dev/null
  echo "CONTROL cases=$cases ok=$okc bad=$bad"
) 2>&1 )"
begin "control: the assertions report a wrong class and a wrong verdict as mismatches ($ctl)"
eq control "$ctl" 'CONTROL cases=2 ok=0 bad=2'; finish

echo "selftest: $cases cases, $okc behaved as expected"
if [ "$cases" -lt "$MIN_CASES" ]; then
  echo "selftest: RED - only $cases cases ran, the hard floor is $MIN_CASES (were cases deleted?)"; exit 1
fi
if [ "$bad" -ne 0 ] || [ "$okc" -ne "$cases" ]; then
  echo "selftest: RED - $bad case(s) did NOT behave as expected"; exit 1
fi
exit 0
