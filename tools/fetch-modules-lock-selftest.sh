#!/usr/bin/env bash
# fetch-modules-lock-selftest.sh — proves (1) tools/fetch-modules.sh REFUSES a bad lock file before it clones anything, and says which fault it found, and
# (2) its ANONYMOUS route (a module whose deploy-key variable is empty is cloned with no credentials) really clones, checks out the pin and asserts HEAD == pin, and is red,
# for the reason it names, when the pin is absent, not on the default branch, or the route has nothing to clone; and that a supplied-but-unusable key is never replaced by that route.
# Needs no network and no deploy keys: lock validation runs first and exits non-zero, so the clone loop is never reached; the anonymous cases clone from three LOCAL bare
# repositories (FETCH_ANON_BASE=file://...) with the key variables removed from the environment.
# NOT CHECKED here (it needs the real repositories on GitHub, so it is proven on the runner by the harness and platform-check workflows): the deploy-key route (SSH clone, the read-only
# probe), the anonymous route against github.com over https, and a repository that is private (which answers an anonymous clone with an authentication failure, not a missing path).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; F="$HERE/fetch-modules.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
A=abcdef1234abcdef1234abcdef1234abcdef1234; B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; C=cccccccccccccccccccccccccccccccccccccccc   # A has letters, so its upper-case form really differs
MIN=14; cases=0; ok=0; bad=0
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
# ---- the ANONYMOUS route: a module whose deploy-key variable is EMPTY is cloned with no credentials ---------------------------------------------------------------------------
# Proved against three LOCAL bare repositories (FETCH_ANON_BASE=file://...), so it needs no network and no key; every case runs with the three key variables REMOVED from the environment
# (the harness workflow runs this file with them set). The first case is the paired control that the clones, the pin checkout and the HEAD == pin assertion all really happen on this
# route, so the reject cases after it are red for the fault they name and not for a broken fixture; every reject case asserts the REASON it fails for.
OWN=selftest-owner; SRC="$T/src"; REM="$T/remote"; mkdir -p "$SRC" "$REM/$OWN"
pathfix() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }                       # a native git on Windows cannot read an MSYS path inside a file:// URL
RP="$(pathfix "$REM")"; case "$RP" in /*) BASE="file://$RP";; *) BASE="file:///$RP";; esac
mkrepo() {   # <name> — two commits on main, and a bare copy that a clone can read
  ( set -e; rm -rf "${SRC:?}/$1"; git init -q -b main "$SRC/$1"; cd "$SRC/$1"; git config user.email selftest@example.invalid; git config user.name selftest; git config commit.gpgsign false
    echo "one $1" > f; git add f; git commit -q -m one; echo "two $1" > f; git commit -q -a -m two
    git clone -q --bare "$SRC/$1" "$REM/$OWN/$1.git" ) >/dev/null 2>&1
}
setup_ok=0; for r in coldmail-verify coldmail-send-managed coldmail-send-byo; do mkrepo "$r" || setup_ok=1; done
FV="$(git -C "$SRC/coldmail-verify" rev-list --max-parents=0 HEAD 2>/dev/null)"; FM="$(git -C "$SRC/coldmail-send-managed" rev-list --max-parents=0 HEAD 2>/dev/null)"; FB="$(git -C "$SRC/coldmail-send-byo" rev-list --max-parents=0 HEAD 2>/dev/null)"
TV="$(git -C "$SRC/coldmail-verify" rev-parse HEAD 2>/dev/null)"
if [ "$setup_ok" -ne 0 ] || [ -z "$FV" ] || [ -z "$FM" ] || [ -z "$FB" ] || [ "$FV" = "$TV" ]; then echo "selftest: RED — could not build the three local fixture repositories"; exit 1; fi
# the three fixtures must have DISTINCT commit ids: a fast machine commits all three in the same second, and identical content + author + time = identical ids (a run on a GitHub runner proved it), which makes every "count the repository that is pinned at X" assertion count all three
if [ "$FV" = "$FM" ] || [ "$FV" = "$FB" ] || [ "$FM" = "$FB" ]; then echo "selftest: RED — the three fixture repositories share a first commit id: the pin assertions would be vacuous"; exit 1; fi
anon() {   # <lock text, printf %b> [VAR=value ...] — fetch with the three keys REMOVED, the anonymous base on the local bare repositories, and the lock; sets out and rc
  lock "$1"; shift; rm -rf "$T/dest"
  out="$(env -u DEPLOY_KEY_COLDMAIL_VERIFY -u DEPLOY_KEY_COLDMAIL_SEND_MANAGED -u DEPLOY_KEY_COLDMAIL_SEND_BYO FETCH_OWNER="$OWN" FETCH_ANON_BASE="$BASE" "$@" bash "$F" --lock "$T/lock" "$T/dest" 2>&1)"; rc=$?
}
count() { printf '%s\n' "$out" | grep -c -F -- "$1" || true; }                         # lines of the output that carry the text
countf() { printf '%s\n' "$out" | grep -F -- 'FAIL: ' | grep -c -F -- "$1" || true; }   # FAIL lines only (every failure is also echoed once more as a ::error line)
verdict() {   # <label> <0 if it behaved as expected> <what was wanted>
  cases=$((cases+1))
  if [ "$2" -eq 0 ]; then ok=$((ok+1)); echo "  ok [$1] — $3"; else bad=$((bad+1)); echo "  MISMATCH [$1]: wanted $3 (rc=$rc)"; printf '%s\n' "$out" | sed 's/^/      | /'; fi
}
GOODLOCK="coldmail-verify $FV\ncoldmail-send-managed $FM\ncoldmail-send-byo $FB\n"
# control: every key empty, every repository readable -> three anonymous clones, each checked out at the pin (the FIRST commit, not the branch tip), HEAD == pin, exit 0, no key route taken
anon "$GOODLOCK"
[ "$rc" -eq 0 ] && [ "$(count 'anonymous clone, no deploy key')" -eq 3 ] && [ "$(count "PINNED at $FV == HEAD")" -eq 1 ] && [ "$(git -C "$T/dest/coldmail-verify" rev-parse HEAD 2>/dev/null)" = "$FV" ] && [ "$(count 'confirmed read-only')" -eq 0 ]
verdict "anonymous route: three clones, each checked out at its pin" $? "exit 0, three 'anonymous clone' lines, HEAD == the pin (not the tip), no 'confirmed read-only' (no key route)"
# the same fetch without --lock (what platform-check wants): the default branch's head, still three anonymous clones
rm -rf "$T/dest"; out="$(env -u DEPLOY_KEY_COLDMAIL_VERIFY -u DEPLOY_KEY_COLDMAIL_SEND_MANAGED -u DEPLOY_KEY_COLDMAIL_SEND_BYO FETCH_OWNER="$OWN" FETCH_ANON_BASE="$BASE" bash "$F" "$T/dest" 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && [ "$(count 'anonymous clone, no deploy key')" -eq 3 ] && [ "$(git -C "$T/dest/coldmail-verify" rev-parse HEAD 2>/dev/null)" = "$TV" ]
verdict "anonymous route without --lock: the default branch's head" $? "exit 0, three anonymous clones, HEAD == the branch tip"
# a pin that is not in the repository is red on this route too, and the other repositories still clone (the red is that repository's, not the fixture's)
anon "coldmail-verify $B\ncoldmail-send-managed $FM\ncoldmail-send-byo $FB\n"
[ "$rc" -ne 0 ] && [ "$(countf "coldmail-verify: the pinned commit $B does not exist in the repository")" -eq 1 ] && [ "$(count 'ok: coldmail-send-managed')" -eq 1 ]
verdict "anonymous route: a pin that does not exist" $? "non-zero, a FAIL line naming coldmail-verify and the missing pin, the other two still cloned"
# the route has no answer (a base that holds nothing): red for EACH repository, naming the anonymous clone
anon "$GOODLOCK" FETCH_ANON_BASE=file:///zz-selftest-no-such-directory
[ "$rc" -ne 0 ] && [ "$(countf 'could not clone anonymously')" -eq 3 ] && [ "$(count '  ok: ')" -eq 0 ]
verdict "anonymous route: nothing to clone" $? "non-zero, three FAIL lines 'could not clone anonymously' and no 'ok:' line"
# a key that IS supplied but unusable is NEVER replaced by the anonymous route (a revoked key must not turn green because the repository happens to be readable)
anon "$GOODLOCK" "DEPLOY_KEY_COLDMAIL_SEND_BYO=this is not a key"
[ "$rc" -ne 0 ] && [ "$(countf 'coldmail-send-byo: the value of DEPLOY_KEY_COLDMAIL_SEND_BYO is not a usable private key')" -eq 1 ] && [ "$(count 'ok: coldmail-send-byo')" -eq 0 ] && [ "$(count 'ok: coldmail-verify')" -eq 1 ] && [ ! -e "$T/dest/coldmail-send-byo" ]
verdict "a supplied malformed key does not fall back to the anonymous route" $? "non-zero, 'not a usable private key' for coldmail-send-byo, no clone of it, the two empty-key repositories still anonymous"
# a pin on a side branch only is not reachable from the default branch (red on this route too); the side branch is created last so no case above sees it
SIDE="$( set -e; cd "$SRC/coldmail-send-byo"; git checkout -q -b side; echo side > g; git add g; git commit -q -m side; git rev-parse HEAD )"
git -C "$SRC/coldmail-send-byo" push -q "$REM/$OWN/coldmail-send-byo.git" side >/dev/null 2>&1
anon "coldmail-verify $FV\ncoldmail-send-managed $FM\ncoldmail-send-byo $SIDE\n"
[ -n "$SIDE" ] && [ "$rc" -ne 0 ] && [ "$(countf "coldmail-send-byo: the pinned commit $SIDE is not reachable from the default branch")" -eq 1 ]
verdict "anonymous route: a pin that is not on the default branch" $? "non-zero, a FAIL line naming coldmail-send-byo and 'not reachable from the default branch'"
echo "selftest: $cases cases, $ok behaved as expected, $bad did not"
[ "$cases" -ge "$MIN" ] || { echo "selftest: RED — $cases cases ran, floor is $MIN"; exit 1; }
[ "$bad" -eq 0 ]
