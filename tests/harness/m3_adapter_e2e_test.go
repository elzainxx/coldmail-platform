//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/m3admin"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 3, chunks B9, B10 and B11 for module 3: the platform's module 3 ADAPTER (internal/adapters/m3 over internal/adapters/sendapi) driven end to end against the real module 3 of the harness stack
// (its Control, its three workers, the controllable SMTP sink as the accounts' SMTP target, the entitlement loops of the platform service). Five tests, in the order they run; the outage proof (B12) is
// m3_adapter_e2e_outage_test.go. Helpers and the oracle: m3_adapter_e2e_helpers_test.go. Evidence lines start with `NOTE m3a`.
//
//	TestM3Adapter_EndToEndAccountsRunReportClassify              B9   accounts (create, test button, bulk add), quote, pre-check, a campaign of 12 (sent, bounced, deferred then sent) and a tenant whose items fail, report, Classify, the books
//	TestM3Adapter_SuspensionRoundTripAndTheUnexplainedConflict   B9   the platform's real suspension path, what the adapter reads back, the module's 409 and what the adapter says of it, the resume
//	TestM3Adapter_PaginationAcrossPageBoundaries                 B10  45 items read at seven page sizes whose boundaries fall at the start, the middle and the end; two look-ahead walkers as the control
//	TestM3Adapter_AccountListPaginationAcrossABoundary           B10  203 accounts, so the list has two pages at the adapter's fixed page size of 200; a look-ahead walker as the control
//	TestM3Adapter_CrossTenantReadsAreRefused                     B11  tenant B asks for tenant A's campaign, jobs and accounts through its own adapter; A's own adapter gets A's rows (control); the admin route refuses the service token
//
// They run beside the other module 3 tests on the same three workers (one campaign per user, a global sink mode), so each finishes its own campaign and leaves the sink idle and in its default mode.

// TestM3Adapter_EndToEndAccountsRunReportClassify (B9): tenant A (entitled the way the module 3 live tests entitle a user: entitlements.Store, delivered by the platform service) adds accounts through the
// adapter, asks for a quote, pre-checks and runs a campaign of twelve, reads the report to the end and classifies it. Tenant F, whose only account cannot log in to the sink, runs two items that module 3
// fails (account exhaustion). The recipients drive every outcome the controllable sink can produce that module 3 reports as sent, bounced or failed:
//
//	ok      x8  accepted: sent, no error class, no provider text, one message at the sink
//	bounce  x2  answered 550 at RCPT: bounced, bounce_permanent, a provider text naming 550, ONE rejected RCPT (never retried), no message
//	defer   x2  answered 451 until each has been retried (attempt_count >= 2), then the sink relents: sent, and the sink counted at least two deferrals before its one acceptance
//	fail    x2  (tenant F) the account's verification failed at creation (the sink refused its AUTH): module 3 fails the READY items on its next tick (state failed, failed_at set, no account, no error class)
//
// The steps and what each proves:
//
//	accounts   CreateAccount (verified, aligned, no secret in any printed verb), TestAccount (the test button against the sink; a refused AUTH is state failed / verify_auth_failed; the account verifies again),
//	           BulkAddAccounts with a duplicate and an invalid row (the closed code set), AllAccounts, AccountEvents; the strict adapter (production) refuses the sink's private address before anything is sent
//	quote      Feasibility: infeasible with no account (no_eligible_accounts), feasible with three, the caps equal module 3's own dials, exceeds_queue_cap one over the cap; Quote's price fields are inert (0)
//	pre-check  PreCheckRun with truthful RunFacts (the platform's campaign cap is m3aCampaignCap) says nothing; each platform fact that is false names its cause
//	run, poll  every item a NEW job (202), the campaign report read to the end through every page
//	(a)        every item reported once, in the order it was run, in the (state, error class, provider text) of its kind, from one of the tenant's accounts; totals_by_account agrees with the items
//	(b)        the sink's per-recipient counters agree with the report
//	(c)        Classify settles every terminal item free by row M3-01 and charges nothing, equal to what BOTH charge.Decide and a hand-written oracle say; the ledger, the balances and the reserves of the tenant
//	           gain nothing, and money.Settle refuses a module 3 job (ErrNotMetered)
//	(d)        unmapped is empty; platform.balance_drift and platform.reserve_overdrawn are empty; a second Run of an item is the module's replay (200) and sends nothing
//
// THE CONTROLS. The same comparison functions are run on corrupted copies of the very data they just accepted: an outcome changed, an item dropped, an item duplicated, an error class and a provider text
// changed, a message duplicated at the sink, a charge in the ledger, a mapping that drifts from the hand oracle, an item that is not final in the settle list. Each must be reported, for the reason it names.
func TestM3Adapter_EndToEndAccountsRunReportClassify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	// the retry ladder of module 3 (30 s, 120 s, 600 s) is the long pole of a deferred item: shortened for the run, restored at its end (module 3 reads it fresh at every outcome)
	e.setDial(t, "retry_backoff_schedule", `{"ladder_seconds":[2,2,2],"max_seconds":2}`)

	// ---- F: the tenant whose only account cannot log in: its items end failed ----
	f := e.seedTenant(t, "fail", []string{"fail", "fail"})
	e.setPacing(t, f, 1, 0, 0)
	e.setMode(t, m3hMode{FailAuth: true}) // every AUTH is answered 535: module 3's verify-on-add records the account as failed
	fAcct, err := f.C.CreateAccount(ctx, e.newAccount("f", f, e.sinkIP))
	e.setMode(t, m3hMode{})
	if err != nil {
		t.Fatalf("CreateAccount for F through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	e.checkAccount(t, f, e.sinkIP, "F's account", fAcct)
	if fAcct.Verification.State != m3.VerificationFailed || fAcct.Verification.Code != m3.VerifyAuthFailed {
		t.Fatalf("F's account was created while the sink refused every AUTH: module 3 says verification %q code %q, want failed / verify_auth_failed (a failed verification is not an error: the account exists and never sends)", fAcct.Verification.State, fAcct.Verification.Code)
	}
	f.Accounts = append(f.Accounts, fAcct)
	t.Logf("NOTE m3a accounts F: created while the sink refused AUTH: verification %s code %s, alignment %s; its items will be failed by module 3 (no eligible account)", fAcct.Verification.State, fAcct.Verification.Code, fAcct.FromAlignment.State)

	// ---- A ----
	a := e.seedTenant(t, "e2e", []string{"ok", "bounce", "ok", "defer", "ok", "ok", "bounce", "ok", "defer", "ok", "ok", "ok"})
	n := len(a.Items)
	wantSent, wantBounced := len(a.emails("ok"))+len(a.emails("defer")), len(a.emails("bounce"))
	if n != 12 || wantSent != 10 || wantBounced != 2 || len(a.emails("defer")) != 2 {
		t.Fatalf("the scenario has %d item(s): %d sent, %d bounced, %d deferred: it needs 12, 10, 2 and 2", n, wantSent, wantBounced, len(a.emails("defer")))
	}
	e.setPacing(t, a, 3, 0, 1)

	// the quote reads the user's accounts: before any, no recipient count is feasible
	f0, err := a.C.Feasibility(ctx, int64(n))
	if err != nil {
		t.Fatalf("Feasibility (no account yet) through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if f0.Feasible || len(f0.Reasons) != 1 || f0.Reasons[0].Code != m3.InfeasibleNoEligibleAccounts || f0.Capacity.EligibleAccounts != 0 {
		t.Errorf("a user with no account asked for a quote of %d: feasible %v reasons %+v eligible %d, want infeasible, one reason no_eligible_accounts, no eligible account", n, f0.Feasible, f0.Reasons, f0.Capacity.EligibleAccounts)
	}

	// the strict adapter (what production runs) refuses the sink's private address before anything is sent
	strict := e.bind(t, a, e.adStrict)
	_, err = strict.CreateAccount(ctx, e.newAccount("strict", a, e.sinkIP))
	if !errors.Is(err, m3.ErrHostNotAllowed) || modclient.KindOf(err) != modclient.Invalid || m3.MaybeApplied(err) {
		t.Errorf("the strict adapter's CreateAccount for a private SMTP host answered %v (kind %s, may have been applied %v), want ErrHostNotAllowed, kind invalid, nothing sent", err, modclient.KindOf(err), m3.MaybeApplied(err))
	}
	if accts, err := a.C.AllAccounts(ctx); err != nil || len(accts) != 0 {
		t.Fatalf("after the strict adapter's refusal the tenant has %d account(s) (error %v), want none: the refused call must not have reached module 3", len(accts), err)
	}

	// the entitlement, through the platform's real path: the store writes, the platform SERVICE delivers it to module 3 and reads it back
	en := e.enable(t, a, "enable")
	st := e.waitSync(t, a, 1, false, "active", 60*time.Second)
	t.Logf("NOTE m3a entitlement A: enabled (ledger row %d), delivered by the service: %s", en.LedgerID, m3aSyncString(st))

	// ---- accounts through the adapter ----
	acct1 := e.createVerified(t, a, "1", e.sinkIP)
	tr, err := a.C.TestAccount(ctx, acct1.ID, false)
	if err != nil {
		t.Fatalf("TestAccount through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if tr.Verification.State != m3.VerificationVerified || tr.Alignment.State != m3.AlignmentAligned || tr.Alignment.NotStored {
		t.Errorf("the test button against the sink answered verification %q (code %q) alignment %q (not stored %v), want verified and aligned", tr.Verification.State, tr.Verification.Code, tr.Alignment.State, tr.Alignment.NotStored)
	}
	e.setMode(t, m3hMode{FailAuth: true}) // the control: a credential the sink refuses is state failed, a SUCCESSFUL call, with module 3's closed code
	trf, err := a.C.TestAccount(ctx, acct1.ID, false)
	e.setMode(t, m3hMode{})
	if err != nil {
		t.Fatalf("TestAccount (the sink refuses AUTH) through the adapter failed (%s): a failed credential is a 200: %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if trf.Verification.State != m3.VerificationFailed || trf.Verification.Code != m3.VerifyAuthFailed {
		t.Errorf("the test button against a sink that refuses AUTH answered verification %q code %q, want failed / verify_auth_failed", trf.Verification.State, trf.Verification.Code)
	}
	tr2, err := a.C.TestAccount(ctx, acct1.ID, false) // the sink accepts again: the account verifies again, so the campaign below can send
	if err != nil || tr2.Verification.State != m3.VerificationVerified {
		t.Fatalf("the test button after the sink relented answered %+v (error %v), want verified: the account must be eligible for the campaign", tr2, err)
	}
	got1, err := a.C.GetAccount(ctx, acct1.ID)
	if err != nil || got1.ID != acct1.ID || got1.Verification.State != m3.VerificationVerified || got1.SMTPHost != e.sinkIP {
		t.Fatalf("GetAccount of the first account answered %+v (error %v), want it verified at the sink's address", got1, err)
	}
	t.Logf("NOTE m3a test button: against the sink verification %s / alignment %s; with AUTH refused %s code %s; accepted again %s; GetAccount verified %s", tr.Verification.State, tr.Alignment.State, trf.Verification.State, trf.Verification.Code, tr2.Verification.State, got1.Verification.State)

	dup := e.newAccount("1", a, e.sinkIP) // the same host and username as the first account
	bad := e.newAccount("bad", a, e.sinkIP)
	bad.FromAddress = "not-an-address"
	bulkStart := time.Now()
	bulk, err := a.C.BulkAddAccounts(ctx, []m3.NewAccount{e.newAccount("2", a, e.sinkIP), dup, bad, e.newAccount("3", a, e.sinkIP)})
	if err != nil {
		t.Fatalf("BulkAddAccounts through the adapter failed (%s, may have been applied: %v): %s", modclient.KindOf(err), m3.MaybeApplied(err), e.raw.scrub(err.Error()))
	}
	if len(bulk.Rows) != 4 || bulk.RequestID == "" {
		t.Fatalf("BulkAddAccounts answered %d row(s) with request id %q, want one per row sent (4) and the module's request id", len(bulk.Rows), bulk.RequestID)
	}
	for i, wantCreated := range []bool{true, false, false, true} {
		r := bulk.Rows[i]
		if r.Row != i+1 || r.Created != wantCreated {
			t.Errorf("bulk row %d: row %d created %v, want row %d created %v", i+1, r.Row, r.Created, i+1, wantCreated)
		}
		if wantCreated && (r.AccountID == "" || r.Verification != m3.VerificationVerified || r.Alignment != m3.AlignmentAligned) {
			t.Errorf("bulk row %d was created with account %q verification %q alignment %q, want an account verified and aligned", i+1, r.AccountID, r.Verification, r.Alignment)
		}
	}
	if bulk.Rows[1].Reason != m3.RejectDuplicate || bulk.Rows[2].Reason != m3.RejectValidation {
		t.Errorf("the rejected rows say %q (the duplicate) and %q (the invalid address), want the closed codes %q and %q", bulk.Rows[1].Reason, bulk.Rows[2].Reason, m3.RejectDuplicate, m3.RejectValidation)
	}
	t.Logf("NOTE m3a bulk add: 4 rows in %s: row 1 created, row 2 %s (a duplicate of the first account), row 3 %s (field issues %v), row 4 created; request %s", time.Since(bulkStart).Round(time.Millisecond), bulk.Rows[1].Reason, bulk.Rows[2].Reason, bulk.Rows[2].Fields, bulk.RequestID)

	accts, err := a.C.AllAccounts(ctx)
	if err != nil {
		t.Fatalf("AllAccounts through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if len(accts) != 3 {
		t.Fatalf("the tenant has %d account(s) after one create and a bulk add of two good rows, want 3", len(accts))
	}
	a.Accounts = nil
	for i, ac := range accts {
		e.checkAccount(t, a, e.sinkIP, fmt.Sprintf("account %d of the list", i+1), ac)
		if ac.Verification.State != m3.VerificationVerified || ac.FromAlignment.State != m3.AlignmentAligned || ac.Paused || ac.Quarantined || ac.Disabled {
			t.Errorf("account %d of the list is verification %q alignment %q paused %v quarantined %v disabled %v, want a verified, aligned account in service", i+1, ac.Verification.State, ac.FromAlignment.State, ac.Paused, ac.Quarantined, ac.Disabled)
		}
		a.Accounts = append(a.Accounts, ac)
	}
	ev, err := a.C.AccountEvents(ctx, acct1.ID, "")
	if err != nil || len(ev.Items) == 0 {
		t.Errorf("AccountEvents of the first account answered %d event(s) (error %v), want its audit trail", len(ev.Items), err)
	}
	var actions []string
	for _, x := range ev.Items {
		if x.AccountID != acct1.ID {
			t.Errorf("an event of the first account names account %q", x.AccountID)
		}
		actions = append(actions, x.Actor+":"+x.Action)
	}
	t.Logf("NOTE m3a accounts A: %d accounts (create + bulk), all verified and aligned at %s:%d; the first account's audit trail: %v", len(accts), "the sink", m3aSMTPPort, actions)

	// ---- the quote, the pre-check ----
	queueCap, acctCap := e.dialInt(t, "max_queued_jobs"), e.dialInt(t, "account_count_cap_per_user")
	q, err := a.C.Quote(ctx, int64(n))
	if err != nil {
		t.Fatalf("Quote through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	fz, err := a.C.Feasibility(ctx, int64(n))
	if err != nil {
		t.Fatalf("Feasibility through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if !fz.Feasible || len(fz.Reasons) != 0 || fz.RecipientCount != int64(n) || fz.Capacity.EligibleAccounts != 3 || fz.Caps.QueueDepthCap != queueCap || fz.Caps.AccountCountCap != acctCap {
		t.Errorf("the feasibility block is %+v, want feasible with no reason, %d recipients, 3 eligible accounts, queue depth cap %d and account cap %d (module 3's own dials)", fz, n, queueCap, acctCap)
	}
	if q.Cost != 0 || q.UnitPriceCredits != 0 || q.Currency == "" || q.RecipientCount != int64(n) || q.Feasible == nil || !*q.Feasible || fz.Cost != 0 {
		t.Errorf("the quote is %+v, want an inert price (cost 0, unit 0, a currency) for %d recipients and feasible", q, n)
	}
	var est string
	if fz.Capacity.EstimatedDurationSec != nil {
		est = fmt.Sprintf("%ds (%s)", *fz.Capacity.EstimatedDurationSec, fz.Capacity.EstimateBasis)
	}
	t.Logf("NOTE m3a quote: %d recipients: feasible %v, reasons %d, cost %d (unit %d, %s, tier %s), caps queue %d / daily send %d / accounts %d, capacity eligible %d, remaining today %d, lanes %d, projection %s, warnings %d; with no account: feasible %v reasons %v",
		n, fz.Feasible, len(fz.Reasons), fz.Cost, fz.UnitPriceCredits, fz.Currency, fz.Tier, fz.Caps.QueueDepthCap, fz.Caps.DailySendCap, fz.Caps.AccountCountCap, fz.Capacity.EligibleAccounts, fz.Capacity.DailyCapacityRemaining, fz.Capacity.EffectiveConcurrency, est, len(fz.Warnings), f0.Feasible, f0.Reasons)
	fx, err := a.C.Feasibility(ctx, queueCap+1)
	if err != nil {
		t.Fatalf("Feasibility one over the queue cap failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if fx.Feasible || len(fx.Reasons) != 1 || fx.Reasons[0].Code != m3.InfeasibleExceedsQueueCap || fx.Reasons[0].Limit == nil || *fx.Reasons[0].Limit != queueCap || fx.Reasons[0].Requested == nil || *fx.Reasons[0].Requested != queueCap+1 || fx.Reasons[0].CapName != "queue_depth_cap" {
		t.Errorf("a quote of %d recipients (one over the queue cap) is %+v, want infeasible with exceeds_queue_cap, limit %d, requested %d, cap name queue_depth_cap", queueCap+1, fx, queueCap, queueCap+1)
	}

	ent, err := e.store.Get(ctx, a.UserID)
	if err != nil || !ent.Exists || !ent.Enabled {
		t.Fatalf("Store.Get(A) = %+v (error %v), want an enabled entitlement", ent, err)
	}
	facts := m3.RunFacts{Entitled: ent.Enabled, JobActive: false, RecipientCount: int64(n), LiveQueuedJobs: 0, QueueDepthCap: fz.QueueDepthLimit(), CampaignSizeCap: m3.KnownLimit(m3aCampaignCap)}
	if err := m3.PreCheckRun(facts); err != nil || len(facts.NotChecked()) != 0 {
		t.Fatalf("PreCheckRun with the platform's truthful facts (%+v) answered %v (not checked: %v), want nothing and every check made", facts, err, facts.NotChecked())
	}
	for _, c := range []struct {
		name string
		mod  func(m3.RunFacts) m3.RunFacts
		want error
	}{
		{"the entitlement is not active", func(f m3.RunFacts) m3.RunFacts { f.Entitled = false; return f }, m3.ErrNotEntitled},
		{"a job of the user is active", func(f m3.RunFacts) m3.RunFacts { f.JobActive = true; return f }, m3.ErrJobActive},
		{"the campaign is over the platform's campaign cap", func(f m3.RunFacts) m3.RunFacts { f.RecipientCount = m3aCampaignCap + 1; return f }, m3.ErrOverCampaignCap},
		{"the campaign is over the module's own queue cap (the number its quote states)", func(f m3.RunFacts) m3.RunFacts { f.RecipientCount = queueCap + 1; return f }, m3.ErrOverQueueCap},
	} {
		err := m3.PreCheckRun(c.mod(facts))
		if !errors.Is(err, c.want) || modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("control: PreCheckRun when %s answered %v (kind %s), want %v and kind invalid", c.name, err, modclient.KindOf(err), c.want)
		}
	}
	unknown := facts
	unknown.CampaignSizeCap = m3.Limit{}
	unknown.RecipientCount = m3aCampaignCap + 1
	if err := m3.PreCheckRun(unknown); errors.Is(err, m3.ErrOverCampaignCap) || len(unknown.NotChecked()) != 1 || unknown.NotChecked()[0] != "campaign_size_cap" {
		t.Errorf("control: with the campaign cap unknown PreCheckRun answered %v and says it did not check %v, want no refusal for a number nobody supplied and the campaign size named as not checked", err, unknown.NotChecked())
	}
	t.Logf("NOTE m3a pre-check: truthful facts (entitled, no active job, %d recipients, queue cap %d from the quote, campaign cap %d the platform's own) -> nothing, not checked []; each false fact names its cause (not entitled, job active, over the campaign cap, over the queue cap); an unsupplied cap is reported as not checked", n, queueCap, m3aCampaignCap)

	// ---- run, poll ----
	e.setMode(t, m3hMode{RejectRcpt: a.emails("bounce"), RejectCode: 550, DeferRcpt: a.emails("defer")})
	took := e.runAll(t, a)
	tookF := e.runAll(t, f)
	t.Logf("NOTE m3a run: A %d new jobs (202 queued, class pending) in %s, F %d in %s; the sink answers 550 to %d recipients and 451 to %d", n, took.Round(time.Millisecond), len(f.Items), tookF.Round(time.Millisecond), wantBounced, len(a.emails("defer")))
	repF := e.waitCampaign(t, f, 90*time.Second, nil)

	const minDeferred = 2
	relented := false
	relent := func(rep sendapi.CampaignReport) {
		if relented {
			return
		}
		byID := map[string]sendapi.Item{}
		for _, it := range rep.Items {
			byID[it.JobItemID] = it
		}
		for _, it := range a.Items {
			if it.Kind == "defer" && byID[it.ID].AttemptCount < minDeferred {
				return
			}
		}
		relented = true
		e.setMode(t, m3hMode{RejectRcpt: a.emails("bounce"), RejectCode: 550})
		t.Logf("NOTE m3a relent: each deferred item has been retried (attempt_count >= %d in the adapter's report): the sink stops answering 451, the 550s stay", minDeferred)
	}
	repA := e.waitCampaign(t, a, 180*time.Second, relent)
	if !relented {
		t.Fatalf("the campaign ended without the deferred items having been retried: the sink never relented, so the deferred-then-sent path was not exercised")
	}
	t.Logf("NOTE m3a report: A %s, %d page(s), progress terminal=%d complete=%v, items match total %v, %d account row(s) in totals_by_account; F %s", m3aTotalsLine(repA.Totals), repA.Pages, repA.Progress.Terminal, repA.Progress.IsComplete, repA.ItemsMatchTotal(), len(repA.ByAccount), m3aTotalsLine(repF.Totals))

	// ---- (a) every item once, in the state, error class and provider text of its kind ----
	m3aProblems(t, "(a) A's report against what was submitted", m3aCompareReport(a, repA.Items))
	m3aProblems(t, "(a) F's report against what was submitted", m3aCompareReport(f, repF.Items))
	ta, tf := repA.Totals, repF.Totals
	if ta.Total != int64(n) || ta.Sent != int64(wantSent) || ta.Bounced != int64(wantBounced) || ta.Failed != 0 || ta.InFlight != 0 || ta.Pending != 0 || ta.WaitingForCapacity != 0 || ta.Cancelled != 0 {
		t.Errorf("(a) A's totals are (%s), want total %d sent %d bounced %d and nothing else", m3aTotalsLine(ta), n, wantSent, wantBounced)
	}
	if tf.Total != 2 || tf.Failed != 2 || tf.Sent != 0 || tf.Bounced != 0 || tf.InFlight != 0 || tf.Pending != 0 || tf.WaitingForCapacity != 0 {
		t.Errorf("(a) F's totals are (%s), want total 2 failed 2 and nothing else", m3aTotalsLine(tf))
	}
	perAccount := map[string][3]int64{}
	for _, it := range repA.Items {
		if it.AccountID == nil {
			continue
		}
		c := perAccount[*it.AccountID]
		switch it.State {
		case "sent":
			c[0]++
		case "bounced":
			c[1]++
		case "failed":
			c[2]++
		}
		perAccount[*it.AccountID] = c
	}
	var sumSent, sumBounced int64
	for _, ba := range repA.ByAccount {
		sumSent += ba.Sent
		sumBounced += ba.Bounced
		if c := perAccount[ba.AccountID]; c[0] != ba.Sent || c[1] != ba.Bounced || c[2] != ba.Failed || ba.InFlight != 0 || !a.accountIDs()[ba.AccountID] {
			t.Errorf("(a) totals_by_account says account %s sent %d bounced %d failed %d in flight %d, its items say sent %d bounced %d failed %d (and it must be one of the tenant's accounts)", ba.AccountID, ba.Sent, ba.Bounced, ba.Failed, ba.InFlight, c[0], c[1], c[2])
		}
	}
	if sumSent != ta.Sent || sumBounced != ta.Bounced {
		t.Errorf("(a) totals_by_account adds up to sent %d bounced %d, the totals say sent %d bounced %d", sumSent, sumBounced, ta.Sent, ta.Bounced)
	}
	for _, kind := range []string{"ok", "bounce", "defer"} { // the single-job report names the same job the run answered, and decodes its omit-until-set keys
		var first *m3aItem
		for _, it := range a.Items {
			if it.Kind == kind {
				first = it
				break
			}
		}
		one, err := a.C.ReportJob(ctx, first.ID)
		byJob, err2 := a.C.ReportJobByID(ctx, first.JobID)
		if err != nil || err2 != nil {
			t.Errorf("(a) ReportJob / ReportJobByID of %s item %02d failed: %v / %v", kind, first.Index, err, err2)
			continue
		}
		w := m3aWantByKind[kind]
		if one.JobID != first.JobID || one.JobItemID != first.ID || one.State != w.State || m3aText(one.ErrorClass) != m3aNullIf(w.ErrorClass) || byJob.JobID != first.JobID || byJob.State != one.State ||
			(w.SMTPContains == "") != (one.SMTPResponse == nil) || (w.SMTPContains != "" && !strings.Contains(*one.SMTPResponse, w.SMTPContains)) || one.AccountID == nil || !a.accountIDs()[*one.AccountID] {
			t.Errorf("(a) the single-job report of %s item %02d is job %s %s/%s (provider text %s, account %s), want job %s %s/%s", kind, first.Index, one.JobID, one.State, m3aText(one.ErrorClass), m3aText(one.SMTPResponse), m3aText(one.AccountID), first.JobID, w.State, m3aNullIf(w.ErrorClass))
		}
	}
	if one, err := f.C.ReportJob(ctx, f.Items[0].ID); err != nil || one.State != "failed" || one.FailedAt == nil || one.AccountID != nil || one.ErrorClass != nil {
		t.Errorf("(a) the single-job report of F's first item is %+v (error %v), want failed with failed_at set, no account and no error class", one, err)
	}

	// ---- (b) the sink saw what the report says ----
	view := e.sinkView(t)
	e.requireNoDups(t, view)
	m3aProblems(t, "(b) the sink against A's report", m3aCompareSink(a, repA.Items, view, minDeferred))
	m3aProblems(t, "(b) the sink against F's report", m3aCompareSink(f, repF.Items, view, 0))
	if got := view.of(a); got != wantSent {
		t.Errorf("(b) the sink holds %d message(s) to A's recipients, want exactly the %d sent", got, wantSent)
	}
	if got := view.of(f); got != 0 {
		t.Errorf("(b) the sink holds %d message(s) to F's recipients, want none: they never reached a worker", got)
	}

	// ---- (c) classify, and what the platform charges: nothing ----
	clA, err := a.C.Classify(repA.Items)
	if err != nil {
		t.Fatalf("Classify refused A's report: %s", e.raw.scrub(err.Error()))
	}
	clF, err := f.C.Classify(repF.Items)
	if err != nil {
		t.Fatalf("Classify refused F's report: %s", e.raw.scrub(err.Error()))
	}
	if len(clA.Settle) != n || len(clA.NotYet) != 0 || len(clA.Unmapped) != 0 || len(clF.Settle) != 2 || len(clF.NotYet) != 0 || len(clF.Unmapped) != 0 {
		t.Fatalf("Classify sorted A into settle %d / not yet %v / unmapped %v and F into settle %d / not yet %v / unmapped %v, want every terminal item settled (free) and nothing else", len(clA.Settle), clA.NotYet, clA.Unmapped, len(clF.Settle), clF.NotYet, clF.Unmapped)
	}
	m3aProblems(t, "(c) A's report, Classify and the charge table", m3aCompareClassified(repA.Items, clA, m3aDecide))
	m3aProblems(t, "(c) F's report, Classify and the charge table", m3aCompareClassified(repF.Items, clF, m3aDecide))
	booksA, booksF := e.books(t, a.UserID), e.books(t, f.UserID)
	m3aProblems(t, "(c) A's books", m3aCompareBooks(booksA))
	m3aProblems(t, "(c) F's books", m3aCompareBooks(booksF))
	if booksA.Ledger != 1 || booksA.Entitlement != "enabled" || booksF.Ledger != 0 || booksF.Entitlement != "none" {
		t.Errorf("(c) A's ledger holds %d row(s) with the entitlement %s and F's %d with %s, want exactly A's one m3_enable row and nothing for F", booksA.Ledger, booksA.Entitlement, booksF.Ledger, booksF.Entitlement)
	}
	var job string
	if err := e.db.QueryRow(ctx, "INSERT INTO platform.jobs (user_id, module_id) VALUES ($1::uuid, 'm3') RETURNING id::text", a.UserID).Scan(&job); err != nil {
		t.Fatalf("create the platform job of A: %v", err)
	}
	if _, err := e.money.Settle(ctx, money.SettleReq{JobID: job, Items: clA.Settle}); !errors.Is(err, money.ErrNotMetered) {
		t.Errorf("(c) money.Settle of module 3's classified items answered %v, want ErrNotMetered: the platform's money core must refuse a module 3 job, whose only gate is the entitlement", err)
	}
	if after := e.books(t, a.UserID); len(m3aBooksDiff(booksA, after)) != 0 {
		t.Errorf("(c) A's books changed after the refused settle: %v", m3aBooksDiff(booksA, after))
	}
	e.requireBooks(t, "(d) after the module 3 campaigns")
	for _, it := range a.Items {
		rp := m3aByID(repA.Items)[it.ID]
		c := view.Rcpt[it.Email]
		d, h := m3aDecide(charge.Outcome{Module: "m3", State: rp.State}), m3aHand(rp.State)
		t.Logf("NOTE m3a item %02d %-6s %s: reported %s/%s/%s (attempt_count %d, advisory) by account %s; sink RCPT accepted=%d rejected=%d deferred=%d messages=%d; charge.Decide %s %q rule %s, hand oracle %s; Classify settle=%v",
			it.Index, it.Kind, it.Email, rp.State, m3aText(rp.ErrorClass), m3aText(rp.SMTPResponse), rp.AttemptCount, m3aShortID(rp.AccountID), c.Accepted, c.Rejected, c.Deferred, view.Counts[it.Email], d.Kind, d.Unit, d.Rule, h, m3aInSettle(clA, it.ID))
	}
	for _, it := range f.Items {
		rp := m3aByID(repF.Items)[it.ID]
		t.Logf("NOTE m3a item F%02d %-4s %s: reported %s/%s/%s, account %s, failed_at set %v; sink RCPT %+v messages=%d; charge.Decide %s rule %s; Classify settle=%v", it.Index, it.Kind, it.Email, rp.State, m3aText(rp.ErrorClass), m3aText(rp.SMTPResponse), m3aShortID(rp.AccountID), rp.FailedAt != nil, view.Rcpt[it.Email], view.Counts[it.Email], m3aDecide(charge.Outcome{Module: "m3", State: rp.State}).Kind, m3aDecide(charge.Outcome{Module: "m3", State: rp.State}).Rule, m3aInSettle(clF, it.ID))
	}
	t.Logf("NOTE m3a classify: A settle %d (all free by M3-01, no unit), not yet %d, unmapped %d; F settle %d, not yet %d, unmapped %d; the ledger of A holds %d row (its m3_enable), F's %d; balances %d/%d, reserves %d/%d, monetary rows %d/%d; money.Settle on a module 3 job: ErrNotMetered; platform.balance_drift and platform.reserve_overdrawn are empty",
		len(clA.Settle), len(clA.NotYet), len(clA.Unmapped), len(clF.Settle), len(clF.NotYet), len(clF.Unmapped), booksA.Ledger, booksF.Ledger, booksA.Balances, booksF.Balances, booksA.Reserves, booksF.Reserves, booksA.Monetary, booksF.Monetary)

	// ---- (d) a second Run is the module's replay, and sends nothing ----
	replay, err := a.C.Run(ctx, a.Items[0].ID)
	if err != nil || replay.Created || replay.JobID != a.Items[0].JobID || replay.State != "sent" || replay.Class != sendapi.ClassTerminal {
		t.Errorf("(d) a second Run of item 01 answered %+v (error %v), want the same job %s, not created, state sent, class terminal", replay, err, a.Items[0].JobID)
	}
	time.Sleep(3 * time.Second)
	if after := e.sinkView(t); len(after.Msgs) != len(view.Msgs) || after.of(a) != view.of(a) {
		t.Errorf("(d) the sink holds %d message(s) after the replay, %d before: a replay sent again", len(after.Msgs), len(view.Msgs))
	}
	if jobs := e.jobsOf(t, a.UserID); len(jobs) != n || e.jobStates(jobs) != fmt.Sprintf("bounced=%d sent=%d", wantBounced, wantSent) {
		t.Errorf("(d) module 3's own jobs of A are %d (%s), want %d: bounced=%d sent=%d", len(jobs), e.jobStates(jobs), n, wantBounced, wantSent)
	}
	t.Logf("NOTE m3a replay: a second Run of item 01 answered the same job (200, not created, sent); the sink still holds %d message(s) to A's recipients; module 3's own jobs of A: %s", view.of(a), e.jobStates(e.jobsOf(t, a.UserID)))

	// ---- the controls: the SAME comparisons on corrupted copies of what they just accepted ----
	copyItems := func() []sendapi.Item { return append([]sendapi.Item(nil), repA.Items...) }
	firstItem := func(items []sendapi.Item, state string) int {
		for i, it := range items {
			if it.State == state {
				return i
			}
		}
		t.Fatalf("no %s item in the report: the control cannot be built", state)
		return -1
	}
	copyView := func() m3aView {
		c := m3aView{Counts: map[string]int{}, Rcpt: map[string]m3hRcpt{}, Msgs: append([]m3hSinkMsg(nil), view.Msgs...), Dups: append([]string(nil), view.Dups...)}
		for k, v := range view.Counts {
			c.Counts[k] = v
		}
		for k, v := range view.Rcpt {
			c.Rcpt[k] = v
		}
		return c
	}
	var caught []string
	check := func(name, want string, got []string) {
		if len(got) == 0 {
			t.Errorf("control: the comparison accepted a corrupted input (%s): it cannot fail", name)
			return
		}
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("control: the comparison rejected %q but not for the reason %q: %v", name, want, got)
			return
		}
		caught = append(caught, name)
	}
	{ // the report
		it := copyItems()
		it[firstItem(it, "sent")].State = "failed"
		check("a reported outcome changed (sent -> failed)", "is reported failed", m3aCompareReport(a, it))
	}
	{
		it := copyItems()
		i := firstItem(it, "sent")
		check("a reported item dropped", "missing "+it[i].JobItemID, m3aCompareReport(a, append(it[:i:i], it[i+1:]...)))
	}
	{
		it := copyItems()
		check("a reported item duplicated", "appears 2 times", m3aCompareReport(a, append(it, it[firstItem(it, "sent")])))
	}
	{
		it := copyItems()
		s := "timeout"
		it[firstItem(it, "bounced")].ErrorClass = &s
		check("an error class changed (bounce_permanent -> timeout)", "is reported bounced / timeout", m3aCompareReport(a, it))
	}
	{
		it := copyItems()
		s := "451 4.7.1 try later"
		it[firstItem(it, "bounced")].SMTPResponse = &s
		check("a provider text changed (550 -> 451)", "want one naming 550", m3aCompareReport(a, it))
	}
	{
		it := copyItems()
		it[firstItem(it, "sent")].ProviderMessageID = nil
		check("a sent item without its provider message id", "provider message id present false", m3aCompareReport(a, it))
	}
	{ // the sink
		v := copyView()
		for _, it := range a.Items {
			if it.Kind == "ok" {
				v.Counts[it.Email]++
				break
			}
		}
		check("a duplicated message at the sink", "the sink holds 2 message(s)", m3aCompareSink(a, repA.Items, v, minDeferred))
	}
	{
		v := copyView()
		for _, it := range a.Items {
			if it.Kind == "bounce" {
				c := v.Rcpt[it.Email]
				c.Rejected++
				v.Rcpt[it.Email] = c
				break
			}
		}
		check("a bounced recipient tried twice", "want exactly one rejection", m3aCompareSink(a, repA.Items, v, minDeferred))
	}
	{
		v := copyView()
		for _, it := range a.Items {
			if it.Kind == "defer" {
				c := v.Rcpt[it.Email]
				c.Deferred = 0
				v.Rcpt[it.Email] = c
				break
			}
		}
		check("a deferred item the sink never deferred", "deferral(s)", m3aCompareSink(a, repA.Items, v, minDeferred))
	}
	{
		v := copyView()
		v.Counts[f.Items[0].Email] = 1
		check("a message at the sink for an item that never reached a worker", "never reached a worker", m3aCompareSink(f, repF.Items, v, 0))
	}
	{ // Classify
		cl := sendapi.Classified{Settle: append(clA.Settle[:0:0], clA.Settle...), NotYet: clA.NotYet, Unmapped: clA.Unmapped}
		cl.Settle[0].Outcome.State = "failed"
		check("a settle outcome changed", "carries outcome", m3aCompareClassified(repA.Items, cl, m3aDecide))
	}
	{
		cl := sendapi.Classified{Settle: append(clA.Settle[:0:0], clA.Settle...)}
		cl.Settle = cl.Settle[1:]
		check("a settle item dropped", "lacks the terminal item", m3aCompareClassified(repA.Items, cl, m3aDecide))
	}
	{
		cl := sendapi.Classified{Settle: append(append(clA.Settle[:0:0], clA.Settle...), clA.Settle[0])}
		check("a settle item duplicated", "repeats", m3aCompareClassified(repA.Items, cl, m3aDecide))
	}
	check("a mapping that charges a bounce (drifting from the hand oracle)", "the hand oracle says free", m3aCompareClassified(repA.Items, clA, func(o charge.Outcome) m3aDecision {
		if o.State == "bounced" {
			return m3aDecision{Kind: "charge", Unit: "m2_bounced", Rule: "M2-02"}
		}
		return m3aDecide(o)
	}))
	{
		it := copyItems()
		it[firstItem(it, "sent")].State = "sending" // the module reports an item that is not final, while the settle list still carries it as sent
		check("an item that is not final in the settle list", "which is notfinal", m3aCompareClassified(it, clA, m3aDecide))
	}
	{ // the books
		b := booksA
		b.OtherLedger, b.Monetary = 1, 1
		check("a charge in the ledger of a module 3 tenant", "not entitlement rows", m3aCompareBooks(b))
		b = booksA
		b.Balances, b.Reserves = 1, 1
		check("a balance and a reserve of a module 3 tenant", "module 3 holds no credits", m3aCompareBooks(b))
		moved := booksA
		moved.Ledger++
		check("a ledger row that appeared between two readings", "the number of ledger rows changed", m3aBooksDiff(booksA, moved))
	}
	if len(caught) != m3aEndToEndControls {
		t.Errorf("control: %d of the %d corruptions were reported, want all of them", len(caught), m3aEndToEndControls)
	}
	t.Logf("NOTE m3a controls: the comparison functions reported %d of %d corruptions, each for its own reason: %s", len(caught), m3aEndToEndControls, strings.Join(caught, "; "))
}

func m3aByID(items []sendapi.Item) map[string]sendapi.Item {
	out := map[string]sendapi.Item{}
	for _, it := range items {
		out[it.JobItemID] = it
	}
	return out
}

func m3aShortID(p *string) string {
	if p == nil {
		return "-"
	}
	if len(*p) > 8 {
		return (*p)[:8]
	}
	return *p
}

func m3aInSettle(cl sendapi.Classified, id string) bool {
	for _, s := range cl.Settle {
		if s.ItemRef == id {
			return true
		}
	}
	return false
}

// TestM3Adapter_SuspensionRoundTripAndTheUnexplainedConflict (B9): the platform's REAL suspension path against the real module 3, and what the adapter says about it.
//
//	before      the tenant is entitled and delivered; Suspension reads exactly what module 3 holds (never suspended: version 0), the platform's delivery row and module 3's own table
//	suspend     entitlements.Store.Disable; the platform SERVICE delivers it and reads it back; Suspension then reads version 1, suspended, the instant module 3 recorded, equal to module 3's own table AND to what
//	            the platform recorded as observed
//	the 409     a Run of a NEW job item is the module's 409 whose cause it does not say: ErrConflictUnexplained and never one of the four causes, from ExplainConflict too, while PreCheckRun with the platform's
//	            facts (the entitlement is off) names ErrNotEntitled BEFORE any call; nothing was created at module 3; the replay of an item that already has a job is still the module's 200 (the lookup precedes
//	            the gate); the account routes the suspension gates answer with the route's own sentence (create: unexplained; the test button and bulk add: ErrActionUnavailable); reads still work
//	resume      entitlements.Store.Enable; delivered; Suspension reads version 2, active, reinstated, equal to module 3's own table and to the platform's record; the SAME Run now succeeds and sends once
//
// The paired control of every refusal is the same call before the suspension and after the resume.
func TestM3Adapter_SuspensionRoundTripAndTheUnexplainedConflict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	s := e.seedTenant(t, "susp", []string{"ok", "ok"})
	e.setPacing(t, s, 1, 0, 0)
	acct := e.createVerified(t, s, "1", e.sinkIP)
	_ = acct
	e.enable(t, s, "enable")
	e.waitSync(t, s, 1, false, "active", 60*time.Second)

	// before: nothing suspended, and the adapter says exactly what module 3 holds
	before, err := e.ad.Suspension(ctx, s.UserID)
	if err != nil {
		t.Fatalf("Suspension through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	own := e.suspension(t, s.UserID)
	st0, _ := e.store.Sync(ctx, s.UserID)
	if before.UserID != s.UserID || before.Suspended || before.SuspendedAt != nil || before.ReinstatedAt != nil || before.Version != 0 || own.Exists || own.Suspended || st0.M3Version == nil || *st0.M3Version != 0 {
		t.Errorf("before any suspension the adapter reads %+v, module 3's own table has row %v suspended %v, the platform's record says m3_version %s, want a never-suspended user at version 0 in all three", before, own.Exists, own.Suspended, entObsStr(st0.M3Version))
	}
	t.Logf("NOTE m3a suspension before: adapter {suspended %v version %d suspended_at %v}; module 3's own row exists %v; platform m3_sync %s", before.Suspended, before.Version, before.SuspendedAt, own.Exists, m3aSyncString(st0))

	// the control run: a Run before the suspension is accepted and sends
	first, err := s.C.Run(ctx, s.Items[0].ID)
	if err != nil || !first.Created {
		t.Fatalf("Run of the first item before the suspension answered %+v (error %v), want a new job", first, err)
	}
	s.Items[0].JobID = first.JobID
	one := &m3aTenant{Name: s.Name, UserID: s.UserID, CampaignID: s.CampaignID, Prefix: s.Prefix, Items: s.Items[:1], C: s.C, Accounts: s.Accounts}
	one.Items[0].Kind = "ok"
	e.waitCampaign(t, one, 90*time.Second, nil)

	// suspend: the platform's real path
	dis := e.disable(t, s, "disable")
	stS := e.waitSync(t, s, 2, true, "suspended", 60*time.Second)
	susp, err := e.ad.Suspension(ctx, s.UserID)
	if err != nil {
		t.Fatalf("Suspension through the adapter failed after the suspension (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	ownS := e.suspension(t, s.UserID)
	if susp.UserID != s.UserID || !susp.Suspended || susp.SuspendedAt == nil || susp.ReinstatedAt != nil || susp.Version != 1 {
		t.Errorf("after the suspension the adapter reads %+v, want suspended at version 1 with a suspension instant and no reinstatement", susp)
	}
	if !ownS.Exists || !ownS.Suspended || susp.SuspendedAt == nil || ownS.SuspendedAt == nil || !ownS.SuspendedAt.Truncate(time.Microsecond).Equal(susp.SuspendedAt.UTC().Truncate(time.Microsecond)) {
		t.Errorf("module 3's own table says %+v, the adapter %+v: the instant of the suspension must be the same", ownS, susp)
	}
	if stS.Observed != "suspended" || stS.ObservedVersion == nil || *stS.ObservedVersion != 2 || stS.M3Version == nil || *stS.M3Version != susp.Version {
		t.Errorf("the platform recorded %s, the adapter reads version %d suspended %v: the platform's observation and the adapter's read must be the same module 3 state", m3aSyncString(stS), susp.Version, susp.Suspended)
	}
	t.Logf("NOTE m3a suspension: disabled through the store (ledger row %d); the service delivered it: %s; the adapter reads suspended=%v version=%d at %v; module 3's own table: suspended=%v at %v", dis.LedgerID, m3aSyncString(stS), susp.Suspended, susp.Version, susp.SuspendedAt, ownS.Suspended, ownS.SuspendedAt)

	// the 409: the platform's facts name the cause BEFORE the call; the module's answer does not
	entNow, err := e.store.Get(ctx, s.UserID)
	if err != nil || !entNow.Exists || entNow.Enabled {
		t.Fatalf("Store.Get(S) = %+v (error %v), want a disabled entitlement", entNow, err)
	}
	pre := m3.PreCheckRun(m3.RunFacts{Entitled: entNow.Enabled, RecipientCount: 1, QueueDepthCap: m3.KnownLimit(e.dialInt(t, "max_queued_jobs")), CampaignSizeCap: m3.KnownLimit(m3aCampaignCap)})
	if !errors.Is(pre, m3.ErrNotEntitled) || errors.Is(pre, m3.ErrJobActive) || modclient.KindOf(pre) != modclient.Invalid {
		t.Errorf("PreCheckRun with the platform's facts (entitlement off) answered %v (kind %s), want ErrNotEntitled alone, refused before any call", pre, modclient.KindOf(pre))
	}
	jobsBefore := len(e.jobsOf(t, s.UserID))
	_, runErr := s.C.Run(ctx, s.Items[1].ID)
	var re *modclient.Error
	if !errors.As(runErr, &re) || re.Kind != modclient.Refused || re.Status != 409 || re.Route != "m3.run" || !errors.Is(runErr, sendapi.ErrConflictUnexplained) {
		t.Fatalf("a Run of a new item while suspended answered %v, want the module's 409 (refused, route m3.run) wrapped as ErrConflictUnexplained", runErr)
	}
	explained := m3.ExplainConflict(runErr)
	for _, cause := range []error{m3.ErrNotEntitled, m3.ErrJobActive, m3.ErrOverCampaignCap, m3.ErrOverQueueCap, sendapi.ErrBackpressure} {
		if errors.Is(explained, cause) || errors.Is(runErr, cause) {
			t.Errorf("the 409 is explained as %v: the module does not say which of its four causes this is, so the adapter must never name one", cause)
		}
	}
	if !errors.Is(explained, m3.ErrConflictUnexplained) || !errors.As(explained, &re) || re.Status != 409 || modclient.KindOf(explained) != modclient.Refused {
		t.Errorf("ExplainConflict answered %v, want ErrConflictUnexplained that is still the module's refused 409", explained)
	}
	if jobs := e.jobsOf(t, s.UserID); len(jobs) != jobsBefore || jobs[s.Items[1].ID].ID != "" {
		t.Errorf("module 3 holds %d job(s) of S (%d before) and one for the refused item: a refused Run must create nothing", len(jobs), jobsBefore)
	}
	if replay, err := s.C.Run(ctx, s.Items[0].ID); err != nil || replay.Created || replay.JobID != first.JobID || replay.State != "sent" {
		t.Errorf("a Run of the item that already has a job, while suspended, answered %+v (error %v), want the module's 200 replay of job %s (the lookup precedes the gate)", replay, err, first.JobID)
	}
	t.Logf("NOTE m3a conflict: PreCheckRun names %v before the call; Run of a new item answered kind %s HTTP %d code %q request %s: ExplainConflict keeps it ErrConflictUnexplained (and none of the four causes); module 3 created no job (%d before and after); the replay of the first item is still a 200", pre, re.Kind, re.Status, re.Code, re.RequestID, jobsBefore)

	// the account routes the suspension gates, and the reads it does not
	_, err = s.C.CreateAccount(ctx, e.newAccount("2", s, e.sinkIP))
	if !errors.Is(err, sendapi.ErrConflictUnexplained) || modclient.KindOf(err) != modclient.Refused || m3.MaybeApplied(err) {
		t.Errorf("CreateAccount while suspended answered %v (may have been applied %v), want the module's 409 as ErrConflictUnexplained and nothing created", err, m3.MaybeApplied(err))
	}
	if _, err := s.C.TestAccount(ctx, acct.ID, false); !errors.Is(err, m3.ErrActionUnavailable) {
		t.Errorf("the test button while suspended answered %v, want ErrActionUnavailable (the route's one cause, never named suspended)", err)
	}
	if _, err := s.C.BulkAddAccounts(ctx, []m3.NewAccount{e.newAccount("3", s, e.sinkIP)}); !errors.Is(err, m3.ErrActionUnavailable) {
		t.Errorf("a bulk add while suspended answered %v, want ErrActionUnavailable", err)
	}
	if list, err := s.C.AllAccounts(ctx); err != nil || len(list) != 1 {
		t.Errorf("AllAccounts while suspended answered %d account(s) (error %v), want the one account: a read is not gated and the refused create made none", len(list), err)
	}
	if _, err := s.C.Quote(ctx, 1); err != nil {
		t.Errorf("Quote while suspended failed (%v): the quote never reads the suspension", err)
	}

	// resume: the platform's real path
	en := e.enable(t, s, "re-enable")
	stR := e.waitSync(t, s, 3, false, "active", 60*time.Second)
	res, err := e.ad.Suspension(ctx, s.UserID)
	if err != nil {
		t.Fatalf("Suspension through the adapter failed after the resume (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	ownR := e.suspension(t, s.UserID)
	if res.UserID != s.UserID || res.Suspended || res.Version != 2 || res.ReinstatedAt == nil || res.SuspendedAt == nil || !res.SuspendedAt.Equal(*susp.SuspendedAt) || !res.ReinstatedAt.After(*res.SuspendedAt) {
		t.Errorf("after the resume the adapter reads %+v, want active at version 2 with the earlier suspension instant kept and a later reinstatement", res)
	}
	if !ownR.Exists || ownR.Suspended || ownR.ReinstatedAt == nil || res.ReinstatedAt == nil || !ownR.ReinstatedAt.Truncate(time.Microsecond).Equal(res.ReinstatedAt.UTC().Truncate(time.Microsecond)) {
		t.Errorf("module 3's own table says %+v after the resume, the adapter %+v", ownR, res)
	}
	if stR.Observed != "active" || stR.ObservedVersion == nil || *stR.ObservedVersion != 3 || stR.M3Version == nil || *stR.M3Version != res.Version {
		t.Errorf("the platform recorded %s after the resume, the adapter reads version %d", m3aSyncString(stR), res.Version)
	}
	t.Logf("NOTE m3a resume: re-enabled through the store (ledger row %d); delivered: %s; the adapter reads suspended=%v version=%d reinstated %v; module 3's own table: suspended=%v", en.LedgerID, m3aSyncString(stR), res.Suspended, res.Version, res.ReinstatedAt, ownR.Suspended)

	// the SAME run, now accepted, sends once
	again, err := s.C.Run(ctx, s.Items[1].ID)
	if err != nil || !again.Created || again.State != "queued" {
		t.Fatalf("the Run that was refused while suspended answered %+v (error %v) after the resume, want a new job (202, queued)", again, err)
	}
	s.Items[1].JobID = again.JobID
	for _, it := range s.Items {
		it.Kind = "ok"
	}
	rep := e.waitCampaign(t, s, 90*time.Second, nil)
	m3aProblems(t, "the report after the resume", m3aCompareReport(s, rep.Items))
	view := e.sinkView(t)
	m3aProblems(t, "the sink after the resume", m3aCompareSink(s, rep.Items, view, 0))
	if _, err := s.C.CreateAccount(ctx, e.newAccount("2", s, e.sinkIP)); err != nil {
		t.Errorf("control: CreateAccount after the resume failed (%v), want it accepted", err)
	}
	cl, err := s.C.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	m3aProblems(t, "Classify after the resume", m3aCompareClassified(rep.Items, cl, m3aDecide))
	b := e.books(t, s.UserID)
	m3aProblems(t, "the books of S", m3aCompareBooks(b))
	if b.Ledger != 3 || b.Entitlement != "enabled" {
		t.Errorf("S's ledger holds %d row(s) with the entitlement %s, want its three entitlement rows (enable, disable, enable) and nothing else", b.Ledger, b.Entitlement)
	}
	e.requireBooks(t, "after the suspension round trip")
	t.Logf("NOTE m3a resume run: the refused Run is accepted (202) after the resume; both of S's items are sent exactly once (the sink holds %d message(s) to S); S's ledger holds %d entitlement rows and nothing else", view.of(s), b.Ledger)
}

// TestM3Adapter_PaginationAcrossPageBoundaries (B10): the campaign report of 45 sent items read through the adapter at seven page sizes. The sizes put a page boundary at every item (1), at uneven points (7: seven
// pages, the last of 3), exactly at the end of a full last page (15: three pages, no empty page after the last), near the middle (23: 23 + 22), one item short of the end (44: a last page of one), exactly at the
// end (45: one page that is the whole campaign) and beyond it (100). For each size the walk must deliver ceil(45/size) pages of that size (the last the remainder), each naming the size the module says it used and a next
// cursor on every page but the last, and the concatenation must be EXACTLY the baseline read at the adapter's own page size of 200, every (job item id, state, error class, provider message id) once, in the order the
// items were run (sendapitest.ExactSet). The single-job reports of the same items are compared with it too.
//
// THE CONTROLS. Two walkers with the look-ahead cursor bug, written over ReportCampaignPage, run against the real module at page size 7, and the SAME equality check must report every item they lose; and the check must
// report a duplicated item and a reordered one.
func TestM3Adapter_PaginationAcrossPageBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	const n = 45
	kinds := make([]string, n)
	for i := range kinds {
		kinds[i] = "ok"
	}
	p := e.seedTenant(t, "page", kinds)
	e.setPacing(t, p, 3, 0, 0)
	e.createVerified(t, p, "1", e.sinkIP)
	took := e.runAll(t, p)
	t.Logf("NOTE m3a run: %d new jobs through the adapter in %s", n, took.Round(time.Millisecond))
	rep := e.waitCampaign(t, p, 240*time.Second, nil)
	m3aProblems(t, "the baseline report (page size 200)", m3aCompareReport(p, rep.Items))
	base := m3aKeys(rep.Items)
	t.Logf("NOTE m3a baseline: %d item(s) in %d page(s) at the adapter's page size; %s", len(rep.Items), rep.Pages, m3aTotalsLine(rep.Totals))

	var singles []string
	for _, it := range p.Items {
		one, err := p.C.ReportJob(ctx, it.ID)
		if err != nil {
			t.Fatalf("ReportJob of item %02d through the adapter failed (%s): %s", it.Index, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		if one.JobID != it.JobID {
			t.Errorf("the single-job report of item %02d names job %s, Run answered %s", it.Index, one.JobID, it.JobID)
		}
		singles = append(singles, m3aRowKey(one))
	}
	m3aProblems(t, "the single-job reports against the campaign report", sendapitest.ExactSet(singles, base))
	t.Logf("NOTE m3a singles: %d single-job reports equal the campaign report's (id, state, error class, provider message id) rows, in order", len(singles))

	pager := func(size int) *sendapi.Client {
		c, err := sendapi.New(e.mc, m3.Profile(), p.UserID, sendapi.Options{PageSize: size})
		if err != nil {
			t.Fatalf("sendapi.New(page size %d): %s", size, e.raw.scrub(err.Error()))
		}
		return c
	}
	for _, ps := range []int{1, 7, 15, 23, 44, 45, 100} {
		var pages []sendapi.CampaignPage
		st, err := pager(ps).Walk(ctx, p.CampaignID, func(pg sendapi.CampaignPage) error { pages = append(pages, pg); return nil })
		if err != nil {
			t.Fatalf("the walk at page size %d failed (%s): %s", ps, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		wantPages := (n + ps - 1) / ps
		var got, shape, boundaries []string
		seen := 0
		for i, pg := range pages {
			last := i == len(pages)-1
			wantLen := ps
			if last {
				wantLen = n - ps*(wantPages-1)
			}
			if pg.PageSize != ps {
				t.Errorf("page size %d: page %d says the module used page size %d", ps, i+1, pg.PageSize)
			}
			if len(pg.Items) != wantLen {
				t.Errorf("page size %d: page %d holds %d item(s), want %d", ps, i+1, len(pg.Items), wantLen)
			}
			if last != (pg.NextCursor == "") {
				t.Errorf("page size %d: page %d of %d has next cursor %q: only the last page may have none", ps, i+1, len(pages), pg.NextCursor)
			}
			if pg.Totals.Total != n {
				t.Errorf("page size %d: page %d reports total %d, want %d", ps, i+1, pg.Totals.Total, n)
			}
			seen += len(pg.Items)
			if !last && len(boundaries) < 6 {
				boundaries = append(boundaries, fmt.Sprint(seen))
			}
			shape = append(shape, fmt.Sprint(len(pg.Items)))
			got = append(got, m3aKeys(pg.Items)...)
		}
		if len(pages) != wantPages || st.Pages != wantPages || st.Items != n {
			t.Errorf("page size %d: the walk read %d page(s) and %d item(s) (stats %+v), want %d page(s) and %d item(s)", ps, len(pages), seen, st, wantPages, n)
		}
		m3aProblems(t, fmt.Sprintf("page size %d against the baseline", ps), sendapitest.ExactSet(got, base))
		if len(shape) > 12 {
			shape = append(shape[:6], append([]string{"..."}, shape[len(shape)-2:]...)...)
		}
		b := strings.Join(boundaries, " ")
		if wantPages > 7 {
			b += " ..."
		}
		t.Logf("NOTE m3a page size %3d: %2d page(s) of [%s], boundaries after item(s) [%s]; (id, state, error class, provider message id) of all %d items equal the baseline, in order", ps, len(pages), strings.Join(shape, " "), b, len(got))
	}

	pg7 := pager(7)
	nonFinal := (n+6)/7 - 1 // pages that have a successor at page size 7
	look, err := m3aLookAheadWalk(ctx, pg7, p.CampaignID)
	if err != nil {
		t.Fatalf("the look-ahead walker (control) failed to read: %s", e.raw.scrub(err.Error()))
	}
	lp := sendapitest.ExactSet(m3aKeys(look), base)
	if missing := m3aCountPrefix(lp, "missing "); missing != n-len(look) || missing != nonFinal || missing == 0 || m3aCountPrefix(lp, "unexpected ") != 0 {
		t.Errorf("control: the equality check reported %d missing item(s) for the look-ahead walker that dropped %d (one per page that has a successor: %d): %v", missing, n-len(look), nonFinal, lp)
	}
	skip, err := m3aSkipAheadWalk(ctx, pg7, p.CampaignID)
	if err != nil {
		t.Fatalf("the skip-ahead walker (control) failed to read: %s", e.raw.scrub(err.Error()))
	}
	sp := sendapitest.ExactSet(m3aKeys(skip), base)
	if missing := m3aCountPrefix(sp, "missing "); missing != n-len(skip) || missing == 0 || m3aCountPrefix(sp, "unexpected ") != 0 {
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
	t.Logf("NOTE m3a controls: at page size 7 the look-ahead walker delivered %d of %d items and the skip-ahead walker %d: the same equality check reported %d and %d missing; a duplicate and a reordered pair are reported too", len(look), n, len(skip), n-len(look), n-len(skip))
	if view := e.sinkView(t); view.of(p) != n || len(view.Dups) != 0 {
		t.Errorf("the sink holds %d message(s) to the tenant's recipients (duplicates %v), want exactly %d", view.of(p), view.Dups, n)
	}
}

// TestM3Adapter_AccountListPaginationAcrossABoundary (B10): the adapter lists accounts at a FIXED page size of 200 (m3.PageLimit, the module's maximum), and module 3 allows 20 accounts per user, so one page is
// all a user can ever have until the operator's dial account_count_cap_per_user is raised. This test raises it (module 3's own dial, restored at the end), adds 203 accounts through the adapter, and so makes the list
// two pages: 200 and 3. The walk page by page, and AllAccounts, must give EXACTLY the accounts created, each once, in the order they were created (oldest first); the first page names a next cursor, the last none.
// THE CONTROL: a walker with the look-ahead cursor bug over ListAccounts drops the last account of the first page, and the SAME equality check reports it; and a duplicate and a reorder.
func TestM3Adapter_AccountListPaginationAcrossABoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	e.setDial(t, "account_count_cap_per_user", "300")

	const total = 203
	l := e.seedTenant(t, "acct", nil)
	var mu sync.Mutex
	var created []m3.Account
	var failures []string
	start := time.Now()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ { // three at a time: module 3 has three workers to verify with
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				a, err := l.C.CreateAccount(ctx, e.newAccount(fmt.Sprintf("%03d", i), l, e.sinkIP))
				mu.Lock()
				if err != nil {
					failures = append(failures, fmt.Sprintf("account %d: %s (%s, may have been applied %v)", i, e.raw.scrub(err.Error()), modclient.KindOf(err), m3.MaybeApplied(err)))
				} else {
					created = append(created, a)
				}
				mu.Unlock()
			}
		}()
	}
	for i := 1; i <= total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("%d of %d CreateAccount call(s) failed (the first: %s)", len(failures), total, failures[0])
	}
	t.Logf("NOTE m3a accounts: %d accounts created through the adapter in %s (the dial account_count_cap_per_user raised to 300 for this test)", len(created), time.Since(start).Round(time.Millisecond))
	want := make([]string, len(created))
	for i, a := range created {
		want[i] = a.ID
	}

	all, err := l.C.AllAccounts(ctx)
	if err != nil {
		t.Fatalf("AllAccounts failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	baseIDs := make([]string, len(all))
	for i, a := range all {
		baseIDs[i] = a.ID
	}
	m3aProblems(t, "AllAccounts against the accounts created (as sets)", sendapitest.ExactSet(m3aSorted(baseIDs), m3aSorted(want)))
	if len(all) != total {
		t.Fatalf("AllAccounts returned %d account(s), want %d", len(all), total)
	}

	var walked []string
	var shape []string
	cursor, pages := "", 0
	for {
		pg, err := l.C.ListAccounts(ctx, cursor)
		if err != nil {
			t.Fatalf("ListAccounts (page %d) failed (%s): %s", pages+1, modclient.KindOf(err), e.raw.scrub(err.Error()))
		}
		pages++
		shape = append(shape, fmt.Sprint(len(pg.Items)))
		for _, a := range pg.Items {
			walked = append(walked, a.ID)
		}
		if pg.NextCursor == "" {
			break
		}
		cursor = pg.NextCursor
		if pages > 5 {
			t.Fatalf("the list did not end within 5 pages")
		}
	}
	if pages != 2 || shape[0] != "200" || shape[1] != fmt.Sprint(total-200) {
		t.Errorf("the list has %d page(s) of [%s], want two: 200 and %d", pages, strings.Join(shape, " "), total-200)
	}
	m3aProblems(t, "the page-by-page walk against AllAccounts", sendapitest.ExactSet(walked, baseIDs))
	byCreation := sort.SliceIsSorted(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })
	if !byCreation {
		t.Errorf("the accounts are not listed oldest first")
	}

	look, err := m3aAccountLookAheadWalk(ctx, l.C)
	if err != nil {
		t.Fatalf("the look-ahead walker (control) failed: %s", e.raw.scrub(err.Error()))
	}
	lookIDs := make([]string, len(look))
	for i, a := range look {
		lookIDs[i] = a.ID
	}
	lp := sendapitest.ExactSet(lookIDs, baseIDs)
	if missing := m3aCountPrefix(lp, "missing "); missing != total-len(look) || missing != 1 || m3aCountPrefix(lp, "unexpected ") != 0 {
		t.Errorf("control: the equality check reported %d missing account(s) for the look-ahead walker that dropped %d (the last of the one page that has a successor): %v", missing, total-len(look), lp)
	}
	dup := sendapitest.ExactSet(append(append([]string(nil), baseIDs...), baseIDs[0]), baseIDs)
	swapped := append([]string(nil), baseIDs...)
	swapped[3], swapped[4] = swapped[4], swapped[3]
	if len(dup) == 0 || !strings.Contains(strings.Join(dup, "\n"), "appears 2 times") || len(sendapitest.ExactSet(swapped, baseIDs)) == 0 {
		t.Errorf("control: the equality check did not report a duplicated account (%v) or two accounts out of order", dup)
	}
	t.Logf("NOTE m3a account list: %d accounts in %d page(s) of [%s]; the walk and AllAccounts equal the %d created, oldest first; the look-ahead walker delivered %d and the same check reported %d missing; a duplicate and a reordered pair are reported too", len(all), pages, strings.Join(shape, " "), total, len(look), total-len(look))
}

// TestM3Adapter_CrossTenantReadsAreRefused (B11): tenant A runs a campaign of four (all sent); tenant B, through an adapter bound to B, asks module 3 for it. What module 3's code answers (research m3-api 3.4,
// read at platformapi/campaign_report.go, store.go, account_store.go and account_events.go) and what is asserted:
//
//	campaign report of A's campaign   200 with total 0, no items, is_complete false (a foreign or unknown campaign is NOT a 404): NothingKnown
//	single-job report by item id      404 not found, naming the field job_item_id (the same answer as for a typo)
//	single-job report by job id       404 not found, naming the field job_id
//	Run of A's job item               404 not found naming job_item_id; nothing is created: A's own replay of the item is the 200 of the job it already had
//	GetAccount of A's account         404 not found (no field)
//	test button of A's account        404 not found: nothing is verified, A's account is not touched
//	events of A's account             200 with an empty page (the module does no existence check there)
//	ListAccounts                      B's OWN accounts only (200): none of A's
//
// and that no id, address, host or username of A is in any of B's answers (m3aLeaks over every answer's text; B's account names a different SMTP host, the module's own Mailpit sink, so the host scan can fail).
// THE CONTROLS: the same calls through A's adapter return A's rows and the leak scan finds A's ids in A's own answers (so it can fail); B runs a campaign of its own, which works, and A's adapter gets the same
// refusals for B's; the Domain A service token is REFUSED on module 3's admin routes (the suspension read, through raw HTTP and through the platform's admin client and the adapter's reader) and module 3's admin
// key is refused on a Domain A route, so a tenant cannot read or change a suspension through the user-facing credential.
func TestM3Adapter_CrossTenantReadsAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)

	a := e.seedTenant(t, "cta", []string{"ok", "ok", "ok", "ok"})
	b := e.seedTenant(t, "ctb", []string{"ok", "ok"})
	for _, tn := range []*m3aTenant{a, b} {
		e.setPacing(t, tn, 3, 0, 0)
	}
	e.createVerified(t, a, "1", e.sinkIP)    // A's account names the controllable sink's private address
	e.createVerified(t, b, "1", "smtp-sink") // B's names module 3's own Mailpit sink: a different host, so a scan for A's host in B's answers means something
	e.runAll(t, a)
	repA := e.waitCampaign(t, a, 120*time.Second, nil)
	m3aProblems(t, "A's own campaign", m3aCompareReport(a, repA.Items))
	secrets := m3aSecrets(a, e.sinkIP)
	jobsA := e.jobsOf(t, a.UserID)
	var asked []string
	say := func(what, text string) {
		asked = append(asked, what)
		if leaked := m3aLeaks(text, secrets); len(leaked) != 0 {
			t.Errorf("B's answer to %s carries tenant A's %v: %s", what, leaked, text)
		}
	}

	// B -> A: the campaign report
	repB, err := b.C.ReportCampaign(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("B's ReportCampaign of A's campaign failed (%s), want the module's empty 200: %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	if !repB.NothingKnown() || repB.Totals != (sendapi.Totals{}) || len(repB.Items) != 0 || repB.Progress.IsComplete || repB.Pages != 1 || len(repB.ByAccount) != 0 {
		t.Errorf("B's report of A's campaign is %+v, want an empty 200: total 0, no item, not complete, no account row, one page", repB)
	}
	say("the campaign report", fmt.Sprintf("%+v", repB))

	// B -> A: single-job reports and a run
	for _, it := range a.Items {
		_, err := b.C.ReportJob(ctx, it.ID)
		if why := m3aNotFound(err, "job_item_id"); why != "" {
			t.Errorf("B's ReportJob of A's item %02d: %s (%v)", it.Index, why, err)
		}
		say(fmt.Sprintf("the single-job report of item %02d", it.Index), fmt.Sprint(err))
	}
	_, err = b.C.ReportJobByID(ctx, a.Items[0].JobID)
	if why := m3aNotFound(err, "job_id"); why != "" {
		t.Errorf("B's ReportJobByID of A's job: %s (%v)", why, err)
	}
	say("the single-job report by job id", fmt.Sprint(err))
	_, err = b.C.Run(ctx, a.Items[0].ID)
	if why := m3aNotFound(err, "job_item_id"); why != "" {
		t.Errorf("B's Run of A's job item: %s (%v)", why, err)
	}
	say("run", fmt.Sprint(err))
	if jobs := e.jobsOf(t, b.UserID); len(jobs) != 0 {
		t.Errorf("module 3 holds %d job(s) of B after its Run of A's item, want none", len(jobs))
	}

	// B -> A: accounts
	_, err = b.C.GetAccount(ctx, a.Accounts[0].ID)
	if why := m3aNotFound(err, ""); why != "" {
		t.Errorf("B's GetAccount of A's account: %s (%v)", why, err)
	}
	say("the account", fmt.Sprint(err))
	_, err = b.C.TestAccount(ctx, a.Accounts[0].ID, false)
	if why := m3aNotFound(err, ""); why != "" {
		t.Errorf("B's TestAccount of A's account: %s (%v)", why, err)
	}
	say("the test button", fmt.Sprint(err))
	evB, err := b.C.AccountEvents(ctx, a.Accounts[0].ID, "")
	if err != nil || len(evB.Items) != 0 || evB.NextCursor != "" {
		t.Errorf("B's AccountEvents of A's account answered %d event(s) (error %v), want the documented empty page", len(evB.Items), err)
	}
	say("the account's events", fmt.Sprintf("%+v", evB))
	listB, err := b.C.AllAccounts(ctx)
	if err != nil || len(listB) != 1 || listB[0].ID != b.Accounts[0].ID {
		t.Fatalf("B's AllAccounts answered %d account(s) (error %v), want B's own account only", len(listB), err)
	}
	say("the account list", fmt.Sprintf("%+v", listB))
	if one, err := e.ad.Suspension(ctx, b.UserID); err != nil || one.UserID != b.UserID {
		t.Errorf("control: the adapter reads B's own suspension as %+v (error %v)", one, err)
	}

	// the controls: A's own adapter gets A's rows, and the leak scan can see them
	repA2, err := a.C.ReportCampaign(ctx, a.CampaignID)
	if err != nil {
		t.Fatalf("A's ReportCampaign failed: %s", e.raw.scrub(err.Error()))
	}
	m3aProblems(t, "control: A's own campaign report", sendapitest.ExactSet(m3aItemIDs(repA2.Items), a.ids()))
	if repA2.Totals.Total != int64(len(a.Items)) || repA2.NothingKnown() {
		t.Errorf("control: A's own report says %s, want total %d", m3aTotalsLine(repA2.Totals), len(a.Items))
	}
	for _, it := range a.Items {
		one, err := a.C.ReportJob(ctx, it.ID)
		if err != nil || one.JobID != it.JobID {
			t.Errorf("control: A's ReportJob of item %02d answered job %q (error %v), want job %s", it.Index, one.JobID, err, it.JobID)
		}
	}
	if one, err := a.C.ReportJobByID(ctx, a.Items[0].JobID); err != nil || one.JobID != a.Items[0].JobID {
		t.Errorf("control: A's ReportJobByID answered job %q (error %v), want %s", one.JobID, err, a.Items[0].JobID)
	}
	if replay, err := a.C.Run(ctx, a.Items[0].ID); err != nil || replay.Created || replay.JobID != a.Items[0].JobID {
		t.Errorf("control: A's own replay of item 01 answered %+v (error %v), want the job it already had (B's attempt created nothing)", replay, err)
	}
	if jobs := e.jobsOf(t, a.UserID); len(jobs) != len(jobsA) {
		t.Errorf("module 3 holds %d job(s) of A after B's attempts, %d before", len(jobs), len(jobsA))
	}
	if got, err := a.C.GetAccount(ctx, a.Accounts[0].ID); err != nil || got.ID != a.Accounts[0].ID || got.SMTPHost != e.sinkIP {
		t.Errorf("control: A's own GetAccount answered %+v (error %v), want its account", got, err)
	}
	if tr, err := a.C.TestAccount(ctx, a.Accounts[0].ID, false); err != nil || tr.Verification.State != m3.VerificationVerified {
		t.Errorf("control: A's own test button answered %+v (error %v), want verified (and B's refused click did not consume or change it)", tr, err)
	}
	if evA, err := a.C.AccountEvents(ctx, a.Accounts[0].ID, ""); err != nil || len(evA.Items) == 0 {
		t.Errorf("control: A's own events answered %d event(s) (error %v), want its audit trail", len(evA.Items), err)
	}
	if lA, err := a.C.AllAccounts(ctx); err != nil || len(lA) != 1 || lA[0].ID != a.Accounts[0].ID {
		t.Errorf("control: A's own list answered %d account(s) (error %v), want its one account", len(lA), err)
	}
	ownText := fmt.Sprintf("%+v %+v", repA2, a.Accounts[0])
	if leaked := m3aLeaks(ownText, secrets); len(leaked) < len(a.Items) {
		t.Errorf("control: the leak scan found only %v in A's own answers, want at least A's four item ids: it cannot fail", leaked)
	}
	t.Logf("NOTE m3a cross-tenant (B asks for A): campaign report total=%d items=%d complete=%v pages=%d; ReportJob of A's %d items -> not found (job_item_id); ReportJobByID -> not found (job_id); Run -> not found (job_item_id), nothing created; GetAccount and the test button -> not found; events -> empty page; B's list holds only B's account; %d answers scanned for A's %d ids, addresses, host and usernames: none", repB.Totals.Total, len(repB.Items), repB.Progress.IsComplete, repB.Pages, len(a.Items), len(asked), len(secrets))
	t.Logf("NOTE m3a cross-tenant control (A asks for A): %d item(s) of %s; ReportJob, ReportJobByID, GetAccount, the test button, the events and the list answer A's rows; the replay is the job A already had; the leak scan finds %d of A's ids in A's own answers", len(repA2.Items), m3aTotalsLine(repA2.Totals), len(m3aLeaks(ownText, secrets)))

	// B runs a campaign of its own through its adapter (it works for B), and A's adapter is refused B's rows the same way
	e.runAll(t, b)
	repBOwn := e.waitCampaign(t, b, 120*time.Second, nil)
	m3aProblems(t, "B's own campaign through B's adapter", m3aCompareReport(b, repBOwn.Items))
	mp := e.mailpitCounts(t, b)
	for _, it := range b.Items {
		if mp[it.Email] != 1 {
			t.Errorf("module 3's Mailpit sink holds %d message(s) to B's recipient %s, want exactly one", mp[it.Email], it.Email)
		}
	}
	repAB, err := a.C.ReportCampaign(ctx, b.CampaignID)
	if err != nil || !repAB.NothingKnown() {
		t.Errorf("A's report of B's campaign is %+v (error %v), want the empty 200", repAB, err)
	}
	if _, err := a.C.ReportJob(ctx, b.Items[0].ID); m3aNotFound(err, "job_item_id") != "" {
		t.Errorf("A's ReportJob of B's item: %s (%v)", m3aNotFound(err, "job_item_id"), err)
	}
	if _, err := a.C.GetAccount(ctx, b.Accounts[0].ID); m3aNotFound(err, "") != "" {
		t.Errorf("A's GetAccount of B's account: %s (%v)", m3aNotFound(err, ""), err)
	}
	if after, err := b.C.ReportCampaign(ctx, b.CampaignID); err != nil || after.Totals != repBOwn.Totals {
		t.Errorf("B's campaign changed after A's attempts: %+v (error %v), was %+v", after.Totals, err, repBOwn.Totals)
	}
	t.Logf("NOTE m3a cross-tenant, the other way: B's own campaign of %d item(s) finished through B's adapter (%s) and Mailpit holds each message once; A's adapter got the empty 200 for B's campaign and not found for its item and its account", len(b.Items), m3aTotalsLine(repBOwn.Totals))

	// the credentials: the service token is refused on the admin surface, the admin key on the user-facing surface
	status, body := e.raw.do(t, e.raw.m3URL, "GET", "/v1/admin/users/"+a.UserID+"/suspension", e.raw.token, nil)
	e.raw.expect(t, "the Domain A SERVICE token on the admin route of A's suspension", status, body, 401, "unauthenticated")
	status, body = e.raw.do(t, e.raw.m3URL, "POST", "/v1/admin/users/"+a.UserID+"/suspend", e.raw.token, map[string]string{"reason": "a tenant must not be able to do this"})
	e.raw.expect(t, "the Domain A SERVICE token on the admin suspend route", status, body, 401, "unauthenticated")
	if st := e.suspension(t, a.UserID); st.Suspended {
		t.Fatalf("module 3 holds A as suspended after a call that carried only the service token: %+v", st)
	}
	wrong, err := m3admin.New(e.raw.m3URL, e.raw.token, nil) // the platform's real admin client, handed the user-facing credential
	if err != nil {
		t.Fatalf("m3admin.New: %v", err)
	}
	if _, err := wrong.Suspension(ctx, a.UserID); !errors.Is(err, m3admin.ErrUnauthorized) {
		t.Errorf("the admin client with the service token read A's suspension: %v, want m3admin.ErrUnauthorized", err)
	}
	if _, err := wrong.SetSuspended(ctx, a.UserID, true, "a tenant must not be able to do this"); !errors.Is(err, m3admin.ErrUnauthorized) {
		t.Errorf("the admin client with the service token suspended A: %v, want m3admin.ErrUnauthorized", err)
	}
	viaWrong := e.adapter(t, e.cfg, wrong, true)
	if s, err := viaWrong.Suspension(ctx, a.UserID); modclient.KindOf(err) != modclient.Refused || s != (m3.Suspension{}) {
		t.Errorf("the adapter's Suspension through a reader that holds only the service token answered %+v (error %v, kind %s), want a refused error and no value", s, err, modclient.KindOf(err))
	}
	if s, err := e.ad.Suspension(ctx, a.UserID); err != nil || s.Suspended || s.UserID != a.UserID {
		t.Errorf("control: the adapter's Suspension through the real admin reader answered %+v (error %v), want A's state: not suspended", s, err)
	}
	cfgSwap := e.cfg
	cfgSwap.M3ServiceToken, cfgSwap.M3AdminToken = e.raw.adminKey, "" // a deployment that handed the user-facing client the ADMIN key
	swapped, err := m3.FromConfig(cfgSwap)
	if err != nil {
		t.Fatalf("m3.FromConfig with the admin key as the service token: %v", err)
	}
	if sc, err := swapped.For(a.UserID); err != nil {
		t.Fatalf("For: %v", err)
	} else if _, err := sc.Quote(ctx, 1); modclient.KindOf(err) != modclient.Refused {
		t.Errorf("a Domain A call carrying module 3's ADMIN key answered %v (kind %s), want refused: the two domains never share a credential", err, modclient.KindOf(err))
	}
	if _, err := m3.FromConfig(config.Config{M3URL: e.raw.m3URL, M3ServiceToken: e.raw.adminKey, M3AdminToken: e.raw.adminKey}); !errors.Is(err, m3.ErrSharedCredential) {
		t.Errorf("m3.FromConfig with the service token equal to the admin key answered %v, want ErrSharedCredential", err)
	}
	t.Logf("NOTE m3a credentials: the Domain A service token on the admin suspension read and on the suspend route -> 401 unauthenticated (A stays unsuspended); the platform's admin client and the adapter's suspension reader holding only that token are refused (no value); a Domain A call carrying the admin key is refused; FromConfig refuses a shared credential; the adapter's own reader (the admin key) reads A: not suspended. That the adapter never SENDS the admin key on a user-facing call is a unit property (internal/adapters/m3)")
}
