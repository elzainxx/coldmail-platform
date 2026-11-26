#!/usr/bin/env bash
# check-faultpoints-selftest.sh — proves scripts/check-faultpoints.sh is green on a tree whose fault points are registered, hit and armed only in a file built with the tag, and RED, FOR THE REASON IT NAMES, on each way
# that can break: a Hit with an unregistered, misspelt, non-literal or commented name, a stale name, an imported-under-another-name package, every way the registry file can be malformed, a file that names the arming
# environment variable without the build line, a Dockerfile that lost its empty GO_TAGS argument or names the tag, a workflow that builds with the tag, and a floor that found nothing. Every case builds a throwaway tree (a stand-in
# for the repository root: internal/, cmd/, tests/, Dockerfile, .github/workflows/) in a temp directory, so it needs no database, no Go toolchain and no checkout.
# Every reject case asserts the printed REASON as well as the exit code (a case that went red for an unrelated reason would read as a pass forever: class 10). The selftest has a hard minimum number of cases, and a CONTROL
# proves it can say "no": a gate that always exits 0, a gate that fails without a reason, and two wrong expectations must each be reported as a mismatch.
set -u
export LC_ALL=C
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-faultpoints.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=121; cases=0; ok=0; bad_expect=0

REG=internal/faultpoint/names.txt
NC_HEAD='NOT CHECKED: test files (*_test.go); Go files outside internal/, cmd/ and tests/'
unl()    { printf '%s:%s: faultpoint.Hit("%s") names a point that %s does not list' "$1" "$2" "$3" "$REG"; }                                                       # unl <path> <line> <name>
nonlit() { printf '%s:%s: faultpoint.Hit is not called with a plain double-quoted string literal' "$1" "$2"; }                                                    # nonlit <path> <line>
stale()  { printf '%s:%s: stale name '"'%s'"': no non-test Go file under internal/ or cmd/ calls faultpoint.Hit("%s") outside a comment' "$REG" "$2" "$1" "$1"; }   # stale <name> <line>
varf()   { printf '%s:%s: names the fault-point environment variable PLATFORM_FAULTPOINT but does not start with the line //go:build faultpoints' "$1" "$2"; }  # varf <path> <line>
gotag()  { printf '%s:%s: uses the build tag faultpoints but the file does not start with //go:build faultpoints (or the inert //go:build !faultpoints)' "$1" "$2"; }   # gotag <path> <line>
alias_() { printf '%s:%s: internal/faultpoint is imported under another name or for its side effects' "$1" "$2"; }                                               # alias_ <path> <line>
regerr() { printf '%s:%s: %s' "$REG" "$1" "$2"; }                                                                                                                   # regerr <line> <text>
wfgo()   { printf '%s:%s: a workflow names GO_TAGS' "$1" "$2"; }                                                                                                    # wfgo <path> <line>
wftag()  { printf '%s:%s: a workflow line names the build tag faultpoints outside a plain go vet line of go.yml' "$1" "$2"; }                                       # wftag <path> <line>
dfword() { printf '%s:%s: the word faultpoints in a Dockerfile' "$1" "$2"; }                                                                                        # dfword <path> <line>
ARG_MISSING='Dockerfile:1: the line ARG GO_TAGS="" is missing'

put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }   # put <dir> <relative path> < content
mk() {   # mk <name>: a fresh CLEAN tree; echoes its directory
  d="$T/$1"; rm -rf "$d"
  put "$d" internal/faultpoint/names.txt <<'EOF'
# fixture registry
confirm.before_commit  # internal/jobs/confirm.go, Store.Confirm
submit.after_module    # internal/jobengine/engine.go, Engine.submit
start.before_running   # internal/jobs/engine.go, Store.Start
settle.after_commit    # internal/jobs/engine.go, Store.RecordResults
release.mid_tx         # internal/jobs/engine.go, Store.Release
EOF
  put "$d" internal/faultpoint/faultpoint.go <<'EOF'
//go:build !faultpoints

// Package faultpoint: in the default build every point is inert.
package faultpoint

func Hit(name string) {}
EOF
  put "$d" internal/faultpoint/faultpoint_on.go <<'EOF'
//go:build faultpoints

package faultpoint

import "os"

// the tagged build reads the arming variable
var spec = os.Getenv("PLATFORM_FAULTPOINT")

func Hit(name string) { _ = spec }
EOF
  put "$d" internal/faultpoint/faultpoint_test.go <<'EOF'
package faultpoint

func TestInert() { _ = "PLATFORM_FAULTPOINT" }
EOF
  put "$d" internal/jobs/confirm.go <<'EOF'
package jobs

import "github.com/coldmail-platform/platform/internal/faultpoint"

func confirm() {
	faultpoint.Hit("confirm.before_commit")
}
EOF
  put "$d" internal/jobs/engine.go <<'EOF'
package jobs

import "github.com/coldmail-platform/platform/internal/faultpoint"

func start() {
	faultpoint.Hit("start.before_running")
}

func settle() {
	faultpoint.Hit("settle.after_commit")
}

func release() {
	faultpoint.Hit("release.mid_tx")
}
EOF
  put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine

import "github.com/coldmail-platform/platform/internal/faultpoint"

func submit() {
	faultpoint.Hit("submit.after_module")
}
EOF
  put "$d" cmd/platform/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" tests/harness/fx/main.go <<'EOF'
package main

func main() {}
EOF
  put "$d" Dockerfile <<'EOF'
FROM golang:1.26-alpine AS build
WORKDIR /src
# the service binary takes the tags; the migrator does not
ARG GO_TAGS=""
RUN go build -tags "${GO_TAGS}" -o /out/platform ./cmd/platform
EOF
  put "$d" .github/workflows/go.yml <<'EOF'
name: go
on: [push]
jobs:
  go:
    runs-on: ubuntu-latest
    steps:
      - name: Vet
        run: |
          go vet ./...
          go vet -tags faultpoints ./internal/... ./cmd/...   # vet only
      - name: Build
        run: go build ./...
EOF
  put "$d" .github/workflows/gates.yml <<'EOF'
# the faultpoints tag is named in this comment only, and the gate's own name is not the tag
name: gates
on: [push]
jobs:
  gates:
    runs-on: ubuntu-latest
    steps:
      - name: check-faultpoints on the real repository
        run: bash scripts/check-faultpoints.sh
EOF
  put "$d" tests/harness/docker-compose.yml <<'EOF'
x-platform-build: &platform-build
  context: .
  args: { GO_TAGS: faultpoints }
services:
  platform:
    build: *platform-build
    environment:
      PLATFORM_FAULTPOINT: "start.before_running:1"
EOF
  echo "$d"
}

# check <label> <rc> <want-rc> <subs> <output>: every @@-separated sub must appear in the output; one that starts with ! must NOT appear
check() {
  local label="$1" rc="$2" want="$3" subs="$4" out="$5" rest part neg more miss=""
  cases=$((cases+1))
  if [ "$rc" -ne "$want" ]; then
    echo "  MISMATCH [$label]: rc=$rc want=$want"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  rest="$subs"
  while :; do
    case "$rest" in *@@*) part="${rest%%@@*}"; rest="${rest#*@@}"; more=1;; *) part="$rest"; more=0;; esac
    neg=0; case "$part" in '!'*) neg=1; part="${part#!}";; esac
    if printf '%s\n' "$out" | grep -q -F -- "$part"; then [ "$neg" -eq 0 ] || miss="$miss [must NOT appear: $part]"
    else [ "$neg" -eq 1 ] || miss="$miss [missing: $part]"; fi
    [ "$more" -eq 1 ] || break
  done
  if [ -n "$miss" ]; then
    echo "  MISMATCH [$label]: rc=$rc as expected, but$miss"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return
  fi
  ok=$((ok+1)); echo "  ok [$label] rc=$rc"
}
# expect <label> <want-rc> <subs> <tree> [gate args replacing the default --root <tree>]
expect() {
  local label="$1" want="$2" subs="$3" d="$4" out rc; shift 4
  if [ $# -gt 0 ]; then out="$(bash "$GATE" "$@" 2>&1)"; rc=$?; else out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?; fi
  check "$label" "$rc" "$want" "$subs" "$out"
}
# g <dir> <path> <body>: a Go file that imports the package and holds <body> inside a function: the body starts on line 6
g() { put "$1" "$2" <<EOF
package app

import "github.com/coldmail-platform/platform/internal/faultpoint"

func f() {
$3
}
EOF
}
RED1='RED — 1 problem(s)'

# ---- clean trees -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk clean);  expect "clean: five registered points each hit once, the arming variable only in the tagged file, an empty GO_TAGS argument, the tag only on a go vet line" 0 "check-faultpoints: OK@@scanned 6 Go file(s) under internal/ and cmd/ (7 with tests/), 2 workflow file(s), 1 Dockerfile(s); 1 compose file(s) under tests/harness are allowed to name the tag and are not read@@registry $REG lists 5 name(s); found 5 faultpoint.Hit call(s) outside comments (0 more in comments, not counted as hits) covering 5 registered point(s); 1 Go file(s) name the fault-point environment variable@@1 go vet line(s) in .github/workflows/go.yml name the tag@@0 registry problem(s), 0 hit problem(s), 0 stale name(s), 0 build-line problem(s), 0 Go tag problem(s), 0 Dockerfile problem(s), 0 workflow problem(s), 0 floor(s), 0 file(s) the matcher failed on" "$d"
d=$(mk clean2); expect "a clean run prints its scope and what it did NOT check" 0 "scope: (a) faultpoint.Hit calls in non-test *.go under internal/ and cmd/ against $REG; (b) non-test *.go under internal/, cmd/ and tests/ naming the fault-point environment variable; (c) the Dockerfiles; (d) the build tag in Go files, Dockerfiles and .github/workflows@@$NC_HEAD@@any Hit call under tests/@@a Hit whose name is built at run time or reached through a variable@@an unqualified Hit called from inside package faultpoint itself@@the variable's name built from fragments@@the build tag set outside this repository@@a -tags argument inside Go source or a shell script@@comments are classified textually@@whether the built image really lacks the armed code: the harness proves the tagged build kills a process" "$d"
d=$(mk crlf);   for f in $REG internal/faultpoint/faultpoint_on.go internal/jobs/engine.go Dockerfile .github/workflows/go.yml tests/harness/docker-compose.yml; do sed -i 's/$/\r/' "$d/$f"; done
                expect "every file with CRLF line endings (the registry, a Go file, the Dockerfile, a workflow): still green, the same counts" 0 "check-faultpoints: OK@@found 5 faultpoint.Hit call(s) outside comments@@1 go vet line(s) in .github/workflows/go.yml name the tag" "$d"
d=$(mk spaced); g "$d" internal/jobs/extra.go '	faultpoint . Hit (
		"start.before_running",
	)
	faultpoint.Hit("settle.after_commit"); faultpoint.Hit("release.mid_tx")'
                expect "a call split over lines, spaced and with a trailing comma, and two calls on one line, are read: 8 live calls" 0 "check-faultpoints: OK@@found 8 faultpoint.Hit call(s) outside comments (0 more in comments, not counted as hits)" "$d"
d=$(mk cmt);    g "$d" internal/jobs/extra.go '	// faultpoint.Hit("start.before_running") is called after the move
	/* faultpoint.Hit("settle.after_commit") */
	faultpoint.Hit("release.mid_tx") // faultpoint.Hit("release.mid_tx")'
                expect "a comment that shows a call of a REGISTERED point is read and is not counted as a hit (two commented calls and a live call with a comment after it)" 0 "check-faultpoints: OK@@found 6 faultpoint.Hit call(s) outside comments (3 more in comments, not counted as hits)" "$d"
d=$(mk testf);  put "$d" internal/jobs/x_test.go <<'EOF'
package jobs

func TestX() {
	faultpoint.Hit("not.registered")
	_ = "PLATFORM_FAULTPOINT"
}
EOF
                expect "a _test.go file with an unregistered Hit and the variable is ignored, and the report says tests were not checked" 0 "check-faultpoints: OK@@scanned 6 Go file(s) under internal/ and cmd/ (7 with tests/)@@$NC_HEAD" "$d"
d=$(mk testsdir); put "$d" tests/harness/fx/hits.go <<'EOF'
package main

import "github.com/coldmail-platform/platform/internal/faultpoint"

func f() { faultpoint.Hit("not.registered") }
EOF
                expect "a Hit in a non-test Go file under tests/ is not read by the registry check (it is listed under NOT CHECKED)" 0 "check-faultpoints: OK@@scanned 6 Go file(s) under internal/ and cmd/ (8 with tests/)@@any Hit call under tests/" "$d"
d=$(mk prose);  put "$d" internal/app/prose.go <<'EOF'
package app

// The faultpoints build and the fault points are described in the phase spec; the package faultpoint is inert by default.
// Nothing here uses the build tag: it only talks about it.
func f() {}
EOF
                expect "prose that merely says faultpoints, fault points or faultpoint is not a use of the tag" 0 "check-faultpoints: OK" "$d"
d=$(mk whole);  g "$d" internal/app/x.go '	myfaultpoint.Hit("zz.zz")
	faultpoint.Hits("zz.zz")
	faultpoint.Hitch("zz.zz")
	faultpoints.Hit("zz.zz")
	faultpoint_on.Hit("zz.zz")
	Hit("zz.zz")
	faultpoint.hit("zz.zz")
	_ = faultpoint.Enabled'
                expect "whole word only: another package, a longer name, an unqualified Hit, the constant and a lower-case hit are not calls" 0 "check-faultpoints: OK@@found 5 faultpoint.Hit call(s) outside comments" "$d"
d=$(mk nongo);  put "$d" tests/harness/stage.sh <<'EOF'
export PLATFORM_FAULTPOINT=start.before_running:1
docker compose build --build-arg GO_TAGS=faultpoints
EOF
                put "$d" Docs/notes.md <<'EOF'
Build with `go build -tags faultpoints ./cmd/platform` or `--build-arg GO_TAGS=faultpoints`; arm with PLATFORM_FAULTPOINT.
EOF
                expect "a shell stage and a Markdown note that name the variable and the tag are not read (NOT CHECKED: the harness sets them on purpose)" 0 "check-faultpoints: OK@@1 Go file(s) name the fault-point environment variable" "$d"
d=$(mk compose2); put "$d" tests/harness/docker-compose.m1node.yml <<'EOF'
services:
  platform:
    build: { context: ., args: { GO_TAGS: faultpoints } }
EOF
                expect "any tests/harness/docker-compose*.yml may name the tag and GO_TAGS, and the report counts them" 0 "check-faultpoints: OK@@2 compose file(s) under tests/harness are allowed to name the tag" "$d"
d=$(mk vets);   put "$d" .github/workflows/go.yml <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: go vet -tags "harness faultpoints" ./...
      - name: Vet
        run: go vet -tags faultpoints ./internal/... ./cmd/...
      - name: Build
        run: go build ./...
EOF
                expect "go vet -tags with the tag, as a one-line run and as a list item, is allowed on a plain go vet line of go.yml" 0 "check-faultpoints: OK@@2 go vet line(s) in .github/workflows/go.yml name the tag" "$d"
d=$(mk gatename); put "$d" .github/workflows/gates.yml <<'EOF'
name: gates
jobs:
  gates:
    steps:
      - name: selftests
        run: for t in scripts/check-faultpoints-selftest.sh scripts/check-job-writers-selftest.sh; do bash "$t"; done
      - name: check-faultpoints on the real repository
        run: bash scripts/check-faultpoints.sh
EOF
                expect "the gate's own name (its selftest, its script, its step) in a workflow line is not the tag" 0 "check-faultpoints: OK@@0 workflow problem(s)" "$d"
d=$(mk dfother); put "$d" tests/harness/m1/Dockerfile <<'EOF'
FROM alpine:3.24
RUN echo no tags here
EOF
                expect "another Dockerfile without the tag is fine, and the report counts both" 0 "check-faultpoints: OK@@2 Dockerfile(s)" "$d"
d=$(mk taggedgo); put "$d" tests/harness/fx/armed.go <<'EOF'
//go:build faultpoints

package main

import "os"

// built with -tags faultpoints only
var args = []string{"go", "build", "-tags", "faultpoints"}
var spec = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "a Go file that starts with the build line may name the variable and the tag in any form (the tagged file under tests/ counts as a second file that names the variable)" 0 "check-faultpoints: OK@@2 Go file(s) name the fault-point environment variable" "$d"
d=$(mk cmtgo);  put "$d" internal/app/note.go <<'EOF'
package app

// the harness builds the image with -tags faultpoints; this package does not care
func f() {}
EOF
                expect "a COMMENT line that mentions -tags faultpoints in a file without the build line is prose, not a use" 0 "check-faultpoints: OK" "$d"
d=$(mk inert);  put "$d" internal/app/inert.go <<'EOF'
//go:build !faultpoints

package app

func f() {}
EOF
                expect "a file with the inert build line //go:build !faultpoints is a use of the tag in an allowed form" 0 "check-faultpoints: OK" "$d"
d=$(mk impone); put "$d" internal/app/imp1.go <<'EOF'
package app

import "github.com/coldmail-platform/platform/internal/faultpoint"

// import fp "github.com/coldmail-platform/platform/internal/faultpoint" is only a comment here
func f() { faultpoint.Hit("start.before_running") }
EOF
                put "$d" internal/app/imp2.go <<'EOF'
package app

import (
	"fmt"

	"github.com/coldmail-platform/platform/internal/faultpoint"
)

func g() { faultpoint.Hit("release.mid_tx") }
EOF
                expect "the plain import in every form (grouped, single-line, with the quoted path of an aliased import only in a comment) is not an alias" 0 "check-faultpoints: OK@@found 7 faultpoint.Hit call(s)" "$d"
d=$(mk dflt);   put "$d" scripts/.keep <<'EOF'
EOF
                cp "$GATE" "$d/scripts/check-faultpoints.sh"
                out="$(cd "$T" && bash "$d/scripts/check-faultpoints.sh" 2>&1)"; rc=$?
                check "with no --root the root is the parent of the script's directory (run from another working directory)" "$rc" 0 "check-faultpoints: OK@@(root: " "$out"
d=$(mk eqroot); expect "--root=DIR is accepted" 0 "check-faultpoints: OK" "$d" "--root=$d"

# ---- (a) the registry against the hits: red, naming the file, the line and the reason ----------------------------------------------------------------------------------------------------
d=$(mk unl);    g "$d" internal/jobs/new.go '	faultpoint.Hit("jobs.brand_new")'
                expect "a Hit of a name the registry does not list: the file, the line, the name" 1 "$(unl internal/jobs/new.go 6 jobs.brand_new)@@$RED1: 0 registry, 1 hit, 0 stale@@!check-faultpoints: OK" "$d"
d=$(mk unlcase); g "$d" internal/jobs/new.go '	faultpoint.Hit("Start.before_running")'
                expect "a name that differs only by case is a different name" 1 "$(unl internal/jobs/new.go 6 Start.before_running)" "$d"
d=$(mk unltypo); g "$d" internal/jobs/new.go '	faultpoint.Hit("start.before_runing")'
                expect "a misspelt name is unlisted (an unarmable point would never be exercised)" 1 "$(unl internal/jobs/new.go 6 start.before_runing)" "$d"
d=$(mk unlemp); g "$d" internal/jobs/new.go '	faultpoint.Hit("")'
                expect "an empty name is a name the registry does not list" 1 "$(unl internal/jobs/new.go 6 '')" "$d"
d=$(mk nlvar);  g "$d" internal/jobs/new.go '	name := "start.before_running"
	faultpoint.Hit(name)'
                expect "a Hit through a variable cannot be read: red, at the call" 1 "$(nonlit internal/jobs/new.go 7)@@$RED1: 0 registry, 1 hit" "$d"
d=$(mk nlcat);  g "$d" internal/jobs/new.go '	faultpoint.Hit("start." + "before_running")'
                expect "a Hit with a concatenation cannot be read" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk nlcat2); g "$d" internal/jobs/new.go '	faultpoint.Hit(prefix + "start.before_running")'
                expect "a Hit with a prefix variable and a literal cannot be read" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk nlraw);  g "$d" internal/jobs/new.go '	faultpoint.Hit(`start.before_running`)'
                expect "a Hit with a raw-string name cannot be read (only a double-quoted literal is)" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk nlnone); g "$d" internal/jobs/new.go '	faultpoint.Hit()'
                expect "a Hit with no argument cannot be read" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk nlval);  g "$d" internal/jobs/new.go '	f := faultpoint.Hit
	f("start.before_running")'
                expect "Hit used as a value (a function variable) hides every call behind it: red at the selector" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk nlargs); g "$d" internal/jobs/new.go '	faultpoint.Hit("start.before_running", extra)'
                expect "a Hit with a second argument cannot be read" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk cmtunl); g "$d" internal/jobs/new.go '	// faultpoint.Hit("jobs.brand_new") would go here'
                expect "a comment that shows a call of an UNREGISTERED point is red (comments are not stripped: reword it)" 1 "$(unl internal/jobs/new.go 6 jobs.brand_new)" "$d"
d=$(mk blkunl); g "$d" internal/jobs/new.go '	/*
	   faultpoint.Hit("jobs.brand_new")
	*/'
                expect "the same inside a multi-line block comment" 1 "$(unl internal/jobs/new.go 7 jobs.brand_new)" "$d"
d=$(mk cmtnl);  g "$d" internal/jobs/new.go '	// the old code called faultpoint.Hit(name) here'
                expect "a comment that shows a call with a non-literal argument is red too" 1 "$(nonlit internal/jobs/new.go 6)" "$d"
d=$(mk twoun);  g "$d" internal/jobs/new.go '	faultpoint.Hit("a.one"); faultpoint.Hit("b.two")'
                expect "two unregistered calls on one line are both reported" 1 "$(unl internal/jobs/new.go 6 a.one)@@$(unl internal/jobs/new.go 6 b.two)@@RED — 2 problem(s)" "$d"
d=$(mk multi);  g "$d" internal/jobs/new.go '	faultpoint.
		Hit(
			"jobs.brand_new",
		)'
                expect "an unregistered call split over lines is reported at the line of the call (the selector)" 1 "$(unl internal/jobs/new.go 6 jobs.brand_new)" "$d"
d=$(mk cmddir); g "$d" cmd/platform/boot.go '	faultpoint.Hit("jobs.brand_new")'
                expect "a file under cmd/ is scanned too" 1 "$(unl cmd/platform/boot.go 6 jobs.brand_new)" "$d"
d=$(mk deep);   g "$d" internal/a/b/c/deep.go '	faultpoint.Hit("jobs.brand_new")'
                expect "a file in a nested directory is scanned" 1 "$(unl internal/a/b/c/deep.go 6 jobs.brand_new)" "$d"
d=$(mk mixed);  g "$d" internal/jobs/new.go '	faultpoint.Hit("start.before_running")
	faultpoint.Hit("jobs.brand_new")
	faultpoint.Hit("settle.after_commit")'
                expect "the control of the negatives: among registered calls exactly the one unregistered call (line 7) is reported" 1 "$(unl internal/jobs/new.go 7 jobs.brand_new)@@!internal/jobs/new.go:6:@@!internal/jobs/new.go:8:@@$RED1: 0 registry, 1 hit, 0 stale" "$d"
d=$(mk alias);  put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine

import (
	"fmt"

	fp "github.com/coldmail-platform/platform/internal/faultpoint"
)

func submit() {
	fmt.Println()
	fp.Hit("submit.after_module")
}
EOF
                expect "the package imported under another name hides its calls: the import is red at its line, and the point it hid is stale" 1 "$(alias_ internal/jobengine/engine.go 6)@@$(stale submit.after_module 3)@@RED — 2 problem(s)" "$d"
d=$(mk dotimp); put "$d" internal/app/dot.go <<'EOF'
package app

import . "github.com/coldmail-platform/platform/internal/faultpoint"

func f() { Hit("start.before_running") }
EOF
                expect "a dot import is red" 1 "$(alias_ internal/app/dot.go 3)" "$d"
d=$(mk blankimp); put "$d" internal/app/blank.go <<'EOF'
package app

import _ "github.com/coldmail-platform/platform/internal/faultpoint"
EOF
                expect "a blank import is red" 1 "$(alias_ internal/app/blank.go 3)" "$d"
d=$(mk oneimp); put "$d" internal/app/one.go <<'EOF'
package app

import fp "github.com/coldmail-platform/platform/internal/faultpoint"

func f() { fp.Hit("start.before_running") }
EOF
                expect "a single-line import with an alias is red too" 1 "$(alias_ internal/app/one.go 3)" "$d"

# ---- (a) the stale half ----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk stale);  printf 'zz.never_hit  # a name nothing hits\n' >> "$d/$REG"
                expect "a registered name that nothing hits: red at its line of the registry" 1 "$(stale zz.never_hit 7)@@$RED1: 0 registry, 0 hit, 1 stale@@!check-faultpoints: OK" "$d"
d=$(mk stalecode); put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine

func submit() {}
EOF
                expect "the hit removed from the code leaves its name stale (the real way a name goes stale), at the name's own line" 1 "$(stale submit.after_module 3)" "$d"
d=$(mk stalecm); put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine

import "github.com/coldmail-platform/platform/internal/faultpoint"

func submit() {
	// faultpoint.Hit("submit.after_module")
}
EOF
                expect "a name that only a // comment still hits is stale, and the reason says it is named in a comment only" 1 "$(stale submit.after_module 3)@@(it is named in a comment only: a comment is not a hit)" "$d"
d=$(mk stalebl); put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine

import "github.com/coldmail-platform/platform/internal/faultpoint"

func submit() {
	/*
		faultpoint.Hit("submit.after_module")
	*/
}
EOF
                expect "a name that only a multi-line block comment still hits (no leading star) is stale" 1 "$(stale submit.after_module 3)@@(it is named in a comment only" "$d"
d=$(mk stalets); mv "$d/internal/jobengine/engine.go" "$d/internal/jobengine/engine_test.go"
                expect "a name hit only by a _test.go file is stale: tests are not hits" 1 "$(stale submit.after_module 3)" "$d"
d=$(mk staletd); rm "$d/internal/jobengine/engine.go"; put "$d" tests/harness/fx/hit.go <<'EOF'
package main

import "github.com/coldmail-platform/platform/internal/faultpoint"

func f() { faultpoint.Hit("submit.after_module") }
EOF
                expect "a name hit only under tests/ is stale: the registry reads internal/ and cmd/" 1 "$(stale submit.after_module 3)" "$d"
d=$(mk stale2); printf 'zz.one  # a\nzz.two  # b\n' >> "$d/$REG"
                expect "two stale names are each reported at their own line" 1 "$(stale zz.one 7)@@$(stale zz.two 8)@@RED — 2 problem(s)" "$d"
d=$(mk twice);  g "$d" internal/jobs/again.go '	faultpoint.Hit("submit.after_module")'
                rm "$d/internal/jobengine/engine.go"
                expect "a name hit in two files stays green when one of the two goes: the other still hits it" 0 "check-faultpoints: OK@@found 5 faultpoint.Hit call(s) outside comments" "$d"
d=$(mk staleblank); printf 'zz.one  # a\n' >> "$d/$REG"; g "$d" internal/jobs/zz.go '	faultpoint.Hit("zz.one")'
                expect "the control of the stale cases: the name hit by a live call is not stale" 0 "check-faultpoints: OK@@covering 6 registered point(s)" "$d"

# ---- (a) the registry file ------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk rblank); { head -n 3 "$d/$REG"; printf '\n'; tail -n +4 "$d/$REG"; } > "$T/reg" && cp "$T/reg" "$d/$REG"
                expect "a blank line in the registry is red, at its line" 1 "$(regerr 4 'blank line: the registry holds one name per line and nothing else')@@$RED1: 1 registry" "$d"
d=$(mk rws);    printf '   \t \n' >> "$d/$REG"
                expect "a line of only whitespace is blank too" 1 "$(regerr 7 'blank line: the registry holds one name per line')" "$d"
d=$(mk rcr);    printf '\r\n' >> "$d/$REG"
                expect "a line of only a carriage return is blank too" 1 "$(regerr 7 'blank line: the registry holds one name per line')" "$d"
d=$(mk rdup);   printf 'start.before_running  # a second comment\n' >> "$d/$REG"
                expect "a duplicate name is red at the second line, naming the first" 1 "$(regerr 7 "duplicate name 'start.before_running' (first on line 4)")" "$d"
d=$(mk rup);    printf 'Zz.Upper  # x\n' >> "$d/$REG"
                expect "an upper-case name is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")@@Zz.Upper  # x" "$d"
d=$(mk rnodot); printf 'nodot  # x\n' >> "$d/$REG"
                expect "a name without a dot is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rdot2);  printf 'a.b.c  # x\n' >> "$d/$REG"
                expect "a name with two dots is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rcolon); printf 'zz.count:2  # x\n' >> "$d/$REG"
                expect "a name with a colon is malformed (the arming variable's own separator)" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rlead);  printf '  zz.indented  # x\n' >> "$d/$REG"
                expect "a name with leading space is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rinner); printf 'zz.two words  # x\n' >> "$d/$REG"
                expect "a name with a space inside is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rnohash); printf 'zz.where it is hit\n' >> "$d/$REG"
                expect "a name followed by words but no # is malformed" 1 "$(regerr 7 "malformed line (want '<point>.<position>  # where it is hit'")" "$d"
d=$(mk rbare);  printf 'zz.bare\n' >> "$d/$REG"
                expect "a bare name has no comment: red (the comment is mandatory)" 1 "$(regerr 7 "name 'zz.bare' has no comment saying where it is hit")" "$d"
d=$(mk remp);   printf 'zz.hash  #   \n' >> "$d/$REG"
                expect "a name with an empty comment has no comment" 1 "$(regerr 7 "name 'zz.hash' has no comment saying where it is hit")" "$d"
d=$(mk rnone);  rm "$d/$REG"
                expect "a missing registry is red, not empty" 1 "$REG:1: the registry does not exist: the gate cannot say which fault points are allowed" "$d"
d=$(mk rcomm);  printf '# only comments\n# nothing else\n' > "$d/$REG"
                expect "a registry of only comments holds 0 names: the floor is red" 1 "$REG:1: the registry holds 0 names: the instrument matched nothing@@lists 0 name(s)" "$d"
d=$(mk rempty); : > "$d/$REG"
                expect "an empty registry holds 0 names: the floor is red" 1 "$REG:1: the registry holds 0 names: the instrument matched nothing" "$d"

# ---- (b) the build line ---------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk armed);  put "$d" internal/faultpoint/zz_armed.go <<'EOF'
package faultpoint

// FAULT: a file that reads the arming variable and carries no build line.
var zzArmed = "PLATFORM_FAULTPOINT"
EOF
                expect "THE FAULT of the first fault proof: a new file that names the arming variable without the build line is red, at the line of the mention" 1 "$(varf internal/faultpoint/zz_armed.go 4)@@$RED1: 0 registry, 0 hit, 0 stale, 1 build-line" "$d"
d=$(mk bnotfirst); put "$d" internal/app/arm.go <<'EOF'
package app

//go:build faultpoints

import "os"

var armed = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "the build line is there but not the first line: red (it must START the file)" 1 "$(varf internal/app/arm.go 7)" "$d"
d=$(mk bcmt);   put "$d" internal/app/arm.go <<'EOF'
// Copyright notice.
//go:build faultpoints

package app

import "os"

var armed = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "a comment before the build line: red, at the mention (line 8), and the build line itself, no longer first, is a second finding (line 2)" 1 "$(varf internal/app/arm.go 8)@@$(gotag internal/app/arm.go 2)@@RED — 2 problem(s)" "$d"
d=$(mk bblank); put "$d" internal/app/arm.go <<'EOF'

//go:build faultpoints

package app

var armed = "PLATFORM_FAULTPOINT"
EOF
                expect "a blank line before the build line: red (the file does not START with it)" 1 "$(varf internal/app/arm.go 6)" "$d"
d=$(mk binert); put "$d" internal/app/arm.go <<'EOF'
//go:build !faultpoints

package app

var armed = "PLATFORM_FAULTPOINT"
EOF
                expect "the INERT build line (!faultpoints) on a file that names the variable is red: the default build would hold it" 1 "$(varf internal/app/arm.go 5)" "$d"
d=$(mk bdef);   put "$d" internal/faultpoint/faultpoint.go <<'EOF'
//go:build !faultpoints

// Package faultpoint: every file that reads PLATFORM_FAULTPOINT carries the build tag.
package faultpoint

func Hit(name string) {}
EOF
                expect "the default file naming the variable in its doc comment (what faultpoint.go did before it was reworded) is red: a comment counts" 1 "$(varf internal/faultpoint/faultpoint.go 3)" "$d"
d=$(mk bcombo); put "$d" internal/app/arm.go <<'EOF'
//go:build faultpoints || linux

package app

var armed = "PLATFORM_FAULTPOINT"
EOF
                expect "a combined build line (faultpoints || linux) is not the build line: red" 1 "$(varf internal/app/arm.go 5)" "$d"
d=$(mk bcmd);   put "$d" cmd/platform/arm.go <<'EOF'
package main

import "os"

var armed = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "a file under cmd/ is scanned" 1 "$(varf cmd/platform/arm.go 5)" "$d"
d=$(mk btests); put "$d" tests/harness/fx/arm.go <<'EOF'
package main

import "os"

var armed = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "a non-test Go file under tests/ is scanned for the variable" 1 "$(varf tests/harness/fx/arm.go 5)" "$d"
d=$(mk blower); put "$d" internal/app/arm.go <<'EOF'
package app

var armed = "platform_faultpoint"
EOF
                expect "the variable's name in lower case is a mention too" 1 "$(varf internal/app/arm.go 3)" "$d"
d=$(mk bcomment); put "$d" internal/app/arm.go <<'EOF'
package app

// tell the process to die by setting PLATFORM_FAULTPOINT to a name
func f() {}
EOF
                expect "a mention in a comment counts (comments are not stripped: reword it)" 1 "$(varf internal/app/arm.go 3)" "$d"
d=$(mk bfirst); put "$d" internal/app/arm.go <<'EOF'
package app

import "os"

var a = os.Getenv("PLATFORM_FAULTPOINT")
var b = os.Getenv("PLATFORM_FAULTPOINT")
EOF
                expect "one finding per file, at the FIRST mention" 1 "$(varf internal/app/arm.go 5)@@!internal/app/arm.go:6:@@$RED1: 0 registry, 0 hit, 0 stale, 1 build-line" "$d"
d=$(mk bnone);  rm "$d/internal/faultpoint/faultpoint_on.go"
                expect "no file names the variable at all: the armed implementation is gone, the floor is red" 1 "no non-test Go file under internal/, cmd/ or tests/ names the fault-point environment variable PLATFORM_FAULTPOINT: the instrument matched nothing@@0 Go file(s) name the fault-point environment variable" "$d"
d=$(mk bonlytest); rm "$d/internal/faultpoint/faultpoint_on.go"; put "$d" internal/faultpoint/on_test.go <<'EOF'
//go:build faultpoints

package faultpoint

var x = "PLATFORM_FAULTPOINT"
EOF
                expect "the variable named only in a test file does not satisfy the floor (the armed implementation is not a test)" 1 "no non-test Go file under internal/, cmd/ or tests/ names the fault-point environment variable" "$d"

# ---- (c) the Dockerfile ----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk dfnoarg); printf 'FROM golang:1.26-alpine\nRUN go build -o /out/platform ./cmd/platform\n' > "$d/Dockerfile"
                expect "a Dockerfile without the ARG GO_TAGS line is red" 1 "$ARG_MISSING@@$RED1: 0 registry, 0 hit, 0 stale, 0 build-line, 0 Go tag, 1 Dockerfile" "$d"
d=$(mk dfnodef); printf 'FROM golang:1.26-alpine\nARG GO_TAGS\nRUN go build -tags "${GO_TAGS}" ./cmd/platform\n' > "$d/Dockerfile"
                expect "ARG GO_TAGS without the empty default is not the line" 1 "$ARG_MISSING" "$d"
d=$(mk dfcmt);  printf 'FROM golang:1.26-alpine\n# ARG GO_TAGS=""\nRUN go build ./cmd/platform\n' > "$d/Dockerfile"
                expect "the line only in a comment does not count" 1 "$ARG_MISSING" "$d"
d=$(mk dfsingle); printf "FROM golang:1.26-alpine\nARG GO_TAGS=''\nRUN go build ./cmd/platform\n" > "$d/Dockerfile"
                expect "the textual check is exact: single quotes are not the line" 1 "$ARG_MISSING" "$d"
d=$(mk dfindent); printf 'FROM golang:1.26-alpine\n  ARG GO_TAGS=""\nRUN go build ./cmd/platform\n' > "$d/Dockerfile"
                expect "the textual check is exact: an indented line is not the line" 1 "$ARG_MISSING" "$d"
d=$(mk dftag);  printf 'FROM golang:1.26-alpine\nARG GO_TAGS="faultpoints"\nRUN go build -tags "${GO_TAGS}" ./cmd/platform\n' > "$d/Dockerfile"
                expect "a default that is the tag is red twice over: the line is missing and the word is there" 1 "$ARG_MISSING@@$(dfword Dockerfile 2)@@RED — 2 problem(s)" "$d"
d=$(mk dfword); printf 'FROM golang:1.26-alpine\nARG GO_TAGS=""\n# the harness builds this with faultpoints\nRUN go build -tags "${GO_TAGS}" ./cmd/platform\n' > "$d/Dockerfile"
                expect "the word in a Dockerfile comment is red: the recipe holds no faultpoints word at all" 1 "$(dfword Dockerfile 3)@@$RED1: 0 registry, 0 hit, 0 stale, 0 build-line, 0 Go tag, 1 Dockerfile" "$d"
d=$(mk dfrun);  printf 'FROM golang:1.26-alpine\nARG GO_TAGS=""\nRUN go build -tags faultpoints ./cmd/platform\n' > "$d/Dockerfile"
                expect "a go build with the tag written into the recipe is red, at the line" 1 "$(dfword Dockerfile 3)" "$d"
d=$(mk dfnone); rm "$d/Dockerfile"
                expect "no root Dockerfile: the instrument matched nothing" 1 "Dockerfile:1: the root Dockerfile does not exist: the instrument matched nothing" "$d"
d=$(mk dfother2); put "$d" tests/harness/m1/Dockerfile <<'EOF'
FROM alpine:3.24
RUN go build -tags faultpoints ./cmd/platform
EOF
                expect "another Dockerfile with the tag is red too, at its own path and line" 1 "$(dfword tests/harness/m1/Dockerfile 2)@@2 Dockerfile(s)" "$d"
d=$(mk dfdot);  put "$d" build/app.Dockerfile <<'EOF'
FROM alpine:3.24
ENV GO_TAGS=faultpoints
EOF
                expect "a *.Dockerfile is a Dockerfile" 1 "$(dfword build/app.Dockerfile 2)" "$d"

# ---- (d) the workflows -----------------------------------------------------------------------------------------------------------------------------------------------------------------------
GOYML=.github/workflows/go.yml
d=$(mk wbuild); put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: go build -tags faultpoints ./...
EOF
                expect "go build with the tag in go.yml is red, at the line" 1 "$(wftag "$GOYML" 5)@@$RED1: 0 registry, 0 hit, 0 stale, 0 build-line, 0 Go tag, 0 Dockerfile, 1 workflow" "$d"
d=$(mk wtest);  put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - name: Test
        run: go test -race -tags faultpoints ./...
EOF
                expect "go test with the tag is red" 1 "$(wftag "$GOYML" 6)" "$d"
d=$(mk wdock);  put "$d" .github/workflows/harness.yml <<'EOF'
name: harness
jobs:
  harness:
    steps:
      - run: docker build --build-arg GO_TAGS=faultpoints -t platform .
EOF
                expect "docker build with GO_TAGS in a workflow is red, naming GO_TAGS" 1 "$(wfgo .github/workflows/harness.yml 5)" "$d"
d=$(mk wenv);   put "$d" .github/workflows/harness.yml <<'EOF'
name: harness
env:
  GO_TAGS: ""
jobs:
  harness:
    steps:
      - run: echo hi
EOF
                expect "a workflow that sets GO_TAGS to anything, even empty, is red: it has no business with the argument" 1 "$(wfgo .github/workflows/harness.yml 3)" "$d"
d=$(mk wvetother); put "$d" .github/workflows/harness.yml <<'EOF'
name: harness
jobs:
  harness:
    steps:
      - run: go vet -tags faultpoints ./...
EOF
                expect "a go vet line with the tag in a workflow other than go.yml is red" 1 "$(wftag .github/workflows/harness.yml 5)" "$d"
d=$(mk wand);   put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: go vet -tags faultpoints ./... && go build -tags faultpoints ./cmd/platform
EOF
                expect "a go vet line joined with a go build by && is red" 1 "$(wftag "$GOYML" 5)" "$d"
d=$(mk wsemi);  put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: go vet -tags faultpoints ./...; go install -tags faultpoints ./cmd/platform
EOF
                expect "a go vet line joined with a go install by ; is red" 1 "$(wftag "$GOYML" 5)" "$d"
d=$(mk wflags); put "$d" "$GOYML" <<'EOF'
name: go
env:
  GOFLAGS: -tags=faultpoints
jobs:
  go:
    steps:
      - run: go build ./...
EOF
                expect "the tag set through GOFLAGS is red" 1 "$(wftag "$GOYML" 3)" "$d"
d=$(mk wrunq);  put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: "go vet -tags faultpoints ./..."
EOF
                expect "a quoted run line is not a plain go vet line (fail closed)" 1 "$(wftag "$GOYML" 5)" "$d"
d=$(mk wtrail); put "$d" "$GOYML" <<'EOF'
name: go
jobs:
  go:
    steps:
      - run: go build ./... # with -tags faultpoints
EOF
                expect "a trailing comment on a build line that names the tag is red (only a whole-line comment is skipped)" 1 "$(wftag "$GOYML" 5)" "$d"
d=$(mk wyaml);  put "$d" .github/workflows/extra.yaml <<'EOF'
name: extra
jobs:
  extra:
    steps:
      - run: go build -tags faultpoints ./...
EOF
                expect "a .yaml workflow is scanned too" 1 "$(wftag .github/workflows/extra.yaml 5)@@3 workflow file(s)" "$d"
d=$(mk wnone);  rm -rf "$d/.github"
                expect "no workflow file at all: the tag rule has no subject, red" 1 "found 0 workflow files under .github/workflows in $d: the instrument matched nothing@@0 workflow file(s)" "$d"

# ---- (d) the tag in Go ----------------------------------------------------------------------------------------------------------------------------------------------------------------------
d=$(mk gotag);  put "$d" internal/app/build.go <<'EOF'
package app

import "os/exec"

func build() { exec.Command("go", "build", "-tags", "faultpoints", "./cmd/platform") }
EOF
                expect "a Go file that passes -tags faultpoints and does not start with the build line is red, at the line" 1 "$(gotag internal/app/build.go 5)" "$d"
d=$(mk gotag2); put "$d" internal/app/build.go <<'EOF'
//go:build faultpoints && linux

package app

func f() {}
EOF
                expect "a //go:build line that names the tag in a combination is not one of the two allowed lines: red" 1 "$(gotag internal/app/build.go 1)" "$d"
d=$(mk gotag3); put "$d" internal/app/build.go <<'EOF'
// Package app.
//go:build faultpoints

package app

func f() {}
EOF
                expect "a //go:build line that is not the first line is red" 1 "$(gotag internal/app/build.go 2)" "$d"
d=$(mk gotag4); put "$d" internal/app/build_test.go <<'EOF'
package app

import "os/exec"

func build() { exec.Command("go", "build", "-tags", "faultpoints", "./cmd/platform") }
EOF
                expect "the same in a _test.go file is not read" 0 "check-faultpoints: OK" "$d"

# ---- the floors, the arguments, the matcher's own canary ------------------------------------------------------------------------------------------------------------------------------------
d=$(mk nogo);   rm -rf "$d/internal" "$d/cmd"; mkdir -p "$d/internal" "$d/cmd"
                expect "no Go file at all: the gate matched nothing" 1 "scanned 0 Go file(s) under internal/ and cmd/ (1 with tests/)@@found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk onlytest); rm -rf "$d/internal" "$d/cmd"; put "$d" internal/app/a_test.go <<'EOF'
package app
EOF
                expect "only _test.go files: no Go file is scanned, so the gate matched nothing" 1 "found 0 Go files under internal/ and cmd/@@the instrument matched nothing" "$d"
d=$(mk nohit);  put "$d" internal/jobs/engine.go <<'EOF'
package jobs
EOF
                put "$d" internal/jobs/confirm.go <<'EOF'
package jobs
EOF
                put "$d" internal/jobengine/engine.go <<'EOF'
package jobengine
EOF
                expect "no Hit anywhere: five stale names and the floor of live hits is red" 1 "found 0 faultpoint.Hit calls outside comments in non-test Go under internal/ and cmd/: the instrument matched nothing@@$(stale confirm.before_commit 2)@@5 stale" "$d"
d=$(mk noroot); expect "a root that is not a directory" 1 "the root '$T/no-such-root' is not a directory: the instrument matched nothing" "$d" --root "$T/no-such-root"
d=$(mk usage1); expect "an unknown argument is a usage error" 2 "check-faultpoints: unknown argument '--bogus'@@usage: check-faultpoints.sh [--root DIR]" "$d" --bogus
d=$(mk usage2); expect "--root with no value is a usage error" 2 "usage: check-faultpoints.sh [--root DIR]" "$d" --root
d=$(mk usage3); expect "a stray positional argument is a usage error" 2 "check-faultpoints: unknown argument 'extra'" "$d" --root "$d" extra
mkdir -p "$T/shim"; printf '#!/bin/sh\nexit 1\n' > "$T/shim/grep"; chmod +x "$T/shim/grep"
d=$(mk canary); out="$(PATH="$T/shim:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "a matcher that finds nothing (grep replaced by a stub) fails its own canary instead of passing a clean tree" "$rc" 1 "check-faultpoints: RED — the matcher failed its own canary@@canary: wanted 'L:a.b', found ''@@!check-faultpoints: OK" "$out"
# a grep that works for the canaries but CRASHES on one file (here: any file containing POISON) must not read as a clean file
REAL_GREP="$(command -v grep)"; mkdir -p "$T/shim2"
printf '#!/bin/sh\nfor last; do :; done\nif "%s" -q POISON "$last" 2>/dev/null; then exit 3; fi\nexec "%s" "$@"\n' "$REAL_GREP" "$REAL_GREP" > "$T/shim2/grep"; chmod +x "$T/shim2/grep"
d=$(mk crash);  g "$d" internal/app/q.go '	faultpoint.Hit("start.before_running") // POISON'
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on one file is red and names the file (a crashed matcher prints no match: it must not read as clean)" "$rc" 1 "the matcher failed on internal/app/q.go (grep exit status 3)@@1 file(s) the matcher failed on@@1 unread file(s)@@!check-faultpoints: OK" "$out"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$(mk crashok)" 2>&1)"; rc=$?
                check "the same crashing-grep stub on a tree without POISON behaves like the real grep (the stub itself is not the cause)" "$rc" 0 "check-faultpoints: OK@@0 file(s) the matcher failed on" "$out"
d=$(mk crashreg); printf '\n// POISON\n' >> "$d/internal/jobs/engine.go"
                out="$(PATH="$T/shim2:$PATH" bash "$GATE" --root "$d" 2>&1)"; rc=$?
                check "grep crashing on a file that holds registered points is red, and its names are not credited" "$rc" 1 "the matcher failed on internal/jobs/engine.go (grep exit status 3)@@$(stale start.before_running 4)@@!check-faultpoints: OK" "$out"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Four deliberately WRONG arrangements, each run through the same `expect`. EVERY one must be reported as a mismatch; if any is accepted, this selftest cannot fail and says so.
#   1. a gate that always exits 0, run on a violating tree that must be red                      (the selftest must go red if the gate were `exit 0`)
#   2. a gate that fails WITHOUT naming a reason, run on a violating tree that must be red for the reason   (class 10: the right exit code for no reason)
#   3. the real gate on a clean tree, expected to be red
#   4. the real gate on a violating tree, expected to be green
printf '#!/bin/sh\nexit 0\n' > "$T/stub-pass.sh"; printf '#!/bin/sh\necho boom\nexit 1\n' > "$T/stub-bare-red.sh"
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases; real="$GATE"
d=$(mk ctlv); g "$d" internal/jobs/new.go '	faultpoint.Hit("jobs.brand_new")'
GATE="$T/stub-pass.sh";     expect "CONTROL 1 (wrong on purpose): a gate that is always green" 1 "$(unl internal/jobs/new.go 6 jobs.brand_new)" "$d" >/dev/null 2>&1
GATE="$T/stub-bare-red.sh"; expect "CONTROL 2 (wrong on purpose): a gate that is red for no named reason" 1 "$(unl internal/jobs/new.go 6 jobs.brand_new)" "$d" >/dev/null 2>&1
GATE="$real"
d=$(mk ctlc); expect "CONTROL 3 (wrong on purpose): a clean tree expected to be RED" 1 "names a point that $REG does not list" "$d" >/dev/null 2>&1
d=$(mk ctlg); g "$d" internal/jobs/new.go '	faultpoint.Hit("jobs.brand_new")'; expect "CONTROL 4 (wrong on purpose): a violating tree expected to be GREEN" 0 "check-faultpoints: OK" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 4 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] all four deliberately wrong arrangements were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 4 wrong arrangements were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
