#!/usr/bin/env bash
# check-license-selftest.sh — proves scripts/check-license.sh is green on a right repository and goes red, FOR THE REASON IT NAMES, on every way the LICENSE / NOTICE / README can be wrong.
# Every case builds a throwaway root, runs the real gate on it and asserts the exit code AND fragments of the printed reason (class 10: a reject case that fails for another reason is no proof).
# A hard minimum number of cases, and controls that prove THIS file can fail: a gate that always passes, one that always fails, one that fails for the wrong reason, and three copies of the
# REAL gate with one check removed each (the matching case must then misbehave).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-license.sh"; CANON_REAL="$HERE/../tests/license-canonical.txt"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
MIN_CASES=40
n=0; bad=0
t() { n=$((n+1)); }
S1="The source is visible for transparency only."
S2="No license is granted to use, copy, modify, distribute or run this software, in whole or in part."
LINE="Source visible for transparency only; all rights reserved (see LICENSE and NOTICE)."
CANON="$TMP/canon.txt"; cp "$CANON_REAL" "$CANON" || { echo "check-license-selftest: RED — cannot read $CANON_REAL"; exit 1; }
CANON_TEXT="$(cat "$CANON")"

good_root() { # good_root DIR — a root the gate accepts
  d="$1"; rm -rf "$d"; mkdir -p "$d"
  cp "$CANON" "$d/LICENSE"
  { printf '%s\n\n' "$CANON_TEXT"; printf 'Third-Party Notices\n===================\n\n  github.com/example/dep  v1.0.0  MIT  - an unmodified dependency\n'; } > "$d/NOTICE"
  { printf '# A repository\n\n%s\n\nMore text.\n' "$LINE"; } > "$d/README.md"
}

ARGS=(); OUT=""
# expect NAME WANT_RC FRAGMENT...   (the gate is run with the global ARGS; each fragment must appear in the output)
expect() {
  t; name="$1"; want="$2"; shift 2
  OUT="$(bash "$GATE" ${ARGS[@]+"${ARGS[@]}"} 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  for frag in "$@"; do
    if ! printf '%s\n' "$OUT" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc as expected but the output lacks '$frag'"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  done
  echo "  ok   $name"
}
# absent NAME FRAGMENT: the output of the last expect must NOT carry the fragment (a reason that must not be reported too)
absent() {
  t
  if printf '%s\n' "$OUT" | grep -qF -- "$2"; then echo "  FAIL $1: the output carries '$2'"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); else echo "  ok   $1"; fi
}

D="$TMP/r"; D2="$TMP/r2"
# ---- green ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; ARGS=(--canonical "$CANON" --root "$D")
expect "a right root is green" 0 "check-license: OK" "check-license: 1 root(s), 1 passed, 0 problem(s)" "LICENSE equals the canonical text (189 bytes)"
expect "the green run prints what it did not check" 0 "NOT CHECKED: that the LICENSE wording is the wording the owner ruled on" "the LICENSE files of vendored third-party code"
good_root "$D"; good_root "$D2"; ARGS=(--canonical "$CANON" --root "$D" --root "$D2")
expect "two right roots are green and both are counted" 0 "check-license: OK" "check-license: 2 root(s), 2 passed, 0 problem(s)"
# the canonical text is found beside the script when --canonical is not given
mkdir -p "$TMP/plat/scripts" "$TMP/plat/tests"; cp "$GATE" "$TMP/plat/scripts/check-license.sh"; cp "$CANON" "$TMP/plat/tests/license-canonical.txt"
good_root "$D"; t
out="$(bash "$TMP/plat/scripts/check-license.sh" --root "$D" 2>&1)"; rc=$?
if [ "$rc" = 0 ] && printf '%s' "$out" | grep -qF "check-license: OK"; then echo "  ok   the canonical text is read from tests/ beside the script when --canonical is not given"; else echo "  FAIL default canonical path: exit $rc"; printf '%s\n' "$out" | sed 's/^/     | /'; bad=$((bad+1)); fi
good_root "$D"; sed -i 's/$/\r/' "$D/NOTICE" "$D/README.md"; ARGS=(--canonical "$CANON" --root "$D")
expect "a NOTICE and a README with CRLF line endings are green (CR stripped; only LICENSE is byte for byte)" 0 "check-license: OK"
good_root "$D"; printf 'Our confidentiality of keys rests on AES-GCM (the encryption property, not the licence).\n' >> "$D/NOTICE"
expect "'confidentiality' is another word: a technical use stays" 0 "check-license: OK"
good_root "$D"; { printf 'Copyright (c) 2026 Zain. All rights reserved. The source is visible\nfor transparency only. No license is granted to use, copy, modify, distribute\nor run this software, in whole or in part.\n\nThird-Party Notices\n'; } > "$D/NOTICE"
expect "a first paragraph wrapped over three lines still carries both sentences" 0 "check-license: OK"
good_root "$D"; { printf '%s\n%s\n\nThird-Party Notices\n' "$S1" "$S2"; } > "$D/NOTICE"
expect "a first paragraph of just the two sentences is green" 0 "check-license: OK"

# ---- the instrument is given nothing ------------------------------------------------------------------------------------------------------------------------------------------------
ARGS=(--canonical "$CANON")
expect "no root at all: the subject is empty" 1 "found 0 roots"
ARGS=(--canonical "$CANON" --root "$TMP/does-not-exist")
expect "a root that is not a directory" 1 "$TMP/does-not-exist: the root is not a directory"
good_root "$D"; ARGS=(--canonical "$CANON" --root "$D" --root "$D/.")
expect "a root listed twice (once spelled differently)" 1 "the root is listed twice"

# ---- LICENSE -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ARGS=(--canonical "$CANON" --root "$D")
good_root "$D"; rm "$D/LICENSE"; expect "no LICENSE" 1 "$D: LICENSE does not exist"
good_root "$D"; : > "$D/LICENSE"; expect "an empty LICENSE" 1 "LICENSE is empty: the gate matched nothing"
good_root "$D"; sed -i 's/visible/hidden/' "$D/LICENSE"
expect "one word of LICENSE changed: the difference is named, with both texts" 1 "LICENSE differs from tests/license-canonical.txt: first difference at line 1" "LICENSE  : Copyright (c) 2026 Zain. All rights reserved. The source is hidden for transparency only." "canonical: Copyright (c) 2026 Zain. All rights reserved. The source is visible for transparency only."
good_root "$D"; sed -i 's/$/\r/' "$D/LICENSE"
expect "LICENSE with CRLF line endings" 1 "LICENSE contains a carriage return (1 of them: CRLF line endings)"
absent "CRLF alone is not reported as a wording difference too" "LICENSE differs from"
good_root "$D"; printf '%s' "$CANON_TEXT" > "$D/LICENSE"
expect "LICENSE without its final newline" 1 "LICENSE differs from tests/license-canonical.txt in its final newline only"
good_root "$D"; printf '\nAn extra paragraph.\n' >> "$D/LICENSE"
expect "LICENSE with a paragraph after the canonical one (the old shape)" 1 "first difference at line 2" "<the canonical text ends before this line>"
good_root "$D"; printf '\n%s\n' "$CANON_TEXT" > "$D/LICENSE"
expect "LICENSE with a blank line before the text" 1 "first difference at line 1" "LICENSE  : "
good_root "$D"; printf 'Copyright (c) 2026 The module owner. All rights reserved.\n\nPROPRIETARY AND CONFIDENTIAL.\n\nThis software is the property of the owner.\n' > "$D/LICENSE"
expect "the old wording: it differs AND the word confidential is named with its line" 1 "LICENSE differs from tests/license-canonical.txt" "LICENSE line 3 contains the word 'confidential'"

# ---- the word 'confidential' ------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; printf 'The code is Confidential and proprietary.\n' >> "$D/NOTICE"
expect "NOTICE says Confidential (any case), the line is named" 1 "NOTICE line 7 contains the word 'confidential'"
good_root "$D"; printf 'Treated as strictly CONFIDENTIAL.\n' >> "$D/NOTICE"
expect "NOTICE says CONFIDENTIAL in capitals" 1 "NOTICE line 7 contains the word 'confidential'"

# ---- NOTICE ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; rm "$D/NOTICE"; expect "no NOTICE" 1 "NOTICE does not exist"
good_root "$D"; : > "$D/NOTICE"; expect "an empty NOTICE" 1 "NOTICE is empty: the gate matched nothing"
good_root "$D"; printf '\n\n   \n' > "$D/NOTICE"; expect "a NOTICE of blank lines only" 1 "NOTICE has no paragraph"
good_root "$D"; { printf 'Source visible for transparency. All rights reserved. No license is granted to use, copy, modify or distribute.\n\nThird-Party Notices\n'; } > "$D/NOTICE"
expect "the old one-line NOTICE lacks both sentences" 1 "NOTICE's first paragraph lacks the LICENSE sentence '$S1'" "NOTICE's first paragraph lacks the LICENSE sentence '$S2'"
good_root "$D"; { printf 'Copyright (c) 2026 Zain. All rights reserved. %s\n\nThird-Party Notices\n' "$S2"; } > "$D/NOTICE"
expect "a first paragraph with only the second sentence" 1 "lacks the LICENSE sentence '$S1'"
absent "and it does not claim the second sentence is missing" "lacks the LICENSE sentence '$S2'"
good_root "$D"; { printf 'Copyright (c) 2026 Zain. All rights reserved. %s\n\nThird-Party Notices\n' "$S1"; } > "$D/NOTICE"
expect "a first paragraph with only the first sentence" 1 "lacks the LICENSE sentence '$S2'"
good_root "$D"; { printf 'Third-Party Notices\n\n%s\n%s\n' "$S1" "$S2"; } > "$D/NOTICE"
expect "both sentences only in a later paragraph: the FIRST paragraph is what is read" 1 "lacks the LICENSE sentence '$S1'" "lacks the LICENSE sentence '$S2'"

# ---- README ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; rm "$D/README.md"; expect "no README.md" 1 "README.md does not exist"
good_root "$D"; : > "$D/README.md"; expect "an empty README.md" 1 "README.md is empty: the gate matched nothing"
good_root "$D"; printf '# A repository\n\nSource visible for transparency; no license is granted. See [NOTICE](NOTICE).\n' > "$D/README.md"
expect "a README with the old notice wording" 1 "README.md has no line that is exactly: $LINE"
good_root "$D"; printf '# A repository\n\n> %s\n' "$LINE" > "$D/README.md"
expect "the notice line inside a longer line (a quote marker in front) is not the line" 1 "README.md has no line that is exactly"
good_root "$D"; printf '# A repository\n\n%s \n' "$LINE" > "$D/README.md"
expect "the notice line with a trailing space is not the line" 1 "README.md has no line that is exactly"

# ---- more than one root ------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; good_root "$D2"; sed -i 's/transparency only\./transparency./' "$D2/LICENSE"; ARGS=(--canonical "$CANON" --root "$D" --root "$D2")
expect "the second root is bad, the first good: the good one is reported ok, the bad one is named" 1 "  ok   $D: LICENSE equals the canonical text" "$D2: LICENSE differs from" "check-license: 2 root(s), 1 passed, 1 problem(s)"

# ---- the canonical text itself ------------------------------------------------------------------------------------------------------------------------------------------------------------
good_root "$D"; ARGS=(--canonical "$TMP/missing.txt" --root "$D"); expect "no canonical text" 1 "the canonical text tests/license-canonical.txt does not exist"
: > "$TMP/c-empty.txt"; ARGS=(--canonical "$TMP/c-empty.txt" --root "$D"); expect "an empty canonical text" 1 "the canonical text tests/license-canonical.txt is empty"
sed 's/$/\r/' "$CANON" > "$TMP/c-crlf.txt"; ARGS=(--canonical "$TMP/c-crlf.txt" --root "$D"); expect "a canonical text with CRLF" 1 "tests/license-canonical.txt: the canonical text contains a carriage return"
{ cat "$CANON"; printf 'A second line.\n'; } > "$TMP/c-two.txt"; ARGS=(--canonical "$TMP/c-two.txt" --root "$D"); expect "a canonical text of two lines" 1 "the canonical text is not one line"
printf '%s' "$CANON_TEXT" > "$TMP/c-nonl.txt"; ARGS=(--canonical "$TMP/c-nonl.txt" --root "$D"); expect "a canonical text without a final newline" 1 "the canonical text does not end with a newline"
printf 'Copyright (c) 2026 Zain. All rights reserved. The source is visible for transparency only.\n' > "$TMP/c-short.txt"; ARGS=(--canonical "$TMP/c-short.txt" --root "$D")
expect "a canonical text that lacks the second sentence (NOTICE could never be held to it)" 1 "the canonical text lacks the sentence the gate requires of NOTICE: '$S2'"
printf 'Copyright (c) 2026 Zain. All rights reserved. Confidential. %s %s\n' "$S1" "$S2" > "$TMP/c-conf.txt"; ARGS=(--canonical "$TMP/c-conf.txt" --root "$D")
expect "a canonical text that says confidential" 1 "the canonical text contains the word 'confidential'"

# ---- usage -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
t; out="$(bash "$GATE" --bogus 2>&1)"; rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi
t; out="$(bash "$GATE" --root 2>&1)"; rc=$?
if [ "$rc" = 2 ]; then echo "  ok   --root without a value is a usage error (exit 2)"; else echo "  FAIL --root without a value: exit $rc"; bad=$((bad+1)); fi

# ---- the controls: this selftest must be able to fail ------------------------------------------------------------------------------------------------------------------------------
# Each control swaps the gate for a fake (or a mutant of the real gate), runs one real case against it, and requires the case to MISBEHAVE (and then forgives it: it is the control).
fake="$TMP/fake.sh"
run_control() { # run_control LABEL GATE-TEXT CASE-FUNCTION ARGS...
  t; label="$1"; printf '%s\n' "$2" > "$fake"; shift 2
  real="$GATE"; GATE="$fake"; before=$bad
  "$@" > /dev/null
  GATE="$real"
  if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: $label"; else echo "  FAIL control: $label — the selftest did not notice"; bad=$((bad+1)); fi
}
mutant() { # mutant FROM TO -> prints the real gate with FROM replaced by TO, and refuses a mutant equal to the gate
  local m; m="$(sed "s/$1/$2/" "$GATE")"
  if [ "$m" = "$(cat "$GATE")" ]; then echo "echo 'control: the gate has no text $1, so the mutant is the gate itself'; exit 0"; else printf '%s\n' "$m"; fi
}
good_root "$D"; sed -i 's/visible/hidden/' "$D/LICENSE"; ARGS=(--canonical "$CANON" --root "$D")
run_control "a gate that always passes makes the 'one word changed' case misbehave" '#!/usr/bin/env bash
echo "check-license: OK"; exit 0' expect "one word changed" 1 "LICENSE differs from tests/license-canonical.txt"
good_root "$D"
run_control "a gate that always fails makes the 'right root is green' case misbehave" '#!/usr/bin/env bash
echo "check-license: RED"; exit 1' expect "right root is green" 0 "check-license: OK"
good_root "$D"; rm "$D/LICENSE"
run_control "a gate that fails for another reason makes the 'no LICENSE' case misbehave (class 10)" '#!/usr/bin/env bash
echo "check-license: RED — something unrelated"; exit 1' expect "no LICENSE" 1 "LICENSE does not exist"
good_root "$D"; sed -i 's/visible/hidden/' "$D/LICENSE"
run_control "a copy of the gate without its byte compare makes the 'one word changed' case misbehave" "$(mutant 'elif ! cmp -s "\$lic" "\$CANON"; then' 'elif false; then')" expect "one word changed" 1 "LICENSE differs from tests/license-canonical.txt"
good_root "$D"; { printf 'Third-Party Notices\n\n%s\n%s\n' "$S1" "$S2"; } > "$D/NOTICE"
run_control "a copy of the gate that reads the whole NOTICE makes the 'only in a later paragraph' case misbehave" "$(mutant "awk 'NF { started = 1; printf \"%s \", \$0; next } started { exit }'" "awk 'NF { printf \"%s \", \$0 }'")" expect "later paragraph" 1 "lacks the LICENSE sentence '$S1'"
good_root "$D"; printf 'Our confidentiality of keys rests on AES-GCM.\n' >> "$D/NOTICE"
run_control "a copy of the gate that reads 'confidential' as a substring makes the 'confidentiality' case misbehave" "$(mutant "grep -n -i -w --" "grep -n -i --")" expect "confidentiality" 0 "check-license: OK"

echo "selftest: $n cases"
[ "$n" -ge "$MIN_CASES" ] || { echo "check-license-selftest: RED — only $n cases ran (minimum $MIN_CASES)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-license-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-license-selftest: OK"
