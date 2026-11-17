package m1

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// listing is a stand-in for module 1's listing: a job of n rows, each settled or not; it lists the settled rows in ordinal order, a cursor is the ordinal of the last row returned, a
// page carries a next cursor only when a settled row follows (module 1 looks ahead by one and takes the cursor from the last row RETURNED), and a cursor it did not issue is a 400.
// onJob runs after each status read (it models the module settling more rows while the caller waits).
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
				return 400, fixture0
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
		return 200, page(testJob, next, elems...)
	}
	l.jobs++
	done := len(l.settled)
	status := "running"
	if done == l.n {
		status = "complete"
	}
	body := statusBody(testJob, status, done == l.n, l.n, done)
	if l.onJob != nil {
		l.onJob(l, l.jobs)
	}
	return 200, body
}

// fixture0 is module 1's 400 for a cursor it did not issue (the recorded error envelope's shape, inline because the listing is a function of the request).
const fixture0 = `{"error":{"code":"validation_failed","message":"One or more fields are invalid.","details":[{"field":"cursor","issue":"malformed"}],"request_id":"00000000-0000-7000-8000-0000000000b8"}}`

func ordinalsOf(rows []Row) []int {
	var o []int
	for _, r := range rows {
		o = append(o, r.Ordinal)
	}
	return o
}

// TestDrain_WalksThreePagesAndAnEmptyLastPage: a complete job read in pages of two: the stand-in's third page is EMPTY with no next cursor (the end of a list that was exactly two
// pages long). fn is called once per non-empty page, in order, with the rows of that page and no others, never for the empty page; the cursors sent are the ones the pages
// returned, unchanged; every row arrives once. Control: the same job read with a page of 100 is ONE call of fn with all four rows, so the per-page behaviour comes from the page
// size and not from the stand-in.
func TestDrain_WalksThreePagesAndAnEmptyLastPage(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.Path, "/results"):
			switch c.params().Get("cursor") {
			case "":
				return 200, page(testJob, "c1", row(0), row(1))
			case "c1":
				return 200, page(testJob, "c3", row(2), row(3))
			case "c3":
				return 200, page(testJob, "") // the empty last page
			}
			return 400, fixture0
		default:
			return 200, completeBody(4)
		}
	})
	a := f.adapter(t)
	a.PageLimit = 2
	var pages [][]int
	st, err := a.Drain(testCtx(t), testJob, func(rows []Row) error {
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
			return 200, page(testJob, "", row(0), row(1), row(2), row(3))
		}
		return 200, completeBody(4)
	})
	var calls int
	if _, err := g.adapter(t).Drain(testCtx(t), testJob, func(rows []Row) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("control: a page of 100 gave %d call(s) of fn (%v), want 1", calls, err)
	}
}

// TestDrain_SettlesAsRowsArriveAndCatchesARowBehindTheCursor: a running job of six rows where row 1 settles AFTER the cursor has passed it (module 1 does not revisit a
// position). Drain hands fn the first page while the job is still running, waits, follows the cursor to the end, and once the job is complete delivers the missed row by one full
// walk: every ordinal exactly once, row 1 last. Control: a plain walker over the same stand-in, which is what a client that trusts next_cursor would be, ends with five rows and
// misses row 1: the hazard is real, so the catch-up is what delivered it.
func TestDrain_SettlesAsRowsArriveAndCatchesARowBehindTheCursor(t *testing.T) {
	script := func() *listing {
		l := newListing(6, 0, 2, 3)
		l.onJob = func(l *listing, _ int) { l.settleAll() } // after the first status read the module settles the rest, row 1 included
		return l
	}

	l := script()
	f := newFakeM1(t, l.handle)
	a := f.adapter(t)
	a.PageLimit = 2
	var pages [][]int
	var settledAtFirstPage int
	st, err := a.Drain(testCtx(t), testJob, func(rows []Row) error {
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
	ga := g.adapter(t)
	ctx := testCtx(t)
	got := map[int]bool{}
	cursor := ""
	for pass := 0; pass < 2; pass++ {
		for {
			rows, next, err := ga.Results(ctx, testJob, cursor, 2)
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
			if _, err := ga.Status(ctx, testJob); err != nil { // the job read that makes the stand-in settle the rest
				t.Fatal(err)
			}
		}
	}
	if got[1] || len(got) != 5 {
		t.Fatalf("control: a walker that trusts next_cursor read %v: it should have missed row 1 and read five rows, otherwise the stand-in does not reproduce the hazard and the catch-up proves nothing", got)
	}
}

// TestDrain_ACompleteJobWhoseRowsDoNotAddUpIsRefused: the job says complete with four rows but the listing, however it is walked, yields three (a pagination that skips a row,
// module 2's bug M2-G5). Drain returns a Contract error (cause ErrIncomplete) naming the numbers and delivers no more than it read; it never reports a job it did not read in
// full. Control: the same script with row 2 present drains cleanly.
func TestDrain_ACompleteJobWhoseRowsDoNotAddUpIsRefused(t *testing.T) {
	build := func(skip bool) *fakeM1 {
		return newFakeM1(t, func(c call) (int, string) {
			if !strings.HasSuffix(c.Path, "/results") {
				return 200, completeBody(4)
			}
			if c.params().Get("cursor") == "" {
				return 200, page(testJob, "c1", row(0), row(1))
			}
			if skip {
				return 200, page(testJob, "", row(3))
			}
			return 200, page(testJob, "", row(2), row(3))
		})
	}
	var rows int
	a := build(true).adapter(t)
	a.PageLimit = 2
	st, err := a.Drain(testCtx(t), testJob, func(r []Row) error { rows += len(r); return nil })
	if !errors.Is(err, ErrIncomplete) || modclient.KindOf(err) != modclient.Contract || !strings.Contains(err.Error(), "3 row(s) were read") || !strings.Contains(err.Error(), "total 4") {
		t.Fatalf("got %v (kind %v), want a Contract error with cause ErrIncomplete naming 3 rows read of a total of 4", err, modclient.KindOf(err))
	}
	if rows != 3 || st.Rows != 3 {
		t.Fatalf("%d row(s) reached fn (stats %+v), want the 3 that were read", rows, st)
	}
	rows = 0
	d := build(false).adapter(t)
	d.PageLimit = 2
	if _, err := d.Drain(testCtx(t), testJob, func(r []Row) error { rows += len(r); return nil }); err != nil || rows != 4 {
		t.Fatalf("control: the complete listing gave %d row(s) and %v", rows, err)
	}
}

// TestDrain_ATimeoutNamesWhatTheModuleLastSaid: a job that never completes (one row settled of three) is waited for until the context's deadline, and the error is a Timeout that
// unwraps to context.DeadlineExceeded and says what module 1 last reported and how many rows were delivered. Control: with a deadline long enough and the rows settling, the same
// stand-in drains.
func TestDrain_ATimeoutNamesWhatTheModuleLastSaid(t *testing.T) {
	l := newListing(3, 0)
	f := newFakeM1(t, l.handle)
	a := f.adapter(t)
	a.Poll = 5 * time.Millisecond
	var delivered int
	_, err := a.Drain(ctxFor(t, 300*time.Millisecond), testJob, func(r []Row) error { delivered += len(r); return nil })
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || modclient.KindOf(err) != modclient.Timeout {
		t.Fatalf("got %v (kind %v), want a Timeout error that unwraps to context.DeadlineExceeded", err, modclient.KindOf(err))
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
	if _, err := g.adapter(t).Drain(testCtx(t), testJob, func([]Row) error { return nil }); err != nil {
		t.Fatalf("control: a job that completes was not drained: %v", err)
	}
}

// TestDrain_NeedsADeadline: a context with no deadline is an Invalid error (cause ErrNoDeadline) before any request (a poll loop with no end), a nil callback is Invalid too, and
// the control with a deadline drains.
func TestDrain_NeedsADeadline(t *testing.T) {
	l := newListing(2, 0, 1)
	f := newFakeM1(t, l.handle)
	a := f.adapter(t)
	_, err := a.Drain(context.Background(), testJob, func([]Row) error { return nil })
	if !errors.Is(err, ErrNoDeadline) || modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("got %v, want an Invalid error with cause ErrNoDeadline", err)
	}
	if f.count() != 0 {
		t.Fatalf("a refused Drain made %d request(s)", f.count())
	}
	if _, err := a.Drain(testCtx(t), testJob, nil); modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("a nil callback: got %v, want an Invalid error", err)
	}
	if st, err := a.Drain(testCtx(t), testJob, func([]Row) error { return nil }); err != nil || st.Rows != 2 {
		t.Fatalf("control: with a deadline %+v, %v", st, err)
	}
}

// TestDrain_ACursorThatDoesNotAdvanceIsRefused: a listing that names, as its next position, the position it was asked from would loop for ever; Drain says so after two requests,
// with a Contract error (cause ErrCursorStuck). Control: a listing that advances drains.
func TestDrain_ACursorThatDoesNotAdvanceIsRefused(t *testing.T) {
	f := newFakeM1(t, func(c call) (int, string) { return 200, page(testJob, "c0", row(0)) })
	a := f.adapter(t)
	_, err := a.Drain(testCtx(t), testJob, func([]Row) error { return nil })
	if !errors.Is(err, ErrCursorStuck) || modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("got %v, want a Contract error with cause ErrCursorStuck", err)
	}
	if f.count() != 2 {
		t.Fatalf("%d request(s) before the refusal, want 2", f.count())
	}
	l := newListing(3, 0, 1, 2)
	g := newFakeM1(t, l.handle)
	ga := g.adapter(t)
	ga.PageLimit = 1
	if st, err := ga.Drain(testCtx(t), testJob, func([]Row) error { return nil }); err != nil || st.Pages != 3 {
		t.Fatalf("control: %+v, %v", st, err)
	}
}

// TestDrain_AFailingCallbackStopsTheDrain: an error from fn (a settlement that failed) is returned unchanged and no further page is requested: a page that was not settled is
// never followed by one that was. Control: a callback that never fails reads every page.
func TestDrain_AFailingCallbackStopsTheDrain(t *testing.T) {
	errSettle := errors.New("settlement failed")
	run := func(failAt int) (*listing, DrainStats, error) {
		l := newListing(6, 0, 1, 2, 3, 4, 5)
		f := newFakeM1(t, l.handle)
		a := f.adapter(t)
		a.PageLimit = 2
		n := 0
		st, err := a.Drain(testCtx(t), testJob, func([]Row) error {
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

// TestDrain_AFailedReadEndsTheWholeDrain: whichever read fails (a page in the middle, the status after the last page), the drain returns that typed error, rows already delivered
// stay delivered and are not claimed to be the rest, and nothing is read after the failure. Control: the same job with the reads succeeding is drained in full, so the failure is
// what stopped it.
func TestDrain_AFailedReadEndsTheWholeDrain(t *testing.T) {
	cases := []struct {
		name string
		fail func(c call) bool
		kind modclient.Kind
		rows int
	}{
		{"the second page is a 500", func(c call) bool { return c.params().Get("cursor") == "c1" }, modclient.Internal, 2},
		{"the status read is a 500", func(c call) bool { return !strings.HasSuffix(c.Path, "/results") }, modclient.Internal, 4},
	}
	build := func(fail func(call) bool) *fakeM1 {
		return newFakeM1(t, func(c call) (int, string) {
			if fail != nil && fail(c) {
				return 500, fixtureErr("internal_error")
			}
			switch {
			case !strings.HasSuffix(c.Path, "/results"):
				return 200, completeBody(4)
			case c.params().Get("cursor") == "":
				return 200, page(testJob, "c1", row(0), row(1))
			case c.params().Get("cursor") == "c1":
				return 200, page(testJob, "", row(2), row(3))
			}
			return 400, fixture0
		})
	}
	for _, tc := range cases {
		f := build(tc.fail)
		a := f.adapter(t)
		a.PageLimit = 2
		var delivered int
		st, err := a.Drain(testCtx(t), testJob, func(r []Row) error { delivered += len(r); return nil })
		if modclient.KindOf(err) != tc.kind || delivered != tc.rows || st.Rows != tc.rows {
			t.Errorf("%s: kind %v, %d row(s) delivered (stats %+v), want %v and %d", tc.name, modclient.KindOf(err), delivered, st, tc.kind, tc.rows)
		}
		if errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: a failed read was reported as an incomplete listing", tc.name)
		}
	}
	var delivered int
	a := build(nil).adapter(t)
	a.PageLimit = 2
	if _, err := a.Drain(testCtx(t), testJob, func(r []Row) error { delivered += len(r); return nil }); err != nil || delivered != 4 {
		t.Fatalf("control: with the reads succeeding the drain gave %d row(s), %v", delivered, err)
	}
}

func fixtureErr(code string) string {
	return `{"error":{"code":"` + code + `","message":"x","request_id":"00000000-0000-7000-8000-0000000000b9"}}`
}
