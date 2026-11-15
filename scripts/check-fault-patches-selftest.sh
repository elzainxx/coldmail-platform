#!/usr/bin/env bash
# check-fault-patches-selftest.sh — proves scripts/check-fault-patches.sh is green on a good set of fault proofs and goes RED, FOR THE REASON IT NAMES, on each way
# a fault proof can rot: a patch with no .expect (and the reverse), an invalid .expect (unknown key, missing MESSAGE, unknown SETUP token, GATE not the file name,
# missing CMD, bad TARGET), a patch that no longer applies, an empty patch, a phase directory with no .expect, an empty tree, a count under the floor.
# A reject case that went red for an unrelated reason would read as a pass forever (class 10), so every case asserts a reason substring AND the exact number of
# `RED:` lines (a fixture that also trips a second rule is caught here), and every run must print its `NOT CHECKED:` line.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-fault-patches.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=28; cases=0; ok=0; bad_expect=0
COMMIT=1

gitc() { git -c user.name=fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false -c core.autocrlf=false -c init.defaultBranch=main "$@"; }
mkrepo() { gitc -C "$1" init -q && gitc -C "$1" add -A && gitc -C "$1" commit -q -m fixture; }
mkpatch() {   # mkpatch <out> <path> <old line> <new line>: a one-line change to a one-line file
  printf 'diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-%s\n+%s\n' "$2" "$2" "$2" "$2" "$3" "$4" > "$1"
}
mkexp() {     # mkexp <file> <gate> <target>
  printf 'GATE=%s\nTARGET=%s\nSETUP=\nSTEP=data check\nCMD=bash gate.sh\nMESSAGE=gate-fail: BROKEN found\n' "$2" "$3" > "$1"
}
mk() {        # mk <name>: a fixture repository (not committed yet): platform fault p1/a and module fault p1/m, both good; prints the directory
  local d="$T/$1"
  rm -rf "$d"; mkdir -p "$d/app" "$d/ci/faults/p1"
  printf 'all good\n' > "$d/app/data.txt"
  mkpatch "$d/ci/faults/p1/a.patch" app/data.txt 'all good' 'BROKEN'
  mkexp   "$d/ci/faults/p1/a.expect" a platform
  mkpatch "$d/ci/faults/p1/m.patch" lib/x.txt 'fine' 'BROKEN'     # lib/x.txt does not exist in the platform: it must never be applied to it
  mkexp   "$d/ci/faults/p1/m.expect" m module:coldmail-verify
  echo "$d"
}

# expect <label> <want-rc> <substring> <want RED: lines | -> <fixture-dir> [gate args...]
expect() {
  local label="$1" want_rc="$2" sub="$3" nred="$4" d="$5" out rc n
  shift 5; cases=$((cases + 1))
  if [ "$COMMIT" -eq 1 ] && [ ! -e "$d/.git" ]; then mkrepo "$d" || { echo "  MISMATCH [$label]: cannot commit the fixture"; bad_expect=$((bad_expect + 1)); return; }; fi
  out="$(bash "$GATE" --root "$d" "$@" 2>&1)"; rc=$?
  if [ "$rc" -ne "$want_rc" ]; then echo "  MISMATCH [$label]: rc=$rc want=$want_rc"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect + 1)); return; fi
  if [ "$rc" -le 1 ] && ! printf '%s\n' "$out" | grep -q '^NOT CHECKED:'; then
    echo "  MISMATCH [$label]: the run did not print its NOT CHECKED: line"; bad_expect=$((bad_expect + 1)); return
  fi
  if [ "$sub" != "-" ]; then
    n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
    if [ "$n" -lt 1 ]; then echo "  MISMATCH [$label]: rc ok, but the output lacks '$sub'"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect + 1)); return; fi
  fi
  if [ "$nred" != "-" ]; then
    n=$(printf '%s\n' "$out" | grep -c '^RED:' || true)
    if [ "$n" -ne "$nred" ]; then echo "  MISMATCH [$label]: $n RED: line(s), want $nred (the fixture tripped another rule too)"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect + 1)); return; fi
  fi
  ok=$((ok + 1)); echo "  ok [$label] rc=$rc${sub:+ — ${sub}}" | sed 's/ — -$//'
}

# ---- the good set, and the ways it stays green ---------------------------------------------------------------------------------------------
d=$(mk good);                                  expect "CONTROL: a good set (one platform fault, one module fault) is green" 0 "check-fault-patches: ok — 2 fault(s)" 0 "$d"
                                               expect "the counts are printed" 0 "faults=2 platform-patches-applied=1 module-patches-not-applied=1" 0 "$d"
                                               expect "a module patch is listed under NOT CHECKED and never applied to the platform" 0 "p1/m (module:coldmail-verify)" 0 "$d"
                                               expect "the floor equal to the count is green" 0 "check-fault-patches: ok" 0 "$d" --min-faults 2
d=$(mk archive); mkdir -p "$d/ci/faults-archive/p0"; mkpatch "$d/ci/faults-archive/p0/old.patch" app/data.txt 'a line that is gone' 'x'
printf 'archive\n' > "$d/ci/faults/README.md"
                                               expect "ci/faults-archive (a patch that does not apply, no .expect) and a README beside the phases are ignored" 0 "check-fault-patches: ok — 2 fault(s)" 0 "$d"
d=$(mk crlf); sed -i 's/$/\r/' "$d"/ci/faults/p1/*.expect
[ "$(tr -cd '\r' < "$d/ci/faults/p1/a.expect" | wc -c)" -gt 0 ] || { echo "selftest: the CRLF fixture holds no CR bytes, so it would prove nothing"; exit 2; }
                                               expect "CRLF .expect files are read with the CR stripped" 0 "check-fault-patches: ok — 2 fault(s)" 0 "$d"

# ---- the rejects: each names its reason and trips exactly that one rule ----------------------------------------------------------------------
d=$(mk orphanpatch); mkpatch "$d/ci/faults/p1/orphan.patch" app/data.txt 'all good' 'BROKEN'
                                               expect "a patch without an .expect" 1 "ci/faults/p1/orphan.patch has no orphan.expect" 1 "$d"
d=$(mk lonelyexpect); mkexp "$d/ci/faults/p1/lonely.expect" lonely platform
                                               expect "an .expect without a patch" 1 "ci/faults/p1/lonely.expect has no lonely.patch" 1 "$d"
d=$(mk unkkey); printf 'FROBNICATE=1\n' >> "$d/ci/faults/p1/a.expect"
                                               expect "an unknown key" 1 "a.expect: line 7: unknown key 'FROBNICATE'" 1 "$d"
d=$(mk badgate); sed -i 's/^GATE=a$/GATE=zzz/' "$d/ci/faults/p1/a.expect"
                                               expect "GATE is not the file basename" 1 "a.expect: GATE is 'zzz' but the file name says 'a' (GATE must equal the file basename)" 1 "$d"
d=$(mk nomsg); sed -i '/^MESSAGE=/d' "$d/ci/faults/p1/a.expect"
                                               expect "no MESSAGE line" 1 "a.expect: no MESSAGE line: at least one MESSAGE is required" 1 "$d"
d=$(mk badtoken); sed -i 's/^SETUP=$/SETUP=frobnicate/' "$d/ci/faults/p1/a.expect"
                                               expect "an unknown SETUP token" 1 "a.expect: unknown SETUP token 'frobnicate'" 1 "$d"
d=$(mk nocmd); sed -i '/^CMD=/d' "$d/ci/faults/p1/a.expect"
                                               expect "no CMD" 1 "a.expect: CMD is missing or empty" 1 "$d"
d=$(mk badtarget); sed -i 's/^TARGET=platform$/TARGET=module:coldmail-nope/' "$d/ci/faults/p1/a.expect"
                                               expect "a TARGET naming a module that does not exist" 1 "a.expect: TARGET is 'module:coldmail-nope'" 1 "$d"
d=$(mk badname); for e in expect patch; do mv "$d/ci/faults/p1/a.$e" "$d/ci/faults/p1/Bad_Name.$e"; done; sed -i 's/^GATE=a$/GATE=Bad_Name/' "$d/ci/faults/p1/Bad_Name.expect"
                                               expect "a gate name the runner would refuse (upper case)" 1 "the file name 'Bad_Name.expect' is not usable" 1 "$d"
d=$(mk badphase); mv "$d/ci/faults/p1" "$d/ci/faults/Phase1"
                                               expect "a phase directory name the workflow would refuse" 1 "ci/faults/Phase1/: the phase directory name must match" 1 "$d"
d=$(mk noapply); mkpatch "$d/ci/faults/p1/a.patch" app/data.txt 'a line that is not there' 'BROKEN'
                                               expect "a platform patch that no longer applies" 1 "ci/faults/p1/a.patch does not apply to a pristine copy of HEAD" 1 "$d"
d=$(mk emptypatch); : > "$d/ci/faults/p1/a.patch"
                                               expect "an empty patch" 1 "ci/faults/p1/a.patch is empty" 1 "$d"
d=$(mk nophasefile); mkdir -p "$d/ci/faults/p2"; printf 'nothing here\n' > "$d/ci/faults/p2/notes.txt"
                                               expect "a phase directory with no .expect" 1 "ci/faults/p2/ has no .expect file" 1 "$d"
d=$(mk phasepatch); mkdir -p "$d/ci/faults/p2"; mkpatch "$d/ci/faults/p2/x.patch" app/data.txt 'all good' 'BROKEN'
                                               expect "a phase directory holding only a patch" 1 "ci/faults/p2/x.patch has no x.expect" 2 "$d"
d=$(mk floor);                                 expect "fewer faults than --min-faults" 1 "found 2 fault(s), fewer than --min-faults 3" 1 "$d" --min-faults 3
d=$(mk none); rm -rf "$d/ci";                  expect "no ci/faults at all (the instrument matched nothing)" 1 "found 0 faults: the instrument matched nothing" 1 "$d"
d=$(mk nogit); COMMIT=0;                       expect "the root is not a git repository, so no pristine copy can be made" 1 "cannot make a pristine copy" 1 "$d"; COMMIT=1

# ---- usage errors (exit 2) ------------------------------------------------------------------------------------------------------------------
d=$(mk usage)
                                               expect "usage: an unknown argument" 2 "unknown argument: --nonsense" - "$d" --nonsense
                                               expect "usage: --min-faults 0 is refused" 2 "--min-faults must be a whole number >= 1" - "$d" --min-faults 0
                                               expect "usage: --min-faults abc is refused" 2 "--min-faults must be a whole number >= 1" - "$d" --min-faults abc
cases=$((cases + 1)); out="$(bash "$GATE" --root "$T/does-not-exist" 2>&1)"; rc=$?
if [ "$rc" -eq 2 ] && printf '%s\n' "$out" | grep -q -F -- "is not a directory"; then ok=$((ok + 1)); echo "  ok [usage: --root names a missing directory] rc=2"
else echo "  MISMATCH [usage: --root names a missing directory]: rc=$rc"; bad_expect=$((bad_expect + 1)); fi

# the selftest's own control: a deliberately WRONG expectation must be reported as a mismatch (a harness that cannot disagree is not testing)
cases_before=$cases; ok_before=$ok; bad_before=$bad_expect
d=$(mk ctl);                                   expect "CONTROL (expected to mismatch): a good set wrongly expected to be red" 1 "-" - "$d" >/dev/null 2>&1
if [ "$bad_expect" -eq $((bad_before + 1)) ]; then
  bad_expect=$bad_before; cases=$cases_before; ok=$ok_before
  cases=$((cases + 1)); ok=$((ok + 1)); echo "  ok [selftest control] a wrong expectation was detected as a mismatch"
else
  echo "  MISMATCH [selftest control]: a wrong expectation was NOT detected — this selftest cannot fail"; cases=$((cases_before + 1)); bad_expect=$((bad_before + 1)); ok=$ok_before
fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — only $cases cases ran, the floor is $MIN_CASES (cases were deleted or skipped)"; exit 1; }
[ "$bad_expect" -eq 0 ] && [ "$ok" -eq "$cases" ] || exit 1
