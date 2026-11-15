#!/usr/bin/env bash
# check-public-schema.sh --allowlist FILE (--listing FILE|- | --dsn DSN) — schema public may hold ONLY what the allowlist names.
#
# WHY IT EXISTS. The platform shares one database with three modules. Modules 2 and 3 live in their own schemas; module 1 lived in public until Phase 1 moved it. The platform's own
# `users` and `job_items` must live in public (the modules read them there) and nothing else may: a second owner in public is how a generic name ("jobs", "cache") collides, or an
# over-broad default privilege reaches a table nobody meant to share. This is an ALLOWLIST, not a list of module-1 names, so it also catches a module-2/3 leak, a platform leak, and
# the next module's habit of creating unqualified tables.
#
# INPUT is the listing produced by scripts/public-schema-listing.sql (pipe-separated: kind|name|parent table|extension-owned t/f). `--dsn` runs that query with psql; `--listing`
# reads it from a file or stdin (this is how the selftest feeds fixtures without a database, and how the harness feeds it from a container).
# ALLOWLIST lines: `table NAME` (the table, plus its indexes and owned sequences, are allowed), or `KIND NAME` for anything else (function, type, view, sequence, ...). `#` comments.
#
# RED when: an object in public is not allowlisted (named, with its kind); an allowlisted table is not found (a stale entry would hide the next miss); the allowlist has no entries or a
# malformed line; the listing is empty (the instrument matched nothing). Objects that belong to an extension are not judged; their number is printed.
# NOT CHECKED: privileges and default privileges on the allowed tables, operators / casts / text-search objects / collations in public, event triggers, and every other schema.
set -u
ALLOW=""; LISTING=""; DSN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --allowlist) ALLOW="${2:?--allowlist needs a file}"; shift 2;;
    --listing)   LISTING="${2:?--listing needs a file or -}"; shift 2;;
    --dsn)       DSN="${2:?--dsn needs a connection string}"; shift 2;;
    *) echo "check-public-schema: unknown argument '$1'" >&2; exit 2;;
  esac
done
[ -n "$ALLOW" ] || { echo "check-public-schema: --allowlist is required" >&2; exit 2; }
[ -f "$ALLOW" ] || { echo "check-public-schema: RED — the allowlist $ALLOW does not exist"; exit 1; }
{ [ -n "$LISTING" ] || [ -n "$DSN" ]; } || { echo "check-public-schema: give --listing or --dsn" >&2; exit 2; }
HERE="$(cd "$(dirname "$0")" && pwd)"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

if [ -n "$DSN" ]; then
  if ! psql -X -At -F '|' -v ON_ERROR_STOP=1 -f "$HERE/public-schema-listing.sql" "$DSN" > "$T/listing" 2> "$T/psql.err"; then
    echo "check-public-schema: RED — the listing query failed: $(tr -d '\r' < "$T/psql.err" | head -3 | tr '\n' ' ')"; exit 1
  fi
elif [ "$LISTING" = "-" ]; then cat > "$T/listing"
else
  [ -f "$LISTING" ] || { echo "check-public-schema: RED — listing file $LISTING does not exist"; exit 1; }
  cp "$LISTING" "$T/listing"
fi
tr -d '\r' < "$T/listing" | awk 'NF > 0' > "$T/l"

fail=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }

# allowlist -> "kind|name" lines
: > "$T/a"
while IFS= read -r line; do
  line="$(printf '%s' "$line" | tr -d '\r')"; case "$line" in ''|'#'*) continue;; esac
  set -- $line
  if [ $# -ne 2 ] || ! printf '%s' "$1" | grep -Eq '^[a-z-]+$'; then bad "allowlist line is not '<kind> <name>': $line"; continue; fi
  printf '%s|%s\n' "$1" "$2" >> "$T/a"
done < "$ALLOW"
na=$(wc -l < "$T/a" | tr -d ' ')
[ "$na" -ge 1 ] || bad "the allowlist has no entries (an empty allowlist would call every public object a leak, or nothing at all)"

total=$(wc -l < "$T/l" | tr -d ' ')
[ "$total" -ge 1 ] || bad "found 0 objects in public: the instrument matched nothing (the allowlisted tables must at least exist)"

ext=$(awk -F'|' '$4=="t"' "$T/l" | wc -l | tr -d ' ')
judged=$(awk -F'|' '$4!="t"' "$T/l" | wc -l | tr -d ' ')

# every judged object must be allowed
leaks=0
while IFS='|' read -r kind name parent isext; do
  [ "$isext" = "t" ] && continue
  if grep -Fxq "$kind|$name" "$T/a"; then continue; fi
  case "$kind" in
    index|sequence) if [ -n "$parent" ] && grep -Fxq "table|$parent" "$T/a"; then continue; fi;;
  esac
  extra=""; [ -n "$parent" ] && extra=" (of table $parent)"
  bad "$kind $name$extra is in public and not allowlisted"; leaks=$((leaks+1))
done < "$T/l"

# every allowlisted entry must exist
stale=0
while IFS='|' read -r kind name; do
  if ! awk -F'|' -v k="$kind" -v n="$name" '$1==k && $2==n && $4!="t" {f=1} END{exit f?0:1}' "$T/l"; then
    bad "$kind $name is in the allowlist but not found in public (a stale entry hides the next miss)"; stale=$((stale+1))
  fi
done < "$T/a"

echo "check-public-schema: $judged object(s) in public judged, $leaks outside the allowlist, $stale stale allowlist entries; $ext owned by an extension (not judged)"
echo "NOT CHECKED: privileges / default privileges on the allowed tables; operators, casts, text-search objects and collations in public; event triggers; every other schema"
[ "$fail" -eq 0 ] || { echo "check-public-schema: RED — $fail problem(s): $leaks object(s) in public outside the allowlist, $stale stale allowlist entries"; exit 1; }
echo "check-public-schema: OK"
