package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/auth/authtest"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/logging"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The rig every test of this package runs on: the REAL httpapi handler (request id, access log, recoverer) with the API mounted through Deps.Mount, over a real listener, a REAL
// auth.Verifier fed tokens by authtest (keys made at run time), a resolver that answers from a table, and a fake of the job store that records every call it gets and the user it was given.

const (
	userA     = "a1a1a1a1-0000-4000-8000-000000000001" // the platform user of the default login
	loginA    = "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90" // the login (auth.users.id) of userA: deliberately NOT the same uuid, so a handler that used the login id for the store is caught
	userB     = "b2b2b2b2-0000-4000-8000-000000000002"
	loginB    = "7d4c2a90-3e51-4b8f-a6d2-91c0e5f3b7a4"
	jobIDA    = "0b000000-0000-4000-8000-000000000001"
	otherJob  = "0b000000-0000-4000-8000-0000000000ff"
	missingID = "0b000000-0000-4000-8000-0000000000aa"
)

var t0 = time.Unix(1_000_000_000, 0).UTC()

// resultsCall is the (after, limit) of one page the store was asked for.
type resultsCall struct{ After, Limit int }

// call is one call the fake store received.
type call struct {
	Method string
	User   string
	Job    string
	Arg    any
}

// fakeStore is the job store as the API sees it. Every method records itself first; a method named in fail answers that error, else the configured answer.
type fakeStore struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]error

	job      jobs.Job
	preview  jobs.Preview
	confirm  jobs.ConfirmResult
	list     []jobs.Job
	next     string
	events   []jobs.Event
	results  []jobs.Result
	maxRows  int
	created  jobs.CreateReq
	confirmR jobs.ConfirmReq

	// resultsHook runs inside Results (call is 1-based), before it answers; failResultsAt makes that call fail with failResultsErr.
	resultsHook    func(ctx context.Context, call int)
	failResultsAt  int
	failResultsErr error
	resultCalls    []resultsCall

	// moveHook runs inside every move (discard, reopen, cancel, pause, resume), after it was recorded.
	moveHook func(ctx context.Context)
}

func newFakeStore() *fakeStore {
	f := &fakeStore{fail: map[string]error{}, maxRows: 100000}
	f.job = jobs.Job{ID: jobIDA, UserID: userA, Module: "m1", Status: jobs.StatusDraft, Settings: jobs.Settings{UnknownChoice: jobs.UnknownExclude}, CreatedAt: t0, UpdatedAt: t0}
	return f
}

func (f *fakeStore) rec(method, user, job string, arg any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{method, user, job, arg})
	return f.fail[method]
}

// seen returns the calls recorded so far.
func (f *fakeStore) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeStore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.resultCalls = nil, nil
}

func (f *fakeStore) CreateDraft(_ context.Context, user string, req jobs.CreateReq) (jobs.Job, error) {
	if err := f.rec("CreateDraft", user, "", req); err != nil {
		return jobs.Job{}, err
	}
	f.mu.Lock()
	f.created = req
	f.mu.Unlock()
	if len(req.Rows) > f.maxRows {
		return jobs.Job{}, &jobs.TooManyRowsError{Limit: f.maxRows}
	}
	return f.job, nil
}

func (f *fakeStore) Get(_ context.Context, user, job string) (jobs.Job, error) {
	if err := f.rec("Get", user, job, nil); err != nil {
		return jobs.Job{}, err
	}
	return f.job, nil
}

func (f *fakeStore) List(_ context.Context, user string, o jobs.ListOpts) ([]jobs.Job, string, error) {
	if err := f.rec("List", user, "", o); err != nil {
		return nil, "", err
	}
	return f.list, f.next, nil
}

func (f *fakeStore) Preview(_ context.Context, user, job string, opts ...jobs.PreviewOption) (jobs.Preview, error) {
	var arg any // the unknown choice the options carry, or nil when the request carried none
	if choice, given := jobs.PreviewChoice(opts...); given {
		arg = choice
	}
	if err := f.rec("Preview", user, job, arg); err != nil {
		return jobs.Preview{}, err
	}
	return f.preview, nil
}

func (f *fakeStore) Confirm(_ context.Context, user string, req jobs.ConfirmReq) (jobs.ConfirmResult, error) {
	if err := f.rec("Confirm", user, req.JobID, req); err != nil {
		return jobs.ConfirmResult{}, err
	}
	f.mu.Lock()
	f.confirmR = req
	f.mu.Unlock()
	return f.confirm, nil
}

func (f *fakeStore) move(ctx context.Context, name, user, job string) (jobs.Job, error) {
	if err := f.rec(name, user, job, nil); err != nil {
		return jobs.Job{}, err
	}
	if f.moveHook != nil {
		f.moveHook(ctx)
	}
	return f.job, nil
}

func (f *fakeStore) DiscardDraft(c context.Context, u, j string) (jobs.Job, error) {
	return f.move(c, "DiscardDraft", u, j)
}
func (f *fakeStore) Reopen(c context.Context, u, j string) (jobs.Job, error) {
	return f.move(c, "Reopen", u, j)
}
func (f *fakeStore) CancelBeforeStart(c context.Context, u, j string) (jobs.Job, error) {
	return f.move(c, "CancelBeforeStart", u, j)
}
func (f *fakeStore) PauseScheduled(c context.Context, u, j string) (jobs.Job, error) {
	return f.move(c, "PauseScheduled", u, j)
}
func (f *fakeStore) ResumeScheduled(c context.Context, u, j string) (jobs.Job, error) {
	return f.move(c, "ResumeScheduled", u, j)
}

func (f *fakeStore) Events(_ context.Context, user, job string, after int64, limit int) ([]jobs.Event, error) {
	if err := f.rec("Events", user, job, [2]int64{after, int64(limit)}); err != nil {
		return nil, err
	}
	return f.events, nil
}

func (f *fakeStore) Results(ctx context.Context, user, job string, after, limit int) ([]jobs.Result, error) {
	if err := f.rec("Results", user, job, [2]int{after, limit}); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.resultCalls = append(f.resultCalls, resultsCall{After: after, Limit: limit})
	callNo := len(f.resultCalls)
	hook, failAt, failErr := f.resultsHook, f.failResultsAt, f.failResultsErr
	f.mu.Unlock()
	if hook != nil {
		hook(ctx, callNo)
	}
	if err := ctx.Err(); err != nil { // a database call whose context has ended fails with it
		return nil, err
	}
	if failAt == callNo {
		return nil, failErr
	}
	var out []jobs.Result
	for _, r := range f.results {
		if r.Ordinal > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// EachResult is the store's own walk (jobs.EachResultOf) over the fake's Results, so that the page calls it makes are the real ones.
func (f *fakeStore) EachResult(ctx context.Context, user, job string, fn func(jobs.Result) error) error {
	return jobs.EachResultOf(ctx, f, user, job, fn)
}

func (f *fakeStore) MaxRows() int { return f.maxRows }

// pageCalls returns the (after, limit) of every Results call so far.
func (f *fakeStore) pageCalls() []resultsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]resultsCall(nil), f.resultCalls...)
}

// ---- the resolver ----------------------------------------------------------------------------------------------------

type fakeResolver struct {
	mu    sync.Mutex
	users map[string]tenant.Tenant
	errs  map[string]error
	any   error // answered for every login when set
	asked int
}

func (r *fakeResolver) ByAuthUser(_ context.Context, login string) (tenant.Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	if r.any != nil {
		return tenant.Tenant{}, r.any
	}
	if err := r.errs[login]; err != nil {
		return tenant.Tenant{}, err
	}
	if t, ok := r.users[login]; ok {
		return t, nil
	}
	return tenant.Tenant{}, tenant.ErrUnknownLogin
}

// ---- the rig -----------------------------------------------------------------------------------------------------

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", raw)
		}
		out = append(out, m)
	}
	return out
}

type rig struct {
	t     *testing.T
	store *fakeStore
	res   *fakeResolver
	mint  *authtest.Minter
	api   *API
	srv   *httptest.Server
	logs  *syncBuf
}

// rigOpt adjusts a rig before its server starts.
type rigOpt func(*rigCfg)

type rigCfg struct {
	pageMax int
	wrap    func(http.Handler) http.Handler
	mint    func(testing.TB) *authtest.Minter
}

func withPageMax(n int) rigOpt                          { return func(c *rigCfg) { c.pageMax = n } }
func withWrap(w func(http.Handler) http.Handler) rigOpt { return func(c *rigCfg) { c.wrap = w } }
func withES256() rigOpt                                 { return func(c *rigCfg) { c.mint = authtest.NewES256 } }

func newRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	cfg := rigCfg{mint: authtest.NewHS256}
	for _, o := range opts {
		o(&cfg)
	}
	r := &rig{t: t, store: newFakeStore(), logs: &syncBuf{}}
	r.res = &fakeResolver{
		users: map[string]tenant.Tenant{
			loginA: {UserID: userA, AuthUserID: loginA, Active: true},
			loginB: {UserID: userB, AuthUserID: loginB, Active: true},
		},
		errs: map[string]error{},
	}
	r.mint = cfg.mint(t)
	r.mint.Now = func() time.Time { return t0 }
	log, err := logging.New("debug", r.logs)
	if err != nil {
		t.Fatal(err)
	}
	r.api, err = New(Deps{Service: r.store, Verifier: r.mint.Verifier(t), Resolver: r.res, Logger: log, ResultsPageMax: cfg.pageMax})
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler = httpapi.NewHandler(httpapi.Deps{Logger: log, Pinger: pingOK{}, ReadyTimeout: time.Second, Now: func() time.Time { return t0 }, Mount: r.api.Mount})
	if cfg.wrap != nil {
		h = cfg.wrap(h)
	}
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

type pingOK struct{}

func (pingOK) Ping(context.Context) error { return nil }

// token is a legal token of the login.
func (r *rig) token(login string) string { return r.mint.Token(login) }

// req describes one request.
type req struct {
	method, path string
	token        string // "" sends no Authorization header
	body         []byte
	ctype        string
	header       http.Header
	rawAuth      *string   // when set, sent as the Authorization header exactly (overrides token)
	stream       io.Reader // when set, the body, of unknown length (sent chunked): the server cannot judge it by a declared length
}

func (r *rig) do(q req) (*http.Response, []byte) {
	r.t.Helper()
	resp, err := r.doRaw(q)
	if err != nil {
		r.t.Fatalf("%s %s: %v", q.method, q.path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("%s %s: reading the answer: %v", q.method, q.path, err)
	}
	return resp, b
}

func (r *rig) doRaw(q req) (*http.Response, error) {
	var body io.Reader
	switch {
	case q.stream != nil:
		body = io.NopCloser(q.stream)
	case q.body != nil:
		body = bytes.NewReader(q.body)
	}
	hr, err := http.NewRequest(q.method, r.srv.URL+q.path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	for k, vs := range q.header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	switch {
	case q.rawAuth != nil:
		hr.Header.Set("Authorization", *q.rawAuth)
	case q.token != "":
		hr.Header.Set("Authorization", "Bearer "+q.token)
	}
	if q.ctype != "" {
		hr.Header.Set("Content-Type", q.ctype)
	}
	return (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(hr)
}

// get is an authenticated GET as the login.
func (r *rig) get(login, path string) (*http.Response, []byte) {
	r.t.Helper()
	return r.do(req{method: http.MethodGet, path: path, token: r.token(login)})
}

// post is an authenticated POST of a JSON body as the login.
func (r *rig) post(login, path string, body any, h ...http.Header) (*http.Response, []byte) {
	r.t.Helper()
	q := req{method: http.MethodPost, path: path, token: r.token(login), ctype: "application/json"}
	switch b := body.(type) {
	case nil:
	case []byte:
		q.body = b
	case string:
		q.body = []byte(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			r.t.Fatal(err)
		}
		q.body = enc
	}
	if len(h) > 0 {
		q.header = h[0]
	}
	return r.do(q)
}

// errorOf decodes an error envelope: status, code, message, details, request id.
type envelopeOf struct {
	Code, Message, RequestID string
	Details                  map[string]any
	keys                     []string
}

func decodeEnvelope(t *testing.T, resp *http.Response, body []byte) envelopeOf {
	t.Helper()
	var raw map[string]map[string]any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 1 || raw["error"] == nil {
		t.Fatalf("status %d: the body is not an error envelope: %q", resp.StatusCode, body)
	}
	e := raw["error"]
	out := envelopeOf{}
	for k := range e {
		out.keys = append(out.keys, k)
	}
	out.Code, _ = e["code"].(string)
	out.Message, _ = e["message"].(string)
	out.RequestID, _ = e["request_id"].(string)
	out.Details, _ = e["details"].(map[string]any)
	if out.RequestID == "" || out.RequestID != resp.Header.Get("X-Request-Id") {
		t.Fatalf("the envelope's request id %q is not the response header's %q", out.RequestID, resp.Header.Get("X-Request-Id"))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type %q on an error", ct)
	}
	return out
}

// wantError asserts the status and the code of an error answer and returns the envelope.
func (r *rig) wantError(resp *http.Response, body []byte, status int, code string) envelopeOf {
	r.t.Helper()
	if resp.StatusCode != status {
		r.t.Fatalf("status %d, want %d: %s", resp.StatusCode, status, body)
	}
	e := decodeEnvelope(r.t, resp, body)
	if e.Code != code {
		r.t.Fatalf("code %q, want %q: %s", e.Code, code, body)
	}
	return e
}

// wantOK asserts a 200 (or the status) and decodes the JSON body into a generic map.
func (r *rig) wantStatus(resp *http.Response, body []byte, status int) map[string]any {
	r.t.Helper()
	if resp.StatusCode != status {
		r.t.Fatalf("status %d, want %d: %s", resp.StatusCode, status, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		r.t.Fatalf("Content-Type %q on a success", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		r.t.Fatalf("the body is not a JSON object: %q", body)
	}
	return m
}

var ridPattern = regexp.MustCompile(`req_[0-9a-f]{32}`)

// sameBody is a body with its request id blanked, to compare two answers that must be the same answer.
func sameBody(b []byte) string { return ridPattern.ReplaceAllString(string(b), "req_X") }

func uuidFor(n int) string { return fmt.Sprintf("0b000000-0000-4000-8000-%012d", n) }

var errBoom = errors.New("boom")

// httpServer serves a handler on a loopback listener for the life of the test.
func httpServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
