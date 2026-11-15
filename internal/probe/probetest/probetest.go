// Package probetest emulates the four services platform-probe calls, as the contract in
// Docs/probe-reference.md describes them (the contract was read from the module code). It exists for
// unit tests only: the modules themselves are never mocked in the harness, which runs the real ones.
// Nothing outside a _test.go file imports it.
//
// Every emulated service is healthy unless a Wrap breaks it, so a test starts from the whole healthy
// stack and changes exactly one thing.
package probetest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recognisable credentials, so a test can search every byte the probe prints for them. They are not
// real secrets.
const (
	SentinelM1 = "zzzz-sentinel-m1-zzzz"
	SentinelM2 = "zzzz-sentinel-m2-zzzz"
	SentinelM3 = "zzzz-sentinel-m3-zzzz"
)

// Wrap changes a healthy handler: it may serve a path itself and delegate everything else.
type Wrap func(http.Handler) http.Handler

// Options wraps each service. A nil Wrap leaves that service healthy.
type Options struct {
	Platform, M1, M2, M3 Wrap
}

// Req is one request an emulated service received. Auth is "none" (no Authorization header), "right"
// (a bearer value equal to the service's credential) or "wrong" (any other value).
type Req struct {
	Method, Path, Auth string
}

// Stack is the four running emulations.
type Stack struct {
	Platform, M1, M2, M3 *httptest.Server

	mu   sync.Mutex
	seen map[string][]Req
}

// Start runs the four emulations and closes them when the test ends.
func Start(t testing.TB, o Options) *Stack {
	t.Helper()
	s := &Stack{seen: map[string][]Req{}}
	start := func(name, credential string, h http.Handler, w Wrap) *httptest.Server {
		if w != nil {
			h = w(h)
		}
		srv := httptest.NewServer(s.record(name, credential, h))
		t.Cleanup(srv.Close)
		return srv
	}
	s.Platform = start("platform", "", platformHandler(), o.Platform)
	s.M1 = start("m1", SentinelM1, m1Handler(SentinelM1), o.M1)
	s.M2 = start("m2", SentinelM2, senderHandler("send_managed", false, SentinelM2), o.M2)
	s.M3 = start("m3", SentinelM3, senderHandler("send_byo", true, SentinelM3), o.M3)
	return s
}

// Env is a complete, valid environment for the probe pointing at this stack.
func (s *Stack) Env() map[string]string {
	return map[string]string{
		"PROBE_PLATFORM_URL": s.Platform.URL,
		"PROBE_M1_URL":       s.M1.URL,
		"PROBE_M1_KEY":       SentinelM1,
		"PROBE_M2_URL":       s.M2.URL,
		"PROBE_M2_TOKEN":     SentinelM2,
		"PROBE_M3_URL":       s.M3.URL,
		"PROBE_M3_TOKEN":     SentinelM3,
	}
}

// Requests returns what the named service ("platform", "m1", "m2", "m3") received, in order.
func (s *Stack) Requests(name string) []Req {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Req(nil), s.seen[name]...)
}

// record sits outside the Wrap, so it sees every request, whatever the fault does with it.
func (s *Stack) record(name, credential string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := "none"
		if tok, present := bearer(r); present {
			auth = "wrong"
			if credential != "" && tok == credential {
				auth = "right"
			}
		} else if r.Header.Get("Authorization") != "" {
			auth = "wrong"
		}
		s.mu.Lock()
		s.seen[name] = append(s.seen[name], Req{r.Method, r.URL.Path, auth})
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// ClosedURL returns the URL of a port nothing listens on: connecting is refused.
func ClosedURL(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("cannot release the port: %v", err)
	}
	return "http://" + addr
}

// ---- the healthy services --------------------------------------------------------------------------------------

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// RequestID mints a sender-style request id: req_ and 32 lowercase hex digits.
func RequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// M1RequestID mints a module-1-style request id: a UUID (version 7 layout), not req_<hex>.
func M1RequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Envelope is the error envelope every module answers an error with.
func Envelope(code, requestID string) map[string]any {
	return map[string]any{"error": map[string]any{
		"code": code, "message": "emulated message", "request_id": requestID,
	}}
}

func bearer(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

func authorised(r *http.Request, credential string) bool {
	tok, ok := bearer(r)
	return ok && tok == credential
}

func platformHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"status": "ok", "at": now()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"status": "ready", "checks": map[string]any{"db": true}, "db": true, "at": now()})
	})
	return mux
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func m1Handler(key string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"status": "ok", "at": now()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{
			"status": "ready", "checks": map[string]any{"pepper": true, "db": true},
			"at": now(), "ready": true, "pepper": "set",
		})
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, key) {
			WriteJSON(w, 401, Envelope("unauthenticated", M1RequestID()))
			return
		}
		if !uuidRE.MatchString(r.PathValue("id")) {
			WriteJSON(w, 400, Envelope("validation_failed", M1RequestID()))
			return
		}
		WriteJSON(w, 404, Envelope("not_found", M1RequestID()))
	})
	return mux
}

// Describe is the healthy body of GET /v1/describe. Module 3 (extension true) additionally carries
// extension_routes, schemas and bulk_add; module 2 carries none of the three.
func Describe(module string, extension bool) map[string]any {
	d := map[string]any{
		"module":       module,
		"api_version":  "v1",
		"capabilities": map[string]any{"single_send": true},
		"pricing":      map[string]any{"currency": "credits", "model": "flat_per_recipient"},
		"vocabularies": map[string]any{"job_state": map[string]any{
			"source": "db_enum", "values": []any{"queued", "running", "done"}, "fields": []any{"run.state"},
		}},
		"error_codes": []any{
			map[string]any{"code": "unauthenticated", "http_status": 401},
			map[string]any{"code": "not_found", "http_status": 404},
		},
	}
	if extension {
		d["extension_routes"] = []any{map[string]any{"method": "POST", "path": "/v1/send-byo/accounts"}}
		d["schemas"] = map[string]any{"account": map[string]any{"type": "object"}}
		d["bulk_add"] = map[string]any{"max_rows": 100}
	}
	return d
}

func senderHandler(module string, extension bool, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"status": "ok", "at": now()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{
			"status": "ready", "checks": map[string]any{"db": true, "singleton_lock": true},
			"db": true, "singleton_lock": true, "at": now(),
		})
	})
	mux.HandleFunc("GET /v1/describe", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r, token) {
			WriteJSON(w, 401, Envelope("unauthenticated", RequestID()))
			return
		}
		WriteJSON(w, 200, Describe(module, extension))
	})
	return mux
}

// ---- faults ----------------------------------------------------------------------------------------------------

// Replace serves path itself. A path ending in "/" matches everything under it.
func Replace(path string, h http.HandlerFunc) Wrap {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path || strings.HasSuffix(path, "/") && strings.HasPrefix(r.URL.Path, path) {
				h(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MutateJSON lets fn edit the healthy JSON answer for path (and its status) before it is sent.
func MutateJSON(path string, fn func(r *http.Request, status int, body map[string]any) (int, map[string]any)) Wrap {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			status, body := fn(r, rec.Code, body)
			WriteJSON(w, status, body)
		})
	}
}

// Slow answers path with a healthy health body, but only after d (or never, if the client gives up first).
func Slow(path string, d time.Duration) Wrap {
	return Replace(path, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		WriteJSON(w, 200, map[string]any{"status": "ok", "at": now()})
	})
}

// Chain applies the wraps so that the first is outermost.
func Chain(wraps ...Wrap) Wrap {
	return func(h http.Handler) http.Handler {
		for i := len(wraps) - 1; i >= 0; i-- {
			h = wraps[i](h)
		}
		return h
	}
}
