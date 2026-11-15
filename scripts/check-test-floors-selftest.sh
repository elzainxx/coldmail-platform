#!/usr/bin/env bash
# check-test-floors-selftest — the PAIRED CONTROL for check-test-floors.sh.
#
# On a green tree the gate prints `ok`, which leaves every one of its failure branches unexercised: it could be broken into
# accepting everything and CI would stay green forever. This builds throwaway fixtures that trip each RED condition in turn and
# asserts, for every REJECT case, THREE things — not just a nonzero exit (false-green class 10: a reject case that fails for an
# unrelated reason is not a control):
#   1. the exit status is 1 (usage errors: 2),
#   2. the output contains a SUBSTRING NAMING THE REASON the case is about,
#   3. the number of `RED:` lines equals what the case expects — normally exactly ONE, so a fixture that also trips some
#      OTHER rule is caught here instead of passing for the wrong reason.
# Every GOOD case asserts rc 0 plus the `ok` verdict. Every run of the gate must also print its `NOT CHECKED:` line.
#
# Two guards on THIS file, for the same reason:
#   * a hard-coded MIN_CASES floor — deleting cases is red, not a quiet shrinkage of coverage (class 8);
#   * a final CONTROL case that feeds the harness two deliberately wrong expectations inside a subshell and requires that the
#     harness reports BOTH as mismatches — proof that `good`/`red` can fail at all (class 9/18: an instrument that matches
#     nothing reads green).
# CRLF: a Windows checkout writes `\r\n`. The CRLF cases build their fixtures with real CR bytes and ASSERT those bytes exist
# (on this author's host awk and sed hide `\r`, so a fixture whose CRs were silently lost would prove nothing).
# Not covered here: a real `go test` / vitest run — the logs are hand-built in the shape those tools print.
set -u -o pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/check-test-floors.sh"
MIN_CASES=69
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

cases=0; okc=0; bad=0
record_ok()  { cases=$((cases + 1)); okc=$((okc + 1)); echo "  ok   $1"; }
record_bad() { cases=$((cases + 1)); bad=$((bad + 1)); echo "  BAD  $1 — $2"; }
# count_in <fixed substring> <text>  ->  number of lines of <text> containing it (grep -c, never grep -q: SIGPIPE under pipefail)
count_in() { local n; n="$(grep -cF -- "$1" <<<"$2" || true)"; echo "${n:-0}"; }
# count_re <regex> <text>
count_re() { local n; n="$(grep -c -- "$1" <<<"$2" || true)"; echo "${n:-0}"; }

# good_with <label> <substring-that-must-appear> <gate args...>
good_with() {
  local label="$1" want="$2"; shift 2
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  [ "$rc" -eq 0 ] || { record_bad "$label" "expected rc 0, got $rc: $out"; return; }
  [ "$(count_in 'check-test-floors: ok' "$out")" -ge 1 ] || { record_bad "$label" "rc 0 but no 'ok' verdict line: $out"; return; }
  [ "$(count_in 'NOT CHECKED:' "$out")" -ge 1 ]          || { record_bad "$label" "no NOT CHECKED: line: $out"; return; }
  [ "$(count_in 'command not found' "$out")" -eq 0 ]     || { record_bad "$label" "green, but the gate printed a shell error (an unbound helper): $out"; return; }
  [ "$(count_in "$want" "$out")" -ge 1 ]                 || { record_bad "$label" "output lacks '$want': $out"; return; }
  record_ok "$label"
}
good() { local label="$1"; shift; good_with "$label" 'check-test-floors: ok' "$@"; }
# red <label> <reason substring> <expected number of RED lines> <gate args...>
red() {
  local label="$1" want="$2" nred="$3"; shift 3
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  [ "$rc" -eq 1 ] || { record_bad "$label" "expected rc 1, got $rc: $out"; return; }
  [ "$(count_in "$want" "$out")" -ge 1 ] || { record_bad "$label" "rejected, but not for the reason named ('$want'): $out"; return; }
  local n; n="$(count_re '^RED: ' "$out")"
  [ "$n" -eq "$nred" ] || { record_bad "$label" "expected exactly $nred RED line(s), got $n (a second rule also fired?): $out"; return; }
  [ "$(count_in "check-test-floors: RED — $nred problem(s)" "$out")" -ge 1 ] || { record_bad "$label" "verdict line does not say $nred problem(s): $out"; return; }
  [ "$(count_in 'NOT CHECKED:' "$out")" -ge 1 ] || { record_bad "$label" "no NOT CHECKED: line: $out"; return; }
  record_ok "$label (rejected: $want)"
}
# usage_case <label> <substring> <gate args...>   -> rc 2
usage_case() {
  local label="$1" want="$2"; shift 2
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  if [ "$rc" -eq 2 ] && [ "$(count_in "$want" "$out")" -ge 1 ]; then record_ok "$label (usage error: $want)"
  else record_bad "$label" "expected rc 2 with '$want', got rc=$rc: $out"; fi
}

# ---- fixtures (shaped like the real tools' output) ----------------------------------------------------------------------
PA=example.com/m/alpha; PB=example.com/m/beta; PG=example.com/m/gamma
F="$TMP/floors.txt"; R="$TMP/run.log"; L="$TMP/list.log"; V="$TMP/vitest.json"
run_alpha()  { printf '=== RUN   TestA1\n--- PASS: TestA1 (0.00s)\n=== RUN   TestA2\n=== RUN   TestA2/sub\n    --- PASS: TestA2/sub (0.00s)\n--- PASS: TestA2 (0.00s)\nPASS\nok  \t%s\t0.012s\n' "$PA"; }
run_beta()   { printf '=== RUN   TestB1\n--- PASS: TestB1 (0.00s)\nPASS\nok  \t%s\t0.003s\n' "$PB"; }
run_gamma()  { printf '?   \t%s\t[no test files]\n' "$PG"; }
list_alpha() { printf 'TestA1\nTestA2\nBenchmarkA1\nExampleA1\nok  \t%s\t0.004s\n' "$PA"; }
list_beta()  { printf 'TestB1\nok  \t%s\t0.004s\n' "$PB"; }
list_gamma() { printf '?   \t%s\t[no test files]\n' "$PG"; }
vjson()      { printf '{"numTotalTests":%s,"numPassedTests":%s,"numFailedTests":%s,"numPendingTests":%s,"numTodoTests":%s}\n' "$@"; }
base() {  # the GOOD fixture, rewritten before every case so no case inherits another's damage
  rm -f "$F" "$R" "$L" "$V"
  printf '# per-package floors\n\ngo|%s|2\ngo|%s|1\n' "$PA" "$PB" > "$F"
  { run_alpha; run_beta; run_gamma; } > "$R"
  { list_alpha; list_beta; list_gamma; } > "$L"
  vjson 3 3 0 0 0 > "$V"
}
G=(--floors "$F" --go-run-log "$R" --go-list-log "$L")
GV=("${G[@]}" --vitest-json "$V")
withvit() { printf 'vitest|ui|%s\n' "${1:-3}" >> "$F"; }   # add a vitest floors entry to the current floors file
# crlf_ok <label> <files...>: convert to CRLF with real CR bytes, then PROVE every file now contains CR (else the case is void).
crlf_ok() {
  local label="$1" f t n l; shift
  for f in "$@"; do
    t="$f.crlf"
    while IFS= read -r l || [ -n "$l" ]; do printf '%s\r\n' "$l"; done < "$f" > "$t"; mv "$t" "$f"
    n="$(tr -cd '\r' < "$f" | wc -c | tr -d ' ')"
    if [ "${n:-0}" -le 0 ]; then record_bad "$label" "fixture $(basename "$f") has no CR bytes: the CRLF case would prove nothing"; return 1; fi
  done
  return 0
}

echo "check-test-floors-selftest: GOOD cases"
base;                                              good "a correct tree passes (2 packages, 1 with subtests, 1 no-test-files)" "${G[@]}"
base; withvit 3;                                   good "a correct tree with a vitest entry passes" "${GV[@]}"
printf 'vitest|ui|3\n' > "$F"; vjson 3 3 0 0 0 > "$V"; good "a vitest-ONLY floors file (no go entries, no go logs) passes with no shell error" --floors "$F" --vitest-json "$V"
base; withvit 3
if crlf_ok "CRLF: every input written with CRLF" "$F" "$R" "$L" "$V"; then good "CRLF: floors, run log, list log and vitest JSON all CRLF — identical green" "${GV[@]}"; fi
base; { printf '=== RUN   TestA1\n--- PASS: TestA1 (0.00s)\n=== RUN   TestA2\n=== RUN   TestA2/sub\n    --- FAIL: TestA2/sub (0.00s)\n    --- SKIP: TestA2/other (0.00s)\n--- PASS: TestA2 (0.00s)\nPASS\nok  \t%s\t0.012s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   good_with "indented SUBTEST fail/skip lines are not counted (ran stays 2, 2 ignored)" "(2 indented subtest result line(s) in the run log ignored)" "${G[@]}"
base; { run_alpha; printf -- '--- PASS: ExampleA1 (0.00s)\n--- PASS: BenchmarkA1 (0.00s)\n'; run_beta; run_gamma; } > "$R"
                                                   good_with "column-0 Example/Benchmark results are not counted as tests" "Benchmark/Fuzz/Example results are not counted (2 ignored)" "${G[@]}"
base; { run_alpha; run_beta; run_gamma; printf 'ok  \texample.com/m/delta\t0.002s [no tests to run]\n'; } > "$R"
                                                   good_with "a package that ran 0 tests and has no floor is reported, not red" "1 package(s) that ran 0 tests without a floor" "${G[@]}"
base; printf '   go|%s|1   # ran 2, floor 1: a floor is a minimum\n' "$PA" > "$F"; printf 'go|%s|1\n' "$PB" >> "$F"
                                                   good "comments, indentation and a floor below the count are all fine" "${G[@]}"

echo "check-test-floors-selftest: REJECT cases — floors file"
base; rm -f "$F";                                  red "floors file missing" "floors file missing" 1 "${G[@]}"
base; : > "$F";                                    red "floors file empty (0 bytes)" "floors file has no entries" 1 "${G[@]}"
base; printf '# nothing but comments\n\n   \n# go|x|1\n' > "$F"; red "floors file with only comments and blanks" "floors file has no entries" 1 "${G[@]}"
base; printf 'go|%s|0\ngo|%s|1\n' "$PA" "$PB" > "$F"; red "a floor of 0" "floor < 1" 1 "${G[@]}"
base; printf 'go|%s|2\ngo|%s|1\ngo|%s\n' "$PA" "$PB" "$PG" > "$F"; red "a floors line with two fields" "malformed floors line 3" 1 "${G[@]}"
base; printf 'go|%s|2\ngo|%s|1\ngo|%s|many\n' "$PA" "$PB" "example.com/m/zeta" > "$F"; red "a non-numeric floor" "malformed floors line 3" 1 "${G[@]}"
base; printf 'npm|%s|2\ngo|%s|2\ngo|%s|1\n' "ui" "$PA" "$PB" > "$F"; red "an unknown kind (npm|...)" "malformed floors line 1" 1 "${G[@]}"
base; printf 'go|%s|2\ngo|%s|1\ngo|%s|5\n' "$PA" "$PB" "$PA" > "$F"; red "the same package listed twice" "duplicate floors entry: go|$PA" 1 "${G[@]}"
base; printf 'go|%s|2\ngo|%s|1\nvitest|ui|3\nvitest|ui2|3\n' "$PA" "$PB" > "$F"; red "two vitest entries" "more than one vitest| entry" 1 "${GV[@]}"
base; crlf_ok "CRLF: duplicate in a CRLF floors file" "$F" && { printf 'go|%s|9\r\n' "$PA" >> "$F"; red "CRLF floors file: the duplicate is still seen" "duplicate floors entry: go|$PA" 1 "${G[@]}"; }

echo "check-test-floors-selftest: REJECT cases — the logs (the instrument must match something)"
base; rm -f "$R";                                  red "run log missing" "run log missing" 1 "${G[@]}"
base; : > "$R";                                    red "run log EMPTY (0 bytes) — the instrument matched nothing" "run log is empty" 1 "${G[@]}"
base; printf 'go: downloading something\nnothing useful here\n' > "$R"; red "run log with no terminating line at all" "no terminating ok/FAIL/? line found in the run log" 1 "${G[@]}"
base; printf 'ok  \t%s\t0.012s\nok  \t%s\t0.003s\n' "$PA" "$PB" > "$R"; red "run log made without -v (terminators, zero test lines)" "was it made without -v" 1 "${G[@]}"
base; rm -f "$L";                                  red "list log missing" "list log missing: " 1 "${G[@]}"
base; : > "$L";                                    red "list log EMPTY (0 bytes)" "list log is empty" 1 "${G[@]}"
base; printf 'just text\n' > "$L";                 red "list log with no terminating line" "no terminating ok/FAIL/? line found in the list log" 1 "${G[@]}"
base; { list_alpha; list_gamma; } > "$L";          red "list log lacks a floors package" "list log missing package $PB" 1 "${G[@]}"
base; { run_alpha; run_beta; run_alpha; run_gamma; } > "$R"; red "a package printed twice in the run log" "package appears twice in the run log: $PA" 1 "${G[@]}"
base; { run_alpha; run_beta; run_gamma; printf -- '--- PASS: TestOrphan (0.00s)\n'; } > "$R"; red "test result lines after the last terminating line" "truncated run log: 1 test result line(s)" 1 "${G[@]}"

echo "check-test-floors-selftest: REJECT cases — the per-package rules"
base; { run_alpha; run_gamma; } > "$R";            red "a floors package vanished from the run log" "vanished subject: floors package $PB is absent from the run log" 1 "${G[@]}"
base; { run_alpha; printf '?   \t%s\t[no test files]\n' "$PB"; run_gamma; } > "$R"; red "a floors package now prints [no test files]" "vanished subject: floors package $PB printed only '[no test files]'" 1 "${G[@]}"
base; printf 'go|%s|2\n' "$PA" > "$F";             red "a package that ran tests has no floor" "package has no floor: $PB" 1 "${G[@]}"
base; { printf 'TestA1\nTestA2\nTestA3\nok  \t%s\t0.004s\n' "$PA"; list_beta; list_gamma; } > "$L"
                                                   red "declared 3 but ran 2" "declared != ran: $PA declared=3 ran=2" 1 "${G[@]}"
base; { printf -- '--- PASS: TestA1 (0.00s)\n--- PASS: TestA2 (0.00s)\n--- PASS: TestA3 (0.00s)\nok  \t%s\t0.012s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   red "ran 3 but declared 2" "declared != ran: $PA declared=2 ran=3" 1 "${G[@]}"
base; printf 'go|%s|3\ngo|%s|1\n' "$PA" "$PB" > "$F"; red "ran 2 below a floor of 3" "ran < floor: $PA ran=2 floor=3" 1 "${G[@]}"
base; { printf -- '=== RUN   TestA1\n--- SKIP: TestA1 (0.00s)\n--- PASS: TestA2 (0.00s)\nPASS\nok  \t%s\t0.012s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   red "a skipped test" "skipped tests: $PA skip=1" 1 "${G[@]}"
base; { printf -- '--- FAIL: TestA1 (0.00s)\n    a_test.go:9: boom\n--- PASS: TestA2 (0.00s)\nFAIL\nFAIL\t%s\t0.012s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   red "a failed test" "failed tests: $PA fail=1" 1 "${G[@]}"
base; { run_alpha; printf '# %s [%s.test]\nb_test.go:5:2: undefined: foo\nFAIL\t%s [build failed]\n' "$PB" "$PB" "$PB"; run_gamma; } > "$R"
                                                   red "a package that no longer builds" "build failure: $PB printed FAIL with no test results" 1 "${G[@]}"
base; { printf -- '--- PASS: TestA1 (0.00s)\n--- PASS: TestA2 (0.00s)\npanic: in TestMain teardown\nexit status 2\nFAIL\t%s\t0.512s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   red "a crash AFTER every test passed (FAIL line, no --- FAIL)" "package FAIL without a failing test: $PA" 1 "${G[@]}"
base; { printf -- '--- PASS: TestA1 (0.00s)\npanic: runtime error\nexit status 2\nFAIL\t%s\t0.512s\n' "$PA"; run_beta; run_gamma; } > "$R"
                                                   red "a panic MID-run also trips declared != ran and ran < floor" "package FAIL without a failing test: $PA" 3 "${G[@]}"
base; { run_alpha; run_beta; run_gamma; printf 'FAIL\texample.com/m/delta\t0.4s\n'; } > "$R"
                                                   red "an unfloored package that fails" "package has no floor: example.com/m/delta" 1 "${G[@]}"

echo "check-test-floors-selftest: REJECT cases — CRLF-terminated logs must be caught exactly like LF ones"
base; { printf -- '--- SKIP: TestA1 (0.00s)\n--- PASS: TestA2 (0.00s)\nok  \t%s\t0.012s\n' "$PA"; run_beta; run_gamma; } > "$R"
if crlf_ok "CRLF skip" "$F" "$R" "$L"; then       red "CRLF: a skipped test is still caught" "skipped tests: $PA skip=1" 1 "${G[@]}"; fi
base; { run_alpha; printf '?   \t%s\t[no test files]\n' "$PB"; run_gamma; } > "$R"
if crlf_ok "CRLF vanished" "$F" "$R" "$L"; then   red "CRLF: a [no test files] package is still a vanished subject" "vanished subject: floors package $PB printed only '[no test files]'" 1 "${G[@]}"; fi
base; { printf 'TestA1\nTestA2\nTestA3\nok  \t%s\t0.004s\n' "$PA"; list_beta; list_gamma; } > "$L"
if crlf_ok "CRLF declared" "$F" "$R" "$L"; then   red "CRLF: declared != ran is still caught" "declared != ran: $PA declared=3 ran=2" 1 "${G[@]}"; fi
base; : > "$R"; printf '\r\n\r\n' > "$R"
if [ "$(tr -cd '\r' < "$R" | wc -c | tr -d ' ')" -gt 0 ]; then red "a run log of only CRLF blank lines is an empty instrument" "no terminating ok/FAIL/? line found in the run log" 1 "${G[@]}"; else record_bad "CRLF blank run log" "fixture has no CR"; fi

echo "check-test-floors-selftest: REJECT cases — vitest"
base; withvit 3;                                   red "vitest floor but no --vitest-json" "--vitest-json was not given" 1 "${G[@]}"
base;                                              red "--vitest-json but no vitest floor" "vitest result with no floor" 1 "${GV[@]}"
base; withvit 3; vjson 3 2 0 0 0 > "$V";           red "vitest total 3, passed 2" "vitest total != passed: total=3 passed=2" 1 "${GV[@]}"
base; withvit 3; vjson 3 3 1 0 0 > "$V";           red "vitest failed 1" "vitest failed tests: failed=1" 1 "${GV[@]}"
base; withvit 3; vjson 3 3 0 1 0 > "$V";           red "vitest pending (skipped) 1" "vitest skipped/todo tests: pending=1 todo=0" 1 "${GV[@]}"
base; withvit 3; vjson 3 3 0 0 1 > "$V";           red "vitest todo 1" "vitest skipped/todo tests: pending=0 todo=1" 1 "${GV[@]}"
base; withvit 3; vjson 2 2 0 0 0 > "$V";           red "vitest total 2 below floor 3" "vitest total < floor: total=2 floor=3" 1 "${GV[@]}"
base; withvit 3; vjson 0 0 0 0 0 > "$V";           red "vitest ran nothing (0 of floor 3)" "vitest total < floor: total=0 floor=3" 1 "${GV[@]}"
base; withvit 3; printf '{"numTotalTests":3,"numPassedTests":3,"numFailedTests":0,"numPendingTests":0}\n' > "$V"; red "vitest JSON lacks numTodoTests" "missing field numTodoTests" 1 "${GV[@]}"
base; withvit 3; printf '{"numTotalTests":"3","numPassedTests":3,"numFailedTests":0,"numPendingTests":0,"numTodoTests":0}\n' > "$V"; red "vitest numTotalTests is a string" "missing field numTotalTests" 1 "${GV[@]}"
base; withvit 3; printf 'this is not json {' > "$V"; red "vitest JSON is not JSON" "not valid JSON" 1 "${GV[@]}"
base; withvit 3; : > "$V";                         red "vitest JSON empty" "vitest JSON file is empty" 1 "${GV[@]}"
base; withvit 3; rm -f "$V";                       red "vitest JSON path does not exist" "vitest JSON file missing" 1 "${GV[@]}"
base; withvit 3; vjson 3 3 0 1 0 > "$V"
if crlf_ok "CRLF vitest" "$F" "$R" "$L" "$V"; then red "CRLF: vitest pending is still caught" "vitest skipped/todo tests: pending=1 todo=0" 1 "${GV[@]}"; fi

echo "check-test-floors-selftest: the Go logs are required exactly when the floors file has a go| entry"
base;                                              red "go| floors but --go-list-log not given" "both --go-run-log and --go-list-log are needed" 1 --floors "$F" --go-run-log "$R"
base;                                              red "go| floors but --go-run-log not given" "both --go-run-log and --go-list-log are needed" 1 --floors "$F" --go-list-log "$L"
base;                                              red "go| floors but NO Go log at all (a silent skip would be a green)" "both --go-run-log and --go-list-log are needed" 1 --floors "$F"
base; printf 'vitest|ui|3\n' > "$F";                good_with "a vitest-only floors file needs no Go logs and says the Go sections were skipped" "go test results were not read at all" --floors "$F" --vitest-json "$V"
base; printf 'vitest|ui|3\n' > "$F";                red "a vitest-only floors file with no --vitest-json" "--vitest-json was not given" 1 --floors "$F"
base; printf 'vitest|ui|3\n' > "$F";                red "a vitest-only floors file handed Go logs: both packages have no floor" "package has no floor: $PA" 2 "${GV[@]}"

echo "check-test-floors-selftest: usage errors"
usage_case "no arguments" "usage:"
usage_case "no --floors given (only the logs)" "usage:" --go-run-log "$R" --go-list-log "$L"
usage_case "an unknown flag" "unknown argument: --bogus" "${G[@]}" --bogus
usage_case "a flag with no value" "usage:" --floors "$F" --go-run-log "$R" --go-list-log

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------
# Inside a subshell (so its counters cannot leak into ours) feed `good` and `red` two WRONG expectations: a green fixture
# claimed to be red for a reason it does not have, and a red fixture (a skipped test) claimed to be green. The harness must
# report BOTH as mismatches. If it reported them as passes, every `ok` above would be meaningless.
echo "check-test-floors-selftest: control"
ctl="$( (
  cases=0; okc=0; bad=0
  { base; red "wrong-reason" "vanished subject" 1 "${G[@]}"; } >/dev/null
  { base; { printf -- '--- SKIP: TestA1 (0.00s)\n--- PASS: TestA2 (0.00s)\nok  \t%s\t0.012s\n' "$PA"; run_beta; } > "$R"; good "wrong-verdict" "${G[@]}"; } >/dev/null
  echo "CONTROL cases=$cases ok=$okc bad=$bad"
) 2>&1 )"
if [ "$(count_in 'CONTROL cases=2 ok=0 bad=2' "$ctl")" -ge 1 ]; then
  record_ok "control: the harness reports a wrong reason and a wrong verdict as mismatches ($ctl)"
else
  record_bad "control" "the harness did NOT flag deliberately wrong expectations: $ctl"
fi

echo "selftest: $cases cases, $okc behaved as expected"
if [ "$cases" -lt "$MIN_CASES" ]; then
  echo "selftest: RED — only $cases cases ran, the hard floor is $MIN_CASES (were cases deleted?)"; exit 1
fi
if [ "$bad" -ne 0 ] || [ "$okc" -ne "$cases" ]; then
  echo "selftest: RED — $bad case(s) did NOT behave as expected"; exit 1
fi
exit 0
