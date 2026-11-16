//go:build harness

package harness

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The ledger tests. Every test runs on a scratch database the real migrator has migrated (migratedScratch), inside a
// transaction that is rolled back (the append-only tests commit their own seed rows, because "the ledger is intact afterwards"
// has to compare real rows). The platform_0002 migration is the specification; each test says how it can fail.
//
// Every refusal asserts the SQLSTATE AND the constraint (or message) it is refused for, and every refusal has a paired
// control: the legal row of the same kind passes, so a refusal cannot be a typo, a missing fixture or a different rule firing.

// ledgerSeedFunded is the starting point of the shape and foreign-key tests: the fixtures plus enough credits that no balance
// check can be what refuses a row.
func ledgerSeedFunded(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ledgerSeed(t, tx)
	ledgerFund(t, tx, ledgerUserA, "m1", 1000)
	ledgerFund(t, tx, ledgerUserA, "m2", 1000)
	ledgerFund(t, tx, ledgerUserB, "m1", 1000)
}

// TestLedger_Lifecycle walks one job's money from grant to release and reads the balance after every step, then does the
// same on module 2. It fails if any delta, the frozen reserve rate, a charge's frozen price or the balance trigger is wrong:
// the balances after the steps are all different, so a trigger that does nothing, or does the wrong arithmetic, fails at the
// first step. The platform_0002 trigger is the only thing that writes a balance, so these numbers come from nowhere else.
func TestLedger_Lifecycle(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA

	step := func(t *testing.T, what, module string, available, reserved int64) {
		t.Helper()
		ledgerRequireBalance(t, tx, what, a, module, available, reserved)
		ledgerRequireViewsEmpty(t, tx, what)
	}

	t.Run("m1 grant, reserve at the frozen rate, charge at frozen prices, release", func(t *testing.T) {
		ledgerPut(t, tx, ledgerGrantCols(a, "m1", 100))
		step(t, "after the grant of 100", "m1", 100, 0)

		reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
		step(t, "after reserving 10 units at the vendor rate 2", "m1", 80, 20)
		if got := ledgerRows(t, tx, "SELECT unit, units, unit_cost FROM platform.ledger WHERE id = $1", reserve); len(got) != 1 || got[0] != "m1_vendor|10|2" {
			t.Fatalf("the reserve row must carry the frozen rate (unit, units, unit_cost) = m1_vendor|10|2, got %v", got)
		}

		// a charge is exactly ONE unit of one job item (platform_0005): three m1_own units are three rows of 1 credit, two m1_vendor units are two rows of 2
		var own, vendor []int64
		for i := 0; i < 3; i++ {
			own = append(own, ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1)))
		}
		step(t, "after charging 3 units at the frozen price 1 (m1_own), one row each", "m1", 80, 17)
		for i := 0; i < 2; i++ {
			vendor = append(vendor, ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_vendor", 1, 2)))
		}
		step(t, "after charging 2 units at the frozen price 2 (m1_vendor), one row each", "m1", 80, 13)
		for _, id := range own {
			if got := ledgerRows(t, tx, "SELECT unit, units, unit_cost, reserved_delta FROM platform.ledger WHERE id = $1", id); len(got) != 1 || got[0] != "m1_own|1|1|-1" {
				t.Fatalf("charge row %d must carry the unit, the frozen price and the consumed credits m1_own|1|1|-1, got %v", id, got)
			}
		}
		for _, id := range vendor {
			if got := ledgerRows(t, tx, "SELECT unit, units, unit_cost, reserved_delta FROM platform.ledger WHERE id = $1", id); len(got) != 1 || got[0] != "m1_vendor|1|2|-2" {
				t.Fatalf("charge row %d must carry the unit, the frozen price and the consumed credits m1_vendor|1|2|-2, got %v", id, got)
			}
		}
		// platform.reserves (written only by the ledger trigger) says the same: 20 held, 3 + 4 charged, nothing released yet
		if got := ledgerRows(t, tx, "SELECT reserved, charged, released FROM platform.reserves WHERE reserve_id = $1", reserve); len(got) != 1 || got[0] != "20|7|0" {
			t.Fatalf("platform.reserves for the reserve reads %v after the charges, want [20|7|0] (reserved|charged|released)", got)
		}

		// a release returns exactly the remainder (platform_0005): 20 - 3 - 4 = 13
		if rem := ledgerReserveRemainder(t, tx, reserve); rem != 13 {
			t.Fatalf("the reserve has %d credits left, want 13", rem)
		}
		ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, 13))
		step(t, "after releasing the remaining 13", "m1", 93, 0)
		if got := ledgerRows(t, tx, "SELECT reserved, charged, released FROM platform.reserves WHERE reserve_id = $1", reserve); len(got) != 1 || got[0] != "20|7|13" {
			t.Fatalf("platform.reserves for the reserve reads %v after the release, want [20|7|13]", got)
		}

		// Settlement adds up: what was charged and released is exactly what was reserved.
		settled := ledgerInt64(t, tx, `SELECT COALESCE(sum(CASE WHEN kind = 'charge' THEN -reserved_delta ELSE available_delta END), 0)::bigint
			FROM platform.ledger WHERE reserve_id = $1`, reserve)
		if settled != 20 {
			t.Fatalf("charges plus releases of the reserve add up to %d, want exactly the 20 that were reserved", settled)
		}

		// Control: both walls are wired. The reserve is fully settled, so one more charge would take the pooled reserved column below zero AND would take the reserve's own charged + released
		// above what it held. Two walls refuse it; each is proven ALONE by switching the other's trigger off in a savepoint, because which of the two answers first when both are on is the order
		// of the trigger names (not a rule worth pinning).
		extra := ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_reserves")
			ledgerRefusedRow(t, sp, "a charge on a fully settled reserve, per-reserve wall off: the per-balance wall", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative"}, extra)
		})
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_balance")
			ledgerRefusedRow(t, sp, "a charge on a fully settled reserve, per-balance wall off: the per-reserve wall", "23514", ledgerWhy{Constraint: "reserves_not_overspent", Table: "reserves"}, extra)
		})
		step(t, "after the refused extra charge (nothing may have changed)", "m1", 93, 0)
	})

	t.Run("credits never move between modules: m1 is untouched and has its own balance row on m2 only once m2 is funded", func(t *testing.T) {
		if b := ledgerBalance(t, tx, a, "m2"); b.Exists {
			t.Fatalf("user A has a balance row on m2 (%+v) although no m2 credit was ever granted", b)
		}
		ledgerPut(t, tx, ledgerGrantCols(a, "m2", 50))
		step(t, "after the m2 grant of 50", "m2", 50, 0)
		reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m2", ledgerJobA2, "m2_accepted", 5, 1))
		step(t, "after reserving 5 m2 units", "m2", 45, 5)
		for i := 0; i < 4; i++ { // one row per job item (platform_0005)
			ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, reserve, "m2_accepted", 1, 1))
		}
		step(t, "after charging 4 accepted sends", "m2", 45, 1)
		ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, reserve, "m2_bounced", 1, 1))
		step(t, "after charging 1 bounce", "m2", 45, 0)
		ledgerRequireBalance(t, tx, "m1 after all the m2 activity", a, "m1", 93, 0)
		if b := ledgerBalance(t, tx, ledgerUserB, "m1"); b.Exists {
			t.Fatalf("user B has a balance %+v although nothing was ever written for B", b)
		}
	})
}

// TestLedger_KindShapes checks the per-kind CHECK constraints. For each kind the legal row is the control and every refused
// row differs from it in exactly one thing, so the constraint named in the assertion is the only rule that can be firing. It
// fails if a CHECK is missing, weakened (a NULL slips through: the NULL trap), or renamed.
func TestLedger_KindShapes(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA

	type shapeCase struct {
		name string
		cols ledgerCols
	}
	// refuse proves each case is refused by the named CHECK. triggersOff names ledger triggers switched off (in a savepoint that is rolled back) around each case: since platform_0005 a BEFORE
	// trigger runs before the CHECK constraints, so for a malformed reserve or release it answers first (23514 too, with its own constraint name). Proving the shape CHECK ALONE therefore
	// takes the trigger away, exactly as the Phase 1 assertion read; what the trigger says about the same rows is asserted separately, with the trigger on.
	refuse := func(t *testing.T, constraint string, cases []shapeCase, triggersOff ...string) {
		t.Helper()
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				if len(triggersOff) == 0 {
					ledgerRefusedCheck(t, tx, c.name, constraint, c.cols)
					return
				}
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					for _, trg := range triggersOff {
						ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+trg)
					}
					ledgerRefusedCheck(t, sp, c.name, constraint, c.cols)
				})
			})
		}
	}

	t.Run("grant", func(t *testing.T) {
		g := func() ledgerCols { return ledgerGrantCols(a, "m1", 10) }
		ledgerOKRow(t, tx, "control: the legal grant", g())
		refuse(t, "ledger_shape_grant", []shapeCase{
			{"zero amount", g().with("available_delta", int64(0))},
			{"a negative amount (a clawback is a new kind, never a negative grant)", g().with("available_delta", int64(-5))},
			{"moves reserved credits", g().with("reserved_delta", int64(1))},
			{"names a job", g().with("job_id", ledgerJobA1)},
			{"names a reserve", g().with("reserve_id", int64(1))},
			{"carries a unit", g().with("unit", "m1_own")},
			{"carries units", g().with("units", int64(1))},
			{"carries a unit cost", g().with("unit_cost", int64(1))},
			{"no acting admin", g().with("acting_admin_id", nil)},
			{"no reason (NULL)", g().with("reason", nil)},
			{"an empty reason", g().with("reason", "")},
			{"a blank reason", g().with("reason", "   ")},
		})
		ledgerRefusedFK(t, tx, "the acting admin must be an admin, not just a user", "ledger_acting_admin_id_fkey", g().with("acting_admin_id", ledgerUserB))
		ledgerRefusedFK(t, tx, "the user must exist", "ledger_user_id_fkey", g().with("user_id", ledgerNoSuchID))
	})

	t.Run("reserve", func(t *testing.T) {
		// job A1b: a job of its own for this subtest (one reserve per job, platform_0005), at the dearest price of its complete snapshot (m1_vendor, 2)
		r := func() ledgerCols { return ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 10, 2) }
		ledgerOKRow(t, tx, "control: the legal reserve (10 units x 2)", r())
		refuse(t, "ledger_shape_reserve", []shapeCase{
			{"the arithmetic is wrong: 10 x 2 is not 19", r().with("available_delta", int64(-19), "reserved_delta", int64(19))},
			{"a positive available delta", r().with("available_delta", int64(20), "reserved_delta", int64(-20))},
			{"zero deltas", r().with("available_delta", int64(0), "reserved_delta", int64(0))},
			{"reserved delta is not the mirror of available", r().with("reserved_delta", int64(19))},
			{"no job", r().with("job_id", nil)},
			{"names a reserve", r().with("reserve_id", int64(1))},
			{"no unit (NULL)", r().with("unit", nil)},
			{"no units (NULL)", r().with("units", nil)},
			{"no unit cost (NULL)", r().with("unit_cost", nil)},
			{"zero units", r().with("units", int64(0), "available_delta", int64(0), "reserved_delta", int64(0))},
			{"a unit cost of zero", r().with("unit_cost", int64(0), "available_delta", int64(0), "reserved_delta", int64(0))},
		}, "ledger_check_reserve")

		// With the trigger ON, the three rows whose malformed field the trigger reads answer to the trigger first (it runs before the CHECKs): a NULL job has no snapshot, a NULL or zero
		// unit cost is not the dearest price. The same rows are still shape violations (proved above with the trigger off), so nothing became legal: only the first reason changed.
		ledgerRefusedCheck(t, tx, "no job: the trigger finds no snapshot", "reserve_needs_the_complete_snapshot", r().with("job_id", nil))
		ledgerRefusedCheck(t, tx, "no unit cost (NULL): the trigger's IS DISTINCT FROM is NULL-safe", "reserve_at_the_dearest_price", r().with("unit_cost", nil))
		ledgerRefusedCheck(t, tx, "a unit cost of zero: not the dearest price", "reserve_at_the_dearest_price", r().with("unit_cost", int64(0), "available_delta", int64(0), "reserved_delta", int64(0)))
	})

	base := ledgerOKRow(t, tx, "the reserve the settlement rows point at", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))

	t.Run("charge", func(t *testing.T) {
		// a charge is ONE unit (platform_0005), so the legal row is 1 unit x 1 credit
		c := func() ledgerCols { return ledgerChargeCols(a, "m1", ledgerJobA1, base, "m1_own", 1, 1) }
		ledgerOKRow(t, tx, "control: the legal charge (1 unit x 1)", c())
		refuse(t, "ledger_shape_charge", []shapeCase{
			{"moves available credits", c().with("available_delta", int64(1))},
			{"consumes nothing", c().with("reserved_delta", int64(0))},
			{"adds to reserved", c().with("reserved_delta", int64(1))},
			{"the arithmetic is wrong: 1 x 1 is not 2", c().with("reserved_delta", int64(-2))},
			{"no job", c().with("job_id", nil)},
			{"no reserve it settles (NULL)", c().with("reserve_id", nil)},
			{"no unit (NULL)", c().with("unit", nil)},
			{"no unit cost (NULL)", c().with("unit_cost", nil)},
		})
		// units = 0 and units = NULL are refused by BOTH ledger_charge_is_one_item and ledger_shape_charge, so each CHECK is proven ALONE by dropping the other in a savepoint that is rolled back
		// (which of the two PostgreSQL names first is not a rule worth pinning; the one-item rule is also NULL-safe: COALESCE turns the NULL into a refusal)
		for _, bad := range []shapeCase{
			{"zero units", c().with("units", int64(0), "reserved_delta", int64(0))},
			{"no units (NULL)", c().with("units", nil)},
		} {
			bad := bad
			t.Run(bad.name+" (the one-item rule alone, with the shape CHECK dropped)", func(t *testing.T) {
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_charge")
					ledgerRefusedCheck(t, sp, bad.name, "ledger_charge_is_one_item", bad.cols)
				})
			})
			t.Run(bad.name+" (the shape CHECK alone, with the one-item rule dropped)", func(t *testing.T) {
				ledgerTrial(t, tx, func(sp pgx.Tx) {
					ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_charge_is_one_item")
					ledgerRefusedCheck(t, sp, bad.name, "ledger_shape_charge", bad.cols)
				})
			})
		}
	})

	t.Run("release", func(t *testing.T) {
		// a release of exactly the remainder of a reserve of its own (platform_0005): 10 units x 2 = 20 credits held, nothing charged, so the legal release is 20
		relJob := ledgerNewJob(t, tx, ledgerUUID("d1", 1), a, "m1", true)
		rel := ledgerOKRow(t, tx, "the reserve the release points at", ledgerReserveCols(a, "m1", relJob, "m1_vendor", 10, 2))
		r := func() ledgerCols { return ledgerReleaseCols(a, "m1", relJob, rel, 20) }
		ledgerTrial(t, tx, func(sp pgx.Tx) { ledgerOKRow(t, sp, "control: the legal release of the whole remainder (20)", r()) })
		refuse(t, "ledger_shape_release", []shapeCase{
			{"releases nothing", r().with("available_delta", int64(0), "reserved_delta", int64(0))},
			{"a negative release", r().with("available_delta", int64(-20), "reserved_delta", int64(20))},
			{"available and reserved do not mirror", r().with("reserved_delta", int64(-19))},
			{"no job", r().with("job_id", nil)},
			{"no reserve it settles (NULL)", r().with("reserve_id", nil)},
			{"carries a unit", r().with("unit", "m1_own")},
			{"carries units", r().with("units", int64(5))},
			{"carries a unit cost", r().with("unit_cost", int64(1))},
		}, "ledger_check_release")

		// With the trigger ON, the rows whose amount or reserve the trigger reads answer to the trigger first: it runs before the CHECKs and a release is exactly the remainder or nothing.
		ledgerRefusedCheck(t, tx, "releases nothing", "release_is_the_remainder", r().with("available_delta", int64(0), "reserved_delta", int64(0)))
		ledgerRefusedCheck(t, tx, "a negative release", "release_is_the_remainder", r().with("available_delta", int64(-20), "reserved_delta", int64(20)))
		ledgerRefusedCheck(t, tx, "no reserve it settles (NULL): the trigger finds no reserve", "release_is_the_remainder", r().with("reserve_id", nil))
		ledgerOKRow(t, tx, "control: the legal release goes in once the refusals are done", r())
	})
}

// TestLedger_M3Kinds covers the entitlement events: admin required for enable and disable, none for the automatic expiry,
// zero deltas, and the module-metering foreign key (a monetary row on M3 and an M3 row on a metered module are refused). It
// fails if the admin rule, the zero-delta rule or the metering key is missing.
func TestLedger_M3Kinds(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA

	t.Run("controls: enable and disable name an admin, expire names none", func(t *testing.T) {
		ledgerOKRow(t, tx, "m3_enable with an admin", ledgerM3Cols("m3_enable", a))
		ledgerOKRow(t, tx, "m3_disable with an admin", ledgerM3Cols("m3_disable", a))
		// Since platform_0006 an m3_expire row is refused unless the user's entitlement is enabled and its date has passed (m3_expire_is_due; the full proof is in
		// TestM3Schema_ExpireIsRefusedUnlessDue): user C is enabled, time passes (ledgerMakeDue), and the system, which names no admin, expires it.
		ledgerOKRow(t, tx, "m3_enable for user C", ledgerM3Cols("m3_enable", ledgerUserC))
		ledgerMakeDue(t, tx, ledgerUserC)
		ledgerOKRow(t, tx, "m3_expire by the system (no admin) of an entitlement that is due", ledgerM3Cols("m3_expire", ledgerUserC))
	})

	t.Run("the admin rule", func(t *testing.T) {
		ledgerRefusedCheck(t, tx, "m3_enable without an admin", "ledger_shape_m3", ledgerM3Cols("m3_enable", a).with("acting_admin_id", nil))
		ledgerRefusedCheck(t, tx, "m3_disable without an admin", "ledger_shape_m3", ledgerM3Cols("m3_disable", a).with("acting_admin_id", nil))
		ledgerRefusedCheck(t, tx, "m3_expire WITH an admin (expiry is the system's act)", "ledger_shape_m3", ledgerM3Cols("m3_expire", a).with("acting_admin_id", ledgerAdminA))
		ledgerRefusedFK(t, tx, "m3_enable naming a user who is not an admin", "ledger_acting_admin_id_fkey", ledgerM3Cols("m3_enable", a).with("acting_admin_id", ledgerUserB))
	})

	t.Run("an entitlement event moves nothing and names no job, reserve or unit", func(t *testing.T) {
		for _, c := range []struct {
			name string
			cols ledgerCols
		}{
			{"available delta", ledgerM3Cols("m3_enable", a).with("available_delta", int64(1))},
			{"reserved delta", ledgerM3Cols("m3_enable", a).with("reserved_delta", int64(1))},
			{"a negative available delta", ledgerM3Cols("m3_disable", a).with("available_delta", int64(-1))},
			{"a job", ledgerM3Cols("m3_enable", a).with("job_id", ledgerJobA1)},
			{"a reserve", ledgerM3Cols("m3_enable", a).with("reserve_id", int64(1))},
			{"a unit", ledgerM3Cols("m3_enable", a).with("unit", "m1_own")},
			{"units", ledgerM3Cols("m3_expire", a).with("units", int64(1))},
			{"a unit cost", ledgerM3Cols("m3_expire", a).with("unit_cost", int64(1))},
		} {
			c := c
			t.Run(c.name, func(t *testing.T) { ledgerRefusedCheck(t, tx, "m3 event with "+c.name, "ledger_shape_m3", c.cols) })
		}
	})

	t.Run("the kind vocabulary is closed", func(t *testing.T) {
		ledgerRefusedCheck(t, tx, "kind adjust (not in the locked list)", "ledger_kind_vocabulary", ledgerM3Cols("m3_enable", a).with("kind", "adjust"))
		ledgerRefusedRow(t, tx, "a NULL kind", "23502", ledgerWhy{Column: "kind"}, ledgerM3Cols("m3_enable", a).with("kind", nil))
	})

	t.Run("module-metering foreign key: credits exist only on metered modules, entitlement events only on the unmetered one", func(t *testing.T) {
		ledgerOKRow(t, tx, "control: a grant on metered m1", ledgerGrantCols(a, "m1", 1))
		ledgerOKRow(t, tx, "control: a grant on metered m2", ledgerGrantCols(a, "m2", 1))
		ledgerRefusedFK(t, tx, "a grant to M3, which has no balance", "ledger_module_metering_fk", ledgerGrantCols(a, "m3", 5))
		ledgerRefusedFK(t, tx, "m3_enable on metered m1", "ledger_module_metering_fk", ledgerM3Cols("m3_enable", a).with("module_id", "m1"))
		ledgerRefusedFK(t, tx, "m3_expire on metered m2", "ledger_module_metering_fk", ledgerM3Cols("m3_expire", a).with("module_id", "m2"))
		ledgerRefusedFK(t, tx, "a module that does not exist", "ledger_module_metering_fk", ledgerGrantCols(a, "m9", 5))
	})

	t.Run("entitlement events never touch a balance and M3 never gets one", func(t *testing.T) {
		if b := ledgerBalance(t, tx, a, "m3"); b.Exists {
			t.Fatalf("a balance row exists for m3: %+v", b)
		}
		// The funded balances (A: 1000 on m1 and m2, B: 1000 on m1) plus the two 1-credit control grants above are the only credits there are.
		want := []string{ledgerUserA + "|m1|1001|0", ledgerUserA + "|m2|1001|0", ledgerUserB + "|m1|1000|0"}
		got := ledgerRows(t, tx, "SELECT user_id::text, module_id, available, reserved FROM platform.balances ORDER BY 1, 2")
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("balances after the m3 events = %v, want %v: an entitlement event must change nothing", got, want)
		}
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE kind LIKE 'm3_%' AND NOT monetary"); n != 4 {
			t.Fatalf("%d m3 events are in the ledger and flagged non-monetary, want the 4 legal controls (enable, disable, enable, expire)", n)
		}
		ledgerRequireViewsEmpty(t, tx, "after the m3 events")
	})
}

// TestLedger_IdempotencyKey checks that one (user, module, key) can be written once: a retried grant, reserve or charge
// cannot double-count. It fails if the unique constraint is missing, the blank-key check is missing, or a refused retry
// leaves a trace.
func TestLedger_IdempotencyKey(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b := ledgerUserA, ledgerUserB

	first := ledgerGrantCols(a, "m1", 100).with("idempotency_key", "retry-me")
	ledgerOKRow(t, tx, "control: the first use of the key", first)
	ledgerRequireBalance(t, tx, "after the first grant", a, "m1", 100, 0)

	t.Run("the same (user, module, key) twice is refused and counts nothing", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "a retried grant", "23505", ledgerWhy{Constraint: "ledger_idempotency_key"}, first)
		ledgerRequireBalance(t, tx, "after the refused retry", a, "m1", 100, 0)
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE idempotency_key = 'retry-me'"); n != 1 {
			t.Fatalf("%d ledger rows carry the key, want exactly 1", n)
		}
	})
	t.Run("the key covers every kind: a reserve reusing a grant's key is refused", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "a reserve with the grant's key", "23505", ledgerWhy{Constraint: "ledger_idempotency_key"},
			ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 1, 2).with("idempotency_key", "retry-me")) // at the dearest price, so only the key can refuse it
	})
	t.Run("controls: the same key is fine for another user and for another module", func(t *testing.T) {
		ledgerOKRow(t, tx, "the same key for user B", first.with("user_id", b))
		ledgerOKRow(t, tx, "the same key on module m2", first.with("module_id", "m2"))
		ledgerRequireBalance(t, tx, "user A on m1 is still 100", a, "m1", 100, 0)
	})
	t.Run("a key must say something", func(t *testing.T) {
		ledgerRefusedCheck(t, tx, "an empty key", "ledger_idempotency_key_not_blank", first.with("idempotency_key", ""))
		ledgerRefusedCheck(t, tx, "a blank key", "ledger_idempotency_key_not_blank", first.with("idempotency_key", "   "))
		ledgerRefusedRow(t, tx, "no key at all (NULL)", "23502", ledgerWhy{Column: "idempotency_key"}, first.with("idempotency_key", nil))
	})
}

// TestLedger_ForeignKeys covers the three keys that tie a row to its job, its reserve and its frozen price. For each, the
// row that follows the rule is the control. It fails if a key is missing, if it ignores the user or the module, or if it
// accepts a price that is not in the job's snapshot.
func TestLedger_ForeignKeys(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, b := ledgerUserA, ledgerUserB
	reserveA := ledgerOKRow(t, tx, "A's reserve on job A1", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
	reserveB := ledgerOKRow(t, tx, "B's reserve on B's own job (the control for the job key)", ledgerReserveCols(b, "m1", ledgerJobB1, "m1_vendor", 5, 2))
	chargeA := ledgerOKRow(t, tx, "A's charge", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 1))
	grantA := ledgerInt64(t, tx, "SELECT id FROM platform.ledger WHERE user_id = $1 AND kind = 'grant' AND module_id = 'm1'", a)

	// Since platform_0005 three BEFORE INSERT triggers answer before the foreign keys can: the reserve trigger (a rate that is not the dearest price of a complete snapshot) and the release
	// trigger (an amount that is not the remainder of a reserve it finds). Every row below that is meant to prove a FOREIGN KEY is therefore built to PASS those triggers (a reserve at the dearest
	// price on a job that holds no reserve yet, since a second reserve for one job is refused by its unique index before any foreign key; a release of exactly its reserve's remainder), or the
	// trigger is switched off in a savepoint that is rolled back, so the key is still proven ALONE and as strictly as before.
	fkWithout := func(t *testing.T, what, constraint string, triggers []string, c ledgerCols) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			for _, trg := range triggers {
				ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER "+trg)
			}
			ledgerRefusedFK(t, sp, what, constraint, c)
		})
	}

	t.Run("ledger_job_fk: a job's rows belong to the job's own user and module", func(t *testing.T) {
		// job A1b is user A's job and holds no reserve yet: B's reserve at the dearest price passes the trigger and the unique index, and only the key can refuse it
		ledgerRefusedFK(t, tx, "B reserving against A's job", "ledger_job_fk", ledgerReserveCols(b, "m1", ledgerJobA1b, "m1_vendor", 1, 2))
		// the release is exactly the remainder (20 held, 1 charged), so the release trigger lets it through to the key
		remA := ledgerReserveRemainder(t, tx, reserveA)
		ledgerRefusedFK(t, tx, "a release recorded on module m2 against an m1 job", "ledger_job_fk", ledgerReleaseCols(a, "m2", ledgerJobA1, reserveA, remA))
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerOKRow(t, sp, "control: A releasing on A's own job and module", ledgerReleaseCols(a, "m1", ledgerJobA1, reserveA, remA))
		})
	})

	t.Run("ledger_reserve_fk: a charge or release points at a RESERVE row of the same user and job", func(t *testing.T) {
		ledgerRefusedFK(t, tx, "B's charge pointing at A's reserve", "ledger_reserve_fk", ledgerChargeCols(b, "m1", ledgerJobB1, reserveA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "A's charge on job A1b pointing at the reserve of job A1", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1b, reserveA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a charge pointing at a grant row", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1, grantA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a charge pointing at nothing", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1, 999999999, "m1_own", 1, 1))
		// a release pointing at a charge row finds no reserve for the trigger: with the trigger on that is the trigger's refusal; with it off the key refuses the very same row
		ledgerRefusedCheck(t, tx, "a release pointing at a charge row: the trigger finds no reserve", "release_is_the_remainder", ledgerReleaseCols(a, "m1", ledgerJobA1, chargeA, 1))
		fkWithout(t, "a release pointing at a charge row", "ledger_reserve_fk", []string{"ledger_check_release"}, ledgerReleaseCols(a, "m1", ledgerJobA1, chargeA, 1))
		// B's release names A's reserve: the trigger only reads the reserve (it is A's remainder it compares), so the exact remainder passes it and the key refuses B
		ledgerRefusedFK(t, tx, "B's release pointing at A's reserve", "ledger_reserve_fk", ledgerReleaseCols(b, "m1", ledgerJobB1, reserveA, ledgerReserveRemainder(t, tx, reserveA)))
		ledgerOKRow(t, tx, "control: A's charge at A's reserve", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 1))
		ledgerOKRow(t, tx, "control: B's charge at B's reserve", ledgerChargeCols(b, "m1", ledgerJobB1, reserveB, "m1_own", 1, 1))
		ledgerOKRow(t, tx, "control: B's release at B's reserve (its exact remainder)", ledgerReleaseCols(b, "m1", ledgerJobB1, reserveB, ledgerReserveRemainder(t, tx, reserveB)))
	})

	t.Run("ledger_frozen_price_fk: the unit and its price must be in the job's snapshot", func(t *testing.T) {
		fk := func(what string, c ledgerCols) { ledgerRefusedFK(t, tx, what, "ledger_frozen_price_fk", c) }
		// reserves on job A1b (no reserve yet), at price 2, the dearest the snapshot holds, so the reserve trigger passes and only the key can refuse the unit
		fk("a reserve of a unit of another module (m2_accepted at 2)", ledgerReserveCols(a, "m1", ledgerJobA1b, "m2_accepted", 10, 2))
		fk("a reserve of a unit that does not exist", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_bogus", 1, 2))
		fk("the right unit at the price of another unit (m1_cache costs 1, not 2)", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_cache", 5, 2))
		// the two refusals the trigger would otherwise answer first: a price the snapshot does not hold, and a job with no snapshot at all. Both the trigger's reason (with it on) and the key's
		// (with it off) are asserted, so the key is as strictly proven as in Phase 1 and the trigger's answer is pinned beside it.
		ledgerRefusedCheck(t, tx, "a reserve at price 3, which the snapshot does not hold: not the dearest price", "reserve_at_the_dearest_price", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 10, 3))
		fkWithout(t, "a reserve at price 3, which the snapshot does not hold", "ledger_frozen_price_fk", []string{"ledger_check_reserve"}, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 10, 3))
		ledgerRefusedCheck(t, tx, "a reserve on a job that has no snapshot at all: no complete snapshot", "reserve_needs_the_complete_snapshot", ledgerReserveCols(a, "m1", ledgerJobA0, "m1_own", 1, 1))
		fkWithout(t, "a reserve on a job that has no snapshot at all", "ledger_frozen_price_fk", []string{"ledger_check_reserve"}, ledgerReserveCols(a, "m1", ledgerJobA0, "m1_own", 1, 1))
		fk("a charge of m1_own at 2, which is the vendor's price", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 2))
		ledgerOKRow(t, tx, "control: a charge at the snapshot price of m1_vendor", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_vendor", 1, 2))
		ledgerOKRow(t, tx, "control: a reserve at the snapshot's dearest price (m1_vendor, 2)", ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2))
	})
	ledgerRequireViewsEmpty(t, tx, "after the foreign-key cases")
}

// TestLedger_FrozenPriceSurvivesUnitCostChange is the heart of locked decision 7: the price a job settles at is the price it
// was confirmed at. A real admin changes a unit cost twice, through the role the service runs as (platform_app, which holds
// UPDATE on the cost columns only); three jobs are confirmed at 1, 5 and 9. Afterwards, with the unit cost at 9, every job
// accepts a charge at ITS price and refuses the other two. It fails if settlement ever reads platform.unit_costs, or if the
// snapshot is not the key of the ledger.
func TestLedger_FrozenPriceSurvivesUnitCostChange(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA
	ledgerFund(t, tx, a, "m1", 1000)

	prices := []int64{1, 5, 9}
	jobs := make([]string, len(prices))
	reserves := make([]int64, len(prices))
	for i, p := range prices {
		if i > 0 {
			ledgerSwitchRole(t, tx, "platform_app")
			ledgerExpectOK(t, tx, fmt.Sprintf("platform_app changes m1_own to %d through a named admin", p),
				"UPDATE platform.unit_costs SET cost = $1, changed_by = $2, change_reason = $3 WHERE unit = 'm1_own'", p, ledgerAdminA, fmt.Sprintf("price review %d", i))
			ledgerResetRole(t, tx)
		}
		if got := ledgerInt64(t, tx, "SELECT cost FROM platform.unit_costs WHERE unit = 'm1_own'"); got != p {
			t.Fatalf("platform.unit_costs says m1_own costs %d, want %d", got, p)
		}
		jobs[i] = ledgerNewJob(t, tx, ledgerUUID("c1", i), a, "m1", true)
		if got := ledgerInt64(t, tx, "SELECT cost FROM platform.job_unit_prices WHERE job_id = $1 AND unit = 'm1_own'", jobs[i]); got != p {
			t.Fatalf("job %d was confirmed at m1_own = %d but its snapshot holds %d", i, p, got)
		}
		// the reserve is at the DEAREST price of the job's snapshot (platform_0005): m1_vendor at 2 for the job confirmed while m1_own cost 1, m1_own itself (5, then 9) for the others
		reserves[i] = ledgerPut(t, tx, ledgerReserveAtDearest(t, tx, a, "m1", jobs[i], 10))
	}

	t.Run("the price changes are recorded: who, why, from what", func(t *testing.T) {
		got := ledgerRows(t, tx, `SELECT COALESCE(old_cost::text, ''), new_cost, COALESCE(changed_by::text, ''), COALESCE(change_reason, '')
			FROM platform.unit_cost_history WHERE unit = 'm1_own' ORDER BY id`)
		want := []string{"|1||seeded", "1|5|" + ledgerAdminA + "|price review 1", "5|9|" + ledgerAdminA + "|price review 2"}
		if len(got) != len(want) {
			t.Fatalf("history rows %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("history row %d is %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("each job settles at the price it was confirmed at and refuses every other", func(t *testing.T) {
		for i, jp := range prices {
			for _, cp := range prices {
				c := ledgerChargeCols(a, "m1", jobs[i], reserves[i], "m1_own", 1, cp)
				if cp == jp {
					ledgerOKRow(t, tx, fmt.Sprintf("control: job confirmed at %d charged at %d", jp, cp), c)
				} else {
					ledgerRefusedFK(t, tx, fmt.Sprintf("job confirmed at %d charged at %d (the table now says %d)", jp, cp, prices[len(prices)-1]), "ledger_frozen_price_fk", c)
				}
			}
			if got := ledgerInt64(t, tx, "SELECT -sum(reserved_delta) FROM platform.ledger WHERE kind = 'charge' AND job_id = $1", jobs[i]); got != jp {
				t.Fatalf("job %d (confirmed at %d) was charged %d credits in total, want exactly its one unit at %d", i, jp, got, jp)
			}
		}
		// Every job consumed exactly one unit at its own price: 1 + 5 + 9 credits out of the 20 + 50 + 90 reserved (10 units at each job's dearest price: 2, 5 and 9).
		if got := ledgerInt64(t, tx, "SELECT -sum(reserved_delta) FROM platform.ledger WHERE kind = 'charge'"); got != 15 {
			t.Fatalf("charged credits = %d, want 1 + 5 + 9 = 15", got)
		}
		ledgerRequireViewsEmpty(t, tx, "after the three jobs settled")
	})
}

// TestLedger_OverdrawRefusedByDatabase proves the database, not application code, refuses to spend credits that are not there.
// The boundary is tested on both sides (exactly the balance passes, one credit more is refused), and a refused row leaves
// nothing behind. It fails if either per-balance CHECK is missing or if the trigger writes the balance before the CHECK sees it.
func TestLedger_OverdrawRefusedByDatabase(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA
	rowsOf := func(t *testing.T) int64 {
		t.Helper()
		return ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE user_id = $1", a)
	}

	ledgerFund(t, tx, a, "m1", 100)
	ledgerRequireBalance(t, tx, "after the grant", a, "m1", 100, 0)

	t.Run("available cannot go below zero", func(t *testing.T) {
		n := rowsOf(t)
		ledgerRefusedRow(t, tx, "reserving 102 credits (51 x 2) with 100 available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"},
			ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 51, 2))
		if rowsOf(t) != n {
			t.Fatalf("the refused reserve left a ledger row behind")
		}
		ledgerRequireBalance(t, tx, "after the refused reserve", a, "m1", 100, 0)
		reserve := ledgerOKRow(t, tx, "control: reserving exactly the 100 available (50 x 2)", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 50, 2))
		ledgerRequireBalance(t, tx, "after reserving everything", a, "m1", 0, 100)
		// "one more credit when none is available": a reserve is at the dearest price (platform_0005), so the smallest reserve is one unit at 2 credits; on job A1b, which holds no reserve yet
		ledgerRefusedRow(t, tx, "two more credits (one unit at the dearest price) when none is available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"},
			ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 1, 2))

		t.Run("reserved cannot go below zero", func(t *testing.T) {
			// A charge is ONE unit (platform_0005), so the 100 reserved credits are consumed one unit at a time: 49 charges of 2 leave 2, the 50th takes exactly the last 2, a 51st would take the
			// pooled reserved column below zero.
			for i := 0; i < 49; i++ {
				ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_vendor", 1, 2))
			}
			ledgerRequireBalance(t, tx, "after 49 charges", a, "m1", 0, 2)
			ledgerOKRow(t, tx, "control: the 50th charge takes exactly the last 2 reserved credits", ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_vendor", 1, 2))
			ledgerRequireBalance(t, tx, "after settling everything", a, "m1", 0, 0)
			// the per-balance wall is proven ALONE: the per-reserve wall (which would refuse the same row) is switched off in a savepoint that is rolled back
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_reserves")
				ledgerRefusedRow(t, sp, "a 51st unit against 100 reserved credits that are all spent", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative", Table: "balances"},
					ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_vendor", 1, 2))
			})
			ledgerRequireBalance(t, tx, "after the refused charge", a, "m1", 0, 0)
		})
	})

	t.Run("a release that is not the remainder is refused (platform_0005); the per-balance wall still stands behind it", func(t *testing.T) {
		ledgerFund(t, tx, a, "m1", 20)
		reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_vendor", 5, 2))
		ledgerRequireBalance(t, tx, "after the second reserve", a, "m1", 10, 10)
		ledgerRefusedRow(t, tx, "releasing 11 credits of a reserve of 10", "23514", ledgerWhy{Constraint: "release_is_the_remainder"}, ledgerReleaseCols(a, "m1", ledgerJobA1b, reserve, 11))
		ledgerRefusedRow(t, tx, "releasing 9 credits of a reserve of 10", "23514", ledgerWhy{Constraint: "release_is_the_remainder"}, ledgerReleaseCols(a, "m1", ledgerJobA1b, reserve, 9))
		// Phase 1's assertion, kept as strict: an over-large release is refused by the per-balance CHECK, proven ALONE with the two newer walls (the remainder rule and the per-reserve cap) off.
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_check_release")
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_apply_to_reserves")
			ledgerRefusedRow(t, sp, "releasing 11 credits of a reserve of 10, the newer walls off", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative", Table: "balances"},
				ledgerReleaseCols(a, "m1", ledgerJobA1b, reserve, 11))
		})
		ledgerRequireBalance(t, tx, "after the refused releases", a, "m1", 10, 10)
		ledgerOKRow(t, tx, "control: releasing exactly the 10", ledgerReleaseCols(a, "m1", ledgerJobA1b, reserve, 10))
		ledgerRequireBalance(t, tx, "after the release", a, "m1", 20, 0)
	})
	ledgerRequireViewsEmpty(t, tx, "after the overdraw cases")
}

// TestLedger_GeneratedColumnsAndIdentity checks what the ledger derives itself: monetary and reserve_kind are generated (no
// writer can set them, and they come out right for every kind) and ids are identity values that only the database assigns,
// strictly increasing. It fails if a generated expression is wrong (the foreign keys that use them would then mis-route) or
// becomes writable.
func TestLedger_GeneratedColumnsAndIdentity(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a := ledgerUserA

	grant := ledgerFund(t, tx, a, "m1", 100)
	reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 5, 2)) // 10 credits, at the dearest price of the snapshot (platform_0005)
	charge := ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1))
	release := ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, ledgerReserveRemainder(t, tx, reserve))) // exactly the 9 credits the reserve has left
	adjust := ledgerPut(t, tx, ledgerAdjustmentCols(a, "m1", 1))                                                          // platform_0004: 100 - 10 + 9 = 99 available, so one credit can be taken back
	enable := ledgerPut(t, tx, ledgerM3Cols("m3_enable", a))
	disable := ledgerPut(t, tx, ledgerM3Cols("m3_disable", a))
	// m3_expire is refused unless the entitlement is enabled and its date has passed (platform_0006): user B is enabled, time passes, then the system expires it (ids stay increasing in the order below)
	ledgerPut(t, tx, ledgerM3Cols("m3_enable", ledgerUserB))
	ledgerMakeDue(t, tx, ledgerUserB)
	expire := ledgerPut(t, tx, ledgerM3Cols("m3_expire", ledgerUserB))

	t.Run("the generated columns come out right for every kind", func(t *testing.T) {
		for _, c := range []struct {
			name         string
			id           int64
			want         string
			explainsWhat string
		}{
			{"grant", grant, "true|", "a grant is money and points at no reserve"},
			{"reserve", reserve, "true|", "a reserve is money and points at no reserve"},
			{"charge", charge, "true|reserve", "a charge must point at a reserve row"},
			{"release", release, "true|reserve", "a release must point at a reserve row"},
			{"adjustment", adjust, "true|", "an adjustment is money: it moves a balance, so the balance trigger and balance_drift must see it; it points at no reserve"},
			{"m3_enable", enable, "false|", "an entitlement event is not money"},
			{"m3_disable", disable, "false|", "an entitlement event is not money"},
			{"m3_expire", expire, "false|", "an entitlement event is not money"},
		} {
			got := ledgerString(t, tx, "SELECT monetary::text || '|' || COALESCE(reserve_kind, '') FROM platform.ledger WHERE id = $1", c.id)
			if got != c.want {
				t.Fatalf("%s row: monetary|reserve_kind = %q, want %q (%s)", c.name, got, c.want, c.explainsWhat)
			}
		}
	})

	t.Run("no writer can set a generated column or the identity", func(t *testing.T) {
		g := func() ledgerCols { return ledgerGrantCols(a, "m1", 1) }
		ledgerRefusedRow(t, tx, "writing monetary", "428C9", ledgerWhy{Message: `column "monetary"`}, g().with("monetary", false))
		ledgerRefusedRow(t, tx, "writing reserve_kind", "428C9", ledgerWhy{Message: `column "reserve_kind"`}, g().with("reserve_kind", "reserve"))
		ledgerRefusedRow(t, tx, "choosing an id", "428C9", ledgerWhy{Message: `column "id"`}, g().with("id", int64(999999)))
		ledgerOKRow(t, tx, "control: the same grant without those columns", g())
	})

	t.Run("ids are assigned by the database, strictly increasing in insertion order", func(t *testing.T) {
		ids := []int64{grant, reserve, charge, release, adjust, enable, disable, expire}
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("ledger ids %v are not strictly increasing", ids)
			}
		}
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// Adjustment (platform_0004, the owner's ruling OD-4): a clawback. An admin removes AVAILABLE credits, with a reason, tied to no
// job; it never touches reserved credits and never takes a balance below zero; it is money (the balance trigger applies it and
// balance_drift covers it); and an admin_audit row is written by trigger so no writer can skip it. Four tests: the row shape,
// the arithmetic, the audit row, and the same through the two roles the service really runs as.
// ---------------------------------------------------------------------------------------------------------------------

// TestLedger_AdjustmentShapes is TestLedger_KindShapes for the kind platform_0004 adds. The legal adjustment is the control and
// every refused row differs from it in exactly one thing, so ledger_shape_adjustment is the only rule that can be firing. It
// also covers what is NOT the shape's business: the module-metering key (an adjustment is money, so it exists only on metered
// modules), the closed kind vocabulary, the admin and user keys and the idempotency key. It fails if the CHECK is missing,
// weakened (a NULL slips through: the NULL trap) or renamed, or if an adjustment is accepted on M3.
func TestLedger_AdjustmentShapes(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a := ledgerUserA
	adj := func() ledgerCols { return ledgerAdjustmentCols(a, "m1", 10) }

	first := adj()
	id := ledgerOKRow(t, tx, "control: the legal adjustment (10 credits off 1000 available)", first)
	ledgerRequireBalance(t, tx, "after the legal adjustment", a, "m1", 990, 0)
	if got := ledgerRows(t, tx, "SELECT kind, available_delta, reserved_delta, COALESCE(job_id::text, ''), acting_admin_id::text, reason FROM platform.ledger WHERE id = $1", id); len(got) != 1 || got[0] != "adjustment|-10|0||"+ledgerAdminA+"|test clawback" {
		t.Fatalf("the adjustment row reads %v, want adjustment|-10|0||%s|test clawback", got, ledgerAdminA)
	}
	rowsBefore := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger")

	t.Run("refused by ledger_shape_adjustment, one difference at a time", func(t *testing.T) {
		for _, c := range []struct {
			name string
			cols ledgerCols
		}{
			{"a positive delta (that would be a grant)", adj().with("available_delta", int64(10))},
			{"a zero delta", adj().with("available_delta", int64(0))},
			{"moves reserved credits", adj().with("reserved_delta", int64(1))},
			{"moves reserved credits instead of available ones", adj().with("available_delta", int64(0), "reserved_delta", int64(-10))},
			{"names a job", adj().with("job_id", ledgerJobA1)},
			{"names a reserve", adj().with("reserve_id", int64(1))},
			{"carries a unit", adj().with("unit", "m1_own")},
			{"carries units", adj().with("units", int64(1))},
			{"carries a unit cost", adj().with("unit_cost", int64(1))},
			{"no acting admin (NULL)", adj().with("acting_admin_id", nil)},
			{"no reason (NULL)", adj().with("reason", nil)},
			{"an empty reason", adj().with("reason", "")},
			{"a blank reason", adj().with("reason", "   ")},
		} {
			c := c
			t.Run(c.name, func(t *testing.T) { ledgerRefusedCheck(t, tx, c.name, "ledger_shape_adjustment", c.cols) })
		}
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"); got != rowsBefore {
			t.Fatalf("the refused adjustments left %d ledger rows behind", got-rowsBefore)
		}
		ledgerRequireBalance(t, tx, "after the refused adjustments (nothing may have moved)", a, "m1", 990, 0)
	})

	t.Run("what the shape does not decide: the keys, the vocabulary, the metering", func(t *testing.T) {
		ledgerRefusedFK(t, tx, "the acting admin must be an admin, not just a user", "ledger_acting_admin_id_fkey", adj().with("acting_admin_id", ledgerUserB))
		ledgerRefusedFK(t, tx, "the user must exist", "ledger_user_id_fkey", adj().with("user_id", ledgerNoSuchID))
		ledgerRefusedFK(t, tx, "an adjustment on M3, which has no balance (an adjustment is money)", "ledger_module_metering_fk", adj().with("module_id", "m3"))
		ledgerRefusedFK(t, tx, "an adjustment on a module that does not exist", "ledger_module_metering_fk", adj().with("module_id", "m9"))
		ledgerRefusedCheck(t, tx, "the kind vocabulary is closed: `adjustments` is not a kind", "ledger_kind_vocabulary", adj().with("kind", "adjustments"))
		ledgerRefusedRow(t, tx, "a retried adjustment (the same user, module and key)", "23505", ledgerWhy{Constraint: "ledger_idempotency_key"}, first)
		ledgerRequireBalance(t, tx, "after the refused retry (the first adjustment counted once)", a, "m1", 990, 0)
		ledgerOKRow(t, tx, "control: the same key for another user is a different row", first.with("user_id", ledgerUserB))
		ledgerRequireBalance(t, tx, "user B's own adjustment", ledgerUserB, "m1", 990, 0)
		ledgerRequireViewsEmpty(t, tx, "after the adjustment shape cases")
	})
}

// TestLedger_AdjustmentMoney proves the arithmetic of a clawback, with the numbers of the ruling except one: available 10 and reserved 6 (16
// granted, one reserve of 3 units at the dearest price 2; the ruling's 5 reserved cannot be written since platform_0005 holds a reserve at the dearest price). An adjustment of 10
// passes and reserved stays 6; an adjustment of 11 is refused by the balance CHECK even
// though 16 credits exist, and the refusal leaves no ledger row and no audit row; one credit more is refused again; a user who
// never held credits cannot be put into debt; another module's balance is untouched. It fails if an adjustment can touch reserved
// credits, take available below zero, or leave a trace when it is refused.
func TestLedger_AdjustmentMoney(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	a, b := ledgerUserA, ledgerUserB
	count := func(t *testing.T, table string) int64 {
		t.Helper()
		return ledgerInt64(t, tx, "SELECT count(*) FROM "+table)
	}

	ledgerFund(t, tx, a, "m1", 16)
	ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 3, 2))
	ledgerRequireBalance(t, tx, "the starting point: 16 granted, 6 of them reserved", a, "m1", 10, 6)
	ledgerRequireViewsEmpty(t, tx, "at the starting point")

	t.Run("adjusting 11 is refused although 16 credits exist: 6 are reserved and an adjustment never draws on them", func(t *testing.T) {
		nLedger, nAudit := count(t, "platform.ledger"), count(t, "platform.admin_audit")
		ledgerRefusedRow(t, tx, "adjusting 11 off 10 available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"}, ledgerAdjustmentCols(a, "m1", 11))
		if got := count(t, "platform.ledger"); got != nLedger {
			t.Fatalf("the refused adjustment left %d ledger row(s) behind", got-nLedger)
		}
		if got := count(t, "platform.admin_audit"); got != nAudit {
			t.Fatalf("the refused adjustment left %d admin_audit row(s) behind", got-nAudit)
		}
		ledgerRequireBalance(t, tx, "after the refused adjustment", a, "m1", 10, 6)
	})

	t.Run("modules are separate: an m2 adjustment leaves the m1 balance alone", func(t *testing.T) {
		ledgerFund(t, tx, a, "m2", 20)
		ledgerOKRow(t, tx, "control: an m2 adjustment of 7", ledgerAdjustmentCols(a, "m2", 7))
		ledgerRequireBalance(t, tx, "m2 after its adjustment", a, "m2", 13, 0)
		ledgerRequireBalance(t, tx, "m1 after the m2 adjustment", a, "m1", 10, 6)
	})

	t.Run("control: adjusting exactly the 10 available passes, and reserved stays 6", func(t *testing.T) {
		nAudit := count(t, "platform.admin_audit")
		ledgerOKRow(t, tx, "adjusting exactly the available credits", ledgerAdjustmentCols(a, "m1", 10))
		ledgerRequireBalance(t, tx, "after the adjustment of 10", a, "m1", 0, 6)
		if got := count(t, "platform.admin_audit"); got != nAudit+1 {
			t.Fatalf("the adjustment wrote %d admin_audit row(s), want exactly 1", got-nAudit)
		}
	})

	t.Run("one credit more is refused: nothing is available and the 6 reserved credits are not a reserve to draw on", func(t *testing.T) {
		nLedger := count(t, "platform.ledger")
		ledgerRefusedRow(t, tx, "adjusting 1 off 0 available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"}, ledgerAdjustmentCols(a, "m1", 1))
		if got := count(t, "platform.ledger"); got != nLedger {
			t.Fatalf("the refused adjustment left %d ledger row(s) behind", got-nLedger)
		}
		ledgerRequireBalance(t, tx, "after the refused adjustment", a, "m1", 0, 6)
	})

	t.Run("an adjustment cannot put a user into debt: a user who never held credits", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "adjusting 1 off a user with no balance row at all", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"}, ledgerAdjustmentCols(b, "m1", 1))
		if bal := ledgerBalance(t, tx, b, "m1"); bal.Exists {
			t.Fatalf("the refused adjustment left a balance row behind for user B: %+v", bal)
		}
	})
	ledgerRequireViewsEmpty(t, tx, "after the adjustments (the balance equals the sum of the ledger, adjustments included)")
}

// TestLedger_AdjustmentAudit proves the second record of a clawback: exactly one admin_audit row per adjustment, with the fields
// the ruling names, written by a trigger so no writer can skip it - and none for anything else. The paired controls: the same
// instrument sees nothing before the first adjustment, the row disappears when the trigger is disabled (so it is the trigger's
// doing), and a refused adjustment leaves no row. It fails if the trigger is missing, writes a wrong field, writes twice, or
// also fires for another kind.
func TestLedger_AdjustmentAudit(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeedFunded(t, tx)
	a, b := ledgerUserA, ledgerUserB
	const auditCols = "admin_id::text, entry_type, action, target_type, target_id, COALESCE(target_user_id::text, ''), reason"
	adjustmentAudits := func(t *testing.T, db ledgerDB) []string {
		t.Helper()
		return ledgerRows(t, db, "SELECT "+auditCols+" FROM platform.admin_audit WHERE action = 'ledger.adjustment' ORDER BY id")
	}
	totalAudits := func(t *testing.T, db ledgerDB) int64 {
		t.Helper()
		return ledgerInt64(t, db, "SELECT count(*) FROM platform.admin_audit")
	}

	t.Run("no audit row for a grant, a reserve, a charge, a release or an entitlement event", func(t *testing.T) {
		if n := totalAudits(t, tx); n != 0 {
			t.Fatalf("%d admin_audit rows after three grants, want none: only an adjustment writes one", n)
		}
		reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))
		for i := 0; i < 3; i++ { // a charge is one unit (platform_0005)
			ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1))
		}
		ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, 17)) // exactly the remainder: 20 - 3
		for _, k := range []string{"m3_enable", "m3_disable"} {
			ledgerPut(t, tx, ledgerM3Cols(k, a))
		}
		ledgerPut(t, tx, ledgerM3Cols("m3_enable", ledgerUserC)) // an m3_expire is refused unless the entitlement is enabled and due (platform_0006)
		ledgerMakeDue(t, tx, ledgerUserC)
		ledgerPut(t, tx, ledgerM3Cols("m3_expire", ledgerUserC))
		if n := totalAudits(t, tx); n != 0 {
			t.Fatalf("%d admin_audit rows after a reserve, charges, a release and four entitlement events, want none", n)
		}
	})

	var firstID int64
	t.Run("one adjustment writes exactly one audit row, with every field of the ruling", func(t *testing.T) {
		const key, reason = "adj-audit-1", "chargeback dispute 4711"
		firstID = ledgerOKRow(t, tx, "the adjustment", ledgerAdjustmentCols(a, "m1", 7).with("idempotency_key", key, "reason", reason))
		got := adjustmentAudits(t, tx)
		want := fmt.Sprintf("%s|action|ledger.adjustment|ledger|%d|%s|%s", ledgerAdminA, firstID, a, reason)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("the admin_audit rows for the adjustment are %v, want exactly [%s]", got, want)
		}
		details := fmt.Sprintf(`{"module_id": "m1", "credits_removed": 7, "idempotency_key": %q}`, key)
		if !ledgerBool(t, tx, "SELECT details = $1::text::jsonb FROM platform.admin_audit WHERE action = 'ledger.adjustment' AND target_id = $2", details, fmt.Sprint(firstID)) {
			t.Fatalf("details of the audit row is %s, want %s (module_id, credits_removed as a POSITIVE number, idempotency_key)", ledgerString(t, tx, "SELECT details::text FROM platform.admin_audit WHERE action = 'ledger.adjustment' AND target_id = $1", fmt.Sprint(firstID)), details)
		}
		if got := ledgerString(t, tx, "SELECT jsonb_typeof(details -> 'credits_removed') FROM platform.admin_audit WHERE target_id = $1", fmt.Sprint(firstID)); got != "number" {
			t.Fatalf("credits_removed is a JSON %s, want a number", got)
		}
		if n := totalAudits(t, tx); n != 1 {
			t.Fatalf("%d admin_audit rows in all, want exactly the one", n)
		}
	})

	t.Run("a retried adjustment is refused and writes no second row", func(t *testing.T) {
		ledgerRefusedRow(t, tx, "the same adjustment again", "23505", ledgerWhy{Constraint: "ledger_idempotency_key"},
			ledgerAdjustmentCols(a, "m1", 7).with("idempotency_key", "adj-audit-1"))
		if n := len(adjustmentAudits(t, tx)); n != 1 {
			t.Fatalf("%d audit rows after the refused retry, want still 1", n)
		}
	})

	t.Run("a refused adjustment (overdraw, shape) writes no row", func(t *testing.T) {
		nLedger := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger")
		ledgerRefusedRow(t, tx, "an adjustment of 5000, far more than the 990 available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"}, ledgerAdjustmentCols(a, "m1", 5000))
		ledgerRefusedCheck(t, tx, "an adjustment without a reason", "ledger_shape_adjustment", ledgerAdjustmentCols(a, "m1", 1).with("reason", nil))
		if n := len(adjustmentAudits(t, tx)); n != 1 {
			t.Fatalf("%d audit rows after two refused adjustments, want still 1", n)
		}
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"); got != nLedger {
			t.Fatalf("the refused adjustments left %d ledger rows behind", got-nLedger)
		}
	})

	t.Run("a second adjustment by another admin for another user has its own row", func(t *testing.T) {
		secondID := ledgerOKRow(t, tx, "admin B's adjustment for user B", ledgerAdjustmentCols(b, "m1", 3).with("acting_admin_id", ledgerAdminB, "reason", "second clawback"))
		got := adjustmentAudits(t, tx)
		want := []string{
			fmt.Sprintf("%s|action|ledger.adjustment|ledger|%d|%s|chargeback dispute 4711", ledgerAdminA, firstID, a),
			fmt.Sprintf("%s|action|ledger.adjustment|ledger|%d|%s|second clawback", ledgerAdminB, secondID, b),
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("the admin_audit rows are %v, want %v", got, want)
		}
	})

	t.Run("control: the row is the trigger's doing - with the trigger disabled the adjustment is written and the audit row is not", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			before := totalAudits(t, sp)
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_audit_adjustment")
			ledgerOKRow(t, sp, "an adjustment with the audit trigger off", ledgerAdjustmentCols(a, "m1", 1))
			if got := totalAudits(t, sp); got != before {
				t.Fatalf("%d audit rows were written with the trigger disabled: something other than the trigger writes them, so the assertions above prove nothing about it", got-before)
			}
		})
	})

	t.Run("a disabled admin cannot adjust, and the refusal leaves nothing behind", func(t *testing.T) {
		ledgerMust(t, tx, "UPDATE platform.admins SET disabled_at = now() WHERE id = $1", ledgerAdminB)
		nLedger, nAudit := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"), totalAudits(t, tx)
		ledgerRefusedRow(t, tx, "admin B (disabled) adjusts", "23514", ledgerWhy{Constraint: "admin_is_active", Message: "platform.ledger.acting_admin_id names admin " + ledgerAdminB},
			ledgerAdjustmentCols(a, "m1", 1).with("acting_admin_id", ledgerAdminB))
		if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger"); got != nLedger {
			t.Fatalf("the refused adjustment left %d ledger rows behind", got-nLedger)
		}
		if got := totalAudits(t, tx); got != nAudit {
			t.Fatalf("the refused adjustment left %d audit rows behind", got-nAudit)
		}
		ledgerOKRow(t, tx, "control: the same adjustment by the still-active admin A", ledgerAdjustmentCols(a, "m1", 1))
		if got := totalAudits(t, tx); got != nAudit+1 {
			t.Fatalf("the control adjustment wrote %d audit rows, want 1", got-nAudit)
		}
	})
	ledgerRequireViewsEmpty(t, tx, "after the adjustments")
}

// TestLedger_AdjustmentThroughTheRoles runs a clawback as the two roles the service really runs as. The ledger trigger that checks
// the admin and the trigger that writes the audit row run for the CALLER, so a role that cannot use the objects those triggers
// call fails here and not in production: a legal adjustment must pass for platform_app and platform_admin, write its audit row,
// and an overdraw or a blank reason must be refused with the database's reason (never with a permission error). It fails if either
// role cannot adjust, if the audit row is missing, or if a refusal comes from the wrong rule.
func TestLedger_AdjustmentThroughTheRoles(t *testing.T) {
	_, admin := migratedScratch(t)
	for _, role := range []string{"platform_app", "platform_admin"} {
		role := role
		t.Run(role, func(t *testing.T) {
			tx := ledgerBegin(t, admin)
			ledgerSeedFunded(t, tx)
			a := ledgerUserA
			ledgerSwitchRole(t, tx, role)
			nAudit := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admin_audit WHERE action = 'ledger.adjustment'")
			id := ledgerOKRow(t, tx, "control: "+role+" writes an adjustment of 25", ledgerAdjustmentCols(a, "m1", 25))
			ledgerRequireBalance(t, tx, "after "+role+"'s adjustment", a, "m1", 975, 0)
			if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admin_audit WHERE action = 'ledger.adjustment' AND target_id = $1 AND target_user_id = $2", fmt.Sprint(id), a); got != 1 {
				t.Fatalf("%d audit rows for %s's adjustment, want exactly 1", got, role)
			}
			if got := ledgerInt64(t, tx, "SELECT count(*) FROM platform.admin_audit WHERE action = 'ledger.adjustment'"); got != nAudit+1 {
				t.Fatalf("%s's adjustment wrote %d audit rows, want 1", role, got-nAudit)
			}
			ledgerRefusedRow(t, tx, role+" adjusting more than is available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"}, ledgerAdjustmentCols(a, "m1", 976))
			ledgerRefusedCheck(t, tx, role+" adjusting without a reason", "ledger_shape_adjustment", ledgerAdjustmentCols(a, "m1", 1).with("reason", "  "))
			ledgerRequireViewsEmpty(t, tx, "after "+role+"'s adjustments")
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// Immutability: the headline exit criterion of Phase 1 (chunk B6). The ledger, the price history, the price snapshot and the
// admin audit are append-only. Two lines of defence, tested separately:
//   1. privileges: platform_app and platform_admin hold no UPDATE, DELETE or TRUNCATE (SQLSTATE 42501);
//   2. triggers: row triggers for UPDATE/DELETE and a statement trigger for TRUNCATE raise 23001, and they stop the OWNER too.
// What the owner can still do (switch the trigger off, drop the table, re-grant itself) is demonstrated, not hidden.
// ---------------------------------------------------------------------------------------------------------------------

// ledgerLogTable describes one append-only table: statements that touch exactly one row nothing references, what the
// superuser control must do first so the SAME statements can succeed, and who may INSERT.
type ledgerLogTable struct {
	name          string
	update, del   string
	truncate      string          // what the app and admin roles and the control run
	ownerTruncate string          // what the owner runs: CASCADE where another table's foreign key would answer before the trigger can
	prep          []string        // the control's preparation: disable the append-only triggers (and drop a foreign key that blocks a TRUNCATE)
	insert        func() string   // a valid INSERT
	insertBy      map[string]bool // roles that hold INSERT on the table
}

func ledgerLogTables() []ledgerLogTable {
	return []ledgerLogTable{
		{
			name:          "ledger",
			update:        "UPDATE platform.ledger SET reason = 'tampered' WHERE idempotency_key = 'ao-grant-b'",
			del:           "DELETE FROM platform.ledger WHERE idempotency_key = 'ao-grant-b'",
			truncate:      "TRUNCATE platform.ledger CASCADE", // CASCADE since platform_0005: platform.reserves references the ledger, and a plain TRUNCATE of a referenced table is refused by the foreign key (0A000) before the BEFORE TRUNCATE trigger can answer
			ownerTruncate: "TRUNCATE platform.ledger CASCADE",
			prep:          []string{"ALTER TABLE platform.ledger DISABLE TRIGGER ledger_append_only", "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_no_truncate"},
			insert: func() string {
				return fmt.Sprintf("INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, acting_admin_id, reason, idempotency_key) VALUES ('%s', 'm1', 'grant', 1, '%s', 'insert control', '%s')",
					ledgerUserB, ledgerAdminA, ledgerNextKey("ao-insert"))
			},
			insertBy: map[string]bool{"platform_app": true, "platform_admin": true},
		},
		{
			name:          "job_unit_prices",
			update:        fmt.Sprintf("UPDATE platform.job_unit_prices SET cost = 9 WHERE job_id = '%s' AND unit = 'm1_cache'", ledgerJobA1),
			del:           fmt.Sprintf("DELETE FROM platform.job_unit_prices WHERE job_id = '%s' AND unit = 'm1_cache'", ledgerJobA1),
			truncate:      "TRUNCATE platform.job_unit_prices",
			ownerTruncate: "TRUNCATE platform.job_unit_prices CASCADE",
			prep: []string{
				"ALTER TABLE platform.job_unit_prices DISABLE TRIGGER job_unit_prices_append_only", "ALTER TABLE platform.job_unit_prices DISABLE TRIGGER job_unit_prices_no_truncate",
				"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_frozen_price_fk", // a TRUNCATE of a referenced table is refused by the foreign key before any trigger runs
			},
			insert: func() string {
				return fmt.Sprintf("INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) VALUES ('%s', 'm1', 'm1_own', 1)", ledgerJobA0)
			},
			insertBy: map[string]bool{"platform_app": true}, // the snapshot is written by the service at confirm; the break-glass role only reads
		},
		{
			name:          "unit_cost_history",
			update:        "UPDATE platform.unit_cost_history SET new_cost = 99 WHERE unit = 'm2_bounced'",
			del:           "DELETE FROM platform.unit_cost_history WHERE unit = 'm2_bounced'",
			truncate:      "TRUNCATE platform.unit_cost_history",
			ownerTruncate: "TRUNCATE platform.unit_cost_history",
			prep:          []string{"ALTER TABLE platform.unit_cost_history DISABLE TRIGGER unit_cost_history_append_only", "ALTER TABLE platform.unit_cost_history DISABLE TRIGGER unit_cost_history_no_truncate"},
			insert: func() string {
				return "INSERT INTO platform.unit_cost_history (unit, old_cost, new_cost, change_reason) VALUES ('m2_bounced', 1, 2, 'a direct insert')"
			},
			insertBy: map[string]bool{}, // nobody: the unit_costs trigger (SECURITY DEFINER) is the only writer
		},
		{
			name:          "admin_audit",
			update:        "UPDATE platform.admin_audit SET reason = 'tampered' WHERE action = 'seed-action'",
			del:           "DELETE FROM platform.admin_audit WHERE action = 'seed-action'",
			truncate:      "TRUNCATE platform.admin_audit",
			ownerTruncate: "TRUNCATE platform.admin_audit",
			prep:          []string{"ALTER TABLE platform.admin_audit DISABLE TRIGGER admin_audit_append_only", "ALTER TABLE platform.admin_audit DISABLE TRIGGER admin_audit_no_truncate"},
			insert: func() string {
				return fmt.Sprintf("INSERT INTO platform.admin_audit (admin_id, entry_type, action, target_type, target_id, reason) VALUES ('%s', 'action', '%s', 'user', 'x', 'insert control')",
					ledgerAdminA, ledgerNextKey("ao-insert"))
			},
			insertBy: map[string]bool{"platform_app": true, "platform_admin": true},
		},
	}
}

// ledgerSuperuserControl is the control of every refusal below: as the superuser, with the append-only triggers disabled (and
// whatever else would get in the way), the SAME statements succeed. So a refusal afterwards is the role's doing: not a typo,
// not an empty table, not a foreign key. The statements are valid and each touches rows that exist.
func ledgerSuperuserControl(t *testing.T, tx pgx.Tx, tb ledgerLogTable) {
	t.Helper()
	ctx := testCtx(t)
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		for _, p := range tb.prep {
			ledgerMust(t, sp, p)
		}
		for _, st := range []struct{ op, sql string }{{"UPDATE", tb.update}, {"DELETE", tb.del}} {
			tag, err := sp.Exec(ctx, st.sql)
			if err != nil {
				t.Fatalf("control: the superuser's %s on %s failed (%v), so a refusal of it would prove nothing\n  sql: %s", st.op, tb.name, err, st.sql)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("control: the %s on %s touched %d rows, want 1: the statement must hit a real row", st.op, tb.name, tag.RowsAffected())
			}
		}
		if _, err := sp.Exec(ctx, tb.truncate); err != nil {
			t.Fatalf("control: the superuser's TRUNCATE on %s failed (%v)", tb.name, err)
		}
		if n := ledgerInt64(t, sp, "SELECT count(*) FROM platform."+tb.name); n != 0 {
			t.Fatalf("control: after the superuser's TRUNCATE %s still holds %d rows", tb.name, n)
		}
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		if _, err := sp.Exec(ctx, tb.insert()); err != nil {
			t.Fatalf("control: the superuser's INSERT into %s failed (%v), so a role's refusal of it would prove nothing", tb.name, err)
		}
	})
}

// TestLedger_AppendOnly_PrivilegesRefuseMutation is the first line of defence, for the ledger and the three other append-only
// tables, for platform_app AND platform_admin: UPDATE, DELETE and TRUNCATE are refused with 42501 (permission denied for the
// table), and the paired control is that the role CAN do what it was granted: INSERT where the migration grants it (where it does
// not, that INSERT is refused with 42501 too, and the superuser's INSERT of the same statement works). It fails if a grant
// is wider than designed (the privilege check would pass and the trigger would answer 23001 instead of 42501), if the role
// switch does nothing (ledgerSwitchRole proves it bites), or if the table is empty (the superuser control touches real rows).
func TestLedger_AppendOnly_PrivilegesRefuseMutation(t *testing.T) {
	_, admin := migratedScratch(t)
	ledgerSeedCommitted(t, admin)
	for _, tb := range ledgerLogTables() {
		for _, role := range []string{"platform_app", "platform_admin"} {
			tb, role := tb, role
			t.Run(tb.name+"/"+role, func(t *testing.T) {
				tx := ledgerBegin(t, admin)
				ledgerSuperuserControl(t, tx, tb)
				ledgerSwitchRole(t, tx, role)
				if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform."+tb.name); n < 1 {
					t.Fatalf("as %s, SELECT sees %d rows in %s: the refusals below would prove nothing on an empty table", role, n, tb.name)
				}
				why := ledgerWhy{Message: "permission denied for table " + tb.name}
				for _, st := range []struct{ op, sql string }{{"UPDATE", tb.update}, {"DELETE", tb.del}, {"TRUNCATE", tb.truncate}} {
					ledgerExpectRefused(t, tx, fmt.Sprintf("%s %s on %s", role, st.op, tb.name), "42501", why, st.sql)
				}
				if tb.insertBy[role] {
					if tag := ledgerExpectOK(t, tx, fmt.Sprintf("control: %s INSERT into %s", role, tb.name), tb.insert()); tag.RowsAffected() != 1 {
						t.Fatalf("control: %s's INSERT into %s inserted %d rows", role, tb.name, tag.RowsAffected())
					}
				} else {
					ledgerExpectRefused(t, tx, fmt.Sprintf("%s INSERT into %s (it holds SELECT only)", role, tb.name), "42501", why, tb.insert())
				}
			})
		}
	}

	t.Run("unit_cost_history is written by the unit_costs trigger and by nothing else", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		before := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history")
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectOK(t, tx, "control: platform_app changes a cost through a named admin",
			"UPDATE platform.unit_costs SET cost = 7, changed_by = $1, change_reason = 'append-only control' WHERE unit = 'm2_bounced'", ledgerAdminA)
		ledgerResetRole(t, tx)
		if after := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_cost_history"); after != before+1 {
			t.Fatalf("the cost change wrote %d history rows, want exactly 1", after-before)
		}
		if got := ledgerString(t, tx, "SELECT old_cost || '>' || new_cost || ' ' || changed_by::text || ' ' || change_reason FROM platform.unit_cost_history ORDER BY id DESC LIMIT 1"); got != "1>7 "+ledgerAdminA+" append-only control" {
			t.Fatalf("the history row is %q, want the old cost, new cost, admin and reason of the change", got)
		}
	})
}

// TestLedger_AppendOnly_OwnerIsStoppedByTriggers is the second line of defence, and the honest one: platform_owner PASSES the
// privilege check for UPDATE, DELETE and TRUNCATE (it owns the tables), so any refusal here can only be the trigger, which
// raises 23001 (restrict_violation) with the table named. The paired control is that with the trigger disabled the same
// statement succeeds, so the trigger is the only barrier. It fails if a trigger is missing, is not FOR EACH ROW / STATEMENT as
// needed, or does not raise 23001.
func TestLedger_AppendOnly_OwnerIsStoppedByTriggers(t *testing.T) {
	_, admin := migratedScratch(t)
	ledgerSeedCommitted(t, admin)
	for _, tb := range ledgerLogTables() {
		tb := tb
		t.Run(tb.name, func(t *testing.T) {
			tx := ledgerBegin(t, admin)
			ledgerSwitchRole(t, tx, "platform_owner")
			for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
				if !ledgerBool(t, tx, "SELECT has_table_privilege(current_user::text, $1::text, $2::text)", "platform."+tb.name, priv) {
					t.Fatalf("platform_owner lacks %s on platform.%s: then the refusal below would be a privilege error, not the trigger", priv, tb.name)
				}
			}
			for _, st := range []struct{ op, sql string }{{"UPDATE", tb.update}, {"DELETE", tb.del}, {"TRUNCATE", tb.ownerTruncate}} {
				ledgerExpectRefused(t, tx, fmt.Sprintf("the owner's %s on %s", st.op, tb.name), "23001",
					ledgerWhy{Message: fmt.Sprintf("%s on platform.%s is not allowed: the table is append-only", st.op, tb.name)}, st.sql)
			}
			// The paired control: take the triggers away and the same statements go through, so the 23001 above was the trigger and nothing else.
			ledgerTrial(t, tx, func(sp pgx.Tx) {
				for _, p := range tb.prep {
					ledgerMust(t, sp, p)
				}
				if tag := ledgerMust(t, sp, tb.update); tag.RowsAffected() != 1 {
					t.Fatalf("control: with the trigger disabled the owner's UPDATE on %s touched %d rows, want 1", tb.name, tag.RowsAffected())
				}
				if tag := ledgerMust(t, sp, tb.del); tag.RowsAffected() != 1 {
					t.Fatalf("control: with the trigger disabled the owner's DELETE on %s touched %d rows, want 1", tb.name, tag.RowsAffected())
				}
			})
		})
	}

	// The triggers are not a rule about one role: even a superuser, who passes every privilege check, is stopped.
	t.Run("a superuser is stopped too", func(t *testing.T) {
		for _, tb := range ledgerLogTables() {
			tb := tb
			t.Run(tb.name, func(t *testing.T) {
				tx := ledgerBegin(t, admin)
				if !ledgerBool(t, tx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user") {
					t.Fatal("precondition: this connection must be a superuser")
				}
				for _, st := range []struct{ op, sql string }{{"UPDATE", tb.update}, {"DELETE", tb.del}, {"TRUNCATE", tb.ownerTruncate}} {
					ledgerExpectRefused(t, tx, fmt.Sprintf("the superuser's %s on %s", st.op, tb.name), "23001",
						ledgerWhy{Message: fmt.Sprintf("%s on platform.%s is not allowed: the table is append-only", st.op, tb.name)}, st.sql)
				}
			})
		}
	})

	t.Run("job_unit_prices TRUNCATE: the foreign key answers first, the trigger is the second line", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		ledgerSwitchRole(t, tx, "platform_owner")
		ledgerExpectRefused(t, tx, "the owner's TRUNCATE of the referenced snapshot table", "0A000",
			ledgerWhy{Message: "cannot truncate a table referenced in a foreign key constraint"}, "TRUNCATE platform.job_unit_prices")
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_frozen_price_fk")
			ledgerExpectRefused(t, sp, "with the foreign key gone the trigger itself answers", "23001",
				ledgerWhy{Message: "TRUNCATE on platform.job_unit_prices is not allowed: the table is append-only"}, "TRUNCATE platform.job_unit_prices")
		})
	})
}

// TestLedger_AppendOnly_OwnerResidualPower writes down, as a test, what the table owner can still do, because a claim of
// immutability that hides its exception is worse than none. Everything happens in a transaction that is rolled back, and the
// last step reads the committed ledger again and compares it with the fingerprint taken before: the ledger is intact. The
// compensating controls (platform_owner is NOLOGIN and used only by the migrator; the balance-drift gate shows a tamper that
// changes a delta) are asserted here too. It fails if the owner's power changes (a test that the exception is still exactly
// what the spec says), or if anything leaks out of the rolled-back transaction.
func TestLedger_AppendOnly_OwnerResidualPower(t *testing.T) {
	_, admin := migratedScratch(t)
	ledgerSeedCommitted(t, admin)
	tables := []string{"platform.ledger", "platform.job_unit_prices", "platform.unit_cost_history", "platform.admin_audit", "platform.balances", "platform.reserves"}
	before := map[string]string{}
	for _, tb := range tables {
		before[tb] = ledgerDigest(t, admin, tb)
	}
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM platform.ledger"); n < 4 {
		t.Fatalf("the seeded ledger holds %d rows, want at least 4: the intact-afterwards comparison needs real rows", n)
	}

	t.Run("the compensating control: platform_owner cannot log in", func(t *testing.T) {
		if ledgerBool(t, admin, "SELECT rolcanlogin FROM pg_roles WHERE rolname = 'platform_owner'") {
			t.Fatal("platform_owner can log in: the owner's residual power is then reachable by any connection holding its password")
		}
	})

	t.Run("the owner can switch the append-only trigger off and then rewrite a row; balance_drift shows a rewritten delta", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		ledgerSwitchRole(t, tx, "platform_owner")
		ledgerExpectRefused(t, tx, "with the trigger on, the owner's UPDATE is refused", "23001", ledgerWhy{Message: "append-only"},
			"UPDATE platform.ledger SET reason = 'tampered' WHERE idempotency_key = 'ao-grant-a'")
		ledgerExpectOK(t, tx, "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_append_only", "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_append_only")
		if tag := ledgerExpectOK(t, tx, "the same UPDATE now succeeds", "UPDATE platform.ledger SET reason = 'tampered' WHERE idempotency_key = 'ao-grant-a'"); tag.RowsAffected() != 1 {
			t.Fatalf("the owner's UPDATE touched %d rows, want 1", tag.RowsAffected())
		}
		t.Log("RESIDUAL POWER 1: platform_owner can ALTER TABLE platform.ledger DISABLE TRIGGER ledger_append_only, and after that an UPDATE of a ledger row succeeds. Nothing in the database stops it.")

		ledgerRequireViewsEmpty(t, tx, "after a tamper that changed only the reason (a pure text edit leaves every delta alone)")
		t.Log("NOT DETECTED: a rewrite that keeps every delta (here: the reason) leaves platform.balance_drift empty. The drift gate sees sums, not rows.")
		ledgerExpectOK(t, tx, "a tamper that changes a delta", "UPDATE platform.ledger SET available_delta = available_delta + 1 WHERE idempotency_key = 'ao-grant-a'")
		want := ledgerUserA + "|m1|80|81|17|17"
		if got := ledgerDrift(t, tx); len(got) != 1 || got[0] != want {
			t.Fatalf("platform.balance_drift after the owner changed a delta = %v, want [%s] (stored 80 vs ledger 81)", got, want)
		}
		t.Log("DETECTED: a rewrite that changes a delta shows in platform.balance_drift as stored 80 vs ledger 81, and scripts/check-balance-invariant.sh is red on it.")
	})

	t.Run("the owner can drop the table (run in a savepoint that is rolled back)", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		ledgerSwitchRole(t, tx, "platform_owner")
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "DROP TABLE platform.ledger CASCADE")
			if !ledgerBool(t, sp, "SELECT to_regclass('platform.ledger') IS NULL") {
				t.Fatal("DROP TABLE ran but platform.ledger still exists")
			}
			t.Log("RESIDUAL POWER 2: platform_owner can DROP TABLE platform.ledger CASCADE (the append-only triggers guard rows, not the table); it also drops the two views that read it.")
		})
		if !ledgerBool(t, tx, "SELECT to_regclass('platform.ledger') IS NOT NULL AND to_regclass('platform.balance_drift') IS NOT NULL") {
			t.Fatal("the savepoint rollback did not bring the ledger and its view back")
		}
	})

	t.Run("the owner can re-grant itself: the trigger still stops platform_app, and with the trigger also off the app rewrites the ledger", func(t *testing.T) {
		tx := ledgerBegin(t, admin)
		ledgerSwitchRole(t, tx, "platform_owner")
		ledgerExpectOK(t, tx, "GRANT UPDATE on the ledger to platform_app", "GRANT UPDATE ON platform.ledger TO platform_app")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectRefused(t, tx, "platform_app's UPDATE after the mistaken grant: the privilege passes, the trigger answers", "23001", ledgerWhy{Message: "append-only"},
			"UPDATE platform.ledger SET reason = 'tampered' WHERE idempotency_key = 'ao-grant-b'")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_owner")
		ledgerExpectOK(t, tx, "DISABLE TRIGGER", "ALTER TABLE platform.ledger DISABLE TRIGGER ledger_append_only")
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_app")
		if tag := ledgerExpectOK(t, tx, "platform_app's UPDATE with the grant AND the trigger gone", "UPDATE platform.ledger SET reason = 'tampered' WHERE idempotency_key = 'ao-grant-b'"); tag.RowsAffected() != 1 {
			t.Fatalf("the app's UPDATE touched %d rows, want 1", tag.RowsAffected())
		}
		t.Log("RESIDUAL POWER 3: platform_owner can GRANT UPDATE to another role; the trigger is then the only defence, and with the trigger disabled too the ledger is rewritable by that role. Both steps need the owner, which is NOLOGIN and used only by the migrator.")
	})

	t.Run("the other append-only tables have the same exception", func(t *testing.T) {
		for _, tb := range ledgerLogTables()[1:] {
			tb := tb
			t.Run(tb.name, func(t *testing.T) {
				tx := ledgerBegin(t, admin)
				ledgerSwitchRole(t, tx, "platform_owner")
				ledgerExpectOK(t, tx, "DISABLE TRIGGER "+tb.name+"_append_only", "ALTER TABLE platform."+tb.name+" DISABLE TRIGGER "+tb.name+"_append_only")
				if tag := ledgerExpectOK(t, tx, "the owner's UPDATE on "+tb.name+" after that", tb.update); tag.RowsAffected() != 1 {
					t.Fatalf("the owner's UPDATE on %s touched %d rows, want 1", tb.name, tag.RowsAffected())
				}
				t.Logf("RESIDUAL POWER 4 (%s): the owner can disable %s_append_only and then UPDATE.", tb.name, tb.name)
			})
		}
	})

	t.Run("afterwards: nothing leaked out of the rolled-back transactions", func(t *testing.T) {
		for _, tb := range tables {
			if got := ledgerDigest(t, admin, tb); got != before[tb] {
				t.Fatalf("%s changed: its fingerprint was %s and is now %s", tb, before[tb], got)
			}
		}
		ledgerRequireViewsEmpty(t, admin, "on the committed ledger after the owner's rolled-back tampering")
		for _, trg := range []string{"ledger_append_only", "ledger_no_truncate", "ledger_apply_to_balance", "ledger_check_reserve", "ledger_apply_to_reserves", "ledger_check_release", "ledger_apply_to_entitlements", "ledger_stamp_created_at"} {
			if st := ledgerString(t, admin, "SELECT tgenabled::text FROM pg_trigger WHERE tgrelid = 'platform.ledger'::regclass AND tgname = $1", trg); st != "O" {
				t.Fatalf("trigger %s is in state %q after the tests, want O (enabled)", trg, st)
			}
		}
		if ledgerBool(t, admin, "SELECT has_table_privilege('platform_app', 'platform.ledger', 'UPDATE')") {
			t.Fatal("platform_app holds UPDATE on the ledger after the rolled-back GRANT: the GRANT leaked out of its transaction")
		}
	})
}
