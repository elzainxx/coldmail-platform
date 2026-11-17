#!/usr/bin/env bash
# check-personal-details.sh --root DIR --allow FILE — BLOCKING gate: the tracked tree of a public repository names no Windows home path, no drive-letter path and no IPv4 address that is not allowed.
#
# WHY IT EXISTS. The five repositories are public (owner decision 2026-11-17). The pre-publication audit found a Windows user name in a home path, the layout of the owner's disks (drive-letter paths) and one residential
# IP address in files that were committed years of work ago; history cannot be rewritten, so what the CURRENT tree says is all a gate can keep clean. This gate fails on every NEW occurrence.
#
# WHAT IT READS. The files `git ls-files` lists when DIR has a .git (a checkout, a submodule), otherwise every file below DIR except .git. Symlinks and submodule entries are not followed (counted); binary
# files (what `grep -I` calls binary: a NUL byte) and empty files are skipped (counted). Line endings are irrelevant (CR is stripped). The history is never read.
#
# WHAT IT FLAGS (one RED line per occurrence: `path:line: <class>: <literal>`):
#   Windows home path    <letter>:\Users\  <letter>:\\Users\\  <letter>:/Users/    (any case; one to four backslashes: the escaped forms of JSON and Go strings)
#   drive-letter path    <letter>:\ or <letter>:/ followed by a name of at least TWO characters ([A-Za-z0-9_.$~@-]), in either case of the letter. The letter must stand alone: not preceded by a letter, digit, _ % $ \ or /
#                        (so `log:\n` of a Go string, `Code:\s+` of a regexp, `%s:\x` of a printf and `https://x` are not drive paths; `file:///C:/x` is one). A name of ONE character is not read (`x:\n` of a printf).
#                        `/Users/<name>/` is not in scope.
#   IPv4 address         four dot-separated numbers 0-255 not glued to a letter, digit, _ or . on the left, nor to a letter, digit, _ or a further .digit on the right (so 1.2.3.4.5 and v1.2.3.4 are versions, not
#                        addresses). Allowed with NO entry (documentation, private and special-use classes): 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 127.0.0.0/8, 0.0.0.0, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16,
#                        169.254.0.0/16, 100.64.0.0/10, 224.0.0.0/4 (multicast) and 255.255.255.255 (broadcast). An octet with a leading zero (the four-part version tag of a container image is one) is never classified: it needs an entry.
#
# THE ALLOWLIST (--allow FILE; the platform keeps one per repository under tests/personal-details/): one entry per line, `path-glob|literal|reason`; blank lines and lines starting with # are ignored.
#   path-glob  matched against the path below the root with bash [[ == ]] (a * crosses /); a leading `**/` also matches the file at the top. `literal` is the address (or the reported drive-and-first-name form of a path) or `*` for any
#   literal in the matched files. THE REASON IS MANDATORY (at least 10 characters: what makes this a fixture, a documented public service or a version number). A glob made only of wildcards is refused
#   (fewer than four characters that are not * ? [ ] /), a duplicate entry is refused, and AN ENTRY THAT MATCHES NOTHING IS RED: the list cannot rot (a finding gone, a file moved, an address that is now of an
#   allowed class). A finding no entry covers is RED.
#
# FLOORS (a gate whose subject is empty is red, classes 8 and 18): a root that is not a directory, a missing allowlist, no text file read at all, a path with a newline or tab in it (the gate cannot attribute findings).
# Output: what was read and skipped, how many addresses were allowed by class, how many findings the allowlist covers, one RED line per problem, and a `NOT CHECKED:` line. Exit status: 0 green, 1 red, 2 usage error.
# Paired control: scripts/check-personal-details-selftest.sh (every reject case asserts the reason it fails for).
#
# NOT CHECKED (also printed on every run): the commit history (only the current tree is read: history cannot be rewritten, gitleaks reads it for secrets); files git does not track when DIR is a repository;
# binary files and anything encoded (base64, compressed); IPv6 addresses, addresses not written as four dotted numbers, host names and e-mail addresses; paths of other shapes (`/Users/<name>/`, `/home/<name>/`, an MSYS
# `/d/...` path, a UNC path); a drive path whose first name is one character; the owner's NAME in prose (it stays by decision).
set -u
export LC_ALL=C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOT_CHECKED="NOT CHECKED: the commit history (only the current tree is read); files git does not track when the root is a repository; binary and encoded files; IPv6 addresses, addresses not written as four dotted numbers, host names and e-mail addresses; paths of other shapes (/Users/<name>/, /home/<name>/, an MSYS /d/... path, a UNC path); a drive path whose first name is one character; the owner's name in prose (it stays by decision)"
usage() { echo "usage: check-personal-details.sh --root DIR --allow FILE" >&2; exit 2; }
ROOT_IN=""; ALLOW_IN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --root)    [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)  ROOT_IN="${1#*=}"; shift;;
    --allow)   [ $# -ge 2 ] || usage; ALLOW_IN="$2"; shift 2;;
    --allow=*) ALLOW_IN="${1#*=}"; shift;;
    -h|--help) usage;;
    *) echo "check-personal-details: unknown argument '$1'" >&2; usage;;
  esac
done
[ -n "$ROOT_IN" ] && [ -n "$ALLOW_IN" ] || usage

T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
fail=0
bad() { echo "  RED: $*"; fail=$((fail+1)); }
die() { echo "check-personal-details: RED — $*"; echo "$NOT_CHECKED"; exit 1; }

ROOT="$(cd "$ROOT_IN" 2>/dev/null && pwd)" || die "the root '$ROOT_IN' is not a directory: the instrument matched nothing"
[ -f "$ALLOW_IN" ] || die "the allowlist '$ALLOW_IN' does not exist (an allowlist with no entries is a file with only comments: a missing one is a gate that was never configured)"

# ---- the allowlist ------------------------------------------------------------------------------------------------------------------------------------------------------------------
EG=(); EL=(); ER=(); EN=(); EH=(); SEEN=$'\n'
trim() { local s="$1"; s="${s#"${s%%[![:space:]]*}"}"; s="${s%"${s##*[![:space:]]}"}"; printf '%s' "$s"; }
lineno=0
while IFS= read -r raw || [ -n "$raw" ]; do
  lineno=$((lineno+1)); raw="${raw%$'\r'}"
  l="$(trim "$raw")"
  case "$l" in ''|'#'*) continue;; esac
  IFS='|' read -r g lit why <<< "$l"
  g="$(trim "$g")"; lit="$(trim "${lit:-}")"; why="$(trim "${why:-}")"
  where="allowlist $ALLOW_IN line $lineno"
  if [ -z "$g" ] || [ -z "$lit" ]; then bad "$where: not path-glob|literal|reason (got '$l')"; continue; fi
  if [ -z "$why" ]; then bad "$where: the entry '$g|$lit' has no reason: a reason is mandatory (what makes it a fixture, a documented public service or a version number)"; continue; fi
  if [ "${#why}" -lt 10 ]; then bad "$where: the reason '$why' is too short to be a reason (at least 10 characters)"; continue; fi
  case "$g" in /*|*..*|[A-Za-z]:*) bad "$where: the glob '$g' is not a path below the repository root"; continue;; esac
  core="${g//[\*\?\[\]\/]/}"
  if [ "${#core}" -lt 4 ]; then bad "$where: the glob '$g' is too broad (fewer than four characters that are not wildcards or /): name the file or the directory"; continue; fi
  case "$SEEN" in *$'\n'"$g|$lit"$'\n'*) bad "$where: the entry '$g|$lit' is listed twice"; continue;; esac
  SEEN+="$g|$lit"$'\n'
  EG+=("$g"); EL+=("$lit"); ER+=("$why"); EN+=("$lineno"); EH+=(0)
done < "$ALLOW_IN"
nent=${#EG[@]}

glob_match() { # glob_match PATH GLOB
  # shellcheck disable=SC2053  # the glob is meant to be a pattern
  [[ "$1" == $2 ]] && return 0
  case "$2" in '**/'*) [[ "$1" == ${2#'**/'} ]] && return 0;; esac
  return 1
}

# ---- the files ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
cd "$ROOT" || die "cannot enter '$ROOT'"
nlink=0; nsub=0; mode_name="file tree"
if [ -e "$ROOT/.git" ]; then
  mode_name="git ls-files"
  command -v git > /dev/null 2>&1 || die "$ROOT has a .git and git is not on PATH: the tracked files cannot be listed"
  prefix="$(git -C "$ROOT" rev-parse --show-prefix 2>/dev/null)" || die "$ROOT has a .git that git cannot read"
  [ -z "$prefix" ] || die "$ROOT is inside a repository, not its top level (prefix '$prefix')"
  git -C "$ROOT" ls-files -s -z > "$T/ls.z" || die "git ls-files failed in $ROOT"
  nrec="$(tr -cd '\0' < "$T/ls.z" | wc -c | tr -d ' ')"
  [ "$(tr '\0' '\n' < "$T/ls.z" | wc -l | tr -d ' ')" -eq "$nrec" ] || die "a path in $ROOT has a newline in its name: the gate cannot attribute findings to it"
  tr '\0' '\n' < "$T/ls.z" | awk -F'\t' -v out="$T/links" '
    NF >= 2 { m = substr($1, 1, 6); p = substr($0, index($0, "\t") + 1)
      if (m == "100644" || m == "100755") print p; else if (m == "120000") l++; else if (m == "160000") s++ }
    END { printf "%d %d\n", l + 0, s + 0 > out }' | sort -u > "$T/paths"
  read -r nlink nsub < "$T/links"
else
  find . -name .git -prune -o -type f -print0 > "$T/find.z" || die "find failed in $ROOT"
  nrec="$(tr -cd '\0' < "$T/find.z" | wc -c | tr -d ' ')"
  tr '\0' '\n' < "$T/find.z" | sed 's#^\./##' | sort -u > "$T/paths"
  nlink="$(find . -name .git -prune -o -type l -print | wc -l | tr -d ' ')"
fi
npaths="$(wc -l < "$T/paths" | tr -d ' ')"
if grep -q "$(printf '\t')" "$T/paths"; then die "a path in $ROOT has a tab in its name: the gate cannot attribute findings to it"; fi
if [ "$mode_name" = "file tree" ] && [ "$npaths" -ne "$nrec" ]; then die "a path in $ROOT has a newline in its name: the gate cannot attribute findings to it"; fi
if [ "$npaths" -eq 0 ]; then die "found 0 files below $ROOT ($mode_name): the instrument matched nothing"; fi
tr '\n' '\0' < "$T/paths" > "$T/paths.z"

# text, binary and empty files: `grep -I -c ''` counts the lines of a text file and says 0 for a binary one
xargs -r -0 grep -I -c -H -Z -e '' -- < "$T/paths.z" 2> "$T/count.err" | tr '\0' '\001' | awk -v zero="$T/zero" '
  { i = index($0, "\001"); c = substr($0, i + 1) + 0; if (c > 0) t++; else print substr($0, 1, i - 1) > zero }
  END { printf "%d\n", t + 0 }' > "$T/ntext"
[ -s "$T/count.err" ] && die "grep could not read some of the files of $ROOT ($mode_name): $(head -n 1 "$T/count.err" | cut -c1-200)"
ntext="$(cat "$T/ntext")"; nbin=0; nempty=0
if [ -f "$T/zero" ]; then
  while IFS= read -r z; do if [ -s "$z" ]; then nbin=$((nbin+1)); else nempty=$((nempty+1)); fi; done < "$T/zero"
fi
if [ "$ntext" -eq 0 ]; then die "found 0 text files to read below $ROOT ($mode_name; $npaths path(s), $nbin binary, $nempty empty): the instrument matched nothing"; fi

# ---- the candidates, then the exact reading ----------------------------------------------------------------------------------------------------------------------------------
IP_CAND='[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+'
DRIVE_CAND='[A-Za-z]:[\\/]'
xargs -r -0 grep -I -n -H -Z -E -e "$IP_CAND" -e "$DRIVE_CAND" -- < "$T/paths.z" 2> "$T/scan.err" | tr '\0' '\001' | awk -v defout="$T/defaults" '
BEGIN {
  NAMECH = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.$~@-"
  LEFTBAD = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_%$\\/"
  IPRE = "[0-9][0-9]?[0-9]?[.][0-9][0-9]?[0-9]?[.][0-9][0-9]?[0-9]?[.][0-9][0-9]?[0-9]?"
  DRVRE = "[A-Za-z]:[\\\\/]"
  defn = 0
}
function lead0(s) { return (length(s) > 1 && substr(s, 1, 1) == "0") }
function allowed_class(o,   a, b, c, d) {
  if (lead0(o[1]) || lead0(o[2]) || lead0(o[3]) || lead0(o[4])) return 0
  a = o[1] + 0; b = o[2] + 0; c = o[3] + 0; d = o[4] + 0
  if (a == 127) return 1                                   # loopback
  if (a == 0 && b == 0 && c == 0 && d == 0) return 1       # the unspecified address
  if (a == 10) return 1                                    # private
  if (a == 172 && b >= 16 && b <= 31) return 1             # private
  if (a == 192 && b == 168) return 1                       # private
  if (a == 169 && b == 254) return 1                       # link-local
  if (a == 100 && b >= 64 && b <= 127) return 1            # carrier-grade NAT
  if (a == 192 && b == 0 && c == 2) return 1               # documentation
  if (a == 198 && b == 51 && c == 100) return 1            # documentation
  if (a == 203 && b == 0 && c == 113) return 1             # documentation
  if (a >= 224 && a <= 239) return 1                       # multicast
  if (a == 255 && b == 255 && c == 255 && d == 255) return 1   # broadcast
  return 0
}
function scan_ip(   pos, rest, s, len, m, prev, nx, nx2, ok, n, o, k) {
  pos = 1
  while (pos <= length(line)) {
    rest = substr(line, pos)
    if (!match(rest, IPRE)) break
    s = pos + RSTART - 1; len = RLENGTH
    m = substr(line, s, len)
    prev = (s > 1) ? substr(line, s - 1, 1) : ""
    nx = substr(line, s + len, 1); nx2 = substr(line, s + len + 1, 1)
    ok = 1
    if (prev != "" && prev ~ /[0-9A-Za-z_.]/) ok = 0
    if (nx != "" && nx ~ /[0-9A-Za-z_]/) ok = 0
    if (nx == "." && nx2 ~ /[0-9]/) ok = 0
    if (ok) { n = split(m, o, "."); for (k = 1; k <= 4; k++) if (o[k] + 0 > 255) ok = 0 }
    if (ok) {
      if (allowed_class(o)) defn++
      else print path "\t" lno "\tipv4\t" m
      pos = s + len
    } else pos = s + 1
  }
}
function scan_drive(   pos, rest, s, prev, ok, p, b, seplen, q, complen, lit, comp, cls) {
  pos = 1
  while (pos <= length(line)) {
    rest = substr(line, pos)
    if (!match(rest, DRVRE)) break
    s = pos + RSTART - 1
    prev = (s > 1) ? substr(line, s - 1, 1) : ""
    ok = 1
    if (prev != "" && index(LEFTBAD, prev) > 0) { ok = 0; if (s > 8 && substr(line, s - 8, 8) == "file:///") ok = 1 }
    if (ok) {
      p = s + 2
      if (substr(line, p, 1) == "/") { seplen = 1; p++ }
      else { b = p; while (substr(line, p, 1) == "\\") p++; seplen = p - b; if (seplen > 4) ok = 0 }
    }
    if (ok) {
      q = p
      while (q <= length(line) && index(NAMECH, substr(line, q, 1)) > 0) q++
      complen = q - p
      while (complen > 2 && substr(line, p + complen - 1, 1) == ".") complen--
      if (complen < 2) ok = 0
    }
    if (ok) {
      lit = substr(line, s, 2 + seplen + complen)
      comp = substr(line, p, complen)
      cls = (tolower(comp) == "users") ? "home" : "drive"
      print path "\t" lno "\t" cls "\t" lit
      pos = q
    } else pos = s + 1
  }
}
{
  i = index($0, "\001"); if (i == 0) next
  path = substr($0, 1, i - 1); rest = substr($0, i + 1)
  j = index(rest, ":"); if (j == 0) next
  lno = substr(rest, 1, j - 1); line = substr(rest, j + 1)
  sub(/\r$/, "", line)
  scan_ip(); scan_drive()
}
END { printf "%d\n", defn > defout }' > "$T/findings"
[ -s "$T/scan.err" ] && die "grep could not read some of the files of $ROOT ($mode_name): $(head -n 1 "$T/scan.err" | cut -c1-200)"
ndef="$(cat "$T/defaults" 2>/dev/null || echo 0)"

# ---- the findings against the allowlist --------------------------------------------------------------------------------------------------------------------------------------
ncov=0; nred=0; nfind=0; nredip=0; red_lines=()
while IFS=$'\t' read -r p l c lit; do
  [ -n "$p" ] || continue
  nfind=$((nfind+1)); covered=0
  for ((k = 0; k < nent; k++)); do
    if { [ "${EL[k]}" = '*' ] || [ "${EL[k]}" = "$lit" ]; } && glob_match "$p" "${EG[k]}"; then EH[k]=$((EH[k]+1)); covered=1; fi
  done
  if [ "$covered" -eq 1 ]; then ncov=$((ncov+1)); continue; fi
  case "$c" in
    home)  what="Windows home path";;
    drive) what="drive-letter path";;
    *)     what="IPv4 address"; nredip=$((nredip+1));;
  esac
  nred=$((nred+1)); red_lines+=("$p:$l: $what: $lit")
done < "$T/findings"
shown=0
for ((k = 0; k < ${#red_lines[@]}; k++)); do
  if [ "$shown" -lt 200 ]; then bad "${red_lines[k]}"; shown=$((shown+1)); else fail=$((fail+1)); fi
done
[ "${#red_lines[@]}" -le 200 ] || echo "  ... and $(( ${#red_lines[@]} - 200 )) more finding(s) not shown"
if [ "$nred" -gt 0 ]; then
  echo "  A path is removed from the file (a generic form: the repository root, %USERPROFILE%); an address outside the allowed classes that is a test fixture, a documented public service or a version number needs an entry in $ALLOW_IN: path-glob|literal|reason."
fi
for ((k = 0; k < nent; k++)); do
  if [ "${EH[k]}" -eq 0 ]; then bad "allowlist $ALLOW_IN line ${EN[k]}: the entry '${EG[k]}|${EL[k]}' matches nothing (the finding is gone, the file moved, or the address is of a class that needs no entry): remove it, the list cannot rot"; fi
done

echo "check-personal-details: $ROOT_IN ($mode_name): $npaths path(s), $ntext text file(s) read, $nbin binary skipped, $nempty empty skipped, $nlink symlink(s) and $nsub submodule(s) not followed"
echo "check-personal-details: $ndef address(es) in allowed classes (no entry needed), $ncov finding(s) covered by $nent allowlist entr$([ "$nent" -eq 1 ] && echo y || echo ies) in $ALLOW_IN, $nred finding(s) RED ($nredip of them addresses)"
echo "$NOT_CHECKED"
[ "$fail" -eq 0 ] || { echo "check-personal-details: RED — $fail problem(s)"; exit 1; }
echo "check-personal-details: OK"
