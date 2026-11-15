#!/usr/bin/env bash
# check-module-reads-selftest.sh — proves scripts/check-module-reads.sh is green on module sources that match their declaration and RED, FOR THE REASON IT NAMES, on each way the code can drift
# away from it. It builds two tiny fixture modules (a stand-in for module 2 and module 3) in a temp directory, so it needs no module checkout and no database; the gate on the REAL pinned modules
# is a separate step (tests/harness/run.sh, stage 1). Every reject case asserts the reason text (a case that went red for an unrelated reason would read as a pass forever: class 10), the selftest
# has a hard minimum number of cases, and a control proves the harness itself can say "no": two deliberately wrong expectations must be reported as mismatches.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; GATE="$HERE/check-module-reads.sh"
T="$(mktemp -d)" || exit 2; trap 'rm -rf "$T"' EXIT
MIN_CASES=24; cases=0; ok=0; bad_expect=0

U1='SELECT EXISTS(SELECT 1 FROM public.users WHERE id=$1)'
J2='INSERT INTO m2.jobs (job_item_id, campaign_id) SELECT ji.id, ji.campaign_id FROM public.job_items ji WHERE ji.id = $1 AND ji.user_id = $2 RETURNING id'
J4='INSERT INTO public.job_items (campaign_id, user_id, status) VALUES ($1, $1, '"'pending'"') RETURNING id'
STORE="services/control/store.go"

mk() { # mk <name> — a fresh fixture: modules root $d/mods (two modules, each with a store.go that matches the declaration) and $d/stm; echoes the directory
  d="$T/$1"; rm -rf "$d"
  m2="$d/mods/coldmail-send-managed"; m3="$d/mods/coldmail-send-byo"
  mkdir -p "$m2/services/control" "$m3/services/control" "$m2/tests/harness" "$m2/scripts" "$m2/Docs"
  cat > "$m2/$STORE" <<GO
package control

// This comment names public.users and public.job_items. A comment is not a reference.
const userExists = "$U1"

const freeze = \`INSERT INTO m2.jobs
		   (job_item_id, campaign_id)
		 SELECT ji.id, ji.campaign_id
		 FROM public.job_items ji
		 WHERE ji.id = \$1 AND ji.user_id = \$2
		 RETURNING id\`
GO
  cat > "$m2/services/control/other.go" <<GO
package control

const nothing = "SELECT 1 FROM m2.config"
GO
  cat > "$m3/$STORE" <<GO
package control

const userExists = "$U1"

const testSend = \`INSERT INTO public.job_items (campaign_id, user_id, status)
		 VALUES (\$1, \$1, 'pending') RETURNING id\`
GO
  cat > "$d/stm" <<DECL
# fixture declaration
module-dir|m2|coldmail-send-managed
module-dir|m3|coldmail-send-byo
m2|send_managed_app|U1|$STORE|$U1
m2|send_managed_app|J2|$STORE|$J2
m3|send_byo_app|U1|$STORE|$U1
m3|send_byo_app|J4|$STORE|$J4
DECL
  echo "$d"
}

# expect <label> <want-rc> <want-substring|-> <fixture-dir> [extra gate args replacing the default ones]
expect() {
  label="$1"; want_rc="$2"; sub="$3"; d="$4"; shift 4
  cases=$((cases+1))
  if [ $# -gt 0 ]; then out="$(bash "$GATE" "$@" 2>&1)"; rc=$?; else out="$(bash "$GATE" --modules "$d/mods" --statements "$d/stm" 2>&1)"; rc=$?; fi
  if [ "$rc" -ne "$want_rc" ]; then echo "  MISMATCH [$label]: rc=$rc want=$want_rc"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return; fi
  if [ "$sub" != "-" ]; then
    n=$(printf '%s\n' "$out" | grep -c -F -- "$sub" || true)
    if [ "$n" -lt 1 ]; then echo "  MISMATCH [$label]: rc=$rc as expected, but not for the named reason '$sub'"; printf '%s\n' "$out" | sed 's/^/      | /'; bad_expect=$((bad_expect+1)); return; fi
  fi
  ok=$((ok+1)); echo "  ok [$label] rc=$rc${sub:+ — ${sub}}" | sed 's/ — -$//'
}
M2="mods/coldmail-send-managed"; M3="mods/coldmail-send-byo"

d=$(mk clean);   expect "clean: every statement present, every reference declared" 0 "check-module-reads: OK" "$d"
d=$(mk clean2);  expect "clean prints how much it did not check" 0 "NOT CHECKED: SQL in the modules' migrations" "$d"
d=$(mk crlf); for f in "$d/stm" "$d/$M2/$STORE" "$d/$M3/$STORE"; do sed -i 's/$/\r/' "$f"; done
                 expect "clean with CRLF line endings in the sources and the declaration" 0 "check-module-reads: OK" "$d"
d=$(mk reflow); printf 'package control\n\nconst userExists = `SELECT   EXISTS(SELECT 1\n\tFROM public.users\n\t\tWHERE id=$1)`\n\nconst freeze = `INSERT INTO m2.jobs (job_item_id, campaign_id)  SELECT ji.id, ji.campaign_id FROM public.job_items ji WHERE ji.id = $1 AND ji.user_id = $2 RETURNING id`\n' > "$d/$M2/$STORE"
                 expect "a reformatted (re-wrapped, tabbed) but identical statement is NOT drift" 0 "check-module-reads: OK" "$d"
d=$(mk changed); sed -i 's/WHERE id=\$1)/WHERE id = $2)/' "$d/$M2/$STORE"
                 expect "a statement was changed in the source" 1 "DRIFT: U1 no longer appears in services/control/store.go" "$d"
d=$(mk changedm3); sed -i "s/VALUES (\\\$1, \\\$1, 'pending')/VALUES (\$1, \$2, 'pending')/" "$d/$M3/$STORE"
                 expect "a statement changed in module 3 (its own module is named)" 1 "DRIFT: J4 no longer appears in services/control/store.go (module m3)" "$d"
d=$(mk newfile); printf 'package control\n\nconst leak = "SELECT email FROM public.users WHERE id = $1"\n' > "$d/$M2/services/control/leak.go"
                 expect "a new undeclared file references public.users" 1 "DRIFT: services/control/leak.go references public.users/public.job_items but is not declared" "$d"
d=$(mk newfile2); printf 'package control\n\nconst leak = `UPDATE public.job_items SET status = $2 WHERE id = $1`\n' > "$d/$M3/services/control/writer.go"
                 expect "a new undeclared file WRITES public.job_items" 1 "DRIFT: services/control/writer.go references public.users/public.job_items but is not declared" "$d"
d=$(mk testfile); printf 'package control\n\nconst seed = "INSERT INTO public.users (email) VALUES ($1)"\n' > "$d/$M2/services/control/store_test.go"
                 expect "a reference in a *_test.go file is ignored" 0 "check-module-reads: OK" "$d"
d=$(mk testdirs); for x in tests/harness scripts Docs; do mkdir -p "$d/$M2/$x"; printf 'package x\n\nconst q = "SELECT 1 FROM public.users"\n' > "$d/$M2/$x/helper.go"; done
                 expect "references under tests/, scripts/ and Docs/ are ignored" 0 "check-module-reads: OK" "$d"
d=$(mk comment); printf 'package control\n\n// SELECT 1 FROM public.users is described here, nothing more.\n\tconst x = 1\n' > "$d/$M2/services/control/notes.go"
                 expect "a file that only mentions the tables in a // comment is ignored" 0 "check-module-reads: OK" "$d"
d=$(mk extra); printf '\nconst more = "SELECT created_at FROM public.users WHERE id = $1"\n' >> "$d/$M2/$STORE"
                 expect "a new statement inside a DECLARED file is not covered by its declaration" 1 "DRIFT: services/control/store.go has 1 reference(s) to public.users/public.job_items outside its declared statements" "$d"
d=$(mk unq); printf 'package control\n\nconst q = "SELECT id FROM users WHERE id = $1"\n' > "$d/$M2/services/control/unq.go"
                 expect "an unqualified FROM users is flagged (search_path would resolve it)" 1 "DRIFT: services/control/unq.go names users/job_items without a schema" "$d"
d=$(mk commented); sed -i 's|^const userExists = |// const userExists = |' "$d/$M2/$STORE"
                 expect "a declared statement that was commented out is gone" 1 "DRIFT: U1 no longer appears in services/control/store.go" "$d"
d=$(mk twice); printf '%s\n' "m2|send_managed_app|U2|$STORE|$U1" >> "$d/stm"
                 expect "a statement declared twice that appears once (a call site was removed)" 1 "DRIFT: U2 no longer appears in services/control/store.go (module m2) as many times as it is declared (found 1, declared 2)" "$d"
d=$(mk twiceok); printf '\nconst again = "%s"\n' "$U1" >> "$d/$M2/$STORE"; printf '%s\n' "m2|send_managed_app|U2|$STORE|$U1" >> "$d/stm"
                 expect "the same statement at two call sites, both declared, is clean" 0 "check-module-reads: OK" "$d"
d=$(mk nofile); rm "$d/$M3/$STORE"; printf 'package control\n' > "$d/$M3/services/control/stub.go"
                 expect "a declared file no longer exists" 1 "DRIFT: J4 no longer appears in services/control/store.go (module m3: the file does not exist)" "$d"
d=$(mk empty); rm -rf "$d/$M3/services"; mkdir -p "$d/$M3/services"
                 expect "a module directory with no Go files (the instrument matched nothing)" 1 "module m3: found 0 Go files under coldmail-send-byo" "$d"
d=$(mk nodir); rm -rf "$d/$M3"
                 expect "a module directory that does not exist" 1 "module m3: the directory" "$d"
d=$(mk nostm); rm "$d/stm"
                 expect "a missing statements file is red, not a pass" 1 "the statements file $d/stm does not exist" "$d"
d=$(mk nostmts); printf 'module-dir|m2|coldmail-send-managed\nmodule-dir|m3|coldmail-send-byo\nm2|send_managed_app|U1|%s|%s\n' "$STORE" "$U1" > "$d/stm"
                 expect "a module with zero declared statements fails the floor" 1 "module m3: 0 declared statements (floor is 1)" "$d"
d=$(mk nomods); printf '# nothing declared\n' > "$d/stm"
                 expect "a declaration with no module at all" 1 "declares no module" "$d"
d=$(mk malformed); printf 'm2|send_managed_app|U9|%s\n' "$STORE" >> "$d/stm"
                 expect "a malformed statement line (4 fields)" 1 "malformed line" "$d"
d=$(mk testdecl); printf 'm2|send_managed_app|U9|services/control/store_test.go|%s\n' "$U1" >> "$d/stm"
                 expect "declaring a test file is refused" 1 "is a test file" "$d"
d=$(mk dup); printf 'm2|send_managed_app|U1|%s|%s\n' "$STORE" "$U1" >> "$d/stm"
                 expect "a label declared twice for one module" 1 "label U1 is declared twice for module m2" "$d"
d=$(mk ref); printf 'm2|ref|P1|services/control/ref.go|preflight: OK (public.users + public.job_items exist)\n' >> "$d/stm"; printf 'package control\n\nfunc f() { println("preflight: OK (public.users + public.job_items exist)") }\n' > "$d/$M2/services/control/ref.go"
                 expect "a declared non-SQL reference (a log line) is accepted" 0 "check-module-reads: OK" "$d"
sed -i 's/preflight: OK/preflight: FINE/' "$d/$M2/services/control/ref.go"
                 expect "the log line changed: the reference is now undeclared drift" 1 "DRIFT: P1 no longer appears in services/control/ref.go" "$d"
d=$(mk usage); cases=$((cases+1)); out="$(bash "$GATE" --statements "$d/stm" 2>&1)"; rc=$?
if [ "$rc" -eq 2 ] && printf '%s' "$out" | grep -q "usage: check-module-reads.sh --modules DIR --statements FILE"; then ok=$((ok+1)); echo "  ok [missing --modules is a usage error] rc=2"; else bad_expect=$((bad_expect+1)); echo "  MISMATCH [missing --modules is a usage error]: rc=$rc"; fi

# ---- the control for THIS file: prove the harness can say "no" ---------------------------------------------------------------------------------------------------------------------
# Two deliberately WRONG expectations, run through the same `expect`: a clean fixture expected to be red, and a drifted fixture expected to be red for a reason it does not give. BOTH must be
# reported as mismatches; if either is accepted, this selftest cannot fail and says so.
before_bad=$bad_expect; before_ok=$ok; before_cases=$cases
d=$(mk ctl1);                 expect "CONTROL 1 (wrong on purpose): a clean fixture expected to be RED" 1 "DRIFT" "$d" >/dev/null 2>&1
d=$(mk ctl2); sed -i 's/WHERE id=\$1)/WHERE id = $2)/' "$d/$M2/$STORE"
                              expect "CONTROL 2 (wrong on purpose): drift expected for a reason it does not give" 1 "references public.users/public.job_items but is not declared" "$d" >/dev/null 2>&1
caught=$((bad_expect - before_bad))
bad_expect=$before_bad; ok=$before_ok; cases=$before_cases
if [ "$caught" -eq 2 ]; then cases=$((cases+1)); ok=$((ok+1)); echo "  ok [selftest control] both deliberately wrong expectations were reported as mismatches"
else cases=$((cases+1)); bad_expect=$((bad_expect+1)); echo "  MISMATCH [selftest control]: only $caught of 2 wrong expectations were caught — this selftest cannot fail"; fi

echo "selftest: $cases cases, $ok behaved as expected, $bad_expect did not"
[ "$cases" -ge "$MIN_CASES" ] || { echo "selftest: RED — $cases cases ran, the floor is $MIN_CASES"; exit 1; }
[ "$bad_expect" -eq 0 ]
