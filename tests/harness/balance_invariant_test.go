//go:build harness

package harness

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The balance-invariant tests (Phase 1, chunk B7; locked decision 5). The invariant: the stored balance of every (user, module)
// is exactly the sum of that user's ledger deltas, and no reserve is settled for more than it held. The database enforces it
// with a trigger (the balance's only writer) and two CHECKs; platform.balance_drift and platform.reserve_overdrawn are the
// recompute that notices if the trigger is ever off, and scripts/check-balance-invariant.sh turns any row of either into a red
// gate. These tests prove the three parts separately: the invariant holds under a long random sequence, the views are not
// vacuous (each fires when the trigger is off or a reserve is overspent, and stays silent otherwise), and who can write a
// balance at all.

// balUnit is one billable unit of a module and its current price, read from platform.unit_costs.
type balUnit struct {
	name string
	cost int64
}

func balUnits(t *testing.T, db ledgerDB, module string) []balUnit {
	t.Helper()
	rows, err := db.Query(testCtx(t), "SELECT unit, cost FROM platform.unit_costs WHERE module_id = $1 ORDER BY unit", module)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []balUnit
	for rows.Next() {
		var u balUnit
		if err := rows.Scan(&u.name, &u.cost); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("module %s has no unit costs: the instrument matched nothing", module)
	}
	return out
}

type balKey struct{ user, module string }

type balReserve struct {
	id        int64
	user      string
	module    string
	job       string
	remaining int64 // credits of this reserve not yet charged or released
}

type balDelta struct {
	key                 balKey
	available, reserved int64
}

// TestBalance_RandomisedSequenceKeepsInvariant drives 200 pseudo-random ledger writes (a FIXED seed, so a failure repeats):
// three users, both metered modules, grants, reserves at a random unit's frozen price, charges at random units of the job's
// snapshot, and releases, each chosen within what its balance and its reserve still allow. Afterwards both views are empty and
// every stored balance equals a recomputation done HERE, in Go, from the deltas of the rows this test inserted (not from the
// database's own sums); a second recomputation in SQL agrees with the first. The test is not vacuous: it asserts how many
// rows of each kind it really wrote. It fails if the trigger mis-applies any delta, if a CHECK is wrong, or if either view
// disagrees with the arithmetic.
func TestBalance_RandomisedSequenceKeepsInvariant(t *testing.T) {
	const seed, operations = 20260116, 200
	_, admin := migratedScratch(t)
	ledgerSeed(t, admin) // autocommit: this is a scratch database
	users := []string{ledgerUserA, ledgerUserB, ledgerUserC}
	modules := []string{"m1", "m2"}
	units := map[string][]balUnit{}
	jobs := map[balKey][]string{}
	n := 0
	for _, m := range modules {
		units[m] = balUnits(t, admin, m)
		for _, u := range users {
			for j := 0; j < 2; j++ {
				n++
				jobs[balKey{u, m}] = append(jobs[balKey{u, m}], ledgerNewJob(t, admin, ledgerUUID("bb", n), u, m, true))
			}
		}
	}

	rng := rand.New(rand.NewSource(seed))
	available := map[balKey]int64{} // what the test believes each balance holds; it decides what is allowed
	reserved := map[balKey]int64{}
	var opens []*balReserve
	var written []balDelta // the independent record: the deltas of every row inserted
	counts := map[string]int{}

	for i := 0; i < operations; i++ {
		u, m := users[rng.Intn(len(users))], modules[rng.Intn(len(modules))]
		k := balKey{u, m}
		kind := "release"
		switch x := rng.Intn(100); {
		case x < 25:
			kind = "grant"
		case x < 55:
			kind = "reserve"
		case x < 85:
			kind = "charge"
		}
		var open []*balReserve
		if kind == "charge" || kind == "release" {
			for _, o := range opens {
				if o.remaining > 0 {
					open = append(open, o)
				}
			}
			if len(open) == 0 {
				kind = "grant" // nothing to settle yet
			}
		}

		switch kind {
		case "reserve":
			un := units[m][rng.Intn(len(units[m]))]
			maxUnits := available[k] / un.cost
			if maxUnits < 1 {
				kind = "grant" // the balance cannot pay for one unit: fund it instead
				break
			}
			count := 1 + rng.Int63n(min(maxUnits, 20))
			job := jobs[k][rng.Intn(len(jobs[k]))]
			id := ledgerPut(t, admin, ledgerReserveCols(u, m, job, un.name, count, un.cost))
			available[k] -= count * un.cost
			reserved[k] += count * un.cost
			opens = append(opens, &balReserve{id: id, user: u, module: m, job: job, remaining: count * un.cost})
			written = append(written, balDelta{k, -count * un.cost, count * un.cost})
		case "charge":
			o := open[rng.Intn(len(open))]
			var fit []balUnit
			for _, un := range units[o.module] {
				if un.cost <= o.remaining {
					fit = append(fit, un)
				}
			}
			if len(fit) == 0 {
				kind = "release" // no unit is cheap enough to charge from what is left: hand it back
				// fall through to the release branch below
			} else {
				un := fit[rng.Intn(len(fit))]
				count := 1 + rng.Int63n(min(o.remaining/un.cost, 10))
				ledgerPut(t, admin, ledgerChargeCols(o.user, o.module, o.job, o.id, un.name, count, un.cost))
				ok := balKey{o.user, o.module}
				reserved[ok] -= count * un.cost
				o.remaining -= count * un.cost
				written = append(written, balDelta{ok, 0, -count * un.cost})
				break
			}
			fallthrough
		case "release":
			o := open[rng.Intn(len(open))]
			amount := 1 + rng.Int63n(o.remaining)
			ledgerPut(t, admin, ledgerReleaseCols(o.user, o.module, o.job, o.id, amount))
			ok := balKey{o.user, o.module}
			available[ok] += amount
			reserved[ok] -= amount
			o.remaining -= amount
			written = append(written, balDelta{ok, amount, -amount})
		}
		if kind == "grant" {
			amount := 1 + rng.Int63n(60)
			ledgerFund(t, admin, u, m, amount)
			available[k] += amount
			written = append(written, balDelta{k, amount, 0})
		}
		counts[kind]++
		if i%40 == 39 {
			ledgerRequireViewsEmpty(t, admin, fmt.Sprintf("after operation %d", i+1))
		}
	}
	t.Logf("%d operations (seed %d): %d grants, %d reserves, %d charges, %d releases", operations, seed, counts["grant"], counts["reserve"], counts["charge"], counts["release"])

	// The sequence really exercised every kind, otherwise a pass says little.
	for kind, floor := range map[string]int{"grant": 10, "reserve": 10, "charge": 10, "release": 5} {
		if counts[kind] < floor {
			t.Fatalf("only %d %s rows were written (floor %d): the random sequence did not exercise the kind", counts[kind], kind, floor)
		}
	}
	if total := counts["grant"] + counts["reserve"] + counts["charge"] + counts["release"]; total != operations || len(written) != operations {
		t.Fatalf("%d operations counted and %d rows recorded, want %d of each", total, len(written), operations)
	}
	if got := ledgerInt64(t, admin, "SELECT count(*) FROM platform.ledger"); got != operations {
		t.Fatalf("the ledger holds %d rows, want exactly the %d written", got, operations)
	}

	// The recomputation, from the rows this test inserted.
	sums := map[balKey]*balDelta{}
	for _, d := range written {
		s := sums[d.key]
		if s == nil {
			s = &balDelta{key: d.key}
			sums[d.key] = s
		}
		s.available += d.available
		s.reserved += d.reserved
	}
	keys := make([]balKey, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].user != keys[j].user {
			return keys[i].user < keys[j].user
		}
		return keys[i].module < keys[j].module
	})
	if len(keys) < 4 {
		t.Fatalf("only %d (user, module) balances were touched, want at least 4 of the 6", len(keys))
	}
	var recomputed []string
	for _, k := range keys {
		s := sums[k]
		if s.available != available[k] || s.reserved != reserved[k] {
			t.Fatalf("the test's own bookkeeping is inconsistent for %v: recomputed %d/%d, running %d/%d", k, s.available, s.reserved, available[k], reserved[k])
		}
		recomputed = append(recomputed, fmt.Sprintf("%s|%s|%d|%d", k.user, k.module, s.available, s.reserved))
	}

	stored := ledgerRows(t, admin, "SELECT user_id::text, module_id, available, reserved FROM platform.balances ORDER BY 1, 2")
	if strings.Join(stored, "\n") != strings.Join(recomputed, "\n") {
		t.Fatalf("the stored balances differ from the recomputation done in Go\n  stored:     %v\n  recomputed: %v", stored, recomputed)
	}
	inSQL := ledgerRows(t, admin, `SELECT user_id::text, module_id, sum(available_delta)::bigint, sum(reserved_delta)::bigint FROM platform.ledger WHERE monetary GROUP BY 1, 2 ORDER BY 1, 2`)
	if strings.Join(inSQL, "\n") != strings.Join(recomputed, "\n") {
		t.Fatalf("SQL's SUM over the ledger differs from the recomputation done in Go\n  sql: %v\n  go:  %v", inSQL, recomputed)
	}
	ledgerRequireViewsEmpty(t, admin, "after the whole sequence")
	// every reserve is settled for no more than it held (the second view's claim, checked here by hand)
	for _, o := range opens {
		if o.remaining < 0 {
			t.Fatalf("reserve %d was settled for more than it held (remaining %d)", o.id, o.remaining)
		}
	}
}

// TestBalance_DriftViewSeesDisabledTrigger is the paired control of the invariant: the trigger is what keeps the balance equal
// to the ledger, and platform.balance_drift is the alarm that notices when it is not. Each subtest does its damage inside a
// rolled-back transaction. It fails if the view is empty when the trigger is off (a dead alarm), or non-empty when it is on.
func TestBalance_DriftViewSeesDisabledTrigger(t *testing.T) {
	_, admin := migratedScratch(t)
	a := ledgerUserA
	fresh := func(t *testing.T) pgx.Tx {
		t.Helper()
		tx := ledgerBegin(t, admin)
		ledgerSeed(t, tx)
		return tx
	}
	const triggerOff = "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_balance"

	t.Run("trigger disabled: the grant is in the ledger (7) and not in the balance (0)", func(t *testing.T) {
		tx := fresh(t)
		ledgerMust(t, tx, triggerOff)
		ledgerFund(t, tx, a, "m1", 7)
		if got, want := ledgerDrift(t, tx), []string{a + "|m1|0|7|0|0"}; strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift with the trigger off = %v, want %v (stored 0 vs ledger 7)", got, want)
		}
		if b := ledgerBalance(t, tx, a, "m1"); b.Exists {
			t.Fatalf("a balance row exists (%+v) although the trigger was off", b)
		}
	})

	t.Run("control: the identical insert with the trigger enabled leaves no drift", func(t *testing.T) {
		tx := fresh(t)
		ledgerFund(t, tx, a, "m1", 7)
		ledgerRequireBalance(t, tx, "after the grant", a, "m1", 7, 0)
		ledgerRequireViewsEmpty(t, tx, "with the trigger on")
	})

	t.Run("re-enabling the trigger does not repair earlier drift: the alarm keeps ringing until someone fixes it", func(t *testing.T) {
		tx := fresh(t)
		ledgerMust(t, tx, triggerOff)
		ledgerFund(t, tx, a, "m1", 7)
		ledgerMust(t, tx, "ALTER TABLE platform.ledger ENABLE TRIGGER ledger_apply_to_balance")
		ledgerFund(t, tx, a, "m1", 5)
		if got, want := ledgerDrift(t, tx), []string{a + "|m1|5|12|0|0"}; strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift after re-enabling = %v, want %v (stored 5 vs ledger 12: the first grant is still missing)", got, want)
		}
	})

	t.Run("reserved credits drift too: both columns are compared", func(t *testing.T) {
		tx := fresh(t)
		ledgerFund(t, tx, a, "m1", 100)
		ledgerMust(t, tx, triggerOff)
		ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
		if got, want := ledgerDrift(t, tx), []string{a + "|m1|100|80|0|20"}; strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift = %v, want %v (available 100 vs 80 and reserved 0 vs 20)", got, want)
		}
	})

	t.Run("a balance with no ledger rows is drift too", func(t *testing.T) {
		tx := fresh(t)
		ledgerMust(t, tx, "INSERT INTO platform.balances (user_id, module_id, available) VALUES ($1, 'm1', 5)", a)
		if got, want := ledgerDrift(t, tx), []string{a + "|m1|5|0|0|0"}; strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift = %v, want %v (stored 5 vs ledger 0)", got, want)
		}
	})

	t.Run("a balance edited behind the ledger's back is drift", func(t *testing.T) {
		tx := fresh(t)
		ledgerFund(t, tx, a, "m1", 50)
		ledgerRequireViewsEmpty(t, tx, "before the edit")
		ledgerMust(t, tx, "UPDATE platform.balances SET available = available + 1 WHERE user_id = $1 AND module_id = 'm1'", a)
		if got, want := ledgerDrift(t, tx), []string{a + "|m1|51|50|0|0"}; strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift = %v, want %v (stored 51 vs ledger 50)", got, want)
		}
	})

	t.Run("control: entitlement events are not money, so a disabled trigger makes no drift for them", func(t *testing.T) {
		tx := fresh(t)
		ledgerMust(t, tx, triggerOff)
		ledgerPut(t, tx, ledgerM3Cols("m3_enable", a))
		ledgerRequireViewsEmpty(t, tx, "after an m3 event with the trigger off")
	})
}

// TestBalance_ReserveOverdrawnSeesWhatBalanceCheckCannot proves the second view earns its place. Two reserves of one user and
// one module share a single balance: a charge of 15 against a reserve of 10 leaves the balance's reserved column at 5 (20 - 15),
// which the per-balance CHECK (reserved >= 0) accepts; only platform.reserve_overdrawn knows that reserve A was settled for more
// than it held. With ONE reserve the same charge is caught by the CHECK. It fails if the view misses an overspend, lists a
// reserve that was settled exactly, or if the CHECK stops being the net for a lone reserve.
func TestBalance_ReserveOverdrawnSeesWhatBalanceCheckCannot(t *testing.T) {
	_, admin := migratedScratch(t)
	a := ledgerUserA
	const jobA = ledgerJobA1
	// two reserves of 10 credits each on one job, one funded balance: available 100 - 20, reserved 20
	setup := func(t *testing.T) (tx pgx.Tx, reserveA, reserveB int64) {
		t.Helper()
		tx = ledgerBegin(t, admin)
		ledgerSeed(t, tx)
		ledgerFund(t, tx, a, "m1", 100)
		reserveA = ledgerPut(t, tx, ledgerReserveCols(a, "m1", jobA, "m1_own", 10, 1))
		reserveB = ledgerPut(t, tx, ledgerReserveCols(a, "m1", jobA, "m1_own", 10, 1))
		ledgerRequireBalance(t, tx, "after the two reserves", a, "m1", 80, 20)
		ledgerRequireViewsEmpty(t, tx, "after the two reserves")
		return tx, reserveA, reserveB
	}
	charge := func(t *testing.T, tx pgx.Tx, reserve, units int64) {
		t.Helper()
		ledgerOKRow(t, tx, fmt.Sprintf("charging %d against reserve %d", units, reserve), ledgerChargeCols(a, "m1", jobA, reserve, "m1_own", units, 1))
	}
	listed := func(t *testing.T, tx pgx.Tx) []string { t.Helper(); return ledgerOverdrawn(t, tx) }

	t.Run("a charge of 15 against a reserve of 10: the balance check passes, the view lists the reserve", func(t *testing.T) {
		tx, reserveA, _ := setup(t)
		charge(t, tx, reserveA, 15)
		ledgerRequireBalance(t, tx, "the per-balance CHECK saw reserved = 20 - 15 = 5 and was content", a, "m1", 80, 5)
		if d := ledgerDrift(t, tx); len(d) != 0 {
			t.Fatalf("balance_drift = %v: the balance and the ledger agree, so the first view must be empty", d)
		}
		want := []string{fmt.Sprintf("%d|%s|m1|%s|10|15", reserveA, a, jobA)}
		if got := listed(t, tx); strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("reserve_overdrawn = %v, want %v (reserved 10, settled 15)", got, want)
		}
	})

	t.Run("control: a charge of exactly 10 settles the reserve and is not listed", func(t *testing.T) {
		tx, reserveA, _ := setup(t)
		charge(t, tx, reserveA, 10)
		if got := listed(t, tx); len(got) != 0 {
			t.Fatalf("reserve_overdrawn = %v for a reserve settled exactly", got)
		}
	})

	t.Run("a charge plus a release that together exceed the reserve are listed", func(t *testing.T) {
		tx, reserveA, _ := setup(t)
		charge(t, tx, reserveA, 6)
		ledgerOKRow(t, tx, "releasing 6 more", ledgerReleaseCols(a, "m1", jobA, reserveA, 6))
		ledgerRequireBalance(t, tx, "the balance is still within its CHECKs", a, "m1", 86, 8)
		want := []string{fmt.Sprintf("%d|%s|m1|%s|10|12", reserveA, a, jobA)}
		if got := listed(t, tx); strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("reserve_overdrawn = %v, want %v (6 charged + 6 released of 10)", got, want)
		}
	})

	t.Run("control: 4 charged and 6 released settle exactly 10 and are not listed; an unsettled reserve is not listed", func(t *testing.T) {
		tx, reserveA, reserveB := setup(t)
		charge(t, tx, reserveA, 4)
		ledgerOKRow(t, tx, "releasing 6", ledgerReleaseCols(a, "m1", jobA, reserveA, 6))
		if got := listed(t, tx); len(got) != 0 {
			t.Fatalf("reserve_overdrawn = %v (reserve A settled 4 + 6 = 10 exactly; reserve %d has no settlement at all)", got, reserveB)
		}
	})

	t.Run("only the overspent reserve is listed, not its neighbour on the same balance", func(t *testing.T) {
		tx, reserveA, reserveB := setup(t)
		charge(t, tx, reserveA, 15)
		charge(t, tx, reserveB, 5)
		got := listed(t, tx)
		if len(got) != 1 || !strings.HasPrefix(got[0], fmt.Sprintf("%d|", reserveA)) {
			t.Fatalf("reserve_overdrawn = %v, want only reserve %d (B settled 5 of 10)", got, reserveA)
		}
	})

	t.Run("with ONE reserve on the balance the same charge is refused by the per-balance CHECK", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		ledgerSeed(t, tx)
		ledgerFund(t, tx, a, "m1", 100)
		only := ledgerPut(t, tx, ledgerReserveCols(a, "m1", jobA, "m1_own", 10, 1))
		ledgerRefusedRow(t, tx, "a charge of 15 against the only reserve", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative", Table: "balances"},
			ledgerChargeCols(a, "m1", jobA, only, "m1_own", 15, 1))
		ledgerRequireViewsEmpty(t, tx, "after the refused charge")
	})
}

// TestBalance_NoRoleWritesBalancesExceptTheOwner is about who can write platform.balances. platform_app and platform_admin
// hold SELECT only: UPDATE, INSERT, DELETE and TRUNCATE are refused with 42501, and so is calling the trigger function by hand;
// the paired controls are that the superuser's same statements succeed and that the only thing these roles CAN do is insert into
// the ledger, which moves the balance through the trigger. Then the honest part: platform_owner CAN write the table, and the
// invariant for that role rests on platform.balance_drift, not on privileges. It fails if a role gains a write privilege or
// EXECUTE on the trigger function.
func TestBalance_NoRoleWritesBalancesExceptTheOwner(t *testing.T) {
	_, admin := migratedScratch(t)
	a, b := ledgerUserA, ledgerUserB
	fresh := func(t *testing.T) pgx.Tx {
		t.Helper()
		tx := ledgerBegin(t, admin)
		ledgerSeed(t, tx)
		ledgerFund(t, tx, a, "m1", 40)
		return tx
	}
	statements := []struct{ op, sql string }{
		{"UPDATE", "UPDATE platform.balances SET available = available + 1"},
		{"INSERT", fmt.Sprintf("INSERT INTO platform.balances (user_id, module_id) VALUES ('%s', 'm1')", b)},
		{"DELETE", "DELETE FROM platform.balances"},
		{"TRUNCATE", "TRUNCATE platform.balances"},
	}

	for _, role := range []string{"platform_app", "platform_admin"} {
		role := role
		t.Run(role, func(t *testing.T) {
			tx := fresh(t)
			ledgerTrial(t, tx, func(sp pgx.Tx) { // the control: the superuser may run every one of these, so the refusals below are the role's
				for _, st := range statements {
					if _, err := sp.Exec(testCtx(t), st.sql); err != nil {
						t.Fatalf("control: the superuser's %s on platform.balances failed (%v), so a refusal of it would prove nothing", st.op, err)
					}
				}
			})
			ledgerSwitchRole(t, tx, role)
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.balances"); n != 1 {
				t.Fatalf("as %s SELECT sees %d balance rows, want the 1 that was funded", role, n)
			}
			for _, st := range statements {
				ledgerExpectRefused(t, tx, fmt.Sprintf("%s %s on platform.balances", role, st.op), "42501", ledgerWhy{Message: "permission denied for table balances"}, st.sql)
			}
			ledgerExpectRefused(t, tx, role+" calling the trigger function by hand", "42501", ledgerWhy{Message: "permission denied for function ledger_apply_to_balance"}, "SELECT platform.ledger_apply_to_balance()")
			// What the role CAN do: put a row in the ledger, and the balance follows through the trigger (which is SECURITY DEFINER).
			ledgerOKRow(t, tx, "control: "+role+" inserts a grant into the ledger", ledgerGrantCols(a, "m1", 2))
			ledgerRequireBalance(t, tx, "after "+role+"'s ledger insert", a, "m1", 42, 0)
		})
	}

	t.Run("platform_owner CAN write balances: the invariant for that role rests on the drift check, not on privileges", func(t *testing.T) {
		tx := fresh(t)
		ledgerSwitchRole(t, tx, "platform_owner")
		if tag := ledgerExpectOK(t, tx, "the owner's UPDATE of a balance", "UPDATE platform.balances SET available = available + 1 WHERE user_id = $1", a); tag.RowsAffected() != 1 {
			t.Fatalf("the owner's UPDATE touched %d rows", tag.RowsAffected())
		}
		ledgerExpectOK(t, tx, "the owner's INSERT of a balance for a user with no ledger rows", "INSERT INTO platform.balances (user_id, module_id, available) VALUES ($1, 'm1', 9)", b)
		t.Log("RESIDUAL POWER: platform_owner can UPDATE and INSERT platform.balances directly (no trigger or privilege stops the table's owner). It is NOLOGIN and used only by the migrator; the compensating control is platform.balance_drift and scripts/check-balance-invariant.sh.")
		got := ledgerDrift(t, tx)
		want := []string{a + "|m1|41|40|0|0", b + "|m1|9|0|0|0"}
		if strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("balance_drift after the owner's direct writes = %v, want %v", got, want)
		}
	})
}

// TestBalance_TriggerFunctionsAreSafeDefiners checks the property the invariant leans on: the two SECURITY DEFINER functions (the
// balance writer and the price-history writer) run as platform_owner with a PINNED search_path, so a caller cannot hijack them
// by shadowing a table or operator, and no role can call them. The control is the same catalog query run against a
// SECURITY DEFINER function WITHOUT a pinned path, which it must report. It fails if a definer function loses its search_path
// or gains an EXECUTE grant.
func TestBalance_TriggerFunctionsAreSafeDefiners(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	const unpinned = `SELECT p.proname FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.prosecdef
		AND NOT EXISTS (SELECT 1 FROM unnest(COALESCE(p.proconfig, ARRAY[]::text[])) c WHERE c = 'search_path=pg_catalog, pg_temp') ORDER BY 1`

	definers := ledgerRows(t, tx, `SELECT p.proname || '|' || pg_get_userbyid(p.proowner) FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.prosecdef ORDER BY 1`)
	want := []string{"ledger_apply_to_balance|platform_owner", "unit_costs_record_history|platform_owner"}
	if strings.Join(definers, ";") != strings.Join(want, ";") {
		t.Fatalf("SECURITY DEFINER functions in schema platform = %v, want exactly %v (a new one needs a review of its search_path and its callers)", definers, want)
	}
	if bad := ledgerRows(t, tx, unpinned); len(bad) != 0 {
		t.Fatalf("SECURITY DEFINER functions without search_path = pg_catalog, pg_temp: %v", bad)
	}

	t.Run("control: the query reports a definer function that has no pinned path", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "CREATE FUNCTION platform.zz_unsafe() RETURNS int LANGUAGE sql SECURITY DEFINER AS $$ SELECT 1 $$")
			if got := ledgerRows(t, sp, unpinned); len(got) != 1 || got[0] != "zz_unsafe" {
				t.Fatalf("the unpinned-definer query reports %v for an unsafe function, want [zz_unsafe]: it cannot fail", got)
			}
		})
	})

	t.Run("no role can execute them", func(t *testing.T) {
		for _, fn := range []string{"platform.ledger_apply_to_balance()", "platform.unit_costs_record_history()"} {
			for _, role := range []string{"platform_app", "platform_admin"} {
				if ledgerBool(t, tx, "SELECT has_function_privilege($1::text, $2::text, 'EXECUTE')", role, fn) {
					t.Fatalf("%s can EXECUTE %s", role, fn)
				}
			}
		}
		if !ledgerBool(t, tx, "SELECT has_function_privilege('platform_owner', 'platform.ledger_apply_to_balance()', 'EXECUTE')") {
			t.Fatal("control: platform_owner cannot execute its own function, so the denials above would prove nothing about grants")
		}
	})
}
