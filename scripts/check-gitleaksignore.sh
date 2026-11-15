#!/usr/bin/env bash
# check-gitleaksignore.sh [--file FILE] — the secret scanner's ignore file may hold NO entries.
#
# WHY IT EXISTS: gitleaks runs with --log-opts=--all, so it reads every branch and the whole history. An ignore file is a hole in the scanner: whoever adds a line there silences a
# finding. Phase 0 needed exactly one such hole (the fake token planted on a throwaway branch, fault/p0-secret) and this gate made the hole exactly that size. From Phase 1 on
# fault proofs are PATCHES applied to a scratch copy by the `fault-proofs` workflow; no fault commit ever enters this repository's history, so there is nothing legitimate to ignore.
# The gate therefore says the file is empty: blank lines and `#` comments are fine, every other line is red — a real finding is fixed (rotate the secret, rewrite the history if the
# owner agrees) and never ignored from here.
#
# NOT CHECKED: that gitleaks reads this file at all (gitleaks.yml proves the scanner can go red on every run; this gate only keeps the file empty), or a `[allowlist]` section in a
# gitleaks config file (this repository has none; a gitleaks.toml would have to be added to this gate).
set -u
FILE=".gitleaksignore"
while [ $# -gt 0 ]; do
  case "$1" in
    --file) FILE="${2:?--file needs a path}"; shift 2;;
    --repo) shift 2;;   # accepted and ignored: the Phase 0 form of this gate took it
    *) echo "check-gitleaksignore: unknown argument '$1'" >&2; exit 2;;
  esac
done
n=0; bad=0
if [ -f "$FILE" ]; then
  while IFS= read -r l; do
    l="$(printf '%s' "$l" | tr -d '\r')"; case "$l" in ''|'#'*) continue;; esac
    case "$l" in *[![:space:]]*) ;; *) continue;; esac
    n=$((n+1)); bad=$((bad+1)); echo "  FAIL: the ignore file must hold no entries, found: $l"
  done < "$FILE"
fi
echo "check-gitleaksignore: $n ignore entries (must be 0)"
echo "NOT CHECKED: that gitleaks reads this file; a gitleaks.toml allowlist (none exists)"
[ "$bad" -eq 0 ] || { echo "check-gitleaksignore: RED"; exit 1; }
echo "check-gitleaksignore: OK"
