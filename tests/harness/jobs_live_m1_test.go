//go:build harness

package harness

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/suppress"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// TestJobsLive_M1_VerifyJobSettlesItemByItem (Phase 4, chunks B8, B9 and B16, with the REAL module 1): a real tenant with credits creates a verify job of FORTY rows, previews it, confirms it, and the job engine of this
// process (the production registry over the production adapter, a real store on the live database) starts it, observes it, settles it and ends it, against module 1's real control plane and bulk queue.
//
// THE LIST (known by construction): 32 distinct good addresses at four domains of the harness's own, FOUR the platform's address rule refuses (no @, two @, no local part, a domain with no dot), FOUR repeats of
// the first addresses in other spellings (upper case, the very text, spaces, a domain in capitals), and two of the good addresses on the user's suppression list. So 30 rows reach module 1 and 10 are the platform's
// own outcomes (P-08, P-07, P-SUP).
//
// THE PROPERTIES, each read from the three places and compared ITEM BY ITEM (jlM1ThreeWay): the ledger's charge rows, platform.job_results and MODULE 1'S OWN REPORT (its status, and its results listing read with the user's
// derived key, page by page), with the hand-written oracle m1bOracle (not charge.Decide) deciding what each verdict of the report costs. And:
//
//	the key        B16: before the job there is no verify.api_callers row for the user, the confirm writes none, the engine's FIRST submit writes exactly one (platform.provision_m1_caller through the real
//	               tenant.M1CallerEnsurer) and it stays one; module 1 holds the user's job under that key and no other job
//	one job        module 1 holds ONE job for the platform job (its id is platform.jobs.module_job_ref, written by the move to running), with exactly the 30 rows the platform sent: no malformed row, no repeat
//	started once  the job moved to running once (by the engine), to completing once and to succeeded once
//	the books      the reserve held units x rate, charged what the report says, the remainder is 0, and exactly one release row exists when anything was left (none when every row was charged: a release of 0 writes
//	               nothing); platform.balance_drift, reserve_overdrawn and job_settlement_drift are empty
//
// THE CONTROLS: the comparison is run on in-memory COPIES of what it accepted, tampered one way at a time (a result's verdict, a ledger charge, a row of module 1's report) and must report each
// (TestJobsLive_ComparisonCanFail_M1 does the same exhaustively without a stack).
//
// WHAT MODULE 1 CAN SAY HERE. This is the harness's main module 1 (no verifier node, no fixture DNS): a good address at a domain of ours gets whatever the private network's resolver answers (unknown/dns_error with a
// failure origin, or invalid/no_mx if the resolver says NXDOMAIN); valid, risky and invalid-by-proof need the node of the m1node stack (its own control plane, a later chunk). The test prints the distribution it saw
// and the oracle follows the module's own words, as the module 1 settlement test does.
func TestJobsLive_M1_VerifyJobSettlesItemByItem(t *testing.T) {
	ctx := jlCtx(t, 10*time.Minute)
	s := newJlStack(t, ctx)
	user := s.newUser("verify")
	const grant = 1000
	s.grant(user, jobs.ModuleVerify, grant)

	// ---- before: no key ----
	if n := s.callerRows(user); n != 0 {
		t.Fatalf("precondition: module 1's verify.api_callers already holds %d row(s) for a user that has never been served", n)
	}

	// ---- the list, and what the platform must make of it ----
	pending := jlAddrs("jlv"+randomHex(t, 3), 32, 4)
	list := jlBuildList(pending, 4, 4)
	for _, a := range pending[30:] {
		r, err := suppress.Add(ctx, s.pool, user, a, suppress.SourceManual)
		if err != nil || !r.Created {
			t.Fatalf("suppress.Add: created %v, error %v", r.Created, err)
		}
	}
	j := s.create(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	pv := s.preview(user, j.ID)
	if pv.Rows != 40 || pv.Pending != 32 || pv.Malformed != 4 || pv.Duplicates != 4 || pv.Suppressed != 2 || pv.Units != 30 || pv.Excluded != 0 || !pv.Metered {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d suppressed %d units %d excluded %d metered %v, want 40, 32, 4, 4, 2, 30, 0 and metered", pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Suppressed, pv.Units, pv.Excluded, pv.Metered)
	}
	if pv.UnitCost < 1 || pv.Credits != int64(pv.Units)*int64(pv.UnitCost) || pv.Available != grant || pv.Shortfall != 0 {
		t.Fatalf("the preview prices the list at %d credits (rate %d, unit %s) for %d units with %d available and shortfall %d: want units x rate, the grant of %d and no shortfall", pv.Credits, pv.UnitCost, pv.RateUnit, pv.Units, pv.Available, pv.Shortfall, grant)
	}
	res := s.confirmOK(user, j.ID, pv.Fingerprint)
	if res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != 30 || res.Suppressed != 2 {
		t.Fatalf("the confirm answered status %s reserved %d pending %d suppressed %d, want confirmed, %d, 30 and 2", res.Job.Status, res.Reserved, res.Pending, res.Suppressed, pv.Credits)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-pv.Credits || rv != pv.Credits {
		t.Fatalf("after the confirm the balance is available %d reserved %d, want %d and %d", av, rv, grant-pv.Credits, pv.Credits)
	}
	if n := s.callerRows(user); n != 0 {
		t.Fatalf("the confirm wrote %d module 1 caller row(s): the key is registered by the engine's first submit, not by the platform's API", n)
	}

	// ---- the engine's first tick submits the job: the key is registered by it and by nothing else ----
	s.tickToStatus(j.ID, jobs.StatusRunning, 2*time.Minute, s.m1Diag(user, j.ID))
	if n := s.callerRows(user); n != 1 {
		t.Fatalf("after the engine's first submit module 1's verify.api_callers holds %d row(s) for the user, want exactly 1 (platform.provision_m1_caller, once)", n)
	}
	if ref := s.moduleJobRef(j.ID); ref == "" {
		t.Fatal("the job is running but carries no module reference: the move to running must record module 1's own id of the job")
	}
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 8*time.Minute, s.m1Diag(user, j.ID))

	// ---- after: read the three places ----
	final, err := s.store.Get(ctx, user, j.ID)
	if err != nil || final.Status != jobs.StatusSucceeded || final.FinishedAt == nil || final.StartedAt == nil {
		t.Fatalf("the job is %+v (error %v): want succeeded with a start and a finish", final, err)
	}
	ref := s.moduleJobRef(j.ID)
	rows, results, ledger, prices := s.rows(j.ID), s.results(j.ID), s.ledgerCharges(j.ID), s.prices(j.ID)
	report := s.m1Report(user, ref, 30)
	view := jlM1View{Rows: rows, Results: results, Ledger: ledger, Prices: prices, Report: report}

	// the list became what it must be, by construction
	by := map[string]int{}
	for _, r := range rows {
		by[r.State]++
		if r.State == "pending" && !r.Submitted {
			t.Errorf("row %d is pending and was never stamped as handed over", r.Ordinal)
		}
	}
	if by["pending"] != 30 || by["malformed"] != 4 || by["duplicate"] != 4 || by["suppressed"] != 2 || len(rows) != 40 || len(results) != 40 {
		t.Fatalf("the job's rows are %v (%d) and it holds %d results, want pending 30, malformed 4, duplicate 4, suppressed 2 (40) and 40 results", by, len(rows), len(results))
	}
	if n := len(report); n != 30 {
		t.Fatalf("module 1's report holds %d row(s), want the 30 the platform sent", n)
	}

	// (a) the three-way equality, item by item
	table := m1bTable(report)
	if problems := jlM1ThreeWay(view); len(problems) != 0 {
		t.Fatalf("the ledger, the results and module 1's own report disagree (%d):\n  %s\nmodule 1's report and the oracle's decisions:\n%s", len(problems), strings.Join(problems, "\n  "), table)
	}
	// the platform's own classes never reached module 1 and are free: asserted again from module 1's OWN tables
	mrows, mmal, mrep, owner := s.m1ModuleRows(ref)
	if mrows != 30 || mmal != 0 || mrep != 0 || owner != user {
		t.Fatalf("module 1's own tables hold %d row(s) for the job (%d malformed, %d repeats) owned by %q, want the 30 rows the platform sent, no malformed row, no repeat, owned by the user", mrows, mmal, mrep, owner)
	}
	if n := s.count("count module 1's jobs of the user", `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); n != 1 {
		t.Fatalf("module 1 holds %d job(s) for the user, want ONE: the engine submits a job once under the platform job's id as the idempotency key", n)
	}
	if got := s.str("read module 1's own id of its job", `SELECT id::text FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); got != ref {
		t.Fatalf("module 1's one job of the user is %s, the platform job's module_job_ref is %s", got, ref)
	}
	if n := s.callerRows(user); n != 1 {
		t.Fatalf("at the end module 1's verify.api_callers holds %d row(s) for the user, want 1", n)
	}
	var callerID, callerStatus string
	if err := s.admin.QueryRow(ctx, `SELECT caller_id, status FROM verify.api_callers WHERE user_id = $1::uuid`, user).Scan(&callerID, &callerStatus); err != nil || callerID != tenant.M1CallerID(user) || callerStatus != "active" {
		t.Fatalf("the user's module 1 caller row is (%q, %q, error %v), want the user's own caller id, active", callerID, callerStatus, err)
	}

	// (b) the money: what the report implies, by the oracle, against the reserve
	var wantCharged int64
	var charged, free, unmapped int
	for _, m := range report {
		switch d := m1bOracle(m); d.Kind {
		case "charge":
			wantCharged += int64(prices[d.Unit])
			charged++
		case "free":
			free++
		default:
			unmapped++
		}
	}
	reserved, chargedCr, released, ok := s.reserveOf(j.ID)
	left := pv.Credits - wantCharged
	if !ok || reserved != pv.Credits || chargedCr != wantCharged || released != left || reserved-chargedCr-released != 0 {
		t.Fatalf("the reserve is (reserved %d, charged %d, released %d, exists %v), the report implies charged %d of the %d held (remainder %d): want the whole reserve accounted for, remainder 0", reserved, chargedCr, released, ok, wantCharged, pv.Credits, left)
	}
	relRows, relCredits := s.releaseRows(j.ID)
	wantRel := int64(0)
	if left > 0 {
		wantRel = 1
	}
	if relRows != wantRel || relCredits != left {
		t.Fatalf("the job has %d release row(s) for %d credits, want %d row(s) for %d (the remainder; a remainder of 0 writes no row)", relRows, relCredits, wantRel, left)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-wantCharged || rv != 0 {
		t.Fatalf("at the end the balance is available %d reserved %d, want %d and 0 (the grant less what was charged)", av, rv, grant-wantCharged)
	}
	if int64(len(ledger)) != int64(charged) {
		t.Fatalf("the ledger holds %d charge row(s), the report implies %d", len(ledger), charged)
	}
	s.requireBooks("after the verify job")

	// (c) the job's history: one move to each status, by the right actor
	for _, c := range []struct {
		to, by string
	}{{jobs.StatusConfirmed, "user"}, {jobs.StatusRunning, "engine"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusSucceeded, "engine"}} {
		if n, m := s.moves(j.ID, c.to), s.movesBy(j.ID, c.to, c.by); n != 1 || m != 1 {
			t.Errorf("the job moved to %s %d time(s), %d by %s, want exactly once by %s\n%s", c.to, n, m, c.by, c.by, strings.Join(s.events(j.ID), "\n"))
		}
	}

	// ---- the controls: the same comparison on tampered copies of what it just accepted ----
	s.jlM1Controls(t, view)

	// ---- what the run saw ----
	dist := map[string]int{}
	for _, m := range report {
		dist[fmt.Sprintf("%s/%s origin=%s skip=%s cache=%v -> %s", m.Status, m.Reason, orDash(m.FailureOrigin), orDash(m.SkipReason), m.FromCache, m1bOracle(m))]++
	}
	var lines []string
	for k, n := range dist {
		lines = append(lines, fmt.Sprintf("%d x %s", n, k))
	}
	sort.Strings(lines)
	t.Logf("NOTE jl m1: 40 rows (30 sent, 4 malformed P-08, 4 repeats P-07, 2 suppressed P-SUP); module 1 said %s; charged %d (%d credits), free %d, unmapped %d; reserve %d, released %d in %d row(s); key rows 1; module 1 job %s",
		strings.Join(lines, "; "), charged, wantCharged, free, unmapped, pv.Credits, left, relRows, jlShort(ref))
	for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
		t.Logf("NOTE jl m1 report | %s", line)
	}
}

// m1View reads the three places of a finished module 1 job whose list sent `sent` rows to the module: the platform's rows, results, ledger and price snapshot, and module 1's own report with the user's key.
func (s *jlStack) m1View(user, job string, sent int) jlM1View {
	s.t.Helper()
	ref := s.moduleJobRef(job)
	if ref == "" {
		s.t.Fatalf("job %s carries no module reference", jlShort(job))
	}
	return jlM1View{Rows: s.rows(job), Results: s.results(job), Ledger: s.ledgerCharges(job), Prices: s.prices(job), Report: s.m1Report(user, ref, sent)}
}

// resultsHash is a digest of everything a job's results say: two equal values mean the results did not change between two reads.
func (s *jlStack) resultsHash(job string) string {
	s.t.Helper()
	return s.str("digest the results of a job", `SELECT count(*)::text || ':' || COALESCE(md5(string_agg(item_ref::text || '|' || ordinal::text || '|' || origin || '|' || module_state || '|' || COALESCE(reason, '') || '|' || decision || '|' || COALESCE(unit, '') || '|' || rule, ',' ORDER BY item_ref)), '-')
		FROM platform.job_results WHERE job_id = $1::uuid`, job)
}

// m1Diag is what a failed wait of a module 1 job says of module 1's own side.
func (s *jlStack) m1Diag(user, job string) func() string {
	return func() string {
		var id string
		if err := s.admin.QueryRow(s.ctx, `SELECT COALESCE(module_job_ref, '') FROM platform.jobs WHERE id = $1::uuid`, job).Scan(&id); err != nil {
			return "module 1: the job's reference could not be read"
		}
		if id == "" {
			return "module 1: the job carries no module reference yet (the engine has not submitted it)"
		}
		st, err := s.m1For(user).Status(s.ctx, id)
		if err != nil {
			return fmt.Sprintf("module 1: its own status of the job could not be read: %v", err)
		}
		return fmt.Sprintf("module 1's own status of its job: status %q complete %v total %d settled %d pending %d paused %v", st.Status, st.Complete, st.Counts.Total, st.Counts.Settled, st.Counts.Pending, st.ProcessingPaused)
	}
}

// jlM1Controls tampers with copies of the accepted view, one way at a time, and requires jlM1ThreeWay to say so: a result's expected outcome changed, a ledger charge removed (or, when module 1's answers were all
// free, one invented), a row of module 1's report removed, and a result of the platform's own claimed by the module.
func (s *jlStack) jlM1Controls(t *testing.T, v jlM1View) {
	t.Helper()
	if p := jlM1ThreeWay(v); len(p) != 0 {
		t.Fatalf("control setup: the accepted view disagrees with itself: %v", p)
	}
	mustReport := func(name, mention string, w jlM1View) {
		t.Helper()
		p := jlM1ThreeWay(w)
		if len(p) == 0 {
			t.Errorf("control %q: the comparison found nothing wrong with the tampered copy", name)
			return
		}
		if !strings.Contains(strings.Join(p, "\n"), mention) {
			t.Errorf("control %q: the comparison is red but does not say %q:\n  %s", name, mention, strings.Join(p, "\n  "))
		}
	}
	copyView := func() jlM1View {
		w := v
		w.Results = append([]jlResult(nil), v.Results...)
		w.Ledger = append([]m1bCharge(nil), v.Ledger...)
		w.Report = append([]m1ad.Row(nil), v.Report...)
		w.Rows = append([]jlRow(nil), v.Rows...)
		return w
	}
	firstModule := -1
	for i, r := range v.Results {
		if r.Origin == "module" {
			firstModule = i
			break
		}
	}
	if firstModule < 0 {
		t.Fatal("control setup: the job has no result of module 1")
	}
	// a result's expected outcome changed
	w := copyView()
	if w.Results[firstModule].State == "valid" {
		w.Results[firstModule].State = "invalid"
	} else {
		w.Results[firstModule].State = "valid"
	}
	mustReport("a result's verdict changed", "the result says", w)
	// a ledger charge removed, or one invented
	w = copyView()
	if len(w.Ledger) > 0 {
		w.Ledger = w.Ledger[1:]
		mustReport("a ledger charge removed", "holds 0 charge row(s)", w)
	} else {
		w.Ledger = append(w.Ledger, m1bCharge{ItemRef: v.Results[firstModule].Ref, Unit: "m1_own", Cost: 1, Units: 1})
		mustReport("a charge invented for a free item", "but the ledger charged it", w)
	}
	// a row of module 1's own report removed
	w = copyView()
	w.Report = w.Report[1:]
	mustReport("a row of module 1's report removed", "module 1's report holds 0 row(s)", w)
	// a result of the platform's own claimed by the module
	w = copyView()
	for i, r := range w.Results {
		if r.Origin == "platform" {
			w.Results[i].Origin = "module"
			break
		}
	}
	mustReport("a platform result claimed by module 1", "want platform", w)
	// a result's decision changed
	w = copyView()
	if w.Results[firstModule].Decision == "free" {
		w.Results[firstModule].Decision = "charge"
	} else {
		w.Results[firstModule].Decision = "free"
	}
	mustReport("a result's decision changed", "the hand-written oracle says", w)
}
