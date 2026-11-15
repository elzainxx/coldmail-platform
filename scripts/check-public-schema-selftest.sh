#!/usr/bin/env bash
# check-public-schema-selftest.sh — proves check-public-schema.sh is green on a clean public schema and RED, for the reason it names, on each way public can be wrong. It feeds the
# gate fixture LISTINGS (the output format of scripts/public-schema-listing.sql), so it needs no database; the SQL itself is exercised live by the harness and the fault-proofs legs.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; G="$HERE/check-public-schema.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=14; cases=0; ok=0; bad=0
printf '# fixture allowlist\ntable users\ntable job_items\n' > "$T/allow"
CLEAN='table|users||f
index|users_pkey|users|f
table|job_items||f
index|job_items_pkey|job_items|f
sequence|job_items_seq|job_items|f
'
expect() { # label want-rc substring listing [allowlist-file]
  label="$1"; want="$2"; sub="$3"; listing="$4"; allow="${5:-$T/allow}"; cases=$((cases+1))
  printf '%s' "$listing" > "$T/l"
  out="$(bash "$G" --allowlist "$allow" --listing "$T/l" 2>&1)"; rc=$?
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), '$sub' x$n"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
expect "clean: the platform's two tables, their indexes and an owned sequence"  0 "check-public-schema: OK" "$CLEAN"
expect "clean listing with CRLF line endings"                                  0 "check-public-schema: OK" "$(printf '%s' "$CLEAN" | sed 's/$/\r/')"
expect "an extension-owned object is not judged, and is counted"               0 "1 owned by an extension (not judged)" "${CLEAN}function|gen_stuff||t
"
expect "a module-1 table leaked"                      1 "table verifications is in public and not allowlisted" "${CLEAN}table|verifications||f
"
expect "a leaked table's own index is named too"      1 "index idx_verifications_email_hash (of table verifications) is in public and not allowlisted" "${CLEAN}table|verifications||f
index|idx_verifications_email_hash|verifications|f
"
expect "a partition leaked"                           1 "table verification_raw_2026_11 is in public" "${CLEAN}table|verification_raw_2026_11||f
"
expect "a function leaked"                            1 "function ensure_verification_raw_partitions is in public" "${CLEAN}function|ensure_verification_raw_partitions||f
"
expect "an unowned sequence leaked"                   1 "sequence stray_seq is in public" "${CLEAN}sequence|stray_seq||f
"
expect "an enum type leaked"                          1 "type job_status is in public" "${CLEAN}type|job_status||f
"
expect "a view leaked"                                1 "view v_users is in public" "${CLEAN}view|v_users||f
"
expect "a stale allowlist entry (an allowed table is gone)"  1 "table job_items is in the allowlist but not found" "table|users||f
index|users_pkey|users|f
"
expect "an empty listing: the instrument matched nothing"    1 "found 0 objects in public: the instrument matched nothing" ""
printf '# nothing\n' > "$T/allow-empty"
expect "an allowlist with no entries"                 1 "the allowlist has no entries" "$CLEAN" "$T/allow-empty"
printf 'table users extra\n' > "$T/allow-bad"
expect "a malformed allowlist line"                   1 "allowlist line is not '<kind> <name>'" "$CLEAN" "$T/allow-bad"
# the allowlist may name a non-table kind explicitly
printf 'table users\ntable job_items\nfunction gen_stuff\n' > "$T/allow-fn"
expect "an explicitly allowlisted function is allowed"       0 "check-public-schema: OK" "${CLEAN}function|gen_stuff||f
" "$T/allow-fn"
# a missing allowlist file is red, not a pass
out="$(bash "$G" --allowlist "$T/nope" --listing "$T/l" 2>&1)"; rc=$?; cases=$((cases+1))
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "the allowlist $T/nope does not exist"; then ok=$((ok+1)); echo "  ok [a missing allowlist file] rc=1"; else bad=$((bad+1)); echo "  MISMATCH [a missing allowlist file]: rc=$rc"; fi
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
