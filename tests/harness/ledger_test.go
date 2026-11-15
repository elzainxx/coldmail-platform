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

		c1 := ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 3, 1))
		step(t, "after charging 3 units at the frozen price 1 (m1_own)", "m1", 80, 17)
		c2 := ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_vendor", 2, 2))
		step(t, "after charging 2 units at the frozen price 2 (m1_vendor)", "m1", 80, 13)
		for id, want := range map[int64]string{c1: "m1_own|3|1|-3", c2: "m1_vendor|2|2|-4"} {
			if got := ledgerRows(t, tx, "SELECT unit, units, unit_cost, reserved_delta FROM platform.ledger WHERE id = $1", id); len(got) != 1 || got[0] != want {
				t.Fatalf("charge row %d must carry the unit, the frozen price and the consumed credits %s, got %v", id, want, got)
			}
		}

		ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, 13))
		step(t, "after releasing the remaining 13", "m1", 93, 0)

		// Settlement adds up: what was charged and released is exactly what was reserved.
		settled := ledgerInt64(t, tx, `SELECT COALESCE(sum(CASE WHEN kind = 'charge' THEN -reserved_delta ELSE available_delta END), 0)::bigint
			FROM platform.ledger WHERE reserve_id = $1`, reserve)
		if settled != 20 {
			t.Fatalf("charges plus releases of the reserve add up to %d, want exactly the 20 that were reserved", settled)
		}

		// Control: the balance check is wired. The reserve is fully settled, so one more charge would take reserved below zero and is refused.
		ledgerRefusedRow(t, tx, "a charge on a fully settled reserve", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative"},
			ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 1, 1))
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
		ledgerPut(t, tx, ledgerChargeCols(a, "m2", ledgerJobA2, reserve, "m2_accepted", 4, 1))
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
	refuse := func(t *testing.T, constraint string, cases []shapeCase) {
		t.Helper()
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) { ledgerRefusedCheck(t, tx, c.name, constraint, c.cols) })
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
		r := func() ledgerCols { return ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2) }
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
		})
	})

	base := ledgerOKRow(t, tx, "the reserve the settlement rows point at", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 2))

	t.Run("charge", func(t *testing.T) {
		c := func() ledgerCols { return ledgerChargeCols(a, "m1", ledgerJobA1, base, "m1_own", 3, 1) }
		ledgerOKRow(t, tx, "control: the legal charge (3 units x 1)", c())
		refuse(t, "ledger_shape_charge", []shapeCase{
			{"moves available credits", c().with("available_delta", int64(1))},
			{"consumes nothing", c().with("reserved_delta", int64(0))},
			{"adds to reserved", c().with("reserved_delta", int64(3))},
			{"the arithmetic is wrong: 3 x 1 is not 4", c().with("reserved_delta", int64(-4))},
			{"no job", c().with("job_id", nil)},
			{"no reserve it settles (NULL)", c().with("reserve_id", nil)},
			{"no unit (NULL)", c().with("unit", nil)},
			{"no units (NULL)", c().with("units", nil)},
			{"no unit cost (NULL)", c().with("unit_cost", nil)},
			{"zero units", c().with("units", int64(0), "reserved_delta", int64(0))},
		})
	})

	t.Run("release", func(t *testing.T) {
		r := func() ledgerCols { return ledgerReleaseCols(a, "m1", ledgerJobA1, base, 5) }
		ledgerOKRow(t, tx, "control: the legal release of 5", r())
		refuse(t, "ledger_shape_release", []shapeCase{
			{"releases nothing", r().with("available_delta", int64(0), "reserved_delta", int64(0))},
			{"a negative release", r().with("available_delta", int64(-5), "reserved_delta", int64(5))},
			{"available and reserved do not mirror", r().with("reserved_delta", int64(-4))},
			{"no job", r().with("job_id", nil)},
			{"no reserve it settles (NULL)", r().with("reserve_id", nil)},
			{"carries a unit", r().with("unit", "m1_own")},
			{"carries units", r().with("units", int64(5))},
			{"carries a unit cost", r().with("unit_cost", int64(1))},
		})
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
		ledgerOKRow(t, tx, "m3_expire by the system (no admin)", ledgerM3Cols("m3_expire", a))
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
		if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.ledger WHERE kind LIKE 'm3_%' AND NOT monetary"); n != 3 {
			t.Fatalf("%d m3 events are in the ledger and flagged non-monetary, want the 3 legal controls", n)
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
			ledgerReserveCols(a, "m1", ledgerJobA1, "m1_own", 1, 1).with("idempotency_key", "retry-me"))
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
	reserveB := ledgerOKRow(t, tx, "B's reserve on B's own job (the control for the job key)", ledgerReserveCols(b, "m1", ledgerJobB1, "m1_own", 5, 1))
	chargeA := ledgerOKRow(t, tx, "A's charge", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 1))
	grantA := ledgerInt64(t, tx, "SELECT id FROM platform.ledger WHERE user_id = $1 AND kind = 'grant' AND module_id = 'm1'", a)

	t.Run("ledger_job_fk: a job's rows belong to the job's own user and module", func(t *testing.T) {
		ledgerRefusedFK(t, tx, "B reserving against A's job", "ledger_job_fk", ledgerReserveCols(b, "m1", ledgerJobA1, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a release recorded on module m2 against an m1 job", "ledger_job_fk", ledgerReleaseCols(a, "m2", ledgerJobA1, reserveA, 5))
		ledgerOKRow(t, tx, "control: A releasing on A's own job and module", ledgerReleaseCols(a, "m1", ledgerJobA1, reserveA, 5))
	})

	t.Run("ledger_reserve_fk: a charge or release points at a RESERVE row of the same user and job", func(t *testing.T) {
		ledgerRefusedFK(t, tx, "B's charge pointing at A's reserve", "ledger_reserve_fk", ledgerChargeCols(b, "m1", ledgerJobB1, reserveA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "A's charge on job A1b pointing at the reserve of job A1", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1b, reserveA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a charge pointing at a grant row", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1, grantA, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a charge pointing at nothing", "ledger_reserve_fk", ledgerChargeCols(a, "m1", ledgerJobA1, 999999999, "m1_own", 1, 1))
		ledgerRefusedFK(t, tx, "a release pointing at a charge row", "ledger_reserve_fk", ledgerReleaseCols(a, "m1", ledgerJobA1, chargeA, 1))
		ledgerRefusedFK(t, tx, "B's release pointing at A's reserve", "ledger_reserve_fk", ledgerReleaseCols(b, "m1", ledgerJobB1, reserveA, 1))
		ledgerOKRow(t, tx, "control: A's charge at A's reserve", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 1))
		ledgerOKRow(t, tx, "control: B's charge at B's reserve", ledgerChargeCols(b, "m1", ledgerJobB1, reserveB, "m1_own", 1, 1))
		ledgerOKRow(t, tx, "control: B's release at B's reserve", ledgerReleaseCols(b, "m1", ledgerJobB1, reserveB, 1))
	})

	t.Run("ledger_frozen_price_fk: the unit and its price must be in the job's snapshot", func(t *testing.T) {
		fk := func(what string, c ledgerCols) { ledgerRefusedFK(t, tx, what, "ledger_frozen_price_fk", c) }
		fk("a reserve at price 3, which the snapshot does not hold", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_vendor", 10, 3))
		fk("a reserve of a unit of another module", ledgerReserveCols(a, "m1", ledgerJobA1, "m2_accepted", 10, 1))
		fk("a reserve on a job that has no snapshot at all", ledgerReserveCols(a, "m1", ledgerJobA0, "m1_own", 1, 1))
		fk("a reserve of a unit that does not exist", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_bogus", 1, 1))
		fk("the right unit at the price of another unit (m1_cache costs 1, not 2)", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_cache", 5, 2))
		fk("a charge of m1_own at 2, which is the vendor's price", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_own", 1, 2))
		ledgerOKRow(t, tx, "control: a reserve at the snapshot price of m1_cache", ledgerReserveCols(a, "m1", ledgerJobA1, "m1_cache", 5, 1))
		ledgerOKRow(t, tx, "control: a charge at the snapshot price of m1_vendor", ledgerChargeCols(a, "m1", ledgerJobA1, reserveA, "m1_vendor", 1, 2))
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
		reserves[i] = ledgerPut(t, tx, ledgerReserveCols(a, "m1", jobs[i], "m1_own", 10, p))
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
		// Every job consumed exactly one unit at its own price: 1 + 5 + 9 credits out of the 10 x (1 + 5 + 9) reserved.
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
		ledgerRefusedRow(t, tx, "one more credit when none is available", "23514", ledgerWhy{Constraint: "balances_available_not_negative", Table: "balances"},
			ledgerReserveCols(a, "m1", ledgerJobA1, "m1_cache", 1, 1))

		t.Run("reserved cannot go below zero", func(t *testing.T) {
			ledgerRefusedRow(t, tx, "charging 101 units against 100 reserved credits", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative", Table: "balances"},
				ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_cache", 101, 1))
			ledgerRequireBalance(t, tx, "after the refused charge", a, "m1", 0, 100)
			ledgerOKRow(t, tx, "control: charging exactly the 100 reserved", ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_cache", 100, 1))
			ledgerRequireBalance(t, tx, "after settling everything", a, "m1", 0, 0)
		})
	})

	t.Run("a release larger than the reserve is refused the same way", func(t *testing.T) {
		ledgerFund(t, tx, a, "m1", 20)
		reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1b, "m1_own", 10, 1))
		ledgerRequireBalance(t, tx, "after the second reserve", a, "m1", 10, 10)
		ledgerRefusedRow(t, tx, "releasing 11 credits of a reserve of 10", "23514", ledgerWhy{Constraint: "balances_reserved_not_negative", Table: "balances"},
			ledgerReleaseCols(a, "m1", ledgerJobA1b, reserve, 11))
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
	reserve := ledgerPut(t, tx, ledgerReserveCols(a, "m1", ledgerJobA1, "m1_own", 5, 1))
	charge := ledgerPut(t, tx, ledgerChargeCols(a, "m1", ledgerJobA1, reserve, "m1_own", 2, 1))
	release := ledgerPut(t, tx, ledgerReleaseCols(a, "m1", ledgerJobA1, reserve, 3))
	enable := ledgerPut(t, tx, ledgerM3Cols("m3_enable", a))
	disable := ledgerPut(t, tx, ledgerM3Cols("m3_disable", a))
	expire := ledgerPut(t, tx, ledgerM3Cols("m3_expire", a))

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
		ids := []int64{grant, reserve, charge, release, enable, disable, expire}
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("ledger ids %v are not strictly increasing", ids)
			}
		}
	})
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
			truncate:      "TRUNCATE platform.ledger",
			ownerTruncate: "TRUNCATE platform.ledger",
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
	tables := []string{"platform.ledger", "platform.job_unit_prices", "platform.unit_cost_history", "platform.admin_audit", "platform.balances"}
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
		for _, trg := range []string{"ledger_append_only", "ledger_no_truncate", "ledger_apply_to_balance"} {
			if st := ledgerString(t, admin, "SELECT tgenabled::text FROM pg_trigger WHERE tgrelid = 'platform.ledger'::regclass AND tgname = $1", trg); st != "O" {
				t.Fatalf("trigger %s is in state %q after the tests, want O (enabled)", trg, st)
			}
		}
		if ledgerBool(t, admin, "SELECT has_table_privilege('platform_app', 'platform.ledger', 'UPDATE')") {
			t.Fatal("platform_app holds UPDATE on the ledger after the rolled-back GRANT: the GRANT leaked out of its transaction")
		}
	})
}
