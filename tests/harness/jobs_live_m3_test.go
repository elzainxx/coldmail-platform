//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// jlM3Addrs are n distinct recipients of the tenant: <its prefix>NN@example.test (the prefix is what the sink's view of the test is filtered by).
func jlM3Addrs(tn *m3aTenant, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d@example.test", tn.Prefix, i+1)
	}
	return out
}

func jlRowsOf(addrs []string) []jobs.RowIn {
	out := make([]jobs.RowIn, len(addrs))
	for i, a := range addrs {
		out[i] = jobs.RowIn{Email: a}
	}
	return out
}

// attachItems turns the pending rows of a job into the m3aItems the module 3 adapter tests' comparisons read (the id the module knows the item by is the row's id: job_rows.id = public.job_items.id), the kind
// of each from its address. It replaces tn.Items.
func (s *jlStack) attachItems(tn *m3aTenant, rows []jlRow, kindOf func(addr string) string) {
	s.t.Helper()
	tn.Items = nil
	for _, r := range rows {
		if r.State != "pending" {
			continue
		}
		tn.Items = append(tn.Items, &m3aItem{Index: len(tn.Items) + 1, ID: r.ID, Email: r.Norm, Kind: kindOf(r.Norm)})
	}
}

// m3JobsOfCampaign is module 3's OWN job table for the campaign (the job's id): job_item_id -> state. The superuser reads it; a user's earlier campaigns are not part of it.
func (s *jlStack) m3JobsOfCampaign(e *m3aEnv, tn *m3aTenant, campaign string) map[string]string {
	s.t.Helper()
	q, err := e.db.Query(s.ctx, `SELECT job_item_id::text, state::text FROM send_byo.jobs WHERE user_id = $1::uuid AND campaign_id = $2::uuid`, tn.UserID, campaign)
	if err != nil {
		s.t.Fatalf("read module 3's own jobs of the campaign: %v", err)
	}
	defer q.Close()
	out := map[string]string{}
	for q.Next() {
		var item, state string
		if err := q.Scan(&item, &state); err != nil {
			s.t.Fatal(err)
		}
		out[item] = state
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// jlSinkCount is how many messages the sink holds to addresses that start with the prefix (the sink view is already limited to this test's run).
func jlSinkCount(v m3aView, prefix string) int {
	n := 0
	for a, c := range v.Counts {
		if strings.HasPrefix(a, prefix) {
			n += c
		}
	}
	return n
}

// m3View reads everything module 3 and the sink say of the tenant's campaign (the job's id) and assembles the three-way view with the platform's rows, results and ledger. rep is the campaign report read through
// the adapter, every page.
func (s *jlStack) m3View(e *m3aEnv, tn *m3aTenant, job string) (jlM3View, m3aView, []jlRow) {
	s.t.Helper()
	rep, err := tn.C.ReportCampaign(s.ctx, job)
	if err != nil {
		s.t.Fatalf("ReportCampaign of the job through the adapter failed: %s", e.raw.scrub(err.Error()))
	}
	sink := e.sinkView(s.t)
	e.requireNoDups(s.t, sink)
	own := s.m3JobsOfCampaign(e, tn, job)
	rows := s.rows(job)
	// the sink holds the messages of every tenant of the test run: the view of THIS job is the addresses of this tenant (another tenant's message is not an address this job's rows own, and it is not this job's to answer for)
	counts, rcpt := map[string]int{}, map[string]m3hRcpt{}
	for a, n := range sink.Counts {
		if strings.HasPrefix(a, tn.Prefix) {
			counts[a] = n
		}
	}
	for a, r := range sink.Rcpt {
		if strings.HasPrefix(a, tn.Prefix) {
			rcpt[a] = r
		}
	}
	return jlM3View{
		Rows: rows, Results: s.results(job), Report: rep.Items, Jobs: own, Counts: counts, Rcpt: rcpt, Ledger: int(s.ledgerRowsOf(job)),
	}, sink, rows
}

// TestJobsLive_M3_SendJobSettlesItemByItem (Phase 4, chunks B8 and B9, with the REAL module 3, its workers and its sink): a user with an entitlement, a verified sender and three lanes sends a job of FOURTEEN rows with a
// subject and a body. The engine of this process writes public.job_items (EnsureModuleItems), runs each row at module 3 (Submit), reads the campaign report (Observe) and settles it.
//
// THE LIST (known by construction): ten good recipients (two of them answered 550 at RCPT by the controllable sink, so module 3 reports them bounced and never retries), three addresses the platform's rule refuses (no @,
// two @, a domain with no dot) and one REPEAT of the first recipient in capitals. So ten rows reach module 3 and four are the platform's own (P-08 x3, P-07).
//
// THE PROPERTIES (jlM3ThreeWay, and the module 3 adapter tests' own comparisons m3aCompareReport and m3aCompareSink on the same data):
//
//	the report     module 3's campaign report holds exactly the ten pending rows, each once, in the state, error class and provider text of its kind (sent, or bounced with 550), each from one of the user's accounts
//	the results    platform.job_results holds a result per row, 14: the ten of origin module carry the report's state and are free (M3-99, no unit), the four of the platform are P-08 and P-07 and free
//	the sink       one message and one accepted RCPT for each sent recipient (the repeat of the first recipient made no second message: the sink holds ONE), a rejected RCPT and no message for each bounced one, and nothing for
//	               anything else; module 3's own job table and public.job_items hold exactly the ten rows, no row of the platform's classes
//	the books      module 3 is not metered: no ledger row, no balance, no reserve; platform.job_settlement_drift and balance_drift are empty
//
// THE CONTROLS: the comparison is run on in-memory copies tampered one way at a time (a result's state, a second message for a repeat, a sent item with no message, a bounced one reported sent, a message to an
// address nobody owns, a ledger row, an item of the platform's class in the report); each must be reported (TestJobsLive_ComparisonCanFail_M3 does the same exhaustively without a stack).
func TestJobsLive_M3_SendJobSettlesItemByItem(t *testing.T) {
	ctx := jlCtx(t, 9*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s := newJlStack(t, ctx)
	tn := s.m3User(e, "jlsend", true)

	a := jlM3Addrs(tn, 10)
	bounce := map[string]bool{a[3]: true, a[7]: true}
	list := []jobs.RowIn{
		{Email: a[0]}, {Email: a[1]}, {Email: "no-at-sign.example"}, {Email: a[2]}, {Email: strings.ToUpper(a[0])}, {Email: a[3]}, {Email: "two@@signs.example"},
		{Email: a[4]}, {Email: a[5]}, {Email: "dotless@nodot"}, {Email: a[6]}, {Email: a[7]}, {Email: a[8]}, {Email: a[9]},
	}
	e.setMode(t, m3hMode{RejectRcpt: []string{a[3], a[7]}, RejectCode: 550})

	j := s.create(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: list, Content: jlContent("send settles item by item")})
	pv := s.preview(tn.UserID, j.ID)
	if pv.Rows != 14 || pv.Pending != 10 || pv.Malformed != 3 || pv.Duplicates != 1 || pv.Units != 10 || pv.Metered || pv.Credits != 0 {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v credits %d, want 14, 10, 3, 1, 10, not metered and 0 credits", pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, pv.Credits)
	}
	res := s.confirmOK(tn.UserID, j.ID, pv.Fingerprint)
	if res.Job.Status != jobs.StatusConfirmed || res.Reserved != 0 || res.Pending != 10 || res.Suppressed != 0 {
		t.Fatalf("the confirm answered status %s reserved %d pending %d suppressed %d, want confirmed, 0 (module 3 holds no credits), 10 and 0", res.Job.Status, res.Reserved, res.Pending, res.Suppressed)
	}
	// nothing is at module 3 before the engine's first tick
	if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); n != 0 {
		t.Fatalf("public.job_items holds %d row(s) of the campaign before the engine ran: the confirm must not write the module's rows", n)
	}
	if n := e.jobCount(t, tn.UserID, ""); n != 0 {
		t.Fatalf("module 3 holds %d job(s) of the user before the engine ran", n)
	}

	s.tickToStatus(j.ID, jobs.StatusSucceeded, 4*time.Minute, func() string { return e.diagnostics(t, tn) })

	// ---- the three places ----
	view, sink, rows := s.m3View(e, tn, j.ID)
	s.attachItems(tn, rows, func(addr string) string {
		if bounce[addr] {
			return "bounce"
		}
		return "ok"
	})
	if len(tn.Items) != 10 {
		t.Fatalf("the job has %d pending row(s), want 10", len(tn.Items))
	}
	if problems := jlM3ThreeWay(view); len(problems) != 0 {
		t.Fatalf("module 3's report, the sink, module 3's own table and the job's results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	// The adapter tests' comparisons take the report in the order the tenant's items were created. Module 3 lists a campaign's items by creation time and id; the platform inserts a batch of them in ONE statement (one creation time) with
	// random ids, so the report's order is the ids' and not the rows': the tenant's items are put in the report's order for the comparison, which still fails on a missing, an unexpected or a repeated item.
	ordered := *tn
	pos := map[string]int{}
	for i, it := range view.Report {
		pos[it.JobItemID] = i
	}
	ordered.Items = append([]*m3aItem(nil), tn.Items...)
	slices.SortStableFunc(ordered.Items, func(a, b *m3aItem) int { return pos[a.ID] - pos[b.ID] })
	m3aProblems(t, "the report against what was submitted (the adapter tests' comparison)", m3aCompareReport(&ordered, view.Report))
	m3aProblems(t, "the sink against the report (the adapter tests' comparison)", m3aCompareSink(&ordered, view.Report, sink, 0))

	// the platform's classes never reached module 3, and the module holds exactly the pending rows
	byState := map[string]int{}
	for _, r := range rows {
		byState[r.State]++
	}
	if byState["pending"] != 10 || byState["malformed"] != 3 || byState["duplicate"] != 1 || len(view.Results) != 14 {
		t.Fatalf("the job's rows are %v and it holds %d results, want pending 10, malformed 3, duplicate 1 and 14 results", byState, len(view.Results))
	}
	var itemIDs []string
	q, err := s.admin.Query(ctx, `SELECT id::text FROM public.job_items WHERE campaign_id = $1::uuid ORDER BY id`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for q.Next() {
		var id string
		if err := q.Scan(&id); err != nil {
			t.Fatal(err)
		}
		itemIDs = append(itemIDs, id)
	}
	q.Close()
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(itemIDs)
	if want := m3aSorted(tn.ids()); !slices.Equal(want, itemIDs) {
		t.Fatalf("public.job_items holds %d row(s) for the campaign, the pending rows are %d: want exactly the pending rows, none for malformed or a repeat", len(itemIDs), len(want))
	}
	if got := len(e.jobsOf(t, tn.UserID)); got != 10 {
		t.Fatalf("module 3 holds %d job(s) of the user, want 10", got)
	}
	if got := sink.of(tn); got != 8 {
		t.Fatalf("the sink holds %d message(s) to the tenant, want 8: ten recipients less the two the sink rejected, and none for the repeat", got)
	}
	var sent, bounced int
	for _, it := range view.Report {
		switch it.State {
		case "sent":
			sent++
		case "bounced":
			bounced++
		}
	}
	if sent != 8 || bounced != 2 {
		t.Fatalf("module 3 reports %d sent and %d bounced, want 8 and 2", sent, bounced)
	}

	// the money side: nothing, whatever the state of the entitlement
	if _, _, _, held := s.reserveOf(j.ID); held {
		t.Fatalf("the job holds a reserve: module 3 is not metered")
	}
	if av, rv := s.balance(tn.UserID, jobs.ModuleSendBYO); av != 0 || rv != 0 {
		t.Fatalf("the user holds a module 3 balance (available %d, reserved %d): module 3 holds no credits", av, rv)
	}
	m3aProblems(t, "the books of the user", m3aCompareBooks(e.books(t, tn.UserID)))
	s.requireBooks("after the module 3 send job")
	e.requireBooks(t, "after the module 3 send job (the adapter tests' check)")
	for _, to := range []string{jobs.StatusConfirmed, jobs.StatusRunning, jobs.StatusCompleting, jobs.StatusSucceeded} {
		if n := s.moves(j.ID, to); n != 1 {
			t.Errorf("the job moved to %s %d time(s), want exactly once\n%s", to, n, strings.Join(s.events(j.ID), "\n"))
		}
	}

	// ---- the controls ----
	s.jlM3Controls(t, view, rows)
	t.Logf("NOTE jl m3 send: 14 rows (10 run at module 3, 3 malformed P-08, 1 repeat P-07); module 3 reports %d sent and %d bounced; the sink holds %d message(s) to the tenant, each recipient at most once; results 14; ledger rows %d", sent, bounced, sink.of(tn), view.Ledger)
}

// jlM3Controls tampers with copies of the accepted view and requires jlM3ThreeWay to say so.
func (s *jlStack) jlM3Controls(t *testing.T, v jlM3View, rows []jlRow) {
	t.Helper()
	if p := jlM3ThreeWay(v); len(p) != 0 {
		t.Fatalf("control setup: the accepted view disagrees with itself: %v", p)
	}
	clone := func() jlM3View {
		w := v
		w.Results = append([]jlResult(nil), v.Results...)
		w.Report = append(v.Report[:0:0], v.Report...)
		w.Jobs = map[string]string{}
		for k, x := range v.Jobs {
			w.Jobs[k] = x
		}
		w.Counts = map[string]int{}
		for k, x := range v.Counts {
			w.Counts[k] = x
		}
		w.Rcpt = map[string]m3hRcpt{}
		for k, x := range v.Rcpt {
			w.Rcpt[k] = x
		}
		return w
	}
	var sentRow, bouncedRow, platformRow jlRow
	state := map[string]string{}
	for _, it := range v.Report {
		state[it.JobItemID] = it.State
	}
	for _, r := range rows {
		switch {
		case r.State == "pending" && state[r.ID] == "sent" && sentRow.ID == "":
			sentRow = r
		case r.State == "pending" && state[r.ID] == "bounced" && bouncedRow.ID == "":
			bouncedRow = r
		case r.State == "duplicate" && platformRow.ID == "":
			platformRow = r
		}
	}
	if sentRow.ID == "" || bouncedRow.ID == "" || platformRow.ID == "" {
		t.Fatal("control setup: the job needs a sent row, a bounced row and a repeat")
	}
	must := func(name, mention string, w jlM3View) {
		t.Helper()
		p := jlM3ThreeWay(w)
		if len(p) == 0 {
			t.Errorf("control %q: the comparison found nothing wrong with the tampered copy", name)
			return
		}
		if !strings.Contains(strings.Join(p, "\n"), mention) {
			t.Errorf("control %q: the comparison is red but does not say %q:\n  %s", name, mention, strings.Join(p, "\n  "))
		}
	}
	w := clone()
	for i := range w.Results {
		if w.Results[i].Ref == sentRow.ID {
			w.Results[i].State = "bounced"
		}
	}
	must("a result's state changed", "module 3's report says sent", w)
	w = clone()
	w.Counts[sentRow.Norm] = 2
	must("a second message to a recipient (a repeat that was sent)", "the sink holds 2 message(s)", w)
	w = clone()
	w.Counts[sentRow.Norm] = 0
	must("a sent item the sink never saw", "the sink holds 0 message(s)", w)
	w = clone()
	for i := range w.Report {
		if w.Report[i].JobItemID == bouncedRow.ID {
			w.Report[i].State = "sent"
		}
	}
	must("a bounced item reported sent", "module 3's own job table says", w)
	w = clone()
	w.Counts["m3a-nobody-owns-this-address@example.test"] = 1
	must("a message to an address no pending row owns", "no pending row of the job owns", w)
	w = clone()
	w.Ledger = 1
	must("a ledger row of a module 3 job", "module 3 is not metered", w)
	w = clone()
	w.Report = append(w.Report, v.Report[0])
	w.Report[len(w.Report)-1].JobItemID = platformRow.ID
	must("an item of a repeat in module 3's report", "the platform ended it and never ran it", w)
	w = clone()
	w.Jobs[platformRow.ID] = "queued"
	must("a module 3 job for a repeat", "it never reached the module", w)
}

// TestJobsLive_M3_ConfirmChecksEntitlementAndFeasibilityAgainstTheModule (Phase 4, chunk B6 against the REAL module 3): the confirm of a module 3 job asks module 3's own quote, through jobs.PreChecks over the REAL adapter
// (jlM3PreChecks, a line-for-line copy of internal/app's m3PreChecks, which is unexported), and the platform's entitlement, and holds NOTHING when either says no. One user, one job of six rows, a verified account,
// four steps, each refused or accepted for the reason it names:
//
//	infeasible    module 3's dial max_queued_jobs lowered to 5 for one step (restored at once): the quote of six recipients is infeasible with the cause `queue_cap` and the confirm is ErrInfeasible carrying exactly that word;
//	              then the user suspended through the platform's own admin client: the cause is `suspended`. The user has NO entitlement, and the answer is still ErrInfeasible: the module is asked first (the order of
//	              Confirm), which is also what shows the entitlement check is not what refused
//	no entitlement the causes cleared, the SAME job: the quote is feasible and the confirm is ErrNoEntitlement
//	feasible      the user entitled (the entitlements store, delivered to module 3 by the platform service): the SAME job confirms and holds the slot
//	cancelled     cancelled before it started: free, every row a P-01 result, the slot free again
//
// After each refusal: the job is still previewed, nothing is held (no reserve, no balance, no ledger row of the job), the slot is free (no active job of the user), and module 3 holds no job and no job item exists: the
// refusals happened BEFORE anything moved. The control of every refusal is the step after it: the same job, the cause cleared, goes one step further.
func TestJobsLive_M3_ConfirmChecksEntitlementAndFeasibilityAgainstTheModule(t *testing.T) {
	ctx := jlCtx(t, 8*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s := newJlStack(t, ctx)
	tn := s.m3User(e, "jlconf", false) // a verified account, three lanes, and NO entitlement

	j := s.create(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf(jlM3Addrs(tn, 6)), Content: jlContent("confirm checks")})
	pv := s.preview(tn.UserID, j.ID)
	if pv.Units != 6 || pv.Metered {
		t.Fatalf("the preview counts %d unit(s), metered %v, want 6 and not metered", pv.Units, pv.Metered)
	}
	activeJobs := func() int64 {
		return s.count("count the active jobs of the user", `SELECT count(*) FROM platform.jobs WHERE user_id = $1::uuid AND status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')`, tn.UserID)
	}
	requireNothingHeld := func(when string) {
		t.Helper()
		if st := s.status(j.ID); st != jobs.StatusPreviewed {
			t.Fatalf("%s: the job is %s, want previewed: a refused confirm moves nothing", when, st)
		}
		if _, _, _, held := s.reserveOf(j.ID); held {
			t.Fatalf("%s: the job holds a reserve", when)
		}
		if n := s.ledgerRowsOf(j.ID); n != 0 {
			t.Fatalf("%s: the ledger holds %d row(s) of the job", when, n)
		}
		if av, rv := s.balance(tn.UserID, jobs.ModuleSendBYO); av != 0 || rv != 0 {
			t.Fatalf("%s: the user's module 3 balance is available %d reserved %d, want none", when, av, rv)
		}
		if n := activeJobs(); n != 0 {
			t.Fatalf("%s: the user holds %d active job(s): the slot must be free", when, n)
		}
		if n := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); n != 0 {
			t.Fatalf("%s: public.job_items holds %d row(s) of the campaign", when, n)
		}
		if n := e.jobCount(t, tn.UserID, ""); n != 0 {
			t.Fatalf("%s: module 3 holds %d job(s) of the user", when, n)
		}
		s.requireBooks(when)
	}
	requireInfeasible := func(step string, want ...string) {
		t.Helper()
		_, err := s.confirm(tn.UserID, j.ID, pv.Fingerprint, nil)
		var inf jobs.ErrInfeasible
		if !errors.As(err, &inf) || !slices.Equal(inf.Causes, want) || errors.Is(err, jobs.ErrNoEntitlement) {
			t.Fatalf("%s: Confirm answered %v, want ErrInfeasible with exactly the causes %v (and not ErrNoEntitlement: the module is asked first)", step, err, want)
		}
		requireNothingHeld(step)
		t.Logf("NOTE jl confirm %s: refused with the module's own words %v; the job is still previewed, nothing is held, the slot is free", step, inf.Causes)
	}

	// ---- infeasible: a cause module 3 really reports ----
	e.withDial(t, "max_queued_jobs", "5", func() { requireInfeasible("queue_cap", "queue_cap") })
	if _, err := e.adm.SetSuspended(ctx, tn.UserID, true, "jobs live harness: the suspended cause of an infeasible campaign"); err != nil {
		t.Fatalf("suspend the user through the platform's admin client: %v", err)
	}
	unsuspended := false
	unsuspend := func() {
		if unsuspended {
			return
		}
		unsuspended = true
		if _, err := e.adm.SetSuspended(context.WithoutCancel(ctx), tn.UserID, false, ""); err != nil {
			t.Errorf("unsuspend the user: %v", err)
		}
	}
	t.Cleanup(unsuspend)
	requireInfeasible("suspended", "suspended")
	unsuspend()

	// ---- the causes cleared, the same job: feasible, and now the entitlement is what refuses ----
	_, err := s.confirm(tn.UserID, j.ID, pv.Fingerprint, nil)
	var inf jobs.ErrInfeasible
	if !errors.Is(err, jobs.ErrNoEntitlement) || errors.As(err, &inf) {
		t.Fatalf("with the causes cleared Confirm answered %v, want ErrNoEntitlement (the quote is feasible; the user has no entitlement)", err)
	}
	requireNothingHeld("no entitlement")
	t.Logf("NOTE jl confirm no entitlement: the quote is feasible and the confirm is refused with ErrNoEntitlement; nothing is held")

	// ---- entitled: the same job confirms ----
	s.entitle(e, tn)
	res, err := s.confirm(tn.UserID, j.ID, pv.Fingerprint, nil)
	if err != nil || res.Job.Status != jobs.StatusConfirmed || res.Reserved != 0 || res.Pending != 6 {
		t.Fatalf("the entitled user's confirm answered %+v (error %v), want confirmed, nothing reserved, 6 pending", res, err)
	}
	if n := activeJobs(); n != 1 {
		t.Fatalf("the confirmed job holds %d active slot(s), want 1", n)
	}
	// the engine has not run: nothing is at module 3 yet, whatever the confirm said of its quote
	if n := e.jobCount(t, tn.UserID, ""); n != 0 {
		t.Fatalf("module 3 holds %d job(s) after a confirm: only the engine submits", n)
	}

	// ---- cancelled before it started: free, and the slot is free again ----
	cj, err := s.store.CancelBeforeStart(ctx, tn.UserID, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelled || cj.FinishedAt == nil {
		t.Fatalf("CancelBeforeStart answered %+v (error %v), want a cancelled job with a finish time", cj, err)
	}
	results := s.results(j.ID)
	if len(results) != 6 {
		t.Fatalf("the cancelled job holds %d result(s), want 6", len(results))
	}
	for _, r := range results {
		if r.Origin != "platform" || r.State != "cancelled" || r.Decision != "free" || r.Unit != "" || r.Rule != "P-01" {
			t.Fatalf("a result of the cancelled job is %+v, want the platform's own free cancelled result by P-01", r)
		}
	}
	if n := activeJobs(); n != 0 {
		t.Fatalf("after the cancel the user holds %d active job(s), want the slot free", n)
	}
	s.requireBooks("after the confirm checks")
}
