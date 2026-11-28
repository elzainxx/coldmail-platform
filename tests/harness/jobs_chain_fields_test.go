//go:build harness

package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 6: a CHAINED send keeps the merge columns of the verification list it was made from, and a job lists the NAMES of its merge data. Against a real database, over the whole stack (newJaaEnv: the API
// beside the real stores, the job store rendering through the template renderer as the service's does). Names start with `jcf`.
//
//	TheSourceRowsMergeData...   a verify job made from a CSV with first_name and city, a send chained to it: each selected row holds the fields of ITS source row (the join is in the query that reads the results),
//	                            an invalid row holds {}, an unknown row holds its data whatever the user's choice (the choice flips its state, not its data); the preview counts the row that lacks first_name,
//	                            the samples render the carried values, a changed carried value is a changed fingerprint (and an unchanged preview is the same fingerprint); merge_fields is the CSV header
//	                            minus the address column for both jobs, in the store's job and in the API's JSON; another user's job as the source is refused and writes nothing
//	TheRetentionClearsTheValues the purge empties the values of both jobs and clears the template of the send, and keeps merge_fields and every other byte of the frozen document; a chained send made from
//	                            the cleared source afterwards carries {} and still lists the names; a job younger than the dial keeps its values

const jcfCSV = "email,first_name,city\n" +
	"ann@example.test,Ann,Oslo\n" +
	"bob@example.test,Bob,\n" +
	"cy@example.test,Cy,Rome\n" +
	"dee@example.test,,Paris\n" +
	"eve@example.test,Eve,\n" +
	"fay@example.test,,\n"

// the verdicts module 1 gave each row of jcfCSV, in order
var jcfVerdicts = []string{jobs.VerdictValid, jobs.VerdictValid, jobs.VerdictInvalid, jobs.VerdictUnknown, jobs.VerdictRisky, jobs.VerdictValid}

// the merge data each row of jcfCSV holds as the platform stores it (an empty cell is an empty value)
var jcfSourceFields = []string{
	`{"first_name": "Ann", "city": "Oslo"}`,
	`{"first_name": "Bob", "city": ""}`,
	`{"first_name": "Cy", "city": "Rome"}`,
	`{"first_name": "", "city": "Paris"}`,
	`{"first_name": "Eve", "city": ""}`,
	`{"first_name": "", "city": ""}`,
}

func jcfTemplate() *jobs.Content {
	return &jobs.Content{Subject: "Hello {{first_name}}", HTML: "<p>Hi {{first_name}} of {{city|nowhere}}</p>", ReplyTo: "reply@example.test"}
}

// jcfHolds says whether the job's row at the ordinal holds exactly this merge data (jsonb equality: no spacing or key order involved).
func (e *japEnv) jcfHolds(job string, ordinal int, want string) bool {
	e.t.Helper()
	return e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND ordinal = $2 AND fields = $3::jsonb", job, ordinal, want) == 1
}

// jcfSource makes the verify job from jcfCSV through the API, finishes it the way the engine would (confirmed through the store, the results written as the superuser, walked to succeeded) and returns its id.
func (e *japEnv) jcfSource(user string) string {
	e.t.Helper()
	id := jobIDOf(e.t, e.callAs(user, "POST", "/v1/jobs?module=m1", "text/csv", []byte(jcfCSV)), 201)
	e.fund(user, "m1", 100)
	e.confirm(user, id, e.preview(user, id).Fingerprint)
	for i, v := range jcfVerdicts {
		e.exec(`INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule)
			SELECT r.job_id, r.user_id, r.id, r.ordinal, r.email_norm, 'module', $3, 'free', 'M1-19' FROM platform.job_rows r WHERE r.job_id = $1 AND r.ordinal = $2`, id, i+1, v)
	}
	jobWalkTo(e.t, e.admin, id, "succeeded")
	return id
}

func (e *japEnv) jcfChain(user, source, choice string) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.CreateDraft(testCtx(e.t), user, jobs.CreateReq{Module: "m2", Content: jcfTemplate(), SourceJobID: source, Settings: jobs.Settings{UnknownChoice: choice}})
	if err != nil {
		e.t.Fatalf("CreateDraft(chained to %s): %v", source, err)
	}
	return j
}

func jcfNames(v any) string { return fmt.Sprint(v) }

func TestChainFields_ASendChainedToAVerifyListKeepsTheMergeColumnsOfItsSourceRows(t *testing.T) {
	e := newJaaEnv(t)
	ctx := testCtx(t)
	A, B := mnyUserA, mnyUserB
	src := e.jcfSource(A)

	// the names: the CSV's header minus the address column, in the store's job and in what the API says of it
	srcJob, err := e.jobs.Get(ctx, A, src)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(srcJob.MergeFields); got != "[city first_name]" {
		t.Fatalf("the verify job's merge fields are %s, want [city first_name] (the header minus the address column, sorted)", got)
	}
	if got := jcfNames(dig(e.json(A, "GET", "/v1/jobs/"+src, nil).obj(t, 200), "job", "merge_fields")); got != "[city first_name]" {
		t.Fatalf("the API says the verify job's merge fields are %s", got)
	}
	if e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid", src) != 6 {
		t.Fatal("the fixture: the source job does not hold the six rows of the CSV")
	}
	for i, want := range jcfSourceFields {
		if !e.jcfHolds(src, i+1, want) {
			t.Fatalf("the fixture: source row %d does not hold %s", i+1, want)
		}
	}

	chain := e.jcfChain(A, src, "")
	if got := fmt.Sprint(chain.MergeFields); got != "[city first_name]" {
		t.Errorf("the chained send's merge fields are %s, want the source job's", got)
	}
	if got := jcfNames(dig(e.json(A, "GET", "/v1/jobs/"+chain.ID, nil).obj(t, 200), "job", "merge_fields")); got != "[city first_name]" {
		t.Errorf("the API says the chained send's merge fields are %s", got)
	}
	// EACH ROW HOLDS THE FIELDS OF ITS SOURCE ROW: the selected ones and the unknown one (which the user's choice may still make a recipient); the invalid one holds nothing, which is the control
	// that the chain does not copy everything
	for i, want := range jcfSourceFields {
		ordinal := i + 1
		if jcfVerdicts[i] == jobs.VerdictInvalid {
			want = `{}`
		}
		if !e.jcfHolds(chain.ID, ordinal, want) {
			t.Errorf("chained row %d (%s) does not hold %s", ordinal, jcfVerdicts[i], want)
		}
	}
	// the same rows hold the same data as the source in ONE comparison, so a mismatch of the join (a row given another row's data) cannot hide in the table above
	if n := e.int(`SELECT count(*) FROM platform.job_rows c JOIN platform.job_rows s ON s.id = c.source_item AND s.job_id = $2::uuid
		WHERE c.job_id = $1::uuid AND c.state IN ('pending', 'excluded') AND c.reason IS DISTINCT FROM 'invalid' AND c.fields = s.fields`, chain.ID, src); n != 5 {
		t.Errorf("%d chained rows hold exactly their own source row's data, want the 5 that are or can become recipients", n)
	}

	// THE PREVIEW: the template needs first_name with no fallback. Ann, Bob, Eve and Fay are selected; Fay has none, so ONE row is missing it (before the data was carried every pending row was)
	p0 := e.preview(A, chain.ID)
	if p0.Pending != 4 || p0.MissingFields != 1 || fmt.Sprint(p0.MissingFieldNames) != "[first_name]" || p0.Units != 3 {
		t.Fatalf("the preview says pending %d, missing %d %v, units %d; want 4 pending, 1 missing [first_name], 3 units", p0.Pending, p0.MissingFields, p0.MissingFieldNames, p0.Units)
	}
	if len(p0.Samples) != 3 || p0.Samples[0].Ordinal != 1 || p0.Samples[1].Ordinal != 2 || p0.Samples[2].Ordinal != 5 {
		t.Fatalf("the samples are %+v, want the rows that would be sent: 1 (Ann), 2 (Bob) and 5 (Eve)", p0.Samples)
	}
	for i, want := range []struct{ subject, html string }{{"Hello Ann", "Hi Ann of Oslo"}, {"Hello Bob", "Hi Bob of nowhere"}, {"Hello Eve", "Hi Eve of nowhere"}} {
		if s := p0.Samples[i]; s.Subject != want.subject || !strings.Contains(s.HTML, want.html) {
			t.Errorf("sample %d renders %q / %q, want the carried values: %q / ...%q...", i+1, s.Subject, s.HTML, want.subject, want.html)
		}
	}
	// the choice for the unknown row flips its STATE and keeps its DATA: Dee becomes a recipient with her city and no first name, so a second row is missing the field
	p1 := e.previewChoice(A, chain.ID, jobs.UnknownInclude)
	if p1.Pending != 5 || p1.MissingFields != 2 || p1.Fingerprint == p0.Fingerprint {
		t.Fatalf("with the unknown row included: pending %d, missing %d, fingerprint changed %v; want 5, 2 and a new fingerprint", p1.Pending, p1.MissingFields, p1.Fingerprint != p0.Fingerprint)
	}
	if !e.jcfHolds(chain.ID, 4, jcfSourceFields[3]) || e.str("SELECT state FROM platform.job_rows WHERE job_id = $1::uuid AND ordinal = 4", chain.ID) != "pending" {
		t.Error("the unknown row lost its data, or did not become pending, when the user chose to include it")
	}
	// the fingerprint names the carried data: asked again it is the same, a changed carried value is a new one and the samples render it
	p2 := e.preview(A, chain.ID)
	if p2.Fingerprint != p1.Fingerprint {
		t.Fatal("an unchanged list has two fingerprints: the control of the next assertion cannot tell a change from noise")
	}
	e.exec(`UPDATE platform.job_rows SET fields = jsonb_set(fields, '{first_name}', '"Anne"') WHERE job_id = $1::uuid AND ordinal = 1`, chain.ID)
	p3 := e.preview(A, chain.ID)
	if p3.Fingerprint == p2.Fingerprint {
		t.Error("a changed merge value did not change the fingerprint: a stale preview would be confirmed")
	}
	if len(p3.Samples) == 0 || p3.Samples[0].Subject != "Hello Anne" {
		t.Errorf("the samples after the change: %+v, want the new value rendered", p3.Samples)
	}
	// the control of the whole: a send chained to a source whose rows have NO merge data misses the field on every pending row, so the carried values above are what changed the count
	plain := e.createRows(A, "m1", jbsAddrs(3, "plain"))
	e.confirm(A, plain.ID, e.preview(A, plain.ID).Fingerprint)
	for i := 1; i <= 3; i++ {
		e.exec(`INSERT INTO platform.job_results (job_id, user_id, item_ref, ordinal, email, origin, module_state, decision, rule)
			SELECT r.job_id, r.user_id, r.id, r.ordinal, r.email_norm, 'module', 'valid', 'free', 'M1-19' FROM platform.job_rows r WHERE r.job_id = $1 AND r.ordinal = $2`, plain.ID, i)
	}
	jobWalkTo(t, e.admin, plain.ID, "succeeded")
	plainChain := e.jcfChain(A, plain.ID, "")
	if pp := e.preview(A, plainChain.ID); pp.Pending != 3 || pp.MissingFields != 3 || len(plainChain.MergeFields) != 0 {
		t.Errorf("a send chained to a list with no merge data: pending %d, missing %d, merge fields %v; want 3, 3 and none", pp.Pending, pp.MissingFields, plainChain.MergeFields)
	}

	// ANOTHER USER'S JOB AS THE SOURCE is refused as a job that does not exist, nothing is written, and the answer holds none of the list
	jobsBefore, rowsBefore := e.int("SELECT count(*) FROM platform.jobs"), e.int("SELECT count(*) FROM platform.job_rows")
	_, err = e.jobs.CreateDraft(ctx, B, jobs.CreateReq{Module: "m2", Content: jcfTemplate(), SourceJobID: src})
	jbsBadRequest(t, "a chained send of another user's verify job", err, "the source job does not exist")
	resp := e.json(B, "POST", "/v1/jobs", map[string]any{"module": "m2", "source_job_id": src, "content": map[string]any{"subject": "Hello {{first_name}}", "html": "<p>x</p>"}})
	resp.refused(t, 400, "validation_failed")
	if strings.Contains(string(resp.Body), "first_name") || strings.Contains(string(resp.Body), "Ann") || strings.Contains(string(resp.Body), "ann@") {
		t.Errorf("the refusal tells another user something of the list: %s", resp.Body)
	}
	if e.int("SELECT count(*) FROM platform.jobs") != jobsBefore || e.int("SELECT count(*) FROM platform.job_rows") != rowsBefore {
		t.Error("a refused chained send wrote a job or rows")
	}
	e.json(B, "GET", "/v1/jobs/"+chain.ID, nil).refused(t, 404, "not_found") // and the chained send of A is not B's to read, names included
}

func TestChainFields_TheRetentionClearsTheValuesAndKeepsTheNamesOfTheMergeFields(t *testing.T) {
	e := newJaaEnv(t)
	ctx := testCtx(t)
	A := mnyUserA
	src := e.jcfSource(A)
	old := e.jcfChain(A, src, "")
	fresh := e.jcfChain(A, src, "")
	e.preview(A, old.ID)
	for _, id := range []string{old.ID, fresh.ID} {
		if _, err := e.jobs.DiscardDraft(ctx, A, id); err != nil { // an ended send job (cancelled), the quickest way to a terminal one that still holds its rows
			t.Fatalf("DiscardDraft(%s): %v", id, err)
		}
	}
	values := func(ids ...string) int64 {
		return e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = ANY($1::uuid[]) AND fields <> '{}'::jsonb", ids)
	}
	namesKept := func(id string) bool {
		return e.int("SELECT count(*) FROM platform.jobs WHERE id = $1::uuid AND frozen_settings -> 'merge_fields' = '[\"city\", \"first_name\"]'::jsonb", id) == 1
	}
	// BEFORE: the values are there for the three jobs and the names are frozen with each
	if values(src, old.ID, fresh.ID) == 0 || values(old.ID) == 0 || !namesKept(src) || !namesKept(old.ID) || !namesKept(fresh.ID) {
		t.Fatalf("the fixture: values %d (send %d), names frozen: %v %v %v", values(src, old.ID, fresh.ID), values(old.ID), namesKept(src), namesKept(old.ID), namesKept(fresh.ID))
	}
	frozenBefore := map[string]string{src: e.jrtFrozen(src), old.ID: e.jrtFrozen(old.ID), fresh.ID: e.jrtFrozen(fresh.ID)}
	freshValues := values(fresh.ID)

	// THE GUARD (platform_0021) COMPARES WHOLE DOCUMENTS, so a key it has never heard of must neither break the cleared form nor be free to change: as platform_app, on the ended job that is not old enough for the
	// purge, in a transaction that is rolled back each time. The template cleared with merge_fields kept is allowed; the same without the key, with another list in it, or the key changed alone are refused (23001)
	const cleared = `jsonb_set(jsonb_set(jsonb_set(frozen_settings, '{content,subject}', '"[removed]"'::jsonb), '{content,html}', '"[removed]"'::jsonb), '{content,text}', '""'::jsonb)`
	try := func(set string) error {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // a rolled-back probe
		_, err = tx.Exec(ctx, "UPDATE platform.jobs SET frozen_settings = "+set+" WHERE id = $1::uuid", fresh.ID)
		return err
	}
	for _, c := range []struct {
		name, set string
		allowed   bool
	}{
		{"the template cleared, merge_fields kept", cleared, true},
		{"the template cleared and merge_fields dropped", "(" + cleared + ") - 'merge_fields'", false},
		{"the template cleared and merge_fields replaced", "jsonb_set(" + cleared + ", '{merge_fields}', '[\"other\"]'::jsonb)", false},
		{"merge_fields changed alone", "jsonb_set(frozen_settings, '{merge_fields}', '[\"other\"]'::jsonb)", false},
		{"merge_fields dropped alone", "frozen_settings - 'merge_fields'", false},
	} {
		err := try(c.set)
		var pe *pgconn.PgError
		switch {
		case c.allowed && err != nil:
			t.Errorf("the guard refused %s: %v", c.name, err)
		case !c.allowed && (!errors.As(err, &pe) || pe.Code != "23001"):
			t.Errorf("the guard did not refuse %s with 23001: %v", c.name, err)
		}
	}
	if e.jrtFrozen(fresh.ID) != frozenBefore[fresh.ID] {
		t.Fatal("a probe that was rolled back changed the job")
	}

	// two jobs become old by the database's clock; the third ended a moment ago
	e.jrtAgeEnd(src, 2*time.Hour)
	e.jrtAgeEnd(old.ID, 2*time.Hour)
	res, err := e.jobs.ClearRetainedContent(ctx, time.Hour, 0, 0)
	if err != nil || res.Cleared != 2 {
		t.Fatalf("the purge cleared %d jobs (%+v, %v), want the two that are old", res.Cleared, res, err)
	}

	// THE VALUES ARE GONE from both old jobs, the template of the send is its placeholders, and the NAMES are still there, in the store, in the JSON and in the frozen document, which is otherwise unchanged
	if values(src, old.ID) != 0 {
		t.Errorf("%d rows of the two old jobs still hold merge values", values(src, old.ID))
	}
	for _, id := range []string{src, old.ID} {
		if !namesKept(id) {
			t.Errorf("job %s lost its merge_fields in the purge: %s", id, e.jrtFrozen(id))
		}
		if !ledgerBool(t, e.admin, "SELECT (frozen_settings - 'content') = ($2::jsonb - 'content') FROM platform.jobs WHERE id = $1::uuid", id, frozenBefore[id]) {
			t.Errorf("job %s: something besides the template changed in the frozen document", id)
		}
		j, err := e.jobs.Get(ctx, A, id)
		if err != nil || fmt.Sprint(j.MergeFields) != "[city first_name]" || j.ContentClearedAt == nil {
			t.Errorf("job %s as the store tells it: merge fields %v, cleared at %v (%v)", id, j.MergeFields, j.ContentClearedAt, err)
		}
		m := e.json(A, "GET", "/v1/jobs/"+id, nil).obj(t, 200)
		if jcfNames(dig(m, "job", "merge_fields")) != "[city first_name]" || dig(m, "job", "content_cleared_at") == nil {
			t.Errorf("job %s as the API tells it: %v", id, dig(m, "job"))
		}
	}
	if got := e.str("SELECT frozen_settings #>> '{content,subject}' FROM platform.jobs WHERE id = $1::uuid", old.ID); got != "[removed]" {
		t.Errorf("the send's template says %q, want the placeholder", got)
	}
	// the control: the job that ended a moment ago keeps its values, its template and its names
	if values(fresh.ID) != freshValues || e.jrtFrozen(fresh.ID) != frozenBefore[fresh.ID] {
		t.Error("a job younger than the dial was cleared")
	}

	// A SEND CHAINED TO THE CLEARED SOURCE afterwards: its rows carry {} (the values are gone) and it still lists the names, so the author's screen keeps offering the columns; every pending row then lacks the field
	after := e.jcfChain(A, src, "")
	if fmt.Sprint(after.MergeFields) != "[city first_name]" || e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND fields <> '{}'::jsonb", after.ID) != 0 {
		t.Errorf("a send chained to the cleared source: merge fields %v, rows holding values %d", after.MergeFields, e.int("SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND fields <> '{}'::jsonb", after.ID))
	}
	if p := e.preview(A, after.ID); p.Pending != 4 || p.MissingFields != 4 {
		t.Errorf("the preview of a send chained to a cleared source: pending %d, missing %d, want 4 and 4", p.Pending, p.MissingFields)
	}
}
