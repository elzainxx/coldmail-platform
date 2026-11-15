#!/usr/bin/env bash
# check-test-floors — BLOCKING CI gate: a per-package TEST-COUNT FLOOR, and "tests that RAN == tests DECLARED".
#
#   check-test-floors.sh --floors FILE [--go-run-log FILE --go-list-log FILE] [--vitest-json FILE]
#
# THE DEFECT CLASS. A green test run says nothing about HOW MANY tests it ran. Four times in this project (false-green
# classes 4, 8, 18) tests that were written, correct and passing were executed by nothing, or a check's subject was empty and
# `0 == 0` read as green. A count that is compared only with itself cannot catch a package that lost its tests; a count
# compared with a COMMITTED NONZERO FLOOR can. The floors file is that commitment: lowering a floor is a reviewed diff.
#
# INPUTS (this gate reads logs; it never runs a test, so it needs no Go toolchain and no Docker):
#   --floors FILE        one entry per line; blank lines and `#` comments ignored:
#                          go|<import path>|<floor>        e.g. go|example.com/m/internal/store|12
#                          vitest|<label>|<floor>          e.g. vitest|worker|40   (at most ONE vitest entry)
#   --go-run-log FILE    the captured text of `go test -v -count=1 ./... 2>&1`
#   --go-list-log FILE   the captured text of `go test -list '.*' ./... 2>&1`
#                        Both are REQUIRED when the floors file has any go| line (red if either is missing). They may be omitted
#                        only for a vitest-only floors file, in which case the Go sections are skipped and say so.
#   --vitest-json FILE   a vitest JSON-reporter file. REQUIRED iff the floors file has a vitest| line, and an error otherwise.
#
# WHAT IT ASSERTS, per `go|` package (each failure is its own RED line, with its own wording):
#   * the package is in the run log with at least one top-level test (else: "vanished subject")
#   * declared == ran        declared = names beginning `Test` in the list log; ran = column-0 `--- PASS|FAIL|SKIP: Test*`
#   * ran >= floor           and every floor is >= 1 (a floor of 0 would let a package that ran nothing pass)
#   * no test skipped, no test failed, no package FAIL (a build failure prints FAIL and zero test lines)
# and over the whole run:
#   * the SET of packages that ran tests == the set of `go|` entries: a new package with no floor is red ("has no floor"),
#     so is one that vanished. Duplicate floors entries, malformed lines and an empty floors file are red.
#   * the logs must contain something: no terminating `ok|FAIL|?` line, or no `--- PASS: Test` line at all (log made without
#     -v), is red ("the instrument matched nothing") — never a green with zero subjects (class 8 / 18).
# For the vitest entry: total == passed, failed == 0, pending + todo == 0, total >= floor.
#
# Attribution: `go test ./...` prints a package's tests BEFORE that package's terminating line (`ok  \t<pkg>\t0.012s`,
# `FAIL\t<pkg>\t0.012s`, `FAIL\t<pkg> [build failed]`, `?   \t<pkg>\t[no test files]`); test lines belong to the next one.
# Every input line has its `\r` stripped (a Windows checkout writes CRLF; on this very host awk and sed hide it and bash
# `read` does not, so the strip is an explicit `tr -d '\r'` and not left to whichever tool happens to read the file).
#
# WHAT IT DOES NOT CHECK — also printed on EVERY run as the `NOT CHECKED:` line, with the counts of what was skipped:
#   * a test that passes VACUOUSLY (no assertion, an empty table loop) is counted as a pass; only a mutation control finds it
#   * SUBTESTS (indented `--- PASS: TestX/sub`) are not counted; Benchmark/Fuzz/Example functions are not counted
#   * a floor is a MINIMUM: tests deleted down to the floor, but not below it, are not detected
#   * the logs and the JSON were produced by someone else: that they come from this commit and a real run is on the caller
#   * vitest test NAMES / files are not compared, only the five totals
# Paired control: scripts/check-test-floors-selftest.sh proves each RED condition fires, for the reason it names.
# Exit status: 0 green, 1 red, 2 usage error. Requires bash >= 4 (associative arrays), awk, tr and, for vitest, node.
set -u -o pipefail

usage() {
  echo "usage: check-test-floors.sh --floors FILE [--go-run-log FILE --go-list-log FILE] [--vitest-json FILE]" >&2
  exit 2
}
FLOORS=""; RUNLOG=""; LISTLOG=""; VITEST=""
while [ $# -gt 0 ]; do
  case "$1" in
    --floors)        [ $# -ge 2 ] || usage; FLOORS="$2";  shift 2;;
    --go-run-log)    [ $# -ge 2 ] || usage; RUNLOG="$2";  shift 2;;
    --go-list-log)   [ $# -ge 2 ] || usage; LISTLOG="$2"; shift 2;;
    --vitest-json)   [ $# -ge 2 ] || usage; VITEST="$2";  shift 2;;
    -h|--help)       usage;;
    *) echo "check-test-floors: unknown argument: $1" >&2; usage;;
  esac
done
[ -n "$FLOORS" ] || usage

# ---- reporting ------------------------------------------------------------------------------------------------------
REDS=()
red() { REDS+=("$*"); }
# one result row; defined here, not inside the go block, because the vitest-only path prints rows too
row() { printf '  %-44s declared=%s ran=%s pass=%s skip=%s fail=%s floor=%s\n' "$@"; }
# counts of what this run did NOT look at; "not read" until the log that yields them has been parsed
N_SUB="not read"; N_OTHER="not read"; N_NOTEST="not read"; N_ZERO="not read"
GO_SKIPPED=0

finish() {
  local r gonc
  for r in ${REDS[@]+"${REDS[@]}"}; do echo "RED: $r"; done
  if [ "$GO_SKIPPED" -eq 1 ]; then
    gonc="go test results were not read at all (no go| entry in the floors file, no go logs given)"
  else
    gonc="subtests are not counted ($N_SUB indented subtest result line(s) in the run log ignored); Benchmark/Fuzz/Example results are not counted ($N_OTHER ignored); $N_NOTEST package(s) with '[no test files]' and $N_ZERO package(s) that ran 0 tests without a floor were not examined"
  fi
  echo "NOT CHECKED: a test that passes vacuously is counted as a pass; $gonc;" \
       "a floor is a minimum, so tests deleted down to the floor are not detected; the logs/JSON are read as given, not proven to come from this commit; vitest test names are not compared"
  if [ ${#REDS[@]} -eq 0 ]; then
    echo "check-test-floors: ok — $SUMMARY"
    exit 0
  fi
  echo "check-test-floors: RED — ${#REDS[@]} problem(s)"
  exit 1
}
SUMMARY="(nothing evaluated)"

echo "check-test-floors: floors=$FLOORS run-log=$RUNLOG list-log=$LISTLOG vitest=${VITEST:-none}"

# ---- 1. the floors file ---------------------------------------------------------------------------------------------
declare -A FLOOR_GO=() FLOOR_VT=() FIRST_LINE=()
go_keys=(); vt_keys=()
if [ ! -f "$FLOORS" ]; then red "floors file missing: $FLOORS"; finish; fi
ENTRY_RE='^(go|vitest)\|([^|[:space:]]+)\|([0-9]{1,9})$'
lineno=0
while IFS= read -r l || [ -n "$l" ]; do
  lineno=$((lineno + 1))
  l="${l%%#*}"
  l="${l#"${l%%[![:space:]]*}"}"; l="${l%"${l##*[![:space:]]}"}"
  [ -n "$l" ] || continue
  if [[ "$l" =~ $ENTRY_RE ]]; then
    kind="${BASH_REMATCH[1]}"; name="${BASH_REMATCH[2]}"; fl=$((10#${BASH_REMATCH[3]}))
    key="$kind|$name"
    if [ -n "${FIRST_LINE[$key]:-}" ]; then
      red "duplicate floors entry: $key (line $lineno repeats line ${FIRST_LINE[$key]})"; continue
    fi
    FIRST_LINE[$key]=$lineno
    [ "$fl" -ge 1 ] || red "floor < 1: $key has floor $fl (line $lineno); a floor of zero lets a package that ran nothing pass"
    if [ "$kind" = go ]; then FLOOR_GO[$name]=$fl; go_keys+=("$name"); else FLOOR_VT[$name]=$fl; vt_keys+=("$name"); fi
  else
    red "malformed floors line $lineno: '$l' (want go|<import path>|<floor> or vitest|<label>|<floor>, floor a whole number)"
  fi
done < <(tr -d '\r' < "$FLOORS")
if [ $(( ${#go_keys[@]} + ${#vt_keys[@]} )) -eq 0 ]; then
  red "floors file has no entries (empty, or comments only) — a gate with nothing to check must not pass"; finish
fi
[ ${#vt_keys[@]} -le 1 ] || red "more than one vitest| entry in the floors file: only one --vitest-json result is supported"
if [ ${#vt_keys[@]} -ge 1 ] && [ -z "$VITEST" ]; then
  red "floors file has a vitest| entry but --vitest-json was not given: a vitest floor needs a result to check"
fi
if [ ${#vt_keys[@]} -eq 0 ] && [ -n "$VITEST" ]; then
  red "vitest result with no floor: --vitest-json was given but the floors file has no vitest| entry"
fi
echo "floors: ${#go_keys[@]} go package(s), ${#vt_keys[@]} vitest entry(ies)"
# The two Go logs are needed only when there is something to check them against. A floors file with NO go| line and no go logs
# (e.g. a vitest-only floors file for a web workflow) skips the Go sections; a go| line with a missing log is red, never skipped.
if [ ${#go_keys[@]} -eq 0 ] && [ -z "$RUNLOG" ] && [ -z "$LISTLOG" ]; then
  GO_SKIPPED=1; SUMMARY="go not evaluated (no go| entries, no go logs)"
elif [ -z "$RUNLOG" ] || [ -z "$LISTLOG" ]; then
  red "both --go-run-log and --go-list-log are needed (given: run-log=${RUNLOG:-none} list-log=${LISTLOG:-none}); the floors file has ${#go_keys[@]} go| entry(ies)"; finish
fi

if [ "$GO_SKIPPED" -eq 0 ]; then
  # ---- 2. the logs: must exist, be non-empty, and contain subjects ----------------------------------------------------
  if [ ! -f "$RUNLOG" ];  then red "run log missing: $RUNLOG"; finish; fi
  if [ ! -s "$RUNLOG" ];  then red "run log is empty ($RUNLOG) — the instrument matched nothing"; finish; fi
  if [ ! -f "$LISTLOG" ]; then red "list log missing: $LISTLOG"; finish; fi
  if [ ! -s "$LISTLOG" ]; then red "list log is empty ($LISTLOG) — the instrument matched nothing"; finish; fi

  # Run log -> one `R` record per terminating line, `X` records for counters. A top-level test is a COLUMN-0 `--- PASS|FAIL|SKIP:
  # Test...`; indented ones are subtests (counted only so the NOT CHECKED line can say how many were ignored).
  RUN_AWK='
  function emit(pkg, st, flag) { nterm++; printf "R\t%s\t%s\t%d\t%d\t%d\t%s\n", pkg, st, p, f, s, flag; p = 0; f = 0; s = 0 }
  BEGIN { p = 0; f = 0; s = 0; nterm = 0; nres = 0; nsub = 0; nother = 0 }
  { sub(/\r$/, "") }
  /^--- (PASS|FAIL|SKIP): Test/ { if ($2 == "PASS:") p++; else if ($2 == "FAIL:") f++; else s++; nres++; next }
  /^--- (PASS|FAIL|SKIP): /     { nother++; next }
  /^[ \t]+--- (PASS|FAIL|SKIP): / { nsub++; next }
  /^ok[ \t]+[^ \t]+[ \t]+(\(cached\)|[0-9.]+s)/ { emit($2, "ok", "-"); next }
  /^FAIL[ \t]+[^ \t]+[ \t]+([0-9.]+s|\[(build|setup) failed\])/ { emit($2, "FAIL", ($0 ~ /\[(build|setup) failed\]/) ? "buildfail" : "-"); next }
  /^\?[ \t]+[^ \t]+[ \t]+\[no test files\]/ { emit($2, "NOTEST", "-"); next }
  END { printf "X\tnterm\t%d\nX\tnres\t%d\nX\tnsub\t%d\nX\tnother\t%d\nX\ttrailing\t%d\n", nterm, nres, nsub, nother, p + f + s }
  '
  # List log -> one `L` record per terminating line: the number of names beginning `Test` printed before it.
  LIST_AWK='
  function emit(pkg, st) { nterm++; printf "L\t%s\t%s\t%d\n", pkg, st, n; n = 0 }
  BEGIN { n = 0; nterm = 0 }
  { sub(/\r$/, "") }
  /^Test[^ \t]*$/ { n++; next }
  /^ok[ \t]+[^ \t]+[ \t]+(\(cached\)|[0-9.]+s)/ { emit($2, "ok"); next }
  /^FAIL[ \t]+[^ \t]+[ \t]+([0-9.]+s|\[(build|setup) failed\])/ { emit($2, "FAIL"); next }
  /^\?[ \t]+[^ \t]+[ \t]+\[no test files\]/ { emit($2, "NOTEST"); next }
  END { printf "X\tnterm\t%d\nX\ttrailing\t%d\n", nterm, n }
  '

  declare -A RUN_ST=() RUN_P=() RUN_F=() RUN_S=() RUN_FLAG=()
  nterm=0; nres=0; trailing=0
  while IFS=$'\t' read -r k a b c d e g; do
    case "$k" in
      R) if [ -n "${RUN_ST[$a]:-}" ]; then
           red "package appears twice in the run log: $a (two logs concatenated, or a rerun appended)"
         else
           RUN_ST[$a]=$b; RUN_P[$a]=$c; RUN_F[$a]=$d; RUN_S[$a]=$e; RUN_FLAG[$a]=$g
         fi;;
      X) case "$a" in
           nterm) nterm=$b;; nres) nres=$b;; nsub) N_SUB=$b;; nother) N_OTHER=$b;; trailing) trailing=$b;;
         esac;;
    esac
  done < <(tr -d '\r' < "$RUNLOG" | awk "$RUN_AWK")

  declare -A LIST_ST=() LIST_N=()
  lnterm=0; ltrailing=0
  while IFS=$'\t' read -r k a b c; do
    case "$k" in
      L) if [ -n "${LIST_ST[$a]:-}" ]; then
           red "package appears twice in the list log: $a"
         else
           LIST_ST[$a]=$b; LIST_N[$a]=$c
         fi;;
      X) case "$a" in nterm) lnterm=$b;; trailing) ltrailing=$b;; esac;;
    esac
  done < <(tr -d '\r' < "$LISTLOG" | awk "$LIST_AWK")

  if [ "$nterm" -eq 0 ]; then
    red "no terminating ok/FAIL/? line found in the run log $RUNLOG — the instrument matched nothing (is it the output of 'go test -v ./...'?)"; finish
  fi
  if [ "$nres" -eq 0 ]; then
    red "no '--- PASS: Test...' / '--- FAIL' / '--- SKIP' line at column 0 in the run log — the instrument matched nothing (was it made without -v?)"; finish
  fi
  if [ "$lnterm" -eq 0 ]; then
    red "no terminating ok/FAIL/? line found in the list log $LISTLOG — the instrument matched nothing (is it the output of 'go test -list .* ./...'?)"; finish
  fi
  [ "$trailing" -eq 0 ]  || red "truncated run log: $trailing test result line(s) after the last terminating line belong to no package"
  [ "$ltrailing" -eq 0 ] || red "truncated list log: $ltrailing test name(s) after the last terminating line belong to no package"

  # ---- 3. per-package evaluation --------------------------------------------------------------------------------------
  tot_d=0; tot_r=0; tot_p=0; tot_s=0; tot_f=0; tot_floor=0

  sorted_go=""
  [ ${#go_keys[@]} -eq 0 ] || sorted_go="$(printf '%s\n' "${go_keys[@]}" | LC_ALL=C sort)"
  for pkg in $sorted_go; do
    fl="${FLOOR_GO[$pkg]}"; tot_floor=$((tot_floor + fl))
    st="${RUN_ST[$pkg]:-}"
    if [ -z "$st" ]; then
      row "$pkg" "-" 0 0 0 0 "$fl"
      red "vanished subject: floors package $pkg is absent from the run log (it ran nothing, or was removed — either way the floor is unmet)"; continue
    fi
    if [ "$st" = NOTEST ]; then
      row "$pkg" "-" 0 0 0 0 "$fl"
      red "vanished subject: floors package $pkg printed only '[no test files]' in the run log"; continue
    fi
    p="${RUN_P[$pkg]}"; f="${RUN_F[$pkg]}"; s="${RUN_S[$pkg]}"; ran=$((p + f + s))
    lst="${LIST_ST[$pkg]:-}"; decl="?"
    if   [ -z "$lst" ];           then red "list log missing package $pkg: it has no terminating line in the list log, so 'declared' is unknown"
    elif [ "$lst" = NOTEST ];     then red "list log missing package $pkg: the list log says '[no test files]' but the run log ran tests"
    elif [ "$lst" = FAIL ];       then red "list log missing package $pkg: 'go test -list' reported FAIL for it (build failure?), so 'declared' is unknown"
    else decl="${LIST_N[$pkg]}"; fi
    row "$pkg" "$decl" "$ran" "$p" "$s" "$f" "$fl"
    [ "$decl" = "?" ] || tot_d=$((tot_d + decl))
    tot_r=$((tot_r + ran)); tot_p=$((tot_p + p)); tot_s=$((tot_s + s)); tot_f=$((tot_f + f))

    if [ "$st" = FAIL ] && [ "$ran" -eq 0 ]; then
      red "build failure: $pkg printed FAIL with no test results (build or setup failure, or a crash before any test finished)"; continue
    fi
    if [ "$st" = FAIL ] && [ "$f" -eq 0 ]; then
      red "package FAIL without a failing test: $pkg printed FAIL but no '--- FAIL: Test' line (a panic, timeout or os.Exit mid-run)"
    fi
    [ "$f" -eq 0 ] || red "failed tests: $pkg fail=$f"
    [ "$s" -eq 0 ] || red "skipped tests: $pkg skip=$s (a skipped test is a test that did not run)"
    if [ "$decl" != "?" ] && [ "$decl" -ne "$ran" ]; then
      red "declared != ran: $pkg declared=$decl ran=$ran (a declared test never printed a result, or a result has no declaration)"
    fi
    [ "$ran" -ge "$fl" ] || red "ran < floor: $pkg ran=$ran floor=$fl"
  done

  # the SET of packages that ran tests must equal the set of floors entries
  n_notest=0; n_zero=0
  sorted_run="$(printf '%s\n' "${!RUN_ST[@]}" | LC_ALL=C sort)"
  for pkg in $sorted_run; do
    st="${RUN_ST[$pkg]}"
    [ "$st" != NOTEST ] || { n_notest=$((n_notest + 1)); continue; }
    [ -z "${FLOOR_GO[$pkg]:-}" ] || continue
    ran=$(( ${RUN_P[$pkg]} + ${RUN_F[$pkg]} + ${RUN_S[$pkg]} ))
    if [ "$ran" -gt 0 ] || [ "$st" = FAIL ]; then
      red "package has no floor: $pkg ran $ran test(s) (status $st) but is not in the floors file — add 'go|$pkg|<floor>'"
    else
      n_zero=$((n_zero + 1))
    fi
  done
  N_NOTEST=$n_notest; N_ZERO=$n_zero
  # a floors package that is [no test files] was already counted as vanished; do not double-report it as "not examined"
  for pkg in $sorted_go; do [ "${RUN_ST[$pkg]:-}" != NOTEST ] || N_NOTEST=$((N_NOTEST - 1)); done

  echo "go total: packages=${#go_keys[@]} declared=$tot_d ran=$tot_r pass=$tot_p skip=$tot_s fail=$tot_f floor-sum=$tot_floor"
  SUMMARY="${#go_keys[@]} go package(s), declared=$tot_d ran=$tot_r pass=$tot_p skip=$tot_s fail=$tot_f (floor-sum $tot_floor)"

fi

# ---- 4. vitest ------------------------------------------------------------------------------------------------------
if [ ${#vt_keys[@]} -ge 1 ] && [ -n "$VITEST" ]; then
  label="${vt_keys[0]}"; vfloor="${FLOOR_VT[$label]}"
  if [ ! -f "$VITEST" ]; then
    red "vitest JSON file missing: $VITEST"
  elif [ ! -s "$VITEST" ]; then
    red "vitest JSON file is empty ($VITEST) — the instrument matched nothing"
  else
    NODE_JS='
      const fs = require("fs");
      let j;
      try { j = JSON.parse(fs.readFileSync(0, "utf8").replace(/^\uFEFF/, "")); }
      catch (e) { console.log("ERR\tnot valid JSON: " + String(e.message).split("\n")[0]); process.exit(0); }
      const keys = ["numTotalTests", "numPassedTests", "numFailedTests", "numPendingTests", "numTodoTests"];
      const v = [];
      for (const k of keys) {
        if (typeof j !== "object" || j === null || !Number.isInteger(j[k]) || j[k] < 0) {
          console.log("ERR\tmissing field " + k + " (absent or not a non-negative integer)"); process.exit(0);
        }
        v.push(j[k]);
      }
      console.log("OK\t" + v.join("\t"));
    '
    vout="$(tr -d '\r' < "$VITEST" | node -e "$NODE_JS" 2>&1)"; vrc=$?
    if [ "$vrc" -ne 0 ]; then
      red "cannot read the vitest JSON: node failed (rc=$vrc): $vout"
    else
      IFS=$'\t' read -r vk vt vp vf vpend vtodo <<<"$vout"
      if [ "$vk" != OK ]; then
        red "vitest JSON unusable: $vt"
      else
        vskip=$((vpend + vtodo)); vran=$((vp + vf))
        row "vitest:$label" "$vt" "$vran" "$vp" "$vskip" "$vf" "$vfloor"
        [ "$vt" -eq "$vp" ]   || red "vitest total != passed: total=$vt passed=$vp"
        [ "$vf" -eq 0 ]       || red "vitest failed tests: failed=$vf"
        [ "$vskip" -eq 0 ]    || red "vitest skipped/todo tests: pending=$vpend todo=$vtodo"
        [ "$vt" -ge "$vfloor" ] || red "vitest total < floor: total=$vt floor=$vfloor"
        SUMMARY="$SUMMARY; vitest:$label total=$vt passed=$vp (floor $vfloor)"
      fi
    fi
  fi
fi

finish
