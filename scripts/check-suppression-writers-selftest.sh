#!/usr/bin/env bash
# check-suppression-writers-selftest.sh — proves scripts/check-suppression-writers.sh is green on a tree whose only writer of platform.suppressions is internal/suppress/store.go (validate first, then
# write) and RED, FOR THE REASON IT NAMES, on each other way the table can be written, on each way the sanctioned writer can stop validating first, and on each way its register of migration exceptions
# can rot. Every case builds a throwaway tree (a stand-in for the repository root: internal/, cmd/, db/, tests/suppression-writer-exceptions.txt) in a temp directory, so it needs no database, no Go
# toolchain and no checkout.
# Every reject case asserts the printed REASON as well as the exit code (a case that went red for an unrelated reason would read as a pass forever: class 10). The selftest has a hard minimum number of
# cases, and a CONTROL proves it can say "no": a gate that always exits 0, a gate that fails without a reason, and two wrong expectations must each be reported as a mismatch.
set -u
export LC_ALL=C
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-suppression-writers.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=77; cases=0; ok=0; bad_expect=0

SAN=internal/suppress/store.go
OUT_TAIL="outside the sanctioned writer $SAN — write through suppress.Add, which validates the address first"
VF_TAIL='is not preceded by a call of norm.Validate( earlier in the same function — the sanctioned writer must validate the address first, then write what the validator returned'
NF_TAIL='is not inside a function — the sanctioned writer must write from a function that calls norm.Validate( first, in the same function'
MIG_TAIL='in a migration — a migration creates the table and its constraints and writes no rows'
outside() { printf '%s:%s: write to platform.suppressions (%s) %s' "$1" "$2" "$3" "$OUT_TAIL"; }   # outside <path> <line> <kind>
vfirst()  { printf '%s:%s: write to platform.suppressions (%s) %s' "$SAN" "$1" "$2" "$VF_TAIL"; } # vfirst <line> <kind>
nofn()    { printf '%s:%s: write to platform.suppressions (%s) %s' "$SAN" "$1" "$2" "$NF_TAIL"; }  # nofn <line> <kind>
inmig()   { printf '%s:%s: write to platform.suppressions (%s) %s' "$1" "$2" "$3" "$MIG_TAIL"; }  # inmig <path> <line> <kind>
NC_HEAD='NOT CHECKED: test files (*_test.go) and everything under tests/'

put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }   # put <dir> <relative path> < content
mk() {   # mk <name>: a fresh CLEAN tree; echoes its directory
  d="$T/$1"; rm -rf "$d"
  put "$d" internal/suppress/store.go <<'EOF'
package suppress

import (
	"context"

	"github.com/coldmail-platform/platform/internal/norm"
)

// Add validates first, then writes what the validator returned.
func Add(ctx context.Context, q Execer, user, email, source string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO platform.suppressions (user_id, email, source)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT DO NOTHING`, user, n, source)
	return err
}
EOF
  put "$d" internal/suppress/read.go <<'EOF'
package suppress

const read = `SELECT email FROM platform.suppressions WHERE user_id = $1::uuid AND email = ANY($2::text[])`
EOF
  put "$d" internal/app/app.go <<'EOF'
package app

const list = "SELECT id, email FROM platform.listable_users ORDER BY created_at"
EOF
  put "$d" cmd/platform/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" db/migrations/platform_0003_suppressions.up.sql <<'EOF'
CREATE TABLE platform.suppressions (user_id uuid NOT NULL, email text NOT NULL, source text NOT NULL);
GRANT SELECT, INSERT ON platform.suppressions TO platform_app;
EOF
  put "$d" tests/suppression-writer-exceptions.txt <<'EOF'
# fixture register: no entries
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

# ---- clean trees -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk clean);  expect "clean: the only writer is the sanctioned one, and it validates first" 0 "check-suppression-writers: OK@@scanned 4 Go file(s) and 0 SQL file(s) outside db/migrations/, and 1 migration file(s)@@sanctioned writer $SAN: 1 write statement(s), 1 norm.Validate( call(s) outside comments, the first at byte@@0 write(s) outside the sanctioned writer, 0 write(s) in it before a validate call, 0 sanctioned-writer problem(s)@@migrations: 0 write(s) flagged@@!write to platform.suppressions" "$d"
d=$(mk clean2); expect "a clean run prints its scope and what it did NOT check" 0 "scope: non-test *.go under internal/ and cmd/, *.sql under db/ except db/migrations/, and (on a separate line) db/migrations/*.sql@@$NC_HEAD@@shell scripts and Docs@@string concatenation, fmt.Sprintf or a variable@@a write through a view, a function, a trigger or a rule@@whether norm.Validate( runs on the path that reaches the write@@the database's own walls (the grants and the CHECK)@@Comments are NOT stripped for the write forms" "$d"
d=$(mk reads);  put "$d" internal/app/r.go <<'EOF'
package app

const a = "SELECT email FROM platform.suppressions WHERE user_id = $1"
const b = `SELECT 1 FROM platform.suppressions FOR UPDATE OF platform.suppressions`
const c = "COPY platform.suppressions TO STDOUT"
const e = "COPY (SELECT email FROM platform.suppressions) TO STDOUT"
const f = "GRANT SELECT ON platform.suppressions TO platform_admin"
EOF
                expect "reads, COPY ... TO, FOR UPDATE OF and a GRANT anywhere are not writes" 0 "check-suppression-writers: OK" "$d"
d=$(mk ddl);    put "$d" db/migrations/platform_0009_rule.up.sql <<'EOF'
ALTER TABLE platform.suppressions DROP CONSTRAINT suppressions_email_norm_v1;
ALTER TABLE platform.suppressions ADD CONSTRAINT suppressions_email_norm_v1 CHECK (email = btrim(email));
COMMENT ON TABLE platform.suppressions IS 'Rows are never updated and nothing deletes one.';
INSERT INTO platform.schema_migrations (version) VALUES ('0009');
EOF
                expect "migrations may create, alter, grant and comment: none of it is a write of a row" 0 "check-suppression-writers: OK@@and 2 migration file(s)@@migrations: 0 write(s) flagged" "$d"
d=$(mk whole);  put "$d" internal/app/x.go <<'EOF'
package app

const a = "INSERT INTO platform.suppressions_archive (email) VALUES ($1)"
const b = "INSERT INTO platform.suppressions2 (email) VALUES ($1)"
const c = `INSERT INTO "platform"."suppressions_archive" (email) VALUES ($1)`
const d = "INSERT INTO send_managed.suppressions (email) VALUES ($1)"
const e = "INSERT INTO myplatform.suppressions (email) VALUES ($1)"
const f = "xinsert into platform.suppressions"
const g = "DELETE FROM platform.suppressions_archive"
const h = "DELETE FROM platform.ledger WHERE id = $1"
EOF
                expect "whole word only: *_archive, *2, another schema, a longer schema name, an identifier ending in 'insert', and other tables pass" 0 "check-suppression-writers: OK" "$d"
d=$(mk testfile); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')"
EOF
                expect "a _test.go file with a write is ignored, and the report says tests were not checked" 0 "check-suppression-writers: OK@@scanned 4 Go file(s)@@$NC_HEAD" "$d"
d=$(mk outside); put "$d" tests/harness/q.go <<'EOF'
package harness

const q = "INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, 'manual')"
EOF
                put "$d" scripts/g.sh <<'EOF'
psql -c "DELETE FROM platform.suppressions"
EOF
                expect "Go under tests/ and shell under scripts/ are outside the scope" 0 "check-suppression-writers: OK@@scanned 4 Go file(s) and 0 SQL file(s)" "$d"
d=$(mk sanctwo); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	n, err := norm.Validate(email) // validate first
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	exec(`DELETE FROM platform.suppressions WHERE email = $1`, n)
	return nil
}
EOF
                expect "the sanctioned file may hold several writes, each after the call (a // comment AFTER the call on its line is fine); the count is printed" 0 "check-suppression-writers: OK@@sanctioned writer $SAN: 2 write statement(s)" "$d"
d=$(mk crlfok);  put "$d" internal/suppress/store.go <<'EOF'
package suppress

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions
		(user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                sed -i 's/$/\r/' "$d/internal/suppress/store.go"
                expect "the sanctioned writer with CRLF line endings, its statement split over two lines, is green" 0 "check-suppression-writers: OK@@sanctioned writer $SAN: 1 write statement(s)" "$d"

# ---- every other way to write the table: red, naming the file, the line and the kind --------------------------------------------------------------------------------------------------
w() { put "$1" "$2" <<EOF
package app

const q = $3
EOF
}
d=$(mk ins);    w "$d" internal/app/q.go '"INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)"'
                expect "INSERT INTO platform.suppressions in another file names the file, the line and the kind" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@check-suppression-writers: 1 write(s) outside the sanctioned writer@@RED — 1 problem(s): 1 write(s) outside $SAN" "$d"
d=$(mk cmd);    put "$d" cmd/platform/q.go <<'EOF'
package main

import "fmt"

func list() { fmt.Println("INSERT INTO platform.suppressions (email) VALUES ('x')") }
EOF
                expect "a file under cmd/ is scanned too" 1 "$(outside cmd/platform/q.go 5 'INSERT INTO')" "$d"
d=$(mk upd);    w "$d" internal/app/q.go '"UPDATE platform.suppressions SET source = $1 WHERE email = $2"'
                expect "UPDATE" 1 "$(outside internal/app/q.go 3 'UPDATE')" "$d"
d=$(mk updonly); w "$d" internal/app/q.go '"UPDATE ONLY platform.suppressions SET source = $1"'
                expect "UPDATE ONLY" 1 "$(outside internal/app/q.go 3 'UPDATE')" "$d"
d=$(mk del);    w "$d" internal/app/q.go '"DELETE FROM platform.suppressions WHERE user_id = $1"'
                expect "DELETE FROM" 1 "$(outside internal/app/q.go 3 'DELETE FROM')" "$d"
d=$(mk delonly); w "$d" internal/app/q.go '"DELETE FROM ONLY platform.suppressions"'
                expect "DELETE FROM ONLY" 1 "$(outside internal/app/q.go 3 'DELETE FROM')" "$d"
d=$(mk trunc);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "TRUNCATE platform.suppressions"
const b = "TRUNCATE TABLE platform.suppressions"
const c = "TRUNCATE TABLE ONLY platform.suppressions CASCADE"
const d = "TRUNCATE platform.ledger, platform.suppressions"
const e = "TRUNCATE platform.suppressions, platform.ledger"
EOF
                expect "TRUNCATE, TRUNCATE TABLE, TRUNCATE TABLE ONLY, and a list that holds the table before or after another" 1 "$(outside internal/app/q.go 3 TRUNCATE)@@$(outside internal/app/q.go 4 TRUNCATE)@@$(outside internal/app/q.go 5 TRUNCATE)@@$(outside internal/app/q.go 6 TRUNCATE)@@$(outside internal/app/q.go 7 TRUNCATE)@@RED — 5 problem(s): 5 write(s) outside $SAN" "$d"
d=$(mk copy);   put "$d" internal/app/q.go <<'EOF'
package app

const a = "COPY platform.suppressions (user_id, email, source) FROM STDIN"
const b = `COPY platform.suppressions
	FROM STDIN`
EOF
                expect "COPY ... FROM, with and without a column list, and split over lines" 1 "$(outside internal/app/q.go 3 'COPY ... FROM')@@$(outside internal/app/q.go 4 'COPY ... FROM')" "$d"
d=$(mk merge);  w "$d" internal/app/q.go '"MERGE INTO platform.suppressions s USING src ON s.email = src.email WHEN NOT MATCHED THEN DO NOTHING"'
                expect "MERGE INTO" 1 "$(outside internal/app/q.go 3 'MERGE INTO')" "$d"
d=$(mk ident);  put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"platform", "suppressions"}, []string{"user_id", "email", "source"}, rows)
}
EOF
                expect "pgx.Identifier{\"platform\", \"suppressions\"} (what CopyFrom writes through) is flagged" 1 "$(outside internal/app/q.go 4 'pgx.Identifier, as CopyFrom writes through it')" "$d"
d=$(mk lower);  w "$d" internal/app/q.go '"insert into platform.suppressions (email) values ($1)"'
                expect "lower case" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk mixed);  w "$d" internal/app/q.go '"InSeRt InTo PlAtFoRm.SuPpReSsIoNs (email) values ($1)"'
                expect "mixed case" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk raw);    put "$d" internal/app/q.go <<'EOF'
package app

const q = `
	INSERT
	  INTO
		platform.suppressions (user_id, email, source)
	VALUES ($1, $2, $3)`
EOF
                expect "split over lines in a Go raw string (INSERT, newline, tabs, INTO, newline, the table): reported at the INSERT line" 1 "$(outside internal/app/q.go 4 'INSERT INTO')@@!internal/app/q.go:5:@@!internal/app/q.go:6:" "$d"
d=$(mk crlf);   put "$d" internal/app/q.go <<'EOF'
package app

const q = `INSERT INTO
		platform.suppressions (email)`
EOF
                sed -i 's/$/\r/' "$d/internal/app/q.go"
                expect "the same split statement in a file with CRLF line endings" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk quoted); put "$d" internal/app/q.go <<'EOF'
package app

const a = `INSERT INTO "platform"."suppressions" (email) VALUES ($1)`
const b = `INSERT INTO platform."suppressions" (email) VALUES ($1)`
const c = `INSERT INTO "platform".suppressions (email) VALUES ($1)`
const d = `INSERT INTO platform . suppressions (email) VALUES ($1)`
EOF
                expect "quoted and spaced identifiers: \"platform\".\"suppressions\", platform.\"suppressions\", \"platform\".suppressions, platform . suppressions" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@$(outside internal/app/q.go 4 'INSERT INTO')@@$(outside internal/app/q.go 5 'INSERT INTO')@@$(outside internal/app/q.go 6 'INSERT INTO')@@RED — 4 problem(s)" "$d"
d=$(mk escaped); w "$d" internal/app/q.go '"INSERT INTO \"platform\".\"suppressions\" (email) VALUES ($1)"'
                expect "quoted identifiers escaped inside a Go interpreted string" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk bare);   w "$d" internal/app/q.go '"INSERT INTO suppressions (email) VALUES ($1)"'
                expect "an unqualified INSERT INTO suppressions is flagged (it would resolve through search_path)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk twice);  w "$d" internal/app/q.go '"INSERT INTO platform.suppressions (email) VALUES ($1); DELETE FROM platform.suppressions"'
                expect "two writes in a row on one line are both reported (a consumed boundary would hide the second)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')@@$(outside internal/app/q.go 3 'DELETE FROM')@@RED — 2 problem(s): 2 write(s) outside $SAN" "$d"
d=$(mk with);   w "$d" internal/app/q.go '`WITH x AS (INSERT INTO platform.suppressions (email) VALUES ($1) RETURNING email) SELECT email FROM x`'
                expect "a write inside a WITH (a data-modifying CTE)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk comment); put "$d" internal/app/q.go <<'EOF'
package app

// the old code ran INSERT INTO platform.suppressions here, before the store existed
const q = 1
EOF
                expect "a comment containing the statement IS flagged (comments are not stripped)" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk sqlfile); put "$d" db/queries/add.sql <<'EOF'
-- add a suppression
INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3);
EOF
                expect "a .sql file under db/ outside db/migrations is scanned and flagged" 1 "scanned 4 Go file(s) and 1 SQL file(s) outside db/migrations/@@$(outside db/queries/add.sql 2 'INSERT INTO')" "$d"
d=$(mk sibling); put "$d" internal/suppress/read.go <<'EOF'
package suppress

const q = `INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`
EOF
                expect "another file of the SAME package is not the sanctioned one: only store.go may write" 1 "$(outside internal/suppress/read.go 3 'INSERT INTO')" "$d"
d=$(mk namesake); put "$d" internal/other/store.go <<'EOF'
package other

import "github.com/coldmail-platform/platform/internal/norm"

func Add(e string) {
	n, _ := norm.Validate(e)
	exec(`INSERT INTO platform.suppressions (email) VALUES ($1)`, n)
}
EOF
                expect "a store.go in another directory is not the sanctioned path, even if it validates first" 1 "$(outside internal/other/store.go 7 'INSERT INTO')" "$d"
d=$(mk two);    put "$d" internal/app/a.go <<'EOF'
package app

const a = "UPDATE platform.suppressions SET source = $1"
EOF
                put "$d" internal/jobs/b.go <<'EOF'
package jobs

const b = "DELETE FROM platform.suppressions"
EOF
                expect "writers in two files are each reported with their own path and line" 1 "$(outside internal/app/a.go 3 'UPDATE')@@$(outside internal/jobs/b.go 3 'DELETE FROM')@@RED — 2 problem(s)" "$d"

# ---- the sanctioned writer must validate first -----------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk early);  put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

const insert = `INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(insert, n)
	return nil
}
EOF
                expect "the write as a package-level constant ABOVE the validating function is red: it is not inside a function" 1 "$(nofn 5 'INSERT INTO')@@RED — 1 problem(s): 0 write(s) outside $SAN, 1 not preceded by norm.Validate(" "$d"
d=$(mk nocall); put "$d" internal/suppress/store.go <<'EOF'
package suppress

func Add(email string) error {
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, email)
	return nil
}
EOF
                expect "a sanctioned writer that never calls the validator is red" 1 "$(vfirst 4 'INSERT INTO')@@no norm.Validate( call outside a comment" "$d"
d=$(mk normal); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	n, err := norm.Normalize(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                expect "norm.Normalize instead of norm.Validate (the syntax layer skipped) is red" 1 "$(vfirst 10 'INSERT INTO')@@no norm.Validate( call outside a comment" "$d"
d=$(mk cmt1);   put "$d" internal/suppress/store.go <<'EOF'
package suppress

// Add calls norm.Validate(email) first and then writes.
func Add(email string) error {
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, email)
	return nil
}
EOF
                expect "norm.Validate( inside a // comment does not count as the call" 1 "$(vfirst 5 'INSERT INTO')@@no norm.Validate( call outside a comment" "$d"
d=$(mk cmt2);   put "$d" internal/suppress/store.go <<'EOF'
package suppress

func Add(email string) error {
	n := email // norm.Validate(email)
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                expect "norm.Validate( in a trailing // comment after code does not count either" 1 "$(vfirst 5 'INSERT INTO')@@no norm.Validate( call outside a comment" "$d"
d=$(mk cmt3);   put "$d" internal/suppress/store.go <<'EOF'
package suppress

// norm.Validate(email) is mentioned here.
func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                expect "CONTROL: a comment that mentions the call, AND the real call before the write, is green (the comment is skipped, the code counts)" 0 "check-suppression-writers: OK@@norm.Validate( call(s) outside comments, the first at byte" "$d"
d=$(mk second); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Raw(email string) {
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, email)
}

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                expect "two writes: the one before the call is red (line 6), the one after is not" 1 "$(vfirst 6 'INSERT INTO')@@!suppress/store.go:14:@@RED — 1 problem(s)" "$d"
d=$(mk neighbour); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}

func AddBulk(emails []string) {
	for _, e := range emails {
		exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, e)
	}
}
EOF
                expect "a second function that writes WITHOUT its own call, after a function that validates, is red: the call is not credited to a neighbour (line 16, not line 10)" 1 "$(vfirst 16 'INSERT INTO')@@!suppress/store.go:10:@@RED — 1 problem(s)" "$d"
d=$(mk prevfn); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Check(email string) (string, error) {
	return norm.Validate(email)
}

func Insert(email string) {
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, email)
}
EOF
                expect "the validate call in a helper function ABOVE the writing function is not credited to it" 1 "$(vfirst 10 'INSERT INTO')@@1 norm.Validate( call(s) outside comments" "$d"
d=$(mk afterfn); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	return write(n)
}

var insert = `INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`
EOF
                expect "a package-level statement BELOW a validating function is not inside any function, so the call is not credited to it" 1 "$(nofn 13 'INSERT INTO')@@1 norm.Validate( call(s) outside comments" "$d"
d=$(mk order);  put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, email)
	_, err := norm.Validate(email)
	return err
}
EOF
                expect "write first, validate after, in the same function: red (the call must come BEFORE the write)" 1 "$(vfirst 6 'INSERT INTO')@@1 norm.Validate( call(s) outside comments" "$d"
d=$(mk twofn);  put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}

func (s *Store) AddAll(emails []string) error {
	for _, e := range emails {
		n, err := norm.Validate(e)
		if err != nil {
			return err
		}
		do := func() { s.q.Exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n) }
		do()
	}
	return nil
}

func one(e string) { n, _ := norm.Validate(e); exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n) }
EOF
                expect "CONTROL: three functions (a function, a method with a closure, a one-line function) each validate before their own write: green, 3 calls found" 0 "check-suppression-writers: OK@@sanctioned writer $SAN: 3 write statement(s), 3 norm.Validate( call(s) outside comments" "$d"
d=$(mk blockc); put "$d" internal/suppress/store.go <<'EOF'
package suppress

/*
 norm.Validate(email) in a block comment looks like code to this gate
*/
func Add(email string) error {
	n, err := norm.Validate(email)
	if err != nil {
		return err
	}
	exec(`INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)`, n)
	return nil
}
EOF
                expect "a block comment in the sanctioned writer is refused (the gate cannot tell it from code)" 1 "$SAN:1: the sanctioned writer contains a block comment (/*)@@use // comments@@RED — 1 problem(s)" "$d"
d=$(mk nowrite); put "$d" internal/suppress/store.go <<'EOF'
package suppress

import "github.com/coldmail-platform/platform/internal/norm"

func Add(email string) error {
	_, err := norm.Validate(email)
	return err
}
EOF
                expect "a sanctioned writer with no write statement at all: the instrument matched nothing" 1 "$SAN:1: the sanctioned writer holds no write statement on platform.suppressions: the instrument matched nothing@@sanctioned writer $SAN: 0 write statement(s)" "$d"
d=$(mk nosan);  rm "$d/internal/suppress/store.go"
                expect "no sanctioned writer file: red, and the reason says there is nowhere to write from" 1 "$SAN:1: the sanctioned writer does not exist@@!check-suppression-writers: OK" "$d"
d=$(mk renamed); mv "$d/internal/suppress/store.go" "$d/internal/suppress/add.go"
                expect "the sanctioned writer moved to another file name: red twice over (it does not exist; the write is outside the sanctioned path)" 1 "$SAN:1: the sanctioned writer does not exist@@$(outside internal/suppress/add.go 15 'INSERT INTO')" "$d"
d=$(mk testonly); rm "$d/internal/suppress/store.go"; put "$d" internal/suppress/store_test.go <<'EOF'
package suppress

func TestX() { _ = norm.Validate; exec(`INSERT INTO platform.suppressions (email) VALUES ($1)`) }
EOF
                expect "the sanctioned file replaced by a _test.go is not a writer (tests are not scanned, so the file does not exist)" 1 "$SAN:1: the sanctioned writer does not exist" "$d"

# ---- migrations --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
migw() { put "$1" db/migrations/platform_0099_seed.up.sql <<'EOF'
CREATE TABLE platform.other (id int);
INSERT INTO platform.suppressions (user_id, email, source) VALUES ('11111111-1111-4111-8111-111111111111', 'a@example.com', 'manual');
EOF
}
d=$(mk migins); migw "$d"
                expect "an INSERT INTO platform.suppressions in a migration is red, on its own report line, naming the file and line" 1 "$(inmig db/migrations/platform_0099_seed.up.sql 2 'INSERT INTO')@@migrations: 1 write(s) flagged, 0 write(s) excused by exception@@RED — 1 problem(s): 0 write(s) outside $SAN, 0 not preceded by norm.Validate(, 0 sanctioned-writer problem(s), 1 migration write(s)" "$d"
d=$(mk migdown); put "$d" db/migrations/platform_0099_x.down.sql <<'EOF'
DELETE FROM platform.suppressions WHERE source = 'import';
UPDATE platform.suppressions SET source = 'manual';
EOF
                expect "UPDATE and DELETE in a (down) migration are red too" 1 "$(inmig db/migrations/platform_0099_x.down.sql 1 'DELETE FROM')@@$(inmig db/migrations/platform_0099_x.down.sql 2 'UPDATE')@@migrations: 2 write(s) flagged" "$d"
d=$(mk migexc); migw "$d"; printf '# register\ndb/migrations/platform_0099_seed.up.sql|the one-off import of the vendor suppression list, every row validated at build time\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a migration exception makes the migration pass, and the report shows it and its reason" 0 "check-suppression-writers: OK@@excused: db/migrations/platform_0099_seed.up.sql (1 write(s), exception on line 2): the one-off import of the vendor suppression list, every row validated at build time@@migrations: 0 write(s) flagged, 1 write(s) excused by exception@@!in a migration —" "$d"
d=$(mk migpart); migw "$d"; put "$d" db/migrations/platform_0098_other.up.sql <<'EOF'
INSERT INTO platform.suppressions (user_id, email, source) VALUES ('11111111-1111-4111-8111-111111111111', 'b@example.com', 'manual');
EOF
                printf 'db/migrations/platform_0099_seed.up.sql|the one seeded file\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "an exception covers its own migration only: another migration with a write is still red" 1 "$(inmig db/migrations/platform_0098_other.up.sql 1 'INSERT INTO')@@!platform_0099_seed.up.sql:2:" "$d"
d=$(mk excnotmig); w "$d" internal/app/q.go '"INSERT INTO platform.suppressions (email) VALUES ($1)"'; printf 'internal/app/q.go|the old importer\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "an exception for a non-migration file is stale AND excuses nothing: the write is still red" 1 "tests/suppression-writer-exceptions.txt:1: stale exception for internal/app/q.go: exceptions cover db/migrations/*.sql only@@$(outside internal/app/q.go 3 'INSERT INTO')" "$d"
d=$(mk excsan); printf 'internal/suppress/store.go|the writer\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "the sanctioned writer needs no exception and cannot be given one (it is not a migration)" 1 "tests/suppression-writer-exceptions.txt:1: stale exception for internal/suppress/store.go: exceptions cover db/migrations/*.sql only" "$d"
d=$(mk stale1); printf 'db/migrations/platform_0099_gone.up.sql|used to seed\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a STALE exception: the file no longer exists" 1 "tests/suppression-writer-exceptions.txt:1: stale exception for db/migrations/platform_0099_gone.up.sql: the file does not exist@@RED — 1 problem(s): 0 write(s) outside $SAN, 0 not preceded by norm.Validate(, 0 sanctioned-writer problem(s), 0 migration write(s), 1 exception problem(s)" "$d"
d=$(mk stale2); printf '# note\ndb/migrations/platform_0003_suppressions.up.sql|used to seed\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a STALE exception: the migration exists but contains no write" 1 "tests/suppression-writer-exceptions.txt:2: stale exception for db/migrations/platform_0003_suppressions.up.sql: the migration contains no write to platform.suppressions" "$d"
d=$(mk blank);  migw "$d"; printf 'db/migrations/platform_0099_seed.up.sql|\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a blank reason is red, and excuses nothing" 1 "tests/suppression-writer-exceptions.txt:1: exception for db/migrations/platform_0099_seed.up.sql has a blank reason@@$(inmig db/migrations/platform_0099_seed.up.sql 2 'INSERT INTO')" "$d"
d=$(mk blank2); migw "$d"; printf 'db/migrations/platform_0099_seed.up.sql|   \t \n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a reason of only whitespace is blank too" 1 "exception for db/migrations/platform_0099_seed.up.sql has a blank reason" "$d"
d=$(mk nobar);  migw "$d"; printf 'db/migrations/platform_0099_seed.up.sql\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "an entry with no reason field at all is malformed" 1 "malformed exception line (want 'relative/path|reason'): db/migrations/platform_0099_seed.up.sql" "$d"
d=$(mk dup);    migw "$d"; printf 'db/migrations/platform_0099_seed.up.sql|first\ndb/migrations/platform_0099_seed.up.sql|second\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "the same file listed twice is red" 1 "tests/suppression-writer-exceptions.txt:2: duplicate exception for db/migrations/platform_0099_seed.up.sql (first on line 1)" "$d"
d=$(mk dotdot); printf '../elsewhere.sql|escape\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "an exception path with .. is refused" 1 "exception path '../elsewhere.sql' must be relative to the root and must not contain '..'" "$d"
d=$(mk exccrlf); migw "$d"; printf '# register\r\ndb/migrations/platform_0099_seed.up.sql | the seeded file\r\n' > "$d/tests/suppression-writer-exceptions.txt"
                expect "a register with CRLF line endings and spaces around the bar still excuses" 0 "check-suppression-writers: OK@@excused: db/migrations/platform_0099_seed.up.sql" "$d"
d=$(mk excalt); migw "$d"; printf 'db/migrations/platform_0099_seed.up.sql|alt register\n' > "$T/alt-register.txt"
                expect "--exceptions FILE names a register elsewhere" 0 "check-suppression-writers: OK@@excused: db/migrations/platform_0099_seed.up.sql (1 write(s), exception on line 1): alt register" "$d" --root "$d" --exceptions "$T/alt-register.txt"
d=$(mk noreg);  rm "$d/tests/suppression-writer-exceptions.txt"
                expect "a missing register is red, not an empty one" 1 "the exceptions file $d/tests/suppression-writer-exceptions.txt does not exist" "$d"

# ---- the floors, the arguments, the matcher's own canary ------------------------------------------------------------------------------------------------------------------------------------
d=$(mk nogo);   rm -rf "$d/internal" "$d/cmd"; mkdir -p "$d/internal" "$d/cmd"
                expect "no Go file at all: the gate matched nothing" 1 "scanned 0 Go file(s) and 0 SQL file(s) outside db/migrations/, and 1 migration file(s)@@found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk onlytest); rm -rf "$d/internal" "$d/cmd"; put "$d" internal/app/a_test.go <<'EOF'
package app
EOF
                expect "only _test.go files: no Go file is scanned, so the gate matched nothing" 1 "found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk nomig);  rm -rf "$d/db"
                expect "no migration file at all: the gate matched nothing" 1 "found 0 migration files under db/migrations/@@the instrument matched nothing" "$d"
d=$(mk noroot); expect "a root that is not a directory" 1 "the root '$T/no-such-root' is not a directory: the instrument matched nothing" "$d" --root "$T/no-such-root"
d=$(mk usage1); expect "an unknown argument is a usage error" 2 "check-suppression-writers: unknown argument '--bogus'@@usage: check-suppression-writers.sh [--root DIR] [--exceptions FILE]" "$d" --bogus
d=$(mk usage2); expect "--root with no value is a usage error" 2 "usage: check-suppression-writers.sh [--root DIR] [--exceptions FILE]" "$d" --root
mkdir -p "$T/shim"; printf '#!/bin/sh\nexit 1\n' > "$T/shim/grep"; chmod +x "$T/shim/grep"
d=$(mk canary); out="$(PATH="$T/shim:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "a matcher that finds nothing (grep replaced by a stub) fails its own canary instead of passing a clean tree" "$rc" 1 "check-suppression-writers: RED — the matcher failed its own canary@@wanted 1 match(es), found 0@@!check-suppression-writers: OK" "$out"
# a grep that works for the canaries but CRASHES on one file (here: any file containing POISON) must not read as a clean file
REAL_GREP="$(command -v grep)"; mkdir -p "$T/shim2"
printf '#!/bin/sh\nfor last; do :; done\nif "%s" -q POISON "$last" 2>/dev/null; then exit 3; fi\nexec "%s" "$@"\n' "$REAL_GREP" "$REAL_GREP" > "$T/shim2/grep"; chmod +x "$T/shim2/grep"
d=$(mk crash);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "INSERT INTO platform.suppressions (email) VALUES ($1)" // POISON
EOF
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on one file is red and names the file (a crashed matcher prints no match: it must not read as clean)" "$rc" 1 "the matcher failed on internal/app/q.go (grep exit status 3)@@1 file(s) the matcher failed on@@!check-suppression-writers: OK" "$out"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$(mk crashok)" 2>&1)"; rc=$?
                check "the same crashing-grep stub on a tree without POISON behaves like the real grep (the stub itself is not the cause)" "$rc" 0 "check-suppression-writers: OK@@0 file(s) the matcher failed on" "$out"
d=$(mk crashsan); printf '\n// POISON\n' >> "$d/internal/suppress/store.go"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on the sanctioned writer itself is red (it cannot be read, so nothing about it is proven)" "$rc" 1 "the matcher failed on $SAN (grep exit status 3)@@!check-suppression-writers: OK" "$out"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Four deliberately WRONG arrangements, each run through the same `expect`. EVERY one must be reported as a mismatch; if any is accepted, this selftest cannot fail and says so.
#   1. a gate that always exits 0, run on a violating tree that must be red                      (the selftest must go red if the gate were `exit 0`)
#   2. a gate that fails WITHOUT naming a reason, run on a violating tree that must be red for the reason   (class 10: the right exit code for no reason)
#   3. the real gate on a clean tree, expected to be red
#   4. the real gate on a violating tree, expected to be green
printf '#!/bin/sh\nexit 0\n' > "$T/stub-pass.sh"; printf '#!/bin/sh\necho boom\nexit 1\n' > "$T/stub-bare-red.sh"
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases; real="$GATE"
d=$(mk ctlv); w "$d" internal/app/q.go '"INSERT INTO platform.suppressions (email) VALUES ($1)"'
GATE="$T/stub-pass.sh";     expect "CONTROL 1 (wrong on purpose): a gate that is always green" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$T/stub-bare-red.sh"; expect "CONTROL 2 (wrong on purpose): a gate that is red for no named reason" 1 "$(outside internal/app/q.go 3 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$real"
d=$(mk ctlc); expect "CONTROL 3 (wrong on purpose): a clean tree expected to be RED" 1 "write to platform.suppressions" "$d" >/dev/null 2>&1
d=$(mk ctlg); w "$d" internal/app/q.go '"INSERT INTO platform.suppressions (email) VALUES ($1)"'; expect "CONTROL 4 (wrong on purpose): a violating tree expected to be GREEN" 0 "check-suppression-writers: OK" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 4 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] all four deliberately wrong arrangements were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 4 wrong arrangements were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
