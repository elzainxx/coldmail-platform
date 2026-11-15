#!/usr/bin/env bash
# check-fault-patches — BLOCKING CI gate: every fault proof under ci/faults/<phase>/ is well-formed and its patch still applies.
#
#   check-fault-patches.sh [--root DIR] [--min-faults N]     (DIR = a git repository root, default: the one this script lives in; N >= 1, default 1)
#
# WHY: the fault-proofs workflow is dispatch-only. A patch that stops applying (the file it edits moved, or a gate was rewritten) would go unseen until the next
# dispatch, which may be the phase-close run. This gate runs on every push, so the push that breaks a patch goes red.
#
# WHAT IT CHECKS, for every directory ci/faults/<phase>/ (ci/faults-archive is a record, not run: it is not looked at):
#   * the directory holds at least one .expect file;
#   * each .patch has a .expect and each .expect has a .patch, and the .patch is not empty;
#   * each .expect parses under the format of ci/faults/README.md (parsed by `ci/faults/run-leg.sh --check-expect`, the SAME parser the runner uses);
#   * each TARGET=platform patch passes `git apply --check` on a pristine `git archive HEAD` copy of the repository;
#   * at least --min-faults faults (.expect files) were found in total: "found 0 faults: the instrument matched nothing" is red.
#
# NOT CHECKED — also printed on EVERY run as the `NOT CHECKED:` line: whether any fault actually turns its gate red (only the dispatch-only fault-proofs
# workflow runs them); TARGET=module patches (no module checkout here: they are counted and named, never applied); that an .expect CMD or MESSAGE still matches
# what its gate prints; uncommitted changes (the pristine copy is HEAD).
# Every input line has `\r` stripped by the parser. Paired control: scripts/check-fault-patches-selftest.sh. Exit status: 0 green, 1 red, 2 usage error.
# Needs bash >= 4, git, tar.
set -u -o pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNLEG="$HERE/../ci/faults/run-leg.sh"

usage() { echo "usage: check-fault-patches.sh [--root DIR] [--min-faults N]" >&2; exit 2; }
ROOT_IN=""; MIN_FAULTS=1
while [ $# -gt 0 ]; do
  case "$1" in
    --root)         [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)       ROOT_IN="${1#*=}"; shift;;
    --min-faults)   [ $# -ge 2 ] || usage; MIN_FAULTS="$2"; shift 2;;
    --min-faults=*) MIN_FAULTS="${1#*=}"; shift;;
    -h|--help)      usage;;
    *)              echo "check-fault-patches: unknown argument: $1" >&2; usage;;
  esac
done
if ! [[ "$MIN_FAULTS" =~ ^[0-9]+$ ]] || [ "$((10#$MIN_FAULTS))" -lt 1 ]; then
  echo "check-fault-patches: --min-faults must be a whole number >= 1 (got '$MIN_FAULTS'): a floor of 0 would let an empty tree pass" >&2; usage
fi
MIN_FAULTS=$((10#$MIN_FAULTS))
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-fault-patches: --root '$ROOT_IN' is not a directory" >&2; exit 2; }
[ -f "$RUNLEG" ] || { echo "check-fault-patches: $RUNLEG not found (the shared .expect parser)" >&2; exit 2; }

T="$(mktemp -d)" || exit 2
trap 'rm -rf "$T"' EXIT
problems=0
problem() { problems=$((problems + 1)); echo "RED: $*"; }

PRISTINE="$T/pristine"; pristine_state=""   # "", "ok" or "failed"
ensure_pristine() {
  [ "$pristine_state" != ok ] || return 0
  [ "$pristine_state" != failed ] || return 1
  if mkdir -p "$PRISTINE" && git -C "$ROOT" archive HEAD | tar -x -C "$PRISTINE"; then pristine_state=ok; return 0; fi
  pristine_state=failed
  problem "cannot make a pristine copy of $ROOT with git archive HEAD (is it a git repository with a commit?)"
  return 1
}

FAULTS="$ROOT/ci/faults"
phases=(); found=0; n_checked=0; n_module=0; modlist=()
if [ -d "$FAULTS" ]; then
  for d in "$FAULTS"/*/; do
    [ -d "$d" ] || continue
    d="${d%/}"; phases+=("${d##*/}")
  done
fi

for ph in ${phases[@]+"${phases[@]}"}; do
  dir="$FAULTS/$ph"; names=(); n_exp=0
  if ! [[ "$ph" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; then problem "ci/faults/$ph/: the phase directory name must match ^[a-z0-9][a-z0-9_-]*\$ (the workflow and the runner refuse other names)"; fi
  for f in "$dir"/*.expect "$dir"/*.patch; do
    [ -f "$f" ] || continue
    b="${f##*/}"; names+=("${b%.*}")
    case "$b" in *.expect) n_exp=$((n_exp + 1));; esac
  done
  if [ "$n_exp" -eq 0 ]; then problem "ci/faults/$ph/ has no .expect file — the instrument matched nothing in this phase directory"; fi
  while IFS= read -r g; do
    [ -n "$g" ] || continue
    rel="ci/faults/$ph/$g"; status=""; tgt="?"
    if [ ! -f "$dir/$g.expect" ]; then problem "$rel.patch has no $g.expect"; printf '  %-34s %s\n' "$ph/$g" "RED: patch without an .expect"; continue; fi
    found=$((found + 1))
    out="$(cd "$ROOT" && bash "$RUNLEG" --check-expect "$rel.expect" 2>&1 </dev/null)"; rc=$?
    if [ "$rc" -eq 0 ]; then
      tgt="${out#ok TARGET=}"; tgt="${tgt%% *}"; status="expect ok"
    else
      status="RED: expect invalid"
      while IFS= read -r l; do [ -n "$l" ] && problem "$l"; done <<< "$out"
    fi
    if [ ! -f "$dir/$g.patch" ]; then
      problem "$rel.expect has no $g.patch"; status="$status; RED: no .patch"
    elif [ ! -s "$dir/$g.patch" ]; then
      problem "$rel.patch is empty"; status="$status; RED: empty .patch"
    elif [ "$rc" -eq 0 ]; then
      case "$tgt" in
        platform)
          if ensure_pristine; then
            if err="$(cd "$PRISTINE" && git apply --check --whitespace=nowarn -- "$dir/$g.patch" 2>&1 </dev/null)"; then
              n_checked=$((n_checked + 1)); status="$status; patch applies to HEAD"
            else
              problem "$rel.patch does not apply to a pristine copy of HEAD: $(printf '%s' "$err" | head -n 1)"; status="$status; RED: patch does not apply"
            fi
          else status="$status; RED: no pristine copy"; fi;;
        module:*)
          n_module=$((n_module + 1)); modlist+=("$ph/$g ($tgt)"); status="$status; patch NOT CHECKED (needs the module checkout)";;
      esac
    fi
    printf '  %-34s target=%s  %s\n' "$ph/$g" "$tgt" "$status"
  done < <(printf '%s\n' ${names[@]+"${names[@]}"} | LC_ALL=C sort -u)
done

if [ "$found" -eq 0 ]; then
  problem "found 0 faults: the instrument matched nothing (no ci/faults/<phase>/<gate>.expect under $ROOT)"
elif [ "$found" -lt "$MIN_FAULTS" ]; then
  problem "found $found fault(s), fewer than --min-faults $MIN_FAULTS — the instrument matched less than expected (faults were deleted, or a phase directory was renamed)"
fi

echo "check-fault-patches: ${#phases[@]} phase director(y/ies) under ci/faults; faults=$found platform-patches-applied=$n_checked module-patches-not-applied=$n_module (min-faults $MIN_FAULTS)"
if [ "$n_module" -gt 0 ]; then mods="${modlist[*]}"; else mods="none"; fi
echo "NOT CHECKED: whether any fault turns its gate red (only the dispatch-only fault-proofs workflow runs them);" \
     "$n_module TARGET=module patch(es), counted but never applied here: $mods;" \
     "that an .expect CMD or MESSAGE still matches what its gate prints; uncommitted changes (the pristine copy is HEAD); ci/faults-archive (a record, not run)"
if [ "$problems" -eq 0 ]; then
  echo "check-fault-patches: ok — $found fault(s), every .expect parses, every platform patch applies to HEAD"
  exit 0
fi
echo "check-fault-patches: RED ($problems problem(s))"
exit 1
