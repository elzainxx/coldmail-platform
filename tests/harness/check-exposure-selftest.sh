#!/usr/bin/env bash
# check-exposure-selftest.sh — proves check-exposure.sh goes RED, for the named reason, when a service publishes a port, when an expected service is missing from the
# listing, when the listing is empty or unparseable, and when it is told to vouch for nothing; and stays green on a clean listing (one object per line, a JSON array,
# and CRLF line endings). Needs node (the gate's JSON reader), no Docker.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; G="$HERE/check-exposure.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=9; cases=0; ok=0; bad=0
svc() { # svc <name> <published-port|0>  -> one compose-ps JSON object
  if [ "$2" = 0 ]; then printf '{"Service":"%s","Publishers":[{"URL":"","TargetPort":8080,"PublishedPort":0,"Protocol":"tcp"}]}' "$1"
  else printf '{"Service":"%s","Publishers":[{"URL":"0.0.0.0","TargetPort":8080,"PublishedPort":%s,"Protocol":"tcp"}]}' "$1" "$2"; fi
}
expect() { # label want-rc want-substring file services...
  label="$1"; want="$2"; sub="$3"; f="$4"; shift 4; cases=$((cases+1))
  out="$(bash "$G" "$f" "$@" 2>&1)"; rc=$?
  n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
  if [ "$rc" -eq "$want" ] && [ "$n" -ge 1 ]; then ok=$((ok+1)); echo "  ok [$label] rc=$rc — $sub"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$rc (want $want), substring '$sub' found $n times"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
{ svc m1 0; echo; svc m2 0; echo; svc m3 0; echo; svc postgres 0; echo; } > "$T/good.jsonl"
expect "clean listing, one object per line"        0 "check-exposure: OK"                      "$T/good.jsonl" m1 m2 m3 postgres
printf '[%s,%s,%s]' "$(svc m1 0)" "$(svc m2 0)" "$(svc m3 0)" > "$T/arr.json"
expect "clean listing as a JSON array"             0 "check-exposure: OK"                      "$T/arr.json" m1 m2 m3
sed 's/$/\r/' "$T/good.jsonl" > "$T/crlf.jsonl"
expect "clean listing with CRLF line endings"      0 "check-exposure: OK"                      "$T/crlf.jsonl" m1 m2 m3 postgres
{ svc m1 0; echo; svc m2 18080; echo; svc m3 0; echo; } > "$T/pub.jsonl"
expect "a module publishes a port"                 1 "FAIL: m2 publishes 18080->8080/tcp"      "$T/pub.jsonl" m1 m2 m3
{ svc m1 0; echo; svc m2 0; echo; } > "$T/missing.jsonl"
expect "an expected service is absent"             1 "expected service 'm3' is not in the listing" "$T/missing.jsonl" m1 m2 m3
: > "$T/empty.jsonl"
expect "an empty listing (instrument saw nothing)" 1 "is empty: the instrument saw no services" "$T/empty.jsonl" m1
printf 'not json at all\n' > "$T/junk.jsonl"
expect "an unparseable listing"                    1 "could not be parsed"                     "$T/junk.jsonl" m1
expect "told to vouch for nothing"                 1 "no expected services given"              "$T/good.jsonl"
# the service that publishes a port is NOT one the caller listed: it is still red, because the claim is about the whole stack
{ svc m1 0; echo; svc extra 9999; echo; } > "$T/extra.jsonl"
expect "an UNLISTED service publishing a port is still red" 1 "FAIL: extra publishes 9999->8080/tcp" "$T/extra.jsonl" m1
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
