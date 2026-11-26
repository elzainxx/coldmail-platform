//go:build harness

package harness

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// TestJobsLive_M3_ScheduledStartsOnTime (Phase 4, chunk B11, the part a test can do without stopping a container; the REAL module 3 and its sink): a send job scheduled about a minute and ten seconds ahead (the store
// refuses less than a minute, by the DATABASE's clock) is held by the platform until its time and then run by the engine.
//
// WHILE IT WAITS (every second, for the whole wait, with the engine ticking in between: its ticks are COUNTED, and a wait in which the engine did not tick would prove nothing):
//
//	the slot        the job holds it: a second job of the same user and module confirmed meanwhile is ErrSlotTaken, and holds nothing (no reserve, no ledger row; module 3 is not metered)
//	nothing sent    the job is still `scheduled`, no row of it is stamped as handed over, public.job_items holds no row of its campaign, module 3 holds no job of the user and the sink holds no message to the tenant;
//	                and the store does not even offer the job to the engine (EngineWork, which judges by the database's clock, does not list it)
//
// AFTER ITS TIME (the control of every "nothing" above: the same reads now see something): the store offers the job to the engine; the engine submits it, and it runs to `succeeded` with every row sent, three ways
// equal (jlM3ThreeWay: the report, module 3's own table, the results, the sink); public.job_items now holds the rows, module 3 the jobs, the sink the messages; and by the DATABASE's clock the job started at or
// after its scheduled start (started_at >= scheduled_start), the lag between the two is printed, and its start deadline is its scheduled start plus the max start delay.
func TestJobsLive_M3_ScheduledStartsOnTime(t *testing.T) {
	ctx := jlCtx(t, 7*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	s := newJlStack(t, ctx)
	tn := s.m3User(e, "jlsched", true)
	const n = 6
	diag := func() string { return e.diagnostics(t, tn) }

	j := s.create(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf(jlM3Addrs(tn, n)), Content: jlContent("scheduled")})
	pv := s.preview(tn.UserID, j.ID)
	// the second job of the same user, previewed now so that the slot test below needs no time of its own
	j2 := s.create(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf([]string{tn.Prefix + "second@example.test"}), Content: jlContent("the second job")})
	pv2 := s.preview(tn.UserID, j2.ID)

	start := s.dbNow().Add(jobs.MinScheduleAhead + 10*time.Second)
	res, err := s.confirm(tn.UserID, j.ID, pv.Fingerprint, &start)
	if err != nil {
		t.Fatalf("the confirm of a job scheduled %s ahead: %v", jobs.MinScheduleAhead+10*time.Second, err)
	}
	if res.Job.Status != jobs.StatusScheduled || res.Job.ScheduledStart == nil || res.Job.StartDeadline == nil || res.Reserved != 0 || res.Pending != n {
		t.Fatalf("the confirm answered %+v, want a scheduled job with its start and its deadline, nothing reserved and %d pending", res, n)
	}
	if d := res.Job.ScheduledStart.Sub(start); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("the job's scheduled start is %s, the one asked for is %s", res.Job.ScheduledStart, start)
	}
	if got := s.str("compare the deadline with the start", `SELECT (start_deadline = scheduled_start + make_interval(secs => $2::double precision))::text FROM platform.jobs WHERE id = $1::uuid`, j.ID, jobs.DefaultMaxStartDelay.Seconds()); got != "true" {
		t.Fatalf("the job's start deadline is not its scheduled start plus the max start delay (%s)", jobs.DefaultMaxStartDelay)
	}

	// ---- it holds the slot ----
	_, err = s.confirm(tn.UserID, j2.ID, pv2.Fingerprint, nil)
	if !errors.Is(err, jobs.ErrSlotTaken) {
		t.Fatalf("the confirm of a second job of the user while the first is scheduled answered %v, want ErrSlotTaken", err)
	}
	if st := s.status(j2.ID); st != jobs.StatusPreviewed || s.ledgerRowsOf(j2.ID) != 0 {
		t.Fatalf("the refused second job is %s with %d ledger row(s), want previewed and none: the refusal leaves nothing behind", st, s.ledgerRowsOf(j2.ID))
	}
	if _, err := s.store.DiscardDraft(ctx, tn.UserID, j2.ID); err != nil {
		t.Fatalf("discard the second job: %v", err)
	}

	// ---- the wait: the engine ticks, and nothing of the job moves ----
	listed := func() bool {
		work, err := s.store.EngineWork(ctx, jobs.MaxEngineWorkLimit)
		if err != nil {
			t.Fatalf("EngineWork: %v", err)
		}
		for _, w := range work {
			if w.ID == j.ID {
				return true
			}
		}
		return false
	}
	nothingSent := func(when string) {
		t.Helper()
		if st := s.status(j.ID); st != jobs.StatusScheduled {
			t.Fatalf("%s: the job is %s, want scheduled until its time\n%s", when, st, s.dump(diag))
		}
		if stamped := s.count("count the rows handed to a module", `SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND submitted_at IS NOT NULL`, j.ID); stamped != 0 {
			t.Fatalf("%s: %d row(s) of the job are stamped as handed over before its time", when, stamped)
		}
		if items := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); items != 0 {
			t.Fatalf("%s: public.job_items holds %d row(s) of the campaign before the job's time", when, items)
		}
		if jobsAt := e.jobCount(t, tn.UserID, ""); jobsAt != 0 {
			t.Fatalf("%s: module 3 holds %d job(s) of the user before the job's time", when, jobsAt)
		}
		if msgs := jlSinkCount(e.sinkView(t), tn.Prefix); msgs != 0 {
			t.Fatalf("%s: the sink holds %d message(s) to the tenant before the job's time", when, msgs)
		}
		if listed() {
			t.Fatalf("%s: the store offers the scheduled job to the engine before its scheduled start", when)
		}
	}
	ticks0 := s.eng.Stats().Ticks
	waited := time.Now()
	checks := 0
	for s.dbNow().Before(start) {
		s.eng.Tick(ctx)
		nothingSent("while the job waits")
		checks++
		select {
		case <-ctx.Done():
			t.Fatalf("the test's time budget ended while the job waited")
		case <-time.After(time.Second):
		}
	}
	ticksWhileWaiting := s.eng.Stats().Ticks - ticks0
	if ticksWhileWaiting < 30 || checks < 30 {
		t.Fatalf("the engine ticked %d time(s) and the test checked %d time(s) during the %s wait, want at least 30 of each: a wait the engine did not take part in proves nothing", ticksWhileWaiting, checks, time.Since(waited).Round(time.Second))
	}

	// ---- the time has come: the same reads now see the job ----
	if !listed() {
		t.Fatalf("after the scheduled start the store does not offer the job to the engine\n%s", s.dump(diag))
	}
	s.tickToStatus(j.ID, jobs.StatusRunning, 2*time.Minute, diag)
	s.tickToStatus(j.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	view, sink, _ := s.m3View(e, tn, j.ID)
	if p := jlM3ThreeWay(view); len(p) != 0 {
		t.Fatalf("module 3's report, the sink, module 3's own table and the job's results disagree (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if items := s.count("count the job items of the campaign", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, j.ID); items != n || len(view.Jobs) != n || jlSinkCount(sink, tn.Prefix) != n {
		t.Fatalf("after the job ran public.job_items holds %d row(s), module 3 %d job(s) and the sink %d message(s) to the tenant, want %d of each: the reads that saw nothing while it waited do see the job once it ran", items, len(view.Jobs), jlSinkCount(sink, tn.Prefix), n)
	}
	var onTime bool
	var lagSecs float64
	if err := s.admin.QueryRow(ctx, `SELECT started_at >= scheduled_start, EXTRACT(EPOCH FROM (started_at - scheduled_start))::double precision FROM platform.jobs WHERE id = $1::uuid`, j.ID).Scan(&onTime, &lagSecs); err != nil {
		t.Fatalf("read the job's start: %v", err)
	}
	lag := time.Duration(lagSecs * float64(time.Second))
	if !onTime {
		t.Fatalf("the job started before its scheduled start by the database's clock (lag %s)", lag)
	}
	for _, to := range []string{jobs.StatusScheduled, jobs.StatusRunning, jobs.StatusCompleting, jobs.StatusSucceeded} {
		if moved := s.moves(j.ID, to); moved != 1 {
			t.Errorf("the job moved to %s %d time(s), want exactly once\n%s", to, moved, strings.Join(s.events(j.ID), "\n"))
		}
	}
	s.requireBooks("after the scheduled job")
	t.Logf("NOTE jl schedule: scheduled %s ahead; for %s the engine ticked %d time(s) and %d reads saw the job scheduled, nothing handed over, no job item, no module 3 job, no message at the sink, and not offered by EngineWork; the job started %s after its scheduled start (the database's clock) and ran to succeeded: %d of %d rows sent",
		jobs.MinScheduleAhead+10*time.Second, time.Since(waited).Round(time.Second), ticksWhileWaiting, checks, lag.Round(time.Millisecond), jlSinkCount(sink, tn.Prefix), n)
}
