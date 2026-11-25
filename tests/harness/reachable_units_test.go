//go:build harness

package harness

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// The database half of chunk B3 (platform_0013, Docs/phase-4-spec.md Locked 23): a reserve is whole, dearest and once over the units a job can actually REACH. platform.unit_costs.reachable says which units those
// are (module 1's vendor unit ships unreachable: the vendor fallback ships disabled), the price snapshot of a job holds exactly the reachable units, the reserve rate is the dearest of them, and the flag is
// written only by platform.set_unit_reachable. These tests use RAW SQL for the snapshot and the reserve (the Go store is not what they test), and they start from the SEEDED state: ledgerSeed switches the vendor
// fallback on for the tests written before platform_0013, so reachSetup switches it off again through the one writer.

// reachSetup is a seeded transaction (rolled back when the test ends) in which module 1's vendor unit is unreachable again, as the migration seeds it.
func reachSetup(t *testing.T) pgx.Tx {
	t.Helper()
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	ledgerMust(t, tx, "SELECT platform.set_unit_reachable('m1_vendor', false, 'test: back to the state the migration seeds')")
	if !ledgerBool(t, tx, "SELECT NOT reachable FROM platform.unit_costs WHERE unit = 'm1_vendor'") {
		t.Fatal("the vendor unit is still reachable after the writer switched it off")
	}
	return tx
}

// TestReachableUnits_TheSeededState: the migration seeds m1_vendor unreachable (with the reason and the time) and every other unit reachable with no source; the history says the same, one row per unit, the
// vendor's with the seed's reason. The flag is NOT NULL, a source is never blank (NULL is allowed: no source stated), and the history refuses a blank or NULL source, a NULL flag and an unknown unit.
// The CHECK on unit_costs and the history's own CHECKs each have their legal twin; the history is append-only for everybody (23001 for the superuser and the owner, 42501 for the roles that only read it).
// It fails if a unit that a job cannot be charged is reachable, or the reverse, in the state a database starts in.
func TestReachableUnits_TheSeededState(t *testing.T) {
	_, admin := migratedScratch(t) // NOT seeded: the state the migration leaves
	tx := ledgerBegin(t, admin)
	want := []string{"m1_cache|true|-|true", "m1_own|true|-|true", "m1_vendor|false|seed: the vendor fallback ships disabled|true", "m2_accepted|true|-|true", "m2_bounced|true|-|true"}
	got := ledgerRows(t, tx, "SELECT unit, reachable::text, COALESCE(reachable_source, '-'), (reachable_changed_at IS NOT NULL OR reachable_source IS NULL)::text FROM platform.unit_costs ORDER BY unit")
	if len(got) != len(want) {
		t.Fatalf("platform.unit_costs says %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unit %d of platform.unit_costs reads %q, want %q", i, got[i], want[i])
		}
	}
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_costs WHERE reachable_source IS NULL AND reachable_changed_at IS NOT NULL"); n != 0 {
		t.Fatalf("%d unit(s) have a change time and no source", n)
	}
	hist := ledgerRows(t, tx, "SELECT unit, reachable::text, source FROM platform.unit_reachability_history ORDER BY unit, id")
	wantHist := []string{"m1_cache|true|seed: reachable by default", "m1_own|true|seed: reachable by default", "m1_vendor|false|seed: the vendor fallback ships disabled", "m2_accepted|true|seed: reachable by default", "m2_bounced|true|seed: reachable by default"}
	if len(hist) != len(wantHist) {
		t.Fatalf("the reachability history is %v, want exactly the seed's one row per unit %v", hist, wantHist)
	}
	for i := range wantHist {
		if hist[i] != wantHist[i] {
			t.Fatalf("history row %d reads %q, want %q", i, hist[i], wantHist[i])
		}
	}

	t.Run("the flag and its source", func(t *testing.T) {
		const blank = "platform.unit_costs"
		ledgerExpectRefused(t, tx, "a NULL flag", "23502", ledgerWhy{Column: "reachable"}, "UPDATE "+blank+" SET reachable = NULL WHERE unit = 'm1_own'")
		ledgerExpectRefused(t, tx, "a blank source", "23514", ledgerWhy{Constraint: "unit_costs_reachable_source_not_blank"}, "UPDATE "+blank+" SET reachable_source = '   ' WHERE unit = 'm1_own'")
		ledgerExpectOK(t, tx, "control: a source", "UPDATE "+blank+" SET reachable_source = 'by hand' WHERE unit = 'm1_own'")
		ledgerExpectOK(t, tx, "control: no source (NULL is not blank: none was stated)", "UPDATE "+blank+" SET reachable_source = NULL WHERE unit = 'm1_cache'")
	})

	t.Run("a direct change of the flag writes its history by trigger, and only a change does", func(t *testing.T) {
		count := func() int64 {
			return ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_reachability_history WHERE unit = 'm2_bounced'")
		}
		before := count()
		ledgerExpectOK(t, tx, "a write that leaves the flag alone", "UPDATE platform.unit_costs SET reachable = reachable, description = description WHERE unit = 'm2_bounced'")
		ledgerExpectOK(t, tx, "a write of the source alone", "UPDATE platform.unit_costs SET reachable_source = 'a note' WHERE unit = 'm2_bounced'")
		if n := count(); n != before {
			t.Fatalf("a write that did not change the flag wrote %d history row(s)", n-before)
		}
		ledgerExpectOK(t, tx, "the owner flips the flag", "UPDATE platform.unit_costs SET reachable = false, reachable_source = 'stated with the flip' WHERE unit = 'm2_bounced'")
		if n := count(); n != before+1 {
			t.Fatalf("a change of the flag wrote %d history row(s), want 1", n-before)
		}
		if got := ledgerString(t, tx, "SELECT reachable::text || '|' || source FROM platform.unit_reachability_history WHERE unit = 'm2_bounced' ORDER BY id DESC LIMIT 1"); got != "false|stated with the flip" {
			t.Fatalf("the newest history row of the unit reads %q, want the flip and its source", got)
		}
		ledgerExpectOK(t, tx, "a flip that states no source", "UPDATE platform.unit_costs SET reachable = true, reachable_source = NULL WHERE unit = 'm2_bounced'")
		if got := ledgerString(t, tx, "SELECT source FROM platform.unit_reachability_history WHERE unit = 'm2_bounced' ORDER BY id DESC LIMIT 1"); got != "unspecified" {
			t.Fatalf("a flip with no source is recorded as %q, want unspecified (a history row always says who said so)", got)
		}
	})

	t.Run("the history table", func(t *testing.T) {
		const ins = "INSERT INTO platform.unit_reachability_history (unit, reachable, source) VALUES ($1, $2, $3)"
		ledgerExpectOK(t, tx, "control: a history row", ins, "m1_own", true, "a control")
		ledgerExpectRefused(t, tx, "a blank source", "23514", ledgerWhy{Constraint: "unit_reachability_history_source_not_blank"}, ins, "m1_own", true, "   ")
		ledgerExpectRefused(t, tx, "a NULL source", "23502", ledgerWhy{Column: "source"}, ins, "m1_own", true, nil)
		ledgerExpectRefused(t, tx, "a NULL flag", "23502", ledgerWhy{Column: "reachable"}, ins, "m1_own", nil, "a control")
		ledgerExpectRefused(t, tx, "a NULL unit", "23502", ledgerWhy{Column: "unit"}, ins, nil, true, "a control")
		ledgerExpectRefused(t, tx, "a unit that does not exist", "23503", ledgerWhy{Constraint: "unit_reachability_history_unit_fkey"}, ins, "m9_nothing", true, "a control")
		mutations := []string{"UPDATE platform.unit_reachability_history SET source = 'edited'", "DELETE FROM platform.unit_reachability_history", "TRUNCATE platform.unit_reachability_history"}
		for _, role := range []string{"", "platform_owner"} {
			if role != "" {
				ledgerSwitchRole(t, tx, role)
			}
			for _, m := range mutations {
				ledgerExpectRefused(t, tx, role+": "+m, "23001", ledgerWhy{Message: "append-only"}, m)
			}
			if role != "" {
				ledgerResetRole(t, tx)
			}
		}
		for _, role := range []string{"platform_app", "platform_admin"} {
			ledgerSwitchRole(t, tx, role)
			if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_reachability_history"); n < 5 {
				t.Fatalf("%s reads %d history rows", role, n)
			}
			for _, m := range append(mutations, "INSERT INTO platform.unit_reachability_history (unit, reachable, source) VALUES ('m1_own', true, 'x')") {
				ledgerExpectRefused(t, tx, role+": "+m, "42501", ledgerWhy{Message: "permission denied for table unit_reachability_history"}, m)
			}
			ledgerResetRole(t, tx)
		}
	})
}

// TestReachableUnits_SnapshotAndReserve: with the vendor unit unreachable a snapshot row for it is refused (23514, snapshot_only_reachable_units; so is the copy of EVERY unit of the module that the store used
// to run), a snapshot of the two reachable units is the complete one and a reserve over it is accepted at the dearest of THOSE (1) and refused at the vendor's 2 (reserve_at_the_dearest_price); a snapshot that lacks
// a reachable unit, or is empty, is refused (reserve_needs_the_complete_snapshot); a charge for the vendor unit cannot be written for such a job (the composite foreign key to the snapshot: ledger_frozen_price_fk)
// while a charge for a unit it holds can. With the vendor switched ON through set_unit_reachable the complete snapshot needs it again: the two-row snapshot is now incomplete, the three-row one is reserved at 2
// and refused at 1, and the job reserved earlier keeps its two-row snapshot and its reserve (a change of the flag moves no existing job). A job frozen while the vendor was on can still be charged for it after it is
// switched off again (settlement uses the frozen snapshot).
// It fails if the reserve rate or the complete snapshot is computed over units a job cannot reach, or over a flag that has changed under a job.
func TestReachableUnits_SnapshotAndReserve(t *testing.T) {
	tx := reachSetup(t)
	a := ledgerUserA
	ledgerFund(t, tx, a, "m1", 1000)
	next := jobIDs("d1")
	const snap = "INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.unit = ANY($2::text[])"
	const snapEveryUnit = "INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = 'm1'"
	const snapReachable = snapEveryUnit + " AND u.reachable"
	newJob := func() string { return ledgerNewJob(t, tx, next(), a, "m1", false) }
	only := ledgerWhy{Constraint: "snapshot_only_reachable_units"}

	j1 := newJob()
	ledgerExpectRefused(t, tx, "a snapshot row for the unreachable vendor unit", "23514", only, snap, j1, []string{"m1_vendor"})
	ledgerExpectRefused(t, tx, "the copy of every unit of the module (what the store ran before platform_0013)", "23514", only, snapEveryUnit, j1)
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", j1); n != 0 {
		t.Fatalf("a refused snapshot left %d row(s)", n)
	}
	ledgerExpectOK(t, tx, "control: the copy of the REACHABLE units", snapReachable, j1)
	if got := ledgerRows(t, tx, "SELECT unit FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", j1); len(got) != 2 || got[0] != "m1_cache" || got[1] != "m1_own" {
		t.Fatalf("the snapshot of the reachable units holds %v, want m1_cache and m1_own", got)
	}
	ledgerRefusedCheck(t, tx, "a reserve at the vendor's price over a snapshot that cannot hold the vendor", "reserve_at_the_dearest_price", ledgerReserveCols(a, "m1", j1, "m1_own", 10, 2))
	reserve := ledgerOKRow(t, tx, "control: a reserve at the dearest REACHABLE price", ledgerReserveCols(a, "m1", j1, "m1_own", 10, 1))
	ledgerRefusedFK(t, tx, "a charge for the vendor unit of a job whose snapshot has none", "ledger_frozen_price_fk", ledgerChargeCols(a, "m1", j1, reserve, "m1_vendor", 1, 2))
	ledgerOKRow(t, tx, "control: a charge for a unit the snapshot holds", ledgerChargeCols(a, "m1", j1, reserve, "m1_own", 1, 1))

	j2 := newJob()
	ledgerRefusedCheck(t, tx, "a reserve over an EMPTY snapshot", "reserve_needs_the_complete_snapshot", ledgerReserveCols(a, "m1", j2, "m1_own", 5, 1))
	ledgerExpectOK(t, tx, "a snapshot with one of the two reachable units", snap, j2, []string{"m1_own"})
	ledgerRefusedCheck(t, tx, "a reserve over a snapshot that lacks a reachable unit", "reserve_needs_the_complete_snapshot", ledgerReserveCols(a, "m1", j2, "m1_own", 5, 1))
	ledgerExpectOK(t, tx, "the other reachable unit joins it", snap, j2, []string{"m1_cache"})
	ledgerOKRow(t, tx, "control: the reserve over the complete reachable snapshot", ledgerReserveCols(a, "m1", j2, "m1_own", 5, 1))

	// the vendor fallback is switched ON through the one writer
	if !ledgerBool(t, tx, "SELECT platform.set_unit_reachable('m1_vendor', true, 'test: the owner switched the fallback on')") {
		t.Fatal("switching the vendor on reported no change")
	}
	j3, j4 := newJob(), newJob()
	ledgerExpectOK(t, tx, "a snapshot of the two units that were reachable when it was taken", snap, j3, []string{"m1_own", "m1_cache"})
	ledgerRefusedCheck(t, tx, "a reserve over a snapshot taken before the vendor was switched on (it lacks a reachable unit now)", "reserve_needs_the_complete_snapshot", ledgerReserveCols(a, "m1", j3, "m1_own", 5, 1))
	ledgerExpectOK(t, tx, "control: the vendor joins the snapshot", snap, j3, []string{"m1_vendor"})
	ledgerExpectOK(t, tx, "a snapshot of every reachable unit", snapReachable, j4)
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", j4); n != 3 {
		t.Fatalf("the snapshot taken with the vendor on holds %d rows, want 3", n)
	}
	ledgerRefusedCheck(t, tx, "a reserve at 1 over a snapshot that holds the vendor", "reserve_at_the_dearest_price", ledgerReserveCols(a, "m1", j4, "m1_own", 5, 1))
	reserve4 := ledgerOKRow(t, tx, "control: a reserve at the vendor price, the dearest reachable one", ledgerReserveCols(a, "m1", j4, "m1_vendor", 5, 2))
	if n := ledgerInt64(t, tx, "SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", j1); n != 2 {
		t.Fatalf("the job reserved before the flip has %d snapshot rows, want its 2: a change of the flag moves no existing job", n)
	}

	// and OFF again: a job frozen while it was on keeps the unit it holds and may be charged for it
	ledgerMust(t, tx, "SELECT platform.set_unit_reachable('m1_vendor', false, 'test: switched off again')")
	ledgerOKRow(t, tx, "control: a charge for the vendor unit of a job frozen while the vendor was on (settlement reads the frozen snapshot)", ledgerChargeCols(a, "m1", j4, reserve4, "m1_vendor", 1, 2))
}

// TestReachableUnits_SetUnitReachable: platform.set_unit_reachable(unit, reachable, source) returns true when it changed the flag and false when it already held (nothing written, no history row); each change writes
// exactly one history row with the source, and sets reachable_source and reachable_changed_at (the transaction's now()); a NULL flag, a NULL source and a blank source are refused (22023) and so is an unknown
// unit, a NULL unit included (P0002), each leaving the flag and the history as they were. platform_app can EXECUTE it - and flip the flag through it although it holds no UPDATE on the column (a SECURITY
// DEFINER) - and cannot write the flag itself (42501); platform_admin cannot execute it (42501).
// It fails if the flag can be written any other way, a change leaves no history, or a refusal changes anything.
func TestReachableUnits_SetUnitReachable(t *testing.T) {
	tx := reachSetup(t)
	const call = "SELECT platform.set_unit_reachable($1::text, $2::boolean, $3::text)"
	hist := func() int64 { return ledgerInt64(t, tx, "SELECT count(*) FROM platform.unit_reachability_history") }
	flag := func(unit string) string {
		return ledgerString(t, tx, "SELECT reachable::text FROM platform.unit_costs WHERE unit = $1", unit)
	}

	before := hist()
	if ledgerBool(t, tx, call, "m1_vendor", false, "test: already off") {
		t.Fatal("switching off a unit that is off reported a change")
	}
	if hist() != before {
		t.Fatal("a call that changed nothing wrote a history row")
	}
	if !ledgerBool(t, tx, call, "m1_vendor", true, "test: switched on") {
		t.Fatal("switching the vendor on reported no change")
	}
	if hist() != before+1 || flag("m1_vendor") != "true" {
		t.Fatalf("after one change the history grew by %d and the flag reads %s", hist()-before, flag("m1_vendor"))
	}
	if got := ledgerString(t, tx, "SELECT source || '|' || (changed_at = now())::text FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id DESC LIMIT 1"); got != "test: switched on|true" {
		t.Fatalf("the history row of the change reads %q, want its source and the transaction's time", got)
	}
	if got := ledgerString(t, tx, "SELECT reachable_source || '|' || (reachable_changed_at = now())::text FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != "test: switched on|true" {
		t.Fatalf("the unit reads %q, want the source and the time of the change", got)
	}
	if ledgerBool(t, tx, call, "m1_vendor", true, "test: already on") || hist() != before+1 {
		t.Fatal("a second call with the same flag reported a change or wrote a history row")
	}

	now := hist()
	for _, c := range []struct {
		what string
		args []any
	}{
		{"a NULL flag", []any{"m1_own", nil, "why"}}, {"a NULL source", []any{"m1_own", false, nil}}, {"a blank source", []any{"m1_own", false, "   "}}, {"an empty source", []any{"m1_own", false, ""}},
	} {
		ledgerExpectRefused(t, tx, c.what, "22023", ledgerWhy{Message: "a unit is reachable or not"}, call, c.args...)
	}
	ledgerExpectRefused(t, tx, "a unit that does not exist", "P0002", ledgerWhy{Message: "no unit"}, call, "m9_nothing", false, "why")
	ledgerExpectRefused(t, tx, "a NULL unit", "P0002", ledgerWhy{Message: "no unit"}, call, nil, false, "why")
	if hist() != now || flag("m1_own") != "true" {
		t.Fatalf("the refused calls left the history at %d (was %d) and m1_own reading %s", hist(), now, flag("m1_own"))
	}
	ledgerExpectOK(t, tx, "control: a legal call", call, "m1_own", false, "why")
	if flag("m1_own") != "false" {
		t.Fatal("a legal call did not change the flag")
	}
	ledgerExpectOK(t, tx, "the unit goes back", call, "m1_own", true, "why again")

	t.Run("who may call it and who may write the flag", func(t *testing.T) {
		ledgerSwitchRole(t, tx, "platform_app")
		ledgerExpectRefused(t, tx, "platform_app writes the flag itself", "42501", ledgerWhy{Message: "permission denied for table unit_costs"}, "UPDATE platform.unit_costs SET reachable = false WHERE unit = 'm1_own'")
		ledgerExpectRefused(t, tx, "platform_app writes the source itself", "42501", ledgerWhy{Message: "permission denied for table unit_costs"}, "UPDATE platform.unit_costs SET reachable_source = 'x' WHERE unit = 'm1_own'")
		ledgerExpectOK(t, tx, "control: platform_app flips the flag through the function (a SECURITY DEFINER)", call, "m1_own", false, "the service")
		if got := ledgerString(t, tx, "SELECT source FROM platform.unit_reachability_history WHERE unit = 'm1_own' ORDER BY id DESC LIMIT 1"); got != "the service" {
			t.Fatalf("the history row of platform_app's change reads %q", got)
		}
		ledgerResetRole(t, tx)
		ledgerSwitchRole(t, tx, "platform_admin")
		ledgerExpectRefused(t, tx, "platform_admin calls the writer", "42501", ledgerWhy{Message: "permission denied for function set_unit_reachable"}, call, "m1_own", true, "admin")
		ledgerResetRole(t, tx)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			ledgerMust(t, sp, "CREATE ROLE zz_reach_holds_nothing NOLOGIN")
			ledgerMust(t, sp, "GRANT USAGE ON SCHEMA platform TO zz_reach_holds_nothing")
			ledgerSwitchRole(t, sp, "zz_reach_holds_nothing")
			ledgerExpectRefused(t, sp, "a role that holds nothing calls the writer", "42501", ledgerWhy{Message: "permission denied for function set_unit_reachable"}, call, "m1_own", true, "nobody")
		})
		if flag("m1_own") != "false" {
			t.Fatalf("m1_own reads %s: a refused caller changed the flag", flag("m1_own"))
		}
	})
}
