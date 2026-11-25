package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

// Deps.Mount and WriteError (Phase 4 B14b): the job API's routes are registered by another package, beneath the same middleware, and answer in the same envelope. Every test below has a
// paired control: what is absent without the thing it proves (a request with no Mount, an envelope with no details given), so a test cannot pass because the property is true everywhere.

// mounted builds a handler whose Deps.Mount registers the given routes (the production seam: not the test hook `extra`).
func mounted(t *testing.T, routes func(*http.ServeMux)) *env {
	t.Helper()
	e := &env{logs: &syncBuffer{}}
	log, err := logging.New("debug", e.logs)
	if err != nil {
		t.Fatal(err)
	}
	e.h = NewHandler(Deps{
		Logger:       log,
		Pinger:       pingerFunc(healthy),
		ReadyTimeout: time.Second,
		Now:          func() time.Time { return fixedNow },
		Mount:        routes,
	})
	return e
}

func TestMount_RoutesAreServedBeneathTheSameMiddleware(t *testing.T) {
	e := mounted(t, func(m *http.ServeMux) {
		m.HandleFunc("GET /v1/thing", func(w http.ResponseWriter, r *http.Request) {
			// the request id is in the context the route sees, and is the one the response carries
			writeJSON(w, http.StatusOK, map[string]string{"id": logging.RequestID(r.Context())})
		})
		m.HandleFunc("POST /v1/boom", func(http.ResponseWriter, *http.Request) { panic("mounted route panics") })
	})

	rec := e.do(http.MethodGet, "/v1/thing?q=QUERY-CANARY-4417")
	if rec.Code != http.StatusOK {
		t.Fatalf("a mounted route answered %d, want 200", rec.Code)
	}
	rid := rec.Header().Get(RequestIDHeader)
	if !requestIDShape.MatchString(rid) || decode(t, rec)["id"] != rid {
		t.Fatalf("the mounted route saw request id %v, the response carries %q", decode(t, rec)["id"], rid)
	}
	// the access log names the path and never the query
	var logged bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "http_request" && l["path"] == "/v1/thing" && l[logging.RequestIDKey] == rid {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no access log line for the mounted route under its request id: %s", e.logs.String())
	}
	if strings.Contains(e.logs.String(), "QUERY-CANARY-4417") {
		t.Fatalf("the query string reached the log: %s", e.logs.String())
	}

	// the recoverer covers it: a panic is the 500 envelope, the detail stays in the log
	rec = e.do(http.MethodPost, "/v1/boom")
	envelope(t, rec, "internal_error", "An unexpected error occurred.")
	if strings.Contains(rec.Body.String(), "mounted route panics") {
		t.Fatalf("the panic value reached the client: %s", rec.Body)
	}

	// the probes still answer beside the mounted routes
	if rec := e.do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d beside the mounted routes", rec.Code)
	}
}

func TestMount_TheCatchAllStillOwnsEveryOtherPathAndVerb(t *testing.T) {
	routes := func(m *http.ServeMux) {
		m.HandleFunc("POST /v1/only-post", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	e := mounted(t, routes)

	// the right verb: served
	if rec := e.do(http.MethodPost, "/v1/only-post"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /v1/only-post = %d, want 204", rec.Code)
	}
	// the wrong verb: the 405 envelope with an Allow header (the mux is asked, so a mounted route is counted)
	rec := e.do(http.MethodGet, "/v1/only-post")
	envelope(t, rec, "method_not_allowed", "This method is not allowed on this path.")
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
	// another path: the 404 envelope
	envelope(t, e.do(http.MethodGet, "/v1/nothing-here"), "not_found", "The requested resource was not found.")

	// the control: with nothing mounted, the very same request is a 404, never a 405 or a 204
	none := mounted(t, nil)
	if rec := none.do(http.MethodPost, "/v1/only-post"); rec.Code != http.StatusNotFound {
		t.Fatalf("with no Mount, POST /v1/only-post = %d, want 404", rec.Code)
	}
	envelope(t, none.do(http.MethodGet, "/v1/only-post"), "not_found", "The requested resource was not found.")
}

func TestWriteError_EnvelopeCarriesDetailsOnlyWhenGiven(t *testing.T) {
	write := func(details ...any) *httptest.ResponseRecorder {
		e := mounted(t, func(m *http.ServeMux) {
			m.HandleFunc("GET /v1/err", func(w http.ResponseWriter, r *http.Request) {
				WriteError(w, r, http.StatusConflict, "job_slot_taken", "A message.", details...)
			})
		})
		return e.do(http.MethodGet, "/v1/err")
	}

	// no details: the envelope has exactly the three members and no `details` key at all (not null, not {})
	rec := write()
	if rec.Code != http.StatusConflict || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := decode(t, rec)
	errObj, _ := body["error"].(map[string]any)
	if got := strings.Join(keys(errObj), ","); got != "code,message,request_id" {
		t.Fatalf("the envelope without details has members %q, want code,message,request_id", got)
	}
	if errObj["code"] != "job_slot_taken" || errObj["message"] != "A message." || errObj["request_id"] != rec.Header().Get(RequestIDHeader) {
		t.Fatalf("envelope = %v", errObj)
	}

	// the control: with details, the member is there and holds exactly the pairs given
	rec = write("available", 3, "needed", 10, "shortfall", 7)
	errObj, _ = decode(t, rec)["error"].(map[string]any)
	d, ok := errObj["details"].(map[string]any)
	if !ok || len(d) != 3 || d["available"] != float64(3) || d["needed"] != float64(10) || d["shortfall"] != float64(7) {
		t.Fatalf("details = %v, want the three pairs", errObj["details"])
	}

	// a dangling key and a key that is not a string are dropped, never guessed at; with nothing left there is still no `details` member
	for name, args := range map[string][]any{
		"a lone key":         {"reason"},
		"a non-string key":   {42, "x"},
		"an empty key":       {"", "x"},
		"nothing but a nil":  {nil},
		"a key then nothing": {"a", 1, "dangling"},
	} {
		rec := write(args...)
		errObj, _ := decode(t, rec)["error"].(map[string]any)
		d, has := errObj["details"].(map[string]any)
		switch name {
		case "a key then nothing":
			if !has || len(d) != 1 || d["a"] != float64(1) {
				t.Errorf("%s: details = %v, want only a=1", name, errObj["details"])
			}
		default:
			if _, has := errObj["details"]; has {
				t.Errorf("%s: a details member was written although no complete pair was given: %v", name, errObj["details"])
			}
		}
	}
}

func TestWriteError_UsesTheRequestIDOfTheRequest(t *testing.T) {
	// outside the middleware there is no id in the context: the member is present and empty, never missing
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	WriteError(rec, req, http.StatusTeapot, "teapot", "short and stout")
	errObj, _ := decode(t, rec)["error"].(map[string]any)
	if rid, has := errObj["request_id"]; !has || rid != "" {
		t.Fatalf("request_id = %v (present %v), want the empty string", rid, has)
	}
	// with an id in the context (what the middleware does) it is echoed
	rec = httptest.NewRecorder()
	WriteError(rec, req.WithContext(logging.WithRequestID(context.Background(), "req_abc")), http.StatusTeapot, "teapot", "short and stout")
	errObj, _ = decode(t, rec)["error"].(map[string]any)
	if errObj["request_id"] != "req_abc" {
		t.Fatalf("request_id = %v, want req_abc", errObj["request_id"])
	}
}
