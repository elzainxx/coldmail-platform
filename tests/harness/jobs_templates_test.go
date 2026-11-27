//go:build harness

package harness

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S2, templates in jobs, against a real database (db-tests runs them on the runner): the syntax of a campaign is judged when the job is saved, the preview counts the rows that lack a merge field
// and shows sample messages that are never stored, the confirm leaves those rows out (free, not reserved, never given a module-facing item), the message of a row is the same bytes whoever writes it, a
// row whose rendered message the shared table refuses no longer kills the job, and a verify job has no templates. The pure half (the renderer, the footer, the arithmetic) is internal/jobs/template_test.go.
// Every store here is built WITH the template renderer (jobs.WithPreparer), as the service's is; the harness link maker is added by newJbsEnv.
//
//	SyntaxErrors...             a mistake in the subject, the html or the text is a *TemplateError naming the part, the line and the column and nothing is written; a store with only a Renderer accepts the same content
//	PreviewCounts...            the number of rows that lack a field (the suppression list first), the sorted names, the units and credits without them, up to three samples spread over the list, the samples
//	                            and the names in the stored document or not, the fingerprint when a value changes, an entirely skipped list
//	ConfirmLeavesOut...         the rows become missing_field with their names as the reason and a free P-09 result, are not reserved, never get an item, the replay says the same, and what the preview sampled is what the item holds
//	Unrenderable...             a blank subject or an oversize message is left out as unrenderable (P-10) and the rest of the job runs
//	SameBytes...                the item is what an independent renderer makes of the row, after a rolled-back EnsureJobItems, and the templates are compiled once per call
//	Reserved...                 a column named like a reserved field does not override it
//	VerifyJob...                module 1 has no templates and is not touched
//	TheStateVocabulary...       the two new states and their reasons at the table's own CHECKs

func newJtEnv(t *testing.T, opts ...jobs.Option) *jbsEnv {
	t.Helper()
	return newJbsEnv(t, append([]jobs.Option{jobs.WithPreparer(jobs.TemplateRenderer{})}, opts...)...)
}

// jtGreeting is the campaign most of the tests use: a field with no fallback in the subject and in the body, one with a fallback, no text template (the text is made from the HTML).
func jtGreeting() jobs.Content {
	return jobs.Content{Subject: "Hello {{first_name}}", HTML: "<p>Hi {{first_name}}, your plan is {{plan|free}}.</p>", ReplyTo: "reply@example.test"}
}

// jtRows is n distinct addresses t00001@example.test ... with the merge data fields(ordinal) gives (nil for none).
func jtRows(n int, fields func(i int) map[string]string) []jobs.RowIn {
	rows := jbsAddrs(n, "t")
	if fields != nil {
		for i := range rows {
			rows[i].Fields = fields(i + 1)
		}
	}
	return rows
}

func (e *jbsEnv) jtCreate(user, module string, rows []jobs.RowIn, c jobs.Content) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.CreateDraft(testCtx(e.t), user, jobs.CreateReq{Module: module, Rows: rows, Content: &c})
	if err != nil {
		e.t.Fatalf("CreateDraft(%s, %d rows): %v", module, len(rows), err)
	}
	return j
}

// jtLink is the unsubscribe link the harness stores make for an item.
func jtLink(id string) string {
	l, err := harnessUnsubSigner.LinkMaker(harnessPublicBase)(id)
	if err != nil {
		panic(err)
	}
	return l
}

// jtItem is what module 2 or 3 reads for an item: subject, html, text ("" for NULL) and the link.
type jtItem struct{ Subject, Body, Text, Link string }

func (e *jbsEnv) jtItem(id string) jtItem {
	e.t.Helper()
	var it jtItem
	if err := e.admin.QueryRow(testCtx(e.t), `SELECT rendered_subject, rendered_body, COALESCE(rendered_text, ''), COALESCE(unsubscribe_url, '') FROM public.job_items WHERE id = $1`, id).Scan(&it.Subject, &it.Body, &it.Text, &it.Link); err != nil {
		e.t.Fatalf("job item %s: %v", id, err)
	}
	return it
}

func (e *jbsEnv) jtItemCount(job string) int64 {
	e.t.Helper()
	return e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", job)
}

func (e *jbsEnv) jtFundedUser(module string) string {
	e.t.Helper()
	u := e.newUser()
	e.fund(u, module, 1_000_000)
	return u
}

// ---- syntax at save ------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsTemplates_SyntaxErrorsAreRefusedAtSaveWithTheirPositionAndNothingIsWritten(t *testing.T) {
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.newUser()
	before := e.writes()
	for _, c := range []struct {
		name         string
		content      jobs.Content
		part         string
		line, column int
	}{
		{"the subject", jobs.Content{Subject: "Hi {a|b", HTML: "<p>x</p>", Text: "x"}, "subject", 1, 4},
		{"the html, on its second line", jobs.Content{Subject: "S", HTML: "<style>\n  p } </style>"}, "html", 2, 5},
		{"the text, on its third line", jobs.Content{Subject: "S", HTML: "<p>x</p>", Text: "ok\n\n{only}"}, "text", 3, 1},
	} {
		content := c.content
		_, err := e.jobs.CreateDraft(ctx, user, jobs.CreateReq{Module: "m2", Rows: jbsAddrs(2, "s"), Content: &content})
		var te *jobs.TemplateError
		if !errors.As(err, &te) || !errors.Is(err, jobs.ErrBadRequest) {
			t.Errorf("%s: got %v, want a *TemplateError that is an ErrBadRequest", c.name, err)
			continue
		}
		if te.Part != c.part || te.Line != c.line || te.Column != c.column || te.Message == "" {
			t.Errorf("%s: %+v, want part %s at line %d column %d with a message", c.name, te, c.part, c.line, c.column)
		}
		if strings.Contains(err.Error(), "Hi {a|b") || strings.Contains(err.Error(), "<style>") || strings.Contains(err.Error(), "@") {
			t.Errorf("%s: the refusal quotes the user's text or an address: %v", c.name, err)
		}
	}
	e.requireWrites("the three refused saves", before) // not a job, not a row, not an event

	// the controls: the same content with its braces escaped is saved, and a store with only a Renderer (no template work at all) saves the mistaken content as it always did
	ok := jobs.Content{Subject: "Hi \\{a|b", HTML: "<style>\n  p \\} </style>", Text: "ok\n\n\\{only\\}"}
	if _, err := e.jobs.CreateDraft(ctx, user, jobs.CreateReq{Module: "m2", Rows: jbsAddrs(2, "s"), Content: &ok}); err != nil {
		t.Errorf("the escaped content was refused: %v", err)
	}
	plain := newJbsEnv(t) // the default Renderer: PlainRenderer
	bad := jobs.Content{Subject: "Hi {a|b", HTML: "<style>p } </style>"}
	if _, err := plain.jobs.CreateDraft(testCtx(t), plain.newUser(), jobs.CreateReq{Module: "m2", Rows: jbsAddrs(2, "s"), Content: &bad}); err != nil {
		t.Errorf("a store with only a Renderer refused content it does not read: %v", err)
	}
}

// ---- the preview -------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsTemplates_PreviewCountsTheMissingRowsListsTheNamesAndShowsSamplesThatAreNeverStored(t *testing.T) {
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.newUser()
	e.fund(user, "m2", 1000)
	rows := jtRows(12, func(i int) map[string]string {
		switch i {
		case 1:
			return map[string]string{"first_name": "Ann"}
		case 4: // no merge data at all: first_name is missing
			return nil
		case 5: // blank after trimming: missing
			return map[string]string{"first_name": "   "}
		case 6:
			return map[string]string{"first_name": "Dee", "plan": "pro"}
		case 7: // lacks first_name too, but is on the suppression list: counted as suppressed, whatever it lacks
			return nil
		}
		return map[string]string{"first_name": fmt.Sprintf("N%d", i)}
	})
	job := e.jtCreate(user, "m2", rows, jtGreeting())
	e.suppressFor(user, "t00007@example.test")
	p := e.preview(user, job.ID)

	if p.Rows != 12 || p.Pending != 12 || p.Suppressed != 1 || p.MissingFields != 2 || p.Unrenderable != 0 || p.Units != 9 {
		t.Fatalf("rows %d pending %d suppressed %d missing %d unrenderable %d units %d; want 12 12 1 2 0 9 (the list is applied first: the suppressed row is not counted as missing)", p.Rows, p.Pending, p.Suppressed, p.MissingFields, p.Unrenderable, p.Units)
	}
	if !reflect.DeepEqual(p.MissingFieldNames, []string{"first_name"}) || p.MissingFieldNamesTruncated {
		t.Errorf("names %v truncated %v, want [first_name] false (plan has a fallback and is never listed)", p.MissingFieldNames, p.MissingFieldNamesTruncated)
	}
	if want := int64(9) * int64(p.UnitCost); p.Credits != want || p.UnitCost == 0 {
		t.Errorf("credits %d, want the 9 rows that would be sent x %d = %d", p.Credits, p.UnitCost, want)
	}
	// the samples: the first, the middle and the last of the rows that would be sent (1 2 3 6 8 9 10 11 12: the rows 4, 5 and 7 are not among them)
	var ords []int
	for _, s := range p.Samples {
		ords = append(ords, s.Ordinal)
	}
	if !reflect.DeepEqual(ords, []int{1, 8, 12}) {
		t.Fatalf("sample ordinals %v, want [1 8 12]", ords)
	}
	ids := e.rowsOf(job.ID)
	s1 := p.Samples[0]
	if s1.Subject != "Hello Ann" || !strings.HasPrefix(s1.HTML, "<p>Hi Ann, your plan is free.</p>") || !strings.Contains(s1.Text, "Hi Ann, your plan is free.") {
		t.Errorf("the first sample is %+v, want what the templates say for Ann (plan falls back to free)", s1)
	}
	if strings.Count(s1.HTML, jtLink(ids[0].ID)) != 1 || strings.Count(s1.Text, jtLink(ids[0].ID)) != 1 || strings.Count(s1.HTML+s1.Text, "To stop receiving these messages") != 2 {
		t.Errorf("the first sample does not carry its own link and the footer once in each part: %+v", s1)
	}
	if p.Samples[1].Subject != "Hello N8" {
		t.Errorf("the middle sample's subject is %q, want Hello N8", p.Samples[1].Subject)
	}
	// nothing was written by the preview but the stored document: every row is still pending
	if got := e.stateCounts(job.ID); !reflect.DeepEqual(got, map[string]int{"pending": 12}) {
		t.Errorf("after the preview the rows are %v, want 12 pending (the preview counts, the confirm writes)", got)
	}

	// THE SAMPLES ARE NEVER STORED: not in the document of the job, not in what Get returns; the counts and the names are
	doc := e.str("SELECT preview::text FROM platform.jobs WHERE id = $1", job.ID)
	for _, secret := range []string{"Hello Ann", "Hello N8", "Hi Ann", "amples"} {
		if strings.Contains(doc, secret) {
			t.Errorf("the stored preview holds %q: %s", secret, doc)
		}
	}
	if got := e.str("SELECT (preview->'missing_field_names')::text FROM platform.jobs WHERE id = $1", job.ID); got != `["first_name"]` {
		t.Errorf("the stored names are %s, want [\"first_name\"]", got)
	}
	got, err := e.jobs.Get(ctx, user, job.ID)
	if err != nil || got.Preview == nil || len(got.Preview.Samples) != 0 || got.Preview.MissingFields != 2 || got.Preview.Units != 9 {
		t.Fatalf("Get: %+v %v; want the stored counts and no sample", got.Preview, err)
	}

	// THE FINGERPRINT: a second preview of the same job and list is the same (samples included), and a changed MERGE VALUE (not the count of missing rows) changes it, so that a confirm under the old one is stale
	p2 := e.preview(user, job.ID)
	if p2.Fingerprint != p.Fingerprint || !reflect.DeepEqual(p2.Samples, p.Samples) {
		t.Errorf("two previews of one job differ: fingerprints %s %s, samples equal %v", p.Fingerprint, p2.Fingerprint, reflect.DeepEqual(p2.Samples, p.Samples))
	}
	e.exec(`UPDATE platform.job_rows SET fields = '{"first_name":"Dee","plan":"team"}'::jsonb WHERE job_id = $1 AND ordinal = 6`, job.ID)
	p3 := e.preview(user, job.ID)
	if p3.Fingerprint == p.Fingerprint || p3.MissingFields != 2 {
		t.Errorf("after a value changed: fingerprint %s (was %s), missing %d; want another fingerprint and the same two missing rows", p3.Fingerprint, p.Fingerprint, p3.MissingFields)
	}
	_, err = e.tryConfirm(user, job.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "a confirm under the fingerprint from before the value changed", err, jobs.ErrStalePreview)
	if _, err = e.tryConfirm(user, job.ID, p3.Fingerprint, e.key(), nil); err != nil {
		t.Errorf("the control: a confirm under the current fingerprint was refused: %v", err)
	}
}

func TestJobsTemplates_AnEntirelySkippedListIsAValidPreviewAndConfirmRefusesItLeavingNothing(t *testing.T) {
	e := newJtEnv(t)
	user := e.newUser()
	e.fund(user, "m2", 1000)
	job := e.jtCreate(user, "m2", jtRows(3, nil), jtGreeting()) // not one row has first_name
	p := e.preview(user, job.ID)
	if p.MissingFields != 3 || p.Units != 0 || p.Credits != 0 || p.Shortfall != 0 || len(p.Samples) != 0 || !reflect.DeepEqual(p.MissingFieldNames, []string{"first_name"}) {
		t.Fatalf("an entirely skipped list previews as missing %d units %d credits %d shortfall %d samples %d names %v; want 3 0 0 0 0 [first_name]", p.MissingFields, p.Units, p.Credits, p.Shortfall, len(p.Samples), p.MissingFieldNames)
	}
	before := e.writes()
	_, err := e.tryConfirm(user, job.ID, p.Fingerprint, e.key(), nil)
	jbsIs(t, "the confirm of an entirely skipped list", err, jobs.ErrNothingToDo)
	e.requireWrites("the refused confirm (the skipping it had begun is rolled back with it)", before)
	if got := e.stateCounts(job.ID); !reflect.DeepEqual(got, map[string]int{"pending": 3}) {
		t.Errorf("after the refused confirm the rows are %v, want 3 pending", got)
	}
	if st := e.jobStatus(job.ID); st != "previewed" {
		t.Errorf("the job is %s, want it still previewed", st)
	}
	// the control: one row that has the field makes the same list confirmable
	e.exec(`UPDATE platform.job_rows SET fields = '{"first_name":"Ann"}'::jsonb WHERE job_id = $1 AND ordinal = 2`, job.ID)
	p = e.preview(user, job.ID)
	if p.MissingFields != 2 || p.Units != 1 {
		t.Fatalf("with one row complete: missing %d units %d, want 2 and 1", p.MissingFields, p.Units)
	}
	if r, err := e.tryConfirm(user, job.ID, p.Fingerprint, e.key(), nil); err != nil || r.Pending != 1 || r.MissingFields != 2 {
		t.Errorf("the control confirm: %+v %v, want one row sent and two left out", r, err)
	}
}

// ---- the confirm ------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsTemplates_ConfirmLeavesOutTheMissingRowsFreeAndNeverGivesThemAnItem(t *testing.T) {
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.newUser()
	e.fund(user, "m2", 3) // far too little: the first confirm is refused for credits, and what it began is rolled back
	rows := jtRows(10, func(i int) map[string]string {
		switch i {
		case 2, 5, 9:
			return map[string]string{"plan": "pro"} // no first_name: left out
		case 3:
			return map[string]string{"first_name": "Cy"} // complete, but on the suppression list
		}
		return map[string]string{"first_name": fmt.Sprintf("Name%d", i)}
	})
	job := e.jtCreate(user, "m2", rows, jtGreeting())
	e.suppressFor(user, "t00003@example.test")
	p := e.preview(user, job.ID)
	if p.Units != 6 || p.MissingFields != 3 || p.Suppressed != 1 {
		t.Fatalf("preview: units %d missing %d suppressed %d, want 6 3 1", p.Units, p.MissingFields, p.Suppressed)
	}

	// the control that the skipping is in the confirm's transaction: no credits, so the confirm fails AFTER the pass over the rows, and every row is as it was
	_, err := e.tryConfirm(user, job.ID, p.Fingerprint, e.key(), nil)
	var short jobs.ErrInsufficientCredits
	if !errors.As(err, &short) {
		t.Fatalf("a confirm with no credits: %v, want ErrInsufficientCredits", err)
	}
	if got := e.stateCounts(job.ID); !reflect.DeepEqual(got, map[string]int{"pending": 10}) || e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", job.ID) != 0 {
		t.Fatalf("after the refused confirm the rows are %v: the skipping was not rolled back with the confirm", got)
	}

	e.fund(user, "m2", 997) // 1000 in all
	key := e.key()
	res, err := e.tryConfirm(user, job.ID, p.Fingerprint, key, nil)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if res.Pending != 6 || res.Suppressed != 1 || res.MissingFields != 3 || res.Unrenderable != 0 || res.Reserved != 6*int64(p.UnitCost) || res.Replayed {
		t.Fatalf("confirm: %+v, want 6 pending, 1 suppressed, 3 missing, none unrenderable and exactly 6 rows' credits held", res)
	}
	if got := e.stateCounts(job.ID); !reflect.DeepEqual(got, map[string]int{"pending": 6, "suppressed": 1, "missing_field": 3}) {
		t.Errorf("states %v, want 6 pending, 1 suppressed, 3 missing_field", got)
	}
	// the results: free, the platform's own, rule P-09, the NAMES of the fields as the reason (never a value), and nothing charged
	want := []string{"2:missing_field:first_name:P-09:free:", "5:missing_field:first_name:P-09:free:", "9:missing_field:first_name:P-09:free:"}
	if got := e.rows(`SELECT ordinal || ':' || module_state || ':' || COALESCE(reason, '') || ':' || rule || ':' || decision || ':' || COALESCE(unit, '') FROM platform.job_results WHERE job_id = $1 AND rule = 'P-09' ORDER BY ordinal`, job.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("the P-09 results are %v, want %v", got, want)
	}
	if n := e.int(`SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND origin = 'platform' AND (reason ~ '(pro|Name|Cy)' OR detail::text ~ '(pro|Name|Cy)')`, job.ID); n != 0 {
		t.Errorf("%d result(s) of the platform name a value of a row", n)
	}
	if e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", job.ID) != 4 || e.chargeRows(job.ID) != 0 {
		t.Errorf("results %d (want 4: three missing, one suppressed), charges %d (want 0)", e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", job.ID), e.chargeRows(job.ID))
	}
	rs, err := e.jobs.Results(ctx, user, job.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs {
		if r.Ordinal == 2 {
			found = true
			if r.State != "missing_field" || r.Rule != "P-09" || r.Decision != "free" || r.Reason != "first_name" || r.Origin != "platform" || r.Unit != "" {
				t.Errorf("the result of row 2 as the store lists it: %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("row 2 has no result in %+v", rs)
	}
	// the reserve holds the six rows that go and nothing for the others
	if e.reserveRows(job.ID) != 1 || e.int("SELECT reserved_delta FROM platform.ledger WHERE job_id = $1 AND kind = 'reserve'", job.ID) != res.Reserved {
		t.Errorf("the reserve of the job is not the one the confirm reported (%d)", res.Reserved)
	}

	// NO ITEM: the module-facing table has nothing for the job before the engine writes it, and after it nothing for a row that was left out
	if n := e.jtItemCount(job.ID); n != 0 {
		t.Fatalf("%d job item(s) exist right after the confirm", n)
	}
	n, err := e.jobs.EnsureJobItems(ctx, job.ID)
	if err != nil || n != 6 {
		t.Fatalf("EnsureJobItems: %d, %v; want the 6 pending rows", n, err)
	}
	pendingIDs := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'pending' ORDER BY id", job.ID)
	if got := e.rows("SELECT id::text FROM public.job_items WHERE campaign_id = $1 ORDER BY id", job.ID); !reflect.DeepEqual(got, pendingIDs) {
		t.Errorf("the items are %v, want exactly the pending rows %v", got, pendingIDs)
	}
	if n := e.int(`SELECT count(*) FROM public.job_items i JOIN platform.job_rows w ON w.id = i.id WHERE w.job_id = $1 AND w.state <> 'pending'`, job.ID); n != 0 {
		t.Errorf("%d row(s) that were not pending have an item", n)
	}
	if n := e.int(`SELECT count(*) FROM public.job_items WHERE id IN (SELECT id FROM platform.job_rows WHERE job_id = $1 AND state = 'missing_field')`, job.ID); n != 0 {
		t.Errorf("%d row(s) left out as missing have an item", n)
	}

	// THE PREVIEW'S SAMPLE IS WHAT THE RECIPIENT GETS: byte for byte, and the footer is once in each part
	ids := e.rowsOf(job.ID)
	for _, s := range p.Samples {
		it := e.jtItem(ids[s.Ordinal-1].ID)
		if it.Subject != s.Subject || it.Body != s.HTML || it.Text != s.Text {
			t.Errorf("sample of row %d: %+v, the item holds %+v", s.Ordinal, s, it)
		}
	}
	if len(p.Samples) == 0 {
		t.Fatal("the preview had no sample")
	}
	for _, id := range pendingIDs {
		it := e.jtItem(id)
		if it.Link != jtLink(id) || strings.Count(it.Body, it.Link) != 1 || strings.Count(it.Text, it.Link) != 1 || strings.Count(it.Body, "To stop receiving") != 1 || strings.Count(it.Text, "To stop receiving") != 1 {
			t.Errorf("item %s: the footer is not once in each part with its own link: %+v", id, it)
		}
		if it.Text == "" || !strings.Contains(it.Body, "your plan is free") {
			t.Errorf("item %s has no generated text, or the fallback was not used: %+v", id, it)
		}
	}

	// A REPLAYED CONFIRM returns the same numbers and writes nothing
	again, err := e.tryConfirm(user, job.ID, p.Fingerprint, key, nil)
	if err != nil || !again.Replayed || again.Pending != res.Pending || again.Suppressed != res.Suppressed || again.MissingFields != res.MissingFields || again.Unrenderable != res.Unrenderable || again.Reserved != res.Reserved {
		t.Errorf("the replay: %+v %v, want the numbers of %+v", again, err, res)
	}
	e.requireBooks("after the confirm that left rows out")
}

func TestJobsTemplates_ARowWhoseMessageTheTableRefusesIsLeftOutAndTheJobStillRuns(t *testing.T) {
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.jtFundedUser("m2")
	content := jobs.Content{Subject: "{{s|}}", HTML: "<p>" + strings.Repeat("{{big|}}", 300) + "</p>", ReplyTo: "reply@example.test"}
	rows := jtRows(4, func(i int) map[string]string {
		switch i {
		case 1:
			return map[string]string{"s": "Hi", "big": "a"}
		case 2:
			return map[string]string{"big": "a"} // the subject renders blank: {{s|}} has a fallback, so the field is not missing, and the table refuses a blank subject
		case 3:
			return map[string]string{"s": "Hi", "big": strings.Repeat("b", 4000)} // 300 x 4000 bytes: over a MiB
		}
		return map[string]string{"s": "Hello", "big": "c"}
	})
	job := e.jtCreate(user, "m2", rows, content)
	p := e.preview(user, job.ID)
	if p.Unrenderable != 2 || p.MissingFields != 0 || p.Units != 2 {
		t.Fatalf("preview: unrenderable %d missing %d units %d, want 2 0 2", p.Unrenderable, p.MissingFields, p.Units)
	}
	var ords []int
	for _, s := range p.Samples {
		ords = append(ords, s.Ordinal)
	}
	if !reflect.DeepEqual(ords, []int{1, 4}) {
		t.Errorf("sample ordinals %v, want [1 4]: a row the table refuses is never a sample", ords)
	}
	res := e.confirm(user, job.ID, p.Fingerprint)
	if res.Pending != 2 || res.Unrenderable != 2 || res.MissingFields != 0 || res.Reserved != 2*int64(p.UnitCost) {
		t.Fatalf("confirm: %+v, want 2 pending, 2 unrenderable and the credits of 2 rows", res)
	}
	want := []string{"2:unrenderable:subject_blank:P-10:free:", "3:unrenderable:html_too_large:P-10:free:"}
	if got := e.rows(`SELECT ordinal || ':' || module_state || ':' || COALESCE(reason, '') || ':' || rule || ':' || decision || ':' || COALESCE(unit, '') FROM platform.job_results WHERE job_id = $1 AND rule = 'P-10' ORDER BY ordinal`, job.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("the P-10 results are %v, want %v", got, want)
	}
	// the engine's write of the items, which used to fail the WHOLE job on the first refused row, now writes the two that can be sent
	n, err := e.jobs.EnsureJobItems(ctx, job.ID)
	if err != nil || n != 2 {
		t.Fatalf("EnsureJobItems: %d, %v; want the 2 renderable rows", n, err)
	}
	if got := e.rows(`SELECT w.ordinal::text FROM public.job_items i JOIN platform.job_rows w ON w.id = i.id WHERE i.campaign_id = $1 ORDER BY w.ordinal`, job.ID); !reflect.DeepEqual(got, []string{"1", "4"}) {
		t.Errorf("the items are of rows %v, want 1 and 4", got)
	}
	e.requireBooks("after the confirm that left unrenderable rows out")
}

// ---- the same bytes ------------------------------------------------------------------------------------------------------------------------------------------------------------------

// jtCounting counts how often the store asks for a campaign: the save check asks with no job id, every other use with the job's.
type jtCounting struct{ saves, uses *atomic.Int64 }

func (c jtCounting) Prepare(jobID string, content jobs.Content) (jobs.PreparedCampaign, error) {
	if jobID == "" {
		c.saves.Add(1)
	} else {
		c.uses.Add(1)
	}
	return jobs.TemplateRenderer{}.Prepare(jobID, content)
}

// jtFlaky is the template renderer whose rendering of one row fails while the switch is on: the way a test makes an EnsureJobItems roll back half way.
type jtFlaky struct {
	fail    *atomic.Bool
	ordinal int
}

func (f jtFlaky) Prepare(jobID string, content jobs.Content) (jobs.PreparedCampaign, error) {
	in, err := jobs.TemplateRenderer{}.Prepare(jobID, content)
	if err != nil || jobID == "" {
		return in, err
	}
	return jtFlakyCampaign{PreparedCampaign: in, f: f}, nil
}

type jtFlakyCampaign struct {
	jobs.PreparedCampaign
	f jtFlaky
}

func (c jtFlakyCampaign) Render(row jobs.EngineRow) (string, string, string, error) {
	if c.f.fail.Load() && row.Ordinal == c.f.ordinal {
		return "", "", "", errors.New("the renderer failed on purpose")
	}
	return c.PreparedCampaign.Render(row)
}

func TestJobsTemplates_TheMessageIsTheSameBytesAfterARolledBackEnsureAndFromAnIndependentRenderer(t *testing.T) {
	var fail atomic.Bool
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.jtFundedUser("m2")
	g := "{a|b|c|d|e|f|g|h}"
	content := jobs.Content{Subject: "S " + g + g + g + g, HTML: "<p>" + g + g + g + g + " {{n}}</p>", Text: g + g + g + g + " {{n}}", ReplyTo: "reply@example.test"}
	job := e.jtCreate(user, "m2", jtRows(12, func(i int) map[string]string { return map[string]string{"n": fmt.Sprintf("n%d", i)} }), content)
	res := e.confirm(user, job.ID, e.preview(user, job.ID).Fingerprint)
	if res.Pending != 12 {
		t.Fatalf("confirm: %+v", res)
	}
	// an EnsureJobItems that fails at row 7 writes NOTHING (the whole call is one transaction) and records nothing
	flaky, err := jobs.New(e.pool, e.store, harnessUnsubscribeOption(), jobs.WithPreparer(jtFlaky{fail: &fail, ordinal: 7}))
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	_, err = flaky.EnsureJobItems(ctx, job.ID)
	var br *jobs.BadRequestError
	if !errors.As(err, &br) || !strings.Contains(br.Reason, "row 7") || strings.Contains(err.Error(), "on purpose") {
		t.Fatalf("EnsureJobItems with a failing renderer: %v, want ErrBadRequest naming row 7 and not the renderer's text", err)
	}
	if e.jtItemCount(job.ID) != 0 || e.int("SELECT (items_ensured_at IS NOT NULL)::int FROM platform.jobs WHERE id = $1", job.ID) != 0 {
		t.Fatalf("the failed call left %d item(s) or marked the job: it was not rolled back", e.jtItemCount(job.ID))
	}
	// the same call made again, by another store instance, writes all twelve
	fail.Store(false)
	if n, err := e.jobs.EnsureJobItems(ctx, job.ID); err != nil || n != 12 {
		t.Fatalf("EnsureJobItems after the rollback: %d, %v", n, err)
	}
	// every item is what an INDEPENDENT renderer (a third instance, here in the test) makes of the same job and row, byte for byte
	indep, err := jobs.TemplateRenderer{}.Prepare(job.ID, content)
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for _, r := range e.rowsOf(job.ID) {
		row := jobs.EngineRow{ID: r.ID, Ordinal: r.Ordinal, EmailNorm: r.Norm, Fields: map[string]string{"n": fmt.Sprintf("n%d", r.Ordinal)}, UnsubscribeURL: jtLink(r.ID)}
		subject, body, text, err := indep.Render(row)
		if err != nil {
			t.Fatal(err)
		}
		it := e.jtItem(r.ID)
		if it.Subject != subject || it.Body != body || it.Text != text {
			t.Errorf("row %d: the item is %+v, an independent render says %q %q %q", r.Ordinal, it, subject, body, text)
		}
		subjects[subject] = true
	}
	// and the spin follows the row: twelve rows of one job spin differently (four groups of eight: 4096 messages), the control of the equality above
	if len(subjects) < 9 {
		t.Errorf("twelve rows of one job gave %d distinct subjects: the row is not in the seed", len(subjects))
	}
}

func TestJobsTemplates_TheTemplatesAreCompiledOncePerCallWhateverThePagesAndRows(t *testing.T) {
	saves, uses := new(atomic.Int64), new(atomic.Int64)
	e := newJtEnv(t, jobs.WithPreparer(jtCounting{saves: saves, uses: uses}))
	ctx := testCtx(t)
	user := e.jtFundedUser("m2")
	// 5001 rows: two pages of the engine's paging (rowBatch is 5000)
	content := jobs.Content{Subject: "S {{n|x}}", HTML: "<p>{{n|x}}</p>", ReplyTo: "reply@example.test"}
	job := e.jtCreate(user, "m2", jbsAddrs(5001, "p"), content)
	if saves.Load() != 1 || uses.Load() != 0 {
		t.Fatalf("after the create: %d save check(s) and %d use(s), want 1 and 0", saves.Load(), uses.Load())
	}
	p := e.preview(user, job.ID)
	if uses.Load() != 1 {
		t.Errorf("the preview of 5001 rows prepared the campaign %d times, want once", uses.Load())
	}
	e.confirm(user, job.ID, p.Fingerprint)
	if uses.Load() != 2 {
		t.Errorf("the confirm of 5001 rows prepared the campaign %d more times, want once", uses.Load()-1)
	}
	uses.Store(0)
	if n, err := e.jobs.EnsureJobItems(ctx, job.ID); err != nil || n != 5001 {
		t.Fatalf("EnsureJobItems: %d, %v", n, err)
	}
	if uses.Load() != 1 {
		t.Errorf("EnsureJobItems of 5001 rows (two pages) prepared the campaign %d times, want once", uses.Load())
	}
	// the control: EnsureModuleItems is a call of its own and prepares once for the ids it is given
	uses.Store(0)
	ids := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 ORDER BY ordinal LIMIT 3", job.ID)
	if n, err := e.jobs.EnsureModuleItems(ctx, job.ID, ids); err != nil || n != 0 || uses.Load() != 1 {
		t.Errorf("EnsureModuleItems of three rows that have items: %d new, %v, %d use(s); want 0, nil and one", n, err, uses.Load())
	}
}

// ---- reserved names, module 1 ------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsTemplates_AColumnNamedLikeAReservedFieldDoesNotOverrideIt(t *testing.T) {
	e := newJtEnv(t)
	ctx := testCtx(t)
	user := e.jtFundedUser("m2")
	content := jobs.Content{Subject: "S {{name}}", HTML: "<p>{{email}} {{unsubscribe_url}} {{name}}</p>", Text: "{{email}} {{unsubscribe_url}}", ReplyTo: "reply@example.test"}
	rows := jtRows(2, func(i int) map[string]string {
		return map[string]string{"email": "evil@example.test", "unsubscribe_url": "https://evil.example.test/u", "name": "Ann"}
	})
	job := e.jtCreate(user, "m2", rows, content)
	e.confirm(user, job.ID, e.preview(user, job.ID).Fingerprint)
	if n, err := e.jobs.EnsureJobItems(ctx, job.ID); err != nil || n != 2 {
		t.Fatalf("EnsureJobItems: %d, %v", n, err)
	}
	for _, r := range e.rowsOf(job.ID) {
		it := e.jtItem(r.ID)
		all := it.Subject + it.Body + it.Text
		if strings.Contains(all, "evil") {
			t.Errorf("row %d: a column overrode a reserved field: %+v", r.Ordinal, it)
		}
		if !strings.Contains(it.Body, r.Norm) || strings.Count(it.Body, jtLink(r.ID)) != 1 || !strings.Contains(it.Text, r.Norm+" "+jtLink(r.ID)) || strings.Contains(all, "To stop receiving") {
			t.Errorf("row %d: the reserved values are not the row's (and the template placed the link itself, so no footer): %+v", r.Ordinal, it)
		}
		if it.Subject != "S Ann" || !strings.Contains(it.Body, " Ann") { // the control: an ordinary column is used
			t.Errorf("row %d: the ordinary column was not used: %+v", r.Ordinal, it)
		}
	}
}

func TestJobsTemplates_AVerifyJobHasNoTemplatesAndIsNotTouched(t *testing.T) {
	e := newJtEnv(t)
	user := e.newUser()
	e.fund(user, "m1", 1000)
	// rows whose merge data looks like a template and lacks everything: a verify job reads none of it
	rows := jtRows(5, func(i int) map[string]string { return map[string]string{"x": "{{y}}", "z": "{a|b"} })
	rows[3].Fields = nil
	job, err := e.jobs.CreateDraft(testCtx(t), user, jobs.CreateReq{Module: "m1", Rows: rows})
	if err != nil {
		t.Fatalf("CreateDraft(m1): %v", err)
	}
	p := e.preview(user, job.ID)
	if p.MissingFields != 0 || p.Unrenderable != 0 || len(p.MissingFieldNames) != 0 || len(p.Samples) != 0 || p.Units != 5 || p.Pending != 5 {
		t.Fatalf("a verify job's preview: missing %d unrenderable %d names %v samples %d units %d pending %d; want 0 0 [] 0 5 5", p.MissingFields, p.Unrenderable, p.MissingFieldNames, len(p.Samples), p.Units, p.Pending)
	}
	if p.MissingFieldNames == nil {
		t.Error("the names of a verify job's preview are nil, not an empty list")
	}
	res := e.confirm(user, job.ID, p.Fingerprint)
	if res.Pending != 5 || res.MissingFields != 0 || res.Unrenderable != 0 || res.Reserved != 5*int64(p.UnitCost) {
		t.Errorf("a verify job's confirm: %+v, want all five rows sent to module 1 and none left out", res)
	}
	if got := e.stateCounts(job.ID); !reflect.DeepEqual(got, map[string]int{"pending": 5}) {
		t.Errorf("states %v, want 5 pending", got)
	}
	if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1 AND rule IN ('P-09', 'P-10')", job.ID); n != 0 {
		t.Errorf("%d template result(s) in a verify job", n)
	}
	// the control: the same kind of list in a send job whose template needs a field IS left out
	su := e.jtFundedUser("m2")
	send := e.jtCreate(su, "m2", jtRows(5, func(i int) map[string]string { return map[string]string{"z": "q"} }), jtGreeting())
	if sp := e.preview(su, send.ID); sp.MissingFields != 5 {
		t.Errorf("the control send job: missing %d, want 5", sp.MissingFields)
	}
}

// ---- the table's own CHECKs ------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestJobsTemplates_TheStateVocabularyAndTheReasonRuleAtTheTable(t *testing.T) {
	e := newJtEnv(t)
	user := e.newUser()
	job := e.jtCreate(user, "m2", jtRows(1, nil), jtGreeting())
	tx, err := e.admin.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(testCtx(t)) }()
	const ins = `INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, reason, submitted_at, suppressed)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $4, $5, $6, $7, $8)`
	n := 100
	next := func() (int, string) { n++; return n, fmt.Sprintf("x%d@example.test", n) }
	try := func(state string, reason any, submitted any, suppressed bool) []any {
		o, a := next()
		return []any{job.ID, user, o, a, state, reason, submitted, suppressed}
	}
	// the controls: both new states are accepted with a reason, and a pending row needs none (the rule names the states that do)
	ledgerExpectOK(t, tx, "a missing_field row with its field names", ins, try("missing_field", "first_name,plan", nil, false)...)
	ledgerExpectOK(t, tx, "an unrenderable row with its word", ins, try("unrenderable", "subject_blank", nil, false)...)
	ledgerExpectOK(t, tx, "a pending row with no reason", ins, try("pending", nil, nil, false)...)
	// the refusals, each for its own reason: no reason (the NULL case), a blank reason, a state that is none of the seven, a submitted row that is not pending, a flag on a row that is neither pending nor suppressed
	for _, s := range []string{"missing_field", "unrenderable"} {
		ledgerExpectRefused(t, tx, "a "+s+" row with no reason", "23514", ledgerWhy{Constraint: "job_rows_reason_shape"}, ins, try(s, nil, nil, false)...)
		ledgerExpectRefused(t, tx, "a "+s+" row with a blank reason", "23514", ledgerWhy{Constraint: "job_rows_reason_shape"}, ins, try(s, "   ", nil, false)...)
		ledgerExpectRefused(t, tx, "a "+s+" row that was submitted", "23514", ledgerWhy{Constraint: "job_rows_only_pending_is_submitted"}, ins, try(s, "why", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false)...)
		ledgerExpectRefused(t, tx, "a "+s+" row that carries the suppression flag", "23514", ledgerWhy{Constraint: "job_rows_flag_only_on_listable_rows"}, ins, try(s, "why", nil, true)...)
	}
	ledgerExpectRefused(t, tx, "a state that is none of the seven", "23514", ledgerWhy{Constraint: "job_rows_state_vocabulary"}, ins, try("skipped", "why", nil, false)...)
}
