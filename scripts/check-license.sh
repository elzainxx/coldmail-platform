#!/usr/bin/env bash
# check-license.sh [--root DIR ...] [--canonical FILE] — BLOCKING gate: every repository carries the one LICENSE the owner ruled on, and its NOTICE and README say the same thing.
#
# WHY IT EXISTS. The five repositories are public: the source is visible, nothing is licensed (owner decision 2026-11-17). Five copies of one paragraph drift (the modules' old LICENSE files said
# "PROPRIETARY AND CONFIDENTIAL", which is false for a public repository). The paragraph lives ONCE, in tests/license-canonical.txt, and this gate fails when a repository's copy differs by one byte.
#
# WHAT IT CHECKS, for every --root DIR given (one or more; zero roots is red):
#   1. DIR/LICENSE exists, is not empty and equals tests/license-canonical.txt byte for byte. CRLF is red (reported as such), a missing final newline is red, one changed word is red and the first
#      differing line is printed with both texts;
#   2. neither DIR/LICENSE nor DIR/NOTICE contains the whole word `confidential` (any case: "PROPRIETARY AND CONFIDENTIAL" is the wording that is no longer true; `confidentiality`, the property
#      of encryption, is a different word and stays);
#   3. DIR/NOTICE's FIRST paragraph (the lines up to the first blank line, CR stripped, whitespace squeezed) contains both sentences of the LICENSE that matter to a reader:
#        "The source is visible for transparency only."   and   "No license is granted to use, copy, modify, distribute or run this software, in whole or in part."
#      (the third-party notices that follow it are not read);
#   4. DIR/README.md has a line that is exactly: Source visible for transparency only; all rights reserved (see LICENSE and NOTICE).
# The canonical file itself is checked once: it exists, is one LF-terminated line without CR, carries both sentences above and no `confidential`. A root listed twice is red (its problems would be
# counted twice and a green once would be read as two).
# FLOORS (a gate whose subject is empty is red, classes 8 and 18): no --root at all, a root that is not a directory, a missing or empty LICENSE / NOTICE / README.md, a missing or empty canonical file.
#
# Output: one `ok` line per root that passed every check, one `RED:` line per problem (prefixed with the root as it was given), `check-license: N root(s), M problem(s)`, and a `NOT CHECKED:` line.
# Exit status: 0 green, 1 red, 2 usage error. Paired control: scripts/check-license-selftest.sh (every reject case asserts the reason it fails for).
# Wired in: the platform's `gates` workflow (the platform tree), its `platform-check` workflow (the three module checkouts) and the parent repository's `modules-lock` workflow (all five).
#
# NOT CHECKED (also printed on every run): that the LICENSE wording is the wording the owner ruled on (tests/license-canonical.txt is the one statement of it: changing it is a ruling and shows in a diff);
# the word `confidential` outside LICENSE and NOTICE (README and the documents are judged by a person with grep, technical uses such as encryption stay); that NOTICE's third-party notices are right
# or complete; the LICENSE files of vendored third-party code (verifier-node/truemail-rack/LICENSE.txt is theirs); that a root is a repository (the files of the directory are read, git is not asked).
set -u
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CANON_REL="tests/license-canonical.txt"
S1="The source is visible for transparency only."
S2="No license is granted to use, copy, modify, distribute or run this software, in whole or in part."
README_LINE="Source visible for transparency only; all rights reserved (see LICENSE and NOTICE)."
NOT_CHECKED="NOT CHECKED: that the LICENSE wording is the wording the owner ruled on ($CANON_REL is the one statement of it: changing it is a ruling and shows in a diff); the word 'confidential' outside LICENSE and NOTICE (README and the documents are judged by a person, technical uses such as encryption stay); that NOTICE's third-party notices are right or complete; the LICENSE files of vendored third-party code; that a root is a repository (the files of the directory are read, git is not asked)"

usage() { echo "usage: check-license.sh --root DIR [--root DIR ...] [--canonical FILE]" >&2; exit 2; }
roots=(); CANON="$HERE/../$CANON_REL"
while [ $# -gt 0 ]; do
  case "$1" in
    --root)        [ $# -ge 2 ] || usage; roots+=("$2"); shift 2;;
    --root=*)      roots+=("${1#*=}"); shift;;
    --canonical)   [ $# -ge 2 ] || usage; CANON="$2"; shift 2;;
    --canonical=*) CANON="${1#*=}"; shift;;
    -h|--help)     usage;;
    *) echo "check-license: unknown argument '$1'" >&2; usage;;
  esac
done

T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
fail=0; label=""
bad() { echo "  RED: ${label:+$label: }$*"; fail=$((fail+1)); }
squeeze() { tr -s ' \t' '  ' | sed 's/^ //; s/ $//'; }   # whitespace squeezed to single spaces, ends trimmed

if [ "${#roots[@]}" -eq 0 ]; then
  echo "check-license: RED — found 0 roots (give at least one --root DIR): the instrument matched nothing"; echo "$NOT_CHECKED"; exit 1
fi

# ---- the canonical text ----------------------------------------------------------------------------------------------------------------------------------------------------------------
label="$CANON_REL"
if [ ! -f "$CANON" ]; then echo "check-license: RED — the canonical text $CANON_REL does not exist ($CANON): there is nothing to compare the LICENSE files with"; echo "$NOT_CHECKED"; exit 1; fi
if [ ! -s "$CANON" ]; then echo "check-license: RED — the canonical text $CANON_REL is empty: the gate matched nothing"; echo "$NOT_CHECKED"; exit 1; fi
if [ "$(tr -cd '\r' < "$CANON" | wc -c | tr -d ' ')" -gt 0 ]; then bad "the canonical text contains a carriage return: it must be LF"; fi
if [ "$(tail -c 1 "$CANON" | od -An -c | tr -d ' ')" != '\n' ]; then bad "the canonical text does not end with a newline"; fi
if [ "$(wc -l < "$CANON" | tr -d ' ')" -ne 1 ]; then bad "the canonical text is not one line (the owner's ruling is one paragraph, one line)"; fi
canon_flat="$(tr -d '\r\n' < "$CANON")"
case "$canon_flat" in *"$S1"*) ;; *) bad "the canonical text lacks the sentence the gate requires of NOTICE: '$S1'";; esac
case "$canon_flat" in *"$S2"*) ;; *) bad "the canonical text lacks the sentence the gate requires of NOTICE: '$S2'";; esac
if printf '%s\n' "$canon_flat" | grep -q -i -w -- 'confidential'; then bad "the canonical text contains the word 'confidential'"; fi
canon_bytes="$(wc -c < "$CANON" | tr -d ' ')"
label=""

# ---- every root ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------
first_diff() { # first_diff A B -> "N<TAB>a line<TAB>b line" for the first differing line, nothing when every line is equal
  awk -v A="$1" -v B="$2" 'BEGIN {
    while ((getline l < A) > 0) { na++; a[na] = l }
    while ((getline l < B) > 0) { nb++; b[nb] = l }
    max = (na > nb) ? na : nb
    for (i = 1; i <= max; i++) {
      if (i > na) { printf "%d\t<the file ends before this line>\t%s\n", i, substr(b[i], 1, 220); exit }
      if (i > nb) { printf "%d\t%s\t<the canonical text ends before this line>\n", i, substr(a[i], 1, 220); exit }
      if (a[i] != b[i]) { printf "%d\t%s\t%s\n", i, substr(a[i], 1, 220), substr(b[i], 1, 220); exit }
    }
  }'
}
seen=" "; n_ok=0
for r in "${roots[@]}"; do
  label="$r"; before="$fail"
  if [ ! -d "$r" ]; then bad "the root is not a directory: the instrument matched nothing"; continue; fi
  abs="$(cd "$r" && pwd -P)"
  case "$seen" in *" $abs "*) bad "the root is listed twice ($abs): its problems would be counted twice"; continue;; esac
  seen+="$abs "

  # 1. LICENSE equals the canonical text
  lic="$r/LICENSE"
  if [ ! -f "$lic" ]; then bad "LICENSE does not exist"
  elif [ ! -s "$lic" ]; then bad "LICENSE is empty: the gate matched nothing"
  elif ! cmp -s "$lic" "$CANON"; then
    ncr="$(tr -cd '\r' < "$lic" | wc -c | tr -d ' ')"
    tr -d '\r' < "$lic" > "$T/lic.lf"
    if [ "$ncr" -gt 0 ]; then bad "LICENSE contains a carriage return ($ncr of them: CRLF line endings): the file must be LF, byte for byte the canonical text"; fi
    if ! cmp -s "$T/lic.lf" "$CANON"; then
      d="$(first_diff "$T/lic.lf" "$CANON")"
      if [ -n "$d" ]; then
        n="${d%%$'\t'*}"; rest="${d#*$'\t'}"; ll="${rest%%$'\t'*}"; cl="${rest#*$'\t'}"
        bad "LICENSE differs from $CANON_REL: first difference at line $n"
        echo "       LICENSE  : $ll"
        echo "       canonical: $cl"
      else
        bad "LICENSE differs from $CANON_REL in its final newline only (every line is equal): the file must end with exactly one newline, as the canonical text does"
      fi
    fi
  fi
  # 2. no 'confidential' in LICENSE or NOTICE
  for f in LICENSE NOTICE; do
    [ -f "$r/$f" ] || continue
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      bad "$f line ${hit%%:*} contains the word 'confidential': the repositories are public, the source is visible and all rights are reserved, nothing is confidential"
    done < <(tr -d '\r' < "$r/$f" | grep -n -i -w -- 'confidential')
  done
  # 3. NOTICE's first paragraph carries both sentences
  nt="$r/NOTICE"
  if [ ! -f "$nt" ]; then bad "NOTICE does not exist"
  elif [ ! -s "$nt" ]; then bad "NOTICE is empty: the gate matched nothing"
  else
    para="$(tr -d '\r' < "$nt" | awk 'NF { started = 1; printf "%s ", $0; next } started { exit }' | squeeze)"
    if [ -z "$para" ]; then bad "NOTICE has no paragraph (only blank lines)"
    else
      case "$para" in *"$S1"*) ;; *) bad "NOTICE's first paragraph lacks the LICENSE sentence '$S1'";; esac
      case "$para" in *"$S2"*) ;; *) bad "NOTICE's first paragraph lacks the LICENSE sentence '$S2'";; esac
    fi
  fi
  # 4. README carries the notice line
  rd="$r/README.md"
  if [ ! -f "$rd" ]; then bad "README.md does not exist"
  elif [ ! -s "$rd" ]; then bad "README.md is empty: the gate matched nothing"
  elif ! tr -d '\r' < "$rd" | grep -q -x -F -- "$README_LINE"; then bad "README.md has no line that is exactly: $README_LINE"
  fi
  if [ "$fail" -eq "$before" ]; then n_ok=$((n_ok+1)); echo "  ok   $r: LICENSE equals the canonical text ($canon_bytes bytes), NOTICE's first paragraph carries both sentences, README.md carries the notice line"; fi
done
label=""

echo "check-license: ${#roots[@]} root(s), $n_ok passed, $fail problem(s); canonical text $CANON_REL ($canon_bytes bytes)"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-license: RED — $fail problem(s)"; exit 1; }
echo "check-license: OK"
