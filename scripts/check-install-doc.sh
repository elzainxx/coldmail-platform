#!/usr/bin/env bash
# check-install-doc.sh [--root DIR] — BLOCKING gate: Docs/install.md names only what exists, and CI runs the install step it documents.
#
#   1. the document names each REQUIRED item (the provisioning file, the platform migrator's command, the first-admin command, the exposed-schemas setting, the module-1 queue key, the vendor price):
#      a document that stops mentioning one of them is red (class 5: a gate satisfied by the absence of the thing it guards)
#   2. every file path the document puts in backticks (db/…, scripts/…, cmd/…, tests/…) exists in the repository
#   3. the commands it documents exist: `create-first-admin` is a subcommand of cmd/platform-migrate, `up-and-set-password` is too
#   4. CI RUNS THE STEP EXACTLY AS THE DOCUMENT SAYS: db/install/preprovision.sql is referenced by the harness compose file, by run-db-tests.sh and by the supabase workflow, and the OLD location
#      (tests/supabase/preprovision.sql) is referenced by none of the files that run it — so the install step cannot silently stop being exercised
#
# NOT CHECKED (printed on every run): that the prose is true, that a step works against a real project (the supabase and harness workflows run the steps; the dashboard steps are an owner's), or
# that nothing else is missing from the document. It reads names and paths.
set -u
ROOT="."
while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="${2:?--root needs a directory}"; shift 2;;
    *) echo "check-install-doc: unknown argument '$1'" >&2; exit 2;;
  esac
done
DOC="$ROOT/Docs/install.md"
fail=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }
[ -f "$DOC" ] || { echo "check-install-doc: RED — $DOC does not exist: the install procedure is a required document"; exit 1; }
text="$(tr -d '\r' < "$DOC")"
[ -n "$text" ] || { echo "check-install-doc: RED — $DOC is empty: the gate matched nothing"; exit 1; }

# 1. required items
nreq=0
for item in 'db/install/preprovision.sql' 'up-and-set-password' 'create-first-admin' 'Exposed schemas' 'TM_QUEUE_ADDR_KEY' 'm1_vendor'; do
  nreq=$((nreq+1))
  printf '%s\n' "$text" | grep -qF -- "$item" || bad "Docs/install.md does not mention '$item' (a required item of the install procedure)"
done

# 2. every backticked repository path exists
npaths=0
paths="$(printf '%s\n' "$text" | grep -oE '`(db|scripts|cmd|tests|ci|tools)/[A-Za-z0-9_./-]+`' | tr -d '`' | sort -u)"
for p in $paths; do
  npaths=$((npaths+1))
  [ -e "$ROOT/$p" ] || bad "Docs/install.md names '$p' and no such path exists"
done
[ "$npaths" -ge 1 ] || bad "found 0 backticked repository paths in Docs/install.md: the gate matched nothing (it must name at least db/install/preprovision.sql)"

# 3. the documented commands exist
MAIN="$ROOT/cmd/platform-migrate/main.go"; CFG="$ROOT/internal/migrate/config.go"   # the command names are constants in config.go that main.go dispatches on; either file may carry the literal
ncmd=0
if [ -f "$MAIN" ]; then
  for c in create-first-admin up-and-set-password; do
    ncmd=$((ncmd+1))
    { grep -qF "\"$c\"" "$MAIN" || { [ -f "$CFG" ] && grep -qF "\"$c\"" "$CFG"; }; } || bad "neither cmd/platform-migrate/main.go nor internal/migrate/config.go has a \"$c\" subcommand but Docs/install.md documents it"
  done
else
  bad "cmd/platform-migrate/main.go does not exist"
fi

# 4. CI runs the documented step; the old location is gone from every runner
nci=0
for f in tests/harness/docker-compose.yml tests/harness/run-db-tests.sh .github/workflows/supabase.yml; do
  nci=$((nci+1))
  if [ ! -f "$ROOT/$f" ]; then bad "$f does not exist"; continue; fi
  grep -qF 'db/install/preprovision.sql' "$ROOT/$f" || bad "$f does not reference db/install/preprovision.sql: CI would not run the documented install step"
  if grep -qF 'tests/supabase/preprovision.sql' "$ROOT/$f"; then bad "$f still references the old location tests/supabase/preprovision.sql"; fi
done
[ -f "$ROOT/db/install/preprovision.sql" ] || bad "db/install/preprovision.sql does not exist"

echo "check-install-doc: $nreq required item(s), $npaths backticked path(s), $ncmd command(s), $nci CI file(s) checked"
echo "NOT CHECKED: that the prose is true; that a step works against a real project (the supabase and harness workflows run the steps; the dashboard steps are the owner's); that nothing else is missing"
if [ "$fail" -gt 0 ]; then echo "check-install-doc: RED — $fail problem(s)"; exit 1; fi
echo "check-install-doc: OK"
