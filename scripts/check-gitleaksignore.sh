#!/usr/bin/env bash
# check-gitleaksignore.sh [--file FILE] [--repo DIR] — the secret scanner's ignore file may hold ONLY the fingerprints of the planted fault-injection commits.
#
# WHY IT EXISTS: gitleaks is run with --log-opts=--all, so it reads every branch. The Phase 0 proof that the secret scan can go red plants a (fake, non-provider-shaped) token on
# a throwaway branch `fault/p0-*`; that branch cannot be deleted by the agent that made it, so its commit stays in the scanned history and would turn every later run red.
# That one commit's finding is therefore listed in .gitleaksignore. An ignore file is a hole in the scanner, so this gate makes the hole exactly that size: every entry must be
#     <commit>:<file>:<rule-id>:<line>   where <commit> is reachable from a remote branch named fault/p0-* and is NOT reachable from HEAD.
# An entry for a commit on the real history (HEAD) — the way a real leak would be silenced — is red; so is any other shape, an unknown commit, or a commit that is on no fault branch.
# An absent or empty file is green (nothing is ignored).
#
# NOT CHECKED: that the fault branch's content really is a fake token (read the commit), or that the branch still exists on the remote (it is not deleted by this project).
set -u
FILE=".gitleaksignore"; REPO="."
while [ $# -gt 0 ]; do
  case "$1" in
    --file) FILE="${2:?--file needs a path}"; shift 2;;
    --repo) REPO="${2:?--repo needs a path}"; shift 2;;
    *) echo "check-gitleaksignore: unknown argument '$1'" >&2; exit 2;;
  esac
done
fail=0; n=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }
if [ -f "$FILE" ]; then
  while IFS= read -r l; do
    l="$(printf '%s' "$l" | tr -d '\r')"; case "$l" in ''|'#'*) continue;; esac
    n=$((n+1))
    if ! printf '%s\n' "$l" | grep -E -q '^[0-9a-f]{40}:[^:[:space:]]+:[A-Za-z0-9._-]+:[0-9]+$'; then bad "not a gitleaks fingerprint (<40-hex commit>:<file>:<rule>:<line>): $l"; continue; fi
    c="${l%%:*}"
    if ! git -C "$REPO" cat-file -e "$c^{commit}" 2>/dev/null; then bad "$c is not a commit in this repository"; continue; fi
    if git -C "$REPO" merge-base --is-ancestor "$c" HEAD 2>/dev/null; then bad "$c is reachable from HEAD — the real history may not be ignored"; continue; fi
    on=$(git -C "$REPO" for-each-ref --contains "$c" --format='%(refname)' 'refs/remotes/origin/fault/p0-*' 'refs/heads/fault/p0-*' | wc -l | tr -d ' ')
    [ "$on" -ge 1 ] || bad "$c is on no fault/p0-* branch"
  done < "$FILE"
fi
echo "check-gitleaksignore: $n ignore entries, failures: $fail"
echo "NOT CHECKED: that the planted content is a fake token, or that the fault branch still exists on the remote"
[ "$fail" -eq 0 ] || { echo "check-gitleaksignore: RED"; exit 1; }
echo "check-gitleaksignore: OK"
