package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/csvsafe"
	"github.com/coldmail-platform/platform/internal/jobs"
)

func manyResults(n int) []jobs.Result {
	out := make([]jobs.Result, n)
	for i := range out {
		out[i] = jobs.Result{Ordinal: i + 1, Email: fmt.Sprintf("u%05d@example.test", i+1), Origin: "module", State: "valid", Reason: "", Decision: "charge", Unit: "m1_syntax", Rule: "P-03", RecordedAt: t0}
	}
	return out
}

// ---- the JSON results ----------------------------------------------------------------------------------------------

func TestResultsJSON_ACursorWalkNeitherRepeatsNorSkipsAcrossPageBoundaries(t *testing.T) {
	for _, n := range []int{25, 20, 10, 11, 1, 0} {
		r := newRig(t, withPageMax(10)) // the dial: no page is longer than 10
		r.store.results = manyResults(n)
		var seen []int
		cursor, pages := "", 0
		for {
			path := "/v1/jobs/" + jobIDA + "/results?limit=10"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			resp, body := r.get(loginA, path)
			m := r.wantStatus(resp, body, 200)
			page := m["results"].([]any)
			if len(page) > 10 {
				t.Fatalf("n=%d: a page of %d results with the dial at 10", n, len(page))
			}
			for _, it := range page {
				seen = append(seen, int(it.(map[string]any)["ordinal"].(float64)))
			}
			pages++
			next, _ := m["next_cursor"].(string)
			if next == "" {
				break
			}
			if len(page) != 10 {
				t.Fatalf("n=%d: a short page of %d came with a next cursor", n, len(page))
			}
			cursor = next
			if pages > 10 {
				t.Fatalf("n=%d: the walk does not end", n)
			}
		}
		if len(seen) != n {
			t.Fatalf("n=%d: the walk saw %d results: %v", n, len(seen), seen)
		}
		for i, o := range seen {
			if o != i+1 {
				t.Fatalf("n=%d: result %d of the walk has ordinal %d (a repeat or a skip): %v", n, i+1, o, seen)
			}
		}
		// the last page never points at an empty one: exactly 20 results with a page of 10 is two pages
		if want := (n + 9) / 10; n > 0 && pages != want {
			t.Errorf("n=%d: %d pages, want %d", n, pages, want)
		}
	}
}

func TestResultsJSON_TheCursorTheLimitAndTheDialAreChecked(t *testing.T) {
	r := newRig(t, withPageMax(10))
	r.store.results = manyResults(30)
	base := "/v1/jobs/" + jobIDA + "/results"
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	// a cursor nobody gave us, in every way of being one
	for _, bad := range []string{"abc", "!!!", b64("o0"), b64("o-1"), b64("o1x"), b64("x5"), b64("o05"), b64("o"), b64("o99999999999"), b64("o5") + "=", "o5"} {
		r.store.reset()
		resp, body := r.get(loginA, base+"?cursor="+bad)
		e := r.wantError(resp, body, 400, "validation_failed")
		if e.Details["reason"] != "the cursor is not one this API gave" {
			t.Errorf("cursor %q: %v", bad, e.Details)
		}
		if len(r.store.seen()) != 0 {
			t.Errorf("cursor %q reached the store", bad)
		}
	}
	// the control: a cursor the API gave is taken, and one past the end is an empty last page, not an error
	if resp, body := r.get(loginA, base+"?limit=3&cursor="+cursorOf(5)); resp.StatusCode != 200 || !strings.Contains(string(body), `"ordinal":6`) {
		t.Fatalf("a good cursor: %d %s", resp.StatusCode, body)
	}
	resp, body := r.get(loginA, base+"?cursor="+cursorOf(30))
	m := r.wantStatus(resp, body, 200)
	if len(m["results"].([]any)) != 0 || m["next_cursor"] != nil {
		t.Fatalf("past the end: %s", body)
	}

	// the limit and the dial: the dial is the largest page, an empty limit is the default, and the store is asked for ONE MORE than the page (to know whether another follows)
	for _, bad := range []string{"0", "-1", "11", "1000", "x"} {
		resp, body := r.get(loginA, base+"?limit="+bad)
		r.wantError(resp, body, 400, "validation_failed")
	}
	r.store.reset()
	r.get(loginA, base+"?limit=10")
	r.get(loginA, base)
	r.get(loginA, base+"?limit=")
	calls := r.store.seen()
	if len(calls) != 3 {
		t.Fatalf("calls %+v", calls)
	}
	for i, c := range calls {
		if got := c.Arg.([2]int); got != [2]int{0, 11} {
			t.Errorf("request %d asked the store for %v, want (after 0, limit 11)", i+1, got)
		}
	}

	// the dial's own bounds: the default dial is the CSV page (1000), and it cannot be set beyond what the store serves
	big := newRig(t)
	for p, want := range map[string]int{"?limit=1000": 200, "?limit=1001": 400} {
		resp, _ := big.get(loginA, base+p)
		if resp.StatusCode != want {
			t.Errorf("default dial, %s: %d, want %d", p, resp.StatusCode, want)
		}
	}
	big.store.reset()
	big.get(loginA, base)
	if c := big.store.seen(); len(c) != 1 || c[0].Arg.([2]int) != [2]int{0, defaultResultsPage + 1} {
		t.Fatalf("the default page of the default dial is %+v, want %d", c, defaultResultsPage)
	}
	for _, bad := range []int{-1, 1001, 5000} {
		if _, err := New(Deps{Service: big.store, Verifier: big.mint.Verifier(t), Resolver: big.res, ResultsPageMax: bad}); err == nil {
			t.Errorf("a results page maximum of %d was accepted", bad)
		}
	}

	// another user's job, or none: the store's 404
	r.store.fail["Results"] = jobs.ErrNotFound
	resp, body = r.get(loginA, base)
	r.wantError(resp, body, 404, "not_found")
}

func TestResultsJSON_TheShapeOfAResultAndNothingMoreThanTheCustomerMayKnow(t *testing.T) {
	r := newRig(t)
	r.store.results = []jobs.Result{
		{Ordinal: 1, Email: "a@example.test", Origin: "module", State: "valid", Reason: "", Decision: "charge", Unit: "m1_syntax", Rule: "P-03", RecordedAt: t0},
		{Ordinal: 2, Email: "no-at-sign", Origin: "platform", State: "malformed", Reason: "no_at", Decision: "free", Rule: "P-08", RecordedAt: t0},
	}
	resp, body := r.get(loginA, "/v1/jobs/"+jobIDA+"/results")
	m := r.wantStatus(resp, body, 200)
	first, second := m["results"].([]any)[0].(map[string]any), m["results"].([]any)[1].(map[string]any)
	if len(first) != 9 {
		t.Fatalf("a result has %d members: %v", len(first), first)
	}
	if first["reason"] != nil || first["unit"] != "m1_syntax" || first["recorded_at"] != "2001-09-09T01:46:40Z" || first["decision"] != "charge" {
		t.Errorf("first = %v", first)
	}
	if second["reason"] != "no_at" || second["unit"] != nil || second["origin"] != "platform" {
		t.Errorf("second = %v", second)
	}
	for _, banned := range []string{"user_id", "detail", "item_ref", "job_id"} {
		if strings.Contains(string(body), banned) {
			t.Errorf("a result shows %q: %s", banned, body)
		}
	}
}

// ---- results.csv --------------------------------------------------------------------------------------------------

// awkward is every kind of value a list can hold that a CSV writer must get right, and the formula triggers a spreadsheet must not run.
var awkward = []string{
	"plain@example.test", "with,comma@example.test", `with"quote@example.test`, "with\nnewline@example.test", "a\rb@example.test", "\"quoted\", and =x",
	"=cmd|' /C calc'!A0", `=HYPERLINK("http://example.test","click")`, "+1+1", "-2+3", "@SUM(1,1)", "\ttab", "\rcr", "'leading", "''two", "'=cmd", "=", "+", "-", "@", "'",
	" leading space", "trailing space ", "", "ünïcödé ☃ 日本語", "😀@example.test", ",=", "a,b\n=c",
}

// awkwardResults puts every awkward value in the three columns a customer's text can reach (the address, the reason, the unit) and keeps the rest plain.
func awkwardResults() []jobs.Result {
	var out []jobs.Result
	for i, v := range awkward {
		out = append(out, jobs.Result{Ordinal: i + 1, Email: v, Origin: "platform", State: "malformed", Reason: v, Decision: "free", Unit: v, Rule: "P-08", RecordedAt: t0})
	}
	return out
}

func (r *rig) csvOf(path string) (*http.Response, []byte) {
	r.t.Helper()
	return r.do(req{method: http.MethodGet, path: path, token: r.token(loginA)})
}

func TestResultsCSV_AwkwardValuesRoundTripExactlyAndNoCellOpensAsAFormula(t *testing.T) {
	r := newRig(t)
	r.store.results = awkwardResults()
	resp, body := r.csvOf("/v1/jobs/" + jobIDA + "/results.csv")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="job-`+jobIDA+`.csv"` {
		t.Errorf("Content-Disposition %q", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", resp.Header)
	}
	cr := csv.NewReader(bytes.NewReader(body))
	cr.FieldsPerRecord = 8
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("the file is not a CSV: %v", err)
	}
	if len(recs) != len(awkward)+1 {
		t.Fatalf("%d records, want the header and %d", len(recs), len(awkward))
	}
	if strings.Join(recs[0], ",") != "ordinal,email,origin,state,reason,decision,unit,rule" {
		t.Fatalf("header %v", recs[0])
	}
	triggers := 0
	for i, v := range awkward {
		row := recs[i+1]
		for col, name := range map[int]string{1: "email", 4: "reason", 6: "unit"} {
			if got := csvsafe.Unescape(row[col]); got != v {
				t.Errorf("row %d %s: read back %q, want %q", i+1, name, got, v)
			}
			if csvsafe.Triggers(row[col]) {
				t.Errorf("row %d %s: the cell %q opens as a formula", i+1, name, row[col])
			}
		}
		if row[0] != fmt.Sprint(i+1) || row[2] != "platform" || row[3] != "malformed" || row[5] != "free" || row[7] != "P-08" {
			t.Errorf("row %d: %v", i+1, row)
		}
		if csvsafe.Triggers(v) {
			triggers++
		}
	}

	// the control: the same values written WITHOUT csvsafe (what a plain encoding/csv stream would send) do open as formulas: the check above can fail
	var naive bytes.Buffer
	w := csv.NewWriter(&naive)
	for _, v := range awkward {
		_ = w.Write([]string{v})
	}
	w.Flush()
	nr, _ := csv.NewReader(&naive).ReadAll()
	exposed := 0
	for _, rec := range nr {
		if csvsafe.Triggers(rec[0]) {
			exposed++
		}
	}
	if triggers < 8 || exposed != triggers {
		t.Fatalf("the control is not a control: %d awkward values start with a trigger, %d of them survive a plain writer as triggers", triggers, exposed)
	}
}

func TestResultsCSV_ZeroResultsIsTheHeaderAlone(t *testing.T) {
	r := newRig(t)
	resp, body := r.csvOf("/v1/jobs/" + jobIDA + "/results.csv")
	if resp.StatusCode != 200 || string(body) != "ordinal,email,origin,state,reason,decision,unit,rule\n" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestResultsCSV_ABeforeTheFirstByteFailureIsTheEnvelopeAndOtherUsersJobsAreNotFound(t *testing.T) {
	r := newRig(t)
	r.store.failResultsAt, r.store.failResultsErr = 1, jobs.ErrNotFound
	resp, body := r.csvOf("/v1/jobs/" + otherJob + "/results.csv")
	r.wantError(resp, body, 404, "not_found")
	if resp.Header.Get("Content-Disposition") != "" || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("a refusal came with the headers of a file: %v", resp.Header)
	}
	r.store.reset()
	r.store.failResultsAt, r.store.failResultsErr = 1, errBoom
	resp, body = r.csvOf("/v1/jobs/" + jobIDA + "/results.csv")
	r.wantError(resp, body, 500, "internal_error")
	// the control: with nothing failing it is a file
	r.store.failResultsAt = 0
	if resp, _ := r.csvOf("/v1/jobs/" + jobIDA + "/results.csv"); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// noFlush hides the Flusher of the connection from the handler: what a response that is only flushed when its buffers fill looks like.
type noFlush struct{ http.ResponseWriter }

// lineGate counts the lines a client has read of a streamed body and says when it has read n of them.
type lineGate struct {
	mu    sync.Mutex
	n     int
	want  int
	ready chan struct{}
	once  sync.Once
}

func newLineGate(want int) *lineGate { return &lineGate{want: want, ready: make(chan struct{})} }

func (g *lineGate) line() {
	g.mu.Lock()
	g.n++
	hit := g.n >= g.want
	g.mu.Unlock()
	if hit {
		g.once.Do(func() { close(g.ready) })
	}
}

// streamed runs a 2500-result download against a store whose SECOND page waits for the client to have received the whole first page (the header and 1000 lines), and says whether the wait
// was satisfied. A server that holds the response back until it is finished (or that flushes only when its buffers fill) cannot satisfy it.
func streamed(t *testing.T, opts ...rigOpt) (starved bool, body []byte, r *rig) {
	t.Helper()
	r = newRig(t, opts...)
	r.store.results = manyResults(2500)
	gate := newLineGate(1 + jobs.ResultsPageSize)
	var mu sync.Mutex
	r.store.resultsHook = func(ctx context.Context, call int) {
		if call != 2 {
			return
		}
		select {
		case <-gate.ready:
		case <-time.After(2 * time.Second):
			mu.Lock()
			starved = true
			mu.Unlock()
		}
	}
	resp, err := r.doRaw(req{method: http.MethodGet, path: "/v1/jobs/" + jobIDA + "/results.csv", token: r.token(loginA)})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	sc := bufio.NewScanner(io.TeeReader(resp.Body, &buf))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		gate.line()
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return starved, buf.Bytes(), r
}

func TestResultsCSV_StreamsAPageAtATimeAndFlushesEachPage(t *testing.T) {
	starved, body, r := streamed(t)
	if starved {
		t.Fatal("the client had not received the first page when the second page was asked for: the response is not flushed page by page")
	}
	calls := r.store.pageCalls()
	if len(calls) != 3 || calls[0] != (resultsCall{0, 1000}) || calls[1] != (resultsCall{1000, 1000}) || calls[2] != (resultsCall{2000, 1000}) {
		t.Fatalf("the store was read in %d calls: %v, want three pages of 1000 from 0, 1000 and 2000", len(calls), calls)
	}
	recs, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil || len(recs) != 2501 {
		t.Fatalf("the file has %d records (%v), want the header and 2500", len(recs), err)
	}
	for i, rec := range recs[1:] {
		if rec[0] != fmt.Sprint(i+1) {
			t.Fatalf("record %d has ordinal %s", i+1, rec[0])
		}
	}

	// the control: with the connection's Flusher hidden from the handler the same download is starved (the tail of the first page waits in a buffer), so the check above can fail
	starvedHidden, hiddenBody, _ := streamed(t, withWrap(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { h.ServeHTTP(noFlush{w}, q) })
	}))
	if !starvedHidden {
		t.Fatal("with the Flusher hidden the second page was still asked for after the client had the whole first one: the proof of flushing proves nothing")
	}
	if recs, err := csv.NewReader(bytes.NewReader(hiddenBody)).ReadAll(); err != nil || len(recs) != 2501 {
		t.Fatalf("the starved download still has to be complete: %d records, %v", len(recs), err)
	}
}

func TestResultsCSV_AClientThatGoesAwayStopsTheStream(t *testing.T) {
	r := newRig(t)
	r.store.results = manyResults(5000)
	gate := newLineGate(1 + jobs.ResultsPageSize)
	sawCancel := make(chan struct{})
	r.store.resultsHook = func(ctx context.Context, call int) {
		if call != 2 {
			return
		}
		select {
		case <-ctx.Done(): // the request's context ends when the client's connection does
			close(sawCancel)
		case <-time.After(10 * time.Second):
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hr, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL+"/v1/jobs/"+jobIDA+"/results.csv", nil)
	hr.Header.Set("Authorization", "Bearer "+r.token(loginA))
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		gate.line()
		if gate.reached() {
			break
		}
	}
	cancel() // the client goes away with the first page in hand, while the server is waiting for the second
	resp.Body.Close()
	select {
	case <-sawCancel:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never noticed that the client had gone: the store's context did not end")
	}
	time.Sleep(200 * time.Millisecond) // a stream that went on would have asked for a third page by now
	if calls := r.store.pageCalls(); len(calls) != 2 {
		t.Fatalf("the store was read %d times (%v) after the client left, want it to stop at the page in flight", len(calls), calls)
	}
	var logged bool
	for _, l := range r.logs.lines(t) {
		if strings.HasPrefix(fmt.Sprint(l["msg"]), "results.csv stopped") {
			logged = true
		}
	}
	if !logged {
		t.Errorf("the log does not say the stream stopped because the client left: %s", r.logs.String())
	}
}

func TestResultsCSV_AFailureAfterTheFirstByteAbortsTheConnectionAndNeverLooksComplete(t *testing.T) {
	r := newRig(t)
	r.store.results = manyResults(2500)
	r.store.failResultsAt, r.store.failResultsErr = 2, fmt.Errorf("jobs.results: read bob@example.test: %w", errBoom)
	resp, err := r.doRaw(req{method: http.MethodGet, path: "/v1/jobs/" + jobIDA + "/results.csv", token: r.token(loginA)})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the status line had been sent as %d", resp.StatusCode)
	}
	if readErr == nil {
		t.Fatalf("the body ended cleanly after %d bytes although the stream failed: a truncated file looks complete", len(got))
	}
	if n := bytes.Count(got, []byte("\n")); n != 1+jobs.ResultsPageSize {
		t.Errorf("the client got %d lines before the abort, want the header and the first page (%d)", n, 1+jobs.ResultsPageSize)
	}
	var line map[string]any
	for _, l := range r.logs.lines(t) {
		if l["msg"] == "results.csv aborted after it had started" {
			line = l
		}
	}
	if line == nil || line["rows_sent"] != float64(jobs.ResultsPageSize) || line["level"] != "ERROR" || line["request_id"] == nil {
		t.Fatalf("the abort was not logged under the request id: %v", line)
	}
	if strings.Contains(r.logs.String(), "bob@") {
		t.Fatalf("an address is in the log")
	}

	// the control: the same download with no failure ends cleanly, with every record
	r.store.failResultsAt = 0
	resp, err = r.doRaw(req{method: http.MethodGet, path: "/v1/jobs/" + jobIDA + "/results.csv", token: r.token(loginA)})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || bytes.Count(got, []byte("\n")) != 2501 {
		t.Fatalf("without a failure: %v after %d lines", readErr, bytes.Count(got, []byte("\n")))
	}
}

func (g *lineGate) reached() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n >= g.want
}
