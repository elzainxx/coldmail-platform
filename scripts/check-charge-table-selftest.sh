#!/usr/bin/env bash
# check-charge-table-selftest.sh — proves scripts/check-charge-table.sh is green on a right tree and goes red, FOR THE REASON IT NAMES, on every way the charge table can be wrong.
# Every case builds a throwaway tree (a spec with the marked, fenced table; rules.psv; the migration that seeds the units), runs the real gate on it and asserts the exit code AND fragments of
# the printed reason (class 10: a reject case that fails for another reason is no proof). A hard minimum number of cases, and three controls that prove THIS file can fail: a gate that
# always passes, a gate that always fails, and a gate that fails for the wrong reason must each make a case misbehave.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-charge-table.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
MIN_CASES=30
n=0; bad=0
t() { n=$((n+1)); }
HEADER='id|module|state|reason|cache|skip|decision|unit|note'
BEGIN='<!-- charge-table:begin -->'; END='<!-- charge-table:end -->'
PSV_REL="internal/charge/rules.psv"; SPEC_REL="Docs/phase-2-spec.md"; SQL_REL="db/migrations/platform_0002_registry_and_money.up.sql"

write_spec() { # write_spec DIR — the spec, holding the table that is in DIR's rules.psv
  { printf '# Phase 2 spec\n\nprose before the table\n\n'; printf '%s\n' "$BEGIN"; printf '%s\n' '```'; cat "$1/$PSV_REL"; printf '%s\n' '```'; printf '%s\n' "$END"; printf '\nprose after the table\n'; } > "$1/$SPEC_REL"
}
write_sql() { # write_sql DIR — the migration, with a decoy insert before and after the unit list
  cat > "$1/$SQL_REL" <<'SQL'
CREATE TABLE platform.unit_costs (unit text PRIMARY KEY);
INSERT INTO platform.unit_cost_history (unit) VALUES ('m1_decoy');
INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES
    ('m1_own',      'm1', 'own infrastructure', 1),
    ('m1_cache',    'm1', 'cache (full price: the answer is the value)', 1),
    ('m1_vendor',   'm1', 'vendor', 2),
    ('m2_accepted', 'm2', 'accepted', 1),
    ('m2_bounced',  'm2', 'bounced: the user''s list', 1);
INSERT INTO platform.unit_costs_other (unit, module_id) VALUES
    ('m1_decoy2',   'm1');
SQL
}
# good_tree DIR: a tree the gate accepts: 32 rows, 22 charge rows, 5 unmapped rows (M1-01 is line 2 of the table, M1-04 is line 5)
good_tree() {
  d="$1"; rm -rf "$d"; mkdir -p "$d/Docs" "$d/internal/charge" "$d/db/migrations"
  {
    echo "$HEADER"
    echo 'M1-01|m1|*|*|yes|*|charge|m1_cache|n'
    echo 'M1-02|m1|*|external_verified|no|*|charge|m1_vendor|n'
    for i in $(seq 3 20); do printf 'M1-%02d|m1|valid|r%02d|no|*|charge|m1_own|n\n' "$i" "$i"; done
    for i in 21 22 23; do printf 'M1-%02d|m1|unknown|e%02d|no|*|unmapped||n\n' "$i" "$i"; done
    echo 'M1-99|m1|*|*|*|*|unmapped||n'
    echo 'M2-01|m2|sent|*|*|*|charge|m2_accepted|n'
    echo 'M2-02|m2|bounced|*|*|*|charge|m2_bounced|n'
    echo 'M2-03|m2|failed|*|*|*|free||n'
    echo 'M2-04|m2|queued|*|*|*|notfinal||n'
    echo 'M2-99|m2|*|*|*|*|unmapped||n'
    echo 'M3-01|m3|*|*|*|*|free||n'
    echo 'P-01|platform|cancelled|*|*|*|free||n'
    echo 'P-02|platform|expired|*|*|*|free||n'
  } > "$d/$PSV_REL"
  write_sql "$d"; write_spec "$d"
}
# edit_psv DIR SED-SCRIPT: change rules.psv and rebuild the spec from it, so the two stay equal and only the edit under test can make the gate red
edit_psv() { sed -i "$2" "$1/$PSV_REL"; write_spec "$1"; }

# expect NAME DIR WANT_RC FRAGMENT...   (each fragment must appear in the output)
expect() {
  t; name="$1"; d="$2"; want="$3"; shift 3
  out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; printf '%s\n' "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  for frag in "$@"; do
    if ! printf '%s\n' "$out" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc as expected but the output lacks '$frag'"; printf '%s\n' "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  done
  echo "  ok   $name"
}

D="$TMP/t"
# ---- green ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; expect "a right tree is green" "$D" 0 "check-charge-table: OK"
good_tree "$D"; expect "the green run states the counts" "$D" 0 "check-charge-table: 32 rows, 22 charge rows, 5 unmapped rows"
good_tree "$D"; expect "the green run says what it compared" "$D" 0 "equals internal/charge/rules.psv byte for byte (33 lines, header included)"
good_tree "$D"; expect "the units are read from the migration (the decoy inserts are not units)" "$D" 0 "5 seeded unit(s) read from $SQL_REL: m1_own m1_cache m1_vendor m2_accepted m2_bounced"
good_tree "$D"; expect "the green run prints what it did not check" "$D" 0 "NOT CHECKED: that the table is right" "that the Go matcher implements the rules"
good_tree "$D"; sed -i 's/$/\r/' "$D/$SPEC_REL"; expect "a spec with CRLF line endings is green (CR stripped)" "$D" 0 "check-charge-table: OK"
good_tree "$D"; printf 'The block is delimited by `%s` and `%s`.\n' "$BEGIN" "$END" >> "$D/$SPEC_REL"; expect "prose that quotes a marker mid-line is not a marker" "$D" 0 "check-charge-table: OK"

# ---- the subject is missing or empty ------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; rm "$D/$SPEC_REL"; expect "no spec" "$D" 1 "$SPEC_REL does not exist"
good_tree "$D"; : > "$D/$SPEC_REL"; expect "an empty spec (the subject is empty)" "$D" 1 "$SPEC_REL is empty: the gate matched nothing"
good_tree "$D"; rm "$D/$PSV_REL"; expect "no rules.psv" "$D" 1 "$PSV_REL does not exist"
good_tree "$D"; : > "$D/$PSV_REL"; expect "an empty rules.psv" "$D" 1 "$PSV_REL is empty: the gate matched nothing"
good_tree "$D"; rm "$D/$SQL_REL"; expect "no migration" "$D" 1 "$SQL_REL does not exist"
good_tree "$D"; rm -rf "$D"; expect "a root that is not a directory" "$D" 1 "is not a directory"

# ---- the markers and the fenced block --------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; sed -i '/charge-table:begin/d' "$D/$SPEC_REL"; expect "no begin marker" "$D" 1 "found 0 begin marker(s)"
good_tree "$D"; sed -i '/charge-table:end/d' "$D/$SPEC_REL"; expect "no end marker" "$D" 1 "found 0 end marker(s)"
good_tree "$D"; printf '%s\n' "$BEGIN" >> "$D/$SPEC_REL"; expect "a duplicated begin marker" "$D" 1 "found 2 begin marker(s)"
good_tree "$D"; printf '%s\n' "$END" >> "$D/$SPEC_REL"; expect "a duplicated end marker" "$D" 1 "found 2 end marker(s)"
good_tree "$D"; sed -i '/charge-table:/d' "$D/$SPEC_REL"; printf '%s\n%s\n' "$END" "$BEGIN" >> "$D/$SPEC_REL"; expect "the markers out of order" "$D" 1 "is not after the begin marker"
good_tree "$D"; sed -i "s|^$BEGIN\$|  $BEGIN|" "$D/$SPEC_REL"; expect "an indented begin marker is no marker (and the near miss is named)" "$D" 1 "found 0 begin marker(s)" "1 other line(s) contain the text but are not exactly the marker line"
good_tree "$D"; sed -i '/^```/d' "$D/$SPEC_REL"; expect "no fenced block between the markers" "$D" 1 "no fenced block between the markers"
good_tree "$D"; awk '/^```/ { k++; if (k == 2) next } { print }' "$D/$SPEC_REL" > "$D/s" && mv "$D/s" "$D/$SPEC_REL"; expect "an unclosed fenced block" "$D" 1 "an unclosed fenced block between the markers"
good_tree "$D"; awk '{ print } /^```/ { k++; if (k == 2) { print "```"; print "second block"; print "```" } }' "$D/$SPEC_REL" > "$D/s" && mv "$D/s" "$D/$SPEC_REL"; expect "two fenced blocks" "$D" 1 "more than one fenced block between the markers"
good_tree "$D"; sed -i "/charge-table:begin/a stray prose between the markers" "$D/$SPEC_REL"; expect "text between the markers outside the fence" "$D" 1 "1 non-blank line(s) between the markers"
good_tree "$D"; awk '/^```/ { k++; print; next } k == 1 { next } { print }' "$D/$SPEC_REL" > "$D/s" && mv "$D/s" "$D/$SPEC_REL"; expect "an empty fenced block" "$D" 1 "the fenced block between the markers in $SPEC_REL is empty"
good_tree "$D"; awk '/^```/ { k++; if (k == 2) { print "``` trailing"; next } } { print }' "$D/$SPEC_REL" > "$D/s" && mv "$D/s" "$D/$SPEC_REL"; expect "a closing fence with text after it" "$D" 1 "the closing fence between the markers in $SPEC_REL is not exactly three backticks"

# ---- the spec block and rules.psv differ -------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; sed -i 's/^M1-04|m1|valid|r04|no|\*|charge|m1_own|n$/M1-04|m1|valid|r04|no|*|free||n/' "$D/$PSV_REL"
expect "one row of rules.psv changed, the spec not: the first differing line is named, with both lines" "$D" 1 "$PSV_REL differs from the fenced block in $SPEC_REL: first difference at line 5" "spec block: M1-04|m1|valid|r04|no|*|charge|m1_own|n" "rules.psv : M1-04|m1|valid|r04|no|*|free||n"
good_tree "$D"; sed -i 's/^M1-05|m1|valid|r05|no|\*|charge|m1_own|n$/M1-05|m1|valid|r05|no|*|charge|m1_own|CHANGED/' "$D/$SPEC_REL"
expect "one row of the spec changed, rules.psv not" "$D" 1 "first difference at line 6" "spec block: M1-05|m1|valid|r05|no|*|charge|m1_own|CHANGED" "rules.psv : M1-05|m1|valid|r05|no|*|charge|m1_own|n"
good_tree "$D"; echo 'P-03|platform|quarantined|*|*|*|free||n' >> "$D/$PSV_REL"; expect "rules.psv has a row the spec does not" "$D" 1 "first difference at line 34" "<the spec block ends before this line>"
good_tree "$D"; sed -i '$d' "$D/$PSV_REL"; expect "rules.psv lacks the spec's last row" "$D" 1 "first difference at line 33" "<rules.psv ends before this line>"
good_tree "$D"; printf '%s' "$(cat "$D/$PSV_REL")" > "$D/p" && mv "$D/p" "$D/$PSV_REL"; expect "rules.psv without its final newline" "$D" 1 "in its final newline only"
good_tree "$D"; sed -i 's/$/\r/' "$D/$PSV_REL"; expect "rules.psv with CRLF line endings" "$D" 1 "$PSV_REL contains a carriage return"

# ---- the table itself ------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; edit_psv "$D" '/^M1-18|/d; /^M1-19|/d; /^M1-20|/d'; expect "fewer than 30 rows (29), spec and file equal" "$D" 1 "has 29 row(s), fewer than the floor of 30" "check-charge-table: 29 rows"
good_tree "$D"; edit_psv "$D" 's/^M1-05|m1|valid|r05|no|\*|charge|m1_own|n$/M1-05|m1|valid|r05|no|*|charge|m1_bogus|n/'; expect "a charge of a unit the database does not seed" "$D" 1 "row M1-05 (line 6 of $PSV_REL): charges unit 'm1_bogus', which is not one of the seeded units (m1_own m1_cache m1_vendor m2_accepted m2_bounced)"
good_tree "$D"; edit_psv "$D" 's/^M2-01|m2|sent|\*|\*|\*|charge|m2_accepted|n$/M2-01|m2|sent|*|*|*|charge|m1_own|n/'; expect "a seeded unit of the other module" "$D" 1 "row M2-01 (line 26 of $PSV_REL): charges unit 'm1_own', which belongs to module m1, not m2"
good_tree "$D"; edit_psv "$D" 's/^M1-05|m1|valid|r05|no|\*|charge|m1_own|n$/M1-05|m1|valid|r05|no|*|charge||n/'; expect "a charge row without a unit" "$D" 1 "row M1-05 (line 6 of $PSV_REL): a charge row has no unit"
good_tree "$D"; edit_psv "$D" 's/^M2-03|m2|failed|\*|\*|\*|free||n$/M2-03|m2|failed|*|*|*|free|m2_bounced|n/'; expect "a free row that carries a unit" "$D" 1 "row M2-03 (line 28 of $PSV_REL): has a unit ('m2_bounced') but is not a charge row"
good_tree "$D"; edit_psv "$D" 's/^M1-05|m1|valid|r05|no|\*|charge|m1_own|n$/M1-05|m1|valid|r05|no|*|charge|m1_own/'; expect "a row with 8 columns" "$D" 1 "row M1-05 (line 6 of $PSV_REL) has 8 columns, want 9"
good_tree "$D"; edit_psv "$D" 's/^M1-06|/M1-05|/'; expect "a duplicate id" "$D" 1 "row M1-05 (line 7 of $PSV_REL): duplicate id (first on line 6)"
good_tree "$D"; edit_psv "$D" 's/^M1-05|m1|valid|r05|no|\*|charge|m1_own|n$/M1-05|m1|valid|r05|no|*|refund|m1_own|n/'; expect "an unknown decision" "$D" 1 "row M1-05 (line 6 of $PSV_REL): unknown decision 'refund'"
good_tree "$D"; edit_psv "$D" '1s/|decision|/|verdict|/'; expect "a wrong header" "$D" 1 "$PSV_REL line 1 is not the header $HEADER"

# ---- the units come from the migration -----------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; sed -i 's/m1_own/m1_mine/' "$D/$SQL_REL"; expect "the migration renamed a unit: the table still naming it is red (the list is read, not remembered)" "$D" 1 "charges unit 'm1_own', which is not one of the seeded units (m1_mine m1_cache m1_vendor m2_accepted m2_bounced)"
good_tree "$D"; printf 'INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES\n    (%s, %s, %s, 1),\n    (%s, %s, %s, 1),\n    (%s, %s, %s, 2);\n' "'m1_own'" "'m1'" "'a'" "'m1_cache'" "'m1'" "'b'" "'m1_vendor'" "'m1'" "'c'" > "$D/$SQL_REL"
expect "fewer than five seeded units found" "$D" 1 "found 3 seeded unit(s) in the INSERT INTO platform.unit_costs list" "fewer than 5"
good_tree "$D"; printf 'CREATE TABLE platform.unit_costs (unit text PRIMARY KEY);\n' > "$D/$SQL_REL"; expect "no unit list in the migration (the instrument matched nothing)" "$D" 1 "found 0 seeded unit(s)"

# ---- usage -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
out="$(bash "$GATE" --bogus 2>&1)"; rc=$?; t
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi

# ---- the controls: this selftest must be able to fail ------------------------------------------------------------------------------------------------------------------------------
# Each control swaps the gate for a fake, runs one real case against it, and requires the case to MISBEHAVE (and then forgives it: it is the control).
fake="$TMP/fake.sh"
run_control() { # run_control LABEL FAKE-GATE-TEXT CASE-FUNCTION ARGS...
  t; label="$1"; printf '%s\n' "$2" > "$fake"; shift 2
  real="$GATE"; GATE="$fake"; before=$bad
  "$@" > /dev/null
  GATE="$real"
  if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: $label"; else echo "  FAIL control: $label — the selftest did not notice"; bad=$((bad+1)); fi
}
good_tree "$D"; rm "$D/$SPEC_REL"
run_control "a gate that always passes makes the 'no spec' case misbehave" '#!/usr/bin/env bash
echo "check-charge-table: OK"; exit 0' expect "no spec" "$D" 1 "$SPEC_REL does not exist"
good_tree "$D"
run_control "a gate that always fails makes the 'right tree is green' case misbehave" '#!/usr/bin/env bash
echo "check-charge-table: RED"; exit 1' expect "a right tree is green" "$D" 0 "check-charge-table: OK"
good_tree "$D"; printf '%s\n' "$BEGIN" >> "$D/$SPEC_REL"
run_control "a gate that fails for another reason makes the 'duplicated begin marker' case misbehave (class 10)" '#!/usr/bin/env bash
echo "check-charge-table: RED — something unrelated"; exit 1' expect "a duplicated begin marker" "$D" 1 "found 2 begin marker(s)"

echo "selftest: $n cases"
[ "$n" -ge "$MIN_CASES" ] || { echo "check-charge-table-selftest: RED — only $n cases ran (minimum $MIN_CASES)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-charge-table-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-charge-table-selftest: OK"
