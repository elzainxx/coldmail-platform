//go:build harness && m2worker

package harness

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 3, chunk B12 for module 2: THE OUTAGE PROOF. Module 2's platform API container (`m2`) is stopped in the middle of a campaign and started again, and the platform's adapter, its money core and the sink
// show that nothing was invented while it was down and nothing was lost or done twice when it came back. Three tests, in the order they run, each a separate `go test` invocation of the stage script
// (tests/harness/m2worker/stage.sh, m2w_outage) with the stop and the start of `m2` between them; the state between them is the row each phase leaves in harness_outage.m2 (helpers: m2_worker_outage_helpers_test.go).
//
//	TestM2Worker_OutagePhase1Start   a real tenant (auth.users, tenant scope, the adapter), 1000 credits, a reserve for 11 units; 10 items handed to module 2 through the adapter and an 11th (the LATE item) committed
//	                                 in public.job_items but not handed over; the sink holds every accepted message 5 s so that the campaign takes about 40 s; the test polls the adapter's campaign report until
//	                                 an item has been SENT, asserts the campaign is not complete and at least 4 items are not terminal, SETTLES EXACTLY WHAT IS FINAL (and records which items), and records the books,
//	                                 module 2's own job rows and the sink's Message-IDs.
//	(the stage)                      stops `m2` and waits until the container is gone AND a request from the private network gets no HTTP answer; the worker, the sink and Postgres keep running.
//	TestM2Worker_OutagePhase2Down    with module 2 gone: every call the platform makes (Quote, Run of the late item = a write, ReportJob, ReportJobByID, ReportCampaignPage, ReportCampaign, Walk, Cancel = a write,
//	                                 Describe) fails typed as unavailable or timeout with the zero value and a request that never left; a bounded polling loop with the production dials stops by its context; the
//	                                 platform's poll-and-settle step run three times against the dead module changes nothing; the books, public.job_items and module 2's job rows are EXACTLY what phase 1 recorded
//	                                 (the failed write created nothing), the sink holds no second copy of anything, balance_drift and reserve_overdrawn are empty, and the platform's /readyz handler answers 200.
//	(the stage)                      starts `m2`, waits for its readiness over the private network (bounded; the stage dies with the module's logs if it never comes back) and for the worker's heartbeat, starting the
//	                                 worker again if its own 30-attempt reconnect budget ran out and it exited (module 2's design: exit for the orchestrator to restart).
//	TestM2Worker_OutagePhase3Resume  polls with the adapter until the module answers (a typed unavailable or timeout in the first seconds is the module coming back and is retried within a bounded budget; any other
//	                                 error, or an answer that disagrees with phase 1, is a wrong answer and fails the test), hands over the late item (now created: 202), lifts the sink's hold so the rest is quick, polls to
//	                                 completion, and asserts: every item is reported exactly once with the state of its kind and the job id it had before the outage; the sink holds exactly one message per sent item and
//	                                 not one of the messages it held during the outage was replaced; the ledger holds exactly one charge per charged item (the ones phase 1 settled are NOT charged again: AlreadySettled);
//	                                 the books close (balance_drift, reserve_overdrawn empty, the reserve released); a second settle writes nothing; a second Run of every item is the module's 200 replay and sends nothing.
//
// THE CONTROL IS THE SAME FLOW WITH THE MODULE UP: TestM2Worker_AdapterEndToEndSettlesItemByItem (m2_worker_e2e_test.go) runs quote, reserve, run, poll, classify, settle and replay through the same adapter against the
// same worker and the same oracle (m2eCompare, m2eHand) without an outage; the outage phases reuse its comparison. Inside these tests the paired controls are m2oControlPhase1/2/3: each assertion function is run
// against fakes that break exactly one property (a listing that returns zero values with a nil error, a value alongside an error, an error of the wrong kind, a fake that settles during the outage, a job created by the
// failed write, an item that regressed, a message sent twice) and the test fails unless each is reported for the reason it names. They run in every invocation, with or without the stack.
//
// WITHOUT HARNESS_M2_OUTAGE=1 (an ordinary run of the whole harness) each test runs its controls and logs `NOTE outage phaseN control-only`; the sub-stage requires the `NOTE outage phaseN live` line besides the PASS line.
// Evidence lines start with `NOTE outage`.

// TestM2Worker_OutagePhase1Start is phase 1: a job in flight.
func TestM2Worker_OutagePhase1Start(t *testing.T) {
	m2oControls(t, 1)
	if !m2oLive(t, 1) {
		return
	}
	r := m2wNewRun(t)
	r.requireWorker(t)
	r.ctlCall(t, http.MethodGet, "/readyz", nil, http.StatusOK)
	prior := r.readDials(t)
	// Unlike prepare, this test leaves its scene standing for the stop: the campaign keeps running, the sink keeps holding and the dials stay set until phase 3 restores them. Only a RED phase 1 undoes it.
	t.Cleanup(func() {
		if t.Failed() {
			r.cancelAll()
			r.restoreSink()
			r.restoreDials(t, prior)
		}
	})
	r.putDials(t)
	r.setMode(t, m2wMode{})
	m2oWaitSinkIdle(t, r)

	tn := r.newTenant(t, "outage", m2oKinds, true)
	n := len(tn.Items)
	if n != m2oRunFirst+1 {
		t.Fatalf("the scenario has %d item(s), want %d handed to the module and one late item", n, m2oRunFirst)
	}
	late := tn.Items[m2oRunFirst]
	e := m2oEnv(t, r, 20*time.Second, 2, 250*time.Millisecond)
	ad := e.client(t, tn)
	r.setMode(t, m2wMode{RejectRcpt: tn.emails("bounce"), RejectCode: 550, DeferRcpt: tn.emails("defer"), DataDelayMs: m2oHoldMs})
	job, rsv := e.fundAndReserve(t, tn, n)
	frozen := e.prices(t, job)
	t.Logf("NOTE outage phase1 setup: tenant %s, campaign %s, platform job %s; %d items of which %d go to module 2 now and item %02d (%s) is the late one; reserve %d credits for %d units at %d; the sink holds every accepted message %d ms; the dials are %s",
		tn.UserID, tn.CampaignID, job, n, m2oRunFirst, late.Index, late.Kind, rsv.Reserved, n, rsv.UnitCost, m2oHoldMs, m2wDialText())

	runStart := time.Now()
	m2oRunSome(t, e, ad, tn.Items[:m2oRunFirst])
	t.Logf("NOTE outage phase1 run: %d new jobs (202 queued, class pending) in %s", m2oRunFirst, time.Since(runStart).Round(time.Millisecond))

	// ---- the job is in flight: progress has started and the campaign is far from done ----
	var rep sendapi.CampaignReport
	last := ""
	for {
		var err error
		rep, err = ad.ReportCampaign(r.ctx, tn.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign through the adapter failed while module 2 was still up (%s): %s", modclient.KindOf(err), r.scrub(err.Error()))
		}
		if line := m2eTotalsLine(rep.Totals); line != last {
			last = line
			t.Logf("NOTE outage phase1 t+%5.1fs: the adapter's campaign report: %s, is_complete %v", time.Since(runStart).Seconds(), line, rep.Progress.IsComplete)
		}
		if rep.Totals.Sent >= 1 {
			break
		}
		if time.Since(runStart) > 90*time.Second {
			t.Fatalf("no item was sent within %s although the worker is up and the sink holds a message %d ms: %s\n%s", time.Since(runStart).Round(time.Second), m2oHoldMs, m2eTotalsLine(rep.Totals), r.diagnostics(t, tn))
		}
		time.Sleep(500 * time.Millisecond)
	}
	sampleAt := time.Now()
	if p := m2oInFlightProblems(rep, m2oRunFirst, m2oMinLeft); len(p) > 0 {
		t.Fatalf("the job is not certainly in flight at the sample (%s): %s\n%s", m2eTotalsLine(rep.Totals), strings.Join(p, "; "), r.diagnostics(t, tn))
	}
	terminal := map[string]m2oTerm{}
	var kindProblems []string
	for _, it := range rep.Items {
		if it.Class != sendapi.ClassTerminal {
			continue
		}
		terminal[it.JobItemID] = m2oTerm{State: it.State, Reason: m2eReason(it.Reason)}
		x := tn.byID[it.JobItemID]
		if x == nil {
			kindProblems = append(kindProblems, fmt.Sprintf("the report lists %s, which is not one of this run's items", it.JobItemID))
			continue
		}
		if w := m2eWantByKind[x.Kind]; it.State != w.State || m2eText(it.Reason) != w.Reason || m2eText(it.ErrorClass) != w.ErrorClass {
			kindProblems = append(kindProblems, fmt.Sprintf("item %02d (%s) is reported %s/%s/%s, want %s/%s/%s", x.Index, x.Kind, it.State, m2eText(it.Reason), m2eText(it.ErrorClass), w.State, w.Reason, w.ErrorClass))
		}
	}
	m2eProblems(t, "phase 1: the items that are final at the sample are what their kind says", kindProblems)
	inflight := r.inflight(t)
	t.Logf("NOTE outage phase1 sample: t+%.1fs after the first Run: %s, %d of %d not terminal, is_complete %v; the sink holds %d session(s) right now (a held send is in flight)",
		sampleAt.Sub(runStart).Seconds(), m2eTotalsLine(rep.Totals), m2oRunFirst-len(terminal), m2oRunFirst, rep.Progress.IsComplete, inflight.Inflight)

	// ---- settle EXACTLY what is final and record it: the platform's poll-and-settle step, once, at this moment ----
	cl, err := ad.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify refused the report: %s", r.scrub(err.Error()))
	}
	if len(cl.Unmapped) != 0 || len(cl.Settle) != len(terminal) {
		t.Fatalf("Classify sorted the report into settle %d (terminal %d), not-yet %d, unmapped %v: every terminal item must be settled and nothing else", len(cl.Settle), len(terminal), len(cl.NotYet), cl.Unmapped)
	}
	res, err := e.store.Settle(r.ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	if err != nil {
		t.Fatalf("Settle refused the adapter's list: %v", err)
	}
	ledger := e.ledger(t, job)
	m2eProblems(t, "phase 1: the settle of what was final", m2eCompare(rep.Items, cl.Settle, ledger, frozen, m2eDecide))
	var settled, charged []string
	for _, s := range cl.Settle {
		settled = append(settled, s.ItemRef)
		if m2eHand(s.Outcome.State).Kind == "charge" {
			charged = append(charged, s.ItemRef)
		}
	}
	sort.Strings(settled)
	sort.Strings(charged)
	if res.Charged != len(charged) || res.NotFinal != 0 || res.AlreadySettled != 0 || len(ledger) != len(charged) || len(charged) < 1 {
		t.Errorf("the settle of what was final wrote charged=%d notfinal=%d already=%d, the ledger holds %d charge row(s), the oracle charges %d item(s) (at least one: an item was sent)", res.Charged, res.NotFinal, res.AlreadySettled, len(ledger), len(charged))
	}
	e.requireBooks(t, "phase 1 after the settle of what was final")

	// ---- what the platform, module 2 and the sink hold now: the record every later phase is compared with ----
	books := e.m2oBooks(t, tn, job)
	mod := r.m2oModuleJobs(t, tn)
	var modProblems []string
	if mod.Jobs != m2oRunFirst {
		modProblems = append(modProblems, fmt.Sprintf("module 2 holds %d job(s) for the tenant, the platform handed it %d", mod.Jobs, m2oRunFirst))
	}
	if _, has := mod.IDs[late.ID]; has {
		modProblems = append(modProblems, "module 2 already holds a job for the late item, which was never handed to it")
	}
	for _, it := range tn.Items[:m2oRunFirst] {
		if mod.IDs[it.ID] != it.JobID {
			modProblems = append(modProblems, fmt.Sprintf("item %02d: module 2's job row says %q, the adapter's Run answered %q", it.Index, mod.IDs[it.ID], it.JobID))
		}
	}
	m2eProblems(t, "phase 1: module 2's own job rows", modProblems)
	view := r.sinkView(t)
	sink := m2oSinkIDs(view)
	for a, ids := range sink {
		if len(ids) != 1 {
			t.Errorf("phase 1: the sink holds %d messages for %s, want at most one", len(ids), a)
		}
		if x := r.known()[a]; x == nil || x.Kind != "ok" {
			t.Errorf("phase 1: the sink holds a message for %s, which is not a recipient it accepts", a)
		}
	}

	m2oCreateState(t, r)
	p1 := m2oPhase1{
		RunID: r.runID, UserID: tn.UserID, CampaignID: tn.CampaignID, PoolID: tn.PoolID, AccountID: tn.AccountID, From: tn.From, Job: job, Run: m2oRunFirst,
		Dials: prior, HoldMs: m2oHoldMs, Reserved: rsv.Reserved, UnitCost: int64(rsv.UnitCost), Totals: rep.Totals, Terminal: terminal, Settled: settled, ChargedRefs: charged,
		Books: books, Module: mod, Sink: sink, SampleAt: sampleAt.UTC(), SinceRunSec: sampleAt.Sub(runStart).Seconds(),
	}
	for _, it := range tn.Items {
		p1.Items = append(p1.Items, m2oItem{Index: it.Index, ID: it.ID, Email: it.Email, Kind: it.Kind, JobID: it.JobID})
	}
	m2oSave(t, r, 1, p1)
	t.Logf("NOTE outage phase1 recorded: %s; module 2 holds %d job(s) %v; the sink holds %d message(s); settled %d final item(s) (%d charged: %v); the late item %s has no job",
		books, mod.Jobs, mod.States, len(sink), len(settled), len(charged), charged, late.ID)
	t.Logf("NOTE outage phase1 live: a job is in flight in module 2 (%s at t+%.1fs, %d of %d items not terminal) and phase 1 is recorded in %s; the stage may stop m2 now", m2eTotalsLine(rep.Totals), p1.SinceRunSec, m2oRunFirst-len(terminal), m2oRunFirst, m2oStateTable())
}

// TestM2Worker_OutagePhase2Down is phase 2: module 2 is stopped.
func TestM2Worker_OutagePhase2Down(t *testing.T) {
	m2oControls(t, 2)
	if !m2oLive(t, 2) {
		return
	}
	r := m2wNewRun(t)
	var p1 m2oPhase1
	m2oLoad(t, r, 1, 2, &p1)
	r.runID = p1.RunID
	tn := p1.tenant()
	r.tenants = []*m2wTenant{tn}
	late, first := tn.Items[p1.Run], tn.Items[0]

	// The adapter is built exactly as in phase 1 (m2.FromConfig over the same URL and token, the tenant resolved by internal/tenant), with ONE difference, which this test says: the calls below use a SHORT per-attempt
	// timeout (3 s) and ZERO retries, for speed and so that each call is one dial; the polling loop afterwards uses phase 1's own PRODUCTION dials (20 s, two retries, 250 ms backoff) under a context deadline.
	e := m2oEnv(t, r, 3*time.Second, 0, 0)
	short := e.client(t, tn)
	prod := e.withDials(t, 20*time.Second, 2, 250*time.Millisecond).client(t, tn)

	type call struct {
		name string
		do   func(ctx context.Context) (any, error)
	}
	calls := []call{
		{"Quote", func(ctx context.Context) (any, error) { return short.Quote(ctx, 5) }},
		{"Run of the late item (a write)", func(ctx context.Context) (any, error) { return short.Run(ctx, late.ID) }},
		{"ReportJob", func(ctx context.Context) (any, error) { return short.ReportJob(ctx, first.ID) }},
		{"ReportJobByID", func(ctx context.Context) (any, error) { return short.ReportJobByID(ctx, first.JobID) }},
		{"ReportCampaignPage", func(ctx context.Context) (any, error) { return short.ReportCampaignPage(ctx, tn.CampaignID, "") }},
		{"ReportCampaign", func(ctx context.Context) (any, error) { return short.ReportCampaign(ctx, tn.CampaignID) }},
		{"Walk", func(ctx context.Context) (any, error) {
			pages := 0
			st, err := short.Walk(ctx, tn.CampaignID, func(sendapi.CampaignPage) error { pages++; return nil })
			return struct {
				Stats sendapi.WalkStats
				Pages int
			}{st, pages}, err
		}},
		{"Cancel (a write)", func(ctx context.Context) (any, error) { return short.Cancel(ctx, tn.CampaignID) }},
		{"Describe", func(ctx context.Context) (any, error) { return e.ad.Describe(ctx) }},
	}
	var lines []string
	for _, c := range calls {
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		start := time.Now()
		v, err := c.do(ctx)
		took := time.Since(start)
		cancel()
		m2eProblems(t, "phase 2: a call against the stopped module", m2oJudgeCall(c.name, v, err, took, 8*time.Second))
		cause, sent, attempts := m2oCause(err)
		line := fmt.Sprintf("%-31s kind=%s cause=%q sent=%v attempts=%d took=%s value=%s", c.name, modclient.KindOf(err), cause, sent, attempts, took.Round(time.Millisecond), map[bool]string{true: "zero", false: "NOT ZERO"}[m2oIsZero(v)])
		lines = append(lines, line)
		t.Logf("NOTE outage phase2 call %s", line)
	}

	// ---- a polling loop with the production dials stops by the context it was given ----
	const pollFor = 6 * time.Second
	pctx, pcancel := context.WithTimeout(r.ctx, pollFor)
	st := m2oPoll(pctx, prod, tn.CampaignID, 300*time.Millisecond)
	pcancel()
	m2eProblems(t, "phase 2: the bounded polling loop", m2oPollProblems(st, pollFor, 3*time.Second))
	t.Logf("NOTE outage phase2 poll: ReportCampaign every 300 ms with the production dials (20 s, 2 retries, 250 ms backoff) under a %s context: %d call(s) in %s, %d answered without error, %d returned a value, errors by kind %v", pollFor, st.Calls, st.Elapsed.Round(time.Millisecond), st.Nil, st.Values, st.Kinds)

	// ---- the platform's poll-and-settle step, three times, against the dead module: it must fail and change nothing ----
	before := e.m2oBooks(t, tn, p1.Job)
	for i := 1; i <= 3; i++ {
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		err := e.m2oPlatformStep(ctx, short, short, p1.Job, tn.CampaignID)
		cancel()
		if err == nil || !m2oTransient(err) {
			t.Errorf("phase 2: the platform's poll-and-settle step (round %d) ended with %v (kind %s), want a typed unavailable or timeout from the read and nothing settled", i, err, modclient.KindOf(err))
		}
	}
	after := e.m2oBooks(t, tn, p1.Job)
	m2eProblems(t, "phase 2: the books after three poll-and-settle steps against the stopped module", m2oBooksProblems(before, after))

	// ---- nothing was settled, released or invented: the platform's rows are EXACTLY what phase 1 recorded ----
	m2eProblems(t, "phase 2: the platform's books against what phase 1 recorded", m2oBooksProblems(p1.Books, after))
	e.requireBooks(t, "phase 2 with the module stopped")
	if n := len(e.ledger(t, p1.Job)); n != p1.Books.Charges {
		t.Errorf("phase 2: the ledger holds %d charge row(s) of the job, phase 1 recorded %d", n, p1.Books.Charges)
	}
	mod := r.m2oModuleJobs(t, tn)
	m2eProblems(t, "phase 2: module 2's own job rows against what phase 1 recorded (the write that failed created nothing)", m2oModuleProblems(p1.Module, mod))
	if _, has := mod.IDs[late.ID]; has {
		t.Errorf("phase 2: module 2 holds a job for the late item although its Run failed")
	}
	view := r.sinkView(t)
	sink := m2oSinkIDs(view)
	for a, ids := range sink {
		if len(ids) != 1 {
			t.Errorf("phase 2: the sink holds %d messages for %s, want at most one", len(ids), a)
		}
	}
	for a, ids := range p1.Sink {
		if fmt.Sprint(sink[a]) != fmt.Sprint(ids) {
			t.Errorf("phase 2: the sink held %v for %s in phase 1 and holds %v now: a message that was sent changed", ids, a, sink[a])
		}
	}

	// ---- the platform's readiness does not follow a module ----
	code, body := m2oReadyz(e.pool)
	m2eProblems(t, "phase 2: the platform's /readyz with module 2 stopped", m2oReadyzProblems(code, body))
	t.Logf("NOTE outage phase2 readyz: the platform's own readiness handler (internal/httpapi, a database ping and a draining flag; its Deps hold no module) answers %d %s while module 2 is stopped", code, body)

	m2oSave(t, r, 2, m2oPhase2{Books: after, Module: mod, Sink: sink, Calls: lines, Polls: st.Calls, At: time.Now().UTC()})
	t.Logf("NOTE outage phase2 recorded: %s; module 2 holds %d job(s); the sink holds %d message(s) (phase 1 saw %d: the send in flight when the stop began finished during the drain, or nothing was in flight)", after, mod.Jobs, len(sink), len(p1.Sink))
	t.Logf("NOTE outage phase2 live: %d calls against the stopped module were typed unavailable or timeout with no value, the %d-call polling loop halted by its context, three poll-and-settle steps changed nothing, the books, public.job_items and module 2's job rows are what phase 1 recorded", len(calls), st.Calls)
}

// TestM2Worker_OutagePhase3Resume is phase 3: module 2 is back and the job completes exactly once.
func TestM2Worker_OutagePhase3Resume(t *testing.T) {
	m2oControls(t, 3)
	if !m2oLive(t, 3) {
		return
	}
	r := m2wNewRun(t)
	var p1 m2oPhase1
	var p2 m2oPhase2
	m2oLoad(t, r, 1, 3, &p1)
	m2oLoad(t, r, 2, 3, &p2)
	r.runID = p1.RunID
	tn := p1.tenant()
	r.tenants = []*m2wTenant{tn}
	// the end of the outage proof puts module 2 and the sink back as the other tests expect them: LIFO, the campaign is cancelled (a no-op once it is complete) first, the sink drained and reset second, the dials restored last
	t.Cleanup(func() { r.restoreDials(t, p1.Dials) })
	t.Cleanup(r.restoreSink)
	t.Cleanup(r.cancelAll)
	e := m2oEnv(t, r, 20*time.Second, 2, 250*time.Millisecond)
	ad := e.client(t, tn)
	job, n := p1.Job, len(tn.Items)
	late := tn.Items[p1.Run]
	wantSent, wantBounced, wantFailed := len(tn.emails("ok")), len(tn.emails("bounce")), len(tn.emails("defer"))
	wantCharged := wantSent + wantBounced

	// ---- before anything is asked of module 2: nothing happened to the platform's rows or to the module's own while the stage started it ----
	m2eProblems(t, "phase 3: the platform's books before the first call", m2oBooksProblems(p1.Books, e.m2oBooks(t, tn, job)))
	m2eProblems(t, "phase 3: module 2's own job rows before the first call (the Run that failed in phase 2 created nothing, the restart created nothing)", m2oModuleProblems(p1.Module, r.m2oModuleJobs(t, tn)))

	// ---- the module comes back: transient unavailable/timeout is retried, a wrong answer is not ----
	first, tr, took := m2oResume(t, r, ad, tn, &p1, 150*time.Second)
	t.Logf("NOTE outage phase3 resume: module 2 answered %s after the test began asking, with %s transient error(s) before; its first report: %s, %d item(s) listed, is_complete %v", took.Round(time.Millisecond), tr, m2eTotalsLine(first.Totals), len(first.Items), first.Progress.IsComplete)
	r.requireWorker(t)
	r.setMode(t, m2wMode{RejectRcpt: tn.emails("bounce"), RejectCode: 550, DeferRcpt: tn.emails("defer")}) // the hold of phase 1 is lifted: the rest of the campaign is quick, the recipients' behaviour is unchanged

	// ---- the platform retries the write that failed: now it creates the job, once ----
	var lr sendapi.RunResult
	runStart := time.Now()
	for {
		var err error
		lr, err = ad.Run(r.ctx, late.ID)
		if err == nil {
			break
		}
		if !m2oTransient(err) || time.Since(runStart) > 60*time.Second {
			t.Fatalf("Run of the late item after the restart failed (%s) after %s: %s", modclient.KindOf(err), time.Since(runStart).Round(time.Second), r.scrub(err.Error()))
		}
		time.Sleep(time.Second)
	}
	if !lr.Created || lr.State != m2wStateQueued || lr.Class != sendapi.ClassPending || lr.JobID == "" {
		t.Fatalf("Run of the late item answered %+v, want a NEW job (Created, queued, pending): the failed write of phase 2 must have created nothing", lr)
	}
	late.JobID = lr.JobID
	t.Logf("NOTE outage phase3 late item: Run of item %02d answered 202 queued, job %s (a new job: the write that failed during the outage left nothing behind)", late.Index, lr.JobID)

	// ---- poll to completion through the adapter: after the first answer no error is acceptable ----
	var rep sendapi.CampaignReport
	start, last := time.Now(), ""
	for {
		var err error
		rep, err = ad.ReportCampaign(r.ctx, tn.CampaignID)
		if err != nil {
			t.Fatalf("ReportCampaign failed (%s) after module 2 had answered: only the first seconds of the recovery may fail, this is a wrong answer: %s", modclient.KindOf(err), r.scrub(err.Error()))
		}
		if line := m2eTotalsLine(rep.Totals); line != last {
			last = line
			t.Logf("NOTE outage phase3 t+%5.1fs: the adapter's campaign report (%d page(s), %d item(s)): %s", time.Since(start).Seconds(), rep.Pages, len(rep.Items), line)
		}
		if m2eAllTerminal(rep, n) {
			break
		}
		if time.Since(start) > 150*time.Second {
			t.Fatalf("the campaign did not finish within %s after the restart (%d item(s) submitted): %s, is_complete %v\n%s", time.Since(start).Round(time.Second), n, m2eTotalsLine(rep.Totals), rep.Progress.IsComplete, r.diagnostics(t, tn))
		}
		time.Sleep(m2wPollEvery)
	}
	t.Logf("NOTE outage phase3 report: %s, %d page(s), progress terminal=%d complete=%v, items match total %v", m2eTotalsLine(rep.Totals), rep.Pages, rep.Progress.Terminal, rep.Progress.IsComplete, rep.ItemsMatchTotal())

	// ---- (a) every item reported exactly once, with the state of its kind, and with the job id it had before the outage ----
	m2eProblems(t, "(a) the report against what was submitted", m2eKinds(tn, rep.Items))
	if rep.Totals.Total != int64(n) || rep.Totals.Sent != int64(wantSent) || rep.Totals.Bounced != int64(wantBounced) || rep.Totals.Failed != int64(wantFailed) || rep.Totals.Cancelled != 0 || rep.Totals.InFlight != 0 || rep.Totals.Pending != 0 || rep.Totals.WaitingForCapacity != 0 {
		t.Errorf("(a) the totals are (%s), want total %d sent %d bounced %d failed %d and nothing else", m2eTotalsLine(rep.Totals), n, wantSent, wantBounced, wantFailed)
	}
	byID := map[string]sendapi.Item{}
	for _, it := range rep.Items {
		byID[it.JobItemID] = it
	}
	for _, it := range tn.Items {
		one, err := ad.ReportJob(r.ctx, it.ID)
		if err != nil {
			t.Errorf("(a) ReportJob of item %02d failed (%s): %s", it.Index, modclient.KindOf(err), r.scrub(err.Error()))
			continue
		}
		row := byID[it.ID]
		if one.JobID != it.JobID || one.State != row.State || m2eText(one.Reason) != m2eText(row.Reason) || m2eText(one.ErrorClass) != m2eText(row.ErrorClass) {
			t.Errorf("(a) the single-job report of item %02d is job %s %s/%s/%s, the campaign report says %s/%s/%s and the job id before the outage (or the answer of Run) is %s", it.Index, one.JobID, one.State, m2eText(one.Reason), m2eText(one.ErrorClass), row.State, m2eText(row.Reason), m2eText(row.ErrorClass), it.JobID)
		}
	}

	// ---- (b) the sink: exactly one message per sent item, none for the others, and nothing that was there during the outage replaced ----
	view := r.sinkView(t)
	capN := int(e.cfgInt(t, "job_max_failed_attempts"))
	e.sinkAgainstReport(t, "(b) the sink against the report", tn, rep.Items, view, capN)
	m2eProblems(t, "(b) nothing was sent twice because of the outage", m2oSinkProblems(tn, rep.Items, view, p2.Sink))
	totalMsgs := 0
	for _, it := range tn.Items {
		totalMsgs += view.messages(it.Email)
	}
	t.Logf("NOTE outage phase3 sink: %d message(s) of this run for %d sent item(s); %d message(s) were already there while the module was down and are unchanged (same Message-IDs)", totalMsgs, wantSent, len(p2.Sink))
	mod := r.m2oModuleJobs(t, tn)
	var modProblems []string
	if mod.Jobs != n {
		modProblems = append(modProblems, fmt.Sprintf("module 2 holds %d job(s) for the tenant, want exactly %d (one per item)", mod.Jobs, n))
	}
	for _, it := range tn.Items {
		if mod.IDs[it.ID] != it.JobID {
			modProblems = append(modProblems, fmt.Sprintf("item %02d: module 2's job row says %q, the job id of the item is %q", it.Index, mod.IDs[it.ID], it.JobID))
		}
		if st, row := mod.States[it.ID], byID[it.ID]; st != row.State {
			modProblems = append(modProblems, fmt.Sprintf("item %02d: module 2's own row is %q, its report says %q", it.Index, st, row.State))
		}
	}
	m2eProblems(t, "(b) module 2's own job rows at the end", modProblems)

	// ---- (c) classify, settle through the money core, compare with the ledger (the same comparison as the end-to-end test) ----
	frozen := e.prices(t, job)
	cl, err := ad.Classify(rep.Items)
	if err != nil {
		t.Fatalf("Classify refused the report: %s", r.scrub(err.Error()))
	}
	if len(cl.Settle) != n || len(cl.NotYet) != 0 || len(cl.Unmapped) != 0 {
		t.Fatalf("Classify sorted %d item(s) into settle %d, not-yet %v, unmapped %v, want all %d settled", n, len(cl.Settle), cl.NotYet, cl.Unmapped, n)
	}
	res, err := e.store.Settle(r.ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	if err != nil {
		t.Fatalf("Settle refused the adapter's list: %v", err)
	}
	ledger := e.ledger(t, job)
	m2eProblems(t, "(c) the report, the settle list and the ledger", m2eCompare(rep.Items, cl.Settle, ledger, frozen, m2eDecide))
	wantCredits := int64(wantSent)*frozen["m2_accepted"] + int64(wantBounced)*frozen["m2_bounced"]
	prevCharged := len(p1.ChargedRefs)
	if res.Charged != wantCharged-prevCharged || res.AlreadySettled != prevCharged || res.Free != wantFailed || res.NotFinal != 0 || len(res.Unmapped) != 0 {
		t.Errorf("(c) the settle after the outage wrote charged=%d already-settled=%d free=%d notfinal=%d unmapped=%v, want charged %d (every charged item except the %d phase 1 had settled), already-settled %d, free %d and nothing else", res.Charged, res.AlreadySettled, res.Free, res.NotFinal, res.Unmapped, wantCharged-prevCharged, prevCharged, prevCharged, wantFailed)
	}
	var ledgerCredits int64
	rows := map[string]int{}
	for _, c := range ledger {
		ledgerCredits += c.Units * c.Cost
		rows[c.ItemRef]++
	}
	if len(ledger) != wantCharged || ledgerCredits != wantCredits {
		t.Errorf("(c) the ledger holds %d charge(s) worth %d credits, want %d worth %d", len(ledger), ledgerCredits, wantCharged, wantCredits)
	}
	for _, ref := range p1.ChargedRefs {
		if rows[ref] != 1 {
			t.Errorf("(c) item %s was charged in phase 1 and the ledger holds %d charge row(s) for it now, want exactly one: the outage must not charge it again", ref, rows[ref])
		}
	}
	t.Logf("NOTE outage phase3 settle: charged %d new item(s) = %d credits, already settled in phase 1 %d, free %d; the ledger holds %d charge row(s) = %d credits, one per charged item", res.Charged, res.Credits, res.AlreadySettled, res.Free, len(ledger), ledgerCredits)

	// ---- (d) the books close ----
	bal := e.balance(t, tn)
	if int64(bal.Available) != m2eGrant-p1.Reserved || int64(bal.Reserved) != p1.Reserved-wantCredits {
		t.Errorf("(d) after the settle the balance is available %d reserved %d, want available %d reserved %d", bal.Available, bal.Reserved, m2eGrant-p1.Reserved, p1.Reserved-wantCredits)
	}
	before := e.fingerprint(t, tn)
	again, err := e.store.Settle(r.ctx, money.SettleReq{JobID: job, Items: cl.Settle})
	if err != nil {
		t.Fatalf("(e) settling the same adapter output again was refused: %v", err)
	}
	if again.Charged != 0 || again.Credits != 0 || again.AlreadySettled != wantCharged || again.Free != wantFailed || again.NotFinal != 0 || len(again.Unmapped) != 0 {
		t.Errorf("(e) settling again wrote charged=%d credits=%d already-settled=%d free=%d notfinal=%d unmapped=%v, want charged 0 credits 0 already-settled %d free %d", again.Charged, again.Credits, again.AlreadySettled, again.Free, again.NotFinal, again.Unmapped, wantCharged, wantFailed)
	}
	if after := e.fingerprint(t, tn); after != before {
		t.Errorf("(e) the books changed although the same settle was replayed (%s -> %s)", before, after)
	}
	e.setStatus(t, job, "succeeded")
	rel, err := e.store.Release(r.ctx, job)
	if err != nil {
		t.Fatalf("(d) release of the job's reserve was refused: %v", err)
	}
	if rel.Released != p1.Reserved-wantCredits {
		t.Errorf("(d) the release returned %d credits, want reserve %d - charged %d = %d", rel.Released, p1.Reserved, wantCredits, p1.Reserved-wantCredits)
	}
	bal = e.balance(t, tn)
	if int64(bal.Available) != m2eGrant-wantCredits || bal.Reserved != 0 {
		t.Errorf("(d) after the release the balance is available %d reserved %d, want available %d (grant - charged) reserved 0", bal.Available, bal.Reserved, m2eGrant-wantCredits)
	}
	if rs, ch, rl := e.reserveRow(t, job); rs != p1.Reserved || ch != wantCredits || rl != p1.Reserved-wantCredits {
		t.Errorf("(d) platform.reserves holds reserved %d charged %d released %d, want %d, %d, %d", rs, ch, rl, p1.Reserved, wantCredits, p1.Reserved-wantCredits)
	}
	e.requireBooks(t, "(d) after the release")

	// ---- (e) a second Run of every item is the module's 200 replay and sends nothing ----
	var replayProblems []string
	for _, it := range tn.Items {
		res, err := ad.Run(r.ctx, it.ID)
		row := byID[it.ID]
		switch {
		case err != nil:
			replayProblems = append(replayProblems, fmt.Sprintf("a second Run of item %02d failed (%s): %s", it.Index, modclient.KindOf(err), r.scrub(err.Error())))
		case res.Created || res.JobID != it.JobID || res.State != row.State || res.Class != sendapi.ClassTerminal:
			replayProblems = append(replayProblems, fmt.Sprintf("a second Run of item %02d answered %+v, want the replay (200, not created) of job %s in state %s, class terminal", it.Index, res, it.JobID, row.State))
		}
	}
	m2eProblems(t, "(e) the replay of Run", replayProblems)
	v2 := r.sinkView(t)
	if len(v2.Msgs) != len(view.Msgs) {
		t.Errorf("(e) the sink holds messages for %d recipient(s) after the replays, %d before: a replay sent again", len(v2.Msgs), len(view.Msgs))
	}
	for _, it := range tn.Items {
		if v2.Rcpt[it.Email] != view.Rcpt[it.Email] || v2.messages(it.Email) != view.messages(it.Email) {
			t.Errorf("(e) the sink's counts of %s moved after the replays (RCPT %+v -> %+v, messages %d -> %d)", it.Email, view.Rcpt[it.Email], v2.Rcpt[it.Email], view.messages(it.Email), v2.messages(it.Email))
		}
	}
	if mod2 := r.m2oModuleJobs(t, tn); mod2.Jobs != n {
		t.Errorf("(e) module 2 holds %d job(s) after the replays, want %d: a replay created a job", mod2.Jobs, n)
	}

	m2oSave(t, r, 3, map[string]any{
		"resume_seconds": took.Seconds(), "transient_errors": tr.N, "charged_in_phase3": res.Charged, "already_settled": res.AlreadySettled, "ledger_rows": len(ledger), "credits": ledgerCredits, "at": time.Now().UTC(),
	})
	t.Logf("NOTE outage phase3 books: after the release available %d reserved 0; platform.reserves %d/%d/%d; the replay of the settle wrote charged 0, already-settled %d, free %d; platform.balance_drift and platform.reserve_overdrawn are empty; a second Run of all %d items was the 200 replay", bal.Available, p1.Reserved, wantCredits, p1.Reserved-wantCredits, again.AlreadySettled, again.Free, n)
	t.Logf("NOTE outage phase3 live: module 2 came back after %s (%s transient error(s)); %d items were reported exactly once, the sink holds %d message(s) for %d sent items, the ledger holds %d charge row(s) for %d charged items, the books are closed", took.Round(time.Millisecond), tr, n, totalMsgs, wantSent, len(ledger), wantCharged)
}
