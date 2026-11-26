//go:build harness

package harness

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// itemStatuses is the job's public.job_items rows by status ("cancelled=6" or "pending=6"), read through the superuser.
func (s *jlStack) itemStatuses(job string) string {
	s.t.Helper()
	q, err := s.admin.Query(s.ctx, `SELECT status || '=' || count(*) FROM public.job_items WHERE campaign_id = $1::uuid GROUP BY status ORDER BY status`, job)
	if err != nil {
		s.t.Fatalf("read the items of job %s: %v", jlShort(job), err)
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var one string
		if err := q.Scan(&one); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, one)
	}
	if err := q.Err(); err != nil {
		s.t.Fatal(err)
	}
	return strings.Join(out, ",")
}

// TestJobsLive_M3_NeverSubmittedItemsOfAnExpiredJobAreMarkedAndNeverRunAndTheMarkIsNotALock (Phase 4 close-out D9, with the REAL module 3, its workers and its sink). A module 3 send job is confirmed and the platform's
// path to module 3 is cut before the engine's first tick (the store's max start delay is 10 s): the engine writes the job's public.job_items rows ONCE (its first tick: the module being down does not stop that), waits,
// and at the deadline EXPIRES the job; in the same transaction every item of a row no module was given is marked 'cancelled' (nothing is deleted). THE THREE THINGS PROVED:
//
//	the marker     every item of the expired job is 'cancelled', the job's items_ensured_at is set, and the rows of the platform's own classes (a malformed row, a repeat) never had an item
//	never run      after the expiry, with every tick of the engine and the module's own loops, module 3 holds no job of the user, its campaign report has nothing for the job and the sink holds no message to the tenant
//	the control    the very next job of the same user and module (the link open) is run and sent to the last recipient, three ways equal, and ITS items are not marked: a job that sends keeps its items 'pending'
//
// AND ONE FINDING, recorded as a measurement and not hidden: module 3 does not read an item's status (its two statements on the table, `SELECT campaign_id ... WHERE id AND user_id` and `INSERT ... SELECT ... WHERE
// id AND user_id`: db/module-statements.txt, pinned to its code), so a DIRECT Run(job_item_id) of a marked item through the adapter is ACCEPTED: module 3 creates its own job for it. The marker is the platform's record
// that the item will never be submitted, not a lock; what keeps it from being run is that nothing calls Run for a job that has ended (the engine submits the rows of confirmed, scheduled and running jobs only, and
// no module scans the table). The test cancels the campaign it started with that one call at once. If module 3 starts REFUSING such a run, this test is red on purpose: Docs/install.md ("What a module can do with an
// item that was never submitted") is then the document to change with it.
func TestJobsLive_M3_NeverSubmittedItemsOfAnExpiredJobAreMarkedAndNeverRunAndTheMarkIsNotALock(t *testing.T) {
	ctx := jlCtx(t, 12*time.Minute)
	e := newM3aEnv(t, ctx)
	e.prepare(t, true, true)
	link := newJlLink(t, "module 3", e.raw.m3URL)
	s, tr := newJlStackWith(t, ctx, jlOpts{
		Cfg: func(sec config.Secret) config.Config {
			c := jlConfig(t, sec)
			c.M3URL = link.URL()
			return c
		},
		MaxStartDelay: jlExpiryDelay,
	})
	t.Cleanup(func() {
		if err := link.Open(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	tn := s.m3User(e, "jlitems", true)
	diag := func() string { return e.diagnostics(t, tn) }

	// ---- the job whose module is unreachable from the start ----
	const n = 6
	list := jlBuildList(jlM3Addrs(tn, n), 1, 1)
	link.requireUp("before the confirm")
	a, pv, res := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: list.Rows, Content: jlContent("never submitted")})
	if pv.Pending != n || res.Job.Status != jobs.StatusConfirmed || res.Job.StartDeadline == nil {
		t.Fatalf("the job's preview counts pending %d and the confirm answered %s with a deadline %v; want %d, confirmed and a deadline", pv.Pending, res.Job.Status, res.Job.StartDeadline != nil, n)
	}
	if got := s.itemStatuses(a.ID); got != "" {
		t.Fatalf("the confirm wrote the module's rows (%s): the engine does, at its first tick", got)
	}
	link.Cut()
	link.requireDown("before the engine's first tick")
	itemsWhileWaiting := ""
	waits := s.jlExpiryWait(a.ID, tr, link, func() string {
		if c := e.jobCount(t, tn.UserID, ""); c != 0 {
			return fmt.Sprintf("module 3 holds %d job(s) of the user", c)
		}
		if m := jlSinkCount(e.sinkView(t), tn.Prefix); m != 0 {
			return fmt.Sprintf("the sink holds %d message(s) to the tenant", m)
		}
		itemsWhileWaiting = s.itemStatuses(a.ID)
		if itemsWhileWaiting != fmt.Sprintf("pending=%d", n) {
			return fmt.Sprintf("the job's items are %q while it waits for its module, want pending=%d (written once at the first tick, never marked while the job can still be submitted)", itemsWhileWaiting, n)
		}
		return ""
	}, diag)
	var ensured string
	if err := s.admin.QueryRow(ctx, `SELECT COALESCE(items_ensured_at::text, '') FROM platform.jobs WHERE id = $1::uuid`, a.ID).Scan(&ensured); err != nil || ensured == "" {
		t.Fatalf("the job does not record that its items were written (items_ensured_at %q, %v)", ensured, err)
	}
	s.tickToStatus(a.ID, jobs.StatusExpired, 90*time.Second, diag)
	s.jlExpired(a.ID)

	// ---- the marker ----
	if got := s.itemStatuses(a.ID); got != fmt.Sprintf("cancelled=%d", n) {
		t.Fatalf("the items of the expired job are %q, want cancelled=%d: every item of a row no module was given is marked in the transaction that expires the job", got, n)
	}
	var ensuredAfter string
	if err := s.admin.QueryRow(ctx, `SELECT items_ensured_at::text FROM platform.jobs WHERE id = $1::uuid`, a.ID).Scan(&ensuredAfter); err != nil || ensuredAfter != ensured {
		t.Fatalf("items_ensured_at moved from %s to %s (%v): it is set once", ensured, ensuredAfter, err)
	}
	if rowsOfPlatformClass := s.count("count the platform's own rows", `SELECT count(*) FROM platform.job_rows w WHERE w.job_id = $1::uuid AND w.state <> 'pending' AND EXISTS (SELECT 1 FROM public.job_items i WHERE i.id = w.id)`, a.ID); rowsOfPlatformClass != 0 {
		t.Fatalf("%d row(s) of the platform's own classes (malformed, repeat) have an item: only pending rows ever reach the module's table", rowsOfPlatformClass)
	}
	if total := s.count("count the job's items", `SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid`, a.ID); total != n {
		t.Fatalf("the expired job has %d item(s), want %d: the marker deletes nothing", total, n)
	}

	// ---- never run: nothing of the job is at module 3 or at the sink ----
	if c := e.jobCount(t, tn.UserID, ""); c != 0 {
		t.Fatalf("module 3 holds %d job(s) of the user after the expiry", c)
	}
	if own := s.m3JobsOfCampaign(e, tn, a.ID); len(own) != 0 {
		t.Fatalf("module 3's own table holds %d job(s) of the expired campaign", len(own))
	}
	if m := jlSinkCount(e.sinkView(t), tn.Prefix); m != 0 {
		t.Fatalf("the sink holds %d message(s) to the tenant after the expiry", m)
	}
	s.requireBooks("after the module 3 job expired with its items marked")
	t.Logf("NOTE jl items m3: the link to module 3 was down from before the first tick; the engine wrote the job's %d items once (items_ensured_at %s) and they stayed pending while the job waited (%d submits came back as waits); at the deadline the job expired and its %d items were marked cancelled in that transaction; module 3 holds no job and the sink no message", n, ensured, waits, n)

	// ---- the control: the link open, the same user and module: the job is run, sent, and its items are not marked ----
	if err := link.Open(); err != nil {
		t.Fatal(err)
	}
	link.requireUp("before the control job")
	b, pvB, resB := s.start(tn.UserID, jobs.CreateReq{Module: jobs.ModuleSendBYO, Rows: jlRowsOf(jlM3Addrs(&m3aTenant{Prefix: tn.Prefix + "b"}, n)), Content: jlContent("never submitted: control")})
	if resB.Job.Status != jobs.StatusConfirmed || pvB.Pending != n {
		t.Fatalf("the control job's confirm answered %s with %d pending, want confirmed and %d: the slot the expired job held must be free", resB.Job.Status, pvB.Pending, n)
	}
	s.tickToStatus(b.ID, jobs.StatusSucceeded, 4*time.Minute, diag)
	view, sink, rows := s.m3View(e, tn, b.ID)
	if problems := jlM3ThreeWay(view); len(problems) != 0 {
		t.Fatalf("the control job: module 3's report, the sink, module 3's own table and the results disagree (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	if len(rows) != n || len(view.Jobs) != n || jlSinkCount(sink, tn.Prefix+"b") != n {
		t.Fatalf("the control job holds %d row(s), %d module 3 job(s) and the sink %d message(s) to its recipients: want %d of each", len(rows), len(view.Jobs), jlSinkCount(sink, tn.Prefix+"b"), n)
	}
	if got := s.itemStatuses(b.ID); got != fmt.Sprintf("pending=%d", n) {
		t.Fatalf("the items of the control job, which ran and sent, are %q, want pending=%d: only the items of a job that ended without sending are marked", got, n)
	}
	if got := s.itemStatuses(a.ID); got != fmt.Sprintf("cancelled=%d", n) {
		t.Fatalf("the expired job's items are %q after the control job ran: another job's run changed them", got)
	}
	s.requireBooks("after the module 3 control job")
	t.Logf("NOTE jl items m3 control: the next job of the same user and module ran and sent %d message(s) three ways equal; its %d items are pending", n, n)

	// ---- the finding: the marker is not a lock ----
	var marked string
	if err := s.admin.QueryRow(ctx, `SELECT id::text FROM public.job_items WHERE campaign_id = $1::uuid ORDER BY id LIMIT 1`, a.ID).Scan(&marked); err != nil {
		t.Fatalf("read a marked item: %v", err)
	}
	run, err := tn.C.Run(ctx, marked)
	if err != nil {
		t.Fatalf("module 3 REFUSED a direct Run of an item the platform marked cancelled (%s): the marker works as a lock after all, so the finding of this test (module 3 does not read an item's status) is wrong: change Docs/install.md, \"What a module can do with an item that was never submitted\", and this test", e.raw.scrub(err.Error()))
	}
	if !run.Created {
		t.Fatalf("module 3 answered a direct Run of a marked item with %+v, want a new job (the item had never been run)", run)
	}
	if own := s.m3JobsOfCampaign(e, tn, a.ID); len(own) != 1 {
		t.Fatalf("after the direct Run module 3's own table holds %d job(s) of the expired campaign, want exactly the one it just created", len(own))
	}
	// end what that one call started: cancel the campaign and wait until module 3 has nothing in flight for it (a campaign left sending would put a message into the next test's sink)
	if _, err := tn.C.Cancel(ctx, a.ID); err != nil {
		t.Fatalf("cancel the campaign the direct Run started: %s", e.raw.scrub(err.Error()))
	}
	s.tickUntil("module 3 has nothing in flight for the campaign the direct Run started", 90*time.Second, time.Second, diag, func() (bool, string) {
		own := s.m3JobsOfCampaign(e, tn, a.ID)
		if len(own) != 1 {
			return false, fmt.Sprintf("module 3's table holds %d job(s) of the campaign", len(own))
		}
		for _, st := range own {
			switch st {
			case "cancelled", "sent", "bounced", "failed":
				return true, st
			}
			return false, st
		}
		return false, ""
	})
	t.Logf("NOTE jl items m3 finding: a direct Run of a marked item through the adapter was ACCEPTED by module 3 (a new job): it does not read an item's status. The marker is the platform's record, not a lock; the campaign was cancelled at once")
}
