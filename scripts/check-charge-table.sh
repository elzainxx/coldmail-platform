#!/usr/bin/env bash
# check-charge-table.sh [--root DIR] — BLOCKING gate: the charge table in the spec and the table the code embeds are one table, and every unit it charges is a unit the database seeds.
#
# WHY IT EXISTS. What a settled outcome costs the customer is DATA: internal/charge/rules.psv (embedded and parsed by the Go package, first match wins). The same table is printed in
# Docs/phase-2-spec.md between two marker lines so that a person can read and rule on it; a table in prose that can drift from the table in code is a table nobody read. This gate fails
# when the two differ by one byte, and when the table names a unit platform.unit_costs does not seed (the database would refuse the ledger row at the worst moment: settlement).
#
# WHAT IT CHECKS (every input line has its CR stripped; rules.psv is the exception: a CR in it is itself red, because the file is embedded as it is):
#   1. the spec holds exactly one line `<!-- charge-table:begin -->` and exactly one `<!-- charge-table:end -->`, in that order, with exactly one fenced block between them and nothing else
#      (blank lines apart); the block is not empty;
#   2. internal/charge/rules.psv equals that fenced block byte for byte (header line included). RED prints the first differing line number and both lines;
#   3. rules.psv has at least 30 data rows, each with 10 columns (id|module|state|reason|cache|skip|origin|decision|unit|note) and a unique id, an origin cell that is one of * own remote
#      undetermined none (the value M1's signals.failure_origin can take, or `none` for "absent"), a known decision (charge, free, notfinal, unmapped), and for a CHARGE row a unit that is
#      one of the seeded units AND belongs to the row's module; a non-charge row carries no unit. The seeded units are read from db/migrations/platform_0002_registry_and_money.up.sql, the
#      `INSERT INTO platform.unit_costs` list (at least five must be found: "found 0 units" is the instrument matching nothing).
# FLOORS (a gate whose subject is empty is red, classes 8 and 18): a missing or empty spec, rules.psv or migration, zero markers, an empty block, fewer than 30 rows, fewer than five units.
#
# Output: `check-charge-table: N rows, M charge rows, K unmapped rows`, one line per problem, and a `NOT CHECKED:` line. Exit status: 0 green, 1 red, 2 usage error.
# Paired control: scripts/check-charge-table-selftest.sh (every reject case asserts the reason it fails for).
#
# NOT CHECKED (also printed on every run): that the table is RIGHT (what each module outcome should cost the customer is derived from module code and read by a person: the reports in
# Docs/history/p2-research/); that the Go matcher implements the rules (the unit tests in internal/charge do, in the `go` workflow, and TestRulesFileMatchesSpec repeats check 2 there).
set -u
export LC_ALL=C

usage() { echo "usage: check-charge-table.sh [--root DIR]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)   [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*) ROOT_IN="${1#*=}"; shift;;
    -h|--help) usage;;
    *) echo "check-charge-table: unknown argument '$1'" >&2; usage;;
  esac
done
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-charge-table: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }

SPEC_REL="Docs/phase-2-spec.md"; PSV_REL="internal/charge/rules.psv"; SQL_REL="db/migrations/platform_0002_registry_and_money.up.sql"
SPEC="$ROOT/$SPEC_REL"; PSV="$ROOT/$PSV_REL"; SQL="$ROOT/$SQL_REL"
BEGIN='<!-- charge-table:begin -->'; END='<!-- charge-table:end -->'
ROW_FLOOR=30; UNIT_FLOOR=5
HEADER='id|module|state|reason|cache|skip|origin|decision|unit|note'
NOT_CHECKED="NOT CHECKED: that the table is right (what each module outcome should cost the customer is derived from module code and read by a person: Docs/history/p2-research/); that the Go matcher implements the rules (the unit tests in internal/charge do, in the go workflow)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

fail=0
bad() { echo "  RED: $*"; fail=$((fail+1)); }
need() { # need <path> <rel> <what it is> — a missing or empty subject is red at once
  if [ ! -f "$1" ]; then echo "check-charge-table: RED — $2 does not exist: $3"; exit 1; fi
  if [ ! -s "$1" ]; then echo "check-charge-table: RED — $2 is empty: the gate matched nothing ($3)"; exit 1; fi
}
need "$SPEC" "$SPEC_REL" "the spec holds the charge table the owner rules on"
need "$PSV"  "$PSV_REL"  "the table the code embeds"
need "$SQL"  "$SQL_REL"  "the migration that seeds the five units"

tr -d '\r' < "$SPEC" > "$T/spec"
if [ ! -s "$T/spec" ]; then echo "check-charge-table: RED — $SPEC_REL has no content after the CR strip: the gate matched nothing"; exit 1; fi

# ---- 1. the markers and the one fenced block between them ---------------------------------------------------------------------------------------------------------------------------
lines_of() { awk -v m="$1" '$0 == m { print NR }' "$T/spec"; }   # whole-line match only: prose that quotes a marker is not a marker
read -r -a bl <<< "$(lines_of "$BEGIN" | tr '\n' ' ')"
read -r -a el <<< "$(lines_of "$END" | tr '\n' ' ')"
marker_ok=1
mention() { # mention <marker> <exact count>: how many lines contain the marker text without being exactly the marker line
  local all; all=$(grep -c -F -- "$1" "$T/spec" || true); echo $((all - $2))
}
if [ "${#bl[@]}" -ne 1 ]; then
  extra=$(mention "$BEGIN" "${#bl[@]}")
  bad "found ${#bl[@]} begin marker(s) in $SPEC_REL, exactly one line $BEGIN is required${bl[*]:+ (lines ${bl[*]})}$([ "$extra" -gt 0 ] && echo "; $extra other line(s) contain the text but are not exactly the marker line")"
  marker_ok=0
fi
if [ "${#el[@]}" -ne 1 ]; then
  extra=$(mention "$END" "${#el[@]}")
  bad "found ${#el[@]} end marker(s) in $SPEC_REL, exactly one line $END is required${el[*]:+ (lines ${el[*]})}$([ "$extra" -gt 0 ] && echo "; $extra other line(s) contain the text but are not exactly the marker line")"
  marker_ok=0
fi
if [ "$marker_ok" -eq 1 ] && [ "${bl[0]}" -ge "${el[0]}" ]; then
  bad "the end marker (line ${el[0]}) is not after the begin marker (line ${bl[0]}) in $SPEC_REL: the markers are out of order"
  marker_ok=0
fi
have_block=0
if [ "$marker_ok" -eq 1 ]; then
  # the lines strictly between the markers (awk, not `sed -n 'a,bp'`: sed prints line a when b < a, i.e. when the two markers or the two fences are adjacent)
  awk -v a="${bl[0]}" -v b="${el[0]}" 'NR > a && NR < b' "$T/spec" > "$T/region"
  read -r -a fl <<< "$(grep -n '^```' "$T/region" | cut -d: -f1 | tr '\n' ' ')"
  nf=${#fl[@]}
  if [ "$nf" -eq 0 ]; then
    bad "no fenced block between the markers in $SPEC_REL: the table must sit in one \`\`\` fence"
  elif [ $((nf % 2)) -eq 1 ]; then
    bad "an unclosed fenced block between the markers in $SPEC_REL ($nf fence line(s)): the fence is opened and not closed"
  elif [ "$nf" -gt 2 ]; then
    bad "more than one fenced block between the markers in $SPEC_REL ($((nf / 2)) blocks, $nf fence lines): exactly one is required"
  else
    outside=$(awk -v a="${fl[0]}" -v b="${fl[1]}" '(NR < a || NR > b) && $0 ~ /[^[:space:]]/ { n++ } END { print n + 0 }' "$T/region")
    closing=$(sed -n "${fl[1]}p" "$T/region")
    if [ "$outside" -gt 0 ]; then
      bad "$outside non-blank line(s) between the markers in $SPEC_REL outside the fenced block: the markers must hold exactly one fenced block and nothing else"
    elif [ "$closing" != '```' ]; then
      bad "the closing fence between the markers in $SPEC_REL is not exactly three backticks: '$closing'"
    else
      awk -v a="${fl[0]}" -v b="${fl[1]}" 'NR > a && NR < b' "$T/region" > "$T/block"
      if [ ! -s "$T/block" ]; then bad "the fenced block between the markers in $SPEC_REL is empty: the gate matched nothing"; else have_block=1; fi
    fi
  fi
fi

# ---- 2. the spec block equals rules.psv, byte for byte ---------------------------------------------------------------------------------------------------------------------------------
first_diff() { # first_diff <spec block> <rules.psv> — prints "N<TAB>spec line<TAB>rules line" for the first differing line, nothing when every line is equal
  awk -v A="$1" -v B="$2" 'BEGIN {
    while ((getline l < A) > 0) { na++; a[na] = l }
    while ((getline l < B) > 0) { nb++; b[nb] = l }
    max = (na > nb) ? na : nb
    for (i = 1; i <= max; i++) {
      if (i > na) { printf "%d\t<the spec block ends before this line>\t%s\n", i, b[i]; exit }
      if (i > nb) { printf "%d\t%s\t<rules.psv ends before this line>\n", i, a[i]; exit }
      if (a[i] != b[i]) { printf "%d\t%s\t%s\n", i, a[i], b[i]; exit }
    }
  }'
}
if [ "$(tr -cd '\r' < "$PSV" | wc -c | tr -d ' ')" -gt 0 ]; then   # counted with tr, not grep "$(printf '\r')": a Windows bash strips the CR from $(...) (class 20)
  bad "$PSV_REL contains a carriage return: the file must be LF (it is embedded as it is, and the spec block is compared after a CR strip)"
fi
if [ "$have_block" -eq 1 ]; then
  if cmp -s "$T/block" "$PSV"; then
    echo "check-charge-table: the fenced block in $SPEC_REL equals $PSV_REL byte for byte ($(wc -l < "$T/block" | tr -d ' ') lines, header included)"
  else
    cp "$PSV" "$T/psv"
    d="$(first_diff "$T/block" "$T/psv")"
    if [ -n "$d" ]; then
      n="${d%%$'\t'*}"; rest="${d#*$'\t'}"; sl="${rest%%$'\t'*}"; rl="${rest#*$'\t'}"
      bad "$PSV_REL differs from the fenced block in $SPEC_REL: first difference at line $n"
      echo "       spec block: $sl"
      echo "       rules.psv : $rl"
    else
      bad "$PSV_REL differs from the fenced block in $SPEC_REL in its final newline only (every line is equal): the file must end with exactly one newline, as the block does"
    fi
  fi
fi

# ---- 3. the seeded units, then every row of rules.psv ----------------------------------------------------------------------------------------------------------------------------------
# the units are the first two quoted strings of each row of the INSERT INTO platform.unit_costs list: ('m1_own', 'm1', 'description', 1),
tr -d '\r' < "$SQL" | awk '
  /INSERT[[:space:]]+INTO[[:space:]]+platform\.unit_costs[[:space:]]*\(/ { on = 1; next }
  on { print; if ($0 ~ /;[[:space:]]*(--.*)?$/) on = 0 }
' | sed -n -E "s/^[[:space:]]*\('([a-z0-9_]+)'[[:space:]]*,[[:space:]]*'([a-z0-9_]+)'.*/\1|\2/p" > "$T/units"
nunits=$(wc -l < "$T/units" | tr -d ' ')
unit_list="$(cut -d'|' -f1 "$T/units" | tr '\n' ' ' | sed 's/ $//')"
if [ "$nunits" -lt "$UNIT_FLOOR" ]; then
  bad "found $nunits seeded unit(s) in the INSERT INTO platform.unit_costs list of $SQL_REL, fewer than $UNIT_FLOOR: the instrument matched too little (the migration changed shape, or the list moved)"
fi

nrows=0; ncharge=0; nunmapped=0
while IFS= read -r l; do
  case "$l" in
    P:*) bad "${l#P:}";;
    N:*) IFS=: read -r _ nrows ncharge nunmapped <<< "$l";;
  esac
done < <(tr -d '\r' < "$PSV" | awk -F'|' -v units="$T/units" -v ulist="$unit_list" -v header="$HEADER" -v psv="$PSV_REL" -v sql="$SQL_REL" '
  BEGIN { while ((getline l < units) > 0) { split(l, p, "|"); owner[p[1]] = p[2] } }
  NR == 1 { if ($0 != header) print "P:" psv " line 1 is not the header " header; next }
  {
    rows++
    if (NF != 10) { print "P:row " $1 " (line " NR " of " psv ") has " NF " columns, want 10"; next }
    id = $1; mod = $2; org = $7; dec = $8; unit = $9
    if (id in first) print "P:row " id " (line " NR " of " psv "): duplicate id (first on line " first[id] ")"; else first[id] = NR
    if (org != "*" && org != "own" && org != "remote" && org != "undetermined" && org != "none") print "P:row " id " (line " NR " of " psv "): the origin cell \047" org "\047 must be *, own, remote, undetermined or none"
    if (dec == "charge") {
      charge++
      if (unit == "") print "P:row " id " (line " NR " of " psv "): a charge row has no unit"
      else if (!(unit in owner)) print "P:row " id " (line " NR " of " psv "): charges unit \047" unit "\047, which is not one of the seeded units (" ulist ") of " sql
      else if (owner[unit] != mod) print "P:row " id " (line " NR " of " psv "): charges unit \047" unit "\047, which belongs to module " owner[unit] ", not " mod
    } else if (dec == "free" || dec == "notfinal" || dec == "unmapped") {
      if (dec == "unmapped") unmapped++
      if (unit != "") print "P:row " id " (line " NR " of " psv "): has a unit (\047" unit "\047) but is not a charge row"
    } else print "P:row " id " (line " NR " of " psv "): unknown decision \047" dec "\047 (want charge, free, notfinal or unmapped)"
  }
  END { print "N:" rows + 0 ":" charge + 0 ":" unmapped + 0 }
')
if [ "$nrows" -lt "$ROW_FLOOR" ]; then
  bad "$PSV_REL has $nrows row(s), fewer than the floor of $ROW_FLOOR: the table is incomplete (or the gate read the wrong file)"
fi

echo "check-charge-table: $nrows rows, $ncharge charge rows, $nunmapped unmapped rows"
echo "check-charge-table: $nunits seeded unit(s) read from $SQL_REL: ${unit_list:-none}"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-charge-table: RED — $fail problem(s)"; exit 1; }
echo "check-charge-table: OK"
