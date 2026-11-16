#!/usr/bin/env bash
# check-user-lists.sh [--root DIR] [--exceptions FILE] — platform code must never SELECT from public.users directly; platform.listable_users is the ONLY sanctioned way to list users.
#
# WHY IT EXISTS. public.users holds one row that is not a person: the all-zero SYSTEM USER, module 2's owner of shared pools (no login). The owner's ruling (Docs/phase-2-spec.md, decision 12) is
# that it is excluded from every user list the platform exposes. platform_0004 creates the view platform.listable_users (public.users without that row, and without any row that has no
# Supabase login) and this gate fails when platform code reads public.users any other way, so a new screen, count or job cannot list the system user by forgetting to filter it.
#
# WHAT IT SCANS (printed on every run): every non-test *.go (not *_test.go) under <root>/internal and <root>/cmd, and every *.sql under <root>/db EXCEPT <root>/db/migrations/ (migrations
# create objects, they are not application reads: the view itself selects public.users there).
# WHAT IT FLAGS: FROM or JOIN, then whitespace (a newline and indentation count: a Go raw string splits its SQL across lines, so each file is normalised to one line first and the line number
# of the FROM/JOIN keyword is recovered from the byte offset), then an optional ONLY, then public.users, case-insensitively, as a whole word: public.users_archive and platform.listable_users do
# not match; "public"."users" (quoted, or quoted-and-escaped inside a Go string) does. INSERT INTO public.users and UPDATE public.users are not lists and pass.
# EXCEPTIONS: tests/user-list-exceptions.txt (or --exceptions FILE), lines `relative/path|reason` (`#` comments and blank lines allowed). An excepted file may select. An exception is RED when
# it is STALE (the file does not exist, is not one the gate scans, or contains no match: a register that rots hides the next miss, false-green class 17), when its reason is blank, when its line
# is malformed, and when the same file is listed twice.
# FLOOR (a gate whose subject is empty is red, classes 8 and 18): at least one Go file must have been scanned ("found 0 Go files ... the instrument matched nothing"), and the matcher must pass
# its own canary (known positives and negatives, including two matches in a row) before it is trusted on the real tree.
#
# Output: `check-user-lists: scanned N Go file(s) and M SQL file(s)`, one line per finding
#   path:line: SELECT from public.users — use platform.listable_users (or add a reasoned exception in tests/user-list-exceptions.txt)
# and a `NOT CHECKED:` line. Exit status: 0 clean, 1 red, 2 usage error. Paired control: scripts/check-user-lists-selftest.sh.
#
# NOT CHECKED (also printed on every run): test files and everything under tests/ (a test may read the table it is proving things about); db/migrations/; shell scripts and Docs; SQL built by
# string concatenation or fmt.Sprintf around the table name; a read of public.users through any view or function other than platform.listable_users (including another view over it);
# an unqualified FROM users (it would resolve through search_path); a comma-separated FROM list, USING, TABLE and COPY forms (only FROM/JOIN followed directly by the table is matched); a SQL
# comment between FROM and the table name. COMMENTS ARE NOT STRIPPED: a comment that contains `FROM public.users` IS flagged (the gate fails closed; reword the comment or except the file).
# DELETE FROM public.users matches too (the platform never deletes from the table, CLAUDE.md rule 15), and its finding line still says SELECT.
set -u
export LC_ALL=C

usage() { echo "usage: check-user-lists.sh [--root DIR] [--exceptions FILE]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""; EXC_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)         [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)       ROOT_IN="${1#*=}"; shift;;
    --exceptions)   [ $# -ge 2 ] || usage; EXC_IN="$2"; shift 2;;
    --exceptions=*) EXC_IN="${1#*=}"; shift;;
    -h|--help)      usage;;
    *)              echo "check-user-lists: unknown argument '$1'" >&2; usage;;
  esac
done
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

FIND_TAIL="SELECT from public.users — use platform.listable_users (or add a reasoned exception in tests/user-list-exceptions.txt)"
SCOPE="non-test *.go under internal/ and cmd/, and *.sql under db/ except db/migrations/"
NOT_CHECKED="NOT CHECKED: test files (*_test.go) and everything under tests/; db/migrations/; shell scripts and Docs; SQL built by string concatenation or fmt.Sprintf around the table name; a read of public.users through any view or function other than platform.listable_users; an unqualified FROM users (it resolves through search_path); a comma-separated FROM list, USING, TABLE and COPY forms; a SQL comment between FROM and the table name. Comments are NOT stripped: a comment containing 'FROM public.users' IS flagged"

# ---- the matcher ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# The ERE has no word boundaries (ERE has no lookaround, and a consumed boundary character would hide a second match that follows closely); they are checked on the normalised text afterwards.
# (\\?")? is an optional double quote, which inside a Go interpreted string may be escaped.
ERE='(from|join)[[:space:]]+(only[[:space:]]+)?(\\?")?public(\\?")?[[:space:]]*\.[[:space:]]*(\\?")?users(\\?")?'
# normalise <in> <out>: CR, tab and newline become spaces, byte for byte, so an offset in <out> is an offset in <in>
normalise() { tr '\r\t\n' '   ' < "$1" > "$2"; }
# offsets <normalised file> <out file>: writes the byte offset of every match whose edges are word boundaries; returns non-zero when grep ITSELF failed (a crashed grep prints no match, which would read
# as a clean file: grep exits 0 when it matched, 1 when it did not, and anything else is a failure)
offsets() {
  local nf="$1" out="$2" S rec off text prev next rc
  : > "$out"
  grep -a -o -b -i -E "$ERE" "$nf" > "$T/g.out"; rc=$?
  [ "$rc" -le 1 ] || return "$rc"
  S="$(<"$nf")"
  while IFS= read -r rec; do
    off="${rec%%:*}"; text="${rec#*:}"
    if [ "$off" -gt 0 ]; then prev="${S:$((off-1)):1}"; case "$prev" in [A-Za-z0-9_]) continue;; esac; fi
    next="${S:$((off+${#text})):1}"; case "$next" in [A-Za-z0-9_]) continue;; esac
    printf '%s\n' "$off" >> "$out"
  done < "$T/g.out"
  return 0
}
# canary <want> <text>: the matcher must find exactly <want> matches in <text>. All the canaries go through ONE pass of the real matcher (each is followed by a separator that no match can span),
# so a run costs one grep, and each match is credited to the canary whose byte range holds its offset.
CANARY_WANT=(); CANARY_TEXT=()
canary() { CANARY_WANT+=("$1"); CANARY_TEXT+=("$2"); }
run_canaries() {
  local i pos=0 off got rc bad=0; local -a start=() count=()
  : > "$T/c.in"
  for i in "${!CANARY_TEXT[@]}"; do
    start[i]=$pos; count[i]=0
    printf '%s\n@@@\n' "${CANARY_TEXT[i]}" >> "$T/c.in"; pos=$((pos + ${#CANARY_TEXT[i]} + 5))
  done
  normalise "$T/c.in" "$T/c.n"
  offsets "$T/c.n" "$T/c.off"; rc=$?
  if [ "$rc" -ne 0 ]; then echo "  canary: grep itself failed (exit status $rc)"; return 99; fi
  while IFS= read -r off; do
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
canary 1 'SELECT id FROM public.users WHERE id = $1'
canary 1 'select id from public.users'
canary 1 "$(printf 'SELECT id\n\tFROM\n\t\tpublic.users u')"
canary 1 'SELECT 1 FROM ONLY public.users'
canary 1 'SELECT 1 FROM "public"."users"'
canary 1 'q := "SELECT 1 FROM \"public\".\"users\""'
canary 2 'SELECT 1 FROM public.users JOIN public.users b ON true'
canary 0 'INSERT INTO public.users (id) VALUES ($1)'
canary 0 'UPDATE public.users SET is_active = false'
canary 0 'SELECT 1 FROM platform.listable_users'
canary 0 'SELECT 1 FROM public.users_archive'
canary 0 'SELECT 1 FROM "public"."users_archive"'
canary 0 'SELECT 1 FROM public.users2'
canary 0 'SELECT 1 xfrom public.users'
run_canaries; canary_bad=$?
if [ "$canary_bad" -gt 0 ]; then
  echo "check-user-lists: RED — the matcher failed its own canary ($canary_bad case(s) above): the instrument cannot be trusted (is grep -o -b -E available?)"
  exit 1
fi

# ---- the subject ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-user-lists: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }
EXC="${EXC_IN:-$ROOT/tests/user-list-exceptions.txt}"
EXCNAME="${EXC#"$ROOT"/}"

: > "$T/go.abs"; : > "$T/sql.abs"
for d in internal cmd; do
  [ -d "$ROOT/$d" ] || continue
  find "$ROOT/$d" -type f -name '*.go' ! -name '*_test.go' >> "$T/go.abs"
done
if [ -d "$ROOT/db" ]; then find "$ROOT/db" -path "$ROOT/db/migrations" -prune -o -type f -name '*.sql' -print >> "$T/sql.abs"; fi
: > "$T/files"
for kind in go sql; do
  sort "$T/$kind.abs" | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/$kind.rel"
  cat "$T/$kind.rel" >> "$T/files"
done
ngo=$(wc -l < "$T/go.rel" | tr -d ' '); nsql=$(wc -l < "$T/sql.rel" | tr -d ' ')

echo "check-user-lists: scope: $SCOPE (root: $ROOT)"
echo "check-user-lists: scanned $ngo Go file(s) and $nsql SQL file(s)"
if [ "$ngo" -lt 1 ]; then
  echo "check-user-lists: RED — found 0 Go files under internal/ and cmd/ in $ROOT: the instrument matched nothing (a wrong --root, or an empty checkout)"
  exit 1
fi
if [ ! -f "$EXC" ]; then
  echo "check-user-lists: RED — the exceptions file $EXC does not exist (the gate cannot say what it excuses; an empty register is a file with only comments)"
  exit 1
fi

# ---- the hits: LINES[file] = "12 40 " for every match, in file order ----------------------------------------------------------------------------------------------------------------
declare -A LINES COUNT EXC_REASON EXC_LINE
nbad_files=0
while IFS= read -r rel; do
  f="$ROOT/$rel"
  # every match contains the word "users": a file without it (in any case) cannot hold one, and is skipped without spawning a process (checked in bash itself, so no external tool can fail silently here)
  content="$(<"$f")"; case "${content,,}" in *users*) ;; *) continue;; esac
  normalise "$f" "$T/n"
  offsets "$T/n" "$T/off"; rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "check-user-lists: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue
  fi
  while IFS= read -r off; do
    [ -n "$off" ] || continue
    ln=$(( $(head -c "$off" "$f" | wc -l) + 1 ))
    LINES[$rel]="${LINES[$rel]:-}$ln "; COUNT[$rel]=$(( ${COUNT[$rel]:-0} + 1 ))
  done < "$T/off"
done < "$T/files"

# ---- the exceptions ---------------------------------------------------------------------------------------------------------------------------------------------------------------------
fail=$nbad_files; nfind=0; nexc_bad=0; nexcused=0
exc_bad() { echo "$EXCNAME:$1: $2"; fail=$((fail+1)); nexc_bad=$((nexc_bad+1)); }
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
  if [ -z "$reason" ]; then exc_bad "$n" "exception for $path has a blank reason: say why this file may select from public.users (an exception without a reason is a hole)"; continue; fi
  if [ -n "${EXC_REASON[$path]+x}" ]; then exc_bad "$n" "duplicate exception for $path (first on line ${EXC_LINE[$path]})"; continue; fi
  EXC_REASON[$path]="$reason"; EXC_LINE[$path]="$n"; exc_order+=("$path")
done < "$EXC"

for path in ${exc_order[@]+"${exc_order[@]}"}; do
  if grep -q -F -x -e "$path" "$T/files"; then
    if [ "${COUNT[$path]:-0}" -lt 1 ]; then exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the file contains no SELECT from public.users — remove the entry (a register that rots hides the next miss)"; fi
  elif [ -e "$ROOT/$path" ]; then
    exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the gate does not scan that file ($SCOPE) — remove the entry"
  else
    exc_bad "${EXC_LINE[$path]}" "stale exception for $path: the file does not exist — remove the entry (a register that rots hides the next miss)"
  fi
done

# ---- the verdict ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
while IFS= read -r rel; do
  [ "${COUNT[$rel]:-0}" -ge 1 ] || continue
  if [ -n "${EXC_REASON[$rel]+x}" ]; then
    nexcused=$((nexcused+${COUNT[$rel]})); echo "  excused: $rel (${COUNT[$rel]} match(es), exception on line ${EXC_LINE[$rel]}): ${EXC_REASON[$rel]}"; continue
  fi
  for ln in ${LINES[$rel]}; do echo "$rel:$ln: $FIND_TAIL"; fail=$((fail+1)); nfind=$((nfind+1)); done
done < "$T/files"

echo "check-user-lists: $nfind finding(s), $nexcused match(es) excused by exception, $nexc_bad exception problem(s), $nbad_files unreadable file(s), over $ngo Go and $nsql SQL file(s)"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-user-lists: RED — $fail problem(s): $nfind SELECT(s) from public.users outside platform.listable_users, $nexc_bad exception problem(s), $nbad_files file(s) the matcher failed on"; exit 1; }
echo "check-user-lists: OK"
