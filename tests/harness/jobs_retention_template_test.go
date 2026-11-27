//go:build harness

package harness

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S8 as extended by the owner's ruling (platform_0021): at the same mark as the rendered messages, the platform clears the campaign TEMPLATE of an ended job (subject and HTML to [removed], text to empty,
// reply_to stays) and the MERGE DATA of every uploaded row (fields to {}), for every ended job (a verify job's rows carry merge columns too), and keeps for ever the addresses, the outcomes, the charges, the dates and
// counts and the events. Real Postgres, the real stores. The names of this file start with `jrt`.
//
//	TheTemplateAndTheMergeData...   a send job and a verify job older than the dial: template and merge data cleared, and a digest of everything that must stay (the rows without their merge data, the ended jobs without their
//	                                template, the results, the events, the ledger, the reserves) is the same before and after; a job younger than the dial, a job still running (other users') and the items of no job are untouched
//	TheResultsJSONAndCSV...         the results of a cleared job, over the HTTP API, as JSON pages and as the CSV, are byte for byte what they were, and hold no merge value before or after; the job JSON gains
//	                                content_cleared_at (null until then) and shows the cleared template
//	PlatformAppMayEmpty...          the database, as platform_app, in each of the twelve statuses: fields may become {} on an ended job and the template its placeholders, and no other value, no other column beside
//	                                it, no non-ended job (draft and previewed lists stay editable as before), no administrator

func jrtList(n int, tag string) []jobs.RowIn {
	out := make([]jobs.RowIn, n)
	for i := range out {
		out[i] = jobs.RowIn{Email: fmt.Sprintf("%s%05d@example.test", tag, i+1), Fields: map[string]string{"first_name": fmt.Sprintf("Ann%d", i+1), "city": "Oslo"}}
	}
	return out
}

func jrtTemplate() *jobs.Content {
	return &jobs.Content{Subject: "Hello {{first_name}}", HTML: "<p>Hi {{first_name}} of {{city}}</p>", Text: "Hi {{first_name}}", ReplyTo: "reply@example.test"}
}

// jrtAgeEnd makes an ended job's END old by the database's clock (no role may write finished_at: the guard is off for the one statement).
func (e *mnyEnv) jrtAgeEnd(job string, by time.Duration) {
	e.t.Helper()
	e.exec("ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard")
	tag := e.exec("UPDATE platform.jobs SET finished_at = now() - make_interval(secs => $2::float8) WHERE id = $1::uuid AND status IN ('succeeded', 'failed', 'cancelled', 'expired')", job, by.Seconds())
	e.exec("ALTER TABLE platform.jobs ENABLE TRIGGER jobs_transition_guard")
	if tag.RowsAffected() != 1 {
		e.t.Fatalf("could not age the end of job %s: %d rows (it has not ended)", job, tag.RowsAffected())
	}
}

// jrtRows is the job's rows as one line each: the address as typed and normalised, the state, the reason, and (when withFields) the merge data.
func (e *mnyEnv) jrtRows(job string, withFields bool) []string {
	e.t.Helper()
	return e.rows(`SELECT ordinal::text || '|' || email_raw || '|' || COALESCE(email_norm, '') || '|' || state || '|' || COALESCE(reason, '') || '|' || COALESCE(submitted_at::text, '')
		|| CASE WHEN $2::boolean THEN '|' || fields::text ELSE '' END FROM platform.job_rows WHERE job_id = $1::uuid ORDER BY ordinal`, job, withFields)
}

func (e *mnyEnv) jrtFrozen(job string) string {
	e.t.Helper()
	return e.str("SELECT frozen_settings::text FROM platform.jobs WHERE id = $1::uuid", job)
}

func (e *mnyEnv) jrtMerged(job string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND fields <> '{}'::jsonb", job)
}

func TestContentRetention_TheTemplateAndTheMergeDataOfEveryEndedJobAreClearedAndEverythingThatMustStayIsByteIdentical(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	fake := &jobenginetest.FakeModule{Module: "m2", Plan: jpcPlan("m2", 0)}
	rig := e.newRig(fake, jobengine.Config{RetentionAfter: time.Hour, SweepEvery: -1})
	uOld, uVerify, uFresh, uRun, uTest := e.newUser(), e.newUser(), e.newUser(), e.newUser(), e.newUser()

	old := e.confirmRows(uOld, "m2", jrtList(4, "o"), jeConfirmOpts{Content: jrtTemplate()})
	verify := e.confirmRows(uVerify, "m1", jrtList(3, "v"))
	fresh := e.confirmRows(uFresh, "m2", jrtList(3, "f"), jeConfirmOpts{Content: jrtTemplate()})
	// the verify job ends through the store, before any pass of the engine (the fake plays module 2): started, then failed, every row given the platform's free result
	if _, err := e.jobs.Start(ctx, verify.ID, "ref-retention-verify"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.FailJob(ctx, verify.ID, "test: the job ends here"); err != nil {
		t.Fatal(err)
	}
	e.tickUntil(rig.eng, old.ID, jobs.StatusSucceeded, 10)
	e.tickUntil(rig.eng, fresh.ID, jobs.StatusSucceeded, 10)
	fake.SetDown(true) // from here no module answers: the engine's passes change nothing but what the sweeps do
	run := e.confirmRows(uRun, "m2", jrtList(3, "r"), jeConfirmOpts{Content: jrtTemplate()})
	if _, err := e.jobs.Start(ctx, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := e.jobs.EnsureJobItems(ctx, run.ID); err != nil || n != 3 {
		t.Fatalf("the running job's items: %d, %v", n, err)
	}
	e.exec(`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, reply_to) VALUES ($1, $1, 'test@example.test', 'Test subject', '<p>test</p>', 'test text', 'r@example.test')`, uTest)
	e.jcrDress(old.ID, fresh.ID, run.ID)
	e.jcrDress(uTest)

	type snap struct {
		rowsWith, rowsWithout []string
		frozen                string
		items                 []string
	}
	take := func(job string) snap {
		return snap{rowsWith: e.jrtRows(job, true), rowsWithout: e.jrtRows(job, false), frozen: e.jrtFrozen(job), items: e.jcrItems(job)}
	}
	before := map[string]snap{old.ID: take(old.ID), verify.ID: take(verify.ID), fresh.ID: take(fresh.ID), run.ID: take(run.ID)}
	testItems := e.jcrItems(uTest) // module 3's test-send writes an item of its own, with the user's id as campaign_id and no job
	for _, id := range []string{old.ID, verify.ID, fresh.ID, run.ID} {
		if len(before[id].rowsWith) == 0 || e.jrtMerged(id) != int64(len(before[id].rowsWith)) {
			t.Fatalf("the fixture: job %s has %d merge-data rows of %d", id, e.jrtMerged(id), len(before[id].rowsWith))
		}
	}
	if !strings.Contains(before[old.ID].frozen, `"subject": "Hello {{first_name}}"`) || strings.Contains(before[verify.ID].frozen, `"content"`) {
		t.Fatalf("the fixture: the frozen documents are %s and %s", before[old.ID].frozen, before[verify.ID].frozen)
	}

	// CONTROL 1: nothing has been old for an hour yet: the sweep runs and clears nothing, not the template, not the merge data
	rig.eng.Tick(ctx)
	if s := rig.eng.SweepStats(); s.RetentionSweeps < 1 || s.ContentCleared != 0 || s.ContentRows != 0 || s.ContentItems != 0 {
		t.Fatalf("after a pass with nothing old the sweep stats are %+v", s)
	}
	for id, b := range before {
		if got := e.jrtRows(id, true); fmt.Sprint(got) != fmt.Sprint(b.rowsWith) || e.jrtFrozen(id) != b.frozen {
			t.Fatalf("job %s changed before it was old enough", id)
		}
	}

	// the two ended jobs become old by the database's clock; the third ended job (fresh) and the running one do not
	e.jrtAgeEnd(old.ID, 2*time.Hour)
	e.jrtAgeEnd(verify.ID, 2*time.Hour)
	outcomes := e.jcrOutcomes()
	itemsBefore := e.jrtItemsDigest(old.ID, fresh.ID, run.ID, uTest)
	rig.eng.Tick(ctx)
	if s := rig.eng.SweepStats(); s.ContentCleared != 2 || s.ContentItems != 4 || s.ContentRows != 7 {
		t.Fatalf("the sweep stats are %+v, want two jobs cleared, the 4 messages of the send job and the 7 rows of the two jobs", s)
	}

	// the send job: the merge data is gone, the template is its placeholders, reply_to and the settings are as they were, the messages are cleared, the job is marked
	if e.jrtMerged(old.ID) != 0 || fmt.Sprint(e.jrtRows(old.ID, false)) != fmt.Sprint(before[old.ID].rowsWithout) {
		t.Errorf("the send job: %d rows still hold merge data, or an address, a state or a reason changed", e.jrtMerged(old.ID))
	}
	for key, want := range map[string]string{"subject": "[removed]", "html": "[removed]", "text": "", "reply_to": "reply@example.test"} {
		if got := e.str("SELECT jsonb_extract_path_text(frozen_settings, 'content', $2::text) FROM platform.jobs WHERE id = $1::uuid", old.ID, key); got != want {
			t.Errorf("the send job's template says %s = %q, want %q", key, got, want)
		}
	}
	if !ledgerBool(t, e.admin, "SELECT (frozen_settings - 'content') = ($2::jsonb - 'content') AND jsonb_typeof(frozen_settings -> 'content') = 'object' AND (SELECT count(*) FROM jsonb_object_keys(frozen_settings -> 'content')) = 4 FROM platform.jobs WHERE id = $1::uuid", old.ID, before[old.ID].frozen) {
		t.Error("the settings of the send job changed, or its content lost or gained a key")
	}
	for i, l := range e.jcrItems(old.ID) {
		if !jcrCleared(t, before[old.ID].items[i], l) {
			t.Errorf("a message of the send job is %q after the purge", l)
		}
	}
	// the verify job: merge data gone, no template to clear, the frozen document byte for byte as it was
	if e.jrtMerged(verify.ID) != 0 || fmt.Sprint(e.jrtRows(verify.ID, false)) != fmt.Sprint(before[verify.ID].rowsWithout) {
		t.Errorf("the verify job: %d rows still hold merge data, or an address, a state or a reason changed", e.jrtMerged(verify.ID))
	}
	if e.jrtFrozen(verify.ID) != before[verify.ID].frozen {
		t.Error("the frozen document of a verify job changed: it has no template to clear")
	}
	for _, id := range []string{old.ID, verify.ID} {
		if !ledgerBool(t, e.admin, "SELECT content_cleared_at IS NOT NULL AND content_cleared_at >= finished_at FROM platform.jobs WHERE id = $1", id) {
			t.Errorf("job %s is not marked, or is marked before it ended", id)
		}
	}
	// WHAT MUST STAY: the digest of the addresses and states of every row, the ended jobs without their template, the results, the events, the ledger and the reserves is the same
	if got := e.jcrOutcomes(); got != outcomes {
		t.Fatalf("the purge changed something that stays (rows without merge data, ended jobs, results, events, ledger or reserves):\n  before %s\n  after  %s", outcomes, got)
	}
	e.requireBooks("after the purge of the template and the merge data")
	// the job's own counts and results as the store says them
	for _, id := range []string{old.ID, verify.ID} {
		if n := e.resultCount(id); n != int64(len(before[id].rowsWith)) {
			t.Errorf("job %s has %d results for %d rows after the purge", id, n, len(before[id].rowsWith))
		}
	}

	// EVERYTHING ELSE IS LEFT ALONE: the job that ended a moment ago and the job that is running (both other users'), and the item of module 3's test-send
	for _, id := range []string{fresh.ID, run.ID} {
		b := before[id]
		if fmt.Sprint(e.jrtRows(id, true)) != fmt.Sprint(b.rowsWith) || e.jrtFrozen(id) != b.frozen || fmt.Sprint(e.jcrItems(id)) != fmt.Sprint(b.items) {
			t.Errorf("job %s (younger than the dial, or not ended) changed: its rows, its template or its messages", id)
		}
		if !ledgerBool(t, e.admin, "SELECT content_cleared_at IS NULL FROM platform.jobs WHERE id = $1", id) {
			t.Errorf("job %s is marked", id)
		}
	}
	if fmt.Sprint(e.jcrItems(uTest)) != fmt.Sprint(testItems) {
		t.Error("the item of module 3's test-send changed")
	}
	if e.jrtItemsDigest(old.ID, fresh.ID, run.ID, uTest) == itemsBefore {
		t.Error("the digest of the items did not change although the send job's messages were cleared: the instrument cannot see them")
	}
	// the job as the store tells it
	for id, user := range map[string]string{old.ID: uOld, verify.ID: uVerify, fresh.ID: uFresh, run.ID: uRun} {
		j, err := e.jobs.Get(ctx, user, id)
		if err != nil {
			t.Fatal(err)
		}
		cleared := id == old.ID || id == verify.ID
		if (j.ContentClearedAt != nil) != cleared {
			t.Errorf("job %s: ContentClearedAt is %v, want set: %v", id, j.ContentClearedAt, cleared)
		}
		if id == old.ID && (j.Content == nil || j.Content.Subject != "[removed]" || j.Content.HTML != "[removed]" || j.Content.Text != "" || j.Content.ReplyTo != "reply@example.test") {
			t.Errorf("the store shows the cleared send job's content as %+v", j.Content)
		}
	}
	// IDEMPOTENT: nothing more to do, nothing written
	again := e.fingerprint()
	rig.eng.Tick(ctx)
	rig.eng.Tick(ctx)
	if got := e.fingerprint(); got != again {
		t.Error("a second pass changed the books or the jobs")
	}
	if res, err := e.jobs.ClearRetainedContent(ctx, time.Hour, 0, 0); err != nil || res.Found != 0 {
		t.Errorf("a purge after the purge: %+v, %v; want nothing found", res, err)
	}
}

// jrtItemsDigest is a digest of every item of the campaigns named, as the modules read them.
func (e *jeEnv) jrtItemsDigest(campaigns ...string) string {
	e.t.Helper()
	var all []string
	for _, c := range campaigns {
		all = append(all, e.jcrItems(c)...)
	}
	return e.str("SELECT md5($1::text)", strings.Join(all, "\n"))
}

// ---- the API -----------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jrtEnd finishes a confirmed job the way the engine does, through the store: running, a result for every pending row, completing, succeeded.
func (e *japEnv) jrtEnd(job, module string) {
	e.t.Helper()
	ctx := testCtx(e.t)
	jobWalkTo(e.t, e.admin, job, "running")
	if module != "m1" {
		if _, err := e.jobs.EnsureJobItems(ctx, job); err != nil {
			e.t.Fatalf("EnsureJobItems(%s): %v", job, err)
		}
	}
	var items []jobs.ResultItem
	for _, id := range e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY ordinal", job) {
		o := charge.Outcome{Module: module, State: "sent"}
		if module == "m1" {
			o = charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}
		}
		items = append(items, jobs.ResultItem{ItemRef: id, Outcome: o})
	}
	if _, err := e.jobs.RecordResults(ctx, job, items); err != nil {
		e.t.Fatalf("RecordResults(%s): %v", job, err)
	}
	if _, err := e.jobs.Complete(ctx, job); err != nil {
		e.t.Fatalf("Complete(%s): %v", job, err)
	}
	if _, err := e.jobs.Finish(ctx, job); err != nil {
		e.t.Fatalf("Finish(%s): %v", job, err)
	}
	if got := e.jobStatus(job); got != "succeeded" {
		e.t.Fatalf("job %s is %s, want succeeded", job, got)
	}
}

func jrtObj(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not a JSON object (%v): %s", err, body)
	}
	return m
}

func TestContentRetention_TheResultsJSONAndCSVOfAClearedJobAreByteForByteWhatTheyWereAndTheJobSaysWhenItWasCleared(t *testing.T) {
	e := newJapEnv(t)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	mk := func(user, module string, n int, tag string) string {
		e.fund(user, module, 1000)
		var j jobs.Job
		if module == "m1" {
			j = e.createRows(user, module, jrtList(n, tag))
		} else {
			j = e.createRowsContent(user, module, jrtList(n, tag))
		}
		p := e.preview(user, j.ID)
		e.confirm(user, j.ID, p.Fingerprint)
		e.jrtEnd(j.ID, module)
		return j.ID
	}
	verify := mk(A, "m1", 3, "v")
	send := mk(B, "m2", 3, "s")
	for _, id := range []string{verify, send} {
		if e.jrtMerged(id) != 3 {
			t.Fatalf("the fixture: job %s has %d rows with merge data, want 3", id, e.jrtMerged(id))
		}
	}
	type view struct{ job, results, csv []byte }
	fetch := func(user, id string) view {
		return view{
			job:     e.callAs(user, "GET", "/v1/jobs/"+id, "", nil).Body,
			results: e.callAs(user, "GET", "/v1/jobs/"+id+"/results?limit=100", "", nil).Body,
			csv:     e.callAs(user, "GET", "/v1/jobs/"+id+"/results.csv", "", nil).Body,
		}
	}
	before := map[string]view{verify: fetch(A, verify), send: fetch(B, send)}
	for id, v := range before {
		if jrtObj(t, v.job)["job"].(map[string]any)["content_cleared_at"] != nil {
			t.Fatalf("job %s says it was cleared before the purge", id)
		}
		for _, body := range [][]byte{v.results, v.csv, v.job} {
			if bytes.Contains(body, []byte("Ann")) || bytes.Contains(body, []byte("Oslo")) {
				t.Fatalf("job %s: a merge value is in what the API shows before the purge: %s", id, body)
			}
		}
		recs, err := csv.NewReader(bytes.NewReader(v.csv)).ReadAll()
		if err != nil || len(recs) != 4 {
			t.Fatalf("job %s: the CSV has %d records (%v), want the header and three rows: %s", id, len(recs), err, v.csv)
		}
		if strings.Join(recs[0], ",") != "ordinal,email,origin,state,reason,decision,unit,rule,suppressed" {
			t.Fatalf("job %s: the CSV header is not the nine columns of address and outcome: %v", id, recs[0])
		}
	}

	e.jrtAgeEnd(verify, 2*time.Hour)
	e.jrtAgeEnd(send, 2*time.Hour)
	res, err := e.jobs.ClearRetainedContent(ctx, time.Hour, 0, 0)
	if err != nil || res.Found != 2 || res.Cleared != 2 || res.Rows != 6 || res.Items != 3 {
		t.Fatalf("the purge: %+v, %v; want two jobs cleared, 6 rows of merge data and the 3 messages of the send job", res, err)
	}
	for id, user := range map[string]string{verify: A, send: B} {
		after := fetch(user, id)
		if !bytes.Equal(after.results, before[id].results) {
			t.Errorf("job %s: the results JSON changed:\n  before %s\n  after  %s", id, before[id].results, after.results)
		}
		if !bytes.Equal(after.csv, before[id].csv) {
			t.Errorf("job %s: the results CSV changed:\n  before %s\n  after  %s", id, before[id].csv, after.csv)
		}
		// the job JSON: content_cleared_at is now a time, the update time moved with the update, and nothing else changed except the template of a send job
		b, a := jrtObj(t, before[id].job)["job"].(map[string]any), jrtObj(t, after.job)["job"].(map[string]any)
		if a["content_cleared_at"] == nil {
			t.Errorf("job %s: content_cleared_at is still null after the purge: %v", id, a)
		}
		for _, m := range []map[string]any{a, b} {
			delete(m, "content_cleared_at")
			delete(m, "updated_at")
		}
		if id == send {
			c, _ := a["content"].(map[string]any)
			if c["subject"] != "[removed]" || c["html"] != "[removed]" || c["text"] != "" || c["reply_to"] != "reply@example.test" {
				t.Errorf("the cleared send job's content is %v", c)
			}
			delete(a, "content")
			delete(b, "content")
		} else if _, has := a["content"]; has {
			t.Errorf("a verify job's JSON has a content: %v", a["content"])
		}
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("job %s: the job JSON changed in more than content_cleared_at, updated_at and the template:\n  before %v\n  after  %v", id, b, a)
		}
		if e.jrtMerged(id) != 0 {
			t.Errorf("job %s still holds merge data", id)
		}
	}
	// another user cannot read the cleared job (the purge widened nothing)
	if r := e.callAs(B, "GET", "/v1/jobs/"+verify+"/results.csv", "", nil); r.Status != 404 {
		t.Errorf("another user's request for the cleared job's CSV answered %d, want 404", r.Status)
	}
	// the store's own readers, page by page, are the same too: EachResult over the whole list
	var n int
	if err := e.jobs.EachResult(ctx, A, verify, func(r jobs.Result) error { n++; return nil }); err != nil || n != 3 {
		t.Errorf("EachResult over the cleared verify job: %d results, %v; want 3", n, err)
	}
}

// createRowsContent is createRows for a send module with the template of this file (the helper of the store tests carries a campaign of its own with no merge fields).
func (e *jbsEnv) createRowsContent(user, module string, rows []jobs.RowIn) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.CreateDraft(testCtx(e.t), user, jobs.CreateReq{Module: module, Rows: rows, Content: jrtTemplate()})
	if err != nil {
		e.t.Fatalf("CreateDraft(%s, %d rows): %v", module, len(rows), err)
	}
	return j
}

// ---- the database --------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestContentRetentionSchema_PlatformAppMayEmptyTheMergeDataAndClearTheTemplateOfAnEndedJobAndNothingElseInAnyOfTheTwelveStatuses(t *testing.T) {
	_, admin := migratedScratch(t)
	tx := ledgerBegin(t, admin)
	ledgerSeed(t, tx)
	next := jobIDs("e5")
	a := ledgerUserA
	frozen := ledgerWhy{Constraint: "job_rows_frozen"}
	frozenDoc := ledgerWhy{Message: "frozen"}
	const doc = `{"settings":{"unknown_choice":""},"content":{"subject":"Real subject","html":"<p>Real</p>","text":"Real text","reply_to":"reply@example.test"}}`
	// the ONE value the guard lets the frozen document of an ended job become: the store's own expression
	const cleared = `jsonb_set(jsonb_set(jsonb_set(frozen_settings, '{content,subject}', '"[removed]"'::jsonb), '{content,html}', '"[removed]"'::jsonb), '{content,text}', '""'::jsonb)`
	editable := map[string]bool{"draft": true, "previewed": true}

	// a job in each status with two rows of merge data
	build := func(sp pgx.Tx, status string) string {
		job := next()
		ledgerMust(t, sp, "INSERT INTO platform.jobs (id, user_id, module_id, frozen_settings) VALUES ($1, $2, 'm2', $3::jsonb)", job, a, doc)
		for i, who := range []string{"ann", "bob"} {
			ledgerMust(t, sp, "INSERT INTO platform.job_rows (job_id, user_id, ordinal, email_raw, email_norm, fields) VALUES ($1, $2, $3, $4, $4, $5::jsonb)",
				job, a, i+1, who+"@example.test", `{"first_name":"`+who+`"}`)
		}
		jobWalkTo(t, sp, job, status)
		return job
	}
	for _, s := range jobStates {
		s := s
		ended := jobIsTerminal(s)
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			job := build(sp, s)
			ledgerSwitchRole(t, sp, "platform_app")
			rowCases := []struct {
				what, set string
				ok        bool   // accepted in this status
				editState string // the refusal of a draft's or a previewed job's row (the guard lets it through, a CHECK of the row does not); "" when it is accepted there
				editWhy   string
			}{
				{"the merge data emptied", "fields = '{}'::jsonb", ended || editable[s], "", ""},
				{"the merge data changed to another value", `fields = '{"first_name":"changed"}'::jsonb`, editable[s], "", ""},
				{"the merge data emptied and the address changed beside it", "fields = '{}'::jsonb, email_raw = 'z@example.test'", editable[s], "", ""},
				{"the merge data emptied and the state changed beside it", "fields = '{}'::jsonb, state = 'duplicate'", false, "23514", "job_rows_duplicate_shape"},
				{"the merge data replaced by an array", "fields = '[]'::jsonb", false, "23514", "job_rows_fields_is_an_object"},
			}
			for _, c := range rowCases {
				sql := "UPDATE platform.job_rows SET " + c.set + " WHERE job_id = $1"
				switch {
				case c.ok:
					ledgerExpectOK(t, sp, fmt.Sprintf("%s, a %s job", c.what, s), sql, job)
				case editable[s] && c.editState != "": // the list is still the user's to edit: the guard of the list lets it through and the row's own CHECK answers
					ledgerExpectRefused(t, sp, fmt.Sprintf("%s, a %s job", c.what, s), c.editState, ledgerWhy{Constraint: c.editWhy}, sql, job)
				default:
					ledgerExpectRefused(t, sp, fmt.Sprintf("%s, a %s job", c.what, s), "23001", frozen, sql, job)
				}
			}
			docCases := []struct {
				what, set string
				ok        bool
			}{
				{"the template cleared", "frozen_settings = " + cleared, ended || editable[s]},
				{"the template cleared and reply_to changed", "frozen_settings = jsonb_set(" + cleared + ", '{content,reply_to}', '\"other@example.test\"')", editable[s]},
				{"the subject changed to another value", `frozen_settings = jsonb_set(frozen_settings, '{content,subject}', '"changed"')`, editable[s]},
				{"the text emptied and nothing else", `frozen_settings = jsonb_set(frozen_settings, '{content,text}', '""')`, editable[s]},
				{"the template cleared and a setting changed beside it", "frozen_settings = jsonb_set(" + cleared + ", '{settings,unknown_choice}', '\"include\"')", editable[s]},
				{"the whole document replaced", `frozen_settings = '{}'::jsonb`, editable[s]},
				{"the template cleared and the preview changed beside it", "frozen_settings = " + cleared + `, preview = '{"x":1}'::jsonb`, editable[s]},
			}
			for _, c := range docCases {
				sql := "UPDATE platform.jobs SET " + c.set + " WHERE id = $1"
				if c.ok {
					ledgerExpectOK(t, sp, fmt.Sprintf("%s, a %s job", c.what, s), sql, job)
				} else {
					ledgerExpectRefused(t, sp, fmt.Sprintf("%s, a %s job", c.what, s), "23001", frozenDoc, sql, job)
				}
			}
		})
	}

	t.Run("the administrator role may not clear anything, and the clearing of an ended job writes nothing else", func(t *testing.T) {
		ledgerTrial(t, tx, func(sp pgx.Tx) {
			job := build(sp, "succeeded")
			ledgerTrial(t, sp, func(sp2 pgx.Tx) {
				ledgerSwitchRole(t, sp2, "platform_admin")
				ledgerExpectRefused(t, sp2, "platform_admin empties the merge data", "42501", ledgerWhy{Message: "permission denied"}, "UPDATE platform.job_rows SET fields = '{}'::jsonb WHERE job_id = $1", job)
				ledgerExpectRefused(t, sp2, "platform_admin clears the template", "42501", ledgerWhy{Message: "permission denied"}, "UPDATE platform.jobs SET frozen_settings = "+cleared+" WHERE id = $1", job)
			})
			// the store's last statement as platform_app: the template and the mark together; a second time is a no-op for the document and the mark is set once
			ledgerSwitchRole(t, sp, "platform_app")
			ledgerExpectOK(t, sp, "the template and the mark in one statement", "UPDATE platform.jobs SET frozen_settings = "+cleared+", content_cleared_at = now() WHERE id = $1", job)
			ledgerExpectOK(t, sp, "control: the same statement's document, a second time, is no change", "UPDATE platform.jobs SET frozen_settings = "+cleared+" WHERE id = $1", job)
			if got := ledgerString(t, sp, "SELECT frozen_settings::text FROM platform.jobs WHERE id = $1", job); !strings.Contains(got, `"subject": "[removed]"`) || !strings.Contains(got, `"reply_to": "reply@example.test"`) || !strings.Contains(got, `"text": ""`) {
				t.Errorf("the cleared document is %s", got)
			}
			if n := ledgerInt64(t, sp, "SELECT count(*) FROM platform.job_rows WHERE job_id = $1 AND email_raw IN ('ann@example.test', 'bob@example.test')", job); n != 2 {
				t.Errorf("%d addresses of the 2 rows survive: the clearing touches the addresses", n)
			}
		})
	})

	t.Run("the grants are the ones platform_0012 and platform_0014 made: no new column of the rows", func(t *testing.T) {
		got := ledgerRows(t, tx, `SELECT string_agg(column_name::text, ',' ORDER BY column_name::text) FROM information_schema.column_privileges
			WHERE grantee = 'platform_app' AND table_schema = 'platform' AND table_name = 'job_rows' AND privilege_type = 'UPDATE'`)
		const want = "duplicate_of,email_norm,email_raw,fields,reason,source_item,state,submitted_at,suppressed"
		if len(got) != 1 || got[0] != want {
			t.Errorf("platform_app may update these columns of platform.job_rows: %v, want %s", got, want)
		}
	})
}

// retentionTemplateObjects is the CONTROL (present) and the ASSERTION (absent) of platform_0021's down (TestRunnerLifecycle): before it, the two guards carry the purge's branch (the merge data of a row of an ended job
// may become {}, the template of an ended job its placeholders), the retention index covers every ended job and the guards still refuse every other change; after it, the guards are the older texts (nothing about a row of an
// ended job changes but its stamp, no frozen column of an ended job changes) and the index is platform_0018's again.
func retentionTemplateObjects(t *testing.T, c *pgx.Conn, present bool, when string) {
	t.Helper()
	expect := lifecycleCounter(t, c, present, when)
	expect("the rows' guard that names the retention purge", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'job_rows_frozen' AND prosrc LIKE '%THE RETENTION PURGE%'")
	expect("the job guard that names the retention purge", 1,
		"SELECT count(*) FROM pg_proc WHERE pronamespace = 'platform'::regnamespace AND proname::text = 'jobs_guard' AND prosrc LIKE '%THE RETENTION PURGE%'")
	expect("the retention index that covers every ended job (no jsonb_exists in its predicate)", 1,
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname::text = 'jobs_content_retention_idx' AND indexdef NOT LIKE '%jsonb_exists%'")
	expect("the retention index of platform_0018 (limited to jobs with a campaign)", 0,
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname::text = 'jobs_content_retention_idx' AND indexdef LIKE '%jsonb_exists%'")
	if got := ledgerInt64(t, c, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'platform' AND indexname::text = 'jobs_content_retention_idx'"); got != 1 {
		t.Fatalf("%s: the retention index exists %d times, want 1 in either state", when, got)
	}
	tx, err := c.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ledgerSeed(t, tx)
	const doc = `{"settings":{"unknown_choice":""},"content":{"subject":"S","html":"H","text":"T","reply_to":"r@example.test"}}`
	const cleared = `jsonb_set(jsonb_set(jsonb_set(frozen_settings, '{content,subject}', '"[removed]"'::jsonb), '{content,html}', '"[removed]"'::jsonb), '{content,text}', '""'::jsonb)`
	job := "e7000000-0000-4000-8000-000000000001"
	ledgerMust(t, tx, "INSERT INTO platform.jobs (id, user_id, module_id, frozen_settings) VALUES ($1, $2, 'm2', $3::jsonb)", job, ledgerUserA, doc)
	ledgerMust(t, tx, "INSERT INTO platform.job_rows (job_id, user_id, ordinal, email_raw, email_norm, fields) VALUES ($1, $2, 1, 'a@example.test', 'a@example.test', '{\"k\":\"v\"}')", job, ledgerUserA)
	jobWalkTo(t, tx, job, "succeeded")
	frozen := ledgerWhy{Constraint: "job_rows_frozen"}
	const rows, emptied, other = "UPDATE platform.job_rows SET fields = %s WHERE job_id = $1", "'{}'::jsonb", `'{"k":"w"}'::jsonb`
	if present {
		ledgerExpectOK(t, tx, when+": the merge data of a row of an ended job emptied", fmt.Sprintf(rows, emptied), job)
		ledgerExpectOK(t, tx, when+": the template of an ended job cleared", "UPDATE platform.jobs SET frozen_settings = "+cleared+" WHERE id = $1", job)
	} else {
		ledgerExpectRefused(t, tx, when+": the merge data of a row of an ended job emptied", "23001", frozen, fmt.Sprintf(rows, emptied), job)
		ledgerExpectRefused(t, tx, when+": the template of an ended job cleared", "23001", ledgerWhy{Message: "frozen"}, "UPDATE platform.jobs SET frozen_settings = "+cleared+" WHERE id = $1", job)
	}
	// in either state every other change of an ended job is refused
	ledgerExpectRefused(t, tx, when+": the merge data of a row of an ended job changed to another value", "23001", frozen, fmt.Sprintf(rows, other), job)
	ledgerExpectRefused(t, tx, when+": the frozen document of an ended job replaced", "23001", ledgerWhy{Message: "frozen"}, "UPDATE platform.jobs SET frozen_settings = '{}'::jsonb WHERE id = $1", job)
}
