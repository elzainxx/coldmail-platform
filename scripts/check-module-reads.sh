#!/usr/bin/env bash
# check-module-reads.sh --modules DIR --statements FILE — the DRIFT GATE for what modules 2 and 3 do to the platform's two tables, public.users and public.job_items.
#
# WHY IT EXISTS. The platform owns those two tables and must shape them for code it does not control (Docs/phase-1-spec.md, "Inventory"; findings F19-F22). db/module-reads.txt declares the shape
# modules 2 and 3 require; db/module-statements.txt lists the statements behind it. Neither means anything if the pinned module code can start reading another column, or add another statement,
# without anyone noticing. This gate holds the declaration against the SOURCE of every module it names (the module checkouts the harness builds from tools/modules.lock):
#   1. every declared statement still appears, as written, in its declared file (whitespace normalised to single spaces; lines that are only a `//` comment are dropped first, so a statement that has
#      been commented out is gone). Red: `DRIFT: <label> no longer appears in <file>`. A statement declared N times (the same text at N call sites) must still appear N times.
#   2. every OTHER reference to public.users / public.job_items in the module's non-test Go source is declared: a file that references them and is not declared is red
#      (`DRIFT: <file> references public.users/public.job_items but is not declared`), and so is a new reference inside a declared file that no declared statement covers (the declared statements are
#      masked out of the file, and what still names the tables is the drift). An unqualified `FROM users` / `JOIN job_items` is red too: it would resolve through search_path, and the gate cannot tie it to public.
#   Non-test Go source = every *.go except *_test.go, and except the module root's tests/, scripts/, Docs/ and .git/ directories (and any node_modules).
# FLOORS (a gate whose subject is empty is red): the statements file must exist, declare at least one module, and declare at least one statement per module; a module directory must exist and hold at
# least one non-test Go file.
#
# FORMAT of the statements file: see the header of db/module-statements.txt (`module-dir|<id>|<dir>` lines, then `<id>|<role>|<label>|<file>|<SQL>` lines).
# NOT CHECKED (printed on every run): SQL in the modules' migrations (their foreign keys and grants are exercised by the harness against a real database); statements built dynamically (string
# concatenation or fmt.Sprintf around the table name); a table reached through a helper that hides its name; quoted identifiers ("public"."users"); non-Go source (module 2's and 3's worker is
# Node and has no database driver); trailing `// ...` comments after code on the same line (they count as code: the gate fails closed).
set -u
export LC_ALL=C
MODS=""; STM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --modules)    MODS="${2:?--modules needs a directory}"; shift 2;;
    --statements) STM="${2:?--statements needs a file}"; shift 2;;
    *) echo "check-module-reads: unknown argument '$1'" >&2; exit 2;;
  esac
done
[ -n "$MODS" ] && [ -n "$STM" ] || { echo "usage: check-module-reads.sh --modules DIR --statements FILE" >&2; exit 2; }
[ -d "$MODS" ] || { echo "check-module-reads: RED — the modules directory $MODS does not exist"; exit 1; }
[ -f "$STM" ]  || { echo "check-module-reads: RED — the statements file $STM does not exist"; exit 1; }
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/n"

fail=0; bad() { echo "  $*"; fail=$((fail+1)); }
trim() { printf '%s' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'; }
# the normal form both sides are compared in: CR removed, lines that are only a // comment dropped, tabs and newlines turned into spaces, runs of spaces squeezed to one
norm_stream() { tr -d '\r' | grep -v -E '^[[:space:]]*//' | tr '\t\n' '  ' | tr -s ' '; }
key_of() { local k="${1//\//@}"; printf '%s' "${k// /#}"; }

# ---- the declaration -> $T/dirs (id|dir) and $T/stm (id|role|label|file|sql) ----
: > "$T/dirs"; : > "$T/stm"
ln=0
while IFS= read -r raw || [ -n "$raw" ]; do
  ln=$((ln+1)); line="$(printf '%s' "$raw" | tr -d '\r')"
  case "$line" in ''|'#'*) continue;; esac
  [ -n "$(trim "$line")" ] || continue
  IFS='|' read -r f1 f2 f3 f4 f5 <<EOF
$line
EOF
  f1="$(trim "$f1")"; f2="$(trim "$f2")"; f3="$(trim "$f3")"; f4="$(trim "$f4")"
  if [ "$f1" = "module-dir" ]; then
    if [ -z "$f2" ] || [ -z "$f3" ] || [ -n "$f4" ]; then bad "malformed line $ln in $STM: a module-dir line is 'module-dir|<id>|<directory>'"; continue; fi
    printf '%s|%s\n' "$f2" "$f3" >> "$T/dirs"; continue
  fi
  sql="$(printf '%s' "$f5" | tr '\t' ' ' | tr -s ' ' | sed -e 's/^ //' -e 's/ $//')"
  if [ -z "$f1" ] || [ -z "$f2" ] || [ -z "$f3" ] || [ -z "$f4" ] || [ -z "$sql" ]; then bad "malformed line $ln in $STM: a statement line is '<module>|<role>|<label>|<file>|<SQL>' with all five fields filled"; continue; fi
  case "$f4" in
    /*|*..*)   bad "malformed line $ln in $STM: the file '$f4' must be relative to the module root and must not contain '..'"; continue;;
    *_test.go) bad "malformed line $ln in $STM: '$f4' is a test file; test statements are not declared here"; continue;;
    *.go) ;;
    *)         bad "malformed line $ln in $STM: '$f4' is not a Go file"; continue;;
  esac
  case "$f4" in tests/*|scripts/*|Docs/*|.git/*) bad "malformed line $ln in $STM: '$f4' is in a directory the gate does not read (tests/, scripts/, Docs/)"; continue;; esac
  if ! awk -F'|' -v m="$f1" '$1==m {f=1} END{exit f?0:1}' "$T/dirs"; then
    bad "malformed line $ln in $STM: module '$f1' has no module-dir line before it"; continue
  fi
  if awk -F'|' -v m="$f1" -v l="$f3" '$1==m && $3==l {f=1} END{exit f?0:1}' "$T/stm"; then
    bad "malformed line $ln in $STM: label $f3 is declared twice for module $f1"; continue
  fi
  printf '%s|%s|%s|%s|%s\n' "$f1" "$f2" "$f3" "$f4" "$sql" >> "$T/stm"
done < "$STM"

ndirs=$(wc -l < "$T/dirs" | tr -d ' ')
[ "$ndirs" -ge 1 ] || bad "$STM declares no module (no module-dir line): the instrument matched nothing"

total_stmts=0; total_files=0
while IFS='|' read -r id dir; do
  [ -n "$id" ] || continue
  mroot="$MODS/$dir"
  if [ ! -d "$mroot" ]; then bad "module $id: the directory $mroot does not exist"; continue; fi
  find "$mroot" -type d \( -path "$mroot/.git" -o -path "$mroot/tests" -o -path "$mroot/scripts" -o -path "$mroot/Docs" -o -name node_modules \) -prune -o -type f -name '*.go' ! -name '*_test.go' -print | sort > "$T/go.$id"
  ngo=$(wc -l < "$T/go.$id" | tr -d ' ')
  if [ "$ngo" -lt 1 ]; then bad "module $id: found 0 Go files under $dir: the instrument matched nothing (a wrong directory, or an empty checkout)"; continue; fi
  total_files=$((total_files+ngo))
  awk -F'|' -v m="$id" '$1==m' "$T/stm" > "$T/s.$id"
  nst=$(wc -l < "$T/s.$id" | tr -d ' ')
  if [ "$nst" -lt 1 ]; then bad "module $id: 0 declared statements (floor is 1): nothing here says what the module does to the platform tables"; continue; fi
  total_stmts=$((total_stmts+nst))

  # the files worth reading: every non-test Go file that mentions either table at all, plus every declared file (a declared file that no longer mentions them is drift, found below)
  tr '\n' '\0' < "$T/go.$id" | xargs -0 -r grep -l -i -E 'users|job_items' -- 2>/dev/null | sort > "$T/cand.$id" || true
  awk -F'|' '{print $4}' "$T/s.$id" | while IFS= read -r rel; do printf '%s\n' "$mroot/$rel"; done >> "$T/cand.$id"
  sort -u "$T/cand.$id" -o "$T/cand.$id"
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    rel="${f#"$mroot"/}"
    norm_stream < "$f" > "$T/n/$id.$(key_of "$rel")"
  done < "$T/cand.$id"

  # 1. every declared statement still appears (as many times as it is declared)
  : > "$T/seen.$id"
  while IFS='|' read -r m role label rel sql; do
    nf="$T/n/$id.$(key_of "$rel")"
    if [ ! -f "$mroot/$rel" ]; then bad "DRIFT: $label no longer appears in $rel (module $id: the file does not exist)"; continue; fi
    content="$(cat "$nf")"
    without="${content//"$sql"/}"
    occ=$(( (${#content} - ${#without}) / ${#sql} ))
    mult=$(awk -F'|' -v f="$rel" -v s="$sql" '$4==f && $5==s' "$T/s.$id" | wc -l | tr -d ' ')
    nth=$(grep -c -F -x "$rel|$sql" "$T/seen.$id" || true); nth=$((nth+1))
    printf '%s|%s\n' "$rel" "$sql" >> "$T/seen.$id"
    if [ "$occ" -eq 0 ]; then bad "DRIFT: $label no longer appears in $rel (module $id): the statement text is gone or changed"
    elif [ "$nth" -gt "$occ" ]; then bad "DRIFT: $label no longer appears in $rel (module $id) as many times as it is declared (found $occ, declared $mult)"; fi
  done < "$T/s.$id"

  # 2. nothing else names the tables: mask the declared statements out of each candidate file and look at what is left
  nref=0
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    rel="${f#"$mroot"/}"
    content="$(cat "$T/n/$id.$(key_of "$rel")")"
    declared=0
    while IFS= read -r sql; do
      declared=1
      content="${content//"$sql"/ @ }"
    done < <(awk -F'|' -v f="$rel" '$4==f {print $5}' "$T/s.$id" | sort -u)
    left=$(printf '%s' "$content" | grep -o -E 'public\.(users|job_items)([^A-Za-z0-9_]|$)' | wc -l | tr -d ' ')
    unq=$(printf '%s' "$content" | grep -o -i -E '(from|join|into|update|references)[[:space:]]+(users|job_items)([^A-Za-z0-9_.]|$)' | wc -l | tr -d ' ')
    if [ "$left" -gt 0 ]; then
      nref=$((nref+1))
      if [ "$declared" -eq 0 ]; then bad "DRIFT: $rel references public.users/public.job_items but is not declared (module $id: $left reference(s); add its statements to $STM, and their columns to db/module-reads.txt)"
      else bad "DRIFT: $rel has $left reference(s) to public.users/public.job_items outside its declared statements (module $id: a new or changed statement; declare it)"; fi
    fi
    if [ "$unq" -gt 0 ]; then bad "DRIFT: $rel names users/job_items without a schema ($unq time(s), module $id): it resolves through search_path, and the gate cannot tie it to public"; fi
  done < "$T/cand.$id"
  nfiles=$(awk -F'|' '{print $4}' "$T/s.$id" | sort -u | wc -l | tr -d ' ')
  echo "  module $id ($dir): $nst declared statement(s) in $nfiles file(s); $ngo non-test Go file(s) scanned; $nref file(s) with an undeclared reference"
done < "$T/dirs"

echo "check-module-reads: $ndirs module(s), $total_stmts declared statement(s), $total_files non-test Go file(s) scanned"
echo "NOT CHECKED: SQL in the modules' migrations (their foreign keys and grants are exercised by the harness against a real database); statements built dynamically (string concatenation / fmt.Sprintf around a table name); a table reached through a helper that hides its name; quoted identifiers; non-Go source (the workers are Node and hold no database driver); a trailing // comment after code counts as code (fails closed)"
[ "$fail" -eq 0 ] || { echo "check-module-reads: RED — $fail problem(s)"; exit 1; }
echo "check-module-reads: OK"
