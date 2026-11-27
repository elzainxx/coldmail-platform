// Package httpapi is the platform's HTTP surface. Phase 0 serves only the two probe endpoints, but the
// scaffolding every later route inherits is complete: a request id on every response, the coded error
// envelope for unknown paths, wrong verbs and panics, an access log, and panic recovery.
//
//	GET /healthz  200 {"status":"ok","at":...}                      liveness, no checks
//	GET /readyz   200|503 {"status","checks":{"db"},"db","at"}      readiness: database AND not draining
//
// Both probe paths accept any method, like the three modules'.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

// Pinger is the one dependency readiness needs. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(context.Context) error
}

// Deps are the handler's dependencies. Pinger and a positive ReadyTimeout are required (NewHandler panics
// without them: a missing dependency is a wiring bug and must stop the process at start, not answer
// "not ready" for ever). The rest default: no Logger discards, no Draining never drains, no Now is
// time.Now.
type Deps struct {
	Logger       *slog.Logger
	Pinger       Pinger
	Draining     func() bool
	ReadyTimeout time.Duration
	Now          func() time.Time
	// Mount, when set, registers the routes of another package (the job API, internal/api) on the handler's mux. They sit beneath the same middleware as the probes: a request id on every
	// response, the access log (without the query string) and the panic recovery. nil mounts nothing and the catch-all answers every other path with the 404 envelope, as before. Such a
	// package writes its errors with WriteError so that they have the envelope of this one.
	Mount func(mux *http.ServeMux)
}

type service struct {
	log          *slog.Logger
	pinger       Pinger
	draining     func() bool
	readyTimeout time.Duration
	now          func() time.Time
}

// NewHandler returns the platform's handler.
func NewHandler(d Deps) http.Handler { return newHandler(d, nil) }

// newHandler is NewHandler plus a seam for tests: extra registers additional routes on the mux (a
// verb-restricted route to provoke a 405, a route that panics) beneath the same middleware. Production
// code passes nil.
func newHandler(d Deps, extra func(*http.ServeMux)) http.Handler {
	if d.Pinger == nil {
		panic("httpapi: Deps.Pinger is required")
	}
	if d.ReadyTimeout <= 0 {
		panic("httpapi: Deps.ReadyTimeout must be positive")
	}
	s := &service{pinger: d.Pinger, draining: d.Draining, readyTimeout: d.ReadyTimeout, now: d.Now}
	if d.Logger == nil {
		s.log = slog.New(slog.DiscardHandler)
	} else {
		// Whatever logger the caller built, every line a request produces must carry its id.
		s.log = logging.Contextual(d.Logger)
	}
	if s.draining == nil {
		s.draining = func() bool { return false }
	}
	if s.now == nil {
		s.now = time.Now
	}

	mux := http.NewServeMux()
	// No method in the pattern: the probes accept any verb (the modules' behaviour), so a load balancer
	// or a curl -X POST never gets a 405 from them.
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	if d.Mount != nil {
		d.Mount(mux)
	}
	if extra != nil {
		extra(mux)
	}
	// The catch-all. Because "/" matches every method and path, the mux never produces its own plain-text
	// 404 or 405; this handler decides which of the two it is and answers in the envelope.
	mux.HandleFunc("/", s.noRoute(mux))

	return s.requestID(s.accessLog(s.recoverer(mux)))
}

// requestID mints the id, exposes it on the response BEFORE anything can fail, and puts it in the
// context. An inbound X-Request-Id is deliberately ignored: a caller-chosen id would be a way to forge
// log correlation.
func (s *service) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// statusWriter records what was written so the access log can report the status and the panic recovery
// can tell whether an error envelope can still be sent.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx informational responses are not the final status (101 Switching Protocols is).
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog writes one line per request: method, path, status, duration_ms (and request_id, attached by
// the logger from the context). The query string is never logged (it is where tokens end up); the path is
// a structured attribute, so a path carrying control characters is JSON-escaped and cannot forge a second
// log line.
func (s *service) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelWarn
		}
		s.log.LogAttrs(r.Context(), level, "http_request",
			slog.String("method", r.Method),
			slog.String("path", loggedPath(r.URL.Path)),
			slog.Int("status", sw.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// secretPathPrefixes are the paths whose remainder is a credential: the public unsubscribe routes (/u/{token}, internal/unsubscribe/endpoint) carry a per-recipient token in the path, and the access
// log must not hold it (a log is read by more people than a link is sent to).
var secretPathPrefixes = []string{"/u/"}

// loggedPath is the path as the access log records it: the path itself, except that everything after a secret prefix is replaced by [token], whatever it is and whether or not a route took it.
func loggedPath(path string) string {
	for _, p := range secretPathPrefixes {
		if strings.HasPrefix(path, p) {
			return p + "[token]"
		}
	}
	return path
}

// recoverer turns a handler panic into a 500 envelope. The panic value and stack go to the log under the
// request id; the response carries only the fixed message. If the response has already started, no
// envelope can be sent and the status stays as written. http.ErrAbortHandler is re-panicked: it is the
// standard way for a handler to abort a response and net/http handles it silently.
func (s *service) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			ctx := r.Context()
			s.log.ErrorContext(ctx, "panic in handler", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			if sw, ok := w.(*statusWriter); ok && sw.wrote {
				return
			}
			writeError(w, logging.RequestID(ctx), codeInternalError)
		}()
		next.ServeHTTP(w, r)
	})
}

// probeMethods are the verbs tried, in this order, to build an Allow header.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// noRoute answers a request no real route took: 405 with an Allow header when the path exists for other
// verbs, 404 otherwise. Which verbs a path serves is asked of the mux itself (a probe request per verb,
// a real route being any pattern other than the catch-all), so the answer cannot drift from the routes
// actually registered.
func (s *service) noRoute(mux *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := logging.RequestID(r.Context())
		var allowed []string
		for _, m := range probeMethods {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "" && pattern != "/" {
				allowed = append(allowed, m)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, rid, codeMethodNotAllowed)
			return
		}
		writeError(w, rid, codeNotFound)
	}
}
