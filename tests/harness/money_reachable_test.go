//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/money"
)

// The Go half of chunk B3 (Docs/phase-4-spec.md, Locked 23): the money store against platform_0013's reachable units, on a REAL migrated database as a real platform_app login. Module 1's vendor unit (m1_vendor,
// 2 credits) can be charged only when the owner has switched the vendor fallback on, so a reserve freezes the REACHABLE units of the module and holds the dearest of THOSE: 1 credit per row with the vendor off, 2 with it
// on; a job's snapshot is frozen at its reserve, so a flip moves no job that already reserved; and a vendor answer in a job whose snapshot has no vendor price is flagged and never charged.
//
// newMnyEnv switches the vendor ON for the tests written before platform_0013 (money_helpers_test.go). These tests do not rely on that: each sets the flag it needs through the one writer, at the point it
// needs it, and puts back what it found when it ends. The database half (the constraint, the trigger, the writer's own refusals) is in reachable_units_test.go, which uses raw SQL; what is proved here is the store.

// mrcVendorSource is the "who said so" of the flips these tests make.
const mrcVendorSource = "money reachable test"

// mrcRemember records the state of module 1's vendor unit and puts it back when the test ends (a flip of the shared fixture is not left behind; the scratch database is dropped anyway, so this is for the
// reader as much as for the run).
func (e *mnyEnv) mrcRemember() {
	e.t.Helper()
	found := e.str("SELECT reachable::text FROM platform.unit_costs WHERE unit = 'm1_vendor'") == "true"
	e.t.Cleanup(func() {
		if _, err := e.admin.Exec(context.Background(), "SELECT platform.set_unit_reachable('m1_vendor', $1, 'money reachable test: what the test found')", found); err != nil {
			e.t.Errorf("putting the vendor flag back to %v: %v", found, err)
		}
	})
}

// mrcVendor sets the vendor flag through the one writer on the superuser connection and fails the test unless the flag then reads as asked.
func (e *mnyEnv) mrcVendor(on bool) {
	e.t.Helper()
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', $1, $2)", on, mrcVendorSource)
	if got := e.str("SELECT reachable::text FROM platform.unit_costs WHERE unit = 'm1_vendor'"); (got == "true") != on {
		e.t.Fatalf("the vendor flag reads %s after switching it to %v", got, on)
	}
}

// mrcState is a fingerprint of the reachability of every unit and of its history: two equal states mean no flag moved and no history row was written between them.
func (e *mnyEnv) mrcState() string {
	e.t.Helper()
	return e.str(`SELECT (SELECT count(*) FROM platform.unit_reachability_history)::text || '/' || (SELECT string_agg(unit || ':' || reachable::text, ',' ORDER BY unit) FROM platform.unit_costs)`)
}

// mrcFrozenUnits is the units of the job's price snapshot, sorted.
func (e *mnyEnv) mrcFrozenUnits(job string) string {
	e.t.Helper()
	return strings.Join(e.rows("SELECT unit FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job), ",")
}

// mrcRequireReserve asserts a ReserveResult and the ledger row behind it.
func (e *mnyEnv) mrcRequireReserve(what string, got money.ReserveResult, unit string, cost int, units int, prices map[string]int) {
	e.t.Helper()
	if got.Unit != unit || got.UnitCost != cost || got.Units != units || got.Reserved != int64(cost*units) || got.Replayed {
		e.t.Fatalf("%s: the reserve is %+v, want %d unit(s) at %d credit(s), rate unit %s, %d credits held", what, got, units, cost, unit, cost*units)
	}
	mnyPricesEqual(e.t, what+": the snapshot the reserve returned", got.Prices, prices)
	if row := e.rows("SELECT kind, available_delta, reserved_delta, unit, units, unit_cost FROM platform.ledger WHERE id = $1", got.ReserveID); len(row) != 1 ||
		row[0] != fmt.Sprintf("reserve|-%d|%d|%s|%d|%d", cost*units, cost*units, unit, units, cost) {
		e.t.Fatalf("%s: the ledger row of the reserve is %v", what, row)
	}
}

// TestMoneyReachable_ReserveFollowsTheVendorFlag: with the vendor fallback OFF a module 1 job of 10 rows reserves 10 credits (1 per row; the rate's unit is the lexicographically first of the two tied units,
// m1_cache), its snapshot holds exactly m1_own and m1_cache (two rows, never the vendor's) and the returned Prices say the same. With it ON the same request reserves 20 (2 per row, rate unit m1_vendor), the snapshot
// has three rows. The flag is a state, not a one-way switch: after OFF again the next job is back at 1 per row. A user with 15 credits is refused at 20 with the exact shortfall (ErrInsufficientCredits, nothing
// written) while the flag is on and reserves at 10 once it is off (the same job: the refusal left nothing behind). Module 2 is not touched by the flag: both of its units stay at 1 whatever it says.
// It fails if the reserve rate or the snapshot is computed over a unit the module cannot reach, or over the wrong flag.
func TestMoneyReachable_ReserveFollowsTheVendorFlag(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 1000)
	e.fund(mnyUserB, "m1", 1000)
	e.fund(mnyUserC, "m1", 15)
	own := map[string]int{"m1_own": 1, "m1_cache": 1}
	all := map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2}

	// (a) the vendor OFF: 1 credit per row, two units in the snapshot
	e.mrcVendor(false)
	jobOff := e.newJob(mnyUserA, "m1")
	off := e.reserve(jobOff, 10)
	e.mrcRequireReserve("vendor off", off, "m1_cache", 1, 10, own)
	if got := e.mrcFrozenUnits(jobOff); got != "m1_cache,m1_own" {
		t.Fatalf("with the vendor off the snapshot holds %q, want exactly m1_cache,m1_own", got)
	}
	if n := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", jobOff); n != 2 {
		t.Fatalf("the snapshot has %d rows, want 2", n)
	}
	e.requireBal("after the reserve with the vendor off", mnyUserA, "m1", 990, 10)

	// (b) the control: the same request with the vendor ON is 2 per row and three units
	e.mrcVendor(true)
	jobOn := e.newJob(mnyUserB, "m1")
	on := e.reserve(jobOn, 10)
	e.mrcRequireReserve("vendor on", on, "m1_vendor", 2, 10, all)
	if got := e.mrcFrozenUnits(jobOn); got != "m1_cache,m1_own,m1_vendor" {
		t.Fatalf("with the vendor on the snapshot holds %q, want all three units", got)
	}
	if n := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", jobOn); n != 3 {
		t.Fatalf("the snapshot has %d rows, want 3", n)
	}
	e.requireBal("after the reserve with the vendor on", mnyUserB, "m1", 980, 20)
	if got := e.mrcFrozenUnits(jobOff); got != "m1_cache,m1_own" {
		t.Fatalf("the job reserved while the vendor was off now holds %q: a flip moved it", got)
	}

	// a customer's credits decide the same job differently under the two flags
	jobC := e.newJob(mnyUserC, "m1")
	before := e.state()
	_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: jobC, Units: 10})
	mnyInsufficient(t, "10 rows at 2 credits against 15 credits (the vendor is on)", err, 15, 20, 5)
	e.requireSame("the refused reserve", before)
	if n := e.int("SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1", jobC); n != 0 {
		t.Fatalf("the refused reserve left %d snapshot row(s)", n)
	}
	e.mrcVendor(false)
	fits := e.reserve(jobC, 10)
	e.mrcRequireReserve("the same job with the vendor off", fits, "m1_cache", 1, 10, own)
	e.requireBal("after the reserve that fits at 1 per row", mnyUserC, "m1", 5, 10)

	// the flag is a state: OFF again after ON, the next job is back at 1 per row
	e.mrcVendor(true)
	e.mrcVendor(false)
	jobAgain := e.newJob(mnyUserA, "m1")
	e.mrcRequireReserve("off again", e.reserve(jobAgain, 4), "m1_cache", 1, 4, own)

	// module 2 does not read the flag: two units at 1, a tie that goes to the first, on either setting
	e.fund(mnyUserA, "m2", 50)
	e.fund(mnyUserB, "m2", 50)
	for _, c := range []struct {
		on   bool
		user string
	}{{true, mnyUserA}, {false, mnyUserB}} {
		e.mrcVendor(c.on)
		res := e.reserve(e.newJob(c.user, "m2"), 7)
		if res.Unit != "m2_accepted" || res.UnitCost != 1 || res.Reserved != 7 {
			t.Fatalf("a module 2 reserve with the vendor flag %v is %+v, want 7 credits at 1 (the flag is module 1's)", c.on, res)
		}
		mnyPricesEqual(t, "the module 2 snapshot", res.Prices, map[string]int{"m2_accepted": 1, "m2_bounced": 1})
	}
	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}

// TestMoneyReachable_AFlipMovesNoJobThatAlreadyReserved: a job reserved while the vendor was OFF keeps its two-row snapshot and its reserve of 1 per row when the flag is turned ON (the reserve replays
// as the same reserve, and an own answer is charged 1 from the frozen snapshot); a job reserved while it was ON keeps its three rows and its reserve of 2 per row when the flag is turned OFF (a vendor
// answer in it IS charged, at the frozen 2). The controls are the jobs reserved AFTER each flip, which are in the new world: three rows at 2, and two rows at 1.
// It fails if a reserve or a settlement reads the flag (or the price table) instead of the job's own frozen snapshot.
func TestMoneyReachable_AFlipMovesNoJobThatAlreadyReserved(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	for _, u := range []string{mnyUserA, mnyUserB, mnyUserC} {
		e.fund(u, "m1", 1000)
	}
	own := map[string]int{"m1_own": 1, "m1_cache": 1}
	all := map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2}

	// reserved while OFF, then the flag is turned ON
	e.mrcVendor(false)
	jobOff := e.newJob(mnyUserA, "m1")
	first := e.reserve(jobOff, 10)
	e.mrcRequireReserve("job reserved while the vendor was off", first, "m1_cache", 1, 10, own)
	e.setStatus(jobOff, "running")
	e.mrcVendor(true)
	if got := e.mrcFrozenUnits(jobOff); got != "m1_cache,m1_own" {
		t.Fatalf("after the flag went ON the job reserved while it was off holds %q, want its two rows", got)
	}
	again, err := e.store.Reserve(ctx, money.ReserveReq{JobID: jobOff, Units: 10})
	mnyNoErr(t, "the replay of the reserve after the flip", err)
	if !again.Replayed || again.ReserveID != first.ReserveID || again.UnitCost != 1 || again.Reserved != 10 {
		t.Fatalf("the replay after the flip is %+v, want the first reserve (1 per row, 10 held) with Replayed", again)
	}
	mnyPricesEqual(t, "the replayed snapshot", again.Prices, own)
	sr := e.settle(jobOff, mnyItem("o-1", mnyM1Accepted), mnyItem("c-1", mnyM1Cached))
	mnyRequireSettle(t, "own and cached answers settled after the flip", sr, 2, 2, 0, 0, 0, 0)
	if got := e.rows("SELECT unit, unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'charge' ORDER BY unit", jobOff); strings.Join(got, ",") != "m1_cache|1,m1_own|1" {
		t.Fatalf("the charge rows are %v, want the frozen 1 credit each", got)
	}

	// the control: a job reserved AFTER the flip is in the new world
	jobNew := e.newJob(mnyUserB, "m1")
	e.mrcRequireReserve("job reserved after the flag went on", e.reserve(jobNew, 10), "m1_vendor", 2, 10, all)

	// the converse: reserved while ON, then the flag is turned OFF
	jobOn := e.newJob(mnyUserC, "m1")
	second := e.reserve(jobOn, 10)
	e.mrcRequireReserve("job reserved while the vendor was on", second, "m1_vendor", 2, 10, all)
	e.setStatus(jobOn, "running")
	e.mrcVendor(false)
	if got := e.mrcFrozenUnits(jobOn); got != "m1_cache,m1_own,m1_vendor" {
		t.Fatalf("after the flag went OFF the job reserved while it was on holds %q, want its three rows", got)
	}
	again, err = e.store.Reserve(ctx, money.ReserveReq{JobID: jobOn, Units: 10})
	mnyNoErr(t, "the replay of the second reserve after the flip", err)
	if !again.Replayed || again.ReserveID != second.ReserveID || again.UnitCost != 2 || again.Reserved != 20 {
		t.Fatalf("the replay after the flip is %+v, want the second reserve (2 per row, 20 held) with Replayed", again)
	}
	mnyPricesEqual(t, "the replayed snapshot of the second job", again.Prices, all)
	sr = e.settle(jobOn, mnyItem("v-1", mnyM1Vendor), mnyItem("o-1", mnyM1Accepted))
	mnyRequireSettle(t, "a vendor and an own answer settled after the flag went OFF (the job was frozen with the vendor in it)", sr, 2, 3, 0, 0, 0, 0)
	if sr.ByUnit["m1_vendor"] != 1 || sr.ByUnit["m1_own"] != 1 || len(sr.ByUnit) != 2 {
		t.Fatalf("ByUnit = %v, want one m1_vendor and one m1_own", sr.ByUnit)
	}
	if got := e.rows("SELECT unit, unit_cost, reserved_delta FROM platform.ledger WHERE job_id = $1 AND item_ref = 'v-1'", jobOn); len(got) != 1 || got[0] != "m1_vendor|2|-2" {
		t.Fatalf("the vendor charge row is %v, want the frozen 2", got)
	}

	// the control of the converse: a job reserved after the flag went OFF is in the new world, and the first job is still where it was after TWO flips
	jobLater := e.newJob(mnyUserB, "m1")
	e.mrcRequireReserve("job reserved after the flag went off again", e.reserve(jobLater, 10), "m1_cache", 1, 10, own)
	if got := e.mrcFrozenUnits(jobOff); got != "m1_cache,m1_own" {
		t.Fatalf("after two flips the first job holds %q, want its two rows", got)
	}
	mnyRequireSettle(t, "the first job settles a further answer from its frozen snapshot", e.settle(jobOff, mnyItem("o-2", mnyM1Accepted)), 1, 1, 0, 0, 0, 0)
	e.requireBal("the first customer after its two charges and the third", mnyUserA, "m1", 990, 7)
	e.requireViewsEmpty("at the end")
	if n := e.requireReservesMatchLedger("at the end"); n != 4 {
		t.Fatalf("%d reserves compared, want 4", n)
	}
}

// TestMoneyReachable_AVendorAnswerThatWasNotReachableIsFlaggedAndNeverCharged: a vendor answer (module 1's external_verified, charge rule M1-02, unit m1_vendor) settled in a job reserved while the vendor
// was OFF is returned in SettleResult.Unmapped with rule M1-02 and a note that names the unit, is not charged (Charged 0, no ledger row, the reserve and the balance untouched, balance_drift empty) and is flagged
// again, never "already settled", when the page is sent again; switching the flag ON afterwards does not make it chargeable (the job's snapshot is what settlement reads). In a page with other answers the
// others are charged and only the vendor answer is flagged: the page is not refused. An m2_accepted outcome in a module 1 job is a different thing (a unit of ANOTHER module, never a configuration) and is
// still money.ErrUnpriced, with nothing written. The control is the same vendor answer in a job reserved while the vendor was ON, which is charged 2.
// It fails if the vendor answer is charged without a vendor price, fails the page, or if the unit of another module stops being an error.
func TestMoneyReachable_AVendorAnswerThatWasNotReachableIsFlaggedAndNeverCharged(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	e.fund(mnyUserB, "m1", 100)

	// the outcome is the one rule M1-02 decides, as a charge of m1_vendor: if the charge table moves, this test says so instead of testing nothing
	if d := charge.Decide(mnyM1Vendor); d.Kind != charge.Charge || d.Unit != "m1_vendor" || d.Rule != "M1-02" {
		t.Fatalf("the vendor outcome is decided as %+v, want a charge of m1_vendor by rule M1-02", d)
	}

	e.mrcVendor(false)
	job := e.newJob(mnyUserA, "m1")
	e.reserve(job, 10)
	e.setStatus(job, "running")
	e.requireBal("after the reserve", mnyUserA, "m1", 90, 10)

	before := e.state()
	sr := e.settle(job, mnyItem("v-1", mnyM1Vendor))
	mnyRequireSettle(t, "a vendor answer in a job reserved with the vendor off", sr, 0, 0, 0, 0, 0, 1)
	if u := sr.Unmapped[0]; u.ItemRef != "v-1" || u.Rule != "M1-02" || !strings.Contains(u.Note, "m1_vendor") || !strings.Contains(u.Note, "not charged") {
		t.Fatalf("the flagged item is %+v, want v-1 by rule M1-02 with a note that names the unit and says it was not charged", u)
	}
	if n := e.int("SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND item_ref = 'v-1'", job); n != 0 {
		t.Fatalf("%d ledger row(s) for the vendor answer, want none", n)
	}
	e.requireSame("the flagged vendor answer", before)
	if got := e.rows("SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1", job); len(got) != 1 || got[0] != "10|0|0" {
		t.Fatalf("the reserve is %v after the flagged answer, want 10 held and nothing charged", got)
	}
	e.requireBal("after the flagged answer", mnyUserA, "m1", 90, 10)
	e.requireViewsEmpty("after the flagged answer")

	// sent again: flagged again (it is not "already settled": nothing was ever charged), and still nothing written
	sr = e.settle(job, mnyItem("v-1", mnyM1Vendor))
	mnyRequireSettle(t, "the same vendor answer again", sr, 0, 0, 0, 0, 0, 1)
	e.requireSame("the same vendor answer again", before)

	// in a page with other answers, the others are charged and only the vendor answer is flagged
	sr = e.settle(job, mnyItem("o-1", mnyM1Accepted), mnyItem("v-2", mnyM1Vendor), mnyItem("c-1", mnyM1Cached))
	mnyRequireSettle(t, "a page with a vendor answer among own and cached ones", sr, 2, 2, 0, 0, 0, 1)
	if sr.Unmapped[0].ItemRef != "v-2" || sr.Unmapped[0].Rule != "M1-02" || sr.ByUnit["m1_own"] != 1 || sr.ByUnit["m1_cache"] != 1 || len(sr.ByUnit) != 2 {
		t.Fatalf("the page was settled as %+v (by unit %v), want v-2 flagged and one own and one cached charge", sr.Unmapped, sr.ByUnit)
	}
	if got := e.rows("SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1", job); len(got) != 1 || got[0] != "10|2|0" {
		t.Fatalf("the reserve is %v, want 10 held and 2 charged", got)
	}

	// switching the vendor ON afterwards does not make the answer chargeable: settlement reads the job's frozen snapshot, not the flag
	e.mrcVendor(true)
	after := e.state()
	sr = e.settle(job, mnyItem("v-1", mnyM1Vendor), mnyItem("v-2", mnyM1Vendor))
	mnyRequireSettle(t, "the vendor answers after the flag went ON", sr, 0, 0, 0, 0, 0, 2)
	e.requireSame("the vendor answers after the flag went ON", after)

	// an m2_accepted outcome in a module 1 job is a unit of another module: an error, and not even the legal item of the page is written
	before = e.state()
	_, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: []money.SettleItem{mnyItem("o-2", mnyM1Accepted), mnyItem("alien", mnyM2Sent)}})
	mnyIs(t, "an m2_accepted charge in a module 1 job", err, money.ErrUnpriced)
	e.requireSame("the page with an outcome of another module", before)
	mnyRequireSettle(t, "control: the legal item alone", e.settle(job, mnyItem("o-2", mnyM1Accepted)), 1, 1, 0, 0, 0, 0)

	// THE CONTROL: the same vendor answer in a job reserved while the vendor was ON is charged 2 (the flag is on now)
	jobOn := e.newJob(mnyUserB, "m1")
	e.mrcRequireReserve("job reserved while the vendor is on", e.reserve(jobOn, 10), "m1_vendor", 2, 10, map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2})
	e.setStatus(jobOn, "running")
	sr = e.settle(jobOn, mnyItem("v-1", mnyM1Vendor))
	mnyRequireSettle(t, "the vendor answer in a job reserved with the vendor on", sr, 1, 2, 0, 0, 0, 0)
	if sr.ByUnit["m1_vendor"] != 1 {
		t.Fatalf("ByUnit = %v, want one m1_vendor", sr.ByUnit)
	}
	if got := e.rows("SELECT unit, unit_cost, reserved_delta FROM platform.ledger WHERE job_id = $1 AND item_ref = 'v-1'", jobOn); len(got) != 1 || got[0] != "m1_vendor|2|-2" {
		t.Fatalf("the vendor charge row is %v, want m1_vendor at 2", got)
	}
	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}

// TestMoneyReachable_AFlipWaitsForAReserveInProgress: the reserve share-locks every unit row of the module, and platform.set_unit_reachable row-locks the unit it changes, so a flip made while a reserve is
// in progress WAITS for it, and the snapshot that reserve took is the reachable set of that instant. The test holds the reserve transaction OPEN (ReserveIn in a transaction it owns) with the vendor OFF,
// starts the flip (the store's SetUnitReachable, on another connection) and PROVES it is blocked behind the reserve's backend (pg_stat_activity wait_event_type = Lock, the function's own statement, and
// pg_blocking_pids naming the reserve's pid), sees the committed flag still off and the open transaction's snapshot at two rows, commits, lets the flip finish (changed = true) and sees the job at its two rows
// and the flag on. A flip made by a store with a short lock wait gives up with ErrLockTimeout while a transaction holds the rows, changes nothing, and succeeds once they are let go.
// THE PAIRED CONTROL runs the same statements WITHOUT the share lock in raw SQL: the flip is not blocked, completes while the transaction is open, and the snapshot is stale by commit.
// It fails if the FOR SHARE is removed from Reserve (the flip would not be blocked), or if the writer does not take the unit's row lock.
func TestMoneyReachable_AFlipWaitsForAReserveInProgress(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 1000)
	e.mrcVendor(false)
	job := e.newJob(mnyUserA, "m1")

	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mnyNoErr(t, "begin the reserve transaction", err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var reserverPID int32
	mnyNoErr(t, "the reserve transaction's pid", tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&reserverPID))
	res, err := e.store.ReserveIn(ctx, tx, money.ReserveReq{JobID: job, Units: 4})
	mnyNoErr(t, "ReserveIn in the caller's transaction", err)

	type flipped struct {
		changed bool
		err     error
	}
	flip := make(chan flipped, 1)
	go func() {
		fctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		changed, err := e.store.SetUnitReachable(fctx, "m1_vendor", true, "test: the owner switched the fallback on while a reserve was open")
		flip <- flipped{changed, err}
	}()
	ws, blocked := e.waitUntil(20*time.Second, func(ws []mnyWait) bool {
		for _, w := range ws {
			if strings.Contains(w.Query, "platform.set_unit_reachable") && mnyHasPID(w.Blockers, reserverPID) {
				return true
			}
		}
		return false
	})
	if !blocked {
		t.Fatalf("the flip was NOT blocked behind the open reserve transaction (pid %d): %s. The reserve holds no share lock on the unit rows, or the writer takes no row lock, so a snapshot can be taken half-way through a flip", reserverPID, mnyDescribe(ws))
	}
	select {
	case r := <-flip:
		t.Fatalf("the flip finished (%v, %v) while the reserve transaction was still open: it was not really blocked", r.changed, r.err)
	default:
	}

	// at that instant the committed flag is still OFF, and the snapshot (read inside the open transaction) holds the two units that were reachable then
	if got := e.str("SELECT reachable::text FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != "false" {
		t.Fatalf("the committed flag reads %s while the flip waits, want false", got)
	}
	if inTx := mnyRows(t, tx, "SELECT unit FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job); strings.Join(inTx, ",") != "m1_cache,m1_own" {
		t.Fatalf("the open transaction's snapshot is %v, want the two units reachable when it was taken", inTx)
	}
	mnyNoErr(t, "commit the reserve transaction", tx.Commit(ctx))
	select {
	case r := <-flip:
		mnyNoErr(t, "the flip once the reserve committed", r.err)
		if !r.changed {
			t.Fatal("the flip finished with changed = false: the flag was off, so switching it on is a change")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the flip did not finish after the reserve transaction committed")
	}
	if res.UnitCost != 1 || res.Reserved != 4 {
		t.Fatalf("the reserve was priced %+v, want 1 per row x 4 rows = 4 (the flag was off when it ran)", res)
	}
	if got := e.mrcFrozenUnits(job); got != "m1_cache,m1_own" {
		t.Fatalf("the job's snapshot is %q after the flip, want the two units it was frozen with", got)
	}
	if got := e.str("SELECT reachable::text FROM platform.unit_costs WHERE unit = 'm1_vendor'"); got != "true" {
		t.Fatalf("the flag reads %s after the flip, want true", got)
	}
	if got := e.rows("SELECT reachable, source FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id DESC LIMIT 1"); len(got) != 1 || got[0] != "true|test: the owner switched the fallback on while a reserve was open" {
		t.Fatalf("the newest history row of the vendor unit is %v, want the flip and its source", got)
	}

	// the wait is bounded: a store whose lock wait is 300 ms gives up with ErrLockTimeout while the rows are held, and writes nothing
	hold, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mnyNoErr(t, "begin the holding transaction", err)
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	_, err = hold.Exec(ctx, "SELECT unit FROM platform.unit_costs WHERE module_id = 'm1' ORDER BY unit FOR SHARE")
	mnyNoErr(t, "the holding transaction's share lock (the statement a reserve takes)", err)
	short := money.New(e.pool, money.WithLockTimeout(300*time.Millisecond))
	state := e.mrcState()
	t0 := time.Now()
	changed, err := short.SetUnitReachable(ctx, "m1_vendor", false, "test: a flip that must give up")
	mnyIs(t, "a flip while the unit rows are held, with a 300 ms wait", err, money.ErrLockTimeout)
	if changed || time.Since(t0) < 250*time.Millisecond || time.Since(t0) > 20*time.Second {
		t.Fatalf("the flip gave up after %s with changed=%v, want to have waited about 300 ms and changed nothing", time.Since(t0), changed)
	}
	if got := e.mrcState(); got != state {
		t.Fatalf("the flip that gave up changed the reachability or its history (%s -> %s)", state, got)
	}
	mnyNoErr(t, "release the holding transaction", hold.Rollback(ctx))
	changed, err = short.SetUnitReachable(ctx, "m1_vendor", false, "test: the same flip once the rows are free")
	if err != nil || !changed {
		t.Fatalf("the same flip once the rows were free returned %v, %v, want changed = true", changed, err)
	}

	// THE PAIRED CONTROL: the same statements without the share lock. The flag is OFF now. The flip is not blocked and the snapshot is stale before the transaction ends.
	job2 := e.newJob(mnyUserA, "m1")
	raw, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mnyNoErr(t, "begin the control transaction", err)
	t.Cleanup(func() { _ = raw.Rollback(context.Background()) })
	_, err = raw.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", money.LockKey(mnyUserA, "m1"))
	mnyNoErr(t, "the control's advisory lock", err)
	var status string
	mnyNoErr(t, "the control's job lock", raw.QueryRow(ctx, "SELECT status FROM platform.jobs WHERE id = $1 FOR UPDATE", job2).Scan(&status))
	// (no `SELECT ... FROM platform.unit_costs ... FOR SHARE` here: that is the one statement the store has and this copy has not)
	_, err = raw.Exec(ctx, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, u.module_id, u.unit, u.cost FROM platform.unit_costs u WHERE u.module_id = 'm1' AND u.reachable`, job2)
	mnyNoErr(t, "the control's snapshot copy", err)
	freeFlip := make(chan flipped, 1)
	go func() {
		fctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		changed, err := e.store.SetUnitReachable(fctx, "m1_vendor", true, "test: the control's flip")
		freeFlip <- flipped{changed, err}
	}()
	select {
	case r := <-freeFlip:
		mnyNoErr(t, "the control's flip", r.err)
		if !r.changed {
			t.Fatal("the control's flip reported no change")
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("CONTROL FAILED: the flip was blocked although the control transaction took no share lock (%s): the interleave above proves nothing about FOR SHARE", mnyDescribe(e.waiting()))
	}
	stale := mnyRows(t, raw, "SELECT unit FROM platform.job_unit_prices WHERE job_id = $1 ORDER BY unit", job2)
	now := e.rows("SELECT unit FROM platform.unit_costs WHERE module_id = 'm1' AND reachable ORDER BY unit")
	if strings.Join(stale, ",") == strings.Join(now, ",") {
		t.Fatalf("CONTROL FAILED: without the share lock the snapshot %v still equals the reachable set %v: the flip did not move the table under it", stale, now)
	}
	if strings.Join(stale, ",") != "m1_cache,m1_own" || strings.Join(now, ",") != "m1_cache,m1_own,m1_vendor" {
		t.Fatalf("CONTROL: snapshot %v and reachable set %v, want the stale two units against the table's three", stale, now)
	}
}

// TestMoneyReachable_SetUnitReachable: the store's writer says whether it changed the flag. Switching a flag to what it already is returns changed = false and writes nothing (not even a history row); a
// change returns true, writes exactly one history row with the (trimmed) source and sets the unit's source and time; the same call again is false. An unknown unit is ErrUnknownUnit (and not ErrInvalid), a blank,
// over-long or non-UTF-8 source and a blank unit are InvalidErrors that name the field; every refusal leaves the flags and the history exactly as they were. The controls are the legal calls beside each refusal.
// It fails if a no-op writes, a change is reported as no change (or the reverse), or a refusal is untyped or changes anything.
func TestMoneyReachable_SetUnitReachable(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	hist := func() int64 {
		return e.int("SELECT count(*) FROM platform.unit_reachability_history WHERE unit = 'm1_vendor'")
	}
	e.mrcVendor(true) // the starting point: on

	h0, state := hist(), e.mrcState()
	changed, err := e.store.SetUnitReachable(ctx, "m1_vendor", true, "test: already so")
	if err != nil || changed {
		t.Fatalf("switching on a unit that is on returned changed=%v, %v; want false, nil", changed, err)
	}
	if hist() != h0 || e.mrcState() != state {
		t.Fatal("a call that changed nothing wrote a history row or moved a flag")
	}

	changed, err = e.store.SetUnitReachable(ctx, "m1_vendor", false, "  m1_describe \t")
	if err != nil || !changed {
		t.Fatalf("switching off a unit that is on returned changed=%v, %v; want true, nil", changed, err)
	}
	if hist() != h0+1 {
		t.Fatalf("one change wrote %d history row(s), want 1", hist()-h0)
	}
	if got := e.rows("SELECT reachable, source FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id DESC LIMIT 1"); len(got) != 1 || got[0] != "false|m1_describe" {
		t.Fatalf("the history row of the change is %v, want the flag and the TRIMMED source", got)
	}
	if got := e.rows("SELECT reachable, reachable_source, reachable_changed_at IS NOT NULL FROM platform.unit_costs WHERE unit = 'm1_vendor'"); len(got) != 1 || got[0] != "false|m1_describe|true" {
		t.Fatalf("the unit reads %v, want the flag, the source and a time", got)
	}
	if changed, err = e.store.SetUnitReachable(ctx, "m1_vendor", false, "test: the same again"); err != nil || changed || hist() != h0+1 {
		t.Fatalf("the same change again returned changed=%v, %v and the history grew to %d: want false, nil and no new row", changed, err, hist()-h0)
	}
	if changed, err = e.store.SetUnitReachable(ctx, "m1_vendor", true, "test: and back"); err != nil || !changed || hist() != h0+2 {
		t.Fatalf("switching back on returned changed=%v, %v (history +%d): want true, nil, +2", changed, err, hist()-h0)
	}

	// the refusals, each beside the legal call that is not refused; none of them writes
	state = e.mrcState()
	_, err = e.store.SetUnitReachable(ctx, "m9_nothing", false, "test: a unit nobody priced")
	mnyIs(t, "an unknown unit", err, money.ErrUnknownUnit)
	if errors.Is(err, money.ErrInvalid) || !strings.Contains(err.Error(), "m9_nothing") {
		t.Fatalf("an unknown unit came back as %v: want ErrUnknownUnit, not ErrInvalid, naming the unit", err)
	}
	_, err = e.store.SetUnitReachable(ctx, "M1_VENDOR", false, "test: the right unit in the wrong case")
	mnyIs(t, "a unit in the wrong case", err, money.ErrUnknownUnit)
	for _, c := range []struct {
		name         string
		unit, source string
		field        string
	}{
		{"a blank unit", " ", "test: x", "unit"},
		{"a blank source", "m1_vendor", "", "source"},
		{"a source of white space", "m1_vendor", " \t\n ", "source"},
		{"a source of 201 bytes", "m1_vendor", strings.Repeat("s", 201), "source"},
		{"a source that is not UTF-8", "m1_vendor", "bad \xff byte", "source"},
	} {
		_, err := e.store.SetUnitReachable(ctx, c.unit, false, c.source)
		mnyInvalid(t, c.name, err, c.field)
	}
	if got := e.mrcState(); got != state {
		t.Fatalf("the refused calls changed the reachability or its history (%s -> %s)", state, got)
	}
	if changed, err = e.store.SetUnitReachable(ctx, "m1_vendor", false, strings.Repeat("s", 200)); err != nil || !changed {
		t.Fatalf("control: a source of exactly 200 bytes returned changed=%v, %v; want true, nil", changed, err)
	}

	// the writer is generic: any priced unit has a flag (the dial only ever uses m1_vendor)
	if changed, err = e.store.SetUnitReachable(ctx, "m2_bounced", false, "test: another unit"); err != nil || !changed {
		t.Fatalf("m2_bounced returned changed=%v, %v; want true, nil", changed, err)
	}
	if changed, err = e.store.SetUnitReachable(ctx, "m2_bounced", true, "test: and back"); err != nil || !changed {
		t.Fatalf("m2_bounced back on returned changed=%v, %v; want true, nil", changed, err)
	}
	e.requireViewsEmpty("at the end")
}

// TestMoneyReachable_AModuleWithNoReachableUnitCannotReserveAndTheRateIsTheDearestReachableOne: the reserve rate is the dearest of the REACHABLE units whatever they are. With the vendor off and m1_own and
// m1_cache unreachable too, the module has nothing a job can be charged and a reserve is refused as ErrSnapshot ("no reachable unit price"), writing nothing: never a reserve at nothing. With one unit
// reachable the snapshot is that one row and the rate is its price: m1_cache alone is 1 per row, the vendor alone is 2 per row (the dearest of ONE unit is that unit, not the module's dearest).
// It fails if a reserve is made over an empty snapshot, or the rate is taken from a unit the snapshot does not hold.
func TestMoneyReachable_AModuleWithNoReachableUnitCannotReserveAndTheRateIsTheDearestReachableOne(t *testing.T) {
	e := newMnyEnv(t)
	e.mrcRemember()
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	set := func(unit string, on bool) {
		e.t.Helper()
		e.exec("SELECT platform.set_unit_reachable($1, $2, $3)", unit, on, mrcVendorSource)
	}
	set("m1_vendor", false)
	set("m1_own", false)
	set("m1_cache", false)
	job := e.newJob(mnyUserA, "m1")
	before := e.state()
	_, err := e.store.Reserve(ctx, money.ReserveReq{JobID: job, Units: 3})
	mnyIs(t, "a reserve of a module none of whose units is reachable", err, money.ErrSnapshot)
	if !strings.Contains(err.Error(), "no reachable unit") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	e.requireSame("the refused reserve", before)

	// the control: one unit reachable and the SAME job reserves, at that unit's price
	set("m1_cache", true)
	res := e.reserve(job, 3)
	e.mrcRequireReserve("only m1_cache reachable", res, "m1_cache", 1, 3, map[string]int{"m1_cache": 1})
	if got := e.mrcFrozenUnits(job); got != "m1_cache" {
		t.Fatalf("the snapshot holds %q, want only m1_cache", got)
	}

	// the vendor alone: the rate is the vendor's 2 although the other units are the module's cheap ones
	set("m1_cache", false)
	set("m1_vendor", true)
	job2 := e.newJob(mnyUserA, "m1")
	e.mrcRequireReserve("only m1_vendor reachable", e.reserve(job2, 3), "m1_vendor", 2, 3, map[string]int{"m1_vendor": 2})
	e.requireBal("after both reserves", mnyUserA, "m1", 91, 9)
	e.requireViewsEmpty("at the end")
	e.requireReservesMatchLedger("at the end")
}
