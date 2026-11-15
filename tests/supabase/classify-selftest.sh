#!/usr/bin/env bash
# classify-selftest.sh — the PAIRED CONTROL for tests/supabase/classify.sh. No database, no psql: it feeds the classifier recorded-shape psql output (written inline below, in the
# format classify.sh documents) and asserts, for every case, the EXACT chain, file, line, SQLSTATE, class, message and statement text of each record; then it feeds the
# comparison against known-blockers.txt every way it can be wrong.
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
MIN_CASES=79
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
