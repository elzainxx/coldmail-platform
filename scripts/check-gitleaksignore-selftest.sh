#!/usr/bin/env bash
# check-gitleaksignore-selftest.sh — builds a scratch repository with a main history and a fault/p0-x branch, and proves check-gitleaksignore.sh is green on a legitimate entry and
# RED, for the reason it names, on every way of using the ignore file to silence something real.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; G="$HERE/check-gitleaksignore.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
R="$T/r"; git init -q "$R"; git -C "$R" config user.email t@example.invalid; git -C "$R" config user.name t; git -C "$R" config core.autocrlf false
git -C "$R" checkout -q -b main 2>/dev/null
echo a > "$R/a"; git -C "$R" add a; git -C "$R" commit -q -m main1; MAIN1=$(git -C "$R" rev-parse HEAD)
git -C "$R" checkout -q -b fault/p0-x; echo b > "$R/b"; git -C "$R" add b; git -C "$R" commit -q -m fault; FAULT=$(git -C "$R" rev-parse HEAD)
git -C "$R" checkout -q -b fault/other "$MAIN1"; echo c > "$R/c"; git -C "$R" add c; git -C "$R" commit -q -m notfault; NOTFAULT=$(git -C "$R" rev-parse HEAD)
git -C "$R" checkout -q main
MIN=8; cases=0; ok=0; bad=0
expect() { # label want-rc substring content-of-ignore-file(or -)
  label="$1"; want="$2"; sub="$3"; content="$4"; cases=$((cases+1))
  if [ "$content" = "-" ]; then rm -f "$T/ign"; else printf '%b' "$content" > "$T/ign"; fi
  out="$(bash "$G" --file "$T/ign" --repo "$R" 2>&1)"; rc=$?
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), '$sub' x$n"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
expect "no ignore file"                              0 "0 ignore entries" "-"
expect "an empty file with only comments"            0 "0 ignore entries" "# nothing\n\n"
expect "a fingerprint of a commit on a fault branch" 0 "check-gitleaksignore: OK" "$FAULT:planted.txt:generic-api-key:1\n"
expect "a fingerprint of a commit on HEAD (real history)" 1 "reachable from HEAD" "$MAIN1:a:generic-api-key:1\n"
expect "a commit on a branch that is not fault/p0-*" 1 "is on no fault/p0-* branch" "$NOTFAULT:c:generic-api-key:1\n"
expect "a commit that does not exist"                1 "is not a commit in this repository" "0123456789012345678901234567890123456789:f:r:1\n"
expect "a path ignore instead of a fingerprint"      1 "not a gitleaks fingerprint" "Docs/history/\n"
expect "a legitimate entry with CRLF line endings"   0 "check-gitleaksignore: OK" "$FAULT:planted.txt:generic-api-key:1\r\n"
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
