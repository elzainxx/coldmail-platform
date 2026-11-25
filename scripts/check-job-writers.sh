#!/usr/bin/env bash
# check-job-writers.sh [--root DIR] [--exceptions FILE] — no code may write the job tables except the job store (internal/jobs).
#
# WHY IT EXISTS. Every rule about a job is enforced where every writer passes (Docs/phase-4-spec.md, Locked 28): the lifecycle by the database's guard, the rest by the store. The store's
# functions take the acting user from the authenticated principal, scope every statement by it, and are the ONLY code that moves a job, writes its rows, its results and its events. A second
# writer (the API, the engine, the admin console of Phase 7, a script) would be a parallel write path that none of those rules pass through. This gate fails when anything outside internal/jobs/
# writes platform.jobs, platform.job_rows, platform.job_results, platform.job_events, platform.job_transitions or public.job_items, or moves a job through the function platform.job_transition().
# public.job_items is the one policed table of SCHEMA PUBLIC (never platform): the shared table modules 2 and 3 read (CLAUDE.md rule 15), so the same single writer, the job store, fills it for every job.
#
# WHAT IT SCANS (printed on every run): every non-test *.go (not *_test.go) under <root>/internal and <root>/cmd, and every *.sql under <root>/db EXCEPT <root>/db/migrations/. Each file is
# normalised to one line first (CR, tab and newline become spaces, byte for byte), so a Go raw string that splits its SQL across lines is read as one statement and the line of the FIRST keyword is
# recovered from the byte offset.
# WHAT IT FLAGS: a write statement naming one of the six tables, case-insensitively, as a whole word, with the schema optional (an unqualified name would resolve through search_path) and every
# identifier optionally quoted, or quoted and escaped inside a Go string ("platform"."jobs", "public"."job_items", \"platform\".\"jobs\"): INSERT INTO, UPDATE [ONLY], DELETE FROM [ONLY], TRUNCATE [TABLE]
# (also in a list of tables), COPY <table> [(columns)] FROM, MERGE INTO [ONLY], a pgx.Identifier{"platform", "<table>"} or pgx.Identifier{"public", "job_items"} (what CopyFrom writes through), and a CALL
# of platform.job_transition( (the one function that updates platform.jobs.status for its caller: granted to platform_app, so a caller outside the store could move a job through it). The schema, when
# named, must be the table's own: platform for the five job tables, public for job_items. platform.jobs_archive, platform.job_rows2, public.job_items_archive, public.job_items2, another schema's
# tables (verify.jobs, send_managed.jobs, send_managed.job_items, platform.job_items), a longer word before the keyword, COPY ... TO (a read), SELECT, FOR UPDATE [OF], GRANT, ALTER, COMMENT, CREATE INDEX
# ... ON and REFERENCES are not matched. The finding names the table as platform.<name>, or public.job_items.
# THE SANCTIONED WRITER: every non-test file under internal/jobs/. The package must exist, must hold at least one non-test Go file and at least one write, on any of the six tables ("the instrument
# matched nothing" is red: a store that never writes, or a matcher that no longer reads it). internal/money only SELECTs platform.jobs (FOR UPDATE is a read lock, not a write): it is flagged by nothing here.
# EXCEPTIONS: tests/job-writer-exceptions.txt (or --exceptions FILE), lines `relative/path|reason` (`#` comments and blank lines allowed), for a scanned file OUTSIDE internal/jobs/ that must write. An
# exception is RED when it is STALE (the file does not exist, is not a scanned file, lies inside internal/jobs/ and needs none, or contains no write: a register that rots hides the next miss, false-green
# class 17), when its reason is blank, when its line is malformed, and when the same file is listed twice. The register starts with comments only: nothing outside the store writes a job table.
# FLOOR (a gate whose subject is empty is red, classes 8 and 18): at least one Go file must have been scanned, the sanctioned package must exist and hold a write, and the matcher must pass its own
# canary (known positives and negatives, including two matches in a row) before it is trusted on the real tree.
#
# Output: `check-job-writers: scanned ...` lines, one line per finding `path:line: reason`, a `NOT CHECKED:` line, and `check-job-writers: OK` or `RED`. Exit status: 0 clean, 1 red, 2 usage error.
# Paired control: scripts/check-job-writers-selftest.sh. Fault proofs: ci/faults/p4b/job-writer (platform.jobs) and ci/faults/p4b/job-items-writer (public.job_items).
#
# NOT CHECKED (also printed on every run): test files and everything under tests/; shell scripts and Docs; db/migrations/*.sql (they create the tables, seed platform.job_transitions and define the
# lifecycle's function: DDL by the privileged migrator, not a path of the application); SQL built by string concatenation, fmt.Sprintf or a variable around the table name; a write through a view, a
# rule or another function than platform.job_transition (a trigger writes platform.job_events by design); a SQL comment between the keywords; what the modules themselves write to public.job_items
# (module 3's test-send inserts a row from its own code: only the platform's source is read here); whether the store scopes a statement by the user or
# sets the three lifecycle settings (the harness tests prove that against a real database); the database's own walls (the grants and the guard): this gate reads source, not the database.
# COMMENTS ARE NOT STRIPPED: a comment that contains a write statement IS flagged (the gate fails closed; reword the comment).
set -u
export LC_ALL=C

usage() { echo "usage: check-job-writers.sh [--root DIR] [--exceptions FILE]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""; EXC_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)         [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)       ROOT_IN="${1#*=}"; shift;;
    --exceptions)   [ $# -ge 2 ] || usage; EXC_IN="$2"; shift 2;;
    --exceptions=*) EXC_IN="${1#*=}"; shift;;
    -h|--help)      usage;;
    *)              echo "check-job-writers: unknown argument '$1'" >&2; usage;;
  esac
done
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

SAN_DIR="internal/jobs"
SCOPE="non-test *.go under internal/ and cmd/, and *.sql under db/ except db/migrations/"
NOT_CHECKED="NOT CHECKED: test files (*_test.go) and everything under tests/; shell scripts and Docs; db/migrations/*.sql (DDL by the privileged migrator: they create the tables and seed platform.job_transitions); SQL built by string concatenation, fmt.Sprintf or a variable around the table name; a write through a view, a rule or another function than platform.job_transition; a SQL comment between the keywords; what the modules themselves write to public.job_items (module 3's test-send inserts a row from its own code: only the platform's source is read); whether the store scopes a statement by the user or sets the lifecycle settings (the harness tests prove that); the database's own walls (the grants and the guard). Comments are NOT stripped: a comment containing a write statement IS flagged"

# ---- the matcher ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# The ERE has no word boundaries (ERE has no lookaround, and a consumed boundary character would hide a second match that follows closely); they are checked on the normalised text afterwards.
# (\\?")? is an optional double quote, which inside a Go interpreted string may be escaped. The schema is optional: an unqualified name resolves through search_path, and CLAUDE.md rule 12 says every
# statement names its schema, so the bare name is flagged too (another schema's table, verify.jobs, is not: the match must start at the keyword and the schema, if any, must be the table's own).
# TBL is ONE group of two alternatives, so every statement form below picks both: the five tables of schema platform (NAMES), and public.job_items (the shared table of schema public; its schema is
# public, never platform, and the two alternatives are not crossed: platform.job_items and public.jobs match neither).
Q='(\\?")?'
S='[[:space:]]+'
OPT_ONLY='(only[[:space:]]+)?'
NAMES='(job_transitions|job_results|job_events|job_rows|jobs)'
PUB_NAMES='(job_items)'
TBL_PLAT='('"$Q"'platform'"$Q"'[[:space:]]*\.[[:space:]]*)?'"$Q""$NAMES""$Q"
TBL_PUB='('"$Q"'public'"$Q"'[[:space:]]*\.[[:space:]]*)?'"$Q""$PUB_NAMES""$Q"
TBL='('"$TBL_PLAT"'|'"$TBL_PUB"')'
LIST_ITEM='[A-Za-z0-9_."\]+'
F_INSERT="insert${S}into${S}${TBL}"
F_UPDATE="update${S}${OPT_ONLY}${TBL}"
F_DELETE="delete${S}from${S}${OPT_ONLY}${TBL}"
F_TRUNCATE="truncate${S}(table${S})?(${LIST_ITEM}[[:space:]]*,[[:space:]]*)*${OPT_ONLY}${TBL}"
F_COPY="copy${S}${TBL}[[:space:]]*(\([^)]*\))?[[:space:]]*from"
F_MERGE="merge${S}into${S}${OPT_ONLY}${TBL}"
F_IDENT='identifier[[:space:]]*\{[[:space:]]*("platform"[[:space:]]*,[[:space:]]*"'"$NAMES"'"|"public"[[:space:]]*,[[:space:]]*"'"$PUB_NAMES"'")[[:space:]]*\}'
F_FUNC='('"$Q"'platform'"$Q"'[[:space:]]*\.[[:space:]]*)?'"$Q"'job_transition'"$Q"'[[:space:]]*\('
ERE="${F_INSERT}|${F_UPDATE}|${F_DELETE}|${F_TRUNCATE}|${F_COPY}|${F_MERGE}|${F_IDENT}|${F_FUNC}"

# normalise <in> <out>: CR, tab and newline become spaces, byte for byte, so an offset in <out> is an offset in <in>
normalise() { tr '\r\t\n' '   ' < "$1" > "$2"; }
# kind_of <matched text>: what the write is, from its first word (sets KIND_R; no command substitution, so no process is forked per match)
KIND_R=""; TARGET_R=""
kind_of() {
  local t="${1,,}"
  case "$t" in
    insert*) KIND_R="INSERT INTO";; update*) KIND_R="UPDATE";; delete*) KIND_R="DELETE FROM";; truncate*) KIND_R="TRUNCATE";;
    copy*) KIND_R="COPY ... FROM";; merge*) KIND_R="MERGE INTO";; identifier*) KIND_R="pgx.Identifier, as CopyFrom writes through it";;
    *) KIND_R="a call of platform.job_transition()";;
  esac
}
# target_of <matched text>: the table (or function) the match flags (sets TARGET_R): the LAST whole-word table name in the text. A TRUNCATE list names the flagged table last (the tables before it are
# other tables, perhaps another schema's jobs or job_items), every other form names one table, and a COPY column list is cut off first (a column may be called anything). A longer name that merely
# contains a table name (job_items_archive, jobs_old) is not that table; job_items is public.job_items and the five others are platform.<name>.
target_of() {
  local t="${1,,}"
  case "$t" in *'(') TARGET_R="platform.job_transition()"; return;; esac   # only a call of the function matches up to an opening parenthesis
  case "$t" in copy*) t="${t%%(*}";; esac
  if [[ "$t" =~ ^(.*[^a-z0-9_])?(job_transitions|job_results|job_events|job_rows|jobs|job_items)([^a-z0-9_].*)?$ ]]; then
    case "${BASH_REMATCH[2]}" in job_items) TARGET_R="public.job_items";; *) TARGET_R="platform.${BASH_REMATCH[2]}";; esac
  else TARGET_R="a job table"; fi
}
# offsets <normalised file> <out file>: writes one `offset|kind|target` line for every match whose edges are word boundaries; returns non-zero when grep ITSELF failed (a crashed grep prints no match,
# which would read as a clean file: grep exits 0 when it matched, 1 when it did not, and anything else is a failure)
offsets() {
  local nf="$1" out="$2" S_ rec off text prev next rc
  : > "$out"
  grep -a -o -b -i -E "$ERE" "$nf" > "$T/g.out"; rc=$?
  [ "$rc" -le 1 ] || return "$rc"
  S_="$(<"$nf")"
  while IFS= read -r rec; do
    off="${rec%%:*}"; text="${rec#*:}"
    if [ "$off" -gt 0 ]; then prev="${S_:$((off-1)):1}"; case "$prev" in [A-Za-z0-9_]) continue;; esac; fi
    next="${S_:$((off+${#text})):1}"; case "$next" in [A-Za-z0-9_]) continue;; esac
    kind_of "$text"; target_of "$text"
    printf '%s|%s|%s\n' "$off" "$KIND_R" "$TARGET_R" >> "$out"
  done < "$T/g.out"
  return 0
}
# canary <want> <text>: the matcher must find exactly <want> matches in <text>. All the canaries go through ONE pass of the real matcher (each is followed by a separator that no match can span),
# so a run costs one grep, and each match is credited to the canary whose byte range holds its offset.
CANARY_WANT=(); CANARY_TEXT=()
canary() { CANARY_WANT+=("$1"); CANARY_TEXT+=("$2"); }
run_canaries() {
  local i pos=0 off got rc bad=0 rec; local -a start=() count=()
  : > "$T/c.in"
  for i in "${!CANARY_TEXT[@]}"; do
    start[i]=$pos; count[i]=0
    printf '%s\n@@@\n' "${CANARY_TEXT[i]}" >> "$T/c.in"; pos=$((pos + ${#CANARY_TEXT[i]} + 5))
  done
  normalise "$T/c.in" "$T/c.n"
  offsets "$T/c.n" "$T/c.off"; rc=$?
  if [ "$rc" -ne 0 ]; then echo "  canary: grep itself failed (exit status $rc)"; return 99; fi
  while IFS= read -r rec; do
    off="${rec%%|*}"
    [ -n "$off" ] || continue
    for i in "${!start[@]}"; do
      if [ "$off" -ge "${start[i]}" ] && { [ -z "${start[i+1]:-}" ] || [ "$off" -lt "${start[i+1]}" ]; }; then count[i]=$((count[i]+1)); break; fi
    done
  done < "$T/c.off"
  for i in "${!CANARY_TEXT[@]}"; do
    got="${count[i]}"
    if [ "$got" != "${CANARY_WANT[i]}" ]; then echo "  canary: wanted ${CANARY_WANT[i]} match(es), found $got, in: ${CANARY_TEXT[i]}"; bad=$((bad+1)); fi
  done
  return "$bad"
}
# the positives: every write form on every table, every quoting, the bare name and the function
canary 1 'INSERT INTO platform.jobs (user_id, module_id) VALUES ($1, $2)'
canary 1 'insert into platform.job_rows'
canary 1 'INSERT INTO platform.job_results (job_id) SELECT 1'
canary 1 'INSERT INTO platform.job_events (job_id, kind) VALUES ($1, $2)'
canary 1 'INSERT INTO platform.job_transitions (from_status, to_status) VALUES ($1, $2)'
canary 1 "$(printf 'INSERT\n\tINTO\n\t\tplatform.job_rows (id)')"
canary 1 'INSERT INTO "platform"."jobs" (user_id)'
canary 1 'q := "INSERT INTO \"platform\".\"job_rows\" (id)"'
canary 1 'INSERT INTO platform . job_events (job_id)'
canary 1 'INSERT INTO jobs (user_id)'
canary 1 'INSERT INTO job_results (job_id)'
canary 1 'WITH x AS (INSERT INTO platform.job_events (job_id) VALUES ($1) RETURNING id) SELECT 1'
canary 1 'UPDATE platform.jobs SET status = $1'
canary 1 'UPDATE ONLY platform.job_rows SET state = $1'
canary 1 'UPDATE platform.job_results SET rule = $1'
canary 1 'UPDATE platform.job_events SET reason = $1'
canary 1 'DELETE FROM platform.jobs WHERE id = $1'
canary 1 'DELETE FROM ONLY platform.job_rows'
canary 1 'DELETE FROM platform.job_results'
canary 1 'TRUNCATE platform.jobs'
canary 1 'TRUNCATE TABLE platform.job_events'
canary 1 'TRUNCATE TABLE ONLY platform.job_rows CASCADE'
canary 1 'TRUNCATE platform.ledger, platform.job_results'
canary 1 'TRUNCATE platform.job_transitions, platform.ledger'
canary 1 'COPY platform.job_rows (id, job_id) FROM STDIN'
canary 1 'COPY platform.job_results FROM STDIN'
canary 1 'MERGE INTO platform.jobs j USING x ON true WHEN NOT MATCHED THEN INSERT VALUES (1)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"platform", "job_rows"}, cols, src)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"platform","job_results"}, cols, src)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{ "platform" , "job_events" }, cols, src)'
canary 1 'SELECT platform.job_transition($1::uuid, $2, $3, $4::uuid, $5)'
canary 1 'SELECT "platform"."job_transition"($1)'
canary 1 'SELECT job_transition ($1)'
canary 2 'INSERT INTO platform.jobs (user_id) VALUES ($1); DELETE FROM platform.job_rows'
canary 2 'UPDATE platform.jobs SET status = $1; SELECT platform.job_transition($1)'
# the positives on public.job_items (schema public): every write form, every quoting, the bare name
canary 1 'INSERT INTO public.job_items (campaign_id, user_id) VALUES ($1, $2)'
canary 1 'insert into public.job_items'
canary 1 "$(printf 'INSERT\n\tINTO\n\t\tpublic.job_items (id)')"
canary 1 'INSERT INTO "public"."job_items" (user_id)'
canary 1 'INSERT INTO public."job_items" (user_id)'
canary 1 'INSERT INTO "public".job_items (user_id)'
canary 1 'q := "INSERT INTO \"public\".\"job_items\" (user_id)"'
canary 1 'INSERT INTO public . job_items (user_id)'
canary 1 'INSERT INTO job_items (user_id)'
canary 1 'INSERT INTO "job_items" (user_id)'
canary 1 'WITH x AS (INSERT INTO public.job_items (user_id) VALUES ($1) RETURNING id) SELECT 1'
canary 1 'UPDATE public.job_items SET status = $1'
canary 1 'UPDATE ONLY public.job_items SET status = $1'
canary 1 'UPDATE job_items SET status = $1'
canary 1 'DELETE FROM public.job_items WHERE id = $1'
canary 1 'DELETE FROM ONLY public.job_items'
canary 1 'TRUNCATE public.job_items'
canary 1 'TRUNCATE TABLE public.job_items'
canary 1 'TRUNCATE TABLE ONLY public.job_items CASCADE'
canary 1 'TRUNCATE platform.ledger, public.job_items'
canary 1 'TRUNCATE public.job_items, platform.ledger'
canary 1 'TRUNCATE public.users, "public"."job_items" RESTART IDENTITY'
canary 1 'COPY public.job_items (campaign_id, user_id) FROM STDIN'
canary 1 'COPY public.job_items FROM STDIN'
canary 1 'MERGE INTO public.job_items j USING x ON true WHEN NOT MATCHED THEN INSERT VALUES (1)'
canary 1 'MERGE INTO ONLY public.job_items j USING x ON true WHEN MATCHED THEN DELETE'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"public", "job_items"}, cols, src)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"public","job_items"}, cols, src)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{ "public" , "job_items" }, cols, src)'
canary 2 'INSERT INTO public.job_items (user_id) VALUES ($1); DELETE FROM public.job_items'
canary 2 'INSERT INTO platform.jobs (user_id) VALUES ($1); UPDATE public.job_items SET status = $1'
canary 2 'UPDATE public.job_items SET status = $1; INSERT INTO platform.job_events (job_id) VALUES ($1)'
# the negatives: reads, locks, grants, DDL, a longer name, another schema, a longer word before the keyword, a trigger that merely names the table
canary 0 'SELECT status FROM platform.jobs WHERE id = $1 FOR UPDATE'
canary 0 'SELECT 1 FROM platform.jobs j WHERE j.id = $1 FOR UPDATE OF j'
canary 0 'SELECT 1 FROM platform.jobs FOR UPDATE OF platform.jobs'
canary 0 'SELECT 1 FROM platform.job_rows FOR UPDATE'
canary 0 'INSERT INTO platform.jobs_archive (id) VALUES ($1)'
canary 0 'INSERT INTO platform.job_rows2 (id) VALUES ($1)'
canary 0 'INSERT INTO "platform"."job_results_old" (id)'
canary 0 'INSERT INTO platform.job_events_check_actor (id)'
canary 0 'INSERT INTO verify.jobs (id)'
canary 0 'INSERT INTO send_managed.jobs (id)'
canary 0 'UPDATE verify.jobs SET status = $1'
canary 0 'INSERT INTO myplatform.jobs (id)'
canary 0 'xinsert into platform.jobs'
canary 0 'UPDATE platform.jobs_guard SET x = 1'
canary 0 'TRUNCATE platform.jobs_archive'
canary 0 'COPY platform.job_rows TO STDOUT'
canary 0 'COPY (SELECT id FROM platform.job_rows) TO STDOUT'
canary 0 'GRANT SELECT, INSERT, UPDATE ON platform.jobs TO platform_app'
canary 0 'GRANT UPDATE (state) ON platform.job_rows TO platform_app'
canary 0 'ALTER TABLE platform.jobs ADD CONSTRAINT c CHECK (true)'
canary 0 'COMMENT ON TABLE platform.job_events IS $$append only$$'
canary 0 'CREATE TRIGGER t BEFORE UPDATE OR DELETE ON platform.job_events FOR EACH ROW EXECUTE FUNCTION f()'
canary 0 'CREATE TRIGGER jobs_record_transition AFTER UPDATE OF status ON platform.jobs FOR EACH ROW EXECUTE FUNCTION f()'
canary 0 'FOREIGN KEY (job_id) REFERENCES platform.jobs (id) ON DELETE RESTRICT'
canary 0 'ON CONFLICT (job_id) DO UPDATE SET reason = 1'
canary 0 'DELETE FROM platform.ledger'
canary 0 'UPDATE platform.users SET is_active = false'
canary 0 'INSERT INTO platform.balances (user_id) VALUES ($1)'
canary 0 'SELECT platform.job_transitions FROM x'
canary 0 'SELECT * FROM platform.job_transitions WHERE from_status = $1'
canary 0 'pgx.Identifier{"platform", "job_rows_archive"}'
canary 0 'pgx.Identifier{"platform", "ledger"}'
canary 0 'SELECT my_job_transition($1)'
canary 0 'SELECT platform.job_transition_log($1)'
# the negatives on public.job_items: reads, locks, grants, DDL, a longer name, another schema (and the crossed pairs platform.job_items, public.jobs), a longer word before the keyword
canary 0 'SELECT campaign_id FROM public.job_items WHERE id = $1 AND user_id = $2 FOR UPDATE'
canary 0 'SELECT 1 FROM public.job_items j WHERE j.id = $1 FOR UPDATE OF j'
canary 0 'SELECT 1 FROM public.job_items FOR UPDATE OF public.job_items'
canary 0 'INSERT INTO public.job_items_archive (id) VALUES ($1)'
canary 0 'INSERT INTO public.job_items2 (id) VALUES ($1)'
canary 0 'INSERT INTO "public"."job_items_old" (id)'
canary 0 'INSERT INTO send_managed.job_items (id)'
canary 0 'INSERT INTO send_byo.job_items (id)'
canary 0 'INSERT INTO platform.job_items (id)'
canary 0 'UPDATE send_managed.job_items SET status = $1'
canary 0 'UPDATE platform.job_items SET status = $1'
canary 0 'DELETE FROM "send_managed"."job_items"'
canary 0 'INSERT INTO mypublic.job_items (id)'
canary 0 'xinsert into public.job_items'
canary 0 'UPDATE public.job_items_guard SET x = 1'
canary 0 'TRUNCATE public.job_items_archive'
canary 0 'INSERT INTO public.jobs_items (id)'
canary 0 'INSERT INTO public.job_item (id)'
canary 0 'COPY public.job_items TO STDOUT'
canary 0 'COPY (SELECT id FROM public.job_items) TO STDOUT'
canary 0 'COPY send_byo.job_items (id) FROM STDIN'
canary 0 'GRANT SELECT, INSERT, UPDATE ON public.job_items TO platform_app'
canary 0 'GRANT UPDATE (status) ON public.job_items TO platform_app'
canary 0 'GRANT TRUNCATE ON public.job_items TO platform_admin'
canary 0 'CREATE INDEX job_items_user_idx ON public.job_items (user_id)'
canary 0 'ALTER TABLE public.job_items ADD COLUMN note text'
canary 0 'COMMENT ON TABLE public.job_items IS $$the shared table$$'
canary 0 'FOREIGN KEY (item_id) REFERENCES public.job_items (id) ON DELETE RESTRICT'
canary 0 'pgx.Identifier{"public", "job_items_archive"}'
canary 0 'pgx.Identifier{"public", "users"}'
canary 0 'pgx.Identifier{"send_managed", "job_items"}'
canary 0 'pgx.Identifier{"platform", "job_items"}'
canary 0 'DELETE FROM public.users'
run_canaries; canary_bad=$?
if [ "$canary_bad" -gt 0 ]; then
  echo "check-job-writers: RED — the matcher failed its own canary ($canary_bad case(s) above): the instrument cannot be trusted (is grep -o -b -E available?)"
  exit 1
fi

# ---- the subject ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-job-writers: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }
EXC="${EXC_IN:-$ROOT/tests/job-writer-exceptions.txt}"
EXCNAME="${EXC#"$ROOT"/}"

: > "$T/go.abs"; : > "$T/sql.abs"
for d in internal cmd; do
  [ -d "$ROOT/$d" ] || continue
  find "$ROOT/$d" -type f -name '*.go' ! -name '*_test.go' >> "$T/go.abs"
done
if [ -d "$ROOT/db" ]; then find "$ROOT/db" -path "$ROOT/db/migrations" -prune -o -type f -name '*.sql' -print >> "$T/sql.abs"; fi
: > "$T/files"
declare -A IN_SCOPE
for kind in go sql; do
  sort "$T/$kind.abs" | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/$kind.rel"
  cat "$T/$kind.rel" >> "$T/files"
done
while IFS= read -r rel; do [ -n "$rel" ] && IN_SCOPE[$rel]=1; done < "$T/files"
ngo=$(wc -l < "$T/go.rel" | tr -d ' '); nsql=$(wc -l < "$T/sql.rel" | tr -d ' ')
nmigf=0; if [ -d "$ROOT/db/migrations" ]; then nmigf=$(find "$ROOT/db/migrations" -type f -name '*.sql' | wc -l | tr -d ' '); fi
nsan_files=$(grep -c "^$SAN_DIR/" "$T/go.rel" || true)

echo "check-job-writers: scope: $SCOPE (root: $ROOT)"
echo "check-job-writers: scanned $ngo Go file(s) and $nsql SQL file(s) outside db/migrations/; the sanctioned writer $SAN_DIR/ holds $nsan_files of the Go files; $nmigf migration file(s) are not scanned"
if [ "$ngo" -lt 1 ]; then
  echo "check-job-writers: RED — found 0 Go files under internal/ and cmd/ in $ROOT: the instrument matched nothing (a wrong --root, or an empty checkout)"
  exit 1
fi
if [ ! -f "$EXC" ]; then
  echo "check-job-writers: RED — the exceptions file $EXC does not exist (the gate cannot say what it excuses; an empty register is a file with only comments)"
  exit 1
fi

# ---- the hits: HITS[file] = lines "offset|line|kind|target", in file order -----------------------------------------------------------------------------------------------------------
declare -A HITS COUNT EXC_REASON EXC_LINE
nbad_files=0
while IFS= read -r rel; do
  f="$ROOT/$rel"
  # every match contains one of these words: a file without any of them (in any case) cannot hold one, and is skipped without spawning a process (checked in bash itself, so no external tool can fail silently here)
  content="$(<"$f")"; case "${content,,}" in *job_transition*|*jobs*|*job_rows*|*job_results*|*job_events*|*job_items*) ;; *) continue;; esac
  normalise "$f" "$T/n"
  offsets "$T/n" "$T/off"; rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-job-writers: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue
  fi
  while IFS='|' read -r off kind target; do
    [ -n "$off" ] || continue
    ln=$(( $(head -c "$off" "$f" | wc -l) + 1 ))
    HITS[$rel]="${HITS[$rel]:-}$off|$ln|$kind|$target"$'\n'; COUNT[$rel]=$(( ${COUNT[$rel]:-0} + 1 ))
  done < "$T/off"
done < "$T/files"

fail=$nbad_files; nout=0; nsanbad=0; nexc_bad=0; nexcused=0; san_writes=0
problem() { echo "$1"; fail=$((fail+1)); }
exc_bad() { echo "$EXCNAME:$1: $2"; fail=$((fail+1)); nexc_bad=$((nexc_bad+1)); }

# ---- the sanctioned writer ------------------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  case "$rel" in "$SAN_DIR"/*) san_writes=$((san_writes + ${COUNT[$rel]:-0}));; esac
done < "$T/go.rel"
if [ ! -d "$ROOT/$SAN_DIR" ] || [ "$nsan_files" -lt 1 ]; then
  problem "$SAN_DIR/:1: the sanctioned writer does not exist (no non-test Go file under $SAN_DIR/): there is no place the platform may write the job tables from (the instrument has no subject; restore the package or move the gate's sanctioned path deliberately)"; nsanbad=$((nsanbad+1))
elif [ "$san_writes" -lt 1 ]; then
  problem "$SAN_DIR/:1: the sanctioned writer holds no write statement on a job table: the instrument matched nothing (a store that never writes, or a matcher that no longer reads it)"; nsanbad=$((nsanbad+1))
fi
echo "check-job-writers: sanctioned writer $SAN_DIR/: $san_writes write statement(s) in $nsan_files Go file(s)"

# ---- the migration-free register of exceptions ------------------------------------------------------------------------------------------------------------------------------------------
trim() { printf '%s' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'; }
exc_order=(); n=0
while IFS= read -r raw || [ -n "$raw" ]; do
  n=$((n+1)); line="$(printf '%s' "$raw" | tr -d '\r')"
  [ -n "$(trim "$line")" ] || continue
  case "$(trim "$line")" in '#'*) continue;; esac
  case "$line" in
    *'|'*) ;;
    *) exc_bad "$n" "malformed exception line (want 'relative/path|reason'): $line"; continue;;
  esac
  path="$(trim "${line%%|*}")"; reason="$(trim "${line#*|}")"
  if [ -z "$path" ]; then exc_bad "$n" "malformed exception line (the path is empty; want 'relative/path|reason'): $line"; continue; fi
  case "$path" in /*|*..*) exc_bad "$n" "exception path '$path' must be relative to the root and must not contain '..'"; continue;; esac
  if [ -z "$reason" ]; then exc_bad "$n" "exception for $path has a blank reason: say why this file may write a job table (an exception without a reason is a hole)"; continue; fi
  if [ -n "${EXC_REASON[$path]+x}" ]; then exc_bad "$n" "duplicate exception for $path (first on line ${EXC_LINE[$path]})"; continue; fi
  EXC_REASON[$path]="$reason"; EXC_LINE[$path]="$n"; exc_order+=("$path")
done < "$EXC"

for path in ${exc_order[@]+"${exc_order[@]}"}; do
  case "$path" in
    "$SAN_DIR"/*) exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the sanctioned writer package $SAN_DIR/ needs no exception — remove the entry";;
    *)
      if [ -z "${IN_SCOPE[$path]+x}" ]; then
        if [ -e "$ROOT/$path" ]; then exc_bad "${EXC_LINE[$path]}" "stale exception for $path: exceptions cover the scanned files only (non-test Go under internal/ and cmd/, SQL under db/ outside db/migrations/) — remove the entry"
        else exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the file does not exist — remove the entry (a register that rots hides the next miss)"; fi
      elif [ "${COUNT[$path]:-0}" -lt 1 ]; then
        exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the file contains no write to a job table — remove the entry (a register that rots hides the next miss)"
      fi;;
  esac
done

# ---- everything else that writes ---------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  [ "${COUNT[$rel]:-0}" -ge 1 ] || continue
  case "$rel" in "$SAN_DIR"/*) continue;; esac
  if [ -n "${EXC_REASON[$rel]+x}" ]; then
    nexcused=$((nexcused+${COUNT[$rel]})); echo "  excused: $rel (${COUNT[$rel]} write(s), exception on line ${EXC_LINE[$rel]}): ${EXC_REASON[$rel]}"; continue
  fi
  while IFS='|' read -r off ln kind target; do
    [ -n "$off" ] || continue
    problem "$rel:$ln: write to $target ($kind) outside the sanctioned writer $SAN_DIR/ — write through the job store (internal/jobs), the one package that writes the job tables"; nout=$((nout+1))
  done <<< "${HITS[$rel]}"
done < "$T/files"

# ---- the verdict ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
echo "check-job-writers: $nout write(s) outside the sanctioned writer, $nexcused write(s) excused by exception, $nsanbad sanctioned-writer problem(s), $nexc_bad exception problem(s), over $ngo Go and $nsql SQL file(s); $nbad_files file(s) the matcher failed on"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-job-writers: RED — $fail problem(s): $nout write(s) outside $SAN_DIR/, $nsanbad sanctioned-writer problem(s), $nexc_bad exception problem(s), $nbad_files file(s) the matcher failed on"; exit 1; }
echo "check-job-writers: OK"
