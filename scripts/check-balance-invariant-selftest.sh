#!/usr/bin/env bash
# check-balance-invariant-selftest.sh — proves scripts/check-balance-invariant.sh is green on empty views and RED, FOR THE REASON IT NAMES, on every way the ledger and the balances can disagree
# and on every way the instrument itself can be absent or misused. It needs no database: the offline mode is fed files, and the live and --exercise modes are fed by a FAKE psql (a shell script
# written into a temporary directory, put first on PATH or named by --psql-cmd) that records the arguments and the SQL it was given on STDIN and prints canned rows. So this proves the
# gate's argument rules, its parsing, its RED/green decisions and the shape of the exercise SQL (one transaction, always rolled back, covers the four monetary kinds); that the SQL itself runs
# and that a disabled balance trigger turns the views non-empty is proven against a real Postgres by tests/harness (TestBalance_*) and the fault-proofs leg.
# Every case asserts the REASON text (not only an exit code), and the last block is a CONTROL that proves this selftest can fail.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; G="$HERE/check-balance-invariant.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN=60; cases=0; ok=0; bad=0
U1=11111111-1111-1111-1111-111111111111; U2=22222222-2222-2222-2222-222222222222

# ---- the fake psql: records its arguments (one per line) and its stdin, then prints $FAKE_DIR/<kind>.out with exit code <kind>.rc, where <kind> is chosen by the SQL it was given ----
mkdir -p "$T/bin" "$T/emptybin" "$T/fake"
cat > "$T/bin/psql" <<'FAKE'
#!/usr/bin/env bash
D="${FAKE_DIR:?}"
printf '%s\n' "$@" > "$D/args"
cat > "$D/stdin"
if grep -q 'pg_catalog.pg_class' "$D/stdin"; then kind=views; elif grep -q '^BEGIN;' "$D/stdin"; then kind=exercise; else kind=data; fi
echo "$kind" >> "$D/kinds"
[ ! -f "$D/$kind.err" ] || cat "$D/$kind.err" >&2
[ ! -f "$D/$kind.out" ] || cat "$D/$kind.out"
rc=0; [ ! -f "$D/$kind.rc" ] || rc="$(cat "$D/$kind.rc")"
exit "$rc"
FAKE
cp "$T/bin/psql" "$T/bin/fakecmd"; chmod +x "$T/bin/psql" "$T/bin/fakecmd"

reset_fake() { rm -rf "$T/fake"; mkdir -p "$T/fake"; printf 'VIEW|balance_drift|1\nVIEW|reserve_overdrawn|1\n' > "$T/fake/views.out"; }
put() { printf '%s' "$2" > "$T/fake/$1"; }           # put <kind>.out|.err|.rc  <text>
OUT=""; RC=0
run() { OUT="$(PATH="$T/bin:$PATH" FAKE_DIR="$T/fake" bash "$G" "$@" 2>&1)"; RC=$?; }
run_nopsql() { OUT="$(PATH="$T/emptybin" FAKE_DIR="$T/fake" "$BASH" "$G" "$@" 2>&1)"; RC=$?; }

# matches <want-rc> <needle>... — true if the last run exited with want-rc, printed every needle (fixed strings) and, for rc 0 and 1, ENDED with the NOT CHECKED line
matches() {
  want="$1"; shift
  [ "$RC" -eq "$want" ] || return 1
  for n in "$@"; do printf '%s\n' "$OUT" | grep -qF -- "$n" || return 1; done
  if [ "$want" -ne 2 ]; then printf '%s\n' "$OUT" | tail -n 1 | grep -q '^NOT CHECKED: ' || return 1; fi
  return 0
}
expect() { # label want-rc needle...
  label="$1"; shift; cases=$((cases+1))
  if matches "$@"; then ok=$((ok+1)); echo "  ok [$label] rc=$RC — $2"
  else bad=$((bad+1)); echo "  MISMATCH [$label]: rc=$RC (want $1), needles: ${*:2}"; printf '%s\n' "$OUT" | sed 's/^/      | /'; fi
}
assert() { # label, then a command: ok if it succeeds
  label="$1"; shift; cases=$((cases+1))
  if "$@"; then ok=$((ok+1)); echo "  ok [$label]"; else bad=$((bad+1)); echo "  MISMATCH [$label]"; printf '%s\n' "$OUT" | sed 's/^/      | /'; fi
}
off() { printf '%s' "$1" > "$T/d"; printf '%s' "$2" > "$T/o"; run --drift "$T/d" --overdrawn "$T/o"; }   # offline run on two file bodies

# ============================ offline: the decision on the rows of the two views ============================
off "" ""
expect "both views empty: green" 0 "check-balance-invariant: OK" "0 BALANCE DRIFT row(s), 0 RESERVE OVERDRAWN row(s), 0 malformed line(s)"
off "

" "

"
expect "only blank lines: still empty, green" 0 "check-balance-invariant: OK"
off "$U1|m1|0|100|0|0
" ""
expect "a ledger sum with no balance row (stored 0, ledger 100)" 1 "BALANCE DRIFT: user $U1 module m1: stored available 0 vs ledger 100, stored reserved 0 vs ledger 0" "RED —" "1 BALANCE DRIFT row(s)"
off "$U2|m2|10|10|3|5
" ""
expect "drift in the reserved column only" 1 "BALANCE DRIFT: user $U2 module m2: stored available 10 vs ledger 10, stored reserved 3 vs ledger 5"
off "$U1|m1|5|0|0|0
" ""
expect "a balance row with no ledger rows (stored 5, ledger 0)" 1 "BALANCE DRIFT: user $U1 module m1: stored available 5 vs ledger 0"
off "$U1|m1|0|100|0|0
$U1|m2|1|2|3|4
$U2|m1|-1|0|0|0
" ""
expect "three drift rows are counted" 1 "3 BALANCE DRIFT row(s), 0 RESERVE OVERDRAWN row(s)" "module m2: stored available 1 vs ledger 2, stored reserved 3 vs ledger 4" "stored available -1 vs ledger 0"
off "" "17|$U1|m1|0a000000-0000-0000-0000-000000000001|10|15
"
expect "an overdrawn reserve names the reserve, user, module, job and both amounts" 1 "RESERVE OVERDRAWN: reserve 17 (user $U1 module m1 job 0a000000-0000-0000-0000-000000000001): reserved 10 but charges and releases settled 15" "0 BALANCE DRIFT row(s), 1 RESERVE OVERDRAWN row(s)"
off "$U1|m1|0|100|0|0
" "17|$U1|m1|j1|10|15
"
expect "both views non-empty: both reported" 1 "BALANCE DRIFT: user $U1" "RESERVE OVERDRAWN: reserve 17" "1 BALANCE DRIFT row(s), 1 RESERVE OVERDRAWN row(s)"
off "$U1|m1|0|100|0|0
" ""
assert "CRLF input: no carriage return survives into the output (the message is clean)" bash -c '! printf "%s" "$1" | grep -q "$(printf "\r")"' _ "$OUT"
printf '%s\r\n' "$U1|m1|0|100|0|0" > "$T/d"; : > "$T/o"; run --drift "$T/d" --overdrawn "$T/o"
expect "a drift file with CRLF line endings is read correctly" 1 "BALANCE DRIFT: user $U1 module m1: stored available 0 vs ledger 100, stored reserved 0 vs ledger 0"
assert "  ...and prints no carriage return" bash -c '! printf "%s" "$1" | grep -q "$(printf "\r")"' _ "$OUT"
printf '\r\n\r\n' > "$T/d"; printf '\r\n' > "$T/o"; run --drift "$T/d" --overdrawn "$T/o"
expect "CRLF blank lines are an empty view, green" 0 "check-balance-invariant: OK"
off "$U1|m1|0|100
" ""
expect "a drift line with too few fields is RED, not skipped" 1 "MALFORMED balance_drift line 1" "1 malformed line(s)"
off "$U1|m1|zero|100|0|0
" ""
expect "a drift line with a non-integer amount is RED" 1 "MALFORMED balance_drift line 1" "stored_available"
off "" "x|$U1|m1|j1|10|15
"
expect "an overdrawn line with a non-integer reserve id is RED" 1 "MALFORMED reserve_overdrawn line 1"
run --drift "$T/nope" --overdrawn "$T/o"
expect "a missing input file is RED, not an empty view" 1 "the input file $T/nope does not exist"

# ============================ usage: the argument rules (exit 2, message on stderr) ============================
run --drift "$T/d"
expect "usage: --drift without --overdrawn" 2 "needs BOTH --drift FILE and --overdrawn FILE"
run --dsn "postgres://x" --psql-cmd "psql -U x"
expect "usage: --dsn and --psql-cmd are mutually exclusive" 2 "--dsn and --psql-cmd are mutually exclusive"
run
expect "usage: no input at all" 2 "give --dsn DSN, --psql-cmd CMD, or --drift FILE --overdrawn FILE"
run --dsn "postgres://x" --drift "$T/d" --overdrawn "$T/o"
expect "usage: offline files cannot be combined with --dsn" 2 "cannot be combined with --dsn, --psql-cmd or --exercise"
run --exercise --drift "$T/d" --overdrawn "$T/o"
expect "usage: --exercise cannot be offline" 2 "cannot be combined with --dsn, --psql-cmd or --exercise"
run --exercise
expect "usage: --exercise needs a database" 2 "give --dsn DSN, --psql-cmd CMD"
run --bogus
expect "usage: an unknown argument" 2 "unknown argument '--bogus'"
run --dsn
expect "usage: --dsn without a value" 2 "--dsn needs a connection string"

# ============================ live mode: the views are read through psql ============================
reset_fake; put data.out "COUNTS|12|3
"
run --dsn "postgres://u@h/db"
expect "live, empty views: green, and says how much it compared" 0 "check-balance-invariant: OK" "12 ledger row(s) and 3 balance row(s) were compared" "[live]"
want_args="$(printf '%s\n' -X -At -F '|' -v ON_ERROR_STOP=1 'postgres://u@h/db')"
assert "live: psql gets exactly -X -At -F '|' -v ON_ERROR_STOP=1 and the DSN" test "$(cat "$T/fake/args")" = "$want_args"
assert "live: both queries ran (the existence check, then the read), and the read names both views" bash -c 'test "$(cat "$1/kinds")" = "$(printf "views\ndata")" && grep -q "FROM platform.balance_drift" "$1/stdin" && grep -q "FROM platform.reserve_overdrawn" "$1/stdin"' _ "$T/fake"
reset_fake; put data.out "COUNTS|0|0
"
run --dsn "postgres://u@h/db"
expect "live, empty ledger: green but says the check was vacuous" 0 "check-balance-invariant: OK" "WARNING — the ledger is empty, so this live check is vacuous"
reset_fake; put views.out "VIEW|balance_drift|0
VIEW|reserve_overdrawn|1
"
run --dsn "postgres://u@h/db"
expect "live: a missing balance_drift view is RED with the instrument message" 1 "found no platform.balance_drift view: the instrument matched nothing" "1 of the 2 views"
assert "  ...and does not blame the view that exists" bash -c '! printf "%s" "$1" | grep -q "found no platform.reserve_overdrawn"' _ "$OUT"
reset_fake; put views.out "VIEW|balance_drift|0
VIEW|reserve_overdrawn|0
"
run --dsn "postgres://u@h/db"
expect "live: both views missing: both named" 1 "found no platform.balance_drift view: the instrument matched nothing" "found no platform.reserve_overdrawn view: the instrument matched nothing" "2 of the 2 views"
reset_fake; put views.rc 2; put views.err "psql: error: connection to server at \"h\" failed: Connection refused
"
run --dsn "postgres://u@h/db"
expect "live: psql cannot connect: RED with psql's own words" 1 "psql failed (looking for the two views, exit 2)" "Connection refused"
reset_fake; put data.out "COUNTS|5|2
"; put data.rc 1; put data.err "ERROR:  permission denied for table ledger
"
run --dsn "postgres://u@h/db"
expect "live: the read fails (a role that cannot read the ledger): RED, not green" 1 "psql failed (reading the views, exit 1)" "permission denied for table ledger"
reset_fake; put data.out "COUNTS|40|6
DRIFT|$U1|m1|90|100|0|0
"
run --dsn "postgres://u@h/db"
expect "live: a drift row is RED and names user, module, stored and ledger" 1 "BALANCE DRIFT: user $U1 module m1: stored available 90 vs ledger 100, stored reserved 0 vs ledger 0" "[live]"
reset_fake; put data.out "COUNTS|40|6
OVERDRAWN|9|$U2|m2|j2|3|4
"
run --dsn "postgres://u@h/db"
expect "live: an overdrawn reserve is RED" 1 "RESERVE OVERDRAWN: reserve 9 (user $U2 module m2 job j2): reserved 3 but charges and releases settled 4"
reset_fake
run --dsn "postgres://u@h/db"
expect "live: a read that printed no COUNTS row is RED (the instrument matched nothing)" 1 "the read printed no COUNTS row"
reset_fake; put data.out "COUNTS|1|1
"
run_nopsql --dsn "postgres://u@h/db"
expect "live: no psql on PATH is RED, not green" 1 "psql is not on PATH"
reset_fake; put data.out "COUNTS|8|2
"
run --psql-cmd "$T/bin/fakecmd --via-container"
expect "--psql-cmd: runs the prefix, green on empty views" 0 "check-balance-invariant: OK" "8 ledger row(s) and 2 balance row(s) were compared"
want_args="$(printf '%s\n' --via-container -X -At -F '|' -v ON_ERROR_STOP=1)"
assert "--psql-cmd: the prefix's own argument comes first, then the standard flags, and no DSN" test "$(cat "$T/fake/args")" = "$want_args"
assert "--psql-cmd: the SQL arrived on STDIN" grep -q "FROM platform.balance_drift" "$T/fake/stdin"
run --psql-cmd "no-such-command-xyz -U postgres"
expect "--psql-cmd: a command that is not on PATH is RED" 1 "--psql-cmd starts with 'no-such-command-xyz', which is not on PATH"
reset_fake; put data.out "COUNTS|8|2
DRIFT|$U1|m1|0|100|0|0
"
run --psql-cmd "$T/bin/fakecmd"
expect "--psql-cmd: a drift row is RED the same way" 1 "BALANCE DRIFT: user $U1 module m1: stored available 0 vs ledger 100"

# ============================ --exercise: fixtures written through the ledger, then the views ============================
reset_fake; put exercise.out "FIXTURE|$U1|$U2|0a000000-0000-0000-0000-000000000001
EXERCISED|6
BALANCE|993|0
ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise, views empty: green, and says how many ledger rows it exercised" 0 "check-balance-invariant: OK" "exercised 6 ledger row(s)" "rolled back" "[live exercise]" "available|reserved = 993|0"
assert "exercise: the SQL runs as ONE transaction that is rolled back: starts BEGIN;, has ROLLBACK;, has no COMMIT" bash -c 'f="$1/stdin"; [ "$(head -n 1 "$f")" = "BEGIN;" ] && grep -q "^ROLLBACK;" "$f" && ! grep -Eiq "(^|[^A-Za-z_])(COMMIT|END)[; ]" "$f"' _ "$T/fake"
assert "exercise: the existence check ran first, then the exercise, and nothing else" test "$(cat "$T/fake/kinds")" = "$(printf 'views\nexercise')"
reset_fake; put exercise.out "EXERCISED|6
BALANCE|-1|-1
DRIFT|$U1|m1|0|1000|0|0
ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise, balance trigger disabled (no balance row, ledger 1000): RED, naming the stored and ledger values" 1 "BALANCE DRIFT: user $U1 module m1: stored available 0 vs ledger 1000, stored reserved 0 vs ledger 0" "[live exercise]"
reset_fake; put exercise.out "EXERCISED|6
BALANCE|90|10
OVERDRAWN|3|$U1|m1|j1|10|12
ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise: an overdrawn reserve in the fixtures is RED" 1 "RESERVE OVERDRAWN: reserve 3"
reset_fake; put exercise.out "EXERCISED|3
ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise that exercised only 3 ledger rows: RED, an exercise that exercised nothing" 1 "the exercise wrote only 3 ledger row(s) (floor 5)" "an exercise that exercised nothing proves nothing"
reset_fake; put exercise.out "EXERCISED|5
ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise at the floor (5 rows) is accepted" 0 "exercised 5 ledger row(s)"
reset_fake; put exercise.out "ROLLED_BACK
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise that printed no row count: RED" 1 "the exercise printed no ledger-row count"
reset_fake; put exercise.out "EXERCISED|6
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise whose ROLLBACK marker never appeared: RED" 1 "the exercise never printed ROLLED_BACK"
reset_fake; put exercise.out "FIXTURE|x|y|z
"; put exercise.rc 3; put exercise.err "ERROR:  insert or update on table \"ledger\" violates foreign key constraint \"ledger_job_fk\"
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise whose SQL fails midway: RED with psql's error" 1 "psql failed (the exercise, which was rolled back with its connection, exit 3)" "ledger_job_fk"
reset_fake; put views.out "VIEW|balance_drift|1
VIEW|reserve_overdrawn|0
"
run --dsn "postgres://u@h/db" --exercise
expect "exercise against a database with a missing view: RED before anything is written" 1 "found no platform.reserve_overdrawn view: the instrument matched nothing"
assert "  ...and the exercise SQL was never sent" test "$(cat "$T/fake/kinds")" = "views"
reset_fake; put exercise.out "EXERCISED|6
ROLLED_BACK
"
run --psql-cmd "$T/bin/fakecmd --via-container" --exercise
expect "exercise through --psql-cmd: the whole script goes on STDIN" 0 "exercised 6 ledger row(s)"
assert "  ...the first line on stdin is BEGIN;" test "$(head -n 1 "$T/fake/stdin")" = "BEGIN;"

# ============================ the exercise SQL itself (static) ============================
run --print-exercise-sql
assert "print-exercise-sql: exits 0 and prints a transaction: BEGIN; first, ROLLBACK; present, no COMMIT anywhere" bash -c 'test "$2" -eq 0 && printf "%s\n" "$1" | head -n 1 | grep -qx "BEGIN;" && printf "%s\n" "$1" | grep -qx "ROLLBACK;" && ! printf "%s\n" "$1" | grep -Eiq "(^|[^A-Za-z_])COMMIT"' _ "$OUT" "$RC"
assert "print-exercise-sql: it writes all four monetary kinds through the ledger (grant, reserve, charge, release)" bash -c 'for k in grant reserve charge release; do printf "%s\n" "$1" | grep -q "INSERT INTO platform.ledger" && printf "%s\n" "$1" | grep -q "'"'"'$k'"'"'" || exit 1; done' _ "$OUT"
assert "print-exercise-sql: it reads both views, restricted to its own fixture user" bash -c 'printf "%s\n" "$1" | grep -q "FROM platform.balance_drift WHERE user_id" && printf "%s\n" "$1" | grep -q "FROM platform.reserve_overdrawn WHERE user_id"' _ "$OUT"
assert "print-exercise-sql: its final statement proves the ROLLBACK ran (SELECT 'ROLLED_BACK')" bash -c 'test "$(printf "%s\n" "$1" | tail -n 1)" = "SELECT '"'"'ROLLED_BACK'"'"';"' _ "$OUT"

# ============================ the CONTROL: this selftest can fail ============================
# `matches` is what every case above stands on. If it could not say "no", the whole file would be green whatever the gate did. Feed it outputs it MUST reject.
off "$U1|m1|0|100|0|0
" ""                                                         # a RED run (rc 1, "BALANCE DRIFT")
cases=$((cases+1)); if ! matches 0 "check-balance-invariant: OK"; then ok=$((ok+1)); echo "  ok [control: a RED run is NOT accepted as green]"; else bad=$((bad+1)); echo "  MISMATCH [control]: matches() accepted a RED run as green: this selftest cannot fail"; fi
cases=$((cases+1)); if ! matches 1 "RESERVE OVERDRAWN: reserve"; then ok=$((ok+1)); echo "  ok [control: rc 1 with the WRONG reason is NOT accepted]"; else bad=$((bad+1)); echo "  MISMATCH [control]: matches() accepted the wrong reason: this selftest cannot fail"; fi
off "" ""                                                    # a green run
cases=$((cases+1)); if ! matches 1 "BALANCE DRIFT"; then ok=$((ok+1)); echo "  ok [control: a green run is NOT accepted as RED]"; else bad=$((bad+1)); echo "  MISMATCH [control]: matches() accepted a green run as RED: this selftest cannot fail"; fi
OUT="$OUT
a line after the NOT CHECKED line"
cases=$((cases+1)); if ! matches 0 "check-balance-invariant: OK"; then ok=$((ok+1)); echo "  ok [control: output that does not END with NOT CHECKED is NOT accepted]"; else bad=$((bad+1)); echo "  MISMATCH [control]: matches() ignored the last-line rule"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
