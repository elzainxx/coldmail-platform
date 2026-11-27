//go:build harness

package harness

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/money"
)

// Phase 5 S8 (platform_0018): the rendered content of a SEND job is cleared a dial of days after the job ended, and its OUTCOMES are never touched. Real Postgres, the real stores, the engine driven tick by tick (its
// last step is the purge) over the deterministic fake module. THE DIAL IS A DURATION HERE (seconds or an hour; the variable of the service is in days) and the clock is the database's: a job is made old by moving its
// finished_at back by the database's clock (jcrAgeEnd, the lifecycle guard off for the one statement). Every assertion has its control: a job that ended a moment ago, a job that is still running, an item that
// belongs to no job, a job that was already cleared. The names of this file start with `jcr`.
//
//	AnEndedSendJobIsCleared...   after the dial: subject and body are [removed], text and link NULL, reply_to and the recipient stay; the job is marked; outcomes (rows, results, events, ledger, reserves) are the same
//	                             digest before and after; a job that ended a moment ago, a job that is running and an item with no job are not touched; the purge is idempotent and, once marked, never looks again
//	AJobOfMoreThanOnePage...     a job of 12,001 items is cleared a page of 5,000 at a time, each call bounded to one page, by DIFFERENT store instances (nothing is remembered but what the items say); the mark comes
//	                             with the last page
//	ARealShortDial...            a dial of three seconds, no aging: nothing is cleared before it, and the mark is no sooner than three seconds after the job ended by the database's clock

// jcrAgeEnd makes a job's END old by the database's clock (no role may write finished_at: the guard is off for the one statement).
func (e *jeEnv) jcrAgeEnd(job string, by time.Duration) {
	e.t.Helper()
	e.exec("ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	tag := e.exec("UPDATE platform.jobs SET finished_at = now() - make_interval(secs => $2::float8) WHERE id = $1::uuid AND status IN ('succeeded', 'failed', 'cancelled', 'expired')", job, by.Seconds())
	e.exec("ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
	if tag.RowsAffected() != 1 {
		e.t.Fatalf("could not age the end of job %s: %d rows (it has not ended)", job, tag.RowsAffected())
	}
}

// jcrItems is every module-facing item of a campaign (a job, or a user for module 3's test-send), by id, as one line each: what the purge may change and what it may not.
func (e *jeEnv) jcrItems(campaign string) []string {
	e.t.Helper()
	return e.rows(`SELECT id::text || '|' || recipient_email || '|' || rendered_subject || '|' || rendered_body || '|' || COALESCE(rendered_text, '<null>') || '|' || COALESCE(unsubscribe_url, '<null>')
		|| '|' || COALESCE(reply_to, '<null>') || '|' || status FROM public.job_items WHERE campaign_id = $1::uuid ORDER BY id`, campaign)
}

// jcrOutcomes is a digest of everything the platform keeps as the OUTCOME of jobs: the rows, the results, the events, the ledger and the reserves. The purge must leave it exactly as it was.
func (e *jeEnv) jcrOutcomes() string {
	e.t.Helper()
	return e.str(`SELECT md5(COALESCE((SELECT string_agg(t::text, E'\n' ORDER BY t::text) FROM platform.job_rows t), ''))
		|| md5(COALESCE((SELECT string_agg(t::text, E'\n' ORDER BY t::text) FROM platform.job_results t), ''))
		|| md5(COALESCE((SELECT string_agg(t::text, E'\n' ORDER BY t::text) FROM platform.job_events t), ''))
		|| md5(COALESCE((SELECT string_agg(t::text, E'\n' ORDER BY t::text) FROM platform.ledger t), ''))
		|| md5(COALESCE((SELECT string_agg(t::text, E'\n' ORDER BY t::text) FROM platform.reserves t), ''))`)
}

// jcrRealContent is a campaign whose every part is something the purge must clear.
func jcrRealContent() *jobs.Content {
	return &jobs.Content{Subject: "Real subject", HTML: "<p>real body</p>", Text: "real text", ReplyTo: "reply@example.test"}
}

// jcrDress gives every item of the campaigns a plain-text part and a stored link of known shape, whatever the renderer wrote (the link column is platform_0017's), so that the purge has all four things to clear.
func (e *jeEnv) jcrDress(campaigns ...string) {
	e.t.Helper()
	for _, c := range campaigns {
		e.exec("UPDATE public.job_items SET rendered_text = 'plain text of ' || id::text, unsubscribe_url = 'https://app.example.test/u/' || id::text WHERE campaign_id = $1::uuid", c)
	}
}

// jcrCleared says whether a line of jcrItems is what the purge leaves: the placeholder for subject and body, no text, no link; and whether the parts it must NOT touch (the id, the recipient, reply_to, status) are
// the ones of the line before.
func jcrCleared(t *testing.T, before, after string) bool {
	t.Helper()
	b, a := strings.Split(before, "|"), strings.Split(after, "|")
	if len(b) != 8 || len(a) != 8 {
		t.Fatalf("an item line has %d and %d parts, want 8: %q / %q", len(b), len(a), before, after)
	}
	for _, i := range []int{0, 1, 6, 7} { // id, recipient, reply_to, status
		if a[i] != b[i] {
			t.Errorf("the purge changed part %d of an item: %q -> %q", i, b[i], a[i])
		}
	}
	return a[2] == "[removed]" && a[3] == "[removed]" && a[4] == "<null>" && a[5] == "<null>"
}

func TestContentRetention_AnEndedSendJobIsClearedAfterTheDialItsOutcomesAreUntouchedAndEverythingElseIsLeftAlone(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	fake := &jobenginetest.FakeModule{Module: "m2", Plan: jpcPlan("m2", 0)}
	rig := e.newRig(fake, jobengine.Config{RetentionAfter: time.Hour, SweepEvery: -1})
	uOld, uFresh, uRun, uTest := e.newUser(), e.newUser(), e.newUser(), e.newUser()
	old := e.confirmRows(uOld, "m2", jeAddrs(4, "o"), jeConfirmOpts{Content: jcrRealContent()})
	fresh := e.confirmRows(uFresh, "m2", jeAddrs(3, "f"), jeConfirmOpts{Content: jcrRealContent()})
	e.tickUntil(rig.eng, old.ID, jobs.StatusSucceeded, 10)
	e.tickUntil(rig.eng, fresh.ID, jobs.StatusSucceeded, 10)
	fake.SetDown(true) // from here no module answers: the engine's passes change nothing but what the sweeps do

	// a job that is RUNNING: started, its items written, never finished
	run := e.confirmRows(uRun, "m2", jeAddrs(3, "r"), jeConfirmOpts{Content: jcrRealContent()})
	if _, err := e.jobs.Start(ctx, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := e.jobs.EnsureJobItems(ctx, run.ID); err != nil || n != 3 {
		t.Fatalf("the running job's items: %d, %v", n, err)
	}
	// module 3's test-send writes its own item, with the USER's id as campaign_id and no job; and an item that names a job id nobody has
	e.exec(`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, reply_to) VALUES ($1, $1, 'test@example.test', 'Test subject', '<p>test</p>', 'test text', 'r@example.test')`, uTest)
	e.exec(`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text) VALUES ('f0f0f0f0-0000-4000-8000-000000000001', $1, 'ghost@example.test', 'Ghost', '<p>g</p>', 'ghost text')`, uTest)
	e.jcrDress(old.ID, fresh.ID, run.ID)
	e.jcrDress(uTest)
	const ghost = "f0f0f0f0-0000-4000-8000-000000000001"

	beforeItems := map[string][]string{old.ID: e.jcrItems(old.ID), fresh.ID: e.jcrItems(fresh.ID), run.ID: e.jcrItems(run.ID), uTest: e.jcrItems(uTest), ghost: e.jcrItems(ghost)}
	for id, want := range map[string]int{old.ID: 4, fresh.ID: 3, run.ID: 3, uTest: 1, ghost: 1} {
		if len(beforeItems[id]) != want {
			t.Fatalf("the fixture: %d items for %s, want %d", len(beforeItems[id]), id, want)
		}
	}
	for _, l := range beforeItems[old.ID] {
		if !strings.Contains(l, "|Real subject|<p>real body</p>|plain text of ") || !strings.Contains(l, "|https://app.example.test/u/") || !strings.Contains(l, "|reply@example.test|") {
			t.Fatalf("the fixture: an item of the old job does not hold the real content: %q", l)
		}
	}

	// CONTROL 1: nothing has been old for an hour yet: a pass over the engine clears nothing, and the sweep did run
	rig.eng.Tick(ctx)
	if s := rig.eng.SweepStats(); s.RetentionSweeps < 1 || s.ContentCleared != 0 || s.ContentItems != 0 {
		t.Fatalf("after a pass with nothing old the sweep stats are %+v: it must have run and cleared nothing", s)
	}
	if got := e.jcrItems(old.ID); fmt.Sprint(got) != fmt.Sprint(beforeItems[old.ID]) {
		t.Fatal("a job that ended a moment ago was cleared: the dial is an hour")
	}
	// CONTROL 2: THE CLOCK IS THE DATABASE'S: a hundred days pass on the engine's clock and the database's does not move
	rig.clock.Advance(100 * 24 * time.Hour)
	rig.eng.Tick(ctx)
	if got := e.jcrItems(old.ID); fmt.Sprint(got) != fmt.Sprint(beforeItems[old.ID]) {
		t.Fatal("a job was cleared because the ENGINE's clock moved: the age of a job must be read from the database's clock")
	}

	// THE JOB BECOMES OLD by the database's clock
	e.jcrAgeEnd(old.ID, 2*time.Hour)
	outcomesBefore := e.jcrOutcomes()
	rig.eng.Tick(ctx)
	if s := rig.eng.SweepStats(); s.ContentCleared != 1 || s.ContentItems != 4 {
		t.Fatalf("the sweep stats are %+v, want one job cleared and its four items", s)
	}
	after := e.jcrItems(old.ID)
	if len(after) != 4 {
		t.Fatalf("the old job has %d items after the purge, want 4: nothing is deleted", len(after))
	}
	for i := range after {
		if !jcrCleared(t, beforeItems[old.ID][i], after[i]) {
			t.Errorf("an item of the old job is %q after the purge: want [removed] for the subject and the body, no text, no link", after[i])
		}
		if !strings.Contains(after[i], "|reply@example.test|") {
			t.Errorf("reply_to is not the one the campaign carried: %q (it is not content of the message and stays)", after[i])
		}
	}
	if !ledgerBool(t, e.admin, "SELECT content_cleared_at IS NOT NULL AND content_cleared_at >= finished_at FROM platform.jobs WHERE id = $1", old.ID) {
		t.Error("the old job is not marked, or is marked before it ended")
	}
	// OUTCOMES ARE NEVER TOUCHED: the digest of the rows, the results, the events, the ledger and the reserves is the same before and after
	if got := e.jcrOutcomes(); got != outcomesBefore {
		t.Fatalf("the purge changed an outcome (rows, results, events, ledger or reserves):\n  before %s\n  after  %s", outcomesBefore, got)
	}
	e.requireBooks("after the purge")
	// the control of the digest: it SEES a new event and a new ledger row (it is not a digest of nothing)
	e.exec("INSERT INTO platform.job_events (job_id, kind, actor_kind, reason) VALUES ($1, 'note', 'engine', 'a control of the digest')", old.ID)
	if e.jcrOutcomes() == outcomesBefore {
		t.Fatal("the digest did not change when an event was written: it cannot see the events")
	}
	e.fund(uOld, "m2", 5)
	if e.jcrOutcomes() == outcomesBefore {
		t.Fatal("the digest did not change when the ledger grew")
	}

	// EVERYTHING ELSE IS LEFT ALONE: the job that ended a moment ago, the job that is running, the item of module 3's test-send and the item that names no job
	for id, want := range beforeItems {
		if id == old.ID {
			continue
		}
		if got := e.jcrItems(id); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("the items of %s changed:\n  before %v\n  after  %v", id, want, got)
		}
	}
	for _, id := range []string{fresh.ID, run.ID} {
		if !ledgerBool(t, e.admin, "SELECT content_cleared_at IS NULL FROM platform.jobs WHERE id = $1", id) {
			t.Errorf("job %s is marked", id)
		}
	}
	if e.jobStatus(run.ID) != jobs.StatusRunning {
		t.Errorf("the running job is %s", e.jobStatus(run.ID))
	}

	// IDEMPOTENT AND CHEAP: a second pass finds nothing to do; a marked job is not looked at again (an item that holds content again stays as it is)
	e.exec("UPDATE public.job_items SET rendered_body = 'written again' WHERE id = (SELECT id FROM public.job_items WHERE campaign_id = $1::uuid ORDER BY id LIMIT 1)", old.ID)
	res, err := e.jobs.ClearRetainedContent(ctx, time.Hour, 0, 0)
	if err != nil || res.Found != 0 || res.Items != 0 {
		t.Fatalf("a second purge: %+v, %v; want nothing found (the only old job is marked)", res, err)
	}
	if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND rendered_body = 'written again'", old.ID); n != 1 {
		t.Errorf("a marked job's item was cleared again (%d rows hold what the test wrote): the purge selects only jobs without the mark", n)
	}

	// THE SECOND JOB, when its own end is old, is cleared the same way: the sweep was never broken for it, only not due
	e.jcrAgeEnd(fresh.ID, 2*time.Hour)
	rig.eng.Tick(ctx)
	for i, l := range e.jcrItems(fresh.ID) {
		if !jcrCleared(t, beforeItems[fresh.ID][i], l) {
			t.Errorf("an item of the second job is %q after its end became old", l)
		}
	}
	// and the running job is STILL untouched: it has not ended, whatever the others did
	if got := e.jcrItems(run.ID); fmt.Sprint(got) != fmt.Sprint(beforeItems[run.ID]) {
		t.Errorf("the running job's items changed:\n  before %v\n  after  %v", beforeItems[run.ID], got)
	}
	if s := rig.eng.SweepStats(); s.ContentCleared != 2 {
		t.Errorf("sweep stats %+v, want two jobs cleared in all", s)
	}
}

func TestContentRetention_AJobOfMoreThanOnePageIsClearedPageByPageByDifferentInstancesAndTheMarkComesWithTheLastPage(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u := e.newUser()
	const rows = 12001 // pages of 5,000, 5,000 and 2,001
	j := e.confirmRows(u, "m2", jeAddrs(rows, "p"), jeConfirmOpts{Content: jcrRealContent(), Fund: 20000})
	if _, err := e.jobs.Start(ctx, j.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := e.jobs.EnsureJobItems(ctx, j.ID); err != nil || n != rows {
		t.Fatalf("the job's items: %d, %v", n, err)
	}
	if _, err := e.jobs.FailJob(ctx, j.ID, "test: the job ends here"); err != nil {
		t.Fatal(err)
	}
	e.jcrDress(j.ID)
	e.jcrAgeEnd(j.ID, 2*time.Hour)
	outcomes := e.jcrOutcomes()
	cleared := func() int64 {
		return e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid AND rendered_subject = '[removed]' AND rendered_body = '[removed]' AND rendered_text IS NULL AND unsubscribe_url IS NULL", j.ID)
	}
	marked := func() bool {
		return ledgerBool(t, e.admin, "SELECT content_cleared_at IS NOT NULL FROM platform.jobs WHERE id = $1", j.ID)
	}
	if cleared() != 0 || marked() {
		t.Fatal("the fixture: the job is cleared or marked before the purge")
	}
	// every call is another store instance (a restarted process, the next leader) bounded to ONE page: nothing carries over but what the items say
	steps := []struct {
		items      int64
		cleared    int64
		unfinished int
		done       int
	}{{5000, 5000, 1, 0}, {5000, 10000, 1, 0}, {2001, 12001, 0, 1}}
	for i, want := range steps {
		st, err := jobs.New(e.pool, money.New(e.pool))
		if err != nil {
			t.Fatal(err)
		}
		res, err := st.ClearRetainedContent(ctx, time.Hour, 0, 1)
		if err != nil || res.Found != 1 || res.Pages != 1 || res.Items != want.items || res.Unfinished != want.unfinished || res.Cleared != want.done {
			t.Fatalf("call %d: %+v, %v; want one page and %d items (unfinished %d, cleared %d)", i+1, res, err, want.items, want.unfinished, want.done)
		}
		if got := cleared(); got != want.cleared {
			t.Fatalf("after call %d %d items are cleared, want %d", i+1, got, want.cleared)
		}
		if marked() != (want.done == 1) {
			t.Fatalf("after call %d the job's mark is %v: it must come with the last page and not before", i+1, marked())
		}
	}
	if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1::uuid", j.ID); n != rows {
		t.Errorf("%d items after the purge, want %d: nothing is deleted", n, rows)
	}
	if got := e.jcrOutcomes(); got != outcomes {
		t.Error("the purge of a job of three pages changed an outcome")
	}
	if res, err := e.jobs.ClearRetainedContent(ctx, time.Hour, 0, 0); err != nil || res.Found != 0 {
		t.Errorf("a purge after the last page: %+v, %v; want nothing found", res, err)
	}
	e.requireBooks("after the purge of a job of three pages")
}

func TestContentRetention_ARealShortDialClearsNothingBeforeItsSecondsAndTheMarkIsNoSoonerByTheDatabasesClock(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	fake := &jobenginetest.FakeModule{Module: "m2", Plan: jpcPlan("m2", 0)}
	rig := e.newRig(fake, jobengine.Config{RetentionAfter: 3 * time.Second, SweepEvery: -1})
	u := e.newUser()
	j := e.confirmRows(u, "m2", jeAddrs(3, "s"), jeConfirmOpts{Content: jcrRealContent()})
	e.tickUntil(rig.eng, j.ID, jobs.StatusSucceeded, 10)
	before := e.jcrItems(j.ID)
	deadline := time.Now().Add(90 * time.Second)
	ticks := 0
	for ledgerBool(t, e.admin, "SELECT content_cleared_at IS NULL FROM platform.jobs WHERE id = $1", j.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("the job was not cleared within 90 seconds of a dial of three (%d ticks, sweeps %+v)", ticks, rig.eng.SweepStats())
		}
		if ticks == 0 && fmt.Sprint(e.jcrItems(j.ID)) != fmt.Sprint(before) {
			t.Fatal("the content changed before the dial had passed")
		}
		rig.eng.Tick(ctx)
		ticks++
		time.Sleep(300 * time.Millisecond)
	}
	if ticks < 2 {
		t.Error("the job was cleared at the first tick: one that ended seconds ago was judged older than three")
	}
	gap := e.str("SELECT extract(epoch FROM (content_cleared_at - finished_at))::text FROM platform.jobs WHERE id = $1", j.ID)
	if secs, err := strconv.ParseFloat(gap, 64); err != nil || secs < 3 || secs > 60 {
		t.Errorf("the job was cleared %s seconds after it ended, want at least 3 (the dial) and not a minute (%v)", gap, err)
	}
	for i, l := range e.jcrItems(j.ID) {
		if !jcrCleared(t, before[i], l) {
			t.Errorf("an item is %q after the purge", l)
		}
	}
}
