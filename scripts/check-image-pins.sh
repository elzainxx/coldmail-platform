#!/usr/bin/env bash
# check-image-pins — BLOCKING CI gate: EVERY container image reference in the tree carries a DIGEST.
#
#   check-image-pins.sh [--min-refs N] <root> [<root>...]       (roots are directories or single files; N >= 1, default 1)
#
# THE DEFECT CLASS (false-green class 20 b): a tag is a moving target. A CI runner pulls cold and gets whatever the registry
# serves that day, while a developer's machine keeps an old cached copy for months. `ruby:3.2.0-slim` "worked" for months and
# could not be built from scratch at all; `alpine:3` sat in a tooling script while the per-repo gate said OK because it only
# read the files it was first written for. A gate whose reading is narrower than the claim it prints is class 19.
#
# WHAT IT SCANS (under each root; node_modules, .git and Docs/history are skipped, and the number skipped is printed):
#   * Dockerfile*, dockerfile*, *.dockerfile, *.Dockerfile   -> every `FROM` line (flags such as --platform=... skipped,
#                                                                continuation lines joined, `AS name` recorded, FROM is case-insensitive)
#   * docker-compose*.yml|yaml, compose*.yml|yaml           -> every `image:` value (quoted or not; `# comment` ignored)
#   * .github/workflows/*.yml|yaml                          -> `image:` under a `services:` or `container:` ancestor, the string
#                                                                form `container: <image>` of a job, and every `docker://<ref>`
#                                                                (so `uses: docker://...` too). An `image:` anywhere else in a
#                                                                workflow (e.g. a `with:` input that NAMES an image to build) is
#                                                                counted as IGNORED, not treated as a pull. `run: |` block text is
#                                                                not read.
# THE RULES. A reference is GREEN only if it is  name[:tag]@sha256:<64 lowercase hex>.  RED, each with its own wording:
#   * interpolated: `${...}`, `$VAR`, `${{ ... }}` cannot be read statically — red, NEVER skipped
#   * malformed digest: not exactly `sha256:` + 64 lowercase hex (truncated, uppercase, sha512:, empty after `@`)
#   * tag `latest` — red EVEN WHEN a digest is present: the digest is what is pulled, so the `latest` label would lie as soon
#     as the tag moves; this follows the sender repos' image_pins gate. (Decision of this script; the brief's GREEN rule alone
#     would have allowed `name:latest@sha256:...`.)
#   * no tag AND no digest (implicitly :latest);   * a tag but no digest (the tag moves)
#   * empty value, or whitespace inside the reference
# EXEMPT (counted, not red): `FROM scratch`; `FROM <alias>` where <alias> was defined by an EARLIER `FROM ... AS <alias>` in the
# same file (case-insensitive; an alias used before it is defined is NOT exempt).
# A Dockerfile with no FROM line at all is red (the instrument matched nothing in that file); a compose file or workflow with no
# image reference is fine (override files legitimately have none).
# --min-refs N: red if fewer than N references (pinned + exempt + red) were found IN TOTAL — "the instrument matched nothing"
# (class 8/18). An empty tree can never be a green.
#
# WHAT IT DOES NOT CHECK — also printed on EVERY run as the `NOT CHECKED:` line:
#   * images pulled by `docker run|pull|build` inside shell scripts and workflow `run:` blocks
#   * images named only in comments, docs, READMEs; `COPY --from=<image>` / `RUN --mount=from=<image>`
#   * `uses: owner/action@ref` pins (a different gate), Kubernetes/Helm manifests, flow-style YAML (`container: {image: ...}`),
#     YAML anchors/aliases
#   * whether a pinned digest still exists upstream or still belongs to the tag written beside it
# Every input line has `\r` stripped explicitly (Windows checkout; on the author's host awk and sed hide it, bash `read` does not).
# Paired control: scripts/check-image-pins-selftest.sh. Exit status: 0 green, 1 red, 2 usage error. Needs bash >= 4, find, awk, tr, sort.
set -u -o pipefail

usage() { echo "usage: check-image-pins.sh [--min-refs N] <root> [<root>...]" >&2; exit 2; }
MIN_REFS=1; roots=()
while [ $# -gt 0 ]; do
  case "$1" in
    --min-refs)   [ $# -ge 2 ] || usage; MIN_REFS="$2"; shift 2;;
    --min-refs=*) MIN_REFS="${1#*=}"; shift;;
    -h|--help)    usage;;
    --)           shift; while [ $# -gt 0 ]; do roots+=("$1"); shift; done;;
    -*)           echo "check-image-pins: unknown option: $1" >&2; usage;;
    *)            roots+=("$1"); shift;;
  esac
done
if ! [[ "$MIN_REFS" =~ ^[0-9]+$ ]] || [ "$((10#$MIN_REFS))" -lt 1 ]; then
  echo "check-image-pins: --min-refs must be a whole number >= 1 (got '$MIN_REFS'): a floor of 0 would let an empty tree pass" >&2; usage
fi
MIN_REFS=$((10#$MIN_REFS))
[ ${#roots[@]} -gt 0 ] || usage

# ---- extraction (awk): one record per reference, `<line> TAB <kind> TAB <ref>` --------------------------------------
# kinds: REF (must be checked) | SCRATCH, ALIAS (exempt) | IGNORED (a workflow `image:` outside services/container)
AWK_EXTRACT='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
function yval(v,    c, i) {                       # a YAML scalar: unquote, or cut a trailing comment
  v = trim(v); c = substr(v, 1, 1)
  if (c == "\"" || c == sq) { v = substr(v, 2); i = index(v, c); if (i > 0) v = substr(v, 1, i - 1); return v }
  if (c == "#") return ""
  sub(/[ \t]+#.*$/, "", v); return trim(v)
}
function from_line(L, ln,    n, t, i, img, stg, kind) {
  L = trim(L)
  n = split(L, t, /[ \t]+/)
  if (tolower(t[1]) != "from") return
  i = 2
  while (i <= n && substr(t[i], 1, 2) == "--") i++
  img = (i <= n) ? t[i] : ""
  stg = ""
  if (i + 2 <= n && tolower(t[i + 1]) == "as") stg = tolower(t[i + 2])
  if (img == "scratch") kind = "SCRATCH"
  else if (img != "" && (tolower(img) in stages)) kind = "ALIAS"
  else kind = "REF"
  printf "%d\t%s\t%s\n", ln, kind, img
  if (stg != "") stages[stg] = 1
}
BEGIN { sq = sprintf("%c", 39); depth = 0; cont = 0; inblock = 0 }
{ sub(/\r$/, "") }

mode == "docker" {
  if (trim($0) ~ /^#/) next                      # a comment line, also inside a continuation
  if (cont == 0) { bl = NR; buf = "" }
  line = $0
  if (line ~ /\\[ \t]*$/) { sub(/\\[ \t]*$/, "", line); buf = buf " " line; cont = 1; next }
  buf = buf " " line; cont = 0
  from_line(buf, bl); buf = ""
  next
}

mode == "compose" {
  if (trim($0) ~ /^#/) next
  t0 = $0
  if (t0 ~ /^[ \t]*(-[ \t]+)?image:([ \t]|$)/) { sub(/^[ \t]*(-[ \t]+)?image:/, "", t0); printf "%d\tREF\t%s\n", NR, yval(t0) }
  next
}

mode == "workflow" {
  line = $0
  if (line ~ /^[ \t]*$/ || line ~ /^[ \t]*#/) next
  match(line, /^ */); ind = RLENGTH
  if (inblock) { if (ind > blockind) next; inblock = 0 }      # the body of a `run: |` block scalar is shell text, not YAML
  body = substr(line, ind + 1); rest = body; keycol = ind
  if (body ~ /^-[ \t]+/) { sub(/^-[ \t]+/, "", rest); keycol = ind + (length(body) - length(rest)) }
  code = line; sub(/[ \t]+#.*$/, "", code)
  p = index(code, "docker://")                                # every docker:// reference on the line (uses: docker://...)
  while (p > 0) {
    tail = substr(code, p + 9); match(tail, /^[^ \t"\047,]*/); ref = substr(tail, 1, RLENGTH)
    printf "%d\tREF\tdocker://%s\n", NR, ref
    code = substr(tail, RLENGTH + 1); p = index(code, "docker://")
  }
  if (rest ~ /^[A-Za-z0-9_."\047\/-]+[ \t]*:([ \t]|$)/) {    # a `key: value` line: maintain the ancestor stack by indentation
    key = rest; sub(/[ \t]*:([ \t].*)?$/, "", key); gsub(/["\047]/, "", key)
    val = rest; sub(/^[A-Za-z0-9_."\047\/-]+[ \t]*:/, "", val)
    while (depth > 0 && sind[depth] >= keycol) depth--
    v = yval(val)
    if (key == "image") {
      ok = 0
      for (i = 1; i <= depth; i++) if (skey[i] == "services" || skey[i] == "container") ok = 1
      printf "%d\t%s\t%s\n", NR, (ok ? "REF" : "IGNORED"), v
    } else if (key == "container" && v != "" && depth == 2 && skey[1] == "jobs") {
      printf "%d\tREF\t%s\n", NR, v                          # the string form:  container: node:20
    }
    depth++; sind[depth] = keycol; skey[depth] = key
    if (trim(val) ~ /^[|>][-+0-9]*([ \t]+#.*)?$/) { inblock = 1; blockind = keycol }
  }
  next
}
END { if (mode == "docker" && cont == 1) from_line(buf, bl) }
'
extract() { tr -d '\r' < "$2" | awk -v mode="$1" "$AWK_EXTRACT"; }

# ---- classification: sets REASON ("" = green) --------------------------------------------------------------------------
DIGEST_RE='^sha256:[0123456789abcdef]{64}$'   # explicit set: a [a-f] range can match uppercase in some locales
classify() {
  local ref="$1" name digest="" has_at=0 last tag=""
  REASON=""
  ref="${ref#docker://}"
  if [ -z "$ref" ]; then REASON="empty image reference"; return; fi
  case "$ref" in
    *'$'*|*'{{'*|*'`'*) REASON='interpolated reference ($VAR, ${...} or ${{ ... }}) cannot be read statically — write the literal name@sha256:<digest>'; return;;
    *[[:space:]]*)      REASON="reference contains whitespace"; return;;
  esac
  if [[ "$ref" == *@* ]]; then has_at=1; name="${ref%%@*}"; digest="${ref#*@}"; else name="$ref"; fi
  if [ -z "$name" ]; then REASON="image name is empty before the digest"; return; fi
  last="${name##*/}"                                   # a registry port (host:5000/img) is not a tag: look at the last segment only
  case "$last" in *:*) tag="${last#*:}";; esac
  if [ "$has_at" -eq 1 ] && ! [[ "$digest" =~ $DIGEST_RE ]]; then
    REASON="malformed digest '$digest' (want sha256: followed by exactly 64 lowercase hex characters)"; return
  fi
  if [ "$tag" = latest ]; then REASON="tag is :latest — a moving target, even beside a digest the label would lie once the tag moves"; return; fi
  [ "$has_at" -eq 1 ] && return                        # name[:tag]@sha256:<64 hex>
  if [ -z "$tag" ]; then REASON="no tag and no digest (implicitly :latest)"; return; fi
  REASON="no digest (tag '$tag' only — a tag moves)"
}

# ---- discovery -----------------------------------------------------------------------------------------------------------
problems=0
problem() { problems=$((problems + 1)); echo "RED: $*"; }
list=(); declare -A SEEN=()
n_hist=0
FTYPE=""
classify_file() {   # sets FTYPE for a path, "" if not a file this gate reads
  local f="$1" b="${1##*/}"; FTYPE=""
  case "/$f" in */.github/workflows/*.yml|*/.github/workflows/*.yaml) FTYPE=workflow; return;; esac
  case "$b" in *.dockerignore|*.md|*.txt) return;; esac
  case "$b" in
    Dockerfile*|dockerfile*|*.dockerfile|*.Dockerfile) FTYPE=docker;;
    docker-compose*.yml|docker-compose*.yaml|compose*.yml|compose*.yaml) FTYPE=compose;;
  esac
}
add_file() {        # add_file <path> <explicit: 1 if the user named this file as a root>
  classify_file "$1"
  if [ -z "$FTYPE" ]; then
    [ "$2" -eq 0 ] || problem "root file $1 is not a recognised Dockerfile / compose / workflow name (Dockerfile*, *.dockerfile, docker-compose*.yml, compose*.yml, .github/workflows/*.yml)"
    return
  fi
  if [ "$2" -eq 0 ]; then case "/$1/" in */Docs/history/*) n_hist=$((n_hist + 1)); return;; esac; fi
  [ -z "${SEEN[$1]:-}" ] || return
  SEEN[$1]=1; list+=("$FTYPE"$'\t'"$1")
}
for root in "${roots[@]}"; do
  if [ -f "$root" ]; then add_file "$root" 1
  elif [ -d "$root" ]; then
    while IFS= read -r -d '' f; do add_file "$f" 0; done < <(
      find "$root" \( -name node_modules -o -name .git \) -prune -o -type f \( \
          -name 'Dockerfile*' -o -name 'dockerfile*' -o -name '*.dockerfile' -o -name '*.Dockerfile' \
          -o -name 'docker-compose*.yml' -o -name 'docker-compose*.yaml' -o -name 'compose*.yml' -o -name 'compose*.yaml' \
          -o -path '*/.github/workflows/*.yml' -o -path '*/.github/workflows/*.yaml' -o -path '.github/workflows/*.yml' -o -path '.github/workflows/*.yaml' \
        \) -print0 | LC_ALL=C sort -z)
  else problem "root does not exist: $root"
  fi
done

# ---- scan ------------------------------------------------------------------------------------------------------------------
n_docker=0; n_compose=0; n_workflow=0
pinned=0; exempt=0; redrefs=0; ignored=0
for entry in ${list[@]+"${list[@]}"}; do
  t="${entry%%$'\t'*}"; f="${entry#*$'\t'}"
  case "$t" in docker) n_docker=$((n_docker + 1));; compose) n_compose=$((n_compose + 1));; workflow) n_workflow=$((n_workflow + 1));; esac
  fr=0; fp=0; fe=0; fred=0
  while IFS=$'\t' read -r ln kind ref; do
    case "$kind" in
      SCRATCH|ALIAS) exempt=$((exempt + 1)); fe=$((fe + 1)); fr=$((fr + 1));;
      IGNORED)       ignored=$((ignored + 1));;
      REF)
        fr=$((fr + 1)); classify "$ref"
        if [ -z "$REASON" ]; then pinned=$((pinned + 1)); fp=$((fp + 1))
        else redrefs=$((redrefs + 1)); fred=$((fred + 1)); problems=$((problems + 1)); echo "$f:$ln: ${ref:-<empty>} — $REASON"; fi;;
    esac
  done < <(extract "$t" "$f")
  printf '  %-9s %s  refs=%s pinned=%s exempt=%s red=%s\n' "$t" "$f" "$fr" "$fp" "$fe" "$fred"
  if [ "$t" = docker ] && [ "$fr" -eq 0 ]; then problem "$f has no FROM line — the instrument matched nothing in this Dockerfile"; fi
done

found=$((pinned + exempt + redrefs))
if [ "$found" -lt "$MIN_REFS" ]; then
  problem "found $found image reference(s), fewer than --min-refs $MIN_REFS — the instrument matched nothing (scanned $n_docker Dockerfile(s), $n_compose compose file(s), $n_workflow workflow file(s))"
fi

echo "check-image-pins: scanned $n_docker Dockerfile(s), $n_compose compose file(s), $n_workflow workflow file(s) under: ${roots[*]}"
echo "check-image-pins: image references: found=$found pinned=$pinned exempt=$exempt red=$redrefs (min-refs $MIN_REFS)"
echo "NOT CHECKED: images pulled by 'docker run|pull|build' in shell scripts and workflow run: blocks; images named only in comments or docs; COPY --from=<image> / --mount=from=<image>;" \
     "'uses: owner/action@ref' pins; Kubernetes/Helm manifests; flow-style YAML and YAML anchors; whether a pinned digest still exists upstream or still matches its tag;" \
     "$ignored workflow 'image:' key(s) outside services/container were not treated as pulls; $n_hist matching file(s) under Docs/history were skipped"
if [ "$problems" -eq 0 ]; then
  echo "check-image-pins: ok — $found image reference(s), all pinned by digest or exempt (scratch / earlier stage alias)"
  exit 0
fi
echo "check-image-pins: RED ($problems problem(s))"
exit 1
