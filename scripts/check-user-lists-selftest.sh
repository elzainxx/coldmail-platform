#!/usr/bin/env bash
# check-user-lists-selftest.sh — proves scripts/check-user-lists.sh is green on a tree that lists users only through platform.listable_users and RED, FOR THE REASON IT NAMES, on each way platform
# code can read public.users directly, and on each way its register of exceptions can rot. Every case builds a throwaway tree (a stand-in for the repository root: internal/, cmd/, db/,
# tests/user-list-exceptions.txt) in a temp directory, so it needs no database, no Go toolchain and no checkout.
# Every reject case asserts the printed REASON as well as the exit code (a case that went red for an unrelated reason would read as a pass forever: class 10). The selftest has a hard minimum
# number of cases, and a CONTROL proves it can say "no": a gate that always exits 0, a gate that fails without a reason, and two wrong expectations must each be reported as a mismatch.
set -u
export LC_ALL=C
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-user-lists.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=45; cases=0; ok=0; bad_expect=0

TAIL='SELECT from public.users — use platform.listable_users (or add a reasoned exception in tests/user-list-exceptions.txt)'
finding() { printf '%s:%s: %s' "$1" "$2" "$TAIL"; }   # finding <path> <line>
NC_HEAD='NOT CHECKED: test files (*_test.go)'

put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }   # put <dir> <relative path> < content
mk() {   # mk <name>: a fresh CLEAN tree; echoes its directory
  d="$T/$1"; rm -rf "$d"
  put "$d" internal/app/app.go <<'EOF'
package app

const list = "SELECT id, email FROM platform.listable_users ORDER BY created_at"
EOF
  put "$d" cmd/platform/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" tests/user-list-exceptions.txt <<'EOF'
# fixture register: no entries
EOF
  echo "$d"
}

# check <label> <rc> <want-rc> <subs> <output>: every @@-separated sub must appear in the output; one that starts with ! must NOT appear
check() {
  local label="$1" rc="$2" want="$3" subs="$4" out="$5" rest part neg more miss=""
  cases=$((cases+1))
  if [ "$rc" -ne "$want" ]; then
    echo "  MISMATCH [$label]: rc=$rc want=$want"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  rest="$subs"
  while :; do
    case "$rest" in *@@*) part="${rest%%@@*}"; rest="${rest#*@@}"; more=1;; *) part="$rest"; more=0;; esac
    neg=0; case "$part" in '!'*) neg=1; part="${part#!}";; esac
    if printf '%s\n' "$out" | grep -q -F -- "$part"; then [ "$neg" -eq 0 ] || miss="$miss [must NOT appear: $part]"
    else [ "$neg" -eq 1 ] || miss="$miss [missing: $part]"; fi
    [ "$more" -eq 1 ] || break
  done
  if [ -n "$miss" ]; then
    echo "  MISMATCH [$label]: rc=$rc as expected, but$miss"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  ok=$((ok+1)); echo "  ok [$label] rc=$rc"
}
# expect <label> <want-rc> <subs> <tree> [gate args replacing the default --root <tree>]
expect() {
  local label="$1" want="$2" subs="$3" d="$4" out rc; shift 4
  if [ $# -gt 0 ]; then out="$(bash "$GATE" "$@" 2>&1)"; rc=$?; else out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?; fi
  check "$label" "$rc" "$want" "$subs" "$out"
}

# ---- clean trees -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk clean);  expect "clean: users are listed through platform.listable_users" 0 "check-user-lists: OK@@scanned 2 Go file(s) and 0 SQL file(s)@@0 finding(s)@@!SELECT from public.users" "$d"
d=$(mk clean2); expect "a clean run prints its scope and what it did NOT check" 0 "scope: non-test *.go under internal/ and cmd/, and *.sql under db/ except db/migrations/@@$NC_HEAD and everything under tests/@@db/migrations/@@shell scripts@@string concatenation@@any view or function other than platform.listable_users@@Comments are NOT stripped: a comment containing 'FROM public.users' IS flagged" "$d"
d=$(mk write);  put "$d" internal/app/w.go <<'EOF'
package app

const ins = "INSERT INTO public.users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING"
const upd = "UPDATE public.users SET is_active = false WHERE id = $1"
const insSel = "INSERT INTO public.users (email) SELECT email FROM platform.pending_users"
EOF
                expect "INSERT INTO public.users and UPDATE public.users are not lists" 0 "check-user-lists: OK@@!SELECT from public.users" "$d"
d=$(mk view);   put "$d" internal/app/v.go <<'EOF'
package app

const a = `SELECT u.id FROM platform.listable_users u
	JOIN platform.balances b ON b.user_id = u.id`
const b = "SELECT count(*) FROM platform.listable_users"
EOF
                expect "platform.listable_users, alone or joined, is the sanctioned way" 0 "check-user-lists: OK" "$d"
d=$(mk whole);  put "$d" internal/app/x.go <<'EOF'
package app

const a = "SELECT id FROM public.users_archive"
const b = "SELECT id FROM public.users2 u"
const c = `SELECT id FROM "public"."users_archive"`
const d = "SELECT id FROM public.usersx"
const e = "SELECT xfrom public.users"
EOF
                expect "whole word only: public.users_archive, public.users2, \"users_archive\" and an identifier ending in 'from' pass" 0 "check-user-lists: OK" "$d"
d=$(mk unq);    put "$d" internal/app/u.go <<'EOF'
package app

const a = "SELECT id FROM users"
EOF
                expect "an unqualified FROM users is NOT flagged (a stated gap: it is named in NOT CHECKED)" 0 "check-user-lists: OK@@an unqualified FROM users (it resolves through search_path)" "$d"

# ---- the forms that must be red -----------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk sel);    put "$d" internal/app/q.go <<'EOF'
package app

const q = "SELECT id FROM public.users WHERE id = $1"
EOF
                expect "SELECT id FROM public.users names the file and the line" 1 "$(finding internal/app/q.go 3)@@check-user-lists: RED — 1 problem(s): 1 SELECT(s) from public.users outside platform.listable_users" "$d"
d=$(mk cmd);    put "$d" cmd/platform/q.go <<'EOF'
package main

import "fmt"

func list() { fmt.Println("SELECT id FROM public.users") }
EOF
                expect "a file under cmd/ is scanned too" 1 "$(finding cmd/platform/q.go 5)" "$d"
d=$(mk join);   put "$d" internal/app/q.go <<'EOF'
package app

const q = "SELECT b.id FROM platform.balances b JOIN public.users u ON u.id = b.user_id"
EOF
                expect "the JOIN form" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk lower);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "select id from public.users"
EOF
                expect "lower case" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk mixed);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "Select id FrOm PuBlIc.UsErS"
EOF
                expect "mixed case" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk raw);    put "$d" internal/app/q.go <<'EOF'
package app

const q = `
	SELECT id, email
	  FROM
		public.users u
	 WHERE u.id = $1`
EOF
                expect "split over lines in a Go raw string (FROM, newline, tabs, the table): reported at the FROM line" 1 "$(finding internal/app/q.go 5)@@!internal/app/q.go:6:" "$d"
d=$(mk crlf);   put "$d" internal/app/q.go <<'EOF'
package app

const q = `SELECT id
	FROM
		public.users`
EOF
                sed -i 's/$/\r/' "$d/internal/app/q.go"
                expect "the same split statement in a file with CRLF line endings" 1 "$(finding internal/app/q.go 4)" "$d"
d=$(mk only);   put "$d" internal/app/q.go <<'EOF'
package app

const a = "SELECT id FROM ONLY public.users"
const b = `SELECT id FROM
		only
		public.users`
EOF
                expect "FROM ONLY public.users, on one line and split over three" 1 "$(finding internal/app/q.go 3)@@$(finding internal/app/q.go 4)" "$d"
d=$(mk quoted); put "$d" internal/app/q.go <<'EOF'
package app

const a = `SELECT id FROM "public"."users"`
const b = `SELECT id FROM public."users"`
const c = `SELECT id FROM "public".users u`
const d = `SELECT id FROM public . users`
EOF
                expect "quoted and spaced identifiers: \"public\".\"users\", public.\"users\", \"public\".users, public . users" 1 "$(finding internal/app/q.go 3)@@$(finding internal/app/q.go 4)@@$(finding internal/app/q.go 5)@@$(finding internal/app/q.go 6)@@RED — 4 problem(s)" "$d"
d=$(mk escaped); put "$d" internal/app/q.go <<'EOF'
package app

const q = "SELECT id FROM \"public\".\"users\" WHERE id = $1"
EOF
                expect "quoted identifiers escaped inside a Go interpreted string" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk twice);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "SELECT 1 FROM public.users a JOIN public.users b ON a.id <> b.id"
EOF
                expect "two reads in a row on one line are both reported (a consumed boundary would hide the second)" 1 "RED — 2 problem(s): 2 SELECT(s) from public.users" "$d"
d=$(mk del);    put "$d" internal/app/q.go <<'EOF'
package app

const q = "DELETE FROM public.users WHERE id = $1"
EOF
                expect "DELETE FROM public.users matches too (the platform never deletes from it)" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk comment); put "$d" internal/app/q.go <<'EOF'
package app

// the old query was SELECT id FROM public.users, which listed the system user too
const q = 1
EOF
                expect "a comment containing FROM public.users IS flagged (comments are not stripped)" 1 "$(finding internal/app/q.go 3)" "$d"
d=$(mk sqlfile); put "$d" db/queries/list.sql <<'EOF'
-- the user list
SELECT id FROM public.users;
EOF
                expect "a .sql file under db/ outside db/migrations is scanned and flagged" 1 "scanned 2 Go file(s) and 1 SQL file(s)@@$(finding db/queries/list.sql 2)" "$d"

# ---- what is out of scope ------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk testfile); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "SELECT id FROM public.users"
EOF
                expect "a _test.go file with the pattern is ignored, and the report says tests were not checked" 0 "check-user-lists: OK@@scanned 2 Go file(s)@@$NC_HEAD" "$d"
d=$(mk migr);   put "$d" db/migrations/platform_0099_x.up.sql <<'EOF'
CREATE VIEW platform.v AS SELECT id FROM public.users;
EOF
                expect "a .sql file under db/migrations is ignored (migrations create objects), and is not counted" 0 "check-user-lists: OK@@scanned 2 Go file(s) and 0 SQL file(s)@@db/migrations/" "$d"
d=$(mk outside); put "$d" tests/harness/q.go <<'EOF'
package harness

const q = "SELECT id FROM public.users"
EOF
                put "$d" scripts/g.sh <<'EOF'
psql -c "SELECT id FROM public.users"
EOF
                expect "Go under tests/ and shell under scripts/ are outside the scope" 0 "check-user-lists: OK@@scanned 2 Go file(s) and 0 SQL file(s)" "$d"

# ---- exceptions -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
viol() { put "$1" internal/app/q.go <<'EOF'
package app

const q = "SELECT id FROM public.users"
EOF
}
d=$(mk exc);    viol "$d"; printf '# register\ninternal/app/q.go|the consistency check must see the system user too\n' > "$d/tests/user-list-exceptions.txt"
                expect "an exception entry makes the flagged file pass, and the report shows it and its reason" 0 "check-user-lists: OK@@excused: internal/app/q.go (1 match(es), exception on line 2): the consistency check must see the system user too@@1 match(es) excused by exception@@!SELECT from public.users" "$d"
d=$(mk excpart); viol "$d"; put "$d" internal/app/other.go <<'EOF'
package app

const o = "SELECT email FROM public.users"
EOF
                printf 'internal/app/q.go|the consistency check\n' > "$d/tests/user-list-exceptions.txt"
                expect "an exception covers its own file only: another flagged file is still red" 1 "$(finding internal/app/other.go 3)@@!internal/app/q.go:3:" "$d"
d=$(mk exccrlf); viol "$d"; printf '# register\r\ninternal/app/q.go | the consistency check\r\n' > "$d/tests/user-list-exceptions.txt"
                expect "a register with CRLF line endings and spaces around the bar still excuses" 0 "check-user-lists: OK@@excused: internal/app/q.go" "$d"
d=$(mk excalt); viol "$d"; printf 'internal/app/q.go|alt register\n' > "$T/alt-register.txt"
                expect "--exceptions FILE names a register elsewhere" 0 "check-user-lists: OK@@excused: internal/app/q.go (1 match(es), exception on line 1): alt register" "$d" --root "$d" --exceptions "$T/alt-register.txt"
d=$(mk stale1); printf 'internal/app/gone.go|used to select\n' > "$d/tests/user-list-exceptions.txt"
                expect "a STALE exception: the file no longer exists" 1 "tests/user-list-exceptions.txt:1: stale exception for internal/app/gone.go: the file does not exist@@RED — 1 problem(s): 0 SELECT(s) from public.users outside platform.listable_users, 1 exception problem(s)" "$d"
d=$(mk stale2); printf '# note\ninternal/app/app.go|used to select\n' > "$d/tests/user-list-exceptions.txt"
                expect "a STALE exception: the file exists but contains no match" 1 "tests/user-list-exceptions.txt:2: stale exception for internal/app/app.go: the file contains no SELECT from public.users" "$d"
d=$(mk stale3); put "$d" internal/app/q_test.go <<'EOF'
package app

const seed = "SELECT id FROM public.users"
EOF
                printf 'internal/app/q_test.go|a test\n' > "$d/tests/user-list-exceptions.txt"
                expect "an exception for a file the gate does not scan (a test file) is stale" 1 "stale exception for internal/app/q_test.go: the gate does not scan that file" "$d"
d=$(mk blank);  viol "$d"; printf 'internal/app/q.go|\n' > "$d/tests/user-list-exceptions.txt"
                expect "a blank reason is red, and excuses nothing" 1 "tests/user-list-exceptions.txt:1: exception for internal/app/q.go has a blank reason@@$(finding internal/app/q.go 3)" "$d"
d=$(mk blank2); viol "$d"; printf 'internal/app/q.go|   \t \n' > "$d/tests/user-list-exceptions.txt"
                expect "a reason of only whitespace is blank too" 1 "exception for internal/app/q.go has a blank reason" "$d"
d=$(mk nobar);  viol "$d"; printf 'internal/app/q.go\n' > "$d/tests/user-list-exceptions.txt"
                expect "an entry with no reason field at all is malformed" 1 "malformed exception line (want 'relative/path|reason'): internal/app/q.go" "$d"
d=$(mk dup);    viol "$d"; printf 'internal/app/q.go|first\ninternal/app/q.go|second\n' > "$d/tests/user-list-exceptions.txt"
                expect "the same file listed twice is red" 1 "tests/user-list-exceptions.txt:2: duplicate exception for internal/app/q.go (first on line 1)" "$d"
d=$(mk dotdot); viol "$d"; printf '../elsewhere.go|escape\n' > "$d/tests/user-list-exceptions.txt"
                expect "an exception path with .. is refused" 1 "exception path '../elsewhere.go' must be relative to the root and must not contain '..'" "$d"
d=$(mk noreg);  rm "$d/tests/user-list-exceptions.txt"
                expect "a missing register is red, not an empty one" 1 "the exceptions file $d/tests/user-list-exceptions.txt does not exist" "$d"

# ---- the floor, the arguments, the matcher's own canary ------------------------------------------------------------------------------------------------------------------------
d=$(mk nogo);   rm -rf "$d/internal" "$d/cmd"; mkdir -p "$d/internal" "$d/cmd"
                expect "no Go file at all: the gate matched nothing" 1 "scanned 0 Go file(s) and 0 SQL file(s)@@found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk onlytest); rm -rf "$d/internal" "$d/cmd"; put "$d" internal/app/a_test.go <<'EOF'
package app
EOF
                expect "only _test.go files: no Go file is scanned, so the gate matched nothing" 1 "found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk noroot); expect "a root that is not a directory" 1 "the root '$T/no-such-root' is not a directory: the instrument matched nothing" "$d" --root "$T/no-such-root"
d=$(mk usage1); expect "an unknown argument is a usage error" 2 "check-user-lists: unknown argument '--bogus'@@usage: check-user-lists.sh [--root DIR] [--exceptions FILE]" "$d" --bogus
d=$(mk usage2); expect "--root with no value is a usage error" 2 "usage: check-user-lists.sh [--root DIR] [--exceptions FILE]" "$d" --root
mkdir -p "$T/shim"; printf '#!/bin/sh\nexit 1\n' > "$T/shim/grep"; chmod +x "$T/shim/grep"
d=$(mk canary); out="$(PATH="$T/shim:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "a matcher that finds nothing (grep replaced by a stub) fails its own canary instead of passing a clean tree" "$rc" 1 "check-user-lists: RED — the matcher failed its own canary@@wanted 1 match(es), found 0@@!check-user-lists: OK" "$out"
# a grep that works for the canaries but CRASHES on one file (here: any file containing POISON; Git Bash's grep 3.0 really does abort on `-i -F`) must not read as a clean file
REAL_GREP="$(command -v grep)"; mkdir -p "$T/shim2"
printf '#!/bin/sh\nfor last; do :; done\nif "%s" -q POISON "$last" 2>/dev/null; then exit 3; fi\nexec "%s" "$@"\n' "$REAL_GREP" "$REAL_GREP" > "$T/shim2/grep"; chmod +x "$T/shim2/grep"
d=$(mk crash);  put "$d" internal/app/q.go <<'EOF'
package app

const q = "SELECT id FROM public.users" // POISON
EOF
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on one file is red and names the file (a crashed matcher prints no match: it must not read as clean)" "$rc" 1 "the matcher failed on internal/app/q.go (grep exit status 3)@@1 unreadable file(s)@@!check-user-lists: OK" "$out"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$(mk crashok)" 2>&1)"; rc=$?
                check "the same crashing-grep stub on a tree without POISON behaves like the real grep (the stub itself is not the cause)" "$rc" 0 "check-user-lists: OK@@0 unreadable file(s)" "$out"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Four deliberately WRONG arrangements, each run through the same `expect`. EVERY one must be reported as a mismatch; if any is accepted, this selftest cannot fail and says so.
#   1. a gate that always exits 0, run on a violating tree that must be red                      (the selftest must go red if the gate were `exit 0`)
#   2. a gate that fails WITHOUT naming a reason, run on a violating tree that must be red for the reason   (class 10: the right exit code for no reason)
#   3. the real gate on a clean tree, expected to be red
#   4. the real gate on a violating tree, expected to be green
printf '#!/bin/sh\nexit 0\n' > "$T/stub-pass.sh"; printf '#!/bin/sh\necho boom\nexit 1\n' > "$T/stub-bare-red.sh"
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases; real="$GATE"
d=$(mk ctlv); viol "$d"
GATE="$T/stub-pass.sh";     expect "CONTROL 1 (wrong on purpose): a gate that is always green" 1 "$(finding internal/app/q.go 3)" "$d" >/dev/null 2>&1
GATE="$T/stub-bare-red.sh"; expect "CONTROL 2 (wrong on purpose): a gate that is red for no named reason" 1 "$(finding internal/app/q.go 3)" "$d" >/dev/null 2>&1
GATE="$real"
d=$(mk ctlc); expect "CONTROL 3 (wrong on purpose): a clean tree expected to be RED" 1 "SELECT from public.users" "$d" >/dev/null 2>&1
d=$(mk ctlg); viol "$d"; expect "CONTROL 4 (wrong on purpose): a violating tree expected to be GREEN" 0 "check-user-lists: OK" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 4 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] all four deliberately wrong arrangements were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 4 wrong arrangements were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
