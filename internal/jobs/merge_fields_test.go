package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// THE MERGE DATA OF A CHAINED SEND AND THE NAMES OF A JOB'S MERGE FIELDS (Phase 6; classify.go, merge_fields.go). The statements (the join that reads the source rows' merge data in the query that reads
// the results, the frozen document, the purge that keeps the names) are proved against a real PostgreSQL by tests/harness/jobs_chain_fields_test.go; what needs no database is proved here, each property
// beside a control that fails when it breaks.

func fieldsOf(t *testing.T, c classified) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(c.Fields), &m); err != nil {
		t.Fatalf("row %d holds %q, which is not a JSON object of strings: %v", c.Ordinal, c.Fields, err)
	}
	return m
}

func TestClassifyChain_ARowThatIsOrCanBecomeARecipientCarriesTheMergeDataOfItsSourceRow(t *testing.T) {
	ann := `{"city": "Oslo", "first_name": "Ann"}`
	res := []sourceResult{
		{"r1", "v1@example.com", VerdictValid, ann},
		{"r2", "i1@example.com", VerdictInvalid, `{"first_name": "Never"}`},
		{"r3", "k1@example.com", VerdictRisky, `{"first_name": "Rik"}`},
		{"r4", "u1@example.com", VerdictUnknown, `{"first_name": "Una"}`},
		{"r5", "w1@example.com", "teleported", `{"first_name": "Nobody"}`},
		{"r6", "v1@example.com", VerdictValid, `{"first_name": "Repeat"}`}, // an impossible repeat: a duplicate is never sent
		{"r7", "v2@example.com", VerdictValid, `{}`},                       // the retention cleared the source row's merge data
		{"r8", "v3@example.com", VerdictValid, ``},                         // no merge data read at all
		{"r9", "nul\x00@example.com", VerdictValid, `{"first_name": "Bad"}`},
	}
	for _, c := range []struct {
		choice string
		want   []string // the merge data each row must hold, as "" for {}
	}{
		// exclude: the unknown row is excluded NOW but the user's choice at the preview may turn it into a recipient, so it keeps its data
		{UnknownExclude, []string{ann, "", `{"first_name": "Rik"}`, `{"first_name": "Una"}`, "", "", "", "", ""}},
		{UnknownInclude, []string{ann, "", `{"first_name": "Rik"}`, `{"first_name": "Una"}`, "", "", "", "", ""}}, // the twin: the flip changes the state, never the data
	} {
		got := classifyChain(res, c.choice, seqIDs())
		for i, g := range got {
			if c.want[i] == "" {
				if g.Fields != "{}" {
					t.Errorf("choice %s, result %d (%s, %s): holds %q, want {} (a row nobody sends, or one with no data, carries none)", c.choice, i+1, res[i].Verdict, g.State, g.Fields)
				}
				continue
			}
			var want, have map[string]string
			if err := json.Unmarshal([]byte(c.want[i]), &want); err != nil {
				t.Fatal(err)
			}
			have = fieldsOf(t, g)
			if !reflect.DeepEqual(have, want) {
				t.Errorf("choice %s, result %d (%s, %s): holds %v, want the source row's %v", c.choice, i+1, res[i].Verdict, g.State, have, want)
			}
		}
	}

	// the controls: the same results with no merge data in the sources give a chain with none (a classifier that invents data fails here), and a result's data never lands on another row
	blank := make([]sourceResult, len(res))
	copy(blank, res)
	for i := range blank {
		blank[i].Fields = ""
	}
	for i, g := range classifyChain(blank, UnknownInclude, seqIDs()) {
		if g.Fields != "{}" {
			t.Errorf("result %d of a source with no merge data holds %q", i+1, g.Fields)
		}
	}
	got := classifyChain(res, UnknownInclude, seqIDs())
	if got[0].Fields == got[2].Fields || got[2].Fields == got[3].Fields {
		t.Errorf("two different source rows' merge data is one: %q, %q, %q", got[0].Fields, got[2].Fields, got[3].Fields)
	}
	// a pending row's data is the source row's byte for byte (nothing is re-written, so the fingerprint of the merge data is the same text)
	if got[0].Fields != ann {
		t.Errorf("the merge data was re-written: %q, want %q", got[0].Fields, ann)
	}
}

func TestMergeFieldsOfRows_AreTheNamesATemplateCanUseSortedUniqueAndCapped(t *testing.T) {
	rows := []RowIn{
		{Email: "a@example.test", Fields: map[string]string{"first_name": "Ann", "City": "Oslo", "First name": "x", "e-mail": "x", "1st": "x", "email": "x", "unsubscribe_url": "x", "": "x", "café": "x"}},
		{Email: "b@example.test", Fields: map[string]string{"first_name": "Bob", "_x": "1", "zip2": "2"}},
		{Email: "c@example.test"},
		{Email: "d@example.test", Fields: map[string]string{}},
	}
	got := mergeFieldsOfRows(rows)
	if want := []string{"City", "_x", "first_name", "zip2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v: the union, in byte order, of the keys a template can name (the two reserved names and every key the grammar refuses are left out)", got, want)
	}
	if got := mergeFieldsOfRows(nil); got == nil || len(got) != 0 {
		t.Errorf("no rows: %v, want an empty list that is not nil", got)
	}
	if got := mergeFieldsOfRows([]RowIn{{Email: "a@example.test"}}); got == nil || len(got) != 0 {
		t.Errorf("rows with no merge data: %v, want an empty list that is not nil", got)
	}
	// the cap: more names than MaxMergeFieldNames keeps the first ones in byte order, whatever order the rows came in
	big := map[string]string{}
	for i := 0; i < MaxMergeFieldNames+50; i++ {
		big[fmt.Sprintf("f%03d", i)] = "v"
	}
	capped := mergeFieldsOfRows([]RowIn{{Email: "a@example.test", Fields: big}})
	if len(capped) != MaxMergeFieldNames || capped[0] != "f000" || capped[len(capped)-1] != fmt.Sprintf("f%03d", MaxMergeFieldNames-1) {
		t.Errorf("a list of %d names became %d (%s to %s), want the first %d in byte order", len(big), len(capped), capped[0], capped[len(capped)-1], MaxMergeFieldNames)
	}
	exact := map[string]string{}
	for i := 0; i < MaxMergeFieldNames; i++ {
		exact[fmt.Sprintf("f%03d", i)] = "v"
	}
	if got := mergeFieldsOfRows([]RowIn{{Email: "a@example.test", Fields: exact}}); len(got) != MaxMergeFieldNames { // the twin: the cap itself is not cut
		t.Errorf("exactly %d names became %d", MaxMergeFieldNames, len(got))
	}
	// no value is ever in the list
	if strings.Contains(strings.Join(got, ","), "Ann") {
		t.Error("a value reached the names")
	}
}

func TestMergeFieldsOfSource_IsTheSourcesOwnListAndOtherwiseTheKeysOfItsRows(t *testing.T) {
	res := []sourceResult{
		{"r1", "a@example.test", VerdictValid, `{"first_name": "Ann", "city": "Oslo"}`},
		{"r2", "b@example.test", VerdictInvalid, `{"zip": "1"}`},
		{"r3", "c@example.test", VerdictValid, `{}`},
		{"r4", "d@example.test", VerdictValid, ``},
		{"r5", "e@example.test", VerdictValid, `not json`},
		{"r6", "f@example.test", VerdictValid, `["a", "b"]`},
		{"r7", "g@example.test", VerdictValid, `{"First name": "x", "email": "y", "_ok": "z"}`},
	}
	// the source has its own list: it is the answer (cleaned), and the rows are not read
	if got := mergeFieldsOfSource([]string{"zip", "city", "city", "First name", "email"}, res); !reflect.DeepEqual(got, []string{"city", "zip"}) {
		t.Errorf("a source with a list: %v, want its usable names, sorted and unique", got)
	}
	// the source has none (made before they were kept, or no merge data): the keys of its rows, every row, whatever its verdict
	if got := mergeFieldsOfSource(nil, res); !reflect.DeepEqual(got, []string{"_ok", "city", "first_name", "zip"}) {
		t.Errorf("a source with no list: %v, want the union of the keys of its rows", got)
	}
	if got := mergeFieldsOfSource([]string{}, res); len(got) != 4 {
		t.Errorf("an empty list is no list: %v", got)
	}
	// neither: an empty list that is not nil
	if got := mergeFieldsOfSource(nil, res[2:6]); got == nil || len(got) != 0 {
		t.Errorf("a source whose rows hold no usable key: %v, want an empty list that is not nil", got)
	}
	if got := mergeFieldsOfSource(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("no results: %v, want an empty list that is not nil", got)
	}
}

func TestFreezeVendorChoice_TheNamesOfTheMergeDataAreFrozenWithTheJobAndOnlyTheNames(t *testing.T) {
	q := &settingQ{row: settingRow{none: true}}
	content := &Content{Subject: "Hello {{first_name}}", HTML: "<p>{{city}}</p>"}
	b, _, err := freezeVendorChoice(context.Background(), q, optOutUser, ModuleSendManaged, Settings{}, content, []string{"city", "first_name"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["merge_fields"]) != `["city","first_name"]` {
		t.Errorf("the frozen document holds merge_fields %s: %s", doc["merge_fields"], b)
	}
	var f frozenSettings
	if err := json.Unmarshal(b, &f); err != nil || !reflect.DeepEqual(f.MergeFields, []string{"city", "first_name"}) || f.Content == nil {
		t.Errorf("the document does not read back: %+v, %v", f, err)
	}
	// the twin: a job with no merge data carries no key at all, so a job made before the names were kept and one made without data are one document
	for _, none := range [][]string{nil, {}} {
		b, _, err := freezeVendorChoice(context.Background(), &settingQ{row: settingRow{none: true}}, optOutUser, ModuleVerify, Settings{}, nil, none)
		if err != nil || strings.Contains(string(b), "merge_fields") {
			t.Errorf("a job with no merge data froze %s (%v)", b, err)
		}
	}
}
