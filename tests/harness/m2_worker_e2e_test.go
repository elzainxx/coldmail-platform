//go:build harness && m2worker

package harness

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 3, chunks B9, B10, B11 and B13 for module 2: the platform's module 2 ADAPTER (internal/adapters/m2 over internal/adapters/sendapi) driven end to end against the real worker stack of
// m2_worker_test.go (module 2's Control and worker, the controllable sink as its SMTP target). Four tests, in the order they run:
//
//	TestM2Worker_AdapterEndToEndSettlesItemByItem            B9   one campaign of 10: quote, reserve, run, poll, classify, settle, replay; sent, bounced and exhausted items, each compared three ways
//	TestM2Worker_AdapterPaginationAcrossPageBoundaries       B10  42 items read at seven page sizes whose boundaries fall at the start, the middle and the end; two look-ahead walkers as the control
//	TestM2Worker_AdapterCrossTenantReadsAndCancelAreRefused  B11  tenant B asks for tenant A's campaign, jobs and cancel through its own adapter; A's own adapter gets A's rows (control)
//	TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve  B13 a held campaign cancelled through the adapter: nothing new is sent, the in-flight item is settled once, the rest are free
//
// Helpers and the oracle: m2_worker_e2e_helpers_test.go. Evidence lines start with `NOTE e2e`. They run beside the five tests of m2_worker_test.go on the same worker (one job at a time), so each
// finishes its own campaign and leaves the sink idle before it ends (the cleanups of prepare).

// TestM2Worker_AdapterEndToEndSettlesItemByItem (B9): tenant A runs ONE campaign of ten recipients through the adapter and the platform's money core settles it. The recipients drive every outcome
// the sink can produce except cancel (B13): six accepted (sent), two rejected with 550 (bounced), two deferred with 451 (failed / attempts_exhausted at the module's own cap). The steps and what
// each proves:
//
//	Quote           the price is the recipient count times the module's own unit price (read through its operator API) and times the platform's dearest unit of module 2
//	reserve         the money core holds that many credits at the dearest frozen price, as the job engine will
//	Run             every item is a NEW job (202 queued) keyed by a job item id that exists in public.job_items
//	poll            the adapter's campaign report is read until module 2 says complete AND every submitted item is terminal
//	(a)             every item is reported exactly once, in the order it was run, with the (state, reason, error class) of its kind
//	(b)             the sink saw what the report says: one accepted message per sent item, one rejected RCPT per bounced item, as many deferrals as the module's cap per exhausted item
//	(c)             Classify, then ONE settle: the ledger holds exactly one charge per sent and per bounced item at the job's frozen price and nothing for the free ones, equal to what BOTH charge.Decide and a
//	                hand-written oracle say (m2eCompare), the settle result's counters equal the oracle's, and the unmapped and not-final lists are empty
//	(d)             the books: balance after the settle, the release of the remainder, the reserve row, balance_drift and reserve_overdrawn empty
//	(e)             the same settle again writes nothing (idempotent per business key); a second Run of an item is the replay (200) and sends nothing
//
// THE CONTROLS. The same comparison function (m2eCompare) is run on corrupted copies of the very data it just accepted: a settle outcome changed, an item dropped, an item duplicated, a ledger
// row dropped, duplicated, in the wrong unit or at the wrong price, a charge for a free item, a mapping that drifts from the hand oracle, an item that is not final in the settle list. Each must
// be reported, for the reason it names.
func TestM2Worker_AdapterEndToEndSettlesItemByItem(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	tn := r.newTenant(t, "e2e", []string{"ok", "bounce", "ok", "defer", "ok", "ok", "bounce", "ok", "defer", "ok"}, true)
	n := len(tn.Items)
	ad := e.client(t, tn)
	r.setMode(t, m2wMode{RejectRcpt: tn.emails("bounce"), RejectCode: 550, DeferRcpt: tn.emails("defer")})
	capN := int(e.cfgInt(t, "job_max_failed_attempts")) // the module's own number of failed attempts that ends a job
	wantSent, wantBounced, wantFailed := len(tn.emails("ok")), len(tn.emails("bounce")), len(tn.emails("defer"))
	if n < 8 || wantSent < 2 || wantBounced < 1 || wantFailed < 1 {
		t.Fatalf("the scenario has %d item(s): %d sent, %d bounced, %d exhausted; it needs at least 8 with each outcome", n, wantSent, wantBounced, wantFailed)
	}

	// ---- Quote: the module's rule and the platform's price table ----
	q, err := ad.Quote(ctx, int64(n))
	if err != nil {
		t.Fatalf("Quote through the adapter failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	moduleUnit := e.cfgInt(t, "unit_price_credits") // module 2's price per recipient, a live dial
	table := e.prices(t, "")                        // the platform's current price of each unit of module 2
	var dearest int64
	for _, c := range table {
		if c > dearest {
			dearest = c
		}
	}
	if q.RecipientCount != int64(n) || q.UnitPriceCredits != moduleUnit || q.Cost != int64(n)*moduleUnit || q.Currency != "credits" {
		t.Errorf("the quote is %+v, want %d recipients at module 2's own unit price %d (cost %d, currency credits)", q, n, moduleUnit, int64(n)*moduleUnit)
	}
	if q.Cost != int64(n)*dearest {
		t.Errorf("module 2 quotes %d credits for %d recipients but the platform's dearest unit of module 2 (%v) makes the reserve %d: the module's price dial and the platform's price table have drifted apart", q.Cost, n, table, int64(n)*dearest)
	}
	t.Logf("NOTE e2e quote: %d recipients -> %d credits (unit %d, tier %s); module 2's own unit_price_credits %d; platform price table %v, dearest %d", n, q.Cost, q.UnitPriceCredits, q.Tier, moduleUnit, table, dearest)

	// ---- the money: grant, a platform job, a reserve at the dearest frozen price ----
	job, rsv := e.fundAndReserve(t, tn, n)
	if rsv.Reserved != q.Cost || rsv.Reserved != int64(n)*int64(rsv.UnitCost) || int64(rsv.UnitCost) != dearest {
		t.Errorf("the reserve holds %d credits at %d per unit (%s), want the quoted %d = %d units at the dearest unit price %d", rsv.Reserved, rsv.UnitCost, rsv.Unit, q.Cost, n, dearest)
	}
	frozen := e.prices(t, job)
	t.Logf("NOTE e2e money: granted %d credits; reserved %d for %d unit(s) at %d (%s); the job's frozen prices %v", m2eGrant, rsv.Reserved, n, rsv.UnitCost, rsv.Unit, frozen)

	// ---- Run, poll ----
	took := e.runAll(t, ad, tn)
	t.Logf("NOTE e2e run: %d new jobs (202 queued, class pending) in %s", n, took.Round(time.Millisecond))
	rep := e.waitReport(t, ad, tn, 120*time.Second, nil)
	t.Logf("NOTE e2e report: %s, %d page(s), progress terminal=%d complete=%v, items match total %v", m2eTotalsLine(rep.Totals), rep.Pages, rep.Progress.Terminal, rep.Progress.IsComplete, rep.ItemsMatchTotal())

	// ---- (a) every item once, with the state, reason and error class of its kind ----
	m2eProblems(t, "(a) the report against what was submitted", m2eKinds(tn, rep.Items))
	if rep.Totals.Total != int64(n) || rep.Totals.Sent != int64(wantSent) || rep.Totals.Bounced != int64(wantBounced) || rep.Totals.Failed != int64(wantFailed) ||
		rep.Totals.Cancelled != 0 || rep.Totals.InFlight != 0 || rep.Totals.Pending != 0 || rep.Totals.WaitingForCapacity != 0 {
		t.Errorf("(a) the totals are (%s), want total %d sent %d bounced %d failed %d and nothing else", m2eTotalsLine(rep.Totals), n, wantSent, wantBounced, wantFailed)
	}
	for _, kind := range []string{"ok", "bounce", "defer"} { // the single-job report names the same job the run answered
		var first *m2wItem
		for _, it := range tn.Items {
			if it.Kind == kind {
				first = it
				break
			}
		}
		one, err := ad.ReportJob(ctx, first.ID)
		if err != nil {
			t.Errorf("(a) ReportJob of %s item %02d failed (%s): %s", kind, first.Index, modclient.KindOf(err), r.scrub(err.Error()))
			continue
		}
		w := m2eWantByKind[kind]
		if one.JobID != first.JobID || one.JobItemID != first.ID || one.State != w.State || m2eText(one.Reason) != w.Reason || m2eText(one.ErrorClass) != w.ErrorClass {
			t.Errorf("(a) the single-job report of %s item %02d is job %s %s/%s/%s, want job %s %s/%s/%s", kind, first.Index, one.JobID, one.State, m2eText(one.Reason), m2eText(one.ErrorClass), first.JobID, w.State, w.Reason, w.ErrorClass)
		}
	}

	// ---- (b) the sink saw what the report says ----
	view := r.sinkView(t)
	e.sinkAgainstReport(t, "(b) the sink against the report", tn, rep.Items, view, capN)
	byID := map[string]sendapi.Item{}
	for _, it := range rep.Items {
		byID[it.JobItemID] = it
	}
	if got := len(view.Msgs); got != wantSent {
		t.Errorf("(b) the sink holds messages for %d recipient(s) of this run, want exactly the %d sent", got, wantSent)
	}

	// ---- (c) classify, settle, compare with the ledger ----
	cl, err := ad.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify refused the report: %s", r.scrub(err.Error()))
	}
	if len(cl.Settle) != n || len(cl.NotYet) != 0 || len(cl.Unmapped) != 0 {
		t.Fatalf("Classify sorted %d item(s) into settle %d, not-yet %v, unmapped %v, want all %d settled (a terminal item is charged or free) and nothing else", n, len(cl.Settle), cl.NotYet, cl.Unmapped, n)
	}
	res, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	if err != nil {
		t.Fatalf("Settle refused the adapter's list: %v", err)
	}
	ledger := e.ledger(t, job)
	m2eProblems(t, "(c) the report, the settle list and the ledger", m2eCompare(rep.Items, cl.Settle, ledger, frozen, m2eDecide))
	wantCredits := int64(wantSent)*frozen["m2_accepted"] + int64(wantBounced)*frozen["m2_bounced"]
	if res.Charged != wantSent+wantBounced || res.Credits != wantCredits || res.Free != wantFailed || res.AlreadySettled != 0 || res.NotFinal != 0 || len(res.Unmapped) != 0 ||
		res.ByUnit["m2_accepted"] != wantSent || res.ByUnit["m2_bounced"] != wantBounced {
		t.Errorf("(c) the first settle wrote charged=%d credits=%d free=%d already=%d notfinal=%d unmapped=%v by unit %v, want charged %d (sent %d + bounced %d) credits %d free %d and nothing else", res.Charged, res.Credits, res.Free, res.AlreadySettled, res.NotFinal, res.Unmapped, res.ByUnit, wantSent+wantBounced, wantSent, wantBounced, wantCredits, wantFailed)
	}
	var ledgerCredits int64
	for _, c := range ledger {
		ledgerCredits += c.Units * c.Cost
	}
	if len(ledger) != wantSent+wantBounced || ledgerCredits != wantCredits {
		t.Errorf("(c) the ledger holds %d charge(s) worth %d credits, want %d worth %d", len(ledger), ledgerCredits, wantSent+wantBounced, wantCredits)
	}
	rows := map[string][]m2eCharge{}
	for _, c := range ledger {
		rows[c.ItemRef] = append(rows[c.ItemRef], c)
	}
	for _, it := range tn.Items {
		rp := byID[it.ID]
		o := charge.Outcome{Module: "m2", State: rp.State, Reason: m2eReason(rp.Reason)}
		d, h := m2eDecide(o), m2eHand(rp.State)
		c := view.Rcpt[it.Email]
		t.Logf("NOTE e2e item %02d %-6s %s: job %s reported %s/%s/%s (attempt_count %d, advisory); sink RCPT accepted=%d rejected=%d deferred=%d messages=%d; charge.Decide %s %q, hand oracle %s %q; ledger entries %d", it.Index, it.Kind, it.Email, it.JobID, rp.State, m2eText(rp.Reason), m2eText(rp.ErrorClass), rp.AttemptCount, c.Accepted, c.Rejected, c.Deferred, view.messages(it.Email), d.Kind, d.Unit, h.Kind, h.Unit, len(rows[it.ID]))
	}
	t.Logf("NOTE e2e settle: charged %d item(s) = %d credits (%v), free %d (no ledger row: money.Settle writes nothing for a free item), unmapped %d, not final %d; the ledger holds %d charge row(s) = %d credits", res.Charged, res.Credits, res.ByUnit, res.Free, len(res.Unmapped), res.NotFinal, len(ledger), ledgerCredits)

	// ---- (d) the books, then (e) the replay ----
	bal := e.balance(t, tn)
	if bal.Available != m2eGrant-rsv.Reserved || bal.Reserved != rsv.Reserved-wantCredits {
		t.Errorf("(d) after the settle the balance is available %d reserved %d, want available %d (grant %d - reserve %d) reserved %d (reserve - charged %d)", bal.Available, bal.Reserved, m2eGrant-rsv.Reserved, m2eGrant, rsv.Reserved, rsv.Reserved-wantCredits, wantCredits)
	}
	before := e.fingerprint(t, tn)
	again, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	if err != nil {
		t.Fatalf("(e) settling the same adapter output again was refused: %v", err)
	}
	if again.Charged != 0 || again.Credits != 0 || again.AlreadySettled != wantSent+wantBounced || again.Free != wantFailed || again.NotFinal != 0 || len(again.Unmapped) != 0 {
		t.Errorf("(e) settling again wrote charged=%d credits=%d already-settled=%d free=%d notfinal=%d unmapped=%v, want charged 0 credits 0 already-settled %d free %d", again.Charged, again.Credits, again.AlreadySettled, again.Free, again.NotFinal, again.Unmapped, wantSent+wantBounced, wantFailed)
	}
	if after := e.fingerprint(t, tn); after != before {
		t.Errorf("(e) the books of %s changed although the same settle was replayed (ledger count / newest id / balance / reserves %s -> %s)", tn.Name, before, after)
	}
	if len(e.ledger(t, job)) != len(ledger) {
		t.Errorf("(e) the ledger of the job grew from %d to %d charge row(s) on a replay", len(ledger), len(e.ledger(t, job)))
	}
	replay, err := ad.Run(ctx, tn.Items[0].ID) // the module's own idempotency: 200, the same job, its current state; nothing is sent again
	if err != nil || replay.Created || replay.JobID != tn.Items[0].JobID || replay.State != "sent" || replay.Class != sendapi.ClassTerminal {
		t.Errorf("(e) a second Run of item 01 answered %+v (error %v), want the same job %s, not created, state sent, class terminal", replay, err, tn.Items[0].JobID)
	}
	e.setStatus(t, job, "succeeded")
	rel, err := e.store.Release(ctx, job)
	if err != nil {
		t.Fatalf("(d) release of the job's reserve was refused: %v", err)
	}
	if rel.Released != rsv.Reserved-wantCredits {
		t.Errorf("(d) the release returned %d credits, want reserve %d - charged %d = %d", rel.Released, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits)
	}
	bal = e.balance(t, tn)
	if bal.Available != m2eGrant-wantCredits || bal.Reserved != 0 {
		t.Errorf("(d) after the release the balance is available %d reserved %d, want available %d (grant - charged) reserved 0", bal.Available, bal.Reserved, m2eGrant-wantCredits)
	}
	if rs, ch, rl := e.reserveRow(t, job); rs != rsv.Reserved || ch != wantCredits || rl != rsv.Reserved-wantCredits {
		t.Errorf("(d) platform.reserves holds reserved %d charged %d released %d, want %d, %d, %d", rs, ch, rl, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits)
	}
	e.requireBooks(t, "(d) after the release")
	if v2 := r.sinkView(t); len(v2.Msgs) != len(view.Msgs) {
		t.Errorf("(e) the sink holds messages for %d recipient(s) after the replays, %d before: a replay sent again", len(v2.Msgs), len(view.Msgs))
	} else {
		for _, it := range tn.Items {
			if v2.Rcpt[it.Email] != view.Rcpt[it.Email] || v2.messages(it.Email) != view.messages(it.Email) {
				t.Errorf("(e) the sink's counts of %s moved after the replays (RCPT %+v -> %+v)", it.Email, view.Rcpt[it.Email], v2.Rcpt[it.Email])
			}
		}
	}
	t.Logf("NOTE e2e books: after the release available %d reserved 0; platform.reserves %d/%d/%d; replay wrote charged 0, already-settled %d, free %d; platform.balance_drift and platform.reserve_overdrawn are empty", bal.Available, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits, again.AlreadySettled, again.Free)

	// ---- the controls: the SAME comparison on corrupted copies of what it just accepted ----
	copySettle := func() []money.SettleItem { return append([]money.SettleItem(nil), cl.Settle...) }
	copyLedger := func() []m2eCharge { return append([]m2eCharge(nil), ledger...) }
	copyItems := func() []sendapi.Item { return append([]sendapi.Item(nil), rep.Items...) }
	firstOf := func(state string) int {
		for i, s := range cl.Settle {
			if s.Outcome.State == state {
				return i
			}
		}
		t.Fatalf("no %s item in the settle list: the control cannot be built", state)
		return -1
	}
	firstLedger := func(unit string) int {
		for i, c := range ledger {
			if c.Unit == unit {
				return i
			}
		}
		t.Fatalf("no %s charge in the ledger: the control cannot be built", unit)
		return -1
	}
	type corruption struct {
		name, want string
		settle     []money.SettleItem
		ledger     []m2eCharge
		items      []sendapi.Item
		decide     func(charge.Outcome) m2eDecision
	}
	var cs []corruption
	{
		s := copySettle()
		s[firstOf("sent")].Outcome.State = "failed" // one outcome changed
		cs = append(cs, corruption{name: "a settle outcome changed (sent -> failed)", want: "carries outcome", settle: s})
	}
	{
		s := copySettle()
		i := firstOf("bounced")
		cs = append(cs, corruption{name: "a settle item dropped (a bounced one)", want: "lacks the terminal item", settle: append(s[:i:i], s[i+1:]...)})
	}
	{
		s := copySettle()
		cs = append(cs, corruption{name: "a settle item duplicated", want: "repeats", settle: append(s, s[firstOf("sent")])})
	}
	{
		l := copyLedger()
		i := firstLedger("m2_accepted")
		cs = append(cs, corruption{name: "a ledger charge dropped", want: "must be charged exactly once", ledger: append(l[:i:i], l[i+1:]...)})
	}
	{
		l := copyLedger()
		cs = append(cs, corruption{name: "a ledger charge duplicated", want: "must be charged exactly once", ledger: append(l, l[firstLedger("m2_bounced")])})
	}
	{
		l := copyLedger()
		l[firstLedger("m2_accepted")].Unit = "m2_bounced"
		cs = append(cs, corruption{name: "a ledger charge in the wrong unit", want: "is charged as unit", ledger: l})
	}
	{
		l := copyLedger()
		l[firstLedger("m2_bounced")].Cost++
		cs = append(cs, corruption{name: "a ledger charge at the wrong price", want: "frozen price", ledger: l})
	}
	{
		l := copyLedger()
		var free string
		for _, it := range rep.Items {
			if it.State == "failed" {
				free = it.JobItemID
				break
			}
		}
		cs = append(cs, corruption{name: "a charge for a free (failed) item", want: "is free but the ledger holds", ledger: append(l, m2eCharge{ItemRef: free, Unit: "m2_accepted", Cost: frozen["m2_accepted"], Units: 1})})
	}
	cs = append(cs, corruption{name: "a mapping that drifts from the table (bounced treated as free)", want: "the hand oracle says", decide: func(o charge.Outcome) m2eDecision {
		d := m2eDecide(o)
		if o.State == "bounced" {
			return m2eDecision{Kind: "free"}
		}
		return d
	}})
	{
		it := copyItems()
		for i := range it {
			if it[i].State == "sent" {
				it[i].State = "sending" // the module reports an item that is not final, while the settle list still carries it as sent
				break
			}
		}
		cs = append(cs, corruption{name: "an item that is not final in the settle list", want: "which is notfinal", items: it})
	}
	var caught []string
	for _, c := range cs {
		settle, led, items, dec := c.settle, c.ledger, c.items, c.decide
		if settle == nil {
			settle = cl.Settle
		}
		if led == nil {
			led = ledger
		}
		if items == nil {
			items = rep.Items
		}
		if dec == nil {
			dec = m2eDecide
		}
		got := m2eCompare(items, settle, led, frozen, dec)
		if len(got) == 0 {
			t.Errorf("control: m2eCompare accepted a corrupted input (%s): the comparison cannot fail", c.name)
			continue
		}
		if !strings.Contains(strings.Join(got, "\n"), c.want) {
			t.Errorf("control: m2eCompare rejected %q but not for the reason %q: %v", c.name, c.want, got)
			continue
		}
		caught = append(caught, c.name)
	}
	t.Logf("NOTE e2e controls: m2eCompare reported %d of %d corruptions, each for its own reason: %s", len(caught), len(cs), strings.Join(caught, "; "))
}

// TestM2Worker_AdapterPaginationAcrossPageBoundaries (B10): the campaign report of 42 sent items read through the adapter at seven page sizes. The page sizes put a page boundary at every item
// (1), at uneven points (5: 9 pages, the last of 2; 13: the last of 3), exactly in the middle (21), one item short of the end (41: a last page of one), exactly at the end (42: one page that is the
// whole campaign) and beyond it (100). For each size the walk must deliver ceil(42/size) pages of that size (the last the remainder), each page naming the size the module says it used and a next
// cursor on every page but the last, and the concatenation must be EXACTLY the baseline read at the module's largest page: every (job item id, state, reason) once, in the order the items were run
// (sendapitest.ExactSet). The single-job reports of the same items are compared with it too.
//
// THE CONTROLS. Two walkers with the look-ahead cursor bug, written over ReportCampaignPage, run against the real module at page size 5, and the SAME equality check must report every item they
// lose (one drops the last item of each page, believing it is the look-ahead row; the other reads a page ahead to learn its cursor and discards it); and the check must report a duplicated item and a
// reordered one.
func TestM2Worker_AdapterPaginationAcrossPageBoundaries(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	const n = 42
	kinds := make([]string, n)
	for i := range kinds {
		kinds[i] = "ok"
	}
	tn := r.newTenant(t, "page", kinds, true)
	ad := e.client(t, tn)
	took := e.runAll(t, ad, tn)
	t.Logf("NOTE e2e run: %d new jobs through the adapter in %s", n, took.Round(time.Millisecond))
	rep := e.waitReport(t, ad, tn, 180*time.Second, nil)
	m2eProblems(t, "the baseline report (page size 200)", m2eKinds(tn, rep.Items))
	base := m2eKeys(rep.Items)
	t.Logf("NOTE e2e baseline: %d item(s) in %d page(s) at the module's largest page size; %s", len(rep.Items), rep.Pages, m2eTotalsLine(rep.Totals))

	// the single-job reports of the same items equal the campaign report's rows
	var singles []string
	for _, it := range tn.Items {
		one, err := ad.ReportJob(ctx, it.ID)
		if err != nil {
			t.Fatalf("ReportJob of item %02d through the adapter failed (%s): %s", it.Index, modclient.KindOf(err), r.scrub(err.Error()))
		}
		if one.JobID != it.JobID {
			t.Errorf("the single-job report of item %02d names job %s, Run answered %s", it.Index, one.JobID, it.JobID)
		}
		singles = append(singles, m2eRowKey(one))
	}
	m2eProblems(t, "the single-job reports against the campaign report", sendapitest.ExactSet(singles, base))
	t.Logf("NOTE e2e singles: %d single-job reports equal the campaign report's (id, state, reason) rows, in order", len(singles))

	sizes := []int{1, 5, 13, 21, 41, 42, 100}
	for _, ps := range sizes {
		pg := e.pager(t, tn, ps)
		var pages []sendapi.CampaignPage
		st, err := pg.Walk(ctx, tn.CampaignID, func(p sendapi.CampaignPage) error { pages = append(pages, p); return nil })
		if err != nil {
			t.Fatalf("the walk at page size %d failed (%s): %s", ps, modclient.KindOf(err), r.scrub(err.Error()))
		}
		wantPages := (n + ps - 1) / ps
		var got []string
		var shape []string
		var boundaries []string
		seen := 0
		for i, p := range pages {
			last := i == len(pages)-1
			wantLen := ps
			if last {
				wantLen = n - ps*(wantPages-1)
			}
			if p.PageSize != ps {
				t.Errorf("page size %d: page %d says the module used page size %d", ps, i+1, p.PageSize)
			}
			if len(p.Items) != wantLen {
				t.Errorf("page size %d: page %d holds %d item(s), want %d", ps, i+1, len(p.Items), wantLen)
			}
			if last != (p.NextCursor == "") {
				t.Errorf("page size %d: page %d of %d has next cursor %q: only the last page may have none", ps, i+1, len(pages), p.NextCursor)
			}
			if p.Totals.Total != n {
				t.Errorf("page size %d: page %d reports total %d, want %d", ps, i+1, p.Totals.Total, n)
			}
			seen += len(p.Items)
			if !last && len(boundaries) < 6 {
				boundaries = append(boundaries, fmt.Sprint(seen))
			}
			shape = append(shape, fmt.Sprint(len(p.Items)))
			got = append(got, m2eKeys(p.Items)...)
		}
		if len(pages) != wantPages || st.Pages != wantPages || st.Items != n {
			t.Errorf("page size %d: the walk read %d page(s) and %d item(s) (stats %+v), want %d page(s) and %d item(s)", ps, len(pages), seen, st, wantPages, n)
		}
		m2eProblems(t, fmt.Sprintf("page size %d against the baseline", ps), sendapitest.ExactSet(got, base))
		if len(shape) > 12 {
			shape = append(shape[:6], append([]string{"..."}, shape[len(shape)-2:]...)...)
		}
		b := strings.Join(boundaries, " ")
		if wantPages > 7 {
			b += " ..."
		}
		t.Logf("NOTE e2e page size %3d: %2d page(s) of [%s], boundaries after item(s) [%s]; (id, state, reason) of all %d items equal the baseline, in order", ps, len(pages), strings.Join(shape, " "), b, len(got))
	}

	// ---- the controls ----
	pg5 := e.pager(t, tn, 5)
	nonFinal := (n+4)/5 - 1 // pages that have a successor at page size 5
	look, err := m2eLookAheadWalk(ctx, pg5, tn.CampaignID)
	if err != nil {
		t.Fatalf("the look-ahead walker (control) failed to read: %s", r.scrub(err.Error()))
	}
	lp := sendapitest.ExactSet(m2eKeys(look), base)
	if missing := countPrefix(lp, "missing "); missing != n-len(look) || missing != nonFinal || missing == 0 || countPrefix(lp, "unexpected ") != 0 {
		t.Errorf("control: the equality check reported %d missing item(s) for the look-ahead walker that dropped %d (one per page that has a successor: %d): %v", missing, n-len(look), nonFinal, lp)
	}
	skip, err := m2eSkipAheadWalk(ctx, pg5, tn.CampaignID)
	if err != nil {
		t.Fatalf("the skip-ahead walker (control) failed to read: %s", r.scrub(err.Error()))
	}
	sp := sendapitest.ExactSet(m2eKeys(skip), base)
	if missing := countPrefix(sp, "missing "); missing != n-len(skip) || missing == 0 || countPrefix(sp, "unexpected ") != 0 {
		t.Errorf("control: the equality check reported %d missing item(s) for the skip-ahead walker that lost %d: %v", missing, n-len(skip), sp)
	}
	dup := sendapitest.ExactSet(append(append([]string(nil), base...), base[0]), base)
	if len(dup) == 0 || !strings.Contains(strings.Join(dup, "\n"), "appears 2 times") {
		t.Errorf("control: the equality check did not report an item that appears twice: %v", dup)
	}
	swapped := append([]string(nil), base...)
	swapped[3], swapped[4] = swapped[4], swapped[3]
	if ro := sendapitest.ExactSet(swapped, base); len(ro) == 0 || !strings.Contains(strings.Join(ro, "\n"), "the order differs") {
		t.Errorf("control: the equality check did not report two items out of order: %v", ro)
	}
	t.Logf("NOTE e2e controls: at page size 5 the look-ahead walker delivered %d of %d items and the skip-ahead walker %d: the same equality check reported %d and %d missing; a duplicate and a reordered pair are reported too", len(look), n, len(skip), n-len(look), n-len(skip))
}

func countPrefix(ss []string, prefix string) int {
	n := 0
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// TestM2Worker_AdapterCrossTenantReadsAndCancelAreRefused (B11): tenant A runs a campaign of five (the first two held for 3 s at the sink, so the campaign is still in flight), and tenant B, through
// an adapter bound to B, asks module 2 for it. What module 2's code answers (research m2 section 3, read at platformapi/campaign_report.go and the single-report handler) and what is asserted:
//
//	campaign report of A's campaign   200 with total 0, no items, is_complete false (an unknown, foreign or mistyped campaign id is NOT a 404): NothingKnown, the totals equal the zero value
//	single-job report by job item id  404 not found, naming the field job_item_id (the same answer for a job of another tenant, a typo and a job never run)
//	single-job report by job id       404 not found, naming the field job_id
//	Run of A's job item               404 not found (no such job item FOR THIS TENANT); nothing is created: A's own replay of the item is the 200 of the job it already had
//	Cancel of A's campaign            200 with {0, 0, 0}: Empty; A's campaign is not touched
//
// and that no item id, job id, address, user, pool or account id of A is in any of B's answers (m2eLeaks over every answer's text). THE CONTROLS: the same three reads through A's adapter return A's
// five rows and A's jobs, the leak scan finds A's ids in A's own answer (so it can fail), and, after B's attempted cancel, A's campaign still completes: all five sent, none cancelled, the sink
// receives every one of A's messages once. Then B runs a campaign of its own (it works for B), and A's adapter asks for B's campaign and job items and gets the same refusals.
func TestM2Worker_AdapterCrossTenantReadsAndCancelAreRefused(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	a := r.newTenant(t, "tena", []string{"hold", "hold", "ok", "ok", "ok"}, true)
	b := r.newTenant(t, "tenb", []string{"ok", "ok", "ok"}, true)
	adA, adB := e.client(t, a), e.client(t, b)
	r.setMode(t, m2wMode{DataDelayMs: 3000, DelayRecipients: a.emails("hold")})
	e.runAll(t, adA, a)
	r.waitInflight(t, 1, 60*time.Second) // A's first held item is in flight: the campaign cannot be complete for at least 6 s

	secrets := m2eIDs(a)
	secrets["user"], secrets["pool"], secrets["account"] = a.UserID, a.PoolID, a.AccountID // not the campaign id: B asked with it, and the module echoes what it was asked
	var asked []string
	say := func(what, text string) {
		asked = append(asked, what)
		if leaked := m2eLeaks(text, secrets); len(leaked) != 0 {
			t.Errorf("B's answer to %s carries tenant A's %v: %s", what, leaked, text)
		}
	}

	// B -> A: the campaign report
	repB, err := adB.ReportCampaign(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("B's ReportCampaign of A's campaign failed (%s), want the module's empty 200: %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	if !repB.NothingKnown() || repB.Totals != (sendapi.Totals{}) || len(repB.Items) != 0 || repB.Progress.IsComplete || repB.Pages != 1 || repB.CampaignID != a.CampaignID {
		t.Errorf("B's report of A's campaign is %+v, want an empty 200: total 0, no item, not complete, one page", repB)
	}
	say("the campaign report", fmt.Sprintf("%+v", repB))

	// B -> A: the single-job reports, by job item id (every item of A) and by job id
	for _, it := range a.Items {
		_, err := adB.ReportJob(ctx, it.ID)
		if why := m2eNotFound(err, "job_item_id"); why != "" {
			t.Errorf("B's ReportJob of A's item %02d: %s (%v)", it.Index, why, err)
		}
		say(fmt.Sprintf("the single-job report of item %02d", it.Index), fmt.Sprint(err))
	}
	_, err = adB.ReportJobByID(ctx, a.Items[0].JobID)
	if why := m2eNotFound(err, "job_id"); why != "" {
		t.Errorf("B's ReportJobByID of A's job: %s (%v)", why, err)
	}
	say("the single-job report by job id", fmt.Sprint(err))

	// B -> A: run, and cancel
	_, err = adB.Run(ctx, a.Items[0].ID)
	if why := m2eNotFound(err, "job_item_id"); why != "" {
		t.Errorf("B's Run of A's job item: %s (%v)", why, err)
	}
	say("run", fmt.Sprint(err))
	before, err := adA.ReportCampaign(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("A's own report failed: %s", r.scrub(err.Error()))
	}
	if before.Progress.IsComplete || before.Totals.Sent >= int64(len(a.Items)) {
		t.Fatalf("A's campaign was already complete (%s) before B's cancel: the runner is too slow for the 3 s hold, so B's cancel would prove nothing", m2eTotalsLine(before.Totals))
	}
	pos, err := adB.Cancel(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("B's Cancel of A's campaign failed (%s), want the module's 200 with all zeros: %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	if !pos.Empty() {
		t.Errorf("B's Cancel of A's campaign answered %+v, want {0, 0, 0}", pos)
	}
	say("cancel", fmt.Sprintf("cancelled=%d in_flight=%d already_terminal=%d", pos.Cancelled, pos.InFlight, pos.AlreadyTerminal))

	// the controls: A's own adapter gets A's rows, and the leak scan can see them
	repA, err := adA.ReportCampaign(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("A's ReportCampaign failed: %s", r.scrub(err.Error()))
	}
	m2eProblems(t, "control: A's own campaign report", sendapitest.ExactSet(itemIDs(repA.Items), tenantItemIDs(a)))
	if repA.Totals.Total != int64(len(a.Items)) || repA.NothingKnown() {
		t.Errorf("control: A's own report says %s, want total %d", m2eTotalsLine(repA.Totals), len(a.Items))
	}
	for _, it := range a.Items {
		one, err := adA.ReportJob(ctx, it.ID)
		if err != nil || one.JobID != it.JobID {
			t.Errorf("control: A's ReportJob of item %02d answered job %q (error %v), want job %s", it.Index, one.JobID, err, it.JobID)
		}
	}
	if one, err := adA.ReportJobByID(ctx, a.Items[0].JobID); err != nil || one.JobID != a.Items[0].JobID {
		t.Errorf("control: A's ReportJobByID answered job %q (error %v), want %s", one.JobID, err, a.Items[0].JobID)
	}
	if replay, err := adA.Run(ctx, a.Items[0].ID); err != nil || replay.Created || replay.JobID != a.Items[0].JobID {
		t.Errorf("control: A's own replay of item 01 answered %+v (error %v), want the job it already had (B's attempt created nothing)", replay, err)
	}
	if leaked := m2eLeaks(fmt.Sprintf("%+v", repA), secrets); len(leaked) < len(a.Items) {
		t.Errorf("control: the leak scan found only %v in A's own report, want at least A's five item ids (a campaign item carries no job id or address): it cannot fail", leaked)
	}
	t.Logf("NOTE e2e cross-tenant (B asks for A): campaign report total=%d items=%d complete=%v pages=%d; ReportJob of A's %d items -> not found (job_item_id); ReportJobByID -> not found (job_id); Run -> not found; Cancel -> %+v (empty %v); %d answers scanned for A's ids, addresses, user, pool and account: none", repB.Totals.Total, len(repB.Items), repB.Progress.IsComplete, repB.Pages, len(a.Items), pos, pos.Empty(), len(asked))
	t.Logf("NOTE e2e cross-tenant control (A asks for A): %d item(s) of %s; ReportJob and ReportJobByID answer A's jobs; the leak scan finds %d of A's ids in A's own report", len(repA.Items), m2eTotalsLine(repA.Totals), len(m2eLeaks(fmt.Sprintf("%+v", repA), secrets)))

	// B's cancel changed nothing: A's campaign completes, all sent, and the sink receives every message of A once
	final := e.waitReport(t, adA, a, 90*time.Second, nil)
	m2eProblems(t, "A's campaign after B's cancel", m2eKindsAllSent(a, final.Items))
	if final.Totals.Sent != int64(len(a.Items)) || final.Totals.Cancelled != 0 {
		t.Errorf("A's campaign ended (%s) after B's cancel, want all %d sent and none cancelled", m2eTotalsLine(final.Totals), len(a.Items))
	}
	view := r.sinkView(t)
	e.sinkAgainstReport(t, "A after B's cancel", a, final.Items, view, int(e.cfgInt(t, "job_max_failed_attempts")))
	r.noteSink(t, "cross-tenant A", a, view)

	// B runs a campaign of its own through its adapter, and A's adapter is refused B's rows the same way
	e.runAll(t, adB, b)
	repBOwn := e.waitReport(t, adB, b, 60*time.Second, nil)
	m2eProblems(t, "B's own campaign through B's adapter", m2eKindsAllSent(b, repBOwn.Items))
	repAB, err := adA.ReportCampaign(ctx, b.CampaignID)
	if err != nil || !repAB.NothingKnown() {
		t.Errorf("A's report of B's campaign is %+v (error %v), want the empty 200", repAB, err)
	}
	if _, err := adA.ReportJob(ctx, b.Items[0].ID); m2eNotFound(err, "job_item_id") != "" {
		t.Errorf("A's ReportJob of B's item: %s (%v)", m2eNotFound(err, "job_item_id"), err)
	}
	if p, err := adA.Cancel(ctx, b.CampaignID); err != nil || !p.Empty() {
		t.Errorf("A's Cancel of B's (finished) campaign answered %+v (error %v), want {0, 0, 0}", p, err)
	}
	if after, err := adB.ReportCampaign(ctx, b.CampaignID); err != nil || after.Totals != repBOwn.Totals {
		t.Errorf("B's campaign changed after A's attempts: %+v (error %v), was %+v", after.Totals, err, repBOwn.Totals)
	}
	t.Logf("NOTE e2e cross-tenant, the other way: B's own campaign of %d item(s) finished through B's adapter (%s); A's adapter got the empty 200 for it, not found for its job item and {0,0,0} for a cancel", len(b.Items), m2eTotalsLine(repBOwn.Totals))
}

func itemIDs(items []sendapi.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.JobItemID
	}
	return out
}

func tenantItemIDs(tn *m2wTenant) []string {
	out := make([]string, len(tn.Items))
	for i, it := range tn.Items {
		out[i] = it.ID
	}
	return out
}

// m2eKindsAllSent is m2eKinds for a campaign whose every item must be sent (the kinds are only labels there: the sink accepts everything of it).
func m2eKindsAllSent(tn *m2wTenant, items []sendapi.Item) []string {
	p := sendapitest.ExactSet(itemIDs(items), tenantItemIDs(tn))
	for _, it := range items {
		if it.State != "sent" || it.Reason != nil || it.ErrorClass != nil {
			p = append(p, fmt.Sprintf("item %s is reported %s / %s / %s, want sent / <null> / <null>", it.JobItemID, it.State, m2eText(it.Reason), m2eText(it.ErrorClass)))
		}
	}
	sort.Strings(p)
	return p
}

// TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve (B13): a campaign of five whose sends the sink holds for 7 s (the held campaign of the control test), cancelled THROUGH THE ADAPTER while the
// first item is in flight, with the money core behind it:
//
//	Cancel #1 and #2   a position (cancelled + in_flight + already_terminal = 5, at least one in flight), and the second call, made at once, answers the same position (a no-op)
//	the window         from the instant the cancel returned until 3 s after the campaign is complete (far longer than module 2's 250 ms dispatch tick here, and than its default second), at every poll the
//	                   set of recipients the sink has been asked about is the in-flight one and no other: nothing NEW reached the sink
//	settle, round 1    straight after the cancel: Classify puts the four cancelled items in the settle list (free: rule M2-08) and the in-flight one in NotYet; the settle writes NO ledger row
//	settle, round 2    after the campaign is complete: the in-flight item is reported exactly once, as sent, with one message at the sink, and charged exactly once; the four others are cancelled with
//	                   reason cancelled and free; m2eCompare agrees with charge.Decide and the hand oracle; sent + bounced + failed + cancelled = total
//	the reserve        the platform job is cancelled and the release returns the credits reserved for the four cancelled items; balance_drift and reserve_overdrawn are empty
//	Cancel #3          after the end the position is {4 cancelled, 0 in flight, 1 terminal}; a replay of the settle writes nothing
//
// The paired control is TestM2Worker_ControlHeldCampaignAllSent (m2_worker_test.go): the same campaign and the same hold with no cancel ends with five messages at the sink.
func TestM2Worker_AdapterCancelSettlesFreeAndReleasesTheReserve(t *testing.T) {
	r := m2wNewRun(t)
	r.prepare(t)
	e := newM2eEnv(t, r)
	ctx := r.ctx

	tn := r.newTenant(t, "canc", m2wHeldKinds(), true)
	n := len(tn.Items)
	ad := e.client(t, tn)
	r.setMode(t, m2wMode{DataDelayMs: m2wHoldMs, DelayRecipients: tn.emails("")})
	q, err := ad.Quote(ctx, int64(n))
	if err != nil {
		t.Fatalf("Quote through the adapter failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	job, rsv := e.fundAndReserve(t, tn, n)
	frozen := e.prices(t, job)
	t.Logf("NOTE e2e quote and reserve: %d recipients quoted %d credits; reserved %d at %d (%s); frozen prices %v", n, q.Cost, rsv.Reserved, rsv.UnitCost, rsv.Unit, frozen)
	e.runAll(t, ad, tn)

	// one item reaches DATA and is held; the rest cannot start (one worker, one lane)
	held := r.waitInflight(t, 1, 60*time.Second)
	flying := map[string]bool{}
	for _, h := range held.Held {
		for _, a := range h.To {
			flying[strings.ToLower(a)] = true
		}
	}
	var flyingList []string
	for a := range flying {
		flyingList = append(flyingList, a)
	}
	sort.Strings(flyingList)
	known := r.known()
	for _, a := range flyingList {
		if it := known[a]; it == nil || !strings.HasPrefix(a, r.prefix()+tn.Name+"-") {
			t.Fatalf("the sink holds a send to %s: it is not an item of this campaign", a)
		}
	}
	if held.Held[0].RemainingMs < 3500 {
		t.Fatalf("the held send has only %d ms of its hold left: too late to cancel while it is in flight (the hold is %d ms; the runner is slow); nothing was asserted", held.Held[0].RemainingMs, m2wHoldMs)
	}
	if got := r.touched(r.sinkView(t), tn); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Errorf("before the cancel the sink has answered RCPT TO for %v, want exactly the in-flight %v (one worker, one lane: nothing else may have started)", got, flyingList)
	}

	// the platform's job is cancelling; the cancel goes through the adapter, twice
	e.setStatus(t, job, "cancelling")
	c1, err := ad.Cancel(ctx, tn.CampaignID)
	if err != nil {
		t.Fatalf("Cancel #1 through the adapter failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	cancelledAt := time.Now()
	touchedAtCancel := r.touched(r.sinkView(t), tn)
	c2, err := ad.Cancel(ctx, tn.CampaignID)
	if err != nil {
		t.Fatalf("Cancel #2 through the adapter failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	still := r.inflight(t)
	t.Logf("NOTE e2e cancel #1: cancelled=%d in_flight=%d already_terminal=%d; #2 straight after: cancelled=%d in_flight=%d already_terminal=%d; the sink holds %d session(s); before the cancel returned the sink had answered RCPT TO for %v", c1.Cancelled, c1.InFlight, c1.AlreadyTerminal, c2.Cancelled, c2.InFlight, c2.AlreadyTerminal, still.Inflight, touchedAtCancel)
	if c1.Cancelled+c1.InFlight+c1.AlreadyTerminal != int64(n) || c1.InFlight < 1 || c1.Cancelled < 1 {
		t.Errorf("the first cancel's position is %+v: its three numbers must add up to the %d items, with at least one in flight and one cancelled", c1, n)
	}
	if still.Inflight < 1 {
		t.Fatalf("the held send finished before the second cancel was made: the hold (%d ms) is too short for this runner; the comparison of the two cancels is not meaningful", m2wHoldMs)
	}
	if c2 != c1 {
		t.Errorf("a second cancel straight after answered %+v, the first %+v: it must be a no-op with the same position", c2, c1)
	}
	if strings.Join(touchedAtCancel, ",") != strings.Join(flyingList, ",") {
		t.Errorf("when the cancel returned the sink had answered RCPT TO for %v, want exactly the in-flight %v", touchedAtCancel, flyingList)
	}

	// settle, round 1: right after the cancel the four cancelled items are final and free, the in-flight one is not an outcome yet
	mid, err := ad.ReportCampaign(ctx, tn.CampaignID)
	if err != nil {
		t.Fatalf("the report right after the cancel failed: %s", r.scrub(err.Error()))
	}
	cl1, err := ad.Classify(mid.Items)
	if err != nil {
		t.Fatalf("Classify of the report right after the cancel: %s", r.scrub(err.Error()))
	}
	res1, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl1.Settle})
	if err != nil {
		t.Fatalf("settle round 1: %v", err)
	}
	if len(cl1.NotYet) != len(flyingList) || len(cl1.Settle) != n-len(flyingList) || len(cl1.Unmapped) != 0 || res1.Charged != 0 || res1.Free != n-len(flyingList) || len(res1.Unmapped) != 0 || len(e.ledger(t, job)) != 0 {
		t.Errorf("round 1 (straight after the cancel): Classify gave %d settle / %v not yet / %v unmapped and Settle charged %d, free %d, ledger rows %d; want %d free cancelled items settled, %d in flight not yet, nothing charged and no ledger row", len(cl1.Settle), cl1.NotYet, cl1.Unmapped, res1.Charged, res1.Free, len(e.ledger(t, job)), n-len(flyingList), len(flyingList))
	}
	t.Logf("NOTE e2e settle round 1 (straight after the cancel): %d cancelled item(s) -> Classify settle list (charge.Decide: free, rule M2-08), %d in flight -> not yet; Settle: charged %d, free %d, ledger rows %d", len(cl1.Settle), len(cl1.NotYet), res1.Charged, res1.Free, len(e.ledger(t, job)))

	// the window: nothing new reaches the sink until every item is terminal, and for 3 s after
	seenNew := map[string]bool{}
	sample := func(sendapi.CampaignReport) {
		now := r.sinkView(t)
		for _, it := range tn.Items {
			if now.Rcpt[it.Email].tried() > 0 && !flying[it.Email] && !seenNew[it.Email] {
				seenNew[it.Email] = true
				t.Errorf("%.1f s after the cancel returned the sink answered RCPT TO for %s: a send STARTED after the cancel (%+v)", time.Since(cancelledAt).Seconds(), it.Email, now.Rcpt[it.Email])
			}
		}
	}
	rep := e.waitReport(t, ad, tn, m2wHoldMs*time.Millisecond+90*time.Second, sample)
	drain := time.Now()
	for r.inflight(t).Inflight > 0 {
		if time.Since(drain) > 30*time.Second {
			t.Fatalf("the sink still holds a session 30 s after the campaign was complete")
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	end := r.sinkView(t)
	t.Logf("NOTE e2e the window: from the cancel's return to %.1f s after it, no recipient other than %v was tried (the dispatch tick is 250 ms here)", time.Since(cancelledAt).Seconds(), flyingList)
	if got := r.touched(end, tn); strings.Join(got, ",") != strings.Join(flyingList, ",") {
		t.Errorf("at the end the sink has answered RCPT TO for %v, want only the in-flight %v: something was sent after the cancel returned", got, flyingList)
	}

	// the end state through the adapter: the in-flight item exactly once as sent, the others cancelled, the invariant of the totals
	byID := map[string]sendapi.Item{}
	for _, it := range rep.Items {
		byID[it.JobItemID] = it
	}
	if len(rep.Items) != n || len(byID) != n {
		t.Errorf("the final report holds %d item(s) (%d distinct), want the %d submitted, each once", len(rep.Items), len(byID), n)
	}
	var nSent, nCancelled int
	var sentRef string
	for _, it := range tn.Items {
		rp, ok := byID[it.ID]
		switch {
		case !ok:
			t.Errorf("%s item %02d is not in the final report", tn.Name, it.Index)
		case flying[it.Email]:
			if rp.State != "sent" || rp.Reason != nil || rp.ErrorClass != nil || rp.ProviderMessageID == nil {
				t.Errorf("the in-flight item %02d is reported %s/%s/%s (provider message id set: %v), want sent/<null>/<null> with a provider message id: it finished and said so truthfully", it.Index, rp.State, m2eText(rp.Reason), m2eText(rp.ErrorClass), rp.ProviderMessageID != nil)
			}
			nSent++
			sentRef = it.ID
		default:
			if rp.State != "cancelled" || m2eText(rp.Reason) != "cancelled" || rp.ProviderMessageID != nil {
				t.Errorf("the item %02d that never started is reported %s/%s (provider message id set: %v), want cancelled/cancelled and no message id", it.Index, rp.State, m2eText(rp.Reason), rp.ProviderMessageID != nil)
			}
			nCancelled++
		}
	}
	tt := rep.Totals
	if tt.Total != int64(n) || tt.Sent != int64(len(flyingList)) || tt.Cancelled != int64(n-len(flyingList)) || tt.Bounced != 0 || tt.Failed != 0 || tt.InFlight != 0 || tt.Pending != 0 || tt.WaitingForCapacity != 0 ||
		tt.Sent+tt.Bounced+tt.Failed+tt.Cancelled != tt.Total {
		t.Errorf("the final totals are (%s), want sent %d + cancelled %d = total %d and nothing else (sent + bounced + failed + cancelled must equal the total)", m2eTotalsLine(tt), len(flyingList), n-len(flyingList), n)
	}
	e.sinkAgainstReport(t, "the sink against the report after the cancel", tn, rep.Items, end, int(e.cfgInt(t, "job_max_failed_attempts")))
	if end.messages(strings.ToLower(flyingList[0])) != 1 || len(end.Msgs) != 1 {
		t.Errorf("the sink holds messages for %d recipient(s), %d for the in-flight one, want exactly one message in all: the in-flight item reported exactly once", len(end.Msgs), end.messages(strings.ToLower(flyingList[0])))
	}

	// settle, round 2: everything is final; the in-flight item is charged exactly once, the cancelled are free
	cl2, err := ad.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify of the final report: %s", r.scrub(err.Error()))
	}
	if len(cl2.Settle) != n || len(cl2.NotYet) != 0 || len(cl2.Unmapped) != 0 {
		t.Fatalf("Classify of the final report gave %d settle / %v not yet / %v unmapped, want all %d settled", len(cl2.Settle), cl2.NotYet, cl2.Unmapped, n)
	}
	res2, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl2.Settle})
	if err != nil {
		t.Fatalf("settle round 2: %v", err)
	}
	ledger := e.ledger(t, job)
	m2eProblems(t, "the final report, the settle list and the ledger", m2eCompare(rep.Items, cl2.Settle, ledger, frozen, m2eDecide))
	wantCredits := frozen["m2_accepted"]
	if res2.Charged != 1 || res2.Credits != wantCredits || res2.Free != n-1 || res2.AlreadySettled != 0 || res2.NotFinal != 0 || len(res2.Unmapped) != 0 || len(ledger) != 1 || ledger[0].ItemRef != sentRef || ledger[0].Unit != "m2_accepted" {
		t.Errorf("round 2: Settle charged %d (%d credits), free %d, already %d, unmapped %v; the ledger holds %+v; want exactly one charge, the in-flight item %s at m2_accepted for %d credits, free %d, nothing else", res2.Charged, res2.Credits, res2.Free, res2.AlreadySettled, res2.Unmapped, ledger, sentRef, wantCredits, n-1)
	}
	unit := "none"
	if len(ledger) == 1 {
		unit = ledger[0].Unit
	}
	t.Logf("NOTE e2e settle round 2 (after the end): charged %d = %d credits (the in-flight item %s, unit %s), free %d (the cancelled: charge.Decide free, rule M2-08; money.Settle writes NO ledger row for a free item, so the cancelled items have none), unmapped %d; the ledger holds %d charge row(s)", res2.Charged, res2.Credits, sentRef, unit, res2.Free, len(res2.Unmapped), len(ledger))

	// the reserve of the cancelled items goes back
	before := e.fingerprint(t, tn)
	again, err := e.store.Settle(ctx, money.SettleReq{JobID: job, Items: cl2.Settle})
	if err != nil || again.Charged != 0 || again.Credits != 0 || again.AlreadySettled != 1 || again.Free != n-1 || e.fingerprint(t, tn) != before {
		t.Errorf("a replay of the settle answered %+v (error %v) and the books moved: want charged 0, already-settled 1, free %d and nothing written", again, err, n-1)
	}
	e.setStatus(t, job, "cancelled")
	rel, err := e.store.Release(ctx, job)
	if err != nil {
		t.Fatalf("release of the reserve of the cancelled job: %v", err)
	}
	if rel.Released != rsv.Reserved-wantCredits {
		t.Errorf("the release returned %d credits, want the reserve %d minus the one charge %d = %d (the credits held for the %d cancelled items)", rel.Released, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits, n-1)
	}
	bal := e.balance(t, tn)
	if bal.Available != m2eGrant-wantCredits || bal.Reserved != 0 {
		t.Errorf("after the release the balance is available %d reserved %d, want available %d reserved 0", bal.Available, bal.Reserved, m2eGrant-wantCredits)
	}
	if rs, ch, rl := e.reserveRow(t, job); rs != rsv.Reserved || ch != wantCredits || rl != rsv.Reserved-wantCredits {
		t.Errorf("platform.reserves holds reserved %d charged %d released %d, want %d, %d, %d", rs, ch, rl, rsv.Reserved, wantCredits, rsv.Reserved-wantCredits)
	}
	e.requireBooks(t, "after the cancelled job's release")

	// the third cancel, after the end: the final position
	c3, err := ad.Cancel(ctx, tn.CampaignID)
	if err != nil {
		t.Fatalf("Cancel #3 through the adapter failed (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
	}
	if want := int64(n - len(flyingList)); c3.Cancelled != want || c3.InFlight != 0 || c3.AlreadyTerminal != int64(len(flyingList)) {
		t.Errorf("a cancel after the end answered %+v, want cancelled %d, in flight 0, already terminal %d", c3, want, len(flyingList))
	}
	t.Logf("NOTE e2e after the end: %d sent (exactly one message at the sink, one ledger entry), %d cancelled (free, no ledger row), totals (%s); release returned %d credits (reserve %d - charged %d); balance available %d reserved 0; cancel #3 %+v; control: TestM2Worker_ControlHeldCampaignAllSent sends all %d of the same campaign without a cancel", nSent, nCancelled, m2eTotalsLine(tt), rel.Released, rsv.Reserved, wantCredits, bal.Available, c3, n)
}
