#!/usr/bin/env bash
# check-m1-callers-writer.sh [--root DIR] — module 1's key table, verify.api_callers, is written by exactly two things in this repository, and the service's runtime path writes it through neither of them directly.
#
# WHY IT EXISTS. Module 1 has no API that mints a caller key: verify.api_callers is written only by its privileged identity (its migration 0010). The platform service runs as platform_app, which has no right on module 1's
# schema, and must never hold module 1's admin credential. Phase 4 chunk B16 gives the service ONE narrow door: the SECURITY DEFINER function platform.provision_m1_caller (db/install/postmodule1.sql), owned by module 1's
# grant-admin, executable by platform_app, which can write one kind of row (an INSERT of a constant-shaped caller row, ON CONFLICT DO NOTHING, read back and judged) and cannot UPDATE or DELETE anything. This gate keeps it
# the only door: nothing else in the repository may write the table, so a second writer (one that skips the read-back, picks its own caller_id, leaves a key with no owner, or runs with a credential it should not hold)
# cannot appear unnoticed.
#
# WHAT IT SCANS (printed on every run): every non-test *.go (not *_test.go) under <root>/internal and <root>/cmd, every *.sql under <root>/db EXCEPT <root>/db/migrations/, and, on a SEPARATE report line, every *.sql
# under <root>/db/migrations/ (the platform's migrations run BEFORE module 1's, so none has a reason to write module 1's table, and none is excused). Each file is normalised to one line first (CR, tab and newline become
# spaces, byte for byte), so a Go raw string that splits its SQL across lines is read as one statement and the line of the FIRST keyword is recovered from the byte offset.
# WHAT IT FLAGS: a write statement naming the table, case-insensitively, as a whole word, with the schema optional (an unqualified name would resolve through search_path) and every identifier optionally quoted, or quoted and
# escaped inside a Go string: INSERT INTO, UPDATE [ONLY], DELETE FROM [ONLY], TRUNCATE [TABLE] (also in a list of tables), COPY <table> [(columns)] FROM, MERGE INTO [ONLY], and a pgx.Identifier{"verify", "api_callers"} (what
# CopyFrom writes through). verify.api_callers_archive, verify.api_callers2, another schema's api_callers and COPY ... TO (a read) are not matched. GRANT, ALTER, COMMENT, SELECT and FOR UPDATE OF are not writes.
# THE TWO SANCTIONED WRITERS, each of which must exist and hold a write ("the instrument matched nothing" is red):
#   internal/tenant/m1key.go      the privileged-connection writer of the operator tools and the tests (ProvisionM1Caller, ProvisionM1CallerVersion, RetireM1CallerVersion): any write form
#   db/install/postmodule1.sql    the install function: EXACTLY ONE INSERT INTO verify.api_callers and no other write form (it writes one kind of row and cannot UPDATE, DELETE, TRUNCATE, COPY or MERGE anything)
# THE RUNTIME PATH. internal/tenant/m1provision.go must call platform.provision_m1_caller( (the door exists, and the Go half asks the database for it by name), and no non-test Go under internal/ OUTSIDE internal/tenant/ may
# call the privileged writers (.ProvisionM1Caller( .ProvisionM1CallerVersion( .RetireM1CallerVersion(): handed the service's pool they fail with 42501, and handed anything else they hold module 1's admin credential in the service).
# FLOOR (a gate whose subject is empty is red, classes 8 and 18): at least one Go file and at least one migration must have been scanned, both sanctioned writers must hold a write, and the matcher must pass its own canary
# (known positives and negatives, including two matches in a row) before it is trusted on the real tree.
#
# Output: `check-m1-callers-writer: scanned ...` lines, one line per finding `path:line: reason`, a `NOT CHECKED:` line, and `check-m1-callers-writer: OK` or `RED`. Exit status: 0 clean, 1 red, 2 usage error.
# Paired control: scripts/check-m1-callers-writer-selftest.sh. Fault proof: ci/faults/p4b/m1-callers-writer.
#
# NOT CHECKED (also printed on every run): test files (*_test.go) and everything under tests/ (the harness plants rows on purpose); shell scripts, the parent repository and Docs; SQL built by string concatenation, fmt.Sprintf or a
# variable around the table name; a write through a view, a function, a trigger or a rule (the install function itself is such a door; what it does is proven by the harness, TestM1LiveProvision_*); GRANTs on the table (module 1's
# grants are module 1's, and the harness's tests read them); a SQL comment between the keywords; the database's own walls. COMMENTS ARE NOT STRIPPED for the write forms: a comment that contains a write statement IS flagged
# (the gate fails closed; reword the comment).
set -u
export LC_ALL=C

usage() { echo "usage: check-m1-callers-writer.sh [--root DIR]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)   [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*) ROOT_IN="${1#*=}"; shift;;
    -h|--help) usage;;
    *)        echo "check-m1-callers-writer: unknown argument '$1'" >&2; usage;;
  esac
done
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

PRIV=internal/tenant/m1key.go
INST=db/install/postmodule1.sql
DOOR=internal/tenant/m1provision.go
SCOPE="non-test *.go under internal/ and cmd/, *.sql under db/ except db/migrations/, and (on a separate line) db/migrations/*.sql"
NOT_CHECKED="NOT CHECKED: test files (*_test.go) and everything under tests/; shell scripts, the parent repository and Docs; SQL built by string concatenation, fmt.Sprintf or a variable around the table name; a write through a view, a function, a trigger or a rule (the install function is one: the harness proves what it does); GRANTs on the table; a SQL comment between the keywords; the database's own walls. Comments are NOT stripped for the write forms: a comment containing a write statement IS flagged"

# ---- the matcher ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# The ERE has no word boundaries (ERE has no lookaround, and a consumed boundary character would hide a second match that follows closely); they are checked on the normalised text afterwards.
# (\\?")? is an optional double quote, which inside a Go interpreted string may be escaped. The schema is optional: an unqualified name resolves through search_path, and CLAUDE.md rule 12 says every
# statement names its schema, so the bare name is flagged too (another schema's table, send_managed.api_callers, is not: the match must start at the keyword and the schema, if any, must be verify).
Q='(\\?")?'
S='[[:space:]]+'
OPT_ONLY='(only[[:space:]]+)?'
TBL='('"$Q"'verify'"$Q"'[[:space:]]*\.[[:space:]]*)?'"$Q"'api_callers'"$Q"
LIST_ITEM='[A-Za-z0-9_."\]+'
F_INSERT="insert${S}into${S}${TBL}"
F_UPDATE="update${S}${OPT_ONLY}${TBL}"
F_DELETE="delete${S}from${S}${OPT_ONLY}${TBL}"
F_TRUNCATE="truncate${S}(table${S})?(${LIST_ITEM}[[:space:]]*,[[:space:]]*)*${OPT_ONLY}${TBL}"
F_COPY="copy${S}${TBL}[[:space:]]*(\([^)]*\))?[[:space:]]*from"
F_MERGE="merge${S}into${S}${OPT_ONLY}${TBL}"
F_IDENT='identifier[[:space:]]*\{[[:space:]]*"verify"[[:space:]]*,[[:space:]]*"api_callers"[[:space:]]*\}'
ERE="${F_INSERT}|${F_UPDATE}|${F_DELETE}|${F_TRUNCATE}|${F_COPY}|${F_MERGE}|${F_IDENT}"
# a call of one of the privileged writers (a method call: the name is preceded by a dot; a definition or a comment that merely names the function is not)
CALL_ERE='\.[[:space:]]*(ProvisionM1CallerVersion|ProvisionM1Caller|RetireM1CallerVersion)[[:space:]]*\('

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
canary 1 'INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ($1, $2, now(), $3::uuid)'
canary 1 'insert into verify.api_callers'
canary 1 "$(printf 'INSERT\n\tINTO\n\t\tverify.api_callers (caller_id)')"
canary 1 'INSERT INTO "verify"."api_callers" (caller_id)'
canary 1 'q := "INSERT INTO \"verify\".\"api_callers\" (caller_id)"'
canary 1 'INSERT INTO verify . api_callers (caller_id)'
canary 1 'INSERT INTO api_callers (caller_id)'
canary 1 'WITH x AS (INSERT INTO verify.api_callers (caller_id) VALUES ($1) RETURNING caller_id) SELECT 1'
canary 1 'UPDATE verify.api_callers SET not_after = now()'
canary 1 'UPDATE ONLY verify.api_callers SET status = $1'
canary 1 'DELETE FROM verify.api_callers WHERE user_id = $1'
canary 1 'DELETE FROM ONLY verify.api_callers'
canary 1 'TRUNCATE verify.api_callers'
canary 1 'TRUNCATE TABLE verify.api_callers'
canary 1 'TRUNCATE TABLE ONLY verify.api_callers CASCADE'
canary 1 'TRUNCATE verify.jobs, verify.api_callers'
canary 1 'TRUNCATE verify.api_callers, verify.jobs'
canary 1 'COPY verify.api_callers (caller_id, key_sha256) FROM STDIN'
canary 1 'COPY verify.api_callers FROM STDIN'
canary 1 'MERGE INTO verify.api_callers c USING x ON true WHEN NOT MATCHED THEN INSERT VALUES (1)'
canary 1 'tx.CopyFrom(ctx, pgx.Identifier{"verify", "api_callers"}, cols, src)'
canary 2 'INSERT INTO verify.api_callers (caller_id) VALUES ($1); DELETE FROM verify.api_callers'
# the negatives: reads, grants, DDL, a longer name, another schema, a longer word before the keyword
canary 0 'SELECT caller_id FROM verify.api_callers WHERE key_sha256 = $1'
canary 0 'SELECT 1 FROM verify.api_callers FOR UPDATE'
canary 0 'SELECT 1 FROM verify.api_callers FOR UPDATE OF verify.api_callers'
canary 0 'INSERT INTO verify.api_callers_archive (caller_id) VALUES ($1)'
canary 0 'INSERT INTO "verify"."api_callers_archive"'
canary 0 'INSERT INTO verify.api_callers2 (caller_id)'
canary 0 'INSERT INTO send_managed.api_callers (caller_id)'
canary 0 'INSERT INTO myverify.api_callers (caller_id)'
canary 0 'xinsert into verify.api_callers'
canary 0 'TRUNCATE verify.api_callers_archive'
canary 0 'COPY verify.api_callers TO STDOUT'
canary 0 'COPY (SELECT caller_id FROM verify.api_callers) TO STDOUT'
canary 0 'GRANT SELECT ON verify.api_callers TO verify_app'
canary 0 'ALTER TABLE verify.api_callers ADD COLUMN x int'
canary 0 'COMMENT ON TABLE verify.api_callers IS $$written only by module 1$$'
canary 0 'DELETE FROM verify.jobs'
canary 0 'UPDATE verify.jobs SET status = $1'
canary 0 'INSERT INTO platform.balances (user_id) VALUES ($1)'
canary 0 'SELECT platform.provision_m1_caller($1::uuid, $2::bytea)'
canary 0 'pgx.Identifier{"verify", "api_callers_archive"}'
run_canaries; canary_bad=$?
if [ "$canary_bad" -gt 0 ]; then
  echo "check-m1-callers-writer: RED — the matcher failed its own canary ($canary_bad case(s) above): the instrument cannot be trusted (is grep -o -b -E available?)"
  exit 1
fi
# the call matcher has its own canary: a method call is a hit, a definition, a bare mention and the ensurer's own method are not
callhits() { printf '%s' "$1" | tr '\r\t\n' '   ' | grep -a -o -E "$CALL_ERE" | wc -l | tr -d ' '; }
for c in 'r.ProvisionM1Caller(ctx, db, u)|1' 'res . ProvisionM1CallerVersion(ctx, db, u, 2)|1' 'x.RetireM1CallerVersion(ctx, db, u, 1, 0)|1' 'a.ProvisionM1Caller(c, d, u); b.RetireM1CallerVersion(c, d, u, 1, 0)|2' \
         'func (r *Resolver) ProvisionM1Caller(ctx context.Context)|0' '// see ProvisionM1Caller for the row it writes|0' 'e.EnsureCaller(ctx, u)|0' 'tenant.M1CallerID(u)|0'; do
  if [ "$(callhits "${c%|*}")" != "${c##*|}" ]; then echo "check-m1-callers-writer: RED — the call matcher failed its own canary on: ${c%|*}"; exit 1; fi
done

# ---- the subject ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-m1-callers-writer: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }

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

echo "check-m1-callers-writer: scope: $SCOPE (root: $ROOT)"
echo "check-m1-callers-writer: scanned $ngo Go file(s) and $nsql SQL file(s) outside db/migrations/, and $nmigf migration file(s)"
if [ "$ngo" -lt 1 ]; then
  echo "check-m1-callers-writer: RED — found 0 Go files under internal/ and cmd/ in $ROOT: the instrument matched nothing (a wrong --root, or an empty checkout)"
  exit 1
fi
if [ "$nmigf" -lt 1 ]; then
  echo "check-m1-callers-writer: RED — found 0 migration files under db/migrations/ in $ROOT: the instrument matched nothing (a wrong --root, or the migrations moved)"
  exit 1
fi

# ---- the hits: HITS[file] = lines "offset|line|kind", in file order ------------------------------------------------------------------------------------------------------------------
declare -A HITS COUNT KINDS
nbad_files=0
while IFS= read -r rel; do
  f="$ROOT/$rel"
  # every match contains the word "api_callers": a file without it (in any case) cannot hold one, and is skipped without spawning a process (checked in bash itself, so no external tool can fail silently here)
  content="$(<"$f")"; case "${content,,}" in *api_callers*) ;; *) continue;; esac
  normalise "$f" "$T/n"
  offsets "$T/n" "$T/off"; rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-m1-callers-writer: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue
  fi
  while IFS='|' read -r off kind; do
    [ -n "$off" ] || continue
    ln=$(( $(head -c "$off" "$f" | wc -l) + 1 ))
    HITS[$rel]="${HITS[$rel]:-}$off|$ln|$kind"$'\n'; COUNT[$rel]=$(( ${COUNT[$rel]:-0} + 1 ))
    [ "$kind" = "INSERT INTO" ] && KINDS[$rel]=$(( ${KINDS[$rel]:-0} + 1 ))
  done < "$T/off"
done < "$T/files"

fail=$nbad_files; nout=0; nmig=0; nsan=0; ncall=0
problem() { echo "$1"; fail=$((fail+1)); }

# ---- the two sanctioned writers ---------------------------------------------------------------------------------------------------------------------------------------------------------
# 1. the privileged-connection writer: it must exist and write (the operator tools' and the tests' door); every write form is allowed there
if [ ! -f "$ROOT/$PRIV" ]; then
  problem "$PRIV:1: the sanctioned privileged writer does not exist (the instrument has no subject: restore the file, or move the gate's sanctioned path deliberately)"; nsan=$((nsan+1))
elif [ "${COUNT[$PRIV]:-0}" -lt 1 ]; then
  problem "$PRIV:1: the sanctioned privileged writer holds no write statement on verify.api_callers: the instrument matched nothing (a writer that never writes, or a matcher that no longer reads it)"; nsan=$((nsan+1))
fi
# 2. the install function: exactly one INSERT and no other write form; it names its own function
if [ ! -f "$ROOT/$INST" ]; then
  problem "$INST:1: the sanctioned install step does not exist: the service has no door to module 1's key table (the instrument has no subject)"; nsan=$((nsan+1))
else
  if [ "${COUNT[$INST]:-0}" -lt 1 ]; then
    problem "$INST:1: the install step holds no INSERT INTO verify.api_callers: the instrument matched nothing (a function that never writes, or a matcher that no longer reads it)"; nsan=$((nsan+1))
  else
    while IFS='|' read -r off ln kind; do
      [ -n "$off" ] || continue
      if [ "$kind" != "INSERT INTO" ]; then
        problem "$INST:$ln: write to verify.api_callers ($kind) in the install function: it may INSERT one kind of row and cannot UPDATE, DELETE, TRUNCATE, COPY or MERGE anything"; nsan=$((nsan+1))
      fi
    done <<< "${HITS[$INST]}"
    if [ "${KINDS[$INST]:-0}" -ne 1 ]; then
      problem "$INST:1: the install step holds ${KINDS[$INST]:-0} INSERT INTO verify.api_callers, want exactly 1 (one kind of row, written in one place)"; nsan=$((nsan+1))
    fi
  fi
  if ! grep -a -q -F -e 'platform.provision_m1_caller' "$ROOT/$INST"; then
    problem "$INST:1: the install step does not define platform.provision_m1_caller: the service's door has another name or does not exist"; nsan=$((nsan+1))
  fi
fi
# 3. the Go half of the door: it asks the database for the function by name
if [ ! -f "$ROOT/$DOOR" ]; then
  problem "$DOOR:1: the service's runtime path to module 1's key table does not exist (the ensurer that calls platform.provision_m1_caller is missing)"; nsan=$((nsan+1))
elif ! grep -a -q -F -e 'platform.provision_m1_caller(' "$ROOT/$DOOR"; then
  problem "$DOOR:1: the ensurer does not call platform.provision_m1_caller( : the service's runtime path to module 1's key table must go through the function"; nsan=$((nsan+1))
fi
echo "check-m1-callers-writer: sanctioned writers: $PRIV ${COUNT[$PRIV]:-0} write statement(s); $INST ${COUNT[$INST]:-0} write statement(s), ${KINDS[$INST]:-0} INSERT INTO"

# ---- everything else that writes ---------------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  [ "${COUNT[$rel]:-0}" -ge 1 ] || continue
  [ "$rel" != "$PRIV" ] && [ "$rel" != "$INST" ] || continue
  while IFS='|' read -r off ln kind; do
    [ -n "$off" ] || continue
    if [ -n "${IS_MIG[$rel]+x}" ]; then
      problem "$rel:$ln: write to verify.api_callers ($kind) in a platform migration — the platform's migrations run before module 1's and never write its table (module 1's rows are written by its own identity and by platform.provision_m1_caller)"; nmig=$((nmig+1))
    else
      problem "$rel:$ln: write to verify.api_callers ($kind) outside the two sanctioned writers $PRIV and $INST — register a user's key through platform.provision_m1_caller (tenant.M1CallerEnsurer)"; nout=$((nout+1))
    fi
  done <<< "${HITS[$rel]}"
done < "$T/files"

# ---- the privileged writers are not called from the service's runtime path -----------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  case "$rel" in internal/tenant/*) continue;; internal/*) ;; *) continue;; esac
  f="$ROOT/$rel"
  content="$(<"$f")"; case "$content" in *ProvisionM1Caller*|*RetireM1CallerVersion*) ;; *) continue;; esac
  normalise "$f" "$T/n"
  grep -a -o -b -E "$CALL_ERE" "$T/n" > "$T/call.out"; rc=$?
  if [ "$rc" -gt 1 ]; then echo "check-m1-callers-writer: the call matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; fail=$((fail+1)); continue; fi
  while IFS= read -r rec; do
    [ -n "$rec" ] || continue
    off="${rec%%:*}"; name="$(printf '%s' "${rec#*:}" | sed -E 's/^\.[[:space:]]*//; s/[[:space:]]*\($//')"
    ln=$(( $(head -c "$off" "$f" | wc -l) + 1 ))
    problem "$rel:$ln: call of $name( outside internal/tenant/ — the service's runtime path reaches module 1's key table only through platform.provision_m1_caller (tenant.M1CallerEnsurer); the privileged writers are for operator tools and tests"; ncall=$((ncall+1))
  done < "$T/call.out"
done < "$T/go.rel"

# ---- the verdict ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
echo "check-m1-callers-writer: $nout write(s) outside the sanctioned writers, $nmig write(s) in platform migrations, $nsan sanctioned-writer problem(s), $ncall call(s) of a privileged writer from the runtime path, over $ngo Go and $nsql SQL file(s) and $nmigf migration file(s); $nbad_files file(s) the matcher failed on"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-m1-callers-writer: RED — $fail problem(s): $nout write(s) outside $PRIV and $INST, $nmig migration write(s), $nsan sanctioned-writer problem(s), $ncall privileged-writer call(s) from the runtime path, $nbad_files file(s) the matcher failed on"; exit 1; }
echo "check-m1-callers-writer: OK"
