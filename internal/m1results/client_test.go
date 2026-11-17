package m1results

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// The client is tested against in-process stand-ins that speak module 1's documented wire format. They are for THESE unit tests only: the module itself is
// never mocked in the harness, where tests/harness/m1_bulk_settle_test.go drives this client against the real module 1 and compares the settlement with the
// module's own report.

const (
	// testKey is the per-caller key every test client uses. Not a real secret; recognisable, so a test can search every string it produces for it.
	testKey = "zzzz-m1results-key-zzzz"
	// testAddr stands for an address that must never appear in an error string.
	testAddr = "jane.private@leak-check.example"

	// docSubmit202 is section 3 of module 1's bulk-item-identity.md, verbatim.
	docSubmit202 = `{
  "job_id": "019a…",
  "status": "accepted",
  "counts": {"total": 4, "enqueued": 2, "duplicate": 1, "malformed": 1},
  "malformed_rows": [{"ordinal": 3, "item_id": "row-3"}],
  "malformed_rows_truncated": false,
  "processing_paused": false
}`
	// docStatus is section 4, verbatim.
	docStatus = `{"job_id": "019a…", "status": "complete", "complete": true, "caller_ref": "platform-job-42",
 "counts": {"total": 4, "settled": 4, "pending": 0}, "processing_paused": false}`
	// docCursor400 is module 1's error envelope (api-contract.md, "Errors") for a cursor it did not issue (bulk-item-identity.md section 5: 400 validation_failed on
	// field cursor, malformed).
	docCursor400 = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"cursor","issue":"malformed"}],"request_id":"01997f4e-0000-7000-8000-000000000000"}}`
)

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func testCtx(t *testing.T) context.Context { return ctxFor(t, 30*time.Second) }

// leaks reports whether text contains any of the secrets. It is itself tested against a string that does contain one.
func leaks(text string, secrets ...string) bool {
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// call is one request a stand-in received.
type call struct {
	Method, Path, Query, Auth, ContentType string
	Body                                   []byte
}

func (c call) params() url.Values {
	v, _ := url.ParseQuery(c.Query)
	return v
}

// fakeM1 answers every request with what handle says and records it.
type fakeM1 struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []call
	handle func(c call) (status int, body string)
}

func newFakeM1(t *testing.T, handle func(c call) (int, string)) *fakeM1 {
	t.Helper()
	f := &fakeM1{handle: handle}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"), Body: b}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		status, body := f.handle(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeM1) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(f.srv.URL, config.Secret(testKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Poll = time.Millisecond
	return c
}

func (f *fakeM1) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeM1) count() int { return len(f.seen()) }

// page builds the body of GET /v1/jobs/{id}/results (the envelope api.go's handleJobResults writes).
func page(jobID, next string, elems ...string) string {
	return `{"job_id":` + strconv.Quote(jobID) + `,"results":[` + strings.Join(elems, ",") + `],"next_cursor":` + strconv.Quote(next) + `}`
}

func row(ordinal int) string {
	return element("valid", "accepted", "row-"+strconv.Itoa(ordinal), ordinal, "")
}

// ---- the three calls ----

// TestSubmit_SendsTheItemsShapeWithTheBearerKey: POST /v1/verify/bulk with `Authorization: Bearer <key>`, a JSON body that is exactly {"items":[{item_id,
// address}...]} (no `addresses` key: module 1 refuses both), and the 202 of the module's own contract decodes to the job id and the counts. Control: the
// stand-in answers 401 to any other credential, so a client that sent the wrong header would be seen; and one with the wrong key is.
func TestSubmit_SendsTheItemsShapeWithTheBearerKey(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		if c.Auth != "Bearer "+testKey {
			return 401, `{"error":{"code":"unauthenticated","message":"x","request_id":"r"}}`
		}
		return 202, docSubmit202
	})
	c := f.client(t)
	items := []Item{{"row-0", "jane@acmecorp.com"}, {"row-1", "bob@acmecorp.com"}, {"row-2", "Jane@AcmeCorp.com"}, {"row-3", "not-an-address"}}
	id, counts, err := c.Submit(testCtx(t), items)
	if err != nil {
		t.Fatal(err)
	}
	if id != "019a…" || counts != (SubmitCounts{Total: 4, Enqueued: 2, Duplicate: 1, Malformed: 1}) {
		t.Fatalf("Submit returned %q %+v", id, counts)
	}
	calls := f.seen()
	if len(calls) != 1 {
		t.Fatalf("%d request(s), want 1", len(calls))
	}
	got := calls[0]
	if got.Method != http.MethodPost || got.Path != "/v1/verify/bulk" || got.Auth != "Bearer "+testKey || got.ContentType != "application/json" {
		t.Fatalf("the request was %+v", got)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["items"] == nil {
		t.Fatalf("the body has keys %v, want exactly [items]", keysOf(body))
	}
	var sent []Item
	if err := json.Unmarshal(body["items"], &sent); err != nil || !reflect.DeepEqual(sent, items) {
		t.Fatalf("the items sent were %+v (%v), want %+v", sent, err, items)
	}

	// control: another key is refused by the stand-in, and the client reports the 401 by status and route
	bad, err := New(f.srv.URL, config.Secret("zzzz-another-key-zzzz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = bad.Submit(testCtx(t), items)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 401 || se.Code != "unauthenticated" || se.Route != "POST /v1/verify/bulk" {
		t.Fatalf("control: the wrong key gave %v, want a StatusError 401 unauthenticated on POST /v1/verify/bulk", err)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestJob_ReadsTheDocumentedStatus: GET /v1/jobs/{id} with the key, and section 4's body decodes to status, complete and counts. Control: a body that says
// pending > 0 and complete false decodes as such (the decoder does not default to complete).
func TestJob_ReadsTheDocumentedStatus(t *testing.T) {
	running := `{"job_id":"019a…","status":"running","complete":false,"caller_ref":"","counts":{"total":4,"settled":1,"pending":3},"processing_paused":true}`
	body := docStatus
	f := newFakeM1(t, func(call) (int, string) { return 200, body })
	c := f.client(t)
	got, err := c.Job(testCtx(t), "019a…")
	if err != nil {
		t.Fatal(err)
	}
	want := JobStatus{JobID: "019a…", Status: "complete", Complete: true, Counts: JobCounts{Total: 4, Settled: 4, Pending: 0}}
	if got != want {
		t.Fatalf("Job returned %+v, want %+v", got, want)
	}
	if r := f.seen()[0]; r.Method != http.MethodGet || r.Path != "/v1/jobs/019a…" || r.Auth != "Bearer "+testKey {
		t.Fatalf("the request was %+v", r)
	}
	body = running
	ctl, err := c.Job(testCtx(t), "019a…")
	if err != nil || ctl.Complete || ctl.Counts.Pending != 3 || !ctl.ProcessingPaused || ctl.Status != "running" {
		t.Fatalf("control: %+v, %v", ctl, err)
	}
}

// TestResults_ReadsADocumentedPage: a page of section 5's own elements (the repeat of row 0, then the malformed row) with a next cursor decodes to two
// rows in order and the cursor. Control: the last page ("" cursor) gives "" and an empty page gives no rows, not an error.
func TestResults_ReadsADocumentedPage(t *testing.T) {
	body := page("J1", "3", docResultElement, docMalformedElement)
	f := newFakeM1(t, func(call) (int, string) { return 200, body })
	c := f.client(t)
	rows, next, err := c.Results(testCtx(t), "J1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Ordinal: 2, ItemID: "row-2", DuplicateOf: ptr(0), Status: "valid", Reason: "accepted"},
		{Ordinal: 3, ItemID: "row-3", Status: "invalid", Reason: "invalid_syntax"},
	}
	if !reflect.DeepEqual(rows, want) || next != "3" {
		t.Fatalf("Results returned %+v next %q, want %+v next %q", rows, next, want, "3")
	}
	body = page("J1", "")
	rows, next, err = c.Results(testCtx(t), "J1", "3", 0)
	if err != nil || len(rows) != 0 || next != "" {
		t.Fatalf("control: an empty last page gave %+v %q %v", rows, next, err)
	}
}

// TestResults_CursorAndLimitTravelAndAreChecked: the cursor goes back unchanged and the limit as given; 0 sends no limit (module 1's default); a limit outside
// 0..1000 is refused BEFORE any request, because module 1 would silently answer with its default. Control: 1 and 1000 are sent.
func TestResults_CursorAndLimitTravelAndAreChecked(t *testing.T) {
	f := newFakeM1(t, func(call) (int, string) { return 200, page("J1", "") })
	c := f.client(t)
	if _, _, err := c.Results(testCtx(t), "J1", "op aque/cur+sor=", 7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Results(testCtx(t), "J1", "", 0); err != nil {
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
		_, _, err := c.Results(testCtx(t), "J1", "", bad)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("limit %d: got %v, want ErrInvalid", bad, err)
		}
	}
	if _, _, err := c.Results(testCtx(t), "  ", "", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("a blank job id: got %v, want ErrInvalid", err)
	}
	if f.count() != before {
		t.Fatalf("a refused argument still made %d request(s)", f.count()-before)
	}
	for _, ok := range []int{1, 1000} {
		if _, _, err := c.Results(testCtx(t), "J1", "", ok); err != nil {
			t.Errorf("control: limit %d was refused: %v", ok, err)
		}
	}
}

// ---- Drain ----

// listing is a stand-in for module 1's listing: a job of n rows, each settled or not; it lists the settled rows in ordinal order, a cursor is the ordinal of
// the last row returned, a page carries a next cursor only when a settled row follows (module 1 looks ahead by one and takes the cursor from the last row
// RETURNED), and a cursor it did not issue is a 400. onJob runs after each status read (it models the module settling more rows while the caller waits).
type listing struct {
	mu      sync.Mutex
	n       int
	settled map[int]bool
	onJob   func(l *listing, jobCall int)
	jobs    int
	results int
	cursors []string
}

func newListing(n int, settled ...int) *listing {
	l := &listing{n: n, settled: map[int]bool{}}
	for _, o := range settled {
		l.settled[o] = true
	}
	return l
}

func (l *listing) settleAll() {
	for i := 0; i < l.n; i++ {
		l.settled[i] = true
	}
}

func (l *listing) settledCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.settled)
}

func (l *listing) handle(c call) (int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.HasSuffix(c.Path, "/results") {
		l.results++
		cur := c.params().Get("cursor")
		l.cursors = append(l.cursors, cur)
		start := -1
		if cur != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(cur, "c"))
			if err != nil || !strings.HasPrefix(cur, "c") {
				return 400, docCursor400
			}
			start = n
		}
		limit := 100
		if v := c.params().Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		var later []int
		for o := start + 1; o < l.n; o++ {
			if l.settled[o] {
				later = append(later, o)
			}
		}
		take := later
		if len(take) > limit {
			take = take[:limit]
		}
		var elems []string
		for _, o := range take {
			elems = append(elems, row(o))
		}
		next := ""
		if len(take) > 0 && len(later) > len(take) {
			next = "c" + strconv.Itoa(take[len(take)-1])
		}
		return 200, page("J1", next, elems...)
	}
	l.jobs++
	done := len(l.settled)
	status := "running"
	if done == l.n {
		status = "complete"
	}
	body := fmt.Sprintf(`{"job_id":"J1","status":%q,"complete":%t,"caller_ref":"","counts":{"total":%d,"settled":%d,"pending":%d},"processing_paused":false}`, status, done == l.n, l.n, done, l.n-done)
	if l.onJob != nil {
		l.onJob(l, l.jobs)
	}
	return 200, body
}

func ordinalsOf(rows []Row) []int {
	var o []int
	for _, r := range rows {
		o = append(o, r.Ordinal)
	}
	return o
}

// TestDrain_WalksThreePagesAndAnEmptyLastPage: a complete job read in pages of two: the stand-in's third page is EMPTY with no next cursor (the end of a list
// that was exactly two pages long). fn is called once per non-empty page, in order, with the rows of that page and no others, never for the empty page; the
// cursors sent are the ones the pages returned, unchanged; every row arrives once. Control: the same job read with a page of 100 is ONE call of fn with all
// four rows, so the per-page behaviour comes from the page size and not from the stand-in.
func TestDrain_WalksThreePagesAndAnEmptyLastPage(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.Path, "/results"):
			switch c.params().Get("cursor") {
			case "":
				return 200, page("J1", "c1", row(0), row(1))
			case "c1":
				return 200, page("J1", "c3", row(2), row(3))
			case "c3":
				return 200, page("J1", "") // the empty last page
			}
			return 400, docCursor400
		default:
			return 200, `{"job_id":"J1","status":"complete","complete":true,"counts":{"total":4,"settled":4,"pending":0}}`
		}
	})
	c := f.client(t)
	c.PageLimit = 2
	var pages [][]int
	st, err := c.Drain(testCtx(t), "J1", func(rows []Row) error {
		pages = append(pages, ordinalsOf(rows))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int{{0, 1}, {2, 3}}; !reflect.DeepEqual(pages, want) {
		t.Fatalf("fn saw the pages %v, want %v (the empty third page must not reach it)", pages, want)
	}
	if st != (DrainStats{Pages: 2, Rows: 4}) {
		t.Fatalf("stats %+v, want 2 pages, 4 rows, no waits, no catch-up", st)
	}
	var queries []string
	for _, cl := range f.seen() {
		if strings.HasSuffix(cl.Path, "/results") {
			queries = append(queries, cl.Query)
		}
	}
	if want := []string{"limit=2", "cursor=c1&limit=2", "cursor=c3&limit=2"}; !reflect.DeepEqual(queries, want) {
		t.Fatalf("the result requests were %v, want %v", queries, want)
	}

	// control: one big page
	g := newFakeM1(t, func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/results") {
			return 200, page("J1", "", row(0), row(1), row(2), row(3))
		}
		return 200, `{"job_id":"J1","status":"complete","complete":true,"counts":{"total":4,"settled":4,"pending":0}}`
	})
	var calls int
	if _, err := g.client(t).Drain(testCtx(t), "J1", func(rows []Row) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("control: a page of 100 gave %d call(s) of fn (%v), want 1", calls, err)
	}
}

// TestDrain_SettlesAsRowsArriveAndCatchesARowBehindTheCursor: a running job of six rows where row 1 settles AFTER the cursor has passed it (module 1 does not
// revisit a position). Drain hands fn the first page while the job is still running, waits, follows the cursor to the end, and once the job is complete
// delivers the missed row by one full walk: every ordinal exactly once, row 1 last. Control: a plain walker over the same stand-in, which is what a client
// that trusts next_cursor would be, ends with five rows and misses row 1: the hazard is real, so the catch-up is what delivered it.
func TestDrain_SettlesAsRowsArriveAndCatchesARowBehindTheCursor(t *testing.T) {
	script := func() *listing {
		l := newListing(6, 0, 2, 3)
		l.onJob = func(l *listing, _ int) { l.settleAll() } // after the first status read the module settles the rest, row 1 included
		return l
	}

	l := script()
	f := newFakeM1(t, l.handle)
	c := f.client(t)
	c.PageLimit = 2
	var pages [][]int
	var settledAtFirstPage int
	st, err := c.Drain(testCtx(t), "J1", func(rows []Row) error {
		if len(pages) == 0 {
			settledAtFirstPage = l.settledCount() // read through the stand-in's own lock, from the callback's goroutine
		}
		pages = append(pages, ordinalsOf(rows))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int{{0, 2}, {3}, {4}, {5}, {1}}; !reflect.DeepEqual(pages, want) {
		t.Fatalf("fn saw %v, want %v", pages, want)
	}
	if settledAtFirstPage != 3 {
		t.Fatalf("the first page was handed over when %d of 6 rows had settled, want 3: Drain must not wait for the job", settledAtFirstPage)
	}
	if st.Pages != 5 || st.Rows != 6 || st.Waits != 1 || st.CatchUp != 1 {
		t.Fatalf("stats %+v, want 5 pages, 6 rows, 1 wait, 1 row caught up", st)
	}

	// control: the walker that trusts next_cursor
	l2 := script()
	g := newFakeM1(t, l2.handle)
	gc := g.client(t)
	ctx := testCtx(t)
	got := map[int]bool{}
	cursor := ""
	for pass := 0; pass < 2; pass++ {
		for {
			rows, next, err := gc.Results(ctx, "J1", cursor, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				got[r.Ordinal] = true
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if pass == 0 {
			if _, err := gc.Job(ctx, "J1"); err != nil { // the job read that makes the stand-in settle the rest
				t.Fatal(err)
			}
		}
	}
	if got[1] || len(got) != 5 {
		t.Fatalf("control: a walker that trusts next_cursor read %v: it should have missed row 1 and read five rows, otherwise the stand-in does not reproduce the hazard and the catch-up proves nothing", got)
	}
}

// TestDrain_ACompleteJobWhoseRowsDoNotAddUpIsRefused: the job says complete with four rows but the listing, however it is walked, yields three (a
// pagination that skips a row, module 2's bug M2-G5). Drain returns ErrIncomplete naming the numbers and delivers no more than it read; it never reports a
// job it did not read in full. Control: the same script with row 2 present drains cleanly.
func TestDrain_ACompleteJobWhoseRowsDoNotAddUpIsRefused(t *testing.T) {
	build := func(skip bool) *fakeM1 {
		return newFakeM1(t, func(c call) (int, string) {
			if !strings.HasSuffix(c.Path, "/results") {
				return 200, `{"job_id":"J1","status":"complete","complete":true,"counts":{"total":4,"settled":4,"pending":0}}`
			}
			if c.params().Get("cursor") == "" {
				return 200, page("J1", "c1", row(0), row(1))
			}
			if skip {
				return 200, page("J1", "", row(3))
			}
			return 200, page("J1", "", row(2), row(3))
		})
	}
	var rows int
	c := build(true).client(t)
	c.PageLimit = 2
	st, err := c.Drain(testCtx(t), "J1", func(r []Row) error { rows += len(r); return nil })
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "3 row(s) read") || !strings.Contains(err.Error(), "total 4") {
		t.Fatalf("got %v, want ErrIncomplete naming 3 rows read of a total of 4", err)
	}
	if rows != 3 || st.Rows != 3 {
		t.Fatalf("%d row(s) reached fn (stats %+v), want the 3 that were read", rows, st)
	}
	rows = 0
	d := build(false).client(t)
	d.PageLimit = 2
	if _, err := d.Drain(testCtx(t), "J1", func(r []Row) error { rows += len(r); return nil }); err != nil || rows != 4 {
		t.Fatalf("control: the complete listing gave %d row(s) and %v", rows, err)
	}
}

// TestDrain_ATimeoutNamesWhatTheModuleLastSaid: a job that never completes (one row settled of three) is waited for until the context's deadline, and the
// error unwraps to context.DeadlineExceeded and says what module 1 last reported and how many rows were delivered. Control: with a deadline long enough
// and the rows settling, the same stand-in drains.
func TestDrain_ATimeoutNamesWhatTheModuleLastSaid(t *testing.T) {
	l := newListing(3, 0)
	f := newFakeM1(t, l.handle)
	c := f.client(t)
	c.Poll = 5 * time.Millisecond
	var delivered int
	_, err := c.Drain(ctxFor(t, 300*time.Millisecond), "J1", func(r []Row) error { delivered += len(r); return nil })
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want an error that unwraps to context.DeadlineExceeded", err)
	}
	for _, want := range []string{`status "running"`, "total 3", "settled 1", "pending 2", "1 row(s) delivered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	if delivered != 1 {
		t.Errorf("fn received %d row(s), want the one that had settled", delivered)
	}

	l2 := newListing(3, 0)
	l2.onJob = func(l *listing, call int) {
		if call >= 2 {
			l.settleAll()
		}
	}
	g := newFakeM1(t, l2.handle)
	if _, err := g.client(t).Drain(testCtx(t), "J1", func([]Row) error { return nil }); err != nil {
		t.Fatalf("control: a job that completes was not drained: %v", err)
	}
}

// TestDrain_NeedsADeadline: a context with no deadline is refused before any request (a poll loop with no end), and the control with a deadline drains.
func TestDrain_NeedsADeadline(t *testing.T) {
	l := newListing(2, 0, 1)
	f := newFakeM1(t, l.handle)
	c := f.client(t)
	_, err := c.Drain(context.Background(), "J1", func([]Row) error { return nil })
	if !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("got %v, want ErrNoDeadline", err)
	}
	if f.count() != 0 {
		t.Fatalf("a refused Drain made %d request(s)", f.count())
	}
	if _, err := c.Drain(context.Background(), "J1", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a nil callback: got %v, want ErrInvalid", err)
	}
	if st, err := c.Drain(testCtx(t), "J1", func([]Row) error { return nil }); err != nil || st.Rows != 2 {
		t.Fatalf("control: with a deadline %+v, %v", st, err)
	}
}

// TestDrain_ACursorThatDoesNotAdvanceIsRefused: a listing that names, as its next position, the position it was asked from would loop for ever; Drain says so
// after two requests. Control: a listing that advances drains.
func TestDrain_ACursorThatDoesNotAdvanceIsRefused(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) { return 200, page("J1", "c0", row(0)) })
	c := f.client(t)
	_, err := c.Drain(testCtx(t), "J1", func([]Row) error { return nil })
	if !errors.Is(err, ErrCursorStuck) {
		t.Fatalf("got %v, want ErrCursorStuck", err)
	}
	if f.count() != 2 {
		t.Fatalf("%d request(s) before the refusal, want 2", f.count())
	}
	l := newListing(3, 0, 1, 2)
	g := newFakeM1(t, l.handle)
	gc := g.client(t)
	gc.PageLimit = 1
	if st, err := gc.Drain(testCtx(t), "J1", func([]Row) error { return nil }); err != nil || st.Pages != 3 {
		t.Fatalf("control: %+v, %v", st, err)
	}
}

// TestDrain_AFailingCallbackStopsTheDrain: an error from fn (a settlement that failed) is returned unchanged and no further page is requested: a page that
// was not settled is never followed by one that was. Control: a callback that never fails reads every page.
func TestDrain_AFailingCallbackStopsTheDrain(t *testing.T) {
	errSettle := errors.New("settlement failed")
	run := func(failAt int) (*listing, DrainStats, error) {
		l := newListing(6, 0, 1, 2, 3, 4, 5)
		f := newFakeM1(t, l.handle)
		c := f.client(t)
		c.PageLimit = 2
		n := 0
		st, err := c.Drain(testCtx(t), "J1", func([]Row) error {
			n++
			if n == failAt {
				return errSettle
			}
			return nil
		})
		return l, st, err
	}
	l, st, err := run(2)
	if err != errSettle {
		t.Fatalf("got %v, want the callback's own error, unchanged", err)
	}
	if st.Pages != 2 || l.results != 2 {
		t.Fatalf("stats %+v after %d result request(s): the drain went on past a failed page", st, l.results)
	}
	l, st, err = run(0)
	if err != nil || st.Pages != 3 || l.results != 3 {
		t.Fatalf("control: %+v after %d result request(s), %v", st, l.results, err)
	}
}

// ---- errors, limits, transport ----

// TestResults_AForeignCursorIsA400: a cursor module 1 did not issue is its 400 validation_failed; the client reports it as a StatusError naming the route and
// the status, and the error text does not repeat the cursor. Control: the cursor the stand-in did issue works.
func TestResults_AForeignCursorIsA400(t *testing.T) {
	l := newListing(3, 0, 1, 2)
	f := newFakeM1(t, l.handle)
	c := f.client(t)
	_, _, err := c.Results(testCtx(t), "J1", "garbage-"+testAddr, 2)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 400 || se.Want != 200 || se.Code != "validation_failed" || se.Route != "GET /v1/jobs/{id}/results" {
		t.Fatalf("got %v, want a StatusError 400 validation_failed on GET /v1/jobs/{id}/results", err)
	}
	if leaks(err.Error(), testAddr, "garbage") {
		t.Fatalf("the error repeats the cursor: %q", err)
	}
	if _, _, err := c.Results(testCtx(t), "J1", "c0", 2); err != nil {
		t.Fatalf("control: the cursor the module issued was refused: %v", err)
	}
}

// TestErrors_NameStatusAndRouteNeverTheKeyOrAnAddress: a hostile module 1 that puts the Authorization header and an address into every field of every
// answer. Whatever the status (5xx, 401, 404, 400, a redirect, a 200 with a garbage body), the error names the status and the route and carries neither the
// key nor the address; module 1's own error.code is kept only when it is a short lower-case word. Control: leaks() sees the key in a string that holds it, and
// every scenario's body really does hold both secrets.
func TestErrors_NameStatusAndRouteNeverTheKeyOrAnAddress(t *testing.T) {
	hostile := func(code string) string {
		b, _ := json.Marshal(map[string]any{
			"error":   map[string]any{"code": code, "message": "Bearer " + testKey + " " + testAddr, "details": []map[string]string{{"field": testAddr, "issue": testKey}}, "request_id": testKey},
			"job_id":  testAddr,
			"results": []map[string]string{{"email": testAddr, "item_id": testKey}},
		})
		return string(b)
	}
	type sc struct {
		name   string
		status int
		body   string
		route  string // the call to make: submit, job, results
		want   string // what the error text must contain
	}
	scenarios := []sc{
		{"submit 500", 500, hostile("internal_error"), "submit", "HTTP 500"},
		{"job 503", 503, hostile("unavailable"), "job", "HTTP 503"},
		{"results 500 with the key as its code", 500, hostile(testKey), "results", "HTTP 500"},
		{"results 401", 401, hostile("unauthenticated"), "results", "HTTP 401"},
		{"job 404", 404, hostile("not_found"), "job", "HTTP 404"},
		{"submit 400", 400, hostile("validation_failed"), "submit", "HTTP 400"},
		{"submit 413", 413, hostile("payload_too_large"), "submit", "HTTP 413"},
		{"results 302", 302, hostile("x"), "results", "HTTP 302"},
		{"job 200 garbage", 200, "Bearer " + testKey + " " + testAddr, "job", "documented"},
		{"results 200 garbage", 200, "{" + testKey + " " + testAddr, "results", "documented"},
	}
	routes := map[string]string{"submit": "POST /v1/verify/bulk", "job": "GET /v1/jobs/{id}", "results": "GET /v1/jobs/{id}/results"}
	for _, s := range scenarios {
		if !leaks(s.body, testKey) || !leaks(s.body, testAddr) {
			t.Fatalf("%s: the hostile body does not hold both secrets, so the scenario proves nothing", s.name)
		}
		f := newFakeM1(t, func(call) (int, string) { return s.status, s.body })
		c := f.client(t)
		var err error
		switch s.route {
		case "submit":
			_, _, err = c.Submit(testCtx(t), []Item{{"row-0", testAddr}})
		case "job":
			_, err = c.Job(testCtx(t), "J1")
		case "results":
			_, _, err = c.Results(testCtx(t), "J1", "", 0)
		}
		if err == nil {
			t.Errorf("%s: no error", s.name)
			continue
		}
		text := err.Error()
		if leaks(text, testKey, testAddr, "leak-check") {
			t.Errorf("%s: the error carries the key or an address: %q", s.name, text)
		}
		if !strings.Contains(text, routes[s.route]) || !strings.Contains(text, s.want) {
			t.Errorf("%s: the error %q does not name the route %q and %q", s.name, text, routes[s.route], s.want)
		}
	}
	var se *StatusError
	f := newFakeM1(t, func(call) (int, string) { return 500, hostile("internal_error") })
	if _, err := f.client(t).Job(testCtx(t), "J1"); !errors.As(err, &se) || se.Code != "internal_error" {
		t.Fatalf("module 1's own code was not kept: %v", err)
	}
	if !leaks("Bearer "+testKey, testKey) || leaks("fine", testKey) {
		t.Fatal("control: leaks() does not behave: the checks above prove nothing")
	}
}

// TestResponses_AreSizeLimited: a body of exactly MaxBodyBytes is read, one byte more is ErrBodyTooLarge, and an oversized ERROR answer is still reported by
// its status (with no code taken from a body that was not read in full). Control: the at-limit body decodes to its (empty) page.
func TestResponses_AreSizeLimited(t *testing.T) {
	const head = `{"job_id":"J1","results":[],"next_cursor":"","pad":"`
	const tail = `"}`
	padTo := func(size int) string { return head + strings.Repeat("a", size-len(head)-len(tail)) + tail }
	f := newFakeM1(t, func(c call) (int, string) {
		switch c.params().Get("cursor") {
		case "big":
			return 200, padTo(MaxBodyBytes + 1)
		case "err":
			return 500, `{"error":{"code":"internal_error"},"pad":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
		}
		return 200, padTo(MaxBodyBytes)
	})
	c := f.client(t)
	if rows, next, err := c.Results(testCtx(t), "J1", "", 0); err != nil || len(rows) != 0 || next != "" {
		t.Fatalf("control: a body of exactly MaxBodyBytes: %v %q %v", rows, next, err)
	}
	if _, _, err := c.Results(testCtx(t), "J1", "big", 0); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("a body of MaxBodyBytes+1: got %v, want ErrBodyTooLarge", err)
	}
	var se *StatusError
	if _, _, err := c.Results(testCtx(t), "J1", "err", 0); !errors.As(err, &se) || se.Status != 500 || se.Code != "" {
		t.Fatalf("an oversized 500: got %v, want a StatusError 500 with no code", err)
	}
}

// TestTransportFailures_AreClassifiedNotQuoted: a connection refused and a timeout are TransportErrors whose reason is a class written here; the text names
// the route and never the host or port. The timeout unwraps to context.DeadlineExceeded. Control: leaks() sees the host in the raw error of the same failure.
func TestTransportFailures_AreClassifiedNotQuoted(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	hostport := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	c, err := New(dead.URL, config.Secret(testKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Job(testCtx(t), "J1")
	var te *TransportError
	if !errors.As(err, &te) || te.Reason != "connection refused" || te.Route != "GET /v1/jobs/{id}" {
		t.Fatalf("a closed port: got %v, want a TransportError (connection refused) on GET /v1/jobs/{id}", err)
	}
	if leaks(err.Error(), hostport, "127.0.0.1", testKey) {
		t.Fatalf("the error quotes the address it dialled: %q", err)
	}
	// control: the raw error of the same failure does carry it
	raw, rerr := http.Get(dead.URL + "/x")
	if rerr == nil {
		_ = raw.Body.Close()
		t.Fatal("control: the closed server answered")
	}
	if !leaks(rerr.Error(), hostport) {
		t.Fatalf("control: the standard library's own error does not hold the host, so the check above proves nothing: %q", rerr)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	sc, err := New(slow.URL, config.Secret(testKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sc.Job(ctxFor(t, 100*time.Millisecond), "J1")
	if !errors.As(err, &te) || te.Reason != "timeout" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a hanging module: got %v, want a TransportError (timeout) that unwraps to DeadlineExceeded", err)
	}
	if leaks(err.Error(), strings.TrimPrefix(slow.URL, "http://"), testKey) {
		t.Fatalf("the timeout error quotes the address it dialled: %q", err)
	}
}

// TestRedirects_AreNeverFollowed: a 302 is a StatusError and the place it points at is never contacted, even when the caller supplies an http.Client that
// follows redirects (a redirect would re-send the key). Control: that very client, used directly, does follow it, so the target is reachable and the
// assertion could have failed.
func TestRedirects_AreNeverFollowed(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirector.Close()

	following := &http.Client{} // the default policy: follows up to ten redirects
	c, err := New(redirector.URL, config.Secret(testKey), following)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Job(testCtx(t), "J1")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 302 {
		t.Fatalf("got %v, want a StatusError 302", err)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 0 {
		t.Fatalf("the redirect target was contacted %d time(s): the key and the request were forwarded", got)
	}
	if following.CheckRedirect != nil {
		t.Fatal("New modified the caller's http.Client instead of copying it")
	}
	resp, err := following.Get(redirector.URL + "/x")
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	got = hits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("control: the client that follows redirects reached the target %d time(s), want 1", got)
	}
}

// TestNew_ValidatesItsInputs: a base URL that is not plain http(s) with a host, or that carries a credential, query or fragment, and a key that is empty or has
// whitespace, control or non-ASCII characters are refused, and the refusal quotes neither. Control: plain URLs and a hex key build, a trailing slash is
// trimmed.
func TestNew_ValidatesItsInputs(t *testing.T) {
	for _, u := range []string{"", "ftp://m1:8080", "http://", "http://user:pw-secret@m1:8080", "http://m1:8080?x=pw-secret", "http://m1:8080#pw-secret", "http://m1:8080?", "mailto:pw-secret@m1", "m1:8080"} {
		_, err := New(u, config.Secret(testKey), nil)
		if err == nil {
			t.Errorf("base URL %q was accepted", u)
			continue
		}
		if leaks(err.Error(), "pw-secret", testKey) {
			t.Errorf("base URL %q: the error quotes it: %q", u, err)
		}
	}
	for _, k := range []string{"", "pwx-has space", "pwx-tab\there", "pwx-new\nline", "pwx-café", " pwx-lead", "pwx-trail\r"} {
		_, err := New("http://m1:8080", config.Secret(k), nil)
		if err == nil {
			t.Errorf("key %q was accepted", k)
			continue
		}
		if leaks(err.Error(), "pwx-") {
			t.Errorf("key %q: the error quotes it: %q", k, err)
		}
	}
	if _, err := New("http://m1:8080", config.Secret("pwx-0123456789abcdef"), nil); err != nil {
		t.Errorf("control: a plain hex-ish key was refused: %v", err)
	}
	for _, u := range []string{"http://m1:8080", "http://m1:8080/", "https://m1.example/prefix/"} {
		c, err := New(u, config.Secret("0123456789abcdef0123456789abcdef"), nil)
		if err != nil {
			t.Errorf("control: %q was refused: %v", u, err)
			continue
		}
		if strings.HasSuffix(c.base, "/") {
			t.Errorf("control: the base %q was not trimmed of its trailing slash", c.base)
		}
	}
}

// TestKey_IsRedactedForEveryVerb: the Client prints without its key under every fmt verb, as a value and as a pointer, and encoding/json (the exported
// fields) does not carry it. Control: a struct holding the same key as a plain string does print it, so the check can see a leak.
func TestKey_IsRedactedForEveryVerb(t *testing.T) {
	c, err := New("http://m1:8080", config.Secret(testKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{c, *c} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, testKey) || strings.Contains(out, fmt.Sprintf("%x", testKey)) {
				t.Errorf("%s of a %T prints the key: %q", verb, v, out)
			}
		}
		if out := fmt.Sprint(v); !strings.Contains(out, "[redacted]") {
			t.Errorf("a %T renders as %q, want it to say the key is redacted", v, out)
		}
	}
	if b, err := json.Marshal(c); err != nil || leaks(string(b), testKey) {
		t.Errorf("json.Marshal of the Client: %s, %v", b, err)
	}
	if out := fmt.Sprintf("%v", struct{ Key string }{testKey}); !leaks(out, testKey) {
		t.Fatal("control: a plain string field does not print the key, so the checks above prove nothing")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEveryCallHasADeadline: a call whose context has no deadline gets DefaultCallTimeout; a call whose context has one keeps it (the earlier of the two
// wins). Checked for all three calls on the request's own context. Control: the contexts the test hands in really differ in having a deadline.
func TestEveryCallHasADeadline(t *testing.T) {
	var mu sync.Mutex
	var deadlines []time.Time
	var missing int
	body := func(r *http.Request) string {
		switch {
		case strings.HasSuffix(r.URL.Path, "/results"):
			return page("J1", "")
		case r.Method == http.MethodPost:
			return docSubmit202
		}
		return `{"job_id":"J1","status":"running","complete":false,"counts":{"total":1,"settled":0,"pending":1}}`
	}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dl, ok := r.Context().Deadline()
		mu.Lock()
		if ok {
			deadlines = append(deadlines, dl)
		} else {
			missing++
		}
		mu.Unlock()
		status := 200
		if r.Method == http.MethodPost {
			status = 202
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body(r))), Request: r}, nil
	})}
	c, err := New("http://m1.invalid:8080", config.Secret(testKey), hc)
	if err != nil {
		t.Fatal(err)
	}
	calls := []func(ctx context.Context) error{
		func(ctx context.Context) error {
			_, _, err := c.Submit(ctx, []Item{{"row-0", "a@b.example"}})
			return err
		},
		func(ctx context.Context) error { _, err := c.Job(ctx, "J1"); return err },
		func(ctx context.Context) error { _, _, err := c.Results(ctx, "J1", "", 0); return err },
	}
	if _, ok := context.Background().Deadline(); ok {
		t.Fatal("control: context.Background() has a deadline")
	}
	start := time.Now()
	for i, fn := range calls {
		if err := fn(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	mu.Lock()
	if missing != 0 || len(deadlines) != 3 {
		t.Fatalf("with no deadline in the context: %d request(s) had none, %d had one, want 0 and 3", missing, len(deadlines))
	}
	for i, dl := range deadlines {
		if d := dl.Sub(start); d < DefaultCallTimeout-2*time.Second || d > DefaultCallTimeout+2*time.Second {
			t.Errorf("call %d: the default deadline is %s ahead, want about %s", i, d, DefaultCallTimeout)
		}
	}
	deadlines = nil
	mu.Unlock()

	short := ctxFor(t, 5*time.Second)
	start = time.Now()
	for i, fn := range calls {
		if err := fn(short); err != nil {
			t.Fatalf("call %d with a deadline: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, dl := range deadlines {
		if d := dl.Sub(start); d > 6*time.Second {
			t.Errorf("call %d: the caller's 5 s deadline became %s ahead: it was replaced by the default", i, d)
		}
	}
	if len(deadlines) != 3 {
		t.Fatalf("%d request(s) with a deadline, want 3", len(deadlines))
	}
}

// TestBadShapes_AreRefused: a 2xx answer that is not the documented shape is ErrBadResponse (never a half-read result), for each route: no job_id, no
// complete flag, no counts, no results array, a page about another job, an element with no ordinal, text after the JSON value, a body that is not JSON; and a
// 200 where the route answers 202 is a StatusError. Control: the documented bodies of the same routes are accepted.
func TestBadShapes_AreRefused(t *testing.T) {
	ctx := testCtx(t)
	submit := func(status int, body string) error {
		f := newFakeM1(t, func(call) (int, string) { return status, body })
		_, _, err := f.client(t).Submit(ctx, []Item{{"row-0", "a@b.example"}})
		return err
	}
	job := func(body string) error {
		f := newFakeM1(t, func(call) (int, string) { return 200, body })
		_, err := f.client(t).Job(ctx, "019a…")
		return err
	}
	results := func(body string) error {
		f := newFakeM1(t, func(call) (int, string) { return 200, body })
		_, _, err := f.client(t).Results(ctx, "J1", "", 0)
		return err
	}
	for name, err := range map[string]error{
		"submit: control (the documented 202)": submit(202, docSubmit202),
		"job: control (the documented body)":   job(docStatus),
		"results: control (a page)":            results(page("J1", "", row(0))),
	} {
		if err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	bad := map[string]error{
		"submit: no job_id":                    submit(202, `{"status":"accepted","counts":{"total":1,"enqueued":1,"duplicate":0,"malformed":0}}`),
		"submit: no counts":                    submit(202, `{"job_id":"j","status":"accepted"}`),
		"submit: not JSON":                     submit(202, `accepted`),
		"job: no complete flag":                job(`{"job_id":"019a…","status":"running","counts":{"total":1,"settled":0,"pending":1}}`),
		"job: no counts":                       job(`{"job_id":"019a…","status":"running","complete":false}`),
		"job: a counts object without pending": job(`{"job_id":"019a…","status":"running","complete":false,"counts":{"total":1,"settled":0}}`),
		"job: another job":                     job(`{"job_id":"someone-else","status":"complete","complete":true,"counts":{"total":1,"settled":1,"pending":0}}`),
		"results: no results key":              results(`{"job_id":"J1","next_cursor":""}`),
		"results: results is null":             results(`{"job_id":"J1","results":null,"next_cursor":""}`),
		"results: another job":                 results(page("someone-else", "", row(0))),
		"results: an element with no ordinal":  results(`{"job_id":"J1","results":[{"status":"valid","reason":"accepted","item_id":"a"}],"next_cursor":""}`),
		"results: text after the value":        results(page("J1", "") + ` {"again":1}`),
		"results: not JSON":                    results(`<html>`),
	}
	for name, err := range bad {
		if !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s: got %v, want ErrBadResponse", name, err)
		}
	}
	var se *StatusError
	if err := submit(200, docSubmit202); !errors.As(err, &se) || se.Status != 200 || se.Want != 202 {
		t.Errorf("a 200 where the route answers 202: got %v, want a StatusError 200 (want 202)", err)
	}
	if err := results(`{"job_id":"J1","results":[{"status":"valid","reason":"accepted","item_id":"a"}],"next_cursor":""}`); err == nil || !strings.Contains(err.Error(), "no ordinal") {
		t.Errorf("an element with no ordinal: the error %v does not say why", err)
	}
}
