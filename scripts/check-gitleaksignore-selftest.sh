#!/usr/bin/env bash
# check-gitleaksignore-selftest.sh — proves check-gitleaksignore.sh is green on an absent or empty ignore file and RED, for the reason it names, on every way of putting an entry in.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; G="$HERE/check-gitleaksignore.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=8; cases=0; ok=0; bad=0
expect() { # label want-rc substring content-of-ignore-file(or -)
  label="$1"; want="$2"; sub="$3"; content="$4"; cases=$((cases+1))
  if [ "$content" = "-" ]; then rm -f "$T/ign"; else printf '%b' "$content" > "$T/ign"; fi
  out="$(bash "$G" --file "$T/ign" 2>&1)"; rc=$?
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), '$sub' x$n"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
FP="0123456789abcdef0123456789abcdef01234567:planted.txt:generic-api-key:1"
expect "no ignore file"                          0 "0 ignore entries (must be 0)"        "-"
expect "an empty file"                           0 "check-gitleaksignore: OK"            ""
expect "comments and blank lines only"           0 "0 ignore entries (must be 0)"        "# nothing is ignored\n\n   \n# really\n"
expect "comments and blank lines, CRLF"          0 "check-gitleaksignore: OK"            "# nothing\r\n\r\n"
expect "a well-formed fingerprint"               1 "must hold no entries, found: $FP"     "$FP\n"
expect "a fingerprint after a comment"           1 "check-gitleaksignore: RED"           "# ok\n$FP\n"
expect "a path ignore instead of a fingerprint"  1 "must hold no entries, found: Docs/history/" "Docs/history/\n"
expect "an entry with CRLF line endings"         1 "must hold no entries, found: $FP"     "$FP\r\n"
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
