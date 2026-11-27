//go:build harness

package harness

import (
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/money"
)

// The money store's half of the per-user vendor opt-out (Phase 5 S6; internal/money reserve.go, jobsapi.go, settle.go; platform_0020) against a REAL PostgreSQL, with module 1's vendor unit REACHABLE (the
// environment switches it on): a job whose frozen settings say it is opted out is quoted, reserved and snapshotted over the units that do not need the vendor, the quote and the reserve are one set, a vendor
// answer of such a job is the unpriced case (never charged, flagged, with its own words), and the ledger's own foreign key cannot hold a charge for a unit the snapshot lacks. The opted-in job beside it is the
// control throughout: the same user-visible state, the vendor reachable, 2 per row.

const optOutFrozen = `{"settings":{"unknown_choice":"","vendor_opt_out":true}}`

func TestMoneyOptOut_ARateQuoteAndAReserveAreOverTheSameUnitsAndAnOptedOutJobLeavesTheVendorUnitOut(t *testing.T) {
	e := newMnyEnv(t) // the vendor is reachable
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	e.fund(mnyUserB, "m1", 100)
	out := e.newJob(mnyUserA, "m1")
	e.exec("UPDATE platform.jobs SET frozen_settings = $2::jsonb WHERE id = $1", out, optOutFrozen)
	in := e.newJob(mnyUserB, "m1")

	quote := func(optOut bool) money.RateQuote {
		t.Helper()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q, err := e.store.ReserveRateForIn(ctx, tx, "m1", true, optOut)
		if err != nil {
			t.Fatalf("ReserveRateForIn(optOut %v): %v", optOut, err)
		}
		return q
	}
	qOut, qIn := quote(true), quote(false)
	if qIn.UnitCost != 2 || qIn.Unit != money.UnitM1Vendor || len(qIn.Prices) != 3 {
		t.Fatalf("control: the opted-in quote is %+v, want 2 at m1_vendor over three units", qIn)
	}
	if qOut.UnitCost != 1 || qOut.Unit == money.UnitM1Vendor || !reflect.DeepEqual(qOut.Prices, map[string]int{"m1_own": 1, "m1_cache": 1}) {
		t.Fatalf("the opted-out quote is %+v, want 1 at a unit that is not m1_vendor over the two units that do not need the vendor", qOut)
	}
	if q, err := func() (money.RateQuote, error) {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return money.RateQuote{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		return e.store.ReserveRateIn(ctx, tx, "m1", false) // the older entry point is the opted-in quote
	}(); err != nil || !reflect.DeepEqual(q, qIn) {
		t.Errorf("ReserveRateIn = %+v, %v, want the opted-in quote %+v", q, err, qIn)
	}

	// the reserve reads the job's OWN frozen choice (not a flag of the caller) and freezes exactly what the quote said
	rOut := e.reserve(out, 10)
	if rOut.Unit != qOut.Unit || rOut.UnitCost != qOut.UnitCost || !reflect.DeepEqual(rOut.Prices, qOut.Prices) || rOut.Reserved != 10 {
		t.Fatalf("the opted-out reserve is %+v, the quote said %+v: the rate a preview shows must be the rate a confirm freezes", rOut, qOut)
	}
	if got := e.frozen(out); !reflect.DeepEqual(got, qOut.Prices) {
		t.Errorf("the opted-out job's snapshot in the table is %v, want %v", got, qOut.Prices)
	}
	rIn := e.reserve(in, 10)
	if rIn.Unit != money.UnitM1Vendor || rIn.UnitCost != 2 || rIn.Reserved != 20 || len(rIn.Prices) != 3 {
		t.Fatalf("control: the opted-in reserve is %+v, want 2 at m1_vendor, 20 held, three units", rIn)
	}
	e.requireBal("after the two reserves", mnyUserA, "m1", 90, 10)
	e.requireBal("after the two reserves", mnyUserB, "m1", 80, 20)
	// a reserve that is repeated is the same reserve (the idempotence holds for the opted-out job too)
	if again, err := e.store.Reserve(ctx, money.ReserveReq{JobID: out, Units: 10}); err != nil || !again.Replayed || again.Unit != rOut.Unit || again.UnitCost != 1 || len(again.Prices) != 2 {
		t.Errorf("the repeated reserve is %+v, %v, want the same reserve replayed", again, err)
	}
	e.requireViewsEmpty("after the reserves")
}

func TestMoneyOptOut_AVendorAnswerOfAnOptedOutJobIsTheUnpricedCaseWithItsOwnWordsAndTheLedgerCannotHoldACharge(t *testing.T) {
	e := newMnyEnv(t)
	ctx := testCtx(t)
	e.fund(mnyUserA, "m1", 100)
	e.fund(mnyUserB, "m1", 100)
	out := e.newJob(mnyUserA, "m1")
	e.exec("UPDATE platform.jobs SET frozen_settings = $2::jsonb WHERE id = $1", out, optOutFrozen)
	in := e.newJob(mnyUserB, "m1")
	e.reserve(out, 10)
	e.reserve(in, 10)
	e.setStatus(out, "running")
	e.setStatus(in, "running")

	// the opted-out job: the vendor answer is returned unmapped by the rule that decided it, with a note that says why this job's snapshot never held the unit; nothing is written
	before := e.state()
	sr := e.settle(out, mnyItem("v-1", mnyM1Vendor), mnyItem("v-2", mnyM1Vendor))
	mnyRequireSettle(t, "two vendor answers in an opted-out job", sr, 0, 0, 0, 0, 0, 2)
	if u := sr.Unmapped[0]; u.Rule != "M1-02" || !strings.Contains(u.Note, "m1_vendor") || !strings.Contains(u.Note, "opted out") || !strings.Contains(u.Note, "not charged") {
		t.Fatalf("the flagged item is %+v, want rule M1-02 and a note that names the unit, says the job is opted out and says it was not charged", u)
	}
	e.requireSame("the flagged vendor answers", before)
	// in a page with other answers the others are charged and only the vendor answers are flagged: the page is not refused
	sr = e.settle(out, mnyItem("o-1", mnyM1Accepted), mnyItem("v-3", mnyM1Vendor), mnyItem("c-1", mnyM1Cached))
	mnyRequireSettle(t, "a page with a vendor answer among own and cached ones", sr, 2, 2, 0, 0, 0, 1)
	if got := e.rows("SELECT reserved, charged, released FROM platform.reserves WHERE job_id = $1", out); len(got) != 1 || got[0] != "10|2|0" {
		t.Fatalf("the reserve is %v, want 10 held and 2 charged", got)
	}

	// the control: the same vendor answer in the opted-in job is a charge of 2 at m1_vendor, with the older note nowhere
	mnyRequireSettle(t, "the same vendor answer in an opted-in job", e.settle(in, mnyItem("v-1", mnyM1Vendor)), 1, 2, 0, 0, 0, 0)

	// the ledger's own wall: a charge of m1_vendor for the opted-out job cannot be written by anyone (its snapshot has no such unit), whatever any code believes
	var reserveID int64
	if err := e.admin.QueryRow(ctx, "SELECT reserve_id FROM platform.reserves WHERE job_id = $1", out).Scan(&reserveID); err != nil {
		t.Fatal(err)
	}
	tx := ledgerBegin(t, e.admin)
	const chargeSQL = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, reserve_id, unit, units, unit_cost, item_ref, idempotency_key)
		VALUES ($1::uuid, 'm1', 'charge', 0, $2, $3::uuid, $4, $5, 1, $6, $7, $8)`
	ledgerExpectRefused(t, tx, "a charge of m1_vendor in an opted-out job", "23503", ledgerWhy{Constraint: "ledger_frozen_price_fk"}, chargeSQL, mnyUserA, -2, out, reserveID, "m1_vendor", 2, "wall-1", "charge:"+out+":wall-1")
	ledgerExpectOK(t, tx, "control: a charge of m1_own in the same job", chargeSQL, mnyUserA, -1, out, reserveID, "m1_own", 1, "wall-2", "charge:"+out+":wall-2")
	e.requireViewsEmpty("after the opted-out settlements")
}
