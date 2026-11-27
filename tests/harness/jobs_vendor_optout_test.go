//go:build harness

package harness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/usersettings"
)

// The per-user vendor opt-out in the job store (Phase 5 S6; internal/jobs optout.go, platform_0020) against a REAL PostgreSQL. The store reads module 1's vendor reachability through a vendorFake and its
// opt-out policy key through an optOutFake (module 1 itself is the harness scope jobs); the user's setting is the real internal/usersettings over the real table.
//
//	TheSettingIsReadOnceWhenAModuleOneJobIsCreatedAndFrozen    a change of the setting never alters a job that exists, a new job takes the new value, a send job never carries it, a user's value is not another's
//	TheReserveIsOneForAnOptedOutJobAndTwoForAnOptedInJob...     with the vendor reachable: 1 per row and no vendor unit in the snapshot, against 2 and the vendor unit; the rate a preview shows is the rate the confirm freezes
//	AnOptedOutJobIsPricedWithoutModuleOneAndNeverRefused...     module 1 unreadable and the flag the seeded off: the opted-in job's immediate confirm is refused, the opted-out one is priced and confirmed; its preview is not stale when the vendor flips
//	TheDatabaseRefusesAReserveThatDoesNotMatchTheJobsChoice     an opted-out job whose snapshot holds the vendor unit, and an opted-in job whose snapshot omits it, are both refused by the ledger's own check
//	AVendorAnswerOfAnOptedOutJobIsNeverChargedAndIsFlagged      the unpriced safety net, with the opted-in job charged 2 for the same answer as the control
//	TheConfirmOfAnOptedOutJobNeedsModuleOnesPolicyKey           refused, nothing held, until the key is known; an opted-in job is not asked
//	AnUnconfiguredModuleOneStoreStillPricesByTheJobsOwnChoice   a store with no provider and no source: the reserve is the job's own choice

// optOutFake is a jobs.OptOutPolicySource that says what the test tells it and counts its asks.
type optOutFake struct {
	mu    sync.Mutex
	key   string
	err   error
	calls int
}

var _ jobs.OptOutPolicySource = (*optOutFake)(nil)

func (f *optOutFake) M1OptOutPolicy(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.key, f.err
}

func (f *optOutFake) says(key string) { f.mu.Lock(); f.key, f.err = key, nil; f.mu.Unlock() }
func (f *optOutFake) fails(err error) { f.mu.Lock(); f.key, f.err = "", err; f.mu.Unlock() }
func (f *optOutFake) count() int      { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

const optOutTestKey = "cold-email-no-vendor"

// newOptOutEnv is a job-store environment that reads module 1's vendor flag through the fake (no memory), knows module 1's opt-out key through the other fake, and holds the real settings store. The vendor
// flag starts false (jbsEnv's start) and the key is known.
func newOptOutEnv(t *testing.T) (*jbsEnv, *vendorFake, *optOutFake, *usersettings.Store) {
	t.Helper()
	vf, of := &vendorFake{}, &optOutFake{key: optOutTestKey}
	e := newJbsEnv(t, jobs.WithVendorReachability(vf), jobs.WithVendorReachabilityTTL(0), jobs.WithOptOutPolicy(of))
	us, err := usersettings.New(e.pool)
	if err != nil {
		t.Fatalf("usersettings.New: %v", err)
	}
	return e, vf, of, us
}

func optOutSet(t *testing.T, us *usersettings.Store, user string, v bool) {
	t.Helper()
	if _, err := us.Set(testCtx(t), user, v); err != nil {
		t.Fatalf("Set(%s, %v): %v", user, v, err)
	}
}

// storedChoice is the key of the vendor opt-out in the job's frozen settings as the table holds it: "true", "false" or "-" (no key).
func (e *jbsEnv) storedChoice(job string) string {
	e.t.Helper()
	return e.str(`SELECT COALESCE(frozen_settings #>> '{settings,vendor_opt_out}', '-') FROM platform.jobs WHERE id = $1`, job)
}

func TestJobsStore_VendorOptOut_TheSettingIsReadOnceWhenAModuleOneJobIsCreatedAndFrozen(t *testing.T) {
	e, _, _, us := newOptOutEnv(t)
	u := e.newUser()

	// opted in (no row): the job carries no key and reads as opted in
	in1 := e.create(u, "m1", 3)
	if in1.Settings.VendorOptOut || e.storedChoice(in1.ID) != "-" {
		t.Fatalf("a job of a user with no setting: %+v, stored %q, want opted in and no key", in1.Settings, e.storedChoice(in1.ID))
	}
	// the user opts out: the job that exists keeps what it was created under, and the NEXT job is opted out
	optOutSet(t, us, u, true)
	got, err := e.jobs.Get(testCtx(t), u, in1.ID)
	if err != nil || got.Settings.VendorOptOut || e.storedChoice(in1.ID) != "-" {
		t.Fatalf("after the user opted out, the job that existed reads %+v (%v), stored %q: it must keep what it was created under", got.Settings, err, e.storedChoice(in1.ID))
	}
	out1 := e.create(u, "m1", 3)
	if !out1.Settings.VendorOptOut || e.storedChoice(out1.ID) != "true" {
		t.Fatalf("a job created after the user opted out: %+v, stored %q, want opted out and the key true", out1.Settings, e.storedChoice(out1.ID))
	}
	// the user opts back in: the opted-out job keeps its value, and the next one is opted in again
	optOutSet(t, us, u, false)
	if got, err := e.jobs.Get(testCtx(t), u, out1.ID); err != nil || !got.Settings.VendorOptOut || e.storedChoice(out1.ID) != "true" {
		t.Fatalf("after the user opted back in, the opted-out job reads %+v (%v), stored %q: it must keep its choice", got.Settings, err, e.storedChoice(out1.ID))
	}
	in2 := e.create(u, "m1", 3)
	if in2.Settings.VendorOptOut || e.storedChoice(in2.ID) != "-" {
		t.Fatalf("a job created after the user opted back in: %+v, stored %q", in2.Settings, e.storedChoice(in2.ID))
	}
	// the choice is part of what the job's preview says, and the API's read of it is the stored one
	optOutSet(t, us, u, true)
	if p := e.preview(u, out1.ID); !p.VendorOptOut {
		t.Errorf("the preview of the opted-out job says vendor_opt_out %v while the user is opted out again", p.VendorOptOut)
	}
	if p := e.preview(u, in2.ID); p.VendorOptOut {
		t.Errorf("the preview of the job created opted in says vendor_opt_out %v: it must follow the job, not the setting as it is now", p.VendorOptOut)
	}

	// a send job has no vendor: the user is opted out and neither carries the key
	for _, module := range []string{"m2", "m3"} {
		j := e.create(u, module, 2)
		if j.Settings.VendorOptOut || e.storedChoice(j.ID) != "-" {
			t.Errorf("a %s job of an opted-out user: %+v, stored %q, want no key", module, j.Settings, e.storedChoice(j.ID))
		}
	}
	// another user's value is never read: B never stated one while A is opted out
	b := e.newUser()
	if jb := e.create(b, "m1", 2); jb.Settings.VendorOptOut || e.storedChoice(jb.ID) != "-" {
		t.Errorf("a job of another user took the first user's value: %+v", jb.Settings)
	}
	e.requireBooks("after the creates")
}

func TestJobsStore_VendorOptOut_TheReserveIsOneForAnOptedOutJobAndTwoForAnOptedInJobWithTheVendorReachableAndThePreviewIsTheConfirmsRate(t *testing.T) {
	e, vf, _, us := newOptOutEnv(t)
	vf.says(true) // module 1 says its vendor fallback is reachable now
	in, out := e.newUser(), e.newUser()
	e.fund(in, "m1", 1000)
	e.fund(out, "m1", 1000)
	optOutSet(t, us, out, true)
	const rows = 10

	// the opted-in job: 2 per row at the vendor unit, module 1 was asked (the control: the vendor IS reachable in this test)
	jIn, pIn := e.previewed(in, "m1", rows)
	if pIn.UnitCost != 2 || pIn.RateUnit != money.UnitM1Vendor || pIn.Credits != 2*rows || pIn.VendorOptOut || !pIn.VendorReachableKnown {
		t.Fatalf("the opted-in preview is %+v, want 2 per row at m1_vendor, not opted out, and module 1 read", pIn)
	}
	askedBefore := vf.count()

	// the opted-out job: 1 per row, not the vendor unit, and module 1 is NOT asked about the vendor for it
	jOut, pOut := e.previewed(out, "m1", rows)
	if pOut.UnitCost != 1 || pOut.RateUnit == money.UnitM1Vendor || pOut.Credits != rows || !pOut.VendorOptOut || pOut.VendorReachableKnown || pOut.Units != rows {
		t.Fatalf("the opted-out preview is %+v, want 1 per row at a unit that is not m1_vendor, opted out, and module 1 not read for it", pOut)
	}
	if vf.count() != askedBefore {
		t.Errorf("the preview of an opted-out job asked module 1 about the vendor (%d -> %d calls)", askedBefore, vf.count())
	}
	if got := e.str("SELECT preview->>'vendor_opt_out' FROM platform.jobs WHERE id = $1", jOut.ID); got != "true" {
		t.Errorf("the stored preview says vendor_opt_out = %q, want true: the stored document and the answer must agree", got)
	}
	// the same preview again is the same preview: its fingerprint does not move with module 1's answer, which it does not read
	vf.says(false)
	if p2 := e.preview(out, jOut.ID); p2.Fingerprint != pOut.Fingerprint || p2.UnitCost != 1 {
		t.Errorf("a second preview with module 1 saying the other thing is %+v, want the same fingerprint and rate", p2)
	}
	vf.says(true)

	// the confirm reserves at the rate the preview showed, with the snapshot of the units this job can produce
	rOut := e.confirm(out, jOut.ID, pOut.Fingerprint)
	if rOut.Reserved != rows || rOut.Pending != rows {
		t.Fatalf("the opted-out confirm: %+v, want %d credits held for %d rows", rOut, rows, rows)
	}
	if u, c := e.str("SELECT unit FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", jOut.ID), e.int("SELECT unit_cost FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", jOut.ID); u != pOut.RateUnit || int(c) != pOut.UnitCost {
		t.Errorf("the reserve is at %s x %d, the preview said %s x %d: the rate a preview shows must be the rate a confirm freezes", u, c, pOut.RateUnit, pOut.UnitCost)
	}
	if got := e.frozen(jOut.ID); len(got) != 2 || got["m1_own"] != 1 || got["m1_cache"] != 1 {
		t.Errorf("the opted-out job's price snapshot is %v, want the two units that do not need the vendor", got)
	}
	// the opted-in job, same moment: 2 per row and all three units (the vendor's reachability is what it follows)
	rIn := e.confirm(in, jIn.ID, pIn.Fingerprint)
	if rIn.Reserved != 2*rows {
		t.Fatalf("the opted-in confirm held %d credits, want %d", rIn.Reserved, 2*rows)
	}
	if got := e.frozen(jIn.ID); len(got) != 3 || got[money.UnitM1Vendor] != 2 {
		t.Errorf("the opted-in job's price snapshot is %v, want the three units with m1_vendor at 2", got)
	}
	// the opted-out job asked module 1 nothing about the vendor at its confirm either: only the opted-in job's two calls (its preview and its confirm) and nothing for the opted-out job's three reads
	if want := askedBefore + 1; vf.count() != want { // the opted-in confirm
		t.Errorf("module 1 was asked %d time(s) in all, want %d (the opted-in preview and confirm; none for the opted-out job)", vf.count(), want)
	}
	if a, r, _ := e.bal(out, "m1"); a != 1000-rows || r != rows {
		t.Errorf("the opted-out user's balance is %d available, %d reserved, want %d and %d", a, r, 1000-rows, rows)
	}
	e.requireBooks("after the two confirms")
}

func TestJobsStore_VendorOptOut_AnOptedOutJobIsPricedWithoutModuleOneAndNeverRefusedForItsReachabilityAndItsPreviewIsNotStaleWhenTheVendorFlips(t *testing.T) {
	e, vf, _, us := newOptOutEnv(t)
	in, out := e.newUser(), e.newUser()
	e.fund(in, "m1", 1000)
	e.fund(out, "m1", 1000)
	optOutSet(t, us, out, true)
	const rows = 6

	// the state that refuses an immediate confirm: the flag is the seeded off that module 1 never set, and module 1 cannot be asked
	e.setVendorFlag(false, vendorSeedSource)
	vf.cannotBeAsked()
	jIn, pIn := e.previewed(in, "m1", rows)
	jOut, pOut := e.previewed(out, "m1", rows)
	if pOut.UnitCost != 1 || !pOut.VendorOptOut || pOut.VendorReachableKnown {
		t.Fatalf("the opted-out preview with module 1 unreadable is %+v", pOut)
	}
	// the control: the opted-in job is refused in this very state (nothing can price it)
	w := e.writes()
	if _, err := e.tryConfirm(in, jIn.ID, pIn.Fingerprint, e.key(), nil); !errors.Is(err, jobs.ErrModuleUnavailable) {
		t.Fatalf("control: the opted-in immediate confirm with module 1 unreadable gave %v, want ErrModuleUnavailable", err)
	}
	e.requireWrites("the refused opted-in confirm", w)
	// the opted-out job is priced from its own choice: confirmed at 1 per row, and module 1 was not asked
	asked := vf.count()
	r, err := e.tryConfirm(out, jOut.ID, pOut.Fingerprint, e.key(), nil)
	if err != nil || r.Reserved != rows || r.Job.Status != "confirmed" {
		t.Fatalf("the opted-out immediate confirm with module 1 unreadable: %+v, %v; want confirmed and %d credits", r, err, rows)
	}
	if vf.count() != asked {
		t.Errorf("the confirm of an opted-out job asked module 1 about the vendor (%d -> %d calls)", asked, vf.count())
	}

	// a preview made while the vendor was off is NOT stale when the vendor flips on before the confirm (its price does not depend on it); the opted-in job's would be (the contrast)
	out2 := e.newUser()
	e.fund(out2, "m1", 1000)
	optOutSet(t, us, out2, true)
	vf.says(false)
	jOut2, pOut2 := e.previewed(out2, "m1", rows)
	in2 := e.newUser()
	e.fund(in2, "m1", 1000)
	jIn2, pIn2 := e.previewed(in2, "m1", rows) // under off
	vf.says(true)
	e.previewed(e.newUser(), "m1", 1) // anything of a module 1 job reads module 1 and makes the flag module 1's answer: on
	if got := e.vendorFlag(); got != "true|m1_describe" {
		t.Fatalf("the flag is %q, want it flipped on by module 1's answer", got)
	}
	if _, err := e.tryConfirm(in2, jIn2.ID, pIn2.Fingerprint, e.key(), nil); !errors.Is(err, jobs.ErrStalePreview) {
		t.Fatalf("control: the opted-in preview made under off, confirmed under on, gave %v, want ErrStalePreview", err)
	}
	if r, err := e.tryConfirm(out2, jOut2.ID, pOut2.Fingerprint, e.key(), nil); err != nil || r.Reserved != rows {
		t.Errorf("the opted-out preview made under off, confirmed under on, gave %+v, %v; want it accepted at 1 per row (its price never depended on the vendor)", r, err)
	}
	e.requireBooks("after the confirms")
}

func TestJobsStore_VendorOptOut_TheDatabaseRefusesAReserveThatDoesNotMatchTheJobsChoice(t *testing.T) {
	e, _, _, us := newOptOutEnv(t)
	outUser, inUser := e.newUser(), e.newUser()
	e.fund(outUser, "m1", 1000)
	e.fund(inUser, "m1", 1000)
	optOutSet(t, us, outUser, true)
	bad := e.create(outUser, "m1", 2)  // opted out, to be given a snapshot that holds the vendor unit
	good := e.create(outUser, "m1", 2) // opted out, to be given the right one (the control)
	omit := e.create(inUser, "m1", 2)  // opted IN, to be given a snapshot that omits the vendor unit (it cannot opt out by omission)

	tx := ledgerBegin(t, e.admin)
	ledgerMust(t, tx, "SELECT platform.set_unit_reachable('m1_vendor', true, 'test: the vendor is reachable')")
	snap := func(job string, units ...string) {
		t.Helper()
		ledgerMust(t, tx, `INSERT INTO platform.job_unit_prices (job_id, module_id, unit, cost) SELECT $1::uuid, module_id, unit, cost FROM platform.unit_costs WHERE unit = ANY($2::text[])`, job, units)
	}
	const reserve = `INSERT INTO platform.ledger (user_id, module_id, kind, available_delta, reserved_delta, job_id, unit, units, unit_cost, idempotency_key)
		VALUES ($1::uuid, 'm1', 'reserve', $2, $3, $4::uuid, $5, $6, $7, $8)`

	snap(bad.ID, "m1_own", "m1_vendor") // two rows: as many as an opted-out job's complete snapshot, so the count passes and the unit is what is wrong
	ledgerExpectRefused(t, tx, "a reserve of an opted-out job whose snapshot holds the vendor unit", "23514", ledgerWhy{Constraint: "reserve_no_vendor_unit_for_an_opted_out_job"},
		reserve, outUser, -4, 4, bad.ID, "m1_vendor", 2, 2, "optout-bad")

	snap(good.ID, "m1_own", "m1_cache")
	ledgerExpectOK(t, tx, "control: the snapshot an opted-out job can produce, reserved at its dearest price (1)", reserve, outUser, -2, 2, good.ID, "m1_own", 2, 1, "optout-good")

	snap(omit.ID, "m1_own", "m1_cache") // the vendor is reachable and this job is opted in: its complete snapshot is the three units
	ledgerExpectRefused(t, tx, "a reserve of an opted-IN job whose snapshot leaves the vendor unit out (a job cannot opt out by omission)", "23514", ledgerWhy{Constraint: "reserve_needs_the_complete_snapshot"},
		reserve, inUser, -2, 2, omit.ID, "m1_own", 2, 1, "optout-omit")
}

func TestJobsStore_VendorOptOut_AVendorAnswerOfAnOptedOutJobIsNeverChargedAndIsFlaggedWhileTheOptedInJobIsChargedTwoForTheSame(t *testing.T) {
	e, vf, _, us := newOptOutEnv(t)
	vf.says(true)
	in, out := e.newUser(), e.newUser()
	e.fund(in, "m1", 1000)
	e.fund(out, "m1", 1000)
	optOutSet(t, us, out, true)
	const rows = 4
	jIn, pIn := e.previewed(in, "m1", rows)
	jOut, pOut := e.previewed(out, "m1", rows)
	e.confirm(in, jIn.ID, pIn.Fingerprint)
	e.confirm(out, jOut.ID, pOut.Fingerprint)
	// the outcome is the one rule M1-02 decides, as a charge of m1_vendor: if the charge table moves, this test says so instead of testing nothing
	vendor := charge.Outcome{Module: "m1", State: "valid", Reason: "external_verified"}
	if d := charge.Decide(vendor); d.Kind != charge.Charge || d.Unit != money.UnitM1Vendor {
		t.Fatalf("the vendor outcome is decided as %+v, want a charge of m1_vendor", d)
	}
	record := func(job string) jobs.RecordSummary {
		t.Helper()
		jobWalkTo(t, e.admin, job, "running")
		var items []jobs.ResultItem
		for _, id := range e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY ordinal", job) {
			items = append(items, jobs.ResultItem{ItemRef: id, Outcome: vendor})
		}
		sum, err := e.jobs.RecordResults(testCtx(t), job, items)
		if err != nil {
			t.Fatalf("RecordResults(%s): %v", job, err)
		}
		return sum
	}

	// the control: the opted-in job IS charged for the vendor's answer, 2 a row (so what follows is the opt-out and not a vendor that cannot be charged)
	sIn := record(jIn.ID)
	if sIn.Charged != rows || sIn.Credits != 2*rows || sIn.Unmapped != 0 {
		t.Fatalf("the opted-in job's vendor answers were recorded as %+v, want %d charged for %d credits and none unmapped", sIn, rows, 2*rows)
	}
	if got := e.chargesByUnit(jIn.ID); len(got) != 1 || got[0] != "m1_vendor:4:8" {
		t.Fatalf("the opted-in job's charges are %v, want m1_vendor:4:8", got)
	}

	// the opted-out job: nothing is charged, every answer is recorded UNMAPPED (free) and the job is flagged once for the rule, and the whole reserve stays held for the release
	sOut := record(jOut.ID)
	if sOut.Charged != 0 || sOut.Credits != 0 || sOut.Unmapped != rows || sOut.Recorded != rows {
		t.Fatalf("the opted-out job's vendor answers were recorded as %+v, want none charged and %d recorded unmapped", sOut, rows)
	}
	if got := e.chargesByUnit(jOut.ID); len(got) != 0 {
		t.Errorf("the opted-out job was charged %v for answers its snapshot could not price", got)
	}
	if got := e.rows("SELECT decision || '|' || rule || '|' || COALESCE(unit, '-') FROM platform.job_results WHERE job_id = $1 ORDER BY ordinal", jOut.ID); len(got) != rows || got[0] != "unmapped|M1-02|-" {
		t.Errorf("the opted-out job's results are %v, want %d rows of unmapped|M1-02|-", got, rows)
	}
	if n := e.int("SELECT count(*) FROM platform.job_events WHERE job_id = $1 AND kind = 'flag'", jOut.ID); n != 1 {
		t.Errorf("the opted-out job has %d flag event(s), want one (for rule M1-02)", n)
	}
	if got := e.rows("SELECT reserved || '|' || charged || '|' || released FROM platform.reserves WHERE job_id = $1", jOut.ID); len(got) != 1 || got[0] != "4|0|0" {
		t.Errorf("the opted-out job's reserve is %v, want 4 held and nothing charged or released yet", got)
	}
	if a, r, _ := e.bal(out, "m1"); a != 1000-rows || r != rows {
		t.Errorf("the opted-out user's balance is %d available, %d reserved: the answers must cost them nothing", a, r)
	}
	e.requireBooks("after the vendor answers")
}

func TestJobsStore_VendorOptOut_TheConfirmOfAnOptedOutJobNeedsModuleOnesPolicyKeyAndAnOptedInJobIsNotAsked(t *testing.T) {
	e, vf, of, us := newOptOutEnv(t)
	vf.says(false)
	out, in := e.newUser(), e.newUser()
	e.fund(out, "m1", 1000)
	e.fund(in, "m1", 1000)
	optOutSet(t, us, out, true)
	const rows = 5
	jOut, pOut := e.previewed(out, "m1", rows)

	for _, c := range []struct {
		name string
		set  func()
	}{
		{"module 1 cannot be asked", func() { of.fails(errors.New("module 1 is down")) }},
		{"an older module 1 that publishes no key", func() { of.fails(jobs.ErrNoOptOutPolicy) }},
		{"an answer with no key and no error", func() { of.says("") }},
	} {
		c.set()
		w := e.writes()
		_, err := e.tryConfirm(out, jOut.ID, pOut.Fingerprint, e.key(), nil)
		jbsNotAllowed(t, c.name+": the immediate confirm", err, jobs.ReasonOptOutPolicyUnavailable)
		start := time.Now().Add(2 * time.Hour)
		_, err = e.tryConfirm(out, jOut.ID, pOut.Fingerprint, e.key(), &start)
		jbsNotAllowed(t, c.name+": the scheduled confirm", err, jobs.ReasonOptOutPolicyUnavailable)
		e.requireWrites(c.name+": the refused confirms", w)
		if st := e.jobStatus(jOut.ID); st != "previewed" || e.reserveRows(jOut.ID) != 0 {
			t.Errorf("%s: the refused job is %s with %d reserve row(s), want previewed and none", c.name, st, e.reserveRows(jOut.ID))
		}
		if a, r, _ := e.bal(out, "m1"); a != 1000 || r != 0 {
			t.Errorf("%s: the balance is %d available, %d reserved, want 1000 and 0", c.name, a, r)
		}
		// the control: an opted-IN job confirms in the very same state (it needs no key), and the source was not asked for it
		asked := of.count()
		jIn, pIn := e.previewed(e.newUser(), "m1", 2)
		_ = jIn
		_ = pIn
		if of.count() != asked {
			t.Errorf("%s: a preview of an opted-in job asked for the opt-out key", c.name)
		}
	}
	// opted-in control confirm, in the state "module 1 cannot be asked"
	of.fails(errors.New("module 1 is down"))
	jIn, pIn := e.previewed(in, "m1", rows)
	asked := of.count()
	if r := e.confirm(in, jIn.ID, pIn.Fingerprint); r.Reserved != rows {
		t.Fatalf("the opted-in confirm with the key unknown: %+v, want it accepted at 1 per row (the vendor flag is off)", r)
	}
	if of.count() != asked {
		t.Errorf("the confirm of an opted-in job asked for the opt-out key")
	}

	// the same confirm, once the key is known (a retry): accepted at 1 per row, and the key was asked for it
	of.says(optOutTestKey)
	asked = of.count()
	r := e.confirm(out, jOut.ID, pOut.Fingerprint)
	if r.Reserved != rows || r.Job.Status != "confirmed" {
		t.Fatalf("the retry once the key is known: %+v, want confirmed and %d credits", r, rows)
	}
	if of.count() != asked+1 {
		t.Errorf("the confirm asked for the key %d time(s), want once", of.count()-asked)
	}

	// a store that was given a vendor provider but NO source for the key refuses an opted-out job the same way (module 1 is configured, so its key MUST come from somewhere)
	noSource, err := jobs.New(e.pool, e.store, jobs.WithVendorReachability(vf), jobs.WithVendorReachabilityTTL(0))
	if err != nil {
		t.Fatal(err)
	}
	u3 := e.newUser()
	e.fund(u3, "m1", 100)
	optOutSet(t, us, u3, true)
	j3 := e.create(u3, "m1", 2)
	p3, err := noSource.Preview(testCtx(t), u3, j3.ID)
	if err != nil || !p3.VendorOptOut {
		t.Fatalf("the preview on the store with no source: %+v, %v", p3, err)
	}
	_, err = noSource.Confirm(testCtx(t), u3, jobs.ConfirmReq{JobID: j3.ID, Fingerprint: p3.Fingerprint, Key: e.key()})
	jbsNotAllowed(t, "the confirm on a store with a vendor provider and no source for the key", err, jobs.ReasonOptOutPolicyUnavailable)
	e.requireBooks("after the refusals")
}

func TestJobsStore_VendorOptOut_AStoreWithNoModuleOneAtAllStillPricesByTheJobsOwnChoiceAndAJobWithNoKeyIsOptedIn(t *testing.T) {
	e := newJbsEnv(t) // no provider, no source: module 1 is not configured in this process
	us, err := usersettings.New(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	// the vendor IS reachable in the database: an opted-in job reserves at 2, an opted-out job at 1, whatever the store knows of module 1
	e.exec("SELECT platform.set_unit_reachable('m1_vendor', true, 'test: the vendor is reachable')")
	in, out := e.newUser(), e.newUser()
	e.fund(in, "m1", 1000)
	e.fund(out, "m1", 1000)
	optOutSet(t, us, out, true)
	jIn, pIn := e.previewed(in, "m1", 5)
	jOut, pOut := e.previewed(out, "m1", 5)
	if pIn.UnitCost != 2 || pOut.UnitCost != 1 || pIn.VendorOptOut || !pOut.VendorOptOut {
		t.Fatalf("the previews are %+v and %+v, want 2 per row opted in and 1 per row opted out", pIn, pOut)
	}
	if r := e.confirm(in, jIn.ID, pIn.Fingerprint); r.Reserved != 10 {
		t.Errorf("the opted-in confirm held %d, want 10", r.Reserved)
	}
	if r := e.confirm(out, jOut.ID, pOut.Fingerprint); r.Reserved != 5 {
		t.Errorf("the opted-out confirm held %d, want 5", r.Reserved)
	}
	// a job whose frozen settings have no such key at all (every job created before platform_0020) reads and prices as opted in
	old := e.newUser()
	e.fund(old, "m1", 1000)
	jOld := e.create(old, "m1", 5)
	e.exec(`UPDATE platform.jobs SET frozen_settings = '{"settings":{"unknown_choice":""}}'::jsonb WHERE id = $1`, jOld.ID)
	if got, err := e.jobs.Get(testCtx(t), old, jOld.ID); err != nil || got.Settings.VendorOptOut {
		t.Fatalf("a job with no key reads %+v (%v), want opted in", got.Settings, err)
	}
	if p := e.preview(old, jOld.ID); p.UnitCost != 2 || p.VendorOptOut {
		t.Errorf("a job with no key previews at %+v, want opted in at 2 per row", p)
	}
	e.requireBooks("after the confirms")
}
