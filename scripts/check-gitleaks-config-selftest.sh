#!/usr/bin/env bash
# check-gitleaks-config-selftest.sh — proves scripts/check-gitleaks-config.sh goes red, FOR THE REASON IT NAMES, on every way the secret scanner's configuration can be loosened, and green on the real shapes.
# Each case builds a throwaway git repository with a .gitleaks.toml, runs the real gate on it, and asserts the exit code AND a fragment of the printed reason (class 10).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-gitleaks-config.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
n=0; bad=0
t() { n=$((n+1)); }
SHA=""
# repo DIR: a git repository with one commit; SHA is that commit
repo() {
  d="$1"; rm -rf "$d"; mkdir -p "$d"; git -C "$d" init -q; git -C "$d" config user.email ci@example.invalid; git -C "$d" config user.name ci
  echo x > "$d/f"; git -C "$d" config core.autocrlf false; git -C "$d" add f 2>/dev/null; git -C "$d" commit -q -m one; SHA="$(git -C "$d" rev-parse HEAD)"
}
HEAD_TOML='title = "t"

[extend]
useDefault = true
'
PATH_BLOCK='
[[allowlists]]
description = "request ids in the cited run logs (not secrets)"
targetRules = ["generic-api-key"]
paths = ['"'''"'(^|/)Docs/history/audit-scratch/.*\.log$'"'''"']
'
commit_block() { printf '\n[[allowlists]]\ndescription = "%s"\ntargetRules = ["generic-api-key"]\ncommits = ["%s"]\n' "${3:-a placeholder password in a test (not a secret)}" "$1"; }
expect() {
  t; name="$1"; d="$2"; want="$3"; frag="$4"
  out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; echo "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  if [ -n "$frag" ] && ! printf '%s\n' "$out" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc but the output lacks '$frag'"; echo "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  echo "  ok   $name"
}
D="$TMP/r"

repo "$D"; printf '%s' "$HEAD_TOML" > "$D/.gitleaks.toml"; expect "no exception at all is green" "$D" 0 "0 allowlist block(s)"
repo "$D"; { printf '%s%s' "$HEAD_TOML" "$PATH_BLOCK"; } > "$D/.gitleaks.toml"; expect "the audit-scratch path exception is green" "$D" 0 "1 path, 0 commit"
repo "$D"; { printf '%s%s' "$HEAD_TOML" "$PATH_BLOCK"; commit_block "$SHA"; } > "$D/.gitleaks.toml"; expect "a path and a commit exception are green and counted" "$D" 0 "1 path, 1 commit"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "$SHA"; } > "$D/.gitleaks.toml"; expect "a commit exception in the repository's history is green" "$D" 0 "check-gitleaks-config: OK"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "$SHA"; } > "$D/.gitleaks.toml"; expect "the green run prints what it did not check" "$D" 0 "NOT CHECKED: that an excepted line is not a secret"

repo "$D"; rm -f "$D/.gitleaks.toml"; expect "no .gitleaks.toml" "$D" 1 "does not exist: the secret scanner's configuration is a required file"
repo "$D"; printf 'title = "t"\n' > "$D/.gitleaks.toml"; expect "the default ruleset is dropped" "$D" 1 "does not say 'useDefault = true'"
repo "$D"; printf 'title = "t"\n\n[extend]\n# useDefault = true\n' > "$D/.gitleaks.toml"; expect "useDefault only in a comment" "$D" 1 "does not say 'useDefault = true'"
repo "$D"; { printf '%s\n[allowlist]\npaths = ["x"]\n' "$HEAD_TOML"; } > "$D/.gitleaks.toml"; expect "a legacy [allowlist] table" "$D" 1 "a legacy [allowlist] table exists"
repo "$D"; { printf '%s\n[[allowlists]]\ndescription = "a regex exception for everything"\ntargetRules = ["generic-api-key"]\nregexes = ["abc"]\ncommits = ["%s"]\n' "$HEAD_TOML" "$SHA"; } > "$D/.gitleaks.toml"; expect "a regex exception" "$D" 1 "regexes, stopwords or disabledRules are configured"
repo "$D"; { printf '%s\nstopwords = ["x"]\n' "$HEAD_TOML"; } > "$D/.gitleaks.toml"; expect "stopwords" "$D" 1 "regexes, stopwords or disabledRules are configured"
repo "$D"; { printf '%s\n[[rules]]\nid = "mine"\nregex = "x"\n' "$HEAD_TOML"; } > "$D/.gitleaks.toml"; expect "the file defines its own rules" "$D" 1 "the file defines its own [[rules]]"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "0123456789abcdef0123456789abcdef01234567"; } > "$D/.gitleaks.toml"; expect "a commit that is not in the history (a stale exception)" "$D" 1 "is not in this repository's history"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "deadbeef"; } > "$D/.gitleaks.toml"; expect "a commit that is not a 40-hex sha" "$D" 1 "is not a 40-hex commit sha"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "$SHA" "" "short"; } > "$D/.gitleaks.toml"; expect "a commit exception without a real description" "$D" 1 "needs a description of at least 10 characters"
repo "$D"; { printf '%s\n[[allowlists]]\ndescription = "two rules at once for one commit"\ntargetRules = ["generic-api-key", "aws-access-token"]\ncommits = ["%s"]\n' "$HEAD_TOML" "$SHA"; } > "$D/.gitleaks.toml"; expect "two rules in one exception" "$D" 1 "targetRules must name exactly ONE rule (found 2)"
repo "$D"; { printf '%s\n[[allowlists]]\ndescription = "no rule named: applies to every rule"\ncommits = ["%s"]\n' "$HEAD_TOML" "$SHA"; } > "$D/.gitleaks.toml"; expect "an exception for every rule" "$D" 1 "targetRules must name exactly ONE rule (found 0)"
repo "$D"; { printf '%s\n[[allowlists]]\ndescription = "a path exception for the whole tests tree"\ntargetRules = ["generic-api-key"]\npaths = ["tests/.*"]\n' "$HEAD_TOML"; } > "$D/.gitleaks.toml"; expect "a path exception outside audit-scratch" "$D" 1 "a path exception may only cover Docs/history/audit-scratch"
repo "$D"; { printf '%s' "$HEAD_TOML"; commit_block "$SHA"; commit_block "$SHA"; commit_block "$SHA"; commit_block "$SHA"; commit_block "$SHA"; commit_block "$SHA"; } > "$D/.gitleaks.toml"; expect "five exceptions (a decision, not a drift)" "$D" 1 "6 [[allowlists]] blocks (at most 5 are accepted"
repo "$D"; { printf '%s\n[[allowlists]]\ndescription = "both a path and a commit at once"\ntargetRules = ["generic-api-key"]\npaths = ["Docs/history/audit-scratch/x"]\ncommits = ["%s"]\n' "$HEAD_TOML" "$SHA"; } > "$D/.gitleaks.toml"; expect "a block that is both a path and a commit exception" "$D" 1 "must be a path exception OR a commit exception"
repo "$D"; { printf "%s" "$HEAD_TOML"; commit_block "$SHA"; } > "$D/.gitleaks.toml"; rm -rf "$D/.git"; expect "no .git (a git archive copy): green, and it says the commit was not checked" "$D" 0 "existence of the excepted commit(s) was NOT checked"
out="$(bash "$GATE" --bogus 2>&1)"; rc=$?; t
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi

# the real repository's own configuration is green (the selftest also proves the gate on the shipped file)
t; ROOT="$(cd "$HERE/.." && pwd)"
if bash "$GATE" --root "$ROOT" >/dev/null 2>&1; then echo "  ok   the repository's own .gitleaks.toml is accepted"; else echo "  FAIL the repository's own .gitleaks.toml is rejected"; bad=$((bad+1)); fi

# the control: the selftest itself must be able to fail. A gate that always passes makes a reject case misbehave.
t; repo "$D"; printf 'title = "t"\n' > "$D/.gitleaks.toml"
fake="$TMP/always-ok.sh"; printf '#!/usr/bin/env bash\necho "check-gitleaks-config: OK"\nexit 0\n' > "$fake"
REAL_GATE="$GATE"; GATE="$fake"; before=$bad
expect "(control) the default ruleset dropped, against a gate that always passes" "$D" 1 "does not say 'useDefault = true'" > /dev/null
GATE="$REAL_GATE"
if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: a gate that always passes makes a reject case misbehave, so the cases above can fail"; else echo "  FAIL control: the selftest did not notice a gate that always passes"; bad=$((bad+1)); fi

echo "selftest: $n cases"
[ "$n" -ge 25 ] || { echo "check-gitleaks-config-selftest: RED — only $n cases ran (minimum 25)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-gitleaks-config-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-gitleaks-config-selftest: OK"
