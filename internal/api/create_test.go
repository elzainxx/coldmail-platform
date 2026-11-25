package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

func (r *rig) postCSV(login, query, body string) (*http.Response, []byte) {
	r.t.Helper()
	return r.do(req{method: http.MethodPost, path: "/v1/jobs" + query, token: r.token(login), ctype: "text/csv", body: []byte(body)})
}

func (r *rig) created() jobs.CreateReq {
	r.t.Helper()
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.store.created
}

func TestCreate_JSONBecomesTheStoresRequestAndIsAnswered201WithTheJob(t *testing.T) {
	r := newRig(t)
	r.store.job = jobs.Job{ID: jobIDA, UserID: userA, Module: "m2", Status: jobs.StatusDraft, CreatedAt: t0, UpdatedAt: t0,
		Settings: jobs.Settings{UnknownChoice: "include"}, SourceJobID: otherJob, Content: &jobs.Content{Subject: "Hi", HTML: "<p>x</p>", Text: "x", ReplyTo: "r@example.test"}}
	resp, body := r.post(loginA, "/v1/jobs", `{"module":"m2","rows":[{"email":"a@example.test","fields":{"first":"Ann","city":"Paris"}},{"email":"b@example.test"}],
		"content":{"subject":"Hi","html":"<p>x</p>","text":"x","reply_to":"r@example.test"},"source_job_id":"`+otherJob+`","settings":{"unknown_choice":"include"}}`)
	m := r.wantStatus(resp, body, 201)
	if resp.Header.Get("Location") != "/v1/jobs/"+jobIDA {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}
	job := m["job"].(map[string]any)
	if job["id"] != jobIDA || job["module"] != "m2" || job["status"] != "draft" || job["source_job_id"] != otherJob {
		t.Errorf("job = %v", job)
	}
	if c := job["content"].(map[string]any); c["subject"] != "Hi" || c["reply_to"] != "r@example.test" {
		t.Errorf("content = %v", c)
	}
	if _, has := job["user_id"]; has {
		t.Errorf("the job shows its owner: %v", job)
	}
	want := jobs.CreateReq{
		Module: "m2", SourceJobID: otherJob, Settings: jobs.Settings{UnknownChoice: "include"},
		Content: &jobs.Content{Subject: "Hi", HTML: "<p>x</p>", Text: "x", ReplyTo: "r@example.test"},
		Rows:    []jobs.RowIn{{Email: "a@example.test", Fields: map[string]string{"first": "Ann", "city": "Paris"}}, {Email: "b@example.test"}},
	}
	got := r.created()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the store was asked\n  %+v\nwant\n  %+v", got, want)
	}
	if c := r.store.seen(); len(c) != 1 || c[0].User != userA {
		t.Fatalf("calls %+v", c)
	}
	// a body that sets none of the optional members is a request with none of them
	r.post(loginA, "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test"}]}`)
	if g := r.created(); g.Content != nil || g.SourceJobID != "" || g.Settings.UnknownChoice != "" {
		t.Fatalf("members nobody sent were invented: %+v", g)
	}
}

func TestCreate_TheStoresRefusalsAreTheClientsAnswers(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{&jobs.BadRequestError{Reason: "a job needs at least one row"}, 400, "validation_failed"},
		{&jobs.TooManyRowsError{Limit: 5}, 413, "too_many_rows"},
		{jobs.ErrNotFound, 404, "not_found"},
	} {
		r.store.fail["CreateDraft"] = c.err
		resp, body := r.post(loginA, "/v1/jobs", `{"module":"m1","rows":[{"email":"a@example.test"}]}`)
		r.wantError(resp, body, c.status, c.code)
	}
}

// ---- CSV, the body ----

func TestCreate_CSVHeaderEmailColumnAndMergeFields(t *testing.T) {
	r := newRig(t)
	type row = jobs.RowIn
	for _, c := range []struct {
		name string
		csv  string
		want []row
	}{
		{"the email column alone", "email\nalice@example.test\nbob@example.test\n", []row{{Email: "alice@example.test"}, {Email: "bob@example.test"}}},
		{"any letter case and surrounding space in the header", " EMAIL \nalice@example.test\n", []row{{Email: "alice@example.test"}}},
		{"Email, then fields, in any order", "first,Email,city\nAnn,ann@example.test,Paris\n", []row{{Email: "ann@example.test", Fields: map[string]string{"first": "Ann", "city": "Paris"}}}},
		{"a byte order mark before the header", "\xef\xbb\xbfemail,first\nann@example.test,Ann\n", []row{{Email: "ann@example.test", Fields: map[string]string{"first": "Ann"}}}},
		{"CRLF line ends", "email,first\r\nann@example.test,Ann\r\nbob@example.test,Bob\r\n", []row{
			{Email: "ann@example.test", Fields: map[string]string{"first": "Ann"}}, {Email: "bob@example.test", Fields: map[string]string{"first": "Bob"}}}},
		{"no newline at the end", "email\nalice@example.test", []row{{Email: "alice@example.test"}}},
		{"quoted cells with commas, quotes and a newline", "email,note\n\"a@example.test\",\"one, \"\"two\"\"\nthree\"\n", []row{{Email: "a@example.test", Fields: map[string]string{"note": "one, \"two\"\nthree"}}}},
		{"empty cells stay empty fields", "email,first\nann@example.test,\n", []row{{Email: "ann@example.test", Fields: map[string]string{"first": ""}}}},
		{"an address typed badly is still the store's to judge", "email\n not an address \n", []row{{Email: " not an address "}}},
		{"blank lines are skipped", "email\n\nalice@example.test\n\n", []row{{Email: "alice@example.test"}}},
		{"a cell that starts with a formula trigger is read as it is (the apostrophe is the export's, not the import's)", "email,note\na@example.test,=1+1\n", []row{{Email: "a@example.test", Fields: map[string]string{"note": "=1+1"}}}},
	} {
		r.store.created = jobs.CreateReq{}
		resp, body := r.postCSV(loginA, "?module=m1", c.csv)
		if resp.StatusCode != 201 {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, body)
			continue
		}
		if got := r.created(); fmt.Sprint(got.Rows) != fmt.Sprint(c.want) || got.Module != "m1" {
			t.Errorf("%s: the store got module %q rows %v, want %v", c.name, got.Module, got.Rows, c.want)
		}
	}
}

func TestCreate_ABadCSVIsRefusedByLineNeverByContentAndEachHasALegalTwin(t *testing.T) {
	r := newRig(t)
	const cellCanary = "CANARY-cell-5531"
	for _, c := range []struct {
		name, bad, good, reason string
	}{
		{"no header at all", "", "email\na@example.test\n", "no header row"},
		{"no email column", "address," + cellCanary + "\na@example.test," + cellCanary + "\n", "email," + cellCanary + "\na@example.test," + cellCanary + "\n", "no email column"},
		{"the email column twice", "email,Email\na,b\n", "email,city\na,b\n", "email column twice"},
		{"a column with no name", "email,\na,b\n", "email,x\na,b\n", "column 2 of the header has no name"},
		{"a column named twice", "email,city,city\na,b,c\n", "email,city,town\na,b,c\n", "names a column twice"},
		{"a row that is short", "email,city\na@example.test,Paris\nb@example.test\n", "email,city\na@example.test,Paris\nb@example.test,Rome\n", "line 3 has a different number of columns"},
		{"a row that is long", "email\na@example.test\nb@example.test," + cellCanary + "\n", "email\na@example.test\nb@example.test\n", "line 3 has a different number of columns"},
		{"a quote inside an unquoted field", "email\na@exa\"mple.test\n", "email\na@example.test\n", "line 2 has a quote inside an unquoted field"},
		{"a quoted field that is not closed", "email\n\"a@example.test\n", "email\n\"a@example.test\"\n", "line 2"},
	} {
		r.store.reset()
		resp, body := r.postCSV(loginA, "?module=m1", c.bad)
		e := r.wantError(resp, body, 400, "validation_failed")
		if got, _ := e.Details["reason"].(string); !strings.Contains(got, c.reason) {
			t.Errorf("%s: the reason is %q, want it to say %q", c.name, got, c.reason)
		}
		if strings.Contains(string(body), cellCanary) || strings.Contains(string(body), "example.test") {
			t.Errorf("%s: the answer quotes the file: %s", c.name, body)
		}
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called for a CSV that could not be read", c.name)
		}
		if resp, body := r.postCSV(loginA, "?module=m1", c.good); resp.StatusCode != 201 {
			t.Errorf("%s: its legal twin was answered %d: %s", c.name, resp.StatusCode, body)
		}
	}
}

// ---- CSV, the query ----

func TestCreate_CSVQueryCarriesTheCampaignAndTheOptions(t *testing.T) {
	r := newRig(t)
	awkward := map[string]string{
		"subject":  "Hello & welcome, 100% = fun? #1 +more",
		"html":     "<p style=\"x\">café ☃ &amp; 'q'</p>\r\n<p>two</p>",
		"text":     "line one\nline two\ttabbed",
		"reply_to": "reply@example.test",
	}
	q := url.Values{"module": {"m3"}, "unknown_choice": {"include"}, "source_job_id": {""}}
	for k, v := range awkward {
		q.Set(k, v)
	}
	q.Del("source_job_id")
	resp, body := r.postCSV(loginA, "?"+q.Encode(), "email,first\na@example.test,Ann\n")
	r.wantStatus(resp, body, 201)
	got := r.created()
	if got.Module != "m3" || got.Settings.UnknownChoice != "include" || got.Content == nil {
		t.Fatalf("the store got %+v", got)
	}
	if *got.Content != (jobs.Content{Subject: awkward["subject"], HTML: awkward["html"], Text: awkward["text"], ReplyTo: awkward["reply_to"]}) {
		t.Fatalf("the campaign was changed on the way: %+v", *got.Content)
	}

	// no campaign parameter at all is a request with no content (a verify job); one of them is a content (the store judges whether it is a good one)
	resp, body = r.postCSV(loginA, "?module=m1", "email\na@example.test\n")
	r.wantStatus(resp, body, 201)
	if r.created().Content != nil {
		t.Fatalf("a content was made up for a list that sent none: %+v", r.created().Content)
	}
	r.postCSV(loginA, "?module=m1&subject=x", "email\na@example.test\n")
	if c := r.created().Content; c == nil || c.Subject != "x" || c.HTML != "" {
		t.Fatalf("the one parameter sent is not the content: %+v", c)
	}

	// the parameters of a chained send
	r.postCSV(loginA, "?module=m2&source_job_id="+otherJob+"&unknown_choice=exclude&subject=s&html=h", "email\n")
	if g := r.created(); g.SourceJobID != otherJob || g.Settings.UnknownChoice != "exclude" || g.Module != "m2" {
		t.Fatalf("the chained send reached the store as %+v", g)
	}
}

func TestCreate_CSVQueryRefusesWhatItDoesNotKnowAndWhatIsTooLong(t *testing.T) {
	r := newRig(t)
	body := "email\na@example.test\n"
	for _, c := range []struct {
		name, query string
		status      int
		code        string
	}{
		{"an unknown parameter", "?module=m1&colour=red", 400, "bad_request"},
		{"a user", "?module=m1&user_id=" + userA, 400, "bad_request"},
		{"a parameter twice", "?module=m1&module=m2", 400, "bad_request"},
		{"a query that is not one", "?module=m1&%zz", 400, "bad_request"},
		{"a query of 64 KiB and a byte", "?module=m1&html=" + strings.Repeat("a", maxCSVQuery), 413, "payload_too_large"},
	} {
		r.store.reset()
		resp, b := r.postCSV(loginA, c.query, body)
		r.wantError(resp, b, c.status, c.code)
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called", c.name)
		}
	}
	// the control: a query just under 64 KiB is read
	q := "?module=m2&subject=s&html=" + strings.Repeat("a", maxCSVQuery-len("?module=m2&subject=s&html="))
	if len(q) != maxCSVQuery {
		t.Fatalf("the query is %d bytes", len(q))
	}
	resp, b := r.postCSV(loginA, q, body)
	r.wantStatus(resp, b, 201)
	if got := r.created(); got.Content == nil || len(got.Content.HTML) != maxCSVQuery-len("?module=m2&subject=s&html=") {
		t.Fatalf("a query at the cap was not read whole")
	}
}

func TestCreate_TheRowCapIsTheStoresMaxRowsForBothShapes(t *testing.T) {
	r := newRig(t)
	r.store.maxRows = 3
	row := func(i int) string { return fmt.Sprintf("u%d@example.test", i) }
	csv := func(n int) string {
		var b strings.Builder
		b.WriteString("email\n")
		for i := 1; i <= n; i++ {
			b.WriteString(row(i) + "\n")
		}
		return b.String()
	}
	js := func(n int) string {
		var rows []string
		for i := 1; i <= n; i++ {
			rows = append(rows, `{"email":"`+row(i)+`"}`)
		}
		return `{"module":"m1","rows":[` + strings.Join(rows, ",") + `]}`
	}
	for _, shape := range []string{"csv", "json"} {
		send := func(n int) (*http.Response, []byte) {
			if shape == "csv" {
				return r.postCSV(loginA, "?module=m1", csv(n))
			}
			return r.post(loginA, "/v1/jobs", js(n))
		}
		// at the limit: a job; one over: 413 too_many_rows naming the limit, whatever the shape
		resp, body := send(3)
		r.wantStatus(resp, body, 201)
		r.store.reset()
		resp, body = send(4)
		e := r.wantError(resp, body, 413, "too_many_rows")
		if e.Details["limit"] != float64(3) {
			t.Errorf("%s: details %v, want the limit 3", shape, e.Details)
		}
		if shape == "csv" && len(r.store.seen()) != 0 {
			t.Errorf("csv: the store was asked to make a job from a list that was cut off at the limit")
		}
	}
	// a CSV is cut off AT the limit while it is read: a list far over it is refused without being read to its end. The server may answer and close the connection while the client is still
	// sending (what a refusal that did not need the rest of the body looks like on the wire): then the client sees the closed connection and not the answer, which is the same refusal.
	r.store.reset()
	if resp, err := r.doRaw(req{method: http.MethodPost, path: "/v1/jobs?module=m1", token: r.token(loginA), ctype: "text/csv", body: []byte(csv(50000))}); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		r.wantError(resp, b, 413, "too_many_rows")
	}
	if len(r.store.seen()) != 0 {
		t.Fatalf("the store was asked to make a job from a list that was cut off at the limit")
	}
}

func TestCreate_AChainedSendTakesNoRowsFromTheBody(t *testing.T) {
	r := newRig(t)
	// CSV with no body at all is the chained send's way of sending nothing; JSON with no rows is the same
	resp, body := r.do(req{method: "POST", path: "/v1/jobs?module=m2&source_job_id=" + otherJob + "&subject=s&html=h", token: r.token(loginA), ctype: "text/csv"})
	r.wantStatus(resp, body, 201)
	if g := r.created(); g.SourceJobID != otherJob || len(g.Rows) != 0 {
		t.Fatalf("%+v", g)
	}
	resp, body = r.post(loginA, "/v1/jobs", `{"module":"m2","source_job_id":"`+otherJob+`","content":{"subject":"s","html":"h"}}`)
	r.wantStatus(resp, body, 201)
	if g := r.created(); g.SourceJobID != otherJob || len(g.Rows) != 0 {
		t.Fatalf("%+v", g)
	}
	// the control: without a source, an empty CSV is not a list
	resp, body = r.do(req{method: "POST", path: "/v1/jobs?module=m1", token: r.token(loginA), ctype: "text/csv"})
	r.wantError(resp, body, 400, "validation_failed")
}
