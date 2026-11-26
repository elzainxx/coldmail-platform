#!/usr/bin/env bash
# check-install-doc-selftest.sh — proves scripts/check-install-doc.sh goes red, FOR THE REASON IT NAMES, on every way the install document can be wrong, and green on a right one.
# Every case builds a throwaway tree, runs the real gate on it and asserts the exit code AND a fragment of the printed reason (class 10: a reject case that fails for another reason is no proof).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-install-doc.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
n=0; bad=0
t() { n=$((n+1)); }
# good_tree DIR: a minimal tree the gate accepts
good_tree() {
  d="$1"; rm -rf "$d"; mkdir -p "$d/Docs" "$d/db/install" "$d/cmd/platform-migrate" "$d/tests/harness" "$d/.github/workflows" "$d/scripts"
  cat > "$d/Docs/install.md" <<'EOF'
# Install
Step 2: `db/install/preprovision.sql` is required. Step 3: `platform-migrate up-and-set-password`. Step 4: `platform-migrate create-first-admin`.
Step 3b: `db/install/postmodule1.sql` runs after module 1.
Step 5: remove public from the Exposed schemas. Module 1 needs TM_QUEUE_ADDR_KEY. The vendor price is m1_vendor.
The migrator reference is `scripts/check-install-doc.sh`.
EOF
  : > "$d/db/install/preprovision.sql"; : > "$d/db/install/postmodule1.sql"
  printf 'case "create-first-admin": case "up-and-set-password":\n' > "$d/cmd/platform-migrate/main.go"
  printf '# preprovision\n- ${H_PLATFORM_DIR}/db/install/preprovision.sql:/preprovision.sql:ro\n' > "$d/tests/harness/docker-compose.yml"
  printf 'psql -f "$ROOT/db/install/preprovision.sql"\n' > "$d/tests/harness/run-db-tests.sh"
  printf 'preprovision: db/install/preprovision.sql\npostmodule1: db/install/postmodule1.sql\n' > "$d/.github/workflows/supabase.yml"
  printf 'psql -f db/install/postmodule1.sql\n' > "$d/tests/harness/run.sh"
  : > "$d/scripts/check-install-doc.sh"
}
# expect NAME DIR WANT_RC FRAGMENT
expect() {
  t; name="$1"; d="$2"; want="$3"; frag="$4"
  out="$(bash "$GATE" --root "$d" 2>&1)"; rc=$?
  if [ "$rc" != "$want" ]; then echo "  FAIL $name: exit $rc, want $want"; echo "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  if [ -n "$frag" ] && ! printf '%s\n' "$out" | grep -qF -- "$frag"; then echo "  FAIL $name: exit $rc but the output lacks '$frag'"; echo "$out" | sed 's/^/     | /'; bad=$((bad+1)); return; fi
  echo "  ok   $name"
}

D="$TMP/t"
good_tree "$D"; expect "a right tree is green" "$D" 0 "check-install-doc: OK"
good_tree "$D"; expect "the green run states the counts" "$D" 0 "7 required item(s)"
good_tree "$D"; expect "the green run prints what it did not check" "$D" 0 "NOT CHECKED: that the prose is true"

good_tree "$D"; rm "$D/Docs/install.md"; expect "no install document" "$D" 1 "does not exist: the install procedure is a required document"
good_tree "$D"; : > "$D/Docs/install.md"; expect "an empty document (the subject is empty)" "$D" 1 "is empty: the gate matched nothing"
for item in 'db/install/preprovision.sql' 'db/install/postmodule1.sql' 'up-and-set-password' 'create-first-admin' 'Exposed schemas' 'TM_QUEUE_ADDR_KEY' 'm1_vendor'; do
  good_tree "$D"; sed -i "s#$item#REMOVED#g" "$D/Docs/install.md"; expect "the document stops mentioning $item" "$D" 1 "does not mention '$item'"
done
good_tree "$D"; printf 'Also see `db/install/missing.sql`.\n' >> "$D/Docs/install.md"; expect "a backticked path that does not exist" "$D" 1 "names 'db/install/missing.sql' and no such path exists"
good_tree "$D"; sed -i 's#`db/install/preprovision.sql`#preprovision (no path)#; s#`db/install/postmodule1.sql`#postmodule1 (no path)#; s#`scripts/check-install-doc.sh`#nothing#' "$D/Docs/install.md"; printf 'x db/install/preprovision.sql db/install/postmodule1.sql x
' >> "$D/Docs/install.md"
expect "no backticked path at all (the path check matched nothing)" "$D" 1 "found 0 backticked repository paths"
good_tree "$D"; printf 'case "up-and-set-password":\n' > "$D/cmd/platform-migrate/main.go"; expect "create-first-admin is not a subcommand" "$D" 1 "has a \"create-first-admin\" subcommand but Docs/install.md documents it"
good_tree "$D"; rm "$D/cmd/platform-migrate/main.go"; expect "no migrator source" "$D" 1 "cmd/platform-migrate/main.go does not exist"
good_tree "$D"; : > "$D/tests/harness/docker-compose.yml"; expect "the harness compose file does not run the step" "$D" 1 "tests/harness/docker-compose.yml does not reference db/install/preprovision.sql"
good_tree "$D"; : > "$D/tests/harness/run-db-tests.sh"; expect "db-tests does not run the step" "$D" 1 "tests/harness/run-db-tests.sh does not reference db/install/preprovision.sql"
good_tree "$D"; : > "$D/.github/workflows/supabase.yml"; expect "the supabase workflow does not run the step" "$D" 1 ".github/workflows/supabase.yml does not reference db/install/preprovision.sql"
good_tree "$D"; printf 'tests/supabase/preprovision.sql db/install/preprovision.sql\n' >> "$D/tests/harness/run-db-tests.sh"; expect "a runner still names the old location" "$D" 1 "still references the old location tests/supabase/preprovision.sql"
good_tree "$D"; rm "$D/db/install/preprovision.sql"; expect "the provisioning file is gone" "$D" 1 "db/install/preprovision.sql does not exist"
good_tree "$D"; : > "$D/tests/harness/run.sh"; expect "the harness run script does not run the step after module 1's migrations" "$D" 1 "tests/harness/run.sh does not reference db/install/postmodule1.sql"
good_tree "$D"; printf 'preprovision: db/install/preprovision.sql\n' > "$D/.github/workflows/supabase.yml"; expect "the supabase workflow does not run the step after module 1's migrations" "$D" 1 ".github/workflows/supabase.yml does not reference db/install/postmodule1.sql"
good_tree "$D"; rm "$D/tests/harness/run.sh"; expect "the harness run script is gone" "$D" 1 "tests/harness/run.sh does not exist"
good_tree "$D"; rm "$D/db/install/postmodule1.sql"; expect "the post-module-1 install file is gone" "$D" 1 "db/install/postmodule1.sql does not exist"
out="$(bash "$GATE" --bogus 2>&1)"; rc=$?; t
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "unknown argument"; then echo "  ok   an unknown argument is a usage error (exit 2)"; else echo "  FAIL unknown argument: exit $rc"; bad=$((bad+1)); fi

# the control: the selftest itself must be able to fail. Run one reject case against a gate that always passes: the case MUST register as misbehaving (and is then forgiven, because it is the control)
t; good_tree "$D"; rm "$D/Docs/install.md"
fake="$TMP/always-ok.sh"; printf '#!/usr/bin/env bash\necho "check-install-doc: OK"\nexit 0\n' > "$fake"
REAL_GATE="$GATE"; GATE="$fake"; before=$bad
expect "(control) no install document, against a gate that always passes" "$D" 1 "does not exist: the install procedure is a required document" > /dev/null
GATE="$REAL_GATE"
if [ "$bad" -gt "$before" ]; then bad="$before"; echo "  ok   control: a gate that always passes makes the 'no install document' case misbehave, so the cases above can fail"; else echo "  FAIL control: the selftest did not notice a gate that always passes"; bad=$((bad+1)); fi
echo "selftest: $n cases"
[ "$n" -ge 20 ] || { echo "check-install-doc-selftest: RED — only $n cases ran (minimum 20)"; exit 1; }
[ "$bad" -eq 0 ] || { echo "check-install-doc-selftest: RED — $bad case(s) misbehaved"; exit 1; }
echo "check-install-doc-selftest: OK"
