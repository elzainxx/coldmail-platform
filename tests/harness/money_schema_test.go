//go:build harness

package harness

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The money-schema tests (Phase 2, Part B): what platform_0005 (the money core) and platform_0006 (the entitlement flow) make the DATABASE refuse, whoever the writer is. Every refusal asserts the
// SQLSTATE AND the constraint or trigger name that raised it (ledgerWhy), and every refusal has its paired control: the legal row of the same kind passes, so a refusal cannot be a typo, a missing
// fixture or a different rule firing. The tests of the two migrations are TestMoneySchema_* (item reference, one unit, one charge per item, one reserve per job, the dearest price of a complete
// snapshot, platform.reserves, the release as the remainder) and TestM3Schema_* (the end date, the entitlement written by the ledger, expiry, the delivery table, who may write what).
//
// Why BEFORE triggers show up in so many refusals below: a BEFORE INSERT trigger runs before the CHECK constraints, the unique indexes and the foreign keys, so a row the reserve or release trigger
// refuses is refused by THAT trigger first. Every row below that is meant to prove another rule is therefore built to pass the triggers (a reserve at the dearest price, a release of exactly the
// remainder) or has the trigger switched off in a savepoint that is rolled back (ledgerTrial), so each rule is proven alone.

// mschPaired runs the legal row in a savepoint that is rolled back (the control: it passes), then the same row with the given differences, which must be refused with SQLSTATE state for why. The
// legal row is not kept, so a refused row that conflicts with it (the same key, the same reserve) still reaches the rule that is being proven.
func mschPaired(t *testing.T, tx pgx.Tx, what, state string, why ledgerWhy, legal ledgerCols, kv ...any) {
	t.Helper()
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerOKRow(t, sp, "control: the legal row of "+what+" (without the one difference)", legal)
	})
	ledgerRefusedRow(t, tx, what, state, why, legal.with(kv...))
}

// mschM3SQL is an m3_* ledger insert whose end date is a RAW SQL expression (NULL, now() + interval '30 days', ...), so that every date in these tests is the DATABASE's clock and never the test host's.
func mschM3SQL(untilExpr string) string {
	return "INSERT INTO platform.ledger (user_id, module_id, kind, acting_admin_id, reason, idempotency_key, until_at) VALUES ($1, 'm3', $2, $3, 'money schema test', $4, " + untilExpr + ") RETURNING id"
}

// mschM3Put writes an m3_enable, m3_disable or m3_expire row (admin is a uuid string, or nil for the automatic expiry) and returns its ledger id; it fails the test if the row is refused.
func mschM3Put(t *testing.T, db ledgerDB, kind, user string, admin any, untilExpr string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(testCtx(t), mschM3SQL(untilExpr), user, kind, admin, ledgerNextKey(kind)).Scan(&id); err != nil {
		t.Fatalf("a legal %s row for user %s (until_at = %s) was refused: %v", kind, user, untilExpr, err)
	}
	return id
}

// mschM3Refused runs the same insert in a savepoint and requires the refusal (state, why).
func mschM3Refused(t *testing.T, tx pgx.Tx, what, state string, why ledgerWhy, kind, user string, admin any, untilExpr string) {
	t.Helper()
	ledgerExpectRefused(t, tx, what, state, why, mschM3SQL(untilExpr), user, kind, admin, ledgerNextKey(kind))
}

// mschEntitlement reads a user's entitlement as "enabled|changed_by|has end date" (changed_by is empty for the automatic expiry), or "none".
func mschEntitlement(t *testing.T, db ledgerDB, user string) string {
	t.Helper()
	got := ledgerRows(t, db, "SELECT enabled::text, COALESCE(changed_by::text, ''), (enabled_until IS NOT NULL)::text FROM platform.m3_entitlements WHERE user_id = $1", user)
	if len(got) == 0 {
		return "none"
	}
	return got[0]
}

// mschSync reads a user's delivery row as "want_suspended|want_version|converged_version|observed", or "none".
func mschSync(t *testing.T, db ledgerDB, user string) string {
	t.Helper()
	got := ledgerRows(t, db, "SELECT want_suspended::text, want_version, converged_version, observed FROM platform.m3_sync WHERE user_id = $1", user)
	if len(got) == 0 {
		return "none"
	}
	return got[0]
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0005: the charge
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneySchema_ChargeNeedsAnItem proves ledger_item_ref_shape: a charge row MUST carry a non-blank item_ref of at most 200 BYTES, and every other kind must carry none. The legal charge is the control
// of each refusal (NULL, empty, blank, 201 bytes - counted in bytes, not characters), the 200-byte boundary is accepted on both sides, and for each of the seven other kinds the legal row passes
// and the same row with an item_ref is refused. It fails if the CHECK is missing, lets a NULL through (the NULL trap), counts characters instead of bytes, or lets a non-charge carry an item.
func TestMoneySchema_ChargeNeedsAnItem(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, c := ledgerUserA, ledgerUserC
	reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 50, 2)) // 100 credits: room for every control charge below
	charge := func(itemRef any) ledgerCols {
		return ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1).with("item_ref", itemRef)
	}
	const shape = "ledger_item_ref_shape"

	t.Run("a charge needs a non-blank item_ref of at most 200 bytes", func(t *testing.T) {
		ledgerOKRow(t, tx, "control: a charge with an item_ref", charge("item-1"))
		for _, bad := range []struct {
			name string
			ref  any
		}{
			{"no item_ref (NULL)", nil},
			{"an empty item_ref", ""},
			{"a blank item_ref (spaces)", "   "},
			{"201 bytes (201 ASCII characters)", strings.Repeat("x", 201)},
			{"201 bytes in 101 characters (the limit is in BYTES: 100 two-byte letters and one more byte)", strings.Repeat("é", 100) + "x"},
			{"202 bytes in 101 two-byte characters", strings.Repeat("é", 101)},
		} {
			ledgerRefusedCheck(t, tx, "a charge with "+bad.name, shape, charge(bad.ref))
		}
		ledgerOKRow(t, tx, "control: exactly 200 bytes (the boundary is allowed)", charge(strings.Repeat("x", 200)))
		ledgerOKRow(t, tx, "control: exactly 200 bytes in 100 two-byte characters", charge(strings.Repeat("é", 100)))
	})

	t.Run("no other kind may carry an item_ref", func(t *testing.T) {
		release := func() ledgerCols { // exactly the remainder of the 100-credit reserve (the release trigger runs before the CHECK)
			return ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, ledgerReserveRemainder(t, tx, reserve))
		}
		mschPaired(t, tx, "a grant carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, ledgerGrantCols(a, "m1", 5), "item_ref", "an-item")
		mschPaired(t, tx, "an adjustment carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, ledgerAdjustmentCols(a, "m1", 1), "item_ref", "an-item")
		mschPaired(t, tx, "a reserve carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 1, 2), "item_ref", "an-item")
		mschPaired(t, tx, "a release carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, release(), "item_ref", "an-item")
		mschPaired(t, tx, "an m3_enable carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, ledgerM3Cols("m3_enable", c), "item_ref", "an-item")
		mschPaired(t, tx, "an m3_disable carrying an item_ref", "23514", ledgerWhy{Constraint: shape}, ledgerM3Cols("m3_disable", c), "item_ref", "an-item")
		// an m3_expire is legal only for an entitlement that is enabled and due (platform_0006): the control builds one in a savepoint; the refused row fails its CHECK before any trigger looks at it
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "control: an m3_enable for user C", ledgerM3Cols("m3_enable", c))
			ledgerMakeDue(t, sp, c)
			ledgerOKRow(t, sp, "control: the legal m3_expire of a due entitlement", ledgerM3Cols("m3_expire", c))
		})
		ledgerRefusedCheck(t, tx, "an m3_expire carrying an item_ref", shape, ledgerM3Cols("m3_expire", c).with("item_ref", "an-item"))
		ledgerRequireViewsEmpty(t, tx, "after the item_ref cases")
	})
}

// TestMoneySchema_OneChargePerItem proves two rules of platform_0005 about the charge row. (1) A charge is ONE unit: units = 2, 3, 0 or NULL is refused by ledger_charge_is_one_item (with the
// row otherwise consistent - the arithmetic of ledger_shape_charge holds - so only that rule can be speaking); one unit passes. (2) Billing is per job item: a second charge for the same (job, item_ref)
// is refused by the unique index ledger_one_charge_per_item whatever idempotency key the writer derived and whatever unit or price it names, while the same item_ref on ANOTHER job, and another item
// on the same job, are allowed; a refused charge leaves no row, no balance change and no change to platform.reserves. It fails if either rule is missing or if the index also keys on the idempotency
// key (a retry with a fresh key would then bill twice).
func TestMoneySchema_OneChargePerItem(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	r1 := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 20, 2))
	r2 := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 20, 2))

	t.Run("a charge is exactly one unit", func(t *testing.T) {
		ledgerOKRow(t, tx, "control: one unit at the frozen price", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_own", 1, 1))
		for _, bad := range []struct {
			name string
			cols ledgerCols
		}{
			{"2 units at 1 (reserved_delta -2: the arithmetic is right, only the unit count is wrong)", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_own", 2, 1)},
			{"3 units at 2 (m1_vendor)", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_vendor", 3, 2)},
		} {
			ledgerRefusedCheck(t, tx, "a charge of "+bad.name, "ledger_charge_is_one_item", bad.cols)
		}
		// 0 units and NULL units break the arithmetic CHECK of the charge too, so the one-item rule is proven ALONE with that CHECK dropped in a savepoint that is rolled back (the NULL case is
		// the NULL trap: the CHECK says COALESCE(units = 1, false), so a NULL is a refusal and not a pass)
		for _, bad := range []struct {
			name string
			cols ledgerCols
		}{
			{"0 units", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_own", 0, 1)},
			{"no units (NULL)", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_own", 1, 1).with("units", nil)},
		} {
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_charge")
				ledgerRefusedCheck(t, sp, "a charge of "+bad.name+" (the shape CHECK dropped)", "ledger_charge_is_one_item", bad.cols)
			})
		}
		ledgerOKRow(t, tx, "control: a one-unit charge at the dearer unit's price", ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_vendor", 1, 2))
	})

	t.Run("a second charge for the same job item is refused, whatever the idempotency key", func(t *testing.T) {
		first := ledgerChargeCols(a, "m1", ledgerJobA1, r1, "m1_own", 1, 1).with("item_ref", "item-dup", "idempotency_key", "k-first")
		ledgerOKRow(t, tx, "control: the first charge of the item", first)
		nLedger := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger")
		reservesBefore := ledgerRows(t, tx, "SELECT reserve_id, reserved, charged, released FROM platform.reserves ORDER BY 1")
		balanceBefore := ledgerBalance(t, tx, a, "m1")
		const unique = "ledger_one_charge_per_item"
		ledgerRefusedRow(t, tx, "the same item with a DIFFERENT idempotency key", "23505", ledgerWhy{Constraint: unique}, first.with("idempotency_key", "k-second"))
		ledgerRefusedRow(t, tx, "the same item at another unit and price (the key is the item, not the amount)", "23505", ledgerWhy{Constraint: unique},
			first.with("idempotency_key", "k-third", "unit", "m1_vendor", "unit_cost", int64(2), "reserved_delta", int64(-2)))
		ledgerRefusedRow(t, tx, "the very same retry (same key, same item): a duplicate either way", "23505", ledgerWhy{Message: "duplicate key value violates unique constraint"}, first)
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"); got != nLedger {
			t.Fatalf("the refused charges left %d ledger rows behind", got-nLedger)
		}
		if got := ledgerRows(t, tx, "SELECT reserve_id, reserved, charged, released FROM platform.reserves ORDER BY 1"); strings.Join(got, ";") != strings.Join(reservesBefore, ";") {
			t.Fatalf("the refused charges changed platform.reserves: %v, was %v", got, reservesBefore)
		}
		if got := ledgerBalance(t, tx, a, "m1"); got != balanceBefore {
			t.Fatalf("the refused charges moved the balance: %+v, was %+v", got, balanceBefore)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE kind = 'charge' AND job_id = $1 AND item_ref = 'item-dup'", ledgerJobA1); n != 1 {
			t.Fatalf("%d charges bill the item twice, want exactly 1", n)
		}

		// the controls: the rule is per (job, item)
		ledgerOKRow(t, tx, "control: the SAME item_ref on ANOTHER job is another item", ledgerChargeCols(a, "m1", ledgerJobA1b, r2, "m1_own", 1, 1).with("item_ref", "item-dup"))
		ledgerOKRow(t, tx, "control: ANOTHER item on the same job", first.with("idempotency_key", "k-fourth", "item_ref", "item-other"))
		ledgerRequireViewsEmpty(t, tx, "after the one-charge-per-item cases")
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0005: the reserve
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneySchema_OneReservePerJob proves ledger_one_reserve_per_job: a job holds credits once. A second reserve for one job is refused with 23505 whatever its key and amount, also after the first has
// been charged and released (the rule is about the job, not about an open reserve); another job of the same user, another user's job and another module's job are allowed, and a refused reserve
// moves no balance. It fails if the index is missing, is not partial on the kind, or keys on the idempotency key.
func TestMoneySchema_OneReservePerJob(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, b := ledgerUserA, ledgerUserB
	const unique = "ledger_one_reserve_per_job"

	first := ledgerOKRow(t, tx, "control: the first reserve of job A1", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))
	balance := ledgerBalance(t, tx, a, "m1")
	ledgerRefusedRow(t, tx, "a second reserve for job A1 (another key, another amount)", "23505", ledgerWhy{Constraint: unique}, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 3, 2))
	ledgerRefusedRow(t, tx, "a second reserve for job A1 with the same amount", "23505", ledgerWhy{Constraint: unique}, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))
	if got := ledgerBalance(t, tx, a, "m1"); got != balance {
		t.Fatalf("the refused reserves moved the balance: %+v, was %+v", got, balance)
	}
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves WHERE job_id = $1", ledgerJobA1); n != 1 {
		t.Fatalf("platform.reserves holds %d rows for job A1, want 1", n)
	}

	ledgerOKRow(t, tx, "control: another job of the same user holds its own reserve", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2))
	ledgerOKRow(t, tx, "control: another user's job", ledgerReserveCols(b, "m1", ledgerJobB1, "m1_vendor", 5, 2))
	ledgerOKRow(t, tx, "control: a job of the other module", ledgerReserveCols(a, "m2", ledgerJobA2, "m2_accepted", 5, 1))

	// the rule is about the JOB, for the whole of its life: charge and release the reserve, then try again
	ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, first, "m1_own", 1, 1))
	ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, first, ledgerReserveRemainder(t, tx, first)))
	ledgerRefusedRow(t, tx, "a new reserve for job A1 after its reserve was charged and released", "23505", ledgerWhy{Constraint: unique}, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 1, 2))
	ledgerRequireViewsEmpty(t, tx, "after the one-reserve-per-job cases")
}

// TestMoneySchema_ReserveAtTheDearestPrice proves reserve_at_the_dearest_price: a reserve row's unit_cost must be the DEAREST price in the job's price snapshot, because module 1's price depends on
// the outcome (own, cache, vendor) and is only known at settlement. A reserve below it (one cent under, the cheap units, zero, NULL) is refused with 23514 by the trigger; at the dearest price it is
// accepted; where two units tie for dearest (module 2) either is accepted; at the dearest price with a unit that does not cost it the frozen-price key refuses. The price is the SNAPSHOT's, never
// the table's: after an admin raises m1_vendor to 7, a job frozen earlier still reserves at 2 (7 is refused) and a job frozen later reserves at 7 (2 is refused). It fails if the trigger is missing,
// reads platform.unit_costs instead of the snapshot, or lets a NULL rate through.
func TestMoneySchema_ReserveAtTheDearestPrice(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	const dearest = "reserve_at_the_dearest_price"

	t.Run("module 1: the snapshot of job A1 holds 1, 1 and 2, so the dearest price is 2", func(t *testing.T) {
		if unit, cost := ledgerDearest(t, tx, ledgerJobA1); unit != "m1_vendor" || cost != 2 {
			t.Fatalf("the dearest unit of job A1's snapshot is %s at %d, want m1_vendor at 2: the fixtures are not what this test is about", unit, cost)
		}
		for _, bad := range []struct {
			name string
			cols ledgerCols
		}{
			{"m1_own at 1", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_own", 10, 1)},
			{"m1_cache at 1", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_cache", 10, 1)},
			{"m1_vendor at 3 (dearer than the snapshot holds)", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 3)},
			{"zero", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 0).with("available_delta", int64(0), "reserved_delta", int64(0))},
			{"no rate (NULL): the trigger's comparison is NULL-safe", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2).with("unit_cost", nil)},
		} {
			ledgerRefusedCheck(t, tx, "a reserve at "+bad.name, dearest, bad.cols)
		}
		ledgerRefusedFK(t, tx, "at the dearest price with a unit that does not cost it (m1_own at 2): the price is right, the frozen-price key refuses the pair",
			"ledger_frozen_price_fk", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_own", 10, 2))
		ledgerOKRow(t, tx, "control: a reserve at the dearest price", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
	})

	t.Run("module 2: both units cost 1, so either is the dearest", func(t *testing.T) {
		ledgerRefusedCheck(t, tx, "a reserve at 2 on a module whose dearest unit costs 1", dearest, ledgerReserveCols(a, "m2", ledgerJobA2, "m2_accepted", 5, 2))
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "control: m2_bounced at 1 (a tie for dearest)", ledgerReserveCols(a, "m2", ledgerJobA2, "m2_bounced", 5, 1))
		})
		ledgerOKRow(t, tx, "control: m2_accepted at 1", ledgerReserveCols(a, "m2", ledgerJobA2, "m2_accepted", 5, 1))
	})

	t.Run("the rate is the job's SNAPSHOT, not the table: a price change moves only the jobs frozen after it", func(t *testing.T) {
		later := ledgerNewJob(t, tx, ledgerUUID("d4", 1), a, "m1", false)
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "platform_app raises m1_vendor to 7 through a named admin", "UPDATE platform.unit_costs SET cost = 7, changed_by = $1, change_reason = 'dearest-price proof' WHERE unit = 'm1_vendor'", ledgerAdminA)
		ledgerResetRole(t, tx)
		ledgerFreeze(t, tx, later) // frozen AFTER the change: its snapshot holds m1_vendor at 7
		if _, cost := ledgerDearest(t, tx, later); cost != 7 {
			t.Fatalf("the job frozen after the price change has a dearest price of %d, want 7", cost)
		}
		// job A1b was frozen by the fixtures BEFORE the change: its snapshot still says 2
		ledgerRefusedCheck(t, tx, "the OLD job reserving at the table's new price 7", dearest, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 7))
		ledgerOKRow(t, tx, "control: the OLD job reserving at its own frozen price 2", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2))
		ledgerRefusedCheck(t, tx, "the NEW job reserving at the old price 2", dearest, ledgerReserveCols(a, "m1", later, "m1_vendor", 5, 2))
		ledgerOKRow(t, tx, "control: the NEW job reserving at its frozen price 7", ledgerReserveCols(a, "m1", later, "m1_vendor", 5, 7))
		ledgerRequireViewsEmpty(t, tx, "after the dearest-price cases")
	})
}

// TestMoneySchema_ReserveNeedsTheCompleteSnapshot proves reserve_needs_the_complete_snapshot: the job's price snapshot must hold a price for EVERY unit of the job's module (as platform.unit_costs
// lists them), otherwise a settlement at a dearer unit could not be paid for by what was held. A snapshot missing the vendor unit is refused even though the price asked for is the dearest of what
// the snapshot DOES hold; a snapshot that holds the dearest unit but misses a cheap one is refused although the rate is right; a job with no snapshot is refused; another job's snapshot does not
// count; a unit added to the module after the snapshot makes it incomplete; each of them is accepted once the snapshot is complete (the paired control). It fails if the trigger counts only
// the rows it finds, or lets an empty snapshot through.
func TestMoneySchema_ReserveNeedsTheCompleteSnapshot(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	const complete = "reserve_needs_the_complete_snapshot"
	price := func(t *testing.T, job, unit string, cost int64) {
		t.Helper()
		ledgerMust(t, tx, "INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) VALUES ($1, 'm1', $2, $3)", job, unit, cost)
	}

	t.Run("a snapshot without the vendor unit, asked at the dearest price OF WHAT IT HOLDS", func(t *testing.T) {
		job := ledgerNewJob(t, tx, ledgerUUID("d5", 1), a, "m1", false)
		price(t, job, "m1_own", 1)
		price(t, job, "m1_cache", 1)
		ledgerRefusedCheck(t, tx, "m1_own at 1, the dearest of the two rows the snapshot holds", complete, ledgerReserveCols(a, "m1", job, "m1_own", 5, 1))
		ledgerRefusedCheck(t, tx, "m1_vendor at 2, the missing unit", complete, ledgerReserveCols(a, "m1", job, "m1_vendor", 5, 2))
		price(t, job, "m1_vendor", 2)
		ledgerOKRow(t, tx, "control: the same job once its snapshot is complete (at the dearest price 2)", ledgerReserveCols(a, "m1", job, "m1_vendor", 5, 2))
	})

	t.Run("a snapshot that holds the dearest unit but misses a cheap one: the rate is right, the snapshot is not", func(t *testing.T) {
		job := ledgerNewJob(t, tx, ledgerUUID("d5", 2), a, "m1", false)
		price(t, job, "m1_vendor", 2)
		price(t, job, "m1_own", 1)
		ledgerRefusedCheck(t, tx, "m1_vendor at 2 with m1_cache missing", complete, ledgerReserveCols(a, "m1", job, "m1_vendor", 5, 2))
		price(t, job, "m1_cache", 1)
		ledgerOKRow(t, tx, "control: complete now", ledgerReserveCols(a, "m1", job, "m1_vendor", 5, 2))
	})

	t.Run("no snapshot at all, and another job's snapshot does not count", func(t *testing.T) {
		// job A0 has no price row; job A1 has a complete snapshot of its own
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", ledgerJobA0); n != 0 {
			t.Fatalf("job A0 has %d price rows, want none: the fixtures changed", n)
		}
		ledgerRefusedCheck(t, tx, "a reserve for a job with no snapshot", complete, ledgerReserveCols(a, "m1", ledgerJobA0, "m1_vendor", 5, 2))
		ledgerFreeze(t, tx, ledgerJobA0)
		ledgerOKRow(t, tx, "control: the same job once it is frozen", ledgerReserveCols(a, "m1", ledgerJobA0, "m1_vendor", 5, 2))
	})

	t.Run("a unit added to the module after the snapshot makes it incomplete", func(t *testing.T) {
		// job A1's snapshot is complete (3 rows for 3 units): the control passes in a savepoint, then a fourth unit appears in the table and the same row is refused
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "control: job A1 reserving against its complete snapshot", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))
		})
		ledgerMust(t, tx, "INSERT INTO platform.unit_costs (unit, module_id, description, cost) VALUES ('m1_extra', 'm1', 'a unit added after the snapshot', 1)")
		ledgerRefusedCheck(t, tx, "job A1 reserving once the module has a unit its snapshot lacks", complete, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))
		ledgerRequireViewsEmpty(t, tx, "after the snapshot cases")
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0005: platform.reserves, the release, and the roles
// ---------------------------------------------------------------------------------------------------------------------

// mschReservesDiff is a recompute of platform.reserves from the ledger, written HERE and compared with the table both ways: a reserve's held credits are its row's reserved_delta, charged is the sum
// of the consumed credits of the charges that name it, released the sum of the releases that name it. It returns one row per disagreement and none when the table is exactly what the ledger says.
const mschReservesDiff = `
WITH recomputed AS (
  SELECT r.id AS reserve_id, r.user_id, r.module_id, r.job_id, (-r.available_delta)::bigint AS reserved,
         COALESCE(sum(-s.reserved_delta) FILTER (WHERE s.kind = 'charge'), 0)::bigint AS charged,
         COALESCE(sum(s.available_delta) FILTER (WHERE s.kind = 'release'), 0)::bigint AS released
    FROM platform.ledger r LEFT JOIN platform.ledger s ON s.reserve_id = r.id
   WHERE r.kind = 'reserve'
   GROUP BY r.id, r.user_id, r.module_id, r.job_id, r.available_delta),
stored AS (SELECT reserve_id, user_id, module_id, job_id, reserved, charged, released FROM platform.reserves)
SELECT 'only recomputed', reserve_id::text, charged::text, released::text FROM (SELECT * FROM recomputed EXCEPT SELECT * FROM stored) x
UNION ALL
SELECT 'only stored', reserve_id::text, charged::text, released::text FROM (SELECT * FROM stored EXCEPT SELECT * FROM recomputed) y
ORDER BY 1, 2`

// TestMoneySchema_ReservesFollowTheLedger proves the single writer of platform.reserves, the ledger trigger. A reserve row appears with the reserve (what was held, nothing charged or released); `charged`
// follows every charge and `released` follows the release; and after a mixed sequence over three users, two modules and four reserves (untouched, partly charged, fully charged and never released, charged
// and released) the table equals a recompute from the ledger written in this test (mschReservesDiff). The controls make the comparison earn its keep: with the trigger off one more charge makes the
// recompute differ, and so does an edit of the table behind the ledger's back. It fails if the trigger writes a wrong amount, misses a kind, or if the comparison cannot see a difference.
func TestMoneySchema_ReservesFollowTheLedger(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, b := ledgerUserA, ledgerUserB
	row := func(reserve int64) string {
		return ledgerString(t, tx, "SELECT reserve_id || '|' || user_id::text || '|' || module_id || '|' || job_id::text || '|' || reserved || '|' || charged || '|' || released FROM platform.reserves WHERE reserve_id = $1", reserve)
	}

	var rA int64
	t.Run("a reserve row appears with the reserve, and charged and released follow", func(t *testing.T) {
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves"); n != 0 {
			t.Fatalf("platform.reserves holds %d rows before any reserve", n)
		}
		rA = ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
		if got, want := row(rA), fmt.Sprintf("%d|%s|m1|%s|20|0|0", rA, a, ledgerJobA1); got != want {
			t.Fatalf("the reserves row of a fresh reserve reads %q, want %q", got, want)
		}
		for i := 0; i < 3; i++ {
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, rA, "m1_own", 1, 1))
		}
		if got, want := row(rA), fmt.Sprintf("%d|%s|m1|%s|20|3|0", rA, a, ledgerJobA1); got != want {
			t.Fatalf("after three charges of 1 the row reads %q, want %q", got, want)
		}
		for i := 0; i < 2; i++ {
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, rA, "m1_vendor", 1, 2))
		}
		if got, want := row(rA), fmt.Sprintf("%d|%s|m1|%s|20|7|0", rA, a, ledgerJobA1); got != want {
			t.Fatalf("after two more charges of 2 the row reads %q, want %q", got, want)
		}
		ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, rA, 13))
		if got, want := row(rA), fmt.Sprintf("%d|%s|m1|%s|20|7|13", rA, a, ledgerJobA1); got != want {
			t.Fatalf("after the release of the remaining 13 the row reads %q, want %q", got, want)
		}
	})

	t.Run("the table equals a recompute from the ledger after a mixed sequence", func(t *testing.T) {
		rB := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2)) // untouched: 10 | 0 | 0
		rC := ledgerPut(t, tx, ledgerReserveCols(b, "m1", ledgerJobB1, "m1_vendor", 8, 2))  // fully charged, never released: 16 | 16 | 0
		for i := 0; i < 8; i++ {
			ledgerPut(t, tx, ledgerChargeCols(b, "m1", ledgerJobB1, rC, "m1_vendor", 1, 2))
		}
		rD := ledgerPut(t, tx, ledgerReserveCols(a, "m2", ledgerJobA2, "m2_accepted", 6, 1)) // module 2: two accepted and a bounce, three released: 6 | 3 | 3
		ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, rD, "m2_accepted", 1, 1))
		ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, rD, "m2_accepted", 1, 1))
		ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, rD, "m2_bounced", 1, 1))
		ledgerPut(t, tx, ledgerReleaseCols(a, "m2", ledgerJobA2, rD, ledgerReserveRemainder(t, tx, rD)))
		for reserve, want := range map[int64]string{rB: "10|0|0", rC: "16|16|0", rD: "6|3|3"} {
			if got := ledgerString(t, tx, "SELECT reserved || '|' || charged || '|' || released FROM platform.reserves WHERE reserve_id = $1", reserve); got != want {
				t.Fatalf("reserve %d reads %s, want %s", reserve, got, want)
			}
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves"); n != 4 {
			t.Fatalf("platform.reserves holds %d rows, want the 4 reserves of this test", n)
		}
		if diff := ledgerRows(t, tx, mschReservesDiff); len(diff) != 0 {
			t.Fatalf("platform.reserves differs from the recompute from the ledger: %v", diff)
		}
		ledgerRequireViewsEmpty(t, tx, "after the mixed sequence")

		// the controls: the comparison can see a difference
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_reserves")
			ledgerPut(t, sp, ledgerChargeCols(a, "m1", ledgerJobA1b, rB, "m1_own", 1, 1))
			diff := ledgerRows(t, sp, mschReservesDiff)
			want := []string{fmt.Sprintf("only recomputed|%d|1|0", rB), fmt.Sprintf("only stored|%d|0|0", rB)}
			if strings.Join(diff, ";") != strings.Join(want, ";") {
				t.Fatalf("with the trigger off, one charge unseen by platform.reserves gives the difference %v, want %v: the recompute cannot fail", diff, want)
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "UPDATE platform.reserves SET charged = charged + 1 WHERE reserve_id = $1", rB)
			if diff := ledgerRows(t, sp, mschReservesDiff); len(diff) != 2 {
				t.Fatalf("an edit of platform.reserves behind the ledger's back gives the difference %v, want the two sides of one row", diff)
			}
		})
	})
}

// TestMoneySchema_OneJobCannotSpendAnothersReservation is the test the per-reserve cap exists for. A user's credits are POOLED in one balance: `reserved` is the sum of every open reserve, so the balance
// check alone cannot tell whose credits a charge consumes. Here user A holds three reserves on three jobs (10, 10 and 100 credits): job A1 is charged exactly its 10 and the 11th credit is refused by
// reserves_not_overspent EVEN THOUGH the pool holds 110 credits that could pay it (the paired control switches the per-reserve trigger off in a savepoint: the same row is then accepted and the pool is
// what pays, so the per-reserve cap and nothing else stood in the way). The boundary holds on both sides for job A1b (a 2-credit charge against 1 credit left is refused, the last credit passes,
// one more is refused), a charge cannot NAME another job's reserve (the foreign key), and no refusal leaves a trace in the ledger, the balance or platform.reserves. It fails if a job can spend a
// credit another job reserved.
func TestMoneySchema_OneJobCannotSpendAnothersReservation(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	const capRule = "reserves_not_overspent"
	pool := ledgerNewJob(t, tx, ledgerUUID("d6", 1), a, "m1", true)
	rA := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))  // 10 credits
	rB := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2)) // 10 credits
	rPool := ledgerPut(t, tx, ledgerReserveCols(a, "m1", pool, "m1_vendor", 50, 2))     // 100 credits that no charge below touches: the pool that "could pay"
	ledgerRequireBalance(t, tx, "three reserves of 10, 10 and 100", a, "m1", 880, 120)
	snapshot := func() string {
		return strings.Join(ledgerRows(t, tx, "SELECT reserve_id, reserved, charged, released FROM platform.reserves ORDER BY 1"), ";") + "#" +
			fmt.Sprint(ledgerBalance(t, tx, a, "m1")) + "#" + fmt.Sprint(ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"))
	}

	t.Run("job A1 is charged exactly its own 10 credits, and the 11th is refused while the pool could pay", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, rA, "m1_own", 1, 1))
		}
		ledgerRequireBalance(t, tx, "A1's reserve is spent, the other 110 credits are still reserved", a, "m1", 880, 110)
		before := snapshot()
		over := ledgerChargeCols(a, "m1", ledgerJobA1, rA, "m1_own", 1, 1)
		ledgerRefusedRow(t, tx, "an 11th credit against reserve A1 while the pool holds 110", "23514", ledgerWhy{Constraint: capRule, Table: "reserves"}, over)
		ledgerRefusedRow(t, tx, "a charge of 2 credits (one m1_vendor unit) against reserve A1, which has nothing left", "23514", ledgerWhy{Constraint: capRule, Table: "reserves"},
			ledgerChargeCols(a, "m1", ledgerJobA1, rA, "m1_vendor", 1, 2))
		if after := snapshot(); after != before {
			t.Fatalf("a refused charge left a trace:\n  before %s\n  after  %s", before, after)
		}
		// the control: with the per-reserve trigger off the SAME row is accepted - the pooled balance would have paid, so the cap is what refused it
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_reserves")
			ledgerOKRow(t, sp, "control: the same charge with the per-reserve wall off", over)
			ledgerRequireBalance(t, sp, "the pool paid for it", a, "m1", 880, 109)
			if o := ledgerOverdrawn(t, sp); len(o) != 1 || !strings.HasPrefix(o[0], fmt.Sprintf("%d|", rA)) {
				t.Fatalf("reserve_overdrawn lists %v, want reserve %d alone: the alarm must see the overspend the wall refused", o, rA)
			}
		})
	})

	t.Run("the boundary of job A1b, on both sides", func(t *testing.T) {
		for i := 0; i < 9; i++ { // 9 of its 10 credits
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1b, rB, "m1_own", 1, 1))
		}
		before := snapshot()
		ledgerRefusedRow(t, tx, "2 credits against the 1 credit left", "23514", ledgerWhy{Constraint: capRule, Table: "reserves"}, ledgerChargeCols(a, "m1", ledgerJobA1b, rB, "m1_vendor", 1, 2))
		if after := snapshot(); after != before {
			t.Fatalf("a refused charge left a trace:\n  before %s\n  after  %s", before, after)
		}
		ledgerOKRow(t, tx, "control: exactly the last credit", ledgerChargeCols(a, "m1", ledgerJobA1b, rB, "m1_own", 1, 1))
		ledgerRefusedRow(t, tx, "one credit more", "23514", ledgerWhy{Constraint: capRule, Table: "reserves"}, ledgerChargeCols(a, "m1", ledgerJobA1b, rB, "m1_own", 1, 1))
		if got := ledgerString(t, tx, "SELECT reserved || '|' || charged || '|' || released FROM platform.reserves WHERE reserve_id = $1", rB); got != "10|10|0" {
			t.Fatalf("reserve B reads %s, want 10|10|0", got)
		}
	})

	t.Run("a charge cannot name another job's reserve", func(t *testing.T) {
		// the pool's reserve belongs to the third job: a charge on job A1b that names it is refused by the key, so the only way to spend a reserve is to charge ITS OWN job
		ledgerRefusedFK(t, tx, "a charge on job A1b naming the pool's reserve", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1b, rPool, "m1_own", 1, 1))
		ledgerOKRow(t, tx, "control: the pool's own job charging the pool's reserve", ledgerChargeCols(a, "m1", pool, rPool, "m1_own", 1, 1))
		ledgerRequireViewsEmpty(t, tx, "after the cap cases")
	})
}

// TestMoneySchema_ReleaseIsTheRemainder proves ledger_check_release and ledger_one_release_per_reserve. A release returns exactly reserved - charged - released of the reserve it names: a release one
// credit too small, one too large, the whole reserve ignoring the charges, a nonsense amount, are each refused with 23514 release_is_the_remainder; the exact remainder passes and platform.reserves says
// so. A second release is refused (the remainder is now 0); with the remainder trigger switched off in a savepoint the unique index ledger_one_release_per_reserve refuses it alone. A fully charged
// reserve has a remainder of 0, so no release can be written for it. A charge after a release is refused by reserves_not_overspent (another job's reserve keeps the pool above zero, so only the cap can
// speak). It fails if a release can return a wrong amount, twice, or a reserve can be charged after it was released.
func TestMoneySchema_ReleaseIsTheRemainder(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	const remainder = "release_is_the_remainder"
	r := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2)) // 20 credits
	ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 10, 2))     // another job's 20 credits: the pool never runs dry below
	for i := 0; i < 3; i++ {
		ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1)) // 3 charged: 17 left
	}
	if rem := ledgerReserveRemainder(t, tx, r); rem != 17 {
		t.Fatalf("the reserve has %d credits left, want 17", rem)
	}

	t.Run("a release that is not exactly the remainder is refused", func(t *testing.T) {
		for _, amount := range []int64{16, 18, 20, 3, 1, 1000} {
			ledgerRefusedRow(t, tx, fmt.Sprintf("a release of %d from a reserve with 17 left", amount), "23514", ledgerWhy{Constraint: remainder}, ledgerReleaseCols(a, "m1", ledgerJobA1, r, amount))
		}
		ledgerRequireBalance(t, tx, "after the refused releases (nothing moved)", a, "m1", 960, 37)
		ledgerOKRow(t, tx, "control: exactly the remainder, 17", ledgerReleaseCols(a, "m1", ledgerJobA1, r, 17))
		ledgerRequireBalance(t, tx, "after the release", a, "m1", 977, 20)
		if got := ledgerString(t, tx, "SELECT reserved || '|' || charged || '|' || released FROM platform.reserves WHERE reserve_id = $1", r); got != "20|3|17" {
			t.Fatalf("platform.reserves reads %s after the release, want 20|3|17", got)
		}
	})

	t.Run("a second release is refused, by the remainder rule and, alone, by the unique index", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "a second release of 17", "23514", ledgerWhy{Constraint: remainder}, ledgerReleaseCols(a, "m1", ledgerJobA1, r, 17))
		ledgerRefusedRow(t, tx, "a second release of 1", "23514", ledgerWhy{Constraint: remainder}, ledgerReleaseCols(a, "m1", ledgerJobA1, r, 1))
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_check_release")
			ledgerRefusedRow(t, sp, "with the remainder rule off, a second release of one credit", "23505", ledgerWhy{Constraint: "ledger_one_release_per_reserve"}, ledgerReleaseCols(a, "m1", ledgerJobA1, r, 1))
		})
	})

	t.Run("a charge after the release is refused by the per-reserve cap", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "a charge of 1 against a reserve that was charged 3 and released 17 of 20", "23514", ledgerWhy{Constraint: "reserves_not_overspent", Table: "reserves"},
			ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1))
		ledgerRequireBalance(t, tx, "after the refused charge", a, "m1", 977, 20)
	})

	t.Run("a fully charged reserve has a remainder of 0: no release can be written for it", func(t *testing.T) {
		job := ledgerNewJob(t, tx, ledgerUUID("d7", 1), a, "m1", true)
		full := ledgerPut(t, tx, ledgerReserveCols(a, "m1", job, "m1_vendor", 2, 2)) // 4 credits
		for i := 0; i < 4; i++ {
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", job, full, "m1_own", 1, 1))
		}
		if rem := ledgerReserveRemainder(t, tx, full); rem != 0 {
			t.Fatalf("the fully charged reserve has %d credits left, want 0", rem)
		}
		ledgerRefusedRow(t, tx, "a release of 1 from a fully charged reserve", "23514", ledgerWhy{Constraint: remainder}, ledgerReleaseCols(a, "m1", job, full, 1))
		ledgerRefusedRow(t, tx, "a release of 4 (the whole reserve) from a fully charged one", "23514", ledgerWhy{Constraint: remainder}, ledgerReleaseCols(a, "m1", job, full, 4))
		ledgerRequireViewsEmpty(t, tx, "after the release cases")
	})
}

// TestMoneySchema_NoRoleWritesReserves proves platform.reserves has ONE writer. For platform_app and platform_admin: SELECT works and INSERT, UPDATE, DELETE and TRUNCATE are each refused with 42501 (the
// paired control: the superuser's same statements work, each touching a real row). What the roles CAN do is put a row in the ledger, and platform.reserves follows through the trigger (which is SECURITY
// DEFINER): a reserve written by each role appears in the table, written by neither. It fails if a role gains a write privilege on the table, or if the trigger stops being able to write it for the
// roles that use it.
func TestMoneySchema_NoRoleWritesReserves(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	r := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2))
	grant := ledgerInt64(t, tx, "SELECT id FROM platform.ledger WHERE user_id = $1 AND module_id = 'm1' AND kind = 'grant'", a)
	stmts := []struct {
		op, sql string
		args    []any
	}{
		{"INSERT", "INSERT INTO platform.reserves (reserve_id, user_id, module_id, job_id, reserved) VALUES ($1, $2, 'm1', $3, 5)", []any{grant, a, ledgerJobA1b}},
		{"UPDATE", "UPDATE platform.reserves SET charged = charged + 1 WHERE reserve_id = $1", []any{r}},
		{"DELETE", "DELETE FROM platform.reserves WHERE reserve_id = $1", []any{r}},
		{"TRUNCATE", "TRUNCATE platform.reserves", nil},
	}
	for _, s := range stmts { // the control: each statement works for the superuser, on a real row
		s := s
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			tag := ledgerExpectOK(t, sp, "control: the superuser's "+s.op+" on platform.reserves", s.sql, s.args...)
			if s.op != "TRUNCATE" && tag.RowsAffected() != 1 {
				t.Fatalf("control: the superuser's %s touched %d rows, want 1", s.op, tag.RowsAffected())
			}
		})
	}
	// a job of its own for each role to reserve against through the ledger (created here, as the superuser: platform_admin cannot create jobs)
	jobs := map[string]string{
		"platform_app":   ledgerNewJob(t, tx, ledgerUUID("d8", 1), a, "m1", true),
		"platform_admin": ledgerNewJob(t, tx, ledgerUUID("d8", 2), a, "m1", true),
	}
	for _, role := range []string{"platform_app", "platform_admin"} {
		role := role
		t.Run(role, func(t *testing.T) {
			ledgerSwitchRole(t, tx, role)
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves"); n < 1 {
				t.Fatalf("%s sees %d rows in platform.reserves: SELECT is granted and a refusal on an empty table would prove nothing", role, n)
			}
			for _, s := range stmts {
				ledgerExpectRefused(t, tx, role+" "+s.op+" on platform.reserves", "42501", ledgerWhy{Message: "permission denied for table reserves"}, s.sql, s.args...)
			}
			before := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves")
			ledgerOKRow(t, tx, "control: "+role+" reserves through the ledger", ledgerReserveCols(a, "m1", jobs[role], "m1_vendor", 1, 2))
			if after := ledgerInt64(t, tx, "SELECT count(*) FROM platform.reserves"); after != before+1 {
				t.Fatalf("%s's ledger reserve left platform.reserves with %d rows, want %d: the trigger is the writer and it must work for this role", role, after, before+1)
			}
			ledgerResetRole(t, tx)
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0006: the entitlement flow
// ---------------------------------------------------------------------------------------------------------------------

// TestM3Schema_EntitlementFollowsTheLedger proves that platform.m3_entitlements and platform.m3_sync are written by the ledger and by nothing else. An m3_enable with an end date creates the entitlement
// (enabled, enabled_until = the row's until_at, changed_by = the acting admin, changed_at = the row's created_at) and its delivery row (want_suspended false, want_version 1, converged_version 0, observed
// 'unknown'); an m3_disable flips it (want_version 2, want_suspended true, the date cleared, the other admin named); a re-enable bumps again; a disable of an already-disabled user and an enable with a NEW
// date but no change of state do NOT bump want_version (only a change of `enabled` is something module 3 must be told); a first event for a user is an insert (disable: want_suspended true), and one
// user's events never touch another's delivery row. The paired controls are the bumps themselves. It fails if the entitlement drifts from its ledger row or if want_version moves when the wanted
// state did not.
func TestM3Schema_EntitlementFollowsTheLedger(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b, c := ledgerUserA, ledgerUserB, ledgerUserC
	adminA, adminB := ledgerAdminA, ledgerAdminB

	enable := mschM3Put(t, tx, "m3_enable", a, adminA, "now() + interval '30 days'")
	if got := ledgerBool(t, tx, `SELECT e.enabled AND e.enabled_until = l.until_at AND e.changed_by = l.acting_admin_id AND e.changed_at = l.created_at
		FROM platform.m3_entitlements e JOIN platform.ledger l ON l.id = $1 AND l.user_id = e.user_id WHERE e.user_id = $2`, enable, a); !got {
		t.Fatalf("the entitlement written by the m3_enable is not (enabled, enabled_until = until_at, changed_by = the admin, changed_at = created_at): %s", mschEntitlement(t, tx, a))
	}
	if got := ledgerRows(t, tx, "SELECT want_suspended::text, want_version, converged_version, observed, COALESCE(observed_at::text, ''), attempts, COALESCE(last_attempt_at::text, ''), COALESCE(last_error, '') FROM platform.m3_sync WHERE user_id = $1", a); len(got) != 1 || got[0] != "false|1|0|unknown||0||" {
		t.Fatalf("the delivery row written by the entitlement trigger reads %v, want [false|1|0|unknown||0||]", got)
	}

	step := func(what, user string, wantEntitlement, wantSync string) {
		t.Helper()
		if got := mschEntitlement(t, tx, user); got != wantEntitlement {
			t.Fatalf("%s: the entitlement reads %q, want %q", what, got, wantEntitlement)
		}
		if got := mschSync(t, tx, user); got != wantSync {
			t.Fatalf("%s: the delivery row reads %q, want %q", what, got, wantSync)
		}
	}
	step("after the enable", a, "true|"+adminA+"|true", "false|1|0|unknown")

	mschM3Put(t, tx, "m3_disable", a, adminB, "NULL")
	step("after the disable by admin B (the state changed: want_version 2, the date cleared)", a, "false|"+adminB+"|false", "true|2|0|unknown")

	mschM3Put(t, tx, "m3_disable", a, adminA, "NULL")
	step("after a second disable (no change of state: want_version stays 2, the acting admin is the new one)", a, "false|"+adminA+"|false", "true|2|0|unknown")

	mschM3Put(t, tx, "m3_enable", a, adminA, "NULL")
	step("after the re-enable (the state changed again: want_version 3)", a, "true|"+adminA+"|false", "false|3|0|unknown")

	mschM3Put(t, tx, "m3_enable", a, adminB, "now() + interval '60 days'")
	step("after an enable with a NEW date and no change of state (want_version stays 3, the date is set)", a, "true|"+adminB+"|true", "false|3|0|unknown")

	mschM3Put(t, tx, "m3_disable", b, adminA, "NULL")
	step("a first event for user B that is a DISABLE: the entitlement is inserted disabled and module 3 must be told to suspend", b, "false|"+adminA+"|false", "true|1|0|unknown")
	mschM3Put(t, tx, "m3_enable", c, adminA, "NULL")
	step("a first event for user C that is an enable", c, "true|"+adminA+"|false", "false|1|0|unknown")
	step("user A's delivery row after the events of B and C (one user's events never touch another's)", a, "true|"+adminB+"|true", "false|3|0|unknown")

	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_entitlements e LEFT JOIN platform.m3_sync s ON s.user_id = e.user_id WHERE s.user_id IS NULL"); n != 0 {
		t.Fatalf("%d entitlements have no delivery row: the entitlement trigger must write one for every insert", n)
	}
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_sync"); n != 3 {
		t.Fatalf("%d delivery rows for 3 entitlements", n)
	}
	// nothing the ledger does here is money
	ledgerRequireViewsEmpty(t, tx, "after the entitlement events")
}

// TestM3Schema_UntilAtShape proves ledger_until_at_shape: only an m3_enable row may carry an end date and it must be after the row's own created_at. An end date in the past, an end date EQUAL to
// created_at (the boundary) and an end date on any other kind (m3_disable, m3_expire, grant, adjustment, reserve, charge, release) are each refused with 23514; one microsecond after created_at is
// accepted, a far-future date on an m3_enable is accepted, and no date (NULL) is accepted for every kind (the CHECK is NULL-safe). The dates are the database's clock (now() is the transaction's, and
// created_at defaults to it). It fails if the CHECK is missing, off by one, or lets a non-enable carry a date.
func TestM3Schema_UntilAtShape(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, c := ledgerUserA, ledgerUserC
	const shape = "ledger_until_at_shape"
	adminA := ledgerAdminA

	t.Run("an m3_enable: the date must be after created_at", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) { mschM3Put(t, sp, "m3_enable", a, adminA, "NULL") })
		ledgerTrial(t, tx, func(sp pgx.Tx) { mschM3Put(t, sp, "m3_enable", a, adminA, "now() + interval '1 microsecond'") }) // the boundary, one microsecond on the legal side
		ledgerTrial(t, tx, func(sp pgx.Tx) { mschM3Put(t, sp, "m3_enable", a, adminA, "now() + interval '30 days'") })
		mschM3Refused(t, tx, "an end date in the past", "23514", ledgerWhy{Constraint: shape}, "m3_enable", a, adminA, "now() - interval '1 hour'")
		mschM3Refused(t, tx, "an end date equal to created_at (the boundary)", "23514", ledgerWhy{Constraint: shape}, "m3_enable", a, adminA, "now()")
		mschM3Refused(t, tx, "an end date one microsecond BEFORE created_at", "23514", ledgerWhy{Constraint: shape}, "m3_enable", a, adminA, "now() - interval '1 microsecond'")
	})

	t.Run("no other kind may carry an end date", func(t *testing.T) {
		const far = "2999-01-01T00:00:00Z"
		r := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
		mschPaired(t, tx, "an m3_disable carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerM3Cols("m3_disable", a), "until_at", far)
		mschPaired(t, tx, "a grant carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerGrantCols(a, "m1", 5), "until_at", far)
		mschPaired(t, tx, "an adjustment carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerAdjustmentCols(a, "m1", 1), "until_at", far)
		mschPaired(t, tx, "a reserve carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 1, 2), "until_at", far)
		mschPaired(t, tx, "a charge carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1), "until_at", far)
		mschPaired(t, tx, "a release carrying an end date", "23514", ledgerWhy{Constraint: shape}, ledgerReleaseCols(a, "m1", ledgerJobA1, r, 20), "until_at", far)
		// an m3_expire is legal only for a due entitlement: the control builds one in a savepoint; the refused row fails its CHECK before any trigger looks at it
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			mschM3Put(t, sp, "m3_enable", c, adminA, "NULL")
			ledgerMakeDue(t, sp, c)
			mschM3Put(t, sp, "m3_expire", c, nil, "NULL")
		})
		ledgerRefusedCheck(t, tx, "an m3_expire carrying an end date", shape, ledgerM3Cols("m3_expire", c).with("until_at", far))
		ledgerOKRow(t, tx, "control: a far-future end date IS legal on an m3_enable", ledgerM3Cols("m3_enable", a).with("until_at", far))
	})
}

// TestM3Schema_ExpireIsRefusedUnlessDue proves m3_expire_is_due, with the DATABASE's clock and a real, bounded wait. An m3_expire row is refused (23514, constraint m3_expire_is_due) for a user with NO
// entitlement, for a DISABLED one, for one enabled with NO end date, and for one whose date is still in the future; nothing is left behind by a refusal. Then an entitlement enabled with a date two seconds
// ahead is committed in one transaction, the expiry is refused at once (the date has not passed), the test waits - polling the DATABASE's clock, bounded at 15 s - until the date has passed, and
// in a LATER transaction the expiry is accepted: the entitlement is disabled, its date and its admin are cleared (changed_by NULL: the system did it) and want_version rises with want_suspended
// true. Expiring a second time is refused. It fails if an entitlement can be expired early or twice, if the clock is the application's, or if the expiry does not reach module 3's delivery row.
func TestM3Schema_ExpireIsRefusedUnlessDue(t *testing.T) {
	_, admin := migratedScratch(t)
	ledgerSeed(t, admin) // committed: this is a scratch database, and the expiry below needs a committed enable and a LATER transaction
	a, b, c, d := ledgerUserA, ledgerUserB, ledgerUserC, ledgerAdminB
	adminA := ledgerAdminA
	const due = "m3_expire_is_due"

	t.Run("refused: no entitlement, disabled, no end date, a future end date", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		defer func() { _ = tx.Rollback(testCtx(t)) }()
		mschM3Refused(t, tx, "user C has no entitlement", "23514", ledgerWhy{Constraint: due}, "m3_expire", c, nil, "NULL")
		mschM3Put(t, tx, "m3_disable", b, adminA, "NULL")
		mschM3Refused(t, tx, "user B's entitlement is disabled", "23514", ledgerWhy{Constraint: due}, "m3_expire", b, nil, "NULL")
		mschM3Put(t, tx, "m3_enable", c, adminA, "NULL")
		mschM3Refused(t, tx, "user C is enabled with NO end date: nothing to expire", "23514", ledgerWhy{Constraint: due}, "m3_expire", c, nil, "NULL")
		mschM3Put(t, tx, "m3_enable", d, adminA, "now() + interval '30 days'")
		mschM3Refused(t, tx, "the end date of the enabled user is in the future", "23514", ledgerWhy{Constraint: due}, "m3_expire", d, nil, "NULL")
		// the paired control: the same row IS accepted once the entitlement is due (the date moved into the past by the superuser, which is what the passing of time does)
		ledgerMakeDue(t, tx, d)
		mschM3Put(t, tx, "m3_expire", d, nil, "NULL")
		if got := mschEntitlement(t, tx, d); got != "false||false" {
			t.Fatalf("the expired entitlement reads %q, want false||false (disabled, no admin, no date)", got)
		}
		// a refusal leaves the entitlement and its delivery row exactly as they were
		if got := mschEntitlement(t, tx, c); got != "true|"+adminA+"|false" {
			t.Fatalf("user C's entitlement reads %q after the refused expiries, want it untouched", got)
		}
	})

	t.Run("accepted when due: a committed enable, a wait on the database's clock, a later transaction", func(t *testing.T) {
		const untilExpr = "now() + interval '2 seconds'" // created_at is the same now(): the date is exactly two seconds after the row
		mschM3Put(t, admin, "m3_enable", a, adminA, untilExpr)
		if got := mschEntitlement(t, admin, a); got != "true|"+adminA+"|true" {
			t.Fatalf("the committed enable left the entitlement %q, want true|%s|true", got, adminA)
		}
		if got := mschSync(t, admin, a); got != "false|1|0|unknown" {
			t.Fatalf("the delivery row reads %q, want false|1|0|unknown", got)
		}
		// at once: the date has not passed
		tx := ledgerBegin(t, admin)
		mschM3Refused(t, tx, "the date is two seconds ahead", "23514", ledgerWhy{Constraint: due}, "m3_expire", a, nil, "NULL")
		_ = tx.Rollback(testCtx(t))

		// the wait, on the DATABASE's clock, bounded
		deadline := time.Now().Add(15 * time.Second)
		for !ledgerBool(t, admin, "SELECT clock_timestamp() >= enabled_until FROM platform.m3_entitlements WHERE user_id = $1", a) {
			if time.Now().After(deadline) {
				t.Fatal("the database's clock did not pass the entitlement's end date within 15 seconds: the wait is bounded and the date was 2 seconds ahead")
			}
			time.Sleep(100 * time.Millisecond)
		}

		// a LATER transaction (autocommit): now it is due
		expire := mschM3Put(t, admin, "m3_expire", a, nil, "NULL")
		if got := mschEntitlement(t, admin, a); got != "false||false" {
			t.Fatalf("the expired entitlement reads %q, want false||false (disabled, changed_by cleared, no end date)", got)
		}
		if !ledgerBool(t, admin, "SELECT e.changed_at = l.created_at FROM platform.m3_entitlements e JOIN platform.ledger l ON l.id = $1 AND l.user_id = e.user_id WHERE e.user_id = $2", expire, a) {
			t.Fatalf("changed_at of the expired entitlement is not the created_at of the m3_expire row")
		}
		if got := mschSync(t, admin, a); got != "true|2|0|unknown" {
			t.Fatalf("the delivery row after the expiry reads %q, want true|2|0|unknown (module 3 must be told to suspend)", got)
		}

		// expiring twice
		tx = ledgerBegin(t, admin)
		mschM3Refused(t, tx, "a second expiry of the same entitlement", "23514", ledgerWhy{Constraint: due}, "m3_expire", a, nil, "NULL")
		_ = tx.Rollback(testCtx(t))
		if got := mschSync(t, admin, a); got != "true|2|0|unknown" {
			t.Fatalf("the refused second expiry changed the delivery row to %q", got)
		}
	})
}

// mschDrift is the recompute of the entitlements from the ledger, written HERE: for every user the LAST m3 ledger event (by id) says what the entitlement must be - m3_enable: enabled, its until_at, its admin;
// m3_disable: disabled, no date, its admin; m3_expire: disabled, no date, no admin - with changed_at the row's created_at; and the delivery row must say want_suspended = NOT enabled and a want_version of
// 1 plus the number of times the state flipped along the user's events. It returns one row per disagreement and none when the tables are exactly what the ledger says.
func mschDrift(t *testing.T, db ledgerDB) []string {
	t.Helper()
	diff := ledgerRows(t, db, `
WITH last AS (
  SELECT DISTINCT ON (user_id) user_id, kind, until_at, acting_admin_id, created_at
    FROM platform.ledger WHERE kind IN ('m3_enable', 'm3_disable', 'm3_expire') ORDER BY user_id, id DESC),
implied AS (
  SELECT user_id, (kind = 'm3_enable') AS enabled, CASE WHEN kind = 'm3_enable' THEN until_at END AS enabled_until,
         CASE WHEN kind <> 'm3_expire' THEN acting_admin_id END AS changed_by, created_at AS changed_at FROM last),
stored AS (SELECT user_id, enabled, enabled_until, changed_by, changed_at FROM platform.m3_entitlements)
SELECT 'entitlement only implied', user_id::text, enabled::text FROM (SELECT * FROM implied EXCEPT SELECT * FROM stored) x
UNION ALL
SELECT 'entitlement only stored', user_id::text, enabled::text FROM (SELECT * FROM stored EXCEPT SELECT * FROM implied) y
ORDER BY 1, 2`)
	diff = append(diff, ledgerRows(t, db, `
WITH ev AS (SELECT user_id, id, (kind = 'm3_enable') AS st FROM platform.ledger WHERE kind IN ('m3_enable', 'm3_disable', 'm3_expire')),
flips AS (SELECT user_id, id, st, lag(st) OVER (PARTITION BY user_id ORDER BY id) AS prev FROM ev),
implied AS (SELECT user_id, 1 + count(*) FILTER (WHERE prev IS NOT NULL AND st <> prev) AS want_version, NOT (array_agg(st ORDER BY id DESC))[1] AS want_suspended FROM flips GROUP BY user_id),
stored AS (SELECT user_id, want_version, want_suspended FROM platform.m3_sync)
SELECT 'sync only implied', user_id::text, want_version::text FROM (SELECT * FROM implied EXCEPT SELECT * FROM stored) x
UNION ALL
SELECT 'sync only stored', user_id::text, want_version::text FROM (SELECT * FROM stored EXCEPT SELECT * FROM implied) y
ORDER BY 1, 2`)...)
	return diff
}

// TestM3Schema_EntitlementEqualsLastEvent is the entitlement's drift check, as balance_drift is the balance's. Twelve users are enabled with a date two seconds ahead (committed), the test waits on the
// database's clock until every date has passed, and then 250 pseudo-random events (a FIXED seed) hit them: enable (with no date or a far one), disable (either admin), and expire. The first eight users are
// expired first, so every user has an entitlement that is due at the start; after that an expiry is tried at random and the test's OWN model says whether it is legal - a legal one must be accepted, an
// illegal one must be refused with m3_expire_is_due. Every 25 events and at the end the entitlement and the delivery row equal what the LAST ledger event of each user implies (mschDrift, a SQL
// recompute) and equal the test's own Go model (enabled flag, want_version). The paired control: the recompute sees an entitlement whose trigger is switched off, and one edited behind the ledger's
// back. It fails if the stored entitlement or the wanted version is ever not what the ledger says.
func TestM3Schema_EntitlementEqualsLastEvent(t *testing.T) {
	const seed, events, nUsers = 20260117, 250, 12
	_, admin := migratedScratch(t)
	ledgerSeed(t, admin) // committed
	users := []string{ledgerUserA, ledgerUserB, ledgerUserC}
	for i := 1; len(users) < nUsers; i++ {
		id := ledgerUUID("e1", i)
		ledgerMust(t, admin, "INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, fmt.Sprintf("drift-%d@example.test", i))
		ledgerMust(t, admin, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, $2)", id, fmt.Sprintf("drift-%d@example.test", i))
		users = append(users, id)
	}
	admins := []string{ledgerAdminA, ledgerAdminB}

	type model struct {
		enabled, due bool
		version      int64
	}
	m := map[string]*model{}
	for _, u := range users {
		mschM3Put(t, admin, "m3_enable", u, ledgerAdminA, "now() + interval '2 seconds'")
		m[u] = &model{enabled: true, version: 1}
	}
	// wait, on the database's clock, until every date has passed (bounded); the dates are two seconds after the rows, written within a few milliseconds of each other
	deadline := time.Now().Add(15 * time.Second)
	for !ledgerBool(t, admin, "SELECT bool_and(clock_timestamp() >= enabled_until) FROM platform.m3_entitlements") {
		if time.Now().After(deadline) {
			t.Fatal("the database's clock did not pass the 2-second end dates within 15 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, u := range users {
		m[u].due = true
	}
	if d := mschDrift(t, admin); len(d) != 0 {
		t.Fatalf("the entitlements differ from what the ledger implies before any random event: %v", d)
	}

	rng := rand.New(rand.NewSource(seed))
	counts := map[string]int{}
	expireLegal := func(u string) {
		mschM3Put(t, admin, "m3_expire", u, nil, "NULL")
		m[u].enabled, m[u].due = false, false
		m[u].version++
		counts["expire"]++
	}
	for i := 0; i < events; i++ {
		var u string
		var r int
		if i < 8 {
			u = users[i] // the first eight users: their due entitlement is expired first, so the sequence always exercises a LEGAL expiry
			expireLegal(u)
			continue
		}
		u, r = users[rng.Intn(len(users))], rng.Intn(100)
		st := m[u]
		switch {
		case r < 20 && st.enabled && st.due:
			expireLegal(u)
		case r < 20:
			tx := ledgerBegin(t, admin)
			mschM3Refused(t, tx, fmt.Sprintf("event %d: an expiry the model says is not due (enabled=%v due=%v)", i, st.enabled, st.due), "23514", ledgerWhy{Constraint: "m3_expire_is_due"}, "m3_expire", u, nil, "NULL")
			_ = tx.Rollback(testCtx(t))
			counts["refused expire"]++
		case r < 60:
			until := "NULL"
			if rng.Intn(2) == 0 {
				until = "now() + interval '30 days'"
			}
			mschM3Put(t, admin, "m3_enable", u, admins[rng.Intn(2)], until)
			if !st.enabled {
				st.version++
			}
			st.enabled, st.due = true, false
			counts["enable"]++
		default:
			mschM3Put(t, admin, "m3_disable", u, admins[rng.Intn(2)], "NULL")
			if st.enabled {
				st.version++
			}
			st.enabled, st.due = false, false
			counts["disable"]++
		}
		if i%25 == 24 {
			if d := mschDrift(t, admin); len(d) != 0 {
				t.Fatalf("after event %d the entitlements differ from what the ledger implies: %v", i+1, d)
			}
		}
	}
	t.Logf("%d events after the 12 enables (seed %d): %d enables, %d disables, %d legal expiries, %d expiries refused as not due", events, seed, counts["enable"], counts["disable"], counts["expire"], counts["refused expire"])
	for kind, floor := range map[string]int{"enable": 20, "disable": 20, "expire": 8, "refused expire": 10} {
		if counts[kind] < floor {
			t.Fatalf("only %d %s events (floor %d): the random sequence did not exercise the kind", counts[kind], kind, floor)
		}
	}

	// the recompute in SQL, and the test's own model
	if d := mschDrift(t, admin); len(d) != 0 {
		t.Fatalf("after the whole sequence the entitlements differ from what the ledger implies: %v", d)
	}
	for _, u := range users {
		want := fmt.Sprintf("%v|%d|%d", !m[u].enabled, m[u].version, 0)
		if got := ledgerString(t, admin, "SELECT want_suspended::text || '|' || want_version || '|' || converged_version FROM platform.m3_sync WHERE user_id = $1", u); got != want {
			t.Fatalf("user %s: the delivery row reads %s, the test's model says %s", u, got, want)
		}
		if got := ledgerBool(t, admin, "SELECT enabled FROM platform.m3_entitlements WHERE user_id = $1", u); got != m[u].enabled {
			t.Fatalf("user %s: the entitlement is enabled=%v, the test's model says %v", u, got, m[u].enabled)
		}
	}

	// the controls: the recompute can see a difference
	t.Run("control: the recompute sees an entitlement whose trigger is off, and one edited behind the ledger's back", func(t *testing.T) {
		var disabled string
		for _, u := range users {
			if !m[u].enabled {
				disabled = u
				break
			}
		}
		if disabled == "" {
			t.Fatal("no user is disabled at the end of the sequence: the control needs one")
		}
		tx := ledgerBegin(t, admin)
		ledgerMust(t, tx, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_entitlements")
		mschM3Put(t, tx, "m3_enable", disabled, ledgerAdminA, "NULL")
		if d := mschDrift(t, tx); len(d) < 2 {
			t.Fatalf("with the entitlement trigger off, an m3_enable the entitlement never saw gives the difference %v, want the two sides of the entitlement row (and the delivery row)", d)
		}
		_ = tx.Rollback(testCtx(t))
		tx = ledgerBegin(t, admin)
		ledgerMust(t, tx, "UPDATE platform.m3_sync SET want_version = want_version + 1 WHERE user_id = $1", users[0])
		if d := mschDrift(t, tx); len(d) != 2 {
			t.Fatalf("a delivery row edited behind the ledger's back gives the difference %v, want the two sides of one row", d)
		}
		_ = tx.Rollback(testCtx(t))
		if d := mschDrift(t, admin); len(d) != 0 {
			t.Fatalf("the controls leaked out of their transactions: %v", d)
		}
	})
}

// TestM3Schema_SyncConvergenceRule proves the rule that stops a false success on the delivery table: converged_version may equal want_version ONLY while `observed` - what module 3 itself answered - equals
// the wanted state (suspended when want_suspended, active when not). With the entitlement wanted ACTIVE, converged = want is refused while observed is 'unknown' and while it is 'suspended' (the opposite
// state) and accepted while it is 'active'; a refusal also stops observed from being moved away from the wanted state while the row claims convergence. Disabling the entitlement raises want_version above
// converged_version again (the earlier convergence does not carry over), and with the wanted state SUSPENDED the same rule holds the other way round. converged_version beyond want_version is refused
// (m3_sync_converged_not_ahead); `observed` outside {unknown, suspended, active} is refused (m3_sync_observed_vocabulary, with the other rules not in play), NULL by NOT NULL; the version and attempt
// counters cannot go negative. Every refusal is named and has its legal update as the control. It fails if "converged" can be recorded for a state module 3 was not seen to be in.
func TestM3Schema_SyncConvergenceRule(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b := ledgerUserA, ledgerUserB
	const only, ahead = "m3_sync_converged_only_when_observed", "m3_sync_converged_not_ahead"
	set := func(user, sets string) (string, []any) {
		return "UPDATE platform.m3_sync SET " + sets + " WHERE user_id = $1", []any{user}
	}
	refused := func(t *testing.T, what, constraint, user, sets string) {
		t.Helper()
		sql, args := set(user, sets)
		ledgerExpectRefused(t, tx, what, "23514", ledgerWhy{Constraint: constraint}, sql, args...)
	}
	ok := func(t *testing.T, what, user, sets string) {
		t.Helper()
		sql, args := set(user, sets)
		if tag := ledgerExpectOK(t, tx, what, sql, args...); tag.RowsAffected() != 1 {
			t.Fatalf("%s: touched %d rows, want 1", what, tag.RowsAffected())
		}
	}

	mschM3Put(t, tx, "m3_enable", a, ledgerAdminA, "NULL")
	if got := mschSync(t, tx, a); got != "false|1|0|unknown" {
		t.Fatalf("the starting delivery row reads %q, want false|1|0|unknown", got)
	}

	t.Run("wanted ACTIVE (want_version 1): converged only while module 3 was seen active", func(t *testing.T) {
		refused(t, "converged = want while observed is 'unknown'", only, a, "converged_version = 1")
		refused(t, "converged = want while observed is 'suspended' (the opposite of the wanted state)", only, a, "converged_version = 1, observed = 'suspended'")
		refused(t, "converged = want while observed is 'unknown', set explicitly", only, a, "converged_version = 1, observed = 'unknown'")
		ok(t, "control: observed 'suspended' alone (not converged: converged 0 < want 1)", a, "observed = 'suspended'")
		ok(t, "control: converged = want while observed is 'active'", a, "converged_version = 1, observed = 'active'")
		if got := mschSync(t, tx, a); got != "false|1|1|active" {
			t.Fatalf("the converged row reads %q, want false|1|1|active", got)
		}
		refused(t, "moving observed away from the wanted state while the row still claims convergence", only, a, "observed = 'unknown'")
		ok(t, "control: dropping the claim and the observation together", a, "converged_version = 0, observed = 'unknown'")
		ok(t, "control: re-converging while active", a, "converged_version = 1, observed = 'active'")
	})

	t.Run("a later entitlement change makes want_version exceed converged_version again", func(t *testing.T) {
		mschM3Put(t, tx, "m3_disable", a, ledgerAdminA, "NULL")
		if got := mschSync(t, tx, a); got != "true|2|1|active" {
			t.Fatalf("after the disable the delivery row reads %q, want true|2|1|active (want_version 2 > converged_version 1, observed is stale)", got)
		}
		if !ledgerBool(t, tx, "SELECT want_version > converged_version FROM platform.m3_sync WHERE user_id = $1", a) {
			t.Fatal("want_version does not exceed converged_version after the entitlement changed: module 3 would never be told")
		}
	})

	t.Run("wanted SUSPENDED (want_version 2): converged only while module 3 was seen suspended", func(t *testing.T) {
		refused(t, "converged = want while observed is still 'active' (the opposite)", only, a, "converged_version = 2")
		refused(t, "converged = want while observed is 'unknown'", only, a, "converged_version = 2, observed = 'unknown'")
		ok(t, "control: converged = want while observed is 'suspended'", a, "converged_version = 2, observed = 'suspended'")
		if got := mschSync(t, tx, a); got != "true|2|2|suspended" {
			t.Fatalf("the converged row reads %q, want true|2|2|suspended", got)
		}
	})

	t.Run("converged_version beyond want_version", func(t *testing.T) {
		// observed matches the wanted state, so the convergence rule is satisfied and only the ahead rule can speak
		refused(t, "converged 3 against want 2", ahead, a, "converged_version = 3")
		ok(t, "control: converged = want", a, "converged_version = 2")
	})

	t.Run("the vocabulary and the counters (user B, converged below want, so the convergence rule is not in play)", func(t *testing.T) {
		mschM3Put(t, tx, "m3_enable", b, ledgerAdminA, "NULL")
		for _, bad := range []string{"paused", "", "ACTIVE", "suspended "} {
			refused(t, fmt.Sprintf("observed = %q", bad), "m3_sync_observed_vocabulary", b, "observed = '"+bad+"'")
		}
		sql, args := set(b, "observed = NULL")
		ledgerExpectRefused(t, tx, "observed = NULL", "23502", ledgerWhy{Column: "observed"}, sql, args...)
		refused(t, "a negative attempt count", "m3_sync_attempts_not_negative", b, "attempts = -1")
		// want_version 0 needs converged_version 0 beside it (not ahead), and converged = want needs observed to match the wanted state, so that every other rule is quiet
		refused(t, "want_version 0 (with converged 0 and observed 'active', so only the counter rule is in play)", "m3_sync_versions_are_positive", b, "want_version = 0, observed = 'active'")
		refused(t, "converged_version -1", "m3_sync_versions_are_positive", b, "converged_version = -1")
		ok(t, "control: every legal value of observed, in turn", b, "observed = 'suspended'")
		ok(t, "control: observed 'active'", b, "observed = 'active'")
		ok(t, "control: observed back to 'unknown'", b, "observed = 'unknown'")
		ok(t, "control: attempts = 3", b, "attempts = 3")
	})
}

// TestM3Schema_RolesAndPrivileges proves who may write what since platform_0006. platform_app: updates EXACTLY the seven columns of platform.m3_sync the reconcile loop records (observed, observed_at,
// attempts, last_attempt_at, last_error, converged_version, updated_at) - each alone, and all together - and is refused (42501) any statement that touches want_suspended, want_version or user_id, alone or
// beside an allowed column (all or nothing), and INSERT, DELETE and TRUNCATE; it reads platform.m3_entitlements and is refused INSERT, UPDATE, DELETE and TRUNCATE there. platform_admin only reads both
// tables (it is refused the seven columns too). The paired control of every refusal is the superuser running the same statement successfully on a real row; and the entitlement and delivery row appear
// when either role writes the LEDGER (m3_enable), written by neither role. It fails if a role regains a write it lost or loses one it needs.
func TestM3Schema_RolesAndPrivileges(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b := ledgerUserA, ledgerUserB
	mschM3Put(t, tx, "m3_enable", a, ledgerAdminA, "NULL")
	mschM3Put(t, tx, "m3_enable", b, ledgerAdminA, "NULL")

	listed := []struct{ col, set string }{
		{"observed", "observed = 'active'"},
		{"observed_at", "observed_at = now()"},
		{"attempts", "attempts = attempts + 1"},
		{"last_attempt_at", "last_attempt_at = now()"},
		{"last_error", "last_error = 'connection refused'"},
		{"converged_version", "converged_version = converged_version"},
		{"updated_at", "updated_at = now()"},
	}
	forbidden := []struct{ col, set string }{
		{"want_suspended", "want_suspended = NOT want_suspended"},
		{"want_version", "want_version = want_version + 1"},
		{"user_id", "user_id = user_id"},
		{"an allowed column AND want_version", "observed = 'active', want_version = want_version + 1"},
	}
	update := func(sets string) (string, []any) {
		return "UPDATE platform.m3_sync SET " + sets + " WHERE user_id = $1", []any{a}
	}

	t.Run("the controls: the superuser runs every statement below", func(t *testing.T) {
		for _, f := range append(append([]struct{ col, set string }{}, listed...), forbidden...) {
			f := f
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				sql, args := update(f.set)
				if tag := ledgerExpectOK(t, sp, "control: the superuser updating "+f.col, sql, args...); tag.RowsAffected() != 1 {
					t.Fatalf("control: updating %s touched %d rows, want 1", f.col, tag.RowsAffected())
				}
			})
		}
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "DELETE FROM platform.m3_sync WHERE user_id = $1", a)
			ledgerExpectOK(t, sp, "control: the superuser inserting a delivery row", "INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES ($1, true)", a)
		})
	})

	t.Run("platform_app updates exactly the seven columns, and nothing else", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		why := ledgerWhy{Message: "permission denied for table m3_sync"}
		for _, l := range listed {
			sql, args := update(l.set)
			if tag := ledgerExpectOK(t, tx, "platform_app updating "+l.col, sql, args...); tag.RowsAffected() != 1 {
				t.Fatalf("platform_app updating %s touched %d rows, want 1", l.col, tag.RowsAffected())
			}
		}
		all := make([]string, len(listed))
		for i, l := range listed {
			all[i] = l.set
		}
		sql, args := update(strings.Join(all, ", "))
		ledgerExpectOK(t, tx, "platform_app updating all seven at once", sql, args...)
		for _, f := range forbidden {
			sql, args := update(f.set)
			ledgerExpectRefused(t, tx, "platform_app updating "+f.col, "42501", why, sql, args...)
		}
		ledgerExpectRefused(t, tx, "platform_app INSERT on m3_sync", "42501", why, "INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES ($1, true)", a)
		ledgerExpectRefused(t, tx, "platform_app DELETE on m3_sync", "42501", why, "DELETE FROM platform.m3_sync WHERE user_id = $1", a)
		ledgerExpectRefused(t, tx, "platform_app TRUNCATE on m3_sync", "42501", why, "TRUNCATE platform.m3_sync")
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_sync"); n != 2 {
			t.Fatalf("platform_app reads %d delivery rows, want 2 (SELECT is granted)", n)
		}
		ledgerResetRole(t, tx)
	})

	t.Run("platform_admin reads the delivery table and writes none of it", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_admin")
		why := ledgerWhy{Message: "permission denied for table m3_sync"}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_sync"); n != 2 {
			t.Fatalf("platform_admin reads %d delivery rows, want 2 (SELECT is granted)", n)
		}
		for _, l := range append(append([]struct{ col, set string }{}, listed...), forbidden[:2]...) {
			sql, args := update(l.set)
			ledgerExpectRefused(t, tx, "platform_admin updating "+l.col, "42501", why, sql, args...)
		}
		ledgerExpectRefused(t, tx, "platform_admin INSERT on m3_sync", "42501", why, "INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES ($1, true)", a)
		ledgerExpectRefused(t, tx, "platform_admin DELETE on m3_sync", "42501", why, "DELETE FROM platform.m3_sync")
		ledgerExpectRefused(t, tx, "platform_admin TRUNCATE on m3_sync", "42501", why, "TRUNCATE platform.m3_sync")
		ledgerResetRole(t, tx)
	})

	t.Run("the entitlements: both roles read them and neither writes them", func(t *testing.T) {
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			why := ledgerWhy{Message: "permission denied for table m3_entitlements"}
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.m3_entitlements"); n != 2 {
				t.Fatalf("%s reads %d entitlements, want 2 (SELECT is granted)", role, n)
			}
			ledgerExpectRefused(t, tx, role+" INSERT on m3_entitlements", "42501", why, "INSERT INTO platform.m3_entitlements (user_id, enabled) VALUES ($1, true)", ledgerUserC)
			ledgerExpectRefused(t, tx, role+" UPDATE on m3_entitlements", "42501", why, "UPDATE platform.m3_entitlements SET enabled = false WHERE user_id = $1", a)
			ledgerExpectRefused(t, tx, role+" DELETE on m3_entitlements", "42501", why, "DELETE FROM platform.m3_entitlements WHERE user_id = $1", a)
			ledgerExpectRefused(t, tx, role+" TRUNCATE on m3_entitlements", "42501", why, "TRUNCATE platform.m3_entitlements CASCADE")
			ledgerResetRole(t, tx)
		}
		// the superuser's control for the entitlement statements is TestConstraints_M3Entitlements; what the roles CAN do is write the LEDGER, and the tables follow through the trigger
		for _, w := range []struct{ role, user string }{{"platform_app", ledgerUserC}, {"platform_admin", ledgerAdminB}} {
			ledgerSwitchRole(t, tx, w.role)
			ledgerOKRow(t, tx, "control: "+w.role+" enables an entitlement through the ledger", ledgerM3Cols("m3_enable", w.user))
			ledgerResetRole(t, tx)
			if got := mschEntitlement(t, tx, w.user); got != "true|"+ledgerAdminA+"|false" {
				t.Fatalf("the entitlement %s's ledger row created reads %q, want true|%s|false", w.role, got, ledgerAdminA)
			}
			if got := mschSync(t, tx, w.user); got != "false|1|0|unknown" {
				t.Fatalf("the delivery row %s's ledger row created reads %q, want false|1|0|unknown", w.role, got)
			}
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// platform_0005: the ledger's clock is the database's (ledger_stamp_created_at), and the two silent skips that are now refusals
// ---------------------------------------------------------------------------------------------------------------------

// TestMoneySchema_CreatedAtIsTheDatabasesClock proves ledger_stamp_created_at: a ledger INSERT that supplies created_at - in the past, in the far future, or NULL - ends with created_at = the
// transaction's now(), for every kind a writer can reach (grant, adjustment, reserve, charge, release, m3_enable, m3_disable). The paired control of each is the same row with the stamp switched off in a
// savepoint that is rolled back: the supplied value is KEPT (and a NULL is refused by NOT NULL), so the trigger is what does it. It matters because two rules lean on created_at: a future created_at
// on an m3_enable would refuse a legal end date, and a past created_at with a past end date would let an already-expired entitlement in; both are decided by now() with the stamp on and by the
// writer's value with it off. It fails if any writer can still choose the ledger's time.
func TestMoneySchema_CreatedAtIsTheDatabasesClock(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, c := ledgerUserA, ledgerUserC
	const past, future = "2000-01-01T00:00:00Z", "2999-01-01T00:00:00Z"
	const stamp = "ledger_stamp_created_at"
	r := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2)) // 20 credits for the charge and the release below

	t.Run("every kind, with a past and a future created_at supplied", func(t *testing.T) {
		kinds := []struct {
			name  string
			legal func() ledgerCols
		}{
			{"grant", func() ledgerCols { return ledgerGrantCols(a, "m1", 3) }},
			{"adjustment", func() ledgerCols { return ledgerAdjustmentCols(a, "m1", 1) }},
			{"reserve", func() ledgerCols { return ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 1, 2) }},
			{"charge", func() ledgerCols { return ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1) }},
			{"release", func() ledgerCols { return ledgerReleaseCols(a, "m1", ledgerJobA1, r, 20) }},
			{"m3_enable", func() ledgerCols { return ledgerM3Cols("m3_enable", c) }},
			{"m3_disable", func() ledgerCols { return ledgerM3Cols("m3_disable", c) }},
		}
		for _, k := range kinds {
			for _, supplied := range []string{past, future} {
				k, supplied := k, supplied
				t.Run(k.name+" supplying created_at = "+supplied[:4], func(t *testing.T) {
					ledgerTrial(t, tx, func(sp pgx.Tx) {
						id := ledgerPut(t, sp, k.legal().with("created_at", supplied))
						if !ledgerBool(t, sp, "SELECT created_at = now() FROM platform.ledger WHERE id = $1", id) {
							t.Fatalf("a %s row that supplied created_at = %s was stored with created_at = %s, want the transaction's now() %s", k.name, supplied,
								ledgerString(t, sp, "SELECT created_at::text FROM platform.ledger WHERE id = $1", id), ledgerString(t, sp, "SELECT now()::text"))
						}
					})
					// the control: with the stamp off, the writer's value is kept
					ledgerTrial(t, tx, func(sp pgx.Tx) {
						ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+stamp)
						id := ledgerPut(t, sp, k.legal().with("created_at", supplied))
						if !ledgerBool(t, sp, "SELECT created_at = $2::timestamptz FROM platform.ledger WHERE id = $1", id, supplied) {
							t.Fatalf("control: with the stamp off, a %s row that supplied created_at = %s did not keep it: the assertion above would prove nothing", k.name, supplied)
						}
					})
				})
			}
		}
	})

	t.Run("an explicit NULL is stamped too (the BEFORE trigger runs before NOT NULL)", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			id := ledgerPut(t, sp, ledgerGrantCols(a, "m1", 3).with("created_at", nil))
			if !ledgerBool(t, sp, "SELECT created_at = now() FROM platform.ledger WHERE id = $1", id) {
				t.Fatal("a grant that supplied created_at = NULL was not stamped with now()")
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+stamp)
			ledgerRefusedRow(t, sp, "control: the same grant with the stamp off", "23502", ledgerWhy{Column: "created_at"}, ledgerGrantCols(a, "m1", 3).with("created_at", nil))
		})
	})

	t.Run("the two rules that lean on created_at now rest on now()", func(t *testing.T) {
		// a FUTURE created_at beyond the end date would refuse a legal enable if the writer's value counted; stamped, the date (2999) is after now() and the row passes
		futureStamp := ledgerM3Cols("m3_enable", c).with("created_at", "3000-01-01T00:00:00Z", "until_at", future)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "an m3_enable with created_at = 3000 and an end date of 2999 (stamped to now(), so the end date is in the future)", futureStamp)
			if got := ledgerBool(t, sp, "SELECT enabled_until = $2::timestamptz FROM platform.m3_entitlements WHERE user_id = $1", c, future); !got {
				t.Fatal("the entitlement written by the stamped m3_enable does not carry the end date")
			}
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+stamp)
			ledgerRefusedCheck(t, sp, "control: the same row with the stamp off is refused by the writer's own created_at", "ledger_until_at_shape", futureStamp)
		})
		// a PAST created_at with a past end date would let an already-expired entitlement in; stamped, the end date is before now() and the row is refused
		backdated := ledgerM3Cols("m3_enable", c).with("created_at", past, "until_at", "2001-01-01T00:00:00Z")
		ledgerRefusedCheck(t, tx, "an m3_enable with created_at = 2000 and an end date of 2001 (stamped to now(), so the end date is in the past)", "ledger_until_at_shape", backdated)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+stamp)
			ledgerOKRow(t, sp, "control: the same row with the stamp off passes on the writer's own created_at", backdated)
		})
	})
}

// TestM3Schema_ExpiryCannotBeBroughtForward proves the early-expiry abuse is closed. An entitlement is enabled with an end date 30 days ahead; an m3_expire row that supplies created_at = enabled_until + 1
// day (so that "enabled_until <= created_at" would hold if the writer's clock counted) is refused with 23514 m3_expire_is_due, and the entitlement and its delivery row are untouched. The paired
// controls: with the created_at stamp switched off (a rolled-back savepoint) the SAME row is accepted and the entitlement is expired at once - the abuse was real, and the stamp is what closes it - and
// the legitimate expiry of a due entitlement (the date moved into the past as the passing of time does) passes without supplying any time. It fails if a writer can expire an entitlement before the
// database's clock says it is due.
func TestM3Schema_ExpiryCannotBeBroughtForward(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	u, adminA := ledgerUserA, ledgerAdminA
	mschM3Put(t, tx, "m3_enable", u, adminA, "now() + interval '30 days'")
	var after time.Time // one day after the end date, taken from the database
	if err := tx.QueryRow(testCtx(t), "SELECT enabled_until + interval '1 day' FROM platform.m3_entitlements WHERE user_id = $1", u).Scan(&after); err != nil {
		t.Fatal(err)
	}
	abuse := ledgerM3Cols("m3_expire", u).with("created_at", after)

	ledgerRefusedCheck(t, tx, "an m3_expire that supplies created_at = enabled_until + 1 day", "m3_expire_is_due", abuse)
	if got := mschEntitlement(t, tx, u); got != "true|"+adminA+"|true" {
		t.Fatalf("the refused expiry left the entitlement %q, want true|%s|true", got, adminA)
	}
	if got := mschSync(t, tx, u); got != "false|1|0|unknown" {
		t.Fatalf("the refused expiry left the delivery row %q, want false|1|0|unknown", got)
	}

	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_stamp_created_at")
		ledgerOKRow(t, sp, "control: with the stamp off the same row expires the entitlement a month early", abuse)
		if got := mschEntitlement(t, sp, u); got != "false||false" {
			t.Fatalf("control: with the stamp off the entitlement reads %q, want false||false (expired early)", got)
		}
	})

	ledgerMakeDue(t, tx, u)
	mschM3Put(t, tx, "m3_expire", u, nil, "NULL")
	if got := mschEntitlement(t, tx, u); got != "false||false" {
		t.Fatalf("the legitimate expiry left the entitlement %q, want false||false", got)
	}
	if got := mschSync(t, tx, u); got != "true|2|0|unknown" {
		t.Fatalf("the legitimate expiry left the delivery row %q, want true|2|0|unknown", got)
	}
}

// TestMoneySchema_ReserveRowMustExist proves reserve_row_exists: a charge or a release whose reserve has no platform.reserves row (a reserve written before platform_0005, or one whose row was lost) is
// refused with 23514, never silently skipped - a skip would leave that reserve uncapped. The row is removed as the superuser inside a savepoint that is rolled back. For the charge the first wall is the
// writer itself; for the release the remainder trigger answers first (it finds no reserve), so the writer's refusal is shown alone with that trigger off. The paired controls are the same charge and
// the same release passing while the row exists, and a new reserve still writing its row. It fails if a row can go missing and the ledger goes on.
func TestMoneySchema_ReserveRowMustExist(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	const missing = "reserve_row_exists"
	r := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
	ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 50, 2)) // another job's 100 credits: the pooled balance is never what refuses the charge below
	for i := 0; i < 3; i++ {
		ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1))
	}
	remainder := ledgerReserveRemainder(t, tx, r)
	rowGone := func(t *testing.T, sp pgx.Tx) {
		t.Helper()
		if tag := ledgerMust(t, sp, "DELETE FROM platform.reserves WHERE reserve_id = $1", r); tag.RowsAffected() != 1 {
			t.Fatalf("deleting the reserves row touched %d rows, want 1: the refusals below would prove nothing", tag.RowsAffected())
		}
		if n := ledgerInt64(t, sp, "SELECT count(*) FROM platform.reserves WHERE reserve_id = $1", r); n != 0 {
			t.Fatalf("the reserves row of reserve %d is still there", r)
		}
	}
	charge := ledgerChargeCols(a, "m1", ledgerJobA1, r, "m1_own", 1, 1)
	release := ledgerReleaseCols(a, "m1", ledgerJobA1, r, remainder)

	t.Run("a charge against a reserve with no row", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) { ledgerOKRow(t, sp, "control: the same charge while the row exists", charge) })
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			rowGone(t, sp)
			ledgerRefusedRow(t, sp, "the charge once the reserves row is gone", "23514", ledgerWhy{Constraint: missing}, charge)
		})
	})

	t.Run("a release of a reserve with no row", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "control: the same release (exactly the remainder) while the row exists", release)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			rowGone(t, sp)
			ledgerRefusedRow(t, sp, "the release once the row is gone: the remainder trigger finds no reserve first", "23514", ledgerWhy{Constraint: "release_is_the_remainder"}, release)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			rowGone(t, sp)
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_check_release")
			ledgerRefusedRow(t, sp, "the release once the row is gone, with the remainder trigger off: the writer's own refusal", "23514", ledgerWhy{Constraint: missing}, release)
		})
	})

	t.Run("a new reserve still writes its row, and the other reserves are untouched", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			rowGone(t, sp)
			job := ledgerNewJob(t, sp, ledgerUUID("d9", 1), a, "m1", true)
			fresh := ledgerPut(t, sp, ledgerReserveCols(a, "m1", job, "m1_vendor", 2, 2))
			if got := ledgerString(t, sp, "SELECT reserved || '|' || charged || '|' || released FROM platform.reserves WHERE reserve_id = $1", fresh); got != "4|0|0" {
				t.Fatalf("the row of a new reserve reads %s, want 4|0|0", got)
			}
			ledgerPut(t, sp, ledgerChargeCols(a, "m1", job, fresh, "m1_own", 1, 1))
		})
		ledgerRequireViewsEmpty(t, tx, "after the missing-row cases")
	})
}

// TestM3Schema_SyncRowIsRecreated proves the upsert in m3_entitlements_raise_sync: an entitlement whose delivery row is missing (one that predates platform_0006, or whose row was lost) gets a row from
// its NEXT change of state, with want_suspended the new wanted state and want_version 2, instead of the change being skipped in silence; a further change then raises it to 3 as usual. The row is removed
// as the superuser in a savepoint that is rolled back. The same holds when the change is the automatic expiry. The paired control is the trigger itself: with it switched off, the same flip leaves no
// delivery row, so the row is the trigger's doing. It fails if a missing delivery row can stay missing through a change of state.
func TestM3Schema_SyncRowIsRecreated(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, adminA := ledgerUserA, ledgerAdminA
	mschM3Put(t, tx, "m3_enable", a, adminA, "NULL")
	if got := mschSync(t, tx, a); got != "false|1|0|unknown" {
		t.Fatalf("the starting delivery row reads %q, want false|1|0|unknown", got)
	}
	rowGone := func(t *testing.T, sp pgx.Tx) {
		t.Helper()
		if tag := ledgerMust(t, sp, "DELETE FROM platform.m3_sync WHERE user_id = $1", a); tag.RowsAffected() != 1 {
			t.Fatalf("deleting the delivery row touched %d rows, want 1", tag.RowsAffected())
		}
		if got := mschSync(t, sp, a); got != "none" {
			t.Fatalf("the delivery row is still there: %q", got)
		}
	}

	t.Run("a disable recreates the row at want_version 2, and the next change raises it to 3", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			rowGone(t, sp)
			mschM3Put(t, sp, "m3_disable", a, adminA, "NULL")
			if got := mschSync(t, sp, a); got != "true|2|0|unknown" {
				t.Fatalf("after the disable the recreated delivery row reads %q, want true|2|0|unknown (suspend wanted, version 2, nothing converged or observed)", got)
			}
			mschM3Put(t, sp, "m3_enable", a, adminA, "NULL")
			if got := mschSync(t, sp, a); got != "false|3|0|unknown" {
				t.Fatalf("after the re-enable the delivery row reads %q, want false|3|0|unknown", got)
			}
		})
		// the control: the trigger is what writes the row
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.m3_entitlements DISABLE TRIGGER m3_entitlements_raise_sync")
			rowGone(t, sp)
			mschM3Put(t, sp, "m3_disable", a, adminA, "NULL")
			if got := mschEntitlement(t, sp, a); got != "false|"+adminA+"|false" {
				t.Fatalf("control: the entitlement reads %q after the disable, want it flipped", got)
			}
			if got := mschSync(t, sp, a); got != "none" {
				t.Fatalf("control: with the trigger off the flip left the delivery row %q, want none", got)
			}
		})
	})

	t.Run("the automatic expiry is a change of state too", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMakeDue(t, sp, a)
			rowGone(t, sp)
			mschM3Put(t, sp, "m3_expire", a, nil, "NULL")
			if got := mschSync(t, sp, a); got != "true|2|0|unknown" {
				t.Fatalf("after the expiry the recreated delivery row reads %q, want true|2|0|unknown", got)
			}
		})
		ledgerRequireViewsEmpty(t, tx, "after the delivery-row cases")
	})
}
