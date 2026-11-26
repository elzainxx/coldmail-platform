//go:build harness

package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// jlDelta is what the sink gained between two readings: the messages and the RCPT answers per address.
func jlDelta(before, after m3aView) (map[string]int, map[string]m3hRcpt) {
	counts := map[string]int{}
	for a, n := range after.Counts {
		counts[a] = n - before.Counts[a]
	}
	rcpt := map[string]m3hRcpt{}
	for a, r := range after.Rcpt {
		b := before.Rcpt[a]
		rcpt[a] = m3hRcpt{Accepted: r.Accepted - b.Accepted, Rejected: r.Rejected - b.Rejected, Deferred: r.Deferred - b.Deferred}
	}
	return counts, rcpt
}

// requireChainPreview: the preview of a chained send says what the source's verdicts and the user's choice make of the list, by the owner's rule (jlSelects): every verdict counted, the selected ones pending, the rest
// excluded, nothing metered.
func (s *jlStack) requireChainPreview(what string, src []jlSrc, pv jobs.Preview, include bool) {
	s.t.Helper()
	want := map[string]int{"valid": 0, "invalid": 0, "risky": 0, "unknown": 0}
	selected := 0
	for _, x := range src {
		want[x.State]++
		if jlSelects(x.State, include) {
			selected++
		}
	}
	choice := jobs.UnknownExclude
	if include {
		choice = jobs.UnknownInclude
	}
	for v, n := range want {
		if pv.Verdicts[v] != n {
			s.t.Fatalf("%s: the preview counts %d result(s) of verdict %s, the source job's results hold %d (%v)", what, pv.Verdicts[v], v, n, pv.Verdicts)
		}
	}
	if pv.Rows != len(src) || pv.Selected != selected || pv.Pending != selected || pv.Units != selected || pv.Excluded != len(src)-selected || pv.UnknownChoice != choice || pv.Metered || pv.Credits != 0 {
		s.t.Fatalf("%s: the preview says rows %d selected %d pending %d units %d excluded %d choice %q metered %v credits %d, want rows %d selected, pending and units %d, excluded %d, choice %q, not metered and 0 credits", what,
			pv.Rows, pv.Selected, pv.Pending, pv.Units, pv.Excluded, pv.UnknownChoice, pv.Metered, pv.Credits, len(src), selected, len(src)-selected, choice)
	}
}

// requireNothingStarted: a refused confirm moved nothing: the job is previewed, holds no reserve and no ledger row, has no result, no module 3 job and no job item, the user holds no active module 3 job, and the
// user's module 1 balance is what it was.
func (s *jlStack) requireNothingStarted(e *m3aEnv, tn *m3aTenant, job string, m1Available, m1Reserved int64, when string) {
	s.t.Helper()
	if st := s.status(job); st != jobs.StatusPreviewed {
		s.t.Fatalf("%s: the job is %s, want previewed", when, st)
	}
	if _, _, _, held := s.reserveOf(job); held || s.ledgerRowsOf(job) != 0 {
		s.t.Fatalf("%s: the job holds a reserve or a ledger row", when)
	}
	if n := len(s.results(job)); n != 0 {
		s.t.Fatalf("%s: the job holds %d result(s): a refused confirm writes none (the platform's own results are written by the confirm that succeeds)", when, n)
	}
	if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, job); n != 0 {
		s.t.Fatalf("%s: public.job_items holds %d row(s) of the campaign", when, n)
	}
	if n := e.jobCount(s.t, tn.UserID, ""); n != 0 {
		s.t.Fatalf("%s: module 3 holds %d job(s) of the user", when, n)
	}
	if n := s.count("count the active module 3 jobs of the user", `SELECT count(*) FROM platform.jobs WHERE user_id = $1::uuid AND module_id = 'm3' AND status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')`, tn.UserID); n != 0 {
		s.t.Fatalf("%s: the user holds %d active module 3 job(s): the slot must be free", when, n)
	}
	if av, rv := s.balance(tn.UserID, jobs.ModuleVerify); av != m1Available || rv != m1Reserved {
		s.t.Fatalf("%s: the user's module 1 balance moved to available %d reserved %d from %d and %d", when, av, rv, m1Available, m1Reserved)
	}
}

// TestJobsLive_Chain_ExactlySelectedVerdictsMove (Phase 4, chunk B12 with the REAL module 1, the REAL module 3 and its sink): a real verify job succeeds through the engine, and a module 3 send CHAINED to it
// (CreateReq.SourceJobID: its rows are the source's results by verdict) moves EXACTLY the verdicts the owner's rule selects, item by item at module 3's report and at the sink.
//
// THE RULE (the owner's, written out by hand in jlSelects, not read from the store): valid and risky are sent to, invalid never, unknown only when the user chose to include it (default: exclude). Two chains of the
// same source and the same user, one after the other (the slot): the DEFAULT chain, and the chain that INCLUDES unknown.
//
// THE STEPS, each with its control:
//
//	the rows        every chain row is a module result of the source, in its order, pending exactly when the rule selects its verdict, excluded (reason: the verdict) otherwise; the preview counts every verdict
//	                and says how many are selected (requireChainPreview, jlChainRowsProblems)
//	entitlement     the user has a verified sender but NO entitlement: the first chain that has rows to send is refused at confirm with ErrNoEntitlement, and NOTHING moved (still previewed, no reserve or ledger
//	                row, no result, no job item, no module 3 job, the slot free, the user's module 1 credits as they were); entitled, the same job confirms. (A chain with nothing selected is refused earlier, with
//	                ErrNothingToDo: no row would reach the module, and again nothing is written.)
//	delivery        a chain that confirms is run by the engine to `succeeded`; module 3's report holds exactly the selected rows (jlM3ThreeWay), every excluded row has the platform's own result P-EXC (free, the
//	                verdict as its reason) and is in neither module 3's report nor its job table nor public.job_items, and what the SINK gained during the chain is exactly one message to each selected address and none
//	                to any other (jlSinkDeltaProblems)
//	the sink check  can fail: the including chain's delivery is judged by the DEFAULT selection and must be refused (the addresses of a class the default excludes DID reach the sink)
//	the source      is not changed by any of this: its results (a digest of every column), its status and its ledger charges are the same after the chains as before them, and the chains held no credits
//
// (Since the Phase 4 close-out the verdict classes are ALSO shown with a real verifier node: stage M1N-4c of the full run, tests/harness/m1_node_chain_real_test.go, chains a node-verified source into a real module 2 send. This test covers module 3 with the main stack's module 1.)
//
// WHAT THE REAL MODULE 1 CAN SAY HERE, honestly: the harness's main module 1 has no verifier node and no fixture DNS (the m1node stack has both, in its own control plane, and no module 3): a good address at a domain
// of ours gets whatever the private network's resolver answers, `unknown/dns_error` in every environment this harness has run in, so `valid`, `risky` and `invalid` cannot be produced HERE and the DEFAULT selection of this
// source is EMPTY (nothing is sent: ErrNothingToDo, and the sink must stay empty), while the choice for `unknown` is the whole of what the sink can be shown to move. The test is written over the verdicts the source
// ACTUALLY holds (it reads them and applies the owner's rule to them), so with a module 1 that has the node it would exercise every class unchanged; it FAILS, naming the verdicts it saw, when the source holds no
// `unknown` row (then the user's choice moves nothing and the control cannot bite). It prints the distribution it saw.
func TestJobsLive_Chain_ExactlySelectedVerdictsMove(t *testing.T) {
	ctx := jlCtx(t, 14*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s := newJlStack(t, ctx)
	tn := s.m3User(e, "jlchain", false) // a verified account at the sink, NO entitlement yet
	user := tn.UserID
	s.grant(user, jobs.ModuleVerify, 500)

	// ---- the source: a real verify job of the user, the recipients being the tenant's addresses (so that the sink's view of them is this test's) ----
	list := jlBuildList(jlM3Addrs(tn, 12), 1, 1)
	src, spv, _ := s.start(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	if spv.Units != 12 || spv.Malformed != 1 || spv.Duplicates != 1 {
		t.Fatalf("the source job's preview counts %d unit(s), %d malformed, %d repeat(s), want 12, 1 and 1", spv.Units, spv.Malformed, spv.Duplicates)
	}
	s.tickToStatus(src.ID, jobs.StatusSucceeded, 9*time.Minute, s.m1Diag(user, src.ID))
	if problems := jlM1ThreeWay(s.m1View(user, src.ID, 12)); len(problems) != 0 {
		t.Fatalf("the source verify job: the ledger, the results and module 1's report disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	var srcMod []jlSrc
	var universe []string
	for _, r := range s.results(src.ID) {
		if r.Origin == "module" {
			srcMod = append(srcMod, jlSrc{Ref: r.Ref, Email: r.Email, State: r.State})
			universe = append(universe, r.Email)
		}
	}
	if len(srcMod) != 12 {
		t.Fatalf("the source job holds %d result(s) of module 1, want 12", len(srcMod))
	}
	selDefault, selInclude := jlSelectedSet(srcMod, false), jlSelectedSet(srcMod, true)
	t.Logf("NOTE jl chain source: module 1 said %s for the 12 addresses it was sent; the default selection (valid and risky) holds %d, the selection that includes unknown %d", jlStates(srcMod), len(selDefault), len(selInclude))
	if len(selInclude) == len(selDefault) {
		t.Fatalf("the real module 1 said %s: it produced no `unknown` verdict, so the user's choice for `unknown` moves nothing and the chain cannot show that exactly one more class moves (the verdicts a source needs come from the node of the m1node stack)", jlStates(srcMod))
	}
	srcHash, srcCharges := s.resultsHash(src.ID), len(s.ledgerCharges(src.ID))
	m1Available, m1Reserved := s.balance(user, jobs.ModuleVerify)

	// ---- the two chains, previewed ----
	ja := s.create(user, jobs.CreateReq{Module: jobs.ModuleSendBYO, SourceJobID: src.ID, Content: jlContent("chain, exclude"), Settings: jobs.Settings{UnknownChoice: jobs.UnknownExclude}})
	pa := s.preview(user, ja.ID)
	s.requireChainPreview("the default chain", srcMod, pa, false)
	if p := jlChainRowsProblems(srcMod, s.rows(ja.ID), false); len(p) != 0 {
		t.Fatalf("the default chain's rows are not the source's results by the owner's rule: %v", p)
	}
	jb := s.create(user, jobs.CreateReq{Module: jobs.ModuleSendBYO, SourceJobID: src.ID, Content: jlContent("chain, unknown included"), Settings: jobs.Settings{UnknownChoice: jobs.UnknownInclude}})
	pb := s.preview(user, jb.ID)
	s.requireChainPreview("the chain that includes unknown", srcMod, pb, true)
	if p := jlChainRowsProblems(srcMod, s.rows(jb.ID), true); len(p) != 0 {
		t.Fatalf("the including chain's rows are not the source's results by the owner's rule: %v", p)
	}
	// the control of the row check: the including chain's rows judged by the default choice are refused
	jlWantProblem(t, "the including chain judged by the default choice", "became a chain row in state", jlChainRowsProblems(srcMod, s.rows(jb.ID), false))

	// ---- a chain with nothing selected: nothing would reach the module, nothing is written, the sink stays empty ----
	if len(selDefault) == 0 {
		_, err := s.confirmChoice(user, ja.ID, pa.Fingerprint, jobs.UnknownExclude)
		if !errors.Is(err, jobs.ErrNothingToDo) {
			t.Fatalf("the confirm of a chain that selects nothing answered %v, want ErrNothingToDo", err)
		}
		s.requireNothingStarted(e, tn, ja.ID, m1Available, m1Reserved, "the default chain, nothing selected")
		if n := jlSinkCount(e.sinkView(t), tn.Prefix); n != 0 {
			t.Fatalf("the sink holds %d message(s) to the tenant after a chain that selected nothing", n)
		}
		if _, err := s.store.DiscardDraft(ctx, user, ja.ID); err != nil {
			t.Fatalf("discard the default chain: %v", err)
		}
		t.Logf("NOTE jl chain default: every verdict of the source is one the default excludes (%s): the confirm is refused with ErrNothingToDo, nothing was written and the sink holds nothing", jlStates(srcMod))
	}

	// ---- owner ruling OD-4: the source holds unknown verdicts (the test requires it above), so a confirm that says nothing about them is refused, with nothing started (the control of every confirm below, which carries its choice) ----
	if _, err := s.confirm(user, jb.ID, pb.Fingerprint, nil); !errors.Is(err, jobs.ErrUnknownChoiceRequired) {
		t.Fatalf("the confirm of a chain whose source holds unknown verdicts, with no choice, answered %v, want ErrUnknownChoiceRequired", err)
	}
	s.requireNothingStarted(e, tn, jb.ID, m1Available, m1Reserved, "a chain confirmed with no choice for the unknown verdicts")

	// ---- entitlement: the first chain with rows to send is refused until the user is entitled ----
	first, firstPv := ja, pa
	if len(selDefault) == 0 {
		first, firstPv = jb, pb
	}
	_, err := s.confirmChoice(user, first.ID, firstPv.Fingerprint, firstPv.UnknownChoice)
	if !errors.Is(err, jobs.ErrNoEntitlement) {
		t.Fatalf("the confirm of a chain by a user with no entitlement answered %v, want ErrNoEntitlement (the quote is feasible: the user has a verified sender)", err)
	}
	s.requireNothingStarted(e, tn, first.ID, m1Available, m1Reserved, "a chain by a user with no entitlement")
	s.entitle(e, tn)

	// ---- a chain that confirms is run by the engine and judged item by item ----
	deliver := func(job jobs.Job, pv jobs.Preview, include bool, what string) (before, after m3aView) {
		t.Helper()
		sel := jlSelectedSet(srcMod, include)
		before = e.sinkView(t)
		res := s.confirmChoiceOK(user, job.ID, pv.Fingerprint, pv.UnknownChoice)
		if res.Job.Status != jobs.StatusConfirmed || res.Reserved != 0 || res.Pending != len(sel) {
			t.Fatalf("%s: the confirm answered status %s reserved %d pending %d, want confirmed, nothing reserved, %d pending", what, res.Job.Status, res.Reserved, res.Pending, len(sel))
		}
		// the platform's own results of the excluded rows exist from the confirm, before any module is asked
		if n := s.count("count the excluded results", `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'platform' AND module_state = 'excluded' AND rule = 'P-EXC' AND decision = 'free'`, job.ID); n != int64(len(srcMod)-len(sel)) {
			t.Fatalf("%s: the confirm wrote %d free P-EXC result(s) for the excluded rows, want %d", what, n, len(srcMod)-len(sel))
		}
		s.tickToStatus(job.ID, jobs.StatusSucceeded, 5*time.Minute, func() string { return e.diagnostics(t, tn) })
		view, sink, rows := s.m3View(e, tn, job.ID)
		view.Counts, view.Rcpt = jlDelta(before, sink)
		if p := jlM3ThreeWay(view); len(p) != 0 {
			t.Fatalf("%s: module 3's report, the sink, module 3's own table and the results disagree (%d):\n  %s", what, len(p), strings.Join(p, "\n  "))
		}
		if p := jlChainRowsProblems(srcMod, rows, include); len(p) != 0 {
			t.Fatalf("%s: the chain's rows are not the source's results by the owner's rule: %v", what, p)
		}
		if p := jlSinkDeltaProblems(before.Counts, sink.Counts, sel, universe); len(p) != 0 {
			t.Fatalf("%s: what the sink gained is not exactly the selected addresses (%d problem(s)): %s", what, len(p), strings.Join(p, "; "))
		}
		if len(view.Report) != len(sel) || len(view.Jobs) != len(sel) || len(view.Results) != len(srcMod) {
			t.Fatalf("%s: module 3's report holds %d item(s) and its table %d job(s) and the job %d result(s), want %d, %d and %d", what, len(view.Report), len(view.Jobs), len(view.Results), len(sel), len(sel), len(srcMod))
		}
		if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, job.ID); n != int64(len(sel)) {
			t.Fatalf("%s: public.job_items holds %d row(s) of the chain, want one per selected row (%d): an excluded row is never written for the module", what, n, len(sel))
		}
		if _, _, _, held := s.reserveOf(job.ID); held || s.ledgerRowsOf(job.ID) != 0 {
			t.Fatalf("%s: the chain holds a reserve or a ledger row: module 3 is not metered", what)
		}
		s.requireBooks(what)
		t.Logf("NOTE jl chain %s: %d of %d source rows selected and sent, %d excluded (P-EXC, never at module 3); the sink gained exactly one message to each selected address and none to any other", what, len(sel), len(srcMod), len(srcMod)-len(sel))
		return before, sink
	}
	if len(selDefault) > 0 {
		deliver(ja, pa, false, "the default chain")
	}
	beforeB, afterB := deliver(jb, pb, true, "the chain that includes unknown")

	// ---- the control of the sink check: the including chain's delivery is NOT what the default selection says ----
	jlWantProblem(t, "the default selection's check on what the including chain delivered", "selected=false gained 1", jlSinkDeltaProblems(beforeB.Counts, afterB.Counts, selDefault, universe))
	if len(selInclude) <= len(selDefault) {
		t.Fatal("control setup: the including selection must hold more than the default one")
	}

	// ---- the source and the credits are untouched ----
	if got := s.resultsHash(src.ID); got != srcHash {
		t.Fatalf("the source job's results changed during the chains: %s -> %s", srcHash, got)
	}
	if st := s.status(src.ID); st != jobs.StatusSucceeded {
		t.Fatalf("the source job is %s after the chains, want succeeded", st)
	}
	if got := len(s.ledgerCharges(src.ID)); got != srcCharges {
		t.Fatalf("the source job's ledger charges went from %d to %d", srcCharges, got)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != m1Available || rv != m1Reserved {
		t.Fatalf("the user's module 1 balance moved during the chains to available %d reserved %d from %d and %d: a chained module 3 send holds no credits", av, rv, m1Available, m1Reserved)
	}
	m3aProblems(t, "the books of the tenant's module 3 side", func() []string {
		b := e.books(t, user)
		// the user has module 1 credits (the source job), so only what is of module 3 is judged: no module 3 balance and no module 3 reserve
		var p []string
		if av, rv := s.balance(user, jobs.ModuleSendBYO); av != 0 || rv != 0 {
			p = append(p, fmt.Sprintf("the user holds a module 3 balance (available %d, reserved %d)", av, rv))
		}
		if n := s.count("count the module 3 reserves", `SELECT count(*) FROM platform.reserves WHERE user_id = $1::uuid AND module_id = 'm3'`, user); n != 0 {
			p = append(p, fmt.Sprintf("the platform holds %d module 3 reserve(s) for the user", n))
		}
		if b.Entitlement != "enabled" {
			p = append(p, "the entitlement is "+b.Entitlement)
		}
		return p
	}())
	s.requireBooks("after the chains")
}
