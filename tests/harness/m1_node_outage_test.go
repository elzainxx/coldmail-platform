//go:build harness && m1node

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 3, chunk B12 for module 1: module 1's control plane (m1n) goes down in the MIDDLE of a job, and the platform neither invents nor loses anything. Three tests, three separate `go test`
// processes, run in order by sub-stage M1N-4b of tests/harness/m1node/m1node.sh, which stops m1n after the first and starts it again after the second. They carry the order in their names, find
// each other's state in the scratch table harness_outage.m1 (m1_node_outage_helpers_test.go), and fail with a sentence, never skip, when the state of the phase before is not there. The ordinary
// run of the stage excludes them by name (-test.skip), and each refuses to run unless the sub-stage set HARNESS_M1_OUTAGE=1; their PASS lines are required and counted by the stage like the others.
//
//	TestM1Node_OutagePhase1Start    a real tenant (a real auth.users row, its Scope, its adapter), credits granted, a reserve held, and ONE job submitted with Prepare + Submit that cannot be finished
//	                                when the stage stops the module a few seconds later: 8 rows the node is not needed for that settle at once (charged), a repeat and a malformed row, and 20 rows each at a
//	                                name of its own that the fixture's DNS never answers (2 s each, the drainer is sequential: 40 s of work). It waits until the adapter's own Status shows PROGRESS (at least one
//	                                row settled) and shows the job NOT complete, checks module 1's own work-item table agrees, and records the tenant, the ids, the items, the reserve and the platform's
//	                                row counts. It settles nothing.
//	TestM1Node_OutagePhase2Down     with the control plane stopped (the stage proved the container is not running and its health endpoint no longer answers): every call the platform polls or writes
//	                                with (Status, Results with and without a cursor, Drain, Submit, Health, Ready), through an adapter built exactly as phase 1's (same factory dials: 5 s per attempt, no retries),
//	                                returns an error of kind Unavailable or Timeout and NO value beside it; a polling loop on a 4 s context makes several polls, every one typed, and stops by its context; a direct
//	                                connection fails; module 1's own work-item table shows the job still NOT finished and NOT moving over several seconds; the platform's ledger, reserve, charge and
//	                                public.job_items rows, the tenant's balance and module 1's job and row counts are EXACTLY what phase 1 recorded; balance_drift and reserve_overdrawn are empty. The assertion
//	                                functions are first run on fakes and must report them (a nil error with zero values, a value beside an error, a wrong kind, a settlement during the outage, a job that had
//	                                already finished)
//	TestM1Node_OutagePhase3Resume   with the control plane started again and ready: Status is polled to complete as the platform does (a typed Unavailable or Timeout in the first 60 s is a transient and is
//	                                counted; any other kind is a wrong answer and fails at once), every page is read, each page is settled through the money core, and the same comparisons as the end-to-end
//	                                test (m1_node_e2e_test.go: the control, the same flow with the module up) hold: one result per sent row with the verdict its address must produce, module 1 holds exactly ONE
//	                                job for the tenant with exactly the rows phase 1 submitted, the ledger holds exactly one charge per charged item and none for a free one (charge.Decide and a hand-written
//	                                oracle agree), the books close (balance_drift, reserve_overdrawn empty, the reserve released), and a replay of every settle writes nothing
//
// WHY THE ROWS ARE DNS-ONLY. Module 1 registers its node as provisioning at every boot (UpsertNodeState), so the restart resets the node the SMTP stage needs: a row that needed it would settle as
// our own capacity failure after the restart, which would be module 1's behaviour and not the outage's. These rows need no node, so their verdicts are the same whether the module was restarted or not.

// m1oSent is the number of rows of the job that are SENT (the phases are written for it).
const m1oSent = m1oFast + 1 + m1oSlow // the rows that are sent: fast, one repeat, slow (the malformed one is not)

// TestM1Node_OutagePhase1Start: see the file comment.
func TestM1Node_OutagePhase1Start(t *testing.T) {
	m1oRequireStage(t)
	s := newM1eStackWith(t, config.Secret(randomHex(t, 32)), m1oOptions())
	s.m1oCreateState()
	tn := s.newTenant("outage", true)
	items, names := m1oBuildItems(s.m1nEnv)
	for _, n := range names {
		s.fx.setMode(n, "silent")
	}
	plan := m1ad.Prepare(m1oInputs(items))
	if err := plan.Err(); err != nil {
		t.Fatalf("Prepare refused the batch: %v", err)
	}
	if len(plan.Items) != m1oSent || len(plan.Malformed) != 1 || plan.Duplicates != 1 {
		t.Fatalf("Prepare found %d to send, %d malformed, %d repeat(s); the outage job is written for %d, 1 and 1", len(plan.Items), len(plan.Malformed), plan.Duplicates, m1oSent)
	}

	ma := s.mny
	if _, err := ma.store.Grant(s.ctx, money.GrantReq{UserID: tn.userID, Module: "m1", Amount: m1oGrant, AdminID: s.adminID, Reason: "module 1 outage test", Key: "m1o-grant-" + tn.userID}); err != nil {
		t.Fatalf("granting credits: %v", err)
	}
	job := s.newJob(tn.userID)
	rsv := ma.reserve(job, len(items))
	ma.setStatus(job, "running")

	fxBase := s.fx.seq()
	submitted := time.Now()
	accepted, err := tn.ad.Submit(s.ctx, plan, m1eKey("outage"))
	if err != nil {
		t.Fatalf("Submit failed (kind %v): %v", modclient.KindOf(err), err)
	}
	jobID, counts := accepted.JobID, accepted.Counts

	// IN FLIGHT, proved by the adapter's own Status and not assumed: progress has started (a row is settled) and the job is not complete
	var js m1ad.JobStatus
	deadline := time.Now().Add(60 * time.Second)
	for {
		js, err = tn.ad.Status(s.ctx, jobID)
		if err != nil {
			t.Fatalf("Status failed while the module was still up (kind %v): %v", modclient.KindOf(err), err)
		}
		if js.Complete {
			t.Fatalf("the job is already complete (%+v) %v after the submission: it cannot be in flight when the stage stops the module; the slow rows are too few or the fixture's silence did not hold", js.Counts, time.Since(submitted).Round(100*time.Millisecond))
		}
		if js.Counts.Settled >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no row of the job was settled 60 s after the submission (module 1 says %+v, status %q, paused %t): the drainer is not working it", js.Counts, js.Status, js.ProcessingPaused)
		}
		time.Sleep(m1nPollEvery)
	}
	progressAfter := time.Since(submitted)
	if js.Counts.Pending <= 0 || js.Counts.Total != m1oSent {
		t.Fatalf("module 1's counts are %+v: want total %d and rows still pending", js.Counts, m1oSent)
	}
	snap := s.m1oSnapshot(tn.userID, jobID)
	if snap.Terminal >= snap.Items {
		t.Fatalf("module 1's work-item table says all %d work item(s) of the job are settled: the job is not in flight", snap.Items)
	}
	if snap.Ledger != 2 || snap.Reserves != 1 || snap.Charges != 0 || snap.JobItems != 0 || snap.Reserved != rsv.Reserved || snap.Available != m1oGrant-rsv.Reserved || snap.ModuleJobs != 1 || snap.ModuleRows != m1oSent {
		t.Fatalf("the books at the start are %+v, want the grant and the reserve only (2 ledger rows, 1 reserve, no charge, no job_items), reserved %d, available %d, one module job of %d rows", snap, rsv.Reserved, m1oGrant-rsv.Reserved, m1oSent)
	}

	s.m1oSave(1, &m1oState{
		KeySecret: s.secret.Reveal(), UserID: tn.userID, PlatformJob: job, ModuleJob: jobID, Grant: m1oGrant, Reserve: rsv,
		Items: m1oItemsToJSON(items), Counts: counts, FxBase: fxBase, Before: snap, Progress: js,
	})
	t.Logf("NOTE B12 p1 tenant: user %s, a real auth.users row; its derived key is provisioned and the secret it derives from is kept (never printed) in %s.m1", m1eShort(tn.userID), m1oSchema)
	t.Logf("NOTE B12 p1 job: module job %s, %d row(s) sent (%d slow: each at a name of its own that the fixture's DNS never answers), 1 malformed never sent; the 202 counted %+v", jobID, m1oSent, m1oSlow, counts)
	t.Logf("NOTE B12 p1 in flight: %v after the submission the adapter's Status says settled %d, pending %d of %d, complete=%t; module 1's own work-item table says %d of %d terminal", progressAfter.Round(100*time.Millisecond), js.Counts.Settled, js.Counts.Pending, js.Counts.Total, js.Complete, snap.Terminal, snap.Items)
	t.Logf("NOTE B12 p1 books: ledger %d row(s) (the grant and the reserve), %d reserve, %d charge(s), %d public.job_items, available %d, reserved %d (reserve %d credits for %d units at %d); nothing is settled", snap.Ledger, snap.Reserves, snap.Charges, snap.JobItems, snap.Available, snap.Reserved, rsv.Reserved, rsv.Units, rsv.UnitCost)
}

// m1oSelfCheck runs the assertion functions of phase 2 on fakes and requires each to report the fault it is for: an outage assertion that cannot fail proves nothing.
func m1oSelfCheck(t *testing.T) {
	t.Helper()
	unavailable := &modclient.Error{Kind: modclient.Unavailable, Module: modclient.M1, Route: "m1.status"}
	for _, c := range []struct {
		name string
		obs  m1oObs
		want string
	}{
		{"a nil error with zero values (a fake that answers although the module is down)", m1oObs{name: "Status", noValue: true}, "no error"},
		{"a value beside an error", m1oObs{name: "Results", err: unavailable, noValue: false}, "came back beside the error"},
		{"a wrong kind of error", m1oObs{name: "Status", err: &modclient.Error{Kind: modclient.Refused}, noValue: true}, "want unavailable or timeout"},
		{"an error that is not typed", m1oObs{name: "Status", err: fmt.Errorf("boom"), noValue: true}, "want unavailable or timeout"},
		{"a Submit that was sent whose error does not say the key may be sent again", m1oObs{name: "Submit", err: &modclient.Error{Kind: modclient.Timeout, Sent: true, Message: "x"}, noValue: true, sent: true}, "may be sent again"},
	} {
		if p := m1oCallProblems(c.obs); !m1eContains(p, c.want) {
			t.Fatalf("the outage assertion on %s said %v, want a line containing %q: it cannot fail", c.name, p, c.want)
		}
	}
	good := m1oObs{name: "Status", err: unavailable, noValue: true}
	if p := m1oCallProblems(good); len(p) != 0 {
		t.Fatalf("the control: a typed Unavailable with no value is what a stopped module gives, the assertion said %v", p)
	}
	if p := m1oCallProblems(m1oObs{name: "Submit", err: unavailable, noValue: true}); len(p) != 0 {
		t.Fatalf("the control: a Submit that never left (Unavailable, not sent) needs no word about the key, the assertion said %v", p)
	}
	if p := m1oCallProblems(m1oObs{name: "Submit", err: &modclient.Error{Kind: modclient.Timeout, Sent: true, Message: "x; the submission is not confirmed, and the same Idempotency-Key may be sent again"}, noValue: true, sent: true, again: true}); len(p) != 0 {
		t.Fatalf("the control: a Submit that was sent and says the key may be sent again is what the adapter gives, the assertion said %v", p)
	}
	before := m1oSnapshot{Ledger: 2, Reserves: 1, Available: 440, Reserved: 60, ModuleJobs: 1, ModuleRows: 29, Items: 28, Terminal: 4}
	if p := m1oBooksProblems(before, before); len(p) != 0 {
		t.Fatalf("the control: equal books must be equal, got %v", p)
	}
	for _, c := range []struct {
		name string
		mut  func(s *m1oSnapshot)
		want string
	}{
		{"a charge settled during the outage", func(s *m1oSnapshot) { s.Ledger++; s.Charges++; s.Reserved-- }, "ledger row count"},
		{"a charge row alone", func(s *m1oSnapshot) { s.Charges++ }, "charge row count"},
		{"a release during the outage", func(s *m1oSnapshot) { s.Available += 10; s.Reserved -= 10 }, "available balance"},
		{"a row invented in public.job_items", func(s *m1oSnapshot) { s.JobItems++ }, "job_items"},
		{"a second reserve", func(s *m1oSnapshot) { s.Reserves++ }, "reserve row count"},
		{"a second module job (a write that got through)", func(s *m1oSnapshot) { s.ModuleJobs++ }, "job count for the tenant"},
		{"module rows that appeared", func(s *m1oSnapshot) { s.ModuleRows++ }, "row count for the job"},
	} {
		now := before
		c.mut(&now)
		if p := m1oBooksProblems(before, now); !m1eContains(p, c.want) {
			t.Fatalf("the books check on %s said %v, want a line containing %q", c.name, p, c.want)
		}
	}
	for _, c := range []struct {
		name    string
		a       m1oSnapshot
		want    string
		wantNil bool
	}{
		{"a job that had finished before the stop", m1oSnapshot{Items: 28, Terminal: 28}, "finished before the stage could stop the module", false},
		{"a job that went backwards", m1oSnapshot{Items: 28, Terminal: 2}, "went backwards", false},
		{"a job that moves while the module is down", m1oSnapshot{Items: 28, Terminal: 6}, "something is still draining it", false},
		{"no work items at all", m1oSnapshot{}, "holds no row for the job", false},
		{"a job in flight and still", m1oSnapshot{Items: 28, Terminal: 6}, "", true},
	} {
		b := c.a
		if c.want == "something is still draining it" {
			b.Terminal++
		}
		p := m1oWhileDownProblems(before, c.a, b)
		if c.wantNil && len(p) != 0 || !c.wantNil && !m1eContains(p, c.want) {
			t.Fatalf("the in-flight check on %s said %v, want %q", c.name, p, c.want)
		}
	}
}

// TestM1Node_OutagePhase2Down: see the file comment.
func TestM1Node_OutagePhase2Down(t *testing.T) {
	m1oRequireStage(t)
	m1oSelfCheck(t)
	st := m1oLoad(t, dial(t, requireEnv(t, envAdminURL), nil), 1)
	s := newM1eStackWith(t, config.Secret(st.KeySecret), m1oOptions())
	tn := s.existingTenant("outage", st.UserID)

	var problems []string
	add := func(p []string) { problems = append(problems, p...) }

	// the module is gone: a direct connection fails, and says how
	class := m1oDialClass(s.base)
	if class == "" {
		t.Fatalf("a TCP connection to module 1 was accepted: the stage did not stop the control plane, so there is no outage to prove anything about")
	}
	t.Logf("NOTE B12 p2 down: a direct connection to module 1 fails: %s", class)

	// every call the platform polls or writes with, through an adapter built as phase 1's (the same factory dials: 5 s per attempt, no retries; the context below only bounds the whole test)
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Minute)
	defer cancel()
	plan := m1ad.Prepare([]m1ad.Input{{ItemRef: "down-1", Address: fresh("e2eo") + "@" + m1eRecipe(s.m1nEnv, "no_mx").domain}})
	for _, o := range m1oDownCalls(ctx, tn.ad, st.ModuleJob, plan) {
		add(m1oCallProblems(o))
		kind, sent := modclient.KindOf(o.err), false
		if me, ok := asModclientErr(o.err); ok {
			sent = me.Sent
		}
		t.Logf("NOTE B12 p2 %-28s kind=%s, request left=%t, says the same key may be sent again=%t, no value beside the error=%t", o.name, kind, sent, o.again, o.noValue)
	}

	// a polling loop that loops on the error stops by the context it was given
	const budget = 4 * time.Second
	h := m1oPollUntilHalted(tn.ad, st.ModuleJob, budget)
	add(m1oHaltProblems(h, budget))
	t.Logf("NOTE B12 p2 halt: a polling loop on a %v context made %d poll(s), every one failed typed (%s), and stopped after %v", budget, h.polls, m1oKindCounts(h.kinds), h.took.Round(10*time.Millisecond))

	// module 1's own tables: the job is not finished and does not move; nothing was created by the writes above
	a := s.m1oSnapshot(st.UserID, st.ModuleJob)
	time.Sleep(3 * time.Second)
	b := s.m1oSnapshot(st.UserID, st.ModuleJob)
	add(m1oWhileDownProblems(st.Before, a, b))
	t.Logf("NOTE B12 p2 module tables: %d of %d work item(s) of the job are terminal (phase 1 saw %d) and still %d three seconds later: the job was in flight when the module stopped and nothing drains it; module 1 holds %d job(s) for the tenant with %d row(s)", a.Terminal, a.Items, st.Before.Terminal, b.Terminal, a.ModuleJobs, a.ModuleRows)

	// the platform's own books are exactly what phase 1 recorded
	add(prefixed("books", m1oBooksProblems(st.Before, b)))
	add(s.viewProblems("while the module is down"))
	t.Logf("NOTE B12 p2 books: ledger %d row(s), %d reserve(s), %d charge(s), %d public.job_items, available %d, reserved %d: exactly what phase 1 recorded; balance_drift and reserve_overdrawn are empty", b.Ledger, b.Reserves, b.Charges, b.JobItems, b.Available, b.Reserved)

	if len(problems) != 0 {
		t.Fatalf("%d difference(s) while module 1 was down:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	st.During = &b
	s.m1oSave(2, st)
}

// TestM1Node_OutagePhase3Resume: see the file comment.
func TestM1Node_OutagePhase3Resume(t *testing.T) {
	m1oRequireStage(t)
	st := m1oLoad(t, dial(t, requireEnv(t, envAdminURL), nil), 2)
	s := newM1eStackWith(t, config.Secret(st.KeySecret), m1oOptions())
	tn := s.existingTenant("outage", st.UserID)
	items := m1oItemsFromJSON(st.Items)
	sent := m1eSent(items)
	r := &m1eRun{tn: tn, items: items, grant: st.Grant, job: st.PlatformJob, rsv: st.Reserve, jobID: st.ModuleJob, counts: st.Counts, fxBase: st.FxBase}
	r.plan = m1ad.Prepare(m1oInputs(items))
	if err := r.plan.Err(); err != nil || len(r.plan.Items) != len(sent) {
		t.Fatalf("Prepare of the rows phase 1 submitted: err %v, %d to send, want %d", err, len(r.plan.Items), len(sent))
	}

	// poll to completion as the platform does: transients within the grace are counted, any other answer is wrong
	r.status, r.polls, r.waited = func() (m1ad.JobStatus, int, time.Duration) {
		js, polls, transient, took := s.m1oWaitResumed(tn, st.ModuleJob, len(sent))
		t.Logf("NOTE B12 p3 resume: the job is complete %v after the restart's first poll (%d poll(s), %d transient Unavailable/Timeout answer(s) tolerated); module 1's Status says %+v", took.Round(100*time.Millisecond), polls, transient, js.Counts)
		return js, polls, took
	}()

	// the module holds the SAME job once: no second job, the rows phase 1 submitted, every work item terminal
	after := s.m1oSnapshot(st.UserID, st.ModuleJob)
	var problems []string
	add := func(p []string) { problems = append(problems, p...) }
	add(prefixed("the module after the restart", m1oBooksProblems(st.Before, after)))
	if after.Terminal != after.Items {
		problems = append(problems, fmt.Sprintf("module 1's work-item table has %d of %d work item(s) terminal although Status says the job is complete", after.Terminal, after.Items))
	}
	if after.ModuleJobs != 1 || after.ModuleRows != len(sent) {
		problems = append(problems, fmt.Sprintf("module 1 holds %d job(s) for the tenant with %d row(s), want exactly one job of %d row(s)", after.ModuleJobs, after.ModuleRows, len(sent)))
	}

	// read every page and settle each as it arrives, then the same comparisons as the end-to-end test
	s.drainAndSettle(r)
	s.noteRun("B12 p3", r)
	add(s.assess(r))

	// a replay of the settle writes nothing
	before := s.mny.state()
	chargedOf := map[string]bool{}
	for _, it := range items {
		chargedOf[it.ref] = it.unit != ""
	}
	for i, page := range r.pages {
		want := 0
		for _, it := range page {
			if chargedOf[it.ItemRef] {
				want++
			}
		}
		again, err := s.mny.store.Settle(s.ctx, money.SettleReq{JobID: r.job, Items: page})
		if err != nil {
			t.Fatalf("settling page %d a second time: %v", i, err)
		}
		if again.Charged != 0 || again.Credits != 0 || again.AlreadySettled != want || len(again.Unmapped) != 0 {
			problems = append(problems, fmt.Sprintf("replaying page %d charged %d for %d credit(s), already-settled %d, unmapped %v; want nothing charged and %d already settled", i, again.Charged, again.Credits, again.AlreadySettled, again.Unmapped, want))
		}
	}
	if after2 := s.mny.state(); after2 != before {
		problems = append(problems, fmt.Sprintf("the replay of the settle changed the database (fingerprint %s -> %s)", before, after2))
	}
	t.Logf("NOTE B12 p3 replay: settling all %d page(s) again charged nothing and left the ledger, reserves, prices, audit log and balances exactly as they were", len(r.pages))

	if len(problems) != 0 {
		t.Fatalf("%d difference(s) after module 1 came back:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}

	// the books close
	tot := m1eOracleTotals(items, r.rsv.Prices)
	rel := s.release(r)
	if rel.Released != r.rsv.Reserved-tot.credits {
		t.Fatalf("the release returned %d, want reserve %d - charged %d = %d", rel.Released, r.rsv.Reserved, tot.credits, r.rsv.Reserved-tot.credits)
	}
	s.mny.requireBal("after the release", st.UserID, "m1", st.Grant-tot.credits, 0)
	if got := s.mny.rows(`SELECT reserved, charged, released FROM platform.reserves WHERE reserve_id = $1`, r.rsv.ReserveID); len(got) != 1 || got[0] != fmt.Sprintf("%d|%d|%d", r.rsv.Reserved, tot.credits, r.rsv.Reserved-tot.credits) {
		t.Fatalf("platform.reserves holds %v, want reserved %d, charged %d, released %d", got, r.rsv.Reserved, tot.credits, r.rsv.Reserved-tot.credits)
	}
	if p := s.viewProblems("at the end of the outage proof"); len(p) != 0 {
		t.Fatalf("%s", strings.Join(p, "\n"))
	}
	fin := s.m1oSnapshot(st.UserID, st.ModuleJob)
	if fin.Charges != int64(tot.charged) || fin.Ledger != st.Before.Ledger+int64(tot.charged)+1 || fin.Reserved != 0 {
		t.Fatalf("the books at the end are %+v, want %d charge row(s), %d ledger rows (the grant, the reserve, the charges, the release) and nothing reserved", fin, tot.charged, st.Before.Ledger+int64(tot.charged)+1)
	}
	n := s.mny.requireReservesMatchLedger("at the end of the outage proof")
	t.Logf("NOTE B12 p3 books: %d charge row(s) for %d credit(s) (one per charged item, none for a repeat, a malformed row or a free unknown), %d free; released %d of the %d reserved; available %d, reserved 0; balance_drift and reserve_overdrawn are empty; %d reserve(s) of the database agree with the ledger", fin.Charges, tot.credits, tot.free, rel.Released, r.rsv.Reserved, st.Grant-tot.credits, n)
	t.Logf("NOTE B12 verdict: the job that was in flight when module 1 stopped completed exactly once (one module job of %d rows), was charged exactly once per charged item, and nothing was settled or invented while the module was down; the control is TestM1Node_E2EBulkSettleThroughAdapter, the same comparisons with the module up", len(sent))
	s.m1oSave(3, st)
}

// asModclientErr is err as the *modclient.Error it is.
func asModclientErr(err error) (*modclient.Error, bool) {
	var me *modclient.Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// TestM1Node_OutageChecksBite: see the file comment of the three phases. It needs no stack and runs in the ordinary run: it shows that the outage assertions can fail, that the rows
// of the job are what the phases expect, and that the calls of phase 2 behave as asserted against a real adapter whose module is not there (a closed local server) and against a fake that
// answers although it should not.
func TestM1Node_OutageChecksBite(t *testing.T) {
	e := &m1nEnv{nodeIP: "198.51.100.3", smtpIP: "198.51.100.2"}
	prices := map[string]int{"m1_own": 1, "m1_cache": 1, "m1_vendor": 2}

	// the rows
	items, names := m1oBuildItems(e)
	plan := m1ad.Prepare(m1oInputs(items))
	if err := plan.Err(); err != nil || len(plan.Items) != m1oSent || len(plan.Malformed) != 1 || plan.Duplicates != 1 || len(names) != m1oSlow {
		t.Fatalf("the outage rows: err %v, %d to send, %d malformed, %d repeat(s), %d slow name(s); want %d, 1, 1 and %d", err, len(plan.Items), len(plan.Malformed), plan.Duplicates, len(names), m1oSent, m1oSlow)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] || !strings.HasSuffix(n, "."+m1oZone) {
			t.Fatalf("the slow name %q is repeated or outside the fixture's zone: each slow row needs a silent name of its own", n)
		}
		seen[n] = true
	}
	if o := m1eOracleTotals(items, prices); o.charged != m1oFast || o.free != len(items)-m1oFast {
		t.Fatalf("the oracle charges %d and frees %d of the outage rows, want %d and %d", o.charged, o.free, m1oFast, len(items)-m1oFast)
	}
	for _, it := range items {
		if it.unit == "?" || it.reaches {
			t.Fatalf("the row %q (%s) is unpriced or reaches the node: the outage rows must need neither", it.ref, it.label)
		}
	}
	if back := m1oItemsFromJSON(m1oItemsToJSON(items)); !reflect.DeepEqual(back, items) {
		t.Fatalf("the rows do not survive the state table: the next phase would judge other rows than the first one submitted")
	}
	if p := m1eCompareSettleList(items, func() []money.SettleItem {
		list, err := m1ad.ToSettleItems(m1eReportedRows(items))
		if err != nil {
			t.Fatal(err)
		}
		return append(list, plan.MalformedSettleItems()...)
	}()); len(p) != 0 {
		t.Fatalf("the control: the adapter's mapping of the rows the outage job must produce does not match the oracle: %v", p)
	}

	// the assertions can fail
	m1oSelfCheck(t)
	for _, c := range []struct {
		name string
		h    m1oHalt
		want string
	}{
		{"a loop that got an answer", m1oHalt{polls: 3, took: 4 * time.Second, answered: true, kinds: map[modclient.Kind]int{}}, "module is not down"},
		{"a loop that gave up at the first error", m1oHalt{polls: 1, took: 10 * time.Millisecond, kinds: map[modclient.Kind]int{modclient.Unavailable: 1}}, "gives up at the first error"},
		{"a loop that outlived its context", m1oHalt{polls: 40, took: 30 * time.Second, kinds: map[modclient.Kind]int{modclient.Unavailable: 40}}, "did not stop by its context"},
		{"a loop with a wrong kind", m1oHalt{polls: 16, took: 4 * time.Second, kinds: map[modclient.Kind]int{modclient.Contract: 3, modclient.Unavailable: 13}}, "kind contract"},
		{"a loop that stopped early by itself", m1oHalt{polls: 2, took: time.Second, kinds: map[modclient.Kind]int{modclient.Unavailable: 2}}, "before its context"},
	} {
		if p := m1oHaltProblems(c.h, 4*time.Second); !m1eContains(p, c.want) {
			t.Fatalf("the halting check on %s said %v, want a line containing %q", c.name, p, c.want)
		}
	}
	if p := m1oHaltProblems(m1oHalt{polls: 16, took: 4 * time.Second, kinds: map[modclient.Kind]int{modclient.Unavailable: 15, modclient.Timeout: 1}}, 4*time.Second); len(p) != 0 {
		t.Fatalf("the control: sixteen typed failures over the whole context are a halted loop, got %v", p)
	}
	for _, c := range []struct {
		name  string
		kind  modclient.Kind
		since time.Duration
		want  string
	}{
		{"an Unavailable at once after the restart", modclient.Unavailable, 3 * time.Second, "transient"},
		{"a Timeout after the restart", modclient.Timeout, 20 * time.Second, "transient"},
		{"an Unavailable after the grace", modclient.Unavailable, 2 * m1oResumeGrace, "never came back"},
		{"a Refused", modclient.Refused, time.Second, "wrong answer"},
		{"a NotFound (the job is gone)", modclient.NotFound, time.Second, "wrong answer"},
		{"a Contract error", modclient.Contract, time.Second, "wrong answer"},
		{"an Internal error", modclient.Internal, time.Second, "wrong answer"},
	} {
		if got := m1oResumeVerdict(&modclient.Error{Kind: c.kind}, c.since); got != c.want {
			t.Fatalf("%s: the resume verdict is %q, want %q", c.name, got, c.want)
		}
	}
	if got := m1oPhaseSentence(false, 0, 2); !strings.Contains(got, "found none") || !strings.Contains(got, "never skips") {
		t.Fatalf("a phase that finds no state must fail with a sentence that says so, got %q", got)
	}
	if got := m1oPhaseSentence(true, 1, 2); !strings.Contains(got, "out of order") {
		t.Fatalf("a phase that finds the state of an earlier phase must say the phases ran out of order, got %q", got)
	}
	if m1oPhaseSentence(true, 1, 1) != "" {
		t.Fatal("the control: the state of phase 1 is what phase 2 needs, and said so")
	}

	// the calls of phase 2 against a real adapter whose module is not there: a local server that has been closed
	jobID := "0b9a2f50-1d3c-4a8e-8f21-6c7d5e4f3a2b"
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	ad, err := m1ad.New(goneURL, config.Secret(randomHex(t, 16)), nil, modclient.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx(t)
	obs := m1oDownCalls(ctx, ad, jobID, plan)
	if len(obs) != 8 {
		t.Fatalf("m1oDownCalls made %d call(s), want 8 (Status, three Results, Drain, Submit, Health, Ready)", len(obs))
	}
	for _, o := range obs {
		if p := m1oCallProblems(o); len(p) != 0 {
			t.Fatalf("a real adapter against a module that is not there: %v (the error: %v)", p, o.err)
		}
		if o.name == "Submit" && (o.again || o.sent) {
			t.Fatalf("a Submit whose connection was refused is reported sent=%t, says the key may be sent again=%t: a request that never left is not an unconfirmed submission", o.sent, o.again)
		}
	}
	if m1oDialClass(goneURL) == "" {
		t.Fatal("a direct connection to a closed server was not reported as failing")
	}
	h := m1oPollUntilHalted(ad, jobID, 1500*time.Millisecond)
	if p := m1oHaltProblems(h, 1500*time.Millisecond); len(p) != 0 {
		t.Fatalf("a polling loop against a module that is not there: %v (%d poll(s), %v, kinds %s)", p, h.polls, h.took, m1oKindCounts(h.kinds))
	}

	// the same calls against a fake that ANSWERS although the module is meant to be down: the assertions must report it
	live := m1eFakeListing(t, jobID, 3)
	liveAd, err := m1ad.New(live.URL, config.Secret(randomHex(t, 16)), nil, modclient.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, o := range m1oDownCalls(ctx, liveAd, jobID, plan) {
		all = append(all, m1oCallProblems(o)...)
	}
	if !m1eContains(all, "Results (the default page): no error") {
		t.Fatalf("a listing that answers while the module is down was not reported: %v", all)
	}
	if !m1eContains(all, "Status: the error is of kind contract") {
		t.Fatalf("a status route that answers something that is not a job was not reported as its kind: %v", all)
	}
}
