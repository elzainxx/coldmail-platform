#!/usr/bin/env bash
# check-personal-details-selftest.sh — proves scripts/check-personal-details.sh is green on a clean tree and goes red, FOR THE REASON IT NAMES, on every way a tree can name a home path, a drive path
# or an address, and on every way the allowlist can be wrong.
# Every case builds a throwaway tree, runs the real gate on it and asserts the exit code AND fragments of the printed reason (class 10: a reject case that fails for another reason is no proof).
# A hard minimum number of cases, and controls that prove THIS file can fail: a gate that always passes, one that always fails, one that fails for the wrong reason, and copies of the REAL gate with one
# check removed or loosened each (the matching case must then misbehave).
# THIS FILE CONTAINS NO PERSONAL PATH AND NO PUBLIC ADDRESS: the gate reads this very file on the platform tree. Every path and address that must be flagged is assembled at run time from pieces
# (a drive letter, a backslash, four numbers), so the text of this file never holds the shape the gate looks for.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-personal-details.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
MIN_CASES=45
n=0; bad=0
t() { n=$((n+1)); }
BSL='\'; DL=C; DL2=D
ip() { printf '%s.%s.%s.%s' "$1" "$2" "$3" "$4"; }
PUB="$(ip 93 184 216 34)"; PUB2="$(ip 8 8 4 4)"
HOME_BS="${DL}:${BSL}Users${BSL}bob${BSL}work"          # a Windows home path, backslashes
HOME_BS2="${DL}:${BSL}${BSL}Users${BSL}${BSL}bob"       # the escaped form of a JSON or Go string
HOME_FS="${DL}:/Users/bob/work"                         # forward slashes
HOME_LOW="$(printf '%s' "$DL" | tr 'A-Z' 'a-z'):${BSL}users${BSL}bob"
DRV_BS="${DL2}:${BSL}Work${BSL}coldmail"                # a drive-letter path
DRV_FS="${DL2}:/Work/coldmail"
DRV_BS4="${DL2}:${BSL}${BSL}${BSL}${BSL}Work${BSL}${BSL}${BSL}${BSL}x"

EMPTY_ALLOW="$TMP/empty.allow"; printf '# no entries\n' > "$EMPTY_ALLOW"

mk() { rm -rf "$1"; mkdir -p "$1/src" "$1/Docs"; printf '# A clean repository\n\nSee https://example.com/docs for details.\n' > "$1/README.md"; printf 'package main\n\nfunc main() {}\n' > "$1/src/main.go"; }
put() { # put DIR REL TEXT... — one line per argument
  local d="$1" rel="$2"; shift 2; mkdir -p "$(dirname "$d/$rel")"; printf '%s\n' "$@" > "$d/$rel"
}
allow() { # allow LINE... — a fresh allowlist file ALLOW with one entry per argument
  ALLOW="$TMP/test.allow"; printf '%s\n' "$@" > "$ALLOW"
}

ARGS=(); OUT=""
# expect NAME WANT_RC FRAGMENT...   (the gate is run with the global ARGS; each fragment must appear in the output)
expect() {
  t; name="$1"; want="$2"; shift 2
  OUT="$(bash "$GATE" ${ARGS[@]+"${ARGS[@]}"} 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  for frag in "$@"; do
    if ! printf '%s\n' "$OUT" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc as expected but the output lacks '$frag'"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  done
  echo "  ok   $name"
}
absent() { # absent NAME FRAGMENT: the output of the last expect must NOT carry the fragment
  t
  if printf '%s\n' "$OUT" | grep -qF -- "$2"; then echo "  FAIL $1: the output carries '$2'"; printf '%s\n' "$OUT" | sed 's/^/     | /'; bad=$((bad+1)); else echo "  ok   $1"; fi
}
D="$TMP/tree"
run_on() { ARGS=(--root "$D" --allow "${1:-$EMPTY_ALLOW}"); }

# ---- green ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------
mk "$D"; run_on
expect "a clean tree is green and says what it read" 0 "check-personal-details: OK" "2 text file(s) read, 0 binary skipped, 0 empty skipped" "(file tree)" "0 finding(s) RED"
expect "the green run prints what it did not check" 0 "NOT CHECKED: the commit history" "a drive path whose first name is one character" "the owner's name in prose"
# the classes allowed with no entry: three documentation ranges, loopback, unspecified, private (three), link-local, carrier-grade NAT, multicast, broadcast
mk "$D"; put "$D" Docs/classes.md "192.0.2.5 198.51.100.9 203.0.113.77" "127.0.0.1 127.255.255.254 0.0.0.0" "10.1.2.3 172.16.0.1 172.31.255.254 192.168.1.1" "169.254.169.254 100.64.0.1 100.127.255.254" "224.0.0.1 239.255.255.255 255.255.255.255"
expect "every allowed class is green with no entry (all 16 addresses counted)" 0 "check-personal-details: OK" "16 address(es) in allowed classes"
# not drive paths: a URL, a Go escape in a log format, a regexp class, a printf format, a one-letter YAML key in a printf, a Windows-free docker volume
mk "$D"; put "$D" Docs/notdrive.md 'curl https://example.com/x and postgres://u@h/db and ssh://git@h/x.git and s3://bucket/key' 't.Fatalf("the log carries it:\n%s", out)' 'pat := regexp.MustCompile(`Code:\s+(x)`)' 'printf "%s:\\x41" "$name"' "printf 'services:\\n  a:\\n    image: x\\n'" 'docker run -v /a:/s alpine'
expect "a URL, a Go escape, a regexp, a printf format and a docker volume are not drive paths" 0 "check-personal-details: OK"
# not addresses: versions (five parts, a v prefix, a suffix), a number above 255, a trailing letter
mk "$D"; put "$D" Docs/versions.md "version 1.2.3.4.5 and v1.2.3.4 and $(ip 300 1 1 1) and $(ip 1 2 3 4)x and 2.1.1" "build 5.1.2.3.4 and name1.2.3.4"
expect "version numbers and numbers above 255 are not addresses" 0 "check-personal-details: OK"
# a binary file holding a home path is skipped and counted (grep -I: a NUL byte), an empty file is skipped and counted
mk "$D"; printf 'abc\000%s\n' "$HOME_BS" > "$D/blob.bin"; : > "$D/empty.txt"
expect "a binary file is skipped and counted, an empty file is counted" 0 "check-personal-details: OK" "1 binary skipped, 1 empty skipped"
# git mode: only tracked files; a symlink entry and a submodule entry are not followed (index entries only, so no OS support for symlinks is needed)
mk "$D"; git -C "$D" init -q 2>/dev/null; put "$D" Docs/untracked.md "$HOME_BS"; git -C "$D" add -- README.md src/main.go
blob="$(git -C "$D" hash-object -w --stdin < /dev/null)"
git -C "$D" update-index --add --cacheinfo "120000,$blob,link" 2>/dev/null; git -C "$D" update-index --add --cacheinfo "160000,$blob,sub" 2>/dev/null; run_on
expect "git mode reads tracked files only (an untracked home path is not read), and counts a symlink and a submodule as not followed" 0 "(git ls-files)" "2 text file(s) read" "1 symlink(s) and 1 submodule(s) not followed" "check-personal-details: OK"
git -C "$D" add -- Docs/untracked.md
expect "git mode: the same file once tracked is read and is RED" 1 "Docs/untracked.md:1: Windows home path: ${DL}:${BSL}Users"

# ---- Windows home paths and drive paths ------------------------------------------------------------------------------------------------------------------------------------
mk "$D"; put "$D" Docs/a.md "x" "y" "build log in $HOME_BS ok"; run_on
expect "a Windows home path, backslashes: file, line and literal are named" 1 "Docs/a.md:3: Windows home path: ${DL}:${BSL}Users" "check-personal-details: RED"
mk "$D"; put "$D" Docs/a.md "{\"path\": \"$HOME_BS2\"}"
expect "a Windows home path in its escaped (JSON) form" 1 "Docs/a.md:1: Windows home path: ${DL}:${BSL}${BSL}Users"
mk "$D"; put "$D" Docs/a.md "cd $HOME_FS"
expect "a Windows home path with forward slashes" 1 "Docs/a.md:1: Windows home path: ${DL}:/Users"
mk "$D"; put "$D" Docs/a.md "cd $HOME_LOW"
expect "a lower-case home path (Windows paths are case-insensitive)" 1 "Docs/a.md:1: Windows home path: $(printf '%s' "$DL" | tr 'A-Z' 'a-z'):${BSL}users"
mk "$D"; put "$D" Docs/a.md "the repository is under $DRV_BS today"
expect "a drive-letter path, backslashes" 1 "Docs/a.md:1: drive-letter path: ${DL2}:${BSL}Work"
absent "a drive path that is not a home path is not called one" "Windows home path"
mk "$D"; put "$D" Docs/a.md "the repository is under $DRV_FS today"
expect "a drive-letter path, forward slashes" 1 "Docs/a.md:1: drive-letter path: ${DL2}:/Work"
mk "$D"; put "$D" Docs/a.md "s := \"$DRV_BS4\""
expect "a drive-letter path with four backslashes (escaped twice)" 1 "Docs/a.md:1: drive-letter path: ${DL2}:${BSL}${BSL}${BSL}${BSL}Work"
mk "$D"; put "$D" Docs/a.md "open file:///${DL}:/Users/bob/x.txt"
expect "a file URL with a drive letter is a home path" 1 "Docs/a.md:1: Windows home path: ${DL}:/Users"
mk "$D"; put "$D" Docs/a.md "$HOME_BS" ; sed -i 's/$/\r/' "$D/Docs/a.md"
expect "a CRLF file is read and the literal is reported" 1 "Docs/a.md:1: Windows home path: ${DL}:${BSL}Users"
# the CR check reads a FILE, not $(...) (a Windows bash strips CR from command substitution, which would make this vacuous)
t; bash "$GATE" "${ARGS[@]}" > "$TMP/crlf.out" 2>&1; ncr="$(tr -cd '\r' < "$TMP/crlf.out" | wc -c | tr -d ' ')"
if [ "$ncr" = 0 ]; then echo "  ok   the CR of a CRLF file is not part of anything the gate prints"; else echo "  FAIL the gate printed $ncr carriage return(s) for a CRLF file"; bad=$((bad+1)); fi
mk "$D"; put "$D" "Docs/a file with spaces.md" "x" "$DRV_FS"
expect "a file whose name has spaces is named whole" 1 "Docs/a file with spaces.md:2: drive-letter path"

# ---- addresses --------------------------------------------------------------------------------------------------------------------------------------------------------------------------
mk "$D"; put "$D" Docs/a.md "one" "two" "the host is $PUB, not ours"
expect "a public address is RED, with file, line and the address" 1 "Docs/a.md:3: IPv4 address: $PUB" "1 finding(s) RED (1 of them addresses)"
mk "$D"; put "$D" Docs/edge.md "$(ip 172 15 0 1)" "$(ip 172 32 0 1)" "$(ip 100 63 255 255)" "$(ip 100 128 0 0)" "$(ip 192 0 3 1)" "$(ip 198 51 101 1)" "$(ip 203 0 114 1)" "$(ip 11 0 0 1)" "$(ip 192 169 0 1)" "$(ip 240 0 0 1)" "$(ip 255 255 255 0)" "$(ip 169 253 0 1)" "$(ip 1 1 1 1)"
expect "an address just outside each allowed class is RED (13 boundary cases, each named)" 1 "Docs/edge.md:1: IPv4 address: $(ip 172 15 0 1)" "Docs/edge.md:2: IPv4 address: $(ip 172 32 0 1)" "Docs/edge.md:3: IPv4 address: $(ip 100 63 255 255)" "Docs/edge.md:4: IPv4 address: $(ip 100 128 0 0)" "Docs/edge.md:5: IPv4 address: $(ip 192 0 3 1)" "Docs/edge.md:6: IPv4 address: $(ip 198 51 101 1)" "Docs/edge.md:7: IPv4 address: $(ip 203 0 114 1)" "Docs/edge.md:8: IPv4 address: $(ip 11 0 0 1)" "Docs/edge.md:9: IPv4 address: $(ip 192 169 0 1)" "Docs/edge.md:10: IPv4 address: $(ip 240 0 0 1)" "Docs/edge.md:11: IPv4 address: $(ip 255 255 255 0)" "Docs/edge.md:12: IPv4 address: $(ip 169 253 0 1)" "Docs/edge.md:13: IPv4 address: $(ip 1 1 1 1)" "13 finding(s) RED (13 of them addresses)"
mk "$D"; put "$D" Docs/a.md "resolvers $PUB,$PUB2 and $(ip 1 1 1 1)/24"
expect "two addresses joined by a comma and one with a prefix length are all found" 1 "IPv4 address: $PUB" "IPv4 address: $PUB2" "IPv4 address: $(ip 1 1 1 1)" "3 finding(s) RED"
mk "$D"; put "$D" Docs/a.md "curl http://$PUB:8080/health"
expect "an address inside a URL with a port" 1 "Docs/a.md:1: IPv4 address: $PUB"
mk "$D"; put "$D" Docs/a.md "host $(ip 010 0 0 1) and $(ip 17 12 0 000)"
expect "an octet with a leading zero is never classified: it needs an entry" 1 "IPv4 address: $(ip 010 0 0 1)" "IPv4 address: $(ip 17 12 0 000)"

# ---- the allowlist -----------------------------------------------------------------------------------------------------------------------------------------------------------------
mk "$D"; put "$D" Docs/a.md "the fixture host is $PUB"; allow "# a comment" "" "Docs/a.md|$PUB|a documented public test address used as a fixture"; run_on "$ALLOW"
expect "an entry covers its address: green, and the report says so" 0 "check-personal-details: OK" "1 finding(s) covered by 1 allowlist entry in $ALLOW" "0 finding(s) RED"
mk "$D"; put "$D" Docs/a.md "the fixture host is $PUB and again $PUB2"; allow "Docs/*.md|*|public test addresses used as fixtures in the docs"; run_on "$ALLOW"
expect "a wildcard literal covers every address of the matched files" 0 "check-personal-details: OK" "2 finding(s) covered by 1 allowlist entry"
mk "$D"; put "$D" Docs/a.md "the fixture host is $PUB"; put "$D" Docs/b.md "the fixture host is $PUB"; allow "Docs/a.md|$PUB|a documented public test address used as a fixture"; run_on "$ALLOW"
expect "an entry covers its own file only: the same address in another file is RED" 1 "Docs/b.md:1: IPv4 address: $PUB" "1 finding(s) covered"
absent "(and the entry that did match is not called stale)" "matches nothing"
mk "$D"; put "$D" top.txt "the fixture host is $PUB"; allow "**/top.txt|$PUB|a documented public test address used as a fixture"; run_on "$ALLOW"
expect "a glob that starts with **/ also matches a file at the top" 0 "check-personal-details: OK"
mk "$D"; put "$D" Docs/a.md "under $DRV_BS"; allow "Docs/a.md|${DL2}:${BSL}Work|a fixture that shows a Windows path on purpose"; run_on "$ALLOW"
expect "an entry covers a path literal as the gate reports it" 0 "check-personal-details: OK" "1 finding(s) covered"
mk "$D"; put "$D" Docs/a.md "the fixture host is $PUB"; allow "Docs/a.md|$PUB|"; run_on "$ALLOW"
expect "an entry with no reason is refused" 1 "line 1: the entry 'Docs/a.md|$PUB' has no reason: a reason is mandatory" "IPv4 address: $PUB"
mk "$D"; allow "Docs/a.md|$PUB|fixture"; run_on "$ALLOW"
expect "an entry with a one-word reason is refused" 1 "the reason 'fixture' is too short to be a reason"
mk "$D"; allow "Docs/a.md|$PUB"; run_on "$ALLOW"
expect "an entry of two fields is refused" 1 "line 1: the entry 'Docs/a.md|$PUB' has no reason"
mk "$D"; allow "Docs/a.md"; run_on "$ALLOW"
expect "an entry of one field is refused" 1 "line 1: not path-glob|literal|reason (got 'Docs/a.md')"
mk "$D"; put "$D" Docs/a.md "host $PUB"; allow "Docs/a.md|$PUB|a documented public test address used as a fixture" "Docs/a.md|$PUB|the same entry once more, with another reason"; run_on "$ALLOW"
expect "a duplicate entry is refused" 1 "line 2: the entry 'Docs/a.md|$PUB' is listed twice"
mk "$D"; put "$D" Docs/a.md "host $PUB"; allow "*|*|everything is a fixture, says this entry"; run_on "$ALLOW"
expect "a glob of wildcards only is refused (it would allow everything), and the finding stays RED" 1 "the glob '*' is too broad" "IPv4 address: $PUB"
mk "$D"; allow "/etc/*.conf|*|an absolute path is not below the repository root"; run_on "$ALLOW"
expect "a glob that is an absolute path is refused" 1 "is not a path below the repository root"
mk "$D"; allow "Docs/gone.md|$PUB|an address that was removed from the file long ago"; run_on "$ALLOW"
expect "an entry that matches nothing is RED: the list cannot rot" 1 "line 1: the entry 'Docs/gone.md|$PUB' matches nothing" "0 finding(s) covered by 1 allowlist entry"
mk "$D"; put "$D" Docs/a.md "host 192.168.1.1"; allow "Docs/a.md|192.168.1.1|an address of an allowed class needs no entry at all"; run_on "$ALLOW"
expect "an entry for an address of an allowed class matches nothing and is RED" 1 "the entry 'Docs/a.md|192.168.1.1' matches nothing"
mk "$D"; put "$D" Docs/a.md "host $PUB"; allow "Docs/other.md|$PUB|an address in a file that does not hold it"; run_on "$ALLOW"
expect "an entry for the wrong file: the finding is RED AND the entry is stale (two reasons, both named)" 1 "Docs/a.md:1: IPv4 address: $PUB" "the entry 'Docs/other.md|$PUB' matches nothing"
mk "$D"; put "$D" Docs/a.md "host $PUB"; allow "Docs/a.md|$PUB|a documented public test address used as a fixture"; sed -i 's/$/\r/' "$ALLOW"; run_on "$ALLOW"
expect "an allowlist with CRLF line endings is read" 0 "check-personal-details: OK"
mk "$D"; run_on "$TMP/no-such.allow"
expect "no allowlist file: the gate was never configured" 1 "the allowlist '$TMP/no-such.allow' does not exist"
mk "$D"; run_on
expect "an allowlist of comments only is valid" 0 "check-personal-details: OK" "0 allowlist entries"

# ---- the subject is empty or unreadable -----------------------------------------------------------------------------------------------------------------------------------------
ARGS=(--root "$TMP/no-such-dir" --allow "$EMPTY_ALLOW")
expect "a root that is not a directory" 1 "is not a directory: the instrument matched nothing"
rm -rf "$D"; mkdir -p "$D"; run_on
expect "a root with no file at all" 1 "found 0 files below"
rm -rf "$D"; mkdir -p "$D"; printf 'abc\000def\n' > "$D/only.bin"; run_on
expect "a root with only a binary file: no text file to read" 1 "found 0 text files to read" "1 binary"
# A tab or a newline in a path cannot be made on every host (Windows refuses both, in the file system and in the index), so a fake `git` first on PATH answers `ls-files -s -z` with such a path.
mkdir -p "$TMP/fakebin"
cat > "$TMP/fakebin/git" <<'EOF'
#!/usr/bin/env bash
# a fake git for the selftest: rev-parse --show-prefix prints nothing (the top level); ls-files -s -z prints the records in $FAKE_LS (printf format)
case "$*" in
  *"rev-parse --show-prefix"*) exit 0;;
  *"ls-files -s -z"*) printf "$FAKE_LS"; exit 0;;
esac
exit 1
EOF
mk "$D"; printf 'gitdir: elsewhere\n' > "$D/.git"; run_on; OLDPATH="$PATH"; PATH="$TMP/fakebin:$PATH"
FAKE_LS='100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\tREADME.md\000100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\tDocs/a\tb.md\000'; export FAKE_LS
expect "a file name with a tab in it cannot be attributed" 1 "has a tab in its name"
FAKE_LS='100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\tREADME.md\000100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\tDocs/a\nb.md\000'
expect "a file name with a newline in it cannot be attributed" 1 "has a newline in its name"
FAKE_LS='100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\tREADME.md\000'
expect "the fake git itself lists a clean name and the gate is then green (the two cases above fail for their reason)" 0 "(git ls-files)" "check-personal-details: OK"
PATH="$OLDPATH"; unset FAKE_LS

# ---- usage -------------------------------------------------------------------------------------------------------------------------------------------------------------------------
t; out="$(bash "$GATE" --root "$D" 2>&1)"; rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "usage"; then echo "  ok   no --allow is a usage error (exit 2)"; else echo "  FAIL no --allow: exit $rc"; bad=$((bad+1)); fi
t; out="$(bash "$GATE" --allow "$EMPTY_ALLOW" 2>&1)"; rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "usage"; then echo "  ok   no --root is a usage error (exit 2)"; else echo "  FAIL no --root: exit $rc"; bad=$((bad+1)); fi
t; out="$(bash "$GATE" --bogus 2>&1)"; rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi

# ---- the controls: this selftest must be able to fail ------------------------------------------------------------------------------------------------------------------------------
# Each control swaps the gate for a fake (or a mutant of the real gate), runs one real case against it, and requires the case to MISBEHAVE (and then forgives it: it is the control).
fake="$TMP/fake.sh"
run_control() { # run_control LABEL GATE-TEXT CASE-FUNCTION ARGS...
  t; label="$1"; printf '%s\n' "$2" > "$fake"; shift 2
  real="$GATE"; GATE="$fake"; before=$bad
  "$@" > /dev/null
  GATE="$real"
  if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: $label"; else echo "  FAIL control: $label — the selftest did not notice"; bad=$((bad+1)); fi
}
mutant() { # mutant FROM TO -> prints the real gate with FROM replaced by TO (sed, # as the delimiter), and a stub that exits 0 when the gate has no such text (so the control then fails to misbehave)
  local m; m="$(sed "s#$1#$2#" "$GATE")"
  if [ "$m" = "$(cat "$GATE")" ]; then echo "echo 'control: the gate has no text $1, so the mutant is the gate itself'; exit 0"; else printf '%s\n' "$m"; fi
}
# the mutants copy the gate into $TMP, so HERE-relative reads are not used by the gate (it reads nothing beside itself)
mk "$D"; put "$D" Docs/a.md "x" "y" "build log in $HOME_BS ok"; run_on
run_control "a gate that always passes makes the 'home path' case misbehave" '#!/usr/bin/env bash
echo "check-personal-details: OK"; exit 0' expect "home path control" 1 "Docs/a.md:3: Windows home path: ${DL}:${BSL}Users"
mk "$D"; run_on
run_control "a gate that always fails makes the 'clean tree' case misbehave" '#!/usr/bin/env bash
echo "check-personal-details: RED"; exit 1' expect "clean tree control" 0 "check-personal-details: OK"
mk "$D"; put "$D" Docs/a.md "the host is $PUB"; run_on
run_control "a gate that fails for another reason makes the 'public address' case misbehave (class 10)" '#!/usr/bin/env bash
echo "check-personal-details: RED — something unrelated"; exit 1' expect "address control" 1 "Docs/a.md:1: IPv4 address: $PUB"
mk "$D"; put "$D" Docs/a.md "x" "y" "build log in $HOME_BS ok"; run_on
run_control "a copy of the gate that cannot tell a home path from a drive path makes the 'home path' case misbehave" "$(mutant 'cls = (tolower(comp) == "users") ? "home" : "drive"' 'cls = "drive"')" expect "home path control" 1 "Docs/a.md:3: Windows home path: ${DL}:${BSL}Users"
mk "$D"; put "$D" Docs/edge.md "$(ip 172 32 0 1)"; run_on
run_control "a copy of the gate that allows 172.16.0.0/11 instead of /12 makes the boundary case misbehave" "$(mutant 'a == 172 \&\& b >= 16 \&\& b <= 31' 'a == 172 \&\& b >= 16 \&\& b <= 32')" expect "boundary control" 1 "Docs/edge.md:1: IPv4 address: $(ip 172 32 0 1)"
mk "$D"; put "$D" Docs/a.md "host $PUB"; allow "Docs/a.md|$PUB|"; run_on "$ALLOW"
run_control "a copy of the gate without its reason check makes the 'no reason' case misbehave" "$(mutant 'if \[ -z "\$why" \]; then bad' 'if false; then bad')" expect "reason control" 1 "has no reason: a reason is mandatory"
mk "$D"; allow "Docs/gone.md|$PUB|an address that was removed from the file long ago"; run_on "$ALLOW"
run_control "a copy of the gate without its stale-entry check makes the 'matches nothing' case misbehave" "$(mutant 'if \[ "\${EH\[k\]}" -eq 0 \]; then bad' 'if false; then bad')" expect "stale control" 1 "matches nothing"
mk "$D"; put "$D" Docs/a.md "under $DRV_FS"; run_on
run_control "a copy of the gate that only reads backslash paths makes the 'forward slashes' case misbehave" "$(mutant 'if (substr(line, p, 1) == "/") { seplen = 1; p++ }' 'if (0) { seplen = 1; p++ }')" expect "slash control" 1 "Docs/a.md:1: drive-letter path: ${DL2}:/Work"

echo "selftest: $n cases"
[ "$n" -ge "$MIN_CASES" ] || { echo "check-personal-details-selftest: RED — only $n cases ran (minimum $MIN_CASES)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-personal-details-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-personal-details-selftest: OK"
