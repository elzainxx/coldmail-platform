package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

var fixedNow = time.Date(2026, 11, 15, 12, 30, 45, 123456789, time.UTC)

var requestIDShape = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

// syncBuffer is a log sink that is safe to read while handlers (and the ping goroutines they start) write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lines decodes every log line written so far.
func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

type env struct {
	h        http.Handler
	logs     *syncBuffer
	draining atomic.Bool
	pings    atomic.Int32
}

func healthy(context.Context) error { return nil }

// newEnv builds a handler over a counting Pinger, a draining switch, a fixed clock and a debug logger.
func newEnv(t *testing.T, ping func(context.Context) error, timeout time.Duration, extra func(*http.ServeMux)) *env {
	t.Helper()
	e := &env{logs: &syncBuffer{}}
	log, err := logging.New("debug", e.logs)
	if err != nil {
		t.Fatal(err)
	}
	e.h = newHandler(Deps{
		Logger: log,
		Pinger: pingerFunc(func(ctx context.Context) error {
			e.pings.Add(1)
			return ping(ctx)
		}),
		Draining:     e.draining.Load,
		ReadyTimeout: timeout,
		Now:          func() time.Time { return fixedNow },
	}, extra)
	return e
}

func (e *env) do(method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not a JSON object: %q: %v", rec.Body.String(), err)
	}
	return m
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- /healthz and /readyz ----------------------------------------------------------------------------------

func TestHealthz_ShapeAndNoChecks(t *testing.T) {
	e := newEnv(t, func(context.Context) error { return errors.New("database is down") }, time.Second, nil)
	rec := e.do(http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 even with a failing database", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
	body := decode(t, rec)
	if !reflect.DeepEqual(keys(body), []string{"at", "status"}) {
		t.Fatalf("healthz keys = %v, want exactly [at status] (no checks)", keys(body))
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	at, ok := body["at"].(string)
	if !ok || !strings.HasSuffix(at, "Z") {
		t.Fatalf("at = %v, want an RFC3339 UTC string ending in Z", body["at"])
	}
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil || !parsed.Equal(fixedNow) {
		t.Errorf("at = %q parses to %v (%v), want %v", at, parsed, err, fixedNow)
	}
	if n := e.pings.Load(); n != 0 {
		t.Errorf("healthz called the database Pinger %d times; liveness runs no checks", n)
	}

	// Paired control: readyz on the same handler DOES consult the Pinger, so the zero above is a
	// property of healthz and not of a Pinger that is never wired.
	e.do(http.MethodGet, "/readyz")
	if n := e.pings.Load(); n != 1 {
		t.Errorf("control failed: readyz made %d pings, want 1", n)
	}
}

func TestReadyz_Ready200Shape(t *testing.T) {
	e := newEnv(t, healthy, time.Second, nil)
	rec := e.do(http.MethodGet, "/readyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if !reflect.DeepEqual(keys(body), []string{"at", "checks", "db", "status"}) {
		t.Fatalf("readyz keys = %v, want [at checks db status]", keys(body))
	}
	if body["status"] != "ready" || body["db"] != true {
		t.Errorf("body = %v", body)
	}
	if !reflect.DeepEqual(body["checks"], map[string]any{"db": true}) {
		t.Errorf("checks = %v, want {db:true}", body["checks"])
	}
	if at, _ := body["at"].(string); !strings.HasSuffix(at, "Z") {
		t.Errorf("at = %v is not UTC", body["at"])
	}
}

func TestReadyz_PingErrorIs503(t *testing.T) {
	e := newEnv(t, func(context.Context) error { return errors.New("connection refused") }, time.Second, nil)
	rec := e.do(http.MethodGet, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	body := decode(t, rec)
	if body["status"] != "not_ready" || body["db"] != false || !reflect.DeepEqual(body["checks"], map[string]any{"db": false}) {
		t.Errorf("body = %v", body)
	}
	if e.pings.Load() != 1 {
		t.Errorf("control failed: the Pinger was called %d times, want 1", e.pings.Load())
	}
}

func TestReadyz_PingBlockedPastTheTimeoutIs503WithinTwiceTheTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	upper := 2*timeout + 100*time.Millisecond

	// A Pinger that IGNORES its context, the worst case: only the handler's own deadline can save the probe.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var started, returned atomic.Bool
	e := newEnv(t, func(context.Context) error {
		started.Store(true)
		<-release
		returned.Store(true)
		return nil
	}, timeout, nil)

	t0 := time.Now()
	rec := e.do(http.MethodGet, "/readyz")
	took := time.Since(t0)

	if rec.Code != http.StatusServiceUnavailable || decode(t, rec)["db"] != false {
		t.Fatalf("status %d body %s, want 503 db:false", rec.Code, rec.Body)
	}
	if took >= upper {
		t.Errorf("the response took %v, want under %v (about twice the %v timeout)", took, upper, timeout)
	}
	// Anti-vacuity: the Pinger really was called and really was still blocked when the answer arrived;
	// and the handler really waited for the deadline instead of failing fast for another reason.
	if !started.Load() || returned.Load() {
		t.Errorf("control failed: pinger started=%v returned=%v; it must be started and still blocked", started.Load(), returned.Load())
	}
	if took < timeout-20*time.Millisecond {
		t.Errorf("the response arrived after %v, before the %v deadline: the block was not what produced the 503", took, timeout)
	}
}

func TestReadyz_ContextHonouringPingerAndSlowButInTimePinger(t *testing.T) {
	const timeout = 300 * time.Millisecond

	// A well-behaved Pinger that waits for its context: 503, and quickly.
	e := newEnv(t, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, timeout, nil)
	t0 := time.Now()
	rec := e.do(http.MethodGet, "/readyz")
	if took := time.Since(t0); rec.Code != http.StatusServiceUnavailable || took >= 2*timeout+100*time.Millisecond {
		t.Errorf("context-honouring pinger: status %d after %v", rec.Code, took)
	}

	// Paired control: a Pinger that is slow but answers INSIDE the timeout gives 200, after actually
	// having been slow. Without this, "always 503 once a pinger takes a while" would pass the test above.
	slow := newEnv(t, func(context.Context) error { time.Sleep(timeout / 3); return nil }, timeout, nil)
	t0 = time.Now()
	rec = slow.do(http.MethodGet, "/readyz")
	took := time.Since(t0)
	if rec.Code != http.StatusOK {
		t.Fatalf("slow-but-in-time pinger: status %d, want 200: %s", rec.Code, rec.Body)
	}
	if took < timeout/3-10*time.Millisecond || took >= timeout {
		t.Errorf("control failed: the slow pinger took %v, want between %v and %v", took, timeout/3, timeout)
	}
}

func TestReadyz_DrainingIs503EvenWhenThePingIsHealthy(t *testing.T) {
	e := newEnv(t, healthy, time.Second, nil)

	e.draining.Store(true)
	rec := e.do(http.MethodGet, "/readyz")
	body := decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("draining: status %d body %v, want 503 not_ready", rec.Code, body)
	}
	// The database check is reported truthfully: it is healthy, the instance is just leaving.
	if body["db"] != true || !reflect.DeepEqual(body["checks"], map[string]any{"db": true}) {
		t.Errorf("draining with a healthy ping must still report db:true, got %v", body)
	}
	if e.pings.Load() != 1 {
		t.Errorf("control failed: the healthy pinger was consulted %d times, want 1", e.pings.Load())
	}

	// Paired control: the SAME handler and pinger answer 200 once the drain flag clears.
	e.draining.Store(false)
	if rec := e.do(http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("control failed: not draining, healthy ping, status %d", rec.Code)
	}
}

func TestProbes_AcceptAnyMethod_WhileARegisteredVerbIsEnforced(t *testing.T) {
	e := newEnv(t, healthy, time.Second, func(m *http.ServeMux) {
		m.HandleFunc("GET /only-get", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		for _, path := range []string{"/healthz", "/readyz"} {
			if rec := e.do(method, path); rec.Code != http.StatusOK {
				t.Errorf("%s %s = %d, want 200 (the probes accept any method)", method, path, rec.Code)
			}
		}
	}
	// Paired control: a route registered with a verb DOES refuse the others, so the 200s above are a
	// property of the probe routes, not of a handler that accepts everything.
	if rec := e.do("POST", "/only-get"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("control failed: POST to a GET-only route = %d, want 405", rec.Code)
	}
}

// ---- error envelopes ---------------------------------------------------------------------------------------

func envelope(t *testing.T, rec *httptest.ResponseRecorder, code, message string) {
	t.Helper()
	body := decode(t, rec)
	if !reflect.DeepEqual(keys(body), []string{"error"}) {
		t.Fatalf("envelope keys = %v, want [error]", keys(body))
	}
	inner, ok := body["error"].(map[string]any)
	if !ok || !reflect.DeepEqual(keys(inner), []string{"code", "message", "request_id"}) {
		t.Fatalf("error object = %v, want exactly code, message, request_id", body["error"])
	}
	if inner["code"] != code || inner["message"] != message {
		t.Errorf("error = %v, want code %q message %q", inner, code, message)
	}
	if rid := rec.Header().Get(RequestIDHeader); rid == "" || inner["request_id"] != rid {
		t.Errorf("request_id %v does not match the %s header %q", inner["request_id"], RequestIDHeader, rid)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
}

func TestErrorEnvelope_NotFound(t *testing.T) {
	e := newEnv(t, healthy, time.Second, nil)

	rec := e.do(http.MethodGet, "/no/such/path")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	envelope(t, rec, "not_found", "The requested resource was not found.")

	// Nothing the caller sent is reflected, and a near-miss of a real path is still a 404.
	rec = e.do(http.MethodGet, "/reflect-me-ZZZ?x=QQQ")
	if strings.Contains(rec.Body.String(), "ZZZ") || strings.Contains(rec.Body.String(), "QQQ") {
		t.Errorf("the 404 body reflects the request: %s", rec.Body)
	}
	if rec := e.do(http.MethodGet, "/healthz/"); rec.Code != http.StatusNotFound {
		t.Errorf("/healthz/ = %d, want 404", rec.Code)
	}
	// Paired control: a real path is not a 404.
	if rec := e.do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("control failed: /healthz = %d", rec.Code)
	}
}

func TestErrorEnvelope_MethodNotAllowedCarriesAllow(t *testing.T) {
	e := newEnv(t, healthy, time.Second, func(m *http.ServeMux) {
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		m.HandleFunc("GET /only-get", ok)
		m.HandleFunc("POST /only-post", ok)
		m.HandleFunc("GET /multi", ok)
		m.HandleFunc("PUT /multi", ok)
		m.HandleFunc("DELETE /multi", ok)
	})

	cases := []struct{ method, path, allow string }{
		{"POST", "/only-get", "GET, HEAD"}, // a GET route serves HEAD too, exactly as net/http does
		{"DELETE", "/only-get", "GET, HEAD"},
		{"GET", "/only-post", "POST"},
		{"PATCH", "/multi", "GET, HEAD, PUT, DELETE"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := e.do(tc.method, tc.path)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			envelope(t, rec, "method_not_allowed", "This method is not allowed on this path.")
		})
	}

	// Paired control: the right verb reaches the route, and an unknown path is a 404 with NO Allow.
	if rec := e.do("GET", "/only-get"); rec.Code != http.StatusNoContent {
		t.Fatalf("control failed: GET /only-get = %d", rec.Code)
	}
	rec := e.do("POST", "/unknown")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Allow") != "" {
		t.Fatalf("an unknown path gave %d with Allow %q, want 404 and none", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestErrorEnvelope_PanicIs500AndTheDetailStaysInTheLog(t *testing.T) {
	const canary = "boom-CANARY-7731"
	e := newEnv(t, healthy, time.Second, func(m *http.ServeMux) {
		m.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic(canary) })
	})

	rec := e.do(http.MethodGet, "/boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	envelope(t, rec, "internal_error", "An unexpected error occurred.")
	if strings.Contains(rec.Body.String(), canary) {
		t.Fatalf("the panic value reached the client: %s", rec.Body)
	}

	// The detail is in the log, under the same request id (and that is what makes the redaction above
	// a decision rather than an accident: the value is available and was withheld).
	var found bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "panic in handler" {
			found = true
			if l["panic"] != canary || l[logging.RequestIDKey] != rec.Header().Get(RequestIDHeader) || l["stack"] == "" {
				t.Errorf("panic log line = %v", l)
			}
		}
	}
	if !found {
		t.Fatalf("no panic line in the log: %s", e.logs.String())
	}

	// The process is still serving.
	if rec := e.do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("after a panic, /healthz = %d", rec.Code)
	}
}

func TestRecoverer_ResponseAlreadyStartedAndAbortHandler(t *testing.T) {
	e := newEnv(t, healthy, time.Second, func(m *http.ServeMux) {
		m.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("partial"))
			panic("after the response started")
		})
		m.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	})

	// A panic after the first byte cannot become a 500: the status stays and no envelope is appended.
	rec := e.do(http.MethodGet, "/half")
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Fatalf("half-written response became %d %q, want 202 \"partial\" untouched", rec.Code, rec.Body)
	}
	var logged bool
	for _, l := range e.logs.lines(t) {
		logged = logged || l["msg"] == "panic in handler"
	}
	if !logged {
		t.Error("the panic after the response started was not logged")
	}

	// ErrAbortHandler is re-panicked (net/http aborts the connection quietly) and is not logged as a panic.
	before := len(e.logs.lines(t))
	func() {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", r)
			}
		}()
		e.do(http.MethodGet, "/abort")
	}()
	for _, l := range e.logs.lines(t)[before:] {
		if l["msg"] == "panic in handler" {
			t.Errorf("an intentional abort was logged as a panic: %v", l)
		}
	}
}

// ---- request ids and logging -------------------------------------------------------------------------------

func TestRequestID_ShapeUniquenessAndPresenceOnEveryStatus(t *testing.T) {
	e := newEnv(t, healthy, time.Second, func(m *http.ServeMux) {
		m.HandleFunc("GET /only-get", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		m.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("x") })
		m.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(logging.RequestID(r.Context())))
		})
	})

	targets := []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", 200}, {"GET", "/readyz", 200}, {"GET", "/nope", 404},
		{"POST", "/only-get", 405}, {"GET", "/boom", 500},
	}
	seen := map[string]bool{}
	const rounds = 40
	for i := 0; i < rounds; i++ {
		for _, tc := range targets {
			rec := e.do(tc.method, tc.path)
			if rec.Code != tc.status {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
			}
			id := rec.Header().Get(RequestIDHeader)
			if !requestIDShape.MatchString(id) {
				t.Fatalf("%s %s: request id %q is not req_<32 hex>", tc.method, tc.path, id)
			}
			if seen[id] {
				t.Fatalf("request id %s was issued twice", id)
			}
			seen[id] = true
		}
	}
	if want := rounds * len(targets); len(seen) != want {
		t.Fatalf("%d distinct ids, want %d", len(seen), want)
	}

	// The id is in the handler's context, and equals the one on the response.
	rec := e.do("GET", "/whoami")
	if got := rec.Body.String(); got != rec.Header().Get(RequestIDHeader) || !requestIDShape.MatchString(got) {
		t.Errorf("context request id %q != header %q", got, rec.Header().Get(RequestIDHeader))
	}

	// A caller-supplied id is not trusted: it would be a way to forge log correlation.
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set(RequestIDHeader, "req_attacker-chosen")
	out := httptest.NewRecorder()
	e.h.ServeHTTP(out, req)
	if id := out.Header().Get(RequestIDHeader); id == "req_attacker-chosen" || !requestIDShape.MatchString(id) {
		t.Errorf("inbound id was honoured or the minted one is malformed: %q", id)
	}
}

func TestRequestID_IsOnEveryLogLineARequestProduces(t *testing.T) {
	// A PLAIN slog logger (no request-id handler of ours): NewHandler must make the property hold anyway.
	var raw syncBuffer
	plain := slog.New(slog.NewJSONHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newHandler(Deps{
		Logger:       plain,
		Pinger:       pingerFunc(func(context.Context) error { return errors.New("down") }),
		ReadyTimeout: time.Second,
	}, func(m *http.ServeMux) {
		m.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("x") })
	})

	// Paired control: the same plain logger, used directly with a request-id context, carries no id.
	plain.InfoContext(logging.WithRequestID(context.Background(), "req_control"), "direct")
	if l := raw.lines(t); len(l) != 1 || l[0][logging.RequestIDKey] != nil {
		t.Fatalf("control failed: a plain slog logger already attaches the id: %v", l)
	}

	cases := []struct {
		method, path string
		minLines     int // an access line, plus the failure line when the request produces one
	}{
		{"GET", "/healthz", 1},
		{"GET", "/readyz", 2}, // ping-failure warning + access line
		{"GET", "/nope", 1},
		{"GET", "/boom", 2}, // panic + access line
	}
	for _, tc := range cases {
		before := len(raw.lines(t))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		id := rec.Header().Get(RequestIDHeader)
		produced := raw.lines(t)[before:]
		if len(produced) < tc.minLines {
			t.Fatalf("%s %s produced %d log lines, want at least %d: %s", tc.method, tc.path, len(produced), tc.minLines, raw.String())
		}
		for _, l := range produced {
			if l[logging.RequestIDKey] != id {
				t.Errorf("%s %s: log line %q has request_id %v, want %s", tc.method, tc.path, l["msg"], l[logging.RequestIDKey], id)
			}
		}
	}
}

func TestAccessLog_FieldsLevelAndLogForgeryResistance(t *testing.T) {
	e := newEnv(t, func(ctx context.Context) error { return errors.New("down") }, time.Second, nil)

	// Fields, and the query string (where tokens end up) is not logged.
	rec := e.do(http.MethodPost, "/healthz?token=QUERYSECRET")
	lines := e.logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("one request produced %d log lines: %s", len(lines), e.logs.String())
	}
	l := lines[0]
	if l["msg"] != "http_request" || l["method"] != "POST" || l["path"] != "/healthz" || l["status"] != float64(200) ||
		l[logging.RequestIDKey] != rec.Header().Get(RequestIDHeader) {
		t.Errorf("access line = %v", l)
	}
	if d, ok := l["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms = %v, want a non-negative number", l["duration_ms"])
	}
	if l["level"] != "INFO" {
		t.Errorf("a 200 was logged at %v, want INFO", l["level"])
	}
	if strings.Contains(e.logs.String(), "QUERYSECRET") || strings.Contains(e.logs.String(), "token") {
		t.Errorf("the query string reached the log: %s", e.logs.String())
	}

	// A 5xx is logged at WARN (the readyz failure line comes first, then the access line).
	before := len(e.logs.lines(t))
	e.do(http.MethodGet, "/readyz")
	var access map[string]any
	for _, l := range e.logs.lines(t)[before:] {
		if l["msg"] == "http_request" {
			access = l
		}
	}
	if access == nil || access["status"] != float64(503) || access["level"] != "WARN" {
		t.Errorf("503 access line = %v, want WARN with status 503", access)
	}

	// A path carrying an encoded newline stays on ONE line (a structured attribute is JSON-escaped), and
	// the decoded newline is really in the value: the control that the request was not sanitised away.
	fresh := newEnv(t, healthy, time.Second, nil)
	fresh.do(http.MethodGet, "/a%0Aforged%20line")
	if n := strings.Count(strings.TrimRight(fresh.logs.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("a request path forged extra log lines (%d newlines inside): %q", n, fresh.logs.String())
	}
	got := fresh.logs.lines(t)
	if len(got) != 1 || got[0]["path"] != "/a\nforged line" {
		t.Fatalf("path in the log = %v, want the decoded path with its newline escaped inside the JSON string", got)
	}
	if line := fmt.Sprintf("path=%s", got[0]["path"]); !strings.Contains(line, "\n") {
		t.Fatal("control failed: a plain key=value rendering of this path would not have forged a line, so the test proves nothing")
	}
}

// ---- secrets and robustness --------------------------------------------------------------------------------

func TestReadyz_APingerErrorCarryingASecretNeverReachesTheResponseOrTheLog(t *testing.T) {
	const canary = "CANARY-s3cr3t-4471"
	secretErr := fmt.Errorf("failed to connect to host=db user=platform_app: postgres://platform_app:%s@db:5432/x refused", canary)

	for name, ping := range map[string]func(context.Context) error{
		"error":                func(context.Context) error { return secretErr },
		"wrapped error":        func(context.Context) error { return fmt.Errorf("ping: %w", secretErr) },
		"panic with the error": func(context.Context) error { panic(secretErr) },
		"panic with a string":  func(context.Context) error { panic("password=" + canary) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, ping, time.Second, nil)
			rec := e.do(http.MethodGet, "/readyz")

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503", rec.Code)
			}
			if strings.Contains(rec.Body.String(), canary) || strings.Contains(fmt.Sprint(rec.Header()), canary) {
				t.Errorf("the secret reached the response: %s", rec.Body)
			}
			logs := e.logs.String()
			if strings.Contains(logs, canary) {
				t.Errorf("the secret reached the log: %s", logs)
			}
			// Anti-vacuity: the failure was logged (a fixed message and a reason), so the absence of the
			// canary is not the absence of any logging.
			var warned bool
			for _, l := range e.logs.lines(t) {
				if l["msg"] == "readiness check failed: database ping" {
					warned = true
					if r := l["reason"]; r != "error" && r != "panic" {
						t.Errorf("reason = %v", r)
					}
				}
			}
			if !warned {
				t.Fatalf("no readiness-failure line was logged: %s", logs)
			}
			// A panicking Pinger must not have taken the process down: the next probe is answered.
			if rec := e.do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
				t.Errorf("after the failure, /healthz = %d", rec.Code)
			}
		})
	}
	if !strings.Contains(secretErr.Error(), canary) {
		t.Fatal("control failed: the error under test does not contain the canary")
	}
}

func TestNewHandler_RejectsMiswiringAndDefaultsTheOptionalDeps(t *testing.T) {
	mustPanic := func(name string, d Deps) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: NewHandler accepted the miswired Deps", name)
			}
		}()
		NewHandler(d)
	}
	mustPanic("no pinger", Deps{ReadyTimeout: time.Second})
	mustPanic("zero timeout", Deps{Pinger: pingerFunc(healthy)})
	mustPanic("negative timeout", Deps{Pinger: pingerFunc(healthy), ReadyTimeout: -time.Second})

	// Paired control: the minimal valid Deps works, with a discarded log, no draining and the real clock.
	h := NewHandler(Deps{Pinger: pingerFunc(healthy), ReadyTimeout: time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("minimal Deps: /readyz = %d", rec.Code)
	}
	at, err := time.Parse(time.RFC3339, decode(t, rec)["at"].(string))
	if err != nil || time.Since(at) > time.Minute || time.Since(at) < -time.Minute {
		t.Errorf("default clock gave at=%v (%v)", at, err)
	}
}
