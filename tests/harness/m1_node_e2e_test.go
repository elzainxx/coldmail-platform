//go:build harness && m1node

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
	"github.com/coldmail-platform/platform/internal/norm"
)

// Phase 3, chunks B9, B10 and B11 for module 1: the platform's OWN adapter (internal/adapters/m1, built per tenant by Factory.ForScope from a tenant.Scope, the user's derived key
// provisioned with tenant.ProvisionM1Caller) against the real node stack, never through hand-written HTTP; the platform's own money core (internal/money) on the live database as the
// platform_app login. These tests run only behind the build tag m1node, in run.sh's scope m1node (stage M1N-4) and in the full harness once m1node is in FULL_INCLUDES.
//
// WHAT IS PROVED.
//
//	TestM1Node_E2EBulkSettleThroughAdapter   B9. Two real tenants. Tenant A submits ONE job with Prepare + Submit that holds one address per recipe of the shared table (every family the fixture can
//	                                         produce), a cache hit (the address of a warm-up job), the first address again, in another spelling, and the greylisted address again, and three malformed
//	                                         addresses of three different norm reasons. Status is polled to complete, Drain reads every page and each page is settled through the money core as it arrives (the
//	                                         malformed rows last, free under P-08). Asserted: (a) one result per sent row, in order, with the verdict its address must produce; (b) module 1's own tables hold
//	                                         exactly the sent rows and no malformed one; (c) every repeat carries duplicate_of and the address group is charged once; (d) the ledger equals the sum charge.Decide
//	                                         gives for the reported outcomes AND the sum of a hand-written oracle per recipe, item by item; (e) nothing unmapped, balance_drift and reserve_overdrawn empty, the
//	                                         balances equal the ledger, a second tenant's job beside it leaves the first one's books alone; (f) the fixture's SMTP counters show one verification (one target RCPT,
//	                                         one nx- probe) per distinct address: no repeat and no cache hit reached the node. Paired controls: the comparison re-run on a settle list with one outcome changed, one
//	                                         item dropped, one duplicated, a free repeat made a charge and the cache flag lost, and on a ledger with a charge dropped, a unit changed and a charge keyed by a
//	                                         position; and a job that SENDS the malformed rows, which the module-rows check reports
//	TestM1Node_E2EPaginationBoundaries       B10. ONE job of 220 rows (crossing the module's default page of 100 twice) drained at limits 0 (the default), 1, N/2, N-1, N, N+1 and the module's maximum: each
//	                                         is the same set of (item_id, status, reason, origin), equal to what the rows were built to be; the page counts and sizes are the look-ahead ones. Paired
//	                                         control: a walker with the look-ahead cursor bug, written here against Results, loses exactly one row per page and the same equality check reports it, at every limit
//	TestM1Node_E2ECrossTenant                B11. Tenant B asks, with an adapter built from B's key, for A's job: Status, Results (also with a garbage cursor), Drain all answer 404 not_found as a typed
//	                                         NotFound, no value and no row, the same answer as for a job that does not exist; A's own adapter on the same job succeeds (the control), B reads its own job, A
//	                                         cannot read B's; an adapter built for a real user whose key was never provisioned gets 401 unauthenticated as a typed Refused, and its Submit creates no job. No
//	                                         error text holds a job id, an address, a user id or a key
//	TestM1Node_E2EChecksBite                 pure (no stack): every comparison above is shown to bite on a broken input, the walker and the page arithmetic are run against a fake listing with the module's
//	                                         look-ahead rule, and Prepare agrees with the rows the tests build

const (
	m1eBulkSent      = 24 // rows of tenant A's job that are sent: 20 recipes, a cache hit and three repeats
	m1eBulkMalformed = 3
	m1ePageRows      = 220 // the pagination job: > 2 x the module's default page of 100
)

// ---- B9 ---------------------------------------------------------------------------------------------------------------------------------------------------------

// TestM1Node_E2EBulkSettleThroughAdapter: see the file comment.
func TestM1Node_E2EBulkSettleThroughAdapter(t *testing.T) {
	s := newM1eStack(t)
	a := s.newTenant("a", true)
	b := s.newTenant("b", true)
	t.Logf("NOTE B9 tenants: A is user %s and B is user %s: real auth.users and public.users rows, one derived key each (never printed), both provisioned with tenant.ProvisionM1Caller, one adapter each from Factory.ForScope", m1eShort(a.userID), m1eShort(b.userID))
	if a.userID == b.userID || a.key() == b.key() {
		t.Fatalf("two tenants share a user id or a key")
	}

	// the warm-up: A verifies one address on its own, so that the main job has an address already verified and cached to ask for again
	warm := fresh("e2ewarm") + "@valid.fixture.test"
	cacheBefore := s.cacheRows("valid.fixture.test")
	w0 := s.fx.seq()
	wID, _, err := a.ad.Submit(s.ctx, m1ad.Prepare([]m1ad.Input{{ItemRef: "warm-1", Address: warm}}))
	if err != nil {
		t.Fatalf("the warm-up job was refused (kind %v): %v", modclient.KindOf(err), err)
	}
	s.waitComplete(a, wID, 1, m1eJobWait)
	wRows, _ := s.drainAll(a.ad, wID)
	if len(wRows) != 1 || wRows[0].Status != "valid" || wRows[0].Reason != "accepted" || wRows[0].FromCache {
		t.Fatalf("the warm-up address came back %+v, want one fresh valid/accepted row", wRows)
	}
	if p := m1eCheckProbes(s.fx.sessions(w0, ""), s.nodeIP, []string{warm}); len(p) != 0 {
		t.Fatalf("the fixture's view of the warm-up job: %s", strings.Join(p, "; "))
	}
	s.waitCacheRow("valid.fixture.test", cacheBefore)
	t.Logf("NOTE B9 warm-up: the address %s was verified once (one target RCPT and one nx- probe at the fixture) and cached", warm)

	// the main job, end to end
	items := m1eBuildBulkItems(s.m1nEnv, warm)
	if got := len(m1eSent(items)); got != m1eBulkSent || len(items)-got != m1eBulkMalformed {
		t.Fatalf("the test builds %d sent and %d malformed rows, it was written for %d and %d", got, len(items)-got, m1eBulkSent, m1eBulkMalformed)
	}
	r := s.settleThrough(a, items, m1eGrantA)
	s.noteRun("B9 A", r)
	if problems := s.assess(r); len(problems) != 0 {
		t.Fatalf("%d difference(s) between tenant A's settled job and what its rows must be:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	prices := r.rsv.Prices
	want := m1eOracleTotals(items, prices)
	t.Logf("NOTE B9 A verdict: all %d sent rows are what their addresses must produce; the ledger holds %d charge row(s) for %d credit(s) (%s), the hand-written oracle and charge.Decide over the reported outcomes agree on that, %d item(s) free", len(m1eSent(items)), len(r.ledger), want.credits, want.byUnit, want.free)

	// PAIRED CONTROLS: the SAME comparison functions on broken inputs
	var caught []string
	for _, c := range m1eCorruptLists(items, r.list) {
		got := m1eCompareSettleList(items, c.list)
		if !m1eReports(got, c.ref, c.want) {
			t.Fatalf("control %q: the comparison of the settle list said %v, want a line naming %q and containing %q", c.name, got, c.ref, c.want)
		}
		caught = append(caught, c.name)
	}
	for _, c := range m1eCorruptLedgers(r.ledger) {
		if got := m1eCompareLedger(items, c.ledger, prices); !m1eContains(got, c.want) {
			t.Fatalf("control %q: the comparison of the ledger said %v, want a line containing %q", c.name, got, c.want)
		}
		caught = append(caught, "ledger: "+c.name)
	}
	t.Logf("NOTE B9 controls: the comparison reports every corruption of a settle list or a ledger by name: %s", strings.Join(caught, "; "))

	// the control of (b): the same kind of address SENT (SubmitRaw is the harness's door: Submit never sends a malformed row), so module 1 records it, and the check of its tables says so
	sent := []m1ad.Item{
		{ItemID: "c0", Address: fresh("e2ec") + "@nomx.fixture.test"},
		{ItemID: "c1", Address: "not-an-address"},
		{ItemID: "c2", Address: "first@second@valid.fixture.test"},
	}
	cID, cCounts, err := a.ad.SubmitRaw(s.ctx, sent)
	if err != nil {
		t.Fatalf("the raw control job was refused (kind %v): %v", modclient.KindOf(err), err)
	}
	s.waitDone(a, cID, m1eJobWait)
	cRows, cMal, _, _ := s.moduleJob(cID)
	cProblems := m1eCheckModuleRows(cRows, cMal, 1)
	if cCounts.Malformed != 2 || cRows != 3 || cMal != 2 || len(cProblems) == 0 || !strings.Contains(strings.Join(cProblems, ";"), "malformed") {
		t.Fatalf("the raw control job: the 202 counted %d malformed, module 1 holds %d row(s) of which %d malformed, the check said %v: sending malformed rows must be visible in module 1's tables and reported", cCounts.Malformed, cRows, cMal, cProblems)
	}
	t.Logf("NOTE B9 control (b): the same job sent WITH its malformed rows (SubmitRaw) leaves %d job_rows in module 1, %d malformed, and the check reports: %s", cRows, cMal, strings.Join(cProblems, "; "))

	// tenant B, while A's reserve is still held: its own job, its own books, and A's books do not move
	aAvail, aRes, _ := s.mny.bal(a.userID, "m1")
	rb := s.settleThrough(b, m1eBuildSmallItems(s.m1nEnv), m1eGrantB)
	s.noteRun("B9 B", rb)
	if problems := s.assess(rb); len(problems) != 0 {
		t.Fatalf("%d difference(s) between tenant B's settled job and what its rows must be:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if a2, r2, _ := s.mny.bal(a.userID, "m1"); a2 != aAvail || r2 != aRes {
		t.Fatalf("tenant A's balance moved from available %d reserved %d to %d / %d while tenant B settled its own job", aAvail, aRes, a2, r2)
	}
	for _, rr := range []*m1eRun{r, rb} {
		if n := s.mny.int(`SELECT count(*) FROM platform.ledger WHERE job_id = $1 AND user_id <> $2`, rr.job, rr.tn.userID); n != 0 {
			t.Fatalf("%d ledger row(s) of tenant %s's job belong to another user", n, rr.tn.name)
		}
	}
	if r.jobID == rb.jobID {
		t.Fatalf("the two tenants share a module job id")
	}

	// the end of both jobs: the unused remainder of each reserve comes back, once
	for _, rr := range []*m1eRun{r, rb} {
		tot := m1eOracleTotals(rr.items, rr.rsv.Prices)
		rel := s.release(rr)
		if rel.Released != rr.rsv.Reserved-tot.credits {
			t.Fatalf("tenant %s: the release returned %d, want reserve %d - charged %d = %d", rr.tn.name, rel.Released, rr.rsv.Reserved, tot.credits, rr.rsv.Reserved-tot.credits)
		}
		s.mny.requireBal("after the release of "+rr.tn.name, rr.tn.userID, "m1", rr.grant-tot.credits, 0)
		if got := s.mny.rows(`SELECT reserved, charged, released FROM platform.reserves WHERE reserve_id = $1`, rr.rsv.ReserveID); len(got) != 1 || got[0] != fmt.Sprintf("%d|%d|%d", rr.rsv.Reserved, tot.credits, rr.rsv.Reserved-tot.credits) {
			t.Fatalf("tenant %s: platform.reserves holds %v, want reserved %d, charged %d, released %d", rr.tn.name, got, rr.rsv.Reserved, tot.credits, rr.rsv.Reserved-tot.credits)
		}
		t.Logf("NOTE B9 books %s: granted %d, reserved %d (%d units at %d), charged %d credit(s), released %d, final available %d reserved 0", rr.tn.name, rr.grant, rr.rsv.Reserved, rr.rsv.Units, rr.rsv.UnitCost, tot.credits, rel.Released, rr.grant-tot.credits)
	}
	if p := s.viewProblems("at the end of B9"); len(p) != 0 {
		t.Fatalf("%s", strings.Join(p, "\n"))
	}
	n := s.mny.requireReservesMatchLedger("at the end of B9")
	t.Logf("NOTE B9 invariants: platform.balance_drift and platform.reserve_overdrawn are empty; %d reserve(s) of the database agree with the ledger", n)
}

// m1eRowProblems compares the delivered rows with the rows that were sent: (a) one result per sent row, in order, each the verdict its address must produce; (c) a repeat says which row it
// repeats and the adapter maps it to the platform's duplicate outcome, and a first row says nothing of the kind. It reads no database, so the pure test runs it on broken rows.
func m1eRowProblems(items []m1eItem, rows []m1ad.Row) []string {
	var p []string
	sent := m1eSent(items)
	ordOf := map[string]int{}
	for i, it := range sent {
		ordOf[it.ref] = i
	}
	// (a) one result per sent row, in order, each the verdict its address must produce; (c) a repeat says which row it repeats
	if len(rows) != len(sent) {
		p = append(p, fmt.Sprintf("(a) %d result row(s) for %d sent row(s)", len(rows), len(sent)))
	}
	for i, row := range rows {
		if i >= len(sent) {
			break
		}
		it := sent[i]
		if row.Ordinal != i || row.ItemID != it.ref {
			p = append(p, fmt.Sprintf("(a) result %d is ordinal %d item %q, want ordinal %d item %q", i, row.Ordinal, row.ItemID, i, it.ref))
			continue
		}
		if got := m1eVerdictOfRow(row); got != it.want {
			p = append(p, fmt.Sprintf("(a) %s (%s): module 1 reported %v, want %v", it.ref, it.label, got, it.want))
		}
		if it.kind == m1eKindDuplicate {
			first, ok := ordOf[it.firstRef]
			if row.DuplicateOf == nil || !ok || *row.DuplicateOf != first {
				p = append(p, fmt.Sprintf("(c) %s (%s): duplicate_of is %q, want ordinal %d, the first row of the address", it.ref, it.label, m1bDup(row), first))
			}
			if oc := row.Outcome(); oc != (charge.Outcome{Module: "platform", State: "duplicate"}) {
				p = append(p, fmt.Sprintf("(c) %s (%s): the adapter maps the repeat to %+v, want platform/duplicate", it.ref, it.label, oc))
			}
		} else if row.DuplicateOf != nil {
			p = append(p, fmt.Sprintf("(c) %s (%s): duplicate_of %s on a first row", it.ref, it.label, m1bDup(row)))
		}
	}

	return p
}

// assess is every assertion of B9 on one settled run, as a list of differences (empty: the job is exactly what its rows must be). The letters are the brief's.
func (s *m1eStack) assess(r *m1eRun) []string {
	var p []string
	items, sent := r.items, m1eSent(r.items)
	dups := m1eDuplicates(items)

	p = append(p, m1eRowProblems(items, r.rows)...)

	// (b) module 1's own tables hold exactly the sent rows and never a malformed one; the malformed rows are free under P-08
	rows, malformed, repeats, owner := s.moduleJob(r.jobID)
	p = append(p, prefixed("(b)", m1eCheckModuleRows(rows, malformed, len(sent)))...)
	if r.status.Counts.Total != len(sent) {
		p = append(p, fmt.Sprintf("(b) module 1's job status counts total %d, want the %d sent rows", r.status.Counts.Total, len(sent)))
	}
	if want := (m1ad.SubmitCounts{Total: len(sent), Enqueued: len(sent) - dups, Duplicate: dups}); r.counts != want {
		p = append(p, fmt.Sprintf("(b) the 202's counts are %+v, want %+v", r.counts, want))
	}
	if repeats != dups {
		p = append(p, fmt.Sprintf("(c) module 1's verify.job_rows marks %d row(s) as a repeat, want %d", repeats, dups))
	}
	if owner != r.tn.userID {
		p = append(p, fmt.Sprintf("(b) module 1's verify.jobs says the job belongs to %q, want the tenant's user %q", m1eShort(owner), m1eShort(r.tn.userID)))
	}
	var wantMal []m1eItem
	for _, it := range items {
		if it.kind == m1eKindMalformed {
			wantMal = append(wantMal, it)
		}
	}
	if len(r.plan.Malformed) != len(wantMal) {
		p = append(p, fmt.Sprintf("(b) Prepare found %d malformed row(s), want %d", len(r.plan.Malformed), len(wantMal)))
	} else {
		for i, m := range r.plan.Malformed {
			if m.ItemRef != wantMal[i].ref || m.Reason != wantMal[i].malform {
				p = append(p, fmt.Sprintf("(b) malformed row %d is %q with reason %q, want %q with %q", i, m.ItemRef, m.Reason, wantMal[i].ref, wantMal[i].malform))
			}
		}
	}
	if d := charge.Decide(m1ad.MalformedOutcome()); d.Kind != charge.Free || d.Rule != "P-08" {
		p = append(p, fmt.Sprintf("(b) a malformed row is decided %s by rule %s, want free by P-08", d.Kind, d.Rule))
	}
	if nm := len(r.plan.Malformed); nm > 0 {
		if m := r.malRes; m.Free != nm || m.Charged != 0 || m.Credits != 0 || m.NotFinal != 0 || m.AlreadySettled != 0 || len(m.Unmapped) != 0 {
			p = append(p, fmt.Sprintf("(b) settling the %d malformed row(s) gave %+v, want all free: charged 0, credits 0, free %d", nm, m, nm))
		}
	}

	// (d) the settle list, the ledger and the three totals
	ledger := s.mny.m1bLedger(r.job)
	r.ledger = ledger
	p = append(p, prefixed("(d) settle list", m1eCompareSettleList(items, r.list))...)
	p = append(p, prefixed("(d) ledger", m1eCompareLedger(items, ledger, r.rsv.Prices))...)
	p = append(p, prefixed("(c)", m1eGroupProblems(items, ledger))...)
	var outcomes []charge.Outcome
	for _, row := range r.rows {
		outcomes = append(outcomes, row.Outcome())
	}
	for range r.plan.Malformed {
		outcomes = append(outcomes, m1ad.MalformedOutcome())
	}
	byDecide := m1eDecideTotals(outcomes, r.rsv.Prices)
	byOracle := m1eOracleTotals(items, r.rsv.Prices)
	var ledgerCredits int64
	for _, c := range ledger {
		ledgerCredits += int64(c.Cost) * int64(c.Units)
	}
	if byDecide != byOracle {
		p = append(p, fmt.Sprintf("(d) charge.Decide over the reported outcomes says %+v, the hand-written oracle says %+v", byDecide, byOracle))
	}
	if ledgerCredits != byOracle.credits || len(ledger) != byOracle.charged {
		p = append(p, fmt.Sprintf("(d) the ledger holds %d charge row(s) for %d credit(s), the oracle expects %d for %d", len(ledger), ledgerCredits, byOracle.charged, byOracle.credits))
	}

	// (e) what the settle calls reported, the balances, the invariants
	var charged, free, already, notFinal, unmapped int
	var credits int64
	for _, res := range append(append([]money.SettleResult(nil), r.results...), r.malRes) {
		charged += res.Charged
		free += res.Free
		already += res.AlreadySettled
		notFinal += res.NotFinal
		unmapped += len(res.Unmapped)
		credits += res.Credits
	}
	if charged != byOracle.charged || free != byOracle.free || credits != byOracle.credits || already != 0 || notFinal != 0 || unmapped != 0 {
		p = append(p, fmt.Sprintf("(e) the settle calls reported charged %d free %d credits %d already-settled %d not-final %d unmapped %d, want charged %d free %d credits %d and none of the rest", charged, free, credits, already, notFinal, unmapped, byOracle.charged, byOracle.free, byOracle.credits))
	}
	avail, resv, ok := s.mny.bal(r.tn.userID, "m1")
	if wantAvail, wantRes := r.grant-r.rsv.Reserved, r.rsv.Reserved-byOracle.credits; !ok || avail != wantAvail || resv != wantRes {
		p = append(p, fmt.Sprintf("(e) the balance is available %d reserved %d, want %d and %d (grant %d, reserve %d, charged %d)", avail, resv, wantAvail, wantRes, r.grant, r.rsv.Reserved, byOracle.credits))
	}
	if la, lr := s.ledgerSums(r.tn.userID); la != avail || lr != resv {
		p = append(p, fmt.Sprintf("(e) the ledger sums to available %d reserved %d, the stored balance says %d and %d", la, lr, avail, resv))
	}
	p = append(p, s.viewProblems("(e) after settling tenant "+r.tn.name)...)

	// (f) the fixture's SMTP counters: one verification per distinct address that reaches the node, none for a repeat or a cache hit
	var wantAddrs []string
	for _, it := range items {
		if it.kind == m1eKindRecipe && it.reaches {
			wantAddrs = append(wantAddrs, it.address)
		}
	}
	p = append(p, prefixed("(f)", m1eCheckProbes(r.sessions, s.nodeIP, wantAddrs))...)
	return p
}

// noteRun prints the evidence of one run: the plan, the submission, the drain, every row with its decision, the ledger.
func (s *m1eStack) noteRun(tag string, r *m1eRun) {
	t := s.t
	var reasons []string
	for _, m := range r.plan.Malformed {
		reasons = append(reasons, string(m.Reason))
	}
	t.Logf("NOTE %s plan: %d input row(s): %d to send (%d of them repeat an earlier address), %d malformed (%s), never sent", tag, len(r.items), len(r.plan.Items), r.plan.Duplicates, len(r.plan.Malformed), strings.Join(reasons, ", "))
	t.Logf("NOTE %s submit: module job %s, the 202 counted %+v; Status complete after %d poll(s) in %v: status %q, counts %+v", tag, r.jobID, r.counts, r.polls, r.waited.Round(100*time.Millisecond), r.status.Status, r.status.Counts)
	t.Logf("NOTE %s drain: %d row(s) in %d page(s) of at most %d (%d wait(s), %d row(s) caught up behind the cursor), %d settle call(s) for the pages and one for the malformed rows", tag, r.stats.Rows, r.stats.Pages, r.tn.ad.PageLimit, r.stats.Waits, r.stats.CatchUp, len(r.results))
	sent := m1eSent(r.items)
	for i, row := range r.rows {
		label := ""
		if i < len(sent) {
			label = sent[i].label
		}
		d := charge.Decide(row.Outcome())
		dec := fmt.Sprintf("%s %s", d.Kind, d.Unit)
		t.Logf("NOTE %s row | ord=%02d %-30s %-44s dup=%-2s decision=%s (rule %s)", tag, row.Ordinal, label, m1eVerdictOfRow(row), orDash(m1bDup(row)), strings.TrimSpace(dec), d.Rule)
	}
	for _, m := range r.plan.Malformed {
		d := charge.Decide(m1ad.MalformedOutcome())
		t.Logf("NOTE %s row | malformed %-28s (%s) never sent, decision=%s (rule %s)", tag, m.ItemRef, m.Reason, d.Kind, d.Rule)
	}
	for _, c := range s.mny.m1bLedger(r.job) {
		t.Logf("NOTE %s ledger | %-34s unit=%-9s cost=%d units=%d", tag, c.ItemRef, c.Unit, c.Cost, c.Units)
	}
	var charged, free int
	var credits int64
	for _, res := range append(append([]money.SettleResult(nil), r.results...), r.malRes) {
		charged, free, credits = charged+res.Charged, free+res.Free, credits+res.Credits
	}
	t.Logf("NOTE %s settle: %d item(s) charged for %d credit(s), %d free; the fixture saw %d RCPT(s) from the node in this job", tag, charged, credits, free, len(r.sessions))
}

// ---- B10 --------------------------------------------------------------------------------------------------------------------------------------------------------

// m1eBuildPageItems is the job of the pagination test: n rows, each a distinct cheap address (DNS and the lists only: it never reaches the node) cycling through five recipes, and
// the last three rows repeat earlier ones, so that a repeat is on the far side of the default page too.
func m1eBuildPageItems(e *m1nEnv, n int) []m1eItem {
	byID := map[string]m1nRecipe{}
	for _, r := range e.recipes() {
		byID[r.id] = r
	}
	cycle := []string{"no_mx", "no_mx_nodata", "null_mx", "spam_trap", "disposable"}
	var items []m1eItem
	for i := 0; i < n-3; i++ {
		m1eAddItem(&items, m1eNewRecipeItem(byID[cycle[i%len(cycle)]], fresh("e2ep")))
	}
	for _, from := range []int{10, 100, 150} {
		m1eAddItem(&items, m1eNewDuplicate(items[from], "duplicate", strings.ToUpper(items[from].address)))
	}
	return items
}

// m1eShape is the page sizes on one line.
func m1eShape(sizes []int) string {
	if len(sizes) <= 6 {
		parts := make([]string, len(sizes))
		for i, n := range sizes {
			parts[i] = strconv.Itoa(n)
		}
		return strings.Join(parts, "+")
	}
	return fmt.Sprintf("%d x %d, last %d", len(sizes)-1, sizes[0], sizes[len(sizes)-1])
}

// TestM1Node_E2EPaginationBoundaries: see the file comment.
func TestM1Node_E2EPaginationBoundaries(t *testing.T) {
	s := newM1eStack(t)
	tn := s.newTenant("pages", true)
	items := m1eBuildPageItems(s.m1nEnv, m1ePageRows)
	n := len(items)
	inputs := make([]m1ad.Input, n)
	for i, it := range items {
		inputs[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	plan := m1ad.Prepare(inputs)
	if err := plan.Err(); err != nil || len(plan.Items) != n || len(plan.Malformed) != 0 || plan.Duplicates != 3 {
		t.Fatalf("Prepare of the %d pagination rows: err %v, %d to send, %d malformed, %d repeat(s), want %d, 0 and 3", n, err, len(plan.Items), len(plan.Malformed), plan.Duplicates, n)
	}
	jobID, counts, err := tn.ad.Submit(s.ctx, plan)
	if err != nil {
		t.Fatalf("Submit of the pagination job failed (kind %v): %v", modclient.KindOf(err), err)
	}
	js, polls, took := s.waitComplete(tn, jobID, n, m1eJobWait)
	rows, malformed, repeats, owner := s.moduleJob(jobID)
	t.Logf("NOTE B10 job: %d rows (%d of them repeats), module job %s, the 202 counted %+v; complete after %d poll(s) in %v with counts %+v; module 1 holds %d job_rows (%d malformed, %d repeats), owner is the tenant: %t",
		n, plan.Duplicates, jobID, counts, polls, took.Round(100*time.Millisecond), js.Counts, rows, malformed, repeats, owner == tn.userID)
	if p := m1eCheckModuleRows(rows, malformed, n); len(p) != 0 || repeats != 3 || owner != tn.userID {
		t.Fatalf("module 1's own rows of the pagination job: %v, repeats %d (want 3), owner ok %t", p, repeats, owner == tn.userID)
	}

	// what the job was built to answer, by construction and independent of any listing
	var built []m1eTuple
	for _, it := range items {
		built = append(built, m1eTuple{it.ref, it.want.status, it.want.reason, it.want.origin})
	}

	limits := []int{0, 1, n / 2, n - 1, n, n + 1, m1ad.MaxPageLimit}
	var base []m1eTuple
	var problems []string
	for _, limit := range limits {
		ad := s.adapterFor(tn, limit)
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
		var got []m1ad.Row
		st, err := ad.Drain(ctx, jobID, func(page []m1ad.Row) error { got = append(got, page...); return nil })
		if err != nil {
			cancel()
			t.Fatalf("Drain at limit %d failed (kind %v): %v", limit, modclient.KindOf(err), err)
		}
		eff := limit
		if eff == 0 {
			eff = m1ad.DefaultPageLimit
		}
		tuples := m1eTuplesOfRows(got)
		if base == nil {
			base = tuples
		}
		lim := fmt.Sprintf("limit %d", limit)
		problems = append(problems, prefixed(lim+", against what the rows were built to be", m1eSetDiff(built, tuples))...)
		problems = append(problems, prefixed(lim+", against the first drain", m1eSetDiff(base, tuples))...)
		problems = append(problems, prefixed(lim, m1eOrdinalProblems(got, n))...)
		wantPages := m1ePages(n, limit)
		if st.Pages != wantPages || st.Rows != n || st.CatchUp != 0 || st.Waits != 0 {
			problems = append(problems, fmt.Sprintf("%s: Drain says %d page(s), %d row(s), %d wait(s), %d caught up; want %d page(s), %d row(s), none waited or caught up (a complete job)", lim, st.Pages, st.Rows, st.Waits, st.CatchUp, wantPages, n))
		}

		// the module's own look-ahead, read directly: the page sizes and where the cursor ends
		walked, sizes, err := m1eWalk(ctx, ad, jobID, limit, false)
		if err != nil {
			cancel()
			t.Fatalf("a walk at limit %d failed (kind %v): %v", limit, modclient.KindOf(err), err)
		}
		problems = append(problems, prefixed(lim+", walked with Results", m1eSetDiff(built, m1eTuplesOfRows(walked)))...)
		wantSizes := make([]int, wantPages)
		for i := range wantSizes {
			wantSizes[i] = eff
		}
		wantSizes[wantPages-1] = n - (wantPages-1)*eff
		if fmt.Sprint(sizes) != fmt.Sprint(wantSizes) {
			problems = append(problems, fmt.Sprintf("%s: the pages hold %v rows, want %v (the module reads one row ahead to know whether another page exists; the cursor is the last row returned)", lim, sizes, wantSizes))
		}

		// the control: the look-ahead cursor bug, which drops the last row of every page, is caught by the same equality
		buggy, bsizes, err := m1eWalk(ctx, ad, jobID, limit, true)
		cancel()
		if err != nil {
			t.Fatalf("the look-ahead-bug walk at limit %d failed (kind %v): %v", limit, modclient.KindOf(err), err)
		}
		diff := m1eSetDiff(built, m1eTuplesOfRows(buggy))
		missing := 0
		for _, d := range diff {
			if strings.HasPrefix(d, "missing ") {
				missing++
			}
		}
		if len(diff) == 0 || missing != len(diff) || missing != len(bsizes) {
			problems = append(problems, fmt.Sprintf("%s: the look-ahead-bug walker kept %d of %d row(s) over %d page(s), the equality reported %d line(s) of which %d missing: it must report exactly one missing row per page and nothing else", lim, len(buggy), n, len(bsizes), len(diff), missing))
		}
		t.Logf("NOTE B10 limit %4d: %3d page(s) of %s (expected %d); Drain delivered %d row(s), %d wait(s), %d caught up; set equal to what the rows were built to be and to the first drain; the look-ahead-bug walker kept %d of %d row(s) and was caught (%d missing)",
			limit, st.Pages, m1eShape(sizes), wantPages, st.Rows, st.Waits, st.CatchUp, len(buggy), n, missing)
	}
	if len(problems) != 0 {
		t.Fatalf("%d difference(s) between the drains of one job:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("NOTE B10 verdict: %d limits (the default page, 1, N/2, N-1, N, N+1, the maximum %d) each read the same %d (item_id, status, reason, origin) tuples exactly, in %v; the module's default page of %d was crossed at limit 0 (3 pages)", len(limits), m1ad.MaxPageLimit, n, took.Round(time.Second), m1ad.DefaultPageLimit)
}

// ---- B11 --------------------------------------------------------------------------------------------------------------------------------------------------------

// TestM1Node_E2ECrossTenant: see the file comment.
func TestM1Node_E2ECrossTenant(t *testing.T) {
	s := newM1eStack(t)
	a := s.newTenant("a", true)
	b := s.newTenant("b", true)
	c := s.newTenant("c", false) // a real user whose derived key module 1 was never told about
	t.Logf("NOTE B11 tenants: A is user %s, B is user %s (both provisioned), C is user %s (a real user, its key NOT provisioned)", m1eShort(a.userID), m1eShort(b.userID), m1eShort(c.userID))

	var problems []string
	add := func(p []string) { problems = append(problems, p...) }
	var texts []struct {
		what string
		text string
	}
	typed := func(what string, err error, kind modclient.Kind, status int, code string) *modclient.Error {
		me, p := m1eWantErr(what, err, kind, status, code)
		add(p)
		if err != nil {
			texts = append(texts, struct{ what, text string }{what, m1eErrText(err)})
		}
		return me
	}

	// A's job, two cheap rows
	e := s.m1nEnv
	aItems := []m1eItem{}
	m1eAddItem(&aItems, m1eNewRecipeItem(m1eRecipe(e, "no_mx"), fresh("e2ex")))
	m1eAddItem(&aItems, m1eNewRecipeItem(m1eRecipe(e, "spam_trap"), fresh("e2ex")))
	aID := s.submitItems(a, aItems)
	s.waitComplete(a, aID, len(aItems), m1eJobWait)
	if _, _, _, owner := s.moduleJob(aID); owner != a.userID {
		t.Fatalf("module 1 says A's job belongs to %q, want A's user %q", m1eShort(owner), m1eShort(a.userID))
	}
	rowsBefore, _, _, _ := s.moduleJob(aID)

	// B's job, one row
	bItems := []m1eItem{}
	m1eAddItem(&bItems, m1eNewRecipeItem(m1eRecipe(e, "no_mx"), fresh("e2ex")))
	bID := s.submitItems(b, bItems)
	s.waitComplete(b, bID, len(bItems), m1eJobWait)
	if _, _, _, owner := s.moduleJob(bID); owner != b.userID {
		t.Fatalf("module 1 says B's job belongs to %q, want B's user %q", m1eShort(owner), m1eShort(b.userID))
	}

	// what must never appear in an error: the ids, the addresses, the users and the keys of both tenants
	secrets := map[string]string{"A's job id": aID, "B's job id": bID, "A's user id": a.userID, "B's user id": b.userID, "C's user id": c.userID, "A's key": a.key(), "B's key": b.key(), "C's key": c.key()}
	for i, it := range aItems {
		secrets[fmt.Sprintf("A's address %d", i)] = it.address
		secrets[fmt.Sprintf("A's local part %d", i)] = strings.Split(it.address, "@")[0]
	}
	secrets["B's address"], secrets["B's local part"] = bItems[0].address, strings.Split(bItems[0].address, "@")[0]
	absent, err := s.freshUUID()
	if err != nil {
		t.Fatal(err)
	}
	secrets["an absent job's id"] = absent

	// 1. B asks for A's job: 404 not_found, a typed NotFound, no value, no row
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	js, err := b.ad.Status(ctx, aID)
	strangerStatus := typed("B's Status on A's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	if js != (m1ad.JobStatus{}) {
		problems = append(problems, fmt.Sprintf("B's Status on A's job returned a value beside the error: %+v", js))
	}
	page, next, err := b.ad.Results(ctx, aID, "", 0)
	strangerResults := typed("B's Results on A's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	if page != nil || next != "" {
		problems = append(problems, fmt.Sprintf("B's Results on A's job returned %d row(s) and cursor %q beside the error", len(page), next))
	}
	page, next, err = b.ad.Results(ctx, aID, "", 1)
	typed("B's Results (limit 1) on A's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	if page != nil || next != "" {
		problems = append(problems, "B's Results (limit 1) on A's job returned a row beside the error")
	}
	// a stranger learns nothing from a cursor either: the module checks the tenancy BEFORE it judges the cursor, so a garbage cursor is a 404 for B and a 400 for the owner
	_, _, err = b.ad.Results(ctx, aID, "not-a-cursor", 5)
	typed("B's Results with a garbage cursor on A's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	delivered := 0
	st, err := b.ad.Drain(ctx, aID, func(rows []m1ad.Row) error { delivered += len(rows); return nil })
	typed("B's Drain of A's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	if delivered != 0 || st != (m1ad.DrainStats{}) {
		problems = append(problems, fmt.Sprintf("B's Drain of A's job delivered %d row(s) and reports %+v beside the error", delivered, st))
	}

	// 2. the control: the SAME calls with A's adapter succeed
	ajs, err := a.ad.Status(ctx, aID)
	if err != nil || !ajs.Complete || ajs.Counts.Total != len(aItems) {
		problems = append(problems, fmt.Sprintf("the control: A's Status on A's job gave %+v, %v, want a complete job of %d row(s)", ajs, err, len(aItems)))
	}
	apage, anext, err := a.ad.Results(ctx, aID, "", 0)
	if err != nil || len(apage) != len(aItems) || anext != "" {
		problems = append(problems, fmt.Sprintf("the control: A's Results on A's job gave %d row(s), cursor %q, %v, want %d rows and no cursor", len(apage), anext, err, len(aItems)))
	}
	for i, row := range apage {
		if i < len(aItems) && row.ItemID != aItems[i].ref {
			problems = append(problems, fmt.Sprintf("the control: A's result %d is item %q, want %q", i, row.ItemID, aItems[i].ref))
		}
	}
	_, _, err = a.ad.Results(ctx, aID, "not-a-cursor", 5)
	typed("the control: A's Results with the same garbage cursor", err, modclient.Refused, http.StatusBadRequest, "validation_failed")
	adrained, _ := s.drainAll(a.ad, aID)
	if len(adrained) != len(aItems) {
		problems = append(problems, fmt.Sprintf("the control: A's Drain of A's job delivered %d row(s), want %d", len(adrained), len(aItems)))
	}

	// 3. B reads its own job, and A cannot read B's
	bjs, err := b.ad.Status(ctx, bID)
	if err != nil || !bjs.Complete || bjs.Counts.Total != len(bItems) {
		problems = append(problems, fmt.Sprintf("B's Status on B's own job gave %+v, %v, want a complete job of %d row(s)", bjs, err, len(bItems)))
	}
	bdrained, _ := s.drainAll(b.ad, bID)
	if len(bdrained) != 1 || bdrained[0].ItemID != bItems[0].ref || bdrained[0].Status != "invalid" || bdrained[0].Reason != "no_mx" {
		problems = append(problems, fmt.Sprintf("B's Drain of B's own job gave %+v, want one invalid/no_mx row for item %q", bdrained, bItems[0].ref))
	}
	_, err = a.ad.Status(ctx, bID)
	typed("A's Status on B's job", err, modclient.NotFound, http.StatusNotFound, "not_found")
	_, _, err = a.ad.Results(ctx, bID, "", 0)
	typed("A's Results on B's job", err, modclient.NotFound, http.StatusNotFound, "not_found")

	// 4. a job that does not exist answers exactly like a job that is not yours: the 404 is not a way to find out which ids exist
	_, err = b.ad.Status(ctx, absent)
	absentStatus := typed("B's Status on a job that does not exist", err, modclient.NotFound, http.StatusNotFound, "not_found")
	_, _, err = b.ad.Results(ctx, absent, "", 0)
	absentResults := typed("B's Results on a job that does not exist", err, modclient.NotFound, http.StatusNotFound, "not_found")
	for _, pair := range []struct {
		what             string
		stranger, absent *modclient.Error
	}{{"Status", strangerStatus, absentStatus}, {"Results", strangerResults, absentResults}} {
		if pair.stranger != nil && pair.absent != nil && (pair.stranger.Kind != pair.absent.Kind || pair.stranger.Status != pair.absent.Status || pair.stranger.Code != pair.absent.Code || pair.stranger.Message != pair.absent.Message || pair.stranger.Enveloped != pair.absent.Enveloped) {
			problems = append(problems, fmt.Sprintf("the answer for another tenant's job (%s) differs from the answer for a job that does not exist: %v versus %v", pair.what, pair.stranger, pair.absent))
		}
	}

	// 5. a real user whose key module 1 was never told about: 401 unauthenticated, a typed Refused, never a result; and its Submit creates nothing
	_, err = c.ad.Status(ctx, aID)
	typed("C's Status on A's job", err, modclient.Refused, http.StatusUnauthorized, "unauthenticated")
	_, _, err = c.ad.Results(ctx, aID, "", 0)
	typed("C's Results on A's job", err, modclient.Refused, http.StatusUnauthorized, "unauthenticated")
	jobsBefore := s.moduleJobCount()
	_, _, err = c.ad.Submit(ctx, m1ad.Prepare([]m1ad.Input{{ItemRef: "c-1", Address: fresh("e2ec") + "@nomx.fixture.test"}}))
	typed("C's Submit", err, modclient.Refused, http.StatusUnauthorized, "unauthenticated")
	if errors.Is(err, m1ad.ErrOrphanPossible) {
		problems = append(problems, "C's refused Submit is reported as a possible orphan job: a 401 created nothing and must not look like an unconfirmed submission")
	}
	if jobsAfter := s.moduleJobCount(); jobsAfter != jobsBefore {
		problems = append(problems, fmt.Sprintf("module 1 holds %d job(s) after C's refused Submit, %d before: the 401 must create nothing", jobsAfter, jobsBefore))
	}

	// 6. nothing changed on A's job, and no error text carries anything that identifies a job, an address, a user or a key
	if rowsAfter, _, _, owner := s.moduleJob(aID); rowsAfter != rowsBefore || owner != a.userID {
		problems = append(problems, fmt.Sprintf("A's job holds %d row(s) owned by %q after the strangers' calls, %d owned by A before", rowsAfter, m1eShort(owner), rowsBefore))
	}
	for _, x := range texts {
		if leaked := m1eLeaks(x.text, secrets); len(leaked) != 0 {
			problems = append(problems, fmt.Sprintf("the error of %q carries %s", x.what, strings.Join(leaked, ", ")))
		}
	}

	if strangerStatus != nil {
		t.Logf("NOTE B11 B -> A's Status: %v (kind %s, HTTP %d, code %s, enveloped %t); no value returned", strangerStatus, strangerStatus.Kind, strangerStatus.Status, strangerStatus.Code, strangerStatus.Enveloped)
	}
	if absentStatus != nil {
		t.Logf("NOTE B11 B -> an absent job's Status: %v: the same answer as for A's job (kind, status, code, message and envelope are equal)", absentStatus)
	}
	t.Logf("NOTE B11 B -> A's job: Status, Results (limit 0, limit 1, a garbage cursor) and Drain are all NotFound 404 not_found: %d row(s) returned; A -> A's job: Status complete (%d rows), Results %d row(s), the garbage cursor is Refused 400 validation_failed (the cursor is judged only for the owner)", delivered, ajs.Counts.Total, len(apage))
	t.Logf("NOTE B11 B -> B's job: Status complete, Drain %d row(s); A -> B's job: NotFound; module 1's verify.jobs owners: A's job is owned by A's user, B's by B's", len(bdrained))
	t.Logf("NOTE B11 C (real user, key not provisioned) -> Status, Results, Submit: Refused 401 unauthenticated; module 1 holds %d job(s) before and after its Submit; %d error text(s) checked against %d identifying value(s) (job ids, addresses, local parts, user ids, keys): none appears", jobsBefore, len(texts), len(secrets))
	if len(problems) != 0 {
		t.Fatalf("%d difference(s) in the cross-tenant answers:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// submitItems prepares and submits the rows as the tenant and returns module 1's job id (the rows are all well formed).
func (s *m1eStack) submitItems(tn *m1eTenant, items []m1eItem) string {
	s.t.Helper()
	inputs := make([]m1ad.Input, len(items))
	for i, it := range items {
		inputs[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	id, _, err := tn.ad.Submit(s.ctx, m1ad.Prepare(inputs))
	if err != nil {
		s.t.Fatalf("Submit as tenant %s failed (kind %v): %v", tn.name, modclient.KindOf(err), err)
	}
	return id
}

func (s *m1eStack) freshUUID() (string, error) {
	var id string
	err := s.admin.QueryRow(s.ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
	return id, err
}

// ---- the pure controls ---------------------------------------------------------------------------------------------------------------------------------------------

// m1eReportedRows is what module 1 would report for the rows, built from their wants: the input of the pure test of the mapping and the comparisons.
func m1eReportedRows(items []m1eItem) []m1ad.Row {
	var rows []m1ad.Row
	ord := map[string]int{}
	for _, it := range items {
		if it.kind == m1eKindMalformed {
			continue
		}
		i := len(rows)
		ord[it.ref] = i
		r := m1ad.Row{Ordinal: i, ItemID: it.ref, Status: it.want.status, Reason: it.want.reason, FromCache: it.want.cache, SkipReason: it.want.skip, FailureOrigin: it.want.origin}
		if it.kind == m1eKindDuplicate {
			first := ord[it.firstRef]
			r.DuplicateOf = &first
		}
		rows = append(rows, r)
	}
	return rows
}

// m1eFakeListing serves the results route of a finished job of n rows with the module's own look-ahead rule (it reads one row more than the page to know whether another exists, and the
// cursor is the last row RETURNED), so that the walkers and the page arithmetic can be run without a stack.
func m1eFakeListing(t *testing.T, jobID string, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if k, err := strconv.Atoi(v); err == nil && k > 0 && k <= 1000 {
				limit = k
			}
		}
		after := -1
		if c := r.URL.Query().Get("cursor"); c != "" {
			after, _ = strconv.Atoi(c)
		}
		var els []string
		next := ""
		for i := after + 1; i < n; i++ {
			if len(els) == limit {
				next = strconv.Itoa(i - 1)
				break
			}
			els = append(els, fmt.Sprintf(`{"ordinal":%d,"item_id":"r%03d","status":"invalid","reason":"no_mx","confidence":96,"recommendation":"skip","domain":"fixture.test","flags":{"from_cache":false},"signals":{}}`, i, i))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"results":[%s],"next_cursor":%q}`, jobID, strings.Join(els, ","), next)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestM1Node_E2EChecksBite: see the file comment. It needs no stack: the addresses are documentation-range stand-ins and the listing is a fake.
func TestM1Node_E2EChecksBite(t *testing.T) {
	e := &m1nEnv{nodeIP: "198.51.100.3", smtpIP: "198.51.100.2"}
	warm := "e2ewarm001@valid.fixture.test"
	prices := map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2}

	// Prepare agrees with the rows the tests build: the three malformed rows are three different norm reasons and are never sent
	items := m1eBuildBulkItems(e, warm)
	inputs := make([]m1ad.Input, len(items))
	for i, it := range items {
		inputs[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	plan := m1ad.Prepare(inputs)
	if err := plan.Err(); err != nil {
		t.Fatalf("Prepare refused the rows of the bulk test: %v", err)
	}
	if len(plan.Items) != m1eBulkSent || len(plan.Malformed) != m1eBulkMalformed || plan.Duplicates != 3 {
		t.Fatalf("Prepare found %d to send, %d malformed, %d repeat(s), the bulk test is written for %d, %d and 3", len(plan.Items), len(plan.Malformed), plan.Duplicates, m1eBulkSent, m1eBulkMalformed)
	}
	var reasons []norm.Reason
	for _, m := range plan.Malformed {
		reasons = append(reasons, m.Reason)
	}
	if fmt.Sprint(reasons) != fmt.Sprint([]norm.Reason{norm.ReasonNoAt, norm.ReasonMultipleAt, norm.ReasonEmptyLocal}) {
		t.Fatalf("the malformed rows have the reasons %v, want three different ones: no_at, multiple_at, empty_local", reasons)
	}
	seenRef := map[string]bool{}
	for _, it := range items {
		if seenRef[it.ref] {
			t.Fatalf("two rows share the ref %q", it.ref)
		}
		seenRef[it.ref] = true
		if it.unit == "?" {
			t.Fatalf("the oracle has no price for the row %q (%s)", it.ref, it.label)
		}
	}
	if got := len(m1ePageItemsForTest(e)); got != m1ePageRows {
		t.Fatalf("the pagination rows are %d, want %d", got, m1ePageRows)
	}
	if pp := m1ad.Prepare(m1ePageInputsForTest(e)); pp.Err() != nil || len(pp.Items) != m1ePageRows || pp.Duplicates != 3 || len(pp.Malformed) != 0 {
		t.Fatalf("Prepare of the pagination rows: %v, %d to send, %d repeat(s), %d malformed", pp.Err(), len(pp.Items), pp.Duplicates, len(pp.Malformed))
	}

	// the clean path: the real mapping of rows module 1 would report, the oracle's ledger, the totals
	rows := m1eReportedRows(items)
	list, err := m1ad.ToSettleItems(rows)
	if err != nil {
		t.Fatal(err)
	}
	list = append(list, plan.MalformedSettleItems()...)
	if p := m1eCompareSettleList(items, list); len(p) != 0 {
		t.Fatalf("the control: the adapter's mapping of the expected rows must match the oracle, got %v", p)
	}
	var ledger []m1bCharge
	for _, it := range items {
		if it.unit != "" {
			ledger = append(ledger, m1bCharge{ItemRef: it.ref, Unit: it.unit, Cost: prices[it.unit], Units: 1})
		}
	}
	if p := m1eCompareLedger(items, ledger, prices); len(p) != 0 {
		t.Fatalf("the control: the oracle's own ledger must match, got %v", p)
	}
	if p := m1eGroupProblems(items, ledger); len(p) != 0 {
		t.Fatalf("the control: the oracle's own ledger charges every address group once, got %v", p)
	}
	var outcomes []charge.Outcome
	for _, r := range rows {
		outcomes = append(outcomes, r.Outcome())
	}
	for range plan.Malformed {
		outcomes = append(outcomes, m1ad.MalformedOutcome())
	}
	if d, o := m1eDecideTotals(outcomes, prices), m1eOracleTotals(items, prices); d != o || d.other != 0 || d.charged == 0 || d.free == 0 {
		t.Fatalf("the control: charge.Decide over the expected outcomes says %+v, the oracle %+v (both must hold charges and free items and nothing else)", d, o)
	}

	// the comparisons bite
	for _, c := range m1eCorruptLists(items, list) {
		if got := m1eCompareSettleList(items, c.list); !m1eReports(got, c.ref, c.want) {
			t.Fatalf("%s: the settle-list comparison said %v, want a line naming %q and containing %q", c.name, got, c.ref, c.want)
		}
	}
	for _, c := range m1eCorruptLedgers(ledger) {
		if got := m1eCompareLedger(items, c.ledger, prices); !m1eContains(got, c.want) {
			t.Fatalf("%s: the ledger comparison said %v, want a line containing %q", c.name, got, c.want)
		}
	}
	doubled := append(append([]m1bCharge(nil), ledger...), ledger[0])
	if got := m1eCompareLedger(items, doubled, prices); !m1eContains(got, "once but the ledger holds 2") {
		t.Fatalf("a charge written twice was not reported: %v", got)
	}
	if got := m1eGroupProblems(items, append(append([]m1bCharge(nil), ledger...), m1bCharge{ItemRef: func() string {
		for _, it := range items {
			if it.kind == m1eKindDuplicate {
				return it.ref
			}
		}
		return ""
	}(), Unit: "m1_own", Cost: 1, Units: 1})); len(got) == 0 {
		t.Fatal("a charge on a repeat made the address group be charged twice and the group check did not say so")
	}
	if got := m1eCompareLedger(items, ledger, map[string]int{"m1_own": 2, "m1_cache": 1}); !m1eContains(got, "frozen price") {
		t.Fatalf("a charge at another price than the frozen one was not reported: %v", got)
	}
	odd := append([]m1eItem(nil), items...)
	odd[1].unit = m1eOracle(odd[1].kind, "no_such_recipe")
	if got := m1eCompareSettleList(odd, list); !m1eContains(got, "no price for this recipe") {
		t.Fatalf("a recipe the oracle does not know was not reported: %v", got)
	}
	if d, o := m1eDecideTotals(outcomes[1:], prices), m1eOracleTotals(items, prices); d == o {
		t.Fatal("the totals of charge.Decide are equal after an outcome was dropped: the comparison of the totals cannot fail")
	}

	// the row-level assertions (a) and (c), on the rows module 1 would report and on broken ones
	if p := m1eRowProblems(items, rows); len(p) != 0 {
		t.Fatalf("the control: the expected rows must pass the row check, got %v", p)
	}
	five, zero := 5, 0
	for _, c := range []struct {
		name string
		mut  func(rs []m1ad.Row) []m1ad.Row
		want string
	}{
		{"a verdict changed", func(rs []m1ad.Row) []m1ad.Row { rs[3].Reason = "smtp_error"; return rs }, "module 1 reported"},
		{"the cache flag lost on the cache hit", func(rs []m1ad.Row) []m1ad.Row { rs[len(rs)-2].FromCache = false; return rs }, "module 1 reported"},
		{"the origin lost on an unknown", func(rs []m1ad.Row) []m1ad.Row { rs[12].FailureOrigin = ""; return rs }, "module 1 reported"},
		{"a row dropped", func(rs []m1ad.Row) []m1ad.Row { return rs[:len(rs)-1] }, "result row(s) for"},
		{"two rows swapped", func(rs []m1ad.Row) []m1ad.Row { rs[2], rs[3] = rs[3], rs[2]; return rs }, "result 2 is ordinal"},
		{"a repeat that does not say what it repeats", func(rs []m1ad.Row) []m1ad.Row { rs[10].DuplicateOf = nil; return rs }, "duplicate_of is"},
		{"a repeat that names another row", func(rs []m1ad.Row) []m1ad.Row { rs[10].DuplicateOf = &five; return rs }, "duplicate_of is"},
		{"a first row that claims to be a repeat", func(rs []m1ad.Row) []m1ad.Row { rs[4].DuplicateOf = &zero; return rs }, "on a first row"},
	} {
		if p := m1eRowProblems(items, c.mut(append([]m1ad.Row(nil), rows...))); !m1eContains(p, c.want) {
			t.Fatalf("%s: the row check said %v, want a line containing %q", c.name, p, c.want)
		}
	}

	// the module-rows check
	if p := m1eCheckModuleRows(24, 0, 24); len(p) != 0 {
		t.Fatalf("the control: 24 rows and none malformed is right, got %v", p)
	}
	if p := m1eCheckModuleRows(27, 3, 24); len(p) != 2 {
		t.Fatalf("malformed rows that reached the module must be reported twice (the count and the malformed), got %v", p)
	}
	if p := m1eCheckModuleRows(24, 1, 24); len(p) != 1 || !strings.Contains(p[0], "malformed") {
		t.Fatalf("one malformed row among the right number must be reported, got %v", p)
	}

	// the fixture's counters
	sess := func(remote, rcpt string) m1nSession {
		return m1nSession{Remote: remote, Rcpt: rcpt, Domain: rcpt[strings.LastIndex(rcpt, "@")+1:]}
	}
	node := e.nodeIP
	good := []m1nSession{sess(node, "ada@valid.fixture.test"), sess(node, "nx-1@valid.fixture.test"), sess(node, "bob@full.fixture.test"), sess(node, "nx-2@full.fixture.test")}
	wantAddrs := []string{"ada@valid.fixture.test", "bob@full.fixture.test"}
	if p := m1eCheckProbes(good, node, wantAddrs); len(p) != 0 {
		t.Fatalf("the control: one target and one probe per address is right, got %v", p)
	}
	for _, c := range []struct {
		name     string
		sessions []m1nSession
		want     string
	}{
		{"a repeat reached the node a second time", append(append([]m1nSession(nil), good...), sess(node, "ada@valid.fixture.test"), sess(node, "nx-3@valid.fixture.test")), "was RCPTed 2 time(s), want exactly once"},
		{"a cache hit reached the node", append(append([]m1nSession(nil), good...), sess(node, "cat@valid.fixture.test"), sess(node, "nx-3@valid.fixture.test")), "no row of this job should have caused it"},
		{"a probe missing", append(append([]m1nSession(nil), good[:1]...), good[2:]...), "saw 0 nx- probe(s), want 1"},
		{"a foreign source", append([]m1nSession{sess("203.0.113.9", "ada@valid.fixture.test")}, good[1:]...), "only the node"},
		{"a session where none belongs", append(append([]m1nSession(nil), good...), sess(node, "nx-9@grey.fixture.test")), "although no address of this job should have reached it"},
		{"no session at all", nil, "was RCPTed 0 time(s)"},
	} {
		if p := m1eCheckProbes(c.sessions, node, wantAddrs); !m1eContains(p, c.want) {
			t.Fatalf("%s: the counter check said %v, want a line containing %q", c.name, p, c.want)
		}
	}
	if p := m1eCheckProbes(good[:2], node, nil); len(p) == 0 {
		t.Fatal("sessions in a job that should reach the node with no address were not reported")
	}

	// the set equality
	ref := []m1eTuple{{"r0", "valid", "accepted", ""}, {"r1", "unknown", "greylisted", "remote"}, {"r2", "invalid", "no_mx", ""}}
	if p := m1eSetDiff(ref, append([]m1eTuple(nil), ref...)); len(p) != 0 {
		t.Fatalf("the control: equal sets must be equal, got %v", p)
	}
	for _, c := range []struct {
		name string
		got  []m1eTuple
		want string
	}{
		{"a row dropped", ref[:2], "missing r2="},
		{"a row added", append(append([]m1eTuple(nil), ref...), m1eTuple{"r3", "valid", "accepted", ""}), "extra r3="},
		{"a verdict changed", []m1eTuple{ref[0], {"r1", "unknown", "greylisted", "own"}, ref[2]}, "changed r1="},
		{"a row twice", append(append([]m1eTuple(nil), ref...), ref[0]), "r0 appears 2 time(s)"},
		{"an item renamed", []m1eTuple{ref[0], ref[1], {"r9", "invalid", "no_mx", ""}}, "missing r2="},
	} {
		if p := m1eSetDiff(ref, c.got); !m1eContains(p, c.want) {
			t.Fatalf("%s: the equality said %v, want a line containing %q", c.name, p, c.want)
		}
	}

	// the walkers and the page arithmetic, on a fake listing with the module's look-ahead
	const n = 23
	const jobID = "0b9a2f50-1d3c-4a8e-8f21-6c7d5e4f3a2b"
	srv := m1eFakeListing(t, jobID, n)
	ad, err := m1ad.New(srv.URL, config.Secret(randomHex(t, 16)), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var built []m1eTuple
	for i := 0; i < n; i++ {
		built = append(built, m1eTuple{fmt.Sprintf("r%03d", i), "invalid", "no_mx", ""})
	}
	ctx := testCtx(t)
	for _, limit := range []int{0, 1, n / 2, n - 1, n, n + 1, m1ad.MaxPageLimit} {
		got, sizes, err := m1eWalk(ctx, ad, jobID, limit, false)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if p := m1eSetDiff(built, m1eTuplesOfRows(got)); len(p) != 0 {
			t.Fatalf("limit %d: the correct walker differs from the listing: %v", limit, p)
		}
		if p := m1eOrdinalProblems(got, n); len(p) != 0 {
			t.Fatalf("limit %d: %v", limit, p)
		}
		if len(sizes) != m1ePages(n, limit) {
			t.Fatalf("limit %d: %d page(s), want %d (sizes %v)", limit, len(sizes), m1ePages(n, limit), sizes)
		}
		buggy, bsizes, err := m1eWalk(ctx, ad, jobID, limit, true)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		d := m1eSetDiff(built, m1eTuplesOfRows(buggy))
		if len(d) != len(bsizes) || len(buggy) != n-len(bsizes) {
			t.Fatalf("limit %d: the look-ahead-bug walker kept %d of %d row(s) over %d page(s), the equality reported %d line(s): want exactly one missing row per page", limit, len(buggy), n, len(bsizes), len(d))
		}
		for _, line := range d {
			if !strings.HasPrefix(line, "missing ") {
				t.Fatalf("limit %d: the equality said %q about the buggy walker, want only missing rows", limit, line)
			}
		}
	}
	if sizes := func() []int { _, s, _ := m1eWalk(ctx, ad, jobID, 0, false); return s }(); fmt.Sprint(sizes) != "[23]" {
		t.Fatalf("a listing of %d rows at the default page is %v, want one page", n, sizes)
	}
	big := m1eFakeListing(t, jobID, 220)
	bad, err := m1ad.New(big.URL, config.Secret(randomHex(t, 16)), nil, modclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, sizes, err := m1eWalk(ctx, bad, jobID, 0, false); err != nil || fmt.Sprint(sizes) != "[100 100 20]" {
		t.Fatalf("a listing of 220 rows at the default page is %v (%v), want 100+100+20", sizes, err)
	}
	if got := m1ePages(220, 110); got != 2 {
		t.Fatalf("220 rows at 110 per page are %d page(s), want 2 (the boundary is exactly in the middle)", got)
	}
	if m1ePages(220, 219) != 2 || m1ePages(220, 220) != 1 || m1ePages(220, 221) != 1 || m1ePages(220, 1) != 220 || m1ePages(220, 0) != 3 {
		t.Fatal("the page arithmetic is wrong at one of the boundaries")
	}

	// the typed-error check and the leak check
	nf := &modclient.Error{Kind: modclient.NotFound, Module: modclient.M1, Route: "m1.status", Status: 404, Code: "not_found", Enveloped: true}
	if _, p := m1eWantErr("x", nf, modclient.NotFound, 404, "not_found"); len(p) != 0 {
		t.Fatalf("the control: a 404 not_found is a NotFound 404 not_found, got %v", p)
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, "no error"},
		{"a plain error", errors.New("boom"), "want a *modclient.Error"},
		{"another kind", &modclient.Error{Kind: modclient.Refused, Status: 404, Code: "not_found"}, "kind refused, want not found"},
		{"another status", &modclient.Error{Kind: modclient.NotFound, Status: 410, Code: "not_found"}, "HTTP status 410"},
		{"another code", &modclient.Error{Kind: modclient.NotFound, Status: 404, Code: "gone"}, `module error code "gone"`},
	} {
		if _, p := m1eWantErr("x", c.err, modclient.NotFound, 404, "not_found"); !m1eContains(p, c.want) {
			t.Fatalf("%s: the error check said %v, want a line containing %q", c.name, p, c.want)
		}
	}
	secrets := map[string]string{"a job id": jobID, "an address": "ada@valid.fixture.test", "a key": randomHex(t, 16)}
	if l := m1eLeaks(m1eErrText(nf), secrets); len(l) != 0 {
		t.Fatalf("the control: an error that carries none of the values leaked %v", l)
	}
	for name, v := range secrets {
		for _, text := range []string{m1eErrText(fmt.Errorf("could not read job %s", v)), m1eErrText(fmt.Errorf("could not read job %s", strings.ToUpper(v)))} {
			if l := m1eLeaks(text, secrets); !m1eContains(l, name) {
				t.Fatalf("an error carrying %s (%q) was not reported as leaking it: %v", name, v, l)
			}
		}
	}
	if m1eShape([]int{100, 100, 20}) != "100+100+20" || m1eShape(make([]int, 9)) != "8 x 0, last 0" {
		t.Fatal("the page shape rendering is wrong")
	}

	// the contract edge cases (m1_node_contract_edge_test.go): the helpers of that test, and the allowlist of the drift test
	oversize := m1xRows("edge-cap", "nomx.fixture.test", 501)
	seenID := map[string]bool{}
	for _, r := range oversize {
		if seenID[r.ItemID] || strings.TrimSpace(r.ItemID) == "" {
			t.Fatalf("the oversize rows repeat or blank an item_id (%q): the module would answer 400, not 413", r.ItemID)
		}
		seenID[r.ItemID] = true
		if _, err := norm.Normalize(r.Address); err != nil {
			t.Fatalf("the oversize row %q has an address that fails norm/v1: a 413 must be the only thing wrong with the request", r.ItemID)
		}
	}
	if len(oversize) != 501 {
		t.Fatalf("m1xRows made %d row(s), want 501", len(oversize))
	}
	refusal := func(status int, code string, details ...modclient.Detail) error {
		return &modclient.Error{Kind: modclient.Refused, Module: modclient.M1, Route: "m1.submit", Status: status, Code: code, Details: details}
	}
	items413 := modclient.Detail{Field: "items", Issue: "out_of_range"}
	if p := m1xRefusal("x", refusal(413, "payload_too_large", items413), 413, "payload_too_large", items413); len(p) != 0 {
		t.Fatalf("the control: a 413 payload_too_large naming items:out_of_range is what the edge test expects, got %v", p)
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"no error at all (the oversize request was accepted)", nil, "no error"},
		{"another status", refusal(400, "payload_too_large", items413), "HTTP status 400"},
		{"another code", refusal(413, "validation_failed", items413), `module error code "validation_failed"`},
		{"details that do not name the field", refusal(413, "payload_too_large", modclient.Detail{Field: "addresses", Issue: "out_of_range"}), "do not name items"},
		{"details that do not name the issue", refusal(413, "payload_too_large", modclient.Detail{Field: "items", Issue: "required"}), "out_of_range"},
		{"an error of another kind", &modclient.Error{Kind: modclient.Internal, Status: 500, Code: "internal_error"}, "kind internal"},
	} {
		if p := m1xRefusal("x", c.err, 413, "payload_too_large", items413); !m1eContains(p, c.want) {
			t.Fatalf("%s: the refusal check said %v, want a line containing %q", c.name, p, c.want)
		}
	}
	declared := map[string]bool{}
	for _, rc := range m1ad.Contracts() {
		declared[string(rc.Module)+"|"+rc.Route+"|"+rc.Case] = true
	}
	for key, why := range m1DriftNotElicited {
		if !declared[key] {
			t.Fatalf("the drift test allows %q, which names no contract of module 1: an allowlist entry for a contract that does not exist is red at Finish", key)
		}
		if len(strings.TrimSpace(why)) < 40 {
			t.Fatalf("the allowlist reason for %q is %q: a reason says what would have to happen in the module", key, why)
		}
	}
	if len(m1DriftNotElicited) != 3 {
		t.Fatalf("the drift test allows %d contract(s), the edge test is written for the three that need module 1 to fail on its own (paused, unavailable, internal_error)", len(m1DriftNotElicited))
	}
}

// m1ePageItemsForTest and m1ePageInputsForTest are the pagination rows and their inputs, for the pure test.
func m1ePageItemsForTest(e *m1nEnv) []m1eItem { return m1eBuildPageItems(e, m1ePageRows) }

func m1ePageInputsForTest(e *m1nEnv) []m1ad.Input {
	items := m1ePageItemsForTest(e)
	out := make([]m1ad.Input, len(items))
	for i, it := range items {
		out[i] = m1ad.Input{ItemRef: it.ref, Address: it.address}
	}
	return out
}

// m1eContains says whether some line holds the fragment.
func m1eContains(lines []string, fragment string) bool {
	for _, l := range lines {
		if strings.Contains(l, fragment) {
			return true
		}
	}
	return false
}
