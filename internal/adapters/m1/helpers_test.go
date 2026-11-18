package m1

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// These tests are unit tests: they decode the fixtures of testdata/contract (seven RECORDED from the real module in run 44211659227 and normalised by contractcheck.Normalise, the rest
// hand-written from the module's code: see testdata/README.md) served by in-process stand-ins. Module 1 itself is never mocked in the harness, where the live tests drive this adapter against the real module.

const (
	// testJob is the job id of every recorded fixture: contractcheck.Normalise rewrites each uuid of a recording to 00000000-0000-4000-8000-00000000000N by order of first
	// appearance, and a job answer's job_id is the first uuid of its body, so it is N = 1 in every recorded job fixture. The hand-written fixtures use the same id, so one
	// constant serves them all (the adapter refuses an answer about another job).
	testJob = "00000000-0000-4000-8000-000000000001"
	// testAddr stands for an address that must never appear in an error string.
	testAddr = "jane.private@leak-check.example"
)

// newToken makes a caller key at run time; none is committed and none is ever printed.
func newToken(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "k-" + hex.EncodeToString(b[:])
}

func ctxFor(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func testCtx(t testing.TB) context.Context { return ctxFor(t, 30*time.Second) }

// noWait makes retries immediate and observable-free: the dials stay in force, only the sleeping is removed.
func noWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// fixture reads one recorded body of testdata/contract.
func fixture(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "contract", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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
	key    string
	mu     sync.Mutex
	calls  []call
	handle func(c call) (status int, body string)
}

func newFakeM1(t testing.TB, handle func(c call) (int, string)) *fakeM1 {
	t.Helper()
	f := &fakeM1{handle: handle, key: newToken(t)}
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

// adapter is an Adapter for the stand-in with no retries and a fast poll.
func (f *fakeM1) adapter(t testing.TB) *Adapter {
	return f.adapterWith(t, modclient.Options{Sleep: noWait})
}

func (f *fakeM1) adapterWith(t testing.TB, opts modclient.Options) *Adapter {
	t.Helper()
	a, err := New(f.srv.URL, config.Secret(f.key), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	a.Poll = time.Millisecond
	return a
}

func (f *fakeM1) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeM1) count() int { return len(f.seen()) }

// serve answers every request with one status and body.
func serve(t testing.TB, status int, body string) *fakeM1 {
	return newFakeM1(t, func(call) (int, string) { return status, body })
}

// page builds the body of GET /v1/jobs/{id}/results (the envelope api.go's handleJobResults writes).
func page(jobID, next string, elems ...string) string {
	return `{"job_id":` + strconv.Quote(jobID) + `,"results":[` + strings.Join(elems, ",") + `],"next_cursor":` + strconv.Quote(next) + `}`
}

// element builds one result element for a test that needs a row the module's doc does not show. The keys are the verdict object's own (status, reason, flags.from_cache,
// signals.*) and the three identity keys; extra is raw JSON appended inside the object ("" for none).
func element(status, reason, itemID string, ordinal int, extra string) string {
	var b strings.Builder
	b.WriteString(`{"api_version":"v1","status":` + strconv.Quote(status) + `,"reason":` + strconv.Quote(reason))
	b.WriteString(`,"confidence":10,"recommendation":"review","email":"","domain":"","flags":{"from_cache":false},"signals":{},"raw":null`)
	if itemID != "" {
		b.WriteString(`,"item_id":` + strconv.Quote(itemID))
	}
	b.WriteString(`,"ordinal":` + strconv.Itoa(ordinal))
	if extra != "" {
		b.WriteString("," + extra)
	}
	b.WriteString("}")
	return b.String()
}

func row(ordinal int) string {
	return element("valid", "accepted", "row-"+strconv.Itoa(ordinal), ordinal, "")
}

// statusBody is a status answer of the shape handleJobStatus writes.
func statusBody(jobID, status string, complete bool, total, settled int) string {
	return `{"job_id":` + strconv.Quote(jobID) + `,"status":` + strconv.Quote(status) + `,"complete":` + strconv.FormatBool(complete) +
		`,"caller_ref":"","counts":{"total":` + strconv.Itoa(total) + `,"settled":` + strconv.Itoa(settled) + `,"pending":` + strconv.Itoa(total-settled) + `},"processing_paused":false}`
}

func completeBody(total int) string { return statusBody(testJob, "complete", true, total, total) }

func ptr(n int) *int { return &n }

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// kindAndKey is what a test asserts about a modclient error: its Kind and the response key it names.
func kindAndKey(err error) (modclient.Kind, string) {
	e := asModclient(err)
	if e == nil {
		return 0, ""
	}
	return e.Kind, e.Key
}

// ---- values derived from the recorded fixtures ------------------------------------------------------------------------------------------------------------
//
// A recorded fixture holds real values (a job of 220 rows, counts of 3/1/0/2) that the hand-written ones did not. Tests that serve one never repeat those values as literals: they
// parse the fixture with encoding/json into generic maps (a decoder independent of the adapter's own) and compare what the adapter decoded with what that parse says.

// recordedDoc is a fixture of testdata/contract as a generic JSON document.
func recordedDoc(t testing.TB, name string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(fixture(t, name)))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return doc
}

// num reads an integer out of a generic document.
func num(t testing.TB, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		t.Fatalf("%v is not an integer", v)
	}
	return int(f)
}

func obj(t testing.TB, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return m
}

// recordedCounts is the counts object of a recorded 202.
func recordedCounts(t testing.TB, name string) (jobID string, c SubmitCounts) {
	t.Helper()
	doc := recordedDoc(t, name)
	cm := obj(t, doc["counts"])
	return doc["job_id"].(string), SubmitCounts{Total: num(t, cm["total"]), Enqueued: num(t, cm["enqueued"]), Duplicate: num(t, cm["duplicate"]), Malformed: num(t, cm["malformed"])}
}

// recordedStatus is what a recorded job answer says, read without the adapter.
func recordedStatus(t testing.TB, name string) JobStatus {
	t.Helper()
	doc := recordedDoc(t, name)
	cm := obj(t, doc["counts"])
	return JobStatus{
		JobID: doc["job_id"].(string), Status: doc["status"].(string), Complete: doc["complete"].(bool), ProcessingPaused: doc["processing_paused"].(bool),
		Counts: JobCounts{Total: num(t, cm["total"]), Settled: num(t, cm["settled"]), Pending: num(t, cm["pending"])},
	}
}

// acceptedFor is the recorded 202 with its counts replaced by c and its malformed_rows list emptied: the recording's shape, keys and job id, with the numbers a given plan predicts.
// (The recording itself is that of a raw submission that held two malformed rows, which a plan never sends.)
func acceptedFor(t testing.TB, c SubmitCounts) string {
	t.Helper()
	doc := recordedDoc(t, "submit-accepted.json")
	doc["counts"] = map[string]any{"total": c.Total, "enqueued": c.Enqueued, "duplicate": c.Duplicate, "malformed": c.Malformed}
	doc["malformed_rows"] = []any{}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recordedRows is the results of a recorded page as generic objects, in order.
func recordedRows(t testing.TB, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range recordedDoc(t, name)["results"].([]any) {
		out = append(out, obj(t, r))
	}
	return out
}

// withoutResults is a recorded page with its results emptied: the same job id and next_cursor, an empty array (the shape of a page with nothing on it).
func withoutResults(t testing.TB, name string) string {
	t.Helper()
	doc := recordedDoc(t, name)
	doc["results"] = []any{}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
