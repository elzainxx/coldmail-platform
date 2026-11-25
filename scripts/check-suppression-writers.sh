#!/usr/bin/env bash
# check-suppression-writers.sh [--root DIR] [--exceptions FILE] — no code may write platform.suppressions except the one store function that validates the address first (internal/suppress/store.go).
#
# WHY IT EXISTS. platform.suppressions has a CHECK (suppressions_email_norm_v1, platform_0009) that cannot decode punycode, so for two address shapes (a label such as xn--a that is not valid
# punycode, and one whose decoded form breaks a rule) the Go validator, internal/norm Validate, is the ONLY wall: the table would store them if they reached it. The owner accepted that limit
# (Phase 4, Part A item A2) and ordered it made a gate: no code writes the table except the store function that validates first. internal/suppress Add is that function; this gate fails when anything
# else writes the table, and when a write in the sanctioned file is not preceded by a call of norm.Validate( earlier in the same function.
#
# WHAT IT SCANS (printed on every run): every non-test *.go (not *_test.go) under <root>/internal and <root>/cmd, every *.sql under <root>/db EXCEPT <root>/db/migrations/ (the application's SQL), and,
# on a SEPARATE report line, every *.sql under <root>/db/migrations/ (migrations create the table and its constraints; a row written there is a different kind of finding with its own register).
# Each file is normalised to one line first (CR, tab and newline become spaces, byte for byte), so a Go raw string that splits its SQL across lines is read as one statement and the line of the
# FIRST keyword is recovered from the byte offset.
# WHAT IT FLAGS: a write statement naming the table, case-insensitively, as a whole word, with the schema optional (an unqualified name would resolve through search_path) and every identifier
# optionally quoted, or quoted and escaped inside a Go string ("platform"."suppressions", \"platform\".\"suppressions\"): INSERT INTO, UPDATE [ONLY], DELETE FROM [ONLY], TRUNCATE [TABLE] (also in a list
# of tables), COPY <table> [(columns)] FROM, MERGE INTO [ONLY], and a pgx.Identifier{"platform", "suppressions"} (what CopyFrom writes through). platform.suppressions_archive, platform.suppressions2,
# another schema's suppressions and COPY ... TO (a read) are not matched. GRANT, ALTER, COMMENT, SELECT and FOR UPDATE OF are not writes.
# THE ONE SANCTIONED FILE: internal/suppress/store.go. It must exist, must hold at least one write ("the instrument matched nothing" is red: a writer that never writes, or a matcher that no longer
# reads it), must hold no block comment (a /* comment cannot be told from code by this gate), and EVERY write in it must sit INSIDE a top-level function (a package-level statement is refused) and come AFTER a call of norm.Validate( that is code in that SAME function (a line whose text before the
# call holds // is a comment and does not count; a call in the function before it is not credited to it: a neighbour's fix, class 11). Every other file that writes the table is a finding.
# MIGRATION EXCEPTIONS: tests/suppression-writer-exceptions.txt (or --exceptions FILE), lines `relative/path|reason` (`#` comments and blank lines allowed), for a db/migrations/*.sql file that must
# write rows. An exception is RED when it is STALE (the file does not exist, is not a migration, or contains no write: a register that rots hides the next miss, false-green class 17), when its reason
# is blank, when its line is malformed, and when the same file is listed twice. The exception file starts with comments only: no migration writes a row.
# FLOOR (a gate whose subject is empty is red, classes 8 and 18): at least one Go file and at least one migration must have been scanned, the sanctioned file must hold a write, and the matcher must
# pass its own canary (known positives and negatives, including two matches in a row) before it is trusted on the real tree.
#
# Output: `check-suppression-writers: scanned ...` lines, one line per finding `path:line: reason`, a `NOT CHECKED:` line, and `check-suppression-writers: OK` or `RED`. Exit status: 0 clean, 1 red,
# 2 usage error. Paired control: scripts/check-suppression-writers-selftest.sh. Fault proofs: ci/faults/p4a/.
#
# NOT CHECKED (also printed on every run): test files and everything under tests/; shell scripts and Docs; SQL built by string concatenation, fmt.Sprintf or a variable around the table name; a write
# through a view, a function, a trigger or a rule; a SQL comment between the keywords; whether norm.Validate( runs on the path that reaches the write (the gate asks that a call stands EARLIER in the same function of
# store.go, not that its result is used or that the branch executes: the harness test tests/harness/suppress_store_test.go proves the behaviour against a real database); a norm.Validate( inside a
# string literal on a line without //; the database's own walls (the grants and the CHECK): this gate reads source, not the database. COMMENTS ARE NOT STRIPPED for the write forms: a comment that
# contains a write statement IS flagged (the gate fails closed; reword the comment).
set -u
export LC_ALL=C

usage() { echo "usage: check-suppression-writers.sh [--root DIR] [--exceptions FILE]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""; EXC_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)         [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)       ROOT_IN="${1#*=}"; shift;;
    --exceptions)   [ $# -ge 2 ] || usage; EXC_IN="$2"; shift 2;;
    --exceptions=*) EXC_IN="${1#*=}"; shift;;
    -h|--help)      usage;;
    *)              echo "check-suppression-writers: unknown argument '$1'" >&2; usage;;
  esac
done
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

SAN="internal/suppress/store.go"
SCOPE="non-test *.go under internal/ and cmd/, *.sql under db/ except db/migrations/, and (on a separate line) db/migrations/*.sql"
NOT_CHECKED="NOT CHECKED: test files (*_test.go) and everything under tests/; shell scripts and Docs; SQL built by string concatenation, fmt.Sprintf or a variable around the table name; a write through a view, a function, a trigger or a rule; a SQL comment between the keywords; whether norm.Validate( runs on the path that reaches the write (only that a call stands earlier in the same function of $SAN; the harness test proves the behaviour); a norm.Validate( inside a string literal on a line without //; the database's own walls (the grants and the CHECK). Comments are NOT stripped for the write forms: a comment containing a write statement IS flagged"

# ---- the matcher ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# The ERE has no word boundaries (ERE has no lookaround, and a consumed boundary character would hide a second match that follows closely); they are checked on the normalised text afterwards.
# (\\?")? is an optional double quote, which inside a Go interpreted string may be escaped. The schema is optional: an unqualified name resolves through search_path, and CLAUDE.md rule 12 says every
# statement names its schema, so the bare name is flagged too (another schema's table, send_managed.suppressions, is not: the match must start at the keyword and the schema, if any, must be platform).
Q='(\\?")?'
S='[[:space:]]+'
OPT_ONLY='(only[[:space:]]+)?'
TBL='('"$Q"'platform'"$Q"'[[:space:]]*\.[[:space:]]*)?'"$Q"'suppressions'"$Q"
LIST_ITEM='[A-Za-z0-9_."\]+'
F_INSERT="insert${S}into${S}${TBL}"
F_UPDATE="update${S}${OPT_ONLY}${TBL}"
F_DELETE="delete${S}from${S}${OPT_ONLY}${TBL}"
F_TRUNCATE="truncate${S}(table${S})?(${LIST_ITEM}[[:space:]]*,[[:space:]]*)*${OPT_ONLY}${TBL}"
F_COPY="copy${S}${TBL}[[:space:]]*(\([^)]*\))?[[:space:]]*from"
F_MERGE="merge${S}into${S}${OPT_ONLY}${TBL}"
F_IDENT='identifier[[:space:]]*\{[[:space:]]*"platform"[[:space:]]*,[[:space:]]*"suppressions"[[:space:]]*\}'
ERE="${F_INSERT}|${F_UPDATE}|${F_DELETE}|${F_TRUNCATE}|${F_COPY}|${F_MERGE}|${F_IDENT}"

# normalise <in> <out>: CR, tab and newline become spaces, byte for byte, so an offset in <out> is an offset in <in>
normalise() { tr '\r\t\n' '   ' < "$1" > "$2"; }
# kind_of <matched text>: what the write is, from its first word
kind_of() {
  local w="${1%%[^A-Za-z]*}"
  case "${w,,}" in
    insert) echo "INSERT INTO";; update) echo "UPDATE";; delete) echo "DELETE FROM";; truncate) echo "TRUNCATE";;
    copy) echo "COPY ... FROM";; merge) echo "MERGE INTO";; identifier) echo "pgx.Identifier, as CopyFrom writes through it";; *) echo "write";;
  esac
}
# offsets <normalised file> <out file>: writes one `offset|kind` line for every match whose edges are word boundaries; returns non-zero when grep ITSELF failed (a crashed grep prints no match, which would
# read as a clean file: grep exits 0 when it matched, 1 when it did not, and anything else is a failure)
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
    printf '%s|%s\n' "$off" "$(kind_of "$text")" >> "$out"
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
# the positives: every write form, every quoting, the table's own name and the bare one
canary 1 'INSERT INTO platform.suppressions (user_id, email, source) VALUES ($1, $2, $3)'
canary 1 'insert into platform.suppressions'
canary 1 "$(printf 'INSERT\n\tINTO\n\t\tplatform.suppressions (email)')"
canary 1 'INSERT INTO "platform"."suppressions" (email)'
canary 1 'q := "INSERT INTO \"platform\".\"suppressions\" (email)"'
canary 1 'INSERT INTO platform . suppressions (email)'
canary 1 'INSERT INTO suppressions (email)'
canary 1 'WITH x AS (INSERT INTO platform.suppressions (email) VALUES ($1) RETURNING email) SELECT 1'
canary 1 'UPDATE platform.suppressions SET source = $1'
canary 1 'UPDATE ONLY platform.suppressions SET source = $1'
canary 1 'DELETE FROM platform.suppressions WHERE user_id = $1'
canary 1 'DELETE FROM ONLY platform.suppressions'
canary 1 'TRUNCATE platform.suppressions'
canary 1 'TRUNCATE TABLE platform.suppressions'
canary 1 'TRUNCATE TABLE ONLY platform.suppressions CASCADE'
canary 1 'TRUNCATE platform.ledger, platform.suppressions'
canary 1 'TRUNCATE platform.suppressions, platform.ledger'
canary 1 'COPY platform.suppressions (user_id, email, source) FROM STDIN'
canary 1 'COPY platform.suppressions FROM STDIN'
canary 1 'MERGE INTO platform.suppressions s USING x ON true WHEN NOT MATCHED THEN INSERT VALUES (1)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"platform", "suppressions"}, cols, src)'
canary 2 'INSERT INTO platform.suppressions (email) VALUES ($1); DELETE FROM platform.suppressions'
# the negatives: reads, grants, DDL, a longer name, another schema, a longer word before the keyword
canary 0 'SELECT email FROM platform.suppressions WHERE user_id = $1'
canary 0 'SELECT 1 FROM platform.suppressions FOR UPDATE'
canary 0 'SELECT 1 FROM platform.suppressions FOR UPDATE OF platform.suppressions'
canary 0 'INSERT INTO platform.suppressions_archive (email) VALUES ($1)'
canary 0 'INSERT INTO "platform"."suppressions_archive" (email)'
canary 0 'INSERT INTO platform.suppressions2 (email)'
canary 0 'INSERT INTO send_managed.suppressions (email)'
canary 0 'INSERT INTO myplatform.suppressions (email)'
canary 0 'xinsert into platform.suppressions'
canary 0 'TRUNCATE platform.suppressions_archive'
canary 0 'COPY platform.suppressions TO STDOUT'
canary 0 'COPY (SELECT email FROM platform.suppressions) TO STDOUT'
canary 0 'GRANT SELECT, INSERT ON platform.suppressions TO platform_app'
canary 0 'ALTER TABLE platform.suppressions ADD CONSTRAINT c CHECK (true)'
canary 0 'COMMENT ON TABLE platform.suppressions IS $$rows are never updated$$'
canary 0 'DELETE FROM platform.ledger'
canary 0 'UPDATE platform.users SET is_active = false'
canary 0 'INSERT INTO platform.balances (user_id) VALUES ($1) ON CONFLICT DO UPDATE SET x = 1'
canary 0 'pgx.Identifier{"platform", "suppressions_archive"}'
run_canaries; canary_bad=$?
if [ "$canary_bad" -gt 0 ]; then
  echo "check-suppression-writers: RED — the matcher failed its own canary ($canary_bad case(s) above): the instrument cannot be trusted (is grep -o -b -E available?)"
  exit 1
fi

# ---- the subject ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-suppression-writers: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }
EXC="${EXC_IN:-$ROOT/tests/suppression-writer-exceptions.txt}"
EXCNAME="${EXC#"$ROOT"/}"

: > "$T/go.abs"; : > "$T/sql.abs"; : > "$T/mig.abs"
for d in internal cmd; do
  [ -d "$ROOT/$d" ] || continue
  find "$ROOT/$d" -type f -name '*.go' ! -name '*_test.go' >> "$T/go.abs"
done
if [ -d "$ROOT/db" ]; then find "$ROOT/db" -path "$ROOT/db/migrations" -prune -o -type f -name '*.sql' -print >> "$T/sql.abs"; fi
if [ -d "$ROOT/db/migrations" ]; then find "$ROOT/db/migrations" -type f -name '*.sql' >> "$T/mig.abs"; fi
: > "$T/files"
declare -A IS_MIG
for kind in go sql mig; do
  sort "$T/$kind.abs" | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/$kind.rel"
  cat "$T/$kind.rel" >> "$T/files"
done
while IFS= read -r rel; do [ -n "$rel" ] && IS_MIG[$rel]=1; done < "$T/mig.rel"
ngo=$(wc -l < "$T/go.rel" | tr -d ' '); nsql=$(wc -l < "$T/sql.rel" | tr -d ' '); nmigf=$(wc -l < "$T/mig.rel" | tr -d ' ')

echo "check-suppression-writers: scope: $SCOPE (root: $ROOT)"
echo "check-suppression-writers: scanned $ngo Go file(s) and $nsql SQL file(s) outside db/migrations/, and $nmigf migration file(s)"
if [ "$ngo" -lt 1 ]; then
  echo "check-suppression-writers: RED — found 0 Go files under internal/ and cmd/ in $ROOT: the instrument matched nothing (a wrong --root, or an empty checkout)"
  exit 1
fi
if [ "$nmigf" -lt 1 ]; then
  echo "check-suppression-writers: RED — found 0 migration files under db/migrations/ in $ROOT: the instrument matched nothing (a wrong --root, or the migrations moved)"
  exit 1
fi
if [ ! -f "$EXC" ]; then
  echo "check-suppression-writers: RED — the exceptions file $EXC does not exist (the gate cannot say what it excuses; an empty register is a file with only comments)"
  exit 1
fi

# ---- the hits: HITS[file] = lines "offset|line|kind", in file order ------------------------------------------------------------------------------------------------------------------
declare -A HITS COUNT EXC_REASON EXC_LINE
nbad_files=0
while IFS= read -r rel; do
  f="$ROOT/$rel"
  # every match contains the word "suppressions": a file without it (in any case) cannot hold one, and is skipped without spawning a process (checked in bash itself, so no external tool can fail silently here)
  content="$(<"$f")"; case "${content,,}" in *suppressions*) ;; *) continue;; esac
  normalise "$f" "$T/n"
  offsets "$T/n" "$T/off"; rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-suppression-writers: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue
  fi
  while IFS='|' read -r off kind; do
    [ -n "$off" ] || continue
    ln=$(( $(head -c "$off" "$f" | wc -l) + 1 ))
    HITS[$rel]="${HITS[$rel]:-}$off|$ln|$kind"$'\n'; COUNT[$rel]=$(( ${COUNT[$rel]:-0} + 1 ))
  done < "$T/off"
done < "$T/files"

fail=$nbad_files; nout=0; nvf=0; nsanbad=0; nmig=0; nexc_bad=0; nexcused=0
problem() { echo "$1"; fail=$((fail+1)); }
exc_bad() { echo "$EXCNAME:$1: $2"; fail=$((fail+1)); nexc_bad=$((nexc_bad+1)); }

# ---- the sanctioned writer ------------------------------------------------------------------------------------------------------------------------------------------------------------
V=""; san_writes=0; VOFFS=(); FN_S=(); FN_E=()
if [ ! -f "$ROOT/$SAN" ]; then
  problem "$SAN:1: the sanctioned writer does not exist: there is no place the platform may write platform.suppressions from (the instrument has no subject; restore the file or move the gate's sanctioned path deliberately)"; nsanbad=$((nsanbad+1))
else
  san_writes="${COUNT[$SAN]:-0}"
  grep -a -q -F -e '/*' "$ROOT/$SAN"; brc=$?
  if [ "$brc" -eq 0 ]; then
    problem "$SAN:1: the sanctioned writer contains a block comment (/*): this gate reads code and cannot tell a block comment from code, so a norm.Validate( inside one could pass for the call — use // comments"; nsanbad=$((nsanbad+1))
  elif [ "$brc" -gt 1 ]; then
    problem "$SAN:1: grep failed on the sanctioned writer (exit status $brc): the file was not read, so a pass would prove nothing"; nsanbad=$((nsanbad+1))
  fi
  # the offset of every norm.Validate( that is code: nothing on its line before it contains //
  grep -a -o -b -F -e 'norm.Validate(' "$ROOT/$SAN" > "$T/v.out"; vrc=$?
  if [ "$vrc" -gt 1 ]; then
    problem "$SAN:1: grep failed on the sanctioned writer (exit status $vrc): the file was not read, so a pass would prove nothing"; nsanbad=$((nsanbad+1))
  else
    SS="$(<"$ROOT/$SAN")"
    while IFS= read -r vrec; do
      voff="${vrec%%:*}"; before="${SS:0:voff}"; pre="${before##*$'\n'}"
      case "$pre" in *'//'*) continue;; esac
      VOFFS+=("$voff")
    done < "$T/v.out"
    V="${VOFFS[0]:-}"
  fi
  # the top-level functions of the file, as `start end` byte offsets: a line that begins with `func` in column 0 opens one, the next line that begins with `}` in column 0 closes it (gofmt writes
  # every top-level function so; a one-line function opens and closes on its own line). A write is judged against the function that holds it, so a validate call in the function BEFORE it (or a
  # package-level statement) cannot be credited to it: class 11, a gate crediting a neighbour's fix.
  awk 'BEGIN { off = 0; infn = 0 }
       { t = $0; sub(/\r$/, "", t); n = length($0) + 1
         if (!infn && t ~ /^func[ (]/) { start = off; infn = 1; if (t ~ /\{.*\}[ \t]*$/) { print start, off + n; infn = 0 } }
         else if (infn && t ~ /^\}/) { print start, off + n; infn = 0 }
         off += n }
       END { if (infn) print start, off }' "$ROOT/$SAN" > "$T/fn.spans"; arc=$?
  if [ "$arc" -ne 0 ]; then
    problem "$SAN:1: awk failed on the sanctioned writer (exit status $arc): its functions were not read, so a pass would prove nothing"; nsanbad=$((nsanbad+1))
  else
    while read -r fs fe; do [ -n "$fs" ] || continue; FN_S+=("$fs"); FN_E+=("$fe"); done < "$T/fn.spans"
  fi
  if [ "$san_writes" -lt 1 ]; then
    problem "$SAN:1: the sanctioned writer holds no write statement on platform.suppressions: the instrument matched nothing (a writer that never writes, or a matcher that no longer reads it)"; nsanbad=$((nsanbad+1))
  else
    while IFS='|' read -r off ln kind; do
      [ -n "$off" ] || continue
      fstart=""
      for ((i = 0; i < ${#FN_S[@]}; i++)); do
        if [ "${FN_S[i]}" -le "$off" ] && [ "$off" -lt "${FN_E[i]}" ]; then fstart="${FN_S[i]}"; break; fi
      done
      if [ -z "$fstart" ]; then
        problem "$SAN:$ln: write to platform.suppressions ($kind) is not inside a function — the sanctioned writer must write from a function that calls norm.Validate( first, in the same function"; nvf=$((nvf+1)); continue
      fi
      seen=0
      for v in ${VOFFS[@]+"${VOFFS[@]}"}; do
        if [ "$v" -ge "$fstart" ] && [ "$v" -lt "$off" ]; then seen=1; break; fi
      done
      if [ "$seen" -eq 0 ]; then
        problem "$SAN:$ln: write to platform.suppressions ($kind) is not preceded by a call of norm.Validate( earlier in the same function — the sanctioned writer must validate the address first, then write what the validator returned"; nvf=$((nvf+1))
      fi
    done <<< "${HITS[$SAN]}"
  fi
fi
if [ "${#VOFFS[@]}" -gt 0 ]; then vtext="${#VOFFS[@]} norm.Validate( call(s) outside comments, the first at byte $V"; else vtext="no norm.Validate( call outside a comment"; fi
echo "check-suppression-writers: sanctioned writer $SAN: $san_writes write statement(s), $vtext"

# ---- everything else that writes ---------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  [ "${COUNT[$rel]:-0}" -ge 1 ] || continue
  [ "$rel" != "$SAN" ] || continue
  [ -z "${IS_MIG[$rel]+x}" ] || continue
  while IFS='|' read -r off ln kind; do
    [ -n "$off" ] || continue
    problem "$rel:$ln: write to platform.suppressions ($kind) outside the sanctioned writer $SAN — write through suppress.Add, which validates the address first"; nout=$((nout+1))
  done <<< "${HITS[$rel]}"
done < "$T/files"

# ---- the migration exceptions ---------------------------------------------------------------------------------------------------------------------------------------------------------
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
  if [ -z "$reason" ]; then exc_bad "$n" "exception for $path has a blank reason: say why this migration may write platform.suppressions (an exception without a reason is a hole)"; continue; fi
  if [ -n "${EXC_REASON[$path]+x}" ]; then exc_bad "$n" "duplicate exception for $path (first on line ${EXC_LINE[$path]})"; continue; fi
  EXC_REASON[$path]="$reason"; EXC_LINE[$path]="$n"; exc_order+=("$path")
done < "$EXC"

for path in ${exc_order[@]+"${exc_order[@]}"}; do
  if [ -n "${IS_MIG[$path]+x}" ]; then
    if [ "${COUNT[$path]:-0}" -lt 1 ]; then exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the migration contains no write to platform.suppressions — remove the entry (a register that rots hides the next miss)"; fi
  elif [ -e "$ROOT/$path" ]; then
    exc_bad "${EXC_LINE[$path]}" "stale exception for $path: exceptions cover db/migrations/*.sql only (the sanctioned writer is $SAN, and no other file may write the table) — remove the entry"
  else
    exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the file does not exist — remove the entry (a register that rots hides the next miss)"
  fi
done

# ---- the migrations ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  [ "${COUNT[$rel]:-0}" -ge 1 ] || continue
  if [ -n "${EXC_REASON[$rel]+x}" ]; then
    nexcused=$((nexcused+${COUNT[$rel]})); echo "  excused: $rel (${COUNT[$rel]} write(s), exception on line ${EXC_LINE[$rel]}): ${EXC_REASON[$rel]}"; continue
  fi
  while IFS='|' read -r off ln kind; do
    [ -n "$off" ] || continue
    problem "$rel:$ln: write to platform.suppressions ($kind) in a migration — a migration creates the table and its constraints and writes no rows (list the file in tests/suppression-writer-exceptions.txt with a reason if it must)"; nmig=$((nmig+1))
  done <<< "${HITS[$rel]}"
done < "$T/mig.rel"

# ---- the verdict ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
echo "check-suppression-writers: $nout write(s) outside the sanctioned writer, $nvf write(s) in it before a validate call, $nsanbad sanctioned-writer problem(s), over $ngo Go and $nsql SQL file(s)"
echo "check-suppression-writers: migrations: $nmig write(s) flagged, $nexcused write(s) excused by exception, $nexc_bad exception problem(s), over $nmigf migration file(s); $nbad_files file(s) the matcher failed on"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-suppression-writers: RED — $fail problem(s): $nout write(s) outside $SAN, $nvf not preceded by norm.Validate(, $nsanbad sanctioned-writer problem(s), $nmig migration write(s), $nexc_bad exception problem(s), $nbad_files file(s) the matcher failed on"; exit 1; }
echo "check-suppression-writers: OK"
