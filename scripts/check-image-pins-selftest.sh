#!/usr/bin/env bash
# check-image-pins-selftest — the PAIRED CONTROL for check-image-pins.sh.
#
# On a pinned tree the gate prints `ok`, which leaves every one of its failure branches unexercised: it could be broken into
# accepting everything and CI would stay green forever. This builds throwaway trees that trip each rule in turn — in Dockerfile,
# compose AND workflow form — and asserts, for every REJECT case, THREE things, not just a nonzero exit (false-green class 10: a
# reject case that fails for an unrelated reason is not a control):
#   1. exit status 1 (usage errors: 2),
#   2. the output contains a SUBSTRING NAMING THE REASON — usually `<file>:<line>: <ref> — <reason>`, so the right line of the
#      right file is flagged for the right reason,
#   3. the verdict says exactly N problem(s), normally ONE — a fixture that also trips another rule is caught here.
# Every GOOD case asserts rc 0 plus the `ok` verdict; every run must print its `NOT CHECKED:` line.
#
# Two guards on THIS file:
#   * a hard-coded MIN_CASES floor — deleting cases is red, not a quiet shrinkage of coverage (class 8);
#   * a final CONTROL case: two deliberately wrong expectations are fed to the harness inside a subshell and BOTH must be
#     reported as mismatches — proof that `good`/`red` can fail at all (class 9/18).
# CRLF cases write real CR bytes and ASSERT they are present (on this author's host awk and sed hide `\r`, so a fixture whose
# CRs were lost would prove nothing). A pinned CRLF Dockerfile must be GREEN: left unstripped, `\r` would make every digest
# malformed.
# Not covered here: a real registry (no network, no Docker), and the repositories' own files — this runs on fixtures only.
set -u -o pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/check-image-pins.sh"
MIN_CASES=77
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
D=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa   # 64 lowercase hex
UP=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA   # 64 UPPERCASE hex
PIN="alpine:3.20@sha256:$D"

cases=0; okc=0; bad=0
record_ok()  { cases=$((cases + 1)); okc=$((okc + 1)); echo "  ok   $1"; }
record_bad() { cases=$((cases + 1)); bad=$((bad + 1)); echo "  BAD  $1 — $2"; }
count_in() { local n; n="$(grep -cF -- "$1" <<<"$2" || true)"; echo "${n:-0}"; }

good_with() {   # good_with <label> <substring that must appear> <gate args...>
  local label="$1" want="$2"; shift 2
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  [ "$rc" -eq 0 ] || { record_bad "$label" "expected rc 0, got $rc: $out"; return; }
  [ "$(count_in 'check-image-pins: ok' "$out")" -ge 1 ] || { record_bad "$label" "rc 0 but no 'ok' verdict: $out"; return; }
  [ "$(count_in 'NOT CHECKED:' "$out")" -ge 1 ]         || { record_bad "$label" "no NOT CHECKED: line: $out"; return; }
  [ "$(count_in "$want" "$out")" -ge 1 ]                || { record_bad "$label" "output lacks '$want': $out"; return; }
  record_ok "$label"
}
good() { local label="$1"; shift; good_with "$label" 'check-image-pins: ok' "$@"; }
red() {         # red <label> <reason substring> <expected problem count> <gate args...>
  local label="$1" want="$2" np="$3"; shift 3
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  [ "$rc" -eq 1 ] || { record_bad "$label" "expected rc 1, got $rc: $out"; return; }
  [ "$(count_in "$want" "$out")" -ge 1 ] || { record_bad "$label" "rejected, but not for the reason named ('$want'): $out"; return; }
  [ "$(count_in "check-image-pins: RED ($np problem(s))" "$out")" -ge 1 ] || { record_bad "$label" "verdict is not exactly $np problem(s) (a second rule also fired, or fewer): $out"; return; }
  [ "$(count_in 'NOT CHECKED:' "$out")" -ge 1 ] || { record_bad "$label" "no NOT CHECKED: line: $out"; return; }
  record_ok "$label (rejected: $want)"
}
usage_case() {  # usage_case <label> <substring> <gate args...>   -> rc 2
  local label="$1" want="$2"; shift 2
  local out rc; out="$(bash "$GATE" "$@" 2>&1)"; rc=$?
  if [ "$rc" -eq 2 ] && [ "$(count_in "$want" "$out")" -ge 1 ]; then record_ok "$label (usage error: $want)"
  else record_bad "$label" "expected rc 2 with '$want', got rc=$rc: $out"; fi
}

ROOT="$TMP/root"
fx()  { rm -rf "$ROOT"; mkdir -p "$ROOT"; }
put() { local p="$ROOT/$1"; mkdir -p "$(dirname "$p")"; printf '%b' "$2" > "$p"; }       # put <relpath> <content with \n escapes>
putf() { local p="$ROOT/$1"; mkdir -p "$(dirname "$p")"; cat > "$p"; }                      # putf <relpath> <<EOF ... EOF
# crlf_file <relpath>: rewrite with CRLF line ends and PROVE CR bytes are present; returns 1 (and records BAD) otherwise
crlf_file() {
  local p="$ROOT/$1" t l n; t="$p.crlf"
  while IFS= read -r l || [ -n "$l" ]; do printf '%s\r\n' "$l"; done < "$p" > "$t"; mv "$t" "$p"
  n="$(tr -cd '\r' < "$p" | wc -c | tr -d ' ')"
  if [ "${n:-0}" -le 0 ]; then record_bad "CRLF fixture $1" "no CR bytes were written: the CRLF case would prove nothing"; return 1; fi
  return 0
}

echo "check-image-pins-selftest: GOOD cases"
fx; put Dockerfile "FROM $PIN\nRUN true\n"
good_with "a digest-pinned Dockerfile passes" "found=1 pinned=1 exempt=0 red=0" "$ROOT"
fx; put Dockerfile "FROM --platform=\$BUILDPLATFORM golang:1.22@sha256:$D AS build\nFROM build AS test\nFROM scratch\nFROM $PIN AS Final\nFROM final\n"
good_with "exempt forms: --platform flag, FROM scratch, FROM <earlier alias> (case-insensitive)" "found=5 pinned=2 exempt=3 red=0" "$ROOT"
fx; put Dockerfile "FROM alpine@sha256:$D\n"
good "a digest with no tag (name@sha256:...) passes" "$ROOT"
fx; put Dockerfile "FROM localhost:5000/team/app:1.2@sha256:$D\n"
good "a registry host:port is not mistaken for a tag" "$ROOT"
fx; put Dockerfile "from $PIN as x\nFROM \\\\\n   $PIN\n"
good_with "lower-case 'from' and a FROM continued over two lines are both read" "found=2 pinned=2" "$ROOT"
fx; put Dockerfile "# FROM alpine:latest\nARG V=1\nFROM $PIN\nCOPY --from=0 /a /b\n# a comment ending in a backslash \\\\\nRUN docker pull alpine:latest\n"
good_with "commented-out FROM, ARG, COPY --from and RUN docker pull are ignored" "found=1 pinned=1" "$ROOT"
fx; putf docker-compose.yml <<EOF
services:
  a:
    image: "postgres:16@sha256:$D"
  b:
    image: 'redis:7@sha256:$D'   # a trailing comment
  c:
    image: mailpit:v1@sha256:$D
  # image: ghost:latest
  d:
    # image: ghost:latest
    build: .
EOF
good_with "compose: quoted (\" and ') and unquoted pinned images; commented-out image: lines are ignored" "found=3 pinned=3 exempt=0 red=0" "$ROOT"
fx; putf docker-compose.override.yml <<EOF
services:
  a:
    environment: [X=1]
EOF
put Dockerfile "FROM $PIN\n"
good_with "a compose override with no image: is fine when the tree has references" "found=1" "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
# column-0 comments: uses: docker://alpine:latest
# image: ghost:latest
jobs:
  j:
    runs-on: ubuntu-latest
    container:
      image: node:20@sha256:$D
    services:
      pg:
        image: "postgres:16@sha256:$D"
      cache:
        image: redis:7@sha256:$D # pinned
    steps:
      - uses: actions/checkout@v4
      - uses: docker://alpine:3.20@sha256:$D
      # - uses: docker://alpine:latest
      - uses: some/action@v1
        with:
          image: ghcr.io/me/to-build:latest
      - run: |
          image: fake2:latest
          echo docker://alpine:latest
  k:
    container: node:20@sha256:$D
    steps: []
EOF
good_with "workflow: services, container (mapping and string form), docker:// all pinned; with: image and run: text are not pulls" "found=5 pinned=5 exempt=0 red=0" "$ROOT"
good_with "workflow: the non-pull image: key is COUNTED as ignored, not silently dropped" "1 workflow 'image:' key(s) outside services/container" "$ROOT"
fx; put Dockerfile "FROM $PIN\n"; put docker-compose.yml "services:\n  a:\n    image: postgres:16@sha256:$D\n"
put .github/workflows/ci.yml "jobs:\n  j:\n    services:\n      pg:\n        image: postgres:16@sha256:$D\n"
for f in Dockerfile docker-compose.yml .github/workflows/ci.yml; do crlf_file "$f" || continue; done
good_with "CRLF: a pinned Dockerfile, compose file and workflow are all still GREEN (an unstripped \\r would malform every digest)" "found=3 pinned=3" "$ROOT"
fx; put Dockerfile "FROM $PIN\n"; put node_modules/x/Dockerfile "FROM alpine:latest\n"; put .git/Dockerfile "FROM alpine:latest\n"
put Docs/history/Dockerfile "FROM alpine:latest\n"; put Docs/history/docker-compose.yml "services:\n  a:\n    image: foo\n"
good_with "node_modules, .git and Docs/history are skipped, and the skipped count is printed" "2 matching file(s) under Docs/history were skipped" "$ROOT"
fx; put "dir with space/Dockerfile" "FROM $PIN\n"
good_with "a path containing a space is scanned" "found=1 pinned=1" "$ROOT"
fx; put app.Dockerfile "FROM $PIN\n"; put Dockerfile.dev "FROM $PIN\n"; put Dockerfile.dockerignore "node_modules\n"; put README.md "FROM alpine:latest\n"
good_with "app.Dockerfile and Dockerfile.dev are scanned; Dockerfile.dockerignore and .md are not" "found=2 pinned=2" "$ROOT"
fx; put a/Dockerfile "FROM $PIN\n"; put b/docker-compose.yml "services:\n  a:\n    image: postgres:16@sha256:$D\n"
good_with "two roots (a directory and a single file) are both scanned" "found=2 pinned=2" "$ROOT/a" "$ROOT/b/docker-compose.yml"
fx; put Dockerfile "FROM $PIN\nFROM scratch\nFROM $PIN AS b\n"
good "--min-refs 3 passes with exactly 3 references (scratch counts as found)" --min-refs 3 "$ROOT"

echo "check-image-pins-selftest: REJECT cases — Dockerfile"
fx; put Dockerfile "FROM alpine:3.20\n";                           red "Dockerfile: tag but no digest" "Dockerfile:1: alpine:3.20 — no digest (tag" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:latest\n";                         red "Dockerfile: :latest" "Dockerfile:1: alpine:latest — tag is :latest" 1 "$ROOT"
fx; put Dockerfile "FROM alpine\n";                                red "Dockerfile: no tag and no digest" "Dockerfile:1: alpine — no tag and no digest" 1 "$ROOT"
fx; put Dockerfile 'FROM ${BASE}\n';                               red "Dockerfile: \${BASE} interpolation" 'Dockerfile:1: ${BASE} — interpolated reference' 1 "$ROOT"
fx; put Dockerfile 'FROM $BASE AS x\n';                            red "Dockerfile: \$BASE interpolation" 'Dockerfile:1: $BASE — interpolated reference' 1 "$ROOT"
fx; put Dockerfile "FROM --platform=\$BUILDPLATFORM golang:1.22 AS build\n"; red "Dockerfile: --platform flag does not hide an unpinned image" "Dockerfile:1: golang:1.22 — no digest (tag" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20@sha256:abc\n";                red "Dockerfile: truncated digest" "Dockerfile:1: alpine:3.20@sha256:abc — malformed digest 'sha256:abc'" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20@sha256:$UP\n";                red "Dockerfile: UPPERCASE hex digest" "malformed digest 'sha256:$UP'" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20@sha512:$D\n";                 red "Dockerfile: sha512 algorithm" "malformed digest 'sha512:" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20@sha256:${D}0\n";              red "Dockerfile: a 65-character digest" "malformed digest 'sha256:${D}0'" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20@\n";                          red "Dockerfile: an empty digest after @" "malformed digest ''" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:latest@sha256:$D\n";               red "Dockerfile: :latest even WITH a valid digest (decision documented in the gate)" "Dockerfile:1: alpine:latest@sha256:$D — tag is :latest" 1 "$ROOT"
fx; put Dockerfile "FROM build\nFROM $PIN AS build\n";             red "Dockerfile: an alias used BEFORE it is defined is not exempt" "Dockerfile:1: build — no tag and no digest" 1 "$ROOT"
fx; put Dockerfile "FROM $PIN AS base\nFROM base\nFROM alpine:3.19 AS x\n"; red "Dockerfile: the unpinned later stage of a multi-stage build" "Dockerfile:3: alpine:3.19 — no digest (tag" 1 "$ROOT"
fx; put Dockerfile "from alpine:3.20\n";                           red "Dockerfile: lower-case 'from' is still read" "Dockerfile:1: alpine:3.20 — no digest (tag" 1 "$ROOT"
fx; put Dockerfile "FROM \\\\\n  alpine:3.20\n";                  red "Dockerfile: a FROM continued over two lines is still read" "Dockerfile:1: alpine:3.20 — no digest (tag" 1 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20\nFROM node:latest\nFROM \${X}\n"; red "Dockerfile: every red line is reported, not just the first (3 problems)" "Dockerfile:3: \${X} — interpolated reference" 3 "$ROOT"
fx; put Dockerfile "FROM alpine:3.20\r\n"
if crlf_file Dockerfile; then                                      red "CRLF Dockerfile: an unpinned FROM is still caught (and the ref carries no \\r)" "Dockerfile:1: alpine:3.20 — no digest (tag '3.20' only" 1 "$ROOT"; fi
fx; put Dockerfile "FROM alpine:3.20@sha256:abc\n"
if crlf_file Dockerfile; then                                      red "CRLF Dockerfile: a malformed digest is still caught" "Dockerfile:1: alpine:3.20@sha256:abc — malformed digest" 1 "$ROOT"; fi
fx; put docker-compose.yml "services:\n  a:\n    image: postgres:16@sha256:$D\n"; put Dockerfile "# nothing but a comment\nRUN true\n"
                                                                   red "Dockerfile with no FROM line at all" "Dockerfile has no FROM line" 1 "$ROOT"

echo "check-image-pins-selftest: REJECT cases — compose"
fx; put docker-compose.yml "services:\n  a:\n    image: postgres:16-alpine\n";    red "compose: tag but no digest" "docker-compose.yml:3: postgres:16-alpine — no digest (tag" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: axllent/mailpit:latest\n"; red "compose: :latest" "docker-compose.yml:3: axllent/mailpit:latest — tag is :latest" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: somevendor/thing\n";        red "compose: untagged" "docker-compose.yml:3: somevendor/thing — no tag and no digest" 1 "$ROOT"
fx; put docker-compose.yml 'services:\n  a:\n    image: "postgres:${PG_TAG}"\n';     red "compose: \${PG_TAG} interpolation, quoted" 'docker-compose.yml:3: postgres:${PG_TAG} — interpolated reference' 1 "$ROOT"
fx; put docker-compose.yml 'services:\n  a:\n    image: ${IMG:-postgres:16}\n';      red "compose: \${IMG:-default} interpolation" 'interpolated reference' 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: postgres:16@sha256:abc123\n"; red "compose: truncated digest" "malformed digest 'sha256:abc123'" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: postgres:16@sha256:$UP\n";    red "compose: UPPERCASE hex digest" "malformed digest 'sha256:$UP'" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: 'redis:7'\n";                 red "compose: single-quoted, no digest" "docker-compose.yml:3: redis:7 — no digest (tag" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image: redis:7 # pinned later\n";    red "compose: a trailing comment does not hide an unpinned image" "docker-compose.yml:3: redis:7 — no digest (tag" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    image:\n";                            red "compose: an empty image: value" "docker-compose.yml:3: <empty> — empty image reference" 1 "$ROOT"
fx; put docker-compose.prod.yml "services:\n  a:\n    image: redis:7\n";                red "compose: docker-compose.prod.yml is found by name" "docker-compose.prod.yml:3: redis:7" 1 "$ROOT"
fx; put sub/compose.yaml "services:\n  a:\n    image: redis:7\n";                       red "compose: compose.yaml in a subdirectory is found by name" "compose.yaml:3: redis:7" 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    - image: redis:7\r\n"
if crlf_file docker-compose.yml; then                                                    red "CRLF compose: an unpinned image is still caught (and the ref carries no \\r)" "docker-compose.yml:3: redis:7 — no digest (tag '7' only" 1 "$ROOT"; fi

echo "check-image-pins-selftest: REJECT cases — workflow"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    services:
      pg:
        image: postgres:16-alpine
EOF
red "workflow: a services image with a tag but no digest" "ci.yml:5: postgres:16-alpine — no digest (tag" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    container:
      image: node:latest
EOF
red "workflow: container image :latest" "ci.yml:4: node:latest — tag is :latest" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    container: node:20
    steps: []
EOF
red "workflow: the string form 'container: node:20' without a digest" "ci.yml:3: node:20 — no digest (tag" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    steps:
      - uses: docker://alpine:3.20
EOF
red "workflow: 'uses: docker://alpine:3.20' without a digest" "ci.yml:4: docker://alpine:3.20 — no digest (tag" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    steps:
      - uses: "docker://alpine"
EOF
red "workflow: a quoted 'uses: docker://alpine' with no tag" "ci.yml:4: docker://alpine — no tag and no digest" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<'EOF'
jobs:
  j:
    services:
      pg:
        image: ${{ matrix.image }}
EOF
red "workflow: a \${{ matrix.image }} expression" 'ci.yml:5: ${{ matrix.image }}' 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    services:
      pg:
        image: postgres:16@sha256:abc
EOF
red "workflow: a truncated digest" "ci.yml:5: postgres:16@sha256:abc — malformed digest" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    services:
      pg:
        image: postgres:16@sha256:$D
      redis:
        image: redis:7
EOF
red "workflow: the SECOND of two services is unpinned (sibling after a pinned one)" "ci.yml:7: redis:7 — no digest (tag" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  a:
    steps:
      - uses: x/y@v1
        with:
          image: ghcr.io/me/build:latest
  b:
    services:
      pg:
        image: postgres:16
EOF
red "workflow: after an ignored with: image, a later job's service is still caught" "ci.yml:10: postgres:16 — no digest (tag" 1 "$ROOT"
fx; putf .github/workflows/ci.yml <<EOF
jobs:
  j:
    container:
      image: node:20@sha256:$D
    services:
      pg:
        image: postgres:16@sha256:$D
    steps:
      - uses: docker://alpine:3.20
EOF
red "workflow: only the one unpinned docker:// ref among pinned ones is flagged" "ci.yml:9: docker://alpine:3.20" 1 "$ROOT"
fx; put .github/workflows/ci.yml "jobs:\n  j:\n    services:\n      pg:\n        image: postgres:16\r\n"
if crlf_file .github/workflows/ci.yml; then                       red "CRLF workflow: an unpinned service image is still caught" "ci.yml:5: postgres:16 — no digest (tag '16' only" 1 "$ROOT"; fi
fx; put Dockerfile "FROM $PIN\n"; put docker-compose.yml "services:\n  a:\n    image: redis:7\n"; put .github/workflows/ci.yml "jobs:\n  j:\n    container: node:20\n"
                                                                   red "mixed tree: a pinned Dockerfile, a bad compose AND a bad workflow = 2 problems" "docker-compose.yml:3: redis:7" 2 "$ROOT"

echo "check-image-pins-selftest: REJECT cases — the empty subject (--min-refs)"
fx;                                                                red "an EMPTY directory (default --min-refs 1)" "the instrument matched nothing" 1 "$ROOT"
fx;                                                                red "an empty directory, --min-refs 1 given explicitly" "found 0 image reference(s), fewer than --min-refs 1" 1 --min-refs 1 "$ROOT"
fx; put docker-compose.yml "services:\n  a:\n    environment: [X=1]\n"; red "a compose file with no image: line" "found 0 image reference(s)" 1 "$ROOT"
fx; put node_modules/x/Dockerfile "FROM alpine:latest\n"; put Docs/history/Dockerfile "FROM alpine:latest\n"
                                                                   red "only files in skipped directories: nothing found" "the instrument matched nothing" 1 "$ROOT"
fx; put README.md "image: foo:latest\n"; put script.sh "docker run alpine:latest\n"
                                                                   red "only docs and a shell script (not scanned)" "the instrument matched nothing" 1 "$ROOT"
fx; put Dockerfile "FROM $PIN\n"; put docker-compose.yml "services:\n  a:\n    image: redis:7@sha256:$D\n"
                                                                   red "--min-refs 3 with only 2 references" "found 2 image reference(s), fewer than --min-refs 3" 1 --min-refs 3 "$ROOT"
fx; put Dockerfile "FROM scratch\n";                               red "--min-refs 2 with a single FROM scratch" "found 1 image reference(s), fewer than --min-refs 2" 1 --min-refs 2 "$ROOT"
fx; put Dockerfile "FROM scratch\n";                               good "FROM scratch alone satisfies the default --min-refs 1 (it is exempt, and was found)" "$ROOT"

echo "check-image-pins-selftest: REJECT cases — roots"
fx; put Dockerfile "FROM $PIN\n";                                  red "a root that does not exist" "root does not exist: $ROOT/nope" 1 "$ROOT" "$ROOT/nope"
fx; put Dockerfile "FROM $PIN\n"; put notes.txt "x\n";             red "a root FILE with an unrecognised name" "is not a recognised Dockerfile / compose / workflow name" 1 "$ROOT" "$ROOT/notes.txt"

echo "check-image-pins-selftest: usage errors"
usage_case "no roots" "usage:"
usage_case "--min-refs 0 (a zero floor would let an empty tree pass)" "must be a whole number >= 1" --min-refs 0 "$ROOT"
usage_case "--min-refs abc" "must be a whole number >= 1" --min-refs abc "$ROOT"
usage_case "--min-refs with no value" "usage:" --min-refs
usage_case "an unknown option" "unknown option: --bogus" --bogus "$ROOT"

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------
# Inside a subshell (so its counters cannot leak into ours): a GREEN tree claimed to be red for a reason it does not have, and a
# RED tree (an unpinned image) claimed to be green. The harness must report BOTH as mismatches.
echo "check-image-pins-selftest: control"
ctl="$( (
  cases=0; okc=0; bad=0
  { fx; put Dockerfile "FROM $PIN\n"; red "wrong-reason" "no digest (tag" 1 "$ROOT"; } >/dev/null
  { fx; put Dockerfile "FROM alpine:3.20\n"; good "wrong-verdict" "$ROOT"; } >/dev/null
  echo "CONTROL cases=$cases ok=$okc bad=$bad"
) 2>&1 )"
if [ "$(count_in 'CONTROL cases=2 ok=0 bad=2' "$ctl")" -ge 1 ]; then
  record_ok "control: the harness reports a wrong reason and a wrong verdict as mismatches ($ctl)"
else
  record_bad "control" "the harness did NOT flag deliberately wrong expectations: $ctl"
fi

echo "selftest: $cases cases, $okc behaved as expected"
if [ "$cases" -lt "$MIN_CASES" ]; then
  echo "selftest: RED — only $cases cases ran, the hard floor is $MIN_CASES (were cases deleted?)"; exit 1
fi
if [ "$bad" -ne 0 ] || [ "$okc" -ne "$cases" ]; then
  echo "selftest: RED — $bad case(s) did NOT behave as expected"; exit 1
fi
exit 0
