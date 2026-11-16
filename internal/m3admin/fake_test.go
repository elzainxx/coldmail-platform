package m3admin

// An in-process module 3 admin surface: it speaks the wire format the research record documents
// (Docs/history/p2-research/m3-suspend.txt, read from module 3's adminapi code) and nothing else. It is
// for this package's unit tests only. The module itself is never mocked in the harness, where another
// test drives this client against the real Control.
//
// The fake keeps module 3's state (which users exist, which are suspended) and answers by module 3's
// rules, including the trap that unsuspend does not check that the user exists. A test that needs a
// response the model would not give installs a force function, which sees every request first.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

const (
	// testToken is the admin key every test client uses. Not a real secret; recognisable so a test can
	// search every string for it.
	testToken = "zzzz-m3admin-token-zzzz"

	userA = "11111111-2222-4333-8444-555555555555" // exists, not suspended
	userB = "66666666-7777-4888-9999-aaaaaaaaaaaa" // exists, suspended
	userC = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff" // unknown to module 3

	// reqID has module 3's request id shape: "req_" and 32 hex digits.
	reqID = "req_0123456789abcdef0123456789abcdef"
)

var testSecret = config.Secret(testToken)

// seen is one request the fake received.
type seen struct {
	Method, Path, Query, Auth, ContentType string
	Body                                   []byte
}

// reply is one response the fake gives.
type reply struct {
	status int
	body   string
	header map[string]string
	delay  time.Duration // held before answering; ends early if the client goes away or the fake shuts down
	hangup bool          // close the connection without answering
}

type detail struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

type fakeM3 struct {
	srv     *httptest.Server
	release chan struct{}

	mu        sync.Mutex
	reqs      []seen
	known     map[string]bool
	suspended map[string]bool
	force     func(seen) *reply
}

// newFake starts the fake with userA (exists, active) and userB (exists, suspended); userC is unknown.
func newFake(t *testing.T) *fakeM3 {
	t.Helper()
	f := &fakeM3{
		release:   make(chan struct{}),
		known:     map[string]bool{userA: true, userB: true},
		suspended: map[string]bool{userB: true},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// Cleanups run last-in first-out: this releases any handler still holding a request before Close waits for it.
	t.Cleanup(func() { close(f.release) })
	return f
}

func (f *fakeM3) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s := seen{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
		ContentType: r.Header.Get("Content-Type"), Body: body}
	f.mu.Lock()
	f.reqs = append(f.reqs, s)
	force := f.force
	f.mu.Unlock()
	if force != nil {
		if rep := force(s); rep != nil {
			f.answer(w, r, rep)
			return
		}
	}
	f.answer(w, r, f.model(s))
}

func (f *fakeM3) answer(w http.ResponseWriter, r *http.Request, rep *reply) {
	if rep.delay > 0 {
		select {
		case <-time.After(rep.delay):
		case <-r.Context().Done():
			return
		case <-f.release:
			return
		}
	}
	if rep.hangup {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

var anyCaseUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// model answers by module 3's rules: route and method first (404 / 405, before auth), then the bearer
// key (401), then the id (a malformed id is a 404), then the strict body (400), then the state.
func (f *fakeM3) model(s seen) *reply {
	const prefix = "/v1/admin/users/"
	rest, found := strings.CutPrefix(s.Path, prefix)
	id, action, hasAction := strings.Cut(rest, "/")
	if !found || !hasAction {
		return notFound()
	}
	post := action == "suspend" || action == "unsuspend"
	if !post && action != "config" {
		return notFound()
	}
	if post != (s.Method == http.MethodPost) || !post && s.Method != http.MethodGet {
		return envReply(405, "method_not_allowed", "This method is not allowed on this path.")
	}
	if s.Auth != "Bearer "+testToken {
		return envReply(401, "unauthenticated", "Missing or invalid admin API key.")
	}
	if !anyCaseUUID.MatchString(id) {
		return notFound()
	}
	if post && len(s.Body) > 0 {
		var req struct {
			Reason *string `json:"reason"`
		}
		dec := json.NewDecoder(bytes.NewReader(s.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || (req.Reason != nil && len(*req.Reason) > 2000) {
			return envReply(400, "validation_failed", "One or more fields are invalid.", detail{"body", "malformed"})
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch action {
	case "suspend":
		if !f.known[id] {
			return notFound()
		}
		if f.suspended[id] {
			return conflict("This user is already suspended.")
		}
		f.suspended[id] = true
		return &reply{status: 200, body: okBody(id, true)}
	case "unsuspend":
		// No existence check: an unknown user is the same 422 as a user who is not suspended.
		if !f.suspended[id] {
			return conflict("This user is not suspended.")
		}
		delete(f.suspended, id)
		return &reply{status: 200, body: okBody(id, false)}
	}
	// config: 200 for a user that exists (with or without an override row), 404 for one that does not.
	if !f.known[id] {
		return notFound()
	}
	return &reply{status: 200, body: `{"user_id":"` + id + `","overrides":{}}`}
}

// ---- response builders --------------------------------------------------------------------------------------------

// okBody is the 200 body exactly as module 3 writes it.
func okBody(id string, suspended bool) string {
	if suspended {
		return `{"user_id":"` + id + `","suspended":true,"reason":"operator"}`
	}
	return `{"user_id":"` + id + `","suspended":false}`
}

func envBody(code, message string, details ...detail) string {
	e := map[string]any{"code": code, "message": message, "request_id": reqID}
	if len(details) > 0 {
		e["details"] = details
	}
	b, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func envReply(status int, code, message string, details ...detail) *reply {
	return &reply{status: status, body: envBody(code, message, details...)}
}

func notFound() *reply {
	return envReply(404, "not_found", "The requested resource was not found.")
}

func conflict(message string) *reply {
	return envReply(422, "unprocessable", message, detail{"suspended", "conflict"})
}

// ---- forcing, inspecting -----------------------------------------------------------------------------------------

func always(rep *reply) func(seen) *reply { return func(seen) *reply { return rep } }

// onPost forces the answer to the suspend / unsuspend POST only; the existence probe falls through to the model.
func onPost(rep *reply) func(seen) *reply {
	return func(s seen) *reply {
		if s.Method == http.MethodPost {
			return rep
		}
		return nil
	}
}

// onGet forces the answer to the existence probe only.
func onGet(rep *reply) func(seen) *reply {
	return func(s seen) *reply {
		if s.Method == http.MethodGet {
			return rep
		}
		return nil
	}
}

func (f *fakeM3) setForce(fn func(seen) *reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.force = fn
}

func (f *fakeM3) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func (f *fakeM3) count() int { return len(f.requests()) }

func (f *fakeM3) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

func (f *fakeM3) isSuspended(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suspended[id]
}

// newClient builds a client over the fake with the test admin key.
func (f *fakeM3) newClient(t *testing.T, hc *http.Client) *Client {
	t.Helper()
	c, err := New(f.srv.URL, testSecret, hc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// rawDo makes one authenticated request that bypasses the client under test.
func rawDo(t *testing.T, f *fakeM3, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(testCtx(t), method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- assertions --------------------------------------------------------------------------------------------------

// classes names every error class err belongs to, joined with "+", or "none". A correct error is in
// exactly one.
func classes(err error) string {
	var got []string
	if errors.Is(err, ErrUnauthorized) {
		got = append(got, "unauthorized")
	}
	if errors.Is(err, ErrUnknownUser) {
		got = append(got, "unknown-user")
	}
	if errors.Is(err, ErrInvalid) {
		got = append(got, "invalid")
	}
	if Retryable(err) {
		got = append(got, "retryable")
	}
	if len(got) == 0 {
		return "none"
	}
	return strings.Join(got, "+")
}

// wantFailure asserts the call returned no Result and an error in exactly the named class.
func wantFailure(t *testing.T, label string, res Result, err error, class string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want class %s (the call returned %+v)", label, class, res)
		return
	}
	if res != (Result{}) {
		t.Errorf("%s: a Result %+v was reported alongside the error %v", label, res, err)
	}
	if got := classes(err); got != class {
		t.Errorf("%s: error class = %s, want %s (error: %v)", label, got, class, err)
	}
}

func wantResult(t *testing.T, label string, res Result, err error, want Result) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: unexpected error: %v", label, err)
		return
	}
	if res != want {
		t.Errorf("%s: result = %+v, want %+v", label, res, want)
	}
}

// statusOf is the HTTP status a *RetryableError carries, -1 when err is not one.
func statusOf(err error) int {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.HTTPStatus
	}
	return -1
}

func reasonOf(err error) string {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

func stateFor(suspended bool) State {
	if suspended {
		return StateSuspended
	}
	return StateActive
}

// both runs fn once per operation: suspend and unsuspend.
func both(fn func(suspended bool, op string)) {
	fn(true, "suspend")
	fn(false, "unsuspend")
}
