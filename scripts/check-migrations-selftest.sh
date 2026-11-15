#!/usr/bin/env bash
# check-migrations-selftest.sh — proves scripts/check-migrations.sh goes RED, FOR THE REASON IT NAMES, on each convention it enforces,
# and stays green on a good chain (including one checked out with CRLF line endings, and one whose forbidden text sits only in a comment).
# A reject case that failed for an unrelated reason would read as a pass forever (class 10), so every case asserts a reason substring.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-migrations.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=19; cases=0; ok=0; bad_expect=0

mk() { # mk <name> — fresh fixture dir with a good two-migration chain and a matching lifecycle test; echoes the dir
  d="$T/$1"; rm -rf "$d"; mkdir -p "$d/m"
  for v in 0000 0001; do
    printf -- "-- up %s\nCREATE TABLE IF NOT EXISTS platform.t%s (id int);\nINSERT INTO platform.schema_migrations (version) VALUES ('%s');\n" "$v" "$v" "$v" > "$d/m/platform_${v}_x.up.sql"
    printf -- "-- down %s\nDELETE FROM platform.schema_migrations WHERE version = '%s';\n" "$v" "$v" > "$d/m/platform_${v}_x.down.sql"
  done
  printf 'wantChain := []string{\n\t"0000",\n\t"0001",\n}\n' > "$d/lifecycle_test.go"
  echo "$d"
}
# expect <label> <want-rc> <want-substring|-> <fixture-dir> [extra gate args...]
expect() {
  label="$1"; want_rc="$2"; sub="$3"; d="$4"; shift 4
  cases=$((cases+1))
  out="$(bash "$GATE" --dir "$d/m" "$@" 2>&1)"; rc=$?
  if [ "$rc" -ne "$want_rc" ]; then echo "  MISMATCH [$label]: rc=$rc want=$want_rc"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return; fi
  if [ "$sub" != "-" ]; then
    n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
    if [ "$n" -lt 1 ]; then echo "  MISMATCH [$label]: red, but not for the named reason '$sub'"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return; fi
  fi
  ok=$((ok+1)); echo "  ok [$label] rc=$rc${sub:+ — ${sub}}" | sed 's/ — -$//'
}

d=$(mk good);                                   expect "good chain, with lifecycle coupling" 0 "check-migrations: OK" "$d" --lifecycle-test "$d/lifecycle_test.go"
d=$(mk empty); rm -f "$d"/m/*;                  expect "empty directory (the gate matched nothing)" 1 "found 0 .up.sql files" "$d"
d=$(mk nodown); rm "$d/m/platform_0001_x.down.sql"; expect "up without a down" 1 "has no matching platform_0001_x.down.sql" "$d"
d=$(mk gap); for e in up down; do mv "$d/m/platform_0001_x.$e.sql" "$d/m/platform_0002_x.$e.sql"; done
                                                 expect "a gap in the chain" 1 "gap or duplicate: after 0000 expected 0001, got 0002" "$d"
d=$(mk start1); rm "$d/m/platform_0000_x."*;    expect "chain not starting at 0000" 1 "the chain must start at 0000, got 0001" "$d"
d=$(mk norec); printf -- "CREATE TABLE IF NOT EXISTS platform.t (id int);\n" > "$d/m/platform_0001_x.up.sql"
                                                 expect "up that does not self-record" 1 "does not self-record version '0001'" "$d"
d=$(mk norm); printf -- "DROP TABLE platform.t0001;\n" > "$d/m/platform_0001_x.down.sql"
                                                 expect "down that neither deletes its row nor says no-op" 1 "neither DELETEs version '0001'" "$d"
d=$(mk fwd); printf -- "-- forward-only: cannot safely remove an enum value\n" > "$d/m/platform_0001_x.down.sql"
                                                 expect "down documented as a forward-only no-op is accepted" 0 "check-migrations: OK" "$d"
d=$(mk stray); printf 'select 1;\n' > "$d/m/notes.sql"; expect "a stray .sql file" 1 "notes.sql: not named" "$d"
d=$(mk pub); printf -- "GRANT SELECT ON platform.t0001 TO PUBLIC;\n" >> "$d/m/platform_0001_x.up.sql"
                                                 expect "GRANT ... TO PUBLIC" 1 "platform_0001_x.up.sql grants to PUBLIC" "$d"
d=$(mk pubc); printf -- "-- never GRANT SELECT ON anything TO PUBLIC\n" >> "$d/m/platform_0001_x.up.sql"
                                                 expect "forbidden text only inside a comment is ignored" 0 "check-migrations: OK" "$d"
d=$(mk unq); printf -- "CREATE TABLE t (id int);\nINSERT INTO platform.schema_migrations (version) VALUES ('0001');\n" > "$d/m/platform_0001_x.up.sql"
                                                 expect "CREATE TABLE that is not schema-qualified" 1 "CREATE TABLE that is not schema-qualified" "$d"
d=$(mk lc1); printf 'wantChain := []string{\n\t"0000",\n}\n' > "$d/lifecycle_test.go"
                                                 expect "newest version missing from wantChain" 1 "newest migration '0001' is absent from wantChain" "$d" --lifecycle-test "$d/lifecycle_test.go"
d=$(mk lc2); printf 'want := []string{"0000", "0001"}\n' > "$d/lifecycle_test.go"
                                                 expect "wantChain literal renamed away" 1 "could not find the 'wantChain" "$d" --lifecycle-test "$d/lifecycle_test.go"
d=$(mk lc3);                                     expect "lifecycle test file missing" 1 "does not exist" "$d" --lifecycle-test "$d/nope_test.go"
d=$(mk lc4); printf '// "0001" appears only in this comment, outside wantChain\nwantChain := []string{\n\t"0000",\n}\n' > "$d/lifecycle_test.go"
                                                 expect "newest version mentioned elsewhere in the file is not credited" 1 "absent from wantChain" "$d" --lifecycle-test "$d/lifecycle_test.go"
d=$(mk crlf); for f in "$d"/m/*.sql "$d/lifecycle_test.go"; do sed -i 's/$/\r/' "$f"; done
                                                 expect "good chain with CRLF line endings" 0 "check-migrations: OK" "$d" --lifecycle-test "$d/lifecycle_test.go"
d=$(mk crlfbad); for f in "$d"/m/*.sql; do sed -i 's/$/\r/' "$f"; done; printf -- "CREATE TABLE IF NOT EXISTS platform.t (id int);\r\n" > "$d/m/platform_0001_x.up.sql"
                                                 expect "CRLF chain whose up does not self-record is still red" 1 "does not self-record version '0001'" "$d"
d=$(mk badargs); cases=$((cases+1)); out="$(bash "$GATE" --nonsense 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then ok=$((ok+1)); echo "  ok [unknown argument] rc=2"; else echo "  MISMATCH [unknown argument]: rc=$rc"; bad_expect=$((bad_expect+1)); fi

# the selftest's own control: a deliberately WRONG expectation must be reported as a mismatch (a harness that cannot disagree is not testing)
cases_before=$cases; ok_before=$ok; bad_before=$bad_expect
d=$(mk ctl); expect "CONTROL (expected to mismatch): a good chain wrongly expected to be red" 1 "-" "$d" >/dev/null 2>&1
if [ "$bad_expect" -eq $((bad_before+1)) ]; then
  bad_expect=$bad_before; cases=$cases_before; echo "  ok [selftest control] a wrong expectation was detected as a mismatch"; cases=$((cases+1)); ok=$((ok+1))
else
  echo "  MISMATCH [selftest control]: a wrong expectation was NOT detected — this selftest cannot fail"; bad_expect=$((bad_expect+1))
fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — only $cases cases ran, the floor is $MIN_CASES (cases were deleted or skipped)"; exit 1; }
[ "$bad_expect" -eq 0 ] && [ "$ok" -eq "$cases" ] || exit 1
