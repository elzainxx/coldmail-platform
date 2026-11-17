#!/usr/bin/env bash
# check-verified-heads-selftest.sh — proves scripts/check-verified-heads.sh is green on a right set of documents and goes red, FOR THE REASON IT NAMES, on every way a "verified" claim can be wrong.
# Every case builds a throwaway tree (the build plan's status table, the close-out spec's item table, the document list) and a directory of FIXTURES, and runs the real gate on it with --api-cmd pointing at a
# STUB that answers `api <endpoint> --jq <expr>` from the fixtures: the selftest never touches the network and needs no token. Each reject case asserts the exit code AND the printed reason (class 10: a reject
# case that fails for another reason is no proof); the stub logs every call so a case can assert that the gate asked (or did not ask) for a given thing. A hard minimum number of cases, and controls that prove THIS
# file can fail: a gate that always passes, a gate that always fails, a gate that fails for the wrong reason, and copies of the real gate with one check removed (the sha comparison, the conclusion check, the
# unreachable-API stop, the zero-claims floor), each of which must make its case misbehave.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-verified-heads.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
MIN_CASES=60
n=0; bad=0
t() { n=$((n+1)); }
U="https://github.com/elzainxx"
PLAN_REL="Docs/coldmail-platform-build-plan.md"; CLOSE_REL="Docs/phase-2-closeout-spec.md"; LIST_REL="tests/verified-heads-docs.txt"
EM=$'\xe2\x80\x94'

# ---- the stub api command ------------------------------------------------------------------------------------------------------------------------------------------------------------------------
# `<stub> api <endpoint> --jq <expr>`: the fixture for an endpoint is the file named by the endpoint with / ? & = turned into _ (the text the --jq expression would print: the real expression is evaluated by gh
# on the runner, so the gate refuses an answer whose shape it cannot parse). __down: every call fails with that text; <fixture>.422: that endpoint answers HTTP 422. A missing fixture is HTTP 404.
STUB="$TMP/stub-gh.sh"
cat > "$STUB" <<'STUBEOF'
#!/usr/bin/env bash
[ "${1:-}" = api ] && [ "${3:-}" = --jq ] && [ -n "${4:-}" ] || { echo "stub: expected 'api <endpoint> --jq <expr>', got: $*" >&2; exit 2; }
printf '%s\n' "$2" >> "$FIXDIR/__calls"
key="${2//\//_}"; key="${key//\?/_}"; key="${key//&/_}"; key="${key//=/_}"
emit() { local l; while IFS= read -r l || [ -n "$l" ]; do printf '%s\n' "$l"; done < "$1"; }
if [ -f "$FIXDIR/__down" ]; then emit "$FIXDIR/__down" >&2; exit 1; fi
if [ -f "$FIXDIR/$key.422" ]; then echo "gh: No commit found for SHA (HTTP 422)" >&2; exit 1; fi
if [ -f "$FIXDIR/$key" ]; then emit "$FIXDIR/$key"; exit 0; fi
echo "gh: Not Found (HTTP 404)" >&2; exit 1
STUBEOF
chmod +x "$STUB"

# ---- fixtures (bash builtins only: a Windows machine pays for every process) ---------------------------------------------------------------------------------------------------------
full() { printf '%s%033d' "$1" 0; }                  # the 40-hex sha of a 7-hex head
FXK=""; FULL=""
fxk() { FXK="${1//\//_}"; FXK="${FXK//\?/_}"; FXK="${FXK//&/_}"; FXK="${FXK//=/_}"; }   # the fixture file name of an endpoint, in FXK
fx() { fxk "$2"; printf '%s' "$3" > "$1/fix/$FXK"; }          # fx DIR ENDPOINT BODY: the fixture for an endpoint is exactly BODY
fx_rm() { fxk "$2"; rm -f "$1/fix/$FXK"; }                    # fx_rm DIR ENDPOINT: the endpoint now answers 404
fx_commit() { printf -v FULL '%s%033d' "$3" 0; fx "$1" "repos/elzainxx/$2/commits/$3" "$FULL"$'\n'; }            # fx_commit DIR REPO SHORT
RID=5000
fx_list() { # fx_list DIR REPO SHORT "wf:conclusion wf:conclusion ..." [TOTAL]: the runs of one sha (the first line is the count)
  local f pair line k=0 body=""; printf -v f '%s%033d' "$3" 0
  for pair in $4; do k=$((k+1)); RID=$((RID+1)); printf -v line '%s\t%s\tcompleted\t%s\t%s\t1\n' "$RID" "${pair%%:*}" "${pair#*:}" "$f"; body+="$line"; done
  fx "$1" "repos/elzainxx/$2/actions/runs?head_sha=$f&per_page=100" "${5:-$k}"$'\n'"$body"
}
fx_run() { # fx_run DIR REPO ID WF STATUS CONCLUSION SHORT ATTEMPT [ENDPOINT-SUFFIX]: one cited run (or attempt)
  local f line; printf -v f '%s%033d' "$7" 0; printf -v line '%s\t%s\t%s\t%s\t%s\t%s\n' "$3" "$4" "$5" "$6" "$f" "$8"
  fx "$1" "repos/elzainxx/$2/actions/runs/$3${9:-}" "$line"
}
fx_wf() { # fx_wf DIR REPO SHORT "wf wf ...": the workflow files exist at that sha (an absent one answers 404)
  local f wf; printf -v f '%s%033d' "$3" 0
  for wf in $4; do fx "$1" "repos/elzainxx/$2/contents/.github/workflows/$wf.yml?ref=$f" ".github/workflows/$wf.yml"$'\n'; done
}
PLAT_ALL="harness:success db-tests:success supabase:success go:success gates:success gitleaks:success web:success platform-check:success"
PA=a1b2c3d; PB=b2c3d4e; VA=c3d4e5f; MB=d4e5f60; MC=e5f6071       # platform, another platform head, verify, send-managed, send-byo
FULLPA="$(full "$PA")"; FULLPB="$(full "$PB")"; FULLVA="$(full "$VA")"; FULLMB="$(full "$MB")"; FULLMC="$(full "$MC")"

# good_tree DIR [VAR=value ...]: a tree the gate accepts: 3 claims (P0 platform, A1 verify, C1 send-byo), 4 skipped rows, 3 heads, 6 cited runs, 12 API calls. A VAR=value argument replaces one row's text.
good_tree() {
  local d="$1"; shift
  local P0_STATUS="**done** $EM platform \`main\` @ \`$PA\`, every workflow green: [harness run 1001]($U/coldmail-platform/actions/runs/1001) and [db-tests 1002]($U/coldmail-platform/actions/runs/1002)"
  local P1_STATUS="IN PROGRESS $EM platform@$MB is pushed and has no run"
  local P2_STATUS="not started"
  local A1_STATUS="CLOSED"
  local A1_EVID="verify@$VA: [ci]($U/coldmail-verify/actions/runs/2001) + [gitleaks attempt 1]($U/coldmail-verify/actions/runs/2002/attempts/1) green on the runner"
  local B1_STATUS="IN PROGRESS"
  local B1_EVID="send-managed@$MB pushed; no runner run exists for it"
  local C1_STATUS="CLOSED"
  local C1_EVID="module 3 \`$MC\`: [real-infra]($U/coldmail-send-byo/actions/runs/3001), [gitleaks]($U/coldmail-send-byo/actions/runs/3002)"
  local D1_STATUS="OPEN"
  local kv
  for kv in "$@"; do printf -v "${kv%%=*}" '%s' "${kv#*=}"; done
  rm -rf "$d"; mkdir -p "$d/Docs" "$d/tests" "$d/fix"
  printf '%s\n' '# build plan' '' '## Status' '' '| Phase | Status |' '|---|---|' \
    "| P0 Groundwork | $P0_STATUS |" "| P1 Schema | $P1_STATUS |" "| P2 Money core | $P2_STATUS |" '' '**Keep this table current.**' '' \
    '| Unit | Default cost | Notes |' '|---|---|---|' '| M1 verification | 1 | a table that is not a status table: ignored |' > "$d/$PLAN_REL"
  printf '%s\n' '# close-out' '' 'prose' '' '| # | Item | Exit check | Status | Evidence |' '|---|---|---|---|---|' \
    "| A1 | an item with an escaped pipe \`own \\| remote\` | exit | $A1_STATUS | $A1_EVID |" \
    "| B1 | item | exit | $B1_STATUS | $B1_EVID |" \
    "| C1 | item | exit | $C1_STATUS | $C1_EVID |" \
    "| D1 | item | exit | $D1_STATUS | |" > "$d/$CLOSE_REL"
  printf '%s\n' '# the documents the gate reads' "$PLAN_REL" "$CLOSE_REL" > "$d/$LIST_REL"
  fx_commit "$d" coldmail-platform "$PA";    fx_list "$d" coldmail-platform "$PA" "$PLAT_ALL"; fx_wf "$d" coldmail-platform "$PA" "harness db-tests supabase go gates gitleaks web platform-check"
  fx_run "$d" coldmail-platform 1001 harness completed success "$PA" 1;  fx_run "$d" coldmail-platform 1002 db-tests completed success "$PA" 1
  fx_commit "$d" coldmail-verify "$VA";      fx_list "$d" coldmail-verify "$VA" "ci:success gitleaks:success"; fx_wf "$d" coldmail-verify "$VA" "ci gitleaks"
  fx_run "$d" coldmail-verify 2001 ci completed success "$VA" 1;         fx_run "$d" coldmail-verify 2002 gitleaks completed success "$VA" 1 /attempts/1
  fx_commit "$d" coldmail-send-byo "$MC";    fx_list "$d" coldmail-send-byo "$MC" "real-infra:success gitleaks:success"; fx_wf "$d" coldmail-send-byo "$MC" "real-infra gitleaks"
  fx_run "$d" coldmail-send-byo 3001 real-infra completed success "$MC" 1;   fx_run "$d" coldmail-send-byo 3002 gitleaks completed success "$MC" 1
}

# expect NAME DIR WANT_RC FRAGMENT...  (each fragment must appear in the output; CALL:<endpoint> must be a line of the stub's call log, NOCALL:<text> must appear in no call)
CMD_ARGS=(--api-cmd "$STUB")
expect() {
  t; local name="$1" d="$2" want="$3" out rc frag; shift 3
  out="$(FIXDIR="$d/fix" bash "$GATE" --root "$d" "${CMD_ARGS[@]}" 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; printf '%s\n' "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  for frag in "$@"; do
    case "$frag" in
      CALL:*)   if ! grep -qxF -- "${frag#CALL:}" "$d/fix/__calls" 2>/dev/null; then echo "  FAIL $name: exit $rc as expected but the stub saw no call '${frag#CALL:}'"; sed 's/^/     | /' "$d/fix/__calls" 2>/dev/null; bad=$((bad+1)); return; fi;;
      NOCALL:*) if grep -qF -- "${frag#NOCALL:}" "$d/fix/__calls" 2>/dev/null; then echo "  FAIL $name: exit $rc as expected but the stub saw a call containing '${frag#NOCALL:}'"; bad=$((bad+1)); return; fi;;
      *) if ! printf '%s\n' "$out" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc as expected but the output lacks '$frag'"; printf '%s\n' "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi;;
    esac
  done
  echo "  ok   $name"
}
NCALLS() { wc -l < "$1/fix/__calls" | tr -d ' '; }

D="$TMP/t"
PLAT="$U/coldmail-platform/actions/runs"
P0L="$PLAN_REL:7 P0 Groundwork:"        # the file:line and row id of the P0 row in every fixture tree
A1L="$CLOSE_REL:7 A1:"; C1L="$CLOSE_REL:9 C1:"

# ---- green ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; expect "a right tree is green" "$D" 0 "check-verified-heads: OK" "  ok    $P0L" "  ok    $A1L" "  ok    $C1L"
good_tree "$D"; expect "the green run states the counts" "$D" 0 "check-verified-heads: claims: 3, heads checked: 3, runs checked: 6"
good_tree "$D"; expect "the green run prints the rows parsed and skipped, per document and overall" "$D" 0 "rows parsed: 7 (3 claim(s), 4 skipped as not claims)" \
  "$PLAN_REL: 1 status table(s) read (1 other table(s) ignored), 3 row(s) parsed, 1 claim(s), 2 skipped as not claims" "$CLOSE_REL: 1 status table(s) read (0 other table(s) ignored), 4 row(s) parsed, 2 claim(s), 2 skipped as not claims"
good_tree "$D"; expect "every skipped row is printed with its status (the keyword before an em dash)" "$D" 0 "  skip  $PLAN_REL:8 P1 Schema [IN PROGRESS]" "  skip  $PLAN_REL:9 P2 Money core [not started]" "  skip  $CLOSE_REL:8 B1 [IN PROGRESS]" "  skip  $CLOSE_REL:10 D1 [OPEN]"
good_tree "$D"; expect "the green run asked the API for each head, each sha's runs and each cited run, and for nothing else (12 calls)" "$D" 0 \
  "CALL:repos/elzainxx/coldmail-platform/commits/$PA" "CALL:repos/elzainxx/coldmail-platform/actions/runs?head_sha=$FULLPA&per_page=100" "CALL:repos/elzainxx/coldmail-platform/actions/runs/1001" \
  "CALL:repos/elzainxx/coldmail-verify/actions/runs/2002/attempts/1" "CALL:repos/elzainxx/coldmail-send-byo/actions/runs/3002" "NOCALL:$MB"
good_tree "$D"; out="$(FIXDIR="$D/fix" bash "$GATE" --root "$D" "${CMD_ARGS[@]}" 2>&1)"; t
if [ "$(NCALLS "$D")" = 12 ]; then echo "  ok   the green run made exactly 12 API calls (3 commits, 3 run lists, 6 cited runs)"; else echo "  FAIL the green run made $(NCALLS "$D") API calls, want 12"; bad=$((bad+1)); fi
good_tree "$D"; expect "the green run prints what it did not check" "$D" 0 "NOT CHECKED: that a cited run's logs are the right logs" "that the sha a human typed is the sha they meant" "claims phrased outside the table formats" "documents not in $LIST_REL"
good_tree "$D"; sed -i 's/$/\r/' "$D/$PLAN_REL" "$D/$CLOSE_REL" "$D/$LIST_REL"; expect "documents and list with CRLF line endings are green (CR stripped)" "$D" 0 "check-verified-heads: OK" "claims: 3,"
# the owner's real case: a gitleaks run whose attempt 1 was green and which a re-run turned into failure: the row cites /attempts/1, the run's latest conclusion is failure
good_tree "$D"; fx_list "$D" coldmail-verify "$VA" "ci:success gitleaks:failure"; fx_run "$D" coldmail-verify 2002 gitleaks completed failure "$VA" 2
expect "a cited /attempts/1 that was green is green although the run's latest attempt failed (the list shows the failure; the cited attempt is the evidence)" "$D" 0 "check-verified-heads: OK" "CALL:repos/elzainxx/coldmail-verify/actions/runs/2002/attempts/1"
# every head form of the grammar
good_tree "$D" P0_STATUS="**done** $EM main @ \`$PA\`: [harness]($PLAT/1001) [db-tests]($PLAT/1002)"; expect "the form 'main @ <sha>' is the platform" "$D" 0 "check-verified-heads: OK"
good_tree "$D" P0_STATUS="**done** $EM every workflow green on \`$PA\`: [harness]($PLAT/1001) [db-tests]($PLAT/1002)"; expect "the form 'green on <sha>' is the platform" "$D" 0 "check-verified-heads: OK"
good_tree "$D" P0_STATUS="done $EM coldmail-platform@$PA [harness]($PLAT/1001) [db-tests]($PLAT/1002)"; expect "the form 'done —' (not bold) is a claim, and coldmail-<repo>@<sha> is a head" "$D" 0 "check-verified-heads: OK" "claims: 3,"
good_tree "$D" P0_STATUS="**done** $EM platform@$FULLPA [harness]($PLAT/1001) [db-tests]($PLAT/1002)"; fx "$D" "repos/elzainxx/coldmail-platform/commits/$FULLPA" "$FULLPA"$'\n'
expect "a 40-hex head is looked up and accepted" "$D" 0 "check-verified-heads: OK" "CALL:repos/elzainxx/coldmail-platform/commits/$FULLPA"
good_tree "$D" A1_STATUS="**CLOSED**" A1_EVID="module 1 $VA+ [ci]($U/coldmail-verify/actions/runs/2001) [gitleaks]($U/coldmail-verify/actions/runs/2002/attempts/1)"; expect "a bold CLOSED is a claim, and 'module 1 <sha>' is the verify head" "$D" 0 "check-verified-heads: OK" "claims: 3,"
good_tree "$D" B1_STATUS="IN PROGRESS"; expect "a document whose rows are all not-claims is green when another document holds claims, and says 0 claims" "$D" 0 "check-verified-heads: OK" \
  "$CLOSE_REL: 1 status table(s) read (0 other table(s) ignored), 4 row(s) parsed, 2 claim(s), 2 skipped as not claims"
good_tree "$D" B1_STATUS="BLOCKED $EM owner action" D1_STATUS="owner action" P2_STATUS="**Not started**"; expect "BLOCKED, owner action and not started are named non-claims: skipped, counted and printed" "$D" 0 "check-verified-heads: OK" "  skip  $CLOSE_REL:8 B1 [BLOCKED]" "  skip  $CLOSE_REL:10 D1 [owner action]" "  skip  $PLAN_REL:9 P2 Money core [**Not started**]"
good_tree "$D" A1_STATUS="IN PROGRESS" C1_STATUS="OPEN"; expect "a document with 0 claims and no mention of done is green and says so" "$D" 0 "check-verified-heads: OK" "$CLOSE_REL: 1 status table(s) read (0 other table(s) ignored), 4 row(s) parsed, 0 claim(s), 4 skipped as not claims" "claims: 1,"

# ---- a claim must cite the head and the run ------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D" P0_STATUS="**done** $EM every workflow green: [harness run 1001]($PLAT/1001)"
expect "a claim with a run and no head token" "$D" 1 "RED: $P0L a claim must cite the head and the run: no head token found" "RED — 1 problem(s)"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA, every workflow green"
expect "a claim with a head and no run URL" "$D" 1 "RED: $P0L a claim must cite the head and the run: no GitHub Actions run URL found"
good_tree "$D" P0_STATUS="**done** $EM green at \`$PA\` (Part A e5c58f4): [harness]($PLAT/1001)"
expect "a sha with no repo word before it is not a head, and the unread hash is named" "$D" 1 "RED: $P0L a claim must cite the head and the run: no head token found" "hash-like string(s) with no repo word before them were not read as heads: $PA e5c58f4" "2 hash-like string(s) in claim rows"
good_tree "$D" C1_EVID="module 3 \`$MC\` green"
expect "a CLOSED row without a run URL is red and named by file, line and row id" "$D" 1 "RED: $C1L a claim must cite the head and the run: no GitHub Actions run URL found" "RED — "
good_tree "$D" A1_EVID="closed, runs: [ci]($U/coldmail-verify/actions/runs/2001)"
expect "a CLOSED row without a head token (the close-out spec's E4 shape) names its row" "$D" 1 "RED: $A1L a claim must cite the head and the run: no head token found"

# ---- the cited runs --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness completed success "$PB" 1
expect "a cited run for another sha of the same repo (an older head)" "$D" 1 "RED: $P0L run 1001 (harness) is for sha $FULLPB, not for the head the row claims (coldmail-platform@$PA)" "RED — 1 problem(s)"
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness completed failure "$PA" 1
expect "a cited run that concluded failure" "$D" 1 "RED: $P0L run 1001 (harness) concluded 'failure', not success"
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness completed cancelled "$PA" 1
expect "a cited run that was cancelled" "$D" 1 "RED: $P0L run 1001 (harness) concluded 'cancelled', not success"
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness in_progress - "$PA" 1
expect "a cited run still in progress has no conclusion" "$D" 1 "RED: $P0L run 1001 (harness) is in_progress, not completed: it has no conclusion yet"
good_tree "$D"; fx_rm "$D" repos/elzainxx/coldmail-platform/actions/runs/1001
expect "a cited run that does not exist (404)" "$D" 1 "RED: $P0L run 1001 does not exist in elzainxx/coldmail-platform (HTTP 404" "a token that cannot read coldmail-platform"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001/attempts/2) [db-tests]($PLAT/1002)"; fx_run "$D" coldmail-platform 1001 harness completed failure "$PA" 2 /attempts/2
expect "a cited attempt that failed is red with the attempt named, whatever the other attempts did" "$D" 1 "RED: $P0L run 1001 attempt 2 (harness) concluded 'failure', not success" "CALL:repos/elzainxx/coldmail-platform/actions/runs/1001/attempts/2"
good_tree "$D" A1_EVID="verify@$VA: [gitleaks]($U/coldmail-verify/actions/runs/2002) (the latest attempt) [ci]($U/coldmail-verify/actions/runs/2001)"; fx_list "$D" coldmail-verify "$VA" "ci:success gitleaks:failure"; fx_run "$D" coldmail-verify 2002 gitleaks completed failure "$VA" 2
expect "a plain run URL whose latest attempt failed is red (only /attempts/<n> pins an older attempt) and the workflow is then missing" "$D" 1 "RED: $A1L run 2002 (gitleaks) concluded 'failure', not success" "missing a successful run of: gitleaks (runs on this sha: ci=success, gitleaks=failure)"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001) [db-tests]($PLAT/1002) [ci]($U/coldmail-verify/actions/runs/2001)"
expect "a cited run in a repository whose head the row does not cite" "$D" 1 "RED: $P0L run 2001 (ci) is in elzainxx/coldmail-verify but the row cites no head of coldmail-verify"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001) [elsewhere](https://github.com/elzainxx/other-repo/actions/runs/9)"
expect "a run URL in a repository outside the four" "$D" 1 "RED: $P0L the run URL https://github.com/elzainxx/other-repo/actions/runs/9 is not in one of the four repositories"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001) [elsewhere](https://github.com/someone-else/coldmail-platform/actions/runs/9)"
expect "a run URL under another owner" "$D" 1 "RED: $P0L the run URL https://github.com/someone-else/coldmail-platform/actions/runs/9 is not in one of the four repositories"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA and verify@$VA: [harness]($PLAT/1001) [db-tests]($PLAT/1002)"
expect "a cited head with no cited run for it (the row cites the platform's runs and a verify head)" "$D" 1 "RED: $P0L coldmail-verify@$VA ($FULLVA): the row cites no run for this head: the evidence must cite the run, not only the head"

# ---- the required workflows on the exact sha ---------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [go]($PLAT/1003)"; fx_run "$D" coldmail-platform 1003 go completed success "$PA" 1; fx_list "$D" coldmail-platform "$PA" "go:success"
expect "only go ran: the heavy workflows (harness, db-tests, supabase) and the rest of a done claim are missing" "$D" 1 \
  "RED: $P0L coldmail-platform@$PA ($FULLPA): the heavy workflows did not all run green on this exact sha: missing a successful run of: harness, db-tests, supabase, gates, gitleaks, web, platform-check (runs on this sha: go=success)"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [db-tests]($PLAT/1002)"; fx_list "$D" coldmail-platform "$PA" "harness:failure db-tests:success supabase:success go:success gates:success gitleaks:success web:success platform-check:success"
expect "harness failed on the sha (the list shows failure) and the row does not cite a green one" "$D" 1 "missing a successful run of: harness (runs on this sha: harness=failure, db-tests=success"
good_tree "$D"; fx_list "$D" coldmail-platform "$PA" "harness:success db-tests:success supabase:success go:success gates:success gitleaks:success web:success"
expect "platform-check never ran on the sha: a done claim needs the whole set" "$D" 1 "RED: $P0L coldmail-platform@$PA ($FULLPA): the heavy workflows did not all run green on this exact sha: missing a successful run of: platform-check"
good_tree "$D"; fx_list "$D" coldmail-platform "$PA" "" 0
expect "a head with no run at all is red and says so (an empty run list is a valid answer, not a green)" "$D" 1 "missing a successful run of: supabase, go, gates, gitleaks, web, platform-check (runs on this sha: no runs at all)"
good_tree "$D" A1_EVID="verify@$VA: [gitleaks]($U/coldmail-verify/actions/runs/2002/attempts/1)"; fx_list "$D" coldmail-verify "$VA" "gitleaks:success"
expect "a module claim without a ci run" "$D" 1 "RED: $A1L coldmail-verify@$VA ($FULLVA): the heavy workflows did not all run green on this exact sha: missing a successful run of: ci (runs on this sha: gitleaks=success)"
good_tree "$D" A1_EVID="verify@$VA: [ci]($U/coldmail-verify/actions/runs/2001)"; fx_list "$D" coldmail-verify "$VA" "ci:success"
expect "a module claim without a gitleaks run" "$D" 1 "RED: $A1L coldmail-verify@$VA" "missing a successful run of: gitleaks (runs on this sha: ci=success)"
good_tree "$D" C1_EVID="module 3 \`$MC\`: [gitleaks]($U/coldmail-send-byo/actions/runs/3002)"; fx_list "$D" coldmail-send-byo "$MC" "gitleaks:success"
expect "a send-byo claim without a real-infra run" "$D" 1 "RED: $C1L coldmail-send-byo@$MC ($FULLMC): the heavy workflows did not all run green on this exact sha: missing a successful run of: real-infra (runs on this sha: gitleaks=success)"
good_tree "$D" C1_EVID="send-managed@$MB: [real-infra]($U/coldmail-send-managed/actions/runs/4001)"; fx_commit "$D" coldmail-send-managed "$MB"; fx_run "$D" coldmail-send-managed 4001 real-infra completed success "$MB" 1; fx_list "$D" coldmail-send-managed "$MB" "real-infra:success"; fx_wf "$D" coldmail-send-managed "$MB" "real-infra gitleaks"
expect "a send-managed claim needs real-infra AND gitleaks" "$D" 1 "RED: $C1L coldmail-send-managed@$MB ($FULLMB): the heavy workflows did not all run green on this exact sha: missing a successful run of: gitleaks (runs on this sha: real-infra=success)"
good_tree "$D" A1_EVID="verify@$VA: [ci]($U/coldmail-verify/actions/runs/2001)"; fx_list "$D" coldmail-verify "$VA" "$(for i in $(seq 1 100); do printf 'ci:success '; done)" 150
expect "more than 100 runs on one sha: only the first page is read, and the message says so" "$D" 1 "missing a successful run of: gitleaks" "only the first 100 of 150 runs were read (no pagination)"

# ---- a workflow the head's tree did not have is not required of it (P0 predates db-tests and supabase); the first workflow of a set is never waived ----------------------------------------------
WAIVE_LIST="harness:success go:success gates:success gitleaks:success web:success platform-check:success"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001)"; fx_list "$D" coldmail-platform "$PA" "$WAIVE_LIST"
fx_rm "$D" "repos/elzainxx/coldmail-platform/contents/.github/workflows/db-tests.yml?ref=$FULLPA"; fx_rm "$D" "repos/elzainxx/coldmail-platform/contents/.github/workflows/supabase.yml?ref=$FULLPA"
expect "db-tests and supabase did not exist at the head: waived, printed and counted" "$D" 0 "check-verified-heads: OK" "note  $P0L coldmail-platform@$PA: workflow(s) db-tests, supabase did not exist at this sha (no .github/workflows/<name>.yml at $FULLPA), so they are not required of it" \
  "2 required workflow(s) were waived" "CALL:repos/elzainxx/coldmail-platform/contents/.github/workflows/db-tests.yml?ref=$FULLPA"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [harness]($PLAT/1001)"; fx_list "$D" coldmail-platform "$PA" "$WAIVE_LIST"
expect "db-tests and supabase exist at the head and never ran: red, not waived" "$D" 1 "RED: $P0L coldmail-platform@$PA ($FULLPA): the heavy workflows did not all run green on this exact sha: missing a successful run of: db-tests, supabase" "0 required workflow(s) were waived"
good_tree "$D" P0_STATUS="**done** $EM platform@$PA [db-tests]($PLAT/1002)"; fx_list "$D" coldmail-platform "$PA" "db-tests:success supabase:success go:success gates:success gitleaks:success web:success platform-check:success"
fx_rm "$D" "repos/elzainxx/coldmail-platform/contents/.github/workflows/harness.yml?ref=$FULLPA"
expect "harness is the first workflow of the set and is never waived, even when its file is gone" "$D" 1 "RED: $P0L coldmail-platform@$PA ($FULLPA): the heavy workflows did not all run green on this exact sha: missing a successful run of: harness (" "NOCALL:contents/.github/workflows/harness.yml"
good_tree "$D" A1_EVID="verify@$VA: [ci]($U/coldmail-verify/actions/runs/2001)"; fx_list "$D" coldmail-verify "$VA" "ci:success"; fx_rm "$D" "repos/elzainxx/coldmail-verify/contents/.github/workflows/gitleaks.yml?ref=$FULLVA"
expect "a module whose tree had no gitleaks workflow: waived" "$D" 0 "check-verified-heads: OK" "note  $A1L coldmail-verify@$VA: workflow(s) gitleaks did not exist at this sha"
good_tree "$D"; fx "$D" "repos/elzainxx/coldmail-platform/contents/.github/workflows/web.yml?ref=$FULLPA" $'garbage\n'; fx_list "$D" coldmail-platform "$PA" "harness:success db-tests:success supabase:success go:success gates:success gitleaks:success platform-check:success"
expect "a workflow file that is there (any answer but 404) is not waived" "$D" 1 "missing a successful run of: web"

# ---- the head does not resolve ---------------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; fx_rm "$D" "repos/elzainxx/coldmail-platform/commits/$PA"
expect "a head that is no commit (404)" "$D" 1 "RED: $P0L coldmail-platform@$PA does not resolve to a commit in elzainxx/coldmail-platform (HTTP 404)"
good_tree "$D"; fx_rm "$D" "repos/elzainxx/coldmail-platform/commits/$PA"; fx "$D" "repos/elzainxx/coldmail-platform/commits/$PA.422" ""
expect "a head the API cannot resolve (422)" "$D" 1 "RED: $P0L coldmail-platform@$PA does not resolve to a commit in elzainxx/coldmail-platform (HTTP 422)"
good_tree "$D"; fx "$D" "repos/elzainxx/coldmail-platform/commits/$PA" "$FULLPB"$'\n'
expect "an API answer that does not start with the cited hex is refused" "$D" 1 "RED: $P0L coldmail-platform@$PA: the API answered '$FULLPB' where a full 40-hex sha starting with the cited hex was expected"
good_tree "$D"; fx_rm "$D" "repos/elzainxx/coldmail-platform/actions/runs?head_sha=$FULLPA&per_page=100"
expect "the runs of a sha cannot be listed (404: the token cannot read Actions)" "$D" 1 "RED: $P0L coldmail-platform@$PA ($FULLPA): the runs of this sha could not be listed (HTTP 404)"
good_tree "$D"; fx "$D" "repos/elzainxx/coldmail-platform/actions/runs?head_sha=$FULLPA&per_page=100" $'3\n'
expect "the API says 3 runs and the gate parses none: the instrument matched too little" "$D" 1 "RED: $P0L coldmail-platform@$PA ($FULLPA): the API reported 3 run(s) and the gate parsed 0 of the 3 it should have read"
good_tree "$D"; fx "$D" "repos/elzainxx/coldmail-platform/actions/runs?head_sha=$FULLPA&per_page=100" $'not json at all\n'
expect "an answer whose first line is not a run count" "$D" 1 "the first line of the answer is 'not json at all', not a run count"
good_tree "$D"; fx "$D" "repos/elzainxx/coldmail-platform/actions/runs/1001" $'garbage\n'
expect "a cited run whose answer is not one run" "$D" 1 "RED: $P0L run 1001 in elzainxx/coldmail-platform: the API answered something that is not one run ('garbage')"

# ---- the API is unreachable: red with the reason, never green -----------------------------------------------------------------------------------------------------------------------
good_tree "$D"; echo "gh: error connecting to api.github.com" > "$D/fix/__down"
expect "the network is down" "$D" 1 "check-verified-heads: RED — the GitHub API is unreachable or refused the request" "error connecting to api.github.com" "the call was: api repos/elzainxx/coldmail-platform/commits/$PA" "a gate that cannot reach the API proves nothing and is never green" "NOT CHECKED:"
good_tree "$D"; echo "gh: API rate limit exceeded for user (HTTP 403)" > "$D/fix/__down"
expect "a rate limit" "$D" 1 "check-verified-heads: RED — the GitHub API is unreachable or refused the request" "API rate limit exceeded"
good_tree "$D"; echo "gh: To use GitHub CLI in automation, set the GH_TOKEN environment variable." > "$D/fix/__down"
expect "no token" "$D" 1 "check-verified-heads: RED — the GitHub API is unreachable or refused the request" "set the GH_TOKEN environment variable"
good_tree "$D"; CMD_ARGS=(--api-cmd /no/such/command); expect "an api command that does not exist" "$D" 1 "check-verified-heads: RED — the api command '/no/such/command' was not found"; CMD_ARGS=(--api-cmd "$STUB")

# ---- the documents and the floors ----------------------------------------------------------------------------------------------------------------------------------------------------------
good_tree "$D"; printf '# nothing here\n' > "$D/$LIST_REL"; expect "a list that names no document is red" "$D" 1 "check-verified-heads: RED — $LIST_REL lists 0 documents: the instrument matched nothing"
good_tree "$D"; : > "$D/$LIST_REL"; expect "an empty list is red" "$D" 1 "$LIST_REL lists 0 documents"
good_tree "$D"; rm "$D/$LIST_REL"; expect "no list" "$D" 1 "check-verified-heads: RED — $LIST_REL does not exist"
good_tree "$D"; printf '%s\n' "$PLAN_REL" "$CLOSE_REL" "Docs/no-such.md" > "$D/$LIST_REL"; expect "a listed document that does not exist" "$D" 1 "RED: $LIST_REL names Docs/no-such.md, which does not exist"
good_tree "$D"; printf '%s\n' "$CLOSE_REL" > "$D/$LIST_REL"; expect "a list that drops the build plan" "$D" 1 "RED: $LIST_REL does not name $PLAN_REL"
good_tree "$D"; printf '%s\n' "$PLAN_REL" "$PLAN_REL" "$CLOSE_REL" > "$D/$LIST_REL"; expect "a document listed twice" "$D" 1 "RED: $LIST_REL lists $PLAN_REL twice"
good_tree "$D"; printf '%s\n' "$PLAN_REL" "../elsewhere.md" "$CLOSE_REL" > "$D/$LIST_REL"; expect "a listed path that leaves the repository" "$D" 1 "RED: $LIST_REL names '../elsewhere.md', which is not a path below the repository root"
good_tree "$D"; : > "$D/$CLOSE_REL"; expect "an empty document" "$D" 1 "RED: $CLOSE_REL is empty: the gate matched nothing"
good_tree "$D"; printf '# close-out\n\nno table here\n' > "$D/$CLOSE_REL"; expect "a document with no status table" "$D" 1 "RED: $CLOSE_REL: found 0 status tables (a table whose header ends Status, or Status | Evidence): the instrument matched nothing"
good_tree "$D"; printf '# close-out\n\n| # | Item | Exit check | Status | Evidence |\n|---|---|---|---|---|\n\nno rows\n' > "$D/$CLOSE_REL"; expect "a status table without rows" "$D" 1 "$CLOSE_REL: found 0 table rows under its status table(s): the instrument matched nothing"
good_tree "$D"; printf '# close-out\n\n```\n| # | Item | Exit check | Status | Evidence |\n|---|---|---|---|---|\n| A1 | x | y | CLOSED | z |\n```\n' > "$D/$CLOSE_REL"; expect "a table inside a code fence is not a table" "$D" 1 "$CLOSE_REL: found 0 status tables"
good_tree "$D" P0_STATUS="IN PROGRESS" A1_STATUS="IN PROGRESS" C1_STATUS="OPEN"; expect "no claim anywhere is red (the instrument matched nothing)" "$D" 1 "found 0 claims in 2 document(s) — the instrument matched nothing" "claims: 0,"
good_tree "$D" P0_STATUS="IN PROGRESS $EM nearly done, see the close-out"; expect "a document with 0 claims whose status mentions done outside the claim forms is red" "$D" 1 \
  "$PLAN_REL: found 0 claims but 1 status cell(s) mention done/closed/green/verified/complete outside the claim forms" "note  $PLAN_REL:7 P0 Groundwork: the status mentions done/closed/green/verified/complete but is not in a claim form"
good_tree "$D" A1_STATUS="CLOSED (runner pending)"; expect "a qualified CLOSED is ambiguous, not a claim and not a skip" "$D" 1 "RED: $A1L the status 'CLOSED (runner pending)' cannot be classified" "a qualified status is ambiguous, so say CLOSED or IN PROGRESS"
good_tree "$D" A1_STATUS="finished"; expect "an unknown status word is reported, never guessed" "$D" 1 "RED: $A1L the status 'finished' cannot be classified: it is neither a claim"
good_tree "$D" A1_EVID="verify@$VA | and a pipe nobody escaped"; expect "an unescaped pipe in a cell: the row cannot be parsed and is reported with its line" "$D" 1 "RED: $CLOSE_REL:7 (row): malformed table row: the row has 6 cell(s) and the header (line 5) has 5: an unescaped | inside a cell? write it as \\|"
good_tree "$D" A1_EVID="" C1_EVID=""; expect "two violations in two rows are both reported, each with its file, line and row id" "$D" 1 "RED: $A1L a claim must cite the head and the run: no head token found" "RED: $C1L a claim must cite the head and the run: no head token found"

# ---- usage -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
out="$(bash "$GATE" --bogus 2>&1)"; rc=$?; t
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi
good_tree "$D"; rm -rf "$D"; expect "a root that is not a directory" "$D" 1 "is not a directory: the instrument matched nothing"

# ---- the controls: this selftest must be able to fail -------------------------------------------------------------------------------------------------------------------------------------
# Each control swaps the gate for a fake, runs one real case against it, and requires the case to MISBEHAVE (and then forgives it: it is the control).
fake="$TMP/fake.sh"
run_control() { # run_control LABEL FAKE-GATE-TEXT CASE-FUNCTION ARGS...
  local label="$1" real before; printf '%s\n' "$2" > "$fake"; shift 2     # the case it runs (expect) counts itself
  real="$GATE"; GATE="$fake"; before=$bad
  "$@" > /dev/null
  GATE="$real"
  if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: $label"; else echo "  FAIL control: $label — the selftest did not notice"; bad=$((bad+1)); fi
}
mutant() { # mutant SED-SCRIPT: the real gate with one check changed; the mutant must differ from the gate, or the control proves nothing
  local m; m="$(sed "$1" "$GATE")"
  if [ "$m" = "$(cat "$GATE")" ]; then echo "  FAIL control: the sed script '$1' changes nothing in the gate, so the mutant is the gate itself" >&2; bad=$((bad+1)); fi
  printf '%s' "$m"
}
good_tree "$D" P0_STATUS="**done** $EM every workflow green: [harness run 1001]($PLAT/1001)"
run_control "a gate that always passes makes the 'claim without a head' case misbehave" '#!/usr/bin/env bash
echo "check-verified-heads: OK"; exit 0' expect "a claim with a run and no head token" "$D" 1 "a claim must cite the head and the run: no head token found"
good_tree "$D"
run_control "a gate that always fails makes the 'right tree is green' case misbehave" '#!/usr/bin/env bash
echo "check-verified-heads: RED"; exit 1' expect "a right tree is green" "$D" 0 "check-verified-heads: OK"
good_tree "$D"; fx_rm "$D" repos/elzainxx/coldmail-platform/actions/runs/1001
run_control "a gate that fails for another reason makes the 'run 404' case misbehave (class 10)" '#!/usr/bin/env bash
echo "check-verified-heads: RED — something unrelated"; exit 1' expect "a cited run that does not exist (404)" "$D" 1 "does not exist in elzainxx/coldmail-platform (HTTP 404"
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness completed success "$PB" 1
run_control "a copy of the gate without its sha comparison makes the 'run for a different sha' case misbehave" "$(mutant 's/^sha_equal() {.*/sha_equal() { true; }/')" \
  expect "a cited run for another sha" "$D" 1 "run 1001 (harness) is for sha $FULLPB, not for the head the row claims (coldmail-platform@$PA)"
good_tree "$D"; fx_run "$D" coldmail-platform 1001 harness completed failure "$PA" 1
run_control "a copy of the gate without its conclusion check makes the 'run concluded failure' case misbehave" "$(mutant 's/^conclusion_ok() {.*/conclusion_ok() { true; }/')" \
  expect "a cited run that concluded failure" "$D" 1 "run 1001 (harness) concluded 'failure', not success"
good_tree "$D"; echo "gh: error connecting to api.github.com" > "$D/fix/__down"
run_control "a copy of the gate that takes an unreachable API for 'not found' makes the 'network is down' case misbehave" "$(mutant 's/\*) die_api "\$1";;/*) A_RC=4;;/')" \
  expect "the network is down" "$D" 1 "check-verified-heads: RED — the GitHub API is unreachable or refused the request"
good_tree "$D" P0_STATUS="IN PROGRESS" A1_STATUS="IN PROGRESS" C1_STATUS="OPEN"
run_control "a copy of the gate without its zero-claims floor makes the 'no claim anywhere' case misbehave" "$(mutant 's/^if \[ "\$NCLAIMS" -eq 0 \]; then bad /if false; then bad /')" \
  expect "no claim anywhere is red" "$D" 1 "found 0 claims in 2 document(s) — the instrument matched nothing"
good_tree "$D"; fx_list "$D" coldmail-platform "$PA" "harness:success db-tests:success supabase:success go:success gates:success gitleaks:success web:success"
run_control "a copy of the gate that waives every missing workflow makes the 'platform-check never ran' case misbehave" "$(mutant 's/^waivable() {.*/waivable() { return 0; }/')" \
  expect "platform-check never ran on the sha" "$D" 1 "missing a successful run of: platform-check"
good_tree "$D"; fx_list "$D" coldmail-platform "$PA" "go:success"
run_control "a copy of the gate that requires no workflow makes the 'only go ran' case misbehave" "$(mutant 's/^    required_for "\$repo"; missing=""; waived=""/    NEED=""; missing=""; waived=""/')" \
  expect "only go ran (harness and db-tests are cited, the rest never ran)" "$D" 1 "missing a successful run of: supabase, gates, gitleaks, web, platform-check (runs on this sha: go=success)"

echo "selftest: $n cases"
[ "$n" -ge "$MIN_CASES" ] || { echo "check-verified-heads-selftest: RED — only $n cases ran (minimum $MIN_CASES)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-verified-heads-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-verified-heads-selftest: OK"
