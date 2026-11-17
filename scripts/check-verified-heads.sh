#!/usr/bin/env bash
# check-verified-heads.sh [--root DIR] [--api-cmd CMD] — BLOCKING gate: a head is called verified only when its workflows ran green on that exact sha and the evidence row cites that run.
#
# WHY IT EXISTS. The status tables say "done" and "CLOSED" in prose, and prose does not know which commit a green run belonged to. Three times a row has been green for the wrong head: a run for an older sha of the
# same repo, a run that was later re-run into failure, a head that was pushed and never ran. The owner's rule (CLAUDE.md rule 23): a head is called verified only when the heavy workflows ran green on that exact
# sha AND every evidence row cites that run. This gate asks the GitHub API, for every claim in the listed documents, whether that is true. Local runs are rehearsal; the runner is the citation.
#
# THE DOCUMENTS (tests/verified-heads-docs.txt: one path relative to the root per line, '#' starts a comment line; a list that names no document is red, and so is a list that drops the build plan):
#   Docs/coldmail-platform-build-plan.md   the phase status table   | Phase | Status |                       the status cell is also the evidence cell
#   Docs/phase-2-closeout-spec.md          the item tables          | # | Item | Exit check | Status | Evidence |   the last two cells are Status and Evidence
# A TABLE is read when its header's last cell is `Status`, or its last two cells are `Status` and `Evidence` (case-insensitive, markdown emphasis ignored); cells split on | but not on \| ; fenced code is skipped;
# any other table is ignored (and counted). The ROW ID is the first cell (P2 Money core, E4, ...): every violation is printed as `<file>:<line> <row id>: <reason>`.
#
# WHAT IS A CLAIM (checked on the status cell, case-insensitively; claim first, so a row that says both `done` and `IN PROGRESS` is a claim):
#   a) the status cell starts with `**done**`, or contains `done —` (done, a space, an em dash);        b) the status cell, emphasis stripped, is exactly `CLOSED`.
# NOT A CLAIM (skipped, counted and printed): a status that starts with IN PROGRESS, OPEN, BLOCKED, NOT STARTED, PARKED, DEFERRED, PENDING, OWNER ACTION. Any other status is RED ("cannot be classified"),
# including `CLOSED (runner pending)` and a bare `done`: a status that is neither a claim nor a named non-claim is never guessed.
#
# THE TOKENS (the evidence text, after removing URLs, backticks and asterisks; lowercase hex only, 7 to 40 characters, not followed by a letter or digit):
#   HEAD  <repo word> [ main ] ( @ | whitespace ) <sha>     platform@8fbae10   platform main @ a6709c9   verify@e32c1b9   send-managed@4ea078b   send-byo@238bd0a   module 1 e32c1b9   platform 8fbae10+
#         <repo word> = platform | verify | send-managed | send-byo (each also as coldmail-<name>) | module 1 | module 2 | module 3     (module 1 = verify, 2 = send-managed, 3 = send-byo)
#         `main @ <sha>` with no repo word is the platform;  `green on <sha>` is the platform (the document's own repository).
#         A sha with no repo word before it (`Part A e5c58f4`, `ancestor 7740aa8`) is NOT a cited head: it is how a row mentions a commit it does not claim. Hash-like strings left unread are printed.
#   RUN   https://github.com/elzainxx/<repo>/actions/runs/<id>[/attempts/<n>]       <repo> is coldmail-platform, coldmail-verify, coldmail-send-managed or coldmail-send-byo; any other repo or owner is red.
#
# WHAT IT CHECKS, for every claim (the GitHub API through --api-cmd, default `gh`, which reads GH_TOKEN; the token must be able to read Actions on all four repositories: the workflow's own GITHUB_TOKEN reads only the platform):
#   1. the row cites at least one head token AND at least one run URL: "a claim must cite the head and the run";
#   2. every cited head resolves (`api repos/elzainxx/<repo>/commits/<sha> --jq .sha`) to a full sha that starts with the cited hex;
#   3. every cited head has a successful run of EVERY required workflow on that exact full sha (`api repos/elzainxx/<repo>/actions/runs?head_sha=<full>&per_page=100`): the platform needs harness, db-tests and supabase
#      (the heavy ones) and, because a claim is done/CLOSED, also go, gates, gitleaks, web, platform-check; verify needs ci and gitleaks; send-managed and send-byo need real-infra and gitleaks.
#      A cited run that is green for its own attempt also satisfies its workflow (a /attempts/1 run that was green and was re-run into failure is the owner's real case: the cited attempt is what the row calls evidence).
#      A required workflow whose file .github/workflows/<name>.yml did not exist AT THAT SHA (`api repos/…/contents/.github/workflows/<name>.yml?ref=<full>` is a 404) is waived: a head cannot have run what its tree did not
#      have (P0's head predates db-tests and supabase). The waiver is printed per row, counted in NOT CHECKED, and never covers the first workflow of a set (harness, ci, real-infra): a deleted harness is not a green one;
#   4. every cited run exists (a 404 is red), is completed with conclusion success (a /attempts/<n> URL uses THAT attempt's conclusion, a plain URL the run's latest), and is for one of the row's cited full shas of
#      the same repo ("run 123 is for sha X, not for the head the row claims");
#   5. every cited head has at least one cited run that is for it (the row must cite the run, not only the head).
# FLOORS (a gate whose subject is empty is red, classes 8 and 18): an empty or missing list, a listed document that is missing or empty, a document with 0 tables or 0 rows parsed, 0 claims overall, 0 claims in a
# document whose status cells mention done/closed/green/verified/complete outside the claim forms ("the instrument matched nothing"), an API answer whose shape the gate cannot parse (the API said N runs, 0 parsed),
# and an API that cannot be reached (no token, a rate limit, no network): that is RED with its reason, never green.
#
# Output: `claims: N, heads checked: M, runs checked: K`, the rows parsed and skipped, one line per problem, and a `NOT CHECKED:` line. Exit status: 0 green, 1 red, 2 usage error.
# Paired control: scripts/check-verified-heads-selftest.sh (a stub api command answers from fixtures; the selftest never touches the network; every reject case asserts the reason and the exit code).
# Wiring (the `gates` workflow): `bash scripts/check-verified-heads.sh` with GH_TOKEN set, and `bash scripts/check-verified-heads-selftest.sh`.
#
# NOT CHECKED (also printed on every run, with its counts): that a cited run's logs are the right logs (only its workflow name, status, conclusion and head sha are read); that the sha a human typed is the sha they
# meant (a typo that is another real green commit passes); documents not in the list; claims phrased outside the table formats (a status that mentions done/closed/green outside the claim forms, a sha written without a
# repo word before it, a run named without its URL); that no job was skipped inside a green run; workflows other than the required sets.
set -u
export LC_ALL=C

usage() { echo "usage: check-verified-heads.sh [--root DIR] [--api-cmd CMD]   (the default api command is gh: set GH_TOKEN)" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_IN=""; API_CMD="gh"
while [ $# -gt 0 ]; do
  case "$1" in
    --root)       [ $# -ge 2 ] || usage; ROOT_IN="$2"; shift 2;;
    --root=*)     ROOT_IN="${1#*=}"; shift;;
    --api-cmd)    [ $# -ge 2 ] || usage; API_CMD="$2"; shift 2;;
    --api-cmd=*)  API_CMD="${1#*=}"; shift;;
    -h|--help)    usage;;
    *) echo "check-verified-heads: unknown argument '$1'" >&2; usage;;
  esac
done

OWNER="elzainxx"
LIST_REL="tests/verified-heads-docs.txt"; PLAN_REL="Docs/coldmail-platform-build-plan.md"
REPOS="coldmail-platform coldmail-verify coldmail-send-managed coldmail-send-byo"
HEAVY_PLATFORM="harness db-tests supabase"                      # the heavy workflows: the claim is red without them
CLAIM_PLATFORM="go gates gitleaks web platform-check"           # a claim (done / CLOSED) also needs these
REQ_VERIFY="ci gitleaks"; REQ_MANAGED="real-infra gitleaks"; REQ_BYO="real-infra gitleaks"
EMDASH=$'\xe2\x80\x94'
# the jq the API call asks for (gh evaluates it itself, so no jq binary is needed): one tab-separated line per run; a null conclusion is "-" so no field is ever empty
RUN_JQ='[.id, .name, .status, (.conclusion // "-"), .head_sha, .run_attempt] | @tsv'
LIST_JQ='.total_count, (.workflow_runs[] | [.id, .name, .status, (.conclusion // "-"), .head_sha, .run_attempt] | @tsv)'
LIST_PAGE=100

# the token grammar (bash ERE; see THE TOKENS above). Group numbers: 2 = the repo word, 9 = the hex.
TOK1='(^|[^A-Za-z0-9_-])(((coldmail-)?(platform|verify|send-managed|send-byo))|(module[ ]+[123]))( main)?([ ]*@[ ]*|[ ]+)([0-9a-f]{7,40})([^0-9A-Za-z_]|$)'
TOK_MAIN='(^|[^A-Za-z0-9_-])main[ ]*@[ ]*([0-9a-f]{7,40})([^0-9A-Za-z_]|$)'
TOK_GREEN='(^|[^A-Za-z0-9_-])green on[ ]+([0-9a-f]{7,40})([^0-9A-Za-z_]|$)'
TOK_BARE='(^|[^0-9A-Za-z_])([0-9a-f]{7,40})([^0-9A-Za-z_]|$)'
URL_RE='https://github\.com/[^/ )>]+/[^/ )>]+/actions/runs/[0-9]+(/attempts/[0-9]+)?'
URL_PARTS='^https://github\.com/([^/]+)/([^/]+)/actions/runs/([0-9]+)(/attempts/([0-9]+))?$'

ROOT="$(cd "${ROOT_IN:-$HERE/..}" 2>/dev/null && pwd)" || { echo "check-verified-heads: RED — the root '$ROOT_IN' is not a directory: the instrument matched nothing"; exit 1; }
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT

fail=0; NBARE=0; NMENTION=0; NWAIVED=0
declare -A HEADSET=() RUNSET=() HSEEN=() RES_CACHE=() LST_STATE=() LST_OKS=() LST_SEEN=() LST_NOTE=() LST_TOTAL=() RUN_CACHE=() ABSENT_CACHE=()
CUR_DOC=""; CUR_LINE=""; CUR_ID=""
bad() { echo "  RED: $*"; fail=$((fail+1)); }
viol() { bad "$CUR_DOC:$CUR_LINE $CUR_ID: $*"; }
print_not_checked() {
  echo "NOT CHECKED: that a cited run's logs are the right logs (only its workflow name, status, conclusion and head sha are read); that the sha a human typed is the sha they meant (a typo that is another real green commit passes); documents not in $LIST_REL; claims phrased outside the table formats ($NMENTION status cell(s) mention done/closed/green outside the claim forms, $NBARE hash-like string(s) in claim rows had no repo word before them, a run named without its URL); that no job was skipped inside a green run; workflows other than the required sets; $NWAIVED required workflow(s) were waived because .github/workflows/<name>.yml did not exist at that sha (a head cannot run a workflow it does not have; the first workflow of a set is never waived)"
}

# ---- the API: one function, so that an unreachable API is one decision --------------------------------------------------------------------------------------------------------------------------
API=()
if [ -f "$API_CMD" ]; then if [ -x "$API_CMD" ]; then API=("$API_CMD"); else API=(bash "$API_CMD"); fi; else read -r -a API <<< "$API_CMD"; fi
if [ "${#API[@]}" -eq 0 ] || { ! command -v "${API[0]}" > /dev/null 2>&1 && [ ! -f "${API[0]}" ]; }; then
  echo "check-verified-heads: RED — the api command '$API_CMD' was not found: the instrument cannot run, and a gate that cannot ask proves nothing"; print_not_checked; exit 1
fi
A_OUT=""; A_ERR=""; A_RC=0
die_api() { # die_api ENDPOINT — the call failed and it was not a 404 / 422: no token, a rate limit, no network, a server error
  echo "check-verified-heads: RED — the GitHub API is unreachable or refused the request (no token, a rate limit, no network or a server error): $A_ERR"
  echo "  the call was: api $1"
  echo "  a gate that cannot reach the API proves nothing and is never green: set GH_TOKEN (a token that can read Actions on $REPOS) and run again"
  print_not_checked; exit 1
}
api_call() { # api_call ENDPOINT JQ — A_RC: 0 ok (A_OUT), 4 not found (HTTP 404), 5 unprocessable (HTTP 422: gh's answer for a sha that is no commit); anything else ends the run
  local out rc
  out="$("${API[@]}" api "$1" --jq "$2" 2> "$T/err" < /dev/null)"; rc=$?      # stdin closed: the claims are being read from a file by the caller's loop
  IFS= read -r -d '' A_ERR < "$T/err"; A_ERR="${A_ERR//$'\r'/}"; A_ERR="${A_ERR//$'\n'/ }"; A_ERR="${A_ERR:0:300}"
  A_OUT="${out//$'\r'/}"
  if [ "$rc" -eq 0 ]; then A_RC=0; return; fi
  case "$A_ERR $A_OUT" in
    *"HTTP 404"*) A_RC=4;;
    *"HTTP 422"*) A_RC=5;;
    *) die_api "$1";;
  esac
}

# ---- the tokens ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
REPO_OF=""
repo_of_word() { # repo_of_word WORD -> REPO_OF: the repository, or empty
  REPO_OF=""
  case "$1" in
    platform|coldmail-platform) REPO_OF=coldmail-platform;;
    verify|coldmail-verify) REPO_OF=coldmail-verify;;
    send-managed|coldmail-send-managed) REPO_OF=coldmail-send-managed;;
    send-byo|coldmail-send-byo) REPO_OF=coldmail-send-byo;;
    module*) case "${1: -1}" in 1) REPO_OF=coldmail-verify;; 2) REPO_OF=coldmail-send-managed;; 3) REPO_OF=coldmail-send-byo;; esac;;
  esac
}
HEAD_REPO=(); HEAD_SHA=(); BARE=""
add_head() { local k="$1 $2"; [ -n "${HSEEN[$k]:-}" ] && return; HSEEN[$k]=1; HEAD_REPO+=("$1"); HEAD_SHA+=("$2"); }
extract_heads() { # extract_heads TEXT — fills HEAD_REPO[] / HEAD_SHA[] (distinct pairs, in order) and BARE (hash-like strings that were not read as heads)
  local t rest left="" matched pre word hex repo
  HEAD_REPO=(); HEAD_SHA=(); BARE=""; HSEEN=()
  t="$(printf '%s' "$1" | sed -E 's#https?://[^][ )>]*# #g; s/[`*]//g')"
  rest="$t"
  while [[ $rest =~ $TOK1 ]]; do                      # pass 1: <repo word> [main] [@] <sha>
    matched="${BASH_REMATCH[0]}"; word="${BASH_REMATCH[2]}"; hex="${BASH_REMATCH[9]}"
    pre="${rest%%"$matched"*}"; left+="$pre "; rest="${rest#*"$matched"}"
    repo_of_word "$word"; [ -z "$REPO_OF" ] || add_head "$REPO_OF" "$hex"
  done
  left+="$rest"; rest="$left"; left=""
  while [[ $rest =~ $TOK_MAIN ]]; do                  # pass 2: main @ <sha> (no repo word: the platform)
    matched="${BASH_REMATCH[0]}"; hex="${BASH_REMATCH[2]}"
    pre="${rest%%"$matched"*}"; left+="$pre "; rest="${rest#*"$matched"}"
    add_head coldmail-platform "$hex"
  done
  left+="$rest"; rest="$left"; left=""
  while [[ $rest =~ $TOK_GREEN ]]; do                 # pass 3: green on <sha> (the platform)
    matched="${BASH_REMATCH[0]}"; hex="${BASH_REMATCH[2]}"
    pre="${rest%%"$matched"*}"; left+="$pre "; rest="${rest#*"$matched"}"
    add_head coldmail-platform "$hex"
  done
  left+="$rest"; rest="$left"
  while [[ $rest =~ $TOK_BARE ]]; do                  # what is left: a hash-like string (a digit and a letter a-f) that had no repo word before it
    matched="${BASH_REMATCH[0]}"; hex="${BASH_REMATCH[2]}"
    rest="${rest#*"$matched"}"
    if [[ $hex == *[0-9]* && $hex == *[a-f]* ]]; then BARE+="$hex "; fi
  done
}
RUN_REPO=(); RUN_ID=(); RUN_ATT=(); RUN_URL=()
extract_runs() { # extract_runs TEXT — fills RUN_*[] (distinct URLs); an owner or repo outside the four is red here
  local u seen=" "
  RUN_REPO=(); RUN_ID=(); RUN_ATT=(); RUN_URL=()
  while IFS= read -r u; do
    [ -n "$u" ] || continue
    case "$seen" in *" $u "*) continue;; esac
    seen+="$u "
    if [[ $u =~ $URL_PARTS ]]; then
      if [ "${BASH_REMATCH[1]}" != "$OWNER" ] || [[ " $REPOS " != *" ${BASH_REMATCH[2]} "* ]]; then
        viol "the run URL $u is not in one of the four repositories ($OWNER/{${REPOS// /,}}): a run elsewhere is no evidence for these heads"
        continue
      fi
      RUN_REPO+=("${BASH_REMATCH[2]}"); RUN_ID+=("${BASH_REMATCH[3]}"); RUN_ATT+=("${BASH_REMATCH[5]:-}"); RUN_URL+=("$u")
    fi
  done < <(printf '%s' "$1" | grep -oE "$URL_RE")
}

# ---- what the API says about a head and a run ----------------------------------------------------------------------------------------------------------------------------------------------------
R_ST=""; R_VAL=""
resolve_head() { # resolve_head REPO SHORT — R_ST: ok (R_VAL = the full sha) | nf (R_VAL = the HTTP code) | bad (R_VAL = what came back)
  local k="$1 $2"
  if [ -z "${RES_CACHE[$k]:-}" ]; then
    api_call "repos/$OWNER/$1/commits/$2" .sha
    case "$A_RC" in
      0) if [[ $A_OUT =~ ^[0-9a-f]{40}$ ]] && [[ $A_OUT == "$2"* ]]; then RES_CACHE[$k]="ok|$A_OUT"; else RES_CACHE[$k]="bad|${A_OUT:0:80}"; fi;;
      4) RES_CACHE[$k]="nf|404";;
      5) RES_CACHE[$k]="nf|422";;
    esac
  fi
  R_ST="${RES_CACHE[$k]%%|*}"; R_VAL="${RES_CACHE[$k]#*|}"
}
fetch_list() { # fetch_list REPO FULL — the runs on one sha: LST_STATE[k] ok|nf|bad, LST_OKS[k] = " wf wf " (successful), LST_SEEN[k] = "wf=conclusion, ..."
  local k="$1 $2" total body want parsed=0 id name st conc rsha att oks=" " seen=""
  [ -z "${LST_STATE[$k]:-}" ] || return 0
  api_call "repos/$OWNER/$1/actions/runs?head_sha=$2&per_page=$LIST_PAGE" "$LIST_JQ"
  if [ "$A_RC" -ne 0 ]; then LST_STATE[$k]=nf; return 0; fi
  total="${A_OUT%%$'\n'*}"; body=""; case "$A_OUT" in *$'\n'*) body="${A_OUT#*$'\n'}";; esac
  if ! [[ $total =~ ^[0-9]+$ ]]; then LST_STATE[$k]=bad; LST_NOTE[$k]="the first line of the answer is '${total:0:60}', not a run count"; return 0; fi
  while IFS=$'\t' read -r id name st conc rsha att; do
    [[ $id =~ ^[0-9]+$ ]] && [ -n "$att" ] || continue     # a line that is not one run (six fields, a numeric id) is not counted: the total below then disagrees and the gate is red
    parsed=$((parsed+1)); name="${name// /_}"
    seen+="$name=$conc, "
    if [ "$st" = completed ] && [ "$conc" = success ] && [ "$rsha" = "$2" ]; then oks+="$name "; fi
  done <<< "$body"
  want="$total"; [ "$want" -le "$LIST_PAGE" ] || want="$LIST_PAGE"
  LST_TOTAL[$k]="$total"; LST_OKS[$k]="$oks"; LST_SEEN[$k]="${seen%, }"
  if [ "$parsed" -ne "$want" ]; then LST_STATE[$k]=bad; LST_NOTE[$k]="the API reported $total run(s) and the gate parsed $parsed of the $want it should have read: the instrument matched too little (the answer's shape changed?)"; return 0; fi
  LST_STATE[$k]=ok
}
F_ST=""; F_NOTE=""; F_NAME=""; F_STATUS=""; F_CONC=""; F_SHA=""; F_ATT=""
fetch_run() { # fetch_run REPO ID ATT — F_ST ok|nf|bad and F_NAME F_STATUS F_CONC F_SHA F_ATT (ATT empty: the run's latest attempt)
  local k="$1 $2 $3" ep="repos/$OWNER/$1/actions/runs/$2" f_id f_name f_st f_conc f_sha f_att
  [ -z "$3" ] || ep="$ep/attempts/$3"
  if [ -z "${RUN_CACHE[$k]:-}" ]; then
    api_call "$ep" "$RUN_JQ"
    case "$A_RC" in
      0) IFS=$'\t' read -r f_id f_name f_st f_conc f_sha f_att <<< "$A_OUT"
         if [ "$f_id" = "$2" ] && [ -n "$f_att" ]; then RUN_CACHE[$k]="ok"$'\037'"$f_id"$'\037'"$f_name"$'\037'"$f_st"$'\037'"$f_conc"$'\037'"$f_sha"$'\037'"$f_att"
         else RUN_CACHE[$k]="bad"$'\037'"${A_OUT:0:100}"; fi;;
      *) RUN_CACHE[$k]="nf"$'\037'"$A_RC";;
    esac
  fi
  IFS=$'\037' read -r F_ST F_NOTE F_NAME F_STATUS F_CONC F_SHA F_ATT <<< "${RUN_CACHE[$k]}"
  if [ "$F_ST" != ok ]; then F_NAME=""; F_STATUS=""; F_CONC=""; F_SHA=""; F_ATT=""; fi
}
sha_equal() { [ "$1" = "$2" ]; }                 # a run is for the head the row claims only when the full shas are equal
conclusion_ok() { [ "$1" = success ]; }          # and only a conclusion of success is evidence

NEED=""
required_for() { # required_for REPO -> NEED: the workflows a claim needs on that repo's head (the first is the main one and is never waived)
  case "$1" in
    coldmail-platform) NEED="$HEAVY_PLATFORM $CLAIM_PLATFORM";;
    coldmail-verify) NEED="$REQ_VERIFY";;
    coldmail-send-managed) NEED="$REQ_MANAGED";;
    coldmail-send-byo) NEED="$REQ_BYO";;
    *) NEED="";;
  esac
}
workflow_absent() { # workflow_absent REPO FULL WF — true only when .github/workflows/<WF>.yml does not exist at that sha (HTTP 404); anything else, a 422 included, is "present"
  local k="$1 $2 $3"
  if [ -z "${ABSENT_CACHE[$k]:-}" ]; then
    api_call "repos/$OWNER/$1/contents/.github/workflows/$3.yml?ref=$2" .path
    case "$A_RC" in 4) ABSENT_CACHE[$k]=absent;; *) ABSENT_CACHE[$k]=present;; esac
  fi
  [ "${ABSENT_CACHE[$k]}" = absent ]
}
waivable() { workflow_absent "$@"; }             # a head cannot have run a workflow its tree did not have (P0 predates db-tests and supabase): the waiver is printed, counted, and never covers the first workflow of a set

# ---- 1. the documents -------------------------------------------------------------------------------------------------------------------------------------------------------------------------
LIST="$ROOT/$LIST_REL"
if [ ! -f "$LIST" ]; then echo "check-verified-heads: RED — $LIST_REL does not exist: the list of documents the gate reads"; print_not_checked; exit 1; fi
docs=()
while IFS= read -r l || [ -n "$l" ]; do
  l="${l%$'\r'}"; l="${l#"${l%%[![:space:]]*}"}"; l="${l%"${l##*[![:space:]]}"}"
  case "$l" in ''|'#'*) continue;; esac
  case "$l" in /*|*..*|[A-Za-z]:*) bad "$LIST_REL names '$l', which is not a path below the repository root"; continue;; esac
  for d in ${docs[@]+"${docs[@]}"}; do [ "$d" != "$l" ] || { bad "$LIST_REL lists $l twice (its claims would be counted twice)"; continue 2; }; done
  docs+=("$l")
done < "$LIST"
if [ "${#docs[@]}" -eq 0 ]; then echo "check-verified-heads: RED — $LIST_REL lists 0 documents: the instrument matched nothing"; print_not_checked; exit 1; fi
case " ${docs[*]} " in *" $PLAN_REL "*) ;; *) bad "$LIST_REL does not name $PLAN_REL: the build plan's status table is the record of what is done, and a list that drops it narrows the gate";; esac
echo "check-verified-heads: ${#docs[@]} document(s) from $LIST_REL: ${docs[*]}"

# ---- 2. every table row of every document -----------------------------------------------------------------------------------------------------------------------------------------------------
# awk (POSIX: no gawk extensions, no {n,m}) emits US-separated records:  R doc line kind rowid status evidence | B doc line message | S doc tables ignored-tables
PARSE_AWK='
function trim(s) { sub(/^[ ]+/, "", s); sub(/[ ]+$/, "", s); return s }
function norm(s) { gsub(/[*`]/, "", s); return tolower(trim(s)) }
function rowid(s) { gsub(/[*`]/, "", s); return trim(s) }
function splitrow(line, arr,   n, i) {
  split("", arr)
  gsub(/\\\|/, "\001", line)
  sub(/^[ ]*\|/, "", line); sub(/\|[ ]*$/, "", line)
  n = split(line, arr, "|")
  for (i = 1; i <= n; i++) { gsub(/\001/, "|", arr[i]); arr[i] = trim(arr[i]) }
  return n
}
BEGIN { OFS = "\037"; state = 0; fence = 0; ntab = 0; nign = 0 }
{ sub(/\r$/, ""); gsub(/\t/, " ") }
/^[ ]*(```|~~~)/ { fence = !fence; state = 0; next }
fence { next }
{
  isrow = ($0 ~ /^[ ]*\|/)
  if (state == 0) { if (isrow) { hn = splitrow($0, hc); hline = NR; state = 1 }; next }
  if (state == 1) {
    if ($0 ~ /^[ ]*\|[ :|-]*-[ :|-]*$/) {
      for (i = 1; i <= hn; i++) h[i] = norm(hc[i])
      kind = ""
      if (hn >= 2 && h[hn] == "evidence" && h[hn - 1] == "status") { kind = "evid"; si = hn - 1; ei = hn }
      else if (hn >= 1 && h[hn] == "status") { kind = "plan"; si = hn; ei = hn }
      if (kind == "") nign++; else ntab++
      state = 2; next
    }
    state = 0
    if (isrow) { hn = splitrow($0, hc); hline = NR; state = 1 }
    next
  }
  if (isrow) {
    if (kind == "") next
    n = splitrow($0, c)
    if (n != hn) { print "B", doc, NR, "the row has " n " cell(s) and the header (line " hline ") has " hn ": an unescaped | inside a cell? write it as \\|"; next }
    print "R", doc, NR, kind, rowid(c[1]), c[si], c[ei]
    next
  }
  state = 0
}
END { print "S", doc, ntab, nign }
'
: > "$T/rows"
for rel in "${docs[@]}"; do
  f="$ROOT/$rel"
  if [ ! -f "$f" ]; then bad "$LIST_REL names $rel, which does not exist"; continue; fi
  if [ ! -s "$f" ]; then bad "$rel is empty: the gate matched nothing"; continue; fi
  awk -v doc="$rel" "$PARSE_AWK" "$f" >> "$T/rows"
done

norm_status() { # norm_status STATUS -> NS: lowercase, emphasis and backticks stripped, trimmed
  NS="${1,,}"; NS="${NS//\*/}"; NS="${NS//\`/}"; NS="${NS#"${NS%%[![:space:]]*}"}"; NS="${NS%"${NS##*[![:space:]]}"}"
}
declare -A D_ROWS=() D_CLAIMS=() D_SKIP=() D_MENTION=() D_TABLES=() D_IGN=()
: > "$T/claims"
NROWS=0; NCLAIMS=0; NSKIP=0
while IFS=$'\037' read -r tag doc line kind rowid status evid; do
  CUR_DOC="$doc"; CUR_LINE="$line"; CUR_ID="$rowid"
  case "$tag" in
    S) D_TABLES[$doc]="$line"; D_IGN[$doc]="$kind";;
    B) CUR_ID="(row)"; viol "malformed table row: $kind";;
    R)
      NROWS=$((NROWS+1)); D_ROWS[$doc]=$(( ${D_ROWS[$doc]:-0} + 1 ))
      norm_status "$status"
      l="${status,,}"
      if [[ $l == '**done**'* ]] || [[ $l == *"done $EMDASH"* ]] || [ "$NS" = closed ]; then
        NCLAIMS=$((NCLAIMS+1)); D_CLAIMS[$doc]=$(( ${D_CLAIMS[$doc]:-0} + 1 ))
        printf '%s\037%s\037%s\037%s\n' "$doc" "$line" "$rowid" "$evid" >> "$T/claims"
      else
        case "$NS" in
          "in progress"*|"open"*|"blocked"*|"not started"*|"parked"*|"deferred"*|"pending"*|"owner action"*|"owner-action"*)
            NSKIP=$((NSKIP+1)); D_SKIP[$doc]=$(( ${D_SKIP[$doc]:-0} + 1 ))
            short="${status%%"$EMDASH"*}"; short="${short%"${short##*[![:space:]]}"}"       # the keyword before the em dash (a byte cut inside the dash would print a broken character)
            echo "  skip  $doc:$line $rowid [${short:0:44}]"
            if [[ $NS =~ (^|[^a-z])(done|closed|verified|green|complete|completed)([^a-z]|$) ]]; then
              NMENTION=$((NMENTION+1)); D_MENTION[$doc]=$(( ${D_MENTION[$doc]:-0} + 1 ))
              echo "  note  $doc:$line $rowid: the status mentions done/closed/green/verified/complete but is not in a claim form (**done**, 'done —', CLOSED): it is not checked"
            fi;;
          *)
            extra=""
            case "$NS" in closed*|done*) extra=" (a claim is exactly CLOSED, or **done** / 'done —': a qualified status is ambiguous, so say CLOSED or IN PROGRESS)";; esac
            viol "the status '${status:0:60}' cannot be classified: it is neither a claim (**done**, 'done —', CLOSED) nor a named non-claim (IN PROGRESS, OPEN, BLOCKED, NOT STARTED, PARKED, DEFERRED, PENDING, OWNER ACTION)$extra";;
        esac
      fi;;
  esac
done < "$T/rows"

# the floors, per document and overall
for rel in "${docs[@]}"; do
  [ -f "$ROOT/$rel" ] && [ -s "$ROOT/$rel" ] || continue
  CUR_DOC="$rel"; CUR_LINE=1; CUR_ID="-"
  nt="${D_TABLES[$rel]:-0}"; nr="${D_ROWS[$rel]:-0}"; nc="${D_CLAIMS[$rel]:-0}"; ns="${D_SKIP[$rel]:-0}"; nm="${D_MENTION[$rel]:-0}"
  echo "check-verified-heads: $rel: $nt status table(s) read (${D_IGN[$rel]:-0} other table(s) ignored), $nr row(s) parsed, $nc claim(s), $ns skipped as not claims"
  if [ "$nt" -eq 0 ]; then bad "$rel: found 0 status tables (a table whose header ends Status, or Status | Evidence): the instrument matched nothing"
  elif [ "$nr" -eq 0 ]; then bad "$rel: found 0 table rows under its status table(s): the instrument matched nothing"
  elif [ "$nc" -eq 0 ] && [ "$nm" -gt 0 ]; then bad "$rel: found 0 claims but $nm status cell(s) mention done/closed/green/verified/complete outside the claim forms: the instrument matched nothing (a phrasing it does not read)"; fi
done
if [ "$NCLAIMS" -eq 0 ]; then bad "found 0 claims in ${#docs[@]} document(s) — the instrument matched nothing (a repository with phases done has claims; a gate that checked nothing is red)"; fi
echo "check-verified-heads: rows parsed: $NROWS ($NCLAIMS claim(s), $NSKIP skipped as not claims)"

# ---- 3. every claim, against the API ---------------------------------------------------------------------------------------------------------------------------------------------------------
check_claim() { # check_claim DOC LINE ROWID EVIDENCE
  CUR_DOC="$1"; CUR_LINE="$2"; CUR_ID="$3"
  local text="$4" before="$fail" i j k nh nr repo short full word need wf missing seen heads_of_repo hit cited_for unfetched
  extract_heads "$text"; extract_runs "$text"
  nh=${#HEAD_REPO[@]}; nr=${#RUN_ID[@]}
  if [ -n "$BARE" ]; then NBARE=$((NBARE + $(wc -w <<< "$BARE"))); echo "  note  $CUR_DOC:$CUR_LINE $CUR_ID: hash-like string(s) with no repo word before them were not read as heads: ${BARE% }"; fi
  [ "$nh" -gt 0 ] || viol "a claim must cite the head and the run: no head token found (write <repo>@<sha>, e.g. platform@8fbae10, platform main @ a6709c9, module 1 e32c1b9; the repo is platform, verify, send-managed, send-byo or module 1/2/3)"
  [ "$nr" -gt 0 ] || viol "a claim must cite the head and the run: no GitHub Actions run URL found (https://github.com/$OWNER/<repo>/actions/runs/<id>[/attempts/<n>])"

  local -a HF=()                        # the full sha of each cited head ("" when it did not resolve)
  for ((i = 0; i < nh; i++)); do
    repo="${HEAD_REPO[i]}"; short="${HEAD_SHA[i]}"; HF[i]=""
    resolve_head "$repo" "$short"
    case "$R_ST" in
      ok) HF[i]="$R_VAL"; HEADSET["$repo $R_VAL"]=1;;
      nf) HEADSET["$repo $short"]=1; viol "$repo@$short does not resolve to a commit in $OWNER/$repo (HTTP $R_VAL): a typo, a commit never pushed, or a token that cannot read $repo";;
      *)  HEADSET["$repo $short"]=1; viol "$repo@$short: the API answered '$R_VAL' where a full 40-hex sha starting with the cited hex was expected";;
    esac
  done

  local -a CR_OKF=() CR_SHA=() CR_NAME=()   # per cited run: fetched (1), its head sha, its workflow
  declare -A CITED_OK=()
  for ((j = 0; j < nr; j++)); do
    repo="${RUN_REPO[j]}"; RUNSET["$repo ${RUN_ID[j]} ${RUN_ATT[j]}"]=1
    fetch_run "$repo" "${RUN_ID[j]}" "${RUN_ATT[j]}"
    CR_OKF[j]=0; CR_SHA[j]=""; CR_NAME[j]=""
    case "$F_ST" in
      ok) ;;
      nf) viol "run ${RUN_ID[j]} does not exist in $OWNER/$repo (HTTP 404; the URL is ${RUN_URL[j]}): a wrong id, a run deleted, or a token that cannot read $repo"; continue;;
      *)  viol "run ${RUN_ID[j]} in $OWNER/$repo: the API answered something that is not one run ('$F_NOTE'): the instrument matched nothing"; continue;;
    esac
    CR_OKF[j]=1; CR_SHA[j]="$F_SHA"; CR_NAME[j]="$F_NAME"
    local att=""; [ -z "${RUN_ATT[j]}" ] || att=" attempt ${RUN_ATT[j]}"
    # is it for a head the row claims (same repo, full shas equal)?
    heads_of_repo=""; hit=0
    for ((k = 0; k < nh; k++)); do
      [ "${HEAD_REPO[k]}" = "$repo" ] || continue
      heads_of_repo+="$repo@${HEAD_SHA[k]}, "
      [ -n "${HF[k]}" ] && sha_equal "$F_SHA" "${HF[k]}" && hit=1
    done
    if [ -z "$heads_of_repo" ]; then
      [ "$nh" -eq 0 ] || viol "run ${RUN_ID[j]} ($F_NAME) is in $OWNER/$repo but the row cites no head of $repo: a run for a repository the row does not claim"
    elif [ "$hit" -eq 0 ]; then
      viol "run ${RUN_ID[j]} ($F_NAME) is for sha $F_SHA, not for the head the row claims (${heads_of_repo%, })"
    fi
    if [ "$F_STATUS" != completed ]; then
      viol "run ${RUN_ID[j]}$att ($F_NAME) is $F_STATUS, not completed: it has no conclusion yet"
    elif ! conclusion_ok "$F_CONC"; then
      viol "run ${RUN_ID[j]}$att ($F_NAME) concluded '$F_CONC', not success"
    elif [ "$hit" -eq 1 ]; then
      CITED_OK["$repo $F_SHA $F_NAME"]=1
    fi
  done

  # the required workflows must have run green on each resolved head
  for ((i = 0; i < nh; i++)); do
    repo="${HEAD_REPO[i]}"; short="${HEAD_SHA[i]}"; full="${HF[i]}"
    [ -n "$full" ] || continue
    fetch_list "$repo" "$full"
    case "${LST_STATE["$repo $full"]}" in
      nf) viol "$repo@$short ($full): the runs of this sha could not be listed (HTTP 404): the token cannot read Actions on $repo?"; continue;;
      bad) viol "$repo@$short ($full): ${LST_NOTE["$repo $full"]}"; continue;;
    esac
    required_for "$repo"; missing=""; waived=""
    for wf in $NEED; do
      case "${LST_OKS["$repo $full"]}" in *" $wf "*) continue;; esac
      [ -z "${CITED_OK["$repo $full $wf"]:-}" ] || continue
      if [ "$wf" != "${NEED%% *}" ] && waivable "$repo" "$full" "$wf"; then waived+="$wf, "; NWAIVED=$((NWAIVED+1)); else missing+="$wf, "; fi
    done
    [ -z "$waived" ] || echo "  note  $CUR_DOC:$CUR_LINE $CUR_ID: $repo@$short: workflow(s) ${waived%, } did not exist at this sha (no .github/workflows/<name>.yml at $full), so they are not required of it"
    if [ -n "$missing" ]; then
      seen="${LST_SEEN["$repo $full"]}"; [ -n "$seen" ] || seen="no runs at all"
      trunc=""; [ "${LST_TOTAL["$repo $full"]}" -le "$LIST_PAGE" ] || trunc="; only the first $LIST_PAGE of ${LST_TOTAL["$repo $full"]} runs were read (no pagination)"
      viol "$repo@$short ($full): the heavy workflows did not all run green on this exact sha: missing a successful run of: ${missing%, } (runs on this sha: $seen$trunc)"
    fi
    # the row must cite a run for this head
    cited_for=0; unfetched=0
    for ((j = 0; j < nr; j++)); do
      [ "${RUN_REPO[j]}" = "$repo" ] || continue
      if [ "${CR_OKF[j]}" -eq 1 ]; then sha_equal "${CR_SHA[j]}" "$full" && cited_for=$((cited_for+1)); else unfetched=$((unfetched+1)); fi
    done
    if [ "$nr" -gt 0 ] && [ "$cited_for" -eq 0 ] && [ "$unfetched" -eq 0 ]; then
      viol "$repo@$short ($full): the row cites no run for this head: the evidence must cite the run, not only the head"
    fi
  done
  if [ "$fail" -le "$before" ]; then
    seen=""; for ((i = 0; i < nh; i++)); do seen+="${HEAD_REPO[i]}@${HEAD_SHA[i]}, "; done
    echo "  ok    $CUR_DOC:$CUR_LINE $CUR_ID: ${seen%, }; $nr cited run(s)"
  fi
}

while IFS=$'\037' read -r cdoc cline cid cevid; do
  check_claim "$cdoc" "$cline" "$cid" "$cevid"
done < "$T/claims"

echo "check-verified-heads: claims: $NCLAIMS, heads checked: ${#HEADSET[@]}, runs checked: ${#RUNSET[@]}"
print_not_checked
[ "$fail" -eq 0 ] || { echo "check-verified-heads: RED — $fail problem(s)"; exit 1; }
echo "check-verified-heads: OK"
