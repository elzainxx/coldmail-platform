#!/usr/bin/env bash
# platform-check.sh <layout-root> [--checks 1,2,3,4] — verification of coldmail-platform (a docs-only repo: "the suite" is its citations).
#
# <layout-root> is a directory that contains coldmail-platform (this repository) and, for checks 3 and 4, the three module repositories beside it.
#
#   1. every tracked JSON file parses                       (python in Docker, digest-pinned; runs on the CI runner)
#   2. every relative markdown link inside the repo resolves to a file
#   3. every cross-repo citation (../coldmail-*/path) resolves in <layout-root>          needs the three module repositories
#   4. every commit sha cited in a `backtick` resolves to a commit in one of the four    needs the three module repositories
#
# --checks selects a subset (default: all four). The CI workflow runs ALL FOUR on every push that touches what it reads (documents, JSON, tools/): 3 and 4 need the three module repositories beside
# this one, which tools/fetch-modules.sh clones over SSH with read-only per-repository deploy keys when supplied, or anonymously over https when a key's variable is empty (and goes RED if a key is
# wrong or revoked, or an anonymous clone fails — there is no skip path).
# Check 3 and check 4 each refuse to run (red) when a module repository is absent from the layout. Checks not selected are NAMED in the output — never silently green.
#
# Check 4 is strict: a backticked 7–40 hex string that resolves to no commit in any of the four repositories is RED unless tools/non-commit-hex.txt lists it with the
# reason it is not a commit (an agent id, a commit of the parent repository, …); and a listed entry that no document cites any more is RED too (a stale list
# hides the next real miss).
#
# History: this is a RE-IMPLEMENTATION of four of the five checks of the original runner (cprun.sh, which no longer exists). It is NOT byte-for-byte the
# original, and the original's check 5 (every sha attributed to a NAMED repo is in that repo) is not re-implemented. A green means the CITATIONS resolve;
# it does not mean the prose is correct.
set -u
ROOT="${1:?usage: platform-check.sh <layout-root containing coldmail-platform (and the three modules for checks 3-4)> [--checks 1,2,3,4]}"; shift
CHECKS="1,2,3,4"
while [ $# -gt 0 ]; do
  case "$1" in
    --checks) CHECKS="${2:?--checks needs a list like 1,2}"; shift 2;;
    *) echo "platform-check: unknown argument '$1'" >&2; exit 2;;
  esac
done
want() { case ",$CHECKS," in *",$1,"*) return 0;; esac; return 1; }
P="$ROOT/coldmail-platform"; fail=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }
cd "$P" || exit 2
echo "platform @ $(git rev-parse --short HEAD) in layout $ROOT   (checks requested: $CHECKS)"

# 1. JSON
if want 1; then
  n=0; jf=$(git ls-files '*.json'); for f in $jf; do n=$((n+1)); done
  echo "[1] JSON files: $n"
  [ "$n" -gt 0 ] || bad "no JSON files found — check 1 would be vacuous"
  PW="$(cygpath -w "$P" 2>/dev/null || printf '%s' "$P")"
  # shellcheck disable=SC2086  # $jf is a deliberate word-split list of tracked paths (none contains whitespace)
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PW:/r:ro" -w /r python:3.15.2-alpine@sha256:a0f2e268f2ce8f3709611a90054ec6a8ae31f6251f5896b8648b9bb99835e30a python -c "
import json,sys
bad=0
for f in sys.argv[1:]:
    try: json.load(open(f,encoding='utf-8'))
    except Exception as e: print('  FAIL: %s: %s'%(f,e)); bad+=1
sys.exit(1 if bad else 0)" $jf || fail=$((fail+1))
else echo "[1] JSON files: NOT RUN (not in --checks $CHECKS)"; fi

# 2. relative markdown links
if want 2; then
  echo "[2] internal markdown links"; PC2=$(mktemp) || exit 2   # scratch file: no fixed /tmp path
  # Docs/history/audit-scratch/ holds verbatim captured scratch (copies of module docs whose relative links point at files that were never copied);
  # it is preserved raw and never edited, so its internal links are deliberately out of scope here (see audit-scratch/README.md).
  for f in $(git ls-files '*.md' ':!Docs/history/audit-scratch'); do
    d=$(dirname "$f")
    grep -o -E '\]\(([^)#[:space:]]+)(#[^)]*)?\)' "$f" | sed -E 's/^\]\(//; s/(#[^)]*)?\)$//' | while read -r l; do
      case "$l" in http*|mailto:*|/*) continue;; esac
      case "$l" in ../coldmail-*) continue;; esac     # cross-repo: check 3
      [ -e "$d/$l" ] || echo "  FAIL: $f -> $l"
    done
  done > "$PC2"; c=$(grep -c FAIL "$PC2"); if [ "$c" != 0 ]; then cat "$PC2"; fail=$((fail+c)); fi; rm -f "$PC2"
  checked=$(for f in $(git ls-files '*.md' ':!Docs/history/audit-scratch'); do grep -o -E '\]\(([^)#[:space:]]+)(#[^)]*)?\)' "$f"; done | grep -v -E '\]\((http|mailto)' | grep -v '\.\./coldmail-' | wc -l); echo "    links examined: $checked"
  [ "$checked" -gt 0 ] || bad "no internal links examined — check 2 would be vacuous"
else echo "[2] internal markdown links: NOT RUN (not in --checks $CHECKS)"; fi

MODULES="coldmail-verify coldmail-send-managed coldmail-send-byo"
need_modules() { for m in $MODULES; do [ -d "$ROOT/$m/.git" ] || bad "the layout has no $m (a git repository) beside coldmail-platform — check $1 needs all three module repositories"; done; }

# 3. cross-repo citations
if want 3; then
  echo "[3] cross-repo citations (../coldmail-*/...)"; xr=0; xbad=0
  need_modules 3
  for f in $(git ls-files '*.md'); do
    d=$(dirname "$f")
    for l in $(grep -o -E '\.\./coldmail-[a-z-]+/[^) `"'"'"'#:]+' "$f" | sort -u); do
      xr=$((xr+1)); l="${l%.}"; [ -e "$d/$l" ] || { echo "  FAIL: $f -> $l"; xbad=$((xbad+1)); }
    done
  done; fail=$((fail+xbad)); echo "    citations examined: $xr  broken: $xbad"
else echo "[3] cross-repo citations: NOT RUN (not in --checks $CHECKS)"; fi

# 4. cited shas
miss="n/a"
if want 4; then
  echo "[4] cited commit shas"; tot=0; ok=0; miss=0; allowed=0; AL="$P/tools/non-commit-hex.txt"
  need_modules 4
  [ -f "$AL" ] || bad "tools/non-commit-hex.txt is missing — check 4 has no way to tell an id from a commit"
  cited=$(git ls-files '*.md' | xargs grep -h -o -E '`[0-9a-f]{7,40}`' | tr -d '`' | sort -u)
  for s in $cited; do
    tot=$((tot+1)); found=0
    for r in coldmail-verify coldmail-send-managed coldmail-send-byo coldmail-platform .; do
      [ -e "$ROOT/$r/.git" ] && git -C "$ROOT/$r" cat-file -e "$s^{commit}" 2>/dev/null && { found=1; break; }
    done
    if [ $found = 1 ]; then ok=$((ok+1))
    elif [ -f "$AL" ] && grep -q -E "^$s\|" "$AL"; then allowed=$((allowed+1))
    else miss=$((miss+1)); bad "$s is cited as a commit, resolves to none in the four repositories, and is not in tools/non-commit-hex.txt"; fi
  done
  # a listed entry nothing cites any more is stale, and a stale list hides the next real miss
  stale=0
  if [ -f "$AL" ]; then
    for h in $(grep -v -E '^[[:space:]]*(#|$)' "$AL" | cut -d'|' -f1); do
      printf '%s\n' "$cited" | grep -q -x "$h" || { stale=$((stale+1)); bad "tools/non-commit-hex.txt lists $h but no document cites it any more — remove the entry"; }
    done
  fi
  echo "    candidates: $tot  resolve to a commit: $ok  listed as not-a-commit: $allowed  unresolved and unlisted: $miss  stale list entries: $stale"
else echo "[4] cited commit shas: NOT RUN (not in --checks $CHECKS)"; fi
echo "RESULT: checks run = $CHECKS ; structural failures = $fail ; unresolved sha candidates (4) = $miss"
[ "$fail" -eq 0 ]
