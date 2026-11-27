package api

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// The CSV create's `email_column` (Phase 6 A3, create.go): the header the query names is the address, every other column is a merge field, and the two names the renderer reserves cannot be a merge
// column beside an address column that is not `email`. Without the parameter nothing changed: the tests of create_test.go still hold, and the twins below repeat the cases that must behave as they did.

func (r *rig) rowsOf() []jobs.RowIn { return r.created().Rows }

func TestCreateEmailColumn_TheNamedColumnIsTheAddressAndEveryOtherColumnIsAMergeField(t *testing.T) {
	r := newRig(t)
	resp, body := r.postCSV(loginA, "?module=m1&email_column=work_email", "name,work_email,city\nAnn,ann@example.test,Paris\nBob,bob@example.test,Oslo\n")
	r.wantStatus(resp, body, 201)
	want := []jobs.RowIn{
		{Email: "ann@example.test", Fields: map[string]string{"name": "Ann", "city": "Paris"}},
		{Email: "bob@example.test", Fields: map[string]string{"name": "Bob", "city": "Oslo"}},
	}
	if got := r.rowsOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
}

func TestCreateEmailColumn_TheNameIsMatchedWithoutCaseAndWithoutSurroundingSpace(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct{ query, csv string }{
		"another case in the header":  {"?module=m1&email_column=work_email", "name, WORK_Email ,city\nAnn,ann@example.test,Paris\n"},
		"another case in the query":   {"?module=m1&email_column=WORK_EMAIL", "name,work_email,city\nAnn,ann@example.test,Paris\n"},
		"space around the query":      {"?module=m1&email_column=" + url.QueryEscape("  work_email "), "name,work_email,city\nAnn,ann@example.test,Paris\n"},
		"a name with a space in it":   {"?module=m1&email_column=" + url.QueryEscape("Work E-mail"), "name,Work E-mail,city\nAnn,ann@example.test,Paris\n"},
		"a name that is not ASCII":    {"?module=m1&email_column=" + url.QueryEscape("courrier électronique"), "name,Courrier Électronique,city\nAnn,ann@example.test,Paris\n"},
		"a byte order mark and quote": {"?module=m1&email_column=work_email", "\xef\xbb\xbf\"name\",\"work_email\",\"city\"\n\"Ann\",\"ann@example.test\",\"Paris\"\n"},
	} {
		resp, body := r.postCSV(loginA, c.query, c.csv)
		r.wantStatus(resp, body, 201)
		rows := r.rowsOf()
		if len(rows) != 1 || rows[0].Email != "ann@example.test" || len(rows[0].Fields) != 2 || rows[0].Fields["name"] != "Ann" || rows[0].Fields["city"] != "Paris" {
			t.Errorf("%s: rows = %+v", name, rows)
		}
	}
}

func TestCreateEmailColumn_TheDefaultIsStillEmailAndNamingEmailIsTheDefault(t *testing.T) {
	r := newRig(t)
	csv := "Name,Email,city\nAnn,ann@example.test,Paris\n"
	want := []jobs.RowIn{{Email: "ann@example.test", Fields: map[string]string{"Name": "Ann", "city": "Paris"}}}
	for name, q := range map[string]string{"no parameter": "?module=m1", "an empty parameter": "?module=m1&email_column=", "email itself": "?module=m1&email_column=email", "email in capitals": "?module=m1&email_column=%20EMAIL%20"} {
		resp, body := r.postCSV(loginA, q, csv)
		r.wantStatus(resp, body, 201)
		if got := r.rowsOf(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows = %+v, want %+v", name, got, want)
		}
	}
	// the default refuses what it always refused (the twin of every refusal below: the same CSV without the parameter)
	resp, body := r.postCSV(loginA, "?module=m1", "name,work_email\nAnn,ann@example.test\n")
	e := r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
	if e.Details["reason"] != "the header has no email column" {
		t.Errorf("the default's refusal changed: %v", e.Details)
	}
	// and a column called unsubscribe_url is accepted with the default address column, as it always was (it is shadowed, which the renderer's own tests pin)
	resp, body = r.postCSV(loginA, "?module=m1", "email,unsubscribe_url\nann@example.test,x\n")
	r.wantStatus(resp, body, 201)
}

func TestCreateEmailColumn_AnotherColumnNamedEmailOrUnsubscribeURLIsRefusedBesideAnExplicitAddressColumn(t *testing.T) {
	r := newRig(t)
	for name, csv := range map[string]string{
		"an email column that is not the address":       "work_email,email\nann@example.test,personal@example.test\n",
		"an Email column in capitals":                   "Email,work_email\npersonal@example.test,ann@example.test\n",
		"an email column with space around it":          "work_email, email \nann@example.test,x\n",
		"an unsubscribe_url column":                     "work_email,unsubscribe_url\nann@example.test,x\n",
		"an Unsubscribe_URL column in another case":     "Unsubscribe_URL,work_email\nx,ann@example.test\n",
		"both, and the address is not the first column": "name,email,unsubscribe_url,work_email\nAnn,a@example.test,x,ann@example.test\n",
	} {
		r.store.reset()
		resp, body := r.postCSV(loginA, "?module=m1&email_column=work_email", csv)
		e := r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
		reason, _ := e.Details["reason"].(string)
		if !strings.Contains(reason, "reserved") {
			t.Errorf("%s: the reason is %q, want it to say the name is reserved", name, reason)
		}
		for _, leak := range []string{"ann@example.test", "personal@example.test", "a@example.test", "work_email"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s: the answer holds %q, a value or a column of the file: %s", name, leak, body)
			}
		}
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called for a list that was refused", name)
		}
	}
	// the twins: the same two names are fine when they ARE the address (email), and any other name beside the address is a merge field
	resp, body := r.postCSV(loginA, "?module=m1&email_column=work_email", "work_email,personal_email,unsubscribe_link\nann@example.test,p@example.test,x\n")
	r.wantStatus(resp, body, 201)
	if rows := r.rowsOf(); len(rows) != 1 || rows[0].Fields["personal_email"] != "p@example.test" || rows[0].Fields["unsubscribe_link"] != "x" {
		t.Errorf("the names that are not reserved did not become merge fields: %+v", rows)
	}
}

func TestCreateEmailColumn_ARefusalNamesNoValueAndTheColumnMustBeThereExactlyOnce(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct{ query, csv, reason string }{
		"no such column":                     {"?module=m1&email_column=work_email", "name,mail\nAnn,ann@example.test\n", "the header has no column named by email_column"},
		"the column twice":                   {"?module=m1&email_column=work_email", "work_email,name,work_email\na@example.test,Ann,b@example.test\n", "the header names the email_column column twice"},
		"the column twice in two cases":      {"?module=m1&email_column=work_email", "Work_Email,name,WORK_EMAIL\na@example.test,Ann,b@example.test\n", "the header names the email_column column twice"},
		"a blank parameter":                  {"?module=m1&email_column=%20%20", "email\na@example.test\n", "email_column must name a column of the header"},
		"a header with an empty name":        {"?module=m1&email_column=work_email", "work_email,\na@example.test,x\n", "column 2 of the header has no name"},
		"two other columns of the same name": {"?module=m1&email_column=work_email", "work_email,name,name\na@example.test,x,y\n", "the header names a column twice"},
		"a row of another width":             {"?module=m1&email_column=work_email", "work_email,name\na@example.test\n", "line 2 has a different number of columns than the header"},
	} {
		r.store.reset()
		resp, body := r.postCSV(loginA, c.query, c.csv)
		e := r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
		if e.Details["reason"] != c.reason {
			t.Errorf("%s: the reason is %v, want %q", name, e.Details["reason"], c.reason)
		}
		if strings.Contains(string(body), "a@example.test") || strings.Contains(string(body), "ann@example.test") {
			t.Errorf("%s: the answer holds an address of the file: %s", name, body)
		}
		if len(r.store.seen()) != 0 {
			t.Errorf("%s: the store was called", name)
		}
	}
	// a parameter given twice is a 400 like every repeated parameter
	resp, body := r.postCSV(loginA, "?module=m1&email_column=a&email_column=b", "a\nx@example.test\n")
	r.wantError(resp, body, http.StatusBadRequest, "bad_request")
	// the twin: the first of the table's CSVs, with its column named once and rightly, is accepted
	resp, body = r.postCSV(loginA, "?module=m1&email_column=work_email", "work_email,name\na@example.test,Ann\n")
	r.wantStatus(resp, body, 201)
}

func TestCreateEmailColumn_AChainedSendHasNoListAndTheParameterIsHarmless(t *testing.T) {
	r := newRig(t)
	resp, body := r.postCSV(loginA, "?module=m2&source_job_id="+otherJob+"&subject=s&html=h&email_column=work_email", "")
	r.wantStatus(resp, body, 201)
	if g := r.created(); g.SourceJobID != otherJob || len(g.Rows) != 0 {
		t.Fatalf("the chained send reached the store as %+v", g)
	}
	// a list with no rows and no source is a list with no list, as ever
	resp, body = r.postCSV(loginA, "?module=m1&email_column=work_email", "")
	r.wantError(resp, body, http.StatusBadRequest, "validation_failed")
}
