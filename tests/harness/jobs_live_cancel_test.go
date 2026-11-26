//go:build harness

package harness

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// m3Job creates, previews and confirms a send job of the addresses for a user that is entitled, and returns the confirmed job.
func (s *jlStack) m3Job(tn *m3aTenant, addrs []string, what string) jobs.Job {
	s.t.Helper()
	j, pv, res := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf(addrs), Content: jlContent(what)})
	if res.Job.Status != jobs.StatusConfirmed || pv.Units != len(addrs) || res.Pending != len(addrs) {
		s.t.Fatalf("the confirm of the %q job answered status %s, %d unit(s) previewed, %d pending, want confirmed and %d", what, res.Job.Status, pv.Units, res.Pending, len(addrs))
	}
	return j
}

// TestJobsLive_M3_CancelRunningJob (Phase 4, chunk B10, with the REAL module 3, its three workers and the controllable sink; and the module 1 refusal, LIVE).
//
// THE SCENARIO (the one TestM3Adapter_CancelStopsTheCampaign uses, so that what is deterministic here is deterministic there): user K sends ten rows with ONE lane held for 60 s after every push and the sink holding
// every send in DATA for 6 s. The engine of this process submits the ten rows; module 3 hands exactly one item to a worker, the sink holds it, and the other nine stay queued. K then cancels the RUNNING job through the
// store (`running -> cancelling`, as the user: the database accepts it because module 3 can cancel), and the engine, at its next tick, calls module 3's cancel (POST /v1/cancel, repeated at every tick of a cancelling job),
// records what the module reports, and ends the job when the module says every item is final.
//
// WHAT MUST HOLD, each from the three places (jlM3ThreeWay: module 3's report and its own job table, platform.job_results, the sink's counters per recipient, item by item):
//
//	the partition   at least one item is cancelled (a queued one) and at least one was in flight and finished (sent), and the two make ten; the job ends `cancelled`, the cancel request recorded, finished once
//	reported once   every item has ONE result: the cancelled ones free by the table's row M3-01 with no unit, the sent one free by M3-99; the sink holds ONE message for the sent item and nothing for a cancelled one
//	nothing new     no cancelled item ever reaches the sink (no RCPT, no message), and the sink does not change AT ALL for longer than three of module 3's dispatch ticks after the job ended: nothing is
//	                sent after the module reported complete
//	the moves       the user moved the job to cancelling once, the engine to completing once and to cancelled once; no `flag` and no `stale` event; the books of module 3 gain nothing
//
// THE CONTROL (the same job NOT cancelled): user C has the same shape (ten rows, one lane held for 60 s); while the lane holds, nine of the ten are still queued and nothing is cancelled; the lane is then released and the
// job ends `succeeded` with EVERY row sent, ten messages at the sink, ten results free by M3-99. So the nine cancellations are the cancel's doing, and the sink check is shown to see messages.
//
// MODULE 1 HAS NO PER-JOB PAUSE (the data says so: platform.modules.can_pause is false for m1; can_cancel is TRUE for every module since platform_0016): a running module 1 job's pause is refused BY THE DATABASE
// (ErrNotAllowed, constraint job_module_cannot_pause, which the API words `pause_not_supported`), typed with no raw database error; nothing moved (still running, no `paused` move); and the job goes on to
// `succeeded`, settled three ways like any other.
//
// LIVE-REWRITE (for the agent that writes the live tests against the re-pinned modules): this test used to assert that module 1 cannot be cancelled. It no longer does, and it does NOT cancel the module 1 job
// either: module 1's own cancel (POST /v1/jobs/{id}/cancel) exists only at the NEW module 1 head, so the live story of a cancelled module 1 job (unclaimed rows free by M1-24, in-flight probes charged,
// ledger = results = report) is the later agent's, with the recorded module. What is left here is the part that still holds: no per-job pause, typed, nothing moved.
func TestJobsLive_M3_CancelRunningJob(t *testing.T) {
	ctx := jlCtx(t, 14*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s := newJlStack(t, ctx)
	const n = 10
	k := s.m3User(e, "jlcxl", true)
	c := s.m3User(e, "jlctl", true)
	e.setPacing(t, k, 1, 60, 60)
	e.setPacing(t, c, 1, 60, 60)
	diagK := func() string { return e.diagnostics(t, k) }
	diagC := func() string { return e.diagnostics(t, c) }

	// ---- K: ten rows, one handed to a worker and held by the sink ----
	kj := s.m3Job(k, jlM3Addrs(k, n), "cancel")
	e.setMode(t, m3hMode{DataDelayMs: 6000})
	s.tickToStatus(kj.ID, jobs.StatusRunning, 2*time.Minute, diagK)
	s.tickUntil("module 3 hands one item to a worker and the sink holds its DATA", 60*time.Second, 300*time.Millisecond, diagK, func() (bool, string) {
		inf := e.inflight(t)
		return inf.Inflight >= 1, fmt.Sprintf("the sink holds %d session(s)", inf.Inflight)
	})
	before, err := k.C.ReportCampaign(ctx, kj.ID)
	if err != nil {
		t.Fatalf("ReportCampaign before the cancel failed: %s", e.raw.scrub(err.Error()))
	}
	// one lane: at most one item may be in flight and one may already have been answered (the report counts an item sent once its DATA is accepted, which is before the sink's held final reply); what the cancel needs is that
	// items are still QUEUED at module 3, none cancelled yet, and that the whole list is there
	if before.Totals.Total != n || before.Totals.Cancelled != 0 || before.Totals.Pending+before.Totals.WaitingForCapacity < 1 || before.Totals.Sent+before.Totals.InFlight > 2 || e.jobCount(t, k.UserID, "cancelled") != 0 {
		t.Fatalf("before the cancel module 3's totals are (%s), want %d items, none cancelled, at least one still queued and at most two sent or in flight (one lane)", m3aTotalsLine(before.Totals), n)
	}

	// ---- the user cancels the RUNNING job ----
	cj, err := s.store.CancelBeforeStart(ctx, k.UserID, kj.ID)
	if err != nil || cj.Status != jobs.StatusCancelling || cj.CancelRequestedAt == nil {
		t.Fatalf("the user's cancel of the running module 3 job answered %+v (error %v), want cancelling with the request recorded: the database accepts it because module 3 can cancel", cj, err)
	}
	s.tickToStatus(kj.ID, jobs.StatusCancelled, 90*time.Second, diagK)
	endedAt := time.Now()
	atEnd := e.sinkView(t)

	// ---- nothing new reaches the sink: for longer than three dispatch ticks after the job ended ----
	window := 3 * time.Duration(e.dialInt(t, "dispatch_tick_interval_ms")) * time.Millisecond
	if window < 5*time.Second {
		window = 5 * time.Second
	}
	for time.Since(endedAt) < window {
		s.eng.Tick(ctx) // an ended job is none of the engine's work: a tick must not resubmit, recancel or settle it again
		v := e.sinkView(t)
		if !slices.Equal(jlSortedCounts(v.Counts), jlSortedCounts(atEnd.Counts)) || fmt.Sprint(v.Rcpt) != fmt.Sprint(atEnd.Rcpt) {
			t.Fatalf("the sink changed after the job ended and the module reported complete: %s -> %s", jlSortedCounts(atEnd.Counts), jlSortedCounts(v.Counts))
		}
		time.Sleep(500 * time.Millisecond)
	}

	// ---- the three places ----
	view, sink, rows := s.m3View(e, k, kj.ID)
	if problems := jlM3ThreeWay(view); len(problems) != 0 {
		t.Fatalf("module 3's report, the sink, module 3's own table and the job's results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	var cancelled, sent []string
	for _, it := range view.Report {
		switch it.State {
		case "cancelled":
			cancelled = append(cancelled, it.JobItemID)
		case "sent":
			sent = append(sent, it.JobItemID)
		default:
			t.Fatalf("an item is reported %s after the cancel: want cancelled or sent", it.State)
		}
	}
	if len(cancelled) < 1 || len(sent) < 1 || len(cancelled)+len(sent) != n {
		t.Fatalf("the partition is %d cancelled and %d sent, want at least one of each and %d in all: a queued item was cancelled and the one the sink held finished", len(cancelled), len(sent), n)
	}
	if got := sink.of(&m3aTenant{Items: itemsOf(rows)}); got != len(sent) {
		t.Fatalf("the sink holds %d message(s) to K, want %d (one per item that was sent, none for the %d cancelled)", got, len(sent), len(cancelled))
	}
	if got := jlSinkCount(sink, k.Prefix); got != len(sent) {
		t.Fatalf("the sink holds %d message(s) with K's prefix, want %d", got, len(sent))
	}
	finalK, err := s.store.Get(ctx, k.UserID, kj.ID)
	if err != nil || finalK.Status != jobs.StatusCancelled || finalK.FinishedAt == nil || finalK.CancelRequestedAt == nil {
		t.Fatalf("K's job is %+v (error %v), want cancelled with a finish time and the cancel request", finalK, err)
	}
	for _, c := range []struct {
		to, by string
		n      int64
	}{{jobs.StatusCancelling, "user", 1}, {jobs.StatusCompleting, "engine", 1}, {jobs.StatusCancelled, "engine", 1}} {
		if got, by := s.moves(kj.ID, c.to), s.movesBy(kj.ID, c.to, c.by); got != c.n || by != c.n {
			t.Errorf("K's job moved to %s %d time(s), %d by %s, want exactly once by %s\n%s", c.to, got, by, c.by, c.by, strings.Join(s.events(kj.ID), "\n"))
		}
	}
	if n := s.count("count the flags and stale events", `SELECT count(*) FROM platform.job_events WHERE job_id = $1::uuid AND kind IN ('flag', 'stale')`, kj.ID); n != 0 {
		t.Errorf("K's job raised %d flag or stale event(s): the cancel must end the job without the engine inventing or doubting anything\n%s", n, strings.Join(s.events(kj.ID), "\n"))
	}
	for _, r := range view.Results {
		if r.State == "cancelled" && (r.Decision != "free" || r.Unit != "" || r.Rule != "M3-01") {
			t.Errorf("a cancelled item's result is %+v, want free, no unit, M3-01", r)
		}
	}
	m3aProblems(t, "the books of K", m3aCompareBooks(e.books(t, k.UserID)))
	s.requireBooks("after the cancelled module 3 job")

	// the control of the comparison and of the sink check: a cancelled item that reached the sink, and a cancelled item reported sent, are both caught
	w := view
	w.Counts = map[string]int{}
	for a, x := range view.Counts {
		w.Counts[a] = x
	}
	for _, r := range rows {
		if r.State == "pending" && slices.Contains(cancelled, r.ID) {
			w.Counts[r.Norm] = 1
			break
		}
	}
	jlWantProblem(t, "a cancelled item that reached the sink", "want nothing at all", jlM3ThreeWay(w))
	w = view
	w.Results = append([]jlResult(nil), view.Results...)
	for i := range w.Results {
		if w.Results[i].Ref == cancelled[0] {
			w.Results[i].State = "sent"
		}
	}
	jlWantProblem(t, "a cancelled item whose result says sent", "module 3's report says cancelled", jlM3ThreeWay(w))
	t.Logf("NOTE jl cancel K: the user cancelled the running job; module 3 cancelled %d of %d (queued), %d finished (in flight at the cancel: sent), the job ended cancelled; the sink holds %d message(s) to K and did not change for %s after the end; results %d, all free (M3-01 for the cancelled, M3-99 for the sent)", len(cancelled), n, len(sent), jlSinkCount(sink, k.Prefix), window, len(view.Results))

	// ---- the control: the same job, never cancelled ----
	e.setMode(t, m3hMode{})
	cjob := s.m3Job(c, jlM3Addrs(c, n), "cancel control")
	s.tickToStatus(cjob.ID, jobs.StatusRunning, 2*time.Minute, diagC)
	s.tickUntil("the first item of the control job reaches the sink", 60*time.Second, 300*time.Millisecond, diagC, func() (bool, string) {
		got := jlSinkCount(e.sinkView(t), c.Prefix)
		return got >= 1, fmt.Sprintf("the sink holds %d message(s) to C", got)
	})
	mid, err := c.C.ReportCampaign(ctx, cjob.ID)
	if err != nil || mid.Totals.Total != n || mid.Totals.Cancelled != 0 || mid.Totals.Sent >= n || s.status(cjob.ID) != jobs.StatusRunning {
		t.Fatalf("while C's lane is held the report is (%s) (error %v) and the job %s: want %d items, none cancelled, fewer than %d sent (the rest queued behind the lane) and the job running", m3aTotalsLine(mid.Totals), err, s.status(cjob.ID), n, n)
	}
	e.setPacing(t, c, 3, 0, 1) // released: the lane's hold is over for the rest of the campaign
	s.tickToStatus(cjob.ID, jobs.StatusSucceeded, 4*time.Minute, diagC)
	cview, csink, crows := s.m3View(e, c, cjob.ID)
	if problems := jlM3ThreeWay(cview); len(problems) != 0 {
		t.Fatalf("the control job: module 3's report, the sink, module 3's own table and the results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	for _, r := range cview.Results {
		if r.State != "sent" || r.Rule != "M3-99" {
			t.Fatalf("a result of the control job is %+v, want sent by M3-99: without a cancel every row is sent", r)
		}
	}
	if got := jlSinkCount(csink, c.Prefix); got != n || len(crows) != n || len(cview.Results) != n {
		t.Fatalf("the control job holds %d row(s) and %d result(s) and the sink %d message(s) to C, want %d of each: without a cancel every row is sent exactly once", len(crows), len(cview.Results), got, n)
	}
	if moved := s.moves(cjob.ID, jobs.StatusCancelling); moved != 0 {
		t.Fatalf("the control job moved to cancelling %d time(s)", moved)
	}
	t.Logf("NOTE jl cancel control C: the same job, never cancelled: while the lane held, %d of %d were sent and none cancelled; released, the job succeeded with %d message(s) at the sink", mid.Totals.Sent, n, jlSinkCount(csink, c.Prefix))

	// ---- module 1: the refusal, live ----
	mu := s.newUser("verify-cancel")
	s.grant(mu, jobs.ModuleVerify, 100)
	list := jlBuildList(jlAddrs("jlc"+randomHex(t, 3), 8, 2), 1, 1)
	mj, _, _ := s.start(mu, jobs.CreateReq{Module: jobs.ModuleVerify, Rows: list.Rows})
	s.tickToStatus(mj.ID, jobs.StatusRunning, 2*time.Minute, s.m1Diag(mu, mj.ID))
	for _, cp := range []struct {
		module string
		want   bool
	}{{jobs.ModuleVerify, true}, {jobs.ModuleSendBYO, true}} {
		if got := s.str("read what the module can do", `SELECT can_cancel::text FROM platform.modules WHERE id = $1`, cp.module); got != fmt.Sprint(cp.want) {
			t.Fatalf("platform.modules.can_cancel of %s is %s, want %v: the capability is data (every module can be cancelled since platform_0016)", cp.module, got, cp.want)
		}
	}
	if got := s.str("read what module 1 can pause", `SELECT can_pause::text FROM platform.modules WHERE id = 'm1'`); got != "false" {
		t.Fatalf("platform.modules.can_pause of m1 is %s, want false: module 1 has no per-job pause", got)
	}
	var na *jobs.NotAllowedError
	_, err = s.store.Pause(ctx, mu, mj.ID)
	if !errors.Is(err, jobs.ErrNotAllowed) || !errors.As(err, &na) || na.Reason != "job_module_cannot_pause" {
		t.Fatalf("the pause of a running module 1 job answered %v, want ErrNotAllowed with the database's constraint job_module_cannot_pause (the API words it pause_not_supported)", err)
	}
	jeNoRawSQL(t, "the pause of a running module 1 job", err)
	if st := s.status(mj.ID); st != jobs.StatusRunning || s.moves(mj.ID, jobs.StatusCancelling) != 0 || s.moves(mj.ID, jobs.StatusPaused) != 0 ||
		s.count("count the cancel requests", `SELECT count(*) FROM platform.jobs WHERE id = $1::uuid AND cancel_requested_at IS NOT NULL`, mj.ID) != 0 {
		t.Fatalf("after the refusals the module 1 job is %s with moves to cancelling %d and paused %d: a refused move must change nothing\n%s", st, s.moves(mj.ID, jobs.StatusCancelling), s.moves(mj.ID, jobs.StatusPaused), strings.Join(s.events(mj.ID), "\n"))
	}
	s.tickToStatus(mj.ID, jobs.StatusSucceeded, 8*time.Minute, s.m1Diag(mu, mj.ID))
	mview := s.m1View(mu, mj.ID, 8)
	if problems := jlM1ThreeWay(mview); len(problems) != 0 {
		t.Fatalf("the module 1 job that was not cancelled: the ledger, the results and module 1's report disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if reserved, charged, released, ok := s.reserveOf(mj.ID); !ok || reserved-charged-released != 0 {
		t.Fatalf("the module 1 job's reserve is (%d, %d, %d, held %v), want the whole reserve accounted for", reserved, charged, released, ok)
	}
	s.requireBooks("after the module 1 job that could not be cancelled")
	t.Logf("NOTE jl cancel m1: a running module 1 job's pause was refused by the database (job_module_cannot_pause); nothing moved; the job went on and succeeded, settled three ways (its cancel is the later live agent's: LIVE-REWRITE)")
}

// itemsOf makes the m3aItems of the pending rows (only their addresses are read: m3aView.of sums the sink's messages to them).
func itemsOf(rows []jlRow) []*m3aItem {
	var out []*m3aItem
	for _, r := range rows {
		if r.State == "pending" {
			out = append(out, &m3aItem{Index: len(out) + 1, ID: r.ID, Email: r.Norm, Kind: "ok"})
		}
	}
	return out
}

// jlSortedCounts renders the sink's counters in a fixed order, so that two readings can be compared and a difference printed.
func jlSortedCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for a, n := range m {
		if n != 0 {
			out = append(out, fmt.Sprintf("%s=%d", a, n))
		}
	}
	slices.Sort(out)
	return out
}
