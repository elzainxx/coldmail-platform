//go:build harness

package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The close-out schema tests (Phase 2 close-out, items D3 and D4): what platform_0007 makes the DATABASE refuse, whoever the writer is. Every refusal asserts the SQLSTATE AND the constraint name that raised
// it (ledgerWhy), and every refusal has its paired control: the legal statement of the same kind passes, so a refusal cannot be a typo, a missing fixture or a different rule firing; and each rule has the
// control that proves it is the ONLY barrier (the trigger disabled by the superuser in a savepoint that is rolled back: the refused statement then succeeds).
//
//	D3 (open decision 10) TestCloseout_SnapshotPriceEqualsUnitCosts, TestCloseout_SnapshotPriceThroughTheRoles, TestCloseout_SnapshotTriggerIsTheOnlyBarrier
//	    a row of platform.job_unit_prices must carry the cost platform.unit_costs holds for the unit at the instant of the INSERT (constraint snapshot_equals_unit_costs, 23514).
//	D4 (open decision 11) TestCloseout_TerminalSucceededIsFinal, TestCloseout_TerminalFailedIsFinal, TestCloseout_TerminalCancelledIsFinal, TestCloseout_TerminalExpiredIsFinal,
//	    TestCloseout_NonTerminalJobsMoveOnlyAlongTheLifecycle
//	    once a job is succeeded, failed, cancelled or (since platform_0011) expired no UPDATE may change its status (constraint jobs_terminal_is_final, 23001); an UPDATE that leaves the status alone stays
//	    legal. The test of the non-terminal moves is the RECORD of what the schema allows, so that tightening it is a deliberate change: Phase 2 pinned "every non-terminal status moves to every
//	    status"; platform_0011 (Phase 4, chunk B1) made the lifecycle a table, so the same 40 statements are now refused (a bare UPDATE of status states no actor) and the test says so, each beside the
//	    legal move of the same job through platform.job_transition.
//
// Why BEFORE triggers show up in so many refusals below: a BEFORE INSERT trigger runs before the CHECK constraints, the unique indexes and the foreign keys, so a snapshot row whose price differs from the table
// is refused by THE TRIGGER first, whatever else is wrong with it (a price of 0 reaches job_unit_prices_cost_at_least_one only with the trigger switched off: TestConstraints_JobUnitPrices does exactly that).

const (
	clsSnapshotConstraint = "snapshot_equals_unit_costs"
	clsSnapshotTrigger    = "job_unit_prices_snapshot_equals_unit_costs"
	clsTerminalConstraint = "jobs_terminal_is_final"
	clsTerminalTrigger    = "jobs_terminal_is_final"
	clsPrices             = "platform.job_unit_prices"
	clsStart              = "2030-01-01T00:00:00Z"
	clsSetStatus          = "UPDATE platform.jobs SET status = $2 WHERE id = $1"
)

// clsStatuses is the status vocabulary of platform.jobs since platform_0011, and clsTerminal the four that are final (platform_0007 made three of them so, platform_0011 added expired).
var clsStatuses = []string{"draft", "previewed", "confirmed", "scheduled", "running", "paused", "cancelling", "completing", "succeeded", "failed", "cancelled", "expired"}
var clsTerminal = map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "expired": true}

// clsOldStatuses is the vocabulary Phase 2 recorded (platform_0002's eight), and clsOldNonTerminal the five of them that were not final: TestCloseout_NonTerminalJobsMoveOnlyAlongTheLifecycle
// re-tries the 40 moves Phase 2 recorded as accepted.
var (
	clsOldStatuses    = []string{"draft", "scheduled", "running", "paused", "cancelling", "succeeded", "failed", "cancelled"}
	clsOldNonTerminal = []string{"draft", "scheduled", "running", "paused", "cancelling"}
)

// clsUnit is one row of platform.unit_costs as the tests read it (never hard-coded: the forged prices are derived from what the table holds).
type clsUnit struct {
	unit, module string
	cost         int64
}

// clsUnits reads platform.unit_costs and fails unless it finds the five seeded units (an instrument that matched nothing proves nothing).
func clsUnits(t *testing.T, db ledgerDB) []clsUnit {
	t.Helper()
	rows, err := db.Query(testCtx(t), "SELECT unit, module_id, cost FROM platform.unit_costs ORDER BY unit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []clsUnit
	for rows.Next() {
		var u clsUnit
		if err := rows.Scan(&u.unit, &u.module, &u.cost); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("platform.unit_costs holds %d units, want the five of the seed: the fixtures are not what these tests are about", len(out))
	}
	return out
}

func clsUnitNamed(t *testing.T, units []clsUnit, name string) clsUnit {
	t.Helper()
	for _, u := range units {
		if u.unit == name {
			return u
		}
	}
	t.Fatalf("platform.unit_costs has no unit %s", name)
	return clsUnit{}
}

// clsPriceRow is one row of platform.job_unit_prices for the unit, at the cost given (an int64, or nil for NULL).
func clsPriceRow(job string, u clsUnit, cost any) ledgerCols {
	return ledgerCols{"job_id": job, "module_id": u.module, "unit": u.unit, "cost": cost}
}

// clsFreezes is the message a refused snapshot row carries: the unit, the cost the row froze and the cost the table holds.
func clsFreezes(unit string, snapshot, table int64) string {
	return fmt.Sprintf("freezes unit %s at %d, but platform.unit_costs holds %d", unit, snapshot, table)
}

// clsTriggerEnabled is pg_trigger.tgenabled of a trigger of a platform table ('O' = enabled).
func clsTriggerEnabled(t *testing.T, db ledgerDB, table, trigger string) string {
	t.Helper()
	return ledgerString(t, db, "SELECT tgenabled::text FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = to_regclass($1::text) AND tgname::text = $2", table, trigger)
}

// clsInsertJob makes a job of the user in the given status. DELIBERATE CHANGE (platform_0011): a job is born draft (an INSERT with any other status is refused, job_born_draft), so this inserts a draft job and
// walks it to the status along the lifecycle (jobWalkTo), the way the engine will. The module is one that can reach the status: module 2 can cancel a running job, module 1 cannot (cancelling).
func clsInsertJob(t *testing.T, db ledgerDB, id, user, module, status string) {
	t.Helper()
	jobInsertAt(t, db, id, user, module, status)
}

// ---------------------------------------------------------------------------------------------------------------------
// D3: snapshot prices are enforced by the database
// ---------------------------------------------------------------------------------------------------------------------

// TestCloseout_SnapshotPriceEqualsUnitCosts proves snapshot_equals_unit_costs: a row of the job's price snapshot must carry the cost platform.unit_costs holds for the unit at the instant of the INSERT.
// The legal snapshot (the store's one INSERT ... SELECT from unit_costs, or the same rows one by one at the table's price) passes for both modules; for every unit of the table three forged prices (one below,
// one above, a very large one: the vendor price as 1 among them) are each refused with 23514 and the constraint, the message naming the unit, the price the row froze and the price the table holds, each beside
// a paired control (the same row at the table's price passes). Zero, a negative price and NULL are refused by the trigger itself; a unit the table does not know, a NULL unit, a unit of another module and a
// duplicate are left to the constraints that always answered them. The price is the table's at the INSERT, not at some earlier time: after an admin changes the vendor price the OLD price is refused and the new
// one passes; a row already written is not judged again when the table moves later (it is frozen: settlement reads it, never the table), and a job frozen after the move gets the new price from the copy.
// It fails if the trigger is missing, compares with the wrong column, lets NULL through, or masks the foreign keys.
func TestCloseout_SnapshotPriceEqualsUnitCosts(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	units := clsUnits(t, tx)
	n := 0
	newJob := func(module string) string {
		n++
		return ledgerNewJob(t, tx, ledgerUUID("c3", n), ledgerUserC, module, false)
	}

	t.Run("the legal snapshot is a copy of the table and passes (the control of every refusal below)", func(t *testing.T) {
		for _, module := range []string{"m1", "m2"} {
			want := 0
			for _, u := range units {
				if u.module == module {
					want++
				}
			}
			copied := newJob(module)
			ledgerFreeze(t, tx, copied) // ONE INSERT ... SELECT FROM platform.unit_costs, the statement the store runs
			rowByRow := newJob(module)
			for _, u := range units {
				if u.module == module {
					ledgerOKInsert(t, tx, fmt.Sprintf("control: %s at its table price %d, one row", u.unit, u.cost), clsPrices, clsPriceRow(rowByRow, u, u.cost))
				}
			}
			for _, job := range []string{copied, rowByRow} {
				if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", job); got != int64(want) {
					t.Fatalf("the snapshot of job %s holds %d rows, want the %d units of module %s", job, got, want, module)
				}
				if got := ledgerInt64(t, tx, `SELECT count(*) FROM platform.job_unit_prices p JOIN platform.unit_costs u ON u.unit = p.unit WHERE p.job_id = $1 AND p.cost <> u.cost`, job); got != 0 {
					t.Fatalf("%d row(s) of the legal snapshot of job %s differ from platform.unit_costs", got, job)
				}
			}
		}
	})

	t.Run("a forged price is refused for every unit, each with its paired control", func(t *testing.T) {
		jobs := map[string]string{"m1": newJob("m1"), "m2": newJob("m2")}
		refused := 0
		for _, u := range units {
			job := jobs[u.module]
			legal := clsPriceRow(job, u, u.cost)
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				ledgerOKInsert(t, sp, fmt.Sprintf("control: %s at its table price %d", u.unit, u.cost), clsPrices, legal)
			})
			for _, forged := range []int64{u.cost - 1, u.cost + 1, 1000000} {
				ledgerRefusedInsert(t, tx, fmt.Sprintf("%s frozen at %d while the table says %d", u.unit, forged, u.cost), "23514",
					ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes(u.unit, forged, u.cost)}, clsPrices, legal.with("cost", forged))
				refused++
			}
		}
		if refused != 3*len(units) {
			t.Fatalf("%d forged rows were tried, want %d (three prices for each of the %d units)", refused, 3*len(units), len(units))
		}
		vendor := clsUnitNamed(t, units, "m1_vendor")
		if vendor.cost <= 1 {
			t.Fatalf("m1_vendor costs %d in the table, want more than 1 (the vendor is a premium): the case below would not be a forgery", vendor.cost)
		}
		ledgerRefusedInsert(t, tx, "the vendor price as 1 (the forgery open decision 10 names)", "23514",
			ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes("m1_vendor", 1, vendor.cost)}, clsPrices, clsPriceRow(jobs["m1"], vendor, int64(1)))
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id::text = ANY($1::text[])", []string{jobs["m1"], jobs["m2"]}); got != 0 {
			t.Fatalf("%d price row(s) were left by the refused and rolled-back statements", got)
		}
	})

	t.Run("zero, a negative price and NULL are refused by the trigger itself (it is NULL-safe), not left to the CHECK", func(t *testing.T) {
		job := newJob("m1")
		own := clsUnitNamed(t, units, "m1_own")
		for _, bad := range []struct {
			name string
			cost any
		}{{"zero", int64(0)}, {"negative", int64(-1)}, {"NULL", nil}} {
			ledgerRefusedInsert(t, tx, "a price of "+bad.name, "23514", ledgerWhy{Constraint: clsSnapshotConstraint}, clsPrices, clsPriceRow(job, own, bad.cost))
		}
		ledgerOKInsert(t, tx, "control: the same row at the table's price", clsPrices, clsPriceRow(job, own, own.cost))
	})

	t.Run("what the trigger leaves alone is still refused by the constraint that always refused it", func(t *testing.T) {
		job := newJob("m1")
		own := clsUnitNamed(t, units, "m1_own")
		alien := clsUnitNamed(t, units, "m2_accepted")
		ledgerRefusedInsert(t, tx, "a unit the table does not know", "23503", ledgerWhy{Constraint: "job_unit_prices_unit_fk"},
			clsPrices, clsPriceRow(job, clsUnit{unit: "m1_bogus", module: "m1"}, int64(1)))
		ledgerRefusedInsert(t, tx, "a NULL unit", "23502", ledgerWhy{Column: "unit"}, clsPrices, clsPriceRow(job, clsUnit{unit: "", module: "m1"}, int64(1)).with("unit", nil))
		ledgerRefusedInsert(t, tx, "a unit of another module at the table's own price", "23503", ledgerWhy{Constraint: "job_unit_prices_unit_fk"},
			clsPrices, clsPriceRow(job, clsUnit{unit: alien.unit, module: "m1"}, alien.cost))
		ledgerRefusedInsert(t, tx, "a job that does not exist, at the table's own price", "23503", ledgerWhy{Constraint: "job_unit_prices_job_fk"}, clsPrices, clsPriceRow(ledgerNoSuchID, own, own.cost))
		ledgerOKInsert(t, tx, "control: the unit of the job's own module at the table's price", clsPrices, clsPriceRow(job, own, own.cost))
		ledgerRefusedInsert(t, tx, "the same unit twice at the table's price", "23505", ledgerWhy{Constraint: "job_unit_prices_pk"}, clsPrices, clsPriceRow(job, own, own.cost))
	})

	t.Run("the price is the table's at the instant of the insert: after the table changed the OLD price is refused, the new one passes, a written row is not judged again", func(t *testing.T) {
		vendor := clsUnitNamed(t, units, "m1_vendor")
		job := newJob("m1")
		moved := vendor.cost + 3
		ledgerExpectOK(t, tx, "control: the vendor price changes through a named admin with a reason",
			"UPDATE platform.unit_costs SET cost = $1, changed_by = $2, change_reason = 'closeout: the table moves' WHERE unit = $3", moved, ledgerAdminA, vendor.unit)
		ledgerRefusedInsert(t, tx, "a snapshot taken at the OLD price after the table changed", "23514",
			ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes(vendor.unit, vendor.cost, moved)}, clsPrices, clsPriceRow(job, vendor, vendor.cost))
		ledgerOKInsert(t, tx, "control: the same unit at the NEW price", clsPrices, clsPriceRow(job, vendor, moved))

		later := moved + 4
		ledgerExpectOK(t, tx, "control: the vendor price changes again", "UPDATE platform.unit_costs SET cost = $1, changed_by = $2, change_reason = 'closeout: the table moves again' WHERE unit = $3", later, ledgerAdminA, vendor.unit)
		if got := ledgerInt64(t, tx, "SELECT cost FROM platform.job_unit_prices WHERE job_id = $1 AND unit = $2", job, vendor.unit); got != moved {
			t.Fatalf("the frozen row says %d after the table moved on to %d, want it untouched at %d", got, later, moved)
		}
		fresh := newJob("m1")
		ledgerFreeze(t, tx, fresh)
		if got := ledgerInt64(t, tx, "SELECT cost FROM platform.job_unit_prices WHERE job_id = $1 AND unit = $2", fresh, vendor.unit); got != later {
			t.Fatalf("a job frozen after the change got %d for %s, want the table's %d", got, vendor.unit, later)
		}
	})
}

// TestCloseout_SnapshotPriceThroughTheRoles proves that the refusal holds for the role the service really runs as: as platform_app (SET LOCAL ROLE, proved to have bitten) the forged vendor price is refused with
// the constraint (so the trigger can read platform.unit_costs as the caller: it is SECURITY INVOKER), and the store's own statements, in the order the store runs them (SELECT ... FOR SHARE on the module's
// unit_costs rows, which platform_app may take through its column-level UPDATE, then ONE INSERT ... SELECT ... RETURNING), pass. It fails if the trigger needs a privilege the service does not hold, or the
// store's snapshot statement is refused.
func TestCloseout_SnapshotPriceThroughTheRoles(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	units := clsUnits(t, tx)
	vendor := clsUnitNamed(t, units, "m1_vendor")
	forgedJob := ledgerNewJob(t, tx, ledgerUUID("c4", 1), ledgerUserC, "m1", false)
	storeJob := ledgerNewJob(t, tx, ledgerUUID("c4", 2), ledgerUserC, "m1", false)

	ledgerSwitchRole(t, tx, "platform_app")
	ledgerRefusedInsert(t, tx, "as platform_app, the vendor price frozen as 1", "23514",
		ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes("m1_vendor", 1, vendor.cost)}, clsPrices, clsPriceRow(forgedJob, vendor, int64(1)))
	ledgerRefusedInsert(t, tx, "as platform_app, the vendor price frozen above the table", "23514",
		ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes("m1_vendor", vendor.cost+1, vendor.cost)}, clsPrices, clsPriceRow(forgedJob, vendor, vendor.cost+1))
	ledgerOKInsert(t, tx, "control: as platform_app, the vendor row at the table's price", clsPrices, clsPriceRow(forgedJob, vendor, vendor.cost))

	// the store's two statements, as it runs them (internal/money/reserve.go): the share lock, then the one copy
	ledgerExpectOK(t, tx, "control: as platform_app, the share lock on the module's price rows", "SELECT unit, cost FROM platform.unit_costs WHERE module_id = $1 ORDER BY unit FOR SHARE", "m1")
	tag := ledgerExpectOK(t, tx, "control: as platform_app, the store's snapshot copy",
		`INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = $2 RETURNING unit, cost`, storeJob, "m1")
	if tag.RowsAffected() != 3 {
		t.Fatalf("the store's snapshot copy wrote %d rows, want the 3 units of module 1", tag.RowsAffected())
	}
	ledgerResetRole(t, tx)
	if got := ledgerInt64(t, tx, `SELECT count(*) FROM platform.job_unit_prices p JOIN platform.unit_costs u ON u.unit = p.unit WHERE p.job_id = $1 AND p.cost <> u.cost`, storeJob); got != 0 {
		t.Fatalf("%d row(s) of the store's snapshot differ from the table", got)
	}
}

// TestCloseout_SnapshotTriggerIsTheOnlyBarrier is the control that the trigger is the only thing between a writer and a forged price: with job_unit_prices_snapshot_equals_unit_costs disabled by the superuser (in a
// savepoint that is rolled back) the very row that was refused is ACCEPTED, and the snapshot then differs from the table; after the savepoint the trigger is enabled again and the row is refused again.
// It fails if some other rule already refuses the forged row (the control would then prove nothing about the trigger) or the savepoint leaves the trigger off.
func TestCloseout_SnapshotTriggerIsTheOnlyBarrier(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	units := clsUnits(t, tx)
	vendor := clsUnitNamed(t, units, "m1_vendor")
	job := ledgerNewJob(t, tx, ledgerUUID("c5", 1), ledgerUserC, "m1", false)
	forged := clsPriceRow(job, vendor, int64(1))
	why := ledgerWhy{Constraint: clsSnapshotConstraint, Message: clsFreezes("m1_vendor", 1, vendor.cost)}

	if got := clsTriggerEnabled(t, tx, clsPrices, clsSnapshotTrigger); got != "O" {
		t.Fatalf("the trigger %s is %q before the test, want enabled (O)", clsSnapshotTrigger, got)
	}
	ledgerRefusedInsert(t, tx, "the forged vendor price with the trigger enabled", "23514", why, clsPrices, forged)
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerMust(t, sp, "ALTER TABLE "+clsPrices+" DISABLE TRIGGER "+clsSnapshotTrigger)
		ledgerOKInsert(t, sp, "with the trigger disabled the forged vendor price 1 is accepted: nothing else stops it", clsPrices, forged)
		if got := ledgerInt64(t, sp, "SELECT cost FROM platform.job_unit_prices WHERE job_id = $1 AND unit = 'm1_vendor'", job); got != 1 {
			t.Fatalf("the accepted forged row holds %d, want 1", got)
		}
		if got := ledgerInt64(t, sp, "SELECT cost FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != vendor.cost {
			t.Fatalf("the table holds %d for the vendor, want %d: the control changed the table, not the snapshot", got, vendor.cost)
		}
	})
	if got := clsTriggerEnabled(t, tx, clsPrices, clsSnapshotTrigger); got != "O" {
		t.Fatalf("the trigger %s is %q after the rolled-back savepoint, want enabled (O)", clsSnapshotTrigger, got)
	}
	ledgerRefusedInsert(t, tx, "the forged vendor price once the trigger is back", "23514", why, clsPrices, forged)
}

// ---------------------------------------------------------------------------------------------------------------------
// D4: terminal job states are final
// ---------------------------------------------------------------------------------------------------------------------

func TestCloseout_TerminalSucceededIsFinal(t *testing.T) { clsTerminalIsFinal(t, "succeeded") }
func TestCloseout_TerminalFailedIsFinal(t *testing.T)    { clsTerminalIsFinal(t, "failed") }
func TestCloseout_TerminalCancelledIsFinal(t *testing.T) { clsTerminalIsFinal(t, "cancelled") }
func TestCloseout_TerminalExpiredIsFinal(t *testing.T)   { clsTerminalIsFinal(t, "expired") }

// clsTerminalIsFinal is the test of one terminal state (the four tests above call it, so each state has its own scratch database and its own verdict). A job in the state: (1) is refused every one of the
// eleven other statuses with 23001 and the constraint jobs_terminal_is_final, the message naming the old and the new status, by the bare UPDATE and by the sanctioned platform.job_transition alike, each
// beside a paired control (a fresh job makes the same move along the lifecycle); the job still has its status afterwards; (2) may still be updated while its status stays: the same status written back,
// module_job_ref, scheduled_start, updated_at owned by the trigger; a status change smuggled in beside such an update is refused, and jobs_guard still answers for what it owns (frozen_settings, user_id); (3)
// is refused the same way for the role the service runs as (platform_app), and may be touched by it; (4) is no longer held by ONE wall (DELIBERATE CHANGE, platform_0011): with the terminal trigger disabled
// by the superuser (a savepoint that is rolled back) the lifecycle guard still refuses the move (a terminal status has no row in platform.job_transitions: job_transition_illegal), and with both
// triggers disabled the move happens; each is enabled again afterwards. It fails if the trigger is missing, if it covers only some statuses, if it refuses an UPDATE that does not change the status, or if
// it replaces jobs_guard instead of standing beside it.
func clsTerminalIsFinal(t *testing.T, term string) {
	if !clsTerminal[term] {
		t.Fatalf("%q is not a terminal status", term)
	}
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	n := 0
	next := func() string { n++; return ledgerUUID("d4", n) }
	why := func(to string) ledgerWhy {
		return ledgerWhy{Constraint: clsTerminalConstraint, Message: fmt.Sprintf("is %s and a terminal status is final: it cannot become %s", term, to)}
	}

	t.Run("every other status is refused, each beside a paired control", func(t *testing.T) {
		tried := 0
		for _, to := range clsStatuses {
			if to == term {
				continue
			}
			// the paired control: a job that is not terminal makes the move to `to` the sanctioned way (a fresh job walked there, each hop with its actor and reason), so the refusal below is the
			// terminal rule's and not a status the lifecycle could never reach (module 2: it can cancel, so every status is reachable)
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				id := next()
				ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", id, ledgerUserC)
				if to == "draft" {
					jobWalkTo(t, sp, id, "previewed") // the only way back to draft is from previewed
				}
				jobWalkTo(t, sp, id, to)
				if got := jobStatus(t, sp, id); got != to {
					t.Fatalf("the fresh job is %s after its walk to %s", got, to)
				}
			})
			id := next()
			clsInsertJob(t, tx, id, ledgerUserC, "m2", term)
			ledgerExpectRefused(t, tx, fmt.Sprintf("a %s job moved to %s by a bare UPDATE", term, to), "23001", why(to), clsSetStatus, id, to)
			ledgerExpectRefused(t, tx, fmt.Sprintf("a %s job moved to %s through platform.job_transition", term, to), "23001", why(to), jobMoveSQL, id, to, "engine", nil, "closeout: a terminal job moves")
			if got := jobStatus(t, tx, id); got != term {
				t.Fatalf("the %s job is %s after the refused move to %s", term, got, to)
			}
			tried++
		}
		if tried != len(clsStatuses)-1 {
			t.Fatalf("%d moves were tried out of a %s job, want %d (every other status)", tried, term, len(clsStatuses)-1)
		}
	})

	t.Run("an UPDATE that leaves the status alone stays legal, and a status change cannot ride along", func(t *testing.T) {
		id := next()
		clsInsertJob(t, tx, id, ledgerUserC, "m2", term)
		ledgerExpectOK(t, tx, "control: the same status written back", clsSetStatus, id, term)
		ledgerExpectOK(t, tx, "control: module_job_ref set on a "+term+" job", "UPDATE platform.jobs SET module_job_ref = $2 WHERE id = $1", id, "ref-"+id)
		ledgerExpectOK(t, tx, "control: scheduled_start set on a "+term+" job", "UPDATE platform.jobs SET scheduled_start = $2 WHERE id = $1", id, clsStart)
		ledgerExpectOK(t, tx, "control: scheduled_start changed on a "+term+" job", "UPDATE platform.jobs SET scheduled_start = scheduled_start + interval '1 day' WHERE id = $1", id)
		ledgerExpectOK(t, tx, "control: an UPDATE that tries to set updated_at", "UPDATE platform.jobs SET updated_at = '2000-01-01T00:00:00Z' WHERE id = $1", id)
		if !ledgerBool(t, tx, "SELECT updated_at = now() FROM platform.jobs WHERE id = $1", id) {
			t.Fatal("updated_at is not now() after an UPDATE of a terminal job: jobs_guard no longer owns it")
		}
		ledgerExpectRefused(t, tx, "a status change next to a module_job_ref change", "23001", why("draft"), "UPDATE platform.jobs SET status = 'draft', module_job_ref = 'smuggled' WHERE id = $1", id)
		ledgerExpectRefused(t, tx, "a terminal job's frozen_settings (jobs_guard still answers for it)", "23001", ledgerWhy{Message: "frozen"},
			`UPDATE platform.jobs SET frozen_settings = '{"a":1}' WHERE id = $1`, id)
		ledgerExpectRefused(t, tx, "a terminal job's user_id (jobs_guard still answers for it)", "23001", ledgerWhy{Message: "never change"},
			"UPDATE platform.jobs SET user_id = $2 WHERE id = $1", id, ledgerUserB)
		if got := jobStatus(t, tx, id); got != term {
			t.Fatalf("the %s job is %s after the legal and the refused updates", term, got)
		}
		if got := ledgerString(t, tx, "SELECT module_job_ref FROM platform.jobs WHERE id = $1", id); got != "ref-"+id {
			t.Fatalf("module_job_ref is %q, want the one the control wrote", got)
		}
	})

	t.Run("the same through platform_app, the role the service runs as", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			clsInsertJob(t, sp, id, ledgerUserC, "m2", term)
			ledgerSwitchRole(t, sp, "platform_app")
			ledgerExpectOK(t, sp, "control: platform_app writes the same status back", clsSetStatus, id, term)
			ledgerExpectOK(t, sp, "control: platform_app sets module_job_ref", "UPDATE platform.jobs SET module_job_ref = $2 WHERE id = $1", id, "app-"+id)
			for _, to := range []string{"draft", "running"} {
				ledgerExpectRefused(t, sp, fmt.Sprintf("platform_app moves a %s job to %s", term, to), "23001", why(to), clsSetStatus, id, to)
				ledgerExpectRefused(t, sp, fmt.Sprintf("platform_app moves a %s job to %s through platform.job_transition", term, to), "23001", why(to), jobMoveSQL, id, to, "engine", nil, "closeout: a terminal job moves")
			}
			ledgerResetRole(t, sp)
		})
	})

	t.Run("two walls: with the terminal trigger disabled the lifecycle guard still refuses the move, and with both disabled the move happens", func(t *testing.T) {
		for _, trigger := range []string{clsTerminalTrigger, "jobs_transition_guard"} {
			if got := clsTriggerEnabled(t, tx, "platform.jobs", trigger); got != "O" {
				t.Fatalf("the trigger %s is %q before the control, want enabled (O)", trigger, got)
			}
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			clsInsertJob(t, sp, id, ledgerUserC, "m2", term) // made BEFORE the triggers go: the walk to a terminal status needs the guard (it sets confirmed_at)
			ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER "+clsTerminalTrigger)
			if got := clsTriggerEnabled(t, sp, "platform.jobs", "jobs_guard"); got != "O" {
				t.Fatalf("jobs_guard is %q while the terminal trigger is disabled, want enabled (O): the control must switch off one trigger only", got)
			}
			ledgerExpectRefused(t, sp, "with the terminal trigger off a "+term+" job moved back to draft is still refused: a terminal status has no row in platform.job_transitions", "23001",
				ledgerWhy{Constraint: "job_transition_illegal"}, clsSetStatus, id, "draft")
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := next()
			clsInsertJob(t, sp, id, ledgerUserC, "m2", term)
			ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER "+clsTerminalTrigger)
			ledgerMust(t, sp, "ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
			// the event of a move is a third wall of its own (jobs_record_transition writes it and refuses a move that names no actor), so the move states its three settings
			ledgerExpectOK(t, sp, "with both triggers disabled a "+term+" job moves back to draft: nothing else stops it", jobMoveBy("engine", "", "closeout: both walls are down", id, "draft"))
			if got := jobStatus(t, sp, id); got != "draft" {
				t.Fatalf("the %s job is %s after the move with both triggers disabled, want draft", term, got)
			}
		})
		for _, trigger := range []string{clsTerminalTrigger, "jobs_transition_guard"} {
			if got := clsTriggerEnabled(t, tx, "platform.jobs", trigger); got != "O" {
				t.Fatalf("the trigger %s is %q after the rolled-back savepoints, want enabled (O)", trigger, got)
			}
		}
		id := next()
		clsInsertJob(t, tx, id, ledgerUserC, "m2", term)
		ledgerExpectRefused(t, tx, "the same move once the triggers are back", "23001", why("draft"), clsSetStatus, id, "draft")
	})
}

// TestCloseout_NonTerminalJobsMoveOnlyAlongTheLifecycle is the paired control of the four terminal tests and the RECORD of what the schema allows (it was TestCloseout_NonTerminalJobsMoveFreely: Phase 2
// pinned, on purpose, that every non-terminal status moved to every status, because no lifecycle existed; platform_0011 made the lifecycle a table, so this pin changes deliberately and says what
// replaced it). (1) One job walks a whole life along the lifecycle, every step accepted (draft, previewed, scheduled, paused, scheduled again, running, cancelling, completing, cancelled: module 2 can
// cancel a running job) and then stays cancelled (moving it on is refused by jobs_terminal_is_final). (2) The 40 statements Phase 2 recorded as accepted (each of the five non-terminal statuses of the old
// vocabulary written to each of the eight, as a bare UPDATE) are tried again, each in a savepoint that is rolled back: a write of the status the job already has is still accepted (it is no move: 5 of
// them), every other (35) is refused with 23001 - job_transition_illegal when the pair is not a row of the lifecycle, job_transition_needs_an_actor when it is a row (a bare UPDATE states no actor, no
// reason) - and the pair that IS a row is taken legally beside it, through platform.job_transition (the two rows no job can take today, running -> paused for want of a module that can pause and paused ->
// running for a job that paused from scheduled, are proven in TestJobLifecycle_*). It fails if the terminal rule reaches a job that is not terminal, or if a bare status write is accepted.
func TestCloseout_NonTerminalJobsMoveOnlyAlongTheLifecycle(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	n := 0
	next := func() string { n++; return ledgerUUID("d5", n) }

	t.Run("one job walks its whole life along the lifecycle and then stays where it ended", func(t *testing.T) {
		id := next()
		ledgerMust(t, tx, "INSERT INTO platform.jobs (id, user_id, module_id) VALUES ($1, $2, 'm2')", id, ledgerUserC)
		steps := []string{"previewed", "scheduled", "paused", "scheduled", "running", "cancelling", "completing", "cancelled"}
		for i, to := range steps {
			from := jobStatus(t, tx, id)
			jobHop(t, tx, id, to) // fails the test, naming the hop, when the database refuses it
			if got := jobStatus(t, tx, id); got != to {
				t.Fatalf("the job is %s after its step %d (%s to %s)", got, i+1, from, to)
			}
		}
		for _, to := range []string{"running", "draft", "succeeded"} {
			ledgerExpectRefused(t, tx, "the cancelled job moved on to "+to, "23001",
				ledgerWhy{Constraint: clsTerminalConstraint, Message: "is cancelled and a terminal status is final: it cannot become " + to}, clsSetStatus, id, to)
		}
	})

	t.Run("the 40 bare status writes of Phase 2 are refused (except the five that are no move), each beside the legal move of the pair that is a row", func(t *testing.T) {
		moves, noMoves, legal := 0, 0, 0
		for _, from := range clsOldNonTerminal {
			for _, to := range clsOldStatuses {
				from, to := from, to
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					id := next()
					clsInsertJob(t, sp, id, ledgerUserC, "m2", from)
					switch {
					case from == to:
						ledgerExpectOK(t, sp, fmt.Sprintf("a %s job written %s again is no move", from, to), clsSetStatus, id, to)
						noMoves++
					case jobIsLegal(from, to):
						ledgerExpectRefused(t, sp, fmt.Sprintf("a bare UPDATE of a %s job to %s (a row of the lifecycle, but no actor and no reason)", from, to), "23001", ledgerWhy{Constraint: "job_transition_needs_an_actor"}, clsSetStatus, id, to)
						if from == "running" && to == "paused" || from == "paused" && to == "running" {
							break // not takeable today: see the doc comment
						}
						jobHop(t, sp, id, to)
						if got := jobStatus(t, sp, id); got != to {
							t.Fatalf("the %s job is %s after its legal move to %s", from, got, to)
						}
						legal++
					default:
						ledgerExpectRefused(t, sp, fmt.Sprintf("a bare UPDATE of a %s job to %s (not a row of the lifecycle)", from, to), "23001", ledgerWhy{Constraint: "job_transition_illegal"}, clsSetStatus, id, to)
					}
				})
				moves++
			}
		}
		if want := len(clsOldNonTerminal) * len(clsOldStatuses); moves != want {
			t.Fatalf("%d moves were tried, want %d (five non-terminal statuses to each of the eight)", moves, want)
		}
		if noMoves != len(clsOldNonTerminal) {
			t.Fatalf("%d of the writes were no move, want 5 (a status written back to itself)", noMoves)
		}
		if legal < 10 {
			t.Fatalf("only %d pairs were taken legally beside their refusal: the controls matched too little", legal)
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0007 in the migration lifecycle
// ---------------------------------------------------------------------------------------------------------------------

// snapshotTerminalObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0007's down: before it, the two functions and the two triggers are there, jobs_guard is still the guard of 0002 beside
// the new trigger, and the two rules bite; after it, both functions and both triggers are gone, jobs_guard is still there, and the two rules are gone (the forged price is accepted and a succeeded job moves
// back to draft). The counts are of a fixed list each, so a missing object cannot hide behind another's presence.
func snapshotTerminalObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	funcs := []string{"job_unit_prices_snapshot_equals_unit_costs", "jobs_terminal_is_final"}
	expect("the functions of 0007 (the snapshot price check, the terminal-state rule)", int64(len(funcs)),
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = ANY($1::text[])", funcs)
	expect("the triggers of 0007 (one on job_unit_prices, one on jobs)", int64(len(funcs)),
		"SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname::text = ANY($1::text[])", funcs)
	// what 0007 stands beside and never replaces: jobs_guard and its trigger, either way
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_guard' AND prosrc LIKE '%frozen_settings is frozen%' AND prosrc NOT LIKE '%terminal%'"); got != 1 {
		t.Fatalf("%s: platform.jobs_guard() is not the function of platform_0002 (found %d that is): platform_0007 adds a trigger beside it and never replaces it", when, got)
	}
	lifecycleSnapshotTerminal(t, c, present, when)
}

// lifecycleSnapshotTerminal is the behavioural half of snapshotTerminalObjects, in a transaction that is ALWAYS rolled back (a transaction left open on this connection would hold the locks the next down needs).
// With platform_0007 the forged vendor price is refused for its constraint and the copy is accepted, a succeeded job is refused a move back to draft for its constraint; without it the same forged row is
// accepted, and the same job moves back.
func lifecycleSnapshotTerminal(t *testing.T, c *pgx.Conn, enforced bool, when string) {
	t.Helper()
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	forged := ledgerCols{"job_id": ledgerJobA0, "module_id": "m1", "unit": "m1_vendor", "cost": int64(1)}
	ledgerExpectOK(t, tx, when+": job A1b is succeeded", clsSetStatus, ledgerJobA1b, "succeeded")
	if enforced {
		ledgerRefusedInsert(t, tx, when+": the vendor price frozen as 1", "23514", ledgerWhy{Constraint: clsSnapshotConstraint}, clsPrices, forged)
		ledgerFreeze(t, tx, ledgerJobA0) // the legal copy passes
		ledgerExpectRefused(t, tx, when+": a succeeded job moved back to draft", "23001", ledgerWhy{Constraint: clsTerminalConstraint}, clsSetStatus, ledgerJobA1b, "draft")
		return
	}
	ledgerOKInsert(t, tx, when+": the vendor price frozen as 1 (the older chain does not compare it with the table)", clsPrices, forged)
	ledgerExpectOK(t, tx, when+": a succeeded job moved back to draft (the older chain lets a terminal job move)", clsSetStatus, ledgerJobA1b, "draft")
}
