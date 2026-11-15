#!/usr/bin/env bash
# fetch-modules-lock-selftest.sh — proves tools/fetch-modules.sh REFUSES a bad lock file before it clones anything, and says which fault it found.
# Needs no network and no deploy keys: lock validation runs first and exits non-zero, so the clone loop is never reached.
# NOT CHECKED here (it needs the real private repositories, so it is proven on the runner by the harness workflow): the clone, the checkout at the pin,
# the "HEAD == pin" assertion, the reachable-from-default-branch assertion.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; F="$HERE/fetch-modules.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
A=abcdef1234abcdef1234abcdef1234abcdef1234; B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; C=cccccccccccccccccccccccccccccccccccccccc   # A has letters, so its upper-case form really differs
MIN=8; cases=0; ok=0; bad=0
lock() { printf '%b' "$1" > "$T/lock"; }
expect() { # label, want-substring
  cases=$((cases+1)); out="$(bash "$F" --lock "$T/lock" "$T/dest" 2>&1)"; rc=$?
  n=$(printf '%s\n' "$out" | grep -c -F -- "$2" || true)
  if [ "$rc" -ne 0 ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [$1] rc=$rc — $2"
  else bad=$((bad+1)); echo "  MISMATCH [$1]: rc=$rc, want non-zero with '$2'"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
good="coldmail-verify $A\ncoldmail-send-managed $B\ncoldmail-send-byo $C\n"
lock "coldmail-send-managed $B\ncoldmail-send-byo $C\n";                           expect "an entry missing"            "has 0 entries for coldmail-verify"
lock "$good""coldmail-verify $A\n";                                                expect "a duplicated entry"          "has 2 entries for coldmail-verify"
lock "$good""coldmail-platform $A\n";                                              expect "a repository that is not a module" "is not one of the three modules: coldmail-platform"
lock "coldmail-verify abc123\ncoldmail-send-managed $B\ncoldmail-send-byo $C\n";   expect "a short commit id"           "not a full 40-hex commit id"
lock "coldmail-verify ${A^^}\ncoldmail-send-managed $B\ncoldmail-send-byo $C\n";   expect "an upper-case commit id"     "not a full 40-hex commit id"
lock "$(printf 'coldmail-verify %s\r\ncoldmail-send-managed %s\r\n' $A $B)";  expect "CRLF and a missing entry" "has 0 entries for coldmail-send-byo"
lock "";                                                                           expect "an empty lock"               "has 0 entries for coldmail-verify"
cases=$((cases+1)); out="$(bash "$F" --lock "$T/nope" "$T/dest" 2>&1)"; rc=$?; n=$(printf '%s\n' "$out" | grep -c -F "does not exist" || true)
if [ "$rc" -ne 0 ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [no lock file] rc=$rc — does not exist"; else bad=$((bad+1)); echo "  MISMATCH [no lock file]: rc=$rc"; fi
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
