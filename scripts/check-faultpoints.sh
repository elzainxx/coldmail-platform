#!/usr/bin/env bash
# check-faultpoints.sh [--root DIR] — the fault points are a closed, registered set, and nothing that can be armed reaches a production build.
#
# WHY IT EXISTS. internal/faultpoint lets the harness kill the platform process, for real, at a named point of a job's life (Docs/phase-4-spec.md, the crash stage). That is only safe if three things stay true, and
# each is a thing a person could break without a test noticing: (1) the points are a closed set that a reader and the crash stage can list, so a Hit with a misspelt or unregistered name is an unarmable (never
# exercised) point and a registered name nothing hits any more is a crash test that proves nothing; (2) the code that reads the environment variable which arms a point exists ONLY in a build made with the tag
# `faultpoints`, so a production binary has no way to be told to kill itself; (3) no image recipe and no workflow builds with that tag: only the harness's compose file does. This gate checks all three.
#
# THE CHECKS (their scope is printed on every run):
#   (a) THE REGISTRY, internal/faultpoint/names.txt: one `<point>.<position>  # where it is hit` per line (lower-case letters, digits and _, one dot, a non-empty comment on every name; a `#` line is a comment).
#       A blank line, a duplicate name and a malformed line are RED. Every `faultpoint.Hit(...)` call in non-test Go under internal/ and cmd/ must pass a plain double-quoted string literal that names a registered
#       point (a variable, a concatenation, a raw string, an empty argument, or Hit used as a value cannot be read: RED), and every registered name must be hit by at least one such call that is not in a comment
#       (a stale name is RED). internal/faultpoint must be imported under its own name (an alias would hide every call from the matcher: RED).
#   (b) THE BUILD LINE: every non-test *.go under internal/, cmd/ and tests/ that names the fault-point environment variable (PLATFORM_FAULTPOINT, any case, a comment included) must START with the line
#       `//go:build faultpoints` (the first line of the file, nothing before it). The default file internal/faultpoint/faultpoint.go has `//go:build !faultpoints` and must not name the variable. At least one file names it
#       (the armed implementation: a tree where none does has lost it, or the gate no longer reads it).
#   (c) THE IMAGE RECIPE: the root Dockerfile has the line `ARG GO_TAGS=""` (textually, a comment does not count) and contains no `faultpoints` word at all; no other Dockerfile contains it either.
#   (d) THE TAG'S PLACES: the word `faultpoints` as a build tag may appear only in Go files whose first line is the build line (`//go:build faultpoints`, or the inert `//go:build !faultpoints`: a `//go:build` line that
#       names it, or a non-comment line with it beside `-tags` or GO_TAGS, in a Go file that does not start with one of the two is RED), in tests/harness/docker-compose*.yml (not read), in Markdown (not read), and in
#       .github/workflows/go.yml on a plain `go vet` line (nothing after it joined with && || ; | & $( or a backtick). Any other non-comment workflow line that names the tag is RED, and so is any non-comment
#       workflow line that names GO_TAGS at all (a workflow may build with an empty argument only by not mentioning it): `go build`, `go test`, `docker build --build-arg GO_TAGS=...`, GOFLAGS, all RED.
#
# WHAT IT SCANS (printed on every run): non-test *.go (not *_test.go) under <root>/internal and <root>/cmd for (a); the same plus <root>/tests for (b) and the Go half of (d); <root>/Dockerfile and every other file named
# Dockerfile, Dockerfile.* or *.Dockerfile; every <root>/.github/workflows/*.yml and *.yaml; the tests/harness/docker-compose*.yml files are counted and not read.
# HOW A HIT IS READ. Each file is normalised to one line first (CR, tab and newline become spaces, byte for byte), so a call split across lines is read as one and its line is recovered from the byte offset. A Hit is
# `faultpoint . Hit` as a whole word (the character before and after are not identifier characters): with a literal argument `( "name" )` (an optional trailing comma) it is a literal call, anything else is not. COMMENTS: a call
# in a comment is read like any other for the purposes of (a)'s FIRST half (it must be a literal naming a registered point: the gate fails closed, reword the comment), but does NOT count as a hit for the STALE half (a name
# that only a comment still hits is stale). A call is in a comment when a `//` comes before it on its line, or an unclosed `/*` comes before it in the file. This is textual: strings are not understood (a call after a
# quoted `//` on its line is taken for a comment and so is not counted as a live hit; a call in a string literal is counted as a hit).
# FLOORS (a gate whose subject is empty is red, classes 8 and 18): at least one Go file under internal/ and cmd/ is scanned; the registry exists and holds at least one name; at least one live Hit is found; at least one file
# names the environment variable; the root Dockerfile exists; at least one workflow file is scanned; and the matcher and the comment classifier pass their own canaries (known positives, negatives, two matches in a row,
# a call across lines, a comment) before they are trusted on the real tree. A grep that fails on a file (exit status above 1) is red: a crashed matcher prints no match, which would read as a clean file.
#
# Output: `check-faultpoints: scanned ...` lines, one line per finding `path:line: reason`, a `NOT CHECKED:` line, and `check-faultpoints: OK` or `RED`. Exit status: 0 clean, 1 red, 2 usage error.
# Paired control: scripts/check-faultpoints-selftest.sh. Fault proofs: ci/faults/p4b/faultpoint-armed (b) and ci/faults/p4b/faultpoint-stale-name (a).
#
# NOT CHECKED: printed on every run as the `NOT CHECKED:` line (the variable NOT_CHECKED below): test files, Hit names built at run time, the build tag set outside this repository, and whether the built image really lacks the armed code.
set -u
export LC_ALL=C

usage() { echo "usage: check-faultpoints.sh [--root DIR]" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)    [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)  ROOT_IN="${1#*=}"; shift;;
    -h|--help) usage;;
    *)         echo "check-faultpoints: unknown argument '$1'" >&2; usage;;
  esac
done
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

VAR='PLATFORM_FAULTPOINT'
REG='internal/faultpoint/names.txt'
TAGGED='//go:build faultpoints'
INERT='//go:build !faultpoints'
ARGLINE='ARG GO_TAGS=""'
SCOPE="(a) faultpoint.Hit calls in non-test *.go under internal/ and cmd/ against $REG; (b) non-test *.go under internal/, cmd/ and tests/ naming the fault-point environment variable; (c) the Dockerfiles; (d) the build tag in Go files, Dockerfiles and .github/workflows"
NOT_CHECKED="NOT CHECKED: test files (*_test.go); Go files outside internal/, cmd/ and tests/, and any Hit call under tests/ (the registry check reads internal/ and cmd/ only); the rest of tests/ (shell stages, scripts, docs) except that the compose files may name the tag and are not read; a Hit whose name is built at run time or reached through a variable (a Hit with a non-literal argument is RED, but the name behind it is never seen); an unqualified Hit called from inside package faultpoint itself; the variable's name built from fragments, and any non-Go file that names it (the harness's compose files and stages set it on purpose); the build tag set outside this repository (a workflow in another repository, a deployment, a developer's own build); a -tags argument inside Go source or a shell script; comments are classified textually (strings are not understood); whether the Dockerfile's go build really passes GO_TAGS on, whether go.yml still vets the tagged build, and whether the built image really lacks the armed code: the harness proves the tagged build kills a process, nothing proves the default image beyond (b), (c) and (d) here"

# ---- the matcher ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# ERE_ANY finds the selector `faultpoint . Hit`; word boundaries are checked afterwards (an ERE has no lookaround). ERE_LIT is the same followed by a plain double-quoted literal and the closing parenthesis.
# A selector at offset X is a literal call when ERE_LIT also matches at X; every other selector (a variable, a concatenation, a raw string, an empty argument, a value use) is not readable.
ERE_ANY='faultpoint[[:space:]]*\.[[:space:]]*Hit'
ERE_LIT='faultpoint[[:space:]]*\.[[:space:]]*Hit[[:space:]]*\([[:space:]]*"[^"\\]*"[[:space:]]*,?[[:space:]]*\)'

# prep <file>: reads a file ONCE for everything below: the normalised one-line copy (NORM), its lines and the byte offset each starts at (LINES, STARTS), and where every comment delimiter /* and */ lies (DEL_OFF, DEL_OPEN).
# Only linear bash operations are used on the text (a pattern substitution over a whole source file is quadratic and took seconds). Returns non-zero when grep ITSELF failed (a crashed grep prints no match, which would read
# as a clean file: grep exits 0 when it matched, 1 when it did not, and anything else is a failure).
NORM=""; LINES=(); STARTS=(); DEL_OFF=(); DEL_OPEN=()
prep() {
  local f="$1" rec i rc pos=0
  tr '\r\t\n' '   ' < "$f" > "$T/n" || return 98
  NORM="$(<"$T/n")"
  mapfile -t LINES < "$f"
  STARTS=(); for i in "${!LINES[@]}"; do STARTS[i]=$pos; pos=$((pos + ${#LINES[i]} + 1)); done
  grep -a -o -b -E '/\*|\*/' "$T/n" > "$T/g.del"; rc=$?; [ "$rc" -le 1 ] || return "$rc"
  DEL_OFF=(); DEL_OPEN=()
  while IFS= read -r rec; do DEL_OFF+=("${rec%%:*}"); case "${rec#*:}" in '/*') DEL_OPEN+=(1);; *) DEL_OPEN+=(0);; esac; done < "$T/g.del"
  return 0
}
# hits_of <file> <out>: (calls prep) writes one `offset|L|name` (a literal call) or `offset|N|` (a selector that is not a readable call) line per whole-word `faultpoint.Hit`, in file order. Non-zero when grep failed.
hits_of() {
  local f="$1" out="$2" rec off text prev next rc t name; local -A lit=()
  prep "$f"; rc=$?; [ "$rc" -eq 0 ] || return "$rc"
  grep -a -o -b -E "$ERE_LIT" "$T/n" > "$T/g.lit"; rc=$?; [ "$rc" -le 1 ] || return "$rc"
  grep -a -o -b -E "$ERE_ANY" "$T/n" > "$T/g.any"; rc=$?; [ "$rc" -le 1 ] || return "$rc"
  while IFS= read -r rec; do lit["${rec%%:*}"]="${rec#*:}"; done < "$T/g.lit"
  : > "$out"
  while IFS= read -r rec; do
    off="${rec%%:*}"; text="${rec#*:}"
    if [ "$off" -gt 0 ]; then prev="${NORM:$((off-1)):1}"; case "$prev" in [A-Za-z0-9_]) continue;; esac; fi
    next="${NORM:$((off+${#text})):1}"; case "$next" in [A-Za-z0-9_]) continue;; esac
    if [ -n "${lit[$off]+x}" ]; then t="${lit[$off]#*\"}"; name="${t%%\"*}"; printf '%s|L|%s\n' "$off" "$name" >> "$out"
    else printf '%s|N|\n' "$off" >> "$out"; fi
  done < "$T/g.any"
  return 0
}
# locate <offset>: (after prep) LN_R = the 1-based line of the byte offset, PRE_R = the part of that line before it
LN_R=0; PRE_R=""
locate() {
  local off="$1" i; LN_R=1; PRE_R=""
  for ((i=${#STARTS[@]}-1; i>=0; i--)); do
    if [ "${STARTS[i]}" -le "$off" ]; then LN_R=$((i+1)); PRE_R="${LINES[i]:0:$((off-STARTS[i]))}"; return 0; fi
  done
}
# in_comment <offset>: (after prep and locate) succeeds when the byte at <offset> is after a `//` on its own line, or after a `/*` that no `*/` has closed. Textual only: strings are not understood.
in_comment() {
  local off="$1" i open=0
  case "$PRE_R" in *'//'*) return 0;; esac
  for i in "${!DEL_OFF[@]}"; do [ "${DEL_OFF[i]}" -lt "$off" ] || break; open="${DEL_OPEN[i]}"; done
  [ "$open" -eq 1 ]
}
# line_of <file> <lower-case fixed string>: sets LN_R to the first line that contains it, in any case (bash only, so no external tool can fail silently); 0 when none
line_of() {
  local n=0 l; LN_R=0
  while IFS= read -r l || [ -n "$l" ]; do n=$((n+1)); l="${l,,}"; case "$l" in *"$2"*) LN_R=$n; return 0;; esac; done < "$1"
  return 1
}
trim_left() { TL_R="${1#"${1%%[![:space:]]*}"}"; }
trim_both() { local s="${1#"${1%%[![:space:]]*}"}"; TB_R="${s%"${s##*[![:space:]]}"}"; }

# canary <want> <text>: the matcher must find exactly <want> (the space-separated `L:name` / `N` of each selector, in order) in <text>. All the canaries go through ONE pass of the real matcher (each is followed by a
# separator that no match can span), and each match is credited to the canary whose byte range holds its offset.
CANARY_WANT=(); CANARY_TEXT=()
canary() { CANARY_WANT+=("$1"); CANARY_TEXT+=("$2"); }
run_canaries() {
  local i pos=0 off kind name rec rc bad=0 got; local -a start=() res=()
  : > "$T/c.in"
  for i in "${!CANARY_TEXT[@]}"; do
    start[i]=$pos; res[i]=""
    printf '%s\n@@@\n' "${CANARY_TEXT[i]}" >> "$T/c.in"; pos=$((pos + ${#CANARY_TEXT[i]} + 5))
  done
  hits_of "$T/c.in" "$T/c.off"; rc=$?
  if [ "$rc" -ne 0 ]; then echo "  canary: grep itself failed (exit status $rc)"; return 99; fi
  while IFS='|' read -r off kind name; do
    [ -n "$off" ] || continue
    for i in "${!start[@]}"; do
      if [ "$off" -ge "${start[i]}" ] && { [ -z "${start[i+1]:-}" ] || [ "$off" -lt "${start[i+1]}" ]; }; then
        if [ "$kind" = L ]; then got="L:$name"; else got="N"; fi
        res[i]="${res[i]:+${res[i]} }$got"; break
      fi
    done
  done < "$T/c.off"
  for i in "${!CANARY_TEXT[@]}"; do
    if [ "${res[i]}" != "${CANARY_WANT[i]}" ]; then echo "  canary: wanted '${CANARY_WANT[i]}', found '${res[i]}', in: ${CANARY_TEXT[i]}"; bad=$((bad+1)); fi
  done
  return "$bad"
}
# the positives: a literal call in every shape the formatter or a person writes it, two in a row, a call across lines
canary 'L:a.b' 'faultpoint.Hit("a.b")'
canary 'L:a.b' 'faultpoint . Hit ( "a.b" )'
canary 'L:a.b' "$(printf 'faultpoint.Hit(\n\t"a.b",\n)')"
canary 'L:a.b' 'faultpoint.Hit("a.b",)'
canary 'L:a.b' 'if ok { faultpoint.Hit("a.b") }'
canary 'L:a.b' '// faultpoint.Hit("a.b") in a comment is still read'
canary 'L:' 'faultpoint.Hit("")'
canary 'L:a.b L:c.d' 'faultpoint.Hit("a.b"); faultpoint.Hit("c.d")'
canary 'L:a.b L:c.d' 'faultpoint.Hit("a.b")faultpoint.Hit("c.d")'
canary 'L:a.b L:c.d' "$(printf 'faultpoint.Hit("a.b")\nfaultpoint.Hit("c.d")')"
# a selector that is not a readable call: a variable, a concatenation, a raw string, nothing, an escape, a second argument, the function as a value
canary 'N' 'faultpoint.Hit(name)'
canary 'N' 'faultpoint.Hit("a." + suffix)'
canary 'N' 'faultpoint.Hit(prefix + "a.b")'
canary 'N' 'faultpoint.Hit("a.b" + "c")'
canary 'N' 'faultpoint.Hit(`a.b`)'
canary 'N' 'faultpoint.Hit()'
canary 'N' 'faultpoint.Hit("a\"b")'
canary 'N' 'faultpoint.Hit("a.b", extra)'
canary 'N' 'f := faultpoint.Hit'
canary 'N' 'defer faultpoint.Hit ; x'
canary 'N L:a.b' 'faultpoint.Hit(x); faultpoint.Hit("a.b")'
canary 'L:a.b N' 'faultpoint.Hit("a.b"); faultpoint.Hit(x)'
# the negatives: a longer word, another package, an unqualified call, the constant, prose
canary '' 'faultpoint.Hits("a.b")'
canary '' 'faultpoint.Hitch("a.b")'
canary '' 'faultpoint.HitX(name)'
canary '' 'myfaultpoint.Hit("a.b")'
canary '' 'xfaultpoint . Hit("a.b")'
canary '' 'faultpoints.Hit("a.b")'
canary '' 'faultpoint_on.Hit("a.b")'
canary '' 'Hit("a.b")'
canary '' 'faultpoint.Enabled'
canary '' 'faultpoint: armed "a.b"'
canary '' 'the faultpoint package decides'
canary '' 'faultpoint.hit("a.b")'
run_canaries; canary_bad=$?
# the comment classifier: a call is in a comment after // on its line or after an unclosed /*, and in no other case. All the cases go through ONE prep (each text is followed by a line that closes any block comment the text
# left open, so no case can inherit another's state); the call under test is the LAST `faultpoint` of its text.
CC_WANT=(); CC_TEXT=()
cc() { CC_WANT+=("$1"); CC_TEXT+=("$2"); }
cc no  'faultpoint.Hit("a.b")'
cc no  'x := 1; faultpoint.Hit("a.b")'
cc yes 'x := 1 // faultpoint.Hit("a.b")'
cc yes '// faultpoint.Hit("a.b")'
cc yes '/* faultpoint.Hit("a.b") */'
cc no  '/* a */ faultpoint.Hit("a.b")'
cc yes "$(printf '/*\nfaultpoint.Hit("a.b")\n*/')"
cc yes "$(printf '/* one */\n/*\ntwo\nfaultpoint.Hit("a.b")')"
cc no  "$(printf '/*\nfaultpoint.Hit("a.b")\n*/\n/* x */ faultpoint.Hit("c.d")')"
cc no  "$(printf '// a comment\nx := 1\nfaultpoint.Hit("a.b")')"
cc no  "$(printf '/* closed */\nfaultpoint.Hit("a.b")')"
cc_bad=0
run_cc() {
  local i pos=0 pre got; local -a at=()
  : > "$T/cc.in"
  for i in "${!CC_TEXT[@]}"; do
    pre="${CC_TEXT[i]%faultpoint*}"; at[i]=$((pos + ${#pre}))
    printf '%s\n*/\n' "${CC_TEXT[i]}" >> "$T/cc.in"; pos=$((pos + ${#CC_TEXT[i]} + 4))
  done
  if ! prep "$T/cc.in"; then echo "  canary: grep itself failed"; cc_bad=$((cc_bad+1)); return; fi
  for i in "${!CC_TEXT[@]}"; do
    locate "${at[i]}"; if in_comment "${at[i]}"; then got=yes; else got=no; fi
    if [ "$got" != "${CC_WANT[i]}" ]; then echo "  canary: comment classifier wanted '${CC_WANT[i]}', found '$got', in: ${CC_TEXT[i]}"; cc_bad=$((cc_bad+1)); fi
  done
}
run_cc
if [ "$canary_bad" -gt 0 ] || [ "$cc_bad" -gt 0 ]; then
  echo "check-faultpoints: RED — the matcher failed its own canary ($((canary_bad + cc_bad)) case(s) above): the instrument cannot be trusted (is grep -o -b -E available?)"
  exit 1
fi

# ---- the subject ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-faultpoints: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }
REGF="$ROOT/$REG"

: > "$T/go.abs"; : > "$T/tgo.abs"
for d in internal cmd; do
  if [ -d "$ROOT/$d" ]; then find "$ROOT/$d" -type f -name '*.go' ! -name '*_test.go' >> "$T/go.abs"; fi
done
if [ -d "$ROOT/tests" ]; then find "$ROOT/tests" -type f -name '*.go' ! -name '*_test.go' >> "$T/tgo.abs"; fi
cat "$T/go.abs" "$T/tgo.abs" | sort | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/gofiles"
sort "$T/go.abs" | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/go.rel"
ngo=$(wc -l < "$T/go.rel" | tr -d ' '); ngoall=$(wc -l < "$T/gofiles" | tr -d ' ')

: > "$T/wf.abs"
if [ -d "$ROOT/.github/workflows" ]; then find "$ROOT/.github/workflows" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) >> "$T/wf.abs"; fi
sort "$T/wf.abs" | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/wf.rel"
nwf=$(wc -l < "$T/wf.rel" | tr -d ' ')
find "$ROOT" \( -name .git -o -name node_modules \) -prune -o -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' -o -name '*.Dockerfile' \) -print 2>/dev/null | sort | while IFS= read -r f; do printf '%s\n' "${f#"$ROOT"/}"; done > "$T/df.rel"
ndf=$(wc -l < "$T/df.rel" | tr -d ' ')
ncompose=0; if [ -d "$ROOT/tests/harness" ]; then ncompose=$(find "$ROOT/tests/harness" -maxdepth 1 -type f -name 'docker-compose*.yml' | wc -l | tr -d ' '); fi

echo "check-faultpoints: scope: $SCOPE (root: $ROOT)"
echo "check-faultpoints: scanned $ngo Go file(s) under internal/ and cmd/ ($ngoall with tests/), $nwf workflow file(s), $ndf Dockerfile(s); $ncompose compose file(s) under tests/harness are allowed to name the tag and are not read"
if [ "$ngo" -lt 1 ]; then
  echo "check-faultpoints: RED — found 0 Go files under internal/ and cmd/ in $ROOT: the instrument matched nothing (a wrong --root, or an empty checkout)"
  exit 1
fi

fail=0; nreg=0; nhit=0; nstale=0; nvar=0; ngotag=0; ndfbad=0; nwfbad=0; nfloor=0; nbad_files=0
problem() { echo "$1"; fail=$((fail+1)); }

# ---- (a) the registry -----------------------------------------------------------------------------------------------------------------------------------------------------------------
declare -A NAME_LINE=() LIVE_N=() COMM_N=()
names=(); REG_OK=1
if [ ! -f "$REGF" ]; then
  problem "$REG:1: the registry does not exist: the gate cannot say which fault points are allowed (create it: one '<point>.<position>  # where it is hit' per line)"; nreg=$((nreg+1)); REG_OK=0
else
  n=0
  re_named='^([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)[[:space:]]+#(.*)$'
  re_bare='^([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)[[:space:]]*$'
  while IFS= read -r raw || [ -n "$raw" ]; do
    n=$((n+1)); line="${raw%$'\r'}"; trim_left "$line"
    if [ -z "$TL_R" ]; then problem "$REG:$n: blank line: the registry holds one name per line and nothing else (a comment line starts with #)"; nreg=$((nreg+1)); continue; fi
    case "$TL_R" in '#'*) continue;; esac
    name=""; cmt=""; has=0
    if [[ "$line" =~ $re_named ]]; then name="${BASH_REMATCH[1]}"; trim_both "${BASH_REMATCH[2]}"; cmt="$TB_R"; has=1
    elif [[ "$line" =~ $re_bare ]]; then name="${BASH_REMATCH[1]}"; has=1; fi
    if [ "$has" -eq 0 ]; then problem "$REG:$n: malformed line (want '<point>.<position>  # where it is hit': lower-case letters, digits and _ only, one dot, no leading space): $line"; nreg=$((nreg+1)); continue; fi
    if [ -n "${NAME_LINE[$name]+x}" ]; then problem "$REG:$n: duplicate name '$name' (first on line ${NAME_LINE[$name]})"; nreg=$((nreg+1)); continue; fi
    NAME_LINE[$name]="$n"; names+=("$name")
    if [ -z "$cmt" ]; then problem "$REG:$n: name '$name' has no comment saying where it is hit (want '<name>  # where it is hit')"; nreg=$((nreg+1)); fi
  done < "$REGF"
fi
nnames="${#names[@]}"

# ---- one pass over the Go files: (a) the hits, (b) the build line, (d) the tag in Go ---------------------------------------------------------------------------------------------------
nlive=0; ncomm=0; nvarfiles=0
while IFS= read -r rel; do
  f="$ROOT/$rel"
  content="$(<"$f")"
  IFS= read -r first < "$f" || true; first="${first%$'\r'}"   # (a last line without a newline still fills `first`; an empty file leaves it empty)
  # (b) the build line
  case "${content^^}" in
    *"$VAR"*)
      nvarfiles=$((nvarfiles+1))
      if [ "$first" != "$TAGGED" ]; then
        line_of "$f" "${VAR,,}"
        problem "$rel:$LN_R: names the fault-point environment variable $VAR but does not start with the line $TAGGED: production code (built without the tag) must hold nothing that can be armed"; nvar=$((nvar+1))
      fi;;
  esac
  # (d) the tag in a Go file
  case "$content" in
    *faultpoints*)
      tag_line=0; n=0
      while IFS= read -r l || [ -n "$l" ]; do
        n=$((n+1)); l="${l%$'\r'}"; trim_left "$l"
        case "$TL_R" in
          '//go:build'*faultpoints*) tag_line=$n; break;;
          '//'*) ;;
          *faultpoints*) case "$TL_R" in *-tags*|*GO_TAGS*) tag_line=$n; break;; esac;;
        esac
      done < "$f"
      if [ "$tag_line" -gt 0 ] && [ "$first" != "$TAGGED" ] && [ "$first" != "$INERT" ]; then
        problem "$rel:$tag_line: uses the build tag faultpoints but the file does not start with $TAGGED (or the inert $INERT): the tag may appear only in files that carry the build line"; ngotag=$((ngotag+1))
      fi;;
  esac
  # (a) the hits (internal/ and cmd/ only)
  case "$rel" in internal/*|cmd/*) ;; *) continue;; esac
  case "$content" in *faultpoint*) ;; *) continue;; esac
  grep -n -a -F 'internal/faultpoint"' "$f" > "$T/imp"; rc=$?
  if [ "$rc" -gt 1 ]; then echo "check-faultpoints: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue; fi
  while IFS= read -r rec; do
    [ -n "$rec" ] || continue
    ln="${rec%%:*}"; trim_left "${rec#*:}"; t="$TL_R"
    case "$t" in '//'*|'/*'*) continue;; esac
    case "$t" in import[[:space:]]*) trim_left "${t#import}"; t="$TL_R";; esac
    case "$t" in '"'*) ;; *) problem "$rel:$ln: internal/faultpoint is imported under another name or for its side effects: the gate reads only faultpoint.Hit calls, so a renamed import would hide every hit — import it under its own name"; nhit=$((nhit+1));; esac
  done < "$T/imp"
  hits_of "$f" "$T/h.off"; rc=$?
  if [ "$rc" -ne 0 ]; then echo "check-faultpoints: the matcher failed on $rel (grep exit status $rc): the file was not read, so a pass would prove nothing"; nbad_files=$((nbad_files+1)); continue; fi
  while IFS='|' read -r off kind name; do
    [ -n "$off" ] || continue
    locate "$off"; ln="$LN_R"
    if [ "$kind" = N ]; then
      problem "$rel:$ln: faultpoint.Hit is not called with a plain double-quoted string literal: the gate cannot read the point's name, so it cannot know it is registered (pass the name itself; a comment that shows the call must be reworded: comments are not stripped)"; nhit=$((nhit+1)); continue
    fi
    if in_comment "$off"; then ncomm=$((ncomm+1)); live=0; else nlive=$((nlive+1)); live=1; fi
    if [ "$REG_OK" -eq 1 ] && { ! [[ "$name" =~ ^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$ ]] || [ -z "${NAME_LINE[$name]+x}" ]; }; then
      problem "$rel:$ln: faultpoint.Hit(\"$name\") names a point that $REG does not list — add it there with a comment saying where it is hit, or fix the spelling (a Hit in a comment counts too: comments are not stripped)"; nhit=$((nhit+1)); continue
    fi
    if [ "$REG_OK" -eq 1 ]; then
      if [ "$live" -eq 1 ]; then LIVE_N[$name]=$(( ${LIVE_N[$name]:-0} + 1 )); else COMM_N[$name]=$(( ${COMM_N[$name]:-0} + 1 )); fi
    fi
  done < "$T/h.off"
done < "$T/gofiles"

# the stale half of (a): every registered name is hit, outside a comment, at least once
for name in ${names[@]+"${names[@]}"}; do
  if [ "${LIVE_N[$name]:-0}" -lt 1 ]; then
    extra=""; [ "${COMM_N[$name]:-0}" -lt 1 ] || extra=" (it is named in a comment only: a comment is not a hit)"
    problem "$REG:${NAME_LINE[$name]}: stale name '$name': no non-test Go file under internal/ or cmd/ calls faultpoint.Hit(\"$name\") outside a comment$extra — remove the name or restore the hit (a register that rots hides the next miss)"; nstale=$((nstale+1))
  fi
done
npoints=0; for name in ${names[@]+"${names[@]}"}; do [ "${LIVE_N[$name]:-0}" -lt 1 ] || npoints=$((npoints+1)); done
echo "check-faultpoints: registry $REG lists $nnames name(s); found $nlive faultpoint.Hit call(s) outside comments ($ncomm more in comments, not counted as hits) covering $npoints registered point(s); $nvarfiles Go file(s) name the fault-point environment variable"

# ---- the floors -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
floor() { problem "$1"; nfloor=$((nfloor+1)); }
if [ "$REG_OK" -eq 1 ] && [ "$nnames" -lt 1 ]; then floor "$REG:1: the registry holds 0 names: the instrument matched nothing (a registry with no fault point proves nothing about the crash stage; comments alone do not count)"; fi
if [ "$nlive" -lt 1 ]; then floor "check-faultpoints: found 0 faultpoint.Hit calls outside comments in non-test Go under internal/ and cmd/: the instrument matched nothing (no fault point is hit, or the matcher no longer reads the code)"; fi
if [ "$nvarfiles" -lt 1 ]; then floor "check-faultpoints: no non-test Go file under internal/, cmd/ or tests/ names the fault-point environment variable $VAR: the instrument matched nothing (the armed implementation is gone, or the gate no longer reads it)"; fi
if [ "$nwf" -lt 1 ]; then floor "check-faultpoints: found 0 workflow files under .github/workflows in $ROOT: the instrument matched nothing (the tag rule has no subject)"; fi

# ---- (c) and (d) the Dockerfiles ------------------------------------------------------------------------------------------------------------------------------------------------------
if [ ! -f "$ROOT/Dockerfile" ]; then
  problem "Dockerfile:1: the root Dockerfile does not exist: the instrument matched nothing (the image the harness builds with the tag is built from it)"; ndfbad=$((ndfbad+1))
else
  has_arg=0
  while IFS= read -r l || [ -n "$l" ]; do l="${l%$'\r'}"; [ "$l" != "$ARGLINE" ] || { has_arg=1; break; }; done < "$ROOT/Dockerfile"
  if [ "$has_arg" -eq 0 ]; then problem "Dockerfile:1: the line $ARGLINE is missing (the build argument that carries the tag must default to empty, so an image built without the argument has no armed code)"; ndfbad=$((ndfbad+1)); fi
fi
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  n=0
  while IFS= read -r l || [ -n "$l" ]; do
    n=$((n+1)); case "${l,,}" in *faultpoints*) problem "$rel:$n: the word faultpoints in a Dockerfile: the tag is passed to a build by the harness's compose file only, never written into an image recipe"; ndfbad=$((ndfbad+1));; esac
  done < "$ROOT/$rel"
done < "$T/df.rel"

# ---- (d) the workflows ------------------------------------------------------------------------------------------------------------------------------------------------------------------
# a plain go vet line: after an optional list dash and `run:` it starts with `go vet ` and joins nothing after it
vet_line_ok() {
  local t="$1"; trim_left "$t"; t="$TL_R"; t="${t#- }"; t="${t#run: }"; trim_left "$t"; t="$TL_R"
  case "$t" in 'go vet '*) ;; *) return 1;; esac
  case "$t" in *'&'*|*'|'*|*';'*|*'$('*|*'`'*|*'>'*|*'<'*) return 1;; esac
  return 0
}
nvet=0
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  n=0
  while IFS= read -r l || [ -n "$l" ]; do
    n=$((n+1)); l="${l%$'\r'}"; trim_left "$l"
    case "$TL_R" in '#'*) continue;; esac
    case "${l^^}" in
      *GO_TAGS*) problem "$rel:$n: a workflow names GO_TAGS: the tag goes into an image through the harness's compose file only, never from a workflow (an empty argument is the default and needs no mention)"; nwfbad=$((nwfbad+1)); continue;;
    esac
    lw="${l,,}"; lw="${lw//check-faultpoints/}"   # the gate's own name (its step, its script path, its selftest) is not the tag
    case "$lw" in
      *faultpoints*)
        if [ "$rel" = ".github/workflows/go.yml" ] && vet_line_ok "$l"; then nvet=$((nvet+1))
        else problem "$rel:$n: a workflow line names the build tag faultpoints outside a plain go vet line of go.yml: a go build, go test, docker or any other use would produce or run armed code"; nwfbad=$((nwfbad+1)); fi;;
    esac
  done < "$ROOT/$rel"
done < "$T/wf.rel"
echo "check-faultpoints: $nvet go vet line(s) in .github/workflows/go.yml name the tag (allowed: vet compiles the tagged files and builds nothing)"

# ---- the verdict ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
echo "check-faultpoints: $nreg registry problem(s), $nhit hit problem(s), $nstale stale name(s), $nvar build-line problem(s), $ngotag Go tag problem(s), $ndfbad Dockerfile problem(s), $nwfbad workflow problem(s), $nfloor floor(s), $nbad_files file(s) the matcher failed on"
echo "$NOT_CHECKED"
total=$((fail + nbad_files))
[ "$total" -eq 0 ] || { echo "check-faultpoints: RED — $total problem(s): $nreg registry, $nhit hit, $nstale stale, $nvar build-line, $ngotag Go tag, $ndfbad Dockerfile, $nwfbad workflow, $nfloor floor, $nbad_files unread file(s)"; exit 1; }
echo "check-faultpoints: OK"
