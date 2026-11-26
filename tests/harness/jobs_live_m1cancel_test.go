//go:build harness

package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// jlpcM1Listing is module 1's OWN status of its job and its whole results listing, read with the user's derived key at the module's address (the test's independent reader: nothing here goes through the engine).
func (s *jlStack) jlpcM1Listing(user, moduleJob string) (m1ad.JobStatus, []m1ad.Row) {
	s.t.Helper()
	ad := s.m1For(user)
	st, err := ad.Status(s.ctx, moduleJob)
	if err != nil {
		s.t.Fatalf("module 1's status of its job: %v", err)
	}
	var out []m1ad.Row
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 2000 {
			s.t.Fatalf("module 1's results listing did not end after %d pages", pages)
		}
		rows, next, err := ad.Results(s.ctx, moduleJob, cursor, m1ad.MaxPageLimit)
		if err != nil {
			s.t.Fatalf("module 1's results listing: %v", err)
		}
		out = append(out, rows...)
		if next == "" {
			return st, out
		}
		if next == cursor {
			s.t.Fatalf("module 1's results listing does not advance")
		}
		cursor = next
	}
}

// jlpcM1Start makes a user with credits and starts a verify job of `pending` addresses (and two malformed rows and a repeat) up to running.
func jlpcM1Start(t *testing.T, s *jlStack, who string, pending int) (user string, j jobs.Job, pv jobs.Preview, grant int64, diag func() string) {
	t.Helper()
	user = s.newUser(who)
	grant = int64(pending)*3 + 500
	s.grant(user, jobs.ModuleVerify, grant)
	list := jlBuildList(jlAddrs("jlk"+randomHex(t, 3), pending, 4), 2, 1)
	j, pv, res := s.start(user, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	if pv.Rows != pending+3 || pv.Pending != pending || pv.Malformed != 2 || pv.Duplicates != 1 || pv.Units != pending || !pv.Metered || pv.UnitCost < 1 || pv.Credits != int64(pending)*int64(pv.UnitCost) || pv.Shortfall != 0 ||
		res.Job.Status != jobs.StatusConfirmed || res.Reserved != pv.Credits || res.Pending != pending {
		t.Fatalf("the preview counts rows %d pending %d malformed %d duplicates %d units %d metered %v credits %d (rate %d) and the confirm answered %s reserved %d pending %d; want %d, %d, 2, 1, %d, metered, units x rate, confirmed, the same credits, %d",
			pv.Rows, pv.Pending, pv.Malformed, pv.Duplicates, pv.Units, pv.Metered, pv.Credits, pv.UnitCost, res.Job.Status, res.Reserved, res.Pending, pending+3, pending, pending, pending)
	}
	diag = s.m1Diag(user, j.ID)
	s.tickToStatus(j.ID, jobs.StatusRunning, 3*time.Minute, diag)
	if s.moduleJobRef(j.ID) == "" {
		t.Fatal("the job is running but carries no module reference")
	}
	return user, j, pv, grant, diag
}

// jlpcM1Money asserts the money of a finished module 1 job against its listing: the reserve was whole, charged what the oracle (which knows a cancelled row) says at the frozen prices, the remainder released ONCE
// (one release row when anything was left, none otherwise), the balance is the grant less what was charged, and the ledger's charge rows are exactly the results that name a unit.
func jlpcM1Money(t *testing.T, s *jlStack, user, job string, pv jobs.Preview, grant int64, view jlM1View) (charged, free int, wantCharged, left int64) {
	t.Helper()
	for _, m := range view.Report {
		switch d := jlpcM1Oracle(m); d.Kind {
		case "charge":
			wantCharged += int64(view.Prices[d.Unit])
			charged++
		case "free":
			free++
		}
	}
	reserved, chargedCr, released, ok := s.reserveOf(job)
	left = pv.Credits - wantCharged
	if !ok || reserved != pv.Credits || chargedCr != wantCharged || released != left || reserved-chargedCr-released != 0 {
		t.Fatalf("the reserve is (reserved %d, charged %d, released %d, exists %v), the listing implies charged %d of the %d held (remainder %d): want the whole reserve accounted for, remainder 0", reserved, chargedCr, released, ok, wantCharged, pv.Credits, left)
	}
	wantRel := int64(0)
	if left > 0 {
		wantRel = 1
	}
	if relRows, relCredits := s.releaseRows(job); relRows != wantRel || relCredits != left {
		t.Fatalf("the job has %d release row(s) for %d credits, want %d row(s) for %d (the remainder, ONCE)", relRows, relCredits, wantRel, left)
	}
	if av, rv := s.balance(user, jobs.ModuleVerify); av != grant-wantCharged || rv != 0 {
		t.Fatalf("at the end the balance is available %d reserved %d, want %d and 0 (the grant less what was charged)", av, rv, grant-wantCharged)
	}
	withUnit := 0
	for _, r := range view.Results {
		if r.Unit != "" {
			withUnit++
		}
	}
	if int64(len(view.Ledger)) != s.chargeRows(job) || len(view.Ledger) != withUnit || len(view.Ledger) != charged {
		t.Fatalf("the ledger holds %d charge row(s) (the job's charge rows read %d), %d result(s) name a unit and the listing implies %d charge(s): want all four equal", len(view.Ledger), s.chargeRows(job), withUnit, charged)
	}
	return charged, free, wantCharged, left
}

// jlRunM1Cancel runs one module 1 verify job of `pending` addresses through the engine and cancels it, as its user, once the engine has recorded its first results. It returns false, with the job run to its end and
// nothing asserted about a cancel, when module 1 had nothing left to cancel (it finished the whole list before the engine first recorded a result, or before the cancel reached it): the caller then tries a longer list.
// It returns true when the cancel landed in the middle of the job and everything below held.
func jlRunM1Cancel(t *testing.T, s *jlStack, tr *jlTrace, pending int) bool {
	t.Helper()
	user, j, pv, grant, diag := jlpcM1Start(t, s, "verify-cancel", pending)
	ref := s.moduleJobRef(j.ID)

	landed := false
	s.tickUntil("the engine has recorded the first results of module 1", 10*time.Minute, 150*time.Millisecond, diag, func() (bool, string) {
		st := s.status(j.ID)
		_, mod := s.resultCount(j.ID)
		if mod >= 1 || st != jobs.StatusRunning {
			landed = st == jobs.StatusRunning && mod >= 1 && mod < int64(pending)
			return true, fmt.Sprintf("status %s, %d result(s) of the module recorded", st, mod)
		}
		return false, fmt.Sprintf("status %s, no result of the module recorded yet", st)
	})
	if !landed {
		t.Logf("NOTE jl m1 cancel: with %d addresses module 1 had settled %d of them (status %s) by the time the engine first recorded any: too fast for a cancel in the middle; the job is run to its end and a longer list is tried", pending, func() int64 { _, m := s.resultCount(j.ID); return m }(), s.status(j.ID))
		s.tickToStatus(j.ID, jobs.StatusSucceeded, 10*time.Minute, diag)
		return false
	}

	// ---- the user cancels the RUNNING job: running -> cancelling, as the user, the request recorded; nothing is told to the module by the store ----
	mark := tr.mark()
	cj, err := s.store.Cancel(s.ctx, user, j.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.CancelRequestedAt == nil {
		t.Fatalf("the user's cancel of the running module 1 job answered %+v (error %v), want cancelling with the request recorded", cj, err)
	}
	if n := tr.since(mark, j.ID, "cancel", ""); n != 0 {
		t.Fatalf("the engine called module 1's cancel %d time(s) before it was ticked: the store must not tell the module", n)
	}
	if n := s.movesBy(j.ID, jobs.StatusCancelling, "user"); n != 1 || s.moves(j.ID, jobs.StatusCancelled) != 0 {
		t.Fatalf("the job moved to cancelling %d time(s) by the user and to cancelled %d time(s), want once and never (the engine ends it)", n, s.moves(j.ID, jobs.StatusCancelled))
	}
	// a second cancel by the user is refused by the lifecycle and changes nothing
	before := s.events(j.ID)
	_, err = s.store.Cancel(s.ctx, user, j.ID)
	if !errors.Is(err, jobs.ErrNotAllowed) {
		t.Fatalf("a second cancel of a cancelling job answered %v, want ErrNotAllowed", err)
	}
	jeNoRawSQL(t, "the second cancel", err)
	if after := s.events(j.ID); fmt.Sprint(after) != fmt.Sprint(before) || s.status(j.ID) != jobs.StatusCancelling {
		t.Fatalf("a refused second cancel changed the job's history or status\nbefore: %v\nafter:  %v", before, after)
	}

	// ---- the engine calls module 1's cancel, records what finished, and ends the job ----
	s.tickToStatus(j.ID, jobs.StatusCancelled, 8*time.Minute, diag)
	before = s.events(j.ID)
	if _, err := s.store.Cancel(s.ctx, user, j.ID); err != nil && !errors.Is(err, jobs.ErrNotAllowed) {
		t.Fatalf("a cancel of an ended job answered %v, want a refusal or a no-op", err)
	}
	if after := s.events(j.ID); fmt.Sprint(after) != fmt.Sprint(before) || s.status(j.ID) != jobs.StatusCancelled {
		t.Fatalf("a cancel of an ended job changed its history or status")
	}
	final, err := s.store.Get(s.ctx, user, j.ID)
	if err != nil || final.Status != jobs.StatusCancelled || final.FinishedAt == nil || final.CancelRequestedAt == nil {
		t.Fatalf("the job is %+v (error %v): want cancelled with a finish time and the cancel request", final, err)
	}

	// ---- module 1's own report ----
	st, listing := s.jlpcM1Listing(user, ref)
	if !st.Complete || st.Totals.Submitted != pending || st.Totals.Pending != 0 || st.Totals.Settled+st.Totals.Cancelled != st.Totals.Submitted || st.Counts.Cancelled != st.Totals.Cancelled || st.Totals.Duplicates != 0 || st.Totals.Malformed != 0 {
		t.Fatalf("module 1's own status of the cancelled job is %+v, want complete with %d row(s) submitted, none pending, submitted = settled + cancelled, no repeat and none malformed", st, pending)
	}
	if st.Totals.Cancelled == 0 {
		t.Logf("NOTE jl m1 cancel: with %d addresses module 1 had settled all of them before the cancel reached it (cancelled 0, the job still ended cancelled: the request was accepted); a longer list is tried", pending)
		return false
	}
	if !st.CancelRequested {
		t.Fatalf("module 1 does not report the cancel as requested (cancel_requested false) although it cancelled %d row(s)", st.Totals.Cancelled)
	}
	if n := tr.since(mark, j.ID, "cancel", "ok"); n < 1 {
		t.Fatalf("the engine made %d successful call(s) to module 1's cancel, want at least one", n)
	}
	if n := tr.since(0, j.ID, "pause", ""); n != 0 {
		t.Fatalf("the engine asked module 1 to pause %d time(s): module 1 has no per-job pause and nothing is cancelled and resubmitted", n)
	}
	if len(listing) != pending {
		t.Fatalf("module 1's listing holds %d row(s), want the %d the platform sent", len(listing), pending)
	}
	seen := map[string]bool{}
	nCancelled, nVerdict := 0, 0
	for _, m := range listing {
		if seen[m.ItemID] {
			t.Fatalf("module 1's listing names an item_id twice")
		}
		seen[m.ItemID] = true
		switch m.Status {
		case m1ad.StatusCancelled:
			nCancelled++
			if m.Reason != m1ad.ReasonCancelledByCaller || m.FromCache || m.SkipReason != "" || m.FailureOrigin != "" || m.DuplicateOf != nil {
				t.Fatalf("a cancelled row of module 1's listing is %+v, want the non-verdict words alone (cancelled / cancelled_by_caller): no cache flag, no skip token, no origin", m)
			}
		case "valid", "invalid", "risky", "unknown":
			nVerdict++
		default:
			t.Fatalf("a row of module 1's listing has the status %q: every row of a cancelled job has exactly one final status, a verdict or cancelled", m.Status)
		}
	}
	if nCancelled != st.Totals.Cancelled || nVerdict != st.Totals.Settled || nCancelled+nVerdict != pending {
		t.Fatalf("module 1's listing holds %d cancelled and %d verdict row(s), its status says cancelled %d settled %d of %d: submitted = settled + cancelled must hold row by row", nCancelled, nVerdict, st.Totals.Cancelled, st.Totals.Settled, pending)
	}

	// ---- the platform's results, the ledger and module 1's listing, item by item ----
	view := jlM1View{Rows: s.rows(j.ID), Results: s.results(j.ID), Ledger: s.ledgerCharges(j.ID), Prices: s.prices(j.ID), Report: listing}
	if problems := jlpcM1CancelThreeWay(view); len(problems) != 0 {
		if len(problems) > 15 {
			problems = append(problems[:15:15], fmt.Sprintf("... and %d more", len(problems)-15))
		}
		t.Fatalf("the ledger, the results and module 1's own listing disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	by := map[string]int{}
	for _, r := range view.Rows {
		by[r.State]++
		if r.State == "pending" && !r.Submitted {
			t.Errorf("row %d is pending and was never stamped as handed over (module 1 takes the whole list in one call)", r.Ordinal)
		}
	}
	if by["pending"] != pending || by["malformed"] != 2 || by["duplicate"] != 1 || len(view.Rows) != pending+3 || len(view.Results) != len(view.Rows) {
		t.Fatalf("the job's rows are %v (%d) and it holds %d result(s), want pending %d, malformed 2, duplicate 1 and a result for every row", by, len(view.Rows), len(view.Results), pending)
	}
	cancelledResults := 0
	for _, r := range view.Results {
		if r.Origin == "module" && r.State == "cancelled" {
			cancelledResults++
			if r.Reason != m1ad.ReasonCancelledByCaller || r.Decision != "free" || r.Unit != "" || r.Rule != "M1-24" {
				t.Errorf("a cancelled row's result is %+v, want module 1's cancelled_by_caller, free, no unit, rule M1-24", r)
			}
		}
	}
	if cancelledResults != st.Totals.Cancelled {
		t.Fatalf("the job holds %d cancelled module result(s), module 1 says it cancelled %d row(s)", cancelledResults, st.Totals.Cancelled)
	}
	charged, free, wantCharged, left := jlpcM1Money(t, s, user, j.ID, pv, grant, view)
	if want := int64(cancelledResults) * int64(pv.UnitCost); left < want {
		t.Fatalf("the remainder released is %d credits, at least the %d the %d cancelled row(s) were reserved at is expected (cancelled rows are free)", left, want, cancelledResults)
	}
	mrows, mmal, mrep, owner := s.m1ModuleRows(ref)
	if mrows != int64(pending) || mmal != 0 || mrep != 0 || owner != user {
		t.Fatalf("module 1's own tables hold %d row(s) for the job (%d malformed, %d repeats) owned by %q, want the %d rows the platform sent, none malformed, none a repeat, owned by the user", mrows, mmal, mrep, owner, pending)
	}
	if n := s.count("count module 1's jobs of the user", `SELECT count(*) FROM verify.jobs WHERE owner_user_id = $1::uuid`, user); n != 1 {
		t.Fatalf("module 1 holds %d job(s) for the user, want ONE: a cancel never resubmits", n)
	}
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCancelling: 1, jobs.StatusCompleting: 1, jobs.StatusCancelled: 1})
	for _, c := range []struct{ to, by string }{{jobs.StatusConfirmed, "user"}, {jobs.StatusRunning, "engine"}, {jobs.StatusCancelling, "user"}, {jobs.StatusCompleting, "engine"}, {jobs.StatusCancelled, "engine"}} {
		if n := s.movesBy(j.ID, c.to, c.by); n != 1 {
			t.Errorf("the job moved to %s %d time(s) by %s, want exactly once by %s\n%s", c.to, n, c.by, c.by, strings.Join(s.events(j.ID), "\n"))
		}
	}
	if n := s.eventKindCount(j.ID, "flag") + s.eventKindCount(j.ID, "stale"); n != 0 {
		t.Errorf("the cancelled job raised %d flag or stale event(s)\n%s", n, strings.Join(s.events(j.ID), "\n"))
	}
	if s.invented(j.ID) != 0 {
		t.Errorf("the cancelled job holds an infrastructure_failure or unmapped result")
	}
	if n := s.callerRows(user); n != 1 {
		t.Errorf("module 1's verify.api_callers holds %d row(s) for the user, want 1", n)
	}
	s.requireBooksScoped("after the cancelled module 1 job", j.ID)
	t.Logf("NOTE jl m1 cancel: %d addresses (+2 malformed P-08, +1 repeat P-07); the user cancelled after the engine recorded the first results: running -> cancelling (user) -> completing -> cancelled (engine); module 1 says submitted %d = settled %d + cancelled %d; cancelled rows free by M1-24 (no unit, no ledger row), the %d settled rows charged %d, free %d (%d credits of the %d held, %d released in ONE release); a second cancel was refused and changed nothing",
		pending, st.Totals.Submitted, st.Totals.Settled, st.Totals.Cancelled, nVerdict, charged, free-cancelledResults, wantCharged, pv.Credits, left)
	return true
}

// jlpcRunM1Control is THE CONTROL of the cancel: the same job (the same list size, a user of its own) NOT cancelled ends `succeeded` with every row settled (module 1 cancelled nothing), the ledger, the results and
// module 1's own listing equal item by item, the remainder released once and no move to cancelling.
func jlpcRunM1Control(t *testing.T, s *jlStack, tr *jlTrace, pending int) {
	t.Helper()
	user, j, pv, grant, diag := jlpcM1Start(t, s, "verify-nocancel", pending)
	ref := s.moduleJobRef(j.ID)
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 12*time.Minute, diag)
	view := s.m1View(user, j.ID, pending)
	if problems := jlM1ThreeWay(view); len(problems) != 0 {
		if len(problems) > 15 {
			problems = append(problems[:15:15], fmt.Sprintf("... and %d more", len(problems)-15))
		}
		t.Fatalf("the control job: the ledger, the results and module 1's report disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	st, _ := s.jlpcM1Listing(user, ref)
	if st.Totals.Cancelled != 0 || st.CancelRequested || st.Totals.Settled != pending || !st.Complete {
		t.Fatalf("the control job's own status is %+v, want complete with all %d row(s) settled and none cancelled: without a cancel every row is verified", st, pending)
	}
	if n := s.count("count the cancelled results of the control", `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND module_state = 'cancelled' AND origin = 'module'`, j.ID); n != 0 {
		t.Fatalf("the control job holds %d cancelled module result(s)", n)
	}
	charged, _, wantCharged, left := jlpcM1Money(t, s, user, j.ID, pv, grant, view)
	if n := tr.since(0, j.ID, "cancel", ""); n != 0 {
		t.Fatalf("the engine called module 1's cancel %d time(s) for a job nobody cancelled", n)
	}
	s.requireMoves(j.ID, map[string]int64{jobs.StatusConfirmed: 1, jobs.StatusRunning: 1, jobs.StatusCompleting: 1, jobs.StatusSucceeded: 1})
	s.requireBooksScoped("after the control module 1 job", j.ID)
	t.Logf("NOTE jl m1 cancel control: the same %d addresses, never cancelled, ended succeeded: module 1 settled all %d, %d charged (%d credits of the %d held, %d released), no cancelled row, no move to cancelling", pending, st.Totals.Settled, charged, wantCharged, pv.Credits, left)
}

// TestJobsLive_M1_CancelARunningJob (Phase 4 close-out D1, live: the engine of this process, the REAL module 1 at its cancel head): a verify job is running and the engine has recorded its first results when its
// user cancels it. THE SIZE IS ADAPTIVE (the ladder of the outage test, jlM1OutageSizes): module 1's speed on a runner is not known, and a list it finishes before the cancel reaches it cannot show a cancel in the
// middle of a job, so that job is run to its end (a good job like any other) and the next list, four times longer, is tried; the test fails, saying so, only when module 1 finishes every size first.
//
// WHAT MUST HOLD (jlRunM1Cancel): the store's cancel answers `cancelling` (the request recorded, by the user, nothing told to module 1 yet) and never `cancelled`; a second cancel by the user is refused by the
// lifecycle and changes nothing, and so does a cancel of the ended job; the engine's next ticks call module 1's POST /v1/jobs/{id}/cancel (a successful call) and never its pause; the job ends `cancelled`, through
// completing, by the engine, once; module 1's own status says complete with submitted = settled + cancelled and nothing pending, and its listing holds every item exactly once with ONE final status (a verdict or
// `cancelled`/`cancelled_by_caller`: the rows it cancelled before their check began, free, and no unit); the platform's results agree item by item (a cancelled row: origin module, state cancelled, free, no unit, rule M1-24,
// no ledger row; every other row as in the ordinary settlement test), the ledger's charge rows equal the results that name a unit, the reserve was charged what the oracle says at the frozen prices and the remainder
// was released ONCE, `job_settlement_drift` is empty for the job, and module 1 holds ONE job for the user (a cancel never resubmits).
//
// THE CONTROL (jlpcRunM1Control): the same list, never cancelled, ends `succeeded` with every item settled and nothing cancelled, three ways equal.
func TestJobsLive_M1_CancelARunningJob(t *testing.T) {
	ctx := jlCtx(t, 30*time.Minute)
	s, tr := newJlStackWith(t, ctx, jlOpts{Cfg: func(sec config.Secret) config.Config { return jlConfig(t, sec) }, SettleEvery: -1})
	size := 0
	for _, n := range jlM1OutageSizes {
		if jlRunM1Cancel(t, s, tr, n) {
			size = n
			break
		}
	}
	if size == 0 {
		t.Fatalf("module 1 had nothing left to cancel for every list it was given (%v addresses): it finished each before the engine recorded a first result or before the cancel reached it; add a longer size to jlM1OutageSizes", jlM1OutageSizes)
	}
	jlpcRunM1Control(t, s, tr, size)
}
