package m1

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/money"
)

// handPage is a page of results written by hand, for the cases the real node's recording (testdata/contract/results-page.json: seven rows from the middle of a job, ordinals 7 to 13)
// does not hold: a page that starts at the first row of the job (ordinal 0), an origin other than `remote` (`own`), a skip token that comes together with an origin, a repeat whose
// original is on the same page, and a malformed row. (The recording does hold an unknown with origin `remote`, a skip token and a repeat of row 0 that is not on its page: those
// are compared row for row with the file in TestResults_ReadsEveryRecordedPage.) It keeps the shapes of module 1's handler (api.go handleJobResults, bulk_rows.go,
// queue/enqueue.go rejectTemplate).
const handPage = `{"job_id":"` + testJob + `","results":[
{"api_version":"v1","status":"valid","reason":"accepted","confidence":90,"recommendation":"send","email":"","domain":"acme.example","flags":{"from_cache":false},"signals":{"smtp_code":"250"},"raw":null,"item_id":"row-0","ordinal":0},
{"api_version":"v1","status":"unknown","reason":"dns_error","confidence":10,"recommendation":"review","email":"","domain":"gone.example","flags":{"from_cache":false},"signals":{"failure_origin":"remote","retry_after_hint_s":300},"raw":null,"item_id":"row-1","ordinal":1},
{"api_version":"v1","status":"valid","reason":"accepted","confidence":90,"recommendation":"send","email":"","domain":"acme.example","flags":{"from_cache":false},"signals":{"smtp_code":"250"},"raw":null,"item_id":"row-2","ordinal":2,"duplicate_of":0},
{"api_version":"v1","status":"risky","reason":"plausible","confidence":40,"recommendation":"review","email":"","domain":"slow.example","flags":{"from_cache":false},"signals":{"smtp_skipped_reason":"smtp_unwired","failure_origin":"own"},"raw":null,"item_id":"row-3","ordinal":3},
{"api_version":"v1","status":"invalid","reason":"invalid_syntax","confidence":99,"recommendation":"skip","email":"","domain":"","flags":{"from_cache":false},"signals":{"syntax_valid":false},"raw":null,"item_id":"row-4","ordinal":4}
],"next_cursor":"4"}`

// recordedResultsPages names the recorded fixtures of the two results contracts that answered 200, split by what the FILE holds: the ones with rows and the ones with none. The
// split is made from the content, never from the case name (in an earlier recording the two were filed the wrong way round: `page` held no row and `empty_page` held all the rows).
func recordedResultsPages(t *testing.T) (populated, empty []string) {
	t.Helper()
	for _, c := range Contracts() {
		if c.Route != tplResults || c.Status != 200 || c.Case == "page_cancelled" { // the page that holds a cancelled row has tests of its own (cancel_contracts_test.go)
			continue
		}
		if len(recordedRows(t, c.Fixture)) > 0 {
			populated = append(populated, c.Fixture)
		} else {
			empty = append(empty, c.Fixture)
		}
	}
	return populated, empty
}

// TestResults_ReadsTheCasesTheRecordingsDoNotHold: a hand-written page (handPage: a first answer at ordinal 0, an unknown with its origin, a repeat of row 0 on the same page, a
// degraded answer with its skip token and origin, a malformed row) decodes to exactly the fields the platform acts on, in order, with the cursor; every key it does not read (email, meta, raw, confidence, ...)
// is ignored. Paired control: a page with nothing on it (the recorded empty page) decodes to NO rows and "" (not an error), and two different elements decode to different Rows,
// so the decoder is not returning a constant; duplicate_of 0 is a pointer to 0, never an absent key.
func TestResults_ReadsTheCasesTheRecordingsDoNotHold(t *testing.T) {
	f := newFakeM1(t, func(call) (int, string) { return 200, handPage })
	a := f.adapter(t)
	rows, next, err := a.Results(testCtx(t), testJob, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Ordinal: 0, ItemID: "row-0", Status: "valid", Reason: "accepted"},
		{Ordinal: 1, ItemID: "row-1", Status: "unknown", Reason: "dns_error", FailureOrigin: "remote"},
		{Ordinal: 2, ItemID: "row-2", DuplicateOf: ptr(0), Status: "valid", Reason: "accepted"},
		{Ordinal: 3, ItemID: "row-3", Status: "risky", Reason: "plausible", SkipReason: "smtp_unwired", FailureOrigin: "own"},
		{Ordinal: 4, ItemID: "row-4", Status: "invalid", Reason: "invalid_syntax"},
	}
	if !reflect.DeepEqual(rows, want) || next != "4" {
		t.Fatalf("Results returned %+v next %q, want %+v next %q", rows, next, want, "4")
	}
	if rows[2].DuplicateOf == nil || *rows[2].DuplicateOf != 0 {
		t.Fatal("duplicate_of: 0 is a real value (the first row of the job); it must decode to a pointer to 0, never to absent")
	}
	if rows[4].DuplicateOf != nil {
		t.Fatal("a malformed row is never a duplicate, and carries no duplicate_of")
	}
	if reflect.DeepEqual(rows[0], rows[4]) {
		t.Fatal("control: two different elements decoded to the same Row: the decoder returns something that does not depend on its input")
	}

	// control: a page with nothing on it decodes to no rows
	_, empty := recordedResultsPages(t)
	if len(empty) == 0 {
		t.Fatal("control setup: no recorded results page is empty")
	}
	g := newFakeM1(t, func(call) (int, string) { return 200, fixture(t, empty[0]) })
	rows, next, err = g.adapter(t).Results(testCtx(t), testJob, "4", 0)
	if err != nil || len(rows) != 0 || next != "" {
		t.Fatalf("control: an empty last page gave %+v %q %v", rows, next, err)
	}
}

// TestResults_ReadsEveryRecordedPage: each page RECORDED from the real module decodes row for row to what an independent parse of the same file says: ordinal, item_id, status,
// reason, the duplicate_of of the rows that repeat an address (a pointer to the value, 0 included), the cache flag, the skip token and the failure origin, and the page's next
// cursor; the rows are one per ordinal, in order: a row's ordinal is the file's own (the page's first ordinal plus the row's position), never its position in the page, so a recording
// of a page from the middle of a job (its first ordinal is not 0) is read by the same expectation as one from the start. The recordings must include a page with rows and a page with
// none: a set of one kind only is red. Paired controls: the same comparison over a copy with one row changed, and a page with its rows against the expectation of one without, each
// fail, so it is a comparison that can see a difference. What the recording does and does not exercise (repeats, origins, skip tokens, cache hits) is logged, so a recording that
// holds none of a provenance field is visibly a comparison of empty against empty on that field.
func TestResults_ReadsEveryRecordedPage(t *testing.T) {
	read := func(body string) ([]Row, string, error) {
		return serve(t, 200, body).adapter(t).Results(testCtx(t), testJob, "", 0)
	}
	// differences says where the decoded rows differ from the generic rows of the same body
	differences := func(rows []Row, want []map[string]any) []string {
		var out []string
		if len(rows) != len(want) {
			return []string{fmt.Sprintf("%d row(s), the file has %d", len(rows), len(want))}
		}
		if len(want) == 0 {
			return nil
		}
		first := num(t, want[0]["ordinal"]) // the ordinal the page starts at: 0 for the first page of a job, anything for a page from the middle of one
		for i, w := range want {
			sig := w["signals"].(map[string]any)
			origin, _ := sig["failure_origin"].(string)
			skip, _ := sig["smtp_skipped_reason"].(string)
			r := rows[i]
			var dup *int
			if d, ok := w["duplicate_of"]; ok {
				n := num(t, d)
				dup = &n
			}
			switch {
			case r.Ordinal != num(t, w["ordinal"]) || r.Ordinal != first+i:
				out = append(out, fmt.Sprintf("row %d: ordinal %d, the file has %d and its page starts at %d", i, r.Ordinal, num(t, w["ordinal"]), first))
			case r.ItemID != w["item_id"].(string):
				out = append(out, fmt.Sprintf("row %d: item_id %q", i, r.ItemID))
			case r.Status != w["status"].(string) || r.Reason != w["reason"].(string):
				out = append(out, fmt.Sprintf("row %d: %s/%s", i, r.Status, r.Reason))
			case !reflect.DeepEqual(r.DuplicateOf, dup):
				out = append(out, fmt.Sprintf("row %d: duplicate_of differs", i))
			case r.FromCache != w["flags"].(map[string]any)["from_cache"].(bool) || r.FailureOrigin != origin || r.SkipReason != skip:
				out = append(out, fmt.Sprintf("row %d: provenance differs", i))
			}
		}
		return out
	}
	populated, empty := recordedResultsPages(t)
	if len(populated) == 0 || len(empty) == 0 {
		t.Fatalf("the recorded results pages are %v with rows and %v without: both kinds are needed", populated, empty)
	}
	for _, name := range append(append([]string(nil), populated...), empty...) {
		body := fixture(t, name)
		want := recordedRows(t, name)
		rows, next, err := read(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := differences(rows, want); len(d) != 0 {
			t.Fatalf("%s: the decoded rows differ from the recording: %v", name, d)
		}
		if wantNext := recordedDoc(t, name)["next_cursor"].(string); next != wantNext {
			t.Fatalf("%s: next cursor %q, the file says %q", name, next, wantNext)
		}
	}
	for _, name := range populated {
		want := recordedRows(t, name)
		if len(want) < 2 {
			t.Fatalf("%s holds %d row(s): too few to say anything about paging", name, len(want))
		}
		// control: one row of the recording changed (its reason, to another word of the vocabulary) is a difference the comparison reports
		original := want[0]["reason"].(string)
		other := "null_mx"
		if original == other {
			other = "no_mx"
		}
		mrows, _, err := read(retype("results[0].reason", other).apply(t, fixture(t, name)))
		if err != nil {
			t.Fatal(err)
		}
		if d := differences(mrows, want); len(d) != 1 || !strings.Contains(d[0], "row 0") {
			t.Fatalf("%s control: one changed row gave %v, want exactly that row reported", name, d)
		}
		// control: a page with rows is not a page without
		if d := differences(mrows[:0], want); len(d) == 0 {
			t.Fatalf("%s control: no rows against a page with rows reported no difference", name)
		}
	}
	// what the recordings exercise: a provenance field no recorded row carries was compared as empty against empty
	var nRows, nDup, nOrigin, nSkip, nCache int
	for _, name := range populated {
		for _, w := range recordedRows(t, name) {
			sig := w["signals"].(map[string]any)
			nRows++
			if _, ok := w["duplicate_of"]; ok {
				nDup++
			}
			if _, ok := sig["failure_origin"]; ok {
				nOrigin++
			}
			if _, ok := sig["smtp_skipped_reason"]; ok {
				nSkip++
			}
			if w["flags"].(map[string]any)["from_cache"].(bool) {
				nCache++
			}
		}
	}
	t.Logf("recorded pages with rows: %v (%d row(s) in the first, ordinals from %d); without: %v; over all %d recorded row(s): %d repeat(s), %d with a failure origin, %d with a skip token, %d from the cache (a count of 0 is a field this test only compared as empty)",
		populated, len(recordedRows(t, populated[0])), num(t, recordedRows(t, populated[0])[0]["ordinal"]), empty, nRows, nDup, nOrigin, nSkip, nCache)
}

// TestResults_ReadsOriginSkipTokenAndCache: the three provenance fields the charge table reads come out of their nested places (flags.from_cache, signals.smtp_skipped_reason,
// signals.failure_origin), and the cache signal is flags.from_cache and NOT meta.from_cache (an element whose meta says true and whose flags say false is not cached: M1-F6).
// Control: the same element without them decodes to empty values.
func TestResults_ReadsOriginSkipTokenAndCache(t *testing.T) {
	read := func(body string) Row {
		t.Helper()
		f := serve(t, 200, page(testJob, "", body))
		rows, _, err := f.adapter(t).Results(testCtx(t), testJob, "", 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%d row(s), %v for %s", len(rows), err, body)
		}
		return rows[0]
	}
	got := read(`{"status":"unknown","reason":"dns_error","domain":"gone.example","flags":{"from_cache":true},` +
		`"signals":{"smtp_skipped_reason":"smtp_unwired","failure_origin":"remote","error_capped":true,"retry_after_hint_s":30},"item_id":"row-7","ordinal":7}`)
	want := Row{Ordinal: 7, ItemID: "row-7", Status: "unknown", Reason: "dns_error", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "remote"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
	// the other cache key: meta.from_cache says true, flags.from_cache says false, and the row is NOT cached
	metaOnly := read(`{"status":"valid","reason":"accepted","flags":{"from_cache":false},"signals":{},"meta":{"from_cache":true},"item_id":"row-8","ordinal":8}`)
	if metaOnly.FromCache {
		t.Fatal("meta.from_cache was read as the cache signal: a bulk result's meta is skeletal and always false, flags.from_cache is the signal")
	}
	ctl := read(element("unknown", "dns_error", "row-7", 7, ""))
	if ctl.FromCache || ctl.SkipReason != "" || ctl.FailureOrigin != "" {
		t.Fatalf("control: an element with no provenance decoded to %+v: the three fields must read as empty when the module sends nothing", ctl)
	}
}

// TestResults_StrictOnEveryKeyTheAdapterReads: an element with a missing, null or mistyped key, a value outside its closed vocabulary, a negative ordinal, a blank item_id or a
// duplicate_of that is not an EARLIER row is a Contract error that names the key, and the page returns no rows. (The cases of the old internal/m1results Row.UnmarshalJSON, now
// asked of the adapter's real path; the keys it did not read, the vocabularies and item_id are new.) Each case is paired with the control that the same element, repaired, is
// accepted.
func TestResults_StrictOnEveryKeyTheAdapterReads(t *testing.T) {
	const good = `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`
	f := serve(t, 200, page(testJob, "", good))
	if rows, _, err := f.adapter(t).Results(testCtx(t), testJob, "", 0); err != nil || len(rows) != 1 {
		t.Fatalf("control: the repaired element is refused: %v", err)
	}
	cases := []struct{ name, elem, key string }{
		{"no status", `{"reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"an empty status", `{"status":"","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"a blank status", `{"status":"  ","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"a null status", `{"status":null,"reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"a status outside the four", `{"status":"maybe","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"a status of the wrong type", `{"status":7,"reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].status"},
		{"no reason", `{"status":"valid","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].reason"},
		{"a reason outside the vocabulary", `{"status":"valid","reason":"vibes","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].reason"},
		{"no ordinal", `{"status":"valid","reason":"accepted","item_id":"a","flags":{"from_cache":false},"signals":{}}`, "results[0].ordinal"},
		{"a null ordinal", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":null,"flags":{"from_cache":false},"signals":{}}`, "results[0].ordinal"},
		{"a negative ordinal", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":-1,"flags":{"from_cache":false},"signals":{}}`, "results[0].ordinal"},
		{"an ordinal of the wrong type", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":"4","flags":{"from_cache":false},"signals":{}}`, "results[0].ordinal"},
		{"an ordinal that is not an integer", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4.5,"flags":{"from_cache":false},"signals":{}}`, "results[0].ordinal"},
		{"a duplicate of itself", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].duplicate_of"},
		{"a duplicate of a later row", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":9,"flags":{"from_cache":false},"signals":{}}`, "results[0].duplicate_of"},
		{"a duplicate of a negative row", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"duplicate_of":-2,"flags":{"from_cache":false},"signals":{}}`, "results[0].duplicate_of"},
		{"no item_id", `{"status":"valid","reason":"accepted","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].item_id"},
		{"a blank item_id", `{"status":"valid","reason":"accepted","item_id":"  ","ordinal":4,"flags":{"from_cache":false},"signals":{}}`, "results[0].item_id"},
		{"no flags", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"signals":{}}`, "results[0].flags"},
		{"flags without from_cache", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"flags":{},"signals":{}}`, "results[0].flags.from_cache"},
		{"from_cache of the wrong type", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":"no"},"signals":{}}`, "results[0].flags.from_cache"},
		{"no signals", `{"status":"valid","reason":"accepted","item_id":"a","ordinal":4,"flags":{"from_cache":false}}`, "results[0].signals"},
		{"a failure origin outside the three", `{"status":"unknown","reason":"dns_error","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{"failure_origin":"theirs"}}`, "results[0].signals.failure_origin"},
		{"a failure origin of the wrong type", `{"status":"unknown","reason":"dns_error","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{"failure_origin":1}}`, "results[0].signals.failure_origin"},
		{"a skip token outside the six", `{"status":"risky","reason":"plausible","item_id":"a","ordinal":4,"flags":{"from_cache":false},"signals":{"smtp_skipped_reason":"lazy"}}`, "results[0].signals.smtp_skipped_reason"},
		{"an element that is not an object", `[1,2]`, "results[0]"},
	}
	for _, c := range cases {
		g := serve(t, 200, page(testJob, "", c.elem))
		rows, next, err := g.adapter(t).Results(testCtx(t), testJob, "", 0)
		kind, key := kindAndKey(err)
		if err == nil || kind != modclient.Contract || key != c.key {
			t.Errorf("%s: got kind %v key %q (%v), want a Contract error naming %q", c.name, kind, key, err, c.key)
		}
		if rows != nil || next != "" {
			t.Errorf("%s: the refused page returned %d row(s) and cursor %q beside the error", c.name, len(rows), next)
		}
	}
}

// TestResults_ErrorsCarryNoValueFromTheElement: a refused element's error text is built from fixed words, ordinals and key names only. An element whose item_id, domain and
// reason hold an address (a module that echoes what it was given) puts none of it in the error. Control: leaks() sees an address in a string that holds one.
func TestResults_ErrorsCarryNoValueFromTheElement(t *testing.T) {
	bad := `{"status":"valid","reason":"` + testAddr + `","domain":"leak-check.example","item_id":"` + testAddr + `","ordinal":4,"email":"` + testAddr + `","flags":{"from_cache":false},"signals":{}}`
	f := serve(t, 200, page(testJob, "", bad))
	_, _, err := f.adapter(t).Results(testCtx(t), testJob, "", 0)
	if modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("an element with a reason outside the vocabulary was not refused as a contract error: %v", err)
	}
	if leaks(err.Error(), testAddr, "leak-check.example") {
		t.Fatalf("the error text carries a value taken from the element: %q", err)
	}
	if !leaks("refused: "+testAddr, testAddr) {
		t.Fatal("control: leaks() did not see an address in a string that holds one: the check above proves nothing")
	}
}

// TestResults_APageWithOneBadElementReturnsNothing: a page of three rows whose MIDDLE row is off contract fails whole (partial results are never returned as complete). Control:
// the same page with the middle row repaired returns all three.
func TestResults_APageWithOneBadElementReturnsNothing(t *testing.T) {
	bad := element("valid", "not-a-reason", "row-1", 1, "")
	f := serve(t, 200, page(testJob, "", row(0), bad, row(2)))
	rows, next, err := f.adapter(t).Results(testCtx(t), testJob, "", 0)
	if kind, key := kindAndKey(err); kind != modclient.Contract || key != "results[1].reason" || rows != nil || next != "" {
		t.Fatalf("got %d row(s), cursor %q, kind %v key %q (%v), want nothing and a Contract error naming results[1].reason", len(rows), next, kind, key, err)
	}
	g := serve(t, 200, page(testJob, "", row(0), row(1), row(2)))
	if rows, _, err := g.adapter(t).Results(testCtx(t), testJob, "", 0); err != nil || len(rows) != 3 {
		t.Fatalf("control: %d row(s), %v", len(rows), err)
	}
}

// TestResults_APageAboutAnotherJobIsRefused: the page names a job other than the one asked for. Control: the right job id is accepted (in either letter case).
func TestResults_APageAboutAnotherJobIsRefused(t *testing.T) {
	other := "00000000-0000-7000-8000-0000000000ff"
	f := serve(t, 200, page(other, "", row(0)))
	if _, _, err := f.adapter(t).Results(testCtx(t), testJob, "", 0); modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("got %v, want a Contract error", err)
	} else if _, key := kindAndKey(err); key != "job_id" {
		t.Fatalf("the error names %q, want job_id", key)
	}
	g := serve(t, 200, page(strings.ToUpper(testJob), "", row(0)))
	if _, _, err := g.adapter(t).Results(testCtx(t), testJob, "", 0); err != nil {
		t.Fatalf("control: the same job in upper case was refused: %v", err)
	}
}

// TestResults_CursorAndLimitTravelAndAreChecked: the cursor goes back unchanged and the limit as given; 0 sends no limit (module 1's default); a limit outside 0..1000 is refused
// BEFORE any request, because module 1 would silently answer with its default (M1-F12). Control: 1 and 1000 are sent.
func TestResults_CursorAndLimitTravelAndAreChecked(t *testing.T) {
	f := newFakeM1(t, func(call) (int, string) { return 200, page(testJob, "") })
	a := f.adapter(t)
	if _, _, err := a.Results(testCtx(t), testJob, "op aque/cur+sor=", 7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Results(testCtx(t), testJob, "", 0); err != nil {
		t.Fatal(err)
	}
	calls := f.seen()
	if p := calls[0].params(); p.Get("cursor") != "op aque/cur+sor=" || p.Get("limit") != "7" {
		t.Fatalf("the first request carried %v", p)
	}
	if calls[1].Query != "" {
		t.Fatalf("a first page with no limit must send no query, sent %q", calls[1].Query)
	}
	before := f.count()
	for _, bad := range []int{-1, 1001, 100000} {
		_, _, err := a.Results(testCtx(t), testJob, "", bad)
		if modclient.KindOf(err) != modclient.Invalid {
			t.Errorf("limit %d: got %v, want an Invalid error", bad, err)
		}
	}
	if f.count() != before {
		t.Fatalf("a refused argument still made %d request(s)", f.count()-before)
	}
	for _, ok := range []int{1, 1000} {
		if _, _, err := a.Results(testCtx(t), testJob, "", ok); err != nil {
			t.Errorf("control: limit %d was refused: %v", ok, err)
		}
	}
}

// TestResults_AForeignCursorIsRefusedByTheModule: a cursor module 1 did not issue is its 400 validation_failed on the field cursor; the adapter reports it as a Refused error naming
// the route, the status, the code and the detail, and the error text does not repeat the cursor. Control: the cursor the stand-in did issue works.
func TestResults_AForeignCursorIsRefusedByTheModule(t *testing.T) {
	l := newListing(3, 0, 1, 2)
	f := newFakeM1(t, l.handle)
	a := f.adapter(t)
	_, _, err := a.Results(testCtx(t), testJob, "garbage-"+testAddr, 2)
	e := asModclient(err)
	if e == nil || e.Kind != modclient.Refused || e.Status != 400 || e.Code != "validation_failed" || e.Route != "m1.results" ||
		!reflect.DeepEqual(e.Details, []modclient.Detail{{Field: "cursor", Issue: "malformed"}}) {
		t.Fatalf("got %v, want a Refused error 400 validation_failed naming the field cursor on m1.results", err)
	}
	if leaks(err.Error(), testAddr, "garbage") {
		t.Fatalf("the error repeats the cursor: %q", err)
	}
	if _, _, err := a.Results(testCtx(t), testJob, "c0", 2); err != nil {
		t.Fatalf("control: the cursor the module issued was refused: %v", err)
	}
}

// ---- ToSettleItems ----

// TestToSettleItems_ItemRefIsTheItemIDNeverTheOrdinal: the money.SettleItem's item_ref is the module's item_id. The labels here are deliberately not the ordinals, and the control
// shows that an ordinal-keyed item would differ from every one of them.
func TestToSettleItems_ItemRefIsTheItemIDNeverTheOrdinal(t *testing.T) {
	rows := []Row{
		{Ordinal: 0, ItemID: "acct-77/row-9", Status: "valid", Reason: "accepted"},
		{Ordinal: 1, ItemID: "7", Status: "invalid", Reason: "no_mx"},
		{Ordinal: 2, ItemID: "0", Status: "risky", Reason: "disposable"},
	}
	items, err := ToSettleItems(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(rows) {
		t.Fatalf("%d item(s) for %d row(s)", len(items), len(rows))
	}
	for i, it := range items {
		if it.ItemRef != rows[i].ItemID {
			t.Errorf("row %d: item_ref %q, want the item_id %q", i, it.ItemRef, rows[i].ItemID)
		}
		if it.ItemRef == strconv.Itoa(rows[i].Ordinal) {
			t.Errorf("row %d: the item_ref %q is the ordinal: a position is not a key", i, it.ItemRef)
		}
		if it.Outcome.Module != "m1" || it.Outcome.State != rows[i].Status || it.Outcome.Reason != rows[i].Reason {
			t.Errorf("row %d: outcome %+v is not module 1's own words (%s/%s)", i, it.Outcome, rows[i].Status, rows[i].Reason)
		}
	}
	// control: rows whose item_id IS their ordinal's spelling would be indistinguishable from an ordinal key, which is why the rows above are not
	same, err := ToSettleItems([]Row{{Ordinal: 5, ItemID: "5", Status: "valid", Reason: "accepted"}})
	if err != nil || same[0].ItemRef != "5" {
		t.Fatalf("control: %v %+v", err, same)
	}
}

// TestToSettleItems_ADuplicateIsThePlatformDuplicateOutcome: a row with duplicate_of is platform/duplicate whatever verdict it carries (the first row of the address carries the
// charge), and it is the pointer's PRESENCE that decides, not its value: duplicate_of 0 is a duplicate of row 0. Control: the same verdict with no duplicate_of is module 1's own
// outcome.
func TestToSettleItems_ADuplicateIsThePlatformDuplicateOutcome(t *testing.T) {
	first := Row{Ordinal: 0, ItemID: "row-0", Status: "valid", Reason: "accepted"}
	repeat := Row{Ordinal: 2, ItemID: "row-2", DuplicateOf: ptr(0), Status: "valid", Reason: "accepted", FromCache: true, SkipReason: "x", FailureOrigin: "remote"}
	items, err := ToSettleItems([]Row{first, repeat})
	if err != nil {
		t.Fatal(err)
	}
	if want := (charge.Outcome{Module: "platform", State: "duplicate"}); items[1].Outcome != want {
		t.Fatalf("the repeat's outcome is %+v, want exactly %+v (no reason, no cache flag, no origin: it is the platform's own class)", items[1].Outcome, want)
	}
	if want := (charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"}); items[0].Outcome != want {
		t.Fatalf("control: the first row's outcome is %+v, want module 1's own %+v", items[0].Outcome, want)
	}
	if items[1].ItemRef != "row-2" {
		t.Fatalf("the repeat keeps its own item_id as item_ref, got %q", items[1].ItemRef)
	}
}

// TestToSettleItems_AnUnknownRowCarriesItsOrigin: the failure origin, the SMTP skip token and the cache flag travel into the outcome unchanged, for every value, so that
// money.Settle can decide on them. Control: two rows that differ only in the origin give outcomes that differ only in the origin.
func TestToSettleItems_AnUnknownRowCarriesItsOrigin(t *testing.T) {
	for _, origin := range []string{"own", "remote", "undetermined", ""} {
		items, err := ToSettleItems([]Row{{Ordinal: 3, ItemID: "row-3", Status: "unknown", Reason: "dns_error", FailureOrigin: origin}})
		if err != nil {
			t.Fatal(err)
		}
		want := charge.Outcome{Module: "m1", State: "unknown", Reason: "dns_error", FailureOrigin: origin}
		if items[0].Outcome != want {
			t.Errorf("origin %q: outcome %+v, want %+v", origin, items[0].Outcome, want)
		}
	}
	items, err := ToSettleItems([]Row{{Ordinal: 1, ItemID: "x", Status: "risky", Reason: "plausible", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "own"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := (charge.Outcome{Module: "m1", State: "risky", Reason: "plausible", FromCache: true, SkipReason: "smtp_unwired", FailureOrigin: "own"}); items[0].Outcome != want {
		t.Fatalf("outcome %+v, want %+v", items[0].Outcome, want)
	}
	a, _ := ToSettleItems([]Row{{ItemID: "x", Status: "unknown", Reason: "dns_error", FailureOrigin: "own"}})
	b, _ := ToSettleItems([]Row{{ItemID: "x", Status: "unknown", Reason: "dns_error", FailureOrigin: "remote"}})
	if a[0].Outcome == b[0].Outcome {
		t.Fatal("control: rows that differ only in the failure origin gave the same outcome: the origin is dropped on the way")
	}
}

// TestToSettleItems_NoItemIDIsAnError: a result with no item_id (empty, blank) is a Contract error (cause ErrNoItemID) naming the ordinal; and a page that holds one such row is
// refused whole and returns nothing (a page that cannot be keyed completely is not settled in part). Control: the same page with the label present is accepted in full. A decoded
// element with no item_id never gets this far: Results refuses it first, naming results[i].item_id (TestResults_StrictOnEveryKeyTheAdapterReads).
func TestToSettleItems_NoItemIDIsAnError(t *testing.T) {
	for _, id := range []string{"", " ", "\t"} {
		page := []Row{
			{Ordinal: 3, ItemID: "row-3", Status: "valid", Reason: "accepted"},
			{Ordinal: 4, ItemID: id, Status: "valid", Reason: "accepted"},
		}
		items, err := ToSettleItems(page)
		if !errors.Is(err, ErrNoItemID) || modclient.KindOf(err) != modclient.Contract {
			t.Fatalf("item_id %q: got %v, want a Contract error with cause ErrNoItemID", id, err)
		}
		if !strings.Contains(err.Error(), "ordinal 4") {
			t.Errorf("item_id %q: the error does not name the ordinal: %v", id, err)
		}
		if items != nil {
			t.Errorf("item_id %q: %d item(s) were returned beside the error: a page is settled whole or not at all", id, len(items))
		}
	}
	ok := Row{Ordinal: 4, ItemID: "row-4", Status: "valid", Reason: "accepted"}
	if items, err := ToSettleItems([]Row{{Ordinal: 3, ItemID: "row-3", Status: "valid", Reason: "accepted"}, ok}); err != nil || len(items) != 2 {
		t.Fatalf("control: the same page with its labels present: %d item(s), %v", len(items), err)
	}
}

// TestToSettleItems_ProducesWhatMoneySettleAccepts: the items are the shape money.Settle takes (compile-time: money.SettleItem) and their item_refs are unique for rows with unique
// item_ids, which money's validation requires. Control: two rows with one item_id give two items with one item_ref, which is the thing money.Settle refuses (ErrBadItem): the
// mapping does not hide it by renaming.
func TestToSettleItems_ProducesWhatMoneySettleAccepts(t *testing.T) {
	mustItems := func(rows []Row) []money.SettleItem {
		t.Helper()
		items, err := ToSettleItems(rows)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	var _ []money.SettleItem = mustItems([]Row{{ItemID: "a", Status: "valid", Reason: "accepted"}})
	items := mustItems([]Row{{Ordinal: 0, ItemID: "a", Status: "valid", Reason: "accepted"}, {Ordinal: 1, ItemID: "b", Status: "valid", Reason: "accepted"}})
	if items[0].ItemRef == items[1].ItemRef {
		t.Fatal("two rows with two item_ids gave one item_ref")
	}
	clash := mustItems([]Row{{Ordinal: 0, ItemID: "a", Status: "valid", Reason: "accepted"}, {Ordinal: 1, ItemID: "a", Status: "valid", Reason: "accepted"}})
	if clash[0].ItemRef != clash[1].ItemRef {
		t.Fatal("control: two rows with one item_id were given different item_refs: the mapping invents keys")
	}
}
