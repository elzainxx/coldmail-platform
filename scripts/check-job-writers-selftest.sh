#!/usr/bin/env bash
# check-job-writers-selftest.sh — proves scripts/check-job-writers.sh is green on a tree whose only writer of the job tables is the job store (internal/jobs/) and RED, FOR THE REASON IT NAMES, on
# each other way a job table can be written (every table, every statement form, every quoting, the function that moves a job), on a store that is missing, empty or renamed, and on each way its
# register of exceptions can rot. Every case builds a throwaway tree (a stand-in for the repository root: internal/, cmd/, db/, tests/job-writer-exceptions.txt) in a temp directory, so it needs
# no database, no Go toolchain and no checkout.
# Every reject case asserts the printed REASON as well as the exit code (a case that went red for an unrelated reason would read as a pass forever: class 10). The selftest has a hard minimum number of
# cases, and a CONTROL proves it can say "no": a gate that always exits 0, a gate that fails without a reason, and two wrong expectations must each be reported as a mismatch.
set -u
export LC_ALL=C
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-job-writers.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=117; cases=0; ok=0; bad_expect=0

SAN=internal/jobs
OUT_TAIL="outside the sanctioned writer $SAN/ — write through the job store (internal/jobs), the one package that writes the job tables"
outside() { printf '%s:%s: write to %s (%s) %s' "$1" "$2" "$3" "$4" "$OUT_TAIL"; }   # outside <path> <line> <target> <kind>
NC_HEAD='NOT CHECKED: test files (*_test.go) and everything under tests/'

put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }   # put <dir> <relative path> < content
mk() {   # mk <name>: a fresh CLEAN tree; echoes its directory
  d="$T/$1"; rm -rf "$d"
  put "$d" internal/jobs/store.go <<'EOF'
package jobs

import "context"

// The store is the one writer of the job tables.
func (s *Store) create(ctx context.Context, tx Tx, user string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO platform.jobs (user_id, module_id) VALUES ($1::uuid, $2)`, user, "m1"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.job_events (job_id, kind, actor_kind, reason) VALUES ($1::uuid, 'created', 'user', 'draft created')`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.job_rows SET state = 'suppressed' WHERE job_id = $1::uuid AND user_id = $2::uuid`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.job_results (job_id, item_ref) SELECT job_id, id FROM platform.job_rows`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT platform.job_transition($1::uuid, $2, 'user', $3::uuid, $4)`)
	return err
}
EOF
  put "$d" internal/jobs/read.go <<'EOF'
package jobs

const get = `SELECT id, status FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid FOR UPDATE`
const rows = `SELECT ordinal, state FROM platform.job_rows WHERE job_id = $1::uuid`
EOF
  put "$d" internal/money/jobs.go <<'EOF'
package money

// money only READS the job: a row lock is not a write.
const job = `SELECT j.user_id::text, j.module_id, j.status FROM platform.jobs j WHERE j.id = $1::uuid`
const lock = `SELECT status FROM platform.jobs WHERE id = $1::uuid FOR UPDATE`
const lockOf = `SELECT 1 FROM platform.jobs j WHERE j.id = $1::uuid FOR UPDATE OF j`
EOF
  put "$d" internal/app/app.go <<'EOF'
package app

const list = "SELECT id, email FROM platform.listable_users ORDER BY created_at"
EOF
  put "$d" cmd/platform/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" db/migrations/platform_0011_job_lifecycle.up.sql <<'EOF'
CREATE TABLE platform.job_transitions (from_status text, to_status text);
INSERT INTO platform.job_transitions (from_status, to_status) VALUES ('draft', 'previewed');
CREATE FUNCTION platform.job_transition(p_job uuid) RETURNS void LANGUAGE sql AS $$ UPDATE platform.jobs SET status = 'x' $$;
EOF
  put "$d" tests/job-writer-exceptions.txt <<'EOF'
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
w() { put "$1" "$2" <<EOF
package app

const q = $3
EOF
}

# ---- clean trees -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk clean);  expect "clean: the only writer is the store; money reads with FOR UPDATE; a migration seeds job_transitions" 0 "check-job-writers: OK@@scanned 5 Go file(s) and 0 SQL file(s) outside db/migrations/; the sanctioned writer $SAN/ holds 2 of the Go files; 1 migration file(s) are not scanned@@sanctioned writer $SAN/: 5 write statement(s) in 2 Go file(s)@@0 write(s) outside the sanctioned writer, 0 write(s) excused by exception, 0 sanctioned-writer problem(s), 0 exception problem(s)@@!write to platform." "$d"
d=$(mk clean2); expect "a clean run prints its scope and what it did NOT check" 0 "scope: non-test *.go under internal/ and cmd/, and *.sql under db/ except db/migrations/@@$NC_HEAD@@shell scripts and Docs@@db/migrations/*.sql (DDL by the privileged migrator@@string concatenation, fmt.Sprintf or a variable@@a write through a view, a rule or another function than platform.job_transition@@whether the store scopes a statement by the user@@the database's own walls (the grants and the guard)@@Comments are NOT stripped" "$d"
d=$(mk reads);  put "$d" internal/app/r.go <<'EOF'
package app

const a = "SELECT status FROM platform.jobs WHERE id = $1 FOR UPDATE"
const b = `SELECT 1 FROM platform.job_rows FOR UPDATE OF platform.job_rows`
const c = "COPY platform.job_rows TO STDOUT"
const e = "COPY (SELECT id FROM platform.job_results) TO STDOUT"
const f = "GRANT SELECT ON platform.job_events TO platform_admin"
const g = "SELECT * FROM platform.job_transitions WHERE from_status = $1"
const h = "SELECT 1 FROM platform.job_events e JOIN platform.jobs j ON j.id = e.job_id WHERE e.job_id = $1"
EOF
                expect "reads, COPY ... TO, FOR UPDATE [OF], a GRANT and a JOIN anywhere are not writes" 0 "check-job-writers: OK" "$d"
d=$(mk ddl);    put "$d" db/queries/schema.sql <<'EOF'
ALTER TABLE platform.jobs ADD CONSTRAINT c CHECK (true);
COMMENT ON TABLE platform.job_events IS 'Append-only.';
CREATE TRIGGER t BEFORE UPDATE OR DELETE ON platform.job_events FOR EACH ROW EXECUTE FUNCTION f();
CREATE TRIGGER u AFTER UPDATE OF status ON platform.jobs FOR EACH ROW EXECUTE FUNCTION f();
ALTER TABLE platform.job_rows ADD CONSTRAINT fk FOREIGN KEY (job_id) REFERENCES platform.jobs (id) ON DELETE RESTRICT;
GRANT SELECT, INSERT, UPDATE ON platform.jobs TO platform_app;
EOF
                expect "DDL, a trigger that names the table, REFERENCES ... ON DELETE and GRANTs in a .sql file under db/ are not writes" 0 "check-job-writers: OK@@and 1 SQL file(s) outside db/migrations/" "$d"
d=$(mk whole);  put "$d" internal/app/x.go <<'EOF'
package app

const a = "INSERT INTO platform.jobs_archive (id) VALUES ($1)"
const b = "INSERT INTO platform.job_rows2 (id) VALUES ($1)"
const c = `INSERT INTO "platform"."job_results_old" (id) VALUES ($1)`
const d = "INSERT INTO verify.jobs (id) VALUES ($1)"
const e = "UPDATE send_managed.jobs SET status = $1"
const f = "INSERT INTO myplatform.jobs (id) VALUES ($1)"
const g = "xinsert into platform.jobs"
const h = "DELETE FROM platform.ledger WHERE id = $1"
const i = "UPDATE platform.jobs_guard SET x = 1"
const j = "SELECT platform.job_transition_log($1)"
const k = "SELECT my_job_transition($1)"
const l = "SELECT platform.job_transitions FROM x"
EOF
                expect "whole word only: *_archive, *2, *_old, another schema, a longer schema name, a longer word before the keyword, other tables, and a function with a longer name pass" 0 "check-job-writers: OK" "$d"
d=$(mk testfile); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "INSERT INTO platform.jobs (user_id, module_id) VALUES ($1, $2)"
const move = "SELECT platform.job_transition($1, 'previewed', 'engine', NULL, 'x')"
EOF
                expect "a _test.go file with a write is ignored, and the report says tests were not checked" 0 "check-job-writers: OK@@scanned 5 Go file(s)@@$NC_HEAD" "$d"
d=$(mk outside); put "$d" tests/harness/q.go <<'EOF'
package harness

const q = "INSERT INTO platform.job_rows (id) VALUES ($1)"
EOF
                put "$d" scripts/g.sh <<'EOF'
psql -c "DELETE FROM platform.jobs"
EOF
                expect "Go under tests/ and shell under scripts/ are outside the scope" 0 "check-job-writers: OK@@scanned 5 Go file(s) and 0 SQL file(s)" "$d"
d=$(mk migs);   put "$d" db/migrations/platform_0099_x.down.sql <<'EOF'
DELETE FROM platform.job_transitions;
UPDATE platform.jobs SET status = 'draft';
TRUNCATE platform.job_events;
EOF
                expect "migrations (up and down) may write: they are DDL by the migrator and are not scanned, and the report counts them" 0 "check-job-writers: OK@@2 migration file(s) are not scanned" "$d"
d=$(mk sanctwo); put "$d" internal/jobs/more.go <<'EOF'
package jobs

const a = `DELETE FROM platform.job_rows WHERE job_id = $1::uuid`
const b = `TRUNCATE platform.job_results`
EOF
                put "$d" internal/jobs/sub/deep.go <<'EOF'
package sub

const c = `INSERT INTO platform.job_events (job_id) VALUES ($1)`
EOF
                expect "any non-test file of the store may write, a nested directory included; the count is printed" 0 "check-job-writers: OK@@sanctioned writer $SAN/: 8 write statement(s) in 4 Go file(s)" "$d"
d=$(mk crlfok); put "$d" internal/jobs/store.go <<'EOF'
package jobs

func f() {
	exec(`INSERT INTO platform.jobs
		(user_id, module_id) VALUES ($1, $2)`)
}
EOF
                sed -i 's/$/\r/' "$d/internal/jobs/store.go"
                expect "the store with CRLF line endings, a statement split over two lines, is green" 0 "check-job-writers: OK@@sanctioned writer $SAN/: 1 write statement(s) in 2 Go file(s)" "$d"

# ---- every other way to write a table: red, naming the file, the line, the kind and the table -------------------------------------------------------------------------------------------
d=$(mk ins);    w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id, module_id) VALUES ($1, $2)"'
                expect "INSERT INTO platform.jobs in another file names the file, the line, the kind and the table" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')@@check-job-writers: 1 write(s) outside the sanctioned writer@@RED — 1 problem(s): 1 write(s) outside $SAN/, 0 sanctioned-writer problem(s), 0 exception problem(s), 0 file(s) the matcher failed on" "$d"
d=$(mk insr);   w "$d" internal/app/q.go '"INSERT INTO platform.job_rows (id) VALUES ($1)"'
                expect "INSERT INTO platform.job_rows" 1 "$(outside internal/app/q.go 3 platform.job_rows 'INSERT INTO')" "$d"
d=$(mk insres); w "$d" internal/app/q.go '"INSERT INTO platform.job_results (job_id) VALUES ($1)"'
                expect "INSERT INTO platform.job_results" 1 "$(outside internal/app/q.go 3 platform.job_results 'INSERT INTO')" "$d"
d=$(mk insev);  w "$d" internal/app/q.go '"INSERT INTO platform.job_events (job_id) VALUES ($1)"'
                expect "INSERT INTO platform.job_events" 1 "$(outside internal/app/q.go 3 platform.job_events 'INSERT INTO')" "$d"
d=$(mk instr);  w "$d" internal/app/q.go '"INSERT INTO platform.job_transitions (from_status, to_status) VALUES ($1, $2)"'
                expect "INSERT INTO platform.job_transitions (the lifecycle's own table)" 1 "$(outside internal/app/q.go 3 platform.job_transitions 'INSERT INTO')" "$d"
d=$(mk cmd);    put "$d" cmd/platform/q.go <<'EOF'
package main

import "fmt"

func list() { fmt.Println("INSERT INTO platform.jobs (user_id) VALUES ('x')") }
EOF
                expect "a file under cmd/ is scanned too" 1 "$(outside cmd/platform/q.go 5 platform.jobs 'INSERT INTO')" "$d"
d=$(mk upd);    w "$d" internal/app/q.go '"UPDATE platform.jobs SET status = $1 WHERE id = $2"'
                expect "UPDATE platform.jobs SET status (the engine writing a status itself)" 1 "$(outside internal/app/q.go 3 platform.jobs 'UPDATE')" "$d"
d=$(mk updr);   w "$d" internal/app/q.go '"UPDATE platform.job_rows SET submitted_at = now() WHERE id = $1"'
                expect "UPDATE platform.job_rows" 1 "$(outside internal/app/q.go 3 platform.job_rows 'UPDATE')" "$d"
d=$(mk updonly); w "$d" internal/app/q.go '"UPDATE ONLY platform.job_results SET rule = $1"'
                expect "UPDATE ONLY platform.job_results" 1 "$(outside internal/app/q.go 3 platform.job_results 'UPDATE')" "$d"
d=$(mk del);    w "$d" internal/app/q.go '"DELETE FROM platform.job_events WHERE job_id = $1"'
                expect "DELETE FROM platform.job_events" 1 "$(outside internal/app/q.go 3 platform.job_events 'DELETE FROM')" "$d"
d=$(mk delonly); w "$d" internal/app/q.go '"DELETE FROM ONLY platform.jobs"'
                expect "DELETE FROM ONLY platform.jobs" 1 "$(outside internal/app/q.go 3 platform.jobs 'DELETE FROM')" "$d"
d=$(mk trunc);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "TRUNCATE platform.jobs"
const b = "TRUNCATE TABLE platform.job_rows"
const c = "TRUNCATE TABLE ONLY platform.job_results CASCADE"
const d = "TRUNCATE platform.ledger, platform.job_events"
const e = "TRUNCATE platform.job_transitions, platform.ledger"
EOF
                expect "TRUNCATE, TRUNCATE TABLE, TRUNCATE TABLE ONLY, and a list that holds the table after or before another" 1 "$(outside internal/app/q.go 3 platform.jobs TRUNCATE)@@$(outside internal/app/q.go 4 platform.job_rows TRUNCATE)@@$(outside internal/app/q.go 5 platform.job_results TRUNCATE)@@$(outside internal/app/q.go 6 platform.job_events TRUNCATE)@@$(outside internal/app/q.go 7 platform.job_transitions TRUNCATE)@@RED — 5 problem(s): 5 write(s) outside $SAN/" "$d"
d=$(mk copy);   put "$d" internal/app/q.go <<'EOF'
package app

const a = "COPY platform.job_rows (id, job_id, user_id) FROM STDIN"
const b = `COPY platform.job_results
	FROM STDIN`
EOF
                expect "COPY ... FROM, with and without a column list, and split over lines (the table is named, not the word from)" 1 "$(outside internal/app/q.go 3 platform.job_rows 'COPY ... FROM')@@$(outside internal/app/q.go 4 platform.job_results 'COPY ... FROM')" "$d"
d=$(mk merge);  w "$d" internal/app/q.go '"MERGE INTO platform.jobs j USING src ON j.id = src.id WHEN NOT MATCHED THEN DO NOTHING"'
                expect "MERGE INTO" 1 "$(outside internal/app/q.go 3 platform.jobs 'MERGE INTO')" "$d"
d=$(mk ident);  put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"platform", "job_rows"}, []string{"id", "job_id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"platform","job_results"}, []string{"job_id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{ "platform" , "job_events" }, []string{"job_id"}, rows)
}
EOF
                expect "pgx.Identifier{\"platform\", \"<table>\"} (what CopyFrom writes through) is flagged, spaced and unspaced" 1 "$(outside internal/app/q.go 4 platform.job_rows 'pgx.Identifier, as CopyFrom writes through it')@@$(outside internal/app/q.go 5 platform.job_results 'pgx.Identifier, as CopyFrom writes through it')@@$(outside internal/app/q.go 6 platform.job_events 'pgx.Identifier, as CopyFrom writes through it')" "$d"
d=$(mk identok); put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"platform", "ledger"}, []string{"id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"platform", "job_rows_archive"}, []string{"id"}, rows)
}
EOF
                expect "the control of the previous case: a CopyFrom into another table, or a longer name, is not flagged" 0 "check-job-writers: OK" "$d"
d=$(mk fn);     put "$d" internal/app/q.go <<'EOF'
package app

const a = "SELECT platform.job_transition($1::uuid, 'running', 'engine', NULL, 'submitted')"
const b = `SELECT "platform"."job_transition"($1)`
const c = "select job_transition ($1)"
EOF
                expect "a CALL of platform.job_transition( outside the store is flagged: it is the one function that moves a job (qualified, quoted, bare, spaced)" 1 "$(outside internal/app/q.go 3 'platform.job_transition()' 'a call of platform.job_transition()')@@$(outside internal/app/q.go 4 'platform.job_transition()' 'a call of platform.job_transition()')@@$(outside internal/app/q.go 5 'platform.job_transition()' 'a call of platform.job_transition()')@@RED — 3 problem(s)" "$d"
d=$(mk lower);  w "$d" internal/app/q.go '"insert into platform.job_rows (id) values ($1)"'
                expect "lower case" 1 "$(outside internal/app/q.go 3 platform.job_rows 'INSERT INTO')" "$d"
d=$(mk mixed);  w "$d" internal/app/q.go '"InSeRt InTo PlAtFoRm.JoB_rOwS (id) values ($1)"'
                expect "mixed case" 1 "$(outside internal/app/q.go 3 platform.job_rows 'INSERT INTO')" "$d"
d=$(mk raw);    put "$d" internal/app/q.go <<'EOF'
package app

const q = `
	INSERT
	  INTO
		platform.job_rows (id, job_id)
	VALUES ($1, $2)`
EOF
                expect "split over lines in a Go raw string (INSERT, newline, tabs, INTO, newline, the table): reported at the INSERT line" 1 "$(outside internal/app/q.go 4 platform.job_rows 'INSERT INTO')@@!internal/app/q.go:5:@@!internal/app/q.go:6:" "$d"
d=$(mk crlf);   put "$d" internal/app/q.go <<'EOF'
package app

const q = `INSERT INTO
		platform.jobs (user_id)`
EOF
                sed -i 's/$/\r/' "$d/internal/app/q.go"
                expect "the same split statement in a file with CRLF line endings" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')" "$d"
d=$(mk quoted); put "$d" internal/app/q.go <<'EOF'
package app

const a = `INSERT INTO "platform"."jobs" (user_id) VALUES ($1)`
const b = `INSERT INTO platform."job_rows" (id) VALUES ($1)`
const c = `INSERT INTO "platform".job_events (job_id) VALUES ($1)`
const d = `INSERT INTO platform . job_results (job_id) VALUES ($1)`
EOF
                expect "quoted and spaced identifiers: \"platform\".\"jobs\", platform.\"job_rows\", \"platform\".job_events, platform . job_results" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')@@$(outside internal/app/q.go 4 platform.job_rows 'INSERT INTO')@@$(outside internal/app/q.go 5 platform.job_events 'INSERT INTO')@@$(outside internal/app/q.go 6 platform.job_results 'INSERT INTO')@@RED — 4 problem(s)" "$d"
d=$(mk escaped); w "$d" internal/app/q.go '"INSERT INTO \"platform\".\"job_rows\" (id) VALUES ($1)"'
                expect "quoted identifiers escaped inside a Go interpreted string" 1 "$(outside internal/app/q.go 3 platform.job_rows 'INSERT INTO')" "$d"
d=$(mk bare);   w "$d" internal/app/q.go '"INSERT INTO job_rows (id) VALUES ($1)"'
                expect "an unqualified INSERT INTO job_rows is flagged (it would resolve through search_path)" 1 "$(outside internal/app/q.go 3 platform.job_rows 'INSERT INTO')" "$d"
d=$(mk barejobs); w "$d" internal/app/q.go '"UPDATE jobs SET status = $1"'
                expect "an unqualified UPDATE jobs is flagged too" 1 "$(outside internal/app/q.go 3 platform.jobs 'UPDATE')" "$d"
d=$(mk twice);  w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1); DELETE FROM platform.job_rows"'
                expect "two writes in a row on one line are both reported (a consumed boundary would hide the second)" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')@@$(outside internal/app/q.go 3 platform.job_rows 'DELETE FROM')@@RED — 2 problem(s): 2 write(s) outside $SAN/" "$d"
d=$(mk with);   w "$d" internal/app/q.go '`WITH x AS (INSERT INTO platform.job_events (job_id) VALUES ($1) RETURNING id) SELECT id FROM x`'
                expect "a write inside a WITH (a data-modifying CTE)" 1 "$(outside internal/app/q.go 3 platform.job_events 'INSERT INTO')" "$d"
d=$(mk comment); put "$d" internal/app/q.go <<'EOF'
package app

// the old code ran UPDATE platform.jobs SET status here, before the store existed
const q = 1
EOF
                expect "a comment containing the statement IS flagged (comments are not stripped)" 1 "$(outside internal/app/q.go 3 platform.jobs 'UPDATE')" "$d"
d=$(mk sqlfile); put "$d" db/queries/move.sql <<'EOF'
-- move a job
UPDATE platform.jobs SET status = 'running' WHERE id = $1;
EOF
                expect "a .sql file under db/ outside db/migrations is scanned and flagged" 1 "scanned 5 Go file(s) and 1 SQL file(s) outside db/migrations/@@$(outside db/queries/move.sql 2 platform.jobs 'UPDATE')" "$d"
d=$(mk money);  put "$d" internal/money/jobs.go <<'EOF'
package money

const done = `UPDATE platform.jobs SET status = 'cancelled' WHERE id = $1::uuid`
EOF
                expect "internal/money may SELECT a job but an UPDATE there is flagged (the clean tree's money files are not)" 1 "$(outside internal/money/jobs.go 3 platform.jobs 'UPDATE')" "$d"
d=$(mk engine); put "$d" internal/jobengine/starter.go <<'EOF'
package jobengine

const move = "UPDATE platform.jobs SET status = 'running' WHERE id = $1"
EOF
                expect "THE FAULT of the fault proof: a second writer file under internal/jobengine/ is flagged" 1 "$(outside internal/jobengine/starter.go 3 platform.jobs 'UPDATE')@@RED — 1 problem(s)" "$d"
d=$(mk prefix); put "$d" internal/jobsx/q.go <<'EOF'
package jobsx

const q = `INSERT INTO platform.jobs (user_id) VALUES ($1)`
EOF
                put "$d" internal/jobs_old/q.go <<'EOF'
package jobsold

const q = `INSERT INTO platform.job_rows (id) VALUES ($1)`
EOF
                expect "a directory whose name only STARTS with jobs (jobsx, jobs_old) is not the sanctioned package" 1 "$(outside internal/jobsx/q.go 3 platform.jobs 'INSERT INTO')@@$(outside internal/jobs_old/q.go 3 platform.job_rows 'INSERT INTO')@@RED — 2 problem(s)" "$d"
d=$(mk sibling); put "$d" internal/other/jobs/q.go <<'EOF'
package jobs

const q = `INSERT INTO platform.jobs (user_id) VALUES ($1)`
EOF
                expect "a package named jobs elsewhere (internal/other/jobs) is not the sanctioned path" 1 "$(outside internal/other/jobs/q.go 3 platform.jobs 'INSERT INTO')" "$d"
d=$(mk two);    put "$d" internal/app/a.go <<'EOF'
package app

const a = "UPDATE platform.jobs SET status = $1"
EOF
                put "$d" internal/api/b.go <<'EOF'
package api

const b = "DELETE FROM platform.job_results"
EOF
                expect "writers in two files are each reported with their own path and line" 1 "$(outside internal/app/a.go 3 platform.jobs 'UPDATE')@@$(outside internal/api/b.go 3 platform.job_results 'DELETE FROM')@@RED — 2 problem(s)" "$d"

# ---- the sanctioned writer must exist and write ---------------------------------------------------------------------------------------------------------------------------------------
d=$(mk nosan);  rm -rf "$d/internal/jobs"
                expect "no store package at all: red, and the reason says there is nowhere to write from" 1 "$SAN/:1: the sanctioned writer does not exist (no non-test Go file under $SAN/)@@sanctioned writer $SAN/: 0 write statement(s) in 0 Go file(s)@@!check-job-writers: OK" "$d"
d=$(mk testonly); rm -rf "$d/internal/jobs"; put "$d" internal/jobs/store_test.go <<'EOF'
package jobs

func TestX() { exec(`INSERT INTO platform.jobs (user_id) VALUES ($1)`) }
EOF
                expect "the store replaced by a _test.go is not a writer (tests are not scanned, so no file exists)" 1 "$SAN/:1: the sanctioned writer does not exist (no non-test Go file under $SAN/)" "$d"
d=$(mk nowrite); rm -rf "$d/internal/jobs"; put "$d" internal/jobs/store.go <<'EOF'
package jobs

const get = `SELECT id FROM platform.jobs WHERE id = $1::uuid`
EOF
                expect "a store with no write statement at all: the instrument matched nothing" 1 "$SAN/:1: the sanctioned writer holds no write statement on a job table: the instrument matched nothing@@sanctioned writer $SAN/: 0 write statement(s) in 1 Go file(s)@@RED — 1 problem(s): 0 write(s) outside $SAN/, 1 sanctioned-writer problem(s)" "$d"
d=$(mk renamed); mv "$d/internal/jobs" "$d/internal/jobstore"
                expect "the store moved to another directory: red twice over (it does not exist; its writes are outside the sanctioned path)" 1 "$SAN/:1: the sanctioned writer does not exist@@$(outside internal/jobstore/store.go 7 platform.jobs 'INSERT INTO')" "$d"
d=$(mk onefile); rm -f "$d/internal/jobs/read.go"
                expect "the control of the previous cases: the store with one file (the writer) is green" 0 "check-job-writers: OK@@sanctioned writer $SAN/: 5 write statement(s) in 1 Go file(s)" "$d"

# ---- the exceptions register ------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk exc);    w "$d" internal/app/q.go '"INSERT INTO platform.job_rows (id) VALUES ($1)"'; printf '# register\ninternal/app/q.go|the one-off backfill tool: it cannot call the store because it runs before the store exists\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception makes the file pass, and the report shows it and its reason" 0 "check-job-writers: OK@@excused: internal/app/q.go (1 write(s), exception on line 2): the one-off backfill tool: it cannot call the store because it runs before the store exists@@1 write(s) excused by exception@@!outside the sanctioned writer internal" "$d"
d=$(mk excpart); w "$d" internal/app/q.go '"INSERT INTO platform.job_rows (id) VALUES ($1)"'; put "$d" internal/app/other.go <<'EOF'
package app

const x = "DELETE FROM platform.jobs"
EOF
                printf 'internal/app/q.go|excused\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception covers its own file only: another file with a write is still red" 1 "$(outside internal/app/other.go 3 platform.jobs 'DELETE FROM')@@!internal/app/q.go:3:" "$d"
d=$(mk excsan); printf 'internal/jobs/store.go|the store\n' > "$d/tests/job-writer-exceptions.txt"
                expect "the store needs no exception and cannot be given one" 1 "tests/job-writer-exceptions.txt:1: stale exception for internal/jobs/store.go: the sanctioned writer package $SAN/ needs no exception" "$d"
d=$(mk excsan2); printf 'internal/jobs/gone.go|the store\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception inside the store's directory is stale whether the file exists or not" 1 "tests/job-writer-exceptions.txt:1: stale exception for internal/jobs/gone.go: the sanctioned writer package $SAN/ needs no exception" "$d"
d=$(mk exctest); w "$d" internal/app/q_test.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q_test.go|a test seeds jobs\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception for a test file is stale: tests are not scanned, and the entry excuses nothing" 1 "tests/job-writer-exceptions.txt:1: stale exception for internal/app/q_test.go: exceptions cover the scanned files only" "$d"
d=$(mk excmig); printf 'db/migrations/platform_0011_job_lifecycle.up.sql|seeds job_transitions\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception for a migration is stale: migrations are not scanned" 1 "tests/job-writer-exceptions.txt:1: stale exception for db/migrations/platform_0011_job_lifecycle.up.sql: exceptions cover the scanned files only" "$d"
d=$(mk stale1); printf 'internal/app/gone.go|used to write\n' > "$d/tests/job-writer-exceptions.txt"
                expect "a STALE exception: the file no longer exists" 1 "tests/job-writer-exceptions.txt:1: stale exception for internal/app/gone.go: the file does not exist@@RED — 1 problem(s): 0 write(s) outside $SAN/, 0 sanctioned-writer problem(s), 1 exception problem(s)" "$d"
d=$(mk stale2); printf '# note\ninternal/app/app.go|used to write\n' > "$d/tests/job-writer-exceptions.txt"
                expect "a STALE exception: the file exists and is scanned but contains no write" 1 "tests/job-writer-exceptions.txt:2: stale exception for internal/app/app.go: the file contains no write to a job table" "$d"
d=$(mk blank);  w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q.go|\n' > "$d/tests/job-writer-exceptions.txt"
                expect "a blank reason is red, and excuses nothing" 1 "tests/job-writer-exceptions.txt:1: exception for internal/app/q.go has a blank reason@@$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')" "$d"
d=$(mk blank2); w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q.go|   \t \n' > "$d/tests/job-writer-exceptions.txt"
                expect "a reason of only whitespace is blank too" 1 "exception for internal/app/q.go has a blank reason" "$d"
d=$(mk nobar);  w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q.go\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an entry with no reason field at all is malformed" 1 "malformed exception line (want 'relative/path|reason'): internal/app/q.go" "$d"
d=$(mk nopath); printf '|just a reason\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an entry with no path is malformed" 1 "malformed exception line (the path is empty; want 'relative/path|reason'): |just a reason" "$d"
d=$(mk dup);    w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q.go|first\ninternal/app/q.go|second\n' > "$d/tests/job-writer-exceptions.txt"
                expect "the same file listed twice is red" 1 "tests/job-writer-exceptions.txt:2: duplicate exception for internal/app/q.go (first on line 1)" "$d"
d=$(mk dotdot); printf '../elsewhere.go|escape\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception path with .. is refused" 1 "exception path '../elsewhere.go' must be relative to the root and must not contain '..'" "$d"
d=$(mk abs);    printf '/etc/passwd.go|escape\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an absolute exception path is refused" 1 "exception path '/etc/passwd.go' must be relative to the root" "$d"
d=$(mk exccrlf); w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf '# register\r\ninternal/app/q.go | the seeded file\r\n' > "$d/tests/job-writer-exceptions.txt"
                expect "a register with CRLF line endings and spaces around the bar still excuses" 0 "check-job-writers: OK@@excused: internal/app/q.go" "$d"
d=$(mk excalt); w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; printf 'internal/app/q.go|alt register\n' > "$T/alt-register.txt"
                expect "--exceptions FILE names a register elsewhere" 0 "check-job-writers: OK@@excused: internal/app/q.go (1 write(s), exception on line 1): alt register" "$d" --root "$d" --exceptions "$T/alt-register.txt"
d=$(mk noreg);  rm "$d/tests/job-writer-exceptions.txt"
                expect "a missing register is red, not an empty one" 1 "the exceptions file $d/tests/job-writer-exceptions.txt does not exist" "$d"
d=$(mk excsql); put "$d" db/queries/move.sql <<'EOF'
UPDATE platform.jobs SET status = 'running' WHERE id = $1;
EOF
                printf 'db/queries/move.sql|a maintenance query run by hand\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception for a .sql file under db/ outside migrations is allowed" 0 "check-job-writers: OK@@excused: db/queries/move.sql" "$d"

# ---- the floors, the arguments, the matcher's own canary ------------------------------------------------------------------------------------------------------------------------------------
d=$(mk nogo);   rm -rf "$d/internal" "$d/cmd"; mkdir -p "$d/internal" "$d/cmd"
                expect "no Go file at all: the gate matched nothing" 1 "scanned 0 Go file(s) and 0 SQL file(s) outside db/migrations/@@found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk onlytest); rm -rf "$d/internal" "$d/cmd"; put "$d" internal/app/a_test.go <<'EOF'
package app
EOF
                expect "only _test.go files: no Go file is scanned, so the gate matched nothing" 1 "found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk noroot); expect "a root that is not a directory" 1 "the root '$T/no-such-root' is not a directory: the instrument matched nothing" "$d" --root "$T/no-such-root"
d=$(mk usage1); expect "an unknown argument is a usage error" 2 "check-job-writers: unknown argument '--bogus'@@usage: check-job-writers.sh [--root DIR] [--exceptions FILE]" "$d" --bogus
d=$(mk usage2); expect "--root with no value is a usage error" 2 "usage: check-job-writers.sh [--root DIR] [--exceptions FILE]" "$d" --root
d=$(mk usage3); expect "--exceptions with no value is a usage error" 2 "usage: check-job-writers.sh [--root DIR] [--exceptions FILE]" "$d" --exceptions
mkdir -p "$T/shim"; printf '#!/bin/sh\nexit 1\n' > "$T/shim/grep"; chmod +x "$T/shim/grep"
d=$(mk canary); out="$(PATH="$T/shim:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "a matcher that finds nothing (grep replaced by a stub) fails its own canary instead of passing a clean tree" "$rc" 1 "check-job-writers: RED — the matcher failed its own canary@@wanted 1 match(es), found 0@@!check-job-writers: OK" "$out"
# a grep that works for the canaries but CRASHES on one file (here: any file containing POISON) must not read as a clean file
REAL_GREP="$(command -v grep)"; mkdir -p "$T/shim2"
printf '#!/bin/sh\nfor last; do :; done\nif "%s" -q POISON "$last" 2>/dev/null; then exit 3; fi\nexec "%s" "$@"\n' "$REAL_GREP" "$REAL_GREP" > "$T/shim2/grep"; chmod +x "$T/shim2/grep"
d=$(mk crash);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "INSERT INTO platform.jobs (user_id) VALUES ($1)" // POISON
EOF
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on one file is red and names the file (a crashed matcher prints no match: it must not read as clean)" "$rc" 1 "the matcher failed on internal/app/q.go (grep exit status 3)@@1 file(s) the matcher failed on@@!check-job-writers: OK" "$out"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$(mk crashok)" 2>&1)"; rc=$?
                check "the same crashing-grep stub on a tree without POISON behaves like the real grep (the stub itself is not the cause)" "$rc" 0 "check-job-writers: OK@@0 file(s) the matcher failed on" "$out"
d=$(mk crashsan); printf '\n// POISON\n' >> "$d/internal/jobs/store.go"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on a file of the store itself is red (it cannot be read, so nothing about it is proven)" "$rc" 1 "the matcher failed on internal/jobs/store.go (grep exit status 3)@@!check-job-writers: OK" "$out"

# ---- public.job_items: the shared table of schema public is policed like the job tables (the same sanctioned writer, the same reason, the table named public.job_items) --------------------------------
d=$(mk jins);   w "$d" internal/app/q.go '"INSERT INTO public.job_items (campaign_id, user_id, recipient_email) VALUES ($1, $2, $3)"'
                expect "INSERT INTO public.job_items in another file names the file, the line, the kind and the table (schema public)" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')@@check-job-writers: 1 write(s) outside the sanctioned writer@@RED — 1 problem(s): 1 write(s) outside $SAN/, 0 sanctioned-writer problem(s), 0 exception problem(s), 0 file(s) the matcher failed on@@!write to platform." "$d"
d=$(mk jupd);   w "$d" internal/app/q.go '"UPDATE public.job_items SET status = $1 WHERE id = $2"'
                expect "UPDATE public.job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'UPDATE')" "$d"
d=$(mk jupdonly); w "$d" internal/app/q.go '"UPDATE ONLY public.job_items SET status = $1"'
                expect "UPDATE ONLY public.job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'UPDATE')" "$d"
d=$(mk jdel);   w "$d" internal/app/q.go '"DELETE FROM public.job_items WHERE id = $1"'
                expect "DELETE FROM public.job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'DELETE FROM')" "$d"
d=$(mk jdelonly); w "$d" internal/app/q.go '"DELETE FROM ONLY public.job_items"'
                expect "DELETE FROM ONLY public.job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'DELETE FROM')" "$d"
d=$(mk jtrunc); put "$d" internal/app/q.go <<'EOF'
package app

const a = "TRUNCATE public.job_items"
const b = "TRUNCATE TABLE public.job_items"
const c = "TRUNCATE TABLE ONLY public.job_items CASCADE"
const d = "TRUNCATE platform.ledger, public.job_items"
const e = "TRUNCATE public.job_items, platform.ledger"
const f = "TRUNCATE public.users, \"public\".\"job_items\" RESTART IDENTITY"
EOF
                expect "TRUNCATE public.job_items: plain, TABLE, TABLE ONLY, in a list after another table, in a list before another, and quoted at the end of a list" 1 "$(outside internal/app/q.go 3 public.job_items TRUNCATE)@@$(outside internal/app/q.go 4 public.job_items TRUNCATE)@@$(outside internal/app/q.go 5 public.job_items TRUNCATE)@@$(outside internal/app/q.go 6 public.job_items TRUNCATE)@@$(outside internal/app/q.go 7 public.job_items TRUNCATE)@@$(outside internal/app/q.go 8 public.job_items TRUNCATE)@@RED — 6 problem(s): 6 write(s) outside $SAN/" "$d"
d=$(mk jlist);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "TRUNCATE send_managed.jobs, public.job_items"
const b = "TRUNCATE public.job_items_archive, platform.job_rows"
const c = "TRUNCATE public.job_items, send_byo.job_items"
const d = "TRUNCATE send_managed.job_items, send_byo.job_items"
EOF
                expect "a TRUNCATE list names the table that is flagged, not a longer name or another schema's table before it: public.job_items after send_managed.jobs, platform.job_rows after public.job_items_archive, public.job_items before send_byo.job_items, and a list of other schemas' job_items passes" 1 "$(outside internal/app/q.go 3 public.job_items TRUNCATE)@@$(outside internal/app/q.go 4 platform.job_rows TRUNCATE)@@$(outside internal/app/q.go 5 public.job_items TRUNCATE)@@!internal/app/q.go:6:@@RED — 3 problem(s): 3 write(s) outside $SAN/" "$d"
d=$(mk jcopy);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "COPY public.job_items (campaign_id, user_id, recipient_email) FROM STDIN"
const b = `COPY public.job_items
	FROM STDIN`
EOF
                expect "COPY public.job_items ... FROM, with a column list, and split over lines (reported at the COPY line)" 1 "$(outside internal/app/q.go 3 public.job_items 'COPY ... FROM')@@$(outside internal/app/q.go 4 public.job_items 'COPY ... FROM')@@RED — 2 problem(s)" "$d"
d=$(mk jmerge); w "$d" internal/app/q.go '"MERGE INTO public.job_items j USING src ON j.id = src.id WHEN NOT MATCHED THEN DO NOTHING"'
                expect "MERGE INTO public.job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'MERGE INTO')" "$d"
d=$(mk jident); put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"public", "job_items"}, []string{"campaign_id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"public","job_items"}, []string{"campaign_id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{ "public" , "job_items" }, []string{"campaign_id"}, rows)
}
EOF
                expect "pgx.Identifier{\"public\", \"job_items\"} (what CopyFrom writes through) is flagged, spaced and unspaced" 1 "$(outside internal/app/q.go 4 public.job_items 'pgx.Identifier, as CopyFrom writes through it')@@$(outside internal/app/q.go 5 public.job_items 'pgx.Identifier, as CopyFrom writes through it')@@$(outside internal/app/q.go 6 public.job_items 'pgx.Identifier, as CopyFrom writes through it')@@RED — 3 problem(s)" "$d"
d=$(mk jidentok); put "$d" internal/app/q.go <<'EOF'
package app

func load(tx pgx.Tx, rows pgx.CopyFromSource) {
	tx.CopyFrom(ctx, pgx.Identifier{"public", "users"}, []string{"id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"public", "job_items_archive"}, []string{"id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"send_managed", "job_items"}, []string{"id"}, rows)
	tx.CopyFrom(ctx, pgx.Identifier{"platform", "job_items"}, []string{"id"}, rows)
}
EOF
                expect "the control of the previous case: a CopyFrom into public.users, a longer name, another schema's job_items or platform.job_items is not flagged" 0 "check-job-writers: OK" "$d"
d=$(mk jquoted); put "$d" internal/app/q.go <<'EOF'
package app

const a = `INSERT INTO "public"."job_items" (user_id) VALUES ($1)`
const b = `INSERT INTO public."job_items" (user_id) VALUES ($1)`
const c = `INSERT INTO "public".job_items (user_id) VALUES ($1)`
const d = `INSERT INTO public . job_items (user_id) VALUES ($1)`
EOF
                expect "quoted and spaced identifiers: \"public\".\"job_items\", public.\"job_items\", \"public\".job_items, public . job_items" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')@@$(outside internal/app/q.go 4 public.job_items 'INSERT INTO')@@$(outside internal/app/q.go 5 public.job_items 'INSERT INTO')@@$(outside internal/app/q.go 6 public.job_items 'INSERT INTO')@@RED — 4 problem(s)" "$d"
d=$(mk jescaped); w "$d" internal/app/q.go '"INSERT INTO \"public\".\"job_items\" (user_id) VALUES ($1)"'
                expect "quoted public.job_items escaped inside a Go interpreted string" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')" "$d"
d=$(mk jbare);  put "$d" internal/app/q.go <<'EOF'
package app

const a = "INSERT INTO job_items (user_id) VALUES ($1)"
const b = "UPDATE job_items SET status = $1"
const c = "DELETE FROM job_items WHERE id = $1"
const d = `INSERT INTO "job_items" (user_id) VALUES ($1)`
EOF
                expect "an unqualified job_items (INSERT INTO, UPDATE, DELETE FROM, quoted) is flagged as public.job_items (it would resolve through search_path)" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')@@$(outside internal/app/q.go 4 public.job_items 'UPDATE')@@$(outside internal/app/q.go 5 public.job_items 'DELETE FROM')@@$(outside internal/app/q.go 6 public.job_items 'INSERT INTO')@@RED — 4 problem(s)" "$d"
d=$(mk jlower); w "$d" internal/app/q.go '"insert into public.job_items (user_id) values ($1)"'
                expect "public.job_items in lower case" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')" "$d"
d=$(mk jmixed); w "$d" internal/app/q.go '"InSeRt InTo PuBlIc.JoB_iTeMs (user_id) values ($1)"'
                expect "public.job_items in mixed case" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')" "$d"
d=$(mk jraw);   put "$d" internal/app/q.go <<'EOF'
package app

const q = `
	INSERT
	  INTO
		public
		.job_items (campaign_id, user_id)
	VALUES ($1, $2)`
EOF
                expect "public.job_items split over lines in a Go raw string (the keywords, the schema and the table on lines of their own): reported at the INSERT line" 1 "$(outside internal/app/q.go 4 public.job_items 'INSERT INTO')@@!internal/app/q.go:5:@@!internal/app/q.go:6:@@!internal/app/q.go:7:" "$d"
d=$(mk jcrlf);  put "$d" internal/app/q.go <<'EOF'
package app

const q = `INSERT INTO
		public.job_items (user_id)`
EOF
                sed -i 's/$/\r/' "$d/internal/app/q.go"
                expect "the same split statement on public.job_items in a file with CRLF line endings" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')" "$d"
d=$(mk jengine); put "$d" internal/jobengine/zz_job_items_writer.go <<'EOF'
package jobengine

// FAULT: a second writer of the shared job item table, one that writes a row modules 2 and 3 read without the store, the user's scope or the job's lifecycle.
const zzJobItemsWriter = `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'pending')`
EOF
                expect "THE FAULT of the second fault proof: a second writer of public.job_items under internal/jobengine/ is flagged at line 4" 1 "$(outside internal/jobengine/zz_job_items_writer.go 4 public.job_items 'INSERT INTO')@@RED — 1 problem(s)" "$d"
d=$(mk jcmd);   put "$d" cmd/platform/q.go <<'EOF'
package main

import "fmt"

func list() { fmt.Println("DELETE FROM public.job_items WHERE user_id = 'x'") }
EOF
                expect "a file under cmd/ writing public.job_items is scanned too" 1 "$(outside cmd/platform/q.go 5 public.job_items 'DELETE FROM')" "$d"
d=$(mk jmoney); put "$d" internal/money/jobs.go <<'EOF'
package money

const done = `UPDATE public.job_items SET status = 'cancelled' WHERE id = $1::uuid`
EOF
                expect "internal/money may not write public.job_items either (the clean tree's money files only read the jobs)" 1 "$(outside internal/money/jobs.go 3 public.job_items 'UPDATE')" "$d"
d=$(mk jsqlfile); put "$d" db/queries/items.sql <<'EOF'
-- release the items
UPDATE public.job_items SET status = 'cancelled' WHERE user_id = $1;
EOF
                expect "a .sql file under db/ outside db/migrations that writes public.job_items is scanned and flagged" 1 "scanned 5 Go file(s) and 1 SQL file(s) outside db/migrations/@@$(outside db/queries/items.sql 2 public.job_items 'UPDATE')" "$d"
d=$(mk jwhole); put "$d" internal/app/x.go <<'EOF'
package app

const a = "INSERT INTO public.job_items_archive (id) VALUES ($1)"
const b = "INSERT INTO public.job_items2 (id) VALUES ($1)"
const c = `INSERT INTO "public"."job_items_old" (id) VALUES ($1)`
const d = "INSERT INTO mypublic.job_items (id) VALUES ($1)"
const e = "xinsert into public.job_items"
const f = "UPDATE public.job_items_guard SET x = 1"
const g = "TRUNCATE public.job_items_archive"
const h = "DELETE FROM public.job_items_v2 WHERE id = $1"
const i = "INSERT INTO public.jobs_items (id) VALUES ($1)"
const j = "INSERT INTO public.job_item (id) VALUES ($1)"
EOF
                expect "whole word only: job_items_archive, job_items2, *_old, *_guard, *_v2, jobs_items, job_item, a longer schema name and a longer word before the keyword pass" 0 "check-job-writers: OK" "$d"
d=$(mk jschema); put "$d" internal/app/x.go <<'EOF'
package app

const a = "INSERT INTO send_managed.job_items (id) VALUES ($1)"
const b = "UPDATE send_byo.job_items SET status = $1"
const c = "INSERT INTO platform.job_items (id) VALUES ($1)"
const d = `DELETE FROM "send_managed"."job_items"`
const e = "TRUNCATE verify.job_items"
const f = "COPY send_byo.job_items (id) FROM STDIN"
const g = "MERGE INTO platform.job_items j USING x ON true WHEN MATCHED THEN DELETE"
EOF
                expect "another schema's job_items (send_managed, send_byo, verify) and platform.job_items are not public.job_items: not flagged" 0 "check-job-writers: OK" "$d"
d=$(mk jreads); put "$d" internal/app/r.go <<'EOF'
package app

const a = "SELECT campaign_id FROM public.job_items WHERE id = $1 AND user_id = $2"
const b = `SELECT 1 FROM public.job_items FOR UPDATE OF public.job_items`
const c = "SELECT id FROM public.job_items WHERE id = $1 FOR UPDATE"
const e = "COPY (SELECT id FROM public.job_items) TO STDOUT"
const f = "COPY public.job_items TO STDOUT"
const g = "GRANT SELECT ON public.job_items TO platform_admin"
const h = "SELECT ji.id FROM public.job_items ji JOIN platform.job_rows r ON r.id = ji.id WHERE ji.user_id = $1"
EOF
                expect "reads of public.job_items, FOR UPDATE [OF], COPY ... TO, a GRANT and a JOIN anywhere are not writes" 0 "check-job-writers: OK" "$d"
d=$(mk jddl);   put "$d" db/queries/job_items_schema.sql <<'EOF'
GRANT SELECT, INSERT, UPDATE ON public.job_items TO platform_app;
GRANT UPDATE (status) ON public.job_items TO platform_app;
GRANT TRUNCATE ON public.job_items TO platform_admin;
CREATE INDEX job_items_user_idx ON public.job_items (user_id);
ALTER TABLE public.job_items ADD COLUMN note text;
COMMENT ON TABLE public.job_items IS 'The shared item table.';
CREATE TRIGGER t BEFORE UPDATE OR DELETE ON public.job_items FOR EACH ROW EXECUTE FUNCTION f();
ALTER TABLE platform.job_rows ADD CONSTRAINT fk FOREIGN KEY (item_id) REFERENCES public.job_items (id) ON DELETE RESTRICT;
CREATE POLICY p ON public.job_items FOR UPDATE USING (true);
EOF
                expect "DDL on public.job_items (GRANT, CREATE INDEX ... ON, ALTER, COMMENT, a trigger, REFERENCES ... ON DELETE, a policy) in a .sql file under db/ is not a write" 0 "check-job-writers: OK@@and 1 SQL file(s) outside db/migrations/" "$d"
d=$(mk jmixedneg); put "$d" internal/app/m.go <<'EOF'
package app

const a = "SELECT id FROM public.job_items WHERE id = $1 FOR UPDATE"
const b = "INSERT INTO public.job_items_archive (id) VALUES ($1)"
const c = "INSERT INTO send_managed.job_items (id) VALUES ($1)"
const d = "UPDATE public.job_items SET status = $1"
const e = "INSERT INTO public.job_items2 (id) VALUES ($1)"
const f = "INSERT INTO platform.job_items (id) VALUES ($1)"
EOF
                expect "the control of the negatives: among reads, longer names and other schemas, exactly the one real write (line 6) is reported" 1 "$(outside internal/app/m.go 6 public.job_items 'UPDATE')@@!internal/app/m.go:3:@@!internal/app/m.go:4:@@!internal/app/m.go:5:@@!internal/app/m.go:7:@@!internal/app/m.go:8:@@RED — 1 problem(s): 1 write(s) outside $SAN/" "$d"
d=$(mk jtwice); w "$d" internal/app/q.go '"INSERT INTO public.job_items (user_id) VALUES ($1); DELETE FROM platform.job_rows WHERE id = $2; UPDATE public.job_items SET status = $3"'
                expect "three writes in a row on one line, on two tables of two schemas, are reported with their own table" 1 "$(outside internal/app/q.go 3 public.job_items 'INSERT INTO')@@$(outside internal/app/q.go 3 platform.job_rows 'DELETE FROM')@@$(outside internal/app/q.go 3 public.job_items 'UPDATE')@@RED — 3 problem(s): 3 write(s) outside $SAN/" "$d"
d=$(mk jcomment); put "$d" internal/app/q.go <<'EOF'
package app

// the old code ran UPDATE public.job_items SET status here, before the store existed
const q = 1
EOF
                expect "a comment containing a write on public.job_items IS flagged (comments are not stripped)" 1 "$(outside internal/app/q.go 3 public.job_items 'UPDATE')" "$d"
d=$(mk jprose); put "$d" internal/adapters/sendapi/client.go <<'EOF'
package sendapi

// Run hands the module one job item (POST /v1/run). The module reads the recipient and the rendered message from the platform's own public.job_items row, which must have been COMMITTED before this call.
// Module 3 builds multipart/alternative when the platform has written `public.job_items.rendered_text`; the row is read for the tenant, never written from here.
func Run() {}
EOF
                expect "prose that merely names public.job_items (the adapters' comments) is not a write" 0 "check-job-writers: OK@@scanned 6 Go file(s)" "$d"
d=$(mk jtestfile); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "INSERT INTO public.job_items (campaign_id, user_id) VALUES ($1, $2)"
EOF
                expect "a _test.go file that seeds public.job_items is ignored, and the report says tests were not checked" 0 "check-job-writers: OK@@scanned 5 Go file(s)@@$NC_HEAD" "$d"
d=$(mk jmigs);  put "$d" db/migrations/platform_0098_items.up.sql <<'EOF'
ALTER TABLE public.job_items ADD COLUMN note text;
UPDATE public.job_items SET note = '';
INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) SELECT 1;
TRUNCATE public.job_items;
EOF
                expect "a migration that writes public.job_items is DDL by the migrator: it is not scanned, and the report counts it" 0 "check-job-writers: OK@@2 migration file(s) are not scanned" "$d"
d=$(mk jstore); put "$d" internal/jobs/items.go <<'EOF'
package jobs

const insertItem = `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'pending') RETURNING id`
const updateItem = `UPDATE public.job_items SET rendered_text = $3 WHERE id = $1::uuid AND user_id = $2::uuid`
const readItem = `SELECT campaign_id FROM public.job_items WHERE id = $1::uuid AND user_id = $2::uuid FOR UPDATE`

var copyTarget = pgx.Identifier{"public", "job_items"}
EOF
                expect "the store may write public.job_items: green, and the count of its write statements includes them (the 5 of the clean store, an INSERT, an UPDATE and a CopyFrom identifier; the SELECT FOR UPDATE is a read)" 0 "check-job-writers: OK@@scanned 6 Go file(s) and 0 SQL file(s) outside db/migrations/; the sanctioned writer $SAN/ holds 3 of the Go files; 1 migration file(s) are not scanned@@sanctioned writer $SAN/: 8 write statement(s) in 3 Go file(s)@@0 write(s) outside the sanctioned writer, 0 write(s) excused by exception, 0 sanctioned-writer problem(s), 0 exception problem(s)@@!outside the sanctioned writer internal" "$d"
d=$(mk jonly);  rm -rf "$d/internal/jobs"; put "$d" internal/jobs/items.go <<'EOF'
package jobs

const insertItem = `INSERT INTO public.job_items (campaign_id, user_id) VALUES ($1::uuid, $2::uuid)`
EOF
                expect "a store whose only write is on public.job_items is a writer: the floor counts it" 0 "check-job-writers: OK@@sanctioned writer $SAN/: 1 write statement(s) in 1 Go file(s)" "$d"
d=$(mk jnowrite); rm -rf "$d/internal/jobs"; put "$d" internal/jobs/items.go <<'EOF'
package jobs

const get = `SELECT campaign_id FROM public.job_items WHERE id = $1::uuid FOR UPDATE`
const arch = `INSERT INTO public.job_items_archive (id) VALUES ($1::uuid)`
EOF
                expect "the control of the previous case: a store that only reads public.job_items (or writes a longer-named table) holds no write, and the instrument matched nothing" 1 "$SAN/:1: the sanctioned writer holds no write statement on a job table: the instrument matched nothing@@sanctioned writer $SAN/: 0 write statement(s) in 1 Go file(s)@@RED — 1 problem(s): 0 write(s) outside $SAN/, 1 sanctioned-writer problem(s)" "$d"
d=$(mk jexc);   w "$d" internal/app/q.go '"INSERT INTO public.job_items (user_id) VALUES ($1)"'; printf '# register\ninternal/app/q.go|the one-off item backfill: it cannot call the store because it runs before the store exists\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception makes a file that writes public.job_items pass, and the report shows it and its reason" 0 "check-job-writers: OK@@excused: internal/app/q.go (1 write(s), exception on line 2): the one-off item backfill: it cannot call the store because it runs before the store exists@@1 write(s) excused by exception@@!outside the sanctioned writer internal" "$d"
d=$(mk jexcpart); w "$d" internal/app/q.go '"INSERT INTO platform.job_rows (id) VALUES ($1)"'; put "$d" internal/app/other.go <<'EOF'
package app

const x = "DELETE FROM public.job_items WHERE id = $1"
EOF
                printf 'internal/app/q.go|excused\n' > "$d/tests/job-writer-exceptions.txt"
                expect "an exception covers its own file only: another file writing public.job_items is still red" 1 "$(outside internal/app/other.go 3 public.job_items 'DELETE FROM')@@!internal/app/q.go:3:@@excused: internal/app/q.go (1 write(s)" "$d"
d=$(mk jexcstale); put "$d" internal/app/q.go <<'EOF'
package app

const read = "SELECT campaign_id FROM public.job_items WHERE id = $1"
const arch = "INSERT INTO public.job_items_archive (id) VALUES ($1)"
EOF
                printf 'internal/app/q.go|used to fill the job items\n' > "$d/tests/job-writer-exceptions.txt"
                expect "a STALE exception: the file only reads public.job_items and writes a longer-named table, so it has no write to excuse" 1 "tests/job-writer-exceptions.txt:1: stale exception for internal/app/q.go: the file contains no write to a job table@@RED — 1 problem(s): 0 write(s) outside $SAN/, 0 sanctioned-writer problem(s), 1 exception problem(s)" "$d"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Four deliberately WRONG arrangements, each run through the same `expect`. EVERY one must be reported as a mismatch; if any is accepted, this selftest cannot fail and says so.
#   1. a gate that always exits 0, run on a violating tree that must be red                      (the selftest must go red if the gate were `exit 0`)
#   2. a gate that fails WITHOUT naming a reason, run on a violating tree that must be red for the reason   (class 10: the right exit code for no reason)
#   3. the real gate on a clean tree, expected to be red
#   4. the real gate on a violating tree, expected to be green
printf '#!/bin/sh\nexit 0\n' > "$T/stub-pass.sh"; printf '#!/bin/sh\necho boom\nexit 1\n' > "$T/stub-bare-red.sh"
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases; real="$GATE"
d=$(mk ctlv); w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'
GATE="$T/stub-pass.sh";     expect "CONTROL 1 (wrong on purpose): a gate that is always green" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$T/stub-bare-red.sh"; expect "CONTROL 2 (wrong on purpose): a gate that is red for no named reason" 1 "$(outside internal/app/q.go 3 platform.jobs 'INSERT INTO')" "$d" >/dev/null 2>&1
GATE="$real"
d=$(mk ctlc); expect "CONTROL 3 (wrong on purpose): a clean tree expected to be RED" 1 "write to platform." "$d" >/dev/null 2>&1
d=$(mk ctlg); w "$d" internal/app/q.go '"INSERT INTO platform.jobs (user_id) VALUES ($1)"'; expect "CONTROL 4 (wrong on purpose): a violating tree expected to be GREEN" 0 "check-job-writers: OK" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 4 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] all four deliberately wrong arrangements were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 4 wrong arrangements were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
