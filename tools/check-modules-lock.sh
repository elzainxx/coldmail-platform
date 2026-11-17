#!/usr/bin/env bash
# check-modules-lock.sh <parent-dir> [<lock-file>] — compare tools/modules.lock with the commits the PARENT repository pins as submodules (its gitlinks).
# Run it when re-pinning. There is no CI for this yet: the parent was private and had no job that could read the module repositories (all five repositories have been public since 2026-11-17, so one can now) (Docs/phase-0-spec.md, F9),
# so the comparison is manual, and this script exists so that "manual" is one command whose red names the module that disagrees.
# NOT CHECKED: that either side is the commit you MEANT to pin, or that it is green — only that the two agree.
set -u
PARENT="${1:?usage: check-modules-lock.sh <parent-dir> [<lock-file>]}"; LOCK="${2:-$(dirname "$0")/modules.lock}"
[ -f "$LOCK" ] || { echo "check-modules-lock: RED — no lock file at $LOCK"; exit 1; }
fail=0; n=0
for repo in coldmail-verify coldmail-send-managed coldmail-send-byo; do
  pin=$(tr -d '\r' < "$LOCK" | awk -v r="$repo" '/^[[:space:]]*#/ || NF==0 {next} $1==r {print $2}')
  gl=$(git -C "$PARENT" ls-tree HEAD "$repo" 2>/dev/null | awk '$2=="commit" {print $3}')
  n=$((n+1))
  if [ -z "$pin" ]; then echo "  FAIL: $repo has no entry in $LOCK"; fail=$((fail+1)); continue; fi
  if [ -z "$gl" ]; then echo "  FAIL: $repo is not a submodule (gitlink) in the parent at HEAD of $PARENT"; fail=$((fail+1)); continue; fi
  if [ "$pin" = "$gl" ]; then echo "  ok: $repo lock == parent gitlink == $gl"; else echo "  FAIL: $repo lock $pin != parent gitlink $gl"; fail=$((fail+1)); fi
done
echo "check-modules-lock: $n modules compared, $fail disagree"
echo "NOT CHECKED: that either side is the commit you meant to pin, or that it is green"
[ "$fail" -eq 0 ] || { echo "check-modules-lock: RED"; exit 1; }
echo "check-modules-lock: OK"
