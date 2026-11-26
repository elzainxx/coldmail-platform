#!/usr/bin/env bash
# check-m1-callers-writer-selftest.sh — proves scripts/check-m1-callers-writer.sh is green on a tree whose only writers of verify.api_callers are internal/tenant/m1key.go (the privileged connection of the operator tools)
# and db/install/postmodule1.sql (the install function: exactly one INSERT), and RED, FOR THE REASON IT NAMES, on each other way the table can be written, on each way the install function can grow a second kind of write,
# on a missing door, and on a call of a privileged writer from the service's runtime path. Every case builds a throwaway tree (a stand-in for the repository root: internal/, cmd/, db/) in a temp directory, so it needs
# no database, no Go toolchain and no checkout.
# Every reject case asserts the printed REASON as well as the exit code (a case that went red for an unrelated reason would read as a pass forever: class 10). The selftest has a hard minimum number of cases, and a
# CONTROL proves it can say "no": a gate that always exits 0, a gate that fails without a reason, and two wrong expectations must each be reported as a mismatch.
set -u
export LC_ALL=C
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-m1-callers-writer.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=64; cases=0; ok=0; bad_expect=0

PRIV=internal/tenant/m1key.go
INST=db/install/postmodule1.sql
DOOR=internal/tenant/m1provision.go
OUT_TAIL="outside the two sanctioned writers $PRIV and $INST — register a user's key through platform.provision_m1_caller (tenant.M1CallerEnsurer)"
MIG_TAIL="in a platform migration — the platform's migrations run before module 1's and never write its table (module 1's rows are written by its own identity and by platform.provision_m1_caller)"
INST_TAIL="in the install function: it may INSERT one kind of row and cannot UPDATE, DELETE, TRUNCATE, COPY or MERGE anything"
CALL_TAIL="outside internal/tenant/ — the service's runtime path reaches module 1's key table only through platform.provision_m1_caller (tenant.M1CallerEnsurer); the privileged writers are for operator tools and tests"
outside() { printf '%s:%s: write to verify.api_callers (%s) %s' "$1" "$2" "$3" "$OUT_TAIL"; }   # outside <path> <line> <kind>
inmig()   { printf '%s:%s: write to verify.api_callers (%s) %s' "$1" "$2" "$3" "$MIG_TAIL"; }   # inmig <path> <line> <kind>
ininst()  { printf '%s:%s: write to verify.api_callers (%s) %s' "$INST" "$1" "$2" "$INST_TAIL"; } # ininst <line> <kind>
calls()   { printf '%s:%s: call of %s( %s' "$1" "$2" "$3" "$CALL_TAIL"; }                        # calls <path> <line> <name>
NC_HEAD='NOT CHECKED: test files (*_test.go) and everything under tests/'

put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }   # put <dir> <relative path> < content
mk() {   # mk <name>: a fresh CLEAN tree; echoes its directory
  d="$T/$1"; rm -rf "$d"
  put "$d" "$PRIV" <<'EOF'
package tenant

// the operator tools' writer: through a privileged connection
const insertM1Caller = `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id)
VALUES ($1, $2, now() - interval '1 hour', $3::uuid)
ON CONFLICT (key_sha256) DO NOTHING`

const retireM1Caller = `UPDATE verify.api_callers SET not_after = now() WHERE key_sha256 = $1`
EOF
  put "$d" "$DOOR" <<'EOF'
package tenant

const m1ProvisionCall = `SELECT platform.provision_m1_caller($1::uuid, $2::bytea)`
EOF
  put "$d" internal/app/app.go <<'EOF'
package app

const list = "SELECT id, email FROM platform.listable_users ORDER BY created_at"
EOF
  put "$d" cmd/platform/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" "$INST" <<'EOF'
-- the install step after module 1's migrations
CREATE OR REPLACE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $fn$
BEGIN
    INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id)
    VALUES ('platform-user-' || p_user_id::text, p_key_sha256, now() - interval '1 hour', p_user_id)
    ON CONFLICT (key_sha256) DO NOTHING;
    RETURN 'created';
END
$fn$;
GRANT EXECUTE ON FUNCTION platform.provision_m1_caller(uuid, bytea) TO platform_app;
EOF
  put "$d" db/install/preprovision.sql <<'EOF'
-- the install step before the modules' migrations: roles only
SELECT 1;
EOF
  put "$d" db/migrations/platform_0003_x.up.sql <<'EOF'
CREATE TABLE platform.x (id uuid);
GRANT SELECT ON platform.x TO platform_app;
EOF
  echo "$d"
}

# check <label> <rc> <want-rc> <subs> <output>: every @@-separated sub must appear in the output; one that starts with ! must NOT appear
check() {
  local label="$1" rc="$2" want="$3" subs="$4" out="$5" rest part neg more miss=""
  cases=$((cases+1))
  if [ "$rc" -ne "$want" ]; then
    echo "  MISMATCH [$label]: rc=$rc want=$want"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  rest="$subs"
  while :; do
    case "$rest" in *@@*) part="${rest%%@@*}"; rest="${rest#*@@}"; more=1;; *) part="$rest"; more=0;; esac
    neg=0; case "$part" in '!'*) neg=1; part="${part#!}";; esac
    if printf '%s\n' "$out" | grep -q -F -- "$part"; then [ "$neg" -eq 0 ] || miss="$miss [must NOT appear: $part]"
    else [ "$neg" -eq 1 ] || miss="$miss [missing: $part]"; fi
    [ "$more" -eq 1 ] || break
  done
  if [ -n "$miss" ]; then
    echo "  MISMATCH [$label]: rc=$rc as expected, but$miss"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  ok=$((ok+1)); echo "  ok [$label] rc=$rc"
}
# expect <label> <want-rc> <subs> <tree> [gate args replacing the default --root <tree>]
expect() {
  local label="$1" want="$2" subs="$3" d="$4" out rc; shift 4
  if [ $# -gt 0 ]; then out="$(bash "$GATE" "$@" 2>&1)"; rc=$?; else out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?; fi
  check "$label" "$rc" "$want" "$subs" "$out"
}
# w <tree> <path> <go string literal>: a Go file that holds one constant on line 3
w() { put "$1" "$2" <<EOF
package app

const q = $3
EOF
}

# ---- clean trees -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk clean);  expect "clean: the only writers are the two sanctioned ones, the door exists" 0 "check-m1-callers-writer: OK@@scanned 4 Go file(s) and 2 SQL file(s) outside db/migrations/, and 1 migration file(s)@@sanctioned writers: $PRIV 2 write statement(s); $INST 1 write statement(s), 1 INSERT INTO@@0 write(s) outside the sanctioned writers, 0 write(s) in platform migrations, 0 sanctioned-writer problem(s), 0 call(s) of a privileged writer@@!write to verify.api_callers" "$d"
d=$(mk clean2); expect "a clean run prints its scope and what it did NOT check" 0 "scope: non-test *.go under internal/ and cmd/, *.sql under db/ except db/migrations/, and (on a separate line) db/migrations/*.sql@@$NC_HEAD@@shell scripts, the parent repository and Docs@@string concatenation, fmt.Sprintf or a variable@@a write through a view, a function, a trigger or a rule@@GRANTs on the table@@Comments are NOT stripped for the write forms" "$d"
d=$(mk reads);  put "$d" internal/app/r.go <<'EOF'
package app

const a = "SELECT caller_id FROM verify.api_callers WHERE key_sha256 = $1"
const b = `SELECT 1 FROM verify.api_callers FOR UPDATE OF verify.api_callers`
const c = "COPY verify.api_callers TO STDOUT"
const e = "COPY (SELECT caller_id FROM verify.api_callers) TO STDOUT"
const f = "GRANT SELECT ON verify.api_callers TO verify_app"
const g = "SELECT platform.provision_m1_caller($1::uuid, $2::bytea)"
EOF
                expect "reads, COPY ... TO, FOR UPDATE OF, a GRANT and a call of the function anywhere are not writes" 0 "check-m1-callers-writer: OK" "$d"
d=$(mk ddl);    put "$d" db/migrations/platform_0009_x.up.sql <<'EOF'
ALTER TABLE verify.api_callers ADD COLUMN x int;
COMMENT ON TABLE verify.api_callers IS 'written only by module 1';
GRANT SELECT ON verify.api_callers TO platform_admin;
EOF
                expect "migrations may alter, grant and comment: none of it is a write of a row" 0 "check-m1-callers-writer: OK@@and 2 migration file(s)@@0 write(s) in platform migrations" "$d"
d=$(mk whole);  put "$d" internal/app/x.go <<'EOF'
package app

const a = "INSERT INTO verify.api_callers_archive (caller_id) VALUES ($1)"
const b = "INSERT INTO verify.api_callers2 (caller_id) VALUES ($1)"
const c = `INSERT INTO "verify"."api_callers_archive" (caller_id) VALUES ($1)`
const d = "INSERT INTO send_managed.api_callers (caller_id) VALUES ($1)"
const e = "INSERT INTO myverify.api_callers (caller_id) VALUES ($1)"
const f = "xinsert into verify.api_callers"
const g = "DELETE FROM verify.jobs WHERE id = $1"
const h = "UPDATE verify.jobs SET status = $1"
EOF
                expect "whole word only: *_archive, *2, another schema, a longer schema name, an identifier ending in 'insert', and other tables pass" 0 "check-m1-callers-writer: OK" "$d"
d=$(mk testfile); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "INSERT INTO verify.api_callers (caller_id, key_sha256) VALUES ('x', $1)"
EOF
                expect "a _test.go file with a write is ignored, and the report says tests were not checked" 0 "check-m1-callers-writer: OK@@scanned 4 Go file(s)@@$NC_HEAD" "$d"
d=$(mk outside); put "$d" tests/harness/q.go <<'EOF'
package harness

const q = "INSERT INTO verify.api_callers (caller_id, key_sha256) VALUES ('x', $1)"
EOF
                put "$d" scripts/g.sh <<'EOF'
psql -c "DELETE FROM verify.api_callers"
EOF
                expect "Go under tests/ and shell under scripts/ are outside the scope" 0 "check-m1-callers-writer: OK@@scanned 4 Go file(s) and 2 SQL file(s)" "$d"
d=$(mk privmany); put "$d" "$PRIV" <<'EOF'
package tenant

const a = `INSERT INTO verify.api_callers (caller_id) VALUES ($1)`
const b = `UPDATE verify.api_callers SET status = 'rotating_out' WHERE key_sha256 = $1`
const c = `DELETE FROM verify.api_callers WHERE key_sha256 = $1`
const d = `TRUNCATE verify.api_callers`
EOF
                expect "the privileged writer may hold every write form (it is the operator tools' door); the count is printed" 0 "check-m1-callers-writer: OK@@$PRIV 4 write statement(s)" "$d"
d=$(mk crlfok); put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE sql AS $f$ SELECT 'x' $f$;
INSERT INTO verify.api_callers
    (caller_id, key_sha256, user_id) VALUES ('platform-user-x', $1, $2);
EOF
                sed -i 's/$/\r/' "$d/$INST"
                expect "the install file with CRLF line endings, its statement split over two lines, is green" 0 "check-m1-callers-writer: OK@@$INST 1 write statement(s), 1 INSERT INTO" "$d"

# ---- every other way to write the table: red, naming the file, the line and the kind --------------------------------------------------------------------------------------------------
d=$(mk ins);    w "$d" internal/app/q.go '"INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ($1, $2, $3)"'
                expect "INSERT INTO verify.api_callers in another file names the file, the line and the kind" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@check-m1-callers-writer: 1 write(s) outside the sanctioned writers@@RED — 1 problem(s): 1 write(s) outside $PRIV and $INST" "$d"
d=$(mk cmd);    put "$d" cmd/platform/q.go <<'EOF'
package main

import "fmt"

func list() { fmt.Println("INSERT INTO verify.api_callers (caller_id) VALUES ('x')") }
EOF
                expect "a file under cmd/ is scanned too" 1 "$(outside cmd/platform/q.go 5 'INSERT INTO')" "$d"
d=$(mk upd);    w "$d" internal/app/q.go '"UPDATE verify.api_callers SET user_id = $1 WHERE key_sha256 = $2"'
                expect "UPDATE (repointing a key at another user: the privilege the function does not have)" 1 "$(outside internal/app/q.go 3 'UPDATE')" "$d"
d=$(mk updonly); w "$d" internal/app/q.go '"UPDATE ONLY verify.api_callers SET status = $1"'
                expect "UPDATE ONLY" 1 "$(outside internal/app/q.go 3 'UPDATE')" "$d"
d=$(mk del);    w "$d" internal/app/q.go '"DELETE FROM verify.api_callers WHERE user_id = $1"'
                expect "DELETE FROM" 1 "$(outside internal/app/q.go 3 'DELETE FROM')" "$d"
d=$(mk delonly); w "$d" internal/app/q.go '"DELETE FROM ONLY verify.api_callers"'
                expect "DELETE FROM ONLY" 1 "$(outside internal/app/q.go 3 'DELETE FROM')" "$d"
d=$(mk trunc);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "TRUNCATE verify.api_callers"
const b = "TRUNCATE TABLE verify.api_callers"
const c = "TRUNCATE TABLE ONLY verify.api_callers CASCADE"
const d = "TRUNCATE verify.jobs, verify.api_callers"
const e = "TRUNCATE verify.api_callers, verify.jobs"
EOF
                expect "TRUNCATE, TRUNCATE TABLE, TRUNCATE TABLE ONLY, and a list that holds the table before or after another" 1 "$(outside internal/app/q.go 3 TRUNCATE)@@$(outside internal/app/q.go 4 TRUNCATE)@@$(outside internal/app/q.go 5 TRUNCATE)@@$(outside internal/app/q.go 6 TRUNCATE)@@$(outside internal/app/q.go 7 TRUNCATE)@@RED — 5 problem(s): 5 write(s) outside $PRIV and $INST" "$d"
d=$(mk copy);   put "$d" internal/app/q.go <<'EOF'
package app

const a = "COPY verify.api_callers (caller_id, key_sha256) FROM STDIN"
const b = `COPY verify.api_callers
	FROM STDIN`
EOF
                expect "COPY ... FROM, with and without a column list, and split over lines" 1 "$(outside internal/app/q.go 3 'COPY ... FROM')@@$(outside internal/app/q.go 4 'COPY ... FROM')" "$d"
d=$(mk merge);  w "$d" internal/app/q.go '"MERGE INTO verify.api_callers c USING src ON c.key_sha256 = src.key_sha256 WHEN NOT MATCHED THEN DO NOTHING"'
                expect "MERGE INTO" 1 "$(outside internal/app/q.go 3 'MERGE INTO')" "$d"
d=$(mk ident);  put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"verify", "api_callers"}, []string{"caller_id", "key_sha256"}, rows)
}
EOF
                expect "pgx.Identifier{\"verify\", \"api_callers\"} (what CopyFrom writes through) is flagged" 1 "$(outside internal/app/q.go 4 'pgx.Identifier, as CopyFrom writes through it')" "$d"
d=$(mk lower);  w "$d" internal/app/q.go '"insert into verify.api_callers (caller_id) values ($1)"'
                expect "lower case" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk mixed);  w "$d" internal/app/q.go '"InSeRt InTo VeRiFy.ApI_CaLlErS (caller_id) values ($1)"'
                expect "mixed case" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk raw);    put "$d" internal/app/q.go <<'EOF'
package app

const q = `
	INSERT
	  INTO
		verify.api_callers (caller_id, key_sha256)
	VALUES ($1, $2)`
EOF
                expect "split over lines in a Go raw string (INSERT, newline, tabs, INTO, newline, the table): reported at the INSERT line" 1 "$(outside internal/app/q.go 4 'INSERT INTO')@@!internal/app/q.go:5:@@!internal/app/q.go:6:" "$d"
d=$(mk crlf);   put "$d" internal/app/q.go <<'EOF'
package app

const q = `INSERT INTO
		verify.api_callers (caller_id)`
EOF
                sed -i 's/$/\r/' "$d/internal/app/q.go"
                expect "the same split statement in a file with CRLF line endings" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk quoted); put "$d" internal/app/q.go <<'EOF'
package app

const a = `INSERT INTO "verify"."api_callers" (caller_id) VALUES ($1)`
const b = `INSERT INTO verify."api_callers" (caller_id) VALUES ($1)`
const c = `INSERT INTO "verify".api_callers (caller_id) VALUES ($1)`
const d = `INSERT INTO verify . api_callers (caller_id) VALUES ($1)`
EOF
                expect "quoted and spaced identifiers: \"verify\".\"api_callers\", verify.\"api_callers\", \"verify\".api_callers, verify . api_callers" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@$(outside internal/app/q.go 4 'INSERT INTO')@@$(outside internal/app/q.go 5 'INSERT INTO')@@$(outside internal/app/q.go 6 'INSERT INTO')@@RED — 4 problem(s)" "$d"
d=$(mk escaped); w "$d" internal/app/q.go '"INSERT INTO \"verify\".\"api_callers\" (caller_id) VALUES ($1)"'
                expect "quoted identifiers escaped inside a Go interpreted string" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk bare);   w "$d" internal/app/q.go '"INSERT INTO api_callers (caller_id) VALUES ($1)"'
                expect "an unqualified INSERT INTO api_callers is flagged (it would resolve through search_path)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk twice);  w "$d" internal/app/q.go '"INSERT INTO verify.api_callers (caller_id) VALUES ($1); DELETE FROM verify.api_callers"'
                expect "two writes in a row on one line are both reported (a consumed boundary would hide the second)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@$(outside internal/app/q.go 3 'DELETE FROM')@@RED — 2 problem(s): 2 write(s) outside $PRIV and $INST" "$d"
d=$(mk with);   w "$d" internal/app/q.go '`WITH x AS (INSERT INTO verify.api_callers (caller_id) VALUES ($1) RETURNING caller_id) SELECT caller_id FROM x`'
                expect "a write inside a WITH (a data-modifying CTE)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk comment); put "$d" internal/app/q.go <<'EOF'
package app

// the old code ran INSERT INTO verify.api_callers here, before the function existed
const q = 1
EOF
                expect "a comment containing the statement IS flagged (comments are not stripped)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk sqlfile); put "$d" db/queries/add.sql <<'EOF'
-- add a key
INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ($1, $2, $3);
EOF
                expect "a .sql file under db/ outside db/migrations is scanned and flagged" 1 "scanned 4 Go file(s) and 3 SQL file(s) outside db/migrations/@@$(outside db/queries/add.sql 2 'INSERT INTO')" "$d"
d=$(mk preprov); put "$d" db/install/preprovision.sql <<'EOF'
SELECT 1;
UPDATE verify.api_callers SET status = 'active';
EOF
                expect "the OTHER install file is not sanctioned: only postmodule1.sql may write" 1 "$(outside db/install/preprovision.sql 2 'UPDATE')" "$d"
d=$(mk sibling); put "$d" internal/tenant/m1other.go <<'EOF'
package tenant

const q = `INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ($1, $2, $3)`
EOF
                expect "another file of the SAME package is not the sanctioned one: only m1key.go may write" 1 "$(outside internal/tenant/m1other.go 3 'INSERT INTO')" "$d"
d=$(mk namesake); put "$d" internal/other/m1key.go <<'EOF'
package other

const q = `INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ($1, $2, $3)`
EOF
                expect "an m1key.go in another directory is not the sanctioned path" 1 "$(outside internal/other/m1key.go 3 'INSERT INTO')" "$d"
d=$(mk two);    put "$d" internal/app/a.go <<'EOF'
package app

const a = "UPDATE verify.api_callers SET user_id = $1"
EOF
                put "$d" internal/jobs/b.go <<'EOF'
package jobs

const b = "DELETE FROM verify.api_callers"
EOF
                expect "writers in two files are each reported with their own path and line" 1 "$(outside internal/app/a.go 3 'UPDATE')@@$(outside internal/jobs/b.go 3 'DELETE FROM')@@RED — 2 problem(s)" "$d"
d=$(mk migins); put "$d" db/migrations/platform_0099_seed.up.sql <<'EOF'
-- seed a caller
INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('x', '\x00', NULL);
EOF
                expect "a platform migration that writes module 1's table is red, and no register excuses it" 1 "$(inmig db/migrations/platform_0099_seed.up.sql 2 'INSERT INTO')@@1 write(s) in platform migrations@@RED — 1 problem(s): 0 write(s) outside $PRIV and $INST, 1 migration write(s)" "$d"
d=$(mk migupd); put "$d" db/migrations/platform_0099_fix.up.sql <<'EOF'
UPDATE verify.api_callers SET not_after = now();
EOF
                expect "an UPDATE in a platform migration" 1 "$(inmig db/migrations/platform_0099_fix.up.sql 1 'UPDATE')" "$d"

# ---- the two sanctioned writers must exist and write, and the install function writes ONE kind of row ---------------------------------------------------------------------------------------
d=$(mk nopriv); rm "$d/$PRIV"
                expect "the privileged writer is gone: the instrument has no subject" 1 "$PRIV:1: the sanctioned privileged writer does not exist@@1 sanctioned-writer problem(s)" "$d"
d=$(mk nopw);   put "$d" "$PRIV" <<'EOF'
package tenant

const a = `SELECT caller_id FROM verify.api_callers WHERE key_sha256 = $1`
EOF
                expect "the privileged writer holds no write (a writer that never writes, or a matcher that no longer reads it)" 1 "$PRIV:1: the sanctioned privileged writer holds no write statement on verify.api_callers: the instrument matched nothing" "$d"
d=$(mk noinst); rm "$d/$INST"
                expect "the install step is gone: the service has no door" 1 "$INST:1: the sanctioned install step does not exist: the service has no door to module 1's key table" "$d"
d=$(mk noins);  put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE sql AS $f$ SELECT 'created' $f$;
EOF
                expect "the install step holds no INSERT: it would register nothing" 1 "$INST:1: the install step holds no INSERT INTO verify.api_callers: the instrument matched nothing" "$d"
d=$(mk inst2);  put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE sql AS $f$ SELECT 'created' $f$;
INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('a', $1, $2);
INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('b', $1, $2);
EOF
                expect "a second INSERT in the install step: one kind of row, written in one place" 1 "$INST:1: the install step holds 2 INSERT INTO verify.api_callers, want exactly 1" "$d"
d=$(mk instupd); put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE plpgsql AS $f$
BEGIN
    INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('a', $1, $2) ON CONFLICT (key_sha256) DO NOTHING;
    UPDATE verify.api_callers SET user_id = p_user_id WHERE key_sha256 = p_key_sha256;
    RETURN 'created';
END
$f$;
EOF
                expect "an UPDATE in the install function (it could repoint a key at another user) names the line" 1 "$(ininst 4 'UPDATE')@@1 sanctioned-writer problem(s)" "$d"
d=$(mk instdel); put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE plpgsql AS $f$
BEGIN
    INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('a', $1, $2);
    DELETE FROM verify.api_callers WHERE user_id = p_user_id AND key_sha256 <> p_key_sha256;
    RETURN 'created';
END
$f$;
EOF
                expect "a DELETE in the install function" 1 "$(ininst 4 'DELETE FROM')" "$d"
d=$(mk insttr); put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text LANGUAGE plpgsql AS $f$
BEGIN
    INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('a', $1, $2);
    TRUNCATE verify.api_callers;
    COPY verify.api_callers FROM STDIN;
    MERGE INTO verify.api_callers c USING x ON true WHEN NOT MATCHED THEN DO NOTHING;
    RETURN 'created';
END
$f$;
EOF
                expect "TRUNCATE, COPY ... FROM and MERGE in the install function are each named" 1 "$(ininst 4 'TRUNCATE')@@$(ininst 5 'COPY ... FROM')@@$(ininst 6 'MERGE INTO')@@3 sanctioned-writer problem(s)" "$d"
d=$(mk instname); put "$d" "$INST" <<'EOF'
CREATE FUNCTION platform.something_else(p_user_id uuid) RETURNS text LANGUAGE sql AS $f$ SELECT 'x' $f$;
INSERT INTO verify.api_callers (caller_id, key_sha256, user_id) VALUES ('a', $1, $2);
EOF
                expect "the install step does not define platform.provision_m1_caller: the door the Go half calls does not exist" 1 "$INST:1: the install step does not define platform.provision_m1_caller" "$d"
d=$(mk nodoor); rm "$d/$DOOR"
                expect "the Go half of the door is gone" 1 "$DOOR:1: the service's runtime path to module 1's key table does not exist" "$d"
d=$(mk nocall); put "$d" "$DOOR" <<'EOF'
package tenant

const m1ProvisionCall = `SELECT 1`
EOF
                expect "the ensurer no longer calls the function by name" 1 "$DOOR:1: the ensurer does not call platform.provision_m1_caller(" "$d"

# ---- the privileged writers are not called from the service's runtime path --------------------------------------------------------------------------------------------------------------
d=$(mk call1);  put "$d" internal/jobengine/modules.go <<'EOF'
package jobengine

func provision(r *tenant.Resolver, db tenant.AdminDB, u string) error {
	return r.ProvisionM1Caller(ctx, db, u)
}
EOF
                expect "a call of ProvisionM1Caller under internal/ outside internal/tenant/ is red and names the call" 1 "$(calls internal/jobengine/modules.go 4 ProvisionM1Caller)@@1 call(s) of a privileged writer from the runtime path@@RED — 1 problem(s): 0 write(s) outside $PRIV and $INST, 0 migration write(s), 0 sanctioned-writer problem(s), 1 privileged-writer call(s)" "$d"
d=$(mk call2);  put "$d" internal/api/x.go <<'EOF'
package api

func f(res *tenant.Resolver, db tenant.AdminDB) error {
	if err := res.ProvisionM1CallerVersion(ctx, db, u, 2); err != nil {
		return err
	}
	return res . RetireM1CallerVersion(ctx, db, u, 1, 0)
}
EOF
                expect "ProvisionM1CallerVersion and a spaced RetireM1CallerVersion are named too" 1 "$(calls internal/api/x.go 4 ProvisionM1CallerVersion)@@$(calls internal/api/x.go 7 RetireM1CallerVersion)@@RED — 2 problem(s)" "$d"
d=$(mk callcmd); put "$d" cmd/platform/tool.go <<'EOF'
package main

func run(r *tenant.Resolver, db tenant.AdminDB, u string) error { return r.ProvisionM1Caller(ctx, db, u) }
EOF
                expect "an operator tool under cmd/ may call the privileged writer (it holds the privileged connection on purpose)" 0 "check-m1-callers-writer: OK@@0 call(s) of a privileged writer" "$d"
d=$(mk calltn); put "$d" internal/tenant/other.go <<'EOF'
package tenant

func (r *Resolver) all(db AdminDB, u string) error { return r.ProvisionM1Caller(nil, db, u) }
EOF
                expect "a call inside internal/tenant/ itself is not the runtime path" 0 "check-m1-callers-writer: OK" "$d"
d=$(mk callmen); put "$d" internal/jobengine/doc.go <<'EOF'
package jobengine

// The row is written by tenant.ProvisionM1Caller through a privileged connection; the engine only calls EnsureCaller.
func (p prov) EnsureCaller(ctx context.Context, u string) error { return p.e.EnsureCaller(ctx, u) }
EOF
                expect "a comment that names ProvisionM1Caller, and a call of EnsureCaller, are not calls of a privileged writer" 0 "check-m1-callers-writer: OK@@0 call(s) of a privileged writer" "$d"
d=$(mk calltest); put "$d" internal/jobengine/x_test.go <<'EOF'
package jobengine

func seed(r *tenant.Resolver, db tenant.AdminDB) error { return r.ProvisionM1Caller(ctx, db, "u") }
EOF
                expect "a test file may call the privileged writer (it seeds module 1's table on purpose)" 0 "check-m1-callers-writer: OK" "$d"

# ---- the floors, the arguments, the matcher's own canary ------------------------------------------------------------------------------------------------------------------------------------
d=$(mk nogo);   rm -rf "$d/internal" "$d/cmd"; mkdir -p "$d/internal" "$d/cmd"
                expect "no Go file at all: the gate matched nothing" 1 "scanned 0 Go file(s) and 2 SQL file(s) outside db/migrations/, and 1 migration file(s)@@found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk onlytest); rm -rf "$d/internal" "$d/cmd"; put "$d" internal/app/a_test.go <<'EOF'
package app
EOF
                expect "only _test.go files: no Go file is scanned, so the gate matched nothing" 1 "found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk nomig);  rm -rf "$d/db/migrations"
                expect "no migration file at all: the gate matched nothing" 1 "found 0 migration files under db/migrations/@@the instrument matched nothing" "$d"
d=$(mk noroot); expect "a root that is not a directory" 1 "the root '$T/no-such-root' is not a directory: the instrument matched nothing" "$d" --root "$T/no-such-root"
d=$(mk usage1); expect "an unknown argument is a usage error" 2 "check-m1-callers-writer: unknown argument '--bogus'@@usage: check-m1-callers-writer.sh [--root DIR]" "$d" --bogus
d=$(mk usage2); expect "--root with no value is a usage error" 2 "usage: check-m1-callers-writer.sh [--root DIR]" "$d" --root
mkdir -p "$T/shim"; printf '#!/bin/sh\nexit 1\n' > "$T/shim/grep"; chmod +x "$T/shim/grep"
d=$(mk canary); out="$(PATH="$T/shim:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "a matcher that finds nothing (grep replaced by a stub) fails its own canary instead of passing a clean tree" "$rc" 1 "check-m1-callers-writer: RED — the matcher failed its own canary@@wanted 1 match(es), found 0@@!check-m1-callers-writer: OK" "$out"
# a grep that works for the canaries but CRASHES on one file (here: any file containing POISON) must not read as a clean file
REAL_GREP="$(command -v grep)"; mkdir -p "$T/shim2"
printf '#!/bin/sh\nfor last; do :; done\nif "%s" -q POISON "$last" 2>/dev/null; then exit 3; fi\nexec "%s" "$@"\n' "$REAL_GREP" "$REAL_GREP" > "$T/shim2/grep"; chmod +x "$T/shim2/grep"
d=$(mk crash);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "INSERT INTO verify.api_callers (caller_id) VALUES ($1)" // POISON
EOF
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on one file is red and names the file (a crashed matcher prints no match: it must not read as clean)" "$rc" 1 "the matcher failed on internal/app/q.go (grep exit status 3)@@1 file(s) the matcher failed on@@!check-m1-callers-writer: OK" "$out"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$(mk crashok)" 2>&1)"; rc=$?
                check "the same crashing-grep stub on a tree without POISON behaves like the real grep (the stub itself is not the cause)" "$rc" 0 "check-m1-callers-writer: OK@@0 file(s) the matcher failed on" "$out"
d=$(mk crashpriv); printf '\n// POISON\n' >> "$d/$PRIV"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on the privileged writer itself is red (it cannot be read, so nothing about it is proven)" "$rc" 1 "the matcher failed on $PRIV (grep exit status 3)@@!check-m1-callers-writer: OK" "$out"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Four deliberately WRONG arrangements, each run through the same `expect`. EVERY one must be reported as a mismatch; if any is accepted, this selftest cannot fail and says so.
#   1. a gate that always exits 0, run on a violating tree that must be red                      (the selftest must go red if the gate were `exit 0`)
#   2. a gate that fails WITHOUT naming a reason, run on a violating tree that must be red for the reason   (class 10: the right exit code for no reason)
#   3. the real gate on a clean tree, expected to be red
#   4. the real gate on a violating tree, expected to be green
printf '#!/bin/sh\nexit 0\n' > "$T/stub-pass.sh"; printf '#!/bin/sh\necho boom\nexit 1\n' > "$T/stub-bare-red.sh"
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases; real="$GATE"
d=$(mk ctlv); w "$d" internal/app/q.go '"INSERT INTO verify.api_callers (caller_id) VALUES ($1)"'
GATE="$T/stub-pass.sh";     expect "CONTROL 1 (wrong on purpose): a gate that is always green" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$T/stub-bare-red.sh"; expect "CONTROL 2 (wrong on purpose): a gate that is red for no named reason" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$real"
d=$(mk ctlc); expect "CONTROL 3 (wrong on purpose): a clean tree expected to be RED" 1 "write to verify.api_callers" "$d" >/dev/null 2>&1
d=$(mk ctlg); w "$d" internal/app/q.go '"INSERT INTO verify.api_callers (caller_id) VALUES ($1)"'; expect "CONTROL 4 (wrong on purpose): a violating tree expected to be GREEN" 0 "check-m1-callers-writer: OK" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 4 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] all four deliberately wrong arrangements were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 4 wrong arrangements were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
