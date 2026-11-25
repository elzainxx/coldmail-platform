#!/usr/bin/env bash
# check-gitleaks-config.sh [--root DIR] — BLOCKING gate on .gitleaks.toml: the secret scanner's configuration may hold ONLY the exceptions this repository has decided on, each narrow and auditable.
#
#   - the file exists and extends the default ruleset (`[extend] useDefault = true`): a config that dropped the default rules would blind the scan
#   - no legacy `[allowlist]` table (it applies to every rule), no `regexes`, no `stopwords`, no `[[rules]]` of its own, no `disabledRules`
#   - every `[[allowlists]]` block is exactly ONE of two shapes:
#       (path)   `targetRules` with ONE rule and `paths` with ONE pattern, and the pattern names Docs/history/audit-scratch (the cited run logs of Phase 0)
#       (commit) `targetRules` with ONE rule and `commits` with ONE 40-hex sha that EXISTS in this repository's history (a stale exception is red) and has a `description`
#   - at most 6 allowlist blocks in all (a seventh is a decision, not a drift: change this number in the same commit and say why; the fourth and fifth were added in Phase 3 for commits 8accb189 and 56eb8ab2, the sixth in Phase 4 for commit 114b9e7, see .gitleaks.toml)
#
# NOT CHECKED (printed on every run): whether an excepted line is really not a secret (a person read it: the comment above each block says who, when and what), and gitleaks' own matching (the `gitleaks`
# workflow proves the scanner on every run).
set -u
ROOT="."
while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="${2:?--root needs a directory}"; shift 2;;
    *) echo "check-gitleaks-config: unknown argument '$1'" >&2; exit 2;;
  esac
done
F="$ROOT/.gitleaks.toml"
fail=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }
nogit=0
[ -f "$F" ] || { echo "check-gitleaks-config: RED — $F does not exist: the secret scanner's configuration is a required file"; exit 1; }
text="$(tr -d '\r' < "$F")"
code="$(printf '%s\n' "$text" | sed 's/^[[:space:]]*#.*$//')"          # comments are not configuration
printf '%s\n' "$code" | grep -qE '^[[:space:]]*useDefault[[:space:]]*=[[:space:]]*true[[:space:]]*$' || bad ".gitleaks.toml does not say 'useDefault = true' under [extend]: the default rules would be dropped"
printf '%s\n' "$code" | grep -qE '^[[:space:]]*\[allowlist\][[:space:]]*$' && bad "a legacy [allowlist] table exists: it applies to every rule (use a scoped [[allowlists]] block)"
printf '%s\n' "$code" | grep -qE '^[[:space:]]*(regexes|stopwords|disabledRules)[[:space:]]*=' && bad "regexes, stopwords or disabledRules are configured: an exception must be a path or a commit, never a pattern over content"
printf '%s\n' "$code" | grep -qE '^[[:space:]]*\[\[rules\]\]' && bad "the file defines its own [[rules]]"
n=$(printf '%s\n' "$code" | grep -cE '^[[:space:]]*\[\[allowlists\]\]') || n=0
[ "$n" -le 6 ] || bad "$n [[allowlists]] blocks (at most 6 are accepted: another exception is a decision)"
# split into blocks
blocks=0; paths_blocks=0; commit_blocks=0
awk '/^[[:space:]]*\[\[allowlists\]\]/ {n++; inb=n; next} /^[[:space:]]*\[/ {inb=0} inb>0 {print inb "\t" $0}' <<<"$code" > /tmp/gl-blocks.$$
for i in $(cut -f1 /tmp/gl-blocks.$$ | sort -un); do
  blocks=$((blocks+1))
  blk="$(awk -F'\t' -v b="$i" '$1==b {print $2}' /tmp/gl-blocks.$$)"
  rules=$(printf '%s\n' "$blk" | grep -E '^[[:space:]]*targetRules[[:space:]]*=' | head -1)
  nrules=$(printf '%s' "$rules" | grep -o '"[^"]*"' | wc -l | tr -d ' ')
  [ "$nrules" = 1 ] || bad "allowlist block $i: targetRules must name exactly ONE rule (found $nrules)"
  hascommits=$(printf '%s\n' "$blk" | grep -cE '^[[:space:]]*commits[[:space:]]*=') || hascommits=0
  haspaths=$(printf '%s\n' "$blk" | grep -cE '^[[:space:]]*paths[[:space:]]*=') || haspaths=0
  if [ "$hascommits" = 1 ] && [ "$haspaths" = 0 ]; then
    commit_blocks=$((commit_blocks+1))
    shas=$(printf '%s\n' "$blk" | grep -E '^[[:space:]]*commits[[:space:]]*=' | head -1 | grep -o '"[^"]*"' | tr -d '"')
    ns=$(printf '%s\n' "$shas" | grep -c . ) || ns=0
    [ "$ns" = 1 ] || bad "allowlist block $i: commits must name exactly ONE commit (found $ns)"
    for s in $shas; do
      printf '%s' "$s" | grep -qE '^[0-9a-f]{40}$' || { bad "allowlist block $i: '$s' is not a 40-hex commit sha"; continue; }
      if [ -e "$ROOT/.git" ]; then
        git -C "$ROOT" cat-file -e "$s^{commit}" 2>/dev/null || bad "allowlist block $i: commit $s is not in this repository's history (a stale exception)"
      else
        nogit=1
      fi
    done
    printf '%s\n' "$blk" | grep -qE '^[[:space:]]*description[[:space:]]*=[[:space:]]*"[^"]{10,}"' || bad "allowlist block $i: a commit exception needs a description of at least 10 characters"
  elif [ "$haspaths" = 1 ] && [ "$hascommits" = 0 ]; then
    paths_blocks=$((paths_blocks+1))
    pat=$(printf '%s\n' "$blk" | grep -E '^[[:space:]]*paths[[:space:]]*=' | head -1)
    printf '%s' "$pat" | grep -qF 'Docs/history/audit-scratch' || bad "allowlist block $i: a path exception may only cover Docs/history/audit-scratch (the cited Phase 0 run logs)"
  else
    bad "allowlist block $i: must be a path exception OR a commit exception, not both and not neither"
  fi
done
rm -f /tmp/gl-blocks.$$
[ "$blocks" = "$n" ] || bad "parsed $blocks allowlist block(s) but the file declares $n"
echo "check-gitleaks-config: $blocks allowlist block(s): $paths_blocks path, $commit_blocks commit; useDefault checked"
[ "$nogit" = 0 ] || echo "check-gitleaks-config: NOTE: no .git here (a scratch copy made by git archive): the existence of the excepted commit(s) was NOT checked; the gates workflow checks it on the real checkout"
echo "NOT CHECKED: that an excepted line is not a secret (the comment above each block records who read it and when); gitleaks' own matching (the gitleaks workflow proves the scanner on every run)"
if [ "$fail" -gt 0 ]; then echo "check-gitleaks-config: RED — $fail problem(s)"; exit 1; fi
echo "check-gitleaks-config: OK"
